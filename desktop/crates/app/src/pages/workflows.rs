//! Saved workflows (webui views/Workflows.vue with RunWorkflowModal,
//! PublishWorkflowDialog, VersionSwitcherModal and ForkWorkflowModal).
//!
//! A run streams its steps; approval-gated steps are declined, as on the
//! web. A successful run is saved as a session and opened in chat.

use std::collections::BTreeMap;
use std::sync::Arc;

use aikonos_client::api::chat::DelegatableGroup;
use aikonos_client::api::workflows::{
    RunReply, StepProgress, WorkflowDefinition, WorkflowDetail, WorkflowInput, WorkflowSummary, WorkflowVersion,
};
use aikonos_client::transcript::{self as record, AssistantTurn, SessionRecord, ToolCall, TranscriptEntry};
use aikonos_client::{Connection, StreamItem};
use gpui_kit::component::WindowExt as _;
use gpui_kit::component::button::{Button, ButtonVariant, ButtonVariants as _};
use gpui_kit::component::checkbox::Checkbox;
use gpui_kit::component::dialog::DialogFooter;
use gpui_kit::component::input::{Input, InputEvent, InputState};
use gpui_kit::component::{ActiveTheme as _, Disableable as _, Icon, Sizable as _, h_flex, v_flex};
use gpui_kit::prelude::FluentBuilder as _;
use gpui_kit::*;
use serde_json::Value;

use crate::app;
use crate::assets::AppIcon;
use crate::runtime;
use crate::shell;
use crate::theme::console;
use crate::ui::{self, PillTone};

pub struct WorkflowsPage {
    connection: Arc<Connection>,
    workflows: Vec<WorkflowSummary>,
    shared_unavailable: bool,
    loading: bool,
    error: Option<SharedString>,
    filter: Entity<InputState>,
    agent_names: BTreeMap<String, String>,
    _subscriptions: Vec<Subscription>,
}

impl WorkflowsPage {
    pub fn new(window: &mut Window, cx: &mut Context<Self>) -> Self {
        let filter = cx.new(|cx| InputState::new(window, cx).placeholder("Filter workflows by name…"));
        let filter_changed = cx.subscribe(&filter, |_, _, event: &InputEvent, cx| {
            if matches!(event, InputEvent::Change) {
                cx.notify();
            }
        });
        let mut this = Self {
            connection: app::connection(cx),
            workflows: Vec::new(),
            shared_unavailable: false,
            loading: true,
            error: None,
            filter,
            agent_names: BTreeMap::new(),
            _subscriptions: vec![filter_changed],
        };
        this.load(cx);
        this
    }

    fn load(&mut self, cx: &mut Context<Self>) {
        self.loading = true;
        let connection = self.connection.clone();
        cx.spawn(async move |this, cx| {
            let (page, agents) = runtime::spawn(async move {
                let (page, agents) = futures::join!(connection.workflows(), connection.list_agents());
                Ok((page, agents))
            })
            .await
            .unwrap_or_else(|err| (Err(err.clone()), Err(err)));
            let _ = this.update(cx, |this, cx| {
                this.loading = false;
                match page {
                    Ok(page) => {
                        this.workflows = page.workflows;
                        this.shared_unavailable = page.shared_unavailable;
                        this.error = None;
                    }
                    Err(err) => this.error = Some(err.to_string().into()),
                }
                if let Ok(agents) = agents {
                    this.agent_names = agents.into_iter().map(|a| (a.id, a.name)).collect();
                }
                cx.notify();
            });
        })
        .detach();
    }

    fn agent_label(&self, id: &str) -> String {
        self.agent_names
            .get(id)
            .cloned()
            .unwrap_or_else(|| id.chars().take(8).collect())
    }

    fn with_definition(
        &mut self,
        workflow: &WorkflowSummary,
        window: &mut Window,
        cx: &mut Context<Self>,
        then: impl FnOnce(&mut Self, WorkflowDetail, &mut Window, &mut Context<Self>) + 'static,
    ) {
        let connection = self.connection.clone();
        let lineage = workflow.lineage_id.clone();
        cx.spawn_in(window, async move |this, cx| {
            let detail = runtime::spawn(async move { connection.workflow(&lineage).await }).await;
            let _ = this.update_in(cx, |this, window, cx| match detail {
                Ok(detail) => then(this, detail, window, cx),
                Err(err) => {
                    this.error = Some(err.to_string().into());
                    cx.notify();
                }
            });
        })
        .detach();
    }

    fn open_run(&mut self, workflow: &WorkflowSummary, window: &mut Window, cx: &mut Context<Self>) {
        let summary = workflow.clone();
        self.with_definition(workflow, window, cx, move |this, detail, window, cx| {
            let connection = this.connection.clone();
            let run = cx.new(|cx| RunDialog::new(connection, summary, detail.definition(), window, cx));
            window.open_dialog(cx, move |dialog, _, cx| {
                let footer = run.clone();
                dialog
                    .title(
                        h_flex()
                            .gap_2()
                            .child(Icon::new(AppIcon::PlayCircle).small())
                            .child(run.read(cx).summary.name.clone()),
                    )
                    .w(px(620.))
                    .bg(cx.theme().popover)
                    .child(run.clone())
                    .footer(
                        DialogFooter::new()
                            .child(footer.update(cx, |run, cx| run.render_footer(cx).into_any_element())),
                    )
            });
        });
    }

