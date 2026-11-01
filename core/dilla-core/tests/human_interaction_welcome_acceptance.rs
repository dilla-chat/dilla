#![cfg(not(target_arch = "wasm32"))]

//! End-to-end acceptance of a real kind-3 Welcome by an enrolled human core. The Bot creates and
//! signs the MLS group, its current-context proof is server-key signed, and both leaves have
//! locally pinned UMKs and signed device lists before the client applies the Welcome.

#[path = "client_support/mod.rs"]
mod client_support;

use client_support::{Core, GROUP, INSTANCE, NOW};
use dilla_core::{
    bot::Response,
    cbor::{Encoder, decode_strict},
    client::ClientCore,
    envelope::{Envelope, EnvelopeType},
    identity::{
        CredentialIdentity, DeviceEntry, DeviceList, DeviceListUnsigned, Kind, SignerTier,
        SskSigner, Tier, UmkSigner,
    },
    ids::{CommunityId, DeviceId, InstanceId, MsgId, UserId},
    mls::{CIPHERSUITE, ConnHandle, DILLA_BINDING_ID, DillaInteractionBinding},
};
use ed25519_dalek::{Signer, SigningKey};
use openmls::{extensions::UnknownExtension, prelude::*};
use openmls_basic_credential::SignatureKeyPair;
use openmls_rust_crypto::OpenMlsRustCrypto;
use sha2::{Digest, Sha256};
use std::{
    path::{Path, PathBuf},
    sync::{Arc, Mutex},
    time::{SystemTime, UNIX_EPOCH},
};
use tls_codec::{Deserialize as _, Serialize as _};

const BOT_USER: [u8; 16] = [0x91; 16];
const BOT_DEVICE: [u8; 16] = [0x92; 16];
const INSTALLATION: [u8; 16] = [0x93; 16];
const OPERATION: [u8; 16] = [0x94; 16];
const GROUP_ID: [u8; 16] = GROUP;
const COMMUNITY: [u8; 16] = [0x22; 16];
const POLICY: u64 = 3;
const SERVER_CONTEXT_DOMAIN: &[u8] = b"dilla bot current interaction context v1\0";

struct BotIdentity {
    credential: CredentialWithKey,
    signer: SignatureKeyPair,
    identity: CredentialIdentity,
    list: DeviceList,
}

fn bot_identity() -> BotIdentity {
    let umk = UmkSigner::from_bytes(&[0xb1; 32]);
    let ssk = SskSigner::from_bytes(&[0xb2; 32]);
    let dsk_seed = [0xb3; 32];
    let dsk = SskSigner::from_bytes(&dsk_seed).public();
    let signer =
        SignatureKeyPair::from_raw(SignatureScheme::ED25519, dsk_seed.to_vec(), dsk.to_vec());
    let identity = CredentialIdentity {
        v: 1,
        umk_pub: umk.public(),
        user_id: UserId::from_bytes(BOT_USER),
        device_id: DeviceId::from_bytes(BOT_DEVICE),
        kind: Kind::Bot,
        tier: Tier::Native,
        signer_tier: SignerTier::Native,
        ssk_pub: ssk.public(),
        sig_umk_ssk: umk.sign_ssk(&ssk.public()),
        sig_ssk_dev: ssk.sign_device(
            &DeviceId::from_bytes(BOT_DEVICE),
            &dsk,
            Kind::Bot,
            Tier::Native,
            SignerTier::Native,
        ),
    };
    let unsigned = DeviceListUnsigned {
        v: 1,
        user_id: UserId::from_bytes(BOT_USER),
        version: 1,
        prev_hash: [0; 32],
        entries: vec![DeviceEntry {
            device_id: DeviceId::from_bytes(BOT_DEVICE),
            dsk_pub: dsk,
            tier: Tier::Native,
            added_at: NOW,
            revoked_at: None,
        }],
    };
    let list = DeviceList {
        sig_ssk: ssk.sign_device_list(&unsigned),
        unsigned,
    };
    let credential = CredentialWithKey {
        credential: BasicCredential::new(identity.encode()).into(),
        signature_key: signer.public().into(),
    };
    BotIdentity {
        credential,
        signer,
        identity,
        list,
    }
}

fn enrolled_human_with_connection(instance_signer: &[u8; 32], connection: ConnHandle) -> Core {
    let probe = Arc::clone(&connection);
    let mut core = ClientCore::open(connection).expect("open human core");
    core.signup_begin(&INSTANCE)
        .expect("begin human enrollment");
    core.signup_complete(&[0xa1; 16], "human participant", NOW, instance_signer)
        .expect("enroll and pin instance signer");
    let device = decode_strict(&core.identity().expect("human identity"), |d| {
        d.array(6)?;
        d.uint()?;
        d.opt_bytes_exact::<16>()?;
        d.opt_bytes_exact::<16>()?;
        let device = d.opt_bytes_exact::<16>()?;
        d.text()?;
        d.uint()?;
        Ok(device)
    })
    .expect("human identity shape")
    .expect("enrolled human device");
    Core {
        core,
        probe,
        user: [0xa1; 16],
        device,
    }
}

struct EncryptedTestStore(PathBuf);

impl EncryptedTestStore {
    fn new(label: &str) -> Self {
        let unique = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .expect("clock after epoch")
            .as_nanos();
        Self(std::env::temp_dir().join(format!(
            "dilla-receipt-{label}-{}-{unique}.db",
            std::process::id()
        )))
    }

    fn path(&self) -> &Path {
        &self.0
    }
}

impl Drop for EncryptedTestStore {
    fn drop(&mut self) {
        for suffix in ["", "-journal", "-wal", "-shm"] {
            let mut name = self.0.as_os_str().to_os_string();
            name.push(suffix);
            let _ = std::fs::remove_file(name);
        }
    }
}

fn open_encrypted_test_connection(path: &Path, key_hex: &str) -> ConnHandle {
    let connection = rusqlite::Connection::open(path).expect("open encrypted SQLCipher file");
    connection
        .execute_batch(&format!(
            "PRAGMA cipher = 'chacha20'; PRAGMA key = 'raw:{key_hex}';"
        ))
        .expect("configure SQLCipher chacha20 store key");
    connection
        .query_row("SELECT count(*) FROM sqlite_schema", [], |row| {
            row.get::<_, i64>(0)
        })
        .expect("verify the encrypted store key by reading sqlite schema");
    Arc::new(Mutex::new(connection))
}

fn reopen_encrypted_test_store(core: Core, path: &Path, key_hex: &str) -> Core {
    let Core {
        core,
        probe,
        user,
        device,
    } = core;
    drop(core);
    let connection = Arc::try_unwrap(probe)
        .unwrap_or_else(|_| panic!("full encrypted-store close requires sole connection ownership"))
        .into_inner()
        .expect("unpoison database connection mutex");
    connection
        .close()
        .unwrap_or_else(|(_, error)| panic!("close encrypted test database: {error}"));
    let connection = open_encrypted_test_connection(path, key_hex);
    let probe = Arc::clone(&connection);
    let core = ClientCore::open(connection).expect("reopen native core from encrypted database");
    Core {
        core,
        probe,
        user,
        device,
    }
}

fn snapshot(identity: &CredentialIdentity, list: &DeviceList) -> Vec<u8> {
    let blob = list.encode();
    let hash: [u8; 32] = Sha256::digest(&blob).into();
    let mut e = Encoder::new();
    e.array(9)
        .bytes(identity.user_id.as_bytes())
        .bytes(&identity.umk_pub)
        .bytes(&identity.ssk_pub)
        .bytes(&identity.sig_umk_ssk)
        .uint(list.unsigned.version)
        .bytes(&blob)
        .bytes(&list.sig_ssk)
        .bytes(&list.unsigned.prev_hash)
        .bytes(&hash);
    e.into_vec()
}

