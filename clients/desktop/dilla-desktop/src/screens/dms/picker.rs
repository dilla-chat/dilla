//! The DM picker (Shell.tsx `DmPicker`, requirement 5): a dialog listing the selected server's members (every server's
//! with none selected), one "message" button each, the refusal inside the dialog. Picking one opens the DM with them
//! (made if there is none) and the shell goes to it.
//!
//! Keyboard: the list has focus when the dialog opens. Up and Down move through the members (wrapping), typing jumps to
//! the next member whose name starts with what was typed (type-ahead, the buffer resets after a second without a key),
//! Enter opens the DM with the member the keyboard is on. Tab stays inside and Escape closes (the shared `Dialog`).

use std::rc::Rc;
use std::time::{Duration, Instant};

use dilla_client::Client;
use dilla_client::features::dms::{picker_members, type_ahead};
use dilla_client::state::{AppState, CommunityId, Member};
use gpui::{
    AnyElement, App, AppContext as _, FocusHandle, InteractiveElement, IntoElement, KeyDownEvent,
    ParentElement, Styled, Window, WindowId, div,
};

use dilla_desktop_ui::components::banner::{Banner, BannerTone};
use dilla_desktop_ui::components::dialog::{Dialog, InitialFocus};
use dilla_desktop_ui::components::dms::privacy::body_text;
use dilla_desktop_ui::components::dms::{DmPickerList, PickerMember};
use dilla_desktop_ui::metrics::Density;

use crate::strings::{t, tf};

use super::{Nav, navigate};

/// How long typed letters build one type-ahead prefix.
const TYPE_AHEAD_RESET: Duration = Duration::from_secs(1);

/// The picker's state, one per window (final review sweep: a picker opened in one window never shows in another, or
/// opens a DM on another window's client).
#[derive(Default)]
pub struct PickerModel {
    pub open: bool,
    pub community: Option<CommunityId>,
    pub selected: Option<usize>,
    pub busy: bool,
    pub error: Option<String>,
    typed: String,
    typed_at: Option<Instant>,
    client: Option<Rc<Client>>,
    list: Option<FocusHandle>,
}

fn model(window: WindowId, cx: &mut App) -> &mut PickerModel {
    crate::per_window::get_mut::<PickerModel>(window, cx)
}

/// Window `window`'s picker, as the tests read it.
pub fn model_in(window: WindowId, cx: &mut App) -> &mut PickerModel {
    model(window, cx)
}

/// Lets go of `client` (Forget, sign-out: `crate::release::release_client`): every window's picker of it closes.
pub fn release(client: &Rc<Client>, cx: &mut App) {
    crate::per_window::each_mut::<PickerModel>(cx, |_, m| {
        if m.client.as_ref().is_some_and(|c| Rc::ptr_eq(c, client)) {
            m.client = None;
            m.open = false;
            m.busy = false;
        }
    });
}

/// Opens `window`'s picker over `community`'s members (`None`: every server's).
pub fn open(
    window: WindowId,
    community: Option<CommunityId>,
    client: Option<Rc<Client>>,
    cx: &mut App,
) {
    let m = model(window, cx);
    m.open = true;
    m.community = community;
    m.selected = Some(0);
    m.busy = false;
    m.error = None;
    m.typed.clear();
    m.typed_at = None;
    m.client = client;
    cx.refresh_windows();
}

pub fn close(window: WindowId, cx: &mut App) {
    let m = model(window, cx);
    m.open = false;
    m.busy = false;
    cx.refresh_windows();
}

fn name_of(m: &Member) -> String {
    if m.display.is_empty() {
        m.username.clone()
    } else {
        m.display.clone()
    }
}

/// Opens the DM with `member`.
fn pick(member: &Member, window: &mut Window, cx: &mut App) {
    let wid = window.window_handle().window_id();
    let m = model(wid, cx);
    if m.busy {
        return;
    }
    let Some(client) = m.client.clone() else {
        return;
    };
    m.busy = true;
    m.error = None;
    let answer = client.dms().open_dm(member.user_id);
    let handle = window.window_handle();
    cx.refresh_windows();
    cx.spawn(async move |cx| {
        let result = answer.await;
        let _ = cx.update_window(handle, |_, window, cx| {
            let m = model(wid, cx);
            m.busy = false;
            match result {
                Ok(Ok(channel)) => {
                    close(wid, cx);
                    navigate(Nav::Dm(channel), window, cx);
                }
                Ok(Err(e)) => {
                    m.error = Some(match e {
                        dilla_client::ClientError::Sync(code) => code,
                        _ => "E_NETWORK".to_owned(),
                    });
                }
                Err(_) => m.error = Some("E_STOPPED".to_owned()),
            }
            cx.refresh_windows();
        });
    })
    .detach();
}

/// What a key does to the picker's selection over `names`: the new selection, and whether Enter asked to open it.
pub fn on_key(
    names: &[String],
    selected: Option<usize>,
    typed: &mut String,
    typed_at: &mut Option<Instant>,
    key: &str,
    key_char: Option<&str>,
    now: Instant,
) -> (Option<usize>, bool) {
    let n = names.len();
    if n == 0 {
        return (None, false);
    }
    let at = selected.unwrap_or(0).min(n - 1);
    match key {
        "down" => return (Some((at + 1) % n), false),
        "up" => return (Some((at + n - 1) % n), false),
        "home" => return (Some(0), false),
        "end" => return (Some(n - 1), false),
        "enter" => return (Some(at), true),
        _ => {}
    }
    let Some(ch) = key_char.filter(|c| c.chars().all(|c| !c.is_control()) && !c.is_empty()) else {
        return (selected, false);
    };
    if typed_at.is_none_or(|t| now.duration_since(t) > TYPE_AHEAD_RESET) {
        typed.clear();
    }
    typed.push_str(ch);
    *typed_at = Some(now);
    // A first letter moves on from the current member; a longer prefix keeps it while it still matches.
    let from = if typed.chars().count() == 1 {
        at + 1
    } else {
        at
    };
    (type_ahead(names, typed, from).or(selected), false)
}

