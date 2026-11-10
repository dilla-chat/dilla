use dilla_core::cbor::{CborError, Decoder, Encoder, decode_strict};
use sha2::{Digest, Sha256};
use std::{
    fs::File,
    io::Read,
    time::{Duration, Instant},
};
use zeroize::{Zeroize, Zeroizing};

pub(crate) const MAX_LOGICAL: usize = 32 * 1024 * 1024;
pub(crate) const MAX_CHUNK: usize = 768 * 1024;
const UPLOAD_TTL: Duration = Duration::from_secs(30);
const RESULT_TTL: Duration = Duration::from_secs(30);

pub(crate) struct Transfer {
    id: [u8; 16],
    _request_id: [u8; 16],
    state: State,
}

enum State {
    Upload {
        expected: usize,
        digest: [u8; 32],
        bytes: Zeroizing<Vec<u8>>,
        deadline: Instant,
    },
    Running,
    Complete {
        status: u64,
        bytes: Zeroizing<Vec<u8>>,
        deadline: Instant,
    },
}

pub(crate) enum Command {
    Begin {
        len: usize,
        digest: [u8; 32],
    },
    Chunk {
        id: [u8; 16],
        offset: usize,
        bytes: Zeroizing<Vec<u8>>,
    },
    Execute {
        id: [u8; 16],
    },
    Read {
        id: [u8; 16],
        offset: usize,
        len: usize,
    },
    Drop {
        id: [u8; 16],
    },
}

impl Transfer {
    pub(crate) fn begin(
        request_id: [u8; 16],
        len: usize,
        digest: [u8; 32],
        now: Instant,
    ) -> Result<(Self, [u8; 16]), ()> {
        if len == 0 || len > MAX_LOGICAL {
            return Err(());
        }
        let mut id = [0u8; 16];
        File::open("/dev/urandom")
            .and_then(|mut f| f.read_exact(&mut id))
            .map_err(|_| ())?;
        if id == [0; 16] {
            return Err(());
        }
        let mut bytes = Zeroizing::new(Vec::with_capacity(len));
        bytes.zeroize();
        let transfer = Self {
            id,
            _request_id: request_id,
            state: State::Upload {
                expected: len,
                digest,
                bytes,
                deadline: now + UPLOAD_TTL,
            },
        };
        Ok((transfer, id))
    }

    pub(crate) fn id_matches(&self, id: &[u8; 16]) -> bool {
        self.id == *id
    }
    pub(crate) fn is_running(&self) -> bool {
        matches!(self.state, State::Running)
    }
    #[cfg(test)]
    pub(crate) fn test_expire_at(&mut self, deadline: Instant) {
        match &mut self.state {
            State::Upload { deadline: d, .. } | State::Complete { deadline: d, .. } => {
                *d = deadline
            }
            State::Running => {}
        }
    }
    pub(crate) fn expired(&self, now: Instant) -> bool {
        match &self.state {
            State::Upload { deadline, .. } | State::Complete { deadline, .. } => now >= *deadline,
            State::Running => false,
        }
    }
    pub(crate) fn time_left(&self, now: Instant) -> Option<Duration> {
        match &self.state {
            State::Upload { deadline, .. } | State::Complete { deadline, .. } => {
                Some(deadline.saturating_duration_since(now))
            }
            State::Running => None,
        }
    }

    pub(crate) fn append(
        &mut self,
        id: [u8; 16],
        offset: usize,
        chunk: &[u8],
        now: Instant,
    ) -> Result<usize, ()> {
        if !self.id_matches(&id) {
            return Err(());
        }
        let State::Upload {
            expected,
            bytes,
            deadline,
            ..
        } = &mut self.state
        else {
            return Err(());
        };
        if now >= *deadline
            || chunk.is_empty()
            || chunk.len() > MAX_CHUNK
            || offset != bytes.len()
            || bytes
                .len()
                .checked_add(chunk.len())
                .filter(|n| *n <= *expected)
                .is_none()
        {
            return Err(());
        }
        bytes.extend_from_slice(chunk);
        Ok(bytes.len())
    }

