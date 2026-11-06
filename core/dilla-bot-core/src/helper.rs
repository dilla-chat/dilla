use crate::transfer::{self, Command as TransferCommand, Transfer};
use crate::{
    state::{self, StateError},
    store::{Store, StoreError},
};
use dilla_core::{
    bot::{HELPER_MAX, HELPER_PAYLOAD_MAX, HelperRequest, HelperResponse},
    cbor::{Encoder, decode_strict},
    client::ClientCore,
    client::RemovalFeedEntry,
    ids::MsgId,
};
use std::{
    path::{Path, PathBuf},
    time::Instant,
};
use zeroize::{Zeroize, Zeroizing};

#[derive(Debug, thiserror::Error)]
pub enum HelperError {
    #[error("invalid helper request")]
    Invalid,
    #[error("helper operation is unavailable")]
    Unavailable,
    #[error("helper operation failed")]
    Internal,
}

pub struct Helper {
    state_dir: PathBuf,
    store: Option<Store>,
    core: Option<ClientCore>,
    transfer: Option<Transfer>,
}

impl Helper {
    pub fn new(state_dir: impl AsRef<Path>) -> Self {
        Self {
            state_dir: state_dir.as_ref().to_path_buf(),
            store: None,
            core: None,
            transfer: None,
        }
    }

    pub(crate) fn dispatch(
        &mut self,
        input: &[u8],
    ) -> Result<(u64, Zeroizing<Vec<u8>>, bool), HelperError> {
        let (id, operation, payload) = decode_request(input).map_err(|_| HelperError::Invalid)?;
        let result = self.dispatch_operation(operation, &payload, *id.as_bytes());
        if operation == 16 && matches!(result, Err(DispatchError::Invalid)) {
            self.transfer = None;
        }
        let (mut status, payload, shutdown) = match result {
            Ok((body, stop)) => (0, body, stop),
            Err(DispatchError::Invalid) => (1, empty(), false),
            Err(DispatchError::Forbidden) => (2, empty(), false),
            Err(DispatchError::Expired) => (3, empty(), false),
            Err(DispatchError::Conflict) => (4, empty(), false),
            Err(DispatchError::Unavailable) => (5, empty(), false),
            Err(DispatchError::Busy) => (6, empty(), false),
            Err(DispatchError::NotFound) => (7, empty(), false),
            Err(DispatchError::Upgrade) => (8, empty(), false),
            Err(DispatchError::Internal) => (9, empty(), false),
        };
        let mut response = HelperResponse {
            request_id: id,
            status,
            payload: payload.to_vec(),
        };
        let encoded = match response.encode() {
            Ok(encoded) => encoded,
            Err(_) if operation == 6 => {
                // A successful MLS mutation may have committed an upload intent whose exact
                // result exceeds the direct helper frame. Surface that transport limit; the
                // caller can recover the original bytes through suboperation 20.
                response.payload.zeroize();
                response.status = 5;
                response.payload = empty().to_vec();
                status = 5;
                response.encode().map_err(|_| HelperError::Internal)?
            }
            Err(_) => {
                response.payload.zeroize();
                return Err(HelperError::Internal);
            }
        };
        response.payload.zeroize();
        Ok((status, Zeroizing::new(encoded), shutdown))
    }

