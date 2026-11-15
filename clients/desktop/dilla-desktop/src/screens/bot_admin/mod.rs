//! Area **bot admin** (Task 21): the Bot and Music server settings and the now-playing card.
//!
//! Every file under `screens/bot_admin/` is Task 21's. The hooks below are where `crate::slots` draws this area inside
//! another area's view; their signatures are Task 1's.
//!
//! - [`now_playing`] is the compact row in the shell's dock, drawn from the [`player::MusicFeed`] the shell and the voice
//!   area provide while this device is in a call with the Bot in it.
//! - [`settings_section`] draws the Bot and Music sections. Each keeps its drafts and dialogs in a view of its own, kept
//!   in the window under the section's id, so a section left and entered again finds them as they were.

pub mod bot_page;
pub mod keys;
pub mod music_page;
pub mod player;

use gpui::{AnyElement, App, Empty, IntoElement, Window};

use crate::router::ServerSection;
use crate::slots::{SectionId, SlotCx};

/// The now-playing row in the dock (`slots::dock`, after the voice dock).
pub fn now_playing(window: &mut Window, cx: &mut App) -> Option<AnyElement> {
    player::dock(window, cx)
}
/// The Bot and Music pages' manager (BotSection.tsx `access.loaded && access.manageCommunity`): Manage server in the
/// page's community (the owner holds it), nobody before the access read lands. It reads the one access slice
/// (`state.roles.access`, final review I2) that the host's nav and the members area read too, so a revoked grant hides
/// management everywhere at once.
pub fn is_manager(ctx: &SlotCx) -> bool {
    let Some(community) = ctx.community else {
        return false;
    };
    let access = ctx.state.roles.access_of(community);
    access.loaded && access.manage_community
}

/// Draws one of this area's settings sections (`slots::settings_sections` points here).
pub fn settings_section(
    section: SectionId,
    ctx: &SlotCx,
    window: &mut Window,
    cx: &mut App,
) -> AnyElement {
    match section {
        SectionId::Server(ServerSection::Bot) => {
            let page = window.use_keyed_state("bot-admin-bot-page", cx, |window, cx| {
                bot_page::BotPage::new(ctx.clone(), window, cx)
            });
            page.update(cx, |p, _| p.set_ctx(ctx.clone()));
            // Release lets go of the page's client at once, not when the window next draws (final review M2).
            if let Some(client) = &ctx.client {
                crate::release::hold(client, &page, |p, _| p.set_ctx(SlotCx::default()), cx);
            }
            page.into_any_element()
        }
        SectionId::Server(ServerSection::Music) => {
            let page = window.use_keyed_state("bot-admin-music-page", cx, |_, _| {
                music_page::MusicPage::new(ctx.clone())
            });
            page.update(cx, |p, _| p.set_ctx(ctx.clone()));
            // Release lets go of the page's client at once, not when the window next draws (final review M2).
            if let Some(client) = &ctx.client {
                crate::release::hold(client, &page, |p, _| p.set_ctx(SlotCx::default()), cx);
            }
            page.into_any_element()
        }
        _ => Empty.into_any_element(),
    }
}

#[cfg(test)]
mod tests {
    use std::sync::Arc;

    use dilla_client::state::roles::CommunityAccess;
    use dilla_client::state::{AppState, CommunityId, UserId};

    use super::*;

    /// Integration 3, step 5: the Bot and Music pages' manager is Task 14's resolver's (BotSection.tsx: `access.loaded
    /// && access.manageCommunity`): a member whose roles grant Manage server is one, the owner is one once the access
    /// read landed, and nobody is before it.
    #[test]
    fn the_manager_is_the_resolvers_manage_community() {
        let community = CommunityId([7; 16]);
        let ctx = |access: Option<CommunityAccess>| {
            let mut s = AppState::default();
            s.account.user_id = Some(UserId([1; 16]));
            if let Some(a) = access {
                s.roles.access.insert(community, a);
            }
            SlotCx {
                state: Arc::new(s),
                community: Some(community),
                ..SlotCx::default()
            }
        };
        let granted = CommunityAccess {
            manage_community: true,
            loaded: true,
            ..CommunityAccess::default()
        };
        assert!(is_manager(&ctx(Some(granted.clone()))), "Manage server");
        let owner = CommunityAccess {
            owner: true,
            ..granted.clone()
        };
        assert!(is_manager(&ctx(Some(owner))), "the owner");
        let unloaded = CommunityAccess {
            loaded: false,
            ..granted
        };
        assert!(!is_manager(&ctx(Some(unloaded))), "before the read");
        assert!(!is_manager(&ctx(None)), "no access read");
        let member = CommunityAccess {
            loaded: true,
            ..CommunityAccess::default()
        };
        assert!(!is_manager(&ctx(Some(member))), "a member without it");
    }

    /// Integration 3, final review I2: one resolver and one slice. A grant read once and revoked by the next read hides
    /// management everywhere at the same time: the Bot page, the host's nav (Bot, Music, Overview) and the members area
    /// all read the same access.
    #[test]
    fn a_revoked_grant_hides_management_everywhere_at_once() {
        use crate::screens::server_settings::model;
        let community = CommunityId([7; 16]);
        let with = |bits: u64| {
            let mut s = AppState::default();
            s.roles.access.insert(
                community,
                dilla_client::features::roles::rank::access_of(bits, 1, false),
            );
            s
        };
        let everywhere = |s: AppState| {
            let access = dilla_client::state::server_admin::access_in(&s, community);
            let nav = model::nav_sections(&access);
            let ctx = SlotCx {
                state: Arc::new(s),
                community: Some(community),
                ..SlotCx::default()
            };
            (
                is_manager(&ctx),
                nav.contains(&ServerSection::Bot),
                nav.contains(&ServerSection::Music),
                ctx.state.roles.access_of(community).manage_community,
            )
        };
        let manage = u64::from(dilla_client::features::roles::perm::MANAGE_COMMUNITY);
        assert_eq!(
            everywhere(with(manage)),
            (true, true, true, true),
            "granted"
        );
        assert_eq!(everywhere(with(0)), (false, false, false, false), "revoked");
    }
}
