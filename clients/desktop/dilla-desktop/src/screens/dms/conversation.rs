//! A conversation's trust surfaces (Shell.tsx, Flow 05 "changed", W-01): over the log, the own-account banner when a
//! device of this account is under another identity or unlisted, the removed note when a validated Remove of a changed
//! contact's device restored sending here, and one alert per changed member of the conversation (no dismiss, until the
//! key is verified or a validated Remove); and the composer's pause with its reason.
//!
//! A DM conversation's log and composer are Task 9's views, as a channel's; this file only adds what trust puts around
//! them. Every decision is `AppState::trust`'s (the core's verdicts).

use dilla_client::state::trust::TrustState;
use dilla_client::state::{AppState, ChannelId, UserId};
use gpui::{AnyElement, App, IntoElement, Window};

use dilla_desktop_ui::components::banner::{Banner, BannerTone};
use dilla_desktop_ui::components::dms::{TrustAlert, removed_note};

use crate::slots::SlotCx;
use crate::strings::{t, tf};

use super::{Nav, navigate};

/// Who is in the conversation: a channel's community members, a DM's members.
pub fn audience(state: &AppState, channel: ChannelId) -> Vec<UserId> {
    if let Some(c) = state.channel(channel) {
        return state
            .members
            .get(&c.community_id)
            .map(|m| m.iter().map(|m| m.user_id).collect())
            .unwrap_or_default();
    }
    state
        .dms
        .get(channel)
        .map(|d| d.members.clone())
        .unwrap_or_default()
}

/// Why the composer is paused in `channel`, translated, or `None` while sending is open (Shell.tsx `blockReason` of the
/// core's send gate; before the core first answered, a changed member blocks it).
pub fn composer_block(state: &AppState, channel: ChannelId) -> Option<&'static str> {
    let trust: &TrustState = &state.trust;
    trust
        .block_reason(channel, &audience(state, channel))
        .map(t)
}

/// What trust draws over the conversation's log, in order.
pub fn banners(ctx: &SlotCx, _window: &mut Window, _cx: &mut App) -> Vec<AnyElement> {
    let mut out = Vec::new();
    let state = &ctx.state;
    if let Some(client) = &ctx.client {
        client.trust().start();
    }
    if let Some(own) = state.trust.own {
        let copy = match own {
            dilla_client::state::trust::OwnTrust::Changed => "privacy.ownChanged",
            dilla_client::state::trust::OwnTrust::Unlisted => "privacy.ownUnlisted",
        };
        out.push(
            Banner::new("trust-own", BannerTone::Danger, t(copy))
                .action(t("privacy.ownAction"), |_, window, cx| {
                    navigate(Nav::Devices, window, cx);
                })
                .into_any_element(),
        );
    }
    let Some(channel) = ctx.channel else {
        return out;
    };
    if state.trust.removed_here(channel) {
        out.push(removed_note(t("privacy.removed")).into_any_element());
    }
    for (i, row) in state
        .trust
        .changed_in(channel, &audience(state, channel))
        .into_iter()
        .enumerate()
    {
        let copy = if row.changed_umk.is_some() {
            "privacy.changed"
        } else {
            "privacy.changedNoKey"
        };
        let mut alert = TrustAlert::new(("trust-conversation", i), row.name.clone(), t(copy));
        if row.changed_umk.is_some() {
            alert = alert.compare(
                tf("privacy.compare", &[("name", &row.name)]),
                |_, window, cx| navigate(Nav::Privacy, window, cx),
            );
        }
        out.push(alert.into_any_element());
    }
    out
}
