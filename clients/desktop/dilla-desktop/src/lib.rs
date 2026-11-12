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
pub mod router;
pub mod screens;
pub mod slots;
pub mod strings;
pub mod windows;

use gpui::{
    App, Bounds, Global, IntoElement, Pixels, Result, TitlebarOptions, WindowBounds, WindowHandle,
    WindowOptions, div, prelude::*, px, size,
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

/// Opens the main window on the OS data dir and keychain: 1280x800, titled [`PRODUCT_NAME`].
pub fn open_main_window(cx: &mut App) -> Result<WindowHandle<AppView>> {
    open_main_window_with(cx, Launch::os())
}

/// Opens the main window for `launch`.
pub fn open_main_window_with(cx: &mut App, launch: Launch) -> Result<WindowHandle<AppView>> {
    install(cx);
    let (width, height) = launch.window;
    let bounds = Bounds::centered(None, size(px(width), px(height)), cx);
    cx.open_window(main_window_options(bounds), |window, cx| {
        window.set_window_title(PRODUCT_NAME);
        cx.new(|cx| AppView::new(launch, window, cx))
    })
}

/// The main window's options at `bounds`. They carry no title: the window sets it once, through `set_window_title`, as
/// it is built.
pub fn main_window_options(bounds: Bounds<Pixels>) -> WindowOptions {
    WindowOptions {
        window_bounds: Some(WindowBounds::Windowed(bounds)),
        titlebar: Some(TitlebarOptions {
            title: None,
            ..Default::default()
        }),
        app_id: Some(APP_ID.to_owned()),
        ..Default::default()
    }
}
