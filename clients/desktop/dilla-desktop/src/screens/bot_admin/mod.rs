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

/// The Bot and Music pages' manager (BotSection.tsx `access.loaded && access.manageCommunity`): Task 14's resolver's
/// Manage server in the page's community (the owner holds it), nobody before the access read lands. The same route's
/// read lands in two slices (Task 14's `roles.access`, and `server_admin.access`, which the server settings host loads,
/// both rank.ts `accessOf`): either one counts.
pub fn is_manager(ctx: &SlotCx) -> bool {
    let Some(community) = ctx.community else {
        return false;
    };
    let roles = ctx.state.roles.access_of(community);
    let hosted = ctx
        .state
        .server_admin
        .access
        .get(&community)
        .is_some_and(|a| a.loaded && a.manage_community);
    (roles.loaded && roles.manage_community) || hosted
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
            page.into_any_element()
        }
        SectionId::Server(ServerSection::Music) => {
            let page = window.use_keyed_state("bot-admin-music-page", cx, |_, _| {
                music_page::MusicPage::new(ctx.clone())
            });
            page.update(cx, |p, _| p.set_ctx(ctx.clone()));
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
        // The server settings host reads the same route through Task 15's copy of `accessOf`: its read counts too.
        let mut s = AppState::default();
        s.server_admin.access.insert(
            community,
            dilla_client::features::server_admin::access_of(1 << 16, 1, false),
        );
        let hosted = SlotCx {
            state: Arc::new(s),
            community: Some(community),
            ..SlotCx::default()
        };
        assert!(is_manager(&hosted), "Manage server, read by the host");
    }
}
