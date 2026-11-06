//! Sealed Bot commands and replies (Bot v2 spec, "Sealed commands").
//!
//! A user's command to the Bot is signed by the user's device identity key (DSK, Ed25519) and
//! sealed with HPKE (RFC 9180 base mode, suite `0x0020/0x0001/0x0001`: DHKEM(X25519,
//! HKDF-SHA256), HKDF-SHA256, AES-128-GCM) to the Bot's per-community command key. The Bot
//! publishes that key signed by its own DSK. Each answer is sealed back to the one-time
//! `reply_key` the request carries; its private half (`ReplySecret`) never leaves the sealing
//! core.
//!
//! Wire shapes (deterministic CBOR, fixed-position arrays):
//!
//! - publication `[1, community_id, key_id, public_key, not_after, bot_device_id, sig]`, signed
//!   over `"dilla bot command key v1" || CBOR([1, community_id, key_id, public_key, not_after,
//!   bot_device_id])`;
//! - signed command `[1, request_id, community_id, channel_id|null, operation, args, sent_at,
//!   reply_key, user_id, device_id, sig]`, signed over `"dilla bot command v1" || CBOR(first ten
//!   elements)`;
//! - sealed command `[1, key_id, request_id, enc(32), ciphertext]`, `info = "dilla bot command
//!   v1"`, `aad = request_id`. The request id is carried outside the seal because the opener
//!   needs it as the associated data before it can decrypt anything, and it must equal the
//!   signed request's own id;
//! - sealed reply `[1, enc(32), ciphertext]`, `info = "dilla bot reply v1"`, `aad = request_id
//!   || seq_be32`.
//!
//! Every decoder is strict: version 1 only, exact element counts, exact identifier and key
//! lengths, no trailing bytes, and a refusal never carries caller data.

use std::collections::VecDeque;

use ed25519_dalek::{Signature, VerifyingKey};
use hpke_rs::hpke_types::{AeadAlgorithm, KdfAlgorithm, KemAlgorithm, TwoStageKdfAlgorithm};
use hpke_rs::{Hpke, HpkePrivateKey, Mode};
use hpke_rs_crypto::{HpkeCrypto, HpkeDefaultPrng, TryRng};
use hpke_rs_rust_crypto::HpkeRustCrypto;
use openmls_basic_credential::SignatureKeyPair;
use openmls_traits::signatures::Signer;
use openmls_traits::types::SignatureScheme;
use zeroize::{Zeroize, ZeroizeOnDrop, Zeroizing};

use super::CodecError;
use crate::cbor::{CborError, Decoder, Encoder, decode_strict};
use crate::error::CoreError;
use crate::identity::DeviceList;
use crate::ids::{ChannelId, CommunityId, DeviceId, UserId};

/// DHKEM(X25519, HKDF-SHA256).
pub const HPKE_KEM: u16 = 0x0020;
/// HKDF-SHA256.
pub const HPKE_KDF: u16 = 0x0001;
/// AES-128-GCM.
pub const HPKE_AEAD: u16 = 0x0001;
/// HPKE `info` of a sealed command, and the domain of the command signature.
pub const INFO_COMMAND: &[u8] = b"dilla bot command v1";
/// HPKE `info` of a sealed reply.
pub const INFO_REPLY: &[u8] = b"dilla bot reply v1";
/// Domain of the Bot's command-key publication signature.
pub const DOMAIN_COMMAND_KEY: &[u8] = b"dilla bot command key v1";
/// The largest sealed command the core seals or opens.
pub const MAX_SEALED_COMMAND_BYTES: usize = 16384;
/// The largest sealed reply the core seals or opens.
pub const MAX_SEALED_REPLY_BYTES: usize = 65536;
/// A command whose `sent_at` lies more than this many seconds before or after the verifier's
/// clock is stale.
pub const SENT_AT_WINDOW_SECS: u64 = 120;
/// The most arguments one command carries. The catalog's operations take at most a handful; the
/// cap keeps a hostile array head from costing more than the bytes it arrived in.
pub const MAX_COMMAND_ARGS: usize = 32;
/// The most reply secrets one `ReplyTable` holds. A command times out after 5 s and its answers
/// stream for seconds, so 256 outstanding commands is far beyond any real client. A client that
/// never forgets a handle loses its oldest one, not memory: each entry is 68 bytes.
pub const REPLY_TABLE_CAP: usize = 256;
/// The most verified command-key publications one `VerifiedKeys` holds: one per community.
pub const VERIFIED_KEYS_CAP: usize = 256;

const KEM_SUITE: [u8; 5] = [b'K', b'E', b'M', 0x00, 0x20];
const VERSION: u64 = 1;

