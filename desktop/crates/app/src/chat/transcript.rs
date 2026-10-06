//! Drawing the transcript (webui MessageList.vue, ToolTrace.vue,
//! ToolCard.vue, MarkdownMessage.vue and the three timelines), plus the
//! usage strip above the composer (SessionUsage.vue).

use aikonos_client::agui::{RecalledConcept, SkillAnnouncement};
use aikonos_client::money;
use aikonos_client::tool_labels::tool_label;
use aikonos_client::transcript::{AssistantTurn, BranchStatus, SubagentBranch, ToolCall, TranscriptEntry};
use gpui_kit::component::button::{Button, ButtonVariants as _};
use gpui_kit::component::input::Textarea;
use gpui_kit::component::text::TextView;
use gpui_kit::component::{ActiveTheme as _, Disableable as _, Icon, Sizable as _, h_flex, v_flex};
use gpui_kit::prelude::FluentBuilder as _;
use gpui_kit::*;

use super::ChatView;
use crate::app;
use crate::assets::AppIcon;
use crate::prefs::Prefs;
use crate::theme::console;
use crate::ui;

/// The message column's width (MessageList.vue `max-width: 800px`).
const COLUMN: f32 = 50.;

impl ChatView {
    /// One row of the transcript, laid out in the centred column.
    pub(super) fn render_row(&mut self, ix: usize, window: &mut Window, cx: &mut Context<Self>) -> AnyElement {
        let Some(row) = self.rows.get(ix) else {
            return div().into_any_element();
        };
        let key = row.key;
        let last = ix + 1 == self.rows.len();
        let content = match &row.entry {
            TranscriptEntry::User(turn) => self.render_user(key, turn.text.clone(), cx),
            TranscriptEntry::Assistant(turn) => {
                let markdown = row.markdown.clone();
                let turn = turn.clone();
                self.render_assistant(key, &turn, markdown, last, cx)
            }
            TranscriptEntry::Skills(skills) => render_skills(skills, cx),
            TranscriptEntry::Memory(concepts) => render_memory(concepts, cx),
            TranscriptEntry::Subagents(branches) => render_subagents(key, branches, cx),
            TranscriptEntry::Other(_) => div().into_any_element(),
        };
        let _ = window;
        h_flex()
            .w_full()
            .justify_center()
            .px_6()
            .pt(if ix == 0 { rems(1.) } else { rems(0.) })
            .pb(if last { rems(1.) } else { rems(0.75) })
            .child(div().w_full().max_w(rems(COLUMN)).child(content))
            .into_any_element()
    }

    fn render_user(&mut self, key: u64, text: String, cx: &mut Context<Self>) -> AnyElement {
        let theme = cx.theme();
        if let Some((editing, input)) = &self.editing
            && *editing == key
        {
            let input = input.clone();
            return v_flex()
                .w_full()
                .items_end()
                .child(
                    v_flex()
                        .w(relative(0.7))
                        .gap_1p5()
                        .child(
                            div()
                                .px_3()
                                .py_2()
                                .rounded(theme.radius_lg)
                                .bg(theme.popover)
                                .border_1()
                                .border_color(console(cx).accent_text)
                                .child(Textarea::new(&input).appearance(false)),
                        )
                        .child(
                            h_flex()
                                .justify_end()
                                .gap_1p5()
                                .child(
                                    Button::new(("edit-cancel", key))
                                        .outline()
                                        .xsmall()
                                        .label("Cancel")
                                        .on_click(cx.listener(|this, _, _, cx| this.cancel_edit(cx))),
                                )
                                .child(
                                    Button::new(("edit-save", key))
                                        .primary()
                                        .xsmall()
                                        .label("Save & resend")
                                        .disabled(input.read(cx).value().trim().is_empty())
                                        .on_click(cx.listener(|this, _, window, cx| this.save_edit(window, cx))),
                                ),
                        ),
                )
                .into_any_element();
        }
        let running = self.running();
        let edit_text = text.clone();
        v_flex()
            .id(("user-row", key))
            .group("message-row")
            .w_full()
            .items_end()
            .gap_0p5()
            .child(
                div()
                    .max_w(relative(0.7))
                    .px_3p5()
                    .py_2p5()
                    .rounded(theme.radius_lg)
                    .bg(theme.popover)
                    .border_1()
                    .border_color(theme.border)
                    .text_size(rems(0.9375))
                    .line_height(relative(1.6))
                    .text_color(theme.foreground)
                    .child(text),
            )
            .child(
                h_flex()
                    .gap_1()
                    .invisible()
                    .group_hover("message-row", |this| this.visible())
                    .child(
                        Button::new(("msg-edit", key))
                            .outline()
                            .xsmall()
                            .icon(Icon::new(AppIcon::Edit))
                            .tooltip("Edit and resend from here")
                            .disabled(running)
                            .on_click(cx.listener(move |this, _, window, cx| {
                                this.start_edit(key, edit_text.clone(), window, cx)
                            })),
                    ),
            )
            .into_any_element()
    }

