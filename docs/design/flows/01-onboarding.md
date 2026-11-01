# Flow 01 — first launch with an invite (web-1)

The strings, the refusal table and the flow rules of this document are L-COPY-01 of the web-1 plan, quoted. A change is made there first. Three strings are L-COPY-02 of the web-2a plan: the new `onboarding.connect.signIn` and the changed `onboarding.keys.loss` and `onboarding.browser.oneBrowser`. Signing in to an account that already exists is `03-sign-in.md`.

The first-run flow of the web client: a person opens an invite link in a browser, chooses a name, writes
down the recovery key, reads what this browser keeps, and ends up signed in. Wireframed before it is built
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
| `signup-keys` | step 3, or step 4 when the person already moved on; `account.recoveryKey` holds the 13 groups |
| `registering` | step 4 in the registering state |
| `factor-setup`, origin `signup` | step 5, the second factor (flow 10); a reload there reopens it |
| `ready`, reached by a sign-up in this page | step 6 |
| `ready` on a fresh load | not this flow: the shell |
| `signin-login`, `signin-totp`, `signin-key`, `enrolling` | not this flow: `03-sign-in.md` |
| any other phase | not this flow: the boot screen |

The page is reached at `/welcome` or `/welcome?invite=<code>`. A code, or a link containing one, in
`?invite=` fills the invite field. The page never writes the invite back into the URL, the history or a log.

Worker commands. Steps 1 and 2 are local to the page. Going forward from step 2 sends `signupBegin`
once; the phase becomes `signup-keys` and the key appears in step 3. The `Create account` button of step 4
sends `signupSubmit` with the invite of step 1, the username, display name and password of step 2, and
`recoveryKeyAcknowledged: true`; the account is created only by this submit. A refused `signupSubmit`
leaves the phase at `signup-keys` with the same key, so going back and forward again shows the same key
with its box still ticked; a new key appears only after `signupReset`, which this flow never sends. After
`signupSubmit` resolves the account owes its second factor, so the phase is `factor-setup` with origin `signup`
and step 5 is shown (`FactorSetup`, `factor.*` copy, step line `onboarding.step` with n 5). Step 5 has no skip
and no cancel: its `Back` only returns to the choice between an authenticator app and a passkey. When the
factor is in place and the backup codes are acknowledged, the page sends `factorSetupDone` with the invite of
step 1; the worker reads the factors again, makes the browser ready and joins the invite's server, and only
then, with the phase `ready` and the join answered, step 6 is shown. Its button calls `onFinish(result)`, and
the page navigates, replacing the history entry: to `/c/<communityId>` when `result.communityId` is set; to
`/welcome?invite=<the invite>` when `result.joinError` is set (the shell then opens the join dialog
prefilled); otherwise to `/`.

## Steps

| n | name | heading (`<h1>`) | parts |
|---|---|---|---|
| 1 | connect | `onboarding.connect.title` | `OnboardingFrame`, `TextField` invite, ghost `Button` Use an existing account (`onboarding.connect.signIn`, only when `instance.passwordSignup`; it sends `signInBegin`, `03-sign-in.md`), `Button` Continue (`onboarding.next`) |
| 2 | identity | `onboarding.identity.title` | `TextField` username, display name, password (only when `instance.passwordSignup`), `Button` Back and Continue |
| 3 | recovery key | `onboarding.keys.title` | two paragraphs, `RecoveryKey` (see `02-recovery-key.md`), `Button` Back and Continue |
| 4 | this browser | `onboarding.browser.title` | four paragraphs, `Banner` on a refusal, `Button` Back and Create account |
| 5 | second factor | flow 10 (`factor.choose.title`) | the second-factor flow, see `10-second-factor.md`; there is no skip |
| 6 | done | `onboarding.done.title` | one paragraph, `Button` Open dilla (`onboarding.done.next`) |

Six steps, each one `OnboardingFrame` whose step line is `onboarding.step`. Each of the first four is one
`<form noValidate>`: `Enter` in a field submits the step, as does its forward button (`type="submit"`:
`Continue` on steps 1–3, `Create account` on step 4); `Back` is `type="button"`, comes first in the
footer and never submits. Step 5 is flow 10's, in the same frame, with its own announcer: the page speaks
`factor.totp.keyCopied` and `factor.codes.copied` through a polite region that exists only while step 5 is
shown. Step 6's only button is `Open dilla`.

