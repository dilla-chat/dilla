//! Push to talk (packages/web/src/call/ptt.ts, packages/ui VoiceSettings `PUSH_TO_TALK_KEYS`, decision 2, flow 06 §
//! Keyboard): opt-in, no default key, and it acts only while the key is held and this window has the focus.
//!
//! Only a non-text key pressed on its own binds (task-8b review I2): F13 to F24, Pause, Scroll Lock and Insert. The
//! `voice.pttChord` preference stores the web's `KeyboardEvent.code` of it (`F13`, `Pause`, `ScrollLock`, `Insert`), so a
//! chord means the same on both clients; a combination with Control, Alt, Meta, Shift or AltGr could never be matched
//! and is refused too.

/// The keys push to talk may use, as `KeyboardEvent.code`.
pub const PUSH_TO_TALK_KEYS: [&str; 15] = [
    "F13",
    "F14",
    "F15",
    "F16",
    "F17",
    "F18",
    "F19",
    "F20",
    "F21",
    "F22",
    "F23",
    "F24",
    "Pause",
    "ScrollLock",
    "Insert",
];

/// Whether `code` (a `KeyboardEvent.code`) may be the push-to-talk key.
pub fn is_push_to_talk_key(code: &str) -> bool {
    PUSH_TO_TALK_KEYS.contains(&code)
}

/// The `KeyboardEvent.code` of a key as the window system names it (GPUI's lower-case key names), when it is one of
/// the push-to-talk keys.
pub fn code_of_key(key: &str) -> Option<&'static str> {
    let normal = key.to_ascii_lowercase().replace(['_', '-', ' '], "");
    let code = match normal.as_str() {
        "pause" | "pausebreak" | "break" => "Pause",
        "scrolllock" | "scroll" => "ScrollLock",
        "insert" => "Insert",
        other => {
            let n: u32 = other.strip_prefix('f')?.parse().ok()?;
            if !(13..=24).contains(&n) {
                return None;
            }
            return PUSH_TO_TALK_KEYS.get(n as usize - 13).copied();
        }
    };
    Some(code)
}

/// The modifiers held with a captured key.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct HeldModifiers {
    pub ctrl: bool,
    pub alt: bool,
    pub meta: bool,
    pub shift: bool,
}

impl HeldModifiers {
    pub fn any(self) -> bool {
        self.ctrl || self.alt || self.meta || self.shift
    }
}

/// What a key press does while the set-key button listens.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Capture {
    /// Escape (also Enter and Space, which press the button that now reads Cancel): the capture ends, nothing is set.
    Cancel,
    /// A modifier on its own: wait for the key pressed with it.
    Wait,
    /// A key that types text, moves the focus, or came with a modifier: the prompt says why and the capture goes on.
    Refused,
    /// The key to use, as its `KeyboardEvent.code`.
    Accepted(&'static str),
}

/// What `key` (GPUI's key name) with `modifiers` does as the capture's next key.
pub fn capture(key: &str, modifiers: HeldModifiers) -> Capture {
    match key {
        "escape" | "enter" | "space" | " " => return Capture::Cancel,
        "shift" | "control" | "ctrl" | "alt" | "altgr" | "meta" | "platform" | "super" | "win"
        | "function" | "fn" | "capslock" | "numlock" => return Capture::Wait,
        _ => {}
    }
    if modifiers.any() {
        return Capture::Refused;
    }
    code_of_key(key).map_or(Capture::Refused, Capture::Accepted)
}

/// Whether a stored `voice.pttChord` may be shown and bound: anything else shows as unset (task-8b ruling I2).
pub fn stored_chord(stored: Option<&str>) -> Option<&'static str> {
    let stored = stored?;
    PUSH_TO_TALK_KEYS.iter().copied().find(|c| *c == stored)
}

/// The push-to-talk key held down, with the release delay: it keeps sending that long after the key is let go. Losing
/// the window's focus releases a held key at once ([`PttHold::release`]), so the microphone never stays open behind
/// another window.
#[derive(Clone, Debug, Default)]
pub struct PttHold {
    down: bool,
    up_at_ms: Option<f64>,
}

