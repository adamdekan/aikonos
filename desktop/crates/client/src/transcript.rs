//! Chat sessions as the web console stores them, so a conversation started
//! in either client opens in the other. The files live in the user's
//! workspace: `.agent/Sessions/<id>.json` per session plus
//! `.agent/Sessions/index.json`, a manifest for the sidebar
//! (webui/web/src/api/sessions.js, store/sessions.js). The scheduler writes
//! the same shape for scheduled runs (agent-gateway/src/scheduler).
//!
//! Every type keeps fields it does not know in `extra`, and unknown
//! transcript entries are kept verbatim, so saving a session written by a
//! newer client does not drop its data.

use serde::{Deserialize, Deserializer, Serialize, Serializer};
use serde_json::{Map, Value};

use crate::agui::{RecalledConcept, SkillAnnouncement};

pub const SESSION_DIR: &str = ".agent/Sessions";
pub const MANIFEST_PATH: &str = ".agent/Sessions/index.json";
pub const LEGACY_SESSION_DIR: &str = "Sessions";

pub fn session_path(id: &str) -> String {
    format!("{SESSION_DIR}/{id}.json")
}

/// The session id for a listed workspace path, when it is a session file
/// (and not the manifest).
pub fn session_id_from_path(path: &str) -> Option<&str> {
    let name = path.strip_prefix(SESSION_DIR)?.strip_prefix('/')?;
    let id = name.strip_suffix(".json")?;
    (id != "index" && !id.is_empty() && !id.contains('/')).then_some(id)
}

/// A tool call inside an assistant turn.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct ToolCall {
    pub id: String,
    #[serde(default)]
    pub name: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub description: Option<String>,
    #[serde(rename = "argsJson", default)]
    pub args_json: String,
    /// A string for chat runs; scheduled runs store the raw result value.
    #[serde(default)]
    pub result: Option<Value>,
    #[serde(rename = "isError", default)]
    pub is_error: bool,
    #[serde(default)]
    pub done: bool,
    #[serde(flatten)]
    pub extra: Map<String, Value>,
}

impl ToolCall {
    pub fn new(id: String, name: String, description: Option<String>) -> Self {
        Self {
            id,
            name,
            description,
            args_json: String::new(),
            result: None,
            is_error: false,
            done: false,
            extra: Map::new(),
        }
    }

    pub fn has_result(&self) -> bool {
        !matches!(self.result, None | Some(Value::Null))
    }

    /// The result as display text: strings as they are, anything else as JSON.
    pub fn result_text(&self) -> Option<String> {
        match self.result.as_ref()? {
            Value::Null => None,
            Value::String(text) => Some(text.clone()),
            other => Some(other.to_string()),
        }
    }
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct UserTurn {
    #[serde(default)]
    pub text: String,
    #[serde(flatten)]
    pub extra: Map<String, Value>,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Default)]
pub struct AssistantTurn {
    #[serde(default)]
    pub text: String,
    #[serde(default)]
    pub tools: Vec<ToolCall>,
    #[serde(default)]
    pub error: Option<String>,
    #[serde(flatten)]
    pub extra: Map<String, Value>,
}

impl AssistantTurn {
    pub fn tool_mut(&mut self, id: &str) -> Option<&mut ToolCall> {
        self.tools.iter_mut().find(|tool| tool.id == id)
    }
}

#[derive(Debug, Clone, Copy, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "lowercase")]
pub enum BranchStatus {
    Running,
    Ok,
    Failure,
}

/// One sub-agent of a fan-out, created when it is spawned and settled in
/// place when it completes.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct SubagentBranch {
    pub index: i64,
    #[serde(default)]
    pub task: String,
    #[serde(default)]
    pub role: Option<String>,
    pub status: BranchStatus,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub failure: Option<String>,
    /// Major units (the proto's double), not micros.
    #[serde(default)]
    pub cost: f64,
}

