//! The account's backup objects: ports of `ensureBackups` and `repairBackupState` (`packages/client-core/src/account/
//! enrol.ts`), over a [`BackupPort`] so the cases of `enrol.test.ts` run against a fake as they do in TypeScript.
//!
//! The instance stores two sealed objects per account: the root (kind 0, written once by the first device) and the state
//! (kind 1, the newest device list and keys, sealed under the root's key). This device holds its own sealed copies
//! (`ClientCore::sealed_objects`); it can neither open the instance's copies nor needs to:
//!
//! - [`ensure_backups`] uploads what the instance is missing, and compares a stored root byte for byte with this
//!   device's: a different one means the instance holds a root this device did not seal, so every later recovery on it
//!   would fail as a wrong key ([`BackupError::RootMismatch`], which the client reports and never drops).
//! - [`repair_backup_state`] replaces a stored state object that is behind this device's newest accepted list, or junk
//!   (BACKUPS-RECOVERY-03), once the own device list has been refreshed.
//!
//! [`after_ready`] runs them in the controller's order (controller.ts `onReady`): ensure, then refresh the own device
//! list (`refreshOwnDeviceList`), then, only for a device the refreshed list still names, repair. A device the newest
//! list leaves out is revoked: nothing is repaired. A refresh that fails skips the repair (this device cannot tell
//! whether its state is the newest) and is reported, never dropped.

use std::sync::Mutex;

use crate::gateway::frames::Value;
use crate::http::routes::{self as r, NoBody, Raw};
use crate::sync::api::{Api, ApiError, CoreHandle, try_core};
use crate::sync::value::{self, at_least, opt, uint};

/// The two object kinds.
pub const ROOT: u8 = 0;
pub const STATE: u8 = 1;

/// core-port.ts `SealedObjects`.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct Sealed {
    pub root: Option<Vec<u8>>,
    pub state: Option<Vec<u8>>,
    pub state_uploaded: bool,
}

/// Why the backups could not be brought in line.
#[derive(Clone, Debug, thiserror::Error)]
pub enum BackupError {
    /// enrol.ts `E_ROOT_MISMATCH`: the instance holds a root object this device did not seal.
    #[error("E_ROOT_MISMATCH")]
    RootMismatch,
    #[error("{0}")]
    Api(ApiError),
    /// The local core refused (its code).
    #[error("{0}")]
    Core(String),
}

impl From<ApiError> for BackupError {
    fn from(e: ApiError) -> Self {
        Self::Api(e)
    }
}

/// What the backup logic reads and writes: the instance's backup routes and this device's sealed objects.
#[async_trait::async_trait]
pub trait BackupPort: Send + Sync {
    /// routes.ts `listBackups`: the kinds the instance holds.
    async fn list(&self) -> Result<Vec<u8>, ApiError>;
    /// routes.ts `getBackup`: the stored object, `None` on 404.
    async fn get(&self, kind: u8) -> Result<Option<Vec<u8>>, ApiError>;
    /// routes.ts `putBackup`.
    async fn put(&self, kind: u8, object: &[u8]) -> Result<(), ApiError>;
    async fn sealed(&self) -> Result<Sealed, BackupError>;
    /// `ClientCore::state_sealed_current`.
    async fn state_current(&self) -> Result<bool, BackupError>;
    /// `ClientCore::state_sealed_uploaded`.
    async fn mark_uploaded(&self) -> Result<(), BackupError>;
}

/// enrol.ts `refreshOwnDeviceList`'s answer: the newest own list this device accepted, and whether it names this device.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct OwnList {
    pub version: u64,
    pub listed: bool,
}

/// What a ready's backup pass needs beside the backups: the own device list, refreshed from the instance.
#[async_trait::async_trait]
pub trait ReadyPort: BackupPort {
    async fn refresh_own_list(&self) -> Result<OwnList, BackupError>;
}

/// How a ready's backup pass ended.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum AfterReady {
    /// The own list names this device; `repaired` says whether its state object replaced the instance's.
    Listed { repaired: bool },
    /// The newest own list leaves this device out: it is revoked, and nothing was repaired.
    Unlisted,
}

