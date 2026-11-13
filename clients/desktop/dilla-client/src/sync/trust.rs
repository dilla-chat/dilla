//! The trust refresh: a port of state/trust.ts (`readDeviceListHistory`, `signedHeadList`, `headAttestation`,
//! `historyFrom`, `ingestAccountKind`) and the controller's `refreshTrustOnce`. The server only supplies bytes; the core
//! authenticates every signature and leaf. A leaf whose user's signed device list the core has not accepted keeps its
//! messages held (`cannot-read`) until its history is ingested here and `revalidate_pending` releases them.

use std::collections::{HashMap, HashSet};

use crate::gateway::frames::Value;

use super::api::{Api, CoreHandle, with_core};
use super::value::{self, DecodeError, Rows, arr, bin16, bin32, opt, text, u53, uint};

const MAX_VERSIONS: u64 = 1024;
/// trust.ts `MAX_REPLY_ROWS`: the most rows a core answer (pending leaves, trust state) is read with.
const MAX_REPLY_ROWS: usize = 4096;
const MAX_BYTES: usize = 4_194_304;
const PAGE: usize = 64;
const MAX_HEAD: usize = 1_048_576;

/// core-port.ts `PendingLeaf`.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct PendingLeaf {
    pub group_id: [u8; 16],
    pub leaf_index: u32,
    pub user_id: [u8; 16],
    pub cached_version: u64,
    /// `unknown`, `changed` or `ready-refresh`.
    pub reason: String,
}

/// trust.ts `decodeBoundedArray`: a top-level array of at most [`MAX_REPLY_ROWS`] rows.
fn bounded(raw: &[u8]) -> Result<Rows, DecodeError> {
    let rows = Rows::of(raw)?;
    if rows.iter().len() > MAX_REPLY_ROWS {
        return Err(DecodeError("limit"));
    }
    Ok(rows)
}

pub fn decode_pending_leaves(raw: &[u8]) -> Result<Vec<PendingLeaf>, DecodeError> {
    bounded(raw)?
        .iter()
        .map(|row| {
            let a = arr(row, 5)?;
            let leaf = u53(&a[1])?;
            let reason = text(&a[4])?;
            if !matches!(reason.as_str(), "unknown" | "changed" | "ready-refresh") {
                return Err(DecodeError("leaf reason"));
            }
            Ok(PendingLeaf {
                group_id: bin16(&a[0])?,
                leaf_index: u32::try_from(leaf).map_err(|_| DecodeError("leaf index"))?,
                user_id: bin16(&a[2])?,
                cached_version: uint(&a[3])?,
                reason,
            })
        })
        .collect()
}

/// core-port.ts `TrustRecord`: one `trust_state` row (ABI 8), public keys only.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct TrustRecord {
    pub user_id: [u8; 16],
    pub umk: [u8; 32],
    pub first_seen: u64,
    pub verified: bool,
    pub changed_umk: Option<[u8; 32]>,
    /// The stored verdict of the user's leaves still in an active group (`E_UMK_CHANGED` wins), as the core spelt it.
    pub blocked: Option<String>,
}

/// trust.ts `decodeTrustState`: `[[user_id, umk, first_seen, verified 0|1, changed_umk|null, blocked|null]]`. The
/// five-element row of ABI 7 is refused, as is a verified bit other than 0 or 1.
pub fn decode_trust_state(raw: &[u8]) -> Result<Vec<TrustRecord>, DecodeError> {
    bounded(raw)?
        .iter()
        .map(|row| {
            let a = arr(row, 6)?;
            let verified = u53(&a[3])?;
            if verified > 1 {
                return Err(DecodeError("verified bit"));
            }
            Ok(TrustRecord {
                user_id: bin16(&a[0])?,
                umk: bin32(&a[1])?,
                first_seen: uint(&a[2])?,
                verified: verified == 1,
                changed_umk: opt(&a[4], bin32)?,
                blocked: opt(&a[5], text)?,
            })
        })
        .collect()
}

/// A device-list read that does not hold together (trust.ts `E_DEVICE_LIST_STALE`).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Stale;

