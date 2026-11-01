# Dilla desktop validation builds

The desktop application is under development. **Keep using the existing browser client.**
The first supported desktop release must ship Linux x64, Windows x64, and macOS Intel
and Apple Silicon together. No desktop-only server release is ready.

The development shell in `packages/desktop` bundles the existing React client, core
worker and media implementation. It serves those installed bytes locally at the
selected instance's origin. Only API, invite and signalling requests reach the
instance. Each complete canonical instance URL gets a separate persistent Chromium
partition and one window. API and gateway requests include the selected base path.
An instance cannot replace the installed JavaScript or set the desktop update URL.

Desktop storage is a new device. Browser keys and storage are never copied or wiped.
Use the existing recovery/enrolment flow when testing a desktop device, and keep a
working browser device and your recovery key. A reload must preserve IndexedDB and
OPFS; real enrolment/recovery acceptance is still required on every platform.

## Build and run from source

Use the pinned Node and Rust versions in the repository, and the existing wasm build
instructions in `CONTRIBUTING.md`. Then, from the repository root:

```sh
npm ci
node node_modules/electron/install.js
npm run desktop:build
npm run start -w @dilla/desktop
```

The instance chooser asks for an HTTPS URL. A native confirmation names an unfamiliar
host before adding it. A command-line development instance can be selected with:

```sh
npm run start -w @dilla/desktop -- --instance=https://chat.example.com/
```

Only unpackaged builds accept `--profile=/absolute/test/profile` and
`--allow-loopback`; the latter permits HTTP on `127.0.0.1`, `::1` or `localhost` for
local test hosts. It never permits remote HTTP instances. Packaged builds ignore
these development switches.

Closing an instance window hides it in the tray. Use the tray to reopen it, change
the optional login startup preference, check for updates, or quit. Autostart is
available only in installed builds. Notification clicks open the existing client
destination. Native Answer/Decline actions and background ringing have Linux
acceptance coverage; their remaining platform checks still gate release.
`dilla://invite/chat.example.com/CODE` supports validated HTTPS host/port invites;
path-scoped invite URLs do not yet have a deep-link representation.

## Acceptance and updates

Application audio and the system-browser sign-in handoff are implemented. Linux
selected-application capture passes its encrypted delivery and exclusion checks.
Windows hosted CI provisions a signed virtual USB audio device. Native capture
gates have passed on Linux x64, Windows x64, macOS Intel and Apple Silicon, including
selected-application delivery, own-call and unrelated-application exclusion, and
fail-closed capture errors. Full duplex-voice, shared-client and performance gates
remain open. A failed capture must remain a visible
error rather than substitute whole-system or unencrypted audio. Password,
second-factor, passkey and recovery flows retain the existing worker and device
model. Real passkey browser handoff and recovery have local acceptance coverage;
the complete platform acceptance matrix remains open.

Windows/Linux update transport authenticates a detached Ed25519 signature over the
exact release JSON before consuming its artifact hashes. Keys are compiled into
`packages/desktop/src/update-keys.ts` from the provisioned public release trust
root. Private signing keys remain separate from CI and publishing credentials. Checks fail closed with a visible,
retryable error. A publishing token is not a signing key. The fixed GitHub feeds use
separate `desktop-stable` and `desktop-beta` release tags; the installed version fixes
the channel. Metadata must expire within 93 days and list versioned GitHub artifacts.
There is no instance-supplied feed, downgrade path, or forced installation.

Verified downloads use `electron-updater`. Installation runs only after an actual
quit/restart request and fresh acknowledgments from every renderer. Calls,
recovering calls, drafts, open editors and pending attachments block installation.
Reload is guarded too. AppImage replacement stages and verifies on the destination
filesystem, preserves the installed filename, and retains a `.previous` backup. A
previous-version Electron helper waits for the old app to exit, then requires the
exact new main process, signed version and private startup nonce to report usable
local startup and survive a grace interval. Failed startup stops only the owned
new process before restoring the working program. Windows waits for its per-user
NSIS installer to finish within five minutes. It stages only a new immutable version
tree behind a stable native `Dilla.exe`, then launches and validates the exact new
executable. The signed program manifest covers the executable, ASAR and runtime
files. Failure retains the old committed slot and working tree; no update removes
the normal launcher. Owned HKCU version/size values change only after commitment.
The installer starts suspended inside an owned Windows job, resumes only after its
actual process identifiers are recorded, and is stopped before recovery if its
watchdog exits or it times out. It has no elevation fallback. Device data stays
outside the program trees.

