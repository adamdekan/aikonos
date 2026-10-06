//! The console: the sidebar and the page beside it.
//!
//! Mirrors webui/web/src/components/Sidebar.vue, SessionsNav.vue and
//! SessionItem.vue: the same nav (with skill-gated entries hidden until the
//! grant is known), the assigned agents, the session list with its filter,
//! and the user row. The chat view lives for as long as the console does,
//! so a run keeps streaming while the user looks at another page; other
//! pages are rebuilt on each visit and so load fresh, as a page load does
//! in the browser.

use std::sync::Arc;
use std::time::{Duration, Instant};

use aikonos_client::Connection;
use aikonos_client::api::chat::AgentSummary;
use aikonos_client::transcript::ManifestEntry;
use aikonos_client::version::ReleaseStatus;
use gpui_kit::component::button::{Button, ButtonVariants as _};
use gpui_kit::component::input::{Input, InputEvent, InputState};
use gpui_kit::component::menu::{DropdownMenu as _, PopupMenuItem};
use gpui_kit::component::{ActiveTheme as _, Icon, Selectable as _, Sizable as _, h_flex, v_flex};
use gpui_kit::prelude::FluentBuilder as _;
use gpui_kit::*;

use crate::app::{self, AppState};
use crate::assets::AppIcon;
use crate::chat::ChatView;
use crate::pages::{
    connections::ConnectionsPage, files::FilesPage, home::HomePage, inbox::InboxPage, schedules::SchedulesPage,
    skills::SkillsPage, workflows::WorkflowsPage,
};
use crate::prefs::Prefs;
use crate::runtime;
use crate::sessions::{self, SessionsStore};
use crate::settings;
use crate::ui;

/// How often the inbox badge refreshes (Sidebar.vue polls every 15 s).
const INBOX_POLL: Duration = Duration::from_secs(15);
/// Coming back to the window refreshes the sidebar if it is older than this.
const STALE_AFTER: Duration = Duration::from_secs(30);

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Route {
    Home,
    Chat,
    Files,
    Connections,
    Schedules,
    Workflows,
    Skills,
    Inbox,
}

struct NavItem {
    label: &'static str,
    icon: AppIcon,
    route: Route,
}

struct ShellHandle(WeakEntity<Shell>);

impl Global for ShellHandle {}

pub struct Shell {
    connection: Arc<Connection>,
    route: Route,
    chat: Entity<ChatView>,
    page: Option<AnyView>,
    sessions: Entity<SessionsStore>,
    session_filter: Entity<InputState>,
    scheduled_only: bool,
    renaming: Option<(String, Entity<InputState>)>,
    inbox_count: usize,
    agents: Vec<AgentSummary>,
    /// None until known: skill-gated nav stays hidden meanwhile.
    granted: Option<Vec<String>>,
    has_providers: bool,
    last_refresh: Instant,
    _tasks: Vec<Task<()>>,
    _subscriptions: Vec<Subscription>,
}

impl Shell {
    pub fn new(window: &mut Window, cx: &mut Context<Self>) -> Self {
        let connection = app::connection(cx);
        cx.set_global(ShellHandle(cx.weak_entity()));
        let sessions = sessions::new_store(connection.clone(), cx);
        let chat = cx.new(|cx| ChatView::new(sessions.clone(), window, cx));
        let session_filter = cx.new(|cx| InputState::new(window, cx).placeholder("Filter sessions…"));
        let page = Some(cx.new(|cx| HomePage::new(window, cx)).into());

        let filter_changed = cx.subscribe(&session_filter, |_, _, event: &InputEvent, cx| {
            if matches!(event, InputEvent::Change) {
                cx.notify();
            }
        });
        let sessions_changed = cx.observe(&sessions, |_, _, cx| cx.notify());
        let activation = cx.observe_window_activation(window, |this, window, cx| {
            if window.is_window_active() && this.last_refresh.elapsed() > STALE_AFTER {
                this.refresh(cx);
            }
        });

        let poll = cx.spawn(async move |this, cx| {
            loop {
                cx.background_executor().timer(INBOX_POLL).await;
                if this.update(cx, |this, cx| this.refresh_inbox(cx)).is_err() {
                    break;
                }
            }
        });
        let keep_alive = {
            let connection = connection.clone();
            cx.spawn(async move |this, cx| {
                loop {
                    cx.background_executor().timer(Duration::from_secs(30)).await;
                    let connection = connection.clone();
                    let alive = runtime::spawn(async move { connection.keep_alive().await }).await;
                    if let Err(err) = alive
                        && err.is_unauthorized()
                    {
                        let _ = this.update_in(cx, |_, window, cx| app::session_expired(window, cx));
                        break;
                    }
                }
            })
        };

        let mut this = Self {
            connection,
            route: Route::Home,
            chat,
            page,
            sessions,
            session_filter,
            scheduled_only: false,
            renaming: None,
            inbox_count: 0,
            agents: Vec::new(),
            granted: None,
            has_providers: false,
            last_refresh: Instant::now(),
            _tasks: vec![poll, keep_alive],
            _subscriptions: vec![filter_changed, sessions_changed, activation],
        };
        this.refresh(cx);
        this
    }

