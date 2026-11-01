# Flow 12: phone shell (v0.1.0-web3.7)

The mobile app shell on phones (390px and narrower): a navigation tab bar at the bottom, a banner for link status, a header with the page title and actions, a composer above the keyboard, and a content pane. No zoom on any input field, no status bar, visible focus, 44px targets. Replaced in v0.1.0-web3.9: Home, Activity, You, with DMs as the `@` tile (flow 14).

## States

| State | What shows | Copy keys |
|---|---|---|
| 1. Servers list | Replaced in v0.1.0-web3.9: Home, Activity, You, with DMs as the `@` tile (flow 14). | `nav.tabs.label`, `nav.tab.servers`, `nav.tab.dms`, `nav.tab.activity`, `nav.tab.you`, `nav.tab.unread` |
| 2. Server menu open | A menu for server actions. | `nav.server.menu` |
| 3. Channel view pushed | A channel is opened. The header shows the channel name, a back button (`nav.back`), and a menu (`nav.channel.menu`). The tab bar is hidden. | `nav.back`, `nav.channel.menu` |
| 4. Keyboard up | Composer is above the keyboard. Focus and tab management keep the composer accessible. Tab bar remains hidden. | — |
| 5. DMs list | Replaced in v0.1.0-web3.9: Home, Activity, You, with DMs as the `@` tile (flow 14). | `nav.tab.dms` |
| 6. DM view | A direct message conversation is opened. Similar to channel view but for a DM. | `nav.back`, `nav.channel.menu` |
| 7. Activity tab, mentions and DMs | The Activity tab is selected (Task 11, Jonas Q3: the prototype's). Title `activity.title`. One list of avatar rows, "where" over "who text": each due reminder (`activity.reminder`), the mentions (`activity.where`, `activity.mention`) and direct messages (`activity.dm`), newest first, then what is unread elsewhere (`activity.newCount`, `activity.mentionOne`, `activity.mentionMany`). | `activity.title`, `activity.where`, `activity.mention`, `activity.dm`, `activity.newCount` |
| 8. Activity empty | No mentions or DMs. Shows the empty message. | `activity.empty` |
| 9. Activity unread elsewhere | Folded into the one list (Task 11): a channel by its # glyph ("Midgard Crew · #loot", "#loot 1 new"), a DM by its person. | `activity.where`, `activity.newCount` |
| 10. You tab | The You tab is selected (Task 11, Jonas Q3: the prototype's). A hidden heading `you.title`; the account with `you.line`; one box with `you.connected` (the gateway heartbeat's round trip) or `you.connecting` / `you.offline`, and `you.device`; the rows Settings (`you.settings`), Status (`you.statusRow`, opening the four presence choices) and Sign out (`workspace.signOut`). The node, link, generation and devices facts are Settings → Diagnostics'. | `you.title`, `you.line`, `you.connected`, `you.device`, `you.settings`, `you.statusRow` |
| 11. Offline | An offline banner shows: `nav.link.offline`. | `nav.link.offline` |
| 12. Connecting | A connecting banner shows: `nav.link.connecting` with the node name. | `nav.link.connecting` |
| 13. Call bar | A mini call bar at the bottom with open call status and controls. Shows `nav.call.open` with the location where the call is (e.g., the server or channel name), and reuses keys `voice.mute` and `voice.leave`. | `nav.call.open`, `voice.mute`, `voice.leave` |
| 14. Call stage and members sheet | The call is active. The stage shows video or avatars, and a sheet shows members. | `voice.mute`, `voice.leave` |
| 15. Key step, backup code hint | Cross-reference to flow 03: when entering a backup code in the sign-in form, a hint appears below the field. The key is `signIn.key.isBackupCode`. | `signIn.key.isBackupCode` |

## 390×844

The grid is banner / pane / call bar / tab bar. The tab bar is hidden on pushed views. Safe areas pad the header, tab bar and composer. Fields are 16px. No status bar.

## 1280×800

Desktop layout unchanged from flow 04 and flow 06, except the dock shows no controls while the stage is open.

## Keyboard map

| Key | Where | Effect |
|---|---|---|
| Tab | Tab bar | Move through tabs in order. |
| Enter or Space | Tab link | Activates the tab. |
| Escape | Sheet | Closes the sheet. On a pushed view, back is the first control. |

## Focus rules

- Pressing a tab link focuses that tab's list heading.
- When a view (e.g., a channel) is pushed, focus moves to its heading.
- Pressing back focuses the row that opened the view (if still listed), or the list heading if the row is no longer visible.
- A tab switch focuses the new tab's list heading.

## Announcements

The link banner (`nav.link.offline`, `nav.link.connecting`) is `role="status"`. A tab switch is not announced (the heading takes focus instead).

## Copy

- `nav.tabs.label='sections'`
- `nav.tab.servers='servers'`
- `nav.tab.dms='DMs'`
- `nav.tab.activity='Activity'` (Ruling AA, Jonas 2026-10-27: the prototype's case)
- `nav.tab.you='You'`
- `nav.tab.unread='{name}, {count} new'`
- `nav.back='back'`
- `nav.server.menu='server menu'`
- `nav.channel.menu='channel menu'`
- `nav.channel.menuOf='{name} menu'` (Ruling X: a channel or DM row's context menu, named after its row)
- `nav.call.open='open the call in {where}'`
- `nav.link.offline='Offline. Reconnecting…'`
- `nav.link.connecting='Connecting to {node}…'`
- `activity.title='Activity'`
- `activity.empty='Nothing new. Mentions and direct messages show up here.'`
- `activity.mention='mentioned you: {text}'`, `activity.mentionEmpty='mentioned you'`, `activity.where='{server} · #{channel}'`, `activity.newCount='{n} new'`, `activity.mentionOne='1 mention'`, `activity.mentionMany='{n} mentions'`
- `activity.dm='Direct message'`
- `activity.emptyTitle='Nothing new'`
- `activity.reminder='Reminder · {where}'` (a due reminder's row, first on the tab, as the prototype lists it)
- `you.title='You'`, `you.line='@{handle} · {status}'`, `you.connected='Connected to {host} · {ms} ms'`, `you.connectedNoPing='Connected to {host}'`, `you.connecting='Connecting to {host}…'`, `you.offline='Offline · reconnecting to {host}'`, `you.device='{browser} on {os} · device {id}'`, `you.deviceUnknown='This browser · device {id}'`, `you.statusRow='Status'`
- `you.settings='Settings'`
- `signIn.key.isBackupCode='That looks like a backup code, not the recovery key. This step needs the 52-character recovery key you saved when you created the account.'`


## Fullscreen settings

User and server settings use the existing phone-layout breakpoint, including
landscape/coarse-pointer handling. They are native modal workspaces; the app tab
bar is hidden. The grouped index opens a section with Back to the index. Close
returns to the originating screen. A direct section URL has an index fallback.
The shell and active voice session stay mounted. Incoming-call controls are inside
settings; Return to call uses the same unsaved-draft guard as every other exit.
Visual viewport sizing and safe areas keep the Save/Reset footer above the keyboard.
See [the complete settings contract](18-fullscreen-settings.md).
