//! Plain-language labels for tool calls in the chat trace. A port of
//! webui/web/src/lib/toolLabels.js; keep the two in step.

use serde_json::Value;

/// Tool name to a short gerund phrase, for every built-in tool
/// (agent-gateway/src/pi/tools.ts) plus `load_skill` (handled separately).
fn friendly(name: &str) -> Option<&'static str> {
    Some(match name {
        "web_fetch" => "Reading a web page",
        "web_search" => "Searching the web",
        "workspace_read" => "Listing workspace files",
        "doc_read" => "Reading a file",
        "doc_write" => "Writing a file",
        "email_draft" => "Drafting an email",
        "gdrive_read" => "Reading from Google Drive",
        "gdrive_write" => "Writing to Google Drive",
        "onedrive_read" => "Reading from OneDrive",
        "onedrive_write" => "Writing to OneDrive",
        "delegate" => "Delegating a task",
        "workflow_save" => "Saving a workflow",
        "workflow_run" => "Running a workflow",
        "workflow_list" => "Listing workflows",
        "workflow_publish" => "Publishing a workflow",
        "workflow_propose" => "Proposing a workflow change",
        "analyze_image" => "Analyzing an image",
        "docx_create" => "Creating a Word document",
        "docx_edit" => "Editing a Word document",
        "docx_extract" => "Reading a Word document",
        "xlsx_create" => "Creating a spreadsheet",
        "xlsx_edit" => "Editing a spreadsheet",
        "xlsx_extract" => "Reading a spreadsheet",
        "xlsx_recalc" => "Recalculating a spreadsheet",
        "pptx_create" => "Creating a presentation",
        "pptx_edit" => "Editing a presentation",
        "pptx_extract" => "Reading a presentation",
        "pptx_thumbnail" => "Previewing a presentation",
        "pdf_create" => "Creating a PDF",
        "pdf_transform" => "Transforming a PDF",
        "pdf_extract" => "Reading a PDF",
        "office_convert" => "Converting a document",
        "reason" => "Thinking",
        _ => return None,
    })
}

const ARG_MAX: usize = 60;

/// Workflow steps carry broker skill ids ("web.fetch") rather than tool
/// names ("web_fetch").
fn normalize_name(name: &str) -> String {
    name.replace('.', "_")
}

/// Trim and cut to `max` characters, ending with an ellipsis when cut.
pub fn clamp(text: &str, max: usize) -> String {
    let trimmed = text.trim();
    if trimmed.chars().count() > max {
        let mut cut: String = trimmed.chars().take(max - 1).collect();
        cut.push('…');
        cut
    } else {
        trimmed.to_owned()
    }
}

fn basename(path: &str) -> String {
    path.split('/')
        .rfind(|part| !part.is_empty())
        .unwrap_or(path)
        .to_owned()
}

fn hostname(url: &str) -> String {
    url::Url::parse(url)
        .ok()
        .and_then(|parsed| parsed.host_str().map(str::to_owned))
        .unwrap_or_else(|| url.to_owned())
}

fn first_words(text: &str, n: usize) -> String {
    text.split_whitespace().take(n).collect::<Vec<_>>().join(" ")
}

fn first_sentence(text: &str) -> String {
    clamp(text.split(". ").next().unwrap_or_default(), 80)
}

fn text_arg<'a>(args: &'a Value, key: &str) -> Option<&'a str> {
    args.get(key).and_then(Value::as_str).filter(|value| !value.is_empty())
}

/// One short detail from the call's arguments, or "" when there is nothing
/// useful or the JSON is malformed.
pub fn salient_arg(name: &str, args_json: &str) -> String {
    let name = normalize_name(name);
    let source = if args_json.trim().is_empty() { "{}" } else { args_json };
    let Ok(args) = serde_json::from_str::<Value>(source) else {
        return String::new();
    };
    if !args.is_object() {
        return String::new();
    }
    let value = if name == "web_search" {
        text_arg(&args, "query").unwrap_or_default().to_owned()
    } else if name == "web_fetch" {
        text_arg(&args, "url").map(hostname).unwrap_or_default()
    } else if name == "load_skill" || name.starts_with("workflow_") {
        text_arg(&args, "name").unwrap_or_default().to_owned()
    } else if let Some(path) = text_arg(&args, "path") {
        basename(path)
    } else if let Some(path) = text_arg(&args, "output_path") {
        basename(path)
    } else if let Some(url) = text_arg(&args, "url") {
        hostname(url)
    } else {
        ["query", "name", "title"]
            .iter()
            .find_map(|key| text_arg(&args, key))
            .unwrap_or_default()
            .to_owned()
    };
    clamp(&value, ARG_MAX)
}

/// The trace line for one tool call.
pub fn tool_label(name: &str, description: Option<&str>, args_json: &str) -> String {
    let normalized = normalize_name(name);
    if normalized == "load_skill" {
        let description = description
            .filter(|d| !d.is_empty())
            .map(str::to_owned)
            .or_else(|| {
                serde_json::from_str::<Value>(if args_json.is_empty() { "{}" } else { args_json })
                    .ok()
                    .and_then(|args| text_arg(&args, "name").map(str::to_owned))
            })
            .unwrap_or_else(|| normalized.clone());
        return format!("Loading skill: {}", first_words(&description, 5));
    }
    let base = friendly(&normalized)
        .map(str::to_owned)
        .or_else(|| description.map(first_sentence).filter(|sentence| !sentence.is_empty()))
        .unwrap_or_else(|| normalized.clone());
    let arg = salient_arg(&normalized, args_json);
    if arg.is_empty() {
        return base;
    }
    if normalized == "web_search" {
        format!("{base} — \"{arg}\"")
    } else {
        format!("{base} — {arg}")
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn labels_match_the_web_console() {
        assert_eq!(
            tool_label("web_search", None, r#"{"query":"Reuters news"}"#),
            "Searching the web — \"Reuters news\""
        );
        assert_eq!(
            tool_label("web.fetch", None, r#"{"url":"https://example.org/a/b"}"#),
            "Reading a web page — example.org"
        );
        assert_eq!(
            tool_label("doc_write", None, r#"{"path":"reports/q3/summary.md"}"#),
            "Writing a file — summary.md"
        );
        assert_eq!(
            tool_label("load_skill", Some("Write clear and concise policy documents"), "{}"),
            "Loading skill: Write clear and concise policy"
        );
        assert_eq!(
            tool_label(
                "mcp__crm__lookup",
                Some("Look up a customer. Returns JSON."),
                "not json"
            ),
            "Look up a customer"
        );
        assert_eq!(tool_label("unknown_tool", None, ""), "unknown_tool");
    }

    #[test]
    fn long_arguments_are_clamped() {
        let query = "x".repeat(80);
        let label = salient_arg("web_search", &format!(r#"{{"query":"{query}"}}"#));
        assert_eq!(label.chars().count(), ARG_MAX);
        assert!(label.ends_with('…'));
    }
}
