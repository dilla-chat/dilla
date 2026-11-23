//! `PollDialog` (packages/ui PollDialog): the `/poll create` form in the shared [`Dialog`]: the Bot's tile and a close
//! button in the head, the question with its `n / 300` counter, two to ten answers, the duration segments, the "several
//! answers" switch, and Cancel / Post poll. The inputs are the screen's (it owns their state); this draws around them.

use std::rc::Rc;

use gpui::{
    AnyElement, App, ElementId, InteractiveElement, IntoElement, ParentElement, RenderOnce,
    SharedString, StatefulInteractiveElement, Styled, Window, div, px,
};

use crate::components::dialog::{Dialog, InitialFocus};
use crate::icon::Icon;
use crate::icon_names::IconName;
use crate::text::{line, root_line_height};
use crate::tokens::{color, font, radius, text};

/// `.d-poll-dialog__close` (PollDialog.css:43) at the head's top right: the lead is the tile's box, so the button is placed
/// from it: the form dialog's 520px panel, its head's 16px side padding (and 14px top) and the button's 28px at 14px/16px
/// (the 26px tile is centred in the 28px heading, 1px down, so the button's top is -1).
const PANEL_W: f32 = 520.0;
const HEAD_PAD: f32 = 16.0;
const CLOSE_RIGHT: f32 = 16.0;
const CLOSE_SIZE: f32 = 28.0;

/// The durations, in the form's order.
pub const DURATIONS: [&str; 6] = ["1h", "4h", "8h", "24h", "3d", "7d"];

/// The dialog's copy (`PollDialogLabels`, already filled in: the title names the channel).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Labels {
    pub title: SharedString,
    pub close: SharedString,
    pub cancel: SharedString,
    pub post: SharedString,
    pub question: SharedString,
    pub counter: SharedString,
    pub answers: SharedString,
    pub answer_range: SharedString,
    pub add_answer: SharedString,
    pub duration: SharedString,
    pub multi: SharedString,
    pub footer: SharedString,
    pub durations: [SharedString; 6],
}

type OnIndex = Rc<dyn Fn(usize, &mut Window, &mut App)>;
type OnClick = Rc<dyn Fn(&mut Window, &mut App)>;

#[derive(IntoElement)]
pub struct PollDialog {
    id: ElementId,
    bot_name: SharedString,
    labels: Labels,
    question: AnyElement,
    question_over: bool,
    answers: Vec<(AnyElement, bool)>,
    duration: usize,
    multi: bool,
    can_post: bool,
    error: Option<SharedString>,
    on_cancel: Option<OnClick>,
    on_post: Option<OnClick>,
    on_add: Option<OnClick>,
    on_remove: Option<OnIndex>,
    on_duration: Option<OnIndex>,
    on_multi: Option<OnClick>,
    on_backdrop: Option<OnClick>,
    initial_focus: InitialFocus,
    post_wrap: Option<Box<dyn FnOnce(AnyElement) -> AnyElement>>,
}

impl PollDialog {
    pub fn new(
        id: impl Into<ElementId>,
        bot_name: impl Into<SharedString>,
        labels: Labels,
        question: impl IntoElement,
    ) -> Self {
        Self {
            id: id.into(),
            bot_name: bot_name.into(),
            labels,
            question: question.into_any_element(),
            question_over: false,
            answers: Vec::new(),
            duration: 3,
            multi: false,
            can_post: false,
            error: None,
            on_cancel: None,
            on_post: None,
            on_add: None,
            on_remove: None,
            on_duration: None,
            on_multi: None,
            on_backdrop: None,
            initial_focus: InitialFocus::default(),
            post_wrap: None,
        }
    }

    /// The question is over 300 characters (its counter turns red).
    pub fn question_over(mut self, over: bool) -> Self {
        self.question_over = over;
        self
    }

    /// One answer's input, and whether it is over 100 characters.
    pub fn answer(mut self, input: impl IntoElement, over: bool) -> Self {
        self.answers.push((input.into_any_element(), over));
        self
    }

    /// The chosen duration's index in [`DURATIONS`].
    pub fn duration(mut self, index: usize) -> Self {
        self.duration = index;
        self
    }