    /// Reload what the sidebar shows: inbox count, agents, grants, the
    /// connector providers and the session list.
    fn refresh(&mut self, cx: &mut Context<Self>) {
        self.last_refresh = Instant::now();
        self.refresh_inbox(cx);
        self.sessions.update(cx, |store, cx| store.reload(cx));
        let connection = self.connection.clone();
        cx.spawn(async move |this, cx| {
            let (agents, granted, providers) = runtime::spawn(async move {
                let (agents, granted, providers) = futures::join!(
                    connection.list_agents(),
                    connection.user_skills(),
                    connection.connector_providers()
                );
                Ok((agents, granted, providers))
            })
            .await
            .unwrap_or_else(|err| (Err(err.clone()), Err(err.clone()), Err(err)));
            let _ = this.update(cx, |this, cx| {
                if let Ok(agents) = agents {
                    this.agents = agents;
                }
                // A failed grant read hides gated items, as the web console does.
                this.granted = Some(granted.unwrap_or_default());
                this.has_providers = providers.map(|p| !p.is_empty()).unwrap_or(false);
                cx.notify();
            });
        })
        .detach();
    }

    fn refresh_inbox(&mut self, cx: &mut Context<Self>) {
        let connection = self.connection.clone();
        cx.spawn(async move |this, cx| {
            let count = runtime::spawn(async move { connection.inbox().await })
                .await
                .map(|envelopes| envelopes.len())
                .unwrap_or(0);
            let _ = this.update(cx, |this, cx| {
                if this.inbox_count != count {
                    this.inbox_count = count;
                    cx.notify();
                }
            });
        })
        .detach();
    }

    pub fn navigate(&mut self, route: Route, window: &mut Window, cx: &mut Context<Self>) {
        self.route = route;
        self.page = match route {
            Route::Chat => None,
            Route::Home => Some(cx.new(|cx| HomePage::new(window, cx)).into()),
            Route::Files => Some(cx.new(|cx| FilesPage::new(window, cx)).into()),
            Route::Connections => Some(cx.new(|cx| ConnectionsPage::new(window, cx)).into()),
            Route::Schedules => Some(cx.new(|cx| SchedulesPage::new(window, cx)).into()),
            Route::Workflows => Some(cx.new(|cx| WorkflowsPage::new(window, cx)).into()),
            Route::Skills => Some(cx.new(|cx| SkillsPage::new(window, cx)).into()),
            Route::Inbox => Some(cx.new(|cx| InboxPage::new(window, cx)).into()),
        };
        if route == Route::Chat {
            self.chat.update(cx, |chat, cx| chat.focus_composer(window, cx));
        }
        cx.notify();
    }

    /// Show a conversation: a saved session, an agent's fresh chat, or a
    /// fresh default chat.
    pub fn open_chat(
        &mut self,
        session: Option<String>,
        agent: Option<String>,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let agent_name = agent
            .as_ref()
            .and_then(|id| self.agents.iter().find(|a| &a.id == id))
            .map(|a| a.name.clone());
        self.chat
            .update(cx, |chat, cx| chat.open(session, agent, agent_name, window, cx));
        self.navigate(Route::Chat, window, cx);
    }

    fn new_chat(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let agent = if self.route == Route::Chat {
            self.chat.read(cx).agent_id()
        } else {
            None
        };
        self.open_chat(None, agent, window, cx);
    }

    fn is_granted(&self, skill: &str) -> bool {
        self.granted
            .as_ref()
            .is_some_and(|granted| granted.iter().any(|g| g == skill))
    }

