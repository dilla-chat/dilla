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
    SharedString, Styled, TextAlign, TextRun, div, point, px, transparent_black,
};

use crate::tokens::font;

/// A font family the Signal design uses, with its vertical metrics (hhea/OS2 typo, per em).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Face {
    /// JetBrains Mono: ascent 1.02, descent 0.3, advance 0.6 em.
    Mono,
    /// DM Serif Display italic: ascent 1.036, descent 0.335.
    SerifItalic,
    /// A symbol the web's JetBrains Mono subset lacks (⇧), which Chromium draws from fontconfig's fallback face, DejaVu
    /// Sans on the reference machine, on the line's own (JetBrains Mono) baseline: the embedded JetBrains Mono has the
    /// glyph, so a fallback list would never reach it.
    Symbol,
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
            Self::Mono | Self::Symbol => (1.02, 0.3),
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
            Self::Symbol => font::SYMBOL,
        }
    }
}

/// What a test build hooks into every line: an overlay that reports the line's box (the automation interface's
/// `text-bounds`), a family that replaces every face (the parity test's fallback-font control) and `hidden`, which paints
/// every glyph transparent so a frame shows exactly the pixels the glyphs would cover (the text mask's control).
#[derive(Clone, Copy, Default)]
pub struct Hooks {
    pub probe: Option<fn() -> AnyElement>,
    pub family: Option<&'static str>,
    pub hidden: bool,
}

static HOOKS: OnceLock<Hooks> = OnceLock::new();

/// Installs the hooks, once per process (later calls are ignored). Shipping builds never call it.
pub fn set_hooks(hooks: Hooks) {
    let _ = HOOKS.set(hooks);
}

fn hooks() -> Option<&'static Hooks> {
    HOOKS.get()
}

/// Chromium's layout unit: 1/64 px, values truncated onto it toward zero (`LayoutUnit(float)`), the vendored GPUI's
/// `layout_length` rule: a whole unit off only by f32 noise stays whole, a negative value is not pushed a unit further out.
pub fn layout_unit(v: f32) -> f32 {
    let units = gpui::whole_layout_units(v).unwrap_or_else(|| (f64::from(v) * 64.0).trunc());
    (units / 64.0) as f32
}

/// A CSS `line-height` ratio at a font size, in Chromium's layout units (13px at 1.55 is 20.140625).
pub fn line_height(size: Pixels, ratio: f32) -> f32 {
    layout_unit(f32::from(size) * ratio)
}

/// The font's ascent and descent at `size`, each rounded to a whole pixel as Chromium's `SimpleFontData` does: the box an
/// inline span's background and border cover (a code span, a mention pill), and what text decorations are measured from.
pub fn ascent_descent(face: Face, size: f32) -> (f32, f32) {
    let (a, d) = face.metrics();
    ((a * size).round(), (d * size).round())
}

