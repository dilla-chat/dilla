//! Prepared sends, delivery confirmation, outbox and timeline reads.

use super::error::{
    E_BINDING, E_CORE_INPUT, E_CORE_MLS, E_CORE_NOT_FOUND, E_CORE_STATE, E_CORE_STORAGE,
};
use super::fold::{self, FoldTrigger};
use super::groups::{STATE_ACTIVE, STATE_GONE, checked, group_row};
use super::{ClientCore, ClientError, Own, wire};
use crate::cbor::{Encoder, decode_strict};
use crate::envelope::{Envelope, EnvelopeType};
use crate::ids::MsgId;
use crate::mls::StorageError;
use base64::{Engine as _, engine::general_purpose::URL_SAFE_NO_PAD};
use openmls_traits::OpenMlsProvider;
use openmls_traits::random::OpenMlsRand;
use rusqlite::{OptionalExtension, params};
use sha2::{Digest, Sha256};

pub(crate) const OUTBOX_QUEUED: i64 = 0;
pub(crate) const OUTBOX_IN_FLIGHT: i64 = 1;
pub(crate) const OUTBOX_FAILED: i64 = 2;
pub(crate) const STATUS_OK: i64 = 0;
pub(crate) const STATUS_CANNOT_DECRYPT: i64 = 1;
pub(crate) const STATUS_DELETED: i64 = 2;
pub(crate) const REASON_PRUNED: &str = "E_PRUNED";
pub(crate) const REASON_OWN_UNKNOWN: &str = "E_OWN_UNKNOWN";
pub(crate) const REASON_SENDER_MISMATCH: &str = "E_SENDER_MISMATCH";
/// web3.7 (R5): the host's upload class disagrees with the decrypted type. The row is stored
/// unreadable with type [`TYPE_CLASS_MISMATCH`], outside every displayable and counted query.
pub(crate) const REASON_CLASS_MISMATCH: &str = "E_CLASS_MISMATCH";
pub(crate) const TYPE_CLASS_MISMATCH: i64 = 255;

const BOT_SEND_PREPARING: u64 = 0;
const BOT_SEND_PENDING: u64 = 1;
const BOT_SEND_ACCEPTED: u64 = 2;
const BOT_SEND_CLOSED: u64 = 3;
const BOT_SEND_UNKNOWN: u64 = 4;

#[derive(Clone)]
struct BotSendAttempt {
    predecessor: Option<[u8; 16]>,
    message: [u8; 16],
}

#[derive(Clone, Copy)]
struct HumanInteractionIntent {
    request_id: [u8; 16],
    community: [u8; 16],
    group: [u8; 16],
    input_hash: [u8; 32],
}

fn human_interaction_intent_key(request_id: &[u8; 16]) -> String {
    let mut key = String::from("human_interaction_send/v1/");
    for byte in request_id {
        use core::fmt::Write as _;
        let _ = write!(key, "{byte:02x}");
    }
    key
}

fn encode_human_interaction_intent(intent: HumanInteractionIntent) -> Vec<u8> {
    let mut e = Encoder::new();
    e.array(5)
        .uint(1)
        .bytes(&intent.community)
        .bytes(&intent.group)
        .bytes(&intent.input_hash)
        .bytes(&intent.request_id);
    e.into_vec()
}

fn decode_human_interaction_intent(bytes: &[u8]) -> Result<HumanInteractionIntent, ClientError> {
    decode_strict(bytes, |d| {
        d.array(5)?;
        if d.uint()? != 1 {
            return Err(crate::cbor::CborError::WrongArrayLen {
                expected: 1,
                actual: 0,
            });
        }
        Ok(HumanInteractionIntent {
            community: d.bytes_exact()?,
            group: d.bytes_exact()?,
            input_hash: d.bytes_exact()?,
            request_id: d.bytes_exact()?,
        })
    })
    .map_err(|_| ClientError::new(E_CORE_STORAGE, "human interaction send intent is malformed"))
}

#[derive(Clone)]
struct BotSendEncryptIntent {
    operation: [u8; 16],
    attempt: [u8; 16],
    generation: u64,
    message: [u8; 16],
}

#[derive(Clone, Debug)]
struct BotSendRow {
    state: u64,
    predecessor: Option<[u8; 16]>,
    request_hash: [u8; 32],
    original_now: u64,
    generation: u64,
    message: [u8; 16],
    epoch: Option<u64>,
    class: Option<u64>,
    ciphertext_hash: Option<[u8; 32]>,
    upload: Option<Vec<u8>>,
    receipt: Option<Vec<u8>>,
}

struct BotSendReceipt {
    operation: [u8; 16],
    attempt: [u8; 16],
    predecessor: Option<[u8; 16]>,
    community: [u8; 16],
    group: [u8; 16],
    generation: u64,
    ciphertext_hash: [u8; 32],
    class: u64,
    outcome: u64,
    epoch: u64,
    response: Option<Vec<u8>>,
}

fn decode_bot_send_receipt(bytes: &[u8]) -> Result<BotSendReceipt, ClientError> {
    let receipt = decode_strict(bytes, |d| {
        d.array(12)?;
        if d.uint()? != 1 {
            return Err(crate::cbor::CborError::WrongArrayLen {
                expected: 12,
                actual: 0,
            });
        }
        let r = BotSendReceipt {
            operation: d.bytes_exact()?,
            attempt: d.bytes_exact()?,
            predecessor: d.opt_bytes_exact()?,
            community: d.bytes_exact()?,
            group: d.bytes_exact()?,
            generation: d.uint()?,
            ciphertext_hash: d.bytes_exact()?,
            class: d.uint()?,
            outcome: d.uint()?,
            epoch: d.uint()?,
            response: d.opt_bytes()?.map(ToOwned::to_owned),
        };
        Ok(r)
    })
    .map_err(|_| ClientError::new(E_CORE_INPUT, "bot send receipt is malformed"))?;
    if receipt.operation == [0; 16]
        || receipt.attempt == [0; 16]
        || receipt.predecessor == Some([0; 16])
        || receipt.community == [0; 16]
        || receipt.group == [0; 16]
        || receipt.class > 1
        || receipt.outcome > 2
        || (receipt.outcome == 1) != receipt.response.is_some()
    {
        return Err(ClientError::new(
            E_CORE_INPUT,
            "bot send receipt is malformed",
        ));
    }
    if let Some(response) = receipt.response.as_deref() {
        let decoded = wire::decode_send_response(response)?;
        checked("seq", decoded.seq)?;
        checked("recv_ts", decoded.recv_ts)?;
        if decoded.seq == 0 {
            return Err(ClientError::new(
                E_CORE_INPUT,
                "accepted send receipt sequence must be positive",
            ));
        }
    }
    Ok(receipt)
}

fn encode_human_interaction_request(
    operation_id: &[u8; 16],
    community_id: &[u8; 16],
    request: &[u8],
    now: u64,
) -> Result<String, ClientError> {
    // 4000 body bytes minus the 12-byte prefix leave at most 3988 base64url bytes.
    if request.len() > 2991 {
        return Err(ClientError::new(
            E_CORE_INPUT,
            "human interaction request exceeds body limit",
        ));
    }
    let parsed = crate::bot::Request::decode_cbor(request)
        .map_err(|_| ClientError::new(E_CORE_INPUT, "human interaction request is malformed"))?;
    parsed
        .validate_context(crate::ids::CommunityId::from_bytes(*community_id), now)
        .map_err(|_| {
            ClientError::new(E_CORE_INPUT, "human interaction request context is invalid")
        })?;
    if parsed.request_id.as_bytes() != operation_id
        || parsed.community_id.as_bytes() != community_id
    {
        return Err(ClientError::new(
            E_CORE_INPUT,
            "human interaction request binding mismatch",
        ));
    }
    let body = format!("dilla.bot/1:{}", URL_SAFE_NO_PAD.encode(request));
    if body.len() > 4000 {
        return Err(ClientError::new(
            E_CORE_INPUT,
            "human interaction request exceeds body limit",
        ));
    }
    Ok(body)
}

fn interaction_roster_matches(
    group: &crate::mls::DillaGroup,
    own: &Own,
    bot_user: &[u8; 16],
) -> bool {
    if group.kind() != crate::mls::GroupKind::Interaction {
        return false;
    }
    let mut human = false;
    let mut bot = false;
    for member in group.members() {
        let Ok(basic) = openmls::prelude::BasicCredential::try_from(member.credential) else {
            return false;
        };
        let Ok(identity) = crate::identity::CredentialIdentity::decode(basic.identity()) else {
            return false;
        };
        let Ok(dsk) = <[u8; 32]>::try_from(member.signature_key.as_slice()) else {
            return false;
        };
        if identity.verify_signatures(&dsk).is_err() {
            return false;
        }
        if identity.user_id.as_bytes() == &own.user_id
            && identity.device_id.as_bytes() == &own.device_id
            && identity.kind == crate::identity::Kind::User
            && dsk == own.dsk_pub
        {
            human = true;
        } else if identity.user_id.as_bytes() == bot_user
            && identity.kind == crate::identity::Kind::Bot
            && identity.tier == crate::identity::Tier::Native
            && identity.signer_tier == crate::identity::SignerTier::Native
        {
            bot = true;
        } else {
            return false;
        }
    }
    human && bot
}

fn encode_bot_send_output(message: &[u8; 16], upload: &[u8]) -> Vec<u8> {
    let mut e = Encoder::new();
    e.array(2).bytes(message).bytes(upload);
    e.into_vec()
}

/// The select list every timeline read shares, through the `FROM` keyword (one constant; each
/// read appends its own clause at run time): the stored columns, whether the row is a fold target
/// (not a repeat), its `thread_id`, and its thread summary. The summary is read on a thread root
/// only (the first type-0 row with its msg id, carrying no `thread_id`): the count and latest
/// `recv_ts` of the shown type-0 replies naming it that come after it, through
/// `app_messages_by_thread`.
pub(super) const TIMELINE_SELECT: &str = "SELECT m.seq,m.epoch,m.recv_ts,m.status,m.reason,m.sender_user,m.sender_device, \
     m.sender_kind,m.sender_tier,m.msg_id,m.type,m.body,m.edit_body,m.edit_seq,m.reply_to,m.envelope,m.mention, \
     NOT EXISTS (SELECT 1 FROM app_messages e INDEXED BY app_messages_by_msg WHERE e.group_id=m.group_id AND e.msg_id=m.msg_id \
     AND e.type=0 AND e.status IN (0,2) AND e.seq<m.seq) \
     AND NOT EXISTS (SELECT 1 FROM app_messages r INDEXED BY app_messages_by_pin WHERE r.group_id=m.group_id \
     AND r.reply_to=m.msg_id AND r.type IN (0,1,2,3,4,5,6) AND r.seq<m.seq), \
     m.thread_id, \
     CASE WHEN m.type=0 AND m.thread_id IS NULL AND NOT EXISTS (SELECT 1 FROM app_messages f INDEXED BY app_messages_by_msg \
     WHERE f.group_id=m.group_id AND f.msg_id=m.msg_id AND f.type=0 AND f.seq<m.seq) THEN \
     (SELECT COUNT(*) FROM app_messages s INDEXED BY app_messages_by_thread \
     WHERE s.group_id=m.group_id AND s.thread_id=m.msg_id AND s.type=0 AND s.seq>m.seq AND s.status=0) ELSE 0 END, \
     CASE WHEN m.type=0 AND m.thread_id IS NULL AND NOT EXISTS (SELECT 1 FROM app_messages f INDEXED BY app_messages_by_msg \
     WHERE f.group_id=m.group_id AND f.msg_id=m.msg_id AND f.type=0 AND f.seq<m.seq) THEN (SELECT MAX(s.recv_ts) FROM app_messages s INDEXED BY app_messages_by_thread \
     WHERE s.group_id=m.group_id AND s.thread_id=m.msg_id AND s.type=0 AND s.seq>m.seq AND s.status=0) END \
     FROM app_messages m ";

/// `m.thread_id` names a held root before `m`: the first type-0 row with that msg id (in the
/// group, by seq) carries no `thread_id` and has a lower seq than `m` (fix round 1, I1). Only in a
/// community channel group (FX-core, final review I2): elsewhere a row has no thread and shows in
/// the timeline, whatever its stored `thread_id`.
pub(super) const HELD_ROOT: &str = "(EXISTS (SELECT 1 FROM app_groups g WHERE g.group_id=m.group_id \
     AND g.community_id IS NOT NULL) \
     AND EXISTS (SELECT 1 FROM app_messages t INDEXED BY app_messages_by_msg \
     WHERE t.group_id=m.group_id AND t.msg_id=m.thread_id AND t.type=0 AND t.thread_id IS NULL AND t.seq<m.seq \
     AND NOT EXISTS (SELECT 1 FROM app_messages f INDEXED BY app_messages_by_msg WHERE f.group_id=t.group_id \
     AND f.msg_id=t.msg_id AND f.type=0 AND f.seq<t.seq)))";

/// The main timeline: displayable rows, except a reply whose root is held before it, which shows
/// in its thread only (protocol/04 § Threads).
pub(super) fn timeline_sql() -> String {
    format!(
        "{TIMELINE_SELECT}WHERE m.group_id=?1 AND (m.type IS NULL OR m.type=0) \
         AND (m.thread_id IS NULL OR NOT {HELD_ROOT}) AND (?2=0 OR m.seq<?2) ORDER BY m.seq DESC LIMIT ?3"
    )
}

/// A thread's replies (web3.7): the type-0 rows naming `?2` after its root's seq `?3` (0 when the
/// root is not held), newest first.
pub(super) fn thread_sql() -> String {
    format!(
        "{TIMELINE_SELECT}INDEXED BY app_messages_by_thread WHERE m.group_id=?1 AND m.thread_id=?2 AND m.type=0 \
         AND m.seq>?3 AND (?4=0 OR m.seq<?4) ORDER BY m.seq DESC LIMIT ?5"
    )
}

/// A thread's held root (web3.7): the first type-0 row with msg id `?2`, when it carries no
/// `thread_id`.
pub(super) fn thread_root_sql() -> String {
    format!(
        "{TIMELINE_SELECT}INDEXED BY app_messages_by_msg WHERE m.group_id=?1 AND m.msg_id=?2 AND m.type=0 \
         AND m.thread_id IS NULL AND NOT EXISTS (SELECT 1 FROM app_messages f INDEXED BY app_messages_by_msg \
         WHERE f.group_id=m.group_id AND f.msg_id=m.msg_id AND f.type=0 AND f.seq<m.seq)"
    )
}

/// The group's threads (web3.7): each held root (the first type-0 row with its msg id, carrying no
/// `thread_id`; a fold that reuses the id changes nothing, fix round 2) with a shown type-0 reply after it: the
/// root's seq, status and shown body, the reply count and the latest reply's `recv_ts`; at most
/// 50, the latest reply first.
pub(super) const THREADS_SQL: &str = "SELECT x.thread_id, t.seq, t.status, COALESCE(t.edit_body, t.body), COUNT(*), MAX(x.recv_ts) \
     FROM app_messages x INDEXED BY app_messages_by_thread \
     JOIN app_messages t INDEXED BY app_messages_by_msg ON t.group_id=x.group_id AND t.msg_id=x.thread_id AND t.type=0 \
     AND t.thread_id IS NULL AND t.seq<x.seq \
     WHERE x.group_id=?1 AND x.thread_id IS NOT NULL AND x.type=0 AND x.status=0 \
     AND NOT EXISTS (SELECT 1 FROM app_messages f INDEXED BY app_messages_by_msg WHERE f.group_id=t.group_id \
     AND f.msg_id=t.msg_id AND f.type=0 AND f.seq<t.seq) \
     GROUP BY x.thread_id ORDER BY MAX(x.recv_ts) DESC, x.thread_id LIMIT 50";

