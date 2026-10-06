//! User settings (webui UserSettingsModal.vue and MemorySettings.vue):
//! account, appearance, chat instructions, memory review, and sign-out.
//!
//! The web console's "persist chat history on this device" options have no
//! counterpart: this app never writes a transcript to the PC. Conversations
//! live only in the user's workspace on the server.

use std::sync::Arc;

use aikonos_client::Connection;
use aikonos_client::api::memory::{Concept, ConceptMeta, MemoryGroup, MemoryScope};
use aikonos_client::version::{self, ReleaseStatus};
use gpui_kit::component::WindowExt as _;
use gpui_kit::component::button::{Button, ButtonVariant, ButtonVariants as _};
use gpui_kit::component::dialog::DialogFooter;
use gpui_kit::component::input::{InputEvent, Textarea, TextareaState};
use gpui_kit::component::radio::RadioGroup;
use gpui_kit::component::switch::Switch;
use gpui_kit::component::{ActiveTheme as _, Disableable as _, Selectable as _, Sizable as _, h_flex, v_flex};
use gpui_kit::prelude::FluentBuilder as _;
use gpui_kit::*;

use crate::app::{self, AppState};
use crate::prefs::{MAX_CHAT_INSTRUCTIONS_CHARS, Prefs};
use crate::runtime;
use crate::theme::{self, Appearance, console};
use crate::ui::{self, PillTone};

#[derive(Clone, Copy, PartialEq, Eq)]
enum Category {
    Account,
    Appearance,
    Chat,
    Memory,
}

const CATEGORIES: [(Category, &str); 4] = [
    (Category::Account, "Account"),
    (Category::Appearance, "Appearance"),
    (Category::Chat, "Chat"),
    (Category::Memory, "Memory"),
];

pub struct SettingsPanel {
    connection: Arc<Connection>,
    category: Category,
    instructions: Entity<TextareaState>,
    memory: Entity<MemoryPane>,
    _subscriptions: Vec<Subscription>,
}

impl SettingsPanel {
    fn new(window: &mut Window, cx: &mut Context<Self>) -> Self {
        let connection = app::connection(cx);
        let current = Prefs::global(cx).chat_instructions.clone();
        let instructions = cx.new(|cx| {
            TextareaState::new(window, cx)
                .auto_grow(6, 12)
                .placeholder(
                    "How should your agents behave? e.g. answer in German, keep replies short, always cite file paths…",
                )
                .default_value(current)
        });
        let saved = cx.subscribe(&instructions, |_, input, event: &InputEvent, cx| {
            if matches!(event, InputEvent::Change) {
                let value = input.read(cx).value().to_string();
                Prefs::update(cx, |prefs| prefs.chat_instructions = value);
                cx.notify();
            }
        });
        let memory = cx.new(|cx| MemoryPane::new(connection.clone(), cx));
        Self {
            connection,
            category: Category::Account,
            instructions,
            memory,
            _subscriptions: vec![saved],
        }
    }

    fn field(label: &'static str, cx: &App) -> Div {
        v_flex().gap_1().child(
            div()
                .text_size(rems(0.8))
                .font_weight(FontWeight::MEDIUM)
                .text_color(cx.theme().muted_foreground)
                .child(label),
        )
    }