    /// Hand the definition to the agent with an instruction to propose a
    /// new version (Workflows.vue `openImproveInChat`).
    fn improve(&mut self, workflow: &WorkflowSummary, window: &mut Window, cx: &mut Context<Self>) {
        let name = workflow.name.clone();
        let lineage = workflow.lineage_id.clone();
        self.with_definition(workflow, window, cx, move |_, detail, window, cx| {
            let summary = serde_json::from_str::<Value>(&detail.definition_json)
                .ok()
                .and_then(|value| serde_json::to_string_pretty(&value).ok())
                .unwrap_or_else(|| {
                    if detail.definition_json.is_empty() {
                        "(no definition)".to_owned()
                    } else {
                        detail.definition_json.clone()
                    }
                });
            let message = format!(
                "I want to improve the workflow \"{name}\" (lineageId: {lineage}).\n\nCurrent definition:\n```json\n{summary}\n```\n\nOnce we agree on what to change, use `workflow_propose` to create a proposed version for my approval."
            );
            shell::prefill_chat(message, window, cx);
        });
    }

    fn open_publish(&mut self, workflow: &WorkflowSummary, window: &mut Window, cx: &mut Context<Self>) {
        let page = cx.weak_entity();
        let publish = cx.new(|cx| PublishDialog::new(self.connection.clone(), workflow.clone(), page, cx));
        window.open_dialog(cx, move |dialog, _, cx| {
            let footer = publish.clone();
            dialog
                .title("Publish to groups")
                .w(px(480.))
                .bg(cx.theme().popover)
                .child(publish.clone())
                .footer(
                    DialogFooter::new()
                        .child(footer.update(cx, |publish, cx| publish.render_footer(cx).into_any_element())),
                )
        });
    }

    fn open_versions(&mut self, workflow: &WorkflowSummary, window: &mut Window, cx: &mut Context<Self>) {
        let page = cx.weak_entity();
        let versions = cx.new(|cx| VersionsDialog::new(self.connection.clone(), workflow.clone(), page, cx));
        window.open_dialog(cx, move |dialog, _, cx| {
            dialog
                .title(format!("Versions — {}", versions.read(cx).workflow.name))
                .w(px(520.))
                .bg(cx.theme().popover)
                .child(versions.clone())
        });
    }

    fn open_fork(&mut self, workflow: &WorkflowSummary, window: &mut Window, cx: &mut Context<Self>) {
        let name = cx.new(|cx| {
            InputState::new(window, cx).default_value(if workflow.name.is_empty() {
                String::new()
            } else {
                format!("Fork of {}", workflow.name)
            })
        });
        let page = cx.weak_entity();
        let lineage = workflow.lineage_id.clone();
        let connection = self.connection.clone();
        window.open_dialog(cx, move |dialog, _, cx| {
            let name_for_ok = name.clone();
            let page = page.clone();
            let lineage = lineage.clone();
            let connection = connection.clone();
            dialog
                .title("Fork workflow")
                .w(px(440.))
                .bg(cx.theme().popover)
                .child(
                    v_flex()
                        .gap_1()
                        .child(
                            div()
                                .text_size(rems(0.8))
                                .text_color(cx.theme().muted_foreground)
                                .child("New name"),
                        )
                        .child(Input::new(&name)),
                )
                .footer(
                    DialogFooter::new()
                        .gap_2()
                        .child(
                            Button::new("fork-cancel")
                                .ghost()
                                .label("Cancel")
                                .on_click(|_, window, cx| window.close_dialog(cx)),
                        )
                        .child(
                            Button::new("fork-confirm")
                                .primary()
                                .label("Fork")
                                .disabled(name.read(cx).value().trim().is_empty())
                                .on_click(move |_, window, cx| {
                                    let new_name = name_for_ok.read(cx).value().trim().to_owned();
                                    let connection = connection.clone();
                                    let lineage = lineage.clone();
                                    let page = page.clone();
                                    window
                                        .spawn(cx, async move |cx| {
                                            let forked = runtime::spawn(async move {
                                                connection.fork_workflow(&lineage, &new_name).await
                                            })
                                            .await;
                                            let _ = cx.update(|window, cx| match forked {
                                                Ok(_) => {
                                                    window.close_dialog(cx);
                                                    let _ = page.update(cx, |page, cx| page.load(cx));
                                                }
                                                Err(err) => app::report(&err, window, cx),
                                            });
                                        })
                                        .detach();
                                }),
                        ),
                )
        });
    }

    fn confirm_delete(&mut self, workflow: &WorkflowSummary, window: &mut Window, cx: &mut Context<Self>) {
        let page = cx.weak_entity();
        let lineage = workflow.lineage_id.clone();
        let name = workflow.name.clone();
        let shared = workflow.is_shared();
        window.open_alert_dialog(cx, move |alert, _, _| {
            let page = page.clone();
            let lineage = lineage.clone();
            let mut description = "This removes every version and cannot be undone.".to_owned();
            if shared {
                description.push_str(" This workflow is shared — deleting it removes it for everyone in its groups.");
            }
            alert
                .title(format!("Delete “{name}”?"))
                .description(description)
                .confirm()
                .ok_text("Delete")
                .ok_variant(ButtonVariant::Danger)
                .on_ok(move |_, _, cx| {
                    let _ = page.update(cx, |this, cx| this.delete(lineage.clone(), cx));
                    true
                })
        });
    }

    fn delete(&mut self, lineage: String, cx: &mut Context<Self>) {
        let connection = self.connection.clone();
        cx.spawn(async move |this, cx| {
            let deleted = runtime::spawn(async move { connection.delete_workflow(&lineage).await }).await;
            let _ = this.update(cx, |this, cx| {
                if let Err(err) = deleted {
                    this.error = Some(err.to_string().into());
                }
                this.load(cx);
            });
        })
        .detach();
    }

