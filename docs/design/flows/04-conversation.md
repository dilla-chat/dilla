# Flow 04 — the conversation says more: edit, delete, react, reply, pin, mention, attach (web-2b)

The strings and the flow rules of this document began as L-COPY-03 of the web-2b plan, quoted. From web-2b task 8 on this
document's Copy table is the record of these strings, held equal to `en.ts` by `copy.test` (task 9 adds this file to its
flow list); a change is made here and in `en.ts` together.

How a person edits, deletes, reacts to, replies to and pins messages, mentions people, roles and everyone (`@everyone`
from the autocomplete, a typed `@here` counting as everyone), and sends and opens images and files in a channel or a
direct message. Wireframed before it is built (F16). Built by web-2b task 8 (the components in `@dilla/ui`) and task 9
(the conversation in `packages/web/src/screens/conversation/` and the strings in `packages/web/src/strings/en.ts`). The
shell around it is web-1's; the badges and notifications are web-2a's.

Every user-visible string is a key of `packages/web/src/strings/en.ts`; the "Copy" table at the end is the exact English
text. `{name}` is a member's display name, else their username; `{channel}` a channel's name; `{count}` and `{n}` numbers;
`{code}` an error code such as `E_BLOB_OPEN`; `{size}` a size made by `shell.attachment.bytes`, `.kb` or `.mb`; `{id}` the
first eight hexadecimal characters of an id the device cannot name.

The wireframes write each string as the Copy table has it. Chrome labels are lower case (Q14): the toolbar, the reaction
names, the tray phases, the reply chip, the mention list, the card actions, the pins button. Dialog surfaces are sentence
case: the delete dialog, the pins dialog's heading and empty text, the lightbox's buttons, the tray's refusals. Nothing in
this flow says how messages or files are protected (F10): no lock, no "secure", no "encrypting".

## Entry

The conversation is the main pane of the shell for an open channel or DM whose group is `active` (web-1). Every control
below is offered only there; a channel in any other state shows web-1's states and none of these controls.

## Steps

| the person | the page | the worker command (L-TS-35) |
|---|---|---|
| activates `react` on a row, or the `+` of its reaction bar | opens the emoji grid next to the control | — |
| picks an emoji in the grid | closes the grid, focus back to the control that opened it | `react` with `on` = not already `mine` |
| activates a reaction chip | toggles it | `react` with `on` = not `mine` |
| activates `reply` | shows the reply chip above the composer, focus to the composer | — (the next `send` carries `replyTo`) |
| activates `edit` on an own row, or presses `ArrowUp` in an empty composer | replaces the body by the editor, focus in it | — |
| saves the editor (`Enter` or `save`) | closes the editor; the row shows `saving…` until the edit is folded | `editMessage` |
| activates `pin` / `unpin` | — | `pin` with `on` true / false |
| activates `delete` on an own row | opens the delete dialog | — |
| confirms `Delete` | closes the dialog; the row shows `saving…` until the delete is folded | `deleteMessage` |
| activates `pinned` in the header | opens the pins dialog | `loadPins` (and `closePins` when it closes) |
| activates `go to message` in the pins dialog | closes it, scrolls to the row and flashes it for 1.4 s | — |
| types `@` and letters in the composer | opens the mention list | — |
| picks a person (`Enter`, `Tab` or a click) | inserts `@username ` | — |
| activates `attach files`, drops files on the main pane, or pastes files into the composer | the tray lists them by phase | `attachFiles` |
| removes a tray entry | — | `discardAttachment` |
| sends | the message with its ready files; the tray empties | `send` with `attachments` and `replyTo` |
| activates an image card | opens the lightbox with the full image | `openAttachment` (`thumb` false) |
| activates `save` on a file card or in the lightbox | the browser saves the file under its shown name | `openAttachment` (`thumb` false) |
| activates the reply line of a row | scrolls to the original and flashes it | — |

Nothing is shown as done before the worker's timeline says so: an edit, a delete, a reaction or a pin in flight is the
row's state text (`saving…`), never the result. A pin is visible to everyone in the conversation and anyone in it may
unpin it; the pins dialog says who pinned. A delete removes the message, its reactions and its files for everyone,
including devices that come online later; a copy someone saved stays theirs.

## Wireframes at 1280 × 800

The main pane of the shell (rail, channel list and status bar as web-1). The second message is the active row.

