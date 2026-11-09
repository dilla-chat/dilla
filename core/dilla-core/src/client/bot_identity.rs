//! Installation-bound native bot bootstrap. Human signup remains its own fixed User/Browser path.

use super::error::{E_CORE_INPUT, E_CORE_STATE, E_CORE_STORAGE};
use super::identity::{
    self, AAD_STATE, IdentityRecord, Phase, load_phase, random, seal, state_list,
};
use super::{ClientCore, ClientError, schema, wire};
use crate::cbor::{Encoder, decode_strict};
use crate::identity::{
    CredentialIdentity, DeviceEntry, DeviceList, DeviceListUnsigned, Kind, SignerTier, SskSigner,
    Tier, UmkSigner,
};
use crate::ids::{DeviceId, UserId};
use crate::mls::CIPHERSUITE;
use openmls_basic_credential::SignatureKeyPair;
use zeroize::Zeroizing;

#[derive(Clone, Copy)]
pub(super) struct BotBinding {
    pub instance_id: [u8; 16],
    pub community_id: [u8; 16],
    pub installation_id: [u8; 16],
}

pub(super) struct PendingBot {
    pub binding: BotBinding,
    device_id: [u8; 16],
    dsk_pub: [u8; 32],
    umk_pub: [u8; 32],
    ssk_pub: [u8; 32],
    sig_umk_ssk: [u8; 64],
    sig_ssk_dev: [u8; 64],
    ssk_priv: Zeroizing<[u8; 32]>,
    k_backup: Zeroizing<[u8; 32]>,
}

impl core::fmt::Debug for PendingBot {
    fn fmt(&self, f: &mut core::fmt::Formatter<'_>) -> core::fmt::Result {
        f.write_str("PendingBot { binding, device_id, public_keys, signatures, secret_keys } ")
    }
}
impl PendingBot {
    fn encode(&self) -> Zeroizing<Vec<u8>> {
        let mut e = Encoder::new();
        e.array(12)
            .uint(1)
            .bytes(&self.binding.instance_id)
            .bytes(&self.binding.community_id)
            .bytes(&self.binding.installation_id)
            .bytes(&self.device_id)
            .bytes(&self.dsk_pub)
            .bytes(&self.umk_pub)
            .bytes(&self.ssk_pub)
            .bytes(&self.sig_umk_ssk)
            .bytes(&self.sig_ssk_dev)
            .bytes(&*self.ssk_priv)
            .bytes(&*self.k_backup);
        Zeroizing::new(e.into_vec())
    }

    fn decode(bytes: &[u8]) -> Result<Self, ClientError> {
        let record = decode_strict(bytes, |d| {
            d.array(12)?;
            if d.uint()? != 1 {
                return Err(shape());
            }
            Ok(Self {
                binding: BotBinding {
                    instance_id: d.bytes_exact()?,
                    community_id: d.bytes_exact()?,
                    installation_id: d.bytes_exact()?,
                },
                device_id: d.bytes_exact()?,
                dsk_pub: d.bytes_exact()?,
                umk_pub: d.bytes_exact()?,
                ssk_pub: d.bytes_exact()?,
                sig_umk_ssk: d.bytes_exact()?,
                sig_ssk_dev: d.bytes_exact()?,
                ssk_priv: Zeroizing::new(d.bytes_exact()?),
                k_backup: Zeroizing::new(d.bytes_exact()?),
            })
        })
        .map_err(|_| malformed(schema::BOT_PENDING))?;
        record.validate()?;
        Ok(record)
    }

    fn validate(&self) -> Result<(), ClientError> {
        let invalid = || ClientError::new(E_CORE_STATE, "pending bot identity does not verify");
        if !nonzero_ids(&self.binding) || self.device_id == [0; 16] {
            return Err(invalid());
        }
        if SskSigner::from_bytes(&self.ssk_priv).public() != self.ssk_pub {
            return Err(invalid());
        }
        self.placeholder_credential()
            .verify_signatures(&self.dsk_pub)
            .map_err(|_| invalid())
    }

