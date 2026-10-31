# Signal wave B — unreleased

Planned tag: `v0.1.0-signal2`. This release changes **dillad, its authentication
and store code, the browser client and dillabot's error decoder**. There are no
new database migrations and no negotiated wire-version bump in wave B.

## What changed for users

- **Task 1 — parity tooling:** the design runner compares the running app with
  the Signal prototype, including desktop and phone states. Its optional judge
  is advisory; the closeout captures run without it.
- **Task 2 — commands:** `/` opens all available commands in Built-in and Bot
  groups with icons and descriptions. Arguments are inline chips with keyboard
  navigation and suggestions. Errors appear in the command bar and mark the
  active chip. Escape keeps the draft, including multiline code. Reminder
  previews use the browser's time zone. `/me` keeps its author.
- **Task 3 — sign-in:** six-digit code boxes accept typing, paste and autofill,
  submit the sixth digit, and keep incomplete input available for correction. Passkeys use
  conditional mediation and username-field autofill where supported; other
  browsers show the passkey button. Autofill starts on username focus; pending ceremonies are renewed, and choosing
  a passkey clears the password draft.
  Back navigation, backup-code and passkey alternatives, and the done panel
  follow the Signal flow. A passkey used after a password must belong to that
  account: another account's key is refused as `E_PASSKEY_OTHER_ACCOUNT`.
- **Task 3b — signup:** six steps: invite, account, recovery key, second factor,
  code and done. The account is created only after **I saved it** on the recovery
  key step. **Print** and **Save as file** save the key; there is no Copy button.
  The name starts as the username and can be changed in Profile. Backup codes
  appear on the done step, behind **Show** on phones, and must be acknowledged
  before **Open dilla**. After creation, Sign in and returning to the recovery key
  are unavailable; Back on the authenticator step returns to the factor choice. The joined server
  is selected when the app opens.
- **Task 4 — voice:** your voice menu has Mute, Deafen and their shortcuts,
  Profile, Move to with room counts, and Leave voice. Other people's menus offer
  Profile, Message, Mention, Copy username and Edit roles when allowed, with local
  volume and mute controls. Bot menus leave out Message. Mention opens a text
  channel where you can send. The AFK row opens its page. **Go AFK** leaves your
  call; **Come back** opens a writable text channel without rejoining the old call. The title names who is
  away, or says **Nobody is away**; phones are never moved for inactivity.
- **Task 5 — the remaining shell states:** settings, reminders, status menus,
  phone navigation and chrome follow the prototype. Due and Upcoming reminders
  have their own empty states. Desktop status choices show their selected state;
  the phone You tab has Settings and Sign out, with no Status menu. Phone member
  sheets, DM lists and Profile, Sign-in, Devices, Keyboard and Overview settings
  use the Signal layout. Selected marks, the add button and phone taps are
  aligned; Members does not reopen on its own. Removing another browser still
  asks for the recovery key.
  Device titles derived from the user agent stay local display text and are
  never published as a device rename by themselves.
- **Task 6 — private results:** inline cards show Working, Done, Failed and
  Not confirmed. Bot commands use the bot's name (Glenn by default) and BOT tag;
  built-ins use dilla with the D tile. Uncertain delivery uses Failed with an
  explanation. The /kick and /ban commands ask for confirmation and show a Logged row; only
  Ban shows Duration and Reason. Cancelling only these two commands adds a
  Not confirmed row. Permanent bans have no end date. Confirmations fire once.
- **Task 7 — polls:** the New poll dialog has answer controls, duration,
  multiple-choice selection and a question counter in Unicode code points.
  Cards show answers, bars, chosen votes, named voters and time left. Distinct
  voters are counted across answers; bounded reads show **{n}+ voters** when
  incomplete. An uncertain vote says “Your vote may not have counted. Tap it
  again to retry.” Only that answer can retry while it is pending.
- **Task 8 — conversations:** Threads opens the last opened thread in a channel,
  otherwise the most recently active thread, or the empty **No threads yet**
  aside. Reply jumps keep the highlight tint and accent bar with reduced motion.
  GIF pickers, mixed image/file trays, sent attachment previews, Copy feedback,
  code-only message toolbars, phone message actions, emoji pickers and DM
  conversations use the Signal styling. Uploads report oversized files and
  waiting-to-send states with toasts. Failed attachments do not block sending another message or an
  app upgrade. Bot name lookup is shared by the picker and results without
  periodic Bot polling. Profile comparisons use an updated display name.
  ReactionTooltip is built in the UI library; showing reactor identities in
  the app remains wave C.

## Server, core and protocol compatibility

New or changed passwords need **12 Unicode code points**. Existing shorter
passwords still sign in. Account creation and password changes reject a shorter
new password before storing it; a password change checks the new length before
checking the old password or step-up.

Passkey login begins use separate rate buckets: `passkey_begin` per address
(default 0.2/s, burst 10), and `passkey_begin_net` per IPv4 /24 or IPv6 /48
(default 1/s, burst 40). Beginning autofill spends no password attempt. Finishes
still use the login token bucket; a present ceremony with an invalid assertion
counts as a failed login. Finishing an absent, expired, evicted or already spent
ceremony does not charge the address's login failure budget.

Discoverable login ceremonies live **120 seconds**, matching the browser's
requested timeout. Registration and step-up ceremonies live **5 minutes**.
`auth.webauthn.max_open_logins` defaults to **10000** open discoverable logins;
`max_open_per_user` defaults to **5** registrations/step-ups. At the cap, an
atomic write evicts the oldest ceremony in that scope so a new begin succeeds.
SQLite and PostgreSQL enforce the cap; the maintenance tick prunes expired rows.

[HTTP API](../../../protocol/09-http-api.md) documents an optional third `passkey`
flag on a successful password login that still owes a factor. Older servers omit
it and new clients read omission as zero. Response decoders must allow that
third element; clients that require exactly two elements need updating. Passkey finish accepts an optional
third password-login assertion; it must be live and still owe a factor. A
matching passkey consumes it; another account's passkey returns 403
`E_PASSKEY_OTHER_ACCOUNT` without issuing an assertion. Older two-element finish
requests keep their meaning.

[Error vocabulary](../../../protocol/02-delivery-service.md) adds that error code;
the web/client-core and dillabot decoders recognise it. These are additive HTTP
CBOR changes, not a new delivery-service wire version.
[Identity](../../../protocol/03-identity.md) permits printing or downloading the
recovery key while continuing to forbid a Copy button. The core diff adds a
regression test proving reset/signup creates new keys that the discarded
recovery key cannot open; it changes no core implementation or WASM interface.

## Deploy order

1. Back up the server database and configuration. Plan for the dillad restart to
   interrupt active calls. No new migration is required by wave B.
2. Deploy **dillad first**, including the new web assets. Keep operator rate and
   ceremony-cap overrides, or use the defaults above.
3. Deploy/restart **dillabot** from the same release: `internal/bot/client` changed
   to recognise `E_PASSKEY_OTHER_ACCOUNT`, although `cmd/dillabot` itself did not.
4. Reload browser tabs after the server upgrade. Check password and passkey
   sign-in, signup, a private command result, a poll and direct thread opening.
