//! The user's workspace files (webui views/Files.vue), plus the desktop's
//! two-way bridge to this PC: upload any number of local files by picker or
//! by dropping them from Explorer, and save a workspace file to a folder the
//! user chooses.

use std::path::PathBuf;
use std::sync::Arc;

use aikonos_client::Connection;
use aikonos_client::api::files::{ListFiles, WorkspaceFile};
use aikonos_client::api::workspace::WorkspaceBackend;
use gpui_kit::component::WindowExt as _;
use gpui_kit::component::button::{Button, ButtonVariant, ButtonVariants as _};
use gpui_kit::component::input::{Input, InputEvent, InputState};
use gpui_kit::component::{ActiveTheme as _, Icon, Sizable as _, h_flex, v_flex};
use gpui_kit::prelude::FluentBuilder as _;
use gpui_kit::*;

use crate::app;
use crate::assets::AppIcon;
use crate::local_files;
use crate::runtime;
use crate::theme::console;
use crate::ui;

pub struct FilesPage {
    connection: Arc<Connection>,
    cwd: String,
    entries: Vec<WorkspaceFile>,
    loading: bool,
    error: Option<SharedString>,
    filter: Entity<InputState>,
    new_folder: Option<Entity<InputState>>,
    renaming: Option<(String, Entity<InputState>)>,
    uploading: usize,
    backend: Option<WorkspaceBackend>,
    generation: u64,
    _subscriptions: Vec<Subscription>,
}

impl FilesPage {
    pub fn new(window: &mut Window, cx: &mut Context<Self>) -> Self {
        let connection = app::connection(cx);
        let filter = cx.new(|cx| InputState::new(window, cx).placeholder("filter this folder…"));
        let filter_changed = cx.subscribe(&filter, |_, _, event: &InputEvent, cx| {
            if matches!(event, InputEvent::Change) {
                cx.notify();
            }
        });
        let mut this = Self {
            connection,
            cwd: String::new(),
            entries: Vec::new(),
            loading: true,
            error: None,
            filter,
            new_folder: None,
            renaming: None,
            uploading: 0,
            backend: None,
            generation: 0,
            _subscriptions: vec![filter_changed],
        };
        this.load(cx);
        this.load_backend(cx);
        this
    }

    /// List the current folder's immediate entries. The root is "." because
    /// an empty `dir` asks the gateway for the legacy recursive listing.
    fn load(&mut self, cx: &mut Context<Self>) {
        self.generation += 1;
        let generation = self.generation;
        let dir = if self.cwd.is_empty() {
            ".".to_owned()
        } else {
            self.cwd.clone()
        };
        let connection = self.connection.clone();
        cx.spawn(async move |this, cx| {
            let listed = runtime::spawn(async move {
                connection
                    .list_files(ListFiles {
                        dir: &dir,
                        ..Default::default()
                    })
                    .await
            })
            .await;
            let _ = this.update(cx, |this, cx| {
                if this.generation != generation {
                    return;
                }
                this.loading = false;
                match listed {
                    Ok(entries) => this.entries = entries,
                    Err(err) => this.error = Some(err.to_string().into()),
                }
                cx.notify();
            });
        })
        .detach();
    }

    fn load_backend(&mut self, cx: &mut Context<Self>) {
        let connection = self.connection.clone();
        cx.spawn(async move |this, cx| {
            let backend = runtime::spawn(async move { connection.workspace_backend().await }).await;
            let _ = this.update(cx, |this, cx| {
                this.backend = backend.ok();
                cx.notify();
            });
        })
        .detach();
    }

    fn open_folder(&mut self, path: String, window: &mut Window, cx: &mut Context<Self>) {
        self.cwd = path;
        self.error = None;
        self.renaming = None;
        self.filter.update(cx, |filter, cx| filter.clean(window, cx));
        self.load(cx);
        cx.notify();
    }

    fn join(&self, name: &str) -> String {
        if self.cwd.is_empty() {
            name.to_owned()
        } else {
            format!("{}/{name}", self.cwd)
        }
    }