/// The picker, drawn over the window (render it last in the window's root); `None` before it was ever opened.
pub fn picker_overlay(state: &AppState, window: &mut Window, cx: &mut App) -> Option<AnyElement> {
    let wid = window.window_handle().window_id();
    crate::per_window::get::<PickerModel>(wid, cx)?;
    let list = match model(wid, cx).list.clone() {
        Some(h) => h,
        None => {
            let h = cx.focus_handle().tab_stop(true);
            model(wid, cx).list = Some(h.clone());
            h
        }
    };
    let m = crate::per_window::get::<PickerModel>(wid, cx)?;
    let dialog = Dialog::new(
        "dm-picker-dialog",
        t("shell.dms.newTitle"),
        t("dialog.close"),
    )
    .initial_focus(InitialFocus::Handle(list.clone()))
    .on_close(move |_, cx| close(wid, cx));
    if !m.open {
        return Some(dialog.open(false).into_any_element());
    }
    let members = picker_members(state, m.community);
    let names: Vec<String> = members.iter().map(name_of).collect();
    let no_server = crate::screens::shell::no_server(state);
    let body_key = if no_server {
        "shell.dms.noServer"
    } else if members.is_empty() {
        "shell.dms.alone"
    } else {
        "shell.dms.newBody"
    };
    let density = Density::Regular;
    let mut body = div().flex().flex_col().gap(density.group_gap());
    if let Some(code) = &m.error {
        body = body.child(Banner::new(
            "dm-picker-error",
            BannerTone::Danger,
            tf("shell.banner.commandError", &[("code", code)]),
        ));
    }
    body = body.child(body_text(t(body_key)));
    if !members.is_empty() {
        let picked = members.clone();
        let rows = DmPickerList::new(
            names
                .iter()
                .map(|n| PickerMember {
                    name: n.clone().into(),
                })
                .collect(),
            t("shell.dms.open"),
            move |i, window, cx| {
                if let Some(member) = picked.get(i) {
                    pick(member, window, cx);
                }
            },
        )
        .selected(m.selected)
        .busy(m.busy);
        let keyed = members.clone();
        body = body.child(
            div()
                .id("dm-picker-list")
                .track_focus(&list)
                .on_key_down(move |event: &KeyDownEvent, window, cx| {
                    let ks = &event.keystroke;
                    if ks.modifiers.control || ks.modifiers.alt || ks.modifiers.platform {
                        return;
                    }
                    let now = Instant::now();
                    let m = model(wid, cx);
                    let (mut typed, mut typed_at) = (std::mem::take(&mut m.typed), m.typed_at);
                    let (selected, enter) = on_key(
                        &names,
                        m.selected,
                        &mut typed,
                        &mut typed_at,
                        ks.key.as_str(),
                        ks.key_char.as_deref(),
                        now,
                    );
                    m.typed = typed;
                    m.typed_at = typed_at;
                    m.selected = selected;
                    if matches!(ks.key.as_str(), "down" | "up" | "home" | "end" | "enter")
                        || ks.key_char.is_some()
                    {
                        cx.stop_propagation();
                    }
                    if enter && let Some(member) = selected.and_then(|i| keyed.get(i)) {
                        pick(member, window, cx);
                    }
                    cx.refresh_windows();
                })
                .child(rows),
        );
    }
    Some(dialog.open(true).body(body).into_any_element())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn names() -> Vec<String> {
        ["Ada", "Bo", "Bea", "Cy"]
            .iter()
            .map(|s| (*s).to_owned())
            .collect()
    }

    #[test]
    fn arrows_wrap_and_enter_picks() {
        let (mut typed, mut at) = (String::new(), None);
        let now = Instant::now();
        let n = names();
        assert_eq!(
            on_key(&n, Some(3), &mut typed, &mut at, "down", None, now),
            (Some(0), false)
        );
        assert_eq!(
            on_key(&n, Some(0), &mut typed, &mut at, "up", None, now),
            (Some(3), false)
        );
        assert_eq!(
            on_key(&n, Some(2), &mut typed, &mut at, "enter", None, now),
            (Some(2), true)
        );
    }

    #[test]
    fn type_ahead_moves_on_and_extends() {
        let (mut typed, mut at) = (String::new(), None);
        let t0 = Instant::now();
        let n = names();
        let (s, _) = on_key(&n, Some(0), &mut typed, &mut at, "b", Some("b"), t0);
        assert_eq!(s, Some(1));
        // The same letter again moves on to the next match.
        let (s, _) = on_key(&n, s, &mut typed, &mut at, "e", Some("e"), t0);
        assert_eq!(s, Some(2), "`be` matches Bea");
        // After the reset a new prefix starts.
        let later = t0 + Duration::from_secs(2);
        let (s, _) = on_key(&n, s, &mut typed, &mut at, "c", Some("c"), later);
        assert_eq!(s, Some(3));
        assert_eq!(typed, "c");
    }
}
