//! `PollCard` (packages/ui PollCard): the question, one row per answer (its share as a fill, a check, up to five voter
//! avatars and the percentage) and the meta line with Close poll for its creator. PollCard.css is content-box: 12px
//! padding inside a 1px border, 8px between the parts, 36px answers (34px rows in a 1px border).

use std::rc::Rc;

use gpui::{
    App, ElementId, InteractiveElement, IntoElement, ParentElement, RenderOnce, SharedString,
    StatefulInteractiveElement, Styled, Window, div, px, relative,
};

use crate::components::avatar::{colour_of, initials_of};
use crate::icon::Icon;
use crate::icon_names::IconName;
use crate::metrics::LABEL_TRACKING;
use crate::text::{line, line_height, paragraph};
use crate::tokens::{color, font, radius, text};

/// One answer (`PollCardOption`).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Option_ {
    pub text: SharedString,
    pub pct: u32,
    pub mine: bool,
    pub winner: bool,
    pub voters: Vec<SharedString>,
}

/// The card's copy.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Labels {
    pub closed: SharedString,
    pub close: SharedString,
}

/// PollCard.css: the card's widest (440px content), padding, gaps and rows.
pub const MAX_WIDTH: f32 = 440.0;
const PAD: f32 = 12.0;
const GAP: f32 = 8.0;
const ROW: f32 = 34.0;
const TOP_MARGIN: f32 = 6.0;

type OnVote = Rc<dyn Fn(usize, &mut Window, &mut App)>;
type OnClose = Rc<dyn Fn(&mut Window, &mut App)>;

#[derive(IntoElement)]
pub struct PollCard {
    id: ElementId,
    question: SharedString,
    closed: bool,
    multi: bool,
    meta: SharedString,
    options: Vec<Option_>,
    can_vote: bool,
    can_close: bool,
    labels: Labels,
    width: f32,
    on_vote: Option<OnVote>,
    on_close: Option<OnClose>,
}

impl PollCard {
    /// `width`: the content box the card sits in (it takes at most 440px of it, plus its padding and border).
    #[allow(clippy::too_many_arguments)]
    pub fn new(
        id: impl Into<ElementId>,
        question: impl Into<SharedString>,
        closed: bool,
        multi: bool,
        meta: impl Into<SharedString>,
        options: Vec<Option_>,
        labels: Labels,
        width: f32,
    ) -> Self {
        Self {
            id: id.into(),
            question: question.into(),
            closed,
            multi,
            meta: meta.into(),
            options,
            can_vote: false,
            can_close: false,
            labels,
            width,
            on_vote: None,
            on_close: None,
        }
    }

    pub fn can_vote(mut self, can: bool) -> Self {
        self.can_vote = can;
        self
    }

    pub fn can_close(mut self, can: bool) -> Self {
        self.can_close = can;
        self
    }

    pub fn on_vote(mut self, f: impl Fn(usize, &mut Window, &mut App) + 'static) -> Self {
        self.on_vote = Some(Rc::new(f));
        self
    }

    pub fn on_close(mut self, f: impl Fn(&mut Window, &mut App) + 'static) -> Self {
        self.on_close = Some(Rc::new(f));
        self
    }

    /// The card's content width in a box `width` wide.
    fn inner(width: f32) -> f32 {
        (width - 2.0 * PAD - 2.0).clamp(0.0, MAX_WIDTH)
    }

    /// The question's wrapped lines, in the content width beside its 14px icon and 8px gap.
    fn question_lines(question: &str, width: f32) -> usize {
        paragraph(
            question,
            text::INPUT,
            line_height(text::INPUT, 1.5),
            color::FG,
            Self::inner(width) - 22.0,
        )
        .len()
        .max(1)
    }

    /// The card's height under a message, its 6px top margin included (R21: from the content and the tokens).
    pub fn height(question: &str, answers: usize, width: f32) -> f32 {
        let head = Self::question_lines(question, width) as f32 * line_height(text::INPUT, 1.5);
        let rows = answers as f32 * (ROW + 2.0);
        let meta = 26.0;
        TOP_MARGIN + 2.0 + 2.0 * PAD + head + rows + meta + GAP * (answers as f32 + 1.0)
    }
}