    fn nav_items(&self) -> Vec<NavItem> {
        let mut items = vec![
            NavItem {
                label: "Chat",
                icon: AppIcon::Chat,
                route: Route::Chat,
            },
            NavItem {
                label: "Files",
                icon: AppIcon::Files,
                route: Route::Files,
            },
        ];
        if self.has_providers {
            items.push(NavItem {
                label: "Connections",
                icon: AppIcon::Connections,
                route: Route::Connections,
            });
        }
        if self.is_granted("scheduler") {
            items.push(NavItem {
                label: "Schedules",
                icon: AppIcon::Schedules,
                route: Route::Schedules,
            });
        }
        if self.is_granted("workflows") {
            items.push(NavItem {
                label: "Workflows",
                icon: AppIcon::PlayCircle,
                route: Route::Workflows,
            });
        }
        items.push(NavItem {
            label: "My Skills",
            icon: AppIcon::Book,
            route: Route::Skills,
        });
        items.push(NavItem {
            label: "Inbox",
            icon: AppIcon::Inbox,
            route: Route::Inbox,
        });
        items
    }

    fn toggle_sidebar(&mut self, cx: &mut Context<Self>) {
        Prefs::update(cx, |prefs| prefs.sidebar_collapsed = !prefs.sidebar_collapsed);
        cx.notify();
    }

    fn toggle_sessions(&mut self, cx: &mut Context<Self>) {
        Prefs::update(cx, |prefs| prefs.sessions_collapsed = !prefs.sessions_collapsed);
        cx.notify();
    }

    fn start_rename(&mut self, entry: &ManifestEntry, window: &mut Window, cx: &mut Context<Self>) {
        let input = cx.new(|cx| InputState::new(window, cx).default_value(entry.title.clone()));
        let id = entry.id.clone();
        let subscription = cx.subscribe_in(&input, window, move |this, input, event, _, cx| match event {
            InputEvent::PressEnter { .. } | InputEvent::Blur => {
                let title = input.read(cx).value().trim().to_owned();
                this.commit_rename(&id, title, cx);
            }
            _ => {}
        });
        input.update(cx, |state, cx| {
            state.focus(window, cx);
            state.select_all(window, cx);
        });
        self._subscriptions.push(subscription);
        self.renaming = Some((entry.id.clone(), input));
        cx.notify();
    }

    fn commit_rename(&mut self, id: &str, title: String, cx: &mut Context<Self>) {
        if self.renaming.as_ref().map(|(renaming, _)| renaming.as_str()) != Some(id) {
            return;
        }
        self.renaming = None;
        let unchanged = self.sessions.read(cx).get(id).is_some_and(|e| e.title == title);
        if !title.is_empty() && !unchanged {
            self.sessions.update(cx, |store, cx| store.rename(id, title, cx));
        }
        cx.notify();
    }

    fn confirm_delete(&mut self, entry: &ManifestEntry, window: &mut Window, cx: &mut Context<Self>) {
        let id = entry.id.clone();
        let title = if entry.title.is_empty() {
            "Untitled".to_owned()
        } else {
            entry.title.clone()
        };
        let sessions = self.sessions.clone();
        use gpui_kit::component::WindowExt as _;
        window.open_alert_dialog(cx, move |alert, _, _| {
            let id = id.clone();
            let sessions = sessions.clone();
            alert
                .title(format!("Delete “{title}”?"))
                .description("The conversation is removed from your workspace for every device.")
                .confirm()
                .ok_text("Delete")
                .ok_variant(gpui_kit::component::button::ButtonVariant::Danger)
                .on_ok(move |_, _, cx| {
                    sessions.update(cx, |store, cx| store.remove(&id, cx));
                    true
                })
        });
    }

    fn render_header(&self, collapsed: bool, cx: &mut Context<Self>) -> impl IntoElement {
        h_flex()
            .flex_none()
            .justify_between()
            .when(collapsed, |this| this.justify_center())
            .pt_4()
            .pb_3()
            .px_3p5()
            .when(collapsed, |this| this.px_0())
            .when(!collapsed, |this| {
                this.child(
                    div()
                        .font_family("Space Grotesk")
                        .font_weight(FontWeight::BOLD)
                        .text_xl()
                        .line_height(rems(2.875))
                        .text_color(cx.theme().foreground)
                        .child("aikonOS"),
                )
            })
            .child(
                Button::new("sidebar-toggle")
                    .ghost()
                    .icon(Icon::new(AppIcon::Sidebar))
                    .tooltip(if collapsed {
                        "Expand sidebar"
                    } else {
                        "Collapse sidebar"
                    })
                    .on_click(cx.listener(|this, _, _, cx| this.toggle_sidebar(cx))),
            )
    }

