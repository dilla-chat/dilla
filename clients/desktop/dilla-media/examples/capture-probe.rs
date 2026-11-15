//! The share sources on this machine, for a manual check (clients/desktop/MANUAL.md, macOS and Windows):
//!
//! ```text
//! capture-probe screens|windows        lists the sources, then captures the first one for 3 seconds
//! capture-probe portal                  (Wayland) opens the system portal's dialog and captures the choice
//! capture-probe camera                  lists the cameras and captures the first one for 3 seconds
//! ```
//!
//! It prints each source, then each frame's size and how many frames came; a source that ends says so.

use std::time::{Duration, Instant};

use dilla_media::sources::desktop::{self, SourceKind};
use dilla_media::{CaptureSource, Pull};

fn run(mut source: Box<dyn CaptureSource>) {
    println!("opened at {:?}", source.size());
    let until = Instant::now() + Duration::from_secs(3);
    let mut frames = 0;
    while Instant::now() < until {
        match source.pull() {
            Pull::Frame(f) => {
                frames += 1;
                if frames % 15 == 1 {
                    println!("frame {}x{} {:?}", f.width, f.height, f.format);
                }
            }
            Pull::Idle => {}
            Pull::Ended => {
                println!("ended");
                break;
            }
        }
        std::thread::sleep(Duration::from_millis(33));
    }
    println!("{frames} frames");
}

fn main() {
    let what = std::env::args().nth(1).unwrap_or_else(|| "screens".into());
    let open = |s: Option<&desktop::DesktopSource>, wait| {
        desktop::open(s, 30, wait, &|| false).map(|c| Box::new(c) as Box<dyn CaptureSource>)
    };
    let result = match what.as_str() {
        "portal" => open(None, desktop::PORTAL_WAIT),
        "camera" => camera(),
        kind => {
            let kind = if kind == "windows" {
                SourceKind::Window
            } else {
                SourceKind::Screen
            };
            let list = desktop::list(kind);
            for s in &list {
                let thumb = desktop::thumbnail(s, 320, 180).map(|t| (t.width, t.height));
                println!("{:?} {} {:?} thumbnail {thumb:?}", s.kind, s.id, s.title);
            }
            match list.first() {
                Some(s) => open(Some(s), desktop::START_WAIT),
                None => {
                    println!("no {kind:?} sources");
                    return;
                }
            }
        }
    };
    match result {
        Ok(source) => run(source),
        Err(e) => println!("did not open: {e}"),
    }
}

#[cfg(any(target_os = "linux", target_os = "windows"))]
fn camera() -> Result<Box<dyn CaptureSource>, dilla_media::sources::OpenError> {
    use dilla_media::sources::camera;
    println!("{:?}", camera::devices());
    camera::open(None, desktop::START_WAIT, &|| false)
        .map(|c| Box::new(c) as Box<dyn CaptureSource>)
}

#[cfg(not(any(target_os = "linux", target_os = "windows")))]
fn camera() -> Result<Box<dyn CaptureSource>, dilla_media::sources::OpenError> {
    Err(dilla_media::sources::OpenError::Unavailable(
        "no camera source on this system yet".to_owned(),
    ))
}