    fn placeholder_credential(&self) -> CredentialIdentity {
        CredentialIdentity {
            v: 1,
            umk_pub: self.umk_pub,
            user_id: UserId::from_bytes([0; 16]),
            device_id: DeviceId::from_bytes(self.device_id),
            kind: Kind::Bot,
            tier: Tier::Native,
            signer_tier: SignerTier::Native,
            ssk_pub: self.ssk_pub,
            sig_umk_ssk: self.sig_umk_ssk,
            sig_ssk_dev: self.sig_ssk_dev,
        }
    }

    fn registration(&self) -> Vec<u8> {
        let mut device = Encoder::new();
        device
            .array(5)
            .bytes(&self.device_id)
            .bytes(&self.dsk_pub)
            .uint(u64::from(Tier::Native.as_u8()))
            .uint(u64::from(SignerTier::Native.as_u8()))
            .bytes(&self.placeholder_credential().encode());
        let mut e = Encoder::new();
        e.array(4)
            .bytes(&self.umk_pub)
            .bytes(&self.ssk_pub)
            .bytes(&self.sig_umk_ssk)
            .raw(&device.into_vec());
        e.into_vec()
    }
}

fn shape() -> crate::cbor::CborError {
    crate::cbor::CborError::TypeMismatch {
        expected: "bot record",
        offset: 0,
    }
}
fn malformed(key: &str) -> ClientError {
    ClientError::new(E_CORE_STORAGE, format!("app_meta {key} is malformed"))
}
fn binding_mismatch() -> ClientError {
    ClientError::new(E_CORE_STATE, "bot installation binding does not match")
}
pub(super) fn nonzero_ids(binding: &BotBinding) -> bool {
    binding.instance_id != [0; 16]
        && binding.community_id != [0; 16]
        && binding.installation_id != [0; 16]
}

pub(super) fn validate_open(
    provider: &crate::mls::DillaProvider,
    phase: &Phase,
) -> Result<Option<[u8; 32]>, ClientError> {
    let (pending, binding) = provider
        .storage()
        .read(|c| {
            Ok((
                schema::meta_get(c, schema::BOT_PENDING)?,
                schema::meta_get(c, schema::BOT_BINDING)?,
            ))
        })
        .map_err(ClientError::from)?;
    let conflict = || ClientError::new(E_CORE_STATE, "identity phase metadata conflicts");
    match phase {
        Phase::None => match (pending, binding) {
            (Some(bytes), None) => {
                let rec = PendingBot::decode(&bytes)?;
                if !nonzero_ids(&rec.binding) {
                    return Err(malformed(schema::BOT_PENDING));
                }
                Ok(Some(rec.dsk_pub))
            }
            (None, None) => Ok(None),
            _ => Err(conflict()),
        },
        Phase::Pending(_) | Phase::Enrolling(_) => {
            if pending.is_some() || binding.is_some() {
                Err(conflict())
            } else {
                Ok(None)
            }
        }
        Phase::Ready(rec) => {
            if pending.is_some() {
                return Err(conflict());
            }
            let credential = CredentialIdentity::decode(&rec.credential)
                .map_err(|_| malformed(schema::IDENTITY))?;
            match (credential.kind, binding) {
                (Kind::Bot, Some(bytes)) => {
                    let binding = decode_binding(&bytes)?;
                    if !nonzero_ids(&binding) || binding.instance_id != rec.instance_id {
                        return Err(malformed(schema::BOT_BINDING));
                    }
                    validate_ready(rec)?;
                    Ok(None)
                }
                (Kind::Bot, None) | (Kind::User, Some(_)) => Err(conflict()),
                (Kind::User, None) => Ok(None),
            }
        }
    }
}

pub(super) fn decode_binding(bytes: &[u8]) -> Result<BotBinding, ClientError> {
    decode_strict(bytes, |d| {
        d.array(4)?;
        if d.uint()? != 1 {
            return Err(shape());
        }
        Ok(BotBinding {
            instance_id: d.bytes_exact()?,
            community_id: d.bytes_exact()?,
            installation_id: d.bytes_exact()?,
        })
    })
    .map_err(|_| malformed(schema::BOT_BINDING))
}

