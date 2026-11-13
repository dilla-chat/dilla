//! The desktop shell (packages/ui AppShell, Signal §5.1): the top bar over the server rail, the sidebar (the channel
//! list above the user panel) and the main column (the channel header, the message log, the composer), with the aside a
//! slot guest draws on its right. The panes' widths are the person's (a splitter each; widths and folded categories are
//! kept in prefs, `layout.*`); the shell keeps the route history. Sizes come from the web CSS (AppShell.css, TopBar.css,
//! CommunityRail.css, UserPanel.css, ChannelHeader.css) and Task 6's components.

use std::collections::HashSet;
use std::rc::Rc;
use std::sync::Arc;

use chrono::{Datelike, FixedOffset, TimeZone};
use dilla_client::Client;
use dilla_client::features::notify;
use dilla_client::prefs::{LayoutPrefs, Prefs};
use dilla_client::state::{AppState, ChannelId, CommunityId, ConnectionStatus};
use gpui::{
    AppContext as _, ClipboardItem, Context, Entity, FocusHandle, Focusable, InteractiveElement,
    IntoElement, MouseButton, MouseMoveEvent, MouseUpEvent, ParentElement, Pixels, Point, Render,
    ScrollHandle, SharedString, Styled, Subscription, Window, div,
};

use dilla_desktop_ui::components::action_menu::{ActionMenu, MenuItem};
use dilla_desktop_ui::components::shell::panes::{
    ASIDE_DEFAULT_MEMBERS, ASIDE_DEFAULT_THREAD, COLUMN_FROM, SIDEBAR_DEFAULT, range,
};
use dilla_desktop_ui::components::shell::{
    Pane, PaneInput, Panes, Splitter, SplitterKey, clamp_panes,
};
use dilla_desktop_ui::tokens::{color, font, layout};

pub mod keys;
pub mod resize;
pub mod window;

/// A preference that could not be kept is reported on stderr (the layout still works for this run).
fn warn(what: &str, result: Result<(), dilla_client::prefs::PrefsError>) {
    if let Err(e) = result {
        eprintln!("dilla: {what} was not kept: {e}");
    }
}

/// A button's handler: it gets the window and the app.
pub type Action = Rc<dyn Fn(&mut Window, &mut gpui::App)>;
/// A server tile's handler.
pub type PickServer = Rc<dyn Fn(CommunityId, &mut Window, &mut gpui::App)>;

use crate::router::{History, Route};
use crate::screens::composer::{Composer, Sent};
use crate::screens::header::{self, AsideKind};
use crate::screens::search::{self, Navigate, SearchEnv};
use crate::screens::{
    FocusNext, FocusPrevious, channel_list, rail, timeline, titlebar, user_panel,
};
use crate::slots::{self, SlotCx};
use crate::strings::{t, tf};

/// The clock and zone message times are drawn in.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Clock {
    /// The system clock, the local zone.
    Local,
    /// A fixed `now` (unix seconds), in UTC or the local zone: fixtures and tests.
    Fixed { now: u64, utc: bool },
}

impl Clock {
    pub fn now(self) -> u64 {
        match self {
            Self::Local => std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .map_or(0, |d| d.as_secs()),
            Self::Fixed { now, .. } => now,
        }
    }

    fn at(self, ts: u64) -> chrono::DateTime<FixedOffset> {
        let secs = i64::try_from(ts).unwrap_or(i64::MAX);
        let utc = chrono::Utc
            .timestamp_opt(secs, 0)
            .single()
            .unwrap_or_default();
        match self {
            Self::Fixed { utc: true, .. } => utc.fixed_offset(),
            _ => utc.with_timezone(&chrono::Local).fixed_offset(),
        }
    }

    /// The day `ts` falls on in this clock's zone, as a day count.
    pub fn day(self, ts: u64) -> i64 {
        i64::from(self.at(ts).date_naive().num_days_from_ce())
    }

    /// `ts` in this clock's zone, formatted by `f`.
    pub fn format(
        self,
        ts: u64,
        f: impl FnOnce(chrono::DateTime<FixedOffset>) -> String,
    ) -> String {
        f(self.at(ts))
    }
}

/// The labels of the channel row's menu that `strings.rs` has no key for (the web client's row menu holds pins, mute and
/// edit; the Wave 1 plan's holds mark read, mute and copy link). "copy link" is `invite.copy`'s.
const MARK_READ: &str = "Mark as read";

/// A pane drag in progress: where the pointer went down and the width the pane had then.
#[derive(Clone, Copy, Debug)]
struct Drag {
    pane: Pane,
    start_x: f32,
    start_width: f32,
}

/// The width a drag asks for: the pointer's travel since it went down, in the pane's own direction (the aside's splitter
/// is on its left edge, so moving left widens it).
fn dragged_width(drag: Drag, x: f32) -> f32 {
    let sign = if drag.pane == Pane::Sidebar {
        1.0
    } else {
        -1.0
    };
    drag.start_width + sign * (x - drag.start_x)
}

/// A row's context menu: which channel, and where it opens.
#[derive(Clone, Copy, Debug)]
struct ChannelMenu {
    channel: ChannelId,
    at: Point<Pixels>,
}

/// The shell: the signed-in client's state and what the person has selected.
pub struct ShellView {
    state: Arc<AppState>,
    client: Option<Rc<Client>>,
    community: Option<CommunityId>,
    channel: Option<ChannelId>,
    clock: Clock,
    composer: Entity<Composer>,
    log: ScrollHandle,
    /// The log follows its end (new messages scroll into view) until the person scrolls up.
    pinned: bool,
    focus: FocusHandle,
    /// The routes this window visited (router.ts's history).
    history: History,
    /// The instance's preferences; none while a fixture is drawn.
    prefs: Option<Prefs>,
    /// The pane widths the person chose (`layout.*`), shown clamped to the window.
    widths: LayoutPrefs,
    /// Hex ids of the folded categories (`layout.*`). Muted channels are the notify area's account setting
    /// (`notify.mute.channel.<id>`, Task 22), read from `prefs` on every draw.
    collapsed: HashSet<String>,
    drag: Option<Drag>,
    /// The aside that is open beside the main column.
    aside: Option<AsideKind>,
    menu: Option<ChannelMenu>,
    /// Something is drawn over the shell (the channel menu, a dialog or another overlay of `slots::overlays`), as of the
    /// last frame: Ctrl+K does not open the search palette over it (the web's guard; Task 23).
    overlay_open: bool,
    /// This device's microphone and speakers, as the panel's buttons last set them (the voice area owns the call).
    mic_off: bool,
    deaf: bool,
    sidebar_splitter: FocusHandle,
    aside_splitter: FocusHandle,
    /// The shipping window: the title bar draws the window's own controls.
    controls: bool,
    _subscriptions: Vec<Subscription>,
}

