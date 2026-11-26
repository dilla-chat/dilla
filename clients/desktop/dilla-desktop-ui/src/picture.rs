//! A picture that fills a rounded box. GPUI clips a child to a rectangle only (`overflow_hidden` is a rectangular
//! content mask), so a picture in a rounded box would show square corners wherever it reaches them. The picture carries
//! the box's inner radius itself instead (its outer radius less its border), as CSS's `overflow: hidden` clips it.

use gpui::{ImageSource, Img, ObjectFit, Pixels, Styled, StyledImage, img, px};

/// The radius a picture inside a box of radius `outer` with a `border` takes: the border's inner edge (CSS's inner
/// border radius), never below zero.
pub fn inner_radius(outer: Pixels, border: Pixels) -> Pixels {
    (outer - border).max(px(0.0))
}

/// A picture filling its rounded box (`size_full`, `fit`), rounded to the box's [`inner_radius`]. GPUI rounds the visible
/// part of the picture (`Window::paint_image`), so a letterboxed `Contain` picture gets rounded corners on itself, where
/// CSS would leave its square ones inside the box; it never shows past the box's corners either way.
pub fn rounded_picture(
    src: impl Into<ImageSource>,
    outer: Pixels,
    border: Pixels,
    fit: ObjectFit,
) -> Img {
    img(src)
        .size_full()
        .object_fit(fit)
        .rounded(inner_radius(outer, border))
}

/// What tests of rounded pictures share: a decoded picture and the corner radius a drawing gave its picture.
#[cfg(test)]
pub(crate) mod testing {
    use std::sync::Arc;

    use gpui::{AnyElement, App, Corners, Pixels, RenderImage, Window, px};

    /// A decoded `w` x `h` picture (the test GIF's canvas), as a video frame or a share thumbnail is handed over.
    pub fn decoded(cx: &mut gpui::TestAppContext, w: u8, h: u8) -> Arc<RenderImage> {
        let image = crate::components::attachments::testing::gif_sized(w, h);
        let vcx = cx.add_empty_window();
        let first = vcx.update(|window, cx| image.clone().get_render_image(window, cx));
        if let Some(r) = first {
            return r;
        }
        vcx.run_until_parked();
        vcx.update(|window, cx| image.clone().get_render_image(window, cx))
            .expect("decoded")
    }

    /// A 64 x 48 PNG of one colour: a still picture.
    pub const PNG: &[u8] = &[
        0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44,
        0x52, 0x00, 0x00, 0x00, 0x40, 0x00, 0x00, 0x00, 0x30, 0x08, 0x02, 0x00, 0x00, 0x00, 0x2e,
        0x29, 0xeb, 0x48, 0x00, 0x00, 0x00, 0x43, 0x49, 0x44, 0x41, 0x54, 0x78, 0xda, 0xed, 0xcf,
        0x41, 0x09, 0x00, 0x00, 0x08, 0x04, 0xb0, 0x8b, 0x63, 0x1c, 0xe3, 0x18, 0xdf, 0x0a, 0x7e,
        0x85, 0xc1, 0x0a, 0x2c, 0xd5, 0xf3, 0x5a, 0x04, 0x04, 0x04, 0x04, 0x04, 0x04, 0x04, 0x04,
        0x04, 0x04, 0x04, 0x04, 0x04, 0x04, 0x04, 0x04, 0x04, 0x04, 0x04, 0x04, 0x04, 0x04, 0x04,
        0x04, 0x04, 0x04, 0x04, 0x04, 0x04, 0x04, 0x04, 0x04, 0x04, 0x04, 0x04, 0xae, 0x16, 0x51,
        0xfa, 0x40, 0xa6, 0xca, 0xd6, 0x51, 0xbd, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44,
        0xae, 0x42, 0x60, 0x82,
    ];

    pub fn png() -> Arc<gpui::Image> {
        Arc::new(gpui::Image::from_bytes(
            gpui::ImageFormat::Png,
            PNG.to_vec(),
        ))
    }

    /// The corner radii of every picture a host drawing `build` paints, in painting order, in logical px.
    pub fn picture_corners(
        cx: &mut gpui::TestAppContext,
        build: impl Fn(&mut Window, &mut App) -> AnyElement + 'static,
    ) -> Vec<Corners<Pixels>> {
        let (sprites, scale) = crate::components::attachments::testing::sprites(cx, build);
        assert!(!sprites.is_empty(), "a picture is painted");
        sprites
            .iter()
            .map(|s| {
                let c = s.corner_radii;
                Corners {
                    top_left: px(c.top_left.0 / scale),
                    top_right: px(c.top_right.0 / scale),
                    bottom_left: px(c.bottom_left.0 / scale),
                    bottom_right: px(c.bottom_right.0 / scale),
                }
            })
            .collect()
    }

    /// The smallest corner radius of the last picture a host drawing `build` paints, in logical px.
    pub fn picture_corner(
        cx: &mut gpui::TestAppContext,
        build: impl Fn(&mut Window, &mut App) -> AnyElement + 'static,
    ) -> Pixels {
        let c = *picture_corners(cx, build).last().expect("a picture");
        c.top_left
            .min(c.top_right)
            .min(c.bottom_left)
            .min(c.bottom_right)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use gpui::{IntoElement, ParentElement, div};

    #[test]
    fn the_inner_radius_is_the_outer_less_the_border() {
        assert_eq!(inner_radius(px(8.0), px(1.0)), px(7.0));
        assert_eq!(inner_radius(px(10.0), px(2.0)), px(8.0));
        assert_eq!(inner_radius(px(1.0), px(2.0)), px(0.0));
    }

    /// The picture's own corners are the box's inner radius (GPUI's rounded image).
    #[gpui::test]
    fn a_rounded_picture_takes_the_inner_radius(cx: &mut gpui::TestAppContext) {
        let image = testing::decoded(cx, 64, 48);
        let corner = testing::picture_corner(cx, move |_, _| {
            div()
                .size(px(64.0))
                .child(rounded_picture(
                    ImageSource::Render(image.clone()),
                    px(8.0),
                    px(1.0),
                    ObjectFit::Cover,
                ))
                .into_any_element()
        });
        assert_eq!(corner, px(7.0));
    }
}
