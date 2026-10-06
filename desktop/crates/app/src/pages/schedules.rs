//! Scheduled runs (webui views/Schedules.vue): create a recurring or
//! one-off prompt, then edit, pause, resume or delete it. A scheduled run
//! acts as its owner with only the tools approved for it in advance.

use std::sync::Arc;

use aikonos_client::Connection;
use aikonos_client::api::schedules::{DEFAULT_APPROVED_TOOLS, Schedule, ScheduleSpec};
use aikonos_client::cron;
use chrono::{Local, NaiveDateTime, TimeZone as _};
use gpui_kit::component::WindowExt as _;
use gpui_kit::component::button::{Button, ButtonVariant, ButtonVariants as _};
use gpui_kit::component::input::{Input, InputState, Textarea, TextareaState};
use gpui_kit::component::select::{Select, SelectEvent, SelectState};
use gpui_kit::component::{ActiveTheme as _, Icon, IndexPath, Sizable as _, h_flex, v_flex};
use gpui_kit::prelude::FluentBuilder as _;
use gpui_kit::*;

use super::recurrence::{DEFAULT_CRON, RecurrenceEditor};
use crate::app;
use crate::assets::AppIcon;
use crate::runtime;
use crate::theme::console;
use crate::ui::{self, PillTone};

const KINDS: [&str; 2] = ["Recurring (cron)", "One-off"];
const RUN_AT_FORMAT: &str = "%Y-%m-%d %H:%M";

/// A schedule's form: create, or edit in place.
struct ScheduleForm {
    prompt: Entity<TextareaState>,
    kind: Entity<SelectState<Vec<&'static str>>>,
    once: bool,
    recurrence: Entity<RecurrenceEditor>,
    run_at: Entity<InputState>,
    /// Edits resend the stored tool list: an absent one would clear it.
    approved_tools: Vec<String>,
    workflow: bool,
    _subscription: Subscription,
}

impl ScheduleForm {
    fn new(schedule: Option<&Schedule>, window: &mut Window, cx: &mut Context<SchedulesPage>) -> Self {
        let once = schedule.is_some_and(Schedule::is_once);
        let prompt_text = schedule.map(|s| s.prompt.clone()).unwrap_or_default();
        let prompt = cx.new(|cx| {
            TextareaState::new(window, cx)
                .auto_grow(2, 6)
                .placeholder("Agent prompt")
                .default_value(prompt_text)
        });
        let kind = cx.new(|cx| SelectState::new(KINDS.to_vec(), Some(IndexPath::new(usize::from(once))), window, cx));
        let cron_expr = schedule
            .map(|s| s.cron_expr.clone())
            .filter(|c| !c.is_empty())
            .unwrap_or_else(|| DEFAULT_CRON.to_owned());
        let recurrence = cx.new(|cx| RecurrenceEditor::new(&cron_expr, window, cx));
        let run_at_text = schedule
            .filter(|s| s.is_once())
            .and_then(|s| s.next_fire_at.as_deref())
            .and_then(|iso| chrono::DateTime::parse_from_rfc3339(iso).ok())
            .map(|t| t.with_timezone(&Local).format(RUN_AT_FORMAT).to_string())
            .unwrap_or_default();
        let run_at = cx.new(|cx| {
            InputState::new(window, cx)
                .placeholder("YYYY-MM-DD HH:MM")
                .default_value(run_at_text)
        });
        let form_kind = kind.clone();
        let subscription = cx.subscribe_in(
            &form_kind,
            window,
            |page, select, event: &SelectEvent<Vec<&'static str>>, _, cx| {
                let SelectEvent::Confirm(Some(label)) = event else {
                    return;
                };
                let once = *label == KINDS[1];
                for form in page.forms_mut() {
                    if form.kind == *select {
                        form.once = once;
                    }
                }
                cx.notify();
            },
        );
        Self {
            prompt,
            kind,
            once,
            recurrence,
            run_at,
            approved_tools: schedule
                .map(|s| s.approved_tools.clone())
                .unwrap_or_else(|| DEFAULT_APPROVED_TOOLS.iter().map(|t| (*t).to_owned()).collect()),
            workflow: schedule.is_some_and(Schedule::runs_workflow),
            _subscription: subscription,
        }
    }

    /// The request body, or the reason it can't be sent.
    fn spec(&self, existing_prompt: Option<&str>, cx: &App) -> Result<ScheduleSpec, String> {
        let prompt = if self.workflow {
            existing_prompt.unwrap_or_default().to_owned()
        } else {
            self.prompt.read(cx).value().trim().to_owned()
        };
        if prompt.is_empty() && !self.workflow {
            return Err("Write the prompt the agent should run.".into());
        }
        if self.once {
            let text = self.run_at.read(cx).value().trim().to_owned();
            let naive = NaiveDateTime::parse_from_str(&text, RUN_AT_FORMAT)
                .map_err(|_| "Enter the time as YYYY-MM-DD HH:MM.".to_owned())?;
            // An explicit offset: the gateway reads the time with `new Date`,
            // which would otherwise use the server's zone.
            let local = Local
                .from_local_datetime(&naive)
                .earliest()
                .ok_or_else(|| "That local time does not exist (a clock change).".to_owned())?;
            Ok(ScheduleSpec {
                prompt,
                kind: "ONCE".into(),
                cron_expr: None,
                run_at: Some(local.to_rfc3339()),
                approved_tools: self.approved_tools.clone(),
            })
        } else {
            let cron_expr = self.recurrence.read(cx).cron(cx);
            if cron_expr.is_empty() {
                return Err("Enter a cron expression.".into());
            }
            Ok(ScheduleSpec {
                prompt,
                kind: "CRON".into(),
                cron_expr: Some(cron_expr),
                run_at: None,
                approved_tools: self.approved_tools.clone(),
            })
        }
    }

    fn render(&self, cx: &App) -> impl IntoElement {
        v_flex()
            .gap_2()
            .when(!self.workflow, |this| {
                this.child(
                    div()
                        .px_2()
                        .py_1()
                        .rounded(cx.theme().radius)
                        .border_1()
                        .border_color(cx.theme().input)
                        .child(Textarea::new(&self.prompt).appearance(false)),
                )
            })
            .child(
                h_flex()
                    .items_start()
                    .gap_2()
                    .child(div().w(rems(11.)).child(Select::new(&self.kind).small()))
                    .child(div().flex_1().map(|this| {
                        if self.once {
                            this.child(div().w(rems(11.)).child(Input::new(&self.run_at).small()))
                        } else {
                            this.child(self.recurrence.clone())
                        }
                    })),
            )
    }
}

