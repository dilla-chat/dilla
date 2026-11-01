# Task 9 — Browser acceptance report

## Fixture updates

- Replaced the old settings navigation expectations with the fullscreen workspace registry labels and routes. Phone settings use You → Open settings, the settings index → section → Back flow, and no app tab bar while settings are open.
- Updated grouped server settings coverage for the Overview route and the shared dirty footer. The AFK test changes the loaded timeout, saves it, and checks that the footer clears.
- Moved automated moderation and manual moderation log checks to their own server settings sections. Updated Bot role setup and role panel selectors for their separate pages and the shared Save/announcement surfaces.
- Added WebKit `search-settings.spec.ts` coverage and retained the existing authorization, voice, keyboard, and permission assertions.
- The layout acceptance now checks tap target sizes on both user and server settings indexes and sections, verifies the server Close control is inside the viewport, and closes the server index back to the originating route.

## Browser results

Against the stable post-61 phone-index build, the affected viewport/settings matrix passed:

| Engine | Result | Coverage |
| --- | ---: | --- |
| Chromium | 11 passed | Desktop, tablet, 390×844, 320×740, 720×450; AFK footer; phone index/Back; search focus; dirty draft; themes; phone shell/sign-in/landscape |
| Firefox | 8 passed | Same settings geometry, AFK, index, search, dirty draft, theme and row checks; Firefox does not run `phone.spec.ts` |
| WebKit | 11 passed | Chromium coverage plus phone shell/sign-in and 844×390 landscape |

The 11/8/11 runs preceded the latest strengthening of layout touch-target and Close-rectangle checks. The source typecheck passed after those changes; the final focused matrix should rerun on the next stable build.

After the final Close-label build, the strengthened `layout grammar holds` test passed 1/1 in each engine (Chromium, Firefox, WebKit). It checks the user and server settings index/section touch targets at all six tested sizes, the server Close button rectangle, Back to index, and Close back to the originating route.

Focused Chromium Bot and moderation verification passed all four Bot-runtime tests and manual moderation evidence retrieval. The announcement scheduler's two browser tests also passed in an earlier run (49 seconds and 5 minutes). Automatic moderation initially timed out on operation 62 before a worker upload; the trace/HAR showed no POST to `/groups/*/message` after the click and the host stayed healthy. After the provider-prefix fix build (8b89), the complete automatic moderation test passed 1/1 in 26.9 seconds, including initial read, persisted settings readback, and enforcement/log assertions. A bounded Worker RPC summary is now attached to the initial-load timeout path, but this successful run did not need to emit it.

`npm run typecheck:e2e`, `node --check e2e/scripts/shoot-docs.mjs`, and `git diff --check -- e2e` pass.

## Screenshots

The opt-in `shoot-docs.mjs --fullscreen-settings-out` capture runs against the real integrated testhost and created actual signed-in user/server settings pages. It captured Profile, Sign-in, Devices, Notifications, Voice, Privacy, Diagnostics, search results, a dirty Profile confirmation, and desktop/tablet/phone/narrow/short-wide/short-height and theme variants. Phone index, Privacy, and Diagnostics are portrait captures at 390×844.

The Music capture accurately shows the current test server's missing-Bot prerequisite and unknown provider status; no playback or Spotify connection is claimed. The callback URL wraps within its control area. The Notifications capture uses a scripted `Notification.permission = "default"` browser state and does not represent an operating-system permission prompt.

The Music view was measured on the final Close-label build at 1440×900: settings dialog `scrollTop=0`, panel `scrollTop=0`, body `scrollTop=0`, dialog bounds `(0,0)–(1440,900)`, close bar y=0–48, and close button x=1174–1344/y=9.5–37.5 (fully visible). The four final server images (Overview, Members, Music, and Automoderation) are 1440×900 captures. The user-settings captures are also final.

Actual 200% browser zoom and a physical mobile keyboard were not exercised. The 720×450 viewport is the tested short-height/keyboard-height case.

## Integration with updated main

After merging main's music chat commands and connectivity fixes (`1d50f59c`), the
focused Chromium batch passed all four cases: music chat commands, Bot response
recovery across restarts/devices, Automod, and fullscreen layout grammar. Only the
Music screenshot was refreshed to include chat-command guidance alongside provider
status; it retains the testhost's truthful unavailable state.

## Final CI

The complete browser suite is still required in CI after the fixture commit. Focused tests are not a substitute for that matrix.
