//! Task 15's components (`components/server_settings/`): the server settings' own measures. The sections' markup is
//! `packages/web/src/screens/admin/*.css` (`.dw-settings-section`, `.dw-admin-*`, `.d-manage-list__*`,
//! `.d-invite-panel__*`); the screens take every gap, pad and height from here, never from a literal (R21).

pub mod permission_grid;

/// The spacing of the sections.
pub mod layout {
    use gpui::{Pixels, px};

    /// The gap between the blocks of a section (`.dw-settings-section` is a 14px column).
    pub const SECTION_GAP: Pixels = px(14.0);
    /// The space under a channel group (`--group-gap`).
    pub const GROUP_GAP: Pixels = px(14.0);
    /// The gap between a manage-list row's parts and under a micro heading.
    pub const ROW_GAP: Pixels = px(8.0);
    /// The height of a group's head row (`.dw-admin-grouphead.d-manage-list__row`: 48px).
    pub const HEAD_H: Pixels = px(48.0);
    /// A manage-list row's side padding.
    pub const ROW_PAD_X: Pixels = px(12.0);
    /// The gap between controls beside each other.
    pub const FIELD_GAP: Pixels = px(10.0);
    /// The space over a micro heading (the group heading's 22px).
    pub const HEADING_TOP: Pixels = px(22.0);
    /// A live invite row's padding and gap.
    pub const INVITE_PAD_X: Pixels = px(16.0);
    pub const INVITE_GAP: Pixels = px(12.0);
    /// The gap between the two choices of the invite maker (`.d-invite-panel__choices`).
    pub const CHOICE_GAP: Pixels = px(16.0);
    /// A live link's expiry and uses columns, and its code's flex basis (`.d-invite-panel__meta`, `__code`).
    pub const INVITE_META_W: Pixels = px(110.0);
    pub const INVITE_CODE_BASIS: Pixels = px(200.0);
    /// The side padding of a tag chip.
    pub const TAG_PAD_X: Pixels = px(5.0);
    /// What a manage-list row takes off a settings row's group height: its own padding.
    const ROW_TRIM: f32 = 16.0;

    /// The space between a channel list's rows (`.dw-admin-rows.d-manage-list { row-gap: var(--list-gap) }`, 1px): each row
    /// keeps its 48px, the hairline over it drawn inside (an inset shadow), so the rows step 49px.
    pub const LIST_GAP: Pixels = px(1.0);
    /// A manage-list row (`.d-manage-list__row`): 48px and 16px across, in a card of one hairline edge.
    pub const LIST_ROW_H: Pixels = px(48.0);
    pub const LIST_PAD_X: Pixels = px(16.0);
    /// The space between a group's head and its card (`--row-gap`).
    pub const HEAD_GAP: Pixels = px(2.0);
    /// The gap between the blocks of the GIF settings (1.5rem).
    pub const GIFS_GAP: Pixels = px(24.0);

    /// The GIF settings' column (`.dw-gifs-settings` in its stacked row's content-sized track): as wide as its widest
    /// part's max-content: the link (`--text-base`), the key's row (its label, then Replace and Remove) or the key form
    /// (a bare input and its buttons side by side; the field's 20rem flex basis then wraps them under it).
    pub fn gifs_column_width(
        link: &str,
        set_row: Option<(&str, &[&str])>,
        form: Option<&[&str]>,
    ) -> Pixels {
        let base = f32::from(crate::tokens::text::BASE) * crate::text::MONO_ADVANCE;
        let mut w = link.chars().count() as f32 * base;
        if let Some((label, buttons)) = set_row {
            // `.dw-gifs-settings__set`: 0.75rem apart, the label 0.5rem further.
            let row = label.chars().count() as f32 * base
                + 8.0
                + buttons
                    .iter()
                    .map(|b| f32::from(GIFS_FORM_GAP) + button_width(b))
                    .sum::<f32>();
            w = w.max(row);
        }
        if let Some(buttons) = form {
            let row = f32::from(text_input_width())
                + buttons
                    .iter()
                    .map(|b| f32::from(GIFS_FORM_GAP) + button_width(b))
                    .sum::<f32>();
            w = w.max(row);
        }
        px(w)
    }

