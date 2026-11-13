//! `PermissionGrid` (packages/ui/src/PermissionGrid), tri-state mode: groups of permission rows, each a label over its hint
//! with Allow, Default and Deny as one segmented choice. A row can be disabled (a right this person does not hold, or an
//! entry above their rank): it is shown, not changeable.

use std::rc::Rc;

use gpui::{
    App, ElementId, InteractiveElement, IntoElement, ParentElement, RenderOnce, SharedString,
    Styled, Window, div,
};

use super::layout;
use crate::components::segmented::Segmented;
use crate::metrics::LABEL_TRACKING_WIDE;
use crate::text::{line, line_height};
use crate::tokens::{color, font, text};

/// One permission row: its id (the host's key), label, hint, current value (`allow`, `inherit` or `deny`) and whether it
/// can be changed.
pub struct GridRow {
    pub id: SharedString,
    pub label: SharedString,
    pub hint: SharedString,
    pub value: &'static str,
    pub disabled: bool,
}

pub struct GridGroup {
    pub title: SharedString,
    pub rows: Vec<GridRow>,
}

type Change = Rc<dyn Fn(&str, &str, &mut Window, &mut App) + 'static>;

#[derive(IntoElement)]
pub struct PermissionGrid {
    id: ElementId,
    groups: Vec<GridGroup>,
    /// The three choices' labels, in order: allow, inherit, deny.
    labels: [SharedString; 3],
    on_change: Option<Change>,
}

impl PermissionGrid {
    pub fn new(
        id: impl Into<ElementId>,
        groups: Vec<GridGroup>,
        labels: [SharedString; 3],
    ) -> Self {
        Self {
            id: id.into(),
            groups,
            labels,
            on_change: None,
        }
    }

    /// `(row id, new value)`.
    pub fn on_change(
        mut self,
        handler: impl Fn(&str, &str, &mut Window, &mut App) + 'static,
    ) -> Self {
        self.on_change = Some(Rc::new(handler));
        self
    }
}

impl RenderOnce for PermissionGrid {
    fn render(self, _window: &mut Window, _cx: &mut App) -> impl IntoElement {
        let [allow, inherit, deny] = &self.labels;
        let id_text = self.id_text();
        let mut column = div().flex().flex_col().id(self.id.clone());
        for group in self.groups {
            column = column.child(
                div().pt(layout::HEADING_TOP).pb(layout::ROW_GAP).child(
                    line(
                        group.title.to_uppercase(),
                        text::MICRO,
                        line_height(text::MICRO, 1.5),
                        color::FG_3,
                    )
                    .weight(font::WEIGHT_LABEL)
                    .tracking(LABEL_TRACKING_WIDE),
                ),
            );
            for row in group.rows {
                let handler = self.on_change.clone();
                let row_id = row.id.clone();
                let choices = [
                    ("allow", allow.as_ref()),
                    ("inherit", inherit.as_ref()),
                    ("deny", deny.as_ref()),
                ];
                let mut control = Segmented::new(
                    SharedString::from(format!("{}-{}", id_text, row.id)),
                    &choices,
                    row.value,
                )
                .disabled(row.disabled);
                if let Some(handler) = handler {
                    control = control
                        .on_change(move |value, window, app| handler(&row_id, value, window, app));
                }
                column = column.child(
                    div()
                        .flex()
                        .items_center()
                        .justify_between()
                        .gap(layout::INVITE_GAP)
                        .py(layout::ROW_GAP)
                        .child(
                            div()
                                .flex()
                                .flex_col()
                                .min_w_0()
                                .child(line(
                                    row.label,
                                    text::SM,
                                    line_height(text::SM, 1.5),
                                    color::FG,
                                ))
                                .child(line(
                                    row.hint,
                                    text::XS,
                                    line_height(text::XS, 1.5),
                                    color::FG_2,
                                )),
                        )
                        .child(control),
                );
            }
        }
        column
    }
}

impl PermissionGrid {
    fn id_text(&self) -> String {
        format!("{:?}", self.id)
    }
}
