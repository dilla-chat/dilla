//! The screen-share picker (GPUI parity plan Package 2; beta.5 `screen-capture.ts` `pick`, the packages/ui
//! `ScreenSourcePicker` it drew): Share in a call opens it over the window, it lists the screens and windows with a
//! thumbnail each (refreshed every 2 s, beta.5 `PICKER_REFRESH_MS`), and the person's choice starts the share of that
//! source. Cancel, Escape, the scrim, the 2-minute deadline (`PICKER_DEADLINE_MS`) or the window going away share
//! nothing. KDE Plasma on Wayland draws this same picker too (KDE share picker plan, D22), listing and capturing through
//! KWin's own window list and screencast, so a shared window brings its sound (D21). Elsewhere on Wayland (GNOME, a
//! sandboxed Dilla, an older KWin without the installed package's `.desktop` file) there is no picker: the system
//! portal's own dialog chooses (`dilla_media::sources::desktop::portal`), and is not remembered.
//! macOS draws this same picker since GPUI gaps Task 45 (D22), listing through ScreenCaptureKit: the first open in a
//! process asks for Screen Recording, and without it the picker's empty state says how to allow it.
//!
//! Security boundary (beta.5): the picker opens only from a press on the focused window ([`open`] refuses otherwise);
//! a choice is checked against the sources of the latest enumeration (a window that closed since is refused), and it
//! goes to the media session of the window that opened the picker, never another's.

pub mod apps;
mod view;

use std::collections::HashMap;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::{Duration, Instant};

use dilla_desktop_ui::components::share_picker::TileKind;
use dilla_media::ShareOptions;
use dilla_media::sources::desktop::{self, DesktopSource, SourceKind};
use gpui::{App, Global, RenderImage, SharedString, Window, WindowId};

pub use view::overlay;

/// beta.5 `PICKER_REFRESH_MS`.
pub const REFRESH: Duration = Duration::from_secs(2);
/// beta.5 `PICKER_DEADLINE_MS`.
pub const DEADLINE: Duration = Duration::from_secs(120);
/// beta.5 `THUMBNAIL`.
pub const THUMB_W: u32 = 320;
pub const THUMB_H: u32 = 180;
/// beta.5 `PICKER_SOURCES`: at most this many tiles.
pub const MAX_SOURCES: usize = 64;

/// One enumerated source: what it is, and its thumbnail as packed RGBA (`None`: it gave no frame).
#[derive(Clone)]
pub struct Found {
    pub source: DesktopSource,
    pub thumbnail: Option<dilla_media::Frame>,
}

/// One enumeration: the tiles and whether the system refused (macOS without Screen Recording).
#[derive(Clone, Default)]
pub struct Listing {
    pub found: Vec<Found>,
    pub denied: bool,
}

impl From<Vec<Found>> for Listing {
    fn from(found: Vec<Found>) -> Self {
        Self {
            found,
            denied: false,
        }
    }
}

/// Set when the picker that started a listing closes (a pick, Cancel, Escape, the deadline, its window going): the
/// listing then takes no further thumbnail (review I2: no stream outlives the picker but the chosen one).
pub type Cancel = Arc<AtomicBool>;

/// Lists the sources the picker offers (the real one: [`enumerate`]; the tests' fake), told when to stop.
pub type Lister = Arc<dyn Fn(Cancel) -> Listing + Send + Sync>;

/// Every screen, then every window, each with a 320 x 180 thumbnail; at most [`MAX_SOURCES`]
/// (`desktop::snapshot_until`), stopping at `cancel`. Blocking: the refresh loop runs it on the background executor.
pub fn enumerate(cancel: &Cancel) -> Listing {
    let snapshot = desktop::snapshot_until(MAX_SOURCES, THUMB_W, THUMB_H, &|| {
        cancel.load(Ordering::Acquire)
    });
    listing(snapshot)
}

fn listing(snapshot: desktop::Snapshot) -> Listing {
    Listing {
        found: snapshot
            .sources
            .into_iter()
            .map(|(source, thumbnail)| Found { source, thumbnail })
            .collect(),
        denied: snapshot.denied,
    }
}

/// A tile: its stable key (the kind and the capturer's id) and what it shows.
#[derive(Clone)]
pub struct Tile {
    pub key: SharedString,
    pub source: DesktopSource,
    pub image: Option<Arc<RenderImage>>,
}

pub fn key_of(source: &DesktopSource) -> SharedString {
    let kind = match source.kind {
        SourceKind::Screen => "screen",
        SourceKind::Window => "window",
    };
    format!("{kind}:{}", source.id).into()
}

/// One window's open picker.
pub struct Picker {
    pub tab: TileKind,
    pub selected: Option<SharedString>,
    /// The latest enumeration; `None` until the first one lands.
    pub tiles: Option<Vec<Tile>>,
    /// The latest enumeration was refused by the system (macOS without Screen Recording): the empty state says how to
    /// allow it.
    pub denied: bool,
    /// The stream quality the share starts at.
    pub options: ShareOptions,
    opened: Instant,
    generation: u64,
    enumerating: bool,
    /// Its listings' stop: set when the picker goes ([`Drop`]), whichever way it goes.
    cancel: Cancel,
}

