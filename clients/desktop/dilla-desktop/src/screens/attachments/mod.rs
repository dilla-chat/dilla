//! Area **attachments** (Task 12): the attachment tray, cards, the image grid, the lightbox, the media player and the GIF picker.
//!
//! The hooks below are where `crate::slots` draws this area inside another area's view; their signatures are Task 1's.
//! The state and the work are in [`ui`]; decoding and playback in [`media`]; the components in
//! `dilla_desktop_ui::components::attachments`. The overlays (the lightbox, the GIF picker, the drop overlay) are drawn
//! from [`composer_buttons`], which every open conversation draws, as deferred elements over the window.

pub mod keys;
pub mod media;
pub mod ui;

use std::rc::Rc;

use dilla_client::features::attachments::{
    ATTACHMENT_CAP, AttachmentKind, GIF_AUTOPLAY_MAX, attachment_kind, safe_name,
};
use dilla_client::features::gifs::{GifScope, gif_scope};
use dilla_client::state::{TimelineItem, attachments::TrayPhase};
use dilla_desktop_ui::components::attachments::card::{
    self, FILE_W, GIF_W, IMAGE_W, STATE_GAP, file_card, file_card_height, image_tile, state_line,
    state_line_height, tile_height, too_large_image,
};
use dilla_desktop_ui::components::attachments::player::{MediaCardProps, PlayerState, media_card};
use dilla_desktop_ui::components::attachments::tray::{Preview, Step, TrayEntry};
use dilla_desktop_ui::components::attachments::{
    OnClick, ROW_GAP, drop_overlay, gif_picker, lightbox, tray as tray_view,
};
use dilla_desktop_ui::tokens::{color, layout, radius, text};
use dilla_desktop_ui::{Icon, IconName};
use gpui::{
    AnyElement, App, AppContext, ExternalPaths, Focusable, InteractiveElement, IntoElement,
    ParentElement, SharedString, StatefulInteractiveElement, Styled, Window, anchored, deferred,
    div, point, px,
};

use crate::slots::{Extra, SlotCx};
use crate::strings::{t, tf};
use ui::{CardKey, CardStatus, ImageKey, Player, Slot, TrayPreview};

/// timeline.rs's row geometry (`CONTENT_X`, `ROW_RIGHT`): the body column starts after the gutter, the avatar and their
/// gaps, and ends before the row's right padding. The cards wrap within it.
const BODY_INSET: f32 = 44.0 + 10.0 + 30.0 + 10.0 + 16.0;

/// The area's sizes, logical px, named after the packages/ui CSS they come from (R21: no literal `px(..)` in screens).
mod m {
    /// Composer.css `.d-composer__tool`: 26x24, 2px apart, the 14px glyph; the GIF tool's 6px side padding.
    pub const TOOL_H: f32 = 24.0;
    pub const TOOL_W: f32 = 26.0;
    pub const TOOL_GAP: f32 = 2.0;
    pub const TOOL_GLYPH: f32 = 14.0;
    pub const GIF_PAD_X: f32 = 6.0;
    /// The GIF-in-a-DM notice: 12px in, 8px between its rows and its buttons.
    pub const NOTICE_PAD: f32 = 12.0;
    pub const NOTICE_GAP: f32 = 8.0;
    /// The GIF picker sits 8px over the composer.
    pub const PICKER_LIFT: f32 = 8.0;
    /// GifPicker.css search field: 30px tall, 10px in.
    pub const SEARCH_H: f32 = 30.0;
    pub const SEARCH_PAD_X: f32 = 10.0;
}

fn click(f: impl Fn(&mut Window, &mut App) + 'static) -> OnClick {
    Rc::new(move |_, w, cx| f(w, cx))
}

/// The width the cards wrap in: the log's (the window less the rail and the sidebar) less the row's insets.
fn body_width(window: &Window) -> f32 {
    f32::from(window.viewport_size().width - layout::RAIL - layout::SIDEBAR) - BODY_INSET
}

/// One card and its box.
struct Card {
    element: Box<dyn FnOnce(f32) -> AnyElement>,
    width: f32,
    /// Its own height; `None` for a file card, which stretches to its line (`height: 100%` in a wrapping flex row).
    height: Option<f32>,
}

