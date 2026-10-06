//! Connected storage accounts such as Google Drive and OneDrive
//! (agent-gateway routes connectors.ts; web api/connectors.js).
//!
//! The provider's OAuth callback is the web console's `/connect/callback`
//! page, which completes the connection with the browser's own session. The
//! desktop app therefore opens the authorization page in the browser and
//! reads the result back when the user returns.

use serde::{Deserialize, Serialize};

use super::encode;
use crate::connection::Connection;
use crate::error::ApiResult;

#[derive(Debug, Clone, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct ConnectorStatus {
    pub connector_id: String,
    /// 1 Google Drive, 2 OneDrive.
    #[serde(default)]
    pub provider: i64,
    #[serde(default)]
    pub display_name: String,
    #[serde(default)]
    pub scopes: Vec<String>,
    #[serde(default)]
    pub status: String,
    #[serde(default)]
    pub connected_at: Option<String>,
    /// Provisioned by the organisation; the user cannot revoke it.
    #[serde(default)]
    pub managed: bool,
}

#[derive(Debug, Clone, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct ConnectorProvider {
    #[serde(default)]
    pub provider: i64,
    /// The name `begin` takes, e.g. "google_drive" or "onedrive".
    #[serde(default)]
    pub key: String,
    #[serde(default)]
    pub display_name: String,
    #[serde(default)]
    pub connector_id: String,
}

#[derive(Deserialize)]
struct Connectors {
    #[serde(default)]
    connectors: Vec<ConnectorStatus>,
}

#[derive(Deserialize)]
struct Providers {
    #[serde(default)]
    providers: Vec<ConnectorProvider>,
}

#[derive(Serialize)]
struct BeginBody<'a> {
    provider: &'a str,
}

#[derive(Debug, Clone, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct BeginConnect {
    pub authorize_url: String,
    #[serde(default)]
    pub state: String,
}

impl Connection {
    pub async fn connectors(&self) -> ApiResult<Vec<ConnectorStatus>> {
        match self.get::<Connectors>("/connectors").await {
            Ok(body) => Ok(body.connectors),
            Err(err) if err.is_forbidden() => Ok(Vec::new()),
            Err(err) => Err(err),
        }
    }

    pub async fn connector_providers(&self) -> ApiResult<Vec<ConnectorProvider>> {
        match self.get::<Providers>("/connectors/providers").await {
            Ok(body) => Ok(body.providers),
            Err(err) if err.is_forbidden() => Ok(Vec::new()),
            Err(err) => Err(err),
        }
    }

    pub async fn begin_connect(&self, provider_key: &str) -> ApiResult<BeginConnect> {
        self.post("/connectors/begin", &BeginBody { provider: provider_key })
            .await
    }

    pub async fn revoke_connector(&self, connector_id: &str) -> ApiResult<()> {
        let _: serde_json::Value = self
            .post_empty(&format!("/connectors/{}/revoke", encode(connector_id)))
            .await?;
        Ok(())
    }
}
