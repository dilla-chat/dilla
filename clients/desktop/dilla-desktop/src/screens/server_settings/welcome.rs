//! The Welcome settings page (packages/web/src/screens/admin/WelcomeSettingsPanel.tsx): the channel the Bot greets new
//! members in and the greeting's template, read from and saved to the Bot as sealed operations 1 and 2.
//!
//! [`WelcomeDraft`] is the page's model (every rule of the panel, with its tests); [`Body::welcome_panel`] draws it and
//! [`Body::flush_welcome`] sends its save. The desktop is the web's `managed` mode: there is no Save button in the panel, the
//! settings footer saves (through the workspace's [`crate::edit_state::Section`], see `view.rs`).

use dilla_client::features::bot_admin::{WelcomeAnswer, WelcomeOp};
use dilla_client::state::server_admin::{WelcomeSettings, access_in};
use dilla_client::state::{AppState, ChannelId, CommunityId};
use gpui::{
    AnyElement, Context, InteractiveElement as _, IntoElement, ParentElement, Styled, Window, div,
    prelude::FluentBuilder as _,
};
use gpui_component::input::Textarea;

use dilla_desktop_ui::components::button::Button;
use dilla_desktop_ui::components::select::Select;
use dilla_desktop_ui::components::server_settings::layout;
use dilla_desktop_ui::components::settings::row::{Layout, SettingsRow};
use dilla_desktop_ui::metrics::OPACITY_MUTED;
use dilla_desktop_ui::text::{line, root_line_height};
use dilla_desktop_ui::tokens::{color, font, radius, text};

use super::body::Body;
use super::body::draw::{act_click, act_value, alert, parse_id};
use crate::strings::t;

/// The template a server that never configured one starts from (`'Welcome {member} to {server}!'`, a literal in the panel).
pub const DEFAULT_TEMPLATE: &str = "Welcome {member} to {server}!";
/// The longest template the Bot takes, in UTF-8 bytes.
pub const TEMPLATE_MAX_BYTES: usize = 2000;

const REQUIRES_CAPABILITY: &str = "admin.bot.welcomeSettings.requiresCapability";
const INVALID: &str = "admin.bot.welcomeSettings.invalid";

/// What the page depends on besides its own draft (BotSection.tsx).
pub struct WelcomeInputs<'a> {
    /// The installation is ready (`state == 3`) and holds the welcome capability (bit 1).
    pub enabled: bool,
    /// The permissions are read and this person may manage the server.
    pub can_manage: bool,
    /// The Bot channels the Bot may post in: the text channels the catalog marks, in channel-list order.
    pub channels: &'a [(ChannelId, String)],
    pub server_name: &'a str,
}

/// The page's state (`useBotDraftState` and the request state of the panel).
#[derive(Clone, Debug, Default)]
pub struct WelcomeDraft {
    /// The configuration the Bot last answered (`None`: it has none).
    pub config: Option<WelcomeSettings>,
    /// The revision a save must quote, from the last read or save.
    pub revision: Option<u64>,
    /// The select's value (`None`: "Choose a channel").
    pub channel: Option<ChannelId>,
    pub template: String,
    /// A read landed: the form shows instead of the Load button.
    pub loaded: bool,
    /// A conflict says the settings changed: a read must come before any save.
    pub must_reload: bool,
    /// The operation (1 read, 2 save) of the request in flight.
    pub pending: Option<u64>,
    /// The request id and operation of an unanswered request, which Retry sends again.
    pub uncertain: Option<([u8; 16], u64)>,
    /// What the save in flight, or the last uncertain one, sent: the settings a `Saved` answer lands.
    pub saving: Option<WelcomeSettings>,
    pub error: Option<&'static str>,
    pub notice: Option<&'static str>,
    /// Set by the workspace's save; [`Body::flush_welcome`] sends it.
    pub save_requested: bool,
    /// The newest request's ticket: an answer to an older one is late and changes nothing.
    pub ticket: u64,
}

impl WelcomeDraft {
    /// `unavailable`: no ready Bot with the capability, no right to manage, or no Bot channel to post in.
    pub fn unavailable(i: &WelcomeInputs) -> bool {
        !i.enabled || !i.can_manage || i.channels.is_empty()
    }

    /// `configuredDraft`: the draft is what the Bot has, under the server's current name.
    pub fn configured_draft(&self, server_name: &str) -> bool {
        self.config.as_ref().is_some_and(|c| {
            Some(c.channel) == self.channel
                && c.template == self.template
                && c.server_name == server_name
        })
    }