impl Focusable for ShellView {
    fn focus_handle(&self, _cx: &gpui::App) -> FocusHandle {
        self.focus.clone()
    }
}

impl ShellView {
    pub fn new(
        state: Arc<AppState>,
        client: Option<Rc<Client>>,
        channel: Option<ChannelId>,
        clock: Clock,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) -> Self {
        let composer = cx.new(|cx| Composer::new(window, cx));
        let sent = cx.subscribe(&composer, |this: &mut Self, _, Sent(text), cx| {
            this.send(text.clone(), cx);
        });
        let prefs = client.as_ref().map(|c| c.prefs());
        let (widths, collapsed) = match &prefs {
            Some(p) => (
                LayoutPrefs::read(p),
                LayoutPrefs::ids(p, LayoutPrefs::COLLAPSED)
                    .into_iter()
                    .collect(),
            ),
            None => Default::default(),
        };
        let mut this = Self {
            state,
            client,
            community: None,
            channel,
            clock,
            composer,
            log: ScrollHandle::new(),
            pinned: true,
            focus: cx.focus_handle(),
            history: History::new(Route::Root),
            prefs,
            widths,
            collapsed,
            drag: None,
            aside: None,
            menu: None,
            overlay_open: false,
            mic_off: false,
            deaf: false,
            sidebar_splitter: cx.focus_handle().tab_stop(true),
            aside_splitter: cx.focus_handle().tab_stop(true),
            controls: false,
            _subscriptions: vec![sent],
        };
        this.settle_selection(cx);
        let route = this.current_route();
        this.history = History::new(route);
        this.log.scroll_to_bottom();
        this
    }

    /// Draws the window's own controls in the title bar (the shipping window; a fixture has none).
    pub fn with_controls(mut self, controls: bool) -> Self {
        self.controls = controls;
        self
    }

    /// Keeps and reads the layout (pane widths, folded categories, muted channels) in `prefs`: what a running client's own
    /// preferences do, for a shell built without a client.
    pub fn with_prefs(mut self, prefs: Prefs) -> Self {
        self.widths = LayoutPrefs::read(&prefs);
        self.collapsed = LayoutPrefs::ids(&prefs, LayoutPrefs::COLLAPSED)
            .into_iter()
            .collect();
        self.prefs = Some(prefs);
        self
    }

    /// A new state from the client.
    pub fn set_state(&mut self, state: Arc<AppState>, cx: &mut Context<Self>) {
        let grew = self.channel.is_some_and(|c| {
            state.timelines.get(&c).map(|t| t.items.len())
                != self.state.timelines.get(&c).map(|t| t.items.len())
        });
        self.state = state;
        self.settle_selection(cx);
        if grew {
            self.pinned = true;
        }
        cx.notify();
    }

    /// Keeps the selection valid: the first community, and its first text channel when none is chosen (the web opens a
    /// server on its first channel).
    fn settle_selection(&mut self, cx: &mut Context<Self>) {
        if let Some(channel) = self.channel.and_then(|c| self.state.channel(c)) {
            self.community = Some(channel.community_id);
        }
        if self
            .community
            .is_none_or(|c| !self.state.communities.iter().any(|k| k.id == c))
        {
            self.community = self.state.communities.first().map(|c| c.id);
        }
        let valid = self
            .channel
            .and_then(|c| self.state.channel(c))
            .is_some_and(|c| Some(c.community_id) == self.community);
        if !valid {
            let first = self.community.and_then(|community| {
                channel_list::ordered(&self.state, community)
                    .into_iter()
                    .find(|c| c.is_text())
                    .map(|c| c.id)
            });
            self.channel = None;
            if let Some(first) = first {
                self.focus_channel(first, cx);
            }
        }
        let target: SharedString = self
            .channel
            .and_then(|c| self.state.channel(c))
            .map_or_else(String::new, |c| format!("#{}", c.name))
            .into();
        let slots = self.slot_cx();
        self.composer.update(cx, |composer, cx| {
            composer.set_slots(slots);
            composer.set_target(target, cx);
        });
    }

    /// What the slots this shell hosts draw with.
    fn slot_cx(&self) -> SlotCx {
        SlotCx {
            state: self.state.clone(),
            client: self.client.clone(),
            community: self.community,
            channel: self.channel,
            clock: self.clock,
        }
    }

    /// The route the selection stands on.
    fn current_route(&self) -> Route {
        match self.community {
            Some(community) => Route::Channel {
                community,
                channel: self.channel,
            },
            None => Route::Root,
        }
    }

    /// The route the shell shows (the head of its history).
    pub fn route(&self) -> &Route {
        self.history.current()
    }

    /// Goes to `route`: a channel route selects its channel (and server), a direct-message route only marks the `@` tile
    /// (the DM list and conversations are the DM area's), and every route is an entry of the history.
    pub fn navigate(&mut self, route: Route, cx: &mut Context<Self>) {
        self.history.push(route.clone());
        self.apply(&route, cx);
    }