/// Lays `cards` out as app.css `.dw-attachments` does (a wrapping row, `--row-gap` both ways) in `width`: answers the
/// lines' elements and the total height.
fn wrap(cards: Vec<Card>, width: f32) -> (AnyElement, f32) {
    let mut lines: Vec<Vec<Card>> = Vec::new();
    let mut used = 0.0;
    for c in cards {
        let fits = lines
            .last()
            .is_some_and(|l| !l.is_empty() && used + ROW_GAP + c.width <= width);
        if fits {
            used += ROW_GAP + c.width;
            lines.last_mut().expect("a line").push(c);
        } else {
            used = c.width;
            lines.push(vec![c]);
        }
    }
    let mut total = 0.0;
    let mut column = div().flex().flex_col().gap(px(ROW_GAP));
    for (i, line) in lines.into_iter().enumerate() {
        let h = line
            .iter()
            .map(|c| c.height.unwrap_or_else(file_card_height))
            .fold(0.0, f32::max);
        total += h + if i > 0 { ROW_GAP } else { 0.0 };
        column = column.child(
            div()
                .flex()
                .gap(px(ROW_GAP))
                .h(px(h))
                .children(line.into_iter().map(|c| (c.element)(h))),
        );
    }
    (column.into_any_element(), total)
}

/// A card with its state line under it.
fn with_state(element: AnyElement, width: f32, height: f32, state: Option<(String, bool)>) -> Card {
    match state {
        None => Card {
            element: Box::new(move |_| element),
            width,
            height: Some(height),
        },
        Some((text_, failed)) => Card {
            element: Box::new(move |_| {
                div()
                    .flex()
                    .flex_col()
                    .gap(px(STATE_GAP))
                    .child(element)
                    .child(state_line(text_, failed))
                    .into_any_element()
            }),
            width,
            height: Some(height + STATE_GAP + state_line_height()),
        },
    }
}