/// The thread root of the type-0 row at `?2` when it shows in a thread (its root held before it),
/// for the search hit, pin and reply views (fix round 1, M3).
pub(super) fn shown_thread_root(
    c: &rusqlite::Connection,
    group_id: &[u8; 16],
    seq: u64,
) -> Result<Option<[u8; 16]>, StorageError> {
    c.query_row(
        &format!(
            "SELECT m.thread_id FROM app_messages m WHERE m.group_id=?1 AND m.seq=?2 AND m.type=0 \
             AND m.thread_id IS NOT NULL AND {HELD_ROOT}"
        ),
        params![group_id.as_slice(), seq as i64],
        |r| r.get(0),
    )
    .optional()
    .map_err(Into::into)
}

pub(super) struct StoredMessage<'a> {
    pub group_id: &'a [u8; 16],
    pub seq: u64,
    pub epoch: u64,
    pub recv_ts: u64,
    pub status: i64,
    pub reason: &'a str,
    pub sender_user: Option<&'a [u8; 16]>,
    pub sender_device: &'a [u8; 16],
    pub sender_leaf: Option<u32>,
    pub sender_sig_key: Option<&'a [u8]>,
    pub sender_kind: Option<u8>,
    pub sender_tier: Option<u8>,
    pub msg_id: Option<&'a [u8; 16]>,
    pub ty: Option<u8>,
    pub reply_to: Option<&'a [u8; 16]>,
    /// The envelope's `thread_id`; `insert_message` stores it for a type-0 row only.
    pub thread_id: Option<&'a [u8; 16]>,
    pub body: &'a str,
    pub envelope: Option<&'a [u8]>,
    pub franking_tag: &'a [u8; 32],
    pub mention: bool,
}
pub(super) fn insert_message(
    c: &rusqlite::Connection,
    m: &StoredMessage<'_>,
) -> Result<(), StorageError> {
    c.execute(
        "INSERT INTO app_messages \
         (group_id,seq,epoch,recv_ts,status,reason,sender_user,sender_device, \
          sender_leaf,sender_sig_key,sender_kind,sender_tier,msg_id,type,body,envelope,franking_tag,mention,reply_to,thread_id) \
         VALUES(?1,?2,?3,?4,?5,?6,?7,?8,?9,?10,?11,?12,?13,?14,?15,?16,?17,?18,?19, \
         CASE WHEN EXISTS (SELECT 1 FROM app_groups WHERE group_id=?1 AND community_id IS NOT NULL) THEN ?20 END)",
        params![
            m.group_id.as_slice(),
            m.seq as i64,
            m.epoch as i64,
            m.recv_ts as i64,
            m.status,
            m.reason,
            m.sender_user.map(|v| v.as_slice()),
            m.sender_device.as_slice(),
            m.sender_leaf,
            m.sender_sig_key,
            m.sender_kind,
            m.sender_tier,
            m.msg_id.map(|v| v.as_slice()),
            m.ty,
            m.body,
            m.envelope,
            m.franking_tag.as_slice(),
            i64::from(m.mention),
            m.reply_to.map(|v| v.as_slice()),
            // web3.7: a thread is a property of a message (type 0); a fold names its target only.
            // FX-core (final review I2): and of a community channel group only; the SQL stores none
            // elsewhere, so a DM row with a `thread_id` has no thread.
            m.thread_id.filter(|_| m.ty == Some(0)).map(|v| v.as_slice()),
        ],
    )?;
    super::search::index_fold(c, m.group_id, m.seq)?;
    Ok(())
}

pub(super) fn refold_insert(
    c: &rusqlite::Connection,
    group_id: &[u8; 16],
    seq: u64,
    env: &Envelope,
    sender_user: &[u8; 16],
    sender_device: &[u8; 16],
    own: Option<&Own>,
) -> Result<(), StorageError> {
    let ty = env.kind.as_u8();
    let target = fold::target_of(
        Some(i64::from(ty)),
        Some(*env.msg_id.as_bytes()),
        env.reply_to.as_ref().map(|m| *m.as_bytes()),
    );
    if let Some(target) = target {
        fold::refold(
            c,
            group_id,
            &target,
            &fold::scope_of(Some(i64::from(ty)), Some(*sender_user), &env.body),
            Some(&FoldTrigger {
                seq,
                ty,
                sender_user: *sender_user,
                sender_device: *sender_device,
            }),
            own,
        )?;
        if let Some(row) = fold::resolve(c, group_id, &target)? {
            super::search::index_fold(c, group_id, row.seq)?;
        }
    }
    Ok(())
}
#[allow(dead_code)] // The creation time is part of the persisted row for later sync operations.
pub(super) struct OutboxRow {
    pub group_id: [u8; 16],
    pub envelope: Vec<u8>,
    pub created: i64,
    pub state: i64,
    pub epoch: i64,
}
pub(super) fn outbox_row(
    c: &rusqlite::Connection,
    id: &[u8; 16],
) -> Result<Option<OutboxRow>, StorageError> {
    c.query_row(
        "SELECT group_id,envelope,created,state,epoch FROM app_outbox WHERE msg_id=?1",
        [id.as_slice()],
        |r| {
            Ok(OutboxRow {
                group_id: r.get(0)?,
                envelope: r.get(1)?,
                created: r.get(2)?,
                state: r.get(3)?,
                epoch: r.get(4)?,
            })
        },
    )
    .optional()
    .map_err(Into::into)
}
fn not_found() -> ClientError {
    ClientError::new(E_CORE_NOT_FOUND, "")
}

fn no_attachment() -> ClientError {
    ClientError::new(E_CORE_NOT_FOUND, "no such attachment")
}

fn bot_send_conflict() -> ClientError {
    ClientError::new(
        E_CORE_STATE,
        "bot send attempt conflicts with durable intent",
    )
}

fn id_hex(id: &[u8; 16]) -> String {
    let mut s = String::with_capacity(32);
    for b in id {
        use std::fmt::Write;
        let _ = write!(s, "{b:02x}");
    }
    s
}

fn bot_send_op_key(operation: &[u8; 16]) -> String {
    format!("bot_send_op_{}", id_hex(operation))
}

fn bot_send_attempt_key(operation: &[u8; 16], attempt: &[u8; 16]) -> String {
    format!("bot_send_try_{}_{}", id_hex(operation), id_hex(attempt))
}

fn encode_bot_send_row(row: &BotSendRow) -> Vec<u8> {
    let mut e = Encoder::new();
    e.array(12)
        .uint(1)
        .uint(row.state)
        .opt_bytes(row.predecessor.as_ref().map(|v| v.as_slice()))
        .bytes(&row.request_hash)
        .uint(row.original_now)
        .uint(row.generation)
        .bytes(&row.message)
        .opt_uint(row.epoch)
        .opt_uint(row.class)
        .opt_bytes(row.ciphertext_hash.as_ref().map(|v| v.as_slice()))
        .opt_bytes(row.upload.as_deref())
        .opt_bytes(row.receipt.as_deref());
    e.into_vec()
}

fn decode_bot_send_row(bytes: &[u8]) -> Result<BotSendRow, ClientError> {
    let row = decode_strict(bytes, |d| {
        d.array(12)?;
        if d.uint()? != 1 {
            return Err(crate::cbor::CborError::WrongArrayLen {
                expected: 12,
                actual: 0,
            });
        }
        let state = d.uint()?;
        if state > BOT_SEND_UNKNOWN {
            return Err(crate::cbor::CborError::WrongArrayLen {
                expected: 12,
                actual: 0,
            });
        }
        Ok(BotSendRow {
            state,
            predecessor: d.opt_bytes_exact::<16>()?,
            request_hash: d.bytes_exact()?,
            original_now: d.uint()?,
            generation: d.uint()?,
            message: d.bytes_exact()?,
            epoch: d.opt_uint()?,
            class: d.opt_uint()?,
            ciphertext_hash: d.opt_bytes_exact::<32>()?,
            upload: d.opt_bytes()?.map(ToOwned::to_owned),
            receipt: d.opt_bytes()?.map(ToOwned::to_owned),
        })
    })
    .map_err(|_| ClientError::new(E_CORE_STORAGE, "bot send journal is malformed"))?;
    let fields_match = match row.state {
        BOT_SEND_PREPARING => {
            row.epoch.is_none()
                && row.class.is_none()
                && row.ciphertext_hash.is_none()
                && row.upload.is_none()
                && row.receipt.is_none()
        }
        BOT_SEND_PENDING => {
            row.epoch.is_some()
                && row.class.is_some()
                && row.ciphertext_hash.is_some()
                && row.upload.is_some()
                && row.receipt.is_none()
        }
        BOT_SEND_ACCEPTED | BOT_SEND_CLOSED | BOT_SEND_UNKNOWN => {
            row.epoch.is_some()
                && row.class.is_some()
                && row.ciphertext_hash.is_some()
                && row.upload.is_some()
                && row.receipt.is_some()
        }
        _ => false,
    };
    if row.message == [0; 16]
        || row.predecessor == Some([0; 16])
        || row.original_now > i64::MAX as u64
        || row.generation > i64::MAX as u64
        || row.epoch.is_some_and(|v| v > i64::MAX as u64)
        || row.class.is_some_and(|v| v > 1)
        || !fields_match
    {
        return Err(ClientError::new(
            E_CORE_STORAGE,
            "bot send journal is malformed",
        ));
    }
    if let Some(upload) = row.upload.as_deref() {
        let (epoch, hash, class) = bot_send_upload_fields(upload)?;
        if row.epoch != Some(epoch) || row.ciphertext_hash != Some(hash) || row.class != Some(class)
        {
            return Err(ClientError::new(
                E_CORE_STORAGE,
                "bot send ciphertext journal is inconsistent",
            ));
        }
    }
    if let Some(receipt) = row.receipt.as_deref() {
        if receipt.len() > 512 {
            return Err(ClientError::new(
                E_CORE_STORAGE,
                "bot send receipt journal is malformed",
            ));
        }
        let decoded = decode_bot_send_receipt(receipt).map_err(|_| {
            ClientError::new(E_CORE_STORAGE, "bot send receipt journal is malformed")
        })?;
        let outcome_matches = matches!(
            (row.state, decoded.outcome),
            (BOT_SEND_ACCEPTED, 1) | (BOT_SEND_CLOSED, 0) | (BOT_SEND_UNKNOWN, 2)
        );
        if !outcome_matches
            || row.predecessor != decoded.predecessor
            || row.generation != decoded.generation
            || row.epoch != Some(decoded.epoch)
            || row.class != Some(decoded.class)
            || row.ciphertext_hash != Some(decoded.ciphertext_hash)
        {
            return Err(ClientError::new(
                E_CORE_STORAGE,
                "bot send receipt journal is inconsistent",
            ));
        }
    }
    Ok(row)
}

type BotSendOperationTuple = (
    [u8; 16],
    [u8; 16],
    [u8; 16],
    [u8; 16],
    [u8; 16],
    [u8; 32],
    [u8; 16],
);

fn decode_bot_send_operation(bytes: &[u8]) -> Result<BotSendOperationTuple, ClientError> {
    decode_strict(bytes, |d| {
        d.array(8)?;
        if d.uint()? != 1 {
            return Err(crate::cbor::CborError::WrongArrayLen {
                expected: 8,
                actual: 0,
            });
        }
        Ok((
            d.bytes_exact()?,
            d.bytes_exact()?,
            d.bytes_exact()?,
            d.bytes_exact()?,
            d.bytes_exact()?,
            d.bytes_exact()?,
            d.bytes_exact()?,
        ))
    })
    .map_err(|_| ClientError::new(E_CORE_STORAGE, "bot send operation journal is malformed"))
}

fn encode_bot_send_operation(
    installation: &[u8; 16],
    user: &[u8; 16],
    device: &[u8; 16],
    community: &[u8; 16],
    group: &[u8; 16],
    request_hash: &[u8; 32],
    current_attempt: &[u8; 16],
) -> Vec<u8> {
    let mut e = Encoder::new();
    e.array(8)
        .uint(1)
        .bytes(installation)
        .bytes(user)
        .bytes(device)
        .bytes(community)
        .bytes(group)
        .bytes(request_hash)
        .bytes(current_attempt);
    e.into_vec()
}

fn bot_send_upload_fields(upload: &[u8]) -> Result<(u64, [u8; 32], u64), ClientError> {
    let (epoch, ciphertext, class) = decode_strict(upload, |d| {
        d.array(3)?;
        Ok((d.uint()?, d.bytes()?.to_vec(), d.uint()?))
    })
    .map_err(|_| ClientError::new(E_CORE_STORAGE, "bot send upload is malformed"))?;
    checked("epoch", epoch)?;
    if class > 1 {
        return Err(ClientError::new(
            E_CORE_STORAGE,
            "bot send class is malformed",
        ));
    }
    Ok((epoch, Sha256::digest(&ciphertext).into(), class))
}

impl ClientCore {
    /// `read` behind one non-generic instance: each reader of web-2b adds its closure body and not
    /// another copy of the storage unit (wasm size, the task 3 budget).
    #[inline(never)]
    fn read_dyn(
        &self,
        f: &mut dyn FnMut(&rusqlite::Connection) -> Result<(), StorageError>,
    ) -> Result<(), ClientError> {
        self.read(f)
    }

    /// `write` for a statement-only unit behind one non-generic instance (wasm size).
    #[inline(never)]
    fn write_dyn(
        &mut self,
        f: &mut dyn FnMut(&rusqlite::Connection) -> Result<(), StorageError>,
    ) -> Result<(), ClientError> {
        self.write(|_, u| u.with_conn(f).map_err(Into::into))
    }

    /// `read_dyn` for a closure that produces a value.
    fn read_one<T>(
        &self,
        f: impl FnOnce(&rusqlite::Connection) -> Result<T, StorageError>,
    ) -> Result<T, ClientError> {
        let mut f = Some(f);
        let mut out = None;
        self.read_dyn(&mut |c| {
            if let Some(f) = f.take() {
                out = Some(f(c)?);
            }
            Ok(())
        })?;
        out.ok_or_else(|| ClientError::new(E_CORE_STORAGE, "the read produced no value"))
    }

    /// Advance this device's read marker, clamped to the group's current head.
    pub fn mark_read(&mut self, id: &[u8; 16], seq: u64, now: u64) -> Result<(), ClientError> {
        self.own()?;
        let now = checked("now", now)?;
        let row = self.read(|c| group_row(c, id))?.ok_or_else(not_found)?;
        if row.state == STATE_GONE {
            return Err(ClientError::new(
                E_CORE_STATE,
                format!("group state {}", row.state),
            ));
        }
        let target = seq.min((row.next_seq - 1) as u64);
        let target = checked("seq", target)?;
        self.write(|_, u| {
            u.with_conn(|c| {
                c.execute(
                    "INSERT INTO app_read_state (group_id, last_read_seq, last_read_at) VALUES (?1, ?2, ?3) \
                     ON CONFLICT (group_id) DO UPDATE SET last_read_seq = MAX(last_read_seq, excluded.last_read_seq), \
                     last_read_at = excluded.last_read_at",
                    params![id.as_slice(), target, now],
                )?;
                Ok(())
            })?;
            Ok(())
        })
    }

