//! Moderation-removal recovery: a port of `packages/client-core/src/sync/removals.ts` (`recoverModerationRemovals`) and
//! of `getModerationRemovals`' strict page decoder (`http/routes.ts`).
//!
//! A moderator's signed removal is stored by the instance in a per-group feed. A device that was offline when the
//! removal was made never saw its `message.deleted` (op 21), so before a community group's timeline is published the
//! device reads the feed from its durable cursor, refreshes each moderator's signed device-list history (so the proof's
//! signature is checked against a list the core accepted, not one the server just handed over), and ingests every entry:
//! the core verifies it and tombstones the target, so the removed plaintext is gone from the store before anything
//! shows it.
//!
//! Where it runs, as in the TS engine and the screens plan (Task 2): after every apply of a community group (the engine's
//! `apply_hook`, before the group is reported changed), when a community channel is opened, on each `message.deleted`
//! frame, and after every ready for every active community group ([`recover_moderation_removals`]).

use std::collections::HashMap;
use std::sync::Arc;

use dilla_core::bot::RemovalProof;
use dilla_core::client::RemovalFeedEntry;

use crate::client::Inner;
use crate::http::routes as r;
use crate::state::hex;
use crate::state::messages::TimelineUnavailable;

use super::SyncError;
use super::api::{Api, ApiError, CoreHandle, with_core};
use super::trust::{head_attestation, read_device_list_history, signed_head_list};
use super::value::{self, DecodeError, arr};

/// removals.ts `PAGE_LIMIT` (routes.ts `MODERATION_REMOVALS_PAGE`).
pub const PAGE_LIMIT: u64 = 250;
/// removals.ts `PAGE_BUDGET`: pages read per recovery before it gives up (E_REMOVAL_SYNC_LIMIT).
pub const PAGE_BUDGET: u32 = 8;

/// One page of the feed: `[removal_rows, next_revision]`, each row `[revision, proof, signature64, accepted_at]`.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct RemovalPage {
    pub rows: Vec<RemovalFeedEntry>,
    pub next_revision: u64,
}

/// Why a feed read failed, sorted for the retry: a transient failure (the network, a 5xx, a 429, an unknown outcome, a
/// history read racing a list change) is retried with the backoff; a terminal one (any other refusal, a feed that does
/// not hold together, a removal the core will not verify) stops the retries and is shown as the channel's state.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum FeedFailure {
    Transient(String),
    Terminal(TimelineUnavailable),
}

/// Sorts a feed read's error ([`FeedFailure`]).
pub fn classify(e: &SyncError) -> FeedFailure {
    const TRANSIENT_LOCAL: &[&str] = &[
        "E_NETWORK",
        "E_OUTCOME_UNKNOWN",
        "E_DEVICE_LIST_STALE",
        "E_REMOVAL_SYNC_LIMIT",
        "E_SYNC_STOPPED",
    ];
    match e {
        SyncError::Stopped => FeedFailure::Transient("E_SYNC_STOPPED".into()),
        SyncError::Api(a) if a.status == 0 && TRANSIENT_LOCAL.contains(&a.code.as_str()) => {
            FeedFailure::Transient(a.code.clone())
        }
        SyncError::Api(a) if a.status >= 500 || a.status == 429 => {
            FeedFailure::Transient(a.code.clone())
        }
        SyncError::Api(a) if a.status == 0 => {
            FeedFailure::Terminal(TimelineUnavailable::FeedInvalid {
                code: a.code.clone(),
            })
        }
        SyncError::Api(a) => FeedFailure::Terminal(TimelineUnavailable::FeedRefused {
            status: a.status,
            code: a.code.clone(),
        }),
        SyncError::Core(c) => FeedFailure::Terminal(TimelineUnavailable::RemovalUnverified {
            code: c.code.to_owned(),
        }),
        other => FeedFailure::Terminal(TimelineUnavailable::FeedInvalid { code: other.code() }),
    }
}

fn feed_error(detail: &str) -> SyncError {
    SyncError::Api(ApiError::local(0, "E_REMOVAL_FEED", detail))
}

