//! The device-list push (Bot v2 task 8, fix round 1): what dillad sends the Bot when a user's
//! device list changes, and what the Bot helper's op 24 ingests. It carries the user's key
//! evidence with the history, so the Bot can pin a user it never met in an MLS group (trust on
//! first push) and catch up across missed versions.
//!
//! Deterministic CBOR `[umk_pub(32), ssk_pub(32), sig_umk_ssk(64), history]`, where `history` is
//! 1..=1024 rows `[version, blob, ssk_signature(64), prev_hash(32)]` in version order, each row
//! the device-list PUT body (`blob` is the signed 6-element list). The history runs from
//! version 1 for a user the Bot has not pinned, or for a key other than the pinned one; once the
//! Bot holds a pinned list it may start anywhere at or below the Bot's cached version + 1.
//!
//! This is the shape only; the trust rules are `ClientCore::ingest_device_list`.

use super::CodecError;
use crate::cbor::{CborError, Decoder, Encoder, decode_strict};

/// The most rows one push carries (as `validate_history_from`).
pub const MAX_PUSH_ROWS: usize = 1024;
/// The largest push, in bytes (as `validate_history_from`'s history bound).
pub const MAX_PUSH_BYTES: usize = 4 * 1024 * 1024;

#[derive(Clone, PartialEq, Eq, Debug)]
pub struct DeviceListRow {
    pub version: u64,
    pub blob: Vec<u8>,
    pub ssk_signature: [u8; 64],
    pub prev_hash: [u8; 32],
}

#[derive(Clone, PartialEq, Eq, Debug)]
pub struct DeviceListPush {
    pub umk_pub: [u8; 32],
    pub ssk_pub: [u8; 32],
    pub sig_umk_ssk: [u8; 64],
    pub history: Vec<DeviceListRow>,
}

fn is_zero(b: &[u8]) -> bool {
    b.iter().fold(0u8, |a, x| a | x) == 0
}

impl DeviceListPush {
    pub fn encode(&self) -> Vec<u8> {
        let mut e = Encoder::new();
        e.array(4)
            .bytes(&self.umk_pub)
            .bytes(&self.ssk_pub)
            .bytes(&self.sig_umk_ssk)
            .array(self.history.len());
        for row in &self.history {
            e.array(4)
                .uint(row.version)
                .bytes(&row.blob)
                .bytes(&row.ssk_signature)
                .bytes(&row.prev_hash);
        }
        e.into_vec()
    }

    pub fn decode(b: &[u8]) -> Result<Self, CodecError> {
        if b.len() > MAX_PUSH_BYTES {
            return Err(CodecError::Limit);
        }
        let push = decode_strict(b, |d: &mut Decoder<'_>| {
            d.array(4)?;
            let umk_pub = d.bytes_exact::<32>()?;
            let ssk_pub = d.bytes_exact::<32>()?;
            let sig_umk_ssk = d.bytes_exact::<64>()?;
            let n = d.array_len()?;
            if n == 0 || n > MAX_PUSH_ROWS {
                return Err(CborError::WrongArrayLen {
                    expected: MAX_PUSH_ROWS,
                    actual: n,
                });
            }
            let mut history = Vec::with_capacity(n.min(64));
            for _ in 0..n {
                d.array(4)?;
                history.push(DeviceListRow {
                    version: d.uint()?,
                    blob: d.bytes()?.to_vec(),
                    ssk_signature: d.bytes_exact::<64>()?,
                    prev_hash: d.bytes_exact::<32>()?,
                });
            }
            Ok(Self {
                umk_pub,
                ssk_pub,
                sig_umk_ssk,
                history,
            })
        })?;
        if is_zero(&push.umk_pub)
            || is_zero(&push.ssk_pub)
            || push
                .history
                .iter()
                .any(|r| r.version == 0 || r.blob.is_empty())
        {
            return Err(CodecError::Value);
        }
        Ok(push)
    }
}
