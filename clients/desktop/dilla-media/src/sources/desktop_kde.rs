//! Screens and windows on KDE Plasma (Wayland) for Dilla's own share picker (D22), instead of the XDG portal's dialog:
//! a shared window then brings its application's sound by itself (D21), with no sound question before the pick.
//!
//! - Detection ([`available`], once per process): the compositor's registry offers both
//!   `org_kde_plasma_window_management` (13 or later: windows by uuid) and `zkde_screencast_unstable_v1`. KWin 6.8+
//!   offers them to every client that is not sandboxed (Flatpak, Snap, a `wp_security_context_v1`); KWin 6.0-6.7 only to
//!   a client whose installed `.desktop` file names its binary in `Exec=` and lists them in `X-KDE-Wayland-Interfaces`.
//!   Anything less (one of the two, an older window list, no Wayland) leaves the portal in charge.
//! - Listing: every `wl_output` is a screen; the window list's windows are offered without Dilla's own process tree,
//!   and without unmapped, minimised, skip-taskbar and skip-switcher windows (panels, the desktop, OSDs).
//! - Capture: `stream_output` / `stream_window` make a PipeWire node of KWin's. An input stream of Dilla's, never
//!   autoconnected, is linked to it here (WirePlumber does not link it), and each buffer is copied into a [`Frame`]. The
//!   format asks for at most the share's rate, so KWin holds back what changes faster and sends the pending change
//!   later; every buffer is read ([`Newest`]). KWin sends a frame only when the source changes: the share's feeder
//!   re-sends its last encoded-size buffer while nothing new comes (`CaptureSource::repeats_when_idle`, room.rs).
//!   KWin closing the stream (the window closed, the output went away) ends the source.
//!
//! Nothing is captured before a source is picked: the picker's thumbnails are one frame each of a stream that closes
//! at once, one at a time, and a share's stream lives only as long as its [`super::desktop::DesktopCapture`].
//!
//! Logs say counts and what failed, never a title, an application id or a pid: they describe the person's desktop.

use std::cell::{Cell, RefCell};
use std::collections::{BTreeSet, HashMap};
use std::rc::Rc;
use std::sync::OnceLock;
use std::sync::atomic::{AtomicBool, AtomicU32, Ordering};
use std::time::{Duration, Instant};

use pipewire as pw;
use pw::spa;
use pw::types::ObjectType;
use wayland_client::backend::WaylandError;
use wayland_client::protocol::{wl_output, wl_registry};
use wayland_client::{Connection, Dispatch, EventQueue, QueueHandle};
use wayland_protocols_plasma::plasma_window_management::client::{
    org_kde_plasma_window as kwin_window, org_kde_plasma_window_management as kwin_windows,
};
use wayland_protocols_plasma::screencast::v1::client::{
    zkde_screencast_stream_unstable_v1 as kwin_stream, zkde_screencast_unstable_v1 as kwin_cast,
};

use super::Latest;
use super::desktop::{DesktopSource, Snapshot, SourceKind, fit_rgba};
use crate::capture_source::{Frame, PixelFormat};

/// KWin's window list.
pub(crate) const WINDOW_MANAGEMENT: &str = "org_kde_plasma_window_management";
/// KWin's PipeWire screencast.
pub(crate) const SCREENCAST: &str = "zkde_screencast_unstable_v1";
/// `window_with_uuid` (13) and `get_window_by_uuid` (12): older window lists name windows by a number only.
const MIN_WINDOW_MANAGEMENT: u32 = 13;
/// `stream_output` and `stream_window` are in version 1.
const MIN_SCREENCAST: u32 = 1;
/// What wayland-protocols-plasma 0.3.12 knows (KWin 6.8 offers 21 and 6).
const MAX_WINDOW_MANAGEMENT: u32 = 18;
const MAX_SCREENCAST: u32 = 4;
/// `wl_output` 4: `name` and `description`.
const MAX_OUTPUT: u32 = 4;

/// `org_kde_plasma_window_management.state` bits.
const MINIMIZED: u32 = 0x2;
const SKIP_TASKBAR: u32 = 0x1000;
const SKIP_SWITCHER: u32 = 0x4_0000;

/// `zkde_screencast_unstable_v1.pointer`: a thumbnail without the cursor, a share with it drawn in (as X11's).
const POINTER_HIDDEN: u32 = 1;
const POINTER_EMBEDDED: u32 = 2;

/// The longest wait for KWin's `created` for a share.
const CREATE_WAIT: Duration = Duration::from_secs(5);
/// A thumbnail's whole wait: its stream's creation and its first frame (X11's `thumbnail` waits a second too).
const THUMB_WAIT: Duration = Duration::from_secs(1);
/// One turn of the capture loop at most: the stop flag, KWin's events and the share's ticks are looked at this often.
const POLL: Duration = Duration::from_millis(10);
/// The PipeWire node of Dilla's stream.
const NODE_NAME: &str = "dilla-screen-capture";

/// Whether a registry's globals (`interface`, `version`) hold both protocols, new enough.
pub(crate) fn offered<'a>(globals: impl IntoIterator<Item = (&'a str, u32)>) -> bool {
    let (mut windows, mut cast) = (false, false);
    for (name, version) in globals {
        match name {
            WINDOW_MANAGEMENT => windows |= version >= MIN_WINDOW_MANAGEMENT,
            SCREENCAST => cast |= version >= MIN_SCREENCAST,
            _ => {}
        }
    }
    windows && cast
}

/// Whether this session has KWin's window list and screencast for Dilla (Wayland only; asked once per process: a
/// registry round trip, binding nothing).
pub(super) fn available() -> bool {
    static AVAILABLE: OnceLock<bool> = OnceLock::new();
    *AVAILABLE.get_or_init(|| {
        // Without libpipewire the picker would list tiles it can never capture: the portal then.
        let found = probe() && crate::app_audio::linux::init().is_ok();
        log::info!(
            "dilla-media: KWin's window list and screencast {}",
            if found {
                "are offered: Dilla's own share picker"
            } else {
                "are not offered: the system's share dialog"
            }
        );
        found
    })
}

