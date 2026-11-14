//! Server settings → Self-service roles, as SelfRolesSection.tsx draws it (Task 28 wave 2, lane B):
//!
//! - managers approve the eligible roles in a box of checkboxes ("Allow members to assign these roles");
//! - every member opts into the approved ones in a second box ("Your roles");
//! - managers publish a role panel: an emoji per approved role, a Bot channel, Publish (sealed operation 31, then
//!   `register_role_panel`: [`dilla_client::features::role_panels`]). No answer in time keeps the request, and Retry
//!   sends the same request id (the Bot answers it again and never posts twice).
//!
//! The boxes are the browser's own `<fieldset>` and checkboxes (the web leaves them unstyled): a 2px edge, the legend on
//! it, `0.35em 0.75em 0.625em` inside, 13px boxes with `3px 3px 3px 4px` around them, the labels in the body's text.

use std::collections::HashMap;
use std::rc::Rc;

use dilla_client::Client;
use dilla_client::features::role_panels::{PanelError, PanelOption, panel_valid};
use dilla_client::features::roles::{request_id, self_role_candidates};
use dilla_client::state::roles::{RoleId, RoleSummary};
use dilla_client::state::{AppState, ChannelId, CommunityId};
use gpui::{
    AnyElement, App, AppContext as _, Entity, InteractiveElement, IntoElement, ParentElement,
    SharedString, StatefulInteractiveElement, Styled, Window, WindowId, div, px,
};
use gpui_component::input::{Input, InputState};

use dilla_desktop_ui::components::button::Button;
use dilla_desktop_ui::components::select::Select;
use dilla_desktop_ui::components::settings::frame::section_header;
use dilla_desktop_ui::components::settings::row::SettingsRow;
use dilla_desktop_ui::text::{line, line_height, paragraph};
use dilla_desktop_ui::tokens::{color, font, radius, text};

use super::{ensure_loaded, hex, roles_job, sorted_roles};
use crate::slots::SlotCx;
use crate::strings::{t, tf};

/// The browser's `<fieldset>` and checkbox metrics (html.css): `padding: 0.35em 0.75em 0.625em`, `margin: 0 2px`, a 2px
/// edge; the legend `padding-inline: 2px`; a checkbox 13px with `margin: 3px 3px 3px 4px`.
const FIELDSET_PAD_TOP_EM: f32 = 0.35;
const FIELDSET_PAD_X_EM: f32 = 0.75;
const FIELDSET_PAD_BOTTOM_EM: f32 = 0.625;
const FIELDSET_MARGIN_X: f32 = 2.0;
const FIELDSET_EDGE: f32 = 2.0;
const LEGEND_PAD_X: f32 = 2.0;
const CHECKBOX: f32 = 13.0;
const CHECKBOX_MARGIN: f32 = 3.0;
const CHECKBOX_MARGIN_START: f32 = 4.0;
/// The tick inside a checked box.
const CHECK_ICON: f32 = 11.0;
/// The section header's own 22px is replaced by the siblings' gaps below.
const NO_GAP: f32 = 0.0;
/// The emoji `<input maxlength=32>`'s box (a native text input is 20 characters at the UA's 13.33px sans, ~120px).
const EMOJI_FIELD_W: f32 = 120.0;
/// `.d-root`'s `line-height: var(--line-height)` (1.5).
const NORMAL_LINE: f32 = 1.5;
/// `.d-settings-frame__body > section > * + *`.
const SIBLING_GAP: f32 = 14.0;
/// `h2 + :not(p)`.
const AFTER_TITLE: f32 = 22.0;
/// The h3's UA size, 1.17em.
const H3_EM: f32 = 1.17;

/// A panel publication waiting for its answer (SelfRolesSection.tsx `PendingPanel`).
#[derive(Clone, Debug)]
struct Pending {
    request_id: [u8; 16],
    channel: ChannelId,
    options: Vec<PanelOption>,
    uncertain: bool,
}

