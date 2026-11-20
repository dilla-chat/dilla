# Linux parity references

`<area>/<state>[-b]-2x.png` is the web client's frame of the screen fixture `../../fixtures/<area>/<state>[-b].json`, shot
at device scale 2 (so 1280x800 is 2560x1600). `<area>/<state>[-b].shot.json` beside it is the sidecar: the SHA-256 of the
fixture file the shot was made from (a reference shot from another version of the fixture is refused as stale), the clock
shift the shot ran under and the ids the real dillad handed out (the web client sorts equal positions by id, so the GPUI
side draws the same ids). A reference without a sidecar is drawn with the fixture's own times and derived ids.

| Reference | What |
| --- | --- |
| `shell/channel-view` | the channel view, 1280x800, one server |
| `shell/channel-view-b` | 1440x900, a second server selected, a category with a text and a voice channel, a long channel name (cut in the list, whole in the header), eleven members, a message that wraps over three lines |
| `messages/log-earlier`, `messages/log-earlier-b` | 101 messages in `#general`: the first page of 100, scrolled to the top (Ctrl+Home), the ghost "load earlier" button centred above the first row instead of the welcome, and "viewing older messages" at the foot (`dilla-desktop::tests/messages::parity_messages_log_earlier{,_b}`) |
| `messages/compact` | Task 19b: the `#general` log at the compact density: the fixture's `"density": "compact"` is put on the web root as `data-density` (shoot.mjs) and on the GPUI window (`Density::of`); the head lines, the divider and the welcome take its 1.4 line height, a grouped follow-up keeps its 1px pads (`dilla-desktop::tests/messages::parity_messages_compact`) |
| `user-settings/appearance-compact` | Task 19b: Appearance at the compact density; the setting stays regular (the shooter sets only `data-density`), so Regular is the chosen option on both sides (`dilla-desktop::tests/user_settings::parity_user_settings_appearance_compact`; its own chrome limit 0.26 % for a measured 0.2562 %: Chromium places the Segmented's inline-flex group from its line's rounded baseline, 213.5 + 19.95 rounding to 233 at compact, while GPUI's layout has already rounded 213.5 to 214. Accepted as a browser-rendering artefact, Jonas 2026-11-20, TOLERANCES.md "Linux per-screen") |
| `threads/reply-compact` | Task 19b fix round 1: `threads/reply-chip`'s replies at the compact density: the reply line above a row takes the root line height, so each replying row is as tall as the web's (`dilla-desktop::tests/threads::parity_threads_reply_compact`) |
| `threads/pane-compact` | Task 19b fix round 1: the open thread pane at the compact density: its names, times and rule take the root line height, the reply bodies keep ThreadPane.css's 1.5 (`dilla-desktop::tests/threads::parity_threads_pane_compact`) |
| `dms/home`, `dms/home-b` | the DM list with no conversation open (`/dm`): the main column is empty, as Shell.tsx leaves it; two DMs, and for `-b` a group DM and a person whose long name is cut. The list is in the instance's order (channel id) and the group's sub line ends in "you". The web frame of `-b` carries one live-unread dot on "Sol" (the web's unread for a DM it does not show is nondeterministic, as in `native/unread-badge-b`); GPUI draws none (`dilla-desktop::tests/dms::parity_dms_home{,_b}`) |
| `dms/conversation`, `dms/conversation-b` | a DM open over three messages, authors named from every server's members; `-b` is the group DM with a name that is cut in the list and whole in the header (`dilla-desktop::tests/dms::parity_dms_conversation{,_b}`) |
| `voice/stage-group`, `voice/stage-group-b` | Task 23: a group call in midgard/lounge the viewer joined: Mira, Björn (muted), Lin (deafened), Sam (sharing); `-b` is 1440x900 with eight people. The others are `voice_state` frames with no media peer, so (as VoiceView.tsx `callTiles` builds the stage from `call.state.tiles`) the web stage holds the own tile alone and the sidebar lists everybody with the muted, deafened and LIVE marks; GPUI builds the stage as `callTiles` does from the media session's room (`voice.sfu_remotes`, `verified_peers`, `screen_streams`) and the call group's roster (a tile for every SFU remote, `connecting` for a roster member not in the room, a screen tile only with a screen stream; `voice::tiles`, `voice::remote_tiles`; the seeded people are in neither, so the web shows one tile; Jonas 2026-11-20 "match the web"), shares first, then the room's order. The sidebar's people start `--list-gap` under the row, as `.d-voice-row`'s bottom margin puts them (`dilla-desktop::tests/voice::parity_voice_stage_group{,_b}`, `the_people_list_starts_a_list_gap_under_the_row`) |
| `commands/modlog-dialog` | Task 24: `/modlog` typed and run (Enter picks the command, Enter runs it): the moderation history in a dialog over `#general`, its row "Moderation history opened." under the log. The web draws its `ModerationLog` heading under the dialog title too; the buttons sit side by side (`dilla-desktop::tests/bot::parity_commands_modlog_dialog`) |

Both are compared by `parity/tests/screen.rs` (`channel_view_two_fixtures`) under `../../tolerance.toml`, with no
per-fixture setting. The two shell sidecars carry `"hand_migrated": true`: they were written by hand from the data of the shots of 2026-11-12 (Playwright 1.64.1
Chromium 156.0.8078.4, the web client of `feat/gpui-desktop` with Tasks 9b and 11 merged), which predate the screen
fixtures; shoot them again with `shoot.mjs` to make them pairs of the shooter.

## GPUI-only references (`native/`)

A surface the web client never had has no web frame to compare with. Its reference is the app's own frame, made once
by the test that checks it and looked at by eye against the Signal design (the component it is built from) before it is
committed; afterwards the test compares every run with it under `../../tolerance.toml`, as for a web reference.

A `native/` reference is made with `assert_native_screen(fixture, golden, prepare)` (`parity/src/screen.rs`): it launches
a GPUI-only fixture (a fixture JSON with no web sidecar), runs `prepare` for the steps that have no web counterpart,
and compares the frame with `native/<golden>-2x.png`; `DILLA_PARITY_BLESS=1` writes that file instead, and it is blessed
by eye, never by the tolerance. There is no fixture of its own for the helper: the harness test
`native_screen_blesses_then_compares` runs it on `shell/channel-view` against a temporary references root.

| Reference | What | Test |
| --- | --- | --- |
| `native/quit-active-call` | "Quit Dilla?" over the channel view (`quit-dialog active-call`) | `dilla-desktop::tests/quit::parity_quit_dialog_active_call` |
| `native/quit-unsent-draft` | the same question for unsent text (`quit-dialog unsent-draft`) | `dilla-desktop::tests/quit::parity_quit_dialog_unsent_draft` |
| `native/quit-not-responding` | "Dilla is not responding" (`quit-dialog not-responding`) | `dilla-desktop::tests/quit::parity_quit_dialog_not_responding` |
| `native/unread-badge-b` | the northern server with "1 new" on the three channels other than the open one (`notify/unread-badge-b`). Native because the web's live-unread count for non-open channels is nondeterministic (one random pill per shoot); GPUI marks all three; Jonas 2026-11-19 | `dilla-desktop::tests/notify::parity_notify_unread_badge_b` |
| `native/row-menu` | a channel row's menu as the owner sees it (`shell/row-menu`): the web's items, then Mark as read and copy link (desktop only, Jonas 2026-11-19) | `dilla-desktop::tests/shell::parity_shell_row_menu` |
| `native/thread-earlier` | the thread pane of a 201-reply thread: the newest 200 and "load earlier" over the first (`threads/thread-earlier`). Native because the web never draws it (Task 17) | `dilla-desktop::tests/threads::parity_thread_earlier` |
| `native/server-settings-self-roles` | Server settings, Self-service roles as the owner sees it (`server-settings/self-roles`): the approve box drawn with the Signal `CheckList` and the channel `Select`, not the browser's fieldset, check boxes and `<select>` (Decision D19, Jonas 2026-11-19) | `dilla-desktop::tests/members::parity_server_settings_self_roles` |
| `native/server-settings-self-roles-b` | the same page in the northern world, thirteen roles (`server-settings/self-roles-b`) | `dilla-desktop::tests/members::parity_server_settings_self_roles_b` |
| `native/announcement-editor` | `/announcement create` opens the editor (`commands/announcement-editor`): the web's fields, order and labels (BotCommandDialogs.tsx) drawn with the Signal `Dialog`, `Select` and `TextField` instead of the browser's `<select>`, date and number inputs (Decision D2) | `dilla-desktop::tests/bot::parity_announcement_editor` |
| `native/server-settings-welcome-loaded` | Server settings, Welcome after Load (`server-settings/welcome-loaded`: a ready Bot with the welcome capability, `general` marked): the channel select, the template field with the default, the tokens hint and Reload, the footer's Save and Reset. Native because the web's frame of it needs a Bot whose clock agrees with the shooter's frozen one (ruling R34, Jonas "design, not pixels"); checked by eye | `dilla-desktop::tests/server_settings::parity_server_settings_welcome_loaded` |
| `native/music-card-live` | the Bot's "started music" message in `#general` with its live now-playing card (`bot-admin/music-card-live`: Glenn pinned and in the voice channel, the announced track playing, a queue of two, Ada may control). Native because the web's frame needs a Bot playing real audio in a real call; Task 35, checked by eye | `dilla-desktop::tests/bot_admin::parity_music_card_live` |
| `native/music-card-ended` | the same message when another track plays: "Glenn played this in harbour" over "Finished", no track, no queue (`bot-admin/music-card-ended`) | `dilla-desktop::tests/bot_admin::parity_music_card_ended` |
| `native/voice-speaking` | `voice/stage-group` with every other person's media there (`voice-sim media <person>`) and Mira heard speaking (`voice-sim speaking mira`): the `--ok` tile edge and level bars, and the avatar ring in the sidebar. Native because speaking needs media a web reference cannot hold still; Jonas 2026-11-20 | `dilla-desktop::tests/voice::native_voice_speaking` |
| `native/voice-camera-tile` | the same call with Mira's camera on, a static test pattern (`voice-sim camera mira test-pattern`) filling her tile; Sam's screen flag draws no screen tile, as there is no stream | `dilla-desktop::tests/voice::native_voice_camera_tile` |
| `native/voice-screen-tile` | the same call with Sam's shared screen showing a static test pattern (`voice-sim screen sam test-pattern`), the screen tile first | `dilla-desktop::tests/voice::native_voice_screen_tile` |
| `native/voice-stage-wrap` | `voice/stage-group-b` (1440x900, eight people) with every other person's media there and Eva heard speaking: the grid wraps to three rows, the last row centred, and Sam's screen flag draws no screen tile without a stream. No web reference can show it (the parity world holds no media) | `dilla-desktop::tests/voice::native_voice_stage_wrap` |
| `native/voice-stage-paused` | `voice/stage-group` with every other person's media there, Mira's camera and Sam's screen showing and Mira heard speaking, then the call paused by Mira's changed identity (`voice-sim paused mira`, the client's `RefusedEpoch::Pause`): CallStage's pause notice (the pause glyph and the 3px `--warn` edge, `voice.paused` naming Mira, "compare safety numbers") under the header, Sam's screen tile gone, no camera frame and no speaking ring (CallStage.tsx `hideMedia`); every person tile stays. Matches the CallStage story `Paused` | `dilla-desktop::tests/voice::native_voice_stage_paused`, `screens::voice::tests::a_paused_call_hides_media_and_speaking` |
| `native/voice-tile-states` | `voice/stage-group` with each of session.ts `tiles()`' states drawn as CallTile's `voiceStateWords` draws them: Mira "? not heard yet" (in the room and the roster, no frame verified), Sam "! unverified stream" in `--warn` (in the room, not in the roster), Björn "connecting" with the dashed pending dot (in the roster, not in the room), Lin not heard but muted/deafened (`silentByChoice`: the glyph alone); Ada ok. `voice-sim not-heard\|connecting\|unverified <person>`; the stories CallTile `Connecting`, `NotHeard` and `Unverified` show the same words | `dilla-desktop::tests/voice::native_voice_tile_states`, `a_tile_that_is_not_heard_says_why` |

Remake them after a deliberate change with `DILLA_PARITY_BLESS=1 cargo test -p dilla-desktop --test quit parity_`, and
look at them again before committing.

## How they are made

`node clients/desktop/parity/shoot.mjs clients/desktop/parity/fixtures/<area>/<state>.json` (from the repository root,
Linux):

1. `cargo run -p dilla-desktop --example parity_world -- <fixture> <out>` starts dilla-testhost serving `packages/web/dist-desktop-strings`
   (the web client with the desktop app's wording, `packages/web/src/strings/desktopOverrides.ts` put over `en`, so a
   reference says "this device" where the web says "this browser", as the GPUI frame does),
   signs the world's people up the way the app does (each with an authenticator app, a password and a sealed backup), makes
   its servers and channels and stops for the browser.
2. A fresh Chromium signs the viewer in on a new device the way a person does (password, authenticator code, recovery key,
   "Open dilla"), visits every channel that will hold history (joining its group), with the browser's clock frozen at the
   fixture's `clock` (lifted by the shift, below).
3. The world posts the history in time order. The host's clock is frozen and moves only forward from the moment the host
   starts, so the world lifts the whole fixture by a shift (every message time and the clock) and records it in the
   sidecar. It stops every client, so only the browser is online.
4. The browser opens the fixture's route, replays the steps' `web` selectors (`-`: no web counterpart; `if:` a selector
   that clicks only when the element is there), moves the pointer to the corner (unless a step hovers), blurs the focus,
   awaits `document.fonts` and takes one viewport screenshot.

Chromium runs with `--use-fake-device-for-media-stream --use-fake-ui-for-media-stream` (a granted fake microphone, so the
Voice page's mic test runs; T6c) and, on Linux, `--font-render-hinting=none --enable-font-subpixel-positioning` (Jonas's 2026-11-12 decision for the Linux
reference), `deviceScaleFactor: 2`, `reducedMotion: 'reduce'`, `timezoneId: 'UTC'`, `locale: 'en-US'`.

## Prerequisites (once per worktree)

Verified end to end on 2026-11-12 (CachyOS, clang 23.1.4, a machine shared with other builds; wall-clock times). From the
worktree root, every step in order. `CC=clang CXX=clang++` is needed on Linux (the sqlite3mc and WASI builds want clang >= 21):

```
export CC=clang CXX=clang++
npm ci                                                                  #   3 s
wasm-pack build core/dilla-core-wasm --target web --profile wasm-release --mode no-install \
  --out-dir ../../packages/core-wasm/pkg                                # 1 m 48 s: the web client imports @dilla/core-wasm
npm run build:desktop-strings -w @dilla/web                             #  15 s (fails with "Failed to resolve entry for package
                                                                        #  @dilla/core-wasm" if the wasm-pack step is missing)
go build -o target/dilla-testhost ./cmd/dilla-testhost                  #   9 s
cargo build -p dilla-core-wasi --target wasm32-wasip1 --release         # 2 m 01 s
```

`npm run build:desktop-strings -w @dilla/web` does NOT build `packages/core-wasm/pkg`; run `wasm-pack` yourself as above. Playwright's
Chromium (revision 1248) must be in `~/.cache/ms-playwright` (`npx playwright install chromium` otherwise).

## Shooting

```
node clients/desktop/parity/shoot.mjs clients/desktop/parity/fixtures/shell/channel-view.json --out target/shots/run1
```

The first run in a worktree also compiles `parity_world` (gpui, dilla-client, sqlite3mc): about 15 min on a shared machine;
every later shot takes about 2 min (`shell/channel-view-b`: 1 m 59 s). With `--out <dir>` the PNG and the sidecar go to
`<dir>/shots/<area>/<state>-2x.png` and `<state>.shot.json`, and the world's files (`state.json` with throwaway secrets,
`world.json`, `<state>.layout.json` with every box and text run of the frame) to `<dir>`. Without `--out` the shot REPLACES the
committed reference and its sidecar in this directory: do that only to refresh a reference on purpose, then commit the PNG and
the sidecar together (a PNG and its sidecar are only valid as a pair: the sidecar carries the ids and clock shift the world
handed out). To try a fresh shot against the GPUI frame without touching the committed pair, copy
`fixtures/<area>/` and your shot (PNG and sidecar) into a scratch `parity/{fixtures,references/linux}/<area>/` tree and call
`dilla_parity::screen::assert_screen` on the scratch fixture path (it derives the references from the fixture's location).

Re-shooting the same fixture is not byte-identical to the committed shot: the clock shift differs (the message times in the
header rows are the only pixels that change in `shell/channel-view`: 0.13 % of the pixels, all inside x 626..687, one band per
message header) and the host hands out new ids, which reorder anything sorted by id (`shell/channel-view-b`: 1.3 % differ).

`node shoot.mjs <fixture> --check` prints the fixture line (`fixture <name> <sha256> <kind:local=hex;...>`) without a
browser; the GPUI driver's `fixture` command prints the same line (`shooters_agree_on_fixture`). The parity world seeds
plain text history only; each area adds the seeding of its own facets (reactions, replies, polls, ...) in
`dilla-desktop/examples/parity_world.rs` (`unseedable` and `seed_facets`) and the mapping into its state slice in
`dilla-desktop/src/fixture.rs`.
