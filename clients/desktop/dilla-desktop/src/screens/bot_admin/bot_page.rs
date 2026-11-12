//! Server settings, Bot (`packages/web/src/screens/admin/{BotSection,BotHealth,BotWebhooks}.tsx`): the managed Bot's card with
//! its Restart, the capability switches, the voice access row, then (while it is enabled) the health line with the command
//! key, the Bot channels, and the webhooks.
//!
//! A webhook's address carries its secret: it is shown once, in its dialog, from a [`Zeroizing`] string that is dropped
//! when the dialog closes; it is never kept in the slice and never logged. Enabling the Bot and saving its capabilities
//! need a fresh second factor: the step-up dialog is the factor area's (Task 26), which installs a [`StepUp`] the Save
//! button asks; without one the page says so (`admin.bot.stepUpMissing`), as the web does when the factors are unknown.

use std::rc::Rc;

use dilla_client::features::bot_admin::new_request_id;
use dilla_client::features::bot_admin::{BotAdmin, ReAdd};
use dilla_client::state::bot_admin::{BotWebhook, Installation, health_line};
use dilla_client::state::{ChannelId, CommunityId};
use dilla_client::{Client, ClientError};
use dilla_desktop_ui::IconName;
use dilla_desktop_ui::components::confirm_dialog::ConfirmDialog;
use dilla_desktop_ui::components::dialog::{Dialog, InitialFocus};
use dilla_desktop_ui::components::select::Select;
use dilla_desktop_ui::components::settings::frame::{group_heading, section_header};
use dilla_desktop_ui::components::settings::row::{GroupPosition, SettingsRow};
use dilla_desktop_ui::components::text_field::TextField;
use dilla_desktop_ui::components::toggle::Toggle;
use dilla_desktop_ui::components::{Button, ButtonSize, ButtonVariant};
use gpui::{
    AnyElement, App, AppContext, ClickEvent, Context, Entity, Global, InteractiveElement,
    IntoElement, ParentElement, Render, SharedString, Styled, Window, div,
};
use gpui_component::input::InputState;
use zeroize::Zeroizing;

use crate::slots::SlotCx;
use crate::strings::{t, tf};

/// The capability rows, in the Bot page's order (`CAPABILITY_OPTIONS`): the prototype's Music, Polls and Reminders first,
/// then dilla's others. `(name, bit, icon)`.
pub const CAPABILITIES: [(&str, u64, IconName); 10] = [
    ("music", 1 << 0, IconName::Music),
    ("polls", 1 << 8, IconName::Vote),
    ("reminders", 1 << 9, IconName::AlarmClock),
    ("welcome", 1 << 1, IconName::Hand),
    ("roles", 1 << 2, IconName::UserPlus),
    ("announcements", 1 << 3, IconName::Megaphone),
    ("warnings", 1 << 4, IconName::TriangleAlert),
    ("removal", 1 << 5, IconName::MessageSquareX),
    ("kick", 1 << 6, IconName::UserX),
    ("ban", 1 << 7, IconName::Ban),
];

/// Polls and Reminders (bits 8 and 9) start on, as the prototype shows them: a first enable offers both switched on. A
/// disabled installation keeps the mask it was approved with.
pub const POLLS_AND_REMINDERS: u64 = (1 << 8) | (1 << 9);

/// The mask the switches start from.
pub fn starting_capabilities(installation: Option<&Installation>) -> u64 {
    installation.map_or(POLLS_AND_REMINDERS, |i| i.capabilities)
}

/// The installation state's status key (`stateKey`).
pub fn state_key(state: u8) -> &'static str {
    match state {
        0 | 1 => "admin.bot.status.pending",
        2 => "admin.bot.status.joining",
        3 => "admin.bot.status.ready",
        4 => "admin.bot.status.disabled",
        _ => "admin.bot.status.failed",
    }
}

/// The step-up the factor area installs (Task 26): asks the person for a fresh second factor and hands the token, in a
/// zeroizing buffer, to `then`. `then` is not called when the person cancels.
#[derive(Clone)]
pub struct StepUp(pub Rc<StepUpAsk>);

/// What [`StepUp`] runs: the callback that gets the token, then the window and the app.
pub type StepUpAsk =
    dyn Fn(Box<dyn FnOnce(Zeroizing<String>, &mut Window, &mut App)>, &mut Window, &mut App);

impl Global for StepUp {}

/// A request id for a mutation that is retried with the same id (the server binds it to actor, payload and result).
fn request_id() -> [u8; 16] {
    new_request_id()
}

/// 32 lower-case hex digits.
fn hex(bytes: &[u8]) -> String {
    bytes.iter().map(|b| format!("{b:02x}")).collect()
}