    fn render_account(&self, cx: &mut Context<Self>) -> AnyElement {
        let profile = self.connection.profile();
        let display = profile.display_name();
        let debug = Prefs::global(cx).debug_broker;
        let release = cx.global::<AppState>().release.clone();
        let theme = cx.theme().clone();
        v_flex()
            .gap_4()
            .child(
                Self::field("Display name", cx).child(div().text_sm().child(if display.is_empty() {
                    profile.email.clone()
                } else {
                    display
                })),
            )
            .child(Self::field("Email", cx).child(div().text_sm().child(profile.email.clone())))
            .child(
                Self::field("Server", cx).child(
                    div()
                        .text_sm()
                        .font_family(theme.mono_font_family.clone())
                        .child(self.connection.server().origin.to_string()),
                ),
            )
            .child(
                Self::field("Debug broker", cx).child(
                    Switch::new("debug-broker")
                        .checked(debug)
                        .label("Show broker governance details in chat")
                        .on_change(cx.listener(|_, on: &bool, _, cx| {
                            let on = *on;
                            Prefs::update(cx, |prefs| prefs.debug_broker = on);
                            cx.refresh_windows();
                        })),
                ),
            )
            .child(
                Self::field("aikonOS for Windows", cx)
                    .child(h_flex().gap_2().text_sm().child(version::CURRENT).map(|this| {
                        match &release {
                            ReleaseStatus::UpdateAvailable { version, url, .. } => this
                                .child(ui::pill(format!("{version} available"), PillTone::Accent, cx))
                                .when_some(url.clone(), |this, url| {
                                    this.child(
                                        Button::new("download-update")
                                            .ghost()
                                            .xsmall()
                                            .label("Download")
                                            .on_click(move |_, window, cx| app::open_link(&url, window, cx)),
                                    )
                                }),
                            _ => this.child(
                                div()
                                    .text_color(theme.muted_foreground)
                                    .child("Up to date with this server"),
                            ),
                        }
                    }))
                    .when_some(
                        match &release {
                            ReleaseStatus::UpdateAvailable { notes, .. } => notes.clone(),
                            _ => None,
                        },
                        |this, notes| this.child(div().text_xs().text_color(theme.muted_foreground).child(notes)),
                    ),
            )
            .into_any_element()
    }

    fn render_appearance(&self, cx: &mut Context<Self>) -> AnyElement {
        let appearance = Prefs::global(cx).appearance;
        let selected = match appearance {
            Appearance::Dark => 0,
            Appearance::Light => 1,
            Appearance::System => 2,
        };
        Self::field("Theme", cx)
            .child(
                RadioGroup::new("theme-mode")
                    .children(["Dark", "Light", "System"])
                    .selected_index(Some(selected))
                    .on_change(cx.listener(|_, ix: &usize, window, cx| {
                        let next = match ix {
                            0 => Appearance::Dark,
                            1 => Appearance::Light,
                            _ => Appearance::System,
                        };
                        Prefs::update(cx, |prefs| prefs.appearance = next);
                        theme::apply(next, Some(window), cx);
                    })),
            )
            .into_any_element()
    }

    fn render_chat(&self, cx: &mut Context<Self>) -> AnyElement {
        let theme = cx.theme().clone();
        let length = self.instructions.read(cx).value().chars().count();
        v_flex()
            .gap_4()
            .child(
                Self::field("Agent instructions", cx)
                    .child(
                        div()
                            .px_2()
                            .py_1()
                            .rounded(theme.radius)
                            .border_1()
                            .border_color(if length > MAX_CHAT_INSTRUCTIONS_CHARS { theme.danger } else { theme.input })
                            .child(Textarea::new(&self.instructions).appearance(false)),
                    )
                    .child(
                        div()
                            .text_xs()
                            .text_color(if length > MAX_CHAT_INSTRUCTIONS_CHARS { theme.danger } else { theme.muted_foreground })
                            .child(format!("{length} / {MAX_CHAT_INSTRUCTIONS_CHARS}")),
                    )
                    .child(div().text_xs().text_color(theme.muted_foreground).child(
                        "Applied to every new chat from this PC. Running chats keep the instructions they started with.",
                    )),
            )
            .child(
                Self::field("History", cx).child(div().text_sm().text_color(theme.muted_foreground).child(
                    "Conversations are saved in your workspace on the server, where the web console sees them too. None are kept on this PC.",
                )),
            )
            .into_any_element()
    }
}

