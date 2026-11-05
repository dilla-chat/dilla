# Admin KeyPackage Publication Fix Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** An admin account's devices can publish KeyPackages again, so the Bot can add them and `/play` works. A failed publish can never again hide silently, and one un-addable device can never stall the Bot.

**Architecture:**
- **Server:** drop the role-bit check that refuses admins on `POST /v1/keypackages`. Give "a live device with an empty directory" its own error code. Add a cheap `GET /v1/keypackages` count for the session's own device.
- **Client:** surface publish failures in diagnostics, retry them with backoff, re-check the count after a gateway resume, and publish the first batch during recovery-key enrolment.
- **Bot:** back off per device on the new code, and stop stuck devices from using up the pass budget.

**Tech Stack:** Go (`internal/ds`, `internal/api`, `internal/bot`, `cmd/dillabot`), TypeScript (`packages/client-core`, `packages/web`), Playwright e2e, the protocol docs.

**Spec:** The root-cause report `/home/thim/.claude/jobs/da223a73/tmp/kp-root-cause.md` (investigated 2026-11-05 at main 772882d7, reproduced), plus Jonas's decision of 2026-11-05: "Full fix, one release" (server + client + Bot).

## Global Constraints

- Branch `fix/keypackage-admin-publish`, one PR, one release (`v0.1.0-signal2.1`). Never push to `main`.
- **Wire and storage:**
  - no wire-version change and no gateway frame change;
  - the new HTTP route is additive;
  - schema 55 and client store 13 stay unchanged, with no migrations.
- **Bot-only guards stay as they are:** the `Flags != 0` checks in `internal/bot/access/access.go:56,119`, `internal/api/bot_interaction_preflight.go:91` and `internal/ds/interaction_registration_authority.go:170` are correct for Bot accounts.
- **Error codes:** a new code joins the error vocabulary in `protocol/02-delivery-service.md` and is mapped in client-core like the existing codes.
- **Publish retry:** the client never retries a publish more often than every 5 s, and backs off to at most 5 min.
- **Commit trailers:** every commit ends with the session's Co-Authored-By and Claude-Session lines.
- **Gates:** each task runs its package's tests plus `npm run test:ci-check` and `npm run check:ci` where it touches scripts or workflows. Go changes run gofmt, vet, golangci-lint and `go test -race` for the package.
- **Healing production:** no database edits and no revocations. Upgrading the server, then relaunching each affected device, must be enough.

## Review Focus

1. **Over-correction.** Dropping the flags check must not let a disabled, deleted or Bot account publish human KeyPackages. Test each of disabled, deleted, a Bot-kind account and an admin.
2. **The heal path.** An admin's existing device that has never published must publish its full batch plus a last-resort package on its next fresh start with the fixed server, and the Bot must register it on its next pass. Test this end to end.
3. **Retry storms.** A permanently failing publish (e.g. 403 for a disabled account) must back off to the cap and never loop faster than 5 s. It must also stop when the session ends.
4. **Bot starvation.** With 8 or more permanently exhausted devices queued first, a healthy member queued after them must still be registered within a bounded number of passes.
5. **Distinguishing the error.** An unknown or revoked device keeps returning `E_NOT_FOUND`. Only a live, listed device with no ordinary row and no last-resort row returns the new code.

---

### Task 1: Admins can publish KeyPackages (server)

**Files:**
- Modify: `internal/ds/keypackage.go:158`, `:274` (drop `|| user.Flags != 0`; error text "the publishing account is disabled or deleted" stays accurate once the clause is gone)
- Test: `internal/ds/keypackage_test.go`; `internal/api/keypackages_test.go` (or the existing handler test file for that route)

- [ ] **Step 1: Failing tests.**
  - `TestPublishAcceptsAnInstanceAdmin`: flags = 1 → publish OK, and `TakeKeyPackage` returns one of the published packages.
  - `TestPublishAcceptsABotOperatorFlaggedHuman`: flags = 2 → the same.
  - `TestPublishStillRefusesADisabledAccount` and `TestPublishStillRefusesADeletedAccount`: E_FORBIDDEN.
  - `TestPublishStillRefusesABotKindAccount`: whatever the current code returns for Bot accounts (the `s.Bot != nil` / `accountKindVerdict` path), asserted unchanged.
  - Handler test: an admin session's `POST /v1/keypackages` → 201.