/// Why a sealed command or a command-key publication is refused. No variant retains caller
/// data. `code` is the stable number the Bot helper reports (Task 15: 0 is "no refusal", 8 is
/// the helper's own replay refusal).
#[derive(Clone, Copy, PartialEq, Eq, Debug, thiserror::Error)]
pub enum CommandRefusal {
    /// The bytes are not a well-formed sealed command, or the seal does not open.
    #[error("E_BOT_MALFORMED")]
    Malformed,
    /// The `key_id` names no command key the opener holds. The core's opener is handed one key;
    /// the caller that picks the key by `key_id` reports this one.
    #[error("E_BOT_KEY_STALE")]
    UnknownKey,
    /// The signature does not verify under the listed key of the device it names.
    #[error("E_BOT_SIGNATURE")]
    BadSignature,
    /// The signed user or device is not the one dillad attached to the request.
    #[error("E_BOT_WRONG_DEVICE")]
    WrongDevice,
    /// The signing device is not in the signer's device list.
    #[error("E_DEVICE_UNLISTED")]
    DeviceUnlisted,
    /// The signing device is listed as revoked.
    #[error("E_DEVICE_REVOKED")]
    DeviceRevoked,
    /// `sent_at` is outside the 120 s window, or a publication is past its `not_after`.
    #[error("E_BOT_STALE")]
    Stale,
}

impl CommandRefusal {
    pub const fn code(self) -> u8 {
        match self {
            Self::Malformed => 1,
            Self::UnknownKey => 2,
            Self::BadSignature => 3,
            Self::WrongDevice => 4,
            Self::DeviceUnlisted => 5,
            Self::DeviceRevoked => 6,
            Self::Stale => 7,
        }
    }
}

impl From<CborError> for CommandRefusal {
    fn from(_: CborError) -> Self {
        Self::Malformed
    }
}

fn hpke_error() -> CoreError {
    CoreError::Crypto("hpke: operation failed".into())
}

fn is_zero(b: &[u8]) -> bool {
    b.iter().fold(0u8, |acc, x| acc | x) == 0
}

// ---------------------------------------------------------------------------------------------
// RFC 9180 primitives over hpke-rs and its RustCrypto provider.
// ---------------------------------------------------------------------------------------------

fn kem_labeled_extract(
    salt: &[u8],
    label: &[u8],
    ikm: &[u8],
) -> Result<Zeroizing<Vec<u8>>, CoreError> {
    let input = Zeroizing::new([b"HPKE-v1".as_slice(), &KEM_SUITE, label, ikm].concat());
    HpkeRustCrypto::kdf_extract(TwoStageKdfAlgorithm::HkdfSha256, salt, &input)
        .map(Zeroizing::new)
        .map_err(|_| hpke_error())
}

fn kem_labeled_expand(
    prk: &[u8],
    label: &[u8],
    info: &[u8],
    len: u16,
) -> Result<Zeroizing<Vec<u8>>, CoreError> {
    let labeled = [
        &len.to_be_bytes(),
        b"HPKE-v1".as_slice(),
        &KEM_SUITE,
        label,
        info,
    ]
    .concat();
    HpkeRustCrypto::kdf_expand(
        TwoStageKdfAlgorithm::HkdfSha256,
        prk,
        &labeled,
        usize::from(len),
    )
    .map(Zeroizing::new)
    .map_err(|_| hpke_error())
}

fn hpke() -> Result<Hpke<HpkeRustCrypto>, CoreError> {
    Hpke::<HpkeRustCrypto>::try_new(
        Mode::Base,
        KemAlgorithm::DhKem25519,
        KdfAlgorithm::HkdfSha256,
        AeadAlgorithm::Aes128Gcm,
    )
    .map_err(|_| hpke_error())
}

fn random32() -> Result<Zeroizing<[u8; 32]>, CoreError> {
    let mut prng = HpkeRustCrypto::try_prng().map_err(|_| hpke_error())?;
    let mut out = Zeroizing::new([0u8; 32]);
    prng.try_fill_bytes(out.as_mut_slice())
        .map_err(|_| hpke_error())?;
    Ok(out)
}

