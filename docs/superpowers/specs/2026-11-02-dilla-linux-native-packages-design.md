# Native Linux distributions: DEB, RPM and AUR

**Status:** Concrete design for root and independent security review; no package implementation or host changes.
**Date:** 2026-11-02
**Scope:** User requested DEB, RPM and AUR and delegated engineering choices. Retain AppImage and its authenticated self-update. Publish standalone x64 packages on immutable public GitHub release tags; do not create APT/DNF repositories or change signing/OS policies.
**Plan:** [Implementation plan](../plans/2026-11-02-dilla-linux-native-packages.md).
**Related:** [AppImage sandbox design](2026-11-02-dilla-linux-sandbox-design.md). This design supersedes its earlier package-format non-goal and scopes sole-helper/floor guarantees to distinct ownership domains. Task 1's pure capsule contract remains unchanged.

## Distribution and update ownership

| Distribution | Install/update authority | Application behavior |
|---|---|---|
| AppImage | Existing authenticated application updater; AppImage-domain sandbox maintainer | Keep normal FUSE startup, guarded quit/update/recovery and automatic prerequisite handling |
| DEB / RPM | Administrator through the native package manager, installing downloaded GitHub assets | Check signed metadata and report a newer version; fixed release-page link and “Update using your package manager”; no download/install-ready/quit-install transition |
| AUR dilla-bin | User-reviewed versioned recipe, makepkg as ordinary user, pacman installation | Same manager-owned runtime policy; AUR helpers may update the recipe/package through normal user choice |

A downloaded DEB/RPM does not acquire an update repository. “Package manager updates” means installing its newer local package with that manager unless the administrator separately supplies a repository. Do not imply unattended APT/DNF updates. AUR publication is a later authenticated account action; generate/review the recipe without accessing credentials or claiming submission.

Install kind is a compiled/build-owned enum (appimage or system-package), verified against fixed root-owned package identity and executable/resources paths. Environment APPIMAGE, argv, a writable marker or renderer input cannot turn a system package into a self-updating AppImage. Managed builds must reject app update/recovery/watchdog installation roles before spawning an updater, helper, pkexec, capsule bootstrap or writing an activation pointer. Existing read-only release checks may remain. Development and unknown layouts fail closed for installation.

Native files use the same application/profile identity and OS-protected credentials as AppImage; preserve existing account/KEK/OPFS/draft state and single-instance rules. Native package updates are explicit system operations, not a way around application-initiated call/draft/attachment quit guards. Package tools never open, migrate, chown or delete any home/profile/keyring data, including on purge.

## Fixed ownership and sandbox layout

| Path | Owner and purpose |
|---|---|
| /usr/lib/dilla | Validated root:root 0755 common parent; no writable ACL; no recursive cleanup |
| /usr/lib/dilla/appimage | Existing AppImage maintainer/helper/receipt/intent/lock; AppImage alone manages these |
| /usr/lib/dilla/native/app | Package-owned fused Electron, ASAR, native audio resources and unprivileged frontend |
| /usr/lib/dilla/native/chrome-sandbox | Package-owned helper; archived payload mode 0755, promoted to root:root 4755 only by successful package configuration |
| /usr/lib/dilla/native/package-maintainer | Package-owned non-setuid 0755 fixed-role Go executable; root package hooks only |
| /usr/lib/dilla/native/package.json | Exact bounded build-owned package identity/component tuple and payload manifest; no selectable paths |
| /var/lib/dilla/native-sandbox | Root:root 0700 durable native receipt, intent and removal tombstone, outside the removable payload; no account data |
| /usr/bin/dilla and desktop/icon entries | Fixed unprivileged native entry; desktop/protocol registrations target this entry |

Native app/chrome-sandbox is a packaged absolute symlink to the native helper only; inspect its actual Chromium adjacent-helper behavior in native acceptance. Neither frontend consults caller-selected helper paths; remove CHROME_DEVEL_SANDBOX, never add --no-sandbox/--disable-setuid-sandbox. Native startup validates native package/receipt/helper identity and actual sandbox startup; it never invokes the AppImage setup path. Restricted hosts use the explicitly installed package helper, not a generic unshare guess. No FUSE dependency is added to native packages.

The package manager owns all native executables. AppImage maintainer/capsule operations cannot chmod, replace, remove or adopt them. Native scripts cannot touch the AppImage leaf. Uninstall leaves the shared parent and other domain intact. Both helper domains may coexist; generation numbers have a common release meaning, but maximum accepted generation/time and tombstone are enforced independently. Updating one domain does not remediate an outdated helper owned by the other; report the other distribution's normal update requirement rather than claiming machine-wide retirement.

Only one native app package/channel is installed at once. DEB/RPM package name dilla; AUR dilla-bin provides dilla and conflicts with other native dilla providers. Stable/beta are deliberate manager-controlled replacements using the same root and persistent native floor; no forced file overwrite/adoption flags. AppImage may coexist and share the existing profile lock; changing application channel does not lower either helper floor.

## Native helper lifecycle