    #[allow(clippy::too_many_arguments)]
    fn render_nav_item(
        &self,
        id: impl Into<ElementId>,
        label: SharedString,
        icon: AppIcon,
        active: bool,
        badge: Option<usize>,
        collapsed: bool,
        cx: &mut Context<Self>,
    ) -> Button {
        let theme = cx.theme();
        let icon =
            Icon::new(icon)
                .size(rems(1.125))
                .text_color(if active { theme.primary } else { theme.muted_foreground });
        Button::new(id)
            .ghost()
            .w_full()
            .h_9()
            .px_3p5()
            .rounded(theme.radius)
            .selected(active)
            .text_color(if active {
                theme.foreground
            } else {
                theme.muted_foreground
            })
            .when(active, |this| this.bg(theme.sidebar_accent))
            .tooltip(label.clone())
            .child(
                h_flex()
                    .w_full()
                    .gap_2p5()
                    .when(collapsed, |this| this.justify_center())
                    .child(icon)
                    .when(!collapsed, |this| {
                        this.child(
                            div()
                                .flex_1()
                                .min_w_0()
                                .text_sm()
                                .text_ellipsis()
                                .when(active, |this| this.font_weight(FontWeight::MEDIUM))
                                .child(label),
                        )
                    })
                    .when_some(badge.filter(|count| *count > 0), |this, count| {
                        this.child(
                            div()
                                .flex_none()
                                .min_w(rems(1.125))
                                .h(rems(1.125))
                                .px_1()
                                .rounded_full()
                                .bg(theme.primary)
                                .text_color(theme.primary_foreground)
                                .text_size(rems(0.6875))
                                .font_weight(FontWeight::SEMIBOLD)
                                .flex()
                                .items_center()
                                .justify_center()
                                .child(count.to_string()),
                        )
                    }),
            )
    }

    fn render_nav(&self, collapsed: bool, cx: &mut Context<Self>) -> impl IntoElement {
        let chat_agent = self.chat.read(cx).agent_id();
        let chat_session = self.chat.read(cx).session_id();
        let route = self.route;
        let items = self.nav_items();
        v_flex()
            .py_1()
            .gap_px()
            .when(!collapsed, |this| {
                this.child(ui::section_label("Workspace", cx).px_4().pt_2().pb_1())
            })
            .children(items.into_iter().map(|item| {
                let target = item.route;
                let badge = (target == Route::Inbox).then_some(self.inbox_count);
                div().px_2().child(
                    self.render_nav_item(
                        SharedString::from(format!("nav-{}", item.label)),
                        item.label.into(),
                        item.icon,
                        route == target,
                        badge,
                        collapsed,
                        cx,
                    )
                    .on_click(cx.listener(move |this, _, window, cx| {
                        if target == Route::Chat {
                            this.open_chat(None, None, window, cx);
                        } else {
                            this.navigate(target, window, cx);
                        }
                    })),
                )
            }))
            .children(self.agents.iter().map(|agent| {
                let active =
                    route == Route::Chat && chat_session.is_none() && chat_agent.as_deref() == Some(agent.id.as_str());
                let id = agent.id.clone();
                div().px_2().child(
                    self.render_nav_item(
                        SharedString::from(format!("agent-{}", agent.id)),
                        agent.name.clone().into(),
                        AppIcon::Chat,
                        active,
                        None,
                        collapsed,
                        cx,
                    )
                    .on_click(
                        cx.listener(move |this, _, window, cx| this.open_chat(None, Some(id.clone()), window, cx)),
                    ),
                )
            }))
    }

