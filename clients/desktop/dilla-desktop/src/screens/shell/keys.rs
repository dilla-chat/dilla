//! The shell's key bindings (Task 8): `crate::keys::bind_all` calls [`bind`] once per app. They apply inside the shell
//! (key context `Shell`), so a text field's own bindings, deeper in the tree, win.
//!
//! - Alt+Up and Alt+Down select the previous and next listed channel (Shell.tsx, from anywhere on a channel route).
//! - Alt+Left and Alt+Right go back and forward in the route history (the browser's history in the web client).
//! - Ctrl+, (Cmd+, on macOS, and only Cmd+, there: [`settings_bindings`]) opens the user settings (the settings host,
//!   Task 1b).
//! - Ctrl+End jumps the open conversation to its newest message while it is away from it (Conversation.tsx's "jump to
//!   present"); a text field keeps the chord (it moves the caret), an overlay keeps it. Ctrl on macOS too: the web returns
//!   on `metaKey`, so Cmd+End does nothing.
//! - Ctrl+Shift+Enter (Cmd+Shift+Enter) starts the open DM's call (Shell.tsx `dmCallKey`), as its header button does.
//!
//! The rail, the channel list and the splitters take their own keys where they are focused (roving arrows, Enter, Home
//! and End); Tab and Shift+Tab are the shared focus keys of `crate::screens::bind_keys`.

use gpui::{App, KeyBinding, actions};

actions!(
    dilla_shell,
    [
        PreviousChannel,
        NextChannel,
        HistoryBack,
        HistoryForward,
        OpenSettings,
        JumpToPresent,
        CallOpenDm
    ]
);

/// The key context of the shell's root.
pub const CONTEXT: &str = "Shell";

/// Review L m3 (Shell.tsx returns early on `event.repeat`): a chord held down acts once. GPUI hands a binding's action
/// no `is_held`, so the latch closes when the chord acts and opens on the chord key's release. A release the shell never
/// sees (the focus left it meanwhile) cannot hold it shut: the latch also opens once no repeat came for [`Self::QUIET`].
#[derive(Debug, Default)]
pub struct RepeatLatch {
    last: Option<std::time::Instant>,
}

impl RepeatLatch {
    /// Auto-repeat comes every few tens of ms after a first delay of at most about 0.7 s.
    pub const QUIET: std::time::Duration = std::time::Duration::from_secs(1);

    /// The chord came at `now`: whether it acts (a fresh press) or is a held repeat.
    pub fn press(&mut self, now: std::time::Instant) -> bool {
        let repeat = self
            .last
            .is_some_and(|t| now.saturating_duration_since(t) < Self::QUIET);
        self.last = Some(now);
        !repeat
    }

    /// The chord's key went up.
    pub fn release(&mut self) {
        self.last = None;
    }
}

/// Registers the shell's bindings.
pub fn bind(cx: &mut App) {
    cx.bind_keys(settings_bindings(cfg!(target_os = "macos")));
    cx.bind_keys([
        KeyBinding::new("alt-up", PreviousChannel, Some(CONTEXT)),
        KeyBinding::new("alt-down", NextChannel, Some(CONTEXT)),
        KeyBinding::new("alt-left", HistoryBack, Some(CONTEXT)),
        KeyBinding::new("alt-right", HistoryForward, Some(CONTEXT)),
        // Task 28: the two rows of Settings -> Keyboard that had no binding.
        KeyBinding::new("ctrl-end", JumpToPresent, Some(CONTEXT)),
        KeyBinding::new("ctrl-shift-enter", CallOpenDm, Some(CONTEXT)),
        KeyBinding::new("cmd-shift-enter", CallOpenDm, Some(CONTEXT)),
    ]);
}

/// Integration 3 (the settings host): Cmd+, on macOS, Ctrl+, elsewhere, opens the user settings. One chord per OS: the
/// macOS app menu's Settings… row shows the action's first binding (GPUI's `find_or_first`, none of which match its
/// default context), so a Ctrl+, bound there drew "⌃," instead of "⌘," (the macOS UI check of 2026-11-16).
pub fn settings_bindings(mac: bool) -> Vec<KeyBinding> {
    vec![KeyBinding::new(
        if mac { "cmd-," } else { "ctrl-," },
        OpenSettings,
        Some(CONTEXT),
    )]
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The macOS app menu's Settings… row draws the action's first binding: it must be Cmd+, (not Ctrl+,), and the
    /// other systems keep Ctrl+,.
    #[gpui::test]
    fn settings_is_cmd_comma_on_macos_and_ctrl_comma_elsewhere(cx: &mut gpui::TestAppContext) {
        let first = |mac: bool| {
            let bindings = settings_bindings(mac);
            assert_eq!(bindings.len(), 1, "one chord per OS");
            bindings[0].keystrokes()[0].inner().clone()
        };
        assert_eq!(first(true), gpui::Keystroke::parse("cmd-,").unwrap());
        assert_eq!(first(false), gpui::Keystroke::parse("ctrl-,").unwrap());
        // As registered: the first binding the keymap holds for OpenSettings is this OS's chord.
        cx.update(bind);
        let want = gpui::Keystroke::parse(if cfg!(target_os = "macos") {
            "cmd-,"
        } else {
            "ctrl-,"
        })
        .unwrap();
        cx.update(|cx| {
            let keymap = cx.key_bindings();
            let keymap = keymap.borrow();
            let shown = keymap
                .bindings_for_action(&OpenSettings)
                .next()
                .map(|b| b.keystrokes()[0].inner().clone());
            assert_eq!(shown, Some(want));
        });
    }
    use std::time::{Duration, Instant};

    /// Review L m4: Conversation.tsx returns on `metaKey`, so the jump to present is Ctrl+End only; Cmd+End (unclaimed by
    /// the text field on macOS) does nothing, as on the web.
    #[gpui::test]
    fn the_jump_to_present_is_ctrl_end_only(cx: &mut gpui::TestAppContext) {
        cx.update(bind);
        fn bound(cx: &mut gpui::TestAppContext, k: &'static str) -> bool {
            let input = [gpui::Keystroke::parse(k).unwrap()];
            cx.update(|cx| {
                cx.all_bindings_for_input(&input)
                    .iter()
                    .any(|b| b.action().partial_eq(&JumpToPresent))
            })
        }
        assert!(bound(cx, "ctrl-end"));
        assert!(!bound(cx, "cmd-end"));
    }

    /// Review L m3: a held Ctrl+Shift+Enter starts the DM call once; its auto-repeats do nothing until the key goes up.
    #[test]
    fn a_held_chord_acts_once() {
        let mut latch = RepeatLatch::default();
        let t0 = Instant::now();
        assert!(latch.press(t0), "the first press acts");
        for n in 1..40 {
            assert!(
                !latch.press(t0 + Duration::from_millis(500 + 33 * n)),
                "repeat {n}"
            );
        }
        latch.release();
        assert!(latch.press(t0 + Duration::from_secs(3)), "a new press acts");
        // A release the shell never saw: the next press after a quiet spell acts.
        assert!(latch.press(t0 + Duration::from_secs(3) + RepeatLatch::QUIET));
    }
}
