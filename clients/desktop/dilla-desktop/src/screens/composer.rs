//! The message composer (packages/ui Composer, Signal §5.6), built on Task 4's proof (`experiments/gpui-input-proof`):
//! gpui-component's `Textarea` with `submit_on_enter`, so Enter sends and Shift+Enter starts a new line, as on the web.
//! The owner swallows the `Enter` action the textarea propagates (else GPUI would also type its "\n" into the draft),
//! and Enter during an IME composition confirms it and never sends (the web's `isComposing`).
//!
//! Layout (Composer.css): 16px in from the sides and 14px above the window's edge, a bordered box with a 2px sweep, a row
//! (padding 6 6 4 12, 8px gaps: the `›` prompt, the field, `send ↵`) and a foot (padding 0 6 6, 2px gaps: attach, emoji,
//! code, then the typing text and the `⇧↵ new line` hint). The field grows with its lines from one (30px: 20px lines, 5px
//! above and below) up to the CSS cap of 13.75rem. Above the box: the slow-mode wait or the announcement channel's
//! read-only line (`.d-composer__notice`, 12.5px `--fg-2`); over it, the mention list; below it, the counter near the
//! 4000-byte budget.
//!
//! What the composer keeps: a draft per channel in the instance's prefs (`drafts.<channel>`), written as the text changes
//! and read back when the channel is shown again; the typing signal (an "on" while text is being written, an "off" when it
//! is sent or emptied); the mention list (arrows, Tab and Enter choose, Escape closes); Up in an empty field edits the
//! newest own message.

use std::time::Duration;

use dilla_client::state::{Channel, ChannelId, Member};
use gpui::{
    AppContext as _, Context, Entity, EntityInputHandler, EventEmitter, FocusHandle, Focusable,
    InteractiveElement as _, IntoElement, KeyDownEvent, ParentElement, Render, SharedString,
    StatefulInteractiveElement as _, Styled, Subscription, Task, WeakEntity, Window, div, px,
};
use gpui_component::input::{Enter, InputEvent, MoveDown, MoveUp, Textarea, TextareaState};

use dilla_desktop_ui::components::{Avatar, AvatarSize};
use dilla_desktop_ui::text::{line, line_height, paragraph};
use dilla_desktop_ui::tokens::{color, font, radius, text};
use dilla_desktop_ui::{Icon, IconName};

use crate::screens::messages::composing::{self, Blocked, MAX_LEN};
use crate::screens::messages::css::len;
use crate::screens::messages::mentions::{
    MentionMember, MentionOption, MentionRole, RoleInfo, encode_mentions, mention_options,
    mention_query, mention_roles, offered_roles,
};
use crate::screens::messages::{slow, typing, ui};
use crate::screens::{FocusNext, LABEL_TRACKING_WIDE};
use crate::slots::{self, SlotCx};
use crate::strings::{t, tf};
use crate::tracked_focus;

/// Composer.css `.d-composer__input`: 20px lines, 5px above and below, at most 13.75rem (220px) high.
const FIELD_LINE: f32 = 20.0;
const FIELD_PAD: f32 = 5.0;
const FIELD_MAX: f32 = 220.0;

/// How long a change waits before the draft is written to the prefs.
const DRAFT_DELAY: Duration = Duration::from_millis(400);

/// The rows the field grows to before it scrolls.
fn max_rows() -> usize {
    ((FIELD_MAX - 2.0 * FIELD_PAD) / FIELD_LINE).floor() as usize
}

/// Emitted when the person sends the draft: its text, as it is posted (mentions encoded, a leading `//` folded).
pub struct Sent(pub String);

pub struct Composer {
    pub input: Entity<TextareaState>,
    /// `#general`: the placeholder names the channel.
    target: SharedString,
    /// What the composer's slots (`slots::composer_*`) are drawn with.
    slots: SlotCx,
    /// The channel whose draft the field holds.
    loaded: Option<ChannelId>,
    /// The mention list's chosen line, and the `@` whose list the person closed.
    active: usize,
    dismissed: Option<usize>,
    /// The pending draft write, and the one-second tick while slow mode counts down.
    /// The text `used` was worked out for, and the bytes of it as posted (mentions encoded).
    /// When this composer first drew (unix seconds): only own posts from then on start slow mode's wait.
    opened_s: Option<u64>,
    used_for: SharedString,
    used: usize,
    save: Option<Task<()>>,
    tick: Option<Task<()>>,
    _subscriptions: Vec<Subscription>,
}

