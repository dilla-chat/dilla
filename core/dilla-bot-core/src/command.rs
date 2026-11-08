//! Sealed Bot commands in the helper (Bot v2 task 8): the per-community command key and its
//! rotation, opening a sealed command, sealing a reply, and the device-list cache the opener
//! checks signatures against.
//!
//! Ops (payload → answer):
//! - 21 `command_key_current [community_id, now]` → `[publication]`
//! - 22 `command_open [sealed, attached_user, attached_device, now]` → `[0, request_cbor]`,
//!   `[refusal_code, reply_key]` once the seal opened (codes 3..7; Task 15), `[8, reply_key,
//!   request_id]` for a verified replay (fix round 1 M-1), or `[refusal_code]`
//!   when it did not (1 malformed, 2 unknown key); `request_cbor` is `[request_id, community_id, channel_id|null, operation,
//!   args, sent_at, reply_key]`
//! - 23 `reply_seal [reply_key, request_id, seq, body]` → `[sealed]`
//! - 24 `device_list_ingest [user_id, push, now]` → `[changed 0|1, version, state]`; `push` is
//!   `dilla_core::bot::device_list_push::DeviceListPush` (`[umk_pub, ssk_pub, sig_umk_ssk,
//!   [[version, blob, ssk_signature, prev_hash], ...]]`), `state` 0 current, 1 held (a key
//!   change is pending), 2 behind (the push did not chain from the cache)
//! - 25 `probe_seal [community_id, request_id, now]` → `[sealed, handle]` (Task 17 fix round 2):
//!   the Bot's health probe, an `OpProbe` (90) signed as the Bot's own device and sealed to the
//!   community's **current** command key (never minted or rotated here; no key is a refusal,
//!   status 5). The reply secret stays in the helper behind `handle` (a bounded in-memory table).
//! - 26 `probe_open [handle, seq, sealed, forget 0|1]` → `[body]`: opens answer `seq` of the probe
//!   behind `handle`; `forget` drops the secret afterwards. An unknown handle is status 7.
//!
//! Time is the `now` the caller passes, never the helper's clock, and nothing runs on a timer:
//! rotation happens in op 21, and an expired previous key is deleted by the next op 21 or 22.

use crate::helper::{DispatchError, body, map_client};
use crate::store::Store;
use dilla_core::bot::command::{
    CommandArg, CommandRefusal, CommandRequest, MAX_COMMAND_KEY_LIFETIME_SECS,
    MAX_SEALED_COMMAND_BYTES, ReplyTable, generate_command_key, open_command, open_reply,
    sealed_command_key_id, verify_command,
};
use dilla_core::cbor::{Encoder, decode_strict};
use dilla_core::client::{ClientCore, PushedList};
use dilla_core::ids::{CommunityId, DeviceId, UserId};
use rusqlite::{Connection, OptionalExtension, TransactionBehavior, params};
use std::sync::MutexGuard;
use zeroize::Zeroizing;

/// The helper's own refusal, after Task 7's codes 1..7.
pub(crate) const REFUSAL_REPLAY: u8 = 8;

const DAY: i64 = 86_400;
/// A command key is rotated once it is 7 days old (spec "Key rotation": weekly).
const ROTATE_AFTER_SECS: i64 = 7 * DAY;
/// A retired key still opens for 24 h after its retirement, and is then deleted.
const PREVIOUS_KEPT_SECS: i64 = DAY;
/// A verified command's request id is remembered for 5 minutes (spec "Dedupe 5 min"). The
/// `sent_at` window is ±120 s, so a copy that is still fresh is never more than 240 s after the
/// first open: the window covers every replay that is not already refused as stale.
const REPLAY_WINDOW_SECS: i64 = 300;
/// The publication's `not_after` is 10 minutes short of the clients' 8-day cap, so a client
/// whose clock runs up to 10 minutes behind still accepts a fresh key (clients allow no skew).
const PUBLICATION_SLACK_SECS: u64 = 600;
/// The `sent_at` window, and the most a current key's `created_at` may lie ahead of `now` before
/// op 21 treats it as forward-dated (minted under a clock that later stepped back) and replaces it.
const CLOCK_SKEW_SECS: i64 = 120;
/// A key is never used past its publication's lifetime: 8 days after `created_at`, plus the
/// `sent_at` window for a command sealed just before `not_after`. Op 21 and op 22 both delete and
/// refuse an older key, so a key's life is bounded even when op 21 is not called.
const KEY_MAX_AGE_SECS: i64 = MAX_COMMAND_KEY_LIFETIME_SECS as i64 + CLOCK_SKEW_SECS;

pub(crate) enum Opened {
    Request(CommandRequest),
    /// A refusal code, and the request's one-time reply key when the seal opened (codes 3..8):
    /// the Bot seals its refusal to it, or for a replay re-seals its answers to this newest key
    /// (Task 15). Nothing else of an unverified request leaves the helper. Codes 1 and 2 (the
    /// seal did not open) carry none.
    Refused(u8, Option<[u8; 32]>),
    /// Refusal 8: a verified seal of a request already opened (a Retry). It carries the seal's own request id with
    /// its reply key, so the Bot binds the key to that request and no other (Task 15, fix round 1 M-1).
    Replay {
        reply_key: [u8; 32],
        request_id: [u8; 16],
    },
}

fn checked_now(now: u64) -> Result<i64, DispatchError> {
    // Room for every `now + lifetime` below to stay in i64 and u64.
    match i64::try_from(now) {
        Ok(n) if n > 0 && n < i64::MAX / 2 => Ok(n),
        _ => Err(DispatchError::Invalid),
    }
}

fn lock(store: &Store) -> Result<MutexGuard<'_, Connection>, DispatchError> {
    store.conn.lock().map_err(|_| DispatchError::Internal)
}

fn db<T>(r: rusqlite::Result<T>) -> Result<T, DispatchError> {
    r.map_err(|_| DispatchError::Internal)
}

/// Deletes every retired key whose 24 h have passed at `now`, and every key (current included)
/// older than `KEY_MAX_AGE_SECS`, then truncates the WAL so no old page image that held the key
/// outlives the deletion (`secure_delete` zeroes the cell in the page itself).
fn purge_retired(conn: &Connection, now: i64) -> Result<(), DispatchError> {
    let deleted = db(conn.execute(
        "DELETE FROM bot_command_keys \
         WHERE (retired_at IS NOT NULL AND retired_at <= ?1) OR created_at <= ?2",
        [now - PREVIOUS_KEPT_SECS, now - KEY_MAX_AGE_SECS],
    ))?;
    if deleted > 0 {
        truncate_wal(conn)?;
    }
    Ok(())
}

fn truncate_wal(conn: &Connection) -> Result<(), DispatchError> {
    // `busy` is 0 on the helper's single connection; a busy checkpoint leaves the frames to the
    // next one, and the committed deletion stands either way.
    let _busy: i64 = db(conn.query_row("PRAGMA wal_checkpoint(TRUNCATE)", [], |r| r.get(0)))?;
    Ok(())
}

type CurrentKey = ([u8; 16], i64, Vec<u8>);

fn read_current(
    conn: &Connection,
    community: &[u8; 16],
) -> Result<Option<CurrentKey>, DispatchError> {
    db(conn
        .query_row(
            "SELECT key_id,created_at,publication FROM bot_command_keys \
             WHERE community_id=?1 AND retired_at IS NULL",
            [community.as_slice()],
            |r| Ok((r.get::<_, [u8; 16]>(0)?, r.get(1)?, r.get(2)?)),
        )
        .optional())
}