/// What [`after_ready`] did: `ensured` is [`ensure_backups`]' own result (a failure there does not stop the refresh, as
/// the two run independently in the controller); `outcome` is the refresh and the repair.
#[derive(Debug)]
pub struct ReadyPass {
    pub ensured: Result<(), BackupError>,
    pub outcome: Result<AfterReady, BackupError>,
}

/// controller.ts `onReady`'s backup steps, in its order: [`ensure_backups`]; the own device list refreshed; a device
/// it leaves out is [`AfterReady::Unlisted`] (no repair); a failed refresh is the error and skips the repair; only then
/// [`repair_backup_state`].
pub async fn after_ready(port: &dyn ReadyPort, confirmed: &Mutex<Option<Vec<u8>>>) -> ReadyPass {
    let ensured = ensure_backups(port, confirmed).await;
    let outcome = async {
        let own = port.refresh_own_list().await?;
        if !own.listed {
            return Ok(AfterReady::Unlisted);
        }
        let repaired = repair_backup_state(port).await?;
        Ok(AfterReady::Listed { repaired })
    }
    .await;
    ReadyPass { ensured, outcome }
}

/// enrol.ts `ensureBackups`. `confirmed` is the root this device already compared with the instance's (enrol.ts
/// `confirmedRoots`), so a stored root is read once per client, not on every ready.
pub async fn ensure_backups(
    port: &dyn BackupPort,
    confirmed: &Mutex<Option<Vec<u8>>>,
) -> Result<(), BackupError> {
    let kinds = port.list().await?;
    let sealed = port.sealed().await?;
    if let Some(root) = &sealed.root {
        if kinds.contains(&ROOT) {
            let known = lock(confirmed).as_deref() == Some(root.as_slice());
            if !known {
                compare_stored_root(port, root).await?;
                *lock(confirmed) = Some(root.clone());
            }
        } else {
            match port.put(ROOT, root).await {
                Ok(()) => {}
                // 409: a root is stored. 410 E_PRUNED: an operator purged these bytes, which tombstones them while the
                // root stays served (protocol/09 Admin); either way the stored root is the one to compare.
                Err(e) if e.status == 409 || e.is(410, "E_PRUNED") => {
                    compare_stored_root(port, root).await?;
                }
                Err(e) => return Err(e.into()),
            }
            *lock(confirmed) = Some(root.clone());
        }
    }
    let Some(state) = &sealed.state else {
        return Ok(());
    };
    // The repair of a stored object that is behind or junk is repair_backup_state's, after the own list is refreshed.
    if !kinds.contains(&STATE) || !sealed.state_uploaded {
        port.put(STATE, state).await?;
        port.mark_uploaded().await?;
    }
    Ok(())
}

async fn compare_stored_root(port: &dyn BackupPort, root: &[u8]) -> Result<(), BackupError> {
    match port.get(ROOT).await? {
        Some(stored) if stored == root => Ok(()),
        _ => Err(BackupError::RootMismatch),
    }
}

/// enrol.ts `repairBackupState`: answers whether this device's state object was uploaded over the instance's.
pub async fn repair_backup_state(port: &dyn BackupPort) -> Result<bool, BackupError> {
    let Some(own) = port.sealed().await?.state else {
        return Ok(false);
    };
    let stored = port.get(STATE).await?;
    if stored.as_deref() == Some(own.as_slice()) {
        return Ok(false);
    }
    // Another device's object is left alone unless this device's own state carries the newest list it accepted.
    if stored.is_some() && !port.state_current().await? {
        return Ok(false);
    }
    port.put(STATE, &own).await?;
    port.mark_uploaded().await?;
    Ok(true)
}

fn lock<T>(m: &Mutex<T>) -> std::sync::MutexGuard<'_, T> {
    m.lock().unwrap_or_else(std::sync::PoisonError::into_inner)
}

/// The live port: the instance's routes and the client's core, for the account `user` on device `device`.
pub(crate) struct Live<'a> {
    pub core: &'a CoreHandle,
    pub api: &'a Api,
    pub user: [u8; 16],
    pub device: [u8; 16],
}

/// routes.ts `DEVICE_LIST_HISTORY_PAGE`.
const HISTORY_PAGE: usize = 64;

