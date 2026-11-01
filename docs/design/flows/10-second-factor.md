# Flow 10: second factor (v0.1.0-web3.6)

Every human account on an instance has a second factor, and the server enforces it. This flow covers choosing and setting one up (at sign-up, on a new browser, or forced for an account that has none), signing in with a passkey or a backup code, and managing the factors in Settings. The factor guards signing in on a new browser. It does not guard messages, and no copy says it does. Backup codes are not the recovery key. Related: `01-onboarding.md` (step 5 of 6), `03-sign-in.md` (step 3), `09-roles-and-private-channels.md` (`roles.assign.error.twoFactor`).

## States

| State | What shows | Copy keys |
|---|---|---|
| 1. Choose, for sign-up | Step 5 of 6 of onboarding, in `OnboardingFrame`. `ChoiceCards` with the authenticator app and the passkey, and `Button` Continue. | `factor.choose.title`, `factor.choose.body`, `factor.choose.label`, `factor.choose.app`, `factor.choose.appHint`, `factor.choose.passkey`, `factor.choose.passkeyHint`, `factor.choose.next` |
| 2. Choose, when forced | The same screen, mounted by App for an account that owes a factor. It adds the required paragraph. There is no skip and no cancel control. The one way out is the ghost button `devices.forget` under the frame (all three set-up views have it): it opens a confirm (`devices.forgetTitle`, `devices.forgetBody`, `devices.confirmForget`, `devices.working`, `signin.cancel`) that sends `forgetBrowser` and nothing else. It clears this browser; it never marks the factor as set and never reaches the shell. | `factor.choose.required`, `devices.forget`, `devices.forgetTitle`, `devices.forgetBody`, `devices.confirmForget`, `devices.working` |
| 3. Passkey unusable | The passkey card is disabled and shows its reason. The app card stays selectable. | `factor.choose.passkeyOff` |
| 4. No-password account | Only the passkey card, preselected, with its reason. | `factor.choose.noPassword` |
| 5. TOTP loading | The app step while the setup key is made. | `factor.totp.loading` |
| 6. TOTP shown | The QR code and the setup key beside each other, a Copy button for the key (copied: a polite announcement), a code field, `Button` Back and Continue. | `factor.totp.title`, `factor.totp.body`, `factor.totp.qr`, `factor.totp.key`, `factor.totp.copyKey`, `factor.totp.keyCopied`, `factor.totp.code`, `factor.totp.codeHint`, `factor.totp.submit`, `factor.back` |
| 7. TOTP checking | The Continue button is busy while the code is checked. | `factor.totp.working` |
| 8. Wrong or malformed code | A field error on the code field, focus on the field. A refused code, a code that is not six digits, or an empty field. A dropped connection or any other refusal shows a banner. | `factor.error.code`, `factor.error.codeFormat`, `factor.error.required`, `factor.error.network`, `factor.error.other` |
| 9. Passkey naming | A name field with its hint and a default name, `Button` Back and Add passkey. | `factor.passkey.title`, `factor.passkey.body`, `factor.passkey.name`, `factor.passkey.nameHint`, `factor.passkey.defaultName`, `factor.passkey.submit`, `factor.error.nameLong` |
| 10. Passkey waiting | Add passkey is busy while the browser asks for the passkey. | `factor.passkey.working` |
| 11. Passkey cancelled, wrong address, exists, refused | A banner with the matching text. Focus goes back to the button that started the ceremony. A dropped connection or any other refusal shows `factor.error.network` or `factor.error.other`. | `factor.error.cancelled`, `factor.error.wrongAddress`, `factor.error.exists`, `factor.error.refused`, `factor.error.network`, `factor.error.other` |
| 12. Backup codes | An ordered list of codes (a real `<ol>`) with Print and Copy. The checkbox is unticked and Continue is disabled with its hint. Ticked: Continue is enabled. Copied: a polite announcement. | `factor.codes.title`, `factor.codes.body`, `factor.codes.notKey`, `factor.codes.label`, `factor.codes.print`, `factor.codes.copy`, `factor.codes.copied`, `factor.codes.ack`, `factor.codes.ackHint`, `factor.codes.next` |
| 13. Forced done | The last step of the forced set-up, after the backup codes. The announcement is spoken when the factor is in place. | `factor.done.title`, `factor.done.totp`, `factor.done.passkey`, `factor.done.next`, `factor.added` |
| 14. Sign-in with a passkey | A link under the password field on step 2 of sign-in, then step 3 as a passkey screen. The browser asks for the passkey on Use passkey. A failure and an unknown passkey are banners. | `factorSignin.usePasskey`, `factorSignin.passkey.title`, `factorSignin.passkey.body`, `factorSignin.passkey.submit`, `factorSignin.passkey.working`, `factorSignin.passkey.failed`, `factorSignin.passkey.unknown`, `factorSignin.passkey.unusable` |
| 15. Sign-in needs a code after a passkey | The passkey did not check the person, so step 3 turns into the code form with a note above it. | `factorSignin.passkey.needsCode` |
| 16. Backup-code sign-in, a failure, codes left | A link on the code form leads to a backup-code field. A refused code is a banner. After signing in with a backup code the shell says how many remain. The lost-phone line sits under the form. | `factorSignin.useBackup`, `factorSignin.lost`, `factorSignin.backup.title`, `factorSignin.backup.body`, `factorSignin.backup.label`, `factorSignin.backup.failed`, `factorSignin.backup.format`, `factorSignin.left.one`, `factorSignin.left.other` |
| 17. Settings loading and error | Settings → Sign-in while the factors load, then on a failed load a message and a Try again button. | `factorSettings.loading`, `factorSettings.error.load`, `factorSettings.retry` |
| 18. Settings with the app only | `SettingsFrame` section with grouped `FactorRow`s: password, authenticator app, passkeys (none), backup codes. Remove is disabled while it is the only factor, with the reason beside it. (Task 11: no closing paragraph; the prototype's copy.) | `factorSettings.nav`, `factorSettings.title`, `factorSettings.body`, `factorSettings.password`, `factorSettings.password.changed`, `factorSettings.password.none`, `factorSettings.totp`, `factorSettings.totp.on`, `factorSettings.totp.off`, `factorSettings.totp.add`, `factorSettings.totp.remove`, `factorSettings.totp.removeLabel`, `factorSettings.only`, `factorSettings.codes`, `factorSettings.codes.one`, `factorSettings.codes.other`, `factorSettings.codes.none`, `factorSettings.codes.make` |
| 19. Settings with the app and passkeys | The passkeys are one group under a `factorSettings.passkeys` heading: one row per passkey, "Passkey" over its name (Task 11), each with its own remove button, then add passkey after the rows. The group sits between the app and the backup codes. | `factorSettings.passkeys`, `factorSettings.passkeys.none`, `factorSettings.passkey`, `factorSettings.passkeys.add`, `factorSettings.passkeys.remove`, `factorSettings.passkeys.removeLabel` |
| 20. Step-up | A dialog before any change. A password field, or Use passkey when the account has no password, or a code field (an authenticator code or a backup code) when it has neither a password nor a passkey. Failed: the password did not work, and focus goes back to the emptied field. Expired: the step-up token ran out. Too many: the attempts are throttled. | `factorSettings.stepUp.title`, `factorSettings.stepUp.body`, `factorSettings.stepUp.password`, `factorSettings.stepUp.passkeyBody`, `factorSettings.stepUp.usePasskey`, `factorSettings.stepUp.confirm`, `factorSettings.stepUp.working`, `factorSettings.stepUp.failed`, `factorSettings.stepUp.codeBody`, `factorSettings.stepUp.code`, `factorSettings.stepUp.codeFailed`, `factorSettings.stepUp.expired`, `factor.error.tooMany` |
| 21. Remove and make-codes confirms and results | A confirm dialog per action, opening on Cancel. A removal steps up first; making new codes confirms first, then steps up, then makes them with no second press. A removal says what it costs. Results are polite announcements. A refusal because it is the only factor, or because the factors changed on another device, is shown in the dialog. | `factorSettings.removeTitle.totp`, `factorSettings.removeTitle.passkey`, `factorSettings.removeBody`, `factorSettings.removeConfirm`, `factorSettings.removeBusy`, `factorSettings.cancel`, `factorSettings.removed.totp`, `factorSettings.removed.passkey`, `factorSettings.codesMade`, `factorSettings.codes.makeTitle`, `factorSettings.codes.makeBody`, `factorSettings.codes.makeConfirm`, `factorSettings.error.only`, `factorSettings.error.changed`, `factorSettings.error.other` |
| 22. Assigning a moderator role to an account without a factor | A cross-reference row. `E_TARGET_NO_FACTOR` shows the role dialog's refusal; the key lives in flow 09. | `roles.assign.error.twoFactor` |

## 1280×800

The factor steps sit in `OnboardingFrame`, centred in the column the other onboarding steps use. The QR code is 192 px, beside the setup key and its Copy button. The backup codes are in at most two columns, two only when each column holds one whole code on one line (otherwise one). Settings → Sign-in is a section of `SettingsRow`s in one card: password, authenticator app, each passkey, the passkeys' add row, backup codes; each row's action is its button at the right. Dialogs are centred.

## 360×740

The QR code is above the setup key at 160 px. The backup codes are in one column. The ChoiceCards stack. The `FactorRow` actions wrap under the row text. Dialogs are full width. Nothing scrolls sideways at 200 % zoom, and every target is at least 24 px.

## Keyboard map

| Key | Where | Effect |
|---|---|---|
| Arrow keys | ChoiceCards | Move and select the card, wrapping at the ends. Tab leaves the group. |
| Enter | Each step's form | Submits the step. |
| Enter or Space | Copy, Print | Activates the button. |
| Space | The codes checkbox | Toggles it. |
| Enter | Step-up dialog | Confirms. |
| Escape | Step-up dialog, confirm dialogs | Cancels and closes only that dialog, never the Settings frame under it. |

## Focus rules

- Each step focuses its heading when it appears. A field error focuses the field.
- After a cancelled or refused WebAuthn ceremony, focus goes to the button that started it.
- Dialogs open on their first field, else Cancel, never the destructive button. They close to the row button that opened them.
- After a removal, focus goes to that group's add button.
- `devices.forget` on a set-up view: Cancel, Escape or the close button return focus to it; the confirm opens on Cancel; a refused wipe shows `factor.error.other` in the dialog and focus goes to Cancel.
- A refused `factorSetupDone` (state 13) is read by status and code: 429 is `factor.error.tooMany`, a dropped connection `factor.error.network`, anything else `factor.error.other` with the code. It is a `role="alert"` banner above the frame; focus stays on the button that was pressed.
- `E_FACTOR_REQUIRED` (the account still owes a factor: the worker's fresh read found none, as after an operator reset) shows no banner: the set-up goes back to the choice, whose own copy says a factor is owed, and the title takes focus.
- The set-up skips to its end unasked (a reload after the factor was confirmed) only when the `factors` slice says the account owes nothing; a slice that still says owed shows the choice.

## Announcements

Polite, through the screen's `useAnnouncer` (it queues, so two in a row are both spoken): `factor.added`, `factor.totp.keyCopied`, `factor.codes.copied`, `factorSettings.removed.totp`, `factorSettings.removed.passkey` and `factorSettings.codesMade`. Errors are `role=alert`.

## Copy

- `factor.choose.title='Add a second factor'`
- `factor.choose.body='Every account on {instance} needs a second factor. Signing in on a new browser then takes your password and a code from an authenticator app, or a passkey on its own.'`
- `factor.choose.required='Your account needs a second factor before you can continue. {instance} asks every account for one.'`
- `factor.choose.label='Second factor'`
- `factor.choose.app='authenticator app'`
- `factor.choose.appHint='An app on your phone that shows a new six-digit code every 30 seconds.'`
- `factor.choose.passkey='passkey'`
- `factor.choose.passkeyHint='Your phone, a security key or this computer confirms it is you with a fingerprint, your face or a PIN. A passkey signs you in on its own.'`
- `factor.choose.passkeyOff='Passkeys cannot be used here. Use an authenticator app.'`
- `factor.choose.noPassword='Your account has no password, so its second factor is a passkey.'`
- `factor.choose.next='Continue'`
- `factor.totp.title='Set up your authenticator app'`
- `factor.totp.body='In the app, add an account and scan this code. If you cannot scan it, type the setup key instead.'`
- `factor.totp.qr='QR code to add {username} on {instance} to an authenticator app'`
- `factor.totp.key='Setup key'`
- `factor.totp.copyKey='Copy'`
- `factor.totp.keyCopied='Setup key copied.'`
- `factor.totp.code='Code from the app'`
- `factor.totp.codeHint='Six digits. The app shows a new one every 30 seconds.'`
- `factor.totp.loading='Making a setup key…'`
- `factor.totp.submit='Continue'`
- `factor.totp.working='Checking…'`
- `factor.passkey.title='Add a passkey'`
- `factor.passkey.body='Your browser asks where to keep the passkey: on this computer, your phone or a security key. Keep it on this computer only if no one else uses it.'`
- `factor.passkey.name='Passkey name'`
- `factor.passkey.nameHint='So you can tell your passkeys apart later. Up to 64 characters.'`
- `factor.passkey.defaultName='Passkey {n}'`
- `factor.passkey.submit='Add passkey'`
- `factor.passkey.working='Waiting for your passkey…'`
- `factor.back='Back'`
- `factor.codes.title='Your backup codes'`
- `factor.codes.body='If you lose your phone or your passkey, each of these codes signs you in once in its place. Keep them with your recovery key, away from this computer.'`
- `factor.codes.notKey='Backup codes are not your recovery key. You still need the recovery key to add a browser.'`
- `factor.codes.label='Backup codes'`
- `factor.codes.print='Print'`
- `factor.codes.copy='Copy'`
- `factor.codes.copied='Backup codes copied.'`
- `factor.codes.ack='I have written down or printed my backup codes'`
- `factor.codes.ackHint='Tick the box to continue.'`
- `factor.codes.next='Continue'`
- `factor.done.title='Second factor added'`
- `factor.done.totp='Signing in on a new browser now takes your password and a code from your authenticator app.'`
- `factor.done.passkey='You can now sign in on a new browser with this passkey.'`
- `factor.done.next='Open dilla'`
- `factor.added='Second factor added.'`
- `factor.error.required='Fill in this field.'`
- `factor.error.codeFormat='A code has six digits.'`
- `factor.error.code='That code did not match. Type the code the app shows now, and check that your phone sets its clock automatically.'`
- `factor.error.cancelled='No passkey was added. Try again, or use an authenticator app.'`
- `factor.error.wrongAddress='Passkeys do not work at this address of {instance}. Use an authenticator app, and tell the operator.'`
- `factor.error.exists='This passkey is already on your account.'`
- `factor.error.refused='{instance} did not accept the passkey. Try again, or use an authenticator app.'`
- `factor.error.nameLong='That name is too long. Try a shorter one.'`
- `factor.error.tooMany='Too many attempts. Try again in {seconds} s.'`
- `factor.error.network='The connection dropped. Check it and try again.'`
- `factor.error.other='That did not work ({code}). Try again.'`
- `factorSignin.usePasskey='Sign in with a passkey'`
- `factorSignin.passkey.title='Use your passkey'`
- `factorSignin.passkey.body='This account signs in with a passkey. Your browser asks for it when you continue.'`
- `factorSignin.passkey.submit='Use passkey'`
- `factorSignin.passkey.working='Waiting for your passkey…'`
- `factorSignin.passkey.failed='The passkey did not sign you in. Try again, or use a backup code.'`
- `factorSignin.passkey.unusable='This browser cannot use passkeys here. Use a backup code instead, or sign in from a browser that can.'`
- `factorSignin.passkey.unknown='{instance} does not know this passkey. It may belong to another account or another server.'`
- `factorSignin.passkey.needsCode='This passkey did not check that it is you, so {instance} also asks for your code.'`
- `factorSignin.useBackup='Use a backup code instead'`
- `factorSignin.backup.title='Use a backup code'`
- `factorSignin.backup.body='Type one of the backup codes you saved when you added your second factor. Each code works once.'`
- `factorSignin.backup.label='Backup code'`
- `factorSignin.backup.failed='That backup code did not work. Sign in again and try another one.'`
- `factorSignin.backup.format='A backup code has 26 letters and digits. Hyphens and spaces do not matter.'`
- `factorSignin.lost='Lost your phone? Use a backup code, or ask the operator of {instance} to reset your second factor.'`
- `factorSignin.left.one='You have 1 backup code left. Make new ones in Settings, under sign-in.'`
- `factorSignin.left.other='You have {count} backup codes left. Make new ones in Settings, under sign-in.'`
- `factorSettings.nav='Sign-in'`
- `factorSettings.title='Sign-in'`
- `factorSettings.body='Password and second factors for new browsers.'`
- `factorSettings.loading='Loading…'`
- `factorSettings.error.load='Could not load your sign-in settings ({code}).'`
- `factorSettings.retry='Try again'`
- `factorSettings.password='Password'`
- `factorSettings.password.changed='Changed {when}'` (Task 11: hidden while the date is unknown)
- `factorSettings.password.none='This account has no password.'`
- `factorSettings.totp='Authenticator app'`
- `factorSettings.totp.on='Added {date}'`
- `factorSettings.totp.off='Not set up.'`
- `factorSettings.totp.add='Add authenticator app'`
- `factorSettings.totp.remove='Remove'`
- `factorSettings.totp.removeLabel='Remove the authenticator app'`
- `factorSettings.passkeys='Passkeys'`
- `factorSettings.passkeys.none='No passkeys yet.'`
- `factorSettings.passkey='Passkey'` (Task 11: each passkey row is "Passkey" over its name)
- `factorSettings.passkeys.add='Add passkey'`
- `factorSettings.passkeys.remove='Remove'`
- `factorSettings.passkeys.removeLabel='Remove {name}'`
- `factorSettings.codes='Backup codes'`
- `factorSettings.codes.one='1 of 10 left'`
- `factorSettings.codes.other='{count} of 10 left'`
- `factorSettings.codes.none='No backup codes left. Make new ones so you are not locked out.'`
- `factorSettings.codes.make='New codes'`
- `factorSettings.codes.makeTitle='Make new backup codes?'`
- `factorSettings.codes.makeBody='Your old backup codes stop working at once.'`
- `factorSettings.codes.makeConfirm='Make new codes'`
- `factorSettings.only='This is your only second factor. Add another one before you remove it.'`
- `factorSettings.removeTitle.totp='Remove the authenticator app?'`
- `factorSettings.removeTitle.passkey='Remove {name}?'`
- `factorSettings.removeBody='You can no longer sign in on a new browser with it. Browsers that are already set up keep working.'`
- `factorSettings.removeConfirm='Remove'`
- `factorSettings.removeBusy='Removing…'`
- `factorSettings.cancel='Cancel'`
- `factorSettings.removed.totp='Authenticator app removed.'`
- `factorSettings.removed.passkey='{name} removed.'`
- `factorSettings.codesMade='New backup codes made.'`
- `factorSettings.stepUp.title='Confirm it is you'`
- `factorSettings.stepUp.body='Type your password to change how you sign in.'`
- `factorSettings.stepUp.password='Password'`
- `factorSettings.stepUp.passkeyBody='Use one of your passkeys to change how you sign in.'`
- `factorSettings.stepUp.usePasskey='Use passkey'`
- `factorSettings.stepUp.confirm='Confirm'`
- `factorSettings.stepUp.working='Checking…'`
- `factorSettings.stepUp.failed='That password did not work.'`
- `factorSettings.stepUp.expired='That took too long. Confirm it is you again.'`
- `factorSettings.error.only='You cannot remove your only second factor.'`
- `factorSettings.error.changed='Your sign-in settings changed on another device. Check them and try again.'`
- `factorSettings.error.other='That did not work ({code}). Try again.'`
- `factorSettings.stepUp.passkeyFailed='Your passkey did not confirm it is you. Try again.'`
- `factorSettings.added.totp='Authenticator app added.'`
- `factorSettings.added.passkey='{name} added.'`
- `factorSettings.codes.newTitle='Your new backup codes'`
- `factorSettings.codes.newBody='Your old backup codes no longer work. Each of these signs you in once in place of your second factor. Keep them with your recovery key, away from this computer.'`
- `factorSettings.codes.makeBusy='Making codes…'`
- `factorSettings.stepUp.codeBody='Type the code from your authenticator app, or a backup code, to change how you sign in.'`
- `factorSettings.stepUp.code='Code'`
- `factorSettings.stepUp.codeFailed='That code did not work.'`
- `factorSettings.stepUp.again='Nothing was added. Confirm it is you again to try once more.'`
