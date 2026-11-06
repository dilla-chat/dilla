//! Sealed Bot commands and replies (Bot v2 task 7): RFC 9180 base mode, the signed command, the
//! signed command-key publication, the reply seal and the bounded handle tables the wasm facade
//! keeps reply secrets in.

use dilla_core::bot::command::{
    CommandArg, CommandKeyPublication, CommandRefusal, CommandRequest, HPKE_AEAD, HPKE_KDF,
    HPKE_KEM, INFO_COMMAND, INFO_REPLY, MAX_COMMAND_KEY_LIFETIME_SECS, MAX_SEALED_COMMAND_BYTES,
    MAX_SEALED_REPLY_BYTES, REPLY_TABLE_CAP, ReplySecret, ReplyTable, SENT_AT_WINDOW_SECS,
    SignedCommand, VERIFIED_KEYS_CAP, VerifiedKeys, derive_key_pair, hpke_open_base,
    hpke_seal_base_derand, open_command, open_reply, seal_command, seal_command_derand, seal_reply,
    seal_reply_derand, sealed_command_key_id, verify_command,
};
use dilla_core::cbor::Encoder;
use dilla_core::identity::{DeviceEntry, DeviceList, DeviceListUnsigned, SskSigner, Tier};
use dilla_core::ids::{ChannelId, CommunityId, DeviceId, UserId};
use openmls::prelude::SignatureScheme;
use openmls_basic_credential::SignatureKeyPair;

const NOW: u64 = 1_790_000_000;
const COMMUNITY: CommunityId = CommunityId::from_bytes([0x22; 16]);

fn unhex(s: &str) -> Vec<u8> {
    hex::decode(s).expect("hex")
}

fn dsk(seed: u8) -> (SignatureKeyPair, [u8; 32]) {
    let public = SskSigner::from_bytes(&[seed; 32]).public();
    (
        SignatureKeyPair::from_raw(SignatureScheme::ED25519, vec![seed; 32], public.to_vec()),
        public,
    )
}

fn entry(device: u8, dsk_pub: [u8; 32], revoked_at: Option<u64>) -> DeviceEntry {
    DeviceEntry {
        device_id: DeviceId::from_bytes([device; 16]),
        dsk_pub,
        tier: Tier::Native,
        added_at: 100,
        revoked_at,
    }
}

fn list(user: u8, entries: Vec<DeviceEntry>) -> DeviceList {
    let unsigned = DeviceListUnsigned {
        v: 1,
        user_id: UserId::from_bytes([user; 16]),
        version: 1,
        prev_hash: [0; 32],
        entries,
    };
    let sig_ssk = SskSigner::from_bytes(&[user ^ 0x5a; 32]).sign_device_list(&unsigned);
    DeviceList { unsigned, sig_ssk }
}

struct World {
    bot_list: DeviceList,
    command_sk: [u8; 32],
    publication: CommandKeyPublication,
    user_list: DeviceList,
    user_dsk: SignatureKeyPair,
    other_dsk: SignatureKeyPair,
}

const BOT_USER: u8 = 0xb0;
const BOT_DEVICE: u8 = 0xb1;
const USER: u8 = 0x10;
const DEVICE: u8 = 0x11;
const OTHER_DEVICE: u8 = 0x12;
const REVOKED_DEVICE: u8 = 0x13;

fn world() -> World {
    let (bot_dsk, bot_pub) = dsk(0x71);
    let bot_list = list(BOT_USER, vec![entry(BOT_DEVICE, bot_pub, None)]);
    let (command_sk, command_pk) = derive_key_pair(&[0x42; 32]).expect("derive");
    let publication = CommandKeyPublication::sign(
        CommunityId::from_bytes([0x22; 16]),
        [0x33; 16],
        command_pk,
        NOW + 3600,
        DeviceId::from_bytes([BOT_DEVICE; 16]),
        &bot_dsk,
    )
    .expect("sign publication");
    let (user_dsk, user_pub) = dsk(0x81);
    let (other_dsk, other_pub) = dsk(0x82);
    let (_, revoked_pub) = dsk(0x83);
    let user_list = list(
        USER,
        vec![
            entry(DEVICE, user_pub, None),
            entry(OTHER_DEVICE, other_pub, None),
            entry(REVOKED_DEVICE, revoked_pub, Some(200)),
        ],
    );
    World {
        bot_list,
        command_sk: *command_sk,
        publication,
        user_list,
        user_dsk,
        other_dsk,
    }
}

