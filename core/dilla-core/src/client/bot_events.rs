//! Durable, native Bot-only snapshots of authenticated text-group application events.

use super::error::{E_CORE_INPUT, E_CORE_LIMIT, E_CORE_RELOAD, E_CORE_STATE, E_CORE_STORAGE};
use super::{ClientCore, ClientError, bot_identity, schema, trust};
use crate::cbor::Encoder;
use crate::identity::CredentialIdentity;
use crate::mls::{DillaGroup, GroupKind, StorageError};
use rusqlite::{OptionalExtension, params};

pub(super) type GroupBindingRow = (i64, Option<Vec<u8>>, Vec<u8>, i64);

const PAGE_LIMIT: u64 = 100;
// Leaves room for the helper response tuple and the outer 4-byte frame when the helper wraps
// this body as a bstr. The cap applies to the complete encoded core page.
const PAGE_BYTES: usize = 1_047_900;

struct EventRow {
    delivery_id: i64,
    group_id: Vec<u8>,
    community_id: Vec<u8>,
    channel_id: Vec<u8>,
    source_seq: i64,
    epoch: i64,
    recv_ts: i64,
    sender_user: Vec<u8>,
    sender_device: Vec<u8>,
    sender_kind: i64,
    sender_tier: i64,
    sender_leaf: i64,
    sender_credential: Vec<u8>,
    sender_sig_key: Vec<u8>,
    message_id: Vec<u8>,
    envelope_type: i64,
    original_body: Option<String>,
    reply_to: Option<Vec<u8>>,
    thread_id: Option<Vec<u8>>,
    disposition: i64,
}

fn bot_binding(core: &ClientCore) -> Result<bot_identity::BotBinding, ClientError> {
    let own = core.own()?;
    if own.kind != 1 || own.tier != 0 {
        return Err(ClientError::new(
            E_CORE_STATE,
            "native Bot identity required",
        ));
    }
    let raw = core.read(|c| schema::meta_get(c, schema::BOT_BINDING))?;
    let Some(raw) = raw else {
        return Err(ClientError::new(
            E_CORE_STATE,
            "native Bot binding required",
        ));
    };
    let binding = bot_identity::decode_binding(&raw)?;
    if !bot_identity::nonzero_ids(&binding) || binding.instance_id != own.instance_id {
        return Err(ClientError::new(
            E_CORE_STATE,
            "native Bot binding mismatch",
        ));
    }
    Ok(binding)
}

fn bot_community(core: &ClientCore) -> Result<[u8; 16], ClientError> {
    Ok(bot_binding(core)?.community_id)
}

