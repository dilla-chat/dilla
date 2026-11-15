//! The message body (packages/ui `MessageRow` body, `CodeBlock`; MessageRow.css, CodeBlock.css): the parsed markdown laid
//! out as the web lays it out, every length the CSS's, in Chromium's layout units.
//!
//! A body is a vertical stack of blocks: runs of inline text (strong, em, strike, code spans, links, mention pills), quotes
//! (a 2px left rule, 12px padding, 2px above and below) and fenced code cards. When a body holds a code card the web's
//! body is a grid with a 6px gap between its items; otherwise it is one flow. Inline text is `white-space: pre-wrap;
//! overflow-wrap: anywhere` in a mono face, so every glyph advances 0.6 em: a code span is 0.9 em with a 1px frame and
//! 4px padding, a mention pill has 4px padding, and the line breaks at spaces and, for a word longer than a line, inside
//! it (the way [`dilla_desktop_ui::text::wrap_mono`] does, but in pixels).
//!
//! Em is drawn upright: the embedded mono family has no italic face.

use gpui::{AnyElement, FontWeight, Hsla, IntoElement, ParentElement, Styled, div, px};

use dilla_desktop_ui::text::{
    Face, MONO_ADVANCE, ascent_descent, chromium_baseline, line, line_height,
};
use dilla_desktop_ui::tokens::{color, font, radius, text};

use std::sync::OnceLock;

use super::cache::{Lru, Shared};
use super::highlight::{TokenKind, highlight_cached};
use super::markdown::{Body, Pill};
use crate::screens::messages::css::len;
use crate::strings::{t, tf};

/// What a laid-out body is: its height and its elements, absolutely placed in a box of the body's width.
pub struct Laid {
    pub height: f32,
    pub elements: Vec<AnyElement>,
}

/// MessageRow.css: the body's grid gap when it holds a code card; a quote's margins; the quote's rule and padding.
const BLOCK_GAP: f32 = 6.0;
const QUOTE_MARGIN: f32 = 2.0;
const QUOTE_INDENT: f32 = 2.0 + 12.0;
/// `.d-message-row__code`: 0.9em, a 1px frame and 0.25rem padding; `.d-mention`: 0.25rem padding.
const CODE_SCALE: f32 = 0.9;
const CODE_PAD: f32 = 1.0 + 4.0;
const MENTION_PAD: f32 = 4.0;
/// MessageRow.css `.d-link`: `text-underline-offset: 0.15em`.
const UNDERLINE_OFFSET: f32 = 0.15;

/// CodeBlock.css: the card's 640px maximum, its 30px head (its 1px rule inside), 8px body padding, 19px lines, 320px maximum
/// body, 38px number gutter and 16px right padding.
const CARD_MAX: f32 = 640.0;
const CARD_HEAD: f32 = 30.0;
/// `.d-code-block__head` is border-box: its 1px rule is inside the 30px, so its content is 29px.
const CARD_HEAD_CONTENT: f32 = CARD_HEAD - 1.0;
const CARD_BODY_PAD: f32 = 8.0;
const CARD_LINE: f32 = 19.0;
const CARD_BODY_MAX: f32 = 320.0;
const CARD_GUTTER: f32 = 38.0;
const CARD_RIGHT: f32 = 16.0;

fn body_size() -> f32 {
    f32::from(text::INPUT)
}

/// The body's line height (13px at 1.55).
pub fn body_line() -> f32 {
    line_height(text::INPUT, 1.55)
}

#[derive(Clone, Copy)]
struct Style {
    weight: FontWeight,
    color: Hsla,
    size: f32,
    code: bool,
    strike: bool,
    underline: bool,
    pill: Option<PillStyle>,
}

#[derive(Clone, Copy, PartialEq)]
enum PillStyle {
    Plain,
    Me,
    Broadcast,
    BroadcastMe,
}

impl Style {
    fn base(color: Hsla) -> Self {
        Self {
            weight: font::WEIGHT_BODY,
            color,
            size: body_size(),
            code: false,
            strike: false,
            underline: false,
            pill: None,
        }
    }
}

