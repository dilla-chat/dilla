# GPUI desktop: file ownership (screens wave 1)

The ownership map of `docs/superpowers/plans/2026-11-13-gpui-desktop-screens-wave1.md`, made by Task 1. A task edits only
the files its row gives it, plus new files under its own directories. The controller and the reviewers check every diff
against this file. A task that needs a change outside its row stops and writes the change into its report; the
controller makes it at merge.

Every Wave 1 area already has its client module, its state slice, its screen module (with `keys.rs` and its slot
hooks), its ui component directory, its test files and its parity fixture and reference directories, all stubs. Every
module line, `AppState` field, slot and key registration is already in place, so no Wave 1 task adds a line to a shared
file.

Paths are relative to `clients/desktop/`:

| Short | Path |
|---|---|
| `C/` | `dilla-client/src/` |
| `CT/` | `dilla-client/tests/` |
| `D/` | `dilla-desktop/src/` |
| `DT/` | `dilla-desktop/tests/` |
| `U/` | `dilla-desktop-ui/src/components/` |
| `PF/` | `parity/fixtures/` |
| `PR/` | `parity/references/linux/` |

A trailing `/` means the whole directory, its existing stubs and any new file in it.

## Wave 0b (Tasks 2-7)

| Task | Owns (may create or edit) |
|---|---|
| 2 client carry-overs | `C/client.rs`, `C/client/tests.rs`, `C/sync/mod.rs`, `C/sync/removals.rs`, `C/store.rs`, `C/gateway/mod.rs`, `C/account/` (stubs `mod.rs`, `keypackages.rs`, `backups.rs`); new `CT/{removals,keypackages,backups,lifecycle}.rs`. The removal tombstone may touch `C/state/messages.rs` (Wave 0: before Task 9 owns it) |
| 3 E_ codes, calls | `C/seams.rs`, `C/http/`, `C/sync/requests.rs`, `C/calls/`, `CT/http.rs`, `CT/calls.rs`, `CT/support/dump-routes.mjs`; `dilla-media/src/transform.rs` (the `decode_media_epoch` zeroize only), `dilla-media/tests/glue.rs`, new `dilla-media/tests/e2e_call.rs`, `dilla-media/browser/`. The `HttpError::Status` change edits its matchers wherever they are in dilla-client: only those lines (it merges after Task 2) |
| 4 device audio | `dilla-media/` except the Task 3 lines above: new `src/{capture,playout,apm}.rs`, `src/{devices,noise,room,lib}.rs`, new `tests/audio.rs`, `dilla-media/Cargo.toml` |
| 5 parity harness | `parity/` (new `src/{screen,fixture}.rs`, `src/{driver,lib}.rs`, new `shoot.mjs`, `tests/`, `Cargo.toml`, `references/shoot-channel-view.mjs` folded into `shoot.mjs`); the move of the channel-view fixture and reference into `PF/shell/` and `PR/shell/`; `D/automation.rs`; `DT/support/`, `DT/slice.rs` (paths only), `dilla-desktop/examples/parity_world.rs`; `C/testing.rs` (stub, feature `testing` declared) and its optional dependencies in `dilla-client/Cargo.toml`; `CT/sync.rs` and `dilla-client/examples/peer.rs` (the helpers moved out) |
| 6 shared components | `U/{dialog,confirm_dialog,action_menu,tooltip,toast,text_field,toggle,segmented,select,banner,link_banner,empty_state,pill,tag,check_list,choice_cards,icon_button,key_hint,trailing_slot,state_dot,audio_meter,qr_code}.rs` (stubs), `U/settings/` (stubs), `U/{button,row,badge,avatar}.rs`, `dilla-desktop-ui/src/lib.rs` (a new module such as `roving`); `D/{strings,format,edit_state}.rs` (stubs); new `scripts/gen-gpui-strings.mjs` and `.test.mjs`; new `dilla-desktop/examples/gallery.rs`; new `DT/components.rs`; `PF/components/`, `PR/components/` |
| 7 CI | `.github/workflows/desktop-gpui.yml`, `scripts/ci-local*.mjs`, `scripts/check-ci-workflow*`, `deny.toml`, `core/testkit` (the guard's name), `e2e/web/support/gpui-peer.ts` (stale text), the stale comments in `CT/sync.rs` and `DT/slice.rs`. In Wave 1 the controller appends one matrix line per merged test file |

## Wave 1 (Tasks 8-27)

Each row also owns the area's test files and parity directories in the last column.

| Area | Task | dilla-client | dilla-desktop | ui components | Tests and parity |
|---|---|---|---|---|---|
| shell | 8 | `C/prefs.rs` (and the `layout.*` keys) | `D/screens/{shell,channel_list,rail,header,user_panel,titlebar}.rs`, `D/screens/shell/keys.rs`, `D/router.rs`, `D/app.rs`, the window functions in `D/lib.rs` (`open_main_window*`, `main_window_options`) | `U/shell/` | `CT/shell.rs`, `DT/shell.rs`, `PF/shell/`, `PR/shell/` |
| messages | 9 | `C/features/messages.rs`, `C/state/messages.rs` | `D/screens/{timeline,composer}.rs`, `D/screens/messages/` | `U/messages/` | `CT/messages.rs`, `DT/messages.rs`, `PF/messages/`, `PR/messages/` |
| reactions | 10 | `C/features/reactions.rs`, `C/state/reactions.rs` | `D/screens/reactions/` | `U/reactions/` | `CT/reactions.rs`, `DT/reactions.rs`, `PF/reactions/`, `PR/reactions/` |
| threads | 11 | `C/features/threads.rs`, `C/state/threads.rs` | `D/screens/threads/` | `U/threads/` | `CT/threads.rs`, `DT/threads.rs`, `PF/threads/`, `PR/threads/` |
| attachments | 12 | `C/features/{attachments,gifs}.rs`, `C/state/attachments.rs` | `D/screens/attachments/` | `U/attachments/` | `CT/attachments.rs`, `DT/attachments.rs`, `PF/attachments/`, `PR/attachments/` |
| dms | 13 | `C/features/{dms,trust}.rs`, `C/state/{dms,trust}.rs`, `C/sync/trust.rs` | `D/screens/dms/` | `U/dms/` | `CT/dms.rs`, `DT/dms.rs`, `PF/{dms,trust}/`, `PR/{dms,trust}/` |
| members | 14 | `C/features/{members,roles,moderation}.rs`, `C/state/{members,roles}.rs` | `D/screens/members/` | `U/members/` | `CT/members.rs`, `DT/members.rs`, `PF/members/`, `PR/members/`; in `PF/server-settings/` and `PR/server-settings/` the files `{members,roles,self-roles,bans,moderation-log}[-b]*` |
| server settings | 15 | `C/features/server_admin.rs`, `C/state/server_admin.rs` | `D/screens/server_settings/` | `U/server_settings/` | `CT/server_settings.rs`, `DT/server_settings.rs`; the other files of `PF/server-settings/` and `PR/server-settings/` |
| user settings | 16 | `C/features/{profile,devices}.rs`, `C/state/profile.rs` | `D/screens/user_settings/` | (none) | `CT/user_settings.rs`, `DT/user_settings.rs`; `PF/user-settings/` and `PR/user-settings/` except `sign-in*` |
| voice | 17 | `C/features/voice.rs`, `C/state/voice.rs` | `D/screens/voice/` | `U/voice/` | `CT/voice.rs`, `DT/voice.rs`; `PF/voice/` and `PR/voice/` except `{blocked,afk-call}*`. Also all of `dilla-media/` from Wave 1 on (new `src/capture_source.rs`) |
| ring | 18 | `C/features/ring.rs`, `C/state/ring.rs` | `D/screens/ring/` | `U/ring/` | `CT/ring.rs`, `DT/ring.rs`, `PF/ring/`, `PR/ring/`; in `PF/voice/` and `PR/voice/` the files `{blocked,afk-call}[-b]*` |
| voice settings | 19 | `C/features/voice_prefs.rs`, `C/state/voice_prefs.rs` | `D/screens/voice_settings/` (with `sounds.rs`) | `U/voice_settings/` | `CT/voice_settings.rs`, `DT/voice_settings.rs`, `PF/voice-settings/`, `PR/voice-settings/` |
| bot | 20 | `C/features/{bot,utilities}.rs`, `C/state/bot.rs` | `D/screens/commands/` | `U/commands/` | `CT/bot.rs`, `DT/bot.rs`, `PF/{commands,polls}/`, `PR/{commands,polls}/` |
| bot admin | 21 | `C/features/{bot_admin,music}.rs`, `C/state/bot_admin.rs` | `D/screens/bot_admin/` | `U/bot_admin/` | `CT/bot_admin.rs`, `DT/bot_admin.rs`, `PF/{bot-admin,music}/`, `PR/{bot-admin,music}/` |
| notify | 22 | `C/features/notify.rs`, `C/state/unread.rs` | `D/screens/notify/` | (none) | `CT/notify.rs`, `DT/notify.rs`, `PF/notify/`, `PR/notify/` |
| search | 23 | `C/features/search.rs`, `C/state/search.rs` | `D/screens/search/` | `U/search/` | `CT/search.rs`, `DT/search.rs`, `PF/search/`, `PR/search/` |
| diagnostics | 24 | `C/features/diagnostics.rs`, `C/state/diagnostics.rs` | `D/screens/diagnostics/`, `D/screens/boot.rs` | (none) | `CT/diagnostics.rs`, `DT/diagnostics.rs`, `PF/{diagnostics,app}/`, `PR/{diagnostics,app}/` |
| onboarding | 25 | `C/features/{signup,enrol}.rs`, `C/state/onboarding.rs` | `D/screens/onboarding/`, `D/screens/create/` | `U/onboarding/` | `CT/onboarding.rs`, `DT/onboarding.rs`, `PF/{onboarding,create}/`, `PR/{onboarding,create}/` |
| factors | 26 | `C/features/{factors,browser_auth}.rs`, `C/state/factors.rs`, `C/auth.rs`, `C/signin.rs` | `D/screens/sign_in.rs`, `D/screens/factor/` | `U/factor/` | `CT/factors.rs`, `DT/factors.rs`, `PF/{sign-in,factor}/`, `PR/{sign-in,factor}/`; in `PF/user-settings/` and `PR/user-settings/` the files `sign-in[-b]*` |
| instances | 27 | `C/instances.rs` | `D/screens/instances/`, `D/windows.rs`, `D/main.rs` (the `--instance=<n>` flag) | (none) | `CT/instances.rs`, `DT/instances.rs`, `PF/instances/`, `PR/instances/` |

The shell (8) and instances (27) have no state slice: the shell keeps its layout in prefs and its route in the view, and
the instance manager runs before any `Client` exists.

## Wave 2 (Task 28)

`DT/integration.rs`, `BEHAVIOUR-GATES.md`, `README.md` (repository root, "what works"), `docs/user/desktop.md` and
`docs/user/screenshots/` (the GPUI preview section only). It may touch any file to integrate, and it reports each such
change.

## Task 1's files: only the controller edits them in Wave 1, at merge

- `C/lib.rs`, `C/features/mod.rs` (the `Area` enum), `C/state/mod.rs` (the `AppState` fields), `C/sync/route.rs` (the op
  table) and `CT/support/dump-ops.mjs`.
- `D/lib.rs` (its module list), `D/screens/mod.rs`, `D/slots.rs`, `D/keys.rs` and `DT/scaffold.rs`.
- `U/mod.rs` (`dilla-desktop-ui/src/components/mod.rs`).
- This file.

## Shared files and the rule for each

| File | Rule |
|---|---|
| `Cargo.lock` | Never edited by hand. Task 1 declared every dependency the plan names (R1). A task that needs another one adds it to its crate's `Cargo.toml`, lets cargo update the lock, and says so in its report; on a conflict the controller resolves the lock again. |
| The crates' `Cargo.toml` | No Wave 1 task edits one, except to add a dependency as above (Task 4 and Task 5 own the lines named in their rows). Task 17 owns `dilla-media/Cargo.toml` from Wave 1 on. |
| Module lists (`C/lib.rs`, `C/features/mod.rs`, `C/state/mod.rs`, `C/sync/mod.rs`, `D/lib.rs`, `D/screens/mod.rs`, `U/mod.rs`, `C/account/mod.rs`, `U/settings/mod.rs`) | Every module the plan names is declared already. A task creates submodules only inside its own directory (`D/screens/<area>/`, `U/<area>/`) and declares them in its own `mod.rs`. A new top-level module goes into the report. |
| `AppState` (`C/state/mod.rs`) | Each area has one field, of its own type, in its own file. A task adds fields to its own type, never a field to `AppState`. The core fields (`account`, `connection`, `communities`, `channels`, `timelines`, `members`, `unread`) stay the sync engine's. |
| `C/client.rs` | Task 2's. The area API on `Inner` (`publish`, `socket_requests`, `snapshot`, `api`, `engine`, `prefs`, `spawn`) and `Client::{inner, prefs}` are fixed: a feature reaches the client only through them. |
| `C/sync/route.rs` | The table of which op goes to which area. A task implements `AreaFrames::on_frame` for its own handle in its own feature file. |
| `D/slots.rs`, `D/keys.rs` | A task fills its own hooks in `D/screens/<area>/` and its own `keys.rs`. The order of the guests in a slot is the controller's. |
| Workflows, `scripts/ci-local*.mjs`, `deny.toml` | Task 7 only. Other tasks write what they need into their report. |
| `D/app.rs`, `D/lib.rs` window functions, `D/main.rs` | Task 8 (Task 27 the `--instance` flag in `main.rs`). The boot splash (24), the sign-in and factor flow (26) and the instance windows (27) need hooks there: each of those tasks reports the exact change, and the controller applies it at merge. |
| `D/screens/composer.rs` | Task 9's. Its static paperclip and smile tools are placeholders until Tasks 12 and 10 fill `composer_buttons`; the controller removes each one when it merges the task that replaces it. |

## Slots and cross-area calls

A slot is a function in `D/slots.rs` that a host view calls. It calls each guest's hook, in order. A guest task
implements only its own hook. Every hook below exists as a stub with a fixed signature.

| Slot (host) | Guests in order (task: hook) |
|---|---|
| `message_extras(item, ctx, cx) -> AnyElement` (timeline, 9) | 11 `threads::reply_chip`, 12 `attachments::attachment_cards`, 20 `commands::poll_card`, 10 `reactions::reaction_bar`, 11 `threads::thread_count` |
| `message_actions(item) -> Vec<MessageAction>` (toolbar and context menu, 9) | 9 `messages::message_actions`, 11 `threads::message_actions`, 10 `reactions::message_actions` |
| `composer_leading(ctx, cx) -> AnyElement` (composer, 9) | 11 `threads::reply_bar`, 12 `attachments::tray` |
| `composer_buttons(ctx, cx) -> AnyElement` (composer, 9) | 12 `attachments::composer_buttons`, 10 `reactions::emoji_button` |
| `composer_popup(ctx, cx) -> Option<AnyElement>` (composer, 9) | 20 `commands::picker` (the mention popup stays Task 9's own) |
| `channel_row_extras(ch, cx) -> AnyElement` (channel list, 8) | 17 `voice::channel_people` |
| `aside(route, cx) -> Option<AnyElement>` (shell, 8) | 11 `threads::pane`, else 14 `members::pane` |
| `dock(cx) -> Option<AnyElement>` (shell, 8) | 17 `voice::dock`, 21 `bot_admin::now_playing` |
| `overlays(cx) -> Vec<AnyElement>` (shell, 8) | 23 `search::palette`, 18 `ring::ring_banner`; Task 6's toasts and dialogs join at merge |
| `banners(ctx, cx) -> Vec<AnyElement>` (shell, 8) | 24 `diagnostics::banners` |
| `settings_sections() -> Vec<SettingsSection>` (settings frame, 6 and 8) | user: Profile, Devices, Appearance, Keyboard (16 `user_settings`), Sign-in (26 `factor`), Notifications (22 `notify`), Voice (19 `voice_settings`), Privacy (13 `dms`), Diagnostics (24 `diagnostics`); server: Overview, Channels, GIFs, Invites, Welcome, Join roles, Danger (15 `server_settings`), Members, Roles, Self roles, Moderation log, Bans (14 `members`), Bot, Music (21 `bot_admin`). Each points at the area's `settings_section` |

Hooks are under `D/screens/<area>/mod.rs`, so `threads::pane` means `D/screens/threads/mod.rs`.

Calls from one area into another, declared as stubs and implemented by the callee:

| Caller | Callee: function |
|---|---|
| 8 user panel | 17: `Client::voice().set_muted(bool)`, `Client::voice().set_deafened(bool)` (`C/features/voice.rs`) |
| 8 channel header | 11: `screens::threads::toggle_pane(cx)`; 14: `screens::members::toggle_pane(cx)` |
| 8 channel list "+" | 25: `screens::create::open_create_channel(community, cx)` |
| 13 Privacy section | 12: `screens::attachments::dm_gif_privacy(ctx, window, cx)` |
| 17 join and leave ticks, 18 ring | 19: `screens::voice_settings::sounds::{play, start_ring, stop_ring}` |
| 18 ring notifications | 22: `features::notify::{Notifier, Notification, NotificationKind}` |
| 25 factor choice | 26: the factor views in `screens::factor` (Task 26 defines them; the dispatch order puts 26 before 25) |
| 26 forced factor | 2: `Client::factor_completed` |

## Gateway frames (`C/sync/route.rs`)

The sync engine consumes ops 16 to 21 and 54. Every other op the TS client dispatches on goes to one area, and
`route_table_covers_ts_ops` checks this against `packages/client-core`:

| Op | Area (task): feature |
|---|---|
| 48 `presence` | members (14): `features::members::Members` |
| 49 `typing` | messages (9): `features::messages::Messages` |
| 50 `voice_state` | voice (17): `features::voice::Voice` |
| 53 `call_ring`, 55 `afk_away` | ring (18): `features::ring::Ring` |
| 56 `app_build` | diagnostics (24): `features::diagnostics::Diagnostics` |
| 61 `bot_catalog`, 62 `bot_reply`, 63 `bot_state_changed` | bot (20): `features::bot::Bot` |

## Preferences (`C/prefs.rs`)

There are two files: `<data_dir>/prefs.json` for the instance (`Client::prefs()`) and `<root>/desktop.json` for the app
(`Prefs::app(root)`). Each area writes only keys under its own prefix:

| Prefix | Task |
|---|---|
| `layout.*` | 8 |
| `drafts.*` | 9 |
| `threads.*` | 11 |
| `gifs.*` | 12 |
| `voice.*` | 19 |
| `notify.*` | 22 |
| `appearance.*` | 16 |
| `instances.*` | 27 |

Never put a secret in prefs: a key whose words include `token`, `key` or `secret` panics in debug builds, so the
push-to-talk binding is `voice.pttChord`, not `voice.pttKey`.
