//! The app's motion (packages/ui/src/styles/base.css:170-177): the two keyframes the now-playing player uses, `d-bar` (the
//! equaliser bounce) and `d-up` (an entrance that rises 8px and fades in), driven by GPUI's animation API.
//!
//! The durations are the generated `--duration-*` tokens ([`crate::tokens::motion`]). Reduced motion is GPUI's own
//! `App::reduce_motion`, which the Appearance setting and the parity fixtures set: the web zeroes the duration tokens
//! under it, so here the entrance is not drawn at all and the equaliser stands at its full height, which is what a
//! zero-duration CSS animation leaves behind. What is drawn is decided by the pure functions below ([`bar_scale`],
//! [`rise_at`]); the element helpers only hand them GPUI's clock.
//!
//! An animation asks for a frame only while it is drawn and unfinished. An entrance ends after one run (its element state
//! lives as long as the element is drawn at the same place, so a re-render does not replay it, a remount does), and the
//! equaliser exists only while the caller draws it, which is while the music plays: nothing keeps the window rendering
//! once the player is paused, gone or the window hidden (GPUI stops frames for a window the platform hides).

use std::time::Duration;

use gpui::{
    Animation, AnimationExt, AnyElement, App, BoxShadow, ElementId, Hsla, IntoElement,
    ParentElement, Pixels, SharedString, Styled, div, linear_color_stop, linear_gradient, point,
    px, relative,
};

use crate::icon::Icon;
use crate::tokens::{color, motion};

/// CSS's default timing function, `ease`: `cubic-bezier(0.25, 0.1, 0.25, 1)`, what an animation without a
/// `animation-timing-function` uses between its keyframes.
pub const EASE: [f32; 4] = [0.25, 0.1, 0.25, 1.0];

/// `d-bar`'s rest scale (`scaleY(0.2)`, at 0% and 100%); it peaks at 1 at 50%.
const BAR_LOW: f32 = 0.2;

/// `d-up`'s start offset (`translateY(8px)`).
const RISE_PX: f32 = 8.0;

/// The value of the CSS curve `cubic-bezier(x1, y1, x2, y2)` at time `x` (0 to 1): the curve's parameter is found for `x`
/// by bisection, and its `y` there is the answer.
pub fn cubic_bezier([x1, y1, x2, y2]: [f32; 4], x: f32) -> f32 {
    let x = x.clamp(0.0, 1.0);
    let at = |a: f32, b: f32, s: f32| {
        3.0 * a * s * (1.0 - s) * (1.0 - s) + 3.0 * b * s * s * (1.0 - s) + s * s * s
    };
    let (mut lo, mut hi) = (0.0_f32, 1.0_f32);
    for _ in 0..32 {
        let mid = (lo + hi) / 2.0;
        if at(x1, x2, mid) < x {
            lo = mid;
        } else {
            hi = mid;
        }
    }
    at(y1, y2, (lo + hi) / 2.0)
}

/// The scale of equaliser bar `bar` (0, 1 or 2) `elapsed` after the bars appeared, for a beat of `beat`
/// (NowPlaying.css:12-14): `d-bar` rises from 0.2 to 1 over the first half of the beat and falls back over the second,
/// each half on the `ease` curve; the second bar starts beat/4 late and the third beat/2. A bar's wait before its first
/// beat is drawn as the phase it would have, not at rest height as CSS draws it (a quarter beat at most, once).
pub fn bar_scale(elapsed: Duration, beat: Duration, bar: usize) -> f32 {
    if beat.is_zero() {
        return 1.0;
    }
    let delay = bar as f32 / 4.0;
    bar_phase_scale((elapsed.as_secs_f32() / beat.as_secs_f32() - delay).rem_euclid(1.0))
}

/// The scale of a bar at `phase` (0 to 1) of its beat.
fn bar_phase_scale(phase: f32) -> f32 {
    let (from, to, t) = if phase < 0.5 {
        (BAR_LOW, 1.0, phase * 2.0)
    } else {
        (1.0, BAR_LOW, (phase - 0.5) * 2.0)
    };
    from + (to - from) * cubic_bezier(EASE, t)
}

/// `d-up` `elapsed` into its run of `run` on `curve`: the opacity and the downward offset in px (8px at the start, none at
/// the end). A run of zero is over at once.
pub fn rise_at(elapsed: Duration, run: Duration, curve: [f32; 4]) -> (f32, f32) {
    if run.is_zero() {
        return (1.0, 0.0);
    }
    let p = cubic_bezier(curve, elapsed.as_secs_f32() / run.as_secs_f32());
    (p, RISE_PX * (1.0 - p))
}

/// `d-sweep` (`from { translateX(-100%) } to { translateX(100%) }`, linear, infinite): the strip's offset as a fraction of
/// its own width `elapsed` into the run of `run`, from -1 to 1. A run of zero stands still.
pub fn sweep_at(elapsed: Duration, run: Duration) -> f32 {
    if run.is_zero() {
        return 0.0;
    }
    -1.0 + 2.0 * (elapsed.as_secs_f32() / run.as_secs_f32()).rem_euclid(1.0)
}

/// `d-ring` (`from { opacity: 0.6; scale(1) } to { opacity: 0; scale(1.5) }`, infinite): the wave's opacity and scale
/// `elapsed` into a run of `run` on `curve` (RingBanner.css names `--ease-out`). Each run starts again from the first
/// keyframe. A run of zero is no animation: the full ring, unscaled.
pub fn ring_at(elapsed: Duration, run: Duration, curve: [f32; 4]) -> (f32, f32) {
    if run.is_zero() {
        return (1.0, 1.0);
    }
    let p = cubic_bezier(
        curve,
        (elapsed.as_secs_f32() / run.as_secs_f32()).rem_euclid(1.0),
    );
    (0.6 * (1.0 - p), 1.0 + 0.5 * p)
}

/// `d-spin` (`to { transform: rotate(360deg) }`, linear, infinite): the turn in radians `elapsed` into a run of `run`.
/// A run of zero stands upright.
pub fn spin_at(elapsed: Duration, run: Duration) -> f32 {
    if run.is_zero() {
        return 0.0;
    }
    std::f32::consts::TAU * (elapsed.as_secs_f32() / run.as_secs_f32()).rem_euclid(1.0)
}

/// The CSS keyword `ease-out`: `cubic-bezier(0, 0, 0.58, 1)` (not the `--ease-out` token).
pub const CSS_EASE_OUT: [f32; 4] = [0.0, 0.0, 0.58, 1.0];

/// `d-row-flash` (`0%, 30% { background: var(--accent-flash) } 100% { background: transparent }`, on the keyword
/// `ease-out`, once): how much of the flash tint is left `elapsed` into a run of `run`, 1 to 0. It holds for 30% of the
/// run, then fades. A run of zero has no flash.
pub fn flash_at(elapsed: Duration, run: Duration) -> f32 {
    if run.is_zero() {
        return 0.0;
    }
    let t = elapsed.as_secs_f32() / run.as_secs_f32();
    if t <= 0.3 {
        1.0
    } else {
        1.0 - cubic_bezier(CSS_EASE_OUT, (t - 0.3) / 0.7)
    }
}

/// How often the endless indicators and the flash repaint while they are drawn: a sweep, a spinner or a wave reads as
/// smooth at 30 frames a second, like the equaliser.
pub const INDICATOR_FPS: f32 = 30.0;

/// How often the equaliser repaints while it bounces: the 520ms beat reads as smooth at 30 frames a second, half the
/// cost of a repaint on every display frame.
pub const EQUALISER_FPS: f32 = 30.0;

/// The three `--ok` equaliser bars (`.d-np__bars`), `height` px tall and 2px wide, 1.5px apart, bouncing from their bottom
/// on `--duration-beat`. One animation drives the three, so one timer repaints them together, at [`EQUALISER_FPS`]. Draw
/// them only while the music plays: a drawn equaliser keeps asking for frames, an undrawn one costs nothing. With reduced
/// motion they stand at full height.
pub fn equaliser(id: impl Into<ElementId>, height: f32, cx: &App) -> AnyElement {
    let bars = move |scale: [f32; 3]| {
        div()
            .flex()
            .flex_none()
            .items_end()
            .gap(px(1.5))
            .h(px(height))
            .children(scale.map(|s| div().w(px(2.0)).h(px(height * s)).bg(color::OK)))
    };
    if cx.reduce_motion() {
        return bars([1.0; 3]).into_any_element();
    }
    let beat = motion::BEAT;
    bars([1.0; 3])
        .with_animation(
            id,
            Animation::new(beat).repeat().with_max_fps(EQUALISER_FPS),
            move |_, delta| bars([0, 1, 2].map(|i| bar_scale(beat.mul_f32(delta), beat, i))),
        )
        .into_any_element()
}

/// `el` entering with `d-up` over `run` on `curve` (`EASE` unless the CSS names `--ease-out`), once for as long as it stays
/// drawn at `id`'s place: the element fades in while it rises into position (its layout does not move). With reduced motion
/// it is drawn as it is.
pub fn rise_in<E>(
    el: E,
    id: impl Into<ElementId>,
    run: Duration,
    curve: [f32; 4],
    cx: &App,
) -> AnyElement
where
    E: Styled + IntoElement + 'static,
{
    if cx.reduce_motion() {
        return el.into_any_element();
    }
    el.with_animation(id, Animation::new(run), move |el, delta| {
        let (opacity, offset) = rise_at(run.mul_f32(delta), run, curve);
        // The finished entrance is the element itself, with nothing laid over it.
        if delta >= 1.0 {
            el
        } else {
            el.relative().top(px(offset)).opacity(opacity)
        }
    })
    .into_any_element()
}