#[derive(Clone, Copy)]
enum Cell {
    Ch(char, u16),
    /// Padding inside a span (left or right), in px.
    Pad(f32, u16),
    Break,
}

struct Flow {
    styles: Vec<Style>,
    cells: Vec<Cell>,
}

impl Flow {
    fn new() -> Self {
        Self {
            styles: Vec::new(),
            cells: Vec::new(),
        }
    }

    fn style(&mut self, s: Style) -> u16 {
        self.styles.push(s);
        (self.styles.len() - 1) as u16
    }

    fn text(&mut self, s: &str, style: u16) {
        for c in s.chars() {
            self.cells.push(if c == '\n' {
                Cell::Break
            } else {
                Cell::Ch(c, style)
            });
        }
    }

    fn width(&self, c: &Cell) -> f32 {
        match c {
            Cell::Ch(_, s) => self.styles[usize::from(*s)].size * MONO_ADVANCE,
            Cell::Pad(w, _) => *w,
            Cell::Break => 0.0,
        }
    }
}

/// Adds `parts` to `flow` in `base` (inherited) style. Quotes are blocks and handled by the caller.
fn add_inline(flow: &mut Flow, parts: &[Body], base: Style) {
    for p in parts {
        match p {
            Body::Text(s) => {
                let st = flow.style(base);
                flow.text(s, st);
            }
            Body::Strong(c) => add_inline(
                flow,
                c,
                Style {
                    weight: font::WEIGHT_DISPLAY,
                    ..base
                },
            ),
            Body::Em(c) => add_inline(flow, c, base),
            Body::Strike(c) => add_inline(
                flow,
                c,
                Style {
                    strike: true,
                    ..base
                },
            ),
            Body::Code(s) => {
                let st = flow.style(Style {
                    size: base.size * CODE_SCALE,
                    code: true,
                    ..base
                });
                flow.cells.push(Cell::Pad(CODE_PAD, st));
                flow.text(s, st);
                flow.cells.push(Cell::Pad(CODE_PAD, st));
            }
            Body::Link { href } => {
                let st = flow.style(Style {
                    underline: true,
                    ..base
                });
                flow.text(href, st);
            }
            Body::Mention(pill) => {
                let (kind, fg, weight) = pill_look(pill);
                let st = flow.style(Style {
                    pill: Some(kind),
                    color: fg,
                    weight: weight.unwrap_or(base.weight),
                    ..base
                });
                flow.cells.push(Cell::Pad(MENTION_PAD, st));
                flow.text(&pill.label, st);
                flow.cells.push(Cell::Pad(MENTION_PAD, st));
            }
            Body::Quote(_) | Body::CodeBlock { .. } => {}
        }
    }
}

/// `.d-mention`: mine is the mention fill and ink (600); a broadcast is outlined (700 when mine); anyone else is quiet.
fn pill_look(p: &Pill) -> (PillStyle, Hsla, Option<FontWeight>) {
    match (p.broadcast, p.me) {
        (false, false) => (PillStyle::Plain, color::FG, None),
        (false, true) => (PillStyle::Me, color::MENTION_INK, Some(font::WEIGHT_LABEL)),
        (true, false) => (PillStyle::Broadcast, color::FG, None),
        (true, true) => (
            PillStyle::BroadcastMe,
            color::FG,
            Some(font::WEIGHT_DISPLAY),
        ),
    }
}

