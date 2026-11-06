//! The client half of sealed Bot commands (Bot v2 task 7): verify the Bot's command-key
//! publication against the Bot user's stored device list, and seal a command as this device.
//!
//! The reply secret and the verified-publication set are not stored: the binding layer keeps them
//! in memory (`bot::command::{ReplyTable, VerifiedKeys}`) and they die with it.

use super::error::{E_CORE_INPUT, E_CREDENTIAL, E_DEVICE_LIST_STALE};
use super::{ClientCore, ClientError, account_kind, trust};
use crate::bot::command::{
    CommandArg, CommandKeyPublication, CommandRefusal, CommandRequest, MAX_COMMAND_ARGS,
    ReplySecret, seal_command,
};
use crate::cbor::{CborError, Decoder, Encoder};
use crate::identity::Kind;
use crate::identity::fingerprint;
use crate::ids::{ChannelId, CommunityId, DeviceId, UserId};
use crate::mls::StorageError;

/// The largest unsealed request a binding hands in; the sealed form is bounded at 16384.
const MAX_REQUEST_INPUT: usize = 16384;

pub(crate) fn refusal_code(r: CommandRefusal) -> &'static str {
    match r {
        CommandRefusal::Malformed => "E_BOT_MALFORMED",
        CommandRefusal::UnknownKey => "E_BOT_KEY_STALE",
        CommandRefusal::BadSignature => "E_BOT_SIGNATURE",
        CommandRefusal::WrongDevice => "E_BOT_WRONG_DEVICE",
        CommandRefusal::DeviceUnlisted => "E_DEVICE_UNLISTED",
        CommandRefusal::DeviceRevoked => "E_DEVICE_REVOKED",
        CommandRefusal::Stale => "E_BOT_STALE",
    }
}

fn input(detail: &str) -> ClientError {
    ClientError::new(E_CORE_INPUT, detail)
}

fn read_input_arg(d: &mut Decoder<'_>) -> Result<CommandArg, CborError> {
    let raw = d.skip()?;
    let one = |f: &dyn Fn(&mut Decoder<'_>) -> Result<CommandArg, CborError>| {
        let mut x = Decoder::new(raw);
        let v = f(&mut x)?;
        x.finish()?;
        Ok(v)
    };
    match raw.first().map(|b| b >> 5) {
        Some(0) => one(&|x| Ok(CommandArg::Uint(x.uint()?))),
        Some(2) => one(&|x| Ok(CommandArg::Bytes(x.bytes()?.to_vec()))),
        Some(3) => one(&|x| Ok(CommandArg::Text(x.text()?.to_owned()))),
        Some(7) if raw == [0xf6] => Ok(CommandArg::Null),
        _ => Err(CborError::TypeMismatch {
            expected: "command argument",
            offset: 0,
        }),
    }
}

/// The binding's request: CBOR `[request_id(16), community_id(16), channel_id(16)|null,
/// operation, args, sent_at]`. `reply_key` is made by the core.
pub fn decode_command_input(bytes: &[u8]) -> Result<CommandRequest, ClientError> {
    if bytes.len() > MAX_REQUEST_INPUT {
        return Err(ClientError::new(
            super::error::E_CORE_LIMIT,
            "request: over 16384 bytes",
        ));
    }
    let mut d = Decoder::new(bytes);
    let parse = |d: &mut Decoder<'_>| -> Result<CommandRequest, CborError> {
        d.array(6)?;
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
            args.push(read_input_arg(d)?);
        }
        let sent_at = d.uint()?;
        Ok(CommandRequest {
            request_id,
            community_id,
            channel_id,
            operation,
            args,
            sent_at,
            // Replaced by seal_command; nonzero so the shape check does not refuse it first.
            reply_key: [0xff; 32],
        })
    };
    let req = parse(&mut d).map_err(|_| input("request: malformed"))?;
    d.finish().map_err(|_| input("request: trailing bytes"))?;
    Ok(req)
}

