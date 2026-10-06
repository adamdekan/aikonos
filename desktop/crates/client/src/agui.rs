//! The AG-UI run stream the gateway serves at `POST /agui`, decoded into
//! typed events. Event names and fields follow agent-gateway/src/agui and the
//! web console's dispatcher (webui/web/src/api/agui.js).

use serde::{Deserialize, Serialize};
use serde_json::Value;

/// One turn of prior conversation sent with a run so the agent sees context.
#[derive(Debug, Clone, Serialize, PartialEq, Eq)]
pub struct HistoryTurn {
    pub role: String,
    pub content: String,
}

/// The body of `POST /agui`.
#[derive(Debug, Clone, Serialize, Default)]
pub struct RunRequest {
    pub prompt: String,
    #[serde(rename = "threadId")]
    pub thread_id: String,
    #[serde(rename = "agentId", skip_serializing_if = "Option::is_none")]
    pub agent_id: Option<String>,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub history: Vec<HistoryTurn>,
    /// Set only when the run was started from the `/skill` palette; the
    /// gateway re-resolves and gates it server-side.
    #[serde(rename = "skillName", skip_serializing_if = "Option::is_none")]
    pub skill_name: Option<String>,
    /// The user's standing instructions from Settings.
    #[serde(rename = "userInstructions", skip_serializing_if = "Option::is_none")]
    pub user_instructions: Option<String>,
    /// The session record id, used only to attribute spend to the session.
    #[serde(rename = "session_id", skip_serializing_if = "Option::is_none")]
    pub session_id: Option<String>,
}

/// A tool call paused for a human decision (`aikonos.approval.request`, or a
/// row of `GET /approvals`). Unknown fields are kept in `extra` so a newer
/// gateway's additions survive a round trip.
#[derive(Debug, Clone, Deserialize, Serialize, PartialEq)]
pub struct ApprovalRequest {
    #[serde(rename = "toolCallId")]
    pub tool_call_id: String,
    /// The aikonOS tool id, e.g. `web.fetch` or `mcp:<connector>:<tool>`.
    #[serde(rename = "toolId", default)]
    pub tool_id: String,
    /// The agent harness's name for the tool, e.g. `web_fetch`.
    #[serde(rename = "toolName", default)]
    pub tool_name: String,
    /// proto/plan.proto `EffectClass`, 0–8.
    #[serde(rename = "effectClass", default)]
    pub effect_class: i64,
    #[serde(rename = "stepUp", default)]
    pub step_up: bool,
    #[serde(default)]
    pub risk: Option<String>,
    #[serde(default)]
    pub agent: Option<String>,
    #[serde(default)]
    pub reason: Option<String>,
    #[serde(default)]
    pub args: Option<Value>,
    #[serde(flatten)]
    pub extra: serde_json::Map<String, Value>,
}

impl ApprovalRequest {
    /// Step-up and high-risk calls lock Approve until the user confirms they
    /// reviewed the arguments.
    pub fn is_high_risk(&self) -> bool {
        self.step_up || self.risk.as_deref() == Some("high")
    }

    /// The tool's id as the console shows it in the approval header.
    pub fn title(&self) -> &str {
        if !self.tool_id.is_empty() {
            &self.tool_id
        } else {
            &self.tool_name
        }
    }

    /// proto/plan.proto `EffectClass` in words.
    pub fn effect_label(&self) -> Option<&'static str> {
        Some(match self.effect_class {
            1 => "Reads data",
            2 => "Writes to your workspace",
            3 => "Writes inside the organisation",
            4 => "Writes outside the organisation",
            5 => "Sends data over the network",
            6 => "Uses credentials",
            7 => "Deletes or overwrites data",
            8 => "Changes infrastructure",
            _ => return None,
        })
    }
}

/// A skill the gateway activated (or refused) for this turn.
#[derive(Debug, Clone, Deserialize, Serialize, PartialEq, Eq)]
pub struct SkillAnnouncement {
    pub name: String,
    #[serde(default)]
    pub status: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub description: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub reason: Option<String>,
}

