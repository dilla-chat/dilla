//! The account's upkeep: key packages and backups (Task 2: ports of `account/keypackages.ts` and of `ensureBackups` and
//! `repairBackupState` in `account/enrol.ts`), run where `worker/controller.ts` runs them: on every ready of the
//! gateway (`onReady`), the key packages topped up from the ready's count (the server's own count after a reconnect),
//! then the backups ensured, the own device list refreshed, and (for a device it still names) the stored state object
//! repaired.
//!
//! Like the controller, a failure here never stops the client: a key-package publish that fails is recorded and
//! retried with a backoff (5 s doubling to 5 min, the server's count read first), a backup failure waits for the next
//! ready, and a root mismatch is kept for the UI ([`Upkeep::root_mismatch`]) instead of being dropped.

pub mod backups;
pub mod keypackages;

use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex, Weak};
use std::time::Duration;

use tokio::task::AbortHandle;

use crate::client::Inner;
use crate::sync::api::{Api, CoreHandle};

use backups::{AfterReady, BackupError, Live};
use keypackages::Limits;

/// controller.ts `DiagnosticsState['keyPackages']`: what Diagnostics shows of the last publish.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct KeyPackageStatus {
    /// The directory's ordinary count after the last attempt that succeeded (`None`: not known yet).
    pub count: Option<u32>,
    /// Unix seconds of the last publish attempt.
    pub last_attempt: Option<u64>,
    /// The code of the last failed attempt, cleared by a success.
    pub last_error: Option<String>,
}

/// The last ready's backup pass (controller.ts records the root mismatch on the account; every other failure is kept
/// here so it is shown, not swallowed).
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct BackupStatus {
    /// Unix seconds of the last pass.
    pub last_pass: Option<u64>,
    /// `ensure_backups`' failure (`E_ROOT_MISMATCH` included), if it failed.
    pub ensure_error: Option<String>,
    /// The own-list refresh's or the repair's failure, if one failed (the repair is skipped when the refresh fails).
    pub refresh_error: Option<String>,
}

#[derive(Default)]
struct KeyPackages {
    /// One attempt at a time (controller.ts `keyPackagesBusy`).
    busy: bool,
    delay_ms: u64,
    /// A pending retry (controller.ts `keyPackagesRetry`): a ready meanwhile neither publishes nor cancels it.
    retry: Option<AbortHandle>,
    limits: Option<Limits>,
    status: KeyPackageStatus,
}

/// Told the key package status after each attempt (Task 24's diagnostics: `Diagnostics::set_key_packages`).
pub(crate) type KeyPackageObserver = Arc<dyn Fn(&KeyPackageStatus) + Send + Sync>;

/// One client's account upkeep. Held by the client (`Inner`) and its publisher.
pub(crate) struct Upkeep {
    kp_observer: Mutex<Option<KeyPackageObserver>>,
    core: CoreHandle,
    api: Arc<Api>,
    runtime: tokio::runtime::Handle,
    me: Weak<Upkeep>,
    stopped: AtomicBool,
    kp: Mutex<KeyPackages>,
    /// enrol.ts `confirmedRoots`.
    confirmed_root: Mutex<Option<Vec<u8>>>,
    root_mismatch: AtomicBool,
    /// The last ready's backup pass, as the UI reads it.
    backups_status: Mutex<BackupStatus>,
    user: [u8; 16],
    device: [u8; 16],
    #[cfg(test)]
    panic_next: AtomicBool,
    /// A ready was seen: a later one is a reconnect, whose key-package count is read from the server (controller.ts
    /// `keyPackagesRecheck`).
    seen_ready: AtomicBool,
}

fn lock<T>(m: &Mutex<T>) -> std::sync::MutexGuard<'_, T> {
    m.lock().unwrap_or_else(std::sync::PoisonError::into_inner)
}

fn now_s() -> u64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map_or(0, |d| d.as_secs())
}

impl Upkeep {
    pub(crate) fn new(
        core: CoreHandle,
        api: Arc<Api>,
        runtime: tokio::runtime::Handle,
        user: [u8; 16],
        device: [u8; 16],
    ) -> Arc<Self> {
        Arc::new_cyclic(|me| Self {
            kp_observer: Mutex::default(),
            user,
            device,
            backups_status: Mutex::default(),
            core,
            api,
            runtime,
            me: me.clone(),
            stopped: AtomicBool::new(false),
            kp: Mutex::default(),
            confirmed_root: Mutex::default(),
            root_mismatch: AtomicBool::new(false),
            seen_ready: AtomicBool::new(false),
            #[cfg(test)]
            panic_next: AtomicBool::new(false),
        })
    }

