//! Assets the app bundles: the web console's icon glyphs and fonts.
//!
//! Icons are the console's own glyphs (webui/web/src/components/Icon.vue),
//! not Lucide, so the two clients draw the same marks. They are kept as the
//! SVG bodies the web component wraps, and wrapped the same way here: a
//! 24×24 viewBox, 1.5 stroke, round caps and joins. A test fails when
//! Icon.vue and this table disagree.

use std::borrow::Cow;
use std::collections::HashMap;
use std::sync::OnceLock;

use gpui_kit::component::IconNamed;
use gpui_kit::{App, AssetSource, SharedString};

const ICON_PREFIX: &str = "icons/aikonos/";

macro_rules! glyphs {
    ($(($variant:ident, $name:literal, $body:literal),)*) => {
        /// An icon from the console's set. The whole set is kept, including
        /// the admin screens' glyphs, so the parity test compares like with
        /// like.
        #[allow(dead_code)]
        #[derive(Clone, Copy, Debug, PartialEq, Eq)]
        pub enum AppIcon {
            $($variant,)*
        }

        impl AppIcon {
            pub fn name(self) -> &'static str {
                match self {
                    $(AppIcon::$variant => $name,)*
                }
            }
        }

        const GLYPHS: &[(&str, &str)] = &[$(($name, $body),)*];
    };
}

