# Native Linux distributions implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans task-by-task. Root may explicitly delegate narrowly owned work; workers do not stage/commit/push without root review.

**Goal:** Add real x64 DEB/RPM distribution and a pinned AUR dilla-bin recipe, preserving AppImage self-updates, sandboxing and account keys.
**Architecture:** Native packages own a fixed sibling application/helper namespace and update only through their package manager. Custom root lifecycle hooks retain an independent monotonic native helper floor; app update code cannot become package maintenance. Exact CI packages/recipe bytes enter the existing offline metadata-signing/publisher pipeline.
**Tech Stack:** Existing Go 1.27.3/x/sys/unix, TypeScript/Node 24/electron-builder 26.18.0, dpkg/RPM/ALPM tools and native x64 desktop VMs.
**Spec:** [Native-package design](../specs/2026-11-02-dilla-linux-native-packages-design.md); also read the amended [AppImage design](../specs/2026-11-02-dilla-linux-sandbox-design.md).

## Global constraints

- Retain AppImage and all existing four-platform release gates; add DEB/RPM/AUR, not APT/DNF repositories.
- Fixed ownership leaves /usr/lib/dilla/appimage and /usr/lib/dilla/native; validate but never recursively remove shared /usr/lib/dilla. Native floor/tombstone lives at /var/lib/dilla/native-sandbox.
- No Task 1 capsule wire/schema/domain/bounds changes; adapt later AppImage filesystem tasks to its new leaf.
- Native app cannot self-install, prompt for helper setup, overwrite /usr, invoke AppImage maintenance or touch account files through package hooks.
- Native helper payload 0755; verified configured helper root:root 4755; no old privileged inode/backup after successful native upgrade/removal.
- Exact version mappings/assets/modes in the spec; stable/beta.N only, package revision 1, epoch 0, x64 initially.
- No stock builder Linux hook, security-policy change, privileged network, root Electron, production private key in CI or post-acceptance package mutation.
- Downloaded RPM installation preserves host signature policy; no GPG bypass or claim metadata Ed25519 equals package OpenPGP.
- No local root execution; root plus independent review before privileged package/backend code or native acceptance.

## Review focus

1. First native installation must not poison AppImage's empty-leaf test; both install orders and cross-removal must preserve the other domain.
2. A package manager may unpack despite a failed post hook; no unconfigured/lower-generation helper may become privileged or launch.
3. An old open helper FD survives unlink; retained-inode demotion must precede manager replacement, including ALPM.
4. Fake APPIMAGE/install markers and update roles must never enable self-update in package-owned builds.
5. Beta/version normalization, stale recipe hashes and incomplete package native proof must fail publication rather than select the wrong asset.

## File and interface map

- New packages/desktop/scripts/linux-package-version.mjs (+ test): linuxPackageVersion(version) -> {upstream,deb,rpmVersion,rpmRelease,aurVersion,revision:1}; strict source for all formats.
- New packages/desktop/src/linux-installation.ts (+ test): resolveLinuxInstallation() -> appimage | system-package | unknown using compiled install kind plus fixed owned executable/identity; no renderer/path/env authority.
- New packages/desktop/native/linux-package/main.go and internal/desktoppackage/ (+ tests): native package identity/receipt/intent, process/start admission, lifecycle and eager held-object identity. Keep internal/desktopsandbox Task 1 pure wire validation unchanged.
- New packages/desktop/packaging/linux/{deb,rpm,aur}/ and packages/desktop/scripts/build-linux-packages.mjs (+ tests): explicit control/spec/hooks/PKGBUILD/.SRCINFO from one verified native payload; never use stock after-install/remove.
- Modify electron-builder.cjs, scripts/linux-appimage-launcher.cjs, scripts/build.mjs/build-native.mjs, src/updater.ts/update.ts and relevant entry/UI: separate final native/package frontend without changing AppImage updater target.
- Modify scripts/build-provenance.mjs, release-feed.mjs, verify-release.mjs, publish-release.mjs and tests; new scripts/linux-package-proof.mjs for bounded package proof records.
- New test/native/linux-packages.test.mjs and test/native/linux-package-hooks.test.mjs; reuse existing installed-account fixture for actual bundled client/account behavior.
- Modify .github/workflows/{ci,desktop}.yml and scripts/{ci-areas,check-ci-workflow}.mjs/tests only after reviewed package VM runner/proof design. No new temporary diagnostic branch/workflow.