/// A message's attachment cards and image grid (`slots::message_extras`, after the reply chip).
pub fn attachment_cards(
    item: &TimelineItem,
    ctx: &SlotCx,
    window: &mut Window,
    cx: &mut App,
) -> Option<Extra> {
    if item.attachments.is_empty() {
        return None;
    }
    let channel = ctx.channel?;
    let live = ctx.client.is_some();
    // Review M2: what this window's client opened, never another account's of the same channel.
    let owner = ctx.client.as_deref().map_or(0, ui::owner_of);
    let seq = item.seq;
    let images: Vec<(u32, String, u64)> = item
        .attachments
        .iter()
        .filter(|a| attachment_kind(&a.mime) == AttachmentKind::Image && a.size <= ATTACHMENT_CAP)
        .map(|a| {
            (
                a.index,
                safe_name(&a.name, t("shell.attachment.unnamed")),
                a.size,
            )
        })
        .collect();
    let mut cards = Vec::new();
    for a in &item.attachments {
        let name = safe_name(&a.name, t("shell.attachment.unnamed"));
        let size = ui::format_size(a.size);
        let too_large = a.size > ATTACHMENT_CAP;
        let kind = attachment_kind(&a.mime);
        let key = seq.map(|seq| CardKey {
            client: owner,
            channel,
            seq,
            index: a.index,
        });
        let status = key.and_then(|k| ui::ui(cx).cards.get(&k).cloned());
        let state_text = match (&status, too_large) {
            (_, true) => Some((t("shell.attachment.tooLarge").to_owned(), false)),
            (Some(CardStatus::Loading), _) => {
                Some((t("shell.attachment.opening").to_owned(), false))
            }
            (Some(CardStatus::Failed(code)), _) => {
                Some((tf("shell.attachment.failed", &[("code", code)]), true))
            }
            (None, false) => None,
        };
        let id = format!("attachment-{}-{}", item.key, a.index);
        match kind {
            AttachmentKind::Image if too_large => {
                let (el, h) = too_large_image(name, size);
                cards.push(with_state(el, IMAGE_W, h, state_text));
            }
            AttachmentKind::Image => {
                let gif = a.mime == "image/gif";
                // A GIF plays in its card up to 8 MB (the whole file, GPUI animates it); else and until then, the still.
                let full = key
                    .filter(|_| gif && a.size <= GIF_AUTOPLAY_MAX && live)
                    .map(|k| ImageKey {
                        client: owner,
                        channel,
                        seq: k.seq,
                        index: k.index,
                        thumb: false,
                    });
                let thumb_key = key.map(|k| ImageKey {
                    client: owner,
                    channel,
                    seq: k.seq,
                    index: k.index,
                    thumb: true,
                });
                if let Some(c) = &ctx.client {
                    for k in thumb_key.into_iter().chain(full) {
                        ui::ensure_image(c, k, cx);
                    }
                }
                let ready = |k: Option<ImageKey>, cx: &mut App| match k
                    .and_then(|k| ui::ui(cx).images.get(&k).cloned())
                {
                    Some(Slot::Ready(i)) => Some(i),
                    _ => None,
                };
                let moving = ready(full, cx);
                let fixture = (!live)
                    .then(|| {
                        ctx.state
                            .attachments
                            .fixture_thumbs
                            .get(&(a.name.clone(), a.size))
                    })
                    .flatten()
                    .and_then(|b| ui::picture(b));
                let shown = moving.clone().or_else(|| ready(thumb_key, cx)).or(fixture);
                let badge = (gif && moving.is_none())
                    .then(|| SharedString::from(t("shell.attachment.gif")));
                let w = if gif { GIF_W } else { IMAGE_W };
                let open = key.map(|k| {
                    let images = images.clone();
                    let client = ctx.client.clone();
                    click(move |w, cx| {
                        if let Some(c) = &client {
                            ui::open_lightbox(
                                c.clone(),
                                k.channel,
                                k.seq,
                                images.clone(),
                                k.index,
                                w,
                                cx,
                            )
                        }
                    })
                });
                let el = image_tile(id, w, shown, badge, open);
                cards.push(with_state(el, w, tile_height(w), state_text));
            }
            AttachmentKind::Audio | AttachmentKind::Video if !too_large && key.is_some() => {
                let k = key.expect("checked");
                // A fixture (no client) draws the card; its buttons do nothing.
                let client = ctx.client.clone();
                let saver = client.clone();
                let video = kind == AttachmentKind::Video;
                let state = match ui::ui(cx).players.get(&k) {
                    None => PlayerState::Idle,
                    Some(Player::Loading) => PlayerState::Loading,
                    Some(Player::Error) => PlayerState::Error,
                    Some(Player::Ready(p)) => PlayerState::Ready {
                        playing: !p.is_paused() && !p.ended(),
                        position: p.position(),
                        duration: p.duration(),
                    },
                };
                let mime = a.mime.clone();
                let (el, h) = media_card(MediaCardProps {
                    id: id.into(),
                    video,
                    name: name.clone().into(),
                    size: size.into(),
                    state,
                    play_label: t("media.play").replace("{name}", &name).into(),
                    loading_label: t("media.loading").into(),
                    error_label: t("media.error").into(),
                    save_text: t("media.save").into(),
                    on_play: click(move |_, cx| {
                        if let Some(c) = &client {
                            ui::play(c, k, video, mime.clone(), cx)
                        }
                    }),
                    on_toggle: click(move |_, cx| ui::toggle(k, cx)),
                    on_save: Some(click(move |_, cx| {
                        if let Some(c) = &saver {
                            ui::save(c, k, cx)
                        }
                    })),
                });
                cards.push(Card {
                    element: Box::new(move |_| el),
                    width: dilla_desktop_ui::components::attachments::player::MEDIA_W,
                    height: Some(h),
                });
            }
            _ => {
                let meta = card::file_meta(&name, &size);
                let save = key.filter(|_| !too_large).map(|k| {
                    let client = ctx.client.clone();
                    click(move |_, cx| {
                        if let Some(c) = &client {
                            ui::save(c, k, cx)
                        }
                    })
                });
                let state = state_text.clone();
                let element: Box<dyn FnOnce(f32) -> AnyElement> = Box::new(move |line_h| {
                    let own = line_h
                        - state
                            .as_ref()
                            .map_or(0.0, |_| STATE_GAP + state_line_height());
                    let el = file_card(id, name, meta, save, own.max(file_card_height()));
                    match state {
                        None => el,
                        Some((s, failed)) => div()
                            .flex()
                            .flex_col()
                            .gap(px(STATE_GAP))
                            .child(el)
                            .child(state_line(s, failed))
                            .into_any_element(),
                    }
                });
                let height =
                    state_text.map(|_| file_card_height() + STATE_GAP + state_line_height());
                cards.push(Card {
                    element,
                    width: FILE_W,
                    height,
                });
            }
        }
    }
    let (element, height) = wrap(cards, body_width(window));
    Some(Extra { element, height })
}

