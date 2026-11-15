//! `Dialog` (packages/ui/src/Dialog): the prototype's modal. 520px (a confirmation 440px) at 12 % of the window's height, `--surface`
//! in the `--line-strong` edge, 12px corners, the `--shadow-3` over the `--scrim`.
//!
//! **Focus.** Opening records what had focus and moves it into the dialog: to the dialog itself, or to its close button
//! (`InitialFocus::Close`), or to a handle the caller passes (`initial_focus`, the web's `data-autofocus`). Tab and Shift+Tab
//! stay inside: past the last control focus wraps to the first and back. Escape, the close button and a press on the scrim
//! call `on_close` (unless `can_close` is false). When the dialog goes from open to closed (the host renders it with
//! `open(false)` after rendering it open) focus returns to what had it, if focus was inside the dialog or nowhere; a flow
//! that already moved focus elsewhere keeps it there.
//!
//! **Placement.** The dialog is modal (`showModal`, the browser's top layer): it covers the window, wherever its element sits
//! in the tree, and a closed dialog draws nothing. Its top margin is Dialog.css's 12vh, except for a dialog whose element is
//! a child of a settings section ([`Dialog::in_settings_section`]).

use std::rc::Rc;

use gpui::{
    AnyElement, App, ElementId, FocusHandle, Global, InteractiveElement, IntoElement, KeyBinding,
    MouseButton, ParentElement, RenderOnce, SharedString, StatefulInteractiveElement, Styled,
    Window, actions, anchored, deferred, div, point, px,
};

use super::button::{Button, ButtonSize, ButtonVariant};
use crate::a11y::Accessible;
use crate::metrics::shadow_3;
use crate::text::line_height;
use crate::tokens::{color, font, radius, text};

actions!(
    dilla_dialog,
    [TrapNext, TrapPrevious, MenuNext, MenuPrevious]
);

/// The key context of an overlay panel (a dialog, the settings frame), in which Tab and Shift+Tab are [`TrapNext`] and
/// [`TrapPrevious`] instead of the window's own focus keys. GPUI runs a key binding before any key listener, so the trap has to
/// be an action bound in the overlay's context; a binding in a deeper context wins over the window's plain Tab.
pub const OVERLAY_CONTEXT: &str = "Dialog";

/// The key context of an open action menu's popup: Tab and Shift+Tab close it and move on ([`MenuNext`], [`MenuPrevious`]).
pub const MENU_CONTEXT: &str = "ActionMenu";

/// Binds Tab and Shift+Tab inside an overlay and an open menu. The app calls this once, from `keys::bind_all`.
pub fn bind_keys(cx: &mut App) {
    if cx.has_global::<KeysBound>() {
        return;
    }
    cx.set_global(KeysBound);
    cx.bind_keys([
        KeyBinding::new("tab", TrapNext, Some(OVERLAY_CONTEXT)),
        KeyBinding::new("shift-tab", TrapPrevious, Some(OVERLAY_CONTEXT)),
        KeyBinding::new("tab", MenuNext, Some(MENU_CONTEXT)),
        KeyBinding::new("shift-tab", MenuPrevious, Some(MENU_CONTEXT)),
    ]);
}

struct KeysBound;
impl Global for KeysBound {}

type CloseHandler = Rc<dyn Fn(&mut Window, &mut App) + 'static>;

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub enum DialogKind {
    /// A head bar over a hairline, the body, a footer bar.
    #[default]
    Form,
    /// No bars: a lead (a 40px avatar) beside the 15px title and the description, the fields, then the buttons.
    Confirm,
}

impl DialogKind {
    /// `min(520px, 100vw - 24px)`; a confirmation 440px.
    pub fn width(self, viewport_width: f32) -> f32 {
        let wide: f32 = match self {
            Self::Form => 520.0,
            Self::Confirm => 440.0,
        };
        wide.min(viewport_width - 24.0)
    }

    /// The body's content width: the dialog inside its 1px edge and the body's padding (`.d-dialog__body` 16px, a
    /// confirmation's 18px sides).
    pub fn body_width(self, viewport_width: f32) -> f32 {
        let side: f32 = match self {
            Self::Form => 16.0,
            Self::Confirm => 18.0,
        };
        self.width(viewport_width) - 2.0 - 2.0 * side
    }
}

#[derive(Clone, Default)]
pub enum InitialFocus {
    /// The dialog itself.
    #[default]
    Dialog,
    /// The first footer button (Cancel, in a confirmation).
    Close,
    /// A control inside, which the caller owns the handle of.
    Handle(FocusHandle),
}

/// What a dialog remembers between frames.
pub struct DialogMemory {
    panel: FocusHandle,
    close: FocusHandle,
    focus: OpenFocus,
}

