# Task 3 report — routing and edit guard contracts

## Delivered

- Router imports and re-exports `SettingsSection` and `ServerSettingsSection` from the settings registry and validates URL section IDs against `SETTINGS_REGISTRY`.
- `/settings` is `{ name: 'settings', section: null }`; unknown user sections resolve to that root route.
- Added server settings routes at `/c/:communityId/settings[/:section]`, with unknown sections resolving to the server settings root. Spotify callback parsing remains first.
- Settings origins are validated as same-origin non-settings paths and persist across user/server settings navigation. `baseRoute` unwraps both route kinds.
- Indexed history positions stay contiguous on the active history branch and are recomputed after Back/Forward, so a push after Back replaces the forward branch at its adjacent position.
- Guarded router navigation and browser traversal. Native pop is restored before confirmation; only accepted traversal emits `subscribeCommittedTraversal`. `goBack(expect)` uses that committed traversal stream.
- Exported:
  - `setNavigationGuard(guard: (() => Promise<boolean>) | null): () => void`
  - `requestNavigation(action: () => void): Promise<boolean>`
  - `subscribeCommittedTraversal(listener: () => void): () => void`
  - `pushHistoryState(state, title?, url?)` / `replaceHistoryState(state, title?, url?)`
  - `useRoute()` navigation returns `Promise<boolean>`; without a guard it performs the write synchronously before returning a resolved promise.
- An approved navigation action can call the route callback once inside the approved scope. This supports guarded tab callbacks that call `navigate`.
- Added `SettingsEditProvider`, `useSettingsEdit`, `useSettingsWorkspace`, and `useSettingsPendingOperation(pending)`. Draft operations stay in refs; provider snapshots contain only dirty/busy/canSave booleans. Independently registered pending operations aggregate globally, keep beforeunload/navigation guarded after an edit section unmounts, and use copy that says accepted work continues. They do not disable another section's Save/Reset controls.
- Added local copy in `strings/settingsEditing.ts`.

## Primary wiring contract

Wrap settings workspaces in `<SettingsEditProvider>`. A section calls `useSettingsEdit({ dirty, busy, canSave, save, reset })`; it returns true when managed. Render `useSettingsWorkspace()?.footer` in SettingsFrame's footer and `useSettingsWorkspace()?.confirmation` once in the workspace layer. Use `useSettingsWorkspace()?.clear()` when permission loss invalidates a draft. Long-lived operation providers call `useSettingsPendingOperation(pending)`.

Replace app-owned raw history writes with the indexed helpers. Replace app-owned popstate reactions (phone sheets and pending tab transitions) with `subscribeCommittedTraversal`, so those effects run only after the router guard accepts traversal.

Await the `useRoute` navigation callback for guarded joins and notification thread dispatch; false means the edit prompt was declined.

## Verification

- `npm test -- --run src/router.test.tsx src/settings/edit-state.test.tsx`: **77/77 passed**. Includes actual jsdom history branching (start → A → B → Back → C), restore/replay, accepted-only traversal notification, declined `goBack(expect)`, inline state/fresh operation closures, nested approved navigation, multiple pending-operation registrations, pending work surviving section unmount, provider teardown with an open route prompt, and unindexed raw pop events without a stuck coordinator. Provider teardown resolves pending confirmation as Keep Editing, cancels the active guard request, and clears an in-flight traversal.
- `npm run typecheck`: currently reports two errors in the in-progress primary-owned `BotSettingsPages` files (`BotSettingsPages.tsx:8` null return type and `BotSettingsPages.test.tsx:15` nullable client). No router/edit-state errors.
