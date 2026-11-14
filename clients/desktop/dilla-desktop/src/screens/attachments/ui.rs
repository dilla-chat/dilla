//! The attachments area's view state (one [`AttachmentsUi`] global per app) and its actions: the trays (one per client and
//! channel, as the worker keeps `trays`), the opened thumbnails and images, the cards' open and save states, the
//! lightbox, the players, the GIF picker and whether files are dragged over the window.
//!
//! Not zeroized (a known limit): the decoded PCM blocks of a player and the copies GPUI keeps of a picture
//! (`gpui::Image` bytes and its decoded frames) are plain buffers; they are freed, not wiped, when let go. The
//! decrypted files themselves are `Zeroizing` until they are handed to a decoder or to GPUI.
//!
//! Decrypted bytes live only here, in memory, as the decoded images GPUI draws and the players' buffers; they go when
//! the card, the lightbox or the player lets them go, and with the app. Nothing is written to disk except a file the
//! person saves (after their choice of path) and a video handed to the system player (`media::write_for_system_player`,
//! in the instance's own directory, wiped at start and exit).

use std::collections::{BTreeSet, HashMap, HashSet};
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
use gpui::{App, FocusHandle, Global, Image, ImageFormat, Window, WindowId};

use super::media::{self, MediaDecoder, Playback};
use crate::strings::{t, tf};

/// Which opened picture: a message's attachment, its thumbnail or the whole file, as one client opened it (review M2:
/// two accounts of one server see the same channel and seq; each keeps its own).
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash)]
pub struct ImageKey {
    /// The opening client's [`Client::serial`].
    pub client: u64,
    pub channel: ChannelId,
    pub seq: u64,
    pub index: u32,
    pub thumb: bool,
}

/// An entry of the images, the cards or the players, for [`AttachmentsUi::media_owners`].
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash)]
pub enum MediaKey {
    Image(ImageKey),
    Player(CardKey),
}

/// A card's attachment, as one client shows it.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash)]
pub struct CardKey {
    /// The client's [`Client::serial`].
    pub client: u64,
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
    /// The client whose message it shows (`None` in a drawn fixture, whose whole pictures are already in `images`), and
    /// the window it is drawn in (multi-instance: never another window's).
    pub client: Option<Rc<Client>>,
    pub owner: WindowId,
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
    /// The client that searches and attaches the pick, and the window the picker is drawn in.
    pub client: Option<Rc<Client>>,
    pub owner: Option<WindowId>,
}

/// A tray entry's picture, kept by its tray id (Attachments.tsx `trayPreviews`).
#[derive(Clone)]
pub enum TrayPreview {
    Image(Arc<Image>),
    Gif,
}

/// The attachments area's state. One per app, and that is safe by construction (final review sweep): what belongs to a
/// window is keyed by it (`targets`) or carries its owner window and is drawn only there (the lightbox, the GIF picker,
/// the DM notice); what belongs to a client is keyed by its [`Client::serial`] (review M2: trays, previews, images,
/// cards and players) and goes with it in [`release`]; the played-video directories are per instance and are wiped at
/// open and at quit.
#[derive(Default)]
pub struct AttachmentsUi {
    /// Each client's tray of each channel, by `(Client::serial, channel)`.
    pub trays: HashMap<(u64, ChannelId), Tray>,
    pub tray_previews: HashMap<(u64, ChannelId, String), TrayPreview>,
    pub images: HashMap<ImageKey, Slot>,
    pub cards: HashMap<CardKey, CardStatus>,
    pub players: HashMap<CardKey, Player>,
    /// The newest open's generation of each image, card and player (the key names its client): [`release`] drops what
    /// that client opened, whatever channel it is in (a DM's, a left channel's, a thread's; re-review N3).
    pub media_owners: HashMap<MediaKey, u64>,
    /// The saves in flight, by generation: each one writes the path its own dialog chose.
    pub saves: HashMap<u64, CardKey>,
    /// The last open's generation (images, players, saves).
    pub opens: u64,
    pub lightbox: Option<LightboxState>,
    pub gif: Option<GifState>,
    /// The clients released so far (by `Client::serial`, never reused): a DM GIF notice whose read answers after its
    /// client was released is not shown (final review I5).
    pub released: BTreeSet<u64>,
    /// `gifStatus` per server: `None` while asked.
    pub gif_on: HashMap<CommunityId, Option<bool>>,
    /// Per window, what its composer last drew for: where a paste, a drop or a pick in that window goes. Keyed by
    /// window, so with two instances open a file never goes to the other window's client or channel.
    pub targets: HashMap<WindowId, Target>,
    /// Every opened instance's data directory: each one's played-video directory is wiped at quit.
    pub instance_dirs: BTreeSet<PathBuf>,
}

/// A window's composer target: its client and its open channel, and the window (its toasts show there).
#[derive(Clone, Default)]
pub struct Target {
    pub client: Option<Rc<Client>>,
    pub channel: Option<ChannelId>,
    pub window: Option<WindowId>,
}

/// The composer target of `window` (empty until its composer draws).
pub fn target(window: &Window, cx: &mut App) -> Target {
    let id = window_id(window);
    ui(cx).targets.get(&id).cloned().unwrap_or_default()
}

