//! Area **notify** (Task 22): the Notifications section of the settings.
//!
//! Port of `packages/web/src/screens/settings/Notifications.tsx`: exactly the prototype's four rows. The selected server's
//! mode and its @everyone switch (absent with no server), desktop notifications on this device, and one switch for the join,
//! leave and ring sounds. Every value is written through [`dilla_client::features::notify::Notify`], which keeps it in the
//! instance's preferences; the section keeps no copy and draws what the settings hold on every redraw.
//!
//! Activity (`screens/Activity.tsx`) is out of scope: the web client reaches it only from the phone's tab bar
//! (`Shell.tsx`: `phonePane === 'list' && phoneTab === 'activity'`), and this plan draws desktop sizes only.
//!
//! The section is the area's `settings_section` hook: the settings frame (Task 6) and its host draw the nav, the identity and
//! the close button around it.

pub mod keys;

use std::collections::HashMap;
use std::rc::Rc;

use dilla_client::Client;
use dilla_client::features::notify::{
    NotifyMode, Settings, SettingsSource, key_community_everyone, server_mode_shown,
};
use dilla_client::state::unread::Permission;
use dilla_client::state::{AppState, Community, CommunityId};
use gpui::{AnyElement, App, InteractiveElement, IntoElement, ParentElement, Styled, Window, div};

use dilla_desktop_ui::IconName;
use dilla_desktop_ui::components::segmented::Segmented;
use dilla_desktop_ui::components::settings::frame::section_header;
use dilla_desktop_ui::components::settings::row::{GroupPosition, SettingsRow};
use dilla_desktop_ui::components::toggle::{Fit, Toggle};

use crate::slots::{SectionId, SlotCx};
use crate::strings::{t, tf};

/// The three modes a server (or channel) can notify for, in the segmented control's order.
const MODES: [NotifyMode; 3] = [NotifyMode::All, NotifyMode::Mentions, NotifyMode::Nothing];

fn mode_label(mode: NotifyMode) -> &'static str {
    t(match mode {
        NotifyMode::All => "notify.mode.all",
        NotifyMode::Mentions => "notify.mode.mentions",
        NotifyMode::Nothing => "notify.mode.nothing",
    })
}

/// What the section shows, from the state and the settings: no drawing, so the rules are testable.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct SectionModel {
    /// The selected server and its mode and @everyone switch; `None` with no server (the first two rows are absent).
    pub server: Option<ServerRows>,
    pub desktop_on: bool,
    /// The OS permission blocks the switch (denied or unsupported): the hint says why.
    pub desktop_blocked: Option<&'static str>,
    pub sounds_on: bool,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ServerRows {
    pub id: CommunityId,
    pub name: String,
    pub mode: NotifyMode,
    /// The switch is on while @everyone and @here notify.
    pub everyone_notifies: bool,
}

/// The server the section is about: the one the view is on, else the first (Settings.tsx: `communityId ??
/// fallbackCommunity`).
fn server_of(state: &AppState, selected: Option<CommunityId>) -> Option<&Community> {
    selected
        .and_then(|id| state.communities.iter().find(|c| c.id == id))
        .or_else(|| state.communities.first())
}

/// Builds the section's model (`Notifications` in Notifications.tsx).
pub fn model(
    state: &AppState,
    selected: Option<CommunityId>,
    src: &dyn SettingsSource,
) -> SectionModel {
    let settings = Settings(src);
    let permission = state.notify.permission;
    let blocked = match permission {
        Permission::Denied => Some("notify.permission.denied"),
        Permission::Unsupported => Some("notify.permission.unsupported"),
        Permission::Granted | Permission::Default => None,
    };
    SectionModel {
        server: server_of(state, selected).map(|c| ServerRows {
            id: c.id,
            name: c.name.clone(),
            mode: server_mode_shown(src, c.id),
            everyone_notifies: src.get(&key_community_everyone(c.id)).as_deref() != Some("1"),
        }),
        desktop_on: permission == Permission::Granted && settings.desktop_on(),
        desktop_blocked: blocked,
        sounds_on: settings.sounds_on(),
    }
}

/// One change the section's controls make.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Change {
    ServerMode(CommunityId, NotifyMode),
    /// Whether the server's @everyone and @here notify.
    EveryoneNotifies(CommunityId, bool),
    Desktop(bool),
    Sounds(bool),
}

/// Where the controls report: the client in the app, a recorder in a test.
pub type Sink = Rc<dyn Fn(Change, &mut App)>;

