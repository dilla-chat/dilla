# Flow 08: server admin and moderation (v0.1.0-web3.4)

## States

| state | shown | copy keys | announcement |
|---|---|---|---|
| members closed | the `members` toggle in the channel header is not pressed; the aside is not rendered | `members.toggle` | none |
| members open, wide (shell at 64rem or more) | the aside is a fourth column beside the main pane; the toggle is pressed; it starts open at this width and is not remembered | `members.toggle`, `members.region`, `members.group` | none |
| members open, overlay (below 64rem) | a drawer from the right, `min(20rem, 100%)` wide; focus moves into it; a backdrop covers the rest and a click on it closes the drawer; the rail, sidebar, main and status bar are inert (and so is the banner) while it is open; `members.close` closes it and Escape does the same from anywhere; on close the composer is reachable again | `members.region`, `members.close` | none |
| members loading | the pane says it is loading | `members.loading` | none |
| members empty | no one but this account | `members.empty` | none |
| members grouped | one group per hoisted role, highest first, then the rest; a crown after the owner and a small bot tag, no other tags | `members.group`, `members.groupMembers` | none |
| card open (click, Enter, right-click, Shift+F10) | the member's profile card (flow 14): a popover beside the row, a sheet on a phone; the actions depend on the viewer's permissions and the target's rank | `members.menu.message`, `members.menu.compare`, `members.menu.kick`, `members.menu.ban` | none |
| kick: idle | confirm dialog; focus on Cancel; the destructive button is not focused | `mod.kick.title`, `mod.kick.body`, `mod.kick.confirm`, `mod.cancel` | dialog title |
| kick: busy | the confirm says its busy label and is disabled | `mod.kick.busy` | none |
| kick: done | the dialog closes; focus returns to the opener | `mod.kick.done` | `mod.kick.done` through the shell announcer |
| kick: refused | the dialog stays open with the error | `mod.kick.error.forbidden`, `mod.kick.error.gone`, `mod.error.other` | the error (`role="alert"`) |
| ban: idle | dialog with a reason field and a length select; focus in the reason field | `mod.ban.title`, `mod.ban.body`, `mod.ban.reason`, `mod.ban.reasonHint`, `mod.ban.duration`, `mod.ban.duration.hour`, `mod.ban.duration.day`, `mod.ban.duration.week`, `mod.ban.duration.month`, `mod.ban.duration.forever`, `mod.ban.confirm`, `mod.cancel` | dialog title |
| ban: busy | the confirm says its busy label and is disabled | `mod.ban.busy` | none |
| ban: done | the dialog closes; focus returns to the opener | `mod.ban.done` | `mod.ban.done` through the shell announcer |
| ban: refused | the dialog stays open with the error | `mod.ban.error.forbidden`, `mod.ban.error.reasonLong`, `mod.ban.error.reasonControl`, `mod.ban.error.clock`, `mod.error.other` | the error (`role="alert"`) |
| settings sections by permission | tabs for overview (manage server), channels (manage channels), invites (create invite) and bans (ban members) show only with that permission; the danger tabs are always shown | `admin.open`, `admin.title`, `admin.nav`, `admin.nav.overview`, `admin.nav.channels`, `admin.nav.invites`, `admin.nav.bans`, `admin.nav.delete`, `admin.nav.leave`, `admin.nav.transfer`, `admin.cancel` | dialog title |
| overview: idle | the server name field with its hint | `admin.overview.title`, `admin.overview.name`, `admin.overview.hint`, `admin.overview.save` | none |
| overview: busy | the save says its busy label | `admin.overview.saving` | none |
| overview: saved | the saved note | `admin.overview.saved` | `admin.overview.saved` in the status region |
| overview: error | the field's error | `admin.overview.error.empty`, `admin.overview.error.long`, `admin.overview.error.refused`, `admin.overview.error.forbidden`, `admin.overview.error.conflict`, `admin.overview.error.other` | the error (`role="alert"`) |
| AFK settings | replaced in v0.1.0-web3.9: the built-in AFK channel; one inactive-timeout row (flow 14) | `admin.afk.title`, `admin.afk.hint`, `admin.afk.channel`, `admin.afk.disabled`, `admin.afk.timeout`, `admin.afk.minutes`, `admin.afk.save`, `admin.afk.saved`, `admin.afk.error`, `admin.afk.loadError`, `admin.afk.restricted` | `admin.afk.saved` through the shell announcer; errors use `role="alert"` |
| Bot welcome settings | available only for a ready installation with the welcome capability and Manage Server; loads operation 1 and saves operation 2 against the returned revision; a success appears only after a correlated Bot response | `admin.bot.welcomeSettings.title`, `admin.bot.welcomeSettings.help`, `admin.bot.welcomeSettings.requiresCapability`, `admin.bot.welcomeSettings.readOnly`, `admin.bot.welcomeSettings.noChannels`, `admin.bot.welcomeSettings.channel`, `admin.bot.welcomeSettings.chooseChannel`, `admin.bot.welcomeSettings.template`, `admin.bot.welcomeSettings.tokens`, `admin.bot.welcomeSettings.load`, `admin.bot.welcomeSettings.loading`, `admin.bot.welcomeSettings.reload`, `admin.bot.welcomeSettings.save`, `admin.bot.welcomeSettings.saving`, `admin.bot.welcomeSettings.saved`, `admin.bot.welcomeSettings.configured`, `admin.bot.welcomeSettings.waiting`, `admin.bot.welcomeSettings.uploadUncertain`, `admin.bot.welcomeSettings.uploadFailed`, `admin.bot.welcomeSettings.responseFailed`, `admin.bot.welcomeSettings.conflict`, `admin.bot.welcomeSettings.readFailed`, `admin.bot.welcomeSettings.invalid` | a successful Bot response shows `admin.bot.welcomeSettings.saved`; errors use `role="alert"` |