/// Where Chromium puts the baseline below a line box's top: the half-leading floored, then the rounded ascent.
pub fn chromium_baseline(face: Face, size: f32, line_height: f32) -> f32 {
    let (ascent, descent) = ascent_descent(face, size);
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
    fallback: Option<&'static str>,
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
        fallback: None,
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

    /// The family a glyph the face lacks comes from, where the web's Chromium takes it from fontconfig's fallback (DejaVu
    /// Sans on the reference machine) and the text system would pick another face with another advance.
    pub fn fallback(mut self, family: &'static str) -> Self {
        self.fallback = Some(family);
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

    /// The width the web gives the text: the mono advance per character (plus the tracking), ceiled onto Chromium's
    /// layout unit as its text widths are (`LayoutUnit::FromFloatCeil`: one "3" at 11px is 6.609375), when the embedded
    /// face covers every character, else `None` (measured by the text system, which falls back for the rest).
    fn natural_width(&self) -> Option<f32> {
        (self.face == Face::Mono && self.text.chars().all(mono_covers)).then(|| {
            let w = self.text.chars().count() as f32 * (self.size * MONO_ADVANCE + self.spacing);
            // The epsilon keeps a whole width whole against f32 noise (0.6 is not exact).
            (w * 64.0 - 1e-3).ceil() / 64.0
        })
    }

    fn run(&self, len: usize) -> TextRun {
        let mut font = gpui::font(self.face.family());
        font.weight = self.weight;
        if let Some(family) = self.fallback {
            font.fallbacks = Some(gpui::FontFallbacks::from_fonts(vec![family.to_owned()]));
        }
        if self.face == Face::SerifItalic {
            font.style = FontStyle::Italic;
        }
        TextRun {
            len,
            font,
            color: if hooks().is_some_and(|h| h.hidden) {
                transparent_black()
            } else {
                self.color
            },
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
        if let Some(family) = self.fallback {
            text.text_style().font_fallbacks =
                Some(gpui::FontFallbacks::from_fonts(vec![family.to_owned()]));
        }
        match self.natural_width() {
            Some(width) => text.flex_none().w(px(width)).h(px(self.line_height)),
            None => text.flex_none().child(self.text.clone()),
        }
    }
}

/// Whether a text `natural` wide fits the box it was laid out in, `laid_out` wide (before its edges snap:
/// `Window::laid_out_size`). Chromium decides text overflow in layout units, before any edge is painted: at a whole scale
/// both widths lie on the 1/64 px layout unit (the text's ceiled onto it, the box's floored), so they are compared there
/// exactly, by whole units (f32 noise below a unit never decides). At a fractional scale GPUI lays lengths out in whole
/// device px, so the box may be up to one device px off the web's: that much slack.
pub fn fits_laid_out(natural: f32, laid_out: f32, scale: f32) -> bool {
    if scale.fract() == 0.0 {
        (natural * 64.0).round() <= (laid_out * 64.0).round()
    } else {
        natural <= laid_out + 1.0 / scale
    }
}

/// The text to paint in its box: all of it, or (with an ellipsis) as many characters as fit before "…". `laid_out` is
/// the box's laid-out width at `scale` (before its edges snap), which Chromium decides overflow against
/// ([`fits_laid_out`]).
fn fitted(line: &Line, laid_out: f32, scale: f32) -> SharedString {
    let Some(natural) = line.natural_width() else {
        return line.text.clone();
    };
    if !line.ellipsis || fits_laid_out(natural, laid_out, scale) {
        return line.text.clone();
    }
    let width = laid_out;
    let advance = line.size * MONO_ADVANCE + line.spacing;
    let keep = ((width - advance) / advance).floor().max(0.0) as usize;
    let mut cut: String = line.text.chars().take(keep).collect();
    cut.push('…');
    cut.into()
}

/// The glyph painter's element: a canvas over the line's box (absolute, its full size) whose prepaint also gets the box's
/// laid-out width, before the edges snap (`Window::laid_out_size`): the width Chromium decides an ellipsis against.
struct LinePaint<T> {
    prepaint: Option<LinePrepaint<T>>,
    paint: Option<LinePainting<T>>,
}

/// [`LinePaint`]'s prepaint: the laid-out width in, the shaped glyphs out.
type LinePrepaint<T> = Box<dyn FnOnce(f32, &mut gpui::Window) -> T>;
/// [`LinePaint`]'s paint over the snapped bounds.
type LinePainting<T> = Box<dyn FnOnce(Bounds<Pixels>, T, &mut gpui::Window, &mut gpui::App)>;

impl<T: 'static> LinePaint<T> {
    fn new(
        prepaint: impl FnOnce(f32, &mut gpui::Window) -> T + 'static,
        paint: impl FnOnce(Bounds<Pixels>, T, &mut gpui::Window, &mut gpui::App) + 'static,
    ) -> Self {
        Self {
            prepaint: Some(Box::new(prepaint)),
            paint: Some(Box::new(paint)),
        }
    }
}

impl<T: 'static> IntoElement for LinePaint<T> {
    type Element = Self;

    fn into_element(self) -> Self {
        self
    }
}