fn request(sent_at: u64) -> CommandRequest {
    CommandRequest {
        request_id: [0x44; 16],
        community_id: CommunityId::from_bytes([0x22; 16]),
        channel_id: Some(ChannelId::from_bytes([0x55; 16])),
        operation: 11,
        args: vec![
            CommandArg::Text("never gonna give you up".into()),
            CommandArg::Uint(3),
            CommandArg::Bytes(vec![1, 2, 3]),
            CommandArg::Null,
        ],
        sent_at,
        reply_key: [0; 32],
    }
}

fn user() -> UserId {
    UserId::from_bytes([USER; 16])
}
fn device(d: u8) -> DeviceId {
    DeviceId::from_bytes([d; 16])
}

#[test]
fn constants_are_the_ones_the_plan_names() {
    assert_eq!((HPKE_KEM, HPKE_KDF, HPKE_AEAD), (0x0020, 0x0001, 0x0001));
    assert_eq!(INFO_COMMAND, b"dilla bot command v1");
    assert_eq!(INFO_REPLY, b"dilla bot reply v1");
    assert_eq!(MAX_SEALED_COMMAND_BYTES, 16384);
    assert_eq!(MAX_SEALED_REPLY_BYTES, 65536);
    assert_eq!(SENT_AT_WINDOW_SECS, 120);
}

/// RFC 9180 appendix A.1.1: DHKEM(X25519, HKDF-SHA256), HKDF-SHA256, AES-128-GCM, base mode,
/// sequence number 0.
#[test]
fn hpke_matches_rfc9180_a1() {
    let info = unhex("4f6465206f6e2061204772656369616e2055726e");
    let ikm_e = unhex("7268600d403fce431561aef583ee1613527cff655c1343f29812e66706df3234");
    let ikm_r = unhex("6db9df30aa07dd42ee5e8181afdb977e538f5e1fec8a06223f33f7013e525037");
    let (sk_e, pk_e) = derive_key_pair(&ikm_e.clone().try_into().unwrap()).unwrap();
    let (sk_r, pk_r) = derive_key_pair(&ikm_r.try_into().unwrap()).unwrap();
    assert_eq!(
        hex::encode(*sk_e),
        "52c4a758a802cd8b936eceea314432798d5baf2d7e9235dc084ab1b9cfa2f736"
    );
    assert_eq!(
        hex::encode(pk_e),
        "37fda3567bdbd628e88668c3c8d7e97d1d1253b6d4ea6d44c150f741f1bf4431"
    );
    assert_eq!(
        hex::encode(*sk_r),
        "4612c550263fc8ad58375df3f557aac531d26850903e55a9f23f21d8534e8ac8"
    );
    assert_eq!(
        hex::encode(pk_r),
        "3948cfe0ad1ddb695d780e59077195da6c56506b027329794ab02bca80815c4d"
    );
    let pt = unhex("4265617574792069732074727574682c20747275746820626561757479");
    let aad = unhex("436f756e742d30");
    let (enc, ct) =
        hpke_seal_base_derand(&pk_r, &info, &aad, &pt, &ikm_e.try_into().unwrap()).unwrap();
    assert_eq!(
        hex::encode(enc),
        "37fda3567bdbd628e88668c3c8d7e97d1d1253b6d4ea6d44c150f741f1bf4431"
    );
    assert_eq!(
        hex::encode(&ct),
        "f938558b5d72f1a23810b4be2ab4f84331acc02fc97babc53a52ae8218a355a96d8770ac83d07bea87e13c512a"
    );
    assert_eq!(hpke_open_base(&sk_r, &enc, &info, &aad, &ct).unwrap(), pt);
    let mut flipped = ct.clone();
    flipped[0] ^= 1;
    assert!(hpke_open_base(&sk_r, &enc, &info, &aad, &flipped).is_err());
}

