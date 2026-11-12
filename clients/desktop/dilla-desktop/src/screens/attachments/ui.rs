//! The attachments area's view state (one [`AttachmentsUi`] global per app) and its actions: the trays (one per
//! channel, as the worker keeps `trays`), the opened thumbnails and images, the cards' open and save states, the
//! lightbox, the players, the GIF picker and whether files are dragged over the window.
//!
//! Decrypted bytes live only here, in memory, as the decoded images GPUI draws and the players' buffers; they go when
//! the card, the lightbox or the player lets them go, and with the app. Nothing is written to disk except a file the
//! person saves (after their choice of path) and a video handed to the system player (`media::write_for_system_player`,
//! in the instance's own directory, wiped at start and exit).

use std::collections::HashMap;
use std::path::PathBuf;
use std::rc::Rc;
use std::sync::Arc;
use std::time::Duration;

use dilla_client::Client;
use dilla_client::features::attachments::{
    ATTACHMENT_CAP, AttachmentKind, MAX_ATTACHMENTS, NewFile, Tray, attachment_kind, safe_name,
};
use dilla_client::features::gifs::{GifItem, GifVariant};
use dilla_client::state::{ChannelId, CommunityId};
use gpui::{App, FocusHandle, Global, Image, ImageFormat, Window};
use zeroize::Zeroizing;

use super::media::{self, MediaDecoder, Playback};
use crate::strings::{t, tf};

/// Which opened picture: a message's attachment, its thumbnail or the whole file.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash)]
pub struct ImageKey {
    pub channel: ChannelId,
    pub seq: u64,
    pub index: u32,
    pub thumb: bool,
}

/// A card's attachment.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash)]
pub struct CardKey {
    pub channel: ChannelId,
    pub seq: u64,
    pub index: u32,
}

#[derive(Clone)]
pub enum Slot {
    Loading,
    Ready(Arc<Image>),
    Failed,
}

/// A card's open or save in flight, or its failure.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum CardStatus {
    Loading,
    Failed(String),
}

/// A media card's player (Attachments.tsx `playState` and the media element).
pub enum Player {
    Loading,
    Ready(Playback),
    Error,
}

/// The lightbox (Attachments.tsx `LightboxState`): the images of one message and the one shown.
pub struct LightboxState {
    pub channel: ChannelId,
    pub seq: u64,
    /// `(index, name, size)` of each image of the message, in order.
    pub images: Vec<(u32, String, u64)>,
    pub at: usize,
    pub close_focus: FocusHandle,
}

/// The GIF picker (GifPicker.tsx's state).
#[derive(Default)]
pub struct GifState {
    pub channel: Option<ChannelId>,
    pub community: Option<CommunityId>,
    pub query: String,
    pub items: Vec<GifItem>,
    pub previews: HashMap<String, Arc<Image>>,
    pub next: Option<u32>,
    pub loading: bool,
    pub error: bool,
    pub active: Option<usize>,
    pub picking: Option<String>,
    /// Bumped on every search, so a late answer is dropped.
    pub turn: u64,
    /// The search field, made when the picker first draws.
    pub search: Option<gpui::Entity<gpui_component::input::InputState>>,
}

/// A tray entry's picture, kept by its tray id (Attachments.tsx `trayPreviews`).
#[derive(Clone)]
pub enum TrayPreview {
    Image(Arc<Image>),
    Gif,
}

#[derive(Default)]
pub struct AttachmentsUi {
    pub trays: HashMap<ChannelId, Tray>,
    pub tray_previews: HashMap<(ChannelId, String), TrayPreview>,
    pub images: HashMap<ImageKey, Slot>,
    pub cards: HashMap<CardKey, CardStatus>,
    pub players: HashMap<CardKey, Player>,
    pub lightbox: Option<LightboxState>,
    pub gif: Option<GifState>,
    /// `gifStatus` per server: `None` while asked.
    pub gif_on: HashMap<CommunityId, Option<bool>>,
    /// The channel the composer last drew for: where a paste or a drop goes.
    pub channel: Option<ChannelId>,
    pub client: Option<Rc<Client>>,
    /// The instance's data directory (from its prefs file): where a video for the system player goes.
    pub instance_dir: Option<PathBuf>,
}

