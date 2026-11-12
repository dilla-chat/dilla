//! Call signalling (Task 11, the minimum `dilla-media` needs): joining a call's MLS group and getting
//! the LiveKit token, the media keys of each epoch, leaving, rotating and declining a ring. A port
//! of `packages/client-core/src/calls/join.ts` and the parts of `controller.ts` and `ring.ts` a call
//! needs, over two seams so the sync engine (Task 9) can wire them to its own `ClientCore` and
//! transport later (Task 12):
//!
//! - [`CallCore`]: the `ClientCore` methods a call uses (implemented for `ClientCore` itself);
//! - [`CallRoutes`]: the instance routes a call uses, typed (implemented over any [`Http`] by
//!   [`HttpRoutes`]).
//!
//! The core is held behind a `Mutex` and locked per call, never across an await. Nothing is posted
//! for a tree the core's call gate refuses, and the join commit is posted only after
//! `call_join_check` passes (RV-1). The media key of an epoch leaves the core only through
//! `call_media_epoch`, which applies the gate first.

pub mod controller;
pub mod join;
pub mod wire;

use dilla_core::client::{ClientCore, ClientError};
use zeroize::Zeroizing;

use crate::seams::{Http, HttpError};

pub use controller::{leave_call, media_epoch, rotate_call};
pub use join::{JoinHooks, JoinRefused, JoinTarget, Joined, join_call};
pub use wire::{CallParticipant, CallStarted, ChannelCall, GroupSummary};

/// The core, locked for one call. A poisoned lock (a panic inside the core) is a refusal: the core's
/// state may be half-written, so nothing more is derived from it or posted for it.
pub(crate) fn lock<C>(
    core: &std::sync::Mutex<C>,
) -> Result<std::sync::MutexGuard<'_, C>, CoreRefusal> {
    core.lock().map_err(|_| CoreRefusal {
        code: "E_CORE_POISONED".into(),
        detail: "a panic inside the core".into(),
    })
}

/// A `ClientCore` failure, by its code.
#[derive(Clone, Debug, PartialEq, Eq, thiserror::Error)]
#[error("{code}: {detail}")]
pub struct CoreRefusal {
    pub code: String,
    pub detail: String,
}

impl From<ClientError> for CoreRefusal {
    fn from(e: ClientError) -> Self {
        Self {
            code: e.code.to_owned(),
            detail: e.detail,
        }
    }
}

/// A refused or failed route: the status and, when the transport carries it, the instance's
/// `E_*` code.
#[derive(Clone, Debug, PartialEq, Eq, thiserror::Error)]
#[error("route failed with status {status:?} ({code:?}): {detail}")]
pub struct RouteError {
    /// `None` for a transport or decode failure.
    pub status: Option<u16>,
    pub code: Option<String>,
    pub detail: String,
}

impl RouteError {
    pub fn is(&self, status: u16, code: &str) -> bool {
        self.status == Some(status) && self.code.as_deref() == Some(code)
    }

    pub(crate) fn decode(detail: impl Into<String>) -> Self {
        Self {
            status: None,
            code: Some("E_CBOR_SHAPE".into()),
            detail: detail.into(),
        }
    }
}

impl From<HttpError> for RouteError {
    fn from(e: HttpError) -> Self {
        match e {
            HttpError::Status(s) => Self {
                status: Some(s),
                code: None,
                detail: format!("status {s}"),
            },
            HttpError::Transport(t) => Self {
                status: None,
                code: None,
                detail: t,
            },
            HttpError::Decode(d) => Self {
                status: None,
                code: Some("E_CBOR_SHAPE".into()),
                detail: d,
            },
        }
    }
}

/// One epoch's media keys: `ClientCore::call_media_epoch` decoded. The key is zeroized on drop.
pub struct MediaEpoch {
    pub epoch: u64,
    pub base_key: Zeroizing<[u8; 16]>,
    pub own_leaf: u16,
    /// `(leaf, device_id, user_id)` of every member.
    pub roster: Vec<(u16, [u8; 16], [u8; 16])>,
    /// The epoch's safety code (6 groups of 5 digits).
    pub code: String,
}

impl std::fmt::Debug for MediaEpoch {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("MediaEpoch")
            .field("epoch", &self.epoch)
            .field("own_leaf", &self.own_leaf)
            .field("roster", &self.roster.len())
            .finish_non_exhaustive()
    }
}

