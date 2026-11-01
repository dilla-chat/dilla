use crate::helper::{DispatchError, body, empty, map_client};
use dilla_core::{
    cbor::{Decoder, Encoder, decode_strict},
    client::{ClientCore, ClientError},
};
use zeroize::Zeroizing;

type Binding = ([u8; 16], [u8; 16], [u8; 16]);
const MAX_INTERACTION_GROUP_DESCRIPTORS: usize = 2048;

fn validate_bot(core: &ClientCore, bind: Binding) -> Result<(), DispatchError> {
    core.bot_validate_binding(&bind.0, &bind.1, &bind.2)
        .map_err(map_client)
}

fn group_scope(core: &ClientCore, bind: Binding, group_id: &[u8; 16]) -> Result<(), DispatchError> {
    let row = core.group_row(group_id).map_err(map_client)?;
    let found = decode_strict(&row, |d| {
        if d.try_null()? {
            return Ok(None);
        }
        d.array(9)?;
        let id = d.bytes_exact::<16>()?;
        let kind = d.uint()?;
        let community = d.opt_bytes()?.map(|v| v.to_vec());
        let _target = d.bytes_exact::<16>()?;
        let _state = d.uint()?;
        let _epoch = d.uint()?;
        let _next = d.uint()?;
        let _proposals = d.uint()?;
        let _pending = d.uint()?;
        Ok(Some((id, kind, community)))
    })
    .map_err(|_| DispatchError::Internal)?;
    let Some((id, kind, community)) = found else {
        return Err(DispatchError::NotFound);
    };
    if id != *group_id || !matches!(kind, 0 | 1) || community.as_deref() != Some(&bind.1) {
        return Err(DispatchError::Forbidden);
    }
    Ok(())
}

/// Dispatch native group operations after checking the persisted installation identity. Direct
/// op6 and staged op16 both enter here, so scope and intent semantics cannot diverge.
pub(crate) fn dispatch(
    core: &mut ClientCore,
    bind: Binding,
    payload: &[u8],
) -> Result<Zeroizing<Vec<u8>>, DispatchError> {
    validate_bot(core, bind)?;
    if crate::transfer::validate_target_payload(payload).is_err() {
        return Err(DispatchError::Invalid);
    }
    let mut selector = dilla_core::cbor::Decoder::new(payload);
    selector.array_len().map_err(|_| DispatchError::Invalid)?;
    let sub = selector.uint().map_err(|_| DispatchError::Invalid)?;
    match sub {
        0 => list_groups(core, bind, payload),
        1 => inspect_group(core, bind, payload),
        2 => create_group(core, bind, payload),
        3 => registered(core, bind, payload),
        4 => discard(core, bind, payload),
        5 => external_join(core, bind, payload),
        6 => joined(core, bind, payload),
        7 => welcomes(core, bind, payload),
        8 => apply(core, bind, payload),
        9 => build_commit(core, bind, payload),
        10 => confirm_commit(core, bind, payload),
        11 => abort_commit(core, bind, payload),
        12 => cursor(core, bind, payload),
        13 => revalidate(core, bind, payload),
        14 => trust_sender(core, bind, payload),
        15 => proposed_adds(core, bind, payload),
        16 => trust_add(core, bind, payload),
        17 => pending_leaves(core, bind, payload),
        18 => events_read(core, bind, payload),
        19 => events_ack(core, bind, payload),
        20 => intent_result(core, bind, payload),
        21 => interaction_create(core, payload),
        22 => interaction_inspect(core, payload),
        23 => interaction_groups(core, bind, payload),
        24 => interaction_apply(core, payload),
        25 => interaction_cursor(core, payload),
        26 => interaction_send_prepare(core, payload),
        27 => interaction_heal_snapshot(core, bind, payload),
        _ => Err(DispatchError::Invalid),
    }
}

/// Export only authenticated local MLS material. No group info, tree, roster, or tail is accepted
/// from the caller; a paired restore starts with an empty replay tail.
fn interaction_heal_snapshot(
    core: &ClientCore,
    bind: Binding,
    payload: &[u8],
) -> Result<Zeroizing<Vec<u8>>, DispatchError> {
    let group = decode_strict(payload, |d| {
        d.array(2)?;
        if d.uint()? != 27 {
            return Err(dilla_core::cbor::CborError::TypeMismatch {
                expected: "interaction heal snapshot",
                offset: 0,
            });
        }
        d.bytes_exact::<16>()
    })
    .map_err(|_| DispatchError::Invalid)?;
    let snapshot = core
        .bot_interaction_heal_snapshot(&group, &bind.0, &bind.1, &bind.2)
        .map_err(map_client)?;
    let output = body(snapshot);
    if output.len() > dilla_core::bot::HELPER_PAYLOAD_MAX {
        return Err(DispatchError::Unavailable);
    }
    Ok(Zeroizing::new(output))
}

fn interaction_send_prepare(
    core: &mut ClientCore,
    payload: &[u8],
) -> Result<Zeroizing<Vec<u8>>, DispatchError> {
    let (expected, snapshot, operation, attempt, request, now, generation) =
        decode_strict(payload, |d| {
            d.array(8)?;
            if d.uint()? != 26 {
                return Err(dilla_core::cbor::CborError::TypeMismatch {
                    expected: "interaction send prepare",
                    offset: 0,
                });
            }
            let expected = interaction_expected(d)?;
            let snapshot = d.bytes()?.to_vec();
            let operation = d.bytes_exact::<16>()?;
            let attempt = d.bytes_exact::<16>()?;
            let request = d.bytes()?.to_vec();
            let now = d.uint()?;
            let generation = d.uint()?;
            Ok((
                expected, snapshot, operation, attempt, request, now, generation,
            ))
        })
        .map_err(|_| DispatchError::Invalid)?;
    if snapshot.len() > 160 << 10 || request.len() > 32 << 20 {
        return Err(DispatchError::Invalid);
    }
    let out = core
        .bot_interaction_send_prepare(
            &operation,
            &attempt,
            expected.group.as_bytes(),
            &request,
            now,
            generation,
            &snapshot,
            &expected,
        )
        .map_err(map_client)?;
    Ok(interaction_send_prepare_response(out))
}

fn interaction_send_prepare_response(out: Vec<u8>) -> Zeroizing<Vec<u8>> {
    Zeroizing::new(body(out))
}

