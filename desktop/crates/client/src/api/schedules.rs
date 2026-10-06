//! The user's scheduled runs (agent-gateway routes schedules.ts and
//! schedule-json.ts; web api/schedules.js).

use serde::{Deserialize, Serialize};

use super::encode;
use crate::connection::Connection;
use crate::error::ApiResult;

/// The tools a schedule created in the web console may run unattended
/// (views/Schedules.vue).
pub const DEFAULT_APPROVED_TOOLS: [&str; 3] = ["web.fetch", "doc.read", "doc.write"];

#[derive(Debug, Clone, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct Schedule {
    pub id: String,
    #[serde(default)]
    pub owner: String,
    #[serde(default)]
    pub prompt: String,
    /// "CRON" or "ONCE".
    #[serde(default)]
    pub kind: String,
    #[serde(default)]
    pub cron_expr: String,
    #[serde(default)]
    pub next_fire_at: Option<String>,
    #[serde(default)]
    pub approved_tools: Vec<String>,
    /// Set when the schedule runs a workflow rather than a prompt.
    #[serde(default)]
    pub workflow_lineage_id: String,
    #[serde(default)]
    pub workflow_display_name: String,
    /// ACTIVE, PAUSED, COMPLETED, FAILED or UNKNOWN.
    #[serde(default)]
    pub state: String,
    #[serde(default)]
    pub last_fire_at: Option<String>,
    #[serde(default)]
    pub last_status: String,
    #[serde(default)]
    pub last_summary: String,
    #[serde(default)]
    pub run_count: i64,
    #[serde(default)]
    pub created_by: String,
    #[serde(default)]
    pub created_at: Option<String>,
}

impl Schedule {
    pub fn is_once(&self) -> bool {
        self.kind.eq_ignore_ascii_case("ONCE")
    }

    pub fn is_paused(&self) -> bool {
        self.state == "PAUSED"
    }

    pub fn runs_workflow(&self) -> bool {
        !self.workflow_lineage_id.is_empty()
    }
}

/// What to run and when, for create and full edit.
#[derive(Debug, Clone, Serialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct ScheduleSpec {
    pub prompt: String,
    /// "CRON" or "ONCE".
    pub kind: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub cron_expr: Option<String>,
    /// RFC 3339 with an offset: the gateway parses it with `new Date`, so a
    /// time without one would be read in the server's zone.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub run_at: Option<String>,
    pub approved_tools: Vec<String>,
}

#[derive(Deserialize)]
struct Schedules {
    #[serde(default)]
    schedules: Vec<Schedule>,
}

#[derive(Deserialize)]
struct ScheduleReply {
    #[serde(default)]
    schedule: Option<Schedule>,
}

#[derive(Serialize)]
struct Action<'a> {
    action: &'a str,
}

impl Connection {
    pub async fn schedules(&self) -> ApiResult<Vec<Schedule>> {
        Ok(self.get::<Schedules>("/schedules").await?.schedules)
    }

    pub async fn create_schedule(&self, spec: &ScheduleSpec) -> ApiResult<Option<Schedule>> {
        Ok(self.post::<_, ScheduleReply>("/schedules", spec).await?.schedule)
    }

    /// Overwrite a schedule. Every field is sent: an absent tool list would
    /// clear it.
    pub async fn update_schedule(&self, id: &str, spec: &ScheduleSpec) -> ApiResult<Option<Schedule>> {
        Ok(self
            .patch::<_, ScheduleReply>(&format!("/schedules/{}", encode(id)), spec)
            .await?
            .schedule)
    }

    pub async fn set_schedule_paused(&self, id: &str, paused: bool) -> ApiResult<Option<Schedule>> {
        let action = Action {
            action: if paused { "pause" } else { "resume" },
        };
        Ok(self
            .patch::<_, ScheduleReply>(&format!("/schedules/{}", encode(id)), &action)
            .await?
            .schedule)
    }

    pub async fn delete_schedule(&self, id: &str) -> ApiResult<()> {
        let _: serde_json::Value = self.delete(&format!("/schedules/{}", encode(id))).await?;
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn schedules_decode_the_gateway_shape() {
        let body: Schedules = serde_json::from_str(
            r#"{"schedules":[{"id":"s1","owner":"alice@example.com","prompt":"Summarise the news","kind":"CRON","cronExpr":"CRON_TZ=Europe/Vienna 0 9 * * 1,2,3,4,5","nextFireAt":"2026-10-07T07:00:00.000Z","approvedTools":["web.fetch"],"workflowLineageId":"","workflowDisplayName":"","state":"PAUSED","lastFireAt":null,"lastStatus":"","lastSummary":"","runCount":0,"createdBy":"alice@example.com","createdAt":"2026-10-01T10:00:00.000Z"}]}"#,
        )
        .unwrap();
        let schedule = &body.schedules[0];
        assert!(schedule.is_paused());
        assert!(!schedule.is_once());
        assert!(!schedule.runs_workflow());
    }

    #[test]
    fn specs_omit_the_other_kind_s_field() {
        let spec = ScheduleSpec {
            prompt: "Daily brief".into(),
            kind: "ONCE".into(),
            cron_expr: None,
            run_at: Some("2026-10-07T09:00:00+02:00".into()),
            approved_tools: vec!["web.fetch".into()],
        };
        assert_eq!(
            serde_json::to_value(&spec).unwrap(),
            serde_json::json!({"prompt":"Daily brief","kind":"ONCE","runAt":"2026-10-07T09:00:00+02:00","approvedTools":["web.fetch"]})
        );
    }
}
