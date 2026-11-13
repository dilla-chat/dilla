//! The DM picker's list (Shell.tsx `DmPicker`, app.css `dw-dm-picker`): one row per member, the name (wrapping inside its
//! row) and its small "message" button; rows `--row-gap` apart, the name and button `--group-gap` apart. The row the
//! keyboard is on (type-ahead and arrows) carries the hover fill. Automation ids: `dm-pick-<i>` (the row) and
//! `dm-pick-<i>-message` (its button).

use std::rc::Rc;

use gpui::{
    App, InteractiveElement, IntoElement, ParentElement, RenderOnce, SharedString, Styled, Window,
    div,
};

use crate::components::button::{Button, ButtonSize};
use crate::metrics::Density;
use crate::tokens::{color, radius, text};

/// One member the picker offers.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct PickerMember {
    pub name: SharedString,
}

type PickHandler = Rc<dyn Fn(usize, &mut Window, &mut App) + 'static>;

#[derive(IntoElement)]
pub struct DmPickerList {
    members: Vec<PickerMember>,
    selected: Option<usize>,
    busy: bool,
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
            button: button.into(),
            on_pick: Rc::new(on_pick),
        }
    }

    pub fn selected(mut self, index: Option<usize>) -> Self {
        self.selected = index;
        self
    }

    /// While an open is out the buttons do nothing (`aria-disabled`).
    pub fn busy(mut self, busy: bool) -> Self {
        self.busy = busy;
        self
    }
}

impl RenderOnce for DmPickerList {
    fn render(self, _window: &mut Window, _cx: &mut App) -> impl IntoElement {
        let density = Density::Regular;
        let busy = self.busy;
        div()
            .id("dm-picker")
            .flex()
            .flex_col()
            .gap(density.row_gap())
            .children(self.members.into_iter().enumerate().map(|(i, m)| {
                let pick = self.on_pick.clone();
                let mut row = div()
                    .id(("dm-pick", i))
                    .flex()
                    .justify_between()
                    .items_center()
                    .gap(density.group_gap())
                    .rounded(radius::ROW)
                    .child(
                        div()
                            .min_w_0()
                            .flex_1()
                            .text_size(text::SM)
                            .text_color(color::FG)
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
                if self.selected == Some(i) {
                    row = row.bg(color::HOVER);
                }
                row
            }))
    }
}