/// Op 21: the publication of `community`'s current command key at `now`, minting the first key
/// or rotating a key that is 7 days old. Repeated calls in the same week return the same bytes.
pub(crate) fn current_publication(
    store: &Store,
    core: &ClientCore,
    community: [u8; 16],
    now: u64,
) -> Result<Vec<u8>, DispatchError> {
    let now_i = checked_now(now)?;
    // The helper serves one community identity; it holds keys for that community only.
    if community == [0; 16] || community != store.bind().1 {
        return Err(DispatchError::Invalid);
    }
    let read = {
        let conn = lock(store)?;
        purge_retired(&conn, now_i)?;
        read_current(&conn, &community)?
    };
    // A clock up to `CLOCK_SKEW_SECS` behind `created_at` counts as fresh. A key dated further
    // ahead is replaced: clients refuse its publication, whose `not_after` lies beyond their
    // 8-day cap. Retiring it is no rollback: a retired key never becomes current again.
    if let Some((_, created_at, publication)) = &read
        && now_i < created_at.saturating_add(ROTATE_AFTER_SECS)
        && *created_at <= now_i + CLOCK_SKEW_SECS
    {
        return Ok(publication.clone());
    }
    // Minted and signed outside the store lock: signing reads the identity through the same
    // connection. The private key is zeroized on every return path.
    let (key_id, private_key, public_key) =
        generate_command_key().map_err(|_| DispatchError::Internal)?;
    let not_after = now + MAX_COMMAND_KEY_LIFETIME_SECS - PUBLICATION_SLACK_SECS;
    let publication = core
        .bot_command_key_publish(&community, key_id, public_key, not_after)
        .map_err(map_client)?;

    let mut conn = lock(store)?;
    let tx = db(conn.transaction_with_behavior(TransactionBehavior::Immediate))?;
    // Compare-and-set: install only over the key that was read. If another call installed or
    // rotated meanwhile, its key is the current one and this one is dropped unused.
    let now_current = read_current(&tx, &community)?;
    if now_current.as_ref().map(|k| k.0) != read.as_ref().map(|k| k.0) {
        let Some((_, _, publication)) = now_current else {
            return Err(DispatchError::Conflict);
        };
        return Ok(publication);
    }
    let mut dropped_previous = 0;
    if let Some((old, _, _)) = now_current {
        // One retired key per community: rotation twice inside 24 h (only by a clock jump)
        // deletes the older one now instead of keeping two.
        dropped_previous = db(tx.execute(
            "DELETE FROM bot_command_keys WHERE community_id=?1 AND retired_at IS NOT NULL",
            [community.as_slice()],
        ))?;
        db(tx.execute(
            "UPDATE bot_command_keys SET retired_at=?1 WHERE key_id=?2",
            params![now_i, old.as_slice()],
        ))?;
    }
    db(tx.execute(
        "INSERT INTO bot_command_keys(key_id,community_id,created_at,retired_at,private_key,publication) \
         VALUES(?1,?2,?3,NULL,?4,?5)",
        params![
            key_id.as_slice(),
            community.as_slice(),
            now_i,
            private_key.as_slice(),
            publication
        ],
    ))?;
    db(tx.commit())?;
    if dropped_previous > 0 {
        truncate_wal(&conn)?;
    }
    Ok(publication)
}

/// Op 22: opens a sealed command with the key it names (the current key, or the previous one
/// within 24 h of its retirement), checks it against `user`'s cached device list and the identity
/// dillad attached, and only then records its request id. A command refused for any reason
/// records nothing, so a forgery cannot burn a genuine request's id.
pub(crate) fn open(
    store: &Store,
    core: &ClientCore,
    sealed: &[u8],
    user: [u8; 16],
    device: [u8; 16],
    now: u64,
) -> Result<Opened, DispatchError> {
    let now_i = checked_now(now)?;
    if user == [0; 16] || device == [0; 16] {
        return Err(DispatchError::Invalid);
    }
    let refused = |r: CommandRefusal| Ok(Opened::Refused(r.code(), None));
    let key_id = match sealed_command_key_id(sealed) {
        Ok(k) => k,
        Err(r) => return refused(r),
    };
    let key = {
        let conn = lock(store)?;
        purge_retired(&conn, now_i)?;
        db(conn
            .query_row(
                "SELECT community_id,private_key FROM bot_command_keys \
                 WHERE key_id=?1 AND (retired_at IS NULL OR retired_at>?2) AND created_at>?3",
                params![
                    key_id.as_slice(),
                    now_i - PREVIOUS_KEPT_SECS,
                    now_i - KEY_MAX_AGE_SECS
                ],
                |r| {
                    Ok((
                        r.get::<_, [u8; 16]>(0)?,
                        Zeroizing::new(r.get::<_, Vec<u8>>(1)?),
                    ))
                },
            )
            .optional())?
    };
    let Some((community, stored)) = key else {
        return refused(CommandRefusal::UnknownKey);
    };
    let mut private_key = Zeroizing::new([0u8; 32]);
    if stored.len() != 32 {
        return Err(DispatchError::Internal);
    }
    private_key.copy_from_slice(&stored);
    drop(stored);
    let cmd = match open_command(&private_key, CommunityId::from_bytes(community), sealed) {
        Ok(cmd) => cmd,
        Err(r) => return refused(r),
    };
    drop(private_key);
    // From here the seal opened: a refusal carries the request's reply key and nothing else.
    let reply_key = cmd.request.reply_key;
    let refused = |r: CommandRefusal| Ok(Opened::Refused(r.code(), Some(reply_key)));
    let (user, device) = (UserId::from_bytes(user), DeviceId::from_bytes(device));
    if cmd.user_id != user || cmd.device_id != device {
        return refused(CommandRefusal::WrongDevice);
    }
    // The cache only: ingest (op 24) is the one writer, and no command reads a list remotely. A
    // user whose pushed list did not chain from the cache is refused until it catches up: the
    // versions it missed may revoke this very device.
    let behind: bool = db(lock(store)?.query_row(
        "SELECT EXISTS(SELECT 1 FROM bot_device_list_behind WHERE user_id=?1)",
        [user.as_bytes().as_slice()],
        |r| r.get(0),
    ))?;
    if behind {
        return refused(CommandRefusal::DeviceUnlisted);
    }
    let Some(list) = core
        .trusted_device_list(user.as_bytes())
        .map_err(map_client)?
    else {
        return refused(CommandRefusal::DeviceUnlisted);
    };
    let request = match verify_command(&cmd, &list, user, device, now) {
        Ok(r) => r,
        Err(r) => return refused(r),
    };
    // Verified: now record. Pruning on insert keeps the table to 5 minutes of verified commands.
    // The prune bound is persisted and only rises: a command whose `sent_at` is old enough that
    // its row may already be pruned is refused as a replay, so a clock that steps forward and
    // back cannot reopen a pruned request.
    let mut conn = lock(store)?;
    let tx = db(conn.transaction_with_behavior(TransactionBehavior::Immediate))?;
    let stored: Option<i64> = db(tx
        .query_row(
            "SELECT floor FROM command_replay_floor WHERE id=1",
            [],
            |r| r.get(0),
        )
        .optional())?;
    let floor = stored.map_or(now_i - REPLAY_WINDOW_SECS, |f| {
        f.max(now_i - REPLAY_WINDOW_SECS)
    });
    if i64::try_from(request.sent_at).unwrap_or(i64::MAX) < floor + CLOCK_SKEW_SECS {
        return Ok(Opened::Replay {
            reply_key: request.reply_key,
            request_id: request.request_id,
        });
    }
    db(tx.execute("DELETE FROM command_replay WHERE seen_at<?1", [floor]))?;
    db(tx.execute(
        "INSERT INTO command_replay_floor(id,floor) VALUES(1,?1) \
         ON CONFLICT(id) DO UPDATE SET floor=excluded.floor",
        [floor],
    ))?;
    let inserted = db(tx.execute(
        "INSERT OR IGNORE INTO command_replay(request_id,seen_at) VALUES(?1,?2)",
        params![request.request_id.as_slice(), now_i],
    ))?;
    db(tx.commit())?;
    if inserted == 0 {
        // Verified, and seen before: a Retry. The Bot re-seals what it already answered to this
        // send's reply key (the client keeps only the newest), and dispatches nothing again.
        return Ok(Opened::Replay {
            reply_key: request.reply_key,
            request_id: request.request_id,
        });
    }
    Ok(Opened::Request(request))
}

/// Op 24 state: the cache vouches for the user.
pub(crate) const LIST_CURRENT: u8 = 0;
/// Op 24 state: a key change is pending; the user's commands are refused until it is verified.
pub(crate) const LIST_HELD: u8 = 1;
/// Op 24 state: a pushed list did not chain from the cache; the user's commands are refused
/// until a history reaching the pushed head is ingested.
pub(crate) const LIST_BEHIND: u8 = 2;

/// Op 24: the only writer of the device-list cache the opener reads (`ingest_device_list`),
/// and of the helper's behind marks. Returns `(changed, cached version, state)`.
pub(crate) fn ingest(
    store: &Store,
    core: &mut ClientCore,
    user: [u8; 16],
    body: &[u8],
    now: u64,
) -> Result<(bool, u64, u8), DispatchError> {
    checked_now(now)?;
    let outcome = core
        .ingest_device_list(&user, body, now)
        .map_err(|e| match e.code {
            "E_CORE_LIMIT" => DispatchError::Invalid,
            _ => map_client(e),
        })?;
    let conn = lock(store)?;
    let user_key = user.as_slice();
    match outcome {
        PushedList::Current { changed, version } => {
            db(conn.execute(
                "DELETE FROM bot_device_list_behind WHERE user_id=?1 AND head<=?2",
                params![user_key, i64::try_from(version).unwrap_or(i64::MAX)],
            ))?;
            let behind: bool = db(conn.query_row(
                "SELECT EXISTS(SELECT 1 FROM bot_device_list_behind WHERE user_id=?1)",
                [user_key],
                |r| r.get(0),
            ))?;
            Ok((
                changed,
                version,
                if behind { LIST_BEHIND } else { LIST_CURRENT },
            ))
        }
        PushedList::Held { changed, version } => Ok((changed, version, LIST_HELD)),
        PushedList::Behind { version, head } => {
            db(conn.execute(
                "INSERT INTO bot_device_list_behind(user_id,head) VALUES(?1,?2) \
                 ON CONFLICT(user_id) DO UPDATE SET head=max(head,excluded.head)",
                params![user_key, i64::try_from(head).unwrap_or(i64::MAX)],
            ))?;
            Ok((false, version, LIST_BEHIND))
        }
    }
}