glyphs! {
    (Plus, "plus", r#"<line x1="12" y1="5" x2="12" y2="19"/><line x1="5" y1="12" x2="19" y2="12"/>"#),
    (Chat, "chat", r#"<path d="M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z"/>"#),
    (Home, "home", r#"<path d="M3 9l9-7 9 7v11a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z"/><polyline points="9 22 9 12 15 12 15 22"/>"#),
    (Projects, "projects", r#"<rect x="2" y="3" width="8" height="8" rx="1"/><rect x="14" y="3" width="8" height="8" rx="1"/><rect x="2" y="14" width="8" height="8" rx="1"/><rect x="14" y="14" width="8" height="8" rx="1"/>"#),
    (Artifacts, "artifacts", r#"<polyline points="21 8 21 21 3 21 3 8"/><rect x="1" y="3" width="22" height="5"/><line x1="10" y1="12" x2="14" y2="12"/>"#),
    (Customize, "customize", r#"<circle cx="12" cy="12" r="3"/><path d="M19.07 4.93a10 10 0 0 1 1.41 13.56M4.93 4.93a10 10 0 0 0-1.41 13.56M17.66 17.66A10 10 0 0 1 6.34 17.66M6.34 6.34A10 10 0 0 0 17.66 6.34"/>"#),
    (Code, "code", r#"<polyline points="16 18 22 12 16 6"/><polyline points="8 6 2 12 8 18"/>"#),
    (Design, "design", r#"<circle cx="13.5" cy="6.5" r="3.5"/><circle cx="17.5" cy="14" r="2.5"/><circle cx="8.5" cy="14" r="3.5"/><path d="M3 3l18 18"/>"#),
    (Star, "star", r#"<polygon points="12 2 15.09 8.26 22 9.27 17 14.14 18.18 21.02 12 17.77 5.82 21.02 7 14.14 2 9.27 8.91 8.26 12 2"/>"#),
    (Search, "search", r#"<circle cx="11" cy="11" r="8"/><line x1="21" y1="21" x2="16.65" y2="16.65"/>"#),
    (Sidebar, "sidebar", r#"<rect x="3" y="3" width="18" height="18" rx="2"/><line x1="9" y1="3" x2="9" y2="21"/>"#),
    (Connections, "connections", r#"<path d="M10 13a5 5 0 0 0 7.54.54l3-3a5 5 0 0 0-7.07-7.07l-1.72 1.71"/><path d="M14 11a5 5 0 0 0-7.54-.54l-3 3a5 5 0 0 0 7.07 7.07l1.71-1.71"/>"#),
    (Files, "files", r#"<path d="M22 19a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h5l2 3h9a2 2 0 0 1 2 2z"/>"#),
    (File, "file", r#"<path d="M13 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><polyline points="13 2 13 9 20 9"/>"#),
    (Schedules, "schedules", r#"<circle cx="12" cy="12" r="10"/><polyline points="12 6 12 12 16 14"/>"#),
    (Inbox, "inbox", r#"<polyline points="22 12 16 12 14 15 10 15 8 12 2 12"/><path d="M5.45 5.11L2 12v6a2 2 0 0 0 2 2h16a2 2 0 0 0 2-2v-6l-3.45-6.89A2 2 0 0 0 16.76 4H7.24a2 2 0 0 0-1.79 1.11z"/>"#),
    (Admin, "admin", r#"<path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z"/>"#),
    (Network, "network", r#"<circle cx="12" cy="12" r="10"/><line x1="2" y1="12" x2="22" y2="12"/><path d="M12 2a15.3 15.3 0 0 1 4 10 15.3 15.3 0 0 1-4 10 15.3 15.3 0 0 1-4-10 15.3 15.3 0 0 1 4-10z"/>"#),
    (Audit, "audit", r#"<polyline points="22 12 18 12 15 21 9 3 6 12 2 12"/>"#),
    (Send, "send", r#"<line x1="22" y1="2" x2="11" y2="13"/><polygon points="22 2 15 22 11 13 2 9 22 2"/>"#),
    (Settings, "settings", r#"<circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 0 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-4 0v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 0 1-2.83-2.83l.06-.06A1.65 1.65 0 0 0 4.68 15a1.65 1.65 0 0 0-1.51-1H3a2 2 0 0 1 0-4h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 0 1 2.83-2.83l.06.06A1.65 1.65 0 0 0 9 4.68a1.65 1.65 0 0 0 1-1.51V3a2 2 0 0 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 0 1 2.83 2.83l-.06.06A1.65 1.65 0 0 0 19.4 9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 0 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z"/>"#),
    (Drive, "drive", r#"<path d="M22 12H2"/><path d="M5.45 5.11L2 12v6a2 2 0 0 0 2 2h16a2 2 0 0 0 2-2v-6l-3.45-6.89A2 2 0 0 0 16.76 4H7.24a2 2 0 0 0-1.79 1.11z"/>"#),
    (Trash, "trash", r#"<polyline points="3 6 5 6 21 6"/><path d="M19 6l-1 14H6L5 6"/><path d="M10 11v6"/><path d="M14 11v6"/><path d="M9 6V4h6v2"/>"#),
    (Download, "download", r#"<path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"/><polyline points="7 10 12 15 17 10"/><line x1="12" y1="15" x2="12" y2="3"/>"#),
    (Upload, "upload", r#"<path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"/><polyline points="17 8 12 3 7 8"/><line x1="12" y1="3" x2="12" y2="15"/>"#),
    (Pause, "pause", r#"<rect x="6" y="4" width="4" height="16"/><rect x="14" y="4" width="4" height="16"/>"#),
    (Play, "play", r#"<polygon points="5 3 19 12 5 21 5 3"/>"#),
    (Check, "check", r#"<polyline points="20 6 9 17 4 12"/>"#),
    (Close, "close", r#"<line x1="18" y1="6" x2="6" y2="18"/><line x1="6" y1="6" x2="18" y2="18"/>"#),
    (ChevronDown, "chevron-down", r#"<polyline points="6 9 12 15 18 9"/>"#),
    (Spinner, "spinner", r#"<line x1="12" y1="2" x2="12" y2="6"/><line x1="12" y1="18" x2="12" y2="22"/><line x1="4.93" y1="4.93" x2="7.76" y2="7.76"/><line x1="16.24" y1="16.24" x2="19.07" y2="19.07"/><line x1="2" y1="12" x2="6" y2="12"/><line x1="18" y1="12" x2="22" y2="12"/><line x1="4.93" y1="19.07" x2="7.76" y2="16.24"/><line x1="16.24" y1="7.76" x2="19.07" y2="4.93"/>"#),
    (Stop, "stop", r#"<rect x="6" y="6" width="12" height="12" rx="1"/>"#),
    (Users, "users", r#"<path d="M17 21v-2a4 4 0 0 0-4-4H5a4 4 0 0 0-4 4v2"/><circle cx="9" cy="7" r="4"/><path d="M23 21v-2a4 4 0 0 0-3-3.87"/><path d="M16 3.13a4 4 0 0 1 0 7.75"/>"#),
    (Tool, "tool", r#"<path d="M14.7 6.3a1 1 0 0 0 0 1.4l1.6 1.6a1 1 0 0 0 1.4 0l3.77-3.77a6 6 0 0 1-7.94 7.94l-6.91 6.91a2.12 2.12 0 0 1-3-3l6.91-6.91a6 6 0 0 1 7.94-7.94l-3.76 3.76z"/>"#),
    (Book, "book", r#"<path d="M4 19.5A2.5 2.5 0 0 1 6.5 17H20"/><path d="M6.5 2H20v20H6.5A2.5 2.5 0 0 1 4 19.5v-15A2.5 2.5 0 0 1 6.5 2z"/>"#),
    (Gauge, "gauge", r#"<path d="M4 18a8 8 0 0 1 16 0"/><line x1="12" y1="18" x2="15.5" y2="13"/><circle cx="12" cy="18" r="1"/>"#),
    (Server, "server", r#"<rect x="2" y="2" width="20" height="8" rx="2"/><rect x="2" y="14" width="20" height="8" rx="2"/><line x1="6" y1="6" x2="6.01" y2="6"/><line x1="6" y1="18" x2="6.01" y2="18"/>"#),
    (Bot, "bot", r#"<rect x="4" y="8" width="16" height="12" rx="2"/><path d="M2 14v2"/><path d="M22 14v2"/><circle cx="9" cy="14" r="1.2"/><circle cx="15" cy="14" r="1.2"/><path d="M12 4v4"/><circle cx="12" cy="3" r="1"/>"#),
    (Cpu, "cpu", r#"<rect x="4" y="4" width="16" height="16" rx="2"/><rect x="9" y="9" width="6" height="6"/><line x1="9" y1="1" x2="9" y2="4"/><line x1="15" y1="1" x2="15" y2="4"/><line x1="9" y1="20" x2="9" y2="23"/><line x1="15" y1="20" x2="15" y2="23"/><line x1="20" y1="9" x2="23" y2="9"/><line x1="20" y1="14" x2="23" y2="14"/><line x1="1" y1="9" x2="4" y2="9"/><line x1="1" y1="14" x2="4" y2="14"/>"#),
    (PlayCircle, "play-circle", r#"<circle cx="12" cy="12" r="10"/><polygon points="10 8 16 12 10 16 10 8"/>"#),
    (Scale, "scale", r#"<path d="M12 3v18"/><path d="M5 7h14"/><path d="M2 13l3-6 3 6a3 3 0 0 1-6 0z"/><path d="M16 13l3-6 3 6a3 3 0 0 1-6 0z"/><path d="M8 21h8"/>"#),
    (Shield, "shield", r#"<path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z"/>"#),
    (Copy, "copy", r#"<rect x="9" y="9" width="13" height="13" rx="2" ry="2"/><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"/>"#),
    (Reply, "reply", r#"<polyline points="9 14 4 9 9 4"/><path d="M20 20v-7a4 4 0 0 0-4-4H4"/>"#),
    (Edit, "edit", r#"<path d="M11 4H4a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2v-7"/><path d="M18.5 2.5a2.121 2.121 0 0 1 3 3L12 15l-4 1 1-4 9.5-9.5z"/>"#),
}

impl IconNamed for AppIcon {
    fn path(self) -> SharedString {
        format!("{ICON_PREFIX}{}.svg", self.name()).into()
    }
}

fn wrap_svg(body: &str) -> String {
    format!(
        r#"<svg xmlns="http://www.w3.org/2000/svg" width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round">{body}</svg>"#
    )
}

fn icon_svgs() -> &'static HashMap<&'static str, Vec<u8>> {
    static SVGS: OnceLock<HashMap<&'static str, Vec<u8>>> = OnceLock::new();
    SVGS.get_or_init(|| {
        GLYPHS
            .iter()
            .map(|(name, body)| (*name, wrap_svg(body).into_bytes()))
            .collect()
    })
}

/// The console's icons first, then GPUI Kit's own, which its components
/// (checkboxes, selects, dialogs) load by path.
pub struct AppAssets;

impl AssetSource for AppAssets {
    fn load(&self, path: &str) -> anyhow::Result<Option<Cow<'static, [u8]>>> {
        if let Some(name) = path
            .strip_prefix(ICON_PREFIX)
            .and_then(|file| file.strip_suffix(".svg"))
        {
            return Ok(icon_svgs().get(name).map(|svg| Cow::Borrowed(svg.as_slice())));
        }
        gpui_kit::assets::Assets.load(path)
    }

    fn list(&self, path: &str) -> anyhow::Result<Vec<SharedString>> {
        let mut out = gpui_kit::assets::Assets.list(path)?;
        if ICON_PREFIX.starts_with(path) || path.starts_with(ICON_PREFIX) {
            out.extend(
                GLYPHS
                    .iter()
                    .map(|(name, _)| SharedString::from(format!("{ICON_PREFIX}{name}.svg"))),
            );
        }
        Ok(out)
    }
}

/// Inter and Space Grotesk, derived from the web console's own font files
/// (webui/web/public/fonts) so text has the same outlines. SIL OFL 1.1; the
/// licences ship beside them in assets/fonts.
pub fn load_fonts(cx: &mut App) {
    let fonts: Vec<Cow<'static, [u8]>> = vec![
        Cow::Borrowed(include_bytes!("../../../assets/fonts/Inter-Regular.ttf")),
        Cow::Borrowed(include_bytes!("../../../assets/fonts/Inter-Medium.ttf")),
        Cow::Borrowed(include_bytes!("../../../assets/fonts/Inter-SemiBold.ttf")),
        Cow::Borrowed(include_bytes!("../../../assets/fonts/Inter-Bold.ttf")),
        Cow::Borrowed(include_bytes!("../../../assets/fonts/SpaceGrotesk-Bold.ttf")),
    ];
    if let Err(err) = cx.text_system().add_fonts(fonts) {
        // The system UI font takes over; the app stays usable.
        eprintln!("aikonos: could not load bundled fonts: {err:#}");
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The glyph table must match Icon.vue exactly, so a mark changed in
    /// the web console fails here until it is changed in this app too.
    #[test]
    fn glyphs_match_the_web_console() {
        let source = std::fs::read_to_string(concat!(
            env!("CARGO_MANIFEST_DIR"),
            "/../../../webui/web/src/components/Icon.vue"
        ))
        .expect("Icon.vue is readable from the repository");
        let start = source.find("const GLYPHS = {").expect("GLYPHS table");
        let end = source[start..].find("\n};").expect("end of GLYPHS") + start;
        let table = &source[start..end];
        let mut web = Vec::new();
        for line in table.lines() {
            let line = line.trim();
            let Some((key, rest)) = line.split_once(": `") else {
                continue;
            };
            let key = key.trim_matches('"');
            let body = rest.trim_end_matches(',').trim_end_matches('`');
            web.push((key.to_owned(), body.to_owned()));
        }
        let ours: Vec<(String, String)> = GLYPHS
            .iter()
            .map(|(name, body)| ((*name).to_owned(), (*body).to_owned()))
            .collect();
        assert_eq!(ours, web);
    }

    #[test]
    fn serves_icons_and_falls_back() {
        let svg = AppAssets.load("icons/aikonos/chat.svg").unwrap().unwrap();
        assert!(std::str::from_utf8(&svg).unwrap().starts_with("<svg"));
        assert!(AppAssets.load("icons/aikonos/nope.svg").unwrap().is_none());
        assert_eq!(AppIcon::Chat.path().as_ref(), "icons/aikonos/chat.svg");
    }
}
