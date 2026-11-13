//! The safety-number comparison (packages/ui SafetyCompare in settings/Privacy, Flow 05): a dialog over the window that
//! shows the twelve groups for exactly the key it would approve, the key itself behind a disclosure, and the QR on
//! request. "All 12 blocks match" approves only once the person ticked the confirmation for exactly this comparison
//! (`state::trust::compare_permitted`); Copy puts the number on the clipboard and approves nothing.
//!
//! The number and the approval come from the core through the client (`Trust::safety_number`, `Trust::verify`). The
//! dialog never approves a key other than the one it showed: when the trust slice's comparison key moves while it is
//! open, it says so and refuses (`privacy.keyMoved`). It traps focus and Escape closes it, returning focus to the opener
//! (the shared `Dialog`); after an approval focus lands on the Privacy heading (W-11).

use std::rc::Rc;

use dilla_client::Client;
use dilla_client::state::trust::TrustState;
use dilla_client::state::trust::{
    CompareState, TrustSummary, compare_permitted, safety_qr_value, valid_groups,
};
use dilla_client::state::{AppState, UserId};
use gpui::{
    AnyElement, App, AppContext as _, ClipboardItem, FocusHandle, Global, IntoElement, Window,
};

use dilla_desktop_ui::components::dialog::Dialog;
use dilla_desktop_ui::components::dms::SafetyCompareBody;
use dilla_desktop_ui::components::{Button, ButtonSize, ButtonVariant};

use crate::strings::{t, tf};

/// What the open comparison holds. One per app: one dialog at a time.
#[derive(Default)]
pub struct CompareModel {
    pub open: bool,
    pub user: Option<UserId>,
    /// The key the number was worked out for, and the only key "All 12 blocks match" approves.
    pub approved_umk: Option<String>,
    pub groups: Vec<String>,
    /// The number is being worked out.
    pub loading: bool,
    /// A `strings.rs` key: `privacy.loadFailed`, `privacy.verifyFailed`, `privacy.keyMoved`.
    pub error: Option<&'static str>,
    /// The comparison the person confirmed (`approved_umk|groups`); any other comparison is unconfirmed.
    pub confirmed_for: Option<String>,
    pub qr_shown: bool,
    pub details_open: bool,
    /// An approval is out.
    pub busy: bool,
    client: Option<Rc<Client>>,
    /// The Privacy heading: focus lands there after an approval.
    pub heading: Option<FocusHandle>,
}

impl Global for CompareModel {}

impl CompareModel {
    fn comparison(&self) -> String {
        format!(
            "{}|{}",
            self.approved_umk.as_deref().unwrap_or_default(),
            self.groups.join("|")
        )
    }

    pub fn confirmed(&self) -> bool {
        self.confirmed_for.as_deref() == Some(self.comparison().as_str())
    }
}

fn model(cx: &mut App) -> &mut CompareModel {
    if !cx.has_global::<CompareModel>() {
        cx.set_global(CompareModel::default());
    }
    cx.global_mut::<CompareModel>()
}

/// Remembers the Privacy heading's focus handle (the section calls this as it draws).
pub fn set_heading(heading: FocusHandle, cx: &mut App) {
    model(cx).heading = Some(heading);
}

/// Opens the comparison with `record` and asks the client for the number of its comparison key.
pub fn open(record: &TrustSummary, client: Option<Rc<Client>>, cx: &mut App) {
    let key = record.comparison_key().to_owned();
    let user = record.user_id;
    let heading = model(cx).heading.take();
    *model(cx) = CompareModel {
        open: true,
        user: Some(user),
        approved_umk: Some(key.clone()),
        loading: client.is_some(),
        client: client.clone(),
        heading,
        ..CompareModel::default()
    };
    cx.refresh_windows();
    let Some(client) = client else {
        return;
    };
    let answer = client.trust().safety_number(user, key.clone());
    cx.spawn(async move |cx| {
        let result = answer.await;
        cx.update(|cx| {
            let m = model(cx);
            // A later comparison replaced this one: its answer is not this dialog's.
            if m.user != Some(user) || m.approved_umk.as_deref() != Some(key.as_str()) || !m.open {
                return;
            }
            m.loading = false;
            match result {
                Ok(Ok(groups)) if valid_groups(&groups) => m.groups = groups,
                Ok(Err(dilla_client::ClientError::Sync(code))) if key_moved(&code) => {
                    m.error = Some("privacy.keyMoved");
                }
                _ => m.error = Some("privacy.loadFailed"),
            }
            cx.refresh_windows();
        });
    })
    .detach();
}