/// routes.ts `getModerationRemovals`' decoder: at most `limit` rows, revisions one after another from `after`, and a
/// next revision that is exactly the last row's (or `after` for an empty page).
pub fn decode_page(raw: &[u8], after: u64, limit: u64) -> Result<RemovalPage, DecodeError> {
    let v = value::decode(raw)?;
    let tuple = arr(&v, 2)?;
    let rows = tuple[0]
        .as_array()
        .ok_or(DecodeError("moderation removal rows"))?;
    if rows.len() as u64 > limit {
        return Err(DecodeError("too many moderation removals"));
    }
    let next_revision = value::uint(&tuple[1])?;
    let mut expected = after;
    let mut out = Vec::with_capacity(rows.len());
    for row in rows {
        let a = arr(row, 4)?;
        expected = expected
            .checked_add(1)
            .ok_or(DecodeError("moderation removal revision"))?;
        let revision = value::uint(&a[0])?;
        if revision != expected {
            return Err(DecodeError("moderation removal revision gap"));
        }
        let proof = value::bytes(&a[1])?;
        let signature: [u8; 64] = a[2]
            .as_bytes()
            .and_then(|b| b.try_into().ok())
            .ok_or(DecodeError("moderation removal signature"))?;
        out.push(RemovalFeedEntry {
            revision,
            proof,
            signature,
            accepted_at: value::uint(&a[3])?,
        });
    }
    if next_revision != expected {
        return Err(DecodeError("moderation removal cursor mismatch"));
    }
    Ok(RemovalPage {
        rows: out,
        next_revision,
    })
}

/// removals.ts' checks of a page against the cursor it was read from (a decoder that let them through is no excuse).
fn check_page(page: &RemovalPage, cursor: u64) -> Result<(), SyncError> {
    if page.rows.is_empty() {
        if page.next_revision != cursor {
            return Err(feed_error("empty page advanced cursor"));
        }
        return Ok(());
    }
    if page.rows.len() as u64 > PAGE_LIMIT
        || page.next_revision != cursor.saturating_add(page.rows.len() as u64)
    {
        return Err(feed_error("invalid revision range"));
    }
    let mut expected = cursor;
    for row in &page.rows {
        expected += 1;
        if row.revision != expected {
            return Err(feed_error("revision gap"));
        }
    }
    Ok(())
}

/// The first entry of each actor in `page`, in the order the actors first appear; every proof must name `group`.
fn actors_of(
    page: &RemovalPage,
    group: &[u8; 16],
) -> Result<Vec<([u8; 16], RemovalFeedEntry)>, SyncError> {
    let mut seen: HashMap<[u8; 16], ()> = HashMap::new();
    let mut out = Vec::new();
    for row in &page.rows {
        let proof = RemovalProof::decode(&row.proof).map_err(|_| feed_error("malformed proof"))?;
        if proof.group_id.as_bytes() != group {
            return Err(feed_error("proof group mismatch"));
        }
        let actor = *proof.actor_user.as_bytes();
        if seen.insert(actor, ()).is_none() {
            out.push((actor, row.clone()));
        }
    }
    Ok(out)
}

async fn read_page(api: &Api, group: &[u8; 16], after: u64) -> Result<RemovalPage, SyncError> {
    #[cfg(feature = "testing")]
    if let Some((status, code)) = faults::take(group) {
        return Err(SyncError::Api(ApiError::local(
            status,
            code,
            "injected feed failure",
        )));
    }
    let answer = api
        .socket_call::<r::GetModerationRemovals>(
            &[&hex(group), &after.to_string(), &PAGE_LIMIT.to_string()],
            None,
            None,
            true,
        )
        .await?;
    Ok(decode_page(&answer.body, after, PAGE_LIMIT)?)
}

