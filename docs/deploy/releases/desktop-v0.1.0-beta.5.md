# dilla desktop 0.1.0-beta.5

A desktop release on top of 0.1.0-beta.4. It pairs with server release [v0.1.0-bot2.4](v0.1.0-bot2.4.md), which carries
the same client changes in its bundled web client. Nothing changes in the server, its database or the wire version.

## What you see

- **The dock stays visible inside a voice channel.** The call card and the compact Now playing card no longer hide while
  you are on the voice channel's own page.
- **The animated signal bars are gone** from the dock and the top bar.
- **Private Bot replies go away.** A reply marked "Only you can see this" disappears a minute after it finishes. A reply
  that is still working, or that offers **Retry**, stays. Pointing at a reply or moving focus onto it pauses the
  countdown.
- **A ⋯ button on the Bot card** opens **Rename**, **Refresh status** and **Disable**.
- **Message times are always 24-hour**, such as 20:27, as in the design. Dates and other times follow your system region,
  and those times are 24-hour too.
- **A screen picker with thumbnails.** On macOS, Windows and Linux with X11, sharing your screen opens a picker with
  thumbnails of your screens and windows. Linux with Wayland keeps the system picker.
- **Voice moved to another device.** When another of your devices takes over voice, the one that lost it says
  "Voice moved to your other device." and does not rejoin.
- **The poll form stays open** when the Bot's command list refreshes. If the Bot really stops offering polls, you see
  "The Bot stopped offering polls. Try again."

## Under the hood

- The Linux sandbox helper is built with Go 1.27.4.
