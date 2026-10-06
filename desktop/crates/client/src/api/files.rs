//! The user's workspace files (agent-gateway/src/routes/files.ts and
//! files-list.ts). Content travels base64-encoded in JSON, both ways.

use base64::Engine as _;
use base64::engine::general_purpose::STANDARD;
use serde::{Deserialize, Serialize};

use crate::connection::Connection;
use crate::error::{ApiError, ApiResult};

/// The broker refuses files over this many bytes (decoded, workspacefs
/// store.go); checking first gives the user the reason before the upload.
pub const MAX_UPLOAD_BYTES: usize = 10 * 1024 * 1024;

#[derive(Debug, Clone, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct WorkspaceFile {
    pub path: String,
    #[serde(default)]
    pub size: u64,
    #[serde(default)]
    pub modified: Option<String>,
    #[serde(default)]
    pub is_dir: bool,
}

impl WorkspaceFile {
    pub fn name(&self) -> &str {
        self.path.rsplit('/').next().unwrap_or(&self.path)
    }

    /// The containing folder, "" at the root.
    pub fn parent(&self) -> &str {
        self.path.rsplit_once('/').map(|(dir, _)| dir).unwrap_or("")
    }
}

#[derive(Deserialize)]
struct FileList {
    #[serde(default)]
    files: Vec<WorkspaceFile>,
}

#[derive(Debug, Clone, Deserialize)]
#[serde(rename_all = "camelCase")]
struct FileContentBody {
    #[serde(default)]
    path: String,
    #[serde(default)]
    mime: Option<String>,
    #[serde(default)]
    content_base64: String,
}

#[derive(Debug, Clone)]
pub struct FileContent {
    pub path: String,
    pub mime: Option<String>,
    pub bytes: Vec<u8>,
}

#[derive(Deserialize)]
struct FileBody {
    file: Option<WorkspaceFile>,
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct UploadBody<'a> {
    path: &'a str,
    content_base64: String,
}

#[derive(Serialize)]
struct MoveBody<'a> {
    from: &'a str,
    to: &'a str,
}

#[derive(Serialize)]
struct DirBody<'a> {
    path: &'a str,
}

use super::encode;

/// Options for [`Connection::list_files`].
#[derive(Debug, Clone, Default)]
pub struct ListFiles<'a> {
    /// The folder to list, "" for the root.
    pub dir: &'a str,
    pub recursive: bool,
    /// Include dot-files such as `.agent/`.
    pub include_hidden: bool,
}

impl Connection {
    pub async fn list_files(&self, options: ListFiles<'_>) -> ApiResult<Vec<WorkspaceFile>> {
        let mut params = Vec::new();
        if options.include_hidden {
            params.push("includeHidden=1".to_owned());
        }
        if !options.dir.is_empty() {
            params.push(format!("dir={}", encode(options.dir)));
        }
        if options.recursive {
            params.push("recursive=1".to_owned());
        }
        let path = if params.is_empty() {
            "/files".to_owned()
        } else {
            format!("/files?{}", params.join("&"))
        };
        Ok(self.get::<FileList>(&path).await?.files)
    }

    pub async fn read_file(&self, path: &str) -> ApiResult<FileContent> {
        let body: FileContentBody = self.get(&format!("/files/content?path={}", encode(path))).await?;
        let bytes = STANDARD
            .decode(body.content_base64.as_bytes())
            .map_err(|err| ApiError::Decode(format!("file content: {err}")))?;
        Ok(FileContent {
            path: if body.path.is_empty() {
                path.to_owned()
            } else {
                body.path
            },
            mime: body.mime,
            bytes,
        })
    }

    /// Write `bytes` to `path`, replacing any file there. The broker creates
    /// missing folders.
    pub async fn write_file(&self, path: &str, bytes: &[u8]) -> ApiResult<Option<WorkspaceFile>> {
        if bytes.len() > MAX_UPLOAD_BYTES {
            return Err(ApiError::Status {
                status: 400,
                message: format!(
                    "{} is larger than the {} MB upload limit.",
                    path.rsplit('/').next().unwrap_or(path),
                    MAX_UPLOAD_BYTES / (1024 * 1024)
                ),
                body: serde_json::Value::Null,
            });
        }
        let body: FileBody = self
            .post(
                "/files",
                &UploadBody {
                    path,
                    content_base64: STANDARD.encode(bytes),
                },
            )
            .await?;
        Ok(body.file)
    }

    pub async fn delete_file(&self, path: &str) -> ApiResult<()> {
        let _: serde_json::Value = self.delete(&format!("/files?path={}", encode(path))).await?;
        Ok(())
    }

    pub async fn move_file(&self, from: &str, to: &str) -> ApiResult<Option<WorkspaceFile>> {
        let body: FileBody = self.post("/files/move", &MoveBody { from, to }).await?;
        Ok(body.file)
    }

    pub async fn create_dir(&self, path: &str) -> ApiResult<()> {
        let _: serde_json::Value = self.post("/files/dir", &DirBody { path }).await?;
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn file_entries_decode_the_gateway_shape() {
        let list: FileList = serde_json::from_str(
            r#"{"files":[{"path":"reports/q3.pdf","size":2048,"modified":"2026-10-01T10:00:00.000Z","isDir":false},{"path":"reports","size":0,"modified":null,"isDir":true}]}"#,
        )
        .unwrap();
        assert_eq!(list.files[0].name(), "q3.pdf");
        assert_eq!(list.files[0].parent(), "reports");
        assert!(list.files[1].is_dir);
        assert_eq!(list.files[1].parent(), "");
    }
}
