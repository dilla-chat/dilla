//! The call routes' answers and the core's call bodies, decoded as the TypeScript client decodes
//! them (`packages/client-core/src/http/routes.ts`: an answer may carry more elements than it reads;
//! every element it reads is checked).

use ciborium::value::Value;
use dilla_core::cbor::decode_strict;
use zeroize::Zeroizing;

use super::{MediaEpoch, RouteError};

/// L-TS-55: the most participants one call answer may list.
pub const CALL_PARTICIPANTS_MAX: usize = 1024;

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct CallParticipant {
    pub user_id: [u8; 16],
    pub device_id: [u8; 16],
    pub flags: u8,
}

/// L-HTTP-100: the channel's call group (none before the first join), its live call and who is in
/// it.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ChannelCall {
    pub group_id: Option<[u8; 16]>,
    pub call_id: Option<[u8; 16]>,
    pub participants: Vec<CallParticipant>,
}

/// L-HTTP-102: the call this device started or joined, and its LiveKit room.
#[derive(Clone)]
pub struct CallStarted {
    /// 201: this start opened the call.
    pub created: bool,
    pub call_id: [u8; 16],
    pub group_id: [u8; 16],
    pub livekit_url: String,
    pub token: Zeroizing<String>,
    /// `(urls, username, credential)`.
    pub ice_servers: Vec<(Vec<String>, String, Zeroizing<String>)>,
}

impl std::fmt::Debug for CallStarted {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("CallStarted")
            .field("created", &self.created)
            .field("livekit_url", &self.livekit_url)
            .finish_non_exhaustive()
    }
}

/// One row of `ClientCore::groups`.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct GroupSummary {
    pub group_id: [u8; 16],
    pub kind: u64,
    pub target_id: [u8; 16],
    pub state: u64,
    pub epoch: u64,
    /// This device's cursor: the first seq it has not applied.
    pub next_seq: u64,
    /// A commit of this device is pending (built, neither confirmed nor aborted).
    pub pending_commit: bool,
}

/// The handshake kinds that take an epoch (`KindCommit`, `KindExternalJoin`); 0 is a proposal.
const COMMIT_KINDS: [u64; 2] = [1, 2];

/// A `GET /v1/groups/{id}/handshakes` page (rows `[seq, epoch, kind, sender|null, blob]`, in seq
/// order), as a rotation's reconciliation reads it.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct HandshakePage {
    /// The page's first commit: its seq and blob. Read from this device's cursor while a commit of
    /// this device is pending, it is the commit that took that epoch.
    pub first_commit: Option<(u64, Vec<u8>)>,
    pub rows: usize,
    /// The seq of the page's last row.
    pub last: Option<u64>,
}

pub fn decode_handshake_page(body: &[u8]) -> Result<HandshakePage, RouteError> {
    let rows = match parse(body)? {
        Value::Array(rows) => rows,
        _ => return Err(shape("handshake page")),
    };
    let mut page = HandshakePage {
        rows: rows.len(),
        ..HandshakePage::default()
    };
    for row in rows {
        let r = at_least(row, 5, "handshake row")?;
        let seq = uint(&r[0], "handshake seq")?;
        page.last = Some(seq);
        if page.first_commit.is_none() && COMMIT_KINDS.contains(&uint(&r[2], "handshake kind")?) {
            match &r[4] {
                Value::Bytes(b) => page.first_commit = Some((seq, b.clone())),
                _ => return Err(shape("handshake blob")),
            }
        }
    }
    Ok(page)
}

/// A commit body the core built (`commit_build`/`call_rotate`: `[epoch, commit, group_info,
/// welcomes, null]`): the epoch it commits from and the MLS commit the instance stores as the
/// handshake's blob.
pub fn decode_commit_body(body: &[u8]) -> Result<(u64, Vec<u8>), String> {
    decode_strict(body, |d| {
        d.array(5)?;
        let epoch = d.uint()?;
        let commit = d.bytes()?.to_vec();
        d.bytes()?;
        let n = d.array_len()?;
        for _ in 0..n {
            d.array(2)?;
            d.bytes()?;
            d.bytes()?;
        }
        d.null()?;
        Ok((epoch, commit))
    })
    .map_err(|e| format!("E_CBOR_SHAPE: {e:?}"))
}

