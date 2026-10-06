//! A conversation (webui views/Chat.vue with its composables useAguiRun,
//! useApprovals, useSessionLifecycle, useDelegation and useSoulEditor).
//!
//! The transcript is saved to the same session file the web console uses,
//! after the first message and again when each reply ends, so the
//! conversation continues in either client. A run streams over AG-UI; tool
//! calls the policy routes to a human pause the run until the user decides
//! in the approval dialog, which opens over whatever page is showing.

mod approval;
pub mod composer;
pub mod onedrive;
mod transcript;

use std::collections::{HashSet, VecDeque};
use std::sync::Arc;
use std::time::Duration;

use aikonos_client::agui::{AguiEvent, ApprovalRequest, RunRequest};
use aikonos_client::api::chat::{DelegationTarget, SessionUsage};
use aikonos_client::transcript::{
    self as record, AssistantTurn, BranchStatus, ManifestEntry, SessionRecord, SubagentBranch, ToolCall,
    TranscriptEntry,
};
use aikonos_client::{Connection, StreamItem};
use gpui_kit::component::WindowExt as _;
use gpui_kit::component::button::{Button, ButtonVariants as _};
use gpui_kit::component::dialog::DialogFooter;
use gpui_kit::component::input::{Textarea, TextareaState};
use gpui_kit::component::message_scroller::{MessageScroller, MessageScrollerState};
use gpui_kit::component::notification::Notification;
use gpui_kit::component::text::TextViewState;
use gpui_kit::component::{ActiveTheme as _, Disableable as _, Sizable as _, h_flex, v_flex};
use gpui_kit::prelude::FluentBuilder as _;
use gpui_kit::*;

use self::approval::{ApprovalEvent, ApprovalPanel};
use self::composer::{Composer, ComposerEvent, Discovery, Submission};
use crate::app;
use crate::prefs::Prefs;
use crate::runtime;
use crate::sessions::{SessionsEvent, SessionsStore};
use crate::ui;

pub use self::composer::bind_keys;

/// The web console polls pending approvals every 2 s once one has arrived,
/// and once 2 s after a run starts in case the stream dropped the frame.
const APPROVAL_POLL: Duration = Duration::from_secs(2);
/// Usage lands in the broker shortly after a run ends; read it again then.
const USAGE_SETTLE: Duration = Duration::from_secs(2);
const SOUL_MAX_BYTES: usize = 4096;
const CONNECTION_LOST: &str = "Connection lost — the response may be incomplete.";

struct Row {
    key: u64,
    entry: TranscriptEntry,
    /// Rendered Markdown for an assistant turn with text.
    markdown: Option<Entity<TextViewState>>,
}

/// One AG-UI run in flight.
struct Run {
    /// Reads the stream; dropping it ends the read.
    _task: Task<()>,
    abort: Option<tokio::task::AbortHandle>,
    /// The assistant row this run writes into, once RUN_STARTED arrived.
    assistant: Option<u64>,
    settled: bool,
}

/// The approval dialog on screen.
struct OpenApproval {
    panel: Entity<ApprovalPanel>,
    _subscription: Subscription,
}

pub struct ChatView {
    connection: Arc<Connection>,
    sessions: Entity<SessionsStore>,
    // Which conversation.
    session_id: Option<String>,
    thread_id: String,
    agent_id: Option<String>,
    agent_name: Option<String>,
    /// The loaded session file, for the fields a save must carry over.
    base: Option<SessionRecord>,
    /// Bumped on every switch, so a late load for another conversation is
    /// dropped instead of applied.
    generation: u64,
    loading: bool,
    // The transcript.
    rows: Vec<Row>,
    next_key: u64,
    scroller: Entity<MessageScrollerState>,
    editing: Option<(u64, Entity<TextareaState>)>,
    expanded_tools: HashSet<String>,
    // The run.
    run: Option<Run>,
    text_streaming: bool,
    // Approvals.
    approvals: VecDeque<ApprovalRequest>,
    handled: HashSet<String>,
    approval: Option<OpenApproval>,
    approval_poll: Option<Task<()>>,
    /// The poll failed; told once per run, from the next render.
    approval_poll_failed: bool,
    // Around the transcript.
    composer: Entity<Composer>,
    usage: Option<SessionUsage>,
    usage_task: Option<Task<()>>,
    soul: Option<String>,
    discovery_failed: bool,
    _subscriptions: Vec<Subscription>,
}

impl ChatView {
    pub fn new(sessions: Entity<SessionsStore>, window: &mut Window, cx: &mut Context<Self>) -> Self {
        let connection = app::connection(cx);
        let composer = cx.new(|cx| Composer::new(connection.clone(), window, cx));
        let scroller = cx.new(|cx| MessageScrollerState::new(0, cx));
        let composer_events = cx.subscribe_in(&composer, window, |this, _, event, window, cx| match event {
            ComposerEvent::Submit(submission) => this.on_submit(submission, window, cx),
            ComposerEvent::Stop => this.stop(cx),
            ComposerEvent::WorkspaceChanged => this.load_discovery(cx),
        });
        let session_events = cx.subscribe_in(&sessions, window, |this, _, event, _, cx| match event {
            SessionsEvent::Removed(id) => {
                if this.session_id.as_deref() == Some(id.as_str()) {
                    this.reset(None, None, cx);
                }
            }
        });
        let mut this = Self {
            connection,
            sessions,
            session_id: None,
            thread_id: uuid::Uuid::new_v4().to_string(),
            agent_id: None,
            agent_name: None,
            base: None,
            generation: 0,
            loading: false,
            rows: Vec::new(),
            next_key: 0,
            scroller,
            editing: None,
            expanded_tools: HashSet::new(),
            run: None,
            text_streaming: false,
            approvals: VecDeque::new(),
            handled: HashSet::new(),
            approval: None,
            approval_poll: None,
            approval_poll_failed: false,
            composer,
            usage: None,
            usage_task: None,
            soul: None,
            discovery_failed: false,
            _subscriptions: vec![composer_events, session_events],
        };
        this.load_discovery(cx);
        this
    }

    pub fn session_id(&self) -> Option<String> {
        self.session_id.clone()
    }