/// The attachment tray over the composer (`slots::composer_leading`, after the reply bar).
pub fn tray(ctx: &SlotCx, _window: &mut Window, cx: &mut App) -> Option<AnyElement> {
    let channel = ctx.channel?;
    let client = ctx.client.clone();
    let items = ctx.state.attachments.tray(channel).to_vec();
    let entries = items
        .into_iter()
        .map(|item| {
            let step = match item.phase {
                TrayPhase::Reading => Step::Reading,
                TrayPhase::Preparing => Step::Preparing,
                TrayPhase::Uploading => Step::Uploading,
                TrayPhase::Ready => Step::Ready,
                TrayPhase::Failed => Step::Failed,
            };
            let phase = match item.phase {
                TrayPhase::Failed if item.reason == "E_ATTACHMENT_TOO_LARGE" => {
                    t("shell.tray.overLimit").to_owned()
                }
                TrayPhase::Failed => tf("shell.tray.failed", &[("code", &item.reason)]),
                p => t(&format!("shell.tray.{}", p.as_str())).to_owned(),
            };
            let owner = client.as_deref().map_or(0, ui::owner_of);
            let preview = match ui::ui(cx)
                .tray_previews
                .get(&(owner, channel, item.id.clone()))
            {
                Some(TrayPreview::Image(i)) => Preview::Image(i.clone()),
                Some(TrayPreview::Gif) => Preview::Gif,
                None => Preview::None,
            };
            let id = item.id.clone();
            let client = client.clone();
            TrayEntry {
                id: item.id.clone().into(),
                name: item.name.clone().into(),
                size: ui::format_size(item.size).into(),
                phase: phase.into(),
                step,
                preview,
                remove_label: tf("shell.tray.remove", &[("name", &item.name)]).into(),
                on_remove: click(move |_, cx| {
                    if let Some(c) = &client {
                        ui::discard(c, channel, id.clone(), cx)
                    }
                }),
            }
        })
        .collect();
    tray_view::tray(entries)
}

/// A 26x24 tool of the composer's foot (`.d-composer__tool`).
fn tool(id: &'static str, label: &'static str, f: OnClick) -> gpui::Stateful<gpui::Div> {
    div()
        .id(id)
        .h(px(m::TOOL_H))
        .min_w(px(m::TOOL_W))
        .flex_none()
        .flex()
        .items_center()
        .justify_center()
        .rounded(radius::SM)
        .text_color(color::FG_3)
        .cursor_pointer()
        .hover(|s| s.bg(color::SURFACE_HI).text_color(color::FG))
        .tooltip(dilla_desktop_ui::components::tooltip::tooltip(
            t(label),
            &[],
        ))
        .on_click(move |e, w, cx| f(e, w, cx))
}

