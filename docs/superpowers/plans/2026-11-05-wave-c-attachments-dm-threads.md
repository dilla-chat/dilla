# Wave C: 10 files per message and threads in DMs — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** a message carries up to 10 files, and every DM (1:1 and group) gets threads like a community channel.

**Architecture:** the attachment count is a protocol constant duplicated in the Rust core, Go, TS and the vectors; all move to 10 together. The caps that a 10-file message can exceed move with it: the default ciphertext cap rises to 192 KiB (the frame follows by derivation), and the readable-channel body cap is derived from the ciphertext cap instead of a fixed 96 KiB. Threads: the core drops its three "community groups only" gates (the core cannot tell a 1:1 DM from a group DM, so all DMs get threads), and the web shell opens the existing thread pane for DMs.

**Tech Stack:** Rust (core/dilla-core, rusqlite), Go (dillad), TypeScript/React (client-core, web), Playwright (Electron e2e).

**Spec:** Jonas's decisions of 2026-11-05 (memory `dilla-ci-overhaul-2026-11-04.md`): "Raise the caps", "Desktop-only, just ship" (no wire-version gate), "All DMs" (revised from "Group DMs only" once the core limit was found). Change sites (file:line, mapped 2026-11-05 at be975fbe) are inlined in each task; line numbers are approximate after edits. The original protocol design had `MAX_ATTACHMENTS = 10` (`docs/superpowers/plans/2026-09-23-dilla-core.md:3702`); it was cut to 4 to fit the 128 KiB cap.

## Global Constraints

- `MAX_ATTACHMENTS = 10` everywhere it exists (Rust, Go const, Go advertised limits slot [2], TS, vectors, docs, strings). `MAX_PREVIEWS` stays 2. The 25 MB per-file cap stays.
- Default `limits.max_ciphertext_bytes` = **196608**; `max_frame_bytes` stays derived as ciphertext + 512 (= **197120** at the default). The validate range 4096..1048576 is unchanged.
- The readable-message body cap (`maxEnvelopeBody`, today `96 * 1024`) becomes **the instance's `max_ciphertext_bytes`**, provided a cap-sized readable message's fan-out frame fits `max_frame_bytes`; Task 2 proves it with a test, and if the frame overhead is larger, subtracts the measured overhead (rounded up to 256) and pins it.
- No wire-version change (stays 5). No server schema change (stays 55). No client store change (stays 13). No migrations, no backfill: DM rows stored before this release keep `thread_id` NULL.
- Existing instances' `dilla.toml` pins `max_ciphertext_bytes = 131072` (`dillad init` writes every key). dillad never rewrites an operator's value; the release notes tell operators to set 196608.
- A stale client (≤ signal2) may misrender a > 4-file message or a DM thread until it updates. Accepted (desktop-only, auto-update).
- Threads in DMs reuse the community thread pane and copy exactly; no new design. Phone layout is not in scope.
- The Signal design wins any visual conflict; ask before a deviation.

## Review Focus

1. **A worst-case message on an un-raised instance:** 10 max-thumbnail files + 2 previews on an instance still at 131072 must fail with "This message is too large…" (the mapped `E_TOO_LARGE`), never silently or with a generic error. Owner: Task 2.
2. **A DM thread reply that arrives before its root**, or whose root is deleted: held exactly as in a channel (no orphan in the timeline, no lost reply). Owner: Task 3.
3. **Old DM rows:** a DM row stored before the release with NULL `thread_id` stays in the timeline; nothing disappears after the upgrade. Owner: Task 3.
4. **Leaving the DM context while its thread is open** (switch to a channel, close the DM, the DM removed): the pane closes or switches cleanly, with no stale `side` restored onto a channel with the same id slot. Owner: Task 4.
5. **A search hit inside a DM thread** opens that DM and its thread. Owner: Task 4.

---

### Task 1: The attachment limit is 10