impl PttHold {
    pub fn press(&mut self) {
        self.down = true;
        self.up_at_ms = None;
    }

    pub fn let_go(&mut self, now_ms: f64) {
        if self.down {
            self.down = false;
            self.up_at_ms = Some(now_ms);
        }
    }

    /// The window lost focus or was hidden: nothing is sent from here on, whatever the release delay.
    pub fn release(&mut self) {
        self.down = false;
        self.up_at_ms = None;
    }

    pub fn is_down(&self) -> bool {
        self.down
    }

    /// Whether the microphone sends at `now_ms` with a release delay of `release_ms`.
    pub fn sending(&self, now_ms: f64, release_ms: u32) -> bool {
        self.down
            || self
                .up_at_ms
                .is_some_and(|up| now_ms - up < f64::from(release_ms))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn only_f13_to_f24_pause_scroll_lock_and_insert_may_be_the_key() {
        for code in ["F13", "F24", "Pause", "ScrollLock", "Insert"] {
            assert!(is_push_to_talk_key(code), "{code}");
        }
        for code in [
            "F12",
            "F25",
            "KeyA",
            "Tab",
            "Space",
            "Escape",
            "ArrowLeft",
            "",
            "insert",
        ] {
            assert!(!is_push_to_talk_key(code), "{code}");
        }
        assert_eq!(PUSH_TO_TALK_KEYS.len(), 15);
    }

    #[test]
    fn window_system_key_names_map_to_codes() {
        assert_eq!(code_of_key("f13"), Some("F13"));
        assert_eq!(code_of_key("f24"), Some("F24"));
        assert_eq!(code_of_key("f12"), None);
        assert_eq!(code_of_key("f25"), None);
        assert_eq!(code_of_key("insert"), Some("Insert"));
        assert_eq!(code_of_key("pause"), Some("Pause"));
        assert_eq!(code_of_key("scroll_lock"), Some("ScrollLock"));
        assert_eq!(code_of_key("scrolllock"), Some("ScrollLock"));
        assert_eq!(code_of_key("a"), None);
        assert_eq!(code_of_key("f"), None);
    }

    #[test]
    fn the_capture_takes_the_next_chord_and_escape_cancels() {
        let none = HeldModifiers::default();
        assert_eq!(capture("f14", none), Capture::Accepted("F14"));
        assert_eq!(capture("escape", none), Capture::Cancel);
        // The button reads Cancel while it listens: Enter and Space press it.
        assert_eq!(capture("enter", none), Capture::Cancel);
        assert_eq!(capture("space", none), Capture::Cancel);
        assert_eq!(capture("shift", none), Capture::Wait);
        // Keys that type or operate the page are refused, with or without a modifier.
        assert_eq!(capture("a", none), Capture::Refused);
        assert_eq!(capture("tab", none), Capture::Refused);
        let ctrl = HeldModifiers { ctrl: true, ..none };
        assert_eq!(capture("f14", ctrl), Capture::Refused);
    }

    #[test]
    fn a_stored_chord_that_is_not_one_non_text_key_shows_as_unset() {
        assert_eq!(stored_chord(Some("Insert")), Some("Insert"));
        assert_eq!(stored_chord(Some("KeyA")), None);
        assert_eq!(stored_chord(None), None);
    }

    #[test]
    fn the_release_delay_keeps_sending_after_the_key_is_let_go() {
        let mut hold = PttHold::default();
        assert!(!hold.sending(0.0, 200));
        hold.press();
        assert!(hold.sending(10.0, 200));
        hold.let_go(100.0);
        assert!(hold.sending(299.0, 200));
        assert!(!hold.sending(300.0, 200));
        // A new press cancels the tail; losing focus ends everything at once.
        hold.press();
        hold.let_go(500.0);
        hold.release();
        assert!(!hold.sending(501.0, 200));
        assert!(!hold.is_down());
    }

    #[test]
    fn a_zero_release_delay_stops_with_the_key() {
        let mut hold = PttHold::default();
        hold.press();
        hold.let_go(10.0);
        assert!(!hold.sending(10.0, 0));
    }
}