fn encode_binding(binding: &BotBinding) -> Vec<u8> {
    let mut e = Encoder::new();
    e.array(4)
        .uint(1)
        .bytes(&binding.instance_id)
        .bytes(&binding.community_id)
        .bytes(&binding.installation_id);
    e.into_vec()
}

fn identity_registration(rec: &IdentityRecord) -> Result<Vec<u8>, ClientError> {
    let cred =
        CredentialIdentity::decode(&rec.credential).map_err(|_| malformed(schema::IDENTITY))?;
    validate_bot_credential(&cred, &rec.dsk_pub, &rec.ssk_pub)?;
    let mut placeholder = cred;
    placeholder.user_id = UserId::from_bytes([0; 16]);
    let mut device = Encoder::new();
    device
        .array(5)
        .bytes(&rec.device_id)
        .bytes(&rec.dsk_pub)
        .uint(u64::from(Tier::Native.as_u8()))
        .uint(u64::from(SignerTier::Native.as_u8()))
        .bytes(&placeholder.encode());
    let mut e = Encoder::new();
    e.array(4)
        .bytes(&rec.umk_pub)
        .bytes(&rec.ssk_pub)
        .bytes(&placeholder.sig_umk_ssk)
        .raw(&device.into_vec());
    Ok(e.into_vec())
}

fn validate_bot_credential(
    credential: &CredentialIdentity,
    dsk_pub: &[u8; 32],
    ssk_pub: &[u8; 32],
) -> Result<(), ClientError> {
    if credential.kind != Kind::Bot
        || credential.tier != Tier::Native
        || credential.signer_tier != SignerTier::Native
        || &credential.ssk_pub != ssk_pub
    {
        return Err(ClientError::new(
            E_CORE_STATE,
            "identity is not a native bot",
        ));
    }
    credential.verify_signatures(dsk_pub)?;
    Ok(())
}

pub(super) fn validate_ready(rec: &IdentityRecord) -> Result<DeviceList, ClientError> {
    let credential =
        CredentialIdentity::decode(&rec.credential).map_err(|_| malformed(schema::IDENTITY))?;
    validate_bot_credential(&credential, &rec.dsk_pub, &rec.ssk_pub)?;
    if credential.umk_pub != rec.umk_pub
        || credential.user_id.as_bytes() != &rec.user_id
        || credential.device_id.as_bytes() != &rec.device_id
    {
        return Err(malformed(schema::IDENTITY));
    }
    let body = wire::decode_device_list_body(&rec.device_list_body)
        .map_err(|_| malformed(schema::IDENTITY))?;
    let list = DeviceList::decode(&body.blob).map_err(|_| malformed(schema::IDENTITY))?;
    list.verify(&rec.ssk_pub)?;
    if body.version != list.unsigned.version
        || body.ssk_signature != list.sig_ssk
        || body.prev_hash != list.unsigned.prev_hash
        || rec.device_list != list.encode()
        || list.unsigned.user_id.as_bytes() != &rec.user_id
        || list.unsigned.entries.len() != 1
        || list.unsigned.entries[0].device_id.as_bytes() != &rec.device_id
        || list.unsigned.entries[0].dsk_pub != rec.dsk_pub
        || list.unsigned.entries[0].tier != Tier::Native
        || list.unsigned.entries[0].revoked_at.is_some()
    {
        return Err(malformed(schema::IDENTITY));
    }
    Ok(list)
}

impl ClientCore {
    /// Validate that this open client is the fully enrolled native Bot bound to the helper's
    /// installation tuple. The tuple is public scope metadata; no signing or recovery material is
    /// exposed by this check.
    pub fn bot_validate_binding(
        &self,
        instance_id: &[u8; 16],
        community_id: &[u8; 16],
        installation_id: &[u8; 16],
    ) -> Result<(), ClientError> {
        let own = self.own()?;
        if own.kind != Kind::Bot.as_u8() || own.tier != Tier::Native.as_u8() {
            return Err(binding_mismatch());
        }
        let raw = self.read(|c| schema::meta_get(c, schema::BOT_BINDING))?;
        let Some(raw) = raw else {
            return Err(binding_mismatch());
        };
        let binding = decode_binding(&raw)?;
        if !nonzero_ids(&binding)
            || binding.instance_id != *instance_id
            || binding.community_id != *community_id
            || binding.installation_id != *installation_id
            || own.instance_id != *instance_id
        {
            return Err(binding_mismatch());
        }
        Ok(())
    }