/// Op 23: answer number `seq` of `request_id`, sealed to the request's one-time `reply_key`.
pub(crate) fn seal_reply(
    reply_key: [u8; 32],
    request_id: [u8; 16],
    seq: u32,
    body: &[u8],
) -> Result<Vec<u8>, DispatchError> {
    if reply_key == [0; 32] || request_id == [0; 16] {
        return Err(DispatchError::Invalid);
    }
    dilla_core::bot::command::seal_reply(&reply_key, request_id, seq, body)
        .map_err(|_| DispatchError::Invalid)
}

fn invalid<T>(_: T) -> DispatchError {
    DispatchError::Invalid
}

pub(crate) fn op_key_current(
    store: &Store,
    core: &ClientCore,
    payload: &[u8],
) -> Result<Zeroizing<Vec<u8>>, DispatchError> {
    let (community, now) = decode_strict(payload, |d| {
        d.array(2)?;
        Ok((d.bytes_exact::<16>()?, d.uint()?))
    })
    .map_err(invalid)?;
    let publication = current_publication(store, core, community, now)?;
    Ok(Zeroizing::new(body(publication)))
}

pub(crate) fn op_open(
    store: &Store,
    core: &ClientCore,
    payload: &[u8],
) -> Result<Zeroizing<Vec<u8>>, DispatchError> {
    let (sealed, user, device, now) = decode_strict(payload, |d| {
        d.array(4)?;
        let sealed = d.bytes()?;
        if sealed.len() > MAX_SEALED_COMMAND_BYTES {
            return Err(crate::store::cbor_shape());
        }
        Ok((
            sealed.to_vec(),
            d.bytes_exact::<16>()?,
            d.bytes_exact::<16>()?,
            d.uint()?,
        ))
    })
    .map_err(invalid)?;
    let mut e = Encoder::new();
    match open(store, core, &sealed, user, device, now)? {
        Opened::Refused(code, None) => {
            e.array(1).uint(u64::from(code));
        }
        Opened::Refused(code, Some(reply_key)) => {
            e.array(2).uint(u64::from(code)).bytes(&reply_key);
        }
        Opened::Replay {
            reply_key,
            request_id,
        } => {
            e.array(3)
                .uint(u64::from(REFUSAL_REPLAY))
                .bytes(&reply_key)
                .bytes(&request_id);
        }
        Opened::Request(request) => {
            let request = Zeroizing::new(encode_request(&request));
            e.array(2).uint(0).bytes(&request);
        }
    }
    Ok(Zeroizing::new(e.into_vec()))
}

/// `[request_id, community_id, channel_id|null, operation, args, sent_at, reply_key]`.
fn encode_request(r: &CommandRequest) -> Vec<u8> {
    let mut e = Encoder::new();
    e.array(7)
        .bytes(&r.request_id)
        .bytes(r.community_id.as_bytes());
    match &r.channel_id {
        Some(c) => e.bytes(c.as_bytes()),
        None => e.null(),
    };
    e.uint(r.operation).array(r.args.len());
    for arg in &r.args {
        match arg {
            CommandArg::Text(s) => e.text(s),
            CommandArg::Uint(n) => e.uint(*n),
            CommandArg::Bytes(b) => e.bytes(b),
            CommandArg::Null => e.null(),
        };
    }
    e.uint(r.sent_at).bytes(&r.reply_key);
    e.into_vec()
}

pub(crate) fn op_reply_seal(payload: &[u8]) -> Result<Zeroizing<Vec<u8>>, DispatchError> {
    let (reply_key, request_id, seq, reply) = decode_strict(payload, |d| {
        d.array(4)?;
        Ok((
            d.bytes_exact::<32>()?,
            d.bytes_exact::<16>()?,
            d.uint()?,
            Zeroizing::new(d.bytes()?.to_vec()),
        ))
    })
    .map_err(invalid)?;
    let seq = u32::try_from(seq).map_err(invalid)?;
    Ok(Zeroizing::new(body(seal_reply(
        reply_key, request_id, seq, &reply,
    )?)))
}

pub(crate) fn op_device_list_ingest(
    store: &Store,
    core: &mut ClientCore,
    payload: &[u8],
) -> Result<Zeroizing<Vec<u8>>, DispatchError> {
    let (user, push, now) = decode_strict(payload, |d| {
        d.array(3)?;
        Ok((d.bytes_exact::<16>()?, d.bytes()?.to_vec(), d.uint()?))
    })
    .map_err(invalid)?;
    let (changed, version, state) = ingest(store, core, user, &push, now)?;
    let mut e = Encoder::new();
    e.array(3)
        .uint(u64::from(changed))
        .uint(version)
        .uint(u64::from(state));
    Ok(Zeroizing::new(e.into_vec()))
}

/// Op 25: the health probe, sealed to `community`'s current command key as this device. The
/// reply secret is kept in `replies`; its handle is returned with the sealed bytes.
pub(crate) fn op_probe_seal(
    store: &Store,
    core: &ClientCore,
    replies: &mut ReplyTable,
    payload: &[u8],
) -> Result<Zeroizing<Vec<u8>>, DispatchError> {
    let (community, request_id, now) = decode_strict(payload, |d| {
        d.array(3)?;
        Ok((d.bytes_exact::<16>()?, d.bytes_exact::<16>()?, d.uint()?))
    })
    .map_err(invalid)?;
    let now_i = checked_now(now)?;
    if community == [0; 16] || community != store.bind().1 || request_id == [0; 16] {
        return Err(DispatchError::Invalid);
    }
    // The current key as op 22 would find it: purged when expired, never minted or rotated here,
    // so the probe tests the key members seal to and changes nothing they see.
    let current = {
        let conn = lock(store)?;
        purge_retired(&conn, now_i)?;
        read_current(&conn, &community)?
    };
    let Some((_, created_at, publication)) = current else {
        return Err(DispatchError::Unavailable);
    };
    if created_at <= now_i - KEY_MAX_AGE_SECS {
        return Err(DispatchError::Unavailable);
    }
    let (sealed, secret) = core
        .bot_probe_seal(&publication, &community, request_id, now)
        .map_err(map_client)?;
    let handle = replies.insert(request_id, secret);
    let mut e = Encoder::new();
    e.array(2).bytes(&sealed).uint(u64::from(handle));
    Ok(Zeroizing::new(e.into_vec()))
}

/// Op 26: opens answer `seq` of the probe behind `handle` with its reply secret.
pub(crate) fn op_probe_open(
    replies: &mut ReplyTable,
    payload: &[u8],
) -> Result<Zeroizing<Vec<u8>>, DispatchError> {
    let (handle, seq, sealed, forget) = decode_strict(payload, |d| {
        d.array(4)?;
        Ok((d.uint()?, d.uint()?, d.bytes()?.to_vec(), d.uint()?))
    })
    .map_err(invalid)?;
    let handle = u32::try_from(handle).map_err(invalid)?;
    let seq = u32::try_from(seq).map_err(invalid)?;
    if forget > 1 {
        return Err(DispatchError::Invalid);
    }
    let Some((request_id, secret)) = replies.get(handle) else {
        return Err(DispatchError::NotFound);
    };
    let opened = open_reply(secret, request_id, seq, &sealed);
    if forget == 1 {
        replies.forget(handle);
    }
    let body = Zeroizing::new(opened.map_err(|_| DispatchError::Invalid)?);
    let mut e = Encoder::new();
    e.array(1).bytes(&body);
    Ok(Zeroizing::new(e.into_vec()))
}

#[cfg(test)]
mod tests {
    use super::*;
    use dilla_core::bot::command::{
        CommandArg, CommandKeyPublication, CommandRefusal, MAX_COMMAND_KEY_LIFETIME_SECS,
        ReplySecret, open_reply, seal_command,
    };
    use dilla_core::bot::device_list_push::{DeviceListPush, DeviceListRow};
    use dilla_core::cbor::decode_strict;
    use dilla_core::identity::{
        DeviceEntry, DeviceList, DeviceListUnsigned, SskSigner, Tier, UmkSigner,
    };
    use dilla_core::ids::{ChannelId, CommunityId, DeviceId, UserId};
    use openmls::prelude::SignatureScheme;
    use openmls_basic_credential::SignatureKeyPair;
    use rusqlite::OptionalExtension;
    use std::{
        fs,
        os::unix::fs::PermissionsExt,
        path::PathBuf,
        sync::atomic::{AtomicU64, Ordering},
    };

