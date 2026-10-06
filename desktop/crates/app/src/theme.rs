//! The console's look, as a GPUI Kit theme.
//!
//! Colors are the web console's tokens (webui/web/src/styles/tokens.css),
//! mapped onto GPUI Kit's semantic roles in `aikonos-theme.json`, so stock
//! components (inputs, menus, dialogs, notifications) already read as the
//! console. Roles the console has and GPUI Kit does not (faint text, the
//! translucent fills) live in [`ConsoleTokens`]. This file and the JSON are
//! the only places raw colors appear.

use std::rc::Rc;

use gpui_kit::component::{Theme, ThemeConfig, ThemeMode, ThemeRegistry};
use gpui_kit::{App, Global, Hsla, Window, rgb, rgba};
use serde::{Deserialize, Serialize};

const THEME_SET: &str = include_str!("../../../assets/aikonos-theme.json");
const LIGHT: &str = "aikonOS Light";
const DARK: &str = "aikonOS Dark";

/// The user's choice in Settings. The console offers the same three.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Appearance {
    #[default]
    Dark,
    Light,
    System,
}

/// Console roles with no GPUI Kit equivalent.
#[derive(Clone, Copy, Debug)]
pub struct ConsoleTokens {
    /// `--accent-text`: the accent as text, icons and lines. `primary` is the
    /// yellow fill, which is unreadable as text on the light theme.
    pub accent_text: Hsla,
    /// `--text-faint`: placeholders, queue counters.
    pub text_faint: Hsla,
    /// `--fill-muted`: neutral pills.
    pub fill_muted: Hsla,
    /// `--fill-accent`: step-up pill, drop targets.
    pub fill_accent: Hsla,
    /// `--fill-danger`: error banners.
    pub fill_danger: Hsla,
    /// `--fill-ok`: success banners.
    pub fill_ok: Hsla,
}

impl Global for ConsoleTokens {}

impl ConsoleTokens {
    fn dark() -> Self {
        Self {
            accent_text: rgb(0xffcc00).into(),
            text_faint: rgb(0x666666).into(),
            fill_muted: rgba(0x80808026).into(),
            fill_accent: rgba(0xffcc001a).into(),
            fill_danger: rgba(0xe5675f1f).into(),
            fill_ok: rgba(0x6cba6c1f).into(),
        }
    }

    fn light() -> Self {
        Self {
            accent_text: rgb(0x7d6400).into(),
            text_faint: rgb(0x888888).into(),
            ..Self::dark()
        }
    }
}

/// Read the console-only roles: `console(cx).text_faint`.
pub fn console(cx: &App) -> ConsoleTokens {
    *cx.global::<ConsoleTokens>()
}

struct ThemeConfigs {
    light: Rc<ThemeConfig>,
    dark: Rc<ThemeConfig>,
}

impl Global for ThemeConfigs {}

/// Register the console themes and apply `appearance`. Call once, after
/// `gpui_kit::init`.
pub fn init(appearance: Appearance, cx: &mut App) {
    ThemeRegistry::global_mut(cx)
        .load_themes_from_str(THEME_SET)
        .expect("the bundled theme parses");
    let registry = ThemeRegistry::global(cx);
    let light = registry.themes().get(LIGHT).cloned().expect("light theme registered");
    let dark = registry.themes().get(DARK).cloned().expect("dark theme registered");
    cx.set_global(ThemeConfigs { light, dark });
    cx.set_global(ConsoleTokens::dark());
    apply(appearance, None, cx);
}