    fn visible_entries(&self, cx: &App) -> Vec<WorkspaceFile> {
        let filter = self.filter.read(cx).value().trim().to_lowercase();
        let mut entries: Vec<WorkspaceFile> = self
            .entries
            .iter()
            // Belt and braces: never show dot-paths even if one is returned.
            .filter(|file| !file.path.split('/').any(|segment| segment.starts_with('.')))
            .filter(|file| filter.is_empty() || file.name().to_lowercase().contains(&filter))
            .cloned()
            .collect();
        entries.sort_by(|a, b| b.is_dir.cmp(&a.is_dir).then_with(|| a.path.cmp(&b.path)));
        entries
    }

    // ── this PC → workspace ───────────────────────────────────────────────

    fn pick_and_upload(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        cx.spawn_in(window, async move |this, cx| {
            let paths = local_files::pick_files("Upload", cx).await;
            if !paths.is_empty() {
                let _ = this.update(cx, |this, cx| this.upload_paths(paths, None, cx));
            }
        })
        .detach();
    }

    /// Upload local files into `target_dir` (default: the folder shown),
    /// then refresh once (Files.vue `uploadFiles`).
    fn upload_paths(&mut self, paths: Vec<PathBuf>, target_dir: Option<String>, cx: &mut Context<Self>) {
        let dir = target_dir.unwrap_or_else(|| self.cwd.clone());
        self.uploading += paths.len();
        self.error = None;
        cx.notify();
        let connection = self.connection.clone();
        cx.spawn(async move |this, cx| {
            let mut errors = Vec::new();
            for path in paths {
                let read = cx
                    .background_executor()
                    .spawn(async move { local_files::read_for_upload(&path) })
                    .await;
                match read {
                    Ok(file) => {
                        let target = if dir.is_empty() {
                            file.name.clone()
                        } else {
                            format!("{dir}/{}", file.name)
                        };
                        let connection = connection.clone();
                        let written =
                            runtime::spawn(async move { connection.write_file(&target, &file.bytes).await }).await;
                        if let Err(err) = written {
                            errors.push(err.to_string());
                        }
                    }
                    Err(message) => errors.push(message),
                }
                let _ = this.update(cx, |this, cx| {
                    this.uploading = this.uploading.saturating_sub(1);
                    cx.notify();
                });
            }
            let _ = this.update(cx, |this, cx| {
                if !errors.is_empty() {
                    this.error = Some(errors.join(" ").into());
                }
                this.load(cx);
            });
        })
        .detach();
    }

    // ── workspace → this PC ───────────────────────────────────────────────

    fn save_to_pc(&mut self, path: String, window: &mut Window, cx: &mut Context<Self>) {
        let connection = self.connection.clone();
        let name = path.rsplit('/').next().unwrap_or(&path).to_owned();
        cx.spawn_in(window, async move |this, cx| {
            let Some(target) = local_files::pick_save_path(&name, cx).await else {
                return;
            };
            let content = runtime::spawn(async move { connection.read_file(&path).await }).await;
            let saved = match content {
                Ok(content) => {
                    let target = target.clone();
                    cx.background_executor()
                        .spawn(async move { local_files::save_bytes(&target, &content.bytes) })
                        .await
                        .map_err(|err| format!("Couldn't save {name}: {err}"))
                }
                Err(err) => Err(err.to_string()),
            };
            let _ = this.update_in(cx, |_, window, cx| match saved {
                Ok(()) => {
                    app::toast_ok(format!("Saved to {}", target.display()), window, cx);
                    local_files::reveal(&target, cx);
                }
                Err(message) => app::toast_error(message, window, cx),
            });
        })
        .detach();
    }

    // ── folder and file changes ───────────────────────────────────────────