| channels: list | channels grouped by category, with edit, move and delete per row and a new category button | `admin.channels.title`, `admin.channels.newCategory`, `admin.channels.edit`, `admin.channels.editLabel`, `admin.channels.up`, `admin.channels.upLabel`, `admin.channels.down`, `admin.channels.downLabel`, `admin.channels.delete`, `admin.channels.deleteLabel`, `admin.channels.noCategory` | none |
| channels: moved | the list in its new order | `admin.channels.moved`, `admin.channels.error.move` | `admin.channels.moved`; a failed move is the error (`role="alert"`) |
| channels: new category | dialog with a name field; focus in the field | `admin.category.title`, `admin.category.name`, `admin.category.hint`, `admin.category.submit`, `admin.category.busy`, `admin.category.error.empty`, `admin.category.error.other` | dialog title; the error (`role="alert"`) |
| channels: edit dialog | name, topic (text channels only) and category (not for a category) | `admin.channel.open`, `admin.channel.titleText`, `admin.channel.titleVoice`, `admin.channel.titleCategory`, `admin.channel.name`, `admin.channel.nameHint`, `admin.channel.topic`, `admin.channel.topicHint`, `admin.channel.category`, `admin.channel.save`, `admin.channel.saving`, `admin.channel.error.empty`, `admin.channel.error.long`, `admin.channel.error.topicLong`, `admin.channel.error.topicControl`, `admin.channel.error.forbidden`, `admin.channel.error.refused`, `admin.channel.error.gone`, `admin.channel.error.other` | dialog title; the error (`role="alert"`) |
| channels: delete confirm | confirm dialog with a per-kind title and body; focus on Cancel | `admin.channelDelete.titleText`, `admin.channelDelete.titleVoice`, `admin.channelDelete.titleCategory`, `admin.channelDelete.body`, `admin.channelDelete.bodyVoice`, `admin.channelDelete.bodyCategory`, `admin.channelDelete.confirm`, `admin.channelDelete.busy`, `admin.channelDelete.error.other` | dialog title; the error (`role="alert"`) |
| invites: make with options | the expiry and max-uses selects, the limits note and the make button | `admin.invites.title`, `admin.invites.new`, `admin.invites.expires`, `admin.invites.ttl.halfHour`, `admin.invites.ttl.hour`, `admin.invites.ttl.sixHours`, `admin.invites.ttl.halfDay`, `admin.invites.ttl.day`, `admin.invites.ttl.week`, `admin.invites.ttl.month`, `admin.invites.uses`, `admin.invites.uses.one`, `admin.invites.uses.n`, `admin.invites.limitsNote`, `admin.invites.create`, `invite.options` | none |
| invites: made | the link is shown by the existing invite link; it cannot be shown again later | `admin.invites.creating`, `admin.invites.codeHidden` | as Flow 07 (`invite.copied`, `invite.blocked`) |
| invites: live list | one row per live link with a revoke button | `admin.invites.row`, `admin.invites.empty`, `admin.invites.revoke`, `admin.invites.revokeLabel` | none |
| invites: revoked or refused | the row leaves the list (a server 404 counts as revoked: the row leaves, the announcement fires, no error is shown); a refusal shows its error | `admin.invites.revoked`, `admin.invites.error.forbidden`, `admin.invites.error.load`, `admin.invites.error.other` | `admin.invites.revoked`; the error (`role="alert"`) |
| bans: loading | the pane says it is loading | `admin.bans.title`, `admin.bans.loading` | none |
| bans: error | the load error with a retry | `admin.bans.error.load`, `admin.bans.retry` | the error (`role="alert"`) |
| bans: empty | nobody is banned | `admin.bans.empty` | none |
| bans: list | one row per ban: who banned, when, until when, and the reason | `admin.bans.by`, `admin.bans.until`, `admin.bans.forever`, `admin.bans.lapsed`, `admin.bans.noReason`, `admin.bans.unknownUser`, `admin.bans.unban`, `admin.bans.unbanLabel` | none |
| bans: unban | confirm dialog; focus on Cancel; on success the row leaves the list (a server 404 counts as success: the row leaves, the announcement fires, no error is shown) | `admin.bans.unbanTitle`, `admin.bans.unbanBody`, `admin.bans.unbanConfirm`, `admin.bans.unbanBusy`, `admin.bans.unbanned` | `admin.bans.unbanned` |
| leave (non-owner) | confirm dialog; focus on Cancel | `admin.leave.open`, `admin.leave.title`, `admin.leave.body`, `admin.leave.confirm`, `admin.leave.busy`, `admin.leave.error.other` | dialog title; `admin.left` once done |
| owner cannot leave | the owner sees why, with delete and hand over beside it | `admin.leave.owner` | none |
| delete with typed name | confirm stays disabled until the typed name matches the server name (NFC, trimmed) | `admin.delete.title`, `admin.delete.body`, `admin.delete.confirmName`, `admin.delete.mismatch`, `admin.delete.confirm`, `admin.delete.busy`, `admin.delete.error.forbidden`, `admin.delete.error.other`, `admin.delete.done` | dialog title; the error (`role="alert"`) |
| hand over | a select of the other members and the typed server name | `admin.transfer.title`, `admin.transfer.body`, `admin.transfer.member`, `admin.transfer.confirmName`, `admin.transfer.confirm`, `admin.transfer.busy`, `admin.transfer.done`, `admin.transfer.none`, `admin.transfer.error.forbidden`, `admin.transfer.error.gone`, `admin.transfer.error.other` | dialog title; the error (`role="alert"`) |
| removed | the banner says it with the dismiss action; the rail drops the server; the route moves to the first listed server or `/`; any admin dialog or confirm for that server closes | `removed.notice`, `shell.banner.dismiss` | `removed.notice` (the banner, `role="alert"`) |
| banned rejoin refused | joining again with a link shows the existing refusal | `join.error.forbidden` | the error (`role="alert"`) |

