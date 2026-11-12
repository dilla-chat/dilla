//! `ConfirmDialog` (packages/ui/src/ConfirmDialog): a confirmation over [`Dialog`]. Focus goes to the first field the caller
//! passes (`field_focus`), otherwise to Cancel, never to the destructive button. The confirm button is disabled while
//! `can_confirm` is false or the action is busy. A refusal is shown under the fields; when it arrives with the end of a busy
//! state, focus moves to Cancel, next to it.

use std::rc::Rc;

use gpui::{
    AnyElement, App, ElementId, FocusHandle, IntoElement, ParentElement, RenderOnce, SharedString,
    Styled, Window, div, px,
};

use super::avatar::{Avatar, AvatarSize};
use super::button::{Button, ButtonSize, ButtonVariant};
use super::dialog::{Dialog, DialogKind, InitialFocus};
use crate::icon::Icon;
use crate::icon_names::IconName;
use crate::text::{line, line_height, paragraph};
use crate::tokens::{color, text};

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub enum Tone {
    #[default]
    Danger,
    Accent,
}

/// What the confirm button says and whether it can be pressed.
pub fn confirm_state(busy: bool, can_confirm: bool) -> (bool, bool) {
    // (shows the busy label, is disabled)
    (busy, !can_confirm || busy)
}

/// Whether a refusal just arrived: the action was busy last frame, is not now, and there is an error to show.
pub fn refusal_arrived(was_busy: bool, busy: bool, error: bool) -> bool {
    was_busy && !busy && error
}

type Handler = Rc<dyn Fn(&mut Window, &mut App) + 'static>;

struct Memory {
    cancel: FocusHandle,
    was_busy: bool,
}

#[derive(IntoElement)]
pub struct ConfirmDialog {
    id: ElementId,
    open: bool,
    title: SharedString,
    body: SharedString,
    confirm_label: SharedString,
    busy_label: SharedString,
    cancel_label: SharedString,
    tone: Tone,
    busy: bool,
    error: Option<SharedString>,
    can_confirm: bool,
    can_close: bool,
    person: Option<SharedString>,
    fields: Option<AnyElement>,
    field_focus: Option<FocusHandle>,
    on_confirm: Option<Handler>,
    on_close: Option<Handler>,
}

impl ConfirmDialog {
    pub fn new(
        id: impl Into<ElementId>,
        title: impl Into<SharedString>,
        body: impl Into<SharedString>,
        confirm_label: impl Into<SharedString>,
        busy_label: impl Into<SharedString>,
        cancel_label: impl Into<SharedString>,
    ) -> Self {
        Self {
            id: id.into(),
            open: true,
            title: title.into(),
            body: body.into(),
            confirm_label: confirm_label.into(),
            busy_label: busy_label.into(),
            cancel_label: cancel_label.into(),
            tone: Tone::Danger,
            busy: false,
            error: None,
            can_confirm: true,
            can_close: true,
            person: None,
            fields: None,
            field_focus: None,
            on_confirm: None,
            on_close: None,
        }
    }

    pub fn open(mut self, open: bool) -> Self {
        self.open = open;
        self
    }

    pub fn tone(mut self, tone: Tone) -> Self {
        self.tone = tone;
        self
    }

    /// True while the confirmed action runs.
    pub fn busy(mut self, busy: bool) -> Self {
        self.busy = busy;
        self
    }

    /// The refusal to show. Set it in the same update that clears `busy`: that is what moves focus to Cancel.
    pub fn error(mut self, error: Option<SharedString>) -> Self {
        self.error = error;
        self
    }

    /// False keeps the confirm button disabled (a typed name that does not match yet).
    pub fn can_confirm(mut self, can: bool) -> Self {
        self.can_confirm = can;
        self
    }

    /// False prevents cancelling while an indivisible local save is pending.
    pub fn can_close(mut self, can: bool) -> Self {
        self.can_close = can;
        self
    }

    /// The person the action is about: their 40px avatar beside the title.
    pub fn person(mut self, name: impl Into<SharedString>) -> Self {
        self.person = Some(name.into());
        self
    }