    const INSTANCE: [u8; 16] = [0x11; 16];
    const COMMUNITY: [u8; 16] = [0x22; 16];
    const INSTALLATION: [u8; 16] = [0x33; 16];
    const BOT_USER: [u8; 16] = [0x44; 16];
    const USER: [u8; 16] = [0x55; 16];
    const DEVICE: [u8; 16] = [0x56; 16];
    const SECOND: [u8; 16] = [0x58; 16];
    const T0: u64 = 1_800_000_000;
    const DAY: u64 = 86_400;

    static NEXT: AtomicU64 = AtomicU64::new(0);

    /// A user's key set: the UMK, the SSK it certifies.
    struct Keys {
        umk: UmkSigner,
        ssk: SskSigner,
    }

    impl Keys {
        fn new(seed: u8) -> Self {
            Self {
                umk: UmkSigner::from_bytes(&[seed; 32]),
                ssk: SskSigner::from_bytes(&[seed ^ 0x5a; 32]),
            }
        }

        fn list(
            &self,
            version: u64,
            prev: Option<&DeviceList>,
            entries: Vec<DeviceEntry>,
        ) -> DeviceList {
            let unsigned = DeviceListUnsigned {
                v: 1,
                user_id: UserId::from_bytes(USER),
                version,
                prev_hash: prev.map_or([0; 32], DeviceList::hash),
                entries,
            };
            let sig_ssk = self.ssk.sign_device_list(&unsigned);
            DeviceList { unsigned, sig_ssk }
        }

        /// The op 24 body for `lists` under these keys.
        fn push(&self, lists: &[&DeviceList]) -> Vec<u8> {
            let ssk_pub = self.ssk.public();
            DeviceListPush {
                umk_pub: self.umk.public(),
                ssk_pub,
                sig_umk_ssk: self.umk.sign_ssk(&ssk_pub),
                history: lists.iter().map(|l| row(l)).collect(),
            }
            .encode()
        }
    }

    fn row(list: &DeviceList) -> DeviceListRow {
        DeviceListRow {
            version: list.unsigned.version,
            blob: list.encode(),
            ssk_signature: list.sig_ssk,
            prev_hash: list.unsigned.prev_hash,
        }
    }

    struct World {
        dir: PathBuf,
        store: Store,
        core: ClientCore,
        keys: Keys,
        user_dsk: SignatureKeyPair,
        user_dsk_pub: [u8; 32],
        second_dsk: SignatureKeyPair,
        second_dsk_pub: [u8; 32],
        next_request: u8,
        /// The Bot's own `[umk_pub, ssk_pub, sig_umk_ssk, device]` from its bootstrap.
        registration: Vec<u8>,
    }

    impl Drop for World {
        fn drop(&mut self) {
            let _ = fs::remove_dir_all(&self.dir);
        }
    }

    fn dsk(seed: u8) -> (SignatureKeyPair, [u8; 32]) {
        let public = SskSigner::from_bytes(&[seed; 32]).public();
        (
            SignatureKeyPair::from_raw(SignatureScheme::ED25519, vec![seed; 32], public.to_vec()),
            public,
        )
    }

    fn world() -> World {
        let dir = std::env::temp_dir().join(format!(
            "dilla-bot-command-{}-{}-{}",
            std::process::id(),
            NEXT.fetch_add(1, Ordering::Relaxed),
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_nanos()
        ));
        fs::create_dir(&dir).unwrap();
        fs::set_permissions(&dir, fs::Permissions::from_mode(0o700)).unwrap();
        let store = Store::open(&dir, INSTANCE, COMMUNITY, INSTALLATION, &[0x42; 32]).unwrap();
        let mut core = ClientCore::open(store.conn.clone()).unwrap();
        let registration = core
            .bot_bootstrap_begin(&INSTANCE, &COMMUNITY, &INSTALLATION, &[0x77; 32])
            .unwrap();
        core.bot_bootstrap_complete(&BOT_USER, "command bot", T0)
            .unwrap();
        let (user_dsk, user_dsk_pub) = dsk(0x61);
        let (second_dsk, second_dsk_pub) = dsk(0x63);
        World {
            dir,
            store,
            core,
            keys: Keys::new(0x0a),
            user_dsk,
            user_dsk_pub,
            second_dsk,
            second_dsk_pub,
            next_request: 1,
            registration,
        }
    }

    fn entry(device: [u8; 16], dsk_pub: [u8; 32], revoked_at: Option<u64>) -> DeviceEntry {
        DeviceEntry {
            device_id: DeviceId::from_bytes(device),
            dsk_pub,
            tier: Tier::Native,
            added_at: 100,
            revoked_at,
        }
    }

    fn refused(opened: Opened) -> u8 {
        match opened {
            Opened::Refused(code, _) => code,
            Opened::Replay { .. } => REFUSAL_REPLAY,
            Opened::Request(_) => 0,
        }
    }

    impl World {
        fn v1(&self) -> DeviceList {
            self.keys
                .list(1, None, vec![entry(DEVICE, self.user_dsk_pub, None)])
        }

        fn ingest(&mut self, body: &[u8]) -> Result<(bool, u64, u8), DispatchError> {
            ingest(&self.store, &mut self.core, USER, body, T0)
        }

        /// The first push of USER: trust on first use of the user's UMK.
        fn anchor(&mut self, list: &DeviceList) {
            let body = self.keys.push(&[list]);
            assert_eq!(
                self.ingest(&body).unwrap(),
                (true, list.unsigned.version, LIST_CURRENT)
            );
        }

        fn cached(&self) -> Option<(i64, Vec<u8>)> {
            self.store
                .conn
                .lock()
                .unwrap()
                .query_row(
                    "SELECT version,list FROM app_device_lists WHERE user_id=?1",
                    [USER.as_slice()],
                    |r| Ok((r.get(0)?, r.get(1)?)),
                )
                .optional()
                .unwrap()
        }

        /// `(umk_pub, verified, changed_umk)` of USER's pin.
        fn pin(&self) -> Option<([u8; 32], i64, Option<[u8; 32]>)> {
            self.store
                .conn
                .lock()
                .unwrap()
                .query_row(
                    "SELECT umk_pub,verified,changed_umk FROM app_trust_pins WHERE user_id=?1",
                    [USER.as_slice()],
                    |r| Ok((r.get(0)?, r.get(1)?, r.get(2)?)),
                )
                .optional()
                .unwrap()
        }

        fn publication(&self, now: u64) -> CommandKeyPublication {
            let bytes = current_publication(&self.store, &self.core, COMMUNITY, now).unwrap();
            CommandKeyPublication::decode(&bytes).unwrap()
        }

        fn bot_list(&self) -> DeviceList {
            let body = self.core.device_list_body().unwrap();
            let blob = decode_strict(&body, |d| {
                d.array(4)?;
                d.uint()?;
                let blob = d.bytes()?.to_vec();
                d.skip()?;
                d.skip()?;
                Ok(blob)
            })
            .unwrap();
            DeviceList::decode(&blob).unwrap()
        }

        fn request(&mut self, sent_at: u64) -> CommandRequest {
            let n = self.next_request;
            self.next_request += 1;
            CommandRequest {
                request_id: [n; 16],
                community_id: CommunityId::from_bytes(COMMUNITY),
                channel_id: Some(ChannelId::from_bytes([0x66; 16])),
                operation: 7,
                args: vec![CommandArg::Text("hello".into()), CommandArg::Uint(3)],
                sent_at,
                reply_key: [0xff; 32],
            }
        }

        fn seal(&self, p: &CommandKeyPublication, req: &CommandRequest) -> Vec<u8> {
            self.seal_as(p, req, DEVICE, &self.user_dsk)
        }

        fn seal_as(
            &self,
            p: &CommandKeyPublication,
            req: &CommandRequest,
            device: [u8; 16],
            key: &SignatureKeyPair,
        ) -> Vec<u8> {
            seal_command(
                p,
                req,
                UserId::from_bytes(USER),
                DeviceId::from_bytes(device),
                key,
            )
            .unwrap()
            .0
        }

        fn open_at(&self, sealed: &[u8], now: u64) -> Result<CommandRequest, u8> {
            self.open_as(sealed, DEVICE, now)
        }

        fn open_as(&self, sealed: &[u8], device: [u8; 16], now: u64) -> Result<CommandRequest, u8> {
            match open(&self.store, &self.core, sealed, USER, device, now).unwrap() {
                Opened::Request(r) => Ok(r),
                Opened::Refused(code, _) => Err(code),
                Opened::Replay { .. } => Err(REFUSAL_REPLAY),
            }
        }

        /// A fresh command from `device` at T0, opened at T0.
        fn command_from(&mut self, device: [u8; 16]) -> Result<CommandRequest, u8> {
            let p = self.publication(T0);
            let req = self.request(T0);
            let sealed = if device == DEVICE {
                self.seal(&p, &req)
            } else {
                self.seal_as(&p, &req, device, &self.second_dsk)
            };
            self.open_as(&sealed, device, T0)
        }