pub(crate) fn window_id(window: &Window) -> WindowId {
    window.window_handle().window_id()
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

/// Remembers the client and the channel `window`'s composer draws for. Windows that are gone are let go (with the
/// client they held).
pub fn track(
    window: &Window,
    client: Option<Rc<Client>>,
    channel: Option<ChannelId>,
    cx: &mut App,
) {
    let live: HashSet<WindowId> = cx.windows().iter().map(|w| w.window_id()).collect();
    let dir = client
        .as_ref()
        .and_then(|c| c.prefs().path().parent().map(PathBuf::from));
    let u = ui(cx);
    if let Some(dir) = dir {
        u.instance_dirs.insert(dir);
    }
    u.targets.retain(|w, _| live.contains(w));
    let id = window_id(window);
    u.targets.insert(
        id,
        Target {
            client,
            channel,
            window: Some(id),
        },
    );
}

/// The instance at `data_dir` opens (its client is about to start): a video an earlier run left for the system
/// player (a crash, a kill) is wiped. The app calls it from `start_client` (the controller's hook, Task 12's report).
pub fn instance_opened(data_dir: &std::path::Path, cx: &mut App) {
    media::wipe_open_dir(data_dir);
    ui(cx).instance_dirs.insert(data_dir.to_owned());
}

/// The app quits: every video handed to the system player, by any instance opened in this run, is wiped (the hook in
/// `main.rs`, Task 12's report).
pub fn on_quit(cx: &mut App) {
    for d in &ui(cx).instance_dirs {
        media::wipe_open_dir(d);
    }
}

/// Lets go of `client` (Forget, sign-out: `crate::release::release_client`): the windows' targets, the lightbox, the
/// GIF picker and the trays that hold it go, so its store can close.
pub fn release(client: &Rc<Client>, cx: &mut App) {
    if !cx.has_global::<AttachmentsUi>() {
        return;
    }
    let owner = owner_of(client);
    let u = ui(cx);
    u.targets
        .retain(|_, t| !t.client.as_ref().is_some_and(|c| Rc::ptr_eq(c, client)));
    if u.lightbox
        .as_ref()
        .is_some_and(|l| l.client.as_ref().is_some_and(|c| Rc::ptr_eq(c, client)))
    {
        u.lightbox = None;
    }
    if u.gif
        .as_ref()
        .is_some_and(|g| g.client.as_ref().is_some_and(|c| Rc::ptr_eq(c, client)))
    {
        u.gif = None;
    }
    u.trays.retain(|(c, _), _| *c != owner);
    u.tray_previews.retain(|(c, _, _), _| *c != owner);
    u.released.insert(owner);
    // Final review I5: the DM GIF notice of any window that asked with it goes.
    crate::per_window::each_mut::<Notice>(cx, |_, n| {
        if n.0.as_ref().is_some_and(|n| Rc::ptr_eq(&n.client, client)) {
            n.0 = None;
        }
    });
    // The decrypted images, the cards' state and the players the client opened go too (re-review N3: by owner, so a
    // DM's, a left channel's and a thread's go as well): a released account's media is not left in the process.
    forget_owner(owner, cx);
}

/// `window` closed: its composer target, and the lightbox, GIF picker and DM GIF notice drawn in it, go with their
/// client (final review C1). The window's DM GIF notice is per window and goes with it (`per_window`).
pub fn window_closed(window: WindowId, cx: &mut App) {
    if !cx.has_global::<AttachmentsUi>() {
        return;
    }
    let u = ui(cx);
    u.targets.remove(&window);
    if u.lightbox.as_ref().is_some_and(|l| l.owner == window) {
        u.lightbox = None;
    }
    if u.gif.as_ref().is_some_and(|g| g.owner == Some(window)) {
        u.gif = None;
    }
}

/// An open of `key` starts: it gets the next generation, which [`still_waiting`] compares (an older open's completion
/// never lands on a newer one's entry).
fn owns(key: MediaKey, cx: &mut App) -> u64 {
    let u = ui(cx);
    u.opens += 1;
    let open = u.opens;
    u.media_owners.insert(key, open);
    open
}

impl MediaKey {
    /// The client (its [`Client::serial`]) whose entry this is.
    pub fn client(&self) -> u64 {
        match self {
            MediaKey::Image(k) => k.client,
            MediaKey::Player(k) => k.client,
        }
    }
}

/// Drops the images, cards, saves and players the client `owner` (its [`Client::serial`]) opened.
pub fn forget_owner(owner: u64, cx: &mut App) {
    let u = ui(cx);
    let gone: Vec<MediaKey> = u
        .media_owners
        .keys()
        .filter(|k| k.client() == owner)
        .copied()
        .collect();
    for key in gone {
        u.media_owners.remove(&key);
        match key {
            MediaKey::Image(k) => {
                u.images.remove(&k);
            }
            MediaKey::Player(k) => {
                u.players.remove(&k);
            }
        }
    }
    // Every entry names its client: a card's save state (kept by `saves`, not `media_owners`) goes too.
    u.images.retain(|k, _| k.client != owner);
    u.cards.retain(|k, _| k.client != owner);
    u.players.retain(|k, _| k.client != owner);
    u.saves.retain(|_, k| k.client != owner);
}

/// What the keys record for `client`: its serial, never reused within the process.
pub fn owner_of(client: &Client) -> u64 {
    client.serial()
}

/// Whether the open of `key` with generation `open` may still land: it is still the entry's newest open and the entry
/// still waits for it. A release ([`forget_owner`]) while the open was in flight drops both, and the completion then
/// keeps nothing (re-review N3: a late completion never brings plaintext or a player back without an owner).
fn still_waiting(open: u64, key: MediaKey, cx: &mut App) -> bool {
    let u = ui(cx);
    u.media_owners.get(&key) == Some(&open)
        && match key {
            MediaKey::Image(k) => matches!(u.images.get(&k), Some(Slot::Loading)),
            MediaKey::Player(k) => matches!(u.players.get(&k), Some(Player::Loading)),
        }
}

/// An image's open landed: `slot` is kept only while [`still_waiting`]; otherwise it is dropped. Answers whether it was
/// kept.
fn land_image(open: u64, key: ImageKey, slot: Slot, cx: &mut App) -> bool {
    if !still_waiting(open, MediaKey::Image(key), cx) {
        return false;
    }
    ui(cx).images.insert(key, slot);
    true
}

/// A save starts: it has its own generation in `saves` (two saves of one card each write their own path), and the
/// card shows it opening.
fn save_started(key: CardKey, cx: &mut App) -> u64 {
    let u = ui(cx);
    u.opens += 1;
    let open = u.opens;
    u.saves.insert(open, key);
    u.cards.insert(key, CardStatus::Loading);
    open
}

/// Whether the save `open` may go on: its client was not released.
fn save_live(open: u64, cx: &mut App) -> bool {
    ui(cx).saves.contains_key(&open)
}

/// The save `open` finished: its failure (`Some`) is shown, or, once no other save of the card is in flight, the card is
/// done (`None`). A released client's save lands nothing. Answers whether it landed.
fn land_save(open: u64, status: Option<CardStatus>, cx: &mut App) -> bool {
    let u = ui(cx);
    let Some(key) = u.saves.remove(&open) else {
        return false;
    };
    match status {
        Some(s) => {
            u.cards.insert(key, s);
        }
        None if !u.saves.values().any(|k| *k == key) => {
            u.cards.remove(&key);
        }
        None => {}
    }
    true
}

/// A player's open landed: `start` (which starts the device's playback, or opens the system player) runs only while
/// [`still_waiting`], so a released client's audio never starts; its player, if any, is kept. Answers whether it ran.
fn land_player(
    open: u64,
    key: CardKey,
    start: impl FnOnce(&mut App) -> Option<Player>,
    cx: &mut App,
) -> bool {
    if !still_waiting(open, MediaKey::Player(key), cx) {
        return false;
    }
    match start(cx) {
        Some(p) => ui(cx).players.insert(key, p),
        None => ui(cx).players.remove(&key),
    };
    true
}

/// `client`'s tray of `channel`, made on first use.
pub fn tray_of(client: &Client, channel: ChannelId, cx: &mut App) -> Tray {
    ui(cx)
        .trays
        .entry((owner_of(client), channel))
        .or_insert_with(|| client.attachments().tray(channel))
        .clone()
}

/// Attachments.tsx `attachFiles`: the files join the tray (a toast for a file over 25 MB, the tray's own refusal of
/// an eleventh file), each picture with its preview.
pub fn attach(to: Target, files: Vec<NewFile>, previews: Vec<Option<TrayPreview>>, cx: &mut App) {
    let (Some(client), Some(channel), shown_in) = (to.client, to.channel, to.window) else {
        return;
    };
    if files.is_empty() {
        return;
    }
    let tray = tray_of(&client, channel, cx);
    let owner = owner_of(&client);
    let over = files.iter().any(|f| f.size > ATTACHMENT_CAP);
    if over && tray.entries().len() + files.len() <= MAX_ATTACHMENTS {
        dilla_desktop_ui::components::toast::show_toast_in(
            shown_in,
            t("shell.tray.overLimitToast"),
            cx,
        );
    }
    match client.attachments().attach(&tray, files) {
        Ok(ids) => {
            let u = ui(cx);
            for (id, p) in ids.into_iter().zip(previews) {
                if let Some(p) = p {
                    u.tray_previews.insert((owner, channel, id), p);
                }
            }
        }
        Err(e) => {
            let text = if e.code() == "E_ATTACHMENT_COUNT" {
                t("shell.tray.tooMany").to_owned()
            } else {
                tf("shell.banner.commandError", &[("code", &e.code())])
            };
            dilla_desktop_ui::components::toast::show_toast_in(shown_in, text, cx);
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
            dilla_client::features::attachments::read_bounded(path)
                .ok()
                .filter(|b| b.len() as u64 <= ATTACHMENT_CAP)
                .and_then(|b| picture(&b))
                .map(TrayPreview::Image)
        }
        _ => None,
    };
    Some((file, preview))
}

/// Pasted pictures (Ctrl+V): each becomes a PNG-named file with its preview.
pub fn attach_images(to: Target, images: Vec<Image>, cx: &mut App) {
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
    attach(to, files, previews, cx);
}

/// Opens the person's file dialog and attaches what they pick (the paperclip) to `window`'s channel as it is now.
pub fn pick_files(window: &Window, cx: &mut App) {
    let to = target(window, cx);
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
        attach_paths(to, paths, cx).await;
    })
    .detach();
}

