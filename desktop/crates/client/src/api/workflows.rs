//! Saved multi-step workflows (agent-gateway routes workflows.ts; web
//! api/workflows.js).

use std::collections::BTreeMap;
use std::sync::Arc;

use serde::{Deserialize, Serialize};
use serde_json::Value;

use super::encode;
use crate::connection::{Connection, EventStream};
use crate::error::{ApiError, ApiResult};

#[derive(Debug, Clone, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct WorkflowSummary {
    pub lineage_id: String,
    #[serde(default)]
    pub name: String,
    #[serde(default)]
    pub version: i64,
    /// "private" or "shared".
    #[serde(default)]
    pub visibility_kind: String,
    #[serde(default)]
    pub status: String,
    /// "runnable" or "greyed_out" (a requirement is missing).
    #[serde(default)]
    pub access_state: String,
    #[serde(default)]
    pub missing_requirements: Vec<String>,
    #[serde(default)]
    pub is_owner: bool,
    #[serde(default)]
    pub bound_agent_id: String,
    #[serde(default)]
    pub bound_agent_ok: bool,
}

impl WorkflowSummary {
    pub fn is_runnable(&self) -> bool {
        self.access_state != "greyed_out"
    }

    pub fn is_shared(&self) -> bool {
        self.visibility_kind == "shared"
    }
}

#[derive(Debug, Clone, Default, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct WorkflowPage {
    #[serde(default)]
    pub workflows: Vec<WorkflowSummary>,
    #[serde(default)]
    pub next_cursor: String,
    /// Shared workflows could not be listed; only the user's own are shown.
    #[serde(default)]
    pub shared_unavailable: bool,
}

/// One declared input of a workflow definition.
#[derive(Debug, Clone, Deserialize, PartialEq)]
pub struct WorkflowInput {
    pub name: String,
    #[serde(default)]
    pub default: Option<Value>,
    #[serde(default)]
    pub schema: Option<Value>,
}

#[derive(Debug, Clone, Deserialize, PartialEq)]
pub struct WorkflowStepDef {
    #[serde(default)]
    pub kind: Option<String>,
    #[serde(default)]
    pub skill: Option<String>,
    #[serde(default)]
    pub instruction: Option<String>,
}

#[derive(Debug, Clone, Default, Deserialize, PartialEq)]
pub struct WorkflowMetadata {
    #[serde(default)]
    pub name: String,
    #[serde(default)]
    pub description: Option<String>,
}

/// The parts of the broker's canonical workflow JSON the run dialog reads.
#[derive(Debug, Clone, Default, Deserialize, PartialEq)]
pub struct WorkflowDefinition {
    #[serde(default)]
    pub metadata: WorkflowMetadata,
    #[serde(default)]
    pub inputs: Vec<WorkflowInput>,
    #[serde(default)]
    pub steps: Vec<WorkflowStepDef>,
}

#[derive(Debug, Clone, Deserialize, PartialEq)]
#[serde(rename_all = "camelCase")]
pub struct WorkflowDetail {
    #[serde(default)]
    pub definition_json: String,
    #[serde(default)]
    pub version: i64,
}

impl WorkflowDetail {
    pub fn definition(&self) -> WorkflowDefinition {
        serde_json::from_str(&self.definition_json).unwrap_or_default()
    }
}

#[derive(Debug, Clone, Deserialize, PartialEq)]
#[serde(rename_all = "camelCase")]
pub struct StepOutcome {
    #[serde(default)]
    pub step_index: i64,
    /// "tool" or "reason".
    #[serde(default)]
    pub kind: String,
    #[serde(default)]
    pub skill: String,
    #[serde(default)]
    pub allowed: bool,
    #[serde(default)]
    pub output: Value,
    #[serde(default)]
    pub error: Option<String>,
    #[serde(default)]
    pub deny_reason: Option<String>,
}

#[derive(Debug, Clone, Default, Deserialize, PartialEq)]
#[serde(rename_all = "camelCase")]
pub struct RunResult {
    #[serde(default)]
    pub halted: bool,
    #[serde(default)]
    pub halted_at_step: Option<i64>,
    #[serde(default)]
    pub halt_reason: Option<String>,
    #[serde(default)]
    pub steps: Vec<StepOutcome>,
}

#[derive(Debug, Clone, Deserialize, PartialEq)]
pub struct RunReply {
    #[serde(default)]
    pub ok: bool,
    #[serde(default)]
    pub result: Option<RunResult>,
    #[serde(default)]
    pub error: Option<String>,
}

/// One settled step of a streaming run.
#[derive(Debug, Clone, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct StepProgress {
    #[serde(default)]
    pub index: i64,
    #[serde(default)]
    pub skill: Option<String>,
    #[serde(default)]
    pub ok: bool,
    #[serde(default)]
    pub deny_reason: Option<String>,
}

#[derive(Debug, Clone, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct WorkflowVersion {
    pub version: i64,
    /// "approved", "proposed" or "rejected".
    #[serde(default)]
    pub approval_state: String,
    #[serde(default)]
    pub created_at: String,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct Versions {
    #[serde(default)]
    versions: Vec<WorkflowVersion>,
}

#[derive(Serialize)]
struct RunBody<'a> {
    inputs: &'a BTreeMap<String, String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    session_id: Option<&'a str>,
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct ForkBody<'a> {
    new_name: &'a str,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct Forked {
    lineage_id: String,
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct PublishBody<'a> {
    version: i64,
    group_ids: &'a [String],
}

#[derive(Serialize)]
struct VersionBody {
    version: i64,
}