    pub(crate) fn take_for_execute(
        &mut self,
        id: [u8; 16],
        now: Instant,
    ) -> Result<Zeroizing<Vec<u8>>, ()> {
        if !self.id_matches(&id) {
            return Err(());
        }
        let State::Upload {
            expected,
            digest,
            bytes,
            deadline,
        } = &mut self.state
        else {
            return Err(());
        };
        if now >= *deadline
            || bytes.len() != *expected
            || Sha256::digest(&**bytes).as_slice() != digest
        {
            return Err(());
        }
        let input = std::mem::replace(bytes, Zeroizing::new(Vec::new()));
        self.state = State::Running;
        Ok(input)
    }

    pub(crate) fn execute_with<F>(
        &mut self,
        id: [u8; 16],
        now: Instant,
        mut dispatch: F,
    ) -> Result<(u64, usize, [u8; 32]), ()>
    where
        F: FnMut(&[u8]) -> (u64, Zeroizing<Vec<u8>>),
    {
        if !self.id_matches(&id) {
            return Err(());
        }
        if let Some((status, len, digest)) = self.descriptor() {
            if let State::Complete { deadline, .. } = &mut self.state {
                *deadline = now + RESULT_TTL;
            }
            return Ok((status, len, digest));
        }
        let mut input = self.take_for_execute(id, now)?;
        validate_target_payload(&input)?;
        let (mut status, mut result) = dispatch(&input);
        input.zeroize();
        if status > 9 || result.len() > MAX_LOGICAL {
            status = 9;
            result = enc_empty();
        }
        if status != 0 {
            result = enc_empty();
        } else if validate_target_result(&result).is_err() {
            status = 9;
            result = enc_empty();
        }
        let (len, digest) = self.finish(status, result, Instant::now())?;
        Ok((status, len, digest))
    }

    pub(crate) fn finish(
        &mut self,
        status: u64,
        result: Zeroizing<Vec<u8>>,
        now: Instant,
    ) -> Result<(usize, [u8; 32]), ()> {
        if status > 9 || result.len() > MAX_LOGICAL {
            return Err(());
        }
        let digest: [u8; 32] = Sha256::digest(&*result).into();
        let len = result.len();
        self.state = State::Complete {
            status,
            bytes: result,
            deadline: now + RESULT_TTL,
        };
        Ok((len, digest))
    }

    pub(crate) fn read(
        &mut self,
        id: [u8; 16],
        offset: usize,
        len: usize,
        now: Instant,
    ) -> Result<Zeroizing<Vec<u8>>, ()> {
        if !self.id_matches(&id) || len == 0 || len > MAX_CHUNK {
            return Err(());
        }
        let State::Complete {
            bytes, deadline, ..
        } = &mut self.state
        else {
            return Err(());
        };
        if now >= *deadline
            || offset
                .checked_add(len)
                .filter(|end| *end <= bytes.len())
                .is_none()
        {
            return Err(());
        }
        *deadline = now + RESULT_TTL;
        Ok(Zeroizing::new(bytes[offset..offset + len].to_vec()))
    }
    pub(crate) fn descriptor(&self) -> Option<(u64, usize, [u8; 32])> {
        let State::Complete { status, bytes, .. } = &self.state else {
            return None;
        };
        Some((*status, bytes.len(), Sha256::digest(&**bytes).into()))
    }
    pub(crate) fn drop_id(&self, id: &[u8; 16]) -> bool {
        self.id_matches(id)
    }
}

pub(crate) fn decode_command(payload: &[u8]) -> Result<Command, ()> {
    decode_strict(payload, |d| {
        let n = d.array_len()?;
        let sub = d.uint()?;
        match (sub, n) {
            (0, 4) => {
                if d.uint()? != 6 {
                    return Err(shape());
                }
                let len = usize::try_from(d.uint()?).map_err(|_| shape())?;
                let digest = d.bytes_exact::<32>()?;
                Ok(Command::Begin { len, digest })
            }
            (1, 4) => Ok(Command::Chunk {
                id: d.bytes_exact::<16>()?,
                offset: usize::try_from(d.uint()?).map_err(|_| shape())?,
                bytes: Zeroizing::new(d.bytes()?.to_vec()),
            }),
            (2, 2) => Ok(Command::Execute {
                id: d.bytes_exact::<16>()?,
            }),
            (3, 4) => Ok(Command::Read {
                id: d.bytes_exact::<16>()?,
                offset: usize::try_from(d.uint()?).map_err(|_| shape())?,
                len: usize::try_from(d.uint()?).map_err(|_| shape())?,
            }),
            (4, 2) => Ok(Command::Drop {
                id: d.bytes_exact::<16>()?,
            }),
            _ => Err(shape()),
        }
    })
    .map_err(|_| ())
}
fn shape() -> dilla_core::cbor::CborError {
    dilla_core::cbor::CborError::TypeMismatch {
        expected: "transfer payload",
        offset: 0,
    }
}