    /// Per-group unread and mention counts for joined or resyncing groups.
    pub fn activity(&self) -> Result<Vec<u8>, ClientError> {
        let own = self.own()?;
        type ActivityRow = ([u8; 16], i64, i64, i64, i64, i64);
        let rows: Vec<ActivityRow> = self.read(|c| {
            let mut stmt = c.prepare(
                "SELECT g.group_id, \
                 (SELECT COUNT(*) FROM app_messages m WHERE m.group_id = g.group_id AND m.status = 0 AND m.type = 0 \
                    AND m.sender_user <> ?1 AND m.seq > COALESCE(r.last_read_seq, 0)), \
                 (SELECT COUNT(*) FROM app_messages m WHERE m.group_id = g.group_id AND m.status = 0 AND m.type = 0 \
                    AND m.sender_user <> ?1 AND m.seq > COALESCE(r.last_read_seq, 0) AND m.mention = 1), \
                 COALESCE((SELECT m.seq FROM app_messages m WHERE m.group_id = g.group_id AND m.status = 0 AND m.type = 0 \
                    AND m.sender_user <> ?1 AND m.seq > COALESCE(r.last_read_seq, 0) ORDER BY m.seq DESC LIMIT 1), 0), \
                 COALESCE((SELECT m.recv_ts FROM app_messages m WHERE m.group_id = g.group_id AND m.status = 0 AND m.type = 0 \
                    AND m.sender_user <> ?1 AND m.seq > COALESCE(r.last_read_seq, 0) ORDER BY m.seq DESC LIMIT 1), 0), \
                 COALESCE(r.last_read_seq, 0) \
                 FROM app_groups g LEFT JOIN app_read_state r ON r.group_id = g.group_id \
                 WHERE g.state IN (2, 3) ORDER BY g.group_id",
            )?;
            stmt.query_map([own.user_id.as_slice()], |r| {
                Ok((r.get(0)?, r.get(1)?, r.get(2)?, r.get(3)?, r.get(4)?, r.get(5)?))
            })?
            .collect::<Result<Vec<_>, _>>()
            .map_err(Into::into)
        })?;
        let mut e = Encoder::new();
        e.array(rows.len());
        for (id, unread, mentions, last_seq, last_ts, last_read_seq) in rows {
            e.array(6)
                .bytes(&id)
                .uint(unread as u64)
                .uint(mentions as u64)
                .uint(last_seq as u64)
                .uint(last_ts as u64)
                .uint(last_read_seq as u64);
        }
        Ok(e.into_vec())
    }
}
/// `before_seq` as SQLite's bound (a value above its signed range is later than every stored
/// sequence, so 0: from the newest), after the page size's range check (1..=200).
fn page_bound(before_seq: u64, limit: u32) -> Result<i64, ClientError> {
    if !(1..=200).contains(&limit) {
        return Err(ClientError::new(E_CORE_INPUT, "limit out of range"));
    }
    Ok(if before_seq > i64::MAX as u64 {
        0
    } else {
        before_seq as i64
    })
}

/// One row of a `timeline_select!` query, before its view is built.
struct RawRow {
    seq: i64,
    epoch: i64,
    recv_ts: i64,
    status: i64,
    reason: String,
    sender_user: Option<Vec<u8>>,
    sender_device: Vec<u8>,
    sender_kind: Option<i64>,
    sender_tier: Option<i64>,
    msg_id: Option<[u8; 16]>,
    ty: Option<i64>,
    body: String,
    edit_body: Option<String>,
    edit_seq: i64,
    reply_to: Option<[u8; 16]>,
    envelope: Option<Vec<u8>>,
    mention: i64,
    is_target: bool,
    thread_id: Option<[u8; 16]>,
    thread_count: i64,
    thread_last: Option<i64>,
}

/// The views of a `timeline_select!` query's rows, in the query's order: one non-generic
/// instance for every timeline read (wasm size).
#[inline(never)]
fn timeline_views(
    c: &rusqlite::Connection,
    sql: &str,
    args: &[&dyn rusqlite::ToSql],
    id: &[u8; 16],
    own_user: &[u8; 16],
) -> Result<Vec<wire::TimelineView>, StorageError> {
    let mut s = c.prepare(sql)?;
    let it = s.query_map(args, |r| {
        Ok(RawRow {
            seq: r.get(0)?,
            epoch: r.get(1)?,
            recv_ts: r.get(2)?,
            status: r.get(3)?,
            reason: r.get(4)?,
            sender_user: r.get(5)?,
            sender_device: r.get(6)?,
            sender_kind: r.get(7)?,
            sender_tier: r.get(8)?,
            msg_id: r.get(9)?,
            ty: r.get(10)?,
            body: r.get(11)?,
            edit_body: r.get(12)?,
            edit_seq: r.get(13)?,
            reply_to: r.get(14)?,
            envelope: r.get(15)?,
            mention: r.get(16)?,
            is_target: r.get::<_, i64>(17)? != 0,
            thread_id: r.get(18)?,
            thread_count: r.get(19)?,
            thread_last: r.get(20)?,
        })
    })?;
    let raw: Vec<RawRow> = it.collect::<Result<_, _>>()?;
    raw.into_iter()
        .map(|r| view_of(c, id, own_user, r))
        .collect()
}

/// The 19-element view of one stored row: the folded body, the reply reference, reactions, the
/// pin flag, attachment summaries and, on a held thread root with a shown reply, its summary.
fn view_of(
    c: &rusqlite::Connection,
    id: &[u8; 16],
    own_user: &[u8; 16],
    r: RawRow,
) -> Result<wire::TimelineView, StorageError> {
    let body = if r.status == STATUS_OK {
        r.edit_body.unwrap_or(r.body)
    } else {
        String::new()
    };
    let reply = if r.status == STATUS_OK {
        r.reply_to
            .map(|reply_to| {
                let target = fold::resolve(c, id, &reply_to)?;
                Ok::<_, StorageError>(wire::ReplyView {
                    reply_to,
                    target_seq: target.as_ref().map(|t| t.seq),
                    target_user: target.as_ref().and_then(|t| t.sender_user),
                    excerpt: target
                        .as_ref()
                        .filter(|t| t.status == STATUS_OK)
                        .map_or_else(String::new, |t| fold::excerpt(&t.shown_body)),
                    state: target
                        .as_ref()
                        .map_or(1, |t| if t.status == STATUS_OK { 0 } else { 2 }),
                    thread_root: match &target {
                        Some(t) => shown_thread_root(c, id, t.seq)?,
                        None => None,
                    },
                })
            })
            .transpose()?
    } else {
        None
    };
    let mut reactions = Vec::new();
    let mut pinned = false;
    if r.status == STATUS_OK
        && r.ty == Some(0)
        && r.is_target
        && let Some(msg) = r.msg_id
    {
        let mut q = c.prepare("SELECT emoji, n, mine FROM (SELECT emoji, COUNT(*) AS n, MAX(user_id = ?3) AS mine, MIN(seq) AS first FROM app_reactions WHERE group_id = ?1 AND target = ?2 GROUP BY emoji ORDER BY n DESC, first, emoji LIMIT 20) ORDER BY first, emoji")?;
        reactions = q
            .query_map(
                params![id.as_slice(), msg.as_slice(), own_user.as_slice()],
                |x| {
                    Ok((
                        x.get(0)?,
                        x.get::<_, i64>(1)? as u64,
                        x.get::<_, i64>(2)? != 0,
                    ))
                },
            )?
            .collect::<Result<_, _>>()?;
        pinned = c
            .query_row(
                "SELECT 1 FROM app_pins WHERE group_id = ?1 AND target = ?2",
                params![id.as_slice(), msg.as_slice()],
                |_| Ok(()),
            )
            .optional()?
            .is_some();
    }
    let attachments = if r.status == STATUS_OK && r.ty == Some(0) {
        r.envelope
            .as_deref()
            .and_then(|bytes| Envelope::decode(bytes).ok())
            .map_or_else(Vec::new, |env| {
                env.attachments
                    .into_iter()
                    .map(|a| wire::AttachmentView {
                        size: a.size,
                        mime: a.mime,
                        w: a.w,
                        h: a.h,
                        has_thumb: a.thumb.is_some(),
                        name: a.name,
                    })
                    .collect()
            })
    } else {
        Vec::new()
    };
    // A root is a type-0 row without a thread id; the summary needs a shown reply.
    let thread =
        (r.ty == Some(0) && r.thread_id.is_none() && r.msg_id.is_some() && r.thread_count > 0)
            .then(|| (r.thread_count as u64, r.thread_last.unwrap_or(0) as u64));
    Ok(wire::TimelineView {
        seq: r.seq as u64,
        epoch: r.epoch as u64,
        recv_ts: r.recv_ts as u64,
        status: r.status as u64,
        reason: r.reason,
        sender_user: r.sender_user,
        sender_device: r.sender_device,
        sender_kind: r.sender_kind.map(|v| v as u64),
        sender_tier: r.sender_tier.map(|v| v as u64),
        msg_id: r.msg_id.map(|m| m.to_vec()),
        ty: r.ty.map(|v| v as u64),
        body,
        edited_seq: r.edit_seq as u64,
        reply,
        reactions,
        pinned,
        attachments,
        mention: r.mention != 0,
        thread,
    })
}

fn outbox_state(s: i64) -> ClientError {
    ClientError::new(E_CORE_STATE, format!("outbox row in state {s}"))
}
#[allow(clippy::too_many_arguments)] // The persisted row draws fields from all three inputs.
fn own_message<'a>(
    own: &'a Own,
    g: &'a [u8; 16],
    seq: u64,
    epoch: u64,
    recv_ts: u64,
    tag: &'a [u8; 32],
    env: &'a Envelope,
    bytes: &'a [u8],
    leaf: Option<u32>,
) -> StoredMessage<'a> {
    StoredMessage {
        group_id: g,
        seq,
        epoch,
        recv_ts,
        status: STATUS_OK,
        reason: "",
        sender_user: Some(&own.user_id),
        sender_device: &own.device_id,
        sender_leaf: leaf,
        sender_sig_key: None,
        sender_kind: Some(own.kind),
        sender_tier: Some(own.tier),
        msg_id: Some(env.msg_id.as_bytes()),
        ty: Some(env.kind.as_u8()),
        reply_to: env.reply_to.as_ref().map(MsgId::as_bytes),
        thread_id: env.thread_id.as_ref().map(MsgId::as_bytes),
        body: &env.body,
        envelope: Some(bytes),
        franking_tag: tag,
        mention: false,
    }
}

/// true when `body` contains "<@" + the 32 lower-case hex characters of `user_id` + ">", or "<@everyone>",
/// or "<@here>" — the readable mention syntax of protocol/09 (`<@(everyone|here|[0-9a-f]{32})>`), matched
/// literally (no regex, no case folding; upper-case hex does not match).
///
/// It refuses nothing: a member who types `<@everyone>` raises a count, which is the readable
/// syntax's meaning (Q09, Q20).
pub fn mentions_me(body: &str, user_id: &[u8; 16]) -> bool {
    // A byte search: every needle is ASCII, so a byte match is a match of the same characters.
    // (`str::contains` would add its two-way searcher to the browser build.)
    let has = |n: &[u8]| body.as_bytes().windows(n.len()).any(|w| w == n);
    has(&mention_needle(user_id)) || has(b"<@everyone>") || has(b"<@here>")
}

/// Matches the own user's and known role mentions using the literal lower-case token syntax.
pub fn mentions_me_with_roles(body: &str, user_id: &[u8; 16], roles: &[[u8; 16]]) -> bool {
    mentions_me(body, user_id)
        || roles.iter().any(|role| {
            let needle = mention_needle(role);
            body.as_bytes().windows(needle.len()).any(|w| w == needle)
        })
}

/// `mentions_me_with_roles` gated by the group's role view (web3.5 R1, protocol/04 § Semantics):
/// `<@everyone>`/`<@here>` count only when `broadcast_ok` (the sender may `mention_everyone` in
/// the group, or the group has no known role view); a token of one of the own `roles` counts only
/// when `broadcast_ok` or the role is in `mentionable` (the view's roles anyone may mention). A
/// direct `<@own>` counts either way; a role not in `roles` never does.
pub fn mentions_me_gated(
    body: &str,
    user_id: &[u8; 16],
    roles: &[[u8; 16]],
    mentionable: &[[u8; 16]],
    broadcast_ok: bool,
) -> bool {
    let has = |n: &[u8]| body.as_bytes().windows(n.len()).any(|w| w == n);
    has(&mention_needle(user_id))
        || broadcast_ok && (has(b"<@everyone>") || has(b"<@here>"))
        || roles
            .iter()
            .any(|role| (broadcast_ok || mentionable.contains(role)) && has(&mention_needle(role)))
}

/// Whether `sender` counts for `<@everyone>`/`<@here>` in `group_id`: the group has no known role
/// view (`app_authority_known`), or the view gives `sender` `mention_everyone` (bit 18).
pub(super) fn broadcast_ok(
    c: &rusqlite::Connection,
    group_id: &[u8; 16],
    sender: &[u8; 16],
) -> Result<bool, StorageError> {
    c.query_row(
        "SELECT NOT EXISTS (SELECT 1 FROM app_authority_known WHERE group_id = ?1) \
         OR EXISTS (SELECT 1 FROM app_authority WHERE group_id = ?1 AND bit = 18 AND user_id = ?2)",
        params![group_id.as_slice(), sender.as_slice()],
        |r| r.get(0),
    )
    .map_err(Into::into)
}

/// The own role ids of `group_id`'s community, as the mention flag reads them.
pub(super) fn own_roles_of(
    c: &rusqlite::Connection,
    group_id: &[u8; 16],
) -> Result<Vec<[u8; 16]>, StorageError> {
    let mut q = c.prepare("SELECT r.role_id FROM app_roles r JOIN app_groups g ON g.community_id = r.community_id WHERE g.group_id = ?1 ORDER BY r.role_id")?;
    q.query_map([group_id.as_slice()], |row| row.get(0))?
        .collect::<Result<_, _>>()
        .map_err(Into::into)
}

/// The role ids `group_id`'s view marks mentionable (`app_authority_mentionable`), sorted; empty
/// while the view is unknown, when `broadcast_ok` counts every own role anyway.
pub(super) fn mentionable_of(
    c: &rusqlite::Connection,
    group_id: &[u8; 16],
) -> Result<Vec<[u8; 16]>, StorageError> {
    let mut q = c.prepare(
        "SELECT role_id FROM app_authority_mentionable WHERE group_id = ?1 ORDER BY role_id",
    )?;
    q.query_map([group_id.as_slice()], |row| row.get(0))?
        .collect::<Result<_, _>>()
        .map_err(Into::into)
}

/// Upper bound on each id list `authority_set` takes.
const AUTHORITY_MAX_IDS: usize = 4096;

