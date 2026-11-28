# Desktop docs screenshots Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every screenshot in the user docs comes from the desktop app, made by one script, and the browser screenshot tooling is gone.

**Architecture:** A manifest (`docs/user/screenshots/manifest.json`) names each doc screenshot and the parity test whose GPUI
frame it is (`target/screen-parity/<frame>-gpui-2x.png`, written by `clients/desktop/parity/src/screen.rs`). The script
`scripts/docs-shots-desktop.mjs` (`npm run docs:shots:desktop`) runs those tests and copies, and where asked crops, each
frame into `docs/user/screenshots/`. A node check fails when a doc links a missing image, an image is not linked, or a
non-manual image has no manifest entry. Screens with no fixture get one, with a native golden.

**Tech Stack:** Node 22 (`node:test`, `execFileSync`), ImageMagick `magick` for crops (no new npm dependency),
Rust/GPUI parity harness (`dilla-parity`), Markdown.

**Spec:** none written; Jonas's decisions (2026-11-27/28, AskUserQuestion) are the spec:
- Docs shots come from the desktop, in their own PR ("Own follow-up PR"); after that, delete `e2e/ux`, `docs:shots`
  and the SPA screenshot driver (recon `docs/superpowers/specs/2026-11-26-drop-browser-client-recon.md:100,213`).
- Screens with no fixture: **"Add fixtures"** — each gets a parity fixture and a native golden.
- Browser-flow text (sign-up/sign-in steps, the narrow settings index): **"Rewrite in this PR"**.
- Desktop notifications: **"Docs + screenshot"** — a clear notifications section, with a shot of a real system
  notification raised by the app on this machine.

## Global Constraints

- Build env for every cargo command: `CARGO_TARGET_DIR=$PWD/target CC=clang CXX=clang++ CARGO_BUILD_JOBS=4`; after a
  build delete `target/debug/incremental`; never `cargo clean`.
- Parity tests run as `cargo test -p dilla-desktop --features automation --test <file> -- --exact <fn>` (add
  `--include-ignored` only for tests marked `#[ignore]`).
- Native goldens: `dilla_parity::screen::assert_native_screen(fixture, golden, prepare)`; bless with
  `DILLA_PARITY_BLESS=1`; each new golden gets a row in the `native/` table of
  `clients/desktop/parity/references/linux/README.md` (format of the existing rows: fixture, what it shows, why native,
  the test). `node --test scripts/behaviour-gates.test.mjs` passes.
- Fixtures hold fictional people only (Ada, Björn, Mira, Lin, Sam, Glenn and the existing worlds); no real names,
  hosts or keys. No E2EE content, recovery keys of real accounts, or screen pixels of the user's own desktop in any
  committed file other than the cropped notification popup Jonas has looked at.
- Doc screenshot names: a replaced shot takes its frame's name without the `-gpui-2x` suffix (`shell-conversation.png`
  from `shell-conversation-gpui-2x.png`, `native-<golden>` frames drop `native-`); the existing `gpui-*.png` keep their
  names. One image per distinct frame+crop: two doc links to the same screen share one file.
- Copy in the docs: no "web client", "browser", "tab", "viewport" or "page" for the app; the user docs say "the desktop
  app" / "Dilla". Write as the existing `docs/user/desktop.md` does.
- `npm run ci:local -- --changed` passes before push.

## Review Focus

1. A manifest entry whose test passes but writes a differently named frame (copy silently picks a stale frame from an
   earlier run): the script deletes the expected frame before each test and fails if it is not there afterwards.
2. A doc link whose case or path differs from the file (`Shell-conversation.png`, `./screenshots/` vs `screenshots/`):
   the check resolves links relative to the doc file and compares exact names.
3. A crop box outside the frame: the script fails naming the entry, rather than writing a smaller image.
4. A shot that shows a secret (recovery key, TOTP setup key): only the fixtures' fictional keys appear, and the
   shooter copies only `.png` frames, never a harness sidecar (`*.layout.json`, `*.shot.json`); the check reports any
   non-`.png` file and `manifest.json` aside in `docs/user/screenshots/`.
5. Deleting the SPA tooling leaves a dangling reference (`CONTRIBUTING.md`, `e2e/package.json`, `e2e/tsconfig.json`,
   `scripts/check-ci-workflow.mjs`, `scripts/ci-areas*`): Task 5 greps for each removed name and must find none.

