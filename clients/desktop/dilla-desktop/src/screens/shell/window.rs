//! The window's shapes (packages/desktop `window-layout.ts` `windowShape`): the start window, the compact sign-in window
//! and the workspace. All dimensions are logical px. Pure: `lib.rs` turns a shape into GPUI window options, and
//! [`restore_bounds`] decides where the workspace opens from the bounds the app kept.

use dilla_client::prefs::Prefs;

/// `WindowMode`.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash)]
pub enum WindowMode {
    /// The start window while the client opens: 400x280.
    Starting,
    /// The sign-in, enrol and factor screens: a compact, fixed-width dialog.
    Authentication,
    /// The signed-in app.
    Workspace,
}

impl WindowMode {
    /// A compact mode is a fixed-size window; the workspace resizes.
    pub fn is_compact(self) -> bool {
        self != Self::Workspace
    }
}

/// `WORKSPACE_MINIMUM`.
pub const WORKSPACE_MIN: (f32, f32) = (900.0, 600.0);
/// The compact windows' width, and their shortest height.
pub const COMPACT_WIDTH: f32 = 400.0;
pub const COMPACT_MIN_HEIGHT: f32 = 220.0;
/// What the OS keeps for its own bars on the work area (`workAreaHeight - 80`).
const WORK_AREA_MARGIN: f32 = 80.0;
/// The authentication window's height until its content says (`contentHeight ?? 620`).
const AUTH_HEIGHT: f32 = 620.0;
/// The start window's height.
const STARTING_HEIGHT: f32 = 280.0;
/// The workspace's opening width, and the bounds of its opening height.
const WORKSPACE_WIDTH: f32 = 1240.0;
const WORKSPACE_HEIGHT: (f32, f32) = (600.0, 820.0);

/// One window shape: its size, its minimum and whether the person may resize it.
#[derive(Clone, Copy, Debug, PartialEq)]
pub struct WindowShape {
    pub width: f32,
    pub height: f32,
    pub min_width: f32,
    pub min_height: f32,
    pub resizable: bool,
}

/// `windowShape`: the workspace is 1240 wide and `clamp(600..820)` high (the work area less 80), at least 900x600 and
/// resizable; the compact modes are 400 wide, 220 or taller, and fixed.
pub fn window_shape(
    mode: WindowMode,
    content_height: Option<f32>,
    work_area_height: f32,
) -> WindowShape {
    if mode == WindowMode::Workspace {
        return WindowShape {
            width: WORKSPACE_WIDTH,
            height: (work_area_height - WORK_AREA_MARGIN)
                .min(WORKSPACE_HEIGHT.1)
                .max(WORKSPACE_HEIGHT.0),
            min_width: WORKSPACE_MIN.0,
            min_height: WORKSPACE_MIN.1,
            resizable: true,
        };
    }
    let wanted = if mode == WindowMode::Starting {
        STARTING_HEIGHT
    } else {
        content_height.unwrap_or(AUTH_HEIGHT)
    };
    WindowShape {
        width: COMPACT_WIDTH,
        height: (work_area_height - WORK_AREA_MARGIN)
            .min(wanted)
            .max(COMPACT_MIN_HEIGHT),
        min_width: COMPACT_WIDTH,
        min_height: COMPACT_MIN_HEIGHT,
        resizable: false,
    }
}

/// The workspace bounds the app kept (`<root>/desktop.json`, key `layout.windowBounds`): origin and size, logical px.
#[derive(Clone, Copy, Debug, PartialEq)]
pub struct SavedBounds {
    pub x: f32,
    pub y: f32,
    pub width: f32,
    pub height: f32,
}

impl SavedBounds {
    pub const KEY: &'static str = "layout.windowBounds";

    /// Kept as `[x, y, width, height]`.
    pub fn read(prefs: &Prefs) -> Option<Self> {
        match prefs.get::<Vec<f32>>(Self::KEY)?.as_slice() {
            &[x, y, width, height] => Some(Self {
                x,
                y,
                width,
                height,
            }),
            _ => None,
        }
    }

    pub fn write(&self, prefs: &Prefs) {
        let _ = prefs.set(Self::KEY, &[self.x, self.y, self.width, self.height]);
    }
}

