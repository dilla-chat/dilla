//! [`BotIo`] over a running client: the core (key check, pins, seal) and the instance's routes, as controller.ts wires
//! `BotCatalogStore` and `BotCommandRelay`.

use std::sync::{Arc, Weak};

use async_trait::async_trait;
use dilla_core::bot::command::ReplySecret;
use dilla_core::bot::device_list_push::{DeviceListPush, DeviceListRow};

use super::sealed::{BotIo, Candidate, Refusal};
use crate::client::Inner;
use crate::gateway::frames::Value;
use crate::state::bot::BotCatalogView;
use crate::state::{ChannelId, CommunityId};
use crate::sync::api::{ApiError, try_core};
use crate::sync::trust::{head_attestation, read_device_list_history};

pub(crate) struct InnerIo {
    pub(crate) inner: Weak<Inner>,
}

fn now_s() -> u64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map_or(0, |d| d.as_secs())
}

fn api_refusal(e: &ApiError) -> Refusal {
    Refusal::new(&e.code, e.status)
}

fn stopped() -> Refusal {
    Refusal::new("E_NOT_READY", 0)
}

impl InnerIo {
    fn inner(&self) -> Result<Arc<Inner>, Refusal> {
        self.inner.upgrade().ok_or_else(stopped)
    }

    /// Runs `f` on the core; a refusal keeps its code.
    async fn core<T: Send + 'static>(
        &self,
        f: impl FnOnce(
            &mut dilla_core::client::ClientCore,
        ) -> Result<T, dilla_core::client::ClientError>
        + Send
        + 'static,
    ) -> Result<T, Refusal> {
        let inner = self.inner()?;
        let core = inner.engine().core().clone();
        try_core(&core, f)
            .await
            .map_err(|_| Refusal::new("E_CORE_POISONED", 0))?
            .map_err(|e| Refusal::new(e.code, 0))
    }

    async fn get(&self, path: String) -> Result<Vec<u8>, Refusal> {
        let inner = self.inner()?;
        let answer = inner
            .socket_requests()
            .request("GET", path, Vec::new().into(), None)
            .await
            .map_err(|e| api_refusal(&e))?;
        if answer.status != 200 {
            return Err(api_refusal(&ApiError::from_body(
                answer.status,
                &answer.body,
            )));
        }
        Ok(answer.body)
    }
}

fn uint(v: &Value) -> Option<u64> {
    v.as_uint()
}

fn id16(v: &Value) -> Option<[u8; 16]> {
    v.as_bytes()?.try_into().ok()
}

/// routes.ts `getBotCommandStatus`: `[1, installation_id, bot_user_id | null, state 1..=4, revision]`, its Bot user.
fn command_status_bot_user(body: &[u8]) -> Option<[u8; 16]> {
    let v = Value::decode(body).ok()?;
    let a = v.as_array().filter(|a| a.len() == 5)?;
    if a[0].as_uint() != Some(1) || id16(&a[1]).is_none() || !matches!(a[3].as_uint(), Some(1..=4))
    {
        return None;
    }
    id16(&a[2]).filter(|u| *u != [0; 16])
}

/// trust.ts `readBotDeviceListPush`: the head's identity keys and every signed row from version 1.
fn bot_push(head: &[u8], history: &[u8]) -> Option<Vec<u8>> {
    let row = Value::decode(head).ok()?;
    let row = row.as_array()?;
    if row.len() != 8 {
        return None;
    }
    let history = Value::decode(history).ok()?;
    let rows = history
        .as_array()?
        .iter()
        .map(|r| {
            let r = r.as_array()?;
            Some(DeviceListRow {
                version: r.first()?.as_uint()?,
                blob: r.get(1)?.as_bytes()?.to_vec(),
                ssk_signature: r.get(2)?.as_bytes()?.try_into().ok()?,
                prev_hash: r.get(3)?.as_bytes()?.try_into().ok()?,
            })
        })
        .collect::<Option<Vec<_>>>()?;
    Some(
        DeviceListPush {
            umk_pub: row[5].as_bytes()?.try_into().ok()?,
            ssk_pub: row[6].as_bytes()?.try_into().ok()?,
            sig_umk_ssk: row[7].as_bytes()?.try_into().ok()?,
            history: rows,
        }
        .encode(),
    )
}

#[async_trait]
impl BotIo for InnerIo {
    async fn bot_user_of(&self, community: CommunityId) -> Candidate {
        let Ok(inner) = self.inner() else {
            return Candidate::None;
        };
        let snapshot = inner.snapshot();
        let bots: Vec<[u8; 16]> = snapshot
            .members
            .get(&community)
            .map(|m| {
                m.iter()
                    .filter(|m| m.kind == 1)
                    .map(|m| m.user_id.0)
                    .collect()
            })
            .unwrap_or_default();
        match bots.as_slice() {
            // controller.ts `botUserOf`: with none in the member list (not loaded yet), the command status names it.
            [] => self
                .get(format!(
                    "/v1/communities/{}/bot/command-status",
                    community.hex()
                ))
                .await
                .ok()
                .and_then(|body| command_status_bot_user(&body))
                .map_or(Candidate::None, Candidate::One),
            [one] => Candidate::One(*one),
            _ => Candidate::Ambiguous,
        }
    }

