//! A picture that fills a rounded box. GPUI clips a child to a rectangle only (`overflow_hidden` is a rectangular
//! content mask), so a picture in a rounded box would show square corners wherever it reaches them. The picture carries
//! the box's inner radius itself instead (its outer radius less its border), as CSS's `overflow: hidden` clips it.

use gpui::{
    AnyElement, App, Bounds, Corners, Element, ElementId, GlobalElementId, ImageSource,
    InspectorElementId, IntoElement, LayoutId, ObjectFit, Pixels, Style, StyleRefinement, Styled,
    StyledImage, Window, img, px,
};

/// The radius a picture inside a box of radius `outer` with a `border` takes: the border's inner edge (CSS's inner
/// border radius), never below zero.
pub fn inner_radius(outer: Pixels, border: Pixels) -> Pixels {
    (outer - border).max(px(0.0))
}

/// A picture filling its rounded box (`fit` in the whole box), with the box's [`inner_radius`] at each corner the
/// fitted picture reaches, as CSS's clip shows it: a picture that fills the box (any `Cover`, a `Contain` of the box's
/// aspect) is rounded at all four; a letterboxed `Contain` picture keeps its square corners inside the box, where the
/// box's own rounding is what shows (Jonas, 2026-11-26: "Square inside, like before").
pub fn rounded_picture(
    src: impl Into<ImageSource>,
    outer: Pixels,
    border: Pixels,
    fit: ObjectFit,
) -> RoundedPicture {
    RoundedPicture {
        source: src.into(),
        radius: inner_radius(outer, border),
        fit: Some(fit),
        id: None,
        corners: Corners {
            top_left: true,
            top_right: true,
            bottom_right: true,
            bottom_left: true,
        },
        style: {
            let mut s = StyleRefinement::default();
            s.size.width = Some(gpui::relative(1.0).into());
            s.size.height = Some(gpui::relative(1.0).into());
            s
        },
    }
}

/// The corners of `fitted` that reach the corners of `bounds` (both edges at that corner), within a pixel: a bar under a
/// pixel wide (a 16:9 stream in a 16:9 box inside its border) is no letterbox.
pub fn reached_corners(bounds: Bounds<Pixels>, fitted: Bounds<Pixels>) -> Corners<bool> {
    let near = |a: Pixels, b: Pixels| (a - b).abs() < px(1.0);
    let (left, right) = (
        near(fitted.left(), bounds.left()),
        near(fitted.right(), bounds.right()),
    );
    let (top, bottom) = (
        near(fitted.top(), bounds.top()),
        near(fitted.bottom(), bounds.bottom()),
    );
    Corners {
        top_left: top && left,
        top_right: top && right,
        bottom_right: bottom && right,
        bottom_left: bottom && left,
    }
}

/// See [`rounded_picture`]. It lays out as its own style says (the whole box by default); once its bounds are known
/// it draws the picture there, rounded at the corners the fitted picture reaches.
pub struct RoundedPicture {
    source: ImageSource,
    radius: Pixels,
    /// Taken as it is drawn (`ObjectFit` is not `Clone`).
    fit: Option<ObjectFit>,
    id: Option<ElementId>,
    corners: Corners<bool>,
    style: StyleRefinement,
}

impl RoundedPicture {
    /// The picture's id: it keeps an animated picture's frame clock (GPUI's `Img` animates only with one).
    pub fn id(mut self, id: impl Into<ElementId>) -> Self {
        self.id = Some(id.into());
        self
    }

    /// Rounds only the top corners: the picture is at the top of its box (a tray card's preview over its progress
    /// line), so its bottom corners are not the box's.
    pub fn top_only(mut self) -> Self {
        self.corners.bottom_left = false;
        self.corners.bottom_right = false;
        self
    }

    /// The picture's pixel size, once it is decoded (`None` while it loads: nothing is drawn then).
    fn natural_size(
        &self,
        window: &mut Window,
        cx: &mut App,
    ) -> Option<gpui::Size<gpui::DevicePixels>> {
        match &self.source {
            ImageSource::Render(r) => Some(r.size(0)),
            ImageSource::Image(i) => i.clone().get_render_image(window, cx).map(|r| r.size(0)),
            _ => None,
        }
    }
}

impl Styled for RoundedPicture {
    fn style(&mut self) -> &mut StyleRefinement {
        &mut self.style
    }
}

impl IntoElement for RoundedPicture {
    type Element = Self;

    fn into_element(self) -> Self::Element {
        self
    }
}

impl Element for RoundedPicture {
    type RequestLayoutState = ();
    type PrepaintState = Option<AnyElement>;

    fn id(&self) -> Option<ElementId> {
        None
    }

    fn source_location(&self) -> Option<&'static core::panic::Location<'static>> {
        None
    }

