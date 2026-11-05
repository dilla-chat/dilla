//! The message envelope and its franking commitment (protocol/04-envelope-and-franking.md).
//!
//! The envelope is a 9-element fixed-position CBOR array. It travels inside an MLS
//! `PrivateMessage` whose `authenticated_data` is exactly the 32-byte commitment `C`, and whose
//! ciphertext is padded to a multiple of `PADDING_MULTIPLE` bytes.

mod frank;

pub use frank::{FrankingTagInput, franking_tag, verify_franking_tag};

use crate::cbor::{CborError, Decoder, Encoder, decode_strict};
use crate::error::ProtocolError;
use crate::identity::{hmac_sha256, hmac_sha256_verify};
use crate::ids::MsgId;

/// `body` limit in UTF-8 bytes for types 0 and 1.
pub const MAX_BODY_LONG: usize = 4_000;
/// `body` limit in UTF-8 bytes for types 3 and 4 (the emoji).
pub const MAX_BODY_SHORT: usize = 32;
/// interfaces.md §2.8 (R6, R32): the tightened envelope limits. One maximal attachment is
/// 32+32+12+8+255+255+8192 = 8 786 bytes of content, so ten are 87 860 bytes (~86 KiB) before
/// CBOR heads. Add 2 × (2048+256+1024+16384) = 39 424 bytes of previews and a 4 000-byte body and
/// the theoretical worst case is 131 284 bytes plus heads; the real encoded maximum is 131 643
/// bytes (measured by `TestAMaximalTenFileEnvelopeIsAccepted` in the Go server). That is above the
/// old 131 072-byte ciphertext cap and well inside the 196 608-byte default (192 KiB) of
/// protocol/02, raised for this limit.
pub const MAX_ATTACHMENTS: usize = 10;
pub const MAX_PREVIEWS: usize = 2;
pub const MAX_THUMB: usize = 8_192;
pub const MAX_PREVIEW_IMAGE: usize = 16_384;
pub const MAX_MIME: usize = 255;
/// Element 8 of an attachment: the file's name as the sender chose it, UTF-8, at most MAX_NAME bytes
/// ("" when the sender gives none). A receiver sanitises it for display and saving; it refuses only length.
pub const MAX_NAME: usize = 255;
pub const MAX_URL: usize = 2_048;
pub const MAX_TITLE: usize = 256;
pub const MAX_DESCRIPTION: usize = 1_024;
/// The `PrivateMessage` carrying an envelope is padded so its ciphertext length is a multiple of
/// this (RFC 9420 section 6.3.1).
pub const PADDING_MULTIPLE: usize = 256;
/// `C = HMAC-SHA256(k_f, DOMAIN_FRANK || CBOR(envelope with k_f blanked))`
pub const DOMAIN_FRANK: &[u8] = b"dilla frank v1";
/// `T = HMAC-SHA256(K_frank, DOMAIN_FRANK_TAG || ...)`
pub const DOMAIN_FRANK_TAG: &[u8] = b"dilla frank tag v1";

#[derive(Clone, Copy, PartialEq, Eq, Debug)]
#[repr(u8)]
pub enum EnvelopeType {
    Message = 0,
    Edit = 1,
    Delete = 2,
    ReactionAdd = 3,
    ReactionRemove = 4,
    Pin = 5,
    Unpin = 6,
}

impl EnvelopeType {
    pub const fn as_u8(self) -> u8 {
        self as u8
    }

    pub fn from_u64(v: u64) -> Result<Self, ProtocolError> {
        Ok(match v {
            0 => Self::Message,
            1 => Self::Edit,
            2 => Self::Delete,
            3 => Self::ReactionAdd,
            4 => Self::ReactionRemove,
            5 => Self::Pin,
            6 => Self::Unpin,
            _ => return Err(ProtocolError::EnvelopeType),
        })
    }

    /// 4 000 bytes for a message or an edit, 32 for a reaction, 0 for a tombstone or a pin.
    pub const fn body_limit(self) -> usize {
        match self {
            Self::Message | Self::Edit => MAX_BODY_LONG,
            Self::ReactionAdd | Self::ReactionRemove => MAX_BODY_SHORT,
            Self::Delete | Self::Pin | Self::Unpin => 0,
        }
    }
}

/// `blob_id` is the SHA-256 of the **stored bytes** (ciphertext ‖ tag); a receiver
/// verifies it (`E_BLOB_HASH`). `thumb` is the sealed thumbnail from
/// `crate::attachment::seal_thumb`, not decrypted media.
///
/// `key` and `nonce` are the AES-GCM material of the blob and `thumb` is sealed media, so this
/// does not derive `Debug` (see the hand-written impl below).
#[derive(Clone, PartialEq, Eq)]
pub struct Attachment {
    pub blob_id: [u8; 32],
    pub key: [u8; 32],
    pub nonce: [u8; 12],
    /// Plaintext bytes.
    pub size: u64,
    pub mime: String,
    pub w: Option<u64>,
    pub h: Option<u64>,
    pub thumb: Option<Vec<u8>>,
    pub name: String,
}

/// Sender-generated. A receiver MUST NOT fetch the remote resource.
///
/// Every field is plaintext taken from the message, so this does not derive `Debug` either.
#[derive(Clone, PartialEq, Eq)]
pub struct Preview {
    pub url: String,
    pub title: String,
    pub description: String,
    pub image: Option<Vec<u8>>,
}

/// Carries the decrypted message (`body`, attachments, previews) and `k_f`, the per-message
/// franking key, so it does not derive `Debug`: nothing printable may reproduce the plaintext of
/// an end-to-end encrypted message or its key material.
#[derive(Clone, PartialEq, Eq)]
pub struct Envelope {
    pub v: u64,
    pub msg_id: MsgId,
    pub kind: EnvelopeType,
    pub thread_id: Option<MsgId>,
    pub reply_to: Option<MsgId>,
    pub body: String,
    pub attachments: Vec<Attachment>,
    pub previews: Vec<Preview>,
    /// The franking key, random per envelope and per edit.
    pub k_f: [u8; 32],
}

