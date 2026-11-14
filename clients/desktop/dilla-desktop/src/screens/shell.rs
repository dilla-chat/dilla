//! The desktop shell (packages/ui AppShell, Signal §5.1): the top bar over the server rail, the sidebar (the channel
//! list above the user panel) and the main column (the channel header, the message log, the composer), with the aside a
//! slot guest draws on its right. The panes' widths are the person's (a splitter each; widths and folded categories are
//! kept in prefs, `layout.*`); the shell keeps the route history. Sizes come from the web CSS (AppShell.css, TopBar.css,
//! CommunityRail.css, UserPanel.css, ChannelHeader.css) and Task 6's components.

use std::collections::{HashMap, HashSet};
use std::rc::Rc;
use std::sync::Arc;

use chrono::{Datelike, FixedOffset, TimeZone};
use dilla_client::Client;
use dilla_client::features::notify;
use dilla_client::prefs::{LayoutPrefs, Prefs};
use dilla_client::state::{AppState, Channel, ChannelId, CommunityId, ConnectionStatus};
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

use crate::router::{History, Route, UserSection};
use crate::screens::composer::{Composer, Sent};
use crate::screens::header::{self, AsideKind};
use crate::screens::search::{self, Navigate, SearchEnv};
use crate::screens::{
    FocusNext, FocusPrevious, channel_list, dms, rail, timeline, titlebar, user_panel,
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

/// A hairline's width: the aside's border-left (`.d-app-shell__aside`).
const HAIRLINE_W: f32 = 1.0;

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

/// The shell went to `/settings/<section>` (the title bar's gear, Ctrl+,, a DM banner's Privacy or Devices): the app
/// opens the user settings host over the shell (Task 1b's `AppView::open_route`).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct OpenSettings(pub Route);

impl gpui::EventEmitter<OpenSettings> for ShellView {}

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
    /// The channel list head's server menu (the ⌄) is open.
    server_menu_open: bool,
    /// Something is drawn over the shell (the channel menu, a dialog or another overlay of `slots::overlays`), as of the
    /// last frame: Ctrl+K does not open the search palette over it (the web's guard; Task 23).
    overlay_open: bool,
    /// The Home/DMs view is shown (`/dm`, `/dm/<id>`): the sidebar is the DM list (13) and `channel` is the open DM.
    dm_view: bool,
    /// Ctrl+Shift+Enter held down starts the DM call once (review L m3).
    call_key: keys::RepeatLatch,
    /// This device's microphone and speakers, as the panel's buttons last set them (the voice area owns the call).
    mic_off: bool,
    deaf: bool,
    sidebar_splitter: FocusHandle,
    aside_splitter: FocusHandle,
    /// The shipping window: the title bar draws the window's own controls.
    controls: bool,
    /// The text channel last opened in each server: the AFK page's Come back returns there (Shell.tsx `lastText`).
    last_text: HashMap<CommunityId, ChannelId>,
    /// The window this shell is drawn in: the voice area keeps its state per window (one per instance).
    window_id: gpui::WindowId,
    _subscriptions: Vec<Subscription>,
}

impl Focusable for ShellView {
    fn focus_handle(&self, _cx: &gpui::App) -> FocusHandle {
        self.focus.clone()
    }
}

impl ShellView {
    /// Tests: the composer's field (Task 28's cross-area tests type into it as a person does).
    #[cfg(feature = "test-hooks")]
    pub fn composer_focus(&self, cx: &gpui::App) -> FocusHandle {
        gpui::Focusable::focus_handle(self.composer.read(cx), cx)
    }

    /// Tests: what the composer's field holds.
    #[cfg(feature = "test-hooks")]
    pub fn composer_value(&self, cx: &gpui::App) -> String {
        self.composer.read(cx).value(cx).to_string()
    }