## Wireframes at 1280 × 800

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
footer buttons wrap to a second line when they do not fit, forward button last. On step 1 the ghost
button `Use an existing account` stands on its own line under the invite hint, above the footer, when
the instance offers it.

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
and one paragraph; no step line and no field. Its one button is `Use an existing account`, shown when
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

Identity checks (by the page on `Continue`): a username that is not 3 to 32 of `a-z 0-9 . _ -` after
lowering upper-case letters, or that starts or ends with a dot, shows `onboarding.error.usernameRule` under
the username field; a display name over 64 characters shows `onboarding.error.displayLength`; a password
shorter than 8 characters shows `onboarding.error.passwordShort`. Every failing field shows its message
at once, and focus moves to the first field in error.

Registering (`account.phase` is `registering`, or `signupSubmit` in flight): step 4 with both buttons
blocked (`aria-disabled="true"`, so they stay focusable and focus stays on the pressed button), the
forward button reading `Creating account…`, and a status line in the step's body, after the fourth
paragraph and above the footer:

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
banner, on step 4:

```
|                      What this browser keeps                                                 |
|                      Step 4 of 6                                                             |
|                      +------------------------------------------------------------------+    |
|                      | ✕ The connection dropped while creating your account. Reload     |    |
|                      |   the page to finish.                                 [ Reload ] |    |
|                      +------------------------------------------------------------------+    |
|                      This browser now holds the key of this device, …                        |
```

Refusals of `signupSubmit`, switched on `code` and `status` only (never on `detail`), each clearing the
in-flight state:

