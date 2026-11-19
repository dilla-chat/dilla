//! The preview desktop client: sign in with a password and an authenticator code, then the Signal shell with the
//! servers, the channels, the timeline and the composer (the vertical slice, Task 12).

pub mod app;
#[cfg(feature = "automation")]
pub mod automation;
pub mod edit_state;
#[cfg(feature = "automation")]
pub mod fixture;
pub mod format;
pub mod fullscreen;
pub mod keys;
pub mod logging;
pub mod native;
pub mod per_window;
pub mod print;
pub mod release;
pub mod router;
pub mod screens;
pub mod slots;
pub mod strings;
pub mod updater;
pub mod windows;

use dilla_client::prefs::Prefs;
use gpui::{
    App, Bounds, Global, IntoElement, Pixels, Result, TitlebarOptions, WindowBounds, WindowHandle,
    WindowOptions, div, point, prelude::*, px, size,
};
use screens::shell::window::{
    SavedBounds, Screen, WORKSPACE_MIN, WindowMode, opening_mode, restore_bounds, window_shape,
};

pub use app::{AppView, Launch};

/// The window title and product name: plain "Dilla", as beta.5 (release plan D3). The data root keeps its own name
/// ([`dilla_client::instances::PRODUCT_DIR`]).
pub const PRODUCT_NAME: &str = "Dilla";
/// The application id on macOS (bundle id) and Windows (AppUserModelID): beta.5's id, since the release replaces it
/// (release plan D3). The data root and the keychain service keep their preview names, so GPUI data never mixes with
/// Electron's. Linux uses [`LINUX_APP_ID`].
pub const APP_ID: &str = "dev.dilla.desktop";
/// The application id on Linux: the Wayland app id, the desktop entry's name (`chat.dilla.Dilla.desktop`) and the
/// packages' `StartupWMClass`. It must not end in `.desktop`: KDE strips that suffix before it looks the entry up.
pub const LINUX_APP_ID: &str = "chat.dilla.Dilla";

/// The id a window and the notifications carry: [`LINUX_APP_ID`] on Linux, [`APP_ID`] elsewhere.
pub fn window_app_id() -> &'static str {
    if cfg!(target_os = "linux") {
        LINUX_APP_ID
    } else {
        APP_ID
    }
}

/// Gives `child` the element id `id`, which the test-only automation interface looks up by name.
/// Without the `automation` feature this is a plain id-carrying wrapper.
pub fn tracked(id: &'static str, child: impl IntoElement) -> impl IntoElement {
    let wrapper = div()
        .id(id)
        .debug_selector(move || id.to_owned())
        .relative()
        .size_full()
        .child(child);
    #[cfg(feature = "automation")]
    let wrapper = wrapper.child(automation::bounds_probe(id));
    wrapper
}

/// [`tracked`] for an element that keeps its own size (a button, a field, a row): the wrapper takes the child's size
/// instead of filling its parent.
pub fn tracked_box(id: &'static str, child: impl IntoElement) -> impl IntoElement {
    let wrapper = div()
        .id(id)
        .debug_selector(move || id.to_owned())
        .relative()
        .flex_none()
        .child(child);
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
    let wrapper = div()
        .id(id)
        .debug_selector(move || id.to_owned())
        .relative()
        .flex_none()
        .child(child);
    #[cfg(feature = "automation")]
    let wrapper = wrapper
        .child(automation::bounds_probe(id))
        .child(automation::focus_probe(id, handle));
    #[cfg(not(feature = "automation"))]
    let _ = handle;
    wrapper
}

/// App-wide by design (final review sweep): the theme, the components and the key bindings are installed once per app.
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
    // Task 21's Bot page asks for a fresh second factor through Task 26's step-up dialog (drawn by the overlays slot).
    cx.set_global(screens::bot_admin::bot_page::StepUp(std::rc::Rc::new(
        screens::factor::ask_step_up,
    )));
}

/// Opens the main window on the OS data dir and keychain: the start window (400x280), which takes the authentication and
/// workspace shapes as the screens change ([`screens::shell::window`]), titled [`PRODUCT_NAME`].
pub fn open_main_window(cx: &mut App) -> Result<WindowHandle<AppView>> {
    open_main_window_with(cx, Launch::os())
}