/// A memory concept whose summary was injected as context for this turn.
#[derive(Debug, Clone, Deserialize, Serialize, PartialEq)]
pub struct RecalledConcept {
    pub id: String,
    #[serde(default)]
    pub scope: String,
    #[serde(rename = "groupId", default, skip_serializing_if = "Option::is_none")]
    pub group_id: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub title: Option<String>,
    #[serde(default)]
    pub status: String,
    #[serde(rename = "trustTier", default)]
    pub trust_tier: String,
    #[serde(default)]
    pub stale: bool,
}

#[derive(Debug, Clone, PartialEq)]
pub enum AguiEvent {
    RunStarted,
    TextStart,
    Text(String),
    TextEnd,
    ToolCallStart {
        id: String,
        name: String,
        description: Option<String>,
    },
    /// The arguments so far. The gateway sends the whole JSON in one delta,
    /// and the web console replaces rather than appends; so does this client.
    ToolCallArgs {
        id: String,
        args_json: String,
    },
    ToolCallEnd {
        id: String,
    },
    ToolCallResult {
        id: String,
        content: String,
        is_error: bool,
    },
    ApprovalRequest(Box<ApprovalRequest>),
    ToolError {
        tool_call_id: String,
        content: String,
    },
    SkillsLoaded(Vec<SkillAnnouncement>),
    MemoryRecalled(Vec<RecalledConcept>),
    SubagentSpawned {
        index: i64,
        task: String,
        role: Option<String>,
    },
    SubagentCompleted {
        index: i64,
        task: Option<String>,
        role: Option<String>,
        ok: bool,
        failure: Option<String>,
        cost: f64,
    },
    RunFinished,
    RunError(String),
}

fn str_field(value: &Value, key: &str) -> Option<String> {
    match value.get(key)? {
        Value::String(s) => Some(s.clone()),
        Value::Null => None,
        other => Some(other.to_string()),
    }
}