    fn render_card(&self, workflow: &WorkflowSummary, private: bool, cx: &mut Context<Self>) -> AnyElement {
        let theme = cx.theme().clone();
        let greyed = !workflow.is_runnable();
        let agent_denied = !workflow.bound_agent_id.is_empty() && !workflow.bound_agent_ok;
        let id = workflow.lineage_id.clone();
        let (run_wf, improve_wf, publish_wf, versions_wf, fork_wf, delete_wf) = (
            workflow.clone(),
            workflow.clone(),
            workflow.clone(),
            workflow.clone(),
            workflow.clone(),
            workflow.clone(),
        );
        h_flex()
            .gap_3()
            .p_4()
            .bg(theme.popover)
            .border_1()
            .border_color(theme.border)
            .rounded(theme.radius)
            .when(greyed, |this| this.opacity(0.6))
            .child(
                v_flex()
                    .flex_1()
                    .min_w_0()
                    .gap_1()
                    .child(
                        div()
                            .text_sm()
                            .font_weight(FontWeight::MEDIUM)
                            .child(workflow.name.clone()),
                    )
                    .child(
                        h_flex()
                            .gap_1p5()
                            .flex_wrap()
                            .child(ui::pill(format!("v{}", workflow.version), PillTone::Neutral, cx))
                            .child(ui::pill(
                                workflow.visibility_kind.clone(),
                                if private && workflow.is_shared() {
                                    PillTone::Ok
                                } else {
                                    PillTone::Neutral
                                },
                                cx,
                            ))
                            .when(!workflow.bound_agent_id.is_empty(), |this| {
                                this.child(ui::pill(
                                    format!("agent: {}", self.agent_label(&workflow.bound_agent_id)),
                                    PillTone::Neutral,
                                    cx,
                                ))
                            })
                            .when(greyed, |this| {
                                this.child(
                                    div()
                                        .text_xs()
                                        .text_color(theme.danger)
                                        .child(format!("needs: {}", workflow.missing_requirements.join(", "))),
                                )
                            })
                            .when(agent_denied, |this| {
                                this.child(
                                    div()
                                        .text_xs()
                                        .text_color(theme.danger)
                                        .child("no access to this workflow's agent"),
                                )
                            }),
                    ),
            )
            .child(
                h_flex()
                    .gap_1()
                    .flex_wrap()
                    .justify_end()
                    .child(
                        Button::new(SharedString::from(format!("run-{id}")))
                            .primary()
                            .small()
                            .label("Run")
                            .disabled(greyed || agent_denied)
                            .on_click(cx.listener(move |this, _, window, cx| this.open_run(&run_wf, window, cx))),
                    )
                    .when(private && workflow.is_owner, |this| {
                        this.child(
                            Button::new(SharedString::from(format!("improve-{id}")))
                                .outline()
                                .small()
                                .label("Improve")
                                .on_click(
                                    cx.listener(move |this, _, window, cx| this.improve(&improve_wf, window, cx)),
                                ),
                        )
                    })
                    .when(private, |this| {
                        this.child(
                            Button::new(SharedString::from(format!("publish-{id}")))
                                .outline()
                                .small()
                                .label("Publish")
                                .on_click(
                                    cx.listener(move |this, _, window, cx| this.open_publish(&publish_wf, window, cx)),
                                ),
                        )
                    })
                    .child(
                        Button::new(SharedString::from(format!("versions-{id}")))
                            .outline()
                            .small()
                            .label("Versions")
                            .on_click(
                                cx.listener(move |this, _, window, cx| this.open_versions(&versions_wf, window, cx)),
                            ),
                    )
                    .child(
                        Button::new(SharedString::from(format!("fork-{id}")))
                            .outline()
                            .small()
                            .label("Fork")
                            .on_click(cx.listener(move |this, _, window, cx| this.open_fork(&fork_wf, window, cx))),
                    )
                    .when(private, |this| {
                        this.child(
                            Button::new(SharedString::from(format!("delete-{id}")))
                                .outline()
                                .small()
                                .label("Delete")
                                .text_color(theme.danger)
                                .on_click(
                                    cx.listener(move |this, _, window, cx| this.confirm_delete(&delete_wf, window, cx)),
                                ),
                        )
                    }),
            )
            .into_any_element()
    }
}

impl Render for WorkflowsPage {
    fn render(&mut self, _: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let theme = cx.theme().clone();
        let query = self.filter.read(cx).value().trim().to_lowercase();
        let matches = |w: &&WorkflowSummary| query.is_empty() || w.name.to_lowercase().contains(&query);
        let shared: Vec<WorkflowSummary> = self
            .workflows
            .iter()
            .filter(|w| w.is_shared() && !w.is_owner)
            .filter(matches)
            .cloned()
            .collect();
        let private: Vec<WorkflowSummary> = self
            .workflows
            .iter()
            .filter(|w| w.is_owner)
            .filter(matches)
            .cloned()
            .collect();
        let section = |title: &'static str| {
            div()
                .text_sm()
                .font_weight(FontWeight::SEMIBOLD)
                .text_color(theme.muted_foreground)
                .child(title)
        };
        ui::page("workflows-page", ui::PAGE_WIDTH)
            .child(ui::view_header(AppIcon::PlayCircle, "Workflows", cx))
            .when_some(self.error.clone(), |this, error| {
                this.child(ui::error_banner(error, None, cx))
            })
            .when(!self.workflows.is_empty(), |this| {
                this.child(Input::new(&self.filter).small())
            })
            .when(self.loading && self.workflows.is_empty(), |this| {
                this.child(ui::empty_state(None, "Loading…", cx))
            })
            .when(self.shared_unavailable, |this| {
                this.child(ui::error_banner(
                    "Shared workflows are temporarily unavailable (authorization service unreachable).",
                    Some(
                        Button::new("shared-retry")
                            .ghost()
                            .xsmall()
                            .label("Retry")
                            .on_click(cx.listener(|this, _, _, cx| this.load(cx)))
                            .into_any_element(),
                    ),
                    cx,
                ))
            })
            .when(!shared.is_empty(), |this| {
                this.child(
                    v_flex()
                        .gap_2()
                        .child(section("Shared"))
                        .children(shared.iter().map(|w| self.render_card(w, false, cx))),
                )
            })
            .when(!private.is_empty(), |this| {
                this.child(
                    v_flex()
                        .gap_2()
                        .child(section("Private"))
                        .children(private.iter().map(|w| self.render_card(w, true, cx))),
                )
            })
            .when(
                self.workflows.is_empty() && self.error.is_none() && !self.loading,
                |this| {
                    this.child(
                        div()
                            .text_sm()
                            .text_color(console(cx).text_faint)
                            .child("No workflows yet. Ask the agent to run a task and save it as a workflow."),
                    )
                },
            )
            .when(
                !self.workflows.is_empty() && shared.is_empty() && private.is_empty() && self.error.is_none(),
                |this| {
                    this.child(
                        div()
                            .text_sm()
                            .text_color(console(cx).text_faint)
                            .child(format!("No workflows match \"{query}\".")),
                    )
                },
            )
    }
}

