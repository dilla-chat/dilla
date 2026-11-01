use super::{ClientCore, ClientError, Own, trust};
use crate::{
    bot::{Request, RequestArgs, Response},
    cbor::{Encoder, decode_strict},
    identity::{CredentialIdentity, Kind, SignerTier, Tier},
    mls::{DillaGroup, GroupKind, StorageError},
};
use rusqlite::{OptionalExtension, params};

const PAGE_LIMIT: u64 = 100;
const MAX_PENDING: i64 = 4096;
const MAX_CONSUMED_RECEIPTS: i64 = 4096;
const PAGE_BYTES: usize = 1_047_900;

struct EventRow {
    delivery: i64,
    group: Vec<u8>,
    community: Vec<u8>,
    request_id: Vec<u8>,
    source_seq: i64,
    epoch: i64,
    recv_ts: i64,
    sender_user: Vec<u8>,
    sender_device: Vec<u8>,
    sender_leaf: i64,
    sender_credential: Vec<u8>,
    sender_sig_key: Vec<u8>,
    message_id: Vec<u8>,
    body: Option<String>,
    disposition: i64,
}

fn response_matches_request(
    response_body: &[u8],
    request_body: &[u8],
    community: &[u8; 16],
) -> bool {
    let (Ok(response), Ok(request)) = (
        Response::decode_body(response_body),
        Request::decode_body(request_body),
    ) else {
        return false;
    };
    ((matches!(request.operation, 0 | 24)
        && request.channel_id.is_none()
        && request.revision.is_none()
        && request.expires.is_none()
        && matches!(request.args, RequestArgs::None))
        || (matches!(request.operation, 60 | 64)
            && request.channel_id.is_none()
            && request.revision.is_none()
            && request.expires.is_none()
            && matches!(request.args, RequestArgs::Cursor(_)))
        || (request.operation == 1
            && request.channel_id.is_none()
            && request.revision.is_none()
            && request.expires.is_none()
            && matches!(request.args, RequestArgs::Section { .. }))
        || (request.operation == 2
            && request.channel_id.is_none()
            && request.revision.is_some()
            && request.expires.is_some()
            && matches!(request.args, RequestArgs::SettingsPut { .. }))
        || (request.operation == 31
            && request.channel_id.is_none()
            && request.revision.is_some()
            && request.expires.is_some()
            && matches!(request.args, RequestArgs::RolePanel { .. }))
        || (request.operation == 50
            && request.channel_id.is_none()
            && request.revision.is_none()
            && request.expires.is_none()
            && matches!(request.args, RequestArgs::Number(_)))
        || (request.operation == 51
            && request.channel_id.is_none()
            && request.revision.is_some()
            && request.expires.is_some()
            && matches!(request.args, RequestArgs::AnnouncementPut { .. }))
        || (request.operation == 52
            && request.channel_id.is_none()
            && request.revision.is_some()
            && request.expires.is_some()
            && matches!(request.args, RequestArgs::AnnouncementDelete { .. }))
        || (request.operation == 61
            && request.channel_id.is_none()
            && request.revision.is_some()
            && request.expires.is_some()
            && matches!(request.args, RequestArgs::Evidence { .. }))
        || (request.operation == 62
            && request.channel_id.is_none()
            && request.revision.is_none()
            && request.expires.is_none()
            && matches!(request.args, RequestArgs::None))
        || (request.operation == 63
            && request.channel_id.is_none()
            && request.revision.is_some()
            && request.expires.is_some()
            && matches!(request.args, RequestArgs::AutoModerationPut { .. }))
        || (request.operation == 3
            && request.channel_id.is_none()
            && request.revision.is_none()
            && request.expires.is_none()
            && matches!(request.args, RequestArgs::None))
        || (matches!(request.operation, 70 | 80)
            && request.channel_id.is_none()
            && request.revision == Some(0)
            && request.expires.is_some()
            && matches!(request.args, RequestArgs::UtilityPayload(_)))
        || (matches!(request.operation, 71 | 74 | 82 | 83)
            && request.channel_id.is_none()
            && (if matches!(request.operation, 74 | 82 | 83) {
                request.revision.is_some() && request.expires.is_some()
            } else {
                request.revision.is_none() && request.expires.is_none()
            })
            && matches!(request.args, RequestArgs::UtilityId(_)))
        || (request.operation == 72
            && request.channel_id.is_none()
            && request.revision.is_none()
            && request.expires.is_none()
            && matches!(request.args, RequestArgs::PollList { .. }))
        || (request.operation == 73
            && request.channel_id.is_none()
            && request.revision.is_some()
            && request.expires.is_some()
            && matches!(request.args, RequestArgs::PollVote { .. }))
        || (request.operation == 75
            && request.channel_id.is_none()
            && request.revision.is_none()
            && request.expires.is_none()
            && matches!(request.args, RequestArgs::PollVoters { .. }))
        || (matches!(request.operation, 81 | 84)
            && request.channel_id.is_none()
            && request.revision.is_none()
            && request.expires.is_none()
            && matches!(request.args, RequestArgs::Cursor(_)))
        || ((10..=11).contains(&request.operation)
            && request.channel_id.is_some()
            && request.revision.is_none()
            && request.expires.is_none())
        || (((12..=23).contains(&request.operation) || request.operation == 25)
            && request.channel_id.is_some()
            && request.revision.is_some()
            && request.expires.is_some()))
        && request.community_id.as_bytes() == community
        && response.community_id.as_bytes() == community
        && response.request_id == request.request_id
}

fn claim_response_request(
    c: &rusqlite::Connection,
    group_id: &[u8; 16],
    request_id: &[u8; 16],
    source_seq: u64,
    max_pending: i64,
) -> Result<bool, StorageError> {
    c.execute(
        "INSERT OR IGNORE INTO app_human_interaction_response_claims(group_id,request_id,source_seq) VALUES(?1,?2,?3)",
        params![group_id.as_slice(), request_id.as_slice(), source_seq as i64],
    )?;
    if c.changes() == 0 {
        return Ok(false);
    }
    let pending: i64 = c.query_row(
        "SELECT count(*) FROM app_human_interaction_events WHERE disposition=0 AND body IS NOT NULL",
        [],
        |r| r.get(0),
    )?;
    if pending >= max_pending {
        return Err(StorageError::Sqlite(
            "human interaction event journal is full".into(),
        ));
    }
    Ok(true)
}

