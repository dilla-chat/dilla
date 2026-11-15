//! `QrCode` (packages/ui/src/QrCode): the QR code of a value (medium error correction, any version, automatic mask), a
//! 4-module quiet zone, drawn from the dark modules. The fills are the web's: `--surface` behind, `--fg` for the modules.

use gpui::{
    App, Bounds, ElementId, InteractiveElement, IntoElement, ParentElement, Pixels, RenderOnce,
    Styled, Window, canvas, div, fill, point, px, size,
};
use qrcodegen::{QrCode as Code, QrCodeEcc, QrSegment, Version};

use crate::tokens::color;

/// `QUIET_ZONE` in QrCode.tsx.
pub const QUIET_ZONE: usize = 4;

/// The code's modules: its side and the dark ones in row-major order. `None` when the value is too long for any version.
pub fn modules(value: &str) -> Option<(usize, Vec<bool>)> {
    let code = Code::encode_segments_advanced(
        &QrSegment::make_segments(value),
        QrCodeEcc::Medium,
        Version::MIN,
        Version::MAX,
        None,
        false,
    )
    .ok()?;
    let side = usize::try_from(code.size()).ok()?;
    let dark = (0..side * side)
        .map(|i| code.get_module((i % side) as i32, (i / side) as i32))
        .collect();
    Some((side, dark))
}

#[derive(IntoElement)]
pub struct QrCode {
    id: ElementId,
    value: String,
    size: Pixels,
}

impl QrCode {
    /// `size` defaults to 192px, as the web's.
    pub fn new(id: impl Into<ElementId>, value: impl Into<String>) -> Self {
        Self {
            id: id.into(),
            value: value.into(),
            size: px(192.0),
        }
    }

    pub fn size(mut self, size: Pixels) -> Self {
        self.size = size;
        self
    }
}

impl RenderOnce for QrCode {
    fn render(self, _window: &mut Window, _cx: &mut App) -> impl IntoElement {
        let side_px = self.size;
        let code = modules(&self.value);
        let paint = canvas(
            |_, _, _| (),
            move |bounds: Bounds<Pixels>, (), window, _cx| {
                window.paint_quad(fill(bounds, color::SURFACE));
                let Some((side, dark)) = &code else { return };
                let total = (side + 2 * QUIET_ZONE) as f32;
                let module = f32::from(bounds.size.width) / total;
                for y in 0..*side {
                    // Horizontal runs of dark modules are one quad each, so no seam shows between neighbours.
                    let mut x = 0;
                    while x < *side {
                        if !dark[y * side + x] {
                            x += 1;
                            continue;
                        }
                        let start = x;
                        while x < *side && dark[y * side + x] {
                            x += 1;
                        }
                        let origin = point(
                            bounds.origin.x + px((start + QUIET_ZONE) as f32 * module),
                            bounds.origin.y + px((y + QUIET_ZONE) as f32 * module),
                        );
                        window.paint_quad(fill(
                            Bounds::new(origin, size(px((x - start) as f32 * module), px(module))),
                            color::FG,
                        ));
                    }
                }
            },
        )
        .size_full();
        div().id(self.id).flex_none().size(side_px).child(paint)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_short_value_is_a_version_one_code_with_finder_patterns() {
        let (side, dark) = modules("hi").expect("it fits");
        assert_eq!(side, 21);
        // The top-left finder pattern: a dark ring, a light ring, a dark 3x3 core.
        assert!(dark[0] && dark[6] && dark[6 * side] && dark[2 * side + 2]);
        assert!(!dark[side + 1]);
    }

    #[test]
    fn a_value_beyond_every_version_has_no_code() {
        assert!(modules(&"x".repeat(4000)).is_none());
    }

    #[test]
    fn the_code_is_deterministic() {
        assert_eq!(modules("otpauth://totp/x"), modules("otpauth://totp/x"));
    }
}
