# dilla on iPhone and Android (preview)

Preview, 2026-11-06: the mobile app is a preview. You can install it on iPhone through TestFlight and on Android from
an APK attached to each GitHub release. There is no App Store or Play Store build. **Keep using your existing browser
or desktop device, and keep your recovery key.**

The app is a shell around the same client the browser and desktop app use. It bundles that client, so an instance
never sends the app its own code; the app only talks to the instance you choose, over HTTPS.

## Install

**iPhone.** Install the TestFlight app from the App Store, then open the TestFlight public link on your phone and tap
**Accept** and **Install**. The link is in the line below; it is the only place it is published.

TestFlight link: ask the instance owner (not published yet)

TestFlight installs new builds for you, or shows an update in the TestFlight app. A TestFlight build
expires after 90 days, so keep updating.

**Android.**

1. On your phone, open the [releases page](https://github.com/dilla-chat/dilla/releases), pick the newest release that
   lists a `dilla-v….apk` under Assets (server release tags start with `v`; desktop releases, tagged `desktop-v…`, have no APK)
   and download it.
2. Optional: download `dilla-<tag>.apk.sha256` too and compare its hash with the one of the APK
   (`sha256sum -c dilla-<tag>.apk.sha256` on a computer). The release's `SHA256SUMS` lists the same hash.
3. Open the APK. If Android asks, allow "install unknown apps" for the browser or files app you used, then go back
   and install.
4. Play Protect may warn about an unknown developer. The APK is not from the Play Store, so this is expected. Choose
   to install anyway if you trust the source.

Google has announced Android developer verification for sideloaded apps, first in a few countries from
September 2026. Where it applies, an app from an unverified developer may not install. The project will register as a
developer before it applies more widely.

To update, download the newer APK and install it over the old one. It keeps your data because it is signed with the
same key. The app has no updater of its own yet.

## What works

- **Choosing an instance.** On first launch the app asks for an instance address, such as `https://dilla.chat`.
  It checks that the instance answers before saving it, and says "Could not connect. Enter a complete HTTPS
  instance address, then try again." when it does not. You can add several instances; each one is a separate
  device of your account there.
- **Switching instance.** **You** → **Switch instance** goes back to the list. If an instance cannot be reached when
  the app starts, the error screen offers **Try again** and **Switch instance**.
- **Signing in and signing up.** Username, password and an authenticator code work inside the app. A passkey or your
  identity provider opens the system browser (a Safari view on iPhone, your default browser's tab on Android) and
  returns to the app.
- **Messages.** Channels, direct messages, threads, reactions, replies, attachments, search and settings, in the same
  phone layout as the browser (Home, Activity and You tabs).
- **Notifications while the app is open.** New messages and mentions show as system notifications while the app is
  running in the foreground, once you allow them in Settings → Notifications.

## What does not work yet

- **No push notifications.** Nothing arrives while the app is closed or in the background.
- **No calls in the background.** Voice calls are not part of this preview; calls that keep running with the screen
  locked come later.
- **No store builds.** The app comes from TestFlight (iPhone) and a downloaded APK (Android), not the App Store or
  Play Store. You can still build it yourself (see `packages/mobile/README.md`).
- **No app-icon badge count.**
- **Instances need a recent server.** The instance must be on a release that accepts the app (`http.mobile_origins`,
  on by default from the release that adds the app), reachable over HTTPS with a domain name, and not under a base
  path.

## Changing phones

The app's data is **not backed up**, on either platform: Android excludes it from backups, and on iPhone the app
excludes its data from iCloud and Finder backups. A restored or new phone starts with no device and no messages.

To use dilla on a new phone, install the app, add your instance again and sign in as on any new device: your password,
your second factor, then your **recovery key**. The new phone is a new device. It shows messages sent after it joined,
not the history your old phone held. Remove the old phone in Settings → Devices from any device you still have.

Uninstalling the app deletes its data in the same way, so keep your recovery key before you do.