/// core-port.ts `ownDeviceList`: `[version, published, [[device_id, dsk_pub, tier, added_at, revoked_at|null]]]`;
/// listed is signup.ts' rule (an entry for `device` that is not revoked).
pub fn decode_own_list(raw: &[u8], device: &[u8; 16]) -> Result<OwnList, value::DecodeError> {
    let v = value::decode(raw)?;
    let a = at_least(&v, 3)?;
    let version = uint(&a[0])?;
    let entries = a[2]
        .as_array()
        .ok_or(value::DecodeError("own list entries"))?;
    let mut listed = false;
    for entry in entries {
        let e = at_least(entry, 5)?;
        if value::bin16(&e[0])? == *device && matches!(e[4], Value::Null) {
            listed = true;
        }
    }
    Ok(OwnList { version, listed })
}

/// core-port.ts `ownDeviceListUpdate`: `[version, listed]`.
pub fn decode_list_status(raw: &[u8]) -> Result<OwnList, value::DecodeError> {
    let v = value::decode(raw)?;
    let a = at_least(&v, 2)?;
    Ok(OwnList {
        version: uint(&a[0])?,
        listed: uint(&a[1])? == 1,
    })
}

fn decode_err(e: value::DecodeError) -> ApiError {
    ApiError::local(0, "E_DECODE", e.0)
}

async fn core_call<T: Send + 'static>(
    core: &CoreHandle,
    f: impl FnOnce(&mut dilla_core::client::ClientCore) -> Result<T, dilla_core::client::ClientError>
    + Send
    + 'static,
) -> Result<T, BackupError> {
    try_core(core, f)
        .await
        .map_err(|_| BackupError::Core("E_CORE_POISONED".into()))?
        .map_err(|e| BackupError::Core(e.code.to_owned()))
}

/// core-port.ts `sealedObjects`: `[root|null, state|null, state_uploaded]`.
pub fn decode_sealed(raw: &[u8]) -> Result<Sealed, value::DecodeError> {
    let v = value::decode(raw)?;
    let a = at_least(&v, 3)?;
    Ok(Sealed {
        root: opt(&a[0], value::bytes)?,
        state: opt(&a[1], value::bytes)?,
        state_uploaded: uint(&a[2])? == 1,
    })
}

#[async_trait::async_trait]
impl BackupPort for Live<'_> {
    async fn list(&self) -> Result<Vec<u8>, ApiError> {
        let Raw(raw) = self
            .api
            .http_call::<r::ListBackups>(&[], NoBody, true)
            .await?;
        let v = value::decode(&raw).map_err(decode_err)?;
        let rows = v
            .as_array()
            .ok_or_else(|| ApiError::local(0, "E_DECODE", "backups"))?;
        rows.iter()
            .map(|row| {
                let a = at_least(row, 4).map_err(decode_err)?;
                match a[0].as_uint() {
                    Some(k @ (0 | 1)) => Ok(k as u8),
                    _ => Err(ApiError::local(0, "E_DECODE", "backup kind")),
                }
            })
            .collect()
    }

    async fn get(&self, kind: u8) -> Result<Option<Vec<u8>>, ApiError> {
        let reply = self
            .api
            .http_call::<r::GetBackup>(&[&kind.to_string()], NoBody, true)
            .await?;
        if reply.status == 404 {
            return Ok(None);
        }
        let v = value::decode(&reply.body).map_err(decode_err)?;
        let a = at_least(&v, 2).map_err(decode_err)?;
        Ok(Some(value::bytes(&a[0]).map_err(decode_err)?))
    }

    async fn put(&self, kind: u8, object: &[u8]) -> Result<(), ApiError> {
        let body = Value::Array(vec![Value::Bytes(object.to_vec())]).encode();
        self.api
            .http_call::<r::PutBackup>(&[&kind.to_string()], Raw(body), true)
            .await?;
        Ok(())
    }

    async fn sealed(&self) -> Result<Sealed, BackupError> {
        let raw = core_call(self.core, |c| c.sealed_objects()).await?;
        decode_sealed(&raw).map_err(|e| BackupError::Core(format!("E_CORE_DECODE: {}", e.0)))
    }

    async fn state_current(&self) -> Result<bool, BackupError> {
        core_call(self.core, |c| c.state_sealed_current()).await
    }

    async fn mark_uploaded(&self) -> Result<(), BackupError> {
        core_call(self.core, |c| c.state_sealed_uploaded()).await
    }
}