pub struct SchedulesPage {
    connection: Arc<Connection>,
    schedules: Vec<Schedule>,
    loading: bool,
    error: Option<SharedString>,
    create: ScheduleForm,
    editing: Option<(String, ScheduleForm)>,
    creating: bool,
}

impl SchedulesPage {
    pub fn new(window: &mut Window, cx: &mut Context<Self>) -> Self {
        let create = ScheduleForm::new(None, window, cx);
        let mut this = Self {
            connection: app::connection(cx),
            schedules: Vec::new(),
            loading: true,
            error: None,
            create,
            editing: None,
            creating: false,
        };
        this.load(cx);
        this
    }

    fn forms_mut(&mut self) -> impl Iterator<Item = &mut ScheduleForm> {
        std::iter::once(&mut self.create).chain(self.editing.as_mut().map(|(_, form)| form))
    }

    fn load(&mut self, cx: &mut Context<Self>) {
        let connection = self.connection.clone();
        cx.spawn(async move |this, cx| {
            let listed = runtime::spawn(async move { connection.schedules().await }).await;
            let _ = this.update(cx, |this, cx| {
                this.loading = false;
                match listed {
                    Ok(schedules) => this.schedules = schedules,
                    Err(err) => this.error = Some(err.to_string().into()),
                }
                cx.notify();
            });
        })
        .detach();
    }

    fn replace_row(&mut self, schedule: Schedule) {
        match self.schedules.iter_mut().find(|s| s.id == schedule.id) {
            Some(row) => *row = schedule,
            None => self.schedules.push(schedule),
        }
    }

    fn create(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        if self.creating {
            return;
        }
        let spec = match self.create.spec(None, cx) {
            Ok(spec) => spec,
            Err(message) => {
                self.error = Some(message.into());
                cx.notify();
                return;
            }
        };
        self.creating = true;
        self.error = None;
        cx.notify();
        let connection = self.connection.clone();
        cx.spawn_in(window, async move |this, cx| {
            let created = runtime::spawn(async move { connection.create_schedule(&spec).await }).await;
            let _ = this.update_in(cx, |this, window, cx| {
                this.creating = false;
                match created {
                    Ok(schedule) => {
                        if let Some(schedule) = schedule {
                            this.replace_row(schedule);
                        }
                        this.create = ScheduleForm::new(None, window, cx);
                    }
                    Err(err) => this.error = Some(err.to_string().into()),
                }
                cx.notify();
            });
        })
        .detach();
    }