/// The section's state in one window.
#[derive(Default)]
struct PanelUi {
    community: Option<CommunityId>,
    channel: Option<ChannelId>,
    emojis: HashMap<RoleId, Entity<InputState>>,
    pending: Option<Pending>,
    notice: Option<String>,
    error: Option<String>,
    busy: bool,
    installation_asked: bool,
}

fn ui(w: WindowId, cx: &mut App) -> &mut PanelUi {
    crate::per_window::get_mut::<PanelUi>(w, cx)
}

/// SelfRolesSection.tsx `panelEnabled`: a ready installation (state 3) with the Roles capability (4).
fn panel_enabled(state: &AppState, community: CommunityId) -> bool {
    state
        .bot_admin
        .installation
        .get(&community)
        .is_some_and(|i| i.state == 3 && i.capabilities & 4 != 0)
}

/// The text channels the Bot may post into (Bot v2 Task 22: a panel goes into a Bot channel only).
fn panel_channels(state: &AppState, community: CommunityId) -> Vec<(ChannelId, String)> {
    let bot = state
        .bot
        .catalogs
        .get(&community)
        .and_then(|c| c.bot_channels.clone())
        .unwrap_or_default();
    state
        .channels
        .iter()
        .filter(|c| c.community_id == community && c.kind == 0 && bot.contains(&c.id))
        .map(|c| (c.id, c.name.clone()))
        .collect()
}

fn body_line(s: impl Into<SharedString>, c: gpui::Hsla) -> AnyElement {
    line(
        s.into(),
        text::BASE,
        line_height(text::BASE, NORMAL_LINE),
        c,
    )
    .into_any_element()
}

fn body_paragraph(s: &str, width: f32) -> AnyElement {
    div()
        .flex()
        .flex_col()
        .children(paragraph(
            s,
            text::BASE,
            line_height(text::BASE, NORMAL_LINE),
            color::FG_2,
            width,
        ))
        .into_any_element()
}

/// A browser checkbox and its label, as `<label><input type="checkbox"> name</label>`.
fn checkbox(
    id: String,
    label: &str,
    checked: bool,
    disabled: bool,
    on_toggle: impl Fn(bool, &mut Window, &mut App) + 'static,
) -> AnyElement {
    let mut boxed = div()
        .size(px(CHECKBOX))
        .ml(px(CHECKBOX_MARGIN_START))
        .mr(px(CHECKBOX_MARGIN))
        .my(px(CHECKBOX_MARGIN))
        .flex_none()
        .rounded(radius::SM)
        .border_1()
        .border_color(color::LINE_STRONG)
        .bg(if checked {
            color::ACCENT
        } else {
            color::SURFACE_HI
        });
    if checked {
        boxed = boxed.child(
            dilla_desktop_ui::icon::Icon::new(dilla_desktop_ui::IconName::Check)
                .size(px(CHECK_ICON))
                .color(color::ACCENT_INK),
        );
    }
    let mut el = div()
        .id(SharedString::from(id))
        .flex()
        .items_center()
        .child(boxed)
        // `<input> {name}`: the space before the name is text.
        .child(body_line(format!(" {label}"), color::FG));
    if !disabled {
        el = el
            .cursor_pointer()
            .on_click(move |_, window, cx| on_toggle(!checked, window, cx));
    }
    el.into_any_element()
}

/// A `<fieldset>` with its legend on the top edge.
fn fieldset(legend: &str, items: Vec<AnyElement>) -> AnyElement {
    let size = f32::from(text::BASE);
    let legend_h = size * NORMAL_LINE;
    div()
        .relative()
        .mx(px(FIELDSET_MARGIN_X))
        // The legend's line sits on the edge: half of it above.
        .pt(px(legend_h / 2.0))
        .child(
            div()
                .border(px(FIELDSET_EDGE))
                .border_color(color::FG_STRONG)
                .pt(px(
                    legend_h / 2.0 + size * FIELDSET_PAD_TOP_EM - FIELDSET_EDGE
                ))
                .px(px(size * FIELDSET_PAD_X_EM))
                .pb(px(size * FIELDSET_PAD_BOTTOM_EM))
                .flex()
                .flex_wrap()
                .items_center()
                .gap_x(px(size * 0.6))
                .children(items),
        )
        .child(
            div()
                .absolute()
                .top_0()
                .left(px(size * FIELDSET_PAD_X_EM))
                .px(px(LEGEND_PAD_X))
                .bg(color::BG)
                .child(body_line(legend.to_owned(), color::FG)),
        )
        .into_any_element()
}

