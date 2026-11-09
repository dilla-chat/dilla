//! The fold of envelope types 1..6 onto their targets (protocol/04 § Semantics; L-CORE-33).
//! Derived rows are recomputed in the caller's storage unit; nothing here refuses.

use super::wire;
use crate::cbor::{CborError, decode_strict};
use crate::mls::StorageError;
use rusqlite::{OptionalExtension, params};

pub(super) struct FoldTrigger {
    pub seq: u64,
    pub ty: u8,
    pub sender_user: [u8; 16],
    pub sender_device: [u8; 16],
}

pub(super) struct Target {
    pub seq: u64,
    pub status: i64,
    pub sender_user: Option<[u8; 16]>,
    pub shown_body: String,
}

/// The target of `msg_id`: the earliest type-0 row with it, shown or deleted, unless a row of
/// lower seq already names it (a fold, or a reply's `reply_to`): that type 0 is a repeat of an
/// original this device does not hold (deleted before it was fetched, undecryptable, or sent
/// before this device joined), and a repeat is never a target (protocol/04 § Semantics, Target).
/// Every later candidate has the same lower-seq row before it, so only the earliest is checked.
pub(super) const RESOLVE_SQL: &str = "SELECT m.seq, m.status, m.sender_user, COALESCE(m.edit_body, m.body) FROM (SELECT seq, status, sender_user, edit_body, body FROM app_messages INDEXED BY app_messages_by_msg WHERE group_id = ?1 AND msg_id = ?2 AND type = 0 AND status IN (0, 2) ORDER BY seq LIMIT 1) AS m WHERE NOT EXISTS (SELECT 1 FROM app_messages AS r INDEXED BY app_messages_by_pin WHERE r.group_id = ?1 AND r.reply_to = ?2 AND r.type IN (0, 1, 2, 3, 4, 5, 6) AND r.seq < m.seq)";

pub(super) fn resolve(
    c: &rusqlite::Connection,
    group_id: &[u8; 16],
    msg_id: &[u8; 16],
) -> Result<Option<Target>, StorageError> {
    c.query_row(
        RESOLVE_SQL,
        params![group_id.as_slice(), msg_id.as_slice()],
        |r| {
            Ok(Target {
                seq: r.get::<_, i64>(0)? as u64,
                status: r.get(1)?,
                sender_user: r.get(2)?,
                shown_body: r.get(3)?,
            })
        },
    )
    .optional()
    .map_err(Into::into)
}

/// The blob ids of a stored envelope's attachments, by structural walk (L-CORE-33 step 2,
/// FACTS-SECURITY-06): never `Envelope::decode`, never an error.
fn stored_blob_ids(envelope: &[u8]) -> Vec<[u8; 32]> {
    decode_strict(envelope, |d| {
        d.array(9)?;
        for _ in 0..6 {
            d.skip()?;
        }
        let n = d.array_len()?;
        let mut ids = Vec::new();
        for _ in 0..n {
            let m = d.array_len()?;
            if m == 0 {
                return Err(CborError::WrongArrayLen {
                    expected: 1,
                    actual: 0,
                });
            }
            ids.push(d.bytes_exact::<32>()?);
            for _ in 1..m {
                d.skip()?;
            }
        }
        d.skip()?;
        d.skip()?;
        Ok(ids)
    })
    .unwrap_or_default()
}

pub(super) fn target_of(
    ty: Option<i64>,
    msg_id: Option<[u8; 16]>,
    reply_to: Option<[u8; 16]>,
) -> Option<[u8; 16]> {
    match ty {
        Some(0) => msg_id,
        Some(1..=6) => reply_to,
        _ => None,
    }
}

pub(super) fn excerpt(body: &str) -> String {
    body.chars()
        .take(120)
        .map(|ch| if ch == '\n' || ch == '\r' { ' ' } else { ch })
        .collect()
}

pub(super) enum FoldScope {
    Full,
    Edit,
    Delete { user: [u8; 16] },
    Reaction { user: [u8; 16], emoji: String },
    Pin,
}

pub(super) fn scope_of(ty: Option<i64>, sender_user: Option<[u8; 16]>, body: &str) -> FoldScope {
    match (ty, sender_user) {
        (Some(1), _) => FoldScope::Edit,
        (Some(2), Some(user)) => FoldScope::Delete { user },
        (Some(3 | 4), Some(user)) => FoldScope::Reaction {
            user,
            emoji: body.to_owned(),
        },
        (Some(5 | 6), _) => FoldScope::Pin,
        _ => FoldScope::Full,
    }
}

