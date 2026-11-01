use crate::store::{Store, cbor_shape};
use dilla_core::cbor::{Decoder, Encoder, decode_strict};
use rusqlite::{OptionalExtension, TransactionBehavior, params};
use sha2::{Digest, Sha256};
use zeroize::Zeroizing;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum StateError {
    Invalid,
    Conflict,
    Internal,
}

type StateRow = (Vec<u8>, i64, Option<Zeroizing<Vec<u8>>>);

pub(crate) fn get(store: &Store, payload: &[u8]) -> Result<Vec<u8>, StateError> {
    enum Query {
        One(i64, Vec<u8>),
        Scan(i64, Option<Vec<u8>>, u64),
    }
    let query = decode_strict(payload, |d| {
        let n = d.array_len()?;
        let sub = d.uint()?;
        match (sub, n) {
            (0, 3) => Ok(Query::One(read_area(d)?, read_key(d)?)),
            (1, 4) => {
                let a = read_area(d)?;
                let after = d.opt_bytes()?.map(ToOwned::to_owned);
                if after
                    .as_ref()
                    .is_some_and(|k| k.is_empty() || k.len() > 128)
                {
                    return Err(cbor_shape());
                }
                let limit = d.uint()?;
                if !(1..=100).contains(&limit) {
                    return Err(cbor_shape());
                }
                Ok(Query::Scan(a, after, limit))
            }
            _ => Err(cbor_shape()),
        }
    })
    .map_err(|_| StateError::Invalid)?;
    let conn = store.conn.lock().map_err(|_| StateError::Internal)?;
    match query {
        Query::One(area, key) => {
            let row: Option<(i64, Option<Vec<u8>>)> = conn
                .query_row(
                    "SELECT revision,value FROM bot_state WHERE area=?1 AND key=?2",
                    params![area, key],
                    |r| Ok((r.get(0)?, r.get(1)?)),
                )
                .optional()
                .map_err(|_| StateError::Internal)?;
            let (rev, value) = row.unwrap_or((0, None));
            let value = value.map(Zeroizing::new);
            let mut e = Encoder::new();
            e.array(2)
                .uint(u64::try_from(rev).map_err(|_| StateError::Internal)?)
                .opt_bytes(value.as_ref().map(|v| v.as_slice()));
            Ok(e.into_vec())
        }
        Query::Scan(area, after, limit) => {
            let mut stmt=conn.prepare("SELECT key,revision,value FROM bot_state WHERE area=?1 AND (?2 IS NULL OR key>?2) ORDER BY key LIMIT ?3").map_err(|_|StateError::Internal)?;
            let result = stmt
                .query_map(
                    params![area, after, i64::try_from(limit).unwrap_or(100)],
                    |r| {
                        Ok((
                            r.get::<_, Vec<u8>>(0)?,
                            r.get::<_, i64>(1)?,
                            r.get::<_, Option<Vec<u8>>>(2)?.map(Zeroizing::new),
                        ))
                    },
                )
                .map_err(|_| StateError::Internal)?;
            let mut rows = Vec::new();
            for row in result {
                rows.push(row.map_err(|_| StateError::Internal)?)
            }
            let mut accepted: Vec<StateRow> = Vec::new();
            let mut next = None;
            for row in rows {
                let mut candidate = Encoder::new();
                candidate.array(2).array(accepted.len() + 1);
                for (k, r, v) in &accepted {
                    candidate
                        .array(3)
                        .bytes(k)
                        .uint(u64::try_from(*r).map_err(|_| StateError::Internal)?)
                        .opt_bytes(v.as_ref().map(|v| v.as_slice()));
                }
                candidate
                    .array(3)
                    .bytes(&row.0)
                    .uint(u64::try_from(row.1).map_err(|_| StateError::Internal)?)
                    .opt_bytes(row.2.as_ref().map(|v| v.as_slice()))
                    .opt_bytes(Some(&row.0));
                let candidate = Zeroizing::new(candidate.into_vec());
                if candidate.len() > dilla_core::bot::HELPER_PAYLOAD_MAX {
                    if accepted.is_empty() {
                        return Err(StateError::Internal);
                    }
                    break;
                }
                next = Some(row.0.clone());
                accepted.push(row);
            }
            let mut e = Encoder::new();
            e.array(2).array(accepted.len());
            for (k, r, v) in accepted {
                e.array(3)
                    .bytes(&k)
                    .uint(u64::try_from(r).map_err(|_| StateError::Internal)?)
                    .opt_bytes(v.as_ref().map(|v| v.as_slice()));
            }
            e.opt_bytes(next.as_deref());
            Ok(e.into_vec())
        }
    }
}