        fn count(&self, table: &str) -> i64 {
            self.store
                .conn
                .lock()
                .unwrap()
                .query_row(&format!("SELECT count(*) FROM {table}"), [], |r| r.get(0))
                .unwrap()
        }

        fn private_key(&self, key_id: &[u8; 16]) -> Option<Vec<u8>> {
            self.store
                .conn
                .lock()
                .unwrap()
                .query_row(
                    "SELECT private_key FROM bot_command_keys WHERE key_id=?1",
                    [key_id.as_slice()],
                    |r| r.get(0),
                )
                .optional()
                .unwrap()
        }
    }

    // ---- command keys ----

    #[test]
    fn key_rotates_after_seven_days_and_previous_opens_for_24h() {
        let mut w = world();
        w.anchor(&w.v1());
        let old = w.publication(T0);
        assert_eq!(
            w.publication(T0 + 7 * DAY - 1),
            old,
            "no rotation before 7 days"
        );
        let rotated_at = T0 + 7 * DAY + 1;
        let new = w.publication(rotated_at);
        assert_ne!(new.key_id, old.key_id, "day 7 + 1 s rotates");
        assert_eq!(w.publication(rotated_at), new, "rotation is idempotent");
        assert_eq!(
            w.publication(rotated_at - 10),
            new,
            "an earlier clock never rolls the key back"
        );

        let last = rotated_at + DAY - 1;
        let req = w.request(last);
        let sealed = w.seal(&old, &req);
        let opened = w
            .open_at(&sealed, last)
            .expect("previous key opens until +24 h");
        assert_eq!(opened.request_id, req.request_id);

        let req = w.request(rotated_at + DAY);
        let sealed = w.seal(&old, &req);
        assert_eq!(
            w.open_at(&sealed, rotated_at + DAY),
            Err(CommandRefusal::UnknownKey.code())
        );
        let req = w.request(rotated_at + DAY);
        let sealed = w.seal(&new, &req);
        assert!(
            w.open_at(&sealed, rotated_at + DAY).is_ok(),
            "the current key opens"
        );
    }

    #[test]
    fn a_forward_dated_key_is_replaced_when_the_clock_returns() {
        let mut w = world();
        w.anchor(&w.v1());
        // Op 21 once under a clock an hour ahead.
        let ahead = w.publication(T0 + 3600);
        // Within the skew allowance the key stands.
        assert_eq!(w.publication(T0 + 3600 - 120), ahead);
        // Back at the real clock, clients would refuse `ahead` (not_after beyond their cap).
        let real = w.publication(T0);
        assert_ne!(real.key_id, ahead.key_id, "a forward-dated key is rotated");
        assert_eq!(real.verify(&w.bot_list(), T0), Ok(()));
        assert_eq!(w.publication(T0 + 60), real, "and the new key is stable");
        // The forward-dated key is now the previous one; it never becomes current again.
        let req = w.request(T0);
        assert!(w.open_at(&w.seal(&ahead, &req), T0).is_ok());
        assert_eq!(w.publication(T0 + 3600), real);
    }

    #[test]
    fn a_current_key_past_its_lifetime_is_deleted_and_refused_without_op_21() {
        let mut w = world();
        w.anchor(&w.v1());
        let p = w.publication(T0);
        let edge = T0 + MAX_COMMAND_KEY_LIFETIME_SECS + 119;
        let req = w.request(edge);
        assert!(
            w.open_at(&w.seal(&p, &req), edge).is_ok(),
            "opens through not_after plus the sent_at window"
        );
        let req = w.request(edge + 1);
        assert_eq!(
            w.open_at(&w.seal(&p, &req), edge + 1),
            Err(CommandRefusal::UnknownKey.code())
        );
        assert_eq!(
            w.count("bot_command_keys"),
            0,
            "op 22 deleted the expired current key"
        );
        assert!(w.private_key(&p.key_id).is_none());
        // The next op 21 mints a fresh key.
        assert_ne!(w.publication(edge + 2).key_id, p.key_id);
    }

    #[test]
    fn previous_private_key_is_gone_from_the_store_after_24h() {
        let w = world();
        let old = w.publication(T0);
        let old_sk = w.private_key(&old.key_id).expect("current key is stored");
        assert_eq!(old_sk.len(), 32);
        let rotated_at = T0 + 7 * DAY;
        let new = w.publication(rotated_at);
        assert!(w.private_key(&old.key_id).is_some(), "kept for 24 h");
        let new_sk = w.private_key(&new.key_id).unwrap();
        assert_eq!(w.publication(rotated_at + DAY - 1), new);
        assert!(w.private_key(&old.key_id).is_some());
        assert_eq!(w.publication(rotated_at + DAY), new);
        assert!(w.private_key(&old.key_id).is_none(), "deleted at +24 h");
        assert_eq!(w.count("bot_command_keys"), 1);

        let secure_delete: i64 = w
            .store
            .conn
            .lock()
            .unwrap()
            .query_row("PRAGMA secure_delete", [], |r| r.get(0))
            .unwrap();
        assert_eq!(secure_delete, 1, "deleted cells are overwritten");
        let wal = w.dir.join("state.db-wal");
        assert_eq!(
            fs::metadata(&wal).map(|m| m.len()).unwrap_or(0),
            0,
            "no WAL frame keeps an old page image after the deletion"
        );
        for name in ["state.db", "state.db-wal", "state.db-shm"] {
            let raw = fs::read(w.dir.join(name)).unwrap_or_default();
            for key in [&old_sk, &new_sk] {
                assert!(
                    !raw.windows(32).any(|win| win == key.as_slice()),
                    "{name} holds raw key bytes"
                );
            }
        }
    }

    #[test]
    fn fresh_publication_verifies_with_a_slow_client_clock() {
        let w = world();
        let p = w.publication(T0);
        assert_eq!(
            p.not_after,
            T0 + MAX_COMMAND_KEY_LIFETIME_SECS - 600,
            "8 days less 10 minutes"
        );
        let list = w.bot_list();
        for behind in [0, 1, 60, 300] {
            assert_eq!(p.verify(&list, T0 - behind), Ok(()), "{behind} s behind");
        }
        assert_eq!(p.community_id, CommunityId::from_bytes(COMMUNITY));
        assert_eq!(p.bot_device_id, list.unsigned.entries[0].device_id);
    }

    #[test]
    fn keys_are_only_for_the_bound_community() {
        let w = world();
        assert!(current_publication(&w.store, &w.core, [0x23; 16], T0).is_err());
        assert!(current_publication(&w.store, &w.core, [0; 16], T0).is_err());
        assert!(current_publication(&w.store, &w.core, COMMUNITY, 0).is_err());
        assert_eq!(w.count("bot_command_keys"), 0);
    }

    // ---- opening ----

    #[test]
    fn opened_request_is_the_signed_request_and_its_reply_opens() {
        let mut w = world();
        w.anchor(&w.v1());
        let p = w.publication(T0);
        let req = w.request(T0);
        let (sealed, secret) = seal_command(
            &p,
            &req,
            UserId::from_bytes(USER),
            DeviceId::from_bytes(DEVICE),
            &w.user_dsk,
        )
        .unwrap();
        let opened = w.open_at(&sealed, T0).unwrap();
        let mut expected = req.clone();
        expected.reply_key = secret.public_key();
        assert_eq!(opened, expected);
        let sealed_reply =
            seal_reply(opened.reply_key, opened.request_id, 2, b"Now playing").unwrap();
        assert_eq!(
            open_reply(&secret, opened.request_id, 2, &sealed_reply).unwrap(),
            b"Now playing"
        );
        assert!(open_reply(&secret, opened.request_id, 3, &sealed_reply).is_err());
        let other = ReplySecret::generate().unwrap();
        assert!(open_reply(&other, opened.request_id, 2, &sealed_reply).is_err());
    }

    #[test]
    fn replayed_request_id_is_refused() {
        let mut w = world();
        w.anchor(&w.v1());
        let p = w.publication(T0);
        let req = w.request(T0);
        let sealed = w.seal(&p, &req);
        assert!(w.open_at(&sealed, T0).is_ok());
        assert_eq!(w.open_at(&sealed, T0 + 1), Err(REFUSAL_REPLAY));
        // A fresh seal of the same request (another ephemeral key) is the same request.
        let again = w.seal(&p, &req);
        assert_eq!(w.open_at(&again, T0 + 100), Err(REFUSAL_REPLAY));
    }

    #[test]
    fn a_refused_command_does_not_consume_its_request_id() {
        let mut w = world();
        w.anchor(&w.v1());
        let p = w.publication(T0);
        let req = w.request(T0);
        // Same user, device and request id, signed by a key the list does not hold.
        let (forger, _) = dsk(0x62);
        let forged = w.seal_as(&p, &req, DEVICE, &forger);
        assert_eq!(
            w.open_at(&forged, T0),
            Err(CommandRefusal::BadSignature.code())
        );
        // A stale copy is refused for staleness and records nothing either.
        let mut stale = req.clone();
        stale.sent_at = T0 - 121;
        let stale = w.seal(&p, &stale);
        assert_eq!(w.open_at(&stale, T0), Err(CommandRefusal::Stale.code()));
        assert_eq!(w.count("command_replay"), 0);
        let genuine = w.seal(&p, &req);
        assert!(
            w.open_at(&genuine, T0).is_ok(),
            "the genuine command still opens"
        );
    }