- [ ] **Step 2:** Run `go test ./internal/ds/ -run 'Publish'`. The admin and operator tests FAIL with E_FORBIDDEN.
- [ ] **Step 3:** Remove the clause at both sites.
- [ ] **Step 4:** Run `go test -race ./internal/ds/ ./internal/api/`, then golangci-lint. All PASS.
- [ ] **Step 5:** Commit: `fix(ds): an instance admin's devices can publish KeyPackages again`

### Task 2: A distinct "no key packages" answer, and a count route

**Files:**
- Modify: `internal/ds/keypackage.go` (`TakeKeyPackage` ~325-335) to return `E_KEYPACKAGES_EXHAUSTED` (HTTP 404, distinct `code`) for a target device that is live, in its owner's current device list, not revoked, and has no ordinary and no last-resort row. Unknown or revoked devices keep `E_NOT_FOUND`.
- Create: `GET /v1/keypackages` (session, its own device) → `200 [ordinary_active_count(uint), has_current_fallback(bool)]`, mirroring the Bot runtime's `GET …/keypackages` shape. Use the same counting as `ready`'s `keypackages_remaining` (`internal/gateway/session.go:622-627`); share the store query instead of duplicating it.
- Modify: `protocol/02-delivery-service.md` (error vocabulary row for `E_KEYPACKAGES_EXHAUSTED`) and `protocol/09-http-api.md` (route row); `packages/client-core/src/http/routes.ts` (`getKeyPackageCount`) and its error-code mapping.
- Test: `internal/ds/keypackage_test.go`, the handler test, a client-core routes test, and `npm run check:docs`.

**Interfaces:**
- Produces: error code string `E_KEYPACKAGES_EXHAUSTED`; client-core `routes.getKeyPackageCount(): Promise<{ ordinary: number; fallback: boolean }>`.

- [ ] **Step 1: Failing tests.**
  - `TestTakeFromALiveEmptyDeviceIsExhausted`: E_KEYPACKAGES_EXHAUSTED.
  - `TestTakeFromAnUnknownDeviceIsNotFound` and `TestTakeFromARevokedDeviceIsNotFound`: E_NOT_FOUND.
  - Count route: after publishing 5 ordinary + 1 last-resort it returns `[5, true]`; on an empty device it returns `[0, false]`.
  - A client-core route test parses that response.
- [ ] **Step 2:** Run the tests. FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run `go test -race ./internal/ds/ ./internal/api/ ./internal/gateway/`, `npm test -w @dilla/client-core` and `npm run check:docs`. All PASS.
- [ ] **Step 5:** Commit: `feat(ds): a live device with no key packages says so, and a device can read its own count`

### Task 3: The client never loses a publish failure

**Files:**
- Modify: `packages/client-core/src/controller.ts` (`:2338-2345` signup batch, `:2580-2586` `onReady` refill, `enrolWithKey` ~`:3090-3140`, resume handling); `packages/client-core/src/account/keypackages.ts`; `packages/client-core/src/http/routes.ts:452-453` (mark the POST safe to retry: it is idempotent per `kp_ref`, `internal/ds/keypackage.go:290-300`).
- Modify: the diagnostics state and the Settings → Diagnostics row (`packages/web`), showing last attempt time, last error code and the count.
- Test: `packages/client-core/src/controller.test.ts` (or the existing fake-world suite), and a web test for the diagnostics row.

**Interfaces:**
- Consumes: `routes.getKeyPackageCount()` from Task 2.
- Produces: diagnostics field `keyPackages: { count: number | null; lastAttempt: number | null; lastError: string | null }`.

- [ ] **Step 1: Failing tests.**
  - A 403 from `postKeyPackages` is recorded in diagnostics and retried after 5 s, then 10 s and so on, capped at 5 min, until it succeeds. A 500 is treated the same.
  - The retry timer stops on sign-out and session end.
  - A gateway resume calls `getKeyPackageCount` and refills when the count is below the threshold (8).
  - Recovery-key enrolment posts the first batch (32 + last-resort) before `enterReady` resolves (`keyPackagesOwed = true` in `enrolWithKey`).
  - The signup path is unchanged apart from surfacing errors.