/// The whole check of a command-key publication against this store, at `now`:
///
/// 1. `bot_user_id` must be attested by the pinned instance as a Bot account (`attested_kind`):
///    an attested human is `E_CREDENTIAL`, no verifying attestation is `E_DEVICE_LIST_STALE`.
///    There is no transition window, so a server cannot name an ordinary member as the Bot.
/// 2. Its cached device list must vouch for it (`trust::trusted_list`).
/// 3. The publication must be signed by a listed, unrevoked device of that list, and be valid at
///    `now` for no longer than `MAX_COMMAND_KEY_LIFETIME_SECS` (`CommandKeyPublication::verify`).
///
/// Both the verify export and every seal run it, so a revocation takes effect at the next seal.
pub(super) fn verify_publication(
    c: &rusqlite::Connection,
    publication: &[u8],
    bot_user_id: &[u8; 16],
    now: u64,
) -> Result<Result<CommandKeyPublication, &'static str>, StorageError> {
    let Ok(p) = CommandKeyPublication::decode(publication) else {
        return Ok(Err("E_BOT_MALFORMED"));
    };
    match account_kind::attested_kind(c, bot_user_id)? {
        Some(k) if k == Kind::Bot.as_u8() => {}
        Some(_) => return Ok(Err(E_CREDENTIAL)),
        None => return Ok(Err(E_DEVICE_LIST_STALE)),
    }
    let list = match trust::trusted_list(c, bot_user_id)? {
        Ok(list) => list,
        Err(code) => return Ok(Err(code)),
    };
    if list.unsigned.user_id != UserId::from_bytes(*bot_user_id) {
        return Ok(Err(E_DEVICE_LIST_STALE));
    }
    Ok(p.verify(&list, now).map(|()| p).map_err(refusal_code))
}

impl ClientCore {
    /// The Bot side (Bot v2 task 8): signs the publication of its command key for `community_id`
    /// with this device's DSK, naming this device. Returns the encoded publication. The private
    /// half never reaches the core; the Bot helper keeps it.
    pub fn bot_command_key_publish(
        &self,
        community_id: &[u8; 16],
        key_id: [u8; 16],
        public_key: [u8; 32],
        not_after: u64,
    ) -> Result<Vec<u8>, ClientError> {
        let own = self.own()?;
        let p = CommandKeyPublication::sign(
            CommunityId::from_bytes(*community_id),
            key_id,
            public_key,
            not_after,
            DeviceId::from_bytes(own.device_id),
            self.signer()?,
        )
        .map_err(|e| ClientError::new(E_CORE_INPUT, format!("command key: {e}")))?;
        Ok(p.encode())
    }

    /// Verifies a command-key publication for `bot_user_id` at `now` (`verify_publication`).
    /// Returns the publication and CBOR `[key_id, fingerprint]`, the fingerprint being 12
    /// Crockford characters of SHA-256(command key).
    pub fn bot_command_key_verify(
        &self,
        publication: &[u8],
        bot_user_id: &[u8; 16],
        now: u64,
    ) -> Result<(CommandKeyPublication, Vec<u8>), ClientError> {
        let p = self
            .read(|c| verify_publication(c, publication, bot_user_id, now))?
            .map_err(|code| ClientError::new(code, "command key publication"))?;
        let mut e = Encoder::with_capacity(40);
        e.array(2)
            .bytes(&p.key_id)
            .text(&fingerprint(&p.public_key));
        Ok((p, e.into_vec()))
    }

    /// Re-verifies the publication for `bot_user_id` at `now` against the store as it is now
    /// (so a Bot device revoked since the last verify is refused at once), then signs and seals
    /// a command as this device. Returns the request id, the sealed bytes and the reply secret;
    /// the caller keeps the secret in memory.
    pub fn bot_command_seal(
        &self,
        publication: &[u8],
        bot_user_id: &[u8; 16],
        request: &[u8],
        now: u64,
    ) -> Result<([u8; 16], Vec<u8>, ReplySecret), ClientError> {
        let p = self
            .read(|c| verify_publication(c, publication, bot_user_id, now))?
            .map_err(|code| ClientError::new(code, "command key publication"))?;
        let req = decode_command_input(request)?;
        let own = self.own()?;
        let signer = self.signer()?;
        let (sealed, secret) = seal_command(
            &p,
            &req,
            UserId::from_bytes(own.user_id),
            DeviceId::from_bytes(own.device_id),
            signer,
        )
        .map_err(|e| ClientError::new(E_CORE_INPUT, format!("seal: {e}")))?;
        Ok((req.request_id, sealed, secret))
    }
}

#[cfg(test)]
mod tests {
    use super::super::error::E_UMK_CHANGED;
    use super::*;
    use crate::identity::{DeviceEntry, DeviceList, DeviceListUnsigned, SskSigner, Tier};
    use rusqlite::params;