    fn open_new_folder(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let input = cx.new(|cx| InputState::new(window, cx).placeholder("Folder name"));
        let subscription = cx.subscribe_in(&input, window, |this, input, event, _, cx| match event {
            InputEvent::PressEnter { .. } => {
                let name = input.read(cx).value().trim().to_owned();
                this.new_folder = None;
                if !name.is_empty() {
                    this.create_folder(name, cx);
                }
                cx.notify();
            }
            InputEvent::Blur => {
                this.new_folder = None;
                cx.notify();
            }
            _ => {}
        });
        input.update(cx, |input, cx| input.focus(window, cx));
        self._subscriptions.push(subscription);
        self.new_folder = Some(input);
        cx.notify();
    }

    fn create_folder(&mut self, name: String, cx: &mut Context<Self>) {
        let path = self.join(&name);
        let connection = self.connection.clone();
        self.error = None;
        cx.spawn(async move |this, cx| {
            let created = runtime::spawn(async move { connection.create_dir(&path).await }).await;
            let _ = this.update(cx, |this, cx| {
                if let Err(err) = created {
                    this.error = Some(err.to_string().into());
                }
                this.load(cx);
            });
        })
        .detach();
    }

    fn start_rename(&mut self, file: &WorkspaceFile, window: &mut Window, cx: &mut Context<Self>) {
        let path = file.path.clone();
        let input = cx.new(|cx| InputState::new(window, cx).default_value(file.name().to_owned()));
        let subscription = cx.subscribe_in(&input, window, move |this, input, event, _, cx| match event {
            InputEvent::PressEnter { .. } | InputEvent::Blur => {
                let name = input.read(cx).value().trim().to_owned();
                this.confirm_rename(&path, name, cx);
            }
            _ => {}
        });
        input.update(cx, |input, cx| {
            input.focus(window, cx);
            input.select_all(window, cx);
        });
        self._subscriptions.push(subscription);
        self.renaming = Some((file.path.clone(), input));
        cx.notify();
    }

    fn confirm_rename(&mut self, path: &str, name: String, cx: &mut Context<Self>) {
        // Enter then blur both arrive; only the first acts.
        if self.renaming.as_ref().map(|(renaming, _)| renaming.as_str()) != Some(path) {
            return;
        }
        self.renaming = None;
        cx.notify();
        let current = path.rsplit('/').next().unwrap_or(path);
        if name.is_empty() || name == current {
            return;
        }
        let from = path.to_owned();
        let to = self.join(&name);
        let connection = self.connection.clone();
        self.error = None;
        cx.spawn(async move |this, cx| {
            let moved = runtime::spawn(async move { connection.move_file(&from, &to).await }).await;
            let _ = this.update(cx, |this, cx| {
                if let Err(err) = moved {
                    this.error = Some(err.to_string().into());
                }
                this.load(cx);
            });
        })
        .detach();
    }

    fn request_delete(&mut self, file: &WorkspaceFile, window: &mut Window, cx: &mut Context<Self>) {
        let path = file.path.clone();
        let name = file.name().to_owned();
        let page = cx.weak_entity();
        window.open_alert_dialog(cx, move |alert, _, _| {
            let path = path.clone();
            let page = page.clone();
            alert
                .title("Delete")
                .description(format!("Delete {name}? This cannot be undone."))
                .confirm()
                .ok_text("Delete")
                .ok_variant(ButtonVariant::Danger)
                .on_ok(move |_, _, cx| {
                    let _ = page.update(cx, |this, cx| this.delete(path.clone(), cx));
                    true
                })
        });
    }

    fn delete(&mut self, path: String, cx: &mut Context<Self>) {
        let connection = self.connection.clone();
        self.error = None;
        cx.spawn(async move |this, cx| {
            let deleted = runtime::spawn(async move { connection.delete_file(&path).await }).await;
            let _ = this.update(cx, |this, cx| {
                if let Err(err) = deleted {
                    this.error = Some(err.to_string().into());
                }
                this.load(cx);
            });
        })
        .detach();
    }

    // ── rendering ─────────────────────────────────────────────────────────

