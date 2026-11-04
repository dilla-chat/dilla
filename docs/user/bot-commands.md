# Bot commands

Use the command composer to run a built-in or managed bot action. In a community
channel, type `/` at the start of the message box to open the command picker. Pick
an entry, then fill its inline chips. The list groups commands as Built-in and Bot,
with an icon and description for every available command. Required fields and permissions are checked
before you send. Start typing to filter entries; use Up/Down to move, Enter or Tab
to select, and Escape to close the list or leave command mode while keeping your
draft. You can also type a complete command and its named arguments. Normal
mentions, multiline text, IME input, and attachments keep their usual behavior.

Command drafts stay separate from text drafts for each conversation and thread.
A command does not take an unrelated reply or attachment with it. Unknown slash
input stays private. Choose **Send as text**, or prefix the text with `//`, to post
a slash literally. Ordinary messages, edits, history replay, and bot-authored
messages never run commands.

Results are private to you unless a command explicitly publishes something. A
private card shows **Working**, **Done** or **Failed**. Bot results use the bot’s
name (Glenn by default) and BOT tag; built-in results use **dilla** and the D tile.
An uncertain result uses **Failed** with text explaining what may have happened. An
upload receipt alone does not mean the requested action completed. Commands run
through the built-in or managed bot and obey current community, channel, and role
permissions. Inaccessible users, roles, and channels are not suggested.

## Available commands

Fields in angle brackets are required; square brackets are optional. In the
composer they appear as named chips with suggestions where available. Move
between chips with Tab or Shift+Tab. Errors appear in the command bar and mark
the active chip. Escape leaves command mode and keeps your text.

| Area | Commands |
| --- | --- |
| Help and text | `help [command]`, `me <text>`, `shrug [text]`, `tableflip [text]`, `unflip [text]`, `code <language> <text>` |
| Search and conversation | `giphy [query]`, `dm <user>`, `invite`, `search <query> [scope]`, `topic <text>`, `thread <message>`, `reply <message> <text>`, `edit <message> <text>`, `delete <message> [reason]`, `pin <message>`, `unpin <message>` |
| Voice and music | `join <voice-channel>`, `leave`, `mute`, `unmute`, `deafen`, `undeafen`, `play <query-or-url>`, `pause`, `resume`, `skip`, `stop`, `queue`, `nowplaying`, `seek <position>`, `volume <0..100>`, `shuffle <on|off>`, `repeat <off|one|all>`, `queue remove <item>`, `queue move <item> <position>`, `queue clear`, `music` |
| Moderation | `kick <user>`, `ban <user> [duration] [reason]`, `unban <user>`, `timeout <user> <duration>`, `untimeout <user>`, `warn <user> <reason>`, `modlog`, `role add <user> <role>`, `role remove <user> <role>`, `selfrole list`, `selfrole add <role>`, `selfrole remove <role>`, `rolepanel create` |
| Announcements | `announcement list`, `announcement create`, `announcement edit`, `announcement delete` |
| Polls and reminders | `poll create`, `poll vote`, `poll results`, `poll close`, `remind <when> <text>`, `reminders list`, `reminders cancel`, `reminders dismiss` |

![The command list](screenshots/signal-command-list.png)

![A reminder’s inline chips](screenshots/signal-command-chips.png)

![A private command result](screenshots/signal-private-result.png)

**/kick** and **/ban** ask you to confirm, with the person’s avatar and a Logged row.
Ban also shows Duration and Reason; Kick has neither. **Cancel** adds a private
**Not confirmed** row only for Kick and Ban. Closing another command’s dialog
abandons it without that row. A permanent ban has no end date.

Some commands open a dialog for actions with more choices. Voice/music
commands target the voice channel you joined. Music playback selects the first
search result or plays a supplied URL or playlist. Owners and administrators can
control playback; anyone else needs the **Control music (DJ)** role permission and
**View channels** in that voice channel. The bot needs access
to connect and speak in the voice channel. See [Music](music.md) for setup and
playback details.

## Turning commands off

**Server settings → Bot** has a switch for each part of the bot: Music, Polls,
Reminders, Welcome messages, Roles, Announcements, Warnings and timeouts, Message
removal, Kick and Ban. Turning one off removes its commands from the picker. For
Polls and Reminders only making new ones stops: polls already posted can still be
voted on and closed, and reminders already set are still delivered and can be
listed, cancelled and dismissed. Saving a change asks for your password or second
factor.

The same page shows the bot's card ("Managed bot · online · v0.4.2"; the version
shows once the bot reports it). **Restart** asks the bot to restart; it does so the
next time it checks in with the server, and the page says "Restarting…" until it
has. Pending results recover on their own. The card's menu (right-click, a long
press or Shift+F10) has **Rename**, **Refresh status** and **Disable**.

## Polls

Create polls in bot-enabled community channels and threads. A poll has 2–10
answers, a question of up to 300 Unicode code points, and answers up to 100
code points. The counter matches the server: 🔥 counts as one code point, while a combined
emoji can contain several. Open **/poll create**, type the question and answers, then press **Post poll**.
Use **Add answer** to add answers and the remove control beside an answer to
remove it.

![Create a poll](screenshots/signal-poll-dialog.png)

Voting is single-choice by default; the creator can enable multiple
choices. The poll shows named voters. Members can replace or withdraw their vote.

![A poll card](screenshots/signal-poll-card.png)

The poll message names its author. The card shows time left, answer bars, your choices and a voter count.
For multiple choices it counts distinct people across answers. A bounded read
may show **{n}+ voters**: at least that many people voted. If a vote is uncertain,
“Your vote may not have counted. Tap it again to retry.” appears. Retry only the
answer you tapped; the other answers wait until it settles.

Choose a duration of 1, 4, 8, or 24 hours, or 3 or 7 days; the default is 24 hours.
A community can have at most 128 open polls. The creator or a member with Manage
Messages can close a poll. Closed polls stay readable. Poll records and votes are
authoritative in the encrypted bot state. The bot publishes one authenticated poll
card, with a readable fallback when cards are unavailable.

## Reminders

Reminders are one-time and private to your account across your devices. Choose a
duration or a local date and time; the composer previews the exact time in your browser’s time zone.
Durations must be between 1 minute and 365 days. Times in a daylight-saving gap
are rejected; an ambiguous local time asks you to choose which occurrence you mean.

You can have up to 100 outstanding reminders per community. Due reminders remain
in your inbox until dismissed. The inbox supports upcoming and due lists, cancel,
and dismiss. Reminders are saved before success is shown and catch up after bot
downtime or browser closure. Membership revocation or a disabled bot stops access
and delivery. Browser notifications need the existing notification permission and
are not promised while the browser is closed. Reconnecting and unlocking syncs the
private inbox; notification polling is bounded and staggered across devices.
