# Member profile card (GPUI) — diagnosis

Worktree: /home/thim/Repositories/dilla/.claude/worktrees/signal-design (read only, nothing edited).
Desktop paths are relative to `clients/desktop/`; web paths to the repo root.

- Desktop component: `dilla-desktop-ui/src/components/members/mod.rs` (`ProfileCard`, lines ~593-861)
- Desktop host (placement, open/close, actions): `dilla-desktop/src/screens/members/mod.rs` (`card_overlay` 588-617, `profile_card` 621-680, `choose` 536-586, `list_with_overlays` 420-490, `pane` 1046-1071), `keys.rs:8-12`
- Web spec: `packages/ui/src/ProfileCard/ProfileCard.tsx`, `ProfileCard.css`; host `packages/web/src/screens/admin/MemberCardHost.tsx`

## 1. PRIMARY BUG — the blue banner sticks out of the card's rounded top corners

**What the screenshot shows:** at 20x zoom, both top corners have the card's 1px `--control-edge-field` border drawn as an arc,
while the band's blue fills the full square corner outside that arc, so the corners read as square blue against the
background.

**Why:** the card is `.overflow_hidden().border_1().rounded(radius::XXL)` (`dilla-desktop-ui/src/components/members/mod.rs:846-856`,
`XXL` = 12px, `dilla-desktop-ui/src/tokens.rs:273`). In CSS, `overflow: hidden` + `border-radius` clips children to the
rounded padding box. GPUI does not do that. Its `ContentMask` is a plain rectangle (`core/vendor/gpui-pre/src/window.rs:2469`,
`bounds` only, no corner radii), so `overflow_hidden` clips to the card's square bounds. The band
`div().h(px(56.)).bg(linear_gradient(..))` (`members/mod.rs:689-693`) has no radius of its own, so it paints square corners
over the card's rounded background and border.

Web: `.d-profile-card { overflow: hidden auto; border-radius: var(--r-xxl); border: 1px }` plus `.d-profile-card__band { height: 56px }`
(`ProfileCard.css:3-5, 9`). The browser clips the band to the inner radius.

**Fix (minimal):** give the band its own top radii equal to the card's inner radius, that is the outer radius minus the 1px
border (as the CSS padding-box clip does):

```rust
// members/mod.rs:689
let band = div().h(px(56.)).rounded_t(radius::XXL - px(1.)).bg(gpui::linear_gradient(...));
```

Better, add a single source for this, e.g. `pub const CARD_INNER_R: Pixels = px(11.)` in `members::space` (or a
`radius::inner(outer, border)` helper in tokens), and use it here. The repo already uses this pattern (`rounded_t` on a
first child: `components/factor/factor_row.rs:111`, `components/settings/row.rs:352`).

**Same bug elsewhere:** `dilla-desktop/src/screens/user_settings/profile.rs:215-229`, the profile preview card
(`overflow_hidden` + `rounded(radius::XL)` = 10px, square band child). It needs `.rounded_t(radius::XL - px(1.))` (9px). Any other
`overflow_hidden` + rounded container with a full-bleed coloured first or last child has the same issue. A grep for
`overflow_hidden()` next to `.rounded(` with a `.bg(` child is worth a sweep.

**Why parity didn't catch it:** `parity/tolerance.toml:51-54` lets `members/profile-card` drift by 0.88 % (and `-b` by 0.74 %)
of colour pixels, and `user-settings/profile*` by 0.73–1.36 %. Two corner wedges of about 12x12 px are far below that.