/// RFC 9180 section 7.1.3 `DeriveKeyPair` for DHKEM(X25519, HKDF-SHA256): `(sk, pk)`.
///
/// With a uniformly random `ikm` this is `GenerateKeyPair`; the vectors pass a fixed one.
pub fn derive_key_pair(ikm: &[u8; 32]) -> Result<(Zeroizing<[u8; 32]>, [u8; 32]), CoreError> {
    let dkp_prk = kem_labeled_extract(&[], b"dkp_prk", ikm)?;
    let sk_vec = kem_labeled_expand(&dkp_prk, b"sk", &[], 32)?;
    let mut sk = Zeroizing::new([0u8; 32]);
    sk.copy_from_slice(&sk_vec);
    let pk = HpkeRustCrypto::secret_to_public(KemAlgorithm::DhKem25519, sk.as_slice())
        .map_err(|_| hpke_error())?;
    let pk: [u8; 32] = pk.try_into().map_err(|_| hpke_error())?;
    Ok((sk, pk))
}

/// RFC 9180 base-mode single-shot seal whose ephemeral key is `DeriveKeyPair(ikm_e)`: `(enc,
/// ciphertext)`. Production callers pass 32 fresh random bytes; the vectors pass fixed ones.
#[doc(hidden)]
pub fn hpke_seal_base_derand(
    pk_r: &[u8; 32],
    info: &[u8],
    aad: &[u8],
    plaintext: &[u8],
    ikm_e: &[u8; 32],
) -> Result<([u8; 32], Vec<u8>), CoreError> {
    let (sk_e, pk_e) = derive_key_pair(ikm_e)?;
    let dh = Zeroizing::new(
        HpkeRustCrypto::dh(KemAlgorithm::DhKem25519, pk_r, sk_e.as_slice())
            .map_err(|_| hpke_error())?,
    );
    let kem_context = [pk_e.as_slice(), pk_r.as_slice()].concat();
    let eae_prk = kem_labeled_extract(&[], b"eae_prk", &dh)?;
    let shared = kem_labeled_expand(&eae_prk, b"shared_secret", &kem_context, 32)?;
    let mut context = hpke()?
        .key_schedule(&shared, info, &[], &[])
        .map_err(|_| hpke_error())?;
    let ciphertext = context.seal(aad, plaintext).map_err(|_| hpke_error())?;
    Ok((pk_e, ciphertext))
}

/// RFC 9180 base-mode single-shot open.
#[doc(hidden)]
pub fn hpke_open_base(
    sk_r: &[u8; 32],
    enc: &[u8; 32],
    info: &[u8],
    aad: &[u8],
    ciphertext: &[u8],
) -> Result<Vec<u8>, CoreError> {
    let sk = HpkePrivateKey::new(sk_r.to_vec());
    hpke()?
        .open(enc, &sk, info, aad, ciphertext, None, None, None)
        .map_err(|_| hpke_error())
}

// ---------------------------------------------------------------------------------------------
// The command key publication.
// ---------------------------------------------------------------------------------------------

/// The Bot's per-community command key, signed by the Bot's DSK.
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct CommandKeyPublication {
    pub community_id: CommunityId,
    pub key_id: [u8; 16],
    pub public_key: [u8; 32],
    pub not_after: u64,
    pub bot_device_id: DeviceId,
    pub sig: [u8; 64],
}

impl CommandKeyPublication {
    fn write_unsigned(&self, e: &mut Encoder, len: usize) {
        e.array(len)
            .uint(VERSION)
            .bytes(self.community_id.as_bytes())
            .bytes(&self.key_id)
            .bytes(&self.public_key)
            .uint(self.not_after)
            .bytes(self.bot_device_id.as_bytes());
    }

    /// `"dilla bot command key v1" || CBOR([1, community_id, key_id, public_key, not_after,
    /// bot_device_id])`
    pub fn signing_message(&self) -> Vec<u8> {
        let mut e = Encoder::with_capacity(DOMAIN_COMMAND_KEY.len() + 112);
        e.raw(DOMAIN_COMMAND_KEY);
        self.write_unsigned(&mut e, 6);
        e.into_vec()
    }

    /// The Bot side: signs a fresh publication with its DSK.
    pub fn sign(
        community_id: CommunityId,
        key_id: [u8; 16],
        public_key: [u8; 32],
        not_after: u64,
        bot_device_id: DeviceId,
        dsk: &SignatureKeyPair,
    ) -> Result<Self, CoreError> {
        let mut p = Self {
            community_id,
            key_id,
            public_key,
            not_after,
            bot_device_id,
            sig: [0; 64],
        };
        p.validate()
            .map_err(|_| CoreError::Bot(CodecError::Value))?;
        p.sig = sign_ed25519(dsk, &p.signing_message())?;
        Ok(p)
    }

    fn validate(&self) -> Result<(), CommandRefusal> {
        if is_zero(self.community_id.as_bytes())
            || is_zero(&self.key_id)
            || is_zero(&self.public_key)
            || is_zero(self.bot_device_id.as_bytes())
        {
            return Err(CommandRefusal::Malformed);
        }
        Ok(())
    }