/// `Some(<n bytes>)` / `None` for an optional byte string: presence is protocol-visible, the bytes
/// are not.
fn debug_opt_bytes(v: Option<&Vec<u8>>) -> impl core::fmt::Display + '_ {
    struct D<'a>(Option<&'a Vec<u8>>);
    impl core::fmt::Display for D<'_> {
        fn fmt(&self, f: &mut core::fmt::Formatter<'_>) -> core::fmt::Result {
            match self.0 {
                Some(b) => write!(f, "Some(<{} bytes>)", b.len()),
                None => f.write_str("None"),
            }
        }
    }
    D(v)
}

/// Hand-written so that no `{:?}`, `dbg!`, `tracing` field, `expect` message or failed
/// `assert_eq!` can print the blob key, its nonce or the decrypted thumbnail. `blob_id` is a
/// public ciphertext hash and `mime`/`size`/`w`/`h` are metadata the server sees anyway.
impl core::fmt::Debug for Attachment {
    fn fmt(&self, f: &mut core::fmt::Formatter<'_>) -> core::fmt::Result {
        f.debug_struct("Attachment")
            .field("blob_id", &self.blob_id)
            .field("key", &format_args!("<redacted>"))
            .field("nonce", &format_args!("<redacted>"))
            .field("size", &self.size)
            .field("mime", &self.mime)
            .field("w", &self.w)
            .field("h", &self.h)
            .field(
                "thumb",
                &format_args!("{}", debug_opt_bytes(self.thumb.as_ref())),
            )
            .field("name", &format_args!("<{} bytes>", self.name.len()))
            .finish()
    }
}

/// Hand-written for the same reason: a preview is built from the plaintext message, so its url,
/// title, description and image show their lengths and never their contents.
impl core::fmt::Debug for Preview {
    fn fmt(&self, f: &mut core::fmt::Formatter<'_>) -> core::fmt::Result {
        f.debug_struct("Preview")
            .field("url", &format_args!("<{} bytes>", self.url.len()))
            .field("title", &format_args!("<{} bytes>", self.title.len()))
            .field(
                "description",
                &format_args!("<{} bytes>", self.description.len()),
            )
            .field(
                "image",
                &format_args!("{}", debug_opt_bytes(self.image.as_ref())),
            )
            .finish()
    }
}

/// Hand-written so that the plaintext `body` and the franking key `k_f` cannot reach a log line,
/// a panic message or a failed `assert_eq!`. The routing fields (ids, type, thread) are printed:
/// they are what a diagnostic needs and the server already sees their ciphertext positions.
impl core::fmt::Debug for Envelope {
    fn fmt(&self, f: &mut core::fmt::Formatter<'_>) -> core::fmt::Result {
        f.debug_struct("Envelope")
            .field("v", &self.v)
            .field("msg_id", &self.msg_id)
            .field("kind", &self.kind)
            .field("thread_id", &self.thread_id)
            .field("reply_to", &self.reply_to)
            .field("body", &format_args!("<{} bytes>", self.body.len()))
            .field("attachments", &self.attachments)
            .field("previews", &self.previews)
            .field("k_f", &format_args!("<redacted>"))
            .finish()
    }
}

impl Envelope {
    fn write(&self, e: &mut Encoder, k_f: Option<&[u8; 32]>) {
        e.array(9)
            .uint(self.v)
            .bytes(self.msg_id.as_bytes())
            .uint(u64::from(self.kind.as_u8()))
            .opt_bytes(self.thread_id.as_ref().map(|m| &m.0[..]))
            .opt_bytes(self.reply_to.as_ref().map(|m| &m.0[..]))
            .text(&self.body);
        e.array(self.attachments.len());
        for a in &self.attachments {
            e.array(9)
                .bytes(&a.blob_id)
                .bytes(&a.key)
                .bytes(&a.nonce)
                .uint(a.size)
                .text(&a.mime)
                .opt_uint(a.w)
                .opt_uint(a.h)
                .opt_bytes(a.thumb.as_deref())
                .text(&a.name);
        }
        e.array(self.previews.len());
        for p in &self.previews {
            e.array(4)
                .text(&p.url)
                .text(&p.title)
                .text(&p.description)
                .opt_bytes(p.image.as_deref());
        }
        match k_f {
            Some(k) => e.bytes(k),
            None => e.bytes(&[]),
        };
    }

    pub fn encode(&self) -> Result<Vec<u8>, ProtocolError> {
        self.validate()?;
        let mut e = Encoder::with_capacity(256 + self.body.len());
        self.write(&mut e, Some(&self.k_f));
        Ok(e.into_vec())
    }

    pub fn decode(bytes: &[u8]) -> Result<Self, ProtocolError> {
        let env = decode_strict(bytes, Self::read).map_err(map_cbor)?;
        env.validate()?;
        Ok(env)
    }

