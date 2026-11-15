//! Package 11.1: the shared components' screen-reader semantics, in a test window with accessibility switched on (the
//! vendored GPUI's `Window::activate_a11y_for_test`): each component's AccessKit node has the role, name and state a
//! reader announces, dialogs are modal, the announcer reads each announcement once (a new node every time), and the
//! focused control is the tree's focus. The screens are `tests/a11y.rs` (the automation build, per fixture).

use dilla_desktop_ui::IconName;
use dilla_desktop_ui::a11y::{Live, announce};
use dilla_desktop_ui::components::dialog::Dialog;
use dilla_desktop_ui::components::icon_button::IconButton;
use dilla_desktop_ui::components::toggle::Toggle;
use dilla_desktop_ui::components::{Button, ButtonVariant};
use dilla_parity::a11y::{Node, Tree};
use gpui::accesskit::{Action, TreeUpdate};
use gpui::{
    Context, FocusHandle, InteractiveElement, IntoElement, ParentElement, Render, Styled,
    TestAppContext, VisualTestContext, Window, div,
};

/// The tree the window's last frame built, as the lint reads it.
fn tree_of(update: &TreeUpdate, scale: f32) -> Tree {
    let scale = f64::from(scale);
    Tree {
        root: update.tree.as_ref().map(|t| t.root.0),
        focus: update.focus.0,
        nodes: update
            .nodes
            .iter()
            .map(|(id, n)| Node {
                id: id.0,
                role: format!("{:?}", n.role()),
                label: n.label().map(str::to_owned),
                value: n.value().map(str::to_owned),
                description: n.description().map(str::to_owned),
                placeholder: n.placeholder().map(str::to_owned),
                children: n.children().iter().map(|c| c.0).collect(),
                bounds: n.bounds().map(|r| {
                    [
                        r.x0 / scale,
                        r.y0 / scale,
                        (r.x1 - r.x0) / scale,
                        (r.y1 - r.y0) / scale,
                    ]
                }),
                click: n.supports_action(Action::Click),
                focus: n.supports_action(Action::Focus),
                modal: n.is_modal(),
                live: match n.live() {
                    Some(Live::Polite) => Some("polite".to_owned()),
                    Some(Live::Assertive) => Some("assertive".to_owned()),
                    _ => None,
                },
                toggled: n.toggled().map(|t| format!("{t:?}")),
                selected: n.is_selected(),
                expanded: n.is_expanded(),
                disabled: n.is_disabled(),
            })
            .collect(),
    }
}

/// Switches accessibility on, draws a frame and reads its tree.
fn draw(cx: &mut VisualTestContext) -> Tree {
    cx.update(|window, cx| {
        window.activate_a11y_for_test();
        window.refresh();
        window.draw(cx).clear(cx);
    });
    cx.run_until_parked();
    cx.update(|window, cx| {
        window.refresh();
        window.draw(cx).clear(cx);
        let scale = window.scale_factor();
        tree_of(
            &window
                .a11y_tree_update()
                .expect("a frame with accessibility on"),
            scale,
        )
    })
}

struct Host {
    save: FocusHandle,
    dialog: bool,
}

impl Render for Host {
    fn render(&mut self, _window: &mut Window, _cx: &mut Context<Self>) -> impl IntoElement {
        div()
            .id("host")
            .size_full()
            .flex()
            .gap_4()
            .child(
                Button::new("save", "Save")
                    .variant(ButtonVariant::Accent)
                    .focus_handle(&self.save)
                    .on_click(|_, _, _| {}),
            )
            .child(Button::new("off", "Archive").disabled(true))
            .child(
                IconButton::new("mute", IconName::MicOff, "Mute")
                    .pressed(true)
                    .on_click(|_, _, _| {}),
            )
            .child(Toggle::new("sounds", "Join and leave sounds", false).on_change(|_, _, _| {}))
            .child(
                Dialog::new("rename", "Rename channel", "Close")
                    .open(self.dialog)
                    .body(div().child("body")),
            )
    }
}

