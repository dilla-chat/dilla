# Windows Initial Retry Transaction Implementation Plan

> Superseded on 2026-11-02 by the user-approved standard updater reset.
> This historical custom NSIS/launcher crash-recovery plan is no longer an
> implementation or release requirement. See [current Windows packaging](../../desktop/WINDOWS-UPDATE-BOOTSTRAP.md).

> **For agentic workers:** Use `superpowers:executing-plans` inline after parent approval. No subagents, staging, commits, or pushes by the implementer.

**Goal:** Recover an authenticated interrupted initial installation without overwriting a mapped program, adopting a recreated root, or modifying account data.

**Architecture:** One fixed-layout Go transaction owns quarantine, retained file leases, fresh root claims, copy, seed publication, and rollback. NSIS retains the existing two preflights and old-tree checks, hands the operation lock to that transaction, and skips its ordinary program/seed copies after successful repair. Parent-directory rename is reversible namespace movement, never evidence that an image is unmapped.

**Tech Stack:** Existing Go Windows syscall adapters, NSIS 3.12, pinned Electron/Node native test PEs.

**Spec:** Parent-approved retained-post-quarantine A constraints from the current task, reproduced below.

## Global Constraints

- Source and destination come only from compiled identity, Windows KnownFolders, and the executing protected payload's directory. The new flag accepts no path, nonce, PID, SID, or environment override.
- Existing exact marker, same-package prefix/zero-tail, manifest/tree, owner/DACL, hardlink, reparse, ADS, and zero-history policies remain mandatory.
- All existing archived files are leased before moving update metadata or copying any new program byte. Include root Go, manifests, every nested resource, and the admitted fixed uninstaller; no extension filter.
- Account/KEK/OPFS/preferences and their directories are never copied, archived, deleted, or rewritten. Only the fixed `updates` namespace can move.
- Actual mapped-PE parent-rename success is a retained negative control. No console/error filters, plaintext fallback, retries, or OS security changes.

## Review Focus

- A mapped image appears after the final old-tree scan: post-quarantine writable lease must fail and restore exact source/profile snapshots while the child stays live.
- Another session or recreated canonical root: shared gate plus atomic no-replace claims; never merge into or remove an unowned/recreated root.
- Partial lease acquisition or copy/flush failure: every acquired lease closes before rollback; errors never become successful installation.
- Process crash or power loss: leave marker, prefix-only new files, and quarantined originals recoverable; never publish a seed before complete verified program bytes.
- Finalization after successful native commit: NSIS must not recopy program/seed or roll the committed program back when registry/shortcut work fails.

## Files and interfaces

- `native/windows-launcher/main.go`, `system_windows.go`: parse exact `--installer-retry=initial`; call `runInitialRetry(cfg)`; no caller-selected location. Add read-only `--installer-preflight=initial-complete` for finalization after reacquiring the lock, and a fixed initial-finalization marker removal operation called only after NSIS registration succeeds.
- New `native/windows-launcher/initial_transaction.go`: fixed transaction state/order and rollback, portable fault seams/tests. Reuse `incompleteInitial` and exact marker/prefix validation from `initial_retry.go`.
- New `native/windows-launcher/initial_transaction_windows.go`: KnownFolder/implicit-payload adapter, atomic no-replace directory/file creation, same-volume namespace moves, copy/flush/no-replace seed publication.
- `internal/desktoplaunch/files_windows.go`, new `initial_retry_guard_windows.go`: a purpose-specific retained initial-retry lease, not a general write API. Opening uses `GENERIC_READ|GENERIC_WRITE|DELETE`, **`FILE_SHARE_DELETE` only (4), with no READ or WRITE sharing**, and `OPEN_REPARSE_POINT`; validates type/link count/file identity and existing `checkHandleSecurity` on that exact handle. Existing ADS checks run while that file and every ancestor remain pinned. Prefix comparison reads the retained handle. The returned guard exposes validation/close, not arbitrary writes or handle transfer. Existing read guards remain unchanged.
- `native/windows-launcher/maintenance_windows.go`, NSIS operation functions: reviewed per-user cross-session gate, with existing local gate retained for compatibility.
- `build/windows-installer.nsi`: explicit retry transaction branch and lock handoff, no old-program hold over parent rename, no retry CopyFiles/seed copy afterward, bounded native failure propagation, finalization only after complete authority is checked.
- Existing portable/Windows Go tests and `test/windows-installer.test.ts`, `test/native/windows-installer-maintenance.test.mjs`: kernel controls, race injection, fault matrix, full lifecycle.

## Cross-session lock decision

