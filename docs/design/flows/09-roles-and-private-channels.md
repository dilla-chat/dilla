# Flow 09: roles and private channels (v0.1.0-web3.5)

Roles, the permission editor, role assignment, channel permissions, private channels, role mentions and per-server notifications. Chrome is lower case; titles, bodies and refusals are sentence case. Every refusal is handled with its copy key, never with a raw code alone.

## States

| State | What shows | Copy keys |
| --- | --- | --- |
| Roles tab hidden | Without manage roles the Roles tab is absent from server settings. | `roles.tab` |
| Roles list loading | A loading line in place of the list. | `roles.loading` |
| Roles list error | The error line with a retry button that reloads the list. | `roles.error.load`, `roles.retry` |
| Roles list | Title, body, then the roles from highest to lowest with @everyone last, each with its member count, edit, move up, move down and delete buttons, and a new role button. | `roles.title`, `roles.body`, `roles.new`, `roles.everyone`, `roles.everyoneHint`, `roles.row.one`, `roles.row.other`, `roles.edit`, `roles.editLabel`, `roles.up`, `roles.upLabel`, `roles.down`, `roles.downLabel`, `roles.delete`, `roles.deleteLabel` |
| Locked rows | A role at or above the viewer's highest role shows the locked note and no actions. | `roles.locked` |
| Move done or failed | Done is announced politely. A failure shows an inline error that names the role. | `roles.moved`, `roles.error.move` |
| Editor, new or edit | A dialog with the name, colour swatches, the two toggles and the permission grid in four groups. A permission the viewer does not hold is disabled with the not-held note. | `roles.editor.titleNew`, `roles.editor.titleEdit`, `roles.editor.name`, `roles.editor.nameHint`, `roles.editor.color`, `roles.editor.colorHint`, `roles.editor.hoist`, `roles.editor.mentionable`, `roles.editor.mentionableHint`, `roles.editor.permissions`, `roles.editor.notHeld`, `roles.editor.save`, `roles.editor.create`, `roles.editor.cancel`, `roles.color.none`, `roles.color.red`, `roles.color.orange`, `roles.color.amber`, `roles.color.green`, `roles.color.teal`, `roles.color.cyan`, `roles.color.blue`, `roles.color.indigo`, `roles.color.purple`, `roles.color.pink`, `roles.color.brown`, `roles.color.grey`, `roles.color.custom`, `perm.group.general`, `perm.group.members`, `perm.group.text`, `perm.group.voice`, `perm.view`, `perm.view.hint`, `perm.send`, `perm.send.hint`, `perm.manageMessages`, `perm.manageMessages.hint`, `perm.pin`, `perm.pin.hint`, `perm.attach`, `perm.attach.hint`, `perm.react`, `perm.react.hint`, `perm.history`, `perm.history.hint`, `perm.connect`, `perm.connect.hint`, `perm.speak`, `perm.speak.hint`, `perm.video`, `perm.video.hint`, `perm.screen`, `perm.screen.hint`, `perm.invite`, `perm.invite.hint`, `perm.kick`, `perm.kick.hint`, `perm.ban`, `perm.ban.hint`, `perm.manageChannels`, `perm.manageChannels.hint`, `perm.manageRoles`, `perm.manageRoles.hint`, `perm.manageServer`, `perm.manageServer.hint`, `perm.auditLog`, `perm.auditLog.hint`, `perm.everyone`, `perm.everyone.hint`, `perm.slowmode`, `perm.slowmode.hint`, `perm.admin`, `perm.admin.hint`, `perm.mute`, `perm.mute.hint`, `perm.move`, `perm.move.hint`, `perm.nicknames`, `perm.nicknames.hint` |
| Editor busy | The submit button shows the busy label and the fields are disabled. | `roles.editor.saving`, `roles.editor.creating` |
| Editor refusals | A `role=alert` error in the dialog. The dialog stays open. | `roles.error.empty`, `roles.error.long`, `roles.error.forbidden`, `roles.error.conflict`, `roles.error.full`, `roles.error.gone`, `roles.error.other` |
| Delete role | A confirm dialog that opens on Cancel, a busy state, a polite done announcement, and an alert on failure. | `roles.deleteTitle`, `roles.deleteBody`, `roles.deleteConfirm`, `roles.deleteBusy`, `roles.deleted`, `roles.error.delete` |
| Assign dialog | A checklist of the server's roles for one member. Roles at or above the viewer's highest role are disabled with the locked note. Each change applies at once. | `roles.menu.assign`, `roles.assign.title`, `roles.assign.body`, `roles.assign.locked`, `roles.assign.close`, `roles.access` |
| Assign, nothing to give | The empty note in place of the checklist. | `roles.assign.none` |
| Assign given, taken and refused | Given and taken are announced politely. Refusals show a `role=alert` error in the dialog and the checkbox returns to its prior state. | `roles.assign.given`, `roles.assign.taken`, `roles.assign.error.forbidden`, `roles.assign.error.twoFactor`, `roles.assign.error.gone`, `roles.assign.error.other` |
| Channel permissions, public | A section in channel settings with the private toggle off, the list of entries (@everyone, roles, people) each with allow, default and deny per permission, an add control and a remove button per entry. | `chperm.title`, `chperm.private`, `chperm.privateHint`, `chperm.who`, `chperm.everyone`, `chperm.member`, `chperm.add`, `chperm.addLabel`, `chperm.remove`, `chperm.removeLabel`, `chperm.none`, `perm.allow`, `perm.inherit`, `perm.deny` |
| Channel permissions, private, and confirms | The toggle on, the private tag beside the channel name. Turning it on or off opens a confirm that opens on Cancel; saving shows the busy label, then the saved announcement. | `chperm.privateTag`, `chperm.makePrivate.title`, `chperm.makePrivate.body`, `chperm.makePrivate.confirm`, `chperm.makePublic.title`, `chperm.makePublic.body`, `chperm.makePublic.confirm`, `chperm.cancel`, `chperm.busy`, `chperm.saved` |
| Permission refusals | A `role=alert` error in the section or confirm. | `chperm.error.forbidden`, `chperm.error.hidden`, `chperm.error.gone`, `chperm.error.unread`, `chperm.error.other` |
| Create private | In the create-channel dialog, a private toggle and, when on, a checklist of roles below the viewer's, or the no-roles note. | `chperm.create.private`, `chperm.create.privateHint`, `chperm.create.roles`, `chperm.create.noRoles`, `chperm.create.tooMany` |
| Lost access and cannot send | A channel that leaves the list (hidden from the viewer, or deleted: the client cannot tell which) is left with a polite, neutral announcement. In a channel where the viewer lacks send messages the message box stays, read-only, with the reason, and the state is announced once. | `chperm.lostAccess`, `chperm.cannotSend` |
| Role mention and pins | The composer offers roles as `@name` with the secondary "role". A rendered role mention shows its name. A pin whose pinner may not pin there is hidden, and the pins list carries the note. | `mention.role`, `mention.role.secondary`, `perm.pinsNote` |
| Server notification rows | One row per server in notification settings: mode (default, all messages, mentions, nothing), mute, and ignore @everyone and @here. | `serverNotify.title`, `serverNotify.body`, `serverNotify.mode.default`, `serverNotify.mode.all`, `serverNotify.mode.mentions`, `serverNotify.mode.nothing`, `serverNotify.mute`, `serverNotify.suppressEveryone` |

