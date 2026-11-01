# Combined security and realtime acceptance

Implementation and independent review are complete. One PR contains the full scope; its checks are the authority for final-head CI status. No release or deployment is authorized by this report alone.

## Post-merge speaking measurement

PR #134 passed all 37 CI jobs and five CodeQL analyses before merge. Merged-main CI then
twice exposed the existing Firefox speaking test's driver-side polling deadline: its
100/250 ms polling schedule starts the last probe near 380 ms, leaving too little of the
400 ms bound for another browser round trip. The first trace returned true in the browser
at 397.194 ms but reached the test callback after the deadline; the second late probe also
returned true. Both failed attempts and their traces are retained.

A test-only follow-up uses Playwright's retrying locator assertion for that same attribute
and selector, with the same 400 ms timeout. This keeps retries in the browser; no product
code, assertion predicate, skip condition or timeout is changed. Five Firefox and three
Chromium repetitions pass, along with E2E TypeScript and all 534 CI-contract tests. The
follow-up still requires green merged-main CI and CodeQL before release and deployment.

## Dependency alert discovered before release

GitHub's default-branch alerts identified GHSA-mgcc-cww5-fp2q / CVE-2026-116587 in the
shipping Go JOSE 4.1.5 dependency. The upstream 4.1.6 fix rejects private parameters in
embedded JWK headers and bounds RSA key parsing. A real OIDC exchange regression accepts
validly signed tokens with stray private parameters or an oversized embedded RSA modulus
before the update, and rejects both after it; ordinary login remains accepted. The OIDC/API
race checks and upstream JOSE suite pass. The upstream default maximum RSA modulus is now
4096 bits; the release notes disclose the compatibility consequence without raising that bound.

Four other alerts concern rustls 0.23.45 only in the unshipped
`experiments/bot-providers/librespot-probe/Cargo.lock`. Neither shipping Rust lockfile includes
that crate. These experimental dependency updates are deferred; they are not represented as
resolved or included in the deployed artifacts.

## Later scope clarification

After the draft PR opened, Jonas explicitly chose “Remove web fallbacks; keep Bots working.”
The web app now drops activity/coarse stats when WSS is unavailable and never polls versions
periodically, including before login or on older connections. Startup, foreground, online and
reconnect verification remain, as do bounded retries of an unresolved build hint. Server HTTP
adapters/older wire support and native wire-3 Bots remain intact. The original fallback acceptance
results below are historical; final browser/CI acceptance must cover the clarified behavior.

Regression tests failed before removal and passed afterward: 541 core tests and 243 targeted
web tests. Updated three-engine no-fallback browser probes pass 6/6, and live-call/update probes
pass 2/2 in Chromium/Firefox. TypeScript, lint and protocol documentation checks pass. The full browser run was interrupted for this explicit scope change; its first
images/lightbox failure is preserved for investigation, and no following interrupted test is
reported as a regression.

The full WSS-only JavaScript workspace run and lint also pass, including 2,547 client-core
and 3,012 web tests. The original Chromium image test subsequently passed three unmodified
repeats; its earlier timing failure remains preserved. The complete browser matrix subsequently
passed in CI: 255 web tests with ten existing platform skips, and 59 voice tests with thirteen
existing platform/noise-mode skips. The local full voice run has the same 59/13 result. CI
completed the full web matrix; the redundant serial local web run was stopped after that result.

## CI integration correction

Run 41201576813 passed the production moderation recovery test but exposed a cold-runner
fixture build inside an existing native adapter test's ninety-second readiness wait. CI now
compiles that Rust peer in an explicit preceding step, without increasing the runtime wait
or changing assertions. A workflow regression failed before the fix; all 534 CI script tests
pass afterward. The local scan also exposed a generated-dist-dependent reader classification:
the build-tagged webdist Go test belongs to the web-build/go-release gate, rather than the
ordinary Go gate. The scanner now records that actual owner and keeps checking the dependency.

The replacement run completed all Go test steps, including the full race suite, TURN regression
and media checks, but reached the unit job's thirty-minute limit before its final static build.
Native-helper integration and its cold Rust fixture build now have their own required fifteen-minute
`go-native` job. The ordinary Go job keeps its original tests and thirty-minute limit; no existing
timeout or assertion was weakened. The scenario harness, all browser jobs and all five CodeQL
analyses passed in the preceding run. Final delivery requires the new head's complete CI gate.

## Independent review

Two independent read-only reviews found seven unique Important issues. Regression tests reproduced each before the fix: terminal refusal cleanup after restart; disabled/log-only receipt reconciliation; capability-revoked accepted receipts; journal aging of durable moderation failures; removal proof recovery after membership loss; cache-capacity deferral classification; and unresolved build-hint verification. All targeted fixes pass under race; no deferred minors.

## Verified so far

