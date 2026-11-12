//! Text set the way Chromium sets it, so a GPUI line lands on the pixels the web client's line lands on. Every view and
//! component draws its text through [`Line`] (one line, optionally cut with an ellipsis) or [`paragraph`] (wrapped).
//!
//! Three general rules, none of them per element:
//!
//! - **Where a line is.** A line in ordinary layout paints from its own laid-out box. GPUI rounds every length to device
//!   pixels before layout (`round_to_device_pixel` in its taffy layer), so a box can sit up to a device pixel (more where
//!   rounded lengths add up) off the web's 1/64 px position; no probe of the box can recover the web's value. Where that
//!   matters and the web's position is computable from the CSS (the message log: every height is CSS arithmetic in
//!   layout units), the view places the line with [`Line::exact`] at that position instead.
//! - **The baseline.** Chromium puts it `floor((line_height - round(ascent) - round(descent)) / 2) + round(ascent)` below
//!   the line box's top *rounded to a whole logical pixel* (the reference is shot with device-scale emulation, which lays
//!   out and snaps at scale 1); GPUI would put it `(line_height - ascent - descent) / 2 + ascent` below its snapped box.
//! - **The pen.** Skia quantises a glyph's pen to quarter device pixels a quarter pixel further right than GPUI does
//!   ([`SKIA_PEN_DX`], measured over the message bodies of the reference).
//!
//! A line lays out a box of the web line's size (an invisible copy of the text gives the width) and paints the glyphs itself
//! with `ShapedLine::paint`. Line heights are Chromium's layout units ([`line_height`]: the CSS ratio times the size, floored
//! to 1/64 px).

use std::sync::OnceLock;

use gpui::{
    AnyElement, Bounds, FontStyle, FontWeight, Hsla, IntoElement, ParentElement, Pixels,
    SharedString, Styled, TextAlign, TextRun, canvas, div, point, px, transparent_black,
};

use crate::tokens::font;

/// A font family the Signal design uses, with its vertical metrics (hhea/OS2 typo, per em).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Face {
    /// JetBrains Mono: ascent 1.02, descent 0.3, advance 0.6 em.
    Mono,
    /// DM Serif Display italic: ascent 1.036, descent 0.335.
    SerifItalic,
}

/// JetBrains Mono's advance, per em.
pub const MONO_ADVANCE: f32 = 0.6;

/// Whether the embedded JetBrains Mono has a glyph for `c` (else the text system falls back to another face).
pub fn mono_covers(c: char) -> bool {
    static FACE: OnceLock<Option<ttf_parser::Face<'static>>> = OnceLock::new();
    FACE.get_or_init(|| ttf_parser::Face::parse(crate::theme::JETBRAINS_MONO_REGULAR, 0).ok())
        .as_ref()
        .is_some_and(|f| f.glyph_index(c).is_some())
}

impl Face {
    fn metrics(self) -> (f32, f32) {
        match self {
            Self::Mono => (1.02, 0.3),
            Self::SerifItalic => (1.036, 0.335),
        }
    }

    fn family(self) -> &'static str {
        if let Some(family) = hooks().and_then(|h| h.family) {
            return family;
        }
        match self {
            Self::Mono => font::MONO,
            Self::SerifItalic => font::SERIF_ITALIC,
        }
    }
}

/// What a test build hooks into every line: an overlay that reports the line's box (the automation interface's
/// `text-bounds`), and a family that replaces every face (the parity test's fallback-font control).
#[derive(Clone, Copy, Default)]
pub struct Hooks {
    pub probe: Option<fn() -> AnyElement>,
    pub family: Option<&'static str>,
}

static HOOKS: OnceLock<Hooks> = OnceLock::new();

/// Installs the hooks, once per process (later calls are ignored). Shipping builds never call it.
pub fn set_hooks(hooks: Hooks) {
    let _ = HOOKS.set(hooks);
}

fn hooks() -> Option<&'static Hooks> {
    HOOKS.get()
}

/// Chromium's layout unit: 1/64 px, values floored onto it.
pub fn layout_unit(v: f32) -> f32 {
    (v * 64.0).floor() / 64.0
}

/// A CSS `line-height` ratio at a font size, in Chromium's layout units (13px at 1.55 is 20.140625).
pub fn line_height(size: Pixels, ratio: f32) -> f32 {
    layout_unit(f32::from(size) * ratio)
}

/// Where Chromium puts the baseline below a line box's top: the half-leading floored, then the rounded ascent.
pub fn chromium_baseline(face: Face, size: f32, line_height: f32) -> f32 {
    let (a, d) = face.metrics();
    let (ascent, descent) = ((a * size).round(), (d * size).round());
    ((line_height - ascent - descent) / 2.0).floor() + ascent
}

/// GPUI quantises a glyph's pen to quarter device pixels a quarter device pixel further left than Skia (measured over
/// the reference's message bodies): pens move that much right (logical px at the reference's scale 2).
pub const SKIA_PEN_DX: f32 = 0.125;

