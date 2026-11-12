//! gpui-pixel-proof
//!   render <out.png> <scale 1|2>                                  render the fixture with GPUI, offscreen
//!   diff <reference.png> <rendered.png> <out_dir> <label> <os>    compare, write crops, print numbers
mod diff;
mod fixture;
mod view;

use anyhow::{Context as _, Result, bail};
use gpui::{AppContext as _, PlatformHeadlessRenderer, PlatformTextSystem, px, size};
use std::{path::Path, path::PathBuf, sync::Arc};

const FONT: &[u8] = include_bytes!("../fonts/JetBrainsMono-Variable-latin.ttf");

/// PROOF_LOG=1 prints GPUI's `log` output (a debugging aid).
struct Stderr;
impl log::Log for Stderr {
    fn enabled(&self, _: &log::Metadata) -> bool {
        true
    }
    fn log(&self, r: &log::Record) {
        eprintln!("[{}] {}: {}", r.level(), r.target(), r.args());
    }
    fn flush(&self) {}
}

fn main() -> Result<()> {
    if std::env::var("PROOF_LOG").is_ok() {
        let _ = log::set_logger(&Stderr).map(|()| log::set_max_level(log::LevelFilter::Debug));
    }
    let args: Vec<String> = std::env::args().skip(1).collect();
    match args.first().map(String::as_str) {
        Some("render") => {
            let out = PathBuf::from(args.get(1).context("out.png")?);
            let scale: f32 = args.get(2).context("scale")?.parse()?;
            render_headless(&out, scale)
        }
        Some("diff") => {
            let a = image::open(args.get(1).context("reference")?)?.to_rgba8();
            let b = image::open(args.get(2).context("rendered")?)?.to_rgba8();
            let dir = PathBuf::from(args.get(3).context("out_dir")?);
            let (label, os) = (args.get(4).context("label")?, args.get(5).context("os")?);
            std::fs::create_dir_all(&dir)?;
            let s = diff::compare(&a, &b)?;
            diff::write_outputs(&a, &b, &dir, label, 3, 64 * (a.width() / 1280))?;
            println!(
                "RESULT {os} {label} max_delta={} pct_gt0={:.3} pct_gt8={:.3} pct_gt32={:.3}",
                s.max_delta, s.pct_gt0, s.pct_gt8, s.pct_gt32
            );
            Ok(())
        }
        _ => bail!("usage: render <out.png> <scale> | diff <ref> <got> <dir> <label> <os>"),
    }
}

type RendererFactory = Box<dyn Fn() -> Result<Option<Box<dyn PlatformHeadlessRenderer>>>>;

/// Linux (wgpu) and macOS (Metal): the platform's own text system and headless renderer.
#[cfg(not(windows))]
fn headless_parts() -> (Arc<dyn PlatformTextSystem>, RendererFactory) {
    (
        gpui_platform::current_platform(true).text_system(),
        Box::new(gpui_platform::current_headless_renderer),
    )
}

/// Windows: GPUI's own headless platform has no text system and no renderer, so this uses the wgpu renderer with
/// cosmic-text. That is NOT DirectWrite (what the shipping Windows client uses); the report says so.
#[cfg(windows)]
fn headless_parts() -> (Arc<dyn PlatformTextSystem>, RendererFactory) {
    (
        Arc::new(gpui_wgpu::CosmicTextSystem::new("Segoe UI")),
        Box::new(|| {
            gpui_wgpu::WgpuHeadlessRenderer::new()
                .map(|r| Some(Box::new(r) as Box<dyn PlatformHeadlessRenderer>))
        }),
    )
}

/// Offscreen: GPUI's test platform with a headless renderer.
fn render_headless(out: &Path, scale: f32) -> Result<()> {
    let fx = fixture::Fixture::load();
    let (w, h) = (fx.width, fx.height);
    let (text_system, factory) = headless_parts();
    let mut cx = gpui::HeadlessAppContext::with_platform(text_system, Arc::new(()), factory);
    cx.text_system().add_fonts(vec![FONT.into()])?;
    // Headless windows are 2.0; one fixture px must be `scale` device px.
    let m = scale / 2.0;
    let window = cx.open_window(size(px(w * m), px(h * m)), |_, app| {
        app.new(|_| view::Proof { fx, m })
    })?;
    cx.run_until_parked();
    let img = cx.capture_screenshot(window.into())?;
    anyhow::ensure!(
        img.width() as f32 == w * scale && img.height() as f32 == h * scale,
        "unexpected {}x{}",
        img.width(),
        img.height()
    );
    img.save(out)?;
    println!(
        "rendered {} ({}x{})",
        out.display(),
        img.width(),
        img.height()
    );
    Ok(())
}