#[async_trait::async_trait]
impl ReadyPort for Live<'_> {
    /// enrol.ts `refreshOwnDeviceList` (signup.ts `adoptServedList`): the served head; when it is newer than the list
    /// this device holds, the signed history after it, page by page, each adopted by the core (which checks every
    /// signature against the account's SSK).
    async fn refresh_own_list(&self) -> Result<OwnList, BackupError> {
        let raw = core_call(self.core, |c| c.own_device_list()).await?;
        let device = self.device;
        let mut result = decode_own_list(&raw, &device)
            .map_err(|e| BackupError::Core(format!("E_CORE_DECODE: {}", e.0)))?;
        let Some(served) = self.api.get_device_list(&self.user).await? else {
            return Ok(result);
        };
        if served.version <= result.version {
            return Ok(result);
        }
        let mut after = result.version;
        loop {
            let (page, rows) = self.api.get_device_list_history(&self.user, after).await?;
            if rows.is_empty() {
                return Ok(result);
            }
            let status = core_call(self.core, move |c| c.own_device_list_update(&page)).await?;
            result = decode_list_status(&status)
                .map_err(|e| BackupError::Core(format!("E_CORE_DECODE: {}", e.0)))?;
            if rows.len() < HISTORY_PAGE
                || result.version >= served.version
                || result.version <= after
            {
                return Ok(result);
            }
            after = result.version;
        }
    }
}

#[cfg(test)]
mod tests {
    //! enrol.test.ts `ensureBackups` and `repairBackupState`, case for case, against a fake instance that logs every
    //! request as `METHOD path` the way FakeServer does.
    use super::*;
    use std::collections::{BTreeMap, VecDeque};

    const ROOT_OBJ: &[u8] = b"root object sealed by this device";
    const STATE_OBJ: &[u8] = b"state object sealed by this device";
    const OTHER: &[u8] = b"a root this device did not seal";
    const JUNK: &[u8] = b"junk planted in the state slot";

    #[derive(Default)]
    struct Fake {
        stored: Mutex<BTreeMap<u8, Vec<u8>>>,
        log: Mutex<Vec<String>>,
        /// One-shot answers, by `METHOD path`: an error status and code, or (for the listing) an empty list.
        once: Mutex<BTreeMap<String, VecDeque<(u16, &'static str)>>>,
        sealed: Mutex<Sealed>,
        current: Mutex<bool>,
        marked: Mutex<u32>,
    }

    impl Fake {
        fn new(root: Option<&[u8]>, state: Option<&[u8]>) -> Self {
            let f = Self::default();
            *lock(&f.sealed) = Sealed {
                root: root.map(<[u8]>::to_vec),
                state: state.map(<[u8]>::to_vec),
                state_uploaded: false,
            };
            *lock(&f.current) = true;
            f
        }
        fn store(&self, kind: u8, obj: &[u8]) {
            lock(&self.stored).insert(kind, obj.to_vec());
        }
        fn once(&self, what: &str, status: u16, code: &'static str) {
            lock(&self.once)
                .entry(what.to_owned())
                .or_default()
                .push_back((status, code));
        }
        fn take(&self, what: &str) -> Option<(u16, &'static str)> {
            lock(&self.once).get_mut(what).and_then(VecDeque::pop_front)
        }
        fn paths(&self) -> Vec<String> {
            std::mem::take(&mut *lock(&self.log))
        }
        fn marked(&self) -> u32 {
            *lock(&self.marked)
        }
    }

