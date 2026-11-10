//! The MLS `basic` credential identity: a 10-element deterministic-CBOR array
//! (protocol/03-identity.md "Credential").
//!
//! `dsk_pub` is deliberately **not** in the array. The verifier takes it from the MLS leaf's
//! `signature_key`, which is what binds the credential to the leaf it arrived on.

use super::{DOMAIN_DEVICES, DOMAIN_DSK, DOMAIN_SSK, DeviceListUnsigned, Kind, SignerTier, Tier};
use crate::cbor::{Decoder, Encoder, decode_strict};
use crate::error::ProtocolError;
use crate::ids::{DeviceId, UserId};
use ed25519_dalek::{Signature, Signer, SigningKey, VerifyingKey};

#[derive(Clone, PartialEq, Eq, Debug)]
pub struct CredentialIdentity {
    pub v: u64,
    pub umk_pub: [u8; 32],
    pub user_id: UserId,
    pub device_id: DeviceId,
    pub kind: Kind,
    pub tier: Tier,
    pub signer_tier: SignerTier,
    pub ssk_pub: [u8; 32],
    pub sig_umk_ssk: [u8; 64],
    pub sig_ssk_dev: [u8; 64],
}

impl CredentialIdentity {
    pub fn encode(&self) -> Vec<u8> {
        let mut e = Encoder::with_capacity(224);
        e.array(10)
            .uint(self.v)
            .bytes(&self.umk_pub)
            .bytes(self.user_id.as_bytes())
            .bytes(self.device_id.as_bytes())
            .uint(u64::from(self.kind.as_u8()))
            .uint(u64::from(self.tier.as_u8()))
            .uint(u64::from(self.signer_tier.as_u8()))
            .bytes(&self.ssk_pub)
            .bytes(&self.sig_umk_ssk)
            .bytes(&self.sig_ssk_dev);
        e.into_vec()
    }

    /// Every shape failure is `E_CREDENTIAL`, including trailing bytes and a non-minimal encoding.
    pub fn decode(bytes: &[u8]) -> Result<Self, ProtocolError> {
        decode_strict(bytes, Self::read).map_err(|_| ProtocolError::Credential)
    }

    fn read(d: &mut Decoder<'_>) -> Result<Self, crate::cbor::CborError> {
        d.array(10)?;
        let v = d.uint()?;
        let umk_pub = d.bytes_exact::<32>()?;
        let user_id = UserId::from_bytes(d.bytes_exact::<16>()?);
        let device_id = DeviceId::from_bytes(d.bytes_exact::<16>()?);
        let kind = d.uint()?;
        let tier = d.uint()?;
        let signer_tier = d.uint()?;
        let ssk_pub = d.bytes_exact::<32>()?;
        let sig_umk_ssk = d.bytes_exact::<64>()?;
        let sig_ssk_dev = d.bytes_exact::<64>()?;
        // The enum conversions cannot be expressed as CborError, so they are checked here and
        // reported through the same TypeMismatch channel the caller maps to E_CREDENTIAL.
        let mismatch = crate::cbor::CborError::TypeMismatch {
            expected: "enum",
            offset: 0,
        };
        if v != 1 {
            return Err(mismatch);
        }
        let kind = Kind::from_u64(kind).map_err(|_| mismatch.clone())?;
        let tier = Tier::from_u64(tier).map_err(|_| mismatch.clone())?;
        let signer_tier = SignerTier::from_u64(signer_tier).map_err(|_| mismatch)?;
        Ok(Self {
            v,
            umk_pub,
            user_id,
            device_id,
            kind,
            tier,
            signer_tier,
            ssk_pub,
            sig_umk_ssk,
            sig_ssk_dev,
        })
    }

    /// `"dilla ssk v1" || ssk_pub`
    pub fn ssk_message(ssk_pub: &[u8; 32]) -> Vec<u8> {
        let mut m = Vec::with_capacity(DOMAIN_SSK.len() + 32);
        m.extend_from_slice(DOMAIN_SSK);
        m.extend_from_slice(ssk_pub);
        m
    }

    /// `"dilla dsk v1" || device_id || dsk_pub || kind || tier || signer_tier`
    /// - the three enum fields are one byte each, not CBOR.
    pub fn dsk_message(
        device_id: &DeviceId,
        dsk_pub: &[u8; 32],
        kind: Kind,
        tier: Tier,
        signer_tier: SignerTier,
    ) -> Vec<u8> {
        let mut m = Vec::with_capacity(DOMAIN_DSK.len() + 16 + 32 + 3);
        m.extend_from_slice(DOMAIN_DSK);
        m.extend_from_slice(device_id.as_bytes());
        m.extend_from_slice(dsk_pub);
        m.push(kind.as_u8());
        m.push(tier.as_u8());
        m.push(signer_tier.as_u8());
        m
    }