#[derive(Clone, PartialEq, Eq)]
enum StepStatus {
    Pending,
    Done,
    Failed(String),
}

struct LiveStep {
    label: String,
    status: StepStatus,
}

/// Running a workflow: its inputs, live progress, the result and a rating.
struct RunDialog {
    connection: Arc<Connection>,
    summary: WorkflowSummary,
    definition: WorkflowDefinition,
    inputs: Vec<(WorkflowInput, Entity<InputState>)>,
    flags: BTreeMap<String, bool>,
    running: bool,
    steps: Vec<LiveStep>,
    reply: Option<RunReply>,
    error: Option<SharedString>,
    rating: Option<bool>,
    rated: bool,
    note: Entity<InputState>,
    _task: Option<Task<()>>,
}

fn input_kind(input: &WorkflowInput) -> &'static str {
    let schema = input.schema.as_ref();
    if schema.and_then(|s| s.get("enum")).is_some_and(Value::is_array) {
        return "enum";
    }
    match schema.and_then(|s| s.get("type")).and_then(Value::as_str) {
        Some("boolean") => "boolean",
        Some("number" | "integer") => "number",
        _ => "text",
    }
}

fn step_label(kind: Option<&str>, skill: Option<&str>) -> String {
    if kind == Some("reason") {
        "reason".into()
    } else {
        skill.unwrap_or("step").to_owned()
    }
}

/// A step's output as display text (RunWorkflowModal.vue `renderStepOutput`).
fn render_output(output: &Value, kind: &str) -> String {
    match output {
        Value::Null => String::new(),
        Value::String(text) => text.clone(),
        Value::Object(map) if kind == "tool" && map.get("content").is_some_and(Value::is_string) => {
            map["content"].as_str().unwrap_or_default().to_owned()
        }
        other => serde_json::to_string_pretty(other).unwrap_or_default(),
    }
}

impl RunDialog {
    fn new(
        connection: Arc<Connection>,
        summary: WorkflowSummary,
        definition: WorkflowDefinition,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) -> Self {
        let mut flags = BTreeMap::new();
        let inputs = definition
            .inputs
            .iter()
            .filter(|input| {
                if input_kind(input) == "boolean" {
                    flags.insert(
                        input.name.clone(),
                        input.default.as_ref().and_then(Value::as_bool).unwrap_or(false),
                    );
                    false
                } else {
                    true
                }
            })
            .map(|input| {
                let default = match &input.default {
                    Some(Value::String(text)) => text.clone(),
                    Some(Value::Null) | None => String::new(),
                    Some(other) => other.to_string(),
                };
                let state = cx.new(|cx| {
                    InputState::new(window, cx)
                        .placeholder(input.name.clone())
                        .default_value(default)
                });
                (input.clone(), state)
            })
            .collect();
        let note = cx.new(|cx| InputState::new(window, cx).placeholder("Note (optional)"));
        Self {
            connection,
            summary,
            definition,
            inputs,
            flags,
            running: false,
            steps: Vec::new(),
            reply: None,
            error: None,
            rating: None,
            rated: false,
            note,
            _task: None,
        }
    }

    fn values(&self, cx: &App) -> BTreeMap<String, String> {
        let mut values: BTreeMap<String, String> = self
            .inputs
            .iter()
            .map(|(input, state)| (input.name.clone(), state.read(cx).value().to_string()))
            .collect();
        for (name, on) in &self.flags {
            values.insert(name.clone(), on.to_string());
        }
        values
    }

    /// Inputs without a default must be filled in.
    fn can_run(&self, cx: &App) -> bool {
        self.inputs.iter().all(|(input, state)| {
            let required = matches!(input.default, None | Some(Value::Null));
            !required || !state.read(cx).value().trim().is_empty()
        })
    }

