//! Task 12's components (`components/attachments/`): the ports of `packages/ui/src/{AttachmentCard, AttachmentTray,
//! DropOverlay, Lightbox, MediaPlayer, GifPicker}`. They draw what they are given; the app's `screens::attachments`
//! holds the state and does the work. Every size is the ui CSS's, through the tokens; a constant without a token names
//! the CSS it comes from.

pub mod card;
pub mod drop_overlay;
pub mod gif_picker;
pub mod lightbox;
pub mod player;
pub mod tray;
pub mod video;

use gpui::{App, ClickEvent, Window};

/// A click handler of these components.
pub type OnClick = std::rc::Rc<dyn Fn(&ClickEvent, &mut Window, &mut App) + 'static>;

/// What the components' tests share: a two-frame GIF and hosts that count the animation frames a drawing asks for and
/// the frames and pictures it paints.
#[cfg(test)]
pub(crate) mod testing {
    use std::sync::Arc;
    use std::time::Duration;

    use gpui::{AnyElement, App, Context, Image, ImageFormat, IntoElement, Render, Window};

    /// A 1x1 GIF89a of two frames (black, then white), 100ms each.
    pub const TWO_FRAMES: &[u8] = &[
        0x47, 0x49, 0x46, 0x38, 0x39, 0x61, 0x01, 0x00, 0x01, 0x00, 0x80, 0x00, 0x00, 0x00, 0x00,
        0x00, 0xff, 0xff, 0xff, 0x21, 0xf9, 0x04, 0x00, 0x0a, 0x00, 0x00, 0x00, 0x2c, 0x00, 0x00,
        0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00, 0x02, 0x02, 0x44, 0x01, 0x00, 0x21, 0xf9, 0x04,
        0x00, 0x0a, 0x00, 0x00, 0x00, 0x2c, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00,
        0x02, 0x02, 0x4c, 0x01, 0x00, 0x3b,
    ];

    pub fn gif() -> Arc<Image> {
        Arc::new(Image::from_bytes(ImageFormat::Gif, TWO_FRAMES.to_vec()))
    }

    /// [`TWO_FRAMES`] on a `w` x `h` canvas (its frames are drawn at the top left of it).
    pub fn gif_sized(w: u8, h: u8) -> Arc<Image> {
        let mut bytes = TWO_FRAMES.to_vec();
        bytes[6..10].copy_from_slice(&[w, 0, h, 0]);
        Arc::new(Image::from_bytes(ImageFormat::Gif, bytes))
    }

    type Build = Box<dyn Fn(&mut Window, &mut App) -> AnyElement>;

    struct Host(Build);

    impl Render for Host {
        fn render(&mut self, window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
            (self.0)(window, cx)
        }
    }

    /// The animation frames a host drawing `build` in an active window asks for over three frames, once its pictures
    /// are decoded (frames are delivered by hand: a test has no platform frame loop).
    pub fn frames_asked(
        cx: &mut gpui::TestAppContext,
        reduced: bool,
        build: impl Fn(&mut Window, &mut App) -> AnyElement + 'static,
    ) -> usize {
        cx.update(|cx| cx.set_reduce_motion(reduced));
        let (host, vcx) = cx.add_window_view(|_, _| Host(Box::new(build)));
        vcx.update(|window, _| window.activate_window());
        vcx.run_until_parked();
        // The decode has landed; the next render draws the picture.
        host.update(vcx, |_, cx| cx.notify());
        vcx.run_until_parked();
        let mut asked = 0;
        for _ in 0..3 {
            vcx.executor().advance_clock(Duration::from_millis(50));
            asked += vcx.update(|window, cx| window.simulate_next_frame(cx));
            vcx.run_until_parked();
        }
        asked
    }

    /// The distinct GIF frames a host drawing `build` (one picture) in an active window paints over two renders a GIF
    /// frame's delay apart, once its picture is decoded: 2 when it plays, 1 when it holds a frame. GPUI's `Img` times its
    /// frames by the wall clock (`Instant::now`), so the renders are a real 150ms apart (over the frames' 100ms).
    pub fn frames_painted(
        cx: &mut gpui::TestAppContext,
        reduced: bool,
        build: impl Fn(&mut Window, &mut App) -> AnyElement + 'static,
    ) -> usize {
        cx.update(|cx| cx.set_reduce_motion(reduced));
        let (host, vcx) = cx.add_window_view(|_, _| Host(Box::new(build)));
        vcx.update(|window, _| window.activate_window());
        vcx.run_until_parked();
        let mut tiles = Vec::new();
        for i in 0..3 {
            if i == 2 {
                std::thread::sleep(Duration::from_millis(150));
            }
            host.update(vcx, |_, cx| cx.notify());
            vcx.run_until_parked();
            if let Some(s) = vcx.update(|window, _| window.painted_sprites().last().copied()) {
                tiles.push(s.tile);
            }
        }
        tiles.dedup();
        tiles.len()
    }

    /// The pictures a host drawing `build` paints, once they are decoded.
    pub fn sprites(
        cx: &mut gpui::TestAppContext,
        build: impl Fn(&mut Window, &mut App) -> AnyElement + 'static,
    ) -> Vec<gpui::PolychromeSprite> {
        let (host, vcx) = cx.add_window_view(|_, _| Host(Box::new(build)));
        vcx.run_until_parked();
        host.update(vcx, |_, cx| cx.notify());
        vcx.run_until_parked();
        vcx.update(|window, _| window.painted_sprites())
    }
}