/// The composer's typing sweep (`.d-composer__sweep`, Composer.css:13-15): a 2px strip on the box's top edge, over which a
/// gradient from clear through `--accent` to clear slides from one side to the other on `--duration-flash`, linear. Draw
/// it only while someone types: a drawn sweep keeps asking for frames, at [`INDICATOR_FPS`]; with reduced motion the
/// gradient stands still, as a zero-duration CSS animation leaves it.
pub fn sweep(id: impl Into<ElementId>, cx: &App) -> AnyElement {
    if cx.reduce_motion() {
        return sweep_still();
    }
    let run = motion::FLASH;
    sweep_strip(-1.0)
        .with_animation(
            id,
            Animation::new(run).repeat().with_max_fps(INDICATOR_FPS),
            move |_, delta| sweep_strip(sweep_at(run.mul_f32(delta), run)),
        )
        .into_any_element()
}

/// The sweep's strip with its gradient `offset` of its own width along (-1 to 1).
fn sweep_strip(offset: f32) -> gpui::Div {
    let clear: Hsla = color::ACCENT.opacity(0.0);
    let band = div()
        .absolute()
        .top(px(0.0))
        .left(relative(offset))
        .w_full()
        .h_full()
        .flex()
        .child(div().w(relative(0.5)).h_full().bg(linear_gradient(
            90.0,
            linear_color_stop(clear, 0.0),
            linear_color_stop(color::ACCENT, 1.0),
        )))
        .child(div().w(relative(0.5)).h_full().bg(linear_gradient(
            90.0,
            linear_color_stop(color::ACCENT, 0.0),
            linear_color_stop(clear, 1.0),
        )));
    div()
        .relative()
        .w_full()
        .h(px(2.0))
        .overflow_hidden()
        .child(band)
}

/// The sweep's strip at rest, its gradient centred: what the web's strip shows once its animation is removed (the reduced
/// form, and the strip as it fades out when typing stops).
pub fn sweep_still() -> AnyElement {
    sweep_strip(0.0).into_any_element()
}

/// A CSS `transition` of opacity on the default curve (`ease`), `elapsed` into its run of `run`: 0 to 1. A run of zero is
/// over at once.
pub fn transition_at(elapsed: Duration, run: Duration) -> f32 {
    if run.is_zero() {
        return 1.0;
    }
    cubic_bezier(EASE, elapsed.as_secs_f32() / run.as_secs_f32())
}

/// `el` fading in as an `opacity: 0` to `1` transition over `run` does (`.d-composer__sweep` on `--duration-turn`,
/// `.d-call-tile__meter` on `--duration-fast`), once for as long as it stays drawn at `id`'s place. Reduced motion zeroes
/// the durations: drawn as it is.
pub fn transition_in<E>(el: E, id: impl Into<ElementId>, run: Duration, cx: &App) -> AnyElement
where
    E: Styled + IntoElement + 'static,
{
    if cx.reduce_motion() {
        return el.into_any_element();
    }
    el.with_animation(
        id,
        Animation::new(run).with_max_fps(INDICATOR_FPS),
        move |el, delta| {
            if delta >= 1.0 {
                el
            } else {
                el.opacity(transition_at(run.mul_f32(delta), run))
            }
        },
    )
    .into_any_element()
}

/// `el` fading out as an `opacity: 1` to `0` transition over `run` does, once, and then held clear: the caller draws it for
/// `run` after the thing it shows has gone, then stops drawing it. With reduced motion it is not drawn at all.
pub fn transition_out<E>(el: E, id: impl Into<ElementId>, run: Duration, cx: &App) -> AnyElement
where
    E: Styled + IntoElement + 'static,
{
    if cx.reduce_motion() {
        return div().into_any_element();
    }
    el.with_animation(
        id,
        Animation::new(run).with_max_fps(INDICATOR_FPS),
        move |el, delta| el.opacity(1.0 - transition_at(run.mul_f32(delta), run)),
    )
    .into_any_element()
}

/// An `icon` turning as `d-spin` does (`--duration-result-spin`, linear, infinite). Draw it only while the work is pending:
/// a drawn spinner asks for frames at [`INDICATOR_FPS`]. With reduced motion it is upright.
pub fn spinning(icon: Icon, id: impl Into<ElementId>, cx: &App) -> AnyElement {
    if cx.reduce_motion() {
        return icon.into_any_element();
    }
    let run = motion::RESULT_SPIN;
    let turned = icon.clone();
    icon.with_animation(
        id,
        Animation::new(run).repeat().with_max_fps(INDICATOR_FPS),
        move |_, delta| turned.clone().rotate(spin_at(run.mul_f32(delta), run)),
    )
    .into_any_element()
}

/// The incoming-ring wave (`.d-ring-banner__glyph::after`, RingBanner.css:8-10): a `--ok` ring 2px wide round a box of
/// `size` px with corners of `radius`, drawn over its positioned parent's whole box, that grows to 1.5 times its size while
/// it fades from 0.6 to nothing, on `--duration-pulse` and `--ease-out`, over and over. Draw it only while the call is
/// incoming. With reduced motion there is no wave: the ring stands as a plain 2px `--ok` edge.
pub fn ring_wave(id: impl Into<ElementId>, size: f32, radius: Pixels, cx: &App) -> AnyElement {
    const SPREAD: f32 = 2.0;
    let ring = move |opacity: f32, scale: f32| {
        let grown = size * scale;
        let inset = -(grown - size) / 2.0;
        div()
            .absolute()
            .top(px(inset))
            .left(px(inset))
            .size(px(grown))
            .rounded(radius * scale)
            .shadow(vec![BoxShadow {
                color: color::OK.opacity(opacity),
                offset: point(px(0.0), px(0.0)),
                blur_radius: px(0.0),
                spread_radius: px(SPREAD * scale),
                inset: false,
            }])
    };
    if cx.reduce_motion() {
        return ring(1.0, 1.0).into_any_element();
    }
    let run = motion::PULSE;
    ring(0.6, 1.0)
        .with_animation(
            id,
            Animation::new(run).repeat().with_max_fps(INDICATOR_FPS),
            move |_, delta| {
                let (opacity, scale) = ring_at(run.mul_f32(delta), run, motion::EASE_OUT);
                ring(opacity, scale)
            },
        )
        .into_any_element()
}

/// `el` flashing the accent tint as a jump-to row does (`d-row-flash` on `--duration-flash`, once): the tint is
/// `--accent-flash` for 30% of the run, then fades to clear, and the row is as it was after. The tint is a layer of its
/// own, the element's first child (call this before adding the others, so the content draws over it, as it does over a
/// background), so the element's own id path and its children's state do not change when the flash starts and ends. It
/// stands in for the row's background while it runs: the caller gives a flashing row no fill of its own and no hover style,
/// as a CSS animation beats `:hover` and the row's own background. It runs once for as long as the layer stays drawn at
/// `id`'s place, so give a new jump a new id. With reduced motion (MessageRow.css's own `prefers-reduced-motion` form) the
/// element takes the tint and an inset 2px `--accent` edge on its left, which replace its shadows as the CSS's
/// `box-shadow` does; the caller stops flashing when [`motion::FLASH`] has passed.
pub fn flash<E>(el: E, id: impl Into<ElementId>, cx: &App) -> E
where
    E: Styled + ParentElement,
{
    if cx.reduce_motion() {
        return el.bg(color::ACCENT_FLASH).shadow(vec![BoxShadow {
            color: color::ACCENT,
            offset: point(px(2.0), px(0.0)),
            blur_radius: px(0.0),
            spread_radius: px(0.0),
            inset: true,
        }]);
    }
    let run = motion::FLASH;
    let layer = |alpha: f32| {
        div()
            .absolute()
            .top(px(0.0))
            .left(px(0.0))
            .size_full()
            .bg(color::ACCENT_FLASH.opacity(alpha))
    };
    el.child(layer(1.0).with_animation(
        id,
        Animation::new(run).with_max_fps(INDICATOR_FPS),
        move |_, delta| layer(flash_at(run.mul_f32(delta), run)),
    ))
}

/// `d-modal`'s start offset (`translateY(-8px)`) and scale (`scale(0.985)`).
const MODAL_PX: f32 = 8.0;
const MODAL_SCALE: f32 = 0.985;

/// `d-toast`'s start offset (`translateY(10px)`) and scale (`scale(0.96)`).
const TOAST_PX: f32 = 10.0;
const TOAST_SCALE: f32 = 0.96;

/// `d-pop`'s keyframes: `scale(0.3)` at 0%, `scale(1.2)` at 60%, `scale(1)` at 100%.
const POP_FROM: f32 = 0.3;
const POP_PEAK: f32 = 1.2;
const POP_PEAK_AT: f32 = 0.6;

/// What an entrance that moves, scales and fades shows at one moment: its opacity, its offset down from its place in px
/// (negative is up) and the scale CSS would draw it at.
#[derive(Clone, Copy, Debug, PartialEq)]
pub struct Entrance {
    pub opacity: f32,
    pub offset_y: f32,
    /// What the keyframe scales by. GPUI cannot scale an element and what is in it, so [`modal_in`] and [`toast_in`] do not
    /// draw it (it is here so the keyframe is whole); [`pop_in`] draws its own scale by redrawing at it.
    pub scale: f32,
}

impl Entrance {
    /// The end state: in place, opaque, full size. What a zero-duration animation leaves.
    pub const SETTLED: Self = Self {
        opacity: 1.0,
        offset_y: 0.0,
        scale: 1.0,
    };
}

fn progress(elapsed: Duration, run: Duration, curve: [f32; 4]) -> f32 {
    cubic_bezier(curve, elapsed.as_secs_f32() / run.as_secs_f32())
}

/// `d-modal` (`from { opacity: 0; translateY(-8px) scale(0.985) } to { opacity: 1; none }`) `elapsed` into its run of `run` on
/// `curve` (the components all name `--ease-out`). A run of zero is over at once.
pub fn modal_at(elapsed: Duration, run: Duration, curve: [f32; 4]) -> Entrance {
    if run.is_zero() {
        return Entrance::SETTLED;
    }
    let p = progress(elapsed, run, curve);
    Entrance {
        opacity: p,
        offset_y: -MODAL_PX * (1.0 - p),
        scale: MODAL_SCALE + (1.0 - MODAL_SCALE) * p,
    }
}