    fn render_assistant(
        &mut self,
        key: u64,
        turn: &AssistantTurn,
        markdown: Option<Entity<gpui_kit::component::text::TextViewState>>,
        last: bool,
        cx: &mut Context<Self>,
    ) -> AnyElement {
        let theme = cx.theme().clone();
        let active = last && self.running();
        let debug = Prefs::global(cx).debug_broker;
        let copy_text = turn.text.clone();
        let reply_text = turn.text.clone();
        v_flex()
            .id(("assistant-row", key))
            .group("message-row")
            .w_full()
            .gap_0p5()
            .child(
                v_flex()
                    .w_full()
                    .gap_2()
                    .children(self.render_trace(key, &turn.tools, active, cx))
                    .when_some(markdown.filter(|_| !turn.text.is_empty()), |this, markdown| {
                        this.child(
                            div()
                                .w_full()
                                .text_size(rems(0.9375))
                                .line_height(relative(1.6))
                                .text_color(theme.foreground)
                                .child(
                                    TextView::new(&markdown)
                                        .selectable(true)
                                        .stream_fade(active)
                                        .on_link_click(|url, event, window, cx| {
                                            if event.standard_click() || event.is_middle_click() {
                                                app::open_link(url, window, cx);
                                            }
                                        }),
                                ),
                        )
                    })
                    .when(debug, |this| {
                        this.child(
                            h_flex()
                                .flex_wrap()
                                .gap_1p5()
                                .children(turn.tools.iter().map(|tool| self.render_tool_card(tool, cx))),
                        )
                        .children(
                            turn.tools
                                .iter()
                                .filter(|tool| self.expanded_tools.contains(&tool.id))
                                .map(|tool| render_tool_detail(tool, cx)),
                        )
                    })
                    .when_some(turn.error.clone(), |this, error| {
                        this.child(
                            div()
                                .px_3()
                                .py_2()
                                .rounded(theme.radius)
                                .bg(console(cx).fill_danger)
                                .border_1()
                                .border_color(theme.danger)
                                .text_sm()
                                .text_color(theme.danger)
                                .child(error),
                        )
                    }),
            )
            .when(!turn.text.is_empty(), |this| {
                this.child(
                    h_flex()
                        .gap_1()
                        .invisible()
                        .group_hover("message-row", |this| this.visible())
                        .child(
                            Button::new(("msg-copy", key))
                                .outline()
                                .xsmall()
                                .icon(Icon::new(AppIcon::Copy))
                                .tooltip("Copy response")
                                .on_click(
                                    cx.listener(move |this, _, window, cx| this.copy(copy_text.clone(), window, cx)),
                                ),
                        )
                        .child(
                            Button::new(("msg-reply", key))
                                .outline()
                                .xsmall()
                                .icon(Icon::new(AppIcon::Reply))
                                .tooltip("Reply to this response")
                                .on_click(cx.listener(move |this, _, window, cx| this.reply(&reply_text, window, cx))),
                        ),
                )
            })
            .into_any_element()
    }

    /// One muted line per tool call, with bouncing dots while a call runs
    /// or while the run waits on the model (ToolTrace.vue).
    fn render_trace(&self, key: u64, tools: &[ToolCall], active: bool, cx: &mut Context<Self>) -> Option<AnyElement> {
        let theme = cx.theme();
        let running = |tool: &ToolCall| active && !tool.has_result() && !tool.is_error;
        let standalone = active && !self.text_streaming && !tools.iter().any(running);
        if tools.is_empty() && !standalone {
            return None;
        }
        Some(
            v_flex()
                .gap(rems(0.2))
                .children(tools.iter().map(|tool| {
                    let label = tool_label(&tool.name, tool.description.as_deref(), &tool.args_json);
                    h_flex()
                        .gap(rems(0.4))
                        .min_w_0()
                        .text_size(rems(0.78))
                        .text_color(if tool.is_error {
                            theme.danger
                        } else {
                            theme.muted_foreground
                        })
                        .child(div().min_w_0().text_ellipsis().whitespace_nowrap().child(label))
                        .when(running(tool), |this| {
                            this.child(ui::working_dots(
                                SharedString::from(format!("tool-dots-{}", tool.id)),
                                cx,
                            ))
                        })
                }))
                .when(standalone, |this| {
                    this.child(ui::working_dots(("standalone-dots", key), cx))
                })
                .into_any_element(),
        )
    }