    pub fn agent_id(&self) -> Option<String> {
        self.agent_id.clone()
    }

    fn running(&self) -> bool {
        self.run.is_some()
    }

    pub fn focus_composer(&self, window: &mut Window, cx: &mut Context<Self>) {
        self.composer.update(cx, |composer, cx| composer.focus(window, cx));
    }

    /// Put `text` in the composer without sending it.
    pub fn set_draft(&mut self, text: String, window: &mut Window, cx: &mut Context<Self>) {
        self.composer
            .update(cx, |composer, cx| composer.set_text(text, window, cx));
    }

    /// Send `text` as if typed and submitted.
    pub fn send_text(&mut self, text: String, window: &mut Window, cx: &mut Context<Self>) {
        self.on_submit(&Submission::Text(text), window, cx);
    }

    // ── switching conversations ─────────────────────────────────────────

    /// Show a saved session, or a fresh conversation (with an agent or the
    /// default one).
    pub fn open(
        &mut self,
        session: Option<String>,
        agent: Option<String>,
        agent_name: Option<String>,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        if session.is_some() && session == self.session_id {
            return;
        }
        // Leaving a conversation mid-reply stops it, the same as Stop.
        if self.running() {
            self.stop(cx);
        }
        let agent_changed = agent != self.agent_id;
        self.reset(agent.clone(), agent_name, cx);
        if let Some(id) = session {
            self.load_session(id, window, cx);
        }
        if agent_changed || self.soul.is_none() {
            self.load_soul(cx);
        }
        self.focus_composer(window, cx);
    }

    fn reset(&mut self, agent: Option<String>, agent_name: Option<String>, cx: &mut Context<Self>) {
        self.generation += 1;
        self.session_id = None;
        self.base = None;
        self.thread_id = uuid::Uuid::new_v4().to_string();
        self.agent_id = agent;
        self.agent_name = agent_name;
        self.loading = false;
        self.editing = None;
        self.expanded_tools.clear();
        self.usage = None;
        self.usage_task = None;
        self.set_rows(Vec::new(), cx);
        cx.notify();
    }

    fn load_session(&mut self, id: String, window: &mut Window, cx: &mut Context<Self>) {
        self.session_id = Some(id.clone());
        self.loading = true;
        let generation = self.generation;
        let connection = self.connection.clone();
        cx.spawn_in(window, async move |this, cx| {
            let loaded = {
                let id = id.clone();
                runtime::spawn(async move { connection.read_session(&id).await }).await
            };
            let _ = this.update_in(cx, |this, window, cx| {
                if this.generation != generation {
                    return;
                }
                this.loading = false;
                match loaded {
                    Ok(Some(record)) => {
                        if let Some(thread) = record.thread_id.clone().filter(|t| !t.is_empty()) {
                            this.thread_id = thread;
                        }
                        if this.agent_id.is_none() {
                            this.agent_id = record.agent_id.clone().filter(|a| !a.is_empty());
                            this.agent_name = record.agent_name.clone().filter(|a| !a.is_empty());
                        }
                        let entries = record.messages.clone();
                        this.base = Some(record);
                        this.set_rows(entries, cx);
                        this.load_usage(cx);
                    }
                    Ok(None) => {
                        this.session_id = None;
                        app::toast_error("That conversation could not be opened.", window, cx);
                    }
                    Err(err) => {
                        this.session_id = None;
                        app::report(&err, window, cx);
                    }
                }
                cx.notify();
            });
        })
        .detach();
        cx.notify();
    }

    // ── the transcript ──────────────────────────────────────────────────

    fn make_row(&mut self, entry: TranscriptEntry, cx: &mut Context<Self>) -> Row {
        self.next_key += 1;
        let markdown = match &entry {
            TranscriptEntry::Assistant(turn) if !turn.text.is_empty() => {
                let text = turn.text.clone();
                Some(cx.new(|cx| TextViewState::markdown(&text, cx).selectable(true)))
            }
            _ => None,
        };
        Row {
            key: self.next_key,
            entry,
            markdown,
        }
    }

    fn set_rows(&mut self, entries: Vec<TranscriptEntry>, cx: &mut Context<Self>) {
        let rows: Vec<Row> = entries.into_iter().map(|entry| self.make_row(entry, cx)).collect();
        self.rows = rows;
        let count = self.rows.len();
        self.scroller.update(cx, |scroller, cx| scroller.reset(count, cx));
    }

    fn insert_row(&mut self, ix: usize, entry: TranscriptEntry, cx: &mut Context<Self>) -> u64 {
        let row = self.make_row(entry, cx);
        let key = row.key;
        let ix = ix.min(self.rows.len());
        self.rows.insert(ix, row);
        self.scroller.update(cx, |scroller, cx| {
            scroller.splice(ix..ix, 1, cx);
        });
        key
    }

    fn push_row(&mut self, entry: TranscriptEntry, cx: &mut Context<Self>) -> u64 {
        let ix = self.rows.len();
        self.insert_row(ix, entry, cx)
    }

    fn truncate_rows(&mut self, len: usize, cx: &mut Context<Self>) {
        if len >= self.rows.len() {
            return;
        }
        let old = self.rows.len();
        self.rows.truncate(len);
        self.scroller.update(cx, |scroller, cx| {
            scroller.splice(len..old, 0, cx);
        });
    }

    fn row_ix(&self, key: u64) -> Option<usize> {
        self.rows.iter().position(|row| row.key == key)
    }

    fn remeasure(&self, ix: usize, cx: &mut Context<Self>) {
        self.scroller.update(cx, |scroller, cx| {
            scroller.remeasure_items(ix..ix + 1, cx);
        });
    }

    fn entries(&self) -> Vec<TranscriptEntry> {
        self.rows.iter().map(|row| row.entry.clone()).collect()
    }