/// What an overlay remembers of focus: what had it when the overlay opened, and whether it was open last frame.
#[derive(Default)]
pub struct OpenFocus {
    opener: Option<FocusHandle>,
    was_open: bool,
}

/// Something that holds an [`OpenFocus`].
pub trait HasOpenFocus: 'static {
    fn open_focus(&mut self) -> &mut OpenFocus;
}

impl HasOpenFocus for DialogMemory {
    fn open_focus(&mut self) -> &mut OpenFocus {
        &mut self.focus
    }
}

/// Opening records the opener (unless focus is already inside `panel`) and moves focus to `target` after this frame.
/// Closing hands focus back to the opener, but only when focus was in the overlay or nowhere: a flow that already moved
/// focus elsewhere keeps it there.
pub fn sync_open_focus<T: HasOpenFocus>(
    memory: &gpui::Entity<T>,
    open: bool,
    panel: &FocusHandle,
    target: FocusHandle,
    window: &mut Window,
    cx: &mut App,
) {
    let was_open = memory.update(cx, |m, _| m.open_focus().was_open);
    if open && !was_open {
        let opener = window
            .focused(cx)
            .filter(|h| !panel.contains_focused(window, cx) && h != panel);
        memory.update(cx, |m, _| {
            let f = m.open_focus();
            f.was_open = true;
            f.opener = opener;
        });
        window.defer(cx, move |window, cx| target.focus(window, cx));
    } else if !open && was_open {
        let opener = memory.update(cx, |m, _| {
            let f = m.open_focus();
            f.was_open = false;
            f.opener.take()
        });
        // Decided now, while the last frame still has the overlay in its tree: after the frame the overlay is gone and
        // nothing can be said to be inside it.
        let inside = window
            .focused(cx)
            .is_none_or(|_| panel.contains_focused(window, cx));
        window.defer(cx, move |window, cx| {
            if inside && let Some(opener) = opener {
                opener.focus(window, cx);
            }
        });
    }
}

/// The offset of the panel from the top: 12 % of the window's height.
pub fn top_offset(viewport_height: f32) -> f32 {
    viewport_height * 0.12
}

#[derive(IntoElement)]
pub struct Dialog {
    id: ElementId,
    open: bool,
    title: SharedString,
    kind: DialogKind,
    lead: Option<AnyElement>,
    description: Option<AnyElement>,
    body: Option<AnyElement>,
    footer: Vec<AnyElement>,
    close_label: SharedString,
    show_default_close: bool,
    can_close: bool,
    initial_focus: InitialFocus,
    close_handle: Option<FocusHandle>,
    on_close: Option<CloseHandler>,
    on_backdrop: Option<CloseHandler>,
    in_section: bool,
    /// A wider form dialog's own width (`min(width, 100vw - 24px)`), as a component's CSS widens `.d-dialog` (the
    /// screen-share picker's 680px).
    wide: Option<f32>,
    a11y_description: Option<SharedString>,
}

impl Dialog {
    /// `close_label` names the close button (the web's `dialog.close`); it has no default, so no English lives here.
    pub fn new(
        id: impl Into<ElementId>,
        title: impl Into<SharedString>,
        close_label: impl Into<SharedString>,
    ) -> Self {
        Self {
            id: id.into(),
            open: true,
            title: title.into(),
            kind: DialogKind::Form,
            lead: None,
            description: None,
            body: None,
            footer: Vec::new(),
            close_label: close_label.into(),
            show_default_close: true,
            can_close: true,
            initial_focus: InitialFocus::Dialog,
            close_handle: None,
            on_close: None,
            on_backdrop: None,
            in_section: false,
            wide: None,
            a11y_description: None,
        }
    }

    /// `.d-dialog:has(...) { width: min(<width>px, calc(100vw - 24px)) }`: a form dialog wider than 520px.
    pub fn width(mut self, width: f32) -> Self {
        self.wide = Some(width);
        self
    }

    /// The web renders this dialog's `<dialog>` as a child of a settings section (`.d-settings-frame__body > section`, a
    /// channel's editor, a device's sign-out): the section's rhythm (`> * { margin-block: 0 }`, `> * + * { margin-top:
    /// 14px }`) outranks Dialog.css's `margin: 12vh auto auto`, so the modal sits the section gap from the window's top. Its
    /// `max-height` (`100dvh - 12vh - 20px`) stays Dialog.css's.
    pub fn in_settings_section(mut self) -> Self {
        self.in_section = true;
        self
    }

    pub fn open(mut self, open: bool) -> Self {
        self.open = open;
        self
    }

    pub fn kind(mut self, kind: DialogKind) -> Self {
        self.kind = kind;
        self
    }

    /// Drawn left of the title (a confirmation's avatar).
    pub fn lead(mut self, lead: impl IntoElement) -> Self {
        self.lead = Some(lead.into_any_element());
        self
    }