fn probe() -> bool {
    let Ok(conn) = Connection::connect_to_env() else {
        return false;
    };
    let mut queue = conn.new_event_queue();
    let _registry = conn.display().get_registry(&queue.handle(), ());
    let mut state = State::default();
    if queue.roundtrip(&mut state).is_err() {
        return false;
    }
    offered(state.globals.iter().map(|(_, n, v)| (n.as_str(), *v)))
}

/// A window as KWin's list describes it. `title` and `app_id` are shown only in the picker.
#[derive(Clone, Default)]
pub(crate) struct Win {
    pub uuid: String,
    pub title: String,
    pub app_id: String,
    pub pid: u32,
    pub state: u32,
    pub unmapped: bool,
}

/// A screen: its `wl_output` global and what it calls itself.
#[derive(Clone, Default)]
pub(crate) struct Screen {
    pub global: u32,
    pub name: String,
    pub description: String,
}

/// A window's id in a [`DesktopSource`]: a stable 64-bit hash (FNV-1a) of KWin's uuid for it, never shown.
pub(crate) fn window_id(uuid: &str) -> u64 {
    uuid.bytes().fold(0xcbf2_9ce4_8422_2325, |h, b| {
        (h ^ u64::from(b)).wrapping_mul(0x0100_0000_01b3)
    })
}

/// Whether a window is offered: mapped, not minimised, on the taskbar and in the switcher (so not a panel, the
/// desktop or an OSD), named by a title or an application id, and not of Dilla's own process tree (`own`, through
/// `parent_of`). A window whose process KWin does not know (`pid == 0`) cannot be Dilla's and is offered.
pub(crate) fn shareable(w: &Win, own: u32, parent_of: impl Fn(u32) -> Option<u32>) -> bool {
    !w.unmapped
        && w.state & (MINIMIZED | SKIP_TASKBAR | SKIP_SWITCHER) == 0
        && !(w.title.is_empty() && w.app_id.is_empty())
        && !(w.pid != 0 && crate::app_audio::in_tree(w.pid, own, parent_of))
}

/// The picker's sources: every screen (named by its description, else its connector), then every offered window
/// (named by its title, else its application id), in KWin's order.
pub(crate) fn to_sources(
    screens: &[Screen],
    windows: &[Win],
    own: u32,
    parent_of: impl Fn(u32) -> Option<u32>,
) -> Vec<DesktopSource> {
    let screens = screens.iter().map(|s| DesktopSource {
        kind: SourceKind::Screen,
        id: u64::from(s.global),
        title: if s.description.is_empty() {
            s.name.clone()
        } else {
            s.description.clone()
        },
        owner: None,
    });
    let windows = windows
        .iter()
        .filter(|w| shareable(w, own, &parent_of))
        .map(|w| DesktopSource {
            kind: SourceKind::Window,
            id: window_id(&w.uuid),
            title: if w.title.is_empty() {
                w.app_id.clone()
            } else {
                w.title.clone()
            },
            owner: None,
        });
    screens.chain(windows).collect()
}

/// The byte order of a negotiated video format.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum Layout {
    Bgra,
    Rgba,
}

impl Layout {
    fn of(format: spa::param::video::VideoFormat) -> Option<Layout> {
        use spa::param::video::VideoFormat as F;
        match format {
            F::BGRx | F::BGRA => Some(Layout::Bgra),
            F::RGBx | F::RGBA => Some(Layout::Rgba),
            _ => None,
        }
    }
}

/// One buffer's chunk (`bytes`, the mapped data; `offset`, `size` and `stride` as the chunk says) as a tightly packed
/// frame of `width` x `height`. `None` for an empty chunk (KWin sends one when only the cursor moved) or one too short
/// for the size. A stride of 0 or less means tightly packed.
pub(crate) fn convert(
    bytes: &[u8],
    (offset, size, stride): (usize, usize, i32),
    (width, height): (u32, u32),
    layout: Layout,
) -> Option<Frame> {
    let row = width as usize * 4;
    if width == 0 || height == 0 || size == 0 {
        return None;
    }
    let stride = usize::try_from(stride)
        .ok()
        .filter(|&s| s > 0)
        .unwrap_or(row);
    if stride < row {
        return None;
    }
    let chunk = bytes.get(offset..offset.checked_add(size)?)?;
    let needed = stride.checked_mul(height as usize - 1)?.checked_add(row)?;
    if chunk.len() < needed {
        return None;
    }
    let mut data = Vec::with_capacity(row * height as usize);
    for y in 0..height as usize {
        data.extend_from_slice(&chunk[y * stride..y * stride + row]);
    }
    Some(Frame {
        width,
        height,
        format: match layout {
            Layout::Bgra => PixelFormat::Bgra,
            Layout::Rgba => PixelFormat::Rgba,
        },
        stride: width * 4,
        data,
    })
}

/// The newest frame not handed on yet. Every buffer KWin sends is read into it (KWin keeps to the share's rate, see
/// [`enum_format`]), so the last frame of a burst is the one the loop hands on.
#[derive(Default)]
pub(crate) struct Newest(Option<Frame>);

impl Newest {
    pub(crate) fn put(&mut self, frame: Frame) {
        self.0 = Some(frame);
    }

    pub(crate) fn take(&mut self) -> Option<Frame> {
        self.0.take()
    }
}

/// The thumbnails of `sources`, one `one(source)` each, in order, until `cancelled` says the picker closed: the sources
/// not reached then come back without one, and no further stream opens.
pub(crate) fn thumbnails(
    sources: Vec<DesktopSource>,
    cancelled: &dyn Fn() -> bool,
    mut one: impl FnMut(&DesktopSource) -> Option<Frame>,
) -> Vec<(DesktopSource, Option<Frame>)> {
    let mut stopped = false;
    sources
        .into_iter()
        .map(|source| {
            stopped = stopped || cancelled();
            let thumbnail = if stopped { None } else { one(&source) };
            (source, thumbnail)
        })
        .collect()
}

/// The links from KWin's node (`from`, its output ports) to Dilla's stream (`to`, its input ports), from the ports the
/// registry listed (`port id -> (node id, is an output)`), in a stable order.
pub(crate) fn links(ports: &HashMap<u32, (u32, bool)>, from: u32, to: u32) -> Vec<(u32, u32)> {
    let mut outs: Vec<u32> = ports
        .iter()
        .filter(|(_, (node, output))| *node == from && *output)
        .map(|(id, _)| *id)
        .collect();
    let mut ins: Vec<u32> = ports
        .iter()
        .filter(|(_, (node, output))| *node == to && !*output)
        .map(|(id, _)| *id)
        .collect();
    outs.sort_unstable();
    ins.sort_unstable();
    outs.iter()
        .flat_map(|o| ins.iter().map(move |i| (*o, *i)))
        .collect()
}

