//! The test-only automation interface (cargo feature `automation`, never in a release build).
//!
//! Playwright cannot drive GPUI, so `dilla-desktop --automation --instance <path>` runs the real main
//! window headless (GPUI's test platform with the real wgpu renderer) and serves a line protocol on
//! stdin/stdout. `dilla-parity`'s `Driver` is the client; each reply is exactly one line.
//!
//! ```text
//! element <id>            -> ok <x> <y> <w> <h> | none         (logical pixels, window coordinates)
//! click <id>              -> ok | err <why>                     (left click at the element's centre)
//! hover <id>              -> ok | err <why>                     (the pointer moves to the element's centre)
//! type <id> <hex>         -> ok | err <why>                     (click to focus, then the UTF-8 text typed; id `-`: no click)
//! key <chord>             -> ok | err <why>                     (one keystroke to the focused element: enter, tab, shift-tab,
//!                                                                escape, up, ctrl-k, ctrl-shift-m, ... see `spec::validate_chord`)
//! focused                 -> ok <id> | ok - | none              (the id of the focused element: see `focus_probe`; `-`: an
//!                                                                unregistered element holds the focus)
//! rest                    -> ok                                 (pointer to the window's corner, focus cleared: the web shooter's
//!                                                                last move before its frame)
//! pointer                 -> ok <x> <y>                         (the pointer's position, logical px)
//! scroll <id> <dy>        -> ok | err <why>                     (a wheel step of `dy` logical px over the element's centre)
//! wait-idle               -> ok <frames> | err not idle         (frames drawn until nothing on screen changed twice running;
//!                                                                60 frames without that is an error)
//! text-bounds             -> ok [<x> <y> <w> <h>[;<x> <y> <w> <h>]...]   (every painted text line, logical pixels)
//! fixture                 -> ok <fixture line> | none           (what the screen was drawn from: `ScreenFixture::fixture_line`)
//! log                     -> ok [<entry>[;<entry>]...]          (the harness view's received events; see `--harness`)
//! screenshot <path>       -> ok <width> <height>                (PNG of the rendered frame, device pixels)
//! quit                    -> ok, then the process exits
//! ```
//!
//! Element ids are GPUI element ids: [`crate::tracked`] gives an element its id and a probe that reports
//! its painted bounds. Every line of text the views set through [`crate::text`] reports its visible line box to
//! `text-bounds`, which the parity tests turn into the chrome gate's text mask (`parity::text_mask`). The scale
//! factor of the capture is fixed by GPUI's headless platform (2.0); a reference image at another scale does not
//! compare (`ParityFailure::Scale`).
//!
//! `--instance <path>`: a directory is the app's data root (the instance list and the stores), opened with an
//! in-memory keychain so a test never touches the OS keychain; a `.json` file is a screen fixture
//! ([`crate::fixture`], `dilla_client::testing::spec`), drawn without a network. `--shot <path>` is the reference's
//! sidecar (`<state>.shot.json`): the ids and the time shift the web shooter recorded.
//!
//! `--harness`: no app at all, a small view of the harness's own (see [`HarnessView`]) that the harness tests drive to
//! prove the interface itself: two focusable fields that log the keys they receive, a hover box and a scroll box.

#[cfg(not(debug_assertions))]
compile_error!(
    "the `automation` feature is test-only and must not be built without debug assertions: it never goes into a release build"
);

use std::cell::RefCell;
use std::collections::HashMap;
use std::io::{BufRead, Write};
use std::sync::Mutex;

use dilla_client::testing::spec;
use gpui::{
    AnyWindowHandle, App, Bounds, Context, FocusHandle, HeadlessAppContext, IntoElement, Keystroke,
    Modifiers, MouseButton, MouseDownEvent, MouseMoveEvent, MouseUpEvent, ParentElement, Pixels,
    PlatformInput, Point, Render, ScrollDelta, ScrollEvent, ScrollHandle, SharedString,
    StatefulInteractiveElement, Styled, Window, canvas, div, point, prelude::*, px,
};