Preparation happens after renderer unload acceptance. It retains an old AppImage
copy on Linux. Windows currently prepares two independent old program copies for
its temporary helper and recovery verification, while retaining the old installed
version. It therefore needs roughly twice the old program size in preparation
space, plus the downloaded installer and new inactive version. Only the fixed
signed inactive target can be quarantined for an explicit retry; it is never
substituted for the selected old program. The temporary helper also runs an Electron main
process. Updated windows stay hidden and disabled, with user actions blocked,
until a private main-process handshake confirms durable completion. Account startup
also waits for that handshake. The Windows complete journal is flushed before its
new slot is published; a published version cannot trigger a later rollback. A
visible journal rename alone does not activate the app. Before the old parent
exits, its helper must acknowledge readiness and an authenticated arm sent only
after the active pointer flush succeeds. Private phase and failure records support interrupted recovery and an
explicit retry; the retained failure status disables automatic checks until an
explicit retry, so ordinary quits cannot silently repeat it. Backups are retained for recovery.

Local health covers the bundled UI, worker/WASM initialization and a disposable
OPFS persistence probe. It does not open an account's SQLite store or KEK, and
remote discovery, authentication and network failures do not trigger rollback.
Installed device enrolment, recovery, stored-account and draft acceptance remain
separate gates. File contents are flushed on Windows, but Node cannot reliably
flush directory entries there; this is not a complete power-loss guarantee.
The native installed-version fault and restart matrix remains a release gate.
The [Windows stable launcher and version-directory implementation](../desktop/WINDOWS-UPDATE-BOOTSTRAP.md)
remains under independent review and needs actual native Windows interruption,
slot, retry and installer-maintenance acceptance. Local Linux acceptance uses
AppImage extract-and-run mode; ordinary FUSE-mounted acceptance remains a CI gate.

macOS uses a narrow Sparkle bridge with signed update feeds and archives. Actual
unsigned-to-unsigned installed upgrades passed on Intel and Apple Silicon using
ad-hoc signing and normal OS protections. The broader installed fault matrix and
complete release gates still apply.

Preview packaging is available with `npm run pack -w @dilla/desktop`; it creates an
unpacked app for validation and never publishes. Installer targets are configured
for AppImage, per-user NSIS, and macOS DMG/ZIP. The publication workflow refuses
missing platform acceptance, measured performance, an independent security review,
or unauthenticated artifacts; no desktop release has been published.

Each native job records its actual clean Git commit/tree, workflow run and final
installer hashes after fuse verification. Its `build-provenance-<target>.json`
travels in the same installer artifact. `acceptance.json` supplies exact
`builds` and `installedAccounts` maps for `linux-x64`, `win-x64`, `mac-x64` and
`mac-arm64`; every entry contains the SHA-256 of that target's canonical JSON
report. The offline signer authenticates those digests. Publication independently
downloads the accepted run's build records, compares them byte for byte with the
signing candidate, and never replaces them with candidate-supplied provenance.
A previous green run cannot stand in for a different source tree. Every target
also requires one successful installed-account proof across its three client
shards, including real device/KEK/history recovery and separate draft/file guards.
 Commercial OS signing/notarization is not configured; macOS packaging uses
ad-hoc signing. Windows and macOS may warn about an unidentified publisher. Use the
OS's normal installation approval flow; do not disable SmartScreen, Gatekeeper, SIP
or quarantine protections to make a test pass.

See [platform findings and launch gates](../desktop/FOUNDATIONS.md) for the remaining
work. Packaging success does not satisfy those gates.

The workspace uses the integrated Signal header and desktop window controls.
Startup and sign-in use a compact card; the main workspace has a minimum native
size of 900 by 600 pixels and does not switch to the phone layout. Signal is the
only supported palette, with the theming infrastructure and stored preferences
preserved.