fn install(cx: &mut TestAppContext) {
    cx.update(dilla_desktop::install);
}

#[gpui::test]
fn components_have_their_roles_names_and_states(cx: &mut TestAppContext) {
    install(cx);
    let (_, cx) = cx.add_window_view(|_, cx| Host {
        save: cx.focus_handle().tab_stop(true),
        dialog: false,
    });
    let tree = draw(cx);
    let save = tree.find("Button", "Save").expect("the Save button");
    assert!(save.click && save.focus, "Save is clickable and focusable");
    let archive = tree.find("Button", "Archive").expect("the disabled button");
    assert!(archive.disabled, "a disabled button says so");
    let mute = tree
        .find("Button", "Mute")
        .expect("the icon button, named by its label");
    assert_eq!(
        mute.toggled.as_deref(),
        Some("True"),
        "a pressed toggle is on"
    );
    let sounds = tree
        .find("Switch", "Join and leave sounds")
        .expect("the toggle is a switch named by its label");
    assert_eq!(sounds.toggled.as_deref(), Some("False"));
    assert!(
        tree.by_role("Dialog").next().is_none(),
        "a closed dialog has no node"
    );
    assert_eq!(tree.lint(), Vec::new(), "the components pass the tree lint");
}

#[gpui::test]
fn an_open_dialog_is_modal_and_named_by_its_title(cx: &mut TestAppContext) {
    install(cx);
    let (_, cx) = cx.add_window_view(|_, cx| Host {
        save: cx.focus_handle().tab_stop(true),
        dialog: true,
    });
    let tree = draw(cx);
    let dialog = tree
        .find("Dialog", "Rename channel")
        .expect("the dialog, named by its title");
    assert!(dialog.modal, "the dialog is modal (aria-modal)");
    let close = tree.find("Button", "Close").expect("its close button");
    assert!(
        tree.inside(close.id, "Dialog", Some("Rename channel")),
        "the close button is inside the dialog"
    );
    assert_eq!(tree.lint(), Vec::new());
}

#[gpui::test]
fn the_focused_control_is_the_trees_focus(cx: &mut TestAppContext) {
    install(cx);
    let (host, cx) = cx.add_window_view(|_, cx| Host {
        save: cx.focus_handle().tab_stop(true),
        dialog: false,
    });
    let save = host.read_with(cx, |h, _| h.save.clone());
    cx.update(|window, cx| save.focus(window, cx));
    let tree = draw(cx);
    let focused = tree.focused().expect("a node holds the focus");
    assert_eq!(
        (focused.role.as_str(), focused.name()),
        ("Button", Some("Save"))
    );
}

struct Announcing;

impl Render for Announcing {
    fn render(&mut self, window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        div()
            .id("root")
            .size_full()
            .child(dilla_desktop_ui::a11y::announcer(window, cx))
    }
}

#[gpui::test]
fn each_announcement_is_a_new_polite_live_node(cx: &mut TestAppContext) {
    install(cx);
    let (_, cx) = cx.add_window_view(|_, _| Announcing);
    let quiet = draw(cx);
    assert!(
        quiet.by_role("Status").next().is_none(),
        "nothing announced yet"
    );
    cx.update(|window, cx| announce("Copied", window, cx));
    let first = draw(cx);
    let a = first
        .find("Status", "Copied")
        .expect("the announcement")
        .clone();
    assert_eq!(a.live.as_deref(), Some("polite"));
    assert_eq!(a.value.as_deref(), Some("Copied"), "macOS reads the value");
    cx.update(|window, cx| announce("Copied", window, cx));
    let second = draw(cx);
    let b = second
        .find("Status", "Copied")
        .expect("the repeated announcement");
    assert_ne!(
        a.id, b.id,
        "a repeat is a new node, so the reader says it again"
    );
}