// ---- the Wayland side ---------------------------------------------------------------------------------------------

/// What a screencast stream said.
#[derive(Default)]
struct Cast {
    node: Option<u32>,
    failed: bool,
    closed: bool,
}

#[derive(Default)]
struct State {
    globals: Vec<(u32, String, u32)>,
    screens: Vec<Screen>,
    windows: Vec<Win>,
    casts: Vec<Cast>,
}

impl Dispatch<wl_registry::WlRegistry, ()> for State {
    fn event(
        s: &mut Self,
        _: &wl_registry::WlRegistry,
        e: wl_registry::Event,
        _: &(),
        _: &Connection,
        _: &QueueHandle<Self>,
    ) {
        match e {
            wl_registry::Event::Global {
                name,
                interface,
                version,
            } => s.globals.push((name, interface, version)),
            wl_registry::Event::GlobalRemove { name } => {
                s.globals.retain(|g| g.0 != name);
                s.screens.retain(|o| o.global != name);
            }
            _ => {}
        }
    }
}

impl Dispatch<wl_output::WlOutput, u32> for State {
    fn event(
        s: &mut Self,
        _: &wl_output::WlOutput,
        e: wl_output::Event,
        global: &u32,
        _: &Connection,
        _: &QueueHandle<Self>,
    ) {
        let Some(screen) = s.screens.iter_mut().find(|o| o.global == *global) else {
            return;
        };
        match e {
            wl_output::Event::Name { name } => screen.name = name,
            wl_output::Event::Description { description } => screen.description = description,
            _ => {}
        }
    }
}

impl Dispatch<kwin_windows::OrgKdePlasmaWindowManagement, ()> for State {
    fn event(
        s: &mut Self,
        list: &kwin_windows::OrgKdePlasmaWindowManagement,
        e: kwin_windows::Event,
        _: &(),
        _: &Connection,
        qh: &QueueHandle<Self>,
    ) {
        if let kwin_windows::Event::WindowWithUuid { uuid, .. } = e {
            let index = s.windows.len();
            s.windows.push(Win {
                uuid: uuid.clone(),
                ..Win::default()
            });
            list.get_window_by_uuid(uuid, qh, index);
        }
    }
}

impl Dispatch<kwin_window::OrgKdePlasmaWindow, usize> for State {
    fn event(
        s: &mut Self,
        _: &kwin_window::OrgKdePlasmaWindow,
        e: kwin_window::Event,
        index: &usize,
        _: &Connection,
        _: &QueueHandle<Self>,
    ) {
        let Some(w) = s.windows.get_mut(*index) else {
            return;
        };
        match e {
            kwin_window::Event::TitleChanged { title } => w.title = title,
            kwin_window::Event::AppIdChanged { app_id } => w.app_id = app_id,
            kwin_window::Event::PidChanged { pid } => w.pid = pid,
            kwin_window::Event::StateChanged { flags } => w.state = flags,
            kwin_window::Event::Unmapped => w.unmapped = true,
            _ => {}
        }
    }
}

impl Dispatch<kwin_cast::ZkdeScreencastUnstableV1, ()> for State {
    fn event(
        _: &mut Self,
        _: &kwin_cast::ZkdeScreencastUnstableV1,
        _: kwin_cast::Event,
        _: &(),
        _: &Connection,
        _: &QueueHandle<Self>,
    ) {
    }
}

impl Dispatch<kwin_stream::ZkdeScreencastStreamUnstableV1, usize> for State {
    fn event(
        s: &mut Self,
        _: &kwin_stream::ZkdeScreencastStreamUnstableV1,
        e: kwin_stream::Event,
        slot: &usize,
        _: &Connection,
        _: &QueueHandle<Self>,
    ) {
        let Some(cast) = s.casts.get_mut(*slot) else {
            return;
        };
        match e {
            kwin_stream::Event::Created { node } => cast.node = Some(node),
            // KWin's message is translated prose about the source: not logged.
            kwin_stream::Event::Failed { .. } => cast.failed = true,
            kwin_stream::Event::Closed => cast.closed = true,
            _ => {}
        }
    }
}

/// What a stream captures.
enum Target {
    Screen(wl_output::WlOutput),
    Window(String),
}

/// One connection to KWin with its window list and screencast bound.
struct Session {
    conn: Connection,
    queue: EventQueue<State>,
    state: State,
    outputs: Vec<(u32, wl_output::WlOutput)>,
    cast: kwin_cast::ZkdeScreencastUnstableV1,
}

impl Session {
    /// Connects, binds both protocols and every output, and reads the window list. `None` when they are not offered.
    fn open() -> Option<Session> {
        let conn = Connection::connect_to_env().ok()?;
        let mut queue = conn.new_event_queue();
        let qh = queue.handle();
        let registry = conn.display().get_registry(&qh, ());
        let mut state = State::default();
        queue.roundtrip(&mut state).ok()?;
        if !offered(state.globals.iter().map(|(_, n, v)| (n.as_str(), *v))) {
            return None;
        }
        let mut outputs = Vec::new();
        let (mut windows, mut cast) = (None, None);
        for (name, interface, version) in state.globals.clone() {
            match interface.as_str() {
                "wl_output" => {
                    state.screens.push(Screen {
                        global: name,
                        ..Screen::default()
                    });
                    outputs.push((
                        name,
                        registry.bind::<wl_output::WlOutput, _, _>(
                            name,
                            version.min(MAX_OUTPUT),
                            &qh,
                            name,
                        ),
                    ));
                }
                WINDOW_MANAGEMENT if windows.is_none() => {
                    windows = Some(
                        registry.bind::<kwin_windows::OrgKdePlasmaWindowManagement, _, _>(
                            name,
                            version.min(MAX_WINDOW_MANAGEMENT),
                            &qh,
                            (),
                        ),
                    );
                }
                SCREENCAST if cast.is_none() => {
                    cast = Some(registry.bind::<kwin_cast::ZkdeScreencastUnstableV1, _, _>(
                        name,
                        version.min(MAX_SCREENCAST),
                        &qh,
                        (),
                    ));
                }
                _ => {}
            }
        }
        // The outputs' names and the windows' uuids, then each window's title, process and state.
        for _ in 0..3 {
            queue.roundtrip(&mut state).ok()?;
        }
        let _windows = windows?;
        Some(Session {
            conn,
            queue,
            state,
            outputs,
            cast: cast?,
        })
    }