/// `ids` sorted and deduplicated: `authority_set`'s id sets, kept as sorted vectors rather than a
/// `BTreeSet` so the wasm build carries no extra B-tree code for them.
fn sorted_ids(mut ids: Vec<[u8; 16]>) -> Vec<[u8; 16]> {
    ids.sort_unstable();
    ids.dedup();
    ids
}

/// The ids in exactly one of the sorted sets `a` and `b`, sorted.
fn changed_ids(a: &[[u8; 16]], b: &[[u8; 16]]) -> Vec<[u8; 16]> {
    let only = |x: &[[u8; 16]], y: &[[u8; 16]]| {
        x.iter()
            .filter(|id| y.binary_search(id).is_err())
            .copied()
            .collect::<Vec<_>>()
    };
    let mut out = only(a, b);
    out.extend(only(b, a));
    out.sort_unstable();
    out
}

/// `mentions_me`'s needle for one user: "<@" + the 32 lower-case hex characters + ">".
pub(super) fn mention_needle(user_id: &[u8; 16]) -> [u8; 35] {
    const HEX: &[u8; 16] = b"0123456789abcdef";
    let mut needle = *b"<@0123456789abcdef0123456789abcdef>";
    for (i, b) in user_id.iter().enumerate() {
        needle[2 + 2 * i] = HEX[usize::from(b >> 4)];
        needle[3 + 2 * i] = HEX[usize::from(b & 0x0f)];
    }
    needle
}

/// What a send's `thread_id` names in the group (web3.7, protocol/04 § Threads).
enum ThreadRoot {
    /// A held type-0 row without a `thread_id`: a root.
    Root,
    /// Only type-0 rows that carry a `thread_id` themselves: a reply, never a root.
    Reply,
    /// Only rows of another type (a fold).
    NotAPost,
    /// No row with that msg id.
    Unheld,
}

fn thread_root(
    c: &rusqlite::Connection,
    group_id: &[u8; 16],
    msg_id: &[u8; 16],
) -> Result<ThreadRoot, StorageError> {
    // The first type-0 row with the msg id decides (fix round 1, I1: only it can be the root);
    // without one, any other row makes the id a fold's.
    let best: Option<(Option<i64>, bool)> = c
        .query_row(
            "SELECT type, thread_id IS NULL FROM app_messages INDEXED BY app_messages_by_msg \
             WHERE group_id = ?1 AND msg_id = ?2 \
             ORDER BY type = 0 DESC, seq LIMIT 1",
            params![group_id.as_slice(), msg_id.as_slice()],
            |r| Ok((r.get(0)?, r.get(1)?)),
        )
        .optional()?;
    Ok(match best {
        None => ThreadRoot::Unheld,
        Some((Some(0), true)) => ThreadRoot::Root,
        Some((Some(0), false)) => ThreadRoot::Reply,
        Some(_) => ThreadRoot::NotAPost,
    })
}

impl ClientCore {
    /// Prepare one native Bot send attempt and retain the exact encrypted upload before returning it.
    pub fn bot_send_prepare(
        &mut self,
        operation_id: &[u8; 16],
        attempt_id: &[u8; 16],
        group_id: &[u8; 16],
        request_body: &[u8],
        original_now: u64,
        expected_generation: u64,
    ) -> Result<Vec<u8>, ClientError> {
        self.bot_send_prepare_scoped(
            operation_id,
            attempt_id,
            group_id,
            request_body,
            original_now,
            expected_generation,
            None,
        )
    }

