//! Signing in: the server's address, then the identity provider in the
//! system browser. The web console never shows this screen (the browser is
//! already on the server); a desktop app has to ask where to connect.

use std::sync::Arc;

use aikonos_client::version::{self, ReleaseStatus};
use aikonos_client::{ApiError, Connection, SignInStart};
use gpui_kit::component::button::{Button, ButtonVariants as _};
use gpui_kit::component::input::{Input, InputEvent, InputState};
use gpui_kit::component::{ActiveTheme as _, Disableable as _, Icon, Sizable as _, h_flex, v_flex};
use gpui_kit::prelude::FluentBuilder as _;
use gpui_kit::*;

use crate::app;
use crate::assets::AppIcon;
use crate::prefs::Prefs;
use crate::runtime;
use crate::theme::console;

pub enum SignInEvent {
    SignedIn(Arc<Connection>),
}

enum Phase {
    Idle,
    /// Reading the server's settings and its identity provider.
    Locating,
    /// The browser is open on the identity provider.
    InBrowser {
        authorize_url: SharedString,
        insecure: bool,
    },
    /// The server needs a newer build of this app.
    UpdateRequired {
        minimum: SharedString,
        url: Option<SharedString>,
        notes: Option<SharedString>,
    },
}

pub struct SignIn {
    server: Entity<InputState>,
    phase: Phase,
    error: Option<SharedString>,
    notice: Option<SharedString>,
    task: Option<Task<()>>,
    _subscriptions: Vec<Subscription>,
}

impl EventEmitter<SignInEvent> for SignIn {}

impl SignIn {
    pub fn new(notice: Option<SharedString>, window: &mut Window, cx: &mut Context<Self>) -> Self {
        // `--server <address>` (an IT-deployed shortcut) wins over the last
        // server used, and signs in straight away.
        let preset = notice.is_none().then(server_argument).flatten();
        let last = preset.clone().unwrap_or_else(|| Prefs::global(cx).server.clone());
        let server = cx.new(|cx| {
            InputState::new(window, cx)
                .placeholder("aikonos.example.org")
                .default_value(last)
        });
        let subscription = cx.subscribe_in(&server, window, |this, _, event, window, cx| {
            if matches!(event, InputEvent::PressEnter { .. }) {
                this.start(window, cx);
            }
        });
        server.update(cx, |state, cx| state.focus(window, cx));
        let mut this = Self {
            server,
            phase: Phase::Idle,
            error: None,
            notice,
            task: None,
            _subscriptions: vec![subscription],
        };
        if preset.is_some() {
            this.start(window, cx);
        }
        this
    }

    fn start(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        if !matches!(self.phase, Phase::Idle) {
            return;
        }
        let input = self.server.read(cx).value().to_string();
        self.error = None;
        self.notice = None;
        self.phase = Phase::Locating;
        cx.notify();
        self.task = Some(cx.spawn_in(window, async move |this, cx| {
            let started = runtime::spawn(async move { SignInStart::begin(&input).await }).await;
            let start = match started {
                Ok(start) => start,
                Err(err) => {
                    let _ = this.update(cx, |this, cx| this.fail(err, cx));
                    return;
                }
            };
            if let ReleaseStatus::UpdateRequired { minimum, url, notes } =
                version::release_status(start.server().release.as_ref())
            {
                let _ = this.update(cx, |this, cx| {
                    this.phase = Phase::UpdateRequired {
                        minimum: minimum.into(),
                        url: url.map(Into::into),
                        notes: notes.map(Into::into),
                    };
                    this.task = None;
                    cx.notify();
                });
                return;
            }
            let authorize_url: SharedString = start.authorize_url().to_string().into();
            let insecure = start.server().is_insecure();
            let origin = start.server().origin.to_string();
            let opened = this.update_in(cx, |this, window, cx| {
                this.phase = Phase::InBrowser {
                    authorize_url: authorize_url.clone(),
                    insecure,
                };
                open_sign_in_page(&authorize_url, window, cx);
                cx.notify();
            });
            if opened.is_err() {
                return;
            }
            let finished = runtime::spawn(async move { start.finish().await }).await;
            let _ = this.update_in(cx, |this, window, cx| match finished {
                Ok(connection) => {
                    Prefs::update(cx, |prefs| prefs.server = origin);
                    this.phase = Phase::Idle;
                    this.task = None;
                    cx.emit(SignInEvent::SignedIn(connection));
                    // Bring the app forward over the browser tab.
                    window.activate_window();
                }
                Err(err) => this.fail(err, cx),
            });
        }));
    }

    fn fail(&mut self, err: ApiError, cx: &mut Context<Self>) {
        self.phase = Phase::Idle;
        self.task = None;
        self.error = Some(err.to_string().into());
        cx.notify();
    }

    fn cancel(&mut self, cx: &mut Context<Self>) {
        // Dropping the task aborts the request and closes the listener.
        self.task = None;
        self.phase = Phase::Idle;
        cx.notify();
    }

    fn render_form(&self, cx: &mut Context<Self>) -> impl IntoElement {
        let locating = matches!(self.phase, Phase::Locating);
        v_flex()
            .gap_3()
            .child(
                v_flex()
                    .gap_1()
                    .child(
                        div()
                            .text_size(rems(0.8))
                            .font_weight(FontWeight::MEDIUM)
                            .text_color(cx.theme().muted_foreground)
                            .child("Server"),
                    )
                    .child(Input::new(&self.server).disabled(locating)),
            )
            .child(
                Button::new("sign-in")
                    .primary()
                    .w_full()
                    .label(if locating { "Connecting…" } else { "Sign in" })
                    .loading(locating)
                    .disabled(locating)
                    .on_click(cx.listener(|this, _, window, cx| this.start(window, cx))),
            )
    }

