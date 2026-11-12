//! Area **user settings** (Task 16): the device list: rename, sign out and remove, the key dialog.
//!
//! Ports: packages/web/src/screens/settings/{device-name,device-platform}.ts, and the worker's `refreshDevices`,
//! `revokeDevice`, `renameDevice` and `signOutRevoke` (packages/client-core/src/worker/controller.ts) with
//! `refreshOwnDeviceList` (account/signup.ts).
//!
//! **Keys.** A recovery key crosses into [`Devices::revoke`] and [`Devices::sign_out_and_remove`] once, in a [`Zeroizing`]
//! buffer that is handed to the core and dropped with the call. It is never logged, never in an error, never in the slice.
//! The slice lists a device by id, tier, label and times: never a key of it.

use std::sync::Arc;

use tokio::sync::oneshot;
use zeroize::Zeroizing;

use crate::client::{Client, ClientError, Inner};
use crate::features::profile::refusal;
use crate::features::profile::text::{profile_text_problem, profile_text_value};
use crate::signin::{chain_row, normalise_recovery_key};
use crate::state::DeviceId;
use crate::state::profile::DeviceEntry;
use crate::sync::api::try_core;
use crate::sync::value::{self, arr, at_least, bin16, opt, uint};

/// The longest device name the server accepts (protocol/09 Devices).
pub const DEVICE_NAME_MAX: usize = 64;
/// `DEVICE_LIST_HISTORY_PAGE` (account/signup.ts).
const DEVICE_LIST_HISTORY_PAGE: usize = 64;
/// The characters of a recovery key (52 without separators).
pub const RECOVERY_KEY_LENGTH: usize = 52;

// ---- pure: device-name.ts, device-platform.ts, the display order ----

/// The browser and system named in a user agent, as the words a person would use.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct DeviceNameParts {
    pub browser: &'static str,
    pub os: &'static str,
}

/// Order matters: Edge, Opera and Samsung Internet also say Chrome, and Chrome also says Safari.
fn browser_of(ua: &str) -> Option<&'static str> {
    let has = |needle: &str| ua.contains(needle);
    // `\bEdg(?:e|A|iOS)?\/`
    if has("Edg/") || has("Edge/") || has("EdgA/") || has("EdgiOS/") {
        return Some("Edge");
    }
    if has("OPR/") {
        Some("Opera")
    } else if has("SamsungBrowser/") {
        Some("Samsung Internet")
    } else if has("Firefox/") || has("FxiOS/") {
        Some("Firefox")
    } else if has("Chrome/") || has("CriOS/") {
        Some("Chrome")
    } else if has("Safari/") {
        Some("Safari")
    } else {
        None
    }
}

/// iPhone and iPad say "like Mac OS X", Android says "Linux", and ChromeOS says "X11": the narrow ones go first.
fn system_of(ua: &str) -> Option<&'static str> {
    let has = |needle: &str| ua.contains(needle);
    if has("iPhone") || has("iPad") || has("iPod") {
        Some("iOS")
    } else if has("Android") {
        Some("Android")
    } else if has("CrOS") {
        Some("ChromeOS")
    } else if has("Windows") {
        Some("Windows")
    } else if has("Mac OS X") || has("Macintosh") {
        Some("macOS")
    } else if has("Linux") || has("X11") {
        Some("Linux")
    } else {
        None
    }
}

/// A default name for the device a browser runs on, read from its user agent ("Firefox on Linux"). `None` when either part
/// is not recognised: a wrong guess would be worse than a blank field. It is only a suggestion the person edits.
pub fn suggest_device_name(user_agent: &str) -> Option<DeviceNameParts> {
    Some(DeviceNameParts {
        browser: browser_of(user_agent)?,
        os: system_of(user_agent)?,
    })
}

/// The browser a user agent names ("Firefox"), or `None` when it is not one of the known ones.
pub fn browser_name(user_agent: &str) -> Option<&'static str> {
    browser_of(user_agent)
}

/// A shell's platform name (`process.platform`: `darwin`, `win32`, `linux`) as the words of the label.
pub fn desktop_os(platform: &str) -> Option<&'static str> {
    match platform {
        "darwin" => Some("macOS"),
        "win32" => Some("Windows"),
        "linux" => Some("Linux"),
        _ => None,
    }
}