#[test]
fn seal_then_open_then_verify_round_trips() {
    let w = world();
    assert_eq!(w.publication.verify(&w.bot_list, NOW), Ok(()));
    let decoded = CommandKeyPublication::decode(&w.publication.encode()).unwrap();
    assert_eq!(decoded, w.publication);

    let req = request(NOW);
    let (sealed, secret) =
        seal_command(&w.publication, &req, user(), device(DEVICE), &w.user_dsk).unwrap();
    assert!(sealed.len() <= MAX_SEALED_COMMAND_BYTES);
    assert_eq!(sealed_command_key_id(&sealed), Ok([0x33; 16]));

    let cmd = open_command(&w.command_sk, COMMUNITY, &sealed).unwrap();
    let opened = verify_command(&cmd, &w.user_list, user(), device(DEVICE), NOW).unwrap();
    // The reply key is made inside seal_command; the caller's value is replaced.
    assert_eq!(opened.reply_key, secret.public_key());
    assert_ne!(opened.reply_key, [0; 32]);
    let mut expected = req.clone();
    expected.reply_key = secret.public_key();
    assert_eq!(opened, expected);

    // The answer streams back sealed to the request's own one-time key.
    for (seq, body) in [(0u32, &b"Searching..."[..]), (1, b"Now playing")] {
        let reply = seal_reply(&opened.reply_key, opened.request_id, seq, body).unwrap();
        assert_eq!(
            open_reply(&secret, opened.request_id, seq, &reply).unwrap(),
            body
        );
    }

    // A command sealed to one community's key cannot name another community.
    let mut elsewhere = request(NOW);
    elsewhere.community_id = CommunityId::from_bytes([0x23; 16]);
    assert!(
        seal_command(
            &w.publication,
            &elsewhere,
            user(),
            device(DEVICE),
            &w.user_dsk
        )
        .is_err()
    );
}

#[test]
fn deterministic_seal_is_reproducible_and_opens() {
    let w = world();
    let req = request(NOW);
    let (a, sa) = seal_command_derand(
        &w.publication,
        &req,
        user(),
        device(DEVICE),
        &w.user_dsk,
        &[0x01; 32],
        &[0x02; 32],
    )
    .unwrap();
    let (b, sb) = seal_command_derand(
        &w.publication,
        &req,
        user(),
        device(DEVICE),
        &w.user_dsk,
        &[0x01; 32],
        &[0x02; 32],
    )
    .unwrap();
    assert_eq!(a, b);
    assert_eq!(sa.public_key(), sb.public_key());
    assert_eq!(
        sa.public_key(),
        ReplySecret::derive(&[0x02; 32]).unwrap().public_key()
    );
    let cmd = open_command(&w.command_sk, COMMUNITY, &a).unwrap();
    assert!(verify_command(&cmd, &w.user_list, user(), device(DEVICE), NOW).is_ok());
    let r1 = seal_reply_derand(&sa.public_key(), [0x44; 16], 7, b"ok", &[0x03; 32]).unwrap();
    let r2 = seal_reply_derand(&sa.public_key(), [0x44; 16], 7, b"ok", &[0x03; 32]).unwrap();
    assert_eq!(r1, r2);
    assert_eq!(open_reply(&sb, [0x44; 16], 7, &r1).unwrap(), b"ok");
}

#[test]
fn a_wrong_device_signature_is_refused() {
    let w = world();
    // Claims DEVICE, but is signed with another device's key.
    let (sealed, _) = seal_command(
        &w.publication,
        &request(NOW),
        user(),
        device(DEVICE),
        &w.other_dsk,
    )
    .unwrap();
    let cmd = open_command(&w.command_sk, COMMUNITY, &sealed).unwrap();
    assert_eq!(
        verify_command(&cmd, &w.user_list, user(), device(DEVICE), NOW),
        Err(CommandRefusal::BadSignature)
    );
}

#[test]
fn a_signature_by_another_listed_device_than_the_attached_one_is_refused() {
    let w = world();
    let (sealed, _) = seal_command(
        &w.publication,
        &request(NOW),
        user(),
        device(OTHER_DEVICE),
        &w.other_dsk,
    )
    .unwrap();
    let cmd = open_command(&w.command_sk, COMMUNITY, &sealed).unwrap();
    // Valid for OTHER_DEVICE ...
    assert!(verify_command(&cmd, &w.user_list, user(), device(OTHER_DEVICE), NOW).is_ok());
    // ... but dillad attached DEVICE.
    assert_eq!(
        verify_command(&cmd, &w.user_list, user(), device(DEVICE), NOW),
        Err(CommandRefusal::WrongDevice)
    );
    // ... or another user.
    assert_eq!(
        verify_command(
            &cmd,
            &w.user_list,
            UserId::from_bytes([0x19; 16]),
            device(OTHER_DEVICE),
            NOW
        ),
        Err(CommandRefusal::WrongDevice)
    );
}