The reviewed [admission and manager contract](2026-11-02-dilla-linux-native-admission-design.md) refines this outline: permanent manager-family binding, main-only process-lifetime SH locking, qualified intact effective ALPM hooks and post-unpack successor admission. It governs those ordering details; actual fused-runtime and manager proofs remain required.

Do not enable electron-builder's stock Linux hooks. The pinned after-install.tpl currently infers helper mode from root's unshare --user result, ignores chmod failures, and creates/reloads AppArmor policy; after-remove unloads it. Supply explicit DEB/RPM/ALPM hooks and assert those stock operations are absent from final package scripts.

Native package authorization is the normal administrator-selected package installation trust point, including its root scripts, not an application metadata signature granting a new privileged API. A non-root process cannot call root lifecycle operations. Every new authentic package must retain the prior native receipt/intent/floor schema and component-generation semantics; incompatible migration holds for separate review. Component hashes/generation/epoch/API are pinned to that exact package build, never user paths or unprivileged JSON. App metadata Ed25519 and sandbox-maintenance Ed25519 remain distinct; package hooks do not repurpose the app signature as sandbox-capsule authority.

Use fixed native phases prepare-install, commit-install, prepare-remove, commit-remove, check; no arbitrary paths/commands. Bind journal identities to eagerly captured fstat device/inode/hash, not lazy pathname metadata. Apply the AppImage design's bounded parsing, safe ancestors/ACL/link/type checks, old-FD revocation and forward-only crash principles to this separate native backend, without altering Task 1's capsule parser.

1. Before native payload replacement/removal, acquire the fixed root transaction lock; validate/recover prior native intent/floor, reject unknown state, and hold startup admission. Confirm live native application processes are drained under this admission boundary; fail before replacement rather than kill processes. This requires an actual race proof; a one-time process-name scan is insufficient.
2. Persist/fsync the prepared intent and maximum accepted generation/time before changing privilege. Reject lower generation, same-generation different component hashes, incompatible epoch and clock rollback. Pin the old helper inode and remove/fsync SUID/SGID/capabilities through that FD before the manager can unlink/replace it. Never restore old privilege after interruption.
3. The manager unpacks only non-setuid native helper bytes. Configuration validates complete owned payload, fixed generated identity and helper/component hashes, monotonic floor and transaction identity; only then promote/fsync the new helper to 4755, publish/fsync the active receipt and clear intent last. Launch stays held until both receipt and installed bytes agree. If script/configuration fails, preserve intent/floor and repair using the manager's configure/reinstall operation; do not launch or bootstrap a competing maintainer.
4. Final removal demotes the retained helper before unlink, then records a removed tombstone retaining generation/time/hashes. Removing/purging the package does not reset this security state. Reinstall honors it. No persistent native SUID copy/backup survives successful removal, and no old privileged inode survives successful upgrade.
5. DEB abort-upgrade/abort-remove and RPM old/new script ordering must not turn a failed/old hook into authority to restore privilege or demote the newly committed helper. ALPM must use an already installed PreTransaction AbortOnFail hook for upgrade/removal; .install alone is not the abort boundary. First install has no old helper to revoke. An intact effective required pre-hook must abort on failure before old bytes are removed. Deliberate root removal/disablement of ALPM hooks is outside the qualified precondition; no pre-unpack abort is claimed in that state, and missing/modified hooks refuse startup/configuration where those checks run. Prove real manager semantics, including simultaneous package operations, rather than relying on shell exit assumptions.

A manager-requested application downgrade is allowed only when its bundled helper tuple still meets the native floor and the runtime remains qualified for that helper epoch. If the manager's own rollback installs an older/lower-generation payload, it remains 0755 with a clear repair/startup hold; native recovery does not promise a working old helper or re-grant its privilege. Repair is an authentic manager reinstall/configuration satisfying the retained floor. This differs from AppImage application rollback, which keeps that domain's newer compatible helper. Never remove/reset a floor to make a package downgrade succeed.

The payload's helper mode 0755 versus configured mode 4755 is deliberate and recorded in provenance/verification policy; generic permission repair may safely demote it but must never grant privilege without validation. No generic chmod || true, setcap, root Electron, privileged network, sysctl, AppArmor profile, SELinux module/permissive setting or mount-policy change. Unsupported nosuid/security-policy hosts receive an actionable refusal under their existing policy.

## Versions and release assets

One semantic application version maps deterministically to all formats; native package revision/pkgrel is initially 1 and epoch 0. A packaging rebuild requires a fresh immutable application release, never overwriting same-tag assets.

| App version | DEB Version | RPM Version / Release | AUR pkgver / pkgrel |
|---|---|---|---|
| 0.1.0-beta.1 | 0.1.0~beta.1-1 | 0.1.0 / 0.beta.1.1 | 0.1.0beta1 / 1 |
| 0.1.0-beta.10 | 0.1.0~beta.10-1 | 0.1.0 / 0.beta.10.1 | 0.1.0beta10 / 1 |
| 0.1.0 | 0.1.0-1 | 0.1.0 / 1 | 0.1.0 / 1 |