    pub fn encode(&self) -> Vec<u8> {
        let mut e = Encoder::with_capacity(184);
        self.write_unsigned(&mut e, 7);
        e.bytes(&self.sig);
        e.into_vec()
    }

    pub fn decode(b: &[u8]) -> Result<Self, CodecError> {
        if b.len() > 256 {
            return Err(CodecError::Limit);
        }
        let p = decode_strict(b, |d| {
            d.array(7)?;
            if d.uint()? != VERSION {
                return Err(CborError::TypeMismatch {
                    expected: "command key v1",
                    offset: d.position(),
                });
            }
            Ok(Self {
                community_id: CommunityId::from_bytes(d.bytes_exact::<16>()?),
                key_id: d.bytes_exact::<16>()?,
                public_key: d.bytes_exact::<32>()?,
                not_after: d.uint()?,
                bot_device_id: DeviceId::from_bytes(d.bytes_exact::<16>()?),
                sig: d.bytes_exact::<64>()?,
            })
        })?;
        p.validate().map_err(|_| CodecError::Value)?;
        Ok(p)
    }

    /// The signer must be a listed, unrevoked device of the Bot user whose list this is (the
    /// caller picks the list by the Bot's user id and has verified it), and `now <= not_after`.
    pub fn verify(&self, bot_list: &DeviceList, now: u64) -> Result<(), CommandRefusal> {
        self.validate()?;
        let entry = bot_list
            .lookup(&self.bot_device_id)
            .ok_or(CommandRefusal::DeviceUnlisted)?;
        if entry.revoked_at.is_some() {
            return Err(CommandRefusal::DeviceRevoked);
        }
        verify_ed25519(&entry.dsk_pub, &self.signing_message(), &self.sig)?;
        if now > self.not_after {
            return Err(CommandRefusal::Stale);
        }
        Ok(())
    }
}

fn sign_ed25519(dsk: &SignatureKeyPair, message: &[u8]) -> Result<[u8; 64], CoreError> {
    if dsk.signature_scheme() != SignatureScheme::ED25519 {
        return Err(CoreError::Crypto("signer is not Ed25519".into()));
    }
    let sig = dsk
        .sign(message)
        .map_err(|_| CoreError::Crypto("signing failed".into()))?;
    sig.try_into()
        .map_err(|_| CoreError::Crypto("signature is not 64 bytes".into()))
}

fn verify_ed25519(key: &[u8; 32], message: &[u8], sig: &[u8; 64]) -> Result<(), CommandRefusal> {
    // `verify_strict` rejects small-order keys and non-canonical signatures.
    let key = VerifyingKey::from_bytes(key).map_err(|_| CommandRefusal::BadSignature)?;
    key.verify_strict(message, &Signature::from_bytes(sig))
        .map_err(|_| CommandRefusal::BadSignature)
}

// ---------------------------------------------------------------------------------------------
// The command.
// ---------------------------------------------------------------------------------------------

/// One argument of a command: the four CBOR kinds a catalog operation takes.
#[derive(Clone, PartialEq, Eq, Debug)]
pub enum CommandArg {
    Text(String),
    Uint(u64),
    Bytes(Vec<u8>),
    Null,
}

/// What the user asks the Bot. `reply_key` is the one-time X25519 public key answers are sealed
/// to; `seal_command` makes it and replaces whatever the caller put there.
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct CommandRequest {
    pub request_id: [u8; 16],
    pub community_id: CommunityId,
    pub channel_id: Option<ChannelId>,
    pub operation: u64,
    pub args: Vec<CommandArg>,
    pub sent_at: u64,
    pub reply_key: [u8; 32],
}

impl CommandRequest {
    fn validate(&self) -> Result<(), CommandRefusal> {
        if is_zero(&self.request_id)
            || is_zero(self.community_id.as_bytes())
            || self.channel_id.is_some_and(|c| is_zero(c.as_bytes()))
            || is_zero(&self.reply_key)
            || self.args.len() > MAX_COMMAND_ARGS
        {
            return Err(CommandRefusal::Malformed);
        }
        Ok(())
    }
}

/// An opened command: the request as the user's device signed it. Not yet trusted; pass it to
/// `verify_command`.
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct SignedCommand {
    pub request: CommandRequest,
    pub user_id: UserId,
    pub device_id: DeviceId,
    pub sig: [u8; 64],
}