    /// Prepare a Bot response for a kind-3 interaction only after revalidating the current signed
    /// admission and the exact authenticated two-leaf roster.
    #[expect(
        clippy::too_many_arguments,
        reason = "The request fields are bound independently into the durable intent."
    )]
    pub fn bot_interaction_send_prepare(
        &mut self,
        operation_id: &[u8; 16],
        attempt_id: &[u8; 16],
        group_id: &[u8; 16],
        request_body: &[u8],
        original_now: u64,
        expected_generation: u64,
        snapshot_wire: &[u8],
        expected: &crate::bot::interaction_admission::Expected,
    ) -> Result<Vec<u8>, ClientError> {
        self.bot_inspect_stored_interaction_group(group_id, snapshot_wire, expected)?;
        self.bot_send_prepare_scoped(
            operation_id,
            attempt_id,
            group_id,
            request_body,
            original_now,
            expected_generation,
            Some((snapshot_wire, expected)),
        )
    }

    #[expect(
        clippy::too_many_arguments,
        reason = "The request fields are bound independently into the durable intent."
    )]
    fn bot_send_prepare_scoped(
        &mut self,
        operation_id: &[u8; 16],
        attempt_id: &[u8; 16],
        group_id: &[u8; 16],
        request_body: &[u8],
        original_now: u64,
        expected_generation: u64,
        interaction_proof: Option<(&[u8], &crate::bot::interaction_admission::Expected)>,
    ) -> Result<Vec<u8>, ClientError> {
        if operation_id == &[0; 16] || attempt_id == &[0; 16] || group_id == &[0; 16] {
            return Err(ClientError::new(
                E_CORE_INPUT,
                "bot send ids must be nonzero",
            ));
        }
        checked("now", original_now)?;
        checked("expected_generation", expected_generation)?;
        wire::decode_send_request(request_body)?;
        let binding = super::bot_intents::binding(self)?;
        let own = self.own()?;
        let request_hash: [u8; 32] = Sha256::digest(request_body).into();
        let op_key = bot_send_op_key(operation_id);
        let attempt_key = bot_send_attempt_key(operation_id, attempt_id);
        let existing_op = self.read(|c| super::schema::meta_get(c, &op_key))?;
        let attempt = if let Some(raw) = existing_op {
            let (installation, user, device, community, group, old_request_hash, current) =
                decode_bot_send_operation(&raw)?;
            if installation != binding.installation_id
                || user != own.user_id
                || device != own.device_id
                || community != binding.community_id
                || group != *group_id
                || old_request_hash != request_hash
            {
                return Err(bot_send_conflict());
            }
            let current_raw = self
                .read(|c| {
                    super::schema::meta_get(c, &bot_send_attempt_key(operation_id, &current))
                })?
                .ok_or_else(|| {
                    ClientError::new(E_CORE_STORAGE, "bot send current attempt is missing")
                })?;
            let current_row = decode_bot_send_row(&current_raw)?;
            if current == *attempt_id {
                if current_row.request_hash != request_hash
                    || current_row.original_now != original_now
                    || current_row.generation != expected_generation
                {
                    return Err(bot_send_conflict());
                }
                if matches!(current_row.state, BOT_SEND_PENDING | BOT_SEND_ACCEPTED) {
                    return Ok(encode_bot_send_output(
                        &current_row.message,
                        current_row
                            .upload
                            .as_deref()
                            .ok_or_else(bot_send_conflict)?,
                    ));
                }
                if current_row.state != BOT_SEND_PREPARING {
                    return Err(bot_send_conflict());
                }
                BotSendAttempt {
                    predecessor: current_row.predecessor,
                    message: current_row.message,
                }
            } else {
                if current_row.state != BOT_SEND_CLOSED
                    || current_row.receipt.is_none()
                    || self
                        .read(|c| super::schema::meta_get(c, &attempt_key))?
                        .is_some()
                {
                    return Err(bot_send_conflict());
                }
                let message = self
                    .provider
                    .rand()
                    .random_array::<16>()
                    .map_err(|_| ClientError::new(E_CORE_MLS, "random failed"))?;
                BotSendAttempt {
                    predecessor: Some(current),
                    message,
                }
            }
        } else {
            if self
                .read(|c| super::schema::meta_get(c, &attempt_key))?
                .is_some()
            {
                return Err(bot_send_conflict());
            }
            let message = self
                .provider
                .rand()
                .random_array::<16>()
                .map_err(|_| ClientError::new(E_CORE_MLS, "random failed"))?;
            BotSendAttempt {
                predecessor: None,
                message,
            }
        };

        // Exact pending/accepted retries above return already encrypted bytes after
        // checking every durable operation binding. Only work that can prepare or
        // encrypt a new attempt requires current active-group membership.
        let scope = self
            .read(|c| group_row(c, group_id))?
            .ok_or_else(not_found)?;
        if (scope.kind != 0 && !(scope.kind == 3 && interaction_proof.is_some()))
            || scope.state != STATE_ACTIVE
            || scope.community_id.as_deref() != Some(binding.community_id.as_slice())
        {
            return Err(ClientError::new(
                E_CORE_STATE,
                "bot send group is outside its installation scope",
            ));
        }
        if let Some((snapshot_wire, expected)) = interaction_proof {
            if *group_id != *expected.group.as_bytes()
                || scope.community_id.as_deref() != Some(expected.community.as_bytes().as_slice())
            {
                return Err(bot_send_conflict());
            }
            let loaded = self
                .take_bot_interaction_group_verified(group_id, snapshot_wire, expected)?
                .ok_or_else(not_found)?;
            self.keep_group(*group_id, loaded);
        }

        let row = BotSendRow {
            state: BOT_SEND_PREPARING,
            predecessor: attempt.predecessor,
            request_hash,
            original_now,
            generation: expected_generation,
            message: attempt.message,
            epoch: None,
            class: None,
            ciphertext_hash: None,
            upload: None,
            receipt: None,
        };
        self.write(|_, u| {
            u.with_conn(|c| {
                if let Some(old) = super::schema::meta_get(c, &attempt_key)? {
                    let old =
                        decode_bot_send_row(&old).map_err(|e| StorageError::Codec(e.detail))?;
                    if old.state == BOT_SEND_PREPARING
                        && old.predecessor == row.predecessor
                        && old.request_hash == row.request_hash
                        && old.original_now == row.original_now
                        && old.generation == row.generation
                        && old.message == row.message
                    {
                        return Ok(());
                    }
                    return Err(StorageError::Codec(
                        "bot send attempt already exists".into(),
                    ));
                }
                let op_raw = super::schema::meta_get(c, &op_key)?;
                if let Some(raw) = op_raw {
                    let (_, _, _, _, _, old_hash, current) = decode_bot_send_operation(&raw)
                        .map_err(|e| StorageError::Codec(e.detail))?;
                    if old_hash != request_hash || attempt.predecessor != Some(current) {
                        return Err(StorageError::Codec("bot send operation changed".into()));
                    }
                } else if attempt.predecessor.is_some() {
                    return Err(StorageError::Codec(
                        "bot send predecessor is missing".into(),
                    ));
                }
                super::schema::meta_put(
                    c,
                    &op_key,
                    &encode_bot_send_operation(
                        &binding.installation_id,
                        &own.user_id,
                        &own.device_id,
                        &binding.community_id,
                        group_id,
                        &request_hash,
                        attempt_id,
                    ),
                )?;
                super::schema::meta_put(c, &attempt_key, &encode_bot_send_row(&row))?;
                Ok(())
            })?;
            Ok(())
        })?;

        let queued = self.read(|c| outbox_row(c, &attempt.message))?;
        if queued.is_none() {
            self.send_prepare_with_id(
                group_id,
                request_body,
                original_now,
                Some(attempt.message),
                interaction_proof.is_some(),
                None,
            )?;
        } else if queued
            .as_ref()
            .is_some_and(|o| o.group_id != *group_id || o.state != OUTBOX_QUEUED)
        {
            return Err(ClientError::new(
                E_CORE_STORAGE,
                "bot send outbox conflicts with its journal",
            ));
        }
        let encrypted = self.send_encrypt_inner(
            &attempt.message,
            Some(BotSendEncryptIntent {
                operation: *operation_id,
                attempt: *attempt_id,
                generation: expected_generation,
                message: attempt.message,
            }),
            interaction_proof,
        )?;
        let (encrypted_group, upload) = decode_strict(&encrypted, |d| {
            d.array(2)?;
            Ok((d.bytes_exact::<16>()?, d.bytes()?.to_vec()))
        })
        .map_err(|_| ClientError::new(E_CORE_STORAGE, "native send result is malformed"))?;
        if encrypted_group != *group_id {
            return Err(ClientError::new(
                E_CORE_STORAGE,
                "native send group changed",
            ));
        }
        Ok(encode_bot_send_output(&attempt.message, &upload))
    }

    /// Resolve a native Bot send attempt with the exact canonical receipt returned by the server.
    pub fn bot_send_receipt(
        &mut self,
        operation_id: &[u8; 16],
        receipt_body: &[u8],
    ) -> Result<(), ClientError> {
        if receipt_body.len() > 512 {
            return Err(ClientError::new(
                E_CORE_INPUT,
                "bot send receipt exceeds 512 bytes",
            ));
        }
        let receipt = decode_bot_send_receipt(receipt_body)?;
        if receipt.operation != *operation_id {
            return Err(ClientError::new(
                E_CORE_INPUT,
                "bot send receipt scope is malformed",
            ));
        }
        checked("generation", receipt.generation)?;
        checked("epoch", receipt.epoch)?;
        let binding = super::bot_intents::binding(self)?;
        let own = self.own()?;
        if receipt.community != binding.community_id {
            return Err(bot_send_conflict());
        }
        let op_key = bot_send_op_key(operation_id);
        let raw_op = self
            .read(|c| super::schema::meta_get(c, &op_key))?
            .ok_or_else(bot_send_conflict)?;
        let (installation, user, device, community, group, _, current) =
            decode_bot_send_operation(&raw_op)?;
        if installation != binding.installation_id
            || user != own.user_id
            || device != own.device_id
            || community != binding.community_id
            || group != receipt.group
        {
            return Err(bot_send_conflict());
        }
        let key = bot_send_attempt_key(operation_id, &receipt.attempt);
        let raw = self
            .read(|c| super::schema::meta_get(c, &key))?
            .ok_or_else(bot_send_conflict)?;
        let mut row = decode_bot_send_row(&raw)?;
        if row.predecessor != receipt.predecessor
            || row.generation != receipt.generation
            || row.epoch != Some(receipt.epoch)
            || row.class != Some(receipt.class)
            || row.ciphertext_hash != Some(receipt.ciphertext_hash)
            || row.upload.is_none()
        {
            return Err(bot_send_conflict());
        }
        if let Some(old) = row.receipt.as_deref() {
            if old == receipt_body {
                return Ok(());
            }
            return Err(bot_send_conflict());
        }
        if row.state != BOT_SEND_PENDING || (receipt.attempt != current && receipt.outcome == 1) {
            return Err(bot_send_conflict());
        }
        let message = row.message;
        if receipt.outcome == 1 {
            let response = receipt.response.as_deref().ok_or_else(bot_send_conflict)?;
            let decoded = wire::decode_send_response(response)?;
            let existing: Option<(Vec<u8>, i64, Vec<u8>, i64)> = self.read(|c| {
                c.query_row(
                    "SELECT group_id,seq,franking_tag,recv_ts FROM app_messages WHERE msg_id=?1 AND sender_device=?2 ORDER BY seq DESC LIMIT 1",
                    params![message.as_slice(), own.device_id.as_slice()],
                    |r| Ok((r.get(0)?, r.get(1)?, r.get(2)?, r.get(3)?)),
                ).optional().map_err(Into::into)
            })?;
            if let Some((stored_group, seq, tag, recv_ts)) = existing {
                if stored_group.as_slice() != group.as_slice()
                    || seq != decoded.seq as i64
                    || tag.as_slice() != decoded.franking_tag
                    || recv_ts != decoded.recv_ts as i64
                {
                    return Err(bot_send_conflict());
                }
            } else {
                self.send_confirm(&message, response)?;
            }
            row.state = BOT_SEND_ACCEPTED;
        } else {
            let exists: bool = self.read(|c| {
                c.query_row(
                "SELECT EXISTS(SELECT 1 FROM app_messages WHERE msg_id=?1 AND sender_device=?2)",
                params![message.as_slice(), own.device_id.as_slice()], |r| r.get(0),
            ).map_err(Into::into)
            })?;
            if exists {
                return Err(bot_send_conflict());
            }
            row.state = if receipt.outcome == 0 {
                BOT_SEND_CLOSED
            } else {
                BOT_SEND_UNKNOWN
            };
        }
        row.receipt = Some(receipt_body.to_vec());
        if receipt.outcome == 1 {
            self.write(|_, u| {
                u.with_conn(|c| super::schema::meta_put(c, &key, &encode_bot_send_row(&row)))?;
                Ok(())
            })?;
        } else {
            self.write(|_, u| {
                u.with_conn(|c| {
                    c.execute(
                        "DELETE FROM app_outbox WHERE msg_id=?1",
                        [message.as_slice()],
                    )?;
                    super::schema::meta_put(c, &key, &encode_bot_send_row(&row))?;
                    Ok(())
                })?;
                Ok(())
            })?;
        }
        Ok(())
    }

    /// `request` = [type uint 0..=6, reply_to b16|null, body tstr,
    ///              attachments [[blob_id b32, key b32, nonce b12, size uint, mime tstr, w uint|null, h uint|null, thumb bstr|null, name tstr]],
    ///              thread_id b16|null]
    /// (the L-CORE-30 attachment element, in order; `thread_id` since web3.7, and a 4-element request has none).
    /// Returns [msg_id b16] as before. Phase 2; the group must be in state 2.
    /// A `thread_id` is for type 0 only, only in a community channel group, and must name a held
    /// type-0 root that has no `thread_id` itself.
    /// Every refusal writes nothing (L-CORE-36).
    pub fn send_prepare(
        &mut self,
        id: &[u8; 16],
        request: &[u8],
        now: u64,
    ) -> Result<Vec<u8>, ClientError> {
        self.send_prepare_with_id(id, request, now, None, false, None)
    }

    /// Prepare a protocol-defined Bot interaction request for an authenticated human participant.
    /// `request_body` is the strict deterministic CBOR request tuple from protocol/10; the core
    /// binds its request and community ids to the supplied values, derives the application body,
    /// and uses the stable request id as the MLS message id for exact retries.
    pub fn human_interaction_send_prepare(
        &mut self,
        request_id: &[u8; 16],
        expected_community: &[u8; 16],
        group_id: &[u8; 16],
        request_body: &[u8],
        now: u64,
    ) -> Result<Vec<u8>, ClientError> {
        if request_id == &[0; 16] || expected_community == &[0; 16] || group_id == &[0; 16] {
            return Err(ClientError::new(
                E_CORE_INPUT,
                "human interaction ids must be nonzero",
            ));
        }
        checked("now", now)?;
        let body =
            encode_human_interaction_request(request_id, expected_community, request_body, now)?;
        let own = self.own()?;
        if own.kind != crate::identity::Kind::User.as_u8() {
            return Err(ClientError::new(
                E_CORE_STATE,
                "human identity required for interaction request",
            ));
        }
        let row = self
            .read(|c| group_row(c, group_id))?
            .ok_or_else(not_found)?;
        if row.kind != 3
            || row.state != STATE_ACTIVE
            || row.community_id.as_deref() != Some(expected_community.as_slice())
        {
            return Err(ClientError::new(
                E_CORE_STATE,
                "group is not an active interaction in the expected community",
            ));
        }
        let group = self.take_group(group_id)?.ok_or_else(not_found)?;
        let binding = match group.interaction_binding() {
            Ok(binding) => binding,
            Err(_) => {
                self.keep_group(*group_id, group);
                return Err(ClientError::new(
                    E_BINDING,
                    "interaction group binding rejected",
                ));
            }
        };
        if binding.community_id.as_bytes() != expected_community
            || binding.instance_id.as_bytes() != &own.instance_id
            || binding.participant_user_id != own.user_id
            || *binding.participant_device_id.as_bytes() != own.device_id
            || row.target_id.as_slice() != binding.bot_user_id.as_slice()
            || row.max_epoch < 0
            || row.max_epoch as u64 != group.epoch()
            || group.members().len() != 2
            || !interaction_roster_matches(&group, &own, &binding.bot_user_id)
        {
            self.keep_group(*group_id, group);
            return Err(ClientError::new(
                E_BINDING,
                "interaction group does not match the authenticated human device",
            ));
        }
        let stored_epoch: Result<i64, ClientError> = self.read(|c| {
            c.query_row(
                "SELECT epoch FROM app_groups WHERE group_id=?1",
                [group_id.as_slice()],
                |r| r.get(0),
            )
            .map_err(Into::into)
        });
        let stored_epoch = match stored_epoch {
            Ok(epoch) => epoch,
            Err(err) => {
                self.keep_group(*group_id, group);
                return Err(err);
            }
        };
        if stored_epoch < 0 || stored_epoch as u64 != group.epoch() {
            self.keep_group(*group_id, group);
            return Err(ClientError::new(
                E_CORE_STATE,
                "interaction group epoch is stale",
            ));
        }
        let blocked = self.read(|c| super::trust::send_block(c, &group, group_id));
        self.keep_group(*group_id, group);
        let blocked = blocked?;
        if let Some(code) = blocked {
            return Err(ClientError::new(code, ""));
        }

        let intent = HumanInteractionIntent {
            request_id: *request_id,
            community: *expected_community,
            group: *group_id,
            input_hash: Sha256::digest(request_body).into(),
        };
        let key = human_interaction_intent_key(request_id);
        if let Some(raw) = self.read(|c| super::schema::meta_get(c, &key))? {
            let old = decode_human_interaction_intent(&raw)?;
            if old.request_id != intent.request_id
                || old.community != intent.community
                || old.group != intent.group
                || old.input_hash != intent.input_hash
            {
                return Err(ClientError::new(
                    E_CORE_STATE,
                    "interaction request id conflicts with a prior request",
                ));
            }
            let mut out = Encoder::new();
            out.array(1).bytes(request_id);
            return Ok(out.into_vec());
        }

        // The operation id is also the durable outbox message id. An outbox row without its
        // matching intent is a collision with another send and must never be adopted.
        if let Some((stored_group, envelope)) = self.read(|c| {
            c.query_row(
                "SELECT group_id,envelope FROM app_outbox WHERE msg_id=?1",
                [request_id.as_slice()],
                |r| Ok((r.get::<_, Vec<u8>>(0)?, r.get::<_, Vec<u8>>(1)?)),
            )
            .optional()
            .map_err(Into::into)
        })? {
            let stored = Envelope::decode(&envelope).map_err(|_| {
                ClientError::new(E_CORE_STORAGE, "interaction outbox envelope is malformed")
            })?;
            if stored_group.as_slice() != group_id
                || stored.kind != EnvelopeType::Message
                || stored.body != body
                || !stored.attachments.is_empty()
                || stored.reply_to.is_some()
                || stored.thread_id.is_some()
            {
                return Err(ClientError::new(
                    E_CORE_STATE,
                    "interaction request id conflicts with an existing send",
                ));
            }
            let mut out = Encoder::new();
            out.array(1).bytes(request_id);
            return Ok(out.into_vec());
        }
        let mut send = Encoder::new();
        send.array(4).uint(0).null().text(&body).array(0);
        self.send_prepare_with_id(
            group_id,
            &send.into_vec(),
            now,
            Some(*request_id),
            true,
            Some(intent),
        )
    }

    fn send_prepare_with_id(
        &mut self,
        id: &[u8; 16],
        request: &[u8],
        now: u64,
        fixed_message_id: Option<[u8; 16]>,
        interaction: bool,
        human_intent: Option<HumanInteractionIntent>,
    ) -> Result<Vec<u8>, ClientError> {
        let own = self.own()?;
        let now = checked("now", now)?;
        let t = wire::decode_send_request(request)?;
        if t.ty > 6 {
            return Err(ClientError::new(E_CORE_INPUT, "type is out of range"));
        }
        if t.ty != 0 && !t.attachments.is_empty() {
            return Err(ClientError::new(
                E_CORE_INPUT,
                "only a message carries attachments",
            ));
        }
        if t.ty != 0 && t.thread_id.is_some() {
            return Err(ClientError::new(
                E_CORE_INPUT,
                "only a message starts or joins a thread",
            ));
        }
        // One read: the group row, then the target (the ledger's resolution, `fold::resolve`)
        // and the thread root.
        let (row, target, root) = self.read_one(|c| {
            let row = group_row(c, id)?;
            let target = match (&row, t.reply_to) {
                (Some(_), Some(reply_to)) => fold::resolve(c, id, &reply_to)?,
                _ => None,
            };
            let root = match (&row, t.thread_id) {
                (Some(_), Some(thread)) => Some(thread_root(c, id, &thread)?),
                _ => None,
            };
            Ok((row, target, root))
        })?;
        let row = row.ok_or_else(not_found)?;
        if row.kind != 0 && !(interaction && row.kind == 3) {
            // L-CORE-52: sending stays text-only; a call group carries no messages.
            return Err(ClientError::new(E_CORE_INPUT, "not a text group"));
        }
        if t.thread_id.is_some() && row.community_id.is_none() {
            // FX-core (final review I2): threads exist only in community channel groups.
            return Err(ClientError::new(
                E_CORE_INPUT,
                "threads are for channel groups only",
            ));
        }
        if row.state != STATE_ACTIVE {
            return Err(ClientError::new(
                E_CORE_STATE,
                format!("group state {}", row.state),
            ));
        }
        // A changed-key or failed leaf still in the group blocks queuing (and again at encryption).
        if let Some(code) = self.send_block_code(id)? {
            return Err(ClientError::new(code, ""));
        }
        let input = |detail: &'static str| Err(ClientError::new(E_CORE_INPUT, detail));
        let unheld = || ClientError::new(E_CORE_NOT_FOUND, "target is not held");
        if t.ty == 0 {
            if t.body.trim().is_empty() && t.attachments.is_empty() {
                return input("body is empty");
            }
            if t.reply_to.is_some() && target.is_none() {
                return Err(unheld());
            }
            match root {
                None | Some(ThreadRoot::Root) => {}
                Some(ThreadRoot::Unheld) => return Err(unheld()),
                Some(ThreadRoot::Reply) => return input("the thread root is a thread reply"),
                Some(ThreadRoot::NotAPost) => return input("the thread root is not a message"),
            }
        } else {
            if t.reply_to.is_none() {
                return input("reply_to is required");
            }
            let target = target.ok_or_else(unheld)?;
            if target.status != STATUS_OK {
                return Err(ClientError::new(E_CORE_STATE, "the target is deleted"));
            }
            if t.ty <= 2 && target.sender_user != Some(own.user_id) {
                return input("only the author may edit or delete");
            }
            match t.ty {
                1 if t.body.trim().is_empty() => return input("body is empty"),
                3 | 4 if t.body.is_empty() || t.body.len() > 32 => {
                    return input("emoji must be 1..=32 bytes");
                }
                2 | 5 | 6 if !t.body.is_empty() => return input("body must be empty"),
                _ => {}
            }
        }
        let msg_id = match fixed_message_id {
            Some(id) => id,
            None => self
                .provider
                .rand()
                .random_array::<16>()
                .map_err(|_| ClientError::new(E_CORE_MLS, "random failed"))?,
        };
        let k_f = self
            .provider
            .rand()
            .random_array::<32>()
            .map_err(|_| ClientError::new(E_CORE_MLS, "random failed"))?;
        let env = Envelope {
            v: 1,
            msg_id: MsgId::from_bytes(msg_id),
            kind: EnvelopeType::from_u64(t.ty)?,
            thread_id: t.thread_id.map(MsgId::from_bytes),
            reply_to: t.reply_to.map(MsgId::from_bytes),
            body: t.body,
            attachments: t.attachments,
            previews: vec![],
            k_f,
        };
        env.validate()?;
        let bytes = env.encode()?;
        self.write(|_, u| {
            u.with_conn(|c| {
                c.execute(
                    "INSERT INTO app_outbox(msg_id,group_id,envelope,created,state) \
                     VALUES(?1,?2,?3,?4,0)",
                    params![msg_id.as_slice(), id.as_slice(), bytes, now],
                )?;
                if let Some(intent) = human_intent {
                    if intent.request_id != msg_id || intent.group != *id {
                        return Err(StorageError::Codec(
                            "human interaction intent changed".into(),
                        ));
                    }
                    super::schema::meta_put(
                        c,
                        &human_interaction_intent_key(&intent.request_id),
                        &encode_human_interaction_intent(intent),
                    )?;
                }
                Ok(())
            })?;
            Ok(())
        })?;
        let mut e = Encoder::new();
        e.array(1).bytes(&msg_id);
        Ok(e.into_vec())
    }

    pub fn send_encrypt(&mut self, msg_id: &[u8; 16]) -> Result<Vec<u8>, ClientError> {
        self.send_encrypt_inner(msg_id, None, None)
    }

    fn send_encrypt_inner(
        &mut self,
        msg_id: &[u8; 16],
        bot_intent: Option<BotSendEncryptIntent>,
        interaction_proof: Option<(&[u8], &crate::bot::interaction_admission::Expected)>,
    ) -> Result<Vec<u8>, ClientError> {
        self.own()?;
        let o = self
            .read(|c| outbox_row(c, msg_id))?
            .ok_or_else(not_found)?;
        if o.state != OUTBOX_QUEUED {
            return Err(outbox_state(o.state));
        }
        let id = o.group_id;
        let r = self.read(|c| group_row(c, &id))?.ok_or_else(not_found)?;
        if r.state != STATE_ACTIVE {
            return Err(ClientError::new(
                E_CORE_STATE,
                format!("group state {}", r.state),
            ));
        }
        let (in_flight, proposals): (i64, i64) = self.read(|c| {
            Ok((
                c.query_row(
                    "SELECT COUNT(*) FROM app_outbox WHERE group_id=?1 AND state=1",
                    [id.as_slice()],
                    |r| r.get(0),
                )?,
                c.query_row(
                    "SELECT COUNT(*) FROM app_proposals WHERE group_id=?1",
                    [id.as_slice()],
                    |r| r.get(0),
                )?,
            ))
        })?;
        if in_flight > 0 {
            return Err(ClientError::new(
                E_CORE_STATE,
                "a message of this group is in flight",
            ));
        }
        if proposals > 0 {
            return Err(ClientError::new(E_CORE_STATE, "proposals are pending"));
        }
        self.retry_reload(&id, |this| {
            let mut group = if let Some((snapshot_wire, expected)) = interaction_proof {
                this.take_bot_interaction_group_verified(&id, snapshot_wire, expected)?
            } else {
                this.take_group(&id)?
            }
            .ok_or_else(not_found)?;
            if group.has_pending_commit() {
                this.keep_group(id, group);
                return Err(ClientError::new(E_CORE_STATE, "a commit is pending"));
            }
            // The gate holds where confidentiality is decided: at encryption to the current
            // tree, not only when the row was queued before a key change arrived.
            match this.read(|c| super::trust::send_block(c, &group, &id)) {
                Ok(None) => {}
                Ok(Some(code)) => {
                    this.keep_group(id, group);
                    return Err(ClientError::new(code, ""));
                }
                Err(e) => {
                    this.keep_group(id, group);
                    return Err(e);
                }
            }
            let env = Envelope::decode(&o.envelope)
                .map_err(|_| ClientError::new(E_CORE_STORAGE, "outbox envelope does not decode"))?;
            let result = this.write(|ctx, u| {
                let epoch = group.epoch();
                let message = group.create_message(
                    ctx.provider,
                    ctx.signer
                        .ok_or_else(|| ClientError::new(super::error::E_CORE_NO_IDENTITY, ""))?,
                    &env,
                )?;
                // web3.7: the upload class, 0 for a post and 1 for every other type; the host
                // enforces send and slow mode on class 0 and relays it to every reader.
                let class = if env.kind == EnvelopeType::Message {
                    0
                } else {
                    1
                };
                let mut body = Encoder::new();
                body.array(3)
                    .uint(epoch)
                    .bytes(&wire::tls(&message)?)
                    .uint(class);
                let body = body.into_vec();
                let stored_epoch = checked("epoch", epoch)?;
                if let Some(intent) = bot_intent.as_ref() {
                    if intent.message != *msg_id || intent.generation > i64::MAX as u64 {
                        return Err(bot_send_conflict());
                    }
                    let (journal_epoch, hash, stored_class) = bot_send_upload_fields(&body)?;
                    u.with_conn(|c| {
                        c.execute(
                            "UPDATE app_outbox SET state=1,epoch=?2 WHERE msg_id=?1",
                            params![msg_id.as_slice(), stored_epoch],
                        )?;
                        let key = bot_send_attempt_key(&intent.operation, &intent.attempt);
                        let raw = super::schema::meta_get(c, &key)?.ok_or_else(|| {
                            StorageError::Codec("bot send attempt disappeared".into())
                        })?;
                        let mut row =
                            decode_bot_send_row(&raw).map_err(|e| StorageError::Codec(e.detail))?;
                        if row.state != BOT_SEND_PREPARING
                            || row.message != *msg_id
                            || row.generation != intent.generation
                            || journal_epoch != stored_epoch as u64
                            || stored_class != class
                        {
                            return Err(StorageError::Codec("bot send attempt changed".into()));
                        }
                        row.state = BOT_SEND_PENDING;
                        row.epoch = Some(journal_epoch);
                        row.class = Some(stored_class);
                        row.ciphertext_hash = Some(hash);
                        row.upload = Some(body.clone());
                        super::schema::meta_put(c, &key, &encode_bot_send_row(&row))?;
                        Ok(())
                    })?;
                } else {
                    u.with_conn(|c| {
                        c.execute(
                            "UPDATE app_outbox SET state=1,epoch=?2 WHERE msg_id=?1",
                            params![msg_id.as_slice(), stored_epoch],
                        )?;
                        Ok(())
                    })?;
                }
                Ok(body)
            });
            if result.is_ok() {
                this.keep_group(id, group);
            }
            let body = result?;
            let mut e = Encoder::new();
            e.array(2).bytes(&id).bytes(&body);
            Ok(e.into_vec())
        })
    }

    pub fn send_confirm(
        &mut self,
        msg_id: &[u8; 16],
        response: &[u8],
    ) -> Result<Vec<u8>, ClientError> {
        let own = self.own()?;
        let response = wire::decode_send_response(response)?;
        checked("seq", response.seq)?;
        checked("recv_ts", response.recv_ts)?;
        if response.seq == 0 {
            return Err(ClientError::new(
                E_CORE_INPUT,
                "send confirmation sequence must be positive",
            ));
        }
        let o = self.read(|c| outbox_row(c, msg_id))?;
        let (id, seq) = if let Some(o) = o {
            if o.state != OUTBOX_IN_FLIGHT {
                return Err(outbox_state(o.state));
            }
            let id = o.group_id;
            let occupant: Option<(i64, Vec<u8>, Option<Vec<u8>>)> = self.read(|c| {
                c.query_row(
                    "SELECT status,sender_device,msg_id FROM app_messages \
                     WHERE group_id=?1 AND seq=?2",
                    params![id.as_slice(), response.seq as i64],
                    |r| Ok((r.get(0)?, r.get(1)?, r.get(2)?)),
                )
                .optional()
                .map_err(Into::into)
            })?;
            if let Some((status, device, stored_msg)) = occupant {
                // The deleted marker of this device's upload at that seq (the echo of the deletion
                // came first): the server stored the message, so the row is done, never resent.
                let own_deleted = status == STATUS_DELETED
                    && device == own.device_id
                    && stored_msg.as_deref().is_none_or(|m| m == msg_id);
                self.write(|_, u| {
                    u.with_conn(|c| {
                        if own_deleted {
                            c.execute(
                                "UPDATE app_messages SET msg_id=?3 \
                                 WHERE group_id=?1 AND seq=?2 AND msg_id IS NULL",
                                params![id.as_slice(), response.seq as i64, msg_id.as_slice()],
                            )?;
                            c.execute(
                                "DELETE FROM app_outbox WHERE msg_id=?1",
                                [msg_id.as_slice()],
                            )?;
                        } else {
                            // Another row holds the seq the server names for this upload: the
                            // answer contradicts what is stored. The row leaves flight as failed,
                            // so the group sends on and nothing is resent without the person.
                            c.execute(
                                "UPDATE app_outbox SET state=2,error=?2 WHERE msg_id=?1",
                                params![msg_id.as_slice(), E_CORE_STATE],
                            )?;
                        }
                        Ok(())
                    })?;
                    Ok(())
                })?;
                if !own_deleted {
                    return Err(ClientError::new(E_CORE_STATE, "message seq exists"));
                }
                let mut e = Encoder::new();
                e.array(2).bytes(&id).uint(response.seq);
                return Ok(e.into_vec());
            }
            // A stored group that does not match its row has no leaf of this row: NULL, as when
            // no MLS group is stored.
            let mut group = match self.take_group(&id) {
                Err(e) if e.code == E_CORE_STATE => None,
                other => other?,
            };
            let leaf = group.as_ref().map(|g| g.own_leaf_index().u32());
            let env = Envelope::decode(&o.envelope)
                .map_err(|_| ClientError::new(E_CORE_STORAGE, "outbox envelope does not decode"))?;
            let result = self.write(|_, u| {
                u.with_conn(|c| {
                    insert_message(
                        c,
                        &own_message(
                            &own,
                            &id,
                            response.seq,
                            o.epoch as u64,
                            response.recv_ts,
                            &response.franking_tag,
                            &env,
                            &o.envelope,
                            leaf,
                        ),
                    )?;
                    refold_insert(
                        c,
                        &id,
                        response.seq,
                        &env,
                        &own.user_id,
                        &own.device_id,
                        Some(&own),
                    )?;
                    c.execute(
                        "DELETE FROM app_outbox WHERE msg_id=?1",
                        [msg_id.as_slice()],
                    )?;
                    Ok(())
                })?;
                Ok(())
            });
            if result.is_ok()
                && let Some(g) = group.take()
            {
                self.keep_group(id, g);
            }
            result?;
            (id, response.seq)
        } else {
            self.read(|c| {
                c.query_row(
                    "SELECT group_id,seq FROM app_messages \
                     WHERE msg_id=?1 AND sender_device=?2 ORDER BY seq DESC LIMIT 1",
                    params![msg_id.as_slice(), own.device_id.as_slice()],
                    |r| Ok((r.get(0)?, r.get::<_, i64>(1)? as u64)),
                )
                .optional()
                .map_err(Into::into)
            })?
            .ok_or_else(not_found)?
        };
        let mut e = Encoder::new();
        e.array(2).bytes(&id).uint(seq);
        Ok(e.into_vec())
    }

    fn outbox_transition(
        &mut self,
        id: &[u8; 16],
        from: &[i64],
        to: Option<(i64, &str)>,
    ) -> Result<(), ClientError> {
        self.own()?;
        let row = self.read(|c| outbox_row(c, id))?.ok_or_else(not_found)?;
        if !from.contains(&row.state) {
            return Err(outbox_state(row.state));
        }
        self.write(|_, u| {
            u.with_conn(|c| {
                if let Some((state, error)) = to {
                    c.execute(
                        "UPDATE app_outbox SET state=?2,error=?3 WHERE msg_id=?1",
                        params![id.as_slice(), state, error],
                    )?;
                } else {
                    c.execute("DELETE FROM app_outbox WHERE msg_id=?1", [id.as_slice()])?;
                }
                Ok(())
            })?;
            Ok(())
        })
    }
    pub fn send_requeue(&mut self, id: &[u8; 16]) -> Result<(), ClientError> {
        self.outbox_transition(id, &[OUTBOX_IN_FLIGHT], Some((OUTBOX_QUEUED, "")))
    }
    pub fn send_fail(&mut self, id: &[u8; 16], error: &str) -> Result<(), ClientError> {
        self.own()?;
        if error.len() > 64 {
            return Err(ClientError::new(E_CORE_INPUT, "error exceeds 64 bytes"));
        }
        self.outbox_transition(
            id,
            &[OUTBOX_QUEUED, OUTBOX_IN_FLIGHT],
            Some((OUTBOX_FAILED, error)),
        )
    }
    pub fn send_retry(&mut self, id: &[u8; 16]) -> Result<(), ClientError> {
        self.outbox_transition(id, &[OUTBOX_FAILED], Some((OUTBOX_QUEUED, "")))
    }
    pub fn send_discard(&mut self, id: &[u8; 16]) -> Result<(), ClientError> {
        self.outbox_transition(id, &[OUTBOX_QUEUED, OUTBOX_FAILED], None)
    }

    /// [[msg_id b16, state uint, error tstr, created uint, body tstr, type uint, reply_to b16|null,
    /// attachments [[blob_id b32, size uint, mime tstr, name tstr]]]] (L-CORE-37): no key, nonce or
    /// thumbnail leaves the outbox.
    pub fn outbox(&self, id: &[u8; 16]) -> Result<Vec<u8>, ClientError> {
        type ListedOutbox = ([u8; 16], i64, String, i64, Vec<u8>);
        let rows: Vec<ListedOutbox> = self
            .read_one(|c| {
                if group_row(c, id)?.is_none() {
                    return Ok(None);
                }
                let mut s = c.prepare(
                    "SELECT msg_id,state,error,created,envelope FROM app_outbox \
                     WHERE group_id=?1 ORDER BY created,msg_id",
                )?;
                let it = s.query_map([id.as_slice()], |r| {
                    Ok((r.get(0)?, r.get(1)?, r.get(2)?, r.get(3)?, r.get(4)?))
                })?;
                it.collect::<Result<_, _>>().map(Some).map_err(Into::into)
            })?
            .ok_or_else(not_found)?;
        let mut e = Encoder::new();
        e.array(rows.len());
        for (msg, state, error, created, env) in rows {
            let env = Envelope::decode(&env)
                .map_err(|_| ClientError::new(E_CORE_STORAGE, "outbox envelope does not decode"))?;
            e.array(8)
                .bytes(&msg)
                .uint(state as u64)
                .text(&error)
                .uint(created as u64)
                .text(&env.body)
                .uint(u64::from(env.kind.as_u8()))
                .opt_bytes(env.reply_to.as_ref().map(|m| &m.0[..]))
                .array(env.attachments.len());
            for a in &env.attachments {
                e.array(4)
                    .bytes(&a.blob_id)
                    .uint(a.size)
                    .text(&a.mime)
                    .text(&a.name);
            }
        }
        Ok(e.into_vec())
    }
    /// Phase 2. The attachment element (L-CORE-30, nine elements, key and nonce included) at
    /// `index` of the envelope of the status-0 type-0 row at (group_id, seq). E_CORE_NOT_FOUND
    /// "no such attachment" for any other row or an index past the list (L-CORE-38).
    pub fn attachment_get(
        &self,
        group_id: &[u8; 16],
        seq: u64,
        index: u32,
    ) -> Result<Vec<u8>, ClientError> {
        self.own()?;
        let seq = checked("seq", seq)?;
        let bytes: Option<Option<Vec<u8>>> = self.read_one(|c| {
            c.query_row(
                "SELECT envelope FROM app_messages \
                 WHERE group_id = ?1 AND seq = ?2 AND status = 0 AND type = 0",
                params![group_id.as_slice(), seq],
                |r| r.get(0),
            )
            .optional()
            .map_err(Into::into)
        })?;
        let env = Envelope::decode(&bytes.flatten().ok_or_else(no_attachment)?).map_err(|_| {
            ClientError::new(E_CORE_STORAGE, "app_messages envelope does not decode")
        })?;
        let a = env
            .attachments
            .get(index as usize)
            .ok_or_else(no_attachment)?;
        Ok(wire::encode_attachment(a))
    }

    /// Phase 2. [[group_id b16, seq uint, channel_id b16, blob_ids [b32]]] ordered by group_id,
    /// seq (L-CORE-38).
    pub fn purges(&self) -> Result<Vec<u8>, ClientError> {
        self.own()?;
        type RawPurge = (Vec<u8>, i64, Vec<u8>, Vec<u8>);
        let rows: Vec<RawPurge> = self.read_one(|c| {
            let mut s = c.prepare(
                "SELECT group_id, seq, channel_id, blob_ids FROM app_purges ORDER BY group_id, seq",
            )?;
            s.query_map([], |r| Ok((r.get(0)?, r.get(1)?, r.get(2)?, r.get(3)?)))?
                .collect::<Result<_, _>>()
                .map_err(Into::into)
        })?;
        let mut e = Encoder::new();
        e.array(rows.len());
        for (group_id, seq, channel_id, blob_ids) in rows {
            let ids = wire::decode_blob_ids(&blob_ids)?;
            e.array(4)
                .bytes(&group_id)
                .uint(seq as u64)
                .bytes(&channel_id)
                .array(ids.len());
            for id in ids {
                e.bytes(&id);
            }
        }
        Ok(e.into_vec())
    }

    /// Phase 2. Deletes the app_purges row in one unit; no error when it is absent (L-CORE-38).
    pub fn purge_done(&mut self, group_id: &[u8; 16], seq: u64) -> Result<(), ClientError> {
        self.own()?;
        let seq = checked("seq", seq)?;
        self.write_dyn(&mut |c| {
            c.execute(
                "DELETE FROM app_purges WHERE group_id = ?1 AND seq = ?2",
                params![group_id.as_slice(), seq],
            )?;
            Ok(())
        })
    }
    pub fn timeline(
        &self,
        id: &[u8; 16],
        before_seq: u64,
        limit: u32,
    ) -> Result<Vec<u8>, ClientError> {
        let own = self.own()?;
        let before = page_bound(before_seq, limit)?;
        self.read(|c| group_row(c, id))?.ok_or_else(not_found)?;
        let rows = self.read_one(|c| {
            let mut views = timeline_views(
                c,
                &timeline_sql(),
                params![id.as_slice(), before, i64::from(limit)],
                id,
                &own.user_id,
            )?;
            views.reverse();
            Ok(views)
        })?;
        Ok(wire::encode_timeline(&rows))
    }

    /// web3.7 (R25): `[root_row|null, [row…]]`. The root row is the held type-0 root of `root`
    /// (one without a `thread_id`), else null; the rows are the type-0 rows whose `thread_id` is
    /// `root`, newest first, below `before_seq` (0 = from the newest), at most `limit` (1..=200).
    /// Both in the 19-element timeline shape.
    pub fn thread_timeline(
        &self,
        id: &[u8; 16],
        root: &[u8; 16],
        before_seq: u64,
        limit: u32,
    ) -> Result<Vec<u8>, ClientError> {
        let own = self.own()?;
        let before = page_bound(before_seq, limit)?;
        self.read(|c| group_row(c, id))?.ok_or_else(not_found)?;
        let (head, rows) = self.read_one(|c| {
            let head = timeline_views(
                c,
                &thread_root_sql(),
                params![id.as_slice(), root.as_slice()],
                id,
                &own.user_id,
            )?
            .pop();
            // Only the replies after the root are in its thread (fix round 1, I1).
            let after = head.as_ref().map_or(0, |h| h.seq as i64);
            let rows = timeline_views(
                c,
                &thread_sql(),
                params![
                    id.as_slice(),
                    root.as_slice(),
                    after,
                    before,
                    i64::from(limit)
                ],
                id,
                &own.user_id,
            )?;
            Ok((head, rows))
        })?;
        Ok(wire::encode_thread_timeline(head.as_ref(), &rows))
    }

    /// web3.7: the group's threads, `[[root_msg_id, root_seq, count, last_ts, excerpt]]`, at most
    /// 50, the latest reply first (`THREADS_SQL`). Fix round 1 (M1): a thread is listed under its
    /// held root only, so `root_seq` is always set; `excerpt` is `fold::excerpt` of the root's shown
    /// body, `''` for a deleted root.
    pub fn threads(&self, id: &[u8; 16]) -> Result<Vec<u8>, ClientError> {
        self.own()?;
        self.read(|c| group_row(c, id))?.ok_or_else(not_found)?;
        let rows = self.read_one(|c| {
            let mut q = c.prepare(THREADS_SQL)?;
            let threads: Vec<([u8; 16], i64, i64, String, i64, i64)> = q
                .query_map([id.as_slice()], |r| {
                    Ok((
                        r.get(0)?,
                        r.get(1)?,
                        r.get(2)?,
                        r.get(3)?,
                        r.get(4)?,
                        r.get(5)?,
                    ))
                })?
                .collect::<Result<_, _>>()?;
            Ok(threads
                .into_iter()
                .map(
                    |(root, seq, status, body, count, last_ts)| wire::ThreadView {
                        root,
                        root_seq: Some(seq as u64),
                        count: count as u64,
                        last_ts: last_ts as u64,
                        excerpt: if status == STATUS_OK {
                            fold::excerpt(&body)
                        } else {
                            String::new()
                        },
                    },
                )
                .collect::<Vec<_>>())
        })?;
        Ok(wire::encode_threads(&rows))
    }

    pub fn pins(&self, id: &[u8; 16]) -> Result<Vec<u8>, ClientError> {
        self.own()?;
        let rows = self
            .read_one(|c| {
                if group_row(c, id)?.is_none() {
                    return Ok(None);
                }
                let mut q = c.prepare(
                "SELECT target, seq, by_user FROM app_pins WHERE group_id = ?1 ORDER BY seq DESC",
            )?;
                let pins: Vec<([u8; 16], i64, [u8; 16])> = q
                    .query_map([id.as_slice()], |r| Ok((r.get(0)?, r.get(1)?, r.get(2)?)))?
                    .collect::<Result<_, _>>()?;
                let mut views = Vec::new();
                for (msg_id, seq, by_user) in pins {
                    if let Some(t) = fold::resolve(c, id, &msg_id)? {
                        if super::removals::is_tombstoned(c, id, t.seq)? {
                            continue;
                        }
                        let ts: i64 = c.query_row(
                            "SELECT recv_ts FROM app_messages WHERE group_id = ?1 AND seq = ?2",
                            params![id.as_slice(), t.seq as i64],
                            |r| r.get(0),
                        )?;
                        views.push(wire::PinView {
                            target_seq: t.seq,
                            msg_id,
                            pinned_seq: seq as u64,
                            by_user,
                            author: t.sender_user,
                            excerpt: fold::excerpt(&t.shown_body),
                            target_ts: ts as u64,
                            thread_root: shown_thread_root(c, id, t.seq)?,
                        });
                    }
                }
                Ok(Some(views))
            })?
            .ok_or_else(not_found)?;
        Ok(wire::encode_pins(&rows))
    }

    pub fn own_roles_set(
        &mut self,
        community_id: &[u8; 16],
        role_ids: &[u8],
    ) -> Result<(), ClientError> {
        self.own()?;
        if !role_ids.len().is_multiple_of(16) || role_ids.len() > 1024 {
            return Err(ClientError::new(
                E_CORE_INPUT,
                "role_ids must be 0..=64 ids of 16 bytes",
            ));
        }
        self.write_dyn(&mut |c| {
            c.execute(
                "DELETE FROM app_roles WHERE community_id = ?1",
                [community_id.as_slice()],
            )?;
            for id in role_ids.as_chunks::<16>().0 {
                c.execute(
                    "INSERT OR IGNORE INTO app_roles (community_id, role_id) VALUES (?1, ?2)",
                    params![community_id.as_slice(), id],
                )?;
            }
            Ok(())
        })
    }
}