```
+------------------------------------------------------------------------------------------------------------+
| [ # general ]  | say hi                                                                   [ pinned ]       |
+------------------------------------------------------------------------------------------------------------+
|  (B) björn  web  21:03                                                                                     |
|      after nine, still on the boat                                                                         |
|                                                                                                            |
|  (A) ada  21:04  edited  pinned                            [ react ][ reply ][ edit ][ unpin ][ delete ]   |  <- active row,
|      ↳ björn  after nine, still on the boat                                                                |     toolbar shown
|      @mira bring the map, @everyone meet at the harbour                                                    |
|      +-----------------------------+   +---------------------------------------------+                     |
|      |                             |   | ▤  route.gpx           12.4 KB    [ save ]  |                     |
|      |      (thumbnail, 320×240)   |   +---------------------------------------------+                     |
|      |                             |                                                                       |
|      +-----------------------------+                                                                       |
|      ( 👍 3 )( 🦀 1 )( + )                                                                                 |
|                                                                                                            |
|  (M) mira  21:05                                                                                           |
|      count me in                                                                                           |
|      saving…                                                                                               |  <- a pending reaction
+------------------------------------------------------------------------------------------------------------+
|  replying to björn   after nine, still on the boat                                              [ × ]      |  <- reply chip
|  drawn.png   48 KB   uploading                                                                  [ × ]      |  <- tray
|  notes.txt   2 KB    ready                                                                      [ × ]      |
|  +--------------------------------------------------------------------------------------------------------+|
|  | [+] message #general                                                                        [ send ]  ||
|  +--------------------------------------------------------------------------------------------------------+|
+------------------------------------------------------------------------------------------------------------+
```

The emoji grid, opened from `react` (a popover beside the control, 8 columns of 32):

```
                                   +-------------------------------------------+
                                   | 👍  👎  ❤️  😄  😂  😢  😡  😍            |
                                   | 🎉  🔥  💯  ✨  🙏  👀  🤔  😴            |
                                   | 🛠  🚀  ✅  ❌  💡  📌  🐛  📦            |
                                   | ☕  🍕  🌮  🎨  🎵  🌙  ☀️  🦀            |
                                   +-------------------------------------------+
```

Editing in place (the body replaced by the editor):

```
|  (A) ada  21:04                                                                                            |
|      +--------------------------------------------------------------------------------------------------+  |
|      | meet at the harbour at nine                                                                      |  |
|      +--------------------------------------------------------------------------------------------------+  |
|      escape to cancel · enter to save                                              [ cancel ][ save ]      |
```

The mention list above the composer while typing `@mi`:

```
|  +-------------------------------+                                                                         |
|  | mira          @mira           |  <- active option                                                       |
|  | Mike Dahl     @mike           |                                                                         |
|  +-------------------------------+                                                                         |
|  | [+] message #general  @mi                                                                   [ send ]   |
```

The delete dialog:

```
                      +------------------------------------------------------------+
                      | Delete this message?                                       |
                      | It is removed for everyone in this conversation, with its  |
                      | reactions and files. Copies someone already saved stay     |
                      | with them. This cannot be undone.                          |
                      |                                       [ Cancel ] [ Delete ]|
                      +------------------------------------------------------------+
```

The pins dialog:

```
                      +------------------------------------------------------------+
                      | Pinned in #general                                         |
                      |  björn  21:03                                              |
                      |  after nine, still on the boat                             |
                      |  pinned by ada                     [ go to message ][ unpin ]|
                      |                                                   [ Close ]|
                      +------------------------------------------------------------+
```

The lightbox (the whole viewport):

```
+------------------------------------------------------------------------------------------------------------+
|                                                                                                            |
|                                   (the image, contained)                                                   |
|                                                                                                            |
| drawn.png, 48 KB                     [ Previous image ] [ Next image ]              [ Save ] [ Close ]     |
+------------------------------------------------------------------------------------------------------------+
```

The drop overlay over the main pane while files are dragged over it:

```
+------------------------------------------------------------------------------------------------------------+
|  + - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - +  |
|  |                                      Drop to attach                                                |  |
|  |                                      Up to 4 files, 25 MB each.                                    |  |
|  + - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - +  |
+------------------------------------------------------------------------------------------------------------+
```

## Wireframes at 360 × 740

The shell's narrow layout (web-1): the main pane alone. The toolbar sits in flow on its own line under the head of the
active row (never over the row above); cards fill the width; the lightbox and the dialogs fill the viewport.