fn interaction_expected(
    d: &mut Decoder<'_>,
) -> Result<dilla_core::bot::interaction_admission::Expected, dilla_core::cbor::CborError> {
    use dilla_core::{bot::interaction_admission::Expected, ids::MsgId};
    d.array(11)?;
    Ok(Expected {
        instance: MsgId::from_bytes(d.bytes_exact::<16>()?),
        installation: MsgId::from_bytes(d.bytes_exact::<16>()?),
        community: MsgId::from_bytes(d.bytes_exact::<16>()?),
        bot_user: MsgId::from_bytes(d.bytes_exact::<16>()?),
        bot_device: MsgId::from_bytes(d.bytes_exact::<16>()?),
        participant_user: MsgId::from_bytes(d.bytes_exact::<16>()?),
        participant_device: MsgId::from_bytes(d.bytes_exact::<16>()?),
        group: MsgId::from_bytes(d.bytes_exact::<16>()?),
        minimum_eligibility_revision: d.uint()?,
        server_now: d.uint()?,
        policy_version: d.uint()?,
    })
}

fn interaction_groups(
    core: &ClientCore,
    bind: Binding,
    payload: &[u8],
) -> Result<Zeroizing<Vec<u8>>, DispatchError> {
    decode_strict(payload, |d| {
        d.array(1)?;
        if d.uint()? != 23 {
            return Err(dilla_core::cbor::CborError::TypeMismatch {
                expected: "interaction group discovery",
                offset: 0,
            });
        }
        Ok(())
    })
    .map_err(|_| DispatchError::Invalid)?;
    let descriptors = core
        .bot_interaction_group_descriptors(&bind.0, &bind.1, &bind.2)
        .map_err(map_client)?;
    if descriptors.len() > MAX_INTERACTION_GROUP_DESCRIPTORS {
        return Err(DispatchError::Unavailable);
    }
    let mut e = Encoder::new();
    e.array(descriptors.len());
    for d in descriptors {
        e.array(13)
            .bytes(&d.group)
            .bytes(&d.instance)
            .bytes(&d.bot_user)
            .bytes(&d.bot_device)
            .bytes(&d.installation)
            .bytes(&d.community)
            .bytes(&d.participant_user)
            .bytes(&d.participant_device)
            .uint(d.policy_version)
            .uint(d.e2ee_version)
            .uint(d.media_version)
            .uint(d.epoch)
            .uint(d.state);
    }
    Ok(Zeroizing::new(body(e.into_vec())))
}

/// Inspect a persisted interaction group from its authenticated OpenMLS state and a fresh
/// server-signed context snapshot. The caller supplies no roster authority.
fn interaction_inspect(
    core: &ClientCore,
    payload: &[u8],
) -> Result<Zeroizing<Vec<u8>>, DispatchError> {
    use dilla_core::{bot::interaction_admission::Expected, ids::MsgId};

    let (group, snapshot, expected) = decode_strict(payload, |d| {
        d.array(4)?;
        let _ = d.uint()?;
        let group = d.bytes_exact::<16>()?;
        let snapshot = Zeroizing::new(d.bytes()?.to_vec());
        d.array(11)?;
        let expected = Expected {
            instance: MsgId::from_bytes(d.bytes_exact::<16>()?),
            installation: MsgId::from_bytes(d.bytes_exact::<16>()?),
            community: MsgId::from_bytes(d.bytes_exact::<16>()?),
            bot_user: MsgId::from_bytes(d.bytes_exact::<16>()?),
            bot_device: MsgId::from_bytes(d.bytes_exact::<16>()?),
            participant_user: MsgId::from_bytes(d.bytes_exact::<16>()?),
            participant_device: MsgId::from_bytes(d.bytes_exact::<16>()?),
            group: MsgId::from_bytes(d.bytes_exact::<16>()?),
            minimum_eligibility_revision: d.uint()?,
            server_now: d.uint()?,
            policy_version: d.uint()?,
        };
        Ok((group, snapshot, expected))
    })
    .map_err(|_| DispatchError::Invalid)?;
    if group == [0; 16] || group != *expected.group.as_bytes() || snapshot.is_empty() {
        return Err(DispatchError::Invalid);
    }

    let admission = core
        .bot_inspect_stored_interaction_group(&group, &snapshot, &expected)
        .map_err(map_interaction_read)?;
    let row = core.group_row(&group).map_err(map_client)?;
    let (state, epoch, next_sequence, pending_commit) = decode_strict(&row, |d| {
        d.array(9)?;
        let id = d.bytes_exact::<16>()?;
        let kind = d.uint()?;
        let _community = d.opt_bytes()?;
        let _target = d.bytes_exact::<16>()?;
        let state = d.uint()?;
        let epoch = d.uint()?;
        let next_sequence = d.uint()?;
        d.skip()?;
        let pending_commit = d.uint()?;
        if pending_commit > 1 {
            return Err(dilla_core::cbor::CborError::TypeMismatch {
                expected: "pending commit flag",
                offset: 0,
            });
        }
        if id != group || kind != 3 {
            return Err(dilla_core::cbor::CborError::TypeMismatch {
                expected: "interaction group row",
                offset: 0,
            });
        }
        Ok((state, epoch, next_sequence, pending_commit))
    })
    .map_err(|_| DispatchError::Internal)?;

    let mut e = Encoder::new();
    // v2 binding, the exact two authenticated devices, then persisted MLS epoch and app state.
    e.array(15)
        .uint(2)
        .bytes(admission.instance.as_bytes())
        .bytes(admission.bot_user.as_bytes())
        .bytes(admission.bot_device.as_bytes())
        .bytes(admission.installation.as_bytes())
        .bytes(admission.community.as_bytes())
        .bytes(admission.participant_user.as_bytes())
        .bytes(admission.participant_device.as_bytes())
        .uint(expected.policy_version)
        .uint(1)
        .uint(0)
        .uint(epoch)
        .uint(state)
        .uint(next_sequence)
        .uint(pending_commit);
    Ok(Zeroizing::new(body(e.into_vec())))
}

