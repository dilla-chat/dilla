use gpui::{Bounds, TestAppContext, VisualTestContext, point, px, size};

#[gpui::test]
fn opens_and_closes(cx: &mut TestAppContext) {
    let handle = cx
        .update(|cx| dilla_desktop::open_main_window_with(cx, dilla_desktop::Launch::empty()))
        .expect("the main window opens");
    assert_eq!(cx.windows().len(), 1);

    let mut window_cx = VisualTestContext::from_window(handle.into(), cx);
    assert_eq!(window_cx.window_title().as_deref(), Some("Dilla"));

    handle
        .update(cx, |_, window, _| window.remove_window())
        .expect("the window is still open");
    assert!(cx.windows().is_empty(), "no window remains after the close");
}

/// The title is set once, by `set_window_title` as the window is built: the window options carry none, so the platform
/// never receives a second, earlier title (foundations Task 1 minor).
#[test]
fn window_title_is_set_once_through_set_window_title() {
    let bounds = Bounds::new(point(px(0.), px(0.)), size(px(1280.), px(800.)));
    let options = dilla_desktop::main_window_options(bounds);
    let title = options.titlebar.as_ref().and_then(|t| t.title.clone());
    assert_eq!(title, None, "the title comes only from set_window_title");
    assert_eq!(
        options.app_id.as_deref(),
        Some(dilla_desktop::window_app_id())
    );
}