fn human_owns_interaction(
    group: &DillaGroup,
    own: &Own,
    bot_user: &[u8; 16],
    sender: &CredentialIdentity,
    leaf: u32,
    sig_key: &[u8],
) -> bool {
    if group.kind() != GroupKind::Interaction || group.members().len() != 2 {
        return false;
    }
    let Some(member) = group.member_at(leaf) else {
        return false;
    };
    if member.signature_key.as_slice() != sig_key {
        return false;
    }
    let Ok(bot_sig) = <[u8; 32]>::try_from(member.signature_key.as_slice()) else {
        return false;
    };
    let Ok(basic) = openmls::prelude::BasicCredential::try_from(member.credential) else {
        return false;
    };
    let Ok(member_identity) = CredentialIdentity::decode(basic.identity()) else {
        return false;
    };
    if member_identity != *sender
        || sender.user_id.as_bytes() != bot_user
        || sender.kind != Kind::Bot
        || sender.tier != Tier::Native
        || sender.signer_tier != SignerTier::Native
        || sender.verify_signatures(&bot_sig).is_err()
    {
        return false;
    }
    let mut own_seen = false;
    let mut bot_seen = false;
    for candidate in group.members() {
        let Ok(candidate_basic) = openmls::prelude::BasicCredential::try_from(candidate.credential)
        else {
            return false;
        };
        let Ok(identity) = CredentialIdentity::decode(candidate_basic.identity()) else {
            return false;
        };
        let Ok(key) = <[u8; 32]>::try_from(candidate.signature_key.as_slice()) else {
            return false;
        };
        if identity.verify_signatures(&key).is_err() {
            return false;
        }
        if identity.user_id.as_bytes() == &own.user_id
            && identity.device_id.as_bytes() == &own.device_id
            && identity.kind == Kind::User
            && key == own.dsk_pub
        {
            own_seen = true;
        } else if identity == *sender {
            bot_seen = true;
        } else {
            return false;
        }
    }
    own_seen && bot_seen
}

#[expect(
    clippy::too_many_arguments,
    reason = "Each argument is provenance persisted in one atomic event row."
)]
pub(super) fn record_accepted(
    c: &rusqlite::Connection,
    group: &DillaGroup,
    group_id: &[u8; 16],
    source_seq: u64,
    own: &Own,
    sender: &CredentialIdentity,
    sender_leaf: u32,
    sender_sig_key: &[u8],
    epoch: u64,
    recv_ts: u64,
    message_id: &[u8; 16],
    envelope_type: u8,
    body: &str,
) -> Result<(), StorageError> {
    if own.kind != Kind::User.as_u8()
        || sender.kind != Kind::Bot
        || sender.tier != Tier::Native
        || sender.signer_tier != SignerTier::Native
    {
        return Ok(());
    }
    let group_row: Option<super::bot_events::GroupBindingRow> = c
        .query_row(
            "SELECT kind,community_id,target_id,state FROM app_groups WHERE group_id=?1",
            [group_id.as_slice()],
            |r| Ok((r.get(0)?, r.get(1)?, r.get(2)?, r.get(3)?)),
        )
        .optional()?;
    let Some((kind, Some(community), bot_user, state)) = group_row else {
        return Ok(());
    };
    let Ok(bot_user_id) = <[u8; 16]>::try_from(bot_user.as_slice()) else {
        return Ok(());
    };
    let binding_ok = group.interaction_binding().is_ok_and(|b| {
        b.community_id.as_bytes() == community.as_slice()
            && b.instance_id.as_bytes() == &own.instance_id
            && b.participant_user_id.as_slice() == own.user_id
            && b.participant_device_id.as_bytes() == &own.device_id
            && b.bot_user_id.as_slice() == bot_user.as_slice()
    });
    if kind != 3
        || state != 2
        || community.len() != 16
        || bot_user.as_slice() != sender.user_id.as_bytes()
        || !binding_ok
        || !human_owns_interaction(
            group,
            own,
            &bot_user_id,
            sender,
            sender_leaf,
            sender_sig_key,
        )
    {
        return Ok(());
    }
    let Ok(community_id) = <[u8; 16]>::try_from(community.as_slice()) else {
        return Ok(());
    };
    let Ok(response) = Response::decode_body(body.as_bytes()) else {
        return Ok(());
    };
    let request_body: Option<String> = c.query_row(
        "SELECT body FROM app_messages WHERE group_id=?1 AND msg_id=?2 AND status=0 AND sender_user=?3 AND sender_device=?4 AND sender_kind=0 AND type=0",
        params![group_id.as_slice(), response.request_id.as_bytes().as_slice(), own.user_id.as_slice(), own.device_id.as_slice()],
        |r| r.get(0),
    ).optional()?;
    let Some(request_body) = request_body else {
        return Ok(());
    };
    if !response_matches_request(body.as_bytes(), request_body.as_bytes(), &community_id) {
        return Ok(());
    }
    let source_live: bool = c.query_row(
        "SELECT EXISTS(SELECT 1 FROM app_messages WHERE group_id=?1 AND seq=?2 AND status=0 AND sender_user=?3 AND sender_device=?4 AND sender_leaf=?5 AND sender_sig_key=?6 AND msg_id=?7 AND type=?8)",
        params![group_id.as_slice(), source_seq as i64, sender.user_id.as_bytes().as_slice(), sender.device_id.as_bytes().as_slice(), i64::from(sender_leaf), sender_sig_key, message_id.as_slice(), i64::from(envelope_type)],
        |r| r.get(0),
    )?;
    if !source_live {
        return Ok(());
    }
    // Each moderation.cases cursor page uses its own request ID. This durable claim prevents a
    // second valid response from replaying the same request after the event itself is acked.
    if !claim_response_request(
        c,
        group_id,
        response.request_id.as_bytes(),
        source_seq,
        MAX_PENDING,
    )? {
        return Ok(());
    }
    c.execute(
        "INSERT OR IGNORE INTO app_human_interaction_events(group_id,community_id,request_id,source_seq,epoch,recv_ts,sender_user,sender_device,sender_leaf,sender_credential,sender_sig_key,message_id,body) VALUES(?1,?2,?3,?4,?5,?6,?7,?8,?9,?10,?11,?12,?13)",
        params![group_id.as_slice(), community_id.as_slice(), response.request_id.as_bytes().as_slice(), source_seq as i64, epoch as i64, recv_ts as i64,
            sender.user_id.as_bytes().as_slice(), sender.device_id.as_bytes().as_slice(), i64::from(sender_leaf),
            sender.encode(), sender_sig_key, message_id.as_slice(), body],
    )?;
    let inserted = c.changes();
    if inserted == 1 {
        let high: i64 = c.query_row("SELECT last_insert_rowid()", [], |r| r.get(0))?;
        c.execute(
            "UPDATE app_human_interaction_event_state SET high_water=?1 WHERE singleton=1",
            [high],
        )?;
    }
    Ok(())
}

pub(super) fn suppress_source(
    c: &rusqlite::Connection,
    group: &[u8; 16],
    seq: i64,
) -> Result<(), StorageError> {
    c.execute("UPDATE app_human_interaction_events SET body=NULL,disposition=1 WHERE group_id=?1 AND source_seq=?2 AND disposition=0", params![group.as_slice(), seq])?;
    Ok(())
}