/// The composer's attach and GIF buttons (`slots::composer_buttons`, first), and the area's overlays.
pub fn composer_buttons(ctx: &SlotCx, window: &mut Window, cx: &mut App) -> Option<AnyElement> {
    ui::track(window, ctx.client.clone(), ctx.channel, cx);
    let mut row = div().flex().items_center().gap(px(m::TOOL_GAP)).child(
        tool(
            "composer-attach",
            "shell.composer.attach",
            click(|w, cx| ui::pick_files(w, cx)),
        )
        .child(
            Icon::new(IconName::Paperclip)
                .size(px(m::TOOL_GLYPH))
                .color(color::FG_3),
        ),
    );
    // use-gif-community.ts: a server channel's own server; a 1:1 DM's shared servers, found only on the press; never
    // a thread. `SlotCx` does not say yet whether it draws a thread or a DM's peer (the controller's merge item in
    // Task 12's report): until it does, a DM and a thread pass `None` and `false` here and offer no GIF button.
    let scope = gif_scope(ctx.community, None, false);
    let action: Option<OnClick> = match (scope, ctx.channel, ctx.client.clone()) {
        (GifScope::Server(community), Some(channel), Some(client))
            if ui::gif_on(&client, community, cx) =>
        {
            Some(click(move |w, cx| {
                let owner = ui::window_id(w);
                ui::toggle_gifs(client.clone(), owner, channel, community, cx)
            }))
        }
        (GifScope::Direct(peer), Some(channel), Some(client)) => Some(click(move |w, cx| {
            let owner = ui::window_id(w);
            ui::open_dm_gifs(client.clone(), owner, channel, peer, cx)
        })),
        _ => None,
    };
    if let Some(action) = action {
        let here = ui::window_id(window);
        let open = ui::ui(cx)
            .gif
            .as_ref()
            .is_some_and(|g| g.owner == Some(here));
        row = row.child(
            tool("composer-gif", "gif.button", action)
                .px(px(m::GIF_PAD_X))
                .when_open(open)
                .child(
                    dilla_desktop_ui::text::line(
                        "GIF",
                        text::BTN,
                        18.0,
                        if open { color::ACCENT } else { color::FG_3 },
                    )
                    .weight(dilla_desktop_ui::tokens::font::WEIGHT_LABEL),
                ),
        );
    }
    for overlay in overlays(window, cx) {
        row = row.child(overlay);
    }
    Some(row.into_any_element())
}

trait OpenTool {
    fn when_open(self, open: bool) -> Self;
}

impl OpenTool for gpui::Stateful<gpui::Div> {
    fn when_open(self, open: bool) -> Self {
        if open {
            self.bg(color::ACCENT_SOFT)
        } else {
            self
        }
    }
}

/// The lightbox, the GIF picker and the drop overlay, each over everything else.
fn overlays(window: &mut Window, cx: &mut App) -> Vec<AnyElement> {
    let mut out = Vec::new();
    let viewport = window.viewport_size();
    let here = ui::window_id(window);
    // Files dragged over the window: the overlay over the main pane, which takes the drop into this window's channel.
    let to = ui::target(window, cx);
    if cx.has_active_drag() && to.channel.is_some() && to.client.is_some() {
        let left = layout::RAIL + layout::SIDEBAR;
        let top = layout::TOPBAR;
        let pane = div()
            .id("attachments-drop")
            .relative()
            .w(viewport.width - left)
            .h(viewport.height - top)
            .on_drop(move |paths: &ExternalPaths, _, cx| {
                let paths = paths.paths().to_vec();
                let to = to.clone();
                cx.spawn(async move |cx| ui::attach_paths(to, paths, cx).await)
                    .detach();
            })
            .child(drop_overlay::drop_overlay(
                t("shell.drop.title"),
                t("shell.drop.body"),
            ));
        out.push(
            deferred(anchored().position(point(left, top)).child(pane))
                .with_priority(1)
                .into_any_element(),
        );
    }
    if let Some(element) = lightbox_overlay(here, cx) {
        let full = div()
            .w(viewport.width)
            .h(viewport.height)
            .relative()
            .child(element);
        out.push(
            deferred(anchored().position(gpui::Point::default()).child(full))
                .with_priority(3)
                .into_any_element(),
        );
    }
    if let Some(n) = ui::ui(cx).dm_notice.clone().filter(|n| n.owner == here) {
        let notice = div()
            .id("gif-dm-notice")
            .flex()
            .flex_col()
            .gap(px(m::NOTICE_GAP))
            .w(px(gif_picker::WIDTH))
            .p(px(m::NOTICE_PAD))
            .bg(color::SURFACE_2)
            .border_1()
            .border_color(color::LINE_STRONG)
            .rounded(radius::XL)
            .occlude()
            .child(dilla_desktop_ui::text::line(
                t("gif.dm.title"),
                text::MD,
                21.0,
                color::FG,
            ))
            .child(
                div()
                    .text_size(text::SM)
                    .text_color(color::FG_2)
                    .child(tf("gif.dm.notice", &[("server", &n.server)])),
            )
            .child(
                div()
                    .flex()
                    .gap(px(m::NOTICE_GAP))
                    .child(
                        dilla_desktop_ui::components::button::Button::new(
                            "gif-dm-use",
                            t("gif.dm.use"),
                        )
                        .variant(dilla_desktop_ui::components::button::ButtonVariant::Accent)
                        .on_click(|_, _, cx| ui::accept_dm_notice(cx)),
                    )
                    .child(
                        dilla_desktop_ui::components::button::Button::new(
                            "gif-dm-not-now",
                            t("gif.dm.notNow"),
                        )
                        .variant(dilla_desktop_ui::components::button::ButtonVariant::Ghost)
                        .on_click(|_, _, cx| ui::dismiss_dm_notice(cx)),
                    ),
            );
        out.push(
            deferred(anchored().snap_to_window().child(notice))
                .with_priority(2)
                .into_any_element(),
        );
    }
    if let Some(picker) = gif_overlay(here, window, cx) {
        out.push(
            deferred(
                anchored()
                    .snap_to_window()
                    .child(div().mb(px(m::PICKER_LIFT)).child(picker)),
            )
            .with_priority(2)
            .into_any_element(),
        );
    }
    out
}