/// Copy: the twelve groups on the clipboard. It approves nothing and leaves the confirmation as it was.
pub fn copy(cx: &mut App) {
    let groups = model(cx).groups.join(" ");
    if !groups.is_empty() {
        cx.write_to_clipboard(ClipboardItem::new_string(groups));
    }
}

/// The "all 12 blocks match" checkbox: confirms exactly the comparison shown, or takes the confirmation back.
pub fn toggle_confirm(cx: &mut App) {
    let m = model(cx);
    m.confirmed_for = if m.confirmed() {
        None
    } else {
        Some(m.comparison())
    };
    cx.refresh_windows();
}

/// Whether "All 12 blocks match" would approve now, against the trust slice of `state`: a verifiable contact whose
/// comparison key is still the one shown, twelve groups, and the person's confirmation of exactly this comparison.
pub fn permitted(state: &AppState, cx: &App) -> bool {
    permitted_by(&state.trust, cx)
}

/// [`permitted`] against the trust slice alone. A dialog that says the key moved approves nothing, whatever was
/// confirmed.
fn permitted_by(trust: &TrustState, cx: &App) -> bool {
    let Some(m) = cx.try_global::<CompareModel>() else {
        return false;
    };
    let Some(record) = m.user.and_then(|u| trust.contact(u)) else {
        return false;
    };
    let current = Some(record.comparison_key()) == m.approved_umk.as_deref();
    let moved = m.error == Some("privacy.keyMoved");
    compare_permitted(
        m.open && record.shared && !record.unlisted && current && !moved && !m.busy,
        m.approved_umk.as_deref(),
        &m.groups,
        m.confirmed(),
    )
}

/// A refusal that means the comparison key is no longer the one shown: the list moved on (`E_DEVICE_LIST_STALE`) or
/// the contact's key changed again (`E_UMK_CHANGED`).
fn key_moved(code: &str) -> bool {
    matches!(code, "E_DEVICE_LIST_STALE" | "E_UMK_CHANGED")
}

/// Closes the comparison (its Close, Escape, the scrim, or after an approval).
pub fn close(cx: &mut App) {
    let m = model(cx);
    m.open = false;
    m.confirmed_for = None;
    m.qr_shown = false;
    m.details_open = false;
    cx.refresh_windows();
}

fn approve(window: &mut Window, cx: &mut App) {
    let m = model(cx);
    let (Some(user), Some(key), Some(client)) = (m.user, m.approved_umk.clone(), m.client.clone())
    else {
        return;
    };
    if m.busy {
        return;
    }
    m.busy = true;
    m.error = None;
    let answer = client.trust().verify(user, key);
    let handle = window.window_handle();
    cx.spawn(async move |cx| {
        let result = answer.await;
        let _ = cx.update_window(handle, |_, window, cx| {
            let m = model(cx);
            m.busy = false;
            match result {
                Ok(Ok(())) => {
                    let heading = m.heading.clone();
                    close(cx);
                    if let Some(h) = heading {
                        window.defer(cx, move |window, cx| h.focus(window, cx));
                    }
                }
                Ok(Err(dilla_client::ClientError::Sync(code))) if key_moved(&code) => {
                    m.error = Some("privacy.keyMoved");
                }
                _ => m.error = Some("privacy.verifyFailed"),
            }
            cx.refresh_windows();
        });
    })
    .detach();
}

