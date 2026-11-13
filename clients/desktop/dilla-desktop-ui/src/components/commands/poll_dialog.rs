//! `PollDialog` (packages/ui PollDialog): the `/poll create` form in the shared [`Dialog`]: the Bot's tile and a close
//! button in the head, the question with its `n / 300` counter, two to ten answers, the duration segments, the "several
//! answers" switch, and Cancel / Post poll. The inputs are the screen's (it owns their state); this draws around them.

use std::rc::Rc;

use gpui::{
    AnyElement, App, ElementId, InteractiveElement, IntoElement, ParentElement, RenderOnce,
    SharedString, StatefulInteractiveElement, Styled, Window, div, px,
};

use crate::components::avatar::initials_of;
use crate::components::dialog::Dialog;
use crate::icon::Icon;
use crate::icon_names::IconName;
use crate::text::{line, line_height};
use crate::tokens::{color, font, radius, text};

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
    fn render(self, _window: &mut Window, _cx: &mut App) -> impl IntoElement {
        let xs = line_height(text::XS, 1.5);
        let ui = line_height(text::UI, 1.5);
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
                        div()
                            .flex()
                            .items_center()
                            .gap(px(6.0))
                            .child(div().w(px(18.0)).flex().justify_end().child(line(
                                (i + 1).to_string(),
                                text::META,
                                line_height(text::META, 1.5),
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
                                        Icon::new(IconName::X).size(px(14.0)).color(color::FG_3),
                                    ),
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
                        line_height(text::BTN, 1.5),
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
                        line_height(text::BTN, 1.5),
                        color::DANGER,
                    ))
            }));
        let lead = div()
            .size(px(26.0))
            .rounded(radius::MD)
            .bg(color::BRAND)
            .flex()
            .items_center()
            .justify_center()
            .child(
                line(
                    initials_of(&self.bot_name),
                    text::BADGE,
                    line_height(text::BADGE, 1.5),
                    color::BRAND_INK,
                )
                .weight(font::WEIGHT_DISPLAY),
            );
        let cancel = self.on_cancel.clone();
        let footer_note = div().flex_1().child(line(
            self.labels.footer.clone(),
            text::META,
            line_height(text::META, 1.5),
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
        Dialog::new(
            self.id,
            self.labels.title.clone(),
            self.labels.close.clone(),
        )
        .lead(lead)
        .body(body)
        .footer(footer_note)
        .footer(cancel_button)
        .footer(post)
        .on_close(move |window, cx| {
            if let Some(f) = &cancel {
                f(window, cx);
            }
        })
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