pub(crate) fn enc_id(id: &[u8; 16]) -> Zeroizing<Vec<u8>> {
    let mut e = Encoder::new();
    e.array(1).bytes(id);
    Zeroizing::new(e.into_vec())
}
pub(crate) fn enc_next(offset: usize) -> Zeroizing<Vec<u8>> {
    let mut e = Encoder::new();
    e.array(1).uint(offset as u64);
    Zeroizing::new(e.into_vec())
}
pub(crate) fn enc_descriptor(status: u64, len: usize, digest: &[u8; 32]) -> Zeroizing<Vec<u8>> {
    let mut e = Encoder::new();
    e.array(3).uint(status).uint(len as u64).bytes(digest);
    Zeroizing::new(e.into_vec())
}
pub(crate) fn enc_chunk(bytes: &[u8]) -> Zeroizing<Vec<u8>> {
    let mut e = Encoder::new();
    e.array(1).bytes(bytes);
    Zeroizing::new(e.into_vec())
}
pub(crate) fn enc_empty() -> Zeroizing<Vec<u8>> {
    let mut e = Encoder::new();
    e.array(0);
    Zeroizing::new(e.into_vec())
}

pub(crate) fn validate_target_payload(bytes: &[u8]) -> Result<(), ()> {
    decode_strict(bytes, validate_group_apply).map_err(|_| ())
}

fn validate_group_apply(d: &mut Decoder<'_>) -> Result<(), CborError> {
    let n = d.array_len()?;
    let sub = d.uint()?;
    let valid = match (sub, n) {
        (0 | 17, 1) => true,
        (1 | 12, 2) => {
            id(d)?;
            true
        }
        (4 | 10 | 11, 3) => {
            id(d)?;
            id(d)?;
            true
        }
        (20, 2) => {
            id(d)?;
            true
        }
        (2, 7) => {
            id(d)?;
            id(d)?;
            id(d)?;
            let kind = kind(d)?;
            d.uint()?;
            let sender = d.opt_bytes_exact::<32>()?;
            if sender.is_some_and(|v| v == [0; 32]) || (kind == 3) != sender.is_none() {
                return Err(shape());
            }
            true
        }
        (3, 4) => {
            id(d)?;
            id(d)?;
            nonzero(d.uint()?)?;
            true
        }
        (5, 8) => {
            id(d)?;
            id(d)?;
            id(d)?;
            if kind(d)? == 3 {
                return Err(shape());
            }
            d.uint()?;
            bounded_bytes(d, 1, MAX_LOGICAL)?;
            bounded_bytes(d, 1, MAX_LOGICAL)?;
            true
        }
        (6, 4) => {
            id(d)?;
            id(d)?;
            nonzero(d.uint()?)?;
            true
        }
        (7, 3) => {
            array_body(d)?;
            array_body(d)?;
            true
        }
        (8, 5) => {
            id(d)?;
            array_body(d)?;
            array_body(d)?;
            d.uint()?;
            true
        }
        (9, 4) => {
            id(d)?;
            id(d)?;
            array_body(d)?;
            true
        }
        (13, 4) => {
            id(d)?;
            bounded_uint(d.uint()?, 0, u32::MAX as u64)?;
            bounded_uint(d.uint()?, 1, 256)?;
            true
        }
        (14, 7) => {
            id(d)?;
            bounded_uint(d.uint()?, 0, u32::MAX as u64)?;
            id(d)?;
            array_body(d)?;
            array_body(d)?;
            d.uint()?;
            true
        }
        (15, 3) => {
            id(d)?;
            array_body(d)?;
            true
        }
        (16, 8) => {
            id(d)?;
            array_body(d)?;
            d.uint()?;
            id(d)?;
            array_body(d)?;
            array_body(d)?;
            d.uint()?;
            true
        }
        (18, 3) => {
            d.uint()?;
            bounded_uint(d.uint()?, 1, 100)?;
            true
        }
        (19, 2) => {
            d.uint()?;
            true
        }
        _ => false,
    };
    if !valid {
        return Err(shape());
    }
    Ok(())
}

