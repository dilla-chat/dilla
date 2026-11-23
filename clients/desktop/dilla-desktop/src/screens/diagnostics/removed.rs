//! The removal notices over the shell: "You are no longer in {server}." for a server the community list dropped without
//! this account leaving it (Shell.tsx 1861-1866, `removed.notice`).
//!
//! Each is a warn banner with a dismiss action. Dismissing hides that notice's id for this window (the web's per-page
//! set) and hands the focus back to the composer. The notices themselves live in `AppState::removal_notices`; a server
//! joined, or listed again, drops its own.

use std::collections::HashSet;

use dilla_client::state::AppState;
use gpui::{AnyElement, App, IntoElement, Window};

use dilla_desktop_ui::components::banner::{Banner, BannerTone};

use crate::per_window;
use crate::slots::SlotCx;
use crate::strings::{t, tf};
use crate::tracked_box;

/// The notice ids this window dismissed.
#[derive(Default)]
struct Dismissed(HashSet<u64>);

/// The notices to show as `(id, text)`: every notice of the state whose id is not dismissed, oldest first.
pub fn removal_banners(state: &AppState, dismissed: &HashSet<u64>) -> Vec<(u64, String)> {
    state
        .removal_notices
        .iter()
        .filter(|n| !dismissed.contains(&n.id))
        .map(|n| (n.id, tf("removed.notice", &[("server", &n.name)])))
        .collect()
}

/// The slot's elements for `ctx`: one banner per notice not dismissed in this window.
pub fn banners(ctx: &SlotCx, window: &mut Window, cx: &mut App) -> Vec<AnyElement> {
    let id = window.window_handle().window_id();
    let hidden = per_window::get::<Dismissed>(id, cx)
        .map(|d| d.0.clone())
        .unwrap_or_default();
    removal_banners(&ctx.state, &hidden)
        .into_iter()
        .map(|(notice, text)| {
            let composer = ctx.composer.clone();
            tracked_box(
                "banner-removed",
                Banner::new(("banner-removed-line", notice), BannerTone::Warn, text).action(
                    t("shell.banner.dismiss"),
                    move |_, window, cx| {
                        per_window::get_mut::<Dismissed>(window.window_handle().window_id(), cx)
                            .0
                            .insert(notice);
                        if let Some(composer) = &composer {
                            window.focus(composer, cx);
                        }
                        cx.refresh_windows();
                    },
                ),
            )
            .into_any_element()
        })
        .collect()
}

#[cfg(test)]
mod tests {
    use dilla_client::state::CommunityId;
    use dilla_client::state::removed::RemovalNotice;

    use super::*;

    #[test]
    fn a_dismissed_notice_is_not_shown_again() {
        let state = AppState {
            removal_notices: vec![
                RemovalNotice {
                    id: 1,
                    community: CommunityId([1; 16]),
                    name: "Midgard".into(),
                },
                RemovalNotice {
                    id: 2,
                    community: CommunityId([2; 16]),
                    name: "Asgard".into(),
                },
            ],
            ..AppState::default()
        };
        let all = removal_banners(&state, &HashSet::new());
        assert_eq!(
            all,
            [
                (1, tf("removed.notice", &[("server", "Midgard")])),
                (2, tf("removed.notice", &[("server", "Asgard")])),
            ]
        );
        let rest = removal_banners(&state, &HashSet::from([1]));
        assert_eq!(rest, [(2, tf("removed.notice", &[("server", "Asgard")]))]);
    }
}