/// Persist one accepted application snapshot in the source row's write unit. The authenticated
/// sender credential and leaf key stay internal so each drain can repeat current trust checks.
#[allow(clippy::too_many_arguments)] // Each field is authenticated MLS/envelope provenance for one atomic snapshot.
pub(super) fn record_accepted(
    c: &rusqlite::Connection,
    group: &DillaGroup,
    group_id: &[u8; 16],
    source_seq: u64,
    own: &super::Own,
    sender: &CredentialIdentity,
    sender_leaf: u32,
    sender_sig_key: &[u8],
    epoch: u64,
    recv_ts: u64,
    message_id: &[u8; 16],
    envelope_type: u8,
    original_body: &str,
    reply_to: Option<&[u8; 16]>,
    thread_id: Option<&[u8; 16]>,
) -> Result<(), StorageError> {
    if own.kind != 1
        || own.tier != 0
        || sender.user_id.as_bytes() == &own.user_id
        || sender_sig_key.len() != 32
    {
        return Ok(());
    }
    let Some(raw_binding) = schema::meta_get(c, schema::BOT_BINDING)? else {
        return Ok(());
    };
    let bot_binding = bot_identity::decode_binding(&raw_binding)
        .map_err(|e| StorageError::Codec(e.to_string()))?;
    if !bot_identity::nonzero_ids(&bot_binding) || bot_binding.instance_id != own.instance_id {
        return Ok(());
    }
    let binding: Option<GroupBindingRow> = c
        .query_row(
            "SELECT kind,community_id,target_id,state FROM app_groups WHERE group_id=?1",
            [group_id.as_slice()],
            |r| Ok((r.get(0)?, r.get(1)?, r.get(2)?, r.get(3)?)),
        )
        .optional()?;
    let Some((kind, Some(community), channel, state)) = binding else {
        return Ok(());
    };
    if state != 2 || community.as_slice() != bot_binding.community_id {
        return Ok(());
    }
    let eligible = if kind == 0 {
        group.kind() == GroupKind::Text
            && group.binding().is_ok_and(|b| {
                b.community_id
                    .as_ref()
                    .is_some_and(|id| id.as_bytes().as_slice() == community.as_slice())
                    && b.target_id.as_slice() == channel.as_slice()
            })
    } else {
        false
    };
    if !eligible {
        return Ok(());
    }
    let source_live: bool = c.query_row(
        "SELECT EXISTS(SELECT 1 FROM app_messages WHERE group_id=?1 AND seq=?2 AND status=0 AND sender_user=?3 AND sender_device=?4 AND sender_leaf=?5 AND sender_sig_key=?6)",
        params![group_id.as_slice(), source_seq as i64, sender.user_id.as_bytes().as_slice(), sender.device_id.as_bytes().as_slice(), i64::from(sender_leaf), sender_sig_key],
        |r| r.get(0),
    )?;
    if !source_live {
        return Ok(());
    }
    c.execute(
        "INSERT OR IGNORE INTO app_bot_events(group_id,source_seq,community_id,channel_id,epoch,recv_ts,sender_user,sender_device,sender_kind,sender_tier,sender_leaf,sender_credential,sender_sig_key,message_id,envelope_type,original_body,reply_to,thread_id) \
         VALUES(?1,?2,?3,?4,?5,?6,?7,?8,?9,?10,?11,?12,?13,?14,?15,?16,?17,?18)",
        params![
            group_id.as_slice(), source_seq as i64, bot_binding.community_id.as_slice(), channel,
            epoch as i64, recv_ts as i64, sender.user_id.as_bytes().as_slice(),
            sender.device_id.as_bytes().as_slice(), i64::from(sender.kind.as_u8()),
            i64::from(sender.tier.as_u8()), i64::from(sender_leaf), sender.encode(),
            sender_sig_key, message_id.as_slice(), i64::from(envelope_type), original_body,
            reply_to.map(|v| v.as_slice()), thread_id.map(|v| v.as_slice()),
        ],
    )?;
    Ok(())
}

/// Retain dedup metadata while making a retracted source permanently unreadable.
pub(super) fn suppress_source(
    c: &rusqlite::Connection,
    group_id: &[u8; 16],
    source_seq: i64,
) -> Result<(), StorageError> {
    c.execute(
        "UPDATE app_bot_events SET original_body=NULL,disposition=1 WHERE group_id=?1 AND source_seq=?2 AND disposition=0",
        params![group_id.as_slice(), source_seq],
    )?;
    Ok(())
}

/// An author deletion also withdraws queued edits whose source rows fold blanks.
pub(super) fn suppress_edits_of(
    c: &rusqlite::Connection,
    group_id: &[u8; 16],
    target: &[u8],
    author: &[u8; 16],
    after_seq: Option<i64>,
) -> Result<(), StorageError> {
    c.execute(
        "UPDATE app_bot_events SET original_body=NULL,disposition=1 WHERE disposition=0 AND group_id=?1 AND source_seq IN (SELECT seq FROM app_messages WHERE group_id=?1 AND reply_to=?2 AND type=1 AND status=0 AND sender_user=?3 AND (?4 IS NULL OR seq>?4))",
        params![group_id.as_slice(), target, author.as_slice(), after_seq],
    )?;
    Ok(())
}

fn fetch_rows(
    c: &rusqlite::Connection,
    after: u64,
    limit: u64,
) -> Result<Vec<EventRow>, StorageError> {
    let mut stmt = c.prepare(
        "SELECT delivery_id,group_id,community_id,channel_id,source_seq,epoch,recv_ts,sender_user,sender_device,sender_kind,sender_tier,sender_leaf,sender_credential,sender_sig_key,message_id,envelope_type,original_body,reply_to,thread_id,disposition \
         FROM app_bot_events WHERE delivery_id>?1 ORDER BY delivery_id LIMIT ?2",
    )?;
    let rows = stmt
        .query_map(params![after as i64, limit as i64], |r| {
            Ok(EventRow {
                delivery_id: r.get(0)?,
                group_id: r.get(1)?,
                community_id: r.get(2)?,
                channel_id: r.get(3)?,
                source_seq: r.get(4)?,
                epoch: r.get(5)?,
                recv_ts: r.get(6)?,
                sender_user: r.get(7)?,
                sender_device: r.get(8)?,
                sender_kind: r.get(9)?,
                sender_tier: r.get(10)?,
                sender_leaf: r.get(11)?,
                sender_credential: r.get(12)?,
                sender_sig_key: r.get(13)?,
                message_id: r.get(14)?,
                envelope_type: r.get(15)?,
                original_body: r.get(16)?,
                reply_to: r.get(17)?,
                thread_id: r.get(18)?,
                disposition: r.get(19)?,
            })
        })?
        .collect::<Result<Vec<_>, _>>()?;
    Ok(rows)
}

