//! The Diagnostics settings section (packages/web/src/screens/settings/Diagnostics.tsx): the facts as settings rows, the
//! slowest socket requests, the last 20 stable codes and Copy codes.
//!
//! Device-local and code-only (L-TS-43): what is drawn and what Copy writes come from [`dilla_client::state::diagnostics`]
//! and the connection slice, nothing else.

use dilla_client::features::diagnostics::{copy_text, stable_code};
use dilla_client::state::diagnostics::{ErrorEntry, RouteTiming};
use dilla_client::state::{AppState, ConnectionStatus};
use gpui::{AnyElement, App, ClipboardItem, IntoElement, ParentElement, Styled, Window, div, px};

use dilla_desktop_ui::components::button::Button;
use dilla_desktop_ui::components::settings::frame::{Section, section_header};
use dilla_desktop_ui::components::settings::row::{GroupPosition, SettingsRow};
use dilla_desktop_ui::text::{MONO_ADVANCE, line, line_height};
use dilla_desktop_ui::tokens::{color, font, radius, text};

use crate::slots::SlotCx;
use crate::strings::{t, tf};
use crate::{format, tracked_box, tracked_focus};

/// The error codes shown: the last 20 (Diagnostics.tsx `slice(-20)`).
const SHOWN_ERRORS: usize = 20;
/// The routes shown: at most five (Diagnostics.tsx `slice(0, 5)`).
const SHOWN_ROUTES: usize = 5;

/// The table cells' horizontal padding in px (`.dw-diagnostics-routes__table` `padding: 10px 16px`, the `px_4` of a cell).
const CELL_PAD_X: f32 = 16.0;

/// One fact row: its automation id, the `strings.rs` key of its label and its value.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Fact {
    pub id: &'static str,
    pub label: &'static str,
    pub value: String,
}

/// `data.connection` as the web prints it: the raw status word.
fn connection_word(status: ConnectionStatus) -> &'static str {
    match status {
        ConnectionStatus::Offline => "offline",
        ConnectionStatus::Connecting => "connecting",
        ConnectionStatus::Online => "online",
    }
}

/// The four facts in the web's order: connection, generation, instance, devices. A generation not known yet is empty, as
/// the web's `{data.generation}` is; the instance and the device count fall back to the unknown dash.
pub fn facts(state: &AppState) -> Vec<Fact> {
    let unknown = || t("shell.status.unknown").to_owned();
    vec![
        Fact {
            id: "diag-connection",
            label: "diagnostics.connection",
            value: connection_word(state.connection.status).to_owned(),
        },
        Fact {
            id: "diag-generation",
            label: "diagnostics.generation",
            value: state
                .connection
                .generation
                .map_or_else(String::new, |g| g.to_string()),
        },
        Fact {
            id: "diag-instance",
            label: "diagnostics.instance",
            value: state
                .diagnostics
                .instance_name
                .clone()
                .unwrap_or_else(unknown),
        },
        Fact {
            id: "diag-devices",
            label: "diagnostics.devices",
            value: state
                .diagnostics
                .devices
                .map_or_else(unknown, |n| n.to_string()),
        },
    ]
}

/// The key packages value: the count (a dash while unknown), then when a publish was last tried and the code it failed
/// with; nothing is shown for what is not known. Joined by ` · `.
///
/// PLACEHOLDER SOURCE: the status is [`dilla_client::state::diagnostics::KeyPackageStatus`] in the diagnostics slice,
/// which stays empty until Task 2's key package maintenance (`Client::key_packages`) reports into
/// `Diagnostics::set_key_packages`. This function is the one place the screen reads it.
pub fn key_packages_value(state: &AppState) -> String {
    let status = &state.diagnostics.key_packages;
    let mut parts = vec![
        status
            .count
            .map_or_else(|| t("shell.status.unknown").to_owned(), |n| n.to_string()),
    ];
    if let Some(at) = status.last_attempt_ms {
        parts.push(format::clock(at));
    }
    if let Some(code) = &status.last_error {
        // Whatever the slice holds, only a stable code is drawn.
        parts.push(stable_code(code).to_owned());
    }
    parts.join(" · ")
}

/// The last 20 codes, oldest first.
pub fn shown_errors(state: &AppState) -> &[ErrorEntry] {
    let errors = &state.diagnostics.errors;
    &errors[errors.len().saturating_sub(SHOWN_ERRORS)..]
}

/// The slowest five routes.
pub fn shown_routes(state: &AppState) -> &[RouteTiming] {
    let routes = &state.diagnostics.slow_routes;
    &routes[..routes.len().min(SHOWN_ROUTES)]
}