/// trust.ts `headRow`: `[version, list, ssk_signature, prev_hash]`, plus the account-kind attestation (wire 4), plus the
/// account's keys (wire 6).
fn head_row(head: &[u8]) -> Result<Vec<Value>, Stale> {
    match value::decode(head).map_err(|_| Stale)? {
        Value::Array(row) if matches!(row.len(), 4 | 5 | 8) => Ok(row),
        _ => Err(Stale),
    }
}

/// trust.ts `signedHeadList`: the signed DeviceList inside the head row, as the core caches it.
pub fn signed_head_list(head: &[u8]) -> Result<Vec<u8>, Stale> {
    head_row(head)?[1]
        .as_bytes()
        .map(<[u8]>::to_vec)
        .ok_or(Stale)
}

/// trust.ts `headAttestation`.
pub fn head_attestation(head: &[u8]) -> Result<Option<Vec<u8>>, Stale> {
    let row = head_row(head)?;
    match row.get(4) {
        Some(v) => v.as_bytes().map(|b| Some(b.to_vec())).ok_or(Stale),
        None => Ok(None),
    }
}

/// trust.ts `historyFrom`: the rows of `history` from `version` on, or `None` when it holds none. One malformed row
/// (trust.ts `arr(row, 4)`) makes the whole slice `None`, never a partial one.
pub fn history_from(history: &[u8], version: u64) -> Option<Vec<u8>> {
    let rows = Rows::of(history).ok()?;
    if rows.iter().len() as u64 > MAX_VERSIONS {
        return None;
    }
    let mut kept: Vec<Value> = Vec::new();
    for row in rows.iter() {
        let v = uint(&arr(row, 4).ok()?[0]).ok()?;
        if v >= version {
            kept.push(row.clone());
        }
    }
    (!kept.is_empty()).then(|| Value::Array(kept).encode())
}

/// trust.ts `readDeviceListHistory`: the user's served head and the signed rows from `cached_version` up to it, read
/// twice over when the head moved meanwhile; every bound trust.ts holds (1024 versions, 4 MiB, 64-row pages, rows that
/// chain one version at a time and end at the head) is held here.
pub async fn read_device_list_history(
    api: &Api,
    user: &[u8; 16],
    cached_version: u64,
) -> Result<(Vec<u8>, Vec<u8>), Stale> {
    let mut budget_bytes = 0usize;
    let mut budget_rows = 0u64;
    for _ in 0..2 {
        let Some(candidate) = api.get_device_list(user).await.map_err(|_| Stale)? else {
            return Err(Stale);
        };
        if candidate.raw.len() > MAX_HEAD || candidate.version < cached_version {
            return Err(Stale);
        }
        let row = head_row(&candidate.raw)?;
        let chain_row = Value::Array(row[..4].to_vec());
        if row[0].as_uint() != Some(candidate.version)
            || row[1].as_bytes() != Some(candidate.blob.as_slice())
            || row[2].as_bytes().is_none_or(|b| b.len() != 64)
            || row[3].as_bytes().is_none_or(|b| b.len() != 32)
        {
            return Err(Stale);
        }
        if candidate.version - cached_version > MAX_VERSIONS {
            return Err(Stale);
        }
        let mut after = cached_version.saturating_sub(1);
        let mut rows: Vec<Value> = Vec::new();
        if candidate.version == cached_version {
            rows.push(chain_row.clone());
            after = candidate.version;
            budget_rows += 1;
            budget_bytes += candidate.raw.len();
            if budget_rows > MAX_VERSIONS || budget_bytes > MAX_BYTES {
                return Err(Stale);
            }
        }
        let mut overran = false;
        while after < candidate.version {
            let (raw, got) = api
                .get_device_list_history(user, after)
                .await
                .map_err(|_| Stale)?;
            budget_bytes += raw.len();
            if raw.len() > MAX_HEAD
                || budget_bytes > MAX_BYTES
                || got.is_empty()
                || got.len() > PAGE
            {
                return Err(Stale);
            }
            if budget_rows + got.len() as u64 > MAX_VERSIONS {
                return Err(Stale);
            }
            for v in &got {
                let r = v.as_array().filter(|r| r.len() == 4).ok_or(Stale)?;
                let version = r[0].as_uint().ok_or(Stale)?;
                if version != after + 1 {
                    return Err(Stale);
                }
                if version > candidate.version {
                    overran = true;
                    break;
                }
                if r[1].as_bytes().is_none()
                    || r[2].as_bytes().is_none_or(|b| b.len() != 64)
                    || r[3].as_bytes().is_none_or(|b| b.len() != 32)
                {
                    return Err(Stale);
                }
                rows.push(v.clone());
                after = version;
            }
            budget_rows += got.len() as u64;
            if overran {
                break;
            }
            let last = got
                .last()
                .and_then(|r| r.as_array())
                .and_then(|r| r[0].as_uint());
            if last != Some(after) {
                return Err(Stale);
            }
        }
        if rows.last() != Some(&chain_row) {
            return Err(Stale);
        }
        let head = api.get_device_list(user).await.map_err(|_| Stale)?;
        if overran && head.as_ref().is_none_or(|h| h.version <= candidate.version) {
            return Err(Stale);
        }
        if let Some(head) = head
            && head.version == candidate.version
            && head.raw == candidate.raw
        {
            let history = Value::Array(rows).encode();
            if history.len() > MAX_BYTES {
                return Err(Stale);
            }
            return Ok((candidate.raw, history));
        }
    }
    Err(Stale)
}

