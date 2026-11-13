//! The preview desktop client: sign in with a password and an authenticator code, then the Signal shell with the
//! servers, the channels, the timeline and the composer (the vertical slice, Task 12).

pub mod app;
#[cfg(feature = "automation")]
pub mod automation;
pub mod edit_state;
#[cfg(feature = "automation")]
pub mod fixture;
pub mod format;
pub mod keys;
pub mod release;
pub mod router;
pub mod screens;
pub mod slots;
pub mod strings;
pub mod windows;

use dilla_client::prefs::Prefs;
use gpui::{
    App, Bounds, Global, IntoElement, Pixels, Result, TitlebarOptions, WindowBounds, WindowHandle,
    WindowOptions, div, point, prelude::*, px, size,
};
use screens::shell::window::{
    SavedBounds, Screen, WORKSPACE_MIN, WindowMode, restore_bounds, window_shape,
};

pub use app::{AppView, Launch};

/// The window title and product name of the preview build.
pub const PRODUCT_NAME: &str = "Dilla Preview";
/// The application id (Wayland app id, desktop-entry name) of the preview build.
pub const APP_ID: &str = "dev.dilla.desktop.preview";

/// Gives `child` the element id `id`, which the test-only automation interface looks up by name.
/// Without the `automation` feature this is a plain id-carrying wrapper.
pub fn tracked(id: &'static str, child: impl IntoElement) -> impl IntoElement {
    let wrapper = div().id(id).relative().size_full().child(child);
    #[cfg(feature = "automation")]
    let wrapper = wrapper.child(automation::bounds_probe(id));
    wrapper
}

/// [`tracked`] for an element that keeps its own size (a button, a field, a row): the wrapper takes the child's size
/// instead of filling its parent.
pub fn tracked_box(id: &'static str, child: impl IntoElement) -> impl IntoElement {
    let wrapper = div().id(id).relative().flex_none().child(child);
    #[cfg(feature = "automation")]
    let wrapper = wrapper.child(automation::bounds_probe(id));
    wrapper
}

/// [`tracked_box`] for an element that owns the focus handle `handle`: the automation interface's `focused` command
/// answers `id` while `handle` holds the focus. Without the `automation` feature the handle is not used.
pub fn tracked_focus<C: IntoElement>(
    id: &'static str,
    handle: &gpui::FocusHandle,
    child: C,
) -> impl IntoElement + use<C> {
    let wrapper = div().id(id).relative().flex_none().child(child);
    #[cfg(feature = "automation")]
    let wrapper = wrapper
        .child(automation::bounds_probe(id))
        .child(automation::focus_probe(id, handle));
    #[cfg(not(feature = "automation"))]
    let _ = handle;
    wrapper
}

struct Installed;
impl Global for Installed {}

/// Registers the fonts and the component library's key bindings, once per app.
pub fn install(cx: &mut App) {
    if cx.has_global::<Installed>() {
        return;
    }
    cx.set_global(Installed);
    dilla_desktop_ui::theme::install(cx);
    gpui_component::init(cx);
    keys::bind_all(cx);
}

/// Opens the main window on the OS data dir and keychain: the start window (400x280), which takes the authentication and
/// workspace shapes as the screens change ([`screens::shell::window`]), titled [`PRODUCT_NAME`].
pub fn open_main_window(cx: &mut App) -> Result<WindowHandle<AppView>> {
    open_main_window_with(cx, Launch::os())
}

/// Opens the main window for `launch`: a shipping launch starts in the start shape, any other at `launch.window`.
pub fn open_main_window_with(cx: &mut App, launch: Launch) -> Result<WindowHandle<AppView>> {
    let mode = match (launch.shipping, launch.handoff.is_some()) {
        (false, _) => None,
        (true, true) => Some(WindowMode::Workspace),
        (true, false) => Some(WindowMode::Starting),
    };
    match mode {
        Some(mode) => open_window_in(cx, launch, mode),
        None => {
            install(cx);
            let (width, height) = launch.window;
            let bounds = Bounds::centered(None, size(px(width), px(height)), cx);
            cx.open_window(main_window_options(bounds), |window, cx| {
                window.set_window_title(PRODUCT_NAME);
                cx.new(|cx| AppView::new(launch, window, cx))
            })
        }
    }
}

/// The height of the largest display's work area, logical px (1080 where the platform names no display).
pub fn work_area_height(cx: &App) -> f32 {
    cx.primary_display()
        .map_or(1080.0, |d| f32::from(d.bounds().size.height))
}

/// Opens the shipping window in `mode`: the shape of `window_shape`, and for the workspace the bounds the app kept
/// (`restore_bounds`).
pub fn open_window_in(
    cx: &mut App,
    launch: Launch,
    mode: WindowMode,
) -> Result<WindowHandle<AppView>> {
    install(cx);
    let shape = window_shape(mode, None, work_area_height(cx));
    let screens: Vec<Screen> = cx
        .displays()
        .iter()
        .map(|d| {
            let b = d.bounds();
            Screen {
                x: f32::from(b.origin.x),
                y: f32::from(b.origin.y),
                width: f32::from(b.size.width),
                height: f32::from(b.size.height),
            }
        })
        .collect();
    let saved = (mode == WindowMode::Workspace)
        .then(|| launch.root.as_deref().map(Prefs::app))
        .flatten()
        .and_then(|prefs| SavedBounds::read(&prefs));
    let (width, height, origin) = restore_bounds(saved, shape, &screens);
    let bounds = match origin {
        Some((x, y)) => Bounds::new(point(px(x), px(y)), size(px(width), px(height))),
        None => Bounds::centered(None, size(px(width), px(height)), cx),
    };
    let options = window_options_for(mode, bounds);
    cx.open_window(options, |window, cx| {
        window.set_window_title(PRODUCT_NAME);
        cx.new(|cx| AppView::new(launch, window, cx))
    })
}

/// The options of the shipping window in `mode` at `bounds`: the shape's minimum size (the workspace cannot be resized
/// below 900x600; the compact windows are fixed), and the app's own title bar (no OS frame where the window can be drawn
/// without one; macOS keeps its traffic lights, inset into the bar).
pub fn window_options_for(mode: WindowMode, bounds: Bounds<Pixels>) -> WindowOptions {
    let shape = window_shape(mode, None, bounds.size.height.into());
    let mut options = main_window_options(bounds);
    options.window_min_size = Some(size(px(shape.min_width), px(shape.min_height)));
    options.is_resizable = shape.resizable;
    if let Some(titlebar) = options.titlebar.as_mut() {
        titlebar.appears_transparent = true;
        #[cfg(target_os = "macos")]
        {
            titlebar.traffic_light_position = Some(point(px(18.0), px(14.0)));
        }
    }
    #[cfg(not(target_os = "macos"))]
    {
        options.window_decorations = Some(gpui::WindowDecorations::Client);
    }
    options
}

/// The main window's options at `bounds`: the workspace's minimum size (900x600). They carry no title: the window sets
/// it once, through `set_window_title`, as it is built.
pub fn main_window_options(bounds: Bounds<Pixels>) -> WindowOptions {
    WindowOptions {
        window_bounds: Some(WindowBounds::Windowed(bounds)),
        titlebar: Some(TitlebarOptions {
            title: None,
            ..Default::default()
        }),
        app_id: Some(APP_ID.to_owned()),
        window_min_size: Some(size(px(WORKSPACE_MIN.0), px(WORKSPACE_MIN.1))),
        ..Default::default()
    }
}
