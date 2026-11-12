use gpui::{TestAppContext, VisualTestContext};

#[gpui::test]
fn opens_and_closes(cx: &mut TestAppContext) {
    let handle = cx
        .update(dilla_desktop::open_main_window)
        .expect("the main window opens");
    assert_eq!(cx.windows().len(), 1);

    let mut window_cx = VisualTestContext::from_window(handle.into(), cx);
    assert_eq!(window_cx.window_title().as_deref(), Some("Dilla Preview"));

    handle
        .update(cx, |_, window, _| window.remove_window())
        .expect("the window is still open");
    assert!(cx.windows().is_empty(), "no window remains after the close");
}