    /// The run's assistant row, created on demand (a frame can arrive
    /// before RUN_STARTED was seen, as in the web client).
    fn assistant_ix(&mut self, cx: &mut Context<Self>) -> usize {
        if let Some(key) = self.run.as_ref().and_then(|run| run.assistant)
            && let Some(ix) = self.row_ix(key)
        {
            return ix;
        }
        if let Some(ix) = self.rows.len().checked_sub(1)
            && matches!(self.rows[ix].entry, TranscriptEntry::Assistant(_))
            && self.run.is_some()
        {
            let key = self.rows[ix].key;
            if let Some(run) = self.run.as_mut() {
                run.assistant = Some(key);
            }
            return ix;
        }
        let key = self.push_row(TranscriptEntry::Assistant(AssistantTurn::default()), cx);
        if let Some(run) = self.run.as_mut() {
            run.assistant = Some(key);
        }
        self.rows.len() - 1
    }

    fn with_assistant(&mut self, cx: &mut Context<Self>, edit: impl FnOnce(&mut AssistantTurn)) -> usize {
        let ix = self.assistant_ix(cx);
        if let TranscriptEntry::Assistant(turn) = &mut self.rows[ix].entry {
            edit(turn);
        }
        self.remeasure(ix, cx);
        ix
    }

    fn append_text(&mut self, delta: &str, cx: &mut Context<Self>) {
        let ix = self.assistant_ix(cx);
        let row = &mut self.rows[ix];
        if let TranscriptEntry::Assistant(turn) = &mut row.entry {
            turn.text.push_str(delta);
        }
        match &row.markdown {
            Some(markdown) => markdown.update(cx, |state, cx| state.push_str(delta, cx)),
            None => {
                let text = delta.to_owned();
                row.markdown = Some(cx.new(|cx| TextViewState::markdown(&text, cx).selectable(true)));
            }
        }
        self.remeasure(ix, cx);
    }

    /// Where announcements (skills, memory, sub-agents) go: just before the
    /// run's reply, so the timeline sits between question and answer.
    fn announcement_ix(&self) -> usize {
        self.run
            .as_ref()
            .and_then(|run| run.assistant)
            .and_then(|key| self.row_ix(key))
            .unwrap_or(self.rows.len())
    }

    // ── sending ─────────────────────────────────────────────────────────

    fn on_submit(&mut self, submission: &Submission, window: &mut Window, cx: &mut Context<Self>) {
        match submission {
            Submission::Text(text) => {
                let text = text.trim().to_owned();
                if !text.is_empty() {
                    self.run_chat(text, None, window, cx);
                }
            }
            // The bubble shows "/name"; the gateway resolves and gates the skill.
            Submission::Skill(bundle) => {
                let text = format!("/{}", bundle.name);
                self.run_chat(text, Some(bundle.run_name().to_owned()), window, cx)
            }
            Submission::Delegate { text, target } => self.confirm_delegation(text.clone(), target.clone(), window, cx),
        }
    }

    fn run_chat(&mut self, text: String, skill_name: Option<String>, window: &mut Window, cx: &mut Context<Self>) {
        if self.running() || self.loading {
            return;
        }
        let history = record::history_from(&self.entries());
        self.push_row(TranscriptEntry::user(text.clone()), cx);
        self.run = Some(Run {
            _task: Task::ready(()),
            abort: None,
            assistant: None,
            settled: false,
        });
        self.text_streaming = false;
        self.composer
            .update(cx, |composer, cx| composer.set_running(true, window, cx));
        cx.notify();

        let connection = self.connection.clone();
        let instructions = Prefs::global(cx).chat_instructions.trim().to_owned();
        let create = self.session_id.is_none().then(|| self.new_record(&text));
        let generation = self.generation;
        let task = cx.spawn_in(window, async move |this, cx| {
            // A new conversation is saved before the run starts, so the run
            // can attribute its spend to it (useSessionLifecycle).
            if let Some(record) = create {
                let saved = {
                    let connection = connection.clone();
                    let record = record.clone();
                    runtime::spawn(async move { connection.write_session(&record).await }).await
                };
                let _ = this.update_in(cx, |this, window, cx| {
                    if this.generation != generation {
                        return;
                    }
                    match saved {
                        Ok(()) => {
                            this.session_id = Some(record.id.clone());
                            this.sessions.update(cx, |store, cx| store.insert(&record, cx));
                            this.base = Some(record);
                        }
                        Err(_) => app::toast_error(
                            "Couldn't save this conversation — will retry on your next message.",
                            window,
                            cx,
                        ),
                    }
                });
            }
            let request = match this.update(cx, |this, _| RunRequest {
                prompt: text,
                thread_id: this.thread_id.clone(),
                agent_id: this.agent_id.clone(),
                history,
                skill_name,
                user_instructions: (!instructions.is_empty()).then_some(instructions),
                session_id: this.session_id.clone(),
            }) {
                Ok(request) => request,
                Err(_) => return,
            };
            let started = {
                let connection = connection.clone();
                runtime::spawn(async move { connection.run_agui(&request).await }).await
            };
            let mut stream = match started {
                Ok(stream) => stream,
                Err(err) => {
                    let _ = this.update_in(cx, |this, window, cx| {
                        if err.is_unauthorized() {
                            app::session_expired(window, cx);
                        }
                        this.finish_run(Some(err.to_string()), window, cx);
                    });
                    return;
                }
            };
            let abort = stream.abort_handle();
            let _ = this.update(cx, |this, _| {
                if let Some(run) = this.run.as_mut() {
                    run.abort = Some(abort);
                }
            });
            while let Some(item) = stream.next().await {
                let keep_going = this.update_in(cx, |this, window, cx| match item {
                    StreamItem::Event(event) => {
                        if let Some(event) = aikonos_client::agui::parse_event(&event.data) {
                            this.on_event(event, window, cx);
                        }
                        this.run.as_ref().is_some_and(|run| !run.settled)
                    }
                    StreamItem::Failed(_) | StreamItem::Ended => false,
                });
                if !matches!(keep_going, Ok(true)) {
                    break;
                }
            }
            // A stream that ended without a verdict lost its connection.
            let _ = this.update_in(cx, |this, window, cx| {
                if this.run.as_ref().is_some_and(|run| !run.settled) {
                    this.finish_run(Some(CONNECTION_LOST.to_owned()), window, cx);
                }
            });
        });
        if let Some(run) = self.run.as_mut() {
            run._task = task;
        }
    }

