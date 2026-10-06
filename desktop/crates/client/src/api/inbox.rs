//! Tasks colleagues handed to this user, and shared skills waiting to be
//! accepted (agent-gateway routes delegation.ts and skills.ts; web
//! api/inbox.js and api/skills.js).

use serde::{Deserialize, Serialize};

use super::encode;
use crate::connection::Connection;
use crate::error::ApiResult;

#[derive(Debug, Clone, Default, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct DelegatedTask {
    #[serde(default)]
    pub intent: String,
    #[serde(default)]
    pub payload_ref: String,
    #[serde(default)]
    pub required_skills: Vec<String>,
    #[serde(default)]
    pub deadline: Option<String>,
    #[serde(default)]
    pub priority: String,
    /// "" for a delegated task, "skill_transfer" for a shared skill.
    #[serde(default)]
    pub kind: String,
}

#[derive(Debug, Clone, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct Envelope {
    pub envelope_id: String,
    #[serde(default)]
    pub from_user_id: String,
    #[serde(default)]
    pub from_display_name: String,
    #[serde(default)]
    pub task: Option<DelegatedTask>,
    #[serde(default)]
    pub received_at: Option<String>,
    #[serde(default)]
    pub status: String,
}

impl Envelope {
    pub fn is_skill_transfer(&self) -> bool {
        self.task.as_ref().is_some_and(|task| task.kind == "skill_transfer")
    }

    /// The sender as the console names them.
    pub fn sender(&self) -> &str {
        if self.from_display_name.is_empty() {
            &self.from_user_id
        } else {
            &self.from_display_name
        }
    }
}

#[derive(Deserialize)]
struct Envelopes {
    #[serde(default)]
    envelopes: Vec<Envelope>,
}

/// The preview shown before accepting a shared skill.
#[derive(Debug, Clone, Default, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct SkillTransferPreview {
    #[serde(default)]
    pub skill_name: String,
    #[serde(default)]
    pub from_user_id: String,
    #[serde(default)]
    pub body: String,
    #[serde(default)]
    pub manifest: Vec<TransferFile>,
    /// Warnings from the injection scan.
    #[serde(default)]
    pub flags: Vec<String>,
    #[serde(default)]
    pub content_hash: String,
    /// A personal skill of the same name already exists.
    #[serde(default)]
    pub conflict: bool,
}

#[derive(Debug, Clone, Default, Deserialize, PartialEq, Eq)]
pub struct TransferFile {
    pub path: String,
    #[serde(default)]
    pub size: u64,
}

#[derive(Serialize)]
struct AcceptBody<'a> {
    mode: &'a str,
    #[serde(skip_serializing_if = "Option::is_none")]
    name_override: Option<&'a str>,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct Installed {
    #[serde(default)]
    installed_name: String,
}

/// How to resolve a name clash when accepting a shared skill.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum AcceptMode {
    Rename,
    Replace,
}

impl Connection {
    pub async fn inbox(&self) -> ApiResult<Vec<Envelope>> {
        Ok(self.get::<Envelopes>("/inbox").await?.envelopes)
    }

    pub async fn dismiss_envelope(&self, envelope_id: &str) -> ApiResult<()> {
        let _: serde_json::Value = self
            .post_empty(&format!("/inbox/{}/dismiss", encode(envelope_id)))
            .await?;
        Ok(())
    }

    pub async fn skill_transfer_preview(&self, envelope_id: &str) -> ApiResult<SkillTransferPreview> {
        self.get(&format!("/skills/transfers/{}", encode(envelope_id))).await
    }

    /// Install a shared skill; returns the name it was installed under.
    pub async fn accept_skill_transfer(
        &self,
        envelope_id: &str,
        mode: AcceptMode,
        name_override: Option<&str>,
    ) -> ApiResult<String> {
        let body = AcceptBody {
            mode: match mode {
                AcceptMode::Rename => "rename",
                AcceptMode::Replace => "replace",
            },
            name_override,
        };
        let reply: Installed = self
            .post(&format!("/skills/transfers/{}/accept", encode(envelope_id)), &body)
            .await?;
        Ok(reply.installed_name)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn envelopes_decode_and_classify() {
        let list: Envelopes = serde_json::from_str(
            r#"{"envelopes":[
                {"envelopeId":"e1","fromUserId":"bob@example.com","fromDisplayName":"Bob","task":{"intent":"Check the rota","payloadRef":"","requiredSkills":[],"priority":"normal","kind":""},"receivedAt":"2026-10-06T08:15:30.123Z","status":"pending"},
                {"envelopeId":"e2","fromUserId":"carol@example.com","fromDisplayName":"","task":{"intent":"","payloadRef":"","requiredSkills":[],"priority":"normal","kind":"skill_transfer"},"status":"pending"}
            ]}"#,
        )
        .unwrap();
        assert!(!list.envelopes[0].is_skill_transfer());
        assert_eq!(list.envelopes[0].sender(), "Bob");
        assert!(list.envelopes[1].is_skill_transfer());
        assert_eq!(list.envelopes[1].sender(), "carol@example.com");
    }
}