```
+------------------------------------+
| [ # general ]        [ pinned ]    |
+------------------------------------+
| (A) ada 21:04 edited pinned        |
| [react][reply][edit][unpin][delete]|
| ↳ björn after nine, still on the…  |
| @mira bring the map                |
| +--------------------------------+ |
| |   (thumbnail, full width)      | |
| +--------------------------------+ |
| ( 👍 3 )( + )                      |
+------------------------------------+
| replying to björn            [ × ] |
| drawn.png 48 KB uploading    [ × ] |
| +--------------------------------+ |
| | [+] message #general  [ send ] | |
| +--------------------------------+ |
+------------------------------------+
```

## States

| state | what the row or control shows |
|---|---|
| an edit, delete, reaction or pin in flight (an outbox fold, pending) | the target row's state text `saving…` (`shell.message.saving`); nothing else changes until the fold arrives |
| the same, failed | `not saved` (`shell.message.notSaved`) with `retry` and `discard` (web-1's `shell.message.retry`, `.discard`) |
| edited | `edited` in the head; the body is the latest edit |
| pinned | `pinned` in the head |
| a reply whose original is held | the reply line `↳ {name} {excerpt}`; activating it jumps |
| a reply whose original is further back than the loaded page | the jump shows `shell.message.replyNotLoaded` as an info banner above the log, with a dismiss action |
| a reply whose original cannot be shown here (never received, or not readable on this device) | the reply line says `the original message cannot be shown here`; it is not a button |
| a reply whose original was deleted | the reply line says `the original message was deleted`; it is not a button |
| an attachment-only message | the cards with no body line |
| an image card before its thumbnail is opened | a neutral box of the image's proportions |
| a GIF card (`image/gif`, 8 MiB or less, not too large, sent) first at least half visible | `openAttachment` (`thumb` false) once; the full GIF replaces the thumbnail in the same `img` (same size and alt text) for five minutes from that first sighting, then the thumbnail is back and the blob is released; scrolling out and in, or a re-mount, keeps the first deadline and never fetches again; a failed open keeps the thumbnail with no error text |
| a GIF card that is still (before it plays, after the five minutes, over 8 MiB, or with `prefers-reduced-motion: reduce`, which never plays one) | a small `shell.attachment.gif` badge on the card, visual only (`aria-hidden`); the card's name stays `shell.attachment.open`; activating it opens the lightbox with the full, animated file |
| an image card whose open failed | the box or thumbnail and `could not open ({code})` |
| an attachment over 25 MB received in a browser | the card with `too large to open in a browser` and no button |
| a file picked over 25 MB | nothing is uploaded; an alert above the tray in the composer says `{name} is over 25 MB and cannot be sent from a browser.` |
| a fifth file | nothing is added; the alert says `A message carries at most 4 files.` |
| send while a file is not ready | nothing is sent; the alert says `Wait until every file is ready, or remove it.` |
| a tray entry that failed | `failed ({code})` and, under it, `Remove it and attach it again.` (`shell.tray.failedHint`) |
| a message failed because a file is no longer on the server | `shell.message.attachmentGone` (`A file of this message is no longer on the server. Discard it and attach the file again.`), discard only |
| a mention of me, of a role I hold, `@everyone` or a received `@here` | the pill in the mention colour; the row has the mention bar |
| a mention of someone else | a quiet pill with their `@name` |
| a mention of an id this device cannot name | `@{id}` with its first eight hex characters |
| a role mention | `@role` |

## Keyboard and focus

No single-character key anywhere (WCAG 2.1.4) and no new modifier shortcut (Q14).

- Shell order (web-1): skip link → rail → channel list → header (the `pinned` button after the topic) → log → composer → status bar.
- The log is one tab stop. Entering it focuses the active row (initially the newest). `ArrowUp` / `ArrowDown` move the active row; `Home` / `End` go to the first and last loaded row; `Escape` on a row goes to the composer.
- Inside the active row, after the row itself: the toolbar (one stop; `ArrowLeft` / `ArrowRight`, `Home` / `End`), the reply line's button, each attachment card's button, the reaction bar (one stop; `ArrowLeft` / `ArrowRight` across the chips and `+`). `Escape` inside any of them returns focus to the row. The controls of every other row are not in the tab order; pointers still reach them.
- The toolbar is hidden while the person types in the composer (it never floats over the row above). Shift+Tab from the composer enters the active row at its last visible stop; the toolbar appears once focus is in the log.
- The emoji grid: 8 columns; arrows in two dimensions, `Home` / `End`; `Enter` or `Space` picks; `Escape` closes; focus returns to the control that opened it.
- The editor: `Enter` saves, `Shift+Enter` breaks the line, `Escape` cancels; never during an IME composition; focus returns to the row.
- The composer: `ArrowUp` with an empty value and no open mention list edits the newest own sent message. With the mention list open, `ArrowUp` / `ArrowDown` move, `Enter` or `Tab` picks, `Escape` closes it; with no list open and a reply chip, `Escape` cancels the reply.
- The composer's textarea keeps its `textbox` role and its name at all times. While the mention list is open it carries `aria-controls` (the list's id), `aria-activedescendant` (the active option) and `aria-autocomplete="list"`; the list is a `listbox` of `option`s and none of them takes focus.
- The delete dialog: focus to the dialog, `Escape` cancels; `Delete` is the last button.
- The pins dialog: focus to the dialog; `Escape` closes; focus returns to the `pinned` button.
- The lightbox: focus to `Close`; `ArrowLeft` / `ArrowRight` move between the images of the same message; `Escape` closes; focus returns to the card.
- Dropping files is pointer-only; `attach files` and pasting into the composer are the keyboard paths.