    fn dispatch_operation(
        &mut self,
        op: u64,
        payload: &[u8],
        request_id: [u8; 16],
    ) -> Result<(Zeroizing<Vec<u8>>, bool), DispatchError> {
        if self.transfer.is_some()
            && op != 15
            && op != 16
            && !(op == 6 && self.transfer.as_ref().is_some_and(Transfer::is_running))
        {
            return Err(DispatchError::Busy);
        }
        if op == 1 {
            return self.open(payload).map(|b| (b, false));
        }
        if op == 15 {
            decode_strict(payload, |d| {
                d.array(0)?;
                Ok(())
            })
            .map_err(|_| DispatchError::Invalid)?;
            if self.core.is_none() {
                return Err(DispatchError::Invalid);
            }
            return Ok((empty(), true));
        }
        if self.core.is_none() {
            return Err(DispatchError::Invalid);
        }
        if op == 17 {
            return self.ingest_removals(payload);
        }
        if op == 18 {
            return self.refresh_removal_actor_history(payload);
        }
        match op {
            2 => self.bootstrap(payload),
            3 => self.complete(payload),
            4 => self.session(payload),
            5 => self.keypackage_intent(payload),
            10 => self.removal(payload),
            11 => state::get(self.store.as_ref().expect("opened"), payload)
                .map(|v| (Zeroizing::new(v), false))
                .map_err(Into::into),
            12 => state::transaction(self.store.as_mut().expect("opened"), payload)
                .map(|v| (Zeroizing::new(v), false))
                .map_err(Into::into),
            6 => crate::group_adapter::dispatch(
                self.core.as_mut().expect("opened"),
                self.store.as_ref().expect("opened").bind(),
                payload,
            )
            .map(|v| (v, false)),
            13 => crate::group_adapter::cursor_ack(
                self.core.as_mut().expect("opened"),
                self.store.as_ref().expect("opened").bind(),
                payload,
            )
            .map(|v| (v, false)),
            7 => self.prepare_send(payload),
            8 => self.send_receipt(payload),
            9 => self.call_media_epoch(payload),
            14 => self.keypackage_resolve(payload),
            16 => self.group_transfer(payload, request_id),
            19 => self.call_join_check(payload),
            20 => self.call_group_forget(payload),
            21 => crate::command::op_key_current(
                self.store.as_ref().expect("opened"),
                self.core.as_ref().expect("opened"),
                payload,
            )
            .map(|v| (v, false)),
            22 => crate::command::op_open(
                self.store.as_ref().expect("opened"),
                self.core.as_ref().expect("opened"),
                payload,
            )
            .map(|v| (v, false)),
            23 => crate::command::op_reply_seal(payload).map(|v| (v, false)),
            24 => {
                crate::command::op_device_list_ingest(self.core.as_mut().expect("opened"), payload)
                    .map(|v| (v, false))
            }
            _ => Err(DispatchError::Invalid),
        }
    }

    fn ingest_removals(
        &mut self,
        payload: &[u8],
    ) -> Result<(Zeroizing<Vec<u8>>, bool), DispatchError> {
        let (group, entries) = decode_strict(payload, |d| {
            d.array(2)?;
            let group = d.bytes_exact::<16>()?;
            let n = d.array_len()?;
            if n > 256 {
                return Err(dilla_core::cbor::CborError::WrongArrayLen {
                    expected: 256,
                    actual: n,
                });
            }
            let mut entries = Vec::with_capacity(n);
            for _ in 0..n {
                d.array(4)?;
                entries.push(RemovalFeedEntry {
                    revision: d.uint()?,
                    proof: d.bytes()?.to_vec(),
                    signature: d.bytes_exact::<64>()?,
                    accepted_at: d.uint()?,
                });
            }
            Ok((group, entries))
        })
        .map_err(|_| DispatchError::Invalid)?;
        let cursor = self
            .core
            .as_mut()
            .expect("opened")
            .ingest_removals(&group, &entries)
            .map_err(map_client)?;
        let mut e = Encoder::new();
        e.array(1).uint(cursor);
        Ok((Zeroizing::new(e.into_vec()), false))
    }

    fn refresh_removal_actor_history(
        &mut self,
        payload: &[u8],
    ) -> Result<(Zeroizing<Vec<u8>>, bool), DispatchError> {
        let (group, entry, history, head) = decode_strict(payload, |d| {
            d.array(4)?;
            let group = d.bytes_exact::<16>()?;
            d.array(3)?;
            let entry = RemovalFeedEntry {
                revision: 1,
                proof: d.bytes()?.to_vec(),
                signature: d.bytes_exact::<64>()?,
                accepted_at: d.uint()?,
            };
            let history = d.opt_bytes()?.map(ToOwned::to_owned);
            let head = d.opt_bytes()?.map(ToOwned::to_owned);
            Ok((group, entry, history, head))
        })
        .map_err(|_| DispatchError::Invalid)?;
        let version = match (history, head) {
            (None, None) => self
                .core
                .as_ref()
                .expect("opened")
                .removal_actor_history_version(&group, &entry),
            (Some(history), Some(head)) => self
                .core
                .as_mut()
                .expect("opened")
                .refresh_removal_actor_history(&group, &entry, &history, &head),
            _ => return Err(DispatchError::Invalid),
        }
        .map_err(map_client)?;
        let mut e = Encoder::new();
        e.array(1).uint(version);
        Ok((Zeroizing::new(e.into_vec()), false))
    }

