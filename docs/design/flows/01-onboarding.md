# Flow 01 — first launch with an invite (web-1)

The strings, the refusal table and the flow rules of this document are L-COPY-01 of the web-1 plan, quoted. A change is made there first. One string is L-COPY-02 of the web-2a plan: `onboarding.connect.signIn`. Wave B task 3b (2026-10-30, Jonas: "match the prototype") rebuilt the flow to the Signal prototype's six steps and copy; the sections below describe that flow. Signing in to an account that already exists is `03-sign-in.md`.

The first-run flow of the web client: a person opens an invite link in a browser, picks a username and a
password, saves the recovery key, adds a second factor, saves the backup codes and ends up signed in. Wireframed before it is built
(F16). Built by web-1 task 20 (the `@dilla/ui` parts) and task 23 (the screen
`packages/web/src/screens/Onboarding.tsx` and its strings in `packages/web/src/strings/en.ts`). The
recovery-key step has its own document, `02-recovery-key.md`.

Every user-visible string is a key of `packages/web/src/strings/en.ts`; the "Copy" table at the end is
the exact English text. `{instance}` is `AccountState.instance.name` (the instance's domain, for example
`dilla.thim.dev`), `{username}` is the username as the server stored it, `{n}` the step number,
`{seconds}` a whole number of seconds, `{code}` an error code such as `E_CORE_STATE`.

The wireframes write each string as the Copy table has it. On screen the field labels, the grid label and
the step line are drawn in capitals by CSS (`.d-label` and the field-label rule of `TextField`); the text
itself, and what a screen reader reads, stays as written.

## Entry

| `account.phase` | the page shows |
|---|---|
| `needs-signup`, `instance.registrationMode` 0 or 1 | step 1 |
| `needs-signup`, `instance.registrationMode` 2 | the closed frame and nothing else |
| `signup-keys` | step 3; `account.recoveryKey` holds the 13 groups |
| `registering` | step 3 in the registering state |
| `factor-setup`, origin `signup` | steps 4 and 5, the second factor (flow 10) |
| `ready`, reached by a sign-up in this page | step 6 |
| `ready` on a fresh load | not this flow: the shell |
| `signin-login`, `signin-totp`, `signin-key`, `enrolling` | not this flow: `03-sign-in.md` |
| any other phase | not this flow: the boot screen |

The page is reached at `/welcome` or `/welcome?invite=<code>`. A code, or a link containing one, in
`?invite=` fills the invite field. The page never writes the invite back into the URL, the history or a log.

Worker commands. Step 1 is local to the page. Step 2's `Create account` sends `signupBegin`, which makes the keys:
the phase becomes `signup-keys` and the key appears in step 3. If keys of an earlier attempt are still pending, it
sends `signupReset` first, so every `Create account` makes a new key. Step 3's `I saved it`, enabled only once its box
is ticked, sends `signupSubmit` with the invite of step 1, the username and password of step 2, an empty display name
and `recoveryKeyAcknowledged: true`. **The account is created on the instance only by this submit**, after the key
was shown and confirmed. `Back` on step 3, a refused `signupSubmit` (other than a lost connection) and the sign-in
link on step 3 send `signupReset`: the shown key is discarded and never shown again. The core makes a fresh random key
at every `signupBegin` and refuses a second one while keys are pending, so a key is never reused across attempts.

After `signupSubmit` resolves, the account owes its second factor: the phase is `factor-setup` with origin `signup`
and steps 4 and 5 show (`FactorSetup`, `factor.*` copy, step lines 4 and 5). Step 4 is the choice. Step 5 is the
authenticator app's code (the prototype's `Enter your code` / `Verify`, with the QR code, the setup key and the hint
kept: an accepted deviation) or the passkey. There is no skip, no cancel and no Back to step 3: the account exists. Step
5's Back returns to the choice. When the factor is in place, the page sends `factorSetupDone` with the invite of step 1.
The worker reads the factors again, makes the browser ready and joins the invite's server. Only then, with the phase
`ready` and the join answered, step 6 shows, holding the backup codes the factor's add returned (in page memory only).
`Open dilla` is blocked until their box is ticked. It calls `onFinish(result)`, and the page navigates, replacing the
history entry: to `/c/<communityId>` when `result.communityId` is set; to `/welcome?invite=<the invite>` when
`result.joinError` is set (the shell then opens the join dialog prefilled); otherwise to `/`.

