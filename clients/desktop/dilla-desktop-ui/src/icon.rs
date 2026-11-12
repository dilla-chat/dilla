//! The monoline icon (Lucide, 24x24, stroke 2), tinted by the text colour like `currentColor` on the web.

use gpui::{App, Hsla, IntoElement, Pixels, RenderOnce, Styled, Window, px, svg};

use crate::icon_names::IconName;

/// An icon. Size defaults to 20px (the web `Icon`'s default) and the colour to the inherited text colour.
#[derive(IntoElement)]
pub struct Icon {
    name: IconName,
    size: Pixels,
    color: Option<Hsla>,
}

impl Icon {
    pub fn new(name: IconName) -> Self {
        Self {
            name,
            size: px(20.0),
            color: None,
        }
    }

    /// The rendered size; the stroke scales with it.
    pub fn size(mut self, size: Pixels) -> Self {
        self.size = size;
        self
    }

    /// The stroke colour (default: the inherited text colour).
    pub fn color(mut self, color: Hsla) -> Self {
        self.color = Some(color);
        self
    }
}

impl RenderOnce for Icon {
    fn render(self, _window: &mut Window, _cx: &mut App) -> impl IntoElement {
        let element = svg().path(self.name.path()).size(self.size).flex_none();
        match self.color {
            Some(color) => element.text_color(color),
            None => element,
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn icon_count_is_100() {
        assert_eq!(IconName::ALL.len(), 100);
    }

    #[test]
    fn names_are_unique_and_match_the_assets() {
        let mut names: Vec<_> = IconName::ALL.iter().map(|i| i.name()).collect();
        names.sort_unstable();
        names.dedup();
        assert_eq!(names.len(), 100);
        assert_eq!(IconName::Volume2.name(), "volume-2");
        assert_eq!(IconName::Volume2.path(), "icons/volume-2.svg");
    }
}