| `code` | `status` | goes to | shown |
|---|---|---|---|
| `E_INVITE_INVALID` | any | connect, focus on the invite field | field error `onboarding.error.inviteInvalid` |
| `E_INVALID_REQUEST` | 409 | identity, focus on the username field | field error `onboarding.error.usernameTaken` |
| `E_INVALID_REQUEST` | any other | identity, focus on the heading | danger banner `onboarding.error.detailsRefused` (the page already validates both fields with the server's rules, so this is the rare case) |
| `E_FORBIDDEN` | any | the closed frame | — |
| `E_RATE_LIMITED` | any | stays | warn banner: `onboarding.error.rateLimited` with `seconds = Math.ceil(retryAfterMs / 1000)` when `retryAfterMs` is not null, else `onboarding.error.rateLimitedNoWait` |
| `E_NETWORK`, or any code with `status` ≥ 500 | — | stays | danger banner `onboarding.error.network` with the action `onboarding.error.reload` → `browser.reload()`. The response may have been lost after the account was created; the reload runs `Signup.resume()` (L-TS-06). The page never sends `signupSubmit` again in this case: a second `POST /v1/accounts` would answer "username taken" for the person's own account |
| anything else | below 500 (0 for an error that is not an HTTP answer) | stays | danger banner `onboarding.error.other` with `{code}`, no action (the server answered with a refusal, or the request was never sent, so nothing was created; `Create account` may be pressed again) |

A refusal that returns to step 1 shows the typed invite still in the field; one that returns to step 2
keeps the typed values; going forward again passes step 3 with the box still ticked and the same key.

## Focus and announcements

| event | focus | announced |
|---|---|---|
| a step appears (the first render, forward, back, the closed and done frames) | that step's `<h1>` (`OnboardingFrame` focuses its heading whenever the title changes) | the heading, by the focus move |
| a submit that leaves one or more fields in error | the first field in error; every failing field shows its message at once | the error, by its `role="alert"` paragraph; the field's description is hint then error |
| a refusal that returns to step 1 or 2 for a field | that field | as above |
| a refusal that returns to step 2 with the `detailsRefused` banner | step 2's `<h1>` | the heading, then the banner (`role="alert"`) |
| a banner on the step that is shown | unchanged | the banner (`role="alert"`) |
| registering starts | unchanged (the pressed button becomes `aria-disabled` and keeps focus) | `onboarding.browser.registering` (`role="status"`) |
| the box of step 3 is ticked | the checkbox | nothing extra; `Continue` becomes enabled and loses the description `onboarding.keys.ackHint` |
| step 5 appears | its `<h1>` (flow 10: the choice) | the heading, by the focus move |
| step 5 copies the setup key or the backup codes | unchanged | `factor.totp.keyCopied` or `factor.codes.copied`, once, by the one polite region that exists while step 5 is shown |
| step 5's last press is refused (`factorSetupDone`) | unchanged (the pressed button keeps focus) | `role="alert"` banner: `factor.error.tooMany` for 429, `factor.error.network` for a dropped connection, else `factor.error.other` with `{code}` |
| `devices.forget` on step 5 (the only way out) | Cancel, Escape and close return focus to it; the confirm opens on Cancel | the dialog; a refused wipe is a `role="alert"` in it and focus goes to Cancel |

Tab order inside each step: the fields in visual order, then (step 1, and the closed frame) `Use an
existing account`, then (step 3) the checkbox, then `Print`, then `Back`, then the forward button. There
are no single-key shortcuts.

## Error placement

| check | where | key |
|---|---|---|
| invite empty in mode 0 | under the invite field, step 1 | `onboarding.error.inviteRequired` |
| username rule | under the username field, step 2 | `onboarding.error.usernameRule` |
| display name length | under the display name field, step 2 | `onboarding.error.displayLength` |
| password length | under the password field, step 2 | `onboarding.error.passwordShort` |
| a refusal of `signupSubmit` | as the refusal table under "States" says | — |

## Reload and interruption

- Reload on step 1 or 2: the typed values are gone (nothing is stored); the page opens at step 1 with the
  invite of the URL, if any.
- Reload on step 3 or 4 before `Create account` was pressed, or while registering when the server had not
  created the account: the worker resets the pending sign-up (`Signup.resume()` returns 0) and the page
  opens at step 1. The key shown before was never used and is worthless; the next pass shows a new key.
  The page cannot tell this case from a first visit, so it says nothing about it.
- Reload while registering when the server had created the account: the worker finishes the sign-up
  (`Signup.resume()` returns 2). The account owes its second factor, so step 5 opens (origin `signup`).
- Reload on step 5: the page mounts at the choice again; the setup key and the backup codes were never stored
  and are gone. The phase is still `factor-setup`, so nothing else is reachable, and an account that is
  abandoned there meets the forced screen of flow 10 at its next start.
- When the browser's key is refused for an account that was registered (the stored session proves it), the page shows the `revoked` splash and nothing is deleted.
- When the data saved in this browser was written by a newer version of dilla (a rollback; the core refuses it
  untouched with `E_STORE_NEWER`, and `account.error.store` holds the data version it found and the highest this
  version reads), the boot screen shows the `boot.storeNewer.*` splash with both numbers and one `Reload` button.
  Nothing is deleted and no reset is offered: the newer version can still open the data.

## Copy

| key | text | where |
|---|---|---|
| `onboarding.step` | `New account · step {n} of {total}` | step line, steps 1–6 ({total} is 6; Signal task 10 adds the dots) |
| `onboarding.back` | `Back` | button, first in the footer of steps 2–4 |
| `onboarding.next` | `Continue` | button, forward on steps 1–3 |
| `onboarding.connect.title` | `Join {instance}` | step heading, step 1 |
| `onboarding.connect.body` | `You need an invite from someone on {instance}. Paste it below.` | paragraph, step 1, registration mode 0 |
| `onboarding.connect.bodyOpen` | `{instance} is open to new accounts. Paste an invite if you were given one.` | paragraph, step 1, registration mode 1 |
| `onboarding.connect.invite` | `Invite` | field label, step 1, mode 0 |
| `onboarding.connect.inviteOptional` | `Invite (optional)` | field label, step 1, mode 1 |
| `onboarding.connect.inviteHint` | `A code or a link, as you received it.` | hint, invite field |
| `onboarding.connect.signIn` | `Use an existing account` | ghost button under the invite hint, step 1, and under the closed frame's paragraph, only when `instance.passwordSignup` (L-COPY-02) |
| `onboarding.closed.title` | `{instance} is not taking new accounts` | step heading, closed frame |
| `onboarding.closed.body` | `Ask the host of {instance} when sign-ups open again.` | paragraph, closed frame |
| `onboarding.identity.title` | `Choose your name` | step heading, step 2 |
| `onboarding.identity.body` | `People on {instance} see these next to your messages.` | paragraph, step 2 |
| `onboarding.identity.username` | `Username` | field label, step 2 |
| `onboarding.identity.usernameHint` | `3 to 32 characters: a–z, 0–9, dot, underscore or hyphen.` | hint, username field |
| `onboarding.identity.display` | `Display name (optional)` | field label, step 2 |
| `onboarding.identity.displayHint` | `Up to 64 characters. Your username is shown when this is empty.` | hint, display name field |
| `onboarding.identity.password` | `Password` | field label, step 2, only when `instance.passwordSignup` |
| `onboarding.identity.passwordHint` | `At least 8 characters.` | hint, password field |
| `onboarding.keys.title` | `Your recovery key` | step heading, step 3 |
| `onboarding.keys.body` | `This key is shown once and is kept nowhere. Write it down or print it, and keep it away from this computer.` | first paragraph, step 3 |
| `onboarding.keys.loss` | `This key is the only way to get your account back or to add another browser. If every browser you use loses its data and you do not have the key, the account and its history are gone, and the host cannot bring them back.` | second paragraph, step 3 (L-COPY-02) |
| `onboarding.keys.label` | `Recovery key` | grid label, step 3 |
| `onboarding.keys.acknowledge` | `I have written down or printed my recovery key` | checkbox label, step 3 |
| `onboarding.keys.ackHint` | `Tick the box to continue.` | hint under the blocked (`aria-disabled`) `Continue`, step 3; its description while blocked |
| `onboarding.keys.print` | `Print` | button, step 3 |
| `onboarding.keys.preparing` | `Making your keys` | status line, step 3, while the key is not there yet |
| `onboarding.browser.title` | `What this browser keeps` | step heading, step 4 |
| `onboarding.browser.device` | `This browser now holds the key of this device, in its storage for {instance}. The key never leaves this browser.` | first paragraph, step 4 |
| `onboarding.browser.clear` | `Clearing this site’s data removes the key and the messages kept here, and this browser stops being your device.` | second paragraph, step 4 |
| `onboarding.browser.oneBrowser` | `To use this account in another browser, sign in there with your password and the second factor you add next, then this recovery key. Messages sent before that browser joins are not shown in it.` | third paragraph, step 4 (L-COPY-02; web3.6 adds the second factor) |
| `onboarding.browser.private` | `A private window forgets all of this when it closes.` | fourth paragraph, step 4 |
| `onboarding.browser.submit` | `Create account` | button, forward on step 4 |
| `onboarding.browser.submitting` | `Creating account…` | button, forward on step 4 while registering |
| `onboarding.browser.registering` | `Creating your account on {instance}…` | status line, step 4, while registering |
| `onboarding.done.title` | `You’re in` | step heading, step 6 |
| `onboarding.done.body` | `You are {username} on {instance}.` | paragraph, step 6 |
| `onboarding.done.next` | `Open dilla` | button, step 6 |
| `onboarding.error.inviteRequired` | `Paste the invite you were given.` | field error, invite, mode 0, empty on `Continue` |
| `onboarding.error.inviteInvalid` | `This invite is expired, used up or unknown. Ask for a new one.` | field error, invite, after `E_INVITE_INVALID` |
| `onboarding.error.usernameRule` | `Use 3 to 32 characters: a–z, 0–9, dot, underscore or hyphen, with no dot at the start or end.` | field error, username |
| `onboarding.error.usernameTaken` | `That username is taken. Try another.` | field error, username, after `E_INVALID_REQUEST` with status 409 |
| `onboarding.error.displayLength` | `Use at most 64 characters.` | field error, display name |
| `onboarding.error.detailsRefused` | `{instance} did not accept these details. Check the username and the display name.` | danger banner, step 2, after any other `E_INVALID_REQUEST` |
| `onboarding.error.passwordShort` | `Use at least 8 characters.` | field error, password |
| `onboarding.error.rateLimited` | `Too many attempts from this network. Try again in {seconds} s.` | warn banner, step 4, `retryAfterMs` known |
| `onboarding.error.rateLimitedNoWait` | `Too many attempts from this network. Wait a few minutes, then try again.` | warn banner, step 4, `retryAfterMs` null |
| `onboarding.error.network` | `The connection dropped while creating your account. Reload the page to finish.` | danger banner, step 4, `E_NETWORK` or status ≥ 500 |
| `onboarding.error.reload` | `Reload` | button in the network banner |
| `onboarding.error.other` | `The account could not be created ({code}). Try again.` | danger banner, step 4, any other refusal |
| `boot.storeNewer.status` | `This version of dilla is too old for the data saved in this browser` | boot splash status, phase `error` with `E_STORE_NEWER` (v0.1.0-web3.6, C4x) |
| `boot.storeNewer.detail` | `That data needs data version {found}; this version of dilla reads up to {supported}. Update to the newer version of dilla, or ask the host of this server.` | boot splash detail; `{found}` and `{supported}` are `account.error.store` |
| `boot.storeNewer.action` | `Reload` | the splash's only button; it reloads the page |
