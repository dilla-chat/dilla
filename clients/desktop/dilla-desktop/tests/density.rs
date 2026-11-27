//! Task 19b: the density chosen in Appearance restyles the whole window at once, as the web's `data-density` does
//! (packages/web/src/App.tsx:105; the six variables of tokens.ts:254-258).
//!
//! One window without a client (so the choice is kept in the window, as in a fixture's) holds the shell drawn from a
//! fixture (`messages/mention`) with Settings -> Appearance beside it, plus the components whose step the shell does not
//! draw on this fixture: a medium avatar (`--avatar-size`) and the DM picker's list (`--row-gap` between its rows). A click on Compact
//! must move them all to the compact step, a click on Regular back. Cozy has no setting (the web admits `compact` and
//! `regular`), so it is set the way a fixture sets it.

use std::path::PathBuf;
use std::sync::Arc;

use dilla_desktop::router::UserSection;
use dilla_desktop::screens::shell::{Clock, ShellView};
use dilla_desktop::screens::user_settings::UserSettings;
use dilla_desktop::slots::SlotCx;
use dilla_desktop_ui::components::avatar::Avatar;
use dilla_desktop_ui::components::dms::dm_picker::{DmPickerList, PickerMember};
use dilla_desktop_ui::metrics::Density;
use dilla_desktop_ui::text::line_height;
use dilla_desktop_ui::tokens::text;
use gpui::{
    AppContext, Context, Entity, InteractiveElement, IntoElement, Modifiers, ParentElement, Render,
    Styled, TestAppContext, VisualTestContext, Window, div, px,
};

fn fixture_path(name: &str) -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("../parity/fixtures")
        .join(format!("{name}.json"))
}

/// The shell and Appearance side by side (Appearance never over the shell, whose own overlays would take its clicks), and
/// the two probes over the shell's foot.
struct Host {
    shell: Entity<ShellView>,
    settings: Entity<UserSettings>,
}

impl Render for Host {
    fn render(&mut self, _window: &mut Window, _cx: &mut Context<Self>) -> impl IntoElement {
        let members = ["Ada", "Björn", "Mira"]
            .into_iter()
            .map(|n| PickerMember { name: n.into() })
            .collect();
        div()
            .size_full()
            .relative()
            .flex()
            .child(div().w(px(1280.0)).h_full().child(self.shell.clone()))
            .child(div().w(px(640.0)).h_full().child(self.settings.clone()))
            .child(
                div()
                    .absolute()
                    .left_0()
                    .bottom_0()
                    .flex()
                    .items_start()
                    .child(
                        div()
                            .flex_none()
                            .debug_selector(|| "probe-avatar".into())
                            .child(Avatar::new("probe-avatar-md", "Ada")),
                    )
                    .child(
                        div()
                            .w(px(300.0))
                            .debug_selector(|| "probe-picker".into())
                            .child(DmPickerList::new(members, "message", |_, _, _| {})),
                    ),
            )
    }
}

fn open(cx: &mut TestAppContext) -> &mut VisualTestContext {
    cx.update(dilla_desktop::install);
    let fixture = dilla_desktop::fixture::Fixture::load(&fixture_path("messages/mention"), None)
        .expect("the fixture loads");
    let dilla_desktop::fixture::FixtureScreen::Shell { channel } = fixture.screen else {
        panic!("messages/mention opens a channel");
    };
    let state = Arc::new(fixture.state);
    let (_, vcx) = cx.add_window_view(|window, cx| {
        let clock = Clock::Fixed {
            now: fixture.now,
            utc: true,
        };
        let shell =
            cx.new(|cx| ShellView::new(state.clone(), None, Some(channel), clock, window, cx));
        let settings = cx.new(|cx| {
            let mut v = UserSettings::new(window, cx);
            v.show(UserSection::Appearance, SlotCx::default(), cx);
            v
        });
        Host { shell, settings }
    });
    vcx.simulate_resize(gpui::size(px(1920.0), px(800.0)));
    vcx.run_until_parked();
    vcx
}

fn click(cx: &mut VisualTestContext, selector: &'static str) {
    let at = cx
        .debug_bounds(selector)
        .unwrap_or_else(|| panic!("no {selector}"))
        .center();
    cx.simulate_click(at, Modifiers::default());
    cx.run_until_parked();
}

/// What the probes measure: a message row's height (its head line takes `--line-height`), the medium avatar's side and
/// the DM picker list's height (three rows, `--row-gap` apart).
#[derive(Debug, Clone, Copy)]
struct Measure {
    row: f32,
    avatar: f32,
    picker: f32,
}

fn measure(cx: &mut VisualTestContext) -> Measure {
    cx.run_until_parked();
    // Paint once more so the probes record this frame.
    cx.update(|window, _| window.refresh());
    cx.run_until_parked();
    let window = cx.update(|window, _| window.window_handle().window_id());
    let row = dilla_desktop::automation::probed(window, "message-row-1")
        .expect("the second message's row");
    let avatar = cx.debug_bounds("probe-avatar").expect("the avatar");
    let picker = cx.debug_bounds("probe-picker").expect("the picker list");
    Measure {
        row: f32::from(row.size.height),
        avatar: f32::from(avatar.size.width),
        picker: f32::from(picker.size.height),
    }
}

/// The head line's share of a row at `d` (MessageRow.css: the author at `--text-sm` in the root's line height).
fn head(d: Density) -> f32 {
    line_height(text::SM, d.line_height())
}

#[gpui::test]
fn the_chosen_density_restyles_the_window_at_once(cx: &mut TestAppContext) {
    let cx = open(cx);
    let regular = measure(cx);
    assert_eq!(regular.avatar, 32.0, "regular --avatar-size");

    click(cx, "segmented-appearance-density-compact");
    cx.update(|window, cx| assert_eq!(Density::of(window, cx), Density::Compact));
    let compact = measure(cx);
    assert_eq!(compact.avatar, 28.0, "compact --avatar-size");
    // The row's box sits on whole pixels: its height moves by the head line's change, within that rounding.
    let moved = regular.row - compact.row;
    let want = head(Density::Regular) - head(Density::Compact);
    assert!(
        (moved - want).abs() <= 1.0,
        "the row is {moved}px shorter, the head line {want}px"
    );
    // Three rows: two `--row-gap`s, 2px each at regular, none at compact.
    assert_eq!(regular.picker - compact.picker, 4.0, "--row-gap");

    click(cx, "segmented-appearance-density-regular");
    let back = measure(cx);
    assert_eq!(
        (back.row, back.avatar, back.picker),
        (regular.row, regular.avatar, regular.picker),
        "Regular puts every step back"
    );

    // Cozy: no setting chooses it; a fixture's `"density": "cozy"` sets it on the window the same way.
    cx.update(|window, cx| dilla_desktop::density::set(window, Density::Cozy, cx));
    let cozy = measure(cx);
    assert_eq!(cozy.avatar, 36.0, "cozy --avatar-size");
    assert!(cozy.row > regular.row, "the cozy head line is taller");
    assert_eq!(cozy.picker - regular.picker, 4.0, "two 4px --row-gaps");
}
