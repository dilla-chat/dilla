# dilla, unreleased: the Capacitor mobile app is gone

A draft for the next server release. Rename it to the release's tag when the tag is cut.

The Capacitor mobile app is removed from the repository. Native iPhone and Android apps are planned, with no date yet.
The web client still works in a phone's browser, with its own phone layout.

## Before you upgrade

**Before upgrading, delete the `mobile_origins` line under `[http]` in dilla.toml, or dillad refuses to start (exit 78).**

The `http.mobile_origins` setting no longer exists. A `dilla.toml` that still has it fails to load, so the server does
not start. Delete that line, then upgrade `dillad` as usual.

## What changes

- **No Android APK.** The server release no longer carries an Android APK, and its `SHA256SUMS` does not list one.
- **The TestFlight preview ends.** The iPhone preview on TestFlight is no longer updated. Use the web client in your
  phone's browser until the native apps come.
- **No cross-origin calls from the app.** The server no longer allows the app's origins (`capacitor://localhost` and
  `https://localhost`) to call `/v1/` and `/i/` with a bearer token. The web client is served from the same origin and
  is not affected.