    fn read(d: &mut Decoder<'_>) -> Result<Self, CborError> {
        d.array(9)?;
        let v = d.uint()?;
        if v != 1 {
            return Err(SHAPE);
        }
        let msg_id = MsgId::from_bytes(d.bytes_exact::<16>()?);
        let kind = EnvelopeType::from_u64(d.uint()?).map_err(|_| TYPE)?;
        let thread_id = d.opt_bytes_exact::<16>()?.map(MsgId::from_bytes);
        let reply_to = d.opt_bytes_exact::<16>()?.map(MsgId::from_bytes);
        let body = d.text()?.to_owned();

        let n = d.array_len()?;
        // `array_len` bounds `n` by the bytes that remain, never by the size of what those bytes
        // decode into (its own doc: "a caller must not scale it into a larger allocation"): an
        // `Attachment` in memory is ~168 bytes against the 87 its shortest encoding costs, so
        // `Vec::with_capacity(n)` on an unchecked head turns a 1 MiB hostile blob into ~168 MiB of
        // reservation. The limit is known here, so the lie is rejected before anything is
        // reserved. Same ruling as `device_list::read_entries` and `pairing`.
        if n > MAX_ATTACHMENTS {
            return Err(LIMIT);
        }
        let mut attachments = Vec::with_capacity(n);
        for _ in 0..n {
            d.array(9)?;
            let blob_id = d.bytes_exact::<32>()?;
            let key = d.bytes_exact::<32>()?;
            let nonce = d.bytes_exact::<12>()?;
            let size = d.uint()?;
            let mime = d.text()?.to_owned();
            if mime.len() > MAX_MIME {
                return Err(LIMIT);
            }
            let w = d.opt_uint()?;
            let h = d.opt_uint()?;
            let thumb = if d.try_null()? {
                None
            } else {
                let t = d.bytes()?.to_vec();
                if t.len() > MAX_THUMB {
                    return Err(LIMIT);
                }
                Some(t)
            };
            let name = d.text()?.to_owned();
            if name.len() > MAX_NAME {
                return Err(LIMIT);
            }
            attachments.push(Attachment {
                blob_id,
                key,
                nonce,
                size,
                mime,
                w,
                h,
                thumb,
                name,
            });
        }

        let n = d.array_len()?;
        // Same reasoning: a `Preview` is ~96 bytes in memory against the 5 its shortest encoding
        // costs, a ~96x amplification if the claimed count were reserved unchecked.
        if n > MAX_PREVIEWS {
            return Err(LIMIT);
        }
        let mut previews = Vec::with_capacity(n);
        for _ in 0..n {
            d.array(4)?;
            let url = d.text()?.to_owned();
            if url.len() > MAX_URL {
                return Err(LIMIT);
            }
            let title = d.text()?.to_owned();
            if title.len() > MAX_TITLE {
                return Err(LIMIT);
            }
            let description = d.text()?.to_owned();
            if description.len() > MAX_DESCRIPTION {
                return Err(LIMIT);
            }
            let image = if d.try_null()? {
                None
            } else {
                let i = d.bytes()?.to_vec();
                if i.len() > MAX_PREVIEW_IMAGE {
                    return Err(LIMIT);
                }
                Some(i)
            };
            previews.push(Preview {
                url,
                title,
                description,
                image,
            });
        }

        let k_f = d.bytes_exact::<32>()?;
        Ok(Self {
            v,
            msg_id,
            kind,
            thread_id,
            reply_to,
            body,
            attachments,
            previews,
            k_f,
        })
    }

    /// The reference decoder's order, reproduced exactly: element count, then `v`, then `type`,
    /// then the two fixed lengths, then every limit, then the attachment names, then the type 1..6
    /// rule (L-CORE-30).
    ///
    /// Attacker statement (lesson e): the type 1..6 refusals reject only an envelope its own sender
    /// built wrongly; an honest client never sends a fold without a target or a fold carrying
    /// files, and no other member can make an honest member's envelope fail them.
    pub fn validate(&self) -> Result<(), ProtocolError> {
        if self.v != 1 {
            return Err(ProtocolError::EnvelopeShape);
        }
        if self.body.len() > self.kind.body_limit() {
            return Err(ProtocolError::EnvelopeLimit);
        }
        if self.attachments.len() > MAX_ATTACHMENTS || self.previews.len() > MAX_PREVIEWS {
            return Err(ProtocolError::EnvelopeLimit);
        }
        for a in &self.attachments {
            if a.mime.len() > MAX_MIME {
                return Err(ProtocolError::EnvelopeLimit);
            }
            if a.thumb.as_ref().is_some_and(|t| t.len() > MAX_THUMB) {
                return Err(ProtocolError::EnvelopeLimit);
            }
        }
        for p in &self.previews {
            if p.url.len() > MAX_URL
                || p.title.len() > MAX_TITLE
                || p.description.len() > MAX_DESCRIPTION
            {
                return Err(ProtocolError::EnvelopeLimit);
            }
            if p.image
                .as_ref()
                .is_some_and(|i| i.len() > MAX_PREVIEW_IMAGE)
            {
                return Err(ProtocolError::EnvelopeLimit);
            }
        }
        if self.attachments.iter().any(|a| a.name.len() > MAX_NAME) {
            return Err(ProtocolError::EnvelopeLimit);
        }
        if self.kind != EnvelopeType::Message
            && (self.reply_to.is_none()
                || !self.attachments.is_empty()
                || !self.previews.is_empty())
        {
            return Err(ProtocolError::EnvelopeShape);
        }
        Ok(())
    }

    /// The same 9-element array with element 9 replaced by the empty byte string (`0x40`).
    pub fn commitment_preimage(&self) -> Result<Vec<u8>, ProtocolError> {
        self.validate()?;
        let mut e = Encoder::with_capacity(256 + self.body.len());
        self.write(&mut e, None);
        Ok(e.into_vec())
    }

    /// `C = HMAC-SHA256(k_f, DOMAIN_FRANK || commitment_preimage())`
    pub fn commitment(&self) -> Result<[u8; 32], ProtocolError> {
        let pre = self.commitment_preimage()?;
        let mut data = Vec::with_capacity(DOMAIN_FRANK.len() + pre.len());
        data.extend_from_slice(DOMAIN_FRANK);
        data.extend_from_slice(&pre);
        Ok(hmac_sha256(&self.k_f, &data))
    }