fn interaction_create(
    core: &mut ClientCore,
    payload: &[u8],
) -> Result<Zeroizing<Vec<u8>>, DispatchError> {
    use dilla_core::{bot::interaction_admission::Expected, ids::MsgId};

    let (operation, group, snapshot, expected, key_package) = decode_strict(payload, |d| {
        d.array(6)?;
        let _ = d.uint()?;
        let operation = d.bytes_exact::<16>()?;
        let group = d.bytes_exact::<16>()?;
        let snapshot = Zeroizing::new(d.bytes()?.to_vec());
        d.array(11)?;
        let expected = Expected {
            instance: MsgId::from_bytes(d.bytes_exact::<16>()?),
            installation: MsgId::from_bytes(d.bytes_exact::<16>()?),
            community: MsgId::from_bytes(d.bytes_exact::<16>()?),
            bot_user: MsgId::from_bytes(d.bytes_exact::<16>()?),
            bot_device: MsgId::from_bytes(d.bytes_exact::<16>()?),
            participant_user: MsgId::from_bytes(d.bytes_exact::<16>()?),
            participant_device: MsgId::from_bytes(d.bytes_exact::<16>()?),
            group: MsgId::from_bytes(d.bytes_exact::<16>()?),
            minimum_eligibility_revision: d.uint()?,
            server_now: d.uint()?,
            policy_version: d.uint()?,
        };
        let key_package = Zeroizing::new(d.bytes()?.to_vec());
        Ok((operation, group, snapshot, expected, key_package))
    })
    .map_err(|_| DispatchError::Invalid)?;
    if operation == [0; 16] || group == [0; 16] {
        return Err(DispatchError::Invalid);
    }
    let result = core
        .bot_interaction_group_create(&operation, &group, &snapshot, &expected, &key_package)
        .map_err(|error| {
            if interaction_create_refusal_class(&error) == "snapshot_expired" {
                return DispatchError::Expired;
            }
            if std::env::var_os("DILLA_BOT_PRIVATE_DIAGNOSTICS").as_deref()
                == Some(std::ffi::OsStr::new("1"))
            {
                eprintln!(
                    "dilla_bot_helper interaction_create_refusal={}",
                    interaction_create_refusal_class(&error)
                );
            }
            map_client(error)
        })?;
    Ok(Zeroizing::new(body(result)))
}

fn interaction_apply(
    core: &mut ClientCore,
    payload: &[u8],
) -> Result<Zeroizing<Vec<u8>>, DispatchError> {
    let (group, snapshot, expected, handshakes, messages, through) = decode_strict(payload, |d| {
        d.array(7)?;
        let _ = d.uint()?;
        let group = d.bytes_exact::<16>()?;
        let snapshot = Zeroizing::new(d.bytes()?.to_vec());
        let expected = interaction_expected(d)?;
        let handshakes = Zeroizing::new(d.bytes()?.to_vec());
        let messages = Zeroizing::new(d.bytes()?.to_vec());
        let through = d.uint()?;
        Ok((group, snapshot, expected, handshakes, messages, through))
    })
    .map_err(|_| DispatchError::Invalid)?;
    if group == [0; 16] || group != *expected.group.as_bytes() || snapshot.is_empty() {
        return Err(DispatchError::Invalid);
    }
    let result = core
        .bot_apply_interaction_group(
            &group,
            &snapshot,
            &expected,
            &handshakes,
            &messages,
            through,
        )
        .map_err(map_interaction_read)?;
    Ok(Zeroizing::new(body(result)))
}

fn interaction_cursor(
    core: &ClientCore,
    payload: &[u8],
) -> Result<Zeroizing<Vec<u8>>, DispatchError> {
    let (group, snapshot, expected) = decode_strict(payload, |d| {
        d.array(4)?;
        let _ = d.uint()?;
        let group = d.bytes_exact::<16>()?;
        let snapshot = Zeroizing::new(d.bytes()?.to_vec());
        let expected = interaction_expected(d)?;
        Ok((group, snapshot, expected))
    })
    .map_err(|_| DispatchError::Invalid)?;
    if group == [0; 16] || group != *expected.group.as_bytes() || snapshot.is_empty() {
        return Err(DispatchError::Invalid);
    }
    Ok(Zeroizing::new(body(
        core.bot_interaction_group_cursor(&group, &snapshot, &expected)
            .map_err(map_interaction_read)?,
    )))
}

fn list_groups(
    core: &ClientCore,
    bind: Binding,
    payload: &[u8],
) -> Result<Zeroizing<Vec<u8>>, DispatchError> {
    decode_strict(payload, |d| {
        d.array(1)?;
        let _ = d.uint()?;
        Ok(())
    })
    .map_err(|_| DispatchError::Invalid)?;
    let rows = core.groups().map_err(map_client)?;
    let ids = decode_strict(&rows, |d| {
        let n = d.array_len()?;
        let mut out = Vec::with_capacity(n);
        for _ in 0..n {
            d.array(9)?;
            let id = d.bytes_exact::<16>()?;
            let kind = d.uint()?;
            let community = d.opt_bytes()?.map(|v| v.to_vec());
            d.skip()?;
            d.skip()?;
            d.skip()?;
            d.skip()?;
            d.skip()?;
            d.skip()?;
            if matches!(kind, 0 | 1) && community.as_deref() == Some(&bind.1) {
                out.push(id);
            }
        }
        Ok(out)
    })
    .map_err(|_| DispatchError::Internal)?;
    let mut e = Encoder::new();
    e.array(ids.len());
    for id in ids {
        let row = core.group_row(&id).map_err(map_client)?;
        e.raw(&row);
    }
    Ok(Zeroizing::new(body(e.into_vec())))
}

fn inspect_group(
    core: &ClientCore,
    bind: Binding,
    payload: &[u8],
) -> Result<Zeroizing<Vec<u8>>, DispatchError> {
    let id = decode_strict(payload, |d| {
        d.array(2)?;
        let _ = d.uint()?;
        d.bytes_exact::<16>()
    })
    .map_err(|_| DispatchError::Invalid)?;
    group_scope(core, bind, &id)?;
    Ok(Zeroizing::new(body(
        core.group_row(&id).map_err(map_client)?,
    )))
}

fn create_group(
    core: &mut ClientCore,
    _bind: Binding,
    payload: &[u8],
) -> Result<Zeroizing<Vec<u8>>, DispatchError> {
    let (op, group, channel, kind, policy, external) = decode_strict(payload, |d| {
        d.array(7)?;
        let _ = d.uint()?;
        Ok((
            d.bytes_exact::<16>()?,
            d.bytes_exact::<16>()?,
            d.bytes_exact::<16>()?,
            d.uint()?,
            d.uint()?,
            d.bytes_exact::<32>()?,
        ))
    })
    .map_err(|_| DispatchError::Invalid)?;
    if op == [0; 16] || !matches!(kind, 0 | 1) || external == [0; 32] {
        return Err(DispatchError::Invalid);
    }
    let result = core
        .bot_group_create(&op, &group, &channel, kind, policy, &external)
        .map_err(map_client)?;
    Ok(Zeroizing::new(body(result)))
}