impl Render for SettingsPanel {
    fn render(&mut self, _: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let theme = cx.theme().clone();
        let pane = match self.category {
            Category::Account => self.render_account(cx),
            Category::Appearance => self.render_appearance(cx),
            Category::Chat => self.render_chat(cx),
            Category::Memory => self.memory.clone().into_any_element(),
        };
        let title = CATEGORIES
            .iter()
            .find(|(c, _)| *c == self.category)
            .map(|(_, l)| *l)
            .unwrap_or_default();
        h_flex()
            .items_start()
            .gap_6()
            .min_h(rems(22.))
            .child(
                v_flex()
                    .w(rems(9.))
                    .gap_0p5()
                    .children(CATEGORIES.iter().map(|(category, label)| {
                        let category = *category;
                        let active = category == self.category;
                        ui::row_button(*label, *label, None)
                            .selected(active)
                            .text_color(if active {
                                theme.foreground
                            } else {
                                theme.muted_foreground
                            })
                            .when(active, |this| this.bg(theme.sidebar_accent))
                            .on_click(cx.listener(move |this, _, _, cx| {
                                this.category = category;
                                cx.notify();
                            }))
                    })),
            )
            .child(
                v_flex()
                    .id("settings-pane")
                    .flex_1()
                    .min_w_0()
                    .max_h(rems(30.))
                    .overflow_y_scroll()
                    .gap_4()
                    .child(div().text_base().font_weight(FontWeight::SEMIBOLD).child(title))
                    .child(pane),
            )
    }
}

/// Open the settings dialog.
pub fn open(window: &mut Window, cx: &mut App) {
    let panel = cx.new(|cx| SettingsPanel::new(window, cx));
    window.open_dialog(cx, move |dialog, _, cx| {
        dialog
            .title("Settings")
            .w(px(760.))
            .bg(cx.theme().popover)
            .child(panel.clone())
            .footer(
                DialogFooter::new()
                    .gap_2()
                    .child(
                        Button::new("sign-out")
                            .danger()
                            .label("Sign out")
                            .on_click(|_, window, cx| {
                                window.close_dialog(cx);
                                app::sign_out(window, cx);
                            }),
                    )
                    .child(
                        Button::new("settings-close")
                            .ghost()
                            .label("Close")
                            .on_click(|_, window, cx| window.close_dialog(cx)),
                    ),
            )
    });
}

/// Reviewing what agents remember (MemorySettings.vue): the user's own
/// concepts and their groups'. Agent memory is admin-only and not offered.
struct MemoryPane {
    connection: Arc<Connection>,
    groups: Vec<MemoryGroup>,
    group: Option<String>,
    concepts: Vec<ConceptMeta>,
    selected: Option<Concept>,
    loading: bool,
    forbidden: bool,
    busy: bool,
    error: Option<SharedString>,
}

impl MemoryPane {
    fn new(connection: Arc<Connection>, cx: &mut Context<Self>) -> Self {
        let mut this = Self {
            connection,
            groups: Vec::new(),
            group: None,
            concepts: Vec::new(),
            selected: None,
            loading: true,
            forbidden: false,
            busy: false,
            error: None,
        };
        let connection = this.connection.clone();
        cx.spawn(async move |this, cx| {
            let groups = runtime::spawn(async move { connection.memory_groups().await }).await;
            let _ = this.update(cx, |this, cx| {
                this.groups = groups.unwrap_or_default();
                cx.notify();
            });
        })
        .detach();
        this.load(cx);
        this
    }

    fn scope(&self) -> MemoryScope {
        match &self.group {
            Some(group) => MemoryScope::Group(group.clone()),
            None => MemoryScope::User,
        }
    }

    /// Group managers may verify, deprecate and delete; anyone manages
    /// their own.
    fn can_manage(&self) -> bool {
        match &self.group {
            Some(group) => self.groups.iter().any(|g| &g.group_id == group && g.manager),
            None => true,
        }
    }

    fn load(&mut self, cx: &mut Context<Self>) {
        self.loading = true;
        self.forbidden = false;
        self.error = None;
        self.selected = None;
        let connection = self.connection.clone();
        let scope = self.scope();
        cx.spawn(async move |this, cx| {
            let concepts = runtime::spawn(async move { connection.memory_concepts(&scope).await }).await;
            let _ = this.update(cx, |this, cx| {
                this.loading = false;
                match concepts {
                    Ok(concepts) => this.concepts = concepts,
                    Err(err) if err.is_forbidden() => this.forbidden = true,
                    Err(err) => this.error = Some(err.to_string().into()),
                }
                cx.notify();
            });
        })
        .detach();
        cx.notify();
    }