    async fn ingest_attestation(&self, user: [u8; 16]) -> Result<(), Refusal> {
        let inner = self.inner()?;
        let head = inner
            .api()
            .get_device_list(&user)
            .await
            .map_err(|e| api_refusal(&e))?
            .ok_or_else(|| Refusal::new("E_DEVICE_LIST_STALE", 0))?;
        let Ok(Some(attestation)) = head_attestation(&head.raw) else {
            return Ok(());
        };
        self.core(move |c| c.account_kind_ingest(&user, &attestation))
            .await
    }

    async fn pin_bot_user(
        &self,
        community: CommunityId,
        user: [u8; 16],
    ) -> Result<(u8, [u8; 16]), Refusal> {
        let raw = self
            .core(move |c| c.bot_user_pin(&community.0, &user))
            .await?;
        let v = Value::decode(&raw).map_err(|_| Refusal::new("E_CORE_OUTPUT", 0))?;
        match v.as_array() {
            Some([s, p]) => match (uint(s), id16(p)) {
                (Some(s), Some(p)) if s <= 2 => Ok((s as u8, p)),
                _ => Err(Refusal::new("E_CORE_OUTPUT", 0)),
            },
            _ => Err(Refusal::new("E_CORE_OUTPUT", 0)),
        }
    }

    async fn pinned_bot_user(&self, community: CommunityId) -> Option<[u8; 16]> {
        self.core(move |c| c.bot_user_pinned(&community.0))
            .await
            .ok()
            .flatten()
    }

    async fn verify(
        &self,
        publication: Vec<u8>,
        user: [u8; 16],
    ) -> Result<([u8; 16], String), Refusal> {
        let now = now_s();
        let (_, raw) = self
            .core(move |c| c.bot_command_key_verify(&publication, &user, now))
            .await?;
        let v = Value::decode(&raw).map_err(|_| Refusal::new("E_CORE_OUTPUT", 0))?;
        match v.as_array() {
            Some([k, f]) => match (id16(k), f.as_text()) {
                (Some(k), Some(f)) => Ok((k, f.to_owned())),
                _ => Err(Refusal::new("E_CORE_OUTPUT", 0)),
            },
            _ => Err(Refusal::new("E_CORE_OUTPUT", 0)),
        }
    }

    async fn pin_device_list(&self, user: [u8; 16]) -> Result<u8, Refusal> {
        let inner = self.inner()?;
        let (head, history) = read_device_list_history(inner.api(), &user, 0)
            .await
            .map_err(|_| Refusal::new("E_DEVICE_LIST_STALE", 0))?;
        let push =
            bot_push(&head, &history).ok_or_else(|| Refusal::new("E_DEVICE_LIST_STALE", 0))?;
        // The attestation goes in first: the pin refuses a user the core holds no Bot attestation for.
        if let Ok(Some(attestation)) = head_attestation(&head) {
            let _ = self
                .core(move |c| c.account_kind_ingest(&user, &attestation))
                .await;
        }
        let now = now_s();
        let raw = self
            .core(move |c| c.bot_device_list_pin(&user, &push, now))
            .await?;
        let v = Value::decode(&raw).map_err(|_| Refusal::new("E_CORE_OUTPUT", 0))?;
        match v
            .as_array()
            .and_then(|a| a.first())
            .and_then(Value::as_uint)
        {
            Some(s) if s <= 2 => Ok(s as u8),
            _ => Err(Refusal::new("E_CORE_OUTPUT", 0)),
        }
    }

    async fn seal(
        &self,
        publication: Vec<u8>,
        user: [u8; 16],
        request: zeroize::Zeroizing<Vec<u8>>,
    ) -> Result<(Vec<u8>, ReplySecret), Refusal> {
        let now = now_s();
        let (_, sealed, secret) = self
            .core(move |c| c.bot_command_seal(&publication, &user, &request, now))
            .await?;
        Ok((sealed, secret))
    }

    async fn post(
        &self,
        community: CommunityId,
        body: Vec<u8>,
        idem: Vec<u8>,
    ) -> Result<(), Refusal> {
        let inner = self.inner()?;
        let answer = inner
            .socket_requests()
            .request(
                "POST",
                format!("/v1/communities/{}/bot/commands", community.hex()),
                zeroize::Zeroizing::new(body),
                Some(idem),
            )
            .await
            .map_err(|e| api_refusal(&e))?;
        if answer.status == 202 {
            Ok(())
        } else {
            Err(api_refusal(&ApiError::from_body(
                answer.status,
                &answer.body,
            )))
        }
    }