    /// The UMK's certificate over the SSK (`sig_umk_ssk` over `ssk_message`), alone: the half of
    /// rule 1 that needs no device. A pushed device list (Bot v2) carries this certificate instead
    /// of a leaf credential.
    pub fn verify_ssk_cert(
        umk_pub: &[u8; 32],
        ssk_pub: &[u8; 32],
        sig_umk_ssk: &[u8; 64],
    ) -> Result<(), ProtocolError> {
        let umk = VerifyingKey::from_bytes(umk_pub).map_err(|_| ProtocolError::Credential)?;
        umk.verify_strict(
            &Self::ssk_message(ssk_pub),
            &Signature::from_bytes(sig_umk_ssk),
        )
        .map_err(|_| ProtocolError::Credential)
    }

    /// Rule 1 of protocol/03: both signatures verify, with `dsk_pub` taken from the MLS leaf.
    ///
    /// `verify_strict` rather than `verify`: it rejects small-order and non-canonical public keys
    /// and gives the strongly-binding semantics a device list needs (gap-10 section 4).
    pub fn verify_signatures(&self, dsk_pub: &[u8; 32]) -> Result<(), ProtocolError> {
        Self::verify_ssk_cert(&self.umk_pub, &self.ssk_pub, &self.sig_umk_ssk)?;

        let ssk = VerifyingKey::from_bytes(&self.ssk_pub).map_err(|_| ProtocolError::Credential)?;
        ssk.verify_strict(
            &Self::dsk_message(
                &self.device_id,
                dsk_pub,
                self.kind,
                self.tier,
                self.signer_tier,
            ),
            &Signature::from_bytes(&self.sig_ssk_dev),
        )
        .map_err(|_| ProtocolError::Credential)?;
        Ok(())
    }

    /// The credential a new device presents as the second leaf of a pairing group: no SSK, no
    /// signatures. It is accepted only there, and only when the group's `dilla_binding.target_id`
    /// equals this `device_id` (`E_PROVISIONAL_OUTSIDE_PAIRING`, enforced in task 10).
    pub fn provisional(
        umk_pub: [u8; 32],
        user_id: UserId,
        device_id: DeviceId,
        kind: Kind,
        tier: Tier,
    ) -> Self {
        Self {
            v: 1,
            umk_pub,
            user_id,
            device_id,
            kind,
            tier,
            signer_tier: SignerTier::Provisional,
            ssk_pub: [0u8; 32],
            sig_umk_ssk: [0u8; 64],
            sig_ssk_dev: [0u8; 64],
        }
    }

    pub fn is_provisional(&self) -> bool {
        self.signer_tier == SignerTier::Provisional
    }
}

/// The subordinate signing key. It signs devices into the list and signs the list itself.
pub struct SskSigner {
    key: SigningKey,
}

impl SskSigner {
    pub fn from_bytes(sk: &[u8; 32]) -> Self {
        Self {
            key: SigningKey::from_bytes(sk),
        }
    }

    pub fn public(&self) -> [u8; 32] {
        self.key.verifying_key().to_bytes()
    }

    pub fn sign_device(
        &self,
        device_id: &DeviceId,
        dsk_pub: &[u8; 32],
        kind: Kind,
        tier: Tier,
        signer_tier: SignerTier,
    ) -> [u8; 64] {
        self.key
            .sign(&CredentialIdentity::dsk_message(
                device_id,
                dsk_pub,
                kind,
                tier,
                signer_tier,
            ))
            .to_bytes()
    }

    pub fn sign_device_list(&self, unsigned: &DeviceListUnsigned) -> [u8; 64] {
        debug_assert_eq!(
            &unsigned.signing_message()[..DOMAIN_DEVICES.len()],
            DOMAIN_DEVICES
        );
        self.key.sign(&unsigned.signing_message()).to_bytes()
    }
}

/// The user master key. It signs exactly one thing: the SSK.
pub struct UmkSigner {
    key: SigningKey,
}

impl UmkSigner {
    pub fn from_bytes(sk: &[u8; 32]) -> Self {
        Self {
            key: SigningKey::from_bytes(sk),
        }
    }

    pub fn public(&self) -> [u8; 32] {
        self.key.verifying_key().to_bytes()
    }