impl Global for AttachmentsUi {}

/// The global, made on first use.
pub fn ui(cx: &mut App) -> &mut AttachmentsUi {
    cx.default_global::<AttachmentsUi>()
}

/// shell-model.ts `formatSize`.
pub fn format_size(bytes: u64) -> String {
    if bytes < 1024 {
        return tf("shell.attachment.bytes", &[("n", &bytes.to_string())]);
    }
    let mb = bytes >= 1_048_576;
    let value = bytes as f64 / if mb { 1_048_576.0 } else { 1024.0 };
    // JS `toFixed(1)` under 10, else `Math.round` (half up).
    let n = if value < 10.0 {
        format!("{value:.1}")
    } else {
        format!("{}", (value + 0.5).floor())
    };
    tf(
        if mb {
            "shell.attachment.mb"
        } else {
            "shell.attachment.kb"
        },
        &[("n", &n)],
    )
}

/// The format GPUI decodes a picture as, by its signature; `None` for anything that is not one of the four image
/// types drawn inline (an SVG never is).
pub fn image_format(b: &[u8]) -> Option<ImageFormat> {
    if b.starts_with(b"\x89PNG\r\n\x1a\n") {
        Some(ImageFormat::Png)
    } else if b.starts_with(&[0xff, 0xd8, 0xff]) {
        Some(ImageFormat::Jpeg)
    } else if b.len() >= 12 && &b[0..4] == b"RIFF" && &b[8..12] == b"WEBP" {
        Some(ImageFormat::Webp)
    } else if b.starts_with(b"GIF87a") || b.starts_with(b"GIF89a") {
        Some(ImageFormat::Gif)
    } else {
        None
    }
}

/// A picture GPUI may decode: one of the four inline types whose headers say all its frames fit `DECODE_BUDGET` (a
/// decompression bomb is never handed over; the card keeps its still or its neutral box).
pub(crate) fn picture(bytes: &[u8]) -> Option<Arc<Image>> {
    if !dilla_client::features::attachments::fits_decode_budget(bytes) {
        return None;
    }
    image_format(bytes).map(|f| Arc::new(Image::from_bytes(f, bytes.to_vec())))
}

/// Remembers the client and the channel the composer draws for.
pub fn track(client: Option<Rc<Client>>, channel: Option<ChannelId>, cx: &mut App) {
    let u = ui(cx);
    if let Some(c) = &client
        && u.instance_dir.is_none()
    {
        let dir = c.prefs().path().parent().map(PathBuf::from);
        if let Some(d) = &dir {
            media::wipe_open_dir(d);
        }
        u.instance_dir = dir;
    }
    u.client = client;
    u.channel = channel;
}

/// The tray of `channel`, made on first use.
pub fn tray_of(client: &Client, channel: ChannelId, cx: &mut App) -> Tray {
    ui(cx)
        .trays
        .entry(channel)
        .or_insert_with(|| client.attachments().tray(channel))
        .clone()
}

/// Attachments.tsx `attachFiles`: the files join the tray (a toast for a file over 25 MB, the tray's own refusal of
/// an eleventh file), each picture with its preview.
pub fn attach(files: Vec<NewFile>, previews: Vec<Option<TrayPreview>>, cx: &mut App) {
    let (Some(client), Some(channel)) = (ui(cx).client.clone(), ui(cx).channel) else {
        return;
    };
    if files.is_empty() {
        return;
    }
    let tray = tray_of(&client, channel, cx);
    let over = files.iter().any(|f| f.size > ATTACHMENT_CAP);
    if over && tray.entries().len() + files.len() <= MAX_ATTACHMENTS {
        dilla_desktop_ui::components::toast::show_toast(t("shell.tray.overLimitToast"), cx);
    }
    match client.attachments().attach(&tray, files) {
        Ok(ids) => {
            let u = ui(cx);
            for (id, p) in ids.into_iter().zip(previews) {
                if let Some(p) = p {
                    u.tray_previews.insert((channel, id), p);
                }
            }
        }
        Err(e) => {
            let text = if e.code() == "E_ATTACHMENT_COUNT" {
                t("shell.tray.tooMany").to_owned()
            } else {
                tf("shell.banner.commandError", &[("code", &e.code())])
            };
            dilla_desktop_ui::components::toast::show_toast(text, cx);
        }
    }
    cx.refresh_windows();
}