## 1280×800

Roles is in the People group of the fullscreen server settings workspace. Navigation and content scroll independently, as specified in [Flow 18](18-fullscreen-settings.md). The roles list fills the content pane, one row per role with its colour dot, name, member count and actions at the row end. The role editor is a dialog; its name, colour, toggles and permission grid sit in one column and only the grid scrolls, under a pinned footer with save and cancel. The assign dialog opens from a member row's menu in the member list. The channel permissions section sits in channel settings below the name, with the private toggle at its top and the entries listed beneath.

## 360×740

The grouped settings index opens Roles as a single section, with Back returning to the index and the app tab bar hidden throughout settings. Role rows stack: name and count above, actions below, with primary touch targets at least 44 px. The role editor and assign dialog fill the viewport width; the permission grid is one column and scrolls inside the dialog while the footer stays pinned. Member actions are available in the Members section and from the contextual member drawer; nested dialogs remain above their opener. The channel permissions section is a full-width column and each entry's allow, default and deny control wraps under its name. Nothing needs horizontal scroll at 200 % zoom.

## Keyboard map

| Where | Keys | Result |
| --- | --- | --- |
| PermissionGrid, toggle mode | Tab, Space | Tab moves between rows; Space toggles the focused row. |
| PermissionGrid, tri mode | Tab, arrow keys | Tab enters a row on its checked radio. The arrow keys move and select within the row, wrapping from the last to the first. Tab leaves the row. |
| ColorSwatches | Arrow keys | The arrows move and select, wrapping. |
| CheckList | Tab, Space | Tab moves between boxes; Space toggles. |
| Member row | Shift+F10 | Opens the row menu, which holds `roles.menu.assign`. |
| Roles list rows | Tab, Enter | Tab moves across each row's buttons; Enter activates. |
| Composer `@` | ArrowUp, ArrowDown, Enter, Tab | The arrows move through the options; Enter or Tab inserts. A role option reads its secondary "role". |
| Dialogs | Escape | Closes the dialog without saving and returns focus to its opener. |