    fn new_record(&self, first_message: &str) -> SessionRecord {
        let now = record::now_iso();
        SessionRecord {
            id: uuid::Uuid::new_v4().to_string(),
            title: record::title_from(first_message),
            agent_id: self.agent_id.clone(),
            agent_name: Some(self.agent_name.clone().unwrap_or_default()),
            pinned: false,
            pinned_at: None,
            created_at: Some(now.clone()),
            updated_at: Some(now),
            thread_id: Some(self.thread_id.clone()),
            first_message: Some(first_message.to_owned()),
            messages: self.entries(),
            source: None,
            schedule_id: None,
            extra: Default::default(),
        }
    }

    fn on_event(&mut self, event: AguiEvent, window: &mut Window, cx: &mut Context<Self>) {
        match event {
            AguiEvent::RunStarted => {
                let key = self.push_row(TranscriptEntry::Assistant(AssistantTurn::default()), cx);
                if let Some(run) = self.run.as_mut() {
                    run.assistant = Some(key);
                }
                self.schedule_approval_check(cx);
            }
            AguiEvent::TextStart => {
                self.assistant_ix(cx);
                self.text_streaming = true;
            }
            AguiEvent::Text(delta) => self.append_text(&delta, cx),
            AguiEvent::TextEnd => self.text_streaming = false,
            AguiEvent::ToolCallStart { id, name, description } => {
                self.with_assistant(cx, |turn| turn.tools.push(ToolCall::new(id, name, description)));
            }
            AguiEvent::ToolCallArgs { id, args_json } => {
                self.with_assistant(cx, |turn| {
                    if let Some(tool) = turn.tool_mut(&id) {
                        tool.args_json = args_json;
                    }
                });
            }
            AguiEvent::ToolCallEnd { .. } => {}
            AguiEvent::ToolCallResult { id, content, is_error } => {
                self.with_assistant(cx, |turn| match turn.tool_mut(&id) {
                    Some(tool) => {
                        tool.result = Some(content.into());
                        tool.is_error |= is_error;
                        tool.done = true;
                    }
                    None => {
                        let mut tool = ToolCall::new(id.clone(), id.clone(), None);
                        tool.result = Some(content.into());
                        tool.is_error = is_error;
                        tool.done = true;
                        turn.tools.push(tool);
                    }
                });
                // The call ran or was refused: its approval no longer waits.
                self.settle_approval(&id, window, cx);
            }
            AguiEvent::ToolError { tool_call_id, content } => {
                self.with_assistant(cx, |turn| {
                    if let Some(tool) = turn.tool_mut(&tool_call_id) {
                        tool.result = Some(content.into());
                        tool.is_error = true;
                        tool.done = true;
                    }
                });
            }
            AguiEvent::ApprovalRequest(request) => {
                self.enqueue_approval(*request, window, cx);
                self.start_approval_poll(cx);
            }
            AguiEvent::SkillsLoaded(skills) => {
                let seen: HashSet<(String, String)> = self
                    .rows
                    .iter()
                    .filter_map(|row| match &row.entry {
                        TranscriptEntry::Skills(skills) => Some(skills),
                        _ => None,
                    })
                    .flatten()
                    .map(|skill| (skill.name.clone(), skill.status.clone()))
                    .collect();
                let fresh: Vec<_> = skills
                    .into_iter()
                    .filter(|skill| !seen.contains(&(skill.name.clone(), skill.status.clone())))
                    .collect();
                if !fresh.is_empty() {
                    let ix = self.announcement_ix();
                    self.insert_row(ix, TranscriptEntry::Skills(fresh), cx);
                }
            }
            AguiEvent::MemoryRecalled(concepts) => {
                let seen: HashSet<(String, String)> = self
                    .rows
                    .iter()
                    .filter_map(|row| match &row.entry {
                        TranscriptEntry::Memory(concepts) => Some(concepts),
                        _ => None,
                    })
                    .flatten()
                    .map(|concept| (concept.id.clone(), concept.scope.clone()))
                    .collect();
                let fresh: Vec<_> = concepts
                    .into_iter()
                    .filter(|concept| !seen.contains(&(concept.id.clone(), concept.scope.clone())))
                    .collect();
                if !fresh.is_empty() {
                    let ix = self.announcement_ix();
                    self.insert_row(ix, TranscriptEntry::Memory(fresh), cx);
                }
            }
            AguiEvent::SubagentSpawned { index, task, role } => {
                let branch = SubagentBranch {
                    index,
                    task,
                    role,
                    status: BranchStatus::Running,
                    failure: None,
                    cost: 0.0,
                };
                self.add_branch(branch, cx);
            }
            AguiEvent::SubagentCompleted {
                index,
                task,
                role,
                ok,
                failure,
                cost,
            } => {
                let status = if ok { BranchStatus::Ok } else { BranchStatus::Failure };
                // The most recent still-running branch at this index.
                let target = self
                    .rows
                    .iter()
                    .enumerate()
                    .rev()
                    .find_map(|(ix, row)| match &row.entry {
                        TranscriptEntry::Subagents(branches) => branches
                            .iter()
                            .position(|b| b.index == index && b.status == BranchStatus::Running)
                            .map(|bix| (ix, bix)),
                        _ => None,
                    });
                match target {
                    Some((ix, bix)) => {
                        if let TranscriptEntry::Subagents(branches) = &mut self.rows[ix].entry {
                            let branch = &mut branches[bix];
                            if let Some(task) = task {
                                branch.task = task;
                            }
                            if role.is_some() {
                                branch.role = role;
                            }
                            branch.status = status;
                            branch.failure = failure;
                            branch.cost = cost;
                        }
                        self.remeasure(ix, cx);
                    }
                    // Completed without a spawn seen (a reconnect): its own row.
                    None => self.add_branch(
                        SubagentBranch {
                            index,
                            task: task.unwrap_or_default(),
                            role,
                            status,
                            failure,
                            cost,
                        },
                        cx,
                    ),
                }
            }
            AguiEvent::RunFinished => {
                if let Some(run) = self.run.as_mut() {
                    run.settled = true;
                }
                self.finish_run(None, window, cx);
            }
            AguiEvent::RunError(message) => {
                if let Some(run) = self.run.as_mut() {
                    run.settled = true;
                }
                self.finish_run(Some(message), window, cx);
            }
        }
        cx.notify();
    }

