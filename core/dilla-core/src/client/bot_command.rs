//! The client half of sealed Bot commands (Bot v2 task 7): verify the Bot's command-key
//! publication against the Bot user's stored device list, and seal a command as this device.
//!
//! The reply secret and the verified-publication set are not stored: the binding layer keeps them
//! in memory (`bot::command::{ReplyTable, VerifiedKeys}`) and they die with it.

use super::error::{E_CORE_INPUT, E_DEVICE_LIST_STALE};
use super::{ClientCore, ClientError, trust};
use crate::bot::command::{
    CommandArg, CommandKeyPublication, CommandRefusal, CommandRequest, MAX_COMMAND_ARGS,
    ReplySecret, seal_command,
};
use crate::cbor::{CborError, Decoder, Encoder};
use crate::identity::fingerprint;
use crate::ids::{ChannelId, CommunityId, DeviceId, UserId};

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

impl ClientCore {
    /// Verifies a command-key publication against the stored, trusted device list of
    /// `bot_user_id` at `now`. Returns the publication and CBOR `[key_id, fingerprint]`, the
    /// fingerprint being 12 Crockford characters of SHA-256(command key).
    pub fn bot_command_key_verify(
        &self,
        publication: &[u8],
        bot_user_id: &[u8; 16],
        now: u64,
    ) -> Result<(CommandKeyPublication, Vec<u8>), ClientError> {
        let p = CommandKeyPublication::decode(publication)
            .map_err(|_| ClientError::new("E_BOT_MALFORMED", "command key publication"))?;
        let list = self
            .read(|c| trust::trusted_list(c, bot_user_id))?
            .map_err(|code| ClientError::new(code, "Bot device list"))?;
        if list.unsigned.user_id != UserId::from_bytes(*bot_user_id) {
            return Err(ClientError::new(E_DEVICE_LIST_STALE, "Bot device list"));
        }
        p.verify(&list, now)
            .map_err(|r| ClientError::new(refusal_code(r), "command key publication"))?;
        let mut e = Encoder::with_capacity(40);
        e.array(2)
            .bytes(&p.key_id)
            .text(&fingerprint(&p.public_key));
        Ok((p, e.into_vec()))
    }

    /// Signs and seals a command as this device to an already verified publication. Returns the
    /// sealed bytes and the reply secret; the caller keeps the secret in memory.
    pub fn bot_command_seal(
        &self,
        publication: &CommandKeyPublication,
        request: &[u8],
    ) -> Result<(Vec<u8>, ReplySecret), ClientError> {
        let req = decode_command_input(request)?;
        let own = self.own()?;
        let signer = self.signer()?;
        seal_command(
            publication,
            &req,
            UserId::from_bytes(own.user_id),
            DeviceId::from_bytes(own.device_id),
            signer,
        )
        .map_err(|e| ClientError::new(E_CORE_INPUT, format!("seal: {e}")))
    }
}

#[cfg(test)]
mod tests {
    use super::super::error::{E_CREDENTIAL, E_UMK_CHANGED};
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
}