/// Switch light, dark or system. `window` resolves the system appearance.
pub fn apply(appearance: Appearance, window: Option<&mut Window>, cx: &mut App) {
    let mode = match appearance {
        Appearance::Dark => ThemeMode::Dark,
        Appearance::Light => ThemeMode::Light,
        Appearance::System => {
            let system = window
                .as_ref()
                .map(|window| window.appearance())
                .unwrap_or_else(|| cx.window_appearance());
            ThemeMode::from(system)
        }
    };
    let configs = cx.global::<ThemeConfigs>();
    let (light, dark) = (configs.light.clone(), configs.dark.clone());
    {
        let theme = Theme::global_mut(cx);
        theme.light_theme = light;
        theme.dark_theme = dark;
    }
    Theme::change(mode, None, cx);
    cx.set_global(if mode.is_dark() {
        ConsoleTokens::dark()
    } else {
        ConsoleTokens::light()
    });
    cx.refresh_windows();
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn theme_set_defines_both_modes() {
        let set: serde_json::Value = serde_json::from_str(THEME_SET).unwrap();
        let names: Vec<_> = set["themes"]
            .as_array()
            .unwrap()
            .iter()
            .map(|theme| theme["name"].as_str().unwrap().to_owned())
            .collect();
        assert_eq!(names, [LIGHT, DARK]);
    }

    fn console_css() -> String {
        std::fs::read_to_string(concat!(
            env!("CARGO_MANIFEST_DIR"),
            "/../../../webui/web/src/styles/tokens.css"
        ))
        .unwrap()
        .to_ascii_lowercase()
    }

    /// The value tokens.css gives `token` in `mode`: the light block's own,
    /// else the root block's, which the light theme inherits.
    fn css_token(css: &str, mode: &str, token: &str) -> String {
        let (root, rest) = css.split_once(":root[data-theme=\"light\"]").unwrap();
        let light = rest.split('}').next().unwrap();
        let find = |block: &str| {
            block.lines().map(str::trim_start).find_map(|line| {
                let value = line.strip_prefix(token)?.strip_prefix(':')?;
                Some(value.trim().trim_end_matches(';').trim().to_owned())
            })
        };
        let value = if mode == "light" {
            find(light).or_else(|| find(root))
        } else {
            find(root)
        };
        value.unwrap_or_else(|| panic!("tokens.css has no {token}"))
    }

    #[test]
    fn theme_colors_follow_the_console_tokens() {
        let css = console_css();
        let set: serde_json::Value = serde_json::from_str(THEME_SET).unwrap();
        // A token changed in the console must be changed here too.
        for theme in set["themes"].as_array().unwrap() {
            let mode = theme["mode"].as_str().unwrap();
            for (role, token) in [
                ("background", "--bg"),
                ("foreground", "--text"),
                ("sidebar.background", "--bg-sidebar"),
                ("popover.background", "--bg-elevated"),
                ("accent.background", "--bg-hover"),
                ("list.active.background", "--bg-active"),
                ("border", "--border"),
                ("muted.foreground", "--text-muted"),
                ("primary.background", "--accent"),
                ("primary.hover.background", "--accent-hover"),
                ("primary.foreground", "--text-on-accent"),
                ("link", "--accent-text"),
                ("ring", "--accent-text"),
                ("danger.background", "--danger"),
                ("danger.foreground", "--text-on-status"),
                ("success.background", "--ok"),
                ("success.foreground", "--text-on-status"),
            ] {
                let color = theme["colors"][role].as_str().unwrap().to_ascii_lowercase();
                let value = css_token(&css, mode, token);
                assert_eq!(
                    color, value,
                    "{mode} {role} is {color}, tokens.css says {token}: {value}"
                );
            }
        }
    }

    #[test]
    fn console_tokens_follow_the_console_tokens() {
        let css = console_css();
        let color = |mode: &str, token: &str| -> Hsla {
            let hex = css_token(&css, mode, token);
            rgb(u32::from_str_radix(hex.trim_start_matches('#'), 16).unwrap()).into()
        };
        for (mode, tokens) in [("dark", ConsoleTokens::dark()), ("light", ConsoleTokens::light())] {
            assert_eq!(tokens.accent_text, color(mode, "--accent-text"), "{mode} accent_text");
            assert_eq!(tokens.text_faint, color(mode, "--text-faint"), "{mode} text_faint");
        }
    }
}