fn fetch_rows(
    c: &rusqlite::Connection,
    after: u64,
    limit: u64,
) -> Result<Vec<EventRow>, StorageError> {
    // Cursor pages are consumed by the worker, whose inbox bound is over unconsumed rows. Retained
    // selectively consumed and suppressed tombstones must not consume that page budget or strand
    // a later pending response behind an unrelated unconsumed row.
    let mut stmt = c.prepare("SELECT delivery_id,group_id,community_id,request_id,source_seq,epoch,recv_ts,sender_user,sender_device,sender_leaf,sender_credential,sender_sig_key,message_id,body,disposition FROM app_human_interaction_events WHERE delivery_id>?1 AND disposition=0 AND body IS NOT NULL ORDER BY delivery_id LIMIT ?2")?;
    stmt.query_map(params![after as i64, limit as i64], |r| {
        Ok(EventRow {
            delivery: r.get(0)?,
            group: r.get(1)?,
            community: r.get(2)?,
            request_id: r.get(3)?,
            source_seq: r.get(4)?,
            epoch: r.get(5)?,
            recv_ts: r.get(6)?,
            sender_user: r.get(7)?,
            sender_device: r.get(8)?,
            sender_leaf: r.get(9)?,
            sender_credential: r.get(10)?,
            sender_sig_key: r.get(11)?,
            message_id: r.get(12)?,
            body: r.get(13)?,
            disposition: r.get(14)?,
        })
    })?
    .collect::<Result<Vec<_>, _>>()
    .map_err(Into::into)
}

fn prune_consumed_receipts(c: &rusqlite::Connection) -> Result<(), StorageError> {
    c.execute(
        "DELETE FROM app_human_interaction_receipts WHERE receipt_id IN (SELECT receipt_id FROM app_human_interaction_receipts ORDER BY receipt_id DESC LIMIT -1 OFFSET ?1)",
        [MAX_CONSUMED_RECEIPTS],
    )?;
    Ok(())
}

fn encode_event(row: &EventRow, request: &Request) -> Vec<u8> {
    let mut e = Encoder::new();
    e.array(13)
        .uint(row.delivery as u64)
        .bytes(&row.group)
        .bytes(&row.community)
        .bytes(&row.request_id)
        .uint(row.source_seq as u64)
        .uint(row.epoch as u64)
        .uint(row.recv_ts as u64)
        .bytes(&row.sender_user)
        .bytes(&row.sender_device)
        .bytes(&row.message_id)
        .text(row.body.as_deref().unwrap_or_default())
        .uint(request.operation)
        .opt_bytes(
            request
                .channel_id
                .as_ref()
                .map(|id| id.as_bytes().as_slice()),
        );
    e.into_vec()
}

impl ClientCore {
    /// Reads durable, authenticated kind-3 responses. This verifies MLS origin, local request
    /// correlation and current core trust; moderation evidence remains sensitive. Before rendering
    /// a `moderation.cases` body, the caller must recheck current authoritative `view_audit_log`
    /// permission through the human server API because this core cannot establish that authority.
    pub fn human_interaction_events_read(
        &mut self,
        after: u64,
        limit: u64,
    ) -> Result<Vec<u8>, ClientError> {
        let own = self.own()?;
        if own.kind != Kind::User.as_u8()
            || !(1..=PAGE_LIMIT).contains(&limit)
            || after > i64::MAX as u64
        {
            return Err(ClientError::new(
                "E_CORE_INPUT",
                "invalid human interaction event cursor/limit or identity",
            ));
        }
        let (ack, high, rows) = self.read(|c| {
            let (ack, high): (i64, i64) = c.query_row("SELECT ack_delivery_id,high_water FROM app_human_interaction_event_state WHERE singleton=1", [], |r| Ok((r.get(0)?,r.get(1)?)))?;
            Ok((ack, high, fetch_rows(c, after.max(ack as u64), limit)?))
        })?;
        if after > high as u64 {
            return Err(ClientError::new(
                "E_CORE_INPUT",
                "human interaction event cursor is ahead of the journal",
            ));
        }
        let mut events = Vec::new();
        let mut next = after.max(ack as u64);
        let page_was_short = rows.len() < limit as usize;
        for row in rows {
            let mut visible = row.body.is_some() && row.disposition == 0;
            let mut event_request = None;
            if visible {
                let group_id: [u8; 16] = row.group.as_slice().try_into().map_err(|_| {
                    ClientError::new("E_CORE_STORAGE", "interaction event group malformed")
                })?;
                let community: [u8; 16] = row.community.as_slice().try_into().map_err(|_| {
                    ClientError::new("E_CORE_STORAGE", "interaction event community malformed")
                })?;
                let group = DillaGroup::load(
                    &self.provider,
                    &openmls::prelude::GroupId::from_slice(&group_id),
                )?;
                let Some(group) = group else {
                    visible = false;
                    if !visible {
                        self.suppress_event(row.delivery)?;
                    }
                    next = row.delivery as u64;
                    continue;
                };
                let binding = group.interaction_binding().ok();
                let Ok(sender) = CredentialIdentity::decode(&row.sender_credential) else {
                    self.suppress_event(row.delivery)?;
                    next = row.delivery as u64;
                    continue;
                };
                let valid_binding = binding.as_ref().is_some_and(|b| {
                    b.community_id.as_bytes() == &community
                        && b.instance_id.as_bytes() == &own.instance_id
                        && b.participant_user_id.as_slice() == own.user_id
                        && b.participant_device_id.as_bytes() == &own.device_id
                        && b.bot_user_id.as_slice() == row.sender_user.as_slice()
                });
                let member_matches = binding.as_ref().is_some_and(|b| {
                    sender.user_id.as_bytes() == row.sender_user.as_slice()
                        && sender.device_id.as_bytes() == row.sender_device.as_slice()
                        && human_owns_interaction(
                            &group,
                            &own,
                            &b.bot_user_id,
                            &sender,
                            row.sender_leaf as u32,
                            &row.sender_sig_key,
                        )
                });
                let live: bool = self.write(|_, u| {
                    Ok(u.with_conn(|c| {
                        let live = c.query_row(
                            "SELECT EXISTS(SELECT 1 FROM app_messages m JOIN app_groups g ON g.group_id=m.group_id WHERE m.group_id=?1 AND m.seq=?2 AND m.status=0 AND m.sender_user=?3 AND m.sender_device=?4 AND m.sender_leaf=?5 AND m.sender_sig_key=?6 AND m.msg_id=?7 AND m.type=0 AND g.state=2 AND g.kind=3 AND g.community_id=?8 AND g.target_id=?9)",
                            params![row.group, row.source_seq, row.sender_user, row.sender_device, row.sender_leaf, row.sender_sig_key, row.message_id, community.as_slice(), row.sender_user],
                            |r| r.get(0),
                        )?;
                        Ok(live)
                    })?)
                })?;
                let request_body: Option<String> = self.read(|c| c.query_row(
                    "SELECT body FROM app_messages WHERE group_id=?1 AND msg_id=?2 AND status=0 AND sender_user=?3 AND sender_device=?4 AND sender_kind=0 AND type=0",
                    params![row.group, row.request_id, own.user_id.as_slice(), own.device_id.as_slice()], |r| r.get(0)).optional().map_err(Into::into))?;
                let trusted = if member_matches {
                    self.write(|_, u| {
                        Ok(u.with_conn(|c| {
                            match trust::validate_pending_sender(
                                c,
                                &group,
                                &group_id,
                                row.sender_leaf as u32,
                                &row.sender_credential,
                            ) {
                                Ok(()) => Ok(true),
                                Err(error) if error.code == "E_CORE_STORAGE" || error.code == "E_CORE_RELOAD" => {
                                    Err(StorageError::Sqlite("human interaction event trust recheck failed operationally".into()))
                                }
                                Err(_) => Ok(false),
                            }
                        })?)
                    })?
                } else {
                    false
                };
                event_request = request_body
                    .as_deref()
                    .and_then(|body| Request::decode_body(body.as_bytes()).ok());
                visible = valid_binding
                    && member_matches
                    && live
                    && trusted
                    && event_request.as_ref().is_some_and(|request| {
                        request.request_id.as_bytes() == row.request_id.as_slice()
                    })
                    && request_body.as_deref().is_some_and(|request| {
                        row.body.as_deref().is_some_and(|body| {
                            response_matches_request(
                                body.as_bytes(),
                                request.as_bytes(),
                                &community,
                            )
                        })
                    });
                if !visible {
                    self.suppress_event(row.delivery)?;
                }
            }
            if visible {
                let request = event_request.as_ref().ok_or_else(|| {
                    ClientError::new("E_CORE_STORAGE", "interaction request metadata is missing")
                })?;
                let mut candidate = events.clone();
                candidate.push(encode_event(&row, request));
                let mut page = Encoder::new();
                page.array(3).array(candidate.len());
                for event in &candidate {
                    page.raw(event);
                }
                page.uint(row.delivery as u64).uint(high as u64);
                if page.as_slice().len() > PAGE_BYTES {
                    return Err(ClientError::new(
                        "E_CORE_LIMIT",
                        "human interaction event page exceeds response limit",
                    ));
                }
                events.push(encode_event(&row, request));
            }
            next = row.delivery as u64;
        }
        if page_was_short {
            next = high as u64;
        }
        let mut e = Encoder::new();
        e.array(3).array(events.len());
        for event in &events {
            e.raw(event);
        }
        e.uint(next).uint(high as u64);
        Ok(e.into_vec())
    }