/// Writes `change` through the client's notify handle (which keeps it in the preferences and redraws the screens).
pub fn apply(client: &Client, change: Change) {
    let notify = client.notify();
    let _ = match change {
        Change::ServerMode(id, mode) => notify.set_community_mode(id, Some(mode)),
        Change::EveryoneNotifies(id, on) => notify.set_everyone_notifies(id, on),
        Change::Desktop(on) => notify.set_desktop(on),
        Change::Sounds(on) => notify.set_sounds(on),
    };
}

/// A control with its automation id (the bounds the driver clicks); the id costs nothing in a release build.
fn probed(id: &'static str, control: impl IntoElement) -> AnyElement {
    #[cfg(feature = "automation")]
    {
        div()
            .relative()
            .child(control)
            .child(crate::automation::bounds_probe(id))
            .into_any_element()
    }
    #[cfg(not(feature = "automation"))]
    {
        let _ = id;
        control.into_any_element()
    }
}

/// Draws one of this area's settings sections (`slots::settings_sections` points here).
pub fn settings_section(
    _section: SectionId,
    ctx: &SlotCx,
    window: &mut Window,
    cx: &mut App,
) -> AnyElement {
    let Some(client) = ctx.client.clone() else {
        // No client (a fixture's window): the choices are kept in the window, written as the client's preferences
        // would be, so a click shows what it shows in the app.
        let local = window.use_keyed_state("notify-local-prefs", cx, |_, _| {
            HashMap::<String, String>::new()
        });
        let model = model(&ctx.state, ctx.community, local.read(cx));
        let sink: Sink = Rc::new(move |change, cx| {
            local.update(cx, |prefs, cx| {
                apply_local(prefs, change);
                cx.notify();
            });
        });
        return view(&model, sink);
    };
    let prefs = client.prefs();
    let model = model(&ctx.state, ctx.community, &prefs);
    let sink: Sink = Rc::new(move |change, _| apply(&client, change));
    view(&model, sink)
}

/// Writes `change` into `prefs` under the keys and values the client's notify handle writes (desktop notifications ask
/// the system first: without a client nothing asks, so they stay as they are).
fn apply_local(prefs: &mut HashMap<String, String>, change: Change) {
    let mut write = |key: String, value: Option<&str>| match value {
        Some(v) => {
            prefs.insert(key, v.to_owned());
        }
        None => {
            prefs.remove(&key);
        }
    };
    match change {
        Change::ServerMode(id, mode) => write(
            dilla_client::features::notify::key_community_mode(id),
            Some(mode.as_str()),
        ),
        Change::EveryoneNotifies(id, on) => write(
            dilla_client::features::notify::key_community_everyone(id),
            (!on).then_some("1"),
        ),
        Change::Sounds(on) => write(
            dilla_client::features::notify::KEY_SOUNDS.to_owned(),
            (!on).then_some("off"),
        ),
        Change::Desktop(_) => {}
    }
}

/// The section for `model`; the controls report their changes to `sink`.
pub fn view(model: &SectionModel, sink: Sink) -> AnyElement {
    let rows = 2 * usize::from(model.server.is_some()) + 2;
    let mut index = 0;
    let mut next = || {
        let at = GroupPosition::of(index, rows);
        index += 1;
        at
    };
    let mut group = div().flex().flex_col();

    if let Some(server) = &model.server {
        let id = server.id;
        let on_mode = sink.clone();
        let options: Vec<(&str, &str)> =
            MODES.iter().map(|m| (m.as_str(), mode_label(*m))).collect();
        group = group.child(
            SettingsRow::new("notify-server", server.name.clone())
                .icon(IconName::Bell)
                .hint(t("notify.server.hint"))
                .position(next())
                .child(probed(
                    "notify-server-mode",
                    Segmented::new("notify-server-mode", &options, server.mode.as_str()).on_change(
                        move |value, _, cx| {
                            if let Some(mode) = NotifyMode::parse(value) {
                                on_mode(Change::ServerMode(id, mode), cx);
                            }
                        },
                    ),
                )),
        );
        let on_everyone = sink.clone();
        group = group.child(
            SettingsRow::new("notify-everyone", t("notify.everyone.label"))
                .icon(IconName::AtSign)
                .hint(t("notify.everyone.hint"))
                .position(next())
                .child(probed(
                    "notify-everyone-switch",
                    Toggle::new(
                        "notify-everyone-switch",
                        t("notify.everyone.label"),
                        server.everyone_notifies,
                    )
                    .fit(Fit::Row)
                    .on_change(move |on, _, cx| on_everyone(Change::EveryoneNotifies(id, on), cx)),
                )),
        );
    }

    let on_desktop = sink.clone();
    let desktop_hint = model
        .desktop_blocked
        .map_or_else(|| t("notify.desktop.hint"), t);
    group = group.child(
        SettingsRow::new("notify-desktop", t("notify.desktop.label"))
            .icon(IconName::MonitorSmartphone)
            .hint(desktop_hint)
            .position(next())
            .child(probed(
                "notify-desktop-switch",
                Toggle::new(
                    "notify-desktop-switch",
                    t("notify.desktop.label"),
                    model.desktop_on,
                )
                .fit(Fit::Row)
                .disabled(model.desktop_blocked.is_some())
                .on_change(move |on, _, cx| on_desktop(Change::Desktop(on), cx)),
            )),
    );

    let on_sounds = sink;
    group = group.child(
        SettingsRow::new("notify-sounds", t("notify.sounds.label"))
            .icon(IconName::Volume2)
            .hint(t("notify.sounds.hint"))
            .position(next())
            .child(probed(
                "notify-sounds-switch",
                Toggle::new(
                    "notify-sounds-switch",
                    t("notify.sounds.label"),
                    model.sounds_on,
                )
                .fit(Fit::Row)
                .on_change(move |on, _, cx| on_sounds(Change::Sounds(on), cx)),
            )),
    );

    div()
        .id("notify-section")
        .flex()
        .flex_col()
        .child(section_header(t("notify.title"), Some(t("notify.sub"))))
        .child(group)
        .into_any_element()
}