- Full Go package suite and all 942 API race tests (four exhaustive disjoint shards); focused final authority/moderation/rate race suites.
- Full Rust workspace/all-features; native helper socket suite 16/16. The initial unread-stderr-pipe hang is preserved and fixed without changing protocol assertions.
- Full JavaScript workspace tests and lint; final update watcher 10/10; TypeScript web/e2e checks; shipping build.
- Three-engine real-browser transport probes 6/6: fifteen idle minutes, no recurring version/activity/stats HTTP on healthy wire 5, real input coalescing, reconnects, build verification and legacy fallback.
- Two-engine live-call transport/update probes 2/2; existing Chromium voice suite and correctly configured Firefox voice suite passed applicable cases. Platform/noise-mode skips retain their existing conditions.
- Compiled-helper production dependency recovery tests run with accepted/absent receipts under both disabled and log-only policies; CI now downloads the same-commit helper and requires these integrations instead of silently skipping them.

The first Firefox runs lacked a reachable audio backend; explicit XDG_RUNTIME_DIR/PULSE_SERVER corrected that environment. Earlier baseline-failure classification is superseded by the corrected passing runs. Initial traces and logs remain in the task evidence archive. The full three-engine web matrix passed as recorded above; consult the PR checks for the final CI head.

## Implementation rulings

These are the ordered decisions from the execution ledger, including their tradeoffs.

1. Ruling: Apply the ordinary body deadline at NewHTTPServer's handler boundary, before any application read. Existing commit/blob route deadlines may extend it; outer defer clears for keep-alive. This avoids test-only code or changing every DecodeBody caller. Add a configurable server.read_body_timeout (default30s, zero falls back30s) for short real-transport tests. Non-body WebSocket upgrades are unaffected. Cost if wrong: route override tests must catch shortened upload budgets.

2. Ruling: Use 256 fixed hash-of-actor checkpoint shards and 1,024 reusable pending slots in protected area 8, with one native CAS committing touched shards + frontier + decision. Reusable slots bound tombstones too. A failed transaction reloads durable shards before another evaluation. Overflow defers without ACK. Cost if wrong: hostile concentration in a shard can defer intake rather than exceed 256 KiB.

3. Ruling: Persist the policy activation checkpoint after settings CAS and before acknowledging its PUT, including receipts during the CAS. If that second write fails the PUT fails; restart detects a missing matching policy checkpoint and seeds a conservative boundary once. This preserves the existing config interface and its retry receipts; no acknowledged boundary slides on restart. Cost if wrong: a failed/unacknowledged activation may conservatively classify additional events as historical.

4. Ruling: Add receipt_only=1 to the existing moderation acceptance routes, with identical session/signature/source identity validation and a no-write exit when no prior receipt exists. A changed/disabled/expired pending policy may reconcile an uncertain acceptance but cannot create a new effect. Persist accepted cases before attaching evidence so revocation cannot prevent a known receipt finishing local evidence. Case v3 adds an explicit refusal flag, retaining v2 reads.

5. Ruling: Bound native receipts specifically for new area-8 checkpoint-only transactions to the latest 64, retaining ordinary configuration/command receipts. The additive encrypted index has no core/helper wire-version change. After eviction, CAS + row/frontier reconciliation still prevents reapplying an old checkpoint. RED test retained 257 receipts instead of 65. Checkpoint snapshots also omit expired rolling/dedup entries so a long-lived actor cannot permanently fill a shard with old history (RED reproduced overflow after 3,000 benign spaced messages). Native byte-limited scan pages are bounded by total fixed rows, not an assumed 100 rows per page.

6. Ruling: Binding an anonymous token into an existing device debits its spent allowance against the device instead of discarding it. This is a conservative one-time rate reservation; ordinary waits still never reserve future tokens. Source waiters prevent merging, and only idle/full target entries may be pruned to make a combined target map fit. Cost if wrong: binding can defer rather than reset a live allowance.

7. Task 5: Ruling: wire-5 down migration maps its transport-only selection to wire 4 instead of deleting bearer sessions — wire 5 retains the exact wire-4 HTTP/attestation baseline, so rollback preserves authentication and live-call recovery — if the baseline later diverges, this mapping must be revisited before another rollback. SQLite up/down preservation is covered.

8. Task 5: Ruling: the served web handler is immutable during a process lifetime; advertise its verified entry on ready/resume, with no file watcher — deployment restarts the API and reconnect supplies the replacement handler's entry — a future in-process handler swap must explicitly publish its new verified entry.

9. Task 5: Ruling: pre-login fallback shows a manual update notice instead of reloading forms — no signed-in draft checkpoint exists there — unattended sign-in pages stay on the older build until a person finishes or reloads.

10. Task 6: Ruling: wire-5 sockets start Idle and become active only on real pointer/key input; legacy sockets retain their existing initial Online behavior — avoids fabricating input on wire-5 reconnect while preserving old clients — until a fresh input after reconnect the new tab can show Idle. Foreground and mount do not emit activity; heartbeat active is now zero and never changes the activity clock, even when an older sender sets it to one.