    /// The gap of the GIF key form (`.dw-gifs-settings__form`: 0.75rem).
    pub const GIFS_FORM_GAP: Pixels = px(12.0);

    /// A default button's width (`Button.css`: 12px across, a 1px edge, 11.5px mono).
    pub fn button_width(label: &str) -> f32 {
        let advance = f32::from(crate::tokens::text::BTN) * crate::text::MONO_ADVANCE;
        2.0 * 12.0 + 2.0 + label.chars().count() as f32 * advance
    }
    /// The gap between a row's four trailing slots.
    pub const TRAIL_GAP: Pixels = px(4.0);

    /// The height of a text field's frame (`TextField.css`: 2.25rem).
    pub const FIELD_H: Pixels = px(36.0);
    /// A text field's side padding (`0.75rem`) and edge (`1px`): `.d-text-field__input`.
    const FIELD_PAD_X: f32 = 12.0;
    const FIELD_EDGE: f32 = 1.0;
    /// The gap between a text field's label and its frame.
    const LABEL_GAP_PX: f32 = 6.0;
    /// The same gap as a length (an invite choice's label over its control).
    pub const LABEL_GAP: Pixels = px(LABEL_GAP_PX);

    /// How far up a text field is drawn so its label is out of sight: `.dw-overview-field` hides the label (it stays the
    /// accessible name), which on the web takes no room.
    pub fn hidden_label_offset() -> Pixels {
        px(-(crate::text::line_height(crate::tokens::text::XS, 1.5) + LABEL_GAP_PX))
    }

    /// The web font's average and widest glyph, per em (the fontsource `JetBrains Mono Variable` the web loads: OS/2
    /// `xAvgCharWidth` 602, head bbox `xMax - xMin` 1170 + 1735 per 1000 units). The embedded instance has a narrower bbox
    /// (its ligature glyphs cut), so these are the web's.
    const WEB_MONO_AVG: f32 = 0.602;
    const WEB_MONO_BBOX: f32 = 2.905;

    /// The width of a bare text input: Chromium's intrinsic width of `<input size=20>`, twenty average characters (each
    /// rounded to a whole pixel, as `SimpleFontData` keeps them) plus the widest glyph's excess over one, then the frame's
    /// side padding and edge (`LayoutTextControlSingleLine::PreferredContentLogicalWidth`).
    pub fn text_input_width() -> Pixels {
        let columns = 20.0;
        let type_size: f32 = crate::tokens::text::INPUT.into();
        let avg = (WEB_MONO_AVG * type_size).round();
        let widest = (WEB_MONO_BBOX * type_size).round();
        px(columns * avg + (widest - avg) + 2.0 * (FIELD_PAD_X + FIELD_EDGE))
    }

    /// The height of a manage-list row of a group position's minimum settings-row height.
    pub fn list_row_height(min_height: f32) -> Pixels {
        px(min_height - ROW_TRIM)
    }
}

#[cfg(test)]
mod tests {
    use super::layout;

    #[test]
    fn the_gif_column_is_the_form_side_by_side() {
        // 216 + 12 + 53.6: the web's 281.6px column.
        let w =
            layout::gifs_column_width("get a key at developers.giphy.com", None, Some(&["save"]));
        assert!((f32::from(w) - 281.6).abs() < 0.01);
        // The key's row: 18 characters of 13.5px, 8px, then Replace and Remove 12px apart: 319.5.
        let set = layout::gifs_column_width(
            "get a key at developers.giphy.com",
            Some(("a giphy key is set", &["replace", "remove"])),
            None,
        );
        assert!((f32::from(set) - 319.5).abs() < 0.01);
    }

    #[test]
    fn a_bare_input_is_chromiums_size_twenty() {
        // 20 x 8 + (38 - 8) + 2 x 13 at 13px: the web's 216px Server name field.
        assert_eq!(f32::from(layout::text_input_width()), 216.0);
    }
}
