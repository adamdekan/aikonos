//! Tasks colleagues delegated to this user and skills they shared
//! (webui views/Inbox.vue, inbox/EnvelopeRow.vue, SkillTransferModal.vue).

use std::sync::Arc;

use aikonos_client::Connection;
use aikonos_client::api::inbox::{AcceptMode, Envelope, SkillTransferPreview};
use gpui_kit::component::WindowExt as _;
use gpui_kit::component::button::{Button, ButtonVariants as _};
use gpui_kit::component::dialog::DialogFooter;
use gpui_kit::component::input::{Input, InputState};
use gpui_kit::component::radio::RadioGroup;
use gpui_kit::component::{ActiveTheme as _, Disableable as _, Icon, Sizable as _, h_flex, v_flex};
use gpui_kit::prelude::FluentBuilder as _;
use gpui_kit::*;

use crate::app;
use crate::assets::AppIcon;
use crate::runtime;
use crate::shell;
use crate::theme::console;
use crate::ui::{self, PillTone};

pub struct InboxPage {
    connection: Arc<Connection>,
    envelopes: Vec<Envelope>,
    loading: bool,
    error: Option<SharedString>,
}

impl InboxPage {
    pub fn new(_: &mut Window, cx: &mut Context<Self>) -> Self {
        let mut this = Self {
            connection: app::connection(cx),
            envelopes: Vec::new(),
            loading: true,
            error: None,
        };
        this.load(cx);
        this
    }

    fn load(&mut self, cx: &mut Context<Self>) {
        let connection = self.connection.clone();
        cx.spawn(async move |this, cx| {
            let listed = runtime::spawn(async move { connection.inbox().await }).await;
            let _ = this.update(cx, |this, cx| {
                this.loading = false;
                match listed {
                    Ok(envelopes) => {
                        this.envelopes = envelopes;
                        this.error = None;
                    }
                    Err(err) => this.error = Some(err.to_string().into()),
                }
                shell::refresh_inbox(cx);
                cx.notify();
            });
        })
        .detach();
    }

    /// Send the delegated intent to the agent now, and clear the envelope.
    fn send_to_agent(&mut self, envelope: &Envelope, window: &mut Window, cx: &mut Context<Self>) {
        let intent = envelope
            .task
            .as_ref()
            .map(|t| t.intent.trim().to_owned())
            .unwrap_or_default();
        if intent.is_empty() {
            app::toast_error("Nothing to send", window, cx);
            return;
        }
        let connection = self.connection.clone();
        let id = envelope.envelope_id.clone();
        runtime::detach(async move {
            let _ = connection.dismiss_envelope(&id).await;
        });
        shell::start_chat(intent, window, cx);
    }

    /// Open a new conversation with the intent in the composer, unsent.
    fn start_session(&mut self, envelope: &Envelope, window: &mut Window, cx: &mut Context<Self>) {
        let intent = envelope.task.as_ref().map(|t| t.intent.clone()).unwrap_or_default();
        shell::prefill_chat(intent, window, cx);
    }

    fn dismiss(&mut self, id: String, window: &mut Window, cx: &mut Context<Self>) {
        let Some(ix) = self.envelopes.iter().position(|e| e.envelope_id == id) else {
            return;
        };
        let removed = self.envelopes.remove(ix);
        self.error = None;
        cx.notify();
        let connection = self.connection.clone();
        cx.spawn_in(window, async move |this, cx| {
            let result = runtime::spawn(async move { connection.dismiss_envelope(&id).await }).await;
            let _ = this.update_in(cx, |this, window, cx| {
                match result {
                    Ok(()) => app::toast_ok("Dismissed", window, cx),
                    Err(err) => {
                        // Put it back where it was so nothing is silently lost.
                        let ix = ix.min(this.envelopes.len());
                        this.envelopes.insert(ix, removed);
                        this.error = Some(err.to_string().into());
                        app::toast_error(err.to_string(), window, cx);
                    }
                }
                shell::refresh_inbox(cx);
                cx.notify();
            });
        })
        .detach();
    }

