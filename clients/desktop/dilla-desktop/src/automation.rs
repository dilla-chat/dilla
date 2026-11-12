//! The test-only automation interface (cargo feature `automation`, never in a release build).
//!
//! Playwright cannot drive GPUI, so `dilla-desktop --automation --instance <dir>` runs the real main
//! window headless (GPUI's test platform with the real wgpu renderer) and serves a line protocol on
//! stdin/stdout. `dilla-parity`'s `Driver` is the client; each reply is exactly one line.
//!
//! ```text
//! element <id>            -> ok <x> <y> <w> <h> | none         (logical pixels, window coordinates)
//! click <id>              -> ok | err <why>                     (left click at the element's centre)
//! type <id> <hex>         -> ok | err <why>                     (click to focus, then the UTF-8 text typed)
//! screenshot <path>       -> ok <width> <height>                (PNG of the rendered frame, device pixels)
//! quit                    -> ok, then the process exits
//! ```
//!
//! Element ids are GPUI element ids: [`crate::tracked`] gives an element its id and a probe that reports
//! its painted bounds. The scale factor of the capture is fixed by GPUI's headless platform (2.0); a
//! reference image at another scale does not compare (`ParityFailure::Scale`).

use std::collections::HashMap;
use std::io::{BufRead, Write};
use std::sync::Mutex;

use gpui::{
    AnyWindowHandle, App, Bounds, HeadlessAppContext, Keystroke, Modifiers, MouseButton,
    MouseDownEvent, MouseUpEvent, Pixels, PlatformInput, Point, Window, canvas, point,
};

/// Marks a binary as an automation build. The release-build test greps for it.
pub const AUTOMATION_MARKER: &str = "dilla-automation-interface/1";

/// The painted bounds of every probed element in the latest frame.
static BOUNDS: Mutex<Option<HashMap<&'static str, Bounds<Pixels>>>> = Mutex::new(None);

fn with_bounds<R>(f: impl FnOnce(&mut HashMap<&'static str, Bounds<Pixels>>) -> R) -> R {
    let mut guard = BOUNDS.lock().expect("bounds registry");
    f(guard.get_or_insert_with(HashMap::new))
}

/// An invisible overlay that records the bounds of its parent under `id` each time it paints.
pub fn bounds_probe(id: &'static str) -> impl gpui::IntoElement {
    use gpui::Styled;
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

struct Session {
    cx: HeadlessAppContext,
    window: AnyWindowHandle,
    // The platform owns the headless dispatcher threads; keep it alive.
    _platform: std::rc::Rc<dyn gpui::Platform>,
}

impl Session {
    fn open() -> Self {
        let platform = gpui_platform::current_platform(true);
        let mut cx = HeadlessAppContext::with_platform(
            platform.text_system(),
            std::sync::Arc::new(()),
            gpui_platform::current_headless_renderer,
        );
        let window = cx
            .update(crate::open_main_window)
            .expect("the main window opens");
        let mut session = Self {
            cx,
            window: window.into(),
            _platform: platform,
        };
        session.frame();
        session
    }

    /// Lets pending work run, then repaints so the bounds registry describes the current frame.
    fn frame(&mut self) {
        self.cx.run_until_parked();
        with_bounds(HashMap::clear);
        let window = self.window;
        let _ = self
            .cx
            .update_window(window, |_, window: &mut Window, cx: &mut App| {
                window.refresh();
                window.draw(cx).clear(cx);
            });
        self.cx.run_until_parked();
    }

    fn element(&mut self, id: &str) -> Option<Bounds<Pixels>> {
        self.frame();
        with_bounds(|map| map.get(id).copied())
    }

    fn event(&mut self, event: PlatformInput) {
        let window = self.window;
        let _ = self.cx.update_window(window, |_, window, cx| {
            window.dispatch_event(event, cx);
        });
        self.frame();
    }

    fn click_at(&mut self, position: Point<Pixels>) {
        let modifiers = Modifiers::default();
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
        let bounds = self.element(id).ok_or_else(|| format!("no element {id}"))?;
        self.click_at(point(
            bounds.origin.x + bounds.size.width / 2.0,
            bounds.origin.y + bounds.size.height / 2.0,
        ));
        Ok(())
    }

    fn type_text(&mut self, id: &str, text: &str) -> Result<(), String> {
        self.click(id)?;
        let window = self.window;
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

    fn screenshot(&mut self, path: &str) -> Result<(u32, u32), String> {
        self.frame();
        let image = self
            .cx
            .capture_screenshot(self.window)
            .map_err(|error| format!("capture failed: {error}"))?;
        image
            .save_with_format(path, image::ImageFormat::Png)
            .map_err(|error| format!("write failed: {error}"))?;
        Ok(image.dimensions())
    }
}

fn unhex(hex: &str) -> Option<String> {
    if !hex.len().is_multiple_of(2) {
        return None;
    }
    let bytes: Option<Vec<u8>> = (0..hex.len())
        .step_by(2)
        .map(|i| u8::from_str_radix(hex.get(i..i + 2)?, 16).ok())
        .collect();
    String::from_utf8(bytes?).ok()
}

fn handle(session: &mut Session, line: &str) -> Option<String> {
    let mut parts = line.split_whitespace();
    let reply = match (parts.next(), parts.next(), parts.next()) {
        (Some("element"), Some(id), None) => match session.element(id) {
            Some(b) => format!(
                "ok {} {} {} {}",
                f32::from(b.origin.x),
                f32::from(b.origin.y),
                f32::from(b.size.width),
                f32::from(b.size.height)
            ),
            None => "none".to_owned(),
        },
        (Some("click"), Some(id), None) => match session.click(id) {
            Ok(()) => "ok".to_owned(),
            Err(why) => format!("err {why}"),
        },
        (Some("type"), Some(id), Some(hex)) => match unhex(hex) {
            Some(text) => match session.type_text(id, &text) {
                Ok(()) => "ok".to_owned(),
                Err(why) => format!("err {why}"),
            },
            None => "err bad hex text".to_owned(),
        },
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
/// (`--automation`, `--instance <dir>`); the instance fixture is accepted for the later screens.
pub fn run(args: Vec<String>) {
    let _instance = args
        .iter()
        .position(|arg| arg == "--instance")
        .and_then(|i| args.get(i + 1));
    let mut session = Session::open();
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