    /// Recovers only exact, previously consumed Bot responses. This does not participate in
    /// journal pagination and therefore cannot turn generic reads into historical reads.
    pub fn human_interaction_receipts_read(
        &mut self,
        community_id: &[u8; 16],
        installation_id: &[u8; 16],
        expected_bot_user_id: &[u8; 16],
        request_ids: &[u8],
    ) -> Result<Vec<u8>, ClientError> {
        let own = self.own()?;
        let ids = decode_strict(request_ids, |d| {
            let count = d.array_len()?;
            if !(1..=100).contains(&count) {
                return Err(crate::cbor::CborError::WrongArrayLen {
                    expected: 1,
                    actual: count,
                });
            }
            let mut ids = Vec::with_capacity(count);
            for _ in 0..count {
                ids.push(d.bytes_exact::<16>()?);
            }
            Ok(ids)
        })
        .map_err(|_| {
            ClientError::new("E_CORE_INPUT", "invalid archived interaction request ids")
        })?;
        let mut unique = std::collections::HashSet::with_capacity(ids.len());
        if own.kind != Kind::User.as_u8()
            || installation_id.iter().all(|byte| *byte == 0)
            || expected_bot_user_id.iter().all(|byte| *byte == 0)
            || ids
                .iter()
                .any(|id| id.iter().all(|byte| *byte == 0) || !unique.insert(*id))
        {
            return Err(ClientError::new(
                "E_CORE_INPUT",
                "invalid archived interaction scope",
            ));
        }

        let mut rows = Vec::new();
        for request_id in ids {
            rows.extend(self.read(|c| {
                let mut stmt = c.prepare("SELECT delivery_id,group_id,community_id,request_id,source_seq,epoch,recv_ts,sender_user,sender_device,sender_leaf,sender_credential,sender_sig_key,message_id,body FROM app_human_interaction_receipts WHERE community_id=?1 AND request_id=?2 ORDER BY receipt_id")?;
                stmt.query_map(params![community_id.as_slice(), request_id.as_slice()], |r| Ok(EventRow {
                    delivery: r.get(0)?, group: r.get(1)?, community: r.get(2)?, request_id: r.get(3)?,
                    source_seq: r.get(4)?, epoch: r.get(5)?, recv_ts: r.get(6)?, sender_user: r.get(7)?,
                    sender_device: r.get(8)?, sender_leaf: r.get(9)?, sender_credential: r.get(10)?,
                    sender_sig_key: r.get(11)?, message_id: r.get(12)?, body: Some(r.get(13)?), disposition: 0,
                }))?.collect::<Result<Vec<_>, _>>().map_err(Into::into)
            })?);
        }
        rows.sort_by_key(|row| row.delivery);
        let mut events = Vec::new();
        for row in rows {
            let group_id: [u8; 16] = row.group.as_slice().try_into().map_err(|_| {
                ClientError::new("E_CORE_STORAGE", "archived interaction group malformed")
            })?;
            let community: [u8; 16] = row.community.as_slice().try_into().map_err(|_| {
                ClientError::new("E_CORE_STORAGE", "archived interaction community malformed")
            })?;
            if community != *community_id {
                continue;
            }
            let Some(group) = DillaGroup::load(
                &self.provider,
                &openmls::prelude::GroupId::from_slice(&group_id),
            )?
            else {
                continue;
            };
            let binding = group.interaction_binding().ok();
            let sender = CredentialIdentity::decode(&row.sender_credential).ok();
            let current_group_binding = self.read(|c| {
                c.query_row("SELECT EXISTS(SELECT 1 FROM app_groups WHERE group_id=?1 AND kind=3 AND state=2 AND community_id=?2 AND target_id=?3 AND interaction_installation_id=?4 AND interaction_device_id=?5)",
                    params![row.group, community_id.as_slice(), row.sender_user.as_slice(), installation_id.as_slice(), own.device_id.as_slice()], |r| r.get::<_, bool>(0)).map_err(Into::into)
            })?;
            let valid_binding = binding.as_ref().is_some_and(|b| {
                b.community_id.as_bytes() == community_id
                    && b.instance_id.as_bytes() == &own.instance_id
                    && b.participant_user_id.as_slice() == own.user_id
                    && b.participant_device_id.as_bytes() == &own.device_id
                    && &b.installation_id == installation_id
                    && &b.bot_user_id == expected_bot_user_id
                    && b.bot_user_id.as_slice() == row.sender_user.as_slice()
            });
            let Some(sender) = sender else { continue };
            let member_matches = binding.as_ref().is_some_and(|b| {
                sender.user_id.as_bytes() == row.sender_user.as_slice()
                    && sender.device_id.as_bytes() == row.sender_device.as_slice()
                    && human_owns_interaction(
                        &group,
                        &own,
                        &b.bot_user_id,
                        &sender,
                        row.sender_leaf as u32,
                        &row.sender_sig_key,
                    )
            });
            if !current_group_binding || !valid_binding || !member_matches {
                continue;
            }
            let live_source: bool = self.read(|c| c.query_row(
                "SELECT EXISTS(SELECT 1 FROM app_messages m JOIN app_groups g ON g.group_id=m.group_id WHERE m.group_id=?1 AND m.seq=?2 AND m.status=0 AND m.sender_user=?3 AND m.sender_device=?4 AND m.sender_leaf=?5 AND m.sender_sig_key=?6 AND m.msg_id=?7 AND m.type=0 AND m.body=?12 AND g.state=2 AND g.kind=3 AND g.community_id=?8 AND g.target_id=?9 AND g.interaction_installation_id=?10 AND g.interaction_device_id=?11)",
                params![row.group, row.source_seq, row.sender_user, row.sender_device, row.sender_leaf, row.sender_sig_key, row.message_id, community_id.as_slice(), row.sender_user, installation_id.as_slice(), own.device_id.as_slice(), row.body], |r| r.get(0)).map_err(Into::into))?;
            let request_body: Option<String> = self.read(|c| c.query_row(
                "SELECT body FROM app_messages WHERE group_id=?1 AND msg_id=?2 AND status=0 AND sender_user=?3 AND sender_device=?4 AND sender_kind=0 AND type=0",
                params![row.group, row.request_id, own.user_id.as_slice(), own.device_id.as_slice()], |r| r.get(0)).optional().map_err(Into::into))?;
            let trusted = self.write(|_, u| {
                Ok(u.with_conn(|c| {
                    match trust::validate_pending_sender(
                        c,
                        &group,
                        &group_id,
                        row.sender_leaf as u32,
                        &row.sender_credential,
                    ) {
                        Ok(()) => Ok(true),
                        Err(error)
                            if error.code == "E_CORE_STORAGE" || error.code == "E_CORE_RELOAD" =>
                        {
                            Err(StorageError::Sqlite(
                                "archived interaction trust recheck failed operationally".into(),
                            ))
                        }
                        Err(_) => Ok(false),
                    }
                })?)
            })?;
            let body = row.body.as_deref().unwrap_or_default();
            let request = request_body
                .as_deref()
                .and_then(|body| Request::decode_body(body.as_bytes()).ok());
            let Some(request) = request.filter(|request| {
                live_source
                    && trusted
                    && request.request_id.as_bytes() == row.request_id.as_slice()
                    && response_matches_request(
                        body.as_bytes(),
                        request_body.as_deref().unwrap_or_default().as_bytes(),
                        community_id,
                    )
            }) else {
                continue;
            };
            events.push(encode_event(&row, &request));
        }
        let mut e = Encoder::new();
        e.array(events.len());
        for event in &events {
            e.raw(event);
        }
        if e.as_slice().len() > PAGE_BYTES {
            return Err(ClientError::new(
                "E_CORE_LIMIT",
                "archived interaction response exceeds response limit",
            ));
        }
        Ok(e.into_vec())
    }

