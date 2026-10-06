//! The decision on a paused tool call (webui ApprovalModal.vue).
//!
//! Fail-closed like the web modal: Deny is the focused default, Escape
//! denies, the backdrop does nothing, and a step-up call keeps Approve
//! locked until the user confirms they reviewed the arguments. If the
//! dialog closes any other way while still undecided, the call is denied.

use std::collections::HashSet;
use std::sync::Arc;

use aikonos_client::Connection;
use aikonos_client::agui::ApprovalRequest;
use gpui_kit::component::button::{Button, ButtonVariants as _};
use gpui_kit::component::switch::Switch;
use gpui_kit::component::{ActiveTheme as _, Disableable as _, Icon, Sizable as _, h_flex, v_flex};
use gpui_kit::prelude::FluentBuilder as _;
use gpui_kit::*;
use serde_json::Value;

use crate::assets::AppIcon;
use crate::runtime;
use crate::theme::console;
use crate::ui::{self, PillTone};

const MAX_DEPTH: usize = 4;

pub enum ApprovalEvent {
    /// The server recorded the decision (or the call had already ended).
    Decided { tool_call_id: String },
}

#[derive(Clone, Copy, PartialEq, Eq)]
enum Pending {
    Approve,
    Deny,
}

pub struct ApprovalPanel {
    connection: Arc<Connection>,
    request: ApprovalRequest,
    queue_len: usize,
    reviewed: bool,
    pending: Option<Pending>,
    decided: bool,
    error: Option<SharedString>,
    open_paths: HashSet<String>,
    deny_focus: FocusHandle,
}

impl EventEmitter<ApprovalEvent> for ApprovalPanel {}

impl ApprovalPanel {
    pub fn new(
        connection: Arc<Connection>,
        request: ApprovalRequest,
        queue_len: usize,
        cx: &mut Context<Self>,
    ) -> Self {
        Self {
            connection,
            request,
            queue_len,
            reviewed: false,
            pending: None,
            decided: false,
            error: None,
            open_paths: HashSet::new(),
            deny_focus: cx.focus_handle().tab_stop(true),
        }
    }

    pub fn tool_call_id(&self) -> &str {
        &self.request.tool_call_id
    }

    pub fn is_decided(&self) -> bool {
        self.decided
    }

    pub fn set_queue_len(&mut self, queue_len: usize, cx: &mut Context<Self>) {
        if self.queue_len != queue_len {
            self.queue_len = queue_len;
            cx.notify();
        }
    }

    pub fn focus_deny(&self, window: &mut Window, cx: &mut App) {
        window.focus(&self.deny_focus, cx);
    }

    /// The call settled without a decision from this dialog (its result
    /// arrived, or the run ended): close quietly.
    pub fn mark_settled(&mut self) {
        self.decided = true;
    }

    pub fn respond(&mut self, approved: bool, cx: &mut Context<Self>) {
        if self.pending.is_some() || self.decided {
            return;
        }
        if approved && self.request.is_high_risk() && !self.reviewed {
            return;
        }
        self.pending = Some(if approved { Pending::Approve } else { Pending::Deny });
        self.error = None;
        cx.notify();
        let connection = self.connection.clone();
        let id = self.request.tool_call_id.clone();
        cx.spawn(async move |this, cx| {
            // Detached: a decision must reach the server even if the dialog
            // is torn down while it is in flight.
            let result = runtime::spawn(async move { connection.decide_approval(&id, approved).await }).await;
            let _ = this.update(cx, |this, cx| {
                this.pending = None;
                match result {
                    Ok(_) => {
                        this.decided = true;
                        cx.emit(ApprovalEvent::Decided {
                            tool_call_id: this.request.tool_call_id.clone(),
                        });
                    }
                    // The dialog stays open so the decision is not silently lost.
                    Err(err) => this.error = Some(err.to_string().into()),
                }
                cx.notify();
            });
        })
        .detach();
    }