pub(super) const UNHELD_DELETERS: &str = "SELECT DISTINCT sender_user FROM app_messages INDEXED BY app_messages_by_reply WHERE group_id = ?1 AND reply_to = ?2 AND type = 2 AND status = 0 AND sender_user IS NOT NULL";
pub(super) const BLANK_EDITS_OF: &str = "UPDATE app_messages INDEXED BY app_messages_by_reply SET body = '', envelope = NULL WHERE group_id = ?1 AND reply_to = ?2 AND type = 1 AND status = 0 AND sender_user = ?3";
// The statements that read or blank the fold rows of a held target carry its seq as their last
// parameter: a fold counts only when it was sequenced after its target (`seq > target seq`). On a
// target never held (`AUTHOR_DELETED` there) there is no bound (`i64::MIN`).
pub(super) const AUTHOR_DELETED: &str = "SELECT 1 FROM app_messages INDEXED BY app_messages_by_reply WHERE group_id = ?1 AND reply_to = ?2 AND type = 2 AND sender_user = ?3 AND seq > ?4 AND status = 0 LIMIT 1";
pub(super) const FIRST_UNHELD_DELETE: &str = "SELECT seq FROM app_messages INDEXED BY app_messages_by_reply WHERE group_id = ?1 AND reply_to = ?2 AND type = 2 AND sender_user = ?3 AND status = 0 ORDER BY seq LIMIT 1";
pub(super) const BLANK_EDITS: &str = "UPDATE app_messages INDEXED BY app_messages_by_reply SET body = '', envelope = NULL WHERE group_id = ?1 AND reply_to = ?2 AND type = 1 AND sender_user = ?3 AND seq > ?4";
pub(super) const LATEST_EDIT: &str = "SELECT body, seq FROM app_messages INDEXED BY app_messages_by_reply WHERE group_id = ?1 AND reply_to = ?2 AND type = 1 AND sender_user = ?3 AND seq > ?4 AND status = 0 ORDER BY seq DESC LIMIT 1";
pub(super) const REACTIONS_FULL: &str = "INSERT INTO app_reactions (group_id, target, user_id, emoji, seq) SELECT ?1, ?2, u, b, s FROM (SELECT sender_user AS u, body AS b, type AS t, MAX(seq) AS s FROM app_messages INDEXED BY app_messages_by_reaction WHERE group_id = ?1 AND reply_to = ?2 AND seq > ?3 AND status = 0 AND type IN (3, 4) AND sender_user IS NOT NULL GROUP BY sender_user, body) WHERE t = 3";
pub(super) const REACTION_PAIR: &str = "SELECT type, seq FROM app_messages INDEXED BY app_messages_by_reaction WHERE group_id = ?1 AND reply_to = ?2 AND sender_user = ?3 AND body = ?4 AND seq > ?5 AND status = 0 AND type IN (3, 4) ORDER BY seq DESC LIMIT 1";
/// The latest pin (type 5) or unpin (6) of a target that counts: from any member while the group
/// has no known role view, else only from a sender the view allows `pin_messages` (bit 3) now.
/// An unauthorized 5 is ignored and an unauthorized 6 cannot unpin (protocol/04 § Semantics).
pub(super) const PIN_LATEST: &str = "SELECT seq, sender_user FROM app_messages INDEXED BY app_messages_by_pin WHERE group_id = ?1 AND reply_to = ?2 AND type = ?3 AND seq > ?4 AND status = 0 \
    AND (NOT EXISTS (SELECT 1 FROM app_authority_known k WHERE k.group_id = ?1) \
         OR sender_user IN (SELECT a.user_id FROM app_authority a WHERE a.group_id = ?1 AND a.bit = 3)) \
    ORDER BY seq DESC LIMIT 1";