    const BOT: [u8; 16] = [0xb0; 16];
    type PinRow = ([u8; 32], Option<[u8; 32]>);

    fn list(user: [u8; 16], ssk: &SskSigner) -> DeviceList {
        let unsigned = DeviceListUnsigned {
            v: 1,
            user_id: UserId::from_bytes(user),
            version: 1,
            prev_hash: [0; 32],
            entries: vec![DeviceEntry {
                device_id: DeviceId::from_bytes([0xb1; 16]),
                dsk_pub: [0x01; 32],
                tier: Tier::Native,
                added_at: 1,
                revoked_at: None,
            }],
        };
        let sig_ssk = ssk.sign_device_list(&unsigned);
        DeviceList { unsigned, sig_ssk }
    }

    fn db(list: Option<&DeviceList>, ssk: [u8; 32], pin: Option<PinRow>) -> rusqlite::Connection {
        let db = rusqlite::Connection::open_in_memory().expect("db");
        db.execute_batch(
            "CREATE TABLE app_trust_pins(user_id BLOB PRIMARY KEY,umk_pub BLOB,first_seen INTEGER,verified INTEGER,changed_umk BLOB);
             CREATE TABLE app_device_lists(user_id BLOB PRIMARY KEY,version INTEGER,list BLOB,umk_pub BLOB,ssk_pub BLOB,credential BLOB);",
        )
        .expect("tables");
        if let Some(l) = list {
            db.execute(
                "INSERT INTO app_device_lists(user_id,version,list,umk_pub,ssk_pub,credential) VALUES(?1,1,?2,?3,?4,x'')",
                params![BOT.as_slice(), l.encode(), [0x0au8; 32].as_slice(), ssk.as_slice()],
            )
            .expect("list row");
        }
        if let Some((umk, changed)) = pin {
            db.execute(
                "INSERT INTO app_trust_pins(user_id,umk_pub,first_seen,verified,changed_umk) VALUES(?1,?2,1,0,?3)",
                params![BOT.as_slice(), umk.as_slice(), changed.as_ref().map(|k| k.as_slice())],
            )
            .expect("pin row");
        }
        db
    }

    #[test]
    fn the_bot_list_vouches_only_when_cached_pinned_unchanged_and_signed() {
        let ssk = SskSigner::from_bytes(&[0x5a; 32]);
        let good = list(BOT, &ssk);
        let pinned = Some(([0x0a; 32], None));
        let verdict = |c: rusqlite::Connection| trust::trusted_list(&c, &BOT).expect("query");
        assert_eq!(
            verdict(db(Some(&good), ssk.public(), pinned)),
            Ok(good.clone())
        );
        assert_eq!(
            verdict(db(None, ssk.public(), pinned)),
            Err(E_DEVICE_LIST_STALE)
        );
        assert_eq!(
            verdict(db(Some(&good), ssk.public(), None)),
            Err(E_DEVICE_LIST_STALE)
        );
        let changing = Some(([0x0a; 32], Some([0x0b; 32])));
        assert_eq!(
            verdict(db(Some(&good), ssk.public(), changing)),
            Err(E_UMK_CHANGED)
        );
        let other_umk = Some(([0x0c; 32], None));
        assert_eq!(
            verdict(db(Some(&good), ssk.public(), other_umk)),
            Err(E_CREDENTIAL)
        );
        // Signed by another SSK than the cached one.
        let wrong_ssk = SskSigner::from_bytes(&[0x5b; 32]).public();
        assert_eq!(
            verdict(db(Some(&good), wrong_ssk, pinned)),
            Err(E_CREDENTIAL)
        );
        // Another user's list stored under the Bot's row.
        let other = list([0xc0; 16], &ssk);
        assert_eq!(
            verdict(db(Some(&other), ssk.public(), pinned)),
            Err(E_CREDENTIAL)
        );
    }