    fn render_session_row(&self, entry: &ManifestEntry, active: bool, cx: &mut Context<Self>) -> AnyElement {
        let theme = cx.theme();
        if let Some((id, input)) = &self.renaming
            && id == &entry.id
        {
            return div()
                .mx_1p5()
                .my_px()
                .child(Input::new(input).small())
                .into_any_element();
        }
        let title: SharedString = if entry.title.is_empty() {
            "Untitled".into()
        } else {
            entry.title.clone().into()
        };
        let session_id = entry.id.clone();
        let agent_id = entry.agent_id.clone().filter(|id| !id.is_empty());
        let menu_entry = entry.clone();
        let pinned = entry.pinned;
        let shell = cx.weak_entity();
        h_flex()
            .id(SharedString::from(format!("session-{}", entry.id)))
            .group("session-row")
            .mx_1p5()
            .my_px()
            .pl_3()
            .pr_1()
            .py_1()
            .gap_1p5()
            .rounded(theme.radius)
            .text_size(rems(0.8125))
            .text_color(if active {
                theme.foreground
            } else {
                theme.muted_foreground
            })
            .when(active, |this| {
                this.bg(theme.sidebar_accent).font_weight(FontWeight::MEDIUM)
            })
            .when(!active, |this| {
                this.hover(|this| this.bg(theme.accent).text_color(theme.foreground))
            })
            .cursor_pointer()
            .tooltip({
                let title = title.clone();
                move |window, cx| gpui_kit::component::tooltip::Tooltip::new(title.clone()).build(window, cx)
            })
            .on_click(cx.listener(move |this, _, window, cx| {
                this.open_chat(Some(session_id.clone()), agent_id.clone(), window, cx)
            }))
            .when(entry.is_scheduled(), |this| {
                this.child(
                    Icon::new(AppIcon::Schedules)
                        .size(rems(0.625))
                        .text_color(theme.muted_foreground),
                )
            })
            .when(entry.has_agent(), |this| {
                this.child(Icon::new(AppIcon::Chat).size(rems(0.625)).text_color(theme.primary))
            })
            .child(
                div()
                    .flex_1()
                    .min_w_0()
                    .text_ellipsis()
                    .whitespace_nowrap()
                    .child(title),
            )
            .child(
                div()
                    .flex_none()
                    .invisible()
                    .group_hover("session-row", |this| this.visible())
                    .when(entry.has_agent() || active, |this| this.visible())
                    .child(
                        Button::new(SharedString::from(format!("session-menu-{}", entry.id)))
                            .ghost()
                            .xsmall()
                            .icon(Icon::new(gpui_kit::component::IconName::Ellipsis))
                            .tooltip("Options")
                            .dropdown_menu(move |menu, _, _| {
                                let rename = {
                                    let shell = shell.clone();
                                    let entry = menu_entry.clone();
                                    move |_: &ClickEvent, window: &mut Window, cx: &mut App| {
                                        let _ = shell.update(cx, |this, cx| this.start_rename(&entry, window, cx));
                                    }
                                };
                                let pin = {
                                    let shell = shell.clone();
                                    let id = menu_entry.id.clone();
                                    move |_: &ClickEvent, _: &mut Window, cx: &mut App| {
                                        let _ = shell.update(cx, |this, cx| {
                                            this.sessions.update(cx, |store, cx| store.toggle_pin(&id, cx))
                                        });
                                    }
                                };
                                let delete = {
                                    let shell = shell.clone();
                                    let entry = menu_entry.clone();
                                    move |_: &ClickEvent, window: &mut Window, cx: &mut App| {
                                        let _ = shell.update(cx, |this, cx| this.confirm_delete(&entry, window, cx));
                                    }
                                };
                                menu.item(PopupMenuItem::new("Rename").on_click(rename))
                                    .item(PopupMenuItem::new(if pinned { "Unpin" } else { "Pin" }).on_click(pin))
                                    .separator()
                                    .item(PopupMenuItem::new("Delete").on_click(delete))
                            }),
                    ),
            )
            .into_any_element()
    }