    fn run(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        if self.running || !self.can_run(cx) {
            return;
        }
        self.running = true;
        self.error = None;
        self.reply = None;
        self.steps = self
            .definition
            .steps
            .iter()
            .map(|step| LiveStep {
                label: step_label(step.kind.as_deref(), step.skill.as_deref()),
                status: StepStatus::Pending,
            })
            .collect();
        cx.notify();
        let inputs = self.values(cx);
        let lineage = self.summary.lineage_id.clone();
        let session_id = uuid::Uuid::new_v4().to_string();
        let connection = self.connection.clone();
        self._task = Some(cx.spawn_in(window, async move |this, cx| {
            let streamed = {
                let (connection, lineage, inputs, session_id) =
                    (connection.clone(), lineage.clone(), inputs.clone(), session_id.clone());
                runtime::spawn(async move {
                    connection
                        .run_workflow_stream(&lineage, &inputs, Some(&session_id))
                        .await
                })
                .await
            };
            let mut reply: Option<RunReply> = None;
            if let Ok(mut stream) = streamed {
                while let Some(item) = stream.next().await {
                    let StreamItem::Event(event) = item else {
                        break;
                    };
                    match event.event.as_str() {
                        "step" => {
                            if let Ok(step) = serde_json::from_str::<StepProgress>(&event.data) {
                                let _ = this.update(cx, |this, cx| {
                                    if let Some(live) = this.steps.get_mut(step.index as usize) {
                                        live.status = if step.ok {
                                            StepStatus::Done
                                        } else {
                                            StepStatus::Failed(step.deny_reason.clone().unwrap_or_default())
                                        };
                                        if let Some(skill) = step.skill {
                                            live.label = skill;
                                        }
                                    }
                                    cx.notify();
                                });
                            }
                        }
                        "result" => reply = serde_json::from_str(&event.data).ok(),
                        _ => {}
                    }
                }
            }
            // The stream was refused or produced no result: fall back to the
            // blocking run, as the web console does.
            let reply = match reply {
                Some(reply) => Ok(reply),
                None => {
                    runtime::spawn(async move { connection.run_workflow(&lineage, &inputs, Some(&session_id)).await })
                        .await
                }
            };
            let _ = this.update_in(cx, |this, window, cx| {
                this.running = false;
                match reply {
                    Ok(reply) => {
                        if reply.ok {
                            this.save_session(&reply, window, cx);
                        } else {
                            this.error = Some(reply.error.clone().unwrap_or_else(|| "The run failed.".into()).into());
                        }
                        this.reply = Some(reply);
                    }
                    Err(err) => this.error = Some(err.to_string().into()),
                }
                cx.notify();
            });
        }));
    }

    /// File the run as a session (source "workflow") and open it in chat
    /// (RunWorkflowModal.vue `buildWorkflowSession`).
    fn save_session(&mut self, reply: &RunReply, window: &mut Window, cx: &mut Context<Self>) {
        let steps = reply.result.as_ref().map(|r| r.steps.clone()).unwrap_or_default();
        let inputs = self.values(cx);
        let summary = inputs
            .iter()
            .map(|(k, v)| format!("{k}={v}"))
            .collect::<Vec<_>>()
            .join(", ");
        let invocation = if summary.is_empty() {
            self.summary.name.clone()
        } else {
            format!("{} ({summary})", self.summary.name)
        };
        let header = format!(
            "{} — {} step{}",
            self.summary.name,
            steps.len(),
            if steps.len() == 1 { "" } else { "s" }
        );
        let failed = steps.iter().find(|s| !s.allowed || s.error.is_some());
        let text = match failed {
            Some(step) => format!(
                "{header}\n\nStep {} ({}) {}: {}",
                step.step_index + 1,
                step_label(Some(&step.kind), Some(&step.skill)),
                if step.allowed { "failed" } else { "was denied" },
                step.deny_reason.clone().or(step.error.clone()).unwrap_or_default()
            ),
            None => match steps
                .last()
                .map(|s| render_output(&s.output, &s.kind))
                .filter(|line| !line.is_empty())
            {
                Some(line) => format!("{header}\n\n{}", aikonos_client::tool_labels::clamp(&line, 300)),
                None => header,
            },
        };
        let tools = steps
            .iter()
            .enumerate()
            .map(|(ix, step)| {
                let mut tool = ToolCall::new(
                    format!("wf-step-{ix}"),
                    step_label(Some(&step.kind), Some(&step.skill)),
                    None,
                );
                tool.args_json = "{}".into();
                let result = if step.allowed {
                    render_output(&step.output, &step.kind)
                } else {
                    step.deny_reason.clone().unwrap_or_else(|| "denied".into())
                };
                tool.result = Some(Value::String(result.chars().take(4096).collect()));
                tool.is_error = !step.allowed || step.error.is_some();
                tool.done = true;
                tool
            })
            .collect();
        let now = record::now_iso();
        let session = SessionRecord {
            id: uuid::Uuid::new_v4().to_string(),
            title: self.summary.name.clone(),
            agent_id: None,
            agent_name: None,
            pinned: false,
            pinned_at: None,
            created_at: Some(now.clone()),
            updated_at: Some(now),
            thread_id: Some(uuid::Uuid::new_v4().to_string()),
            first_message: Some(invocation.clone()),
            messages: vec![
                TranscriptEntry::user(invocation),
                TranscriptEntry::Assistant(AssistantTurn {
                    text,
                    tools,
                    error: None,
                    extra: Default::default(),
                }),
            ],
            source: Some("workflow".into()),
            schedule_id: None,
            extra: Default::default(),
        };
        let connection = self.connection.clone();
        cx.spawn_in(window, async move |_, cx| {
            let id = session.id.clone();
            let saved = runtime::spawn(async move { connection.write_session(&session).await }).await;
            if saved.is_ok() {
                let _ = cx.update(|window, cx| {
                    shell::refresh_sessions(cx);
                    window.close_dialog(cx);
                    shell::with_shell_in(window, cx, move |shell, window, cx| {
                        shell.open_chat(Some(id), None, window, cx)
                    });
                });
            }
        })
        .detach();
    }