impl Drop for Picker {
    fn drop(&mut self) {
        self.cancel.store(true, Ordering::Release);
    }
}

/// Every window's open picker, and where the sources come from.
#[derive(Default)]
pub struct SharePickers {
    open: HashMap<WindowId, Picker>,
    next: u64,
    lister: Option<Lister>,
}

impl Global for SharePickers {}

/// The tests' source list (the real [`enumerate`] otherwise). With one set the picker is used even under a Wayland
/// session (the tests' list stands in for the portal's choice).
pub fn set_lister(lister: Lister, cx: &mut App) {
    cx.default_global::<SharePickers>().lister = Some(lister);
}

/// Whether the system portal chooses the source instead of the picker (Linux on Wayland without KWin's protocols, and
/// no test list).
pub fn portal(cx: &App) -> bool {
    cx.try_global::<SharePickers>()
        .is_none_or(|p| p.lister.is_none())
        && desktop::portal()
}

/// The stop of `window`'s open picker's listings (tests: a pick or a cancel sets it).
pub fn listing_cancel(window: WindowId, cx: &App) -> Option<Cancel> {
    picker_of(window, cx).map(|p| p.cancel.clone())
}

pub fn picker_of(window: WindowId, cx: &App) -> Option<&Picker> {
    cx.try_global::<SharePickers>()?.open.get(&window)
}

pub fn is_open(window: WindowId, cx: &App) -> bool {
    picker_of(window, cx).is_some()
}

/// Opens the picker over `window`, for a share at `options`: only while the window is the focused one (a press on it
/// came first). False when it did not open.
pub fn open(options: ShareOptions, window: &mut Window, cx: &mut App) -> bool {
    if !window.is_window_active() {
        return false;
    }
    let id = window.window_handle().window_id();
    let pickers = cx.default_global::<SharePickers>();
    if pickers.open.contains_key(&id) {
        return true;
    }
    pickers.next += 1;
    let generation = pickers.next;
    pickers.open.insert(
        id,
        Picker {
            tab: TileKind::Screen,
            selected: None,
            tiles: None,
            denied: false,
            options,
            opened: Instant::now(),
            generation,
            enumerating: false,
            cancel: Cancel::default(),
        },
    );
    refresh_loop(id, generation, cx);
    window.refresh();
    true
}

/// Closes `window`'s picker, sharing nothing.
pub fn cancel(window: WindowId, cx: &mut App) {
    close(window, cx);
    refresh(window, cx);
}

/// Takes `window`'s picker away and lets its thumbnails go from the window's image cache.
fn close(window: WindowId, cx: &mut App) -> Option<Picker> {
    if !cx.has_global::<SharePickers>() {
        return None;
    }
    let mut picker = cx.global_mut::<SharePickers>().open.remove(&window)?;
    drop_images(window, picker.tiles.take().unwrap_or_default(), cx);
    Some(picker)
}

fn drop_images(window: WindowId, tiles: Vec<Tile>, cx: &mut App) {
    let images: Vec<Arc<RenderImage>> = tiles.into_iter().filter_map(|t| t.image).collect();
    if images.is_empty() {
        return;
    }
    if let Some(handle) = cx.windows().into_iter().find(|w| w.window_id() == window) {
        let _ = handle.update(cx, |_, window, cx| {
            for image in images {
                cx.drop_image(image, Some(window));
            }
        });
    }
}

/// The person chose `key`: the picker closes and, when `key` is one of the latest enumeration's sources, that source's
/// share starts in `window`'s own call. A key the latest enumeration does not hold (its window closed) is refused and the
/// picker stays.
pub fn share(window: WindowId, key: &SharedString, cx: &mut App) -> bool {
    let Some(picker) = picker_of(window, cx) else {
        return false;
    };
    let Some(source) = picker
        .tiles
        .iter()
        .flatten()
        .find(|t| &t.key == key)
        .map(|t| t.source.clone())
    else {
        return false;
    };
    let options = picker.options;
    close(window, cx);
    crate::screens::voice::share_source(window, source, options, cx);
    refresh(window, cx);
    true
}

pub fn select(window: WindowId, key: SharedString, cx: &mut App) {
    if let Some(p) = cx
        .try_global::<SharePickers>()
        .and_then(|p| p.open.get(&window))
        && p.selected.as_ref() != Some(&key)
    {
        if let Some(p) = cx.global_mut::<SharePickers>().open.get_mut(&window) {
            p.selected = Some(key);
        }
        refresh(window, cx);
    }
}

pub fn set_tab(window: WindowId, tab: TileKind, cx: &mut App) {
    if let Some(p) = cx.default_global::<SharePickers>().open.get_mut(&window)
        && p.tab != tab
    {
        p.tab = tab;
        p.selected = None;
    }
    refresh(window, cx);
}