**Files:**
- Modify: `core/dilla-core/src/envelope/mod.rs:21-24` (const + its worst-case comment, recomputed), `:857` (the pinned assert)
- Modify: `protocol/vectors/envelope.json:139-141` ("five attachments" → "eleven attachments", CBOR array head `0x8b`, 11 entries), `core/dilla-core/tests/vectors_native.rs:53` (comment only)
- Modify: `packages/protocol-vectors/src/envelope.ts:26,70`
- Modify: `internal/api/envelope.go:29`, `internal/api/instance.go:126`, `internal/dillad/server_test.go:428`
- Modify: `packages/client-core/src/attachments/crypto.ts:6`, `attachments/crypto.test.ts:34`, the `LIMITS` slot [2] in `worker/world.ts:28`, `testing/fake-server.ts:502`, `http/routes.test.ts:483`
- Modify: `packages/web/src/strings/en.ts:542,556`, `strings/strings.test.ts:114`
- Modify: `protocol/04-envelope-and-franking.md:25,231-232,239`, `protocol/09-http-api.md:895`, `protocol/07-versioning.md` (one changelog row: "attachments 4 → 10, no wire bump")
- Modify: `docs/design/flows/04-conversation.md:163,213,363,375`, `docs/user/getting-started.md:1259,1262`

**Interfaces:**
- Produces: `MAX_ATTACHMENTS = 10` (Rust `envelope::MAX_ATTACHMENTS`, TS `MAX_ATTACHMENTS` from client-core), the advertised `max_attachments` = 10.

- [ ] **Step 1: Change the pinned tests first** — `envelope/mod.rs:857` → `assert_eq!(MAX_ATTACHMENTS, 10)`; `crypto.test.ts:34` → `[8176, 26_214_400, 10]`; `server_test.go:428` → `{2, "max_attachments", 10}`; `strings.test.ts:114` → `'Up to 10 files, 25 MB each.'`. Add Rust test `ten_attachments_encode_and_decode` (10 decode OK) next to the existing limit tests; the vector covers 11 → `E_ENVELOPE_LIMIT`.
- [ ] **Step 2: Run them, expect FAIL:** `cargo test -p dilla-core envelope`, `go test ./internal/dillad -run Limits`, `npx vitest run packages/client-core/src/attachments packages/web/src/strings`.
- [ ] **Step 3: Change every site in Files** to 10. Strings: `'shell.drop.body': 'Up to 10 files, 25 MB each.'`, `'shell.tray.tooMany': 'A message carries at most 10 files.'`. Docs say "ten files" where they spelled "four". Regenerate the vector CBOR with the repo's vector tool if one exists, else hand-encode and check that Rust, Go and TS vector runners all agree.
- [ ] **Step 4: Run, expect PASS:** the Step 2 commands plus `cargo test -p dilla-core --test vectors_native`, `go test ./internal/api/...`, `npm run check:docs`.
- [ ] **Step 5: Commit** `feat(attachments): a message carries up to 10 files`.

### Task 2: The caps fit a 10-file message