fn registered(
    core: &mut ClientCore,
    _bind: Binding,
    payload: &[u8],
) -> Result<Zeroizing<Vec<u8>>, DispatchError> {
    let (op, group, seq) = decode_strict(payload, |d| {
        d.array(4)?;
        let _ = d.uint()?;
        Ok((d.bytes_exact::<16>()?, d.bytes_exact::<16>()?, d.uint()?))
    })
    .map_err(|_| DispatchError::Invalid)?;
    core.bot_group_registered(&op, &group, seq)
        .map_err(map_client)?;
    Ok(empty())
}
fn discard(
    core: &mut ClientCore,
    _bind: Binding,
    payload: &[u8],
) -> Result<Zeroizing<Vec<u8>>, DispatchError> {
    let (op, group) = decode_strict(payload, |d| {
        d.array(3)?;
        let _ = d.uint()?;
        Ok((d.bytes_exact::<16>()?, d.bytes_exact::<16>()?))
    })
    .map_err(|_| DispatchError::Invalid)?;
    core.bot_group_discard(&op, &group).map_err(map_client)?;
    Ok(empty())
}
fn external_join(
    core: &mut ClientCore,
    _bind: Binding,
    payload: &[u8],
) -> Result<Zeroizing<Vec<u8>>, DispatchError> {
    let (op, group, channel, kind, policy, gi, tree) = decode_strict(payload, |d| {
        d.array(8)?;
        let _ = d.uint()?;
        Ok((
            d.bytes_exact::<16>()?,
            d.bytes_exact::<16>()?,
            d.bytes_exact::<16>()?,
            d.uint()?,
            d.uint()?,
            d.bytes()?.to_vec(),
            d.bytes()?.to_vec(),
        ))
    })
    .map_err(|_| DispatchError::Invalid)?;
    if op == [0; 16] || !matches!(kind, 0 | 1) {
        return Err(DispatchError::Invalid);
    }
    let result = core
        .bot_group_external_join(&op, &group, &channel, kind, policy, &gi, &tree)
        .map_err(map_client)?;
    Ok(Zeroizing::new(body(result)))
}
fn joined(
    core: &mut ClientCore,
    _bind: Binding,
    payload: &[u8],
) -> Result<Zeroizing<Vec<u8>>, DispatchError> {
    let (op, group, seq) = decode_strict(payload, |d| {
        d.array(4)?;
        let _ = d.uint()?;
        Ok((d.bytes_exact::<16>()?, d.bytes_exact::<16>()?, d.uint()?))
    })
    .map_err(|_| DispatchError::Invalid)?;
    core.bot_group_joined(&op, &group, seq)
        .map_err(map_client)?;
    Ok(empty())
}
fn welcomes(
    core: &mut ClientCore,
    bind: Binding,
    payload: &[u8],
) -> Result<Zeroizing<Vec<u8>>, DispatchError> {
    let (welcome_bytes, expected_bytes) = decode_strict(payload, |d| {
        d.array(3)?;
        let _ = d.uint()?;
        Ok((d.bytes()?.to_vec(), d.bytes()?.to_vec()))
    })
    .map_err(|_| DispatchError::Invalid)?;
    let welcomes = decode_strict(&welcome_bytes, |d| {
        let n = d.array_len()?;
        let mut rows = Vec::with_capacity(n);
        for _ in 0..n {
            d.array(7)?;
            rows.push((
                d.uint()?,
                d.bytes_exact::<16>()?,
                d.uint()?,
                d.uint()?,
                d.bytes()?.to_vec(),
                d.bytes()?.to_vec(),
                d.bytes_exact::<32>()?,
            ));
        }
        Ok(rows)
    })
    .map_err(|_| DispatchError::Invalid)?;
    let expected = decode_strict(&expected_bytes, |d| {
        let n = d.array_len()?;
        let mut rows = Vec::with_capacity(n);
        for _ in 0..n {
            d.array(5)?;
            rows.push((
                d.bytes_exact::<16>()?,
                d.opt_bytes_exact::<16>()?,
                d.bytes_exact::<16>()?,
                d.uint()?,
                d.uint()?,
            ));
        }
        Ok(rows)
    })
    .map_err(|_| DispatchError::Invalid)?;
    if expected.iter().any(|want| want.1.is_none()) {
        return Err(DispatchError::Unavailable);
    }
    let mut by_group = std::collections::BTreeMap::new();
    for want in &expected {
        if want.0 == [0; 16] || want.2 == [0; 16] || !matches!(want.3, 0 | 1) {
            return Err(DispatchError::Invalid);
        }
        if by_group.insert(want.0, want).is_some() {
            return Err(DispatchError::Invalid);
        }
        if want.1.is_none() {
            return Err(DispatchError::Unavailable);
        }
        if want.1 != Some(bind.1) {
            return Err(DispatchError::Forbidden);
        }
    }
    for welcome in &welcomes {
        if welcome.1 == [0; 16] || !by_group.contains_key(&welcome.1) {
            return Err(DispatchError::Invalid);
        }
    }
    let welcome_groups: std::collections::BTreeSet<_> =
        welcomes.iter().map(|welcome| welcome.1).collect();
    let expected_groups: std::collections::BTreeSet<_> = by_group.keys().copied().collect();
    if welcome_groups != expected_groups {
        return Err(DispatchError::Invalid);
    }
    let mut out = Encoder::new();
    out.array(welcomes.len());
    for welcome in &welcomes {
        let want = by_group
            .get(&welcome.1)
            .expect("prevalidated welcome group");
        let mut one_welcome = Encoder::new();
        one_welcome
            .array(1)
            .array(7)
            .uint(welcome.0)
            .bytes(&welcome.1)
            .uint(welcome.2)
            .uint(welcome.3)
            .bytes(&welcome.4)
            .bytes(&welcome.5)
            .bytes(&welcome.6);
        let mut one_expected = Encoder::new();
        one_expected
            .array(1)
            .array(4)
            .bytes(&want.0)
            .bytes(want.1.as_ref().expect("validated community"))
            .bytes(&want.2)
            .uint(want.4);
        let result = core
            .bot_welcomes_apply(&one_welcome.into_vec(), &one_expected.into_vec(), want.3)
            .map_err(map_client)?;
        let rows = decode_strict(&result, |d| {
            d.array(1)?;
            d.array(4)?;
            Ok((
                d.uint()?,
                d.bytes_exact::<16>()?,
                d.uint()?,
                d.text()?.to_owned(),
            ))
        })
        .map_err(|_| DispatchError::Internal)?;
        out.array(4)
            .uint(rows.0)
            .bytes(&rows.1)
            .uint(rows.2)
            .text(&rows.3);
    }
    Ok(Zeroizing::new(body(out.into_vec())))
}
fn apply(
    core: &mut ClientCore,
    bind: Binding,
    payload: &[u8],
) -> Result<Zeroizing<Vec<u8>>, DispatchError> {
    let (group, hs, messages, through) = decode_strict(payload, |d| {
        d.array(5)?;
        let _ = d.uint()?;
        Ok((
            d.bytes_exact::<16>()?,
            d.bytes()?.to_vec(),
            d.bytes()?.to_vec(),
            d.uint()?,
        ))
    })
    .map_err(|_| DispatchError::Invalid)?;
    group_scope(core, bind, &group)?;
    Ok(Zeroizing::new(body(
        core.group_apply(&group, &hs, &messages, through)
            .map_err(map_client)?,
    )))
}
fn build_commit(
    core: &mut ClientCore,
    bind: Binding,
    payload: &[u8],
) -> Result<Zeroizing<Vec<u8>>, DispatchError> {
    let (op, group, proposals) = decode_strict(payload, |d| {
        d.array(4)?;
        let _ = d.uint()?;
        Ok((
            d.bytes_exact::<16>()?,
            d.bytes_exact::<16>()?,
            d.bytes()?.to_vec(),
        ))
    })
    .map_err(|_| DispatchError::Invalid)?;
    if op == [0; 16] {
        return Err(DispatchError::Invalid);
    }
    group_scope(core, bind, &group)?;
    Ok(Zeroizing::new(body(
        core.bot_commit_build(&op, &group, &proposals)
            .map_err(map_client)?,
    )))
}
fn confirm_commit(
    core: &mut ClientCore,
    _bind: Binding,
    payload: &[u8],
) -> Result<Zeroizing<Vec<u8>>, DispatchError> {
    let (op, group) = decode_strict(payload, |d| {
        d.array(3)?;
        let _ = d.uint()?;
        Ok((d.bytes_exact::<16>()?, d.bytes_exact::<16>()?))
    })
    .map_err(|_| DispatchError::Invalid)?;
    core.bot_commit_confirm(&op, &group).map_err(map_client)?;
    Ok(empty())
}
fn abort_commit(
    core: &mut ClientCore,
    _bind: Binding,
    payload: &[u8],
) -> Result<Zeroizing<Vec<u8>>, DispatchError> {
    let (op, group) = decode_strict(payload, |d| {
        d.array(3)?;
        let _ = d.uint()?;
        Ok((d.bytes_exact::<16>()?, d.bytes_exact::<16>()?))
    })
    .map_err(|_| DispatchError::Invalid)?;
    core.bot_commit_abort(&op, &group).map_err(map_client)?;
    Ok(empty())
}
fn cursor(
    core: &ClientCore,
    bind: Binding,
    payload: &[u8],
) -> Result<Zeroizing<Vec<u8>>, DispatchError> {
    let group = decode_strict(payload, |d| {
        d.array(2)?;
        let _ = d.uint()?;
        d.bytes_exact::<16>()
    })
    .map_err(|_| DispatchError::Invalid)?;
    group_scope(core, bind, &group)?;
    Ok(Zeroizing::new(body(
        core.cursor_body(&group).map_err(map_client)?,
    )))
}
fn revalidate(
    core: &mut ClientCore,
    bind: Binding,
    payload: &[u8],
) -> Result<Zeroizing<Vec<u8>>, DispatchError> {
    let (group, leaf, limit) = decode_strict(payload, |d| {
        d.array(4)?;
        let _ = d.uint()?;
        Ok((d.bytes_exact::<16>()?, d.uint()?, d.uint()?))
    })
    .map_err(|_| DispatchError::Invalid)?;
    if leaf > u32::MAX as u64 || !(1..=256).contains(&limit) {
        return Err(DispatchError::Invalid);
    }
    group_scope(core, bind, &group)?;
    let recovered = core
        .revalidate_pending(&group, leaf as u32, limit as u32)
        .map_err(map_client)?;
    let mut e = Encoder::new();
    e.array(1).uint(recovered as u64);
    Ok(Zeroizing::new(e.into_vec()))
}
fn trust_sender(
    core: &mut ClientCore,
    bind: Binding,
    payload: &[u8],
) -> Result<Zeroizing<Vec<u8>>, DispatchError> {
    let (g, leaf, user, history, heads, now) = decode_strict(payload, |d| {
        d.array(7)?;
        let _ = d.uint()?;
        Ok((
            d.bytes_exact::<16>()?,
            d.uint()?,
            d.bytes_exact::<16>()?,
            d.bytes()?.to_vec(),
            d.bytes()?.to_vec(),
            d.uint()?,
        ))
    })
    .map_err(|_| DispatchError::Invalid)?;
    if leaf > u32::MAX as u64 {
        return Err(DispatchError::Invalid);
    }
    group_scope(core, bind, &g)?;
    let verdict = core
        .device_list_ingest(&g, leaf as u32, &user, &history, &heads, now)
        .map_err(|error| {
            log_private_core_refusal("trust_sender_refusal", error.code);
            map_client(error)
        })?;
    Ok(Zeroizing::new(body(verdict)))
}