/// removals.ts `recoverModerationRemovals` for one community group: reads the feed from the core's durable cursor and
/// ingests every entry, each moderator's history refreshed first. Answers how many entries were ingested. Must run on
/// the group's lane (it reads and moves the group's cursor). Any error means the timeline is not to be published yet
/// (the next apply, ready or open tries again).
pub(crate) async fn recover_group(
    core: &CoreHandle,
    api: &Api,
    group: [u8; 16],
) -> Result<u64, SyncError> {
    // core-port.ts `removalCursor`: an ingest of nothing answers the cursor.
    let mut cursor = with_core(core, move |c| c.ingest_removals(&group, &[])).await?;
    let mut ingested = 0u64;
    for _ in 0..PAGE_BUDGET {
        let page = read_page(api, &group, cursor).await?;
        check_page(&page, cursor)?;
        if page.rows.is_empty() {
            return Ok(ingested);
        }
        // Each previously trusted actor's signed history is refreshed before any proof of the page is verified.
        for (actor, entry) in actors_of(&page, &group)? {
            let e = entry.clone();
            let cached =
                with_core(core, move |c| c.removal_actor_history_version(&group, &e)).await?;
            let (head, history) = read_device_list_history(api, &actor, cached)
                .await
                .map_err(|_| stale())?;
            ingest_account_kind(core, actor, &head).await;
            let list = signed_head_list(&head).map_err(|_| stale())?;
            with_core(core, move |c| {
                c.refresh_removal_actor_history(&group, &entry, &history, &list)
            })
            .await?;
        }
        for row in &page.rows {
            let (one, revision) = (row.clone(), row.revision);
            let persisted = with_core(core, move |c| {
                c.ingest_removals(&group, std::slice::from_ref(&one))
            })
            .await?;
            if persisted != revision {
                return Err(feed_error("core cursor mismatch"));
            }
            ingested += 1;
        }
        cursor = page.next_revision;
        if (page.rows.len() as u64) < PAGE_LIMIT {
            return Ok(ingested);
        }
    }
    Err(SyncError::Api(ApiError::local(
        0,
        "E_REMOVAL_SYNC_LIMIT",
        "feed remains ahead after bounded recovery",
    )))
}

fn stale() -> SyncError {
    SyncError::Api(ApiError::local(0, "E_DEVICE_LIST_STALE", ""))
}

/// trust.ts `ingestAccountKind`: a refusal is a withheld attestation (nothing stored, the history refresh still runs),
/// so no attestation outcome stalls the feed.
async fn ingest_account_kind(core: &CoreHandle, user: [u8; 16], head: &[u8]) {
    let Ok(Some(attestation)) = head_attestation(head) else {
        return;
    };
    let _ = with_core(core, move |c| c.account_kind_ingest(&user, &attestation)).await;
}

/// The screens plan's entry point (Task 2): every active community group's feed is read again on its lane, and this
/// answers once all of them have run; a group whose recovery ingested anything is reported changed, so its timeline
/// shows the tombstones. The client runs it after every ready (a removal made while this device was away arrives with no
/// `message.deleted` frame) and reads the timelines only after it, so a removed message's plaintext is never published.
///
/// Answers the groups whose feed is still unread, with why ([`FeedFailure`]): the client keeps their timelines
/// unpublished, retries the transient ones ([`retry_moderation_removals`]) and shows the terminal ones as the channel's
/// state.
pub(crate) async fn recover_moderation_removals(inner: &Inner) -> HashMap<[u8; 16], FeedFailure> {
    Arc::clone(inner.engine()).recover_removals_all().await
}

/// The retry of [`recover_moderation_removals`]' transient groups: answers those still unread.
pub(crate) async fn retry_moderation_removals(
    inner: &Inner,
    groups: Vec<[u8; 16]>,
) -> HashMap<[u8; 16], FeedFailure> {
    Arc::clone(inner.engine()).recover_removals_of(groups).await
}

/// Test-only fault injection (feature `testing`): the next `n` feed reads of a group fail as an unreachable instance
/// would (`E_NETWORK`), before any request is written.
#[cfg(feature = "testing")]
pub mod faults {
    use std::collections::HashMap;
    use std::sync::Mutex;

    /// Per group: how many reads still fail, with what status and code, and how many were failed so far.
    type Faults = HashMap<[u8; 16], (u32, u16, &'static str, u32)>;
    static FAIL: Mutex<Option<Faults>> = Mutex::new(None);

    fn faults() -> std::sync::MutexGuard<'static, Option<Faults>> {
        FAIL.lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
    }

    /// The next `n` reads of `group`'s removal feed fail as an unreachable instance would (`E_NETWORK`).
    pub fn fail_next_feed_reads(group: [u8; 16], n: u32) {
        fail_next_feed_reads_with(group, n, 0, "E_NETWORK");
    }

    /// The next `n` reads of `group`'s removal feed are refused with `status` and `code` (0 for a local failure).
    pub fn fail_next_feed_reads_with(group: [u8; 16], n: u32, status: u16, code: &'static str) {
        faults()
            .get_or_insert_with(HashMap::new)
            .insert(group, (n, status, code, 0));
    }

    /// How many reads of `group`'s feed were failed so far.
    pub fn failed_reads(group: [u8; 16]) -> u32 {
        faults()
            .as_ref()
            .and_then(|m| m.get(&group))
            .map_or(0, |f| f.3)
    }

