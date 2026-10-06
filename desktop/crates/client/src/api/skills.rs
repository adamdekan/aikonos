//! The user's personal skills (agent-gateway routes skills.ts; web
//! api/skills.js). Request bodies are snake_case, responses camelCase.

use reqwest::Method;
use serde::{Deserialize, Serialize};

use super::chat::SkillBundle;
use super::encode;
use crate::connection::Connection;
use crate::error::{ApiError, ApiResult};

#[derive(Debug, Clone, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct PersonalSkill {
    pub name: String,
    #[serde(default)]
    pub description: String,
    #[serde(default)]
    pub keywords: Vec<String>,
    #[serde(default)]
    pub allowed_tools: Vec<String>,
    #[serde(default)]
    pub disable_model_invocation: bool,
    #[serde(default)]
    pub valid: bool,
    #[serde(default)]
    pub warning: String,
    #[serde(default)]
    pub size_bytes: u64,
}

#[derive(Debug, Clone, Default, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct SkillsPage {
    #[serde(default)]
    pub skills: Vec<PersonalSkill>,
    /// Admin-governed bundles the user is granted.
    #[serde(default)]
    pub granted: Vec<SkillBundle>,
    #[serde(default)]
    pub granted_unavailable: bool,
}

#[derive(Deserialize)]
struct Imported {
    #[serde(default)]
    name: String,
}

#[derive(Serialize)]
#[serde(untagged)]
enum ShareTo<'a> {
    User { user_id: &'a str },
    Group { group_id: &'a str },
}

#[derive(Debug, Clone, Default, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct ShareResult {
    #[serde(default)]
    pub envelope_ids: Vec<String>,
    #[serde(default)]
    pub skipped_user_ids: Vec<String>,
}

/// Who to share a skill with.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum ShareRecipient {
    User(String),
    Group(String),
}

/// A failed import that the server answered with a free name to retry
/// under (HTTP 409).
pub fn suggested_name(err: &ApiError) -> Option<String> {
    (err.status() == Some(409))
        .then(|| err.body()?.get("suggested_name")?.as_str().map(str::to_owned))
        .flatten()
}

impl Connection {
    /// The Skills page. `Forbidden` when the user lacks the personal-skills
    /// grant; the page shows its no-access panel then.
    pub async fn personal_skills(&self) -> ApiResult<SkillsPage> {
        self.get("/skills").await
    }

    /// Import a `SKILL.md` or a `.skill`/`.zip` bundle. Returns the name it
    /// was installed under.
    pub async fn import_skill(&self, bytes: Vec<u8>, is_zip: bool) -> ApiResult<String> {
        let content_type = if is_zip { "application/zip" } else { "text/markdown" };
        let reply: Imported = self.upload(Method::POST, "/skills/import", bytes, content_type).await?;
        Ok(reply.name)
    }

    pub async fn delete_skill(&self, name: &str) -> ApiResult<()> {
        let _: serde_json::Value = self.delete(&format!("/skills/{}", encode(name))).await?;
        Ok(())
    }

    pub async fn share_skill(&self, name: &str, recipient: &ShareRecipient) -> ApiResult<ShareResult> {
        let body = match recipient {
            ShareRecipient::User(id) => ShareTo::User { user_id: id },
            ShareRecipient::Group(id) => ShareTo::Group { group_id: id },
        };
        self.post(&format!("/skills/{}/share", encode(name)), &body).await
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn share_bodies_are_snake_case() {
        assert_eq!(
            serde_json::to_value(ShareTo::Group { group_id: "ward-4" }).unwrap(),
            serde_json::json!({"group_id": "ward-4"})
        );
    }

    #[test]
    fn conflicts_carry_a_suggested_name() {
        let err = ApiError::Status {
            status: 409,
            message: "a skill named \"mine\" already exists".into(),
            body: serde_json::json!({"error": "…", "suggested_name": "mine-2"}),
        };
        assert_eq!(suggested_name(&err).as_deref(), Some("mine-2"));
    }
}
