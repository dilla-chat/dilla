//! Server settings, Music (`BotSection` page `music`, `MusicProviderStatus.tsx`): who can DJ, the YouTube provider's status
//! with Check again, the last music starts, and the defaults that save as they are picked: the volume and how long the Bot
//! stays in voice when idle.

use std::rc::Rc;
use std::time::{SystemTime, UNIX_EPOCH};

use dilla_client::features::bot_admin::{BotAdmin, provider_is_stale};
use dilla_client::state::CommunityId;
use dilla_client::state::bot_admin::{
    Installation, MUSIC_IDLE_MINUTES, MUSIC_VOLUMES, ProviderSetup, ProviderStatusState,
};
use dilla_client::{Client, ClientError};
use dilla_desktop_ui::IconName;
use dilla_desktop_ui::components::Button;
use dilla_desktop_ui::components::segmented::Segmented;
use dilla_desktop_ui::components::settings::frame::section_header;
use dilla_desktop_ui::components::settings::row::{GroupPosition, Layout, SettingsRow};
use gpui::{
    AnyElement, ClickEvent, Context, InteractiveElement, IntoElement, ParentElement, Render,
    SharedString, Styled, Window, div,
};

use super::bot_page::error_line;
use crate::slots::SlotCx;
use crate::strings::{t, tf};

/// The provider row's hint (`youtubeHint`): what the status is, as the person reads it.
pub fn provider_hint(
    state: &ProviderStatusState,
    authorised: bool,
    installation: bool,
    now_s: u64,
) -> &'static str {
    let stale =
        matches!(state, ProviderStatusState::Known(s) if provider_is_stale(s.observed_at, now_s));
    // No installation to ask (or no manager to ask for): the page never sends the read, so it stands "unavailable".
    if !authorised || !installation {
        return "workspace.providers.unavailable";
    }
    let busy = matches!(state, ProviderStatusState::Loading);
    if busy {
        return "workspace.providers.loading";
    }
    if stale {
        return "workspace.providers.stale";
    }
    match state {
        _ if !authorised || matches!(state, ProviderStatusState::Unavailable) => {
            "workspace.providers.unavailable"
        }
        ProviderStatusState::Partial => "workspace.providers.partial",
        ProviderStatusState::Known(s) => match s.youtube_playback {
            ProviderSetup::Configured => "serverSettings.music.youtubeReady",
            ProviderSetup::MissingRequirements => "serverSettings.music.youtubeMissing",
            ProviderSetup::Unknown => "serverSettings.music.youtubeUnknown",
        },
        _ => "serverSettings.music.youtubeUnknown",
    }
}

pub struct MusicPage {
    ctx: SlotCx,
    requested: Option<CommunityId>,
    error: Option<String>,
}

fn now_s() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_or(0, |d| d.as_secs())
}

impl MusicPage {
    pub fn new(ctx: SlotCx) -> Self {
        Self {
            ctx,
            requested: None,
            error: None,
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

    fn installation(&self) -> Option<Installation> {
        let community = self.ctx.community?;
        self.ctx
            .state
            .bot_admin
            .installation
            .get(&community)
            .cloned()
    }

    fn is_manager(&self) -> bool {
        super::is_manager(&self.ctx)
    }

    /// The clock the page reads: the fixture's frozen one, else the device's.
    fn now(&self) -> u64 {
        self.ctx.clock.now()
    }

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
            drop(admin.refresh_status(community));
        }
    }

    fn check_provider(&mut self) {
        let (Some(admin), Some(community), Some(installation)) =
            (self.admin(), self.ctx.community, self.installation())
        else {
            return;
        };
        drop(admin.refresh_provider_status(community, installation.id, now_s()));
    }

    fn save(&mut self, volume: u8, idle: u8, window: &mut Window, cx: &mut Context<Self>) {
        let (Some(admin), Some(community), Some(installation)) =
            (self.admin(), self.ctx.community, self.installation())
        else {
            return;
        };
        self.error = None;
        let rx = admin.set_music_settings(community, installation.id, volume, idle);
        cx.spawn_in(window, async move |this, cx| {
            let answer: Result<Installation, ClientError> =
                rx.await.unwrap_or(Err(ClientError::Stopped));
            let _ = this.update(cx, |this, cx| {
                this.error = answer.err().map(|e| error_line(&e));
                cx.notify();
            });
        })
        .detach();
    }
}