/// Picked or dropped files: their sizes and previews are read off the UI thread, then they join the tray.
pub async fn attach_paths(to: Target, paths: Vec<PathBuf>, cx: &mut gpui::AsyncApp) {
    let picked: Vec<_> = cx
        .background_executor()
        .spawn(async move { paths.iter().filter_map(|p| file_from_path(p)).collect() })
        .await;
    cx.update(|cx| {
        let (files, previews) = picked.into_iter().unzip();
        attach(to, files, previews, cx);
    });
}

/// Removes a tray entry (and its preview).
pub fn discard(client: &Client, channel: ChannelId, id: String, cx: &mut App) {
    let tray = tray_of(client, channel, cx);
    ui(cx)
        .tray_previews
        .remove(&(owner_of(client), channel, id.clone()));
    client.attachments().discard(&tray, id);
    cx.refresh_windows();
}

/// Whether `client`'s tray of `channel` holds a file.
pub fn has_files(client: &Client, channel: ChannelId, cx: &mut App) -> bool {
    ui(cx)
        .trays
        .get(&(owner_of(client), channel))
        .is_some_and(|t| !t.entries().is_empty())
}

/// The descriptors a send carries, taken from `client`'s tray of `channel`; `Err` with the toast's text while a file is
/// not ready (`shell.tray.notReady`). The composer's send calls it (the integration hook named in Task 12's report).
pub fn take_for_send(
    client: &Client,
    channel: ChannelId,
    cx: &mut App,
) -> Result<Vec<dilla_client::features::attachments::Descriptor>, &'static str> {
    let owner = owner_of(client);
    let Some(tray) = ui(cx).trays.get(&(owner, channel)).cloned() else {
        return Ok(Vec::new());
    };
    if !tray.all_ready() {
        return Err(t("shell.tray.notReady"));
    }
    let out = tray.take_all().map_err(|_| t("shell.tray.notReady"))?;
    ui(cx)
        .tray_previews
        .retain(|(o, c, _), _| (*o, *c) != (owner, channel));
    Ok(out)
}

/// What an open answers: the decrypted file (`Zeroizing`), or why not.
pub type OpenAnswer = tokio::sync::oneshot::Receiver<
    Result<dilla_client::features::attachments::Opened, dilla_client::ClientError>,
>;

/// Opens a thumbnail or a whole picture once; the card or the lightbox draws it when it is ready.
pub fn ensure_image(client: &Client, key: ImageKey, cx: &mut App) {
    if ui(cx).images.contains_key(&key) {
        return;
    }
    let rx = client
        .attachments()
        .open(key.channel, key.seq, key.index, key.thumb);
    ensure_image_from(key, rx, cx);
}