/// The section (`slots::settings_sections`' Self-assignable roles).
pub fn section(
    w: WindowId,
    ctx: &SlotCx,
    community: CommunityId,
    window: &mut Window,
    cx: &mut App,
) -> AnyElement {
    ensure_loaded(w, "people", community, ctx.client.as_ref(), cx);
    ensure_loaded(w, "self", community, ctx.client.as_ref(), cx);
    if ui(w, cx).community != Some(community) {
        *ui(w, cx) = PanelUi {
            community: Some(community),
            ..PanelUi::default()
        };
    }
    if !ui(w, cx).installation_asked
        && let Some(client) = &ctx.client
    {
        ui(w, cx).installation_asked = true;
        drop(client.bot_admin().refresh_installation(community));
    }
    let state = ctx.state.clone();
    let access = state.roles.access_of(community);
    let measure = 720.0;
    let mut root = div()
        .id("settings-self-roles")
        .flex()
        .flex_col()
        .w(px(measure))
        .child(section_header(t("admin.bot.selfRoles.title"), None).pb(px(NO_GAP)));
    let gap = |first: bool| px(if first { AFTER_TITLE } else { SIBLING_GAP });
    let mut first = true;
    let Some(list) = state.roles.self_roles.get(&community).cloned() else {
        return root
            .child(
                div()
                    .mt(gap(true))
                    .child(body_line(t("admin.bot.loading"), color::FG_2)),
            )
            .into_any_element();
    };
    let busy = ui(w, cx).busy;
    let manages = access.loaded && access.manage_roles;
    let approved: Vec<RoleId> = list.iter().map(|r| r.id).collect();
    if manages {
        let sorted = sorted_roles(&state, community);
        let candidates: Vec<RoleSummary> = self_role_candidates(&sorted, &access)
            .into_iter()
            .cloned()
            .collect();
        let items = candidates
            .iter()
            .map(|r| {
                let (id, client, now) = (r.id, ctx.client.clone(), approved.clone());
                checkbox(
                    format!("self-role-{}", hex(&r.id)),
                    &r.name,
                    approved.contains(&r.id),
                    busy,
                    move |on, _w, cx| {
                        let mut next: Vec<RoleId> =
                            now.iter().copied().filter(|x| *x != id).collect();
                        if on {
                            next.push(id);
                        }
                        if let Some(client) = &client {
                            roles_job(w, client, cx, move |r| {
                                Box::pin(
                                    async move { r.configure_self_roles(community, &next).await },
                                )
                            });
                        }
                    },
                )
            })
            .collect();
        root = root.child(
            div()
                .mt(gap(first))
                .child(fieldset(t("admin.bot.selfRoles.configure"), items)),
        );
        first = false;
    }
    if !list.is_empty() {
        let items = list
            .iter()
            .map(|r| {
                let (id, client) = (r.id, ctx.client.clone());
                checkbox(
                    format!("self-pick-{}", hex(&r.id)),
                    &r.name,
                    r.self_selected,
                    busy,
                    move |on, _w, cx| {
                        if let Some(client) = &client {
                            roles_job(w, client, cx, move |r| {
                                Box::pin(async move { r.select_self_role(community, id, on).await })
                            });
                        }
                    },
                )
            })
            .collect();
        root = root.child(
            div()
                .mt(gap(first))
                .child(fieldset(t("admin.bot.selfRoles.select"), items)),
        );
        first = false;
    }
    if manages {
        root = root.child(
            div()
                .mt(gap(first))
                .child(panel_form(w, ctx, community, &list, window, cx)),
        );
        first = false;
    }
    // Right under the title, a paragraph is `h2 + p`: 4px.
    let p_gap = px(if first { 4.0 } else { 0.0 });
    let error = ui(w, cx).error.clone();
    if error.is_none() && list.is_empty() {
        // `.d-settings-frame__body > section > p`: `--text-ui` in `--fg-2`; app.css's later `.dw-settings-section p {
        // margin: 0 }` (the same specificity) takes the frame's 14px between siblings away.
        root = root.child(div().mt(p_gap).child(line(
            t("admin.bot.selfRoles.empty").to_owned(),
            text::UI,
            line_height(text::UI, NORMAL_LINE),
            color::FG_2,
        )));
    }
    if let Some(e) = error {
        root = root.child(div().mt(p_gap).child(body_line(e, color::DANGER)));
    }
    root.into_any_element()
}