    /// Group sub-agent branches by fan-out: join the latest group unless it
    /// already has this index, which means a new fan-out began.
    fn add_branch(&mut self, branch: SubagentBranch, cx: &mut Context<Self>) {
        let latest = self
            .rows
            .iter()
            .enumerate()
            .rev()
            .find_map(|(ix, row)| match &row.entry {
                TranscriptEntry::Subagents(branches) => Some((ix, branches.iter().any(|b| b.index == branch.index))),
                _ => None,
            });
        match latest {
            Some((ix, false)) => {
                if let TranscriptEntry::Subagents(branches) = &mut self.rows[ix].entry {
                    branches.push(branch);
                }
                self.remeasure(ix, cx);
            }
            _ => {
                let ix = self.announcement_ix();
                self.insert_row(ix, TranscriptEntry::Subagents(vec![branch]), cx);
            }
        }
    }

    /// End the run: record an error on its reply if there was one, stop
    /// approvals, save the transcript and refresh the usage strip.
    fn finish_run(&mut self, error: Option<String>, window: &mut Window, cx: &mut Context<Self>) {
        if let Some(error) = error {
            self.with_assistant(cx, |turn| turn.error = Some(error));
        }
        self.run = None;
        self.text_streaming = false;
        self.stop_approvals(window, cx);
        self.composer
            .update(cx, |composer, cx| composer.set_running(false, window, cx));
        self.persist(cx);
        self.load_usage(cx);
        let generation = self.generation;
        self.usage_task = Some(cx.spawn(async move |this, cx| {
            cx.background_executor().timer(USAGE_SETTLE).await;
            let _ = this.update(cx, |this, cx| {
                if this.generation == generation {
                    this.load_usage(cx);
                }
            });
        }));
        cx.notify();
    }

    fn stop(&mut self, cx: &mut Context<Self>) {
        let Some(run) = self.run.take() else {
            return;
        };
        if let Some(abort) = run.abort {
            abort.abort();
        }
        self.text_streaming = false;
        self.approvals.clear();
        self.handled.clear();
        self.approval_poll = None;
        // The reader is gone with `run`; the dialog, if any, closes on its
        // next render check (see `render`).
        if let Some(open) = &self.approval {
            open.panel.update(cx, |panel, _| panel.mark_settled());
        }
        self.composer.update(cx, |composer, cx| composer.stop_running(cx));
        self.persist(cx);
        cx.notify();
    }

    /// Save the transcript to the session file (useSessionLifecycle
    /// `persistSessionMessages`).
    fn persist(&mut self, cx: &mut Context<Self>) {
        let Some(id) = self.session_id.clone() else {
            return;
        };
        let manifest: Option<ManifestEntry> = self.sessions.read(cx).get(&id).cloned();
        let mut record = self.base.clone().unwrap_or_else(|| SessionRecord {
            id: id.clone(),
            title: String::new(),
            agent_id: self.agent_id.clone(),
            agent_name: self.agent_name.clone(),
            pinned: false,
            pinned_at: None,
            created_at: Some(record::now_iso()),
            updated_at: None,
            thread_id: None,
            first_message: None,
            messages: Vec::new(),
            source: None,
            schedule_id: None,
            extra: Default::default(),
        });
        // The sidebar may have renamed or pinned it since it was loaded.
        if let Some(entry) = manifest {
            record.title = entry.title;
            record.pinned = entry.pinned;
            record.pinned_at = entry.pinned_at;
        }
        let entries = self.entries();
        record.first_message = entries.iter().find_map(|entry| match entry {
            TranscriptEntry::User(turn) => Some(turn.text.clone()),
            _ => None,
        });
        record.messages = entries;
        record.thread_id = Some(self.thread_id.clone());
        record.updated_at = Some(record::now_iso());
        self.base = Some(record.clone());
        self.sessions.update(cx, |store, cx| store.upsert(&record, cx));
        let connection = self.connection.clone();
        runtime::detach(async move {
            let _ = connection.write_session(&record).await;
        });
    }

    // ── approvals ───────────────────────────────────────────────────────

    fn enqueue_approval(&mut self, request: ApprovalRequest, window: &mut Window, cx: &mut Context<Self>) {
        if !self.handled.insert(request.tool_call_id.clone()) {
            return;
        }
        if !window.is_window_active() {
            window.push_notification(
                Notification::warning(format!("{} is waiting for your approval.", request.title()))
                    .title("Approval needed")
                    .in_app_and_system(),
                cx,
            );
        }
        self.approvals.push_back(request);
        self.show_next_approval(window, cx);
    }

    fn show_next_approval(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        if let Some(open) = &self.approval {
            let len = self.approvals.len();
            open.panel.update(cx, |panel, cx| panel.set_queue_len(len, cx));
            return;
        }
        let Some(request) = self.approvals.front().cloned() else {
            return;
        };
        let len = self.approvals.len();
        let connection = self.connection.clone();
        let panel = cx.new(|cx| ApprovalPanel::new(connection, request, len, cx));
        let subscription = cx.subscribe_in(&panel, window, |this, _, event, window, cx| match event {
            ApprovalEvent::Decided { tool_call_id } => {
                let id = tool_call_id.clone();
                this.approvals.retain(|request| request.tool_call_id != id);
                this.close_approval(window, cx);
                this.show_next_approval(window, cx);
            }
        });
        let dialog_panel = panel.clone();
        window.open_dialog(cx, move |dialog, _, cx| {
            let on_close = dialog_panel.clone();
            dialog
                .w(px(560.))
                .bg(cx.theme().popover)
                .overlay_closable(false)
                .close_button(false)
                .child(dialog_panel.clone())
                .footer(
                    DialogFooter::new()
                        .child(dialog_panel.update(cx, |panel, cx| panel.render_footer(cx).into_any_element())),
                )
                .on_close(move |_, _, cx| {
                    on_close.update(cx, |panel, _| panel.deny_if_undecided());
                })
        });
        // The deny button exists once the dialog has rendered.
        let focus_panel = panel.clone();
        window.on_next_frame(move |window, cx| {
            focus_panel.update(cx, |panel, cx| panel.focus_deny(window, cx));
        });
        self.approval = Some(OpenApproval {
            panel,
            _subscription: subscription,
        });
    }