/// [`ensure_image`] with the open already started (`rx`); the tests drive it with their own answers.
pub fn ensure_image_from(key: ImageKey, rx: OpenAnswer, cx: &mut App) {
    ui(cx).images.insert(key, Slot::Loading);
    let open = owns(MediaKey::Image(key), cx);
    cx.spawn(async move |cx| {
        // The decrypted bytes are `Zeroizing`: they are wiped when `opened` drops, here.
        let slot = match rx.await {
            Ok(Ok(opened)) => picture(&opened.bytes).map_or(Slot::Failed, Slot::Ready),
            _ => Slot::Failed,
        };
        cx.update(|cx| {
            // Released meanwhile: the decoded picture is dropped, never cached without an owner.
            if land_image(open, key, slot, cx) {
                cx.refresh_windows();
            }
        });
    })
    .detach();
}

/// Saves an attachment, `name` as its card shows it, where the person chooses (only after their action): the save dialog
/// asks for a path, then the file is opened and its plaintext written there (review L m7: nothing is decrypted while the
/// dialog asks, for as long as it is left open). Each save is its own: a second save of the card while the first is in
/// flight writes the path its own dialog chose, or shows its own failure.
///
/// Review HL I3: the save holds only a `Weak` client while the dialog asks, upgraded once a path is chosen, so the
/// release of the client (a Forget, a sign-out) is never held up by a dialog left open; a client gone by then opens
/// nothing.
pub fn save(client: &Rc<Client>, key: CardKey, name: &str, cx: &mut App) {
    let open = move |c: &Client| c.attachments().open(key.channel, key.seq, key.index, false);
    save_weak(Rc::downgrade(client), open, key, name, dirs_home(), cx);
}

/// [`save`] over any holder of the open (the client): only `holder` (a `Weak`) is kept while the dialog asks; `open`
/// runs on it once a path is chosen, and a holder gone by then opens nothing (the save ends with no status).
pub fn save_weak<T: 'static>(
    holder: std::rc::Weak<T>,
    open: impl FnOnce(&T) -> OpenAnswer + 'static,
    key: CardKey,
    name: &str,
    home: PathBuf,
    cx: &mut App,
) {
    let open = move || match holder.upgrade() {
        Some(h) => open(&h),
        // A closed answer: the save lands with no status.
        None => tokio::sync::oneshot::channel().1,
    };
    save_from(key, name, open, home, cx);
}