fn write_command(e: &mut Encoder, len: usize, req: &CommandRequest, user: &UserId, dev: &DeviceId) {
    e.array(len)
        .uint(VERSION)
        .bytes(&req.request_id)
        .bytes(req.community_id.as_bytes());
    match &req.channel_id {
        Some(c) => e.bytes(c.as_bytes()),
        None => e.null(),
    };
    e.uint(req.operation).array(req.args.len());
    for arg in &req.args {
        match arg {
            CommandArg::Text(s) => e.text(s),
            CommandArg::Uint(n) => e.uint(*n),
            CommandArg::Bytes(b) => e.bytes(b),
            CommandArg::Null => e.null(),
        };
    }
    e.uint(req.sent_at)
        .bytes(&req.reply_key)
        .bytes(user.as_bytes())
        .bytes(dev.as_bytes());
}

fn read_arg(d: &mut Decoder<'_>) -> Result<CommandArg, CborError> {
    let raw = d.skip()?;
    let mismatch = || CborError::TypeMismatch {
        expected: "command argument",
        offset: 0,
    };
    match raw.first().map(|b| b >> 5) {
        Some(0) => Ok(CommandArg::Uint(decode_strict(raw, |x| x.uint())?)),
        Some(2) => Ok(CommandArg::Bytes(decode_strict(raw, |x| {
            Ok(x.bytes()?.to_vec())
        })?)),
        Some(3) => Ok(CommandArg::Text(decode_strict(raw, |x| {
            Ok(x.text()?.to_owned())
        })?)),
        // `skip` admits no simple value other than null.
        Some(7) if raw == [0xf6] => Ok(CommandArg::Null),
        _ => Err(mismatch()),
    }
}

impl SignedCommand {
    /// `"dilla bot command v1" || CBOR([1, request_id, community_id, channel_id|null,
    /// operation, args, sent_at, reply_key, user_id, device_id])`
    pub fn signing_message(&self) -> Vec<u8> {
        let mut e = Encoder::with_capacity(INFO_COMMAND.len() + 160);
        e.raw(INFO_COMMAND);
        write_command(&mut e, 10, &self.request, &self.user_id, &self.device_id);
        e.into_vec()
    }

    pub fn encode(&self) -> Vec<u8> {
        let mut e = Encoder::with_capacity(224);
        write_command(&mut e, 11, &self.request, &self.user_id, &self.device_id);
        e.bytes(&self.sig);
        e.into_vec()
    }

    pub fn decode(b: &[u8]) -> Result<Self, CommandRefusal> {
        if b.len() > MAX_SEALED_COMMAND_BYTES {
            return Err(CommandRefusal::Malformed);
        }
        let cmd = decode_strict(b, |d| {
            d.array(11)?;
            if d.uint()? != VERSION {
                return Err(CborError::TypeMismatch {
                    expected: "command v1",
                    offset: d.position(),
                });
            }
            let request_id = d.bytes_exact::<16>()?;
            let community_id = CommunityId::from_bytes(d.bytes_exact::<16>()?);
            let channel_id = d.opt_bytes_exact::<16>()?.map(ChannelId::from_bytes);
            let operation = d.uint()?;
            let n = d.array_len()?;
            if n > MAX_COMMAND_ARGS {
                return Err(CborError::WrongArrayLen {
                    expected: MAX_COMMAND_ARGS,
                    actual: n,
                });
            }
            let mut args = Vec::with_capacity(n);
            for _ in 0..n {
                args.push(read_arg(d)?);
            }
            let sent_at = d.uint()?;
            let reply_key = d.bytes_exact::<32>()?;
            let user_id = UserId::from_bytes(d.bytes_exact::<16>()?);
            let device_id = DeviceId::from_bytes(d.bytes_exact::<16>()?);
            let sig = d.bytes_exact::<64>()?;
            Ok(Self {
                request: CommandRequest {
                    request_id,
                    community_id,
                    channel_id,
                    operation,
                    args,
                    sent_at,
                    reply_key,
                },
                user_id,
                device_id,
                sig,
            })
        })?;
        cmd.request.validate()?;
        if is_zero(cmd.user_id.as_bytes()) || is_zero(cmd.device_id.as_bytes()) {
            return Err(CommandRefusal::Malformed);
        }
        Ok(cmd)
    }
}

/// Signs `req` as `user`/`device` with `dsk` and seals it to the publication's command key. The
/// returned secret opens the answers; `req.reply_key` is replaced by its public half.
pub fn seal_command(
    publication: &CommandKeyPublication,
    req: &CommandRequest,
    user: UserId,
    device: DeviceId,
    dsk: &SignatureKeyPair,
) -> Result<(Vec<u8>, ReplySecret), CoreError> {
    let ephemeral = random32()?;
    let reply = random32()?;
    seal_command_derand(publication, req, user, device, dsk, &ephemeral, &reply)
}