    #[async_trait::async_trait]
    impl BackupPort for Fake {
        async fn list(&self) -> Result<Vec<u8>, ApiError> {
            lock(&self.log).push("GET /v1/backups".into());
            if self.take("GET /v1/backups").is_some() {
                return Ok(Vec::new());
            }
            Ok(lock(&self.stored).keys().copied().collect())
        }
        async fn get(&self, kind: u8) -> Result<Option<Vec<u8>>, ApiError> {
            lock(&self.log).push(format!("GET /v1/backups/{kind}/0"));
            Ok(lock(&self.stored).get(&kind).cloned())
        }
        async fn put(&self, kind: u8, object: &[u8]) -> Result<(), ApiError> {
            let what = format!("PUT /v1/backups/{kind}/0");
            lock(&self.log).push(what.clone());
            if let Some((status, code)) = self.take(&what) {
                return Err(ApiError::local(status, code, ""));
            }
            if kind == ROOT && lock(&self.stored).contains_key(&ROOT) {
                return Err(ApiError::local(409, "E_INVALID_REQUEST", ""));
            }
            lock(&self.stored).insert(kind, object.to_vec());
            Ok(())
        }
        async fn sealed(&self) -> Result<Sealed, BackupError> {
            Ok(lock(&self.sealed).clone())
        }
        async fn state_current(&self) -> Result<bool, BackupError> {
            Ok(*lock(&self.current))
        }
        async fn mark_uploaded(&self) -> Result<(), BackupError> {
            *lock(&self.marked) += 1;
            lock(&self.sealed).state_uploaded = true;
            Ok(())
        }
    }

    fn confirmed() -> Mutex<Option<Vec<u8>>> {
        Mutex::new(None)
    }

    /// The fake's own-list refresh: what `GET /v1/users/{me}/device-list` and its history lead the core to.
    struct Ready {
        backups: Fake,
        own: Mutex<Result<OwnList, u16>>,
    }

    #[async_trait::async_trait]
    impl BackupPort for Ready {
        async fn list(&self) -> Result<Vec<u8>, ApiError> {
            self.backups.list().await
        }
        async fn get(&self, kind: u8) -> Result<Option<Vec<u8>>, ApiError> {
            self.backups.get(kind).await
        }
        async fn put(&self, kind: u8, object: &[u8]) -> Result<(), ApiError> {
            self.backups.put(kind, object).await
        }
        async fn sealed(&self) -> Result<Sealed, BackupError> {
            self.backups.sealed().await
        }
        async fn state_current(&self) -> Result<bool, BackupError> {
            self.backups.state_current().await
        }
        async fn mark_uploaded(&self) -> Result<(), BackupError> {
            self.backups.mark_uploaded().await
        }
    }

    #[async_trait::async_trait]
    impl ReadyPort for Ready {
        async fn refresh_own_list(&self) -> Result<OwnList, BackupError> {
            lock(&self.backups.log).push("GET /v1/users/me/device-list".into());
            match *lock(&self.own) {
                Ok(own) => Ok(own),
                Err(status) => Err(BackupError::Api(ApiError::local(status, "E_INTERNAL", ""))),
            }
        }
    }

    fn ready(own: Result<OwnList, u16>) -> Ready {
        let backups = Fake::new(Some(ROOT_OBJ), Some(STATE_OBJ));
        // Junk in the state slot, with this device's state marked uploaded (so ensure leaves it): only a repair that
        // runs replaces it.
        backups.store(STATE, JUNK);
        lock(&backups.sealed).state_uploaded = true;
        Ready {
            backups,
            own: Mutex::new(own),
        }
    }

    #[tokio::test]
    async fn the_ready_pass_refreshes_the_own_list_between_ensure_and_repair() {
        let r = ready(Ok(OwnList {
            version: 3,
            listed: true,
        }));
        let pass = after_ready(&r, &confirmed()).await;
        assert!(pass.ensured.is_ok(), "{:?}", pass.ensured);
        assert_eq!(pass.outcome.unwrap(), AfterReady::Listed { repaired: true });
        assert_eq!(
            r.backups.paths(),
            [
                "GET /v1/backups",
                "PUT /v1/backups/0/0",
                "GET /v1/users/me/device-list",
                "GET /v1/backups/1/0",
                "PUT /v1/backups/1/0"
            ]
        );
    }