    fn suppress_event(&mut self, delivery: i64) -> Result<(), ClientError> {
        self.write(|_, u| Ok(u.with_conn(|c| { c.execute("UPDATE app_human_interaction_events SET body=NULL,disposition=1 WHERE delivery_id=?1 AND disposition=0", [delivery])?; Ok(()) })?))
    }

    pub fn human_interaction_events_ack(&mut self, through: u64) -> Result<(), ClientError> {
        let own = self.own()?;
        if own.kind != Kind::User.as_u8() || through > i64::MAX as u64 {
            return Err(ClientError::new(
                "E_CORE_INPUT",
                "invalid human interaction event acknowledgement",
            ));
        }
        let (ack, high) = self.read(|c| {
            c.query_row(
                "SELECT ack_delivery_id,high_water FROM app_human_interaction_event_state WHERE singleton=1",
                [],
                |r| Ok((r.get::<_, i64>(0)?, r.get::<_, i64>(1)?)),
            )
            .map_err(Into::into)
        })?;
        if through < ack as u64 || through > high as u64 {
            return Err(ClientError::new(
                "E_CORE_INPUT",
                "invalid human interaction event acknowledgement",
            ));
        }
        self.write(|_, u| Ok(u.with_conn(|c| {
            let (ack, high): (i64, i64) = c.query_row("SELECT ack_delivery_id,high_water FROM app_human_interaction_event_state WHERE singleton=1", [], |r| Ok((r.get(0)?, r.get(1)?)))?;
            if through < ack as u64 || through > high as u64 { return Err(StorageError::Sqlite("invalid human interaction event acknowledgement".into())); }
            c.execute("UPDATE app_human_interaction_event_state SET ack_delivery_id=?1 WHERE singleton=1", [through as i64])?;
            c.execute("DELETE FROM app_human_interaction_events WHERE delivery_id<=?1", [through as i64])?;
            Ok(())
        })?))
    }

    /// Consumes exactly the delivered rows named by the worker. The journal is shared by every
    /// screen, so a consumer may not acknowledge an arbitrary prefix. Rows stay durable until
    /// selected, and the global cursor advances only over a contiguous consumed prefix.
    pub fn human_interaction_events_ack_selected(
        &mut self,
        delivery_ids: &[u8],
    ) -> Result<(), ClientError> {
        let own = self.own()?;
        if own.kind != Kind::User.as_u8() {
            return Err(ClientError::new(
                "E_CORE_INPUT",
                "invalid human interaction event acknowledgement identity",
            ));
        }
        let ids = decode_strict(delivery_ids, |d| {
            let count = d.array_len()?;
            if !(1..=100).contains(&count) {
                return Err(crate::cbor::CborError::WrongArrayLen {
                    expected: 1,
                    actual: count,
                });
            }
            let mut ids = Vec::with_capacity(count);
            for _ in 0..count {
                ids.push(d.uint()?);
            }
            Ok(ids)
        })
        .map_err(|_| ClientError::new("E_CORE_INPUT", "invalid selected event ids"))?;
        let mut unique = std::collections::HashSet::with_capacity(ids.len());
        if ids
            .iter()
            .any(|id| *id == 0 || *id > i64::MAX as u64 || !unique.insert(*id))
        {
            return Err(ClientError::new(
                "E_CORE_INPUT",
                "invalid selected event ids",
            ));
        }

        self.write(|_, u| {
            Ok(u.with_conn(|c| {
                let (ack, high): (i64, i64) = c.query_row(
                    "SELECT ack_delivery_id,high_water FROM app_human_interaction_event_state WHERE singleton=1",
                    [], |r| Ok((r.get(0)?, r.get(1)?)),
                )?;
                if ids.iter().any(|id| *id > high as u64) {
                    return Err(StorageError::Sqlite("selected event id exceeds high water".into()));
                }
                for id in &ids {
                    if *id <= ack as u64 { continue; }
                    // Transfer the authenticated response and all source provenance before
                    // consuming its journal row. Both writes run in this same SQLite
                    // transaction; an archive failure therefore leaves the pending body intact.
                    c.execute(
                        "INSERT OR IGNORE INTO app_human_interaction_receipts(delivery_id,group_id,community_id,request_id,source_seq,epoch,recv_ts,sender_user,sender_device,sender_leaf,sender_credential,sender_sig_key,message_id,body) SELECT delivery_id,group_id,community_id,request_id,source_seq,epoch,recv_ts,sender_user,sender_device,sender_leaf,sender_credential,sender_sig_key,message_id,body FROM app_human_interaction_events WHERE delivery_id=?1 AND disposition=0 AND body IS NOT NULL",
                        [*id as i64],
                    )?;
                    c.execute(
                        "UPDATE app_human_interaction_events SET body=NULL,disposition=2 WHERE delivery_id=?1 AND disposition<>2",
                        [*id as i64],
                    )?;
                    if c.changes() == 0 {
                        let exists: bool = c.query_row(
                            "SELECT EXISTS(SELECT 1 FROM app_human_interaction_events WHERE delivery_id=?1 AND disposition=2)",
                            [*id as i64], |r| r.get(0),
                        )?;
                        if !exists {
                            return Err(StorageError::Sqlite("selected event id is not in the journal".into()));
                        }
                    }
                }
                // Oldest consumed receipts may age out at this explicit bound. Pending journal
                // rows are in another table and are never evicted to make archive room.
                prune_consumed_receipts(c)?;
                let mut through = ack as u64;
                while through < high as u64 {
                    let next = through + 1;
                    let consumed: Option<i64> = c.query_row(
                        "SELECT disposition FROM app_human_interaction_events WHERE delivery_id=?1",
                        [next as i64], |r| r.get(0),
                    ).optional()?;
                    if consumed.is_some_and(|disposition| disposition != 0) {
                        through = next;
                    } else {
                        break;
                    }
                }
                if through > ack as u64 {
                    c.execute("UPDATE app_human_interaction_event_state SET ack_delivery_id=?1 WHERE singleton=1", [through as i64])?;
                    c.execute("DELETE FROM app_human_interaction_events WHERE delivery_id<=?1", [through as i64])?;
                }
                Ok(())
            })?)
        })
    }
}