## 1280×800

## 360×740

## Keyboard map

| starting focus | action | destination |
|---|---|---|
| a member row | Enter, click, Shift+F10 or the ContextMenu key | opens that member's profile card beside the row |
| a member row | right-click | opens the same card beside the row |
| an open card | Escape | closes it; focus returns to the row |
| an open card | Tab | moves through the card's buttons |
| the `members` toggle | Enter/Space | opens or closes the member aside |
| the member drawer (below 64rem) | Escape, from anywhere while the drawer is open | closes the drawer; focus returns to the toggle |
| a settings tab list | Tab | one stop for the whole list (roving focus: ArrowUp, ArrowDown, Home and End move between tabs), then the pane |
| any dialog | Escape | closes it; focus returns to what opened it |
| a dialog field | Enter | submits; while busy, nothing more is sent |

## Focus rules

| transition | focus destination | polite status |
|---|---|---|
| a kick, unban, leave or channel-delete confirm opens | Cancel, never the destructive button | dialog title |
| a dialog with fields opens (ban, new category, edit channel, delete server or hand over with a typed name) | its first field | dialog title |
| a dialog closes (Cancel, Escape or success) | back to what opened it | none |
| a card closes with Escape | the member row | none |
| a card action is run | its dialog takes focus as above; with no dialog, back to the row | none |
| the member drawer opens (below 64rem) | into the drawer | none |
| the member drawer closes with Escape or `members.close` | the `members` toggle | none |
| the sidebar tab order | the server actions first, each its own stop: for a member `invite link`, `server settings`, `leave server`; for an owner `+ channel`, `invite link`, `server settings`; then the tabs, then the rows (a member gets five stops in all). `edit channel` and the `members` toggle live in the channel header actions | none |
| the shell tab order, at 64rem or wider | main pane, then the member aside, then the status bar | none |
| the member drawer is open, below 64rem | Tab stays inside the drawer; on close, focus returns to the `members` toggle (the shell passes it to AppShell, so it also holds when the opener was the page body) | none |
| the `members` toggle changes | unchanged; the toggle's pressed state is the state | none |
| a submit is refused | the field or dialog that holds the error | the error (`role="alert"`) |
| the person is removed from the server | the banner is shown; focus is not moved by the page, unless the open dialog closed, then it goes to the shell | `removed.notice` |

## Announcements

Success is announced through the existing shell announcer, a polite status region that is present from the first render: `mod.kick.done` and `mod.ban.done` after a moderation action, `admin.channels.moved` after a channel is moved, `admin.invites.revoked` after a revoke, `admin.bans.unbanned` after an unban, `admin.left` after the person leaves and `admin.overview.saved` after a rename. `admin.delete.done` and `admin.transfer.done` are announced the same way.