fn id(d: &mut Decoder<'_>) -> Result<(), CborError> {
    if d.bytes_exact::<16>()? == [0; 16] {
        Err(shape())
    } else {
        Ok(())
    }
}
fn kind(d: &mut Decoder<'_>) -> Result<u64, CborError> {
    let kind = d.uint()?;
    if matches!(kind, 0 | 1 | 3) {
        Ok(kind)
    } else {
        Err(shape())
    }
}
fn array_body(d: &mut Decoder<'_>) -> Result<(), CborError> {
    let bytes = d.bytes()?;
    if bytes.is_empty() || bytes.len() > MAX_LOGICAL || bytes.first().is_none_or(|b| b >> 5 != 4) {
        return Err(shape());
    }
    decode_strict(bytes, |nested| {
        nested.skip()?;
        Ok(())
    })
    .map_err(|_| shape())
}
fn bounded_bytes(d: &mut Decoder<'_>, min: usize, max: usize) -> Result<(), CborError> {
    let bytes = d.bytes()?;
    if bytes.len() < min || bytes.len() > max {
        Err(shape())
    } else {
        Ok(())
    }
}
fn nonzero(n: u64) -> Result<(), CborError> {
    if n == 0 { Err(shape()) } else { Ok(()) }
}
fn bounded_uint(n: u64, min: u64, max: u64) -> Result<(), CborError> {
    if (min..=max).contains(&n) {
        Ok(())
    } else {
        Err(shape())
    }
}

