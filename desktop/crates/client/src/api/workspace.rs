//! Which storage the user's files and the agent's file tools use: the
//! local workspace or a OneDrive folder (agent-gateway routes
//! workspace-prefs.ts; web store/workspace.js).

use serde::{Deserialize, Serialize};

use super::encode;
use crate::connection::Connection;
use crate::error::ApiResult;

#[derive(Debug, Clone, Default, Deserialize, Serialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct BackendPref {
    #[serde(default)]
    pub backend: String,
    #[serde(default)]
    pub onedrive_folder_path: String,
}

impl BackendPref {
    pub fn is_onedrive(&self) -> bool {
        self.backend == "onedrive"
    }
}

#[derive(Debug, Clone, Default, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct WorkspaceBackend {
    #[serde(default)]
    pub pref: BackendPref,
    #[serde(default)]
    pub onedrive_available: bool,
    #[serde(default)]
    pub onedrive_status: String,
}

#[derive(Debug, Clone, Deserialize, PartialEq, Eq)]
pub struct OneDriveFolder {
    pub name: String,
    pub path: String,
}

#[derive(Deserialize)]
struct Folders {
    #[serde(default)]
    folders: Vec<OneDriveFolder>,
}

#[derive(Deserialize)]
struct PrefReply {
    #[serde(default)]
    pref: Option<BackendPref>,
}

impl Connection {
    pub async fn workspace_backend(&self) -> ApiResult<WorkspaceBackend> {
        self.get("/workspace/backend").await
    }

    pub async fn set_workspace_backend(&self, pref: &BackendPref) -> ApiResult<BackendPref> {
        let reply: PrefReply = self.put("/workspace/backend", pref).await?;
        Ok(reply.pref.unwrap_or_else(|| pref.clone()))
    }

    /// Sub-folders of `dir` in the connected OneDrive ("" for its root).
    pub async fn onedrive_folders(&self, dir: &str) -> ApiResult<Vec<OneDriveFolder>> {
        Ok(self
            .get::<Folders>(&format!("/workspace/onedrive/folders?dir={}", encode(dir)))
            .await?
            .folders)
    }
}