/// A file from disk, with its preview when it is a still picture (a GIF gets the GIF card).
pub fn file_from_path(path: &std::path::Path) -> Option<(NewFile, Option<TrayPreview>)> {
    let file = NewFile::from_path(path).ok()?;
    let mime = dilla_client::features::attachments::mime_of(&file.name);
    let preview = match mime {
        "image/gif" => Some(TrayPreview::Gif),
        m if attachment_kind(m) == AttachmentKind::Image && file.size <= ATTACHMENT_CAP => {
            std::fs::read(path)
                .ok()
                .map(Zeroizing::new)
                .and_then(|b| picture(&b))
                .map(TrayPreview::Image)
        }
        _ => None,
    };
    Some((file, preview))
}

/// Pasted pictures (Ctrl+V): each becomes a PNG-named file with its preview.
pub fn attach_images(images: Vec<Image>, cx: &mut App) {
    let mut files = Vec::new();
    let mut previews = Vec::new();
    for (i, image) in images.into_iter().enumerate() {
        let (ext, mime) = match image.format {
            ImageFormat::Png => ("png", "image/png"),
            ImageFormat::Jpeg => ("jpg", "image/jpeg"),
            ImageFormat::Webp => ("webp", "image/webp"),
            ImageFormat::Gif => ("gif", "image/gif"),
            _ => continue,
        };
        let name = if i == 0 {
            format!("image.{ext}")
        } else {
            format!("image-{}.{ext}", i + 1)
        };
        previews.push(if mime == "image/gif" {
            Some(TrayPreview::Gif)
        } else {
            picture(&image.bytes).map(TrayPreview::Image)
        });
        files.push(NewFile::from_bytes(&name, mime, image.bytes));
    }
    attach(files, previews, cx);
}

/// Opens the person's file dialog and attaches what they pick (the paperclip).
pub fn pick_files(cx: &mut App) {
    let rx = cx.prompt_for_paths(gpui::PathPromptOptions {
        files: true,
        directories: false,
        multiple: true,
        prompt: None,
    });
    cx.spawn(async move |cx| {
        let Ok(Ok(Some(paths))) = rx.await else {
            return;
        };
        let picked: Vec<_> = paths.iter().filter_map(|p| file_from_path(p)).collect();
        cx.update(|cx| {
            let (files, previews) = picked.into_iter().unzip();
            attach(files, previews, cx);
        });
    })
    .detach();
}

/// Removes a tray entry (and its preview).
pub fn discard(channel: ChannelId, id: String, cx: &mut App) {
    let Some(client) = ui(cx).client.clone() else {
        return;
    };
    let tray = tray_of(&client, channel, cx);
    ui(cx).tray_previews.remove(&(channel, id.clone()));
    client.attachments().discard(&tray, id);
    cx.refresh_windows();
}

/// The descriptors a send carries, taken from the tray of `channel`; `Err` with the toast's text while a file is not
/// ready (`shell.tray.notReady`). The composer's send calls it (the integration hook named in Task 12's report).
pub fn take_for_send(
    channel: ChannelId,
    cx: &mut App,
) -> Result<Vec<dilla_client::features::attachments::Descriptor>, &'static str> {
    let Some(tray) = ui(cx).trays.get(&channel).cloned() else {
        return Ok(Vec::new());
    };
    if !tray.all_ready() {
        return Err(t("shell.tray.notReady"));
    }
    let out = tray.take_all().map_err(|_| t("shell.tray.notReady"))?;
    ui(cx).tray_previews.retain(|(c, _), _| *c != channel);
    Ok(out)
}