    fn group_transfer(
        &mut self,
        payload: &[u8],
        request_id: [u8; 16],
    ) -> Result<(Zeroizing<Vec<u8>>, bool), DispatchError> {
        let command = transfer::decode_command(payload).map_err(|_| DispatchError::Invalid)?;
        let now = Instant::now();
        if self.transfer.as_ref().is_some_and(|t| t.expired(now)) {
            self.transfer = None;
        }
        match command {
            TransferCommand::Begin { len, digest } => {
                if self.transfer.is_some() {
                    return Err(DispatchError::Busy);
                }
                let (transfer, id) = Transfer::begin(request_id, len, digest, now)
                    .map_err(|_| DispatchError::Invalid)?;
                self.transfer = Some(transfer);
                Ok((transfer::enc_id(&id), false))
            }
            TransferCommand::Chunk { id, offset, bytes } => {
                let transfer = self.transfer.as_mut().ok_or(DispatchError::Invalid)?;
                let next = transfer
                    .append(id, offset, &bytes, now)
                    .map_err(|_| DispatchError::Invalid)?;
                Ok((transfer::enc_next(next), false))
            }
            TransferCommand::Execute { id } => {
                let mut transfer = self.transfer.take().ok_or(DispatchError::Invalid)?;
                let descriptor = transfer.execute_with(id, now, |input| {
                    match self.dispatch_operation(6, input, request_id) {
                        Ok((body, _)) => (0, body),
                        Err(error) => (dispatch_status(error), empty()),
                    }
                });
                self.transfer = Some(transfer);
                let (target_status, len, digest) =
                    descriptor.map_err(|_| DispatchError::Invalid)?;
                Ok((transfer::enc_descriptor(target_status, len, &digest), false))
            }
            TransferCommand::Read { id, offset, len } => {
                let transfer = self.transfer.as_mut().ok_or(DispatchError::Invalid)?;
                let bytes = transfer
                    .read(id, offset, len, now)
                    .map_err(|_| DispatchError::Invalid)?;
                Ok((transfer::enc_chunk(&bytes), false))
            }
            TransferCommand::Drop { id } => {
                if !self.transfer.as_ref().is_some_and(|t| t.drop_id(&id)) {
                    return Err(DispatchError::Invalid);
                }
                self.transfer = None;
                Ok((transfer::enc_empty(), false))
            }
        }
    }

    pub(crate) fn expire_transfer(&mut self) {
        if self
            .transfer
            .as_ref()
            .is_some_and(|t| t.expired(Instant::now()))
        {
            self.transfer = None;
        }
    }

    pub(crate) fn transfer_timeout(&self) -> Option<std::time::Duration> {
        self.transfer
            .as_ref()
            .and_then(|t| t.time_left(Instant::now()))
    }

    #[cfg(test)]
    pub(crate) fn test_transfer_deadline(&mut self, deadline: Instant) {
        let digest = [0; 32];
        let (mut transfer, _) = Transfer::begin([1; 16], 1, digest, Instant::now()).unwrap();
        transfer.test_expire_at(deadline);
        self.transfer = Some(transfer);
    }

    #[cfg(test)]
    pub(crate) fn test_completed_transfer_deadline(&mut self, deadline: Instant) {
        let now = Instant::now();
        let input = [0x81, 0x11];
        let digest: [u8; 32] = <sha2::Sha256 as sha2::Digest>::digest(input).into();
        let (mut transfer, id) = Transfer::begin([1; 16], input.len(), digest, now).unwrap();
        transfer.append(id, 0, &input, now).unwrap();
        transfer
            .execute_with(id, now, |_| (0, Zeroizing::new(vec![0x80])))
            .unwrap();
        transfer.test_expire_at(deadline);
        self.transfer = Some(transfer);
    }