Only the repository's supported stable/beta.N semver forms are accepted. Test beta.1 < beta.2 < beta.10 < final < next patch with real dpkg --compare-versions, RPM label comparison and vercmp. Keep full upstream semver separately; never reconstruct a GitHub URL from a normalized package version. Architecture mappings are x64 -> amd64 (DEB), x86_64 (RPM/Arch).

Add exact assets dilla-VERSION-linux-x64.deb, dilla-VERSION-linux-x64.rpm and dilla-VERSION-linux-x64.aur.tar.gz alongside the seven existing assets and the three planned sandbox component assets (13 final assets total). The AUR archive contains only PKGBUILD, .SRCINFO and fixed lifecycle/hook inputs; source URL is the immutable matching DEB release asset and its actual SHA256, no latest/mutable channel URL, SKIP, dynamic checksum fetch or source compilation. AUR dilla-bin is stable-track by default; before the first stable release its recipe may pin the current explicitly beta-labeled upstream release using the mapping above. No beta silently replaces a published stable recipe.

Preserve four updater artifacts (Linux updater target remains AppImage); DEB/RPM/AUR are signed release.assets entries, not duplicate linux-x64 updater targets. Extend buildArtifactNames, native package provenance, offline signer, strict verify-release/publisher asset inventories and workflow mutation tests together. Bind each format's bytes, payload manifest, package version, helper generation/epoch/hash, custom scripts, ownership modes, source commit/tree/run and native acceptance digest. AUR proof binds accepted recipe hash, DEB source hash and the locally built pacman package/payload used in tests; do not claim a user's later makepkg output is bit-identical.

Offline release signing continues to add only detached metadata/component/feed signatures after acceptance; no DEB/RPM rebuild or header mutation after byte acceptance. Initial standalone RPMs do not claim an embedded OpenPGP signature: DNF documents localpkg_gpgcheck separately with default false, but actual supported-host policy must be tested unchanged. Hosts requiring local package signatures must reject; never --nogpgcheck, localpkg_gpgcheck overrides, native trust-key injection or disabling RPM verification. Native RPM signing, if later required for wider support, needs a separate offline OpenPGP key and qualification of the final signed RPM bytes before publication; Ed25519 metadata is not an RPM/GPG signature. No such key/service is silently introduced here.

## Acceptance and release boundary

Use real disposable x64 desktop VMs, not chroot/container substitutes for kernel, AppArmor/SELinux, sandbox or credential assertions: Ubuntu 24.04 default restricted policy, Debian 13, Fedora 44 enforcing, and a dated/pinned Arch image. Dependency declarations come from the actual final ELF/runtime set and are proved on clean targets; native packages must launch without FUSE. No workstation root operations.

Required per-format install -> sandboxed launch -> real account/keyring/draft/attachment persistence -> explicit manager upgrade -> relaunch -> remove/purge -> reinstall proof; old/new payload hashes and live old-helper FD execution controls; lower-generation/same-generation conflict; interrupted prepare/unpack/configure/remove and manager repair; busy startup/install race; native/AppImage install-order coexistence and cross-removal isolation; stable/beta ordering and profile preservation. Verify unchanged AppArmor/SELinux/sysctl/mount policy and ordinary unprivileged Electron. Strict local-RPM-signature policy must refuse unchanged bytes without modifying helper/profile state.

Package-owned app updater tests must prove no AppImageUpdater, pkexec, watchdog, capsule bootstrap, activation-pointer or system-file write even with spoofed APPIMAGE/argv/marker. Existing four-platform client/voice/account/performance/installed-update/provenance gates remain mandatory. Add required package VM jobs/proof records to the same accepted release run; missing/partial/diagnostic/native-skip proof cannot publish. Generate the AUR recipe without credentials; actual AUR submission remains a separate authorized release action.

## Primary evidence

- [Debian maintainer-script lifecycle](https://www.debian.org/doc/debian-policy/ch-maintainerscripts.html): fresh/upgrade/abort/remove entry distinctions must be tested.
- [RPM scriptlet ordering](https://rpm.org/docs/latest/manual/file_triggers.html): upgrade includes old/new hook invocations; pre-hook failure behavior differs from post-hook failure.
- [ALPM transaction hooks](https://man.archlinux.org/man/alpm-hooks.5): PreTransaction AbortOnFail is the explicit abort contract; post hooks may not run on failed transactions.
- [Arch package fields/version constraints](https://wiki.archlinux.org/title/PKGBUILD) and [ALPM version comparison](https://man.archlinux.org/man/alpm-pkgver.7.en): recipe mapping and native vercmp tests.
- [DNF5 local package verification](https://dnf5.readthedocs.io/en/stable/dnf5.conf.5.html): localpkg_gpgcheck applies to directly supplied RPM files independently of repository checking.
- [RPM signing](https://rpm.org/docs/6.1.x/man/rpmsign.1): adding an OpenPGP signature modifies the package, so it cannot be represented as detached metadata-only signing.