fn encode_page(events: &[Vec<u8>], next: u64, high_water: u64) -> Vec<u8> {
    let mut e = Encoder::new();
    e.array(3).array(events.len());
    for event in events {
        e.raw(event);
    }
    e.uint(next).uint(high_water);
    e.into_vec()
}

fn encode_event(row: &EventRow) -> Vec<u8> {
    let mut e = Encoder::new();
    e.array(17)
        .uint(row.delivery_id as u64)
        .bytes(&row.group_id)
        .bytes(&row.community_id)
        .bytes(&row.channel_id)
        .uint(row.source_seq as u64)
        .uint(row.epoch as u64)
        .uint(row.recv_ts as u64)
        .bytes(&row.sender_user)
        .bytes(&row.sender_device)
        .uint(row.sender_kind as u64)
        .uint(row.sender_tier as u64)
        .uint(row.sender_leaf as u64)
        .bytes(&row.message_id)
        .uint(row.envelope_type as u64);
    if let Some(body) = &row.original_body {
        e.text(body);
    } else {
        e.null();
    }
    e.opt_bytes(row.reply_to.as_deref())
        .opt_bytes(row.thread_id.as_deref());
    e.into_vec()
}

impl ClientCore {
    /// Read a deterministic page of durable authenticated events. The exclusive cursor is clamped
    /// to the installation's durable acknowledgement; each live row is rechecked against source,
    /// binding, current MLS membership and the current signed-device-list trust state.
    pub fn bot_events_read(
        &mut self,
        after_delivery_id: u64,
        limit: u64,
    ) -> Result<Vec<u8>, ClientError> {
        let scope = bot_binding(self)?;
        let community = scope.community_id;
        self.own()?;
        if !(1..=PAGE_LIMIT).contains(&limit) || after_delivery_id > i64::MAX as u64 {
            return Err(ClientError::new(
                E_CORE_INPUT,
                "event cursor/limit out of range",
            ));
        }
        let (ack, high_water, rows) = self.read(|c| {
            let ack: i64 = c.query_row(
                "SELECT ack_delivery_id FROM app_bot_event_state WHERE singleton=1",
                [],
                |r| r.get(0),
            )?;
            let high: i64 = c.query_row(
                "SELECT COALESCE(MAX(delivery_id),0) FROM app_bot_events",
                [],
                |r| r.get(0),
            )?;
            let rows = fetch_rows(c, after_delivery_id.max(ack as u64), limit)?;
            Ok((ack, high, rows))
        })?;
        let mut encoded_events = Vec::new();
        let mut next = after_delivery_id.max(ack as u64);
        for row in rows {
            let delivery = row.delivery_id as u64;
            let mut visible = row.original_body.is_some() && row.disposition == 0;
            if visible {
                let group_id: [u8; 16] =
                    row.group_id.as_slice().try_into().map_err(|_| {
                        ClientError::new(E_CORE_STORAGE, "journal group id malformed")
                    })?;
                let group = DillaGroup::load(
                    &self.provider,
                    &openmls::prelude::GroupId::from_slice(&group_id),
                )?;
                let mut valid = false;
                if let Some(group) = group {
                    let member_matches =
                        group
                            .member_at(row.sender_leaf as u32)
                            .is_some_and(|member| {
                                member.signature_key.as_slice() == row.sender_sig_key.as_slice()
                            });
                    let binding_matches = row.sender_kind == 0
                        && group.binding().is_ok_and(|binding| {
                            binding.kind == GroupKind::Text
                                && binding
                                    .community_id
                                    .as_ref()
                                    .is_some_and(|id| id.as_bytes() == community.as_slice())
                                && binding.target_id == row.channel_id.as_slice()
                        });
                    let signature_matches = binding_matches && member_matches;
                    if signature_matches {
                        let state = self.write(|_, u| {
                            Ok(u.with_conn(|c| {
                                let live: bool = c.query_row(
                                    "SELECT EXISTS(SELECT 1 FROM app_messages m JOIN app_groups g ON g.group_id=m.group_id WHERE m.group_id=?1 AND m.seq=?2 AND m.status=0 AND m.sender_user=?3 AND m.sender_device=?4 AND m.sender_leaf=?5 AND m.sender_sig_key=?6 AND g.state=2 AND g.kind=0 AND g.community_id=?7 AND g.target_id=?8)",
                                    params![row.group_id, row.source_seq, row.sender_user, row.sender_device, row.sender_leaf, row.sender_sig_key, community.as_slice(), row.channel_id],
                                    |r| r.get(0),
                                )?;
                                if !live { return Ok(false); }
                                match trust::validate_pending_sender(c, &group, &group_id, row.sender_leaf as u32, &row.sender_credential) {
                                    Ok(()) => Ok(true),
                                    Err(error) if error.code == E_CORE_STORAGE || error.code == E_CORE_RELOAD => {
                                        Err(StorageError::Sqlite("Bot event trust recheck failed operationally".into()))
                                    }
                                    Err(_) => Ok(false),
                                }
                            })?)
                        })?;
                        valid = state;
                    }
                }
                if !valid {
                    self.write(|_, u| Ok(u.with_conn(|c| {
                        c.execute("UPDATE app_bot_events SET original_body=NULL,disposition=1 WHERE delivery_id=?1 AND disposition=0", [row.delivery_id])?;
                        Ok(())
                    })?))?;
                    visible = false;
                }
            }
            if visible {
                let event = encode_event(&row);
                let mut candidate = encoded_events.clone();
                candidate.push(event.clone());
                let bytes = encode_page(&candidate, delivery, high_water as u64);
                if bytes.len() > PAGE_BYTES {
                    if encoded_events.is_empty() {
                        return Err(ClientError::new(
                            E_CORE_LIMIT,
                            "one Bot event exceeds the page limit",
                        ));
                    }
                    break;
                }
                encoded_events.push(event);
            }
            next = delivery;
        }
        let body = encode_page(&encoded_events, next, high_water as u64);
        if body.len() > PAGE_BYTES {
            return Err(ClientError::new(
                E_CORE_LIMIT,
                "Bot event page exceeds the response limit",
            ));
        }
        Ok(body)
    }

