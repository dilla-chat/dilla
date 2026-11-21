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
use crate::tokens::{color, font, motion, radius, text};

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
/// The check's outer size (`.d-poll-card__check`: 14px content in a 1px border).
const CHECK: f32 = 16.0;

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
    /// The log the card is drawn in ([`crate::motion::scope`]): its entrances are remembered under it.
    scope: SharedString,
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
            scope: SharedString::default(),
        }
    }

    /// The log the card is drawn in.
    pub fn scope(mut self, scope: impl Into<SharedString>) -> Self {
        self.scope = scope.into();
        self
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

    /// The Close poll button's box: its label, 8px padding and 1px border either side.
    fn close_width(label: &str) -> f32 {
        label.chars().count() as f32 * f32::from(text::META) * MONO_ADVANCE + 2.0 * (8.0 + 1.0)
    }

    /// The meta text's room (`.d-poll-card__meta > span { flex: 1 }`): the content width less the Close poll button and the
    /// line's 10px gap, so a narrow column wraps the text beside the button.
    fn meta_width(width: f32, close: Option<&str>) -> f32 {
        Self::inner(width) - close.map_or(0.0, |l| Self::close_width(l) + 10.0)
    }

    /// The meta line's height: the text's wrapped lines, or the Close poll button's (26px with its border) when that is taller.
    fn meta_height(meta: &str, width: f32, close: Option<&str>) -> f32 {
        let lh = crate::text::root_line_height(text::META);
        let lines = paragraph(
            meta,
            text::META,
            lh,
            color::FG_3,
            Self::meta_width(width, close),
        )
        .len()
        .max(1) as f32;
        let text = lines * lh;
        if close.is_some() {
            text.max(26.0)
        } else {
            text
        }
    }

    /// An answer's room in its row (`.d-poll-card__text { flex: 1 }`): the row's content beside the check (14px and its 1px
    /// border, 8px gap), the voters (their span's `13px` each, 8px gap, present even with none) and the 36px percentage
    /// (8px gap).
    fn answer_width(width: f32, voters: usize) -> f32 {
        let content = Self::inner(width) - 2.0 - 2.0 * ANSWER_PAD;
        content - (CHECK + 8.0) - 36.0 - 2.0 * 8.0 - voters.min(5) as f32 * VOTER_STEP
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
    /// closed tag's label of a closed poll, `meta` the meta line's text, and `close` the label of Close poll when the meta line
    /// holds it (an open poll of the viewer's: its 26px button; else the line is as tall as its text). An answer's row is as tall as its lines, at least the 34px row.
    pub fn height(
        question: &str,
        options: &[Option_],
        width: f32,
        closed: Option<&str>,
        meta: &str,
        close: Option<&str>,
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
        let meta = Self::meta_height(meta, width, close);
        TOP_MARGIN + 2.0 + 2.0 * PAD + head + rows + meta + GAP * (options.len() as f32 + 1.0)
    }
}

impl RenderOnce for PollCard {
    fn render(self, window: &mut Window, cx: &mut App) -> impl IntoElement {
        let ui_lh = crate::text::root_line_height(text::UI);
        // PollCard.css:36-37: the card rises in (`d-poll-card-entry`) and each voter pops in (`d-poll-voter-pop`), both over
        // `--duration-step` on `ease`, once from the first draw: the card is drawn inside a message row, which the log
        // stops drawing off screen, so the entrances are remembered by key, not by element state.
        let card_key = crate::motion::scoped(&self.scope, &format!("poll-card-{:?}", self.id));
        let card_age = crate::motion::entered_age(cx, card_key.clone(), motion::STEP);
        // A vote taken back is a voter gone: when it is cast again the voter pops again, as the web's new span does.
        let voter_prefix = format!("{card_key}|");
        crate::motion::forget_entered(cx, &voter_prefix, |k| {
            self.options.iter().enumerate().any(|(i, o)| {
                o.voters
                    .iter()
                    .take(5)
                    .any(|name| k == format!("{voter_prefix}{i}|{name}"))
            })
        });
        let voter_ages: Vec<Vec<Option<std::time::Duration>>> = self
            .options
            .iter()
            .enumerate()
            .map(|(i, o)| {
                o.voters
                    .iter()
                    .take(5)
                    .map(|name| {
                        crate::motion::entered_age(
                            cx,
                            format!("{card_key}|{i}|{name}"),
                            motion::STEP,
                        )
                    })
                    .collect()
            })
            .collect();
        if card_age.is_some() || voter_ages.iter().flatten().any(Option::is_some) {
            window.request_animation_frame();
        }
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
            // `.d-poll-card__check` is content-box: 14px inside its 1px border, 16px outer.
            let check = div()
                .flex_none()
                .size(px(CHECK))
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
                    let age = voter_ages[i][v];
                    crate::motion::pop_frame(
                        age.map(|a| crate::motion::poll_voter_pop_at(a, motion::STEP)),
                        &|pop| voter_disc(i * 8 + v, name, pop),
                    )
                    .ml(px(-5.0))
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
                            div()
                                .debug_selector(move || format!("poll-answer-text-{i}"))
                                .flex_1()
                                .min_w_0()
                                .flex()
                                .flex_col()
                                .children(
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
        let close_label = (self.can_close && !self.closed).then(|| self.labels.close.to_string());
        let meta_lh = crate::text::root_line_height(text::META);
        let meta = div()
            .flex()
            .items_center()
            .gap(px(10.0))
            .min_h(px(Self::meta_height(
                &self.meta,
                self.width,
                close_label.as_deref(),
            )))
            .child(div().flex_1().flex().flex_col().children(paragraph(
                &self.meta,
                text::META,
                meta_lh,
                color::FG_3,
                Self::meta_width(self.width, close_label.as_deref()),
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
        let card = div()
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
            .child(meta);
        let card = match card_age {
            Some(age) => {
                let (opacity, offset) = crate::motion::poll_entry_at(age, motion::STEP);
                card.relative().top(px(offset)).opacity(opacity)
            }
            None => card,
        };
        div().pt(px(TOP_MARGIN)).child(card)
    }
}

/// A voter's disc in its 2px `--surface` ring, `scale` times its size (a pop's frame, [`crate::motion::pop_frame`]); at
/// full size it keeps its element id. The ring is PollCard.css's `box-shadow: 0 0 0 2px var(--surface)`: drawn outside the
/// 18px disc and taking no room, so each voter after the first adds [`VOTER_STEP`] to the row.
fn voter_disc(n: usize, name: &str, pop: crate::motion::Pop) -> gpui::AnyElement {
    let s = pop.scale();
    let disc = voter(("poll-voter", n), name, s, pop.live());
    div()
        .rounded(radius::PILL)
        .shadow(vec![gpui::BoxShadow {
            color: color::SURFACE,
            offset: gpui::point(px(0.0), px(0.0)),
            blur_radius: px(0.0),
            spread_radius: px(2.0 * s),
            inset: false,
        }])
        .child(disc)
        .into_any_element()
}

/// PollCard.css `.d-poll-card__voters .d-avatar`: an 18px disc with `--text-poll-voter` initials (no shared Avatar size
/// has these, so the card draws its own).
fn voter(id: impl Into<ElementId>, name: &str, scale: f32, live: bool) -> gpui::AnyElement {
    let (h, s, l) = colour_of(name);
    let fill = gpui::hsla(
        f32::from(h) / 360.0,
        f32::from(s) / 100.0,
        f32::from(l) / 100.0,
        1.0,
    );
    let disc = div()
        .size(px(18.0 * scale))
        .rounded(radius::PILL)
        .bg(fill)
        .flex()
        .items_center()
        .justify_center()
        .child(
            line(
                grapheme_initials(name),
                text::POLL_VOTER * scale,
                crate::text::root_line_height(text::POLL_VOTER) * scale,
                color::BRAND_INK,
            )
            .weight(font::WEIGHT_DISPLAY),
        );
    // Only the voter itself (at rest, or scaled while it pops) has the id; the hidden copy that holds its place has none.
    if live {
        disc.id(id.into()).into_any_element()
    } else {
        disc.into_any_element()
    }
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

    /// Final review (ui I1): the drawn answer row is the row [`PollCard::answer_width`] wraps the answer for, with five
    /// voters as with none: the check 16px outer, each voter 13px (its 2px ring is a shadow, taking no room), so a wrapped
    /// answer never runs under the avatars.
    #[gpui::test]
    fn the_answer_column_is_as_wide_as_its_lines_are_wrapped(cx: &mut gpui::TestAppContext) {
        use gpui::{Context, Render};

        struct Host {
            voters: usize,
        }
        impl Render for Host {
            fn render(&mut self, _: &mut Window, _: &mut Context<Self>) -> impl IntoElement {
                let voters = ["Ada", "Bjorn", "Liv", "Mira", "Sven", "Kari"][..self.voters]
                    .iter()
                    .map(|n| SharedString::from(*n))
                    .collect();
                div().w(px(WIDTH)).child(PollCard::new(
                    "poll",
                    "Which route?",
                    false,
                    false,
                    "Open",
                    vec![Option_ {
                        text: "the long coastal road past the old lighthouse".into(),
                        pct: 50,
                        mine: true,
                        winner: false,
                        voters,
                    }],
                    Labels {
                        closed: "CLOSED".into(),
                        close: "Close poll".into(),
                    },
                    WIDTH,
                ))
            }
        }
        const WIDTH: f32 = 400.0;

        cx.update(|cx| cx.set_reduce_motion(true));
        for voters in [0, 1, 5, 6] {
            let (_, vcx) = cx.add_window_view(|_, _| Host { voters });
            vcx.run_until_parked();
            vcx.update(|window, cx| window.draw(cx).clear(cx));
            let column = vcx
                .debug_bounds("poll-answer-text-0")
                .expect("the answer is drawn");
            assert_eq!(
                f32::from(column.size.width),
                PollCard::answer_width(WIDTH, voters),
                "{voters} voters"
            );
        }
    }

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
        let two = PollCard::height(
            "Raid?",
            &answers(2),
            700.0,
            None,
            "Open",
            Some("Close poll"),
        );
        let three = PollCard::height(
            "Raid?",
            &answers(3),
            700.0,
            None,
            "Open",
            Some("Close poll"),
        );
        assert_eq!(three - two, 36.0 + 8.0);
    }
}