#[cfg(test)]
mod tests {
    use super::{
        MAX_CONSUMED_RECEIPTS, MAX_PENDING, claim_response_request, prune_consumed_receipts,
        response_matches_request,
    };
    use crate::{
        bot::{Request, RequestArgs, Response},
        ids::{CommunityId, MsgId},
    };

    fn request(
        request_id: [u8; 16],
        community: [u8; 16],
        operation: u64,
        args: RequestArgs,
    ) -> String {
        Request {
            request_id: MsgId::from_bytes(request_id),
            community_id: CommunityId::from_bytes(community),
            channel_id: (10..=23)
                .contains(&operation)
                .then(|| crate::ids::ChannelId::from_bytes([3; 16])),
            operation,
            revision: (operation == 13
                || operation == 2
                || operation == 31
                || operation == 51
                || operation == 52
                || operation == 61
                || operation == 63
                || matches!(operation, 70 | 73 | 74 | 80 | 82 | 83))
            .then_some(if matches!(operation, 31 | 70 | 80) {
                0
            } else {
                7
            }),
            expires: (operation == 13
                || operation == 2
                || operation == 31
                || operation == 51
                || operation == 52
                || operation == 61
                || operation == 63
                || matches!(operation, 70 | 73 | 74 | 80 | 82 | 83))
            .then_some(20),
            args,
        }
        .encode_body()
        .unwrap_or_else(|e| panic!("encode request op {operation}: {e:?}"))
    }
    fn response(request_id: [u8; 16], community: [u8; 16]) -> String {
        Response {
            request_id: MsgId::from_bytes(request_id),
            community_id: CommunityId::from_bytes(community),
            status: 0,
            revision: None,
            data: vec![],
        }
        .encode_body()
        .unwrap()
    }
    #[test]
    fn moderation_response_requires_exact_local_request_and_community() {
        for operation in [60, 64] {
            let request_body = request([1; 16], [2; 16], operation, RequestArgs::Cursor(None));
            assert!(response_matches_request(
                response([1; 16], [2; 16]).as_bytes(),
                request_body.as_bytes(),
                &[2; 16]
            ));
        }
        let request_body = request([1; 16], [2; 16], 60, RequestArgs::Cursor(None));
        assert!(!response_matches_request(
            response([3; 16], [2; 16]).as_bytes(),
            request_body.as_bytes(),
            &[2; 16]
        ));
        assert!(!response_matches_request(
            response([1; 16], [4; 16]).as_bytes(),
            request_body.as_bytes(),
            &[2; 16]
        ));
    }

    #[test]
    fn provider_observation_responses_require_exact_local_request_and_community() {
        for operation in [0, 24] {
            let request_body = request([1; 16], [2; 16], operation, RequestArgs::None);
            assert!(
                response_matches_request(
                    response([1; 16], [2; 16]).as_bytes(),
                    request_body.as_bytes(),
                    &[2; 16]
                ),
                "operation {operation}"
            );
            for (request_id, response_community, journal_community) in [
                ([3; 16], [2; 16], [2; 16]),
                ([1; 16], [4; 16], [2; 16]),
                ([1; 16], [2; 16], [4; 16]),
            ] {
                assert!(!response_matches_request(
                    response(request_id, response_community).as_bytes(),
                    request_body.as_bytes(),
                    &journal_community
                ));
            }
        }
    }

    #[test]
    fn music_response_requires_exact_local_channel_request_and_community() {
        let request_body = request([1; 16], [2; 16], 13, RequestArgs::None);
        assert!(response_matches_request(
            response([1; 16], [2; 16]).as_bytes(),
            request_body.as_bytes(),
            &[2; 16]
        ));
        assert!(!response_matches_request(
            response([3; 16], [2; 16]).as_bytes(),
            request_body.as_bytes(),
            &[2; 16]
        ));
        assert!(!response_matches_request(
            response([1; 16], [4; 16]).as_bytes(),
            request_body.as_bytes(),
            &[2; 16]
        ));
    }

    #[test]
    fn settings_responses_require_their_exact_read_or_mutation_context() {
        let read = request([1; 16], [2; 16], 1, RequestArgs::Section { section: 5 });
        assert!(response_matches_request(
            response([1; 16], [2; 16]).as_bytes(),
            read.as_bytes(),
            &[2; 16]
        ));
        let write = request(
            [1; 16],
            [2; 16],
            2,
            RequestArgs::SettingsPut {
                section: 5,
                document: vec![0x80],
            },
        );
        assert!(response_matches_request(
            response([1; 16], [2; 16]).as_bytes(),
            write.as_bytes(),
            &[2; 16]
        ));
        assert!(!response_matches_request(
            response([3; 16], [2; 16]).as_bytes(),
            write.as_bytes(),
            &[2; 16]
        ));
        assert!(!response_matches_request(
            response([1; 16], [4; 16]).as_bytes(),
            write.as_bytes(),
            &[2; 16]
        ));
    }