    /// Durably acknowledge and clear event bodies while retaining their compact source/delivery
    /// deduplication metadata. Backward and future acknowledgements are refused.
    pub fn bot_events_ack(&mut self, through_delivery_id: u64) -> Result<(), ClientError> {
        let _ = bot_community(self)?;
        if through_delivery_id > i64::MAX as u64 {
            return Err(ClientError::new(
                E_CORE_INPUT,
                "event acknowledgement out of range",
            ));
        }
        let (current_ack, high_water) = self.read(|c| {
            let ack: i64 = c.query_row(
                "SELECT ack_delivery_id FROM app_bot_event_state WHERE singleton=1",
                [],
                |r| r.get(0),
            )?;
            let high: i64 = c.query_row(
                "SELECT COALESCE(MAX(delivery_id),0) FROM app_bot_events",
                [],
                |r| r.get(0),
            )?;
            Ok((ack, high))
        })?;
        if (through_delivery_id as i64) < current_ack || (through_delivery_id as i64) > high_water {
            return Err(ClientError::new(
                E_CORE_INPUT,
                "invalid Bot event acknowledgement",
            ));
        }
        self.write(|_, u| {
            Ok(u.with_conn(|c| {
                let ack: i64 = c.query_row("SELECT ack_delivery_id FROM app_bot_event_state WHERE singleton=1", [], |r| r.get(0))?;
                let high: i64 = c.query_row("SELECT COALESCE(MAX(delivery_id),0) FROM app_bot_events", [], |r| r.get(0))?;
                let through = through_delivery_id as i64;
                if through < ack || through > high {
                    return Err(StorageError::Sqlite("acknowledgement changed concurrently".into()));
                }
                c.execute("UPDATE app_bot_event_state SET ack_delivery_id=?1 WHERE singleton=1", [through])?;
                c.execute("UPDATE app_bot_events SET original_body=NULL,disposition=2 WHERE delivery_id<=?1", [through])?;
                Ok(())
            })?)
        })
    }
}
