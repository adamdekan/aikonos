//! Characters that change how text reads without being seen. A port of
//! webui/web/src/lib/revealHidden.js; keep the two in step.

use std::fmt::Write as _;

/// C0 and C1 controls (newline and tab stay), bidi overrides and isolates,
/// zero-width and invisible format characters, line and paragraph
/// separators.
pub fn is_hidden(c: char) -> bool {
    matches!(
        c,
        '\u{0}'..='\u{8}'
            | '\u{B}'..='\u{1F}'
            | '\u{7F}'..='\u{9F}'
            | '\u{AD}'
            | '\u{61C}'
            | '\u{200B}'..='\u{200F}'
            | '\u{2028}'..='\u{202E}'
            | '\u{2060}'..='\u{2064}'
            | '\u{2066}'..='\u{2069}'
            | '\u{FEFF}'
    )
}

/// `text` with every hidden character as a visible `\uXXXX` escape, so an
/// approver reads the string the tool will receive, not the one a bidi
/// override draws (F213).
pub fn reveal_hidden(text: &str) -> String {
    let mut shown = String::with_capacity(text.len());
    for c in text.chars() {
        if is_hidden(c) {
            let _ = write!(shown, "\\u{:04X}", c as u32);
        } else {
            shown.push(c);
        }
    }
    shown
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn hidden_characters_are_shown_as_escapes() {
        assert_eq!(reveal_hidden("me@corp\u{202E}moc.live"), "me@corp\\u202Emoc.live");
        assert_eq!(
            reveal_hidden("a\u{200B}b\u{2066}c\u{0}d\u{9F}e\u{FEFF}"),
            "a\\u200Bb\\u2066c\\u0000d\\u009Fe\\uFEFF"
        );
        assert_eq!(reveal_hidden("line\n\ttab é 日本"), "line\n\ttab é 日本");
    }
}
