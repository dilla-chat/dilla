//! The preview desktop client: one window, nothing in it yet.

#[cfg(feature = "automation")]
pub mod automation;

use gpui::{
    App, Bounds, Context, IntoElement, Render, Result, TitlebarOptions, Window, WindowBounds,
    WindowHandle, WindowOptions, div, prelude::*, px, size,
};

/// The window title and product name of the preview build.
pub const PRODUCT_NAME: &str = "Dilla Preview";
/// The application id (Wayland app id, desktop-entry name) of the preview build.
pub const APP_ID: &str = "dev.dilla.desktop.preview";

/// The empty root view of the main window.
pub struct MainView;

impl Render for MainView {
    fn render(&mut self, _window: &mut Window, _cx: &mut Context<Self>) -> impl IntoElement {
        tracked("root", div().size_full())
    }
}

/// Gives `child` the element id `id`, which the test-only automation interface looks up by name.
/// Without the `automation` feature this is a plain id-carrying wrapper.
pub fn tracked(id: &'static str, child: impl IntoElement) -> impl IntoElement {
    let wrapper = div().id(id).relative().size_full().child(child);
    #[cfg(feature = "automation")]
    let wrapper = wrapper.child(automation::bounds_probe(id));
    wrapper
}

/// Opens the main window: 1280x800, titled [`PRODUCT_NAME`].
pub fn open_main_window(cx: &mut App) -> Result<WindowHandle<MainView>> {
    let bounds = Bounds::centered(None, size(px(1280.), px(800.)), cx);
    cx.open_window(
        WindowOptions {
            window_bounds: Some(WindowBounds::Windowed(bounds)),
            titlebar: Some(TitlebarOptions {
                title: Some(PRODUCT_NAME.into()),
                ..Default::default()
            }),
            app_id: Some(APP_ID.to_owned()),
            ..Default::default()
        },
        |window, cx| {
            window.set_window_title(PRODUCT_NAME);
            cx.new(|_| MainView)
        },
    )
}