    /// Check the external-sender key pinned during the native bootstrap. The
    /// key itself is never returned to callers; this is a continuity check for
    /// the Go lifecycle after its authenticated `/v1/instance` discovery.
    pub fn bot_instance_key_matches(&self, observed_key: &[u8; 32]) -> Result<(), ClientError> {
        if observed_key.iter().all(|byte| *byte == 0) {
            return Err(ClientError::new(E_CORE_INPUT, "instance key is all zero"));
        }
        let pinned = self.read(|c| schema::meta_get(c, schema::BOT_INSTANCE_KEY))?;
        if pinned.as_deref() != Some(observed_key.as_slice()) {
            return Err(ClientError::new(
                E_CORE_STATE,
                "instance external-sender key changed or is not pinned",
            ));
        }
        Ok(())
    }

    /// Start (or resume) the one native bot identity bound to an installation and community.
    /// The returned CBOR is the public `[umk_pub, ssk_pub, sig_umk_ssk, device]` registration tuple.
    pub fn bot_bootstrap_begin(
        &mut self,
        instance_id: &[u8; 16],
        community_id: &[u8; 16],
        installation_id: &[u8; 16],
        external_sender_key: &[u8; 32],
    ) -> Result<Vec<u8>, ClientError> {
        let binding = BotBinding {
            instance_id: *instance_id,
            community_id: *community_id,
            installation_id: *installation_id,
        };
        if !nonzero_ids(&binding) {
            return Err(ClientError::new(
                E_CORE_INPUT,
                "bot binding ids must be nonzero",
            ));
        }
        if external_sender_key.iter().all(|byte| *byte == 0) {
            return Err(ClientError::new(E_CORE_INPUT, "instance key is all zero"));
        }

        let (body, new_signer) = self.write(|ctx, u| {
            let (identity_raw, signup, enrol) = u.with_conn(load_phase)?;
            let phase = identity::decode_phase(identity_raw, signup, enrol)?;
            let pin = u.with_conn(|c| schema::meta_get(c, schema::BOT_INSTANCE_KEY))?;
            match pin {
                Some(pinned) if pinned.as_slice() != external_sender_key => {
                    return Err(ClientError::new(
                        E_CORE_STATE,
                        "instance external-sender key changed",
                    ));
                }
                Some(_) => {}
                None if matches!(&phase, Phase::Ready(_)) => {
                    return Err(ClientError::new(
                        E_CORE_STATE,
                        "existing Bot has no pinned instance key; operator recovery required",
                    ));
                }
                None => {
                    u.with_conn(|c| {
                        schema::meta_put(c, schema::BOT_INSTANCE_KEY, external_sender_key)
                    })?;
                }
            }
            match phase {
                Phase::None => {
                    if let Some(raw) = u.with_conn(|c| schema::meta_get(c, schema::BOT_PENDING))? {
                        if u.with_conn(|c| schema::meta_get(c, schema::BOT_BINDING))?
                            .is_some()
                        {
                            return Err(ClientError::new(
                                E_CORE_STATE,
                                "bot identity metadata conflicts",
                            ));
                        }
                        let pending = PendingBot::decode(&raw)?;
                        if pending.binding.instance_id != binding.instance_id
                            || pending.binding.community_id != binding.community_id
                            || pending.binding.installation_id != binding.installation_id
                        {
                            return Err(binding_mismatch());
                        }
                        return Ok((pending.registration(), None));
                    }
                    if u.with_conn(|c| schema::meta_get(c, schema::BOT_BINDING))?
                        .is_some()
                    {
                        return Err(ClientError::new(
                            E_CORE_STATE,
                            "a bot identity already exists",
                        ));
                    }
                    let umk_seed = random::<32>(ctx.provider)?;
                    let ssk_seed = random::<32>(ctx.provider)?;
                    let device_id = random::<16>(ctx.provider)?;
                    let signer =
                        SignatureKeyPair::new(CIPHERSUITE.signature_algorithm()).map_err(|e| {
                            ClientError::new(super::error::E_CORE_MLS, format!("keygen: {e:?}"))
                        })?;
                    signer.store(ctx.provider.storage())?;
                    let dsk_pub: [u8; 32] = signer
                        .public()
                        .try_into()
                        .map_err(|_| ClientError::new(E_CORE_STATE, "dsk is not 32 bytes"))?;
                    let umk = UmkSigner::from_bytes(&umk_seed);
                    let ssk = SskSigner::from_bytes(&ssk_seed);
                    let pending = PendingBot {
                        binding,
                        device_id: *device_id,
                        dsk_pub,
                        umk_pub: umk.public(),
                        ssk_pub: ssk.public(),
                        sig_umk_ssk: umk.sign_ssk(&ssk.public()),
                        sig_ssk_dev: ssk.sign_device(
                            &DeviceId::from_bytes(*device_id),
                            &dsk_pub,
                            Kind::Bot,
                            Tier::Native,
                            SignerTier::Native,
                        ),
                        ssk_priv: ssk_seed,
                        k_backup: random::<32>(ctx.provider)?,
                    };
                    pending.validate()?;
                    let body = pending.registration();
                    let encoded = pending.encode();
                    u.with_conn(|c| schema::meta_put(c, schema::BOT_PENDING, &encoded))?;
                    Ok((body, Some(signer)))
                }
                Phase::Ready(rec) => {
                    let credential = CredentialIdentity::decode(&rec.credential)
                        .map_err(|_| malformed(schema::IDENTITY))?;
                    if credential.kind != Kind::Bot {
                        return Err(ClientError::new(
                            E_CORE_STATE,
                            "a human identity already exists",
                        ));
                    }
                    let binding_raw = u.with_conn(|c| schema::meta_get(c, schema::BOT_BINDING))?;
                    let current = binding_raw
                        .as_deref()
                        .map(decode_binding)
                        .transpose()?
                        .ok_or_else(|| {
                            ClientError::new(E_CORE_STATE, "a human identity already exists")
                        })?;
                    if current.instance_id != binding.instance_id
                        || current.community_id != binding.community_id
                        || current.installation_id != binding.installation_id
                        || rec.instance_id != current.instance_id
                    {
                        return Err(binding_mismatch());
                    }
                    validate_ready(&rec)?;
                    Ok((identity_registration(&rec)?, None))
                }
                Phase::Pending(_) => Err(ClientError::new(E_CORE_STATE, "human signup is pending")),
                Phase::Enrolling(_) => {
                    Err(ClientError::new(E_CORE_STATE, "human enrolment is pending"))
                }
            }
        })?;
        if let Some(signer) = new_signer {
            self.signer = Some(signer);
        }
        Ok(body)
    }