    /// `dirty`: loaded, and the Bot has no configuration or the draft is not it.
    pub fn dirty(&self, server_name: &str) -> bool {
        self.loaded && (self.config.is_none() || !self.configured_draft(server_name))
    }

    /// Whether the draft is a valid save (`submitSave`'s guard): read, not stale, a channel, a template.
    fn valid(&self) -> bool {
        !self.must_reload
            && self.revision.is_some()
            && self.channel.is_some()
            && !self.template.trim().is_empty()
            && self.template.len() <= TEMPLATE_MAX_BYTES
    }

    /// `canSave`.
    pub fn can_save(&self, i: &WelcomeInputs) -> bool {
        self.dirty(i.server_name) && !Self::unavailable(i) && self.valid() && self.pending.is_none()
    }

    /// The save to send, with the revision it quotes: `None`, with the `invalid` refusal, when the draft is not a valid one.
    pub fn to_save(&mut self, i: &WelcomeInputs) -> Option<(WelcomeSettings, u64)> {
        let (Some(channel), Some(revision), true) = (self.channel, self.revision, self.valid())
        else {
            self.error = Some(INVALID);
            return None;
        };
        let value = WelcomeSettings {
            channel,
            template: self.template.clone(),
            server_name: i.server_name.to_owned(),
        };
        self.saving = Some(value.clone());
        Some((value, revision))
    }

    /// A request starts: it is the one in flight and the newest, so the answers of older ones are late. The refusal and the
    /// notice of the last one go; an unanswered request stays, for Retry.
    pub fn begin(&mut self, op: u64) -> u64 {
        self.pending = Some(op);
        self.ticket += 1;
        self.error = None;
        self.notice = None;
        self.ticket
    }

    /// Whether the draft differs from what a read would give it (`dirtyRef`): a read keeps such a draft.
    fn edited(&self) -> bool {
        self.loaded
            && self
                .config
                .as_ref()
                .is_none_or(|c| Some(c.channel) != self.channel || c.template != self.template)
    }

    /// The channel and template a reset or a read of `config` gives the draft.
    fn fill(&mut self, fallback: Option<ChannelId>) {
        self.channel = self.config.as_ref().map(|c| c.channel).or(fallback);
        self.template = self
            .config
            .as_ref()
            .map_or_else(|| DEFAULT_TEMPLATE.to_owned(), |c| c.template.clone());
    }

    /// The answer of request `ticket` (operation `op`; `retried`: it was a Retry). `fallback` is the first Bot channel.
    pub fn answer(
        &mut self,
        ticket: u64,
        op: u64,
        result: Result<WelcomeAnswer, String>,
        fallback: Option<ChannelId>,
        retried: bool,
    ) {
        if ticket < self.ticket {
            return;
        }
        self.pending = None;
        // An answer that settles the unanswered request (anything but another timeout) ends its Retry.
        let settles = matches!(&result, Ok(a) if !matches!(a, WelcomeAnswer::Uncertain { .. }));
        if settles && self.uncertain.is_some_and(|(_, was)| was == op) {
            self.uncertain = None;
        }
        match result {
            Ok(WelcomeAnswer::Read { config, revision }) => {
                let keep = self.edited();
                self.config = config;
                if !keep {
                    self.fill(fallback);
                }
                self.revision = Some(revision);
                self.loaded = true;
                self.must_reload = false;
                self.notice = None;
                self.error = None;
            }
            Ok(WelcomeAnswer::Saved { revision }) => match self.saving.take() {
                Some(saved) => {
                    self.config = Some(saved);
                    self.revision = Some(revision);
                    self.loaded = true;
                    self.must_reload = false;
                    self.notice = Some("admin.bot.welcomeSettings.saved");
                    self.error = None;
                }
                None => self.error = Some("admin.bot.welcomeSettings.responseFailed"),
            },
            Ok(WelcomeAnswer::Conflict) => {
                self.error = Some("admin.bot.welcomeSettings.conflict");
                self.must_reload = true;
            }
            Ok(WelcomeAnswer::Failed) => {
                self.error = Some("admin.bot.welcomeSettings.responseFailed");
            }
            Ok(WelcomeAnswer::Uncertain { request_id }) => {
                self.uncertain = Some((request_id, op));
                self.notice = Some("admin.bot.welcomeSettings.uploadUncertain");
                if retried {
                    self.error = Some("admin.bot.welcomeSettings.readFailed");
                }
            }
            Err(_) => self.error = Some("admin.bot.welcomeSettings.uploadFailed"),
        }
    }