/// Recompute the portion covered by `scope`; inserted triggers touch bounded rows.
pub(super) fn refold(
    c: &rusqlite::Connection,
    group_id: &[u8; 16],
    target: &[u8; 16],
    scope: &FoldScope,
    trigger: Option<&FoldTrigger>,
    own: Option<&super::Own>,
) -> Result<(), StorageError> {
    let g = group_id.as_slice();
    let id = target.as_slice();
    let Some(t) = resolve(c, group_id, target)? else {
        match (scope, trigger) {
            (FoldScope::Edit, Some(tr)) => {
                let deleted = c
                    .query_row(
                        AUTHOR_DELETED,
                        params![g, id, tr.sender_user.as_slice(), i64::MIN],
                        |_| Ok(()),
                    )
                    .optional()?
                    .is_some();
                if deleted {
                    c.execute("UPDATE app_messages SET body = '', envelope = NULL WHERE group_id = ?1 AND seq = ?2", params![g, tr.seq as i64])?;
                    super::bot_events::suppress_source(c, group_id, tr.seq as i64)?;
                }
            }
            (FoldScope::Delete { user }, Some(tr)) => {
                let first: Option<i64> = c
                    .query_row(FIRST_UNHELD_DELETE, params![g, id, user.as_slice()], |r| {
                        r.get(0)
                    })
                    .optional()?;
                if first == Some(tr.seq as i64) {
                    c.execute(BLANK_EDITS_OF, params![g, id, user.as_slice()])?;
                    super::bot_events::suppress_edits_of(c, group_id, target, user, None)?;
                }
            }
            // A type 0 that is no target (a repeat) leaves every parked row as it is: its insert
            // writes only its own row (S1).
            (FoldScope::Full, None) | (FoldScope::Edit, None) => {
                let users: Vec<[u8; 16]> = {
                    let mut q = c.prepare(UNHELD_DELETERS)?;
                    q.query_map(params![g, id], |r| r.get(0))?
                        .collect::<Result<_, _>>()?
                };
                for user in users {
                    c.execute(BLANK_EDITS_OF, params![g, id, user.as_slice()])?;
                    super::bot_events::suppress_edits_of(c, group_id, id, &user, None)?;
                }
            }
            _ => {}
        }
        return Ok(());
    };
    // A trigger sequenced before the target is no fold of it, and a type 0 other than the target
    // is a repeat: neither changes the target.
    if let Some(tr) = trigger
        && (tr.seq < t.seq || tr.ty == 0 && tr.seq != t.seq)
    {
        return Ok(());
    }
    let bound = t.seq as i64;
    if let FoldScope::Delete { user } = scope {
        if t.sender_user != Some(*user) {
            return Ok(());
        }
        if t.status == 2 {
            return Ok(());
        }
    }
    if t.status == 2 && !matches!(scope, FoldScope::Full) {
        if let Some(tr) = trigger {
            if tr.ty == 1 {
                c.execute("UPDATE app_messages SET body = '', envelope = NULL WHERE group_id = ?1 AND seq = ?2", params![g, tr.seq as i64])?;
                super::bot_events::suppress_source(c, group_id, tr.seq as i64)?;
            }
            return Ok(());
        }
        return Ok(());
    }
    let author_deleted = if let Some(user) = t.sender_user {
        c.query_row(
            AUTHOR_DELETED,
            params![g, id, user.as_slice(), bound],
            |_| Ok(()),
        )
        .optional()?
        .is_some()
    } else {
        false
    };
    if t.status == 2 || author_deleted {
        // The purge record (L-CORE-33 step 2, task 3). Attacker: nothing is refused here. The record
        // is written only for this device's own type-2 envelope of its own user's status-0 message,
        // so no other member, and no other device of the same user, can make an honest device hold
        // a purge it did not send; a forged type 2 from another member stops at the author rule
        // above. The blob ids come from a structural walk that never fails, so a stored envelope
        // this build's decoder refuses can never make the own delete unconfirmable or stall
        // `group_apply` (FACTS-SECURITY-06).
        if let (Some(trigger), Some(own)) = (trigger, own)
            && trigger.ty == 2
            && trigger.sender_device == own.device_id
            && trigger.sender_user == own.user_id
            && t.sender_user == Some(own.user_id)
            && t.status == 0
        {
            let channel_id: Vec<u8> = c
                .query_row(
                    "SELECT target_id FROM app_groups WHERE group_id = ?1",
                    [g],
                    |r| r.get(0),
                )
                .optional()?
                .ok_or_else(|| StorageError::Sqlite("app_groups row missing".into()))?;
            let envelope: Option<Vec<u8>> = c.query_row(
                "SELECT envelope FROM app_messages WHERE group_id = ?1 AND seq = ?2",
                params![g, t.seq as i64],
                |r| r.get(0),
            )?;
            let ids = envelope.as_deref().map(stored_blob_ids).unwrap_or_default();
            c.execute(
                "INSERT OR IGNORE INTO app_purges (group_id, seq, channel_id, blob_ids) VALUES (?1, ?2, ?3, ?4)",
                params![g, t.seq as i64, channel_id, wire::encode_blob_ids(&ids)],
            )?;
        }
        c.execute("UPDATE app_messages SET status = 2, body = '', envelope = NULL, reason = '', edit_body = NULL, edit_seq = 0 WHERE group_id = ?1 AND seq = ?2", params![g, t.seq as i64])?;
        super::bot_events::suppress_source(c, group_id, t.seq as i64)?;
        if let Some(user) = t.sender_user {
            c.execute(BLANK_EDITS, params![g, id, user.as_slice(), bound])?;
            super::bot_events::suppress_edits_of(c, group_id, id, &user, Some(bound))?;
        }
        c.execute(
            "DELETE FROM app_reactions WHERE group_id = ?1 AND target = ?2",
            params![g, id],
        )?;
        c.execute(
            "DELETE FROM app_pins WHERE group_id = ?1 AND target = ?2",
            params![g, id],
        )?;
        return Ok(());
    }
    if matches!(scope, FoldScope::Full | FoldScope::Edit) {
        let latest: Option<(String, i64)> = if let Some(user) = t.sender_user {
            c.query_row(LATEST_EDIT, params![g, id, user.as_slice(), bound], |r| {
                Ok((r.get(0)?, r.get(1)?))
            })
            .optional()?
        } else {
            None
        };
        c.execute("UPDATE app_messages SET edit_body = ?3, edit_seq = ?4 WHERE group_id = ?1 AND seq = ?2",
            params![g, t.seq as i64, latest.as_ref().map(|x| x.0.as_str()), latest.as_ref().map_or(0, |x| x.1)])?;
    }
    match scope {
        FoldScope::Full => {
            c.execute(
                "DELETE FROM app_reactions WHERE group_id = ?1 AND target = ?2",
                params![g, id],
            )?;
            c.execute(REACTIONS_FULL, params![g, id, bound])?;
        }
        FoldScope::Reaction { user, emoji } => {
            let pair: Option<(i64, i64)> = c
                .query_row(
                    REACTION_PAIR,
                    params![g, id, user.as_slice(), emoji, bound],
                    |r| Ok((r.get(0)?, r.get(1)?)),
                )
                .optional()?;
            if let Some((3, seq)) = pair {
                c.execute("INSERT INTO app_reactions (group_id, target, user_id, emoji, seq) VALUES (?1, ?2, ?3, ?4, ?5) ON CONFLICT (group_id, target, user_id, emoji) DO UPDATE SET seq = excluded.seq", params![g, id, user.as_slice(), emoji, seq])?;
            } else {
                c.execute("DELETE FROM app_reactions WHERE group_id = ?1 AND target = ?2 AND user_id = ?3 AND emoji = ?4", params![g, id, user.as_slice(), emoji])?;
            }
        }
        _ => {}
    }
    if matches!(scope, FoldScope::Full | FoldScope::Pin) {
        let pin: Option<(i64, Option<[u8; 16]>)> = c
            .query_row(PIN_LATEST, params![g, id, 5, bound], |r| {
                Ok((r.get(0)?, r.get(1)?))
            })
            .optional()?;
        let unpin: Option<i64> = c
            .query_row(PIN_LATEST, params![g, id, 6, bound], |r| r.get(0))
            .optional()?;
        if let Some((seq, Some(user))) = pin
            && unpin.is_none_or(|s| seq > s)
        {
            c.execute("INSERT INTO app_pins (group_id, target, seq, by_user) VALUES (?1, ?2, ?3, ?4) ON CONFLICT (group_id, target) DO UPDATE SET seq = excluded.seq, by_user = excluded.by_user", params![g, id, seq, user.as_slice()])?;
        } else {
            c.execute(
                "DELETE FROM app_pins WHERE group_id = ?1 AND target = ?2",
                params![g, id],
            )?;
        }
    }
    Ok(())
}