    pub fn new(
        state: Arc<AppState>,
        client: Option<Rc<Client>>,
        channel: Option<ChannelId>,
        clock: Clock,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) -> Self {
        let composer = cx.new(|cx| Composer::new(window, cx));
        let sent = cx.subscribe(&composer, |this: &mut Self, _, Sent(text, files), cx| {
            this.send(text.clone(), files.clone(), cx);
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
            server_menu_open: false,
            overlay_open: false,
            dm_view: false,
            call_key: keys::RepeatLatch::default(),
            mic_off: false,
            deaf: false,
            sidebar_splitter: cx.focus_handle().tab_stop(true),
            aside_splitter: cx.focus_handle().tab_stop(true),
            controls: false,
            last_text: HashMap::new(),
            window_id: window.window_handle().window_id(),
            _subscriptions: vec![sent],
        };
        // The DM area (13) follows the instance from the shell's start (the DM list, the trust slice), whatever the shell
        // first shows: a person who never opens the DMs still has their list (the Home tile's counts, the ring gate).
        if let Some(client) = &this.client {
            client.dms().start();
        }
        this.settle_selection(cx);
        // The voice area's hooks read this state and client (17).
        crate::screens::voice::sync(this.window_id, this.state.clone(), this.client.as_ref(), cx);
        let route = this.current_route();
        this.history = History::new(route);
        this.log.scroll_to_bottom();
        this
    }

    /// The AFK page's Come back (Shell.tsx `onLeaveAfk`, `textChannelHere`): the server's text channel last opened when
    /// this person can send in it, else the first one in list order they can send in; with none the page stays.
    pub fn leave_afk(&mut self, cx: &mut Context<Self>) {
        let Some(community) = self.community else {
            return;
        };
        let state = self.state.clone();
        // Shell.tsx `canSendIn`: a plain text channel whose group is readable; unknown bits (a slice still loading)
        // behave as the composer does, sending is not refused here.
        let can_send = |c: &Channel| {
            c.kind == 0
                && c.mode == 0
                && c.group != dilla_client::state::ChannelGroupState::Unsupported
                && dilla_client::features::roles::viewer_channel_bits(&state, c.id)
                    .is_none_or(|b| b & dilla_client::state::threads::SEND_MESSAGES != 0)
        };
        let ordered = channel_list::ordered(&state, community);
        let last = self.last_text.get(&community).copied();
        let to = ordered
            .iter()
            .find(|c| Some(c.id) == last && can_send(c))
            .or_else(|| ordered.iter().find(|c| can_send(c)))
            .map(|c| c.id);
        if let Some(to) = to {
            self.select(to, cx);
        }
    }

    /// The running client this shell shows (`None` for a fixture).
    pub fn client(&self) -> Option<&Rc<Client>> {
        self.client.as_ref()
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
        crate::screens::voice::sync(self.window_id, state.clone(), self.client.as_ref(), cx);
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
        // The DM view keeps its own selection (a DM is no channel of a server); the composer reads "Message {name}".
        if self.dm_view {
            let target: SharedString = self
                .channel
                .and_then(|c| self.state.dms.get(c))
                .map_or_else(String::new, |d| d.name.clone())
                .into();
            let slots = self.slot_cx();
            self.composer.update(cx, |composer, cx| {
                composer.set_slots(slots);
                composer.set_target(target, cx);
            });
            return;
        }
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

    /// What the slots draw this shell's view with (the settings host opened over it takes it too).
    pub fn slot_cx(&self) -> SlotCx {
        SlotCx {
            state: self.state.clone(),
            client: self.client.clone(),
            community: self.community,
            channel: self.channel,
            clock: self.clock,
            workspace: None,
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

    /// Home (the `@` tile, the DM list) is shown: a DM route, or an account in no server on any route but a channel's
    /// (Shell.tsx `homeSelected`: `noServer && base.name !== 'channel'`; flow 07, the sidebar stands on the DMs tab).
    fn home_shown(&self) -> bool {
        self.dm_view
            || (self.state.communities.is_empty() && !matches!(self.route(), Route::Channel { .. }))
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
        self.dm_view = matches!(route, Route::Dms | Route::Dm(_));
        match route {
            Route::Dm(dm) => self.focus_dm(*dm, cx),
            Route::Dms => {
                self.channel = None;
                self.settle_selection(cx);
            }
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
            // The user settings are the app's settings host over this shell (Task 1b): the app opens it.
            Route::Settings(_) | Route::ServerSettings { .. } => {
                cx.emit(OpenSettings(route.clone()))
            }
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
        if let Some(c) = self.state.channel(channel).filter(|c| c.is_text()) {
            self.last_text.insert(c.community_id, channel);
        }
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

    /// Shows DM `dm` (13): its group is opened so its timeline is kept; the log and composer are the channel's (Task 9).
    fn focus_dm(&mut self, dm: ChannelId, cx: &mut Context<Self>) {
        if self.channel == Some(dm) {
            return;
        }
        self.channel = Some(dm);
        self.pinned = true;
        // The composer's slots and its "Message {name}".
        self.settle_selection(cx);
        if let Some(client) = &self.client {
            let opened = client.dms().open(dm);
            cx.background_spawn(async move {
                let _ = opened.await;
            })
            .detach();
        }
        cx.notify();
    }

    /// Shows `channel` as a new history entry.
    pub fn select(&mut self, channel: ChannelId, cx: &mut Context<Self>) {
        if self.channel == Some(channel) && !self.dm_view {
            return;
        }
        self.dm_view = false;
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
                Some(community) => {
                    if !open(self, community, hit.channel, cx) {
                        return false;
                    }
                    // A reply is no row of the channel: its hit opens its thread (Shell.tsx, threads.spec.ts test 4).
                    if let Some(root) = hit.thread_root {
                        crate::screens::threads::open_thread(
                            hit.channel,
                            root,
                            &self.slot_cx(),
                            cx,
                        );
                    }
                    true
                }
                None => false,
            },
            // A setting's result opens its section (search-settings.spec.ts): the app's settings host over the shell.
            Navigate::Settings(section) => {
                self.navigate(Route::Settings(Some(section)), cx);
                true
            }
            Navigate::ServerSettings { community, section } => {
                if !self.state.communities.iter().any(|c| c.id == community) {
                    return false;
                }
                self.navigate(
                    Route::ServerSettings {
                        community,
                        section: Some(section),
                    },
                    cx,
                );
                true
            }
            _ => false,
        }
    }

    /// The server on show (none on Home).
    pub fn community(&self) -> Option<CommunityId> {
        self.community.filter(|_| !self.dm_view)
    }

    /// Shows server `community` on its first text channel.
    pub fn pick_community(&mut self, community: CommunityId, cx: &mut Context<Self>) {
        if self.community == Some(community) && !self.dm_view {
            return;
        }
        self.dm_view = false;
        self.community = Some(community);
        self.channel = None;
        self.settle_selection(cx);
        self.history.push(self.current_route());
        cx.notify();
    }

    /// Alt+Up and Alt+Down: the previous or next listed channel.
    pub fn step_channel(&mut self, delta: i32, cx: &mut Context<Self>) {
        let Some(community) = self.community.filter(|_| !self.dm_view) else {
            return;
        };
        if let Some(next) = channel_list::step_channel(&self.state, community, self.channel, delta)
        {
            self.select(next, cx);
        }
    }

    /// The log follows its newest message again (Ctrl+End, the present bar's "jump to present").
    pub fn jump_to_present(&mut self, cx: &mut Context<Self>) {
        self.pinned = true;
        cx.notify();
    }

    /// Whether the log follows its newest message (false while the person reads older ones).
    pub fn at_present(&self) -> bool {
        self.pinned
    }

    fn send(
        &mut self,
        text: String,
        files: Vec<dilla_client::features::attachments::Descriptor>,
        cx: &mut Context<Self>,
    ) {
        let (Some(client), Some(channel)) = (&self.client, self.channel) else {
            return;
        };
        // A post with files goes through the attachments area (12), which confirms each file first; an aimed reply goes
        // with it, in the same post (Shell.tsx: one `send` with `replyTo` and `attachments`). A reply without files goes
        // through the threads area (11), once. Otherwise a DM's message goes through the DM area (13), which opens the
        // DM's group; a channel's through the client.
        let reply = crate::screens::threads::take_reply(&self.slot_cx(), channel, cx);
        let queued = if !files.is_empty() {
            client.attachments().send(channel, text, files, reply)
        } else if let Some(to) = reply {
            client.threads().reply(channel, to, text)
        } else if self.dm_view {
            client.dms().send_text(channel, text)
        } else {
            client.send_text(channel, text)
        };
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
    fn toggle_aside(&mut self, kind: AsideKind, window: gpui::WindowId, cx: &mut Context<Self>) {
        match kind {
            // The open thread is the threads area's (11): it closes it, or reopens the last one.
            AsideKind::Threads => crate::screens::threads::toggle_pane(&self.slot_cx(), cx),
            // The member list's open state is the members area's (Task 14: open by default, as the web's).
            AsideKind::Members => crate::screens::members::toggle_pane(window, cx),
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
            // Ruling X: the channel row's menu holds the pins (the header has no pin button).
            MenuItem::new("pins", t("shell.pins.open")),
            MenuItem::new("read", MARK_READ),
            MenuItem::new("mute", t("notify.mute")).check(muted),
            MenuItem::new("link", t("invite.copy")),
        ]
    }

    fn choose(&mut self, channel: ChannelId, item: &str, window: &Window, cx: &mut Context<Self>) {
        match item {
            "pins" => {
                crate::screens::messages::open_pins(channel, self.client.as_ref(), window, cx)
            }
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

    // ---- the channel list head's server menu ----------------------------------------------------------------------

    fn set_server_menu_open(&mut self, open: bool, cx: &mut Context<Self>) {
        self.server_menu_open = open;
        cx.notify();
    }

    /// Mute server and the server's notify mode (Shell.tsx `serverMenu`): the account settings Settings → Notifications
    /// writes (Task 22's `set_community_muted` and `set_community_mode`; `default` deletes the stored mode).
    fn choose_server_item(&mut self, community: CommunityId, item: &str, cx: &mut Context<Self>) {
        let mode_key = notify::key_community_mode(community);
        let mute_key = notify::key_community_mute(community);
        if item == channel_list::SERVER_MUTE {
            let on = self
                .prefs
                .as_ref()
                .and_then(|p| notify::SettingsSource::get(p, &mute_key))
                .is_none_or(|v| v != "1");
            if let Some(client) = &self.client {
                warn(
                    "the muted servers",
                    client.notify().set_community_muted(community, on),
                );
            } else if let Some(prefs) = &self.prefs {
                // A shell without a client (tests): the same key in the same preferences.
                warn(
                    "the muted servers",
                    if on {
                        prefs.set(&mute_key, &"1".to_owned())
                    } else {
                        prefs.remove(&mute_key)
                    },
                );
            }
        } else if let Some(value) = item.strip_prefix(channel_list::SERVER_MODE) {
            let mode = notify::NotifyMode::parse(value);
            if let Some(client) = &self.client {
                warn(
                    "the server's notify mode",
                    client.notify().set_community_mode(community, mode),
                );
            } else if let Some(prefs) = &self.prefs {
                warn(
                    "the server's notify mode",
                    match mode {
                        Some(m) => prefs.set(&mode_key, &m.as_str().to_owned()),
                        None => prefs.remove(&mode_key),
                    },
                );
            }
        }
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

    /// The panel's mute and deafen as drawn: the voice slice's own (17) with a client, else the buttons' last press.
    fn mic_and_deaf(&self) -> (bool, bool) {
        if self.client.is_some() {
            let v = &self.state.voice;
            (v.self_mute || v.self_deaf, v.self_deaf)
        } else {
            (self.mic_off, self.deaf)
        }
    }

    fn set_mic(&mut self, cx: &mut Context<Self>) {
        let (muted, _) = self.mic_and_deaf();
        self.mic_off = !muted;
        if let Some(client) = &self.client {
            // session.ts `setMuted`: unmuting a deafened device undeafens it too.
            client.voice().set_muted(!muted);
        }
        cx.notify();
    }

    fn set_deaf(&mut self, cx: &mut Context<Self>) {
        let (_, deafened) = self.mic_and_deaf();
        self.deaf = !deafened;
        if let Some(client) = &self.client {
            client.voice().set_deafened(!deafened);
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
        // The last drawn frame keeps this view (and its composer) until the window draws again: release lets go of
        // their client at once (review M5).
        if let Some(client) = self.client.clone() {
            crate::release::hold(&client, &cx.entity(), |s: &mut Self, _| s.client = None, cx);
            crate::release::hold(&client, &self.composer, |c, cx| c.let_go(cx), cx);
        }
        let state = self.state.clone();
        // An open DM is drawn with the channel's log, header and composer (Task 9's views on the DM's id, 13): a channel
        // record of no server, named as the DM is.
        let dm_channel = self
            .channel
            .filter(|_| self.dm_view)
            .and_then(|c| state.dms.get(c))
            .map(|d| Channel {
                id: d.id,
                community_id: CommunityId([0; 16]),
                kind: 0,
                mode: 0,
                name: d.name.clone(),
                topic: String::new(),
                parent_id: None,
                position: 0,
                slowmode: 0,
                text_group_id: None,
                group: Default::default(),
            });
        let channel = if self.dm_view {
            dm_channel.as_ref()
        } else {
            self.channel.and_then(|c| state.channel(c))
        };
        let members = self
            .community
            .and_then(|c| state.members.get(&c))
            .map_or(0, Vec::len);
        let online = usize::from(state.connection.status == ConnectionStatus::Online);
        let (me, status) = self.me();
        let ctx = self.slot_cx();
        // The threads area reads this shell's view (its pane, the header's toggle, the reply keys), and the Threads button
        // is pressed while a thread is open (11).
        crate::screens::threads::sync(&ctx, window, cx);
        self.aside = crate::screens::threads::is_open(&ctx, cx).then_some(AsideKind::Threads);
        // What the viewer may do in the open channel, from the roles slice (14), gates reply and thread (11); told only
        // when it changes, so the publish does not loop.
        if let (Some(client), Some(open)) = (&self.client, self.channel.filter(|_| !self.dm_view)) {
            let bits = dilla_client::features::roles::viewer_channel_bits(&state, open);
            if state.threads.channel_bits.get(&open).copied() != bits {
                client.threads().set_channel_bits(open, bits);
            }
        }
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
                home_selected: self.home_shown(),
                muted: &muted,
            },
            &rail::RailHandlers {
                on_pick,
                on_home,
                // The add-a-server dialog is the onboarding area's (Task 25).
                on_add: Rc::new(|_, cx| crate::screens::onboarding::open_add_server(cx)),
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
                on_server_menu_open: {
                    let w6 = shell.clone();
                    Rc::new(move |open, _, cx| {
                        let _ = w6.update(cx, |shell, cx| shell.set_server_menu_open(open, cx));
                    })
                },
                on_server_menu_pick: {
                    let w7 = shell.clone();
                    Rc::new(move |item, _, cx| {
                        if let Some(community) = community {
                            let _ = w7.update(cx, |shell, cx| {
                                shell.choose_server_item(community, item, cx)
                            });
                        }
                    })
                },
                on_server_settings: {
                    let w5 = shell.clone();
                    Rc::new(move |_, cx| {
                        if let Some(community) = community {
                            let _ = w5.update(cx, |shell, cx| {
                                shell.server_menu_open = false;
                                shell.navigate(
                                    Route::ServerSettings {
                                        community,
                                        section: None,
                                    },
                                    cx,
                                )
                            });
                        }
                    })
                },
            }
        };
        // The DM area asks the shell to go somewhere through this (13).
        let nav = shell.clone();
        dms::set_navigator(
            window.window_handle().window_id(),
            Rc::new(move |to, _, cx| {
                let route = match to {
                    dms::Nav::Dm(dm) => Route::Dm(dm),
                    dms::Nav::Privacy => Route::Settings(Some(UserSection::Privacy)),
                    dms::Nav::Devices => Route::Settings(Some(UserSection::Devices)),
                };
                let _ = nav.update(cx, |shell, cx| shell.navigate(route, cx));
            }),
            cx,
        );
        let list = if self.home_shown() {
            // Home: the DM list in place of a server's channels (13).
            dms::home::sidebar(&ctx, self.channel, online, window, cx)
        } else {
            channel_list::render(
                &channel_list::ListModel {
                    state: &state,
                    community: self.community,
                    selected: self.channel,
                    online,
                    // People in the server's voice calls (17).
                    in_voice: self.community.map_or(0, |community| {
                        state
                            .channels
                            .iter()
                            .filter(|c| c.community_id == community && c.kind == 1)
                            .map(|c| crate::screens::voice::count(&state, c.id))
                            .sum()
                    }),
                    can_add: owner && !has_categories,
                    can_add_in_category: owner && has_categories,
                    collapsed: &self.collapsed,
                    settings,
                    server_menu_open: self.server_menu_open,
                },
                &handlers,
                window,
                cx,
            )
            .into_any_element()
        };

        let (w_mic, w_deaf) = (shell.clone(), shell.clone());
        let (muted, deafened) = self.mic_and_deaf();
        let panel = user_panel::render(
            &user_panel::PanelModel {
                name: &me,
                status,
                muted,
                deafened,
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
        // The members aside reads the slot context the shell holds (its hook takes none of its own; Task 14).
        crate::screens::members::sync(window.window_handle().window_id(), &ctx, cx);
        let aside = slots::aside(self.route(), window, cx);
        // `.d-app-shell__aside`: the pane's width is its content's; its 1px border-left is outside it.
        let aside_width = if aside.is_some() && column {
            panes.aside + HAIRLINE_W
        } else {
            0.0
        };
        let log_width = window_width - f32::from(layout::RAIL) - panes.sidebar - aside_width;
        // The reaction bars wrap to the message column (10).
        crate::screens::reactions::set_log_width(window.window_handle().window_id(), log_width, cx);
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
            // An account in no server: "Start a server" and "Join a server" (Shell.tsx `noServerState`, Task 25).
            None if state.communities.is_empty() => {
                crate::screens::create::empty_view(&ctx, window, cx)
            }
            None => div().size_full().bg(color::BG).into_any_element(),
        };
        let banners = slots::banners(&ctx, window, cx);
        // What the search palette (23) draws with, and how it opens a result.
        let host = cx.entity().downgrade();
        search::provide(
            window.window_handle().window_id(),
            SearchEnv {
                ctx: ctx.clone(),
                navigate: Rc::new(move |target, _, cx| {
                    host.update(cx, |shell, cx| shell.open_search_result(target, cx))
                        .unwrap_or(false)
                }),
            },
            cx,
        );
        let mut overlays = slots::overlays(&ctx, window, cx);
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
                .on_select(move |item, window, cx| {
                    let id = menu.channel;
                    let _ = w_sel.update(cx, |shell, cx| shell.choose(id, item, window, cx));
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
        // The ring banner (18) reads this window's view; it is the first thing in the main column (AppShell's `ring`), not
        // an overlay, so it never blocks Ctrl+K.
        crate::screens::ring::provide(
            crate::screens::ring::RingEnv { ctx: ctx.clone() },
            window,
            cx,
        );
        let ring = crate::screens::ring::ring_banner(window, cx);
        // A voice channel's stage takes over the conversation (17): its own header, the tiles and the controls.
        // The AFK page's Come back opens a text channel this person can send in (Shell.tsx `onLeaveAfk`).
        let back = shell.clone();
        crate::screens::voice::set_come_back(
            self.window_id,
            Rc::new(move |_, cx| {
                let _ = back.update(cx, |shell, cx| shell.leave_afk(cx));
            }),
            cx,
        );
        let takeover = channel.and_then(|c| slots::main_takeover(c, window, cx));
        // A DM: its call button in the header (VoiceView.tsx `dmCallStart`, Task 18: none while in that call or while a
        // block holds the join), and the call's notice strip under the header while this device is not in its call.
        let dm_call = channel.filter(|_| self.dm_view).and_then(|dm| {
            let id = dm.id;
            let in_call = crate::screens::voice::joined(&state, id);
            let live = crate::screens::voice::count(&state, id) > 0;
            crate::screens::ring::dm_call_start(&state, id, in_call, live).map(|label| {
                // As the ring's answer does: one call per device, the DM's call joined through the ring gate.
                let on_call: header::OnCall =
                    Rc::new(move |window, cx| start_dm_call(id, window, cx));
                (label, on_call)
            })
        });
        let dm_strip = channel
            .filter(|dm| self.dm_view && !crate::screens::voice::joined(&state, dm.id))
            .and_then(|dm| crate::screens::voice::ring_notice(&state, dm));

        let weak = shell.clone();
        let on_toggle: header::OnToggle = Rc::new(move |kind, window, cx| {
            let wid = window.window_handle().window_id();
            let _ = weak.update(cx, |shell, cx| shell.toggle_aside(kind, wid, cx));
        });
        let main = div()
            .flex_1()
            .min_w_0()
            .h_full()
            .flex()
            .flex_col()
            .children(ring.map(|r| div().flex_none().child(r)));
        // An account in no server with no conversation open: the main column is the empty view alone (Shell.tsx `main =
        // noServerState`: no channel header, no composer).
        let no_server = takeover.is_none() && channel.is_none() && state.communities.is_empty();
        let main = match takeover {
            Some(stage) => main.child(div().flex_1().min_h_0().flex().flex_col().child(stage)),
            None if no_server => main.child(div().flex_1().min_h_0().relative().child(log)),
            None => main
                .child(header::render(
                    &header::HeaderModel {
                        channel,
                        members,
                        // Members is pressed while the members area's pane is open (Task 14's `is_open`).
                        aside: self.aside.or_else(|| {
                            (aside.is_some()
                                && crate::screens::members::is_open(
                                    window.window_handle().window_id(),
                                    cx,
                                ))
                            .then_some(AsideKind::Members)
                        }),
                        call: dm_call,
                    },
                    &on_toggle,
                ))
                .children(dm_strip.map(|s| div().flex_none().child(s)))
                .children(
                    crate::screens::messages::notice(channel.map(|c| c.id), window, cx)
                        .map(|n| div().flex_none().child(n)),
                )
                .child({
                    // MessageLog.tsx `present`: away from the newest message, a bar over the log's foot says so and
                    // offers the jump back.
                    let away = channel.is_some() && !self.pinned;
                    let weak = shell.clone();
                    let bar = away.then(|| {
                        let bar = dilla_desktop_ui::components::shell::present::PresentBar::new(
                            "log-jump",
                            t("layout.log.older"),
                            t("layout.log.jump"),
                        )
                        .on_jump(move |_, _, cx| {
                            let _ = weak.update(cx, |shell, cx| shell.jump_to_present(cx));
                        });
                        let wrapped = div().absolute().left_0().right_0().bottom_0().child(bar);
                        #[cfg(feature = "automation")]
                        let wrapped = wrapped.child(crate::automation::bounds_probe("log-present"));
                        wrapped
                    });
                    div().flex_1().min_h_0().relative().child(log).children(bar)
                })
                .child(div().flex_none().child(self.composer.clone())),
        };
        #[cfg(feature = "automation")]
        let main = main
            .relative()
            .child(crate::automation::bounds_probe("shell-main"));

        // The aside is a column from 64rem of window; narrower it is the slot guest's own overlay.
        let aside_column = aside.map(|a| {
            div()
                .flex_none()
                .w(Pixels::from(panes.aside + HAIRLINE_W))
                .h_full()
                .border_l_1()
                .border_color(color::HAIRLINE)
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
                .dragging(self.drag.is_some_and(|d| d.pane == Pane::Sidebar))
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
                    - HAIRLINE_W
                    - dilla_desktop_ui::components::shell::splitter::HIT / 2.0)
                .range(panes.aside, lo, hi)
                .dragging(self.drag.is_some_and(|d| d.pane == Pane::Aside))
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
        // The bell's reminders popover (20): its badge, and the popover itself while open.
        let wid = window.window_handle().window_id();
        let reminders = crate::screens::commands::reminders::popover(&ctx, window, cx);
        let (reminders_due, reminders_open) =
            crate::screens::commands::reminders::bell_state(wid, &state, cx);
        let bell_focus = crate::screens::commands::reminders::bell_focus(wid, cx);
        let titlebar = titlebar::render(
            &titlebar::TitlebarModel {
                controls: self.controls,
                search_hint: SEARCH_HINT,
                reminders_due,
                reminders_open,
                bell_focus: Some(bell_focus),
            },
            &titlebar::TitlebarHandlers {
                on_settings: Rc::new(move |_, cx| {
                    let _ = weak.update(cx, |shell, cx| shell.navigate(Route::Settings(None), cx));
                }),
                // The search field opens the palette (23).
                on_search: Rc::new(|window, cx| {
                    search::request_open_in(window.window_handle().window_id(), cx)
                }),
                on_reminders: Rc::new(|window, cx| {
                    crate::screens::commands::reminders::toggle(
                        window.window_handle().window_id(),
                        cx,
                    )
                }),
            },
            reminders,
        );

        div()
            .id("shell")
            .key_context(keys::CONTEXT)
            .track_focus(&self.focus)
            // Ctrl+K (23) does not open the palette over the channel menu or another overlay (the web's guard).
            .capture_action(
                cx.listener(|this, _: &search::keys::OpenSearch, window, cx| {
                    let open = this.overlay_open || this.server_menu_open;
                    if palette_refused(open, &window.context_stack()) {
                        cx.stop_propagation();
                    }
                }),
            )
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
            .on_action(cx.listener(|this, _: &keys::OpenSettings, _, cx| {
                this.navigate(Route::Settings(None), cx);
            }))
            // Ctrl+End: only while the log is away from its newest message and no overlay is up; else the key goes on.
            .on_action(cx.listener(|this, _: &keys::JumpToPresent, _, cx| {
                if this.pinned
                    || this.channel.is_none()
                    || this.overlay_open
                    || this.server_menu_open
                {
                    cx.propagate();
                    return;
                }
                this.jump_to_present(cx);
            }))
            // Ctrl+Shift+Enter: the open DM's call, as its header button (none while in it, or while a block holds it).
            .on_action(cx.listener(|this, _: &keys::CallOpenDm, window, cx| {
                // Shell.tsx: a held chord's auto-repeats do nothing.
                if !this.call_key.press(std::time::Instant::now()) {
                    return;
                }
                let dm = this.channel.filter(|_| this.dm_view);
                let startable = dm.is_some_and(|id| {
                    let in_call = crate::screens::voice::joined(&this.state, id);
                    let live = crate::screens::voice::count(&this.state, id) > 0;
                    crate::screens::ring::dm_call_start(&this.state, id, in_call, live).is_some()
                });
                match dm {
                    Some(id) if startable && !this.overlay_open && !this.server_menu_open => {
                        start_dm_call(id, window, cx)
                    }
                    _ => cx.propagate(),
                }
            }))
            // The chord's Enter going up opens the latch for the next press.
            .capture_key_up(cx.listener(|this, e: &gpui::KeyUpEvent, _, _| {
                if e.keystroke.key == "enter" {
                    this.call_key.release();
                }
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
            // The search palette, the dialogs and the channel menu, over everything.
            .children(overlays)
    }
}

/// The web's M-01 guard (Shell.tsx): Ctrl+K never opens the palette under a modal: a modal overlay the shell drew last
/// frame (`overlay_open`: the channel menu, the palette itself, a DM dialog), or focus inside a dialog or the settings
/// frame (`contexts`, the focused path's key contexts, carry the ui crate's overlay context).
fn palette_refused(overlay_open: bool, contexts: &[gpui::KeyContext]) -> bool {
    use dilla_desktop_ui::components::dialog::{MENU_CONTEXT, OVERLAY_CONTEXT};
    // An open action menu (its items carry `MENU_CONTEXT` while focused) is an overlay too (review m2).
    overlay_open
        || contexts
            .iter()
            .any(|c| c.contains(OVERLAY_CONTEXT) || c.contains(MENU_CONTEXT))
}

/// Starts the call of the DM `id` from `window` (its header button, Ctrl+Shift+Enter): one call per device, so a call
/// another instance window is in is left first, and the DM's call is joined through the ring gate.
fn start_dm_call(id: ChannelId, window: &mut Window, cx: &mut gpui::App) {
    let w = crate::screens::voice::wid(window);
    let Some(client) = crate::screens::voice::client_of(w, cx) else {
        return;
    };
    crate::screens::voice::after_leaving_other_calls(
        w,
        client.voice(),
        move |voice| voice.join_dm(id),
        cx,
    );
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Review m2: focus inside an open action menu (the channel list head's ⌄, a toolbar's ⋯) refuses Ctrl+K, as the web's
    /// guard refuses it over any open menu.
    #[test]
    fn the_palette_is_refused_over_an_open_menu() {
        let menu =
            gpui::KeyContext::parse(dilla_desktop_ui::components::dialog::MENU_CONTEXT).unwrap();
        let plain = gpui::KeyContext::parse(keys::CONTEXT).unwrap();
        assert!(palette_refused(false, &[plain.clone(), menu]));
        assert!(!palette_refused(false, &[plain]));
    }

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

    use dilla_client::state::{ChannelGroupState, Community, UserId};
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

    /// Integration 3, step 1: Ctrl+, in the shell asks the app for the user settings host (`OpenSettings`).
    #[gpui::test]
    fn ctrl_comma_asks_for_the_settings_host(cx: &mut gpui::TestAppContext) {
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
        let asked = std::rc::Rc::new(std::cell::RefCell::new(Vec::new()));
        let log = asked.clone();
        vcx.update(|_, cx| {
            cx.subscribe(&shell, move |_, e: &OpenSettings, _| {
                log.borrow_mut().push(e.0.clone())
            })
            .detach();
        });
        let focus = shell.update(vcx, |s, _| s.focus.clone());
        vcx.update(|window, cx| focus.focus(window, cx));
        vcx.run_until_parked();
        vcx.simulate_keystrokes("ctrl-,");
        vcx.run_until_parked();
        assert_eq!(*asked.borrow(), vec![Route::Settings(None)]);
    }

    /// Integration-1 follow-up 6 (the web's M-01 guard): Ctrl+K opens the palette over the shell, but not over the
    /// channel menu (or another modal overlay) nor while focus is in a dialog or the settings frame (the ui crate's
    /// overlay key context); a non-modal surface (the ring banner) never blocks it.
    #[gpui::test]
    fn ctrl_k_is_refused_only_under_a_modal(cx: &mut gpui::TestAppContext) {
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
        let open = |vcx: &mut gpui::VisualTestContext| {
            vcx.update(|window, cx| search::inspect(window, cx).is_some_and(|p| p.open))
        };
        let focus = shell.update(vcx, |s, _| s.focus.clone());
        vcx.update(|window, cx| focus.focus(window, cx));
        vcx.run_until_parked();
        // The channel menu is open: Ctrl+K is refused.
        shell.update(vcx, |s, cx| {
            s.open_menu(ChannelId([3; 16]), Point::default(), cx)
        });
        vcx.run_until_parked();
        vcx.update(|window, cx| focus.focus(window, cx));
        vcx.simulate_keystrokes("ctrl-k");
        vcx.run_until_parked();
        assert!(!open(vcx), "no palette over the channel menu");
        shell.update(vcx, |s, cx| {
            s.menu = None;
            cx.notify();
        });
        vcx.run_until_parked();
        vcx.update(|window, cx| focus.focus(window, cx));
        vcx.simulate_keystrokes("ctrl-k");
        vcx.run_until_parked();
        assert!(open(vcx), "Ctrl+K opens the palette over the bare shell");
        // Focus in a dialog or the settings frame (their panels carry the overlay key context) refuses it.
        let dialog =
            gpui::KeyContext::parse(dilla_desktop_ui::components::dialog::OVERLAY_CONTEXT).unwrap();
        let plain = gpui::KeyContext::parse(keys::CONTEXT).unwrap();
        assert!(palette_refused(false, &[plain.clone(), dialog]));
        assert!(palette_refused(true, std::slice::from_ref(&plain)));
        assert!(!palette_refused(false, &[plain]));
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
        shell.update_in(vcx, |s, window, cx| {
            s.toggle_category(ChannelId([4; 16]), cx);
            s.choose(ChannelId([3; 16]), "mute", window, cx);
            assert!(s.channel_muted(ChannelId([3; 16])));
        });
        let prefs = Prefs::instance(&dir);
        assert_eq!(
            LayoutPrefs::ids(&prefs, LayoutPrefs::COLLAPSED),
            [ChannelId([4; 16]).hex()]
        );
        // The mute is the notify area's account setting (Task 22), not a layout key.
        assert!(prefs.get::<Vec<String>>("layout.mutedChannels").is_none());
        assert!(notify::is_muted(&prefs, ChannelId([3; 16]), None));
        shell.update_in(vcx, |s, window, cx| {
            s.choose(ChannelId([3; 16]), "mute", window, cx);
            s.toggle_category(ChannelId([4; 16]), cx);
        });
        let prefs = Prefs::instance(&dir);
        assert!(!notify::is_muted(&prefs, ChannelId([3; 16]), None));
        assert!(LayoutPrefs::ids(&prefs, LayoutPrefs::COLLAPSED).is_empty());
        let _ = std::fs::remove_dir_all(&dir);
    }

    /// A DM route (13) shows the DM list and the DM: a new state keeps it (a DM is no server channel), and a server route
    /// leaves it.
    #[gpui::test]
    fn a_dm_route_keeps_its_dm_until_a_server_is_picked(cx: &mut gpui::TestAppContext) {
        cx.update(crate::install);
        let dm = ChannelId([9; 16]);
        let mut with_dm = (*world()).clone();
        with_dm.dms.list = Some(vec![dilla_client::state::dms::DmSummary {
            id: dm,
            kind: dilla_client::state::dms::DM_KIND_PAIR,
            members: Vec::new(),
            name: "Mira".into(),
            group: ChannelGroupState::None,
        }]);
        let state = Arc::new(with_dm);
        let (shell, vcx) = cx.add_window_view({
            let state = state.clone();
            move |window, cx| {
                ShellView::new(
                    state,
                    None,
                    None,
                    Clock::Fixed { now: 0, utc: true },
                    window,
                    cx,
                )
            }
        });
        shell.update(vcx, |s, cx| {
            s.navigate(Route::Dm(dm), cx);
            assert!(s.dm_view);
            assert_eq!(s.channel, Some(dm));
            s.set_state(state.clone(), cx);
            assert_eq!(s.channel, Some(dm), "a new state keeps the open DM");
            s.step_channel(1, cx);
            assert_eq!(
                s.channel,
                Some(dm),
                "Alt+Down steps no server list in the DM view"
            );
            s.pick_community(CommunityId([1; 16]), cx);
            assert!(!s.dm_view);
            assert_eq!(
                s.channel,
                Some(ChannelId([2; 16])),
                "the server opens on its first channel"
            );
            assert_eq!(s.back(cx), Some(Route::Dm(dm)));
            assert!(s.dm_view);
            assert_eq!(s.channel, Some(dm));
        });
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