#[derive(Debug, Clone, PartialEq)]
pub enum TranscriptEntry {
    User(UserTurn),
    Assistant(AssistantTurn),
    Skills(Vec<SkillAnnouncement>),
    Memory(Vec<RecalledConcept>),
    Subagents(Vec<SubagentBranch>),
    /// A role this client does not render, kept verbatim.
    Other(Value),
}

impl TranscriptEntry {
    pub fn user(text: impl Into<String>) -> Self {
        TranscriptEntry::User(UserTurn {
            text: text.into(),
            extra: Map::new(),
        })
    }

    pub fn assistant_text(text: impl Into<String>) -> Self {
        TranscriptEntry::Assistant(AssistantTurn {
            text: text.into(),
            ..Default::default()
        })
    }

    fn to_value(&self) -> Result<Value, serde_json::Error> {
        fn tagged(role: &str, body: Value) -> Value {
            let mut map = match body {
                Value::Object(map) => map,
                _ => Map::new(),
            };
            let mut out = Map::with_capacity(map.len() + 1);
            out.insert("role".into(), Value::from(role));
            out.append(&mut map);
            Value::Object(out)
        }
        Ok(match self {
            TranscriptEntry::User(turn) => tagged("user", serde_json::to_value(turn)?),
            TranscriptEntry::Assistant(turn) => tagged("assistant", serde_json::to_value(turn)?),
            TranscriptEntry::Skills(skills) => tagged("skills", serde_json::json!({ "skills": skills })),
            TranscriptEntry::Memory(concepts) => tagged("memory", serde_json::json!({ "concepts": concepts })),
            TranscriptEntry::Subagents(branches) => tagged("subagents", serde_json::json!({ "branches": branches })),
            TranscriptEntry::Other(value) => value.clone(),
        })
    }

    fn from_value(value: Value) -> Result<Self, serde_json::Error> {
        let role = value.get("role").and_then(Value::as_str).unwrap_or_default().to_owned();
        let without_role = || {
            let mut value = value.clone();
            if let Value::Object(map) = &mut value {
                map.remove("role");
            }
            value
        };
        let list = |key: &str| value.get(key).cloned().unwrap_or(Value::Array(Vec::new()));
        Ok(match role.as_str() {
            "user" => TranscriptEntry::User(serde_json::from_value(without_role())?),
            "assistant" => TranscriptEntry::Assistant(serde_json::from_value(without_role())?),
            "skills" => TranscriptEntry::Skills(serde_json::from_value(list("skills"))?),
            "memory" => TranscriptEntry::Memory(serde_json::from_value(list("concepts"))?),
            "subagents" => TranscriptEntry::Subagents(serde_json::from_value(list("branches"))?),
            _ => TranscriptEntry::Other(value),
        })
    }
}

impl Serialize for TranscriptEntry {
    fn serialize<S: Serializer>(&self, serializer: S) -> Result<S::Ok, S::Error> {
        self.to_value()
            .map_err(serde::ser::Error::custom)?
            .serialize(serializer)
    }
}

impl<'de> Deserialize<'de> for TranscriptEntry {
    fn deserialize<D: Deserializer<'de>>(deserializer: D) -> Result<Self, D::Error> {
        let value = Value::deserialize(deserializer)?;
        // An entry that fails to decode as its role is kept verbatim rather
        // than failing the whole session.
        Ok(TranscriptEntry::from_value(value.clone()).unwrap_or(TranscriptEntry::Other(value)))
    }
}