/// Opens a thumbnail or a whole picture once; the card or the lightbox draws it when it is ready.
pub fn ensure_image(key: ImageKey, cx: &mut App) {
    let Some(client) = ui(cx).client.clone() else {
        return;
    };
    if ui(cx).images.contains_key(&key) {
        return;
    }
    ui(cx).images.insert(key, Slot::Loading);
    let rx = client
        .attachments()
        .open(key.channel, key.seq, key.index, key.thumb);
    cx.spawn(async move |cx| {
        let slot = match rx.await {
            Ok(Ok(opened)) => picture(&opened.bytes).map_or(Slot::Failed, Slot::Ready),
            _ => Slot::Failed,
        };
        cx.update(|cx| {
            ui(cx).images.insert(key, slot);
            cx.refresh_windows();
        });
    })
    .detach();
}

/// Saves an attachment where the person chooses (only after their action): it is opened, the save dialog asks for a
/// path, and the plaintext is written there.
pub fn save(key: CardKey, cx: &mut App) {
    let Some(client) = ui(cx).client.clone() else {
        return;
    };
    ui(cx).cards.insert(key, CardStatus::Loading);
    cx.refresh_windows();
    let rx = client
        .attachments()
        .open(key.channel, key.seq, key.index, false);
    let home = dirs_home();
    cx.spawn(async move |cx| {
        let opened = match rx.await {
            Ok(Ok(o)) => o,
            Ok(Err(e)) => {
                cx.update(|cx| {
                    ui(cx).cards.insert(key, CardStatus::Failed(e.to_string()));
                    cx.refresh_windows();
                });
                return;
            }
            Err(_) => return,
        };
        let shown = safe_name(&opened.name, t("shell.attachment.unnamed"));
        let path_rx = cx.update(|cx| cx.prompt_for_new_path(&home, Some(&shown)));
        let result = match path_rx.await {
            Ok(Ok(Some(path))) => {
                dilla_client::features::attachments::Attachments::save(&path, &opened.bytes)
                    .map_err(|_| "E_SAVE".to_owned())
            }
            _ => Ok(()),
        };
        drop(opened);
        cx.update(|cx| {
            match result {
                Ok(()) => ui(cx).cards.remove(&key),
                Err(code) => ui(cx).cards.insert(key, CardStatus::Failed(code)),
            };
            cx.refresh_windows();
        });
    })
    .detach();
}

fn dirs_home() -> PathBuf {
    std::env::var_os("HOME")
        .or_else(|| std::env::var_os("USERPROFILE"))
        .map_or_else(|| PathBuf::from("."), PathBuf::from)
}

/// The media card's play: an audio file is opened and played inline (symphonia, rodio); a video is written to the
/// instance's directory and opened in the system player.
pub fn play(key: CardKey, video: bool, mime: String, cx: &mut App) {
    let Some(client) = ui(cx).client.clone() else {
        return;
    };
    if matches!(ui(cx).players.get(&key), Some(Player::Loading)) {
        return;
    }
    ui(cx).players.insert(key, Player::Loading);
    cx.refresh_windows();
    let rx = client
        .attachments()
        .open(key.channel, key.seq, key.index, false);
    cx.spawn(async move |cx| {
        let opened = match rx.await {
            Ok(Ok(o)) => o,
            _ => {
                cx.update(|cx| {
                    ui(cx).players.insert(key, Player::Error);
                    cx.refresh_windows();
                });
                return;
            }
        };
        cx.update(|cx| {
            let state = if video {
                let dir = ui(cx).instance_dir.clone();
                match dir.map(|d| media::write_for_system_player(&d, &mime, &opened.bytes)) {
                    Some(Ok(path)) => {
                        cx.open_with_system(&path);
                        None
                    }
                    _ => Some(Player::Error),
                }
            } else {
                let bytes = Arc::new(opened.bytes);
                match media::open_audio(bytes, &mime)
                    .map(|d| Box::new(d) as Box<dyn MediaDecoder>)
                    .and_then(Playback::start)
                {
                    Ok(p) => Some(Player::Ready(p)),
                    Err(_) => Some(Player::Error),
                }
            };
            match state {
                Some(s) => ui(cx).players.insert(key, s),
                None => ui(cx).players.remove(&key),
            };
            cx.refresh_windows();
        });
        // Redraw the position while it plays.
        loop {
            cx.background_executor().timer(Duration::from_millis(250)).await;
            let go = cx.update(|cx| {
                let playing = matches!(ui(cx).players.get(&key), Some(Player::Ready(p)) if !p.is_paused() && !p.ended());
                if playing {
                    cx.refresh_windows();
                }
                matches!(ui(cx).players.get(&key), Some(Player::Ready(_)))
            });
            if !go {
                return;
            }
        }
    })
    .detach();
}