#[test]
fn a_revoked_device_is_refused() {
    let w = world();
    let (revoked_dsk, _) = dsk(0x83);
    let (sealed, _) = seal_command(
        &w.publication,
        &request(NOW),
        user(),
        device(REVOKED_DEVICE),
        &revoked_dsk,
    )
    .unwrap();
    let cmd = open_command(&w.command_sk, COMMUNITY, &sealed).unwrap();
    assert_eq!(
        verify_command(&cmd, &w.user_list, user(), device(REVOKED_DEVICE), NOW),
        Err(CommandRefusal::DeviceRevoked)
    );
    // A device the list never named.
    let (stranger, _) = dsk(0x84);
    let (sealed, _) = seal_command(
        &w.publication,
        &request(NOW),
        user(),
        device(0x14),
        &stranger,
    )
    .unwrap();
    let cmd = open_command(&w.command_sk, COMMUNITY, &sealed).unwrap();
    assert_eq!(
        verify_command(&cmd, &w.user_list, user(), device(0x14), NOW),
        Err(CommandRefusal::DeviceUnlisted)
    );
    // A list of another user vouches for nobody here.
    assert_eq!(
        verify_command(&cmd, &w.bot_list, user(), device(0x14), NOW),
        Err(CommandRefusal::DeviceUnlisted)
    );
}

#[test]
fn sent_at_older_or_newer_than_120s_is_refused() {
    let w = world();
    let check = |sent_at: u64| {
        let (sealed, _) = seal_command(
            &w.publication,
            &request(sent_at),
            user(),
            device(DEVICE),
            &w.user_dsk,
        )
        .unwrap();
        let cmd = open_command(&w.command_sk, COMMUNITY, &sealed).unwrap();
        verify_command(&cmd, &w.user_list, user(), device(DEVICE), NOW).map(|_| ())
    };
    assert_eq!(check(NOW - 119), Ok(()));
    assert_eq!(check(NOW + 119), Ok(()));
    assert_eq!(check(NOW - 120), Ok(()));
    assert_eq!(check(NOW + 120), Ok(()));
    assert_eq!(check(NOW - 121), Err(CommandRefusal::Stale));
    assert_eq!(check(NOW + 121), Err(CommandRefusal::Stale));
    assert_eq!(check(0), Err(CommandRefusal::Stale));
    assert_eq!(check(u64::MAX), Err(CommandRefusal::Stale));
}

#[test]
fn a_reply_cannot_be_opened_with_another_requests_secret() {
    let w = world();
    let (s1, secret1) = seal_command(
        &w.publication,
        &request(NOW),
        user(),
        device(DEVICE),
        &w.user_dsk,
    )
    .unwrap();
    let (_, secret2) = seal_command(
        &w.publication,
        &request(NOW),
        user(),
        device(DEVICE),
        &w.user_dsk,
    )
    .unwrap();
    let req = verify_command(
        &open_command(&w.command_sk, COMMUNITY, &s1).unwrap(),
        &w.user_list,
        user(),
        device(DEVICE),
        NOW,
    )
    .unwrap();
    let reply = seal_reply(&req.reply_key, req.request_id, 0, b"Saved").unwrap();
    assert!(open_reply(&secret2, req.request_id, 0, &reply).is_err());
    // Wrong seq.
    assert!(open_reply(&secret1, req.request_id, 1, &reply).is_err());
    // Wrong request id.
    assert!(open_reply(&secret1, [0x45; 16], 0, &reply).is_err());
    // Right everything.
    assert_eq!(
        open_reply(&secret1, req.request_id, 0, &reply).unwrap(),
        b"Saved"
    );
    // Strict outer: trailing bytes, another version, a short enc.
    let mut trailing = reply.clone();
    trailing.push(0);
    assert!(open_reply(&secret1, req.request_id, 0, &trailing).is_err());
    let mut v2 = reply.clone();
    assert_eq!(v2[1], 0x01);
    v2[1] = 0x02;
    assert!(open_reply(&secret1, req.request_id, 0, &v2).is_err());
    // An oversize reply is refused on both sides.
    let big = vec![0u8; MAX_SEALED_REPLY_BYTES];
    assert!(seal_reply(&req.reply_key, req.request_id, 0, &big).is_err());
    assert!(
        open_reply(
            &secret1,
            req.request_id,
            0,
            &vec![0x83; MAX_SEALED_REPLY_BYTES + 1]
        )
        .is_err()
    );
}