    /// Task 15: a refusal decided after the seal opened carries the request's reply key, so the Bot
    /// can seal its refusal (401, 408) or, for a replay, re-seal its answers to the newest key.
    /// A seal that did not open (malformed, unknown key) carries none: no reply is possible.
    #[test]
    fn refusals_after_the_seal_opened_carry_the_reply_key() {
        let mut w = world();
        w.anchor(&w.v1());
        let p = w.publication(T0);
        let req = w.request(T0);
        let open_full = |w: &World, sealed: &[u8], device: [u8; 16], now: u64| {
            open(&w.store, &w.core, sealed, USER, device, now).unwrap()
        };
        let (forger, _) = dsk(0x62);
        let (forged, forged_secret) = seal_command(
            &p,
            &req,
            UserId::from_bytes(USER),
            DeviceId::from_bytes(DEVICE),
            &forger,
        )
        .unwrap();
        match open_full(&w, &forged, DEVICE, T0) {
            Opened::Refused(code, Some(key)) => {
                assert_eq!(code, CommandRefusal::BadSignature.code());
                assert_eq!(key, forged_secret.public_key());
            }
            _ => panic!("a bad signature carries the reply key"),
        }
        let sealed = w.seal(&p, &req);
        assert!(matches!(
            open_full(&w, &sealed, [0x57; 16], T0),
            Opened::Refused(c, Some(_)) if c == CommandRefusal::WrongDevice.code()
        ));
        assert!(matches!(
            open_full(&w, &sealed, DEVICE, T0 + 500),
            Opened::Refused(c, Some(_)) if c == CommandRefusal::Stale.code()
        ));
        assert!(matches!(
            open_full(&w, &sealed, DEVICE, T0),
            Opened::Request(_)
        ));
        // A Retry: the same request sealed again with a fresh one-time reply key.
        let (again, again_secret) = seal_command(
            &p,
            &req,
            UserId::from_bytes(USER),
            DeviceId::from_bytes(DEVICE),
            &w.user_dsk,
        )
        .unwrap();
        match open_full(&w, &again, DEVICE, T0 + 2) {
            Opened::Replay {
                reply_key,
                request_id,
            } => {
                assert_eq!(
                    reply_key,
                    again_secret.public_key(),
                    "the newest send's key"
                );
                assert_eq!(
                    request_id, req.request_id,
                    "the verified seal's own request id"
                );
            }
            _ => panic!("a replay carries the newest reply key and its request id"),
        }
        assert!(matches!(
            open_full(&w, &[0x80], DEVICE, T0),
            Opened::Refused(c, None) if c == CommandRefusal::Malformed.code()
        ));
        let mut unknown = sealed.clone();
        // The key id is the outer array's second element: [1, key_id, request_id, enc, ct].
        unknown[3] ^= 1;
        assert!(matches!(
            open_full(&w, &unknown, DEVICE, T0),
            Opened::Refused(c, None) if c == CommandRefusal::UnknownKey.code()
        ));
        // The op 22 answer: [code, reply_key] once the seal opened, [code] otherwise.
        let payload = |sealed: &[u8], now: u64| {
            let mut e = Encoder::new();
            e.array(4)
                .bytes(sealed)
                .bytes(&USER)
                .bytes(&DEVICE)
                .uint(now);
            e.into_vec()
        };
        let answer = op_open(&w.store, &w.core, &payload(&again, T0 + 3)).unwrap();
        let (code, key, request_id) = decode_strict(&answer, |d| {
            d.array(3)?;
            Ok((d.uint()?, d.bytes_exact::<32>()?, d.bytes_exact::<16>()?))
        })
        .unwrap();
        assert_eq!(
            (code, key, request_id),
            (
                u64::from(REFUSAL_REPLAY),
                again_secret.public_key(),
                req.request_id
            )
        );
        let answer = op_open(&w.store, &w.core, &payload(&[0x80], T0 + 3)).unwrap();
        let code = decode_strict(&answer, |d| {
            d.array(1)?;
            d.uint()
        })
        .unwrap();
        assert_eq!(code, u64::from(CommandRefusal::Malformed.code()));
    }

    #[test]
    fn replay_rows_older_than_five_minutes_are_pruned_on_insert() {
        let mut w = world();
        w.anchor(&w.v1());
        let p = w.publication(T0);
        for i in 0..3 {
            let req = w.request(T0 + i);
            assert!(w.open_at(&w.seal(&p, &req), T0 + i).is_ok());
        }
        assert_eq!(w.count("command_replay"), 3);
        // At T0 + 301 the row seen at T0 is older than 5 minutes; the others are not.
        let req = w.request(T0 + 301);
        assert!(w.open_at(&w.seal(&p, &req), T0 + 301).is_ok());
        assert_eq!(w.count("command_replay"), 3, "one pruned, one inserted");
        let req = w.request(T0 + 900);
        assert!(w.open_at(&w.seal(&p, &req), T0 + 900).is_ok());
        assert_eq!(w.count("command_replay"), 1);
    }

    #[test]
    fn a_clock_step_back_cannot_reopen_a_pruned_request() {
        let mut w = world();
        w.anchor(&w.v1());
        let p = w.publication(T0);
        let first = w.request(T0);
        let sealed = w.seal(&p, &first);
        assert!(w.open_at(&sealed, T0).is_ok());
        // One command under a clock 1000 s ahead prunes the first row.
        let ahead = w.request(T0 + 1000);
        assert!(w.open_at(&w.seal(&p, &ahead), T0 + 1000).is_ok());
        assert_eq!(w.count("command_replay"), 1);
        // The clock steps back: the first command's sent_at is fresh again, its row is gone,
        // and the persisted prune floor still refuses it.
        assert_eq!(w.open_at(&sealed, T0 + 5), Err(REFUSAL_REPLAY));
    }

    #[test]
    fn command_from_unknown_user_is_unlisted() {
        let mut w = world();
        let p = w.publication(T0);
        let req = w.request(T0);
        let sealed = w.seal(&p, &req);
        assert_eq!(
            w.open_at(&sealed, T0),
            Err(CommandRefusal::DeviceUnlisted.code())
        );
        // Not recorded: once the list is cached, the same command opens.
        w.anchor(&w.v1());
        assert!(w.open_at(&sealed, T0).is_ok());
    }

    #[test]
    fn command_naming_another_device_than_attached_is_wrong_device() {
        let mut w = world();
        w.anchor(&w.v1());
        let p = w.publication(T0);
        let req = w.request(T0);
        let sealed = w.seal(&p, &req);
        assert_eq!(
            refused(open(&w.store, &w.core, &sealed, USER, [0x57; 16], T0).unwrap()),
            CommandRefusal::WrongDevice.code()
        );
        let mut garbage = sealed.clone();
        let n = garbage.len();
        garbage[n - 1] ^= 1;
        assert_eq!(
            w.open_at(&garbage, T0),
            Err(CommandRefusal::Malformed.code())
        );
        assert_eq!(
            w.open_at(&[0x80], T0),
            Err(CommandRefusal::Malformed.code())
        );
    }

    // ---- device-list pushes ----

    #[test]
    fn first_push_pins_the_user_on_first_use() {
        let mut w = world();
        let v1 = w.v1();
        let v2 = w.keys.list(
            2,
            Some(&v1),
            vec![
                entry(DEVICE, w.user_dsk_pub, None),
                entry(SECOND, w.second_dsk_pub, None),
            ],
        );
        assert_eq!(w.pin(), None);
        let body = w.keys.push(&[&v1, &v2]);
        assert_eq!(w.ingest(&body).unwrap(), (true, 2, LIST_CURRENT));
        assert_eq!(
            w.pin(),
            Some((w.keys.umk.public(), 0, None)),
            "UMK pinned, unverified, as the app pins on first sight"
        );
        assert_eq!(w.cached(), Some((2, v2.encode())));
        assert!(w.command_from(SECOND).is_ok());
        // The same push again changes nothing.
        assert_eq!(w.ingest(&body).unwrap(), (false, 2, LIST_CURRENT));
    }