pub(crate) fn validate_target_result(bytes: &[u8]) -> Result<(), ()> {
    if bytes.first().is_none_or(|b| b >> 5 != 4) {
        return Err(());
    }
    decode_strict(bytes, |d| {
        d.skip()?;
        Ok(())
    })
    .map_err(|_| ())
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::{
        Arc,
        atomic::{AtomicUsize, Ordering},
    };

    fn bytes(n: usize, value: u8) -> Zeroizing<Vec<u8>> {
        Zeroizing::new(vec![value; n])
    }

    #[test]
    fn bounded_upload_execute_once_and_repeatable_result_reads_test_fixture() {
        const FIXTURE: &str =
            "test-only deterministic target handler; production group op6 remains unavailable";
        assert!(FIXTURE.starts_with("test-only"));
        let now = Instant::now();
        let mut cbor = Encoder::new();
        cbor.array(8)
            .uint(5)
            .bytes(&[1; 16])
            .bytes(&[2; 16])
            .bytes(&[3; 16])
            .uint(0)
            .uint(0)
            .bytes(&vec![0x44; 2 * 1024 * 1024])
            .bytes(&[0x80]);
        let input = cbor.into_vec();
        let digest: [u8; 32] = Sha256::digest(&input).into();
        let (mut transfer, id) = Transfer::begin([0x11; 16], input.len(), digest, now).unwrap();
        let mut offset = 0;
        for chunk in input.chunks(MAX_CHUNK) {
            offset = transfer.append(id, offset, chunk, now).unwrap();
        }
        assert_eq!(offset, input.len());
        let calls = Arc::new(AtomicUsize::new(0));
        let calls_in = calls.clone();
        let mut result_encoder = Encoder::new();
        result_encoder.array(1).bytes(&vec![0x55; 2 * 1024 * 1024]);
        let result = result_encoder.into_vec();
        let expected_result = result.clone();
        let descriptor = transfer
            .execute_with(id, now, |payload| {
                calls_in.fetch_add(1, Ordering::SeqCst);
                assert_eq!(payload, input);
                (0, Zeroizing::new(result.clone()))
            })
            .unwrap();
        assert_eq!(descriptor.0, 0);
        assert_eq!(descriptor.1, expected_result.len());
        assert_eq!(descriptor.2, Sha256::digest(&expected_result).as_slice());
        let repeated = transfer
            .execute_with(id, now, |_| panic!("duplicate dispatch"))
            .unwrap();
        assert_eq!(descriptor, repeated);
        assert_eq!(calls.load(Ordering::SeqCst), 1);
        let first = transfer.read(id, 0, MAX_CHUNK, now).unwrap();
        let repeat = transfer.read(id, 0, MAX_CHUNK, now).unwrap();
        assert_eq!(&*first, &*repeat);
        assert!(
            transfer
                .read(id, expected_result.len() - 1, 2, now)
                .is_err()
        );
    }

    #[test]
    fn rejects_length_hash_chunk_offset_target_and_expiry_before_dispatch() {
        let now = Instant::now();
        assert!(Transfer::begin([1; 16], 0, [0; 32], now).is_err());
        assert!(Transfer::begin([1; 16], MAX_LOGICAL + 1, [0; 32], now).is_err());
        let (at_logical_limit, _) = Transfer::begin([1; 16], MAX_LOGICAL, [0; 32], now).unwrap();
        drop(at_logical_limit);

        let (mut at_chunk_limit, chunk_id) =
            Transfer::begin([1; 16], MAX_CHUNK, [0; 32], now).unwrap();
        let exact_chunk = vec![0; MAX_CHUNK];
        assert_eq!(
            at_chunk_limit
                .append(chunk_id, 0, &exact_chunk, now)
                .unwrap(),
            MAX_CHUNK
        );
        let input = [0x81, 0x11];
        let digest: [u8; 32] = Sha256::digest(input).into();
        let (mut transfer, id) = Transfer::begin([1; 16], 2, digest, now).unwrap();
        assert!(transfer.append(id, 1, &input, now).is_err());
        assert!(transfer.append([2; 16], 0, &input, now).is_err());
        let early_calls = AtomicUsize::new(0);
        assert!(
            transfer
                .execute_with(id, now, |_| {
                    early_calls.fetch_add(1, Ordering::SeqCst);
                    (0, bytes(1, 0x80))
                })
                .is_err()
        );
        assert_eq!(early_calls.load(Ordering::SeqCst), 0);
        assert!(transfer.read(id, 0, 1, now).is_err());
        assert!(
            transfer
                .append(id, 0, &vec![0; MAX_CHUNK + 1], now)
                .is_err()
        );
        assert_eq!(transfer.append(id, 0, &input, now).unwrap(), 2);
        assert!(transfer.append(id, 0, &input, now).is_err());
        let wrong_id_calls = AtomicUsize::new(0);
        assert!(
            transfer
                .execute_with([2; 16], now, |_| {
                    wrong_id_calls.fetch_add(1, Ordering::SeqCst);
                    (0, bytes(0, 0))
                })
                .is_err()
        );
        assert_eq!(wrong_id_calls.load(Ordering::SeqCst), 0);
        let now_late = now + UPLOAD_TTL + Duration::from_millis(1);
        let late_calls = AtomicUsize::new(0);
        assert!(
            transfer
                .execute_with(id, now_late, |_| {
                    late_calls.fetch_add(1, Ordering::SeqCst);
                    (0, bytes(0, 0))
                })
                .is_err()
        );
        assert_eq!(late_calls.load(Ordering::SeqCst), 0);

        let mut bad_hash = digest;
        bad_hash[0] ^= 0xff;
        let (mut transfer, id) = Transfer::begin([1; 16], 2, bad_hash, now).unwrap();
        transfer.append(id, 0, &input, now).unwrap();
        let calls = AtomicUsize::new(0);
        assert!(
            transfer
                .execute_with(id, now, |_| {
                    calls.fetch_add(1, Ordering::SeqCst);
                    (0, bytes(0, 0))
                })
                .is_err()
        );
        assert_eq!(calls.load(Ordering::SeqCst), 0);

        let not_array = [0x01];
        let hash: [u8; 32] = Sha256::digest(not_array).into();
        let (mut transfer, id) = Transfer::begin([1; 16], 1, hash, now).unwrap();
        transfer.append(id, 0, &not_array, now).unwrap();
        let calls = AtomicUsize::new(0);
        assert!(
            transfer
                .execute_with(id, now, |_| {
                    calls.fetch_add(1, Ordering::SeqCst);
                    (0, bytes(0, 0))
                })
                .is_err()
        );
        assert_eq!(calls.load(Ordering::SeqCst), 0);

        let mut wrong_target = Encoder::new();
        wrong_target
            .array(4)
            .uint(0)
            .uint(7)
            .uint(1)
            .bytes(&[0; 32]);
        assert!(decode_command(&wrong_target.into_vec()).is_err());
    }

    #[test]
    fn deadlines_are_monotonic_and_result_reads_extend_idle_expiry() {
        let now = Instant::now();
        let input = [0x81, 0x11];
        let digest: [u8; 32] = Sha256::digest(input).into();
        let (mut transfer, id) = Transfer::begin([1; 16], 2, digest, now).unwrap();
        assert!(transfer.expired(now + UPLOAD_TTL));
        transfer.append(id, 0, &input, now).unwrap();
        transfer
            .execute_with(id, now, |_| (0, bytes(1, 0x80)))
            .unwrap();
        assert!(transfer.expired(now + UPLOAD_TTL + RESULT_TTL));
        transfer.read(id, 0, 1, now + UPLOAD_TTL).unwrap();
        transfer
            .execute_with(id, now + UPLOAD_TTL + Duration::from_secs(1), |_| {
                panic!("repeat execution must not dispatch")
            })
            .unwrap();
        assert!(!transfer.expired(now + UPLOAD_TTL + RESULT_TTL - Duration::from_millis(1)));
        assert!(!transfer.expired(now + UPLOAD_TTL + Duration::from_millis(30_900)));
    }

    #[test]
    fn typed_target_requires_known_suboperation_and_exact_fields() {
        let mut valid = Encoder::new();
        valid.array(1).uint(17); // group_apply pending_leaves []
        assert!(validate_target_payload(&valid.into_vec()).is_ok());

        let mut unknown = Encoder::new();
        unknown.array(1).uint(21);
        assert!(validate_target_payload(&unknown.into_vec()).is_err());

        let mut extra = Encoder::new();
        extra.array(2).uint(17).uint(0);
        assert!(validate_target_payload(&extra.into_vec()).is_err());

        let mut zero_group = Encoder::new();
        zero_group.array(2).uint(1).bytes(&[0; 16]);
        assert!(validate_target_payload(&zero_group.into_vec()).is_err());

        let mut wrong_nested_body = Encoder::new();
        wrong_nested_body
            .array(3)
            .uint(15)
            .bytes(&[1; 16])
            .bytes(&[0x01]);
        assert!(validate_target_payload(&wrong_nested_body.into_vec()).is_err());

        // Bot v2 Task 23: the kind-3 suboperations 21 to 27 are gone.
        for sub in 21..=27u64 {
            let mut retired = Encoder::new();
            retired.array(1).uint(sub);
            assert!(validate_target_payload(&retired.into_vec()).is_err());
        }
    }

    #[test]
    fn accepts_valid_minimal_payload_for_every_group_suboperation() {
        fn id(e: &mut Encoder, byte: u8) {
            e.bytes(&[byte; 16]);
        }
        fn body(e: &mut Encoder) {
            e.bytes(&[0x80]);
        }

        for sub in 0..=20 {
            let mut e = Encoder::new();
            match sub {
                0 | 17 => {
                    e.array(1).uint(sub);
                }
                1 | 12 => {
                    e.array(2).uint(sub);
                    id(&mut e, 1);
                }
                2 => {
                    e.array(7).uint(sub);
                    id(&mut e, 1);
                    id(&mut e, 2);
                    id(&mut e, 3);
                    e.uint(0).uint(0).bytes(&[4; 32]);
                }
                3 | 6 => {
                    e.array(4).uint(sub);
                    id(&mut e, 1);
                    id(&mut e, 2);
                    e.uint(1);
                }
                4 | 10 | 11 => {
                    e.array(3).uint(sub);
                    id(&mut e, 1);
                    id(&mut e, 2);
                }
                5 => {
                    e.array(8).uint(sub);
                    id(&mut e, 1);
                    id(&mut e, 2);
                    id(&mut e, 3);
                    e.uint(0).uint(0).bytes(&[0]).bytes(&[0]);
                }
                7 => {
                    e.array(3).uint(sub);
                    body(&mut e);
                    body(&mut e);
                }
                8 => {
                    e.array(5).uint(sub);
                    id(&mut e, 1);
                    body(&mut e);
                    body(&mut e);
                    e.uint(0); // Empty group log has through_sequence zero.
                }
                9 => {
                    e.array(4).uint(sub);
                    id(&mut e, 1);
                    id(&mut e, 2);
                    body(&mut e);
                }
                13 => {
                    e.array(4).uint(sub);
                    id(&mut e, 1);
                    e.uint(0).uint(1);
                }
                14 => {
                    e.array(7).uint(sub);
                    id(&mut e, 1);
                    e.uint(0);
                    id(&mut e, 2);
                    body(&mut e);
                    body(&mut e);
                    e.uint(0);
                }
                15 => {
                    e.array(3).uint(sub);
                    id(&mut e, 1);
                    body(&mut e);
                }
                16 => {
                    e.array(8).uint(sub);
                    id(&mut e, 1);
                    body(&mut e);
                    e.uint(0);
                    id(&mut e, 2);
                    body(&mut e);
                    body(&mut e);
                    e.uint(0);
                }
                18 => {
                    e.array(3).uint(sub).uint(0).uint(1);
                }
                19 => {
                    e.array(2).uint(sub).uint(0);
                }
                20 => {
                    e.array(2).uint(sub);
                    id(&mut e, 1);
                }
                _ => unreachable!(),
            }
            assert!(
                validate_target_payload(&e.into_vec()).is_ok(),
                "minimal contract payload for group suboperation {sub} must validate"
            );
        }
    }

    #[test]
    fn kind_and_sender_constraints_are_checked_before_dispatch() {
        fn create(kind: u64, sender: Option<[u8; 32]>) -> Vec<u8> {
            let mut e = Encoder::new();
            e.array(7)
                .uint(2)
                .bytes(&[1; 16])
                .bytes(&[2; 16])
                .bytes(&[3; 16]);
            e.uint(kind).uint(0);
            match sender {
                Some(key) => e.bytes(&key),
                None => e.null(),
            };
            e.into_vec()
        }
        fn external_join(kind: u64) -> Vec<u8> {
            let mut e = Encoder::new();
            e.array(8)
                .uint(5)
                .bytes(&[1; 16])
                .bytes(&[2; 16])
                .bytes(&[3; 16])
                .uint(kind)
                .uint(0)
                .bytes(&[0])
                .bytes(&[0]);
            e.into_vec()
        }
        fn dispatch_count(payload: &[u8]) -> (Result<(), ()>, usize) {
            let now = Instant::now();
            let digest: [u8; 32] = Sha256::digest(payload).into();
            let (mut transfer, id) = Transfer::begin([9; 16], payload.len(), digest, now).unwrap();
            transfer.append(id, 0, payload, now).unwrap();
            let calls = AtomicUsize::new(0);
            let result = transfer
                .execute_with(id, now, |_| {
                    calls.fetch_add(1, Ordering::SeqCst);
                    (0, Zeroizing::new(vec![0x80]))
                })
                .map(|_| ());
            (result, calls.load(Ordering::SeqCst))
        }

        for invalid in [
            create(0, None),
            create(1, None),
            create(3, Some([4; 32])),
            external_join(3),
        ] {
            let (result, calls) = dispatch_count(&invalid);
            assert!(result.is_err(), "invalid kind/sender pairing was accepted");
            assert_eq!(calls, 0, "invalid target payload reached dispatch");
        }

        for valid in [
            create(0, Some([4; 32])),
            create(1, Some([4; 32])),
            create(3, None),
            external_join(0),
            external_join(1),
        ] {
            let (result, calls) = dispatch_count(&valid);
            assert!(result.is_ok(), "valid kind/sender pairing was rejected");
            assert_eq!(calls, 1, "valid target payload must reach dispatch once");
        }
    }
}