    fn request_layout(
        &mut self,
        _: Option<&GlobalElementId>,
        _: Option<&InspectorElementId>,
        window: &mut Window,
        cx: &mut App,
    ) -> (LayoutId, ()) {
        let mut style = Style::default();
        gpui::Refineable::refine(&mut style, &self.style);
        (window.request_layout(style, [], cx), ())
    }

    fn prepaint(
        &mut self,
        _: Option<&GlobalElementId>,
        _: Option<&InspectorElementId>,
        bounds: Bounds<Pixels>,
        _: &mut (),
        window: &mut Window,
        cx: &mut App,
    ) -> Option<AnyElement> {
        let fit = self.fit.take().unwrap_or(ObjectFit::Fill);
        let reached = match self.natural_size(window, cx) {
            Some(size) if size.width.0 > 0 && size.height.0 > 0 => {
                // What shows is the fitted picture within the box (a `Cover` one overflows it).
                reached_corners(bounds, fit.get_bounds(bounds, size).intersect(&bounds))
            }
            _ => Corners::default(),
        };
        let r = |reaches: bool, allowed: bool| {
            if reaches && allowed {
                self.radius
            } else {
                px(0.0)
            }
        };
        let picture = img(self.source.clone())
            .size_full()
            .object_fit(fit)
            .rounded_tl(r(reached.top_left, self.corners.top_left))
            .rounded_tr(r(reached.top_right, self.corners.top_right))
            .rounded_br(r(reached.bottom_right, self.corners.bottom_right))
            .rounded_bl(r(reached.bottom_left, self.corners.bottom_left));
        let mut picture = match self.id.clone() {
            Some(id) => gpui::InteractiveElement::id(picture, id).into_any_element(),
            None => picture.into_any_element(),
        };
        picture.prepaint_as_root(bounds.origin, bounds.size.into(), window, cx);
        Some(picture)
    }

    fn paint(
        &mut self,
        _: Option<&GlobalElementId>,
        _: Option<&InspectorElementId>,
        _: Bounds<Pixels>,
        _: &mut (),
        picture: &mut Option<AnyElement>,
        window: &mut Window,
        cx: &mut App,
    ) {
        if let Some(p) = picture {
            p.paint(window, cx);
        }
    }
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
    use gpui::{ParentElement, div};

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

    /// "Square inside, like before" (Jonas, 2026-11-26): a `Contain` picture is rounded only at the corners it reaches.
    /// A 4:3 picture in a 16:9 box is letterboxed (bars at its sides) and reaches none: all four are square, and the
    /// box's own rounding is what shows. One of the box's aspect fills it and takes the exact inner radius.
    #[gpui::test]
    fn a_letterboxed_picture_keeps_square_corners(cx: &mut gpui::TestAppContext) {
        let in_box = |image: std::sync::Arc<gpui::RenderImage>| {
            move |_: &mut Window, _: &mut App| {
                div()
                    .w(px(160.0))
                    .h(px(90.0))
                    .child(rounded_picture(
                        ImageSource::Render(image.clone()),
                        px(8.0),
                        px(1.0),
                        ObjectFit::Contain,
                    ))
                    .into_any_element()
            }
        };
        let four_three = testing::decoded(cx, 80, 60);
        let c = *testing::picture_corners(cx, in_box(four_three))
            .last()
            .expect("the picture");
        assert_eq!(c, Corners::all(px(0.0)), "letterboxed: square");
        let sixteen_nine = testing::decoded(cx, 160, 90);
        let c = *testing::picture_corners(cx, in_box(sixteen_nine))
            .last()
            .expect("the picture");
        assert_eq!(c, Corners::all(px(7.0)), "filling: the inner radius");
    }

    /// The corners a fitted picture reaches: all when it fills the box, none when it is letterboxed, two when it sits
    /// against one side.
    #[test]
    fn the_reached_corners_are_where_the_picture_meets_both_edges() {
        let b = |x: f32, y: f32, w: f32, h: f32| Bounds {
            origin: gpui::point(px(x), px(y)),
            size: gpui::size(px(w), px(h)),
        };
        let all = Corners {
            top_left: true,
            top_right: true,
            bottom_right: true,
            bottom_left: true,
        };
        assert_eq!(
            reached_corners(b(0., 0., 160., 90.), b(0., 0., 160., 90.)),
            all
        );
        assert_eq!(
            reached_corners(b(0., 0., 160., 90.), b(20., 0., 120., 90.)),
            Corners::default()
        );
        assert_eq!(
            reached_corners(b(0., 0., 160., 90.), b(0., 0., 120., 90.)),
            Corners {
                top_left: true,
                bottom_left: true,
                ..Corners::default()
            }
        );
    }
}