fn shape(what: &str) -> RouteError {
    RouteError::decode(format!("E_CBOR_SHAPE: {what}"))
}

fn parse(body: &[u8]) -> Result<Value, RouteError> {
    ciborium::from_reader::<Value, _>(body).map_err(|e| shape(&e.to_string()))
}

fn at_least(v: Value, n: usize, what: &str) -> Result<Vec<Value>, RouteError> {
    match v {
        Value::Array(a) if a.len() >= n => Ok(a),
        _ => Err(shape(what)),
    }
}

fn bin16(v: &Value, what: &str) -> Result<[u8; 16], RouteError> {
    match v {
        Value::Bytes(b) => <[u8; 16]>::try_from(b.as_slice()).map_err(|_| shape(what)),
        _ => Err(shape(what)),
    }
}

fn opt_bin16(v: &Value, what: &str) -> Result<Option<[u8; 16]>, RouteError> {
    match v {
        Value::Null => Ok(None),
        other => bin16(other, what).map(Some),
    }
}

fn uint(v: &Value, what: &str) -> Result<u64, RouteError> {
    match v {
        Value::Integer(i) => u64::try_from(*i).map_err(|_| shape(what)),
        _ => Err(shape(what)),
    }
}

fn text(v: &Value, what: &str) -> Result<String, RouteError> {
    match v {
        Value::Text(t) => Ok(t.clone()),
        _ => Err(shape(what)),
    }
}

pub fn decode_channel_call(body: &[u8]) -> Result<ChannelCall, RouteError> {
    let a = at_least(parse(body)?, 3, "channel call")?;
    let rows = match &a[2] {
        Value::Array(rows) if rows.len() <= CALL_PARTICIPANTS_MAX => rows,
        _ => return Err(shape("participants")),
    };
    let mut participants = Vec::with_capacity(rows.len());
    for row in rows {
        let r = at_least(row.clone(), 3, "participant")?;
        let flags = uint(&r[2], "flags")?;
        if flags > 31 {
            return Err(shape("voice flags above 31"));
        }
        participants.push(CallParticipant {
            user_id: bin16(&r[0], "user id")?,
            device_id: bin16(&r[1], "device id")?,
            flags: flags as u8,
        });
    }
    Ok(ChannelCall {
        group_id: opt_bin16(&a[0], "group id")?,
        call_id: opt_bin16(&a[1], "call id")?,
        participants,
    })
}

/// The `seq` (or `next_seq`) at `index` of a `[...]` answer.
pub fn decode_seq(body: &[u8], index: usize) -> Result<u64, RouteError> {
    let a = at_least(parse(body)?, index + 1, "seq answer")?;
    uint(&a[index], "seq")
}

/// The start's body: `[]`, or `[vdec]` naming the decoders.
pub fn start_call_body(vdec: &str) -> Vec<u8> {
    let mut e = dilla_core::cbor::Encoder::new();
    if vdec.is_empty() {
        e.array(0);
    } else {
        e.array(1).text(vdec);
    }
    e.into_vec()
}