/// A display's bounds, logical px.
#[derive(Clone, Copy, Debug, PartialEq)]
pub struct Screen {
    pub x: f32,
    pub y: f32,
    pub width: f32,
    pub height: f32,
}

/// Where the workspace opens: the saved size, never under the minimum nor over the largest display, at the saved origin
/// when its title strip is still on a display; `None` for the origin means the platform's (centred) choice. Without saved
/// bounds the shape's own size.
pub fn restore_bounds(
    saved: Option<SavedBounds>,
    shape: WindowShape,
    screens: &[Screen],
) -> (f32, f32, Option<(f32, f32)>) {
    let Some(b) = saved.filter(|b| {
        [b.x, b.y, b.width, b.height].iter().all(|v| v.is_finite()) && b.width > 0.0 && b.height > 0.0
    }) else {
        return (shape.width, shape.height, None);
    };
    let (max_w, max_h) = screens.iter().fold((f32::INFINITY, f32::INFINITY), |m, s| {
        if m.0.is_infinite() {
            (s.width, s.height)
        } else {
            (m.0.max(s.width), m.1.max(s.height))
        }
    });
    let width = b.width.clamp(shape.min_width, shape.min_width.max(max_w));
    let height = b.height.clamp(shape.min_height, shape.min_height.max(max_h));
    // A grabbable strip of the title bar (the top-left 100x32 corner) must lie on a display.
    let visible = screens.iter().any(|s| {
        b.x + 100.0 <= s.x + s.width
            && b.x + width - 100.0 >= s.x
            && b.y >= s.y - 1.0
            && b.y + 32.0 <= s.y + s.height
    });
    (width, height, visible.then_some((b.x, b.y)))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_workspace_is_1240_wide_and_its_height_follows_the_work_area() {
        let at = |work| window_shape(WindowMode::Workspace, None, work);
        assert_eq!((at(1080.0).width, at(1080.0).height), (1240.0, 820.0));
        assert_eq!(at(800.0).height, 720.0);
        assert_eq!(at(600.0).height, 600.0, "never under 600");
        let s = at(1080.0);
        assert_eq!((s.min_width, s.min_height, s.resizable), (900.0, 600.0, true));
    }

    #[test]
    fn the_compact_windows_are_400_wide_and_fixed() {
        let start = window_shape(WindowMode::Starting, Some(900.0), 1080.0);
        assert_eq!((start.width, start.height, start.resizable), (400.0, 280.0, false));
        let auth = window_shape(WindowMode::Authentication, None, 1080.0);
        assert_eq!(auth.height, 620.0);
        let tall = window_shape(WindowMode::Authentication, Some(2000.0), 1080.0);
        assert_eq!(tall.height, 1000.0, "the work area less 80");
        let short = window_shape(WindowMode::Authentication, Some(100.0), 1080.0);
        assert_eq!(short.height, 220.0);
    }

    #[test]
    fn saved_bounds_are_restored_within_the_minimum_and_the_displays() {
        let shape = window_shape(WindowMode::Workspace, None, 1080.0);
        let screens = [Screen {
            x: 0.0,
            y: 0.0,
            width: 1920.0,
            height: 1080.0,
        }];
        let saved = SavedBounds {
            x: 100.0,
            y: 60.0,
            width: 1500.0,
            height: 700.0,
        };
        assert_eq!(
            restore_bounds(Some(saved), shape, &screens),
            (1500.0, 700.0, Some((100.0, 60.0)))
        );
        // Too small: the minimum holds. Too big: the display's size.
        let small = SavedBounds { width: 300.0, height: 200.0, ..saved };
        assert_eq!(restore_bounds(Some(small), shape, &screens).0, 900.0);
        assert_eq!(restore_bounds(Some(small), shape, &screens).1, 600.0);
        let huge = SavedBounds { width: 9000.0, height: 9000.0, ..saved };
        assert_eq!(restore_bounds(Some(huge), shape, &screens).0, 1920.0);
        // A display that is gone: the size is kept, the origin is the platform's.
        let away = SavedBounds { x: 5000.0, ..saved };
        assert_eq!(restore_bounds(Some(away), shape, &screens).2, None);
        // Nothing saved: the shape.
        assert_eq!(restore_bounds(None, shape, &screens), (1240.0, 820.0, None));
    }
}