    #[test]
    fn first_push_needs_the_history_from_version_one_and_a_valid_certificate() {
        let mut w = world();
        let v1 = w.v1();
        let v2 = w
            .keys
            .list(2, Some(&v1), vec![entry(DEVICE, w.user_dsk_pub, None)]);
        // Head only: there is nothing to chain v2 from.
        assert!(w.ingest(&w.keys.push(&[&v2])).is_err());
        // A certificate the UMK did not make.
        let mut forged = DeviceListPush::decode(&w.keys.push(&[&v1])).unwrap();
        forged.sig_umk_ssk[0] ^= 1;
        assert!(w.ingest(&forged.encode()).is_err());
        // An SSK the UMK certified, but lists signed by another SSK.
        let mut other = DeviceListPush::decode(&w.keys.push(&[&v1])).unwrap();
        let stranger = SskSigner::from_bytes(&[0x77; 32]).public();
        other.ssk_pub = stranger;
        other.sig_umk_ssk = w.keys.umk.sign_ssk(&stranger);
        assert!(w.ingest(&other.encode()).is_err());
        assert!(w.ingest(&[0x80]).is_err(), "not a push");
        assert_eq!(w.pin(), None, "nothing pinned");
        assert_eq!(w.cached(), None, "nothing cached");
        assert_eq!(
            w.command_from(DEVICE),
            Err(CommandRefusal::DeviceUnlisted.code())
        );
    }

    #[test]
    fn a_push_under_another_umk_is_held_and_keeps_the_cached_list() {
        let mut w = world();
        let v1 = w.v1();
        w.anchor(&v1);
        let before = w.cached();
        let other = Keys::new(0x0c);
        let theirs = other.list(1, None, vec![entry(SECOND, w.second_dsk_pub, None)]);
        assert_eq!(
            w.ingest(&other.push(&[&theirs])).unwrap(),
            (true, 1, LIST_HELD)
        );
        assert_eq!(w.cached(), before, "the cached list is not replaced");
        assert_eq!(
            w.pin(),
            Some((w.keys.umk.public(), 0, Some(other.umk.public()))),
            "recorded as the pending change"
        );
        assert_eq!(
            w.command_from(DEVICE),
            Err(CommandRefusal::DeviceUnlisted.code()),
            "held until the change is verified"
        );
        assert_eq!(
            w.command_from(SECOND),
            Err(CommandRefusal::DeviceUnlisted.code())
        );
        // The same pending push again changes nothing and stays held.
        assert_eq!(
            w.ingest(&other.push(&[&theirs])).unwrap(),
            (false, 1, LIST_HELD)
        );
        // An unchanged list under the pinned key does not end the pending change either.
        assert_eq!(
            w.ingest(&w.keys.push(&[&v1])).unwrap(),
            (false, 1, LIST_HELD)
        );
    }

    #[test]
    fn ingest_rejects_a_list_with_a_broken_chain() {
        let mut w = world();
        let v1 = w.v1();
        w.anchor(&v1);
        let before = w.cached();
        let v2_entries = vec![
            entry(DEVICE, w.user_dsk_pub, None),
            entry(SECOND, w.second_dsk_pub, None),
        ];
        let v2 = w.keys.list(2, Some(&v1), v2_entries.clone());

        // The pinned UMK certifying a different SSK: refused outright, as on the leaf path.
        let mut new_ssk = DeviceListPush::decode(&w.keys.push(&[&v2])).unwrap();
        let rotated = SskSigner::from_bytes(&[0x78; 32]).public();
        new_ssk.ssk_pub = rotated;
        new_ssk.sig_umk_ssk = w.keys.umk.sign_ssk(&rotated);
        assert!(w.ingest(&new_ssk.encode()).is_err());
        // A row that disagrees with its blob.
        let mut mismatched = DeviceListPush::decode(&w.keys.push(&[&v2])).unwrap();
        mismatched.history[0].version = 3;
        assert!(w.ingest(&mismatched.encode()).is_err());
        assert_eq!(w.cached(), before);
        assert!(w.command_from(DEVICE).is_ok(), "not behind after a refusal");

        // A newer list that does not link to the cache fails closed: behind.
        let mut wrong_prev = v2.clone();
        wrong_prev.unsigned.prev_hash = [0x99; 32];
        wrong_prev.sig_ssk = w.keys.ssk.sign_device_list(&wrong_prev.unsigned);
        assert_eq!(
            w.ingest(&w.keys.push(&[&wrong_prev])).unwrap(),
            (false, 1, LIST_BEHIND)
        );
        assert_eq!(w.cached(), before, "the cache is unchanged");
        assert_eq!(
            w.command_from(DEVICE),
            Err(CommandRefusal::DeviceUnlisted.code())
        );
        assert_eq!(
            w.command_from(SECOND),
            Err(CommandRefusal::DeviceUnlisted.code())
        );

        // The real next link catches up.
        assert_eq!(
            w.ingest(&w.keys.push(&[&v2])).unwrap(),
            (true, 2, LIST_CURRENT)
        );
        assert_eq!(w.cached(), Some((2, v2.encode())));
        assert!(w.command_from(SECOND).is_ok());
    }

    #[test]
    fn older_or_equal_pushes_are_ignored() {
        let mut w = world();
        let v1 = w.v1();
        let v2 = w
            .keys
            .list(2, Some(&v1), vec![entry(DEVICE, w.user_dsk_pub, None)]);
        w.anchor(&v1);
        assert_eq!(
            w.ingest(&w.keys.push(&[&v2])).unwrap(),
            (true, 2, LIST_CURRENT)
        );
        assert_eq!(
            w.ingest(&w.keys.push(&[&v1])).unwrap(),
            (false, 2, LIST_CURRENT)
        );
        assert_eq!(
            w.ingest(&w.keys.push(&[&v1, &v2])).unwrap(),
            (false, 2, LIST_CURRENT)
        );
        let mut fork = v2.clone();
        fork.unsigned
            .entries
            .push(entry(SECOND, w.second_dsk_pub, None));
        fork.sig_ssk = w.keys.ssk.sign_device_list(&fork.unsigned);
        assert_eq!(
            w.ingest(&w.keys.push(&[&fork])).unwrap(),
            (false, 2, LIST_CURRENT)
        );
        assert_eq!(w.cached(), Some((2, v2.encode())));
        assert!(w.command_from(DEVICE).is_ok());
    }

    #[test]
    fn a_gap_fails_closed_until_the_history_arrives() {
        let mut w = world();
        let v1 = w.v1();
        w.anchor(&v1);
        let v2 = w.keys.list(
            2,
            Some(&v1),
            vec![
                entry(DEVICE, w.user_dsk_pub, None),
                entry(SECOND, w.second_dsk_pub, None),
            ],
        );
        let v3 = w.keys.list(
            3,
            Some(&v2),
            vec![
                entry(DEVICE, w.user_dsk_pub, None),
                entry(SECOND, w.second_dsk_pub, None),
            ],
        );
        assert_eq!(
            w.ingest(&w.keys.push(&[&v3])).unwrap(),
            (false, 1, LIST_BEHIND)
        );
        assert_eq!(
            w.command_from(DEVICE),
            Err(CommandRefusal::DeviceUnlisted.code())
        );
        // A history that ends short of the pushed head is not enough.
        assert_eq!(
            w.ingest(&w.keys.push(&[&v2])).unwrap(),
            (true, 2, LIST_BEHIND)
        );
        assert_eq!(
            w.command_from(DEVICE),
            Err(CommandRefusal::DeviceUnlisted.code())
        );
        // The contiguous history reaches the head: caught up.
        assert_eq!(
            w.ingest(&w.keys.push(&[&v1, &v2, &v3])).unwrap(),
            (true, 3, LIST_CURRENT)
        );
        assert!(w.command_from(DEVICE).is_ok());
        assert!(w.command_from(SECOND).is_ok());
    }

    #[test]
    fn a_forged_huge_version_row_does_not_mark_the_user_behind() {
        let mut w = world();
        let v1 = w.v1();
        w.anchor(&v1);
        let before = w.cached();
        // A well-formed list claiming a huge version, with a signature the SSK never made.
        let mut forged = w.keys.list(
            1_000_000,
            Some(&v1),
            vec![entry(DEVICE, w.user_dsk_pub, Some(200))],
        );
        forged.sig_ssk[0] ^= 1;
        assert_eq!(
            w.ingest(&w.keys.push(&[&forged])).unwrap(),
            (false, 1, LIST_CURRENT),
            "an unsigned claim is ignored"
        );
        // Signed by another SSK (the user's UMK did not certify it for this cache).
        let stranger = Keys::new(0x0c);
        let theirs = stranger.list(2, Some(&v1), vec![entry(DEVICE, w.user_dsk_pub, None)]);
        let mut wrong_key = DeviceListPush::decode(&w.keys.push(&[&theirs])).unwrap();
        wrong_key.history[0].ssk_signature = theirs.sig_ssk;
        assert_eq!(
            w.ingest(&wrong_key.encode()).unwrap(),
            (false, 1, LIST_CURRENT)
        );
        assert_eq!(w.count("bot_device_list_behind"), 0);
        assert_eq!(w.cached(), before);
        assert!(w.command_from(DEVICE).is_ok(), "the user is not behind");
        // Rows out of version order are not a history at all.
        let v2 = w
            .keys
            .list(2, Some(&v1), vec![entry(DEVICE, w.user_dsk_pub, None)]);
        assert!(w.ingest(&w.keys.push(&[&v2, &v1])).is_err());
        assert_eq!(w.count("bot_device_list_behind"), 0);
        // A genuine signed head however far past the cache is a mark: fail closed until the
        // history reaches it.
        let far = w
            .keys
            .list(5000, Some(&v1), vec![entry(DEVICE, w.user_dsk_pub, None)]);
        assert_eq!(w.ingest(&w.keys.push(&[&far])).unwrap().2, LIST_BEHIND);
        assert_eq!(w.count("bot_device_list_behind"), 1);
        assert_eq!(w.cached(), before);
        assert!(
            w.command_from(DEVICE).is_err(),
            "a user behind a far signed head is refused"
        );
    }