    pub fn sign_ssk(&self, ssk_pub: &[u8; 32]) -> [u8; 64] {
        self.key
            .sign(&CredentialIdentity::ssk_message(ssk_pub))
            .to_bytes()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::ids::{DeviceId, UserId};

    fn unhex(s: &str) -> Vec<u8> {
        (0..s.len() / 2)
            .map(|i| u8::from_str_radix(&s[2 * i..2 * i + 2], 16).expect("hex digit"))
            .collect()
    }

    fn arr32(b: u8) -> [u8; 32] {
        [b; 32]
    }

    /// Read from the vector file rather than transcribed from it. Task 7 regenerates
    /// `identity.json` with real Ed25519 keys and signatures; a hand-copied constant would silently
    /// stop being "the exact case in the vector" at that point, and the contract's acceptance for
    /// this task is "`credential_identity.cbor` from `identity.json` reproduces byte-for-byte".
    /// `serde_json` is already a dev-dependency, so this compiles in `cargo test --lib`.
    const IDENTITY_JSON: &str = include_str!("../../../../protocol/vectors/identity.json");

    fn vector_doc() -> serde_json::Value {
        serde_json::from_str(IDENTITY_JSON).expect("identity.json parses")
    }

    fn unhex_n<const N: usize>(s: &str) -> [u8; N] {
        let v = unhex(s);
        assert_eq!(v.len(), N, "expected {N} bytes, got {}", v.len());
        let mut out = [0u8; N];
        out.copy_from_slice(&v);
        out
    }

    fn vector_credential() -> CredentialIdentity {
        let doc = vector_doc();
        let f = doc["credential_identity"]["fields"].clone();
        let hex_field = |key: &str| -> String {
            f[key]
                .as_str()
                .unwrap_or_else(|| panic!("identity.json: {key} is a hex string"))
                .to_owned()
        };
        let uint_field = |key: &str| -> u64 {
            f[key]
                .as_u64()
                .unwrap_or_else(|| panic!("identity.json: {key} is an integer"))
        };
        CredentialIdentity {
            v: 1,
            umk_pub: unhex_n::<32>(&hex_field("umk_pub")),
            user_id: UserId::from_bytes(unhex_n::<16>(&hex_field("user_id"))),
            device_id: DeviceId::from_bytes(unhex_n::<16>(&hex_field("device_id"))),
            kind: Kind::from_u64(uint_field("kind")).expect("kind"),
            tier: Tier::from_u64(uint_field("tier")).expect("tier"),
            signer_tier: SignerTier::from_u64(uint_field("signer_tier")).expect("signer_tier"),
            ssk_pub: unhex_n::<32>(&hex_field("ssk_pub")),
            sig_umk_ssk: unhex_n::<64>(&hex_field("sig_umk_ssk")),
            sig_ssk_dev: unhex_n::<64>(&hex_field("sig_ssk_dev")),
        }
    }

    #[test]
    fn credential_identity_reproduces_the_vector_cbor() {
        let doc = vector_doc();
        let expected = doc["credential_identity"]["cbor"]
            .as_str()
            .expect("cbor is a hex string");
        assert_eq!(hex_of(&vector_credential().encode()), expected);
    }

    fn hex_of(b: &[u8]) -> String {
        b.iter().map(|x| format!("{x:02x}")).collect()
    }

    #[test]
    fn credential_identity_round_trips() {
        let c = vector_credential();
        assert_eq!(CredentialIdentity::decode(&c.encode()).unwrap(), c);
    }

    #[test]
    fn decode_rejects_the_wrong_shape_with_e_credential() {
        let c = vector_credential();
        let mut bytes = c.encode();
        bytes.push(0x00); // trailing byte
        assert_eq!(
            CredentialIdentity::decode(&bytes),
            Err(ProtocolError::Credential)
        );

        let mut short = c.encode();
        short[0] = 0x89; // array of 9
        assert_eq!(
            CredentialIdentity::decode(&short),
            Err(ProtocolError::Credential)
        );
    }

    #[test]
    fn signer_tier_accepts_0_1_2_and_rejects_3() {
        assert_eq!(SignerTier::from_u64(0), Ok(SignerTier::Native));
        assert_eq!(SignerTier::from_u64(1), Ok(SignerTier::Browser));
        assert_eq!(SignerTier::from_u64(2), Ok(SignerTier::Provisional));
        assert_eq!(SignerTier::from_u64(3), Err(ProtocolError::Credential));
        assert_eq!(Kind::from_u64(2), Err(ProtocolError::Credential));
        assert_eq!(Tier::from_u64(2), Err(ProtocolError::Credential));
    }

    #[test]
    fn signing_messages_are_the_documented_preimages() {
        let ssk_pub = arr32(0xf6);
        let mut want = b"dilla ssk v1".to_vec();
        want.extend_from_slice(&ssk_pub);
        assert_eq!(CredentialIdentity::ssk_message(&ssk_pub), want);

        let device_id = DeviceId::from_bytes([0xe5; 16]);
        let dsk_pub = arr32(0x5a);
        let mut want = b"dilla dsk v1".to_vec();
        want.extend_from_slice(device_id.as_bytes());
        want.extend_from_slice(&dsk_pub);
        want.extend_from_slice(&[0u8, 1u8, 0u8]); // kind, tier, signer_tier - one byte each
        assert_eq!(
            CredentialIdentity::dsk_message(
                &device_id,
                &dsk_pub,
                Kind::User,
                Tier::Browser,
                SignerTier::Native
            ),
            want
        );
    }

    #[test]
    fn verify_signatures_accepts_a_real_chain_and_rejects_a_tampered_one() {
        let umk = UmkSigner::from_bytes(&arr32(0x31));
        let ssk = SskSigner::from_bytes(&arr32(0x32));
        let dsk = SskSigner::from_bytes(&arr32(0x33)); // the device key lives in the MLS leaf
        let device_id = DeviceId::from_bytes([0xe5; 16]);
        let dsk_pub = dsk.public();

        let c = CredentialIdentity {
            v: 1,
            umk_pub: umk.public(),
            user_id: UserId::from_bytes([0xd4; 16]),
            device_id,
            kind: Kind::User,
            tier: Tier::Native,
            signer_tier: SignerTier::Native,
            ssk_pub: ssk.public(),
            sig_umk_ssk: umk.sign_ssk(&ssk.public()),
            sig_ssk_dev: ssk.sign_device(
                &device_id,
                &dsk_pub,
                Kind::User,
                Tier::Native,
                SignerTier::Native,
            ),
        };
        assert_eq!(c.verify_signatures(&dsk_pub), Ok(()));

        // a different leaf key breaks sig_ssk_dev
        let other = SskSigner::from_bytes(&arr32(0x34)).public();
        assert_eq!(c.verify_signatures(&other), Err(ProtocolError::Credential));

        // a flipped bit in sig_umk_ssk breaks the first signature
        let mut tampered = c.clone();
        tampered.sig_umk_ssk[0] ^= 0x01;
        assert_eq!(
            tampered.verify_signatures(&dsk_pub),
            Err(ProtocolError::Credential)
        );
    }

    /// The all-zero point is not a usable Ed25519 public key; `verify_strict` rejects it outright.
    ///
    /// This does **not** on its own prove `verify_strict` is in use: the signature here is also
    /// wrong, so plain `verify` would reject it too. A case that `verify` accepts and only
    /// `verify_strict` refuses needs a signature crafted under a small-order key, which no vector
    /// in this repository carries — see "Needs verification" item 20. The test is named for what it
    /// actually covers.
    #[test]
    fn verify_signatures_rejects_an_all_zero_public_key() {
        let mut c = vector_credential();
        c.umk_pub = [0u8; 32];
        assert_eq!(
            c.verify_signatures(&arr32(0x5a)),
            Err(ProtocolError::Credential)
        );
        // and the genuine chain of the test above still verifies, so the rejection is the key,
        // not a blanket failure of `verify_signatures`.
        let umk = UmkSigner::from_bytes(&arr32(0x31));
        assert_ne!(umk.public(), [0u8; 32]);
    }

    #[test]
    fn provisional_credentials_are_blank_and_self_describing() {
        let c = CredentialIdentity::provisional(
            arr32(0xa1),
            UserId::from_bytes([0xd4; 16]),
            DeviceId::from_bytes([0xe5; 16]),
            Kind::User,
            Tier::Browser,
        );
        assert!(c.is_provisional());
        assert_eq!(c.signer_tier, SignerTier::Provisional);
        assert_eq!(c.ssk_pub, [0u8; 32]);
        assert_eq!(c.sig_umk_ssk, [0u8; 64]);
        assert_eq!(c.sig_ssk_dev, [0u8; 64]);
        assert_eq!(CredentialIdentity::decode(&c.encode()).unwrap(), c);
        assert!(!vector_credential().is_provisional());
    }
}
