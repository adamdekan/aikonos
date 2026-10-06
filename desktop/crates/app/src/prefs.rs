//! Preferences kept on this machine, the desktop equivalent of what the
//! web console keeps in localStorage. Never tokens: those stay in memory.
//!
//! Stored as JSON in `%APPDATA%\aikonOS\desktop.json`. A missing or
//! unreadable file means defaults; a failed write is reported once and the
//! app carries on with the in-memory values.

use std::path::PathBuf;

use gpui_kit::{App, Global};
use serde::{Deserialize, Serialize};

use crate::theme::Appearance;

/// The web console caps standing instructions at this many characters
/// (store/prefs.js `MAX_CHAT_INSTRUCTIONS_CHARS`).
pub const MAX_CHAT_INSTRUCTIONS_CHARS: usize = 2000;

#[derive(Clone, Debug, Serialize, Deserialize, PartialEq)]
#[serde(default)]
pub struct Prefs {
    /// The server last signed in to, offered on the next start.
    pub server: String,
    pub appearance: Appearance,
    pub sidebar_collapsed: bool,
    pub sessions_collapsed: bool,
    /// Sent with every run as `userInstructions`.
    pub chat_instructions: String,
    /// Shows each tool call's arguments and result in the transcript.
    pub debug_broker: bool,
    /// Where "Save to this PC" last saved, offered again next time.
    pub last_save_dir: Option<PathBuf>,
    /// Where files were last picked for upload.
    pub last_open_dir: Option<PathBuf>,
}

impl Default for Prefs {
    fn default() -> Self {
        Self {
            server: String::new(),
            appearance: Appearance::Dark,
            sidebar_collapsed: false,
            sessions_collapsed: false,
            chat_instructions: String::new(),
            debug_broker: false,
            last_save_dir: None,
            last_open_dir: None,
        }
    }
}

impl Global for Prefs {}

fn path() -> Option<PathBuf> {
    let base = std::env::var_os("APPDATA")
        .map(PathBuf::from)
        .or_else(|| std::env::var_os("XDG_CONFIG_HOME").map(PathBuf::from))
        .or_else(|| std::env::var_os("HOME").map(|home| PathBuf::from(home).join(".config")))?;
    Some(base.join("aikonOS").join("desktop.json"))
}

impl Prefs {
    pub fn load() -> Self {
        path()
            .and_then(|path| std::fs::read(path).ok())
            .and_then(|bytes| serde_json::from_slice::<Prefs>(&bytes).ok())
            .map(Prefs::clamped)
            .unwrap_or_default()
    }

    fn clamped(mut self) -> Self {
        if self.chat_instructions.chars().count() > MAX_CHAT_INSTRUCTIONS_CHARS {
            self.chat_instructions = self
                .chat_instructions
                .chars()
                .take(MAX_CHAT_INSTRUCTIONS_CHARS)
                .collect();
        }
        self
    }

    fn save(&self) -> std::io::Result<()> {
        let Some(path) = path() else {
            return Ok(());
        };
        if let Some(dir) = path.parent() {
            std::fs::create_dir_all(dir)?;
        }
        let tmp = path.with_extension("json.tmp");
        std::fs::write(&tmp, serde_json::to_vec_pretty(self).expect("prefs serialize"))?;
        std::fs::rename(tmp, path)
    }

    pub fn global(cx: &App) -> &Prefs {
        cx.global::<Prefs>()
    }

    /// Change preferences and write them through.
    pub fn update(cx: &mut App, edit: impl FnOnce(&mut Prefs)) {
        let prefs = cx.global_mut::<Prefs>();
        let before = prefs.clone();
        edit(prefs);
        let prefs = prefs.clone().clamped();
        *cx.global_mut::<Prefs>() = prefs.clone();
        if prefs != before
            && let Err(err) = prefs.save()
        {
            eprintln!("aikonos: could not save preferences: {err}");
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn missing_fields_take_defaults() {
        let prefs: Prefs = serde_json::from_str(r#"{"server":"https://ai.example.org/"}"#).unwrap();
        assert_eq!(prefs.server, "https://ai.example.org/");
        assert_eq!(prefs.appearance, Appearance::Dark);
        assert!(!prefs.debug_broker);
    }

    #[test]
    fn instructions_are_capped() {
        let prefs = Prefs {
            chat_instructions: "x".repeat(MAX_CHAT_INSTRUCTIONS_CHARS + 10),
            ..Prefs::default()
        }
        .clamped();
        assert_eq!(prefs.chat_instructions.chars().count(), MAX_CHAT_INSTRUCTIONS_CHARS);
    }
}
