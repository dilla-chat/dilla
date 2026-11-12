//! Task 15's components (`components/server_settings/`): the server settings' own measures. The sections' markup is
//! `packages/web/src/screens/admin/*.css` (`.dw-settings-section`, `.dw-admin-*`, `.d-manage-list__*`,
//! `.d-invite-panel__*`); the screens take every gap, pad and height from here, never from a literal (R21).

/// The spacing of the sections.
pub mod layout {
    use gpui::{Pixels, px};

    /// The gap between the blocks of a section (`.dw-settings-section` is a 14px column).
    pub const SECTION_GAP: Pixels = px(14.0);
    /// The space under a channel group.
    pub const GROUP_GAP: Pixels = px(18.0);
    /// The gap between a manage-list row's parts and under a micro heading.
    pub const ROW_GAP: Pixels = px(8.0);
    /// The height of a group's head row.
    pub const HEAD_H: Pixels = px(40.0);
    /// A manage-list row's side padding.
    pub const ROW_PAD_X: Pixels = px(12.0);
    /// The gap between controls beside each other.
    pub const FIELD_GAP: Pixels = px(10.0);
    /// The space over a micro heading (the group heading's 22px).
    pub const HEADING_TOP: Pixels = px(22.0);
    /// A live invite row's padding and gap.
    pub const INVITE_PAD_X: Pixels = px(16.0);
    pub const INVITE_GAP: Pixels = px(12.0);
    /// The side padding of a tag chip.
    pub const TAG_PAD_X: Pixels = px(5.0);
    /// What a manage-list row takes off a settings row's group height: its own padding.
    const ROW_TRIM: f32 = 16.0;

    /// A manage-list row (`.d-manage-list__row`): 48px and 16px across, in a card of one hairline edge.
    pub const LIST_ROW_H: Pixels = px(48.0);
    pub const LIST_PAD_X: Pixels = px(16.0);
    /// The space between a group's heading and its card.
    pub const HEAD_GAP: Pixels = px(6.0);
    /// The gap between the blocks of the GIF settings (1.5rem).
    pub const GIFS_GAP: Pixels = px(24.0);
    /// The gap between a row's four trailing slots.
    pub const TRAIL_GAP: Pixels = px(4.0);

    /// The height of a text field's frame (`TextField.css`: 2.25rem).
    pub const FIELD_H: Pixels = px(36.0);
    /// The gap between a text field's label and its frame.
    const LABEL_GAP: f32 = 6.0;

    /// How far up a text field is drawn so its label is out of sight: `.dw-overview-field` hides the label (it stays the
    /// accessible name), which on the web takes no room.
    pub fn hidden_label_offset() -> Pixels {
        px(-(crate::text::line_height(crate::tokens::text::XS, 1.5) + LABEL_GAP))
    }

    /// The width of a bare text input: twenty columns (an input's default `size`) of the field's mono type, plus the
    /// frame's side padding and edge.
    pub fn text_input_width() -> Pixels {
        let columns = 20.0;
        let type_size: f32 = crate::tokens::text::INPUT.into();
        px(columns * type_size * crate::text::MONO_ADVANCE + 26.0)
    }

    /// The height of a manage-list row of a group position's minimum settings-row height.
    pub fn list_row_height(min_height: f32) -> Pixels {
        px(min_height - ROW_TRIM)
    }
}