    /// Recomputes `C` from the decrypted envelope and compares it, in constant time, with the MLS
    /// `authenticated_data`. Any mismatch is a hard reject (protocol/04).
    pub fn verify_commitment(&self, authenticated_data: &[u8]) -> Result<(), ProtocolError> {
        if authenticated_data.len() != 32 {
            return Err(ProtocolError::FrankMismatch);
        }
        let pre = self.commitment_preimage()?;
        let mut data = Vec::with_capacity(DOMAIN_FRANK.len() + pre.len());
        data.extend_from_slice(DOMAIN_FRANK);
        data.extend_from_slice(&pre);
        let mut tag = [0u8; 32];
        tag.copy_from_slice(authenticated_data);
        if hmac_sha256_verify(&self.k_f, &data, &tag) {
            Ok(())
        } else {
            Err(ProtocolError::FrankMismatch)
        }
    }
}

const SHAPE: CborError = CborError::TypeMismatch {
    expected: "envelope shape",
    offset: 0,
};
const TYPE: CborError = CborError::TypeMismatch {
    expected: "envelope type",
    offset: 0,
};
/// A limit that `read` can see before `validate` runs, so that an over-claiming array head is
/// rejected at the head rather than after the elements have been reserved for.
const LIMIT: CborError = CborError::TypeMismatch {
    expected: "envelope limit",
    offset: 0,
};