    /// Who is told the key package status after each attempt.
    pub(crate) fn set_key_package_observer(&self, observer: Option<KeyPackageObserver>) {
        *lock(&self.kp_observer) = observer;
    }

    /// The session ended (revoked, failed, stopped): no retry runs again.
    pub(crate) fn stop(&self) {
        self.stopped.store(true, Ordering::Release);
        if let Some(t) = lock(&self.kp).retry.take() {
            t.abort();
        }
    }

    fn stopped(&self) -> bool {
        self.stopped.load(Ordering::Acquire) || self.core.is_poisoned()
    }

    /// controller.ts `onReady`'s backup steps ([`backups::after_ready`]: ensure, refresh the own device list, repair
    /// only a listed device). A root mismatch is recorded (a later success clears it), and every failure is kept in
    /// [`BackupStatus`] for the UI, never dropped; the next ready tries again.
    async fn ready_pass(&self) -> Result<AfterReady, BackupError> {
        if self.stopped() {
            return Err(BackupError::Core("E_SYNC_STOPPED".into()));
        }
        let port = Live {
            core: &self.core,
            api: &self.api,
            user: self.user,
            device: self.device,
        };
        let pass = backups::after_ready(&port, &self.confirmed_root).await;
        match &pass.ensured {
            Ok(()) => self.root_mismatch.store(false, Ordering::Release),
            Err(BackupError::RootMismatch) => self.root_mismatch.store(true, Ordering::Release),
            Err(_) => {}
        }
        *lock(&self.backups_status) = BackupStatus {
            last_pass: Some(now_s()),
            ensure_error: pass.ensured.as_ref().err().map(ToString::to_string),
            refresh_error: pass.outcome.as_ref().err().map(ToString::to_string),
        };
        pass.outcome
    }

    pub(crate) fn backup_status(&self) -> BackupStatus {
        lock(&self.backups_status).clone()
    }

    /// enrol.ts `E_ROOT_MISMATCH`, as the account slice's `rootMismatch`: the instance holds a backup root this device
    /// did not seal (BACKUPS-RECOVERY-04); the UI alerts.
    pub(crate) fn root_mismatch(&self) -> bool {
        self.root_mismatch.load(Ordering::Acquire)
    }

    pub(crate) fn key_package_status(&self) -> KeyPackageStatus {
        lock(&self.kp).status.clone()
    }

    /// controller.ts `topUpKeyPackages`: one attempt at a time and none while a retry waits; a failure is recorded and
    /// retried after the backoff, the retry reading the server's count first (a publish whose answer was lost is not
    /// published again).
    pub(crate) fn refill(&self, remaining: u32, recheck: bool) {
        let Some(me) = self.me.upgrade() else { return };
        {
            let mut kp = lock(&self.kp);
            if self.stopped() || kp.busy || kp.retry.is_some() {
                return;
            }
            kp.busy = true;
        }
        self.runtime.spawn(async move {
            use futures_util::FutureExt;
            // A panic in the attempt is a failed attempt (recorded, retried with the backoff), never a `busy` left set
            // that would stop every later publish.
            let outcome = std::panic::AssertUnwindSafe(me.refill_once(remaining, recheck))
                .catch_unwind()
                .await
                .unwrap_or_else(|_| Err("E_PANICKED".to_owned()));
            let mut kp = lock(&me.kp);
            kp.busy = false;
            match outcome {
                Ok((known, published)) => {
                    kp.delay_ms = 0;
                    kp.status = KeyPackageStatus {
                        count: Some(known.saturating_add(published)),
                        last_attempt: if published > 0 {
                            Some(now_s())
                        } else {
                            kp.status.last_attempt
                        },
                        last_error: None,
                    };
                }
                Err(code) => {
                    kp.delay_ms = keypackages::next_delay_ms(kp.delay_ms);
                    kp.status.last_attempt = Some(now_s());
                    kp.status.last_error = Some(code);
                    if me.stopped() {
                        return;
                    }
                    let wait = Duration::from_millis(kp.delay_ms);
                    let again = Arc::clone(&me);
                    let handle = me.runtime.spawn(async move {
                        tokio::time::sleep(wait).await;
                        lock(&again.kp).retry = None;
                        again.refill(remaining, true);
                    });
                    kp.retry = Some(handle.abort_handle());
                }
            }
            // Outside the lock: the observer publishes.
            let status = kp.status.clone();
            drop(kp);
            let observer = lock(&me.kp_observer).clone();
            if let Some(observer) = observer {
                observer(&status);
            }
        });
    }