impl EventEmitter<Sent> for Composer {}

impl Focusable for Composer {
    fn focus_handle(&self, cx: &gpui::App) -> FocusHandle {
        self.input.read(cx).focus_handle(cx)
    }
}

/// The members a mention can name in the open channel, without the viewer.
fn candidates(slots: &SlotCx) -> Vec<Member> {
    let state = &slots.state;
    let me = state.account.user_id;
    slots
        .channel
        .and_then(|c| state.channel(c))
        .and_then(|c| state.members.get(&c.community_id))
        .map(|m| {
            m.iter()
                .filter(|m| Some(m.user_id) != me)
                .cloned()
                .collect()
        })
        .unwrap_or_default()
}

fn byte_of_char(s: &str, chars: usize) -> usize {
    s.char_indices().nth(chars).map_or(s.len(), |(i, _)| i)
}

fn chars_of_byte(s: &str, byte: usize) -> usize {
    s[..byte.min(s.len())].chars().count()
}

impl Composer {
    pub fn new(window: &mut Window, cx: &mut Context<Self>) -> Self {
        let input = cx.new(|cx| TextareaState::new(window, cx).submit_on_enter(true));
        let sub = cx.subscribe_in(&input, window, Self::on_input_event);
        Self {
            input,
            target: SharedString::default(),
            slots: SlotCx::default(),
            loaded: None,
            active: 0,
            dismissed: None,
            opened_s: None,
            used_for: SharedString::default(),
            used: 0,
            save: None,
            tick: None,
            _subscriptions: vec![sub],
        }
    }

    /// The view the composer's slots draw for (the shell's state and selection).
    pub fn set_slots(&mut self, slots: SlotCx) {
        self.slots = slots;
    }

    /// The channel the draft goes to (`#general`).
    pub fn set_target(&mut self, target: SharedString, cx: &mut Context<Self>) {
        if self.target != target {
            self.target = target;
            cx.notify();
        }
    }

    pub fn value(&self, cx: &gpui::App) -> SharedString {
        self.input.read(cx).value()
    }

    fn channel(&self) -> Option<&Channel> {
        self.slots.channel.and_then(|c| self.slots.state.channel(c))
    }

    /// Writes the field's text as the draft of the channel it was loaded for (removing the key when it is empty).
    fn write_draft(&self, cx: &gpui::App) {
        let (Some(client), Some(channel)) = (&self.slots.client, self.loaded) else {
            return;
        };
        let value = self.input.read(cx).value();
        let prefs = client.prefs();
        let key = composing::draft_key(channel);
        let _ = if value.is_empty() {
            prefs.remove(&key)
        } else {
            prefs.set(&key, &value.to_string())
        };
    }

    /// The channel changed: its draft goes to the prefs, the next one is read back into the field.
    fn switch_channel(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let next = self.slots.channel;
        if self.loaded == next {
            return;
        }
        self.save = None;
        if let (Some(client), Some(old)) = (&self.slots.client, self.loaded) {
            client.messages().typing(old, false);
        }
        self.write_draft(cx);
        self.loaded = next;
        let draft = match (&self.slots.client, next) {
            (Some(client), Some(channel)) => client
                .prefs()
                .get::<String>(&composing::draft_key(channel))
                .unwrap_or_default(),
            _ => String::new(),
        };
        self.dismissed = None;
        self.active = 0;
        self.input
            .update(cx, |s, cx| s.set_value(draft, window, cx));
    }

