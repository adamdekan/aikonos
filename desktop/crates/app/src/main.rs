// A release build is a GUI program: no console window behind it.
#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

//! aikonOS for Windows: the member console as a native app.
//!
//! It signs in to the organisation's aikonOS server and offers what a
//! member sees in the web console (chat, files, connections, schedules,
//! workflows, skills, inbox), drawn natively with GPUI Kit. Local files
//! move only when the user moves them; see `local_files`.

mod app;
mod assets;
mod chat;
mod local_files;
mod pages;
mod prefs;
mod runtime;
mod sessions;
mod settings;
mod shell;
mod signin;
mod theme;
mod ui;

use gpui_kit::component::notification::NotificationSettings;
use gpui_kit::component::{Theme, TitleBar};
use gpui_kit::*;

actions!(aikonos, [Quit]);

fn main() {
    runtime::init();
    let prefs = prefs::Prefs::load();
    gpui_kit::application().with_assets(assets::AppAssets).run(move |cx| {
        gpui_kit::init(cx);
        assets::load_fonts(cx);
        let appearance = prefs.appearance;
        cx.set_global(prefs);
        theme::init(appearance, cx);
        // The web console stacks its toasts bottom-right.
        Theme::global_mut(cx).notification = NotificationSettings {
            placement: Anchor::BottomRight,
            margins: gpui_kit::base::Edges::all(px(24.)),
            width: px(380.),
            ..NotificationSettings::default()
        };
        chat::bind_keys(cx);
        cx.bind_keys([KeyBinding::new("ctrl-q", Quit, None)]);
        cx.on_action(|_: &Quit, cx| cx.quit());

        let options = WindowOptions {
            window_bounds: Some(WindowBounds::centered(size(px(1280.), px(820.)), cx)),
            window_min_size: Some(size(px(900.), px(600.))),
            app_id: Some("com.aikonos.desktop".into()),
            // The title bar is drawn by the app; this names the window to
            // Windows itself: Alt+Tab, the taskbar preview.
            titlebar: Some(TitlebarOptions {
                title: Some("aikonOS".into()),
                ..TitleBar::title_bar_options()
            }),
            ..TitleBar::window_options()
        };
        gpui_kit::open_window(options, cx, |window, cx| cx.new(|cx| app::Workspace::new(window, cx)))
            .expect("the main window opens");
        cx.activate(true);
    });
}
