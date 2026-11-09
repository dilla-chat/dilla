//! `dilla_binding`: the GroupContext extension that nails a group to one instance, one target and
//! one protocol version (protocol/01-groups.md "dilla_binding"). It is immutable for the group's
//! life, and every Welcome, GroupInfo, Proposal and Commit is checked against it.

use crate::cbor::{Encoder, decode_strict};
use crate::error::ProtocolError;
use crate::ids::{CommunityId, InstanceId};
use openmls::extensions::{ExternalSender, ExternalSendersExtension, SenderExtensionIndex};
use openmls::prelude::*;

/// Private-use extension type. 0xF000-0xFFFF is "Reserved for Private Use" in the live IANA
/// registry for RFC 9420, and 0xF001 is not a GREASE value (gap-4 section 1.1).
pub const DILLA_BINDING_ID: u16 = 0xF001;
pub const DILLA_BINDING: ExtensionType = ExtensionType::Unknown(DILLA_BINDING_ID);

#[derive(Clone, Copy, PartialEq, Eq, Debug)]
#[repr(u8)]
pub enum GroupKind {
    Text = 0,
    Call = 1,
    Pairing = 2,
}

impl GroupKind {
    pub const fn as_u8(self) -> u8 {
        self as u8
    }

    pub fn from_u64(v: u64) -> Result<Self, ProtocolError> {
        Ok(match v {
            0 => Self::Text,
            1 => Self::Call,
            2 => Self::Pairing,
            // 3 was the Bot interaction group, gone since Bot v2 Task 23.
            _ => return Err(ProtocolError::Binding),
        })
    }

    /// Only text and call groups carry the instance as an external sender; pairing groups must not
    /// (`E_EXTERNAL_SENDER_FORBIDDEN`).
    pub const fn has_external_sender(self) -> bool {
        matches!(self, Self::Text | Self::Call)
    }

    pub const fn media_version(self) -> u64 {
        match self {
            Self::Call => 1,
            _ => 0,
        }
    }
}

#[derive(Clone, PartialEq, Eq, Debug)]
pub struct DillaBinding {
    pub v: u64,
    pub instance_id: InstanceId,
    /// `None` for DMs, group DMs and pairing groups.
    pub community_id: Option<CommunityId>,
    /// channel_id, dm_id or the new device_id (pairing).
    pub target_id: [u8; 16],
    pub kind: GroupKind,
    /// The instance's policy version at group creation. A snapshot, not an identity field.
    pub policy_version: u64,
    pub e2ee_version: u64,
    pub media_version: u64,
}

impl DillaBinding {
    pub fn encode(&self) -> Vec<u8> {
        let mut e = Encoder::with_capacity(96);
        e.array(8)
            .uint(self.v)
            .bytes(self.instance_id.as_bytes())
            .opt_bytes(self.community_id.as_ref().map(|c| &c.0[..]))
            .bytes(&self.target_id)
            .uint(u64::from(self.kind.as_u8()))
            .uint(self.policy_version)
            .uint(self.e2ee_version)
            .uint(self.media_version);
        e.into_vec()
    }

    /// Rejects trailing bytes itself: OpenMLS does not re-parse or length-check unknown-extension
    /// payloads beyond the outer `opaque<V>` (gap-4 section 3), so without this an attacker can
    /// append padding to a valid binding and still round-trip it.
    pub fn decode(bytes: &[u8]) -> Result<Self, ProtocolError> {
        decode_strict(bytes, |d| {
            d.array(8)?;
            let v = d.uint()?;
            let instance_id = InstanceId::from_bytes(d.bytes_exact::<16>()?);
            let community_id = d.opt_bytes_exact::<16>()?.map(CommunityId::from_bytes);
            let target_id = d.bytes_exact::<16>()?;
            let kind = d.uint()?;
            Ok((
                v,
                instance_id,
                community_id,
                target_id,
                kind,
                d.uint()?,
                d.uint()?,
                d.uint()?,
            ))
        })
        .map_err(|_| ProtocolError::Binding)
        .and_then(
            |(v, instance_id, community_id, target_id, kind, policy_version, e2ee, media)| {
                if v != 1 {
                    return Err(ProtocolError::Binding);
                }
                Ok(Self {
                    v,
                    instance_id,
                    community_id,
                    target_id,
                    kind: GroupKind::from_u64(kind)?,
                    policy_version,
                    e2ee_version: e2ee,
                    media_version: media,
                })
            },
        )
    }

    pub fn to_extension(&self) -> Extension {
        Extension::Unknown(DILLA_BINDING_ID, UnknownExtension(self.encode()))
    }

    pub fn from_group_context(ctx: &GroupContext) -> Result<Self, ProtocolError> {
        Self::from_extensions(ctx.extensions())
    }

    pub fn from_extensions<T>(exts: &Extensions<T>) -> Result<Self, ProtocolError> {
        let raw = exts
            .unknown(DILLA_BINDING_ID)
            .ok_or(ProtocolError::Binding)?;
        Self::decode(&raw.0)
    }

    /// Everything immutable must match. `policy_version` is deliberately excluded: it is the
    /// instance's policy snapshot at creation and moves on without the group changing identity.
    pub fn matches(&self, expected: &DillaBinding) -> Result<(), ProtocolError> {
        let same = self.instance_id == expected.instance_id
            && self.community_id == expected.community_id
            && self.target_id == expected.target_id
            && self.kind == expected.kind
            && self.e2ee_version == expected.e2ee_version
            && self.media_version == expected.media_version;
        if same {
            Ok(())
        } else {
            Err(ProtocolError::Binding)
        }
    }
}

