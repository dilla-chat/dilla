//! Fonts and assets: what an app needs registered before the Signal components draw correctly.
//!
//! `Application::with_assets(theme::Assets)` makes the icon SVGs loadable, and `theme::install(cx)` registers the two fonts.

use std::borrow::Cow;

use gpui::{App, AssetSource, Result, SharedString};

use crate::icon_names::IconName;

/// JetBrains Mono Variable (OFL-1.1, assets/fonts/README.md).
static JETBRAINS_MONO: &[u8] = include_bytes!("../assets/fonts/JetBrainsMono[wght].ttf");
/// DM Serif Display Italic (OFL-1.1, assets/fonts/README.md).
static DM_SERIF_DISPLAY_ITALIC: &[u8] = include_bytes!("../assets/fonts/DMSerifDisplay-Italic.ttf");

/// The fonts this crate embeds, as GPUI takes them.
fn fonts() -> Vec<Cow<'static, [u8]>> {
    vec![
        Cow::Borrowed(JETBRAINS_MONO),
        Cow::Borrowed(DM_SERIF_DISPLAY_ITALIC),
    ]
}

/// Registers the fonts with GPUI's text system. Call once at start-up, before the first window opens.
///
/// # Panics
/// When an embedded font does not parse, which the `fonts_parse` test rules out.
pub fn install(cx: &mut App) {
    cx.text_system()
        .add_fonts(fonts())
        .expect("the embedded fonts are valid TrueType");
}

/// The icon SVGs, served from the binary under `icons/<name>.svg`.
pub struct Assets;

impl AssetSource for Assets {
    fn load(&self, path: &str) -> Result<Option<Cow<'static, [u8]>>> {
        Ok(IconName::ALL
            .into_iter()
            .find(|icon| icon.path() == path)
            .map(|icon| Cow::Borrowed(icon.svg())))
    }

    fn list(&self, path: &str) -> Result<Vec<SharedString>> {
        Ok(IconName::ALL
            .into_iter()
            .filter(|icon| icon.path().starts_with(path))
            .map(|icon| SharedString::from(icon.path()))
            .collect())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn assets_serve_every_icon() {
        for icon in IconName::ALL {
            let svg = Assets.load(icon.path()).unwrap().expect("icon is served");
            let text = std::str::from_utf8(&svg).unwrap();
            assert!(
                text.contains("stroke=\"currentColor\"") && text.contains("stroke-width=\"2\""),
                "{}",
                icon.name()
            );
        }
        assert!(Assets.load("icons/nope.svg").unwrap().is_none());
        assert_eq!(Assets.list("icons/").unwrap().len(), 100);
    }

    #[test]
    fn fonts_are_ttf() {
        for font in fonts() {
            assert_eq!(
                &font[..4],
                &[0, 1, 0, 0],
                "a TrueType font starts with the sfnt version 1.0"
            );
        }
    }
}