    /// The debug chip for a tool call (ToolCard.vue). Click shows the
    /// arguments and result.
    fn render_tool_card(&self, tool: &ToolCall, cx: &mut Context<Self>) -> AnyElement {
        let theme = cx.theme();
        let tokens = console(cx);
        let (icon, color) = if tool.is_error {
            (AppIcon::Close, theme.danger)
        } else if tool.done {
            (AppIcon::Check, theme.success)
        } else {
            (AppIcon::Code, theme.muted_foreground)
        };
        let id = tool.id.clone();
        h_flex()
            .id(SharedString::from(format!("tool-chip-{}", tool.id)))
            .gap_1()
            .px_2()
            .py_0p5()
            .rounded_full()
            .border_1()
            .border_color(if tool.is_error || tool.done {
                color
            } else {
                theme.border
            })
            .bg(theme.popover)
            .text_xs()
            .text_color(color)
            .cursor_pointer()
            .on_click(cx.listener(move |this, _, _, cx| {
                if !this.expanded_tools.remove(&id) {
                    this.expanded_tools.insert(id.clone());
                }
                cx.notify();
            }))
            .child(Icon::new(icon).xsmall())
            .child(
                div()
                    .font_family(theme.mono_font_family.clone())
                    .child(tool.name.clone()),
            )
            .child(
                div()
                    .px_1p5()
                    .rounded_full()
                    .border_1()
                    .border_color(theme.border)
                    .bg(tokens.fill_muted)
                    .text_size(rems(0.625))
                    .font_weight(FontWeight::SEMIBOLD)
                    .text_color(theme.muted_foreground)
                    .child("GOVERNED"),
            )
            .into_any_element()
    }

    /// Model, tokens and price for the session (SessionUsage.vue). A
    /// session that ran but called no model says so; an empty one shows
    /// nothing.
    pub(super) fn render_usage(&self, cx: &mut Context<Self>) -> Option<AnyElement> {
        let usage = self.usage.as_ref()?;
        let theme = cx.theme();
        let has_content = !self.rows.is_empty();
        if usage.calls == 0 && !has_content {
            return None;
        }
        let item = |label: &'static str, value: String| {
            h_flex()
                .gap(rems(0.3))
                .whitespace_nowrap()
                .child(div().opacity(0.75).child(label))
                .child(div().text_color(theme.foreground).child(value))
        };
        Some(
            h_flex()
                .flex_wrap()
                .gap_x_3p5()
                .gap_y_1()
                .px_0p5()
                .pb_1p5()
                .text_size(rems(0.6875))
                .text_color(theme.muted_foreground)
                .map(|this| {
                    if usage.calls == 0 {
                        this.child(div().opacity(0.75).child("No model calls — no LLM cost"))
                    } else {
                        this.child(item("Model", usage.model_label()))
                            .child(item("Tokens In", money::fmt_count(usage.tokens_in)))
                            .child(item("Tokens Out", money::fmt_count(usage.tokens_out)))
                            .child(item("Price", money::fmt_amount_precise(usage.cost_micros)))
                    }
                })
                .into_any_element(),
        )
    }
}

fn pretty(text: &str) -> String {
    serde_json::from_str::<serde_json::Value>(text)
        .ok()
        .and_then(|value| serde_json::to_string_pretty(&value).ok())
        .unwrap_or_else(|| text.to_owned())
}

fn render_tool_detail(tool: &ToolCall, cx: &App) -> AnyElement {
    let theme = cx.theme();
    let block = |label: &'static str, text: String, error: bool| {
        v_flex()
            .gap_1()
            .child(
                div()
                    .text_size(rems(0.6875))
                    .font_weight(FontWeight::SEMIBOLD)
                    .text_color(theme.muted_foreground)
                    .child(label),
            )
            .child(
                div()
                    .p_2()
                    .rounded(theme.radius)
                    .bg(theme.background)
                    .border_1()
                    .border_color(theme.border)
                    .font_family(theme.mono_font_family.clone())
                    .text_size(rems(0.8125))
                    .text_color(if error { theme.danger } else { theme.muted_foreground })
                    .child(text),
            )
    };
    v_flex()
        .gap_1p5()
        .p_3()
        .rounded(theme.radius)
        .bg(theme.popover)
        .border_1()
        .border_color(theme.border)
        .child(
            div()
                .font_family(theme.mono_font_family.clone())
                .text_size(rems(0.8125))
                .font_weight(FontWeight::SEMIBOLD)
                .child(tool.name.clone()),
        )
        .when(!tool.args_json.is_empty(), |this| {
            this.child(block("ARGS", pretty(&tool.args_json), false))
        })
        .when_some(tool.result_text(), |this, result| {
            this.child(block("RESULT", pretty(&result), tool.is_error))
        })
        .into_any_element()
}

fn timeline(cx: &App) -> Div {
    v_flex()
        .gap(rems(0.375))
        .px_3()
        .py_2()
        .border_l_2()
        .border_color(cx.theme().border)
}