    #[tokio::test]
    async fn an_unlisted_device_is_revoked_and_repairs_nothing() {
        let r = ready(Ok(OwnList {
            version: 4,
            listed: false,
        }));
        let pass = after_ready(&r, &confirmed()).await;
        assert_eq!(pass.outcome.unwrap(), AfterReady::Unlisted);
        let paths = r.backups.paths();
        assert_eq!(paths.last().unwrap(), "GET /v1/users/me/device-list");
        assert!(
            !paths.iter().any(|p| p.contains("/v1/backups/1/0")),
            "{paths:?}"
        );
        assert_eq!(r.backups.get(STATE).await.unwrap().unwrap(), JUNK);
    }

    #[tokio::test]
    async fn a_failed_refresh_skips_the_repair_and_is_reported() {
        let r = ready(Err(503));
        let pass = after_ready(&r, &confirmed()).await;
        assert!(pass.ensured.is_ok());
        match pass.outcome {
            Err(BackupError::Api(e)) => assert_eq!(e.status, 503),
            other => panic!("the refresh failure is the outcome: {other:?}"),
        }
        let paths = r.backups.paths();
        assert!(
            !paths.iter().any(|p| p.contains("/v1/backups/1/0")),
            "{paths:?}"
        );
        assert_eq!(r.backups.get(STATE).await.unwrap().unwrap(), JUNK);
    }

    #[test]
    fn own_lists_decode_as_core_port_ts() {
        let me = [7u8; 16];
        let entry = |id: [u8; 16], revoked: Option<u64>| {
            Value::Array(vec![
                Value::Bytes(id.to_vec()),
                Value::Bytes(vec![1; 32]),
                Value::Uint(0),
                Value::Uint(10),
                revoked.map_or(Value::Null, Value::Uint),
            ])
        };
        let list = |entries: Vec<Value>| {
            Value::Array(vec![Value::Uint(5), Value::Uint(1), Value::Array(entries)]).encode()
        };
        assert_eq!(
            decode_own_list(&list(vec![entry([1; 16], None), entry(me, None)]), &me).unwrap(),
            OwnList {
                version: 5,
                listed: true
            }
        );
        assert!(
            !decode_own_list(&list(vec![entry(me, Some(20))]), &me)
                .unwrap()
                .listed
        );
        assert!(
            !decode_own_list(&list(vec![entry([1; 16], None)]), &me)
                .unwrap()
                .listed
        );
        let status = Value::Array(vec![Value::Uint(6), Value::Uint(0)]).encode();
        assert_eq!(
            decode_list_status(&status).unwrap(),
            OwnList {
                version: 6,
                listed: false
            }
        );
    }

    #[tokio::test]
    async fn uploads_the_root_and_the_state_when_the_instance_holds_neither_then_nothing() {
        let f = Fake::new(Some(ROOT_OBJ), Some(STATE_OBJ));
        let c = confirmed();
        ensure_backups(&f, &c).await.unwrap();
        assert_eq!(
            f.paths(),
            [
                "GET /v1/backups",
                "PUT /v1/backups/0/0",
                "PUT /v1/backups/1/0"
            ]
        );
        assert_eq!(f.marked(), 1);
        ensure_backups(&f, &c).await.unwrap();
        assert_eq!(f.paths(), ["GET /v1/backups"]);
        assert_eq!(f.marked(), 1);
        assert_eq!(f.get(ROOT).await.unwrap().unwrap(), ROOT_OBJ);
        assert_eq!(f.get(STATE).await.unwrap().unwrap(), STATE_OBJ);
    }

    #[tokio::test]
    async fn detects_a_different_stored_root_after_a_409_and_alerts_before_uploading_state() {
        let f = Fake::new(Some(ROOT_OBJ), Some(STATE_OBJ));
        f.store(ROOT, OTHER);
        f.once("GET /v1/backups", 200, "");
        let r = ensure_backups(&f, &confirmed()).await;
        assert!(matches!(r, Err(BackupError::RootMismatch)), "{r:?}");
        assert_eq!(
            f.paths(),
            [
                "GET /v1/backups",
                "PUT /v1/backups/0/0",
                "GET /v1/backups/0/0"
            ]
        );
        assert_eq!(f.marked(), 0);
    }