/// An error line: `E_FORBIDDEN` and a 403 read as the manager's refusal, a conflict as a stale page, the rest by code.
pub fn error_line(error: &ClientError) -> String {
    let code = match error {
        ClientError::Sync(code) => code.as_str(),
        ClientError::Network(_) => "E_NETWORK",
        _ => "E_UNKNOWN",
    };
    match code {
        "E_FORBIDDEN" => t("admin.bot.error.forbidden").to_owned(),
        "E_CONFLICT" => t("admin.bot.error.conflict").to_owned(),
        other => tf("admin.bot.error.other", &[("code", other)]),
    }
}

/// The webhook drafts and dialogs, and what a call in flight has not answered yet.
pub struct BotPage {
    ctx: SlotCx,
    requested: Option<CommunityId>,
    /// The capability switches (the mask a Save would send); `None` until edited.
    edited: Option<u64>,
    creating: bool,
    name: Entity<InputState>,
    channel: Option<ChannelId>,
    allow_everyone: bool,
    /// The one-time address and the webhook's name: held only while its dialog is open.
    shown: Option<(String, Zeroizing<String>)>,
    copied: bool,
    deleting: Option<BotWebhook>,
    busy: bool,
    error: Option<String>,
    status: Option<String>,
    re_adding: Option<ChannelId>,
    re_add_note: Option<(ChannelId, String)>,
    restarting: bool,
}

impl BotPage {
    pub fn new(ctx: SlotCx, window: &mut Window, cx: &mut Context<Self>) -> Self {
        Self {
            ctx,
            requested: None,
            edited: None,
            creating: false,
            name: cx.new(|cx| InputState::new(window, cx)),
            channel: None,
            allow_everyone: false,
            shown: None,
            copied: false,
            deleting: None,
            busy: false,
            error: None,
            status: None,
            re_adding: None,
            re_add_note: None,
            restarting: false,
        }
    }

    pub fn set_ctx(&mut self, ctx: SlotCx) {
        self.ctx = ctx;
    }

    fn client(&self) -> Option<Rc<Client>> {
        self.ctx.client.clone()
    }

    fn admin(&self) -> Option<BotAdmin> {
        self.client().map(|c| c.bot_admin())
    }

    /// Reads what the page shows, once per community.
    fn ensure_loaded(&mut self) {
        let Some(community) = self.ctx.community else {
            return;
        };
        if self.requested == Some(community) {
            return;
        }
        self.requested = Some(community);
        if let Some(admin) = self.admin() {
            drop(admin.refresh_installation(community));
            drop(admin.refresh_webhooks(community));
            drop(admin.refresh_status(community));
        }
    }