fn private_core_error_class(code: &str) -> &'static str {
    match code {
        "E_BINDING" => "binding",
        "E_CREDENTIAL" => "credential",
        "E_DEVICE_LIST_STALE" => "device_list_stale",
        "E_DEVICE_UNLISTED" => "device_unlisted",
        "E_TIER_MISMATCH" => "tier_mismatch",
        "E_UMK_CHANGED" => "umk_changed",
        "E_CORE_INPUT" => "core_input",
        "E_CORE_STATE" => "core_state",
        "E_CORE_STORAGE" => "core_storage",
        _ => "other",
    }
}

fn map_interaction_read(error: ClientError) -> DispatchError {
    if std::env::var_os("DILLA_BOT_PRIVATE_DIAGNOSTICS").as_deref()
        == Some(std::ffi::OsStr::new("1"))
    {
        eprintln!(
            "dilla_bot_helper interaction_read_refusal={}",
            interaction_create_refusal_class(&error)
        );
    }
    map_client(error)
}

fn interaction_create_refusal_class(error: &ClientError) -> &'static str {
    if error.code == "E_CORE_STATE" {
        return match error.detail.as_str() {
            "native Bot binding mismatch" => "binding_mismatch",
            "interaction user-key pin is unavailable" => "user_pin_unavailable",
            "interaction device-list pin is unavailable" => "device_pin_unavailable",
            "group already exists" => "group_exists",
            "participant device already has an interaction group" => "participant_exists",
            "bot upload intent conflicts with an existing operation" => "intent_conflict",
            "interaction group could not be loaded" => "group_load",
            "interaction group is unavailable" => "group_unavailable",
            _ => "core_state",
        };
    }
    if error.code != "E_CORE_INPUT" {
        return private_core_error_class(error.code);
    }
    match error.detail.as_str() {
        "interaction context snapshot rejected" => "snapshot_verification",
        "interaction device-list trust state changed" => "device_trust_changed",
        "interaction snapshot is stale or from the future" => "snapshot_clock",
        "interaction user-key pin is unavailable" => "snapshot_pin_unavailable",
        "interaction create rejected: key_package_decode" => "key_package_decode",
        "interaction create rejected: key_package_kind" => "key_package_kind",
        "interaction create rejected: key_package_validation" => "key_package_validation",
        "interaction create rejected: clock_unavailable" => "clock_unavailable",
        "interaction create rejected: snapshot_expired" => "snapshot_expired",
        "interaction create rejected: welcome_missing" => "welcome_missing",
        "interaction create rejected: candidate_shape" => "candidate_shape",
        _ => "core_input",
    }
}