fn lightbox_overlay(here: gpui::WindowId, cx: &mut App) -> Option<AnyElement> {
    let l = ui::ui(cx).lightbox.as_ref().filter(|l| l.owner == here)?;
    let (channel, seq, at, count) = (l.channel, l.seq, l.at, l.images.len());
    let client = l.client.clone();
    let (index, name, size) = l.images.get(at)?.clone();
    let focus = l.close_focus.clone();
    let key = ImageKey {
        client: ui::owner_of(&client),
        channel,
        seq,
        index,
        thumb: false,
    };
    ui::ensure_image(&client, key, cx);
    let image = match ui::ui(cx).images.get(&key) {
        Some(Slot::Ready(i)) => Some(i.clone()),
        _ => None,
    };
    let card = CardKey {
        client: ui::owner_of(&client),
        channel,
        seq,
        index,
    };
    Some(lightbox::lightbox(lightbox::LightboxProps {
        label: tf(
            "shell.lightbox.label",
            &[("name", &name), ("size", &ui::format_size(size))],
        )
        .into(),
        image,
        save_label: t("shell.lightbox.save").into(),
        on_save: click(move |_, cx| ui::save(&client, card, cx)),
        close_label: t("shell.lightbox.close").into(),
        on_close: click(|_, cx| ui::close_lightbox(cx)),
        close_focus: focus,
        previous: (at > 0).then(|| {
            (
                t("shell.lightbox.previous").into(),
                click(|_, cx| ui::step_lightbox(-1, cx)),
            )
        }),
        next: (at + 1 < count).then(|| {
            (
                t("shell.lightbox.next").into(),
                click(|_, cx| ui::step_lightbox(1, cx)),
            )
        }),
    }))
}

fn gif_overlay(here: gpui::WindowId, window: &mut Window, cx: &mut App) -> Option<AnyElement> {
    ui::ui(cx).gif.as_ref().filter(|g| g.owner == Some(here))?;
    if ui::ui(cx).gif.as_ref().is_some_and(|g| g.search.is_none()) {
        let input = cx.new(|cx| {
            gpui_component::input::InputState::new(window, cx)
                .placeholder(t("gif.search.placeholder"))
        });
        let focus = input.read(cx).focus_handle(cx);
        window.focus(&focus, cx);
        if let Some(g) = ui::ui(cx).gif.as_mut() {
            g.search = Some(input);
        }
    }
    let input = ui::ui(cx).gif.as_ref()?.search.clone()?;
    let typed = input.read(cx).value().to_string();
    let (query, loading) = ui::ui(cx)
        .gif
        .as_ref()
        .map(|g| (g.query.clone(), g.loading))?;
    if typed != query && !loading {
        ui::search_gifs(typed, false, cx);
    }
    let g = ui::ui(cx).gif.as_ref()?;
    let tiles = g
        .items
        .iter()
        .enumerate()
        .map(|(i, item)| gif_picker::GifTile {
            id: item.id.clone().into(),
            width: item.width,
            height: item.height,
            image: g.previews.get(&item.id).cloned(),
            on_pick: click(move |_, cx| ui::pick_gif(i, cx)),
        })
        .collect();
    let status = if g.loading {
        Some((t("gif.loading").into(), false))
    } else if g.error {
        Some((t("gif.error").into(), true))
    } else if g.items.is_empty() {
        Some((t("gif.empty").into(), false))
    } else {
        None
    };
    let more = g.next.map(|_| {
        (
            SharedString::from(t("gif.more")),
            click(|_, cx| {
                let q = ui::ui(cx)
                    .gif
                    .as_ref()
                    .map(|g| g.query.clone())
                    .unwrap_or_default();
                ui::search_gifs(q, true, cx);
            }),
        )
    });
    Some(gif_picker::gif_picker(gif_picker::GifPickerProps {
        search: div()
            .h(px(m::SEARCH_H))
            .px(px(m::SEARCH_PAD_X))
            .flex()
            .items_center()
            .bg(color::BG_2)
            .border_1()
            .border_color(color::HAIRLINE_2)
            .rounded(radius::MD)
            .text_size(text::INPUT)
            .child(gpui_component::input::Input::new(&input).appearance(false))
            .into_any_element(),
        powered: t("gif.powered").into(),
        tiles,
        active: g.active,
        picking: g.picking.clone().map(Into::into),
        status,
        more,
    }))
}

