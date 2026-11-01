# Music in chat

Enable the bot's Music capability in **Server settings → Bot**. Who may DJ is a
role permission: **Control music (DJ)**, under **Music** in a role's permissions.
Owners and administrators always can. The bot needs permission to connect and speak
in the voice channel. Spotify account linking is in **Server settings → Music**;
YouTube needs the host's yt-dlp playback tools, and its row there says whether they
were found ("yt-dlp found on the host · playback ready") with **Check again**.

To let another member control music without making them an administrator:

1. Under **Server settings → Roles**, pick a role (or **New role**) and switch on
   **Control music (DJ)**. It saves at once.
2. Give that role to the member.

**Server settings → Music** lists the roles that have Control music ("Roles with
Control music: DJ. Owners and admins always can.") and **Edit in Roles** opens the
first of them.

The permission is checked for the voice channel you are in, with that channel's
permission settings: a channel can switch **Control music (DJ)** off for a role (or
on for one person) in that channel only. A DJ also needs **View channels** in that
voice channel: someone who cannot see the channel cannot control its music.

Control music is an ordinary permission, like the others in a role. Anyone who may
manage roles can give it to a role below their own highest role, or switch it on for
a role or a person in one channel's permission settings, as they can with any
permission they hold themselves. Servers that had approved DJ roles under the
old Music page keep them: on upgrade, every role the server's bot had approved got
**Control music (DJ)**.

The Music page also sets two defaults for the bot (people who manage the server
change them; they save as you pick them):

- **Default volume** (25, 50, 75 or 100, "For new sessions"): a track started while
  nothing plays or waits starts at this volume. A DJ can still change the volume.
- **Leave voice when idle** (5m, 15m or 30m): once nothing has played for this long
  (after Stop or Pause, or when the queue ends), the bot leaves the voice channel:
  it disconnects from the call, as it does when its connection ends any other way,
  and drops off the channel's list of people in voice. The next play brings it back.

Until someone changes them, they are 100 and 30 minutes.

Join a voice channel and open the command picker by typing `/` at the start of a
community message. Choose a music command and fill its named fields, such as the
search query or URL for `play`. The composer validates required arguments and
permissions before sending. The complete music command list is in
[Bot commands](bot-commands.md).

Music results are private to the requester. Playback itself is heard in the
voice channel. One thing is public: when your `/play` starts the music, the bot
posts "{you} started music in {voice channel}" in the channel where you ran it,
with a live player card (title, who asked for it, progress, and the queue under
**Up next**). Everyone in that channel sees it. The card follows that track and
shows it finished once another plays or the bot leaves voice. The bot posts it
only for the person whose track just started, at most once a minute, and only in
a channel where that person may post. Queuing more tracks, pausing, skipping and
the other commands stay private.

While you are in the voice channel with the bot, the same player sits in your
voice panel and at the top of the voice stage. Its controls work for DJs, owners
and administrators who are in that voice channel; anyone else sees a note saying
why. Commands target the voice channel the requester joined. Playback changes
require **Control music (DJ)** in that voice channel (owners and administrators have
it), and presence in that voice channel. The bot needs permission to connect and speak. Polls, reminders, and
other command results follow their own visibility rules in [Bot commands](bot-commands.md).

Typing `/play` or another slash expression into an ordinary message does not run
it. Use the command picker; to post slash text literally, choose **Send as text**
or prefix it with `//`. Edits, history replay, and bot-authored messages never run
music actions. Existing pending informational replies can still be recovered after
a bot restart, but old text commands are not resumed.

The bot's voice connection is for audio output only. It cannot subscribe to
people's microphone or screen-share tracks; the voice list labels it
**playback only**. This restriction is enforced by the server as well as the
music publisher.

Pause and Stop release the audio stream and its provider resources. The Bot can
remain silently in the voice channel so the next track does not disconnect and
rejoin, for the **Leave voice when idle** time at most. This connection lasts at most
30 minutes and ends when the Bot is disabled or the channel is removed. A server disconnect stops playback; use a new playback
command to request a fresh connection.