use dilla_desktop_ui::tokens::color;

/// Set, every line of text asks for a family no embedded font answers to (the parity test's fallback-font control).
pub const FALLBACK_FONT_VAR: &str = "DILLA_AUTOMATION_FALLBACK_FONT";

/// Set, every line of text is painted transparent: the frame shows exactly the pixels the glyphs cover when it is
/// compared with the normal one (the text mask's control).
pub const HIDE_TEXT_VAR: &str = "DILLA_AUTOMATION_HIDE_TEXT";

/// Marks a binary as an automation build. The release-build test greps for it.
pub const AUTOMATION_MARKER: &str = "dilla-automation-interface/1";

/// The painted bounds of every probed element in the latest frame.
static BOUNDS: Mutex<Option<HashMap<&'static str, Bounds<Pixels>>>> = Mutex::new(None);
/// The visible line boxes of every text line painted in the latest frame.
static TEXT: Mutex<Vec<Bounds<Pixels>>> = Mutex::new(Vec::new());

thread_local! {
    /// The focus handles of the elements that registered one through [`focus_probe`], by element id, in the latest
    /// frame. GPUI draws on the thread that drives the session, so the registry is that thread's.
    static FOCUS: RefCell<Vec<(&'static str, FocusHandle)>> = const { RefCell::new(Vec::new()) };
    /// Every hand-over of the app's client from one window to another ([`handed_over`]), oldest first.
    static HANDOVERS: RefCell<Vec<(AnyWindowHandle, AnyWindowHandle)>> = const { RefCell::new(Vec::new()) };
}

/// Records that the window `from` handed its client to the window `to` and closes (the compact start or authentication
/// window opening the workspace window, Task 8's window shapes): a session driving `from` follows to `to`.
pub fn handed_over(from: AnyWindowHandle, to: AnyWindowHandle) {
    HANDOVERS.with(|h| h.borrow_mut().push((from, to)));
}

/// The window the client of `window` ended up in, through every hand-over since: `window` itself when it handed
/// nothing over.
pub fn followed(window: AnyWindowHandle) -> AnyWindowHandle {
    HANDOVERS.with(|h| {
        h.borrow().iter().fold(
            window,
            |now, (from, to)| if *from == now { *to } else { now },
        )
    })
}

fn with_bounds<R>(f: impl FnOnce(&mut HashMap<&'static str, Bounds<Pixels>>) -> R) -> R {
    let mut guard = BOUNDS.lock().expect("bounds registry");
    f(guard.get_or_insert_with(HashMap::new))
}

/// An invisible overlay that records the bounds of its parent under `id` each time it paints.
pub fn bounds_probe(id: &'static str) -> impl gpui::IntoElement {
    canvas(
        move |bounds, _window, _cx| {
            with_bounds(|map| {
                map.insert(id, bounds);
            })
        },
        |_, (), _, _| {},
    )
    .absolute()
    .top_0()
    .left_0()
    .size_full()
}

/// An invisible overlay that records `handle` under `id` each time it paints, so `focused` can answer with `id` while
/// `handle` holds the focus. Use it through [`crate::tracked_focus`] on the element that owns the handle.
pub fn focus_probe(id: &'static str, handle: &FocusHandle) -> impl gpui::IntoElement {
    let handle = handle.clone();
    canvas(
        move |_, _window, _cx| {
            FOCUS.with(|f| f.borrow_mut().push((id, handle.clone())));
        },
        |_, (), _, _| {},
    )
    .absolute()
    .top_0()
    .left_0()
    .size_full()
}

/// An invisible overlay that records its parent's box as a text line each time it paints. Only the part inside the
/// current clip is kept: a line scrolled out or clipped away reports nothing.
pub fn text_probe() -> impl gpui::IntoElement {
    canvas(
        move |bounds, window, _cx| {
            let visible = window.content_mask().bounds.intersect(&bounds);
            if visible.size.width > px(0.) && visible.size.height > px(0.) {
                TEXT.lock().expect("text registry").push(visible);
            }
        },
        |_, (), _, _| {},
    )
    .absolute()
    .top_0()
    .left_0()
    .size_full()
}

/// What `--harness` shows: the harness's own view, to prove the interface without any screen of the app.
///
/// - `field-a` and `field-b` are focusable, tab stops in that order, and append `<id>:<keystroke>` to the log for every
///   key they receive;
/// - `hover-box` logs `hover-box:enter` and `hover-box:leave`, and fills with the hover colour while the pointer is in it;
/// - `scroll-box` is a scrolling list: the log gets `scroll-box:<offset>` (logical px, rounded) after each wheel step.
pub struct HarnessView {
    focus: [FocusHandle; 2],
    scroll: ScrollHandle,
    hovered: bool,
    log: Vec<String>,
}

thread_local! {
    /// The harness view's log, readable by the `log` command.
    static HARNESS_LOG: RefCell<Vec<String>> = const { RefCell::new(Vec::new()) };
}

impl HarnessView {
    fn new(cx: &mut Context<Self>) -> Self {
        Self {
            focus: [
                cx.focus_handle().tab_stop(true),
                cx.focus_handle().tab_stop(true),
            ],
            scroll: ScrollHandle::new(),
            hovered: false,
            log: Vec::new(),
        }
    }

    fn note(&mut self, entry: String) {
        self.log.push(entry);
        let log = self.log.clone();
        HARNESS_LOG.with(|l| *l.borrow_mut() = log);
    }
}

impl Render for HarnessView {
    fn render(&mut self, _window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let field = |index: usize, id: &'static str, view: &mut Self, cx: &mut Context<Self>| {
            let handle = view.focus[index].clone();
            crate::tracked_focus(
                id,
                &handle,
                div()
                    .id(SharedString::from(format!("{id}-field")))
                    .track_focus(&handle)
                    .w(px(240.))
                    .h(px(32.))
                    .bg(color::SURFACE)
                    .border_1()
                    .border_color(color::HAIRLINE)
                    .focus(|s| s.border_color(color::ACCENT))
                    .on_key_down(cx.listener(move |this, event: &gpui::KeyDownEvent, _, cx| {
                        let k = &event.keystroke;
                        let mut chord = String::new();
                        for (on, name) in [
                            (k.modifiers.control, "ctrl-"),
                            (k.modifiers.alt, "alt-"),
                            (k.modifiers.shift, "shift-"),
                        ] {
                            if on {
                                chord.push_str(name);
                            }
                        }
                        chord.push_str(&k.key);
                        this.note(format!("{id}:{chord}"));
                        cx.notify();
                    })),
            )
        };
        let a = field(0, "field-a", self, cx);
        let b = field(1, "field-b", self, cx);
        let scroll = self.scroll.clone();
        let hovered = self.hovered;
        div()
            .size_full()
            .bg(color::BG)
            .flex()
            .flex_col()
            .items_start()
            .on_action(|_: &crate::screens::FocusNext, window, cx| window.focus_next(cx))
            .on_action(|_: &crate::screens::FocusPrevious, window, cx| window.focus_prev(cx))
            .gap(px(16.))
            .p(px(16.))
            .child(a)
            .child(b)
            .child(crate::tracked_box(
                "hover-box",
                div()
                    .id("hover-box-inner")
                    .w(px(240.))
                    .h(px(48.))
                    .bg(if hovered {
                        color::HOVER_HI
                    } else {
                        color::SURFACE
                    })
                    .on_hover(cx.listener(|this, hovered: &bool, _, cx| {
                        this.hovered = *hovered;
                        this.note(format!(
                            "hover-box:{}",
                            if *hovered { "enter" } else { "leave" }
                        ));
                        cx.notify();
                    })),
            ))
            .child(crate::tracked_box(
                "scroll-box",
                div()
                    .id("scroll-box-inner")
                    .w(px(240.))
                    .h(px(96.))
                    .overflow_y_scroll()
                    .track_scroll(&scroll)
                    .on_scroll(cx.listener(|this, _: &ScrollEvent, _, cx| {
                        let offset = f32::from(this.scroll.offset().y).round();
                        this.note(format!("scroll-box:{offset}"));
                        cx.notify();
                    }))
                    .children((0..40).map(|n| div().h(px(24.)).child(format!("row {n}")))),
            ))
    }
}

/// What a session draws.
pub enum Mode {
    /// The app, on a data root or a fixture.
    App(Box<crate::app::Launch>),
    /// The harness's own view ([`HarnessView`]).
    Harness,
}

struct Session {
    cx: HeadlessAppContext,
    window: AnyWindowHandle,
    /// The fixture line the screen was drawn from, when it is a fixture.
    fixture: Option<String>,
    // The platform owns the headless dispatcher threads; keep it alive.
    _platform: std::rc::Rc<dyn gpui::Platform>,
}

impl Session {
    fn open(mode: Mode) -> Self {
        let platform = gpui_platform::current_platform(true);
        let mut cx = HeadlessAppContext::with_platform(
            platform.text_system(),
            std::sync::Arc::new(dilla_desktop_ui::theme::Assets),
            gpui_platform::current_headless_renderer,
        );
        let (window, fixture) = match mode {
            Mode::App(launch) => {
                let launch = *launch;
                let line = launch.fixture.as_ref().map(crate::fixture::Fixture::line);
                let window = cx
                    .update(|cx| crate::open_main_window_with(cx, launch))
                    .expect("the main window opens");
                (window.into(), line)
            }
            Mode::Harness => {
                let window = cx
                    .update(|cx| {
                        crate::install(cx);
                        let bounds = Bounds::centered(None, gpui::size(px(1280.), px(800.)), cx);
                        cx.open_window(crate::main_window_options(bounds), |window, cx| {
                            window.set_window_title(crate::PRODUCT_NAME);
                            cx.new(HarnessView::new)
                        })
                    })
                    .expect("the harness window opens");
                (window.into(), None)
            }
        };
        let mut session = Self {
            cx,
            window,
            fixture,
            _platform: platform,
        };
        session.frame();
        session
    }

    /// The window the session drives: the one it opened, or the window that took over its client since.
    fn window(&mut self) -> AnyWindowHandle {
        self.window = followed(self.window);
        self.window
    }

    /// Lets pending work run, then repaints so the registries describe the current frame.
    fn frame(&mut self) {
        self.cx.run_until_parked();
        with_bounds(HashMap::clear);
        TEXT.lock().expect("text registry").clear();
        FOCUS.with(|f| f.borrow_mut().clear());
        let window = self.window();
        let _ = self
            .cx
            .update_window(window, |_, window: &mut Window, cx: &mut App| {
                window.refresh();
                window.draw(cx).clear(cx);
            });
        self.cx.run_until_parked();
    }

    /// Draws until two frames in a row describe the same screen (bounds and text lines), at most 60 frames.
    fn wait_idle(&mut self) -> Option<usize> {
        let snapshot = || {
            let mut bounds: Vec<(String, [f32; 4])> = with_bounds(|m| {
                m.iter()
                    .map(|(k, b)| {
                        (
                            (*k).to_owned(),
                            [
                                f32::from(b.origin.x),
                                f32::from(b.origin.y),
                                f32::from(b.size.width),
                                f32::from(b.size.height),
                            ],
                        )
                    })
                    .collect()
            });
            bounds.sort_by(|a, b| a.0.cmp(&b.0));
            let text: Vec<String> = TEXT
                .lock()
                .expect("text registry")
                .iter()
                .map(rect)
                .collect();
            (bounds, text)
        };
        let mut previous = None;
        for frames in 1..=60 {
            self.frame();
            let now = snapshot();
            if previous.as_ref() == Some(&now) {
                return Some(frames);
            }
            previous = Some(now);
            std::thread::sleep(std::time::Duration::from_millis(16));
        }
        None
    }

    fn element(&mut self, id: &str) -> Option<Bounds<Pixels>> {
        self.frame();
        with_bounds(|map| map.get(id).copied())
    }

    fn event(&mut self, event: PlatformInput) {
        let window = self.window();
        let _ = self.cx.update_window(window, |_, window, cx| {
            window.dispatch_event(event, cx);
        });
        self.frame();
    }

    fn centre(&mut self, id: &str) -> Result<Point<Pixels>, String> {
        let bounds = self.element(id).ok_or_else(|| format!("no element {id}"))?;
        Ok(point(
            bounds.origin.x + bounds.size.width / 2.0,
            bounds.origin.y + bounds.size.height / 2.0,
        ))
    }

    fn click_at(&mut self, position: Point<Pixels>) {
        let modifiers = Modifiers::default();
        // The pointer arrives before it presses, as it does for a person: hover and focus-on-click follow it.
        self.event(PlatformInput::MouseMove(MouseMoveEvent {
            position,
            pressed_button: None,
            modifiers,
        }));
        self.event(PlatformInput::MouseDown(MouseDownEvent {
            button: MouseButton::Left,
            position,
            modifiers,
            click_count: 1,
            first_mouse: false,
        }));
        self.event(PlatformInput::MouseUp(MouseUpEvent {
            button: MouseButton::Left,
            position,
            modifiers,
            click_count: 1,
        }));
    }

    fn click(&mut self, id: &str) -> Result<(), String> {
        let position = self.centre(id)?;
        self.click_at(position);
        Ok(())
    }

    fn hover(&mut self, id: &str) -> Result<(), String> {
        let position = self.centre(id)?;
        self.event(PlatformInput::MouseMove(MouseMoveEvent {
            position,
            pressed_button: None,
            modifiers: Modifiers::default(),
        }));
        Ok(())
    }

    fn scroll(&mut self, id: &str, dy: f32) -> Result<(), String> {
        let position = self.centre(id)?;
        self.event(PlatformInput::Scroll(ScrollEvent {
            position,
            delta: ScrollDelta::Pixels(point(px(0.), px(dy))),
            ..ScrollEvent::default()
        }));
        Ok(())
    }

    fn type_text(&mut self, id: &str, text: &str) -> Result<(), String> {
        // `-` types into whatever holds the focus (after a Tab), without a click.
        if id != "-" {
            self.click(id)?;
        }
        let window = self.window();
        for ch in text.chars() {
            let keystroke = Keystroke {
                modifiers: Modifiers::default(),
                key: ch.to_string(),
                key_char: Some(ch.to_string()),
            };
            let _ = self.cx.update_window(window, |_, window, cx| {
                window.dispatch_keystroke(keystroke, cx);
            });
        }
        self.frame();
        Ok(())
    }

    fn key(&mut self, chord: &str) -> Result<(), String> {
        let keystroke = parse_chord(chord)?;
        let window = self.window();
        self.cx
            .update_window(window, |_, window, cx| {
                window.dispatch_keystroke(keystroke, cx);
            })
            .map_err(|e| e.to_string())?;
        self.frame();
        Ok(())
    }

    /// The id of the element whose registered focus handle holds the focus; `-` when something unregistered holds it;
    /// `None` when nothing does.
    fn focused(&mut self) -> Option<&'static str> {
        self.frame();
        let window = self.window();
        self.cx
            .update_window(window, |_, window, cx| {
                FOCUS
                    .with(|f| {
                        f.borrow()
                            .iter()
                            .find(|(_, handle)| handle.is_focused(window))
                            .map(|(id, _)| *id)
                    })
                    .or_else(|| window.focused(cx).map(|_| "-"))
            })
            .ok()
            .flatten()
    }

    /// What a person's hands off the screen look like, and what the web shooter does before its frame: the pointer
    /// goes to the window's corner (2 px in, off any content) and the focus is cleared.
    fn rest(&mut self) {
        let window = self.window();
        let size = self
            .cx
            .update_window(window, |_, window, _| window.viewport_size())
            .expect("the window exists");
        let corner = point(size.width - px(2.), size.height - px(2.));
        self.event(PlatformInput::MouseMove(MouseMoveEvent {
            position: corner,
            pressed_button: None,
            modifiers: Modifiers::default(),
        }));
        let _ = self
            .cx
            .update_window(window, |_, window, cx| window.blur(cx));
        self.frame();
    }

    fn pointer(&mut self) -> Point<Pixels> {
        let window = self.window();
        self.cx
            .update_window(window, |_, window, _| window.mouse_position())
            .expect("the window exists")
    }

    fn text_bounds(&mut self) -> Vec<Bounds<Pixels>> {
        self.frame();
        TEXT.lock().expect("text registry").clone()
    }

    fn screenshot(&mut self, path: &str) -> Result<(u32, u32), String> {
        self.frame();
        let window = self.window();
        let image = self
            .cx
            .capture_screenshot(window)
            .map_err(|error| format!("capture failed: {error}"))?;
        image
            .save_with_format(path, image::ImageFormat::Png)
            .map_err(|error| format!("write failed: {error}"))?;
        Ok(image.dimensions())
    }
}