    /// Awaits a client call and hands its answer to `then` on the UI thread.
    fn run<T: Send + 'static>(
        &mut self,
        rx: tokio::sync::oneshot::Receiver<Result<T, ClientError>>,
        window: &mut Window,
        cx: &mut Context<Self>,
        then: impl FnOnce(&mut Self, Result<T, ClientError>, &mut Window, &mut Context<Self>) + 'static,
    ) {
        cx.spawn_in(window, async move |this, cx| {
            let answer = rx.await.unwrap_or(Err(ClientError::Stopped));
            let _ = this.update_in(cx, |this, window, cx| {
                then(this, answer, window, cx);
                cx.notify();
            });
        })
        .detach();
    }

    fn fail(&mut self, error: &ClientError) {
        let code = match error {
            ClientError::Sync(code) => code.clone(),
            ClientError::Network(_) => "E_NETWORK".to_owned(),
            _ => "E_UNKNOWN".to_owned(),
        };
        self.error = Some(tf("bot.webhooks.error", &[("code", &code)]));
    }

    fn create_webhook(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let (Some(admin), Some(community)) = (self.admin(), self.ctx.community) else {
            return;
        };
        let bot_channels = self.bot_channels();
        let channel = self.channel.or_else(|| bot_channels.first().map(|c| c.0));
        let name = self.name.read(cx).value().trim().to_owned();
        let Some(channel) = channel else { return };
        if self.busy || name.is_empty() {
            return;
        }
        self.busy = true;
        self.error = None;
        let rx = admin.create_webhook(community, channel, name.clone(), self.allow_everyone);
        self.run(rx, window, cx, move |this, answer, window, cx| {
            this.busy = false;
            match answer {
                Ok(made) => {
                    this.creating = false;
                    this.allow_everyone = false;
                    this.channel = None;
                    this.name
                        .update(cx, |input, cx| input.set_value("", window, cx));
                    this.copied = false;
                    this.shown = Some((name, made.url));
                }
                Err(e) => this.fail(&e),
            }
        });
    }

    fn rotate(&mut self, hook: &BotWebhook, window: &mut Window, cx: &mut Context<Self>) {
        let (Some(admin), Some(community)) = (self.admin(), self.ctx.community) else {
            return;
        };
        if self.busy {
            return;
        }
        self.busy = true;
        self.error = None;
        let (id, name) = (hook.webhook_id, hook.name.clone());
        let rx = admin.rotate_webhook(community, id);
        self.run(rx, window, cx, move |this, answer, _, _| {
            this.busy = false;
            match answer {
                Ok(made) => {
                    this.copied = false;
                    this.shown = Some((name, made.url));
                }
                Err(e) => this.fail(&e),
            }
        });
    }

    fn remove(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let (Some(admin), Some(community), Some(hook)) =
            (self.admin(), self.ctx.community, self.deleting.clone())
        else {
            return;
        };
        if self.busy {
            return;
        }
        self.busy = true;
        self.error = None;
        let rx = admin.delete_webhook(community, hook.webhook_id);
        self.run(rx, window, cx, |this, answer, _, _| {
            this.busy = false;
            match answer {
                Ok(()) => this.deleting = None,
                Err(e) => this.fail(&e),
            }
        });
    }

    fn re_add(&mut self, channel: ChannelId, window: &mut Window, cx: &mut Context<Self>) {
        let (Some(admin), Some(community)) = (self.admin(), self.ctx.community) else {
            return;
        };
        if self.re_adding.is_some() {
            return;
        }
        self.re_adding = Some(channel);
        self.re_add_note = None;
        let rx = admin.re_add(community, channel);
        self.run(rx, window, cx, move |this, answer, _, _| {
            this.re_adding = None;
            this.re_add_note = match answer {
                Ok(ReAdd::Pending) => Some((channel, t("bot.channels.reAddPending").to_owned())),
                Ok(ReAdd::Added) => {
                    this.status = Some(t("bot.channels.reAdding").to_owned());
                    None
                }
                Err(e) => {
                    let code = match &e {
                        ClientError::Sync(code) => code.clone(),
                        _ => "E_UNKNOWN".to_owned(),
                    };
                    Some((channel, tf("bot.channels.reAddError", &[("code", &code)])))
                }
            };
        });
    }

    fn restart(
        &mut self,
        installation: &Installation,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let (Some(admin), Some(community)) = (self.admin(), self.ctx.community) else {
            return;
        };
        if self.restarting {
            return;
        }
        self.restarting = true;
        let rx = admin.restart(community, installation.id);
        self.run(rx, window, cx, |this, answer, _, _| {
            this.restarting = false;
            match answer {
                Ok(_) => this.status = None,
                Err(e) => this.error = Some(error_line(&e)),
            }
        });
    }

    /// Saves the capability switches, behind a fresh second factor.
    fn save_capabilities(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let (Some(admin), Some(community)) = (self.admin(), self.ctx.community) else {
            return;
        };
        let mask = self
            .edited
            .unwrap_or_else(|| starting_capabilities(self.installation().as_ref()));
        let (revision, id) = (self.installation().map(|i| i.revision), request_id());
        let Some(step_up) = cx.try_global::<StepUp>().cloned() else {
            self.error = Some(t("admin.bot.stepUpMissing").to_owned());
            cx.notify();
            return;
        };
        let this = cx.entity();
        (step_up.0)(
            Box::new(move |token, window, cx| {
                let rx = admin.configure_installation(community, id, revision, mask, token);
                this.update(cx, |page, cx| {
                    page.busy = true;
                    page.run(rx, window, cx, |page, answer, _, _| {
                        page.busy = false;
                        match answer {
                            Ok(_) => {
                                page.edited = None;
                                page.status = Some(t("admin.bot.capabilities.updated").to_owned());
                            }
                            Err(e) => page.error = Some(error_line(&e)),
                        }
                    });
                });
            }),
            window,
            cx,
        );
    }

    fn installation(&self) -> Option<Installation> {
        let community = self.ctx.community?;
        self.ctx
            .state
            .bot_admin
            .installation
            .get(&community)
            .cloned()
    }

    /// The Bot channels this member can view, as text channels with their names.
    fn bot_channels(&self) -> Vec<(ChannelId, String)> {
        let Some(community) = self.ctx.community else {
            return Vec::new();
        };
        let marked = self
            .ctx
            .state
            .bot_admin
            .catalog
            .get(&community)
            .and_then(|c| c.bot_channels.clone())
            .unwrap_or_default();
        self.ctx
            .state
            .channels
            .iter()
            .filter(|c| c.community_id == community && c.kind == 0 && marked.contains(&c.id))
            .map(|c| (c.id, c.name.clone()))
            .collect()
    }

    fn is_manager(&self) -> bool {
        let (Some(community), Some(me)) = (self.ctx.community, self.ctx.state.account.user_id)
        else {
            return false;
        };
        self.ctx
            .state
            .communities
            .iter()
            .any(|c| c.id == community && c.owner == me)
    }

    fn bot_name(&self) -> String {
        let Some(community) = self.ctx.community else {
            return t("serverSettings.bot.fallbackName").to_owned();
        };
        let user = self.installation().and_then(|i| i.user_id);
        self.ctx
            .state
            .members
            .get(&community)
            .and_then(|m| m.iter().find(|m| Some(m.user_id) == user))
            .map(|m| {
                if m.display.is_empty() {
                    m.username.clone()
                } else {
                    m.display.clone()
                }
            })
            .filter(|n| !n.is_empty())
            .unwrap_or_else(|| t("serverSettings.bot.fallbackName").to_owned())
    }
}

