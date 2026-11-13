//! The six-digit code field behind [`CodeBoxes`]: it owns the digits (in a zeroizing buffer), the caret and the focus,
//! and reads the keys itself. A digit lands at the caret and the caret moves on; Backspace deletes the digit before the
//! caret and moves back; ←/→ move; a paste of a whole code (`123 456`, `123456\n`) fills all six; Enter submits. When the
//! sixth digit lands the field waits the prototype's 350 ms and, if the code is still the same, says it is complete
//! (CodeBoxes `onComplete`), so the step goes on with no press.

use gpui::{
    App, ClickEvent, Context, EventEmitter, FocusHandle, Focusable, InteractiveElement,
    IntoElement, KeyDownEvent, ParentElement, Render, StatefulInteractiveElement, Styled, Window,
    div,
};
use zeroize::Zeroizing;

use dilla_desktop_ui::components::factor::{CODE_LENGTH, COMPLETE_DELAY, CodeBoxes, code_digits};

/// What the field tells its host.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum CodeEvent {
    /// The digits changed (the host clears its error).
    Changed,
    /// Six digits rested 350 ms.
    Complete,
    /// Enter.
    Submit,
}

pub struct CodeField {
    id: &'static str,
    value: Zeroizing<String>,
    caret: usize,
    invalid: bool,
    focus: FocusHandle,
    /// Counts edits, so a completion timer of an older value says nothing.
    edits: u64,
}

impl EventEmitter<CodeEvent> for CodeField {}

impl Focusable for CodeField {
    fn focus_handle(&self, _cx: &App) -> FocusHandle {
        self.focus.clone()
    }
}

impl CodeField {
    pub fn new(id: &'static str, cx: &mut Context<Self>) -> Self {
        Self {
            id,
            value: Zeroizing::new(String::new()),
            caret: 0,
            invalid: false,
            focus: cx.focus_handle(),
            edits: 0,
        }
    }

    /// The digits typed so far.
    pub fn value(&self) -> &str {
        &self.value
    }

    pub fn is_complete(&self) -> bool {
        self.value.len() == CODE_LENGTH
    }

    pub fn set_invalid(&mut self, invalid: bool, cx: &mut Context<Self>) {
        self.invalid = invalid;
        cx.notify();
    }

    /// Empties the field (a refused code, a step left).
    pub fn clear(&mut self, cx: &mut Context<Self>) {
        self.set(String::new(), 0, cx);
    }

    pub fn focus(&self, window: &mut Window, cx: &mut App) {
        self.focus.focus(window, cx);
    }

    fn set(&mut self, next: String, caret: usize, cx: &mut Context<Self>) {
        let next = Zeroizing::new(next);
        if *next == *self.value {
            self.caret = caret.min(next.len());
            cx.notify();
            return;
        }
        self.value = next;
        self.caret = caret.min(self.value.len());
        self.edits += 1;
        cx.emit(CodeEvent::Changed);
        if self.is_complete() {
            let edit = self.edits;
            cx.spawn(async move |this, cx| {
                cx.background_executor().timer(COMPLETE_DELAY).await;
                let _ = this.update(cx, |this, cx| {
                    if this.edits == edit && this.is_complete() {
                        cx.emit(CodeEvent::Complete);
                    }
                });
            })
            .detach();
        }
        cx.notify();
    }

    /// Inserts `typed` at the caret: a whole code replaces the field, a part is inserted (CodeBoxes `onPaste`).
    pub fn insert(&mut self, typed: &str, cx: &mut Context<Self>) {
        let digits = code_digits(typed);
        if digits.is_empty() {
            return;
        }
        if digits.len() == CODE_LENGTH {
            self.set(digits, CODE_LENGTH, cx);
            return;
        }
        let at = self.caret.min(self.value.len());
        let mut next = String::with_capacity(CODE_LENGTH);
        next.push_str(&self.value[..at]);
        next.push_str(&digits);
        next.push_str(&self.value[at..]);
        let next = code_digits(&next);
        let caret = (at + digits.len()).min(next.len());
        self.set(next, caret, cx);
    }

    fn backspace(&mut self, cx: &mut Context<Self>) {
        let at = self.caret.min(self.value.len());
        if at == 0 {
            return;
        }
        let mut next = String::with_capacity(CODE_LENGTH);
        next.push_str(&self.value[..at - 1]);
        next.push_str(&self.value[at..]);
        self.set(next, at - 1, cx);
    }

    fn key(&mut self, e: &KeyDownEvent, window: &mut Window, cx: &mut Context<Self>) {
        let k = &e.keystroke;
        let paste = (k.modifiers.platform || k.modifiers.control) && k.key == "v";
        if paste {
            if let Some(text) = cx.read_from_clipboard().and_then(|c| c.text()) {
                let text = Zeroizing::new(text);
                self.insert(&text, cx);
            }
            cx.stop_propagation();
            return;
        }
        if k.modifiers.platform || k.modifiers.control || k.modifiers.alt {
            return;
        }
        match k.key.as_str() {
            "backspace" => self.backspace(cx),
            "left" => {
                self.caret = self.caret.saturating_sub(1);
                cx.notify();
            }
            "right" => {
                self.caret = (self.caret + 1).min(self.value.len());
                cx.notify();
            }
            "enter" => cx.emit(CodeEvent::Submit),
            _ => match k.key_char.as_deref() {
                Some(c) if c.chars().all(|c| c.is_ascii_digit()) && !c.is_empty() => {
                    self.insert(c, cx);
                }
                _ => return,
            },
        }
        let _ = window;
        cx.stop_propagation();
    }
}

impl Render for CodeField {
    fn render(&mut self, window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let focused = self.focus.is_focused(window);
        div()
            .id(self.id)
            .track_focus(&self.focus)
            .w_full()
            .cursor_text()
            .on_key_down(cx.listener(Self::key))
            .on_click(cx.listener(|this, _: &ClickEvent, window, cx| {
                this.focus.focus(window, cx);
                cx.notify();
            }))
            .child(
                CodeBoxes::new(
                    gpui::ElementId::Name(format!("{}-boxes", self.id).into()),
                    &self.value,
                )
                .caret(self.caret)
                .focused(focused)
                .invalid(self.invalid),
            )
    }
}
