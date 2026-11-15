//! Operating-system permissions (Package 7, decision D1: OS prompts only, no per-instance in-app prompts).
//!
//! To do (Package 7): check the OS microphone, camera and screen-recording permission, and show a denied one as the
//! listen-only or no-camera notice. The macOS usage strings (`NSMicrophoneUsageDescription`, camera, screen recording)
//! go into the Info.plist in the release plan's Task 12.

use gpui::App;

/// Called once by [`super::install`], first. Task 0: nothing yet.
pub fn install(_cx: &mut App) {}