    fn rate(&mut self, cx: &mut Context<Self>) {
        let Some(good) = self.rating else {
            return;
        };
        let note = self.note.read(cx).value().to_string();
        let connection = self.connection.clone();
        let lineage = self.summary.lineage_id.clone();
        let version = self.summary.version;
        cx.spawn(async move |this, cx| {
            let rated = runtime::spawn(async move {
                connection
                    .rate_workflow(&lineage, version, good, (!note.is_empty()).then_some(note.as_str()))
                    .await
            })
            .await;
            let _ = this.update(cx, |this, cx| {
                match rated {
                    Ok(()) => this.rated = true,
                    Err(err) => this.error = Some(err.to_string().into()),
                }
                cx.notify();
            });
        })
        .detach();
    }

    fn render_footer(&mut self, cx: &mut Context<Self>) -> impl IntoElement {
        h_flex()
            .w_full()
            .justify_end()
            .gap_2()
            .child(
                Button::new("run-close")
                    .ghost()
                    .label("Close")
                    .on_click(|_, window, cx| window.close_dialog(cx)),
            )
            .child(
                Button::new("run-start")
                    .primary()
                    .label(if self.running { "Running…" } else { "Run" })
                    .loading(self.running)
                    .disabled(self.running || !self.can_run(cx))
                    .on_click(cx.listener(|this, _, window, cx| this.run(window, cx))),
            )
    }
}

impl Render for RunDialog {
    fn render(&mut self, _: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let theme = cx.theme().clone();
        let label = |text: String| {
            div()
                .text_size(rems(0.8))
                .font_weight(FontWeight::MEDIUM)
                .text_color(theme.muted_foreground)
                .child(text)
        };
        v_flex()
            .gap_4()
            .when_some(self.definition.metadata.description.clone(), |this, description| {
                this.child(div().text_sm().text_color(theme.muted_foreground).child(description))
            })
            .when(!self.inputs.is_empty() || !self.flags.is_empty(), |this| {
                this.child(
                    v_flex()
                        .gap_3()
                        .children(self.inputs.iter().map(|(input, state)| {
                            v_flex()
                                .gap_1()
                                .child(label(input.name.clone()))
                                .child(Input::new(state).small())
                        }))
                        .children(self.flags.iter().map(|(name, on)| {
                            let name = name.clone();
                            Checkbox::new(SharedString::from(format!("flag-{name}")))
                                .label(name.clone())
                                .checked(*on)
                                .on_change(cx.listener(move |this, checked: &bool, _, cx| {
                                    this.flags.insert(name.clone(), *checked);
                                    cx.notify();
                                }))
                        })),
                )
            })
            .when(
                self.steps.is_empty() && self.reply.is_none() && !self.definition.steps.is_empty(),
                |this| {
                    this.child(v_flex().gap_1().child(label("Steps".into())).children(
                        self.definition.steps.iter().enumerate().map(|(ix, step)| {
                            h_flex()
                                .gap_2()
                                .text_sm()
                                .child(div().text_color(theme.muted_foreground).child(format!("{}.", ix + 1)))
                                .child(
                                    div()
                                        .font_family(theme.mono_font_family.clone())
                                        .child(step_label(step.kind.as_deref(), step.skill.as_deref())),
                                )
                        }),
                    ))
                },
            )
            .when(!self.steps.is_empty(), |this| {
                this.child(
                    v_flex()
                        .gap_1()
                        .child(label(if self.running {
                            "Running…".into()
                        } else {
                            "Steps".into()
                        }))
                        .children(self.steps.iter().enumerate().map(|(ix, step)| {
                            let (icon, color, note) = match &step.status {
                                StepStatus::Pending => (AppIcon::Spinner, theme.muted_foreground, String::new()),
                                StepStatus::Done => (AppIcon::Check, theme.success, String::new()),
                                StepStatus::Failed(reason) => (AppIcon::Close, theme.danger, reason.clone()),
                            };
                            h_flex()
                                .gap_2()
                                .text_sm()
                                .child(Icon::new(icon).small().text_color(color))
                                .child(div().text_color(theme.muted_foreground).child(format!("{}.", ix + 1)))
                                .child(
                                    div()
                                        .font_family(theme.mono_font_family.clone())
                                        .child(step.label.clone()),
                                )
                                .when(!note.is_empty(), |this| {
                                    this.child(div().text_xs().text_color(theme.danger).child(note))
                                })
                        })),
                )
            })
            .when_some(self.reply.as_ref().and_then(|r| r.result.clone()), |this, result| {
                this.child(
                    v_flex()
                        .gap_2()
                        .child(label(if result.halted {
                            "Result (halted)".into()
                        } else {
                            "Result".into()
                        }))
                        .when_some(result.halt_reason.clone(), |this, reason| {
                            this.child(div().text_sm().text_color(theme.danger).child(reason))
                        })
                        .children(result.steps.iter().map(|step| {
                            let output = render_output(&step.output, &step.kind);
                            v_flex()
                                .gap_1()
                                .child(
                                    div()
                                        .font_family(theme.mono_font_family.clone())
                                        .text_xs()
                                        .text_color(theme.muted_foreground)
                                        .child(step_label(Some(&step.kind), Some(&step.skill))),
                                )
                                .when(!output.is_empty(), |this| {
                                    this.child(
                                        div()
                                            .id(SharedString::from(format!("step-output-{}", step.step_index)))
                                            .max_h(rems(10.))
                                            .overflow_y_scroll()
                                            .p_2()
                                            .rounded(theme.radius)
                                            .bg(theme.background)
                                            .border_1()
                                            .border_color(theme.border)
                                            .font_family(theme.mono_font_family.clone())
                                            .text_xs()
                                            .child(output),
                                    )
                                })
                        })),
                )
            })
            .when(self.reply.is_some() && !self.rated, |this| {
                let rating = self.rating;
                this.child(
                    v_flex().gap_2().child(label("How did it go?".into())).child(
                        h_flex()
                            .gap_2()
                            .child(
                                Button::new("rate-good")
                                    .outline()
                                    .small()
                                    .label("Worked")
                                    .when(rating == Some(true), |this| {
                                        this.bg(theme.success).text_color(theme.success_foreground)
                                    })
                                    .on_click(cx.listener(|this, _, _, cx| {
                                        this.rating = Some(true);
                                        cx.notify();
                                    })),
                            )
                            .child(
                                Button::new("rate-bad")
                                    .outline()
                                    .small()
                                    .label("Didn't work")
                                    .when(rating == Some(false), |this| {
                                        this.bg(theme.danger).text_color(theme.danger_foreground)
                                    })
                                    .on_click(cx.listener(|this, _, _, cx| {
                                        this.rating = Some(false);
                                        cx.notify();
                                    })),
                            )
                            .child(div().flex_1().child(Input::new(&self.note).small()))
                            .child(
                                Button::new("rate-send")
                                    .ghost()
                                    .small()
                                    .label("Send")
                                    .disabled(rating.is_none())
                                    .on_click(cx.listener(|this, _, _, cx| this.rate(cx))),
                            ),
                    ),
                )
            })
            .when(self.rated, |this| {
                this.child(
                    div()
                        .text_sm()
                        .text_color(theme.success)
                        .child("Thanks — rating recorded."),
                )
            })
            .when_some(self.error.clone(), |this, error| {
                this.child(div().text_sm().text_color(theme.danger).child(error))
            })
    }
}