    pub fn multi(mut self, multi: bool) -> Self {
        self.multi = multi;
        self
    }

    pub fn can_post(mut self, can: bool) -> Self {
        self.can_post = can;
        self
    }

    pub fn error(mut self, error: Option<SharedString>) -> Self {
        self.error = error;
        self
    }

    pub fn on_cancel(mut self, f: impl Fn(&mut Window, &mut App) + 'static) -> Self {
        self.on_cancel = Some(Rc::new(f));
        self
    }

    pub fn on_post(mut self, f: impl Fn(&mut Window, &mut App) + 'static) -> Self {
        self.on_post = Some(Rc::new(f));
        self
    }

    pub fn on_add(mut self, f: impl Fn(&mut Window, &mut App) + 'static) -> Self {
        self.on_add = Some(Rc::new(f));
        self
    }

    pub fn on_remove(mut self, f: impl Fn(usize, &mut Window, &mut App) + 'static) -> Self {
        self.on_remove = Some(Rc::new(f));
        self
    }

    pub fn on_duration(mut self, f: impl Fn(usize, &mut Window, &mut App) + 'static) -> Self {
        self.on_duration = Some(Rc::new(f));
        self
    }

    pub fn on_multi(mut self, f: impl Fn(&mut Window, &mut App) + 'static) -> Self {
        self.on_multi = Some(Rc::new(f));
        self
    }

    /// A press on the backdrop (the web's `onClose`, which the form answers as it does Cancel).
    pub fn on_backdrop(mut self, f: impl Fn(&mut Window, &mut App) + 'static) -> Self {
        self.on_backdrop = Some(Rc::new(f));
        self
    }

    /// What holds the focus when the dialog opens (the question's field: `data-autofocus`).
    pub fn initial_focus(mut self, focus: InitialFocus) -> Self {
        self.initial_focus = focus;
        self
    }

    /// Puts the Post button inside `wrap`'s element (the app's automation box, so a test's pointer can find it).
    pub fn post_wrap(mut self, wrap: impl FnOnce(AnyElement) -> AnyElement + 'static) -> Self {
        self.post_wrap = Some(Box::new(wrap));
        self
    }
}

fn click(id: impl Into<ElementId>, f: Option<OnClick>) -> gpui::Stateful<gpui::Div> {
    div()
        .id(id.into())
        .cursor_pointer()
        .on_click(move |_, window, cx| {
            if let Some(f) = &f {
                f(window, cx);
            }
        })
}