    fn sources(&self) -> Vec<DesktopSource> {
        to_sources(
            &self.state.screens,
            &self.state.windows,
            std::process::id(),
            crate::app_audio::linux::parent,
        )
    }

    /// The offered window `id` (still offered now).
    fn window(&self, id: u64) -> Option<&Win> {
        let own = std::process::id();
        self.state.windows.iter().find(|w| {
            window_id(&w.uuid) == id && shareable(w, own, crate::app_audio::linux::parent)
        })
    }

    /// What `source` captures, when it is still there.
    fn target(&self, source: &DesktopSource) -> Option<Target> {
        match source.kind {
            SourceKind::Screen => self
                .outputs
                .iter()
                .find(|(global, _)| u64::from(*global) == source.id)
                .filter(|(global, _)| self.state.screens.iter().any(|s| s.global == *global))
                .map(|(_, output)| Target::Screen(output.clone())),
            SourceKind::Window => self
                .window(source.id)
                .map(|w| Target::Window(w.uuid.clone())),
        }
    }

    /// Asks KWin for a stream of `target`, waiting up to `wait` for its PipeWire node.
    fn stream(
        &mut self,
        target: &Target,
        pointer: u32,
        wait: Duration,
    ) -> Option<(kwin_stream::ZkdeScreencastStreamUnstableV1, usize, u32)> {
        let qh = self.queue.handle();
        let slot = self.state.casts.len();
        self.state.casts.push(Cast::default());
        let stream = match target {
            Target::Screen(output) => self.cast.stream_output(output, pointer, &qh, slot),
            Target::Window(uuid) => self.cast.stream_window(uuid.clone(), pointer, &qh, slot),
        };
        let deadline = Instant::now() + wait;
        loop {
            if self.queue.roundtrip(&mut self.state).is_err() {
                return None;
            }
            let cast = &self.state.casts[slot];
            if let Some(node) = cast.node.filter(|_| !cast.failed && !cast.closed) {
                return Some((stream, slot, node));
            }
            if cast.failed || cast.closed || Instant::now() >= deadline {
                log::warn!("dilla-media: KWin gave no screencast stream");
                stream.close();
                let _ = self.conn.flush();
                return None;
            }
            std::thread::sleep(Duration::from_millis(10));
        }
    }

    /// Reads and handles whatever KWin sent, without blocking. False when the connection broke.
    fn poll(&mut self) -> bool {
        if self.conn.flush().is_err() {
            return false;
        }
        if let Some(guard) = self.queue.prepare_read() {
            match guard.read() {
                Ok(_) => {}
                Err(WaylandError::Io(e)) if e.kind() == std::io::ErrorKind::WouldBlock => {}
                Err(_) => return false,
            }
        }
        self.queue.dispatch_pending(&mut self.state).is_ok()
    }
}

// ---- the PipeWire side --------------------------------------------------------------------------------------------

/// What the registry listed: every port (`id -> (node, is an output)`), and the ids removed since. PipeWire gives a
/// removed id to the next object (KWin's next stream often gets the last one's node id), so an id that comes back is
/// no longer removed.
#[derive(Default)]
struct Graph {
    ports: HashMap<u32, (u32, bool)>,
    removed: BTreeSet<u32>,
}

impl Graph {
    /// Dilla linked to `node` (its ports were listed, so it is the live node of that id): a removal still on record is
    /// an earlier object's.
    fn linked(&mut self, node: u32) {
        self.removed.remove(&node);
    }

    /// Whether KWin's `node` went away: a removal counts only once Dilla linked to it (`linked`), so a removal of a
    /// reused id that is still being delivered never ends a new capture (KWin's `closed` covers the time before).
    fn gone(&self, node: u32, linked: bool) -> bool {
        linked && self.removed.contains(&node)
    }
}

/// One PipeWire connection with the registry followed, for the thumbnails of one listing or for one share.
struct Pipe {
    mainloop: pw::main_loop::MainLoopRc,
    core: pw::core::CoreRc,
    graph: Rc<RefCell<Graph>>,
    _listener: pw::registry::Listener,
    _registry: pw::registry::RegistryRc,
    _context: pw::context::ContextRc,
}

impl Pipe {
    fn new() -> Option<Pipe> {
        crate::app_audio::linux::init().ok()?;
        let mainloop = pw::main_loop::MainLoopRc::new(None).ok()?;
        let context = pw::context::ContextRc::new(&mainloop, None).ok()?;
        let core = context.connect_rc(None).ok()?;
        let registry = core.get_registry_rc().ok()?;
        let graph = Rc::new(RefCell::new(Graph::default()));
        let (g, g_rm) = (graph.clone(), graph.clone());
        let listener = registry
            .add_listener_local()
            .global(move |global| {
                g.borrow_mut().removed.remove(&global.id);
                if global.type_ != ObjectType::Port {
                    return;
                }
                let Some(props) = global.props else { return };
                if let Some(node) = props.get("node.id").and_then(|n| n.parse::<u32>().ok()) {
                    let output = props.get("port.direction") == Some("out");
                    g.borrow_mut().ports.insert(global.id, (node, output));
                }
            })
            .global_remove(move |id| {
                let mut g = g_rm.borrow_mut();
                g.ports.remove(&id);
                g.removed.insert(id);
            })
            .register();
        Some(Pipe {
            mainloop,
            core,
            graph,
            _listener: listener,
            _registry: registry,
            _context: context,
        })
    }
}

/// What the capture loop hands its caller.
enum Step {
    Frame(Frame),
    /// The share's interval passed.
    Tick,
}

/// Why a capture ended.
#[derive(Debug, PartialEq, Eq)]
enum End {
    /// The caller said stop.
    Stopped,
    /// KWin closed the stream (the window or the output went away).
    Closed,
    /// No stream, or an error on either side.
    Failed,
}