/// Rust's `std::env::consts::OS` as `process.platform` names it.
pub fn this_platform() -> &'static str {
    match std::env::consts::OS {
        "macos" => "darwin",
        "windows" => "win32",
        other => other,
    }
}

/// Where this device runs, for its own row: "Desktop · macOS" ([`this_platform`]), or "Desktop" when the system is unknown.
/// A display label, never a trust signal: it is shown only on the device's own row and is never sent anywhere.
pub fn device_platform_label(platform: &str) -> String {
    match desktop_os(platform) {
        Some(os) => format!("Desktop · {os}"),
        None => "Desktop".to_owned(),
    }
}

/// The tag on this device's own row: always the desktop's own label here (the native app is never a browser).
pub fn own_platform_tag() -> String {
    device_platform_label(this_platform())
}

/// The device list in display order: this device first, then the devices that are not removed by last seen (newest first),
/// then the removed ones by when they were removed (newest first); ties by id. Returns a new list.
pub fn order_devices(devices: &[DeviceEntry]) -> Vec<DeviceEntry> {
    let mut out = devices.to_vec();
    out.sort_by(|a, b| {
        use std::cmp::Ordering::{Greater, Less};
        if a.own != b.own {
            return if a.own { Less } else { Greater };
        }
        match (a.revoked_at, b.revoked_at) {
            (None, Some(_)) => Less,
            (Some(_), None) => Greater,
            (None, None) => b.last_seen.cmp(&a.last_seen).then_with(|| a.id.cmp(&b.id)),
            (Some(x), Some(y)) => y.cmp(&x).then_with(|| a.id.cmp(&b.id)),
        }
    });
    out
}

/// A device name as the server stores it, or `None` when it is not 1..64 characters without control or invisible
/// characters (the worker's `deviceLabel`).
pub fn device_label(value: &str) -> Option<String> {
    if profile_text_problem(value, DEVICE_NAME_MAX, false).is_some() {
        return None;
    }
    let label = profile_text_value(value, false);
    (!label.is_empty()).then_some(label)
}

// ---- the commands ----

/// The user settings area's handle for devices: cheap to clone, no I/O on construction.
#[derive(Clone)]
pub struct Devices {
    inner: Arc<Inner>,
}

impl Client {
    /// The [`Devices`] handle.
    pub fn devices(&self) -> Devices {
        Devices {
            inner: self.inner().clone(),
        }
    }
}

fn poisoned() -> ClientError {
    ClientError::Sync("E_CORE_POISONED".into())
}

fn core_refusal(e: &dilla_core::client::ClientError) -> ClientError {
    ClientError::Sync(e.code.to_owned())
}

fn decode_fail(e: value::DecodeError) -> ClientError {
    ClientError::Sync(format!("E_DECODE {}", e.0))
}

/// `GET /v1/devices` rows: `[id, tier, signer_tier, verified_at|null, revoked_at|null, last_seen, label?]`, with the signed
/// list's word on each ([`OwnList`]) and which one is this device.
fn decode_devices(
    raw: &[u8],
    own: &OwnList,
    me: DeviceId,
) -> Result<Vec<DeviceEntry>, ClientError> {
    let v = value::decode(raw).map_err(decode_fail)?;
    let rows = v
        .as_array()
        .ok_or_else(|| ClientError::Sync("E_DECODE devices".into()))?;
    rows.iter()
        .map(|row| {
            let a = at_least(row, 6).map_err(decode_fail)?;
            let id = DeviceId(bin16(&a[0]).map_err(decode_fail)?);
            Ok(DeviceEntry {
                id,
                tier: u8::try_from(uint(&a[1]).map_err(decode_fail)?).unwrap_or(1),
                signer_tier: u8::try_from(uint(&a[2]).map_err(decode_fail)?).unwrap_or(1),
                last_seen: uint(&a[5]).map_err(decode_fail)?,
                revoked_at: opt(&a[4], uint).map_err(decode_fail)?,
                label: match a.get(6) {
                    Some(v) => opt(v, value::text).map_err(decode_fail)?,
                    None => None,
                },
                listed: own.live.contains(&id.0),
                own: id == me,
            })
        })
        .collect()
}