    /// Drawn under the title, in the head (a confirmation's body).
    pub fn description(mut self, description: impl IntoElement) -> Self {
        self.description = Some(description.into_any_element());
        self
    }

    /// What a screen reader says after the dialog's name (`aria-describedby`): a confirmation's body text.
    pub fn a11y_description(mut self, text: impl Into<SharedString>) -> Self {
        self.a11y_description = Some(text.into());
        self
    }

    pub fn body(mut self, body: impl IntoElement) -> Self {
        self.body = Some(body.into_any_element());
        self
    }

    /// Controls after the close button in the footer (the action).
    pub fn footer(mut self, footer: impl IntoElement) -> Self {
        self.footer.push(footer.into_any_element());
        self
    }

    /// False when the caller draws its own close control.
    pub fn show_default_close(mut self, show: bool) -> Self {
        self.show_default_close = show;
        self
    }

    /// False disables the close button, Escape and the scrim during an indivisible local operation.
    pub fn can_close(mut self, can: bool) -> Self {
        self.can_close = can;
        self
    }

    pub fn initial_focus(mut self, focus: InitialFocus) -> Self {
        self.initial_focus = focus;
        self
    }

    /// The focus handle of the close button, when the caller moves focus to it itself (a confirmation's refusal).
    pub fn close_handle(mut self, handle: &FocusHandle) -> Self {
        self.close_handle = Some(handle.clone());
        self
    }

    pub fn on_close(mut self, handler: impl Fn(&mut Window, &mut App) + 'static) -> Self {
        self.on_close = Some(Rc::new(handler));
        self
    }

    /// A distinct handler for abandoning the dialog by its scrim instead of choosing Cancel.
    pub fn on_backdrop(mut self, handler: impl Fn(&mut Window, &mut App) + 'static) -> Self {
        self.on_backdrop = Some(Rc::new(handler));
        self
    }
}

/// Moves focus one step inside `panel` and keeps it there: past either end it wraps.
pub fn trap_tab(panel: &FocusHandle, forward: bool, window: &mut Window, cx: &mut App) {
    // The panel is itself a tab stop (so Tab from it descends into its first control) but never a place to rest: a step that
    // lands on it, or outside it, goes on to the first control inside.
    if forward {
        window.focus_next(cx);
        if !panel.contains_focused(window, cx) {
            panel.focus(window, cx);
            window.focus_next(cx);
        } else if panel.is_focused(window) {
            window.focus_next(cx);
        }
        return;
    }
    window.focus_prev(cx);
    if panel.contains_focused(window, cx) && !panel.is_focused(window) {
        return;
    }
    // Before the first control: wrap to the last one by walking forward from the panel to its end.
    panel.focus(window, cx);
    let mut last = None;
    for _ in 0..512 {
        window.focus_next(cx);
        match window.focused(cx) {
            Some(h) if panel.contains_focused(window, cx) && !panel.is_focused(window) => {
                last = Some(h);
            }
            _ => break,
        }
    }
    match last {
        Some(h) => h.focus(window, cx),
        None => panel.focus(window, cx),
    }
}

