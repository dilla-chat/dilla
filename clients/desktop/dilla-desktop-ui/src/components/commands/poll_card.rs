//! `PollCard` (packages/ui PollCard): the question, one row per answer (its share as a fill, a check, up to five voter
//! avatars and the percentage) and the meta line with Close poll for its creator. PollCard.css is content-box: 12px
//! padding inside a 1px border, 8px between the parts, 36px answers (34px rows in a 1px border).

use std::rc::Rc;

use gpui::{
    App, ElementId, InteractiveElement, IntoElement, ParentElement, RenderOnce, SharedString,
    StatefulInteractiveElement, Styled, Window, div, px, relative,
};

use crate::components::avatar::colour_of;
use crate::components::commands::poll_dialog::grapheme_initials;
use crate::icon::Icon;
use crate::icon_names::IconName;
use crate::metrics::LABEL_TRACKING;
use crate::text::{MONO_ADVANCE, line, paragraph};
use crate::tokens::{color, font, radius, text};

/// One answer (`PollCardOption`). Its Debug gives the answer's length and the voters' count only: the answer is a
/// decrypted post.
#[derive(Clone, PartialEq, Eq)]
pub struct Option_ {
    pub text: SharedString,
    pub pct: u32,
    pub mine: bool,
    pub winner: bool,
    pub voters: Vec<SharedString>,
}

impl std::fmt::Debug for Option_ {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Option_")
            .field("text_len", &self.text.len())
            .field("pct", &self.pct)
            .field("mine", &self.mine)
            .field("winner", &self.winner)
            .field("voters", &self.voters.len())
            .finish()
    }
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
/// An answer row's side padding, and the width one voter's avatar takes in the voters' span (18px less its -5px margin).
const ANSWER_PAD: f32 = 10.0;
const VOTER_STEP: f32 = 13.0;

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

    /// The closed tag's width (`.d-poll-card__tag`: 6px padding and a 1px border either side, its label tracked), which the
    /// question shares the head with (`h3 { flex: 1 }`); none for a poll that is open.
    fn tag_width(closed: Option<&str>) -> f32 {
        closed.map_or(0.0, |label| {
            let size = f32::from(text::CHIP);
            14.0 + label.chars().count() as f32 * size * (MONO_ADVANCE + LABEL_TRACKING)
        })
    }

    /// The question's room in the head: the content width beside its 14px icon and 8px gap, and the closed tag with its own
    /// 8px gap.
    fn question_width(width: f32, closed: Option<&str>) -> f32 {
        let tag = Self::tag_width(closed);
        Self::inner(width) - 22.0 - if tag > 0.0 { tag + GAP } else { 0.0 }
    }

    /// The question's wrapped lines, in [`PollCard::question_width`].
    fn question_lines(question: &str, width: f32, closed: Option<&str>) -> usize {
        paragraph(
            question,
            text::INPUT,
            crate::text::root_line_height(text::INPUT),
            color::FG,
            Self::question_width(width, closed),
        )
        .len()
        .max(1)
    }

    /// The meta line's height: the Close poll button's (26px with its border) or the text's line.
    fn meta_height(button: bool) -> f32 {
        if button {
            26.0
        } else {
            crate::text::root_line_height(text::META)
        }
    }

    /// An answer's room in its row (`.d-poll-card__text { flex: 1 }`): the row's content beside the check (14px and its 1px
    /// border, 8px gap), the voters (their span's `13px` each, 8px gap, present even with none) and the 36px percentage
    /// (8px gap).
    fn answer_width(width: f32, voters: usize) -> f32 {
        let content = Self::inner(width) - 2.0 - 2.0 * ANSWER_PAD;
        content - (16.0 + 8.0) - 36.0 - 2.0 * 8.0 - voters.min(5) as f32 * VOTER_STEP
    }

    /// An answer's wrapped lines.
    fn answer_lines(text: &str, width: f32, voters: usize) -> usize {
        paragraph(
            text,
            text::UI,
            crate::text::root_line_height(text::UI),
            color::FG,
            Self::answer_width(width, voters),
        )
        .len()
        .max(1)
    }

    /// The card's height under a message, its 6px top margin included (R21: from the content and the tokens). `closed` is the
    /// closed tag's label of a closed poll, and `button` whether the meta line holds Close poll (an open poll of the viewer's:
    /// its 26px button; else the line is as tall as its text). An answer's row is as tall as its lines, at least the 34px row.
    pub fn height(
        question: &str,
        options: &[Option_],
        width: f32,
        closed: Option<&str>,
        button: bool,
    ) -> f32 {
        let head = Self::question_lines(question, width, closed) as f32
            * crate::text::root_line_height(text::INPUT);
        let ui_lh = crate::text::root_line_height(text::UI);
        let rows: f32 = options
            .iter()
            .map(|o| {
                let lines = Self::answer_lines(&o.text, width, o.voters.len()) as f32;
                (lines * ui_lh).max(ROW) + 2.0
            })
            .sum();
        let meta = Self::meta_height(button);
        TOP_MARGIN + 2.0 + 2.0 * PAD + head + rows + meta + GAP * (options.len() as f32 + 1.0)
    }
}

impl RenderOnce for PollCard {
    fn render(self, _window: &mut Window, _cx: &mut App) -> impl IntoElement {
        let ui_lh = crate::text::root_line_height(text::UI);
        let inner = Self::inner(self.width);
        let tag = self.closed.then(|| self.labels.closed.to_string());
        let question = paragraph(
            &self.question,
            text::INPUT,
            crate::text::root_line_height(text::INPUT),
            color::FG,
            Self::question_width(self.width, tag.as_deref()),
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
                        .child(
                            div().flex_1().min_w_0().flex().flex_col().children(
                                paragraph(
                                    &o.text,
                                    text::UI,
                                    ui_lh,
                                    color::FG,
                                    Self::answer_width(self.width, o.voters.len()),
                                )
                                .into_iter()
                                .map(|l| {
                                    l.weight(if winner {
                                        font::WEIGHT_DISPLAY
                                    } else {
                                        font::WEIGHT_BTN
                                    })
                                }),
                            ),
                        )
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
            .min_h(px(Self::meta_height(self.can_close && !self.closed)))
            .child(div().flex_1().child(line(
                self.meta.clone(),
                text::META,
                crate::text::root_line_height(text::META),
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
                        crate::text::root_line_height(text::META),
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
                grapheme_initials(name),
                text::POLL_VOTER,
                crate::text::root_line_height(text::POLL_VOTER),
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
        let answers = |n: usize| -> Vec<Option_> {
            (0..n)
                .map(|i| Option_ {
                    text: format!("answer {i}").into(),
                    pct: 0,
                    mine: false,
                    winner: false,
                    voters: Vec::new(),
                })
                .collect()
        };
        let two = PollCard::height("Raid?", &answers(2), 700.0, None, true);
        let three = PollCard::height("Raid?", &answers(3), 700.0, None, true);
        assert_eq!(three - two, 36.0 + 8.0);
    }
}