/// What this device's core holds of the account's signed device list.
struct OwnList {
    version: u64,
    /// The ids the list names without a revocation.
    live: Vec<[u8; 16]>,
}

/// core `own_device_list`: `[version, published, [[id, dsk_pub, tier, added_at, revoked_at|null], ...]]`.
fn decode_own_list(raw: &[u8]) -> Result<OwnList, ClientError> {
    let v = value::decode(raw).map_err(decode_fail)?;
    let a = arr(&v, 3).map_err(decode_fail)?;
    let entries = a[2]
        .as_array()
        .ok_or_else(|| ClientError::Sync("E_DECODE own list".into()))?;
    let mut live = Vec::new();
    for e in entries {
        let e = at_least(e, 5).map_err(decode_fail)?;
        if matches!(e[4], crate::gateway::frames::Value::Null) {
            live.push(bin16(&e[0]).map_err(decode_fail)?);
        }
    }
    Ok(OwnList {
        version: uint(&a[0]).map_err(decode_fail)?,
        live,
    })
}

/// What a signed revocation is: the list to publish, the re-sealed state object and an interrupted publication.
struct SignedLists {
    list_body: Vec<u8>,
    state_sealed: Vec<u8>,
    interrupted: Option<Vec<u8>>,
    /// The state object the instance held before this revocation: a failed sign-out puts it back.
    served: Option<Vec<u8>>,
}

/// The list body, the re-sealed state object and an interrupted publication.
type SignedParts = (Vec<u8>, Vec<u8>, Option<Vec<u8>>);

fn decode_signed(raw: &[u8]) -> Result<SignedParts, ClientError> {
    let v = value::decode(raw).map_err(decode_fail)?;
    let a = arr(&v, 3).map_err(decode_fail)?;
    let interrupted = match &a[2] {
        crate::gateway::frames::Value::Null => None,
        other => Some(value::bytes(other).map_err(decode_fail)?),
    };
    Ok((
        value::bytes(&a[0]).map_err(decode_fail)?,
        value::bytes(&a[1]).map_err(decode_fail)?,
        interrupted,
    ))
}

const LIST_CONFLICT: [&str; 2] = [
    "the instance served an older device list",
    "the instance served a different device list at the stored version",
];
const STATE_UNUSABLE: [&str; 2] = [
    "the backup state is missing",
    "the backup state could not be read",
];

fn now_s() -> u64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map_or(0, |d| d.as_secs())
}

impl Inner {
    fn own_ids(&self) -> Result<([u8; 16], [u8; 16]), ClientError> {
        let account = self.snapshot().account.clone();
        match (account.user_id, account.device_id) {
            (Some(u), Some(d)) => Ok((u.0, d.0)),
            _ => Err(ClientError::NoIdentity),
        }
    }

    async fn own_list(&self) -> Result<OwnList, ClientError> {
        let raw = try_core(self.engine().core(), |c| c.own_device_list())
            .await
            .map_err(|_| poisoned())?
            .map_err(|e| core_refusal(&e))?;
        decode_own_list(&raw)
    }

    /// account/signup.ts `adoptServedList` (as `refreshOwnDeviceList` calls it): reads the served list, and when it is newer
    /// than the stored one, adopts the history after it, a page at a time. `true` when this device is still listed.
    async fn refresh_own_list(&self, user: &[u8; 16], me: &[u8; 16]) -> Result<bool, ClientError> {
        let served = self.api().get_device_list(user).await.map_err(refusal)?;
        let own = self.own_list().await?;
        let listed = |l: &OwnList| l.live.contains(me);
        let Some(served) = served else {
            return Ok(listed(&own));
        };
        if served.version <= own.version {
            return Ok(listed(&own));
        }
        let mut after = own.version;
        let mut result_listed = listed(&own);
        loop {
            let (raw, rows) = self
                .api()
                .get_device_list_history(user, after)
                .await
                .map_err(refusal)?;
            if rows.is_empty() {
                return Ok(result_listed);
            }
            let status = try_core(self.engine().core(), move |c| {
                c.own_device_list_update(&raw)
            })
            .await
            .map_err(|_| poisoned())?
            .map_err(|e| core_refusal(&e))?;
            let v = value::decode(&status).map_err(decode_fail)?;
            let a = arr(&v, 2).map_err(decode_fail)?;
            let version = uint(&a[0]).map_err(decode_fail)?;
            result_listed = uint(&a[1]).map_err(decode_fail)? != 0;
            if rows.len() < DEVICE_LIST_HISTORY_PAGE
                || version >= served.version
                || version <= after
            {
                return Ok(result_listed);
            }
            after = version;
        }
    }