    fn open_concept(&mut self, id: String, cx: &mut Context<Self>) {
        let connection = self.connection.clone();
        let scope = self.scope();
        cx.spawn(async move |this, cx| {
            let concept = runtime::spawn(async move { connection.memory_concept(&scope, &id).await }).await;
            let _ = this.update(cx, |this, cx| {
                match concept {
                    Ok(concept) => this.selected = Some(concept),
                    Err(err) => this.error = Some(err.to_string().into()),
                }
                cx.notify();
            });
        })
        .detach();
    }

    fn act(&mut self, action: MemoryAction, window: &mut Window, cx: &mut Context<Self>) {
        let Some(selected) = &self.selected else {
            return;
        };
        if self.busy {
            return;
        }
        if action == MemoryAction::Delete {
            let id = selected.meta.id.clone();
            let pane = cx.weak_entity();
            window.open_alert_dialog(cx, move |alert, _, _| {
                let pane = pane.clone();
                alert
                    .title(format!("Delete memory concept “{id}”?"))
                    .description("This cannot be undone.")
                    .confirm()
                    .ok_text("Delete")
                    .ok_variant(ButtonVariant::Danger)
                    .on_ok(move |_, window, cx| {
                        let _ = pane.update(cx, |pane, cx| pane.run(MemoryAction::Delete, window, cx));
                        true
                    })
            });
            return;
        }
        self.run(action, window, cx);
    }

    fn run(&mut self, action: MemoryAction, window: &mut Window, cx: &mut Context<Self>) {
        let Some(selected) = &self.selected else {
            return;
        };
        self.busy = true;
        cx.notify();
        let id = selected.meta.id.clone();
        let scope = self.scope();
        let connection = self.connection.clone();
        cx.spawn_in(window, async move |this, cx| {
            let done = {
                let id = id.clone();
                runtime::spawn(async move {
                    match action {
                        MemoryAction::Verify => connection.verify_concept(&scope, &id).await.map(|_| ()),
                        MemoryAction::Deprecate => connection.deprecate_concept(&scope, &id).await.map(|_| ()),
                        MemoryAction::Delete => connection.delete_concept(&scope, &id).await,
                    }
                })
                .await
            };
            let _ = this.update_in(cx, |this, window, cx| {
                this.busy = false;
                match done {
                    Ok(()) => {
                        let message = match action {
                            MemoryAction::Verify => "Concept verified.",
                            MemoryAction::Deprecate => "Concept deprecated.",
                            MemoryAction::Delete => "Concept deleted.",
                        };
                        app::toast_ok(message, window, cx);
                        let reopen = action != MemoryAction::Delete;
                        this.load(cx);
                        if reopen {
                            this.open_concept(id.clone(), cx);
                        }
                    }
                    Err(err) => app::report(&err, window, cx),
                }
                cx.notify();
            });
        })
        .detach();
    }
}

#[derive(Clone, Copy, PartialEq, Eq)]
enum MemoryAction {
    Verify,
    Deprecate,
    Delete,
}

