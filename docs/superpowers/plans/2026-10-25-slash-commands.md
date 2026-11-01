# Slash commands implementation plan

> Execute with subagent-driven-development; task-scoped tests and reviews; one PR.

**Goal:** Fully working Discord-style slash commands, bot polls and cross-device reminders.
**Spec:** docs/superpowers/specs/2026-10-25-slash-commands.md
**Architecture:** Typed client registry and structured composer invoke existing actions or
authenticated encrypted bot operations; worker multiplexes durable replies; utility bot
services persist mutations and scheduled work in encrypted state.
**Stack:** React/TypeScript/Vitest/Playwright; Go bot; Rust MLS core/helper; deterministic CBOR.

## Global Constraints
User requested cheap implementers: gpt-6-luna high, lead integration/review. Single branch/PR
from fresh main. Private results unless publishing. No ordinary chat text command execution.
Existing permissions, encrypted transport and body limits preserved. No unimplemented
registry entries. Read the applicable spec sections referenced by each brief.

## Review Focus
- Multiple consumers must not ACK away another pending result.
- Authenticated identity/context, not user-provided marker/owner, controls privileged actions.
- Mutation receipts survive process death; retry never doubles an effect.
- Suggestions/async completion must not escape original conversation or disclose hidden data.
- Real shipping startup/wiring and cross-device/offline acceptance, not seeded-only tests.

### Task 1: Contracts and command registry
- Build catalog types, parser, validator, filtering and value suggestions, shared exports.
- Define Go/Rust/TS op3,70..75,80..84 codecs and shared vectors; strict request-response binding.
- Tests malformed data, bounds, contexts, deterministic encoding; registry all catalog entries.

### Task 2: Reliable response transport
- Safe capability discovery and durable response consumer routing; existing consumers migrate.
- Tests concurrent clients, persistence/reload, duplicate requests, unavailable/old bot.

### Task 3: Structured composer
- Picker, argument fields/chips, suggestions, parser integration, independent drafts and private
  result UI. Existing textarea/mentions/files unaffected. User-driven component tests.

### Task 4: Built-in handlers
- Connect every non-bot catalog action to existing services/dialogs, permission/context checks,
  focused entity suggestions; validate real effects and cancellation/draft state.

### Task 5: Existing bot handlers
- Full music plus queue clear, announcements and role panel actions. Preserve current DJ,
  manager, membership and current-call checks; poll private results through shared dispatcher.

### Task 6: Poll backend
- Durable encrypted records/votes/receipts and authenticated handlers; publication with thread
  binding and send receipts; deadlines, closure, voter pages, quotas and concurrency tests.

### Task 7: Reminder backend
- Account-owned records/indexes/receipts, durable due inbox and scheduler, list/cancel/dismiss,
  bounds/time semantics; restart/dedup/cross-user/cross-device tests.

### Task 8: Poll/reminder UI and integration
- Cards, create/vote/results dialogs, timezone reminder input, inbox and notifications; worker
  adapters and actual daemon registration/tick wiring. Complete runtime integration tests.

### Task 9: Legacy removal and documentation
- Disable/remove chat music interpreter/runtime wiring, preserve pending informational reply
  recovery without effects, update docs and verify old text/history/edit/bot cannot execute.

### Task 10: End-to-end and final review
- Browser acceptance and catalog coverage matrix. Unit/integration/lint/typecheck/build/docs
  gates, whole-branch review, fixes/recheck; one PR and final-head CI.