/// `d-toast` (`from { opacity: 0; translateY(10px) scale(0.96) } to { opacity: 1; none }`), as [`modal_at`].
pub fn toast_at(elapsed: Duration, run: Duration, curve: [f32; 4]) -> Entrance {
    if run.is_zero() {
        return Entrance::SETTLED;
    }
    let p = progress(elapsed, run, curve);
    Entrance {
        opacity: p,
        offset_y: TOAST_PX * (1.0 - p),
        scale: TOAST_SCALE + (1.0 - TOAST_SCALE) * p,
    }
}

/// `d-fade` (`from { opacity: 0 } to { opacity: 1 }`): the opacity `elapsed` into `run` on `curve` (`EASE` where the CSS
/// names none, as it does for the scrims, the lightbox and the tooltip). A run of zero is over at once.
pub fn fade_at(elapsed: Duration, run: Duration, curve: [f32; 4]) -> f32 {
    if run.is_zero() {
        return 1.0;
    }
    progress(elapsed, run, curve)
}

/// `d-pop` (`0% { scale(0.3) } 60% { scale(1.2) } 100% { scale(1) }`): the scale `elapsed` into `run`. The curve runs
/// between each pair of keyframes, as CSS's `animation-timing-function` does. A run of zero is over at once.
pub fn pop_at(elapsed: Duration, run: Duration, curve: [f32; 4]) -> f32 {
    if run.is_zero() {
        return 1.0;
    }
    let t = (elapsed.as_secs_f32() / run.as_secs_f32()).clamp(0.0, 1.0);
    if t < POP_PEAK_AT {
        POP_FROM + (POP_PEAK - POP_FROM) * cubic_bezier(curve, t / POP_PEAK_AT)
    } else {
        POP_PEAK + (1.0 - POP_PEAK) * cubic_bezier(curve, (t - POP_PEAK_AT) / (1.0 - POP_PEAK_AT))
    }
}

/// `el` entering by `at`'s keyframe over `run` on `--ease-out`, once for as long as it stays drawn at `id`'s place: it
/// fades in while it moves from its offset into position (its layout does not move). `el` must be in the flow of its
/// parent (not `.absolute()`: put the entrance on the box inside an absolute wrapper). With reduced motion it is drawn as
/// it is.
fn enter<E>(
    el: E,
    id: impl Into<ElementId>,
    run: Duration,
    cx: &App,
    at: fn(Duration, Duration, [f32; 4]) -> Entrance,
) -> AnyElement
where
    E: Styled + IntoElement + 'static,
{
    if cx.reduce_motion() {
        return el.into_any_element();
    }
    el.with_animation(id, Animation::new(run), move |el, delta| {
        // Always positioned, so what is inside it is laid out the same during the run and after it; the finished entrance
        // has nothing else laid over it.
        let el = el.relative();
        if delta >= 1.0 {
            return el;
        }
        let now = at(run.mul_f32(delta), run, motion::EASE_OUT);
        el.top(px(now.offset_y)).opacity(now.opacity)
    })
    .into_any_element()
}

/// `el` entering with `d-modal` over `run` on `--ease-out`, once per mount (see [`enter`]). Draws the fade and the 8px
/// settling from above; the keyframe's `scale(0.985)` is not drawn, because GPUI cannot scale an element and what is in it
/// (0.985 of a 300px menu is 2px a side).
pub fn modal_in<E>(el: E, id: impl Into<ElementId>, run: Duration, cx: &App) -> AnyElement
where
    E: Styled + IntoElement + 'static,
{
    enter(el, id, run, cx, modal_at)
}

/// [`modal_in`], or `el` as it is when `still` (an entrance the web switches off for this element).
pub fn modal_in_unless<E>(
    el: E,
    id: impl Into<ElementId>,
    run: Duration,
    still: bool,
    cx: &App,
) -> AnyElement
where
    E: Styled + IntoElement + 'static,
{
    if still {
        el.into_any_element()
    } else {
        modal_in(el, id, run, cx)
    }
}

/// `el` entering with `d-toast` over [`motion::TOAST`] on `--ease-out`, once per mount (see [`enter`]). The `scale(0.96)` is
/// not drawn, as with [`modal_in`].
pub fn toast_in<E>(el: E, id: impl Into<ElementId>, cx: &App) -> AnyElement
where
    E: Styled + IntoElement + 'static,
{
    enter(el, id, motion::TOAST, cx, toast_at)
}

/// `el` fading in with `d-fade` over `run` on `curve`, once per mount. The whole element fades, what is in it too.
pub fn fade_in<E>(
    el: E,
    id: impl Into<ElementId>,
    run: Duration,
    curve: [f32; 4],
    cx: &App,
) -> AnyElement
where
    E: Styled + IntoElement + 'static,
{
    if cx.reduce_motion() {
        return el.into_any_element();
    }
    el.with_animation(id, Animation::new(run), move |el, delta| {
        if delta >= 1.0 {
            el
        } else {
            el.opacity(fade_at(run.mul_f32(delta), run, curve))
        }
    })
    .into_any_element()
}

/// A backdrop of `scrim` covering its positioned parent, fading in with `d-fade` over [`motion::SCRIM`] (`--duration-scrim`,
/// the CSS's default curve, `both`). It is a layer of its own, the parent's first child, so the panel above it does not
/// fade with it, as the web's `::backdrop` is not the dialog.
pub fn scrim_layer(id: impl Into<ElementId>, scrim: Hsla, cx: &App) -> AnyElement {
    let layer = move |alpha: f32| div().absolute().inset_0().bg(scrim.opacity(alpha));
    if cx.reduce_motion() {
        return layer(1.0).into_any_element();
    }
    let run = motion::SCRIM;
    layer(0.0)
        .with_animation(id, Animation::new(run), move |_, delta| {
            layer(fade_at(run.mul_f32(delta), run, EASE))
        })
        .into_any_element()
}

/// Which of a popping badge's drawings [`pop_in`] and [`pop_frame`] ask for.
#[derive(Clone, Copy, Debug, PartialEq)]
pub enum Pop {
    /// The badge at rest, full size: the only drawing when it is not popping.
    Rest,
    /// The full-size copy that keeps the badge's place in the layout while it pops. Hidden, and **inert**: no focus handle,
    /// action, tab stop or id of the badge, because a hidden element is not painted and so is neither focused nor hit.
    Layout,
    /// The badge `scale` times its size, drawn about its centre: the **real** one while it pops, as the web's scaled element
    /// is the real button (its focus handle, tab stop, actions and id).
    Scaled(f32),
}

impl Pop {
    /// What the drawing multiplies every length and its text by.
    pub fn scale(self) -> f32 {
        match self {
            Self::Scaled(s) => s,
            Self::Rest | Self::Layout => 1.0,
        }
    }

    /// Whether the drawing is the badge itself (it takes the focus, the tab stop, the actions and the id), as opposed to the
    /// hidden copy that only holds the layout.
    pub fn live(self) -> bool {
        self != Self::Layout
    }
}

/// A badge `scale` times its size about its centre, over a hidden, inert full-size copy that keeps its place in the layout
/// (the CSS transform leaves the layout alone): the frame of a pop whose clock the caller keeps. `build` draws the badge as
/// [`Pop`] says. `None` is the badge at rest, and nothing else.
pub fn pop_frame(scale: Option<f32>, build: &dyn Fn(Pop) -> AnyElement) -> gpui::Div {
    let slot = div().relative().flex().flex_none();
    match scale {
        None => slot.child(build(Pop::Rest)),
        Some(scale) => slot
            .child(
                div()
                    .flex()
                    .flex_none()
                    .invisible()
                    .child(build(Pop::Layout)),
            )
            .child(
                div()
                    .absolute()
                    .inset_0()
                    .flex()
                    .items_center()
                    .justify_center()
                    .child(build(Pop::Scaled(scale))),
            ),
    }
}

/// A badge popping in with `d-pop` over `run` on `curve`, once per mount. `build` draws the badge as [`Pop`] says (at its
/// size, as the hidden layout copy, or scaled: every length and its text multiplied by the scale about its centre, GPUI
/// cannot scale an element). A finished pop is the badge at rest and nothing else; with reduced motion it is drawn so at once.
pub fn pop_in(
    id: impl Into<ElementId>,
    run: Duration,
    curve: [f32; 4],
    cx: &App,
    build: impl Fn(Pop) -> AnyElement + 'static,
) -> AnyElement {
    // The same slot holds the badge at rest, in motion and under reduced motion, so the stills exercise what runs. There is
    // no frame cap: a cap is a timer per badge, and N badges popping at once would notify their view N times a window.
    if cx.reduce_motion() {
        return pop_frame(None, &build).into_any_element();
    }
    pop_frame(Some(POP_FROM), &build)
        .with_animation(id, Animation::new(run), move |_, delta| {
            if delta >= 1.0 {
                pop_frame(None, &build)
            } else {
                pop_frame(Some(pop_at(run.mul_f32(delta), run, curve)), &build)
            }
        })
        .into_any_element()
}

// Task 36c: the entrances that rise and fade in as the web's do (base.css:170-181 and the components' own keyframes).

/// `d-log-rise`'s start offset (`translateY(6px)`, MessageLog.css:8).
const LOG_RISE_PX: f32 = 6.0;

/// What an `animation-delay` leaves of `elapsed`: nothing until the delay is over (the keyframe's first frame stands, as a
/// `backwards` or `both` fill draws it), then the time since.
pub fn after(elapsed: Duration, delay: Duration) -> Duration {
    elapsed.saturating_sub(delay)
}

/// `d-up` with an `animation-delay` of `delay` (the message rows' `--d-enter-i` stagger, the call stage's tiles).
pub fn rise_after_at(
    elapsed: Duration,
    delay: Duration,
    run: Duration,
    curve: [f32; 4],
) -> (f32, f32) {
    rise_at(after(elapsed, delay), run, curve)
}

