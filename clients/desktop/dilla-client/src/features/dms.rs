//! Area **dms** (Task 13): open or create a DM, the DM list and DM unread.
//!
//! Ports: the DM list and DmPicker in packages/web/src/screens/Shell.tsx, and controller.ts's DM half (`reloadDms`,
//! `loadDms`, `publishDms`, `openDm`, a DM's `openChannel`).
//!
//! The list is `GET /v1/dms`, read on every ready, after an open, and at an interval while online (the engine keeps a
//! Welcome for a group it does not expect without saying so; the web reloads its DMs on that signal, see the task
//! report). A DM is a text channel of no community: its group is opened through the sync engine as a channel's is, its
//! timeline is `AppState::timelines` under its channel id and its badge `AppState::unread` under the same id (the
//! publisher reads both for every kind-0 group, a DM's included).

use std::sync::Arc;

use tokio::sync::{oneshot, watch};

use crate::client::{Client, ClientError, Inner};
use crate::gateway::frames::Value;
use crate::state::dms::{DmSummary, decode_dm_opened, decode_dms, dm_name};
use crate::state::unread::BadgeState;
use crate::state::{AppState, ChannelId, MessageId, UserId};

use super::trust::{dm_group_state, memory, name_of};

/// The dms area's handle for dms: cheap to clone, no I/O on construction.
#[derive(Clone)]
pub struct Dms {
    inner: Arc<Inner>,
    state: watch::Receiver<Arc<AppState>>,
}

impl Client {
    /// The [`Dms`] handle.
    pub fn dms(&self) -> Dms {
        Dms {
            inner: self.inner().clone(),
            state: self.state(),
        }
    }
}

/// controller.ts `KnownDm`: a DM the instance lists, and its text group once known.
#[derive(Clone, Debug, PartialEq, Eq)]
pub(crate) struct KnownDm {
    pub channel: [u8; 16],
    pub kind: u8,
    pub members: Vec<[u8; 16]>,
    pub group: Option<[u8; 16]>,
}

fn net(e: crate::sync::api::ApiError) -> ClientError {
    if e.status == 0 && e.code == "E_NETWORK" {
        ClientError::Network(e.detail)
    } else {
        ClientError::Sync(e.code)
    }
}

fn idem() -> Vec<u8> {
    let mut b = vec![0u8; 16];
    let _ = getrandom::fill(&mut b);
    b
}

/// The total unread and mention counts of every DM (the Home tile's badge, `home.tileLabel`).
pub fn home_counts(s: &AppState) -> BadgeState {
    let mut total = BadgeState::default();
    for dm in s.dms.all() {
        if let Some(b) = s.unread.get(&dm.id) {
            total.unread += b.unread;
            total.mentions += b.mentions;
        }
    }
    total
}

/// DmPicker's members (Shell.tsx `pickerMembers`): everyone in the selected server but this account, by name, or every
/// server's members when none is selected.
pub fn picker_members(
    s: &AppState,
    community: Option<crate::state::CommunityId>,
) -> Vec<crate::state::Member> {
    let own = s.account.user_id;
    let mut out: Vec<crate::state::Member> = Vec::new();
    let lists: Vec<&Vec<crate::state::Member>> = match community {
        Some(c) => s.members.get(&c).into_iter().collect(),
        None => s
            .communities
            .iter()
            .filter_map(|c| s.members.get(&c.id))
            .collect(),
    };
    for list in lists {
        for m in list {
            if Some(m.user_id) != own && m.kind == 0 && !out.iter().any(|o| o.user_id == m.user_id)
            {
                out.push(m.clone());
            }
        }
    }
    out
}

/// The DmPicker's type-ahead: the index of the first member at or after `from` (wrapping) whose shown name starts with
/// `prefix`, ignoring case.
pub fn type_ahead(names: &[String], prefix: &str, from: usize) -> Option<usize> {
    if prefix.is_empty() || names.is_empty() {
        return None;
    }
    let prefix = prefix.to_lowercase();
    (0..names.len())
        .map(|i| (from + i) % names.len())
        .find(|&i| names[i].to_lowercase().starts_with(&prefix))
}