When the person is removed from a server (kicked, banned or its owner deleted it), `removed.notice` is shown in the shell's banner slot with `shell.banner.dismiss` and is announced as an alert (`role="alert"`). A refusal in a dialog is the dialog's `role="alert"`, and the same refusal again is announced again (the dialog's `attempt` changes). A busy label is shown on the button; the dialog's status region does not repeat it. The state of the `members` toggle is carried by `aria-pressed`, so it is not announced separately. Nothing in this flow moves focus without a person's action.

## Copy

Every key below is one string of `packages/web/src/strings/en.ts`, byte for byte.

*Members:*
- `members.toggle='Members'`, `members.region='members of {server}'`, `members.group='{name} — {count}'`
- `members.groupMembers='members'`, `members.groupOffline='Offline'`
- `members.menu.message='Message'`, `members.menu.compare='compare safety numbers'`
- Signal (task 9), the row's person menu and the status line: `members.menu.profile='Profile'`, `members.menu.mention='Mention'`, `members.menu.copy='Copy username'`,
  `members.menu.copied='@{name} copied'`, `members.menu.roles='Edit roles'`, `members.menu.kickName='Kick {name}'`,
  `members.menu.banName='Ban {name}'`, `members.menu.label='actions for {name}'`, `members.inVoice='in voice'`,
  `members.inChannel='in {channel}'`
- Task 11 (Q7), the prototype's Overview card: `admin.overview.afk='Inactive timeout'`, `admin.overview.afkHint='Idle computers in voice move to AFK after'`, `admin.overview.afk.60='1m'`, `admin.overview.afk.300='5m'`, `admin.overview.afk.900='15m'`, `admin.overview.afk.1800='30m'`, `admin.overview.afk.3600='1h'`, `admin.overview.leave='Leave server'`, `admin.overview.leaveHint='You can rejoin with a new invite'`
- Task 11 (Q5), the Bot's line from its player: `members.playingIn='playing in {room}'`, `members.idleIn='idle in {room}'`
- `members.menu.kick='kick'`, `members.menu.ban='ban'`, `members.loading='Loading members…'`
- `members.empty='No one else is in this server yet.'`, `members.close='close members'`

*Moderation:*
- `mod.cancel='Cancel'`
- `mod.kick.title='Kick {name} from {server}?'`
- `mod.kick.body='{name} leaves every channel and call of {server} at once. They can join again with an invite. Messages they already received stay on their devices.'`
- `mod.kick.confirm='Kick'`, `mod.kick.busy='Kicking…'`, `mod.kick.done='{name} was kicked.'`
- `mod.kick.error.forbidden='You cannot kick {name}. Their highest role is at or above yours, or your roles do not allow kicking.'`
- `mod.kick.error.gone='{name} is no longer in {server}.'`
- `mod.ban.title='Ban {name} from {server}?'`
- `mod.ban.body='{name} leaves every channel and call of {server} at once and cannot join again until the ban ends or is lifted. Messages they already received stay on their devices.'`
- `mod.ban.reason='Reason'`, `mod.ban.reasonHint='Optional. Shown to moderators in the ban list, not to {name}. Up to 512 characters (fewer for some letters).'`
- `mod.ban.duration='Ban length'`, `mod.ban.duration.hour='1 hour'`, `mod.ban.duration.day='1 day'`, `mod.ban.duration.week='7 days'`
- `mod.ban.duration.month='30 days'`, `mod.ban.duration.forever='until lifted'`
- `mod.ban.confirm='Ban'`, `mod.ban.busy='Banning…'`, `mod.ban.done='{name} was banned.'`
- `mod.ban.error.forbidden='You cannot ban {name}. Their highest role is at or above yours, or your roles do not allow banning.'`
- `mod.ban.error.reasonLong='That reason is too long. A reason is up to 512 characters (fewer for some letters).'`
- `mod.ban.error.reasonControl='A reason cannot hold control characters other than line breaks and tabs.'`
- `mod.ban.error.clock='The ban length was refused. Check this device’s clock and try again.'`
- `mod.error.other='That did not work ({code}). Try again.'`
- `removed.notice='You are no longer in {server}.'`

*Admin overview:*
- `admin.open='server settings'`, `admin.title='Server settings'`, `admin.nav='server settings sections'`
- `admin.nav.overview='overview'`, `admin.nav.channels='channels'`, `admin.nav.invites='invites'`, `admin.nav.bans='bans'`
- `admin.nav.delete='delete server'`, `admin.nav.leave='leave server'`, `admin.cancel='Cancel'`
- `admin.overview.title='Overview'`, `admin.overview.name='Server name'`
- `admin.overview.hint='Up to 255 characters (fewer for some letters). Everyone in the server, and the host, can read it.'`
- `admin.overview.save='Save'`, `admin.overview.saving='Saving…'`, `admin.overview.saved='Saved.'`
- `admin.overview.error.empty='Give the server a name.'`
- `admin.overview.error.long='That name is too long. A server name is up to 255 characters (fewer for some letters).'`
- `admin.overview.error.refused='That name was refused. Try another.'`
- `admin.overview.error.forbidden='You cannot rename this server.'`
- `admin.overview.error.conflict='Someone else changed the server at the same time. Check the name and save again.'`
- `admin.overview.error.other='Could not save ({code}). Try again.'`
- `admin.afk.title='AFK channel'` (replaced in v0.1.0-web3.9; see flow 14; `admin.afk.hint` is removed in v0.1.0-web3.11)
- `admin.afk.channel='AFK destination'`, `admin.afk.disabled='Off'`, `admin.afk.timeout='Move after'`, `admin.afk.minutes='{n} minutes'` (replaced in v0.1.0-web3.9; see flow 14)
- `admin.afk.save='Save AFK settings'`, `admin.afk.saved='AFK settings saved.'`, `admin.afk.error='Could not save AFK settings ({code}). Try again.'` (replaced in v0.1.0-web3.9; see flow 14)
- `admin.afk.loadError='Could not load AFK settings. Reopen server settings to try again.'`, `admin.afk.restricted='Current AFK destination (restricted)'` (replaced in v0.1.0-web3.9; see flow 14)

*Channels:*
- `admin.channels.title='Channels'`, `admin.channels.newCategory='new category'`, `admin.channels.edit='edit'`
- `admin.channels.editLabel='edit {name}'`, `admin.channels.up='move up'`, `admin.channels.upLabel='move {name} up'`
- `admin.channels.down='move down'`, `admin.channels.downLabel='move {name} down'`, `admin.channels.delete='delete'`
- `admin.channels.deleteLabel='delete {name}'`, `admin.channels.noCategory='no category'`, `admin.channels.moved='{name} moved.'`
- `admin.channels.error.move='Could not move {name} ({code}). Try again.'`
- `admin.category.title='New category in {server}'`, `admin.category.name='Category name'`
- `admin.category.hint='Up to 100 characters (fewer for some letters). A category groups channels in the list; it holds no messages.'`
- `admin.category.submit='Create'`, `admin.category.busy='Creating…'`, `admin.category.error.empty='Give the category a name.'`
- `admin.category.error.other='Could not add the category ({code}). Try again.'`
- `admin.channel.open='edit channel'`, `admin.channel.titleText='Edit #{name}'`, `admin.channel.titleVoice='Edit {name}'`
- `admin.channel.titleCategory='Edit category {name}'`, `admin.channel.name='Channel name'`, `admin.channel.nameHint='Up to 100 characters (fewer for some letters).'`
- `admin.channel.topic='Topic'`
- `admin.channel.topicHint='Shown under the channel name. Up to 1024 characters (fewer for some letters). The host can read it.'`
- `admin.channel.category='Category'`, `admin.channel.save='Save'`, `admin.channel.saving='Saving…'`
- `admin.channel.error.empty='Give the channel a name.'`
- `admin.channel.error.long='That name is too long. A channel name is up to 100 characters (fewer for some letters).'`
- `admin.channel.error.topicLong='That topic is too long. A topic is up to 1024 characters (fewer for some letters).'`
- `admin.channel.error.topicControl='A topic cannot hold control characters other than line breaks and tabs.'`
- `admin.channel.error.forbidden='You cannot change this channel.'`
- `admin.channel.error.refused='That change was refused. Check the fields and try again.'`
- `admin.channel.error.gone='This channel no longer exists.'`
- `admin.channel.error.other='Could not save the channel ({code}). Try again.'`
- `admin.channelDelete.titleText='Delete #{name}?'`, `admin.channelDelete.titleVoice='Delete {name}?'`
- `admin.channelDelete.titleCategory='Delete category {name}?'`
- `admin.channelDelete.body='It is removed for everyone, with its messages and files. Copies someone already saved stay with them. This cannot be undone.'`
- `admin.channelDelete.bodyVoice='It is removed for everyone, and a call in it ends. This cannot be undone.'`
- `admin.channelDelete.bodyCategory='The category is removed; its channels stay, without a category. This cannot be undone.'`
- `admin.channelDelete.confirm='Delete'`, `admin.channelDelete.busy='Deleting…'`
- `admin.channelDelete.error.other='Could not delete ({code}). Try again.'`

*Invites:*
- `admin.invites.title='Invites'`, `admin.invites.new='new invite link'`, `admin.invites.expires='Expires after'`
- `admin.invites.ttl.halfHour='30 minutes'`, `admin.invites.ttl.hour='1 hour'`, `admin.invites.ttl.sixHours='6 hours'`
- `admin.invites.ttl.halfDay='12 hours'`, `admin.invites.ttl.day='1 day'`, `admin.invites.ttl.week='7 days'`
- `admin.invites.ttl.month='30 days'`, `admin.invites.uses='Max uses'`, `admin.invites.uses.one='1 use'`
- `admin.invites.uses.n='{n} uses'`, `admin.invites.limitsNote='An invite lasts at most 30 days and admits at most 1000 people.'`
- `admin.invites.create='make link'`, `admin.invites.creating='Making an invite link…'`, `admin.invites.empty='No live invite links.'`
- `admin.invites.row='by {name}, {used} of {max} uses, expires {date}'`
- `admin.invites.codeHidden='The server keeps only a fingerprint of each link, so a link cannot be shown again. Make a new one to share.'`
- `admin.invites.revoke='revoke'`, `admin.invites.revokeLabel='revoke the link by {name} that expires {date}'`
- `admin.invites.revoked='Invite link revoked.'`
- `admin.invites.error.forbidden='You cannot revoke this link. Only its maker or someone who manages the server can.'`
- `admin.invites.error.load='Could not load the invite links ({code}).'`, `admin.invites.error.other='That did not work ({code}). Try again.'`

*Bans:*
- `admin.bans.title='Bans'`, `admin.bans.loading='Loading bans…'`, `admin.bans.empty='Nobody is banned from {server}.'`
- `admin.bans.by='banned by {name} on {date}'`, `admin.bans.until='ends {date}'`, `admin.bans.forever='until lifted'`
- `admin.bans.lapsed='ended {date}; can join again'`, `admin.bans.noReason='no reason given'`, `admin.bans.unknownUser='a deleted account'`
- `admin.bans.unban='unban'`, `admin.bans.unbanLabel='unban {name}'`
- `admin.bans.unbanTitle='Unban {name}?'`, `admin.bans.unbanBody='{name} can join {server} again with an invite. This does not add them back.'`
- `admin.bans.unbanConfirm='Unban'`, `admin.bans.unbanBusy='Unbanning…'`, `admin.bans.unbanned='{name} was unbanned.'`
- `admin.bans.error.load='Could not load the ban list ({code}).'`, `admin.bans.retry='Try again'`

*Loading and people:*
- `admin.loading='Loading server settings…'`, `admin.member.former='a former member'`

*Leave and delete:*
- `admin.leave.open='leave server'`, `admin.leave.title='Leave {server}?'`
- `admin.leave.body='You leave every channel and call of {server}. To come back you need an invite.'`
- `admin.leave.confirm='Leave'`, `admin.leave.busy='Leaving…'`
- `admin.leave.owner='You own {server}, so you cannot leave it. Hand it to another member or delete it.'`
- `admin.leave.error.other='Could not leave ({code}). Try again.'`, `admin.left='You left {server}.'`
- `admin.delete.title='Delete {server}?'`
- `admin.delete.body='Every channel, message and file of {server} is removed for everyone, and every call in it ends. Copies someone already saved stay with them. This cannot be undone.'`
- `admin.delete.confirmName='Type the server name to confirm'`, `admin.delete.mismatch='That is not the server name.'`
- `admin.delete.confirm='Delete server'`, `admin.delete.busy='Deleting…'`
- `admin.delete.error.forbidden='Only the owner can delete this server.'`
- `admin.delete.error.other='Could not delete the server ({code}). Try again.'`, `admin.delete.done='{server} was deleted.'`

*Transfer:*
- `admin.nav.transfer='hand over server'`
- `admin.transfer.title='Hand {server} to another member'`
- `admin.transfer.body='The member you choose becomes the owner, with every permission. You stay in {server} as a member and can no longer delete it. This cannot be undone by you.'`
- `admin.transfer.member='New owner'`, `admin.transfer.confirmName='Type the server name to confirm'`
- `admin.transfer.confirm='Hand over'`, `admin.transfer.busy='Handing over…'`, `admin.transfer.done='{name} now owns {server}.'`
- `admin.transfer.none='There is no other member to hand the server to yet.'`
- `admin.transfer.error.forbidden='Only the owner can hand over this server.'`
- `admin.transfer.error.gone='That member is no longer in {server}. Choose another.'`
- `admin.transfer.error.other='Could not hand over the server ({code}). Try again.'`

*Bot setup, self-service roles, and text timeouts:*
- `admin.nav.bot='bot'`
- `admin.nav.selfRoles='self-service roles'`
- `admin.bot.title='Bot'`
- `admin.bot.name.label='Bot display name'`
- `admin.bot.name.save='Save bot name'`
- `admin.bot.name.saved='Bot name saved.'`
- `admin.bot.name.invalid='Enter a bot display name.'`
- `admin.bot.loading='Checking bot installation…'`
- `admin.bot.selfRoles.title='Self-service roles'`
- `admin.bot.selfRoles.configure='Allow members to assign these roles'`
- `admin.bot.selfRoles.select='Your roles'`
- `admin.bot.selfRoles.empty='No roles are currently available for self-service.'`
- `admin.bot.selfRoles.saved='Self-service roles updated.'`
- `admin.bot.joinRole.title='Join role'`
- `admin.bot.joinRole.honest='A ready bot assigns this role on future joins. Existing members are unchanged.'`
- `admin.bot.joinRole.requiresBot='A ready bot installation is required to configure this setting.'`
- `admin.bot.joinRole.label='Role to configure'`
- `admin.bot.joinRole.none='No role'`
- `admin.bot.joinRole.save='Save join role setting'`
- `admin.bot.joinRole.saving='Saving…'`
- `admin.bot.joinRole.saved='Join role setting saved.'`
- `admin.bot.welcomeSettings.title='Welcome messages'`
- `admin.bot.welcomeSettings.help='Choose a text channel and the message the bot posts when a member joins.'`
- `admin.bot.welcomeSettings.requiresCapability='Enable the welcome messages capability on a ready bot installation first.'`
- `admin.bot.welcomeSettings.readOnly='Welcome settings are read-only because you need Manage Server.'`
- `admin.bot.welcomeSettings.noChannels='Create a text channel before configuring welcome messages.'`
- `admin.bot.welcomeSettings.channel='Welcome channel'`
- `admin.bot.welcomeSettings.chooseChannel='Choose a text channel'`
- `admin.bot.welcomeSettings.template='Welcome message'`
- `admin.bot.welcomeSettings.tokens='Available placeholders: member, server, channel.'`
- `admin.bot.welcomeSettings.load='Load welcome settings'`
- `admin.bot.welcomeSettings.loading='Waiting for the bot…'`
- `admin.bot.welcomeSettings.reload='Reload settings'`
- `admin.bot.welcomeSettings.save='Save welcome settings'`
- `admin.bot.welcomeSettings.saving='Waiting for the bot…'`
- `admin.bot.welcomeSettings.saved='Welcome settings saved.'`
- `admin.bot.welcomeSettings.configured='The bot will post this message for future joins.'`
- `admin.bot.welcomeSettings.waiting='Request uploaded. Waiting for the bot response; changes are not saved yet.'`
- `admin.bot.welcomeSettings.uploadUncertain='Upload status is uncertain. Waiting for the bot response.'`
- `admin.bot.welcomeSettings.uploadFailed='Could not upload the welcome settings request.'`
- `admin.bot.welcomeSettings.responseFailed='The bot could not apply this welcome settings request.'`
- `admin.bot.welcomeSettings.conflict='Welcome settings changed elsewhere. Reload before saving again.'`
- `admin.bot.welcomeSettings.readFailed='Could not read the Bot response yet. The request remains pending.'`
- `admin.bot.welcomeSettings.invalid='Choose a text channel and enter a message of at most 2,000 UTF-8 bytes.'`
- `admin.bot.status.pending='Pending'`
- `admin.bot.status.joining='Joining'`
- `admin.bot.status.ready='Ready'`
- `admin.bot.status.disabled='Disabled'`
- `admin.bot.status.failed='Failed'`
- `admin.bot.status.unavailable='Status unavailable'`
- `admin.bot.status.refresh='Refresh status'`
- `admin.bot.status.refreshing='Refreshing…'`
- `admin.bot.none='No bot is installed for this server.'`
- `admin.bot.staged='This staged installation has no capabilities yet and cannot perform server actions.'`
- `admin.bot.capabilities='Enabled capability constraints: {names}.'`
- `admin.bot.capabilities.select='Capabilities for this bot'`
- `admin.bot.capabilities.permissionCeiling='Choose only what this bot needs. Disabled options reflect permissions shown here; the server checks your full current permission ceiling again when you save.'`
- `admin.bot.capabilities.permissionUnavailable='Your current permission summary does not allow this capability.'`
- `admin.bot.capabilities.edit='Edit capabilities'`
- `admin.bot.capabilities.save='Save capabilities'`
- `admin.bot.capabilities.cancel='Cancel capability changes'`
- `admin.bot.capabilities.updated='Bot capabilities updated.'`
- `admin.bot.capability.music='music'`
- `admin.bot.capability.welcome='welcome messages'`
- `admin.bot.capability.roles='role management'`
- `admin.bot.capability.announcements='announcements'`
- `admin.bot.capability.warnings='warnings and timeouts'`
- `admin.bot.capability.removal='message removal'`
- `admin.bot.capability.kick='kicking members'`
- `admin.bot.capability.ban='banning members'`
- `admin.bot.capability.requires.music='Requires Connect and Speak in voice channels; the server verifies both.'`
- `admin.bot.capability.requires.roles='Requires Manage Roles.'`
- `admin.bot.capability.requires.warnings='Requires Moderate Members.'`
- `admin.bot.capability.requires.removal='Requires Manage Messages; the server verifies this permission.'`
- `admin.bot.capability.requires.kick='Requires Kick Members.'`
- `admin.bot.capability.requires.ban='Requires Ban Members.'`
- `admin.bot.enable='Enable bot'`
- `admin.bot.disable='Disable bot'`
- `admin.bot.enabling='Enabling…'`
- `admin.bot.disabling='Disabling…'`
- `admin.bot.stepUpMissing='Sign-in security details are still loading. Try again shortly.'`
- `admin.bot.error.forbidden='You cannot manage the bot for this server.'`
- `admin.bot.error.conflict='The installation changed elsewhere. Reload this section and try again.'`
- `admin.bot.error.other='Could not update the bot installation ({code}). Try again.'`
- `admin.bot.enabledNotice='Bot installation enabled.'`
- `admin.bot.disabledNotice='Bot installation disabled.'`
- `admin.bot.music.title='Music services'`
- `admin.bot.music.spotify='Spotify'`
- `admin.bot.music.spotifyClientId='Spotify application client ID'`
- `admin.bot.music.spotifyClientIdHelp='Create a Spotify application, register the callback URL below, then paste its public client ID here. No client secret is needed.'`
- `admin.bot.music.spotifyCallback='Callback URL to register with Spotify'`
- `admin.bot.music.spotifyClientIdInvalid='Enter the client ID from your Spotify application.'`
- `admin.bot.music.spotifyConfigFailed='Could not load the Spotify application setting.'`
- `admin.bot.music.spotifyLink='Link Spotify account'`
- `admin.bot.music.spotifyLinking='Starting Spotify authorization…'`
- `admin.bot.music.callbackDelivered='Authorization delivered to the bot. Return to the server, join a voice channel, and use /play with a Spotify link. The bot will report the result in text chat.'`
- `admin.bot.music.callbackDenied='Spotify authorization was declined. No account was linked.'`
- `admin.bot.music.callbackExpired='This Spotify authorization attempt expired. Start again from server settings.'`
- `admin.bot.music.callbackInvalid='This Spotify callback does not match an active authorization attempt.'`
- `admin.bot.music.callbackFailed='Could not deliver Spotify authorization. Start again from server settings.'`
- `admin.bot.music.callbackRetry='Retry authorization delivery'`
- `admin.bot.music.callbackSignIn='Sign in to the same account to finish Spotify authorization.'`
- `admin.bot.music.callbackReturn='Return to server'`
- `admin.bot.music.callbackCancel='Cancel authorization attempt'`
- `admin.bot.music.callbackCancelling='Cancelling authorization…'`
- `admin.bot.music.callbackCancelled='Spotify authorization was cancelled.'`
- `admin.bot.music.callbackCancelError='Could not cancel this Spotify authorization attempt.'`
- `admin.bot.music.commands='Music commands'`
- `admin.bot.moderationLog.title='Moderation log'`
- `admin.bot.moderationLog.load='Load manual evidence'`
- `admin.bot.moderationLog.loadAutomatic='Load automatic cases'`
- `admin.bot.moderationLog.loading='Sending request…'`
- `admin.bot.moderationLog.waiting='Waiting for the bot response…'`
- `admin.bot.moderationLog.denied='The instance did not grant view audit log. Response details remain unread.'`
- `admin.bot.moderationLog.blocked='A response from another server is waiting. Open that server’s Bot settings to read it first.'`
- `admin.bot.moderationLog.error='Could not load manual moderation evidence. Try again.'`
- `admin.bot.moderationLog.responseUnavailable='The bot could not load manual moderation evidence. Try again.'`
- `admin.bot.moderationLog.empty='No manual moderation evidence was returned.'`
- `admin.bot.moderationLog.caseMeta='{action} · {reason} · {date}'`
- `admin.bot.moderationLog.unknownDate='Date unavailable'`
- `admin.bot.moderationLog.more='Load more'`
- `admin.bot.moderationLog.automaticMore='More automatic cases are available.'`
- `mod.timeout.title='Text timeout for {name}'`
- `mod.timeout.menu='Text timeout'`
- `mod.timeout.body='{name} can still read, join voice, and use direct messages. They cannot post, react, edit, or pin in {server} while timed out.'`
- `mod.timeout.duration='Action'`
- `mod.timeout.duration.tenMinutes='Timeout for 10 minutes'`
- `mod.timeout.duration.hour='Timeout for 1 hour'`
- `mod.timeout.duration.day='Timeout for 1 day'`
- `mod.timeout.duration.clear='Clear existing timeout'`
- `mod.timeout.confirm='Apply'`
- `mod.timeout.busy='Applying…'`
- `mod.timeout.done='Text timeout updated for {name}.'`
- `mod.timeout.error.forbidden='You cannot time out {name}. Check your moderator permission and role rank.'`
- `mod.timeout.error.clock='The timeout expiry was refused. Check this device’s clock and try again.'`
- `admin.bot.rolePanel.title='Publish a role panel'`
- `admin.bot.rolePanel.help='Enter a reaction emoji beside each approved role to include. Leave a role blank to skip it. The ready bot posts a message in the selected channel.'`
- `admin.bot.rolePanel.requiresBot='A ready bot installation with the Roles capability is required to publish a panel.'`
- `admin.bot.rolePanel.channel='Text channel'`
- `admin.bot.rolePanel.chooseChannel='Choose a channel'`
- `admin.bot.rolePanel.roles='Approved roles to include'`
- `admin.bot.rolePanel.emoji='Emoji'`
- `admin.bot.rolePanel.publish='Publish role panel'`
- `admin.bot.rolePanel.publishing='Publishing…'`
- `admin.bot.rolePanel.published='The bot published and registered the role panel.'`
- `admin.bot.rolePanel.pending='The bot has not confirmed the post yet. Keep this panel open while it finishes.'`
- `admin.bot.rolePanel.uncertain='The send result is uncertain. Retry with the same request to safely continue.'`
- `admin.bot.rolePanel.noChannels='Create a text channel before publishing a panel.'`
- `admin.bot.rolePanel.retry='Retry publishing'`
- `admin.bot.rolePanel.invalid='Choose a text channel and at least one approved role with a unique emoji.'`
- `admin.bot.rolePanel.error='Could not publish the role panel ({code}). Try again.'`
- `admin.bot.automod.title='Automatic moderation'`
- `admin.bot.automod.help='The ready bot checks message rate, repeated messages, and mention bursts. Settings are sent to the bot for this server.'`
- `admin.bot.automod.capabilityHelp='Remove actions require Manage Messages and the bot’s message removal capability. Warnings and timeouts require Moderate Members and the bot’s warnings capability. The server checks permissions again when it handles the change.'`
- `admin.bot.automod.defaults='Defaults are disabled and log-only. The bot does not remove messages or timeout members until you choose an action.'`
- `admin.bot.automod.requiresBot='A ready bot installation is required to load or save automatic moderation settings.'`
- `admin.bot.automod.requiresManager='Only current server managers can read or change automatic moderation settings.'`
- `admin.bot.automod.load='Load automatic moderation settings'`
- `admin.bot.automod.waiting='Waiting for the bot response…'`
- `admin.bot.automod.loaded='Automatic moderation settings loaded.'`
- `admin.bot.automod.enabled='Enable automatic moderation'`
- `admin.bot.automod.rate='Message rate action'`
- `admin.bot.automod.duplicate='Repeated message action'`
- `admin.bot.automod.mention='Mention burst action'`
- `admin.bot.automod.action.log='Log only'`
- `admin.bot.automod.action.remove='Remove message'`
- `admin.bot.automod.action.warn='Warn member'`
- `admin.bot.automod.action.timeout='Timeout member'`
- `admin.bot.automod.timeout='Timeout duration in milliseconds'`
- `admin.bot.automod.exemptions='Exempt user IDs (up to 256 unique IDs, each 32 lowercase hexadecimal characters; the encoded settings document has a size limit)'`
- `admin.bot.automod.save='Save automatic moderation settings'`
- `admin.bot.automod.saved='Automatic moderation settings saved.'`
- `admin.bot.automod.conflict='The settings changed at the same time. Reload before saving again.'`
- `admin.bot.automod.conflictCurrent='The server is now at revision {revision}. Reload its settings before saving again.'`
- `admin.bot.automod.loadError='Could not send the settings request. Try again.'`
- `admin.bot.automod.readError='Could not read the bot response. Try loading settings again.'`
- `admin.bot.automod.readDenied='The bot refused to read these settings. Check manager access and bot status.'`
- `admin.bot.automod.saveError='Could not start or send the save request. Check server access and bot status, then try again.'`
- `admin.bot.automod.saveDenied='The bot refused this change. Check your current permissions and bot capabilities.'`
- `admin.bot.automod.uncertain='The send result is uncertain. The same request remains pending; wait for its response before starting another save.'`
- `admin.bot.automod.invalidExemptions='Use up to 256 unique user IDs, each 32 lowercase hexadecimal characters.'`
- `admin.bot.automod.prepareError='Could not prepare these settings locally. Nothing was sent.'`
- `admin.bot.automod.retry='Retry the same save request'`
- `mod.warn.menu='Warn member'`
- `mod.warn.title='Warn {name} in {server}'`
- `mod.warn.body='The warning will be added to the moderation log. Its case is private to moderators and retained for 90 days.'`
- `mod.warn.reason='Reason'`
- `mod.warn.confirm='Warn'`
- `mod.warn.busy='Sending warning…'`
- `mod.warn.done='Warning sent to the moderation log for {name}.'`
- `mod.warn.evidencePending='Warning accepted for {name}; case delivery is pending and will retry.'`
- `mod.warn.error.forbidden='You cannot warn {name}. Check your moderator permission and role rank.'`
- `mod.warn.error.reasonEmpty='Enter a reason for this warning.'`
- `mod.warn.error.reasonLong='That reason is too long. A reason is up to 512 characters (fewer for some letters).'`
- `mod.warn.error.reasonControl='A reason cannot hold control characters other than line breaks and tabs.'`


## Unified fullscreen workspace

Server settings now live at `/c/:communityId/settings/:section`, with a root index
on phones and the first permitted section on desktop. The registry is the single
source for navigation, search and allowed section selection. Members and personal
self-assignable roles are available to ordinary members; actions retain their own
permission and hierarchy checks. Audit-log permission is distinct from server
management. Loading never exposes privileged content. Losing access clears the
inaccessible draft and chooses an allowed section. The moderation journal remains
separate from bot configuration responses.

GIF configuration is its own section. Bot installation, Welcome messages, Join
roles, Music, Automoderation and Moderation log each have their own section. See
[fullscreen settings](18-fullscreen-settings.md) for the coverage inventory and
partial-save, pending-operation and provider-status contracts.

- `admin.bot.welcomeSettings.retry='Retry the same welcome settings request'`
