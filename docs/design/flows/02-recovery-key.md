# Flow 02 — the recovery-key ceremony (web-1)

The strings of this document are L-COPY-01 of the web-1 plan, quoted, except `onboarding.keys.loss`, which is L-COPY-02 of the web-2a plan. A change is made there first.

Step 3 of `01-onboarding.md`, drawn in detail. Built by web-1 task 20 (`RecoveryKey` in `@dilla/ui`) and
task 23 (the onboarding screen). The rules come from `protocol/03-identity.md` "Recovery": the key is 256
random bits shown once as 52 Crockford base32 characters in 13 groups of 4, the person must acknowledge
that they wrote it down before continuing, and the screen offers no copy button.

**Wave B task 3b (2026-10-30).** The step follows the Signal prototype's "Save your recovery key": one paragraph
(`onboarding.keys.body`), the grid (its name `onboarding.keys.label` is for assistive technology only), the actions row
under it with `Print` (where the prototype has Copy and Save as file: protocol/03 forbids a copy control), the 16px check
box `onboarding.keys.acknowledge`, and the button `I saved it` (`onboarding.keys.submit`), blocked until the tick, which
creates the account (protocol/03: the key is shown before the account is registered). Back and a refused creation discard
the key; the next pass shows a new one. The wireframes below are the web-1 layout.

## What is on the screen

- The heading `onboarding.keys.title`, the step line, and the two paragraphs `onboarding.keys.body` and
  `onboarding.keys.loss`, in that order.
- The grid: a label (`onboarding.keys.label`) and an ordered list of the 13 groups exactly as
  `account.recoveryKey` holds them, in order, in upper case. Each group is plain text. The numbers 01–13
  in front of the groups are drawn by CSS and are not part of the text: selecting the whole grid and
  pasting it yields the 13 groups and line breaks, nothing else. While `account.recoveryKey` is still
  `null`, the grid's place holds the status line `onboarding.keys.preparing` (`role="status"`).
- The acknowledgement: a native checkbox labelled `onboarding.keys.acknowledge`, not ticked when the step
  opens.
- The print button `onboarding.keys.print`.
- The footer: `Back`, and `Continue`, which is blocked until the box is ticked: `aria-disabled="true"`,
  never the native `disabled`, so it stays in the tab order and its reason is heard. While it is blocked the
  hint `onboarding.keys.ackHint` stands under it and is its description, and activating it does nothing.

## Wireframe at 1280

The grid fills the column. Four groups per row at this width (the columns are at least 7.5rem wide and
as many fit as the column allows; no breakpoint decides it). The grid label is drawn in capitals by CSS.

```
                      Recovery key
                      +--------------------------------------------------+
                      |  01  7K3M     02  QW9D     03  X2RT     04  0PNA |
                      |  05  HV5C     06  J8ZE     07  M4TB     08  S6YF |
                      |  09  1GKD     10  R3WP     11  ZN7H     12  C9QX |
                      |  13  5TVA                                        |
                      +--------------------------------------------------+
                      [ ] I have written down or printed my recovery key   [ Print ]
                      --------------------------------------------------
                                               [ Back ]  [ Continue ]
                                                         (blocked)
                                                         Tick the box to continue.

  after ticking:      [x] I have written down or printed my recovery key   [ Print ]
                      --------------------------------------------------
                                               [ Back ]  [ Continue ]
```

## Wireframe at 360

Two groups per row, seven rows; the checkbox label wraps under itself; `Print` sits on its own line.

```
  Recovery key
  +--------------------------------+
  |  01  7K3M        02  QW9D      |
  |  03  X2RT        04  0PNA      |
  |  05  HV5C        06  J8ZE      |
  |  07  M4TB        08  S6YF      |
  |  09  1GKD        10  R3WP      |
  |  11  ZN7H        12  C9QX      |
  |  13  5TVA                      |
  +--------------------------------+
  [ ] I have written down or
      printed my recovery key
  [ Print ]
```

## Type and colour

The groups are `--text-lg` in the mono face with wide letter spacing, `--fg` on `--surface-2` inside a
1px `--edge` border. The numbers are `--text-micro` in `--fg-3`. Nothing in the grid is accent-coloured
and nothing animates. All three themes use the same layout; high contrast draws the border in white.

## Print

`Print` calls the browser's print dialog. The printed page shows the heading, the two paragraphs, the
label and the grid with its numbers; the top bar, the step line, the checkbox, the print button and the
footer are not printed. Nothing is sent anywhere and nothing is written to disk by the page.

The print rules use system colours (`CanvasText` on `Canvas`), because browsers do not print backgrounds and the default theme's text is near-white.

## Keyboard and assistive technology

- Tab order: the grid's text is not a tab stop; Tab goes from the last field above to the checkbox, then
  `Print`, then `Back`, then `Continue` (also while it is blocked, when focus on it reads the hint).
- Space toggles the checkbox; Enter or Space activates `Print`, `Back` and `Continue`.
- A screen reader reads the label `Recovery key` and then a list of 13 items, each one group; the numbers
  are not read (the CSS counter has empty alternative text). The groups can be read character by
  character with the reader's own commands.
- The key is selectable text, so a person who chooses to can select it and paste it into a password
  manager; nothing on the screen invites it.

## What is not offered, and why

| not offered | why |
|---|---|
| a copy button | `protocol/03-identity.md` "Recovery": the client MUST NOT offer one on this screen (DEV-W15, F2). A copy button puts the key on the clipboard, where other programs and clipboard history can read it. Selecting the text by hand stays possible. |
| the 24-word form | F2 defers the word list (DEV-W16); the core has no word list (`core/dilla-core/src/identity/recovery.rs:4-5`). Only the 52-character form is shown. |
| showing the key again later | the key is never stored (plan web-1 ruling 6); once the step is left forward and the account exists, it cannot be shown again |
| a download as a file | a file in a downloads folder is a copy the person did not choose to make |
| a QR code | nothing in web-1 scans one |
| typing the key back to check it | the key is typed only where it is used: signing in on another browser and removing a device (`03-sign-in.md`), where paste is allowed (F2, WCAG 3.3.8) |

## Copy

| key | text |
|---|---|
| `onboarding.keys.title` | `Save your recovery key` |
| `onboarding.keys.body` | `Shown once. You need it to add another browser.` |
| `onboarding.keys.label` | `Recovery key` |
| `onboarding.keys.acknowledge` | `I saved my recovery key somewhere safe` |
| `onboarding.keys.ackHint` | `Tick the box to continue.` |
| `onboarding.keys.print` | `Print` |
| `onboarding.keys.preparing` | `Making your keys` |