impl Dms {
    /// Starts the area's watcher (the DM list and the trust slice), once per client. Idempotent.
    pub fn start(&self) {
        super::trust::start(&self.inner, &self.state);
    }

    /// Reads the DM list again.
    pub fn reload(&self) -> oneshot::Receiver<Result<(), ClientError>> {
        let inner = self.inner.clone();
        self.inner.spawn(async move { reload(&inner).await })
    }

    /// controller.ts `openDm`: the DM with `user` (made if there is none: `POST /v1/dms`), listed before it answers.
    pub fn open_dm(&self, user: UserId) -> oneshot::Receiver<Result<ChannelId, ClientError>> {
        let inner = self.inner.clone();
        self.inner.spawn(async move {
            let body =
                Value::Array(vec![Value::Array(vec![Value::Bytes(user.0.to_vec())])]).encode();
            let answer = inner
                .socket_requests()
                .request("POST", "/v1/dms".to_owned(), body, Some(idem()))
                .await
                .map_err(net)?;
            if answer.status != 200 && answer.status != 201 {
                return Err(net(crate::sync::api::ApiError::from_body(
                    answer.status,
                    &answer.body,
                )));
            }
            let channel =
                decode_dm_opened(&answer.body).map_err(|e| ClientError::Sync(e.to_string()))?;
            reload(&inner).await?;
            Ok(ChannelId(channel))
        })
    }

    /// Opens the DM's text group (join, or register for the first one in), so its timeline is kept.
    pub fn open(&self, channel: ChannelId) -> oneshot::Receiver<Result<(), ClientError>> {
        let inner = self.inner.clone();
        self.inner
            .spawn(async move { open_group(&inner, channel).await.map(|_| ()) })
    }

    /// Queues `text` in the DM (opening its group first). The DM's timeline shows it pending until the instance stored
    /// it, as a channel's does.
    pub fn send_text(
        &self,
        channel: ChannelId,
        text: String,
    ) -> oneshot::Receiver<Result<MessageId, ClientError>> {
        let inner = self.inner.clone();
        self.inner.spawn(async move {
            let g = open_group(&inner, channel).await?;
            let msg = inner.engine().send_text(g, text).await?;
            Ok(MessageId(msg))
        })
    }
}

/// The DM's group, opened through the engine (a DM is a channel of no community).
async fn open_group(inner: &Arc<Inner>, channel: ChannelId) -> Result<[u8; 16], ClientError> {
    let memory = memory(inner);
    let mut known = memory
        .lock()
        .await
        .dms
        .iter()
        .find(|d| d.channel == channel.0)
        .cloned();
    if known.is_none() {
        reload(inner).await?;
        known = memory
            .lock()
            .await
            .dms
            .iter()
            .find(|d| d.channel == channel.0)
            .cloned();
    }
    let known = known.ok_or(ClientError::UnknownChannel)?;
    let g = inner
        .engine()
        .open_channel(None, channel.0, known.group)
        .await?;
    {
        let mut m = memory.lock().await;
        if let Some(dm) = m.dms.iter_mut().find(|d| d.channel == channel.0) {
            dm.group = Some(g);
        }
    }
    publish_dms(inner).await;
    Ok(g)
}