/// Decode one `data:` payload. Events this client does not render (for
/// example `aikonos.user`) and malformed payloads yield `None`, the same as
/// the web console's dispatcher ignoring them.
pub fn parse_event(data: &str) -> Option<AguiEvent> {
    let ev: Value = serde_json::from_str(data).ok()?;
    let kind = ev.get("type")?.as_str()?;
    let event = match kind {
        "RUN_STARTED" => AguiEvent::RunStarted,
        "TEXT_MESSAGE_START" => AguiEvent::TextStart,
        "TEXT_MESSAGE_CONTENT" => AguiEvent::Text(str_field(&ev, "delta").unwrap_or_default()),
        "TEXT_MESSAGE_END" => AguiEvent::TextEnd,
        "TOOL_CALL_START" => AguiEvent::ToolCallStart {
            id: str_field(&ev, "toolCallId")?,
            name: str_field(&ev, "toolCallName").unwrap_or_default(),
            description: str_field(&ev, "toolDescription"),
        },
        "TOOL_CALL_ARGS" => AguiEvent::ToolCallArgs {
            id: str_field(&ev, "toolCallId")?,
            args_json: str_field(&ev, "delta").unwrap_or_default(),
        },
        "TOOL_CALL_END" => AguiEvent::ToolCallEnd {
            id: str_field(&ev, "toolCallId")?,
        },
        "TOOL_CALL_RESULT" => AguiEvent::ToolCallResult {
            id: str_field(&ev, "toolCallId")?,
            content: str_field(&ev, "content").unwrap_or_default(),
            is_error: ev.get("isError").and_then(Value::as_bool).unwrap_or(false),
        },
        "RUN_FINISHED" => AguiEvent::RunFinished,
        "RUN_ERROR" => AguiEvent::RunError(str_field(&ev, "message").unwrap_or_else(|| "The run failed.".into())),
        "CUSTOM" => {
            let name = ev.get("name")?.as_str()?;
            let value = ev.get("value").cloned().unwrap_or(Value::Null);
            match name {
                "aikonos.approval.request" => AguiEvent::ApprovalRequest(Box::new(serde_json::from_value(value).ok()?)),
                "aikonos.tool.error" => AguiEvent::ToolError {
                    tool_call_id: str_field(&value, "toolCallId")?,
                    content: str_field(&value, "content").unwrap_or_default(),
                },
                "aikonos.skills.loaded" => AguiEvent::SkillsLoaded(
                    serde_json::from_value(value.get("skills").cloned().unwrap_or(Value::Null)).unwrap_or_default(),
                ),
                "aikonos.memory.recalled" => AguiEvent::MemoryRecalled(
                    serde_json::from_value(value.get("concepts").cloned().unwrap_or(Value::Null)).unwrap_or_default(),
                ),
                "aikonos.subagent.spawned" => AguiEvent::SubagentSpawned {
                    index: value.get("index")?.as_i64()?,
                    task: str_field(&value, "task").unwrap_or_default(),
                    role: str_field(&value, "role"),
                },
                "aikonos.subagent.completed" => AguiEvent::SubagentCompleted {
                    index: value.get("index")?.as_i64()?,
                    task: str_field(&value, "task"),
                    role: str_field(&value, "role"),
                    ok: value.get("ok").and_then(Value::as_bool).unwrap_or(false),
                    failure: str_field(&value, "failure"),
                    cost: value.get("cost").and_then(Value::as_f64).unwrap_or(0.0),
                },
                _ => return None,
            }
        }
        _ => return None,
    };
    Some(event)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn decodes_text_and_tool_frames() {
        assert_eq!(
            parse_event(r#"{"type":"TEXT_MESSAGE_CONTENT","messageId":"m","delta":"Hi"}"#),
            Some(AguiEvent::Text("Hi".into()))
        );
        assert_eq!(
            parse_event(
                r#"{"type":"TOOL_CALL_START","toolCallId":"t1","toolCallName":"web_fetch","toolDescription":"Fetch a page"}"#
            ),
            Some(AguiEvent::ToolCallStart {
                id: "t1".into(),
                name: "web_fetch".into(),
                description: Some("Fetch a page".into()),
            })
        );
        assert_eq!(
            parse_event(r#"{"type":"TOOL_CALL_RESULT","toolCallId":"t1","content":"ok"}"#),
            Some(AguiEvent::ToolCallResult {
                id: "t1".into(),
                content: "ok".into(),
                is_error: false
            })
        );
    }

    #[test]
    fn decodes_approval_requests_and_keeps_unknown_fields() {
        let event = parse_event(
            r#"{"type":"CUSTOM","name":"aikonos.approval.request","value":{"toolCallId":"t9","toolId":"doc.write","stepUp":true,"reason":"writes a file","args":{"path":"a.md"},"newField":7}}"#,
        );
        let Some(AguiEvent::ApprovalRequest(request)) = event else {
            panic!("expected an approval request");
        };
        assert_eq!(request.tool_call_id, "t9");
        assert!(request.is_high_risk());
        assert_eq!(request.extra.get("newField"), Some(&Value::from(7)));
    }

    #[test]
    fn ignores_unrendered_and_malformed_events() {
        assert_eq!(
            parse_event(r#"{"type":"CUSTOM","name":"aikonos.user","value":{}}"#),
            None
        );
        assert_eq!(parse_event("not json"), None);
        assert_eq!(parse_event(r#"{"type":"STATE_SNAPSHOT"}"#), None);
    }

    #[test]
    fn run_request_omits_empty_optional_fields() {
        let body = serde_json::to_value(RunRequest {
            prompt: "hello".into(),
            thread_id: "th".into(),
            ..Default::default()
        })
        .unwrap();
        assert_eq!(body, serde_json::json!({"prompt": "hello", "threadId": "th"}));
    }
}