fn value(text_: impl Into<gpui::SharedString>) -> AnyElement {
    div()
        .child(line(text_, text::SM, line_height(text::SM, 1.4), color::FG))
        .into_any_element()
}

/// The slowest-requests table: one group (10px radius, a hairline between rows), mono at `--text-sm`; cells 10px 16px,
/// the numbers right-aligned on one line, the route column taking what is left. Column widths are the widest cell of the
/// column (a table's auto layout), from the mono advance.
fn routes_table(routes: &[RouteTiming]) -> AnyElement {
    let size = text::SM;
    let head_size = text::XS;
    let ms = |v: u64| tf("diagnostics.slowRoutes.ms", &[("ms", &v.to_string())]);
    let rows: Vec<[String; 4]> = routes
        .iter()
        .map(|r| [r.pattern.clone(), ms(r.p50), ms(r.p95), r.count.to_string()])
        .collect();
    let heads = [
        t("diagnostics.slowRoutes.route"),
        t("diagnostics.slowRoutes.p50"),
        t("diagnostics.slowRoutes.p95"),
        t("diagnostics.slowRoutes.count"),
    ];
    // Widths of the three number columns: the widest of the header and its cells, plus the 16px either side.
    let mut widths = [0.0_f32; 3];
    for (i, width) in widths.iter_mut().enumerate() {
        let head = heads[i + 1].chars().count() as f32 * f32::from(head_size) * MONO_ADVANCE;
        let cells = rows
            .iter()
            .map(|r| r[i + 1].chars().count() as f32 * f32::from(size) * MONO_ADVANCE)
            .fold(0.0, f32::max);
        *width = head.max(cells) + 2.0 * CELL_PAD_X;
    }
    let cell = |text_: String, width: Option<f32>, head: bool| {
        let (size, ratio, colour) = if head {
            (head_size, 1.5, color::FG_2)
        } else {
            (size, 1.5, color::FG)
        };
        let base = div().px_4().py_2p5();
        let line = line(text_, size, line_height(size, ratio), colour);
        let line = if head {
            line.weight(font::WEIGHT_LABEL)
        } else {
            line
        };
        match width {
            Some(w) => base.flex_none().w(px(w)).flex().justify_end().child(line),
            None => base.flex_1().min_w_0().child(line),
        }
    };
    let mut table = div()
        .flex()
        .flex_col()
        .border_1()
        .border_color(color::HAIRLINE)
        .rounded(radius::XL)
        .overflow_hidden();
    let head = div()
        .flex()
        .child(cell(heads[0].to_owned(), None, true))
        .child(cell(heads[1].to_owned(), Some(widths[0]), true))
        .child(cell(heads[2].to_owned(), Some(widths[1]), true))
        .child(cell(heads[3].to_owned(), Some(widths[2]), true));
    table = table.child(head);
    for row in rows {
        let [route, p50, p95, count] = row;
        table = table.child(
            div()
                .flex()
                .border_t_1()
                .border_color(color::HAIRLINE)
                .child(cell(route, None, false))
                .child(cell(p50, Some(widths[0]), false))
                .child(cell(p95, Some(widths[1]), false))
                .child(cell(count, Some(widths[2]), false)),
        );
    }
    let heading = line(
        t("diagnostics.slowRoutes"),
        text::BASE,
        line_height(text::BASE, 1.5),
        color::FG_2,
    );
    div()
        .flex()
        .flex_col()
        .gap_2()
        .min_w_0()
        .child(heading)
        .child(table)
        .into_any_element()
}

/// The error list: each code with its time, newest last.
fn errors_list(errors: &[ErrorEntry]) -> AnyElement {
    div()
        .flex()
        .flex_col()
        .children(errors.iter().map(|e| {
            div()
                .flex()
                .items_center()
                .gap_2()
                .child(line(
                    e.code.clone(),
                    text::SM,
                    line_height(text::SM, 1.5),
                    color::FG,
                ))
                .child(line(
                    format::clock(e.at_ms),
                    text::SM,
                    line_height(text::SM, 1.5),
                    color::FG_2,
                ))
        }))
        .into_any_element()
}

