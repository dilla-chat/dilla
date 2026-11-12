//! Proof: a multi-line message composer on gpui-component's `Textarea`.
//!
//! Behaviour mirrors the web composer: Enter sends, Shift+Enter inserts a
//! newline, and typing `@` at the start of a word opens the mention popup.

use gpui::{
    AppContext as _, Context, Entity, EntityInputHandler, EventEmitter, InteractiveElement as _,
    IntoElement, ParentElement, Render, SharedString, Styled, Subscription, Window, div,
};
use gpui_component::input::{Enter, InputEvent, Textarea, TextareaState};

/// An open `@mention` query: byte offset of the `@` and the text typed after it.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Mention {
    pub at: usize,
    pub query: String,
}

/// The mention being typed at `cursor`, if any: an `@` at the start of the
/// text or after whitespace, followed only by non-whitespace up to the cursor.
pub fn mention_at(text: &str, cursor: usize) -> Option<Mention> {
    let before = text.get(..cursor)?;
    let word_start = before
        .char_indices()
        .rev()
        .find(|(_, c)| c.is_whitespace())
        .map_or(0, |(i, c)| i + c.len_utf8());
    let query = before[word_start..].strip_prefix('@')?;
    Some(Mention {
        at: word_start,
        query: query.to_owned(),
    })
}

/// Emitted when the user sends the draft.
pub struct Sent(pub String);

pub struct Composer {
    pub input: Entity<TextareaState>,
    pub mention: Option<Mention>,
    pub sent: Vec<String>,
    _subs: Vec<Subscription>,
}

impl EventEmitter<Sent> for Composer {}

impl Composer {
    pub fn new(window: &mut Window, cx: &mut Context<Self>) -> Self {
        let input = cx.new(|cx| TextareaState::new(window, cx).submit_on_enter(true));
        let sub = cx.subscribe_in(&input, window, Self::on_input_event);
        Self {
            input,
            mention: None,
            sent: Vec::new(),
            _subs: vec![sub],
        }
    }

    fn on_input_event(
        &mut self,
        input: &Entity<TextareaState>,
        event: &InputEvent,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        match event {
            InputEvent::Change => self.refresh_mention(cx),
            InputEvent::PressEnter { shift: false, .. } => {
                // Enter while an IME composition is open confirms the
                // composition; it never sends (web: `event.isComposing`).
                let composing = input
                    .update(cx, |s, cx| s.marked_text_range(window, cx))
                    .is_some();
                let text = input.read(cx).value().to_string();
                if composing || text.trim().is_empty() {
                    return;
                }
                input.update(cx, |s, cx| s.set_value("", window, cx));
                self.mention = None;
                self.sent.push(text.clone());
                cx.emit(Sent(text));
                cx.notify();
            }
            _ => {}
        }
    }

    fn refresh_mention(&mut self, cx: &mut Context<Self>) {
        let (text, cursor) = {
            let s = self.input.read(cx);
            (s.value(), s.cursor())
        };
        self.mention = mention_at(&text, cursor);
        cx.notify();
    }

    pub fn value(&self, cx: &gpui::App) -> SharedString {
        self.input.read(cx).value()
    }
}

impl Render for Composer {
    fn render(&mut self, _: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        div()
            // The textarea propagates a plain Enter so its owner can send;
            // swallowing it here stops GPUI from then also typing the key's
            // "\n" into the draft (the send reads the text after the event).
            .on_action(cx.listener(|_, _: &Enter, _, _| {}))
            .flex()
            .flex_col()
            .child(Textarea::new(&self.input))
            .children(
                self.mention
                    .as_ref()
                    .map(|m| div().child(format!("@{}", m.query))),
            )
    }
}