11. Task 6: Ruling: bound the shared activity allowance to 8192 devices per five-second window, fail closed at capacity — keeps authenticated device churn from growing the budget map without bound — excess new devices can remain Idle until the next window. HTTP and WSS coalesce silently on the same budget and never persist/replay input.

12. Task 6/7: Ruling: continue the stats implementation after Task 6's code commit while its unchanged long AFK browser gate runs — its unit/race/type gates are green and the running host/bundle are immutable; no rebuild until that browser run ends — any browser failure must be resolved before Task 6 is recorded complete or the combined PR is opened.

13. Task 7: Ruling: count running devices inside the 128 global telemetry-device slots — a tighter bound than 128 queued plus eight running is easier to reason about and always meets the queue ceiling — at saturation up to eight fewer new devices can queue. Each running device retains at most one replaceable latest sample, with no goroutine per report. Two-second job deadlines and a ten-second queue-age ceiling bound stale work.

14. Task 7: Ruling: preserve the existing bare HTTP handler's trusted-session integration contract while installing Sessions.ResolveHash explicitly in production CallStats — existing in-process API fixtures inject authorized contexts; production rechecks the exact bearer authority before and after repository work, and WSS additionally revalidates from the connection before the shared service — external embedders must install the resolver if their session authority can change during a request.

15. Task 6: Ruling: classify Firefox AFK case 9's unchanged microphone-tone failure as a pre-existing baseline limitation, not a transport regression — detached f53e213 main reproduced the identical enabled/connected-but-DTX failure (8 prior cases pass, final serial case does not run), with shipping baseline bundle and unchanged assertions/timeouts — full local browser runs remain non-green for this known baseline failure; independent review and required CI/release gates still apply, and no unrelated audio behavior or weaker test enters this PR. Evidence preserved in task6-browser-baseline, task6-browser-initial and task6-browser-rerun.

16. Task 7/8: Ruling: continue integration/docs work after the Task 7 code commit while its already-built voice suite and full Go race run finish — do not rebuild or change the served web assets while that suite runs — failures remain required gates before task completion or PR delivery, and rebased main's affected areas receive fresh checks.

17. Task 8: Ruling: inventory the actual current timers rather than the plan’s assumed Activity/Bot polling frequencies — Activity is a local slice view, reminders refresh at sixty seconds, and command status is on demand; retain encrypted interactions, AFK commands and the separate SFU RTT attribute path — moving those distinct authority/encryption workflows into the coarse telemetry frame would be an unreviewed scope expansion. Cost if wrong: some intentional HTTP requests remain, documented in docs/realtime-transport.md.

18. Task 8: Ruling: supersede the earlier Firefox baseline classification pending a correctly configured audio rerun — this shell omitted XDG_RUNTIME_DIR/PULSE_SERVER; pactl failed by default but succeeds at unix:/run/user/1000/pulse/native. Suspended contexts invalidate media checks on both baseline and branch. Interrupted the remaining invalid Firefox run, preserved all initial traces, and will rerun with the CI-required reachable audio backend. Cost if wrong: a branch media defect must still be diagnosed if it persists with audio available. No product assertions/timeouts changed.

19. Task 8: Ruling: start the two required independent read-only reviews against the frozen product implementation while final browser/whole-repository acceptance runs — Task 8 itself contains review and delivery, so its completion line cannot precede them — any subsequent product fix must be recorded and tested RED→GREEN; reviewers see the current docs/probes and ledger. Cost if wrong: a later implementation change needs explicit evidence rather than being silently covered by the old review. Reviewers security_protocol_final and whole_branch_final use gpt-6-astra/xhigh with fresh contexts; no implementer delegation.

20. Final: Ruling: plain Session(token) alias-rotation behavior is outside this fix pass because shipping callers bind SessionForDevice and no production failing path was established — device-bound sessions preserve the key — a future unbound caller must use the binding API or establish its own compatibility coverage.

21. Final: Ruling: retain synchronous presence authority lookup; no new contention defect was established and stats processing is separately bounded — current authority must be checked before recording input — a slow auth backend can delay a presence input/its socket reader; future measurements can justify a separate bounded mechanism.

22. Final: Ruling: durable moderation errors retain journal responsibility instead of the ordinary twenty-four-hour age-out — checkpoint overflow must not ACK and accepted effects require durable completion; there is no independent drain to take responsibility — prolonged native-store failure can block that journal until repaired, rather than silently abandon enforcement. Other age policies remain covered unchanged.

23. Task 8: Ruling: publish the complete implementation as one draft PR while the remaining full browser matrix runs — every planned subsystem and all seven review fixes are included, and remote CI can run concurrently — this is not acceptance or merge approval; mark ready/merge/release/deploy only after the required gates pass. Initial failures remain preserved, and the PR description identifies pending checks.