    /// Requirement 25: the own list first, so a just-enrolled device shows as listed.
    async fn refresh_devices(&self) -> Result<(), ClientError> {
        let (user, me) = self.own_ids()?;
        if !self.refresh_own_list(&user, &me).await? {
            return Err(ClientError::Revoked);
        }
        let raw = self.api().list_devices().await.map_err(refusal)?;
        let own = self.own_list().await?;
        let devices = decode_devices(&raw, &own, DeviceId(me))?;
        self.publish(move |s| {
            s.profile.devices = devices;
            s.profile.devices_loaded = true;
        });
        Ok(())
    }

    /// The shared part of a revocation: read the root, the state object and the list, and have the core sign the revoking
    /// list and the re-sealed state object. An older served list is refreshed and the reads retried once; a second is the
    /// list conflict (`E_LIST_RACE`).
    async fn revoke_lists(
        &self,
        ids: Vec<[u8; 16]>,
        key: &Zeroizing<String>,
    ) -> Result<SignedLists, ClientError> {
        let (user, me) = self.own_ids()?;
        let flat: Vec<u8> = ids.iter().flatten().copied().collect();
        for attempt in 0.. {
            let root = self
                .api()
                .get_backup(0)
                .await
                .map_err(refusal)?
                .ok_or_else(|| ClientError::Sync("E_NO_BACKUP".into()))?;
            // A missing state object is not a refusal here: the re-sealed one repairs it.
            let state = self.api().get_backup(1).await.map_err(refusal)?;
            let list = self
                .api()
                .get_device_list(&user)
                .await
                .map_err(refusal)?
                .ok_or_else(|| ClientError::Sync("E_NO_BACKUP".into()))?;
            let body = chain_row(&list.raw);
            let sign = |sealed: Vec<u8>| {
                let (key, root, body, flat) =
                    (key.clone(), root.clone(), body.clone(), flat.clone());
                async move {
                    try_core(self.engine().core(), move |c| {
                        c.device_list_revoke(&key, &root, &sealed, &body, &flat, now_s())
                    })
                    .await
                    .map_err(|_| poisoned())
                }
            };
            let first = sign(state.clone().unwrap_or_default()).await?;
            let signed = match first {
                Ok(v) => Ok(v),
                // The core accepts a missing or unopenable served state only at list version 1: past it, a session that
                // deleted or spoiled the object would block every revocation with the key. This device's own sealed state
                // is authentic, so it is tried; the re-sealed object then repairs the instance's copy.
                Err(e)
                    if e.code == "E_CORE_INPUT" && STATE_UNUSABLE.contains(&e.detail.as_str()) =>
                {
                    let sealed = try_core(self.engine().core(), |c| c.sealed_objects())
                        .await
                        .map_err(|_| poisoned())?
                        .map_err(|e| core_refusal(&e))?;
                    let v = value::decode(&sealed).map_err(decode_fail)?;
                    let a = arr(&v, 3).map_err(decode_fail)?;
                    let own = match &a[1] {
                        crate::gateway::frames::Value::Null => {
                            return Err(ClientError::Sync("E_NO_BACKUP".into()));
                        }
                        other => value::bytes(other).map_err(decode_fail)?,
                    };
                    match sign(own).await? {
                        Ok(v) => Ok(v),
                        Err(again)
                            if again.code == "E_CORE_INPUT"
                                && STATE_UNUSABLE.contains(&again.detail.as_str()) =>
                        {
                            return Err(ClientError::Sync("E_NO_BACKUP".into()));
                        }
                        Err(again) => Err(again),
                    }
                }
                Err(e) => Err(e),
            };
            match signed {
                Ok(raw) => {
                    let (list_body, state_sealed, interrupted) = decode_signed(&raw)?;
                    return Ok(SignedLists {
                        list_body,
                        state_sealed,
                        interrupted,
                        served: state,
                    });
                }
                Err(e)
                    if e.code == "E_CORE_INPUT" && LIST_CONFLICT.contains(&e.detail.as_str()) =>
                {
                    if attempt > 0 {
                        return Err(ClientError::Sync("E_LIST_RACE".into()));
                    }
                    // The reads below decide.
                    let _ = self.refresh_own_list(&user, &me).await;
                }
                Err(e) => return Err(core_refusal(&e)),
            }
        }
        unreachable!("the loop returns")
    }

