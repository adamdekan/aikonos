//! Choosing the OneDrive folder the agent works in
//! (webui WorkspaceFolderPicker.vue).

use std::rc::Rc;
use std::sync::Arc;

use aikonos_client::Connection;
use aikonos_client::api::workspace::OneDriveFolder;
use gpui_kit::component::WindowExt as _;
use gpui_kit::component::button::{Button, ButtonVariants as _};
use gpui_kit::component::dialog::DialogFooter;
use gpui_kit::component::{ActiveTheme as _, Icon, Sizable as _, h_flex, v_flex};
use gpui_kit::prelude::FluentBuilder as _;
use gpui_kit::*;

use crate::app;
use crate::assets::AppIcon;
use crate::runtime;
use crate::ui;

type OnSelect = Rc<dyn Fn(String, &mut Window, &mut App)>;

pub struct FolderPicker {
    connection: Arc<Connection>,
    dir: String,
    folders: Vec<OneDriveFolder>,
    loading: bool,
    error: Option<SharedString>,
    task: Option<Task<()>>,
}

impl FolderPicker {
    fn new(connection: Arc<Connection>, cx: &mut Context<Self>) -> Self {
        let mut this = Self {
            connection,
            dir: String::new(),
            folders: Vec::new(),
            loading: false,
            error: None,
            task: None,
        };
        this.load(String::new(), cx);
        this
    }

    fn load(&mut self, dir: String, cx: &mut Context<Self>) {
        self.dir = dir.clone();
        self.loading = true;
        self.error = None;
        self.folders.clear();
        let connection = self.connection.clone();
        self.task = Some(cx.spawn(async move |this, cx| {
            let result = runtime::spawn(async move { connection.onedrive_folders(&dir).await }).await;
            let _ = this.update(cx, |this, cx| {
                this.loading = false;
                match result {
                    Ok(folders) => this.folders = folders,
                    Err(err) => this.error = Some(err.to_string().into()),
                }
                cx.notify();
            });
        }));
        cx.notify();
    }

    fn crumbs(&self) -> Vec<(SharedString, String)> {
        let mut crumbs = vec![(SharedString::from("OneDrive"), String::new())];
        let mut path = String::new();
        for segment in self.dir.split('/').filter(|s| !s.is_empty()) {
            if !path.is_empty() {
                path.push('/');
            }
            path.push_str(segment);
            crumbs.push((segment.to_owned().into(), path.clone()));
        }
        crumbs
    }
}

impl Render for FolderPicker {
    fn render(&mut self, _: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let theme = cx.theme();
        let crumbs = self.crumbs();
        let last = crumbs.len() - 1;
        v_flex()
            .gap_3()
            .child(
                h_flex()
                    .flex_wrap()
                    .gap_1()
                    .text_sm()
                    .children(crumbs.into_iter().enumerate().map(|(ix, (label, path))| {
                        h_flex()
                            .gap_1()
                            .when(ix > 0, |this| {
                                this.child(div().text_color(theme.muted_foreground).child("/"))
                            })
                            .child(
                                Button::new(("crumb", ix))
                                    .ghost()
                                    .xsmall()
                                    .label(label)
                                    .when(ix == last, |this| this.text_color(theme.foreground))
                                    .on_click(cx.listener(move |this, _, _, cx| this.load(path.clone(), cx))),
                            )
                    })),
            )
            .child(
                v_flex()
                    .id("folders")
                    .h(rems(15.))
                    .overflow_y_scroll()
                    .border_1()
                    .border_color(theme.border)
                    .rounded(theme.radius)
                    .p_1()
                    .when(self.loading, |this| {
                        this.child(ui::empty_state(None, "Loading folders…", cx))
                    })
                    .when_some(self.error.clone(), |this, error| {
                        this.child(ui::error_banner(
                            error,
                            Some(
                                Button::new("retry")
                                    .ghost()
                                    .xsmall()
                                    .label("Retry")
                                    .on_click(cx.listener(|this, _, _, cx| this.load(this.dir.clone(), cx)))
                                    .into_any_element(),
                            ),
                            cx,
                        ))
                    })
                    .when(
                        !self.loading && self.error.is_none() && self.folders.is_empty(),
                        |this| this.child(ui::empty_state(None, "No sub-folders here.", cx)),
                    )
                    .children(self.folders.iter().map(|folder| {
                        let path = folder.path.clone();
                        ui::row_button(
                            SharedString::from(format!("folder-{}", folder.path)),
                            folder.name.clone(),
                            Some(AppIcon::Files),
                        )
                        .on_click(cx.listener(move |this, _, _, cx| this.load(path.clone(), cx)))
                    })),
            )
    }
}

/// Open the picker; `on_select` receives the chosen folder ("" for the
/// drive root).
pub fn pick_folder(window: &mut Window, cx: &mut App, on_select: impl Fn(String, &mut Window, &mut App) + 'static) {
    let connection = app::connection(cx);
    let picker = cx.new(|cx| FolderPicker::new(connection, cx));
    let on_select: OnSelect = Rc::new(on_select);
    window.open_dialog(cx, move |dialog, _, cx| {
        let picker_for_ok = picker.clone();
        let on_select = on_select.clone();
        dialog
            .title(
                h_flex()
                    .gap_2()
                    .child(Icon::new(AppIcon::Drive).small())
                    .child("Choose a OneDrive folder"),
            )
            .w(px(520.))
            .bg(cx.theme().popover)
            .child(picker.clone())
            .footer(
                DialogFooter::new()
                    .gap_2()
                    .child(
                        Button::new("cancel")
                            .ghost()
                            .label("Cancel")
                            .on_click(|_, window, cx| window.close_dialog(cx)),
                    )
                    .child(Button::new("use-folder").primary().label("Use this folder").on_click(
                        move |_, window, cx| {
                            let dir = picker_for_ok.read(cx).dir.clone();
                            window.close_dialog(cx);
                            on_select(dir, window, cx);
                        },
                    )),
            )
    });
}