    fn review(&mut self, envelope: &Envelope, window: &mut Window, cx: &mut Context<Self>) {
        let page = cx.weak_entity();
        let review = cx.new(|cx| TransferReview::new(self.connection.clone(), envelope, window, cx));
        window.open_dialog(cx, move |dialog, _, cx| {
            let review_for_footer = review.clone();
            let page = page.clone();
            dialog
                .title(
                    h_flex()
                        .gap_2()
                        .child(Icon::new(AppIcon::Book).small())
                        .child("Skill transfer"),
                )
                .w(px(600.))
                .bg(cx.theme().popover)
                .child(review.clone())
                .footer(DialogFooter::new().child(review_for_footer.update(cx, |review, cx| {
                    review.render_footer(page.clone(), cx).into_any_element()
                })))
        });
    }

    fn render_envelope(&self, envelope: &Envelope, cx: &mut Context<Self>) -> AnyElement {
        let theme = cx.theme();
        let transfer = envelope.is_skill_transfer();
        let intent = envelope.task.as_ref().map(|t| t.intent.clone()).unwrap_or_default();
        let id = envelope.envelope_id.clone();
        let for_send = envelope.clone();
        let for_session = envelope.clone();
        let for_review = envelope.clone();
        v_flex()
            .gap_2()
            .p_4()
            .bg(theme.popover)
            .border_1()
            .border_color(theme.border)
            .rounded(theme.radius)
            .child(
                h_flex()
                    .gap_2()
                    .when(transfer, |this| {
                        this.child(
                            Button::new(SharedString::from(format!("review-{id}")))
                                .primary()
                                .small()
                                .icon(Icon::new(AppIcon::Book))
                                .label("Review")
                                .on_click(cx.listener(move |this, _, window, cx| this.review(&for_review, window, cx))),
                        )
                    })
                    .when(!transfer, |this| {
                        this.child(
                            Button::new(SharedString::from(format!("send-{id}")))
                                .primary()
                                .small()
                                .icon(Icon::new(AppIcon::Check))
                                .label("Send to agent")
                                .on_click(
                                    cx.listener(move |this, _, window, cx| this.send_to_agent(&for_send, window, cx)),
                                ),
                        )
                        .child(
                            Button::new(SharedString::from(format!("session-{id}")))
                                .outline()
                                .small()
                                .icon(Icon::new(AppIcon::Send))
                                .label("New session")
                                .on_click(
                                    cx.listener(move |this, _, window, cx| {
                                        this.start_session(&for_session, window, cx)
                                    }),
                                ),
                        )
                    })
                    .child(
                        Button::new(SharedString::from(format!("dismiss-{id}")))
                            .ghost()
                            .small()
                            .label("OK")
                            .on_click(cx.listener(move |this, _, window, cx| this.dismiss(id.clone(), window, cx))),
                    ),
            )
            .child(
                h_flex()
                    .gap_1p5()
                    .text_sm()
                    .text_color(theme.muted_foreground)
                    .child(Icon::new(AppIcon::Send).xsmall())
                    .child(envelope.sender().to_owned()),
            )
            .child(
                div()
                    .id(SharedString::from(format!("intent-{}", envelope.envelope_id)))
                    .max_h(rems(8.))
                    .overflow_y_scroll()
                    .text_sm()
                    .text_color(theme.foreground)
                    .when(transfer, |this| {
                        this.child(
                            h_flex()
                                .gap_2()
                                .child(ui::pill("Skill transfer", PillTone::Accent, cx))
                                .child(intent.trim_start_matches("Skill transfer: ").to_owned()),
                        )
                    })
                    .when(!transfer, |this| this.child(intent)),
            )
            .into_any_element()
    }
}

impl Render for InboxPage {
    fn render(&mut self, _: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let theme = cx.theme().clone();
        let count = self.envelopes.len();
        ui::page("inbox-page", ui::PAGE_WIDTH)
            .child(ui::view_header(AppIcon::Inbox, "Inbox", cx).when(count > 0, |this| {
                this.child(
                    div()
                        .min_w(rems(1.5))
                        .px_2()
                        .rounded_full()
                        .bg(theme.primary)
                        .text_color(theme.primary_foreground)
                        .text_xs()
                        .font_weight(FontWeight::SEMIBOLD)
                        .text_center()
                        .child(count.to_string()),
                )
            }))
            .when_some(self.error.clone(), |this, error| {
                this.child(ui::error_banner(error, None, cx))
            })
            .when(self.loading, |this| this.child(ui::empty_state(None, "Loading…", cx)))
            .when(!self.loading, |this| {
                this.child(
                    v_flex()
                        .gap_3()
                        .child(
                            div()
                                .text_sm()
                                .font_weight(FontWeight::SEMIBOLD)
                                .text_color(theme.muted_foreground)
                                .child("Pending delegations"),
                        )
                        .when(count == 0 && self.error.is_none(), |this| {
                            this.child(ui::empty_state(Some(AppIcon::Inbox), "No pending delegations.", cx))
                        })
                        .children(self.envelopes.iter().map(|envelope| self.render_envelope(envelope, cx))),
                )
            })
    }
}

