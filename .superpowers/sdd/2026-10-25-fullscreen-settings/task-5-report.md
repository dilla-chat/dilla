# Task 5 report — server settings core and people

Implemented registry-driven server settings navigation, permission-loss fallback and in-modal notice, scoped member focus restoration, the Members section, separate GIF settings, and shared Overview Save/Reset state with independent name and AFK baselines. Overview saves only changed values and retries only the AFK timeout after a partial save. Deleted or kicked servers clear edits before closing.

Fix round 1: successful AFK writes now advance the AFK baseline so Save/Reset disappear and accepted timeouts do not repeat. Section announcements use a live region inside the native modal; destructive leave/delete messages relay to the shell when the workspace closes. Permission snapshots becoming unloaded clear the active draft immediately. A ready community list that lacks the direct-linked server, or a fresh removal notice, closes the workspace and clears drafts.

Verified: `ServerSettings.test.tsx`, `MemberPane.test.tsx`, and `MemberCardHost.test.tsx` — 106 tests passed. ESLint passed for the Task 5 files. Web typecheck currently fails in the concurrent App task: `src/App.test.tsx` references `waitFor` without importing it.

Integration gap for the next task: server routes for Welcome, Join Roles, Music, Automoderation, and Moderation Log currently render no page body. Wire the delivered Task 6 `BotSettingsProvider` and page markers into the workspace before integration review. The Task 5 files do not add placeholder bodies.
