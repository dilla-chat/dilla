//! The desktop shell (packages/ui AppShell, Signal §5.1): the top bar over the server rail, the sidebar (the channel
//! list above the user panel) and the main column (the channel header, the message log, the composer). Geometry is the
//! web client's at 1280x800, measured from the beta.5 reference (`clients/desktop/parity/references/linux`).

use std::rc::Rc;
use std::sync::Arc;

use chrono::{Datelike, FixedOffset, TimeZone};
use dilla_client::Client;
use dilla_client::state::{AppState, Channel, ChannelId, CommunityId, ConnectionStatus};
use gpui::{
    AppContext as _, Context, Entity, FocusHandle, Focusable, InteractiveElement, IntoElement,
    ParentElement, Render, ScrollHandle, SharedString, Styled, Subscription, Window, div, px,
};

use dilla_desktop_ui::tokens::{color, font, layout, radius, text};
use dilla_desktop_ui::{Icon, IconName};

use crate::screens::composer::{Composer, Sent};
use crate::screens::{FocusNext, FocusPrevious, avatar, centred, channel_list, timeline};
use crate::text::{Edge, line};

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
            _subscriptions: vec![sent],
        };
        this.settle_selection(cx);
        this.log.scroll_to_bottom();
        this
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
                self.select(first, cx);
            }
        }
        let target: SharedString = self
            .channel
            .and_then(|c| self.state.channel(c))
            .map_or_else(String::new, |c| format!("#{}", c.name))
            .into();
        self.composer
            .update(cx, |composer, cx| composer.set_target(target, cx));
    }

    /// Shows `channel`: its group is opened (joined) so its timeline is kept, and it is marked read while shown.
    pub fn select(&mut self, channel: ChannelId, cx: &mut Context<Self>) {
        if self.channel == Some(channel) {
            return;
        }
        self.channel = Some(channel);
        self.pinned = true;
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

fn topbar() -> impl IntoElement {
    let brand = div()
        .flex_1()
        .min_w_0()
        .flex()
        .items_center()
        .gap(px(8.))
        .child(
            div()
                .size(px(18.))
                .rounded(radius::SM)
                .bg(color::BRAND)
                .flex()
                .items_center()
                .justify_center()
                .child(
                    line("D", text::XS, 11.0, color::BRAND_INK)
                        .weight(font::WEIGHT_DISPLAY)
                        .at(Edge::Start(19.7), Edge::Start(16.0)),
                ),
        )
        .child(
            line("DILLA", text::XS, 16.5, color::FG)
                .weight(font::WEIGHT_DISPLAY)
                .spacing(1.32)
                .at(Edge::Start(40.0), Edge::Start(13.25)),
        )
        .child(div().w(px(6.)).h(px(12.)).bg(color::BRAND));
    let search = div().w(px(420.)).flex_none().flex().justify_center().child(
        div()
            .w(px(360.))
            // The web box sits 8px down the 43px bar (its pixels, not the 7.5 its layout reports).
            .relative()
            .top(px(0.5))
            .h(px(28.))
            .bg(color::SURFACE_2)
            .border_1()
            .border_color(color::HAIRLINE_2)
            .rounded(radius::MD)
            .flex()
            .items_center()
            .pl(px(10.))
            .pr(px(8.))
            .gap(px(8.))
            .child(Icon::new(IconName::Search).size(px(13.)).color(color::FG_3))
            .child(
                div().flex_1().child(
                    line("Search or jump to…", text::UI, 18.0, color::FG_3)
                        .at(Edge::Mid(-147.0), Edge::Start(13.0)),
                ),
            )
            .child(
                div()
                    .w(px(48.))
                    .h(px(18.))
                    .border_1()
                    .border_color(color::CONTROL_EDGE)
                    .rounded(radius::SM)
                    .pl(px(5.))
                    .child(
                        line("Ctrl K", text::MICRO, 16.0, color::FG_2)
                            .at(Edge::Mid(130.0), Edge::Start(14.0)),
                    ),
            ),
    );
    let end = div()
        .flex_1()
        .min_w_0()
        .flex()
        .justify_end()
        .gap(px(8.))
        .child(centred(
            px(28.),
            Icon::new(IconName::Bell).size(px(15.)).color(color::FG_2),
        ))
        .child(centred(
            px(28.),
            Icon::new(IconName::Settings)
                .size(px(15.))
                .color(color::FG_2),
        ));
    div()
        .flex_none()
        .h(layout::TOPBAR)
        .bg(color::BG_2)
        .border_b_1()
        .border_color(color::HAIRLINE)
        .flex()
        .items_center()
        .pl(px(14.))
        .pr(px(12.))
        .gap(px(12.))
        .child(brand)
        .child(search)
        .child(end)
}

fn rail(state: &AppState, selected: Option<CommunityId>) -> impl IntoElement {
    let tiles = state.communities.iter().enumerate().map(|(index, c)| {
        let initials = dilla_desktop_ui::components::avatar::initials_of(&c.name);
        let tile = div()
            .size(layout::RAIL_TILE)
            .rounded(radius::XL)
            .flex()
            .items_center()
            .justify_center();
        if Some(c.id) == selected {
            // The selected tile's ring: 2px of the rail, then 1.5px of the accent (box-shadow 0 0 0 2px, 0 0 0 3.5px).
            div()
                .size(px(45.))
                .flex_none()
                .rounded(px(13.5))
                .bg(color::ACCENT)
                .p(px(1.5))
                .child(
                    div()
                        .size(px(42.))
                        .rounded(px(12.))
                        .bg(color::BG_2)
                        .p(px(2.))
                        .child(
                            tile.bg(color::ACCENT).child(
                                line(initials, text::XS, 11.0, color::ACCENT_INK)
                                    .weight(font::WEIGHT_DISPLAY)
                                    .at(
                                        Edge::Start(21.4),
                                        Edge::Start(122.5 + 45.0 * index as f32),
                                    ),
                            ),
                        ),
                )
                .into_any_element()
        } else {
            div()
                .p(px(3.5))
                .child(tile.bg(color::SURFACE_HI).child(
                    line(initials, text::XS, 11.0, color::FG_2).weight(font::WEIGHT_DISPLAY),
                ))
                .into_any_element()
        }
    });
    div()
        .flex_none()
        .w(layout::RAIL)
        .h_full()
        .relative()
        .bg(color::BG_2)
        // The rail's edge is an inset shadow on the web (no layout width): a 1px line over its last column.
        .child(
            div()
                .absolute()
                .right_0()
                .top_0()
                .w(px(1.))
                .h_full()
                .bg(color::HAIRLINE),
        )
        .flex()
        .flex_col()
        .items_center()
        .pt(px(10.))
        .child(
            div()
                .size(layout::RAIL_TILE)
                .rounded_full()
                .bg(color::HOVER)
                .flex()
                .items_center()
                .justify_center()
                .child(Icon::new(IconName::AtSign).size(px(17.)).color(color::FG_2)),
        )
        .child(
            div()
                .mt(px(8.))
                .mb(px(8.))
                .w(px(20.))
                .h(px(1.))
                .bg(color::HAIRLINE_2),
        )
        // The ring overhangs the 38px tile by 3.5px each side; the column keeps the tile's 6px gaps.
        .child(
            div()
                .mt(px(-3.5))
                .mb(px(-3.5))
                .flex()
                .flex_col()
                .children(tiles),
        )
        .child(
            div()
                .mt(px(6.))
                .size(layout::RAIL_TILE)
                .rounded_full()
                .border_1()
                .border_dashed()
                .border_color(color::CONTROL_EDGE_KEY)
                .flex()
                .items_center()
                .justify_center()
                .child(Icon::new(IconName::Plus).size(px(15.)).color(color::FG_2)),
        )
}

fn user_panel(name: &str, status: &str) -> impl IntoElement {
    let who = div()
        .flex_1()
        .min_w_0()
        .h(px(36.))
        .flex()
        .items_center()
        .pl(px(6.))
        .gap(px(9.))
        .child(
            div()
                .relative()
                .child(avatar(name, px(30.), text::MICRO, 13.0))
                .child(
                    div()
                        .absolute()
                        .left(px(20.))
                        .top(px(20.))
                        .size(px(13.))
                        .rounded_full()
                        .bg(color::BG_2)
                        .flex()
                        .items_center()
                        .justify_center()
                        .child(div().size(px(9.)).rounded_full().bg(if status == "online" {
                            color::OK
                        } else {
                            color::FG_3
                        })),
                ),
        )
        .child(
            div()
                .flex()
                .flex_col()
                .child(
                    line(name.to_owned(), text::UI, 15.6, color::FG)
                        .weight(font::WEIGHT_LABEL)
                        .at(Edge::Start(109.0), Edge::End(46.115)),
                )
                .child(
                    line(status.to_owned(), text::META, 13.65, color::FG_3)
                        .at(Edge::Start(109.0), Edge::End(30.53)),
                ),
        );
    div()
        .flex_none()
        .h(layout::USER_PANEL)
        .bg(color::BG_2)
        .border_t_1()
        .border_color(color::HAIRLINE)
        .flex()
        .items_center()
        .px(px(8.))
        .gap(px(4.))
        .child(who)
        .child(centred(
            px(30.),
            Icon::new(IconName::Mic).size(px(16.)).color(color::FG_2),
        ))
        .child(centred(
            px(30.),
            Icon::new(IconName::Headphones)
                .size(px(16.))
                .color(color::FG_2),
        ))
}

fn header(channel: Option<&Channel>, members: usize) -> impl IntoElement {
    let name = channel.map_or_else(String::new, |c| c.name.clone());
    let button = |icon: IconName, label: String, from_right: f32| {
        div()
            .h(px(28.))
            .px(px(8.))
            .rounded(radius::MD)
            .flex()
            .items_center()
            .gap(px(6.))
            .child(Icon::new(icon).size(px(14.)).color(color::FG_2))
            .child(
                line(label, text::XS, 16.5, color::FG_2)
                    .at(Edge::End(from_right), Edge::Start(57.25)),
            )
    };
    div()
        .flex_none()
        .h(layout::CHANNEL_HEADER_H)
        .bg(color::BG)
        .border_b_1()
        .border_color(color::HAIRLINE)
        .flex()
        .items_center()
        .pl(px(20.))
        .pr(px(10.))
        .child(
            div()
                .flex_1()
                .min_w_0()
                .flex()
                .items_center()
                .gap(px(6.))
                .child(Icon::new(IconName::Hash).size(px(14.)).color(color::FG_3))
                .child(
                    line(name, text::BASE, 20.25, color::FG)
                        .weight(font::WEIGHT_DISPLAY)
                        .at(Edge::Start(340.0), Edge::Start(55.375)),
                ),
        )
        .child(
            div()
                .flex()
                .items_center()
                .gap(px(2.))
                .child(button(IconName::MessagesSquare, "Threads".into(), 108.81))
                .child(button(IconName::Users, members.to_string(), 24.61)),
        )
}

impl Render for ShellView {
    fn render(&mut self, _window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let state = self.state.clone();
        let channel = self.channel.and_then(|c| state.channel(c));
        let members = self
            .community
            .and_then(|c| state.members.get(&c))
            .map_or(0, Vec::len);
        let online = usize::from(state.connection.status == ConnectionStatus::Online);
        let (me, status) = self.me();
        let entity = cx.entity().downgrade();
        let on_select: channel_list::OnSelect = Rc::new(move |id, _window, cx| {
            let _ = entity.update(cx, |shell, cx| shell.select(id, cx));
        });
        let list = channel_list::render(
            &channel_list::ListModel {
                state: &state,
                community: self.community,
                selected: self.channel,
                online,
            },
            on_select,
        );
        let sidebar = div()
            .flex_none()
            .w(layout::SIDEBAR)
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
            .child(user_panel(&me, status));
        let log = match channel {
            Some(c) => {
                let shell = cx.entity().downgrade();
                let scroll = self.log.clone();
                let on_wheel: timeline::OnWheel = Rc::new(move |up, cx| {
                    // Up leaves the end; reaching the end again follows it.
                    let at_end = scroll.offset().y <= -scroll.max_offset().y + px(1.);
                    let _ = shell.update(cx, |shell, cx| {
                        shell.pinned = !up && at_end;
                        cx.notify();
                    });
                });
                timeline::render(&state, c, self.clock, &self.log, self.pinned, on_wheel)
                    .into_any_element()
            }
            None => div().size_full().bg(color::BG).into_any_element(),
        };
        let main = div()
            .flex_1()
            .min_w_0()
            .h_full()
            .flex()
            .flex_col()
            .child(header(channel, members))
            .child(div().flex_1().min_h_0().child(log))
            .child(div().flex_none().h(px(88.)).child(self.composer.clone()));
        div()
            .id("shell")
            .track_focus(&self.focus)
            .on_action(|_: &FocusNext, window, cx| window.focus_next(cx))
            .on_action(|_: &FocusPrevious, window, cx| window.focus_prev(cx))
            .size_full()
            .bg(color::BG)
            .font_family(font::MONO)
            .flex()
            .flex_col()
            .child(topbar())
            .child(
                div()
                    .flex_1()
                    .min_h_0()
                    .flex()
                    .child(rail(&state, self.community))
                    .child(sidebar)
                    .child(main),
            )
    }
}
