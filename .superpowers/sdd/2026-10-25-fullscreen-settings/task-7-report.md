# Task 7 — encrypted runtime provider status

## Implementation

Added encrypted, read-only `music.providers.status` operation 24 across the Go protocol/runtime, Rust codec, client-core protocol, worker, and generated bot vectors. Requests have no channel, revision, expiry, or arguments. Responses use a bounded version-1 tuple with installation ID, observation time, and fixed account/setup/reason enums.

The runtime registers the handler for Music-capable installations. It checks current owner/admin/manageCommunity authority on every read and marks the response for recomputation on exact replay. Spotify inspection reads the existing installation-bound encrypted state and local account guard without claiming OAuth state, migrating, refreshing, writing, or contacting Spotify. Missing credentials, malformed credentials, wrong installation binding, account mismatch, and unavailable reads remain distinct. Spotify helper setup is reported separately. YouTube playback readiness checks the configured runtime tools plus FFmpeg.

Added `CommunityAccess.viewAuditLog?: boolean`, populated from authoritative permission bits, and tests that verify it is distinct from `manageCommunity`. Exported the validated snapshot interface and decoder from `packages/client-core/src/bot/provider-status.ts`, re-exported from the client-core package root.

No database migration was added.

## Verification

- `go test ./internal/bot/protocol ./internal/bot/service ./cmd/dillabot -count=1` — pass.
- `npm test` in `packages/client-core` — 64 files, 2,325 tests passed.
- `npm run typecheck` in `packages/client-core` — pass after building the documented WASM package.
- `cargo test -p dilla-core --features vectors --quiet` — pass; four tests are ignored by the suite.
- `npm run vectors` in `packages/protocol-vectors` — generated operation 24 vector.
- `npm run check:docs` and `git diff --check` — pass.
- The default `cargo test -p dilla-core --quiet` omits the required `vectors` feature and fails to compile `vectors_native`; the full suite passes with `--features vectors`.

## Interface

`MusicProviderStatusSnapshot` has `version`, `installationId`, `observedAt`, `spotifyAccount`, `spotifyAccountReason`, `spotifyPlayback`, `spotifyPlaybackReason`, `youtubePlayback`, and `youtubePlaybackReason`. The TypeScript decoder accepts the CBOR response bytes and returns typed enums.

## Final integration

The registry gates Music with `manageCommunity`; `viewAuditLog` gates only the
separate Moderation log section. Runtime authorization remains authoritative.

Live-browser acceptance found that the Rust human response journal still excluded
both overview operation 0 and provider-status operation 24. The response allowlist
now admits their exact read-only shapes while retaining request/community matching
and the existing authenticated-sender checks. A regression failed before the fix.
Unit tests and encrypted round-trip fixtures cover both operations, including
journal persistence, replay deduplication and acknowledgement after a core restart.
The music browser fixture now requires a fresh runtime observation (Spotify Not
linked and YouTube Configured for its clean installation) before testing playback.