fn log_private_core_refusal(kind: &'static str, code: &'static str) {
    if std::env::var_os("DILLA_BOT_PRIVATE_DIAGNOSTICS").as_deref()
        != Some(std::ffi::OsStr::new("1"))
    {
        return;
    }
    if matches!(kind, "trust_sender_refusal" | "interaction_create_refusal") {
        eprintln!(
            "dilla_bot_helper {}={}",
            kind,
            private_core_error_class(code)
        );
    }
}
fn proposed_adds(
    core: &mut ClientCore,
    bind: Binding,
    payload: &[u8],
) -> Result<Zeroizing<Vec<u8>>, DispatchError> {
    let (g, proposals) = decode_strict(payload, |d| {
        d.array(3)?;
        let _ = d.uint()?;
        Ok((d.bytes_exact::<16>()?, d.bytes()?.to_vec()))
    })
    .map_err(|_| DispatchError::Invalid)?;
    group_scope(core, bind, &g)?;
    Ok(Zeroizing::new(body(
        core.proposed_adds(&g, &proposals).map_err(map_client)?,
    )))
}
fn trust_add(
    core: &mut ClientCore,
    bind: Binding,
    payload: &[u8],
) -> Result<Zeroizing<Vec<u8>>, DispatchError> {
    let (g, proposals, index, user, history, heads, now) = decode_strict(payload, |d| {
        d.array(8)?;
        let _ = d.uint()?;
        Ok((
            d.bytes_exact::<16>()?,
            d.bytes()?.to_vec(),
            d.uint()?,
            d.bytes_exact::<16>()?,
            d.bytes()?.to_vec(),
            d.bytes()?.to_vec(),
            d.uint()?,
        ))
    })
    .map_err(|_| DispatchError::Invalid)?;
    if index > u32::MAX as u64 {
        return Err(DispatchError::Invalid);
    }
    group_scope(core, bind, &g)?;
    Ok(Zeroizing::new(body(
        core.device_list_ingest_add(&g, &proposals, index, &user, &history, &heads, now)
            .map_err(map_client)?,
    )))
}
fn pending_leaves(
    core: &ClientCore,
    bind: Binding,
    payload: &[u8],
) -> Result<Zeroizing<Vec<u8>>, DispatchError> {
    decode_strict(payload, |d| {
        d.array(1)?;
        let _ = d.uint()?;
        Ok(())
    })
    .map_err(|_| DispatchError::Invalid)?;
    let rows = core.pending_leaves().map_err(map_client)?;
    let filtered = decode_strict(&rows, |d| {
        let n = d.array_len()?;
        let mut out = Vec::new();
        for _ in 0..n {
            d.array(5)?;
            let g = d.bytes_exact::<16>()?;
            let leaf = d.uint()?;
            let user = d.bytes_exact::<16>()?;
            let version = d.uint()?;
            let status = d.text()?.to_owned();
            if group_scope(core, bind, &g).is_ok() {
                out.push((g, leaf, user, version, status));
            }
        }
        Ok(out)
    })
    .map_err(|_| DispatchError::Internal)?;
    let mut e = Encoder::new();
    e.array(filtered.len());
    for (g, l, u, v, s) in filtered {
        e.array(5).bytes(&g).uint(l).bytes(&u).uint(v).text(&s);
    }
    Ok(Zeroizing::new(body(e.into_vec())))
}
fn events_read(
    core: &mut ClientCore,
    bind: Binding,
    payload: &[u8],
) -> Result<Zeroizing<Vec<u8>>, DispatchError> {
    let (after, limit) = decode_strict(payload, |d| {
        d.array(3)?;
        let _ = d.uint()?;
        Ok((d.uint()?, d.uint()?))
    })
    .map_err(|_| DispatchError::Invalid)?;
    validate_bot(core, bind)?;
    Ok(Zeroizing::new(body(
        core.bot_events_read(after, limit).map_err(map_client)?,
    )))
}
fn events_ack(
    core: &mut ClientCore,
    bind: Binding,
    payload: &[u8],
) -> Result<Zeroizing<Vec<u8>>, DispatchError> {
    let through = decode_strict(payload, |d| {
        d.array(2)?;
        let _ = d.uint()?;
        d.uint()
    })
    .map_err(|_| DispatchError::Invalid)?;
    validate_bot(core, bind)?;
    core.bot_events_ack(through).map_err(map_client)?;
    Ok(empty())
}
fn intent_result(
    core: &ClientCore,
    bind: Binding,
    payload: &[u8],
) -> Result<Zeroizing<Vec<u8>>, DispatchError> {
    let op = decode_strict(payload, |d| {
        d.array(2)?;
        let _ = d.uint()?;
        d.bytes_exact::<16>()
    })
    .map_err(|_| DispatchError::Invalid)?;
    validate_bot(core, bind)?;
    Ok(Zeroizing::new(body(
        core.bot_intent_result(&op).map_err(map_client)?,
    )))
}