    fn on_input_event(
        &mut self,
        input: &Entity<TextareaState>,
        event: &InputEvent,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        match event {
            InputEvent::Change => {
                self.active = 0;
                let empty = input.read(cx).value().trim().is_empty();
                if let (Some(client), Some(channel)) = (&self.slots.client, self.loaded) {
                    // Someone writing signals typing (at most every 3 s); an emptied field stops it.
                    client.messages().typing(channel, !empty);
                }
                self.save = Some(cx.spawn(async move |this: WeakEntity<Self>, cx| {
                    cx.background_executor().timer(DRAFT_DELAY).await;
                    let _ = this.update(cx, |this, cx| this.write_draft(cx));
                }));
                cx.notify();
            }
            InputEvent::PressEnter { shift: false, .. } => {
                let composing = input
                    .update(cx, |s, cx| s.marked_text_range(window, cx))
                    .is_some();
                if composing {
                    return;
                }
                if let Some(option) = self.open_options(cx).get(self.active).cloned() {
                    self.insert_option(&option, window, cx);
                    return;
                }
                self.send(window, cx);
            }
            _ => {}
        }
    }

    /// The mention list's lines for the field now: empty when it is closed.
    fn open_options(&self, cx: &gpui::App) -> Vec<MentionOption> {
        if self.blocked().is_some() || !self.input.read(cx).value().contains('@') {
            return Vec::new();
        }
        let state = self.input.read(cx);
        let value = state.value();
        let caret = chars_of_byte(&value, state.cursor());
        let Some((start, query)) = mention_query(&value, caret) else {
            return Vec::new();
        };
        if self.dismissed == Some(start) {
            return Vec::new();
        }
        let roles: Vec<RoleInfo> = Vec::new();
        mention_options(
            &query,
            &candidates(&self.slots),
            false,
            &offered_roles(&roles, false),
        )
    }

    /// Replaces the `@query` before the caret with the option's text (`@name `).
    fn insert_option(
        &mut self,
        option: &MentionOption,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let (value, caret) = {
            let s = self.input.read(cx);
            (s.value().to_string(), chars_of_byte(&s.value(), s.cursor()))
        };
        let Some((start, _)) = mention_query(&value, caret) else {
            return;
        };
        let (a, b) = (byte_of_char(&value, start), byte_of_char(&value, caret));
        let next = format!("{}{}{}", &value[..a], option.insert, &value[b..]);
        let at = chars_of_byte(&next, a + option.insert.len());
        self.dismissed = Some(start);
        self.set_text(&next, Some(at), window, cx);
    }

    /// Sets the field's text and, when asked, the caret (in characters).
    fn set_text(
        &mut self,
        text: &str,
        caret: Option<usize>,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let text = text.to_owned();
        self.input.update(cx, |s, cx| {
            s.set_value(text.clone(), window, cx);
            if let Some(c) = caret {
                let pos = text.chars().take(c).fold((0u32, 0u32), |(line, col), ch| {
                    if ch == '\n' {
                        (line + 1, 0)
                    } else {
                        (line, col + 1)
                    }
                });
                s.set_cursor_position(
                    gpui_component::input::Position {
                        line: pos.0,
                        character: pos.1,
                    },
                    window,
                    cx,
                );
            }
        });
    }

    fn blocked(&self) -> Option<Blocked> {
        composing::may_send(&self.slots.state, self.channel()?).err()
    }

    /// What the draft is posted as: names encoded back to their tokens (the web's `encodeMentions`).
    fn encoded(&self, raw: &str) -> String {
        let members: Vec<MentionMember> = candidates(&self.slots)
            .iter()
            .filter(|m| !m.username.is_empty())
            .map(|m| MentionMember {
                user_id: m.user_id.hex(),
                username: m.username.clone(),
            })
            .collect();
        let roles: Vec<RoleInfo> = Vec::new();
        let usernames: Vec<&str> = members.iter().map(|m| m.username.as_str()).collect();
        let role_mentions: Vec<MentionRole> =
            mention_roles(&offered_roles(&roles, false), &usernames);
        encode_mentions(raw, &members, false, &role_mentions)
    }

