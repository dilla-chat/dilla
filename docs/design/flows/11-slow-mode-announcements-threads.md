# Flow 11: slow mode, announcements and threads (v0.1.0-web3.7)

Slow mode on a per-channel basis limits how fast people can post, configurable by channel admins and bypassable by roles. Announcements are channels where only selected roles can post, but everyone can read and react. Threads are replies grouped under a root message, with summary counts and a dedicated pane or sheet.

## States

| State | What shows | Copy keys |
|---|---|---|
| 1. Channel settings, slow mode off | Settings dialog for a text channel. Under the topic, a Select for slow mode with off, 5 s, 10 s, 30 s, 1 min, 5 min, 15 min, 1 h, 6 h. The default is off. | `slow.label`, `slow.hint`, `slow.off` |
| 2. Slow mode set, each step | The Select shows one of the steps and the label beside it. | `slow.s5`, `slow.s10`, `slow.s30`, `slow.m1`, `slow.m5`, `slow.m15`, `slow.h1`, `slow.h6` |
| 3. Channel list with slow mode | A channel row in the list with a badge below or next to the name when slow mode is on. | `slow.badge` |
| 4. Just sent, countdown | After a post in a channel with slow mode, a notice in the composer area shows the remaining wait. | `slow.wait` |
| 5. Send queued behind slow mode | A notice above the composer saying a send is queued and waiting for slow mode to expire. | `slow.queued` |
| 6. Bypass: no countdown | A person with bypass_slowmode sees no countdown or queue for their own posts, and the notice does not show. | none: a person with bypass_slowmode sees no countdown |
| 7. New channel, announcement kind | In create channel, a kind selector offers text and announcement. Announcement is selected: it sets the kind and shows roles checkboxes for who can post. | `announce.kind`, `announce.hint`, `announce.roles` |
| 8. Announcement channel, reader | A person without post permission views the channel and sees a read-only note. | `announce.readOnly` |
| 8b. New channel, announcement, no held role (removed in web3.8) | Removed: the held-role refusal, where a creator who is not the owner or administrator had to tick a role below their highest role. The creator now always posts with `send_messages` (R5). | — |
| 8d. New channel, announcement refused by the server | The creator ticked a role at or above their highest role and the server refuses with 403. The name field says which role to pick, not that channels cannot be added. | `announce.roleTooHigh` |
| 8e. New channel, no manage roles | A creator with manage channels but not manage roles is not offered the announcement kind; one line in the dialog says why. | `announce.needsRoles` |
| 8c. New channel, announcement, no roles | The server has no role besides @everyone: the roles list says so; the channel can still be created (only the owner and administrators post). | `announce.noRoles` |
| 9. Message toolbar, thread item | A toolbar item for each message offers a reply-in-thread action. | `thread.open` |
| 10. Thread summary chip | A chip or badge on a message shows the reply count: 1 reply or N replies. | `thread.summary.one`, `thread.summary.other` |
| 11. Thread pane or sheet | The thread view at full width (pane in the aside) or full screen (sheet on phone), with the title "Thread", a close button (text `thread.close`), and a composer for replies below the message thread. | `thread.title`, `thread.close`, `thread.composer` |
| 11b. Thread pane, no replies | A thread with a root and no replies shows a line in the reply log. Thread rows take reactions, and edit and delete on a person's own replies; pin and reply stay in the channel. | `thread.noReplies` |
| 12b. Thread list, load failed | The read of the channel's threads was refused: the list says so instead of staying blank. | `thread.loadFailed` |
| 12. Thread list, empty and root missing | A list of all threads in a channel. Empty state: "No threads in #{name} yet." The root message may not be reachable: "The message that started this thread cannot be shown here." | `thread.list`, `thread.empty`, `thread.rootMissing` |

## 1280×800

The thread pane replaces the member list in the aside (one aside). The slow-mode Select sits under the topic in channel settings. The countdown is the composer notice.

## 390×844

The slow-mode Select is a native select in the settings sheet. The thread is a full-screen sheet over the channel, with its own header and back button (`thread.close`), and the composer at the bottom. The tab bar is hidden while the sheet is open.

## Keyboard map

| Key | Where | Effect |
|---|---|---|
| Arrow keys | Message toolbar | Move through toolbar items, including the thread item. |
| Enter or Space | Thread item | Opens the thread pane or sheet. |
| Escape | Thread pane or sheet | Closes it and returns focus to the message that opened it. |
| Tab | Settings Select | Move to the next control. |

## Focus rules

- Opening a thread focuses its composer in the desktop aside, and its back control on the phone sheet and the narrow overlay (the sheet convention).
- Closing returns focus to the message toolbar item or chip that opened it.

## Announcements

Polite, through `useAnnouncer`: `slow.queued` when a send is queued behind slow mode. `slow.wait` is shown in the UI, not announced on every countdown tick.

## Copy

- `slow.label='Slow mode'`
- `slow.hint='How long each person waits between messages. People who may bypass slow mode do not wait.'`
- `slow.off='off'`
- `slow.s5='5 seconds'`
- `slow.s10='10 seconds'`
- `slow.s30='30 seconds'`
- `slow.m1='1 minute'`
- `slow.m5='5 minutes'`
- `slow.m15='15 minutes'`
- `slow.h1='1 hour'`
- `slow.h6='6 hours'`
- `slow.wait='Slow mode is on. You can send again in {time}.'`
- `slow.queued='Waiting for slow mode, then sending.'`
- `slow.badge='slow mode'`
- `announce.kind='announcement'`
- `announce.hint='Only the roles you tick can post. Everyone can read and react.'`
- `announce.roles='Roles that can post'`
- `announce.readOnly='Only some people can post in #{name}.'`
- `announce.noRoles='No roles yet. Make one in Server settings, under roles.'`
- `announce.roleTooHigh='Pick a role below your highest role.'`
- `announce.needsRoles='Announcement channels need the manage roles permission.'`
- `thread.open='reply in thread'`
- `thread.summary.one='1 reply'`
- `thread.summary.other='{count} replies'`
- `thread.summary.last='last {time}'` (Signal task 7: after the count, seen only)
- `thread.title='Thread'`
- `thread.close='close thread'`
- `thread.composer='Reply in thread'`
- `thread.list='threads'`
- `thread.empty='No threads in #{name} yet.'`
- `thread.loadFailed='Threads could not be loaded.'`
- `thread.noReplies='No replies yet.'`
- `thread.rootMissing='The message that started this thread cannot be shown here.'`
