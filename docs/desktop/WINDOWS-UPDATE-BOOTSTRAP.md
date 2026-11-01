# Windows installed-update bootstrap

**Status:** stable-launcher implementation under independent review; native Windows acceptance open.
**Date:** 2026-11-01.

The former coordinator restored the old program while a recovery helper was
available, but its stock NSIS template removed the ordinary executable before
installing new files. A retained journal and backup could not restart themselves
after both helper and installer died. The new custom NSIS script stages immutable
program trees behind a stable native launcher. Its full interruption gate remains
open until the implementation passes native Windows acceptance.

## Layout and authority

Use one stable per-user launcher outside every mutable Electron version tree:

```text
%LOCALAPPDATA%/Programs/Dilla/
  Dilla.exe                    stable native launcher
  Uninstall Dilla.exe          explicit user uninstall only
  versions/v-<manifest-id>/     complete immutable Electron program trees
  manifests/<sha512>.json       exact program-file manifests
%APPDATA%/Dilla/
  updates/                     private plans, signed metadata and exact installers
    windows-active-0.json       fixed committed slot
    windows-active-1.json       fixed committed slot
  ...                          existing device data; never part of program trees
```

The launcher derives its own canonical program root from its executable handle
and the canonical device-data location from the compiled application identity and
Windows known folders. A fixture builds a different fixed identity and profile.
No renderer, invite, environment variable or general command-line argument can
choose a filesystem root, executable, update feed or signing key. Program-root
ownership, reparse points, containment and manifest path collisions must be checked
before any file operation. A tiny native launcher avoids shipping a second permanent
Electron runtime; the language/build implementation still needs a scoped review.

Embed the same provisioned public release trust root, channel and URL rules in the
launcher. Account and MLS keys stay in the core worker; derived media keys use
the existing media-worker relay described in `FOUNDATIONS.md`. Retain the original signed
release JSON, detached signature and exact NSIS artifact for each accepted version.
The launcher also needs an authenticated deterministic installed-file manifest so
it can verify the entire selected executable/runtime/ASAR tree without executing
NSIS. Bind that manifest's digest to the release descriptor and publish it from the
same pinned release origin. The signed descriptor now binds a Windows program manifest as a seventh release
asset; the publisher and native launcher validate its exact digest and file tree. An unsigned path or health
report cannot select a different program. Recovery validates the original accepted
signature time, exact artifact and old manifest; it does not authorize reinstalling
an expired descriptor.

## Initial installation, update and activation

1. Initial per-user NSIS installation creates the launcher, one complete version
   tree and an initial committed slot before registering shell entry points.
2. Updating NSIS writes only an inactive nonce/manifest-derived version directory.
   Its update mode must skip `uninstallOldVersion`, launcher writes, shell/registry
   changes and automatic app launch. `/S --updated` on the existing template does
   not meet this requirement. Use the fixed suspended/job-contained actor and
   durable actual installer-PID registration; no elevation or global image kill.
3. The old accepted app remains unchanged. After the existing fresh renderer quit
   probes and late unload acceptance, its restricted helper verifies the signed
   descriptor, exact installer and complete inactive tree, then starts the exact
   new child with the existing registration, health and activation boundaries.
4. New UI and user entry stay unavailable until health succeeds and the new fixed
   slot has been flushed and atomically published. The registered complete journal
   must be flushed before that publication. Keep the previous valid committed slot intact. A slot
   contains bounded identifiers/digests and generation, never executable paths or
   commands. Missing slots are skipped, but any present unreadable, malformed or
   unauthenticated slot holds startup for repair. Select the highest authenticated
   slot before verifying its program; a damaged higher version must never silently
   select old account code. At least one committed slot must exist; the launcher
   never synthesizes initial state. Do not claim Windows directory-entry power-loss
   durability merely from Node file flushes.
5. The post-durable-write challenge/HMAC handshake releases the exact child PID,
   version and nonce. ACK loss after this commit cannot cause rollback. An ordinary
   second launch exits before loading mutable version resources while the exact
   authenticated owner still runs, including prepared and visible-complete phases.

Local startup validation must avoid irreversible account-store migrations before
commit, or establish and test their rollback compatibility. Bundled UI/WASM and a
disposable OPFS probe do not prove that an account's SQLite/KEK state remains usable.
Stored-account, enrolment, recovery, calls and draft acceptance remain separate gates.

## Interrupted recovery and shell integration

Every normal shortcut targets stable `Dilla.exe`, with no version-directory target.
The protocol command targets the same launcher and accepts only the existing
validated deep-link form. Per-user autostart targets it with the fixed hidden-start
option. During a pending update, it forwards no invite or action into a hidden child.
After a committed launch, the normal application's existing single-instance routing
handles validated user intent.

On a dead coordinator, the stable launcher verifies the pending plan and independent
old committed tree, stops/drains only the authenticated private installer job and
nonce-marked child, and selects the unchanged old committed slot. Partial new files
can remain quarantined; recovery does not require a full copy or a two-directory
swap. A status-write ENOSPC cannot prevent a delegated verified-old launch with a
retry error when no new slot was published. The recovered main rechecks the exact
old selected slot, signed provenance, old program and drained private processes
after taking its single-instance lock. A present uncertain new slot holds instead
of authorizing old account code. If the helper dies after a durable complete
journal but before publishing the slot, a hidden child can retain the lock for
its bounded 90-second activation timeout; an ordinary retry then recovers old. The failed descriptor/hash remains suppressed until explicit retry.

Keep the owned HKCU uninstall entry's exact `InstallLocation` at the stable root.
Only successful committed updates change its owned version/size fields. A
metadata write failure is recorded as a bounded private warning and cannot undo
the selected program; the new UI reports it when that diagnostic is writable. Initial
setup registers shortcuts/protocol/autostart once; silent staging never replaces
these with a mutable target. Explicit user uninstall refuses live app images and unresolved updates, validates
the exact root/entry, and removes the launcher plus owned version trees. A retained
write/delete handle blocks new launcher image mappings throughout the mutation.
Only update history is archived under a unique owned sibling; device data remains
in its existing profile. A matching durable first-install marker permits safe
retry of an interrupted same-package initial installation. Routine cleanup cannot remove active, previous-working,
executing-helper or unresolved-transaction trees and cannot recursively delete an
arbitrary report-selected path.

## Alternatives and required proof

A persistent caretaker can repair a helper-only death, and the helper can repair an
actor-only death. It still cannot make a missing executable run after both die or a
power loss. RunOnce recovery covers a later logon, and redirected shortcuts omit
direct executable launch; neither closes the normal-start requirement.

Before closing the Windows gate, actual PS5.1/NSIS native acceptance must kill only
the helper, only the actor and then both at destructive-install, launch-registration,
health, slot-rename/flush and rollback boundaries. Invoke the ordinary installed
shortcut/protocol/autostart path afterward; verify the selected program manifest,
actual process identity, preserved device/account/draft data, owned uninstall values
and explicit retry behavior. Include ENOSPC/quota/torn-slot faults, dropped activation
ACK, conflicting ordinary launches, nested CI jobs and new-only-file cleanup.
Cryptographic/path boundaries and bootstrap dependencies need an independent review.
Linux AppImage replacement and macOS Sparkle remain separate implementations.
