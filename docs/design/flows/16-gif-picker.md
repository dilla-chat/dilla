# Flow 16: the GIF picker and the server's Giphy setting (E8)

GIFs are a per-server setting. A person who manages a server enters that server's Giphy key in its settings; the key is write-only and a visible configured status replaces the field afterwards. A server without a key has no GIF button. Search and the preview and full downloads go through the instance, so Giphy sees the instance and never a person. The picked GIF is attached like any file: it is encrypted like any file, and a reader's browser never contacts giphy.com. There are no GIFs in direct messages.

## States

| State | UI |
|---|---|
| a channel of a server with no Giphy key | no `gif` button in any composer |
| a channel of a server with a Giphy key | a `gif` button (`gif.button`) after the attach `+`, in a channel's composer (a thread's composer has none); the button appears once the server's setting has been read |
| reading the setting fails | no button |
| a direct message or group message, or a thread | never a `gif` button; the setting is not even asked for |
| the composer is blocked (read-only, no permission) | the `gif` button is disabled with the rest of the composer |
| picker open, nothing typed | trending results; the search box (`gif.search`, placeholder `gif.search.placeholder`) is focused; `aria-expanded="true"` on the button |
| searching | `gif.loading` in the status line; results of the last search stay until the new ones arrive |
| results | a masonry grid of three columns (two on a phone), one button per GIF named by its title (`gif.untitled` when it has none), in reading order (left to right, then down); each tile is as tall as its own GIF's width and height make it and is never cropped; a preview that is still downloading is a plain block of that same size, so the grid does not jump as the previews fill in; a size the instance did not give is a square |
| the instance is busy | a preview that was refused as busy is asked for again after a short pause (three tries); the others show meanwhile |
| more results | the `gif.more` button under the grid |
| no results | `gif.empty` in the status line |
| the instance refused the search (key removed, busy, rate limit) | `gif.error` in the status line; typing again retries |
| a GIF picked | the picker shows `gif.picking` and ignores further picks; the full GIF is downloaded through the instance, then added to the file tray like a chosen file; the picker closes and focus goes to the message box |
| the pick failed | the picker stays open with `gif.pickFailed` |
| the GIF is in the tray | the tray shows it as an image file named `giphy-{id}.gif`; Enter in the message box sends it, with or without text; the tray's remove button drops it |
| a reader | sees an ordinary image attachment; nothing marks it as a GIF from a service; it plays in the conversation for five minutes after it is first seen, as any GIF does (flow 04) |
| the picker footer | `gif.powered` |
| a phone | the picker is the width of the screen above the composer; results and buttons are 44px targets |
| server settings → GIFs | titled `gif.settings.title` with `gif.settings.hint` and a link `gif.settings.getKey` to Giphy's developer page; fields and actions sit below the introduction |
| the row, no key set | a password field `gif.settings.key` (never auto-filled) and `gif.settings.save`; a key that is not 8 to 64 letters and digits is refused in the field with `gif.settings.invalid` |
| the row, a key set | visible status `gif.settings.maskedLabel` with `gif.settings.replace` and `gif.settings.remove`; the key is never shown again |
| replacing | the field and `gif.settings.save` with `gif.settings.cancel` take the status' place; the field is empty |
| saved or removed | `gif.settings.saved` or `gif.settings.removed` in the status line; the field is emptied |
| the rating | a select `gif.settings.rating` with `gif.rating.g`, `gif.rating.pg`, `gif.rating.pg-13` (the default) and `gif.rating.r`; changing it saves at once and says `gif.settings.ratingSaved`; it filters search only |
| a person without Manage Server | does not see the row (the settings section is for managers); a refusal from the instance says `gif.settings.forbidden` |
| the instance refuses a change | `gif.settings.error` with the code, never the key |
| the setting cannot be read | `gif.settings.loadError` |

## Keyboard map

- `gif` button: Enter or Space toggles the picker; `aria-haspopup="dialog"`, `aria-expanded` follows, `aria-controls` names the picker while it is open
- Search box: ArrowDown enters the grid
- The grid is one tab stop that follows focus: ArrowLeft and ArrowRight move in reading order, ArrowUp and ArrowDown to the tile above or below in the same column, without wrapping; Home and End go to the ends; ArrowUp from a tile with none above goes back to the search box
- A result: Enter, Space or a click picks it
- Escape closes the picker and returns focus to the `gif` button; it goes no further
- The settings row: Tab reaches the field or the configured key's buttons in order; Enter in the field saves; Escape in the replace field cancels

## Focus rules

- Opening focuses the search box
- Escape returns focus to the `gif` button
- A pick moves focus to the message box, with the GIF in the tray
- A pointer down outside the picker closes it, except on the `gif` button, which toggles
- Saving a key, removing it or cancelling a replace puts focus on the row's next control (`gif.settings.replace`, `gif.settings.key`)

## Announcements

- The status line is a polite status: it speaks `gif.loading`, `gif.empty`, `gif.picking` and `gif.pickFailed`
- A search failure speaks `gif.error` once
- A GIF added to the tray speaks `gif.added` through the shell announcer
- The settings row says `gif.settings.saved`, `gif.settings.removed` or `gif.settings.ratingSaved` through the shell announcer (the settings body keeps no status region of its own); a refusal is an alert

## Copy

`gif.button='add a gif'`
`gif.picker='gifs'`
`gif.search='search gifs'`
`gif.search.placeholder='search giphy'`
`gif.untitled='gif'`
`gif.more='more'`
`gif.powered='Powered by GIPHY'`
`gif.loading='searching'`
`gif.empty='no gifs for that'`
`gif.error='could not reach the gif service'`
`gif.picking='adding the gif'`
`gif.pickFailed='could not add that gif'`
`gif.added='gif added. press enter to send.'`
`gif.settings.title='GIFs (Giphy)'`
`gif.settings.hint='Members can search and send GIFs in this server’s channels. The key stays on this instance, is never shown again and is used only to ask Giphy for GIFs. There are no GIFs in direct messages.'`
`gif.settings.getKey='get a key at developers.giphy.com'`
`gif.settings.key='giphy api key'`
`gif.settings.save='save'`
`gif.settings.cancel='cancel'`
`gif.settings.masked='••••••••'`
`gif.settings.maskedLabel='a giphy key is set'`
`gif.settings.replace='replace'`
`gif.settings.remove='remove'`
`gif.settings.rating='rating'`
`gif.settings.saved='giphy key saved'`
`gif.settings.removed='giphy key removed'`
`gif.settings.ratingSaved='rating saved'`
`gif.settings.invalid='a giphy key is 8 to 64 letters and digits'`
`gif.settings.forbidden='only people who manage this server can change this'`
`gif.settings.error='could not save ({code})'`
`gif.settings.loadError='could not read the gif settings'`
`gif.rating.g='g: everyone'`
`gif.rating.pg='pg'`
`gif.rating.pg-13='pg-13'`
`gif.rating.r='r'`