    fn open(&mut self, payload: &[u8]) -> Result<Zeroizing<Vec<u8>>, DispatchError> {
        if self.core.is_some() {
            return Err(DispatchError::Invalid);
        }
        let (instance, community, installation, mut key) = decode_strict(payload, |d| {
            d.array(4)?;
            Ok((
                d.bytes_exact::<16>()?,
                d.bytes_exact::<16>()?,
                d.bytes_exact::<16>()?,
                Zeroizing::new(d.bytes_exact::<32>()?),
            ))
        })
        .map_err(|_| DispatchError::Invalid)?;
        if instance == [0; 16]
            || community == [0; 16]
            || installation == [0; 16]
            || key.iter().all(|b| *b == 0)
        {
            return Err(DispatchError::Invalid);
        }
        let store = Store::open(&self.state_dir, instance, community, installation, &key)
            .map_err(map_store)?;
        key.zeroize();
        let core = ClientCore::open(store.conn.clone()).map_err(map_client)?;
        let identity = core.identity().map_err(map_client)?;
        let mut e = Encoder::new();
        e.array(2).uint(1).bytes(&identity);
        self.store = Some(store);
        self.core = Some(core);
        Ok(Zeroizing::new(e.into_vec()))
    }

    fn bootstrap(&mut self, payload: &[u8]) -> Result<(Zeroizing<Vec<u8>>, bool), DispatchError> {
        let (i, c, n, external_sender_key) = decode_strict(payload, |d| {
            d.array(4)?;
            Ok((
                d.bytes_exact::<16>()?,
                d.bytes_exact::<16>()?,
                d.bytes_exact::<16>()?,
                d.bytes_exact::<32>()?,
            ))
        })
        .map_err(|_| DispatchError::Invalid)?;
        let bind = self.store.as_ref().expect("opened").bind();
        if (i, c, n) != bind {
            return Err(DispatchError::Invalid);
        }
        let v = self
            .core
            .as_mut()
            .expect("opened")
            .bot_bootstrap_begin(&i, &c, &n, &external_sender_key)
            .map_err(map_client)?;
        Ok((Zeroizing::new(v), false))
    }
    fn complete(&mut self, payload: &[u8]) -> Result<(Zeroizing<Vec<u8>>, bool), DispatchError> {
        let (u, name, now) = decode_strict(payload, |d| {
            d.array(3)?;
            Ok((d.bytes_exact::<16>()?, d.text()?.to_owned(), d.uint()?))
        })
        .map_err(|_| DispatchError::Invalid)?;
        if name.is_empty() || name.len() > 256 {
            return Err(DispatchError::Invalid);
        }
        let v = self
            .core
            .as_mut()
            .expect("opened")
            .bot_bootstrap_complete(&u, &name, now)
            .map_err(map_client)?;
        Ok((Zeroizing::new(v), false))
    }
    fn session(&mut self, payload: &[u8]) -> Result<(Zeroizing<Vec<u8>>, bool), DispatchError> {
        let core = self.core.as_mut().expect("opened");
        let result = decode_strict(payload, |d| {
            let n = d.array_len()?;
            let sub = d.uint()?;
            let v = match (sub, n) {
                (0, 1) => core.identity().map(Some),
                (1, 3) => {
                    let nonce = d.bytes_exact::<32>()?;
                    let purpose = d.uint()?;
                    if purpose > 1 {
                        return Err(crate::store::cbor_shape());
                    }
                    core.session_sign(&nonce, purpose as u8).map(Some)
                }
                (2, 4) => {
                    let token = Zeroizing::new(d.text()?.to_owned());
                    let expires = d.uint()?;
                    let idle = d.uint()?;
                    core.session_store(&token, expires, idle).map(|_| None)
                }
                (3, 1) => core.session().map(Some),
                (4, 1) => core.session_clear().map(|_| None),
                (5, 1) => core.device_list_body().map(Some),
                (6, 1) => core.device_list_published().map(|_| None),
                _ => return Err(crate::store::cbor_shape()),
            };
            v.map_err(|_| crate::store::cbor_shape())
        })
        .map_err(|_| DispatchError::Invalid)?;
        Ok((
            result.map_or_else(empty, |v| Zeroizing::new(body(v))),
            false,
        ))
    }
    fn keypackage_intent(
        &mut self,
        payload: &[u8],
    ) -> Result<(Zeroizing<Vec<u8>>, bool), DispatchError> {
        let (instance, community, installation) = self.store.as_ref().expect("opened").bind();
        let result = self
            .core
            .as_mut()
            .expect("opened")
            .bot_keypackage_intent(&instance, &community, &installation, payload)
            .map_err(map_client)?;
        Ok((Zeroizing::new(result), false))
    }