pub(crate) fn cursor_ack(
    core: &mut ClientCore,
    bind: Binding,
    payload: &[u8],
) -> Result<Zeroizing<Vec<u8>>, DispatchError> {
    validate_bot(core, bind)?;
    let (group, seq, epoch, proof) = decode_strict(payload, |d| match d.array_len()? {
        3 => Ok((d.bytes_exact::<16>()?, d.uint()?, d.uint()?, None)),
        5 => {
            let group = d.bytes_exact::<16>()?;
            let seq = d.uint()?;
            let epoch = d.uint()?;
            let snapshot = Zeroizing::new(d.bytes()?.to_vec());
            if snapshot.is_empty() || snapshot.len() > crate::transfer::MAX_INTERACTION_SNAPSHOT {
                return Err(dilla_core::cbor::CborError::TypeMismatch {
                    expected: "bounded interaction snapshot",
                    offset: 0,
                });
            }
            let expected = interaction_expected(d)?;
            Ok((group, seq, epoch, Some((snapshot, expected))))
        }
        _ => Err(dilla_core::cbor::CborError::TypeMismatch {
            expected: "cursor acknowledgement",
            offset: 0,
        }),
    })
    .map_err(|_| DispatchError::Invalid)?;
    if let Some((snapshot, expected)) = proof {
        if group == [0; 16] || group != *expected.group.as_bytes() || snapshot.is_empty() {
            return Err(DispatchError::Invalid);
        }
        core.bot_ack_interaction_group_cursor(&group, &snapshot, &expected, seq, epoch)
            .map_err(map_interaction_read)?;
    } else {
        group_scope(core, bind, &group)?;
        core.cursor_acked(&group, seq, epoch).map_err(map_client)?;
    }
    Ok(empty())
}

#[cfg(test)]
mod tests {
    use super::*;
    use dilla_core::mls::ConnHandle;
    use std::sync::{Arc, Mutex};

    const INSTANCE: [u8; 16] = [0x11; 16];
    const COMMUNITY: [u8; 16] = [0x22; 16];
    const INSTALLATION: [u8; 16] = [0x33; 16];
    const USER: [u8; 16] = [0x44; 16];
    const BINDING: Binding = (INSTANCE, COMMUNITY, INSTALLATION);

    #[test]
    fn private_core_refusal_diagnostic_is_fixed_and_allowlisted() {
        assert_eq!(
            private_core_error_class("E_DEVICE_LIST_STALE"),
            "device_list_stale"
        );
        assert_eq!(private_core_error_class("E_UMK_CHANGED"), "umk_changed");
        assert_eq!(private_core_error_class("E_CORE_STATE"), "core_state");
        assert_eq!(
            private_core_error_class("arbitrary sensitive text"),
            "other"
        );
        assert_eq!(
            interaction_create_refusal_class(&ClientError {
                code: "E_CORE_INPUT",
                detail: "interaction create rejected: snapshot_expired".into(),
            }),
            "snapshot_expired"
        );
        assert_eq!(
            interaction_create_refusal_class(&ClientError {
                code: "E_CORE_INPUT",
                detail: "private identity material".into(),
            }),
            "core_input"
        );
        for (detail, expected) in [
            (
                "participant device already has an interaction group",
                "participant_exists",
            ),
            ("interaction group could not be loaded", "group_load"),
            ("private identity material", "core_state"),
        ] {
            assert_eq!(
                interaction_create_refusal_class(&ClientError {
                    code: "E_CORE_STATE",
                    detail: detail.into(),
                }),
                expected
            );
        }
    }

    fn bot() -> ClientCore {
        let connection: ConnHandle = Arc::new(Mutex::new(
            rusqlite::Connection::open_in_memory().expect("sqlite"),
        ));
        let mut core = ClientCore::open(connection).expect("open core");
        core.bot_bootstrap_begin(&INSTANCE, &COMMUNITY, &INSTALLATION, &[0x77; 32])
            .expect("bootstrap bot");
        core.bot_bootstrap_complete(&USER, "adapter bot", 1_800_000_000)
            .expect("complete bot");
        core
    }

    #[test]
    fn inspect_refuses_a_native_group_bound_to_another_community() {
        let mut core = bot();
        let foreign = [0x55; 16];
        let group = [0x66; 16];
        core.group_create(&group, Some(&foreign), &[0x77; 16], 0, &[0x88; 32])
            .expect("construct a foreign-scope group fixture");
        let mut request = Encoder::new();
        request.array(2).uint(1).bytes(&group);
        assert!(matches!(
            dispatch(&mut core, BINDING, &request.into_vec()),
            Err(DispatchError::Forbidden)
        ));
    }

    #[test]
    fn interaction_discovery_does_not_widen_generic_group_scope() {
        let mut core = bot();
        let group = [0x61; 16];
        core.group_create(&group, Some(&COMMUNITY), &[0x62; 16], 0, &[0x88; 32])
            .expect("construct ordinary group");

        let mut discover = Encoder::new();
        discover.array(1).uint(23);
        let discovered = dispatch(&mut core, BINDING, &discover.into_vec()).expect("discover");
        let rows = decode_strict(&discovered, |d| {
            d.array(1)?;
            let bytes = d.bytes()?;
            decode_strict(bytes, |nested| {
                let n = nested.array_len()?;
                for _ in 0..n {
                    nested.skip()?;
                }
                Ok(n)
            })
        })
        .expect("descriptor response");
        assert_eq!(rows, 0, "ordinary group leaked into interaction discovery");

        let mut generic = Encoder::new();
        generic.array(1).uint(0);
        let listed = dispatch(&mut core, BINDING, &generic.into_vec()).expect("generic list");
        let count = decode_strict(&listed, |d| {
            d.array(1)?;
            let bytes = d.bytes()?;
            decode_strict(bytes, |nested| {
                let n = nested.array_len()?;
                for _ in 0..n {
                    nested.skip()?;
                }
                Ok(n)
            })
        })
        .expect("group response");
        assert_eq!(count, 1, "generic group list changed");
    }

    #[test]
    fn interaction_send_prepare_wraps_core_result_for_group_transfer_decoder() {
        let mut core_result = Encoder::new();
        core_result
            .array(2)
            .bytes(&[0x41; 16])
            .bytes(&[0x82, 0x00, 0x41]);
        let core_result = core_result.into_vec();

        let helper_result = interaction_send_prepare_response(core_result.clone());
        let wrapped = decode_strict(&helper_result, |d| {
            d.array(1)?;
            Ok(d.bytes()?.to_vec())
        })
        .expect("helper response envelope");
        assert_eq!(wrapped, core_result);
        decode_strict(&wrapped, |d| {
            d.array(2)?;
            assert_eq!(d.bytes_exact::<16>()?, [0x41; 16]);
            assert_eq!(d.bytes()?, &[0x82, 0x00, 0x41]);
            Ok(())
        })
        .expect("core send result");
    }