    #[tokio::test]
    async fn detects_a_planted_root_already_shown_in_the_listing_before_uploading_state() {
        let f = Fake::new(Some(ROOT_OBJ), Some(STATE_OBJ));
        f.store(ROOT, OTHER);
        let r = ensure_backups(&f, &confirmed()).await;
        assert!(matches!(r, Err(BackupError::RootMismatch)), "{r:?}");
        assert_eq!(f.paths(), ["GET /v1/backups", "GET /v1/backups/0/0"]);
        assert_eq!(f.marked(), 0);
    }

    #[tokio::test]
    async fn accepts_equal_root_bytes_after_a_raced_409_and_uploads_state() {
        let f = Fake::new(Some(ROOT_OBJ), Some(STATE_OBJ));
        f.store(ROOT, ROOT_OBJ);
        f.once("GET /v1/backups", 200, "");
        ensure_backups(&f, &confirmed()).await.unwrap();
        assert_eq!(
            f.paths(),
            [
                "GET /v1/backups",
                "PUT /v1/backups/0/0",
                "GET /v1/backups/0/0",
                "PUT /v1/backups/1/0"
            ]
        );
        assert_eq!(f.marked(), 1);
    }

    #[tokio::test]
    async fn accepts_equal_root_bytes_after_a_410_e_pruned_on_the_root_re_put_and_uploads_state() {
        let f = Fake::new(Some(ROOT_OBJ), Some(STATE_OBJ));
        f.store(ROOT, ROOT_OBJ);
        f.once("GET /v1/backups", 200, "");
        f.once("PUT /v1/backups/0/0", 410, "E_PRUNED");
        ensure_backups(&f, &confirmed()).await.unwrap();
        assert_eq!(
            f.paths(),
            [
                "GET /v1/backups",
                "PUT /v1/backups/0/0",
                "GET /v1/backups/0/0",
                "PUT /v1/backups/1/0"
            ]
        );
        assert_eq!(f.marked(), 1);
        assert_eq!(f.get(STATE).await.unwrap().unwrap(), STATE_OBJ);
    }

    #[tokio::test]
    async fn detects_a_different_stored_root_after_a_410_e_pruned_on_the_root_re_put() {
        let f = Fake::new(Some(ROOT_OBJ), Some(STATE_OBJ));
        f.store(ROOT, OTHER);
        f.once("GET /v1/backups", 200, "");
        f.once("PUT /v1/backups/0/0", 410, "E_PRUNED");
        let r = ensure_backups(&f, &confirmed()).await;
        assert!(matches!(r, Err(BackupError::RootMismatch)), "{r:?}");
        assert_eq!(
            f.paths(),
            [
                "GET /v1/backups",
                "PUT /v1/backups/0/0",
                "GET /v1/backups/0/0"
            ]
        );
        assert_eq!(f.marked(), 0);
    }

    #[tokio::test]
    async fn a_410_on_the_root_put_that_is_not_e_pruned_propagates() {
        let f = Fake::new(Some(ROOT_OBJ), Some(STATE_OBJ));
        f.once("PUT /v1/backups/0/0", 410, "E_GONE");
        match ensure_backups(&f, &confirmed()).await {
            Err(BackupError::Api(e)) => assert_eq!((e.status, e.code.as_str()), (410, "E_GONE")),
            other => panic!("{other:?}"),
        }
        assert_eq!(f.marked(), 0);
    }

    #[tokio::test]
    async fn re_uploads_a_state_absent_from_the_listing_despite_the_uploaded_hint() {
        let f = Fake::new(Some(ROOT_OBJ), Some(STATE_OBJ));
        f.store(ROOT, ROOT_OBJ);
        f.mark_uploaded().await.unwrap();
        ensure_backups(&f, &confirmed()).await.unwrap();
        assert_eq!(
            f.paths(),
            [
                "GET /v1/backups",
                "GET /v1/backups/0/0",
                "PUT /v1/backups/1/0"
            ]
        );
        assert_eq!(f.marked(), 2);
        assert_eq!(f.get(STATE).await.unwrap().unwrap(), STATE_OBJ);
    }