/// trust.ts `ingestAccountKind`: the head's attestation goes to the core before the history; any refusal is handled
/// as a withheld attestation (nothing stored, the ingest still runs).
async fn ingest_account_kind(core: &CoreHandle, user: [u8; 16], head: &[u8]) {
    let Ok(Some(attestation)) = head_attestation(head) else {
        return;
    };
    let _ = with_core(core, move |c| c.account_kind_ingest(&user, &attestation)).await;
}

/// controller.ts `LEAF_VERDICTS` (sync/errors.ts): the core's own leaf verdicts, the only refusals that say a device is
/// off its list. A stale read or a limit says nothing about the device.
const LEAF_VERDICTS: &[&str] = &[
    "E_DEVICE_UNLISTED",
    "E_DEVICE_REVOKED",
    "E_CREDENTIAL",
    "E_TIER_MISMATCH",
];

/// What the trust refresh remembers between passes: the leaves already fetched (controller `fetchedLeaves`), so a leaf
/// is read once per (group, leaf, user, cached version, reason) until something about it changes; and the controller's
/// annotations of the trust slice the core stores nothing for (`unlistedUsers`, `historyFailedUsers`, `pendingUsers`,
/// `ownRefusedLeaves`).
#[derive(Default)]
pub struct TrustMemory {
    fetched: HashSet<String>,
    unlisted: HashSet<[u8; 16]>,
    history_failed: HashSet<[u8; 16]>,
    pending: HashSet<[u8; 16]>,
    own_refused: HashSet<([u8; 16], u32)>,
}

/// The refresh's annotations the trust slice reads (controller.ts `publishTrust`).
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct Annotations {
    /// A leaf verdict refused the user's history (`unlistedUsers`).
    pub unlisted: HashSet<[u8; 16]>,
    /// The user's history read has not completed: failed, or never accepted (`pendingUsers`).
    pub pending: HashSet<[u8; 16]>,
    /// An own-user leaf whose history the core refused as under another key (`ownRefusedLeaves` is non-empty).
    pub own_refused: bool,
}

impl TrustMemory {
    pub fn annotations(&self) -> Annotations {
        Annotations {
            unlisted: self.unlisted.clone(),
            pending: self.pending.clone(),
            own_refused: !self.own_refused.is_empty(),
        }
    }

    /// controller.ts `verifyContact`: the user's leaves are fetched again on the next pass.
    pub fn forget_user(&mut self, user: &[u8; 16]) {
        let hex = crate::state::hex(user);
        self.fetched
            .retain(|key| key.split(':').nth(2) != Some(hex.as_str()));
    }
}