pub(crate) fn transaction(store: &mut Store, payload: &[u8]) -> Result<Vec<u8>, StateError> {
    let (id, changes_bytes, changes) = decode_strict(payload, |d| {
        d.array(2)?;
        let id = d.bytes_exact::<16>()?;
        if id == [0; 16] {
            return Err(cbor_shape());
        }
        let n = d.array_len()?;
        if !(1..=64).contains(&n) {
            return Err(cbor_shape());
        }
        let mut out = Vec::new();
        let mut seen = std::collections::BTreeSet::new();
        let mut encoded = Encoder::new();
        encoded.array(n);
        for _ in 0..n {
            d.array(4)?;
            let a = read_area(d)?;
            let k = read_key(d)?;
            let expected = d.uint()?;
            let value = d.opt_bytes()?.map(|v| Zeroizing::new(v.to_vec()));
            if let Some(v) = &value {
                validate_value(v).map_err(|_| cbor_shape())?
            }
            if !seen.insert((a, k.clone())) {
                return Err(cbor_shape());
            }
            encoded
                .array(4)
                .uint(a as u64)
                .bytes(&k)
                .uint(expected)
                .opt_bytes(value.as_ref().map(|v| v.as_slice()));
            out.push((a, k, expected, value));
        }
        Ok((id, Zeroizing::new(encoded.into_vec()), out))
    })
    .map_err(|_| StateError::Invalid)?;
    let digest = Sha256::digest(&changes_bytes[..]);
    // Only the new fixed checkpoint namespace opts into bounded receipts.
    // Ordinary settings/command transactions keep their existing replay
    // contract. Old checkpoint retries remain CAS-safe after receipt eviction:
    // the coordinator reconciles rows and the durable processed frontier.
    let checkpoint = changes
        .iter()
        .all(|(area, key, _, _)| *area == 8 && key.starts_with(b"dilla.bot.automod-state.v1/"));
    let mut conn = store.conn.lock().map_err(|_| StateError::Internal)?;
    let tx = conn
        .transaction_with_behavior(TransactionBehavior::Immediate)
        .map_err(|_| StateError::Internal)?;
    let receipt: Option<(Vec<u8>, Vec<u8>)> = tx
        .query_row(
            "SELECT digest,result FROM bot_state_receipts WHERE id=?1",
            [id.as_slice()],
            |r| Ok((r.get(0)?, r.get(1)?)),
        )
        .optional()
        .map_err(|_| StateError::Internal)?;
    if let Some((old, result)) = receipt {
        if old.as_slice() != digest.as_slice() {
            return Err(StateError::Conflict);
        }
        return Ok(result);
    }
    let mut revisions = Vec::new();
    for (area, key, expected, value) in changes {
        let current: Option<i64> = tx
            .query_row(
                "SELECT revision FROM bot_state WHERE area=?1 AND key=?2",
                params![area, key],
                |r| r.get(0),
            )
            .optional()
            .map_err(|_| StateError::Internal)?;
        let old = current.unwrap_or(0);
        if u64::try_from(old).ok() != Some(expected) {
            return Err(StateError::Conflict);
        }
        let next = old.checked_add(1).ok_or(StateError::Conflict)?;
        tx.execute("INSERT INTO bot_state(area,key,revision,value) VALUES(?1,?2,?3,?4) ON CONFLICT(area,key) DO UPDATE SET revision=excluded.revision,value=excluded.value",params![area,key,next,value.as_ref().map(|v|v.as_slice())]).map_err(|_|StateError::Internal)?;
        revisions.push(u64::try_from(next).map_err(|_| StateError::Internal)?);
    }
    let mut e = Encoder::new();
    e.array(revisions.len());
    for r in revisions {
        e.uint(r);
    }
    let result = e.into_vec();
    tx.execute(
        "INSERT INTO bot_state_receipts(id,digest,result) VALUES(?1,?2,?3)",
        params![id.as_slice(), digest.as_slice(), result],
    )
    .map_err(|_| StateError::Internal)?;
    if checkpoint {
        // Additive, optional index: old stores need no version migration and
        // helpers that never checkpoint create no additional table.
        tx.execute_batch("CREATE TABLE IF NOT EXISTS bot_checkpoint_receipts (seq INTEGER PRIMARY KEY AUTOINCREMENT, id BLOB UNIQUE NOT NULL);")
            .map_err(|_| StateError::Internal)?;
        tx.execute(
            "INSERT INTO bot_checkpoint_receipts(id) VALUES(?1)",
            [id.as_slice()],
        )
        .map_err(|_| StateError::Internal)?;
        tx.execute_batch("DELETE FROM bot_state_receipts WHERE id IN (SELECT id FROM bot_checkpoint_receipts ORDER BY seq DESC LIMIT -1 OFFSET 64); DELETE FROM bot_checkpoint_receipts WHERE seq IN (SELECT seq FROM bot_checkpoint_receipts ORDER BY seq DESC LIMIT -1 OFFSET 64);")
            .map_err(|_| StateError::Internal)?;
    }
    tx.commit().map_err(|_| StateError::Internal)?;
    Ok(result)
}

