//! Connected storage accounts (webui views/Connections.vue).
//!
//! Connecting finishes in the browser: the provider returns to the web
//! console's `/connect/callback` page, which completes the link with the
//! browser's own session. The list refreshes when the user comes back to
//! this window.

use std::sync::Arc;

use aikonos_client::Connection;
use aikonos_client::api::connectors::{ConnectorProvider, ConnectorStatus};
use gpui_kit::component::button::Button;
use gpui_kit::component::{ActiveTheme as _, Icon, Sizable as _, h_flex, v_flex};
use gpui_kit::prelude::FluentBuilder as _;
use gpui_kit::*;

use crate::app;
use crate::assets::AppIcon;
use crate::runtime;
use crate::theme::console;
use crate::ui::{self, PillTone};

pub struct ConnectionsPage {
    connection: Arc<Connection>,
    connectors: Vec<ConnectorStatus>,
    providers: Vec<ConnectorProvider>,
    error: Option<SharedString>,
    /// Waiting for the user to finish in the browser.
    awaiting_browser: bool,
    _subscriptions: Vec<Subscription>,
}

impl ConnectionsPage {
    pub fn new(window: &mut Window, cx: &mut Context<Self>) -> Self {
        let activation = cx.observe_window_activation(window, |this, window, cx| {
            if window.is_window_active() && this.awaiting_browser {
                this.awaiting_browser = false;
                this.load(cx);
            }
        });
        let mut this = Self {
            connection: app::connection(cx),
            connectors: Vec::new(),
            providers: Vec::new(),
            error: None,
            awaiting_browser: false,
            _subscriptions: vec![activation],
        };
        this.load(cx);
        this
    }

    fn load(&mut self, cx: &mut Context<Self>) {
        let connection = self.connection.clone();
        cx.spawn(async move |this, cx| {
            let loaded = runtime::spawn(async move {
                let (connectors, providers) = futures::join!(connection.connectors(), connection.connector_providers());
                Ok((connectors?, providers?))
            })
            .await;
            let _ = this.update(cx, |this, cx| {
                match loaded {
                    Ok((connectors, providers)) => {
                        this.connectors = connectors;
                        this.providers = providers;
                        this.error = None;
                    }
                    Err(err) => this.error = Some(err.to_string().into()),
                }
                cx.notify();
            });
        })
        .detach();
    }

    fn connect(&mut self, key: String, window: &mut Window, cx: &mut Context<Self>) {
        let connection = self.connection.clone();
        self.error = None;
        cx.spawn_in(window, async move |this, cx| {
            let begun = runtime::spawn(async move { connection.begin_connect(&key).await }).await;
            let _ = this.update_in(cx, |this, window, cx| {
                match begun {
                    Ok(begin) => {
                        this.awaiting_browser = true;
                        app::open_link(&begin.authorize_url, window, cx);
                    }
                    Err(err) => this.error = Some(err.to_string().into()),
                }
                cx.notify();
            });
        })
        .detach();
    }

    fn revoke(&mut self, id: String, cx: &mut Context<Self>) {
        let connection = self.connection.clone();
        self.error = None;
        cx.spawn(async move |this, cx| {
            let revoked = runtime::spawn(async move { connection.revoke_connector(&id).await }).await;
            let _ = this.update(cx, |this, cx| {
                if let Err(err) = revoked {
                    this.error = Some(err.to_string().into());
                }
                this.load(cx);
            });
        })
        .detach();
    }
}

fn provider_label(provider: i64) -> String {
    match provider {
        1 => "Google Drive".into(),
        2 => "OneDrive".into(),
        other => other.to_string(),
    }
}

impl Render for ConnectionsPage {
    fn render(&mut self, _: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let theme = cx.theme().clone();
        let section = |title: &'static str| {
            div()
                .text_sm()
                .font_weight(FontWeight::SEMIBOLD)
                .text_color(theme.muted_foreground)
                .child(title)
        };
        ui::page("connections-page", ui::PAGE_WIDTH)
            .child(ui::view_header(AppIcon::Connections, "Connections", cx))
            .when_some(self.error.clone(), |this, error| this.child(ui::error_banner(error, None, cx)))
            .child(
                v_flex()
                    .gap_2()
                    .child(section("Connected accounts"))
                    .when(self.connectors.is_empty() && self.error.is_none(), |this| {
                        this.child(div().text_sm().text_color(console(cx).text_faint).child("No connected accounts."))
                    })
                    .children(self.connectors.iter().map(|connector| {
                        let id = connector.connector_id.clone();
                        let tone = match connector.status.as_str() {
                            "connected" | "active" => PillTone::Ok,
                            "reconnect_needed" => PillTone::Danger,
                            _ => PillTone::Neutral,
                        };
                        h_flex()
                            .gap_3()
                            .px_4()
                            .py_3()
                            .bg(theme.popover)
                            .border_1()
                            .border_color(theme.border)
                            .rounded(theme.radius)
                            .child(Icon::new(AppIcon::Drive))
                            .child(div().flex_1().text_sm().child(provider_label(connector.provider)))
                            .child(ui::pill(connector.status.clone(), tone, cx))
                            .map(|this| {
                                if connector.managed {
                                    this.child(
                                        div()
                                            .text_xs()
                                            .text_color(theme.muted_foreground)
                                            .child("Managed by your organization"),
                                    )
                                } else {
                                    this.child(
                                        Button::new(SharedString::from(format!("revoke-{id}")))
                                            .outline()
                                            .small()
                                            .icon(Icon::new(AppIcon::Trash))
                                            .label("Revoke")
                                            .text_color(theme.danger)
                                            .on_click(cx.listener(move |this, _, _, cx| this.revoke(id.clone(), cx))),
                                    )
                                }
                            })
                    })),
            )
            .when(!self.providers.is_empty(), |this| {
                this.child(
                    v_flex()
                        .gap_2()
                        .child(section("Add connection"))
                        .child(
                            h_flex()
                                .gap_2()
                                .flex_wrap()
                                .children(self.providers.iter().map(|provider| {
                                    let key = provider.key.clone();
                                    Button::new(SharedString::from(format!("connect-{}", provider.key)))
                                        .outline()
                                        .icon(Icon::new(AppIcon::Drive))
                                        .label(format!("Connect {}", provider.display_name))
                                        .on_click(cx.listener(move |this, _, window, cx| this.connect(key.clone(), window, cx)))
                                })),
                        )
                        .when(self.awaiting_browser, |this| {
                            this.child(
                                div()
                                    .text_xs()
                                    .text_color(theme.muted_foreground)
                                    .child("Finish connecting in your browser, then come back here. If the browser asks you to sign in to aikonOS first, do that too."),
                            )
                        }),
                )
            })
    }
}