/// Opens the main window for `launch`: a shipping launch in [`opening_mode`]'s shape, any other at `launch.window`.
pub fn open_main_window_with(cx: &mut App, launch: Launch) -> Result<WindowHandle<AppView>> {
    let mode = launch
        .shipping
        .then(|| opening_mode(launch.handoff.is_some(), has_store(&launch)));
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

/// Whether the instance this launch opens (its `instance`, else the first listed, as `AppView::new` picks it) has a
/// store on disk: without one there is no account to start, only the sign-up or the sign-in.
fn has_store(launch: &Launch) -> bool {
    let Some(list) = launch
        .root
        .as_deref()
        .filter(|_| launch.resume)
        .and_then(|root| dilla_client::instances::Instances::load(root).ok())
    else {
        return false;
    };
    let cfg = match launch.instance {
        Some(id) => list.get(id),
        None => list.list().first(),
    };
    cfg.is_some_and(|cfg| cfg.data_dir.join(dilla_client::store::DB_FILE).exists())
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
        native::window_buttons::apply(window, mode);
        cx.new(|cx| AppView::new(launch, window, cx))
    })
}

/// beta.5 `setWindowButtonPosition({ x: 12, y: 16 })`: where the macOS traffic lights sit in the workspace's top bar,
/// logical px from the window's top left.
pub const MAC_WINDOW_BUTTONS: (f32, f32) = (12.0, 16.0);

/// The rect macOS draws the workspace window's three traffic lights in, logical px from the window's top left
/// (`x, y, width, height`): GPUI puts the close button's frame at [`MAC_WINDOW_BUTTONS`] and the other two after it
/// (gpui-pre-macos `move_traffic_light`). AppKit's button frames are 14x16 pt, 6 pt apart, around 12 pt circles 8 pt
/// apart; the rect is the frames' union (54x16), a superset of the circles (52x12). Nothing of the top bar may be drawn
/// in it (`macos_header_clears_the_traffic_lights`, dilla-parity tests/harness.rs).
pub const MAC_TRAFFIC_LIGHTS: (f32, f32, f32, f32) = (
    MAC_WINDOW_BUTTONS.0,
    MAC_WINDOW_BUTTONS.1,
    3.0 * 14.0 + 2.0 * 6.0,
    16.0,
);

/// The macOS traffic lights of a window in `mode` (beta.5 `setWindowMode`, main.ts:138): at [`MAC_WINDOW_BUTTONS`] in
/// the workspace, hidden (`None`) in the compact windows, which draw their own close button. Pure; the native side is
/// [`native::window_buttons::apply`].
pub fn mac_window_buttons(mode: WindowMode) -> Option<(f32, f32)> {
    (mode == WindowMode::Workspace).then_some(MAC_WINDOW_BUTTONS)
}