/// One line of text: `size` and `line_height` in logical pixels, as the web CSS gives them.
#[derive(Clone)]
pub struct Line {
    text: SharedString,
    face: Face,
    size: f32,
    line_height: f32,
    weight: FontWeight,
    color: Hsla,
    spacing: f32,
    ellipsis: bool,
    clip: bool,
}

/// A line of JetBrains Mono.
pub fn line(text: impl Into<SharedString>, size: Pixels, line_height: f32, color: Hsla) -> Line {
    Line {
        text: text.into(),
        face: Face::Mono,
        size: f32::from(size),
        line_height,
        weight: font::WEIGHT_BODY,
        color,
        spacing: 0.0,
        ellipsis: false,
        clip: false,
    }
}

impl Line {
    pub fn weight(mut self, weight: FontWeight) -> Self {
        self.weight = weight;
        self
    }

    pub fn face(mut self, face: Face) -> Self {
        self.face = face;
        self
    }

    /// CSS `letter-spacing` in em (`--label-tracking-wide` is 0.12): each character's pen moves on by its advance plus
    /// that much. GPUI has none of its own.
    pub fn tracking(mut self, em: f32) -> Self {
        self.spacing = em * self.size;
        self
    }

    /// `text-overflow: ellipsis`: the box may shrink below the text, which is then cut with "…".
    pub fn ellipsis(mut self) -> Self {
        self.ellipsis = true;
        self
    }

    /// `overflow: hidden` without an ellipsis (a flex container's text, which `text-overflow` does not reach): the box
    /// may shrink below the text, which is clipped.
    pub fn clip(mut self) -> Self {
        self.clip = true;
        self
    }

    /// The width the web gives the text: the mono advance per character (plus the tracking) when the embedded face
    /// covers every character, else `None` (measured by the text system, which falls back for the rest).
    fn natural_width(&self) -> Option<f32> {
        (self.face == Face::Mono && self.text.chars().all(mono_covers))
            .then(|| self.text.chars().count() as f32 * (self.size * MONO_ADVANCE + self.spacing))
    }

    fn run(&self, len: usize) -> TextRun {
        let mut font = gpui::font(self.face.family());
        font.weight = self.weight;
        if self.face == Face::SerifItalic {
            font.style = FontStyle::Italic;
        }
        TextRun {
            len,
            font,
            color: self.color,
            background_color: None,
            underline: None,
            strikethrough: None,
        }
    }

    fn layout_text(&self) -> gpui::Div {
        let mut text = div()
            .whitespace_nowrap()
            .text_size(px(self.size))
            .line_height(px(self.line_height))
            .font_family(self.face.family())
            .font_weight(self.weight)
            .text_color(transparent_black());
        if self.face == Face::SerifItalic {
            text = text.italic();
        }
        match self.natural_width() {
            Some(width) => text.flex_none().w(px(width)).h(px(self.line_height)),
            None => text.flex_none().child(self.text.clone()),
        }
    }
}

/// The text to paint in `width`: all of it, or (with an ellipsis) as many characters as fit before "…".
/// `width` is the box as laid out (snapped to device pixels at `scale`): a text that fits its unsnapped box is whole.
fn fitted(line: &Line, width: f32, scale: f32) -> SharedString {
    let Some(natural) = line.natural_width() else {
        return line.text.clone();
    };
    if !line.ellipsis || natural <= width + 1.0 / scale {
        return line.text.clone();
    }
    let advance = line.size * MONO_ADVANCE + line.spacing;
    let keep = ((width - advance) / advance).floor().max(0.0) as usize;
    let mut cut: String = line.text.chars().take(keep).collect();
    cut.push('…');
    cut.into()
}

impl Line {
    /// The glyph painter of a box whose web position is its laid-out origin plus `delta` (logical px).
    fn painter(&self, delta: (f32, f32)) -> impl IntoElement {
        let (a, d) = self.face.metrics();
        let (size, spacing) = (self.size, self.spacing);
        let baseline_in_box = chromium_baseline(self.face, self.size, self.line_height);
        let ascent = a * size;
        let advance = size * MONO_ADVANCE + spacing;
        let this = self.clone();
        canvas(
            move |bounds: Bounds<Pixels>, window, _cx| {
                let text = fitted(&this, f32::from(bounds.size.width), window.scale_factor());
                // Letter-spaced text is shaped one character at a time; anything else in one run.
                let pieces: Vec<(SharedString, TextRun)> = if this.spacing == 0.0 {
                    vec![(text.clone(), this.run(text.len()))]
                } else {
                    text.chars()
                        .map(|c| (SharedString::from(c.to_string()), this.run(c.len_utf8())))
                        .collect()
                };
                pieces
                    .iter()
                    .map(|(t, run)| {
                        window.text_system().shape_line(
                            t.clone(),
                            px(size),
                            std::slice::from_ref(run),
                            None,
                        )
                    })
                    .collect::<Vec<_>>()
            },
            move |bounds, shaped, window, cx| {
                let x = f32::from(bounds.origin.x) + delta.0;
                let top = f32::from(bounds.origin.y) + delta.1;
                // Chromium paints a line from its box's top rounded to a whole logical pixel.
                let baseline = top.round() + baseline_in_box;
                for (i, line) in shaped.iter().enumerate() {
                    let pen = point(
                        px(x + SKIA_PEN_DX + i as f32 * advance),
                        px(baseline - ascent),
                    );
                    // With a line height of ascent + descent, GPUI's baseline is the origin plus the ascent.
                    let _ = line.paint(
                        pen,
                        px(ascent + d * size),
                        TextAlign::Left,
                        None,
                        window,
                        cx,
                    );
                }
            },
        )
        .absolute()
        .top_0()
        .left_0()
        .size_full()
    }