    fn render_header(&self, cx: &mut Context<Self>) -> impl IntoElement {
        let theme = cx.theme();
        let backend_label = self.backend.as_ref().map(|backend| -> SharedString {
            if backend.pref.is_onedrive() {
                format!("OneDrive · /{}", backend.pref.onedrive_folder_path).into()
            } else {
                "Local workspace".into()
            }
        });
        ui::view_header(AppIcon::Files, "Files", cx)
            .when_some(backend_label, |this, label| {
                this.child(
                    div()
                        .px_2()
                        .py(rems(0.1875))
                        .rounded(theme.radius)
                        .border_1()
                        .border_color(theme.border)
                        .font_family(theme.mono_font_family.clone())
                        .text_xs()
                        .text_color(theme.muted_foreground)
                        .child(label),
                )
            })
            .child(
                Button::new("new-folder")
                    .ghost()
                    .icon(Icon::new(AppIcon::Plus))
                    .label("New folder")
                    .on_click(cx.listener(|this, _, window, cx| this.open_new_folder(window, cx))),
            )
            .child(
                Button::new("upload")
                    .primary()
                    .icon(Icon::new(AppIcon::Upload))
                    .label("Upload")
                    .loading(self.uploading > 0)
                    .tooltip("Upload files from this PC")
                    .on_click(cx.listener(|this, _, window, cx| this.pick_and_upload(window, cx))),
            )
    }

    fn render_breadcrumbs(&self, cx: &mut Context<Self>) -> impl IntoElement {
        let theme = cx.theme();
        let mut crumbs = vec![("Files".to_owned(), String::new())];
        let mut path = String::new();
        for segment in self.cwd.split('/').filter(|s| !s.is_empty()) {
            if !path.is_empty() {
                path.push('/');
            }
            path.push_str(segment);
            crumbs.push((segment.to_owned(), path.clone()));
        }
        h_flex()
            .gap_1p5()
            .mt(rems(-0.75))
            .text_size(rems(0.8125))
            .text_color(theme.muted_foreground)
            .children(crumbs.into_iter().enumerate().map(|(ix, (label, path))| {
                h_flex()
                    .gap_1p5()
                    .when(ix > 0, |this| {
                        this.child(div().text_color(console(cx).text_faint).child("/"))
                    })
                    .child(
                        div()
                            .id(("crumb", ix))
                            .cursor_pointer()
                            .hover(|this| this.text_color(theme.foreground).underline())
                            .on_click(
                                cx.listener(move |this, _, window, cx| this.open_folder(path.clone(), window, cx)),
                            )
                            .child(label),
                    )
            }))
    }

