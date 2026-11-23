//! Which of ScreenCaptureKit's displays and windows Dilla's own macOS share picker offers, and how they are named
//! (GPUI gaps Task 45, D22). Pure: the records come from webrtc-sys's `dilla_screen` bridge on macOS
//! (`super::desktop_mac`), and the filtering is compiled and tested on every system.

use super::desktop::{DesktopSource, SourceKind};

/// One display or on-screen window as ScreenCaptureKit lists it, unfiltered (the bridge's `DillaScreenRecord`).
#[derive(Clone, Debug, PartialEq)]
pub struct MacRecord {
    pub window: bool,
    /// A `CGDirectDisplayID` or a `CGWindowID`.
    pub id: u64,
    /// `SCWindow.owningApplication.processID`; 0 for a display or a window without an application.
    pub owner_pid: i32,
    pub bundle_id: String,
    pub app_name: String,
    /// The window's title, or the display's name.
    pub title: String,
    /// `SCWindow.windowLayer`; 0 for a display.
    pub layer: i64,
    /// The frame, in points.
    pub width: f64,
    pub height: f64,
    /// The display is the main one (`CGMainDisplayID`).
    pub main: bool,
}

/// The smallest window side, in points, that the picker offers (smaller ones are helpers, badges and
/// popovers).
pub const MIN_SIDE: f64 = 50.0;

/// System UI that is on screen at layer 0 on some macOS versions, never a share target.
pub const SYSTEM_BUNDLES: &[&str] = &[
    "com.apple.dock",
    "com.apple.controlcenter",
    "com.apple.WindowManager",
    "com.apple.notificationcenterui",
    "com.apple.systemuiserver",
    "com.apple.Spotlight",
    "com.apple.screencaptureui",
];

/// Whether a window record is offered. It is not offered when it is:
/// - Dilla's own (`owner_pid == own`, as on Windows; X11's list doesn't need the check);
/// - without an owning application (`owner_pid <= 0`);
/// - on a layer other than 0 (menu bar, status items, overlays);
/// - narrower or lower than [`MIN_SIDE`];
/// - in [`SYSTEM_BUNDLES`];
/// - without both a title and an application name.
///
/// Displays are always offered.
pub fn shareable(r: &MacRecord, own: u32) -> bool {
    if !r.window {
        return true;
    }
    r.owner_pid > 0
        && u32::try_from(r.owner_pid).is_ok_and(|pid| pid != own)
        && r.layer == 0
        && r.width >= MIN_SIDE
        && r.height >= MIN_SIDE
        && !SYSTEM_BUNDLES.contains(&r.bundle_id.as_str())
        && !(r.title.is_empty() && r.app_name.is_empty())
}

/// Screens first (the main display first, then the list's order), then windows in the list's order (front to back
/// as ScreenCaptureKit gives them). A window is named by its title, or its application's name when the title is
/// empty, and gets `owner: Some(pid)`. A screen is named by its display name (an empty name stays empty, as an
/// untitled X11 screen does).
pub fn to_sources(records: &[MacRecord], own: u32) -> Vec<DesktopSource> {
    let offered = || records.iter().filter(|r| shareable(r, own));
    let screens = offered()
        .filter(|r| !r.window && r.main)
        .chain(offered().filter(|r| !r.window && !r.main))
        .map(|r| DesktopSource {
            kind: SourceKind::Screen,
            id: r.id,
            title: r.title.clone(),
            owner: None,
        });
    let windows = offered().filter(|r| r.window).map(|r| DesktopSource {
        kind: SourceKind::Window,
        id: r.id,
        title: if r.title.is_empty() {
            r.app_name.clone()
        } else {
            r.title.clone()
        },
        // `shareable` let only a positive pid through.
        owner: u32::try_from(r.owner_pid).ok(),
    });
    screens.chain(windows).collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    const OWN: u32 = 100;

    fn window(id: u64, pid: i32, title: &str) -> MacRecord {
        MacRecord {
            window: true,
            id,
            owner_pid: pid,
            bundle_id: "com.example.app".to_owned(),
            app_name: "Example".to_owned(),
            title: title.to_owned(),
            layer: 0,
            width: 800.0,
            height: 600.0,
            main: false,
        }
    }

    fn display(id: u64, main: bool) -> MacRecord {
        MacRecord {
            window: false,
            id,
            owner_pid: 0,
            bundle_id: String::new(),
            app_name: String::new(),
            title: format!("Display {id}"),
            layer: 0,
            width: 1512.0,
            height: 982.0,
            main,
        }
    }

    #[test]
    fn dilla_tiny_system_and_off_layer_windows_are_not_offered() {
        let dropped = [
            ("Dilla's own", window(1, 100, "Dilla")),
            ("no application", window(2, 0, "Orphan")),
            (
                "the menu bar's layer",
                MacRecord {
                    layer: 25,
                    ..window(3, 200, "Status")
                },
            ),
            (
                "narrower than MIN_SIDE",
                MacRecord {
                    width: 40.0,
                    height: 300.0,
                    ..window(4, 200, "Badge")
                },
            ),
            (
                "system UI",
                MacRecord {
                    bundle_id: "com.apple.controlcenter".to_owned(),
                    ..window(5, 200, "Control Centre")
                },
            ),
            (
                "neither a title nor an application name",
                MacRecord {
                    app_name: String::new(),
                    ..window(6, 200, "")
                },
            ),
        ];
        for (why, r) in &dropped {
            assert!(!shareable(r, OWN), "{why} is offered");
        }
        let notes = MacRecord {
            width: 50.0,
            height: 50.0,
            app_name: "Notes".to_owned(),
            ..window(7, 200, "Notes")
        };
        assert!(shareable(&notes, OWN), "a 50 x 50 window is offered");
        assert!(shareable(&display(1, true), OWN), "a display always is");
    }

    #[test]
    fn screens_come_first_with_the_main_display_leading() {
        let records = [window(7, 200, "Notes"), display(2, false), display(1, true)];
        assert_eq!(
            to_sources(&records, OWN),
            vec![
                DesktopSource {
                    kind: SourceKind::Screen,
                    id: 1,
                    title: "Display 1".to_owned(),
                    owner: None,
                },
                DesktopSource {
                    kind: SourceKind::Screen,
                    id: 2,
                    title: "Display 2".to_owned(),
                    owner: None,
                },
                DesktopSource {
                    kind: SourceKind::Window,
                    id: 7,
                    title: "Notes".to_owned(),
                    owner: Some(200),
                },
            ]
        );
    }

    #[test]
    fn an_untitled_window_is_named_by_its_application() {
        let music = MacRecord {
            app_name: "Music".to_owned(),
            ..window(9, 300, "")
        };
        let sources = to_sources(&[music], OWN);
        assert_eq!(sources.len(), 1);
        assert_eq!(sources[0].title, "Music");
        assert_eq!(sources[0].owner, Some(300));
    }
}
