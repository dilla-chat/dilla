//! Area **user settings** (Task 16): the account's profile.
//!
//! Ports: packages/web/src/screens/settings/Profile.tsx and the worker's `updateProfile` and its read of `GET
//! /v1/accounts/me` (packages/client-core/src/worker/controller.ts).
//!
//! The instance stores the display name, the status message and the bio readable. [`Profile::load`] reads them into
//! [`crate::state::profile::ProfileState::me`]; [`Profile::update`] checks each given field with the shared rule
//! ([`text`]), sends only those fields, and on success shows the saved text on the account and on the own row of every loaded
//! member list, with no read (the instance stored what was sent, trimmed).

pub mod tables;
pub mod text;

use std::sync::Arc;

use tokio::sync::oneshot;

use crate::client::{Client, ClientError, Inner};
use crate::state::profile::{
    DensitySetting, KEY_DENSITY, KEY_REDUCE_MOTION, MotionSetting, MyProfile, PresenceChoice,
};
use crate::sync::api::{ApiError, try_core};
use crate::sync::value::{self, at_least, text as text_of};

pub use text::{
    BIO_MAX_CHARS, DISPLAY_MAX_CHARS, ProfileTextProblem, STATUS_MAX_CHARS, profile_text_problem,
    profile_text_value,
};

/// The profile fields a save changes: a field left `None` is left alone, `Some("")` clears it.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct ProfilePatch {
    pub display: Option<String>,
    pub status_msg: Option<String>,
    pub bio: Option<String>,
}

impl ProfilePatch {
    pub fn is_empty(&self) -> bool {
        self.display.is_none() && self.status_msg.is_none() && self.bio.is_none()
    }

    /// The patch as it is sent and stored: every field NFC, trimmed. Refused with `E_BAD_INPUT` when a field fails the rule
    /// (the worker's `updateProfile` validation).
    pub fn checked(&self) -> Result<ProfilePatch, ClientError> {
        let one = |v: &Option<String>, max: usize, multiline: bool| match v {
            None => Ok(None),
            Some(v) => match profile_text_problem(v, max, multiline) {
                Some(_) => Err(ClientError::Sync("E_BAD_INPUT".into())),
                None => Ok(Some(profile_text_value(v, multiline))),
            },
        };
        Ok(ProfilePatch {
            display: one(&self.display, DISPLAY_MAX_CHARS, false)?,
            status_msg: one(&self.status_msg, STATUS_MAX_CHARS, false)?,
            bio: one(&self.bio, BIO_MAX_CHARS, true)?,
        })
    }

    /// routes.ts `patchMe`: `[display|null, status_msg|null, bio|null]`.
    fn body(&self) -> Vec<u8> {
        let mut e = dilla_core::cbor::Encoder::new();
        e.array(3);
        for field in [&self.display, &self.status_msg, &self.bio] {
            match field {
                Some(v) => e.text(v),
                None => e.null(),
            };
        }
        e.into_vec()
    }
}

/// The reason a refused call carries: its stable code, or the network when there was no answer.
pub(crate) fn refusal(e: ApiError) -> ClientError {
    if e.status == 0 && e.code == "E_NETWORK" {
        ClientError::Network(e.detail)
    } else {
        ClientError::Sync(e.code)
    }
}

/// The user settings area's handle for profile: cheap to clone, no I/O on construction.
#[derive(Clone)]
pub struct Profile {
    inner: Arc<Inner>,
}

impl Client {
    /// The [`Profile`] handle.
    pub fn profile(&self) -> Profile {
        Profile {
            inner: self.inner().clone(),
        }
    }
}

/// `GET /v1/accounts/me`: `[user_id, username, display, kind, flags, _, presence, status_msg, bio]`: five elements at least,
/// the status message (index 7) and the bio (index 8) optional (`getAccountMe` in routes.ts).
pub(crate) fn decode_me(raw: &[u8]) -> Result<MyProfile, ApiError> {
    let fail = |e: value::DecodeError| ApiError::local(0, "E_DECODE", e.0);
    let v = value::decode(raw).map_err(fail)?;
    let a = at_least(&v, 5).map_err(fail)?;
    Ok(MyProfile {
        display: text_of(&a[2]).map_err(fail)?,
        status_msg: match a.get(7) {
            Some(v) => text_of(v).map_err(fail)?,
            None => String::new(),
        },
        bio: match a.get(8) {
            Some(v) => text_of(v).map_err(fail)?,
            None => String::new(),
        },
    })
}