/// The "Publish a role panel" form.
fn panel_form(
    w: WindowId,
    ctx: &SlotCx,
    community: CommunityId,
    list: &[dilla_client::state::roles::SelfRole],
    window: &mut Window,
    cx: &mut App,
) -> AnyElement {
    let state = &ctx.state;
    let enabled = panel_enabled(state, community);
    let channels = panel_channels(state, community);
    let width = 720.0;
    let mut form = div().flex().flex_col().child(
        line(
            t("admin.bot.rolePanel.title").to_owned(),
            px(f32::from(text::BASE) * H3_EM),
            line_height(px(f32::from(text::BASE) * H3_EM), NORMAL_LINE),
            color::FG,
        )
        .weight(font::WEIGHT_DISPLAY),
    );
    form = form.child(body_paragraph(t("admin.bot.rolePanel.help"), width));
    if !enabled {
        form = form.child(body_paragraph(t("admin.bot.rolePanel.requiresBot"), width));
    }
    if channels.is_empty() {
        form = form.child(body_paragraph(t("admin.bot.rolePanel.noChannels"), width));
    }
    let locked = !enabled || ui(w, cx).pending.is_some();
    for role in list {
        let input = ui(w, cx).emojis.get(&role.id).cloned();
        let input = match input {
            Some(i) => i,
            None => {
                let i = cx.new(|cx| InputState::new(window, cx));
                ui(w, cx).emojis.insert(role.id, i.clone());
                i
            }
        };
        let label = format!("{} {}", t("admin.bot.rolePanel.emoji"), role.name);
        form = form.child(
            SettingsRow::new(
                SharedString::from(format!("panel-emoji-{}", hex(&role.id))),
                label,
            )
            .child(
                div()
                    .w(px(EMOJI_FIELD_W))
                    .child(Input::new(&input).disabled(locked)),
            ),
        );
    }
    let chosen = ui(w, cx)
        .channel
        .filter(|c| channels.iter().any(|(id, _)| id == c));
    let options: Vec<(String, String)> = std::iter::once((
        String::new(),
        t("admin.bot.rolePanel.chooseChannel").to_owned(),
    ))
    .chain(channels.iter().map(|(id, name)| (id.hex(), name.clone())))
    .collect();
    let pairs: Vec<(&str, &str)> = options
        .iter()
        .map(|(v, l)| (v.as_str(), l.as_str()))
        .collect();
    let picked = chosen.map(|c| c.hex()).unwrap_or_default();
    let ids: Vec<ChannelId> = channels.iter().map(|(id, _)| *id).collect();
    form = form.child(
        SettingsRow::new("panel-channel", t("admin.bot.rolePanel.channel")).child(
            Select::new(
                "panel-channel-select",
                t("admin.bot.rolePanel.channel"),
                &pairs,
                picked,
            )
            .bare()
            .disabled(locked)
            .on_change(move |v, _w, cx| {
                ui(w, cx).channel = ids.iter().copied().find(|c| c.hex() == v);
                cx.refresh_windows();
            }),
        ),
    );
    let (busy, pending, notice) = {
        let u = ui(w, cx);
        (u.busy, u.pending.clone(), u.notice.clone())
    };
    let client = ctx.client.clone();
    let roles: Vec<(RoleId, String)> = list.iter().map(|r| (r.id, r.name.clone())).collect();
    let button = if pending.as_ref().is_some_and(|p| p.uncertain) {
        Button::new("panel-retry", t("admin.bot.rolePanel.retry"))
            .disabled(busy)
            .on_click(move |_, _w, cx| publish(w, community, client.clone(), &roles, true, cx))
    } else {
        Button::new(
            "panel-publish",
            if busy {
                t("admin.bot.rolePanel.publishing")
            } else {
                t("admin.bot.rolePanel.publish")
            },
        )
        .disabled(!enabled || pending.is_some() || busy)
        .on_click(move |_, _w, cx| publish(w, community, client.clone(), &roles, false, cx))
    };
    form = form.child(
        div()
            .flex()
            .child(crate::tracked_box("panel-publish", button)),
    );
    if let Some(n) = notice {
        form = form.child(body_line(n, color::FG_2));
    }
    form.into_any_element()
}