/// The `EnumFormat` Dilla's stream offers: raw 8-bit RGB with alpha or padding in either order, any size, at most `fps`
/// frames a second (1 to 60: KWin holds back changes above it and sends the pending one later, so the final state
/// always arrives), no DMA-BUF modifier (so KWin hands over memory buffers that can be mapped).
fn enum_format(fps: u32) -> Option<Vec<u8>> {
    let fps = fps.clamp(1, 60);
    use spa::param::format::{FormatProperties, MediaSubtype, MediaType};
    use spa::param::video::VideoFormat;
    let obj = spa::pod::object!(
        spa::utils::SpaTypes::ObjectParamFormat,
        spa::param::ParamType::EnumFormat,
        spa::pod::property!(FormatProperties::MediaType, Id, MediaType::Video),
        spa::pod::property!(FormatProperties::MediaSubtype, Id, MediaSubtype::Raw),
        spa::pod::property!(
            FormatProperties::VideoFormat,
            Choice,
            Enum,
            Id,
            VideoFormat::BGRx,
            VideoFormat::BGRx,
            VideoFormat::BGRA,
            VideoFormat::RGBx,
            VideoFormat::RGBA,
        ),
        spa::pod::property!(
            FormatProperties::VideoSize,
            Choice,
            Range,
            Rectangle,
            spa::utils::Rectangle {
                width: 1920,
                height: 1080
            },
            spa::utils::Rectangle {
                width: 1,
                height: 1
            },
            spa::utils::Rectangle {
                width: 16384,
                height: 16384
            }
        ),
        spa::pod::property!(
            FormatProperties::VideoFramerate,
            Choice,
            Range,
            Fraction,
            spa::utils::Fraction { num: 30, denom: 1 },
            spa::utils::Fraction { num: 0, denom: 1 },
            spa::utils::Fraction {
                num: 1000,
                denom: 1
            }
        ),
        spa::pod::property!(
            FormatProperties::VideoMaxFramerate,
            Choice,
            Range,
            Fraction,
            spa::utils::Fraction { num: fps, denom: 1 },
            spa::utils::Fraction { num: 1, denom: 1 },
            spa::utils::Fraction { num: fps, denom: 1 }
        ),
    );
    spa::pod::serialize::PodSerializer::serialize(
        std::io::Cursor::new(Vec::new()),
        &spa::pod::Value::Object(obj),
    )
    .ok()
    .map(|(cursor, _)| cursor.into_inner())
}

/// Captures `target` until `on` says stop, KWin closes the stream, or an error. KWin sends at most `fps` frames a second
/// (renegotiated when `fps` changes) and each one is handed on; a [`Step::Tick`] comes every interval. KWin's stream and
/// Dilla's PipeWire stream (and with it its link) are closed before this returns.
fn capture(
    session: &mut Session,
    pipe: &Pipe,
    target: &Target,
    (pointer, wait): (u32, Duration),
    fps: &AtomicU32,
    mut on: impl FnMut(Step) -> bool,
) -> End {
    let Some((kwin, slot, node)) = session.stream(target, pointer, wait) else {
        return End::Failed;
    };
    let end = consume(session, pipe, slot, node, fps, &mut on);
    kwin.close();
    let _ = session.conn.flush();
    // Lets the stream's and the link's destruction reach the server before the next capture or the end.
    pipe.mainloop.loop_().iterate(Duration::ZERO);
    end
}

fn interval(fps: &AtomicU32) -> Duration {
    Duration::from_micros(1_000_000 / u64::from(fps.load(Ordering::Relaxed).clamp(1, 60)))
}

