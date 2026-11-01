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
Windows hosted CI provisions a signed virtual USB audio device; its actual capture
acceptance and macOS unrelated-application exclusion still need passing native
gates. A failed capture must remain a visible
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
failure to spawn restores that backup. Runtime health after process startup remains
an installed acceptance gate. Windows uses the per-user NSIS installer without
an elevation fallback. Installed-version fault and restart tests remain required.

macOS uses a narrow Sparkle bridge with signed update feeds and archives. Actual
unsigned-to-unsigned installed upgrades passed on Intel and Apple Silicon using
ad-hoc signing and normal OS protections. The broader installed fault matrix and
complete release gates still apply.

Preview packaging is available with `npm run pack -w @dilla/desktop`; it creates an
unpacked app for validation and never publishes. Installer targets are configured
for AppImage, per-user NSIS, and macOS DMG/ZIP. The publication workflow refuses
missing platform acceptance, measured performance, an independent security review,
or unauthenticated artifacts; no desktop release has been published. Commercial OS signing/notarization is not configured; macOS packaging uses
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