    #[test]
    fn role_panel_response_requires_its_exact_local_request_and_community() {
        let mut mappings = crate::cbor::Encoder::new();
        mappings
            .array(1)
            .array(3)
            .text("📚")
            .bytes(&[5; 16])
            .text("Readers");
        let request_body = request(
            [1; 16],
            [2; 16],
            31,
            RequestArgs::RolePanel {
                channel: crate::ids::ChannelId::from_bytes([3; 16]),
                mappings: mappings.into_vec(),
            },
        );
        assert!(response_matches_request(
            response([1; 16], [2; 16]).as_bytes(),
            request_body.as_bytes(),
            &[2; 16]
        ));
        assert!(!response_matches_request(
            response([3; 16], [2; 16]).as_bytes(),
            request_body.as_bytes(),
            &[2; 16]
        ));
        assert!(!response_matches_request(
            response([1; 16], [4; 16]).as_bytes(),
            request_body.as_bytes(),
            &[2; 16]
        ));
    }

    #[test]
    fn announcement_responses_require_their_exact_local_request_and_community() {
        for (operation, args) in [
            (50, RequestArgs::Number(0)),
            (
                51,
                RequestArgs::AnnouncementPut {
                    schedule: None,
                    document: vec![0x80],
                },
            ),
            (
                52,
                RequestArgs::AnnouncementDelete {
                    schedule: MsgId::from_bytes([9; 16]),
                    expected_revision: 2,
                },
            ),
            (
                61,
                RequestArgs::Evidence {
                    action: MsgId::from_bytes([8; 16]),
                    reason: String::new(),
                    excerpt: String::new(),
                },
            ),
            (62, RequestArgs::None),
            (
                63,
                RequestArgs::AutoModerationPut {
                    grant_token: "grant".into(),
                    document: vec![0x80],
                },
            ),
        ] {
            let request_body = request([1; 16], [2; 16], operation, args);
            assert!(
                response_matches_request(
                    response([1; 16], [2; 16]).as_bytes(),
                    request_body.as_bytes(),
                    &[2; 16]
                ),
                "operation {operation}"
            );
            assert!(
                !response_matches_request(
                    response([3; 16], [2; 16]).as_bytes(),
                    request_body.as_bytes(),
                    &[2; 16]
                ),
                "wrong request id for operation {operation}"
            );
            assert!(
                !response_matches_request(
                    response([1; 16], [4; 16]).as_bytes(),
                    request_body.as_bytes(),
                    &[2; 16]
                ),
                "wrong community for operation {operation}"
            );
        }
    }

    #[test]
    fn response_request_receipt_is_unique_and_survives_event_ack() {
        let mut connection = rusqlite::Connection::open_in_memory().expect("open journal db");
        connection
            .execute_batch(super::super::schema::APP_HUMAN_INTERACTION_EVENTS)
            .expect("create human event journal");
        let claims: i64 = connection
            .query_row(
                "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='app_human_interaction_response_claims'",
                [],
                |row| row.get(0),
            )
            .expect("query response receipt table");
        assert_eq!(
            claims, 1,
            "response claims need independent durable storage"
        );
        assert!(
            claim_response_request(&connection, &[1; 16], &[2; 16], 3, MAX_PENDING)
                .expect("claim first response")
        );
        connection
            .execute("DELETE FROM app_human_interaction_events", [])
            .expect("acknowledge and clear response event");
        assert!(
            !claim_response_request(&connection, &[1; 16], &[2; 16], 4, MAX_PENDING)
                .expect("deduplicate after event acknowledgement")
        );

        for seq in 1..=MAX_PENDING {
            connection
                .execute(
                    "INSERT INTO app_human_interaction_events(group_id,community_id,request_id,source_seq,epoch,recv_ts,sender_user,sender_device,sender_leaf,sender_credential,sender_sig_key,message_id,body) VALUES(?1,?2,?3,?4,1,1,?5,?6,0,X'',zeroblob(32),?7,'pending')",
                    rusqlite::params![[3u8; 16].as_slice(), [4u8; 16].as_slice(), [6u8; 16].as_slice(), seq, [7u8; 16].as_slice(), [8u8; 16].as_slice(), [5u8; 16].as_slice()],
                )
                .expect("fill pending journal");
        }
        assert!(
            !claim_response_request(&connection, &[1; 16], &[2; 16], 5, MAX_PENDING)
                .expect("deduplicate even when pending journal is full")
        );
        let tx = connection
            .transaction()
            .expect("begin capacity transaction");
        assert!(claim_response_request(&tx, &[1; 16], &[5; 16], 6, MAX_PENDING).is_err());
        drop(tx);
        let unclaimed: i64 = connection
            .query_row(
                "SELECT count(*) FROM app_human_interaction_response_claims WHERE group_id=?1 AND request_id=?2",
                rusqlite::params![[1u8; 16].as_slice(), [5u8; 16].as_slice()],
                |row| row.get(0),
            )
            .expect("query rolled-back capacity claim");
        assert_eq!(unclaimed, 0, "failed full-queue writes leave no receipt");
    }

    #[test]
    fn consumed_receipt_bound_never_evicts_pending_journal_rows() {
        let connection = rusqlite::Connection::open_in_memory().expect("open journal db");
        connection
            .execute_batch(super::super::schema::APP_HUMAN_INTERACTION_EVENTS)
            .expect("create interaction journal and encrypted receipt archive");
        for delivery in 1..=MAX_CONSUMED_RECEIPTS + 7 {
            connection.execute(
                "INSERT INTO app_human_interaction_receipts(delivery_id,group_id,community_id,request_id,source_seq,epoch,recv_ts,sender_user,sender_device,sender_leaf,sender_credential,sender_sig_key,message_id,body) VALUES(?1,?2,?3,?4,?5,1,1,?6,?7,0,X'',zeroblob(32),?8,'receipt')",
                rusqlite::params![delivery, [3u8;16].as_slice(), [4u8;16].as_slice(), [6u8;16].as_slice(), delivery, [7u8;16].as_slice(), [8u8;16].as_slice(), [5u8;16].as_slice()],
            ).expect("insert consumed response receipt");
        }
        connection.execute(
            "INSERT INTO app_human_interaction_events(delivery_id,group_id,community_id,request_id,source_seq,epoch,recv_ts,sender_user,sender_device,sender_leaf,sender_credential,sender_sig_key,message_id,body) VALUES(9000,?1,?2,?3,9000,1,1,?4,?5,0,X'',zeroblob(32),?6,'still pending')",
            rusqlite::params![[3u8;16].as_slice(), [4u8;16].as_slice(), [9u8;16].as_slice(), [7u8;16].as_slice(), [8u8;16].as_slice(), [5u8;16].as_slice()],
        ).expect("seed independent pending row");
        prune_consumed_receipts(&connection).expect("enforce consumed archive cap");
        let (receipts, pending): (i64, Option<String>) = connection.query_row(
            "SELECT (SELECT count(*) FROM app_human_interaction_receipts), (SELECT body FROM app_human_interaction_events WHERE delivery_id=9000)",
            [], |r| Ok((r.get(0)?, r.get(1)?)),
        ).expect("check archive and pending state");
        assert_eq!(receipts, MAX_CONSUMED_RECEIPTS);
        assert_eq!(
            pending.as_deref(),
            Some("still pending"),
            "archive eviction cannot free capacity by dropping pending data"
        );
        let oldest: i64 = connection
            .query_row(
                "SELECT min(delivery_id) FROM app_human_interaction_receipts",
                [],
                |r| r.get(0),
            )
            .unwrap();
        assert_eq!(
            oldest, 8,
            "FIFO eviction retains the newest consumed receipts"
        );
    }