fn credential_from_key_package(
    bytes: &[u8],
    provider: &OpenMlsRustCrypto,
) -> (KeyPackage, CredentialIdentity, [u8; 32]) {
    let message = MlsMessageIn::tls_deserialize_exact(bytes).expect("human KeyPackage message");
    let MlsMessageBodyIn::KeyPackage(package) = message.extract() else {
        panic!("human package is not a KeyPackage");
    };
    let unverified = package.unverified_credential();
    let basic =
        BasicCredential::try_from(unverified.credential.clone()).expect("human BasicCredential");
    let identity = CredentialIdentity::decode(basic.identity()).expect("human identity");
    let dsk = unverified
        .signature_key
        .as_slice()
        .try_into()
        .expect("human DSK length");
    let package = package
        .validate(provider.crypto(), ProtocolVersion::Mls10)
        .expect("valid human KeyPackage");
    (package, identity, dsk)
}

fn welcomes_body(welcome: &[u8], tree: &[u8], tree_hash: &[u8; 32]) -> Vec<u8> {
    let mut e = Encoder::new();
    e.array(1)
        .array(7)
        .uint(1)
        .bytes(&GROUP_ID)
        .uint(1)
        .uint(1)
        .bytes(welcome)
        .bytes(tree)
        .bytes(tree_hash);
    e.into_vec()
}

fn mutate_welcome_body(body: &[u8]) -> Vec<u8> {
    let (welcome_id, group, epoch, commit_seq, mut blob, tree, hash) = decode_strict(body, |d| {
        d.array(1)?;
        d.array(7)?;
        Ok((
            d.uint()?,
            d.bytes_exact::<16>()?,
            d.uint()?,
            d.uint()?,
            d.bytes()?.to_vec(),
            d.bytes()?.to_vec(),
            d.bytes_exact::<32>()?,
        ))
    })
    .expect("Welcome body");
    blob[0] ^= 1;
    let mut e = Encoder::new();
    e.array(1)
        .array(7)
        .uint(welcome_id)
        .bytes(&group)
        .uint(epoch)
        .uint(commit_seq)
        .bytes(&blob)
        .bytes(&tree)
        .bytes(&hash);
    e.into_vec()
}

#[expect(
    clippy::too_many_arguments,
    reason = "The helper builds proof bytes from independently scoped test fixtures."
)]
fn current_context_proof(
    server_key: &SigningKey,
    human: &Core,
    bot: &BotIdentity,
    human_identity: &CredentialIdentity,
    human_list: &DeviceList,
    human_dsk: &[u8; 32],
    tree_hash: &[u8; 32],
    issued_at: u64,
    accepted_at: u64,
    registration_group: [u8; 16],
) -> Vec<u8> {
    let bot_snapshot = snapshot(&bot.identity, &bot.list);
    let human_snapshot = snapshot(human_identity, human_list);
    let key = server_key.verifying_key().to_bytes();
    let not_after = issued_at + 30;
    let binding = DillaInteractionBinding {
        instance_id: InstanceId::from_bytes(INSTANCE),
        installation_id: INSTALLATION,
        community_id: CommunityId::from_bytes(COMMUNITY),
        bot_user_id: BOT_USER,
        participant_user_id: human.user,
        participant_device_id: DeviceId::from_bytes(human.device),
        policy_version: POLICY,
        e2ee_version: 1,
        media_version: 0,
    };
    let mut payload = Encoder::new();
    payload
        .array(18)
        .uint(1)
        .bytes(&INSTANCE)
        .bytes(&[0x95; 16])
        .bytes(&key)
        .uint(issued_at)
        .uint(not_after)
        .array(11)
        .bytes(&INSTALLATION)
        .bytes(&OPERATION)
        .bytes(&BOT_USER)
        .bytes(&BOT_DEVICE)
        .bytes(&COMMUNITY)
        .bytes(&registration_group)
        .bytes(&[0x96; 32])
        .uint(1)
        .uint(1)
        .uint(2)
        .uint(accepted_at)
        .array(9)
        .bytes(binding.instance_id.as_bytes())
        .bytes(&binding.installation_id)
        .bytes(binding.community_id.as_bytes())
        .bytes(&binding.bot_user_id)
        .bytes(&binding.participant_user_id)
        .bytes(binding.participant_device_id.as_bytes())
        .uint(binding.policy_version)
        .uint(binding.e2ee_version)
        .uint(binding.media_version)
        .uint(1)
        .uint(2)
        .bytes(tree_hash)
        .array(2)
        .array(4)
        .uint(0)
        .bytes(&BOT_USER)
        .bytes(&BOT_DEVICE)
        .bytes(bot.signer.public())
        .array(4)
        .uint(1)
        .bytes(&human.user)
        .bytes(&human.device)
        .bytes(human_dsk)
        .uint(1)
        .uint(POLICY)
        .uint(1)
        .uint(0)
        .raw(&bot_snapshot)
        .raw(&human_snapshot);
    let payload = payload.into_vec();
    let mut preimage = SERVER_CONTEXT_DOMAIN.to_vec();
    preimage.extend_from_slice(&payload);
    let signature = server_key.sign(&preimage).to_bytes();
    let mut wire = Encoder::new();
    wire.array(2).bytes(&payload).bytes(&signature);
    wire.into_vec()
}

fn seed_trust(
    human: &mut Core,
    bot: &BotIdentity,
    human_identity: &CredentialIdentity,
    human_list: &DeviceList,
) {
    let connection = human.probe.lock().expect("human DB");
    connection
        .execute(
            "INSERT INTO app_device_lists(user_id,version,list,umk_pub,ssk_pub,credential) VALUES(?1,1,?2,?3,?4,NULL)",
            rusqlite::params![
                BOT_USER.as_slice(),
                bot.list.encode(),
                bot.identity.umk_pub.as_slice(),
                bot.identity.ssk_pub.as_slice()
            ],
        )
        .expect("cache Bot signed device list");
    connection
        .execute(
            "INSERT INTO app_trust_pins(user_id,umk_pub,first_seen,verified,changed_umk) VALUES(?1,?2,?3,1,NULL)",
            rusqlite::params![BOT_USER.as_slice(), bot.identity.umk_pub.as_slice(), NOW as i64],
        )
        .expect("pin Bot UMK");
    connection
        .execute(
            "INSERT INTO app_device_lists(user_id,version,list,umk_pub,ssk_pub,credential) VALUES(?1,1,?2,?3,?4,NULL) ON CONFLICT(user_id) DO UPDATE SET list=excluded.list,umk_pub=excluded.umk_pub,ssk_pub=excluded.ssk_pub",
            rusqlite::params![
                human.user.as_slice(),
                human_list.encode(),
                human_identity.umk_pub.as_slice(),
                human_identity.ssk_pub.as_slice()
            ],
        )
        .expect("cache human signed list");
    drop(connection);
}

#[test]
fn human_accepts_server_proven_two_leaf_bot_welcome_and_rejects_group_mismatch() {
    authenticated_response_roundtrip(60);
}

#[test]
fn capability_response_survives_authenticated_journal_restart_and_ack() {
    authenticated_response_roundtrip(0);
}

#[test]
fn provider_status_response_survives_authenticated_journal_restart_and_ack() {
    authenticated_response_roundtrip(24);
}