#[cfg(all(test, not(target_arch = "wasm32")))]
mod tests {
    use super::*;

    fn plan(c: &rusqlite::Connection, sql: &str) -> String {
        let mut st = c
            .prepare(&format!("EXPLAIN QUERY PLAN {sql}"))
            .expect("prepare");
        let n = st.parameter_count();
        let nulls = std::iter::repeat_n(rusqlite::types::Null, n);
        let rows = st
            .query_map(rusqlite::params_from_iter(nulls), |r| r.get::<_, String>(3))
            .expect("plan");
        rows.map(|r| r.expect("detail"))
            .collect::<Vec<_>>()
            .join("\n")
    }

    /// A store at app schema 5 (web3.7): the thread column and its index.
    fn v5(c: &rusqlite::Connection) {
        c.execute_batch(super::super::schema::MIGRATE_V4_TO_V5)
            .expect("thread column");
        c.execute_batch(super::super::schema::APP_INDEXES_V5)
            .expect("thread index");
    }

    #[test]
    fn every_fold_statement_is_an_index_search() {
        let c = rusqlite::Connection::open_in_memory().expect("open");
        c.execute_batch(super::super::schema::APP_SCHEMA)
            .expect("schema");
        c.execute_batch(super::super::schema::APP_INDEXES_V3)
            .expect("indexes");
        c.execute_batch(super::super::schema::APP_AUTHORITY)
            .expect("authority");
        v5(&c);
        let cases: [(&str, &str, &str); 9] = [
            (
                "UNHELD_DELETERS",
                UNHELD_DELETERS,
                "(group_id=? AND reply_to=? AND type=?",
            ),
            (
                "BLANK_EDITS_OF",
                BLANK_EDITS_OF,
                "(group_id=? AND reply_to=? AND type=? AND sender_user=?)",
            ),
            (
                "AUTHOR_DELETED",
                AUTHOR_DELETED,
                "(group_id=? AND reply_to=? AND type=? AND sender_user=? AND seq>?)",
            ),
            (
                "FIRST_UNHELD_DELETE",
                FIRST_UNHELD_DELETE,
                "(group_id=? AND reply_to=? AND type=? AND sender_user=?)",
            ),
            (
                "BLANK_EDITS",
                BLANK_EDITS,
                "(group_id=? AND reply_to=? AND type=? AND sender_user=? AND seq>?)",
            ),
            (
                "LATEST_EDIT",
                LATEST_EDIT,
                "(group_id=? AND reply_to=? AND type=? AND sender_user=? AND seq>?)",
            ),
            (
                "REACTIONS_FULL",
                REACTIONS_FULL,
                "(group_id=? AND reply_to=?",
            ),
            (
                "REACTION_PAIR",
                REACTION_PAIR,
                "(group_id=? AND reply_to=? AND sender_user=? AND body=? AND seq>?)",
            ),
            (
                "PIN_LATEST",
                PIN_LATEST,
                "(group_id=? AND reply_to=? AND type=? AND seq>?)",
            ),
        ];
        for (name, sql, want) in cases {
            let p = plan(&c, sql);
            assert!(!p.contains("SCAN app_messages"), "{name} scans: {p}");
            assert!(
                p.contains("INDEX app_messages_by_"),
                "{name} uses no fold index: {p}"
            );
            assert!(p.contains(want), "{name} is not bounded by {want}: {p}");
        }
        let timeline = super::super::messages::timeline_sql();
        for (name, sql, search, forbidden) in [
            (
                "RESOLVE_SQL",
                RESOLVE_SQL,
                "SEARCH app_messages USING INDEX app_messages_by_msg (group_id=? AND msg_id=?",
                "SEARCH app_messages USING PRIMARY KEY (group_id=?)",
            ),
            (
                "TIMELINE_SQL",
                timeline.as_str(),
                "SEARCH e USING INDEX app_messages_by_msg (group_id=? AND msg_id=?",
                "SEARCH e USING PRIMARY KEY (group_id=?)",
            ),
        ] {
            let p = plan(&c, sql);
            assert!(
                p.contains(search),
                "{name} does not search by message id: {p}"
            );
            assert!(
                !p.contains(forbidden),
                "{name} scans the group primary key: {p}"
            );
            // The repeat rule: one bounded probe per type for a lower-seq row naming the id.
            assert!(
                p.contains(
                    "SEARCH r USING COVERING INDEX app_messages_by_pin (group_id=? AND reply_to=? AND type=? AND seq<?)"
                ),
                "{name} does not bound the repeat check by seq: {p}"
            );
            // `SCAN m` in RESOLVE_SQL reads the one-row co-routine of the earliest candidate.
            assert!(
                p.lines().all(|l| !l.starts_with("SCAN") || l == "SCAN m"),
                "{name} scans: {p}"
            );
        }
    }