    fn close_approval(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        if let Some(open) = self.approval.take() {
            open.panel.update(cx, |panel, _| panel.mark_settled());
            if window.has_active_dialog(cx) {
                window.close_dialog(cx);
            }
        }
    }

    /// A tool call's result arrived: drop its approval if still waiting.
    fn settle_approval(&mut self, tool_call_id: &str, window: &mut Window, cx: &mut Context<Self>) {
        self.approvals.retain(|request| request.tool_call_id != tool_call_id);
        let showing = self
            .approval
            .as_ref()
            .is_some_and(|open| open.panel.read(cx).tool_call_id() == tool_call_id);
        if showing {
            self.close_approval(window, cx);
            self.show_next_approval(window, cx);
        }
    }

    fn stop_approvals(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        self.approval_poll = None;
        self.approvals.clear();
        self.handled.clear();
        self.close_approval(window, cx);
    }

    /// One check two seconds into a run, in case the stream dropped an
    /// approval frame; it starts the regular poll only if it finds one.
    fn schedule_approval_check(&mut self, cx: &mut Context<Self>) {
        if self.approval_poll.is_some() {
            return;
        }
        let connection = self.connection.clone();
        self.approval_poll = Some(cx.spawn(async move |this, cx| {
            cx.background_executor().timer(APPROVAL_POLL).await;
            let pending = runtime::spawn(async move { connection.pending_approvals().await }).await;
            let _ = this.update(cx, |this, cx| {
                this.approval_poll = None;
                if let Ok(pending) = pending
                    && !pending.is_empty()
                {
                    this.start_approval_poll(cx);
                    this.queue_from_poll(pending, cx);
                }
            });
        }));
    }

    fn start_approval_poll(&mut self, cx: &mut Context<Self>) {
        let connection = self.connection.clone();
        self.approval_poll = Some(cx.spawn(async move |this, cx| {
            let mut warned = false;
            loop {
                cx.background_executor().timer(APPROVAL_POLL).await;
                let connection = connection.clone();
                let pending = runtime::spawn(async move { connection.pending_approvals().await }).await;
                let alive = this.update(cx, |this, cx| match pending {
                    Ok(pending) => this.queue_from_poll(pending, cx),
                    Err(_) if !warned => {
                        warned = true;
                        this.approval_poll_failed = true;
                        cx.notify();
                    }
                    Err(_) => {}
                });
                if alive.is_err() {
                    break;
                }
            }
        }));
    }

    /// Approvals found by polling join the queue; the dialog opens on the
    /// next frame, which has the window this callback lacks.
    fn queue_from_poll(&mut self, pending: Vec<ApprovalRequest>, cx: &mut Context<Self>) {
        let mut added = false;
        for request in pending {
            if self.handled.insert(request.tool_call_id.clone()) {
                self.approvals.push_back(request);
                added = true;
            }
        }
        if added {
            cx.notify();
        }
    }

    // ── around the transcript ───────────────────────────────────────────

    fn load_usage(&mut self, cx: &mut Context<Self>) {
        let Some(id) = self.session_id.clone() else {
            self.usage = None;
            return;
        };
        let connection = self.connection.clone();
        let generation = self.generation;
        cx.spawn(async move |this, cx| {
            let usage = runtime::spawn(async move { connection.session_usage(&id).await }).await;
            let _ = this.update(cx, |this, cx| {
                if this.generation == generation {
                    // A failed read hides the strip rather than alarming.
                    this.usage = usage.ok();
                    cx.notify();
                }
            });
        })
        .detach();
    }

    fn load_soul(&mut self, cx: &mut Context<Self>) {
        self.soul = None;
        let Some(agent) = self.agent_id.clone() else {
            return;
        };
        let connection = self.connection.clone();
        cx.spawn(async move |this, cx| {
            let soul = {
                let agent = agent.clone();
                runtime::spawn(async move { connection.agent_soul(&agent).await }).await
            };
            let _ = this.update(cx, |this, cx| {
                if this.agent_id.as_deref() == Some(agent.as_str()) {
                    this.soul = soul.ok().flatten();
                    cx.notify();
                }
            });
        })
        .detach();
    }

    /// Fetch what the palettes offer. Failures leave them empty and show
    /// the banner, as the web console does.
    fn load_discovery(&mut self, cx: &mut Context<Self>) {
        let connection = self.connection.clone();
        let composer = self.composer.clone();
        cx.spawn(async move |this, cx| {
            let fetched = runtime::spawn(async move {
                let (bundles, personal, delegatable, files) = futures::join!(
                    connection.user_skill_bundles(),
                    connection.personal_skills(),
                    connection.delegatable(),
                    connection.list_files(Default::default())
                );
                Ok((bundles, personal, delegatable, files))
            })
            .await;
            let Ok((bundles, personal, delegatable, files)) = fetched else {
                return;
            };
            let failed = bundles.is_err() || delegatable.is_err() || files.as_ref().is_err_and(|e| !e.is_forbidden());
            let mut all = bundles.unwrap_or_default();
            // Personal skills join the palette under their qualified name
            // (the gateway's "personal:" prefix).
            if let Ok(page) = personal {
                all.extend(
                    page.skills
                        .into_iter()
                        .map(|skill| aikonos_client::api::chat::SkillBundle {
                            id: format!("personal:{}", skill.name),
                            skill_name: Some(format!("personal:{}", skill.name)),
                            personal: true,
                            name: skill.name,
                            description: skill.description,
                            body: String::new(),
                        }),
                );
            }
            let delegatable = delegatable.unwrap_or_default();
            let discovery = Discovery {
                bundles: all,
                users: delegatable.users,
                groups: delegatable.groups,
                files: files.unwrap_or_default(),
                failed,
            };
            let _ = this.update(cx, |this, cx| {
                this.discovery_failed = discovery.failed;
                composer.update(cx, |composer, cx| composer.set_discovery(discovery, cx));
                cx.notify();
            });
        })
        .detach();
    }