/// `d-invite-up` (InvitePanel.css:35, `from { opacity: 0; translateY(8px) }`): the same fade and 8px rise as `d-up`.
pub fn invite_up_at(elapsed: Duration, run: Duration, curve: [f32; 4]) -> (f32, f32) {
    rise_at(elapsed, run, curve)
}

/// `d-poll-card-entry` (PollCard.css:34) and `d-poll-answer-entry` (PollDialog.css:58): `opacity: 0; translateY(8px)` to
/// none, on `ease` (the CSS names no other curve).
pub fn poll_entry_at(elapsed: Duration, run: Duration) -> (f32, f32) {
    rise_at(elapsed, run, EASE)
}

/// `d-poll-voter-pop` (PollCard.css:35, `scale(.3)` to 1.2 at 60% to 1): `d-pop`'s keyframes, on `ease`.
pub fn poll_voter_pop_at(elapsed: Duration, run: Duration) -> f32 {
    pop_at(elapsed, run, EASE)
}

/// `d-log-fade` (MessageLog.css:7, `from { opacity: 0 }`, `both`, the default curve) `elapsed` into its run.
pub fn log_fade_at(elapsed: Duration, run: Duration) -> f32 {
    fade_at(elapsed, run, EASE)
}

/// `d-log-rise` (MessageLog.css:8, `from { translateY(6px) }`, `--ease-out`): the downward offset in px `elapsed` into its
/// run. A run of zero is over at once.
pub fn log_rise_at(elapsed: Duration, run: Duration, curve: [f32; 4]) -> f32 {
    if run.is_zero() {
        return 0.0;
    }
    LOG_RISE_PX * (1.0 - cubic_bezier(curve, elapsed.as_secs_f32() / run.as_secs_f32()))
}

/// `d-splash-in` (Splash.css:6, `from { opacity: 0 }`, `--ease-out`, no fill): the opacity `elapsed` into its run.
pub fn splash_in_at(elapsed: Duration, run: Duration) -> f32 {
    fade_at(elapsed, run, motion::EASE_OUT)
}

/// `el` entering with `d-up` after `delay` and over `run` on `curve`, once for as long as it stays drawn at `id`'s place
/// (the element fades in while it rises into position; its layout does not move). `el` must be in the flow of its parent
/// (not `.absolute()`). The delay holds the first frame. With reduced motion it is drawn as it is.
pub fn up_in<E>(
    el: E,
    id: impl Into<ElementId>,
    delay: Duration,
    run: Duration,
    curve: [f32; 4],
    cx: &App,
) -> AnyElement
where
    E: Styled + IntoElement + 'static,
{
    if cx.reduce_motion() {
        return el.into_any_element();
    }
    let total = delay + run;
    el.with_animation(id, Animation::new(total), move |el, delta| {
        if delta >= 1.0 {
            el
        } else {
            let (opacity, offset) = rise_after_at(total.mul_f32(delta), delay, run, curve);
            el.relative().top(px(offset)).opacity(opacity)
        }
    })
    .into_any_element()
}

/// Draws `el` through the fade-and-rise `at` gives over `run`, once per mount (the shape the keyframes above share).
fn rise_with<E>(
    el: E,
    id: impl Into<ElementId>,
    run: Duration,
    cx: &App,
    at: impl Fn(Duration) -> (f32, f32) + 'static,
) -> AnyElement
where
    E: Styled + IntoElement + 'static,
{
    if cx.reduce_motion() {
        return el.into_any_element();
    }
    el.with_animation(id, Animation::new(run), move |el, delta| {
        if delta >= 1.0 {
            el
        } else {
            let (opacity, offset) = at(run.mul_f32(delta));
            el.relative().top(px(offset)).opacity(opacity)
        }
    })
    .into_any_element()
}

/// The entrances of things that are drawn inside a list that is not drawn whole (a poll card in a message row), by a key the
/// caller makes of the thing's identity: remembered, so that a row leaving the drawn range and coming back plays nothing
/// again. Every draw of a key stamps it; past [`REMEMBERED_MAX`] keys the least recently drawn are dropped (those of rows
/// long off screen), never the ones on screen, so no visible card enters again for want of room.
#[derive(Default)]
pub struct Remembered {
    keys: std::collections::HashMap<String, (std::time::Instant, u64)>,
    tick: u64,
}
impl gpui::Global for Remembered {}

/// How many keys [`Remembered`] keeps before it drops the least recently drawn.
pub const REMEMBERED_MAX: usize = 4096;

/// How many of the least recently drawn keys go at once when the map is full.
const REMEMBERED_EVICT: usize = REMEMBERED_MAX / 8;

impl Remembered {
    /// `key` is drawn at `now`: how long it has been entering. A new key enters now.
    pub fn touch(&mut self, key: String, now: std::time::Instant) -> Duration {
        self.tick += 1;
        let tick = self.tick;
        if !self.keys.contains_key(&key) && self.keys.len() >= REMEMBERED_MAX {
            let mut stamps: Vec<u64> = self.keys.values().map(|(_, t)| *t).collect();
            stamps.sort_unstable();
            let below = stamps[REMEMBERED_EVICT.min(stamps.len()) - 1];
            self.keys.retain(|_, (_, t)| *t > below);
        }
        let entry = self.keys.entry(key).or_insert((now, tick));
        entry.1 = tick;
        now.saturating_duration_since(entry.0)
    }

    /// Forgets every key that starts with `prefix` and is not `keep`: the thing it stood for is gone (a reaction removed, a
    /// vote taken back), so it enters again if it returns.
    pub fn forget(&mut self, prefix: &str, keep: impl Fn(&str) -> bool) {
        self.keys.retain(|k, _| !k.starts_with(prefix) || keep(k));
    }

    /// Forgets everything.
    pub fn clear(&mut self) {
        self.keys.clear();
    }

    /// Forgets the keys of one log ([`scoped`]): that log mounted anew, and what lives in its rows enters again. Another
    /// log's keys stay.
    pub fn clear_scope(&mut self, scope: &str) {
        let prefix = scoped(scope, "");
        self.keys.retain(|k, _| !k.starts_with(&prefix));
    }

    /// How many keys are remembered.
    pub fn len(&self) -> usize {
        self.keys.len()
    }

    /// Whether no key is remembered.
    pub fn is_empty(&self) -> bool {
        self.keys.is_empty()
    }
}

/// `key` is drawn now: how long its entrance (of `run`) has been running, `None` when it is over or motion is reduced. The
/// first draw starts it. The caller asks for another frame while this answers `Some`.
pub fn entered_age(cx: &mut App, key: String, run: Duration) -> Option<Duration> {
    let now = cx.background_executor().now();
    let reduced = cx.reduce_motion();
    let remembered = cx.default_global::<Remembered>();
    let age = if reduced {
        // Taken in as already over, so that turning motion on plays nothing back.
        remembered.touch(
            key,
            now.checked_sub(Duration::from_secs(3600)).unwrap_or(now),
        )
    } else {
        remembered.touch(key, now)
    };
    (!reduced && age < run).then_some(age)
}

/// The things drawn under `prefix` that are still there are `keep`: the rest are forgotten (see [`Remembered::forget`]).
pub fn forget_entered(cx: &mut App, prefix: &str, keep: impl Fn(&str) -> bool) {
    cx.default_global::<Remembered>().forget(prefix, keep);
}

/// A key of [`entered_age`] in the log `scope` names (the channel's log, a thread's, each in its window): the logs are
/// mounted and unmounted apart, so each has its own keys.
pub fn scoped(scope: &str, key: &str) -> String {
    format!("{scope}\u{1}{key}")
}

/// The scope of the log `log` (the channel's log, a thread's) in `window`: each window's logs are their own, as each web
/// tab's DOM is, so a log mounting in one window never replays what another window's rows show.
pub fn window_scope(window: gpui::WindowId, log: &str) -> SharedString {
    format!("{log}#{}", window.as_u64()).into()
}

/// The log being drawn (see [`set_scope`]).
#[derive(Default)]
struct LogScope(SharedString);
impl gpui::Global for LogScope {}

/// The log `log` of `window` starts drawing its rows: what they key their entrances by (file cards, reaction chips, poll
/// cards) is that log's in that window ([`window_scope`]), so that its mount or unmount forgets its own and no other log's.
/// The log calls [`end_scope`] once its rows are built.
pub fn set_scope(cx: &mut App, window: gpui::WindowId, log: &str) {
    cx.set_global(LogScope(window_scope(window, log)));
}

/// The log's rows are built: what is drawn after them (outside any log) takes no log's scope.
pub fn end_scope(cx: &mut App) {
    cx.set_global(LogScope::default());
}

/// The scope of the log whose rows are being built ([`set_scope`]); empty outside one.
pub fn scope(cx: &App) -> SharedString {
    cx.try_global::<LogScope>()
        .map_or_else(SharedString::default, |s| s.0.clone())
}

/// The log `log` of `window` mounted anew: what [`entered_age`] knows of its rows is forgotten, so the cards, chips and
/// voters in them enter again with it, as the web's new DOM does. Another log's stay, and so do other windows' logs.
pub fn remount_log(cx: &mut App, window: gpui::WindowId, log: &str) {
    cx.default_global::<Remembered>()
        .clear_scope(&window_scope(window, log));
}

/// `d-up` drawn on `el` by hand, `elapsed` into an entrance whose delay is `delay` (the entrances a list remembers per key,
/// [`Entrances`], where an element's own state would be lost): `el` is in the flow of its parent, and is returned as it is
/// once the entrance is over.
pub fn rise_style<E: Styled>(
    el: E,
    elapsed: Duration,
    delay: Duration,
    run: Duration,
    curve: [f32; 4],
) -> E {
    if elapsed >= delay + run {
        return el;
    }
    let (opacity, offset) = rise_after_at(elapsed, delay, run, curve);
    el.relative().top(px(offset)).opacity(opacity)
}

/// `el` entering with `d-invite-up` over `run` on `curve` (see [`up_in`], whose keyframe it shares).
pub fn invite_up_in<E>(
    el: E,
    id: impl Into<ElementId>,
    run: Duration,
    curve: [f32; 4],
    cx: &App,
) -> AnyElement
where
    E: Styled + IntoElement + 'static,
{
    rise_with(el, id, run, cx, move |t| invite_up_at(t, run, curve))
}