    fn render_sessions(&self, cx: &mut Context<Self>) -> impl IntoElement {
        let collapsed = Prefs::global(cx).sessions_collapsed;
        let store = self.sessions.read(cx);
        let filter = self.session_filter.read(cx).value().trim().to_lowercase();
        let filtering = self.scheduled_only || !filter.is_empty();
        let rows: Vec<ManifestEntry> = if filtering {
            store
                .all()
                .iter()
                .filter(|entry| !self.scheduled_only || entry.is_scheduled())
                .filter(|entry| filter.is_empty() || entry.title.to_lowercase().contains(&filter))
                .cloned()
                .collect()
        } else {
            store.visible().to_vec()
        };
        let loaded = store.is_loaded();
        let has_more = !filtering && store.has_more();
        let active_session = if self.route == Route::Chat {
            self.chat.read(cx).session_id()
        } else {
            None
        };
        let theme = cx.theme().clone();
        v_flex()
            .py_1()
            .child(
                h_flex()
                    .id("sessions-toggle")
                    .px_4()
                    .pt_2()
                    .pb_1()
                    .justify_between()
                    .cursor_pointer()
                    .on_click(cx.listener(|this, _, _, cx| this.toggle_sessions(cx)))
                    .child(ui::section_label("Sessions", cx))
                    .child(
                        Icon::new(AppIcon::ChevronDown)
                            .size(rems(0.75))
                            .text_color(theme.muted_foreground)
                            .when(collapsed, |icon| icon.rotate(Radians(-std::f32::consts::FRAC_PI_2))),
                    ),
            )
            .when(!collapsed, |this| {
                this.child(
                    h_flex()
                        .px_4()
                        .py_1()
                        .gap_1p5()
                        .child(
                            div()
                                .flex_1()
                                .min_w_0()
                                .child(Input::new(&self.session_filter).xsmall()),
                        )
                        .child(
                            Button::new("scheduled-only")
                                .xsmall()
                                .outline()
                                .rounded(px(999.))
                                .icon(Icon::new(AppIcon::Schedules))
                                .selected(self.scheduled_only)
                                .when(self.scheduled_only, |this| {
                                    this.bg(theme.primary).text_color(theme.primary_foreground)
                                })
                                .tooltip("Scheduled only")
                                .on_click(cx.listener(|this, _, _, cx| {
                                    this.scheduled_only = !this.scheduled_only;
                                    cx.notify();
                                })),
                        ),
                )
                .when(loaded && rows.is_empty(), |this| {
                    this.child(
                        div()
                            .px_4()
                            .py_1p5()
                            .text_xs()
                            .italic()
                            .text_color(theme.muted_foreground)
                            .child("No conversations yet"),
                    )
                })
                .children(rows.iter().map(|entry| {
                    let active = active_session.as_deref() == Some(entry.id.as_str());
                    self.render_session_row(entry, active, cx)
                }))
                .when(has_more, |this| {
                    this.child(
                        div().px_2().child(
                            Button::new("sessions-more")
                                .ghost()
                                .xsmall()
                                .w_full()
                                .label("Show more")
                                .text_color(theme.muted_foreground)
                                .on_click(cx.listener(|this, _, _, cx| {
                                    this.sessions.update(cx, |store, cx| store.show_more(cx))
                                })),
                        ),
                    )
                })
            })
    }

    fn render_footer(&self, collapsed: bool, cx: &mut Context<Self>) -> impl IntoElement {
        let profile = self.connection.profile();
        let display = profile.display_name();
        let name: SharedString = if display.is_empty() {
            profile.email.clone()
        } else {
            display
        }
        .into();
        let release = cx.global::<AppState>().release.clone();
        let theme = cx.theme();
        v_flex()
            .flex_none()
            .when_some(update_notice(&release), |this, (label, url)| {
                this.child(
                    div().px_2().pt_2().child(
                        Button::new("update")
                            .ghost()
                            .xsmall()
                            .w_full()
                            .text_color(theme.primary)
                            .label(if collapsed { "".into() } else { label })
                            .icon(Icon::new(AppIcon::Download))
                            .tooltip("Download the newer aikonOS for Windows from your server")
                            .when_some(url, |this, url| {
                                this.on_click(move |_, window, cx| app::open_link(&url, window, cx))
                            }),
                    ),
                )
            })
            .child(
                h_flex()
                    .gap_2()
                    .px_3p5()
                    .py_3()
                    .when(collapsed, |this| this.justify_center().px_0())
                    .when(!collapsed, |this| {
                        this.child(
                            div()
                                .id("user-name")
                                .flex_1()
                                .min_w_0()
                                .text_sm()
                                .text_ellipsis()
                                .whitespace_nowrap()
                                .text_color(theme.foreground)
                                .child(name.clone())
                                .tooltip({
                                    let email: SharedString = profile.email.clone().into();
                                    move |window, cx| {
                                        gpui_kit::component::tooltip::Tooltip::new(email.clone()).build(window, cx)
                                    }
                                }),
                        )
                    })
                    .child(
                        Button::new("user-settings")
                            .ghost()
                            .icon(Icon::new(AppIcon::Settings))
                            .tooltip("User settings")
                            .on_click(cx.listener(|_, _, window, cx| settings::open(window, cx))),
                    ),
            )
    }
}