#[derive(Serialize)]
struct RateBody<'a> {
    version: i64,
    rating: &'a str,
    #[serde(skip_serializing_if = "Option::is_none")]
    note: Option<&'a str>,
}

#[derive(Serialize)]
struct DecideBody<'a> {
    version: i64,
    approved: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    reason: Option<&'a str>,
}

impl Connection {
    pub async fn workflows(&self) -> ApiResult<WorkflowPage> {
        self.get("/workflows").await
    }

    pub async fn workflow(&self, lineage_id: &str) -> ApiResult<WorkflowDetail> {
        self.get(&format!("/workflows/{}", encode(lineage_id))).await
    }

    /// Run a workflow, streaming one `step` event per settled step and a
    /// final `result` event. Approval-gated steps are declined.
    pub async fn run_workflow_stream(
        self: &Arc<Self>,
        lineage_id: &str,
        inputs: &BTreeMap<String, String>,
        session_id: Option<&str>,
    ) -> ApiResult<EventStream> {
        self.post_event_stream(
            // The gateway streams only for exactly `stream=1`.
            &format!("/workflows/{}/run?stream=1", encode(lineage_id)),
            &RunBody { inputs, session_id },
        )
        .await
    }

    /// The blocking run, for when the stream is refused.
    pub async fn run_workflow(
        &self,
        lineage_id: &str,
        inputs: &BTreeMap<String, String>,
        session_id: Option<&str>,
    ) -> ApiResult<RunReply> {
        self.post(
            &format!("/workflows/{}/run", encode(lineage_id)),
            &RunBody { inputs, session_id },
        )
        .await
    }

    pub async fn fork_workflow(&self, lineage_id: &str, new_name: &str) -> ApiResult<String> {
        let reply: Forked = self
            .post(
                &format!("/workflows/{}/fork", encode(lineage_id)),
                &ForkBody { new_name },
            )
            .await?;
        Ok(reply.lineage_id)
    }

    pub async fn publish_workflow(&self, lineage_id: &str, version: i64, group_ids: &[String]) -> ApiResult<()> {
        let _: Value = self
            .post(
                &format!("/workflows/{}/publish", encode(lineage_id)),
                &PublishBody { version, group_ids },
            )
            .await?;
        Ok(())
    }

    pub async fn rate_workflow(&self, lineage_id: &str, version: i64, good: bool, note: Option<&str>) -> ApiResult<()> {
        let body = RateBody {
            version,
            rating: if good { "RATING_SUCCESS" } else { "RATING_BAD" },
            note,
        };
        let _: Value = self
            .post(&format!("/workflows/{}/rate", encode(lineage_id)), &body)
            .await?;
        Ok(())
    }

    pub async fn workflow_versions(&self, lineage_id: &str) -> ApiResult<Vec<WorkflowVersion>> {
        Ok(self
            .get::<Versions>(&format!("/workflows/{}/versions", encode(lineage_id)))
            .await?
            .versions)
    }

    pub async fn pin_workflow_version(&self, lineage_id: &str, version: i64) -> ApiResult<()> {
        let _: Value = self
            .post(
                &format!("/workflows/{}/pin", encode(lineage_id)),
                &VersionBody { version },
            )
            .await?;
        Ok(())
    }

    pub async fn clear_workflow_pin(&self, lineage_id: &str) -> ApiResult<()> {
        let _: Value = self.delete(&format!("/workflows/{}/pin", encode(lineage_id))).await?;
        Ok(())
    }

    pub async fn decide_workflow_version(
        &self,
        lineage_id: &str,
        version: i64,
        approved: bool,
        reason: Option<&str>,
    ) -> ApiResult<()> {
        let _: Value = self
            .post(
                &format!("/workflows/{}/decide", encode(lineage_id)),
                &DecideBody {
                    version,
                    approved,
                    reason,
                },
            )
            .await?;
        Ok(())
    }

    pub async fn delete_workflow(&self, lineage_id: &str) -> ApiResult<()> {
        let reply: Value = self.delete(&format!("/workflows/{}", encode(lineage_id))).await?;
        if reply.get("ok").and_then(Value::as_bool) == Some(false) {
            return Err(ApiError::Status {
                status: 400,
                message: reply
                    .get("error")
                    .and_then(Value::as_str)
                    .unwrap_or("The workflow was not deleted.")
                    .to_owned(),
                body: reply,
            });
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn definitions_expose_inputs_and_steps() {
        let detail = WorkflowDetail {
            definition_json: r#"{"apiVersion":"aikonos.com/v1","kind":"Workflow","metadata":{"name":"Brief","visibility":{"kind":"private"}},"inputs":[{"name":"topic","default":"news"}],"steps":[{"kind":"tool","skill":"web.fetch","args":{}},{"kind":"reason","instruction":"Summarise"}]}"#.into(),
            version: 3,
        };
        let definition = detail.definition();
        assert_eq!(definition.metadata.name, "Brief");
        assert_eq!(definition.inputs[0].name, "topic");
        assert_eq!(definition.steps.len(), 2);
    }

    #[test]
    fn run_replies_decode_both_outcomes() {
        let ok: RunReply = serde_json::from_str(
            r#"{"ok":true,"result":{"halted":true,"haltedAtStep":1,"haltReason":"approval declined","steps":[{"stepIndex":0,"kind":"tool","skill":"web.fetch","resolvedArgs":{},"allowed":true,"output":{"status":200}}]}}"#,
        )
        .unwrap();
        assert!(ok.result.unwrap().halted);
        let failed: RunReply = serde_json::from_str(r#"{"ok":false,"error":"Error: boom"}"#).unwrap();
        assert_eq!(failed.error.as_deref(), Some("Error: boom"));
    }
}
