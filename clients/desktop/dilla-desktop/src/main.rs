fn main() {
    #[cfg(feature = "automation")]
    if std::env::args().any(|arg| arg == "--automation") {
        dilla_desktop::automation::run(std::env::args().collect());
        return;
    }
    // The icons are served from the binary (dilla-desktop-ui theme::Assets).
    gpui_platform::application()
        .with_assets(dilla_desktop_ui::theme::Assets)
        .run(|cx| {
            // The app lives exactly as long as its window.
            cx.on_window_closed(|cx, _| {
                if cx.windows().is_empty() {
                    cx.quit();
                }
            })
            .detach();
            // Videos handed to the system player are wiped when the app quits (Task 12).
            cx.on_app_quit(|cx| {
                dilla_desktop::screens::attachments::ui::on_quit(cx);
                async {}
            })
            .detach();
            dilla_desktop::open_main_window(cx).expect("the main window opens");
            cx.activate(true);
        });
}
