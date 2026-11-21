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
    Animation, AnimationExt, AnyElement, App, ElementId, IntoElement, ParentElement, Styled, div,
    px,
};

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
}