impl<T: 'static> gpui::Element for LinePaint<T> {
    type RequestLayoutState = gpui::LayoutId;
    type PrepaintState = Option<T>;

    fn id(&self) -> Option<gpui::ElementId> {
        None
    }

    fn source_location(&self) -> Option<&'static core::panic::Location<'static>> {
        None
    }

    fn request_layout(
        &mut self,
        _id: Option<&gpui::GlobalElementId>,
        _inspector_id: Option<&gpui::InspectorElementId>,
        window: &mut gpui::Window,
        cx: &mut gpui::App,
    ) -> (gpui::LayoutId, gpui::LayoutId) {
        let mut style = gpui::Style {
            position: gpui::Position::Absolute,
            ..gpui::Style::default()
        };
        style.inset.top = px(0.0).into();
        style.inset.left = px(0.0).into();
        style.size.width = gpui::relative(1.0).into();
        style.size.height = gpui::relative(1.0).into();
        let id = window.request_layout(style, [], cx);
        (id, id)
    }

    fn prepaint(
        &mut self,
        _id: Option<&gpui::GlobalElementId>,
        _inspector_id: Option<&gpui::InspectorElementId>,
        _bounds: Bounds<Pixels>,
        layout: &mut gpui::LayoutId,
        window: &mut gpui::Window,
        _cx: &mut gpui::App,
    ) -> Option<T> {
        let laid_out = f32::from(window.laid_out_size(*layout).width);
        self.prepaint.take().map(|f| f(laid_out, window))
    }

    fn paint(
        &mut self,
        _id: Option<&gpui::GlobalElementId>,
        _inspector_id: Option<&gpui::InspectorElementId>,
        bounds: Bounds<Pixels>,
        _layout: &mut gpui::LayoutId,
        prepaint: &mut Option<T>,
        window: &mut gpui::Window,
        cx: &mut gpui::App,
    ) {
        if let (Some(state), Some(f)) = (prepaint.take(), self.paint.take()) {
            f(bounds, state, window, cx);
        }
    }
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
        LinePaint::new(
            move |laid_out: f32, window: &mut gpui::Window| {
                let text = fitted(&this, laid_out, window.scale_factor());
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

/// A mono paragraph that wraps at the width its parent lays it out at, as a CSS block of text with no `max-width` does
/// (a settings row's hint in a `data-measure="full"` section): [`paragraph`] needs the width before layout, this one asks
/// the layout for it. Each line sits at its own fractional top (`k * line_height`), as [`Line::exact`] places it.
pub struct FlowParagraph {
    text: SharedString,
    size: Pixels,
    line_height: f32,
    color: Hsla,
    weight: FontWeight,
}

/// [`FlowParagraph`] of `text`.
pub fn flow_paragraph(
    text: impl Into<SharedString>,
    size: Pixels,
    line_height: f32,
    color: Hsla,
) -> FlowParagraph {
    FlowParagraph {
        text: text.into(),
        size,
        line_height,
        color,
        weight: font::WEIGHT_BODY,
    }
}

impl FlowParagraph {
    pub fn weight(mut self, weight: FontWeight) -> Self {
        self.weight = weight;
        self
    }
}

/// The lines [`FlowParagraph`] breaks `text` into at `width` (`None`: the max-content width, one line per hard line).
fn flow_lines(text: &str, size: Pixels, width: Option<f32>) -> Vec<String> {
    match width {
        Some(w) => wrap_mono(text, columns(size, w)),
        None => text.split('\n').map(str::to_owned).collect(),
    }
}

/// A mono width of `chars` characters on Chromium's layout unit (as [`Line`]'s natural width).
fn mono_width(chars: usize, size: Pixels) -> f32 {
    let w = chars as f32 * f32::from(size) * MONO_ADVANCE;
    (w * 64.0 - 1e-3).ceil() / 64.0
}

/// The min-content (longest word) and max-content (longest hard line) widths of mono `text`.
fn flow_extent(text: &str, size: Pixels) -> (f32, f32) {
    let word = text
        .split([' ', '\n'])
        .map(|w| w.chars().count())
        .max()
        .unwrap_or(0);
    let line = text
        .split('\n')
        .map(|l| l.chars().count())
        .max()
        .unwrap_or(0);
    (mono_width(word, size), mono_width(line, size))
}

impl IntoElement for FlowParagraph {
    type Element = Self;

    fn into_element(self) -> Self::Element {
        self
    }
}

impl gpui::Element for FlowParagraph {
    type RequestLayoutState = ();
    type PrepaintState = Option<AnyElement>;

    fn id(&self) -> Option<gpui::ElementId> {
        None
    }

    fn source_location(&self) -> Option<&'static core::panic::Location<'static>> {
        None
    }

    fn request_layout(
        &mut self,
        _id: Option<&gpui::GlobalElementId>,
        _inspector_id: Option<&gpui::InspectorElementId>,
        window: &mut gpui::Window,
        _cx: &mut gpui::App,
    ) -> (gpui::LayoutId, Self::RequestLayoutState) {
        let (text, size, lh) = (self.text.clone(), self.size, self.line_height);
        let layout_id = window.request_measured_layout(
            gpui::Style::default(),
            move |known, available, _window, _cx| {
                // CSS sizing of a block of text: its min-content width is its longest word, its max-content width its
                // longest hard line, and in a definite space it fits: min(max-content, max(min-content, available)).
                let (min, max) = flow_extent(&text, size);
                let width = known.width.map(f32::from).unwrap_or(match available.width {
                    gpui::AvailableSpace::Definite(w) => max.min(min.max(f32::from(w))),
                    gpui::AvailableSpace::MinContent => min,
                    gpui::AvailableSpace::MaxContent => max,
                });
                let lines = flow_lines(&text, size, Some(width));
                gpui::Size {
                    width: px(width),
                    height: px(lines.len() as f32 * lh),
                }
            },
        );
        (layout_id, ())
    }

    fn prepaint(
        &mut self,
        _id: Option<&gpui::GlobalElementId>,
        _inspector_id: Option<&gpui::InspectorElementId>,
        bounds: Bounds<Pixels>,
        _request_layout: &mut Self::RequestLayoutState,
        window: &mut gpui::Window,
        cx: &mut gpui::App,
    ) -> Self::PrepaintState {
        let lh = self.line_height;
        let lines = flow_lines(&self.text, self.size, Some(f32::from(bounds.size.width)));
        let mut block = div()
            .relative()
            .w(bounds.size.width)
            .h(bounds.size.height)
            .children(lines.into_iter().enumerate().map(|(k, l)| {
                line(l, self.size, lh, self.color)
                    .weight(self.weight)
                    .exact(0.0, k as f32 * lh)
            }))
            .into_any_element();
        block.prepaint_as_root(
            bounds.origin,
            bounds.size.map(gpui::AvailableSpace::Definite),
            window,
            cx,
        );
        Some(block)
    }

    fn paint(
        &mut self,
        _id: Option<&gpui::GlobalElementId>,
        _inspector_id: Option<&gpui::InspectorElementId>,
        _bounds: Bounds<Pixels>,
        _request_layout: &mut Self::RequestLayoutState,
        prepaint: &mut Self::PrepaintState,
        window: &mut gpui::Window,
        cx: &mut gpui::App,
    ) {
        if let Some(block) = prepaint {
            block.paint(window, cx);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// A flow paragraph breaks where a fixed-width one would at the same width, and lays out on one line per hard line when
    /// it is not constrained.
    #[test]
    fn a_flow_paragraph_breaks_at_its_laid_out_width() {
        let text = "Connect the music provider so the Bot can play from it in voice channels";
        let at = |w: f32| flow_lines(text, px(11.0), Some(w));
        assert_eq!(at(396.0), wrap_mono(text, columns(px(11.0), 396.0)));
        assert_eq!(at(800.0).len(), 1);
        assert!(at(200.0).len() > 2);
        assert_eq!(flow_lines("a\nb c", px(11.0), None), ["a", "b c"]);
    }

    /// Shrunk in a flex row, a flow paragraph stops at its longest word (CSS min-content): the channel editor's "private
    /// channel" label is 52.5px ("private") at 12.5px, its max-content one line of 15 characters.
    #[test]
    fn a_flow_paragraph_shrinks_to_its_longest_word() {
        assert_eq!(flow_extent("private channel", px(12.5)), (52.5, 112.5));
        assert_eq!(
            flow_lines("private channel", px(12.5), Some(52.5)),
            ["private", "channel"]
        );
    }

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

    /// Review I2: "general" at 13px is 54.609375 wide. Laid out at x 100.5 in a box of exactly that width, its edges snap
    /// to 101 and 155 (54 painted): it still fits its laid-out box, so no "…". A box really narrower than the text cuts it.
    #[test]
    fn a_line_within_the_snap_slack_keeps_its_text() {
        let general = line("general", px(13.0), 20.0, gpui::black()).ellipsis();
        // The decision is on the laid-out width (54.609375), whatever the box paints (54).
        assert_eq!(fitted(&general, 54.609375, 2.0).as_ref(), "general");
        let cut = fitted(&general, 50.0, 2.0);
        assert!(cut.ends_with('…'), "{cut}");
        // A fractional scale keeps device-pixel lengths: one device pixel of slack.
        assert_eq!(fitted(&general, 54.0, 1.5).as_ref(), "general");
        assert!(fitted(&general, 53.5, 1.5).ends_with('…'));
    }

    /// Task 28 (T6c re-review, I2's tolerance): a text that overflows its laid-out box by less than a pixel shows "…", as
    /// the web's does; the fit is decided in layout units, exactly. f32 noise under a layout unit does not decide.
    #[test]
    fn a_line_overflowing_by_under_a_pixel_is_cut_as_chromium_cuts_it() {
        let general = line("general", px(13.0), 20.0, gpui::black()).ellipsis();
        for laid_out in [54.0, 54.09375, 54.59375] {
            let cut = fitted(&general, laid_out, 2.0);
            assert!(cut.ends_with('…'), "{laid_out}: {cut}");
        }
        assert!(fits_laid_out(54.609375, 54.609375, 2.0));
        assert!(
            fits_laid_out(54.609375 + 1e-5, 54.609375 - 1e-5, 2.0),
            "f32 noise"
        );
        assert!(
            !fits_laid_out(54.625, 54.609375, 2.0),
            "one layout unit over"
        );
        assert!(fits_laid_out(54.609375, 54.609375, 1.0));
        assert!(!fits_laid_out(54.609375, 54.59375, 1.0));
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