    /// BACKUPS-RECOVERY-02: the list another device signed and could not publish, which the core found in the state object
    /// and signed on, goes to the instance first. A 409 is a list there already; any other failure drops the new candidate
    /// back to it, so a later ready publishes it.
    async fn publish_interrupted(&self, signed: &SignedLists) -> Result<(), ClientError> {
        let Some(body) = signed.interrupted.clone() else {
            return Ok(());
        };
        let (user, _) = self.own_ids()?;
        match self.api().put_device_list(&user, body).await {
            Ok(()) => Ok(()),
            Err(e) if e.status == 409 => Ok(()),
            Err(e) => {
                self.core_drop().await;
                Err(refusal(e))
            }
        }
    }

    async fn core_drop(&self) {
        let _ = try_core(self.engine().core(), |c| c.device_list_drop()).await;
    }

    /// The instance moved the list on while this one was being signed: adopt it, then report the race.
    async fn list_race(&self) -> ClientError {
        if let Ok((user, me)) = self.own_ids() {
            let _ = self.refresh_own_list(&user, &me).await;
        }
        ClientError::Sync("E_LIST_RACE".into())
    }
}

/// The key as the dialog collected it, without its separators; `None` unless it has [`RECOVERY_KEY_LENGTH`] characters.
fn checked_key(typed: Zeroizing<String>) -> Result<Zeroizing<String>, ClientError> {
    let key = normalise_recovery_key(&typed);
    if key.chars().count() == RECOVERY_KEY_LENGTH {
        Ok(key)
    } else {
        Err(ClientError::Sync("E_BAD_INPUT".into()))
    }
}

impl Devices {
    /// Reads the device list (this device's own list first) into the slice. [`ClientError::Revoked`] when this device is no
    /// longer listed.
    pub fn refresh(&self) -> oneshot::Receiver<Result<(), ClientError>> {
        let inner = self.inner.clone();
        self.inner
            .spawn(async move { inner.refresh_devices().await })
    }

    /// Names (or, with `None`, unnames) one of the account's devices. The name is not in the signed device list: no key and
    /// no list change; the server stores it and the list is read again.
    pub fn rename(
        &self,
        device: DeviceId,
        label: Option<String>,
    ) -> oneshot::Receiver<Result<(), ClientError>> {
        let inner = self.inner.clone();
        self.inner.spawn(async move {
            let label = match label {
                None => None,
                Some(l) => {
                    Some(device_label(&l).ok_or_else(|| ClientError::Sync("E_BAD_INPUT".into()))?)
                }
            };
            let mut e = dilla_core::cbor::Encoder::new();
            e.array(1);
            match &label {
                Some(l) => e.text(l),
                None => e.null(),
            };
            inner
                .api()
                .patch_device(&device.0, e.into_vec())
                .await
                .map_err(refusal)?;
            inner.refresh_devices().await
        })
    }