impl ClientCore {
    /// Replaces `group_id`'s role view (web3.5 R1): `pinners` and `broadcasters` are 0..=4096
    /// concatenated 16-byte user ids the host-served view allows `pin_messages` (bit 3) and
    /// `mention_everyone` (bit 18) in the group's channel now; `mentionable` is 0..=4096
    /// concatenated 16-byte ids of the server's roles marked "anyone can mention" (W2a review
    /// finding 1). A view equal to the stored one writes nothing. Otherwise, in one unit, it
    /// replaces the rows, marks the view known, refolds the targets with a 5/6 from a user whose
    /// pin bit changed, and recomputes the mention flag of other users' readable messages naming
    /// a `<@…>` token from a sender whose broadcast bit changed (from every sender when the
    /// mentionable set changed); a group's first view refolds and recomputes all of them. A DM
    /// group has no roles: `E_CORE_STATE`, nothing written.
    ///
    /// The view is the host's claim, not something MLS authenticates: it decides only which pins
    /// and broadcast or role mentions this device counts, never which messages it shows.
    pub fn authority_set(
        &mut self,
        group_id: &[u8; 16],
        pinners: &[u8],
        broadcasters: &[u8],
        mentionable: &[u8],
    ) -> Result<(), ClientError> {
        let own = self.own()?;
        let fits = |ids: &[u8]| ids.len().is_multiple_of(16) && ids.len() / 16 <= AUTHORITY_MAX_IDS;
        if !fits(pinners) || !fits(broadcasters) || !fits(mentionable) {
            return Err(ClientError::new(
                E_CORE_INPUT,
                "pinners, broadcasters and mentionable must be 0..=4096 ids of 16 bytes",
            ));
        }
        let g = group_id.as_slice();
        let ids_of = |ids: &[u8]| sorted_ids(ids.as_chunks::<16>().0.to_vec());
        let (new_p, new_b, new_m) = (ids_of(pinners), ids_of(broadcasters), ids_of(mentionable));
        let (dm, known, old_p, old_b, old_m) = self.read_one(|c| {
            let dm: bool = c
                .query_row(
                    "SELECT community_id IS NULL FROM app_groups WHERE group_id = ?1",
                    [g],
                    |r| r.get(0),
                )
                .optional()?
                .unwrap_or(false);
            let known = c
                .query_row(
                    "SELECT 1 FROM app_authority_known WHERE group_id = ?1",
                    [g],
                    |_| Ok(()),
                )
                .optional()?
                .is_some();
            let (mut p, mut b) = (Vec::new(), Vec::new());
            let mut q = c.prepare("SELECT bit, user_id FROM app_authority WHERE group_id = ?1")?;
            let rows = q.query_map([g], |r| Ok((r.get::<_, i64>(0)?, r.get::<_, [u8; 16]>(1)?)))?;
            for row in rows {
                let (bit, user) = row?;
                if bit == 3 { &mut p } else { &mut b }.push(user);
            }
            Ok((
                dm,
                known,
                sorted_ids(p),
                sorted_ids(b),
                mentionable_of(c, group_id)?,
            ))
        })?;
        if dm {
            return Err(ClientError::new(
                E_CORE_STATE,
                "a DM group has no role view",
            ));
        }
        if known && old_p == new_p && old_b == new_b && old_m == new_m {
            return Ok(());
        }
        // A first view changes what counts for every sender; a later one only for the users whose
        // bit changed, or for every sender when the mentionable roles changed.
        let all = !known;
        let pin_changed = changed_ids(&old_p, &new_p);
        let broadcast_changed = changed_ids(&old_b, &new_b);
        let every_sender = all || old_m != new_m;
        self.write_dyn(&mut |c| {
            c.execute("DELETE FROM app_authority WHERE group_id = ?1", [g])?;
            c.execute(
                "DELETE FROM app_authority_mentionable WHERE group_id = ?1",
                [g],
            )?;
            {
                let mut insert = c.prepare_cached(
                    "INSERT OR IGNORE INTO app_authority (group_id, bit, user_id) VALUES (?1, ?2, ?3)",
                )?;
                for (bit, ids) in [(3i64, &new_p), (18, &new_b)] {
                    for id in ids {
                        insert.execute(params![g, bit, id.as_slice()])?;
                    }
                }
                let mut insert = c.prepare_cached(
                    "INSERT OR IGNORE INTO app_authority_mentionable (group_id, role_id) VALUES (?1, ?2)",
                )?;
                for id in &new_m {
                    insert.execute(params![g, id.as_slice()])?;
                }
            }
            // The core keeps no clock of its own (every call that needs the time is handed it).
            c.execute(
                "INSERT INTO app_authority_known (group_id, set_at) VALUES (?1, 0) \
                 ON CONFLICT (group_id) DO UPDATE SET set_at = excluded.set_at",
                [g],
            )?;
            if all || !pin_changed.is_empty() {
                let folds: Vec<(Vec<u8>, Option<Vec<u8>>)> = {
                    let mut q = c.prepare(
                        "SELECT DISTINCT reply_to, sender_user FROM app_messages WHERE group_id = ?1 AND type IN (5, 6) AND reply_to IS NOT NULL",
                    )?;
                    q.query_map([g], |r| Ok((r.get(0)?, r.get(1)?)))?
                        .collect::<Result<_, _>>()?
                };
                let targets = sorted_ids(
                    folds
                        .into_iter()
                        .filter(|(_, sender)| {
                            all || sender
                                .as_deref()
                                .and_then(|s| <[u8; 16]>::try_from(s).ok())
                                .is_some_and(|s| pin_changed.binary_search(&s).is_ok())
                        })
                        .filter_map(|(target, _)| <[u8; 16]>::try_from(target.as_slice()).ok())
                        .collect(),
                );
                for target in targets {
                    fold::refold(c, group_id, &target, &fold::FoldScope::Pin, None, Some(&own))?;
                }
            }
            if !every_sender && broadcast_changed.is_empty() {
                return Ok(());
            }
            let roles = own_roles_of(c, group_id)?;
            // Every gated token (@everyone, @here, a role) starts "<@"; a direct mention of me
            // reads the same under any view, so recomputing it changes nothing.
            let rows: Vec<(i64, [u8; 16], String, i64)> = {
                let mut q = c.prepare(
                    "SELECT seq, sender_user, body, mention FROM app_messages WHERE group_id = ?1 \
                     AND status = 0 AND type = 0 AND sender_user IS NOT NULL AND sender_user <> ?2 \
                     AND instr(body, '<@') > 0",
                )?;
                q.query_map(params![g, own.user_id.as_slice()], |r| {
                    Ok((r.get(0)?, r.get(1)?, r.get(2)?, r.get(3)?))
                })?
                .collect::<Result<_, _>>()?
            };
            for (seq, sender, body, was) in rows {
                if !every_sender && broadcast_changed.binary_search(&sender).is_err() {
                    continue;
                }
                let ok = broadcast_ok(c, group_id, &sender)?;
                let now = i64::from(mentions_me_gated(&body, &own.user_id, &roles, &new_m, ok));
                if now != was {
                    c.execute(
                        "UPDATE app_messages SET mention = ?3 WHERE group_id = ?1 AND seq = ?2",
                        params![g, seq, now],
                    )?;
                }
            }
            Ok(())
        })
    }
}