impl Render for MemoryPane {
    fn render(&mut self, _: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let theme = cx.theme().clone();
        let mut scopes: Vec<(Option<String>, String)> = vec![(None, "Mine".to_owned())];
        scopes.extend(
            self.groups
                .iter()
                .map(|g| (Some(g.group_id.clone()), format!("Group: {}", g.group_id))),
        );
        let manage = self.can_manage();
        v_flex()
            .gap_3()
            .child(
                h_flex()
                    .gap_1()
                    .flex_wrap()
                    .children(scopes.into_iter().enumerate().map(|(ix, (group, label))| {
                        let active = group == self.group;
                        Button::new(("memory-scope", ix))
                            .outline()
                            .xsmall()
                            .label(label)
                            .selected(active)
                            .when(active, |this| {
                                this.bg(theme.primary).text_color(theme.primary_foreground)
                            })
                            .on_click(cx.listener(move |this, _, _, cx| {
                                this.group = group.clone();
                                this.load(cx);
                            }))
                    })),
            )
            .when(self.forbidden, |this| {
                this.child(
                    div()
                        .text_sm()
                        .text_color(console(cx).text_faint)
                        .child("You can't view this memory."),
                )
            })
            .when_some(self.error.clone(), |this, error| {
                this.child(ui::error_banner(error, None, cx))
            })
            .when(self.loading, |this| {
                this.child(div().text_sm().text_color(console(cx).text_faint).child("Loading…"))
            })
            .when(!self.loading && !self.forbidden && self.concepts.is_empty(), |this| {
                this.child(
                    div()
                        .text_sm()
                        .text_color(console(cx).text_faint)
                        .child("Nothing remembered here yet."),
                )
            })
            .children(self.concepts.iter().map(|concept| {
                let id = concept.id.clone();
                let open = self.selected.as_ref().is_some_and(|s| s.meta.id == concept.id);
                h_flex()
                    .id(SharedString::from(format!("concept-{}", concept.id)))
                    .gap_2()
                    .px_3()
                    .py_2()
                    .rounded(theme.radius)
                    .border_1()
                    .border_color(if open { console(cx).accent_text } else { theme.border })
                    .cursor_pointer()
                    .hover(|this| this.bg(theme.accent))
                    .on_click(cx.listener(move |this, _, _, cx| this.open_concept(id.clone(), cx)))
                    .child(
                        v_flex()
                            .flex_1()
                            .min_w_0()
                            .child(div().text_sm().child(if concept.title.is_empty() {
                                concept.id.clone()
                            } else {
                                concept.title.clone()
                            }))
                            .child(
                                div()
                                    .text_xs()
                                    .text_color(theme.muted_foreground)
                                    .child(format!("{} · {}", concept.id, concept.trust_tier)),
                            ),
                    )
                    .child(ui::pill(
                        concept.status.clone(),
                        if concept.status == "deprecated" {
                            PillTone::Danger
                        } else {
                            PillTone::Neutral
                        },
                        cx,
                    ))
                    .when(concept.stale, |this| {
                        this.child(ui::pill("stale", PillTone::Neutral, cx))
                    })
            }))
            .when_some(self.selected.clone(), |this, concept| {
                this.child(
                    v_flex()
                        .gap_2()
                        .p_3()
                        .rounded(theme.radius)
                        .bg(theme.background)
                        .border_1()
                        .border_color(theme.border)
                        .child(
                            div()
                                .text_sm()
                                .font_weight(FontWeight::MEDIUM)
                                .child(concept.meta.title.clone()),
                        )
                        .when(!concept.meta.description.is_empty(), |this| {
                            this.child(
                                div()
                                    .text_xs()
                                    .text_color(theme.muted_foreground)
                                    .child(concept.meta.description.clone()),
                            )
                        })
                        .child(div().text_sm().child(concept.body.clone()))
                        .child(
                            h_flex()
                                .gap_2()
                                .child(
                                    Button::new("concept-verify")
                                        .outline()
                                        .xsmall()
                                        .label("Verify")
                                        .disabled(!manage || self.busy)
                                        .on_click(cx.listener(|this, _, window, cx| {
                                            this.act(MemoryAction::Verify, window, cx)
                                        })),
                                )
                                .child(
                                    Button::new("concept-deprecate")
                                        .outline()
                                        .xsmall()
                                        .label("Deprecate")
                                        .disabled(!manage || self.busy)
                                        .on_click(cx.listener(|this, _, window, cx| {
                                            this.act(MemoryAction::Deprecate, window, cx)
                                        })),
                                )
                                .child(
                                    Button::new("concept-delete")
                                        .outline()
                                        .xsmall()
                                        .label("Delete")
                                        .text_color(theme.danger)
                                        .disabled(!manage || self.busy)
                                        .on_click(cx.listener(|this, _, window, cx| {
                                            this.act(MemoryAction::Delete, window, cx)
                                        })),
                                ),
                        ),
                )
            })
    }
}