### Task 1: deterministic versions, payload identity and update ownership

**Consumes:** spec version table, existing signed Release and UpdateCoordinator contracts.
**Produces:** linuxPackageVersion; fixed NativePackageIdentity format 1 binding manager family, upstream/package version, architecture, channel, payload SHA512 manifest, helper tuple and installKind system-package. resolveLinuxInstallation never accepts a caller-selected path.

- [ ] Add RED tests for supported beta/final mappings and unsupported semver; real dpkg/RPM/vercmp ordering for beta.1, beta.2, beta.10, final, next patch. Add native-package path ownership/identity and forged APPIMAGE/argv/marker negatives.
- [ ] Implement pure mapping/identity resolver and update-driver policy. Manager builds may report authenticated availability and a fixed GitHub release link; download/install/recovery/watchdog/setup roles reject before any privileged/system/update-pointer side effect. No fake ready/installed state.
- [ ] Run node --test packages/desktop/test/linux-package*.test.ts and desktop typecheck; existing updater/quit-guard tests must pass. Independent review before root commits this unprivileged boundary.

### Task 2: fixed native lifecycle and startup admission feasibility

**Consumes:** NativePackageIdentity and component hashes/generation/epoch; existing safe Linux FD primitives.
**Produces:** fixed root lifecycle roles prepare-install, commit-install, prepare-remove, commit-remove, check; bounded receipt/intent/tombstone at the fixed native state root. No caller path/key/network arguments.

- [ ] First prove startup admission plus real process drain under a package transaction on disposable Linux: a native launch racing prepare cannot enter after the drain check, and a live app causes a pre-mutation busy refusal. Define the retained gate/FD lifetime and manager handoff in a reviewed subdesign before implementing it; never substitute process-name scanning or killing apps.
- [ ] Add RED portable state/fault tests: lower/same-generation conflict, expired/rolled-back accepted time, unknown state, native/AppImage isolation, all fsync/close failures, retained old-FD demotion and no rollback to old privilege. Fixed native record schema must be explicitly reviewed before root mutation code.
- [ ] Implement eager fstat identity, owned regular-file/path/ACL/type/link checks and root transaction using the spec's prepare -> demote -> manager unpack0755 -> verify/promote -> receipt -> cleanup order. Missing helper during accepted removal is expected; all uncertain install state holds launch and needs manager repair.
- [ ] Run go test -race ./internal/desktoppackage/... ./internal/desktopsandbox/... and Linux vet; run actual kill/fault/old-FD matrix only on owned disposable VMs. Review backend separately before wiring hooks.

### Task 3: explicit DEB/RPM/ALPM payloads and hooks

**Consumes:** final fused native Electron/native audio payload, version/identity, fixed lifecycle executable.
**Produces:** buildLinuxPackages({payload,version,source,out}) -> exact .deb/.rpm plus manifest/provenance; generateAurRecipe({version,debURL,debSHA256}) -> deterministic recipe archive.

- [ ] Add RED package inspection tests rejecting unshare-based policy selection, ignored chmod, AppArmor/SELinux/sysctl edits, helper payload4755, shell/network bootstrap, unknown file trees, wrong desktop/protocol target or undeclared ELF dependencies. Assert root-owned immutable payload and fixed native helper symlink.
- [ ] Implement separate native staging/explicit format metadata and hooks; package DEB/RPM names dilla, AUR dilla-bin provides/conflicts dilla. No AppImage hooks in native staging, no generic builder root scripts. Hook arguments encode only exact package lifecycle/version modes, with bounded manager-specific adapter validation.
- [ ] Native manager ordering proof: DEB fresh/upgrade/abort-upgrade/abort-remove/remove/purge; RPM new pre/post plus old preun/postun on upgrade; ALPM installed PreTransaction AbortOnFail before upgrade/remove plus first-install configuration. Prove failure before unpack leaves old bytes intact; failed post/configure leaves new helper0755/pending; forced manager rollback to a lower-generation payload leaves repair hold, never old SUID restoration. Missing pre-hook/unsafe state aborts, never quietly unpacks over a still-privileged old FD.
- [ ] Generate AUR PKGBUILD/.SRCINFO/fixed hooks from the accepted immutable DEB URL/hash; makepkg ordinary user, pacman installation. Reject SKIP/latest/curl-root-script/dynamic checksums and compare extracted runtime/helper/script identities to DEB. No AUR account access/submission.
- [ ] Run node --test packages/desktop/test/linux-packages.test.ts packages/desktop/test/linux-package-version.test.ts; inspect with dpkg-deb, rpm -qpl/-qp --scripts, makepkg --printsrcinfo and namcap on native tools. Root reviews exact final scripts and helper mode/ownership exception.