    #[test]
    fn ingest_accepts_a_contiguous_history_past_a_gap() {
        let mut w = world();
        let v1 = w.v1();
        w.anchor(&v1);
        let mut previous = v1.clone();
        let mut history = vec![v1.clone()];
        for version in 2..=6 {
            let next = w.keys.list(
                version,
                Some(&previous),
                vec![entry(DEVICE, w.user_dsk_pub, None)],
            );
            history.push(next.clone());
            previous = next;
        }
        let refs: Vec<&DeviceList> = history[2..].iter().collect();
        assert_eq!(
            w.ingest(&w.keys.push(&refs)).unwrap(),
            (false, 1, LIST_BEHIND),
            "starting at v3 skips v2"
        );
        let refs: Vec<&DeviceList> = history[1..].iter().collect();
        assert_eq!(
            w.ingest(&w.keys.push(&refs)).unwrap(),
            (true, 6, LIST_CURRENT)
        );
        assert_eq!(w.cached(), Some((6, previous.encode())));
    }

    #[test]
    fn a_device_revoked_inside_the_gap_is_refused() {
        let mut w = world();
        let v1 = w.keys.list(
            1,
            None,
            vec![
                entry(DEVICE, w.user_dsk_pub, None),
                entry(SECOND, w.second_dsk_pub, None),
            ],
        );
        w.anchor(&v1);
        assert!(w.command_from(DEVICE).is_ok());
        // v2 revokes DEVICE; v3 changes something else. The Bot sees only v3 at first.
        let v2 = w.keys.list(
            2,
            Some(&v1),
            vec![
                entry(DEVICE, w.user_dsk_pub, Some(200)),
                entry(SECOND, w.second_dsk_pub, None),
            ],
        );
        let (_, third_pub) = dsk(0x64);
        let v3 = w.keys.list(
            3,
            Some(&v2),
            vec![
                entry(DEVICE, w.user_dsk_pub, Some(200)),
                entry(SECOND, w.second_dsk_pub, None),
                entry([0x59; 16], third_pub, None),
            ],
        );
        assert_eq!(
            w.ingest(&w.keys.push(&[&v3])).unwrap(),
            (false, 1, LIST_BEHIND)
        );
        assert_eq!(
            w.command_from(DEVICE),
            Err(CommandRefusal::DeviceUnlisted.code()),
            "the cached v1 no longer vouches"
        );
        assert_eq!(
            w.ingest(&w.keys.push(&[&v2, &v3])).unwrap(),
            (true, 3, LIST_CURRENT)
        );
        assert_eq!(
            w.command_from(DEVICE),
            Err(CommandRefusal::DeviceRevoked.code())
        );
        assert!(w.command_from(SECOND).is_ok());
    }

    // ---- the health probe (ops 25 and 26, Task 17 fix round 2) ----

    impl World {
        /// The Bot's own device list as dillad's feed pushes it, ingested like any member's.
        fn anchor_bot(&mut self) -> [u8; 16] {
            let (umk_pub, ssk_pub, sig_umk_ssk) = decode_strict(&self.registration, |d| {
                d.array(4)?;
                let keys = (
                    d.bytes_exact::<32>()?,
                    d.bytes_exact::<32>()?,
                    d.bytes_exact::<64>()?,
                );
                d.skip()?;
                Ok(keys)
            })
            .unwrap();
            let list = self.bot_list();
            let device = *list.unsigned.entries[0].device_id.as_bytes();
            let push = DeviceListPush {
                umk_pub,
                ssk_pub,
                sig_umk_ssk,
                history: vec![row(&list)],
            }
            .encode();
            let got = ingest(&self.store, &mut self.core, BOT_USER, &push, T0).unwrap();
            assert_eq!(got.2, LIST_CURRENT);
            device
        }

        fn probe_seal(
            &self,
            replies: &mut ReplyTable,
            request: [u8; 16],
            now: u64,
        ) -> Result<(Vec<u8>, u32), DispatchError> {
            let mut e = Encoder::new();
            e.array(3).bytes(&COMMUNITY).bytes(&request).uint(now);
            let answer = op_probe_seal(&self.store, &self.core, replies, &e.into_vec())?;
            Ok(decode_strict(&answer, |d| {
                d.array(2)?;
                Ok((d.bytes()?.to_vec(), u32::try_from(d.uint()?).unwrap()))
            })
            .unwrap())
        }
    }

    fn probe_open(
        replies: &mut ReplyTable,
        handle: u32,
        seq: u32,
        sealed: &[u8],
        forget: bool,
    ) -> Result<Vec<u8>, DispatchError> {
        let mut e = Encoder::new();
        e.array(4)
            .uint(u64::from(handle))
            .uint(u64::from(seq))
            .bytes(sealed)
            .uint(u64::from(forget));
        let answer = op_probe_open(replies, &e.into_vec())?;
        Ok(decode_strict(&answer, |d| {
            d.array(1)?;
            Ok(d.bytes()?.to_vec())
        })
        .unwrap())
    }

    #[test]
    fn probe_is_a_command_from_the_bots_own_device_and_its_answer_opens() {
        let mut w = world();
        let device = w.anchor_bot();
        let p = w.publication(T0);
        let mut replies = ReplyTable::default();
        let request = [0x9a; 16];
        let (sealed, handle) = w.probe_seal(&mut replies, request, T0).unwrap();
        // Opened by op 22 exactly as a member's command: the Bot's own cached list, its own device.
        let opened = match open(&w.store, &w.core, &sealed, BOT_USER, device, T0 + 1).unwrap() {
            Opened::Request(r) => r,
            Opened::Refused(code, _) => panic!("the probe was refused: {code}"),
            Opened::Replay { .. } => panic!("the probe was a replay"),
        };
        assert_eq!(opened.request_id, request);
        assert_eq!(opened.operation, 90);
        assert_eq!(opened.community_id, CommunityId::from_bytes(COMMUNITY));
        assert!(opened.args.is_empty() && opened.channel_id.is_none());
        // Sealed to the current key, and op 25 neither minted nor rotated one.
        assert_eq!(sealed_command_key_id(&sealed).unwrap(), p.key_id);
        assert_eq!(w.count("bot_command_keys"), 1);
        // The answer, sealed by op 23 to the probe's reply key, opens with the handle (op 26).
        let reply = seal_reply(opened.reply_key, opened.request_id, 0, b"answer").unwrap();
        assert!(
            probe_open(&mut replies, handle, 1, &reply, false).is_err(),
            "another seq must not open"
        );
        assert_eq!(
            probe_open(&mut replies, handle, 0, &reply, true).unwrap(),
            b"answer"
        );
        // Forgotten after the final open.
        assert_eq!(
            probe_open(&mut replies, handle, 0, &reply, true),
            Err(DispatchError::NotFound)
        );
        // Another device's attachment (a relay that lied about the sender) is refused.
        assert!(matches!(
            open(&w.store, &w.core, &sealed, BOT_USER, DEVICE, T0 + 2).unwrap(),
            Opened::Refused(..)
        ));
    }

    #[test]
    fn probe_fails_without_a_current_key_or_for_another_community() {
        let mut w = world();
        w.anchor_bot();
        let mut replies = ReplyTable::default();
        // No key yet: the probe does not mint one (status 5).
        assert_eq!(
            w.probe_seal(&mut replies, [1; 16], T0).err(),
            Some(DispatchError::Unavailable)
        );
        assert_eq!(w.count("bot_command_keys"), 0);
        w.publication(T0);
        assert!(w.probe_seal(&mut replies, [0; 16], T0).is_err());
        let mut e = Encoder::new();
        e.array(3).bytes(&[0x23; 16]).bytes(&[1; 16]).uint(T0);
        assert_eq!(
            op_probe_seal(&w.store, &w.core, &mut replies, &e.into_vec()).err(),
            Some(DispatchError::Invalid)
        );
        // An expired key is purged, never used.
        assert_eq!(
            w.probe_seal(&mut replies, [2; 16], T0 + 9 * DAY).err(),
            Some(DispatchError::Unavailable)
        );
        assert!(replies.is_empty());
    }
}
