# Windows update packaging

The 2026-11-02 standard updater decision supersedes the custom bootstrap and
transactional recovery design previously documented here.

Windows uses the pinned electron-builder per-user NSIS installer and
`electron-updater` NsisUpdater. Electron is installed as `dilla-desktop.exe`,
with the Dilla icon, `dev.dilla.desktop` application ID and `dilla` protocol.
Installation does not require elevation and uninstall preserves application data.
The app checks signed release metadata and artifact hashes before allowing an update;
the fixed compiled stable/beta release URLs remain the source of update authority.

The stable Go launcher, immutable slots, Windows program manifest, rollback/registry
restoration, operation gate and custom NSIS script are removed. Interrupted
installation and failed-startup restoration are not Dilla guarantees.

CI retains the normal installed NSIS upgrade smoke, account/key continuity,
application/security tests, hardened Electron fuses and artifact/build provenance.
Final-source native acceptance is still required before release. No desktop release
or browser migration is authorized by packaging success alone.