    fn apply(&mut self, route: &Route, cx: &mut Context<Self>) {
        match route {
            Route::Channel { community, channel } => {
                if self.community != Some(*community) {
                    self.community = Some(*community);
                    self.channel = None;
                }
                match channel {
                    Some(c) => self.focus_channel(*c, cx),
                    None => self.settle_selection(cx),
                }
            }
            Route::Root => self.settle_selection(cx),
            _ => {}
        }
        cx.notify();
    }

    /// One step back in the history (Alt+Left); the route now shown, or `None` at the first entry.
    pub fn back(&mut self, cx: &mut Context<Self>) -> Option<Route> {
        let route = self.history.back()?.clone();
        self.apply(&route, cx);
        Some(route)
    }

    /// One step forward (Alt+Right).
    pub fn forward(&mut self, cx: &mut Context<Self>) -> Option<Route> {
        let route = self.history.forward()?.clone();
        self.apply(&route, cx);
        Some(route)
    }

    /// Shows `channel`: its group is opened (joined) so its timeline is kept, and it is marked read while shown.
    fn focus_channel(&mut self, channel: ChannelId, cx: &mut Context<Self>) {
        if self.channel == Some(channel) {
            return;
        }
        self.channel = Some(channel);
        self.pinned = true;
        let slots = self.slot_cx();
        self.composer
            .update(cx, |composer, _| composer.set_slots(slots));
        if let Some(client) = &self.client {
            client.focus(Some(channel));
            let opened = client.open_channel(channel);
            cx.background_spawn(async move {
                let _ = opened.await;
            })
            .detach();
        }
        cx.notify();
    }

    /// Shows `channel` as a new history entry.
    pub fn select(&mut self, channel: ChannelId, cx: &mut Context<Self>) {
        if self.channel == Some(channel) {
            return;
        }
        let community = self
            .state
            .channel(channel)
            .map(|c| c.community_id)
            .or(self.community);
        self.focus_channel(channel, cx);
        if let Some(community) = community {
            self.community = Some(community);
            self.history.push(Route::Channel {
                community,
                channel: Some(channel),
            });
        }
    }

    /// Opens what a search result names (Task 23). `false` when it is gone, or is a screen the shell does not draw yet
    /// (the settings, the direct messages and the voice mute arrive with their areas, which add their arms here).
    fn open_search_result(&mut self, target: Navigate, cx: &mut Context<Self>) -> bool {
        let open = |this: &mut Self,
                    community: CommunityId,
                    channel: ChannelId,
                    cx: &mut Context<Self>| {
            if this
                .state
                .channel(channel)
                .is_none_or(|c| c.community_id != community)
            {
                return false;
            }
            this.pick_community(community, cx);
            this.select(channel, cx);
            true
        };
        match target {
            Navigate::Channel { community, channel } => open(self, community, channel, cx),
            Navigate::Message(hit) => match hit.community {
                Some(community) => open(self, community, hit.channel, cx),
                None => false,
            },
            _ => false,
        }
    }

    /// Shows server `community` on its first text channel.
    pub fn pick_community(&mut self, community: CommunityId, cx: &mut Context<Self>) {
        if self.community == Some(community) {
            return;
        }
        self.community = Some(community);
        self.channel = None;
        self.settle_selection(cx);
        self.history.push(self.current_route());
        cx.notify();
    }

    /// Alt+Up and Alt+Down: the previous or next listed channel.
    pub fn step_channel(&mut self, delta: i32, cx: &mut Context<Self>) {
        let Some(community) = self.community else {
            return;
        };
        if let Some(next) = channel_list::step_channel(&self.state, community, self.channel, delta)
        {
            self.select(next, cx);
        }
    }

    fn send(&mut self, text: String, cx: &mut Context<Self>) {
        let (Some(client), Some(channel)) = (&self.client, self.channel) else {
            return;
        };
        let queued = client.send_text(channel, text);
        self.pinned = true;
        cx.background_spawn(async move {
            let _ = queued.await;
        })
        .detach();
    }

    // ---- panes ------------------------------------------------------------------------------------------------

    /// The aside column's default width: the thread pane is wider than the member list.
    fn aside_default(&self) -> f32 {
        if self.aside == Some(AsideKind::Threads) {
            ASIDE_DEFAULT_THREAD
        } else {
            ASIDE_DEFAULT_MEMBERS
        }
    }

    /// What the panes' widths are limited by in a shell `width` wide, with a column aside or without.
    fn pane_input(&self, width: f32, aside_column: bool) -> PaneInput {
        PaneInput {
            shell: width,
            rail: f32::from(layout::RAIL),
            sidebar: self.widths.sidebar.unwrap_or(SIDEBAR_DEFAULT),
            aside: self.widths.aside.unwrap_or_else(|| self.aside_default()),
            aside_column,
        }
    }

    /// The widths shown in a window `width` wide.
    pub fn panes(&self, width: f32, aside_column: bool) -> Panes {
        clamp_panes(&self.pane_input(width, aside_column))
    }

    /// Sets a pane to `width` (the request is kept as asked; the shell shows it clamped), or back to its default.
    fn set_pane(
        &mut self,
        pane: Pane,
        width: Option<f32>,
        window_width: f32,
        cx: &mut Context<Self>,
    ) {
        let column = window_width >= COLUMN_FROM;
        let input = self.pane_input(window_width, column);
        let value = width.map(|w| {
            let other = match pane {
                Pane::Sidebar => self.panes(window_width, column).aside,
                Pane::Aside => self.panes(window_width, column).sidebar,
            };
            let (lo, hi) = range(pane, &input, other);
            w.clamp(lo, hi)
        });
        match pane {
            Pane::Sidebar => self.widths.sidebar = value,
            Pane::Aside => self.widths.aside = value,
        }
        cx.notify();
    }

    /// Keeps the widths (the drag ended, or a key moved a splitter).
    fn commit_widths(&self) {
        if let Some(prefs) = &self.prefs {
            warn(
                "the sidebar width",
                LayoutPrefs::write_sidebar(prefs, self.widths.sidebar),
            );
            warn(
                "the aside width",
                LayoutPrefs::write_aside(prefs, self.widths.aside),
            );
        }
    }