/// SelfRolesSection.tsx `publishPanel`: the roles with an emoji, into the chosen channel; a Retry resends the pending
/// request with its own id.
fn publish(
    w: WindowId,
    community: CommunityId,
    client: Option<Rc<Client>>,
    roles: &[(RoleId, String)],
    retry: bool,
    cx: &mut App,
) {
    let Some(client) = client else { return };
    if ui(w, cx).busy || (ui(w, cx).pending.is_some() && !retry) {
        return;
    }
    let current = match ui(w, cx).pending.clone() {
        Some(p) if retry => p,
        _ => {
            let options: Vec<PanelOption> = roles
                .iter()
                .filter_map(|(id, name)| {
                    let input = ui(w, cx).emojis.get(id)?.clone();
                    let emoji = input.read(cx).value().trim().to_owned();
                    (!emoji.is_empty()).then(|| PanelOption {
                        emoji,
                        role: *id,
                        name: name.clone(),
                    })
                })
                .collect();
            let Some(channel) = ui(w, cx).channel else {
                ui(w, cx).error = Some(t("admin.bot.rolePanel.invalid").to_owned());
                cx.refresh_windows();
                return;
            };
            if !panel_valid(&options) {
                ui(w, cx).error = Some(t("admin.bot.rolePanel.invalid").to_owned());
                cx.refresh_windows();
                return;
            }
            Pending {
                request_id: request_id(),
                channel,
                options,
                uncertain: false,
            }
        }
    };
    {
        let u = ui(w, cx);
        u.busy = true;
        u.error = None;
        u.pending = Some(Pending {
            uncertain: false,
            ..current.clone()
        });
        u.notice = Some(t("admin.bot.rolePanel.pending").to_owned());
    }
    cx.refresh_windows();
    let job = client.role_panels().publish_job(
        community,
        current.channel,
        current.options.clone(),
        current.request_id,
    );
    cx.spawn(async move |cx| {
        let answer = job.await;
        cx.update(|cx| {
            let u = ui(w, cx);
            if u.community != Some(community) {
                return;
            }
            u.busy = false;
            match answer {
                Ok(()) => {
                    u.pending = None;
                    u.notice = Some(t("admin.bot.rolePanel.published").to_owned());
                }
                Err(PanelError::Uncertain { .. }) => {
                    u.pending = Some(Pending {
                        uncertain: true,
                        ..current
                    });
                    u.notice = Some(t("admin.bot.rolePanel.uncertain").to_owned());
                }
                Err(PanelError::Invalid) => {
                    u.pending = None;
                    u.notice = None;
                    u.error = Some(t("admin.bot.rolePanel.invalid").to_owned());
                }
                Err(PanelError::Refused(code)) => {
                    u.pending = None;
                    u.notice = None;
                    u.error = Some(tf("admin.bot.rolePanel.error", &[("code", &code)]));
                }
            }
            cx.refresh_windows();
        });
    })
    .detach();
}