    fn start_edit(&mut self, schedule: &Schedule, window: &mut Window, cx: &mut Context<Self>) {
        let form = ScheduleForm::new(Some(schedule), window, cx);
        self.editing = Some((schedule.id.clone(), form));
        cx.notify();
    }

    fn save_edit(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let Some((id, form)) = &self.editing else {
            return;
        };
        let existing = self.schedules.iter().find(|s| &s.id == id).map(|s| s.prompt.clone());
        let spec = match form.spec(existing.as_deref(), cx) {
            Ok(spec) => spec,
            Err(message) => {
                self.error = Some(message.into());
                cx.notify();
                return;
            }
        };
        let id = id.clone();
        let connection = self.connection.clone();
        self.error = None;
        cx.spawn_in(window, async move |this, cx| {
            let updated = runtime::spawn(async move { connection.update_schedule(&id, &spec).await }).await;
            let _ = this.update(cx, |this, cx| {
                match updated {
                    Ok(schedule) => {
                        if let Some(schedule) = schedule {
                            this.replace_row(schedule);
                        }
                        this.editing = None;
                    }
                    Err(err) => this.error = Some(err.to_string().into()),
                }
                cx.notify();
            });
        })
        .detach();
    }

    fn set_paused(&mut self, id: String, paused: bool, cx: &mut Context<Self>) {
        let connection = self.connection.clone();
        self.error = None;
        cx.spawn(async move |this, cx| {
            let updated = runtime::spawn(async move { connection.set_schedule_paused(&id, paused).await }).await;
            let _ = this.update(cx, |this, cx| {
                match updated {
                    Ok(Some(schedule)) => this.replace_row(schedule),
                    Ok(None) => {}
                    Err(err) => this.error = Some(err.to_string().into()),
                }
                cx.notify();
            });
        })
        .detach();
    }

    fn confirm_delete(&mut self, schedule: &Schedule, window: &mut Window, cx: &mut Context<Self>) {
        let id = schedule.id.clone();
        let page = cx.weak_entity();
        window.open_alert_dialog(cx, move |alert, _, _| {
            let id = id.clone();
            let page = page.clone();
            alert
                .title("Delete this schedule?")
                .description("It stops running. Conversations it already created stay.")
                .confirm()
                .ok_text("Delete")
                .ok_variant(ButtonVariant::Danger)
                .on_ok(move |_, _, cx| {
                    let _ = page.update(cx, |this, cx| this.delete(id.clone(), cx));
                    true
                })
        });
    }

    fn delete(&mut self, id: String, cx: &mut Context<Self>) {
        let connection = self.connection.clone();
        self.error = None;
        cx.spawn(async move |this, cx| {
            let deleted = {
                let id = id.clone();
                runtime::spawn(async move { connection.delete_schedule(&id).await }).await
            };
            let _ = this.update(cx, |this, cx| {
                match deleted {
                    Ok(()) => this.schedules.retain(|s| s.id != id),
                    Err(err) => this.error = Some(err.to_string().into()),
                }
                cx.notify();
            });
        })
        .detach();
    }