/// Reviewing a shared skill before installing it.
struct TransferReview {
    connection: Arc<Connection>,
    envelope_id: String,
    sender: String,
    preview: Option<SkillTransferPreview>,
    loading: bool,
    error: Option<SharedString>,
    replace: bool,
    name: Entity<InputState>,
    submitting: bool,
    submit_error: Option<SharedString>,
}

impl TransferReview {
    fn new(connection: Arc<Connection>, envelope: &Envelope, window: &mut Window, cx: &mut Context<Self>) -> Self {
        let mut this = Self {
            connection,
            envelope_id: envelope.envelope_id.clone(),
            sender: envelope.sender().to_owned(),
            preview: None,
            loading: true,
            error: None,
            replace: false,
            name: cx.new(|cx| InputState::new(window, cx).placeholder("New name")),
            submitting: false,
            submit_error: None,
        };
        this.load(window, cx);
        this
    }

    fn load(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        self.loading = true;
        self.error = None;
        let connection = self.connection.clone();
        let id = self.envelope_id.clone();
        cx.spawn_in(window, async move |this, cx| {
            let preview = runtime::spawn(async move { connection.skill_transfer_preview(&id).await }).await;
            let _ = this.update_in(cx, |this, window, cx| {
                this.loading = false;
                match preview {
                    Ok(preview) => {
                        // On a name clash, offer the server's usual free name.
                        if preview.conflict {
                            let suggestion = format!("{}-2", preview.skill_name);
                            this.name.update(cx, |name, cx| name.set_value(suggestion, window, cx));
                        }
                        this.preview = Some(preview);
                    }
                    Err(err) if err.is_forbidden() => {
                        this.error = Some("You do not have access to this transfer.".into())
                    }
                    Err(err) => this.error = Some(err.to_string().into()),
                }
                cx.notify();
            });
        })
        .detach();
    }

    fn render_footer(&mut self, page: WeakEntity<InboxPage>, cx: &mut Context<Self>) -> impl IntoElement {
        let ready = self.preview.is_some();
        h_flex()
            .w_full()
            .justify_end()
            .gap_2()
            .child(
                Button::new("transfer-cancel")
                    .ghost()
                    .label("Cancel")
                    .disabled(self.submitting)
                    .on_click(|_, window, cx| window.close_dialog(cx)),
            )
            .when(ready, |this| {
                this.child(
                    Button::new("transfer-confirm")
                        .map(|button| {
                            if self.replace {
                                button.danger()
                            } else {
                                button.primary()
                            }
                        })
                        .label(if self.submitting {
                            "Working…"
                        } else if self.replace {
                            "Replace"
                        } else {
                            "Accept"
                        })
                        .disabled(self.submitting)
                        .on_click(cx.listener(move |this, _, window, cx| this.confirm(page.clone(), window, cx))),
                )
            })
    }

    fn confirm(&mut self, page: WeakEntity<InboxPage>, window: &mut Window, cx: &mut Context<Self>) {
        let Some(preview) = &self.preview else {
            return;
        };
        if self.submitting {
            return;
        }
        let (mode, name) = if self.replace {
            (AcceptMode::Replace, None)
        } else if preview.conflict {
            (AcceptMode::Rename, Some(self.name.read(cx).value().trim().to_owned()))
        } else {
            (AcceptMode::Rename, None)
        };
        self.submitting = true;
        self.submit_error = None;
        cx.notify();
        let connection = self.connection.clone();
        let id = self.envelope_id.clone();
        cx.spawn_in(window, async move |this, cx| {
            let accepted =
                runtime::spawn(async move { connection.accept_skill_transfer(&id, mode, name.as_deref()).await }).await;
            let _ = this.update_in(cx, |this, window, cx| {
                this.submitting = false;
                match accepted {
                    Ok(_) => {
                        window.close_dialog(cx);
                        app::toast_ok("Skill installed.", window, cx);
                        let _ = page.update(cx, |page, cx| page.load(cx));
                    }
                    Err(err) => this.submit_error = Some(err.to_string().into()),
                }
                cx.notify();
            });
        })
        .detach();
    }
}