## Focus rules

- The role editor opens on the name field and closes to its opener: `roles.new`, or the row's edit button.
- After a delete, focus goes to `roles.new`.
- After a move, focus stays on the same row's move button, or on its other move button when that one is now hidden.
- The assign dialog opens on the first enabled checkbox, else Done, and closes to the member's menu trigger (through `asideReturnRef` from the narrow drawer).
- The private toggle's confirm opens on Cancel and closes to the toggle.
- An entry's settings disclosure keeps focus on its button.
- After `chperm.lostAccess`, focus follows the normal channel-open rule.
- A confirm dialog with an error sets the error in the same update that clears busy; focus goes to the first field, else Cancel, never the destructive button.

## Announcements

All are polite, through the announcer inside settings while the workspace is open, or the shell announcer for contextual actions outside settings. Dialog errors are `role=alert`.

- `roles.moved`
- `roles.editor.saved`
- `roles.editor.created`
- `roles.deleted`
- `roles.assign.given`
- `roles.assign.taken`
- `chperm.saved`
- `chperm.lostAccess`

## Copy

- `roles.tab='roles'`
- `roles.title='Roles'`
- `roles.body='Roles group what people may do. A higher role outranks the ones below it, and you can change only roles below your own highest role. Everyone in the server, and the host, can see every role, its permissions and who holds it.'`
- `roles.new='new role'`
- `roles.everyone='@everyone'`
- `roles.everyoneHint='Everyone in the server holds this role. Its permissions are where every member starts.'`
- `roles.row.one='1 member'`
- `roles.row.other='{count} members'`
- `roles.locked='above your highest role'`
- `roles.edit='edit'`
- `roles.editLabel='edit {name}'`
- `roles.up='move up'`
- `roles.upLabel='move {name} up'`
- `roles.down='move down'`
- `roles.downLabel='move {name} down'`
- `roles.delete='delete'`
- `roles.deleteLabel='delete {name}'`
- `roles.loading='Loading roles…'`
- `roles.error.load='Could not load the roles ({code}).'`
- `roles.retry='Try again'`
- `roles.moved='{name} moved.'`
- `roles.error.move='Could not move {name} ({code}). Try again.'`
- `roles.editor.titleNew='New role in {server}'`
- `roles.editor.titleEdit='Edit {name}'`
- `roles.editor.name='Role name'`
- `roles.editor.nameHint='Up to 100 characters (fewer for some letters). Everyone in the server, and the host, can read it.'`
- `roles.editor.color='Colour'`
- `roles.editor.colorHint='Shown as a dot beside the names of people who hold it.'`
- `roles.editor.hoist='show separately in the member list'`
- `roles.editor.mentionable='anyone can mention this role'`
- `roles.editor.mentionableHint='People who may mention @everyone can mention any role.'`
- `roles.editor.permissions='Permissions'`
- `roles.editor.notHeld='You do not hold this permission, so you cannot give or take it.'`
- `roles.editor.save='Save'`
- `roles.editor.saving='Saving…'`
- `roles.editor.create='Create'`
- `roles.editor.creating='Creating…'`
- `roles.editor.cancel='Cancel'`
- `roles.editor.saved='{name} saved.'`
- `roles.editor.created='{name} created.'`
- `roles.color.none='no colour'`
- `roles.color.red='red'`
- `roles.color.orange='orange'`
- `roles.color.amber='amber'`
- `roles.color.green='green'`
- `roles.color.teal='teal'`
- `roles.color.cyan='cyan'`
- `roles.color.blue='blue'`
- `roles.color.indigo='indigo'`
- `roles.color.purple='purple'`
- `roles.color.pink='pink'`
- `roles.color.brown='brown'`
- `roles.color.grey='grey'`
- `roles.color.custom='current colour'`
- `roles.error.empty='Give the role a name.'`
- `roles.error.long='That name is too long. A role name is up to 100 characters (fewer for some letters).'`
- `roles.error.forbidden='You cannot change this role. It is at or above your highest role, or it carries a permission you do not hold.'`
- `roles.error.conflict='Someone else moved this role at the same time. Check it and save again.'`
- `roles.error.full='This server already has 250 roles, the most it can hold.'`
- `roles.error.gone='This role no longer exists.'`
- `roles.error.other='Could not save the role ({code}). Try again.'`
- `roles.deleteTitle='Delete {name}?'`
- `roles.deleteBody='Everyone who holds it loses it, and with it any channel it opened for them. This cannot be undone.'`
- `roles.deleteConfirm='Delete'`
- `roles.deleteBusy='Deleting…'`
- `roles.deleted='{name} deleted.'`
- `roles.error.delete='Could not delete {name} ({code}). Try again.'`
- `roles.menu.assign='roles'`
- `roles.assign.title='Roles of {name}'`
- `roles.assign.body='Tick a role to give it to {name}, untick it to take it away. Each change applies at once.'`
- `roles.assign.locked='{role} is at or above your highest role.'`
- `roles.assign.none='There are no roles you can give yet. Make one in Server settings, under roles.'`
- `roles.assign.close='Done'`
- `roles.assign.given='{role} given to {name}.'`
- `roles.assign.taken='{role} taken from {name}.'`
- `roles.assign.error.forbidden='You cannot give or take {role}. It is at or above your highest role, or it carries a permission you do not hold.'`
- `roles.assign.error.twoFactor='{name} needs a second sign-in factor before they can hold a moderator role in this server.'`
- `roles.assign.error.gone='{name} is no longer in this server.'`
- `roles.assign.error.other='That did not work ({code}). Try again.'`
- `roles.access='Roles decide who can see private channels. People who lose a channel keep the messages they already have; people who gain one see only messages sent after they join it.'`
- `perm.group.general='General'`
- `perm.group.members='Members'`
- `perm.group.text='Text'`
- `perm.group.voice='Voice'`
- `perm.allow='Allow'`
- `perm.inherit='Default'`
- `perm.deny='Deny'`
- `perm.view='View channels'`
- `perm.view.hint='See text and voice channels'`
- `perm.send='Send messages'`
- `perm.send.hint='Post in text channels'`
- `perm.manageMessages='Manage messages'`
- `perm.manageMessages.hint='Has no effect in this version.'`
- `perm.pin='Pin messages'`
- `perm.pin.hint='Pin for everyone in a conversation'`
- `perm.attach='Attach files'`
- `perm.attach.hint='Send images and files.'`
- `perm.react='Add reactions'`
- `perm.react.hint='Has no effect in channels the host cannot read in this version.'`
- `perm.history='Read message history'`
- `perm.history.hint='Open the files sent in a channel. Messages sent before someone joins a channel are never shown to them, whatever their roles.'`
- `perm.connect='Connect'`
- `perm.connect.hint='Join voice channels'`
- `perm.speak='Speak'`
- `perm.speak.hint='Be heard in voice'`
- `perm.video='Video'`
- `perm.video.hint='Turn on a camera in calls.'`
- `perm.screen='Share screen'`
- `perm.screen.hint='Share a screen or a tab in calls.'`
- `perm.invite='Create invites'`
- `perm.invite.hint='Make invite links to the server.'`
- `perm.kick='Kick members'`
- `perm.kick.hint='Remove members whose highest role is below theirs. They can come back with an invite.'`
- `perm.ban='Ban members'`
- `perm.ban.hint='Remove members whose highest role is below theirs, and keep them out.'`
- `perm.manageChannels='Manage channels'`
- `perm.manageChannels.hint='Create, edit and move channels'`
- `perm.manageRoles='Manage roles'`
- `perm.manageRoles.hint='Make, edit and give out roles below their own highest role, and set channel permissions.'`
- `perm.manageServer='Manage server'`
- `perm.manageServer.hint='Rename the server.'`
- `perm.auditLog='View audit log'`
- `perm.auditLog.hint='Has no effect in this version.'`
- `perm.everyone='Mention @everyone and @here'`
- `perm.everyone.hint='Notifies the whole server'`
- `perm.slowmode='Bypass slow mode'`
- `perm.slowmode.hint='Send without waiting in channels with slow mode.'`
- `perm.admin='Administrator'`
- `perm.admin.hint='Every permission in every channel, whatever the channel’s settings say. Give it only to people you would trust with the whole server.'`
- `perm.mute='Mute members'`
- `perm.mute.hint='Has no effect in this version.'`
- `perm.move='Move members'`
- `perm.move.hint='Has no effect in this version.'`
- `perm.nicknames='Manage nicknames'`
- `perm.nicknames.hint='Has no effect in this version.'`
- `perm.pinsNote='A pin shows while the person who pinned it may pin in this channel.'`
- `chperm.title='Permissions'`
- `chperm.private='private channel'`
- `chperm.privateHint='Only the roles and people you add can see it, with the owner and administrators. The host still knows who is in it.'`
- `chperm.who='Who has settings here'`
- `chperm.everyone='@everyone'`
- `chperm.member='member'`
- `chperm.add='add a role or person'`
- `chperm.addLabel='Role or person'`
- `chperm.remove='remove'`
- `chperm.removeLabel='remove the settings for {name}'`
- `chperm.none='No role or person has settings of their own here yet.'`
- `chperm.saved='Permissions of #{name} saved.'`
- `chperm.busy='Saving…'`
- `chperm.makePrivate.title='Make #{name} private?'`
- `chperm.makePrivate.body='People who are not given access leave the channel now. They keep the messages they already have.'`
- `chperm.makePrivate.confirm='Make private'`
- `chperm.makePublic.title='Open #{name} to everyone?'`
- `chperm.makePublic.body='Everyone in {server} joins the channel. They see only messages sent after they join.'`
- `chperm.makePublic.confirm='Open to everyone'`
- `chperm.cancel='Cancel'`
- `chperm.error.forbidden='You cannot change this. The role or person is at or above your highest role, or the change names a permission you do not hold here.'`
- `chperm.error.hidden='That change would hide the channel from you. Give access to a role you hold first.'`
- `chperm.error.gone='This channel no longer exists.'`
- `chperm.error.unread='Could not read who can see this channel. Close this and open it again.'`
- `chperm.error.other='Could not save the permissions ({code}). Try again.'`
- `chperm.create.private='private channel'`
- `chperm.create.privateHint='Only you, the owner, administrators and the roles you tick can see it.'`
- `chperm.create.roles='Roles that can see it'`
- `chperm.create.noRoles='There are no roles below yours to add. You can add people later in the channel’s permissions.'`
- `chperm.privateTag='private'`
- `chperm.lostAccess='{name} is no longer available to you.'` ({name} is `#name` for a text channel, the bare name for a voice channel)
- `chperm.create.tooMany='A private channel can be opened to at most 98 roles. Untick some and try again.'`
- `chperm.cannotSend='You cannot send messages in #{name}.'`
- `mention.role='@{name}'`
- `mention.role.secondary='role'`
- `serverNotify.title='Servers'`
- `serverNotify.body='A server’s setting applies to its channels unless a channel has its own.'`
- `serverNotify.mode.default='Default'`
- `serverNotify.mode.all='All messages'`
- `serverNotify.mode.mentions='Mentions'`
- `serverNotify.mode.nothing='Nothing'`
- `serverNotify.mute='Mute server'`
- `serverNotify.suppressEveryone='Ignore @everyone and @here'`
- `perm.moderate='Warn and time out members'`
- `perm.moderate.hint='Warn members or temporarily block their messages and reactions.'`
- `perm.group.music='Music'`
- `perm.group.moderation='Moderation'` (the role editor groups permissions as the prototype does: General, Moderation, Voice and Music, with Control music under Music; channel permissions keep General, Members, Text, Voice and Music)
- `perm.music='Control music (DJ)'`
- `perm.music.hint='Play, queue, skip, stop and set volume with Glenn in voice'`