    fn send(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let raw = self.input.read(cx).value().to_string();
        if raw.trim().is_empty() || self.blocked().is_some() {
            return;
        }
        let posted = self.encoded(&composing::outgoing(&raw));
        // Over the budget the text stays in the field; the counter already says by how much.
        if posted.len() > MAX_LEN {
            return;
        }
        self.input.update(cx, |s, cx| s.set_value("", window, cx));
        self.dismissed = None;
        if let (Some(client), Some(channel)) = (&self.slots.client, self.loaded) {
            client.messages().typing(channel, false);
        }
        self.save = None;
        self.write_draft(cx);
        cx.emit(Sent(posted));
        cx.notify();
    }

    /// The keys the textarea would take itself, offered to the mention list and the edit shortcut first.
    fn on_key(&mut self, event: &KeyDownEvent, window: &mut Window, cx: &mut Context<Self>) {
        let k = &event.keystroke;
        if k.modifiers.alt || k.modifiers.control || k.modifiers.platform {
            return;
        }
        // Escape is a plain key; the arrows and Tab are bound to actions, which the textarea and the app take before a
        // key listener sees them, so they come through `capture_action` below.
        if k.key == "escape" {
            self.nav("escape", window, cx);
        }
    }

    /// One navigation key offered to the mention list and the edit shortcut first; true when it was used (the caller
    /// stops the key there).
    fn nav(&mut self, key: &str, window: &mut Window, cx: &mut Context<Self>) -> bool {
        let options = self.open_options(cx);
        if !options.is_empty() {
            let n = options.len();
            match key {
                "down" => self.active = (self.active + 1) % n,
                "up" => self.active = (self.active + n - 1) % n,
                "tab" => {
                    let option = options[self.active.min(n - 1)].clone();
                    self.insert_option(&option, window, cx);
                }
                "escape" => {
                    let (value, caret) = {
                        let s = self.input.read(cx);
                        (s.value(), chars_of_byte(&s.value(), s.cursor()))
                    };
                    self.dismissed = mention_query(&value, caret).map(|(start, _)| start);
                }
                _ => return false,
            }
            cx.notify();
            return true;
        }
        // Up in an empty field edits the newest own message.
        if key == "up" && self.input.read(cx).value().is_empty() && self.blocked().is_none() {
            let target = self
                .slots
                .channel
                .and_then(|c| self.slots.state.timelines.get(&c))
                .and_then(|tl| composing::edit_target(&tl.items).cloned());
            if let (Some(item), Some(h)) = (target, ui::handle(cx)) {
                let slots = self.slots.clone();
                h.update(cx, |ui, cx| ui.begin_edit(&slots, &item, window, cx));
                return true;
            }
        }
        false
    }

    /// Re-draws once a second while slow mode counts down.
    fn arm_tick(&mut self, waiting: bool, cx: &mut Context<Self>) {
        if !waiting {
            self.tick = None;
            return;
        }
        if self.tick.is_none() {
            self.tick = Some(cx.spawn(async move |this: WeakEntity<Self>, cx| {
                loop {
                    cx.background_executor().timer(Duration::from_secs(1)).await;
                    if this
                        .update(cx, |this, cx| {
                            cx.notify();
                            this.tick.is_none()
                        })
                        .unwrap_or(true)
                    {
                        break;
                    }
                }
            }));
        }
    }
}

/// A 26x24 tool button of the foot with a 14px icon.
fn tool(icon: IconName) -> impl IntoElement {
    div()
        .w(len(26.))
        .h(len(24.))
        .flex_none()
        .flex()
        .items_center()
        .justify_center()
        .rounded(radius::SM)
        .child(Icon::new(icon).size(len(14.)).color(color::FG_3))
}

