//! Glyph presence: does a frame show ink inside the line boxes the app says it painted? A build whose text system draws
//! nothing (gpui-pre-macos without `font-kit` installs `NoopTextSystem`; found by hand on 2026-11-16) still lays its lines
//! out and reports their boxes, but every box is one flat colour.

use std::collections::HashMap;

use image::RgbaImage;

use crate::ElementInfo;

/// A pixel is ink when its largest channel differs from its line box's most common colour by more than this.
pub const INK_DELTA: u8 = 48;
/// A line is inked when at least this share (percent) of its box's pixels is ink.
pub const INKED_LINE_PCT: f32 = 2.0;

/// What [`glyph_coverage`] measured.
#[derive(Clone, Copy, Debug, PartialEq)]
pub struct GlyphCoverage {
    /// Line boxes with at least one device pixel inside the frame.
    pub lines: usize,
    /// Of those, the boxes whose ink share is at least [`INKED_LINE_PCT`].
    pub inked_lines: usize,
    /// The ink share over all the boxes' pixels, percent.
    pub ink_pct: f32,
}

/// For each text-bounds line box (`lines`, logical px, scaled by `scale` to the frame's device px), the share of pixels
/// whose largest channel differs from the box's most common colour by more than [`INK_DELTA`]. A line is inked when that
/// share is at least [`INKED_LINE_PCT`].
pub fn glyph_coverage(shot: &RgbaImage, lines: &[ElementInfo], scale: f32) -> GlyphCoverage {
    let (width, height) = shot.dimensions();
    let (mut counted, mut inked, mut ink_px, mut all_px) = (0usize, 0usize, 0u64, 0u64);
    for line in lines {
        let x0 = ((line.x * scale).floor().max(0.0) as u32).min(width);
        let y0 = ((line.y * scale).floor().max(0.0) as u32).min(height);
        let x1 = (((line.x + line.width) * scale).ceil().max(0.0) as u32).min(width);
        let y1 = (((line.y + line.height) * scale).ceil().max(0.0) as u32).min(height);
        if x1 <= x0 || y1 <= y0 {
            continue;
        }
        let mut counts: HashMap<[u8; 3], u32> = HashMap::new();
        for y in y0..y1 {
            for x in x0..x1 {
                let p = shot.get_pixel(x, y).0;
                *counts.entry([p[0], p[1], p[2]]).or_default() += 1;
            }
        }
        let Some(background) = counts
            .iter()
            .max_by_key(|(colour, n)| (**n, **colour))
            .map(|(c, _)| *c)
        else {
            continue;
        };
        let ink: u64 = counts
            .iter()
            .filter(|(c, _)| {
                (0..3)
                    .map(|i| c[i].abs_diff(background[i]))
                    .max()
                    .unwrap_or(0)
                    > INK_DELTA
            })
            .map(|(_, n)| u64::from(*n))
            .sum();
        let area = u64::from(x1 - x0) * u64::from(y1 - y0);
        counted += 1;
        if ink as f32 * 100.0 >= INKED_LINE_PCT * area as f32 {
            inked += 1;
        }
        ink_px += ink;
        all_px += area;
    }
    GlyphCoverage {
        lines: counted,
        inked_lines: inked,
        ink_pct: if all_px == 0 {
            0.0
        } else {
            ink_px as f32 * 100.0 / all_px as f32
        },
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use image::Rgba;

    fn line(x: f32, y: f32, width: f32, height: f32) -> ElementInfo {
        ElementInfo {
            id: "text".into(),
            x,
            y,
            width,
            height,
        }
    }

    #[test]
    fn a_flat_box_is_not_inked_and_a_glyph_is() {
        let mut shot = RgbaImage::from_pixel(40, 20, Rgba([10, 12, 16, 255]));
        // A 2x10 stroke in the left box (logical 0,0 10x10 at scale 2 is 20x20 device px: 20/400 = 5 %).
        for y in 5..15 {
            for x in 4..6 {
                shot.put_pixel(x, y, Rgba([230, 230, 230, 255]));
            }
        }
        // A faint stroke in the right box (delta 20, under INK_DELTA): not ink.
        for y in 5..15 {
            shot.put_pixel(30, y, Rgba([30, 32, 36, 255]));
        }
        let c = glyph_coverage(
            &shot,
            &[line(0.0, 0.0, 10.0, 10.0), line(10.0, 0.0, 10.0, 10.0)],
            2.0,
        );
        assert_eq!((c.lines, c.inked_lines), (2, 1));
        assert!((c.ink_pct - 20.0 * 100.0 / 800.0).abs() < 1e-3, "{c:?}");
    }

    #[test]
    fn empty_and_off_frame_boxes_are_not_lines() {
        let shot = RgbaImage::from_pixel(10, 10, Rgba([0, 0, 0, 255]));
        let c = glyph_coverage(
            &shot,
            &[line(1.0, 1.0, 0.0, 3.0), line(20.0, 20.0, 4.0, 4.0)],
            2.0,
        );
        assert_eq!((c.lines, c.inked_lines, c.ink_pct), (0, 0, 0.0));
    }
}