    #[test]
    fn command_input_decodes_strictly() {
        let mut e = Encoder::new();
        e.array(6).bytes(&[1; 16]).bytes(&[2; 16]).null().uint(11);
        e.array(4).text("x").uint(3).bytes(&[9]).null();
        e.uint(100);
        let ok = e.into_vec();
        let req = decode_command_input(&ok).expect("decodes");
        assert_eq!(
            req.args,
            vec![
                CommandArg::Text("x".into()),
                CommandArg::Uint(3),
                CommandArg::Bytes(vec![9]),
                CommandArg::Null
            ]
        );
        assert_eq!(
            (req.operation, req.sent_at, req.channel_id),
            (11, 100, None)
        );
        let mut trailing = ok.clone();
        trailing.push(0);
        assert!(decode_command_input(&trailing).is_err());
        // A nested array is not an argument.
        let mut e = Encoder::new();
        e.array(6).bytes(&[1; 16]).bytes(&[2; 16]).null().uint(11);
        e.array(1).array(0);
        e.uint(100);
        assert!(decode_command_input(&e.into_vec()).is_err());
        // A short id.
        let mut e = Encoder::new();
        e.array(6)
            .bytes(&[1; 15])
            .bytes(&[2; 16])
            .null()
            .uint(11)
            .array(0)
            .uint(100);
        assert!(decode_command_input(&e.into_vec()).is_err());
        assert!(decode_command_input(&vec![0x80; MAX_REQUEST_INPUT + 1]).is_err());
    }

    // ---- verify_publication: the attested kind, the trusted list and the publication ----

    use crate::bot::command::MAX_COMMAND_KEY_LIFETIME_SECS;
    use crate::identity::account_kind_message;
    use ed25519_dalek::{Signer as _, SigningKey};
    use openmls_basic_credential::SignatureKeyPair;
    use openmls_traits::types::SignatureScheme;

    const INSTANCE: [u8; 16] = [0x11; 16];
    const NOW: u64 = 1_790_000_000;
    const BOT_DEVICE: [u8; 16] = [0xb1; 16];
    const BOT_DSK_SEED: [u8; 32] = [0xb2; 32];

    fn instance_key(seed: u8) -> SigningKey {
        SigningKey::from_bytes(&[seed; 32])
    }

    fn bot_list(ssk: &SskSigner, revoked_at: Option<u64>) -> DeviceList {
        let unsigned = DeviceListUnsigned {
            v: 1,
            user_id: UserId::from_bytes(BOT),
            version: 1,
            prev_hash: [0; 32],
            entries: vec![DeviceEntry {
                device_id: DeviceId::from_bytes(BOT_DEVICE),
                dsk_pub: SskSigner::from_bytes(&BOT_DSK_SEED).public(),
                tier: Tier::Native,
                added_at: 1,
                revoked_at,
            }],
        };
        let sig_ssk = ssk.sign_device_list(&unsigned);
        DeviceList { unsigned, sig_ssk }
    }

    fn publication(not_after: u64) -> Vec<u8> {
        let dsk = SignatureKeyPair::from_raw(
            SignatureScheme::ED25519,
            BOT_DSK_SEED.to_vec(),
            SskSigner::from_bytes(&BOT_DSK_SEED).public().to_vec(),
        );
        CommandKeyPublication::sign(
            CommunityId::from_bytes([0x22; 16]),
            [0x33; 16],
            [0x44; 32],
            not_after,
            DeviceId::from_bytes(BOT_DEVICE),
            &dsk,
        )
        .expect("sign")
        .encode()
    }

    /// A store that pins `instance_key(0x41)` for `INSTANCE`, holds the Bot's trusted list, and,
    /// when `attested` names a kind and a signing key, that key's attestation of the Bot user.
    fn store(attested: Option<(u8, &SigningKey)>) -> rusqlite::Connection {
        let ssk = SskSigner::from_bytes(&[0x5a; 32]);
        let c = db(
            Some(&bot_list(&ssk, None)),
            ssk.public(),
            Some(([0x0a; 32], None)),
        );
        c.execute_batch("CREATE TABLE app_meta(k TEXT PRIMARY KEY,v BLOB);")
            .expect("app_meta");
        c.execute_batch(super::super::account_kind::ACCOUNT_KINDS_SCHEMA)
            .expect("account kinds");
        let pinned = instance_key(0x41).verifying_key().to_bytes();
        super::super::schema::meta_put(&c, super::super::schema::INSTANCE_SIGNER_KEY, &pinned)
            .expect("pin instance");
        let record = super::super::identity::IdentityRecord {
            instance_id: INSTANCE,
            user_id: [0x10; 16],
            device_id: [0x12; 16],
            dsk_pub: [1; 32],
            umk_pub: [2; 32],
            ssk_pub: [3; 32],
            username: "me".into(),
            credential: vec![],
            device_list_body: vec![],
            list_published: true,
            device_list: vec![],
            state_uploaded: false,
        };
        super::super::schema::meta_put(&c, super::super::schema::IDENTITY, &record.encode())
            .expect("identity");
        if let Some((kind, key)) = attested {
            let sig = key
                .sign(&account_kind_message(&INSTANCE, &BOT, kind))
                .to_bytes();
            c.execute(
                "INSERT INTO app_account_kinds(user_id,kind,key_id,signature,instance_key,instance_id) VALUES(?1,?2,?3,?4,?5,?6)",
                params![
                    BOT.as_slice(),
                    i64::from(kind),
                    [0x77u8; 16].as_slice(),
                    sig.as_slice(),
                    key.verifying_key().to_bytes().as_slice(),
                    INSTANCE.as_slice()
                ],
            )
            .expect("attestation");
        }
        c
    }

