//! The preview desktop client: one window, nothing in it yet.

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
        div().size_full()
    }
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