fn authenticated_response_roundtrip(operation: u64) {
    let server_key = SigningKey::from_bytes(&[0xa7; 32]);
    let server_public = server_key.verifying_key().to_bytes();
    let encrypted_store = EncryptedTestStore::new(&format!("op-{operation}"));
    let store_key = "c3".repeat(32);
    let connection = open_encrypted_test_connection(encrypted_store.path(), &store_key);
    let mut human = enrolled_human_with_connection(&server_public, connection);
    let package_body = human.core.key_packages(1, false).expect("human KeyPackage");
    let key_package = decode_strict(&package_body, |d| {
        d.array(2)?;
        d.array(1)?;
        let package = d.bytes()?.to_vec();
        d.null()?;
        Ok(package)
    })
    .expect("human KeyPackage response");
    let provider = OpenMlsRustCrypto::default();
    let (package, human_identity, human_dsk) = credential_from_key_package(&key_package, &provider);
    let bot = bot_identity();
    let human_list = decode_strict(&human.core.device_list_body().expect("own list"), |d| {
        d.array(4)?;
        d.uint()?;
        let blob = d.bytes()?.to_vec();
        d.bytes()?;
        d.bytes()?;
        Ok(blob)
    })
    .map(|blob| DeviceList::decode(&blob).expect("human signed list"))
    .expect("human list");
    seed_trust(&mut human, &bot, &human_identity, &human_list);

    let binding = DillaInteractionBinding {
        instance_id: InstanceId::from_bytes(INSTANCE),
        installation_id: INSTALLATION,
        community_id: CommunityId::from_bytes(COMMUNITY),
        bot_user_id: BOT_USER,
        participant_user_id: human.user,
        participant_device_id: DeviceId::from_bytes(human.device),
        policy_version: POLICY,
        e2ee_version: 1,
        media_version: 0,
    };
    let extensions = Extensions::<GroupContext>::try_from(vec![
        Extension::RequiredCapabilities(RequiredCapabilitiesExtension::new(
            &[ExtensionType::Unknown(DILLA_BINDING_ID)],
            &[],
            &[CredentialType::Basic],
        )),
        Extension::Unknown(DILLA_BINDING_ID, UnknownExtension(binding.encode())),
    ])
    .expect("kind-3 group context extensions");
    let config = MlsGroupCreateConfig::builder()
        .ciphersuite(CIPHERSUITE)
        .use_ratchet_tree_extension(false)
        .wire_format_policy(PURE_PLAINTEXT_WIRE_FORMAT_POLICY)
        .with_group_context_extensions(extensions)
        .capabilities(Capabilities::new(
            None,
            Some(&[
                CIPHERSUITE,
                Ciphersuite::MLS_128_DHKEMP256_AES128GCM_SHA256_P256,
                Ciphersuite::MLS_128_DHKEMX25519_CHACHA20POLY1305_SHA256_Ed25519,
            ]),
            Some(&[ExtensionType::Unknown(DILLA_BINDING_ID)]),
            None,
            Some(&[CredentialType::Basic]),
        ))
        .build();
    let mut group = MlsGroup::new_with_group_id(
        &provider,
        &bot.signer,
        &config,
        GroupId::from_slice(&GROUP_ID),
        bot.credential.clone(),
    )
    .expect("create Bot interaction group");
    let bundle = group
        .add_members(&provider, &bot.signer, &[package])
        .expect("Bot Add commit and Welcome");
    group
        .merge_pending_commit(&provider)
        .expect("merge human Add");
    assert_eq!(group.members().count(), 2);
    let welcome = bundle
        .1
        .tls_serialize_detached()
        .expect("serialize actual MLS Welcome");
    let tree = group
        .export_ratchet_tree()
        .tls_serialize_detached()
        .expect("serialize actual MLS ratchet tree");
    let group_info = group
        .export_group_info(provider.crypto(), &bot.signer, false)
        .expect("authenticated GroupInfo");
    let MlsMessageBodyOut::GroupInfo(group_info) = group_info.body() else {
        panic!("expected GroupInfo");
    };
    let tree_hash: [u8; 32] = group_info
        .group_context()
        .tree_hash()
        .try_into()
        .expect("tree hash length");
    let welcomes = welcomes_body(&welcome, &tree, &tree_hash);
    human
        .core
        .instance_signer_key_matches(&server_public)
        .expect("server key remains pinned after enrollment");
    let now = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .expect("clock")
        .as_secs();
    let good_proof = current_context_proof(
        &server_key,
        &human,
        &bot,
        &human_identity,
        &human_list,
        &human_dsk,
        &tree_hash,
        now - 2,
        now - 3,
        GROUP_ID,
    );
    let wrong_group_proof = current_context_proof(
        &server_key,
        &human,
        &bot,
        &human_identity,
        &human_list,
        &human_dsk,
        &tree_hash,
        now - 2,
        now - 3,
        [0x9f; 16],
    );
    let expected_context = dilla_core::bot::human_interaction_context::Expected {
        instance: InstanceId::from_bytes(INSTANCE),
        group: dilla_core::ids::MsgId::from_bytes(GROUP_ID),
        participant_user: UserId::from_bytes(human.user),
        participant_device: DeviceId::from_bytes(human.device),
        now,
    };
    let initial_context = dilla_core::bot::human_interaction_context::verify(
        &good_proof,
        &server_key.verifying_key().to_bytes(),
        &expected_context,
    )
    .expect("locally verify the valid server proof");
    let rejected = human
        .core
        .human_interaction_welcome_apply(&welcomes, &wrong_group_proof, now)
        .expect_err("a proof for another group is rejected");
    assert_eq!(rejected.code, "E_BINDING");
    assert!(client_support::groups(&human.core).is_empty());

    let accepted = human
        .core
        .human_interaction_welcome_apply(&welcomes, &good_proof, now)
        .expect("human accepts current Bot interaction Welcome");
    let accepted = decode_strict(&accepted, |d| {
        d.array(1)?;
        d.array(4)?;
        let id = d.uint()?;
        let group = d.bytes_exact::<16>()?;
        let outcome = d.uint()?;
        let reason = d.text()?.to_owned();
        Ok((id, group, outcome, reason))
    })
    .expect("accepted outcome");
    assert_eq!(accepted, (1, GROUP_ID, 0, String::new()));
    assert_eq!(
        client_support::groups(&human.core),
        vec![client_support::GroupRow {
            group_id: GROUP_ID,
            kind: 3,
            community_id: Some(COMMUNITY),
            target_id: BOT_USER,
            state: 2,
            epoch: 1,
            next_seq: 2,
            proposals_pending: 0,
            pending_commit: 0,
        }]
    );
    let provider = dilla_core::mls::DillaProvider::new(std::sync::Arc::clone(&human.probe));
    let stored = dilla_core::mls::DillaGroup::load(&provider, &GroupId::from_slice(&GROUP_ID))
        .expect("load persisted MLS state")
        .expect("persisted interaction group");
    assert_eq!(stored.kind(), dilla_core::mls::GroupKind::Interaction);
    assert_eq!(stored.members().len(), 2);

    let same_proof_replay = human
        .core
        .human_interaction_welcome_apply(&welcomes, &good_proof, now)
        .expect("same Welcome with same proof");
    let same_proof_replay = decode_strict(&same_proof_replay, |d| {
        d.array(1)?;
        d.array(4)?;
        d.uint()?;
        d.bytes_exact::<16>()?;
        let outcome = d.uint()?;
        d.text()?;
        Ok(outcome)
    })
    .expect("same-proof replay outcome");
    assert_eq!(same_proof_replay, 1);

    let fresh_proof = current_context_proof(
        &server_key,
        &human,
        &bot,
        &human_identity,
        &human_list,
        &human_dsk,
        &tree_hash,
        now - 1,
        now - 3,
        GROUP_ID,
    );
    assert_ne!(fresh_proof, good_proof, "a refreshed proof has new bytes");
    let fresh_context = dilla_core::bot::human_interaction_context::verify(
        &fresh_proof,
        &server_key.verifying_key().to_bytes(),
        &expected_context,
    )
    .expect("refreshed proof verifies");
    assert_eq!(fresh_context.registration, initial_context.registration);
    assert_eq!(fresh_context.binding, initial_context.binding);
    let before_replay = client_support::groups(&human.core);
    let replay = human
        .core
        .human_interaction_welcome_apply(&welcomes, &fresh_proof, now)
        .expect("same Welcome with refreshed proof");
    let replay = decode_strict(&replay, |d| {
        d.array(1)?;
        d.array(4)?;
        d.uint()?;
        d.bytes_exact::<16>()?;
        let outcome = d.uint()?;
        let reason = d.text()?.to_owned();
        Ok((outcome, reason))
    })
    .expect("replay outcome");
    assert_eq!(replay, (1, String::new()));
    assert_eq!(client_support::groups(&human.core), before_replay);

    let altered = mutate_welcome_body(&welcomes);
    let changed = human
        .core
        .human_interaction_welcome_apply(&altered, &fresh_proof, now)
        .expect("changed Welcome is returned as a refusal");
    let changed = decode_strict(&changed, |d| {
        d.array(1)?;
        d.array(4)?;
        d.uint()?;
        d.bytes_exact::<16>()?;
        let outcome = d.uint()?;
        let reason = d.text()?.to_owned();
        Ok((outcome, reason))
    })
    .expect("changed-Welcome outcome");
    assert_eq!(changed, (2, "E_BINDING".to_owned()));
    assert_eq!(client_support::groups(&human.core), before_replay);

    // A kind-3 send uses the already-authenticated MLS binding and a stable request id as its
    // outbox message id, so an exact retry returns the same encrypted-send handle.
    let request_id = [0x42; 16];
    let mut bot_request = Encoder::new();
    bot_request
        .array(8)
        .uint(1)
        .bytes(&request_id)
        .bytes(&COMMUNITY)
        .null()
        .uint(operation)
        .null()
        .null();
    if operation == 60 {
        bot_request.array(1).null();
    } else {
        bot_request.array(0);
    }
    let bot_request = bot_request.into_vec();
    // Music commands carry the authenticated voice channel in the protocol tuple. Mutations
    // also carry the caller's expected revision and short expiry, while the actor stays implicit.
    let music_state_id = [0x49; 16];
    let mut music_state = Encoder::new();
    music_state
        .array(8)
        .uint(1)
        .bytes(&music_state_id)
        .bytes(&COMMUNITY)
        .bytes(&[0x4a; 16])
        .uint(10)
        .null()
        .null()
        .array(0);
    assert!(
        human
            .core
            .human_interaction_send_prepare(
                &music_state_id,
                &COMMUNITY,
                &GROUP_ID,
                &music_state.into_vec(),
                now,
            )
            .is_ok(),
        "music state requests require and accept a voice channel"
    );
    let music_mutation_id = [0x4b; 16];
    let mut music_mutation = Encoder::new();
    music_mutation
        .array(8)
        .uint(1)
        .bytes(&music_mutation_id)
        .bytes(&COMMUNITY)
        .bytes(&[0x4a; 16])
        .uint(12)
        .uint(7)
        .uint(now + 20)
        .array(1)
        .text("a song");
    assert!(
        human
            .core
            .human_interaction_send_prepare(
                &music_mutation_id,
                &COMMUNITY,
                &GROUP_ID,
                &music_mutation.into_vec(),
                now,
            )
            .is_ok(),
        "music mutations with a channel, revision, and live expiry are accepted"
    );
    let expired_mutation_id = [0x4c; 16];
    let mut expired_mutation = Encoder::new();
    expired_mutation
        .array(8)
        .uint(1)
        .bytes(&expired_mutation_id)
        .bytes(&COMMUNITY)
        .bytes(&[0x4a; 16])
        .uint(12)
        .uint(7)
        .uint(now)
        .array(1)
        .text("a song");
    assert!(
        human
            .core
            .human_interaction_send_prepare(
                &expired_mutation_id,
                &COMMUNITY,
                &GROUP_ID,
                &expired_mutation.into_vec(),
                now,
            )
            .is_err(),
        "expired music mutations are refused"
    );
    let wrong_channel_id = [0x4d; 16];
    let mut wrong_channel = Encoder::new();
    wrong_channel
        .array(8)
        .uint(1)
        .bytes(&wrong_channel_id)
        .bytes(&COMMUNITY)
        .null()
        .uint(10)
        .null()
        .null()
        .array(0);
    assert!(
        human
            .core
            .human_interaction_send_prepare(
                &wrong_channel_id,
                &COMMUNITY,
                &GROUP_ID,
                &wrong_channel.into_vec(),
                now,
            )
            .is_err(),
        "music requests without a voice channel are refused"
    );
    let unknown_operation_id = [0x4e; 16];
    let mut unknown_operation = Encoder::new();
    unknown_operation
        .array(8)
        .uint(1)
        .bytes(&unknown_operation_id)
        .bytes(&COMMUNITY)
        .null()
        .uint(99)
        .null()
        .null()
        .array(0);
    assert!(
        human
            .core
            .human_interaction_send_prepare(
                &unknown_operation_id,
                &COMMUNITY,
                &GROUP_ID,
                &unknown_operation.into_vec(),
                now,
            )
            .is_err(),
        "unknown Bot operations are refused"
    );
    let prepared = human
        .core
        .human_interaction_send_prepare(&request_id, &COMMUNITY, &GROUP_ID, &bot_request, now)
        .expect("prepare encrypted human interaction request");
    let empty_events = human
        .core
        .human_interaction_events_read(0, 10)
        .expect("read empty human interaction event journal");
    let empty_page = decode_strict(&empty_events, |d| {
        d.array(3)?;
        let count = d.array_len()?;
        let next = d.uint()?;
        let high = d.uint()?;
        Ok((count, next, high))
    })
    .expect("empty event page shape");
    assert_eq!(empty_page, (0, 0, 0));
    assert!(human.core.human_interaction_events_ack(1).is_err());
    let encrypted = human
        .core
        .send_encrypt(&request_id)
        .expect("encrypt interaction request");
    let (encrypted_group, upload) = decode_strict(&encrypted, |d| {
        d.array(2)?;
        Ok((d.bytes_exact::<16>()?, d.bytes()?.to_vec()))
    })
    .expect("encrypted upload shape");
    let (request_epoch, request_blob) = decode_strict(&upload, |d| {
        d.array(3)?;
        let epoch = d.uint()?;
        let blob = d.bytes()?.to_vec();
        d.uint()?;
        Ok((epoch, blob))
    })
    .expect("encoded upload body");
    assert_eq!(encrypted_group, GROUP_ID);
    assert!(!upload.is_empty());
    assert_eq!(
        human
            .core
            .human_interaction_send_prepare(&request_id, &COMMUNITY, &GROUP_ID, &bot_request, now,)
            .expect("idempotent request retry"),
        prepared
    );
    // The response journal only accepts a reply to a request that the local device uploaded.
    let mut confirm = Encoder::new();
    confirm.array(3).uint(2).bytes(&[2; 32]).uint(now + 2);
    human
        .core
        .send_confirm(&request_id, &confirm.into_vec())
        .expect("confirm the local request upload");
    let request_echo = client_support::MsgRow {
        seq: 2,
        epoch: request_epoch,
        uploader: human.device,
        commitment: client_support::commitment_of(&request_blob),
        blob: Some(request_blob.clone()),
        franking_tag: [0x32; 32],
        recv_ts: now + 2,
        deleted: false,
        class: None,
    };
    human
        .core
        .group_apply(
            &GROUP_ID,
            &[0x80],
            &client_support::encode_messages(&[&request_echo]),
            2,
        )
        .expect("apply local request echo");
    human
        .probe
        .lock()
        .expect("connection")
        .execute(
            "DELETE FROM app_outbox WHERE msg_id=?1",
            [request_id.as_slice()],
        )
        .expect("simulate completed upload cleanup");
    assert_eq!(
        human
            .core
            .human_interaction_send_prepare(&request_id, &COMMUNITY, &GROUP_ID, &bot_request, now,)
            .expect("completed interaction request remains idempotent"),
        prepared
    );
    let request_message = MlsMessageIn::tls_deserialize_exact(&request_blob)
        .expect("deserialize request echo")
        .try_into_protocol_message()
        .expect("request echo protocol message");
    group
        .process_message(&provider, request_message)
        .expect("advance Bot group with request echo");

    let response = Response {
        request_id: MsgId::from_bytes(request_id),
        community_id: CommunityId::from_bytes(COMMUNITY),
        status: 0,
        revision: None,
        data: vec![],
    }
    .encode_body()
    .expect("encode Bot response");
    let response_envelope = Envelope {
        v: 1,
        msg_id: MsgId::from_bytes([0x4a; 16]),
        kind: EnvelopeType::Message,
        thread_id: None,
        reply_to: None,
        body: response.clone(),
        attachments: Vec::new(),
        previews: Vec::new(),
        k_f: [0x06; 32],
    };
    let response_commitment = response_envelope.commitment().expect("response commitment");
    let response_plaintext = response_envelope.encode().expect("encode envelope");
    group.set_aad(response_commitment.to_vec());
    let response_blob = group
        .create_message(&provider, &bot.signer, &response_plaintext)
        .expect("encrypt Bot response")
        .tls_serialize_detached()
        .expect("serialize Bot response");
    drop(provider);
    let response_row = client_support::MsgRow {
        seq: 3,
        epoch: 1,
        uploader: BOT_DEVICE,
        commitment: Some(response_commitment),
        blob: Some(response_blob),
        franking_tag: [0x33; 32],
        recv_ts: now + 3,
        deleted: false,
        class: None,
    };
    human
        .core
        .group_apply(
            &GROUP_ID,
            &[0x80],
            &client_support::encode_messages(&[&response_row]),
            3,
        )
        .expect("apply authenticated Bot response");
    let received = human
        .core
        .human_interaction_events_read(0, 10)
        .expect("read authenticated Bot response");
    let response_count = decode_strict(&received, |d| {
        d.array(3)?;
        let count = d.array_len()?;
        for _ in 0..count {
            d.array(13)?;
            d.uint()?;
            d.bytes_exact::<16>()?;
            d.bytes_exact::<16>()?;
            d.bytes_exact::<16>()?;
            d.uint()?;
            d.uint()?;
            d.uint()?;
            d.bytes_exact::<16>()?;
            d.bytes_exact::<16>()?;
            d.bytes_exact::<16>()?;
            d.text()?;
            d.uint()?;
            d.opt_bytes_exact::<16>()?;
        }
        d.uint()?;
        d.uint()?;
        Ok(count)
    })
    .expect("response page shape");
    assert_eq!(response_count, 1);
    let prearchive_outbox_id = [0x96u8; 16];
    human
        .probe
        .lock()
        .expect("connection")
        .execute(
            "INSERT INTO app_outbox(msg_id,group_id,envelope,created,state,epoch) VALUES(?1,?2,X'80',?3,1,1)",
            rusqlite::params![prearchive_outbox_id.as_slice(), GROUP_ID.as_slice(), now as i64],
        )
        .expect("seed a retained outbound state in the pre-archive store");
    let outbox_snapshot_before_upgrade = human
        .probe
        .lock()
        .expect("connection")
        .query_row(
            "SELECT msg_id,group_id,envelope,created,state,error,epoch FROM app_outbox WHERE msg_id=?1",
            [prearchive_outbox_id.as_slice()],
            |row| {
                Ok((
                    row.get::<_, Vec<u8>>(0)?,
                    row.get::<_, Vec<u8>>(1)?,
                    row.get::<_, Vec<u8>>(2)?,
                    row.get::<_, i64>(3)?,
                    row.get::<_, i64>(4)?,
                    row.get::<_, String>(5)?,
                    row.get::<_, i64>(6)?,
                ))
            },
        )
        .expect("capture pending outbound row before archive migration");
    let (pending_body_before_upgrade, claims_before_upgrade, outbox_before_upgrade) = {
        let connection = human.probe.lock().expect("connection");
        connection
            .query_row(
                "SELECT (SELECT body FROM app_human_interaction_events WHERE delivery_id=1), (SELECT count(*) FROM app_human_interaction_response_claims WHERE group_id=?1 AND request_id=?2), (SELECT count(*) FROM app_outbox WHERE msg_id=?3)",
                rusqlite::params![GROUP_ID.as_slice(), request_id.as_slice(), prearchive_outbox_id.as_slice()],
                |row| Ok((row.get::<_, String>(0)?, row.get::<_, i64>(1)?, row.get::<_, i64>(2)?)),
            )
            .expect("capture pending response, claim and outbound request before upgrade")
    };
    assert_eq!(pending_body_before_upgrade, response);
    assert_eq!(claims_before_upgrade, 1);
    assert_eq!(outbox_before_upgrade, 1);
    human
        .probe
        .lock()
        .expect("connection")
        .execute_batch(
            "CREATE TABLE test_receipt_restart_seed AS SELECT * FROM app_human_interaction_events WHERE delivery_id=1;",
        )
        .expect("save the authenticated event fixture in the encrypted store");
    human
        .probe
        .lock()
        .expect("connection")
        .execute_batch("DROP TABLE app_human_interaction_receipts;")
        .expect("fixture a real encrypted pre-archive store");

    // Opening a pre-archive encrypted store adds only the archive schema and preserves pending
    // response, outbox and response-claim state.
    human = reopen_encrypted_test_store(human, encrypted_store.path(), &store_key);
    let (pending_body_after_upgrade, claims_after_upgrade, outbox_count_after_upgrade, archive_exists): (
        String,
        i64,
        i64,
        i64,
    ) = human
        .probe
        .lock()
        .expect("connection")
        .query_row(
            "SELECT (SELECT body FROM app_human_interaction_events WHERE delivery_id=1), (SELECT count(*) FROM app_human_interaction_response_claims WHERE group_id=?1 AND request_id=?2), (SELECT count(*) FROM app_outbox WHERE msg_id=?3), (SELECT count(*) FROM sqlite_schema WHERE type='table' AND name='app_human_interaction_receipts')",
            rusqlite::params![GROUP_ID.as_slice(), request_id.as_slice(), prearchive_outbox_id.as_slice()],
            |row| Ok((row.get(0)?, row.get(1)?, row.get(2)?, row.get(3)?)),
        )
        .expect("read migrated encrypted store");
    assert_eq!(pending_body_after_upgrade, response);
    assert_eq!(claims_after_upgrade, claims_before_upgrade);
    assert_eq!(outbox_count_after_upgrade, outbox_before_upgrade);
    assert_eq!(archive_exists, 1, "archive migration created the new table");
    let outbox_row_after_upgrade = human
        .probe
        .lock()
        .expect("connection")
        .query_row(
            "SELECT msg_id,group_id,envelope,created,state,error,epoch FROM app_outbox WHERE msg_id=?1",
            [prearchive_outbox_id.as_slice()],
            |row| {
                Ok((
                    row.get::<_, Vec<u8>>(0)?,
                    row.get::<_, Vec<u8>>(1)?,
                    row.get::<_, Vec<u8>>(2)?,
                    row.get::<_, i64>(3)?,
                    row.get::<_, i64>(4)?,
                    row.get::<_, String>(5)?,
                    row.get::<_, i64>(6)?,
                ))
            },
        )
        .expect("read outbound row after pre-archive migration");
    assert_eq!(outbox_row_after_upgrade, outbox_snapshot_before_upgrade);
    let after_restart = human
        .core
        .human_interaction_events_read(0, 10)
        .expect("read persisted Bot response");
    assert_eq!(
        decode_strict(&after_restart, |d| {
            d.array(3)?;
            let count = d.array_len()?;
            for _ in 0..count {
                d.array(13)?;
                d.uint()?;
                d.bytes_exact::<16>()?;
                d.bytes_exact::<16>()?;
                d.bytes_exact::<16>()?;
                d.uint()?;
                d.uint()?;
                d.uint()?;
                d.bytes_exact::<16>()?;
                d.bytes_exact::<16>()?;
                d.bytes_exact::<16>()?;
                d.text()?;
                d.uint()?;
                d.opt_bytes_exact::<16>()?;
            }
            d.uint()?;
            d.uint()?;
            Ok(count)
        })
        .expect("persisted response page"),
        1
    );
    {
        let connection = human.probe.lock().expect("connection");
        connection
            .execute(
                "INSERT INTO app_human_interaction_events(group_id,community_id,request_id,source_seq,epoch,recv_ts,sender_user,sender_device,sender_leaf,sender_credential,sender_sig_key,message_id,body) SELECT group_id,community_id,request_id,source_seq+1,epoch,recv_ts,?1,sender_device,sender_leaf,sender_credential,sender_sig_key,message_id,body FROM app_human_interaction_events WHERE delivery_id=1",
                [[0xfe; 16].as_slice()],
            )
            .expect("insert forged sender journal row");
        connection
            .execute(
                "INSERT INTO app_human_interaction_events(group_id,community_id,request_id,source_seq,epoch,recv_ts,sender_user,sender_device,sender_leaf,sender_credential,sender_sig_key,message_id,body) SELECT ?1,community_id,request_id,source_seq,epoch,recv_ts,sender_user,sender_device,sender_leaf,sender_credential,sender_sig_key,message_id,body FROM app_human_interaction_events WHERE delivery_id=1",
                [[0xfd; 16].as_slice()],
            )
            .expect("insert wrong-group journal row");
        connection
            .execute(
                "UPDATE app_human_interaction_event_state SET high_water=(SELECT max(delivery_id) FROM app_human_interaction_events) WHERE singleton=1",
                [],
            )
            .expect("advance test journal high water");
    }
    let with_forged_rows = human
        .core
        .human_interaction_events_read(0, 10)
        .expect("filter forged sender and wrong group events");
    assert_eq!(
        decode_strict(&with_forged_rows, |d| {
            d.array(3)?;
            let count = d.array_len()?;
            for _ in 0..count {
                d.array(13)?;
                d.uint()?;
                d.bytes_exact::<16>()?;
                d.bytes_exact::<16>()?;
                d.bytes_exact::<16>()?;
                d.uint()?;
                d.uint()?;
                d.uint()?;
                d.bytes_exact::<16>()?;
                d.bytes_exact::<16>()?;
                d.bytes_exact::<16>()?;
                d.text()?;
                d.uint()?;
                d.opt_bytes_exact::<16>()?;
            }
            d.uint()?;
            d.uint()?;
            Ok(count)
        })
        .expect("forged provenance page"),
        1
    );
    human
        .core
        .group_apply(
            &GROUP_ID,
            &[0x80],
            &client_support::encode_messages(&[&response_row]),
            3,
        )
        .expect("replay already-applied response row");
    let replayed = human
        .core
        .human_interaction_events_read(0, 10)
        .expect("read after response replay");
    assert_eq!(
        decode_strict(&replayed, |d| {
            d.array(3)?;
            let count = d.array_len()?;
            for _ in 0..count {
                d.array(13)?;
                d.uint()?;
                d.bytes_exact::<16>()?;
                d.bytes_exact::<16>()?;
                d.bytes_exact::<16>()?;
                d.uint()?;
                d.uint()?;
                d.uint()?;
                d.bytes_exact::<16>()?;
                d.bytes_exact::<16>()?;
                d.bytes_exact::<16>()?;
                d.text()?;
                d.uint()?;
                d.opt_bytes_exact::<16>()?;
            }
            d.uint()?;
            d.uint()?;
            Ok(count)
        })
        .expect("replayed response page"),
        1
    );
    human
        .probe
        .lock()
        .expect("connection")
        .execute(
            "UPDATE app_trust_pins SET changed_umk=zeroblob(32) WHERE user_id=?1",
            [BOT_USER.as_slice()],
        )
        .expect("revoke the Bot sender trust decision");
    let after_trust_revocation = human
        .core
        .human_interaction_events_read(0, 10)
        .expect("recheck trust when rereading the durable response");
    assert_eq!(
        decode_strict(&after_trust_revocation, |d| {
            d.array(3)?;
            let count = d.array_len()?;
            d.uint()?;
            d.uint()?;
            Ok(count)
        })
        .expect("revoked response page shape"),
        0,
        "trust revocation suppresses a previously readable response"
    );
    {
        let probe = human.probe.lock().expect("connection");
        probe.execute(
            "INSERT INTO app_human_interaction_events(group_id,community_id,request_id,source_seq,epoch,recv_ts,sender_user,sender_device,sender_leaf,sender_credential,sender_sig_key,message_id,body) SELECT group_id,community_id,request_id,source_seq+1,epoch,recv_ts,sender_user,sender_device,sender_leaf,sender_credential,sender_sig_key,message_id,'pending' FROM app_human_interaction_events WHERE delivery_id=3",
            [],
        ).expect("seed a later unconsumed response row");
        probe
            .execute(
                "UPDATE app_human_interaction_event_state SET high_water=4 WHERE singleton=1",
                [],
            )
            .expect("seed journal high water");
    }
    let empty_selection = Encoder::new();
    assert!(
        human
            .core
            .human_interaction_events_ack_selected(&empty_selection.into_vec())
            .is_err()
    );
    let duplicate_selection = {
        let mut e = Encoder::new();
        e.array(2).uint(3).uint(3);
        e.into_vec()
    };
    assert!(
        human
            .core
            .human_interaction_events_ack_selected(&duplicate_selection)
            .is_err()
    );
    let first_selection = {
        let mut e = Encoder::new();
        e.array(1).uint(3);
        e.into_vec()
    };
    human
        .core
        .human_interaction_events_ack_selected(&first_selection)
        .expect("selectively consume response");
    human
        .core
        .human_interaction_events_ack_selected(&first_selection)
        .expect("repeated selective consume is idempotent");
    let (ack, selected_exists, later_body): (i64, i64, Option<String>) = human.probe.lock().expect("connection").query_row(
        "SELECT (SELECT ack_delivery_id FROM app_human_interaction_event_state WHERE singleton=1),
          (SELECT count(*) FROM app_human_interaction_events WHERE delivery_id=3),
          (SELECT body FROM app_human_interaction_events WHERE delivery_id=4)",
        [], |row| Ok((row.get(0)?, row.get(1)?, row.get(2)?)),
    ).expect("read selective acknowledgement state");
    assert_eq!(selected_exists, 0, "the named consumed response was pruned");
    assert_eq!(
        later_body.as_deref(),
        Some("pending"),
        "the later unselected response body remains durable"
    );
    assert_eq!(
        ack, 3,
        "the global prefix advances only through the consumed response"
    );
    let final_selection = {
        let mut e = Encoder::new();
        e.array(1).uint(4);
        e.into_vec()
    };
    human
        .core
        .human_interaction_events_ack_selected(&final_selection)
        .expect("consume remaining response");
    human
        .core
        .human_interaction_events_ack(4)
        .expect("compatibility cleanup for preceding unconsumed rows");
    human = reopen_encrypted_test_store(human, encrypted_store.path(), &store_key);
    let receipts: i64 = human
        .probe
        .lock()
        .expect("connection")
        .query_row(
            "SELECT count(*) FROM app_human_interaction_response_claims WHERE group_id=?1 AND request_id=?2",
            rusqlite::params![GROUP_ID.as_slice(), request_id.as_slice()],
            |r| r.get(0),
        )
        .expect("read durable response receipt");
    assert_eq!(receipts, 1, "response dedup survives ack and restart");
    assert_eq!(
        human
            .core
            .human_interaction_send_prepare(&request_id, &COMMUNITY, &GROUP_ID, &bot_request, now)
            .expect("reload and recognize completed interaction request"),
        prepared
    );
    human
        .core
        .group_apply(
            &GROUP_ID,
            &[0x80],
            &client_support::encode_messages(&[&response_row]),
            3,
        )
        .expect("replay acknowledged response row");
    let after_ack = human
        .core
        .human_interaction_events_read(0, 10)
        .expect("read after ack and restart");
    assert_eq!(
        decode_strict(&after_ack, |d| {
            d.array(3)?;
            let count = d.array_len()?;
            d.uint()?;
            d.uint()?;
            Ok(count)
        })
        .expect("empty response page after ack"),
        0
    );
    let mut changed_request = Encoder::new();
    changed_request
        .array(8)
        .uint(1)
        .bytes(&request_id)
        .bytes(&COMMUNITY)
        .null()
        .uint(50)
        .null()
        .null()
        .array(1)
        .uint(0);
    assert!(
        human
            .core
            .human_interaction_send_prepare(
                &request_id,
                &COMMUNITY,
                &GROUP_ID,
                &changed_request.into_vec(),
                now,
            )
            .is_err(),
        "request ids cannot be reused for a different operation"
    );
    assert!(
        human
            .core
            .human_interaction_send_prepare(&request_id, &[0x23; 16], &GROUP_ID, &bot_request, now,)
            .is_err()
    );
    let ordinary_group = [0x45; 16];
    human
        .core
        .group_create(
            &ordinary_group,
            Some(&COMMUNITY),
            &[0x46; 16],
            POLICY,
            &[0x47; 32],
        )
        .expect("ordinary group fixture");
    human
        .core
        .group_registered(&ordinary_group, 1)
        .expect("ordinary group registration");
    assert!(
        human
            .core
            .human_interaction_send_prepare(
                &[0x48; 16],
                &COMMUNITY,
                &ordinary_group,
                &bot_request,
                now,
            )
            .is_err(),
        "ordinary text groups cannot use the interaction request API"
    );
    // Exercise the corrected transfer after the original receipt has been read, the worker/core
    // has restarted, and older journal rows have been consumed. The stored event remains bound
    // to the real source request and currently active interaction group.
    {
        let probe = human.probe.lock().expect("connection");
        probe
            .execute(
                "UPDATE app_trust_pins SET changed_umk=NULL WHERE user_id=?1",
                [BOT_USER.as_slice()],
            )
            .expect("restore current sender trust for receipt recovery test");
        probe
            .execute(
                "UPDATE app_groups SET max_epoch=epoch WHERE group_id=?1",
                [GROUP_ID.as_slice()],
            )
            .expect("restore active interaction group epoch");
        probe.execute("INSERT INTO app_human_interaction_events(delivery_id,group_id,community_id,request_id,source_seq,epoch,recv_ts,sender_user,sender_device,sender_leaf,sender_credential,sender_sig_key,message_id,body,disposition) SELECT 5,group_id,community_id,request_id,source_seq,epoch,recv_ts,sender_user,sender_device,sender_leaf,sender_credential,sender_sig_key,message_id,body,0 FROM test_receipt_restart_seed", [])
            .expect("restore valid event after older cursor checks");
        probe
            .execute(
                "DELETE FROM app_human_interaction_receipts WHERE delivery_id=1",
                [],
            )
            .expect("isolate the final receipt transfer from the earlier test ACK");
        probe
            .execute(
                "UPDATE app_human_interaction_event_state SET high_water=5 WHERE singleton=1",
                [],
            )
            .expect("advance event high water");
    }
    human = reopen_encrypted_test_store(human, encrypted_store.path(), &store_key);
    let selected = {
        let mut e = Encoder::new();
        e.array(1).uint(5);
        e.into_vec()
    };
    human.probe.lock().expect("connection").execute_batch(
        "CREATE TRIGGER fail_receipt_transfer BEFORE INSERT ON app_human_interaction_receipts BEGIN SELECT RAISE(FAIL,'transfer interrupted'); END;",
    ).expect("install transactional failure trigger");
    assert!(
        human
            .core
            .human_interaction_events_ack_selected(&selected)
            .is_err(),
        "failed archive writes cannot consume journal data"
    );
    let after_failure = human
        .core
        .human_interaction_events_read(4, 10)
        .expect("pending row survived failed transfer");
    assert_eq!(
        decode_strict(&after_failure, |d| {
            d.array(3)?;
            let n = d.array_len()?;
            for _ in 0..n {
                d.skip()?;
            }
            d.uint()?;
            d.uint()?;
            Ok(n)
        })
        .unwrap(),
        1
    );
    human = reopen_encrypted_test_store(human, encrypted_store.path(), &store_key);
    let after_failure_restart = human
        .core
        .human_interaction_events_read(4, 10)
        .expect("read the pending response after closing and reopening the encrypted store");
    let pending_body_after_failure = decode_strict(&after_failure_restart, |d| {
        d.array(3)?;
        d.array(1)?;
        d.array(13)?;
        d.uint()?;
        d.bytes_exact::<16>()?;
        d.bytes_exact::<16>()?;
        d.bytes_exact::<16>()?;
        d.uint()?;
        d.uint()?;
        d.uint()?;
        d.bytes_exact::<16>()?;
        d.bytes_exact::<16>()?;
        d.bytes_exact::<16>()?;
        let body = d.text()?.to_owned();
        d.uint()?;
        d.opt_bytes_exact::<16>()?;
        d.uint()?;
        d.uint()?;
        Ok(body)
    })
    .expect("pending exact event after failed archive transfer survives encrypted reopen");
    assert_eq!(pending_body_after_failure, response);
    human
        .probe
        .lock()
        .expect("connection")
        .execute_batch("DROP TRIGGER fail_receipt_transfer;")
        .expect("remove transfer trigger");
    human
        .core
        .human_interaction_events_ack_selected(&selected)
        .expect("transfer response into native archive");
    human
        .core
        .human_interaction_events_ack_selected(&selected)
        .expect("duplicate transfer ACK remains idempotent");
    human = reopen_encrypted_test_store(human, encrypted_store.path(), &store_key);
    let mut request_ids = Encoder::new();
    request_ids.array(1).bytes(&request_id);
    let archived = human
        .core
        .human_interaction_receipts_read(
            &COMMUNITY,
            &INSTALLATION,
            &BOT_USER,
            &request_ids.into_vec(),
        )
        .expect("recover archived response from reopened native core");
    let recovered = decode_strict(&archived, |d| {
        let count = d.array_len()?;
        let mut events = Vec::with_capacity(count);
        for _ in 0..count {
            d.array(13)?;
            let delivery = d.uint()?;
            let group = d.bytes_exact::<16>()?;
            let community = d.bytes_exact::<16>()?;
            let request = d.bytes_exact::<16>()?;
            let source_seq = d.uint()?;
            let epoch = d.uint()?;
            let recv_ts = d.uint()?;
            let sender = d.bytes_exact::<16>()?;
            let sender_device = d.bytes_exact::<16>()?;
            let message_id = d.bytes_exact::<16>()?;
            let body = d.text()?.to_owned();
            let operation = d.uint()?;
            d.opt_bytes_exact::<16>()?;
            events.push((
                delivery,
                group,
                community,
                request,
                source_seq,
                epoch,
                recv_ts,
                sender,
                sender_device,
                message_id,
                body,
                operation,
            ));
        }
        Ok(events)
    })
    .expect("recover archived responses with their exact source tuples");
    assert_eq!(recovered.iter().map(|row| row.0).collect::<Vec<_>>(), [5]);
    for (
        delivery,
        group,
        community,
        archived_request,
        source_seq,
        epoch,
        recv_ts,
        sender,
        sender_device,
        message_id,
        recovered_body,
        archived_operation,
    ) in &recovered
    {
        assert_eq!(*group, GROUP_ID);
        assert_eq!(*community, COMMUNITY);
        assert_eq!(*archived_request, request_id);
        assert_eq!(*source_seq, 3);
        assert_eq!(*epoch, 1);
        assert_eq!(*recv_ts, now + 3);
        assert_eq!(*sender, BOT_USER);
        assert_eq!(*sender_device, BOT_DEVICE);
        assert_eq!(*message_id, [0x4a; 16]);
        assert_eq!(*archived_operation, operation);
        assert_eq!(
            recovered_body, &response,
            "archive retains the exact authenticated response body"
        );
        assert!(
            Response::decode_body(recovered_body.as_bytes()).expect("decode recovered response")
                == Response {
                    request_id: MsgId::from_bytes(request_id),
                    community_id: CommunityId::from_bytes(COMMUNITY),
                    status: 0,
                    revision: None,
                    data: vec![],
                },
            "the recovered status, revision, data and identity match the original response"
        );
        assert!([1, 5].contains(delivery));
    }
    let archive_count: i64 = human
        .probe
        .lock()
        .expect("connection")
        .query_row(
            "SELECT count(*) FROM app_human_interaction_receipts WHERE request_id=?1 AND delivery_id=5",
            [request_id.as_slice()],
            |row| row.get(0),
        )
        .expect("count exact durable receipts after duplicate ACK");
    assert_eq!(
        archive_count, 1,
        "duplicate ACK persisted exactly one receipt"
    );
    let source_match_count: i64 = human
        .probe
        .lock()
        .expect("connection")
        .query_row(
            "SELECT count(*) FROM app_human_interaction_receipts r JOIN app_messages m ON m.group_id=r.group_id AND m.seq=r.source_seq WHERE r.request_id=?1 AND r.delivery_id=5 AND r.epoch=m.epoch AND r.sender_user=m.sender_user AND r.sender_device=m.sender_device AND r.sender_leaf=m.sender_leaf AND r.sender_sig_key=m.sender_sig_key AND r.message_id=m.msg_id AND r.body=m.body AND m.status=0 AND m.type=0",
            [request_id.as_slice()],
            |row| row.get(0),
        )
        .expect("compare every archived source field to authenticated stored message");
    assert_eq!(
        source_match_count, 1,
        "archived provenance exactly matches original authenticated message"
    );
    let mut request_ids = Encoder::new();
    request_ids.array(1).bytes(&request_id);
    assert_eq!(
        decode_strict(
            &human
                .core
                .human_interaction_receipts_read(
                    &COMMUNITY,
                    &[0x99; 16],
                    &BOT_USER,
                    &request_ids.into_vec()
                )
                .unwrap(),
            |d| d.array_len(),
        )
        .unwrap(),
        0,
        "a replaced installation cannot read the prior installation's receipt"
    );
    let mut request_ids = Encoder::new();
    request_ids.array(1).bytes(&request_id);
    assert_eq!(
        decode_strict(
            &human
                .core
                .human_interaction_receipts_read(
                    &COMMUNITY,
                    &INSTALLATION,
                    &[0x98; 16],
                    &request_ids.into_vec()
                )
                .unwrap(),
            |d| d.array_len()
        )
        .unwrap(),
        0,
        "a different current Bot identity cannot read the prior identity's receipt"
    );
    human.probe.lock().expect("connection").execute(
        "UPDATE app_human_interaction_receipts SET source_seq=source_seq+100 WHERE delivery_id=5", [],
    ).expect("tamper archived source provenance");
    let mut request_ids = Encoder::new();
    request_ids.array(1).bytes(&request_id);
    assert_eq!(
        decode_strict(
            &human
                .core
                .human_interaction_receipts_read(
                    &COMMUNITY,
                    &INSTALLATION,
                    &BOT_USER,
                    &request_ids.into_vec()
                )
                .unwrap(),
            |d| d.array_len()
        )
        .unwrap(),
        0,
        "a source sequence mismatch fails closed"
    );
    let substituted_response = Response {
        request_id: MsgId::from_bytes(request_id),
        community_id: CommunityId::from_bytes(COMMUNITY),
        status: 5,
        revision: Some(9),
        data: vec![0x80],
    }
    .encode_body()
    .expect("encode canonical substituted response");
    human.probe.lock().expect("connection").execute(
        "UPDATE app_human_interaction_receipts SET source_seq=(SELECT source_seq FROM test_receipt_restart_seed), body=?1 WHERE delivery_id=5", [substituted_response],
    ).expect("restore source and substitute canonical response body");
    let mut request_ids = Encoder::new();
    request_ids.array(1).bytes(&request_id);
    assert_eq!(
        decode_strict(
            &human
                .core
                .human_interaction_receipts_read(
                    &COMMUNITY,
                    &INSTALLATION,
                    &BOT_USER,
                    &request_ids.into_vec()
                )
                .unwrap(),
            |d| d.array_len()
        )
        .unwrap(),
        0,
        "a valid but substituted response body cannot be returned as authenticated evidence"
    );
    human.probe.lock().expect("connection").execute(
        "UPDATE app_human_interaction_receipts SET body=(SELECT body FROM test_receipt_restart_seed) WHERE delivery_id=5", [],
    ).expect("restore archived response before trust check");
    human
        .probe
        .lock()
        .expect("connection")
        .execute(
            "UPDATE app_trust_pins SET changed_umk=zeroblob(32) WHERE user_id=?1",
            [BOT_USER.as_slice()],
        )
        .expect("revoke current Bot trust after receipt storage");
    let mut request_ids = Encoder::new();
    request_ids.array(1).bytes(&request_id);
    assert_eq!(
        decode_strict(
            &human
                .core
                .human_interaction_receipts_read(
                    &COMMUNITY,
                    &INSTALLATION,
                    &BOT_USER,
                    &request_ids.into_vec()
                )
                .unwrap(),
            |d| d.array_len(),
        )
        .unwrap(),
        0,
        "archived plaintext is not returned after current trust revocation"
    );

    human
        .probe
        .lock()
        .expect("connection")
        .execute(
            "UPDATE app_groups SET max_epoch=epoch+1 WHERE group_id=?1",
            [GROUP_ID.as_slice()],
        )
        .expect("mark interaction group stale");
    assert!(
        human
            .core
            .human_interaction_send_prepare(&[0x43; 16], &COMMUNITY, &GROUP_ID, &bot_request, now,)
            .is_err(),
        "a stale local epoch must not prepare a request"
    );
}
