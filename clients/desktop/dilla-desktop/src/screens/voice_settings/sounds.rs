//! The synthesised call sounds (packages/web/src/call/sounds.ts `Sounds`), played through dilla-media's `Speaker`
//! (Task 19). The voice area (17) plays the join and leave ticks, the ring area (18) the ring. Stub from Task 1: Task 19
//! implements these and keeps their signatures; whether a sound may play at all is the caller's decision, as on the web.

use gpui::App;

/// sounds.ts `SoundKind`.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum SoundKind {
    Join,
    Leave,
}

/// Plays a join or leave tick.
pub fn play(_kind: SoundKind, _cx: &mut App) {}

/// Starts the ring (2 pings every 2 s, at most 15 times).
pub fn start_ring(_cx: &mut App) {}

/// Stops the ring.
pub fn stop_ring(_cx: &mut App) {}
