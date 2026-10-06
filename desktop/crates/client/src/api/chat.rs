//! What the chat view needs besides the run stream itself: the palettes'
//! sources, approvals, named agents, and the usage strip. Mirrors
//! webui/web/src/api/{agents,delegation,inbox,usage,admin}.js and
//! store/discovery.js.

use std::sync::Arc;

use serde::{Deserialize, Serialize};
use serde_json::Value;

use super::encode;
use crate::agui::{ApprovalRequest, RunRequest};
use crate::connection::{Connection, EventStream};
use crate::error::{ApiError, ApiResult};

/// A named agent the user may chat with.
#[derive(Debug, Clone, Deserialize, PartialEq, Eq)]
pub struct AgentSummary {
    pub id: String,
    #[serde(default)]
    pub name: String,
}

/// A skill offered by the `/command` palette: an admin-governed bundle, or
/// one of the user's personal skills (whose run name is `personal:<name>`).
#[derive(Debug, Clone, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct SkillBundle {
    #[serde(default)]
    pub id: String,
    pub name: String,
    #[serde(default)]
    pub description: String,
    /// The skill's instructions (shown read-only for granted skills).
    #[serde(default)]
    pub body: String,
    /// Set for personal skills; the gateway resolves this name.
    #[serde(skip)]
    pub skill_name: Option<String>,
    #[serde(skip)]
    pub personal: bool,
}

impl SkillBundle {
    /// The name a run sends as `skillName`.
    pub fn run_name(&self) -> &str {
        self.skill_name.as_deref().unwrap_or(&self.name)
    }
}

#[derive(Debug, Clone, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct DelegatableUser {
    pub user_id: String,
    #[serde(default)]
    pub display_name: String,
}

#[derive(Debug, Clone, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct DelegatableGroup {
    pub group_id: String,
    #[serde(default)]
    pub display_name: String,
    #[serde(default)]
    pub member_count: u64,
}

#[derive(Debug, Clone, Default, Deserialize, PartialEq, Eq)]
pub struct Delegatable {
    #[serde(default)]
    pub users: Vec<DelegatableUser>,
    #[serde(default)]
    pub groups: Vec<DelegatableGroup>,
}

/// A delegation target picked from the `@` palette.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum DelegationTarget {
    User {
        user_id: String,
        display_name: String,
    },
    Group {
        group_id: String,
        display_name: String,
        member_count: u64,
    },
}

impl DelegationTarget {
    pub fn display_name(&self) -> &str {
        match self {
            DelegationTarget::User { display_name, .. } | DelegationTarget::Group { display_name, .. } => display_name,
        }
    }
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct DelegateBody<'a> {
    #[serde(skip_serializing_if = "Option::is_none")]
    to: Option<&'a str>,
    #[serde(skip_serializing_if = "Option::is_none")]
    group: Option<&'a str>,
    intent: &'a str,
    scopes: [&'a str; 0],
    max_cost: u32,
}

#[derive(Deserialize)]
struct DelegateReply {
    #[serde(default)]
    ok: Option<bool>,
    #[serde(default)]
    error: Option<String>,
}

/// LLM usage attributed to one session (`GET /sessions/:id/usage`).
#[derive(Debug, Clone, Default, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct SessionUsage {
    #[serde(default)]
    pub models: Vec<String>,
    #[serde(default)]
    pub tokens_in: i64,
    #[serde(default)]
    pub tokens_out: i64,
    #[serde(default)]
    pub cost_micros: i64,
    #[serde(default)]
    pub calls: i64,
}

impl SessionUsage {
    /// The first (most expensive) model, with "+N" when the session used
    /// several.
    pub fn model_label(&self) -> String {
        let mut unique: Vec<&str> = Vec::new();
        for model in &self.models {
            if !unique.contains(&model.as_str()) {
                unique.push(model);
            }
        }
        match unique.as_slice() {
            [] => "—".to_owned(),
            [one] => (*one).to_owned(),
            [first, rest @ ..] => format!("{first} +{}", rest.len()),
        }
    }

    pub fn models_title(&self) -> String {
        let mut unique: Vec<&str> = Vec::new();
        for model in &self.models {
            if !unique.contains(&model.as_str()) {
                unique.push(model);
            }
        }
        unique.join(", ")
    }
}

#[derive(Deserialize)]
struct Agents {
    #[serde(default)]
    agents: Vec<AgentSummary>,
}

#[derive(Deserialize)]
struct Skills {
    #[serde(default)]
    skills: Vec<String>,
}

#[derive(Deserialize)]
struct Bundles {
    #[serde(default)]
    bundles: Vec<SkillBundle>,
}

#[derive(Deserialize)]
struct Approvals {
    #[serde(default)]
    approvals: Vec<ApprovalRequest>,
}

#[derive(Deserialize)]
struct Resolved {
    #[serde(default)]
    resolved: bool,
}

#[derive(Serialize, Deserialize)]
struct Soul {
    #[serde(default)]
    soul: String,
}

#[derive(Serialize)]
struct Decision {
    approved: bool,
}