#[cfg(test)]
mod mention_tests {
    use super::mentions_me;

    const ME: [u8; 16] = [0xa1; 16];

    #[test]
    fn the_readable_mention_syntax_is_matched_literally() {
        let me = "a1".repeat(16);
        for (body, want) in [
            (format!("hi <@{me}>"), true),
            (format!("<@{me}>"), true),
            ("<@everyone> standup".to_owned(), true),
            ("x<@here>y".to_owned(), true),
            (format!("hi <@{}>", "A1".repeat(16)), false),
            (format!("hi <@{}>", "b2".repeat(16)), false),
            (format!("hi <@{me}"), false),
            (format!("hi @{me}"), false),
            ("<@Everyone>".to_owned(), false),
            ("<@everyone >".to_owned(), false),
            (String::new(), false),
        ] {
            assert_eq!(mentions_me(&body, &ME), want, "{body:?}");
        }
    }

    #[test]
    fn a_broadcast_counts_only_when_the_sender_may_send_one() {
        use super::mentions_me_gated;
        let me = "a1".repeat(16);
        let role = [0x3c; 16];
        for (body, ok, want) in [
            ("<@everyone> standup".to_owned(), true, true),
            ("<@everyone> standup".to_owned(), false, false),
            ("x<@here>y".to_owned(), true, true),
            ("x<@here>y".to_owned(), false, false),
            (format!("<@everyone> <@{me}>"), false, true),
            (format!("<@here> <@{}>", "3c".repeat(16)), false, true),
            ("<@Everyone>".to_owned(), true, false),
            (String::new(), true, false),
        ] {
            assert_eq!(
                mentions_me_gated(&body, &ME, &[role], &[role], ok),
                want,
                "{body:?} {ok}"
            );
        }
    }

    #[test]
    fn a_role_token_counts_only_if_mentionable_or_from_a_broadcaster() {
        use super::mentions_me_gated;
        let (held, other, unheld) = ([0x3c; 16], [0x3d; 16], [0x3e; 16]);
        let token = |c: &str| format!("ops <@{}>", c.repeat(16));
        for (body, mentionable, ok, want) in [
            (token("3c"), vec![], false, false),
            (token("3c"), vec![], true, true),
            (token("3c"), vec![held], false, true),
            (token("3d"), vec![held], false, false),
            (token("3d"), vec![held], true, true),
            (token("3e"), vec![unheld], false, false),
            (token("3e"), vec![unheld], true, false),
        ] {
            assert_eq!(
                mentions_me_gated(&body, &ME, &[held, other], &mentionable, ok),
                want,
                "{body:?} {mentionable:?} {ok}"
            );
        }
    }
}

