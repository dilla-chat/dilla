//! Readers over the strict dilla CBOR subset ([`Value`]) for answers the sync engine and the state slices read: the
//! same shape checks routes.ts and core-port.ts make (`arr`, `atLeast`, `bin`, `u64`, `u53`, `str`, `opt`). The
//! normative decoders stay in dilla-core; these only read what the instance or the core already produced.

use crate::gateway::frames::{MAX_SAFE_INTEGER, Value};

/// A shape the reader refused; the text names what was wrong, never the bytes.
#[derive(Clone, Copy, Debug, PartialEq, Eq, thiserror::Error)]
#[error("undecodable: {0}")]
pub struct DecodeError(pub &'static str);

/// A decoded top-level array of rows.
pub(crate) struct Rows(Vec<Value>);

impl Rows {
    pub fn of(raw: &[u8]) -> Result<Self, DecodeError> {
        match Value::decode(raw).map_err(|_| DecodeError("not CBOR"))? {
            Value::Array(rows) => Ok(Self(rows)),
            _ => Err(DecodeError("not an array")),
        }
    }

    pub fn iter(&self) -> std::slice::Iter<'_, Value> {
        self.0.iter()
    }
}

pub(crate) fn decode(raw: &[u8]) -> Result<Value, DecodeError> {
    Value::decode(raw).map_err(|_| DecodeError("not CBOR"))
}

/// An array of exactly `n` elements.
pub(crate) fn arr(v: &Value, n: usize) -> Result<&[Value], DecodeError> {
    match v.as_array() {
        Some(a) if a.len() == n => Ok(a),
        _ => Err(DecodeError("an array of the wrong length")),
    }
}

/// An array of at least `n` elements (an answer a newer instance may extend).
pub(crate) fn at_least(v: &Value, n: usize) -> Result<&[Value], DecodeError> {
    match v.as_array() {
        Some(a) if a.len() >= n => Ok(a),
        _ => Err(DecodeError("an array too short")),
    }
}

pub(crate) fn uint(v: &Value) -> Result<u64, DecodeError> {
    v.as_uint().ok_or(DecodeError("not an unsigned integer"))
}

/// An integer a JS number holds exactly (u53).
pub(crate) fn u53(v: &Value) -> Result<u64, DecodeError> {
    uint(v).and_then(|n| {
        if n <= MAX_SAFE_INTEGER {
            Ok(n)
        } else {
            Err(DecodeError("past the safe integers"))
        }
    })
}

pub(crate) fn bytes(v: &Value) -> Result<Vec<u8>, DecodeError> {
    v.as_bytes()
        .map(<[u8]>::to_vec)
        .ok_or(DecodeError("not a byte string"))
}

pub(crate) fn bin16(v: &Value) -> Result<[u8; 16], DecodeError> {
    v.as_bytes()
        .and_then(|b| <[u8; 16]>::try_from(b).ok())
        .ok_or(DecodeError("not a 16-byte id"))
}

pub(crate) fn bin32(v: &Value) -> Result<[u8; 32], DecodeError> {
    v.as_bytes()
        .and_then(|b| <[u8; 32]>::try_from(b).ok())
        .ok_or(DecodeError("not 32 bytes"))
}

pub(crate) fn text(v: &Value) -> Result<String, DecodeError> {
    v.as_text()
        .map(str::to_owned)
        .ok_or(DecodeError("not a text string"))
}

/// `null` or what `f` reads.
pub(crate) fn opt<T>(
    v: &Value,
    f: impl FnOnce(&Value) -> Result<T, DecodeError>,
) -> Result<Option<T>, DecodeError> {
    match v {
        Value::Null => Ok(None),
        v => f(v).map(Some),
    }
}

/// routes.ts `page`: the rows of a page, their count, and the first element of the last row (its seq or id).
pub struct Page {
    pub raw: Vec<u8>,
    pub count: usize,
    pub last: Option<u64>,
}

pub(crate) fn page(raw: Vec<u8>) -> Result<Page, DecodeError> {
    let rows = Rows::of(&raw)?;
    let last = match rows.0.last() {
        Some(row) => Some(uint(&at_least(row, 1)?[0])?),
        None => None,
    };
    Ok(Page {
        count: rows.0.len(),
        raw,
        last,
    })
}