/// The options of the shipping window in `mode` at `bounds`: the shape's minimum size (the workspace cannot be resized
/// below 900x600; the compact windows are fixed, and on macOS not zoomable either, as beta.5's `setMaximizable`), and
/// the app's own title bar (no OS frame where the window can be drawn without one; macOS keeps its traffic lights in the
/// workspace, inset into the bar at [`MAC_WINDOW_BUTTONS`], and hides them in the compact windows).
pub fn window_options_for(mode: WindowMode, bounds: Bounds<Pixels>) -> WindowOptions {
    let shape = window_shape(mode, None, bounds.size.height.into());
    let mut options = main_window_options(bounds);
    options.window_min_size = Some(size(px(shape.min_width), px(shape.min_height)));
    options.is_resizable = shape.resizable;
    if let Some(titlebar) = options.titlebar.as_mut() {
        titlebar.appears_transparent = true;
        #[cfg(target_os = "macos")]
        {
            titlebar.traffic_light_position =
                mac_window_buttons(mode).map(|(x, y)| point(px(x), px(y)));
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
        app_id: Some(window_app_id().to_owned()),
        window_min_size: Some(size(px(WORKSPACE_MIN.0), px(WORKSPACE_MIN.1))),
        ..Default::default()
    }
}

#[cfg(test)]
mod tests {
    /// KDE strips a trailing `.desktop` from the app id before it looks up the desktop entry, so the entry named
    /// `dev.dilla.desktop.desktop` was never found and Plasma drew a generic icon.
    #[test]
    fn linux_app_id_is_not_a_desktop_name() {
        assert!(!super::LINUX_APP_ID.ends_with(".desktop"));
        assert_eq!(super::LINUX_APP_ID, "chat.dilla.Dilla");
        assert_eq!(
            super::APP_ID,
            "dev.dilla.desktop",
            "macOS and Windows keep theirs"
        );
    }

    #[test]
    fn every_window_uses_window_app_id() {
        let bounds = gpui::Bounds::new(
            gpui::point(gpui::px(0.0), gpui::px(0.0)),
            gpui::size(gpui::px(900.0), gpui::px(600.0)),
        );
        assert_eq!(
            super::main_window_options(bounds).app_id.as_deref(),
            Some(super::window_app_id())
        );
        assert_eq!(
            crate::windows::manager_options(bounds).app_id.as_deref(),
            Some(super::window_app_id())
        );
    }

    /// Integration 3, step 5: the Bot page's step-up (Task 21's `bot_page::StepUp`) is the factor area's dialog (Task
    /// 26's `ask_step_up`), installed with the app, so a Bot capability save asks for a fresh factor instead of saying
    /// the step-up is missing.
    /// Integration 3, re-review listed item (the toast component, `dilla_desktop_ui::components::toast`): a toast shown
    /// for window A draws in A only; one shown with no window to name (no active window) draws in every window.
    #[gpui::test]
    fn a_toast_shows_in_its_own_window(cx: &mut gpui::TestAppContext) {
        use dilla_desktop_ui::components::toast::{current_in, show_toast_in};
        let a = cx
            .add_empty_window()
            .update(|w, _| w.window_handle().window_id());
        let b = cx
            .add_empty_window()
            .update(|w, _| w.window_handle().window_id());
        cx.update(|cx| {
            show_toast_in(Some(a), "saved", cx);
            assert_eq!(current_in(a, cx).as_deref(), Some("saved"));
            assert_eq!(current_in(b, cx), None, "B shows none");
            show_toast_in(None, "everywhere", cx);
            assert_eq!(current_in(b, cx).as_deref(), Some("everywhere"));
        });
    }

    /// beta.5 DesktopTitlebar.css:24-26: the full-window card is drawn in the window the app marks as its
    /// authentication window, and only there.
    #[gpui::test]
    fn the_full_window_card_follows_the_windows_mark(cx: &mut gpui::TestAppContext) {
        use dilla_desktop_ui::components::onboarding::frame::{desktop_window, set_desktop_window};
        let a = cx.add_empty_window();
        a.update(|window, cx| {
            assert!(
                !desktop_window(window, cx),
                "a fixture draws the web's card"
            );
            set_desktop_window(window, true, cx);
            assert!(desktop_window(window, cx));
        });
        let b = cx.add_empty_window();
        b.update(|window, cx| assert!(!desktop_window(window, cx), "another window"));
        let a = cx.add_empty_window();
        a.update(|window, cx| {
            set_desktop_window(window, true, cx);
            set_desktop_window(window, false, cx);
            assert!(!desktop_window(window, cx), "the workspace drops the mark");
        });
    }

    /// beta.5 `startDesktopWindowLayout` + `windowShape`: the authentication window is as tall as its card (rounded up,
    /// 160 to 1600), at least 220 and at most the work area less 80.
    #[test]
    fn the_authentication_window_fits_its_card() {
        use crate::app::card_window_height;
        use crate::screens::shell::window::{WindowMode, window_shape};
        assert_eq!(card_window_height(412.3), 413.0);
        assert_eq!(card_window_height(90.0), 160.0);
        assert_eq!(card_window_height(4000.0), 1600.0);
        let fit = |card: f32, work: f32| {
            window_shape(
                WindowMode::Authentication,
                Some(card_window_height(card)),
                work,
            )
            .height
        };
        assert_eq!(fit(412.3, 1080.0), 413.0);
        assert_eq!(fit(150.0, 1080.0), 220.0);
        assert_eq!(fit(1200.0, 900.0), 820.0);
    }

    /// A card notes its height per window; the same height again changes nothing.
    #[gpui::test]
    fn a_card_notes_its_height_per_window(cx: &mut gpui::TestAppContext) {
        use dilla_desktop_ui::components::onboarding::frame::{
            desktop_card_height, note_card_height,
        };
        let a = cx.add_empty_window();
        a.update(|window, cx| {
            assert_eq!(desktop_card_height(window, cx), None);
            note_card_height(window, 431.5, cx);
            assert_eq!(desktop_card_height(window, cx), Some(431.5));
        });
        let b = cx.add_empty_window();
        b.update(|window, cx| assert_eq!(desktop_card_height(window, cx), None));
    }

    #[gpui::test]
    fn install_gives_the_bot_page_the_factor_step_up(cx: &mut gpui::TestAppContext) {
        cx.update(super::install);
        assert!(cx.update(|cx| cx.has_global::<crate::screens::bot_admin::bot_page::StepUp>()));
    }
}