    /// `reset`: the saved settings again (the defaults without any, and the Load button back).
    pub fn reset(&mut self, fallback: Option<ChannelId>) {
        self.fill(fallback);
        if self.config.is_none() {
            self.loaded = false;
        }
    }
}

/// BotSection.tsx's inputs for the page, from the state: `(enabled, can_manage, bot channels, server name)`.
pub fn welcome_inputs(
    state: &AppState,
    community: CommunityId,
) -> (bool, bool, Vec<(ChannelId, String)>, String) {
    let enabled = state
        .bot_admin
        .installation
        .get(&community)
        .is_some_and(|i| i.state == 3 && i.capabilities & (1 << 1) != 0);
    let access = access_in(state, community);
    let marked = state
        .bot_admin
        .catalog
        .get(&community)
        .and_then(|c| c.bot_channels.as_deref())
        .unwrap_or_default();
    let channels = state
        .channels
        .iter()
        .filter(|c| c.community_id == community && c.kind == 0 && marked.contains(&c.id))
        .map(|c| (c.id, c.name.clone()))
        .collect();
    let name = state
        .communities
        .iter()
        .find(|c| c.id == community)
        .map_or_else(String::new, |c| c.name.clone());
    (
        enabled,
        access.loaded && access.manage_community,
        channels,
        name,
    )
}

impl Body {
    /// The page's inputs now.
    fn welcome_now(&self) -> (bool, bool, Vec<(ChannelId, String)>, String) {
        welcome_inputs(&self.state, self.community)
    }

    /// Sends one request to the Bot and lands its answer in the draft.
    fn welcome_request(&mut self, op: WelcomeOp, cx: &mut Context<Self>) {
        let operation = match &op {
            WelcomeOp::Read => 1,
            WelcomeOp::Save { .. } => 2,
            WelcomeOp::Retry { operation, .. } => *operation,
        };
        let retried = matches!(op, WelcomeOp::Retry { .. });
        let Some(client) = self.client.clone() else {
            // A fixture has no client and no Bot to ask: its Load is answered as a Bot that never configured Welcome
            // answers (what the shooter's world has), so the page can be drawn loaded.
            if operation == 1 {
                let fallback = self.welcome_now().2.first().map(|c| c.0);
                let mut d = self.welcome.borrow_mut();
                let ticket = d.begin(1);
                let answer = WelcomeAnswer::Read {
                    config: None,
                    revision: 1,
                };
                d.answer(ticket, 1, Ok(answer), fallback, false);
                drop(d);
                cx.notify();
            }
            return;
        };
        let ticket = self.welcome.borrow_mut().begin(operation);
        let reply = client.bot_admin().welcome(self.community, op);
        cx.spawn(async move |this, cx| {
            let result = match reply.await {
                Ok(answer) => answer.map_err(|e| e.to_string()),
                Err(_) => Err("canceled".to_owned()),
            };
            let _ = this.update(cx, |this, cx| {
                let fallback = this.welcome_now().2.first().map(|c| c.0);
                this.welcome
                    .borrow_mut()
                    .answer(ticket, operation, result, fallback, retried);
                cx.notify();
            });
        })
        .detach();
        cx.notify();
    }

    /// Sends the page's save (the footer's Save, through [`WelcomeSection`](super::view)).
    pub(super) fn flush_welcome(&mut self, cx: &mut Context<Self>) {
        let (enabled, can_manage, channels, server_name) = self.welcome_now();
        let inputs = WelcomeInputs {
            enabled,
            can_manage,
            channels: &channels,
            server_name: &server_name,
        };
        let save = {
            let mut d = self.welcome.borrow_mut();
            if !d.save_requested {
                return;
            }
            d.save_requested = false;
            if d.pending.is_some() {
                return;
            }
            d.to_save(&inputs)
        };
        match save {
            Some((value, revision)) => {
                self.welcome_request(WelcomeOp::Save { value, revision }, cx);
            }
            None => cx.notify(),
        }
    }

