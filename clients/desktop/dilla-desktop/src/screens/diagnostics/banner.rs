//! The banners over the shell (`slots::banners`): the offline link line, and the version refusals that keep the work.
//!
//! packages/web Shell.tsx and desktop.ts: while the instance refuses this client, the link line is not drawn (the refusal
//! says why the link is down) and a warn banner takes its place:
//!
//! - `VersionRefused { upgrade: true }` (the instance speaks only newer wires, or said E_UPGRADE_REQUIRED): "Update dilla to
//!   continue" with the Update button. The app never reloads or reconnects by itself: an open composer, a half-written edit
//!   and the route all stay where they are, which is what `e2e/web/version-reload.spec.ts` asserts for the desktop app.
//! - `VersionRefused { upgrade: false }` (a plain 4006 `E_VERSION`): "This server needs a newer client", with Reload, which
//!   here reconnects (a native window has no page to reload, and nothing the person typed is touched).
//!
//! Which refusal an `upgrade: false` was (the client is too old for this instance, or the instance is older than the
//! client, web's `reason: 'instance'`) is not told apart by `AccountPhase`; `shell.banner.instanceTooOld` waits for a
//! variant for it (Task 2 owns `client.rs`).

use dilla_client::state::{AccountPhase, AppState, ConnectionStatus};
use gpui::{AnyElement, App, Global, IntoElement, Window};

use dilla_desktop_ui::components::banner::{Banner, BannerTone};
use dilla_desktop_ui::components::link_banner::{LinkBanner, LinkStatus};

use crate::slots::SlotCx;
use crate::strings::{t, tf};
use crate::tracked_box;

/// What the update banner says (desktop.ts `UpgradeRequiredSurface.message`).
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub enum UpdateMessage {
    /// "Update dilla to continue".
    #[default]
    Required,
    /// "The update did not install. Try again."
    Failed,
    /// "No update is ready for this app yet. Try again later."
    Unavailable,
    /// "Update dilla with your package manager to continue."
    Manual,
}

impl UpdateMessage {
    pub fn key(self) -> &'static str {
        match self {
            Self::Required => "update.required",
            Self::Failed => "update.requiredFailed",
            Self::Unavailable => "update.requiredUnavailable",
            Self::Manual => "update.requiredManual",
        }
    }
}

/// What one update attempt came to (desktop.ts: the shell updater's `idle`, `available`, `error` phases; `ready` restarts
/// the app and has no outcome to show).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum UpdateOutcome {
    Unavailable,
    Manual,
    Failed,
}

impl UpdateOutcome {
    pub fn message(self) -> UpdateMessage {
        match self {
            Self::Unavailable => UpdateMessage::Unavailable,
            Self::Manual => UpdateMessage::Manual,
            Self::Failed => UpdateMessage::Failed,
        }
    }
}

/// PLACEHOLDER: the native app has no updater yet (the Electron shell's `check-update` and `restart-update` have no
/// counterpart until the GPUI release plan lands one), so a check ends as the web's `idle` phase does: "No update is ready
/// for this app yet". The one place to replace.
pub fn check_for_update() -> UpdateOutcome {
    UpdateOutcome::Unavailable
}

/// The message the update banner shows now; the press changes it, as desktop.ts's `onChange` does.
/// App-wide by design (final review sweep): an update is the app's, the same in every window and for every account.
#[derive(Default)]
struct UpdateSurface(UpdateMessage);

impl Global for UpdateSurface {}

fn surface(cx: &mut App) -> UpdateMessage {
    cx.default_global::<UpdateSurface>().0
}

/// What the banner slot shows for a state.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum BannerPlan {
    /// The link line: offline, or connecting to `node`.
    Link { status: LinkStatus, node: String },
    /// "Update dilla to continue" (or what the last press made of it) with its button.
    UpdateRequired(UpdateMessage),
    /// "This server needs a newer client", with Reload.
    ClientTooOld,
}

/// The banner a state shows, if any. An online link, and any phase the shell is not drawn in, show none: an empty slot
/// takes no room.
pub fn plan(state: &AppState, update: UpdateMessage) -> Option<BannerPlan> {
    match state.account.phase {
        AccountPhase::VersionRefused { upgrade: true } => Some(BannerPlan::UpdateRequired(update)),
        AccountPhase::VersionRefused { upgrade: false } => Some(BannerPlan::ClientTooOld),
        AccountPhase::Ready => {
            let status = match state.connection.status {
                ConnectionStatus::Online => return None,
                ConnectionStatus::Offline => LinkStatus::Offline,
                ConnectionStatus::Connecting => LinkStatus::Connecting,
            };
            Some(BannerPlan::Link {
                status,
                node: state
                    .diagnostics
                    .instance_name
                    .clone()
                    .unwrap_or_else(|| t("shell.status.unknown").to_owned()),
            })
        }
        _ => None,
    }
}