    fn prepare_send(
        &mut self,
        payload: &[u8],
    ) -> Result<(Zeroizing<Vec<u8>>, bool), DispatchError> {
        let (operation, attempt, group, request, now, generation) = decode_prepare_send(payload)?;
        let result = self
            .core
            .as_mut()
            .expect("opened")
            .bot_send_prepare(&operation, &attempt, &group, &request, now, generation)
            .map_err(map_client)?;
        Ok((Zeroizing::new(result), false))
    }

    fn send_receipt(
        &mut self,
        payload: &[u8],
    ) -> Result<(Zeroizing<Vec<u8>>, bool), DispatchError> {
        let (operation, receipt) = decode_send_receipt(payload)?;
        self.core
            .as_mut()
            .expect("opened")
            .bot_send_receipt(&operation, &receipt)
            .map_err(map_client)?;
        Ok((empty(), false))
    }

    fn call_media_epoch(
        &mut self,
        payload: &[u8],
    ) -> Result<(Zeroizing<Vec<u8>>, bool), DispatchError> {
        let call = decode_call_epoch(payload)?;
        let result = self
            .core
            .as_ref()
            .expect("opened")
            .call_media_epoch(&call)
            .map_err(map_client)?;
        if result.is_empty() || result.len() > 262_144 {
            return Err(DispatchError::Unavailable);
        }
        Ok((Zeroizing::new(body(result)), false))
    }

    fn call_join_check(
        &mut self,
        payload: &[u8],
    ) -> Result<(Zeroizing<Vec<u8>>, bool), DispatchError> {
        let call = decode_call_epoch(payload)?;
        self.core
            .as_ref()
            .expect("opened")
            .call_join_check(&call)
            .map_err(map_client)?;
        Ok((empty(), false))
    }

    fn call_group_forget(
        &mut self,
        payload: &[u8],
    ) -> Result<(Zeroizing<Vec<u8>>, bool), DispatchError> {
        let call = decode_call_epoch(payload)?;
        self.core
            .as_mut()
            .expect("opened")
            .call_group_forget(&call)
            .map_err(map_client)?;
        Ok((empty(), false))
    }

    fn keypackage_resolve(
        &mut self,
        payload: &[u8],
    ) -> Result<(Zeroizing<Vec<u8>>, bool), DispatchError> {
        let (instance, community, installation) = self.store.as_ref().expect("opened").bind();
        self.core
            .as_mut()
            .expect("opened")
            .bot_keypackage_resolve(&instance, &community, &installation, payload)
            .map_err(map_client)?;
        Ok((empty(), false))
    }
    fn removal(&mut self, payload: &[u8]) -> Result<(Zeroizing<Vec<u8>>, bool), DispatchError> {
        let (r, g, seq, expires, now) = decode_strict(payload, |d| {
            d.array(5)?;
            Ok((
                d.bytes_exact::<16>()?,
                d.bytes_exact::<16>()?,
                d.uint()?,
                d.uint()?,
                d.uint()?,
            ))
        })
        .map_err(|_| DispatchError::Invalid)?;
        let v = self
            .core
            .as_mut()
            .expect("opened")
            .sign_removal(&r, &g, seq, expires, now)
            .map_err(map_client)?;
        Ok((Zeroizing::new(v), false))
    }
}

