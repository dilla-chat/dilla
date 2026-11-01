# Flow 17: profile, the display name, a status message and a bio (EOD E6)

## States

| State | UI |
|---|---|
| Settings → profile | the first section of the nav (`settings.nav.profile`); a `Profile` heading (`profile.title`), the line `profile.body`, then (Task 11, Jonas Q7: the prototype's) three fields, `profile.display.label`, `profile.status.label` and `profile.bio.label` (a several-line field), the note `profile.footnote`, and beside them the preview card (`profile.preview`) with the avatar, the name, "@handle · status" and the bio as they are typed. There is no Save: a valid change is saved 700 ms after the last keystroke (Enter in a one-line field saves at once), `profile.saving` then `profile.saved` |
| something changed | only the changed fields are sent, once typing stops, and an empty one clears the field; a field that the rule refuses, or a failed save, holds the section until it is fixed (the workspace asks before leaving) |
| a stored display name that fails the rule (it was kept from before) | no error: a field is checked only when it has been changed, so status and bio save as usual |
| sign-up, step 2 | a display name the server would refuse (an invisible or direction-changing character) shows `profile.error.chars` under the field and no keys are made; spaces only counts as empty |
| the display name emptied | save shows `profile.error.display` under the field and sends nothing |
| text the server would refuse | the field shows its problem as it is typed or pasted and save is disabled: `profile.error.chars` (a tab, an invisible or direction-changing character, a line break in the status message), `profile.error.long`, `profile.error.blank` (only spaces); none says "try again", because trying again cannot help. A refusal that still comes back from the server (a 400) shows `profile.error.chars` in a banner |
| saving | `profile.save` is disabled until the answer, and after it until something changes again |
| saved | a polite status line `profile.saved`; the account, every member list and every open card show the new text |
| too many saves in a short time (429) | a danger banner `profile.error.wait` |
| the save refused or the network down | a danger banner `profile.error.save`; what was typed stays and save is enabled again |
| changed on another device | the fields follow it while nothing here has been edited |
| the profile card | the status message after the presence word; the bio under the head, a focusable group named `profile.card.about` (Tab reaches it, a visible ring), with its line breaks and scrolling after eight rem; both set with `dir="auto"` so a right-to-left text does not reorder against its neighbours |
| a member with no status message or bio | the card shows neither |
| the phone | Settings → profile is the first row of the You tab's settings list |

The three texts are stored readable by the server, like the display name, and shown to everyone who shares a server with the account. They are not end-to-end encrypted, and the section says only that the server can read them (`profile.body`).

## Keyboard map

| Keys | Action |
|---|---|
| Tab | name, status message, bio, save |
| Enter | in the display name or status message: save; in the bio: a line break |

## Focus rules

Opening the section focuses its heading (as every settings section). A refused save keeps focus where it was. Closing the card returns focus to the row, name or mention it came from (flow 14).

## Announcements

`profile.saved` is announced politely when the save lands; `profile.error.display` and `profile.error.save` are alerts.

## Copy

`settings.nav.profile='Profile'`
`profile.title='Profile'`
`profile.body='How people see you across your servers.'`
`profile.footnote='Name, status and bio are stored readable by the host, unlike messages.'`
`profile.preview='Your profile card'`
`profile.saving='Saving…'`
`profile.display.label='Display name'`
`profile.status.label='Status message'`
`profile.bio.label='Bio'`
`profile.saved='Profile saved'`
`profile.error.display='Enter a display name.'`
`profile.error.chars='That has characters dilla can’t show.'`
`profile.error.long='Too long.'`
`profile.error.blank='Write something, or leave it empty.'`
`profile.error.wait='Too many saves. Wait a moment, then save again.'`
`profile.error.save='Could not save your profile. Try again.'`
`profile.card.about='about'`
