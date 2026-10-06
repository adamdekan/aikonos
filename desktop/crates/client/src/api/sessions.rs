//! Chat session files in the workspace, read and written the way
//! webui/web/src/api/sessions.js and store/sessions.js do.

use std::collections::HashSet;

use futures::future::join_all;

use super::files::ListFiles;
use crate::connection::Connection;
use crate::error::ApiResult;
use crate::transcript::{self, LEGACY_SESSION_DIR, MANIFEST_PATH, ManifestEntry, SESSION_DIR, SessionRecord};

impl Connection {
    /// Ids of every session file (not the manifest).
    pub async fn list_session_ids(&self) -> ApiResult<Vec<String>> {
        let files = match self
            .list_files(ListFiles {
                dir: SESSION_DIR,
                include_hidden: true,
                ..Default::default()
            })
            .await
        {
            Ok(files) => files,
            Err(err) if err.is_forbidden() => return Ok(Vec::new()),
            Err(err) => return Err(err),
        };
        Ok(files
            .iter()
            .filter_map(|file| transcript::session_id_from_path(&file.path))
            .map(str::to_owned)
            .collect())
    }

    /// A session file, or `None` when it is missing or unreadable.
    pub async fn read_session(&self, id: &str) -> ApiResult<Option<SessionRecord>> {
        match self.read_file(&transcript::session_path(id)).await {
            Ok(content) => Ok(serde_json::from_slice(&content.bytes).ok()),
            Err(err) if err.is_forbidden() || err.status() == Some(404) => Ok(None),
            Err(err) => Err(err),
        }
    }

    pub async fn write_session(&self, record: &SessionRecord) -> ApiResult<()> {
        let bytes = serde_json::to_vec(record).expect("session records serialize");
        self.write_file(&transcript::session_path(&record.id), &bytes)
            .await
            .map(|_| ())
    }

    pub async fn delete_session(&self, id: &str) -> ApiResult<()> {
        self.delete_file(&transcript::session_path(id)).await
    }

    pub async fn read_manifest(&self) -> ApiResult<Vec<ManifestEntry>> {
        match self.read_file(MANIFEST_PATH).await {
            Ok(content) => Ok(serde_json::from_slice(&content.bytes).unwrap_or_default()),
            Err(err) if err.is_forbidden() || err.status() == Some(404) => Ok(Vec::new()),
            // A missing manifest surfaces as an error on some backends; treat
            // any other failure the same way the web client does: empty.
            Err(_) => Ok(Vec::new()),
        }
    }

    pub async fn write_manifest(&self, entries: &[ManifestEntry]) -> ApiResult<()> {
        let bytes = serde_json::to_vec(entries).expect("manifest serializes");
        self.write_file(MANIFEST_PATH, &bytes).await.map(|_| ())
    }

    /// Move sessions from the old top-level `Sessions/` folder into
    /// `.agent/Sessions/`. Idempotent; failures never block chat.
    pub async fn migrate_legacy_sessions(&self) {
        let legacy = self
            .list_files(ListFiles {
                dir: LEGACY_SESSION_DIR,
                include_hidden: true,
                ..Default::default()
            })
            .await
            .unwrap_or_default();
        let legacy: Vec<_> = legacy
            .into_iter()
            .filter(|file| !file.is_dir && file.path.starts_with("Sessions/"))
            .collect();
        if legacy.is_empty() {
            return;
        }
        let existing: HashSet<String> = self
            .list_files(ListFiles {
                dir: SESSION_DIR,
                include_hidden: true,
                ..Default::default()
            })
            .await
            .unwrap_or_default()
            .into_iter()
            .map(|file| file.path)
            .collect();
        for file in legacy {
            let name = &file.path["Sessions/".len()..];
            let target = format!("{SESSION_DIR}/{name}");
            let mut copied = existing.contains(&target);
            if !copied && let Ok(content) = self.read_file(&file.path).await {
                copied = self.write_file(&target, &content.bytes).await.is_ok();
            }
            if copied {
                let _ = self.delete_file(&file.path).await;
            }
        }
    }

    /// The sidebar's session list: the manifest pruned to files that still
    /// exist, plus any session file the manifest is missing (written by
    /// another client or the scheduler). Repairs the manifest when the two
    /// disagreed, then returns it sorted.
    pub async fn load_sessions(&self) -> ApiResult<Vec<ManifestEntry>> {
        self.migrate_legacy_sessions().await;
        let (manifest, ids) = futures::join!(self.read_manifest(), self.list_session_ids());
        let manifest = manifest?;
        let ids: HashSet<String> = ids?.into_iter().collect();
        let known: HashSet<&str> = manifest.iter().map(|entry| entry.id.as_str()).collect();
        let missing: Vec<&String> = ids.iter().filter(|id| !known.contains(id.as_str())).collect();
        let discovered: Vec<ManifestEntry> = join_all(missing.iter().map(|id| self.read_session(id)))
            .await
            .into_iter()
            .filter_map(|record| record.ok().flatten())
            .map(|record| ManifestEntry::from(&record))
            .collect();
        let before = manifest.len();
        let mut merged: Vec<ManifestEntry> = manifest.into_iter().filter(|entry| ids.contains(&entry.id)).collect();
        let changed = merged.len() != before || !discovered.is_empty();
        merged.extend(discovered);
        if changed {
            let _ = self.write_manifest(&merged).await;
        }
        transcript::sort_sessions(&mut merged);
        Ok(merged)
    }
}