    // ── per-message actions ─────────────────────────────────────────────

    fn copy(&self, text: String, window: &mut Window, cx: &mut Context<Self>) {
        cx.write_to_clipboard(ClipboardItem::new_string(text));
        app::toast_ok("Copied to clipboard.", window, cx);
    }

    /// Seed the composer with the first line of a reply, quoted.
    fn reply(&mut self, text: &str, window: &mut Window, cx: &mut Context<Self>) {
        let snippet: String = text
            .lines()
            .find(|line| !line.trim().is_empty())
            .unwrap_or_default()
            .chars()
            .take(200)
            .collect();
        let draft = if snippet.is_empty() {
            String::new()
        } else {
            format!("> {snippet}\n\n")
        };
        self.set_draft(draft, window, cx);
    }

    fn start_edit(&mut self, key: u64, text: String, window: &mut Window, cx: &mut Context<Self>) {
        if self.running() {
            return;
        }
        let input = cx.new(|cx| {
            TextareaState::new(window, cx)
                .auto_grow(2, 10)
                .submit_on_enter(true)
                .default_value(text)
        });
        let subscription = cx.subscribe_in(&input, window, move |this, _, event, window, cx| {
            if let gpui_kit::component::input::InputEvent::PressEnter { shift: false, .. } = event {
                this.save_edit(window, cx);
            }
        });
        self._subscriptions.push(subscription);
        input.update(cx, |input, cx| input.focus(window, cx));
        self.editing = Some((key, input));
        cx.notify();
    }

    fn cancel_edit(&mut self, cx: &mut Context<Self>) {
        self.editing = None;
        cx.notify();
    }

    /// Replace a sent message and continue from there: everything from it
    /// on is dropped, then the edited text is sent.
    fn save_edit(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let Some((key, input)) = self.editing.take() else {
            return;
        };
        let text = input.read(cx).value().trim().to_owned();
        if text.is_empty() || self.running() {
            cx.notify();
            return;
        }
        if let Some(ix) = self.row_ix(key) {
            self.truncate_rows(ix, cx);
            self.run_chat(text, None, window, cx);
        }
    }

    fn confirm_delegation(
        &mut self,
        text: String,
        target: DelegationTarget,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let prompt: SharedString = match &target {
            DelegationTarget::Group {
                display_name,
                member_count,
                ..
            } => format!("Delegate to group {display_name} ({member_count} people)?").into(),
            DelegationTarget::User { display_name, .. } => format!("Delegate to {display_name}:").into(),
        };
        let view = cx.weak_entity();
        let body: SharedString = text.clone().into();
        window.open_dialog(cx, move |dialog, _, cx| {
            let confirm = {
                let view = view.clone();
                let text = text.clone();
                let target = target.clone();
                move |_: &ClickEvent, window: &mut Window, cx: &mut App| {
                    window.close_dialog(cx);
                    let _ = view.update(cx, |this, cx| this.delegate(text.clone(), target.clone(), window, cx));
                }
            };
            let cancel = {
                let view = view.clone();
                let text = text.clone();
                move |_: &ClickEvent, window: &mut Window, cx: &mut App| {
                    window.close_dialog(cx);
                    let _ = view.update(cx, |this, cx| this.set_draft(text.clone(), window, cx));
                }
            };
            dialog
                .title("Delegate task")
                .w(px(480.))
                .bg(cx.theme().popover)
                .child(
                    v_flex()
                        .gap_2()
                        .text_sm()
                        .child(prompt.clone())
                        .child(div().text_color(cx.theme().muted_foreground).child(body.clone())),
                )
                .footer(
                    DialogFooter::new()
                        .gap_2()
                        .child(Button::new("delegate-cancel").ghost().label("Cancel").on_click(cancel))
                        .child(
                            Button::new("delegate-confirm")
                                .primary()
                                .label("Confirm")
                                .on_click(confirm),
                        ),
                )
        });
    }

    fn delegate(&mut self, text: String, target: DelegationTarget, window: &mut Window, cx: &mut Context<Self>) {
        let connection = self.connection.clone();
        let intent = strip_mention(&text, target.display_name());
        cx.spawn_in(window, async move |this, cx| {
            let result = {
                let target = target.clone();
                runtime::spawn(async move { connection.delegate(&target, &intent).await }).await
            };
            let _ = this.update_in(cx, |this, window, cx| match result {
                Ok(()) => {
                    let note = match &target {
                        DelegationTarget::Group {
                            display_name,
                            member_count,
                            ..
                        } => {
                            format!("✓ Task delegated to group {display_name} ({member_count} people)")
                        }
                        DelegationTarget::User { display_name, .. } => {
                            format!("✓ Task delegated to {display_name}")
                        }
                    };
                    let toast = match &target {
                        DelegationTarget::Group { .. } => note.clone(),
                        DelegationTarget::User { display_name, .. } => format!("Delegated to {display_name}"),
                    };
                    app::toast_ok(toast, window, cx);
                    this.push_row(TranscriptEntry::user(text.clone()), cx);
                    this.push_row(TranscriptEntry::assistant_text(note), cx);
                    this.persist(cx);
                    cx.notify();
                }
                Err(err) => {
                    app::report(&err, window, cx);
                    this.set_draft(text.clone(), window, cx);
                }
            });
        })
        .detach();
    }