### Task 4: package release/provenance integration

**Consumes:** exact CI package bytes and recipe, same accepted source/tree/run, native package proof.
**Produces:** additional three release.assets; existing four updater artifacts unchanged; per-format provenance and bounded linux-packages proof bound through acceptance source.

- [ ] Add RED signer/verifier/publisher tests for missing/duplicate DEB/RPM/recipe, wrong source/version/arch, stale DEB SHA256 in recipe, payload/component mismatch, missing manager VM success, altered final package bytes, false embedded-GPG claims and accidental additional Linux updater target.
- [ ] Expand buildArtifactNames and exact expected asset inventory together: prior7 + planned sandbox3 + native3 =13. Extend Linux build provenance with native formats and recipe source hash; define linux-packages proof format1 with source, build IDs, native distro/format/version, sandbox/kernel/policy receipts and package/helper identities. Bound file/read sizes and reconstruct trusted fields.
- [ ] Existing offline signer signs metadata/component/feed records only after acceptance, adding DEB/RPM/AUR hashes without modifying packages. Publisher verifies exact current assets and all native proofs; never regenerate recipe from mutable latest releases or read signing keys in build/CI.
- [ ] Preserve native RPM host policy: ordinary local dnf install proof with unchanged configuration plus a stricter signature-required refusal control; no --nogpgcheck or config override. Native OpenPGP signing is outside this initial exact-byte path; do not add key/account infrastructure.
- [ ] Run desktop release/provenance/publisher unit suites and mutation tests; existing seven original asset checks are expanded deliberately rather than relaxed.

### Task 5: actual distro/account/ownership acceptance

**Consumes:** exact built packages/recipe plus same-commit bundled-client tools, real package managers and desktop/keyring sessions.
**Produces:** required release proof for DEB Ubuntu24.04+Debian13, RPM Fedora44 enforcing, and AUR pinned Arch x64; no skip/compile-only equivalence.

- [ ] Add actual native fixtures for install, sandboxed ordinary launch, full account/KEK/OPFS/draft/attachment continuity across manager upgrade, remove/purge and reinstall. Use existing real installed-account behavior, not only sentinel files. Native paths work with FUSE absent; AppImage path still uses normal FUSE.
- [ ] Prove install-order coexistence: native first then AppImage empty leaf and reverse; two users/channels; removal/purge of either domain preserves the sibling helper/floor, profile and shared parent. Reject cross-domain source paths and simultaneous unsafe writers.
- [ ] Exercise real lower-generation/downgrade rejection, eager inode checks, live old-FD execution after upgrade/removal, missing/failed hooks, power/process-death between demotion/unpack/configuration/receipt and manager configure/reinstall repair. Busy app/late launch cannot race mutation; no app/account kill.
- [ ] Snapshot AppArmor/SELinux/sysctl/mount/signature policy before/after; test default restricted Ubuntu and Fedora enforcing without policy modules/bypasses, real sandboxed renderer, unprivileged Electron and OS-protected credentials. Capture package signature policy truthfully.
- [ ] Add independent required native-package VM jobs/proof collection to canonical CI and completion/publisher checks; preserve all four existing platform/client/voice/account/performance and native update gates. Workflow tests reject omitted format, filtered fixture, disabled security, weakened provenance or diagnostic-only proof.
- [ ] Update docs/user/desktop.md with GitHub local package installation, honest manual DEB/RPM upgrades/no repositories, AUR workflow/submission status, signature-policy refusal, per-domain helper ownership/floor and account-preserving removal. Root plus independent integrated review precedes any release claim.

## Handoff and limits

Review/freeze these documents before product implementation. Task 2's startup-admission/manager-handoff and native state schema need a narrow concrete subdesign with kernel/manager feasibility proof; this plan does not pretend that a scan or post hook supplies that missing exclusion. Native package support is incomplete until exact final packages pass all distro/account/ownership proofs. No workstation system modification, credential access, AUR publication, commit or push is authorized to workers by this document.