    fn verdict(c: &rusqlite::Connection, publication: &[u8], now: u64) -> Result<(), &'static str> {
        verify_publication(c, publication, &BOT, now)
            .expect("query")
            .map(|_| ())
    }

    #[test]
    fn a_verifying_bot_attestation_admits_the_command_key() {
        let key = instance_key(0x41);
        let c = store(Some((Kind::Bot.as_u8(), &key)));
        assert_eq!(verdict(&c, &publication(NOW + 3600), NOW), Ok(()));
    }

    #[test]
    fn an_attested_human_named_as_the_bot_is_refused() {
        let key = instance_key(0x41);
        let c = store(Some((Kind::User.as_u8(), &key)));
        assert_eq!(
            verdict(&c, &publication(NOW + 3600), NOW),
            Err(E_CREDENTIAL)
        );
    }

    #[test]
    fn a_missing_attestation_is_refused_with_no_transition_window() {
        let c = store(None);
        // Even with the "attestation seen" flag absent (the leaf rule's transition window).
        assert_eq!(
            super::super::schema::meta_get(&c, super::super::account_kind::ACCOUNT_KIND_SEEN)
                .unwrap(),
            None
        );
        assert_eq!(
            verdict(&c, &publication(NOW + 3600), NOW),
            Err(E_DEVICE_LIST_STALE)
        );
    }

    #[test]
    fn an_attestation_under_another_instance_key_is_refused() {
        // Self-consistent (it verifies under the key it names), but not the pinned key.
        let other = instance_key(0x42);
        let c = store(Some((Kind::Bot.as_u8(), &other)));
        assert_eq!(
            verdict(&c, &publication(NOW + 3600), NOW),
            Err(E_DEVICE_LIST_STALE)
        );
    }

    /// `bot_command_seal` runs `verify_publication` before every seal: a Bot device revoked after
    /// the verify is refused at the next seal, not at `not_after`.
    #[test]
    fn a_signer_revoked_after_verify_is_refused_at_the_next_seal() {
        let key = instance_key(0x41);
        let c = store(Some((Kind::Bot.as_u8(), &key)));
        let p = publication(NOW + 3600);
        assert_eq!(verdict(&c, &p, NOW), Ok(()));
        let ssk = SskSigner::from_bytes(&[0x5a; 32]);
        c.execute(
            "UPDATE app_device_lists SET list=?1 WHERE user_id=?2",
            params![bot_list(&ssk, Some(NOW - 1)).encode(), BOT.as_slice()],
        )
        .expect("revoke");
        assert_eq!(verdict(&c, &p, NOW + 1), Err("E_DEVICE_REVOKED"));
    }

    #[test]
    fn a_command_key_valid_for_longer_than_one_rotation_is_refused() {
        let key = instance_key(0x41);
        let c = store(Some((Kind::Bot.as_u8(), &key)));
        assert_eq!(MAX_COMMAND_KEY_LIFETIME_SECS, 8 * 86_400);
        assert_eq!(
            verdict(&c, &publication(NOW + MAX_COMMAND_KEY_LIFETIME_SECS), NOW),
            Ok(())
        );
        assert_eq!(
            verdict(
                &c,
                &publication(NOW + MAX_COMMAND_KEY_LIFETIME_SECS + 1),
                NOW
            ),
            Err("E_BOT_MALFORMED")
        );
        assert_eq!(
            verdict(&c, &publication(u64::MAX), NOW),
            Err("E_BOT_MALFORMED")
        );
    }
}
