//! Text set the way Chromium sets it, so a GPUI line lands on the pixels the web client's line lands on.
//!
//! Three differences between the two have to be undone, all measured against the beta.5 reference:
//!
//! - **The baseline.** Chromium puts it `floor((line_height - round(ascent) - round(descent)) / 2) + round(ascent)` below
//!   the line box's top rounded to a whole logical pixel (metrics rounded, the half-leading floored: a box at y 13.25
//!   paints its text from 13, one at 12.5 from 13); GPUI puts it `(line_height - ascent - descent) / 2 + ascent` below
//!   its own box, unrounded.
//! - **Snapping.** GPUI snaps every laid-out box to device pixels; Chromium lays out in 1/64 px and only the glyphs land on
//!   pixels. A line whose web position GPUI cannot reach says where it is with [`Line::at`] (the message log with
//!   [`Line::exact`]), and is painted from that true position instead of its snapped box.
//! - **The pen.** Skia quantises a glyph's pen to quarter device pixels a quarter pixel further right than GPUI does
//!   ([`SKIA_PEN_DX`]).
//!
//! So a [`Line`] lays out a box of the web line's size (its text, invisible, gives the width) and paints the glyphs itself
//! with `ShapedLine::paint` at the corrected pen. Every line also reports its box to the automation interface
//! (`text-bounds`), which the parity test turns into its text mask.

use gpui::{
    AnyElement, Bounds, FontStyle, FontWeight, Hsla, IntoElement, ParentElement, Pixels,
    SharedString, Styled, TextAlign, TextRun, canvas, div, point, px, transparent_black,
};

use dilla_desktop_ui::tokens::font;

/// A font family the Signal design uses, with its vertical metrics (hhea/OS2 typo, per em).
#[derive(Clone, Copy, Debug, PartialEq)]
pub enum Face {
    /// JetBrains Mono: ascent 1.02, descent 0.3, advance 0.6.
    Mono,
    /// DM Serif Display italic: ascent 1.036, descent 0.335.
    SerifItalic,
}

impl Face {
    fn metrics(self) -> (f32, f32) {
        match self {
            Self::Mono => (1.02, 0.3),
            Self::SerifItalic => (1.036, 0.335),
        }
    }

    fn family(self) -> &'static str {
        // The parity test's negative control: an automation build started with this variable asks for a family no
        // embedded font answers to, so the text system falls back, and parity must fail visibly.
        #[cfg(feature = "automation")]
        if std::env::var_os(FALLBACK_FONT_VAR).is_some() {
            return "Dilla Parity Missing Font";
        }
        match self {
            Self::Mono => font::MONO,
            Self::SerifItalic => font::SERIF_ITALIC,
        }
    }
}

/// Automation builds only: set, every line asks for a missing family (the fallback-font control of the parity test).
#[cfg(feature = "automation")]
pub const FALLBACK_FONT_VAR: &str = "DILLA_AUTOMATION_FALLBACK_FONT";

/// Where Chromium puts the baseline below a line box's top: the half-leading floored, then the rounded ascent.
pub fn chromium_baseline(face: Face, size: f32, line_height: f32) -> f32 {
    let (a, d) = face.metrics();
    let (ascent, descent) = ((a * size).round(), (d * size).round());
    ((line_height - ascent - descent) / 2.0).floor() + ascent
}

/// GPUI quantises a glyph's pen to quarter device pixels a quarter pixel (an eighth of a logical pixel at scale 2)
/// further left than Skia for the same pen (measured over the reference's message bodies): pens move that much right.
pub const SKIA_PEN_DX: f32 = 0.125;

/// The device pixels per logical pixel the line positions are rounded for (the parity references are shot at 2).
const SCALE: f32 = 2.0;

/// A coordinate in the window, from the edge or the middle it keeps to as the window resizes.
#[derive(Clone, Copy, Debug, PartialEq)]
pub enum Edge {
    /// From the left (or top) edge.
    Start(f32),
    /// Back from the right (or bottom) edge.
    End(f32),
    /// From the middle.
    Mid(f32),
}

