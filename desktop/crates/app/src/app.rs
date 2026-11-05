//! The window's root: sign-in until a connection exists, then the console.
//!
//! Signing out, and a session the identity provider ended, both come back
//! here: the console and every page in it are dropped with their in-flight
//! requests, and sign-in opens again with the server filled in.

use std::sync::Arc;

use aikonos_client::version::{self, ReleaseStatus};
use aikonos_client::{ApiError, Connection};
use gpui_kit::component::WindowExt as _;
use gpui_kit::component::notification::Notification;
use gpui_kit::component::{ActiveTheme as _, TitleBar, v_flex};
use gpui_kit::prelude::FluentBuilder as _;
use gpui_kit::*;

use crate::prefs::Prefs;
use crate::runtime;
use crate::shell::Shell;
use crate::signin::{SignIn, SignInEvent};
use crate::theme::{self, Appearance};

/// The signed-in connection and what the server said about this build.
pub struct AppState {
    pub connection: Arc<Connection>,
    pub release: ReleaseStatus,
}

impl Global for AppState {}

/// The connection every page talks through. Only valid while signed in.
pub fn connection(cx: &App) -> Arc<Connection> {
    cx.global::<AppState>().connection.clone()
}

struct WorkspaceHandle(WeakEntity<Workspace>);

impl Global for WorkspaceHandle {}

enum Screen {
    SignIn(Entity<SignIn>),
    Console(Entity<Shell>),
}

pub struct Workspace {
    screen: Screen,
    _subscriptions: Vec<Subscription>,
}

impl Workspace {
    pub fn new(window: &mut Window, cx: &mut Context<Self>) -> Self {
        cx.set_global(WorkspaceHandle(cx.weak_entity()));
        let appearance = cx.observe_window_appearance(window, |_, window, cx| {
            if Prefs::global(cx).appearance == Appearance::System {
                theme::apply(Appearance::System, Some(window), cx);
            }
        });
        let mut this = Self {
            screen: Screen::SignIn(cx.new(|cx| SignIn::new(None, window, cx))),
            _subscriptions: vec![appearance],
        };
        this.watch_sign_in(window, cx);
        this
    }

    fn watch_sign_in(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        if let Screen::SignIn(sign_in) = &self.screen {
            let subscription = cx.subscribe_in(sign_in, window, |this, _, event, window, cx| match event {
                SignInEvent::SignedIn(connection) => this.open_console(connection.clone(), window, cx),
            });
            self._subscriptions.truncate(1);
            self._subscriptions.push(subscription);
        }
    }

    fn open_console(&mut self, connection: Arc<Connection>, window: &mut Window, cx: &mut Context<Self>) {
        let release = version::release_status(connection.server().release.as_ref());
        cx.set_global(AppState { connection, release });
        self.screen = Screen::Console(cx.new(|cx| Shell::new(window, cx)));
        self._subscriptions.truncate(1);
        cx.notify();
    }

    /// Leave the console. `notice` explains why on the sign-in screen.
    fn close_console(
        &mut self,
        notice: Option<SharedString>,
        end_session: bool,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        if let Some(state) = cx.try_global::<AppState>() {
            let connection = state.connection.clone();
            if end_session {
                runtime::detach(async move { connection.sign_out().await });
            }
        }
        cx.remove_global::<AppState>();
        self.screen = Screen::SignIn(cx.new(|cx| SignIn::new(notice, window, cx)));
        self.watch_sign_in(window, cx);
        cx.notify();
    }
}

impl Render for Workspace {
    fn render(&mut self, _: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let theme = cx.theme();
        // The app draws its own title bar so it follows the light or dark
        // theme; the system one follows Windows' setting instead.
        let title_bar = TitleBar::new()
            .bg(theme.title_bar)
            .child(div().text_xs().text_color(theme.muted_foreground).child("Aikonos"));
        v_flex()
            .size_full()
            .bg(theme.background)
            .text_color(theme.foreground)
            .font_family(theme.font_family.clone())
            .child(title_bar)
            .child(div().flex_1().min_h_0().map(|this| match &self.screen {
                Screen::SignIn(view) => this.child(view.clone()),
                Screen::Console(view) => this.child(view.clone()),
            }))
    }
}

fn with_workspace(
    window: &mut Window,
    cx: &mut App,
    f: impl FnOnce(&mut Workspace, &mut Window, &mut Context<Workspace>) + 'static,
) {
    let Some(handle) = cx.try_global::<WorkspaceHandle>().map(|h| h.0.clone()) else {
        return;
    };
    // Deferred: the caller is usually inside a page the switch will drop.
    window.defer(cx, move |window, cx| {
        let _ = handle.update(cx, |workspace, cx| f(workspace, window, cx));
    });
}

/// Sign out: end the identity-provider session and return to sign-in.
pub fn sign_out(window: &mut Window, cx: &mut App) {
    with_workspace(window, cx, |workspace, window, cx| {
        workspace.close_console(None, true, window, cx)
    });
}

/// The bearer is dead and could not be renewed.
pub fn session_expired(window: &mut Window, cx: &mut App) {
    with_workspace(window, cx, |workspace, window, cx| {
        workspace.close_console(
            Some("Your session has ended. Sign in again to continue.".into()),
            false,
            window,
            cx,
        )
    });
}

/// Surface a failed request: a dead session returns to sign-in, anything
/// else becomes a notification.
pub fn report(err: &ApiError, window: &mut Window, cx: &mut App) {
    if err.is_unauthorized() {
        session_expired(window, cx);
    } else {
        window.push_notification(Notification::error(err.to_string()), cx);
    }
}

/// A confirmation in the console's toast style.
pub fn toast_ok(message: impl Into<SharedString>, window: &mut Window, cx: &mut App) {
    window.push_notification(Notification::success(message.into()), cx);
}

pub fn toast_error(message: impl Into<SharedString>, window: &mut Window, cx: &mut App) {
    window.push_notification(Notification::error(message.into()), cx);
}

/// Open a web address in the default browser.
///
/// Windows hands whatever `open_url` gets to ShellExecute, which starts a
/// program or a protocol handler as readily as a browser. The addresses this
/// app opens come from the server and from replies, so only absolute http(s)
/// links go through.
pub fn open_link(url: &str, window: &mut Window, cx: &mut App) {
    if is_web_link(url) {
        cx.open_url(url);
    } else {
        toast_error("That link is not a web address, so Aikonos won't open it.", window, cx);
    }
}

fn is_web_link(url: &str) -> bool {
    url::Url::parse(url).is_ok_and(|url| matches!(url.scheme(), "http" | "https") && url.host().is_some())
}

#[cfg(test)]
mod tests {
    use super::is_web_link;

    #[test]
    fn only_web_links_open() {
        assert!(is_web_link("https://downloads.example.org/aikonos.msi"));
        assert!(is_web_link("http://localhost:4200/connect/callback"));
        assert!(!is_web_link("file:///C:/Windows/System32/calc.exe"));
        assert!(!is_web_link("ms-settings:privacy"));
        assert!(!is_web_link("javascript:alert(1)"));
        assert!(!is_web_link("report.md"));
        assert!(!is_web_link(r"\\fileserver\share\setup.exe"));
        assert!(!is_web_link("https://"));
    }
}