/// One engine's trust refresh: a single pass at a time (controller.ts `trustRefresh` and `trustRefreshAgain`), and the
/// revision its passes move whenever they changed what the trust slice shows, so the slice is read again even when
/// nothing was ingested.
pub struct TrustPass {
    revision: tokio::sync::watch::Sender<u64>,
    /// Held while a pass runs; a later request waits on it.
    running: tokio::sync::Mutex<()>,
    /// A pass was asked for that no running pass has started on yet.
    again: std::sync::atomic::AtomicBool,
}

impl Default for TrustPass {
    fn default() -> Self {
        Self {
            revision: tokio::sync::watch::channel(0).0,
            running: tokio::sync::Mutex::new(()),
            again: std::sync::atomic::AtomicBool::new(false),
        }
    }
}

impl TrustPass {
    /// Runs `pass` unless a pass already running absorbs the request: a request during a pass makes exactly one more pass
    /// run after it, however many came meanwhile. Returns once a pass that began after the request has finished.
    pub async fn run<F, Fut>(&self, mut pass: F)
    where
        F: FnMut() -> Fut,
        Fut: std::future::Future<Output = ()>,
    {
        use std::sync::atomic::Ordering;
        self.again.store(true, Ordering::SeqCst);
        let _running = self.running.lock().await;
        while self.again.swap(false, Ordering::SeqCst) {
            pass().await;
        }
    }

    /// Wakes on every pass that changed what the trust slice shows.
    pub fn revisions(&self) -> tokio::sync::watch::Receiver<u64> {
        self.revision.subscribe()
    }

    fn bump(&self) {
        self.revision.send_modify(|n| *n = n.wrapping_add(1));
    }
}

/// `Engine::refresh_trust`: one single-flight pass ([`TrustPass::run`]) of [`refresh_once`]; an ingest tells the hooks
/// and marks every active group changed, as before.
pub(crate) async fn refresh(engine: &super::Engine, now_s: u64) {
    engine
        .trust_pass
        .run(|| async {
            if engine.stopped() {
                return;
            }
            if refresh_once(
                &engine.api,
                &engine.core,
                &engine.trust,
                &engine.trust_pass,
                now_s,
            )
            .await
            {
                engine.hooks.trust_changed();
                for g in engine.groups().await.iter().filter(|g| g.state == 2) {
                    engine.hooks.group_changed(g.group_id);
                }
            }
        })
        .await;
}

/// The engine's trust revision, for the trust area's watcher.
pub(crate) fn revisions_of(engine: &super::Engine) -> tokio::sync::watch::Receiver<u64> {
    engine.trust_pass.revisions()
}

/// A user's served device-list head and the signed rows read up to it.
type Signed = (Vec<u8>, Vec<u8>);

/// The own user of the core, if it has one.
async fn own_user(core: &CoreHandle) -> Option<[u8; 16]> {
    with_core(core, |c| c.identity())
        .await
        .ok()
        .and_then(|raw| super::api::Identity::decode(&raw))
        .and_then(|i| i.user_id)
}