fn decode_request(input: &[u8]) -> Result<(MsgId, u64, Zeroizing<Vec<u8>>), ()> {
    if input.len() > HELPER_MAX {
        return Err(());
    }
    let (raw_id, operation, payload) = decode_strict(input, |d| {
        d.array(4)?;
        if d.uint()? != 1 {
            return Err(crate::store::cbor_shape());
        }
        let raw_id = d.bytes_exact::<16>()?;
        if raw_id == [0; 16] {
            return Err(crate::store::cbor_shape());
        }
        let operation = d.uint()?;
        let payload = d.bytes()?;
        if payload.is_empty() || payload.len() > HELPER_PAYLOAD_MAX {
            return Err(crate::store::cbor_shape());
        }
        decode_strict(payload, |body| {
            body.skip()?;
            Ok(())
        })
        .map_err(|_| crate::store::cbor_shape())?;
        Ok((raw_id, operation, Zeroizing::new(payload.to_vec())))
    })
    .map_err(|_| ())?;
    if (1..=16).contains(&operation) {
        let request = HelperRequest::decode(input).map_err(|_| ())?;
        Ok((
            request.request_id,
            request.operation,
            Zeroizing::new(request.payload),
        ))
    } else {
        Ok((MsgId::from_bytes(raw_id), operation, payload))
    }
}

type PreparedSend = ([u8; 16], [u8; 16], [u8; 16], Zeroizing<Vec<u8>>, u64, u64);

fn decode_prepare_send(payload: &[u8]) -> Result<PreparedSend, DispatchError> {
    let decoded = decode_strict(payload, |d| {
        d.array(6)?;
        Ok((
            d.bytes_exact::<16>()?,
            d.bytes_exact::<16>()?,
            d.bytes_exact::<16>()?,
            Zeroizing::new(d.bytes()?.to_vec()),
            d.uint()?,
            d.uint()?,
        ))
    })
    .map_err(|_| DispatchError::Invalid)?;
    if decoded.0 == [0; 16] || decoded.1 == [0; 16] || decoded.2 == [0; 16] || decoded.3.is_empty()
    {
        return Err(DispatchError::Invalid);
    }
    Ok(decoded)
}

fn decode_send_receipt(payload: &[u8]) -> Result<([u8; 16], Zeroizing<Vec<u8>>), DispatchError> {
    let decoded = decode_strict(payload, |d| {
        d.array(2)?;
        Ok((d.bytes_exact::<16>()?, Zeroizing::new(d.bytes()?.to_vec())))
    })
    .map_err(|_| DispatchError::Invalid)?;
    if decoded.0 == [0; 16] || decoded.1.is_empty() || decoded.1.len() > 512 {
        return Err(DispatchError::Invalid);
    }
    Ok(decoded)
}

fn decode_call_epoch(payload: &[u8]) -> Result<[u8; 16], DispatchError> {
    let call = decode_strict(payload, |d| {
        d.array(1)?;
        d.bytes_exact::<16>()
    })
    .map_err(|_| DispatchError::Invalid)?;
    if call == [0; 16] {
        return Err(DispatchError::Invalid);
    }
    Ok(call)
}

