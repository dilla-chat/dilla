//! The ordering half of the engine (engine.ts `throughOf`, `frameJob`'s row building, channel.ts `welcomeFrame`): how a
//! live frame or a catch-up page becomes the `group_apply(handshakes, messages, through)` call the core orders and
//! de-duplicates by seq. Pure functions, unit-tested against engine.test.ts's cases.

use crate::gateway::frames::{Frame, Op, Value};

use super::value::{DecodeError, arr, uint};

/// engine.ts `SYNC`.
pub const HANDSHAKE_PAGE: usize = 512;
pub const MESSAGE_PAGE: usize = 256;

/// routes.ts `PageInfo`: a page's row count and its last row's seq.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct PageInfo {
    pub count: usize,
    pub last_seq: Option<u64>,
}

/// engine.ts `throughOf`: how far a catch-up step may apply. A full page bounds the step at its own last row (more may
/// follow); a short message page lets the handshakes' carry from the previous step raise it, and a short handshake page
/// is bounded by the messages, so a handshake is never applied past a message the server still has to send.
pub fn through_of(ms: PageInfo, hs: PageInfo, carry: Option<u64>) -> u64 {
    let m = ms.last_seq.unwrap_or(0);
    let h = hs.last_seq.unwrap_or(0);
    let bound_m = if ms.count == MESSAGE_PAGE {
        m
    } else {
        m.max(carry.unwrap_or(0))
    };
    let bound_h = if hs.count == HANDSHAKE_PAGE {
        h
    } else {
        h.max(m)
    };
    bound_m.min(bound_h)
}

/// What a live frame hands the core: one handshake row or one message row, encoded as a one-row page, and its seq.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct LiveRow {
    pub seq: u64,
    pub handshakes: Vec<u8>,
    pub messages: Vec<u8>,
}

fn empty() -> Vec<u8> {
    Value::Array(Vec::new()).encode()
}

/// engine.ts `frameJob`'s shape checks and row building. A `handshake` (op 16) payload is
/// `[seq, epoch, kind, sender_leaf|null, mls_message]` and becomes the handshake page `[payload]`. A `message.ct` (op 19)
/// payload is `[seq, epoch, sender_device, mls_message, msg_hash, recv_ts]` (a 7th element, the upload class 0, 1 or
/// null, since web3.7) and becomes the message row `[seq, epoch, sender, ct, null, hash, recv_ts, 0(, class)]`.
pub fn live_row(f: &Frame) -> Result<LiveRow, DecodeError> {
    let p = &f.payload;
    if f.op == Op::HANDSHAKE {
        if p.len() != 5 {
            return Err(DecodeError("a handshake has 5 elements"));
        }
        let seq = uint(&p[0])?;
        uint(&p[1])?;
        uint(&p[2])?;
        if !matches!(p[3], Value::Null) {
            uint(&p[3])?;
        }
        p[4].as_bytes().ok_or(DecodeError("handshake message"))?;
        return Ok(LiveRow {
            seq,
            handshakes: Value::Array(vec![Value::Array(p.clone())]).encode(),
            messages: empty(),
        });
    }
    if f.op != Op::MESSAGE_CT {
        return Err(DecodeError("not a group row"));
    }
    if p.len() != 6 && p.len() != 7 {
        return Err(DecodeError("message.ct has 6 or 7 elements"));
    }
    let seq = uint(&p[0])?;
    uint(&p[1])?;
    if p[2].as_bytes().is_none_or(|b| b.len() != 16)
        || p[3].as_bytes().is_none()
        || p[4].as_bytes().is_none_or(|b| b.len() != 32)
    {
        return Err(DecodeError("message.ct fields"));
    }
    uint(&p[5])?;
    let mut row = vec![
        p[0].clone(),
        p[1].clone(),
        p[2].clone(),
        p[3].clone(),
        Value::Null,
        p[4].clone(),
        p[5].clone(),
        Value::Uint(0),
    ];
    if let Some(class) = p.get(6) {
        if !matches!(class, Value::Null | Value::Uint(0) | Value::Uint(1)) {
            return Err(DecodeError("class is 0, 1 or null"));
        }
        row.push(class.clone());
    }
    Ok(LiveRow {
        seq,
        handshakes: empty(),
        messages: Value::Array(vec![Value::Array(row)]).encode(),
    })
}

/// channel.ts `welcomeFrame`: a `welcome` (op 20) payload `[id, epoch, seq, blob, tree, hash]` as the one-row welcomes
/// page `[[id, group_id, epoch, seq, blob, tree, hash]]`.
pub fn welcome_page(f: &Frame) -> Result<Vec<u8>, DecodeError> {
    let g = f.group_id.ok_or(DecodeError("a welcome names its group"))?;
    let p = arr_of(&f.payload, 6)?;
    uint(&p[0])?;
    uint(&p[1])?;
    uint(&p[2])?;
    p[3].as_bytes().ok_or(DecodeError("welcome blob"))?;
    p[4].as_bytes().ok_or(DecodeError("welcome tree"))?;
    if p[5].as_bytes().is_none_or(|b| b.len() != 32) {
        return Err(DecodeError("welcome hash"));
    }
    Ok(Value::Array(vec![Value::Array(vec![
        p[0].clone(),
        Value::Bytes(g.to_vec()),
        p[1].clone(),
        p[2].clone(),
        p[3].clone(),
        p[4].clone(),
        p[5].clone(),
    ])])
    .encode())
}

fn arr_of(p: &[Value], n: usize) -> Result<&[Value], DecodeError> {
    if p.len() == n {
        Ok(p)
    } else {
        Err(DecodeError("wrong payload length"))
    }
}