/// Dilla's side of one KWin stream (`node`): an input stream linked to it, read until the end.
fn consume(
    session: &mut Session,
    pipe: &Pipe,
    slot: usize,
    node: u32,
    fps: &AtomicU32,
    on: &mut impl FnMut(Step) -> bool,
) -> End {
    let Ok(stream) = pw::stream::StreamRc::new(
        pipe.core.clone(),
        NODE_NAME,
        pw::properties::properties! {
            *pw::keys::MEDIA_TYPE => "Video",
            *pw::keys::MEDIA_CATEGORY => "Capture",
            *pw::keys::MEDIA_ROLE => "Screen",
            *pw::keys::NODE_NAME => NODE_NAME,
            "node.autoconnect" => "false",
            "node.dont-reconnect" => "true",
        },
    ) else {
        return End::Failed;
    };
    let format = Rc::new(Cell::new(None::<(Layout, u32, u32)>));
    let pending = Rc::new(RefCell::new(Newest::default()));
    let own = Rc::new(Cell::new(None::<u32>));
    let failed = Rc::new(Cell::new(false));
    let every = Rc::new(Cell::new(interval(fps)));
    let listener = {
        let (format_p, format_f) = (format.clone(), format.clone());
        let (pending, own, failed) = (pending.clone(), own.clone(), failed.clone());
        stream
            .add_local_listener_with_user_data(())
            .state_changed(move |stream, _, _, state| match state {
                pw::stream::StreamState::Error(_) => failed.set(true),
                pw::stream::StreamState::Paused | pw::stream::StreamState::Streaming => {
                    own.set(Some(stream.node_id()));
                }
                _ => {}
            })
            .param_changed(move |_, _, id, param| {
                let Some(param) = param else { return };
                if id != spa::param::ParamType::Format.as_raw() {
                    return;
                }
                let mut info = spa::param::video::VideoInfoRaw::new();
                if info.parse(param).is_err() {
                    format_p.set(None);
                    return;
                }
                let size = info.size();
                format_p.set(Layout::of(info.format()).map(|l| (l, size.width, size.height)));
            })
            .process(move |stream, _| {
                // Every buffer is read (KWin keeps to the share's rate) and given back when dropped.
                let Some(mut buffer) = stream.dequeue_buffer() else {
                    return;
                };
                let Some((layout, w, h)) = format_f.get() else {
                    return;
                };
                let Some(data) = buffer.datas_mut().first_mut() else {
                    return;
                };
                let chunk = data.chunk();
                let (offset, size, stride) = (
                    chunk.offset() as usize,
                    chunk.size() as usize,
                    chunk.stride(),
                );
                if chunk.flags().contains(spa::buffer::ChunkFlags::CORRUPTED) {
                    return;
                }
                let Some(bytes) = data.data() else { return };
                if let Some(frame) = convert(bytes, (offset, size, stride), (w, h), layout) {
                    pending.borrow_mut().put(frame);
                }
            })
            .register()
    };
    let Ok(_listener) = listener else {
        return End::Failed;
    };
    let mut asked = fps.load(Ordering::Relaxed).clamp(1, 60);
    let Some(values) = enum_format(asked) else {
        return End::Failed;
    };
    let Some(pod) = spa::pod::Pod::from_bytes(&values) else {
        return End::Failed;
    };
    let mut params = [pod];
    if stream
        .connect(
            spa::utils::Direction::Input,
            None,
            pw::stream::StreamFlags::MAP_BUFFERS,
            &mut params,
        )
        .is_err()
    {
        return End::Failed;
    }

    let mut linked = BTreeSet::new();
    let mut made: Vec<pw::link::Link> = Vec::new();
    let mut next_tick = Instant::now() + every.get();
    let end = loop {
        pipe.mainloop.loop_().iterate(POLL);
        if !session.poll() {
            break End::Failed;
        }
        if session.state.casts[slot].closed || pipe.graph.borrow().gone(node, !made.is_empty()) {
            break End::Closed;
        }
        if failed.get() {
            break End::Failed;
        }
        if let Some(own) = own.get() {
            let wanted = links(&pipe.graph.borrow().ports, node, own);
            for (out_port, in_port) in wanted {
                if !linked.insert((out_port, in_port)) {
                    continue;
                }
                let props = pw::properties::properties! {
                    "link.output.node" => node.to_string(),
                    "link.output.port" => out_port.to_string(),
                    "link.input.node" => own.to_string(),
                    "link.input.port" => in_port.to_string(),
                    "object.linger" => "false",
                };
                match pipe
                    .core
                    .create_object::<pw::link::Link>("link-factory", &props)
                {
                    Ok(link) => {
                        pipe.graph.borrow_mut().linked(node);
                        made.push(link);
                    }
                    Err(_) => {
                        failed.set(true);
                        break;
                    }
                }
            }
        }
        let newest = pending.borrow_mut().take();
        if let Some(frame) = newest
            && !on(Step::Frame(frame))
        {
            break End::Stopped;
        }
        // The share's rate changed (its quality): KWin is asked for the new maximum.
        let now_fps = fps.load(Ordering::Relaxed).clamp(1, 60);
        if now_fps != asked
            && let Some(values) = enum_format(now_fps)
            && let Some(pod) = spa::pod::Pod::from_bytes(&values)
            && stream.update_params(&mut [pod]).is_ok()
        {
            asked = now_fps;
        }
        let now = Instant::now();
        if now >= next_tick {
            every.set(interval(fps));
            next_tick = now + every.get();
            if !on(Step::Tick) {
                break End::Stopped;
            }
        }
    };
    let _ = stream.disconnect();
    drop(made);
    end
}

// ---- what sources/desktop.rs calls ---------------------------------------------------------------------------------

/// The screens or the windows that can be shared now.
pub(super) fn list(kind: SourceKind) -> Vec<DesktopSource> {
    Session::open()
        .map(|s| s.sources())
        .unwrap_or_default()
        .into_iter()
        .filter(|s| s.kind == kind)
        .collect()
}

/// The picker's sources without thumbnails (one round trip to KWin): what it shows while the thumbnails come.
pub(super) fn titles(max: usize) -> Snapshot {
    Snapshot {
        sources: Session::open()
            .map(|s| s.sources())
            .unwrap_or_default()
            .into_iter()
            .take(max)
            .map(|s| (s, None))
            .collect(),
        denied: false,
    }
}

/// The picker's enumeration: every screen, then every offered window, at most `max`, each with a thumbnail of one
/// frame from a stream that closes before the next one opens (one connection to KWin and to PipeWire for all of them).
/// Once `cancelled` answers true (the picker closed) no further stream opens, and a stream waiting for its frame stops.
pub(super) fn snapshot(
    max: usize,
    max_w: u32,
    max_h: u32,
    cancelled: &dyn Fn() -> bool,
) -> Snapshot {
    let Some(mut session) = Session::open() else {
        return Snapshot::default();
    };
    let sources: Vec<DesktopSource> = session.sources().into_iter().take(max).collect();
    let pipe = Pipe::new();
    if pipe.is_none() {
        log::warn!("dilla-media: no PipeWire for the share picker's thumbnails");
    }
    let sources = thumbnails(sources, cancelled, |source| {
        pipe.as_ref()
            .and_then(|pipe| one_frame(&mut session, pipe, source, (max_w, max_h), cancelled))
    });
    log::debug!(
        "dilla-media: KWin offered {} screens and windows, {} with a thumbnail",
        sources.len(),
        sources.iter().filter(|(_, t)| t.is_some()).count()
    );
    Snapshot {
        sources,
        denied: false,
    }
}

/// One frame of `source`, scaled to fit `max_w` x `max_h`; `None` when it gives none within [`THUMB_WAIT`].
pub(super) fn thumbnail(source: &DesktopSource, max_w: u32, max_h: u32) -> Option<Frame> {
    let mut session = Session::open()?;
    let pipe = Pipe::new()?;
    one_frame(&mut session, &pipe, source, (max_w, max_h), &|| false)
}

fn one_frame(
    session: &mut Session,
    pipe: &Pipe,
    source: &DesktopSource,
    (max_w, max_h): (u32, u32),
    cancelled: &dyn Fn() -> bool,
) -> Option<Frame> {
    let target = session.target(source)?;
    let deadline = Instant::now() + THUMB_WAIT;
    let fps = AtomicU32::new(10);
    let mut got = None;
    capture(
        session,
        pipe,
        &target,
        (POINTER_HIDDEN, THUMB_WAIT),
        &fps,
        |step| match step {
            Step::Frame(frame) => {
                got = Some(fit_rgba(&frame, max_w, max_h));
                false
            }
            Step::Tick => Instant::now() < deadline && !cancelled(),
        },
    );
    got
}