    /// web3.7 TB1: the thread summary and the held-root exclusion of the main timeline, the thread
    /// reads and `threads` are index searches: the summary probes `app_messages_by_thread` by
    /// (group, thread, type) after the root's seq (fix round 1, I1), the exclusion and the
    /// first-root checks probe `app_messages_by_msg` by (group, msg id).
    #[test]
    fn the_timeline_plan_uses_the_thread_index() {
        let c = rusqlite::Connection::open_in_memory().expect("open");
        c.execute_batch(super::super::schema::APP_SCHEMA)
            .expect("schema");
        c.execute_batch(super::super::schema::APP_INDEXES_V3)
            .expect("indexes");
        v5(&c);
        let summary = "SEARCH s USING INDEX app_messages_by_thread (group_id=? AND thread_id=? AND type=? AND seq>?";
        let (timeline, thread, root) = (
            super::super::messages::timeline_sql(),
            super::super::messages::thread_sql(),
            super::super::messages::thread_root_sql(),
        );
        let p = plan(&c, &timeline);
        assert!(
            p.contains(summary),
            "the summary does not search app_messages_by_thread: {p}"
        );
        assert!(
            p.contains(
                "SEARCH t USING INDEX app_messages_by_msg (group_id=? AND msg_id=? AND seq<?)"
            ),
            "the held-root exclusion does not search by message id: {p}"
        );
        for (name, sql, main) in [
            (
                "THREAD_SQL",
                thread.as_str(),
                "SEARCH m USING INDEX app_messages_by_thread (group_id=? AND thread_id=? AND type=? AND seq>?",
            ),
            (
                "THREAD_ROOT_SQL",
                root.as_str(),
                "SEARCH m USING INDEX app_messages_by_msg (group_id=? AND msg_id=?)",
            ),
            (
                "THREADS_SQL",
                super::super::messages::THREADS_SQL,
                "SEARCH x USING INDEX app_messages_by_thread (group_id=? AND thread_id>?)",
            ),
        ] {
            let p = plan(&c, sql);
            assert!(p.contains(main), "{name} is not bounded by its index: {p}");
            assert!(
                p.lines().all(|l| !l.starts_with("SCAN")
                    || l.starts_with("SCAN m") && name == "THREAD_ROOT_SQL"),
                "{name} scans: {p}"
            );
        }
        assert!(
            plan(&c, &thread).contains(summary),
            "THREAD_SQL's summary does not search app_messages_by_thread"
        );
    }