pub fn decode_call_started(status: u16, body: &[u8]) -> Result<CallStarted, RouteError> {
    let a = at_least(parse(body)?, 6, "call started")?;
    let mut ice_servers = Vec::new();
    match &a[4] {
        Value::Array(servers) => {
            for s in servers {
                let s = at_least(s.clone(), 3, "ice server")?;
                let urls = match &s[0] {
                    Value::Array(u) => u
                        .iter()
                        .map(|u| text(u, "ice url"))
                        .collect::<Result<_, _>>()?,
                    _ => return Err(shape("ice urls")),
                };
                ice_servers.push((
                    urls,
                    text(&s[1], "ice username")?,
                    Zeroizing::new(text(&s[2], "ice credential")?),
                ));
            }
        }
        _ => return Err(shape("ice servers")),
    }
    Ok(CallStarted {
        created: status == 201,
        call_id: bin16(&a[0], "call id")?,
        group_id: bin16(&a[1], "group id")?,
        livekit_url: text(&a[2], "livekit url")?,
        token: Zeroizing::new(text(&a[3], "token")?),
        ice_servers,
    })
}

/// `ClientCore::call_media_epoch`: `[epoch, base_key(16), own_leaf, [[leaf, device_id, user_id]],
/// code]`, decoded strictly. The key is copied once, from the (caller's zeroizing) body straight
/// into the zeroizing buffer the answer holds: no temporary array of it is left on the stack, and a
/// refused body drops that buffer zeroized.
pub fn decode_media_epoch(body: &[u8]) -> Result<MediaEpoch, String> {
    let mut base_key = Zeroizing::new([0u8; 16]);
    let (epoch, own_leaf, roster, code) = decode_strict(body, |d| {
        d.array(5)?;
        let epoch = d.uint()?;
        let key = d.bytes()?;
        if key.len() != 16 {
            return Err(dilla_core::cbor::CborError::WrongByteLen {
                expected: 16,
                actual: key.len(),
            });
        }
        base_key.copy_from_slice(key);
        let own_leaf = d.uint()?;
        let n = d.array_len()?;
        let mut roster = Vec::with_capacity(n.min(CALL_PARTICIPANTS_MAX));
        for _ in 0..n {
            d.array(3)?;
            let leaf = d.uint()?;
            roster.push((leaf, d.bytes_exact::<16>()?, d.bytes_exact::<16>()?));
        }
        let code = d.text()?.to_owned();
        Ok((epoch, own_leaf, roster, code))
    })
    .map_err(|e| format!("E_CBOR_SHAPE: {e:?}"))?;
    let leaf = |l: u64| u16::try_from(l).map_err(|_| "E_CBOR_RANGE: leaf".to_owned());
    Ok(MediaEpoch {
        epoch,
        base_key,
        own_leaf: leaf(own_leaf)?,
        roster: roster
            .into_iter()
            .map(|(l, d, u)| Ok((leaf(l)?, d, u)))
            .collect::<Result<_, String>>()?,
        code,
    })
}

/// `ClientCore::groups`: `[[group_id, kind, community_id|null, target_id, state, epoch, next_seq,
/// proposals_pending, pending_commit]]`.
pub fn decode_groups(body: &[u8]) -> Result<Vec<GroupSummary>, String> {
    decode_strict(body, |d| {
        let n = d.array_len()?;
        let mut out = Vec::with_capacity(n);
        for _ in 0..n {
            d.array(9)?;
            let group_id = d.bytes_exact::<16>()?;
            let kind = d.uint()?;
            d.opt_bytes_exact::<16>()?;
            let target_id = d.bytes_exact::<16>()?;
            let state = d.uint()?;
            let epoch = d.uint()?;
            let next_seq = d.uint()?;
            d.uint()?;
            let pending_commit = d.uint()? == 1;
            out.push(GroupSummary {
                group_id,
                kind,
                target_id,
                state,
                epoch,
                next_seq,
                pending_commit,
            });
        }
        Ok(out)
    })
    .map_err(|e| format!("E_CBOR_SHAPE: {e:?}"))
}

/// `call_leave_proposal`'s answer: `[epoch, proposal]`; the proposal route takes it as it is.
pub fn is_epoch_proposal(body: &[u8]) -> bool {
    decode_strict(body, |d| {
        d.array(2)?;
        d.uint()?;
        d.skip()?;
        Ok(())
    })
    .is_ok()
}