    pub(super) fn take(group: &[u8; 16]) -> Option<(u16, &'static str)> {
        let mut fail = faults();
        match fail.as_mut().and_then(|m| m.get_mut(group)) {
            Some((n, status, code, done)) if *n > 0 => {
                *n -= 1;
                *done += 1;
                Some((*status, *code))
            }
            _ => None,
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::gateway::frames::Value;

    fn page(rows: &[(u64, Vec<u8>)], next: u64) -> Vec<u8> {
        Value::Array(vec![
            Value::Array(
                rows.iter()
                    .map(|(rev, proof)| {
                        Value::Array(vec![
                            Value::Uint(*rev),
                            Value::Bytes(proof.clone()),
                            Value::Bytes(vec![4; 64]),
                            Value::Uint(20),
                        ])
                    })
                    .collect(),
            ),
            Value::Uint(next),
        ])
        .encode()
    }

    #[test]
    fn the_page_decoder_holds_routes_ts_bounds() {
        let ok = decode_page(&page(&[(4, vec![1]), (5, vec![2])], 5), 3, 250).unwrap();
        assert_eq!(ok.rows.len(), 2);
        assert_eq!(ok.next_revision, 5);
        assert_eq!(decode_page(&page(&[], 3), 3, 250).unwrap().rows, vec![]);
        // A gap, a cursor that does not match the rows, an empty page that moves the cursor, too many rows.
        assert!(decode_page(&page(&[(5, vec![1])], 5), 3, 250).is_err());
        assert!(decode_page(&page(&[(4, vec![1])], 6), 3, 250).is_err());
        assert!(decode_page(&page(&[], 4), 3, 250).is_err());
        assert!(decode_page(&page(&[(1, vec![1]), (2, vec![1])], 2), 0, 1).is_err());
    }

    /// Review fix (round 2): transient feed failures keep the retry; every other one is terminal and becomes the
    /// channel's state.
    #[test]
    fn feed_failures_are_sorted_for_the_retry() {
        let api = |status, code: &str| SyncError::Api(ApiError::local(status, code, ""));
        for transient in [
            api(0, "E_NETWORK"),
            api(0, "E_OUTCOME_UNKNOWN"),
            api(0, "E_DEVICE_LIST_STALE"),
            api(503, "E_UNAVAILABLE"),
            api(500, "E_INTERNAL"),
            api(429, "E_RATE_LIMITED"),
            SyncError::Stopped,
        ] {
            assert!(
                matches!(classify(&transient), FeedFailure::Transient(_)),
                "{transient:?}"
            );
        }
        assert_eq!(
            classify(&api(403, "E_FORBIDDEN")),
            FeedFailure::Terminal(TimelineUnavailable::FeedRefused {
                status: 403,
                code: "E_FORBIDDEN".into()
            })
        );
        assert!(matches!(
            classify(&api(404, "E_NOT_FOUND")),
            FeedFailure::Terminal(TimelineUnavailable::FeedRefused { status: 404, .. })
        ));
        assert_eq!(
            classify(&feed_error("revision gap")),
            FeedFailure::Terminal(TimelineUnavailable::FeedInvalid {
                code: "E_REMOVAL_FEED".into()
            })
        );
        let core = SyncError::Core(dilla_core::client::ClientError {
            code: "E_CORE_INPUT",
            detail: "moderator has no pinned user key".into(),
        });
        assert_eq!(
            classify(&core),
            FeedFailure::Terminal(TimelineUnavailable::RemovalUnverified {
                code: "E_CORE_INPUT".into()
            })
        );
    }

    #[test]
    fn a_revision_gap_refuses_publication() {
        // removals.test.ts "refuses to publish when the feed has a revision gap": a page that skips revision 1.
        let gap = RemovalPage {
            rows: vec![RemovalFeedEntry {
                revision: 2,
                proof: vec![1],
                signature: [4; 64],
                accepted_at: 20,
            }],
            next_revision: 2,
        };
        let err = check_page(&gap, 0).unwrap_err();
        assert_eq!(err.code(), "E_REMOVAL_FEED");
        assert!(err.to_string().contains("revision range"), "{err}");
        let empty_moved = RemovalPage {
            rows: vec![],
            next_revision: 1,
        };
        assert!(check_page(&empty_moved, 0).is_err());
    }
}