#[cfg(all(test, not(target_arch = "wasm32")))]
mod bot_send_intent_tests {
    use super::*;
    use crate::{cbor::Encoder, mls::ConnHandle};
    use std::sync::{Arc, Mutex};

    const INSTANCE: [u8; 16] = [0x11; 16];
    const COMMUNITY: [u8; 16] = [0x22; 16];
    const INSTALLATION: [u8; 16] = [0x33; 16];
    const USER: [u8; 16] = [0x44; 16];
    const GROUP: [u8; 16] = [0x55; 16];
    const CHANNEL: [u8; 16] = [0x66; 16];
    const CREATE_OP: [u8; 16] = [0x77; 16];
    const OPERATION: [u8; 16] = [0x88; 16];
    const ATTEMPT: [u8; 16] = [0x99; 16];

    fn connection() -> ConnHandle {
        Arc::new(Mutex::new(
            rusqlite::Connection::open_in_memory().expect("sqlite"),
        ))
    }

    fn bot(conn: ConnHandle) -> ClientCore {
        let mut core = ClientCore::open(conn).expect("open core");
        core.bot_bootstrap_begin(&INSTANCE, &COMMUNITY, &INSTALLATION, &[0x77; 32])
            .expect("bootstrap");
        core.bot_bootstrap_complete(&USER, "send bot", 1_800_000_000)
            .expect("enroll");
        core.bot_group_create(&CREATE_OP, &GROUP, &CHANNEL, 0, 0, &[0x88; 32])
            .expect("create group");
        core.bot_group_registered(&CREATE_OP, &GROUP, 1)
            .expect("register group");
        core
    }

    fn request() -> Vec<u8> {
        let mut e = Encoder::new();
        e.array(4)
            .uint(0)
            .null()
            .text("persist exactly once")
            .array(0);
        e.into_vec()
    }

    fn upload_from_output(output: &[u8]) -> ([u8; 16], Vec<u8>) {
        decode_strict(output, |d| {
            d.array(2)?;
            Ok((d.bytes_exact()?, d.bytes()?.to_vec()))
        })
        .expect("prepare output")
    }

    fn receipt(attempt: [u8; 16], upload: &[u8], outcome: u64, response: Option<&[u8]>) -> Vec<u8> {
        let (epoch, hash, class) = bot_send_upload_fields(upload).expect("upload fields");
        let mut e = Encoder::new();
        e.array(12)
            .uint(1)
            .bytes(&OPERATION)
            .bytes(&attempt)
            .null()
            .bytes(&COMMUNITY)
            .bytes(&GROUP)
            .uint(7)
            .bytes(&hash)
            .uint(class)
            .uint(outcome)
            .uint(epoch)
            .opt_bytes(response);
        e.into_vec()
    }

    #[test]
    fn exact_ciphertext_replays_after_reopen_and_after_local_epoch_snapshot_changes() {
        let conn = connection();
        let mut core = bot(conn.clone());
        let request = request();
        let first = core
            .bot_send_prepare(&OPERATION, &ATTEMPT, &GROUP, &request, 1_800_000_001, 7)
            .expect("prepare");
        let (_, upload) = upload_from_output(&first);
        drop(core);

        conn.lock()
            .expect("connection")
            .execute(
                "UPDATE app_groups SET epoch=epoch+1 WHERE group_id=?1",
                [GROUP.as_slice()],
            )
            .expect("simulate newer epoch snapshot");
        let mut restarted = ClientCore::open(conn).expect("reopen core");
        let replay = restarted
            .bot_send_prepare(&OPERATION, &ATTEMPT, &GROUP, &request, 1_800_000_001, 7)
            .expect("replay");
        assert_eq!(replay, first);
        assert!(
            restarted
                .bot_send_prepare(&OPERATION, &ATTEMPT, &GROUP, &request, 1_800_000_001, 8)
                .is_err()
        );
        assert!(
            restarted
                .bot_send_prepare(&OPERATION, &ATTEMPT, &GROUP, &[0x80], 1_800_000_001, 7)
                .is_err()
        );
        let (_, replay_upload) = upload_from_output(&replay);
        assert_eq!(replay_upload, upload);
    }

    fn send_metadata(core: &ClientCore) -> Vec<(String, Vec<u8>)> {
        core.read(|c| {
            let mut statement = c.prepare("SELECT k,v FROM app_meta ORDER BY k")?;
            let rows = statement.query_map([], |r| Ok((r.get(0)?, r.get(1)?)))?;
            rows.collect::<Result<Vec<_>, _>>().map_err(Into::into)
        })
        .expect("send metadata")
    }

    fn remove_send_group(conn: &ConnHandle, deleted: bool) {
        let conn = conn.lock().expect("connection");
        if deleted {
            conn.execute(
                "DELETE FROM app_groups WHERE group_id=?1",
                [GROUP.as_slice()],
            )
            .expect("delete group");
        } else {
            conn.execute(
                "UPDATE app_groups SET state=?1 WHERE group_id=?2",
                params![STATE_GONE, GROUP.as_slice()],
            )
            .expect("leave group");
        }
    }

    #[test]
    fn exact_pending_and_accepted_send_replay_survives_left_or_deleted_group() {
        for deleted in [false, true] {
            for accepted in [false, true] {
                let conn = connection();
                let mut core = bot(conn.clone());
                let request = request();
                let first = core
                    .bot_send_prepare(&OPERATION, &ATTEMPT, &GROUP, &request, 1_800_000_001, 7)
                    .expect("prepare");
                if accepted {
                    let (_, upload) = upload_from_output(&first);
                    let mut response = Encoder::new();
                    response
                        .array(3)
                        .uint(1)
                        .bytes(&[0x5a; 32])
                        .uint(1_800_000_003);
                    let accepted_receipt = receipt(ATTEMPT, &upload, 1, Some(&response.into_vec()));
                    core.bot_send_receipt(&OPERATION, &accepted_receipt)
                        .expect("accept original send");
                }
                drop(core);
                remove_send_group(&conn, deleted);
                let mut core = ClientCore::open(conn).expect("reopen after removal");
                let metadata = send_metadata(&core);
                let replay = core
                    .bot_send_prepare(&OPERATION, &ATTEMPT, &GROUP, &request, 1_800_000_001, 7)
                    .expect("replay original ciphertext after group removal");
                assert_eq!(replay, first, "deleted={deleted}, accepted={accepted}");
                assert_eq!(
                    send_metadata(&core),
                    metadata,
                    "replay rewrote native metadata"
                );

                let mut changed = Encoder::new();
                changed
                    .array(4)
                    .uint(0)
                    .null()
                    .text("changed body")
                    .array(0);
                let changed = changed.into_vec();
                for (operation, attempt, body, now, generation) in [
                    (OPERATION, ATTEMPT, changed.as_slice(), 1_800_000_001, 7),
                    (OPERATION, ATTEMPT, request.as_slice(), 1_800_000_002, 7),
                    (OPERATION, ATTEMPT, request.as_slice(), 1_800_000_001, 8),
                    ([0xab; 16], ATTEMPT, request.as_slice(), 1_800_000_001, 7),
                    (OPERATION, [0xac; 16], request.as_slice(), 1_800_000_001, 7),
                ] {
                    assert!(
                        core.bot_send_prepare(&operation, &attempt, &GROUP, body, now, generation)
                            .is_err()
                    );
                    assert_eq!(
                        send_metadata(&core),
                        metadata,
                        "refused replay wrote metadata"
                    );
                }
            }
        }
    }

    #[test]
    fn inactive_send_group_refuses_preparing_retry_and_closed_successor_without_writes() {
        for deleted in [false, true] {
            for state in [BOT_SEND_PREPARING, BOT_SEND_CLOSED] {
                let conn = connection();
                let mut core = bot(conn.clone());
                let request = request();
                let first = core
                    .bot_send_prepare(&OPERATION, &ATTEMPT, &GROUP, &request, 1_800_000_001, 7)
                    .expect("prepare");
                let attempt = if state == BOT_SEND_CLOSED {
                    let (_, upload) = upload_from_output(&first);
                    core.bot_send_receipt(&OPERATION, &receipt(ATTEMPT, &upload, 0, None))
                        .expect("close original send");
                    [0xad; 16]
                } else {
                    let key = bot_send_attempt_key(&OPERATION, &ATTEMPT);
                    let raw = core
                        .read(|c| super::super::schema::meta_get(c, &key))
                        .expect("attempt")
                        .expect("attempt row");
                    let mut row = decode_bot_send_row(&raw).expect("decode attempt");
                    row.state = BOT_SEND_PREPARING;
                    row.epoch = None;
                    row.class = None;
                    row.ciphertext_hash = None;
                    row.upload = None;
                    row.receipt = None;
                    conn.lock()
                        .expect("connection")
                        .execute(
                            "UPDATE app_meta SET v=?1 WHERE k=?2",
                            params![encode_bot_send_row(&row), key],
                        )
                        .expect("simulate interrupted preparation");
                    ATTEMPT
                };
                drop(core);
                remove_send_group(&conn, deleted);
                let mut core = ClientCore::open(conn).expect("reopen after removal");
                let metadata = send_metadata(&core);
                assert!(
                    core.bot_send_prepare(&OPERATION, &attempt, &GROUP, &request, 1_800_000_001, 7)
                        .is_err()
                );
                assert_eq!(
                    send_metadata(&core),
                    metadata,
                    "inactive retry wrote preparing metadata"
                );
            }
        }
    }

    #[test]
    fn cached_send_replay_still_checks_every_identity_and_scope_binding() {
        let conn = connection();
        let mut core = bot(conn.clone());
        let request = request();
        core.bot_send_prepare(&OPERATION, &ATTEMPT, &GROUP, &request, 1_800_000_001, 7)
            .expect("prepare");
        let own = core.own().expect("own identity");
        remove_send_group(&conn, true);
        let request_hash: [u8; 32] = Sha256::digest(&request).into();
        for field in 0..5 {
            let mut ids = [INSTALLATION, own.user_id, own.device_id, COMMUNITY, GROUP];
            ids[field] = [0xfe; 16];
            let raw = encode_bot_send_operation(
                &ids[0],
                &ids[1],
                &ids[2],
                &ids[3],
                &ids[4],
                &request_hash,
                &ATTEMPT,
            );
            conn.lock()
                .expect("connection")
                .execute(
                    "UPDATE app_meta SET v=?1 WHERE k=?2",
                    params![raw, bot_send_op_key(&OPERATION)],
                )
                .expect("changed binding fixture");
            let metadata = send_metadata(&core);
            assert!(
                core.bot_send_prepare(&OPERATION, &ATTEMPT, &GROUP, &request, 1_800_000_001, 7)
                    .is_err(),
                "binding field={field}"
            );
            assert_eq!(
                send_metadata(&core),
                metadata,
                "binding refusal wrote metadata"
            );
        }
    }

    #[test]
    fn bot_send_refuses_a_group_outside_its_bound_community() {
        let mut core = bot(connection());
        let foreign = [0xbb; 16];
        let foreign_group = [0xcc; 16];
        core.group_create(&foreign_group, Some(&foreign), &[0xdd; 16], 0, &[0xee; 32])
            .expect("foreign group fixture");
        assert!(
            core.bot_send_prepare(
                &OPERATION,
                &ATTEMPT,
                &foreign_group,
                &request(),
                1_800_000_001,
                7
            )
            .is_err()
        );
        assert!(
            core.read(|c| crate::client::schema::meta_get(c, &bot_send_op_key(&OPERATION)))
                .expect("read operation")
                .is_none()
        );
    }

    #[test]
    fn only_closed_attempt_allows_an_explicit_successor() {
        let mut core = bot(connection());
        let request = request();
        let first = core
            .bot_send_prepare(&OPERATION, &ATTEMPT, &GROUP, &request, 1_800_000_001, 7)
            .expect("prepare");
        let (_, upload) = upload_from_output(&first);
        let closed = receipt(ATTEMPT, &upload, 0, None);
        core.bot_send_receipt(&OPERATION, &closed)
            .expect("close not accepted");

        let successor = [0xaa; 16];
        let next = core
            .bot_send_prepare(&OPERATION, &successor, &GROUP, &request, 1_800_000_002, 8)
            .expect("successor");
        assert_ne!(next, first);
        assert_eq!(
            core.bot_send_prepare(&OPERATION, &successor, &GROUP, &request, 1_800_000_002, 8)
                .expect("same successor retry"),
            next
        );
    }

    #[test]
    fn accepted_receipt_confirms_its_original_response_and_unknown_is_terminal() {
        let mut core = bot(connection());
        let request = request();
        let first = core
            .bot_send_prepare(&OPERATION, &ATTEMPT, &GROUP, &request, 1_800_000_001, 7)
            .expect("prepare");
        let (_, upload) = upload_from_output(&first);
        let mut response = Encoder::new();
        response
            .array(3)
            .uint(1)
            .bytes(&[0x5a; 32])
            .uint(1_800_000_003);
        let response = response.into_vec();
        let accepted = receipt(ATTEMPT, &upload, 1, Some(&response));
        core.bot_send_receipt(&OPERATION, &accepted)
            .expect("confirm original response");
        let (message, _) = upload_from_output(&first);
        let (seq, tag, recv_ts): (i64, Vec<u8>, i64) = core
            .read(|c| {
                c.query_row(
                    "SELECT seq,franking_tag,recv_ts FROM app_messages WHERE msg_id=?1",
                    [message.as_slice()],
                    |r| Ok((r.get(0)?, r.get(1)?, r.get(2)?)),
                )
                .map_err(Into::into)
            })
            .expect("confirmed own message");
        assert_eq!(seq, 1);
        assert_eq!(tag, vec![0x5a; 32]);
        assert_eq!(recv_ts, 1_800_000_003);
        core.bot_send_receipt(&OPERATION, &accepted)
            .expect("idempotent accepted receipt");
        let downgraded = receipt(ATTEMPT, &upload, 0, None);
        assert!(core.bot_send_receipt(&OPERATION, &downgraded).is_err());

        let other_conn = connection();
        let mut other = bot(other_conn);
        let unknown_first = other
            .bot_send_prepare(&OPERATION, &ATTEMPT, &GROUP, &request, 1_800_000_001, 7)
            .expect("prepare");
        let (_, unknown_upload) = upload_from_output(&unknown_first);
        let unknown = receipt(ATTEMPT, &unknown_upload, 2, None);
        other
            .bot_send_receipt(&OPERATION, &unknown)
            .expect("resolve unknown");
        assert!(
            other
                .bot_send_prepare(&OPERATION, &[0xaa; 16], &GROUP, &request, 1_800_000_002, 8)
                .is_err()
        );
    }

    #[test]
    fn accepted_receipt_and_direct_confirmation_reject_zero_sequence() {
        let mut core = bot(connection());
        let first = core
            .bot_send_prepare(&OPERATION, &ATTEMPT, &GROUP, &request(), 1_800_000_001, 7)
            .expect("prepare");
        let (message, upload) = upload_from_output(&first);
        let mut response = Encoder::new();
        response
            .array(3)
            .uint(0)
            .bytes(&[0x5a; 32])
            .uint(1_800_000_003);
        let response = response.into_vec();

        let accepted = receipt(ATTEMPT, &upload, 1, Some(&response));
        assert!(core.bot_send_receipt(&OPERATION, &accepted).is_err());
        assert!(core.send_confirm(&message, &response).is_err());
    }
}