Use `Global\Dilla.Install.<AppID>.<TokenUser SID>` in addition to legacy `Local\Dilla.Install.<AppID>`, always Global then Local; release in reverse order. SID comes from the actual process token. Create with protected owner/DACL granting only that user, SYSTEM and Administrators. Read back owner/DACL on the actual mutex handle even when it already existed; reject foreign/unsafe/malformed objects. Object-type collisions/access errors fail closed. Go keeps `LockOSThread` through acquisition, mutations or launch, and release. The Local gate serializes legacy participants only in the SAME session: old Local-only launchers/installers in another session do not join the Global gate. Retained file leases and no-replace claims remain necessary; never claim cross-session mutex exclusion for old code.

NSIS keeps protected payload pins, generates its fixed `Uninstall <ProductName>.exe` into that protected payload before invoking the transaction, and explicitly releases only both operation mutexes before `ExecWait` of the native retry. The child acquires both locks itself, repeats fixed-layout authorization, and owns them through rollback or commit. NSIS always reacquires both afterward before interpreting success/finalizing. A competing process winning this handoff causes safe busy/refusal, not a mutation retry. New launch/NSIS participants use both names; no silent namespace replacement. Mutex creation needs no global file-mapping privilege and changes no OS policy.

## Transaction order

1. Acquire both operation locks. Derive fixed roots and implicit payload. Verify payload/marker/exact initial subset and zero canonical history again. Retain authenticated payload source guards. Before quarantine, classify an already complete same-package program, exact initial seed and matching generated uninstaller with the matching marker as `CommittedNeedsFinalization`: return that result without moving, copying or overwriting anything. An exact seed with damaged program/uninstaller, unreadable state, arbitrary malformed bytes or other history is HOLD. Only a strict authenticated proper-prefix/zero-tail legacy seed may enter repair in this NEW explicit attempt under the matching marker/exact-subset/zero-history checks.
2. Choose random sibling quarantine names internally and require absence. Rename program root, if present, into its sibling quarantine. Do not yet move `updates` or create/copy canonical program files.
3. Pin the quarantined root/ancestors; enumerate its entire admitted allowlist. Acquire every retained writable file lease, validating owner/DACL/type/link/ADS/identity on the same handle, and authenticate prefixes through those handles. Preserve the existing special admission for the old fixed uninstaller: validate/lease/archive its owned regular bytes without inventing a manifest/prefix binding for generated old NSIS code. Refuse unknown entries. Any mapping/share conflict closes all archived leases then rolls the directory back.
4. After every archived file lease is acquired, recheck the exact marker and fixed canonical zero-history update state under the cross-session gate. Any late publication fails and restores the program archive. Only then move `updates` to its sibling quarantine. Hold directory/metadata guards appropriate to the moved namespace.
5. Atomically create the canonical program and updates roots with no-replace `NtCreateFile(FILE_CREATE)` directory claims returning retained handles, using protected token-user/SYSTEM/Administrators security. Existing/reappeared roots fail; never adopt, merge, delete or write through them. Create each destination entry with create-new relative to pinned claimed parents; retain new-file write handles through completion.
6. Copy authenticated root Go, manifest, versioned bytes and the fixed generated uninstaller from protected payload and flush files. The uninstaller's authority is the marker-owning NSIS artifact that generated it in the private source, not a caller-selected path or claimed hash; pin/read that fixed source and require exact size and byte/hash equality with its newly copied destination. Verify all destination hashes/sizes by rereading retained created handles, and enumerate pinned directories against the exact allowlist; do not reopen through `VerifyTree` while exclusive write leases are held. Then atomically publish and flush generation-zero seed last, without replacement. Never copy account data.
7. Enter `PublishAttempted` before invoking final seed publication. Exact seed visibility is the irrevocable authority boundary, even when the publication call reports an error or a later flush/close fails. Reconcile through the retained claimed update root: exact expected seed means `CommittedNeedsFinalization`; unreadable, malformed, unexpected or uncertain metadata means `CommitUncertain`; rollback is allowed only when publication is definitively unsuccessful and the final seed is observably absent. Never infer absence from a read error.
8. After exact publication, close new/archived guards and report success or committed-with-error as appropriate. Retain original quarantines as preserved incomplete-install data; do not delete their contents as part of this change. Later failures must never trigger ordinary precommit rollback.

## Failure restoration

Only `Precommit` permits rollback. Delete created objects through their retained identity-bound handles, never by closing the claim and then recursively deleting its pathname. Created handles request DELETE from their atomic creation; track every created file/directory. Mark created files for disposition through their own handles and close them, then process created directories in postorder, disposing each through its final claim handle before closing it. Close auxiliary traversal handles first. A nonempty/unexpected directory or failed disposition is a restoration error: preserve it and the archive instead of deleting unknown entries. No recursive path-based cleanup of the canonical root is permitted.