The display name is not asked for. The instance stores none, and the username is shown until the person sets one in
Settings → Profile.

## Steps

| n | name | heading (`<h1>`) | parts |
|---|---|---|---|
| 1 | invite | `onboarding.connect.title` | `OnboardingFrame`, `TextField` invite link (placeholder `https://`), `Button` Continue (`onboarding.next`), and under it `Already have an account? Sign in` (`onboarding.connect.signIn`, only when `instance.passwordSignup`; it sends `signInBegin`, `03-sign-in.md`) |
| 2 | account | `onboarding.identity.title` | `TextField` username, password (only when `instance.passwordSignup`; placeholder `at least 12 characters`), `Button` Create account (`signupBegin`), then Back and the sign-in link |
| 3 | recovery key | `onboarding.keys.title` | one paragraph, `RecoveryKey` (see `02-recovery-key.md`), `Button` I saved it (`signupSubmit`, blocked until the tick), then Back and the sign-in link |
| 4 | second factor | flow 10 (`factor.choose.title`) | the choice, `Button` Continue; there is no skip |
| 5 | code | flow 10 (`factor.totp.titleSignup`, or `factor.passkey.title`) | the QR code, the setup key, the code boxes and their hint, `Button` Verify, then Back (to the choice) |
| 6 | done | `onboarding.done.title` | one paragraph, the prototype's panel (`onboarding.done.ready`, when the invite joined a server with a text channel), the backup codes (`CodeList`, `onboarding.done.codesBody`, check box `onboarding.done.codesAck`; accepted deviation), `Button` Open dilla (blocked until the tick) |

Six steps, each one `OnboardingFrame` whose step line is `onboarding.step` and whose dots follow the prototype. Each of
the first three is one `<form noValidate>`: `Enter` in a field submits the step, as does its forward button
(`type="submit"`). `Back` and the sign-in link are `type="button"` in the row under the forward button and never
submit. Steps 4 and 5 are flow 10's, in the same frame, with their own announcer.

## Wireframes at 1280 × 800

The wireframes below predate wave B task 3b and the Signal redesign: they show the old step order and copy. The
layout reference is the prototype (`docs/design/signal/prototype/`, the new-account steps); the Steps table above and
the Copy table below are current.

The frame is the top bar with the brand mark (`--topbar-h`, `--bg-2`) and a centred column of at most
40rem.

Step 1, connect (registration mode 0; in mode 1 the paragraph is `onboarding.connect.bodyOpen` and the
label `Invite (optional)`):

```
+----------------------------------------------------------------------------------------------+
| [D] DILLA_                                                                                   |
+----------------------------------------------------------------------------------------------+
|                                                                                              |
|                      Join dilla.thim.dev                                  <h1>, has focus    |
|                      Step 1 of 6                                                             |
|                                                                                              |
|                      You need an invite from someone on dilla.thim.dev. Paste it below.      |
|                                                                                              |
|                      Invite                                                                  |
|                      +--------------------------------------------------+                    |
|                      | k7qm3zrdw0pahv5cj8zem4tbs6                       |                    |
|                      +--------------------------------------------------+                    |
|                      A code or a link, as you received it.                                   |
|                      [ Use an existing account ]                          (ghost button)     |
|                      --------------------------------------------------                      |
|                                                            [ Continue ]                      |
+----------------------------------------------------------------------------------------------+
```

Step 2, identity (the password field only when the instance offers password sign-in):