impl Render for BotPage {
    fn render(&mut self, window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        self.ensure_loaded();
        let manager = self.is_manager();
        let community = self.ctx.community;
        let installation = self.installation();
        let bot = self.bot_name();
        let server = community
            .and_then(|id| self.ctx.state.communities.iter().find(|c| c.id == id))
            .map(|c| c.name.clone())
            .unwrap_or_default();
        let enabled = installation.as_ref().is_some_and(|i| i.state != 4);
        let mask = self
            .edited
            .unwrap_or_else(|| starting_capabilities(installation.as_ref()));
        let dirty = installation
            .as_ref()
            .is_some_and(|i| i.state <= 3 && mask != i.capabilities);
        let busy = self.busy;

        // The managed Bot's card, the capability switches, the voice access row and the health line are one group.
        let presence = match installation.as_ref() {
            None => t("admin.bot.status.unavailable").to_owned(),
            Some(i) if i.state == 3 => t("serverSettings.bot.online").to_owned(),
            Some(i) if i.state == 4 => t("serverSettings.bot.offline").to_owned(),
            Some(i) => t(state_key(i.state)).to_lowercase(),
        };
        let card_hint = match installation.as_ref() {
            None => t("admin.bot.none").to_owned(),
            Some(i) => match &i.version {
                Some(v) => tf(
                    "serverSettings.bot.cardVersion",
                    &[("presence", &presence), ("version", v)],
                ),
                None => tf("serverSettings.bot.card", &[("presence", &presence)]),
            },
        };
        let health = community
            .and_then(|id| self.ctx.state.bot_admin.catalog.get(&id))
            .and_then(|c| health_line(c).map(|line| (c.clone(), line)));
        let mut rows: Vec<Box<dyn FnOnce(GroupPosition) -> AnyElement>> = Vec::new();

        {
            let (restarting, installation) = (self.restarting, installation.clone());
            let this = cx.entity();
            let bot_name = bot.clone();
            rows.push(Box::new(move |position| {
                let mut row = SettingsRow::new("bot-glenn", bot_name)
                    .icon(IconName::Bot)
                    .tag(t("serverSettings.bot.tag"))
                    .hint(card_hint)
                    .position(position);
                if enabled {
                    let target = installation.clone();
                    row = row.child(
                        Button::new(
                            "bot-restart",
                            if restarting {
                                t("serverSettings.bot.restarting")
                            } else {
                                t("serverSettings.bot.restart")
                            },
                        )
                        .size(ButtonSize::Sm)
                        .disabled(restarting || !manager)
                        .on_click(move |_: &ClickEvent, window, cx| {
                            if let Some(i) = &target {
                                this.update(cx, |page, cx| page.restart(i, window, cx));
                            }
                        }),
                    );
                } else {
                    let this2 = this.clone();
                    row = row.child(
                        Button::new("bot-enable", t("serverSettings.bot.enable"))
                            .size(ButtonSize::Sm)
                            .variant(ButtonVariant::Accent)
                            .disabled(busy || !manager)
                            .on_click(move |_: &ClickEvent, window, cx| {
                                this2.update(cx, |page, cx| page.save_capabilities(window, cx));
                            }),
                    );
                }
                row.into_any_element()
            }));
        }
        for (name, bit, icon) in CAPABILITIES {
            let this = cx.entity();
            let checked = mask & bit != 0;
            let disabled = busy
                || !manager
                || installation
                    .as_ref()
                    .is_some_and(|i| i.state > 3 && i.state != 4);
            rows.push(Box::new(move |position| {
                let label = t(&format!("serverSettings.bot.cap.{name}"));
                SettingsRow::new(format!("bot-capability-{name}"), label)
                    .icon(icon)
                    .hint(t(&format!("serverSettings.bot.cap.{name}.hint")))
                    .position(position)
                    .child(
                        Toggle::new(format!("bot-capability-{name}-switch"), label, checked)
                            .disabled(disabled)
                            .on_change(move |on, _, cx| {
                                this.update(cx, |page, cx| {
                                    let base = page.edited.unwrap_or_else(|| {
                                        starting_capabilities(page.installation().as_ref())
                                    });
                                    page.edited = Some(if on { base | bit } else { base & !bit });
                                    cx.notify();
                                });
                            }),
                    )
                    .into_any_element()
            }));
        }
        {
            let bot_name = bot.clone();
            rows.push(Box::new(move |position| {
                SettingsRow::new("bot-voice", t("serverSettings.bot.voice"))
                    .icon(IconName::Lock)
                    .hint(tf("serverSettings.bot.voiceHint", &[("bot", &bot_name)]))
                    .position(position)
                    .child(Button::new(
                        "bot-voice-roles",
                        t("serverSettings.bot.voiceRoles"),
                    ))
                    .into_any_element()
            }));
        }
        if enabled && let Some((catalog, (key, since))) = health.clone() {
            rows.push(Box::new(move |position| {
                let label = match since {
                    Some(ts) => tf(key, &[("time", &crate::format::day_time(ts))]),
                    None => t(key).to_owned(),
                };
                let mut row = SettingsRow::new("bot-health", label)
                    .icon(IconName::Activity)
                    .position(position);
                if let Some(fingerprint) = &catalog.fingerprint {
                    row = row.hint(tf("bot.key.fingerprint", &[("fingerprint", fingerprint)]));
                }
                row.into_any_element()
            }));
        }
        let n = rows.len();
        let group: Vec<AnyElement> = rows
            .into_iter()
            .enumerate()
            .map(|(i, build)| build(GroupPosition::of(i, n)))
            .collect();

        let mut page = div()
            .flex()
            .flex_col()
            .child(section_header(
                t("admin.bot.title"),
                Some(&tf(
                    "serverSettings.bot.body",
                    &[("bot", &bot), ("server", &server)],
                )),
            ))
            .children(group);

        if dirty {
            let (save, cancel) = (cx.entity(), cx.entity());
            page = page.child(
                div()
                    .flex()
                    .gap_2()
                    .pt_3()
                    .child(
                        Button::new("bot-capabilities-save", t("admin.bot.capabilities.save"))
                            .variant(ButtonVariant::Accent)
                            .disabled(busy || !manager)
                            .on_click(move |_: &ClickEvent, window, cx| {
                                save.update(cx, |p, cx| p.save_capabilities(window, cx));
                            }),
                    )
                    .child(
                        Button::new(
                            "bot-capabilities-cancel",
                            t("admin.bot.capabilities.cancel"),
                        )
                        .on_click(move |_: &ClickEvent, _, cx| {
                            cancel.update(cx, |p, cx| {
                                p.edited = None;
                                cx.notify();
                            });
                        }),
                    ),
            );
        }

        if enabled && installation.is_some() {
            page = page.child(self.channels_group(manager, cx));
            if manager {
                page = page.child(self.webhooks_group(window, cx));
            }
        }
        if let Some(error) = &self.error {
            page = page.child(
                div()
                    .pt_3()
                    .child(SharedString::from(error.clone()))
                    .id("bot-error"),
            );
        }
        if let Some(status) = &self.status {
            page = page.child(
                div()
                    .pt_3()
                    .child(SharedString::from(status.clone()))
                    .id("bot-status"),
            );
        }
        page.child(self.dialogs(window, cx))
    }
}