impl RenderOnce for PollDialog {
    fn render(self, _window: &mut Window, cx: &mut App) -> impl IntoElement {
        let app: &App = cx;
        let xs = root_line_height(text::XS);
        let ui = root_line_height(text::UI);
        let answer_count = self.answers.len();
        let question = div()
            .flex()
            .flex_col()
            .gap(px(6.0))
            .child(
                div()
                    .flex()
                    .justify_between()
                    .child(line(
                        self.labels.question.clone(),
                        text::XS,
                        xs,
                        color::FG_2,
                    ))
                    .child(line(
                        self.labels.counter.clone(),
                        text::XS,
                        xs,
                        if self.question_over {
                            color::DANGER
                        } else {
                            color::FG_FAINT
                        },
                    )),
            )
            .child(self.question);
        let remove = self.on_remove.clone();
        let answers = div()
            .flex()
            .flex_col()
            .gap(px(6.0))
            .child(
                div()
                    .flex()
                    .justify_between()
                    .child(line(self.labels.answers.clone(), text::XS, xs, color::FG_2))
                    .child(line(
                        self.labels.answer_range.clone(),
                        text::XS,
                        xs,
                        color::FG_2,
                    )),
            )
            .children(
                self.answers
                    .into_iter()
                    .enumerate()
                    .map(|(i, (input, _over))| {
                        let remove = remove.clone();
                        // PollDialog.css:59: an answer rises in (`d-poll-answer-entry` over `--duration-normal`, `ease`)
                        // when its row is added.
                        crate::motion::poll_entry_in(
                            div()
                                .flex()
                                .items_center()
                                .gap(px(6.0))
                                .child(div().w(px(18.0)).flex().justify_end().child(line(
                                    (i + 1).to_string(),
                                    text::META,
                                    root_line_height(text::META),
                                    color::FG_FAINT,
                                )))
                                .child(div().flex_1().min_w_0().child(input))
                                .child(
                                    div()
                                        .id(("poll-remove-answer", i))
                                        .size(px(30.0))
                                        .rounded(radius::MD)
                                        .flex()
                                        .items_center()
                                        .justify_center()
                                        .opacity(if answer_count <= 2 { 0.25 } else { 1.0 })
                                        .on_click(move |_, window, cx| {
                                            if answer_count > 2
                                                && let Some(f) = &remove
                                            {
                                                f(i, window, cx);
                                            }
                                        })
                                        .child(
                                            Icon::new(IconName::X)
                                                .size(px(14.0))
                                                .color(color::FG_3),
                                        ),
                                ),
                            gpui::ElementId::NamedInteger("poll-answer-enter".into(), i as u64),
                            crate::tokens::motion::NORMAL,
                            app,
                        )
                    }),
            )
            .children((answer_count < 10).then(|| {
                click("poll-add-answer", self.on_add.clone())
                    .ml(px(24.0))
                    .h(px(30.0))
                    .px(px(10.0))
                    .rounded(radius::MD)
                    .flex()
                    .items_center()
                    .gap(px(6.0))
                    .child(
                        Icon::new(IconName::Plus)
                            .size(px(13.0))
                            .color(color::ACCENT),
                    )
                    .child(line(
                        self.labels.add_answer.clone(),
                        text::UI,
                        ui,
                        color::ACCENT,
                    ))
            }));
        let on_duration = self.on_duration.clone();
        let durations = div()
            .flex()
            .p(px(2.0))
            .bg(color::SURFACE_2)
            .border_1()
            .border_color(color::CONTROL_EDGE_FIELD)
            .rounded(radius::SEG)
            .children(self.labels.durations.iter().enumerate().map(|(i, label)| {
                let on = i == self.duration;
                let pick = on_duration.clone();
                div()
                    .id(("poll-duration", i))
                    .h(px(26.0))
                    .px(px(9.0))
                    .rounded(radius::ROW)
                    .flex()
                    .items_center()
                    .cursor_pointer()
                    .when_bg(on, color::ACCENT)
                    .on_click(move |_, window, cx| {
                        if let Some(f) = &pick {
                            f(i, window, cx);
                        }
                    })
                    .child(line(
                        label.clone(),
                        text::BTN,
                        root_line_height(text::BTN),
                        if on {
                            color::ACCENT_INK
                        } else {
                            color::FG_STRONG
                        },
                    ))
            }));
        let switch = click("poll-multi", self.on_multi.clone())
            .flex()
            .items_center()
            .gap(px(10.0))
            .h(px(32.0))
            .child(
                div()
                    .relative()
                    .w(px(36.0))
                    .h(px(20.0))
                    .rounded(radius::PILL)
                    .bg(if self.multi {
                        color::ACCENT
                    } else {
                        color::CONTROL_EDGE
                    })
                    .child(
                        div()
                            .absolute()
                            .top(px(2.0))
                            .left(px(if self.multi { 18.0 } else { 2.0 }))
                            .size(px(16.0))
                            .rounded(radius::PILL)
                            .bg(color::KNOB),
                    ),
            )
            .child(line(
                self.labels.multi.clone(),
                text::UI,
                ui,
                color::FG_CHIP,
            ));
        let settings = div()
            .flex()
            .flex_wrap()
            .items_end()
            .gap(px(16.0))
            .child(
                div()
                    .flex()
                    .flex_col()
                    .gap(px(6.0))
                    .child(line(
                        self.labels.duration.clone(),
                        text::XS,
                        xs,
                        color::FG_2,
                    ))
                    .child(durations),
            )
            .child(switch);
        let body = div()
            .flex()
            .flex_col()
            .gap(px(16.0))
            .child(question)
            .child(answers)
            .child(settings)
            .children(self.error.map(|e| {
                div()
                    .flex()
                    .items_center()
                    .gap(px(6.0))
                    .child(
                        Icon::new(IconName::TriangleAlert)
                            .size(px(13.0))
                            .color(color::DANGER),
                    )
                    .child(line(
                        e,
                        text::BTN,
                        root_line_height(text::BTN),
                        color::DANGER,
                    ))
            }));
        let tile = div()
            .size(px(26.0))
            .rounded(radius::MD)
            .bg(color::BRAND)
            .flex()
            .items_center()
            .justify_center()
            .child(
                line(
                    grapheme_initials(&self.bot_name),
                    text::BADGE,
                    root_line_height(text::BADGE),
                    color::BRAND_INK,
                )
                .weight(font::WEIGHT_DISPLAY),
            );
        // `.d-poll-dialog__close`: the head's own close button (the dialog's default one is off), 28px at the head's
        // top right.
        let x = click("poll-dismiss", self.on_cancel.clone())
            .absolute()
            .top(px(-1.0))
            .left(px(PANEL_W - CLOSE_RIGHT - CLOSE_SIZE - HEAD_PAD))
            .size(px(CLOSE_SIZE))
            .rounded(radius::MD)
            .flex()
            .items_center()
            .justify_center()
            .child(Icon::new(IconName::X).size(px(15.0)).color(color::FG_2));
        let lead = div().relative().child(tile).child(x);
        let cancel = self.on_cancel.clone();
        // `.d-poll-dialog__footer { flex: 1 }`: the note wraps in the room the buttons leave.
        let footer_note = div().flex_1().min_w_0().child(crate::text::flow_paragraph(
            self.labels.footer.clone(),
            text::META,
            root_line_height(text::META),
            color::FG_3,
        ));
        let cancel_button = click("poll-cancel", self.on_cancel.clone())
            .h(px(34.0))
            .px(px(14.0))
            .rounded(radius::SEG)
            .flex()
            .items_center()
            .child(line(
                self.labels.cancel.clone(),
                text::UI,
                ui,
                color::FG_CHIP,
            ));
        let post = click("poll-post", self.on_post.clone())
            .h(px(34.0))
            .px(px(16.0))
            .rounded(radius::SEG)
            .flex()
            .items_center()
            .bg(if self.can_post {
                color::ACCENT
            } else {
                color::HOVER_HI
            })
            .child(
                line(
                    self.labels.post.clone(),
                    text::UI,
                    ui,
                    if self.can_post {
                        color::ACCENT_INK
                    } else {
                        color::FG_3
                    },
                )
                .weight(font::WEIGHT_DISPLAY),
            );
        let post = match self.post_wrap {
            Some(wrap) => wrap(post.into_any_element()),
            None => post.into_any_element(),
        };
        let backdrop = self.on_backdrop.clone();
        let dialog = Dialog::new(
            self.id,
            self.labels.title.clone(),
            self.labels.close.clone(),
        )
        .heading_min(28.0)
        .lead(lead)
        .show_default_close(false)
        .body(body)
        .footer(footer_note)
        .footer(cancel_button)
        .footer(post)
        .initial_focus(self.initial_focus)
        .on_close(move |window, cx| {
            if let Some(f) = &cancel {
                f(window, cx);
            }
        });
        // Without its own handler a press on the backdrop closes the dialog as its close button does.
        match backdrop {
            Some(f) => dialog.on_backdrop(move |window, cx| f(window, cx)),
            None => dialog,
        }
    }
}

