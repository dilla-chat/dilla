# Task 6 report: the Bot recovers from a refused interaction create

## Changes
- `core/dilla-core/src/client/bot_identity.rs`: `bot_verify_interaction_snapshot` is now `bot_decode_interaction_snapshot` (signature, binding, codec) plus `bot_check_interaction_snapshot_trust` (pins and device-list caches). The public verify composes both, so every other caller is unchanged.
- `core/dilla-core/src/client/bot_interaction_create.rs`: the create decodes, checks freshness, then checks trust. A stale pinned snapshot that also fails trust now returns `snapshot_expired` (helper status 3, which rotates).
- `internal/bot/service/interaction_registration.go`: on helper status 1 from op 21, `renewInteractionAdmission`, `KeyPackage = nil`, save, return the recovery error wrapped with the unexported `errInteractionCreateRefused`.
- `internal/bot/service/interaction_registration_queue.go`: that error records a failure in the existing per-device backoff (30 s doubling to 1 h) and logs one WARN per backoff step, then continues (no entry in the 30-minute INFO report).
- No persisted format changes, no new persisted fields.

## Deviation from the brief
The WARN lives in the queue, next to the existing no-KeyPackage WARN, not in `cmd/dillabot/main.go`. The refusal never reaches main's INFO line, so main.go is unchanged. Other pending errors keep the 30-minute INFO dedupe.

## Tests
- Go `TestRegisterRestartsAfterAnInvalidCreate` (interaction_registration_test.go), `TestAnInvalidCreateBacksOffPerDevice` (interaction_registration_queue_test.go: 5 creates and 5 WARNs in 10 minutes at 30 s passes).
- Rust: an expired-snapshot-plus-changed-participant-list case added to `interaction_create_authenticates_two_leaves_and_retries_after_expiry` (bot_identity.rs); asserts `snapshot_expired`.
- RED: `go test ./internal/bot/service -run 'Invalid'` failed on both; `cargo test -p dilla-core --lib interaction_create_authenticates` failed with "interaction device-list trust state changed".
- GREEN: same commands pass. Full gates: `go test ./internal/bot/... ./cmd/dillabot/... -race -timeout 40m`, `cargo test -p dilla-core --features vectors` (306 lib tests), `cargo test -p dilla-bot-core`, wasi release build, `go test ./internal/mlswasi/...` (wasm testdata refreshed, git-ignored).
- No existing Rust test changed meaning.

## Concerns
- A status-1 caused by a pin (`changed_umk`) or a Bot-list mismatch recurs with fresh inputs; it now costs one KeyPackage per backoff step (at most 120 per day at the 1 h cap) instead of looping every 30 s. The trust root cause (ingesting the participant's current list) is not addressed.
- A caller-supplied `ParticipantKeyPackage` would mismatch the cleared intent on the next `Register`; production passes none (tests only).