/// The cells broken into lines of at most `avail` px (spaces hang at a line's end; a word wider than a line breaks inside).
fn break_lines(flow: &Flow, avail: f32) -> Vec<Vec<Cell>> {
    let eps = 0.01;
    let mut lines: Vec<Vec<Cell>> = Vec::new();
    let mut cur: Vec<Cell> = Vec::new();
    let mut cur_w = 0.0f32;
    let is_space = |c: &Cell| matches!(c, Cell::Ch(' ', _));
    let mut i = 0;
    let cells = &flow.cells;
    while i < cells.len() {
        match cells[i] {
            Cell::Break => {
                lines.push(std::mem::take(&mut cur));
                cur_w = 0.0;
                i += 1;
            }
            c if is_space(&c) => {
                cur_w += flow.width(&c);
                cur.push(c);
                i += 1;
            }
            _ => {
                let mut j = i;
                let mut w = 0.0;
                while j < cells.len() && !is_space(&cells[j]) && !matches!(cells[j], Cell::Break) {
                    w += flow.width(&cells[j]);
                    j += 1;
                }
                let word = &cells[i..j];
                if cur_w + w <= avail + eps {
                    cur.extend_from_slice(word);
                    cur_w += w;
                } else {
                    if !cur.is_empty() {
                        lines.push(std::mem::take(&mut cur));
                        cur_w = 0.0;
                    }
                    if w <= avail + eps {
                        cur.extend_from_slice(word);
                        cur_w = w;
                    } else {
                        // Longer than a line: fill each line to its edge.
                        for c in word {
                            let cw = flow.width(c);
                            if cur_w + cw > avail + eps && !cur.is_empty() {
                                lines.push(std::mem::take(&mut cur));
                                cur_w = 0.0;
                            }
                            cur.push(*c);
                            cur_w += cw;
                        }
                    }
                }
                i = j;
            }
        }
    }
    lines.push(cur);
    lines
}

/// One line of cells as elements at (`x0`, `top`): glyph runs, code frames, pill fills, underlines and strikes.
fn draw_line(out: &mut Vec<AnyElement>, flow: &Flow, cells: &[Cell], x0: f32, top: f32, lh: f32) {
    // Trailing spaces hang: nothing is drawn for them.
    let mut end = cells.len();
    while end > 0 && matches!(cells[end - 1], Cell::Ch(' ', _)) {
        end -= 1;
    }
    let cells = &cells[..end];
    let parent_baseline = top + chromium_baseline(Face::Mono, body_size(), lh);
    let mut x = x0;
    let mut i = 0;
    while i < cells.len() {
        let style_of = |c: &Cell| match c {
            Cell::Ch(_, s) | Cell::Pad(_, s) => Some(*s),
            Cell::Break => None,
        };
        let s = style_of(&cells[i]).unwrap_or(0);
        let mut j = i;
        while j < cells.len() && style_of(&cells[j]) == Some(s) {
            j += 1;
        }
        let st = flow.styles[usize::from(s)];
        let run = &cells[i..j];
        let width: f32 = run.iter().map(|c| flow.width(c)).sum();
        let pad_left: f32 = match run.first() {
            Some(Cell::Pad(w, _)) => *w,
            _ => 0.0,
        };
        let text: String = run
            .iter()
            .filter_map(|c| match c {
                Cell::Ch(ch, _) => Some(*ch),
                _ => None,
            })
            .collect();
        let sz = st.size;
        let (ascent, descent) = ascent_descent(Face::Mono, sz);
        // The span's box: the font's content area around the shared baseline.
        if st.code {
            out.push(frame(
                x,
                parent_baseline - ascent - 1.0,
                width,
                ascent + descent + 2.0,
            ));
        }
        if let Some(kind) = st.pill {
            out.push(pill_box(
                kind,
                x,
                parent_baseline - ascent,
                width,
                ascent + descent,
            ));
        }
        if !text.is_empty() {
            let own_lh = line_height(gpui::px(sz), 1.55);
            let own_top = parent_baseline - chromium_baseline(Face::Mono, sz, own_lh);
            out.push(
                line(text, px(sz), own_lh, st.color)
                    .weight(st.weight)
                    .exact(x + pad_left, own_top),
            );
        }
        if st.underline {
            out.push(rule(
                x,
                // `text-underline-offset: 0.15em` from the baseline (MessageRow.css `.d-link`); 1px thick.
                (parent_baseline + UNDERLINE_OFFSET * sz).round(),
                width,
                color_of(st),
            ));
        }
        if st.strike {
            out.push(rule(
                x,
                // Blink puts a line-through a third of the ascent above the baseline.
                (parent_baseline - ascent / 3.0).round(),
                width,
                color_of(st),
            ));
        }
        x += width;
        i = j;
    }
}