## Shot map

`R` README.md, `GS` getting-started.md, `V` voice.md, `B` bot-commands.md. Test files are in
`clients/desktop/dilla-desktop/tests/`. "new" = Task 2 adds the fixture/test.

| old shot | docs | new name | test (file::fn) | note |
|---|---|---|---|---|
| shell-conversation | R,GS | shell-conversation | shell::parity_shell_conversation | |
| create-invite-desktop | R,GS | server-settings-invites | server_settings::parity_server_settings_invites | |
| onboarding-3-recovery-key | R,GS | onboarding-keys | sign_in::parity_onboarding_keys | fixture key only |
| settings-devices, fullscreen-settings-devices-desktop-signal | R,GS | user-settings-devices | user_settings::parity_user_settings_devices | one file |
| conversation-actions | R,GS | messages-conversation-actions | messages::parity_messages_conversation_actions | |
| voice-stage-desktop, voice-stage-grid-desktop | R,V | voice-stage-group | voice::parity_voice_stage_group | one file |
| members-desktop | R,GS | members-profile-card | members::parity_members_profile_card | |
| dock-desktop, voice-dock-desktop | R,GS,V | voice-dock | voice::parity_voice_stage | crop to the dock |
| bot-page-health-channels-desktop | B,GS | bot-admin-health-channels | bot_admin::parity_bot_admin_health_channels | |
| signal-command-chips | B | commands-reminders | bot::parity_commands_reminders | |
| signal-command-list | B | threads-command-list | threads::parity_threads_command_list | |
| signal-poll-card | B | polls-card | bot::parity_polls_card | |
| signal-poll-dialog | B | gpui-poll-form | (existing) | reuse |
| signal-private-result | B | commands-private-result | bot::parity_commands_private_result | |
| categories-desktop | GS | shell-categories | shell::parity_shell_categories | |
| conversation-attachments | GS | attachments-cards | attachments::parity_attachments_cards | |
| conversation-edit | GS | messages-edit | messages::parity_messages_edit | |
| conversation-lightbox | GS | attachments-lightbox | attachments::parity_attachments_lightbox | |
| conversation-mention | GS | messages-mention | messages::parity_messages_mention | |
| create-channel-desktop | GS | create-channel-private | sign_in::parity_create_channel_private | text says "private" if it shows the switch |
| create-empty-desktop | GS | create-empty | sign_in::parity_create_empty | |
| create-server-desktop | GS | create-server | sign_in::parity_create_server | |
| dm-conversation | GS | dms-conversation | dms::parity_dms_conversation | |
| fullscreen-settings-diagnostics-desktop-signal | GS | diagnostics-section | diagnostics::parity_diagnostics_section | |
| fullscreen-settings-notifications-desktop-signal | GS | notify-settings | notify::parity_notify_settings | |
| fullscreen-settings-overview-desktop | GS | server-settings-overview | server_settings::parity_server_settings_overview | |
| fullscreen-settings-profile-desktop-signal | GS | gpui-user-settings | (existing) | reuse |
| header-tooltip-desktop | GS | shell-header-tooltip | shell::parity_shell_header_tooltip | |
| home-dms-desktop | GS | dms-home | dms::parity_dms_home | |
| link-offline-desktop | GS | shell-reconnecting | shell::parity_shell_reconnecting | |
| onboarding-1-invite, signin-0-entry | GS | sign-in-welcome | sign_in::parity_sign_in_welcome | one file |
| onboarding-2-name | GS | onboarding-identity | sign_in::parity_onboarding_identity | |
| onboarding-5-done | GS | onboarding-done | sign_in::parity_onboarding_done | |
| panes-resized-desktop | GS | shell-panes-resized | shell::parity_shell_panes_resized | |
| search-results-desktop | GS | gpui-search | (existing) | reuse |
| settings-devices-signout | GS | user-settings-devices-sign-out | user_settings::parity_user_settings_devices_sign_out | |
| shell-dm-badge | GS | notify-dm-badge | notify::parity_notify_dm_badge | |
| shell-unread-badge | GS | notify-unread-badge | notify::parity_notify_unread_badge | |
| signin-1-login | GS | sign-in-form | sign_in::parity_sign_in_form | |
| signin-2-code, signal-code-step | GS | sign-in-factor | sign_in::parity_sign_in_factor | one file |
| afk-call-desktop | V | ring-afk | ring::parity_ring_afk | |
| call-ring-desktop | V | ring-banner | ring::parity_ring_banner | |
| media-player-desktop | V | attachments-media-player | attachments::parity_attachments_media_player | |
| voice-settings-desktop | V | voice-settings-section | voice_settings::parity::parity_voice_settings_section | |
| voice-stage-focused-desktop | V | voice-stage-focused | voice::parity_voice_stage_focused | |
| settings-signin, fullscreen-settings-signin-desktop-signal | R,GS | user-settings-sign-in | new: user_settings::native_user_settings_sign_in | `src/screens/factor/settings.rs` |
| fullscreen-settings-privacy-desktop-signal | GS | privacy-section | new: dms::native_privacy_section | `src/screens/dms/privacy.rs` |
| trust-changed-desktop | GS | privacy-changed | new: dms::native_privacy_changed | a contact's changed identity |
| trust-compare-desktop | GS | privacy-compare | new: dms::native_privacy_compare | `src/screens/dms/compare.rs` |
| trust-conversation-desktop | GS | dms-identity-changed | new: dms::native_dms_identity_changed | blocked composer, `privacy.changed` |
| voice-blocked-desktop | V | voice-join-blocked | new: voice::native_voice_join_blocked | `voice.blockedChanged` |
| onboarding-5-factor-choose | GS | factor-choose | new: sign_in::native_factor_choose | `src/screens/factor/setup.rs` |
| onboarding-5-factor-app | GS | factor-app | new: sign_in::native_factor_app | fixture TOTP secret only |
| signin-3-recovery-key | GS | sign-in-key | new: sign_in::native_sign_in_key | only if the desktop has this step; else drop the step from the text |
| signin-4-done | GS | sign-in-done | new: sign_in::native_sign_in_done | same rule |
| shell-removed | GS | shell-removed | new: shell::native_shell_removed | `src/screens/diagnostics/removed.rs` |
| conversation-pins | GS | messages-pins | new: messages::native_messages_pins | `src/screens/messages/dialogs.rs` |
| create-rail | GS | create-rail-menu | new: shell::native_create_rail_menu | the rail's create menu open |
| bot-picker-greyed-desktop | B | commands-picker-unavailable | new: bot::native_commands_picker_unavailable | a command the viewer may not run, greyed |
| bot-channel-toggle-desktop | B | server-settings-bot-channel | new: server_settings::native_server_settings_bot_channel | a text channel's Bot channel switch |
| bot-webhook-post-desktop | B | messages-webhook-post | new: messages::native_messages_webhook_post | `src/screens/timeline.rs`, `webhook_avatar.rs` |