    #[test]
    fn delete_scope_and_replayed_target_return_without_writing() {
        assert!(
            matches!(scope_of(Some(2), Some([9; 16]), ""), FoldScope::Delete { user } if user == [9; 16])
        );
        let c = rusqlite::Connection::open_in_memory().expect("open");
        c.execute_batch(super::super::schema::APP_SCHEMA)
            .expect("schema");
        c.execute_batch(super::super::schema::APP_INDEXES_V3)
            .expect("indexes");
        c.execute_batch(super::super::schema::APP_AUTHORITY)
            .expect("authority");
        let group = [1u8; 16];
        let target = [2u8; 16];
        let author = [3u8; 16];
        let mallory = [9u8; 16];
        c.execute("INSERT INTO app_messages (group_id,seq,epoch,recv_ts,status,sender_user,sender_device,msg_id,type,franking_tag) VALUES (?1,1,0,0,2,?2,?2,?3,0,?4)",
            params![group.as_slice(), author.as_slice(), target.as_slice(), [0u8; 32].as_slice()]).expect("target");
        let changes = c.total_changes();
        let tr = FoldTrigger {
            seq: 2,
            ty: 2,
            sender_user: mallory,
            sender_device: mallory,
        };
        refold(
            &c,
            &group,
            &target,
            &scope_of(Some(2), Some(mallory), ""),
            Some(&tr),
            None,
        )
        .expect("ignored delete");
        assert_eq!(c.total_changes(), changes);
        let replay = FoldTrigger {
            seq: 3,
            ty: 0,
            sender_user: author,
            sender_device: author,
        };
        refold(&c, &group, &target, &FoldScope::Full, Some(&replay), None).expect("replay");
        assert_eq!(c.total_changes(), changes);
    }