/// core-port.ts `ApplyResult`: `[state, epoch, next_seq, new_seqs, proposals_pending, flags]` (flags: 1 the epoch changed,
/// 2 an own upload was adopted).
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct ApplyResult {
    pub state: u8,
    pub epoch: u64,
    pub next_seq: u64,
    pub new_seqs: Vec<u64>,
    pub proposals_pending: u64,
    pub epoch_changed: bool,
    pub own_adopted: bool,
}

impl ApplyResult {
    pub fn decode(raw: &[u8]) -> Result<Self, DecodeError> {
        let v = super::value::decode(raw)?;
        let a = arr(&v, 6)?;
        let state = uint(&a[0])?;
        if state > 4 {
            return Err(DecodeError("group state"));
        }
        let flags = uint(&a[5])?;
        Ok(Self {
            state: state as u8,
            epoch: uint(&a[1])?,
            next_seq: uint(&a[2])?,
            new_seqs: a[3]
                .as_array()
                .ok_or(DecodeError("new seqs"))?
                .iter()
                .map(uint)
                .collect::<Result<_, _>>()?,
            proposals_pending: super::value::u53(&a[4])?,
            epoch_changed: flags & 1 != 0,
            own_adopted: flags & 2 != 0,
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn p(count: usize, last: Option<u64>) -> PageInfo {
        PageInfo {
            count,
            last_seq: last,
        }
    }

    #[test]
    fn through_follows_engine_ts() {
        // Both pages short: everything served is applied (the handshakes bounded by the messages' last seq).
        assert_eq!(through_of(p(3, Some(12)), p(1, Some(10)), None), 12);
        // A handshake past the last message waits for the next step, which the carry then lets through.
        assert_eq!(through_of(p(1, Some(5)), p(1, Some(9)), None), 5);
        assert_eq!(through_of(p(0, None), p(1, Some(9)), Some(9)), 9);
        // A full message page: never past its own last row.
        assert_eq!(
            through_of(p(MESSAGE_PAGE, Some(300)), p(1, Some(400)), None),
            300
        );
        // A full handshake page: never past its last row, even when messages go further.
        assert_eq!(
            through_of(p(2, Some(900)), p(HANDSHAKE_PAGE, Some(700)), None),
            700
        );
        // An empty message page after a step whose handshakes went further: the carry lets the next step finish them.
        assert_eq!(through_of(p(0, None), p(0, None), Some(42)), 0);
        assert_eq!(through_of(p(0, None), p(1, Some(43)), Some(42)), 42);
        // Nothing at all.
        assert_eq!(through_of(p(0, None), p(0, None), None), 0);
    }

    fn frame(op: u8, payload: Vec<Value>) -> Frame {
        Frame {
            op,
            n: 7,
            group_id: Some([3; 16]),
            payload,
        }
    }

    #[test]
    fn message_frames_become_one_message_row() {
        let base = vec![
            Value::Uint(5),
            Value::Uint(2),
            Value::Bytes(vec![9; 16]),
            Value::Bytes(vec![1, 2, 3]),
            Value::Bytes(vec![4; 32]),
            Value::Uint(1_700_000_000),
        ];
        let row = live_row(&frame(Op::MESSAGE_CT, base.clone())).unwrap();
        assert_eq!(row.seq, 5);
        assert_eq!(row.handshakes, Value::Array(vec![]).encode());
        let want = Value::Array(vec![Value::Array(vec![
            Value::Uint(5),
            Value::Uint(2),
            Value::Bytes(vec![9; 16]),
            Value::Bytes(vec![1, 2, 3]),
            Value::Null,
            Value::Bytes(vec![4; 32]),
            Value::Uint(1_700_000_000),
            Value::Uint(0),
        ])]);
        assert_eq!(row.messages, want.encode());
        // The web3.7 class rides along as the row's 9th element; anything but 0, 1 or null is refused.
        let mut seven = base.clone();
        seven.push(Value::Uint(1));
        let row = live_row(&frame(Op::MESSAGE_CT, seven.clone())).unwrap();
        assert_eq!(
            Value::decode(&row.messages).unwrap().as_array().unwrap()[0]
                .as_array()
                .unwrap()
                .len(),
            9
        );
        seven[6] = Value::Uint(2);
        assert!(live_row(&frame(Op::MESSAGE_CT, seven)).is_err());
        let mut short = base;
        short[4] = Value::Bytes(vec![4; 31]);
        assert!(live_row(&frame(Op::MESSAGE_CT, short)).is_err());
    }

    #[test]
    fn handshake_frames_become_one_handshake_row() {
        let payload = vec![
            Value::Uint(8),
            Value::Uint(3),
            Value::Uint(1),
            Value::Null,
            Value::Bytes(vec![7; 10]),
        ];
        let row = live_row(&frame(Op::HANDSHAKE, payload.clone())).unwrap();
        assert_eq!(row.seq, 8);
        assert_eq!(row.messages, Value::Array(vec![]).encode());
        assert_eq!(
            row.handshakes,
            Value::Array(vec![Value::Array(payload)]).encode()
        );
    }

    #[test]
    fn apply_results_read_their_flags() {
        let raw = Value::Array(vec![
            Value::Uint(2),
            Value::Uint(4),
            Value::Uint(11),
            Value::Array(vec![Value::Uint(9), Value::Uint(10)]),
            Value::Uint(0),
            Value::Uint(3),
        ])
        .encode();
        let r = ApplyResult::decode(&raw).unwrap();
        assert_eq!(
            (r.state, r.epoch, r.next_seq, r.new_seqs.clone()),
            (2, 4, 11, vec![9, 10])
        );
        assert!(r.epoch_changed && r.own_adopted);
    }
}