/// controller.ts `refreshTrustOnce`: every pending leaf's user history is read once per pass from the oldest version any
/// of its leaves caches, ingested for that leaf, and the leaf's held rows are revalidated; the annotations the trust slice
/// shows (unlisted, history pending, an own leaf under another key) are kept in `memory`. Answers whether anything was
/// ingested.
pub async fn refresh_once(
    api: &Api,
    core: &CoreHandle,
    memory: &tokio::sync::Mutex<TrustMemory>,
    pass: &TrustPass,
    now_s: u64,
) -> bool {
    let Ok(Ok(leaves)) = with_core(core, |c| {
        c.pending_leaves().map(|raw| decode_pending_leaves(&raw))
    })
    .await
    else {
        return false;
    };
    let own = own_user(core).await;
    let before = memory.lock().await.annotations();
    {
        let mut m = memory.lock().await;
        // A user the core no longer reports in any active group has no history left to wait for.
        let present: HashSet<[u8; 16]> = leaves.iter().map(|l| l.user_id).collect();
        m.history_failed.retain(|u| present.contains(u));
        // A refused own leaf the core no longer reports has left the group (a validated Remove).
        let current: HashSet<([u8; 16], u32)> =
            leaves.iter().map(|l| (l.group_id, l.leaf_index)).collect();
        m.own_refused.retain(|l| current.contains(l));
    }
    // Users with a leaf whose signed history the core has never accepted; a successful ingest below caches their list.
    let mut unknown: HashSet<[u8; 16]> = leaves
        .iter()
        .filter(|l| l.reason == "unknown")
        .map(|l| l.user_id)
        .collect();
    let mut oldest: HashMap<[u8; 16], u64> = HashMap::new();
    for leaf in &leaves {
        let v = oldest.entry(leaf.user_id).or_insert(leaf.cached_version);
        *v = (*v).min(leaf.cached_version);
    }
    // Per user: the served head and its signed history, or `None` once a read of them failed this pass.
    let mut histories: HashMap<[u8; 16], Option<Signed>> = HashMap::new();
    let mut ingested = false;
    for leaf in leaves {
        let key = format!(
            "{}:{}:{}:{}:{}",
            crate::state::hex(&leaf.group_id),
            leaf.leaf_index,
            crate::state::hex(&leaf.user_id),
            leaf.cached_version,
            leaf.reason
        );
        if !memory.lock().await.fetched.insert(key) {
            continue;
        }
        let user = leaf.user_id;
        if let std::collections::hash_map::Entry::Vacant(slot) = histories.entry(user) {
            let from = oldest.get(&user).copied().unwrap_or(leaf.cached_version);
            let read = read_device_list_history(api, &user, from).await.ok();
            let mut m = memory.lock().await;
            if read.is_some() {
                m.history_failed.remove(&user);
            } else {
                m.history_failed.insert(user);
            }
            slot.insert(read);
        }
        let Some(Some((head, history))) = histories.get(&user).cloned() else {
            continue;
        };
        let Ok(head_list) = signed_head_list(&head) else {
            continue;
        };
        ingest_account_kind(core, user, &head).await;
        let (g, index) = (leaf.group_id, leaf.leaf_index);
        let verdict = with_core(core, move |c| {
            c.device_list_ingest(&g, index, &user, &history, &head_list, now_s)
        })
        .await;
        // A refusal (a leaf verdict, a stale read) keeps the leaf untrusted; the next ready retries. Only the core's own
        // leaf verdicts mark the user unlisted; an own-user history under another key is an own-trust signal instead.
        let verdict = match verdict {
            Ok(v) => v,
            Err(e) => {
                let mut m = memory.lock().await;
                if LEAF_VERDICTS.contains(&e.code) {
                    m.unlisted.insert(user);
                }
                if e.code == "E_UMK_CHANGED" && own == Some(user) {
                    m.own_refused.insert((g, index));
                }
                continue;
            }
        };
        let version = value::decode(&verdict).ok().and_then(|v| {
            v.as_array()
                .and_then(|a| a.first())
                .and_then(Value::as_uint)
        });
        // The core caches this version now and anchors the user's later ingests on it.
        if let (Some(version), Some(Some((head, history)))) =
            (version, histories.get(&user).cloned())
            && let Some(rest) = history_from(&history, version)
        {
            histories.insert(user, Some((head, rest)));
        }
        {
            let mut m = memory.lock().await;
            m.unlisted.remove(&user);
            m.own_refused.remove(&(g, index));
        }
        unknown.remove(&user);
        let _ = with_core(core, move |c| c.revalidate_pending(&g, index, 256)).await;
        ingested = true;
    }
    let after = {
        let mut m = memory.lock().await;
        let pending: HashSet<[u8; 16]> = m.history_failed.union(&unknown).copied().collect();
        m.pending = pending;
        m.annotations()
    };
    if ingested || after != before {
        pass.bump();
    }
    ingested
}

/// The engine's trust memory, for the trust area (`crate::features::trust`): the annotations it publishes and the
/// user a verification asks to read again.
pub(crate) async fn annotations_of(engine: &super::Engine) -> Annotations {
    engine.trust.lock().await.annotations()
}