    /// Deny on the way out if no decision was made (Escape, a closed
    /// window). Fire-and-forget: the dialog is already going away.
    pub fn deny_if_undecided(&mut self) {
        if self.decided || self.pending.is_some() {
            return;
        }
        self.decided = true;
        let connection = self.connection.clone();
        let id = self.request.tool_call_id.clone();
        runtime::detach(async move {
            let _ = connection.decide_approval(&id, false).await;
        });
    }

    pub fn render_footer(&self, cx: &mut Context<Self>) -> impl IntoElement {
        let approve_locked = self.request.is_high_risk() && !self.reviewed;
        let busy = self.pending.is_some();
        h_flex()
            .w_full()
            .justify_end()
            .gap_2()
            .child(
                // Focus starts here, inside the dialog, so Escape (deny)
                // reaches it rather than the composer behind it.
                div().track_focus(&self.deny_focus).child(
                    Button::new("deny")
                        .outline()
                        .icon(Icon::new(AppIcon::Close))
                        .label(if self.pending == Some(Pending::Deny) {
                            "Denying…"
                        } else {
                            "Deny"
                        })
                        .loading(self.pending == Some(Pending::Deny))
                        .disabled(busy)
                        .on_click(cx.listener(|this, _, _, cx| this.respond(false, cx))),
                ),
            )
            .child(
                Button::new("approve")
                    .primary()
                    .icon(Icon::new(AppIcon::Check))
                    .label(if self.pending == Some(Pending::Approve) {
                        "Approving…"
                    } else {
                        "Approve"
                    })
                    .loading(self.pending == Some(Pending::Approve))
                    .disabled(busy || approve_locked)
                    .on_click(cx.listener(|this, _, _, cx| this.respond(true, cx))),
            )
    }

    fn render_args(&self, args: &Value, path: &str, depth: usize, cx: &mut Context<Self>) -> AnyElement {
        let theme = cx.theme().clone();
        let Some(map) = args.as_object().filter(|map| !map.is_empty()) else {
            if depth == 0 {
                return div()
                    .text_sm()
                    .italic()
                    .text_color(console(cx).text_faint)
                    .child("No arguments")
                    .into_any_element();
            }
            return div().into_any_element();
        };
        v_flex()
            .gap_1()
            .children(map.iter().map(|(key, value)| {
                let key_path = format!("{path}/{key}");
                let key_label = div()
                    .flex_none()
                    .w(rems(8.))
                    .font_family(theme.mono_font_family.clone())
                    .text_xs()
                    .text_color(theme.muted_foreground)
                    .text_ellipsis()
                    .child(key.clone());
                match value {
                    Value::Object(_) | Value::Array(_) if depth < MAX_DEPTH => {
                        let open = self.open_paths.contains(&key_path);
                        let count = match value {
                            Value::Array(items) => {
                                format!("[ {} item{} ]", items.len(), if items.len() == 1 { "" } else { "s" })
                            }
                            Value::Object(keys) => {
                                format!("{{ {} key{} }}", keys.len(), if keys.len() == 1 { "" } else { "s" })
                            }
                            _ => String::new(),
                        };
                        let nested = match value {
                            Value::Array(items) => Value::Object(
                                items
                                    .iter()
                                    .enumerate()
                                    .map(|(ix, item)| (ix.to_string(), item.clone()))
                                    .collect(),
                            ),
                            other => other.clone(),
                        };
                        let toggle_path = key_path.clone();
                        v_flex()
                            .child(
                                h_flex()
                                    .id(SharedString::from(format!("arg-{key_path}")))
                                    .gap_2()
                                    .cursor_pointer()
                                    .on_click(cx.listener(move |this, _, _, cx| {
                                        if !this.open_paths.remove(&toggle_path) {
                                            this.open_paths.insert(toggle_path.clone());
                                        }
                                        cx.notify();
                                    }))
                                    .child(
                                        Icon::new(AppIcon::ChevronDown)
                                            .xsmall()
                                            .text_color(theme.muted_foreground)
                                            .when(!open, |icon| icon.rotate(Radians(-std::f32::consts::FRAC_PI_2))),
                                    )
                                    .child(key_label)
                                    .child(div().text_xs().text_color(console(cx).text_faint).child(count)),
                            )
                            .when(open, |this| {
                                this.child(
                                    div()
                                        .pl_4()
                                        .ml_1()
                                        .border_l_1()
                                        .border_color(theme.border)
                                        .child(self.render_args(&nested, &key_path, depth + 1, cx)),
                                )
                            })
                            .into_any_element()
                    }
                    other => {
                        let text = match other {
                            Value::String(text) => text.clone(),
                            Value::Null => "null".into(),
                            other => other.to_string(),
                        };
                        h_flex()
                            .items_start()
                            .gap_2()
                            .child(key_label)
                            .child(
                                div()
                                    .flex_1()
                                    .min_w_0()
                                    .text_sm()
                                    .text_color(theme.foreground)
                                    .when(depth >= MAX_DEPTH, |this| {
                                        this.font_family(theme.mono_font_family.clone())
                                    })
                                    .child(text),
                            )
                            .into_any_element()
                    }
                }
            }))
            .into_any_element()
    }
}