#[test]
fn a_publication_signed_by_a_non_bot_device_fails_verify() {
    let w = world();
    // Signed by a human device and naming it: not a device of the Bot user.
    let (_, pk) = derive_key_pair(&[0x43; 32]).unwrap();
    let forged = CommandKeyPublication::sign(
        CommunityId::from_bytes([0x22; 16]),
        [0x34; 16],
        pk,
        NOW + 3600,
        device(DEVICE),
        &w.user_dsk,
    )
    .unwrap();
    assert_eq!(
        forged.verify(&w.bot_list, NOW),
        Err(CommandRefusal::DeviceUnlisted)
    );
    // Naming the Bot's device but signed with another key.
    let forged = CommandKeyPublication::sign(
        CommunityId::from_bytes([0x22; 16]),
        [0x34; 16],
        pk,
        NOW + 3600,
        DeviceId::from_bytes([BOT_DEVICE; 16]),
        &w.user_dsk,
    )
    .unwrap();
    assert_eq!(
        forged.verify(&w.bot_list, NOW),
        Err(CommandRefusal::BadSignature)
    );
    // A field changed after signing.
    let mut moved = w.publication.clone();
    moved.not_after += 1;
    assert_eq!(
        moved.verify(&w.bot_list, NOW),
        Err(CommandRefusal::BadSignature)
    );
    // A revoked Bot device.
    let (_, bot_pub) = dsk(0x71);
    let revoked = list(BOT_USER, vec![entry(BOT_DEVICE, bot_pub, Some(300))]);
    assert_eq!(
        w.publication.verify(&revoked, NOW),
        Err(CommandRefusal::DeviceRevoked)
    );
    // An expired not_after; the last second is still valid.
    assert_eq!(w.publication.verify(&w.bot_list, NOW + 3600), Ok(()));
    assert_eq!(
        w.publication.verify(&w.bot_list, NOW + 3601),
        Err(CommandRefusal::Stale)
    );
}

#[test]
fn publication_decode_is_strict() {
    let w = world();
    let good = w.publication.encode();
    assert_eq!(good[0], 0x87, "a 7-element array");
    let mut trailing = good.clone();
    trailing.push(0xf6);
    assert!(CommandKeyPublication::decode(&trailing).is_err());
    let mut v2 = good.clone();
    v2[1] = 0x02;
    assert!(CommandKeyPublication::decode(&v2).is_err());
    let mut extra = good.clone();
    extra[0] = 0x88;
    extra.push(0xf6);
    assert!(CommandKeyPublication::decode(&extra).is_err());
    assert!(CommandKeyPublication::decode(&good[..good.len() - 1]).is_err());
    // An all-zero command key is not a key.
    let mut zero = w.publication.clone();
    zero.public_key = [0; 32];
    assert!(CommandKeyPublication::decode(&zero.encode()).is_err());
}

#[test]
fn oversize_sealed_command_is_malformed() {
    let w = world();
    let oversize = vec![0x84; MAX_SEALED_COMMAND_BYTES + 1];
    assert_eq!(
        open_command(&w.command_sk, COMMUNITY, &oversize),
        Err(CommandRefusal::Malformed)
    );
    assert_eq!(
        sealed_command_key_id(&oversize),
        Err(CommandRefusal::Malformed)
    );
    // The sealing side refuses to make one.
    let mut req = request(NOW);
    req.args = vec![CommandArg::Bytes(vec![7; MAX_SEALED_COMMAND_BYTES])];
    assert!(seal_command(&w.publication, &req, user(), device(DEVICE), &w.user_dsk).is_err());
}