impl RenderOnce for PollCard {
    fn render(self, _window: &mut Window, _cx: &mut App) -> impl IntoElement {
        let ui_lh = line_height(text::UI, 1.5);
        let inner = Self::inner(self.width);
        let question = paragraph(
            &self.question,
            text::INPUT,
            line_height(text::INPUT, 1.5),
            color::FG,
            inner - 22.0,
        )
        .into_iter()
        .map(|l| l.weight(font::WEIGHT_DISPLAY));
        let head = div()
            .flex()
            .items_start()
            .gap(px(8.0))
            .child(
                div().pt(px(2.0)).child(
                    Icon::new(IconName::Vote)
                        .size(px(14.0))
                        .color(color::ACCENT),
                ),
            )
            .child(div().flex_1().flex().flex_col().children(question))
            .children(self.closed.then(|| {
                div()
                    .px(px(6.0))
                    .rounded(radius::TAG)
                    .border_1()
                    .border_color(color::CONTROL_EDGE_KEY)
                    .child(
                        line(self.labels.closed.clone(), text::CHIP, 16.0, color::FG_2)
                            .tracking(LABEL_TRACKING),
                    )
            }));
        let options = self.options.iter().enumerate().map(|(i, o)| {
            let winner = self.closed && o.winner && o.pct > 0;
            let edge = if o.mine {
                color::ACCENT
            } else if winner {
                color::POLL_WINNER_EDGE
            } else {
                color::CONTROL_EDGE_FIELD
            };
            let fill = if o.mine {
                color::POLL_MINE_FILL
            } else if winner {
                color::POLL_WINNER_FILL
            } else {
                color::POLL_FILL
            };
            let check = div()
                .flex_none()
                .size(px(14.0))
                .rounded(if self.multi {
                    radius::TAG
                } else {
                    radius::PILL
                })
                .border_1()
                .border_color(if o.mine {
                    color::ACCENT
                } else {
                    color::CONTROL_EDGE_KEY
                })
                .when_bg(o.mine, color::ACCENT)
                .flex()
                .items_center()
                .justify_center()
                .children(o.mine.then(|| {
                    Icon::new(IconName::Check)
                        .size(px(10.0))
                        .color(color::ACCENT_INK)
                }));
            let voters = div()
                .flex()
                .children(o.voters.iter().take(5).enumerate().map(|(v, name)| {
                    div()
                        .ml(px(-5.0))
                        .rounded(radius::PILL)
                        .border_2()
                        .border_color(color::SURFACE)
                        .child(voter(("poll-voter", i * 8 + v), name))
                }));
            let vote = self.on_vote.clone();
            let can = !self.closed && self.can_vote;
            div()
                .id(("poll-option", i))
                .relative()
                .min_h(px(ROW + 2.0))
                .rounded(radius::SEG)
                .border_1()
                .border_color(edge)
                .overflow_hidden()
                .when_click(can, move |window, cx| {
                    if let Some(f) = &vote {
                        f(i, window, cx);
                    }
                })
                .child(
                    div()
                        .absolute()
                        .left_0()
                        .top_0()
                        .bottom_0()
                        .w(relative(o.pct as f32 / 100.0))
                        .bg(fill),
                )
                .child(
                    div()
                        .relative()
                        .flex()
                        .items_center()
                        .gap(px(8.0))
                        .min_h(px(ROW))
                        .px(px(10.0))
                        .child(check)
                        .child(div().flex_1().min_w_0().child(
                            line(o.text.clone(), text::UI, ui_lh, color::FG).weight(if winner {
                                font::WEIGHT_DISPLAY
                            } else {
                                font::WEIGHT_BTN
                            }),
                        ))
                        .child(voters)
                        .child(div().w(px(36.0)).flex().justify_end().child(line(
                            format!("{}%", o.pct),
                            text::UI,
                            ui_lh,
                            color::FG_STRONG,
                        ))),
                )
        });
        let close = self.on_close.clone();
        let meta = div()
            .flex()
            .items_center()
            .gap(px(10.0))
            .min_h(px(26.0))
            .child(div().flex_1().child(line(
                self.meta.clone(),
                text::META,
                line_height(text::META, 1.5),
                color::FG_3,
            )))
            .children((self.can_close && !self.closed).then(|| {
                div()
                    .id("poll-close")
                    .h(px(26.0))
                    .px(px(8.0))
                    .rounded(radius::ROW)
                    .border_1()
                    .border_color(color::CONTROL_EDGE)
                    .flex()
                    .items_center()
                    .cursor_pointer()
                    .on_click(move |_, window, cx| {
                        if let Some(f) = &close {
                            f(window, cx);
                        }
                    })
                    .child(line(
                        self.labels.close.clone(),
                        text::META,
                        line_height(text::META, 1.5),
                        color::FG,
                    ))
            }));
        div().pt(px(TOP_MARGIN)).child(
            div()
                .id(self.id)
                .w(px(inner + 2.0 * PAD + 2.0))
                .p(px(PAD))
                .border_1()
                .border_color(color::CONTROL_EDGE_FIELD)
                .rounded(radius::XL)
                .bg(color::SURFACE)
                .flex()
                .flex_col()
                .gap(px(GAP))
                .child(head)
                .children(options)
                .child(meta),
        )
    }
}

/// PollCard.css `.d-poll-card__voters .d-avatar`: an 18px disc with `--text-poll-voter` initials (no shared Avatar size
/// has these, so the card draws its own).
fn voter(id: impl Into<ElementId>, name: &str) -> impl IntoElement {
    let (h, s, l) = colour_of(name);
    let fill = gpui::hsla(
        f32::from(h) / 360.0,
        f32::from(s) / 100.0,
        f32::from(l) / 100.0,
        1.0,
    );
    div()
        .id(id.into())
        .size(px(18.0))
        .rounded(radius::PILL)
        .bg(fill)
        .flex()
        .items_center()
        .justify_center()
        .child(
            line(
                initials_of(name),
                text::POLL_VOTER,
                line_height(text::POLL_VOTER, 1.5),
                color::BRAND_INK,
            )
            .weight(font::WEIGHT_DISPLAY),
        )
}

trait When: Sized {
    fn when_bg(self, on: bool, bg: gpui::Hsla) -> Self;
}

impl When for gpui::Div {
    fn when_bg(self, on: bool, bg: gpui::Hsla) -> Self {
        if on { self.bg(bg) } else { self }
    }
}

trait WhenClick: Sized {
    fn when_click(self, on: bool, f: impl Fn(&mut Window, &mut App) + 'static) -> Self;
}

impl WhenClick for gpui::Stateful<gpui::Div> {
    fn when_click(self, on: bool, f: impl Fn(&mut Window, &mut App) + 'static) -> Self {
        if on {
            self.cursor_pointer()
                .on_click(move |_, window, cx| f(window, cx))
        } else {
            self
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_card_takes_at_most_440px_of_content() {
        assert_eq!(PollCard::inner(1000.0), MAX_WIDTH);
        assert_eq!(PollCard::inner(300.0), 300.0 - 26.0);
    }

    #[test]
    fn its_height_grows_by_answer() {
        let two = PollCard::height("Raid?", 2, 700.0);
        let three = PollCard::height("Raid?", 3, 700.0);
        assert_eq!(three - two, 36.0 + 8.0);
    }
}