```
+----------------------------------------------------------------------------------------------+
| [D] DILLA_                                                                                   |
+----------------------------------------------------------------------------------------------+
|                      Choose your name                                     <h1>, has focus    |
|                      Step 2 of 6                                                             |
|                      People on dilla.thim.dev see these next to your messages.               |
|                                                                                              |
|                      Username                                                                |
|                      +--------------------------------------------------+                    |
|                      | ada                                              |                    |
|                      +--------------------------------------------------+                    |
|                      3 to 32 characters: a–z, 0–9, dot, underscore or hyphen.                |
|                      Display name (optional)                                                 |
|                      +--------------------------------------------------+                    |
|                      | Ada                                              |                    |
|                      +--------------------------------------------------+                    |
|                      Up to 64 characters. Your username is shown when this is empty.         |
|                      Password                                                                |
|                      +--------------------------------------------------+                    |
|                      | ••••••••••                                       |                    |
|                      +--------------------------------------------------+                    |
|                      At least 8 characters.                                                  |
|                      --------------------------------------------------                      |
|                                               [ Back ]    [ Continue ]                       |
+----------------------------------------------------------------------------------------------+
```

Step 3, recovery key (the grid in detail: `02-recovery-key.md`):

```
+----------------------------------------------------------------------------------------------+
| [D] DILLA_                                                                                   |
+----------------------------------------------------------------------------------------------+
|                      Your recovery key                                    <h1>, has focus    |
|                      Step 3 of 6                                                             |
|                      This key is shown once and is kept nowhere. Write it down or print      |
|                      it, and keep it away from this computer.                                |
|                      This key is the only way to get your account back or to add another     |
|                      browser. If every browser you use loses its data and you do not have    |
|                      the key, the account and its history are gone, and the host cannot      |
|                      bring them back.                                                        |
|                      Recovery key                                                            |
|                      +--------------------------------------------------+                    |
|                      | 01 7K3M    02 QW9D    03 X2RT    04 0PNA         |                    |
|                      | 05 HV5C    06 J8ZE    07 M4TB    08 S6YF         |                    |
|                      | 09 1GKD    10 R3WP    11 ZN7H    12 C9QX         |                    |
|                      | 13 5TVA                                          |                    |
|                      +--------------------------------------------------+                    |
|                      [ ] I have written down or printed my recovery key       [ Print ]      |
|                      --------------------------------------------------                      |
|                                               [ Back ]    [ Continue ] (blocked)             |
|                                                           Tick the box to continue.          |
+----------------------------------------------------------------------------------------------+
```

While `account.recoveryKey` is still `null` (the moment after `signupBegin` was sent), the grid's place
holds the status line `Making your keys` (`role="status"`) and no key.

Step 4, this browser:

```
+----------------------------------------------------------------------------------------------+
| [D] DILLA_                                                                                   |
+----------------------------------------------------------------------------------------------+
|                      What this browser keeps                              <h1>, has focus    |
|                      Step 4 of 6                                                             |
|                      This browser now holds the key of this device, in its storage for       |
|                      dilla.thim.dev. The key never leaves this browser.                      |
|                      Clearing this site’s data removes the key and the messages kept         |
|                      here, and this browser stops being your device.                         |
|                      To use this account in another browser, sign in there with your         |
|                      password and the second factor you add next, then this recovery key.    |
|                      Messages sent before that browser joins are not shown in it.            |
|                      A private window forgets all of this when it closes.                    |
|                      --------------------------------------------------                      |
|                                          [ Back ]    [ Create account ]                      |
+----------------------------------------------------------------------------------------------+
```

Step 5, second factor (flow 10 draws it in full; this is its place in the sequence):

```
+----------------------------------------------------------------------------------------------+
| [D] DILLA_                                                                                   |
+----------------------------------------------------------------------------------------------+
|                      Add a second factor                                  <h1>, has focus    |
|                      Step 5 of 6                                                             |
|                      Every account on dilla.thim.dev needs a second factor. ...              |
|                      ( ) authenticator app    ( ) passkey                                    |
|                      --------------------------------------------------                      |
|                                                            [ Continue ]    (no skip)         |
|                                          [ forget this browser ]   (the only way out)        |
+----------------------------------------------------------------------------------------------+
```