trait When: Sized {
    fn when_bg(self, on: bool, bg: gpui::Hsla) -> Self;
}

impl When for gpui::Stateful<gpui::Div> {
    fn when_bg(self, on: bool, bg: gpui::Hsla) -> Self {
        if on { self.bg(bg) } else { self }
    }
}

/// Whether `c` continues the grapheme cluster before it (UAX #29 Extend and SpacingMark for the scripts a name is written in,
/// plus the emoji pieces): combining marks, variation selectors, the zero-width non-joiner, the emoji skin tones and the tag
/// characters of a flag.
fn extends(c: char) -> bool {
    matches!(c,
        '\u{0300}'..='\u{036F}' | '\u{0483}'..='\u{0489}' | '\u{0591}'..='\u{05BD}' | '\u{0610}'..='\u{061A}'
        | '\u{064B}'..='\u{065F}' | '\u{0900}'..='\u{0903}' | '\u{093A}'..='\u{094F}' | '\u{1AB0}'..='\u{1AFF}'
        | '\u{1DC0}'..='\u{1DFF}' | '\u{200C}' | '\u{20D0}'..='\u{20FF}' | '\u{3099}'..='\u{309A}'
        | '\u{FE00}'..='\u{FE0F}' | '\u{FE20}'..='\u{FE2F}' | '\u{1F3FB}'..='\u{1F3FF}' | '\u{E0020}'..='\u{E007F}')
}

