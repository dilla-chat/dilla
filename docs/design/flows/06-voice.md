# Flow 06 — voice, DM calls, sounds and media players

This flow extends Flow 04's shell order and Flow 05's overlays: skip link → rail → channel list (with `active voice` above the channels, and the voice dock above the user panel) → header → ring banner (while a ring lasts) → stage → log → composer → status bar. Chrome labels use lower case; dialog copy uses sentence case; nothing ends in an exclamation mark. *Voice* names a channel's room and *call* names a DM or group-DM call only. Every visible phrase below comes from the Copy section, with `{name}` filled from the contact's display name or username, `{names}` a comma-separated list of them, `{where}` the voice channel's name or the DM's name, and `{n}` a count. Frame boxes describe width constraints, not pixel-perfect components.

The media key never appears in this flow: the browser derives it in the core worker, hands it to the page once per epoch and the page hands it to the media worker (decision 3). Nothing on screen says "encrypted"; the call code is the one visible trust surface, behind a closed disclosure (decision 9).

## States

| state | shown | worker fact | announcement |
|---|---|---|---|
| idle | voice rows with `voice.join` on focus or hover; no dock | `call.phase` idle | none |
| AFK (since v0.1.0-web3.9) | replaced in v0.1.0-web3.11: the AFK channel hosts no call and lists who is away (flow 14). Until then: the AFK channel is an ordinary voice call, joined listen-only (see flow 14); every grant is listen-only, no share; a device idle in voice joins it muted and shows `mic-off` everywhere | `call.phase` in, listen-only (no publish grant) | none; the join is automatic or announced by the AFK move (flow 14) |
| no one in voice | the row shows no sub-rows and no count; the stage says `voice.empty` | `voice[channel]` absent | none |
| live, not joined | the row shows `voice.count` and one sub-row per person with their state words; `voice.join` / `call.join` | `voice[channel]` has participants, `call.phase` idle | none |
| joining | dock shows `voice.dock.connecting`; the stage controls appear but stay `aria-disabled` | `call.phase` joining | none |
| rejoining | as joining | `call.phase` joining with `call.rejoining` true: the join waits, at most 30 s, for the Remove of this device's earlier leaf to be committed (the instance refuses the target of an outstanding Remove with `E_REMOVE_PENDING`); a person alone ends the call as they leave (only while the instance finds no other device in it), so their rejoin never waits | `voice.announce.rejoining`, once, in the dock's status region |
| connected | dock shows the channel name over the link stats (`voice.dock.ping` or `voice.dock.pingSub`, and `voice.dock.rate`, or `voice.dock.pingNone` before the first sample; MP5); tiles for everyone; the toolbar is live | `call.phase` in | `voice.announce.joined` |
| reconnecting | dock shows `voice.dock.reconnecting`; tiles keep their last state | page room reconnecting, `call.phase` in | none |
| audio held back (added 2026-10-14, web3.3) | dock shows `voice.audioBlocked` with its `voice.audioStart` button above the controls; one press starts call audio and the line goes; the rest of the call is unchanged | the call’s own AudioContext not running, or the page room’s `canPlaybackAudio` false (the browser did not let the call start its sound: audio the browser or the system suspended, or a stricter autoplay setting in the browser), `call.phase` in | `voice.audioBlocked`, once per change to held back, in the dock’s status region (with `voice.announce.joined` when the call starts held back); the line itself is no status region |
| blocked before join | `voice.blockedChanged` (the changed contact the core names from the call tree, or one in the call roster) or `voice.blockedJoin`; no join button until the cause is gone (the contact verified: the join is offered again without a reload) | `call.phase` blocked, `call.error.code` E_UMK_CHANGED (with `call.changedUser`) or another block code | the notice (focus moves to it once) |
| blocked mid-call | `voice.blocked`; the dock and stage go away; the row returns to live, not joined | `call.phase` blocked, own leaf left | the notice (focus moves to it once) |
| paused mid-call (RV-3) | banner `voice.paused` on the stage and the dock with its `voice.pausedAction` button; dock says `voice.dock.paused`; tiles show no media, only names and state words; camera and share buttons `aria-disabled` | `call.phase` paused, `call.error.code` E_UMK_CHANGED, `call.changedUser` the changed contact, still in the group and the room, no keys posted | the banner (`role="status"`; focus moves to its button once); `voice.resumed` when it ends |
| held mid-call (added 2026-10-14, WB security I1) | banner `voice.held` on the stage and the dock, no action; otherwise as paused; the page drops its media keys | `call.phase` paused, `call.error.code` other than E_UMK_CHANGED (E_DEVICE_LIST_STALE, or the core's code for any other refusal of a new epoch), `call.changedUser` null; no key posted until the current epoch's | the banner (`role="status"`); `voice.resumed` when it ends |
| removed | `voice.removed`; the dock goes away | `call.phase` removed | the notice |
| removed from the server (added 2026-10-14, product walk I8) | `voice.removedNotMember`; no join button on the stage or the voice row; this device's own entry is gone from the voice list at once | `call.phase` removed with `call.error.code` E_NOT_FOUND: after the removal the worker reads the channel's call again and the instance no longer lets this device see it | the notice |
| connection lost or call ended (added 2026-10-14, WB client I3) | `voice.disconnected` when LiveKit's connection ended and its reconnect gave up, `voice.ended` when the room was closed (the call ended for everyone), `voice.removed` when the instance removed this device from the room; the dock and the stage's controls go away | the page posts `callLeave` with reason `lost`; `call.phase` removed with `call.error.code` E_DISCONNECTED | the notice, in the stage's region when the stage shows, else in the status region that outlives the dock |
| left while away (added 2026-10-21, late rejoin) | `voice.leftWhileAway` with the join button; the dock and the rejoining banner go away; the join is an ordinary join | a lost connection (`E_DISCONNECTED`) of a server's voice channel whose device was gone longer than that server's AFK timeout (`afk:<cid>` `timeoutSeconds`, read fresh before the rejoin), measured in the page from the last moment it was known to be there (before a suspend found as a gap of more than 5 s in its once-a-second tick, held until the call reconnects after it or stays connected for 60 s after the wake without reconnecting; else the moment the loss was noticed) by the larger of `performance.now()` and the wall clock. A shorter drop rejoins by itself as before; a pending reason 1 or 2 wins (`afk.moved`, `afk.elsewhere`); a server with no AFK channel (its sweep is off), a failed read and a DM call keep today's rejoin and retries; an upgrade's restore keeps its 120 s window | the notice, in the stage's region when the stage shows, else in the status region that outlives the dock |
| peer's audio on a spare receiver (added 2026-10-22, Firefox only) | nothing new: the peer's tile shows not heard until their audio returns, about 4 s after they joined or came back | the page's media layer saw, 3 s after subscribing to a peer's unmuted audio, no packet on the receiver LiveKit bound while a receiver no track was bound to got RTP; it re-subscribed that peer once and noted `E_MEDIA_MISROUTE` (`callNote`), which Settings → Diagnostics lists | none |
| full | `voice.full` (or `voice.sharersFull` when the camera or share is refused) | `call.error.code` E_CALL_FULL / E_CALL_SHARERS_FULL | the notice |
| failed | `voice.failed` with the join button available again | `call.phase` failed (also E_DEVICE_LIST_STALE after one refresh, RV-1) | the notice |
| share without sound (added 2026-10-21, #37) | the sharer's stage notice says the one of `voice.shareAudio.none.unsupported`, `voice.shareAudio.none.tabOnly` and `voice.shareAudio.none.tabOrSystem` that this browser can act on (`shareAudioAdvice`: Firefox and Safari, Chromium on Linux, Chromium on Windows, ChromeOS and macOS; `role="status"`, no focus move, nothing shown before the picker); it goes when the share stops or gets sound. Each viewer's tile for that screen shows the glyph-only `volume-off` state named `voice.state.noSound` (`volume-x` is only the pressed mute button: "muted by you") in its caption, which may wrap; the sharer's own tile never shows it | `PageCallState.shareAudio` is `none` (the browser gave no audio for the surface, the grant did not allow it, or the audio track ended); a viewer's tile has `screenAudio` false (no `screen_share_audio` publication) | the notice (the stage's status region); the mark is a `role="img"` named `voice.state.noSound` |
| stream volume and mute (added 2026-10-21, #37) | a remote screen tile with sound has its own range (`voice.streamVolume`, 0 to 200 in steps of 10, shown on hover and focus on the stage, always in the flow on the phone) and a mute toggle (`voice.streamMute`, `aria-pressed`, accent-outlined while pressed, the label never swaps; 40 px, 44 px on the phone). On the phone a screen tile shows only the 44 px mute and a 44 px volume button with the value; the button (`aria-expanded`, named `voice.streamVolume`) opens a small popover anchored to the tile with the range: focus moves to the range, Escape or a press outside closes it and focus returns to the button with the `volume-2` glyph, `volume-x` while pressed. The mute is the stream volume at 0 and unmute brings back the last value; a muted stream keeps its pressed mute shown and never the no-sound mark (the mark is about the stream, not the viewer's choice). Deafen and pause still hold it back | `voice.streamVolume.<user>` (per user, local to this account; one gain path with the stream volume; the voice range `voice.volume.<user>` is separate) | none |
| microphone denied | `voice.micDenied`; joined listen-only | `getUserMedia` NotAllowedError | the notice |
| microphone missing | `voice.micMissing`; joined listen-only | `getUserMedia` NotFoundError | the notice |
| microphone failed | `voice.micFailed`; joined listen-only | the page call's `mic` is `failed` (`E_MIC_PUBLISH`: the microphone exists but could not be published; added 2026-10-13, task-8b review m4) | the notice |
| unsupported browser | dialog `voice.unsupported.title` / `voice.unsupported.body` with `voice.unsupported.close` | a required API (insertable streams or `RTCRtpScriptTransform`, `AudioWorklet`) is missing | dialog title |
| other tab | `voice.otherTab`; no join | Flow 05's other-tab state | the notice |
| incoming ring — normal | ring banner `call.ring.incoming` with `call.ring.answer` and `call.ring.decline`; the ring sound plays; a desktop notification `call.ring.notifyTitle` / `call.ring.notifyBody` when the tab is hidden | `rings` holds the ring, `busy` and `quiet` false | `role="alert"` once |
| incoming ring — busy | ring banner `call.ring.busy`; no sound, no notification; answering leaves the current voice first | `rings` entry with `busy` true | `role="alert"` once |
| incoming ring — quiet | ring banner `call.ring.incoming`; no sound, no notification (R1) | `rings` entry with `quiet` true (the DM is muted) | `role="alert"` once |
| outgoing ring | the DM's call strip says `call.outgoing.ringing`; a participant the instance did not ring (its pair bound held the ring back) is `call.outgoing.notRung` instead; a decline adds `call.outgoing.declined`; when every ring has ended without an answer (30 s from the start, or nobody left ringing), `call.outgoing.noAnswer`; no ringback tone (R3) | `call.outgoing` (`ringing`, `declined`, `notRung`; the start's `rung` element, 09 § Voice) | strip text through the dock's status region |
| unanswered (added 2026-10-14, product walk I6) | when every ring has ended and nobody else was ever in the call, the caller leaves by itself 5 s later (the grace covers an answer still connecting); the strip keeps the ring's last line (`call.outgoing.noAnswer` or `call.outgoing.declined`) as its notice, with `call.start` | `call.outgoing` final, the page's tiles empty since the start | `voice.announce.left`, then the strip's notice |
| attachment player — idle | the attachment card shows `media.play` and `media.save` | `AttachmentSummary.kind` audio or video, not `tooLarge` | none |
| attachment player — loading | `media.loading` in place of the play button | the page is opening the blob | none |
| attachment player — ready | the native `<audio controls>` or `<video controls playsinline>` with the file's shown name as its accessible name; playback starts only on the explicit play | a page-minted `blob:` URL, revoked on unmount | none |
| attachment player — error | `media.error` with `media.save` | the element fired `error`, or the blob did not open | the error text |
| Settings → Notifications → sounds | two switches, `settings.sound.voice` and `settings.sound.ring`, both on by default | `sound.voice`, `sound.ring` (L-TS-54) | none |

A ring reaches only an open dilla tab (R8). A ring ends on every device of that user at the first of: one of their devices joins, they decline, the call ends, or 30 seconds pass (R5); the banner, the sound and the notification all stop then. Each other participant of a group DM is rung independently (R6). A muted DM rings with the banner only (R1). A device already in a call shows the busy banner and plays nothing.

How a block code maps to copy:

- At join: `E_UMK_CHANGED` → `voice.blockedChanged` when exactly one contact in the call roster is changed, else `voice.blockedJoin`.
- Mid-call: `E_UMK_CHANGED` → phase `paused` and `voice.paused` with the changed contact's name (the first by `user_id` when several changed); `voice.pausedAction` opens Privacy's comparison for that contact.
- Any other block → `voice.blocked` mid-call and `voice.blockedJoin` at join.
- `E_DEVICE_LIST_STALE` (phase `failed`, a list that could not be fetched, ruling RV-1) → `voice.failed` at join. Mid-call (amended 2026-10-14, WB security I1): a new epoch refused as pending, or for any other reason that is not a block or a changed identity, holds the call (phase `paused` with that code and no `changedUser`) and shows `voice.held`; the page sends and plays nothing and drops its media keys, because a device the new epoch's commit removed still holds the previous epoch's key. The hold ends when the current epoch's key is posted.

Amended 2026-10-13 (controller ruling I2, task 1 review): the `{name}` of `voice.paused` and `voice.blockedChanged` comes from `call.changedUser`, the 32-hex user id of the changed contact that the worker sets for phase `paused` and for an `E_UMK_CHANGED` join refusal (the first by `user_id` when several changed), and is null otherwise. The page looks the name up as for any contact. At join, "exactly one changed contact" means the worker named one; `voice.blockedJoin` is shown when `changedUser` is null.

Amended 2026-10-14 (product walk M9, M10): a join refused with `voice.blockedChanged` carries the `voice.pausedAction` button, which opens Privacy's comparison for that contact (focus moves to it once), on the stage and on a DM's call strip. A block whose join button shows again (the ruling below) says `voice.blockedLeft` mid-call and `voice.blockedRetry` before joining, never "joining is paused".

Amended 2026-10-13 (controller ruling at integration; narrows "no join button until the cause is gone" in the blocked states above): a block that came only from the call group, meaning `call.phase` is blocked, the channel's own `sendBlocks` entry is clear and the block is not a changed identity (`call.changedUser` null, code other than `E_UMK_CHANGED`), keeps its notice and shows the join button again. The call group was left and forgotten, so nothing can show that block cleared; a new join is safe because `call_join_check` refuses a staged join before anything is posted, and RV-2 closes a poisoned idle group. A block of the channel's text group (its `sendBlocks` entry is set) or a changed identity keeps the join hidden, and so does any block while `sendBlocks` has not been published yet. When the text group's block clears, the worker moves the call slice to idle.

## 1280×800

Removed in v0.1.0-web3.8: the `ACTIVE VOICE` heading (`voice.heading.active`, shown in caps by the token style, and the separate `voice` section heading with `voice.heading.channels`) listed the live voice channels and then the full list of voice channels. See flow 13 for one consolidated channel list instead.

```text
┌──┬──────────────────────┬───────────────────────────────────────────────────────────────────────────────┐
│◆ │ACTIVE VOICE          │┌─────────────────────────────────────────────────────────────────────────────┐│
│  │ ◉ harbour       2    ││ Ada is calling                                     [answer] [decline]       ││
│  │   ada  speaking      │└─────────────────────────────────────────────────────────────────────────────┘│
│  │   bo   muted         │ voice harbour                                                  ▸ call code    │
│  │                      │ ┌───────────────────────┐ ┌───────────────────────┐ ┌───────────────────────┐ │
│  │channels              │ │                       │ │                       │ │                       │ │
│  │ # general            │ │          ada          │ │          bo           │ │          you          │ │
│  │ # build              │ │       speaking        │ │         muted         │ │                       │ │
│  │voice                 │ │ volume for ada ──●──  │ │ volume for bo ───●──  │ │                       │ │
│  │ ◉ harbour       2    │ └───────────────────────┘ └───────────────────────┘ └───────────────────────┘ │
│  │ ◉ workshop           │                                                                               │
│  │                      │        [mute] [deafen] [camera] [share screen] [leave]   ← call controls      │
│  │┌────────────────────┐│                                                                               │
│  ││in harbour          ││                                                                               │
│  ││▮▮▮▮▮▮▯▯▯▯▯▯        ││                                                                               │
│  ││[mute] [deafen]     ││                                                                               │
│  ││[leave]             ││                                                                               │
│  │└────────────────────┘│                                                                               │
│  │ web · online         │                                                                               │
└──┴──────────────────────┴───────────────────────────────────────────────────────────────────────────────┘
```

Amended 2026-10-21 (web3.11, flow 14): **AFK means disconnected.** The AFK channel hosts no call: its page lists who is away, with **go AFK**, and the listen-only call described in the next paragraph is gone.

Amended 2026-10-20 (web3.9, flow 14): **the AFK page is gone; the AFK channel is a built-in, listen-only voice channel.** Every server has a voice channel called "AFK" in the one channel list. It is joined like any voice channel (one click on a computer; on a phone, tap it and then join), and every grant in it is listen-only, so every tile and row shows `mic-off` and the stage header reads `afk.stage.muted`. There is no AFK picker in server settings, no "Move to AFK" or "Leave AFK", and no `afkMove` or `afkLeave` command: the server moves a device that was idle in another voice channel, and a person leaves by joining another voice channel. A phone is never moved. See `docs/afk-architecture.md`. In the same wave **the dock lost its microphone meter** (the meter is in Settings → Voice): it shows `layout.dock.connected`, where you are (`layout.dock.where`), then camera, share and leave; with push to talk on, the mute button carries the `layout.ptt.tag` tag. One click on a voice row joins it on a computer, and the speaking person's avatar has a ring.

Amended 2026-10-21 (MP5; founder): **the dock names the channel and shows the link.** Line 1 is the state dot and the channel name (no "voice connected", no server name; `layout.dock.connected` is gone). The server stays in its tooltip and accessible name (`layout.dock.where`). Line 2 is the ping and the rates, `24 ms · ↑ 48 ↓ 96 kb/s` (`voice.dock.ping`, `voice.dock.rate`), and `— ms` before the first sample (`voice.dock.pingNone`). While the call is connecting, reconnecting or paused, line 2 shows that phase's words instead. The ping is the round-trip time of the selected candidate pair, read locally every 2 s; ↑ is all outbound RTP and ↓ all inbound RTP (amended FD1: ↓ is 0 when nothing is received, since a call with no subscriber connection has none, and a dash only when the read failed). The stats turn `--warn` from 150 ms and `--danger` above 300 ms or 5 % loss, and `voice.dock.stats` names the quality (`voice.dock.quality.*`) for a screen reader; they are never announced.

Amended 2026-10-15 (voice icons, plan 2026-10-15; founder): **the voice controls are icon buttons, and the dock holds all five.** The dock and the stage's toolbar show the same five controls in the same order: mute, deafen, camera, share screen and leave. Each is an icon-only button drawn from the vendored Lucide set (monoline, `currentColor`, so forced colours repaint it). Its accessible name is its label, unchanged (`voice.mute`, `voice.deafen`, `voice.cameraOn`, `voice.shareOn`, `voice.leave`), and every toggle keeps `aria-pressed`. The glyph carries the state, never colour alone: muted is `mic-off` (live `mic`), deafened `headphone-off` (`headphones`), a live camera `video` (off `video-off`), sharing `monitor-x` (not sharing `monitor-up`), and leave is always `phone-off` in the danger variant. On top of the glyph, mute and deafen take `--danger` on their glyph and edge while on, camera and share the accent, with a 2 px pressed underline and no fill (review round 1: a filled red mute read like leave). Under forced colours, where the underline is dropped, a pressed control has a 2 px `Highlight` edge. The voice toolbars drop the `[x]` / `[ ]` mark; other toggles keep it. An `aria-disabled` control dims its glyph and dashes its edge, and hover changes nothing on it. In the dock, camera and share follow the stage's rules: `aria-disabled` while paused or held, while joining, and share also without screen capture (no `getDisplayMedia`); mute, deafen and leave stay live while joining, as before. Pressing the dock's camera or share is the stage's toggle, one state for both.

**Glyph rule** (founder's ruling, 2026-10-15, review round 1): the glyph shows what the device is doing now, and a slash means *not sending* or *not hearing*. So a muted mic is `mic-off`, deafened is `headphone-off`, and the camera is `video-off` until it is live. The colour follows a second rule: red means silenced (others do not hear you, or you do not hear them), the accent means you are broadcasting. The one deliberate exception is share, which shows the action, as Meet's "Present now" and "Stop presenting" do: `monitor-up` to start, `monitor-x` while sharing, to stop. Leave's `phone-off` is an action too, kept apart by its place and its danger outline. A new voice icon follows these rules.

**Leave set apart** (review round 1): leave is the last control, after a hairline divider with space on both sides; it keeps its outline-only danger look at rest and fills only on hover. It stays in the same toolbar and the same roving order. Its place and the divider (a border, so forced colours draw it) mark it as well as its colour.

**Tooltip.** A toolbar has one tooltip, shown above the most recently hovered or keyboard-focused control: the label, then the shortcut where one exists, written for the platform (`⌘⇧M` / `⌘⇧D` on macOS and iOS, `Ctrl+Shift+M` / `Ctrl+Shift+D` elsewhere). It is not a `title` (WCAG 1.4.13), and it is never part of a button, so a click on it never presses a control. Hoverable: a mouse or pen hover shows it, and the pointer can move from the control onto it (a transparent bridge covers the 6 px gap); only then does it take the pointer, over its own area. A tip shown by keyboard focus takes no pointer at all, so a click where it shows reaches what is under it. A tap never shows it, and neither does the focus a press gives a control. Dismissable: Escape hides a shown tip wherever focus is (the composer included) and does nothing else; the key is consumed. Persistent: it stays while the pointer or focus is on its control or the pointer is on the tip. A disabled camera or share says why (founder, 2026-10-15): its tip reads the label, an em dash and one reason, `camera — paused`, and the reason is also its description (`aria-describedby`), never part of its name. The reason is `voice.why.joining` while joining, else `voice.why.paused` while paused or held, else, for share without screen capture, `voice.why.noShare`. Only the shortcut describes the control (`aria-describedby`), so the name is not read twice; `aria-keyshortcuts` is unchanged. Each control is at least 40×40 CSS px (44×44 under 48rem of viewport); the only motion is the 150 ms colour change on hover and focus, none under reduced motion.

```text
the dock (240 px sidebar)                 a tooltip, on hover or keyboard focus
┌──────────────────────────────┐          ┌───────────────────┐
│ ● in harbour                 │          │ mute Ctrl+Shift+M │
│ ▮▮▮▮▮▮▯▯▯▯▯▯                 │          └───────────────────┘
│ [mic][hp][cam][scr] │ [⌀]    │          [mic]   ← it sits above its control
└──────────────────────────────┘

mic = mic / mic-off · hp = headphones / headphone-off · cam = video-off / video
scr = monitor-up / monitor-x · │ = the hairline divider · ⌀ = phone-off (danger)
the stage's toolbar shows the same five, centred, with the same divider
```

**One row** (founder's ruling, 2026-10-15, review round 1; replaces the earlier wrap of four plus a full-width leave): the five controls sit in one row, at 40 px on a desktop and 44 px on a phone. In the 240 px desktop sidebar the dock has 221 px inside: four 40 px toggles 2 px apart, the divider's 13 px and leave's 40 px take 219. In the phone drawer (300 px beside the 60 px rail, about 281 px inside) the 44 px controls take 239. The toggles share any spare width; leave keeps its size. The row wraps only as a fallback, when even that does not fit (a larger default font, 200 % zoom); it never scrolls.

Amended 2026-10-14 (product walk I2, I3, M4, M6, M13; controller rulings): a tile with both a camera and a screen shows the screen as the tile and the camera as an inset in its lower right corner. The own tile is a self-view of what this device sends: its camera mirrored, its screen as sent. The own tile says `voice.state.speaking` when the SFU hears this device (as the others' tiles), not from the input meter. The volume range shows its value (`voice.volumeValue`) beside it, marks 100 % and is drawn in Mesh (a hairline track filled with the accent, a square thumb). No tile and no row carries the `voice.tag.web` tag: the page knows the client only of its own device, so the tag would differ between people on the same client; `voice.tag.bot` stays. The sidebar's people say `voice.state.speaking` while the call this device is in hears them.

The disclosure `voice.code` is closed by default. Opened, it shows the call code as 6 groups of 5 digits and `voice.codeHelp` under it; Escape closes it.

**DM call.** A call strip sits under the conversation header: `call.start` when no call is live, `call.join` when one is, and `call.outgoing.ringing` while this device's call rings others. While in the call, the stage sits above the log at 40 % of the main pane's height; the log and composer stay usable below it.

```text
┌──────────────────────────────────────────────────────────────────────────────┐
│ @ ada                                                                        │
│ ringing ada                                                          [call]  │
│ ┌─────────────────────────────────────────────────────────────────────────┐  │
│ │  ada  connecting          you                         ▸ call code       │  │  40 %
│ │        [mute] [deafen] [camera] [share screen] [leave]                  │  │
│ └─────────────────────────────────────────────────────────────────────────┘  │
│ log …                                                                        │
│ composer                                                                     │
└──────────────────────────────────────────────────────────────────────────────┘
```

**Players.** Inline in the attachment card. A video is at most 480×270 CSS px on desktop; audio is the native control bar at the card's width.

```text
│ ┌──────────────────────────────────────────────┐ │
│ │ harbour-walk.webm · 12.4 MB                  │ │
│ │ ┌──────────────────────────────────────────┐ │ │
│ │ │                (480×270)                 │ │ │
│ │ │ ▶ ━━━━━━━━━━━━━━━━━━━━━━ 0:00  vol  full │ │ │
│ │ └──────────────────────────────────────────┘ │ │
│ │ [save]                                       │ │
│ └──────────────────────────────────────────────┘ │
```

## 360×740

The stage is full width. The dock lives in the sidebar sheet, above the user panel. Every voice control is at least 44×44 CSS px. The ring banner is fixed at the top of the viewport while a ring lasts and never covers the stage controls (the content moves down).

```text
┌────────────────────────────────────┐
│ Ada is calling                     │
│ [answer]            [decline]      │
├────────────────────────────────────┤
│☰ voice harbour        ▸ call code  │
│┌──────────────────────────────────┐│
││               ada                ││
││             speaking             ││
││ volume for ada ───────●───       ││
│└──────────────────────────────────┘│
│┌──────────────────────────────────┐│
││               bo                 ││
││              muted               ││
│└──────────────────────────────────┘│
│                                    │
│ [mute] [deafen] [camera] [leave]   │
│ [share screen]                     │
└────────────────────────────────────┘
```

The sidebar sheet (opened with `☰`) holds the dock above the user panel while this device is in voice:

```text
┌────────────────────────────────────┐
│ACTIVE VOICE                        │
│ ◉ harbour                      2   │
│   ada  speaking                    │
│   bo   muted                       │
│channels                            │
│ # general                          │
│voice                               │
│ ◉ harbour                      2   │
│                                    │
│┌──────────────────────────────────┐│
││in harbour                        ││
││▮▮▮▮▮▮▯▯▯▯▯▯                      ││
││[mute]   [deafen]   [leave]       ││
│└──────────────────────────────────┘│
│ web · online                       │
└────────────────────────────────────┘
```

**DM call on the phone.** The stage is full screen with a `messages` toggle that shows the log and composer in its place; the call strip stays under the header.

Amended 2026-10-15 (voice icons, review round 1): the dock's controls are the five icon buttons of § 1280×800, in one row at 44 px, leave after the divider.

```text
┌──────────────────────────────────┐
│ in harbour      ▮▮▮▮▮▮▯▯▯▯▯▯      │
│ [mic] [hp] [cam] [scr]  │  [⌀]   │
└──────────────────────────────────┘
```

Amended 2026-10-14 (product walk I4, I5): at 360 the dock is pinned to the bottom of the sidebar column (sticky), compact (where and the meter on one line, mute, deafen and leave on the next), so it stays on screen while a text channel is read. The stage of a voice channel or a DM call is in normal flow there: its controls sit under the tiles, never fixed or overlaid on a tile.

Amended 2026-10-13 (task-9 review): the toggle is `voice.messages`, a pressed toggle (pressed while the log shows). The input mode follows Settings → Voice during a call: switching to push to talk closes the microphone at once, and the dock shows `settings.voice.ptt` while it is in force. The ring's desktop notification shows only while the tab is hidden, and closes when the tab shows again (the banner covers a visible tab).

**Players on the phone.** A video takes the full card width at 16:9; audio is the native control bar at the card's width. Nothing scrolls horizontally at 360 CSS px.

## Keyboard map

| starting focus | action | destination |
|---|---|---|
| anywhere, while in voice | Control/⌘+Shift+M | toggles mute; focus stays; `voice.announce.muted` / `voice.announce.unmuted` |
| anywhere, while in voice | Control/⌘+Shift+D | toggles deafen; focus stays; `voice.announce.deafened` / `voice.announce.undeafened` |
| anywhere, page focused, push to talk on, focus not in an editable field | hold the push-to-talk key | microphone open while held; `preventDefault`, so nothing is typed; in a text field, `contenteditable` or `select` the key does nothing to the microphone and keeps its usual effect |
| dock or stage toolbar | Left/Right arrows | previous/next control (roving focus); Home/End first/last |
| into a toolbar | Tab | the toolbar's last-focused control, then out on the next Tab |
| a dock or stage control (added 2026-10-15, voice icons) | keyboard focus, or a mouse or pen over it (never a tap) | its toolbar's one tooltip: the label, then the shortcut for this platform where it has one; the most recent hover or focus wins |
| anywhere, a voice tooltip shown (added 2026-10-15, review round 1) | Escape | hides the tooltip and does nothing else (the key is consumed); focus stays where it is and the call is unchanged; the next focus or hover shows it again |
| `voice.code` disclosure | Enter/Space | opens or closes; Escape closes and returns focus to it |
| unsupported dialog | Escape | closes; focus returns to the join button |
| push-to-talk capture prompt (`settings.voice.pttPrompt`) | one non-text key on its own: F13–F24, Pause, ScrollLock or Insert (accepted, but not suggested: it is the screen readers' modifier key) | that key is saved; any other key, and any key held with Control, Alt, ⌘, Shift or AltGr, is refused with `settings.voice.pttRefused` and the capture goes on; Escape cancels; Tab leaves and cancels |
| ring banner | Tab from the main landmark's start | `call.ring.answer`, then `call.ring.decline`, before any other control of the main landmark |
| attachment player | Enter/Space on `media.play`, then the browser's native controls | the native player |

Amended 2026-10-14 (product walk M5; controller ruling): mute and deafen are kept per browser in the device-local setting `voice.self` (`muted` or `deafened`, absent when live), written on each toggle; a call starts in the kept state with the microphone track already off, and says it to the others (`voice_self`) at once. A reload or a closed tab leaves the call. A call started deafened undeafens to muted (wave-3 fix m1: the kept state does not say whether the person had muted first, so the privacy-safe side wins).

There is no single-key shortcut (decision 1, WCAG 2.1.4): mute and deafen need Control/⌘ and Shift. The push-to-talk key has no default and acts only while dilla is the active tab (decision 2). These shortcuts do not interrupt IME composition. The settings rows show `keyboard.mute` and `keyboard.deafen`, each followed by `keyboard.muteAction` / `keyboard.deafenAction`, as `keyboard.searchAll` is followed by its action. The dock's and the stage's mute and deafen buttons carry `aria-keyshortcuts` (`Control+Shift+M Meta+Shift+M`, `Control+Shift+D Meta+Shift+D`).

Amended 2026-10-13 (controller rulings on the task-7 review, M4 and M9):
- **Shortcut spelling (M4).** The mute and deafen shortcuts follow the web-2c convention of the existing Control/⌘+K entry: the same spelling (`Control/⌘+Shift+M`, `Control/⌘+Shift+D`) and the same mac mapping (Control or ⌘ works on every platform). `keyboard.mute` and `keyboard.deafen` below are aligned to it.
- **Push-to-talk key (M9, superseded 2026-10-13 below).** The capture refuses a key that types text or moves focus without Control, Alt or ⌘: letters, digits, space, punctuation (with or without Shift), dead and IME keys, Enter, Backspace, Delete, the arrows, Home, End, PageUp and PageDown. Accepted are non-text keys (F1–F24, Pause, ScrollLock, Insert and the like) and any key held with Control, Alt or ⌘. Push to talk never acts while focus is in an editable field (a text input, `textarea`, `select` or `contenteditable`), so the key is never swallowed while someone types.

Amended 2026-10-13 (controller ruling on the task-8b review, I2; supersedes the M9 ruling above): **the push-to-talk key is one non-text key pressed on its own**, F13 to F24, Pause, ScrollLock or Insert. `voice.pttKey` stores a `KeyboardEvent.code` alone, so a combination could never be matched: a key that types text (AltGr included), moves focus or has a browser function (F1–F12) is refused with or without Control, Alt or ⌘, and any of these keys held with Control, Alt, ⌘, Shift or AltGr is refused too. Push to talk binds only such a key and ignores a press with a modifier held. `settings.voice.pttRefused` is reworded to match.

## Focus rules

| transition | focus destination | polite status |
|---|---|---|
| join | the first stage control | `voice.announce.joined` |
| answer a ring | the first stage control, as for a join | `voice.announce.joined` |
| leave | the voice row (or the DM's `call.start`) the call came from | `voice.announce.left` |
| block notice appears (before join or mid-call) | the notice, once | the notice text |
| pause starts (RV-3) | the banner's `voice.pausedAction` button, once | `voice.paused` |
| pause ends | unchanged | `voice.resumed`, once, in the dock's status region |
| a ring starts or ends | unchanged: a ring never moves focus | `role="alert"` once at start |
| the unsupported dialog closes | the join button | none |
| join starts (added 2026-10-14, WB a11y I1) | the first stage control at once, focusable while `aria-disabled`; the stage is the same element while joining, joined and not joined | none until `voice.announce.joined` |
| the stage or the DM strip swaps under focus: a failed join, a removal, the end of the call or a lost connection (added 2026-10-14, WB a11y I1) | the notice that says why (or its action), else the join (or call) button | the notice |
| a ring of two or more is declined or answered (added 2026-10-14, WB a11y m3) | the next ring's `call.ring.answer`, else the main landmark | none |
| leave from the dock while the call's row or strip is not on screen (added 2026-10-14, WB a11y m5) | the call's own voice row or DM call button when shown, else the sidebar's channel list | `voice.announce.left` |
| a dock control's tooltip opens or closes (added 2026-10-15, voice icons) | unchanged: the tooltip never takes focus, and the roving tab stop stays on the control | none (the shortcut is the control's description) |
| the dock's camera or share turns `aria-disabled` under focus (pause, hold, join) (added 2026-10-15) | unchanged: it stays focusable and in the roving order | the pause's own announcements |

## Announcements

One `role="status"` region in the dock announces join, leave, mute, deafen and the end of a pause; the paused banner itself is `role="status"`. An incoming ring is announced once through `role="alert"` and never moves focus. Speaking is shown as an outline plus the word `voice.state.speaking`; the 2 s pulse runs only without `prefers-reduced-motion: reduce`, and the 12-bar meter redraws every 120 ms in every mode. Muted, deafened, camera and screen are a glyph plus a word.

## Sounds

Every sound is synthesised at runtime (no sound file ships), under 300 ms, and obeys Settings → Notifications → sounds.

| sound | plays when | never plays when |
|---|---|---|
| join tick | another user's device joins the call this device is in | the person's own join; deafened; `sound.voice` off; no user activation yet on this page |
| leave tick | another user's device leaves the call this device is in | the person's own leave; deafened; `sound.voice` off; no user activation yet |
| ring | an incoming ring that is neither busy nor quiet: a 2 × 120 ms ping every 2 s for at most 30 s, stopping when the ring ends | busy (already in a call); quiet (the DM is muted, R1); `sound.ring` off; no user activation yet |

There is no sound for one's own join, leave, mute or deafen (R2), and no ringback tone for the caller (R3). In Settings → Notifications, a `settings.notify.sounds` group holds two rows, `settings.sound.voice` and `settings.sound.ring`, each an on/off switch, both on by default (R4).

## Copy

Every key below is one string of `packages/web/src/strings/en.ts`, byte for byte (task 9 checks it).

*Sidebar, rows and dock:*
- `voice.heading.active='active voice'`, `voice.heading.channels='voice'` (unused since web3.8)
- `voice.open='voice {name}'`, `voice.count='{n} in voice'`, `voice.join='join voice'`
- `voice.row.joined='connected'` (added to the accessible name of the voice row this device is in: "voice harbour, connected")
- `call.start='call'`, `call.join='join call'`
- `voice.dock.region='voice controls'`, `voice.dock.connecting='connecting'`, `voice.dock.reconnecting='reconnecting'`
- `voice.dock.ping='{ms} ms'`, `voice.dock.pingSub='<1 ms'` (a ping that rounds to 0 ms: a LAN, not a missing reading; added 2026-10-21, FD1), `voice.dock.pingNone='— ms'`, `voice.dock.rate='↑ {up} ↓ {down} kb/s'` (Signal §5.4 spaces the arrows, 2026-10-27; the dock's second line while connected: "24 ms · ↑ 48 ↓ 96 kb/s"; ↑ is all outbound RTP of this device, ↓ all inbound, integer kb/s over the 2 s sample; `— ms` before the first sample; added 2026-10-21, MP5)
- `voice.dock.stats='Ping {ms} milliseconds, {quality}. Sending {up}, receiving {down} kilobits per second.'` (the stats' accessible text, never announced), `voice.dock.quality.good='good'`, `voice.dock.quality.poor='poor'`, `voice.dock.quality.bad='bad'` (good under 150 ms, poor 150 to 300 ms, bad above 300 ms or loss above 5 %; poor takes `--warn`, bad `--danger`)
- `voice.dock.ptt='hold {key} to talk'`, `voice.dock.pttNoKey='push to talk: no key set'`, `voice.dock.pttSetKey='set a key'` (the dock's line while push to talk is in force, on its own line under the meter; `{key}` is the stored key as Settings shows it; `set a key` opens Settings → voice; added 2026-10-14, product walk M7, replacing `settings.voice.ptt` there)
- `voice.audioBlocked='Your browser paused call audio.'`, `voice.audioStart='turn on audio'` (the dock's line and its button while the browser holds call audio back, its autoplay rule; one press starts it; the line goes once audio plays; added 2026-10-14, web3.3)
- `call.where='the call with {name}'` (`{where}` of a DM or group-DM call, with the DM's name; a voice channel's `{where}` is its name; added 2026-10-14, product walk M8)
- `voice.mute='mute'`, `voice.deafen='deafen'`, `voice.leave='leave'`
- `voice.cameraOn='camera'`, `voice.shareOn='share screen'`
- `voice.why.paused='paused'`, `voice.why.joining='connecting'`, `voice.why.noShare='not available in this browser'` (added 2026-10-15, founder: why an inert camera or share control is inert, one reason by priority: joining, then paused or held, then, for share only, no screen capture; its tooltip reads `{label} — {reason}`, an em dash with spaces, and the reason is its description through `aria-describedby`, never its name)
- `voice.controls.region='call controls'` (the stage's toolbar in a DM or group-DM call), `voice.controls.voice='voice channel controls'` (the stage's toolbar in a voice channel; added 2026-10-14, WB a11y m6: *call* names a DM call only, and the dock's toolbar is `voice.dock.region`)

Amended 2026-10-14 (controller ruling on WB a11y I4): mute, deafen, camera and share screen are toggles with one visible label each (`voice.mute`, `voice.deafen`, `voice.cameraOn`, `voice.shareOn`), `aria-pressed` and the aria-hidden `[x]` mark; the label never swaps (the WAI-ARIA toggle pattern; WCAG 2.5.3 and 4.1.2). `voice.unmute`, `voice.undeafen`, `voice.cameraOff` and `voice.shareOff` are gone.

*Participant states:*
- `voice.state.speaking='speaking'`, `voice.state.muted='muted'`, `voice.state.deafened='deafened'`
- `voice.bot.playbackOnly='playback only'` (shown with the bot tag on a managed bot's sub-row in the voice list; the server grants audio publication without media subscription)
- `voice.state.notHeard='not heard yet'`, `voice.state.unverified='unverified stream'`, `voice.state.connecting='connecting'`
- `voice.state.video='camera on'`, `voice.state.screen='sharing screen'`
- `voice.state.noSound='no sound'` (the glyph-only state of a remote screen tile whose share carries no sound; added 2026-10-21, #37)
- `voice.tag.web='web'`, `voice.tag.bot='bot'` (the participant tags, as the message rows show them; added 2026-10-13, task-9 review m7)

*Announcements:*
- `voice.announce.joined='You joined {where}.'`, `voice.announce.left='You left {where}.'`
- `voice.announce.rejoining='Rejoining {where}. This can take up to 30 seconds.'` (the rejoin's wait for this device's earlier Remove; added 2026-10-14, task-10 fix round, f10app m2)
- `voice.announce.muted='Microphone muted.'`, `voice.announce.unmuted='Microphone on.'`
- `voice.announce.deafened='Sound off.'`, `voice.announce.undeafened='Sound on.'`

*Stage and call code:*
- `voice.messages='messages'` (the phone DM call's toggle between the stage and the log with the composer; added 2026-10-13, task-9 review I7)
- `voice.code='call code'`
- `voice.codeHelp='The code changes whenever someone joins or leaves. Read it aloud: if every screen shows the same code, everyone is hearing the same set of devices.'`
- `voice.empty='Nobody is in voice yet.'`, `voice.tiles='people in voice'`, `voice.volume='volume for {name}'`
- `voice.volumeValue='{n} %'` (the volume range's spoken value, `aria-valuetext`; added 2026-10-13, task-7 review M6)
- `voice.streamVolume='stream volume for {name}'`, `voice.streamMute='mute stream'` (the range and the pressed-state toggle of a remote screen tile with sound; added 2026-10-21, #37)

*Refusals and notices:*
- `voice.blocked='A device in this voice failed the identity check, so you left. Joining stays paused until it is removed.'`
- `voice.blockedJoin='A device in this voice failed the identity check. Joining is paused until it is removed.'`
- `voice.blockedChanged='{name}’s identity changed. Compare safety numbers in Privacy before joining.'`
- `voice.blockedLeft='A device in this voice failed the identity check, so you left. You can try to join again.'`, `voice.blockedRetry='A device in this voice failed the identity check. You can try to join again.'` (a block that offers the join again, mid-call and before joining, in place of `voice.blocked` and `voice.blockedJoin`, which say joining is paused; added 2026-10-14, product walk M10)
- `voice.paused='{name}’s identity changed, so this call is paused for you: nothing you say is sent and nothing is played. Compare safety numbers in Privacy to continue, or wait until {name} leaves.'` (RV-3; the banner on the stage and the dock, `role="status"`, focus moves to its `compare safety numbers` button once)
- `voice.pausedAction='compare safety numbers'`
- `voice.held='A device in this call could not be checked yet, so this call is paused for you: nothing you say is sent and nothing is played. It continues once the check is done.'` (the hold after a refused mid-call rekey: the stage and the dock banner, `role="status"`, no action; added 2026-10-14, WB security I1)
- `voice.resumed='The call continues.'` (announced once in the dock's status region when the pause ends)
- `voice.dock.paused='paused in {where}'`
- `voice.removed='You were removed from voice.'`
- `voice.removedNotMember='You were removed from this server, so you left its voice.'` (a removal after which the channel is gone for this device; added 2026-10-14, product walk I8)
- `voice.disconnected='The connection to voice was lost. Join again to continue.'` (the room's connection ended on its own and LiveKit's reconnect gave up; added 2026-10-14, WB client I3)
- `voice.leftWhileAway='You left the call while away.'` (a lost connection whose device was gone longer than its server's AFK timeout: the call is not rejoined by itself; the stage keeps its join button; added 2026-10-21, late rejoin)
- `voice.ended='Voice ended for everyone.'` (the room was closed under this device: the call ended for everyone; added 2026-10-14, WB client I3)
- `voice.full='This voice is full.'`
- `voice.sharersFull='Too many people are sharing. Try again when someone stops.'`
- (Signal task 8, fix round 1, Jonas 2026-10-28: the sharer's sound-source select and its notice action were removed; a silent share keeps the default source. Its seven `voice.shareAudio.*` keys are gone.)
- `voice.shareAudio.none.unsupported='Your screen is shared without sound. This browser can’t share sound; Chrome or Edge can share a browser tab with its sound.'` (the sharer's notice once the share started without sound, in Firefox-based browsers and Safari; added 2026-10-21, #37)
- `voice.shareAudio.none.tabOnly='Your screen is shared without sound. Here only a browser tab can carry sound: share a tab and keep "Also share tab audio" ticked.'` (the same notice in Chromium on Linux)
- `voice.shareAudio.none.tabOrSystem='Your screen is shared without sound. To share sound, share a browser tab, or tick "Also share system audio" when you pick a screen.'` (the same notice in Chromium on Windows, ChromeOS and macOS)
- `voice.failed='Could not join voice. Try again.'`
- `voice.otherTab='Voice is open in another tab of this browser.'`
- `voice.micDenied='The microphone is blocked. You can listen; allow the microphone in the browser to talk.'`
- `voice.micMissing='No microphone found. You can listen.'`
- `voice.micFailed='The microphone could not be connected. You can listen.'` (added 2026-10-13, task-8b review m4)
- `voice.unsupported.title='Voice does not work in this browser'`
- `voice.unsupported.body='This browser lacks a feature dilla needs for voice. Use a current Chromium-based browser or Firefox.'`
- `voice.unsupported.close='Close'`

*Ringing:*
- `call.ring.region='incoming calls'`, `call.ring.incoming='{name} is calling'`
- `call.ring.busy='{name} is calling. Answering leaves {where}.'`
- `call.ring.answer='answer'`, `call.ring.decline='decline'`
- `call.ring.notifyTitle='{name} is calling'`, `call.ring.notifyBody='Open dilla to answer.'`
- `call.outgoing.ringing='ringing {names}'`, `call.outgoing.declined='{name} declined.'`, `call.outgoing.noAnswer='No answer.'`
- `call.outgoing.notRung='{names} not rung: called too recently.'` (the participants the instance did not ring, its pair bound of 15 s and 20 an hour; added 2026-10-14, product walk I6)

*AFK:*
- `voice.afk.count` ('{n} in AFK') counts the away roster on the AFK row since v0.1.0-web3.11 (flow 14). `voice.afk.title`, `voice.afk.loading`, `voice.afk.silent`, `voice.afk.move` and `voice.afk.leave` were unused since v0.1.0-web3.9 and are removed in v0.1.0-web3.11.

*Media players:*
- `media.play='play {name}'`, `media.loading='loading'`
- `media.error='This browser cannot play this file.'`, `media.save='save'`

*Settings:*
- `settings.nav.voice='Voice'`, `settings.voice.section='Voice'`
- `settings.voice.input='Input device'`, `settings.voice.output='Output device'`, `settings.voice.systemDefault='System default'`
- `settings.voice.outputUnsupported='This browser plays voice on the system default device.'` (the output picker shows only where `AudioContext.prototype.setSinkId` exists, Chromium; Firefox and Safari show this line and play on the system default; noted 2026-10-14, web3.3)
- `settings.voice.mode='Input mode'`, `settings.voice.vad='Voice activity'`, `settings.voice.ptt='Push to talk'`
- `settings.voice.pttKey='Shortcut'`, `settings.voice.pttNone='No key set'`, `settings.voice.pttSet='Change'` (Ruling AX, Signal task 10b fix round 1: the prototype's copy; while it listens the key box says `signalSettings.voice.pttListening='Press a key…'` and the button `signalSettings.voice.pttCancel='Cancel'`, which cancels)
- `signalSettings.voice.testEchoOff='Echo cancellation is off: use headphones, or your speakers will feed back into the mic.'` (under the Mic test description while echo cancellation is off; Let's check never plays above 100 %)
- `signalSettings.voice.stageUnsupported='This browser cannot apply the input volume or sensitivity: your microphone is sent as it is.'`, `signalSettings.voice.stageFailedOpen='Input volume and sensitivity could not start: your microphone is sent as it is.'`, `signalSettings.voice.stageFailedClosed='Sensitivity could not start, so with a manual threshold nothing is sent. Turn on automatic sensitivity or use push to talk to be heard.'` (Settings → Voice during a call whose input stage is not applied; fix round 1, M1)
- `settings.voice.pttPrompt='Press the key to use. Escape cancels. It works only while dilla is the active tab.'`
- `settings.voice.pttRefused='That key types text, moves the focus or needs another key. Use one key on its own, such as Pause, Scroll Lock or F13 to F24.'` (added 2026-10-13, task-7 review M9; reworded 2026-10-13, task-8b review I2; 2026-10-14, WB a11y m2: Insert is no longer suggested, since NVDA and JAWS use it as their modifier key; it is still accepted)
- `settings.voice.noise='Noise suppression'`, `settings.voice.level='Input level'`
- Signal task 10b (README §5.9): input and output volume (0–200 %), the Mic test's Let's check (a ten-second loopback through the call's own chain, only you hear it), the sensitivity meter and its threshold (automatic by default; the voice-activity gate in the call), push to talk's release delay, echo cancellation and automatic gain. `settings.voice.level` and the record-and-play test's strings are no longer shown.
- `signalSettings.voice.volume='Volume'`, `signalSettings.voice.inputVolume='Input volume'`, `signalSettings.voice.outputVolume='Output volume'`
- `signalSettings.voice.testStart='Let’s check'`, `signalSettings.voice.testStop='Stop testing'`, `signalSettings.voice.testHint='Talk normally. The bar is your level; the white line is where dilla starts sending your voice. Your voice plays back so you hear what others hear, so wear headphones.'`
- `signalSettings.voice.heard='You’re being heard'`, `signalSettings.voice.tooQuiet='Too quiet to send'`, `signalSettings.voice.notTesting='Not testing'`
- `signalSettings.voice.levelValue='level {value}'`, `signalSettings.voice.thresholdValue='threshold {value}'`, `signalSettings.voice.noLevel='—'`
- `signalSettings.voice.threshold='Sensitivity threshold'`, `signalSettings.voice.auto='Set sensitivity automatically'`, `signalSettings.voice.autoHintOn='dilla picks the threshold from your room noise. Turn off to drag the line yourself.'`
- `signalSettings.voice.autoHintOff='Drag the white line on the meter to set the threshold.'`, `signalSettings.voice.release='Release delay'`, `signalSettings.voice.releaseHint='Keeps sending this long after you let go, so word endings aren’t cut.'`
- `signalSettings.voice.echo='Echo cancellation'`, `signalSettings.voice.echoHint='Stops your speakers from feeding back into the mic'`, `signalSettings.voice.agc='Automatic gain control'`
- `signalSettings.voice.agcHint='Evens out loud and quiet speech'`
- `settings.notify.sounds='Sounds'`, `settings.sound.voice='Join and leave sounds'`, `settings.sound.ring='Ring for calls'`
- `voice.diag.tracks='hearing {attached} of {expected}'`, `voice.outputFailed='The output device did not change. Call sound plays where it did before.'`

*Keyboard:*
- `keyboard.mute='Control/⌘+Shift+M'`, `keyboard.deafen='Control/⌘+Shift+D'` (aligned 2026-10-13 to `keyboard.searchAll='Control/⌘+K'`, task-7 review M4)
- `keyboard.muteAction='Mute or unmute'`, `keyboard.deafenAction='Deafen or undeafen'`