Step 6, done:

```
+----------------------------------------------------------------------------------------------+
| [D] DILLA_                                                                                   |
+----------------------------------------------------------------------------------------------+
|                      You’re in                                            <h1>, has focus    |
|                      Step 6 of 6                                                             |
|                      You are ada on dilla.thim.dev.                                          |
|                      --------------------------------------------------                      |
|                                                            [ Open dilla ]                    |
+----------------------------------------------------------------------------------------------+
```

## Wireframes at 360 × 740

The column takes the full width less 1.5rem on each side; nothing scrolls sideways (WCAG 1.4.10). The
footer buttons wrap to a second line when they do not fit, forward button last. On steps 1 to 3 the link
`Already have an account? Sign in` sits at the right of the row under the forward button (wave B task 3b), when the instance offers it.

```
+--------------------------------------+   +--------------------------------------+
| [D] DILLA_                           |   | [D] DILLA_                           |
+--------------------------------------+   +--------------------------------------+
|  Join dilla.thim.dev                 |   |  Choose your name                    |
|  Step 1 of 6                         |   |  Step 2 of 6                         |
|  You need an invite from someone     |   |  People on dilla.thim.dev see these  |
|  on dilla.thim.dev. Paste it below.  |   |  next to your messages.              |
|  Invite                              |   |  Username                            |
|  +--------------------------------+  |   |  +--------------------------------+  |
|  | k7qm3zrdw0pahv5cj8zem4tbs6     |  |   |  | ada                            |  |
|  +--------------------------------+  |   |  +--------------------------------+  |
|  A code or a link, as you received   |   |  3 to 32 characters: a–z, 0–9,       |
|  it.                                 |   |  dot, underscore or hyphen.          |
|  ------------------------------      |   |  Display name (optional)  (+ hint)   |
|                     [ Continue ]     |   |  Password                 (+ hint)   |
+--------------------------------------+   |  ------------------------------      |
                                           |          [ Back ]  [ Continue ]      |
                                           +--------------------------------------+

+--------------------------------------+   +--------------------------------------+
| [D] DILLA_                           |   | [D] DILLA_                           |
+--------------------------------------+   +--------------------------------------+
|  Your recovery key                   |   |  What this browser keeps             |
|  Step 3 of 6                         |   |  Step 4 of 6                         |
|  This key is shown once and is kept  |   |  This browser now holds the key of   |
|  nowhere. …                          |   |  this device, …                      |
|  This key is the only way to get     |   |  Clearing this site’s data …         |
|  your account back or to add …       |   |  To use this account in another …    |
|  Recovery key                        |   |  A private window forgets all of     |
|  +--------------------------------+  |   |  this when it closes.                |
|  |  01 7K3M        02 QW9D        |  |   |  ------------------------------      |
|  |  03 X2RT        04 0PNA        |  |   |  [ Back ]                            |
|  |  05 HV5C        06 J8ZE        |  |   |             [ Create account ]       |
|  |  07 M4TB        08 S6YF        |  |   +--------------------------------------+
|  |  09 1GKD        10 R3WP        |  |
|  |  11 ZN7H        12 C9QX        |  |   +--------------------------------------+
|  |  13 5TVA                       |  |   | [D] DILLA_                           |
|  +--------------------------------+  |   +--------------------------------------+
|  [ ] I have written down or          |   |  You’re in                           |
|      printed my recovery key         |   |  Step 6 of 6                         |
|  [ Print ]                           |   |  You are ada on dilla.thim.dev.      |
|  ------------------------------      |   |  ------------------------------      |
|          [ Back ]  [ Continue ]      |   |                    [ Open dilla ]    |
|  Tick the box to continue.           |   +--------------------------------------+
+--------------------------------------+
```

Step 5 at 360 × 740 (flow 10 draws it in full; the ChoiceCards stack and the exit stands under the frame):

