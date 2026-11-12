//! The fixture, shared with the Chromium side through fixture.json (one source of truth).
use serde::Deserialize;
use std::collections::HashMap;

#[derive(Deserialize)]
pub struct Fixture {
    pub width: f32,
    pub height: f32,
    pub tokens: HashMap<String, String>,
    pub font: Font,
    pub radius: f32,
    pub authors: Vec<Author>,
    pub messages: Vec<Message>,
    pub layout: Layout,
    /// Negative fixtures only: draw the box borders this many fixture px lower (0 in the real fixture).
    #[serde(skip)]
    pub border_dy: f32,
}
#[derive(Deserialize)]
pub struct Font {
    pub family: String,
    pub base: f32,
    pub small: f32,
    pub code: f32,
    pub pill: f32,
}
#[derive(Deserialize)]
pub struct Author {
    pub name: String,
    pub initials: String,
}
#[derive(Deserialize)]
pub struct Message {
    pub author: usize,
    pub time: String,
    pub lines: Vec<String>,
    #[serde(default)]
    pub code: Vec<String>,
    #[serde(default)]
    pub reactions: Vec<(String, String)>,
}
#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Layout {
    pub outer_pad: f32,
    pub panel_pad: f32,
    pub row_pad_y: f32,
    pub avatar: f32,
    pub row_gap: f32,
    pub line_h: f32,
    pub head_gap: f32,
    pub code_margin_top: f32,
    pub code_line_h: f32,
    pub code_pad_y: f32,
    pub code_pad_x: f32,
    pub code_w: f32,
    pub react_margin_top: f32,
    pub react_gap: f32,
    pub pill_h: f32,
    pub pill_pad_x: f32,
}

impl Fixture {
    /// Deliberately wrong fixtures that the tolerance must reject (see REPORT.md, "Validating the tolerance").
    pub fn apply_variant(&mut self, variant: &str) {
        match variant {
            "good" => {}
            // (a) message and code-block text drawn in fg2 instead of fgBody
            "wrong-colour" => {
                let v = self.tokens["fg2"].clone();
                self.tokens.insert("fgBody".into(), v);
            }
            // (b1) only the panel and code-block border lines one px lower; every size and position stays
            "border-shift" => self.border_dy = 1.0,
            // (b2) the panel and code block moved or resized by 1px (a layout shift, text and pills included)
            "layout-shift" => {
                self.layout.outer_pad += 1.0;
                self.layout.panel_pad -= 1.0;
                self.layout.code_margin_top += 1.0;
                self.layout.code_pad_y -= 1.0;
            }
            // (c) a fallback monospace family instead of JetBrains Mono
            "fallback-font" => self.font.family = "DejaVu Sans Mono".into(),
            other => panic!("unknown variant {other}"),
        }
    }

    pub fn load() -> Self {
        serde_json::from_str(include_str!("../fixture.json")).expect("fixture.json parses")
    }
    /// `#RRGGBB` token to a packed 0xRRGGBB.
    pub fn rgb(&self, key: &str) -> u32 {
        let hex = self
            .tokens
            .get(key)
            .unwrap_or_else(|| panic!("token {key}"));
        u32::from_str_radix(hex.trim_start_matches('#'), 16).expect("hex token")
    }
}
