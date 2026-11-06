//! Sealed Bot commands in the helper (Bot v2 task 8): the per-community command key and its
//! rotation, opening a sealed command, sealing a reply, and the device-list cache the opener
//! checks signatures against.
//!
//! Ops (payload → answer):
//! - 21 `command_key_current [community_id, now]` → `[publication]`
//! - 22 `command_open [sealed, attached_user, attached_device, now]` → `[0, request_cbor]` or
//!   `[refusal_code]`; `request_cbor` is `[request_id, community_id, channel_id|null, operation,
//!   args, sent_at, reply_key]`
//! - 23 `reply_seal [reply_key, request_id, seq, body]` → `[sealed]`
//! - 24 `device_list_ingest [user_id, signed_list_body]` → `[changed 0|1, version]`
//!
//! Time is the `now` the caller passes, never the helper's clock, and nothing runs on a timer:
//! rotation happens in op 21, and an expired previous key is deleted by the next op 21 or 22.

use crate::helper::{DispatchError, body, map_client};
use crate::store::Store;
use dilla_core::bot::command::{
    CommandArg, CommandRefusal, CommandRequest, MAX_COMMAND_KEY_LIFETIME_SECS,
    MAX_SEALED_COMMAND_BYTES, generate_command_key, open_command, sealed_command_key_id,
    verify_command,
};
use dilla_core::cbor::{Encoder, decode_strict};
use dilla_core::client::ClientCore;
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

