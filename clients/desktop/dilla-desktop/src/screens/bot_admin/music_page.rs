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
use dilla_desktop_ui::components::settings::frame::{SECTION_GAP, section_header_full};
use dilla_desktop_ui::components::settings::row::{GroupPosition, Layout, SettingsRow};
use gpui::{
    AnyElement, ClickEvent, Context, InteractiveElement, IntoElement, ParentElement, Render,
    SharedString, Styled, Window, div, px,
};

use super::bot_page::error_line;
use crate::slots::SlotCx;
use crate::strings::{t, tf};
use dilla_client::state::roles::{CommunityAccess, RoleId, RoleSummary};

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

/// BotSection.tsx `djRoles`: the roles whose allow holds Control music, in the order the roles slice lists them.
pub fn dj_roles(roles: &[RoleSummary]) -> Vec<&RoleSummary> {
    roles
        .iter()
        .filter(|r| r.allow & dilla_client::features::roles::perm::CONTROL_MUSIC != 0)
        .collect()
}

/// The Who-can-DJ row's hint: the DJ roles by name (@everyone as `roles.everyone`), or that none has the permission.
pub fn dj_hint(roles: &[RoleSummary], access: &CommunityAccess) -> String {
    let dj = dj_roles(roles);
    if dj.is_empty() {
        return t("serverSettings.music.djNone").to_owned();
    }
    let names = dj
        .iter()
        .map(|r| {
            if Some(r.id) == access.everyone_id {
                t("roles.everyone").to_owned()
            } else {
                r.name.clone()
            }
        })
        .collect::<Vec<_>>()
        .join(", ");
    tf("serverSettings.music.djRoles", &[("roles", &names)])
}

/// BotSection.tsx `editDjRoles`: the role Edit in Roles opens, the first DJ role that is not @everyone (else @everyone).
pub fn dj_role_to_open(roles: &[RoleSummary], access: &CommunityAccess) -> Option<RoleId> {
    let dj = dj_roles(roles);
    dj.iter()
        .find(|r| Some(r.id) != access.everyone_id)
        .or(dj.first())
        .map(|r| r.id)
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
        let (dj_hint, dj_open) = community.map_or_else(
            || (t("serverSettings.music.djNone").to_owned(), None),
            |id| {
                let roles = self.ctx.state.roles.roles_of(id);
                let access = self.ctx.state.roles.access_of(id);
                (dj_hint(roles, &access), dj_role_to_open(roles, &access))
            },
        );
        rows.push(Box::new(move |position| {
            SettingsRow::new("music-dj", t("serverSettings.music.dj"))
                .full_measure()
                .icon(IconName::Disc3)
                .hint(dj_hint)
                .position(position)
                .child(
                    Button::new("music-dj-edit", t("serverSettings.music.djEdit")).on_click(
                        move |_, window, cx| {
                            // Roles, with the first DJ role open in its editor.
                            if let Some(role) = dj_open {
                                crate::screens::members::roles::select_role(
                                    window.window_handle().window_id(),
                                    role,
                                    cx,
                                );
                            }
                            crate::screens::server_settings::open_section(
                                crate::router::ServerSection::Roles,
                                window,
                                cx,
                            );
                        },
                    ),
                )
                .into_any_element()
        }));
        {
            let this = cx.entity();
            let installation_known = installation.is_some();
            rows.push(Box::new(move |position| {
                let mut row =
                    SettingsRow::new("music-provider-youtube", t("serverSettings.music.youtube"))
                        .full_measure()
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
                    .full_measure()
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
                    .full_measure()
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
                    .full_measure()
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

        // BotSection.tsx: `data-measure="full"`, the intro takes the column's width.
        let mut page = div().flex().flex_col().child(section_header_full(
            t("serverSettings.music.title"),
            Some(&tf("serverSettings.music.body", &[("bot", &bot)])),
        ));
        if installation.as_ref().is_none_or(|i| i.state != 3) {
            // `.dw-settings-section` steps its blocks `--group-gap` apart.
            page = page.child(div().flex().pb(px(SECTION_GAP)).child(crate::tracked_box(
                "music-open-bot",
                Button::new("music-open-bot-b", t("workspace.botSetup")).on_click(
                    |_: &ClickEvent, window, cx| {
                        crate::screens::server_settings::open_section(
                            crate::router::ServerSection::Bot,
                            window,
                            cx,
                        );
                    },
                ),
            )));
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

#[cfg(test)]
mod tests {
    use super::*;
    use dilla_client::features::roles::perm::CONTROL_MUSIC;

    fn role(id: u8, name: &str, allow: u32) -> RoleSummary {
        RoleSummary {
            id: [id; 16],
            name: name.into(),
            color: 0,
            position: u32::from(id),
            hoist: false,
            mentionable: false,
            allow,
            deny: 0,
            members: 0,
        }
    }

    /// BotSection.tsx `djRoles`, `djNames` and `editDjRoles`: the hint names every role with Control music (@everyone as
    /// "@everyone"), says none has it otherwise, and Edit in Roles opens the first DJ role that is not @everyone.
    #[test]
    fn the_dj_row_names_the_roles_with_control_music() {
        let access = CommunityAccess {
            everyone_id: Some([0; 16]),
            ..CommunityAccess::default()
        };
        let none = [role(0, "@everyone", 1), role(1, "crew", 1)];
        assert_eq!(dj_hint(&none, &access), t("serverSettings.music.djNone"));
        assert_eq!(dj_role_to_open(&none, &access), None);
        let some = [
            role(0, "@everyone", CONTROL_MUSIC),
            role(1, "crew", 1),
            role(2, "djs", CONTROL_MUSIC | 1),
        ];
        assert_eq!(
            dj_hint(&some, &access),
            tf(
                "serverSettings.music.djRoles",
                &[("roles", &format!("{}, djs", t("roles.everyone")))]
            )
        );
        assert_eq!(dj_role_to_open(&some, &access), Some([2; 16]));
        assert_eq!(dj_role_to_open(&some[..1], &access), Some([0; 16]));
    }
}