/// The section's content, for the settings host to put in the frame's body.
pub fn render(ctx: &SlotCx, window: &mut Window, cx: &mut App) -> AnyElement {
    // Copy codes keeps its focus handle across frames, so Tab reaches it and the automation build can name it.
    let copy_focus = window
        .use_keyed_state("diag-copy-focus", cx, |_, cx| {
            cx.focus_handle().tab_stop(true)
        })
        .read(cx)
        .clone();
    let state = &ctx.state;
    let fact_rows = facts(state);
    let n = fact_rows.len() + 1;
    let mut group = div().flex().flex_col();
    for (i, fact) in fact_rows.iter().enumerate() {
        group = group.child(tracked_box(
            fact.id,
            SettingsRow::new(fact.id, t(fact.label))
                .position(GroupPosition::of(i, n))
                .child(value(fact.value.clone())),
        ));
    }
    group = group.child(tracked_box(
        "diag-keyPackages",
        SettingsRow::new("diag-keyPackages", t("diagnostics.keyPackages"))
            .position(GroupPosition::of(n - 1, n))
            .child(value(key_packages_value(state))),
    ));
    let errors = shown_errors(state);
    let routes = shown_routes(state);
    let copy = copy_text(&state.diagnostics);
    let mut section = Section::new(section_header(t("diagnostics.title"), None)).block(group);
    if !routes.is_empty() {
        section = section.block(routes_table(routes));
    }
    section = if errors.is_empty() {
        section.paragraph(
            div()
                // `.d-settings-frame__body > section > p`: 12px `--fg-2`.
                .child(line(
                    t("diagnostics.empty"),
                    text::UI,
                    line_height(text::UI, 1.5),
                    color::FG_2,
                )),
        )
    } else {
        section.block(errors_list(errors))
    };
    section = section.block(tracked_box(
        "diag-copy",
        SettingsRow::new("diag-copy-row", t("diagnostics.copy")).child(tracked_focus(
            "diag-copy-button",
            &copy_focus,
            Button::new("diag-copy-button-b", t("diagnostics.copy"))
                .focus_handle(&copy_focus)
                .on_click(move |_, _, cx| {
                    cx.write_to_clipboard(ClipboardItem::new_string(copy.clone()))
                }),
        )),
    ));
    section.into_div().into_any_element()
}

#[cfg(test)]
mod tests {
    use super::*;
    use dilla_client::state::diagnostics::KeyPackageStatus;

    fn state() -> AppState {
        let mut s = AppState::default();
        s.connection.status = ConnectionStatus::Online;
        s.connection.generation = Some(4);
        s.diagnostics.instance_name = Some("home".into());
        s.diagnostics.devices = Some(2);
        s
    }

    #[test]
    fn the_four_facts_in_the_webs_order() {
        let f = facts(&state());
        let ids: Vec<_> = f.iter().map(|f| f.id).collect();
        assert_eq!(
            ids,
            [
                "diag-connection",
                "diag-generation",
                "diag-instance",
                "diag-devices"
            ]
        );
        let values: Vec<_> = f.iter().map(|f| f.value.as_str()).collect();
        assert_eq!(values, ["online", "4", "home", "2"]);
    }

    #[test]
    fn unknown_facts_fall_back_like_the_web() {
        let f = facts(&AppState::default());
        assert_eq!(f[0].value, "offline");
        assert_eq!(f[1].value, "");
        assert_eq!(f[2].value, "–");
        assert_eq!(f[3].value, "–");
    }

    #[test]
    fn key_packages_show_only_what_is_known() {
        let mut s = AppState::default();
        assert_eq!(key_packages_value(&s), "–");
        s.diagnostics.key_packages = KeyPackageStatus {
            count: Some(12),
            last_attempt_ms: Some(1_794_481_738_000),
            last_error: Some("E_NETWORK".into()),
        };
        let v = key_packages_value(&s);
        assert!(v.starts_with("12 · "), "{v}");
        assert!(v.ends_with(" · E_NETWORK"), "{v}");
        s.diagnostics.key_packages.last_error = None;
        assert_eq!(key_packages_value(&s).matches(" · ").count(), 1);
    }

    #[test]
    fn a_hostile_last_error_shows_as_unknown() {
        let mut s = AppState::default();
        s.diagnostics.key_packages = KeyPackageStatus {
            count: Some(3),
            last_attempt_ms: None,
            last_error: Some("token=abc123 recovery key".into()),
        };
        let v = key_packages_value(&s);
        assert_eq!(v, "3 · E_UNKNOWN");
        assert!(!v.contains("abc123"));
    }

    #[test]
    fn only_the_last_twenty_codes_and_five_routes_are_shown() {
        let mut s = AppState::default();
        s.diagnostics.errors = (0..25)
            .map(|i| ErrorEntry {
                code: format!("E_{i}"),
                at_ms: i,
            })
            .collect();
        s.diagnostics.slow_routes = (0..8)
            .map(|i| RouteTiming {
                pattern: format!("GET /v1/{i}"),
                p50: 1,
                p95: 2,
                count: 3,
            })
            .collect();
        let shown = shown_errors(&s);
        assert_eq!(shown.len(), 20);
        assert_eq!(shown[0].code, "E_5");
        assert_eq!(shown_routes(&s).len(), 5);
    }
}