impl Render for ApprovalPanel {
    fn render(&mut self, _: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let theme = cx.theme().clone();
        let request = self.request.clone();
        let high_risk = request.is_high_risk();
        let args = request.args.clone().unwrap_or(Value::Null);
        v_flex()
            .gap_4()
            .child(
                h_flex()
                    .gap_2()
                    .child(Icon::new(AppIcon::Tool).small())
                    .child(
                        div()
                            .flex_1()
                            .font_family(theme.mono_font_family.clone())
                            .text_sm()
                            .font_weight(FontWeight::MEDIUM)
                            .child(request.title().to_owned()),
                    )
                    .child(if request.step_up {
                        ui::pill("STEP-UP", PillTone::Accent, cx)
                    } else {
                        ui::pill("HUMAN", PillTone::Neutral, cx)
                    })
                    .when(self.queue_len > 1, |this| {
                        this.child(
                            div()
                                .text_xs()
                                .text_color(console(cx).text_faint)
                                .child(format!("1 of {}", self.queue_len)),
                        )
                    }),
            )
            .child(div().h_px().bg(theme.border))
            .when_some(request.agent.clone(), |this, agent| {
                this.child(
                    h_flex()
                        .gap_2()
                        .text_sm()
                        .text_color(theme.muted_foreground)
                        .child(Icon::new(AppIcon::Bot).xsmall())
                        .child(div().font_weight(FontWeight::MEDIUM).child("Agent"))
                        .child(div().text_color(theme.foreground).child(agent)),
                )
            })
            .when_some(request.reason.clone(), |this, reason| {
                this.child(div().text_sm().text_color(theme.muted_foreground).child(reason))
            })
            .child(div().h_px().bg(theme.border))
            .child(
                v_flex()
                    .id("approval-args")
                    .gap_3()
                    .max_h(rems(18.))
                    .overflow_y_scroll()
                    .child(
                        div()
                            .text_xs()
                            .font_weight(FontWeight::SEMIBOLD)
                            .text_color(theme.muted_foreground)
                            .child("ARGUMENTS"),
                    )
                    .child(self.render_args(&args, "", 0, cx)),
            )
            .when(high_risk && !self.reviewed, |this| {
                this.child(
                    Switch::new("reviewed")
                        .checked(self.reviewed)
                        .label("I have reviewed the arguments above")
                        .on_change(cx.listener(|this, checked: &bool, _, cx| {
                            this.reviewed = *checked;
                            cx.notify();
                        })),
                )
            })
            .when_some(self.error.clone(), |this, error| {
                this.child(div().text_sm().text_color(theme.danger).child(error))
            })
    }
}