    fn begin_drag(&mut self, pane: Pane, x: f32, window_width: f32) {
        let panes = self.panes(window_width, window_width >= COLUMN_FROM);
        self.drag = Some(Drag {
            pane,
            start_x: x,
            start_width: match pane {
                Pane::Sidebar => panes.sidebar,
                Pane::Aside => panes.aside,
            },
        });
    }

    fn on_splitter_key(
        &mut self,
        pane: Pane,
        key: SplitterKey,
        window_width: f32,
        cx: &mut Context<Self>,
    ) {
        match key {
            SplitterKey::To(w) => self.set_pane(pane, Some(w), window_width, cx),
            SplitterKey::Reset => self.set_pane(pane, None, window_width, cx),
        }
        self.commit_widths();
    }

    /// The header's Threads and Members buttons: open the aside, or close it when it is the one open. The areas that draw
    /// the panes (11, 14) keep their own state and are told.
    fn toggle_aside(&mut self, kind: AsideKind, cx: &mut Context<Self>) {
        self.aside = if self.aside == Some(kind) {
            None
        } else {
            Some(kind)
        };
        match kind {
            AsideKind::Threads => crate::screens::threads::toggle_pane(cx),
            AsideKind::Members => crate::screens::members::toggle_pane(cx),
        }
        cx.notify();
    }

    // ---- the channel row's menu -------------------------------------------------------------------------------

    fn open_menu(&mut self, channel: ChannelId, at: Point<Pixels>, cx: &mut Context<Self>) {
        self.menu = Some(ChannelMenu { channel, at });
        cx.notify();
    }

    /// The notify settings the rows and the menu read (Task 22): the instance's preferences, or none for a fixture.
    fn channel_muted(&self, channel: ChannelId) -> bool {
        self.prefs
            .as_ref()
            .is_some_and(|p| notify::is_muted(p, channel, None))
    }

    fn menu_items(&self, channel: ChannelId) -> Vec<MenuItem> {
        let muted = self.channel_muted(channel);
        vec![
            MenuItem::new("read", MARK_READ),
            MenuItem::new("mute", t("notify.mute")).check(muted),
            MenuItem::new("link", t("invite.copy")),
        ]
    }

    fn choose(&mut self, channel: ChannelId, item: &str, cx: &mut Context<Self>) {
        match item {
            "read" => {
                if let Some(client) = &self.client {
                    let marked = client.mark_read(channel);
                    cx.background_spawn(async move {
                        let _ = marked.await;
                    })
                    .detach();
                }
            }
            "mute" => {
                // The account setting the web's channel menu writes (Task 22's `set_channel_muted`), not a layout key.
                let on = !self.channel_muted(channel);
                if let Some(client) = &self.client {
                    warn(
                        "the muted channels",
                        client.notify().set_channel_muted(channel, on),
                    );
                } else if let Some(prefs) = &self.prefs {
                    // A shell without a client (tests): the same key in the same preferences.
                    let key = notify::key_channel_mute(channel);
                    warn(
                        "the muted channels",
                        if on {
                            prefs.set(&key, &"1".to_owned())
                        } else {
                            prefs.remove(&key)
                        },
                    );
                }
            }
            "link" => {
                if let Some(community) = self.state.channel(channel).map(|c| c.community_id) {
                    let route = Route::Channel {
                        community,
                        channel: Some(channel),
                    };
                    cx.write_to_clipboard(ClipboardItem::new_string(route.path()));
                }
            }
            _ => {}
        }
        self.menu = None;
        cx.notify();
    }

    fn toggle_category(&mut self, category: ChannelId, cx: &mut Context<Self>) {
        let on = !self.collapsed.contains(&category.hex());
        if on {
            self.collapsed.insert(category.hex());
        } else {
            self.collapsed.remove(&category.hex());
        }
        if let Some(prefs) = &self.prefs {
            warn(
                "the folded categories",
                LayoutPrefs::set_id(prefs, LayoutPrefs::COLLAPSED, &category.hex(), on),
            );
        }
        cx.notify();
    }

    // ---- the user panel ---------------------------------------------------------------------------------------

    fn set_mic(&mut self, cx: &mut Context<Self>) {
        self.mic_off = !self.mic_off;
        if let Some(client) = &self.client {
            client.voice().set_muted(self.mic_off);
        }
        cx.notify();
    }

    fn set_deaf(&mut self, cx: &mut Context<Self>) {
        self.deaf = !self.deaf;
        if let Some(client) = &self.client {
            client.voice().set_deafened(self.deaf);
        }
        cx.notify();
    }

    fn me(&self) -> (String, &'static str) {
        let user = self.state.account.user_id;
        let me = self
            .community
            .and_then(|c| self.state.members.get(&c))
            .and_then(|m| m.iter().find(|m| Some(m.user_id) == user));
        let name = me.map_or_else(
            || self.state.account.username.clone(),
            |m| {
                if m.display.is_empty() {
                    m.username.clone()
                } else {
                    m.display.clone()
                }
            },
        );
        let status = match self.state.connection.status {
            ConnectionStatus::Online => "online",
            ConnectionStatus::Connecting => "connecting",
            ConnectionStatus::Offline => "offline",
        };
        (name, status)
    }
}

/// The ⌘K / Ctrl K hint of the search field.
const SEARCH_HINT: &str = if cfg!(target_os = "macos") {
    "⌘ K"
} else {
    "Ctrl K"
};