fn color_of(st: Style) -> Hsla {
    st.color
}

fn rule(x: f32, top: f32, width: f32, c: Hsla) -> AnyElement {
    div()
        .absolute()
        .left(px(x.round()))
        .top(px(top))
        .w(px(width.round().max(1.0)))
        .h(len(1.))
        .bg(c)
        .into_any_element()
}

/// A span's box at its CSS place: the layout keeps the fractions and snaps the edges as Chromium paints them
/// (`round(x)` to `round(x + width)`, the vendored GPUI's edge snap), where rounding the width alone could move the
/// right edge a pixel off the web's.
fn frame(x: f32, top: f32, width: f32, height: f32) -> AnyElement {
    div()
        .absolute()
        .left(px(x))
        .top(px(top))
        .w(px(width))
        .h(px(height))
        .border_1()
        .border_color(color::HAIRLINE_2)
        .into_any_element()
}

fn pill_box(kind: PillStyle, x: f32, top: f32, width: f32, height: f32) -> AnyElement {
    // As [`frame`]: the edges snap where Chromium paints them.
    let base = div()
        .absolute()
        .left(px(x))
        .top(px(top))
        .w(px(width))
        .h(px(height))
        .rounded(radius::TAG);
    match kind {
        PillStyle::Plain => base.bg(color::SURFACE_HI),
        PillStyle::Me => base.bg(color::MENTION),
        // `background: transparent; box-shadow: inset 0 0 0 1px var(--mention)`.
        PillStyle::Broadcast | PillStyle::BroadcastMe => {
            base.border_1().border_color(color::MENTION)
        }
    }
    .into_any_element()
}

/// The inline run's lines at `top`: elements pushed, the height answered.
fn layout_inline(out: &mut Vec<AnyElement>, flow: &Flow, x: f32, top: f32, avail: f32) -> f32 {
    let lh = body_line();
    let lines = break_lines(flow, avail);
    for (n, l) in lines.iter().enumerate() {
        draw_line(out, flow, l, x, top + n as f32 * lh, lh);
    }
    lines.len() as f32 * lh
}

/// A run's items (inline parts and quotes) as a stack.
fn layout_run(out: &mut Vec<AnyElement>, parts: &[Body], top: f32, avail: f32, fg: Hsla) -> f32 {
    let mut y = top;
    let mut flow = Flow::new();
    let base = Style::base(fg);
    let flush = |out: &mut Vec<AnyElement>, flow: &mut Flow, y: &mut f32| {
        if !flow.cells.is_empty() {
            *y += layout_inline(out, flow, 0.0, *y, avail);
            *flow = Flow::new();
        }
    };
    for p in parts {
        if let Body::Quote(children) = p {
            flush(out, &mut flow, &mut y);
            let mut qflow = Flow::new();
            add_inline(&mut qflow, children, Style::base(color::FG_2));
            let qavail = (avail - QUOTE_INDENT).max(1.0);
            let before = out.len();
            let h = layout_inline(out, &qflow, QUOTE_INDENT, y + QUOTE_MARGIN, qavail);
            let _ = before;
            out.push(
                div()
                    .absolute()
                    .left(len(0.))
                    .top(px((y + QUOTE_MARGIN).round()))
                    .w(len(2.))
                    .h(px(h))
                    .bg(color::FG_2)
                    .into_any_element(),
            );
            y += h + 2.0 * QUOTE_MARGIN;
        } else {
            add_inline(&mut flow, std::slice::from_ref(p), base);
        }
    }
    flush(out, &mut flow, &mut y);
    y - top
}

fn code_height(lines: usize) -> f32 {
    let body = (lines as f32 * CARD_LINE).min(CARD_BODY_MAX) + 2.0 * CARD_BODY_PAD;
    // The 1px frame, the head (its 1px rule inside it), then the body.
    2.0 + CARD_HEAD + body
}