/// The mention list over the box (MentionList.css): 340px (less the 32px sides) wide, the members title, 34px lines, the
/// chosen one on `--surface-hi` with a 2px accent bar.
fn mention_list(options: &[MentionOption], active: usize) -> gpui::AnyElement {
    let mut list = div().flex().flex_col().px(len(4.)).pb(len(4.));
    for (n, o) in options.iter().enumerate() {
        let lead = if let Some(user) = o.id.strip_prefix("m-") {
            Avatar::new(
                SharedString::from(format!("mention-{user}")),
                o.primary.clone(),
            )
            .size(AvatarSize::Sug)
            .into_any_element()
        } else {
            div()
                .size(len(22.))
                .flex_none()
                .flex()
                .items_center()
                .justify_center()
                .rounded(radius::ROW)
                .bg(color::HOVER)
                .child(
                    Icon::new(IconName::AtSign)
                        .size(len(14.))
                        .color(color::FG_2),
                )
                .into_any_element()
        };
        let mut row = div()
            .relative()
            .h(len(34.))
            .px(len(8.))
            .flex()
            .items_center()
            .gap(len(10.))
            .rounded(radius::MD)
            .child(lead)
            .child(
                div()
                    .flex()
                    .flex_col()
                    .min_w_0()
                    .child(
                        line(
                            o.primary.clone(),
                            text::UI,
                            line_height(text::UI, 1.3),
                            color::FG,
                        )
                        .ellipsis(),
                    )
                    .child(
                        line(
                            o.secondary.clone(),
                            text::META,
                            line_height(text::META, 1.3),
                            color::FG_3,
                        )
                        .ellipsis(),
                    ),
            );
        if n == active {
            row = row.bg(color::SURFACE_HI).child(
                div()
                    .absolute()
                    .left(len(0.))
                    .top(len(0.))
                    .w(len(2.))
                    .h_full()
                    .bg(color::ACCENT),
            );
        }
        list = list.child(row);
    }
    div()
        .w(len(340.))
        .overflow_hidden()
        .bg(color::SURFACE_2)
        .border_1()
        .border_color(color::LINE_STRONG)
        .rounded(radius::XL)
        .child(
            div().px(len(12.)).pt(len(8.)).pb(len(6.)).child(
                line(
                    t("shell.composer.mentionsTitle").to_uppercase(),
                    text::MICRO,
                    line_height(text::MICRO, 1.5),
                    color::FG_3,
                )
                .weight(font::WEIGHT_LABEL)
                .tracking(LABEL_TRACKING_WIDE),
            ),
        )
        .child(list)
        .into_any_element()
}