/// The text `notify.title.dm` and friends as the client's notifier needs them: `strings::tf` as the client's [`Text`].
///
/// [`Text`]: dilla_client::features::notify::Text
pub fn text() -> dilla_client::features::notify::Text {
    std::sync::Arc::new(tf)
}

#[cfg(test)]
mod tests {
    use super::*;
    use dilla_client::features::notify::{KEY_DEFAULT, KEY_DESKTOP, KEY_SOUNDS};
    use dilla_client::state::UserId;

    fn state() -> AppState {
        AppState {
            communities: vec![
                Community {
                    id: CommunityId([1; 16]),
                    name: "Midgard Crew".into(),
                    owner: UserId([9; 16]),
                    policy_version: 0,
                },
                Community {
                    id: CommunityId([2; 16]),
                    name: "Northern Lights".into(),
                    owner: UserId([9; 16]),
                    policy_version: 0,
                },
            ],
            ..AppState::default()
        }
    }

    fn settings(pairs: &[(&str, &str)]) -> HashMap<String, String> {
        pairs
            .iter()
            .map(|(k, v)| ((*k).to_owned(), (*v).to_owned()))
            .collect()
    }

    #[test]
    fn the_rows_are_the_prototypes_four_and_two_with_no_server() {
        let m = model(&state(), None, &settings(&[]));
        let server = m.server.expect("the first server stands in");
        assert_eq!(server.name, "Midgard Crew");
        assert_eq!(server.mode, NotifyMode::Mentions);
        assert!(server.everyone_notifies);
        assert!(m.sounds_on);
        let m = model(&AppState::default(), None, &settings(&[]));
        assert!(m.server.is_none());
    }

    #[test]
    fn the_selected_server_wins_and_its_values_are_shown() {
        let selected = CommunityId([2; 16]);
        let src = settings(&[
            (KEY_DEFAULT, "everything"),
            (&key_community_everyone(selected), "1"),
        ]);
        let server = model(&state(), Some(selected), &src).server.unwrap();
        assert_eq!(server.name, "Northern Lights");
        assert_eq!(server.mode, NotifyMode::All, "the device default stands in");
        assert!(!server.everyone_notifies);
    }

    #[test]
    fn desktop_is_on_only_with_permission_and_not_switched_off() {
        let mut s = state();
        assert!(model(&s, None, &settings(&[])).desktop_on);
        assert!(!model(&s, None, &settings(&[(KEY_DESKTOP, "off")])).desktop_on);
        s.notify.permission = Permission::Default;
        let m = model(&s, None, &settings(&[]));
        assert!(!m.desktop_on && m.desktop_blocked.is_none());
        s.notify.permission = Permission::Denied;
        assert_eq!(
            model(&s, None, &settings(&[])).desktop_blocked,
            Some("notify.permission.denied")
        );
        s.notify.permission = Permission::Unsupported;
        assert_eq!(
            model(&s, None, &settings(&[])).desktop_blocked,
            Some("notify.permission.unsupported")
        );
        assert!(!model(&state(), None, &settings(&[(KEY_SOUNDS, "off")])).sounds_on);
    }
}
