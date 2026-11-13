//! Settings → Privacy (settings/Privacy.tsx): the heading, the DM GIF consent (Task 12's `dm_gif_privacy`), then every
//! contact this device pinned as one settings row, in Flow 05's states: changed (an alert that cannot be dismissed),
//! unlisted (no comparison), verified (the compared public key) or first seen here. Which state a row is in is
//! `state::trust::privacy_row` of the client's trust slice, which the client reads from the core only.
//!
//! Automation ids: `privacy-heading`, `trust-<i>` (a contact's row), `trust-<i>-alert`, `privacy-compare-<i>`.

use dilla_client::state::trust::privacy_row;
use gpui::{AnyElement, App, InteractiveElement, IntoElement, ParentElement, Styled, Window, div};

use dilla_desktop_ui::components::dms::{
    TrustAlert, TrustTone, privacy::body_text, trust_key, trust_state,
};
use dilla_desktop_ui::components::settings::frame::section_header;
use dilla_desktop_ui::components::settings::row::{GroupPosition, SettingsRow};
use dilla_desktop_ui::components::{Button, ButtonVariant};
use dilla_desktop_ui::metrics::Density;

use crate::slots::SlotCx;
use crate::strings::{t, tf};

use super::compare;

struct Heading(gpui::FocusHandle);

/// The Compare buttons' focus handles of one window, by row (kept across frames so focus survives a redraw; per window,
/// final review sweep: a handle is never another window's).
#[derive(Default)]
struct CompareFocus(Vec<gpui::FocusHandle>);

/// The focus handle of row `i`'s Compare button in window `wid` (its alert's Compare for a changed contact).
pub fn compare_handle(wid: gpui::WindowId, i: usize, cx: &mut App) -> gpui::FocusHandle {
    while crate::per_window::get_mut::<CompareFocus>(wid, cx).0.len() <= i {
        let h = cx.focus_handle().tab_stop(true);
        crate::per_window::get_mut::<CompareFocus>(wid, cx)
            .0
            .push(h);
    }
    crate::per_window::get_mut::<CompareFocus>(wid, cx).0[i].clone()
}

/// The Privacy section.
pub fn section(ctx: &SlotCx, window: &mut Window, cx: &mut App) -> AnyElement {
    let heading = window
        .use_keyed_state("privacy-heading", cx, |_, cx| {
            Heading(cx.focus_handle().tab_stop(false))
        })
        .read(cx)
        .0
        .clone();
    let wid = window.window_handle().window_id();
    compare::set_heading(wid, heading.clone(), cx);
    if let Some(client) = &ctx.client {
        client.trust().start();
    }
    let density = Density::Regular;
    let contacts = ctx.state.trust.contacts.clone();
    let n = contacts.len();
    let handles: Vec<gpui::FocusHandle> = (0..n).map(|i| compare_handle(wid, i, cx)).collect();
    let rows = contacts.into_iter().enumerate().map(|(i, record)| {
        let handle = handles[i].clone();
        let row = privacy_row(&record);
        let name = record.name.clone();
        let mut control = div().flex().flex_col().gap(density.row_gap()).min_w_0();
        let open_compare = {
            let (record, client) = (record.clone(), ctx.client.clone());
            move |_: &gpui::ClickEvent, window: &mut Window, cx: &mut App| {
                compare::open(
                    window.window_handle().window_id(),
                    &record,
                    client.clone(),
                    cx,
                );
            }
        };
        if let Some(copy) = row.changed {
            if row.alert {
                let mut alert = TrustAlert::new(("trust-alert", i), name.clone(), t(copy));
                if row.alert_compare {
                    alert = alert.compare(
                        tf("privacy.compare", &[("name", &name)]),
                        open_compare.clone(),
                    );
                }
                control = control.child(alert);
            } else {
                control = control.child(trust_state(t(copy), TrustTone::Danger));
            }
        }
        if row.unlisted {
            control = control.child(trust_state(t("privacy.unlisted"), TrustTone::Warn));
        } else if let Some(key) = row.verified_key {
            control = control
                .child(trust_state(t("privacy.verifiedLabel"), TrustTone::Label))
                .child(trust_key(key));
        } else if row.first_seen {
            control = control
                .child(trust_state(t("privacy.firstSeenLabel"), TrustTone::Label))
                .child(body_text(tf("privacy.firstSeen", &[("name", &name)])));
        }
        if row.no_shared {
            control = control.child(body_text(t("privacy.noShared")));
        } else if row.compare {
            control = control.child(
                Button::new(
                    ("privacy-compare", i),
                    tf("privacy.compare", &[("name", &name)]),
                )
                .variant(ButtonVariant::Ghost)
                .focus_handle(&handle)
                .on_click(open_compare),
            );
        }
        SettingsRow::new(("trust", i), name)
            .position(GroupPosition::of(i, n))
            .child(control)
    });
    div()
        .id("privacy")
        .flex()
        .flex_col()
        .child(
            div()
                .id("privacy-heading")
                .track_focus(&heading)
                .child(section_header(t("privacy.section"), None)),
        )
        .children(crate::screens::attachments::dm_gif_privacy(ctx, window, cx))
        .child(div().flex().flex_col().children(rows))
        .into_any_element()
}
