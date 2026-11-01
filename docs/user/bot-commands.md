# Bot commands

Use the command composer to run a built-in or managed bot action. In a community
channel, type `/` at the start of the message box to open the command picker. Pick
an entry, then fill its named fields. Required fields and permissions are checked
before you send. Start typing to filter entries; use Up/Down to move, Enter or Tab
to select, and Escape to close the list or leave command mode while keeping your
draft. You can also type a complete command and its named arguments. Normal
mentions, multiline text, IME input, and attachments keep their usual behavior.

Command drafts stay separate from text drafts for each conversation and thread.
A command does not take an unrelated reply or attachment with it. Unknown slash
input stays private. Choose **Send as text**, or prefix the text with `//`, to post
a slash literally. Ordinary messages, edits, history replay, and bot-authored
messages never run commands. The old `/play`-style text execution has been removed.

Results are private to you unless a command explicitly publishes something. A
private result can report pending, success, failure, or uncertain delivery. An
upload receipt alone does not mean the requested action completed. Commands run
through the built-in or managed bot and obey current community, channel, and role
permissions. Inaccessible users, roles, and channels are not suggested.

## Available commands

Fields in angle brackets are required; square brackets are optional. In the
composer they appear as named fields with suggestions where available.

| Area | Commands |
| --- | --- |
| Help and text | `help [command]`, `me <text>`, `shrug [text]`, `tableflip [text]`, `unflip [text]`, `code <language> <text>` |
| Search and conversation | `giphy [query]`, `dm <user>`, `invite`, `search <query> [scope]`, `topic <text>`, `thread <message>`, `reply <message> <text>`, `edit <message> <text>`, `delete <message> [reason]`, `pin <message>`, `unpin <message>` |
| Voice and music | `join <voice-channel>`, `leave`, `mute`, `unmute`, `deafen`, `undeafen`, `play <query-or-url>`, `pause`, `resume`, `skip`, `stop`, `queue`, `nowplaying`, `seek <position>`, `volume <0..100>`, `shuffle <on|off>`, `repeat <off|one|all>`, `queue remove <item>`, `queue move <item> <position>`, `queue clear`, `music` |
| Moderation | `kick <user>`, `ban <user> [duration] [reason]`, `unban <user>`, `timeout <user> <duration>`, `untimeout <user>`, `warn <user> <reason>`, `modlog`, `role add <user> <role>`, `role remove <user> <role>`, `selfrole list`, `selfrole add <role>`, `selfrole remove <role>`, `rolepanel create` |
| Announcements | `announcement list`, `announcement create`, `announcement edit`, `announcement delete` |
| Polls and reminders | `poll create`, `poll vote`, `poll results`, `poll close`, `remind <when> <text>`, `reminders list`, `reminders cancel`, `reminders dismiss` |

Some commands open a prefilled dialog for actions with more choices. Voice/music
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
answers, a question of up to 300 Unicode characters, and answers up to 100
characters. Voting is single-choice by default; the creator can enable multiple
choices. The poll shows named voters. Members can replace or withdraw their vote.

Choose a duration of 1, 4, 8, or 24 hours, or 3 or 7 days; the default is 24 hours.
A community can have at most 128 open polls. The creator or a member with Manage
Messages can close a poll. Closed polls stay readable. Poll records and votes are
authoritative in the encrypted bot state. The bot publishes one authenticated poll
card, with a readable fallback when cards are unavailable.

## Reminders

Reminders are one-time and private to your account across your devices. Choose a
duration or a local date and time; the composer previews the exact time and zone.
Durations must be between 1 minute and 365 days. Times in a daylight-saving gap
are rejected; an ambiguous local time asks you to choose which occurrence you mean.

You can have up to 100 outstanding reminders per community. Due reminders remain
in your inbox until dismissed. The inbox supports upcoming and due lists, cancel,
and dismiss. Reminders are saved before success is shown and catch up after bot
downtime or browser closure. Membership revocation or a disabled bot stops access
and delivery. Browser notifications need the existing notification permission and
are not promised while the browser is closed. Reconnecting and unlocking syncs the
private inbox; notification polling is bounded and staggered across devices.
