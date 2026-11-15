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

use crate::strings::t;
use dilla_desktop_ui::a11y::{Accessible, Role};
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

    /// The boxes are drawn as refused (red).
    pub fn is_invalid(&self) -> bool {
        self.invalid
    }

    pub fn set_invalid(&mut self, invalid: bool, cx: &mut Context<Self>) {
        self.invalid = invalid;
        cx.notify();
    }

    /// Empties the field (a refused code, a step left). The host's reset, not the person's edit: it says no
    /// [`CodeEvent::Changed`], which the hosts read as "the person typed" and answer by dropping their error, so a
    /// refusal set together with this reset stays on show. A completion timer of the old value says nothing. The boxes
    /// lose their refused red with their digits; a host that wants them red (a refused code) sets it after the clear.
    pub fn clear(&mut self, cx: &mut Context<Self>) {
        if !self.value.is_empty() {
            self.edits += 1;
        }
        self.value = Zeroizing::new(String::new());
        self.caret = 0;
        self.invalid = false;
        cx.notify();
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
        // A text field named "Code from the app"; the digits typed are not put in the tree (a one-time code).
        div()
            .id(self.id)
            .a11y(Role::TextInput, t("factor.totp.code"))
            .aria_description(t("factor.totp.codeHint"))
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

#[cfg(test)]
mod tests {
    use super::*;
    use gpui::{AppContext as _, TestAppContext};

    /// CodeBoxes.test.tsx and the keyboard rule: digits land at the caret and move it on, a partial paste is inserted, a
    /// whole code (grouped or newline-ended) replaces the field, Backspace deletes before the caret and moves back.
    #[gpui::test]
    fn digits_paste_and_backspace(cx: &mut TestAppContext) {
        let field = cx.new(|cx| CodeField::new("code-test", cx));
        field.update(cx, |f, cx| {
            f.insert("1", cx);
            f.insert("2", cx);
            assert_eq!((f.value(), f.caret), ("12", 2));
            f.backspace(cx);
            assert_eq!((f.value(), f.caret), ("1", 1));
            f.insert("23", cx);
            assert_eq!((f.value(), f.caret), ("123", 3));
            f.caret = 1;
            f.insert("9", cx);
            assert_eq!((f.value(), f.caret), ("1923", 2));
            f.insert("654 321\n", cx);
            assert_eq!((f.value(), f.caret), ("654321", 6));
            assert!(f.is_complete());
            f.insert("7", cx);
            assert_eq!(f.value(), "654321", "a seventh digit is dropped");
            f.clear(cx);
            assert_eq!((f.value(), f.caret), ("", 0));
            f.insert("a b", cx);
            assert_eq!(f.value(), "");
        });
    }

    /// A refused code empties the field (the host's `clear`), and that reset is not the person's change: no `Changed`,
    /// so the host's refusal ("That code did not work") stays until the person types again. A timer of the cleared
    /// value says nothing either.
    #[gpui::test]
    fn clearing_is_not_a_change_of_the_persons(cx: &mut TestAppContext) {
        let field = cx.new(|cx| CodeField::new("code-test", cx));
        let seen = std::rc::Rc::new(std::cell::RefCell::new(Vec::new()));
        let log = seen.clone();
        cx.update(|cx| {
            cx.subscribe(&field, move |_, e: &CodeEvent, _| {
                log.borrow_mut().push(e.clone())
            })
            .detach();
        });
        field.update(cx, |f, cx| f.insert("123456", cx));
        cx.run_until_parked();
        seen.borrow_mut().clear();
        field.update(cx, |f, cx| f.clear(cx));
        cx.executor().advance_clock(COMPLETE_DELAY);
        cx.run_until_parked();
        assert_eq!(*seen.borrow(), Vec::<CodeEvent>::new());
        field.read_with(cx, |f, _| assert_eq!((f.value(), f.caret), ("", 0)));
        // The person's next digit is a change again.
        field.update(cx, |f, cx| f.insert("4", cx));
        cx.run_until_parked();
        assert_eq!(*seen.borrow(), vec![CodeEvent::Changed]);
    }

    /// The reset is a fresh field: a refused field's red goes with its digits (a host that wants it red again, a
    /// refused code, sets it after the clear).
    #[gpui::test]
    fn clearing_drops_the_refused_red(cx: &mut TestAppContext) {
        let field = cx.new(|cx| CodeField::new("code-test", cx));
        field.update(cx, |f, cx| {
            f.insert("123", cx);
            f.set_invalid(true, cx);
            f.clear(cx);
            assert!(!f.is_invalid());
        });
    }

    /// The sixth digit says the code is complete once it rests (the prototype's 350 ms), and only for that value.
    #[gpui::test]
    fn the_sixth_digit_completes_after_the_rest(cx: &mut TestAppContext) {
        let field = cx.new(|cx| CodeField::new("code-test", cx));
        let seen = std::rc::Rc::new(std::cell::RefCell::new(Vec::new()));
        let log = seen.clone();
        cx.update(|cx| {
            cx.subscribe(&field, move |_, e: &CodeEvent, _| {
                log.borrow_mut().push(e.clone())
            })
            .detach();
        });
        field.update(cx, |f, cx| f.insert("123456", cx));
        cx.run_until_parked();
        assert!(!seen.borrow().contains(&CodeEvent::Complete));
        cx.executor().advance_clock(COMPLETE_DELAY);
        cx.run_until_parked();
        assert!(seen.borrow().contains(&CodeEvent::Complete));
        // A change inside the rest cancels the completion of the older value.
        seen.borrow_mut().clear();
        field.update(cx, |f, cx| {
            f.clear(cx);
            f.insert("123456", cx);
            f.backspace(cx);
        });
        cx.executor().advance_clock(COMPLETE_DELAY);
        cx.run_until_parked();
        assert!(!seen.borrow().contains(&CodeEvent::Complete));
    }
}
