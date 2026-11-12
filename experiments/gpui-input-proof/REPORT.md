# gpui-input-proof: composer text input

VERDICT: PASS (automated). IME: PENDING-MANUAL (see MANUAL.md).

Run: `cd experiments/gpui-input-proof && cargo test`.

## What is proved

A multi-line composer (`Composer`, `src/lib.rs`) on gpui-component 0.9.1
`Textarea`/`TextareaState` (gpui-pre 0.3.12), driven headlessly on GPUI's test
platform (`#[gpui::test]`, `simulate_keystrokes`, `simulate_input`, clipboard,
`EntityInputHandler`).

| Test | Result |
|---|---|
| `enter_sends_shift_enter_newlines` | pass |
| `dead_key_e_acute` | pass |
| `paste_multiline_keeps_newlines` | pass |
| `emoji_insert_at_caret` | pass |
| `mention_trigger_opens_popup_at_at_sign` | pass |
| `mention_at_rules` (pure helper) | pass |

clippy `--all-targets -D warnings` clean; `cargo deny check licenses` ok.

## Findings

- `TextareaState::submit_on_enter(true)` gives the web behaviour: plain Enter
  emits `InputEvent::PressEnter { shift: false }` with no newline, Shift+Enter
  inserts `\n`.
- Gotcha: with `submit_on_enter` the textarea calls `cx.propagate()` on plain
  Enter, so GPUI then also types the key's `"\n"` into the draft (first run
  sent `"hi\nthere\n"`), and because subscriptions run after the update the
  send also saw the IME composition already ended. Fix: the owner swallows
  `Enter` with `.on_action(|_: &Enter| {})` on a parent element.
- IME guard: Enter while `marked_text_range()` is Some does not send (mirrors
  the web composer's `isComposing` check); tested with a marked dead key.
- Mention trigger: `@` at start of text or after whitespace; query is the
  non-whitespace run after it; `me@x` does not trigger; whitespace closes it.
  The popup is a stub (`@query` div); caret anchoring (`cursor_bounds()` exists)
  and list filtering are out of scope.
- Paste uses the textarea's own Paste action; newlines survive, nothing sends.
- Emoji insert at caret keeps UTF-8 valid and the caret after the insert.

## Limits

- The test platform has no OS input method. Dead keys are simulated as a
  platform does: `replace_and_mark_text_in_range("´")` then
  `replace_text_in_range("é")` via `EntityInputHandler`. Real preedit
  rendering, candidate-window placement and macOS/Windows/Wayland/X11 IME
  behaviour are untested: IME is `PENDING-MANUAL`.
- Backspace over a skin-tone emoji is only asserted to keep valid UTF-8 and
  surrounding text, not grapheme-cluster deletion.
- No real window; layout and popup placement unverified.

## Build notes

- Standalone workspace with its own committed `Cargo.lock`. The root
  `Cargo.lock` cannot be reused: it pins `taffy 0.15.1` while `gpui-fast`
  (a gpui-component dependency) needs `=0.15.0`; the root lock's GPUI entries
  look stale for any workspace that includes `dilla-desktop-ui`.
- dev-dependencies enable `gpui/test-support` and `gpui-component/test-support`.