/// One keystroke from a chord of `spec::validate_chord`'s grammar.
fn parse_chord(chord: &str) -> Result<Keystroke, String> {
    spec::validate_chord(chord).map_err(|e| e.to_string())?;
    let mut modifiers = Modifiers::default();
    let mut parts: Vec<&str> = chord.split('-').collect();
    let name = parts.pop().expect("validate_chord keeps one key");
    for m in parts {
        match m {
            "ctrl" => modifiers.control = true,
            "alt" => modifiers.alt = true,
            "shift" => modifiers.shift = true,
            "fn" => modifiers.function = true,
            // `secondary-` is the platform's shortcut modifier: Cmd on macOS, Ctrl elsewhere.
            "secondary" => {
                if cfg!(target_os = "macos") {
                    modifiers.platform = true;
                } else {
                    modifiers.control = true;
                }
            }
            other => return Err(format!("`{other}` is not a modifier")),
        }
    }
    let (key, printable) = match name {
        "space" => ("space", Some(" ")),
        "minus" => ("-", Some("-")),
        "comma" => (",", Some(",")),
        "period" => (".", Some(".")),
        "slash" => ("/", Some("/")),
        "semicolon" => (";", Some(";")),
        "quote" => ("'", Some("'")),
        "backslash" => ("\\", Some("\\")),
        "equals" => ("=", Some("=")),
        "backtick" => ("`", Some("`")),
        other => (other, None),
    };
    let single = name.chars().count() == 1;
    // Text reaches an input only from keys without Ctrl, Alt or Cmd; a plain `a` or `shift-a` types.
    let types = !(modifiers.control || modifiers.alt || modifiers.platform);
    let key_char = if types {
        match printable {
            Some(s) => Some(s.to_owned()),
            None if single => Some(if modifiers.shift {
                name.to_uppercase()
            } else {
                name.to_owned()
            }),
            None => None,
        }
    } else {
        None
    };
    Ok(Keystroke {
        modifiers,
        key: key.to_owned(),
        key_char,
    })
}