**Tests:**
- Unit test in `members/mod.rs` (crate style: `#[cfg(test)] mod`, like `shadow_tests`). Pull the band into
  `fn band(hue, saturation) -> Div` and assert `band.style().corner_radii.top_left == Some(px(11.).into())` (and
  `top_right`), and that it equals `radius::XXL - px(1.)` (the card's radius minus its border). Do the same for the profile
  preview's band (9px).
- Pixel test: in the parity harness (`dilla-desktop/tests/members.rs` `parity!` and `assert_screen`), or a `#[gpui::test]`
  that draws the card in an offscreen window, sample the pixel at (card.left+1, card.top+1) and assert it is not the band
  hue (it should be the backdrop or the border). Also lower `members/profile-card` and `user-settings/profile*`
  tolerances after the fix so a regression fails.

## 2. Close button and dismissal

**Spec:** the web card has a close ×, but it is **invisible until keyboard-focused** (`ProfileCard.tsx:227-228`,
`ProfileCard.css:41-45`: `.d-profile-card__close { opacity: 0 } :focus-visible { opacity: 1 }`). The comment there says
"The prototype draws no close control: Escape, the scrim and a click outside close it." So a mouse user on the web also
sees no ×. The visible absence in the screenshot matches the design. The real problem is that the desktop's *other*
dismissals are missing or weak:

| Dismissal | Web | Desktop today |
|---|---|---|
| Escape | card's own `onKeyDown` (`ProfileCard.tsx:175-176`), focus is moved into the card on open (`:103-106`) | `keys::CloseCard` bound to `escape` in context `MembersPane` only (`keys.rs:12`, handler `members/mod.rs:1063-1068`). It works only while focus is inside the members pane. A mouse click on a row focuses the row (GPUI `track_focus` focus-on-mouse-down, `gpui-pre/src/elements/div.rs:2833-2843`), so Escape usually works right after opening. Clicking anywhere else first (composer, chat) moves focus, the card stays and Escape no longer reaches it. The card never takes focus itself. |
| Click outside | capture-phase `pointerdown` outside card and row closes it (`ProfileCard.tsx:159-173`) | **None.** `card_overlay` (`screens/members/mod.rs:606-616`) wraps the card in `deferred(anchored())` with no `on_mouse_down_out`. The card stays open until you click the same row again (toggle, `:445-449`), open another card, right-click a row, close the pane or pick roles/moderation. The voice host already does it right: `screens/voice/menu.rs:513-528` (`on_mouse_down_out` + Escape `on_key_down`). |
| Close × (keyboard) | focusable, shown on focus-visible | absent. There is no focus handle in the card, so no Tab trap and no focus move-in, and no way to Tab to its buttons from the row. |
| Action closes card | every action calls `onCard(null)` (`MemberCardHost.tsx:121-134`) | `choose` (`screens/members/mod.rs:536-586`): **Message and Compare are no-ops** (`_ => return`, comment "Tasks 13 and 28 wire them"). The card stays open and nothing happens, so the two most prominent buttons in the screenshot look broken. Roles and moderation close it. |

**Fixes:**
1. `card_overlay`: wrap `view` in a `div().id("members-profile-card").debug_selector(..).occlude().on_mouse_down_out(close).on_key_down(escape => close)`,
   exactly as `voice/menu.rs:517-528`. `close` sets `ui(w,cx).card = None` and calls `refresh_windows`. Watch out: a
   mouse-down on the opening row must not close then re-open the card. The row's `on_click` toggles, and mouse_down_out
   fires on the row's mouse-down first, so the click would re-open it. Skip the close when the press is inside the open row's bounds (the web
   skips `anchor.contains(target)`), e.g. by keeping the open row's bounds via the row's `laid_out` canvas, or by doing the close on
   `on_mouse_down_out` and having `on_open` ignore a click that lands within N ms of a card close for the same user.
2. Give the card a `FocusHandle` (track_focus on the card root). Focus it, or its first action, on open. Add a keyboard-only close ×
   (absolute top 8 right 8, 28px, `IconName::X` 15px, drawn only while focused, `focus_visible`). Close returns focus to the row.
   This matches the spec's keyboard × and makes Escape work wherever the pointer went.
3. `choose`: route `"message"` to the DM (via `screens::dms::navigate`) and `"compare"` to `screens::dms::compare::open`, and
   clear `u.card` for every action, as `MemberCardHost.tsx:134`. If the wiring is out of scope, at least close the card.

**Tests** (pattern: `dilla-desktop/tests/voice.rs:457-502` `profile_opens_the_card_under_the_tile`):
- `a_press_outside_closes_the_member_card`: open the pane fixture, `simulate_click` a row, assert
  `debug_bounds("members-profile-card")` is some, `simulate_mouse_down` at (4,4), assert none.
- `escape_closes_the_member_card_after_focus_left_the_pane`: open the card, click the composer (or focus elsewhere),
  `simulate_keystrokes("escape")` with the card focused, assert closed.
- `clicking_the_open_row_again_closes_it_once`: the second click on the same row closes it and does not re-open it.
- `message_closes_the_card` / `compare_closes_the_card`: click `profile-action-message` / `profile-action-compare`, assert
  the card is gone and the DM or compare view is open.
- `the_card_has_a_keyboard_close`: Tab from the card's first action reaches `profile-close`, and Enter closes the card.

## 3. Other element-by-element differences (desktop vs web)

| # | Element | Desktop | Web | Fix |
|---|---|---|---|---|
| a | Avatar ring shape | ring div `.rounded(radius::PILL)` around the avatar (`components/members/mod.rs:731-749`): always a circle, also for a Bot's rounded-square avatar | ring is `box-shadow` on the `.d-avatar` itself, so it follows its shape (`ProfileCard.css:11-12`) | use the avatar's own radius for the ring (bot square radius when `is_bot`). Test: bot card's ring radius equals the bot avatar radius. |
| b | Glyph action icon size | 14px (`mod.rs:833`) | 15px (`ProfileCard.tsx:221`) | `.size(px(15.))`. Unit or parity test. |
| c | Name overflow | single line `.ellipsis()` (`mod.rs:708`) | wraps (`overflow-wrap: anywhere`, `ProfileCard.css:15`) | let the name wrap. Long-name fixture test. |
| d | Card max height and scroll | `overflow_hidden`, no max height (`mod.rs:846-856`) | `max-height: calc(100dvh - 16px); overflow: hidden auto` (`ProfileCard.css:3`) | `max_h(viewport - 16)` + `overflow_y_scroll`. Short-window test that the actions stay reachable. |
| e | Bio box | unbounded lines (`mod.rs:763-772`) | `max-height: 8rem`, scrolls, tab stop while it overflows (`ProfileCard.css:20`, tsx:90-94, 206) | cap at 8rem with scroll. |
| f | Placement | pointer rule, mirrors tsx:136-143 (`screens/members/mod.rs:596-604`) and covers the clicked row when the aside is at the window's right edge (as in the screenshot: card x 33-334, pointer about 348). Web does the same. | same | none (matches spec). Keyboard-opened card: web places it beside the row (tsx:144-147). Desktop always uses `window.mouse_position()` (`screens/members/mod.rs:438`), so Enter on a row opens the card at a stale pointer position. Use the row's bounds (`laid_out`) for keyboard opens. |
| g | Enter animation, shadow, band gradient, 56px avatar at -28px, title mt10, roles mt12 gap6, actions mt14 gap6, 34px buttons, WEB tag, status line | match (measured in screenshot: avatar centred on the band edge at y≈91, 16px body padding) | | none |

**Most likely "the GUI bug" in the screenshot:** #1, the band's square corners outside the card's rounded border (confirmed
by Jonas and visible at zoom). The second complaint, "no close button", is really missing outside-click dismissal, focus
not moved into the card (so Escape fails once focus leaves the pane), and Message and Compare doing nothing.