/// `GET /v1/accounts/me` index 6, the account's presence choice (routes.ts `getAccountMe`: `a.length > 6 ? a[6] : 1`).
pub(crate) fn decode_presence_choice(raw: &[u8]) -> Result<PresenceChoice, ApiError> {
    let fail = |e: value::DecodeError| ApiError::local(0, "E_DECODE", e.0);
    let v = value::decode(raw).map_err(fail)?;
    let a = at_least(&v, 5).map_err(fail)?;
    Ok(match a.get(6) {
        Some(v) => PresenceChoice::of_wire(value::u53(v).map_err(fail)?),
        None => PresenceChoice::Online,
    })
}

/// Applies a read presence choice unless a choice was sent since the read began (controller.ts
/// `refreshPresenceChoice`: a write in flight or landed wins over an older read).
fn apply_read_choice(s: &mut crate::state::AppState, writes: u64, choice: PresenceChoice) {
    if s.profile.presence_writes == writes {
        s.profile.presence_choice = Some(choice);
    }
}

/// controller.ts `refreshPresenceChoice`: reads the account's presence choice into the slice (at each ready, and when the
/// instance tells this account's devices its presence changed). A failure keeps what is held: the next ready reads again.
pub(crate) fn refresh_presence_choice(inner: &Arc<Inner>) {
    let reading = inner.clone();
    drop(inner.spawn(async move {
        let writes = reading.snapshot().profile.presence_writes;
        let raw = reading.api().get_account_me().await.map_err(refusal)?;
        let choice = decode_presence_choice(&raw).map_err(refusal)?;
        reading.publish(move |s| apply_read_choice(s, writes, choice));
        Ok(())
    }));
}

impl Profile {
    /// Reads the account's profile (and its presence choice) from the instance into the slice.
    pub fn load(&self) -> oneshot::Receiver<Result<(), ClientError>> {
        let inner = self.inner.clone();
        self.inner.spawn(async move {
            let snapshot = inner.snapshot();
            let (saves, writes) = (snapshot.profile.saves, snapshot.profile.presence_writes);
            drop(snapshot);
            let raw = inner.api().get_account_me().await.map_err(refusal)?;
            let me = decode_me(&raw).map_err(refusal)?;
            let choice = decode_presence_choice(&raw).map_err(refusal)?;
            inner.publish(move |s| {
                // A profile saved while this read was in flight is newer than what the read saw: keep the saved text.
                // Otherwise the read replaces the whole profile (a partial one included).
                if s.profile.saves == saves || s.profile.me.is_none() {
                    s.profile.me = Some(me);
                }
                apply_read_choice(s, writes, choice);
            });
            Ok(())
        })
    }

    /// controller.ts `setPresenceChoice`: sends the account's presence choice (the user panel's status menu) and shows it
    /// once the instance took it, unless another choice was sent meanwhile. A refusal leaves the held choice as it was.
    pub fn set_presence_choice(
        &self,
        choice: PresenceChoice,
    ) -> oneshot::Receiver<Result<(), ClientError>> {
        let inner = self.inner.clone();
        self.inner.spawn(async move {
            let (tx, rx) = oneshot::channel();
            inner.publish(move |s| {
                s.profile.presence_writes += 1;
                let _ = tx.send(s.profile.presence_writes);
            });
            let write = rx.await.unwrap_or(0);
            inner
                .api()
                .set_presence_choice(choice.wire())
                .await
                .map_err(refusal)?;
            inner.publish(move |s| {
                if s.profile.presence_writes == write {
                    s.profile.presence_choice = Some(choice);
                }
            });
            Ok(())
        })
    }