impl Edge {
    fn resolve(self, extent: f32) -> f32 {
        match self {
            Self::Start(v) => v,
            Self::End(v) => extent - v,
            Self::Mid(v) => extent / 2.0 + v,
        }
    }
}

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
    at: Option<(Edge, Edge)>,
    /// The `at` x is where the text ends, not where it starts.
    ends: bool,
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
        at: None,
        ends: false,
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

    /// CSS `letter-spacing` (GPUI has none): each character's pen moves on by its advance plus `spacing`.
    pub fn spacing(mut self, spacing: f32) -> Self {
        self.spacing = spacing;
        self
    }

    /// Where the web lays the line box out in the window (its text's left edge and its top): the glyphs are painted from
    /// there, not from GPUI's box, which its layout snaps to device pixels (by up to a device pixel in a nested box).
    pub fn at(mut self, x: Edge, y: Edge) -> Self {
        self.at = Some((x, y));
        self
    }

    /// [`Line::at`] for right-aligned text: `x` is where the text ends (its width is the shaped glyphs', so a glyph
    /// from a fallback face, whose advance differs between the two renderers, does not move the rest).
    pub fn at_end(mut self, x: Edge, y: Edge) -> Self {
        self.at = Some((x, y));
        self.ends = true;
        self
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

    /// The glyph painter for a box whose true (unsnapped) origin `true_origin` maps from its painted bounds.
    fn painter(
        &self,
        true_origin: impl Fn(Bounds<Pixels>, gpui::Size<Pixels>) -> (f32, f32) + 'static,
    ) -> impl IntoElement {
        let (a, d) = self.face.metrics();
        let (size, spacing, ends) = (self.size, self.spacing, self.ends);
        let baseline_in_box = chromium_baseline(self.face, self.size, self.line_height);
        let ascent = a * size;
        let advance = size * 0.6 + spacing;
        // Letter-spaced text is shaped one character at a time; anything else in one run.
        let pieces: Vec<(SharedString, TextRun)> = if spacing == 0.0 {
            vec![(self.text.clone(), self.run(self.text.len()))]
        } else {
            self.text
                .chars()
                .map(|c| (SharedString::from(c.to_string()), self.run(c.len_utf8())))
                .collect()
        };
        canvas(
            move |_bounds, window, _cx| {
                pieces
                    .iter()
                    .map(|(text, run)| {
                        window.text_system().shape_line(
                            text.clone(),
                            px(size),
                            std::slice::from_ref(run),
                            None,
                        )
                    })
                    .collect::<Vec<_>>()
            },
            move |bounds, shaped, window, cx| {
                let (x, top) = true_origin(bounds, window.viewport_size());
                // Chromium paints a line from its box's top rounded to a whole (logical) pixel.
                let baseline = top.round() + baseline_in_box;
                let x = if ends {
                    x - shaped.iter().map(|l| f32::from(l.width)).sum::<f32>()
                } else {
                    x
                };
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

    /// The width the line takes: the mono advance per character (plus spacing), or the face's own measure.
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
        // The mono advance gives the web's exact width (a measured box would round up to the device pixel); text with
        // characters the face may lack (arrows) is measured.
        if self.face == Face::Mono && self.text.chars().all(|c| (c as u32) < 0x2000) {
            let width = self.text.chars().count() as f32 * (self.size * 0.6 + self.spacing);
            text.w(px(width))
        } else {
            text.child(self.text.clone())
        }
    }

    /// The line with its pen at exactly (`x`, `top`) of its parent, which must sit on a whole device pixel (the message
    /// log places every line this way: it knows each line's web position).
    pub fn exact(self, x: f32, top: f32) -> AnyElement {
        let (bx, by) = ((x * SCALE).floor() / SCALE, (top * SCALE).floor() / SCALE);
        let (dx, dy) = (x - bx, top - by);
        let painter =
            self.painter(move |b, _| (f32::from(b.origin.x) + dx, f32::from(b.origin.y) + dy));
        let boxed = div()
            .absolute()
            .left(px(bx))
            .top(px(by))
            .h(px(self.line_height))
            .child(self.layout_text())
            .child(painter);
        #[cfg(feature = "automation")]
        let boxed = boxed.child(crate::automation::text_probe());
        boxed.into_any_element()
    }
}

impl IntoElement for Line {
    type Element = AnyElement;

    fn into_element(self) -> Self::Element {
        let at = self.at;
        let painter = self.painter(move |b, window| match at {
            Some((x, y)) => (
                x.resolve(f32::from(window.width)),
                y.resolve(f32::from(window.height)),
            ),
            None => (f32::from(b.origin.x), f32::from(b.origin.y)),
        });
        let boxed = div()
            .relative()
            .flex_none()
            .h(px(self.line_height))
            .child(self.layout_text())
            .child(painter);
        probe(boxed.into_any_element())
    }
}

/// Lines of `body`, one per `\n`.
pub fn lines(
    body: &str,
    size: Pixels,
    line_height: f32,
    color: Hsla,
) -> impl Iterator<Item = Line> + '_ {
    body.split('\n')
        .map(move |l| line(l.to_owned(), size, line_height, color))
}

#[cfg(feature = "automation")]
fn probe(element: AnyElement) -> AnyElement {
    div()
        .relative()
        .flex_none()
        .child(element)
        .child(crate::automation::text_probe())
        .into_any_element()
}

#[cfg(not(feature = "automation"))]
fn probe(element: AnyElement) -> AnyElement {
    element
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn baselines_match_the_measured_web_lines() {
        // Message body: 13px on 20.15px: floor((20.15 - 13 - 4) / 2) + 13.
        assert_eq!(chromium_baseline(Face::Mono, 13.0, 20.15), 14.0);
        // Gutter time: 10.5px on 15.75px: floor(0.875) + 11.
        assert_eq!(chromium_baseline(Face::Mono, 10.5, 15.75), 11.0);
        // The user panel's name: 12px on 15.6px has a negative half-leading, floored to -1.
        assert_eq!(chromium_baseline(Face::Mono, 12.0, 15.6), 11.0);
    }

    #[test]
    fn edges_resolve_against_the_window() {
        assert_eq!(Edge::Start(40.0).resolve(1280.0), 40.0);
        assert_eq!(Edge::End(108.81).resolve(1280.0), 1171.19);
        assert_eq!(Edge::Mid(-147.0).resolve(1280.0), 493.0);
    }
}