    /// Removes another device. A device the signed list names needs the recovery key (`key` is `Some`): the core signs the
    /// revoking list and the re-sealed state, the list is published first and the state after it. A live device in no signed
    /// list is removed without a key. The instance's 409 on the key-less removal of a listed device crosses unchanged.
    pub fn revoke(
        &self,
        device: DeviceId,
        key: Option<Zeroizing<String>>,
    ) -> oneshot::Receiver<Result<(), ClientError>> {
        let inner = self.inner.clone();
        self.inner.spawn(async move {
            let (user, me) = inner.own_ids()?;
            if device.0 == me {
                return Err(ClientError::Sync("E_BAD_INPUT".into()));
            }
            // The decision between the two dialogs comes from a fresh own list, never a stale one.
            if !inner.refresh_own_list(&user, &me).await? {
                return Err(ClientError::Revoked);
            }
            let listed = inner.own_list().await?.live.contains(&device.0);
            if !listed {
                inner
                    .api()
                    .delete_device(&device.0)
                    .await
                    .map_err(refusal)?;
                return inner.refresh_devices().await;
            }
            let key = checked_key(key.ok_or_else(|| ClientError::Sync("E_BAD_INPUT".into()))?)?;
            let signed = inner.revoke_lists(vec![device.0], &key).await?;
            drop(key);
            inner.publish_interrupted(&signed).await?;
            // BACKUPS-RECOVERY-01: the revoking list first. The state object's PUT shares the user's upload meter and quota
            // with every session of the account, the stolen one included, so it must not be able to hold the list back.
            match inner
                .api()
                .put_device_list(&user, signed.list_body.clone())
                .await
            {
                Ok(()) => {}
                Err(e) if e.status == 409 => return Err(inner.list_race().await),
                Err(e) => return Err(refusal(e)),
            }
            let _ = try_core(inner.engine().core(), |c| c.device_list_published()).await;
            // A refused state object stays unmarked: the next ready uploads it.
            if inner
                .api()
                .put_backup(1, &signed.state_sealed)
                .await
                .is_ok()
            {
                let _ = try_core(inner.engine().core(), |c| c.state_sealed_uploaded()).await;
            }
            inner.refresh_devices().await
        })
    }

    /// Signs this device out and removes it from the account (the Devices row's Sign out): the revoking list names this
    /// device; once the instance has it the engine stops and the slice says [`crate::state::profile::ProfileState::signed_out`],
    /// which the app answers by wiping the store and showing the sign-in.
    ///
    /// A wrong key is `E_RECOVERY_KEY` with nothing changed; a refused publication drops the candidate list and puts the
    /// instance's state object back, so no later ready publishes a self-revocation the person did not finish.
    pub fn sign_out_and_remove(
        &self,
        key: Zeroizing<String>,
    ) -> oneshot::Receiver<Result<(), ClientError>> {
        let inner = self.inner.clone();
        self.inner.spawn(async move {
            let (user, me) = inner.own_ids()?;
            let key = checked_key(key)?;
            let signed = inner.revoke_lists(vec![me], &key).await?;
            drop(key);
            inner.publish_interrupted(&signed).await?;
            // The state object before the list, so the self-revoking PUT is the last request this device makes.
            if let Err(e) = inner.api().put_backup(1, &signed.state_sealed).await {
                inner.core_drop().await;
                return Err(refusal(e));
            }
            if let Err(e) = inner
                .api()
                .put_device_list(&user, signed.list_body.clone())
                .await
            {
                if e.status == 409 {
                    return Err(inner.list_race().await);
                }
                inner.core_drop().await;
                let previous = match signed.served.clone() {
                    Some(p) => Some(p),
                    None => sealed_state(&inner).await,
                };
                if let Some(previous) = previous {
                    let _ = inner.api().put_backup(1, &previous).await;
                }
                return Err(refusal(e));
            }
            inner.engine().stop();
            inner.publish(|s| s.profile.signed_out = true);
            Ok(())
        })
    }
}

/// This device's own sealed state object, if it holds one.
async fn sealed_state(inner: &Inner) -> Option<Vec<u8>> {
    let raw = try_core(inner.engine().core(), |c| c.sealed_objects())
        .await
        .ok()?
        .ok()?;
    let v = value::decode(&raw).ok()?;
    let a = arr(&v, 3).ok()?;
    value::bytes(&a[1]).ok()
}

#[cfg(test)]
mod tests {
    use super::*;

    // packages/web/src/screens/settings/device-name.test.ts
    #[test]
    fn suggest_device_name_reads_the_agents_it_knows() {
        let cases = [
            (
                "Mozilla/5.0 (X11; Linux x86_64; rv:130.0) Gecko/20100101 Firefox/130.0",
                "Firefox",
                "Linux",
            ),
            (
                "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36",
                "Chrome",
                "Linux",
            ),
            (
                "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36 Edg/130.0.0.0",
                "Edge",
                "Windows",
            ),
            (
                "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Safari/605.1.15",
                "Safari",
                "macOS",
            ),
            (
                "Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Mobile Safari/537.36",
                "Chrome",
                "Android",
            ),
            (
                "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1",
                "Safari",
                "iOS",
            ),
            (
                "Mozilla/5.0 (X11; CrOS x86_64 14541.0.0) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36",
                "Chrome",
                "ChromeOS",
            ),
            (
                "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36 OPR/115.0.0.0",
                "Opera",
                "Linux",
            ),
        ];
        for (ua, browser, os) in cases {
            assert_eq!(
                suggest_device_name(ua),
                Some(DeviceNameParts { browser, os }),
                "{ua}"
            );
        }
    }

