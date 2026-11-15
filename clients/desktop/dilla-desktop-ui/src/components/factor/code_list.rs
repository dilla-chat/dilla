//! `CodeList` (packages/ui/src/CodeList): one-time codes in a numbered list on `--surface-2` in the field edge, radius
//! `--r-lg`, 16px inside, each code normalised and grouped in fives joined by hyphens (the server's own spelling), its
//! number (`01`) in `--text-micro` `--fg-3` before it; then the Copy and Print actions. At most two columns, and two only
//! when each fits a whole code on one line (`20em + 2.25rem`).
//!
//! The codes are secrets: the host keeps them in a zeroizing buffer; this element holds a copy only for one render (and
//! GPUI's text system shapes its own copy of each shown line, which nothing here can wipe).

use gpui::prelude::FluentBuilder;
use gpui::{
    AnyElement, App, ElementId, InteractiveElement, IntoElement, ParentElement, RenderOnce,
    SharedString, Styled, Window, div, px,
};

use crate::text::line;
use crate::tokens::{color, font, radius, text};

/// Characters in a group.
const GROUP: usize = 5;
/// The grid's inset (`padding: 1rem`).
const INSET: f32 = 16.0;
/// The gaps (`0.75rem 1.5rem`).
const ROW_GAP: f32 = 12.0;
const COLUMN_GAP: f32 = 24.0;
/// The number's gap to its code (`gap: 0.75rem`).
const NUMBER_GAP: f32 = 12.0;
/// The label's and the actions' distance (`gap: 1rem`).
const STACK_GAP: f32 = 16.0;
/// `letter-spacing: 0.04em`.
const TRACKING: f32 = 0.04;

/// CodeList's `groups`: the separators dropped and the case raised, then groups of five, each but the last ending in a
/// hyphen (`"JBSWY-", "3DPEH-", …`).
pub fn group_code(code: &str) -> Vec<String> {
    let n: Vec<char> = code
        .chars()
        .filter(|c| !c.is_whitespace() && *c != '-')
        .map(|c| c.to_ascii_uppercase())
        .collect();
    n.chunks(GROUP)
        .enumerate()
        .map(|(i, g)| {
            let mut s: String = g.iter().collect();
            if (i + 1) * GROUP < n.len() {
                s.push('-');
            }
            s
        })
        .collect()
}

/// The text Copy hands the clipboard: the codes as shown, one per line.
pub fn copy_text(codes: &[String]) -> String {
    codes
        .iter()
        .map(|c| group_code(c).concat())
        .collect::<Vec<_>>()
        .join("\n")
}

/// How many columns fit in a list `width` px wide (CodeList.css `minmax(max(20em + 2.25rem, …))`).
pub fn columns(width: f32) -> usize {
    let size = f32::from(text::BASE);
    let cell = 20.0 * size + 2.25 * 16.0;
    if width - 2.0 * INSET >= 2.0 * cell + COLUMN_GAP {
        2
    } else {
        1
    }
}

#[derive(IntoElement)]
pub struct CodeList {
    id: ElementId,
    label: SharedString,
    codes: Vec<String>,
    columns: usize,
    actions: Vec<AnyElement>,
}

impl CodeList {
    pub fn new(id: impl Into<ElementId>, label: impl Into<SharedString>, codes: &[String]) -> Self {
        Self {
            id: id.into(),
            label: label.into(),
            codes: codes.to_vec(),
            columns: 1,
            actions: Vec::new(),
        }
    }

    /// The columns the host's width fits ([`columns`]).
    pub fn columns(mut self, columns: usize) -> Self {
        self.columns = columns.clamp(1, 2);
        self
    }

    /// The actions under the list (Copy, Print), drawn in a wrapping row 12px apart.
    pub fn action(mut self, action: impl IntoElement) -> Self {
        self.actions.push(action.into_any_element());
        self
    }
}

impl RenderOnce for CodeList {
    fn render(mut self, _window: &mut Window, _cx: &mut App) -> impl IntoElement {
        let base = f32::from(text::BASE);
        let micro = f32::from(text::MICRO);
        let codes = std::mem::take(&mut self.codes);
        let cells: Vec<AnyElement> = codes
            .iter()
            .enumerate()
            .map(|(i, code)| {
                div()
                    .flex()
                    .items_baseline()
                    .gap(px(NUMBER_GAP))
                    .min_w_0()
                    .child(line(
                        format!("{:02}", i + 1),
                        text::MICRO,
                        micro * 1.5,
                        color::FG_3,
                    ))
                    .child(div().flex().flex_wrap().min_w_0().children(
                        group_code(code).into_iter().map(|g| {
                            line(g, text::BASE, base * 1.5, color::FG)
                                .weight(font::WEIGHT_LABEL)
                                .tracking(TRACKING)
                        }),
                    ))
                    .into_any_element()
            })
            .collect();
        let columns = self.columns;
        let mut rows = Vec::new();
        let mut cells = cells.into_iter().peekable();
        while cells.peek().is_some() {
            let row: Vec<AnyElement> = cells.by_ref().take(columns).collect();
            let filled = row.len();
            rows.push(
                div()
                    .flex()
                    .gap(px(COLUMN_GAP))
                    .children(row.into_iter().map(|c| div().flex_1().min_w_0().child(c)))
                    .when(filled < columns, |d| d.child(div().flex_1()))
                    .into_any_element(),
            );
        }
        let actions = std::mem::take(&mut self.actions);
        let label = self.label.clone();
        div()
            .id(self.id.clone())
            .flex()
            .flex_col()
            .gap(px(STACK_GAP))
            .min_w_0()
            .child(
                line(label, text::XS, f32::from(text::XS) * 1.5, color::FG_2)
                    .weight(font::WEIGHT_LABEL),
            )
            .child(
                div()
                    .p(px(INSET))
                    .flex()
                    .flex_col()
                    .gap(px(ROW_GAP))
                    .bg(color::SURFACE_2)
                    .border_1()
                    .border_color(color::CONTROL_EDGE_FIELD)
                    .rounded(radius::LG)
                    .children(rows),
            )
            .when(!actions.is_empty(), |d| {
                d.child(div().flex().flex_wrap().gap(px(ROW_GAP)).children(actions))
            })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// CodeList.test.tsx: normalised, in groups of five with hyphens; Copy hands them one per line.
    #[test]
    fn codes_are_grouped_in_fives() {
        assert_eq!(
            group_code("jbswy3dpehpk3pxpjbswy3dpeh"),
            ["JBSWY-", "3DPEH-", "PK3PX-", "PJBSW-", "Y3DPE-", "H"]
        );
        assert_eq!(group_code("ABCDE-FGHIJ"), ["ABCDE-", "FGHIJ"]);
        assert_eq!(
            copy_text(&["abcdefghij".into(), "KLMNO".into()]),
            "ABCDE-FGHIJ\nKLMNO"
        );
    }

    #[test]
    fn two_columns_only_when_a_whole_code_fits() {
        assert_eq!(columns(344.0), 1);
        assert_eq!(columns(700.0), 2);
    }
}