impl BotPage {
    fn channels_group(&mut self, manager: bool, cx: &mut Context<Self>) -> AnyElement {
        let community = self.ctx.community;
        let catalog = community
            .and_then(|id| self.ctx.state.bot_admin.catalog.get(&id))
            .cloned();
        let needs = community
            .and_then(|id| self.ctx.state.bot_admin.needs_re_add.get(&id))
            .cloned()
            .unwrap_or_default();
        let channels = self.bot_channels();
        let mut group = div()
            .flex()
            .flex_col()
            .child(group_heading(t("bot.channels.title")));
        if catalog.as_ref().is_some_and(|c| c.bot_channels.is_some()) && channels.is_empty() {
            group = group.child(div().child(t("bot.channels.empty")));
        }
        let n = channels.len();
        for (i, (id, name)) in channels.into_iter().enumerate() {
            let stale = needs.contains(&id);
            let note = self
                .re_add_note
                .as_ref()
                .filter(|(c, _)| *c == id)
                .map(|(_, text)| text.clone());
            let mut row = SettingsRow::new(format!("bot-channel-{}", id.hex()), format!("#{name}"))
                .icon(IconName::Hash)
                .tag(t("bot.channel.badge"))
                .position(GroupPosition::of(i, n));
            if stale {
                row = row.hint(note.unwrap_or_else(|| t("bot.channels.needsReAdd").to_owned()));
            } else if let Some(note) = note {
                row = row.hint(note);
            }
            if stale && manager {
                let this = cx.entity();
                let adding = self.re_adding == Some(id);
                row = row.child(
                    Button::new(
                        format!("bot-channel-{}-readd", id.hex()),
                        if adding {
                            t("bot.channels.reAdding")
                        } else {
                            t("bot.channels.reAdd")
                        },
                    )
                    .size(ButtonSize::Sm)
                    .disabled(self.re_adding.is_some())
                    .on_click(move |_: &ClickEvent, window, cx| {
                        this.update(cx, |page, cx| page.re_add(id, window, cx));
                    }),
                );
            }
            group = group.child(row);
        }
        group.into_any_element()
    }