/// A window that closes takes its picker with it.
pub fn forget(window: WindowId, cx: &mut App) {
    if cx.has_global::<SharePickers>() {
        cx.global_mut::<SharePickers>().open.remove(&window);
    }
}

/// Whether a picker refresh is running for `window` (tests).
pub fn enumerated(window: WindowId, cx: &App) -> bool {
    picker_of(window, cx).is_some_and(|p| p.tiles.is_some())
}

pub(crate) fn refresh(window: WindowId, cx: &mut App) {
    if let Some(handle) = cx.windows().into_iter().find(|w| w.window_id() == window) {
        let _ = handle.update(cx, |_, window, _| window.refresh());
    }
}

/// Enumerates on a background thread now and every [`REFRESH`] while this picker (`generation`) is open, one
/// enumeration at a time; past [`DEADLINE`], or when its window is gone, the picker closes. Before the first real
/// enumeration (no test list) it asks for what listing needs (macOS: Screen Recording, once per process), on the
/// background executor too: the open was the person's press on the focused window.
fn refresh_loop(window: WindowId, generation: u64, cx: &mut App) {
    cx.spawn(async move |cx| {
        let mut first = true;
        loop {
            let lister = cx.update(|cx| {
                let alive = cx.windows().iter().any(|w| w.window_id() == window);
                let pickers = cx.default_global::<SharePickers>();
                let current = pickers
                    .open
                    .get(&window)
                    .is_some_and(|p| p.generation == generation);
                if !current {
                    return None;
                }
                let over = pickers
                    .open
                    .get(&window)
                    .is_some_and(|p| p.opened.elapsed() >= DEADLINE);
                if !alive || over {
                    if alive {
                        close(window, cx);
                    } else {
                        pickers.open.remove(&window);
                    }
                    return None;
                }
                let lister = pickers.lister.clone();
                let p = pickers.open.get_mut(&window)?;
                if p.enumerating {
                    return Some(None);
                }
                p.enumerating = true;
                Some(Some((lister, p.cancel.clone())))
            });
            match lister {
                None => {
                    cx.update(|cx| refresh(window, cx));
                    return;
                }
                Some(Some((lister, cancel))) => {
                    let ask = std::mem::take(&mut first);
                    // Review m7: where thumbnails are slow (KDE: one KWin stream each), the first open shows the
                    // tiles by name at once and their thumbnails when the listing has them.
                    if ask && lister.is_none() {
                        let quick = cx
                            .background_executor()
                            .spawn(async move { desktop::quick_titles(MAX_SOURCES) })
                            .await;
                        if let Some(quick) = quick {
                            cx.update(|cx| first_names(window, generation, listing(quick), cx));
                        }
                    }
                    let listing = cx
                        .background_executor()
                        .spawn(async move {
                            match lister {
                                Some(l) => l(cancel),
                                None => {
                                    if ask {
                                        desktop::request_access();
                                    }
                                    enumerate(&cancel)
                                }
                            }
                        })
                        .await;
                    cx.update(|cx| {
                        let denied = listing.denied;
                        let mut tiles: Vec<Tile> = listing.found.into_iter().map(tile).collect();
                        let pickers = cx.default_global::<SharePickers>();
                        let mut old = Vec::new();
                        if let Some(p) = pickers.open.get_mut(&window)
                            && p.generation == generation
                        {
                            p.enumerating = false;
                            p.denied = denied;
                            // A refresh that dropped the chosen source (its window closed) drops the selection.
                            if p.selected
                                .as_ref()
                                .is_some_and(|k| !tiles.iter().any(|t| &t.key == k))
                            {
                                p.selected = None;
                            }
                            old = p
                                .tiles
                                .replace(std::mem::take(&mut tiles))
                                .unwrap_or_default();
                        }
                        // The previous thumbnails (or this enumeration's, when the picker closed meanwhile) go.
                        old.extend(tiles);
                        drop_images(window, old, cx);
                        refresh(window, cx);
                    });
                }
                Some(None) => {}
            }
            cx.background_executor().timer(REFRESH).await;
        }
    })
    .detach();
}

/// The quick listing's tiles (names, no thumbnails), shown only while the picker (`generation`) has no listing yet.
fn first_names(window: WindowId, generation: u64, quick: Listing, cx: &mut App) {
    let pickers = cx.default_global::<SharePickers>();
    if let Some(p) = pickers.open.get_mut(&window)
        && p.generation == generation
        && p.tiles.is_none()
    {
        p.tiles = Some(quick.found.into_iter().map(tile).collect());
        refresh(window, cx);
    }
}

fn tile(found: Found) -> Tile {
    let image = found.thumbnail.and_then(|f| {
        // GPUI draws BGRA.
        let mut bgra = f.to_rgba();
        for px in bgra.as_chunks_mut::<4>().0 {
            px.swap(0, 2);
        }
        image::RgbaImage::from_raw(f.width, f.height, bgra)
            .map(|buf| Arc::new(RenderImage::new(vec![image::Frame::new(buf)])))
    });
    Tile {
        key: key_of(&found.source),
        source: found.source,
        image,
    }
}