    #[tokio::test]
    async fn uploads_the_state_again_while_it_is_not_marked_uploaded_though_the_instance_holds_one()
    {
        let f = Fake::new(Some(ROOT_OBJ), None);
        f.store(ROOT, ROOT_OBJ);
        f.store(STATE, b"an older state");
        let c = confirmed();
        // Confirms the pre-existing root once.
        ensure_backups(&f, &c).await.unwrap();
        lock(&f.sealed).state = Some(STATE_OBJ.to_vec());
        f.paths();
        ensure_backups(&f, &c).await.unwrap();
        assert_eq!(f.paths(), ["GET /v1/backups", "PUT /v1/backups/1/0"]);
        assert_eq!(f.marked(), 1);
        assert_eq!(f.get(STATE).await.unwrap().unwrap(), STATE_OBJ);
    }

    #[tokio::test]
    async fn a_refused_state_upload_propagates_and_the_state_stays_unmarked() {
        let f = Fake::new(Some(ROOT_OBJ), Some(STATE_OBJ));
        f.once("PUT /v1/backups/1/0", 507, "E_STORAGE_FULL");
        match ensure_backups(&f, &confirmed()).await {
            Err(BackupError::Api(e)) => {
                assert_eq!((e.status, e.code.as_str()), (507, "E_STORAGE_FULL"))
            }
            other => panic!("{other:?}"),
        }
        assert_eq!(f.marked(), 0);
        assert_eq!(f.get(ROOT).await.unwrap().unwrap(), ROOT_OBJ);
    }

    #[tokio::test]
    async fn uploads_nothing_when_the_core_holds_no_sealed_objects() {
        let f = Fake::new(None, None);
        ensure_backups(&f, &confirmed()).await.unwrap();
        assert_eq!(f.paths(), ["GET /v1/backups"]);
    }

    #[tokio::test]
    async fn repair_re_uploads_this_devices_current_state_over_junk_the_instance_holds() {
        let f = Fake::new(Some(ROOT_OBJ), Some(STATE_OBJ));
        f.store(STATE, JUNK);
        assert!(repair_backup_state(&f).await.unwrap());
        assert_eq!(f.paths(), ["GET /v1/backups/1/0", "PUT /v1/backups/1/0"]);
        assert_eq!(f.get(STATE).await.unwrap().unwrap(), STATE_OBJ);
        assert_eq!(f.marked(), 1);
        f.paths();
        assert!(!repair_backup_state(&f).await.unwrap());
        assert_eq!(f.paths(), ["GET /v1/backups/1/0"]);
    }

    #[tokio::test]
    async fn repair_re_uploads_it_when_the_instance_holds_none() {
        let f = Fake::new(Some(ROOT_OBJ), Some(STATE_OBJ));
        assert!(repair_backup_state(&f).await.unwrap());
        assert_eq!(f.paths(), ["GET /v1/backups/1/0", "PUT /v1/backups/1/0"]);
    }

    #[tokio::test]
    async fn repair_leaves_another_devices_object_alone_when_this_state_is_not_the_newest() {
        let f = Fake::new(Some(ROOT_OBJ), Some(STATE_OBJ));
        *lock(&f.current) = false;
        f.store(STATE, JUNK);
        assert!(!repair_backup_state(&f).await.unwrap());
        assert_eq!(f.paths(), ["GET /v1/backups/1/0"]);
        assert_eq!(f.get(STATE).await.unwrap().unwrap(), JUNK);
    }

    #[tokio::test]
    async fn repair_does_nothing_without_a_sealed_state_of_its_own() {
        let f = Fake::new(Some(ROOT_OBJ), None);
        assert!(!repair_backup_state(&f).await.unwrap());
        assert_eq!(f.paths(), Vec::<String>::new());
    }

    #[test]
    fn sealed_objects_decode_as_core_port_ts() {
        let raw = Value::Array(vec![
            Value::Bytes(ROOT_OBJ.to_vec()),
            Value::Null,
            Value::Uint(1),
        ])
        .encode();
        assert_eq!(
            decode_sealed(&raw).unwrap(),
            Sealed {
                root: Some(ROOT_OBJ.to_vec()),
                state: None,
                state_uploaded: true
            }
        );
    }
}