    fn render_row(&self, file: &WorkspaceFile, cx: &mut Context<Self>) -> AnyElement {
        let theme = cx.theme();
        let icon = Icon::new(if file.is_dir { AppIcon::Files } else { AppIcon::File }).size(rems(0.9375));
        let row = h_flex()
            .id(SharedString::from(format!("file-{}", file.path)))
            .gap(rems(0.625))
            .px_4()
            .py(rems(0.625))
            .bg(theme.popover)
            .border_1()
            .border_color(theme.border)
            .rounded(theme.radius);
        if let Some((renaming, input)) = &self.renaming
            && renaming == &file.path
        {
            return row
                .child(icon)
                .child(div().flex_1().child(Input::new(input).small()))
                .into_any_element();
        }
        let open_path = file.path.clone();
        let drop_path = file.path.clone();
        let save_path = file.path.clone();
        let rename_file = file.clone();
        let delete_file = file.clone();
        let is_dir = file.is_dir;
        row.when(is_dir, |this| {
            this.cursor_pointer()
                .on_click(cx.listener(move |this, _, window, cx| this.open_folder(open_path.clone(), window, cx)))
                .drag_over::<ExternalPaths>(|style, _, _, cx| {
                    style.border_color(cx.theme().primary).bg(console(cx).fill_accent)
                })
                .on_drop(cx.listener(move |this, paths: &ExternalPaths, _, cx| {
                    cx.stop_propagation();
                    this.upload_paths(paths.paths().to_vec(), Some(drop_path.clone()), cx);
                }))
        })
        .child(icon.text_color(theme.foreground))
        .child(
            div()
                .flex_1()
                .min_w_0()
                .font_family(theme.mono_font_family.clone())
                .text_size(rems(0.8125))
                .text_color(theme.foreground)
                .text_ellipsis()
                .whitespace_nowrap()
                .when(is_dir, |this| this.hover(|this| this.underline()))
                .child(file.name().to_owned()),
        )
        .when(!is_dir, |this| {
            this.child(
                div()
                    .flex_none()
                    .text_xs()
                    .text_color(theme.muted_foreground)
                    .child(ui::file_size(file.size)),
            )
        })
        .child(
            Button::new(SharedString::from(format!("rename-{}", file.path)))
                .ghost()
                .small()
                .icon(Icon::new(AppIcon::Customize))
                .tooltip(format!("Rename {}", file.name()))
                .on_click(cx.listener(move |this, _, window, cx| {
                    cx.stop_propagation();
                    this.start_rename(&rename_file, window, cx)
                })),
        )
        .when(!is_dir, |this| {
            this.child(
                Button::new(SharedString::from(format!("save-{}", file.path)))
                    .ghost()
                    .small()
                    .icon(Icon::new(AppIcon::Download))
                    .tooltip("Save to this PC")
                    .on_click(cx.listener(move |this, _, window, cx| {
                        cx.stop_propagation();
                        this.save_to_pc(save_path.clone(), window, cx)
                    })),
            )
        })
        .child(
            Button::new(SharedString::from(format!("delete-{}", file.path)))
                .ghost()
                .small()
                .icon(Icon::new(AppIcon::Trash))
                .text_color(theme.danger)
                .tooltip(format!("Delete {}", file.name()))
                .on_click(cx.listener(move |this, _, window, cx| {
                    cx.stop_propagation();
                    this.request_delete(&delete_file, window, cx)
                })),
        )
        .into_any_element()
    }
}

impl Render for FilesPage {
    fn render(&mut self, _: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let theme = cx.theme().clone();
        let entries = self.visible_entries(cx);
        let reconnect = self
            .backend
            .as_ref()
            .is_some_and(|b| b.pref.is_onedrive() && b.onedrive_status == "reconnect_needed");
        ui::page("files-page", ui::PAGE_WIDTH)
            .child(self.render_header(cx))
            .child(self.render_breadcrumbs(cx))
            .child(Input::new(&self.filter).small())
            .when_some(self.new_folder.clone(), |this, input| {
                this.child(Input::new(&input).small())
            })
            .when(reconnect, |this| {
                this.child(ui::error_banner(
                    "OneDrive connection needs to be refreshed — it reconnects automatically on your next sign-in.",
                    None,
                    cx,
                ))
            })
            .when_some(self.error.clone(), |this, error| {
                this.child(ui::error_banner(error, None, cx))
            })
            .child(
                v_flex()
                    .id("dropzone")
                    .gap(rems(0.375))
                    .rounded(theme.radius)
                    .min_h(rems(6.))
                    .drag_over::<ExternalPaths>(|style, _, _, cx| style.bg(console(cx).fill_muted))
                    .on_drop(cx.listener(|this, paths: &ExternalPaths, _, cx| {
                        this.upload_paths(paths.paths().to_vec(), None, cx);
                    }))
                    .when(self.loading, |this| {
                        this.child(div().text_sm().text_color(console(cx).text_faint).child("Loading…"))
                    })
                    .when(!self.loading && entries.is_empty() && self.error.is_none(), |this| {
                        this.child(
                            div()
                                .text_sm()
                                .text_color(console(cx).text_faint)
                                .child("This folder is empty. Drop files from this PC here to upload them."),
                        )
                    })
                    .children(entries.iter().map(|file| self.render_row(file, cx))),
            )
    }
}