/// Play or pause a ready player; at its end, play starts it again from the beginning.
pub fn toggle(key: CardKey, cx: &mut App) {
    if let Some(Player::Ready(p)) = ui(cx).players.get(&key) {
        if p.ended() || p.is_paused() {
            if p.ended() {
                let _ = p.seek(Duration::ZERO);
            }
            p.play();
        } else {
            p.pause();
        }
    }
    cx.refresh_windows();
}

/// Opens the lightbox on image `index` of a message's images, focus on close.
pub fn open_lightbox(
    channel: ChannelId,
    seq: u64,
    images: Vec<(u32, String, u64)>,
    index: u32,
    window: &mut Window,
    cx: &mut App,
) {
    let at = images.iter().position(|(i, _, _)| *i == index).unwrap_or(0);
    let close_focus = cx.focus_handle();
    window.focus(&close_focus, cx);
    ui(cx).lightbox = Some(LightboxState {
        channel,
        seq,
        images,
        at,
        close_focus,
    });
    cx.refresh_windows();
}

/// Moves the lightbox by `delta` images, when there is one there.
pub fn step_lightbox(delta: isize, cx: &mut App) {
    if let Some(l) = ui(cx).lightbox.as_mut() {
        let to = l.at as isize + delta;
        if to >= 0 && (to as usize) < l.images.len() {
            l.at = to as usize;
        }
    }
    cx.refresh_windows();
}

/// Closes the lightbox; the whole pictures it opened are let go.
pub fn close_lightbox(cx: &mut App) {
    let u = ui(cx);
    u.lightbox = None;
    u.images.retain(|k, _| k.thumb);
    cx.refresh_windows();
}

/// Asks once per server whether its GIFs are on (`gifStatus`): the composer's GIF button shows only then.
pub fn gif_on(community: CommunityId, cx: &mut App) -> bool {
    if let Some(known) = ui(cx).gif_on.get(&community) {
        return known.unwrap_or(false);
    }
    let Some(client) = ui(cx).client.clone() else {
        return false;
    };
    ui(cx).gif_on.insert(community, None);
    let rx = client.gifs().status(community);
    cx.spawn(async move |cx| {
        let on = matches!(rx.await, Ok(Ok(s)) if s.on);
        cx.update(|cx| {
            ui(cx).gif_on.insert(community, Some(on));
            cx.refresh_windows();
        });
    })
    .detach();
    false
}

/// Opens or closes the GIF picker of `channel` in `community`, searching trending at once.
pub fn toggle_gifs(channel: ChannelId, community: CommunityId, cx: &mut App) {
    if ui(cx).gif.take().is_some() {
        cx.refresh_windows();
        return;
    }
    ui(cx).gif = Some(GifState {
        channel: Some(channel),
        community: Some(community),
        ..GifState::default()
    });
    search_gifs(String::new(), false, cx);
}

/// Searches (or, with `more`, reads the next page of) the picker's query.
pub fn search_gifs(query: String, more: bool, cx: &mut App) {
    let Some(client) = ui(cx).client.clone() else {
        return;
    };
    let Some(g) = ui(cx).gif.as_mut() else { return };
    let Some(community) = g.community else { return };
    let offset = if more { g.next.unwrap_or(0) } else { 0 };
    if !more {
        g.items.clear();
        g.active = None;
    }
    g.query = query.clone();
    g.loading = true;
    g.error = false;
    g.turn += 1;
    let turn = g.turn;
    cx.refresh_windows();
    let rx = client.gifs().search(community, query, offset);
    cx.spawn(async move |cx| {
        let page = rx.await;
        let ids = cx.update(|cx| {
            let g = ui(cx).gif.as_mut().filter(|g| g.turn == turn)?;
            g.loading = false;
            match page {
                Ok(Ok(p)) => {
                    let ids: Vec<String> = p.items.iter().map(|i| i.id.clone()).collect();
                    g.items.extend(p.items);
                    g.next = p.next;
                    cx.refresh_windows();
                    Some(ids)
                }
                _ => {
                    g.error = true;
                    cx.refresh_windows();
                    None
                }
            }
        });
        for id in ids.unwrap_or_default() {
            let rx = client
                .gifs()
                .media(community, id.clone(), GifVariant::Preview);
            if let Ok(Ok(bytes)) = rx.await {
                cx.update(|cx| {
                    if let (Some(g), Some(p)) = (ui(cx).gif.as_mut(), picture(&bytes)) {
                        g.previews.insert(id, p);
                        cx.refresh_windows();
                    }
                });
            }
        }
    })
    .detach();
}

