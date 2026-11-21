//! The DM picker's list (Shell.tsx `DmPicker`, app.css `dw-dm-picker`): one row per member, the name (wrapping inside its
//! row) and its small "message" button; rows `--row-gap` apart, the name and button `--group-gap` apart. The row the
//! keyboard is on (type-ahead and arrows) carries the hover fill. Automation ids: `dm-pick-<i>` (the row) and
//! `dm-pick-<i>-message` (its button).

use std::rc::Rc;

use gpui::StatefulInteractiveElement as _;
use gpui::prelude::FluentBuilder as _;
use gpui::{
    App, InteractiveElement, IntoElement, ParentElement, RenderOnce, SharedString, Styled, Window,
    div,
};

use crate::a11y::{Accessible, Role};
use crate::components::button::{Button, ButtonSize};
use crate::metrics::Density;
use crate::tokens::{color, radius, text};

/// One member the picker offers.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct PickerMember {
    pub name: SharedString,
}

/// A member's name: the dialog body's colour (`.d-dialog__body { color: var(--fg-2) }`; `.dw-dm-picker__name` sets none).
pub const NAME_COLOR: gpui::Hsla = color::FG_2;

/// And its size: `.d-dialog__body { font-size: var(--text-ui) }`.
pub const NAME_SIZE: gpui::Pixels = text::UI;

/// Whether row `i` carries the keyboard's fill: the keyboard is only on a row while the list holds the focus.
fn highlighted(selected: Option<usize>, i: usize, focused: bool) -> bool {
    focused && selected == Some(i)
}

type PickHandler = Rc<dyn Fn(usize, &mut Window, &mut App) + 'static>;

#[derive(IntoElement)]
pub struct DmPickerList {
    members: Vec<PickerMember>,
    selected: Option<usize>,
    busy: bool,
    focused: bool,
    button: SharedString,
    on_pick: PickHandler,
}

impl DmPickerList {
    /// `button` is the row button's label (`shell.dms.open`).
    pub fn new(
        members: Vec<PickerMember>,
        button: impl Into<SharedString>,
        on_pick: impl Fn(usize, &mut Window, &mut App) + 'static,
    ) -> Self {
        Self {
            members,
            selected: None,
            busy: false,
            focused: true,
            button: button.into(),
            on_pick: Rc::new(on_pick),
        }
    }

    pub fn selected(mut self, index: Option<usize>) -> Self {
        self.selected = index;
        self
    }

    /// Whether the list holds the focus: the selected row shows the keyboard's fill only then. Defaults to true.
    pub fn focused(mut self, focused: bool) -> Self {
        self.focused = focused;
        self
    }

    /// While an open is out the buttons do nothing (`aria-disabled`).
    pub fn busy(mut self, busy: bool) -> Self {
        self.busy = busy;
        self
    }
}

impl RenderOnce for DmPickerList {
    fn render(self, window: &mut Window, cx: &mut App) -> impl IntoElement {
        let density = Density::of(window, cx);
        let busy = self.busy;
        div()
            .id("dm-picker")
            .flex()
            .flex_col()
            .gap(density.row_gap())
            .children(self.members.into_iter().enumerate().map(|(i, m)| {
                let pick = self.on_pick.clone();
                let picked = self.selected == Some(i);
                let mut row = div()
                    .id(("dm-pick", i))
                    .a11y(Role::ListBoxOption, m.name.clone())
                    .aria_selected(picked)
                    .when(picked, |r| r.aria_active_descendant())
                    .flex()
                    .justify_between()
                    .items_center()
                    .gap(density.group_gap())
                    .rounded(radius::ROW)
                    .child(
                        div()
                            .min_w_0()
                            .flex_1()
                            .text_size(NAME_SIZE)
                            .text_color(NAME_COLOR)
                            .child(m.name),
                    )
                    .child(
                        div().flex_none().child(
                            Button::new(("dm-pick-message", i), self.button.clone())
                                .size(ButtonSize::Sm)
                                .disabled(busy)
                                .on_click(move |_, window, cx| {
                                    if !busy {
                                        pick(i, window, cx);
                                    }
                                }),
                        ),
                    );
                if highlighted(self.selected, i, self.focused) {
                    row = row.bg(color::HOVER);
                }
                row
            }))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_name_is_the_dialog_bodys_colour() {
        assert_eq!(NAME_COLOR, color::FG_2);
        assert_eq!(NAME_SIZE, text::UI);
    }

    #[test]
    fn the_keyboards_row_is_filled_only_while_the_list_has_the_focus() {
        assert!(highlighted(Some(1), 1, true));
        assert!(!highlighted(Some(1), 1, false));
        assert!(!highlighted(Some(1), 0, true));
        assert!(!highlighted(None, 0, true));
    }
}