    /// Saves the given fields (`E_BAD_INPUT` when one fails the rule, `E_RATE_LIMITED` when the instance asks to wait). Only
    /// the fields given are sent; nothing is sent for an empty patch.
    pub fn update(&self, patch: ProfilePatch) -> oneshot::Receiver<Result<(), ClientError>> {
        let inner = self.inner.clone();
        self.inner.spawn(async move {
            let saved = patch.checked()?;
            if saved.is_empty() {
                return Ok(());
            }
            inner.api().patch_me(saved.body()).await.map_err(refusal)?;
            let me_id = inner.snapshot().account.user_id;
            inner.publish(move |s| {
                let me = s.profile.me.get_or_insert_with(MyProfile::default);
                if let Some(v) = &saved.display {
                    me.display.clone_from(v);
                }
                if let Some(v) = &saved.status_msg {
                    me.status_msg.clone_from(v);
                }
                if let Some(v) = &saved.bio {
                    me.bio.clone_from(v);
                }
                s.profile.saves += 1;
                // The own row of every loaded member list shows the saved text at once.
                if let Some(me_id) = me_id {
                    for list in s.members.values_mut() {
                        for m in list.iter_mut().filter(|m| m.user_id == me_id) {
                            if let Some(v) = &saved.display {
                                m.display.clone_from(v);
                            }
                            if let Some(v) = &saved.status_msg {
                                m.status_msg.clone_from(v);
                            }
                            if let Some(v) = &saved.bio {
                                m.bio.clone_from(v);
                            }
                        }
                    }
                }
            });
            Ok(())
        })
    }
}

/// The store's `app_settings` rows (`[[key, value], ...]`) as the appearance settings they hold; an unknown or refused value
/// reads as the default (`pageSettings`).
pub(crate) fn appearance_of(raw: &[u8]) -> (DensitySetting, MotionSetting) {
    let mut density = DensitySetting::default();
    let mut motion = MotionSetting::default();
    if let Ok(v) = value::decode(raw)
        && let Some(rows) = v.as_array()
    {
        for row in rows {
            let Some([k, v]) = row.as_array().and_then(|r| <&[_; 2]>::try_from(r).ok()) else {
                continue;
            };
            match (k.as_text(), v.as_text()) {
                (Some(KEY_DENSITY), Some(v)) => {
                    density = DensitySetting::from_id(v).unwrap_or_default();
                }
                (Some(KEY_REDUCE_MOTION), Some(v)) => {
                    motion = MotionSetting::from_id(v).unwrap_or_default();
                }
                _ => {}
            }
        }
    }
    (density, motion)
}

impl Profile {
    /// Reads the appearance settings the store keeps into the slice.
    pub fn load_appearance(&self) -> oneshot::Receiver<Result<(), ClientError>> {
        let inner = self.inner.clone();
        self.inner.spawn(async move {
            let raw = try_core(inner.engine().core(), |c| c.settings())
                .await
                .map_err(|_| ClientError::Sync("E_CORE_POISONED".into()))?
                .map_err(|e| ClientError::Sync(e.code.to_owned()))?;
            let (density, motion) = appearance_of(&raw);
            inner.publish(move |s| {
                s.profile.density = density;
                s.profile.reduce_motion = motion;
            });
            Ok(())
        })
    }

    /// `setSetting density`: kept in the store, shown at once.
    pub fn set_density(
        &self,
        density: DensitySetting,
    ) -> oneshot::Receiver<Result<(), ClientError>> {
        self.put_setting(KEY_DENSITY, density.id(), move |s| {
            s.profile.density = density;
        })
    }

    /// `setSetting reduceMotion`.
    pub fn set_reduce_motion(
        &self,
        motion: MotionSetting,
    ) -> oneshot::Receiver<Result<(), ClientError>> {
        self.put_setting(KEY_REDUCE_MOTION, motion.id(), move |s| {
            s.profile.reduce_motion = motion;
        })
    }