fn read_area(d: &mut Decoder<'_>) -> Result<i64, dilla_core::cbor::CborError> {
    let a = d.uint()?;
    if a > 9 {
        return Err(cbor_shape());
    }
    Ok(a as i64)
}
fn read_key(d: &mut Decoder<'_>) -> Result<Vec<u8>, dilla_core::cbor::CborError> {
    let k = d.bytes()?.to_vec();
    if k.is_empty() || k.len() > 128 {
        return Err(cbor_shape());
    }
    Ok(k)
}
fn validate_value(v: &[u8]) -> Result<(), StateError> {
    if v.len() > 262144 || v.first().is_none_or(|b| b >> 5 != 4) {
        return Err(StateError::Invalid);
    }
    decode_strict(v, |d| {
        d.skip()?;
        Ok(())
    })
    .map_err(|_| StateError::Invalid)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::store::{Store, StoreError};
    use std::{
        fs,
        os::unix::fs::PermissionsExt,
        path::{Path, PathBuf},
        sync::atomic::{AtomicU64, Ordering},
    };

    static NEXT: AtomicU64 = AtomicU64::new(0);
    fn temp_dir() -> PathBuf {
        let p = std::env::temp_dir().join(format!(
            "dilla-bot-core-{}-{}-{}",
            std::process::id(),
            NEXT.fetch_add(1, Ordering::Relaxed),
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_nanos()
        ));
        fs::create_dir(&p).unwrap();
        fs::set_permissions(&p, fs::Permissions::from_mode(0o700)).unwrap();
        p
    }
    fn store(path: &Path, key: [u8; 32]) -> Store {
        Store::open(path, [1; 16], [2; 16], [3; 16], &key).unwrap()
    }
    fn change(k: &[u8], rev: u64, v: Option<&[u8]>) -> Vec<u8> {
        let mut e = Encoder::new();
        e.array(4).uint(0).bytes(k).uint(rev).opt_bytes(v);
        e.into_vec()
    }
    fn build_transaction(id: [u8; 16], changes: &[Vec<u8>]) -> Vec<u8> {
        let mut c = Encoder::new();
        c.array(changes.len());
        for x in changes {
            c.raw(x);
        }
        let mut e = Encoder::new();
        e.array(2).bytes(&id).raw(&c.into_vec());
        e.into_vec()
    }
    fn state_get(k: &[u8]) -> Vec<u8> {
        let mut e = Encoder::new();
        e.array(3).uint(0).uint(0).bytes(k);
        e.into_vec()
    }
    fn decoded_pair(bytes: &[u8]) -> (u64, Option<Vec<u8>>) {
        decode_strict(bytes, |d| {
            d.array(2)?;
            Ok((d.uint()?, d.opt_bytes()?.map(ToOwned::to_owned)))
        })
        .unwrap()
    }

    #[test]
    fn automod_checkpoint_receipts_are_bounded_without_pruning_commands() {
        let path = temp_dir();
        let mut s = store(&path, [0x36; 32]);
        let regular = build_transaction([9; 16], &[change(b"settings", 0, Some(&[0x80]))]);
        let original = transaction(&mut s, &regular).unwrap();
        let mut last = Vec::new();
        for i in 0..256u64 {
            let mut id = [0; 16];
            id[..8].copy_from_slice(&(i + 1000).to_be_bytes());
            let mut c = Encoder::new();
            c.array(4)
                .uint(8)
                .bytes(b"dilla.bot.automod-state.v1/meta")
                .uint(i)
                .opt_bytes(Some(&[0x80]));
            last = build_transaction(id, &[c.into_vec()]);
            transaction(&mut s, &last).unwrap();
        }
        let count: i64 = s
            .conn
            .lock()
            .unwrap()
            .query_row("SELECT count(*) FROM bot_state_receipts", [], |r| r.get(0))
            .unwrap();
        assert_eq!(count, 65, "checkpoint hot path retained unlimited receipts");
        assert_eq!(transaction(&mut s, &regular).unwrap(), original);
        assert!(transaction(&mut s, &last).is_ok());
        drop(s);
        let _ = fs::remove_dir_all(path);
    }

    #[test]
    fn member_registration_catchup_area_is_available() {
        let path = temp_dir();
        let s = store(&path, [0x35; 32]);
        let mut request = Encoder::new();
        request
            .array(3)
            .uint(0)
            .uint(9)
            .bytes(b"dilla.bot.member-registration-catchup.v1");
        assert_eq!(
            decoded_pair(&get(&s, &request.into_vec()).unwrap()),
            (0, None)
        );
        drop(s);
        let _ = fs::remove_dir_all(path);
    }

    #[test]
    fn encrypted_store_reopens_and_rejects_wrong_key_and_plaintext_reader() {
        let path = temp_dir();
        let key = [0x42; 32];
        drop(store(&path, key));
        let db = path.join("state.db");
        let plain = rusqlite::Connection::open(&db).unwrap();
        assert!(
            plain
                .query_row("SELECT count(*) FROM sqlite_schema", [], |r| r
                    .get::<_, i64>(0))
                .is_err()
        );
        drop(plain);
        drop(store(&path, key));
        assert!(matches!(
            Store::open(&path, [1; 16], [2; 16], [3; 16], &[0x24; 32]),
            Err(StoreError::Key)
        ));
        let _ = fs::remove_dir_all(path);
    }

    #[test]
    fn core_opens_the_same_keyed_connection() {
        let path = temp_dir();
        let s = store(&path, [0x31; 32]);
        let _core = dilla_core::client::ClientCore::open(s.conn.clone()).unwrap();
        drop(s);
        let _ = fs::remove_dir_all(path);
    }

    #[test]
    fn cas_is_atomic_idempotent_and_tombstones_keep_revisions() {
        let path = temp_dir();
        let mut s = store(&path, [0x51; 32]);
        let first = build_transaction(
            [7; 16],
            &[
                change(b"a", 0, Some(&[0x81, 0x01])),
                change(b"b", 9, Some(&[0x81, 0x02])),
            ],
        );
        assert_eq!(transaction(&mut s, &first), Err(StateError::Conflict));
        assert_eq!(decoded_pair(&get(&s, &state_get(b"a")).unwrap()), (0, None));
        let committed = build_transaction([8; 16], &[change(b"a", 0, Some(&[0x81, 0x01]))]);
        let result = transaction(&mut s, &committed).unwrap();
        assert_eq!(result, transaction(&mut s, &committed).unwrap());
        assert_eq!(
            decoded_pair(&get(&s, &state_get(b"a")).unwrap()),
            (1, Some(vec![0x81, 0x01]))
        );
        let conflict = build_transaction([8; 16], &[change(b"a", 1, Some(&[0x81, 0x03]))]);
        assert_eq!(transaction(&mut s, &conflict), Err(StateError::Conflict));
        let deleted = build_transaction([9; 16], &[change(b"a", 1, None)]);
        transaction(&mut s, &deleted).unwrap();
        assert_eq!(decoded_pair(&get(&s, &state_get(b"a")).unwrap()), (2, None));
        drop(s);
        let _ = fs::remove_dir_all(path);
    }

    #[test]
    fn state_pages_are_lexical_and_retry_receipts_survive_reopen() {
        let path = temp_dir();
        let mut s = store(&path, [0x61; 32]);
        let bytes = build_transaction(
            [4; 16],
            &[
                change(b"z", 0, Some(&[0x80])),
                change(b"a", 0, Some(&[0x80])),
            ],
        );
        let result = transaction(&mut s, &bytes).unwrap();
        drop(s);
        let mut s = store(&path, [0x61; 32]);
        assert_eq!(transaction(&mut s, &bytes).unwrap(), result);
        let mut scan = Encoder::new();
        scan.array(4).uint(1).uint(0).null().uint(1);
        let page = get(&s, &scan.into_vec()).unwrap();
        let (rows, next) = decode_strict(&page, |d| {
            d.array(2)?;
            let n = d.array_len()?;
            let mut keys = Vec::new();
            for _ in 0..n {
                d.array(3)?;
                keys.push(d.bytes()?.to_vec());
                d.skip()?;
                d.skip()?;
            }
            Ok((keys, d.opt_bytes()?.map(ToOwned::to_owned)))
        })
        .unwrap();
        assert_eq!(rows, vec![b"a".to_vec()]);
        assert_eq!(next, Some(b"a".to_vec()));
        drop(s);
        let _ = fs::remove_dir_all(path);
    }

    #[test]
    fn maximum_value_is_returned_without_truncation_and_one_byte_over_is_rejected() {
        let path = temp_dir();
        let mut s = store(&path, [0x62; 32]);
        let make_value = |n: usize| {
            let mut v = vec![0x81, 0x5a, 0x00, 0x03, 0xff, 0xfa];
            v.resize(6 + n, 0x55);
            v
        };
        let exact = make_value(262138);
        assert_eq!(exact.len(), 262144);
        let bytes = build_transaction([44; 16], &[change(b"large", 0, Some(&exact))]);
        transaction(&mut s, &bytes).unwrap();
        assert_eq!(
            decoded_pair(&get(&s, &state_get(b"large")).unwrap()),
            (1, Some(exact))
        );
        let too_large = make_value(262139);
        assert_eq!(too_large.len(), 262145);
        let bytes = build_transaction([45; 16], &[change(b"oversize", 0, Some(&too_large))]);
        assert_eq!(transaction(&mut s, &bytes), Err(StateError::Invalid));
        drop(s);
        let _ = fs::remove_dir_all(path);
    }
}