fn regional(c: char) -> bool {
    ('\u{1F1E6}'..='\u{1F1FF}').contains(&c)
}

/// The first `count` grapheme clusters of `text` as one string. A cluster is a character with the characters that extend it,
/// a zero-width joiner with the character it joins (an emoji family), a pair of regional indicators (a flag), or CRLF; this
/// is the extended-cluster rule of `Intl.Segmenter` for names, short of Hangul jamo and the rarest Indic marks.
fn first_graphemes(text: &str, count: usize) -> String {
    let mut out = String::new();
    let mut chars = text.chars().peekable();
    for _ in 0..count {
        let Some(first) = chars.next() else { break };
        out.push(first);
        if first == '\r' && chars.peek() == Some(&'\n') {
            out.extend(chars.next());
            continue;
        }
        if regional(first) && chars.peek().is_some_and(|c| regional(*c)) {
            out.extend(chars.next());
        }
        while let Some(&next) = chars.peek() {
            if extends(next) {
                out.extend(chars.next());
            } else if next == '\u{200D}' {
                out.extend(chars.next());
                out.extend(chars.next());
            } else {
                break;
            }
        }
    }
    out
}

/// packages/ui/src/initials.ts `graphemeInitials`: the tile's two characters, the first two grapheme clusters of the name
/// upper-cased ("Dilla Bot" is "DI", not the two-word "DB" of an `Avatar`), so an emoji or a combining sequence is never
/// split. The poll dialog's Bot tile and the poll card's voters use it.
pub fn grapheme_initials(name: &str) -> String {
    first_graphemes(name, 2).to_uppercase()
}

#[cfg(test)]
mod tests {
    use super::grapheme_initials;

    /// initials.test.ts: the first two characters of the whole name, not the first letters of two words.
    #[test]
    fn the_tile_takes_the_first_two_characters() {
        assert_eq!(grapheme_initials("Dilla Bot"), "DI");
        assert_eq!(grapheme_initials("Mr. Kim"), "MR");
        assert_eq!(grapheme_initials("x"), "X");
        assert_eq!(grapheme_initials(""), "");
    }

    /// A cluster of several code points is one character: a base with a combining mark, an emoji family joined by zero-width
    /// joiners, a skin-toned emoji and a flag (two regional indicators) are never cut.
    #[test]
    fn a_multi_codepoint_grapheme_is_one_character() {
        assert_eq!(grapheme_initials("e\u{301}a b"), "E\u{301}A");
        let family = "\u{1F468}\u{200D}\u{1F469}\u{200D}\u{1F467}";
        assert_eq!(
            grapheme_initials(&format!("{family}xyz")),
            format!("{family}X")
        );
        let wave = "\u{1F44B}\u{1F3FD}";
        assert_eq!(grapheme_initials(&format!("a{wave}b")), format!("A{wave}"));
        let flags = "\u{1F1F8}\u{1F1EA}\u{1F1F3}\u{1F1F4}\u{1F1E9}";
        assert_eq!(
            grapheme_initials(flags),
            "\u{1F1F8}\u{1F1EA}\u{1F1F3}\u{1F1F4}"
        );
        assert_eq!(grapheme_initials("a\r\nb"), "A\r\n");
    }
}