impl Render for Composer {
    fn render(&mut self, window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        self.switch_channel(window, cx);
        let value = self.input.read(cx).value();
        let empty = value.is_empty();
        let ctx = self.slots.clone();
        let leading = slots::composer_leading(&ctx, window, cx);
        let buttons = slots::composer_buttons(&ctx, window, cx);
        let popup = slots::composer_popup(&ctx, window, cx);
        let now_ms = ctx.clock.now() * 1000;
        let now_ms = match ctx.clock {
            crate::screens::shell::Clock::Local => std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .map_or(0, |d| u64::try_from(d.as_millis()).unwrap_or(0)),
            crate::screens::shell::Clock::Fixed { .. } => now_ms,
        };
        let blocked = self.blocked();
        // Slow mode (never for a read-only composer: its line says why nothing can be sent).
        let opened_s = *self.opened_s.get_or_insert(now_ms / 1000);
        let slow_wait = self.channel().filter(|_| blocked.is_none()).map(|c| {
            let items = ctx
                .channel
                .and_then(|id| ctx.state.timelines.get(&id))
                .map(|tl| tl.items.as_slice())
                .unwrap_or_default();
            composing::slow_state_since(c, items, false, now_ms, opened_s)
        });
        let waiting = slow_wait.is_some_and(|s| s.until_ms > now_ms);
        self.arm_tick(waiting, cx);
        let typing_text = ctx
            .channel
            .and_then(|c| typing::text(&ctx.state, c, now_ms));
        let options = self.open_options(cx);
        let active = self.active.min(options.len().saturating_sub(1));
        // The field is as high as the draft's lines (20px each), up to the cap; soft-wrapped lines are not counted.
        let rows = value.split('\n').count().clamp(1, max_rows());
        let placeholder = format!("Message {}", self.target);
        let sendable = !empty && blocked.is_none();
        let field = div()
            .relative()
            .flex_1()
            .min_w_0()
            .min_h(px(FIELD_LINE + 2.0 * FIELD_PAD))
            .py(px(FIELD_PAD))
            .text_size(text::INPUT)
            .line_height(px(FIELD_LINE))
            .font_family(font::MONO)
            .text_color(if blocked.is_some() {
                color::FG_3
            } else {
                color::FG
            })
            .child(tracked_focus(
                "composer",
                &self.input.read(cx).focus_handle(cx),
                div().w_full().h(px(rows as f32 * FIELD_LINE)).child(
                    crate::screens::messages::textarea_origin(
                        Textarea::new(&self.input)
                            .appearance(false)
                            .disabled(blocked.is_some())
                            .h(px(rows as f32 * FIELD_LINE)),
                    ),
                ),
            ))
            .children(empty.then(|| {
                div()
                    .absolute()
                    .left_0()
                    .top(px(FIELD_PAD))
                    .right_0()
                    .flex()
                    .child(line(placeholder, text::INPUT, FIELD_LINE, color::FG_FAINT).ellipsis())
            }));
        // An empty draft's `send` is the disabled one (`--surface-hi`, `--fg-4`); a draft lights it up.
        let (send_bg, send_fg) = if sendable {
            (color::ACCENT, color::ACCENT_INK)
        } else {
            (color::SURFACE_HI, color::FG_4)
        };
        let send = div()
            .id("composer-send")
            .flex_none()
            .h(len(28.))
            .px(len(10.))
            .rounded(radius::ROW)
            .bg(send_bg)
            .flex()
            .items_center()
            .child(
                line("send ↵", text::BTN, line_height(text::BTN, 1.5), send_fg)
                    .weight(font::WEIGHT_DISPLAY),
            )
            .on_click(cx.listener(|this, _, window, cx| this.send(window, cx)));
        let row = div()
            .flex()
            .items_center()
            .gap(len(8.))
            .pt(len(6.))
            .pr(len(6.))
            .pb(len(4.))
            .pl(len(12.))
            .child(
                line("›", text::BASE, line_height(text::BASE, 1.5), color::ACCENT)
                    .weight(font::WEIGHT_DISPLAY),
            )
            .child(field)
            .child(send);
        let code = div()
            .id("composer-code")
            .child(tool(IconName::CodeXml))
            .on_click(cx.listener(|this, _, window, cx| {
                if this.blocked().is_some() {
                    return;
                }
                let (text, caret) = composing::insert_code_fence(&this.input.read(cx).value());
                this.set_text(&text, Some(caret), window, cx);
            }));
        let mut foot = div()
            .flex()
            .items_center()
            .gap(len(2.))
            .px(len(6.))
            .pb(len(6.))
            .child(tool(IconName::Paperclip))
            .child(tool(IconName::Smile))
            .child(code)
            // The other areas' buttons (12, 10). Before the flexible spacer, which absorbs the gap an empty slot adds.
            .child(buttons)
            .child(div().flex_1());
        if let Some(who) = &typing_text {
            foot = foot.child(
                line(
                    who.clone(),
                    text::META,
                    line_height(text::META, 1.5),
                    color::ACCENT,
                )
                .ellipsis(),
            );
        }
        foot = foot.child(div().ml(len(10.)).child(line(
            t("shell.composer.newLine"),
            text::META,
            line_height(text::META, 1.5),
            color::FG_3,
        )));
        // The notice above the box: the read-only line, or the wait and its queue.
        let notice: Option<String> = match (blocked, slow_wait) {
            (Some(Blocked::ReadOnly), _) => None,
            (None, Some(s)) if s.until_ms > now_ms => {
                let mut text = tf(
                    "slow.wait",
                    &[(
                        "time",
                        &slow::format_wait(i64::try_from(s.until_ms - now_ms).unwrap_or(0)),
                    )],
                );
                if s.queued {
                    text.push(' ');
                    text.push_str(t("slow.queued"));
                }
                Some(text)
            }
            _ => None,
        };
        let name = self.channel().map(|c| c.name.clone()).unwrap_or_default();
        let reason = blocked.map(|_| tf("announce.readOnly", &[("name", &name)]));
        // The budget used is worked out when the text changes (compared in place, no copy), not on every draw.
        if self.used_for != value {
            self.used = if value.contains('@') {
                self.encoded(&value).len()
            } else {
                value.len()
            };
            self.used_for = value.clone();
        }
        let used = self.used as i64;
        let remaining = MAX_LEN as i64 - used;
        let box_ = div()
            .w_full()
            .bg(color::SURFACE)
            .border_1()
            .border_color(color::HAIRLINE_2)
            .rounded(radius::LG)
            .flex()
            .flex_col()
            .child(div().h(len(2.)))
            // The reply bar and the attachment tray (11, 12): no gap, so an empty slot takes no height.
            .child(leading)
            .child(row)
            .child(foot);
        let width = f32::from(window.viewport_size().width);
        div()
            // The textarea propagates a plain Enter so its owner can send; swallowing it here stops GPUI from then also
            // typing the key's "\n" into the draft.
            .on_action(cx.listener(|_, _: &Enter, _, _| {}))
            .capture_key_down(cx.listener(|this, event: &KeyDownEvent, window, cx| {
                this.on_key(event, window, cx);
            }))
            .capture_action(cx.listener(|this, _: &MoveUp, window, cx| {
                if this.nav("up", window, cx) {
                    cx.stop_propagation();
                }
            }))
            .capture_action(cx.listener(|this, _: &MoveDown, window, cx| {
                if this.nav("down", window, cx) {
                    cx.stop_propagation();
                }
            }))
            .capture_action(cx.listener(|this, _: &FocusNext, window, cx| {
                if this.nav("tab", window, cx) {
                    cx.stop_propagation();
                }
            }))
            .relative()
            .w_full()
            .bg(color::BG)
            .px(len(16.))
            .pb(len(14.))
            .children(notice.map(|n| {
                div().mb(len(4.)).children(paragraph(
                    &n,
                    text::SM,
                    line_height(text::SM, 1.5),
                    color::FG_2,
                    (width - 32.0).max(1.0),
                ))
            }))
            .child(box_)
            .children(reason.map(|r| {
                div().mt(len(4.)).children(paragraph(
                    &r,
                    text::SM,
                    line_height(text::SM, 1.5),
                    color::FG_2,
                    (width - 32.0).max(1.0),
                ))
            }))
            .children(composing::counter_visible(remaining).then(|| {
                let label = if remaining < 0 {
                    tf("shell.composer.over", &[("n", &(-remaining).to_string())])
                } else {
                    tf("shell.composer.remaining", &[("n", &remaining.to_string())])
                };
                div().mt(len(4.)).flex().justify_end().child(line(
                    label,
                    text::XS,
                    line_height(text::XS, 1.5),
                    if remaining < 0 {
                        color::DANGER
                    } else {
                        color::FG_3
                    },
                ))
            }))
            // The mention list and the command picker (20) open over the composer.
            .children((!options.is_empty()).then(|| {
                div()
                    .absolute()
                    .left(len(16.))
                    .bottom_full()
                    .mb(len(6.))
                    .child(mention_list(&options, active))
                    .child(crate::screens::messages::probe("mention-list"))
            }))
            .children(popup.map(|popup| {
                div()
                    .absolute()
                    .left_0()
                    .right_0()
                    .bottom_full()
                    .child(popup)
            }))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn characters_and_bytes_convert_both_ways() {
        let s = "aé🎉b";
        assert_eq!(byte_of_char(s, 0), 0);
        assert_eq!(byte_of_char(s, 2), 3);
        assert_eq!(byte_of_char(s, 3), 7);
        assert_eq!(byte_of_char(s, 99), s.len());
        assert_eq!(chars_of_byte(s, 7), 3);
        assert_eq!(chars_of_byte(s, 99), 4);
    }

    #[test]
    fn the_field_grows_to_the_css_cap() {
        assert_eq!(max_rows(), 10);
    }
}