    /// Lead "fold spoofing" (2026-10-07): a member who repeats the msg id of an original this
    /// receiver does not hold (DS-deleted before it was fetched, pruned, or sent before a join)
    /// must not collect the reactions, the pin or the edits other users sent for the original. A
    /// type 0 whose msg id a held row of lower seq already names is a repeat and never a target,
    /// and a fold counts only for a target of lower seq.
    #[test]
    fn a_repeat_of_an_unheld_original_collects_no_earlier_fold() {
        let c = rusqlite::Connection::open_in_memory().expect("open");
        c.execute_batch(super::super::schema::APP_SCHEMA)
            .expect("schema");
        c.execute_batch(super::super::schema::APP_INDEXES_V3)
            .expect("indexes");
        c.execute_batch(super::super::schema::APP_AUTHORITY)
            .expect("authority");
        c.execute_batch(super::super::schema::APP_BOT_EVENTS)
            .expect("Bot event journal");
        let g = [1u8; 16];
        let x = [2u8; 16];
        let alice = [3u8; 16];
        let carol = [4u8; 16];
        let mallory = [9u8; 16];
        let tag = [0u8; 32];
        // seq 1: Alice's X, served to this receiver already deleted: no msg id, no sender.
        c.execute("INSERT INTO app_messages (group_id,seq,epoch,recv_ts,status,sender_device,franking_tag) VALUES (?1,1,0,0,2,?2,?3)",
            params![g.as_slice(), alice.as_slice(), tag.as_slice()]).expect("tombstone");
        // seq 2..7: Carol reacts and pins, Alice reacts, Mallory edits, Alice edits and deletes;
        // all name X, all parked.
        for (seq, ty, who, body) in [
            (2i64, 3i64, carol, "x"),
            (3, 5, carol, ""),
            (4, 3, alice, "x"),
            (5, 1, mallory, "edited before"),
            (6, 1, alice, "alice's edit"),
            (7, 2, alice, ""),
        ] {
            c.execute("INSERT INTO app_messages (group_id,seq,epoch,recv_ts,status,sender_user,sender_device,msg_id,type,body,reply_to,franking_tag) VALUES (?1,?2,0,0,0,?3,?3,?4,?5,?6,?7,?8)",
                params![g.as_slice(), seq, who.as_slice(), [seq as u8; 16].as_slice(), ty, body, x.as_slice(), tag.as_slice()]).expect("fold row");
            let tr = FoldTrigger {
                seq: seq as u64,
                ty: ty as u8,
                sender_user: who,
                sender_device: who,
            };
            refold(
                &c,
                &g,
                &x,
                &scope_of(Some(ty), Some(who), body),
                Some(&tr),
                None,
            )
            .expect("parked fold");
        }
        // seq 8: Mallory repeats msg id X as a type 0. Its insert writes no row but its own (S1).
        c.execute("INSERT INTO app_messages (group_id,seq,epoch,recv_ts,status,sender_user,sender_device,msg_id,type,body,franking_tag) VALUES (?1,8,0,0,0,?2,?2,?3,0,'new payment address',?4)",
            params![g.as_slice(), mallory.as_slice(), x.as_slice(), tag.as_slice()]).expect("repeat");
        let tr = FoldTrigger {
            seq: 8,
            ty: 0,
            sender_user: mallory,
            sender_device: mallory,
        };
        let changes = c.total_changes();
        refold(&c, &g, &x, &FoldScope::Full, Some(&tr), None).expect("repeat refold");
        assert_eq!(
            c.total_changes(),
            changes,
            "the repeat's refold writes nothing"
        );
        refold(&c, &g, &x, &FoldScope::Full, None, None).expect("full refold");
        let n = |sql: &str| -> i64 {
            c.query_row(sql, params![g.as_slice(), x.as_slice()], |r| r.get(0))
                .expect("count")
        };
        assert_eq!(
            n("SELECT COUNT(*) FROM app_reactions WHERE group_id = ?1 AND target = ?2"),
            0,
            "the repeat collects no reaction sent before it"
        );
        assert_eq!(
            n("SELECT COUNT(*) FROM app_pins WHERE group_id = ?1 AND target = ?2"),
            0,
            "the repeat collects no pin sent before it"
        );
        assert_eq!(
            n(
                "SELECT COUNT(*) FROM app_messages WHERE group_id = ?1 AND msg_id = ?2 AND edit_body IS NOT NULL"
            ),
            0,
            "the repeat takes no edit sent before it"
        );
        assert!(
            resolve(&c, &g, &x).expect("resolve").is_none(),
            "a type 0 whose msg id a lower-seq row names is never a target"
        );
    }
}
