# Wave B Parity Round 3 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [x]`) syntax for tracking.

**Goal:** Fix the audited product differences and regenerate all 172 app parity rows with evidence-backed verdicts.

**Architecture:** Keep the existing shared UI and application flows; correct their presentation against the prototype. Retain local DM GIF opt-in. Run fresh capture worlds serially and review the resulting pairs; accepted reasons must name a dated Jonas decision or an existing sample-data exclusion.

**Tech Stack:** React/TypeScript, CSS, Vitest, Playwright, existing UX world and parity shooter.

**Spec:** User's round-3 instructions and `/home/thim/Repositories/dilla/.claude/worktrees/signal-design/.superpowers/sdd/2026-10-30-signal-wave-b/parity-accepted-audit.md`.

## Global Constraints

- Work only in lane-t9. Do not push or switch branches; leave uncommitted when git metadata is read-only.
- The prototype is `docs/design/signal/prototype/Dilla Signal Prototype.dc.html`.
- Retain Poll posted after Post poll (Jonas 2026-10-31); replace the earlier border-only field-focus decision with the prototype's accent border and outer ring.
- Capture one spec at a time into `.superpowers/sdd/2026-10-30-signal-wave-b/parity/task-9d/`.
- Do not modify prototype source or accept fixable product differences without a dated decision.

## Review Focus

- Removing the generic phone minimum must also correct the sign-in Continue wrapper; verify actual phone button geometry.
- Small/context-specific prototype controls retain their own heights rather than acquiring one global height.
- The long phone toast remains one line at 390px while narrow viewports retain a viewport clamp.
- Field focus tests/checkers must measure the actual native ring for pointer and keyboard focus, rather than preserve a superseded waiver.
- Mira's poll-dialog captures cannot cite the owner's category-less header +; inspect actual account and pixels for every citation.

### Task 1: Phone button geometry

**Files:** Button.css, phoneTargets.css.test.ts, relevant Button/onboarding stories/tests and any explicit wrapper that overrides prototype heights.
**Interface:** Existing Button props unchanged; actual heights agree with the prototype at desktop/phone widths.

- [x] Write failing prototype-height regression checks, including Continue and icon danger buttons; observe failures.
- [x] Remove the global 44px phone Button minimum and correct only proven wrapper overrides.
- [x] Run targeted tests and report exact geometry evidence and test names.

### Task 2: Toast and poll field focus

**Files:** Toast.css, PollDialog.css/tests/stories, e2e/unit/conversation-layout.test.mjs, UI story checkers.
**Interface:** Toast intrinsic width with `max-width: calc(100vw - 16px)`; native prototype ring measured exactly.

- [x] Write failing long-toast/native-focus regressions and observe failures.
- [x] Implement intrinsic phone toast sizing and prototype focus ring; remove border-only checker exceptions.
- [x] Run focused unit/native/story checks and report results.

### Task 3: Kick details and Bot menu

**Files:** CommandHost.tsx/tests and VoiceView.tsx/tests.
**Interface:** Kick confirmation retains Duration —, Reason none given and Logged without a kick reason input. Bot tile keeps playback-only title; its menu header is plain name.

- [x] Add failing kick-row and Bot-menu header regressions.
- [x] Render read-only kick details and stop passing the decorated Bot title to personMenu.
- [x] Run targeted tests and report exact names.

### Task 4: Decision records, captures and verification

**Files:** parity README, task-9d artifacts, existing capture orchestration as needed.
**Interface:** results.md, differs.txt and counts.json cover all current app manifests exactly.

- [x] Remove unsupported kick/toast deviations; record retained Poll posted and the superseding focus/phone-button decisions with 2026-10-31 dates.
- [x] Build current web and Storybook, then capture every app spec serially into task-9d.
- [x] Inspect all new pairs. Correct filled/error desktop dialog classifications; audit each accepted row's dated/exclusion citation.
- [x] Run root npm test; web/UI/e2e typechecks; parity Node tests; full story check. Record final evidence.
- [x] Verify diff/branch state. Prepare requested commit subject/trailers, leaving changes uncommitted under the read-only git policy.

### Follow-up: signup completion and final reply-jump correction

- [x] Record recovery-key Print plus Save as file, never Copy (Jonas 2026-10-30).
- [x] Reuse backup-code disclosure at every width; codes start outside the DOM and Open dilla remains visible.
- [x] Cover desktop/phone disclosure, Copy/Print, saved tick and ten-code story geometry; re-shoot both done states.
- [x] Correct confirmation action width as well as height with native geometry regressions and fresh confirmation captures.
- [x] Replace the four stale global-44px CSS assertions while retaining icon/segmented-control tiers.
- [x] Reproduce the newly exposed overflow reply-jump clipping in four native pointer/keyboard desktop/phone cases.
- [x] Apply the prototype 80px jump offset after the active/flash render and child log anchoring commit; preserve pointer/keyboard focus semantics.
- [x] Refresh affected gates and all three jump captures on the rebuilt client, then complete the final artifact audit.