- [ ] **Step 2:** Run `npm test -w @dilla/client-core`. FAIL.
- [ ] **Step 3:** Implement. No `.catch(() => undefined)` remains on a publish.
- [ ] **Step 4:** Run `npm test -w @dilla/client-core -w @dilla/web` and `npm run lint`. PASS.
- [ ] **Step 5:** Commit: `fix(client): publish failures are retried and shown, and a new device publishes before it is ready`

### Task 4: The Bot backs off a device with no key packages and can't be starved

**Files:**
- Modify: `internal/bot/service/interaction_registration.go:218-221` (classify `E_KEYPACKAGES_EXHAUSTED` and `E_NOT_FOUND` on the take); `internal/bot/service/interaction_registration_queue.go` (per-device backoff of 30 s doubling to 1 h, recorded in the job; budget counts only devices actually attempted; a job that made no progress moves to the tail); `cmd/dillabot/main.go:623-624` (log WARN once per device on entering backoff, INFO only on state changes).
- Test: `internal/bot/service/interaction_registration_queue_test.go`

- [ ] **Step 1: Failing tests.**
  - `TestExhaustedDeviceBacksOff`: attempts happen at 30 s, 60 s, 120 s and so on, capped at 1 h.
  - `TestStuckDevicesDoNotStarveLaterMembers`: 8 exhausted devices across 4 jobs queued first, then one healthy member. The healthy member registers within 2 passes.
  - `TestPublishClearsBackoff`: once the take succeeds, the device registers and its backoff entry is gone.
  - `TestBackoffLogsOnce`: one WARN per device until its state changes.
- [ ] **Step 2:** Run `go test ./internal/bot/service/ -run 'Exhausted|Starve|Backoff'`. FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run `go test -race ./internal/bot/... ./cmd/dillabot/` and golangci-lint. PASS.
- [ ] **Step 5:** Commit: `fix(bot): back off a device with no key packages and keep stuck devices from starving the queue`

### Task 5: An admin fixture and the end-to-end heal

**Files:**
- Modify: `internal/dillad/dilladtest/host.go` (an option to make the first account an instance admin, as `dillad init`'s bootstrap invite does).
- Create: an e2e spec in the existing e2e layout (e.g. `e2e/web/admin-keypackages.spec.ts`). An admin signs up, enrols a second device via recovery key, and the Bot is installed. The Bot registers the second device and the admin sees a Bot command (e.g. `/play` or another Bot command in the picker) on the new device.
- Modify: wherever e2e shard timing or lists need the new spec (`scripts/go-shard.mjs` / e2e shard config, if specs are enumerated).

- [ ] **Step 1:** Write the e2e. It must FAIL against a server with Task 1 reverted: verify by temporarily reverting locally, without committing.
- [ ] **Step 2:** Run it with the fix in place. PASS in both the desktop lane and the local browser lane where the harness allows.
- [ ] **Step 3:** Commit: `test(e2e): an instance admin's new device is addable and gets Bot commands`

### Task 6: Release notes, docs and the heal instructions

**Files:**
- Create: `docs/deploy/releases/v0.1.0-signal2.1.md` with:
  - **What's fixed:** an admin's devices couldn't publish key packages since bot1, so Bot commands were missing on new devices and older devices would run out by January 2027. Publish failures are now visible and retried, and the Bot backs off.
  - **Heal:** upgrade, then quit and relaunch each affected device. A resume is not enough. No revocation is needed.
  - The new route and error code.
  - The versions table (schema 55, store 13, wire 5 unchanged).
  - **Deploy order:** dillad and dillabot from the same release. Restarting `dilla` restarts `dillabot`.
- Modify: `docs/user/` troubleshooting (if a page on missing Bot commands or devices exists, add "relaunch the app after the server update"), and README "what works" if it lists Bot commands.
- [ ] **Step 1:** Write the docs; `npm run check:docs` PASS.
- [ ] **Step 2:** Commit: `docs(releases): signal2.1 notes and the KeyPackage heal`

---

## Out of scope

- A gateway `keypackagesLow` push frame. The resume count check covers long sessions without a wire change.
- Server metrics and alerts for empty directories. That's a follow-up.
- Revoking or repairing Jonas's two devices. The heal is a relaunch after the upgrade.