    fn render_in_browser(
        &self,
        authorize_url: &SharedString,
        insecure: bool,
        cx: &mut Context<Self>,
    ) -> impl IntoElement {
        let url = authorize_url.clone();
        v_flex()
            .gap_3()
            .child(
                div()
                    .text_sm()
                    .text_color(cx.theme().foreground)
                    .child("Finish signing in in your browser. This window continues on its own."),
            )
            .when(insecure, |this| {
                this.child(
                    div()
                        .text_xs()
                        .text_color(cx.theme().danger)
                        .child("This server does not use HTTPS, so your sign-in travels unencrypted. Ask your administrator to enable HTTPS."),
                )
            })
            .child(
                h_flex()
                    .gap_2()
                    .child(
                        Button::new("reopen")
                            .outline()
                            .label("Open the browser again")
                            .on_click(move |_, window, cx| app::open_link(&url, window, cx)),
                    )
                    .child(
                        Button::new("cancel")
                            .ghost()
                            .label("Cancel")
                            .on_click(cx.listener(|this, _, _, cx| this.cancel(cx))),
                    ),
            )
    }

    fn render_update_required(
        &self,
        minimum: &SharedString,
        url: Option<&SharedString>,
        notes: Option<SharedString>,
        cx: &mut Context<Self>,
    ) -> impl IntoElement {
        v_flex()
            .gap_3()
            .child(div().text_sm().child(format!(
                "This server needs aikonOS for Windows {minimum} or later. You have {}.",
                version::CURRENT
            )))
            .when_some(notes, |this, notes| {
                this.child(div().text_xs().text_color(cx.theme().muted_foreground).child(notes))
            })
            .child(
                h_flex()
                    .gap_2()
                    .when_some(url.cloned(), |this, url| {
                        this.child(
                            Button::new("download")
                                .primary()
                                .label("Download the update")
                                .on_click(move |_, window, cx| app::open_link(&url, window, cx)),
                        )
                    })
                    .child(
                        Button::new("back")
                            .ghost()
                            .label("Use another server")
                            .on_click(cx.listener(|this, _, _, cx| this.cancel(cx))),
                    ),
            )
    }
}

impl Render for SignIn {
    fn render(&mut self, _: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let body = match &self.phase {
            Phase::Idle | Phase::Locating => self.render_form(cx).into_any_element(),
            Phase::InBrowser {
                authorize_url,
                insecure,
            } => {
                let (url, insecure) = (authorize_url.clone(), *insecure);
                self.render_in_browser(&url, insecure, cx).into_any_element()
            }
            Phase::UpdateRequired { minimum, url, notes } => {
                let (minimum, url, notes) = (minimum.clone(), url.clone(), notes.clone());
                self.render_update_required(&minimum, url.as_ref(), notes, cx)
                    .into_any_element()
            }
        };
        v_flex()
            .size_full()
            .items_center()
            .justify_center()
            .bg(cx.theme().background)
            .child(
                v_flex()
                    .w(rems(24.))
                    .gap_5()
                    .p_8()
                    .rounded(cx.theme().radius_lg)
                    .border_1()
                    .border_color(cx.theme().border)
                    .bg(cx.theme().popover)
                    .child(
                        v_flex()
                            .gap_1()
                            .child(
                                div()
                                    .font_family("Space Grotesk")
                                    .font_weight(FontWeight::BOLD)
                                    .text_2xl()
                                    .child("aikonOS"),
                            )
                            .child(
                                div()
                                    .text_sm()
                                    .text_color(cx.theme().muted_foreground)
                                    .child("Sign in to your organisation's aikonOS server."),
                            ),
                    )
                    .when_some(self.notice.clone(), |this, notice| {
                        this.child(
                            h_flex()
                                .gap_2()
                                .text_sm()
                                .text_color(cx.theme().muted_foreground)
                                .child(Icon::new(AppIcon::Schedules).small())
                                .child(notice),
                        )
                    })
                    .child(body)
                    .when_some(self.error.clone(), |this, error| {
                        this.child(div().text_sm().text_color(cx.theme().danger).child(error))
                    }),
            )
            .child(
                div()
                    .mt_4()
                    .text_xs()
                    .text_color(console(cx).text_faint)
                    .child(format!("aikonOS for Windows {}", version::CURRENT)),
            )
    }
}

/// Open the identity provider's sign-in page in the system browser.
///
/// Debug builds started with `AIKONOS_DEV_HEADLESS_SIGNIN=1` fetch the page
/// instead and follow its redirect to the loopback listener, which signs in
/// against desktop/dev/mock-server.mjs (started with `MOCK_AUTO_SIGNIN=1`)
/// without a browser window. Release builds never do.
fn open_sign_in_page(url: &str, window: &mut Window, cx: &mut App) {
    #[cfg(debug_assertions)]
    if std::env::var_os("AIKONOS_DEV_HEADLESS_SIGNIN").is_some() {
        let url = url.to_owned();
        runtime::detach(async move {
            let _ = aikonos_client::connection::http_client().get(url).send().await;
        });
        return;
    }
    app::open_link(url, window, cx);
}

/// The address given as `--server <address>` or `--server=<address>`.
fn server_argument() -> Option<String> {
    let mut args = std::env::args().skip(1);
    while let Some(arg) = args.next() {
        if arg == "--server" {
            return args.next();
        }
        if let Some(value) = arg.strip_prefix("--server=") {
            return Some(value.to_owned());
        }
    }
    None
}