impl Connection {
    /// Start a chat run. The returned stream yields the AG-UI frames.
    pub async fn run_agui(self: &Arc<Self>, request: &RunRequest) -> ApiResult<EventStream> {
        self.post_event_stream("/agui", request).await
    }

    /// The agents the user may chat with. A 403 is no agents.
    pub async fn list_agents(&self) -> ApiResult<Vec<AgentSummary>> {
        match self.get::<Agents>("/agents").await {
            Ok(body) => Ok(body.agents),
            Err(err) if err.is_forbidden() => Ok(Vec::new()),
            Err(err) => Err(err),
        }
    }

    /// The user's granted tool ids and capability skills ("scheduler",
    /// "workflows"), which decide what the sidebar offers.
    pub async fn user_skills(&self) -> ApiResult<Vec<String>> {
        Ok(self.get::<Skills>("/user/skills").await?.skills)
    }

    pub async fn user_skill_bundles(&self) -> ApiResult<Vec<SkillBundle>> {
        Ok(self.get::<Bundles>("/user/skill-bundles").await?.bundles)
    }

    pub async fn delegatable(&self) -> ApiResult<Delegatable> {
        match self.get::<Delegatable>("/delegatable-users").await {
            Ok(body) => Ok(body),
            Err(err) if err.is_forbidden() => Ok(Delegatable::default()),
            Err(err) => Err(err),
        }
    }

    /// Hand `intent` to a colleague or a group, with the web console's
    /// defaults: no extra scopes, a cost cap of 50.
    pub async fn delegate(&self, target: &DelegationTarget, intent: &str) -> ApiResult<()> {
        let body = match target {
            DelegationTarget::User { user_id, .. } => DelegateBody {
                to: Some(user_id),
                group: None,
                intent,
                scopes: [],
                max_cost: 50,
            },
            DelegationTarget::Group { group_id, .. } => DelegateBody {
                to: None,
                group: Some(group_id),
                intent,
                scopes: [],
                max_cost: 50,
            },
        };
        let reply: DelegateReply = self.post("/delegate", &body).await?;
        if reply.ok == Some(false) {
            return Err(ApiError::Status {
                status: 400,
                message: reply.error.unwrap_or_else(|| "Delegation failed".into()),
                body: Value::Null,
            });
        }
        Ok(())
    }

    /// Every approval waiting on this user, from any client.
    pub async fn pending_approvals(&self) -> ApiResult<Vec<ApprovalRequest>> {
        Ok(self.get::<Approvals>("/approvals").await?.approvals)
    }

    /// Approve or deny a paused tool call. `false` when it was no longer
    /// waiting (decided elsewhere, timed out, or its run ended).
    pub async fn decide_approval(&self, tool_call_id: &str, approved: bool) -> ApiResult<bool> {
        let reply: Resolved = self
            .post(&format!("/approve/{}", encode(tool_call_id)), &Decision { approved })
            .await?;
        Ok(reply.resolved)
    }

    pub async fn session_usage(&self, session_id: &str) -> ApiResult<SessionUsage> {
        self.get(&format!("/sessions/{}/usage", encode(session_id))).await
    }

    /// A named agent's personality, or `None` when the user may not edit it.
    pub async fn agent_soul(&self, agent_id: &str) -> ApiResult<Option<String>> {
        match self.get::<Soul>(&format!("/agents/{}/soul", encode(agent_id))).await {
            Ok(body) => Ok(Some(body.soul)),
            Err(err) if err.is_forbidden() || err.status() == Some(404) => Ok(None),
            Err(err) => Err(err),
        }
    }

    pub async fn set_agent_soul(&self, agent_id: &str, soul: &str) -> ApiResult<String> {
        let reply: Soul = self
            .put(
                &format!("/agents/{}/soul", encode(agent_id)),
                &Soul { soul: soul.to_owned() },
            )
            .await?;
        Ok(reply.soul)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn usage_names_the_costliest_model() {
        let usage = SessionUsage {
            models: vec!["big".into(), "small".into(), "big".into()],
            ..Default::default()
        };
        assert_eq!(usage.model_label(), "big +1");
        assert_eq!(usage.models_title(), "big, small");
        assert_eq!(SessionUsage::default().model_label(), "—");
    }

    #[test]
    fn delegation_body_matches_the_web_client() {
        let body = serde_json::to_value(DelegateBody {
            to: None,
            group: Some("ward-4"),
            intent: "check the rota",
            scopes: [],
            max_cost: 50,
        })
        .unwrap();
        assert_eq!(
            body,
            serde_json::json!({"group": "ward-4", "intent": "check the rota", "scopes": [], "maxCost": 50})
        );
    }

    #[test]
    fn approvals_decode_the_gateway_shape() {
        let body: Approvals = serde_json::from_str(
            r#"{"approvals":[{"toolCallId":"toolu_1","toolName":"doc_write","toolId":"doc.write","effectClass":2,"reason":"human approval required","args":{"path":"a.md"},"stepUp":false}]}"#,
        )
        .unwrap();
        let approval = &body.approvals[0];
        assert_eq!(approval.title(), "doc.write");
        assert_eq!(approval.effect_class, 2);
        assert!(!approval.is_high_risk());
    }
}