/// `el` entering with `d-poll-card-entry` or `d-poll-answer-entry` over `run` on `ease`, once per mount.
pub fn poll_entry_in<E>(el: E, id: impl Into<ElementId>, run: Duration, cx: &App) -> AnyElement
where
    E: Styled + IntoElement + 'static,
{
    rise_with(el, id, run, cx, move |t| poll_entry_at(t, run))
}

/// `el` fading in with `d-splash-in` over `run` on `--ease-out`, once per mount. With reduced motion (Splash.css's own
/// `animation: none`) it is drawn as it is.
pub fn splash_in<E>(el: E, id: impl Into<ElementId>, run: Duration, cx: &App) -> AnyElement
where
    E: Styled + IntoElement + 'static,
{
    if cx.reduce_motion() {
        return el.into_any_element();
    }
    el.with_animation(id, Animation::new(run), move |el, delta| {
        if delta >= 1.0 {
            el
        } else {
            el.opacity(splash_in_at(run.mul_f32(delta), run))
        }
    })
    .into_any_element()
}

/// The message log's channel switch (MessageLog.css:3-4) as it stands at `elapsed`: `d-log-fade` over
/// [`motion::SWITCH`] and `d-log-rise` over [`motion::SWITCH_RISE`] on `--ease-out`, at once. The opacity and the downward
/// offset in px.
pub fn log_at(elapsed: Duration) -> (f32, f32) {
    (
        log_fade_at(elapsed, motion::SWITCH),
        log_rise_at(elapsed, motion::SWITCH_RISE, motion::EASE_OUT),
    )
}

/// The entrances of a list that is not drawn whole (the message log draws the rows near its viewport): an element state
/// keyed to the row would be dropped when the row scrolls out of the drawn range and play again when it returns, so the
/// list remembers when each key entered. A key is remembered from the frame the list first holds it, drawn or not (the
/// web's row mounts whether or not it is on screen), and forgotten when the list no longer holds it.
#[derive(Debug)]
pub struct Entrances<K> {
    entered: std::collections::HashMap<K, (std::time::Instant, Duration)>,
}

impl<K> Default for Entrances<K> {
    fn default() -> Self {
        Self {
            entered: std::collections::HashMap::new(),
        }
    }
}

impl<K: std::hash::Hash + Eq + Clone> Entrances<K> {
    /// The list as of `now`: `keys` are what it holds, each with the delay its entrance starts after. Keys not seen before
    /// enter now; keys that are gone are forgotten. Answers whether any entrance is still running at `now` for a run of
    /// `run` (the caller asks for another frame while it is).
    pub fn sync(
        &mut self,
        keys: impl IntoIterator<Item = (K, Duration)>,
        now: std::time::Instant,
        run: Duration,
    ) -> bool {
        let mut seen = std::collections::HashSet::new();
        for (key, delay) in keys {
            self.entered.entry(key.clone()).or_insert((now, delay));
            seen.insert(key);
        }
        self.entered.retain(|k, _| seen.contains(k));
        self.entered
            .values()
            .any(|(at, delay)| now.saturating_duration_since(*at) < *delay + run)
    }

    /// How long `key` has been entering at `now`, and its delay; `None` for a key the list does not hold.
    pub fn of(&self, key: &K, now: std::time::Instant) -> Option<(Duration, Duration)> {
        self.entered
            .get(key)
            .map(|(at, delay)| (now.saturating_duration_since(*at), *delay))
    }

    /// How many keys are remembered.
    pub fn len(&self) -> usize {
        self.entered.len()
    }

    /// Whether no key is remembered.
    pub fn is_empty(&self) -> bool {
        self.entered.is_empty()
    }

    /// `key` is drawn at `now`: it enters now when it is new, and answers how long it has been entering.
    pub fn touch(&mut self, key: K, now: std::time::Instant) -> Duration {
        let (at, _) = *self.entered.entry(key).or_insert((now, Duration::ZERO));
        now.saturating_duration_since(at)
    }