    /// Complete a bot account enrollment and return its credential and signed list body.
    pub fn bot_bootstrap_complete(
        &mut self,
        user_id: &[u8; 16],
        username: &str,
        now: u64,
    ) -> Result<Vec<u8>, ClientError> {
        if *user_id == [0; 16] {
            return Err(ClientError::new(E_CORE_INPUT, "user_id is all zero"));
        }
        self.write(|ctx, u| {
            let (identity_raw, signup, enrol) = u.with_conn(load_phase)?;
            match identity::decode_phase(identity_raw, signup, enrol)? {
                Phase::Ready(rec) => {
                    if rec.user_id != *user_id {
                        return Err(ClientError::new(
                            E_CORE_STATE,
                            "another account is already bound",
                        ));
                    }
                    let binding_raw = u.with_conn(|c| schema::meta_get(c, schema::BOT_BINDING))?;
                    let binding = binding_raw
                        .as_deref()
                        .map(decode_binding)
                        .transpose()?
                        .ok_or_else(|| {
                            ClientError::new(E_CORE_STATE, "a human identity already exists")
                        })?;
                    if binding.instance_id != rec.instance_id {
                        return Err(malformed(schema::BOT_BINDING));
                    }
                    if !nonzero_ids(&binding) {
                        return Err(malformed(schema::BOT_BINDING));
                    }
                    validate_ready(&rec)?;
                    let mut e = Encoder::new();
                    e.array(2)
                        .bytes(&rec.credential)
                        .bytes(&rec.device_list_body);
                    return Ok(e.into_vec());
                }
                Phase::None => {}
                Phase::Pending(_) => {
                    return Err(ClientError::new(E_CORE_STATE, "human signup is pending"));
                }
                Phase::Enrolling(_) => {
                    return Err(ClientError::new(E_CORE_STATE, "human enrolment is pending"));
                }
            }
            let raw = u
                .with_conn(|c| schema::meta_get(c, schema::BOT_PENDING))?
                .ok_or_else(|| ClientError::new(E_CORE_STATE, "no bot bootstrap is pending"))?;
            if u.with_conn(|c| schema::meta_get(c, schema::BOT_BINDING))?
                .is_some()
            {
                return Err(ClientError::new(
                    E_CORE_STATE,
                    "bot identity metadata conflicts",
                ));
            }
            let pending = PendingBot::decode(&raw)?;
            if !nonzero_ids(&pending.binding) {
                return Err(malformed(schema::BOT_PENDING));
            }
            let signer = SskSigner::from_bytes(&pending.ssk_priv);
            let credential = CredentialIdentity {
                v: 1,
                umk_pub: pending.umk_pub,
                user_id: UserId::from_bytes(*user_id),
                device_id: DeviceId::from_bytes(pending.device_id),
                kind: Kind::Bot,
                tier: Tier::Native,
                signer_tier: SignerTier::Native,
                ssk_pub: pending.ssk_pub,
                sig_umk_ssk: pending.sig_umk_ssk,
                sig_ssk_dev: pending.sig_ssk_dev,
            };
            validate_bot_credential(&credential, &pending.dsk_pub, &pending.ssk_pub)?;
            let unsigned = DeviceListUnsigned {
                v: 1,
                user_id: UserId::from_bytes(*user_id),
                version: 1,
                prev_hash: [0; 32],
                entries: vec![DeviceEntry {
                    device_id: DeviceId::from_bytes(pending.device_id),
                    dsk_pub: pending.dsk_pub,
                    tier: Tier::Native,
                    added_at: now,
                    revoked_at: None,
                }],
            };
            let list = DeviceList {
                sig_ssk: signer.sign_device_list(&unsigned),
                unsigned,
            };
            list.verify(&pending.ssk_pub)?;
            let device_list = list.encode();
            let device_list_body = wire::device_list_put_body(&list);
            let mut state_plain = Encoder::new();
            state_plain.array(3).uint(1).bytes(&device_list).array(0);
            let state_plain = Zeroizing::new(state_plain.into_vec());
            let state = seal(ctx.provider, &pending.k_backup, AAD_STATE, &state_plain)?;
            let rec = IdentityRecord {
                instance_id: pending.binding.instance_id,
                user_id: *user_id,
                device_id: pending.device_id,
                dsk_pub: pending.dsk_pub,
                umk_pub: pending.umk_pub,
                ssk_pub: pending.ssk_pub,
                username: username.to_owned(),
                credential: credential.encode(),
                device_list_body: device_list_body.clone(),
                list_published: false,
                device_list,
                state_uploaded: false,
            };
            validate_ready(&rec)?;
            let binding = encode_binding(&pending.binding);
            let identity = rec.encode();
            u.with_conn(|c| {
                schema::meta_put(c, schema::STATE_SEALED, &state)?;
                schema::meta_put(c, schema::STATE_LIST, &state_list(1))?;
                schema::meta_put(c, schema::IDENTITY, &identity)?;
                schema::meta_put(c, schema::BOT_BINDING, &binding)?;
                super::trust::pin_own(
                    c,
                    user_id,
                    &pending.umk_pub,
                    now.min(i64::MAX as u64) as i64,
                )?;
                schema::meta_del(c, schema::BOT_PENDING)
            })?;
            let mut e = Encoder::new();
            e.array(2).bytes(&rec.credential).bytes(&device_list_body);
            Ok(e.into_vec())
        })
    }
}

// These storage tests use the native Arc<Mutex<Connection>> handle. The wasm
// backend uses Rc<RefCell<Connection>>, and wasm test binaries are compile-only
// in CI, so keep these native integration-style unit tests off the wasm target.
