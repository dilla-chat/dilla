# dilla on iPhone and Android (preview)

Preview, 2026-11-05: the mobile app is under development. It is built from source and not distributed yet: there is
no App Store, TestFlight or Play Store build and no signed APK to download. **Keep using your existing browser or
desktop device, and keep your recovery key.**

The app is a shell around the same client the browser and desktop app use. It bundles that client, so an instance
never sends the app its own code; the app only talks to the instance you choose, over HTTPS.

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
- **No store builds.** You build and install the app yourself (see `packages/mobile/README.md`).
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