/// The instance external sender's basic-credential identity: CBOR `[1, "instance", instance_id]`.
pub fn instance_credential_identity(instance_id: &InstanceId) -> Vec<u8> {
    let mut e = Encoder::with_capacity(32);
    e.array(3)
        .uint(1)
        .text("instance")
        .bytes(instance_id.as_bytes());
    e.into_vec()
}

pub fn instance_credential(instance_id: &InstanceId) -> Credential {
    BasicCredential::new(instance_credential_identity(instance_id)).into()
}

pub fn external_senders(
    instance_key: SignaturePublicKey,
    instance_id: &InstanceId,
) -> ExternalSendersExtension {
    vec![ExternalSender::new(
        instance_key,
        instance_credential(instance_id),
    )]
}

/// The instance is always at position 0 and is never reordered (gap-5 section 2.3).
pub fn instance_sender_index() -> SenderExtensionIndex {
    SenderExtensionIndex::new(0)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::ids::{ChannelId, CommunityId, InstanceId};
    use tls_codec::Serialize as _;

    fn binding() -> DillaBinding {
        DillaBinding {
            v: 1,
            instance_id: InstanceId::from_bytes([0x11; 16]),
            community_id: Some(CommunityId::from_bytes([0x22; 16])),
            target_id: *ChannelId::from_bytes([0x33; 16]).as_bytes(),
            kind: GroupKind::Text,
            policy_version: 7,
            e2ee_version: 1,
            media_version: 0,
        }
    }

    #[test]
    fn binding_encodes_an_eight_element_array_and_round_trips() {
        let b = binding();
        let bytes = b.encode();
        assert_eq!(bytes[0], 0x88, "an 8-element array head");
        assert_eq!(DillaBinding::decode(&bytes).unwrap(), b);

        let mut dm = b.clone();
        dm.community_id = None;
        assert_eq!(DillaBinding::decode(&dm.encode()).unwrap(), dm);
    }

    #[test]
    fn binding_decode_rejects_trailing_bytes() {
        // OpenMLS does not validate unknown-extension payloads (gap-4 section 3), so an attacker
        // can append padding unless dilla rejects it here.
        let mut bytes = binding().encode();
        bytes.push(0x00);
        assert_eq!(DillaBinding::decode(&bytes), Err(ProtocolError::Binding));
        assert_eq!(DillaBinding::decode(&[]), Err(ProtocolError::Binding));
    }

    #[test]
    fn matches_compares_every_immutable_field() {
        let b = binding();
        assert_eq!(b.matches(&b), Ok(()));
        for mutate in [
            |x: &mut DillaBinding| x.instance_id = InstanceId::from_bytes([0x99; 16]),
            |x: &mut DillaBinding| x.community_id = None,
            |x: &mut DillaBinding| x.target_id = [0x99; 16],
            |x: &mut DillaBinding| x.kind = GroupKind::Call,
            |x: &mut DillaBinding| x.e2ee_version = 2,
            |x: &mut DillaBinding| x.media_version = 1,
        ] {
            let mut other = b.clone();
            mutate(&mut other);
            assert_eq!(b.matches(&other), Err(ProtocolError::Binding));
        }
        // policy_version is a snapshot, not an identity field
        let mut later = b.clone();
        later.policy_version = 99;
        assert_eq!(b.matches(&later), Ok(()));
    }

    /// `Extension`'s TLS codec is hand-written and emits `F0 01 || varint len || payload`.
    /// `UnknownExtension`'s own derived codec adds a second length prefix; never use it for wire
    /// bytes (gap-4 section 3).
    #[test]
    fn the_extension_serialises_without_a_second_length_prefix() {
        let ext = Extension::Unknown(DILLA_BINDING_ID, UnknownExtension(vec![1, 2, 3]));
        assert_eq!(
            ext.tls_serialize_detached().unwrap(),
            vec![0xf0, 0x01, 0x03, 0x01, 0x02, 0x03]
        );
    }

    #[test]
    fn group_kinds_carry_their_external_sender_and_media_rules() {
        assert!(GroupKind::Text.has_external_sender());
        assert!(GroupKind::Call.has_external_sender());
        assert!(!GroupKind::Pairing.has_external_sender());
        assert_eq!(GroupKind::Call.media_version(), 1);
        for k in [GroupKind::Text, GroupKind::Pairing] {
            assert_eq!(k.media_version(), 0);
        }
        // Kind 3 (the Bot interaction group) is gone since Bot v2 Task 23.
        assert_eq!(GroupKind::from_u64(3), Err(ProtocolError::Binding));
        assert_eq!(GroupKind::from_u64(4), Err(ProtocolError::Binding));
    }

    #[test]
    fn the_instance_credential_identity_is_the_three_element_array() {
        let id = InstanceId::from_bytes([0x11; 16]);
        let bytes = instance_credential_identity(&id);
        let mut want = crate::cbor::Encoder::new();
        want.array(3).uint(1).text("instance").bytes(id.as_bytes());
        assert_eq!(bytes, want.into_vec());
    }
}