    /// Extra fields under the body (a reason, the typed name), and the first one's focus handle.
    pub fn fields(mut self, fields: impl IntoElement, first: Option<&FocusHandle>) -> Self {
        self.fields = Some(fields.into_any_element());
        self.field_focus = first.cloned();
        self
    }

    pub fn on_confirm(mut self, handler: impl Fn(&mut Window, &mut App) + 'static) -> Self {
        self.on_confirm = Some(Rc::new(handler));
        self
    }

    pub fn on_close(mut self, handler: impl Fn(&mut Window, &mut App) + 'static) -> Self {
        self.on_close = Some(Rc::new(handler));
        self
    }
}

impl RenderOnce for ConfirmDialog {
    fn render(self, window: &mut Window, cx: &mut App) -> impl IntoElement {
        let memory = window.use_keyed_state((self.id.clone(), "confirm"), cx, |_, cx| Memory {
            cancel: cx.focus_handle().tab_stop(true),
            was_busy: false,
        });
        let cancel = memory.read(cx).cancel.clone();
        let was_busy = memory.read(cx).was_busy;
        memory.update(cx, |m, _| m.was_busy = self.busy);
        if self.open && refusal_arrived(was_busy, self.busy, self.error.is_some()) {
            let target = cancel.clone();
            window.defer(cx, move |window, cx| target.focus(window, cx));
        }

        let (shows_busy, disabled) = confirm_state(self.busy, self.can_confirm);
        let label = if shows_busy {
            self.busy_label
        } else {
            self.confirm_label
        };
        let variant = match self.tone {
            Tone::Danger => ButtonVariant::Danger,
            Tone::Accent => ButtonVariant::Accent,
        };
        let mut confirm = Button::new((self.id.clone(), "confirm"), label)
            .variant(variant)
            .size(ButtonSize::Dialog)
            .disabled(disabled);
        if let Some(handler) = self.on_confirm {
            confirm = confirm.on_click(move |_, window, cx| handler(window, cx));
        }

        // The body is the head's 12px --fg-2 line; its measure is the panel less the 18px sides and the avatar.
        let measure = 440.0 - 36.0 - if self.person.is_some() { 54.0 } else { 0.0 };
        let description = div().flex().flex_col().children(paragraph(
            &self.body,
            text::UI,
            line_height(text::UI, 1.5),
            color::FG_2,
            measure,
        ));

        let mut dialog = Dialog::new(self.id.clone(), self.title)
            .open(self.open)
            .kind(DialogKind::Confirm)
            .close_label(self.cancel_label)
            .close_handle(&cancel)
            .can_close(self.can_close)
            .description(description)
            .footer(confirm)
            .initial_focus(match &self.field_focus {
                Some(h) => InitialFocus::Handle(h.clone()),
                None => InitialFocus::Close,
            });
        if let Some(handler) = self.on_close {
            dialog = dialog.on_close(move |window, cx| handler(window, cx));
        }
        if let Some(name) = self.person {
            dialog =
                dialog.lead(div().flex_none().child(
                    Avatar::new((self.id.clone(), "person"), name).size(AvatarSize::Confirm),
                ));
        }
        if self.fields.is_some() || self.error.is_some() {
            let mut body = div().flex().flex_col().gap(px(14.0));
            if let Some(fields) = self.fields {
                body = body.child(fields);
            }
            if let Some(error) = self.error {
                body = body.child(
                    div()
                        .flex()
                        .items_center()
                        .gap(px(4.0))
                        .child(Icon::new(IconName::X).size(px(12.0)).color(color::DANGER))
                        .child(line(
                            error,
                            text::BTN,
                            line_height(text::BTN, 1.5),
                            color::DANGER,
                        )),
                );
            }
            dialog = dialog.body(body);
        }
        dialog
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_confirm_button_is_off_while_busy_or_not_confirmable() {
        assert_eq!(confirm_state(false, true), (false, false));
        assert_eq!(confirm_state(true, true), (true, true));
        assert_eq!(confirm_state(false, false), (false, true));
    }

    #[test]
    fn a_refusal_moves_focus_only_at_the_end_of_a_busy_state() {
        assert!(refusal_arrived(true, false, true));
        assert!(!refusal_arrived(true, false, false));
        assert!(!refusal_arrived(false, false, true));
        assert!(!refusal_arrived(true, true, true));
    }
}