fn map_cbor(e: CborError) -> ProtocolError {
    match e {
        CborError::TypeMismatch {
            expected: "envelope type",
            ..
        } => ProtocolError::EnvelopeType,
        CborError::TypeMismatch {
            expected: "envelope limit",
            ..
        } => ProtocolError::EnvelopeLimit,
        _ => ProtocolError::EnvelopeShape,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::ids::MsgId;

    const ENVELOPE_JSON: &str = include_str!("../../../../protocol/vectors/envelope.json");

    fn unhex(s: &str) -> Vec<u8> {
        (0..s.len() / 2)
            .map(|i| u8::from_str_radix(&s[2 * i..2 * i + 2], 16).expect("hex digit"))
            .collect()
    }

    fn unhex_n<const N: usize>(s: &str) -> [u8; N] {
        let v = unhex(s);
        assert_eq!(v.len(), N, "expected {N} bytes, got {}", v.len());
        let mut out = [0u8; N];
        out.copy_from_slice(&v);
        out
    }

    fn hex_of(b: &[u8]) -> String {
        b.iter().map(|x| format!("{x:02x}")).collect()
    }

    fn opt_msg_id(v: &serde_json::Value) -> Option<MsgId> {
        v.as_str().map(|s| MsgId::from_bytes(unhex_n::<16>(s)))
    }

    fn opt_bytes(v: &serde_json::Value) -> Option<Vec<u8>> {
        v.as_str().map(unhex)
    }

    fn envelope_from_json(j: &serde_json::Value) -> Envelope {
        Envelope {
            v: j["v"].as_u64().expect("v"),
            msg_id: MsgId::from_bytes(unhex_n::<16>(j["msgId"].as_str().expect("msgId"))),
            kind: EnvelopeType::from_u64(j["type"].as_u64().expect("type")).expect("type"),
            thread_id: opt_msg_id(&j["threadId"]),
            reply_to: opt_msg_id(&j["replyTo"]),
            body: j["body"].as_str().expect("body").to_owned(),
            attachments: j["attachments"]
                .as_array()
                .expect("attachments")
                .iter()
                .map(|a| Attachment {
                    blob_id: unhex_n::<32>(a["blobId"].as_str().expect("blobId")),
                    key: unhex_n::<32>(a["key"].as_str().expect("key")),
                    nonce: unhex_n::<12>(a["nonce"].as_str().expect("nonce")),
                    size: a["size"].as_u64().expect("size"),
                    mime: a["mime"].as_str().expect("mime").to_owned(),
                    w: a["w"].as_u64(),
                    h: a["h"].as_u64(),
                    thumb: opt_bytes(&a["thumb"]),
                    name: a["name"].as_str().expect("name").to_owned(),
                })
                .collect(),
            previews: j["previews"]
                .as_array()
                .expect("previews")
                .iter()
                .map(|p| Preview {
                    url: p["url"].as_str().expect("url").to_owned(),
                    title: p["title"].as_str().expect("title").to_owned(),
                    description: p["description"].as_str().expect("description").to_owned(),
                    image: opt_bytes(&p["image"]),
                })
                .collect(),
            k_f: unhex_n::<32>(j["kf"].as_str().expect("kf")),
        }
    }

    #[test]
    fn every_envelope_vector_reproduces_cbor_length_and_commitment() {
        let doc: serde_json::Value = serde_json::from_str(ENVELOPE_JSON).expect("envelope.json");
        let cases = doc["cases"].as_array().expect("cases");
        assert_eq!(
            cases.len(),
            5,
            "envelope.json is expected to carry five cases"
        );
        for case in cases {
            let name = case["name"].as_str().expect("name");
            let env = envelope_from_json(&case["envelope"]);
            let bytes = env.encode().unwrap_or_else(|e| panic!("{name}: {e}"));
            assert_eq!(
                hex_of(&bytes),
                case["cbor"].as_str().expect("cbor"),
                "{name}: cbor"
            );
            assert_eq!(
                bytes.len() as u64,
                case["length"].as_u64().expect("length"),
                "{name}: length"
            );
            assert_eq!(
                hex_of(&env.commitment().unwrap()),
                case["commitment"].as_str().expect("commitment"),
                "{name}: commitment"
            );
            assert_eq!(Envelope::decode(&bytes).unwrap(), env, "{name}: round trip");
        }
    }

    #[test]
    fn commitment_preimage_blanks_only_the_ninth_element() {
        let doc: serde_json::Value = serde_json::from_str(ENVELOPE_JSON).expect("envelope.json");
        let env = envelope_from_json(&doc["cases"][0]["envelope"]);
        let full = env.encode().unwrap();
        let pre = env.commitment_preimage().unwrap();
        // the full encoding ends with `58 20` + 32 bytes of k_f; the preimage ends with `40`
        assert_eq!(&pre[..full.len() - 34], &full[..full.len() - 34]);
        assert_eq!(pre[pre.len() - 1], 0x40);
        assert_eq!(pre.len(), full.len() - 33);
    }

    fn base() -> Envelope {
        Envelope {
            v: 1,
            msg_id: MsgId::from_bytes([0x01; 16]),
            kind: EnvelopeType::Message,
            thread_id: None,
            reply_to: None,
            body: String::new(),
            attachments: Vec::new(),
            previews: Vec::new(),
            k_f: [0x06; 32],
        }
    }

    #[test]
    fn decode_rejects_in_the_documented_order() {
        // wrong element count
        let mut bytes = base().encode().unwrap();
        bytes[0] = 0x88; // array of 8
        assert_eq!(Envelope::decode(&bytes), Err(ProtocolError::EnvelopeShape));

        // v != 1. The bad byte is patched into a valid encoding: `Envelope::encode` begins with
        // `self.validate()?`, which rejects `v != 1`, so `base().v = 2; ...encode().unwrap()`
        // would panic and the decode path under test would never run. Element 2 of the array,
        // `v`, sits at offset 1, straight after the `0x89` array head.
        let mut bytes = base().encode().unwrap();
        assert_eq!(
            bytes[1], 0x01,
            "the v element must be where this test expects it"
        );
        bytes[1] = 0x02;
        assert_eq!(Envelope::decode(&bytes), Err(ProtocolError::EnvelopeShape));

        // unknown type: element 3 of the array sits at offset 19
        // (1 array head + 1 v + 17 msg_id)
        let mut bytes = base().encode().unwrap();
        assert_eq!(
            bytes[19], 0x00,
            "the type element must be where this test expects it"
        );
        bytes[19] = 0x07;
        assert_eq!(Envelope::decode(&bytes), Err(ProtocolError::EnvelopeType));

        // trailing bytes
        let mut bytes = base().encode().unwrap();
        bytes.push(0x00);
        assert_eq!(Envelope::decode(&bytes), Err(ProtocolError::EnvelopeShape));
    }

    #[test]
    fn validate_enforces_every_limit() {
        assert_eq!(base().validate(), Ok(()));

        let mut long = base();
        long.body = "a".repeat(MAX_BODY_LONG);
        assert_eq!(long.validate(), Ok(()));
        long.body = "a".repeat(MAX_BODY_LONG + 1);
        assert_eq!(long.validate(), Err(ProtocolError::EnvelopeLimit));

        let mut reaction = base();
        reaction.kind = EnvelopeType::ReactionAdd;
        reaction.reply_to = Some(MsgId::from_bytes([0x02; 16]));
        reaction.body = "a".repeat(MAX_BODY_SHORT);
        assert_eq!(reaction.validate(), Ok(()));
        reaction.body = "a".repeat(MAX_BODY_SHORT + 1);
        assert_eq!(reaction.validate(), Err(ProtocolError::EnvelopeLimit));

        let attachment = Attachment {
            blob_id: [0x03; 32],
            key: [0x04; 32],
            nonce: [0x05; 12],
            size: 1,
            mime: "image/jpeg".to_owned(),
            w: None,
            h: None,
            thumb: None,
            name: String::new(),
        };
        let mut many = base();
        many.attachments = vec![attachment.clone(); MAX_ATTACHMENTS];
        assert_eq!(many.validate(), Ok(()));
        many.attachments.push(attachment.clone());
        assert_eq!(many.validate(), Err(ProtocolError::EnvelopeLimit));

        let mut fat_thumb = base();
        let mut a = attachment.clone();
        a.thumb = Some(vec![0u8; MAX_THUMB]);
        fat_thumb.attachments = vec![a.clone()];
        assert_eq!(fat_thumb.validate(), Ok(()));
        a.thumb = Some(vec![0u8; MAX_THUMB + 1]);
        fat_thumb.attachments = vec![a];
        assert_eq!(fat_thumb.validate(), Err(ProtocolError::EnvelopeLimit));

        let preview = Preview {
            url: "https://example.invalid/".to_owned(),
            title: "t".to_owned(),
            description: "d".to_owned(),
            image: None,
        };
        let mut many = base();
        many.previews = vec![preview.clone(); MAX_PREVIEWS];
        assert_eq!(many.validate(), Ok(()));
        many.previews.push(preview.clone());
        assert_eq!(many.validate(), Err(ProtocolError::EnvelopeLimit));

        let mut fat_image = base();
        let mut p = preview;
        p.image = Some(vec![0u8; MAX_PREVIEW_IMAGE]);
        fat_image.previews = vec![p.clone()];
        assert_eq!(fat_image.validate(), Ok(()));
        p.image = Some(vec![0u8; MAX_PREVIEW_IMAGE + 1]);
        fat_image.previews = vec![p];
        assert_eq!(fat_image.validate(), Err(ProtocolError::EnvelopeLimit));
    }

    #[test]
    fn body_limit_is_per_type() {
        assert_eq!(EnvelopeType::Message.body_limit(), MAX_BODY_LONG);
        assert_eq!(EnvelopeType::Edit.body_limit(), MAX_BODY_LONG);
        assert_eq!(EnvelopeType::ReactionAdd.body_limit(), MAX_BODY_SHORT);
        assert_eq!(EnvelopeType::ReactionRemove.body_limit(), MAX_BODY_SHORT);
        for t in [EnvelopeType::Delete, EnvelopeType::Pin, EnvelopeType::Unpin] {
            assert_eq!(t.body_limit(), 0);
        }
        assert_eq!(EnvelopeType::from_u64(7), Err(ProtocolError::EnvelopeType));
    }

    fn sample_attachment() -> Attachment {
        Attachment {
            blob_id: [0x03; 32],
            key: [0x44; 32],
            nonce: [0x55; 12],
            size: 1,
            mime: "image/jpeg".to_owned(),
            w: None,
            h: None,
            thumb: None,
            name: "map.png".to_owned(),
        }
    }

    fn sample_preview() -> Preview {
        Preview {
            url: "https://example.invalid/".to_owned(),
            title: "t".to_owned(),
            description: "d".to_owned(),
            image: None,
        }
    }

    /// `envelope.json` case 5 now exercises the `Some(bytes)` arm of `thumb`; a preview `image` is
    /// still never set by any vector, so a decoder that silently threw those bytes away would still
    /// pass the vector suite. This round trip covers both.
    #[test]
    fn round_trip_preserves_a_non_null_thumb_and_preview_image() {
        let mut env = base();
        let mut a = sample_attachment();
        a.thumb = Some(vec![0xAB; 64]);
        env.attachments = vec![a];
        let mut p = sample_preview();
        p.image = Some(vec![0xCD; 64]);
        env.previews = vec![p];

        let bytes = env.encode().unwrap();
        let back = Envelope::decode(&bytes).unwrap();
        assert_eq!(back, env, "round trip");
        assert_eq!(
            back.attachments[0].thumb.as_deref(),
            Some(&[0xAB; 64][..]),
            "the thumbnail bytes survived the decoder"
        );
        assert_eq!(
            back.previews[0].image.as_deref(),
            Some(&[0xCD; 64][..]),
            "the preview image bytes survived the decoder"
        );

        // Both fields are inside the commitment preimage, so flipping either moves `C`.
        let c = env.commitment().unwrap();
        let mut other = env.clone();
        other.attachments[0].thumb = Some(vec![0xAC; 64]);
        assert_ne!(other.commitment().unwrap(), c, "thumb is committed to");
        let mut other = env.clone();
        other.previews[0].image = Some(vec![0xCE; 64]);
        assert_ne!(other.commitment().unwrap(), c, "image is committed to");
        assert_eq!(env.commitment().unwrap(), c, "commitment is stable");
    }

    /// Builds an envelope encoding by hand so the attachment and preview array heads can claim
    /// more elements than follow them. `n` is the claimed attachment count, `m` the claimed
    /// preview count; `filler` trailing bytes keep both claims under the bytes that remain, which
    /// is the only bound `array_len` itself applies.
    fn overclaiming(n: usize, m: usize, filler: usize) -> Vec<u8> {
        let mut e = Encoder::with_capacity(256);
        e.array(9)
            .uint(1)
            .bytes(&[0x01u8; 16])
            .uint(0)
            .null()
            .null()
            .text("");
        e.array(n);
        e.array(m);
        e.bytes(&[0x06u8; 32]);
        let mut bytes = e.into_vec();
        bytes.extend(core::iter::repeat_n(0u8, filler));
        bytes
    }

    /// `array_len` bounds the claimed element count by the bytes that remain, never by the size of
    /// what those bytes decode into: an `Attachment` in memory is far wider than the 87 bytes its
    /// shortest encoding costs, so a head claiming one element per remaining byte is a two-orders
    /// -of-magnitude lie that the decoder must reject before it reserves anything.
    #[test]
    fn decode_rejects_an_attachment_array_head_that_overclaims() {
        let bytes = overclaiming(1_000, 0, 1_024);
        // 1 array head + 1 v + 17 msg_id + 1 type + 1 null + 1 null + 1 empty text = offset 23,
        // and 1 000 is well under the bytes that follow, so `array_len` accepts the head.
        assert_eq!(
            bytes[23], 0x99,
            "the attachments head is where this test thinks it is"
        );
        assert!(bytes.len() - 26 >= 1_000);
        assert_eq!(Envelope::decode(&bytes), Err(ProtocolError::EnvelopeLimit));

        // The honest maximum still decodes.
        let mut env = base();
        env.attachments = vec![sample_attachment(); MAX_ATTACHMENTS];
        let ok = env.encode().unwrap();
        assert_eq!(Envelope::decode(&ok).unwrap(), env);
    }

    /// Same lie, on the preview head: a `Preview` costs 5 bytes at its shortest and is far wider
    /// than that in memory.
    #[test]
    fn decode_rejects_a_preview_array_head_that_overclaims() {
        let bytes = overclaiming(0, 1_000, 1_024);
        // the attachments head is a single `0x80` at offset 23, so the previews head starts at 24
        assert_eq!(bytes[23], 0x80, "the attachments head is a 0-element array");
        assert_eq!(
            bytes[24], 0x99,
            "the previews head is where this test thinks it is"
        );
        assert!(bytes.len() - 27 >= 1_000);
        assert_eq!(Envelope::decode(&bytes), Err(ProtocolError::EnvelopeLimit));

        let mut env = base();
        env.previews = vec![sample_preview(); MAX_PREVIEWS];
        let ok = env.encode().unwrap();
        assert_eq!(Envelope::decode(&ok).unwrap(), env);
    }

    /// interfaces.md §2.8 / protocol/04 "Limits": the tightened per-field bounds.
    #[test]
    fn tightened_limits_have_the_r6_values() {
        assert_eq!(MAX_ATTACHMENTS, 10);
        assert_eq!(MAX_PREVIEWS, 2);
        assert_eq!(MAX_THUMB, 8_192);
        assert_eq!(MAX_PREVIEW_IMAGE, 16_384);
        assert_eq!(MAX_MIME, 255);
        assert_eq!(MAX_URL, 2_048);
        assert_eq!(MAX_TITLE, 256);
        assert_eq!(MAX_DESCRIPTION, 1_024);
    }

    /// Wave C: a message carries up to ten files, and the tenth decodes.
    #[test]
    fn ten_attachments_encode_and_decode() {
        let mut env = base();
        env.attachments = vec![sample_attachment(); 10];
        let bytes = env.encode().unwrap();
        assert_eq!(Envelope::decode(&bytes).unwrap(), env);
        env.attachments = vec![sample_attachment(); 11];
        assert_eq!(env.validate(), Err(ProtocolError::EnvelopeLimit));
    }

    /// Each new bound refuses at exactly one byte over, and accepts at the bound.
    #[test]
    fn each_new_limit_refuses_one_byte_over() {
        // The existing test module's envelope constructor is `base()`
        // (core/dilla-core/src/envelope/mod.rs:525); `sample_attachment()` (:650)
        // and `sample_preview()` (:663) are the other two helpers. There is no
        // `sample_envelope()`.
        let mut env = base();
        let mut a = sample_attachment();
        a.mime = "a".repeat(MAX_MIME);
        env.attachments = vec![a.clone()];
        assert!(env.validate().is_ok());
        a.mime = "a".repeat(MAX_MIME + 1);
        env.attachments = vec![a.clone()];
        assert!(env.validate().is_err());

        a.mime = "image/png".into();
        a.thumb = Some(vec![0u8; MAX_THUMB]);
        env.attachments = vec![a.clone()];
        assert!(env.validate().is_ok());
        a.thumb = Some(vec![0u8; MAX_THUMB + 1]);
        env.attachments = vec![a];
        assert!(env.validate().is_err());

        env.attachments = vec![sample_attachment(); MAX_ATTACHMENTS];
        assert!(env.validate().is_ok());
        env.attachments = vec![sample_attachment(); MAX_ATTACHMENTS + 1];
        assert!(env.validate().is_err());
        env.attachments = vec![];

        let mut p = sample_preview();
        p.url = "u".repeat(MAX_URL);
        env.previews = vec![p.clone()];
        assert!(env.validate().is_ok());
        p.url = "u".repeat(MAX_URL + 1);
        env.previews = vec![p.clone()];
        assert!(env.validate().is_err());

        p.url = "https://example".into();
        p.title = "t".repeat(MAX_TITLE + 1);
        env.previews = vec![p.clone()];
        assert!(env.validate().is_err());

        p.title = "t".into();
        p.description = "d".repeat(MAX_DESCRIPTION + 1);
        env.previews = vec![p.clone()];
        assert!(env.validate().is_err());

        p.description = "d".into();
        p.image = Some(vec![0u8; MAX_PREVIEW_IMAGE + 1]);
        env.previews = vec![p];
        assert!(env.validate().is_err());

        env.previews = vec![sample_preview(); MAX_PREVIEWS + 1];
        assert!(env.validate().is_err());
    }

    /// envelope.json's rejects: interfaces.md §2.8's nine limit cases (with the tombstone) and
    /// web-2b task 1's four (L-CORE-30), each refused with the code the file names.
    #[test]
    fn every_vector_reject_is_refused_with_its_own_code() {
        let doc: serde_json::Value = serde_json::from_str(ENVELOPE_JSON).expect("envelope.json");
        let rejects = doc["rejects"].as_array().expect("rejects array");
        assert_eq!(rejects.len(), 13);
        let mut by_code = std::collections::BTreeMap::<String, usize>::new();
        for case in rejects {
            let name = case["name"].as_str().expect("name");
            let want = case["error"].as_str().expect("error");
            let err =
                Envelope::decode(&unhex(case["cbor"].as_str().expect("cbor"))).expect_err(name);
            assert_eq!(err.code(), want, "{name}");
            *by_code.entry(want.to_owned()).or_default() += 1;
        }
        assert_eq!(by_code.get("E_ENVELOPE_LIMIT"), Some(&10));
        assert_eq!(by_code.get("E_ENVELOPE_SHAPE"), Some(&3));
    }

    /// A derived `Debug` prints `k_f`, the attachment key and nonce, the thumbnail, the preview
    /// image and the plaintext `body` in full into any `{:?}`, `dbg!`, `tracing` field, panic
    /// message or failed `assert_eq!`. Same ruling as `PairingPayload` (commit 4c3bc90).
    #[test]
    fn debug_never_prints_plaintext_or_key_material() {
        let mut env = base();
        env.body = "the quick brown fox".to_owned();
        let mut a = sample_attachment();
        a.thumb = Some(vec![0x66; 8]);
        a.name = "secret-plan.pdf".to_owned();
        env.attachments = vec![a];
        let mut p = sample_preview();
        p.image = Some(vec![0x77; 8]);
        env.previews = vec![p];

        let s = format!("{env:?}");
        assert!(!s.contains("the quick brown fox"), "the body leaked: {s}");
        assert!(s.contains("body: <19 bytes>"), "{s}");
        assert!(s.contains("k_f: <redacted>"), "{s}");
        assert!(!s.contains("6, 6"), "k_f leaked: {s}");
        assert!(!s.contains("68, 68"), "the attachment key leaked: {s}");
        assert!(!s.contains("85, 85"), "the attachment nonce leaked: {s}");
        assert!(!s.contains("102, 102"), "the thumbnail leaked: {s}");
        assert!(!s.contains("119, 119"), "the preview image leaked: {s}");
        assert!(
            !s.contains("https://example.invalid/"),
            "the preview url leaked: {s}"
        );

        let a = format!("{:?}", env.attachments[0]);
        assert!(a.contains("key: <redacted>"), "{a}");
        assert!(a.contains("nonce: <redacted>"), "{a}");
        assert!(a.contains("thumb: Some(<8 bytes>)"), "{a}");
        assert!(a.contains("mime: \"image/jpeg\""), "{a}");
        assert!(a.contains("name: <15 bytes>"), "{a}");
        assert!(
            !s.contains("secret-plan") && !a.contains("secret-plan"),
            "the file name leaked: {s}"
        );

        let p = format!("{:?}", env.previews[0]);
        assert!(p.contains("image: Some(<8 bytes>)"), "{p}");

        // absence is still shown as absence
        let bare = base();
        assert!(format!("{bare:?}").contains("body: <0 bytes>"));
        assert!(format!("{:?}", sample_attachment()).contains("thumb: None"));
        assert!(format!("{:?}", sample_preview()).contains("image: None"));
    }

    #[test]
    fn verify_commitment_requires_exactly_thirty_two_authenticated_bytes() {
        let env = base();
        let c = env.commitment().unwrap();
        assert_eq!(env.verify_commitment(&c), Ok(()));
        assert_eq!(
            env.verify_commitment(&c[..31]),
            Err(ProtocolError::FrankMismatch)
        );
        assert_eq!(
            env.verify_commitment(&[]),
            Err(ProtocolError::FrankMismatch)
        );
        let mut wrong = c;
        wrong[0] ^= 0x01;
        assert_eq!(
            env.verify_commitment(&wrong),
            Err(ProtocolError::FrankMismatch)
        );
    }

    /// L-CORE-30: types 1..6 name their target and carry no files. Attacker statement: only the
    /// envelope's own sender can build one that fails; no member can make an honest envelope fail.
    #[test]
    fn types_one_to_six_require_reply_to_and_carry_no_attachments_or_previews() {
        for kind in [
            EnvelopeType::Edit,
            EnvelopeType::Delete,
            EnvelopeType::ReactionAdd,
            EnvelopeType::ReactionRemove,
            EnvelopeType::Pin,
            EnvelopeType::Unpin,
        ] {
            let mut env = base();
            env.kind = kind;
            env.body = match kind {
                EnvelopeType::Edit => "fixed".to_owned(),
                EnvelopeType::ReactionAdd | EnvelopeType::ReactionRemove => "👍".to_owned(),
                _ => String::new(),
            };
            assert_eq!(
                env.validate(),
                Err(ProtocolError::EnvelopeShape),
                "{kind:?} without reply_to"
            );
            env.reply_to = Some(MsgId::from_bytes([0x02; 16]));
            assert_eq!(env.validate(), Ok(()), "{kind:?} with reply_to");
            let bytes = env.encode().expect("encodes");
            assert_eq!(
                Envelope::decode(&bytes),
                Ok(env.clone()),
                "{kind:?} round trip"
            );
            let mut with_file = env.clone();
            with_file.attachments = vec![sample_attachment()];
            assert_eq!(
                with_file.validate(),
                Err(ProtocolError::EnvelopeShape),
                "{kind:?} with a file"
            );
            let mut with_preview = env.clone();
            with_preview.previews = vec![sample_preview()];
            assert_eq!(
                with_preview.validate(),
                Err(ProtocolError::EnvelopeShape),
                "{kind:?} with a preview"
            );
        }
        let mut message = base();
        message.attachments = vec![sample_attachment()];
        assert_eq!(
            message.validate(),
            Ok(()),
            "a message stands alone and carries files"
        );
        let mut tomb = base();
        tomb.kind = EnvelopeType::Delete;
        tomb.body = "x".to_owned();
        assert_eq!(
            tomb.validate(),
            Err(ProtocolError::EnvelopeLimit),
            "the body limit is checked first"
        );
    }

    #[test]
    fn an_attachment_name_is_at_most_255_bytes() {
        assert_eq!(MAX_NAME, 255);
        let mut env = base();
        let mut a = sample_attachment();
        a.name = "n".repeat(MAX_NAME);
        env.attachments = vec![a.clone()];
        let bytes = env.encode().expect("at the bound");
        assert_eq!(
            Envelope::decode(&bytes).expect("decodes").attachments[0].name,
            "n".repeat(MAX_NAME)
        );
        a.name = "n".repeat(MAX_NAME + 1);
        env.attachments = vec![a];
        assert_eq!(env.validate(), Err(ProtocolError::EnvelopeLimit));
        let mut e = Encoder::with_capacity(512);
        e.array(9)
            .uint(1)
            .bytes(&[0x01; 16])
            .uint(0)
            .null()
            .null()
            .text("");
        e.array(1);
        e.array(9)
            .bytes(&[0x03; 32])
            .bytes(&[0x44; 32])
            .bytes(&[0x55; 12])
            .uint(1)
            .text("image/png")
            .null()
            .null()
            .null()
            .text(&"n".repeat(MAX_NAME + 1));
        e.array(0);
        e.bytes(&[0x06; 32]);
        assert_eq!(
            Envelope::decode(&e.into_vec()),
            Err(ProtocolError::EnvelopeLimit)
        );
    }

    #[test]
    fn an_attachment_of_eight_elements_is_refused() {
        let mut e = Encoder::with_capacity(256);
        e.array(9)
            .uint(1)
            .bytes(&[0x01; 16])
            .uint(0)
            .null()
            .null()
            .text("");
        e.array(1);
        e.array(8)
            .bytes(&[0x03; 32])
            .bytes(&[0x44; 32])
            .bytes(&[0x55; 12])
            .uint(1)
            .text("image/png")
            .null()
            .null()
            .null();
        e.array(0);
        e.bytes(&[0x06; 32]);
        assert_eq!(
            Envelope::decode(&e.into_vec()),
            Err(ProtocolError::EnvelopeShape)
        );
    }
}