/// A fenced code card at (`x`, `top`) (README §6.4): the language, the line count, Copy, and the numbered, highlighted
/// lines.
fn code_card(lang: Option<&str>, code: &str, width: f32) -> (AnyElement, f32) {
    let rows = highlight_cached(lang, code);
    let height = code_height(rows.len());
    let inner = width - 2.0;
    let mut kids: Vec<AnyElement> = Vec::new();
    // The head: language, count, Copy.
    let lang_text = lang.unwrap_or("text").to_uppercase();
    let micro = text::MICRO;
    let micro_lh = line_height(micro, 1.5);
    let head_text_top = (CARD_HEAD_CONTENT - micro_lh) / 2.0;
    let tracking = crate::screens::LABEL_TRACKING_WIDE;
    let lang_w = lang_text.chars().count() as f32 * f32::from(micro) * (MONO_ADVANCE + tracking);
    kids.push(
        line(lang_text, micro, micro_lh, color::ACCENT_2)
            .weight(font::WEIGHT_LABEL)
            .tracking(tracking)
            .exact(12.0, head_text_top),
    );
    let n = rows.len();
    let count = if n == 1 {
        tf("shell.code.line", &[("n", "1")])
    } else {
        tf("shell.code.lines", &[("n", &n.to_string())])
    };
    kids.push(
        line(count, micro, micro_lh, color::FG_FAINT).exact(12.0 + lang_w + 8.0, head_text_top),
    );
    let meta = text::META;
    let meta_lh = line_height(meta, 1.5);
    let copy = t("shell.code.copy");
    let copy_w = copy.chars().count() as f32 * f32::from(meta) * MONO_ADVANCE;
    // 22px high, 0 8px padding, a 12px icon and 5px between it and the label; 6px from the head's right edge.
    let btn_w = 8.0 + 12.0 + 5.0 + copy_w + 8.0;
    let btn_x = inner - 6.0 - btn_w;
    kids.push(
        div()
            .absolute()
            .left(px(btn_x.round()))
            .top(px((CARD_HEAD_CONTENT - 22.0) / 2.0))
            .w(px(btn_w.round()))
            .h(len(22.))
            .rounded(radius::ROW)
            .flex()
            .items_center()
            .gap(len(5.))
            .px(len(8.))
            .child(
                dilla_desktop_ui::Icon::new(dilla_desktop_ui::IconName::Copy)
                    .size(len(12.))
                    .color(color::FG_STRONG),
            )
            .child(line(copy, meta, meta_lh, color::FG_STRONG))
            .into_any_element(),
    );
    kids.push(
        div()
            .absolute()
            .left(len(0.))
            .top(px(CARD_HEAD_CONTENT))
            .w(px(inner))
            .h(len(1.))
            .bg(color::HAIRLINE)
            .into_any_element(),
    );
    // The body: numbered lines at 19px, tokens coloured.
    let ui = text::UI;
    let ui_adv = f32::from(ui) * MONO_ADVANCE;
    let ui_lh = CARD_LINE;
    let visible = rows
        .len()
        .min((CARD_BODY_MAX / CARD_LINE).floor() as usize + 1);
    let body_top = CARD_HEAD + CARD_BODY_PAD;
    for (r, toks) in rows.iter().take(visible).enumerate() {
        let top = body_top + r as f32 * CARD_LINE;
        let no = (r + 1).to_string();
        let no_w = no.chars().count() as f32 * ui_adv;
        kids.push(line(no, ui, ui_lh, color::CODE_LINE_NO).exact(CARD_GUTTER - 12.0 - no_w, top));
        let mut x = CARD_GUTTER;
        for tok in toks {
            let (c, weight) = token_color(tok.kind);
            let w = tok.text.chars().count() as f32 * ui_adv;
            if tok.kind != TokenKind::Text || !tok.text.trim().is_empty() {
                kids.push(
                    line(tok.text.clone(), ui, ui_lh, c)
                        .weight(weight)
                        .exact(x, top),
                );
            }
            x += w;
        }
        let _ = CARD_RIGHT;
    }
    let card = div()
        .absolute()
        .left(len(0.))
        .top(len(0.))
        .w(px(width))
        .h(px(height))
        .border_1()
        .border_color(color::HAIRLINE_2)
        .rounded(radius::LG)
        .bg(color::BG_2)
        .overflow_hidden()
        .child(
            div()
                .absolute()
                .left(len(0.))
                .top(len(0.))
                .w(px(inner))
                .h(px(CARD_HEAD_CONTENT))
                .bg(color::BG_3),
        )
        .children(kids)
        .into_any_element();
    (card, height)
}