/// The `ClientCore` methods a call uses (the TS `CorePort` subset). Bodies are the core's CBOR.
pub trait CallCore {
    fn call_group_create(
        &mut self,
        id: &[u8; 16],
        community: Option<&[u8; 16]>,
        channel: &[u8; 16],
        policy: u64,
        external_sender_pub: &[u8; 32],
    ) -> Result<Vec<u8>, CoreRefusal>;
    fn call_group_join_external(
        &mut self,
        id: &[u8; 16],
        community: Option<&[u8; 16]>,
        channel: &[u8; 16],
        policy: u64,
        info: &[u8],
        tree: &[u8],
    ) -> Result<Vec<u8>, CoreRefusal>;
    fn call_join_check(&self, id: &[u8; 16]) -> Result<(), CoreRefusal>;
    fn call_media_epoch(&self, id: &[u8; 16]) -> Result<Vec<u8>, CoreRefusal>;
    fn call_leave_proposal(&mut self, id: &[u8; 16]) -> Result<Vec<u8>, CoreRefusal>;
    fn call_rotate(&mut self, id: &[u8; 16]) -> Result<Vec<u8>, CoreRefusal>;
    fn call_group_forget(&mut self, id: &[u8; 16]) -> Result<(), CoreRefusal>;
    fn group_registered(&mut self, id: &[u8; 16], next_seq: u64) -> Result<(), CoreRefusal>;
    fn group_joined(&mut self, id: &[u8; 16], seq: u64) -> Result<(), CoreRefusal>;
    fn group_discard(&mut self, id: &[u8; 16]) -> Result<(), CoreRefusal>;
    fn commit_confirm(&mut self, id: &[u8; 16]) -> Result<(), CoreRefusal>;
    fn commit_abort(&mut self, id: &[u8; 16]) -> Result<(), CoreRefusal>;
    /// Every group row (`ClientCore::groups`, decoded).
    fn groups(&self) -> Result<Vec<GroupSummary>, CoreRefusal>;
}

impl CallCore for ClientCore {
    fn call_group_create(
        &mut self,
        id: &[u8; 16],
        community: Option<&[u8; 16]>,
        channel: &[u8; 16],
        policy: u64,
        external_sender_pub: &[u8; 32],
    ) -> Result<Vec<u8>, CoreRefusal> {
        Ok(ClientCore::call_group_create(
            self,
            id,
            community,
            channel,
            policy,
            external_sender_pub,
        )?)
    }
    fn call_group_join_external(
        &mut self,
        id: &[u8; 16],
        community: Option<&[u8; 16]>,
        channel: &[u8; 16],
        policy: u64,
        info: &[u8],
        tree: &[u8],
    ) -> Result<Vec<u8>, CoreRefusal> {
        Ok(ClientCore::call_group_join_external(
            self, id, community, channel, policy, info, tree,
        )?)
    }
    fn call_join_check(&self, id: &[u8; 16]) -> Result<(), CoreRefusal> {
        Ok(ClientCore::call_join_check(self, id)?)
    }
    fn call_media_epoch(&self, id: &[u8; 16]) -> Result<Vec<u8>, CoreRefusal> {
        Ok(ClientCore::call_media_epoch(self, id)?)
    }
    fn call_leave_proposal(&mut self, id: &[u8; 16]) -> Result<Vec<u8>, CoreRefusal> {
        Ok(ClientCore::call_leave_proposal(self, id)?)
    }
    fn call_rotate(&mut self, id: &[u8; 16]) -> Result<Vec<u8>, CoreRefusal> {
        Ok(ClientCore::call_rotate(self, id)?)
    }
    fn call_group_forget(&mut self, id: &[u8; 16]) -> Result<(), CoreRefusal> {
        Ok(ClientCore::call_group_forget(self, id)?)
    }
    fn group_registered(&mut self, id: &[u8; 16], next_seq: u64) -> Result<(), CoreRefusal> {
        Ok(ClientCore::group_registered(self, id, next_seq)?)
    }
    fn group_joined(&mut self, id: &[u8; 16], seq: u64) -> Result<(), CoreRefusal> {
        Ok(ClientCore::group_joined(self, id, seq)?)
    }
    fn group_discard(&mut self, id: &[u8; 16]) -> Result<(), CoreRefusal> {
        Ok(ClientCore::group_discard(self, id)?)
    }
    fn commit_confirm(&mut self, id: &[u8; 16]) -> Result<(), CoreRefusal> {
        ClientCore::commit_confirm(self, id)?;
        Ok(())
    }
    fn commit_abort(&mut self, id: &[u8; 16]) -> Result<(), CoreRefusal> {
        Ok(ClientCore::commit_abort(self, id)?)
    }
    fn groups(&self) -> Result<Vec<GroupSummary>, CoreRefusal> {
        wire::decode_groups(&ClientCore::groups(self)?).map_err(|e| CoreRefusal {
            code: "E_CBOR_SHAPE".into(),
            detail: e,
        })
    }
}