/// Sharing a workflow version with groups.
struct PublishDialog {
    connection: Arc<Connection>,
    workflow: WorkflowSummary,
    page: WeakEntity<WorkflowsPage>,
    groups: Vec<DelegatableGroup>,
    selected: Vec<String>,
    loading: bool,
    pending: bool,
    error: Option<SharedString>,
}

impl PublishDialog {
    fn new(
        connection: Arc<Connection>,
        workflow: WorkflowSummary,
        page: WeakEntity<WorkflowsPage>,
        cx: &mut Context<Self>,
    ) -> Self {
        let loader = connection.clone();
        cx.spawn(async move |this, cx| {
            let groups = runtime::spawn(async move { loader.delegatable().await }).await;
            let _ = this.update(cx, |this, cx| {
                this.loading = false;
                match groups {
                    Ok(found) => this.groups = found.groups,
                    Err(err) => this.error = Some(err.to_string().into()),
                }
                cx.notify();
            });
        })
        .detach();
        Self {
            connection,
            workflow,
            page,
            groups: Vec::new(),
            selected: Vec::new(),
            loading: true,
            pending: false,
            error: None,
        }
    }

    fn publish(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        if self.pending || self.selected.is_empty() {
            return;
        }
        self.pending = true;
        self.error = None;
        cx.notify();
        let connection = self.connection.clone();
        let lineage = self.workflow.lineage_id.clone();
        let version = self.workflow.version;
        let groups = self.selected.clone();
        let page = self.page.clone();
        cx.spawn_in(window, async move |this, cx| {
            let published =
                runtime::spawn(async move { connection.publish_workflow(&lineage, version, &groups).await }).await;
            let _ = this.update_in(cx, |this, window, cx| {
                this.pending = false;
                match published {
                    Ok(()) => {
                        window.close_dialog(cx);
                        let _ = page.update(cx, |page, cx| page.load(cx));
                    }
                    Err(err) => this.error = Some(err.to_string().into()),
                }
                cx.notify();
            });
        })
        .detach();
    }

    fn render_footer(&mut self, cx: &mut Context<Self>) -> impl IntoElement {
        h_flex()
            .w_full()
            .justify_end()
            .gap_2()
            .child(
                Button::new("publish-cancel")
                    .ghost()
                    .label("Cancel")
                    .on_click(|_, window, cx| window.close_dialog(cx)),
            )
            .child(
                Button::new("publish-confirm")
                    .primary()
                    .label("Publish")
                    .loading(self.pending)
                    .disabled(self.pending || self.selected.is_empty())
                    .on_click(cx.listener(|this, _, window, cx| this.publish(window, cx))),
            )
    }
}

impl Render for PublishDialog {
    fn render(&mut self, _: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let theme = cx.theme().clone();
        v_flex()
            .gap_3()
            .child(div().text_sm().text_color(theme.muted_foreground).child(format!(
                "Share version {} of “{}” with these groups.",
                self.workflow.version, self.workflow.name
            )))
            .when(self.loading, |this| {
                this.child(ui::empty_state(None, "Loading groups…", cx))
            })
            .when(
                !self.loading && self.groups.is_empty() && self.error.is_none(),
                |this| {
                    this.child(
                        div()
                            .text_sm()
                            .text_color(console(cx).text_faint)
                            .child("You are not in any group you can publish to."),
                    )
                },
            )
            .children(self.groups.iter().map(|group| {
                let id = group.group_id.clone();
                let checked = self.selected.contains(&id);
                Checkbox::new(SharedString::from(format!("group-{id}")))
                    .label(group.display_name.trim_start_matches("group:").to_owned())
                    .checked(checked)
                    .on_change(cx.listener(move |this, on: &bool, _, cx| {
                        this.selected.retain(|g| g != &id);
                        if *on {
                            this.selected.push(id.clone());
                        }
                        cx.notify();
                    }))
            }))
            .when_some(self.error.clone(), |this, error| {
                this.child(div().text_sm().text_color(theme.danger).child(error))
            })
    }
}