    fn boxed(self, delta: (f32, f32)) -> gpui::Div {
        let painter = self.painter(delta);
        let mut boxed = div()
            .relative()
            .h(px(self.line_height))
            .child(self.layout_text())
            .child(painter);
        boxed = if self.ellipsis || self.clip {
            boxed.flex().min_w_0().flex_shrink(1.0).overflow_hidden()
        } else {
            boxed.flex_none()
        };
        if let Some(probe) = hooks().and_then(|h| h.probe) {
            boxed = boxed.child(probe());
        }
        boxed
    }

    /// The line at the web's position (`x`, `top`) in its parent (logical px, fractions kept), for a view that computes
    /// its lines' positions from the CSS (the message log). The parent must sit on a whole logical pixel; the line's box
    /// takes the whole pixel at or before the position, and its glyphs are painted from the position itself.
    pub fn exact(self, x: f32, top: f32) -> AnyElement {
        let (bx, by) = (x.floor(), top.floor());
        div()
            .absolute()
            .left(px(bx))
            .top(px(by))
            .child(self.boxed((x - bx, top - by)))
            .into_any_element()
    }
}

impl IntoElement for Line {
    type Element = AnyElement;

    fn into_element(self) -> Self::Element {
        self.boxed((0.0, 0.0)).into_any_element()
    }
}

/// Mono text broken into lines of at most `columns` characters the way Chromium breaks `white-space: pre-wrap;
/// overflow-wrap: anywhere`: at newlines, after spaces (a run of spaces hangs at the line's end), and inside a word that is
/// longer than a line.
pub fn wrap_mono(text: &str, columns: usize) -> Vec<String> {
    let columns = columns.max(1);
    let mut out = Vec::new();
    for hard in text.split('\n') {
        let chars: Vec<char> = hard.chars().collect();
        if chars.is_empty() {
            out.push(String::new());
            continue;
        }
        let mut start = 0;
        while start < chars.len() {
            if chars.len() - start <= columns {
                out.push(chars[start..].iter().collect());
                break;
            }
            // The last break opportunity (after a space) inside the line, else a break mid-word.
            let limit = start + columns;
            let mut end = limit;
            if chars[limit] != ' '
                && let Some(space) = (start..limit).rev().find(|&i| chars[i] == ' ')
            {
                end = space + 1;
            }
            // Spaces at the break hang: they stay on this line and do not start the next.
            let mut next = end;
            while next < chars.len() && chars[next] == ' ' {
                next += 1;
            }
            let line: String = chars[start..end].iter().collect();
            out.push(line.trim_end_matches(' ').to_owned());
            start = next;
        }
    }
    out
}

/// How many mono characters of `size` fit in `width` (Chromium's advances are the font's 0.6 em).
pub fn columns(size: Pixels, width: f32) -> usize {
    ((width + 0.01) / (f32::from(size) * MONO_ADVANCE))
        .floor()
        .max(1.0) as usize
}

/// Mono `text` wrapped to `width`, one [`Line`] per line, `line_height` apart.
pub fn paragraph(text: &str, size: Pixels, line_height: f32, color: Hsla, width: f32) -> Vec<Line> {
    wrap_mono(text, columns(size, width))
        .into_iter()
        .map(|l| line(l, size, line_height, color))
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn baselines_match_the_measured_web_lines() {
        assert_eq!(chromium_baseline(Face::Mono, 13.0, 20.15), 14.0);
        assert_eq!(chromium_baseline(Face::Mono, 10.5, 15.75), 11.0);
        assert_eq!(chromium_baseline(Face::Mono, 12.0, 15.6), 11.0);
    }

    #[test]
    fn coverage_is_the_embedded_face() {
        assert!(mono_covers('a') && mono_covers('…') && mono_covers('›') && mono_covers('ö'));
        assert!(!mono_covers('↵'));
    }

    #[test]
    fn line_heights_are_layout_units() {
        assert_eq!(line_height(px(13.0), 1.55), 20.140625);
        assert_eq!(line_height(px(12.5), 1.5), 18.75);
    }

    #[test]
    fn wrapping_follows_pre_wrap() {
        assert_eq!(wrap_mono("one two three", 6), ["one", "two", "three"]);
        assert_eq!(wrap_mono("one two three", 7), ["one two", "three"]);
        assert_eq!(wrap_mono("abcdefghij", 4), ["abcd", "efgh", "ij"]);
        assert_eq!(wrap_mono("a\n\nb", 4), ["a", "", "b"]);
        assert_eq!(wrap_mono("fits exactly", 12), ["fits exactly"]);
    }
}
