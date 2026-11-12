//! Pixel diff: max channel delta, % of pixels differing, and the worst regions as crops.
use anyhow::{Result, ensure};
use image::{Rgba, RgbaImage};
use std::path::Path;

pub struct Stats {
    pub max_delta: u8,
    pub pct_gt0: f64,
    pub pct_gt8: f64,
    pub pct_gt32: f64,
}

pub fn compare(a: &RgbaImage, b: &RgbaImage) -> Result<Stats> {
    ensure!(
        a.dimensions() == b.dimensions(),
        "size mismatch {:?} vs {:?}",
        a.dimensions(),
        b.dimensions()
    );
    let (mut max, mut n0, mut n8, mut n32) = (0u8, 0u64, 0u64, 0u64);
    for (pa, pb) in a.pixels().zip(b.pixels()) {
        let d = channel_delta(pa, pb);
        max = max.max(d);
        n0 += u64::from(d > 0);
        n8 += u64::from(d > 8);
        n32 += u64::from(d > 32);
    }
    let total = f64::from(a.width()) * f64::from(a.height());
    Ok(Stats {
        max_delta: max,
        pct_gt0: n0 as f64 * 100.0 / total,
        pct_gt8: n8 as f64 * 100.0 / total,
        pct_gt32: n32 as f64 * 100.0 / total,
    })
}

fn channel_delta(a: &Rgba<u8>, b: &Rgba<u8>) -> u8 {
    (0..3).map(|i| a[i].abs_diff(b[i])).max().unwrap_or(0)
}

/// Writes the diff image and `top` crops of the worst `win`-pixel windows (reference | rendered | diff, 4x nearest).
pub fn write_outputs(
    a: &RgbaImage,
    b: &RgbaImage,
    dir: &Path,
    label: &str,
    top: usize,
    win: u32,
) -> Result<()> {
    let (w, h) = a.dimensions();
    let mut diff = RgbaImage::new(w, h);
    let (cw, ch) = (w.div_ceil(win), h.div_ceil(win));
    let mut score = vec![0u64; (cw * ch) as usize];
    for y in 0..h {
        for x in 0..w {
            let d = channel_delta(a.get_pixel(x, y), b.get_pixel(x, y));
            let v = (u32::from(d) * 4).min(255) as u8;
            diff.put_pixel(x, y, Rgba([v, v, v, 255]));
            if d > 8 {
                score[((y / win) * cw + x / win) as usize] += u64::from(d);
            }
        }
    }
    diff.save(dir.join(format!("{label}-diff.png")))?;
    let mut order: Vec<usize> = (0..score.len()).collect();
    order.sort_by_key(|i| std::cmp::Reverse(score[*i]));
    for (rank, cell) in order.into_iter().take(top).enumerate() {
        let (x0, y0) = ((cell as u32 % cw) * win, (cell as u32 / cw) * win);
        let (x1, y1) = ((x0 + win).min(w), (y0 + win).min(h));
        let (sw, sh) = (x1 - x0, y1 - y0);
        let zoom = 4;
        let mut out =
            RgbaImage::from_pixel((sw * 3 + 2) * zoom, sh * zoom, Rgba([40, 40, 40, 255]));
        for (panel, src) in [a, b, &diff].into_iter().enumerate() {
            for y in 0..sh * zoom {
                for x in 0..sw * zoom {
                    let px = *src.get_pixel(x0 + x / zoom, y0 + y / zoom);
                    out.put_pixel(panel as u32 * (sw + 1) * zoom + x, y, px);
                }
            }
        }
        out.save(dir.join(format!("{label}-worst{}-x{x0}y{y0}.png", rank + 1)))?;
    }
    let _ = ch;
    Ok(())
}