```
+--------------------------------------+
| [D] DILLA_                           |
+--------------------------------------+
|  Add a second factor                 |
|  Step 5 of 6                         |
|  Every account on dilla.thim.dev     |
|  needs a second factor. ...          |
|  ( ) authenticator app               |
|  ( ) passkey                         |
|  ------------------------------      |
|                     [ Continue ]     |
|        [ forget this browser ]       |
+--------------------------------------+
```

## States

The closed frame (`instance.registrationMode` is 2, or a submit refused with `E_FORBIDDEN`): the heading
and one paragraph; no step line and no field. Its one control is the link `Already have an account? Sign in`, shown when
`instance.passwordSignup` is true, because a closed instance still has accounts that want a second
browser (`03-sign-in.md`). Focus moves to its heading.

```
+----------------------------------------------------------------------------------------------+
| [D] DILLA_                                                                                   |
+----------------------------------------------------------------------------------------------+
|                      dilla.thim.dev is not taking new accounts            <h1>, has focus    |
|                      Ask the host of dilla.thim.dev when sign-ups open again.                |
|                      [ Use an existing account ]                          (ghost button)     |
+----------------------------------------------------------------------------------------------+
```

Registration open (`registrationMode` 1): step 1 with the paragraph `onboarding.connect.bodyOpen` and the
field label `Invite (optional)`; an empty invite is allowed and sent as the empty string.

Invite missing (mode 0, the field empty on `Continue`). The error sits under the field, the field is
`aria-invalid`, focus moves to the field:

```
|                      Invite                                                                  |
|                      +--------------------------------------------------+  (danger border)   |
|                      |                                                  |                    |
|                      +--------------------------------------------------+                    |
|                      A code or a link, as you received it.                                   |
|                      ✕ Paste the invite you were given.                      role="alert"     |
```

Account checks (by the page on `Create account`, before any key is made): a username that is not 3 to 32 of
`a-z 0-9 . _ -` after lowering upper-case letters, or that starts or ends with a dot, shows
`onboarding.error.usernameRule` under the username field; a password shorter than 12 characters (code points) shows
`onboarding.error.passwordShort`. Every failing field shows its message at once, and focus moves to the first field
in error. The instance refuses a shorter password too (protocol/09, `POST /v1/accounts`).

Registering (`account.phase` is `registering`, or `signupSubmit` in flight): step 3 with its buttons and links
blocked (`aria-disabled="true"`, so they stay focusable and focus stays on the pressed button), the forward button
reading `Creating account…`, and the status line `onboarding.keys.registering` in the step's body (the old wireframe
below shows it on the removed step 4):

```
|                      A private window forgets all of this when it closes.                    |
|                      Creating your account on dilla.thim.dev…               (role="status")  |
|                      --------------------------------------------------                      |
|                                         [ Back ]  [ Creating account… ]                      |
|                                    aria-disabled   aria-disabled                             |
```

A refusal shown as a banner: the `Banner` is the first child of the step's body, under the step line,
inside `<main>`;
the buttons are enabled again; focus stays where it was and the banner announces itself. The network
banner, on step 3 (the old wireframe shows the removed step 4):

```
|                      What this browser keeps                                                 |
|                      Step 4 of 6                                                             |
|                      +------------------------------------------------------------------+    |
|                      | ✕ The connection dropped while creating your account. Reload     |    |
|                      |   the page to finish.                                 [ Reload ] |    |
|                      +------------------------------------------------------------------+    |
|                      This browser now holds the key of this device, …                        |
```

