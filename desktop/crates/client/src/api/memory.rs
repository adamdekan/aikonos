//! Agent memory the user can review: their own concepts and their groups'
//! (agent-gateway routes memory.ts; web api/memory.js). Query and body keys
//! are snake_case; responses are camelCase.

use serde::{Deserialize, Serialize};

use super::encode;
use crate::connection::Connection;
use crate::error::ApiResult;

#[derive(Debug, Clone, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct MemoryGroup {
    pub group_id: String,
    #[serde(default)]
    pub member: bool,
    #[serde(default)]
    pub manager: bool,
}

#[derive(Debug, Clone, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct ConceptMeta {
    pub id: String,
    #[serde(default)]
    pub scope: String,
    #[serde(default)]
    pub group_id: String,
    #[serde(default)]
    pub title: String,
    #[serde(default)]
    pub description: String,
    #[serde(default)]
    pub tags: Vec<String>,
    /// draft, stable or deprecated.
    #[serde(default)]
    pub status: String,
    /// unverified, machine-confirmed or human-reviewed.
    #[serde(default)]
    pub trust_tier: String,
    #[serde(default)]
    pub stale: bool,
    #[serde(default)]
    pub generated_at: String,
}

#[derive(Debug, Clone, Deserialize, PartialEq, Eq)]
pub struct Concept {
    pub meta: ConceptMeta,
    #[serde(default)]
    pub body: String,
    /// Hash of the content shown; verify sends it back (G19).
    #[serde(default, rename = "contentSha256")]
    pub content_sha256: String,
}

/// Whose memory: the user's own or one of their groups'.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum MemoryScope {
    User,
    Group(String),
}

impl MemoryScope {
    fn query(&self) -> String {
        match self {
            MemoryScope::User => "scope=user".into(),
            MemoryScope::Group(id) => format!("scope=group&group_id={}", encode(id)),
        }
    }
}

#[derive(Serialize)]
struct ConceptRef<'a> {
    scope: &'a str,
    #[serde(skip_serializing_if = "Option::is_none")]
    group_id: Option<&'a str>,
    id: &'a str,
    #[serde(skip_serializing_if = "Option::is_none")]
    content_sha256: Option<&'a str>,
}

impl<'a> ConceptRef<'a> {
    fn new(scope: &'a MemoryScope, id: &'a str) -> Self {
        match scope {
            MemoryScope::User => Self {
                scope: "user",
                group_id: None,
                id,
                content_sha256: None,
            },
            MemoryScope::Group(group) => Self {
                scope: "group",
                group_id: Some(group),
                id,
                content_sha256: None,
            },
        }
    }
}

#[derive(Deserialize)]
struct Groups {
    #[serde(default)]
    groups: Vec<MemoryGroup>,
}

#[derive(Deserialize)]
struct Concepts {
    #[serde(default)]
    concepts: Vec<ConceptMeta>,
}

#[derive(Deserialize)]
struct MetaReply {
    meta: ConceptMeta,
}

impl Connection {
    pub async fn memory_groups(&self) -> ApiResult<Vec<MemoryGroup>> {
        Ok(self.get::<Groups>("/memory/groups").await?.groups)
    }

    pub async fn memory_concepts(&self, scope: &MemoryScope) -> ApiResult<Vec<ConceptMeta>> {
        Ok(self
            .get::<Concepts>(&format!("/memory?{}", scope.query()))
            .await?
            .concepts)
    }

    pub async fn memory_concept(&self, scope: &MemoryScope, id: &str) -> ApiResult<Concept> {
        self.get(&format!("/memory/concept?{}&id={}", scope.query(), encode(id)))
            .await
    }

    /// Mark a concept as reviewed by a human. `content_sha256` is the hash
    /// `memory_concept` returned for the content the user reviewed; the broker
    /// refuses the verify if the concept changed since (G19).
    pub async fn verify_concept(&self, scope: &MemoryScope, id: &str, content_sha256: &str) -> ApiResult<ConceptMeta> {
        let body = ConceptRef {
            content_sha256: Some(content_sha256),
            ..ConceptRef::new(scope, id)
        };
        Ok(self.post::<_, MetaReply>("/memory/verify", &body).await?.meta)
    }

    pub async fn deprecate_concept(&self, scope: &MemoryScope, id: &str) -> ApiResult<ConceptMeta> {
        Ok(self
            .post::<_, MetaReply>("/memory/deprecate", &ConceptRef::new(scope, id))
            .await?
            .meta)
    }

    pub async fn delete_concept(&self, scope: &MemoryScope, id: &str) -> ApiResult<()> {
        let _: serde_json::Value = self.post("/memory/delete", &ConceptRef::new(scope, id)).await?;
        Ok(())
    }
}