When the focused control goes away, focus never falls to the page:

- un-reacting the last chip of a row (the reaction bar goes away) → the row;
- removing a tray entry → the next entry's remove button, else the composer's textarea;
- `cancel reply` → the composer's textarea;
- `unpin` in the pins dialog: the dialog stays open and focus moves to the next item's `unpin`, else to `Close`;
- dismissing the `shell.message.replyNotLoaded` banner → the log's active row;
- `discard` on the `shell.message.attachmentGone` line (the message leaves the log) → the row web-1 hands focus on to (`handOnFromRow(1)`);
- focus leaving the emoji grid (Tab out, or a click elsewhere) closes it without moving focus.

## Announcements

- The log is `role="log"`, polite, with `aria-relevant="additions"` (web-1 plus this flow): a new message is announced;
  a changed reaction count, a pin or an edit's text change is not (they are reflected in the row). A newly inserted node
  inside a row — a first reaction chip, the inline editor, a pending-action line — may be read once as an addition. The
  emoji grid is a popover outside the log and is never read as log content.
- A tray entry's phase is a polite live region: `reading`, `preparing`, `uploading`, `ready`, `failed ({code})`; a failed
  entry adds `Remove it and attach it again.`
- A tray refusal (`shell.tray.tooLarge`, `.tooMany`, `.notReady`) is an alert (`role="alert"`) above the tray in the
  composer; `shell.message.replyNotLoaded` is an info banner above the log with a dismiss action. Neither is shown by
  colour alone.
- A pending action is announced through the row's state text, never by colour alone.

## Markdown in message bodies (E5)

- Parsed on the reader's device, from the decrypted text; the wire body, editing and search stay raw text.
- Shown: `**strong**`, `*em*` or `_em_`, `~~strike~~`, `` `code` ``, fenced code blocks, `> quote` lines, and bare `http://`, `https://` and `mailto:` links. Anything else, including `[text](url)` and raw HTML, is literal text. A fence whose opening line says more than a language word, or that is never closed, is literal text. A code span is outlined, never filled, so it cannot pass for a mention pill.
- A body is rendered as React elements from a parsed tree. There is no `dangerouslySetInnerHTML` and no HTML parsing.
- A link shows exactly the address it opens (the normalised address: a trailing `/` is added, and a host with non-ASCII letters is shown as punycode, `xn--...`, so a lookalike cannot pass for the real domain). It is a left-to-right isolate, opens in a new tab with `rel="noopener noreferrer nofollow"`, and loads nothing by itself. In a message that holds a link, bidi control characters are removed from the text around it. A `mailto:` link is the bare address only (a query such as `?attach=` or `?body=` is not linked). Other schemes (`javascript:`, `data:`, `blob:`, `file:`), addresses with a user name or password, and addresses holding a control, format, bidi, zero-width or filler character are plain text.
- Mentions stay pills inside formatting; code never makes a pill. Unclosed markers and backslash escapes (`\*`) stay literal.
- Links are underlined and keep the text colour; a link is a tab stop only while focus is in its row, like the author name and the mention pills.

## What is not offered, and why