    #[test]
    fn archive_schema_upgrade_preserves_existing_pending_journal_rows_and_claims() {
        let connection = rusqlite::Connection::open_in_memory().expect("open pre-archive database");
        connection
            .execute_batch(super::super::schema::APP_HUMAN_INTERACTION_EVENTS)
            .expect("create prior journal schema");
        connection.execute(
            "INSERT INTO app_human_interaction_events(delivery_id,group_id,community_id,request_id,source_seq,epoch,recv_ts,sender_user,sender_device,sender_leaf,sender_credential,sender_sig_key,message_id,body) VALUES(1,?1,?2,?3,1,1,1,?4,?5,0,X'',zeroblob(32),?6,'pending response')",
            rusqlite::params![[3u8;16].as_slice(), [4u8;16].as_slice(), [6u8;16].as_slice(), [7u8;16].as_slice(), [8u8;16].as_slice(), [5u8;16].as_slice()],
        ).expect("seed pending row before additive schema migration");
        connection.execute(
            "INSERT INTO app_human_interaction_response_claims(group_id,request_id,source_seq) VALUES(?1,?2,1)",
            rusqlite::params![[3u8;16].as_slice(), [6u8;16].as_slice()],
        ).expect("seed existing durable response claim");
        connection
            .execute_batch(super::super::schema::APP_HUMAN_INTERACTION_EVENTS)
            .expect("rerun idempotent archive migration");
        let (pending, archive, claims): (Option<String>, i64, i64) = connection.query_row(
            "SELECT (SELECT body FROM app_human_interaction_events WHERE delivery_id=1), (SELECT count(*) FROM app_human_interaction_receipts), (SELECT count(*) FROM app_human_interaction_response_claims)",
            [], |r| Ok((r.get(0)?, r.get(1)?, r.get(2)?)),
        ).expect("check migrated journal");
        assert_eq!(pending.as_deref(), Some("pending response"));
        assert_eq!(archive, 0);
        assert_eq!(
            claims, 1,
            "the archive migration leaves response claims intact"
        );
    }

    #[test]
    fn response_pages_skip_consumed_and_suppressed_tombstones() {
        let connection = rusqlite::Connection::open_in_memory().expect("open journal db");
        connection
            .execute_batch(super::super::schema::APP_HUMAN_INTERACTION_EVENTS)
            .expect("create event journal");
        for delivery in 1..=4_201i64 {
            let disposition = if delivery == 1 || delivery == 4_201 {
                0
            } else if delivery % 2 == 0 {
                2
            } else {
                1
            };
            let body: Option<&str> = (disposition == 0).then_some("pending");
            connection.execute(
                "INSERT INTO app_human_interaction_events(delivery_id,group_id,community_id,request_id,source_seq,epoch,recv_ts,sender_user,sender_device,sender_leaf,sender_credential,sender_sig_key,message_id,body,disposition) VALUES(?1,?2,?3,?4,?5,1,1,?6,?7,0,X'',zeroblob(32),?8,?9,?10)",
                rusqlite::params![delivery, [3u8; 16].as_slice(), [4u8; 16].as_slice(), [6u8; 16].as_slice(), delivery, [7u8; 16].as_slice(), [8u8; 16].as_slice(), [5u8; 16].as_slice(), body, disposition],
            ).expect("insert event row");
        }
        let page = super::fetch_rows(&connection, 0, 100).expect("fetch active event page");
        assert_eq!(
            page.len(),
            2,
            "tombstones must not consume the active page budget"
        );
        assert_eq!(page[0].delivery, 1);
        assert_eq!(page[1].delivery, 4_201);
        assert_eq!(
            super::fetch_rows(&connection, 1, 100).expect("fetch after old pending row")[0]
                .delivery,
            4_201
        );
    }
    #[test]
    fn utility_response_binding_accepts_only_the_new_exact_request_shapes() {
        use crate::ids::ChannelId;
        let poll_payload = {
            let mut e = crate::cbor::Encoder::new();
            e.array(7)
                .uint(1)
                .bytes(&[9; 16])
                .null()
                .text("Question?")
                .array(2)
                .text("One")
                .text("Two")
                .uint(0)
                .uint(86400);
            e.into_vec()
        };
        let reminder_payload = {
            let mut e = crate::cbor::Encoder::new();
            e.array(7)
                .uint(1)
                .uint(0)
                .uint(3600)
                .text("Take a break")
                .text("UTC")
                .null()
                .null();
            e.into_vec()
        };
        let cases = [
            (3, RequestArgs::None),
            (70, RequestArgs::UtilityPayload(poll_payload.clone())),
            (71, RequestArgs::UtilityId(MsgId::from_bytes([9; 16]))),
            (
                72,
                RequestArgs::PollList {
                    channel: ChannelId::from_bytes([3; 16]),
                    thread: None,
                    cursor: None,
                },
            ),
            (
                73,
                RequestArgs::PollVote {
                    poll: MsgId::from_bytes([9; 16]),
                    selected: vec![0x82, 0, 1],
                },
            ),
            (74, RequestArgs::UtilityId(MsgId::from_bytes([9; 16]))),
            (
                75,
                RequestArgs::PollVoters {
                    poll: MsgId::from_bytes([9; 16]),
                    option: 1,
                    cursor: None,
                },
            ),
            (80, RequestArgs::UtilityPayload(reminder_payload)),
            (81, RequestArgs::Cursor(None)),
            (82, RequestArgs::UtilityId(MsgId::from_bytes([9; 16]))),
            (83, RequestArgs::UtilityId(MsgId::from_bytes([9; 16]))),
            (84, RequestArgs::Cursor(None)),
        ];
        for (index, (op, args)) in cases.into_iter().enumerate() {
            let req_id = [index as u8 + 1; 16];
            let mutation = matches!(op, 70 | 73 | 74 | 80 | 82 | 83);
            let request_body = crate::bot::Request {
                request_id: MsgId::from_bytes(req_id),
                community_id: CommunityId::from_bytes([2; 16]),
                channel_id: None,
                operation: op,
                revision: mutation.then_some(if matches!(op, 70 | 80) { 0 } else { 7 }),
                expires: mutation.then_some(20),
                args,
            }
            .encode_body()
            .unwrap_or_else(|e| panic!("encode utility op {op}: {e:?}"));
            assert!(
                response_matches_request(
                    response(req_id, [2; 16]).as_bytes(),
                    request_body.as_bytes(),
                    &[2; 16]
                ),
                "op {op}"
            );
        }
    }
}
