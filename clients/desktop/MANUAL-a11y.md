# Screen readers: the real-machine checks (Package 11)

CI checks the accessibility tree GPUI builds (`dilla-desktop/tests/a11y.rs`, `tests/a11y_components.rs`): roles,
names, states, landmarks, modality, live regions, the tab order of every parity fixture, and the 24 px target rule.
What CI cannot check is the platform adapter and the screen reader on top of it: AccessKit's AT-SPI adapter on Linux,
NSAccessibility on macOS and UI Automation on Windows. These walkthroughs are that check, once per release candidate
and after any change to `core/vendor/gpui-pre/src/window/a11y.rs` or `dilla-desktop-ui/src/a11y.rs`.

Use a fresh instance and two accounts (A on this machine, B on another device or the web client) in one server with a
text channel `general` and a voice channel `lounge`.

## Set-up

| OS | Reader | Start | Notes |
| --- | --- | --- | --- |
| Linux, GNOME on Wayland | Orca | Super+Alt+S | `gsettings get org.gnome.desktop.interface toolkit-accessibility` is `true`. |
| Linux, GNOME on X11 | Orca | Super+Alt+S | Log in with "GNOME on Xorg"; repeat the walkthrough. |
| macOS | VoiceOver | Cmd+F5 | VO = Ctrl+Option. Use the rotor (VO+U) for landmarks and headings. |
| Windows | NVDA | Ctrl+Alt+N | Browse mode is off in apps: use Tab, then NVDA+Space if a list needs it. Narrator (Win+Ctrl+Enter) once too. |

## 1. Sign in

1. Start the app with the reader running. The window is announced as "Dilla" (the window title).
2. Tab to the address field: the reader says its label ("Server address" or the shown label), "edit".
3. Type the address, Tab to the action: "Continue, button". Enter.
4. Password field: the reader says "password" and **never** reads the typed characters back as text (it says "star"
   or "bullet", or nothing).
5. Code from the app: the reader says "Code from the app, edit" and the hint "Six digits…".
6. A wrong code: the refusal is read at once without moving the focus ("That code did not match…").
7. The recovery key step (a new device): the field is named "Recovery key". The key shown at sign-up is **not** read
   out (by design: Copy and Print are offered; check both are announced as buttons).

## 2. Find your way and send

1. In the shell, the reader's landmark list (Orca: Insert+Ctrl+L; VoiceOver rotor "Landmarks"; NVDA: D) lists:
   banner, navigation "servers", navigation "channels", main, log "messages in #general", region "your account".
2. Tab order from the top bar: "Search or jump to…" → "Reminders" → "settings" → the server tile ("Midgard Crew") →
   "server settings" → the channel row ("general, link, selected"; with unread: "general, unread 3, mentions 1") →
   "mute" → "deafen" → "Threads" → "Members, 3" → the composer "message #general" → "send" → "Code block".
3. Arrow keys in the channel list move between channels and the reader names each.
4. Type a message in the composer and press Enter. Nothing is announced for your own message.
5. Have B send "hello" to `#general`: the reader announces "B, 10:42: hello" once, without moving the focus.
6. With the reader's own cursor (Orca flat review, VO+arrows, NVDA object navigation) the log's messages read as
   "author, time: text", one per message.

## 3. Join a call

1. Select `lounge`: the channel row says "lounge, connected" once joined.
2. On joining the reader announces "You joined lounge." Leaving: "You left lounge."
3. Toggle mute in the panel (or its key): "Microphone muted." / "Microphone on."; deafen: "Sound off." / "Sound on."
4. The call bar is a region "call controls"; each control is a button named by its word ("mute", "camera", "share
   screen", "leave"), and mute and deafen say "pressed" / "not pressed".
5. The sidebar dock is a region "voice controls".

## 4. Answer a ring

1. Have B call A (a DM call). The reader interrupts with the ring text ("B is calling", assertive) once.
2. The ring banner is a region "incoming calls"; Tab reaches "Answer" and "Decline" buttons.
3. Answer with Enter: the call starts and "You joined the call with B." is announced.
4. Ring again and Decline: nothing more is read, the banner is gone, focus returns to where it was.

## 5. Dialogs and menus

1. Open a channel's menu (Shift+F10 on the row): the reader says "menu", then each item ("Mark as read, menu item";
   a check item "Mute channel, check box, not checked").
2. Escape closes it and the focus is back on the row.
3. Open server settings: the reader says the dialog's name (the section, e.g. "Overview, dialog") and that it is
   modal (VoiceOver: "web dialog"/"dialog"; NVDA: "dialog"). The section list is navigation "Settings sections";
   the current section says "selected".
4. A delete confirmation is an "alert dialog" named by its title and described by its body.
5. A toast ("Copied") is announced once each time, also when the same toast repeats.

## Record

For each OS, note the reader and version, the app commit, and any step that failed with what the reader said. A
failure in step 1, 2 (items 1, 2, 5), 3 (item 2) or 4 (item 1) blocks the switch (Decision D3).
