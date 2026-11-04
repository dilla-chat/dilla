# Flow 13: desktop call, shell and sign-in order (v0.1.0-web3.8)

Desktop layout with the call panel at the bottom left above the user panel, stage full-screen capability, sign-in in a new order (login, factor, key), and recovery from expired assertions on the key path.

## States

| State | What shows | Copy keys |
|---|---|---|
| 1. Desktop in a call, stage closed | The call panel above the user panel. | `voice.mute`, `voice.deafen`, `panel.region`, `panel.settings` |
| 2. Stage open | The call panel, the user panel, and the stage toolbar pinned bottom centre. | `stage.fullscreen` |
| 3. Speaking on own and remote tiles | Speaking state shown on own audio or video tile and remote tiles, and on the nested rows. | `voice.state.speaking` |
| 4. Desktop link offline and connecting | One line, no status bar. Shows `nav.link.offline` and `nav.link.connecting`. | `nav.link.offline`, `nav.link.connecting` |
| 5. Settings → Diagnostics | Holds node, link, generation and devices. | `settings.nav.diagnostics` |
| 6. Version refusal | The version close refusal shows `shell.banner.clientTooOld` with `reload.action`. | `shell.banner.clientTooOld`, `reload.action` |
| 7. Sign-in key step after the factor | The sign-in form shows the key step with keys `signin.key.*` and `signIn.key.isBackupCode`. | `signin.key.body`, `signIn.key.isBackupCode` |
| 8. Assertion expired | The assertion expires and returns to sign-in login step, showing `enrol.expired`. | `enrol.expired` |
| 9. A splitter focused and dragged | Splitter shows `pane.resize.sidebar` and `pane.resize.aside`. | `pane.resize.sidebar`, `pane.resize.aside` |
| 10. Channels tab with the "+", and without it | With the "+" (`create.channel.open`) for a member who may manage channels; without the "+" for one who may not: the tab shows the same list and no add control. | `create.channel.open` |
| 11. One channel list with a live voice channel and its people nested | Inline state glyphs. | `voice.state.muted`, `voice.state.deafened`, `voice.state.speaking`, `voice.state.camera`, `voice.state.screen` |
| 12. User panel outside a call, muted | Shows `panel.region` and `panel.settings`, `voice.mute`, `voice.deafen`. | `panel.region`, `panel.settings`, `voice.mute`, `voice.deafen` |
| 13. Stage grid with separate camera and screen tiles | Camera and screen shown as separate tiles with labels like `stage.screen`. | `stage.screen` |
| 14. A focused tile with the filmstrip | The focused tile fills the stage and the other tiles sit in the filmstrip; each tile's focus button carries `aria-pressed`, true on the focused tile. | — |
| 15. A tile in full screen | The tile fills the screen; the full screen button (`stage.fullscreen`) carries `aria-pressed` true, and Escape leaves it. | `stage.fullscreen` |
| 16. User panel, status menu | The avatar and name block is a button named `panel.status`; it opens a menu of `presence.online`, `presence.idle`, `presence.dnd` and `presence.invisible` (a controller ruling that supersedes R24), and the avatar carries the current status as a dot (invisible as the off ring). The old sidebar select is gone on the desktop. | `panel.status`, `presence.online`, `presence.idle`, `presence.dnd`, `presence.invisible` |

## Keyboard map

| Key | Where | Effect |
|---|---|---|
| Mute/Deafen shortcut | User panel | Ctrl/⌘+Shift+M and Ctrl/⌘+Shift+D, in and out of a call; the panel's mute and deafen buttons carry `aria-keyshortcuts` with the same keys |
| ArrowLeft, ArrowRight | Splitter | ±1rem (the aside's ArrowLeft widens it) |
| Shift+ArrowLeft, Shift+ArrowRight | Splitter | ±4rem |
| Home | Splitter | Minimum |
| End | Splitter | Maximum |
| Enter | Splitter | Reset |
| Enter or Space | Tile focus button | Focuses the tile |
| Enter or Space | Focused tile | Returns to the grid |
| Escape | Inside the stage outside full screen | Returns to the grid |
| Tab | Desktop | Rail → sidebar → sidebar splitter → bottom panel (call panel, then user panel: name block, mute/deafen toolbar, gear) → main |
| Enter, Space or ArrowDown | User panel name block | Opens the status menu on its first item (ArrowUp on the last); arrows move, Escape closes and refocuses the name block |
| (none) | `.d-user-panel` | The wrapper has no accessible name and is located by that class; its toolbar is named `panel.region`, so "your account" is not named twice, and its name block is the status button, `panel.status` |

## Focus rules

- Joining a call focuses the stage's first control.
- Leaving a call focuses the voice row.
- Closing Settings focuses the user panel's gear.
- Unfocusing a tile keeps focus on its focus button.

## Announcements

- The link banner (`nav.link.offline`, `nav.link.connecting`) is `role="status"`.
- No new announcement for focus or full screen: `aria-pressed` carries both.

## Copy

- `enrol.expired='Adding this browser took longer than 5 minutes, so the sign-in ran out. Sign in again.'`
- `signin.key.body='This browser is new. Paste your recovery key to read your conversations here.'` (true on the key step as step 2 or step 3)
- `reload.action='reload'`
- `pane.resize.sidebar='resize the channel list'`
- `pane.resize.aside='resize the side panel'`
- `panel.region='your account'`
- `panel.settings='user settings'`
- `panel.status='your status: {status}'` (`{status}` is `presence.online`, `presence.idle`, `presence.dnd` or `presence.invisible`)
- `stage.screen='{name}’s screen'`
- `stage.fullscreen='full screen'`