/// The UTF-8 text of an even number of hex digits. Only digits: `u8::from_str_radix` alone also takes a leading `+`.
fn unhex(hex: &str) -> Option<String> {
    if !hex.len().is_multiple_of(2) || !hex.bytes().all(|b| b.is_ascii_hexdigit()) {
        return None;
    }
    let bytes: Option<Vec<u8>> = (0..hex.len())
        .step_by(2)
        .map(|i| u8::from_str_radix(hex.get(i..i + 2)?, 16).ok())
        .collect();
    String::from_utf8(bytes?).ok()
}

fn rect(b: &Bounds<Pixels>) -> String {
    format!(
        "{} {} {} {}",
        f32::from(b.origin.x),
        f32::from(b.origin.y),
        f32::from(b.size.width),
        f32::from(b.size.height)
    )
}

fn done(result: Result<(), String>) -> String {
    match result {
        Ok(()) => "ok".to_owned(),
        Err(why) => format!("err {why}"),
    }
}

fn handle(session: &mut Session, line: &str) -> Option<String> {
    let mut parts = line.split_whitespace();
    let reply = match (parts.next(), parts.next(), parts.next()) {
        (Some("element"), Some(id), None) => match session.element(id) {
            Some(b) => format!("ok {}", rect(&b)),
            None => "none".to_owned(),
        },
        (Some("click"), Some(id), None) => done(session.click(id)),
        (Some("hover"), Some(id), None) => done(session.hover(id)),
        (Some("scroll"), Some(id), Some(dy)) => match dy.parse::<f32>() {
            Ok(dy) if dy.is_finite() => done(session.scroll(id, dy)),
            _ => "err bad scroll distance".to_owned(),
        },
        (Some("type"), Some(id), Some(hex)) => match unhex(hex) {
            Some(text) => done(session.type_text(id, &text)),
            None => "err bad hex text".to_owned(),
        },
        (Some("key"), Some(chord), None) => done(session.key(chord)),
        (Some("focused"), None, None) => match session.focused() {
            Some(id) => format!("ok {id}"),
            None => "none".to_owned(),
        },
        (Some("wait-idle"), None, None) => match session.wait_idle() {
            Some(frames) => format!("ok {frames}"),
            None => "err not idle".to_owned(),
        },
        (Some("rest"), None, None) => {
            session.rest();
            "ok".to_owned()
        }
        (Some("pointer"), None, None) => {
            let p = session.pointer();
            format!("ok {} {}", f32::from(p.x), f32::from(p.y))
        }
        (Some("text-bounds"), None, None) => {
            let rects: Vec<String> = session.text_bounds().iter().map(rect).collect();
            format!("ok {}", rects.join(";")).trim_end().to_owned()
        }
        (Some("fixture"), None, None) => match &session.fixture {
            Some(line) => format!("ok {line}"),
            None => "none".to_owned(),
        },
        (Some("log"), None, None) => {
            session.frame();
            let log = HARNESS_LOG.with(|l| l.borrow().join(";"));
            format!("ok {log}").trim_end().to_owned()
        }
        (Some("screenshot"), Some(_), _) => {
            let path = line.trim_start().strip_prefix("screenshot")?.trim();
            match session.screenshot(path) {
                Ok((w, h)) => format!("ok {w} {h}"),
                Err(why) => format!("err {why}"),
            }
        }
        (Some("quit"), None, None) => return None,
        _ => format!("err unknown command ({AUTOMATION_MARKER})"),
    };
    Some(reply)
}