/// Picks a GIF: its full file is fetched through the instance and attached as an ordinary attachment (the worker's
/// note: "a GIF crosses back as a Blob and the page sends it with attachFiles").
pub fn pick_gif(index: usize, cx: &mut App) {
    let Some(client) = ui(cx).client.clone() else {
        return;
    };
    let Some(g) = ui(cx).gif.as_mut() else { return };
    if g.picking.is_some() {
        return;
    }
    let (Some(item), Some(community)) = (g.items.get(index).cloned(), g.community) else {
        return;
    };
    g.picking = Some(item.id.clone());
    cx.refresh_windows();
    let rx = client
        .gifs()
        .media(community, item.id.clone(), GifVariant::Full);
    cx.spawn(async move |cx| {
        let got = rx.await;
        cx.update(|cx| {
            match got {
                Ok(Ok(bytes)) => {
                    let title = if item.title.is_empty() {
                        t("gif.untitled").to_owned()
                    } else {
                        item.title.clone()
                    };
                    let name = format!("{}.gif", safe_name(&title, "gif"));
                    ui(cx).gif = None;
                    attach(
                        vec![NewFile::from_bytes(&name, "image/gif", bytes)],
                        vec![Some(TrayPreview::Gif)],
                        cx,
                    );
                }
                _ => {
                    if let Some(g) = ui(cx).gif.as_mut() {
                        g.picking = None;
                    }
                    dilla_desktop_ui::components::toast::show_toast(t("gif.pickFailed"), cx);
                }
            }
            cx.refresh_windows();
        });
    })
    .detach();
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn sizes_read_as_shell_model_ts_formats_them() {
        assert_eq!(format_size(39), "39 B");
        assert_eq!(format_size(1024), "1.0 KB");
        assert_eq!(format_size(48 * 1024), "48 KB");
        assert_eq!(format_size(192_512), "188 KB");
        assert_eq!(format_size(26_214_400), "25 MB");
        assert_eq!(format_size(1_572_864), "1.5 MB");
    }

    #[test]
    fn a_decompression_bomb_is_never_handed_to_gpui() {
        let mut bomb = b"GIF89a\xff\xff\xff\xff\x00\x00\x00".to_vec();
        bomb.extend_from_slice(&[
            0x2c, 0, 0, 0, 0, 0xff, 0xff, 0xff, 0xff, 0, 0x02, 0x02, 0x4c, 0x01, 0x00, 0x3b,
        ]);
        assert!(
            picture(&bomb).is_none(),
            "the card keeps its still or its neutral box"
        );
        let ok = b"GIF89a\x01\x00\x01\x00\x00\x00\x00\x2c\x00\x00\x00\x00\x01\x00\x01\x00\x00\x02\x02\x4c\x01\x00\x3b";
        assert!(picture(ok).is_some());
    }

    #[test]
    fn only_the_four_inline_types_are_pictures() {
        assert_eq!(
            image_format(b"\x89PNG\r\n\x1a\n...."),
            Some(ImageFormat::Png)
        );
        assert_eq!(
            image_format(&[0xff, 0xd8, 0xff, 0xe0]),
            Some(ImageFormat::Jpeg)
        );
        assert_eq!(
            image_format(b"RIFF\0\0\0\0WEBPVP8 "),
            Some(ImageFormat::Webp)
        );
        assert_eq!(image_format(b"GIF89a"), Some(ImageFormat::Gif));
        assert_eq!(image_format(b"<svg xmlns="), None);
    }
}