/// `seal_command` with the HPKE ephemeral key and the reply key derived from fixed input key
/// material (`DeriveKeyPair`). For the conformance vectors; production calls `seal_command`.
#[doc(hidden)]
pub fn seal_command_derand(
    publication: &CommandKeyPublication,
    req: &CommandRequest,
    user: UserId,
    device: DeviceId,
    dsk: &SignatureKeyPair,
    ephemeral_ikm: &[u8; 32],
    reply_ikm: &[u8; 32],
) -> Result<(Vec<u8>, ReplySecret), CoreError> {
    if publication.validate().is_err() {
        return Err(CoreError::Bot(CodecError::Value));
    }
    if publication.community_id != req.community_id {
        return Err(CoreError::Bot(CodecError::Context));
    }
    let secret = ReplySecret::derive(reply_ikm)?;
    let mut cmd = SignedCommand {
        request: req.clone(),
        user_id: user,
        device_id: device,
        sig: [0; 64],
    };
    cmd.request.reply_key = secret.public_key();
    if cmd.request.validate().is_err() || is_zero(user.as_bytes()) || is_zero(device.as_bytes()) {
        return Err(CoreError::Bot(CodecError::Value));
    }
    cmd.sig = sign_ed25519(dsk, &cmd.signing_message())?;
    let plaintext = Zeroizing::new(cmd.encode());
    if plaintext.len() > MAX_SEALED_COMMAND_BYTES {
        return Err(CoreError::Bot(CodecError::Limit));
    }
    let request_id = cmd.request.request_id;
    let (enc, ciphertext) = hpke_seal_base_derand(
        &publication.public_key,
        INFO_COMMAND,
        &request_id,
        &plaintext,
        ephemeral_ikm,
    )?;
    let mut e = Encoder::with_capacity(ciphertext.len() + 80);
    e.array(5)
        .uint(VERSION)
        .bytes(&publication.key_id)
        .bytes(&request_id)
        .bytes(&enc)
        .bytes(&ciphertext);
    let sealed = e.into_vec();
    if sealed.len() > MAX_SEALED_COMMAND_BYTES {
        return Err(CoreError::Bot(CodecError::Limit));
    }
    Ok((sealed, secret))
}

struct SealedCommand<'a> {
    key_id: [u8; 16],
    request_id: [u8; 16],
    enc: [u8; 32],
    ciphertext: &'a [u8],
}

fn read_sealed_command(sealed: &[u8]) -> Result<SealedCommand<'_>, CborError> {
    let mut d = Decoder::new(sealed);
    d.array(5)?;
    if d.uint()? != VERSION {
        return Err(CborError::TypeMismatch {
            expected: "sealed command v1",
            offset: d.position(),
        });
    }
    let s = SealedCommand {
        key_id: d.bytes_exact::<16>()?,
        request_id: d.bytes_exact::<16>()?,
        enc: d.bytes_exact::<32>()?,
        ciphertext: d.bytes()?,
    };
    d.finish()?;
    Ok(s)
}

fn decode_sealed_command(sealed: &[u8]) -> Result<SealedCommand<'_>, CommandRefusal> {
    if sealed.len() > MAX_SEALED_COMMAND_BYTES {
        return Err(CommandRefusal::Malformed);
    }
    let s = read_sealed_command(sealed)?;
    if is_zero(&s.key_id) || is_zero(&s.request_id) {
        return Err(CommandRefusal::Malformed);
    }
    Ok(s)
}

/// The `key_id` a sealed command names, so the opener can pick the private key (or refuse with
/// `UnknownKey`) before decrypting.
pub fn sealed_command_key_id(sealed: &[u8]) -> Result<[u8; 16], CommandRefusal> {
    decode_sealed_command(sealed).map(|s| s.key_id)
}

/// The request id a sealed command carries outside the seal (the AEAD's associated data).
pub fn sealed_command_request_id(sealed: &[u8]) -> Result<[u8; 16], CommandRefusal> {
    decode_sealed_command(sealed).map(|s| s.request_id)
}

/// Opens a sealed command with the command key's private half. The result is not trusted until
/// `verify_command` passes. A seal that does not open under this key is `Malformed`: the caller
/// selects the key by `key_id` and reports `UnknownKey` itself when it holds none.
pub fn open_command(
    private_key: &[u8; 32],
    sealed: &[u8],
) -> Result<SignedCommand, CommandRefusal> {
    let s = decode_sealed_command(sealed)?;
    let plaintext = Zeroizing::new(
        hpke_open_base(
            private_key,
            &s.enc,
            INFO_COMMAND,
            &s.request_id,
            s.ciphertext,
        )
        .map_err(|_| CommandRefusal::Malformed)?,
    );
    let cmd = SignedCommand::decode(&plaintext)?;
    // The signed id must be the one dillad deduplicates on and the AEAD authenticated.
    if cmd.request.request_id != s.request_id {
        return Err(CommandRefusal::Malformed);
    }
    Ok(cmd)
}

