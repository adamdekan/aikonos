//! The session list shared by the sidebar and the chat view: the manifest
//! rows, paged the way the web console pages them, and every change written
//! through to the workspace (store/sessions.js).
//!
//! Changes apply in memory first and are written after, so the sidebar
//! answers at once; a failed write leaves the next load to repair the
//! manifest from the session files.

use std::sync::Arc;

use aikonos_client::Connection;
use aikonos_client::transcript::{self, ManifestEntry, SessionRecord};
use gpui_kit::{AppContext as _, Context, EventEmitter, Task};

use crate::runtime;

const PAGE_SIZE: usize = 10;

pub enum SessionsEvent {
    /// A session file was deleted; the chat view drops it if it is open.
    Removed(String),
}

pub struct SessionsStore {
    connection: Arc<Connection>,
    sessions: Vec<ManifestEntry>,
    cursor: usize,
    loading: bool,
    loaded: bool,
    load_task: Option<Task<()>>,
}

impl EventEmitter<SessionsEvent> for SessionsStore {}

impl SessionsStore {
    pub fn new(connection: Arc<Connection>, cx: &mut Context<Self>) -> Self {
        let mut this = Self {
            connection,
            sessions: Vec::new(),
            cursor: PAGE_SIZE,
            loading: false,
            loaded: false,
            load_task: None,
        };
        this.reload(cx);
        this
    }

    pub fn is_loaded(&self) -> bool {
        self.loaded
    }

    pub fn all(&self) -> &[ManifestEntry] {
        &self.sessions
    }

    pub fn get(&self, id: &str) -> Option<&ManifestEntry> {
        self.sessions.iter().find(|entry| entry.id == id)
    }

    /// Pinned rows always, then unpinned rows up to the paging cursor.
    pub fn visible(&self) -> &[ManifestEntry] {
        let pinned = self.sessions.iter().filter(|entry| entry.pinned).count();
        let end = self.cursor.max(pinned).min(self.sessions.len());
        &self.sessions[..end]
    }

    pub fn has_more(&self) -> bool {
        self.visible().len() < self.sessions.len()
    }

    pub fn show_more(&mut self, cx: &mut Context<Self>) {
        if self.has_more() {
            self.cursor += PAGE_SIZE;
            cx.notify();
        }
    }

    /// Read the list from the server again. Keeps the current rows on
    /// failure: a refresh that cannot reach the server shows stale data, not
    /// an empty sidebar.
    pub fn reload(&mut self, cx: &mut Context<Self>) {
        if self.loading {
            return;
        }
        self.loading = true;
        let connection = self.connection.clone();
        self.load_task = Some(cx.spawn(async move |this, cx| {
            let result = runtime::spawn(async move { connection.load_sessions().await }).await;
            let _ = this.update(cx, |this, cx| {
                this.loading = false;
                this.loaded = true;
                if let Ok(sessions) = result {
                    this.sessions = sessions;
                }
                cx.notify();
            });
        }));
        cx.notify();
    }

    fn persist_manifest(&self) {
        let connection = self.connection.clone();
        let entries = self.sessions.clone();
        runtime::detach(async move {
            let _ = connection.write_manifest(&entries).await;
        });
    }

    fn sort(&mut self) {
        transcript::sort_sessions(&mut self.sessions);
    }

    /// Record a session the chat view just created.
    pub fn insert(&mut self, record: &SessionRecord, cx: &mut Context<Self>) {
        self.sessions.retain(|entry| entry.id != record.id);
        let entry = ManifestEntry::from(record);
        if entry.pinned {
            self.sessions.push(entry);
            self.sort();
        } else {
            let pinned = self.sessions.iter().filter(|entry| entry.pinned).count();
            self.sessions.insert(pinned, entry);
        }
        self.persist_manifest();
        cx.notify();
    }

    /// Refresh a row from a session record the chat view saved.
    pub fn upsert(&mut self, record: &SessionRecord, cx: &mut Context<Self>) {
        let entry = ManifestEntry::from(record);
        match self.sessions.iter_mut().find(|existing| existing.id == record.id) {
            Some(existing) => *existing = entry,
            None => self.sessions.push(entry),
        }
        self.sort();
        self.persist_manifest();
        cx.notify();
    }

    pub fn rename(&mut self, id: &str, title: String, cx: &mut Context<Self>) {
        let Some(entry) = self.sessions.iter_mut().find(|entry| entry.id == id) else {
            return;
        };
        entry.title = title.clone();
        self.persist_manifest();
        cx.notify();
        let connection = self.connection.clone();
        let id = id.to_owned();
        cx.spawn(async move |this, cx| {
            let updated = runtime::spawn(async move {
                let Some(mut record) = connection.read_session(&id).await? else {
                    return Ok(None);
                };
                record.title = title;
                record.updated_at = Some(transcript::now_iso());
                connection.write_session(&record).await?;
                Ok(Some(record))
            })
            .await;
            if let Ok(Some(record)) = updated {
                let _ = this.update(cx, |this, cx| this.upsert(&record, cx));
            }
        })
        .detach();
    }

    pub fn toggle_pin(&mut self, id: &str, cx: &mut Context<Self>) {
        let Some(entry) = self.sessions.iter_mut().find(|entry| entry.id == id) else {
            return;
        };
        let pinned = !entry.pinned;
        let pinned_at = pinned.then(transcript::now_iso);
        entry.pinned = pinned;
        entry.pinned_at = pinned_at.clone();
        self.sort();
        self.persist_manifest();
        cx.notify();
        let connection = self.connection.clone();
        let id = id.to_owned();
        runtime::detach(async move {
            if let Ok(Some(mut record)) = connection.read_session(&id).await {
                record.pinned = pinned;
                record.pinned_at = pinned_at;
                record.updated_at = Some(transcript::now_iso());
                let _ = connection.write_session(&record).await;
            }
        });
    }

    pub fn remove(&mut self, id: &str, cx: &mut Context<Self>) {
        self.sessions.retain(|entry| entry.id != id);
        self.persist_manifest();
        cx.emit(SessionsEvent::Removed(id.to_owned()));
        cx.notify();
        let connection = self.connection.clone();
        let id = id.to_owned();
        runtime::detach(async move {
            let _ = connection.delete_session(&id).await;
        });
    }
}

/// A new store, ready to share between views.
pub fn new_store(connection: Arc<Connection>, cx: &mut gpui_kit::App) -> gpui_kit::Entity<SessionsStore> {
    cx.new(|cx| SessionsStore::new(connection, cx))
}
