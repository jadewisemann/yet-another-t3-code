//! Colors and fonts. The built-in palette leans on warm neutrals with a terracotta accent,
//! close to the Claude desktop app. `theme.json` in the config folder overrides any of it:
//!
//! ```json
//! {
//!   "mode": "auto",
//!   "uiFont": "Inter",
//!   "messageFont": "Georgia",
//!   "fontSize": 14,
//!   "light": { "accent": "#c96442" },
//!   "dark": { "background": "#1f1e1d" }
//! }
//! ```

use gpui::{App, Global, Hsla, Window, WindowAppearance};
use serde::Deserialize;

use crate::config;

#[derive(Clone, Copy, Debug)]
pub struct Palette {
    pub background: Hsla,
    pub sidebar: Hsla,
    pub surface: Hsla,
    pub border: Hsla,
    pub text: Hsla,
    pub muted: Hsla,
    pub accent: Hsla,
    pub accent_text: Hsla,
    pub selection: Hsla,
    pub row_hover: Hsla,
    pub row_selected: Hsla,
    pub user_bubble: Hsla,
    pub danger: Hsla,
    pub working: Hsla,
}

fn hex(value: u32) -> Hsla {
    gpui::rgb(value).into()
}

fn hexa(value: u32) -> Hsla {
    gpui::rgba(value).into()
}

impl Palette {
    pub fn light() -> Self {
        Self {
            background: hex(0xfaf9f5),
            sidebar: hex(0xf0eee6),
            surface: hex(0xffffff),
            border: hex(0xe3dfd4),
            text: hex(0x1f1e1d),
            muted: hex(0x73716b),
            accent: hex(0xc96442),
            accent_text: hex(0xffffff),
            selection: hexa(0xc964423a),
            row_hover: hexa(0x1f1e1d0d),
            row_selected: hexa(0x1f1e1d17),
            user_bubble: hex(0xf0eee6),
            danger: hex(0xb3261e),
            working: hex(0xc96442),
        }
    }

    pub fn dark() -> Self {
        Self {
            background: hex(0x262624),
            sidebar: hex(0x1f1e1d),
            surface: hex(0x30302e),
            border: hex(0x3d3c39),
            text: hex(0xf3f1ea),
            muted: hex(0xa39f95),
            accent: hex(0xd97757),
            accent_text: hex(0x1f1e1d),
            selection: hexa(0xd977574d),
            row_hover: hexa(0xffffff0d),
            row_selected: hexa(0xffffff17),
            user_bubble: hex(0x3a3936),
            danger: hex(0xf2b8b5),
            working: hex(0xd97757),
        }
    }

    fn apply(&mut self, overrides: &PaletteOverrides) {
        let fields: [(&Option<String>, &mut Hsla); 14] = [
            (&overrides.background, &mut self.background),
            (&overrides.sidebar, &mut self.sidebar),
            (&overrides.surface, &mut self.surface),
            (&overrides.border, &mut self.border),
            (&overrides.text, &mut self.text),
            (&overrides.muted, &mut self.muted),
            (&overrides.accent, &mut self.accent),
            (&overrides.accent_text, &mut self.accent_text),
            (&overrides.selection, &mut self.selection),
            (&overrides.row_hover, &mut self.row_hover),
            (&overrides.row_selected, &mut self.row_selected),
            (&overrides.user_bubble, &mut self.user_bubble),
            (&overrides.danger, &mut self.danger),
            (&overrides.working, &mut self.working),
        ];
        for (value, slot) in fields {
            if let Some(color) = value.as_deref().and_then(parse_color) {
                *slot = color;
            }
        }
    }
}

/// `#rgb`, `#rrggbb` or `#rrggbbaa`.
pub fn parse_color(text: &str) -> Option<Hsla> {
    let digits = text.trim().trim_start_matches('#');
    let expanded = match digits.len() {
        3 => digits.chars().flat_map(|c| [c, c]).collect::<String>() + "ff",
        6 => format!("{digits}ff"),
        8 => digits.to_string(),
        _ => return None,
    };
    let value = u32::from_str_radix(&expanded, 16).ok()?;
    Some(gpui::rgba(value).into())
}

#[derive(Default, Deserialize)]
#[serde(rename_all = "camelCase")]
struct PaletteOverrides {
    background: Option<String>,
    sidebar: Option<String>,
    surface: Option<String>,
    border: Option<String>,
    text: Option<String>,
    muted: Option<String>,
    accent: Option<String>,
    accent_text: Option<String>,
    selection: Option<String>,
    row_hover: Option<String>,
    row_selected: Option<String>,
    user_bubble: Option<String>,
    danger: Option<String>,
    working: Option<String>,
}

#[derive(Default, Deserialize)]
#[serde(rename_all = "camelCase")]
struct ThemeFile {
    mode: Option<String>,
    ui_font: Option<String>,
    message_font: Option<String>,
    font_size: Option<f32>,
    #[serde(default)]
    light: PaletteOverrides,
    #[serde(default)]
    dark: PaletteOverrides,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Mode {
    Auto,
    Light,
    Dark,
}

pub struct Theme {
    pub mode: Mode,
    pub light: Palette,
    pub dark: Palette,
    pub ui_font: Option<String>,
    pub message_font: Option<String>,
    pub font_size: f32,
    pub load_error: Option<String>,
}

impl Global for Theme {}

impl Theme {
    pub fn load() -> Self {
        let mut theme = Theme {
            mode: Mode::Auto,
            light: Palette::light(),
            dark: Palette::dark(),
            ui_font: None,
            // Claude-style serif for conversation text, where the platform ships one.
            message_font: cfg!(target_os = "macos").then(|| "Georgia".to_string()),
            font_size: 14.,
            load_error: None,
        };
        let path = config::config_file("theme.json");
        let Ok(text) = std::fs::read_to_string(&path) else {
            return theme;
        };
        match serde_json::from_str::<ThemeFile>(&text) {
            Ok(file) => {
                theme.mode = match file.mode.as_deref() {
                    Some("light") => Mode::Light,
                    Some("dark") => Mode::Dark,
                    _ => Mode::Auto,
                };
                theme.light.apply(&file.light);
                theme.dark.apply(&file.dark);
                if file.ui_font.is_some() {
                    theme.ui_font = file.ui_font;
                }
                if file.message_font.is_some() {
                    theme.message_font = file.message_font.filter(|f| !f.is_empty());
                }
                if let Some(size) = file.font_size.filter(|s| (8.0..=32.0).contains(s)) {
                    theme.font_size = size;
                }
            }
            Err(error) => theme.load_error = Some(format!("theme.json: {error}")),
        }
        theme
    }

    pub fn palette(&self, window: &Window) -> Palette {
        let dark = match self.mode {
            Mode::Light => false,
            Mode::Dark => true,
            Mode::Auto => matches!(
                window.appearance(),
                WindowAppearance::Dark | WindowAppearance::VibrantDark
            ),
        };
        if dark { self.dark } else { self.light }
    }
}

pub fn palette(window: &Window, cx: &App) -> Palette {
    cx.global::<Theme>().palette(window)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_hex_colors() {
        assert!(parse_color("#fff").is_some());
        assert!(parse_color("c96442").is_some());
        assert!(parse_color("#c964423a").is_some());
        assert!(parse_color("#12").is_none());
        let white = parse_color("#ffffff").unwrap();
        assert!((white.l - 1.0).abs() < 0.001);
    }
}