    fn put_setting(
        &self,
        key: &'static str,
        value: &'static str,
        apply: impl FnOnce(&mut crate::state::AppState) + Send + 'static,
    ) -> oneshot::Receiver<Result<(), ClientError>> {
        let inner = self.inner.clone();
        self.inner.spawn(async move {
            try_core(inner.engine().core(), move |c| c.setting_put(key, value))
                .await
                .map_err(|_| ClientError::Sync("E_CORE_POISONED".into()))?
                .map_err(|e| ClientError::Sync(e.code.to_owned()))?;
            inner.publish(apply);
            Ok(())
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn me_with(choice: Option<u64>) -> Vec<u8> {
        let mut e = dilla_core::cbor::Encoder::new();
        e.array(if choice.is_some() { 9 } else { 5 });
        e.bytes(&[1; 16]).text("ada").text("Ada").uint(0).uint(0);
        if let Some(c) = choice {
            e.uint(0).uint(c).text("").text("");
        }
        e.into_vec()
    }

    #[test]
    fn the_presence_choice_is_index_six_and_online_when_absent_or_unknown() {
        assert_eq!(
            decode_presence_choice(&me_with(Some(3))).unwrap(),
            PresenceChoice::Dnd
        );
        assert_eq!(
            decode_presence_choice(&me_with(Some(4))).unwrap(),
            PresenceChoice::Invisible
        );
        assert_eq!(
            decode_presence_choice(&me_with(None)).unwrap(),
            PresenceChoice::Online
        );
        assert_eq!(
            decode_presence_choice(&me_with(Some(9))).unwrap(),
            PresenceChoice::Online
        );
        for c in PresenceChoice::ALL {
            assert_eq!(PresenceChoice::of_wire(u64::from(c.wire())), c);
            assert_eq!(PresenceChoice::from_id(c.id()), Some(c));
        }
    }

    #[test]
    fn a_read_that_began_before_a_choice_was_sent_is_not_applied() {
        let mut s = crate::state::AppState::default();
        apply_read_choice(&mut s, 0, PresenceChoice::Idle);
        assert_eq!(s.profile.presence_choice, Some(PresenceChoice::Idle));
        s.profile.presence_writes = 1;
        apply_read_choice(&mut s, 0, PresenceChoice::Online);
        assert_eq!(
            s.profile.presence_choice,
            Some(PresenceChoice::Idle),
            "the older read lost"
        );
    }

    #[test]
    fn appearance_reads_the_two_settings_and_defaults_the_rest() {
        let mut e = dilla_core::cbor::Encoder::new();
        e.array(3);
        e.array(2).text("density").text("compact");
        e.array(2).text("notify.default").text("all");
        e.array(2).text("reduceMotion").text("off");
        let (d, m) = appearance_of(&e.into_vec());
        assert_eq!((d, m), (DensitySetting::Compact, MotionSetting::Off));
        // `cozy` is not a setting value (settings.ts isSettingValue): it reads as the default.
        let mut e = dilla_core::cbor::Encoder::new();
        e.array(1);
        e.array(2).text("density").text("cozy");
        assert_eq!(appearance_of(&e.into_vec()).0, DensitySetting::Regular);
    }

    #[test]
    fn a_patch_is_checked_and_stored_trimmed() {
        let patch = ProfilePatch {
            display: Some("  Ada  ".into()),
            status_msg: Some(String::new()),
            bio: Some("one\r\ntwo ".into()),
        };
        let saved = patch.checked().unwrap();
        assert_eq!(saved.display.as_deref(), Some("Ada"));
        assert_eq!(saved.status_msg.as_deref(), Some(""));
        assert_eq!(saved.bio.as_deref(), Some("one\ntwo"));
    }

    #[test]
    fn a_field_that_fails_the_rule_refuses_the_whole_patch() {
        let patch = ProfilePatch {
            display: Some("a".repeat(65)),
            ..ProfilePatch::default()
        };
        assert!(matches!(patch.checked(), Err(ClientError::Sync(c)) if c == "E_BAD_INPUT"));
    }

    #[test]
    fn the_body_is_display_status_bio_with_null_for_a_field_left_alone() {
        let patch = ProfilePatch {
            status_msg: Some("away".into()),
            ..ProfilePatch::default()
        };
        let body = patch.body();
        let v = crate::gateway::frames::Value::decode(&body).unwrap();
        let a = v.as_array().unwrap();
        assert_eq!(a.len(), 3);
        assert!(matches!(a[0], crate::gateway::frames::Value::Null));
        assert_eq!(a[1].as_text(), Some("away"));
        assert!(matches!(a[2], crate::gateway::frames::Value::Null));
    }

    #[test]
    fn the_account_read_takes_status_and_bio_from_a_nine_element_row() {
        let mut e = dilla_core::cbor::Encoder::new();
        e.array(9)
            .bytes(&[7; 16])
            .text("ada")
            .text("Ada")
            .uint(0)
            .uint(0)
            .uint(0)
            .uint(1)
            .text("away")
            .text("bio\nline");
        let me = decode_me(&e.into_vec()).unwrap();
        assert_eq!(
            (me.display.as_str(), me.status_msg.as_str(), me.bio.as_str()),
            ("Ada", "away", "bio\nline")
        );
    }

    #[test]
    fn the_account_read_tolerates_the_short_form() {
        let mut e = dilla_core::cbor::Encoder::new();
        e.array(5)
            .bytes(&[7; 16])
            .text("ada")
            .text("Ada")
            .uint(0)
            .uint(0);
        let me = decode_me(&e.into_vec()).unwrap();
        assert_eq!(me.display, "Ada");
        assert_eq!(me.status_msg, "");
    }
}