#[test]
fn sealed_command_outer_is_strict() {
    let w = world();
    let (sealed, _) = seal_command(
        &w.publication,
        &request(NOW),
        user(),
        device(DEVICE),
        &w.user_dsk,
    )
    .unwrap();
    assert_eq!(sealed[0], 0x85, "[1, key_id, request_id, enc, ciphertext]");
    assert_eq!(
        open_command(&w.command_sk, COMMUNITY, &sealed)
            .unwrap()
            .encode()[0],
        0x8c,
        "[1, request_id, community_id, channel_id, operation, args, sent_at, reply_key, key_id, \
         user_id, device_id, sig]"
    );
    let mut trailing = sealed.clone();
    trailing.push(0);
    assert_eq!(
        open_command(&w.command_sk, COMMUNITY, &trailing),
        Err(CommandRefusal::Malformed)
    );
    let mut v2 = sealed.clone();
    v2[1] = 0x02;
    assert_eq!(
        open_command(&w.command_sk, COMMUNITY, &v2),
        Err(CommandRefusal::Malformed)
    );
    // The outer request id is the AEAD's associated data: moving it breaks the seal.
    let mut moved = sealed.clone();
    let at = 1 + 1 + 1 + 16 + 1; // version, key_id head + 16, request_id head
    assert_eq!(&moved[at..at + 16], &[0x44; 16]);
    moved[at] ^= 1;
    assert_eq!(
        open_command(&w.command_sk, COMMUNITY, &moved),
        Err(CommandRefusal::Malformed)
    );
    // Another key cannot open it.
    let (other_sk, _) = derive_key_pair(&[0x99; 32]).unwrap();
    assert_eq!(
        open_command(&other_sk, COMMUNITY, &sealed),
        Err(CommandRefusal::Malformed)
    );
}

#[test]
fn reply_table_is_bounded_and_forgets() {
    let mut table = ReplyTable::default();
    let first = table.insert([0x61; 16], ReplySecret::derive(&[1; 32]).unwrap());
    let pk1 = ReplySecret::derive(&[1; 32]).unwrap().public_key();
    assert_eq!(
        table.get(first).map(|(r, s)| (r, s.public_key())),
        Some(([0x61; 16], pk1))
    );
    assert!(table.forget(first));
    assert!(table.get(first).is_none());
    assert!(!table.forget(first));

    let mut handles = Vec::new();
    for i in 0..(REPLY_TABLE_CAP + 3) {
        handles.push(table.insert(
            [0x62; 16],
            ReplySecret::derive(&[(i % 250) as u8 + 2; 32]).unwrap(),
        ));
    }
    assert_eq!(table.len(), REPLY_TABLE_CAP);
    // The three oldest were evicted; the newest are present; handles are never reused.
    for h in &handles[..3] {
        assert!(table.get(*h).is_none());
    }
    for h in &handles[3..] {
        assert!(table.get(*h).is_some());
    }
    assert!(!handles.contains(&first));
    assert!(!handles.contains(&0));
}

#[test]
fn verified_keys_admit_only_the_exact_verified_unexpired_publication() {
    let w = world();
    let mut keys = VerifiedKeys::default();
    let bytes = w.publication.encode();
    assert!(keys.check(&bytes, NOW).is_err(), "never verified");
    keys.remember(&w.publication, UserId::from_bytes([BOT_USER; 16]));
    assert_eq!(
        keys.check(&bytes, NOW).unwrap(),
        (w.publication.clone(), UserId::from_bytes([BOT_USER; 16]))
    );
    assert!(keys.check(&bytes, NOW + 3601).is_err(), "expired");
    let mut other = w.publication.clone();
    other.not_after += 1;
    assert!(keys.check(&other.encode(), NOW).is_err(), "other bytes");
    for i in 0..(VERIFIED_KEYS_CAP + 1) {
        let mut p = w.publication.clone();
        let mut c = [0u8; 16];
        c[..8].copy_from_slice(&(i as u64 + 1).to_be_bytes());
        p.community_id = CommunityId::from_bytes(c);
        keys.remember(&p, UserId::from_bytes([BOT_USER; 16]));
    }
    assert_eq!(keys.len(), VERIFIED_KEYS_CAP);
    assert!(keys.check(&bytes, NOW).is_err(), "the oldest was evicted");
}

fn sign_raw(dsk: &SignatureKeyPair, msg: &[u8]) -> [u8; 64] {
    use openmls_traits::signatures::Signer;
    dsk.sign(msg).unwrap().try_into().unwrap()
}

/// Seals an arbitrary signed command under an arbitrary outer key_id, as a modified client could.
fn seal_raw(w: &World, cmd: &SignedCommand, outer_key_id: [u8; 16]) -> Vec<u8> {
    let (enc, ct) = hpke_seal_base_derand(
        &w.publication.public_key,
        INFO_COMMAND,
        &cmd.request.request_id,
        &cmd.encode(),
        &[0x05; 32],
    )
    .unwrap();
    let mut e = Encoder::new();
    e.array(5)
        .uint(1)
        .bytes(&outer_key_id)
        .bytes(&cmd.request.request_id)
        .bytes(&enc)
        .bytes(&ct);
    e.into_vec()
}