fn token_color(kind: TokenKind) -> (Hsla, FontWeight) {
    let c = match kind {
        TokenKind::Key => color::ACCENT_2,
        TokenKind::Str => color::OK_TEXT,
        TokenKind::Num => color::WARN,
        TokenKind::Com => color::CODE_COMMENT,
        TokenKind::Bool => color::CODE_BOOL,
        TokenKind::Kw => color::CODE_KEYWORD,
        TokenKind::Punct => color::FG_2,
        TokenKind::Text => color::FG_BODY,
    };
    (c, font::WEIGHT_BODY)
}

/// What the height memo holds: heights by the parts and the width (the keys are bounded by the parts' debug text, so the
/// body limit is the web's 4000 characters of body times the markup around them).
fn heights() -> &'static Shared<f32> {
    static HEIGHTS: OnceLock<Shared<f32>> = OnceLock::new();
    HEIGHTS.get_or_init(|| Shared::new(Lru::new(2000, 2_000_000, 40_000)))
}

/// Forgets every remembered height (its keys hold the parts' text; `super::forget_memos`).
pub fn forget_cached() {
    heights().clear();
}

/// [`layout`]'s height, remembered by (parts, width): a draw of a body already laid out in this width costs one lookup.
pub fn height_cached(parts: &[Body], width: f32) -> f32 {
    let key = format!("{:x}|{parts:?}", width.to_bits());
    *heights().memo(&key, || layout(parts, width, color::FG_BODY).height)
}

/// The body `parts` laid out in `width` px, in the colour `fg`.
pub fn layout(parts: &[Body], width: f32, fg: Hsla) -> Laid {
    layout_from(parts, width, fg, 0.0)
}

/// [`layout`] with the first line `y0` px below the box's top: the box sits on a whole pixel and the body's own position
/// has a fraction, which the lines keep (they are placed at the CSS position, not rounded with the box).
pub fn layout_from(parts: &[Body], width: f32, fg: Hsla, y0: f32) -> Laid {
    let mut out: Vec<AnyElement> = Vec::new();
    if !parts.iter().any(|p| matches!(p, Body::CodeBlock { .. })) {
        let h = layout_run(&mut out, parts, y0, width, fg);
        return Laid {
            height: h,
            elements: out,
        };
    }
    // A grid of runs and cards with a 6px gap.
    let mut y = y0;
    let mut first = true;
    let mut run: Vec<Body> = Vec::new();
    let flush_run =
        |out: &mut Vec<AnyElement>, run: &mut Vec<Body>, y: &mut f32, first: &mut bool| {
            if run.is_empty() {
                return;
            }
            if !*first {
                *y += BLOCK_GAP;
            }
            *first = false;
            *y += layout_run(out, run, *y, width, fg);
            run.clear();
        };
    for p in parts {
        if let Body::CodeBlock { text, lang } = p {
            flush_run(&mut out, &mut run, &mut y, &mut first);
            if !first {
                y += BLOCK_GAP;
            }
            first = false;
            let (card, h) = code_card(lang.as_deref(), text, width.min(CARD_MAX + 2.0));
            out.push(
                div()
                    .absolute()
                    .left(len(0.))
                    .top(px(y.round()))
                    .child(card)
                    .into_any_element(),
            );
            y += h;
        } else {
            run.push(p.clone());
        }
    }
    flush_run(&mut out, &mut run, &mut y, &mut first);
    Laid {
        height: y - y0,
        elements: out,
    }
}