/// [`save`] with the open to start once a path is chosen (`open`) and the dialog starting at `home`; the tests drive it.
pub fn save_from(
    key: CardKey,
    name: &str,
    open: impl FnOnce() -> OpenAnswer + 'static,
    home: PathBuf,
    cx: &mut App,
) {
    let save = save_started(key, cx);
    cx.refresh_windows();
    let shown = safe_name(name, t("shell.attachment.unnamed"));
    let path_rx = cx.prompt_for_new_path(&home, Some(&shown));
    cx.spawn(async move |cx| {
        let path = match path_rx.await {
            Ok(Ok(Some(path))) => path,
            _ => {
                cx.update(|cx| {
                    if land_save(save, None, cx) {
                        cx.refresh_windows();
                    }
                });
                return;
            }
        };
        // Released while the dialog asked: nothing is opened, nothing written.
        if !cx.update(|cx| save_live(save, cx)) {
            return;
        }
        let opened = match open().await {
            Ok(Ok(o)) => o,
            Ok(Err(e)) => {
                cx.update(|cx| {
                    if land_save(save, Some(CardStatus::Failed(e.to_string())), cx) {
                        cx.refresh_windows();
                    }
                });
                return;
            }
            Err(_) => {
                cx.update(|cx| land_save(save, None, cx));
                return;
            }
        };
        // Released while the file opened: the plaintext (`Zeroizing`) is wiped as it drops, unwritten.
        let result = if cx.update(|cx| save_live(save, cx)) {
            dilla_client::features::attachments::Attachments::save(&path, &opened.bytes)
                .map_err(|_| "E_SAVE".to_owned())
        } else {
            Ok(())
        };
        drop(opened);
        cx.update(|cx| {
            let status = result.err().map(CardStatus::Failed);
            if land_save(save, status, cx) {
                cx.refresh_windows();
            }
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
pub fn play(client: &Client, key: CardKey, video: bool, mime: String, cx: &mut App) {
    if matches!(ui(cx).players.get(&key), Some(Player::Loading)) {
        return;
    }
    // The video goes to this client's own instance directory, which the quit wipes.
    let dir = client.prefs().path().parent().map(PathBuf::from);
    let rx = client
        .attachments()
        .open(key.channel, key.seq, key.index, false);
    play_from(key, video, mime, dir, rx, cx);
}

/// [`play`] with the open already started (`rx`) and the instance directory a video goes to (`dir`); the tests drive
/// it.
pub fn play_from(
    key: CardKey,
    video: bool,
    mime: String,
    dir: Option<PathBuf>,
    rx: OpenAnswer,
    cx: &mut App,
) {
    if let Some(d) = &dir {
        ui(cx).instance_dirs.insert(d.clone());
    }
    ui(cx).players.insert(key, Player::Loading);
    let owner = owns(MediaKey::Player(key), cx);
    cx.refresh_windows();
    cx.spawn(async move |cx| {
        let opened = match rx.await {
            Ok(Ok(o)) => o,
            _ => {
                cx.update(|cx| {
                    if land_player(owner, key, |_| Some(Player::Error), cx) {
                        cx.refresh_windows();
                    }
                });
                return;
            }
        };
        // Released meanwhile: the plaintext (`Zeroizing`) is wiped as it drops; nothing is written or decoded.
        if !cx.update(|cx| still_waiting(owner, MediaKey::Player(key), cx)) {
            return;
        }
        // The file write or the decoder's probe runs off the UI thread; only the device's player starts on it.
        let prepared = cx
            .background_executor()
            .spawn(async move {
                if video {
                    Err(dir.map(|d| {
                        media::write_for_system_player(&d, &mime, &opened.bytes)
                    }))
                } else {
                    Ok(media::open_audio(Arc::new(opened.bytes), &mime))
                }
            })
            .await;
        let landed = cx.update(|cx| {
            // A released client's video, written while it was prepared, goes again unopened; its audio never starts (the
            // decoder drops with its `Zeroizing` buffer).
            let written = match &prepared {
                Err(Some(Ok(path))) => Some(path.clone()),
                _ => None,
            };
            let landed = land_player(
                owner,
                key,
                move |cx| match prepared {
                    Err(Some(Ok(path))) => {
                        cx.open_with_system(&path);
                        None
                    }
                    Err(_) => Some(Player::Error),
                    Ok(decoder) => match decoder
                        .map(|d| Box::new(d) as Box<dyn MediaDecoder>)
                        .and_then(Playback::start)
                    {
                        Ok(p) => Some(Player::Ready(p)),
                        Err(_) => Some(Player::Error),
                    },
                },
                cx,
            );
            if landed {
                cx.refresh_windows();
            } else if let Some(path) = written {
                let _ = std::fs::remove_file(path);
            }
            landed
        });
        if !landed {
            return;
        }
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
    client: Option<Rc<Client>>,
    channel: ChannelId,
    seq: u64,
    images: Vec<(u32, String, u64)>,
    index: u32,
    window: &mut Window,
    cx: &mut App,
) {
    let owner = window_id(window);
    let at = images.iter().position(|(i, _, _)| *i == index).unwrap_or(0);
    let close_focus = cx.focus_handle();
    window.focus(&close_focus, cx);
    ui(cx).lightbox = Some(LightboxState {
        channel,
        seq,
        images,
        at,
        close_focus,
        client,
        owner,
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
pub fn gif_on(client: &Client, community: CommunityId, cx: &mut App) -> bool {
    if let Some(known) = ui(cx).gif_on.get(&community) {
        return known.unwrap_or(false);
    }
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
pub fn toggle_gifs(
    client: Rc<Client>,
    owner: WindowId,
    channel: ChannelId,
    community: CommunityId,
    cx: &mut App,
) {
    if ui(cx).gif.take().is_some() {
        cx.refresh_windows();
        return;
    }
    ui(cx).gif = Some(GifState {
        channel: Some(channel),
        community: Some(community),
        client: Some(client),
        owner: Some(owner),
        ..GifState::default()
    });
    search_gifs(String::new(), false, cx);
}

/// The DM GIF notice (DmGifPrivacy.test.tsx): it names the server a 1:1 DM's GIFs would go through; nothing is
/// searched until the person chooses "Use GIFs".
#[derive(Clone)]
pub struct DmNotice {
    pub channel: ChannelId,
    pub community: CommunityId,
    pub server: String,
    /// The client that asked, and the window the notice is drawn in.
    pub client: Rc<Client>,
    pub owner: WindowId,
}

/// One window's DM GIF notice, while it asks (final review I5: per window, never another window's).
#[derive(Default)]
struct Notice(Option<DmNotice>);

/// `window`'s DM GIF notice, while it asks.
pub fn dm_notice(window: WindowId, cx: &App) -> Option<DmNotice> {
    crate::per_window::get::<Notice>(window, cx).and_then(|n| n.0.clone())
}

/// Shows `notice` in its window (`notice.owner`), unless its client was released meanwhile (its read answered late).
pub fn show_dm_notice(notice: DmNotice, cx: &mut App) {
    if ui(cx).released.contains(&owner_of(&notice.client)) {
        return;
    }
    let owner = notice.owner;
    crate::per_window::get_mut::<Notice>(owner, cx).0 = Some(notice);
}

/// The GIF button of a 1:1 DM with `peer` (use-gif-community.ts `findCommunity`, only on the press): the shared
/// servers are asked with `gifStatus` only; none with GIFs on says so; a server this device consented to for DMs
/// opens the picker; any other opens the notice naming it.
pub fn open_dm_gifs(
    client: Rc<Client>,
    owner: WindowId,
    channel: ChannelId,
    peer: dilla_client::state::UserId,
    cx: &mut App,
) {
    // A second press closes this window's picker or notice (never another window's).
    let picker_here = ui(cx).gif.as_ref().is_some_and(|g| g.owner == Some(owner));
    if picker_here {
        ui(cx).gif = None;
    }
    let notice_here = crate::per_window::get_mut::<Notice>(owner, cx)
        .0
        .take()
        .is_some();
    if picker_here || notice_here {
        cx.refresh_windows();
        return;
    }
    let rx = client.gifs().find_dm_community(peer);
    cx.spawn(async move |cx| {
        let found = rx.await.ok().and_then(Result::ok).flatten();
        cx.update(|cx| {
            // The client was released while the instance answered: nothing opens for it.
            if ui(cx).released.contains(&owner_of(&client)) {
                return;
            }
            match found {
                None => dilla_desktop_ui::components::toast::show_toast_in(
                    Some(owner),
                    t("gif.unavailable"),
                    cx,
                ),
                Some(community) if client.gifs().dm_consent_for(community) => {
                    toggle_gifs(client.clone(), owner, channel, community, cx);
                }
                Some(community) => {
                    let server = client
                        .state()
                        .borrow()
                        .communities
                        .iter()
                        .find(|c| c.id == community)
                        .map(|c| c.name.clone())
                        .unwrap_or_default();
                    show_dm_notice(
                        DmNotice {
                            channel,
                            community,
                            server,
                            client: client.clone(),
                            owner,
                        },
                        cx,
                    );
                }
            }
            cx.refresh_windows();
        });
    })
    .detach();
}

/// The notice's "Use GIFs" in `window`: consent for the named server is saved first; only then does the picker open.
pub fn accept_dm_notice(window: WindowId, cx: &mut App) {
    let Some(n) = crate::per_window::get_mut::<Notice>(window, cx).0.take() else {
        return;
    };
    if n.client
        .gifs()
        .set_dm_consent(Some(&n.community.hex()))
        .is_err()
    {
        dilla_desktop_ui::components::toast::show_toast_in(
            Some(n.owner),
            t("workspace.preference.error"),
            cx,
        );
        cx.refresh_windows();
        return;
    }
    toggle_gifs(n.client, n.owner, n.channel, n.community, cx);
}

/// The notice's "Not now" in `window`: nothing is saved or searched.
pub fn dismiss_dm_notice(window: WindowId, cx: &mut App) {
    crate::per_window::set::<Notice>(window, None, cx);
    cx.refresh_windows();
}

/// Searches (or, with `more`, reads the next page of) the picker's query.
pub fn search_gifs(query: String, more: bool, cx: &mut App) {
    let Some(g) = ui(cx).gif.as_mut() else { return };
    let (Some(community), Some(client)) = (g.community, g.client.clone()) else {
        return;
    };
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
    let Some(g) = ui(cx).gif.as_mut() else { return };
    if g.picking.is_some() {
        return;
    }
    // The pick goes to the picker's own client and channel, whatever window drew last.
    let Some(client) = g.client.clone() else {
        return;
    };
    let to = Target {
        client: Some(client.clone()),
        channel: g.channel,
        window: g.owner,
    };
    let (Some(item), Some(community)) = (g.items.get(index).cloned(), g.community) else {
        return;
    };
    g.picking = Some(item.id.clone());
    cx.refresh_windows();
    let shown_in = to.window;
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
                        to,
                        vec![NewFile::from_bytes(&name, "image/gif", bytes)],
                        vec![Some(TrayPreview::Gif)],
                        cx,
                    );
                }
                _ => {
                    if let Some(g) = ui(cx).gif.as_mut() {
                        g.picking = None;
                    }
                    dilla_desktop_ui::components::toast::show_toast_in(
                        shown_in,
                        t("gif.pickFailed"),
                        cx,
                    );
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

    /// Re-review N3: a release drops the decrypted images and the cards the released client opened, by owner: a DM's
    /// image (in no channel list) goes too, and another client's stay. A card's state is dropped by its own key, which
    /// names its client (review L m8: no owner entry for cards).
    #[gpui::test]
    fn a_release_drops_the_media_the_client_opened(cx: &mut gpui::TestAppContext) {
        let (mine, theirs) = (ChannelId([1; 16]), ChannelId([2; 16]));
        let key = |client, channel| ImageKey {
            client,
            channel,
            seq: 1,
            index: 0,
            thumb: true,
        };
        let card = |client, channel| CardKey {
            client,
            channel,
            seq: 1,
            index: 0,
        };
        cx.update(|cx| {
            let u = ui(cx);
            // `mine` is a DM of client 1: no channel list names it.
            u.images.insert(key(1, mine), Slot::Failed);
            u.images.insert(key(2, theirs), Slot::Failed);
            u.media_owners.insert(MediaKey::Image(key(1, mine)), 10);
            u.media_owners.insert(MediaKey::Image(key(2, theirs)), 11);
            u.cards.insert(card(1, mine), CardStatus::Loading);
            u.cards.insert(card(2, theirs), CardStatus::Loading);
            forget_owner(1, cx);
            let u = ui(cx);
            assert!(
                !u.images.contains_key(&key(1, mine)) && u.images.contains_key(&key(2, theirs))
            );
            assert_eq!(u.cards.len(), 1);
        });
    }

    const CHANNEL: ChannelId = ChannelId([3; 16]);

    fn image_key() -> ImageKey {
        ImageKey {
            client: 7,
            channel: CHANNEL,
            seq: 1,
            index: 0,
            thumb: false,
        }
    }

    fn card_key() -> CardKey {
        CardKey {
            client: 7,
            channel: CHANNEL,
            seq: 1,
            index: 0,
        }
    }

    type Answer = tokio::sync::oneshot::Sender<
        Result<dilla_client::features::attachments::Opened, dilla_client::ClientError>,
    >;

    fn answer() -> (Answer, OpenAnswer) {
        tokio::sync::oneshot::channel()
    }

    fn opened(
        bytes: Vec<u8>,
        name: &str,
        mime: &str,
    ) -> dilla_client::features::attachments::Opened {
        dilla_client::features::attachments::Opened {
            bytes: zeroize::Zeroizing::new(bytes),
            name: name.to_owned(),
            mime: mime.to_owned(),
        }
    }

    fn png() -> Vec<u8> {
        let img = image::RgbImage::from_pixel(4, 4, image::Rgb([9, 9, 9]));
        let mut out = std::io::Cursor::new(Vec::new());
        img.write_to(&mut out, image::ImageFormat::Png).unwrap();
        out.into_inner()
    }

    /// One second of silence, 16-bit mono WAV at 8 kHz.
    fn wav() -> Vec<u8> {
        let (rate, n) = (8000u32, 8000u32);
        let mut out = b"RIFF".to_vec();
        out.extend_from_slice(&(36 + n * 2).to_le_bytes());
        out.extend_from_slice(b"WAVEfmt ");
        out.extend_from_slice(&16u32.to_le_bytes());
        out.extend_from_slice(&1u16.to_le_bytes());
        out.extend_from_slice(&1u16.to_le_bytes());
        out.extend_from_slice(&rate.to_le_bytes());
        out.extend_from_slice(&(rate * 2).to_le_bytes());
        out.extend_from_slice(&2u16.to_le_bytes());
        out.extend_from_slice(&16u16.to_le_bytes());
        out.extend_from_slice(b"data");
        out.extend_from_slice(&(n * 2).to_le_bytes());
        out.extend(std::iter::repeat_n(0u8, (n * 2) as usize));
        out
    }

    /// Coordinator item 3 (re-review N3, driven through `ensure_image` itself): a picture whose client is released while
    /// it opens is never cached.
    #[gpui::test]
    fn ensure_image_keeps_nothing_after_a_release_mid_open(cx: &mut gpui::TestAppContext) {
        let (tx, rx) = answer();
        cx.update(|cx| ensure_image_from(image_key(), rx, cx));
        cx.update(|cx| forget_owner(7, cx));
        tx.send(Ok(opened(png(), "a.png", "image/png"))).ok();
        cx.run_until_parked();
        cx.update(|cx| {
            let u = ui(cx);
            assert!(u.images.is_empty(), "no picture cached without an owner");
            assert!(u.media_owners.is_empty());
        });
        // Without a release the same open lands.
        let (tx, rx) = answer();
        cx.update(|cx| ensure_image_from(image_key(), rx, cx));
        tx.send(Ok(opened(png(), "a.png", "image/png"))).ok();
        cx.run_until_parked();
        cx.update(|cx| {
            assert!(matches!(
                ui(cx).images.get(&image_key()),
                Some(Slot::Ready(_))
            ))
        });
    }

    /// An open that answers whatever the test sends, and says whether it was asked for.
    fn recorded(
        rx: OpenAnswer,
    ) -> (
        Rc<std::cell::Cell<bool>>,
        impl FnOnce() -> OpenAnswer + 'static,
    ) {
        let asked = Rc::new(std::cell::Cell::new(false));
        let a = asked.clone();
        (asked, move || {
            a.set(true);
            rx
        })
    }

    /// Review L m7: the save dialog asks first, and the file is opened (decrypted) only once a path was chosen, so no
    /// plaintext waits on a dialog the person may leave open for as long as they like.
    #[gpui::test]
    fn a_save_decrypts_only_after_its_path_is_chosen(cx: &mut gpui::TestAppContext) {
        let dir = std::env::temp_dir().join(format!("dilla-l-m7-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        let (tx, rx) = answer();
        let (asked, open) = recorded(rx);
        cx.update(|cx| save_from(card_key(), "a.txt", open, dir.clone(), cx));
        cx.run_until_parked();
        assert!(cx.did_prompt_for_new_path(), "the dialog asks first");
        assert!(!asked.get(), "nothing is opened while the dialog asks");
        cx.simulate_new_path_selection(|_| Some(dir.join("a.txt")));
        cx.run_until_parked();
        assert!(asked.get(), "the chosen path opens the file");
        tx.send(Ok(opened(b"secret".to_vec(), "a.txt", "text/plain")))
            .ok();
        cx.run_until_parked();
        assert_eq!(std::fs::read(dir.join("a.txt")).unwrap(), b"secret");
        cx.update(|cx| {
            let u = ui(cx);
            assert!(u.cards.is_empty() && u.saves.is_empty());
        });
        let _ = std::fs::remove_dir_all(&dir);
    }

    /// Review HL I3: while the save dialog asks, the save holds no strong handle to its client (a `Weak`, upgraded
    /// only once a path is chosen), so a Forget or a sign-out can close the client's store meanwhile; a client gone by
    /// then opens nothing and writes nothing.
    #[gpui::test]
    fn a_save_holds_no_client_while_its_dialog_asks(cx: &mut gpui::TestAppContext) {
        let dir = std::env::temp_dir().join(format!("dilla-hl-i3-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        // A stand-in for the client: what the save captures of it is all that is checked.
        let holder = Rc::new(());
        let (_tx, rx) = answer();
        let asked = Rc::new(std::cell::Cell::new(false));
        let a = asked.clone();
        let weak = Rc::downgrade(&holder);
        let home = dir.clone();
        cx.update(move |cx| {
            save_weak(
                weak,
                move |_: &()| {
                    a.set(true);
                    rx
                },
                card_key(),
                "a.txt",
                home,
                cx,
            )
        });
        cx.run_until_parked();
        assert!(cx.did_prompt_for_new_path());
        assert_eq!(
            Rc::strong_count(&holder),
            1,
            "the open dialog holds no client"
        );
        drop(holder);
        cx.simulate_new_path_selection(|_| Some(dir.join("a.txt")));
        cx.run_until_parked();
        assert!(!asked.get(), "a client gone by then opens nothing");
        assert!(!dir.join("a.txt").exists());
        cx.update(|cx| {
            let u = ui(cx);
            assert!(u.cards.is_empty() && u.saves.is_empty());
        });
        let _ = std::fs::remove_dir_all(&dir);
    }

    /// Coordinator item 3, driven through `save` itself: a client released while its save dialog asks opens nothing and
    /// writes nothing.
    #[gpui::test]
    fn save_opens_nothing_after_a_release_mid_dialog(cx: &mut gpui::TestAppContext) {
        let dir = std::env::temp_dir().join(format!("dilla-l-m7r-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        let (_tx, rx) = answer();
        let (asked, open) = recorded(rx);
        cx.update(|cx| save_from(card_key(), "a.txt", open, dir.clone(), cx));
        cx.run_until_parked();
        assert!(cx.did_prompt_for_new_path());
        cx.update(|cx| forget_owner(7, cx));
        cx.simulate_new_path_selection(|_| Some(dir.join("a.txt")));
        cx.run_until_parked();
        assert!(!asked.get(), "nothing opened after the release");
        assert!(!dir.join("a.txt").exists());
        cx.update(|cx| {
            let u = ui(cx);
            assert!(u.cards.is_empty() && u.saves.is_empty());
        });
        let _ = std::fs::remove_dir_all(&dir);
    }

    /// Coordinator item 3, driven through `play` itself: a released client's audio never starts, and its video is never
    /// written for (or opened in) the system player.
    #[gpui::test]
    fn play_starts_nothing_after_a_release_mid_open(cx: &mut gpui::TestAppContext) {
        let dir = std::env::temp_dir().join(format!("dilla-t28-play-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        let audio = card_key();
        let video = CardKey {
            index: 1,
            ..card_key()
        };
        let (tx_a, rx_a) = answer();
        let (tx_v, rx_v) = answer();
        cx.update(|cx| {
            play_from(
                audio,
                false,
                "audio/wav".into(),
                Some(dir.clone()),
                rx_a,
                cx,
            );
            play_from(
                video,
                true,
                "video/webm".into(),
                Some(dir.clone()),
                rx_v,
                cx,
            );
            forget_owner(7, cx);
        });
        tx_a.send(Ok(opened(wav(), "a.wav", "audio/wav"))).ok();
        tx_v.send(Ok(opened(
            vec![0x1a, 0x45, 0xdf, 0xa3, 0, 0, 0, 0],
            "v.webm",
            "video/webm",
        )))
        .ok();
        cx.run_until_parked();
        cx.update(|cx| {
            let u = ui(cx);
            assert!(
                u.players.is_empty(),
                "no player is live: the audio never started"
            );
            assert!(u.media_owners.is_empty());
        });
        let left = media::open_dir(&dir);
        let files = std::fs::read_dir(&left).map_or(0, |d| d.count());
        assert_eq!(files, 0, "no video file is left for the system player");
        let _ = std::fs::remove_dir_all(&dir);
    }

    /// Coordinator item 1: an older open's completion never lands on a newer open of the same entry (each open has its
    /// own generation, not the client's tag).
    #[gpui::test]
    fn an_older_open_never_lands_on_a_newer_one(cx: &mut gpui::TestAppContext) {
        let (old_tx, old_rx) = answer();
        cx.update(|cx| ensure_image_from(image_key(), old_rx, cx));
        // Released and opened again by the same client number (as a new open would): the new open waits.
        cx.update(|cx| forget_owner(7, cx));
        let (new_tx, new_rx) = answer();
        cx.update(|cx| ensure_image_from(image_key(), new_rx, cx));
        old_tx
            .send(Ok(opened(b"not a picture".to_vec(), "x", "image/png")))
            .ok();
        cx.run_until_parked();
        cx.update(|cx| {
            assert!(
                matches!(ui(cx).images.get(&image_key()), Some(Slot::Loading)),
                "the old open's failure did not land on the new one"
            );
        });
        new_tx.send(Ok(opened(png(), "a.png", "image/png"))).ok();
        cx.run_until_parked();
        cx.update(|cx| {
            assert!(matches!(
                ui(cx).images.get(&image_key()),
                Some(Slot::Ready(_))
            ))
        });
    }

    /// Coordinator item 2: two saves of one card in flight at once each write the path their own dialog chose.
    #[gpui::test]
    fn two_saves_of_one_card_each_write_their_path(cx: &mut gpui::TestAppContext) {
        let dir = std::env::temp_dir().join(format!("dilla-t28-saves-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        let (tx1, rx1) = answer();
        let (tx2, rx2) = answer();
        cx.update(|cx| save_from(card_key(), "a.txt", move || rx1, dir.clone(), cx));
        cx.run_until_parked();
        assert!(cx.did_prompt_for_new_path());
        cx.simulate_new_path_selection(|_| Some(dir.join("first.txt")));
        cx.run_until_parked();
        cx.update(|cx| save_from(card_key(), "a.txt", move || rx2, dir.clone(), cx));
        cx.run_until_parked();
        assert!(cx.did_prompt_for_new_path(), "the second save asks too");
        cx.simulate_new_path_selection(|_| Some(dir.join("second.txt")));
        cx.run_until_parked();
        tx2.send(Ok(opened(b"two".to_vec(), "a.txt", "text/plain")))
            .ok();
        cx.run_until_parked();
        cx.update(|cx| {
            assert!(
                matches!(ui(cx).cards.get(&card_key()), Some(CardStatus::Loading)),
                "the first save is still in flight"
            );
        });
        tx1.send(Ok(opened(b"one".to_vec(), "a.txt", "text/plain")))
            .ok();
        cx.run_until_parked();
        assert_eq!(std::fs::read(dir.join("first.txt")).unwrap(), b"one");
        assert_eq!(std::fs::read(dir.join("second.txt")).unwrap(), b"two");
        cx.update(|cx| {
            let u = ui(cx);
            assert!(u.cards.is_empty() && u.saves.is_empty(), "both saves done");
        });
        let _ = std::fs::remove_dir_all(&dir);
    }

    /// Fix round 1: a video left for the system player is wiped when the instance opens and when the app quits, not
    /// on a composer's first draw; drawing the composer never wipes.
    #[gpui::test]
    fn plaintext_videos_go_at_instance_open_and_at_quit(cx: &mut gpui::TestAppContext) {
        let dir = std::env::temp_dir().join(format!("dilla-t12-life-{}", std::process::id()));
        let left = media::open_dir(&dir);
        std::fs::create_dir_all(&left).unwrap();
        std::fs::write(left.join("a.mp4"), b"x").unwrap();
        cx.update(|cx| instance_opened(&dir, cx));
        assert!(!left.exists(), "wiped at open");
        std::fs::create_dir_all(&left).unwrap();
        std::fs::write(left.join("b.mp4"), b"x").unwrap();
        let w = cx.add_window(|_, _| Blank);
        w.update(cx, |_, w, cx| track(w, None, None, cx)).unwrap();
        assert!(left.exists(), "a composer's draw never wipes");
        cx.update(on_quit);
        assert!(!left.exists(), "wiped at quit");
        let _ = std::fs::remove_dir_all(&dir);
    }

    /// Integration-1 follow-up 1: with two instances open, the played videos of both are wiped at quit, not only the
    /// last-opened one's.
    #[gpui::test]
    fn every_open_instance_s_videos_go_at_quit(cx: &mut gpui::TestAppContext) {
        let base = std::env::temp_dir().join(format!("dilla-t12-multi-{}", std::process::id()));
        let (a, b) = (base.join("a"), base.join("b"));
        cx.update(|cx| instance_opened(&a, cx));
        cx.update(|cx| instance_opened(&b, cx));
        for d in [&a, &b] {
            let left = media::open_dir(d);
            std::fs::create_dir_all(&left).unwrap();
            std::fs::write(left.join("v.mp4"), b"x").unwrap();
        }
        cx.update(on_quit);
        assert!(
            !media::open_dir(&a).exists(),
            "the first instance's video is wiped too"
        );
        assert!(
            !media::open_dir(&b).exists(),
            "the last instance's video is wiped"
        );
        let _ = std::fs::remove_dir_all(&base);
    }

    struct Blank;
    impl gpui::Render for Blank {
        fn render(
            &mut self,
            _: &mut Window,
            _: &mut gpui::Context<Self>,
        ) -> impl gpui::IntoElement {
            gpui::div()
        }
    }

    /// Integration-1 follow-up 2: what a paste, a drop or a pick goes to is the window's own composer target, never the
    /// last-drawn window's: window A drawn first and B last, A's paste still goes to A's channel.
    #[gpui::test]
    fn each_window_attaches_to_its_own_channel(cx: &mut gpui::TestAppContext) {
        let (one, two) = (ChannelId([1; 16]), ChannelId([2; 16]));
        let a = cx.add_window(|_, _| Blank);
        let b = cx.add_window(|_, _| Blank);
        a.update(cx, |_, w, cx| track(w, None, Some(one), cx))
            .unwrap();
        b.update(cx, |_, w, cx| track(w, None, Some(two), cx))
            .unwrap();
        let in_a = a.update(cx, |_, w, cx| target(w, cx).channel).unwrap();
        let in_b = b.update(cx, |_, w, cx| target(w, cx).channel).unwrap();
        assert_eq!(in_a, Some(one), "A's paste goes to A's channel");
        assert_eq!(in_b, Some(two));
        // A window that is gone lets its target (and the client it holds) go.
        cx.update(|cx| {
            a.update(cx, |_, w, _| w.remove_window()).unwrap();
        });
        cx.run_until_parked();
        b.update(cx, |_, w, cx| track(w, None, Some(two), cx))
            .unwrap();
        let kept = cx.update(|cx| ui(cx).targets.len());
        assert_eq!(kept, 1, "a closed window's target is pruned");
    }

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