/// A full session file.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct SessionRecord {
    pub id: String,
    #[serde(default)]
    pub title: String,
    #[serde(default)]
    pub agent_id: Option<String>,
    #[serde(default)]
    pub agent_name: Option<String>,
    #[serde(default)]
    pub pinned: bool,
    #[serde(default)]
    pub pinned_at: Option<String>,
    #[serde(default)]
    pub created_at: Option<String>,
    #[serde(default)]
    pub updated_at: Option<String>,
    /// Older records spell it `threadId`.
    #[serde(default, alias = "threadId")]
    pub thread_id: Option<String>,
    #[serde(default)]
    pub first_message: Option<String>,
    #[serde(default)]
    pub messages: Vec<TranscriptEntry>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub source: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub schedule_id: Option<String>,
    #[serde(flatten)]
    pub extra: Map<String, Value>,
}

/// One sidebar row, as `index.json` stores it.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct ManifestEntry {
    pub id: String,
    #[serde(default)]
    pub title: String,
    #[serde(default)]
    pub agent_id: Option<String>,
    #[serde(default)]
    pub agent_name: Option<String>,
    #[serde(default)]
    pub pinned: bool,
    #[serde(default)]
    pub pinned_at: Option<String>,
    #[serde(default)]
    pub created_at: Option<String>,
    #[serde(default)]
    pub updated_at: Option<String>,
    #[serde(default)]
    pub source: Option<String>,
    #[serde(default)]
    pub schedule_id: Option<String>,
}

impl ManifestEntry {
    pub fn is_scheduled(&self) -> bool {
        self.source.as_deref() == Some("schedule")
    }

    pub fn has_agent(&self) -> bool {
        self.agent_id.as_deref().is_some_and(|id| !id.is_empty())
    }
}

impl From<&SessionRecord> for ManifestEntry {
    fn from(record: &SessionRecord) -> Self {
        ManifestEntry {
            id: record.id.clone(),
            title: record.title.clone(),
            agent_id: record.agent_id.clone(),
            agent_name: record.agent_name.clone(),
            pinned: record.pinned,
            pinned_at: record.pinned_at.clone(),
            created_at: record.created_at.clone(),
            updated_at: record.updated_at.clone(),
            source: record.source.clone(),
            schedule_id: record.schedule_id.clone(),
        }
    }
}

/// Pinned first (most recently pinned on top), then by last update.
pub fn sort_sessions(list: &mut [ManifestEntry]) {
    list.sort_by(|a, b| match (a.pinned, b.pinned) {
        (true, false) => std::cmp::Ordering::Less,
        (false, true) => std::cmp::Ordering::Greater,
        (true, true) => b.pinned_at.cmp(&a.pinned_at),
        (false, false) => b.updated_at.cmp(&a.updated_at),
    });
}

/// The session title the web console derives from the first message: its
/// first six words, at most 40 characters.
pub fn title_from(text: &str) -> String {
    let six = text.split_whitespace().take(6).collect::<Vec<_>>().join(" ");
    if six.chars().count() > 40 {
        six.chars().take(40).collect()
    } else {
        six
    }
}

/// The history a run sends: every user and assistant turn with text.
pub fn history_from(entries: &[TranscriptEntry]) -> Vec<crate::agui::HistoryTurn> {
    entries
        .iter()
        .filter_map(|entry| match entry {
            TranscriptEntry::User(turn) if !turn.text.is_empty() => Some(("user", &turn.text)),
            TranscriptEntry::Assistant(turn) if !turn.text.is_empty() => Some(("assistant", &turn.text)),
            _ => None,
        })
        .map(|(role, text)| crate::agui::HistoryTurn {
            role: role.into(),
            content: text.clone(),
        })
        .collect()
}

pub fn now_iso() -> String {
    chrono::Utc::now().to_rfc3339_opts(chrono::SecondsFormat::Millis, true)
}

#[cfg(test)]
mod tests {
    use super::*;