Refusals of `signupSubmit` (step 3's `I saved it`), switched on `code` and `status` only (never on `detail`), each
clearing the in-flight state. Except for a lost connection, nothing was created: the page sends `signupReset` (the
key of this attempt is discarded, the tick cleared) and goes back to the step named, where the next `Create account`
makes a new key:

| `code` | `status` | goes to | shown |
|---|---|---|---|
| `E_INVITE_INVALID` | any | step 1, focus on the invite field | field error `onboarding.error.inviteInvalid` |
| `E_INVALID_REQUEST` | 409 | step 2, focus on the username field | field error `onboarding.error.usernameTaken` |
| `E_INVALID_REQUEST` | any other | step 2, focus on the heading | danger banner `onboarding.error.detailsRefused` (the page already checks both fields with the server's rules, so this is the rare case) |
| `E_FORBIDDEN` | any | the closed frame | — |
| `E_RATE_LIMITED` | any | step 2, focus on the heading | warn banner: `onboarding.error.rateLimited` with `seconds = Math.ceil(retryAfterMs / 1000)` when `retryAfterMs` is not null, else `onboarding.error.rateLimitedNoWait` |
| `E_NETWORK`, or any code with `status` ≥ 500 | — | stays on step 3; the key is kept | danger banner `onboarding.error.network` with the action `onboarding.error.reload` → `browser.reload()`. The response may have been lost after the account was created; the reload runs `Signup.resume()` (L-TS-06). The page never sends `signupSubmit` again in this case: a second `POST /v1/accounts` would answer "username taken" for the person's own account |
| anything else | below 500 (0 for an error that is not an HTTP answer) | step 2, focus on the heading | danger banner `onboarding.error.other` with `{code}`, no action |

A refusal that returns to step 1 shows the typed invite still in the field; one that returns to step 2 keeps the
typed username and password; going forward again makes and shows a new key with its box unticked.

## Focus and announcements

| event | focus | announced |
|---|---|---|
| a step appears (the first render, forward, back, the closed and done frames) | that step's `<h1>` (`OnboardingFrame` focuses its heading whenever the title changes) | the heading, by the focus move |
| a submit that leaves one or more fields in error | the first field in error; every failing field shows its message at once | the error, by its `role="alert"` paragraph; the field's description is hint then error |
| a refusal that returns to step 1 or 2 for a field | that field | as above |
| a refusal that returns to step 2 with the `detailsRefused` banner | step 2's `<h1>` | the heading, then the banner (`role="alert"`) |
| a banner on the step that is shown | unchanged | the banner (`role="alert"`) |
| registering starts | unchanged (the pressed button becomes `aria-disabled` and keeps focus) | `onboarding.keys.registering` (`role="status"`) |
| the box of step 3 is ticked | the checkbox | nothing extra; `I saved it` becomes enabled and loses the description `onboarding.keys.ackHint` |
| the box of step 6 is ticked | the checkbox | nothing extra; `Open dilla` becomes enabled and loses the description `onboarding.done.codesHint` |
| step 4 appears | its `<h1>` (flow 10: the choice) | the heading, by the focus move |
| step 5 copies the setup key, or step 6 the backup codes | unchanged | `factor.totp.keyCopied` or `factor.codes.copied`, once, by the polite region of that step |
| step 5's code (or passkey) is in place but `factorSetupDone` is refused | unchanged (the pressed button keeps focus) | `role="alert"` banner: `factor.error.tooMany` for 429, `factor.error.network` for a dropped connection, else `factor.error.other` with `{code}` |
| `devices.forget` on steps 4 and 5 (the only way out) | Cancel, Escape and close return focus to it; the confirm opens on Cancel | the dialog; a refused wipe is a `role="alert"` in it and focus goes to Cancel |

Tab order inside each step: the fields in visual order, then (step 3) `Print`, then the checkbox, then the forward
button, then `Back` and `Already have an account? Sign in` (the prototype's row under the button), then (step 6) the
codes' Copy and Print, the checkbox and `Open dilla`. There are no single-key shortcuts.

## Error placement

| check | where | key |
|---|---|---|
| invite empty in mode 0 | under the invite field, step 1 | `onboarding.error.inviteRequired` |
| username rule | under the username field, step 2 | `onboarding.error.usernameRule` |
| password length | under the password field, step 2 | `onboarding.error.passwordShort` |
| a refusal of `signupSubmit` | as the refusal table under "States" says | — |

## Reload and interruption

What a page closed (or reloaded) at each step leaves, which is the same as before task 3b:

- Steps 1 and 2: nothing is stored. The typed values are gone, and the page opens at step 1 with the invite of the URL,
  if any.
- Step 3 before `I saved it` was pressed, or while registering when the server had not created the account: the
  worker resets the pending keys (`Signup.resume()` returns 0) and the page opens at step 1. **No account exists.** The
  key shown before was never used and is worthless; the next pass shows a new key. The page cannot tell this case from
  a first visit, so it says nothing about it.
- While registering when the server had created the account: the worker finishes the sign-up (`Signup.resume()`
  returns 2). The account owes its second factor, so the reopen lands on flow 10's forced screen (origin `required`).
- Steps 4 and 5: the account exists and owes its factor. The reopen lands on the forced screen. The setup key was never
  stored and is gone; nothing else is reachable until a factor is added.
- Step 6: the factor is in place. The reopen is the shell, and the backup codes shown before are gone (Settings →
  Sign-in makes new ones), as a reload on the old backup-codes step did.
- When the browser's key is refused for an account that was registered (the stored session proves it), the page shows the `revoked` splash and nothing is deleted.
- When the data saved in this browser was written by a newer version of dilla (a rollback; the core refuses it
  untouched with `E_STORE_NEWER`, and `account.error.store` holds the data version it found and the highest this
  version reads), the boot screen shows the `boot.storeNewer.*` splash with both numbers and one `Reload` button.
  Nothing is deleted and no reset is offered: the newer version can still open the data.

## Copy

| key | text | where |
|---|---|---|
| `onboarding.step` | `New account · step {n} of {total}` | step line, steps 1–6 ({total} is 6; Signal task 10 adds the dots) |
| `onboarding.back` | `Back` | link at the left of the row under the button, steps 2 and 3 |
| `onboarding.next` | `Continue` | button, step 1 |
| `onboarding.connect.title` | `Join with an invite` | step heading, step 1 |
| `onboarding.connect.body` | `Paste the link someone sent you.` | paragraph, step 1, registration mode 0 |
| `onboarding.connect.bodyOpen` | `{instance} is open to new accounts. Paste an invite if you were given one.` | paragraph, step 1, registration mode 1 |
| `onboarding.connect.invite` | `Invite link` | field label, step 1, mode 0 |
| `onboarding.connect.inviteOptional` | `Invite link (optional)` | field label, step 1, mode 1 |
| `onboarding.connect.invitePlaceholder` | `https://` | placeholder, invite field |
| `onboarding.connect.signIn` | `Already have an account? Sign in` | link at the right of the row under the button, steps 1 to 3 and the closed frame, only when `instance.passwordSignup` (L-COPY-02) |
| `onboarding.closed.title` | `{instance} is not taking new accounts` | step heading, closed frame |
| `onboarding.closed.body` | `Ask the host of {instance} when sign-ups open again.` | paragraph, closed frame |
| `onboarding.identity.title` | `Create your account` | step heading, step 2 |
| `onboarding.identity.body` | `Pick a username and a password. Your username is how people mention you.` | paragraph, step 2 |
| `onboarding.identity.username` | `Username` | field label, step 2 |
| `onboarding.identity.password` | `Password` | field label, step 2, only when `instance.passwordSignup` |
| `onboarding.identity.passwordPlaceholder` | `at least 12 characters` | placeholder, password field |
| `onboarding.identity.submit` | `Create account` | button, step 2 (makes the keys) |
| `onboarding.keys.title` | `Save your recovery key` | step heading, step 3 |
| `onboarding.keys.body` | `Shown once. You need it to add another browser.` | paragraph, step 3 |
| `onboarding.keys.label` | `Recovery key` | grid label, step 3 |
| `onboarding.keys.acknowledge` | `I saved my recovery key somewhere safe` | checkbox label, step 3 |
| `onboarding.keys.ackHint` | `Tick the box to continue.` | the blocked (`aria-disabled`) `I saved it`'s description (visually hidden; the prototype uses a toast) |
| `onboarding.keys.print` | `Print` | button, step 3 |
| `onboarding.keys.printTitle` | `dilla recovery key for {instance}. You need it to add another browser to your account.` | printed sheet only (`.d-onboarding-frame__print-only`): names what the sheet is (task 3b security review LOW-3) |
| `onboarding.keys.save` | `Save as file` | button beside Print, step 3: downloads a text file with the key (Jonas, task 3b) |
| `onboarding.keys.fileName` | `dilla-recovery-key-{instance}.txt` | the saved file's suggested name |
| `onboarding.keys.fileBody` | `Your dilla recovery key for {instance}. You need it to add another browser to your account. Keep this file away from this computer, and delete it here once it is stored elsewhere.` | the saved file's first line; the key follows once, its 13 groups joined by hyphens |
| `onboarding.keys.preparing` | `Making your keys` | status line, step 3, while the key is not there yet |
| `onboarding.keys.submit` | `I saved it` | button, step 3 (creates the account) |
| `onboarding.keys.submitting` | `Creating account…` | button, step 3 while registering |
| `onboarding.keys.registering` | `Creating your account on {instance}…` | status line, step 3, while registering |
| `onboarding.done.title` | `You’re in` | step heading, step 6 |
| `onboarding.done.body` | `Your account is ready.` | paragraph, step 6 |
| `onboarding.done.ready` | `{server} is waiting in #{channel}.` | the prototype's panel, step 6, when the invite joined a server with a text channel |
| `onboarding.done.codesBody` | `Each backup code signs you in once if you lose your second factor. Keep them with your recovery key.` | paragraph over the backup codes, step 6 |
| `onboarding.done.codesAck` | `I saved my backup codes somewhere safe` | checkbox label, step 6 |
| `onboarding.done.codesHint` | `Tick the box to continue.` | the blocked `Open dilla`'s description (visually hidden) |
| `onboarding.done.note` | `Messages sent before another browser joins are not shown there.` | small print (`.d-onboarding-frame__note`) under the panel, step 6 |
| `onboarding.done.next` | `Open dilla` | button, step 6 |
| `onboarding.error.inviteRequired` | `Paste the invite you were given.` | field error, invite, mode 0, empty on `Continue` |
| `onboarding.error.inviteInvalid` | `This invite is expired, used up or unknown. Ask for a new one.` | field error, invite, after `E_INVITE_INVALID` |
| `onboarding.error.usernameRule` | `Use 3 to 32 characters: a–z, 0–9, dot, underscore or hyphen, with no dot at the start or end.` | field error, username |
| `onboarding.error.usernameTaken` | `That username is taken. Try another.` | field error, username, after `E_INVALID_REQUEST` with status 409 |
| `onboarding.error.detailsRefused` | `{instance} did not accept these details. Check the username and the password.` | danger banner, step 2, after any other `E_INVALID_REQUEST` |
| `onboarding.error.passwordShort` | `Use at least 12 characters.` | field error, password |
| `onboarding.error.rateLimited` | `Too many attempts from this network. Try again in {seconds} s.` | warn banner, step 4, `retryAfterMs` known |
| `onboarding.error.rateLimitedNoWait` | `Too many attempts from this network. Wait a few minutes, then try again.` | warn banner, step 4, `retryAfterMs` null |
| `onboarding.error.network` | `The connection dropped while creating your account. Reload the page to finish.` | danger banner, step 4, `E_NETWORK` or status ≥ 500 |
| `onboarding.error.reload` | `Reload` | button in the network banner |
| `onboarding.error.other` | `The account could not be created ({code}). Try again.` | danger banner, step 2 after a refused `I saved it`, any other refusal |
| `boot.storeNewer.status` | `This version of dilla is too old for the data saved in this browser` | boot splash status, phase `error` with `E_STORE_NEWER` (v0.1.0-web3.6, C4x) |
| `boot.storeNewer.detail` | `That data needs data version {found}; this version of dilla reads up to {supported}. Update to the newer version of dilla, or ask the host of this server.` | boot splash detail; `{found}` and `{supported}` are `account.error.store` |
| `boot.storeNewer.action` | `Reload` | the splash's only button; it reloads the page |