/// Runs the automation server until `quit` or end of input. `args` is the process argv
/// (`--automation`, `--instance <path>`, `--shot <path>`, `--harness`; see the module doc).
pub fn run(args: Vec<String>) {
    let value = |flag: &str| {
        args.iter()
            .position(|arg| arg == flag)
            .and_then(|i| args.get(i + 1))
            .map(std::path::PathBuf::from)
    };
    let mode = if args.iter().any(|a| a == "--harness") {
        Mode::Harness
    } else {
        let launch = match value("--instance") {
            Some(path) if path.extension().is_some_and(|e| e == "json") => {
                let fixture = match crate::fixture::Fixture::load(&path, value("--shot").as_deref())
                {
                    Ok(fixture) => fixture,
                    // The launch fails, loudly and before `ready`: the driver reports this line.
                    Err(e) => {
                        println!("error the fixture {} does not load: {e}", path.display());
                        std::process::exit(2);
                    }
                };
                crate::app::Launch::fixture(fixture)
            }
            Some(root) => crate::app::Launch::live(
                Some(root),
                std::sync::Arc::new(dilla_client::keychain::MemoryKeychain::new()),
            ),
            None => crate::app::Launch::empty(),
        };
        Mode::App(Box::new(launch))
    };
    // Every line of text reports its box; the parity test's negative control asks every line for a missing family.
    dilla_desktop_ui::text::set_hooks(dilla_desktop_ui::text::Hooks {
        probe: Some(|| gpui::IntoElement::into_any_element(text_probe())),
        family: std::env::var_os(FALLBACK_FONT_VAR).map(|_| "Dilla Parity Missing Font"),
        hidden: std::env::var_os(HIDE_TEXT_VAR).is_some(),
    });
    let mut session = Session::open(mode);
    let mut out = std::io::stdout().lock();
    writeln!(out, "ready {AUTOMATION_MARKER}")
        .and_then(|()| out.flush())
        .ok();
    for line in std::io::stdin().lock().lines() {
        let Ok(line) = line else { break };
        let reply = handle(&mut session, &line);
        let quitting = reply.is_none();
        writeln!(out, "{}", reply.as_deref().unwrap_or("ok"))
            .and_then(|()| out.flush())
            .ok();
        if quitting {
            break;
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn chords_become_keystrokes() {
        let k = parse_chord("ctrl-shift-m").unwrap();
        assert!(k.modifiers.control && k.modifiers.shift && !k.modifiers.alt);
        assert_eq!((k.key.as_str(), k.key_char), ("m", None));
        let enter = parse_chord("enter").unwrap();
        assert_eq!((enter.key.as_str(), enter.key_char), ("enter", None));
        let a = parse_chord("shift-a").unwrap();
        assert_eq!(a.key_char.as_deref(), Some("A"));
        assert_eq!(parse_chord("space").unwrap().key_char.as_deref(), Some(" "));
        assert!(parse_chord("hyper-k").is_err());
        assert!(parse_chord("").is_err());
    }

    struct Blank;
    impl Render for Blank {
        fn render(&mut self, _: &mut Window, _: &mut Context<Self>) -> impl IntoElement {
            div()
        }
    }

    /// The start window hands its client to the workspace window and closes (`AppView::open_workspace`): the session
    /// draws, clicks and types into the window the client ends up in, through any number of hand-overs.
    #[gpui::test]
    fn the_session_follows_the_window_the_client_is_handed_to(cx: &mut gpui::TestAppContext) {
        let open = |cx: &mut gpui::TestAppContext| -> AnyWindowHandle {
            cx.add_window(|_, _| Blank).into()
        };
        let (start, workspace, again, other) = (open(cx), open(cx), open(cx), open(cx));
        assert_eq!(followed(start), start, "no hand-over yet");
        handed_over(start, workspace);
        handed_over(workspace, again);
        assert_eq!(followed(start), again);
        assert_eq!(followed(workspace), again);
        assert_eq!(followed(other), other, "another window is not moved");
    }

    #[test]
    fn hex_is_strict() {
        assert_eq!(unhex("68c3a9").as_deref(), Some("h\u{e9}"));
        assert_eq!(unhex("").as_deref(), Some(""));
        // `from_str_radix` takes "+a" as 10: a signed pair must not pass for a byte.
        assert_eq!(unhex("+a"), None);
        assert_eq!(unhex("-a"), None);
        assert_eq!(unhex("6"), None);
        assert_eq!(unhex("zz"), None);
        assert_eq!(unhex("c3"), None, "a lone UTF-8 lead byte is not text");
    }
}