impl Render for TransferReview {
    fn render(&mut self, _: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let theme = cx.theme();
        if self.loading {
            return ui::empty_state(None, "Loading…", cx).into_any_element();
        }
        if let Some(error) = self.error.clone() {
            return ui::error_banner(
                error,
                Some(
                    Button::new("transfer-retry")
                        .ghost()
                        .xsmall()
                        .label("Retry")
                        .on_click(cx.listener(|this, _, window, cx| this.load(window, cx)))
                        .into_any_element(),
                ),
                cx,
            )
            .into_any_element();
        }
        let Some(preview) = self.preview.clone() else {
            return div().into_any_element();
        };
        let section_title = |title: &'static str| {
            div()
                .text_xs()
                .font_weight(FontWeight::SEMIBOLD)
                .text_color(theme.muted_foreground)
                .child(title)
        };
        v_flex()
            .gap_4()
            .child(
                div().text_sm().child(format!(
                    "From {} — {}",
                    if self.sender.is_empty() { &preview.from_user_id } else { &self.sender },
                    preview.skill_name
                )),
            )
            .when(!preview.flags.is_empty(), |this| {
                this.child(
                    v_flex()
                        .gap_1()
                        .p_3()
                        .rounded(theme.radius)
                        .bg(console(cx).fill_danger)
                        .border_1()
                        .border_color(theme.danger)
                        .text_sm()
                        .text_color(theme.danger)
                        .child(div().font_weight(FontWeight::SEMIBOLD).child("Flagged content — review before accepting"))
                        .children(preview.flags.iter().map(|flag| div().child(format!("• {flag}")))),
                )
            })
            .child(
                v_flex()
                    .gap_1()
                    .child(section_title("Body"))
                    .child(
                        div()
                            .id("transfer-body")
                            .max_h(rems(12.))
                            .overflow_y_scroll()
                            .p_2()
                            .rounded(theme.radius)
                            .bg(theme.background)
                            .border_1()
                            .border_color(theme.border)
                            .font_family(theme.mono_font_family.clone())
                            .text_xs()
                            .child(preview.body.clone()),
                    ),
            )
            .child(
                v_flex()
                    .gap_1()
                    .child(section_title("Files"))
                    .children(preview.manifest.iter().map(|file| {
                        h_flex()
                            .justify_between()
                            .text_xs()
                            .child(div().font_family(theme.mono_font_family.clone()).child(file.path.clone()))
                            .child(div().text_color(theme.muted_foreground).child(ui::file_size(file.size)))
                    })),
            )
            .child(
                v_flex()
                    .gap_2()
                    .child(section_title("Accept as"))
                    .when(!preview.conflict, |this| this.child(div().text_sm().child("Install as a new personal skill.")))
                    .when(preview.conflict, |this| {
                        this.child(
                            RadioGroup::new("transfer-mode")
                                .children(["Rename", "Replace"])
                                .selected_index(Some(usize::from(self.replace)))
                                .on_change(cx.listener(|this, ix: &usize, _, cx| {
                                    this.replace = *ix == 1;
                                    cx.notify();
                                })),
                        )
                        .when(!self.replace, |this| this.child(Input::new(&self.name).small()))
                        .when(self.replace, |this| {
                            this.child(div().text_xs().text_color(theme.danger).child(format!(
                                "This deletes your existing Skills/{}/ and replaces it with the transferred version. This cannot be undone.",
                                preview.skill_name
                            )))
                        })
                    }),
            )
            .when_some(self.submit_error.clone(), |this, error| {
                this.child(div().text_sm().text_color(theme.danger).child(error))
            })
            .into_any_element()
    }
}