impl Render for ShellView {
    fn render(&mut self, window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let state = self.state.clone();
        let channel = self.channel.and_then(|c| state.channel(c));
        let members = self
            .community
            .and_then(|c| state.members.get(&c))
            .map_or(0, Vec::len);
        let online = usize::from(state.connection.status == ConnectionStatus::Online);
        let (me, status) = self.me();
        let ctx = self.slot_cx();
        let shell = cx.entity().downgrade();
        let window_width = f32::from(window.viewport_size().width);
        let column = window_width >= COLUMN_FROM;
        let panes = self.panes(window_width, column);

        let weak = shell.clone();
        let on_pick = Rc::new(move |id, _: &mut Window, cx: &mut gpui::App| {
            let _ = weak.update(cx, |shell, cx| shell.pick_community(id, cx));
        });
        let weak = shell.clone();
        let on_home = Rc::new(move |_: &mut Window, cx: &mut gpui::App| {
            let _ = weak.update(cx, |shell, cx| shell.navigate(Route::Dms, cx));
        });
        let no_settings = std::collections::HashMap::<String, String>::new();
        let settings: &dyn notify::SettingsSource = match &self.prefs {
            Some(p) => p,
            None => &no_settings,
        };
        // The rail's sums hide the unread of muted channels and servers (Task 22's `is_muted`).
        let muted: HashSet<String> = state
            .channels
            .iter()
            .filter(|c| notify::is_muted(settings, c.id, Some(c.community_id)))
            .map(|c| c.id.hex())
            .collect();
        let rail_view = rail::render(
            &rail::RailModel {
                state: &state,
                selected: self.community,
                home_selected: matches!(self.route(), Route::Dms | Route::Dm(_)),
                muted: &muted,
            },
            &rail::RailHandlers {
                on_pick,
                on_home,
                // The add-a-server dialog is the onboarding area's (Task 25); the controller wires its hook here.
                on_add: Rc::new(|_, _| {}),
            },
            window,
            cx,
        );

        let owner = self.community.is_some_and(|c| {
            state
                .communities
                .iter()
                .any(|k| k.id == c && Some(k.owner) == state.account.user_id)
        });
        let has_categories = self.community.is_some_and(|c| {
            state
                .channels
                .iter()
                .any(|ch| ch.community_id == c && ch.kind == 2)
        });
        let handlers = {
            let (w1, w2, w4) = (shell.clone(), shell.clone(), shell.clone());
            let community = self.community;
            channel_list::ListHandlers {
                on_select: Rc::new(move |id, _, cx| {
                    let _ = w1.update(cx, |shell, cx| shell.select(id, cx));
                }),
                on_toggle: Rc::new(move |id, _, cx| {
                    let _ = w2.update(cx, |shell, cx| shell.toggle_category(id, cx));
                }),
                on_add: Rc::new(move |_category, _, cx| {
                    // The create-channel flow is the onboarding area's (Task 25); its hook takes the server only.
                    if let Some(community) = community {
                        crate::screens::create::open_create_channel(community, cx);
                    }
                }),
                on_menu: Rc::new(move |id, at, _, cx| {
                    let _ = w4.update(cx, |shell, cx| shell.open_menu(id, at, cx));
                }),
            }
        };
        let list = channel_list::render(
            &channel_list::ListModel {
                state: &state,
                community: self.community,
                selected: self.channel,
                online,
                in_voice: 0,
                can_add: owner && !has_categories,
                can_add_in_category: owner && has_categories,
                collapsed: &self.collapsed,
                settings,
            },
            &handlers,
            window,
            cx,
        );

        let (w_mic, w_deaf) = (shell.clone(), shell.clone());
        let panel = user_panel::render(
            &user_panel::PanelModel {
                name: &me,
                status,
                muted: self.mic_off,
                deafened: self.deaf,
            },
            &user_panel::PanelHandlers {
                on_mute: Rc::new(move |_, cx| {
                    let _ = w_mic.update(cx, |shell, cx| shell.set_mic(cx));
                }),
                on_deafen: Rc::new(move |_, cx| {
                    let _ = w_deaf.update(cx, |shell, cx| shell.set_deaf(cx));
                }),
            },
        );
        let dock = slots::dock(window, cx);
        let sidebar = div()
            .relative()
            .flex_none()
            .w(Pixels::from(panes.sidebar))
            .h_full()
            .flex()
            .flex_col()
            .child(
                div()
                    .flex_1()
                    .min_h_0()
                    .bg(color::BG_2)
                    .border_r_1()
                    .border_color(color::HAIRLINE)
                    .child(list),
            )
            // The voice dock and now playing (17, 21) over the user panel.
            .children(dock)
            .child(panel);
        #[cfg(feature = "automation")]
        let sidebar = sidebar.child(crate::automation::bounds_probe("shell-sidebar"));

        // The log wraps its bodies to its own width: the window less the rail, the sidebar and a column aside.
        let aside = slots::aside(self.route(), window, cx);
        let aside_width = if aside.is_some() && column {
            panes.aside
        } else {
            0.0
        };
        let log_width = window_width - f32::from(layout::RAIL) - panes.sidebar - aside_width;
        let log = match channel {
            Some(c) => {
                let weak = shell.clone();
                let scroll = self.log.clone();
                let on_wheel: timeline::OnWheel = Rc::new(move |up, cx| {
                    // Up leaves the end; reaching the end again follows it.
                    let at_end = scroll.offset().y <= -scroll.max_offset().y + Pixels::from(1.0f32);
                    let _ = weak.update(cx, |shell, cx| {
                        shell.pinned = !up && at_end;
                        cx.notify();
                    });
                });
                timeline::render(
                    &state,
                    c,
                    self.clock,
                    log_width,
                    &self.log,
                    self.pinned,
                    on_wheel,
                    &ctx,
                    window,
                    cx,
                )
                .into_any_element()
            }
            None => div().size_full().bg(color::BG).into_any_element(),
        };
        let banners = slots::banners(&ctx, window, cx);
        // What the search palette (23) draws with, and how it opens a result.
        let host = cx.entity().downgrade();
        search::provide(
            SearchEnv {
                ctx: ctx.clone(),
                navigate: Rc::new(move |target, _, cx| {
                    host.update(cx, |shell, cx| shell.open_search_result(target, cx))
                        .unwrap_or(false)
                }),
            },
            cx,
        );
        let mut overlays = slots::overlays(window, cx);
        if let Some(menu) = self.menu {
            let (w_sel, w_open) = (shell.clone(), shell.clone());
            let name = state
                .channel(menu.channel)
                .map_or_else(String::new, |c| c.name.clone());
            overlays.push(
                ActionMenu::new(
                    "channel-menu",
                    tf("nav.channel.menuOf", &[("name", name.as_str())]),
                    self.menu_items(menu.channel),
                )
                .open(true)
                .at(menu.at)
                .no_trigger(None)
                .on_select(move |item, _, cx| {
                    let id = menu.channel;
                    let _ = w_sel.update(cx, |shell, cx| shell.choose(id, item, cx));
                })
                .on_open_change(move |open, _, cx| {
                    if !open {
                        let _ = w_open.update(cx, |shell, cx| {
                            shell.menu = None;
                            cx.notify();
                        });
                    }
                })
                .into_any_element(),
            );
        }
        // The open palette is one of these too; a second Ctrl+K over it is ignored either way.
        self.overlay_open = !overlays.is_empty();

        let weak = shell.clone();
        let on_toggle: header::OnToggle = Rc::new(move |kind, _, cx| {
            let _ = weak.update(cx, |shell, cx| shell.toggle_aside(kind, cx));
        });
        let main = div()
            .flex_1()
            .min_w_0()
            .h_full()
            .flex()
            .flex_col()
            .child(header::render(
                &header::HeaderModel {
                    channel,
                    members,
                    aside: self.aside,
                },
                &on_toggle,
            ))
            .child(div().flex_1().min_h_0().child(log))
            .child(div().flex_none().child(self.composer.clone()));
        #[cfg(feature = "automation")]
        let main = main
            .relative()
            .child(crate::automation::bounds_probe("shell-main"));

        // The aside is a column from 64rem of window; narrower it is the slot guest's own overlay.
        let aside_column = aside.map(|a| {
            div()
                .flex_none()
                .w(Pixels::from(panes.aside))
                .h_full()
                .child(a)
        });

        let (w_press, w_set) = (shell.clone(), shell.clone());
        let sidebar_input = self.pane_input(window_width, column);
        let (lo, hi) = range(Pane::Sidebar, &sidebar_input, panes.aside);
        let sidebar_splitter =
            Splitter::new("splitter-sidebar", Pane::Sidebar, &self.sidebar_splitter)
                .at(f32::from(layout::RAIL) + panes.sidebar
                    - dilla_desktop_ui::components::shell::splitter::HIT / 2.0)
                .range(panes.sidebar, lo, hi)
                .on_press(move |x, _, cx| {
                    let _ = w_press.update(cx, |shell, _| {
                        shell.begin_drag(Pane::Sidebar, f32::from(x), window_width)
                    });
                })
                .on_set(move |key, _, cx| {
                    let _ = w_set.update(cx, |shell, cx| {
                        shell.on_splitter_key(Pane::Sidebar, key, window_width, cx)
                    });
                });
        #[cfg(feature = "automation")]
        let sidebar_splitter = sidebar_splitter.probe(
            div()
                .absolute()
                .size_full()
                .child(crate::automation::bounds_probe("splitter-sidebar"))
                .child(crate::automation::focus_probe(
                    "splitter-sidebar",
                    &self.sidebar_splitter,
                )),
        );
        let aside_splitter = (aside_column.is_some() && column).then(|| {
            let (w_press, w_set) = (shell.clone(), shell.clone());
            let (lo, hi) = range(Pane::Aside, &sidebar_input, panes.sidebar);
            let splitter = Splitter::new("splitter-aside", Pane::Aside, &self.aside_splitter)
                .at(window_width
                    - panes.aside
                    - dilla_desktop_ui::components::shell::splitter::HIT / 2.0)
                .range(panes.aside, lo, hi)
                .on_press(move |x, _, cx| {
                    let _ = w_press.update(cx, |shell, _| {
                        shell.begin_drag(Pane::Aside, f32::from(x), window_width)
                    });
                })
                .on_set(move |key, _, cx| {
                    let _ = w_set.update(cx, |shell, cx| {
                        shell.on_splitter_key(Pane::Aside, key, window_width, cx)
                    });
                });
            #[cfg(feature = "automation")]
            let splitter = splitter.probe(
                div()
                    .absolute()
                    .size_full()
                    .child(crate::automation::bounds_probe("splitter-aside"))
                    .child(crate::automation::focus_probe(
                        "splitter-aside",
                        &self.aside_splitter,
                    )),
            );
            splitter
        });

        let weak = shell.clone();
        let titlebar = titlebar::render(
            &titlebar::TitlebarModel {
                controls: self.controls,
                search_hint: SEARCH_HINT,
            },
            &titlebar::TitlebarHandlers {
                on_settings: Rc::new(move |_, cx| {
                    let _ = weak.update(cx, |shell, cx| shell.navigate(Route::Settings(None), cx));
                }),
                // The search field opens the palette (23).
                on_search: Rc::new(|_, cx| search::request_open(cx)),
            },
        );

        div()
            .id("shell")
            .key_context(keys::CONTEXT)
            .track_focus(&self.focus)
            // Ctrl+K (23) does not open the palette over the channel menu or another overlay (the web's guard).
            .capture_action(cx.listener(|this, _: &search::keys::OpenSearch, _, cx| {
                if this.overlay_open {
                    cx.stop_propagation();
                }
            }))
            .on_action(|_: &FocusNext, window, cx| window.focus_next(cx))
            .on_action(|_: &FocusPrevious, window, cx| window.focus_prev(cx))
            .on_action(
                cx.listener(|this, _: &keys::PreviousChannel, _, cx| this.step_channel(-1, cx)),
            )
            .on_action(cx.listener(|this, _: &keys::NextChannel, _, cx| this.step_channel(1, cx)))
            .on_action(cx.listener(|this, _: &keys::HistoryBack, _, cx| {
                this.back(cx);
            }))
            .on_action(cx.listener(|this, _: &keys::HistoryForward, _, cx| {
                this.forward(cx);
            }))
            .on_mouse_move(cx.listener(move |this, e: &MouseMoveEvent, _, cx| {
                if let Some(drag) = this.drag
                    && e.dragging()
                {
                    this.set_pane(
                        drag.pane,
                        Some(dragged_width(drag, f32::from(e.position.x))),
                        window_width,
                        cx,
                    );
                }
            }))
            .on_mouse_up(
                MouseButton::Left,
                cx.listener(|this, _: &MouseUpEvent, _, _| {
                    if this.drag.take().is_some() {
                        this.commit_widths();
                    }
                }),
            )
            .size_full()
            .bg(color::BG)
            .font_family(font::MONO)
            .flex()
            .flex_col()
            .child(titlebar)
            // The offline link and version banners (24): across the whole window, under the top bar.
            .children(banners)
            .child(
                div()
                    .relative()
                    .flex_1()
                    .min_h_0()
                    .flex()
                    .child(rail_view)
                    .child(sidebar)
                    .child(main)
                    // The thread or members pane (11, 14).
                    .children(aside_column)
                    .child(sidebar_splitter)
                    .children(aside_splitter),
            )
            // The search palette, the ring banner (23, 18) and the channel menu, over everything.
            .children(overlays)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_drag_moves_the_sidebar_with_the_pointer_and_the_aside_against_it() {
        let side = Drag {
            pane: Pane::Sidebar,
            start_x: 300.0,
            start_width: 244.0,
        };
        assert_eq!(dragged_width(side, 380.0), 324.0);
        let aside = Drag {
            pane: Pane::Aside,
            start_x: 900.0,
            start_width: 232.0,
        };
        assert_eq!(
            dragged_width(aside, 860.0),
            272.0,
            "moving left widens the aside"
        );
    }

    use dilla_client::state::{Channel, ChannelGroupState, Community, UserId};
    use gpui::VisualContext as _;

    fn channel(n: u8, name: &str, position: u32) -> Channel {
        Channel {
            id: ChannelId([n; 16]),
            community_id: CommunityId([1; 16]),
            kind: 0,
            mode: 0,
            name: name.to_owned(),
            topic: String::new(),
            parent_id: None,
            position,
            slowmode: 0,
            text_group_id: None,
            group: ChannelGroupState::Active,
        }
    }

    fn world() -> Arc<AppState> {
        let mut s = AppState::default();
        s.communities.push(Community {
            id: CommunityId([1; 16]),
            name: "Midgard".into(),
            owner: UserId([9; 16]),
            policy_version: 0,
        });
        s.channels = vec![channel(2, "general", 0), channel(3, "random", 1)];
        Arc::new(s)
    }

    fn scratch(tag: &str) -> std::path::PathBuf {
        let dir = std::env::temp_dir().join(format!("dilla-shell-{tag}-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        dir
    }

    /// e2e/web/panes.spec.ts `the sidebar splitter drags and takes keys, the width survives a reload`: a width the splitter
    /// sets is kept in the instance's prefs, a new shell on them shows it again, and Enter forgets it.
    #[gpui::test]
    fn sidebar_splitter_drags_takes_keys_and_persists(cx: &mut gpui::TestAppContext) {
        cx.update(crate::install);
        let dir = scratch("panes");
        let prefs = Prefs::instance(&dir);
        let state = world();
        let make = |prefs: Prefs, cx: &mut gpui::TestAppContext| {
            let state = state.clone();
            cx.add_window_view(move |window, cx| {
                ShellView::new(
                    state,
                    None,
                    None,
                    Clock::Fixed { now: 0, utc: true },
                    window,
                    cx,
                )
                .with_prefs(prefs)
            })
            .0
        };
        let shell = make(prefs.clone(), cx);
        let vcx = &mut gpui::VisualTestContext::from_window(cx.windows()[0], cx);
        // A drag of +80 px, as the pointer's travel gives it, lands 80 wider and is kept.
        let drag = Drag {
            pane: Pane::Sidebar,
            start_x: 300.0,
            start_width: 244.0,
        };
        shell.update(vcx, |s, cx| {
            s.set_pane(Pane::Sidebar, Some(dragged_width(drag, 380.0)), 1280.0, cx);
            s.commit_widths();
            assert_eq!(s.panes(1280.0, true).sidebar, 324.0);
        });
        assert_eq!(LayoutPrefs::read(&prefs).sidebar, Some(324.0));
        // A new shell on the same preferences (the reload) shows it.
        let again = make(Prefs::instance(&dir), cx);
        let vcx = &mut gpui::VisualTestContext::from_window(cx.windows()[1], cx);
        again.update(vcx, |s, cx| {
            assert_eq!(s.panes(1280.0, true).sidebar, 324.0);
            // Keys: Enter resets to 244, ArrowRight twice is two rem, Home the minimum, End the maximum.
            s.on_splitter_key(Pane::Sidebar, SplitterKey::Reset, 1280.0, cx);
            assert_eq!(s.panes(1280.0, true).sidebar, 244.0);
            let sidebar = |s: &ShellView| s.panes(1280.0, true).sidebar;
            for _ in 0..2 {
                let now = sidebar(s);
                s.on_splitter_key(Pane::Sidebar, SplitterKey::To(now + 16.0), 1280.0, cx);
            }
            assert_eq!(sidebar(s), 276.0);
            s.on_splitter_key(Pane::Sidebar, SplitterKey::To(0.0), 1280.0, cx);
            assert_eq!(sidebar(s), 192.0, "Home: the 12rem minimum");
            s.on_splitter_key(Pane::Sidebar, SplitterKey::To(9999.0), 1280.0, cx);
            assert_eq!(sidebar(s), 384.0, "End: the 24rem maximum");
            s.on_splitter_key(Pane::Sidebar, SplitterKey::Reset, 1280.0, cx);
        });
        assert_eq!(LayoutPrefs::read(&Prefs::instance(&dir)).sidebar, None);
        let _ = std::fs::remove_dir_all(&dir);
    }

    /// A folded category and a muted channel are kept per device, in the layout preferences.
    #[gpui::test]
    fn folds_and_mutes_persist(cx: &mut gpui::TestAppContext) {
        cx.update(crate::install);
        let dir = scratch("folds");
        let state = world();
        let (shell, vcx) = cx.add_window_view({
            let (state, prefs) = (state.clone(), Prefs::instance(&dir));
            move |window, cx| {
                ShellView::new(
                    state,
                    None,
                    None,
                    Clock::Fixed { now: 0, utc: true },
                    window,
                    cx,
                )
                .with_prefs(prefs)
            }
        });
        shell.update(vcx, |s, cx| {
            s.toggle_category(ChannelId([4; 16]), cx);
            s.choose(ChannelId([3; 16]), "mute", cx);
            assert!(s.channel_muted(ChannelId([3; 16])));
        });
        let prefs = Prefs::instance(&dir);
        assert_eq!(
            LayoutPrefs::ids(&prefs, LayoutPrefs::COLLAPSED),
            [ChannelId([4; 16]).hex()]
        );
        // The mute is the notify area's account setting (Task 22), not a layout key.
        assert!(LayoutPrefs::ids(&prefs, LayoutPrefs::MUTED).is_empty());
        assert!(notify::is_muted(&prefs, ChannelId([3; 16]), None));
        shell.update(vcx, |s, cx| {
            s.choose(ChannelId([3; 16]), "mute", cx);
            s.toggle_category(ChannelId([4; 16]), cx);
        });
        let prefs = Prefs::instance(&dir);
        assert!(!notify::is_muted(&prefs, ChannelId([3; 16]), None));
        assert!(LayoutPrefs::ids(&prefs, LayoutPrefs::COLLAPSED).is_empty());
        let _ = std::fs::remove_dir_all(&dir);
    }

    /// Route history: selecting a channel is an entry, back and forward step through them, Alt+Up and Alt+Down step the list.
    #[gpui::test]
    fn route_history_and_channel_steps(cx: &mut gpui::TestAppContext) {
        cx.update(crate::install);
        let state = world();
        let (shell, vcx) = cx.add_window_view(move |window, cx| {
            ShellView::new(
                state,
                None,
                None,
                Clock::Fixed { now: 0, utc: true },
                window,
                cx,
            )
        });
        let at = |channel: u8| Route::Channel {
            community: CommunityId([1; 16]),
            channel: Some(ChannelId([channel; 16])),
        };
        shell.update(vcx, |s, cx| {
            assert_eq!(s.route(), &at(2), "a server opens on its first channel");
            s.step_channel(1, cx);
            assert_eq!(s.route(), &at(3));
            s.step_channel(1, cx);
            assert_eq!(s.route(), &at(3), "the list ends at its last channel");
            assert_eq!(s.back(cx), Some(at(2)));
            assert_eq!(s.channel, Some(ChannelId([2; 16])));
            assert_eq!(s.forward(cx), Some(at(3)));
            assert_eq!(s.channel, Some(ChannelId([3; 16])));
            s.navigate(Route::Settings(None), cx);
            assert_eq!(s.route(), &Route::Settings(None));
            assert_eq!(s.back(cx), Some(at(3)));
        });
    }

    /// Alt+Up and Alt+Down step the listed channels from anywhere in the shell, Alt+Left and Alt+Right the route history,
    /// as keystrokes through the registered bindings.
    #[gpui::test]
    fn alt_arrows_step_channels_and_history(cx: &mut gpui::TestAppContext) {
        cx.update(crate::install);
        let state = world();
        let (shell, vcx) = cx.add_window_view(move |window, cx| {
            ShellView::new(
                state,
                None,
                None,
                Clock::Fixed { now: 0, utc: true },
                window,
                cx,
            )
        });
        vcx.focus(&shell);
        let channel = |shell: &Entity<ShellView>, vcx: &mut gpui::VisualTestContext| {
            shell.read_with(vcx, |s, _| s.channel)
        };
        assert_eq!(channel(&shell, vcx), Some(ChannelId([2; 16])));
        vcx.simulate_keystrokes("alt-down");
        assert_eq!(channel(&shell, vcx), Some(ChannelId([3; 16])));
        vcx.simulate_keystrokes("alt-down");
        assert_eq!(
            channel(&shell, vcx),
            Some(ChannelId([3; 16])),
            "the last channel stops it"
        );
        vcx.simulate_keystrokes("alt-up");
        assert_eq!(channel(&shell, vcx), Some(ChannelId([2; 16])));
        // History: the two selections are entries; back goes to the one before, forward again.
        vcx.simulate_keystrokes("alt-left");
        assert_eq!(channel(&shell, vcx), Some(ChannelId([3; 16])));
        vcx.simulate_keystrokes("alt-left");
        assert_eq!(channel(&shell, vcx), Some(ChannelId([2; 16])));
        vcx.simulate_keystrokes("alt-right");
        assert_eq!(channel(&shell, vcx), Some(ChannelId([3; 16])));
    }

    /// A click on a title bar button reaches the button's handler: the bar's drag handler does not take the press
    /// (the web leaves `button, input, a` out of its drag region).
    #[cfg(not(target_os = "macos"))]
    #[gpui::test]
    fn a_click_on_a_title_bar_button_reaches_its_handler(cx: &mut gpui::TestAppContext) {
        cx.update(crate::install);
        let state = world();
        let (shell, vcx) = cx.add_window_view(move |window, cx| {
            ShellView::new(
                state,
                None,
                None,
                Clock::Fixed { now: 0, utc: true },
                window,
                cx,
            )
            .with_controls(true)
        });
        let width = vcx.update(|window, _| f32::from(window.viewport_size().width));
        // The settings button is the fourth from the right: settings, minimise, maximise, close (28 wide, 8 apart, 12
        // from the edge); the bar is 44 high.
        let x = width - 12.0 - 3.0 * 36.0 - 14.0;
        vcx.simulate_click(
            gpui::point(Pixels::from(x), Pixels::from(22.0f32)),
            gpui::Modifiers::none(),
        );
        shell.read_with(vcx, |s, _| assert_eq!(s.route(), &Route::Settings(None)));
    }
}