/// controller.ts `loadDms`: the instance's DM list, each DM's text group read once it is known, published. Every DM
/// whose group is known is opened in the background, so a DM another person opened is joined (the web's expected set).
pub(crate) async fn reload(inner: &Arc<Inner>) -> Result<(), ClientError> {
    let answer = inner
        .socket_requests()
        .request("GET", "/v1/dms".to_owned(), Vec::new(), None)
        .await
        .map_err(net)?;
    if answer.status != 200 {
        return Err(net(crate::sync::api::ApiError::from_body(
            answer.status,
            &answer.body,
        )));
    }
    let rows = decode_dms(&answer.body).map_err(|e| ClientError::Sync(e.to_string()))?;
    let memory = memory(inner);
    let before = memory.lock().await.dms.clone();
    let mut next = Vec::with_capacity(rows.len());
    for row in rows {
        let mut group = before
            .iter()
            .find(|d| d.channel == row.channel_id)
            .and_then(|d| d.group);
        if group.is_none() {
            // A failed read leaves the group unknown; the next load asks again.
            group = inner
                .api()
                .get_channel_text_group(&row.channel_id)
                .await
                .ok()
                .flatten();
        }
        next.push(KnownDm {
            channel: row.channel_id,
            kind: row.kind,
            members: row.members,
            group,
        });
    }
    let to_open: Vec<KnownDm> = {
        let mut m = memory.lock().await;
        m.dms = next.clone();
        next.into_iter()
            .filter(|d| d.group.is_some() && m.opening.insert(d.channel))
            .collect()
    };
    for dm in to_open {
        let (inner2, memory) = (inner.clone(), memory.clone());
        inner.spawn(async move {
            let opened = inner2
                .engine()
                .open_channel(None, dm.channel, dm.group)
                .await;
            memory.lock().await.opening.remove(&dm.channel);
            opened?;
            publish_dms(&inner2).await;
            Ok(())
        });
    }
    publish_dms(inner).await;
    // Product P1: a DM is a shared group for its members, so the trust slice is read again with it.
    let _ = super::trust::publish_trust(inner).await;
    Ok(())
}

/// controller.ts `publishDms`: the list as the UI shows it, named from the member lists.
pub(crate) async fn publish_dms(inner: &Arc<Inner>) {
    let s = inner.snapshot();
    let groups = inner.engine().groups().await;
    let dms = memory(inner).lock().await.dms.clone();
    let own = s.account.user_id;
    let list: Vec<DmSummary> = dms
        .iter()
        .map(|dm| {
            let members: Vec<UserId> = dm.members.iter().copied().map(UserId).collect();
            DmSummary {
                id: ChannelId(dm.channel),
                kind: dm.kind,
                name: dm_name(&members, own, &s.account.username, |u| name_of(&s, u)),
                members,
                group: dm_group_state(ChannelId(dm.channel), &groups),
            }
        })
        .collect();
    inner.publish(move |state| {
        if state.dms.list.as_ref() != Some(&list) {
            state.dms.list = Some(list);
        }
    });
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn type_ahead_wraps_and_ignores_case() {
        let names: Vec<String> = ["Ada", "bo", "Bea", "Cy"]
            .iter()
            .map(|s| (*s).to_owned())
            .collect();
        assert_eq!(type_ahead(&names, "b", 0), Some(1));
        assert_eq!(type_ahead(&names, "b", 2), Some(2));
        assert_eq!(type_ahead(&names, "B", 3), Some(1));
        assert_eq!(type_ahead(&names, "be", 0), Some(2));
        assert_eq!(type_ahead(&names, "z", 0), None);
        assert_eq!(type_ahead(&names, "", 0), None);
    }

    #[test]
    fn home_counts_sum_only_dms() {
        let dm = ChannelId([1; 16]);
        let mut s = AppState::default();
        s.dms.list = Some(vec![DmSummary {
            id: dm,
            kind: 3,
            members: vec![],
            name: "x".into(),
            group: crate::state::ChannelGroupState::Active,
        }]);
        s.unread.insert(
            dm,
            BadgeState {
                unread: 3,
                mentions: 1,
            },
        );
        s.unread.insert(
            ChannelId([2; 16]),
            BadgeState {
                unread: 5,
                mentions: 0,
            },
        );
        assert_eq!(
            home_counts(&s),
            BadgeState {
                unread: 3,
                mentions: 1
            }
        );
    }
}