    fn webhooks_group(&mut self, _window: &mut Window, cx: &mut Context<Self>) -> AnyElement {
        let Some(community) = self.ctx.community else {
            return div().into_any_element();
        };
        let hooks = self.ctx.state.bot_admin.webhooks.get(&community).cloned();
        let channels = self.bot_channels();
        let name_of = |id: ChannelId| {
            channels
                .iter()
                .find(|(c, _)| *c == id)
                .map(|(_, n)| n.clone())
                .unwrap_or_default()
        };
        let mut group = div()
            .flex()
            .flex_col()
            .child(group_heading(t("bot.webhooks.title")));
        if hooks.as_ref().is_some_and(Vec::is_empty) {
            group = group.child(div().child(t("bot.webhooks.empty")));
        }
        let rows = hooks.unwrap_or_default();
        let n = rows.len();
        for (i, hook) in rows.into_iter().enumerate() {
            let id_hex = hex(&hook.webhook_id);
            let (toggle_page, rotate_page, delete_page) = (cx.entity(), cx.entity(), cx.entity());
            let (for_toggle, for_rotate, for_delete) = (hook.clone(), hook.clone(), hook.clone());
            let busy = self.busy;
            group = group.child(
                SettingsRow::new(format!("bot-hook-{id_hex}"), hook.name.clone())
                    .icon(IconName::Link)
                    .hint(tf(
                        "bot.webhooks.posts",
                        &[("channel", &name_of(hook.channel_id))],
                    ))
                    .position(GroupPosition::of(i, n))
                    .child(
                        Toggle::new(
                            format!("bot-hook-{id_hex}-everyone"),
                            t("bot.webhooks.allowEveryone"),
                            hook.allow_everyone,
                        )
                        .on_change(move |on, _, cx| {
                            let Some(admin) = toggle_page.read(cx).admin() else {
                                return;
                            };
                            let Some(community) = toggle_page.read(cx).ctx.community else {
                                return;
                            };
                            drop(admin.set_webhook_everyone(community, for_toggle.webhook_id, on));
                        }),
                    )
                    .child(
                        Button::new(
                            format!("bot-hook-{id_hex}-rotate"),
                            t("bot.webhooks.rotate"),
                        )
                        .size(ButtonSize::Sm)
                        .disabled(busy)
                        .on_click(move |_: &ClickEvent, window, cx| {
                            rotate_page.update(cx, |page, cx| page.rotate(&for_rotate, window, cx));
                        }),
                    )
                    .child(
                        Button::new(
                            format!("bot-hook-{id_hex}-delete"),
                            t("bot.webhooks.delete"),
                        )
                        .size(ButtonSize::Sm)
                        .variant(ButtonVariant::Danger)
                        .disabled(busy)
                        .on_click(move |_: &ClickEvent, _, cx| {
                            delete_page.update(cx, |page, cx| {
                                page.deleting = Some(for_delete.clone());
                                cx.notify();
                            });
                        }),
                    ),
            );
        }
        let no_channel = channels.is_empty();
        if no_channel {
            group = group.child(div().child(t("bot.webhooks.needsChannel")));
        }
        let this = cx.entity();
        group
            .child(
                div().pt_3().child(
                    Button::new("bot-webhook-create", t("bot.webhooks.create"))
                        .disabled(no_channel)
                        .on_click(move |_: &ClickEvent, _, cx| {
                            this.update(cx, |page, cx| {
                                page.creating = true;
                                cx.notify();
                            });
                        }),
                ),
            )
            .into_any_element()
    }