    fn open_soul_editor(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let (Some(agent), Some(soul)) = (self.agent_id.clone(), self.soul.clone()) else {
            return;
        };
        let input = cx.new(|cx| {
            TextareaState::new(window, cx)
                .auto_grow(8, 16)
                .placeholder("Describe how this agent should behave…")
                .default_value(soul)
        });
        let view = cx.weak_entity();
        let error: Entity<Option<SharedString>> = cx.new(|_| None);
        window.open_dialog(cx, move |dialog, _, cx| {
            let bytes = input.read(cx).value().len();
            let over = bytes > SOUL_MAX_BYTES;
            let save = {
                let view = view.clone();
                let input = input.clone();
                let error = error.clone();
                let agent = agent.clone();
                move |_: &ClickEvent, window: &mut Window, cx: &mut App| {
                    let soul = input.read(cx).value().to_string();
                    let connection = app::connection(cx);
                    let view = view.clone();
                    let error = error.clone();
                    let agent = agent.clone();
                    window
                        .spawn(cx, async move |cx| {
                            let saved =
                                runtime::spawn(async move { connection.set_agent_soul(&agent, &soul).await }).await;
                            let _ = cx.update(|window, cx| match saved {
                                Ok(soul) => {
                                    window.close_dialog(cx);
                                    app::toast_ok("Personality saved.", window, cx);
                                    let _ = view.update(cx, |this, cx| {
                                        this.soul = Some(soul);
                                        cx.notify();
                                    });
                                }
                                Err(err) => error.update(cx, |error, cx| {
                                    *error = Some(err.to_string().into());
                                    cx.notify();
                                }),
                            });
                        })
                        .detach();
                }
            };
            dialog
                .title("Agent Personality")
                .w(px(560.))
                .bg(cx.theme().popover)
                .child(
                    v_flex()
                        .gap_1()
                        .child(
                            div()
                                .text_size(rems(0.8))
                                .font_weight(FontWeight::MEDIUM)
                                .text_color(cx.theme().muted_foreground)
                                .child(format!("Personality (markdown, ≤ {SOUL_MAX_BYTES} bytes)")),
                        )
                        .child(Textarea::new(&input))
                        .child(
                            div()
                                .text_xs()
                                .text_color(if over {
                                    cx.theme().danger
                                } else {
                                    cx.theme().muted_foreground
                                })
                                .when(over, |this| this.font_weight(FontWeight::SEMIBOLD))
                                .child(format!("{bytes} / {SOUL_MAX_BYTES} bytes")),
                        )
                        .when_some(error.read(cx).clone(), |this, error| {
                            this.child(div().text_xs().text_color(cx.theme().danger).child(error))
                        }),
                )
                .footer(
                    DialogFooter::new()
                        .gap_2()
                        .child(
                            Button::new("soul-cancel")
                                .ghost()
                                .label("Cancel")
                                .on_click(|_, window, cx| window.close_dialog(cx)),
                        )
                        .child(
                            Button::new("soul-save")
                                .primary()
                                .label("Save")
                                .disabled(over)
                                .on_click(save),
                        ),
                )
        });
    }
}

/// Remove the first `@<name>` (and one following space) from `text`,
/// collapsing doubled spaces (webui lib/mention.js `stripMention`).
fn strip_mention(text: &str, display_name: &str) -> String {
    if display_name.is_empty() {
        return text.trim().to_owned();
    }
    let token = format!("@{display_name}");
    let stripped = match text.find(&token) {
        Some(at) => {
            let rest = &text[at + token.len()..];
            let rest = rest.strip_prefix(' ').unwrap_or(rest);
            format!("{}{}", &text[..at], rest)
        }
        None => text.to_owned(),
    };
    let mut out = String::with_capacity(stripped.len());
    let mut previous_space = false;
    for ch in stripped.chars() {
        if ch == ' ' && previous_space {
            continue;
        }
        previous_space = ch == ' ';
        out.push(ch);
    }
    out.trim().to_owned()
}

impl Render for ChatView {
    fn render(&mut self, window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        if std::mem::take(&mut self.approval_poll_failed) {
            app::toast_error("Couldn't check pending approvals — retrying…", window, cx);
        }
        // Approvals found by polling open here, where the window is at hand.
        if self.approval.is_none() && !self.approvals.is_empty() {
            self.show_next_approval(window, cx);
        }
        if let Some(open) = &self.approval
            && open.panel.read(cx).is_decided()
            && self.run.is_none()
        {
            self.close_approval(window, cx);
        }

        let theme = cx.theme();
        let view = cx.entity();
        let transcript = MessageScroller::new("transcript", self.scroller.clone(), move |ix, window, cx| {
            view.update(cx, |this, cx| this.render_row(ix, window, cx))
        })
        .size_full()
        // Rows own their spacing (the web list's 0.75rem gap), not the
        // scroller's default 2rem.
        .with_row_style(StyleRefinement::default().px_0().pb_0())
        .with_bottom_fade(theme.background);

        let header = (self.agent_id.is_some() && self.soul.is_some()).then(|| {
            h_flex().flex_none().justify_end().px_6().pt_2().child(
                Button::new("personality")
                    .outline()
                    .xsmall()
                    .label("Personality")
                    .on_click(cx.listener(|this, _, window, cx| this.open_soul_editor(window, cx))),
            )
        });

        v_flex()
            .size_full()
            .children(header)
            .when(self.discovery_failed, |this| {
                this.child(
                    div().flex_none().px_6().pt_2().child(ui::error_banner(
                        "Mention and tool palettes unavailable",
                        Some(
                            Button::new("discovery-retry")
                                .ghost()
                                .xsmall()
                                .label("Retry")
                                .on_click(cx.listener(|this, _, _, cx| this.load_discovery(cx)))
                                .into_any_element(),
                        ),
                        cx,
                    )),
                )
            })
            .child(
                div()
                    .flex_1()
                    .min_h_0()
                    .when(self.loading, |this| {
                        this.child(ui::empty_state(None, "Loading conversation…", cx))
                    })
                    .when(!self.loading, |this| this.child(transcript)),
            )
            .child(
                div().flex_none().px_6().pt_3().pb_5().child(
                    v_flex()
                        .w_full()
                        .max_w(rems(50.))
                        .mx_auto()
                        .children(self.render_usage(cx))
                        .child(self.composer.clone()),
                ),
            )
    }
}

#[cfg(test)]
mod tests {
    use super::strip_mention;

    #[test]
    fn mentions_strip_like_the_web_client() {
        assert_eq!(strip_mention("@Bob Smith please check", "Bob Smith"), "please check");
        assert_eq!(strip_mention("please @Bob check", "Bob"), "please check");
        assert_eq!(strip_mention("no mention here", "Bob"), "no mention here");
        assert_eq!(strip_mention("  spaced  ", ""), "spaced");
    }
}
