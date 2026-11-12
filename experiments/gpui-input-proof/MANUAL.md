# Composer IME checklist (about 10 minutes)

The automated tests cannot drive a real OS input method. Run this once per
platform on the proof app. Step 1 on every platform:

    cd experiments/gpui-input-proof && cargo run --example composer

It opens a window with the composer focused, prints each sent message to the
terminal as `SENT: "..."`, and shows the mention popup state under the
composer. On Linux, run it once under Wayland and once with
`WAYLAND_DISPLAY= cargo run --example composer` for X11. Mark each line PASS or
FAIL and note the platform and IME version.

Common to every platform: the preedit (underlined) text must appear inline at
the caret, the candidate window must sit next to the caret (not at the window
corner), and Enter while a candidate is being chosen must confirm the
candidate and NOT send the message.

## macOS, Japanese (Hiragana, Kotoba)
1. Switch to Hiragana. Type `nihongo`: preedit shows にほんご underlined.
2. Space: candidate window opens at the caret; choose 日本語 with Enter. Text
   inserted, message NOT sent.
3. Press Enter again: message sends.
4. Type `a`, then Shift+Enter, then `b`: two lines, no send.
5. Compose, then Escape: preedit is dropped, draft keeps earlier text.
6. Dead key (ABC - Extended or US International): Option+e then `e` gives é.
7. Type `@` after a space: mention popup opens; with IME on, `@` still opens it.

## Windows, Chinese (Microsoft Pinyin)
1. Switch to Chinese. Type `nihao`: preedit shows ni hao.
2. Press `1` or Space to pick 你好: inserted, not sent.
3. Press Enter with preedit open: commits the Latin preedit, not sent.
4. Press Enter with no preedit: sends.
5. Shift+Enter inserts a newline (IME idle).
6. Click elsewhere mid-composition: preedit commits or clears without a crash.
7. Dead key (US International): `'` then `e` gives é.

## Linux, fcitx5 (run on Wayland AND on X11)
Run once under a Wayland session and once under X11 (or XWayland).
1. Pinyin or Mozc: type `nihao` / `nihon`, preedit is inline, candidate window
   is near the caret.
2. Choose a candidate: inserted, not sent. Enter on idle sends.
3. Shift+Enter newline.
4. Compose in the second line of a 3-line draft: candidate window follows the
   caret line, not the first line.
5. Compose at the right edge of the window: candidate window stays on screen.
6. Dead key (`us(intl)` layout): `'` then `e` gives é. Compose key
   (Compose, `'`, `e`) gives é.
7. Paste a multi-line text with Ctrl+Shift+V and Ctrl+V: newlines kept, not sent.

## Report back
For every FAIL: platform, IME, the steps, what you saw. Update `REPORT.md`
(IME line) from `PENDING-MANUAL` to `PASS` or `FAIL`.