fn state_label(state: CompareState) -> &'static str {
    // SafetyCompare's `labels.states`: the Privacy section names each state with its own copy.
    match state {
        CompareState::FirstSeen => t("privacy.firstSeenLabel"),
        CompareState::Changed => t("privacy.changed"),
        CompareState::Verified => t("privacy.verifiedLabel"),
        CompareState::Unlisted => t("privacy.unlisted"),
    }
}

fn dialog() -> Dialog {
    Dialog::new(
        "safety-compare-dialog",
        t("privacy.title"),
        t("dialog.close"),
    )
}

/// The comparison, drawn over the window (render it last in the window's root); `None` before one was ever opened. A
/// closed one still draws its (empty) dialog for a frame, so the dialog returns focus to its opener.
pub fn compare_overlay(state: &AppState, window: &mut Window, cx: &mut App) -> Option<AnyElement> {
    if !cx.has_global::<CompareModel>() {
        return None;
    }
    let m = cx.global::<CompareModel>();
    let Some(user) = m.user.filter(|_| m.open) else {
        return Some(dialog().open(false).into_any_element());
    };
    let record = state.trust.contact(user).cloned();
    let name = record
        .as_ref()
        .map_or_else(|| user.hex()[..8].to_owned(), |r| r.name.clone());
    let compare_state = record
        .as_ref()
        .map_or(CompareState::Unlisted, CompareState::of);
    // The key the slice compares now: a key that moved while the dialog was open is never approved.
    let moved = record
        .as_ref()
        .is_none_or(|r| Some(r.comparison_key()) != m.approved_umk.as_deref());
    let error = if moved && !m.loading {
        Some("privacy.keyMoved")
    } else {
        m.error
    };
    let confirmed = m.confirmed();
    let qr_value = safety_qr_value(user, &m.groups);
    let permitted = permitted(state, cx);
    let trust = state.trust.clone();
    let m = cx.global::<CompareModel>();
    let mut body = SafetyCompareBody::new(
        format!("{name} · {}", state_label(compare_state)),
        t("privacy.instructions"),
        (
            t("privacy.copy").into(),
            |_: &gpui::ClickEvent, _: &mut Window, cx: &mut App| copy(cx),
        ),
        (
            t("privacy.confirm").into(),
            confirmed,
            |_: &gpui::ClickEvent, _: &mut Window, cx: &mut App| toggle_confirm(cx),
        ),
    )
    .groups(m.groups.clone())
    .loading(t("privacy.loading"));
    if compare_state == CompareState::FirstSeen {
        body = body.first_seen(tf("privacy.firstSeen", &[("name", &name)]));
    }
    if let Some(key) = m.approved_umk.clone() {
        body = body.details(t("privacy.keyDetails"), key, m.details_open, |_, _, cx| {
            let m = model(cx);
            m.details_open = !m.details_open;
            cx.refresh_windows();
        });
    }
    if let Some(value) = qr_value {
        let shown = m.qr_shown;
        body = body.qr_toggle(
            t(if shown {
                "privacy.hideQr"
            } else {
                "privacy.showQr"
            }),
            shown,
            |_, _, cx| {
                let m = model(cx);
                m.qr_shown = !m.qr_shown;
                cx.refresh_windows();
            },
        );
        if shown {
            body = body.qr(value, t("privacy.qrLabel"));
        }
    }
    if let Some(error) = error {
        body = body.error(t(error));
    }
    let _ = window;
    Some(
        dialog()
            .open(true)
            .body(body)
            .footer(
                Button::new("safety-verify", t("privacy.verify"))
                    .variant(ButtonVariant::Accent)
                    .size(ButtonSize::Dialog)
                    .disabled(!permitted)
                    .on_click(move |_, window, cx| {
                        // Judged again at the click: the model may have moved since this frame was drawn.
                        if permitted_by(&trust, cx) {
                            approve(window, cx);
                        }
                    }),
            )
            .on_close(|_, cx| close(cx))
            .into_any_element(),
    )
}