pub(crate) async fn forget_user(engine: &super::Engine, user: &[u8; 16]) {
    engine.trust.lock().await.forget_user(user);
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn head_rows_and_history_slices() {
        let row = |v: u64| {
            Value::Array(vec![
                Value::Uint(v),
                Value::Bytes(vec![v as u8; 3]),
                Value::Bytes(vec![0; 64]),
                Value::Bytes(vec![0; 32]),
            ])
        };
        let mut head = row(3).as_array().unwrap().to_vec();
        head.push(Value::Bytes(vec![9; 5]));
        let head = Value::Array(head).encode();
        assert_eq!(signed_head_list(&head).unwrap(), vec![3; 3]);
        assert_eq!(head_attestation(&head).unwrap(), Some(vec![9; 5]));
        assert_eq!(head_attestation(&row(3).encode()).unwrap(), None);
        assert!(signed_head_list(&Value::Array(vec![Value::Uint(1)]).encode()).is_err());
        let history = Value::Array(vec![row(1), row(2), row(3)]).encode();
        assert_eq!(
            history_from(&history, 2),
            Some(Value::Array(vec![row(2), row(3)]).encode())
        );
        assert_eq!(history_from(&history, 4), None);
    }

    fn id16(n: u8) -> Value {
        Value::Bytes(vec![n; 16])
    }
    fn key32(n: u8) -> Value {
        Value::Bytes(vec![n; 32])
    }
    fn trust_row(verified: u64, changed: Value, blocked: Value) -> Value {
        Value::Array(vec![
            id16(2),
            key32(4),
            Value::Uint(12),
            Value::Uint(verified),
            changed,
            blocked,
        ])
    }

    // trust.test.ts "bounds core-owned pending and trust rows before copying".
    #[test]
    fn bounds_core_owned_pending_and_trust_rows() {
        let leaf = || {
            Value::Array(vec![
                id16(1),
                Value::Uint(0),
                id16(2),
                Value::Uint(0),
                Value::Text("unknown".into()),
            ])
        };
        let one = decode_pending_leaves(&Value::Array(vec![leaf()]).encode()).unwrap();
        assert_eq!((one[0].leaf_index, one[0].reason.as_str()), (0, "unknown"));
        let many = Value::Array((0..4097).map(|_| leaf()).collect()).encode();
        assert!(decode_pending_leaves(&many).is_err());
        let rows = decode_trust_state(
            &Value::Array(vec![trust_row(0, Value::Null, Value::Null)]).encode(),
        )
        .unwrap();
        assert_eq!(
            (
                rows[0].first_seen,
                rows[0].verified,
                rows[0].blocked.clone()
            ),
            (12, false, None)
        );
        let short_key = Value::Array(vec![Value::Array(vec![
            id16(2),
            Value::Bytes(vec![0; 31]),
            Value::Uint(12),
            Value::Uint(0),
            Value::Null,
            Value::Null,
        ])])
        .encode();
        assert!(decode_trust_state(&short_key).is_err());
    }

    // trust.test.ts "reads the ABI-8 blocked verdict of a trust row and refuses the ABI-7 five-element row".
    #[test]
    fn reads_the_blocked_verdict_and_refuses_abi_7_rows() {
        use crate::state::trust::TrustBlock;
        for code in [
            "E_UMK_CHANGED",
            "E_CREDENTIAL",
            "E_DEVICE_UNLISTED",
            "E_TIER_MISMATCH",
        ] {
            let rows = decode_trust_state(
                &Value::Array(vec![trust_row(1, Value::Null, Value::Text(code.into()))]).encode(),
            )
            .unwrap();
            assert!(rows[0].verified);
            assert_eq!(rows[0].blocked.as_deref(), Some(code));
        }
        let rows = decode_trust_state(
            &Value::Array(vec![trust_row(
                0,
                key32(5),
                Value::Text("E_UMK_CHANGED".into()),
            )])
            .encode(),
        )
        .unwrap();
        assert_eq!(rows[0].changed_umk, Some([5; 32]));
        // A code the page has no copy for fails closed as a failed identity check.
        let rows = decode_trust_state(
            &Value::Array(vec![trust_row(
                1,
                Value::Null,
                Value::Text("E_SOMETHING_NEW".into()),
            )])
            .encode(),
        )
        .unwrap();
        assert_eq!(
            TrustBlock::of(rows[0].blocked.as_deref().unwrap()),
            TrustBlock::Credential
        );
        let five = Value::Array(vec![Value::Array(vec![
            id16(2),
            key32(4),
            Value::Uint(12),
            Value::Uint(0),
            Value::Null,
        ])])
        .encode();
        assert!(decode_trust_state(&five).is_err());
        assert!(
            decode_trust_state(
                &Value::Array(vec![trust_row(0, Value::Null, Value::Uint(7))]).encode()
            )
            .is_err()
        );
        assert!(
            decode_trust_state(
                &Value::Array(vec![trust_row(2, Value::Null, Value::Null)]).encode()
            )
            .is_err()
        );
    }

    // trust.test.ts "answers null, never a partial or empty slice, for an input that leaves nothing to slice".
    #[test]
    fn history_slices_are_all_or_nothing() {
        let row = |v: u64| {
            Value::Array(vec![
                Value::Uint(v),
                Value::Bytes(vec![v as u8]),
                Value::Bytes(vec![0; 64]),
                key32(v as u8),
            ])
        };
        assert_eq!(history_from(&Value::Array(vec![]).encode(), 0), None);
        let two = Value::Array(vec![row(1), row(2)]).encode();
        assert_eq!(history_from(&two, 3), None);
        assert_eq!(
            history_from(&two, 2),
            Some(Value::Array(vec![row(2)]).encode())
        );
        let malformed = Value::Array(vec![
            row(1),
            Value::Array(vec![Value::Uint(2), Value::Bytes(vec![2])]),
        ])
        .encode();
        assert_eq!(history_from(&malformed, 1), None);
        assert_eq!(history_from(&Value::Uint(7).encode(), 0), None);
        assert_eq!(history_from(&[0xff], 1), None);
    }
    // Review fix 3 (controller.ts `trustRefresh` / `trustRefreshAgain`): one pass at a time; a request during a pass
    // runs exactly one more pass after it, however many came, and every caller returns after a pass that began after
    // its request.
    #[tokio::test(flavor = "multi_thread", worker_threads = 4)]
    async fn refresh_passes_are_single_flight_with_one_again() {
        use std::sync::atomic::{AtomicUsize, Ordering};
        let pass = std::sync::Arc::new(TrustPass::default());
        let (running, most, runs) = (
            std::sync::Arc::new(AtomicUsize::new(0)),
            std::sync::Arc::new(AtomicUsize::new(0)),
            std::sync::Arc::new(AtomicUsize::new(0)),
        );
        let mut tasks = Vec::new();
        for _ in 0..8 {
            let (pass, running, most, runs) =
                (pass.clone(), running.clone(), most.clone(), runs.clone());
            tasks.push(tokio::spawn(async move {
                pass.run(|| async {
                    let now = running.fetch_add(1, Ordering::SeqCst) + 1;
                    most.fetch_max(now, Ordering::SeqCst);
                    tokio::time::sleep(std::time::Duration::from_millis(50)).await;
                    runs.fetch_add(1, Ordering::SeqCst);
                    running.fetch_sub(1, Ordering::SeqCst);
                })
                .await;
            }));
        }
        for t in tasks {
            t.await.unwrap();
        }
        assert_eq!(most.load(Ordering::SeqCst), 1, "never two passes at once");
        let n = runs.load(Ordering::SeqCst);
        assert!(
            (1..=3).contains(&n),
            "the burst of 8 coalesced into {n} passes"
        );
        // Idle again: the next request runs its own pass.
        pass.run(|| async {
            runs.fetch_add(1, Ordering::SeqCst);
        })
        .await;
        assert_eq!(runs.load(Ordering::SeqCst), n + 1);
    }

    #[test]
    fn each_engine_has_its_own_revision() {
        let (a, b) = (TrustPass::default(), TrustPass::default());
        let (ra, rb) = (a.revisions(), b.revisions());
        a.bump();
        assert!(ra.has_changed().unwrap());
        assert!(
            !rb.has_changed().unwrap(),
            "one engine's pass does not wake another's watcher"
        );
    }
}