/// The process of the offered window `source`, asked of KWin's window list now (KWin knows a Wayland client's process
/// from its socket); `None` for a screen, a window that is gone, or one whose process KWin does not know.
pub(super) fn owner(source: &DesktopSource) -> Option<u32> {
    if source.kind != SourceKind::Window {
        return None;
    }
    let session = Session::open()?;
    session
        .window(source.id)
        .map(|w| w.pid)
        .filter(|&pid| pid > 1)
}

/// The capture thread of a share of `source`: until `stop`, KWin closing the stream, or an error; `latest` ends then.
/// Only new frames go to `latest`: the feeder repeats its last one while KWin sends none.
pub(super) fn run(source: &DesktopSource, latest: &Latest, stop: &AtomicBool, fps: &AtomicU32) {
    let started = Session::open().and_then(|session| {
        let target = session.target(source)?;
        Some((session, target, Pipe::new()?))
    });
    let Some((mut session, target, pipe)) = started else {
        log::warn!("dilla-media: the chosen screen or window could not be captured");
        latest.end();
        return;
    };
    let end = capture(
        &mut session,
        &pipe,
        &target,
        (POINTER_EMBEDDED, CREATE_WAIT),
        fps,
        |step| {
            if stop.load(Ordering::Acquire) {
                return false;
            }
            if let Step::Frame(frame) = step {
                latest.put(frame);
            }
            true
        },
    );
    if end != End::Stopped {
        log::info!("dilla-media: the shared screen or window ended ({end:?})");
    }
    latest.end();
}

#[cfg(test)]
mod tests {
    use super::*;

    fn win(uuid: &str, title: &str, pid: u32, state: u32) -> Win {
        Win {
            uuid: uuid.to_owned(),
            title: title.to_owned(),
            app_id: "org.example.app".to_owned(),
            pid,
            state,
            unmapped: false,
        }
    }

    /// The own picker needs both globals, each new enough; one of them, or an older window list, is the portal.
    #[test]
    fn both_protocols_new_enough_or_the_portal() {
        let kwin_68 = [
            ("wl_compositor", 6),
            (WINDOW_MANAGEMENT, 21),
            ("wl_output", 4),
            (SCREENCAST, 6),
        ];
        assert!(offered(kwin_68));
        assert!(offered([(WINDOW_MANAGEMENT, 13), (SCREENCAST, 1)]));
        assert!(!offered([(WINDOW_MANAGEMENT, 21)]), "no screencast");
        assert!(!offered([(SCREENCAST, 6)]), "no window list");
        assert!(
            !offered([(WINDOW_MANAGEMENT, 12), (SCREENCAST, 6)]),
            "a window list without uuids"
        );
        assert!(!offered([(SCREENCAST, 0), (WINDOW_MANAGEMENT, 18)]));
        assert!(!offered([]), "GNOME, or a sandboxed client");
    }

    /// Screens first, named by their description; then the windows a person would pick: not Dilla's own process tree,
    /// not a panel or the desktop (skip-taskbar, skip-switcher), not minimised or unmapped, not nameless.
    #[test]
    fn the_picker_offers_screens_then_other_people_s_windows() {
        let own = 100;
        // 101 is Dilla's child (its audio helper, say); 4000's parent is init.
        let parent_of = |pid: u32| match pid {
            101 => Some(100),
            4000 | 100 => Some(1),
            _ => None,
        };
        let screens = [
            Screen {
                global: 7,
                name: "DP-1".to_owned(),
                description: "Dell U2720Q".to_owned(),
            },
            Screen {
                global: 9,
                name: "HDMI-A-1".to_owned(),
                description: String::new(),
            },
        ];
        let mut unmapped = win("u-unmapped", "Gone", 4000, 0);
        unmapped.unmapped = true;
        let mut nameless = win("u-nameless", "", 4000, 0);
        nameless.app_id.clear();
        let mut untitled = win("u-untitled", "", 4000, 0);
        untitled.app_id = "org.kde.kwrite".to_owned();
        let windows = [
            win("u-editor", "Editor", 4000, 0x100 | 0x1),
            win("u-dilla", "Dilla", own, 0),
            win("u-dilla-child", "Dilla helper", 101, 0),
            win("u-panel", "Panel", 4000, SKIP_TASKBAR | SKIP_SWITCHER),
            win("u-desktop", "Desktop", 4000, SKIP_TASKBAR),
            win("u-osd", "OSD", 4000, SKIP_SWITCHER),
            win("u-min", "Minimised", 4000, MINIMIZED),
            unmapped,
            nameless,
            untitled,
            win("u-nopid", "No pid", 0, 0),
        ];
        let sources = to_sources(&screens, &windows, own, parent_of);
        let kinds: Vec<SourceKind> = sources.iter().map(|s| s.kind).collect();
        assert_eq!(
            kinds,
            [
                SourceKind::Screen,
                SourceKind::Screen,
                SourceKind::Window,
                SourceKind::Window,
                SourceKind::Window
            ]
        );
        assert_eq!((sources[0].id, sources[1].id), (7, 9));
        assert_eq!(sources[0].title, "Dell U2720Q");
        assert_eq!(
            sources[1].title, "HDMI-A-1",
            "no description: the connector"
        );
        let ids: Vec<u64> = sources[2..].iter().map(|s| s.id).collect();
        assert_eq!(
            ids,
            [
                window_id("u-editor"),
                window_id("u-untitled"),
                window_id("u-nopid")
            ]
        );
        assert_eq!(sources[3].title, "org.kde.kwrite", "untitled: the app id");
        assert!(sources.iter().all(|s| s.owner.is_none()));
    }

    #[test]
    fn a_window_s_id_is_a_stable_hash_of_its_uuid() {
        let a = window_id("{6a1a0b2c-0000-4000-8000-000000000001}");
        assert_eq!(a, window_id("{6a1a0b2c-0000-4000-8000-000000000001}"));
        assert_ne!(a, window_id("{6a1a0b2c-0000-4000-8000-000000000002}"));
        assert_eq!(
            window_id(""),
            0xcbf2_9ce4_8422_2325,
            "FNV-1a's offset basis"
        );
    }