    /// Forgets everything: the next [`Self::sync`] enters every key it holds (a log remounted for another channel).
    pub fn clear(&mut self) {
        self.entered.clear();
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const BEAT: Duration = motion::BEAT;

    fn near(a: f32, b: f32) -> bool {
        (a - b).abs() < 0.01
    }

    /// The curves are the CSS ones: `ease` at its midpoint is 0.8024, `--ease-out` runs ahead of linear, both end where
    /// they start and finish.
    #[test]
    fn the_curves_match_css() {
        assert!(near(cubic_bezier(EASE, 0.0), 0.0) && near(cubic_bezier(EASE, 1.0), 1.0));
        assert!(near(cubic_bezier(EASE, 0.5), 0.8024));
        assert!(cubic_bezier(motion::EASE_OUT, 0.25) > 0.5);
        assert!(near(cubic_bezier(motion::EASE_OUT, 1.0), 1.0));
    }

    /// Each bar at 0, beat/4, beat/2 and 3beat/4: the first from its low point up to full and back, the others the same
    /// shifted by their delays (beat/4 and beat/2).
    #[test]
    fn the_bars_bounce_between_a_fifth_and_full_height_a_quarter_beat_apart() {
        let at = |bar, quarters: u32| bar_scale(BEAT * quarters / 4, BEAT, bar);
        // Bar 0: low, rising (each half is eased, so a quarter beat is 0.8024 of the way up), peak, then falling the same way.
        assert!(near(at(0, 0), 0.2));
        assert!(near(at(0, 1), 0.2 + 0.8 * cubic_bezier(EASE, 0.5)));
        assert!(near(at(0, 2), 1.0));
        assert!(near(at(0, 3), 1.0 - 0.8 * cubic_bezier(EASE, 0.5)));
        // Bar 1 is bar 0 a quarter beat late.
        for q in 0..4 {
            assert!(near(at(1, q + 1), at(0, q)));
            assert!(near(at(2, q + 2), at(0, q)));
        }
        assert!(near(at(1, 3), 1.0) && near(at(2, 0), 1.0));
    }

    /// `d-up` at its start, half way (on `ease`) and its end.
    #[test]
    fn the_rise_goes_from_eight_px_down_and_clear_to_in_place_and_opaque() {
        let run = motion::PLAYER;
        let (o, y) = rise_at(Duration::ZERO, run, EASE);
        assert!(near(o, 0.0) && near(y, 8.0));
        let (o, y) = rise_at(run / 2, run, EASE);
        assert!(near(o, 0.8024) && near(y, 8.0 * (1.0 - 0.8024)));
        let (o, y) = rise_at(run, run, EASE);
        assert!(near(o, 1.0) && near(y, 0.0));
    }

    /// Reduced motion zeroes the durations: the bars are full and nothing rises.
    #[test]
    fn reduced_motion_has_full_bars_and_no_entrance() {
        for bar in 0..3 {
            assert_eq!(
                bar_scale(Duration::from_millis(100), Duration::ZERO, bar),
                1.0
            );
        }
        assert_eq!(rise_at(Duration::ZERO, Duration::ZERO, EASE), (1.0, 0.0));
    }

    /// A view that counts its renders and draws `build`'s element.
    struct Host {
        renders: usize,
        build: Box<dyn Fn(&App) -> AnyElement>,
    }
    impl gpui::Render for Host {
        fn render(
            &mut self,
            _: &mut gpui::Window,
            cx: &mut gpui::Context<Self>,
        ) -> impl IntoElement {
            self.renders += 1;
            (self.build)(cx)
        }
    }

    /// Renders of a host drawing `build` over `ms` of test clock, in 10ms steps (animation frames are delivered by hand,
    /// as a test has no platform frame loop).
    fn renders_over(
        cx: &mut gpui::TestAppContext,
        reduced: bool,
        ms: u64,
        build: impl Fn(&App) -> AnyElement + 'static,
    ) -> usize {
        cx.update(|cx| cx.set_reduce_motion(reduced));
        let (host, vcx) = cx.add_window_view(|_, _| Host {
            renders: 0,
            build: Box::new(build),
        });
        vcx.run_until_parked();
        let before = host.read_with(vcx, |h, _| h.renders);
        for _ in 0..ms / 10 {
            vcx.executor().advance_clock(Duration::from_millis(10));
            vcx.run_until_parked();
            vcx.update(|window, cx| window.simulate_next_frame(cx));
            vcx.run_until_parked();
        }
        host.read_with(vcx, |h, _| h.renders) - before
    }

    /// A drawn equaliser repaints, at the cap and not at every frame (a 10ms step is a 100 Hz display: a second of it asks
    /// for about 30 repaints, not 100); reduced motion, or no equaliser drawn (a paused player draws none), repaints nothing.
    #[gpui::test]
    fn the_equaliser_repaints_at_its_cap_only_while_drawn(cx: &mut gpui::TestAppContext) {
        let drawn = |cx: &App| equaliser("eq", 11.0, cx);
        let playing = renders_over(cx, false, 1000, drawn);
        assert!(
            (20..=40).contains(&playing),
            "{playing} repaints in a second"
        );
        assert_eq!(renders_over(cx, true, 1000, drawn), 0, "reduced motion");
        assert_eq!(
            renders_over(cx, false, 1000, |_| div().into_any_element()),
            0,
            "nothing drawn"
        );
    }

    /// An entrance runs once: it repaints while it rises, and not at all after it has finished, however long the element
    /// stays and however often its parent renders; reduced motion draws no entrance.
    #[gpui::test]
    fn an_entrance_runs_once_per_mount(cx: &mut gpui::TestAppContext) {
        let rising = |cx: &App| rise_in(div(), "up", motion::PLAYER, EASE, cx);
        let during = renders_over(cx, false, 300, rising);
        assert!(during > 0, "the entrance repaints while it runs");
        // The element has been up for longer than its run; frames after that are none.
        let (host, vcx) = cx.add_window_view(|_, _| Host {
            renders: 0,
            build: Box::new(rising),
        });
        vcx.run_until_parked();
        for _ in 0..60 {
            vcx.executor().advance_clock(Duration::from_millis(10));
            vcx.update(|window, cx| window.simulate_next_frame(cx));
            vcx.run_until_parked();
        }
        let settled = host.read_with(vcx, |h, _| h.renders);
        host.update(vcx, |_, cx| cx.notify());
        vcx.run_until_parked();
        for _ in 0..10 {
            vcx.executor().advance_clock(Duration::from_millis(10));
            vcx.update(|window, cx| window.simulate_next_frame(cx));
            vcx.run_until_parked();
        }
        assert_eq!(
            host.read_with(vcx, |h, _| h.renders),
            settled + 1,
            "a re-render after the entrance asks for no more frames"
        );
        assert_eq!(renders_over(cx, true, 300, rising), 0, "reduced motion");
    }

    /// `d-sweep` at its start, middle and end, and over again: -100% to 100%, linear.
    #[test]
    fn the_sweep_runs_from_one_edge_to_the_other_and_starts_again() {
        let run = motion::FLASH;
        assert!(near(sweep_at(Duration::ZERO, run), -1.0));
        assert!(near(sweep_at(run / 4, run), -0.5));
        assert!(near(sweep_at(run / 2, run), 0.0));
        assert!(near(sweep_at(run * 3 / 4, run), 0.5));
        assert!(near(sweep_at(run + run / 4, run), -0.5), "the next run");
        assert_eq!(
            sweep_at(run / 3, Duration::ZERO),
            0.0,
            "reduced: stands still"
        );
    }

    /// An opacity transition runs 0 to 1 on `ease`, and is over at once with a zero duration.
    #[test]
    fn a_transition_runs_from_clear_to_opaque_on_ease() {
        let run = motion::TURN;
        assert!(near(transition_at(Duration::ZERO, run), 0.0));
        assert!(near(transition_at(run / 2, run), 0.8024));
        assert!(near(transition_at(run, run), 1.0));
        assert_eq!(transition_at(run / 2, Duration::ZERO), 1.0);
    }

    /// `d-ring`: 0.6 and unscaled at the start, on `--ease-out` to nothing at 1.5 times, and again.
    #[test]
    fn the_ring_wave_grows_to_one_and_a_half_while_it_fades_from_six_tenths() {
        let run = motion::PULSE;
        let (o, s) = ring_at(Duration::ZERO, run, motion::EASE_OUT);
        assert!(near(o, 0.6) && near(s, 1.0));
        let p = cubic_bezier(motion::EASE_OUT, 0.5);
        let (o, s) = ring_at(run / 2, run, motion::EASE_OUT);
        assert!(near(o, 0.6 * (1.0 - p)) && near(s, 1.0 + 0.5 * p));
        assert!(p > 0.5, "--ease-out runs ahead of linear");
        let (o, s) = ring_at(run - Duration::from_millis(1), run, motion::EASE_OUT);
        assert!(o < 0.01 && near(s, 1.5));
        let (o, s) = ring_at(run, run, motion::EASE_OUT);
        assert!(near(o, 0.6) && near(s, 1.0), "the next run starts over");
        assert_eq!(
            ring_at(run / 3, Duration::ZERO, motion::EASE_OUT),
            (1.0, 1.0)
        );
    }

    /// `d-spin`: linear, a full turn per run.
    #[test]
    fn the_spinner_turns_once_a_run() {
        let run = motion::RESULT_SPIN;
        let pi = std::f32::consts::PI;
        assert!(near(spin_at(Duration::ZERO, run), 0.0));
        assert!(near(spin_at(run / 2, run), pi));
        assert!(near(spin_at(run * 3 / 4, run), 1.5 * pi));
        assert!(near(spin_at(run + run / 2, run), pi), "the next run");
        assert_eq!(spin_at(run / 3, Duration::ZERO), 0.0, "reduced: upright");
    }

    /// `d-row-flash`: the full tint to 30% of the run, then `ease-out` to nothing.
    #[test]
    fn the_row_flash_holds_for_thirty_percent_then_fades_out() {
        let run = motion::FLASH;
        assert!(near(flash_at(Duration::ZERO, run), 1.0));
        assert!(near(flash_at(run * 3 / 10, run), 1.0));
        let mid = flash_at(run * 65 / 100, run);
        assert!(near(mid, 1.0 - cubic_bezier(CSS_EASE_OUT, 0.5)));
        assert!(mid > 0.0 && mid < 1.0);
        assert!(near(flash_at(run, run), 0.0));
        assert_eq!(flash_at(run / 2, Duration::ZERO), 0.0, "reduced: no flash");
    }

    /// The sweep repaints at its cap while drawn (a 10ms step is a 100 Hz display: a second asks for about 30, not 100);
    /// an idle composer draws none, and reduced motion draws it still.
    #[gpui::test]
    fn the_sweep_repaints_at_its_cap_only_while_drawn(cx: &mut gpui::TestAppContext) {
        let drawn = |cx: &App| sweep("sweep", cx);
        let typing = renders_over(cx, false, 1000, drawn);
        assert!((20..=40).contains(&typing), "{typing} repaints in a second");
        assert_eq!(renders_over(cx, true, 1000, drawn), 0, "reduced motion");
        assert_eq!(
            renders_over(cx, false, 1000, |_| div().h(px(2.0)).into_any_element()),
            0,
            "an idle composer"
        );
    }

    /// A pending result's spinner repaints at its cap; a settled result draws a plain icon and schedules nothing.
    #[gpui::test]
    fn the_spinner_repaints_only_while_pending(cx: &mut gpui::TestAppContext) {
        use crate::icon_names::IconName;
        let pending = |cx: &App| spinning(Icon::new(IconName::Loader), "spin", cx);
        let turning = renders_over(cx, false, 1000, pending);
        assert!(
            (20..=40).contains(&turning),
            "{turning} repaints in a second"
        );
        assert_eq!(renders_over(cx, true, 1000, pending), 0, "reduced motion");
        assert_eq!(
            renders_over(cx, false, 1000, |_| Icon::new(IconName::Check)
                .into_any_element()),
            0,
            "a settled result"
        );
    }

    /// The wave repaints at its cap while an incoming ring is drawn; a busy ring draws none, and reduced motion a still ring.
    #[gpui::test]
    fn the_ring_wave_repaints_only_while_drawn(cx: &mut gpui::TestAppContext) {
        let drawn = |cx: &App| {
            div()
                .relative()
                .size(px(32.0))
                .child(ring_wave("wave", 32.0, px(999.0), cx))
                .into_any_element()
        };
        let waving = renders_over(cx, false, 1000, drawn);
        assert!((20..=40).contains(&waving), "{waving} repaints in a second");
        assert_eq!(renders_over(cx, true, 1000, drawn), 0, "reduced motion");
        assert_eq!(
            renders_over(cx, false, 1000, |_| div()
                .relative()
                .size(px(32.0))
                .into_any_element()),
            0,
            "a busy ring"
        );
    }

    /// A flash runs once: it repaints while it fades, never again after its run however the parent renders; reduced motion
    /// repaints nothing.
    #[gpui::test]
    fn a_flash_runs_once_per_jump(cx: &mut gpui::TestAppContext) {
        let flashing = |cx: &App| flash(div().relative(), "flash", cx).into_any_element();
        let during = renders_over(cx, false, 1600, flashing);
        assert!(during > 0, "the flash repaints while it fades");
        let (host, vcx) = cx.add_window_view(|_, _| Host {
            renders: 0,
            build: Box::new(flashing),
        });
        vcx.run_until_parked();
        for _ in 0..160 {
            vcx.executor().advance_clock(Duration::from_millis(10));
            vcx.update(|window, cx| window.simulate_next_frame(cx));
            vcx.run_until_parked();
        }
        let settled = host.read_with(vcx, |h, _| h.renders);
        host.update(vcx, |_, cx| cx.notify());
        vcx.run_until_parked();
        for _ in 0..10 {
            vcx.executor().advance_clock(Duration::from_millis(10));
            vcx.update(|window, cx| window.simulate_next_frame(cx));
            vcx.run_until_parked();
        }
        assert_eq!(
            host.read_with(vcx, |h, _| h.renders),
            settled + 1,
            "a re-render after the flash asks for no more frames"
        );
        assert_eq!(renders_over(cx, true, 1600, flashing), 0, "reduced motion");
    }

    /// An entrance `build` draws runs once: it repaints while it runs, and a parent that renders again after its run asks
    /// for no more frames; reduced motion repaints nothing.
    fn runs_once(
        cx: &mut gpui::TestAppContext,
        run_ms: u64,
        build: impl Fn(&App) -> AnyElement + Clone + 'static,
    ) {
        let during = renders_over(cx, false, run_ms + 100, build.clone());
        assert!(during > 0, "the entrance repaints while it runs");
        let (host, vcx) = cx.add_window_view(|_, _| Host {
            renders: 0,
            build: Box::new(build.clone()),
        });
        vcx.run_until_parked();
        for _ in 0..(run_ms / 10 + 20) {
            vcx.executor().advance_clock(Duration::from_millis(10));
            vcx.update(|window, cx| window.simulate_next_frame(cx));
            vcx.run_until_parked();
        }
        let settled = host.read_with(vcx, |h, _| h.renders);
        host.update(vcx, |_, cx| cx.notify());
        vcx.run_until_parked();
        for _ in 0..10 {
            vcx.executor().advance_clock(Duration::from_millis(10));
            vcx.update(|window, cx| window.simulate_next_frame(cx));
            vcx.run_until_parked();
        }
        assert_eq!(
            host.read_with(vcx, |h, _| h.renders),
            settled + 1,
            "a re-render after the entrance asks for no more frames"
        );
        assert_eq!(
            renders_over(cx, true, run_ms + 100, build),
            0,
            "reduced motion"
        );
    }

    /// `d-modal` at its start, half way and its end: it rises from 8px above and 0.985 of its size, opaque only at the end.
    #[test]
    fn the_modal_settles_from_eight_px_above_and_clear() {
        let run = motion::DIALOG;
        let start = modal_at(Duration::ZERO, run, motion::EASE_OUT);
        assert!(near(start.opacity, 0.0) && near(start.offset_y, -8.0) && near(start.scale, 0.985));
        let mid = modal_at(run / 2, run, motion::EASE_OUT);
        let p = cubic_bezier(motion::EASE_OUT, 0.5);
        assert!(near(mid.opacity, p) && near(mid.offset_y, -8.0 * (1.0 - p)));
        assert!(near(mid.scale, 0.985 + 0.015 * p));
        assert!(mid.opacity > 0.5, "--ease-out is ahead of linear");
        let end = modal_at(run, run, motion::EASE_OUT);
        assert!(near(end.opacity, 1.0) && near(end.offset_y, 0.0) && near(end.scale, 1.0));
    }

    /// A zero run is the end state, for every one of the keyframes (reduced motion zeroes the durations).
    #[test]
    fn a_zero_run_is_the_end_state() {
        let mid = Duration::from_millis(50);
        assert_eq!(modal_at(mid, Duration::ZERO, EASE), Entrance::SETTLED);
        assert_eq!(toast_at(mid, Duration::ZERO, EASE), Entrance::SETTLED);
        assert_eq!(fade_at(mid, Duration::ZERO, EASE), 1.0);
        assert_eq!(pop_at(mid, Duration::ZERO, EASE), 1.0);
    }

    /// `d-toast` at its start, half way and its end: it rises from 10px below at 0.96 of its size.
    #[test]
    fn the_toast_rises_from_ten_px_below_at_ninety_six_percent() {
        let run = motion::TOAST;
        let start = toast_at(Duration::ZERO, run, motion::EASE_OUT);
        assert!(near(start.opacity, 0.0) && near(start.offset_y, 10.0) && near(start.scale, 0.96));
        let mid = toast_at(run / 2, run, motion::EASE_OUT);
        let p = cubic_bezier(motion::EASE_OUT, 0.5);
        assert!(near(mid.offset_y, 10.0 * (1.0 - p)) && near(mid.scale, 0.96 + 0.04 * p));
        let end = toast_at(run, run, motion::EASE_OUT);
        assert!(near(end.opacity, 1.0) && near(end.offset_y, 0.0) && near(end.scale, 1.0));
    }

    /// `d-fade` at its start, half way (on `ease`: 0.8024) and its end.
    #[test]
    fn the_fade_goes_from_clear_to_opaque_on_ease() {
        let run = motion::SCRIM;
        assert!(near(fade_at(Duration::ZERO, run, EASE), 0.0));
        assert!(near(fade_at(run / 2, run, EASE), 0.8024));
        assert!(near(fade_at(run, run, EASE), 1.0));
    }

    /// `d-pop` at its start, at its 60% stop, between the stops (on the curve) and its end: 0.3, 1.2 and back to 1.
    #[test]
    fn the_pop_overshoots_to_one_and_a_fifth_at_sixty_percent() {
        let run = motion::BADGE;
        assert!(near(pop_at(Duration::ZERO, run, motion::EASE_OUT), 0.3));
        assert!(near(pop_at(run.mul_f32(0.6), run, motion::EASE_OUT), 1.2));
        assert!(near(pop_at(run, run, motion::EASE_OUT), 1.0));
        // Half way up the first interval (30% of the run) the curve has gone further than half.
        let first = pop_at(run.mul_f32(0.3), run, motion::EASE_OUT);
        assert!(near(first, 0.3 + 0.9 * cubic_bezier(motion::EASE_OUT, 0.5)));
        assert!(first > 0.75, "--ease-out is ahead of linear");
        // Half way down the second (80%) it is between the peak and its rest.
        let second = pop_at(run.mul_f32(0.8), run, motion::EASE_OUT);
        assert!(near(
            second,
            1.2 - 0.2 * cubic_bezier(motion::EASE_OUT, 0.5)
        ));
        let between = pop_at(run.mul_f32(0.45), run, EASE);
        assert!(between > 1.0 && between < 1.2);
    }

    #[gpui::test]
    fn a_modal_enters_once_per_mount(cx: &mut gpui::TestAppContext) {
        runs_once(cx, 260, |cx| modal_in(div(), "modal", motion::DIALOG, cx));
    }

    #[gpui::test]
    fn a_toast_enters_once_per_mount(cx: &mut gpui::TestAppContext) {
        runs_once(cx, 220, |cx| toast_in(div(), "toast", cx));
    }

    #[gpui::test]
    fn a_fade_runs_once_per_mount(cx: &mut gpui::TestAppContext) {
        runs_once(cx, 160, |cx| {
            fade_in(div(), "fade", motion::PRESS, EASE, cx)
        });
    }

    #[gpui::test]
    fn a_scrim_fades_in_once_per_mount(cx: &mut gpui::TestAppContext) {
        runs_once(cx, 160, |cx| scrim_layer("scrim", color::SCRIM, cx));
    }

    #[gpui::test]
    fn a_badge_pops_once_per_mount(cx: &mut gpui::TestAppContext) {
        runs_once(cx, 450, |cx| {
            pop_in("pop", motion::BADGE, motion::EASE_OUT, cx, |pop| {
                div().size(px(16.0 * pop.scale())).into_any_element()
            })
        });
    }

    /// An entrance the web switches off (a settings handoff) asks for no frames, as one that runs asks for some.
    #[gpui::test]
    fn a_still_modal_asks_for_no_frames(cx: &mut gpui::TestAppContext) {
        let run =
            |still: bool| move |cx: &App| modal_in_unless(div(), "m", motion::MODAL, still, cx);
        assert!(renders_over(cx, false, 400, run(false)) > 3);
        assert_eq!(renders_over(cx, false, 400, run(true)), 0);
    }

    /// An overlay that is not drawn (a closed menu, a hidden tooltip) schedules no frames at all.
    #[gpui::test]
    fn an_overlay_that_is_not_drawn_costs_no_frames(cx: &mut gpui::TestAppContext) {
        assert_eq!(
            renders_over(cx, false, 500, |_| div().into_any_element()),
            0
        );
    }

    /// The opacity transitions repaint while they run (150ms is 15 steps), never after, and reduced motion draws none.
    #[gpui::test]
    fn a_transition_repaints_only_while_it_runs(cx: &mut gpui::TestAppContext) {
        let fading_in = |cx: &App| transition_in(div(), "in", motion::FAST, cx);
        let during = renders_over(cx, false, 150, fading_in);
        assert!(during > 0 && during <= 6, "{during} repaints while it runs");
        assert!(
            renders_over(cx, false, 1000, fading_in) <= 8,
            "no frames once the transition is over"
        );
        let fading_out = |cx: &App| transition_out(div(), "out", motion::FAST, cx);
        let during = renders_over(cx, false, 150, fading_out);
        assert!(during > 0, "{during}");
        assert_eq!(renders_over(cx, true, 500, fading_in), 0, "reduced motion");
        assert_eq!(renders_over(cx, true, 500, fading_out), 0, "reduced motion");
    }

    /// With reduced motion the flash is MessageRow.css's own form: the tint and an inset 2px edge that replaces the
    /// element's shadows; with motion it is a layer in the element and the element's own style stays as it was.
    #[gpui::test]
    fn the_flash_form_follows_reduced_motion(cx: &mut gpui::TestAppContext) {
        let shadow = || {
            vec![BoxShadow {
                color: color::FG,
                offset: point(px(0.0), px(0.0)),
                blur_radius: px(0.0),
                spread_radius: px(2.0),
                inset: true,
            }]
        };
        cx.update(|cx| cx.set_reduce_motion(true));
        let mut reduced = cx.update(|cx| flash(div().shadow(shadow()), "f", cx));
        let shadows = reduced.style().box_shadow.clone().unwrap();
        assert_eq!(shadows.len(), 1, "the edge replaces the shadows");
        assert!(shadows[0].inset && shadows[0].offset.x == px(2.0));
        assert!(reduced.style().background.is_some());
        cx.update(|cx| cx.set_reduce_motion(false));
        let mut moving = cx.update(|cx| flash(div().shadow(shadow()), "f", cx));
        assert_eq!(moving.style().box_shadow.clone().unwrap(), shadow());
        assert!(moving.style().background.is_none(), "the layer is the tint");
    }

    // Task 36c.

    /// `d-up` after a delay holds its first frame (opacity 0, 8px down) until the delay is over, then runs as `d-up` does.
    #[test]
    fn a_delayed_rise_holds_its_first_frame_then_runs() {
        let (run, delay) = (motion::ENTER, motion::STAGGER * 3);
        let curve = motion::EASE_OUT;
        let first = rise_after_at(Duration::ZERO, delay, run, curve);
        assert!(near(first.0, 0.0) && near(first.1, 8.0));
        let held = rise_after_at(delay, delay, run, curve);
        assert!(near(held.0, 0.0) && near(held.1, 8.0));
        assert!(
            near(
                rise_after_at(delay - Duration::from_millis(1), delay, run, curve).0,
                0.0
            ),
            "the delay holds the first frame"
        );
        let mid = rise_after_at(delay + run / 2, delay, run, curve);
        assert_eq!(mid, rise_at(run / 2, run, curve));
        assert!(mid.0 > 0.5 && mid.1 < 4.0, "--ease-out is ahead of linear");
        let end = rise_after_at(delay + run, delay, run, curve);
        assert!(near(end.0, 1.0) && near(end.1, 0.0));
    }

    /// `d-invite-up`, `d-poll-card-entry` and `d-poll-answer-entry` are `d-up`'s fade and 8px rise.
    #[test]
    fn the_invite_and_poll_entries_fade_and_rise_8px() {
        let run = motion::STEP;
        for at in [
            invite_up_at(Duration::ZERO, run, motion::EASE_OUT),
            poll_entry_at(Duration::ZERO, run),
        ] {
            assert!(near(at.0, 0.0) && near(at.1, 8.0), "{at:?}");
        }
        let mid = poll_entry_at(run / 2, run);
        assert!(near(mid.0, 0.8024) && near(mid.1, 8.0 * (1.0 - 0.8024)));
        let end = invite_up_at(run, run, motion::EASE_OUT);
        assert!(near(end.0, 1.0) && near(end.1, 0.0));
        assert_eq!(poll_entry_at(Duration::ZERO, Duration::ZERO), (1.0, 0.0));
    }

    /// `d-poll-voter-pop`: 0.3 at the start, 1.2 at 60%, 1 at the end.
    #[test]
    fn the_voter_pop_is_the_pop_on_ease() {
        let run = motion::STEP;
        assert!(near(poll_voter_pop_at(Duration::ZERO, run), 0.3));
        assert!(near(poll_voter_pop_at(run.mul_f32(0.6), run), 1.2));
        assert!(near(poll_voter_pop_at(run, run), 1.0));
        assert!(near(poll_voter_pop_at(Duration::ZERO, Duration::ZERO), 1.0));
    }

    /// `d-log-fade` (140ms, `ease`) and `d-log-rise` (240ms, `--ease-out`, 6px) run at once and end apart.
    #[test]
    fn the_log_fades_in_140ms_and_rises_6px_in_240ms() {
        let start = log_at(Duration::ZERO);
        assert!(near(start.0, 0.0) && near(start.1, 6.0));
        let mid = log_at(Duration::from_millis(70));
        assert!(near(mid.0, 0.8024), "{mid:?}");
        assert!(
            mid.1 < 1.0 && mid.1 > 0.0,
            "--ease-out is ahead of linear: {mid:?}"
        );
        let faded = log_at(motion::SWITCH);
        assert!(near(faded.0, 1.0) && faded.1 > 0.0, "{faded:?}");
        let over = log_at(motion::SWITCH_RISE);
        assert!(near(over.0, 1.0) && near(over.1, 0.0));
        assert_eq!(log_rise_at(Duration::ZERO, Duration::ZERO, EASE), 0.0);
    }

    /// `d-splash-in`: opacity 0 to 1 on `--ease-out`; a zero run is over at once.
    #[test]
    fn the_splash_fades_in_on_ease_out() {
        let run = motion::SLOW;
        assert!(near(splash_in_at(Duration::ZERO, run), 0.0));
        assert!(splash_in_at(run / 4, run) > 0.5);
        assert!(near(splash_in_at(run, run), 1.0));
        assert_eq!(splash_in_at(Duration::ZERO, Duration::ZERO), 1.0);
    }

    /// A list that is drawn in part remembers when each key entered: a key seen again does not enter again, a gone key is
    /// forgotten (it enters afresh if it returns), and the list answers whether anything still runs.
    #[test]
    fn entrances_are_remembered_per_key() {
        let t0 = std::time::Instant::now();
        let run = Duration::from_millis(300);
        let mut e: Entrances<&str> = Entrances::default();
        let delay = Duration::from_millis(40);
        assert!(e.sync([("a", Duration::ZERO), ("b", delay)], t0, run));
        let later = t0 + Duration::from_millis(100);
        assert_eq!(
            e.of(&"a", later),
            Some((Duration::from_millis(100), Duration::ZERO))
        );
        assert_eq!(e.of(&"b", later), Some((Duration::from_millis(100), delay)));
        // A key new later enters then, with no delay; the old ones keep their start.
        assert!(e.sync(
            [
                ("a", Duration::ZERO),
                ("b", Duration::ZERO),
                ("c", Duration::ZERO)
            ],
            later,
            run
        ));
        assert_eq!(e.of(&"b", later).unwrap().1, delay);
        assert_eq!(e.of(&"c", later).unwrap().0, Duration::ZERO);
        // Over: nothing runs once every run is done.
        let over = t0 + Duration::from_secs(5);
        assert!(!e.sync(
            [
                ("a", Duration::ZERO),
                ("b", Duration::ZERO),
                ("c", Duration::ZERO)
            ],
            over,
            run
        ));
        // `a` leaves and returns: it enters again.
        e.sync([("b", Duration::ZERO)], over, run);
        assert!(e.of(&"a", over).is_none());
        e.sync([("a", Duration::ZERO), ("b", Duration::ZERO)], over, run);
        assert_eq!(e.of(&"a", over).unwrap().0, Duration::ZERO);
        assert_eq!(e.touch("z", over), Duration::ZERO);
        assert_eq!(
            e.touch("z", over + Duration::from_millis(7)),
            Duration::from_millis(7)
        );
    }

    #[gpui::test]
    fn an_up_entrance_with_a_delay_runs_once_per_mount(cx: &mut gpui::TestAppContext) {
        runs_once(cx, 380 + 80, |cx| {
            up_in(
                div(),
                "up-delayed",
                motion::STAGGER * 2,
                motion::ENTER,
                motion::EASE_OUT,
                cx,
            )
        });
    }

    #[gpui::test]
    fn an_invite_entrance_runs_once_per_mount(cx: &mut gpui::TestAppContext) {
        runs_once(cx, 300, |cx| {
            invite_up_in(div(), "invite", motion::SLOW, motion::EASE_OUT, cx)
        });
    }

    #[gpui::test]
    fn a_poll_entrance_runs_once_per_mount(cx: &mut gpui::TestAppContext) {
        runs_once(cx, 360, |cx| poll_entry_in(div(), "poll", motion::STEP, cx));
    }

    #[gpui::test]
    fn the_splash_fades_in_once_per_mount(cx: &mut gpui::TestAppContext) {
        runs_once(cx, 300, |cx| splash_in(div(), "splash", motion::SLOW, cx));
    }

    /// What the process remembers of its entrances is bounded and drops the least recently drawn: the keys on screen are
    /// drawn every frame and stay, so a full map never makes visible things enter again together.
    #[test]
    fn the_remembered_entrances_drop_the_least_recently_drawn_not_everything() {
        let t0 = std::time::Instant::now();
        let mut r = Remembered::default();
        for n in 0..REMEMBERED_MAX {
            r.touch(format!("k{n}"), t0);
        }
        assert_eq!(r.len(), REMEMBERED_MAX);
        // The newest 100 are on screen: drawn again 50ms later.
        let later = t0 + Duration::from_millis(50);
        for n in REMEMBERED_MAX - 100..REMEMBERED_MAX {
            assert_eq!(r.touch(format!("k{n}"), later), Duration::from_millis(50));
        }
        // A new key finds the map full: the oldest eighth goes, the on-screen keys keep their clocks.
        r.touch("new".into(), later);
        assert!(r.len() < REMEMBERED_MAX && r.len() > REMEMBERED_MAX - REMEMBERED_MAX / 4);
        for n in REMEMBERED_MAX - 100..REMEMBERED_MAX {
            let age = r.touch(format!("k{n}"), later);
            assert_eq!(age, Duration::from_millis(50), "k{n} enters again");
        }
        assert_eq!(
            r.touch("k0".into(), later),
            Duration::ZERO,
            "k0 was dropped"
        );
    }

    /// Final review I1: entrances are remembered per window. A log mounting anew in window A replays A's cards and chips,
    /// never window B's (B keeps its key, its entrance over); a second window drawing the same log enters its own. A
    /// log's scope ends with its rows: what is drawn after them is in no log.
    #[gpui::test]
    fn a_log_remounting_in_one_window_leaves_the_other_windows_entrances(
        cx: &mut gpui::TestAppContext,
    ) {
        cx.update(|cx| cx.set_reduce_motion(false));
        let a = cx
            .add_empty_window()
            .update(|w, _| w.window_handle().window_id());
        let b = cx
            .add_empty_window()
            .update(|w, _| w.window_handle().window_id());
        let run = motion::SLOW;
        let draw = |window, cx: &mut App| {
            set_scope(cx, window, "channel-log");
            let key = scoped(&scope(cx), "poll-card-x");
            let age = entered_age(cx, key, run);
            end_scope(cx);
            age
        };
        cx.update(|cx| {
            assert_eq!(draw(a, cx), Some(Duration::ZERO), "A's card enters");
            assert_eq!(draw(b, cx), Some(Duration::ZERO), "B's own card enters too");
            assert!(scope(cx).is_empty(), "no log after the rows");
        });
        cx.executor().advance_clock(run * 2);
        cx.update(|cx| {
            remount_log(cx, a, "channel-log");
            assert_eq!(draw(b, cx), None, "B's entrance stays over");
            assert_eq!(draw(a, cx), Some(Duration::ZERO), "A's log mounted anew");
        });
    }

    /// A remounted log forgets its own scope's keys and keeps every other scope's.
    #[test]
    fn a_log_mounting_anew_forgets_its_own_keys_only() {
        let t0 = std::time::Instant::now();
        let later = t0 + Duration::from_secs(1);
        let mut r = Remembered::default();
        r.touch(scoped("log", "chip"), t0);
        r.touch(scoped("thread", "chip"), t0);
        r.clear_scope("thread");
        assert_eq!(
            r.touch(scoped("log", "chip"), later),
            Duration::from_secs(1)
        );
        assert_eq!(r.touch(scoped("thread", "chip"), later), Duration::ZERO);
    }

    /// A thing that is gone is forgotten and enters again when it returns; a cleared memory forgets them all.
    #[test]
    fn a_thing_that_is_gone_enters_again_when_it_returns() {
        let t0 = std::time::Instant::now();
        let later = t0 + Duration::from_secs(1);
        let mut r = Remembered::default();
        for k in ["bar-pop-a", "bar-pop-b", "other"] {
            r.touch(k.into(), t0);
        }
        r.forget("bar-pop-", |k| k == "bar-pop-a");
        assert_eq!(r.touch("bar-pop-a".into(), later), Duration::from_secs(1));
        assert_eq!(r.touch("bar-pop-b".into(), later), Duration::ZERO);
        assert_eq!(r.touch("other".into(), later), Duration::from_secs(1));
        r.clear();
        assert!(r.is_empty());
    }
}