/// The instance routes a call uses, typed (packages/client-core/src/http/routes.ts).
#[async_trait::async_trait]
pub trait CallRoutes: Send + Sync {
    /// L-HTTP-100 `GET /v1/channels/{id}/call`.
    async fn get_channel_call(&self, channel: &[u8; 16]) -> Result<ChannelCall, RouteError>;
    /// `POST /v1/groups`: the group's `next_seq`.
    async fn post_group(&self, body: Vec<u8>) -> Result<u64, RouteError>;
    async fn get_group_info(&self, group: &[u8; 16]) -> Result<Vec<u8>, RouteError>;
    async fn get_group_tree(&self, group: &[u8; 16]) -> Result<Vec<u8>, RouteError>;
    /// `POST /v1/groups/{id}/resync`: the commit's `seq`.
    async fn post_resync(&self, group: &[u8; 16], body: Vec<u8>) -> Result<u64, RouteError>;
    /// `POST /v1/groups/{id}/commit`: the commit's `seq`.
    async fn post_commit(&self, group: &[u8; 16], body: Vec<u8>) -> Result<u64, RouteError>;
    /// `POST /v1/groups/{id}/proposal`: the proposal's `seq`.
    async fn post_proposal(&self, group: &[u8; 16], body: Vec<u8>) -> Result<u64, RouteError>;
    /// L-HTTP-102 `POST /v1/channels/{id}/calls`.
    async fn start_call(&self, channel: &[u8; 16], vdec: &str) -> Result<CallStarted, RouteError>;
    /// `DELETE /v1/calls/{id}?if_alone=1`: true when the call ended, false when others are in it.
    async fn end_call_if_alone(&self, call: &[u8; 16]) -> Result<bool, RouteError>;
    /// L-HTTP-104 `POST /v1/calls/{id}/decline` with the ring's `expires_ts`.
    async fn decline_call(&self, call: &[u8; 16], ring_ts: u64) -> Result<(), RouteError>;
}

/// [`CallRoutes`] over the client's [`Http`] seam and its typed route table.
pub struct HttpRoutes<H>(pub H);

fn hex(b: &[u8]) -> String {
    b.iter().map(|x| format!("{x:02x}")).collect()
}

#[async_trait::async_trait]
impl<H: Http> CallRoutes for HttpRoutes<H> {
    async fn get_channel_call(&self, channel: &[u8; 16]) -> Result<ChannelCall, RouteError> {
        use crate::http::routes::{GetChannelCall, NoBody};
        let raw = self
            .0
            .call::<GetChannelCall>(&[&hex(channel)], NoBody)
            .await?;
        wire::decode_channel_call(&raw.0)
    }
    async fn post_group(&self, body: Vec<u8>) -> Result<u64, RouteError> {
        use crate::http::routes::{PostGroup, Raw};
        let raw = self.0.call::<PostGroup>(&[], Raw(body)).await?;
        wire::decode_seq(&raw.0, 1)
    }
    async fn get_group_info(&self, group: &[u8; 16]) -> Result<Vec<u8>, RouteError> {
        use crate::http::routes::{GetGroupInfo, NoBody};
        Ok(self.0.call::<GetGroupInfo>(&[&hex(group)], NoBody).await?.0)
    }
    async fn get_group_tree(&self, group: &[u8; 16]) -> Result<Vec<u8>, RouteError> {
        use crate::http::routes::{GetGroupTree, NoBody};
        Ok(self.0.call::<GetGroupTree>(&[&hex(group)], NoBody).await?.0)
    }
    async fn post_resync(&self, group: &[u8; 16], body: Vec<u8>) -> Result<u64, RouteError> {
        use crate::http::routes::{PostResync, Raw};
        let raw = self.0.call::<PostResync>(&[&hex(group)], Raw(body)).await?;
        wire::decode_seq(&raw.0, 0)
    }
    async fn post_commit(&self, group: &[u8; 16], body: Vec<u8>) -> Result<u64, RouteError> {
        use crate::http::routes::{PostCommit, Raw};
        let raw = self.0.call::<PostCommit>(&[&hex(group)], Raw(body)).await?;
        wire::decode_seq(&raw.0, 0)
    }
    async fn post_proposal(&self, group: &[u8; 16], body: Vec<u8>) -> Result<u64, RouteError> {
        use crate::http::routes::{PostProposal, Raw};
        let raw = self
            .0
            .call::<PostProposal>(&[&hex(group)], Raw(body))
            .await?;
        wire::decode_seq(&raw.0, 0)
    }
    async fn start_call(&self, channel: &[u8; 16], vdec: &str) -> Result<CallStarted, RouteError> {
        use crate::http::routes::{Raw, StartCall};
        let reply = self
            .0
            .call::<StartCall>(&[&hex(channel)], Raw(wire::start_call_body(vdec)))
            .await?;
        wire::decode_call_started(reply.status, &reply.body)
    }
    async fn end_call_if_alone(&self, call: &[u8; 16]) -> Result<bool, RouteError> {
        use crate::http::routes::{EndCallIfAlone, NoBody};
        match self.0.call::<EndCallIfAlone>(&[&hex(call)], NoBody).await {
            Ok(()) => Ok(true),
            // 409 E_CALL_NOT_ALONE is the only 409 of this route.
            Err(HttpError::Status(409)) => Ok(false),
            Err(e) => Err(e.into()),
        }
    }
    async fn decline_call(&self, call: &[u8; 16], ring_ts: u64) -> Result<(), RouteError> {
        use crate::http::routes::{DeclineCall, Raw};
        let mut e = dilla_core::cbor::Encoder::new();
        e.array(1).uint(ring_ts);
        Ok(self
            .0
            .call::<DeclineCall>(&[&hex(call)], Raw(e.into_vec()))
            .await?)
    }
}