fn signed(w: &World, community: CommunityId, key_id: [u8; 16]) -> SignedCommand {
    let mut req = request(NOW);
    req.community_id = community;
    req.reply_key = [0x09; 32];
    let mut cmd = SignedCommand {
        request: req,
        key_id,
        user_id: user(),
        device_id: device(DEVICE),
        sig: [0; 64],
    };
    cmd.sig = sign_raw(&w.user_dsk, &cmd.signing_message());
    cmd
}

#[test]
fn the_signed_key_id_must_be_the_outer_key_id() {
    let w = world();
    // Honest: the signed key id is the publication's.
    let ok = seal_raw(&w, &signed(&w, COMMUNITY, [0x33; 16]), [0x33; 16]);
    let cmd = open_command(&w.command_sk, COMMUNITY, &ok).unwrap();
    assert_eq!(cmd.key_id, [0x33; 16]);
    assert!(verify_command(&cmd, &w.user_list, user(), device(DEVICE), NOW).is_ok());
    // A command signed for another key (relayed by a masquerading "Bot") is refused.
    let relayed = seal_raw(&w, &signed(&w, COMMUNITY, [0x34; 16]), [0x33; 16]);
    assert_eq!(
        open_command(&w.command_sk, COMMUNITY, &relayed),
        Err(CommandRefusal::Malformed)
    );
    // The key id is inside the signature: changing it breaks the signature.
    let mut moved = signed(&w, COMMUNITY, [0x33; 16]);
    moved.key_id = [0x35; 16];
    let sealed = seal_raw(&w, &moved, [0x35; 16]);
    let cmd = open_command(&w.command_sk, COMMUNITY, &sealed).unwrap();
    assert_eq!(
        verify_command(&cmd, &w.user_list, user(), device(DEVICE), NOW),
        Err(CommandRefusal::BadSignature)
    );
}

#[test]
fn a_command_for_another_community_than_the_keys_is_malformed() {
    let w = world();
    let other = CommunityId::from_bytes([0x23; 16]);
    let sealed = seal_raw(&w, &signed(&w, other, [0x33; 16]), [0x33; 16]);
    // Opened with community 0x22's key: refused.
    assert_eq!(
        open_command(&w.command_sk, COMMUNITY, &sealed),
        Err(CommandRefusal::Malformed)
    );
    // The same bytes under a key published for community 0x23 open.
    assert!(open_command(&w.command_sk, other, &sealed).is_ok());
}

#[test]
fn a_command_key_valid_for_longer_than_one_rotation_fails_verify() {
    let (bot_dsk, bot_pub) = dsk(0x71);
    let bot_list = list(BOT_USER, vec![entry(BOT_DEVICE, bot_pub, None)]);
    let sign = |not_after| {
        CommandKeyPublication::sign(
            COMMUNITY,
            [0x33; 16],
            [0x44; 32],
            not_after,
            DeviceId::from_bytes([BOT_DEVICE; 16]),
            &bot_dsk,
        )
        .unwrap()
    };
    assert_eq!(MAX_COMMAND_KEY_LIFETIME_SECS, 8 * 24 * 3600);
    assert_eq!(
        sign(NOW + MAX_COMMAND_KEY_LIFETIME_SECS).verify(&bot_list, NOW),
        Ok(())
    );
    assert_eq!(
        sign(NOW + MAX_COMMAND_KEY_LIFETIME_SECS + 1).verify(&bot_list, NOW),
        Err(CommandRefusal::Malformed)
    );
    assert_eq!(
        sign(u64::MAX).verify(&bot_list, NOW),
        Err(CommandRefusal::Malformed)
    );
}

#[test]
fn a_reply_table_entry_keeps_its_request_id() {
    let mut table = ReplyTable::default();
    let h = table.insert([0x44; 16], ReplySecret::derive(&[9; 32]).unwrap());
    let (request_id, secret) = table.get(h).unwrap();
    assert_eq!(request_id, [0x44; 16]);
    let reply = seal_reply(&secret.public_key(), request_id, 0, b"ok").unwrap();
    assert_eq!(open_reply(secret, request_id, 0, &reply).unwrap(), b"ok");
}