**Files:**
- Modify: `internal/config/defaults.go:48` (196608) and the comments at `:13,:157-165`; `internal/ds/ds.go:134`, `internal/api/messages.go:43` (the duplicated defaults); `internal/gateway/gateway.go:129` (default 197120)
- Modify: `internal/api/readable.go:21-25` (derive the cap; the handler gets the instance's limit the way `messages.go` gets its ciphertext cap), `internal/api/reports.go:59`
- Modify: tests that assert the Go defaults: `internal/config/config_test.go:108`, `internal/dillad/server_test.go:426-448`, `internal/ds/welcome_test.go:446,479`, `internal/api/groups_test.go:441`, `internal/api/readable_test.go:352-370`. Leave `internal/config/testdata/legacy-generated.toml` unchanged (it is a byte-for-byte legacy file).
- Modify: `protocol/02-delivery-service.md:1036-1039`, `protocol/09-http-api.md:1815-1817,1866,1872`
- Modify (client): the send-error mapping in `packages/web` (where other send `E_*` codes become strings) + `strings/en.ts`: `E_TOO_LARGE` → `'send.tooLarge': 'This message is too large to send. Send fewer files or remove a link preview.'`
- TS test fakes that hardcode 131072/131584 (`world.ts`, `fake-server.ts`, `gateway.test.ts`, `controller.test.ts`, `sync/testing/*`, `protocol-vectors/src/frames.ts:40`) and `testkit/src/ds/remote.rs:3353,3953` stay unless a test asserts the Go default; they model a configured instance.

**Interfaces:**
- Consumes: `MAX_ATTACHMENTS = 10` (Task 1).
- Produces: default ciphertext 196608, frame 197120; readable cap = instance `max_ciphertext_bytes` (minus measured overhead if needed).

- [ ] **Step 1: Failing Go tests** in `internal/api/readable_test.go`:
  - `TestAMaximalTenFileEnvelopeIsAccepted`: 10 attachments each with max mime/name/thumb (8192), 2 max previews, a 4000-byte body → 201 at the default config.
  - `TestAReadableBodyOverTheCapIs413`: cap + 1 bytes → 413.
  - `TestACapSizedReadableMessageFitsItsFanOutFrame`: post a cap-sized readable message, capture the gateway fan-out frame, assert `len(frame) <= MaxFrameBytes()`.
  - `TestDefaultCiphertextCapIs196608` in `internal/config` (and the frame = 197120).
- [ ] **Step 2: Run, expect FAIL:** `go test ./internal/api -run 'Maximal|Readable|CapSized' ./internal/config -race`.
- [ ] **Step 3: Implement** the default, the derived readable cap and the comments. Recompute the worst case in the comments from the test's real encoded size (print it once), not by hand.
- [ ] **Step 4: Client mapping:** a web test that a send failing with `E_TOO_LARGE` shows `send.tooLarge` (find the existing send-error test and mirror it).
- [ ] **Step 5: Run, expect PASS:** `go test ./internal/... -race -timeout 40m`, `npx vitest run packages/web packages/client-core`, `npm run check:docs`.
- [ ] **Step 6: Commit** `feat(limits): 192 KiB ciphertext cap, readable cap follows it, a too-large send says so`.

### Task 3: The core keeps threads in DMs

**Files:**
- Modify: `core/dilla-core/src/client/messages.rs`: `HELD_ROOT` (~294-295, drop the `community_id IS NOT NULL` EXISTS), the insert `CASE` (~393, store `?20` unconditionally) and its comment (~415-417), the send gate (~1847) and its doc (~1626)
- Modify: `packages/client-core/src/sync/testing/model.ts:1501,2053` (the TS mirror, in lockstep)
- Modify: `core/dilla-core/tests/client_threads.rs:1052-1160`, `packages/client-core/src/sync/testing/model.test.ts:825-836`
- Modify: `protocol/04-envelope-and-franking.md:157-160` ("Threads apply to every text group: community channels, DMs and group DMs."), `protocol/07-versioning.md` (row: "threads in DMs, no wire bump; older clients show DM thread replies flat")

**Interfaces:**
- Produces: `send` with `threadId` succeeds in a DM; `threads(dm)` lists roots; thread replies in a DM are held out of the DM timeline exactly like a channel's.

- [ ] **Step 1: Rewrite the pinned tests as positive ones** in `client_threads.rs`:
  - `a_dm_thread_reply_is_stored_held_and_listed`: start_dm, send a root, send a reply with `thread_id` = root → the DM timeline shows the root (with reply count 1) and not the reply; `threads` lists the root; the thread timeline has the reply.
  - `send_prepare_in_a_dm_accepts_a_thread_id` (replaces the refusal test).
  - `a_dm_thread_reply_before_its_root_is_held` (reply delivered first, then root: the reply never shows in the DM timeline once the root lands; before the root it shows as the channel case does — mirror the existing channel test of the same shape).
  - `a_pre_release_dm_row_without_thread_id_stays_in_the_timeline` (plant a NULL-`thread_id` row: it is in the timeline).
  - Same cases in `model.test.ts`.
- [ ] **Step 2: Run, expect FAIL:** `cargo test -p dilla-core --test client_threads`, `npx vitest run packages/client-core/src/sync/testing/model.test.ts`.
- [ ] **Step 3: Remove the three gates** and mirror in `model.ts`. `THREADS_SQL` needs no change.
- [ ] **Step 4: Run, expect PASS:** `cargo test -p dilla-core`, the wasm build (`npm run build -w packages/core-wasm` or the repo's equivalent), `npx vitest run packages/client-core`.
- [ ] **Step 5: Commit** `feat(core): threads in DMs and group DMs`.

### Task 4: The shell opens threads in DMs

**Files:**
- Modify: `packages/web/src/shell/Shell.tsx` (the DM branch): `:1082` pass `onOpenThread` for a DM too; `:1459-1460` an aside for a DM conversation (thread pane only; the DM members aside is not added); `:1534-1566` `openThread` / `useThreadRequest` / `openHeaderThread` keyed on the open conversation's id (channel or DM); `:1663-1667` the DM header gets the same Threads button as a channel (delete the "A DM has no threads" comment); `:1728-1738` a search hit in a DM thread opens it; `:2207-2220` render `ThreadView`/`ThreadPane` for a DM; `:376,2108` the persisted `side` key covers DMs
- Modify: `packages/web/src/screens/conversation/ThreadView.tsx` (props work with `communityId: null` and a DM title in place of `channelName`), `ComposerArea.tsx:66` (`useGifCommunity` with no community: the DM's existing GIF behaviour)
- Test: `packages/web/src/shell/ShellThreads.test.tsx` (DM cases), `e2e/web/threads.spec.ts` (one DM case)

**Interfaces:**
- Consumes: Task 3's core behaviour (worker `openThread`/`loadThreads` already resolve DMs via `this.target`).

- [ ] **Step 1: Failing tests** in `ShellThreads.test.tsx`:
  - `a DM message offers Reply in thread and opens the thread pane`
  - `the DM header has a Threads button that lists the DM's threads`
  - `switching from a DM with an open thread to a channel closes the pane` (Review Focus 4)
  - `a search hit in a DM thread opens the DM and its thread` (Review Focus 5)
  - e2e `threads.spec.ts`: `two people thread in a DM` (A sends a root in a 1:1 DM, B replies in its thread; A sees the reply count and the reply in the pane, not in the timeline).
- [ ] **Step 2: Run, expect FAIL:** `npx vitest run packages/web/src/shell/ShellThreads.test.tsx`.
- [ ] **Step 3: Implement.** Reuse the channel components; no new copy except the pane title, which uses the DM's name (`dmTopic`).
- [ ] **Step 4: Run, expect PASS:** `npx vitest run packages/web`, `npx playwright test e2e/web/threads.spec.ts` (Electron lane per the repo's e2e config), `npm run lint && npm run typecheck`.
- [ ] **Step 5: Screenshots:** the DM thread pane open at 1240×820 and at the 900×600 minimum, saved under the plan workspace for Jonas.
- [ ] **Step 6: Commit** `feat(web): threads in DMs`.

### Task 5: Release notes and user docs

**Files:**
- Create: `docs/deploy/releases/v0.1.0-signal3.md` (what changed; **Operators:** set `[limits] max_ciphertext_bytes = 196608` in `dilla.toml` and restart dillad, because `dillad init` wrote the old 131072; until then a very large 10-file message is refused with a clear message; older desktop apps show > 4-file messages and DM threads wrongly until they update)
- Modify: `README.md` "what works", `docs/user/` pages on files and threads; re-shoot the screenshots that show the drop zone text or a DM header

- [ ] **Step 1:** write the notes and docs; every number matches Tasks 1-2 (10, 196608, 197120).
- [ ] **Step 2:** `npm run check:docs`.
- [ ] **Step 3: Commit** `docs: signal3 notes, 10 files and DM threads`.