- Threads, quote reply, forward, "mark unread", "save message", "copy link", a right-click menu: later cards (the toolbar's five actions only).
- Edit history: only the latest edit, marked `edited`.
- A free emoji picker, custom emoji, emoji in the composer: the fixed 32 reactions. Message bodies show a small markdown subset (below), never raw HTML; there is no formatting toolbar, and `[text](url)` links are not links.
- Byte progress for uploads: `fetch` reports none; the tray shows phases.
- Deleting someone else's message: only its author can.
- Files over 25 MB in a browser: refused at the tray, shown as too large on receipt.
- An `@here` option in the mention list: the client has no presence (ruling 29 as amended); a typed `@here` is still sent and counts as everyone.

## Copy

`@everyone` and `@here` are protocol tokens shown verbatim (a mention pill's text, the `everyone` option's primary text in
the mention list): they are not keys of `en.ts` and have no row in this table.

| key | text | where |
|---|---|---|
| `shell.message.toolbar` | `actions for this message` | the toolbar's accessible name |
| `shell.message.react` | `react` | toolbar button |
| `shell.message.reply` | `reply` | toolbar button |
| `shell.message.edit` | `edit` | toolbar button, own rows |
| `shell.message.pin` | `pin` | toolbar button |
| `shell.message.unpin` | `unpin` | toolbar button on a pinned row; pins dialog button |
| `shell.message.delete` | `delete` | toolbar button, own rows |
| `shell.message.edited` | `edited` | row head |
| `shell.message.pinned` | `pinned` | row head |
| `shell.message.replyLabel` | `reply to {name}` | the reply line's description (first part) |
| `shell.message.replyMissing` | `the original message cannot be shown here` | the reply line, original not received or not readable here |
| `shell.message.replyDeleted` | `the original message was deleted` | the reply line, original deleted |
| `shell.message.replyJump` | `go to the original message` | the reply line's description (second part); the button is named by its visible text |
| `shell.message.replyNotLoaded` | `The original message is further back. Load earlier messages to reach it.` | info banner above the log after a jump, with dismiss |
| `shell.message.reactions` | `reactions` | the reaction bar's name |
| `shell.message.reaction` | `{name}, {count}` | a reaction chip's name |
| `shell.message.reactionAdd` | `add a reaction` | the reaction bar's `+` |
| `shell.message.saving` | `saving…` | row state text, a fold in flight |
| `shell.message.notSaved` | `not saved` | row state text, a fold failed |
| `shell.message.mentionUnknown` | `@{id}` | a mention of an id this device cannot name |
| `shell.message.mentionRole` | `@role` | a role mention |
| `shell.emoji.label` | `pick a reaction` | the emoji grid's name |
| `shell.emoji.01` | `thumbs up` | 👍 |
| `shell.emoji.02` | `thumbs down` | 👎 |
| `shell.emoji.03` | `red heart` | ❤️ |
| `shell.emoji.04` | `grinning face` | 😄 |
| `shell.emoji.05` | `tears of joy` | 😂 |
| `shell.emoji.06` | `crying face` | 😢 |
| `shell.emoji.07` | `angry face` | 😡 |
| `shell.emoji.08` | `heart eyes` | 😍 |
| `shell.emoji.09` | `party popper` | 🎉 |
| `shell.emoji.10` | `fire` | 🔥 |
| `shell.emoji.11` | `hundred points` | 💯 |
| `shell.emoji.12` | `sparkles` | ✨ |
| `shell.emoji.13` | `folded hands` | 🙏 |
| `shell.emoji.14` | `eyes` | 👀 |
| `shell.emoji.15` | `thinking face` | 🤔 |
| `shell.emoji.16` | `sleeping face` | 😴 |
| `shell.emoji.17` | `hammer and wrench` | 🛠 |
| `shell.emoji.18` | `rocket` | 🚀 |
| `shell.emoji.19` | `check mark` | ✅ |
| `shell.emoji.20` | `cross mark` | ❌ |
| `shell.emoji.21` | `light bulb` | 💡 |
| `shell.emoji.22` | `pushpin` | 📌 |
| `shell.emoji.23` | `bug` | 🐛 |
| `shell.emoji.24` | `package` | 📦 |
| `shell.emoji.25` | `hot beverage` | ☕ |
| `shell.emoji.26` | `pizza` | 🍕 |
| `shell.emoji.27` | `taco` | 🌮 |
| `shell.emoji.28` | `artist palette` | 🎨 |
| `shell.emoji.29` | `musical note` | 🎵 |
| `shell.emoji.30` | `crescent moon` | 🌙 |
| `shell.emoji.31` | `sun` | ☀️ |
| `shell.emoji.32` | `crab` | 🦀 |
| `shell.delete.title` | `Delete this message?` | delete dialog heading |
| `shell.delete.body` | `It is removed for everyone in this conversation, with its reactions and files. Copies someone already saved stay with them. This cannot be undone.` | delete dialog body |
| `shell.delete.confirm` | `Delete` | delete dialog, danger button |
| `shell.delete.cancel` | `Cancel` | delete dialog, close button |
| `shell.edit.label` | `edit your message` | the editor's name |
| `shell.edit.hint` | `escape to cancel · enter to save` | under the editor |
| `shell.edit.save` | `save` | editor button |
| `shell.edit.cancel` | `cancel` | editor button |
| `shell.composer.replying` | `Replying to {name}` | the reply bar (Signal §5.5, the prototype's case) |
| `shell.composer.replyCancel` | `cancel reply` | the reply chip's `×` |
| `shell.composer.attach` | `Attach files (25 MB each)` | the composer's paperclip (Signal §5.5, the prototype's tooltip) |
| `shell.composer.mentions` | `people to mention` | the mention list's name |
| `shell.composer.mentionEveryone` | `everyone in this channel` | the `everyone` option's secondary text |
| `shell.pins.open` | `pinned` | header button |
| `shell.pins.title` | `Pinned in #{channel}` | pins dialog heading, channel |
| `shell.pins.titleDm` | `Pinned with {name}` | pins dialog heading, DM |
| `shell.pins.empty` | `Nothing is pinned here yet.` | pins dialog, no pins |
| `shell.pins.by` | `pinned by {name}` | pins dialog item |
| `shell.pins.jump` | `go to message` | pins dialog item button |
| `shell.pins.close` | `Close` | pins dialog close |
| `shell.drop.title` | `Drop to attach` | drop overlay |
| `shell.drop.body` | `Up to 4 files, 25 MB each.` | drop overlay |
| `shell.tray.label` | `files to send` | the tray's name |
| `shell.tray.reading` | `reading` | tray phase |
| `shell.tray.preparing` | `preparing` | tray phase |
| `shell.tray.uploading` | `uploading` | tray phase |
| `shell.tray.ready` | `ready` | tray phase |
| `shell.tray.failed` | `failed ({code})` | tray phase |
| `shell.tray.failedHint` | `Remove it and attach it again.` | beside a failed tray entry |
| `shell.tray.remove` | `remove {name}` | a tray entry's `×` |
| `shell.tray.removeTip` | `Remove` | the `×`'s tooltip (Signal §6.8) |
| `shell.tray.tooLarge` | `{name} is over 25 MB and cannot be sent from a browser.` | alert above the tray in the composer |
| `shell.tray.tooMany` | `A message carries at most 4 files.` | alert above the tray in the composer |
| `shell.tray.notReady` | `Wait until every file is ready, or remove it.` | alert above the tray in the composer |
| `shell.attachment.open` | `open {name}` | image card button |
| `shell.attachment.save` | `save {name}` | file card button |
| `shell.attachment.opening` | `opening…` | card state while opening |
| `shell.attachment.failed` | `could not open ({code})` | card state after a failed open |
| `shell.attachment.tooLarge` | `too large to open in a browser` | card state over 25 MB |
| `shell.attachment.unnamed` | `file` | the shown name of a file sent without one |
| `shell.attachment.gif` | `GIF` | badge on a still GIF card |
| `shell.message.attachmentGone` | `A file of this message is no longer on the server. Discard it and attach the file again.` | a sent message that failed with `E_ATTACHMENT_MISSING`; only discard is offered |
| `shell.attachment.bytes` | `{n} B` | size under 1 KB |
| `shell.attachment.kb` | `{n} KB` | size under 1 048 576 bytes, one decimal under 10 |
| `shell.attachment.mb` | `{n} MB` | size from 1 048 576 bytes, one decimal under 10 |
| `shell.lightbox.close` | `Close` | lightbox button |
| `shell.lightbox.save` | `Save` | lightbox button |
| `shell.lightbox.previous` | `Previous image` | lightbox button |
| `shell.lightbox.next` | `Next image` | lightbox button |
| `shell.lightbox.label` | `{name}, {size}` | the lightbox's name and caption |

`@here` counts as everyone in the conversation until presence exists (ruling 29); the mention list offers only `@everyone`, and a received `<@here>` renders as `@here`.
