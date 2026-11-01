# Flow 15: change password (EOD gaps, E1)

## States

| State | UI |
|---|---|
| Settings → Sign-in, the account has a password | the password row keeps `factorSettings.password.set` and gains the action `settings.password.change` |
| Settings → Sign-in, the account has no password | the password row keeps `factorSettings.password.none` and gains the action `settings.password.add` |
| change dialog | `settings.password.titleChange`, `settings.password.bodyChange`, three fields: `settings.password.old`, `settings.password.new` (with `settings.password.newHint`), `settings.password.confirm`; the confirm button reads `settings.password.confirmChange` |
| set dialog, the account holds no second factor | `settings.password.titleSet`, `settings.password.bodySet`, two fields: new and confirm; the confirm button reads `settings.password.confirmSet` |
| set password pressed | a fresh session is minted first (the instance wants one under five minutes old), then, if the account holds a second factor, the step-up dialog of flow 10 (a passkey, or a code when it has no passkey); its token is spent by the set |
| a fresh session is refused | `settings.password.error.other` with the code, in the Sign-in section |
| a field is empty or short | `settings.password.error.oldRequired` on the current password, `settings.password.error.short` on the new one; nothing is sent, focus goes to that field |
| the two new passwords differ | `settings.password.error.mismatch` on the confirm field; nothing is sent |
| sending | the confirm button reads `settings.password.busy`; the current password field is emptied the moment it is sent |
| the current password is wrong (403) | `settings.password.error.wrong` as an alert; the current password field is empty and has focus |
| too many tries (429) | `factor.error.tooMany` with the seconds the instance names; the person decides when to try again |
| a first password on a session that went stale while the dialog was open (403 on a set) | the dialog closes and the Sign-in section shows `settings.password.error.stale`; the next press of "set password" mints a fresh session |
| the step-up token ran out (403 `E_STEP_UP`) | back to the step-up dialog of flow 10 with `factorSettings.stepUp.expired` |
| the instance is out of reach | `factor.error.network` |
| any other refusal | `settings.password.error.other` with the code |
| done | the dialog closes, focus returns to the password row's button, the row now says `factorSettings.password.set` |

The change dialog also holds a read-only `settings.password.username` field, visually hidden, so a password manager updates the right entry. The new password and its confirmation take at most 1024 characters; the instance has no bound of its own beyond a non-empty password and its request size cap.

## 1280×800

The dialog is the same centred dialog the factor actions use. The three fields stack in one column, each with its label above it.

## 360×740

The dialog is full width. The fields stack, every target is at least 24 px, and nothing scrolls sideways at 200 % zoom. The password manager is offered the fields: the current one is `current-password`, the other two `new-password`.

## Keyboard map

| Key | Where | Effect |
|---|---|---|
| Enter | Any field of the dialog | Submits it. |
| Escape | The dialog | Cancels and closes only that dialog, never the Settings frame under it. |

## Focus rules

- The dialog opens on its first field. It closes to the row button that opened it.
- A field error focuses the field. A refused current password focuses its emptied field.
- A refusal that is not about a field (429, network, other) leaves focus on Cancel, next to the alert.

## Announcements

Polite, through the Settings announcer: `settings.password.changed` after a change and `settings.password.added` after a first password. Errors are `role=alert`. The worker keeps neither password: they pass through the one `changePassword` command and the form state is cleared when the dialog closes.

## Copy

`settings.password.change='Change'`
`settings.password.add='Set password'`
`settings.password.titleChange='Change your password'`
`settings.password.titleSet='Set a password'`
`settings.password.bodyChange='Signing in on a new browser will ask for the new password. Browsers that are already set up do not ask again.'`
`settings.password.bodySet='With a password you can sign in on a new browser with it and your second factor.'`
`settings.password.username='Username'`
`settings.password.old='Current password'`
`settings.password.new='New password'`
`settings.password.newHint='At least 8 characters.'`
`settings.password.confirm='New password again'`
`settings.password.confirmChange='Change password'`
`settings.password.confirmSet='Set password'`
`settings.password.busy='Saving…'`
`settings.password.cancel='Cancel'`
`settings.password.error.oldRequired='Type your current password.'`
`settings.password.error.short='Use at least 8 characters.'`
`settings.password.error.mismatch='The two passwords do not match.'`
`settings.password.error.wrong='That password did not work.'`
`settings.password.error.stale='That took too long. Try again.'`
`settings.password.error.other='That did not work ({code}). Try again.'`
`settings.password.changed='Password changed.'`
`settings.password.added='Password set.'`