fn timeline_item(icon: AppIcon, icon_color: Hsla, title: impl IntoElement, detail: impl IntoElement, cx: &App) -> Div {
    h_flex()
        .items_start()
        .gap_2()
        .text_color(cx.theme().muted_foreground)
        .child(div().mt_px().child(Icon::new(icon).small().text_color(icon_color)))
        .child(v_flex().child(title).child(detail))
}

/// Skills the gateway loaded (or refused) for a turn (SkillTimeline.vue).
fn render_skills(skills: &[SkillAnnouncement], cx: &App) -> AnyElement {
    let theme = cx.theme();
    timeline(cx)
        .children(skills.iter().map(|skill| {
            let suppressed = skill.status == "suppressed";
            let detail = if suppressed {
                skill.reason.clone()
            } else {
                skill.description.clone()
            };
            timeline_item(
                if suppressed { AppIcon::Close } else { AppIcon::Book },
                if suppressed { theme.danger } else { theme.success },
                div()
                    .font_family(theme.mono_font_family.clone())
                    .text_size(rems(0.8125))
                    .text_color(theme.foreground)
                    .child(skill.name.clone()),
                div()
                    .text_xs()
                    .text_color(if suppressed {
                        theme.danger
                    } else {
                        theme.muted_foreground
                    })
                    .child(detail.unwrap_or_default()),
                cx,
            )
        }))
        .into_any_element()
}

/// Memory injected as context for a turn (MemoryTimeline.vue).
fn render_memory(concepts: &[RecalledConcept], cx: &App) -> AnyElement {
    let theme = cx.theme();
    timeline(cx)
        .children(concepts.iter().map(|concept| {
            let scope = match (&concept.group_id, concept.scope.as_str()) {
                (Some(group), "group") if !group.is_empty() => format!("group:{group}"),
                (_, scope) => scope.to_owned(),
            };
            let title = concept
                .title
                .clone()
                .filter(|t| !t.is_empty())
                .unwrap_or_else(|| concept.id.clone());
            let mut detail = format!("{} · {} · {}", concept.id, concept.status, concept.trust_tier);
            if concept.stale {
                detail.push_str(", stale");
            }
            timeline_item(
                AppIcon::Book,
                if concept.stale {
                    theme.muted_foreground
                } else {
                    console(cx).accent_text
                },
                h_flex()
                    .gap_1()
                    .text_size(rems(0.8125))
                    .text_color(theme.foreground)
                    .child(div().text_color(theme.muted_foreground).child(format!("({scope})")))
                    .child(title),
                div().text_xs().child(detail),
                cx,
            )
        }))
        .into_any_element()
}

fn failure_label(failure: Option<&str>) -> &'static str {
    match failure {
        Some("timeout") => "timed out",
        Some("denied") => "needs to run directly in chat",
        Some("systemic") => "system error",
        _ => "failed",
    }
}

/// A fan-out of sub-agents and how each branch ended
/// (SubagentTimeline.vue).
fn render_subagents(key: u64, branches: &[SubagentBranch], cx: &App) -> AnyElement {
    let theme = cx.theme();
    let accent_text = console(cx).accent_text;
    let resolved = !branches.is_empty() && branches.iter().all(|b| b.status != BranchStatus::Running);
    let total: f64 = branches.iter().map(|b| b.cost).sum();
    timeline(cx)
        .children(branches.iter().map(|branch| {
            let (icon, color, status) = match branch.status {
                BranchStatus::Running => (AppIcon::Spinner, theme.muted_foreground, "running"),
                BranchStatus::Ok => (AppIcon::Check, theme.success, "done"),
                BranchStatus::Failure => match branch.failure.as_deref() {
                    Some("timeout") => (AppIcon::Schedules, accent_text, failure_label(Some("timeout"))),
                    Some("denied") => (AppIcon::Pause, accent_text, failure_label(Some("denied"))),
                    other => (AppIcon::Close, theme.danger, failure_label(other)),
                },
            };
            let detail = if branch.status == BranchStatus::Running {
                status.to_owned()
            } else {
                format!(
                    "{status} · {}",
                    money::fmt_amount_precise(money::to_micros(branch.cost))
                )
            };
            timeline_item(
                icon,
                color,
                div()
                    .text_size(rems(0.8125))
                    .text_color(theme.foreground)
                    .child(format!("agent spawned to do {}", branch.task)),
                div()
                    .font_family(theme.mono_font_family.clone())
                    .text_xs()
                    .child(detail),
                cx,
            )
        }))
        .when(resolved, |this| {
            this.child(
                div()
                    .id(("subagent-total", key))
                    .pt_1()
                    .text_xs()
                    .font_weight(FontWeight::SEMIBOLD)
                    .text_color(theme.foreground)
                    .child(format!("Total: {}", money::fmt_amount_precise(money::to_micros(total)))),
            )
        })
        .into_any_element()
}