impl Render for MusicPage {
    fn render(&mut self, window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let _ = window;
        self.ensure_loaded();
        let community = self.ctx.community;
        let manager = self.is_manager();
        let installation = self.installation();
        let bot = community
            .and_then(|id| {
                let user = installation.as_ref().and_then(|i| i.user_id)?;
                self.ctx
                    .state
                    .members
                    .get(&id)?
                    .iter()
                    .find(|m| m.user_id == user)
                    .map(|m| {
                        if m.display.is_empty() {
                            m.username.clone()
                        } else {
                            m.display.clone()
                        }
                    })
            })
            .filter(|n| !n.is_empty())
            .unwrap_or_else(|| t("serverSettings.bot.fallbackName").to_owned());
        let volume = installation.as_ref().map_or(50, |i| i.music_volume);
        let idle = installation.as_ref().map_or(15, |i| i.music_idle_minutes);
        let provider = community
            .and_then(|id| self.ctx.state.bot_admin.provider.get(&id))
            .cloned()
            .unwrap_or_default();
        let now = self.now();
        let known_now = match &provider {
            ProviderStatusState::Known(s) if !provider_is_stale(s.observed_at, now) => {
                Some(s.youtube_playback)
            }
            _ => None,
        };
        let hint = provider_hint(&provider, manager, installation.is_some(), now);
        let busy =
            manager && matches!(provider, ProviderStatusState::Loading) && installation.is_some();
        let starts = community
            .and_then(|id| self.ctx.state.bot_admin.music_starts.get(&id))
            .cloned();

        let mut rows: Vec<Box<dyn FnOnce(GroupPosition) -> AnyElement>> = Vec::new();
        rows.push(Box::new(|position| {
            SettingsRow::new("music-dj", t("serverSettings.music.dj"))
                .icon(IconName::Disc3)
                .hint(t("serverSettings.music.djNone"))
                .position(position)
                .child(Button::new(
                    "music-dj-edit",
                    t("serverSettings.music.djEdit"),
                ))
                .into_any_element()
        }));
        {
            let this = cx.entity();
            let installation_known = installation.is_some();
            rows.push(Box::new(move |position| {
                let mut row =
                    SettingsRow::new("music-provider-youtube", t("serverSettings.music.youtube"))
                        .icon(IconName::Youtube)
                        .hint(t(hint))
                        .position(position);
                if known_now == Some(ProviderSetup::Configured) {
                    row = row.tag(t("serverSettings.music.youtubeTag"));
                }
                row.child(
                    Button::new("music-provider-check", t("serverSettings.music.checkAgain"))
                        .size(dilla_desktop_ui::components::ButtonSize::Sm)
                        .disabled(!manager || !installation_known || busy)
                        .on_click(move |_: &ClickEvent, _, cx| {
                            this.update(cx, |page, _| page.check_provider());
                        }),
                )
                .into_any_element()
            }));
        }
        if let Some(starts) = starts {
            rows.push(Box::new(move |position| {
                let mut row = SettingsRow::new("music-start-timings", t("bot.music.timings"))
                    .icon(IconName::Timer)
                    .layout(Layout::Stacked)
                    .position(position);
                if starts.is_empty() {
                    row = row.hint(t("bot.music.noStarts"));
                } else {
                    row = row.child(div().flex().flex_col().children(
                        starts.iter().enumerate().map(|(i, s)| {
                            let key = if s.cold {
                                "bot.music.startCold"
                            } else {
                                "bot.music.start"
                            };
                            div().id(("music-start", i)).child(SharedString::from(tf(
                                key,
                                &[("ms", &s.first_frame_ms.to_string())],
                            )))
                        }),
                    ));
                }
                row.into_any_element()
            }));
        }
        {
            let this = cx.entity();
            let disabled = !manager || installation.is_none();
            let options: Vec<(String, String)> = MUSIC_VOLUMES
                .iter()
                .map(|v| (v.to_string(), v.to_string()))
                .collect();
            rows.push(Box::new(move |position| {
                let refs: Vec<(&str, &str)> = options
                    .iter()
                    .map(|(v, l)| (v.as_str(), l.as_str()))
                    .collect();
                SettingsRow::new("music-volume", t("serverSettings.music.volume"))
                    .icon(IconName::Volume2)
                    .hint(t("serverSettings.music.volumeHint"))
                    .position(position)
                    .child(
                        Segmented::new("music-volume-choice", &refs, volume.to_string())
                            .disabled(disabled)
                            .on_change(move |value, window, cx| {
                                let Ok(v) = value.parse::<u8>() else { return };
                                this.update(cx, |page, cx| page.save(v, idle, window, cx));
                            }),
                    )
                    .into_any_element()
            }));
        }
        {
            let this = cx.entity();
            let disabled = !manager || installation.is_none();
            let bot_name = bot.clone();
            let options: Vec<(String, String)> = MUSIC_IDLE_MINUTES
                .iter()
                .map(|v| {
                    (
                        v.to_string(),
                        tf(
                            "serverSettings.music.idle.option",
                            &[("minutes", &v.to_string())],
                        ),
                    )
                })
                .collect();
            rows.push(Box::new(move |position| {
                let refs: Vec<(&str, &str)> = options
                    .iter()
                    .map(|(v, l)| (v.as_str(), l.as_str()))
                    .collect();
                SettingsRow::new("music-idle", t("serverSettings.music.idle"))
                    .icon(IconName::Timer)
                    .hint(tf("serverSettings.music.idleHint", &[("bot", &bot_name)]))
                    .position(position)
                    .child(
                        Segmented::new("music-idle-choice", &refs, idle.to_string())
                            .disabled(disabled)
                            .on_change(move |value, window, cx| {
                                let Ok(v) = value.parse::<u8>() else { return };
                                this.update(cx, |page, cx| page.save(volume, v, window, cx));
                            }),
                    )
                    .into_any_element()
            }));
        }
        let n = rows.len();
        let group: Vec<AnyElement> = rows
            .into_iter()
            .enumerate()
            .map(|(i, b)| b(GroupPosition::of(i, n)))
            .collect();

        let mut page = div().flex().flex_col().child(section_header(
            t("serverSettings.music.title"),
            Some(&tf("serverSettings.music.body", &[("bot", &bot)])),
        ));
        if installation.as_ref().is_none_or(|i| i.state != 3) {
            page = page.child(
                div()
                    .flex()
                    .pb_3()
                    .child(Button::new("music-open-bot", t("workspace.botSetup"))),
            );
        }
        page = page.children(group);
        if let Some(error) = &self.error {
            page = page.child(
                div()
                    .id("music-error")
                    .pt_3()
                    .child(SharedString::from(error.clone())),
            );
        }
        page
    }
}