/// Checks an opened command against the signer's device list (the caller's cached, verified
/// list of `attached_user`) and the identity dillad attached outside the seal.
pub fn verify_command(
    cmd: &SignedCommand,
    list: &DeviceList,
    attached_user: UserId,
    attached_device: DeviceId,
    now: u64,
) -> Result<CommandRequest, CommandRefusal> {
    if cmd.user_id != attached_user || cmd.device_id != attached_device {
        return Err(CommandRefusal::WrongDevice);
    }
    if list.unsigned.user_id != attached_user {
        return Err(CommandRefusal::DeviceUnlisted);
    }
    let entry = list
        .lookup(&cmd.device_id)
        .ok_or(CommandRefusal::DeviceUnlisted)?;
    if entry.revoked_at.is_some() {
        return Err(CommandRefusal::DeviceRevoked);
    }
    verify_ed25519(&entry.dsk_pub, &cmd.signing_message(), &cmd.sig)?;
    if now.abs_diff(cmd.request.sent_at) > SENT_AT_WINDOW_SECS {
        return Err(CommandRefusal::Stale);
    }
    Ok(cmd.request.clone())
}

// ---------------------------------------------------------------------------------------------
// Replies.
// ---------------------------------------------------------------------------------------------

/// The private half of a request's one-time reply key. Zeroized on drop; never cloned, never
/// encoded, and its `Debug` prints nothing secret.
#[derive(Zeroize, ZeroizeOnDrop)]
pub struct ReplySecret {
    sk: [u8; 32],
    #[zeroize(skip)]
    pk: [u8; 32],
}

impl core::fmt::Debug for ReplySecret {
    fn fmt(&self, f: &mut core::fmt::Formatter<'_>) -> core::fmt::Result {
        f.debug_struct("ReplySecret")
            .field("pk", &self.pk)
            .finish_non_exhaustive()
    }
}

impl ReplySecret {
    /// A fresh one-time reply key.
    pub fn generate() -> Result<Self, CoreError> {
        Self::derive(&*random32()?)
    }

    /// `DeriveKeyPair(ikm)`. For the conformance vectors; production uses `generate`.
    #[doc(hidden)]
    pub fn derive(ikm: &[u8; 32]) -> Result<Self, CoreError> {
        let (sk, pk) = derive_key_pair(ikm)?;
        Ok(Self { sk: *sk, pk })
    }

    pub fn public_key(&self) -> [u8; 32] {
        self.pk
    }
}

fn reply_aad(request_id: &[u8; 16], seq: u32) -> [u8; 20] {
    let mut aad = [0u8; 20];
    aad[..16].copy_from_slice(request_id);
    aad[16..].copy_from_slice(&seq.to_be_bytes());
    aad
}

/// The Bot side: seals answer number `seq` of `request_id` to the request's `reply_key`.
pub fn seal_reply(
    reply_key: &[u8; 32],
    request_id: [u8; 16],
    seq: u32,
    body: &[u8],
) -> Result<Vec<u8>, CoreError> {
    seal_reply_derand(reply_key, request_id, seq, body, &*random32()?)
}

/// `seal_reply` with a fixed ephemeral `ikm`. For the conformance vectors.
#[doc(hidden)]
pub fn seal_reply_derand(
    reply_key: &[u8; 32],
    request_id: [u8; 16],
    seq: u32,
    body: &[u8],
    ephemeral_ikm: &[u8; 32],
) -> Result<Vec<u8>, CoreError> {
    if body.len() > MAX_SEALED_REPLY_BYTES {
        return Err(CoreError::Bot(CodecError::Limit));
    }
    let (enc, ciphertext) = hpke_seal_base_derand(
        reply_key,
        INFO_REPLY,
        &reply_aad(&request_id, seq),
        body,
        ephemeral_ikm,
    )?;
    let mut e = Encoder::with_capacity(ciphertext.len() + 48);
    e.array(3).uint(VERSION).bytes(&enc).bytes(&ciphertext);
    let sealed = e.into_vec();
    if sealed.len() > MAX_SEALED_REPLY_BYTES {
        return Err(CoreError::Bot(CodecError::Limit));
    }
    Ok(sealed)
}