/// The body's height without drawing (for the log's row heights, computed from the same layout).
pub fn height_of(parts: &[Body], width: f32) -> f32 {
    layout(parts, width, color::FG_BODY).height
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::screens::messages::markdown::parse_markdown;

    fn parts(s: &str) -> Vec<Body> {
        parse_markdown(s, &|t| Pill {
            label: format!("@{}", &t[..t.len().min(3)]),
            me: false,
            broadcast: false,
            user: None,
        })
    }

    fn lines_of(s: &str, width: f32) -> usize {
        (layout(&parts(s), width, color::FG_BODY).height / body_line()).round() as usize
    }

    #[test]
    fn plain_text_wraps_like_the_mono_pre_wrap() {
        // 7.8px per character: 10 columns in 78px.
        let w = 10.0 * 7.8;
        for s in [
            "one two three",
            "abcdefghij klmno",
            "fits exactly",
            "a\n\nb",
            "aaaaaaaaaaaaaaaaaaaaaaaaaaa",
        ] {
            let expected = dilla_desktop_ui::text::wrap_mono(s, 10).len();
            assert_eq!(lines_of(s, w), expected, "{s:?}");
        }
    }

    #[test]
    fn a_code_span_costs_its_frame_and_padding() {
        // `ab` is 2 * 7.02 + 10 = 24.04px; "xxxxxxxxxxxxxxxxxxxxxxxxxx" fills the rest of the line.
        let one_line = 30.0 * 7.8;
        assert_eq!(lines_of("`ab` end", one_line), 1);
        // The span plus the text cannot fit 4 columns, so it moves to its own line.
        assert_eq!(lines_of("abcd `ab`", 6.0 * 7.8), 2);
    }

    #[test]
    fn a_quote_adds_its_margins_and_a_code_card_its_gap() {
        let w = 400.0;
        let lh = body_line();
        let q = layout(&parts("> quoted"), w, color::FG_BODY).height;
        assert!((q - (lh + 4.0)).abs() < 1e-3);
        let both = layout(&parts("a\n```\nx\n```\nb"), w, color::FG_BODY).height;
        // a, 6, the card, 6, b.
        assert!((both - (2.0 * lh + 2.0 * BLOCK_GAP + code_height(1))).abs() < 1e-3);
    }

    #[test]
    fn a_code_card_scrolls_after_320px_of_lines() {
        assert_eq!(code_height(1), 2.0 + 30.0 + 19.0 + 16.0);
        assert_eq!(code_height(100), 2.0 + 30.0 + 320.0 + 16.0);
    }

    /// The height memo answers what a layout would, for every body and width, and a second ask is a lookup.
    #[test]
    fn the_height_memo_agrees_with_the_layout() {
        for text in [
            "a",
            "one two three four five six seven",
            "> q\nb",
            "```yaml\na: 1\n```\nafter",
            "`c` **b** https://example.com/x",
        ] {
            for width in [60.0, 200.0, 400.0] {
                let p = parts(text);
                let direct = layout(&p, width, color::FG_BODY).height;
                assert_eq!(height_cached(&p, width), direct, "{text:?} at {width}");
                assert_eq!(height_cached(&p, width), direct);
            }
        }
        // A different width is a different entry.
        let p = parts("one two three four five six seven");
        assert_ne!(height_cached(&p, 60.0), height_cached(&p, 400.0));
    }

    /// Underlines and strikes are placed from the CSS offset and the font's ascent, not from fixed guesses: a link and a
    /// struck word draw a rule each.
    #[test]
    fn links_and_strikes_draw_their_rules() {
        let plain = layout(&parts("word"), 400.0, color::FG_BODY).elements.len();
        let link = layout(&parts("https://example.com/x"), 400.0, color::FG_BODY)
            .elements
            .len();
        let strike = layout(&parts("~~word~~"), 400.0, color::FG_BODY)
            .elements
            .len();
        assert_eq!(link, plain + 1);
        assert_eq!(strike, plain + 1);
    }

    #[test]
    fn a_body_with_no_text_has_no_height() {
        assert_eq!(layout(&[], 300.0, color::FG_BODY).height, 0.0);
    }
}