impl RenderOnce for Dialog {
    fn render(self, window: &mut Window, cx: &mut App) -> impl IntoElement {
        let memory =
            window.use_keyed_state((self.id.clone(), "dialog"), cx, |_, cx| DialogMemory {
                panel: cx.focus_handle().tab_stop(true),
                close: cx.focus_handle().tab_stop(true),
                focus: OpenFocus::default(),
            });
        let (panel, close) = memory.read_with(cx, |m, _| (m.panel.clone(), m.close.clone()));
        let close = self.close_handle.clone().unwrap_or(close);
        let target = match &self.initial_focus {
            InitialFocus::Dialog => panel.clone(),
            InitialFocus::Close if self.show_default_close => close.clone(),
            InitialFocus::Close => panel.clone(),
            InitialFocus::Handle(h) => h.clone(),
        };
        sync_open_focus(&memory, self.open, &panel, target, window, cx);
        let root = div().id(self.id.clone());
        if !self.open {
            return root.size_0().into_any_element();
        }

        let viewport = window.viewport_size();
        let can_close = self.can_close;
        let close_with = |handler: Option<CloseHandler>| {
            move |window: &mut Window, cx: &mut App| {
                if can_close && let Some(h) = &handler {
                    h(window, cx);
                }
            }
        };
        let on_close = close_with(self.on_close.clone());
        let on_backdrop = close_with(self.on_backdrop.clone().or_else(|| self.on_close.clone()));
        let confirm = self.kind == DialogKind::Confirm;

        let mut head = div()
            .flex()
            .gap(px(if confirm { 14.0 } else { 10.0 }))
            .items_center();
        head = if confirm {
            head.items_start().px(px(18.0)).pt(px(18.0)).pb(px(4.0))
        } else {
            head.px(px(16.0))
                .py(px(14.0))
                .border_b_1()
                .border_color(color::HAIRLINE)
        };
        if let Some(lead) = self.lead {
            head = head.child(lead);
        }
        let title_size = if confirm { text::CARD } else { text::BASE };
        head = head.child(
            div()
                .flex()
                .flex_col()
                .gap(px(4.0))
                .flex_1()
                .min_w_0()
                .child(
                    // `.d-dialog__title` is an h2 that wraps in the heading column.
                    crate::text::flow_paragraph(
                        self.title.clone(),
                        title_size,
                        line_height(title_size, 1.5),
                        color::FG,
                    )
                    .weight(font::WEIGHT_DISPLAY),
                )
                .children(self.description),
        );

        let mut footer = div().flex().items_center().justify_end().gap(px(8.0));
        footer = if confirm {
            footer.px(px(18.0)).py(px(16.0))
        } else {
            footer
                .px(px(16.0))
                .py(px(12.0))
                .border_t_1()
                .border_color(color::HAIRLINE)
        };
        if self.show_default_close {
            let on_close = on_close.clone();
            footer = footer.child(
                Button::new((self.id.clone(), "close-button"), self.close_label)
                    .variant(ButtonVariant::Ghost)
                    .size(ButtonSize::Dialog)
                    .focus_handle(&close)
                    .disabled(!can_close)
                    .on_click(move |_, window, cx| on_close(window, cx)),
            );
        }
        footer = footer.children(self.footer);

        // `<dialog aria-modal aria-labelledby=title>`; a confirm is an alert dialog (it asks before something happens).
        let panel_el = div().id((self.id.clone(), "panel"));
        let mut panel_el = if confirm {
            panel_el.modal_alert(self.title.clone())
        } else {
            panel_el.modal_dialog(self.title.clone())
        };
        if let Some(text) = self.a11y_description.clone() {
            panel_el = panel_el.aria_description(text);
        }
        let mut panel_el = panel_el
            .track_focus(&panel)
            .key_context(OVERLAY_CONTEXT)
            .flex()
            .flex_col()
            .flex_none()
            .w(px(self.wide.map_or_else(
                || self.kind.width(f32::from(viewport.width)),
                |w| w.min(f32::from(viewport.width) - 24.0),
            )))
            .max_h(viewport.height - px(top_offset(f32::from(viewport.height)) + 20.0))
            .bg(color::SURFACE)
            .border_1()
            .border_color(color::LINE_STRONG)
            .rounded(radius::XXL)
            .shadow(shadow_3())
            // Taller than its max height, the dialog scrolls as one box (the `<dialog>`'s `overflow: auto`): its parts
            // keep their heights.
            .overflow_y_scroll()
            .on_mouse_down(MouseButton::Left, |_, _, cx| cx.stop_propagation())
            .child(div().flex_none().child(head));
        if let Some(body) = self.body {
            let pad = if confirm {
                div().px(px(18.0)).pt(px(12.0))
            } else {
                div().p(px(16.0))
            };
            panel_el = panel_el.child(pad.flex_none().child(body));
        }
        panel_el = panel_el.child(div().flex_none().child(footer));
        let (next_panel, prev_panel) = (panel.clone(), panel.clone());
        let panel_el = panel_el
            .on_action(move |_: &TrapNext, window, cx| trap_tab(&next_panel, true, window, cx))
            .on_action(move |_: &TrapPrevious, window, cx| trap_tab(&prev_panel, false, window, cx))
            .on_key_down(move |event, window, cx| {
                if event.keystroke.key == "escape" {
                    cx.stop_propagation();
                    on_close(window, cx);
                }
            });

        let top = if self.in_section {
            super::settings::frame::SECTION_GAP
        } else {
            top_offset(f32::from(viewport.height))
        };
        // The top layer: the scrim and the panel cover the window, whatever box the dialog's element is in.
        deferred(
            anchored().position(point(px(0.0), px(0.0))).child(
                root.w(viewport.width)
                    .h(viewport.height)
                    .flex()
                    .flex_col()
                    .items_center()
                    .pt(px(top))
                    .bg(color::SCRIM)
                    .occlude()
                    .on_mouse_down(MouseButton::Left, move |_, window, cx| {
                        on_backdrop(window, cx)
                    })
                    .child(panel_el),
            ),
        )
        .with_priority(10)
        .into_any_element()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn widths_and_offset_follow_dialog_css() {
        assert_eq!(DialogKind::Form.width(1280.0), 520.0);
        assert_eq!(DialogKind::Confirm.width(1280.0), 440.0);
        assert_eq!(DialogKind::Form.width(360.0), 336.0);
        assert_eq!(top_offset(800.0), 96.0);
    }
}