fn read_sealed_reply(sealed: &[u8]) -> Result<([u8; 32], &[u8]), CborError> {
    let mut d = Decoder::new(sealed);
    d.array(3)?;
    if d.uint()? != VERSION {
        return Err(CborError::TypeMismatch {
            expected: "sealed reply v1",
            offset: d.position(),
        });
    }
    let out = (d.bytes_exact::<32>()?, d.bytes()?);
    d.finish()?;
    Ok(out)
}

/// Opens answer number `seq` of `request_id` with the request's reply secret.
pub fn open_reply(
    secret: &ReplySecret,
    request_id: [u8; 16],
    seq: u32,
    sealed: &[u8],
) -> Result<Vec<u8>, CoreError> {
    if sealed.len() > MAX_SEALED_REPLY_BYTES {
        return Err(CoreError::Bot(CodecError::Limit));
    }
    let (enc, ciphertext) =
        read_sealed_reply(sealed).map_err(|_| CoreError::Bot(CodecError::Format))?;
    hpke_open_base(
        &secret.sk,
        &enc,
        INFO_REPLY,
        &reply_aad(&request_id, seq),
        ciphertext,
    )
}

// ---------------------------------------------------------------------------------------------
// Bounded in-memory tables for a client that seals commands.
// ---------------------------------------------------------------------------------------------

/// Reply secrets behind opaque `u32` handles, so the secret never crosses into JavaScript.
///
/// At most `REPLY_TABLE_CAP` entries: inserting into a full table evicts (and zeroizes) the
/// oldest. Handles count up from 1, skip 0, and are not reused until the counter wraps, by
/// which time every older handle has long been evicted.
#[derive(Debug)]
pub struct ReplyTable {
    entries: VecDeque<(u32, ReplySecret)>,
    next: u32,
}

impl Default for ReplyTable {
    fn default() -> Self {
        Self {
            entries: VecDeque::new(),
            next: 1,
        }
    }
}

impl ReplyTable {
    pub fn insert(&mut self, secret: ReplySecret) -> u32 {
        while self.entries.len() >= REPLY_TABLE_CAP {
            self.entries.pop_front();
        }
        let handle = self.next;
        self.next = match self.next.wrapping_add(1) {
            0 => 1,
            n => n,
        };
        self.entries.push_back((handle, secret));
        handle
    }

    pub fn get(&self, handle: u32) -> Option<&ReplySecret> {
        self.entries
            .iter()
            .find(|(h, _)| *h == handle)
            .map(|(_, s)| s)
    }

    /// Drops (and so zeroizes) the secret; false if the handle holds none.
    pub fn forget(&mut self, handle: u32) -> bool {
        match self.entries.iter().position(|(h, _)| *h == handle) {
            Some(i) => {
                self.entries.remove(i);
                true
            }
            None => false,
        }
    }

    pub fn len(&self) -> usize {
        self.entries.len()
    }

    pub fn is_empty(&self) -> bool {
        self.entries.is_empty()
    }

    pub fn clear(&mut self) {
        self.entries.clear();
    }
}

/// Command-key publications this client verified against the Bot user's stored device list, one
/// per community. Sealing accepts only the exact bytes verified here, and only until their
/// `not_after`, so a publication can never be sealed to without passing `verify` first.
/// At most `VERIFIED_KEYS_CAP` communities; the oldest is evicted.
#[derive(Debug, Default)]
pub struct VerifiedKeys {
    entries: VecDeque<(Vec<u8>, CommandKeyPublication)>,
}

impl VerifiedKeys {
    /// Records a publication the caller has just verified, replacing its community's previous.
    pub fn remember(&mut self, publication: &CommandKeyPublication) {
        self.entries
            .retain(|(_, p)| p.community_id != publication.community_id);
        while self.entries.len() >= VERIFIED_KEYS_CAP {
            self.entries.pop_front();
        }
        self.entries
            .push_back((publication.encode(), publication.clone()));
    }

    /// The publication `bytes` decodes to, if exactly these bytes were verified and are not
    /// expired at `now`.
    pub fn check(&self, bytes: &[u8], now: u64) -> Result<CommandKeyPublication, CommandRefusal> {
        let p = CommandKeyPublication::decode(bytes).map_err(|_| CommandRefusal::Malformed)?;
        let (_, verified) = self
            .entries
            .iter()
            .find(|(b, _)| b.as_slice() == bytes)
            .ok_or(CommandRefusal::UnknownKey)?;
        if now > verified.not_after {
            return Err(CommandRefusal::Stale);
        }
        Ok(p)
    }

    pub fn len(&self) -> usize {
        self.entries.len()
    }

    pub fn is_empty(&self) -> bool {
        self.entries.is_empty()
    }
}
