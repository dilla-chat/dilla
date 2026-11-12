//! The trust refresh: a port of state/trust.ts (`readDeviceListHistory`, `signedHeadList`, `headAttestation`,
//! `historyFrom`, `ingestAccountKind`) and the controller's `refreshTrustOnce`. The server only supplies bytes; the core
//! authenticates every signature and leaf. A leaf whose user's signed device list the core has not accepted keeps its
//! messages held (`cannot-read`) until its history is ingested here and `revalidate_pending` releases them.

use std::collections::{HashMap, HashSet};

use crate::gateway::frames::Value;

use super::api::{Api, CoreHandle, with_core};
use super::value::{self, DecodeError, Rows, arr, bin16, text, u53, uint};

const MAX_VERSIONS: u64 = 1024;
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

pub fn decode_pending_leaves(raw: &[u8]) -> Result<Vec<PendingLeaf>, DecodeError> {
    Rows::of(raw)?
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

/// trust.ts `historyFrom`: the rows of `history` from `version` on, or `None` when it holds none.
pub fn history_from(history: &[u8], version: u64) -> Option<Vec<u8>> {
    let rows = Rows::of(history).ok()?;
    let kept: Vec<Value> = rows
        .iter()
        .filter(|row| {
            row.as_array()
                .and_then(|r| r.first())
                .and_then(Value::as_uint)
                .is_some_and(|v| v >= version)
        })
        .cloned()
        .collect();
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

/// What the trust refresh remembers between passes: the leaves already fetched (controller `fetchedLeaves`), so a leaf
/// is read once per (group, leaf, user, cached version, reason) until something about it changes.
#[derive(Default)]
pub struct TrustMemory {
    fetched: HashSet<String>,
}

/// A user's served device-list head and the signed rows read up to it.
type Signed = (Vec<u8>, Vec<u8>);

/// controller.ts `refreshTrustOnce`, without the UI's bookkeeping (unlisted and changed-key notices): every pending leaf's
/// user history is read once per pass from the oldest version any of its leaves caches, ingested for that leaf, and the
/// leaf's held rows are revalidated. Answers whether anything was ingested.
pub async fn refresh_once(
    api: &Api,
    core: &CoreHandle,
    memory: &tokio::sync::Mutex<TrustMemory>,
    now_s: u64,
) -> bool {
    let Ok(Ok(leaves)) = with_core(core, |c| {
        c.pending_leaves().map(|raw| decode_pending_leaves(&raw))
    })
    .await
    else {
        return false;
    };
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
            slot.insert(read_device_list_history(api, &user, from).await.ok());
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
        // A refusal (a leaf verdict, a stale read) keeps the leaf untrusted; the next ready retries.
        let Ok(verdict) = verdict else {
            continue;
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
        let _ = with_core(core, move |c| c.revalidate_pending(&g, index, 256)).await;
        ingested = true;
    }
    ingested
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
}