    async fn refill_once(&self, remaining: u32, recheck: bool) -> Result<(u32, u32), String> {
        #[cfg(test)]
        if self.panic_next.swap(false, Ordering::AcqRel) {
            panic!("an injected panic in the refill");
        }
        let cached = lock(&self.kp).limits;
        let limits = match cached {
            Some(l) => l,
            None => {
                let l = keypackages::get_limits(&self.api)
                    .await
                    .map_err(|e| e.code)?;
                lock(&self.kp).limits = Some(l);
                l
            }
        };
        // After a reconnect (or on a retry) the server's own count is asked for; an older server without the route
        // leaves the ready's count.
        let known = if recheck {
            keypackages::get_count(&self.api)
                .await
                .ok()
                .flatten()
                .unwrap_or(remaining)
        } else {
            remaining
        };
        let published = keypackages::refill(&self.core, &self.api, known, limits)
            .await
            .map_err(|e| e.code)?;
        Ok((known, published))
    }
}

/// The screens plan's entry point (Task 2), run on every ready: publishes key packages up to the instance's target
/// when `remaining` (the ready's count; after a reconnect the server's own count is read first) is below its refill
/// threshold.
pub(crate) fn refill_key_packages(inner: &Inner, remaining: u32) {
    let upkeep = inner.upkeep();
    let recheck = upkeep.seen_ready.swap(true, Ordering::AcqRel);
    upkeep.refill(remaining, recheck);
}

/// The screens plan's backup entry point (Task 2), run on every ready in controller.ts' order: `ensure_backups`, then the
/// own device list refreshed, then `repair_backup_state` only for a device the list still names. Answers
/// [`AfterReady::Unlisted`] for a device the newest own list leaves out (the caller signs it out); a failed refresh is
/// the error, and nothing was repaired.
pub(crate) async fn backups_after_ready(inner: &Inner) -> Result<AfterReady, BackupError> {
    inner.upkeep().ready_pass().await
}

#[cfg(test)]
mod tests {
    use super::*;

    /// A panic inside a refill attempt resets `busy` (and is recorded and retried), so later publishes still run.
    #[tokio::test]
    async fn a_panicking_refill_resets_busy() {
        let conn = rusqlite::Connection::open_in_memory().unwrap();
        let core: CoreHandle = Arc::new(Mutex::new(
            dilla_core::client::ClientCore::open(Arc::new(Mutex::new(conn))).unwrap(),
        ));
        let auth = crate::auth::AuthToken::new("token");
        let http = crate::http::ReqwestHttp::new(
            url::Url::parse("http://127.0.0.1:9").unwrap(),
            auth.clone(),
        )
        .unwrap();
        let api = Arc::new(Api::new(
            http,
            crate::sync::requests::SocketRequests::new(),
            auth,
            core.clone(),
            Arc::new(|_: &str| {}),
        ));
        let upkeep = Upkeep::new(
            core,
            api,
            tokio::runtime::Handle::current(),
            [1; 16],
            [2; 16],
        );
        upkeep.panic_next.store(true, Ordering::Release);
        upkeep.refill(0, false);
        tokio::time::timeout(Duration::from_secs(5), async {
            loop {
                if !lock(&upkeep.kp).busy && upkeep.key_package_status().last_error.is_some() {
                    return;
                }
                tokio::time::sleep(Duration::from_millis(10)).await;
            }
        })
        .await
        .expect("the attempt ended");
        let kp = lock(&upkeep.kp);
        assert!(!kp.busy, "busy is reset");
        assert_eq!(kp.status.last_error.as_deref(), Some("E_PANICKED"));
        assert!(kp.retry.is_some(), "a retry is armed");
        drop(kp);
        upkeep.stop();
    }
}
