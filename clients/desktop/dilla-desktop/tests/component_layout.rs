//! T6c lane B (R6): the shared settings components pinned to where Chromium paints the same CSS. Each case lays the
//! component out in a test window (scale 2, as the parity references) and compares its painted boxes with the numbers
//! Chromium gives `packages/ui`'s CSS for the same content (getBoundingClientRect, then the whole CSS pixels Chromium
//! paints a box on). The component screen fixtures need a web story path the parity shooter does not have yet; these
//! keep the fixes from drifting until then.

use std::cell::RefCell;
use std::rc::Rc;

use dilla_desktop_ui::components::segmented::Segmented;
use dilla_desktop_ui::components::settings::row::{GroupPosition, SettingsRow};
use dilla_desktop_ui::components::toggle::{Fit, Toggle};
use gpui::{
    Bounds, Context, IntoElement, ParentElement, Pixels, Render, Styled, TestAppContext, Window,
    canvas, div, px,
};

type Seen = Rc<RefCell<Vec<(&'static str, Bounds<Pixels>)>>>;

/// A zero-cost probe filling its parent: records the parent's painted bounds under `name`.
fn probe(name: &'static str, seen: &Seen) -> impl IntoElement {
    let seen = seen.clone();
    canvas(
        move |bounds, _, _| seen.borrow_mut().push((name, bounds)),
        |_, _, _, _| {},
    )
    .absolute()
    .top_0()
    .left_0()
    .size_full()
}

fn boxed(name: &'static str, seen: &Seen, child: impl IntoElement) -> impl IntoElement {
    div().relative().child(child).child(probe(name, seen))
}

struct Rows {
    seen: Seen,
}

impl Render for Rows {
    fn render(&mut self, _window: &mut Window, _cx: &mut Context<Self>) -> impl IntoElement {
        let seen = &self.seen;
        // The Overview's group: a two-line hint, then two one-line hints with a 32px control.
        div().w(px(720.0)).flex().flex_col().children([
            boxed(
                "row1",
                seen,
                SettingsRow::new("r1", "Server name")
                    .hint("Up to 255 characters (fewer for some letters). Everyone in the server, and the host, can read it.")
                    .position(GroupPosition::First)
                    .child(div().w(px(216.0)).h(px(36.0))),
            )
            .into_any_element(),
            boxed(
                "row2",
                seen,
                SettingsRow::new("r2", "Inactive timeout")
                    .hint("Idle computers in voice move to AFK after")
                    .position(GroupPosition::Middle)
                    .child(boxed(
                        "control2",
                        seen,
                        Segmented::new(
                            "seg",
                            &[("60", "1m"), ("300", "5m"), ("900", "15m"), ("1800", "30m"), ("3600", "1h")],
                            "300",
                        ),
                    )),
            )
            .into_any_element(),
            boxed(
                "row3",
                seen,
                SettingsRow::new("r3", "Leave server")
                    .hint("You can rejoin with a new invite")
                    .position(GroupPosition::Last)
                    .child(boxed("switch3", seen, Toggle::new("t", "x", true).fit(Fit::Row))),
            )
            .into_any_element(),
        ])
    }
}

fn last(seen: &Seen, name: &str) -> Bounds<Pixels> {
    seen.borrow()
        .iter()
        .rev()
        .find(|(n, _)| *n == name)
        .map(|(_, b)| *b)
        .unwrap_or_else(|| panic!("{name} was not painted"))
}

#[gpui::test]
fn settings_rows_paint_where_chromium_paints_them(cx: &mut TestAppContext) {
    cx.update(dilla_desktop::install);
    let seen: Seen = Rc::default();
    let (_view, cx) = cx.add_window_view(|_, _| Rows { seen: seen.clone() });
    cx.run_until_parked();
    let (row1, row2, row3) = (
        last(&seen, "row1"),
        last(&seen, "row2"),
        last(&seen, "row3"),
    );
    // Chromium: the rows at 0, 69.28 and 125.28 (69.28, 56 and 57 tall), painted from 0, 69 and 125.
    assert_eq!(f32::from(row1.top()), 0.0);
    assert_eq!(
        f32::from(row2.top()),
        69.0,
        "a two-line hint's row is 69 painted pixels"
    );
    assert_eq!(f32::from(row3.top()), 125.0);
    assert_eq!(f32::from(row3.bottom()), 182.0);
    // The 32px segmented group, centred in the 35px line: laid out at 81.78, painted from 82.
    let control = last(&seen, "control2");
    assert_eq!(
        f32::from(control.top()),
        82.0,
        "the control paints on the web's pixel"
    );
    // Its mono labels: 188.84px of options and frame, painted 189 wide.
    assert_eq!(f32::from(control.size.width), 189.0);
    // A row's switch: the row lays out the 36 x 20 track (the 44 x 24 target reaches into the padding), its right edge
    // on the row's padding edge (720 - 1 - 16).
    let switch = last(&seen, "switch3");
    assert_eq!(f32::from(switch.right()), 720.0 - 1.0 - 16.0);
    assert_eq!(
        (f32::from(switch.size.width), f32::from(switch.size.height)),
        (36.0, 20.0)
    );
}