/// A workflow's versions: pin one, clear the pin, decide proposals.
struct VersionsDialog {
    connection: Arc<Connection>,
    workflow: WorkflowSummary,
    page: WeakEntity<WorkflowsPage>,
    versions: Vec<WorkflowVersion>,
    loading: bool,
    pending: bool,
    error: Option<SharedString>,
}

impl VersionsDialog {
    fn new(
        connection: Arc<Connection>,
        workflow: WorkflowSummary,
        page: WeakEntity<WorkflowsPage>,
        cx: &mut Context<Self>,
    ) -> Self {
        let mut this = Self {
            connection,
            workflow,
            page,
            versions: Vec::new(),
            loading: true,
            pending: false,
            error: None,
        };
        this.load(cx);
        this
    }

    fn load(&mut self, cx: &mut Context<Self>) {
        let connection = self.connection.clone();
        let lineage = self.workflow.lineage_id.clone();
        cx.spawn(async move |this, cx| {
            let versions = runtime::spawn(async move { connection.workflow_versions(&lineage).await }).await;
            let _ = this.update(cx, |this, cx| {
                this.loading = false;
                match versions {
                    Ok(versions) => this.versions = versions,
                    Err(err) => this.error = Some(err.to_string().into()),
                }
                cx.notify();
            });
        })
        .detach();
    }

    fn act(&mut self, action: VersionAction, window: &mut Window, cx: &mut Context<Self>) {
        if self.pending {
            return;
        }
        self.pending = true;
        self.error = None;
        cx.notify();
        let connection = self.connection.clone();
        let lineage = self.workflow.lineage_id.clone();
        let page = self.page.clone();
        cx.spawn_in(window, async move |this, cx| {
            let done = runtime::spawn(async move {
                match action {
                    VersionAction::Pin(version) => connection.pin_workflow_version(&lineage, version).await,
                    VersionAction::ClearPin => connection.clear_workflow_pin(&lineage).await,
                    VersionAction::Decide(version, approved) => {
                        connection
                            .decide_workflow_version(&lineage, version, approved, Some(""))
                            .await
                    }
                }
            })
            .await;
            let _ = this.update_in(cx, |this, window, cx| {
                this.pending = false;
                match (done, action) {
                    (Ok(()), VersionAction::Decide(..)) => this.load(cx),
                    (Ok(()), _) => {
                        window.close_dialog(cx);
                        let _ = page.update(cx, |page, cx| page.load(cx));
                    }
                    (Err(err), _) => this.error = Some(err.to_string().into()),
                }
                cx.notify();
            });
        })
        .detach();
    }
}

#[derive(Clone, Copy)]
enum VersionAction {
    Pin(i64),
    ClearPin,
    Decide(i64, bool),
}

impl Render for VersionsDialog {
    fn render(&mut self, _: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let theme = cx.theme().clone();
        v_flex()
            .gap_2()
            .when(self.loading, |this| {
                this.child(ui::empty_state(None, "Loading versions…", cx))
            })
            .children(self.versions.iter().map(|version| {
                let number = version.version;
                let proposed = version.approval_state == "proposed";
                h_flex()
                    .gap_2()
                    .p_2()
                    .rounded(theme.radius)
                    .border_1()
                    .border_color(theme.border)
                    .child(
                        div()
                            .font_weight(FontWeight::MEDIUM)
                            .text_sm()
                            .child(format!("v{number}")),
                    )
                    .child(ui::pill(
                        version.approval_state.clone(),
                        match version.approval_state.as_str() {
                            "approved" => PillTone::Ok,
                            "rejected" => PillTone::Danger,
                            _ => PillTone::Accent,
                        },
                        cx,
                    ))
                    .when(number == self.workflow.version, |this| {
                        this.child(ui::pill("current", PillTone::Neutral, cx))
                    })
                    .child(
                        div()
                            .flex_1()
                            .text_xs()
                            .text_color(theme.muted_foreground)
                            .child(ui::local_time(&version.created_at)),
                    )
                    .when(proposed, |this| {
                        this.child(
                            Button::new(("approve-version", number as usize))
                                .primary()
                                .xsmall()
                                .label("Approve")
                                .disabled(self.pending)
                                .on_click(cx.listener(move |this, _, window, cx| {
                                    this.act(VersionAction::Decide(number, true), window, cx)
                                })),
                        )
                        .child(
                            Button::new(("reject-version", number as usize))
                                .outline()
                                .xsmall()
                                .label("Reject")
                                .disabled(self.pending)
                                .on_click(cx.listener(move |this, _, window, cx| {
                                    this.act(VersionAction::Decide(number, false), window, cx)
                                })),
                        )
                    })
                    .when(
                        version.approval_state == "approved" && number != self.workflow.version,
                        |this| {
                            this.child(
                                Button::new(("pin-version", number as usize))
                                    .outline()
                                    .xsmall()
                                    .label("Pin")
                                    .disabled(self.pending)
                                    .on_click(cx.listener(move |this, _, window, cx| {
                                        this.act(VersionAction::Pin(number), window, cx)
                                    })),
                            )
                        },
                    )
            }))
            .child(
                h_flex().justify_end().child(
                    Button::new("clear-pin")
                        .ghost()
                        .small()
                        .label("Clear pin (use latest)")
                        .disabled(self.pending)
                        .on_click(cx.listener(|this, _, window, cx| this.act(VersionAction::ClearPin, window, cx))),
                ),
            )
            .when_some(self.error.clone(), |this, error| {
                this.child(div().text_sm().text_color(theme.danger).child(error))
            })
    }
}