pub(crate) fn body(mut v: Vec<u8>) -> Vec<u8> {
    let mut e = Encoder::new();
    e.array(1).bytes(&v);
    v.zeroize();
    e.into_vec()
}
pub(crate) fn empty() -> Zeroizing<Vec<u8>> {
    let mut e = Encoder::new();
    e.array(0);
    Zeroizing::new(e.into_vec())
}
fn dispatch_status(error: DispatchError) -> u64 {
    match error {
        DispatchError::Invalid => 1,
        DispatchError::Forbidden => 2,
        DispatchError::Expired => 3,
        DispatchError::Conflict => 4,
        DispatchError::Unavailable => 5,
        DispatchError::Busy => 6,
        DispatchError::NotFound => 7,
        DispatchError::Upgrade => 8,
        DispatchError::Internal => 9,
    }
}
#[derive(Debug)]
pub(crate) enum DispatchError {
    Invalid,
    Forbidden,
    Expired,
    Conflict,
    Unavailable,
    Busy,
    NotFound,
    Upgrade,
    Internal,
}
impl From<StateError> for DispatchError {
    fn from(e: StateError) -> Self {
        match e {
            StateError::Invalid => Self::Invalid,
            StateError::Conflict => Self::Conflict,
            StateError::Internal => Self::Internal,
        }
    }
}
fn map_store(e: StoreError) -> DispatchError {
    match e {
        StoreError::Binding => DispatchError::Conflict,
        StoreError::Locked => DispatchError::Busy,
        StoreError::Version => DispatchError::Upgrade,
        StoreError::Database => DispatchError::Internal,
        _ => DispatchError::Invalid,
    }
}
pub(crate) fn map_client(e: dilla_core::client::ClientError) -> DispatchError {
    match e.code {
        "E_STORE_NEWER" => DispatchError::Upgrade,
        "E_CORE_STATE" => DispatchError::Conflict,
        "E_CORE_INPUT" => DispatchError::Invalid,
        "E_CORE_NO_IDENTITY" => DispatchError::Unavailable,
        "E_CORE_NOT_FOUND" => DispatchError::NotFound,
        "E_BINDING"
        | "E_CREDENTIAL"
        | "E_DEVICE_LIST_STALE"
        | "E_DEVICE_UNLISTED"
        | "E_TIER_MISMATCH"
        | "E_UMK_CHANGED" => DispatchError::Forbidden,
        _ => DispatchError::Internal,
    }
}

#[cfg(test)]
mod send_abi_tests {
    use super::*;

    #[test]
    fn expired_dispatch_status_is_frozen_status_three() {
        assert_eq!(dispatch_status(DispatchError::Expired), 3);
    }

    #[test]
    fn send_operations_require_exact_shapes_nonzero_ids_and_bounded_receipts() {
        let mut prepare = Encoder::new();
        prepare
            .array(6)
            .bytes(&[1; 16])
            .bytes(&[2; 16])
            .bytes(&[3; 16])
            .bytes(&[0x80])
            .uint(4)
            .uint(5);
        assert!(decode_prepare_send(&prepare.into_vec()).is_ok());

        let mut extra = Encoder::new();
        extra
            .array(7)
            .bytes(&[1; 16])
            .bytes(&[2; 16])
            .bytes(&[3; 16])
            .bytes(&[0x80])
            .uint(4)
            .uint(5)
            .null();
        assert!(matches!(
            decode_prepare_send(&extra.into_vec()),
            Err(DispatchError::Invalid)
        ));

        let mut zero_id = Encoder::new();
        zero_id
            .array(6)
            .bytes(&[0; 16])
            .bytes(&[2; 16])
            .bytes(&[3; 16])
            .bytes(&[0x80])
            .uint(4)
            .uint(5);
        assert!(matches!(
            decode_prepare_send(&zero_id.into_vec()),
            Err(DispatchError::Invalid)
        ));

        let mut receipt = Encoder::new();
        receipt.array(2).bytes(&[1; 16]).bytes(&[0x81]);
        assert!(decode_send_receipt(&receipt.into_vec()).is_ok());
        let mut oversized = Encoder::new();
        oversized.array(2).bytes(&[1; 16]).bytes(&vec![0; 513]);
        assert!(matches!(
            decode_send_receipt(&oversized.into_vec()),
            Err(DispatchError::Invalid)
        ));
    }

    #[test]
    fn call_epoch_requires_exact_nonzero_call_id() {
        let mut valid = Encoder::new();
        valid.array(1).bytes(&[7; 16]);
        assert_eq!(decode_call_epoch(&valid.into_vec()).unwrap(), [7; 16]);

        for payload in [
            {
                let mut e = Encoder::new();
                e.array(1).bytes(&[0; 16]);
                e.into_vec()
            },
            {
                let mut e = Encoder::new();
                e.array(1).bytes(&[7; 15]);
                e.into_vec()
            },
            {
                let mut e = Encoder::new();
                e.array(2).bytes(&[7; 16]).uint(0);
                e.into_vec()
            },
        ] {
            assert!(matches!(
                decode_call_epoch(&payload),
                Err(DispatchError::Invalid)
            ));
        }
    }
}