    const WEB_RECORD: &str = r#"{
        "id": "s1", "title": "Summarise the Q3 report", "agent_id": null, "agent_name": "",
        "pinned": false, "pinned_at": null,
        "created_at": "2026-10-01T10:00:00.000Z", "updated_at": "2026-10-01T10:05:00.000Z",
        "thread_id": "t1", "first_message": "Summarise the Q3 report",
        "messages": [
            {"role": "user", "text": "Summarise the Q3 report"},
            {"role": "skills", "skills": [{"name": "pdf", "status": "loaded", "description": "Read PDFs"}]},
            {"role": "assistant", "text": "Here it is.", "tools": [
                {"id": "c1", "name": "pdf_extract", "argsJson": "{\"path\":\"q3.pdf\"}", "result": "…", "isError": false, "done": true}
            ], "error": null},
            {"role": "delegation", "to": "bob@example.com"}
        ],
        "run_at": "kept"
    }"#;

    #[test]
    fn web_records_round_trip_without_loss() {
        let record: SessionRecord = serde_json::from_str(WEB_RECORD).unwrap();
        assert_eq!(record.messages.len(), 4);
        assert!(matches!(record.messages[1], TranscriptEntry::Skills(_)));
        assert!(matches!(record.messages[3], TranscriptEntry::Other(_)));
        let again: Value = serde_json::to_value(&record).unwrap();
        let original: Value = serde_json::from_str(WEB_RECORD).unwrap();
        assert_eq!(again, original);
    }

    #[test]
    fn scheduled_runs_store_raw_results() {
        let record: SessionRecord = serde_json::from_str(
            r#"{"id":"r1","title":"Daily","source":"schedule","schedule_id":"sc","messages":[
                {"role":"user","text":"Daily"},
                {"role":"assistant","text":"","tools":[{"id":"c","name":"web_fetch","argsJson":"{}","result":{"status":200},"isError":false,"done":true}],"error":null}
            ]}"#,
        )
        .unwrap();
        let TranscriptEntry::Assistant(turn) = &record.messages[1] else {
            panic!("expected an assistant turn");
        };
        assert_eq!(turn.tools[0].result_text().as_deref(), Some(r#"{"status":200}"#));
        assert!(ManifestEntry::from(&record).is_scheduled());
    }

    #[test]
    fn sessions_sort_pinned_first_then_recent() {
        let entry = |id: &str, pinned_at: Option<&str>, updated: &str| ManifestEntry {
            id: id.into(),
            title: id.into(),
            agent_id: None,
            agent_name: None,
            pinned: pinned_at.is_some(),
            pinned_at: pinned_at.map(Into::into),
            created_at: None,
            updated_at: Some(updated.into()),
            source: None,
            schedule_id: None,
        };
        let mut list = vec![
            entry("old", None, "2026-01-01"),
            entry("pin-early", Some("2026-02-01"), "2026-01-01"),
            entry("new", None, "2026-03-01"),
            entry("pin-late", Some("2026-04-01"), "2026-01-01"),
        ];
        sort_sessions(&mut list);
        let ids: Vec<_> = list.iter().map(|e| e.id.as_str()).collect();
        assert_eq!(ids, ["pin-late", "pin-early", "new", "old"]);
    }

    #[test]
    fn titles_and_paths_follow_the_web_rules() {
        assert_eq!(
            title_from("  one two three four five six seven "),
            "one two three four five six"
        );
        assert_eq!(title_from(&"abcdefghij ".repeat(6)).chars().count(), 40);
        assert_eq!(session_id_from_path(".agent/Sessions/abc.json"), Some("abc"));
        assert_eq!(session_id_from_path(".agent/Sessions/index.json"), None);
        assert_eq!(session_id_from_path("Sessions/abc.json"), None);
    }

    #[test]
    fn history_skips_empty_and_non_chat_turns() {
        let entries = vec![
            TranscriptEntry::user("hi"),
            TranscriptEntry::Skills(vec![]),
            TranscriptEntry::Assistant(AssistantTurn::default()),
            TranscriptEntry::assistant_text("hello"),
        ];
        let history = history_from(&entries);
        assert_eq!(history.len(), 2);
        assert_eq!(history[1].role, "assistant");
    }
}
