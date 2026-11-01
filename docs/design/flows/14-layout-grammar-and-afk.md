# Flow 14: layout grammar, Discord layout and the built-in AFK channel (v0.1.0-web3.9, AFK reworked in v0.1.0-web3.11)

## States

| State | UI |
|---|---|
| a new server | the channel list shows `AFK` last, with the voice glyph |
| the AFK channel renamed and moved into a category | |
| idle in voice on desktop (past the inactive timeout) | the sweep cuts the call; the stage or the region that outlives the dock says `afk.moved`, and nothing joins (no AFK call, no microphone, no dock) |
| the AFK row | no call, no LiveKit room and no microphone; its away roster: each person away there, with no voice connection, listed under the row with `afk.away` and counted; a click leaves the current call and marks the person away |
| the AFK channel's page | the away roster as on its row (`afk.away`), or `afk.empty`; the `afk.go` button, which leaves the current call and then marks the person away |
| the AFK page while away | no button; the line `afk.youAway` |
| going AFK failed | the line `afk.failed`; the call left stays left |
| AFK chosen on another device | the call here is cut and its stage (or the region that outlives the dock) says `afk.elsewhere`, a calm notice, never a removal |
| a phone in voice | never cut for inactivity |
| coming back from away | joining any voice channel, sending a message or typing in that community, or having no connection for 30 seconds (a reload or a short network drop keeps away); there is no leave-AFK action |
| server settings → overview | one `afk.timeout.label` row with the five `afk.timeout.*` options and `afk.timeout.hint`, inside the overview's one save footer |
| channel settings of the AFK channel | name and category only, plus `afk.channel.note`; server settings → channels: no delete on the AFK row |
| the rail | the `@` tile (`home.tile`, `home.tileLabel`), a hairline, the servers, one dashed "+" tile (`layout.rail.add`) that opens one dialog (`layout.add.title`) with two choices (`layout.add.join`, `layout.add.create`) |
| `@` selected on desktop | the DM list in the sidebar |
| a server with categories | 32px category headers, one collapsed (`aria-expanded="false"`) keeping its current and unread rows, the trailing "+" (`layout.category.add`) for managers |
| a voice row | one click joins on desktop; the trailing join icon (`voice.join`); participants with 24px avatars, the speaking ring and the mute and deafen glyphs in the slot |
| the dock | the channel name (its tooltip `layout.dock.where`) over the link stats (flow 06, MP5), then camera, share and leave; no meter |
| the header at 1440 | the icon cluster |
| the header below 36rem | pins and threads folded into `layout.header.more` |
| the members aside | hoisted roles, `layout.members.online`, `layout.members.offline` dimmed, two-line rows |
| the message log | grouping within 7 minutes, the date divider (`layout.log.today`, `layout.log.yesterday`, else the day), and `layout.log.older` with `layout.log.jump` |
| the phone | tabs Home (`home.tab`), Activity, You; `/dm` lands on Home with `@` selected |
| the phone PhoneHeader | a 48 PhoneHeader on each tab root with its lone actions |
| the phone with typing | typing with the call up shows a dot after the title instead of the call bar |
| a message tap-to-reveal | tap a message to reveal its toolbar; a second tap, Escape or a scroll hides it |
| a tooltip | on hover (400 ms) and on focus (at once), with its KeyHint chord, dismissed by Escape |

## Keyboard map

- Category header: Enter or Space toggles, `aria-expanded` follows
- The rail: ArrowUp/ArrowDown move, the `@` tile is first
- A voice row: Enter opens it, the trailing join icon is its own stop and joins
- The message log: `Ctrl+End` jumps to present
- A phone message row: Enter reveals its toolbar, Escape hides it
- A tooltip: Escape dismisses it without moving focus

## Focus rules

- Joining from the trailing icon keeps focus on it
- The sweep’s cut does not move focus
- Collapsing a category keeps focus on its header
- The `@` tile keeps focus when the sidebar swaps to DMs

## Announcements

- The sweep's cut announces "Moved to {channel}" with the channel's name, once, and joins nothing
- `afk.elsewhere` is a calm info notice, never an error
- Nothing else new

## Copy

`afk.timeout.label='inactive timeout'`
`afk.timeout.60='1 minute'`
`afk.timeout.300='5 minutes'`
`afk.timeout.900='15 minutes'`
`afk.timeout.1800='30 minutes'`
`afk.timeout.3600='1 hour'`
`afk.timeout.hint='Members idle in voice this long are disconnected and listed as away in {channel}. Phones are not disconnected.'`
`afk.channel.note='This is the server’s AFK channel. Nobody is connected here; people who are away are listed.'`
`afk.moved='Moved to {channel} for inactivity'`
`afk.away='away'`
`afk.go='go AFK'`
`afk.youAway='You’re away. Join a voice channel, or send a message or type in this server, to come back.'`
`afk.empty='Nobody is away.'`
`afk.failed='Could not go AFK. Try again.'`
`afk.elsewhere='You went AFK on another device.'`
`layout.dock.where='{channel} · {server}'`
`layout.members.online='online'`
`layout.members.offline='offline'`
`layout.members.bots='bots'`
`layout.profile.web='WEB'`
`layout.profile.owner='owner'`
`layout.profile.you='you'`
`layout.profile.roles='roles'`
`layout.profile.close='close'`
`layout.profile.bot='bot'`
`layout.profile.manageRoles='edit roles'`
`layout.profile.formerMember='former member'`
`presence.online='online'`
`presence.idle='idle'`
`presence.dnd='do not disturb'`
`presence.offline='offline'`
`presence.invisible='invisible'`
`layout.log.older='viewing older messages'`
`layout.log.jump='jump to present'`
`layout.log.today='today'`
`layout.log.yesterday='yesterday'`
`layout.header.more='more'`
`layout.category.add='new channel in {name}'`
`layout.rail.add='add a server'`
`layout.row.new='{n} new'`
`layout.sidebar.online='{n} online'`
`layout.header.threads='Threads'`
`layout.header.members='Members'`
`layout.header.membersCount='Members, {n}'`
`layout.add.title='add a server'`
`layout.add.join='join a server'`
`layout.add.create='new server'`
`layout.ptt.tag='PTT'`
`layout.search.hint.move='move'`
`layout.search.hint.open='open'`
`home.tile='direct messages'`
`home.tileLabel='direct messages, unread {unread}, mentions {mentions}'`
`home.tab='Home'`