    #[test]
    fn proof_bearing_cursor_ack_refuses_ordinary_group_while_legacy_ack_remains_scoped() {
        let mut core = bot();
        let group = [0x71; 16];
        core.group_create(&group, Some(&COMMUNITY), &[0x72; 16], 0, &[0x88; 32])
            .expect("construct ordinary group");
        core.group_registered(&group, 2)
            .expect("activate ordinary group");

        let mut snapshot = Encoder::new();
        snapshot.array(2).bytes(&[0x80]).bytes(&[0; 64]);
        let snapshot = snapshot.into_vec();
        let mut expected = Encoder::new();
        expected
            .array(11)
            .bytes(&INSTANCE)
            .bytes(&INSTALLATION)
            .bytes(&COMMUNITY)
            .bytes(&USER)
            .bytes(&[0x73; 16])
            .bytes(&[0x74; 16])
            .bytes(&[0x75; 16])
            .bytes(&group)
            .uint(1)
            .uint(1)
            .uint(1);

        let mut proved_ack = Encoder::new();
        proved_ack
            .array(5)
            .bytes(&group)
            .uint(1)
            .uint(0)
            .bytes(&snapshot)
            .raw(&expected.into_vec());
        assert!(
            cursor_ack(&mut core, BINDING, &proved_ack.into_vec()).is_err(),
            "proof-bearing interaction ack must refuse kind 0 even when legacy ack is valid"
        );

        let mut legacy_ack = Encoder::new();
        legacy_ack.array(3).bytes(&group).uint(1).uint(0);
        assert!(
            cursor_ack(&mut core, BINDING, &legacy_ack.into_vec()).is_ok(),
            "legacy cursor ack remains available for its generic kind-0 scope"
        );
    }

    #[test]
    fn malformed_interaction_snapshot_leaves_no_group_or_intent_state() {
        let mut core = bot();
        let operation = [0x51; 16];
        let group = [0x52; 16];
        let bot_device = decode_strict(&core.identity().expect("identity"), |d| {
            d.array(6)?;
            d.skip()?;
            d.skip()?;
            d.skip()?;
            let device =
                d.opt_bytes_exact::<16>()?
                    .ok_or(dilla_core::cbor::CborError::TypeMismatch {
                        expected: "Bot device ID",
                        offset: d.position(),
                    })?;
            d.skip()?;
            d.skip()?;
            Ok(device)
        })
        .expect("Bot device");
        let mut request = Encoder::new();
        request
            .array(6)
            .uint(21)
            .bytes(&operation)
            .bytes(&group)
            .bytes(&[0x80]);
        request
            .array(11)
            .bytes(&INSTANCE)
            .bytes(&INSTALLATION)
            .bytes(&COMMUNITY)
            .bytes(&USER)
            .bytes(&bot_device)
            .bytes(&[0x66; 16])
            .bytes(&[0x77; 16])
            .bytes(&group)
            .uint(1)
            .uint(1)
            .uint(1)
            .bytes(&[0x01]);

        assert!(matches!(
            dispatch(&mut core, BINDING, &request.into_vec()),
            Err(DispatchError::Invalid)
        ));
        let groups = decode_strict(&core.groups().expect("groups"), |d| {
            let n = d.array_len()?;
            for _ in 0..n {
                d.skip()?;
            }
            Ok(n)
        })
        .expect("group list");
        assert_eq!(groups, 0);
        let intent = core.bot_intent_result(&operation).expect("intent lookup");
        decode_strict(&intent, |d| {
            d.array(1)?;
            assert!(d.try_null()?);
            Ok(())
        })
        .expect("no persisted intent");
    }

    #[test]
    fn malformed_direct_create_and_interaction_create_leave_no_group() {
        let mut core = bot();
        let mut malformed = Encoder::new();
        malformed.array(3).uint(2).bytes(&[1; 16]);
        assert!(matches!(
            dispatch(&mut core, BINDING, &malformed.into_vec()),
            Err(DispatchError::Invalid)
        ));

        let mut interaction = Encoder::new();
        interaction
            .array(7)
            .uint(2)
            .bytes(&[0x31; 16])
            .bytes(&[0x41; 16])
            .bytes(&[0x51; 16])
            .uint(2)
            .uint(0)
            .bytes(&[0x88; 32]);
        assert!(matches!(
            dispatch(&mut core, BINDING, &interaction.into_vec()),
            Err(DispatchError::Invalid)
        ));
        let groups = decode_strict(&core.groups().expect("groups"), |d| {
            let count = d.array_len()?;
            for _ in 0..count {
                d.skip()?;
            }
            Ok(count)
        })
        .expect("group list");
        assert_eq!(groups, 0);
    }

    #[test]
    fn helper_direct_group_shapes_match_transfer_validation_before_native_mutation() {
        let mut core = bot();
        let mut zero_group = Encoder::new();
        zero_group
            .array(7)
            .uint(2)
            .bytes(&[0x31; 16])
            .bytes(&[0; 16])
            .bytes(&[0x51; 16])
            .uint(0)
            .uint(0)
            .bytes(&[0x88; 32]);
        let mut null_external_sender = Encoder::new();
        null_external_sender
            .array(7)
            .uint(2)
            .bytes(&[0x32; 16])
            .bytes(&[0x42; 16])
            .bytes(&[0x52; 16])
            .uint(0)
            .uint(0)
            .bytes(&[0; 32]);
        let mut interaction_join = Encoder::new();
        interaction_join
            .array(8)
            .uint(5)
            .bytes(&[0x33; 16])
            .bytes(&[0x43; 16])
            .bytes(&[0x53; 16])
            .uint(3)
            .uint(0)
            .bytes(&[1])
            .bytes(&[1]);
        for invalid in [
            zero_group.into_vec(),
            null_external_sender.into_vec(),
            interaction_join.into_vec(),
        ] {
            assert!(matches!(
                dispatch(&mut core, BINDING, &invalid),
                Err(DispatchError::Invalid)
            ));
        }
        let groups = decode_strict(&core.groups().expect("groups"), |d| {
            let n = d.array_len()?;
            for _ in 0..n {
                d.skip()?;
            }
            Ok(n)
        })
        .expect("group list");
        assert_eq!(groups, 0);
    }

    #[test]
    fn interaction_welcome_operation_is_explicitly_unavailable() {
        let mut core = bot();
        let mut welcomes = Encoder::new();
        welcomes.array(0);
        let mut expected = Encoder::new();
        expected
            .array(1)
            .array(5)
            .bytes(&[0x61; 16])
            .null()
            .bytes(&[0x62; 16])
            .uint(2)
            .uint(0);
        let mut welcome = Encoder::new();
        welcome
            .array(3)
            .uint(7)
            .bytes(&welcomes.into_vec())
            .bytes(&expected.into_vec());
        assert!(matches!(
            dispatch(&mut core, BINDING, &welcome.into_vec()),
            Err(DispatchError::Unavailable)
        ));
    }
}