fn update_notice(release: &ReleaseStatus) -> Option<(SharedString, Option<SharedString>)> {
    match release {
        ReleaseStatus::UpdateAvailable { version, url, .. } => {
            Some((format!("Update to {version}").into(), url.clone().map(Into::into)))
        }
        _ => None,
    }
}

impl Render for Shell {
    fn render(&mut self, _: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let collapsed = Prefs::global(cx).sidebar_collapsed;
        let theme = cx.theme();
        let sidebar = v_flex()
            .id("sidebar")
            .flex_none()
            .h_full()
            .w(if collapsed { rems(4.) } else { rems(13.75) })
            .bg(theme.sidebar)
            .border_r_1()
            .border_color(theme.sidebar_border)
            .child(self.render_header(collapsed, cx))
            .child(
                v_flex()
                    .id("sidebar-scroll")
                    .flex_1()
                    .min_h_0()
                    .overflow_y_scroll()
                    .child(
                        div().px_2p5().pb_2().when(collapsed, |this| this.px_2()).child(
                            Button::new("new-chat")
                                .primary()
                                .w_full()
                                .h_9()
                                .px_3()
                                .rounded(cx.theme().radius)
                                .tooltip("New chat")
                                .child(
                                    h_flex()
                                        .w_full()
                                        .gap_2()
                                        .when(collapsed, |this| this.justify_center())
                                        .child(Icon::new(AppIcon::Plus))
                                        .when(!collapsed, |this| {
                                            this.child(
                                                div()
                                                    .font_weight(FontWeight::BOLD)
                                                    .text_size(rems(0.8125))
                                                    .child("New chat"),
                                            )
                                        }),
                                )
                                .on_click(cx.listener(|this, _, window, cx| this.new_chat(window, cx))),
                        ),
                    )
                    .child(self.render_nav(collapsed, cx))
                    .when(!collapsed, |this| this.child(self.render_sessions(cx))),
            )
            .child(self.render_footer(collapsed, cx));

        let main = div()
            .id("main-pane")
            .flex_1()
            .min_w_0()
            .h_full()
            .bg(cx.theme().background)
            .map(|this| match &self.page {
                Some(page) => this.child(page.clone()),
                None => this.child(self.chat.clone()),
            });

        h_flex()
            .size_full()
            .items_stretch()
            .text_color(cx.theme().foreground)
            .child(sidebar)
            .child(main)
    }
}

/// Run `f` on the console from a page.
pub fn with_shell(cx: &mut App, f: impl FnOnce(&mut Shell, &mut Context<Shell>) + 'static) {
    if let Some(handle) = cx.try_global::<ShellHandle>().map(|h| h.0.clone()) {
        let _ = handle.update(cx, |shell, cx| f(shell, cx));
    }
}

/// Run `f` on the console from a page, with the window, after the current
/// update (the page itself may be replaced by it).
pub fn with_shell_in(
    window: &mut Window,
    cx: &mut App,
    f: impl FnOnce(&mut Shell, &mut Window, &mut Context<Shell>) + 'static,
) {
    let Some(handle) = cx.try_global::<ShellHandle>().map(|h| h.0.clone()) else {
        return;
    };
    window.defer(cx, move |window, cx| {
        let _ = handle.update(cx, |shell, cx| f(shell, window, cx));
    });
}

/// Start a new conversation with `prompt` already sent (Home's composer).
pub fn start_chat(prompt: String, window: &mut Window, cx: &mut App) {
    with_shell_in(window, cx, move |shell, window, cx| {
        shell.open_chat(None, None, window, cx);
        shell.chat.update(cx, |chat, cx| chat.send_text(prompt, window, cx));
    });
}

/// Open a new conversation with `text` in the composer, not sent (the
/// inbox's "Start a new session").
pub fn prefill_chat(text: String, window: &mut Window, cx: &mut App) {
    with_shell_in(window, cx, move |shell, window, cx| {
        shell.open_chat(None, None, window, cx);
        shell.chat.update(cx, |chat, cx| chat.set_draft(text, window, cx));
    });
}

/// Re-read the inbox count after the inbox page changed it.
pub fn refresh_inbox(cx: &mut App) {
    with_shell(cx, |shell, cx| shell.refresh_inbox(cx));
}

/// Re-read the session list (a workflow run saved one).
pub fn refresh_sessions(cx: &mut App) {
    with_shell(cx, |shell, cx| shell.sessions.update(cx, |store, cx| store.reload(cx)));
}