Every new test is `assert_native_screen(&fixture("<area>/<state>"), "<new name>", prepare)` with a fixture at
`clients/desktop/parity/fixtures/<area>/<state>.json` built from the nearest existing world (the "note" column names
the screen's source). Its frame is `native-<new name>-gpui-2x.png`.

---

### Task 1: The shooter, the manifest and the docs-image check

**Files:**
- Create: `scripts/docs-shots-desktop.mjs`, `scripts/check-docs-shots.mjs`, `scripts/check-docs-shots.test.mjs`
- Create: `docs/user/screenshots/manifest.json`
- Modify: `package.json` (scripts `docs:shots:desktop`, `check:docs-shots`, `test:docs-shots-check`; add
  `check:docs-shots` to `test`)

**Interfaces:**
- Produces: manifest shape, used by Tasks 2–4:
  ```json
  { "shots": { "<name>.png": { "test": "<file>::<fn>", "frame": "<frame>-gpui-2x.png", "crop": [x, y, w, h] } },
    "manual": { "<name>.png": "<how it was made, one line>" } }
  ```
  `crop` is optional, in frame pixels. `test` with `::parity::` keeps the module path (`voice_settings::parity::parity_voice_settings_section`).
- Produces: `node scripts/docs-shots-desktop.mjs [<name>.png ...]` — no names = all; `--list` prints the names.
- Produces: `checkDocsShots(root) -> string[]` (problems; empty = OK) exported from `check-docs-shots.mjs`.

- [ ] **Step 1: Write the failing tests** in `scripts/check-docs-shots.test.mjs` (`node:test`, temp dirs built per test):
  - `a doc linking a missing image is reported` → problem contains `missing` and the link.
  - `an image no doc links is reported` → problem contains `unlinked` and the file name.
  - `an image with neither a manifest shot nor a manual entry is reported` → contains `no manifest entry`.
  - `a manifest shot whose file is absent is reported` → contains `not shot`.
  - `the repository passes` → `checkDocsShots(repoRoot)` deep-equals `[]` (runs on the real tree; fails until Task 3).
    Mark it `{ todo: 'Task 3' }` now; Task 3 removes the `todo`.
- [ ] **Step 2:** `node --test scripts/check-docs-shots.test.mjs` → the four unit tests FAIL (module missing).
- [ ] **Step 3: Implement `check-docs-shots.mjs`.** Scan `README.md` and `docs/user/*.md` for `](…screenshots/<f>.png)`
  and `<img src=…>`; resolve relative to the doc; exact-name compare against `docs/user/screenshots/*.png`.
- [ ] **Step 4: Implement `docs-shots-desktop.mjs`.** For each selected entry: delete `target/screen-parity/<frame>`,
  run `cargo test -p dilla-desktop --features automation --test <file> -- --exact <fn-path> --include-ignored` with the
  Global Constraints env (one cargo invocation per test file, `--exact` names joined, to share the build), fail with
  the entry name if the frame is absent afterwards, then copy (or crop with `magick <frame> -crop WxH+X+Y +repage`) to
  `docs/user/screenshots/<name>`; a crop box outside the frame fails naming the entry. Delete
  `target/debug/incremental` at the end.
- [ ] **Step 5:** Write `manifest.json` with the existing `gpui-*.png` shots under `shots` (tests from
  `docs/superpowers/plans/2026-11-19-gpui-gui-gaps.md` Task 43 Step 3 and PR #173: `threads/pane-open`, `voice/stage`,
  `user-settings/profile`, `polls/dialog`, `native/music-card-live`, `server-settings/welcome-loaded`, and the rest
  from `git log -- docs/user/screenshots/gpui-*.png`). Run `node scripts/docs-shots-desktop.mjs gpui-search.png` and
  confirm the file is rewritten with the same dimensions.
- [ ] **Step 6:** `node --test scripts/check-docs-shots.test.mjs` → 4 pass, 1 todo. Commit
  `docs: a desktop docs shooter and a check that every doc image is linked and made`.

### Task 2: Fixtures for the screens the docs show and no fixture covers

Two builders in parallel, on disjoint test files: **2a** `user_settings.rs`, `dms.rs`, `sign_in.rs` and their fixtures
(sign-in settings, Privacy, changed, compare, the DM's changed identity, factor choose/app, sign-in key/done);
**2b** `voice.rs`, `shell.rs`, `messages.rs`, `bot.rs`, `server_settings.rs` and theirs (join blocked, removed,
pins, create menu, picker unavailable, Bot channel switch, webhook post). The README `native/` table rows are added
by 2a for its rows and 2b for its rows, committed with `git commit --only`.

**Files:**
- Create: `clients/desktop/parity/fixtures/<area>/<state>.json` per row marked "new" in the shot map
- Create: `clients/desktop/parity/references/linux/native/<new name>-2x.png` (blessed)
- Modify: the test files above; `clients/desktop/parity/references/linux/README.md` (`native/` table)
- Modify (only if a screen cannot be reached from a fixture): the fixture loader in
  `clients/desktop/dilla-desktop/src/fixture/` — no product behaviour change.

**Interfaces:**
- Consumes: shot map rows marked "new" (name, test fn, source file).
- Produces: each test writes `target/screen-parity/native-<new name>-gpui-2x.png`.

- [ ] **Step 1:** For each row, read the screen's source and pick the nearest existing fixture world; write the
  fixture. If a screen has no desktop counterpart (the sign-in key/done rows), do not invent one: report it, and
  Task 3 rewrites that text instead.
- [ ] **Step 2:** Add the test (`#[test] fn native_…() { assert_native_screen(...).unwrap_or_else(|e| panic!(...)) }`),
  run it without bless → FAIL (no golden).
- [ ] **Step 3:** Run it with `DILLA_PARITY_BLESS=1` → PASS; open the golden and check it shows the doc's state
  (the "note" column). Run once more without bless → PASS.
- [ ] **Step 4:** Add the README row. `node --test scripts/behaviour-gates.test.mjs` → PASS.
- [ ] **Step 5:** Report both plain and `--include-ignored` runs of every touched test binary. Commit per builder:
  `test(desktop): fixtures and native goldens for the docs' <group> screens`.

### Task 2c: The trust send pause and Compare from Settings → Privacy (amendment, Jonas 2026-11-28 "Into this docs PR")

E2EE/trust boundary: opus implementer and opus review.

**Files:**
- Modify: `clients/desktop/dilla-desktop/src/screens/composer.rs` (`Composer::blocked`),
  `src/screens/messages/composing.rs` (`Blocked`, `blocked_at`), `src/screens/dms/conversation.rs` (`composer_block`),
  the settings host's overlay drawing so `src/screens/dms/compare.rs` opens from `src/screens/dms/privacy.rs`
- Modify: `clients/desktop/dilla-desktop/tests/dms.rs` (`dms-identity-changed` re-blessed with the paused composer; the
  comment at :479 made true), the README row
- Create: fixture + native golden `privacy-compare` (`dms::native_privacy_compare`, the Compare dialog opened from
  Settings → Privacy for a changed contact, with fixture keys)

- [ ] **Step 1: Prove the send path first.** A `dilla-client` test with a channel whose `trust.send_blocks` holds an
  `UmkChanged` block: does `messages` send (or `sync` post) refuse before any network call? Write the result in the
  report. If a send can leave, that is the primary fix: refuse in the client before encrypting, error
  `privacy.changed`; test `a_send_to_a_changed_member_is_refused_before_it_leaves`.
- [ ] **Step 2:** Failing unit test `a_changed_member_pauses_the_composer` (composing or composer tests): with the
  trust gate set for the channel, `blocked_at` returns a `Blocked::Trust(&'static str)` carrying
  `composer_block`'s key; the composer draws the paused line with that text and Send/Enter do nothing.
- [ ] **Step 3:** Wire `composer_block` into `blocked_at` (after the timeout and read-only checks, as Shell.tsx orders
  `blockReason`). Pass.
- [ ] **Step 4:** Failing test `compare_opens_from_settings_privacy` (automation: click `privacy-compare-0` in the
  settings screen, the compare dialog's id is probed); make the settings host draw the compare overlay. Pass.
- [ ] **Step 5:** Re-bless `dms-identity-changed`; add `privacy-compare`; README rows. Plain and `--include-ignored`
  runs of every touched test binary, `cargo test -p dilla-client` for touched modules. Commit
  `desktop: a changed member pauses the composer, and Compare opens from Settings → Privacy`.

### Task 2d: Fixture facets for the client-only screens (amendment, Jonas 2026-11-28 "Make them fixture-able")

**Files:** `clients/desktop/dilla-desktop/src/fixture*` (automation-gated, keyed by fixture name, as `apply_trust`),
the screens' read paths only where they hard-require a `Client` for drawing (read state through the same seam the
fixture fills; no behaviour change with a real client), tests in `sign_in.rs` / `user_settings.rs`, fixtures, goldens,
README rows.

Rows: `sign-in-done` (sign_in::native_sign_in_done), `user-settings-sign-in`
(user_settings::native_user_settings_sign_in), `factor-choose`, `factor-app` (sign_in::native_factor_choose,
native_factor_app; fixture TOTP secret `JBSWY3DPEHPK3PXP`-style fictional base32 only).

- [ ] For each: failing test (no golden) → facet + fixture → bless → look → pass without bless. A screen change is
  only allowed where drawing needs a `Client` for state a fixture can supply; any such change gets a unit test that
  the real-client path is unchanged. Plain and `--include-ignored` runs reported. Commit
  `test(desktop): fixture facets and goldens for sign-in done, Sign-in settings and second-factor setup`.

### Task 3: Swap the docs to the desktop shots and rewrite the browser text

**Files:**
- Modify: `docs/user/screenshots/manifest.json` (every row of the shot map), `README.md`,
  `docs/user/getting-started.md`, `docs/user/voice.md`, `docs/user/bot-commands.md`
- Delete: every `docs/user/screenshots/*.png` no doc links afterwards (browser shots, phone shots `*-phone*.png`,
  `fullscreen-settings-*`, the `signal-*` shots)
- Modify: `scripts/check-docs-shots.test.mjs` (drop the `todo`)

- [ ] **Step 1:** Add the shot map's entries (crop boxes for `voice-dock.png` measured on the frame: the dock row
  only) and run `npm run docs:shots:desktop`. Look at every image written.
- [ ] **Step 2:** Re-point each doc link to its new name; alt text says what the image shows in the desktop app.
- [ ] **Step 3: Rewrite the browser text** in `getting-started.md`: the sign-up and sign-in sections as the desktop app
  does them (read `src/screens/sign_in.rs`, `src/screens/onboarding/`, `src/screens/factor/` for the real steps and
  copy, `src/strings.rs` for button names); remove "Settings uses a grouped index at narrow layouts … Back to
  settings"; remove QR-code and browser passkey wording that has no desktop counterpart. Search all four docs for
  `browser|web client|tab|viewport|phone` and fix every hit that describes the app.
- [ ] **Step 4:** Delete unlinked images. `npm run check:docs-shots` → no problems; the repo test passes without
  `todo`. Commit `docs: the user docs show the desktop app (shots from docs:shots:desktop, desktop sign-up and sign-in)`.

### Task 4: Desktop notifications in the docs, with a real notification (controller, on this machine)

**Files:**
- Modify: `docs/user/desktop.md` (a "Notifications" section, moved out of the "Platform notes" bullet at :311 and
  expanded), `docs/user/getting-started.md` (one line linking to it from the notification settings section)
- Create: `docs/user/screenshots/notification-linux.png`; `manifest.json` `manual` entry for it

- [ ] **Step 1:** Write the section from `src/native/notifier.rs` and the settings it obeys
  (`dilla_client::features::notify::should_notify`: the device switch, the permission, mutes, the channel on screen):
  what raises one (messages, mentions, rings with Answer/Decline), what a click does, replacing per conversation,
  closed to the tray, Windows Start-menu entry and macOS app bundle requirements, how to turn them off.
- [ ] **Step 2: The shot** (Jonas 2026-11-28: "Automation trigger"). An automation-only command `notify-demo`
  (`#[cfg(feature = "automation")]` in `src/automation.rs`) hands a fictional message notification — Mira in
  #general, "Raid at nine?" — to `native::notifier::show`, the same `post` → XDG path a real message takes; a unit
  test that the command builds that `Notification` and that it does not exist without the feature. Launch the
  automation build, send `notify-demo`. Capture with `spectacle -b -n -f -o <scratchpad>/full.png`, crop to the notification popup alone with
  `magick`, delete `full.png` at once. Show the crop to Jonas before committing it.
- [ ] **Step 3:** `npm run check:docs-shots` → OK. Commit `docs(desktop): notifications, with a Linux notification shot`.

### Task 5: Remove the browser screenshot tooling

**Files:**
- Delete: `e2e/ux/` (all), `e2e/scripts/shoot-docs.mjs`, `e2e/scripts/shoot-docs.test.mjs`
- Modify: `e2e/package.json` (drop `docs:shots`, `ux:world`, `ux:shots`, `ux:sheets`), `e2e/tsconfig.json`,
  `CONTRIBUTING.md`, and any of `scripts/check-ci-workflow.mjs`, `scripts/ci-areas*`, `scripts/ci-local-plan*`,
  `scripts/e2e-test-tools.mjs` that name them
- Keep: `clients/desktop/parity/shoot.mjs` (the web reference shooter the parity goldens' sidecars cite) and
  `e2e/web/support/host.ts` if anything else still imports it.

- [ ] **Step 1:** `git grep -n -e 'e2e/ux' -e 'shoot-docs' -e 'docs:shots\b' -e 'ux:world' -e 'ux:shots' -e 'ux:sheets'`
  outside `docs/superpowers/` → list; fix each (CONTRIBUTING points to `npm run docs:shots:desktop`).
- [ ] **Step 2:** Delete the files; the same grep returns nothing outside `docs/superpowers/`.
- [ ] **Step 3:** `npm test` and `node --test scripts/*.test.mjs` → PASS. Commit
  `e2e: remove the browser screenshot tooling (docs shots come from the desktop now)`.

### Task 6: Gate and PR

- [ ] `npm run ci:local -- --changed` → PASS (report every failure with its root cause; no retries).
- [ ] README "What works today" and `docs/user/desktop.md` read right with the new images.
- [ ] Push `docs/desktop-shots`, open the PR (title: `docs: user docs shot from the desktop app; browser screenshot
  tooling removed`), body lists the new fixtures and the removed tooling. Merge only on Jonas's go.