    /// The create dialog, the one-time address and the delete confirmation.
    fn dialogs(&mut self, _window: &mut Window, cx: &mut Context<Self>) -> AnyElement {
        let channels = self.bot_channels();
        let selected = self.channel.or_else(|| channels.first().map(|c| c.0));
        let options: Vec<(String, String)> = channels
            .iter()
            .map(|(id, n)| (id.hex(), format!("#{n}")))
            .collect();
        let option_refs: Vec<(&str, &str)> = options
            .iter()
            .map(|(v, l)| (v.as_str(), l.as_str()))
            .collect();
        let busy = self.busy;
        let (close_create, submit, pick, every) =
            (cx.entity(), cx.entity(), cx.entity(), cx.entity());
        let name_empty = self.name.read(cx).value().trim().is_empty();
        let create = Dialog::new(
            "bot-webhook-new",
            t("bot.webhooks.newTitle"),
            t("admin.cancel"),
        )
        .open(self.creating)
        .can_close(!busy)
        .initial_focus(InitialFocus::Dialog)
        .body(
            div()
                .flex()
                .flex_col()
                .gap_3()
                .child(TextField::new(
                    "bot-webhook-name",
                    t("bot.webhooks.name"),
                    &self.name,
                ))
                .child(
                    Select::new(
                        "bot-webhook-channel",
                        t("bot.webhooks.channel"),
                        &option_refs,
                        selected.map(|c| c.hex()).unwrap_or_default(),
                    )
                    .on_change(move |value, _, cx| {
                        let id = value
                            .as_bytes()
                            .chunks(2)
                            .filter_map(|p| {
                                u8::from_str_radix(std::str::from_utf8(p).ok()?, 16).ok()
                            })
                            .collect::<Vec<u8>>();
                        if let Ok(id) = <[u8; 16]>::try_from(id) {
                            pick.update(cx, |page, cx| {
                                page.channel = Some(ChannelId(id));
                                cx.notify();
                            });
                        }
                    }),
                )
                .child(
                    Toggle::new(
                        "bot-webhook-everyone",
                        t("bot.webhooks.allowEveryone"),
                        self.allow_everyone,
                    )
                    .show_label(true)
                    .on_change(move |on, _, cx| {
                        every.update(cx, |page, cx| {
                            page.allow_everyone = on;
                            cx.notify();
                        });
                    }),
                ),
        )
        .footer(
            Button::new("bot-webhook-submit", t("bot.webhooks.create"))
                .variant(ButtonVariant::Accent)
                .disabled(busy || name_empty)
                .on_click(move |_: &ClickEvent, window, cx| {
                    submit.update(cx, |page, cx| page.create_webhook(window, cx));
                }),
        )
        .on_close(move |_, cx| {
            close_create.update(cx, |page, cx| {
                if !page.busy {
                    page.creating = false;
                    cx.notify();
                }
            });
        });

        // The one-time address: held in a zeroizing string while the dialog is open, dropped when it closes.
        let (done, copy) = (cx.entity(), cx.entity());
        let shown = self
            .shown
            .as_ref()
            .map(|(name, url)| (name.clone(), url.clone()));
        let copied = self.copied;
        let address = Dialog::new(
            "bot-webhook-address",
            t("bot.webhooks.addressTitle"),
            t("admin.cancel"),
        )
        .open(shown.is_some())
        .show_default_close(false)
        .body(
            div()
                .flex()
                .flex_col()
                .gap_3()
                .child(t("bot.webhooks.urlOnce"))
                .child(SharedString::from(
                    shown
                        .as_ref()
                        .map(|(name, url)| format!("{name}: {}", url.as_str()))
                        .unwrap_or_default(),
                )),
        )
        .footer(
            Button::new(
                "bot-webhook-copy",
                if copied {
                    t("bot.webhooks.copied")
                } else {
                    t("bot.webhooks.copy")
                },
            )
            .on_click(move |_: &ClickEvent, _, cx| {
                let url = copy.read(cx).shown.as_ref().map(|(_, url)| url.clone());
                if let Some(url) = url {
                    cx.write_to_clipboard(gpui::ClipboardItem::new_string(url.as_str().to_owned()));
                    copy.update(cx, |page, cx| {
                        page.copied = true;
                        cx.notify();
                    });
                }
            }),
        )
        .footer(
            Button::new("bot-webhook-done", t("bot.webhooks.done"))
                .variant(ButtonVariant::Accent)
                .on_click(move |_: &ClickEvent, _, cx| {
                    done.update(cx, |page, cx| {
                        page.shown = None;
                        cx.notify();
                    });
                }),
        )
        .on_close({
            let close = cx.entity();
            move |_, cx| {
                close.update(cx, |page, cx| {
                    page.shown = None;
                    cx.notify();
                });
            }
        });

        let (confirm, close) = (cx.entity(), cx.entity());
        let deleting_name = self
            .deleting
            .as_ref()
            .map(|h| h.name.clone())
            .unwrap_or_default();
        let delete = ConfirmDialog::new(
            "bot-webhook-delete",
            t("bot.webhooks.delete"),
            tf("bot.webhooks.deleteBody", &[("name", &deleting_name)]),
            t("bot.webhooks.delete"),
            t("bot.webhooks.delete"),
            t("admin.cancel"),
        )
        .open(self.deleting.is_some())
        .busy(busy)
        .on_confirm(move |window, cx| confirm.update(cx, |page, cx| page.remove(window, cx)))
        .on_close(move |_, cx| {
            close.update(cx, |page, cx| {
                if !page.busy {
                    page.deleting = None;
                    cx.notify();
                }
            });
        });
        div()
            .child(create)
            .child(address)
            .child(delete)
            .into_any_element()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use gpui::TestAppContext;

    #[test]
    fn the_capability_rows_are_the_ten_of_the_bot_page_in_its_order() {
        let names: Vec<&str> = CAPABILITIES.iter().map(|c| c.0).collect();
        assert_eq!(
            names,
            [
                "music",
                "polls",
                "reminders",
                "welcome",
                "roles",
                "announcements",
                "warnings",
                "removal",
                "kick",
                "ban"
            ]
        );
        let all = CAPABILITIES.iter().fold(0u64, |mask, c| mask | c.1);
        assert_eq!(
            all,
            (1 << 10) - 1,
            "ten distinct bits under the instance's mask bound"
        );
        assert_eq!(
            CAPABILITIES.iter().map(|c| c.1.count_ones()).sum::<u32>(),
            10
        );
        // Every row has its label and hint in the strings.
        for (name, _, _) in CAPABILITIES {
            assert!(
                crate::strings::try_t(&format!("serverSettings.bot.cap.{name}")).is_some(),
                "{name}"
            );
            assert!(
                crate::strings::try_t(&format!("serverSettings.bot.cap.{name}.hint")).is_some(),
                "{name}"
            );
        }
    }

    #[test]
    fn a_first_enable_offers_polls_and_reminders_on_and_an_installation_keeps_its_mask() {
        assert_eq!(starting_capabilities(None), (1 << 8) | (1 << 9));
        let installation = Installation {
            id: [1; 16],
            state: 4,
            revision: 2,
            user_id: None,
            capabilities: 0b11,
            updated_at: 0,
            restart_requested: 0,
            restart_handled: 0,
            version: None,
            music_volume: 50,
            music_idle_minutes: 15,
        };
        assert_eq!(starting_capabilities(Some(&installation)), 0b11);
    }

    #[test]
    fn the_status_names_follow_the_installation_state() {
        assert_eq!(state_key(0), "admin.bot.status.pending");
        assert_eq!(state_key(2), "admin.bot.status.joining");
        assert_eq!(state_key(3), "admin.bot.status.ready");
        assert_eq!(state_key(4), "admin.bot.status.disabled");
        assert_eq!(state_key(5), "admin.bot.status.failed");
    }

    #[test]
    fn a_refusal_reads_as_the_managers_not_the_codes() {
        assert_eq!(
            error_line(&ClientError::Sync("E_FORBIDDEN".into())),
            t("admin.bot.error.forbidden")
        );
        assert_eq!(
            error_line(&ClientError::Sync("E_CONFLICT".into())),
            t("admin.bot.error.conflict")
        );
        assert_eq!(
            error_line(&ClientError::Sync("E_SOMETHING".into())),
            tf("admin.bot.error.other", &[("code", "E_SOMETHING")])
        );
    }

    fn page(cx: &mut TestAppContext) -> (Entity<BotPage>, &mut gpui::VisualTestContext) {
        cx.update(crate::install);
        cx.add_window_view(|window, cx| BotPage::new(SlotCx::default(), window, cx))
    }

    /// The one-time address: held while its dialog is open and gone when it closes.
    #[gpui::test]
    fn the_webhook_address_is_dropped_when_its_dialog_closes(cx: &mut TestAppContext) {
        let (page, cx) = page(cx);
        page.update(cx, |p, cx| {
            p.shown = Some((
                "CI".to_owned(),
                Zeroizing::new("https://dilla.test/hooks/a/SECRET".to_owned()),
            ));
            cx.notify();
        });
        cx.run_until_parked();
        assert!(
            page.read_with(cx, |p, _| p.shown.is_some()),
            "the address shows once it is made"
        );
        cx.simulate_keystrokes("escape");
        cx.run_until_parked();
        assert!(
            page.read_with(cx, |p, _| p.shown.is_none()),
            "Escape closes the dialog and drops the address"
        );
    }
}