    /// The Welcome section's content, in the panel's order (WelcomeSettingsPanel.tsx).
    pub(super) fn welcome_panel(
        &mut self,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) -> AnyElement {
        let (enabled, can_manage, channels, server_name) = self.welcome_now();
        let inputs = WelcomeInputs {
            enabled,
            can_manage,
            channels: &channels,
            server_name: &server_name,
        };
        let unavailable = WelcomeDraft::unavailable(&inputs);
        let d = self.welcome.borrow().clone();
        // A read or a reset changed the template: the field takes it (typing keeps the two equal).
        if self.welcome_template.read(cx).value().as_ref() != d.template.as_str() {
            let template = d.template.clone();
            self.welcome_template
                .update(cx, |s, cx| s.set_value(template, window, cx));
        }
        let size = dilla_desktop_ui::text::root_line_height(text::BASE);
        // Each of the panel's `<p>`s is a paragraph a reader reads.
        let para = |key: &str| {
            let words = t(key);
            dilla_desktop_ui::a11y::paragraph(words, line(words, text::BASE, size, color::FG_2))
        };
        let mut panel = div()
            .flex()
            .flex_col()
            .child(
                line(
                    t("admin.bot.welcomeSettings.title"),
                    text::PREVIEW,
                    root_line_height(text::PREVIEW),
                    color::FG,
                )
                .weight(font::WEIGHT_DISPLAY),
            )
            .child(para("admin.bot.welcomeSettings.help"));
        if !enabled {
            panel = panel.child(para(REQUIRES_CAPABILITY));
        }
        if !can_manage {
            panel = panel.child(para("admin.bot.welcomeSettings.readOnly"));
        }
        if channels.is_empty() {
            panel = panel.child(para("admin.bot.welcomeSettings.noChannels"));
        }
        // A saved target that was unmarked since posts nothing until another is chosen.
        if d.loaded
            && d.config
                .as_ref()
                .is_some_and(|c| !channels.iter().any(|(id, _)| *id == c.channel))
        {
            panel = panel.child(para("bot.feature.needsChannel"));
        }
        let busy = unavailable || d.pending.is_some();
        if !d.loaded {
            let label = if d.pending == Some(1) {
                t("admin.bot.welcomeSettings.loading")
            } else {
                t("admin.bot.welcomeSettings.load")
            };
            panel =
                panel.child(
                    div()
                        .flex()
                        .child(Button::new("welcome-load", label).disabled(busy).on_click(
                            act_click(cx, |this, _, cx| {
                                this.welcome_request(WelcomeOp::Read, cx);
                            }),
                        )),
                );
        } else {
            let mut options: Vec<(String, String)> = vec![(
                String::new(),
                t("admin.bot.welcomeSettings.chooseChannel").to_owned(),
            )];
            options.extend(channels.iter().map(|(id, name)| (id.hex(), name.clone())));
            let options: Vec<(&str, &str)> = options
                .iter()
                .map(|(v, l)| (v.as_str(), l.as_str()))
                .collect();
            let draft = self.welcome.clone();
            let select = Select::new(
                "welcome-channel",
                t("admin.bot.welcomeSettings.channel"),
                &options,
                d.channel.map(|c| c.hex()).unwrap_or_default(),
            )
            .bare()
            .disabled(busy)
            .on_change(act_value(cx, move |_, value, _, cx| {
                draft.borrow_mut().channel = parse_id(value).map(ChannelId);
                cx.notify();
            }));
            panel = panel.child(
                SettingsRow::new(
                    "welcome-channel-row",
                    t("admin.bot.welcomeSettings.channel"),
                )
                .child(div().w_full().child(select)),
            );
            let frame = div()
                .id("welcome-template")
                .w_full()
                .max_w(layout::TEXTAREA_MAX_W)
                .min_h(layout::TEXTAREA_MIN_H)
                .px(layout::TEXTAREA_PAD_X)
                .py(layout::TEXTAREA_PAD_Y)
                .bg(color::SURFACE_2)
                .rounded(radius::MD)
                .border_1()
                .border_color(color::CONTROL_EDGE_FIELD)
                .text_size(text::BASE)
                .text_color(color::FG)
                .when(busy, |d| d.opacity(OPACITY_MUTED).cursor_not_allowed())
                .child(
                    Textarea::new(&self.welcome_template)
                        .aria_label(t("admin.bot.welcomeSettings.template"))
                        .appearance(false)
                        .disabled(busy)
                        .text_size(text::BASE)
                        .size_full(),
                );
            panel = panel.child(
                SettingsRow::new(
                    "welcome-template-row",
                    t("admin.bot.welcomeSettings.template"),
                )
                .layout(Layout::Stacked)
                .child(frame)
                .child(para("admin.bot.welcomeSettings.tokens")),
            );
            panel = panel.child(
                div().flex().child(
                    Button::new("welcome-reload", t("admin.bot.welcomeSettings.reload"))
                        .disabled(d.pending.is_some())
                        .on_click(act_click(cx, |this, _, cx| {
                            this.welcome_request(WelcomeOp::Read, cx);
                        })),
                ),
            );
        }
        // The status line: waiting, an unanswered request, or the last notice.
        let status = if d.pending.is_some() {
            Some("admin.bot.welcomeSettings.waiting")
        } else if d.uncertain.is_some() {
            Some("admin.bot.welcomeSettings.uploadUncertain")
        } else {
            d.notice
        };
        if let Some(key) = status {
            panel = panel.child(para(key));
        }
        if let (Some((request_id, operation)), None) = (d.uncertain, d.pending) {
            panel = panel.child(div().flex().child(
                Button::new("welcome-retry", t("admin.bot.welcomeSettings.retry")).on_click(
                    act_click(cx, move |this, _, cx| {
                        this.welcome_request(
                            WelcomeOp::Retry {
                                request_id,
                                operation,
                            },
                            cx,
                        );
                    }),
                ),
            ));
        }
        if let Some(key) = d.error {
            panel = panel.child(alert(t(key)));
        }
        if d.loaded && d.configured_draft(&server_name) {
            panel = panel.child(para("admin.bot.welcomeSettings.configured"));
        }
        panel.into_any_element()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const G: ChannelId = ChannelId([7; 16]);
    const H: ChannelId = ChannelId([8; 16]);

    fn inputs<'a>(channels: &'a [(ChannelId, String)], name: &'a str) -> WelcomeInputs<'a> {
        WelcomeInputs {
            enabled: true,
            can_manage: true,
            channels,
            server_name: name,
        }
    }

    fn config(channel: ChannelId, template: &str, name: &str) -> WelcomeSettings {
        WelcomeSettings {
            channel,
            template: template.to_owned(),
            server_name: name.to_owned(),
        }
    }

    fn read(config: Option<WelcomeSettings>, revision: u64) -> Result<WelcomeAnswer, String> {
        Ok(WelcomeAnswer::Read { config, revision })
    }

    fn answer(d: &mut WelcomeDraft, op: u64, result: Result<WelcomeAnswer, String>, retried: bool) {
        let ticket = d.begin(op);
        d.answer(ticket, op, result, Some(G), retried);
    }

    #[test]
    fn the_page_is_unavailable_without_capability_manage_or_channels() {
        let channels = [(G, "general".to_owned())];
        let mut i = inputs(&channels, "Midgard");
        assert!(!WelcomeDraft::unavailable(&i));
        i.enabled = false;
        assert!(WelcomeDraft::unavailable(&i));
        i.enabled = true;
        i.can_manage = false;
        assert!(WelcomeDraft::unavailable(&i));
        assert!(WelcomeDraft::unavailable(&inputs(&[], "Midgard")));
    }

    #[test]
    fn a_first_read_of_an_unconfigured_server_picks_the_first_bot_channel_and_the_default() {
        let mut d = WelcomeDraft::default();
        answer(&mut d, 1, read(None, 3), false);
        assert!(d.loaded);
        assert_eq!(d.channel, Some(G));
        assert_eq!(d.template, DEFAULT_TEMPLATE);
        assert_eq!(d.revision, Some(3));
        assert!(d.dirty("Midgard Crew"));
    }

    #[test]
    fn a_read_keeps_a_dirty_draft() {
        let mut d = WelcomeDraft::default();
        answer(
            &mut d,
            1,
            read(Some(config(G, "Hi {member}", "Midgard")), 4),
            false,
        );
        assert_eq!(d.template, "Hi {member}");
        d.template = "Edited".to_owned();
        let other = config(H, "Someone else's", "Midgard");
        answer(&mut d, 1, read(Some(other.clone()), 5), false);
        assert_eq!(d.template, "Edited");
        assert_eq!(d.channel, Some(G));
        assert_eq!(d.config, Some(other));
        assert_eq!(d.revision, Some(5));
    }

    #[test]
    fn the_save_is_refused_locally_past_2000_bytes() {
        let channels = [(G, "general".to_owned())];
        let i = inputs(&channels, "Midgard");
        let mut d = WelcomeDraft::default();
        answer(&mut d, 1, read(None, 1), false);
        d.template = "é".repeat(1001);
        assert_eq!(d.template.len(), 2002);
        assert!(d.to_save(&i).is_none());
        assert_eq!(d.error, Some("admin.bot.welcomeSettings.invalid"));
        d.template = "é".repeat(1000);
        let (saved, revision) = d.to_save(&i).expect("2000 bytes is the limit");
        assert_eq!(
            (saved.channel, saved.server_name.as_str(), revision),
            (G, "Midgard", 1)
        );
    }

    #[test]
    fn a_conflict_must_be_reloaded_before_saving() {
        let channels = [(G, "general".to_owned())];
        let i = inputs(&channels, "Midgard");
        let mut d = WelcomeDraft::default();
        answer(&mut d, 1, read(None, 1), false);
        assert!(d.can_save(&i));
        answer(&mut d, 2, Ok(WelcomeAnswer::Conflict), false);
        assert!(d.must_reload);
        assert_eq!(d.error, Some("admin.bot.welcomeSettings.conflict"));
        assert!(!d.can_save(&i));
        answer(&mut d, 1, read(None, 2), false);
        assert!(!d.must_reload && d.can_save(&i));
    }

    #[test]
    fn renaming_the_server_makes_the_saved_draft_dirty() {
        let mut d = WelcomeDraft::default();
        answer(&mut d, 1, read(Some(config(G, "Hi", "Midgard")), 1), false);
        assert!(!d.dirty("Midgard"));
        assert!(d.dirty("Midgard Crew"));
    }

    #[test]
    fn an_uncertain_save_offers_retry_and_a_second_timeout_says_read_failed() {
        let mut d = WelcomeDraft::default();
        answer(&mut d, 1, read(None, 1), false);
        answer(
            &mut d,
            2,
            Ok(WelcomeAnswer::Uncertain {
                request_id: [1; 16],
            }),
            false,
        );
        assert_eq!(d.uncertain, Some(([1; 16], 2)));
        assert_eq!(d.notice, Some("admin.bot.welcomeSettings.uploadUncertain"));
        assert_eq!(d.pending, None);
        assert_eq!(d.error, None);
        answer(
            &mut d,
            2,
            Ok(WelcomeAnswer::Uncertain {
                request_id: [1; 16],
            }),
            true,
        );
        assert_eq!(d.error, Some("admin.bot.welcomeSettings.readFailed"));
    }

    #[test]
    fn reset_without_a_config_unloads() {
        let mut d = WelcomeDraft::default();
        answer(&mut d, 1, read(None, 1), false);
        d.template = "Edited".to_owned();
        d.channel = None;
        d.reset(Some(G));
        assert!(!d.loaded);
        assert_eq!(
            (d.channel, d.template.as_str()),
            (Some(G), DEFAULT_TEMPLATE)
        );
        // With a config it keeps the page loaded and puts the saved values back.
        answer(
            &mut d,
            1,
            read(Some(config(H, "Saved", "Midgard")), 2),
            false,
        );
        d.template = "Edited".to_owned();
        d.reset(Some(G));
        assert!(d.loaded);
        assert_eq!((d.channel, d.template.as_str()), (Some(H), "Saved"));
    }

    #[test]
    fn a_late_save_answer_does_not_overwrite_a_newer_read() {
        let channels = [(G, "general".to_owned())];
        let i = inputs(&channels, "Midgard");
        let mut d = WelcomeDraft::default();
        answer(&mut d, 1, read(None, 7), false);
        d.to_save(&i).expect("a valid draft");
        let t1 = d.begin(2);
        d.answer(
            t1,
            2,
            Ok(WelcomeAnswer::Uncertain {
                request_id: [1; 16],
            }),
            Some(G),
            false,
        );
        // Reload while the save is unanswered.
        let cfg9 = config(H, "Nine", "Midgard");
        let t2 = d.begin(1);
        d.answer(t2, 1, read(Some(cfg9.clone()), 9), Some(G), false);
        assert_eq!(d.revision, Some(9));
        assert_eq!(d.uncertain, Some(([1; 16], 2)), "Retry is still offered");
        // The save's late answer is older than the read.
        d.answer(
            t1,
            2,
            Ok(WelcomeAnswer::Saved { revision: 8 }),
            Some(G),
            false,
        );
        assert_eq!(d.revision, Some(9));
        assert_eq!(d.config, Some(cfg9));
        // A Retry is a request of its own and applies.
        let t3 = d.begin(2);
        d.answer(
            t3,
            2,
            Ok(WelcomeAnswer::Saved { revision: 10 }),
            Some(G),
            true,
        );
        assert_eq!(d.revision, Some(10));
        assert_eq!(d.uncertain, None);
        assert_eq!(
            d.config.as_ref().map(|c| c.server_name.as_str()),
            Some("Midgard")
        );
    }
}