Separately, close every partial/full ARCHIVED file and directory guard before restoring archives. Restore updates then program archive with no-replace renames. If an unexpected root occupies either canonical path, preserve both it and the quarantine, report failure and never merge/delete it. Mapped-image race cases with no root substitution must restore exact original source/profile bytes while the mapped child remains alive. Flush/close/rollback failures remain errors. `PublishAttempted` is reconciled first; `CommittedNeedsFinalization` and `CommitUncertain` never take this rollback path.

A crash can leave owned prefix-only roots plus preserved quarantines, but seed publication is attempted only after complete program verification. On restart: absent seed plus valid exact initial subset remains a retry candidate; exact complete same-package seed/program/generated-uninstaller plus matching marker is idempotent finalization; an exact seed with damaged bytes, unreadable/arbitrary malformed seed or accepted/pending history holds. A strict authenticated proper-prefix/zero-tail legacy seed can qualify only for a NEW explicit same-package installer repair attempt, never automatic current-attempt rollback. No scan of arbitrary sibling directories becomes installation authority.

## NSIS finalization boundary

On native success or `CommittedNeedsFinalization`, NSIS reacquires both locks and asks the read-only complete-initial preflight to verify the fixed canonical roots, exact same-package complete program/seed/generated-uninstaller bytes, matching marker and absence of accepted/pending update history. It skips all ordinary program/manifest/seed/uninstaller copies and never sets NSIS archive variables for the native transaction. Post-commit NSIS work is idempotent registry/shortcuts only: it writes no program executable. Only after all registration succeeds, remove the exact matching initial marker through a validated identity-bound metadata handle. Marker removal is finalization, never precommit cleanup. Failure before marker removal leaves the complete state and marker so the next same-package setup resumes finalization without requarantine. Failure after visible marker removal never restores old authority. `CommitUncertain` reports hold and preserves all state; it does not finalize or recopy. Existing uninstaller image guards remain unchanged.

The ordinary first-install branch must also write its generated uninstaller BEFORE initial seed publication, then stage the seed under a private temporary name, flush it, and atomically publish the final generation-zero name without replacement. Direct `CopyFiles` into the final seed name is forbidden so new installations do not produce partial final-name seeds. Recovery of a legacy authenticated prefix remains limited to a new explicit setup under the compatibility rule below. It then uses the same successful-finalization marker-removal boundary. No program/executable write may follow the seed boundary on either path. Tests must distinguish failure before final publication (retry eligible) from failure after exact final visibility (committed/uncertain, no rollback).

## Test and review sequence

- [ ] Add/run RED portable state tests for partial acquisition, mapped post-scan conflict, root reappearance, metadata race, copy/flush/seed publication failures, rollback identity conflict, and no NSIS recopy after native success. Inject publish-success/error-return, exact-seed visibility followed by flush/close errors, unreadable/malformed published seed and registration failure/restart; none may restore old authority in that attempt. Prove complete+marker resumes registration/marker removal without quarantine and foreign children are never path-deleted during rollback.
- [ ] Implement restricted leases and atomic claims; run Go race tests, Windows vet and Windows crosscompile. Validate same-handle ACL/type/link/ADS negatives and complete prefix comparison.
- [ ] Add native gate tests for restricted Global mutex creation/readback, unsafe precreated-object refusal, two-process contention, lock handoff, and abandoned ownership validation. Claim cross-session runtime proof only if actually exercised in distinct Windows sessions.
- [ ] Preserve held-child rename refusal/release success and mapped-PE rename-success negative controls. Inject the actual original-path PE after the last old-tree scan, prove postguard refusal + exact rollback while alive, then successful same-package retry after verified exit.
- [ ] Run actual Windows focused interrupted retry and all injected flush/failure cases; then full unfocused maintenance, reinstall/account preservation and complete Windows acceptance. A diagnostic probe is not all-platform release acceptance.
- [ ] Parent and independent security review approve the frozen diff and actual native evidence before any acceptance claim or feature push.

## Approved legacy seed classification

1. Exact expected initial seed plus complete verified same-package program and matching staged uninstaller: `FinalizeOnly`, never requarantine.
2. A strict authenticated proper-prefix/zero-tail seed plus matching marker, exact-package subset and zero other update history: repair is allowed only in a NEW explicit installer attempt. This preserves the prior interrupted final-name copy recovery policy; it is never ordinary launcher fallback.
3. Exact seed with damaged program, unreadable state, arbitrary malformed bytes or other history: HOLD, never fall through to prefix repair.
4. Current-attempt `PublishAttempted` uncertainty always preserves state and exits; no automatic retry or rollback. No marker-v2 journal is added solely for the legacy distinction.