/// The DM GIF privacy setting (settings/DmGifPrivacySetting.tsx), drawn inside the Privacy section. Task 13's Privacy
/// section calls it; Task 12 implements it. Fixed signature: change it only with the controller.
pub fn dm_gif_privacy(ctx: &SlotCx, _window: &mut Window, _cx: &mut App) -> Option<AnyElement> {
    let client = ctx.client.clone()?;
    let on = client.gifs().dm_consent();
    let toggle_client = client.clone();
    Some(
        dilla_desktop_ui::components::settings::row::SettingsRow::new(
            "privacy-dm-gifs",
            t("gif.dm.title"),
        )
        .icon(IconName::Image)
        .hint(t("gif.dm.hint"))
        .child(
            dilla_desktop_ui::components::toggle::Toggle::new(
                "privacy-dm-gifs-toggle",
                t("gif.dm.title"),
                on,
            )
            .fit(dilla_desktop_ui::components::toggle::Fit::Row)
            .on_change(move |v, _, cx| {
                // DmGifPrivacySetting.tsx: enabling here is "on", which still asks with the server named on next use.
                if toggle_client
                    .gifs()
                    .set_dm_consent(v.then_some("on"))
                    .is_err()
                {
                    dilla_desktop_ui::components::toast::show_toast(
                        t("workspace.preference.error"),
                        cx,
                    );
                }
                cx.refresh_windows();
            }),
        )
        .into_any_element(),
    )
}

/// Whether a tray file is still working (the composer's send waits: `shell.tray.notReady`).
pub fn tray_busy(ctx: &SlotCx) -> bool {
    ctx.channel.is_some_and(|c| {
        ctx.state
            .attachments
            .tray(c)
            .iter()
            .any(|e| e.phase != TrayPhase::Ready)
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    fn fixed(width: f32, height: f32) -> Card {
        Card {
            element: Box::new(|_| div().into_any_element()),
            width,
            height: Some(height),
        }
    }

    #[test]
    fn cards_wrap_like_a_flex_row_with_the_row_gap() {
        // An image and a file card fit one 870px line; the file card stretches to the image's 210px.
        let (_, h) = wrap(
            vec![
                fixed(IMAGE_W, 210.0),
                Card {
                    element: Box::new(|_| div().into_any_element()),
                    width: FILE_W,
                    height: None,
                },
            ],
            870.0,
        );
        assert_eq!(h, 210.0);
        // Nine images at 870px: three a line (3 * 280 + 2 * 2 = 844), three lines.
        let (_, h) = wrap((0..9).map(|_| fixed(IMAGE_W, 210.0)).collect(), 870.0);
        assert_eq!(h, 3.0 * 210.0 + 2.0 * ROW_GAP);
        // A file card alone is its own height.
        let (_, h) = wrap(
            vec![Card {
                element: Box::new(|_| div().into_any_element()),
                width: FILE_W,
                height: None,
            }],
            870.0,
        );
        assert_eq!(h, file_card_height());
    }
}