    async fn fetch_catalog(&self, community: CommunityId) -> Result<Vec<u8>, Refusal> {
        self.get(format!("/v1/communities/{}/bot/catalog", community.hex()))
            .await
    }

    async fn fetch_bot_channels(&self, community: CommunityId) -> Result<Vec<ChannelId>, Refusal> {
        let body = self
            .get(format!("/v1/communities/{}/bot/channels", community.hex()))
            .await?;
        let v = Value::decode(&body).map_err(|_| Refusal::new("E_BOT_CATALOG", 0))?;
        v.as_array()
            .ok_or_else(|| Refusal::new("E_BOT_CATALOG", 0))?
            .iter()
            .map(|row| {
                row.as_array()
                    .filter(|r| r.len() == 3)
                    .and_then(|r| id16(&r[0]))
                    .map(ChannelId)
                    .ok_or_else(|| Refusal::new("E_BOT_CATALOG", 0))
            })
            .collect()
    }

    fn now_s(&self) -> u64 {
        // serverNowS: the instance's clock (the Bot refuses a `sent_at` more than 2 min off it), else this device's.
        self.inner
            .upgrade()
            .map_or_else(now_s, |inner| inner.api().server_now_s())
    }

    fn random16(&self) -> [u8; 16] {
        crate::features::bot_admin::new_request_id()
    }

    fn spawn(&self, task: super::sealed::Task) {
        // The client's runtime: the UI thread that asks has none of its own.
        if let Some(inner) = self.inner.upgrade() {
            drop(inner.spawn(async move {
                task.await;
                Ok(())
            }));
        }
    }

    fn publish(&self, community: CommunityId, view: Option<BotCatalogView>) {
        let Some(inner) = self.inner.upgrade() else {
            return;
        };
        // The Bot page (Task 21) reads the same verdict: its state, since, key fingerprint and Bot channels.
        let admin = view.as_ref().map(|v| {
            use crate::state::bot::BotStatusState as S;
            use crate::state::bot_admin::{BotCatalog, CatalogState as C};
            BotCatalog {
                state: match v.status.state {
                    S::Ready => C::Ready,
                    S::Degraded => C::Degraded,
                    S::Offline => C::Offline,
                    S::Unverified => C::Unverified,
                    S::Changed => C::Changed,
                    S::Disabled | S::Absent => C::Other,
                },
                since: v.status.since,
                fingerprint: v.fingerprint.clone(),
                bot_channels: v.bot_channels.clone(),
            }
        });
        let transport = crate::features::bot_admin::default_transport(&inner);
        crate::features::bot_admin::BotAdmin::new(inner.clone(), transport)
            .set_catalog(community, admin);
        inner.publish(move |s| match view {
            Some(v) => {
                s.bot.catalogs.insert(community, v);
            }
            None => {
                s.bot.catalogs.remove(&community);
            }
        });
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_command_status_names_the_bot_user() {
        let row = |user: Value, state: u64| {
            Value::Array(vec![
                Value::Uint(1),
                Value::Bytes(vec![1; 16]),
                user,
                Value::Uint(state),
                Value::Uint(3),
            ])
            .encode()
        };
        assert_eq!(
            command_status_bot_user(&row(Value::Bytes(vec![0xb0; 16]), 1)),
            Some([0xb0; 16])
        );
        assert_eq!(command_status_bot_user(&row(Value::Null, 1)), None);
        assert_eq!(
            command_status_bot_user(&row(Value::Bytes(vec![0xb0; 16]), 9)),
            None
        );
        assert_eq!(command_status_bot_user(&Value::Null.encode()), None);
    }

    #[test]
    fn a_push_needs_the_heads_identity_keys() {
        let head = Value::Array(vec![
            Value::Uint(1),
            Value::Bytes(vec![1]),
            Value::Bytes(vec![0; 64]),
            Value::Bytes(vec![0; 32]),
        ])
        .encode();
        assert!(bot_push(&head, &Value::Array(vec![]).encode()).is_none());
        let head8 = Value::Array(vec![
            Value::Uint(1),
            Value::Bytes(vec![1]),
            Value::Bytes(vec![0; 64]),
            Value::Bytes(vec![0; 32]),
            Value::Bytes(vec![9]),
            Value::Bytes(vec![1; 32]),
            Value::Bytes(vec![2; 32]),
            Value::Bytes(vec![3; 64]),
        ])
        .encode();
        let history = Value::Array(vec![Value::Array(vec![
            Value::Uint(1),
            Value::Bytes(vec![1]),
            Value::Bytes(vec![0; 64]),
            Value::Bytes(vec![0; 32]),
        ])])
        .encode();
        let push = bot_push(&head8, &history).unwrap();
        let back = DeviceListPush::decode(&push).unwrap();
        assert_eq!((back.umk_pub, back.history.len()), ([1; 32], 1));
    }
}