    #[test]
    fn suggest_device_name_gives_nothing_for_an_agent_it_does_not_know() {
        assert_eq!(suggest_device_name(""), None);
        assert_eq!(suggest_device_name("curl/8.0"), None);
        assert_eq!(
            suggest_device_name(
                "Mozilla/5.0 (linux) AppleWebKit/537.36 (KHTML, like Gecko) jsdom/26.0.0"
            ),
            None
        );
    }

    // packages/web/src/screens/settings/device-platform.test.ts
    #[test]
    fn the_platform_label_names_the_desktop_os_and_says_only_desktop_when_it_is_unknown() {
        assert_eq!(device_platform_label("darwin"), "Desktop · macOS");
        assert_eq!(device_platform_label("win32"), "Desktop · Windows");
        assert_eq!(device_platform_label("linux"), "Desktop · Linux");
        assert_eq!(device_platform_label("aix"), "Desktop");
        assert_eq!(device_platform_label(""), "Desktop");
        assert_eq!(
            browser_name("Mozilla/5.0 (X11; Linux x86_64; rv:139.0) Gecko/20100101 Firefox/139.0"),
            Some("Firefox")
        );
        assert_eq!(browser_name("something else"), None);
    }

    fn device(id: u8, own: bool, last_seen: u64, revoked_at: Option<u64>) -> DeviceEntry {
        DeviceEntry {
            id: DeviceId([id; 16]),
            tier: 1,
            signer_tier: 1,
            last_seen,
            revoked_at,
            label: None,
            listed: true,
            own,
        }
    }

    // Devices.tsx `orderDevices`.
    #[test]
    fn devices_are_ordered_own_first_then_live_by_last_seen_then_removed_by_removal() {
        let list = [
            device(1, false, 10, Some(90)),
            device(2, false, 50, None),
            device(3, true, 1, None),
            device(4, false, 70, None),
            device(5, false, 10, Some(95)),
            device(6, false, 70, None),
        ];
        let order: Vec<u8> = order_devices(&list).iter().map(|d| d.id.0[0]).collect();
        assert_eq!(order, [3, 4, 6, 2, 5, 1]);
    }

    #[test]
    fn a_device_name_is_one_to_sixty_four_visible_characters() {
        assert_eq!(device_label("  Laptop  ").as_deref(), Some("Laptop"));
        assert_eq!(device_label(""), None);
        assert_eq!(device_label("   "), None);
        assert_eq!(device_label(&"a".repeat(65)), None);
        assert_eq!(device_label("a\u{200b}b"), None);
        assert_eq!(device_label("tab\tname"), None);
    }

    #[test]
    fn a_key_of_the_wrong_length_is_refused_before_any_request() {
        assert!(checked_key(Zeroizing::new("ABCD".into())).is_err());
        let ok = "ABCDEFGHIJKLM".repeat(4);
        assert_eq!(ok.len(), 52);
        assert!(checked_key(Zeroizing::new(ok)).is_ok());
        // Separators are not characters of the key.
        let spaced = "ABCDEFGHIJKLM ".repeat(4);
        assert!(checked_key(Zeroizing::new(spaced)).is_ok());
    }

    #[test]
    fn the_own_list_names_the_devices_it_still_lists() {
        let mut e = dilla_core::cbor::Encoder::new();
        e.array(3).uint(4).uint(1).array(2);
        e.array(5)
            .bytes(&[1; 16])
            .bytes(&[0; 32])
            .uint(1)
            .uint(5)
            .opt_uint(None);
        e.array(5)
            .bytes(&[2; 16])
            .bytes(&[0; 32])
            .uint(1)
            .uint(5)
            .opt_uint(Some(9));
        let own = decode_own_list(&e.into_vec()).unwrap();
        assert_eq!(own.version, 4);
        assert_eq!(own.live, vec![[1u8; 16]]);
    }
}