/// The slot's elements for `ctx`.
pub fn banners(ctx: &SlotCx, _window: &mut Window, cx: &mut App) -> Vec<AnyElement> {
    let Some(plan) = plan(&ctx.state, surface(cx)) else {
        return Vec::new();
    };
    let client = ctx.client.clone();
    vec![match plan {
        BannerPlan::Link { status, node } => tracked_box(
            "banner-link",
            LinkBanner::new(
                "banner-link-line",
                status,
                t("nav.link.offline"),
                tf("nav.link.connecting", &[("node", &node)]),
            ),
        )
        .into_any_element(),
        BannerPlan::UpdateRequired(message) => tracked_box(
            "banner-update",
            Banner::new("banner-update-line", BannerTone::Warn, t(message.key())).action(
                t("update.requiredAction"),
                |_, _, cx| {
                    cx.default_global::<UpdateSurface>().0 = check_for_update().message();
                    cx.refresh_windows();
                },
            ),
        )
        .into_any_element(),
        BannerPlan::ClientTooOld => tracked_box(
            "banner-client-too-old",
            Banner::new(
                "banner-client-too-old-line",
                BannerTone::Warn,
                t("shell.banner.clientTooOld"),
            )
            .action(t("reload.action"), move |_, _, _| {
                if let Some(client) = &client {
                    client.reconnect();
                }
            }),
        )
        .into_any_element(),
    }]
}

#[cfg(test)]
mod tests {
    use super::*;

    fn state(phase: AccountPhase, status: ConnectionStatus) -> AppState {
        let mut s = AppState::default();
        s.account.phase = phase;
        s.connection.status = status;
        s
    }

    #[test]
    fn online_shows_nothing() {
        let s = state(AccountPhase::Ready, ConnectionStatus::Online);
        assert_eq!(plan(&s, UpdateMessage::Required), None);
    }

    #[test]
    fn offline_and_connecting_show_the_link_line() {
        let mut s = state(AccountPhase::Ready, ConnectionStatus::Offline);
        s.diagnostics.instance_name = Some("dilla.test".into());
        assert_eq!(
            plan(&s, UpdateMessage::Required),
            Some(BannerPlan::Link {
                status: LinkStatus::Offline,
                node: "dilla.test".into()
            })
        );
        s.connection.status = ConnectionStatus::Connecting;
        s.diagnostics.instance_name = None;
        assert_eq!(
            plan(&s, UpdateMessage::Required),
            Some(BannerPlan::Link {
                status: LinkStatus::Connecting,
                node: "–".into()
            })
        );
    }

    #[test]
    fn a_refusal_replaces_the_link_line() {
        let upgrade = state(
            AccountPhase::VersionRefused { upgrade: true },
            ConnectionStatus::Offline,
        );
        assert_eq!(
            plan(&upgrade, UpdateMessage::Unavailable),
            Some(BannerPlan::UpdateRequired(UpdateMessage::Unavailable))
        );
        let plain = state(
            AccountPhase::VersionRefused { upgrade: false },
            ConnectionStatus::Offline,
        );
        assert_eq!(
            plan(&plain, UpdateMessage::Required),
            Some(BannerPlan::ClientTooOld)
        );
    }

    #[test]
    fn phases_the_shell_is_not_drawn_in_show_nothing() {
        for phase in [
            AccountPhase::Loading,
            AccountPhase::FactorSetup,
            AccountPhase::Revoked,
            AccountPhase::Failed,
        ] {
            assert_eq!(
                plan(
                    &state(phase, ConnectionStatus::Offline),
                    UpdateMessage::Required
                ),
                None,
                "{phase:?}"
            );
        }
    }

    #[test]
    fn the_press_message_keys_exist_in_strings() {
        for m in [
            UpdateMessage::Required,
            UpdateMessage::Failed,
            UpdateMessage::Unavailable,
            UpdateMessage::Manual,
        ] {
            assert!(crate::strings::try_t(m.key()).is_some(), "{m:?}");
        }
        assert_eq!(check_for_update().message(), UpdateMessage::Unavailable);
    }
}