    fn render_row(&self, schedule: &Schedule, cx: &mut Context<Self>) -> AnyElement {
        let theme = cx.theme().clone();
        let editing = self
            .editing
            .as_ref()
            .filter(|(id, _)| id == &schedule.id)
            .map(|(_, form)| form);
        let main = match editing {
            Some(form) => form.render(cx).into_any_element(),
            None => v_flex()
                .gap_1()
                .child(if schedule.runs_workflow() {
                    h_flex()
                        .gap_2()
                        .child(ui::pill("Workflow", PillTone::Accent, cx))
                        .child(if schedule.workflow_display_name.is_empty() {
                            "(deleted workflow)".to_owned()
                        } else {
                            schedule.workflow_display_name.clone()
                        })
                        .into_any_element()
                } else {
                    div().text_sm().child(schedule.prompt.clone()).into_any_element()
                })
                .child(
                    h_flex()
                        .gap_2()
                        .flex_wrap()
                        .child(ui::pill(
                            schedule.state.clone(),
                            if schedule.state == "ACTIVE" {
                                PillTone::Ok
                            } else {
                                PillTone::Neutral
                            },
                            cx,
                        ))
                        .child(ui::pill(schedule.kind.clone(), PillTone::Neutral, cx))
                        .when(!schedule.cron_expr.is_empty(), |this| {
                            this.child(
                                div()
                                    .id(SharedString::from(format!("cron-{}", schedule.id)))
                                    .font_family(theme.mono_font_family.clone())
                                    .text_xs()
                                    .text_color(theme.muted_foreground)
                                    .child(cron::describe_cron(&schedule.cron_expr)),
                            )
                        }),
                )
                .when_some(schedule.next_fire_at.clone(), |this, next| {
                    this.child(
                        div()
                            .text_xs()
                            .text_color(console(cx).text_faint)
                            .child(format!("Next: {}", ui::local_time(&next))),
                    )
                })
                .into_any_element(),
        };
        let id = schedule.id.clone();
        let actions = if editing.is_some() {
            h_flex()
                .gap_1()
                .child(
                    Button::new(SharedString::from(format!("save-{id}")))
                        .ghost()
                        .small()
                        .icon(Icon::new(AppIcon::Check))
                        .tooltip("Save")
                        .on_click(cx.listener(|this, _, window, cx| this.save_edit(window, cx))),
                )
                .child(
                    Button::new(SharedString::from(format!("cancel-{id}")))
                        .ghost()
                        .small()
                        .icon(Icon::new(AppIcon::Close))
                        .tooltip("Cancel editing")
                        .on_click(cx.listener(|this, _, _, cx| {
                            this.editing = None;
                            cx.notify();
                        })),
                )
        } else {
            let for_edit = schedule.clone();
            let for_delete = schedule.clone();
            let pause_id = id.clone();
            let state = schedule.state.clone();
            h_flex()
                .gap_1()
                .child(
                    Button::new(SharedString::from(format!("edit-{id}")))
                        .ghost()
                        .small()
                        .icon(Icon::new(AppIcon::Edit))
                        .tooltip("Edit")
                        .on_click(cx.listener(move |this, _, window, cx| this.start_edit(&for_edit, window, cx))),
                )
                .when(state == "ACTIVE" || state == "PAUSED", |this| {
                    let paused = state == "PAUSED";
                    this.child(
                        Button::new(SharedString::from(format!("pause-{id}")))
                            .ghost()
                            .small()
                            .icon(Icon::new(if paused { AppIcon::Play } else { AppIcon::Pause }))
                            .tooltip(if paused { "Resume" } else { "Pause" })
                            .on_click(
                                cx.listener(move |this, _, _, cx| this.set_paused(pause_id.clone(), !paused, cx)),
                            ),
                    )
                })
                .child(
                    Button::new(SharedString::from(format!("delete-{id}")))
                        .ghost()
                        .small()
                        .icon(Icon::new(AppIcon::Trash))
                        .text_color(theme.danger)
                        .tooltip("Delete")
                        .on_click(cx.listener(move |this, _, window, cx| this.confirm_delete(&for_delete, window, cx))),
                )
        };
        h_flex()
            .items_start()
            .gap_3()
            .p_4()
            .bg(theme.popover)
            .border_1()
            .border_color(theme.border)
            .rounded(theme.radius)
            .child(div().flex_1().min_w_0().child(main))
            .child(actions)
            .into_any_element()
    }
}

impl Render for SchedulesPage {
    fn render(&mut self, _: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let theme = cx.theme().clone();
        ui::page("schedules-page", ui::PAGE_WIDTH)
            .child(ui::view_header(AppIcon::Schedules, "Schedules", cx))
            .when_some(self.error.clone(), |this, error| {
                this.child(ui::error_banner(error, None, cx))
            })
            .child(
                v_flex()
                    .gap_3()
                    .p_4()
                    .bg(theme.popover)
                    .border_1()
                    .border_color(theme.border)
                    .rounded(theme.radius)
                    .child(
                        div()
                            .text_sm()
                            .font_weight(FontWeight::SEMIBOLD)
                            .text_color(theme.muted_foreground)
                            .child("New schedule"),
                    )
                    .child(self.create.render(cx))
                    .child(
                        h_flex().justify_end().child(
                            Button::new("create-schedule")
                                .primary()
                                .icon(Icon::new(AppIcon::Plus))
                                .label("Create")
                                .loading(self.creating)
                                .on_click(cx.listener(|this, _, window, cx| this.create(window, cx))),
                        ),
                    ),
            )
            .when(self.loading, |this| this.child(ui::empty_state(None, "Loading…", cx)))
            .when(
                !self.loading && self.schedules.is_empty() && self.error.is_none(),
                |this| {
                    this.child(
                        div()
                            .text_sm()
                            .text_color(console(cx).text_faint)
                            .child("No schedules yet."),
                    )
                },
            )
            .child(
                v_flex()
                    .gap_2()
                    .children(self.schedules.iter().map(|schedule| self.render_row(schedule, cx))),
            )
    }
}