    /// A padded chunk becomes a tightly packed frame in the negotiated byte order; an empty chunk (a cursor-only
    /// update), a short one or a stride under a row is no frame.
    #[test]
    fn a_buffer_becomes_a_packed_frame() {
        // 2 x 2 BGRx at a stride of 12 (4 bytes of padding per row), 3 bytes into the buffer.
        let mut bytes = vec![0xee; 3];
        bytes.extend_from_slice(&[1, 2, 3, 0, 4, 5, 6, 0, 0xaa, 0xaa, 0xaa, 0xaa]);
        bytes.extend_from_slice(&[7, 8, 9, 0, 10, 11, 12, 0]);
        let size = bytes.len() - 3;
        let f = convert(&bytes, (3, size, 12), (2, 2), Layout::Bgra).expect("a frame");
        assert_eq!((f.width, f.height, f.stride), (2, 2, 8));
        assert_eq!(f.format, PixelFormat::Bgra);
        assert!(f.is_whole());
        assert_eq!(
            f.to_rgba(),
            [3, 2, 1, 255, 6, 5, 4, 255, 9, 8, 7, 255, 12, 11, 10, 255]
        );
        let rgba = convert(&bytes, (3, size, 12), (2, 2), Layout::Rgba).expect("a frame");
        assert_eq!(rgba.format, PixelFormat::Rgba);
        let packed = convert(&bytes[3..11], (0, 8, 0), (2, 1), Layout::Bgra).expect("stride 0");
        assert_eq!(packed.stride, 8);
        assert!(
            convert(&bytes, (3, 0, 12), (2, 2), Layout::Bgra).is_none(),
            "empty"
        );
        assert!(
            convert(&bytes, (3, size - 1, 12), (2, 2), Layout::Bgra).is_none(),
            "short"
        );
        assert!(
            convert(&bytes, (3, size, 4), (2, 2), Layout::Bgra).is_none(),
            "stride < row"
        );
        assert!(
            convert(&bytes, (30, size, 12), (2, 2), Layout::Bgra).is_none(),
            "past the end"
        );
        assert!(
            convert(&bytes, (3, size, 12), (0, 2), Layout::Bgra).is_none(),
            "no width"
        );
    }

    /// Review I1: every buffer KWin sends is read (KWin throttles to the share's rate itself), so the last frame of a
    /// burst, the finished slide, is the one handed on; nothing is dropped for coming early.
    #[test]
    fn the_last_frame_of_a_burst_is_delivered() {
        let mut n = Newest::default();
        assert!(n.take().is_none());
        for w in 1..=3 {
            n.put(Frame::rgba(w, 1, vec![0; (w * 4) as usize]));
        }
        assert_eq!(n.take().map(|f| f.width), Some(3), "the burst's last frame");
        assert!(n.take().is_none(), "handed on once");
    }

    /// The maximum frame rate of the format Dilla asks KWin for (so KWin holds back changes above it and sends the
    /// pending one later): the share's rate, 1 to 60.
    fn max_framerate(bytes: &[u8]) -> Option<(u32, u32, u32)> {
        use spa::pod::deserialize::PodDeserializer;
        use spa::pod::{ChoiceValue, Value};
        use spa::utils::{Choice, ChoiceEnum};
        let (_, value) = PodDeserializer::deserialize_any_from(bytes).ok()?;
        let Value::Object(obj) = value else {
            return None;
        };
        let key = spa::param::format::FormatProperties::VideoMaxFramerate.as_raw();
        obj.properties
            .iter()
            .find(|p| p.key == key)
            .and_then(|p| match &p.value {
                Value::Choice(ChoiceValue::Fraction(Choice(
                    _,
                    ChoiceEnum::Range { default, min, max },
                ))) => Some((default.num, min.num, max.num)),
                _ => None,
            })
    }

    #[test]
    fn kwin_is_asked_for_at_most_the_share_s_rate() {
        assert_eq!(max_framerate(&enum_format(30).unwrap()), Some((30, 1, 30)));
        assert_eq!(max_framerate(&enum_format(15).unwrap()), Some((15, 1, 15)));
        assert_eq!(max_framerate(&enum_format(0).unwrap()), Some((1, 1, 1)));
        assert_eq!(max_framerate(&enum_format(240).unwrap()), Some((60, 1, 60)));
    }

    /// Review I2: a listing whose picker closed opens no further stream: the thumbnails stop at the cancel, and the
    /// sources not reached come back without one.
    #[test]
    fn a_cancelled_listing_opens_no_more_streams() {
        let sources: Vec<DesktopSource> = (0..5)
            .map(|id| DesktopSource {
                kind: SourceKind::Window,
                id,
                title: String::new(),
                owner: None,
            })
            .collect();
        let opened = Cell::new(0);
        let listed = thumbnails(sources.clone(), &|| opened.get() >= 2, |_| {
            opened.set(opened.get() + 1);
            Some(Frame::rgba(1, 1, vec![0; 4]))
        });
        assert_eq!(opened.get(), 2, "no stream after the cancel");
        assert_eq!(listed.len(), 5);
        assert_eq!(listed.iter().filter(|(_, t)| t.is_some()).count(), 2);
        let none = thumbnails(sources, &|| true, |_| panic!("a stream after the cancel"));
        assert!(none.iter().all(|(_, t)| t.is_none()));
    }

    /// Review m1: a removal of KWin's node counts only once Dilla linked to it, so a removal of the same id that was an
    /// earlier stream's (PipeWire reuses ids) never ends a new capture.
    #[test]
    fn only_a_removal_after_the_link_ends_the_capture() {
        let mut g = Graph::default();
        g.removed.insert(203);
        assert!(
            !g.gone(203, false),
            "an earlier stream's removal, not linked yet"
        );
        g.linked(203);
        assert!(
            !g.gone(203, true),
            "the stale removal was cleared at the link"
        );
        g.removed.insert(203);
        assert!(g.gone(203, true), "removed after the link");
    }

    /// KWin's node's output ports go to Dilla's stream's input ports only: never another node's, never the wrong way.
    #[test]
    fn only_kwin_s_output_is_linked_to_dilla_s_input() {
        let ports = HashMap::from([
            (31, (203, true)),
            (32, (203, false)),
            (41, (300, false)),
            (42, (300, true)),
            (51, (400, true)),
        ]);
        assert_eq!(links(&ports, 203, 300), [(31, 41)]);
        assert!(
            links(&ports, 203, 999).is_empty(),
            "our ports are not listed yet"
        );
        assert!(links(&ports, 999, 300).is_empty());
    }
}
