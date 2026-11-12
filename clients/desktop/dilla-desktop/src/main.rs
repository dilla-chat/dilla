fn main() {
    gpui_platform::application().run(|cx| {
        // The app lives exactly as long as its window.
        cx.on_window_closed(|cx, _| {
            if cx.windows().is_empty() {
                cx.quit();
            }
        })
        .detach();
        dilla_desktop::open_main_window(cx).expect("the main window opens");
        cx.activate(true);
    });
}
