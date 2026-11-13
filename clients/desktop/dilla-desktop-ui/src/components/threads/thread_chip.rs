//! The thread chip under a root (MessageRow.css `.d-message-row__thread`, §5.5): 24px, radius 5px, a hairline frame, the
//! accent text and, seen only, "· last 20:33" in `--fg-3`. A press opens the thread.

use std::rc::Rc;

use gpui::{
    App, ClickEvent, ElementId, FocusHandle, InteractiveElement, IntoElement, ParentElement,
    RenderOnce, SharedString, StatefulInteractiveElement, Styled, Window, div, px,
};

use crate::icon::Icon;
use crate::icon_names::IconName;
use crate::text::{line, line_height};
use crate::tokens::{color, radius, text};

/// `height: 1.5rem`, `margin-top: 0.25rem`, padding 0 0.5rem, gap 0.375rem, the glyph 12px.
const HEIGHT: f32 = 24.0;
const MARGIN_TOP: f32 = 4.0;
/// The row's main column is a grid with `gap: 1px` (MessageRow.css `.d-message-row__main`), so the chip sits 1px lower.
const GRID_GAP: f32 = 1.0;
const PAD_X: f32 = 8.0;
const GAP: f32 = 6.0;
const GLYPH: f32 = 12.0;

/// What the chip takes under a row's body: its box and the margin above it.
pub fn height() -> f32 {
    GRID_GAP + MARGIN_TOP + HEIGHT
}

type Open = Rc<dyn Fn(&ClickEvent, &mut Window, &mut App)>;

#[derive(IntoElement)]
pub struct ThreadChip {
    id: ElementId,
    label: SharedString,
    last: Option<SharedString>,
    focus: Option<FocusHandle>,
    on_open: Option<Open>,
}

impl ThreadChip {
    /// `label` is "2 replies"; `last` the latest reply's time as the chip prints it ("last 20:33").
    pub fn new(id: impl Into<ElementId>, label: impl Into<SharedString>) -> Self {
        Self {
            id: id.into(),
            label: label.into(),
            last: None,
            focus: None,
            on_open: None,
        }
    }

    pub fn last(mut self, last: impl Into<SharedString>) -> Self {
        self.last = Some(last.into());
        self
    }

    /// The chip is a tab stop that Enter and Space press, and the pane gives focus back to it when it closes.
    pub fn focus(mut self, handle: FocusHandle) -> Self {
        self.focus = Some(handle);
        self
    }

    pub fn on_open(mut self, f: impl Fn(&ClickEvent, &mut Window, &mut App) + 'static) -> Self {
        self.on_open = Some(Rc::new(f));
        self
    }
}

impl RenderOnce for ThreadChip {
    fn render(self, _window: &mut Window, _cx: &mut App) -> impl IntoElement {
        let size = text::XS;
        let lh = line_height(size, 1.5);
        let mut chip = div()
            .id(self.id)
            .flex()
            .flex_none()
            .items_center()
            .gap(px(GAP))
            .h(px(HEIGHT))
            .px(px(PAD_X))
            .border_1()
            .border_color(color::HAIRLINE_2)
            .rounded(radius::ROW)
            .cursor_pointer()
            .hover(|s| s.bg(color::ACCENT_HOVER).border_color(color::ACCENT))
            .child(
                Icon::new(IconName::MessagesSquare)
                    .size(px(GLYPH))
                    .color(color::ACCENT),
            )
            .child(line(self.label, size, lh, color::ACCENT));
        if let Some(last) = self.last {
            chip = chip.child(line(format!("· {last}"), size, lh, color::FG_3));
        }
        if let Some(handle) = &self.focus {
            chip = chip
                .track_focus(handle)
                .tab_stop(true)
                .focus_visible(|s| s.border_color(color::ACCENT));
        }
        if let Some(open) = self.on_open {
            let press = open.clone();
            chip = chip
                .on_click(move |e, window, cx| open(e, window, cx))
                .on_key_down(move |event, window, cx| {
                    if matches!(event.keystroke.key.as_str(), "enter" | "space") {
                        cx.stop_propagation();
                        press(&ClickEvent::default(), window, cx);
                    }
                });
        }
        div().mt(px(GRID_GAP + MARGIN_TOP)).flex().child(chip)
    }
}

#[cfg(test)]
mod tests {
    #[test]
    fn the_chip_is_24px_under_a_4px_margin_and_the_grids_1px_gap() {
        assert_eq!(super::height(), 29.0);
    }
}
