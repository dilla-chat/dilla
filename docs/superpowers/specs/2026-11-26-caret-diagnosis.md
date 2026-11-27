# Composer caret invisible: diagnosis

Worktree: /home/thim/Repositories/dilla/.claude/worktrees/signal-design (HEAD 24bb67390). Nothing edited.
Crates: gpui-component =0.9.1, gpui-base 0.9.1, gpui-pre =0.3.12 (cargo registry, not vendored).

## Root cause

The caret is painted, but in near-black on the near-black field. Dilla never sets the component
library's theme, so the caret keeps gpui-component's *Default Light* colour.

1. `clients/desktop/dilla-desktop/src/lib.rs:124-125`: `install` calls `dilla_desktop_ui::theme::install(cx)`
   (fonts only, `dilla-desktop-ui/src/theme.rs:36-40`), then `gpui_component::init(cx)`. Nothing after that
   changes `gpui_component::Theme`. A grep for `Theme::change`, `Theme::update`, `global_mut`, `.caret =` and
   `sync_system_appearance` in dilla-desktop and dilla-desktop-ui finds nothing, and `git log -S` shows no
   such override ever existed.
2. `gpui-component-0.9.1/src/theme/mod.rs:36-40`: `init` runs `Theme::change(ThemeMode::Light, None, cx)`.
   `src/theme/default-theme.json:20` sets the light theme's `"caret": "#0a0a0a"`. Line 227 has the dark
   theme's `#fafafa`, which dilla never selects.
3. `gpui-component-0.9.1/src/input/input.rs:562-569`: each `Input`/`Textarea` render sets the editor style
   with `caret: cx.theme().caret`. That colour is an explicit, non-transparent value, so gpui-base's
   "unset means foreground" fallback (`gpui-base-0.9.1/src/input/editor/highlighting.rs:157`) never applies.
4. `gpui-base-0.9.1/src/input/base/element.rs:4030-4036` paints the caret with
   `paint_quad(fill(snap_caret_to_device_pixels(bounds, scale), editor_style.caret))` when `focused && show_cursor`.
5. The composer frame is `color::SURFACE_2` = #0F1216 (`dilla-desktop/src/screens/composer.rs:840`,
   `dilla-desktop-ui/src/tokens.rs:19`). The contrast of #0A0A0A on #0F1216 is about 1.04:1, so the caret
   cannot be seen. The glyphs are visible only because they take the inherited `text_color(color::FG)`
   (`composer.rs:956-960`), not a theme colour.

### Ruled out
- **PR #199 (0378158bf).** It touched only `dilla-desktop-ui/src/motion.rs` (removed `caret_at`, `caret_on`,
  `caret_next_stop`, `CaretEpoch` and `blink()`, plus their tests) and `screens/titlebar.rs`. No text field used
  `motion::blink`. The input caret is gpui-base's own `BlinkCursor`. `tokens.rs:384` `motion::CARET` (1000 ms)
  is unchanged and is not read by the input. `tokens.rs` is not in the diff. It did not cause this regression.
  The caret has been invisible since the GPUI client's first input.
- **Width or rounding at scale 1.** `CURSOR_WIDTH = px(1.)` (`blink_cursor.rs:8`), and
  `snap_caret_to_device_pixels` (`element.rs:2959-2963`) clamps the width to at least 1 device px.
- **Blink stuck off, or reduced motion.** `BlinkCursor::start` on focus sets it visible
  (`blink_cursor.rs:113-119`). The policy is the default 500 ms blink, and dilla's reduce-motion does not touch
  `cursor_blink`.
- **Focus on a wrapper.** `show_cursor` (`gpui-base .../state.rs:3998-4003`) requires the input's own
  `focus_handle` to be focused and the window to be active. Typing works, so the input is focused.
  `tracked_focus` only observes the handle.

## Affected fields
All of them, because they share `cx.theme().caret`:
- the composer (`screens/composer.rs:972`)
- the message edit box (`messages/editor.rs:117`)
- the thread reply (`threads/view.rs:779`)
- moderation reason (`messages/moderation.rs:130`)
- welcome template (`server_settings/welcome.rs:508`)
- every `TextField` (`dilla-desktop-ui/src/components/text_field.rs:309,332,364`): settings, sign-in, create flows
- search (`screens/search/mod.rs:576`)
- attachments caption (`attachments/mod.rs:1112`)
- self roles (`members/self_roles.rs:398`)
- poll form (`commands/poll_form.rs:789`)
- reaction or GIF search (`components/reactions/grid.rs:395`)
- onboarding (`components/onboarding/frame.rs:874`)

DM and channel composers are the same component.

Not affected: the TOTP code boxes (`factor/code_boxes.rs`), which draw their own box. In the old client their
native input is `caret-color: transparent` on purpose (`packages/ui/src/CodeBoxes/CodeBoxes.css:10`).

The rest of the light theme still applies under the dark UI. In particular `cx.theme().selection` is the light
theme's selection colour. That is worth checking when fixing, but it is outside this report.

## Intended look
The Signal `packages/ui` and `packages/web` CSS sets no `caret-color` for text fields. Only the old Mesh handoff
set `caret-color: var(--accent)`, in `docs/design/reference/mesh-handoff/mesh-chrome.css:454`, and Signal replaces
Mesh. A browser caret therefore follows `currentColor`, which is the field's text colour `--fg` = `color::FG`
(#E1E5EB, `tokens.rs:29`). It is 1 CSS px wide, the full line height, and blinks at the platform rate (about 500 ms
on and off). gpui-base already does 1 px, the line height, and a 500 ms blink, so only the colour is wrong.

## Minimal fix
In `clients/desktop/dilla-desktop/src/lib.rs`, after `gpui_component::init(cx);` (line 125):

```rust
gpui_component::Theme::update(cx, |theme| theme.caret = dilla_desktop_ui::tokens::color::FG);
```

Use `Theme::update`, not `global_mut`, so the tokens and the Base projection stay in sync and windows refresh.
Do not switch to `ThemeMode::Dark` just for the caret: it would also restyle other component colours. A separate
pass could project dilla's palette (selection, and so on) onto the theme.

## Failing test that proves it
Unit test, cheapest. Put it in `dilla-desktop/src/lib.rs` tests:

```rust
#[gpui::test]
fn text_fields_draw_their_caret_in_the_text_colour(cx: &mut gpui::TestAppContext) {
    cx.update(crate::install);
    cx.update(|cx| {
        use gpui_component::ActiveTheme as _;
        assert_eq!(cx.theme().caret, dilla_desktop_ui::tokens::color::FG,
            "a caret in the light theme's #0a0a0a is invisible on --surface-2");
    });
}
```

It fails today (`caret` = #0A0A0A) and passes with the fix.

End-to-end, using the vendored `Window::painted_quads` (`gpui-pre-0.3.12/src/window.rs:3112`, test-support):
1. Open the composer as the existing composer tests do.
2. Activate the window and focus `self.input`.
3. Run until parked, then `simulate_next_frame`.
4. Take `painted_quads()` and keep the quads 1 px wide whose height is the composer's `FIELD_LINE`, inside the
   composer's bounds.
5. Assert that exactly one exists and that its background is `color::FG`. A WCAG contrast of at least 3:1 against
   `SURFACE_2` also works.

Today that quad exists but is #0A0A0A.

No existing parity screenshot or test asserts a caret. Parity shots are taken unfocused, and the only "caret"
test is the title bar's `the_top_bar_has_no_blinking_caret` (`screens/titlebar.rs:894`).

I did not run a test: it needs a new test file, and this task allows no edits. The evidence is the static colour
constants above.