pub(crate) enum Opened {
    Request(CommandRequest),
    Refused(u8),
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

/// Deletes every retired key whose 24 h have passed at `now`, then truncates the WAL so no old
/// page image that held the key outlives the deletion (`secure_delete` zeroes the cell in the
/// page itself).
fn purge_retired(conn: &Connection, now: i64) -> Result<(), DispatchError> {
    let deleted = db(conn.execute(
        "DELETE FROM bot_command_keys WHERE retired_at IS NOT NULL AND retired_at <= ?1",
        [now - PREVIOUS_KEPT_SECS],
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
    // A clock behind `created_at` counts as fresh: an earlier `now` never rolls a key back.
    if let Some((_, created_at, publication)) = &read
        && now_i < created_at.saturating_add(ROTATE_AFTER_SECS)
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
    let refused = |r: CommandRefusal| Ok(Opened::Refused(r.code()));
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
                 WHERE key_id=?1 AND (retired_at IS NULL OR retired_at>?2)",
                params![key_id.as_slice(), now_i - PREVIOUS_KEPT_SECS],
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
    let (user, device) = (UserId::from_bytes(user), DeviceId::from_bytes(device));
    if cmd.user_id != user || cmd.device_id != device {
        return refused(CommandRefusal::WrongDevice);
    }
    // The cache only: ingest (op 24) is the one writer, and no command reads a list remotely.
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
    let mut conn = lock(store)?;
    let tx = db(conn.transaction_with_behavior(TransactionBehavior::Immediate))?;
    db(tx.execute(
        "DELETE FROM command_replay WHERE seen_at<?1",
        [now_i - REPLAY_WINDOW_SECS],
    ))?;
    let inserted = db(tx.execute(
        "INSERT OR IGNORE INTO command_replay(request_id,seen_at) VALUES(?1,?2)",
        params![request.request_id.as_slice(), now_i],
    ))?;
    db(tx.commit())?;
    if inserted == 0 {
        return Ok(Opened::Refused(REFUSAL_REPLAY));
    }
    Ok(Opened::Request(request))
}

/// Op 24: the only writer of the device-list cache the opener reads (`ingest_device_list`).
pub(crate) fn ingest(
    core: &mut ClientCore,
    user: [u8; 16],
    body: &[u8],
) -> Result<(bool, u64), DispatchError> {
    core.ingest_device_list(&user, body)
        .map_err(|e| match e.code {
            "E_CORE_LIMIT" => DispatchError::Invalid,
            _ => map_client(e),
        })
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
        Opened::Refused(code) => {
            e.array(1).uint(u64::from(code));
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
    core: &mut ClientCore,
    payload: &[u8],
) -> Result<Zeroizing<Vec<u8>>, DispatchError> {
    let (user, list) = decode_strict(payload, |d| {
        d.array(2)?;
        Ok((d.bytes_exact::<16>()?, d.bytes()?.to_vec()))
    })
    .map_err(invalid)?;
    let (changed, version) = ingest(core, user, &list)?;
    let mut e = Encoder::new();
    e.array(2).uint(u64::from(changed)).uint(version);
    Ok(Zeroizing::new(e.into_vec()))
}

#[cfg(test)]
mod tests {
    use super::*;
    use dilla_core::bot::command::{
        CommandArg, CommandKeyPublication, CommandRefusal, MAX_COMMAND_KEY_LIFETIME_SECS,
        ReplySecret, open_reply, seal_command,
    };
    use dilla_core::cbor::{Encoder, decode_strict};
    use dilla_core::identity::{DeviceEntry, DeviceList, DeviceListUnsigned, SskSigner, Tier};
    use dilla_core::ids::{ChannelId, CommunityId, DeviceId, UserId};
    use openmls::prelude::SignatureScheme;
    use openmls_basic_credential::SignatureKeyPair;
    use rusqlite::{OptionalExtension, params};
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
    const T0: u64 = 1_800_000_000;
    const DAY: u64 = 86_400;
    const UMK: [u8; 32] = [0x0a; 32];

    static NEXT: AtomicU64 = AtomicU64::new(0);

    struct World {
        dir: PathBuf,
        store: Store,
        core: ClientCore,
        user_ssk: SskSigner,
        user_dsk: SignatureKeyPair,
        user_dsk_pub: [u8; 32],
        next_request: u8,
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
        core.bot_bootstrap_begin(&INSTANCE, &COMMUNITY, &INSTALLATION, &[0x77; 32])
            .unwrap();
        core.bot_bootstrap_complete(&BOT_USER, "command bot", T0)
            .unwrap();
        let (user_dsk, user_dsk_pub) = dsk(0x61);
        World {
            dir,
            store,
            core,
            user_ssk: SskSigner::from_bytes(&[0x5a; 32]),
            user_dsk,
            user_dsk_pub,
            next_request: 1,
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

    fn signed(
        ssk: &SskSigner,
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
        let sig_ssk = ssk.sign_device_list(&unsigned);
        DeviceList { unsigned, sig_ssk }
    }

    /// The device-list PUT body dillad pushes: `[version, blob, ssk_signature, prev_hash]`.
    fn body(list: &DeviceList) -> Vec<u8> {
        let mut e = Encoder::new();
        e.array(4)
            .uint(list.unsigned.version)
            .bytes(&list.encode())
            .bytes(&list.sig_ssk)
            .bytes(&list.unsigned.prev_hash);
        e.into_vec()
    }

    fn refused(opened: Opened) -> u8 {
        match opened {
            Opened::Refused(code) => code,
            Opened::Request(_) => 0,
        }
    }

    impl World {
        fn v1(&self) -> DeviceList {
            signed(
                &self.user_ssk,
                1,
                None,
                vec![entry(DEVICE, self.user_dsk_pub, None)],
            )
        }

        /// The first sight of USER, as the MLS leaf path (`device_list_ingest`) leaves it: the
        /// pin and the cached list under the leaf credential's SSK.
        fn anchor(&self, list: &DeviceList) {
            let conn = self.store.conn.lock().unwrap();
            conn.execute(
                "INSERT INTO app_device_lists(user_id,version,list,umk_pub,ssk_pub,credential) VALUES(?1,?2,?3,?4,?5,NULL)",
                params![
                    USER.as_slice(),
                    list.unsigned.version as i64,
                    list.encode(),
                    UMK.as_slice(),
                    self.user_ssk.public().as_slice()
                ],
            )
            .unwrap();
            conn.execute(
                "INSERT INTO app_trust_pins(user_id,umk_pub,first_seen,verified,changed_umk) VALUES(?1,?2,1,0,NULL)",
                params![USER.as_slice(), UMK.as_slice()],
            )
            .unwrap();
        }

        fn cached(&self) -> (i64, Vec<u8>) {
            self.store
                .conn
                .lock()
                .unwrap()
                .query_row(
                    "SELECT version,list FROM app_device_lists WHERE user_id=?1",
                    [USER.as_slice()],
                    |r| Ok((r.get(0)?, r.get(1)?)),
                )
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
            seal_command(
                p,
                req,
                UserId::from_bytes(USER),
                DeviceId::from_bytes(DEVICE),
                &self.user_dsk,
            )
            .unwrap()
            .0
        }

        fn open_at(&self, sealed: &[u8], now: u64) -> Result<CommandRequest, u8> {
            match open(&self.store, &self.core, sealed, USER, DEVICE, now).unwrap() {
                Opened::Request(r) => Ok(r),
                Opened::Refused(code) => Err(code),
            }
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
        let (forged, _) = seal_command(
            &p,
            &req,
            UserId::from_bytes(USER),
            DeviceId::from_bytes(DEVICE),
            &forger,
        )
        .unwrap();
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

    #[test]
    fn ingest_rejects_a_list_with_a_broken_chain() {
        let mut w = world();
        let v1 = w.v1();
        let (second_dsk, second_pub) = dsk(0x63);
        let second = [0x58; 16];
        let v2_entries = vec![
            entry(DEVICE, w.user_dsk_pub, None),
            entry(second, second_pub, None),
        ];
        let v2 = signed(&w.user_ssk, 2, Some(&v1), v2_entries.clone());

        // Nothing cached: a pushed list carries no key to check it against.
        assert!(ingest(&mut w.core, USER, &body(&v2)).is_err());
        assert_eq!(w.count("app_device_lists"), 0);
        w.anchor(&v1);
        let before = w.cached();

        let mut wrong_prev = v2.clone();
        wrong_prev.unsigned.prev_hash = [0x99; 32];
        wrong_prev.sig_ssk = w.user_ssk.sign_device_list(&wrong_prev.unsigned);
        let other_ssk = signed(
            &SskSigner::from_bytes(&[0x5b; 32]),
            2,
            Some(&v1),
            v2_entries,
        );
        let gap = signed(
            &w.user_ssk,
            3,
            Some(&v2),
            vec![entry(DEVICE, w.user_dsk_pub, None)],
        );
        let mut fork = v1.clone();
        fork.unsigned.entries.push(entry(second, second_pub, None));
        fork.sig_ssk = w.user_ssk.sign_device_list(&fork.unsigned);
        // The row's version disagrees with the signed blob's.
        let mut mismatched_row = Encoder::new();
        mismatched_row
            .array(4)
            .uint(3)
            .bytes(&v2.encode())
            .bytes(&v2.sig_ssk)
            .bytes(&v2.unsigned.prev_hash);
        for (name, bad) in [
            ("wrong prev_hash", body(&wrong_prev)),
            ("another SSK", body(&other_ssk)),
            ("a gap", body(&gap)),
            ("a fork of the cached version", body(&fork)),
            (
                "a row that disagrees with its blob",
                mismatched_row.into_vec(),
            ),
            ("not a list", vec![0x80]),
        ] {
            assert!(ingest(&mut w.core, USER, &bad).is_err(), "{name} accepted");
            assert_eq!(w.cached(), before, "{name} changed the cache");
        }

        // The device only the refused lists add is still unlisted.
        let p = w.publication(T0);
        let req = w.request(T0);
        let (sealed, _) = seal_command(
            &p,
            &req,
            UserId::from_bytes(USER),
            DeviceId::from_bytes(second),
            &second_dsk,
        )
        .unwrap();
        assert_eq!(
            refused(open(&w.store, &w.core, &sealed, USER, second, T0).unwrap()),
            CommandRefusal::DeviceUnlisted.code()
        );

        // The next link is accepted, once; the same list again changes nothing.
        assert_eq!(ingest(&mut w.core, USER, &body(&v2)).unwrap(), (true, 2));
        assert_eq!(ingest(&mut w.core, USER, &body(&v2)).unwrap(), (false, 2));
        assert_eq!(w.cached(), (2, v2.encode()));
        assert!(
            ingest(&mut w.core, USER, &body(&v1)).is_err(),
            "a rollback to v1"
        );
        assert!(matches!(
            open(&w.store, &w.core, &sealed, USER, second, T0).unwrap(),
            Opened::Request(_)
        ));

        // v3 revokes the first device: its commands are refused as revoked.
        let v3 = signed(
            &w.user_ssk,
            3,
            Some(&v2),
            vec![
                entry(DEVICE, w.user_dsk_pub, Some(200)),
                entry(second, second_pub, None),
            ],
        );
        assert_eq!(ingest(&mut w.core, USER, &body(&v3)).unwrap(), (true, 3));
        let req = w.request(T0);
        let sealed = w.seal(&p, &req);
        assert_eq!(
            w.open_at(&sealed, T0),
            Err(CommandRefusal::DeviceRevoked.code())
        );
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
}
