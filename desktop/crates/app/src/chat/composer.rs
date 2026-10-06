//! The message box under a conversation (webui Composer.vue): Enter sends,
//! Shift+Enter breaks the line, `/` offers the user's skills, `@` their
//! delegation targets and `#` their workspace files. Attach and drop upload
//! local files into the workspace and mention them, as the web console's
//! attach button does.

use std::path::PathBuf;
use std::sync::Arc;
use std::time::Duration;

use aikonos_client::Connection;
use aikonos_client::api::chat::{DelegatableGroup, DelegatableUser, DelegationTarget, SkillBundle};
use aikonos_client::api::files::WorkspaceFile;
use aikonos_client::api::workspace::{BackendPref, WorkspaceBackend};
use gpui_kit::component::button::{Button, ButtonVariants as _};
use gpui_kit::component::input::{InputEvent, Textarea, TextareaState};
use gpui_kit::component::menu::{DropdownMenu as _, PopupMenuItem};
use gpui_kit::component::{ActiveTheme as _, Disableable as _, Icon, Sizable as _, h_flex, v_flex};
use gpui_kit::prelude::FluentBuilder as _;
use gpui_kit::*;

use crate::app;
use crate::assets::AppIcon;
use crate::local_files;
use crate::runtime;
use crate::theme::console;

actions!(composer, [PaletteUp, PaletteDown, PaletteAccept, PaletteDismiss]);

const PALETTE_CONTEXT: &str = "ComposerPalette";
const MAX_MENTIONS: usize = 8;
const HINT_PERIOD: Duration = Duration::from_secs(5);

/// Key bindings that steer an open palette instead of the text box. They
/// match only while a palette is open, and outrank the text box's own
/// bindings because they are more specific and registered later.
pub fn bind_keys(cx: &mut App) {
    let context = Some("ComposerPalette > Input");
    cx.bind_keys([
        KeyBinding::new("up", PaletteUp, context),
        KeyBinding::new("down", PaletteDown, context),
        KeyBinding::new("enter", PaletteAccept, context),
        KeyBinding::new("tab", PaletteAccept, context),
        KeyBinding::new("escape", PaletteDismiss, context),
    ]);
}

/// What the palettes and mentions draw from. Discovery only: the gateway
/// re-checks every skill, delegation and file on use.
#[derive(Clone, Default)]
pub struct Discovery {
    pub bundles: Vec<SkillBundle>,
    pub users: Vec<DelegatableUser>,
    pub groups: Vec<DelegatableGroup>,
    pub files: Vec<WorkspaceFile>,
    pub failed: bool,
}

/// What the user sent.
pub enum Submission {
    Text(String),
    /// A skill picked from the `/` palette.
    Skill(SkillBundle),
    /// Text that mentions a colleague or group picked from `@`.
    Delegate {
        text: String,
        target: DelegationTarget,
    },
}

pub enum ComposerEvent {
    Submit(Submission),
    Stop,
    /// The working folder moved between local and OneDrive, so `#` must
    /// offer the other backend's files.
    WorkspaceChanged,
}

#[derive(Clone, Copy, PartialEq, Eq, Debug)]
enum Trigger {
    At,
    Hash,
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct Mention {
    trigger: Trigger,
    query: String,
    /// Byte offset of the trigger character.
    start: usize,
}

enum PaletteItem {
    Skill(SkillBundle),
    User(DelegatableUser),
    Group(DelegatableGroup),
    File(String),
}

pub struct Composer {
    connection: Arc<Connection>,
    input: Entity<TextareaState>,
    placeholder: SharedString,
    hint_ix: usize,
    running: bool,
    disabled: bool,
    discovery: Discovery,
    mention: Option<Mention>,
    highlighted: usize,
    /// `@` picks this composing session; a pick counts while its
    /// `@<name>` survives in the text.
    picked: Vec<DelegationTarget>,
    uploading: usize,
    workspace: Option<WorkspaceBackend>,
    _subscriptions: Vec<Subscription>,
    _hints: Task<()>,
}

impl EventEmitter<ComposerEvent> for Composer {}

impl Composer {
    pub fn new(connection: Arc<Connection>, window: &mut Window, cx: &mut Context<Self>) -> Self {
        let placeholder: SharedString = "Message aikonOS…".into();
        let input = cx.new(|cx| {
            TextareaState::new(window, cx)
                .auto_grow(1, 8)
                .submit_on_enter(true)
                .placeholder(placeholder.clone())
        });
        let changes = cx.subscribe_in(&input, window, |this, _, event, window, cx| match event {
            InputEvent::Change => this.on_change(cx),
            InputEvent::PressEnter { shift: false, .. } => this.submit(window, cx),
            _ => {}
        });
        let hints = cx.spawn_in(window, async move |this, cx| {
            loop {
                cx.background_executor().timer(HINT_PERIOD).await;
                let rotated = this.update_in(cx, |this, window, cx| this.rotate_hint(window, cx));
                if rotated.is_err() {
                    break;
                }
            }
        });
        let mut this = Self {
            connection,
            input,
            placeholder,
            hint_ix: 0,
            running: false,
            disabled: false,
            discovery: Discovery::default(),
            mention: None,
            highlighted: 0,
            picked: Vec::new(),
            uploading: 0,
            workspace: None,
            _subscriptions: vec![changes],
            _hints: hints,
        };
        this.load_workspace(cx);
        this
    }

    pub fn set_discovery(&mut self, discovery: Discovery, cx: &mut Context<Self>) {
        self.discovery = discovery;
        cx.notify();
    }

    pub fn set_running(&mut self, running: bool, window: &mut Window, cx: &mut Context<Self>) {
        if self.running == running {
            return;
        }
        let was_running = self.running;
        self.running = running;
        self.input
            .update(cx, |input, cx| input.set_disabled(running || self.disabled, cx));
        // The box re-enables when a reply finishes; give it focus back.
        if was_running && !running {
            self.focus(window, cx);
        }
        cx.notify();
    }

    /// End the running state without taking focus (the user pressed Stop,
    /// or left the conversation).
    pub fn stop_running(&mut self, cx: &mut Context<Self>) {
        self.running = false;
        let disabled = self.disabled;
        self.input.update(cx, |input, cx| input.set_disabled(disabled, cx));
        cx.notify();
    }

    pub fn focus(&self, window: &mut Window, cx: &mut Context<Self>) {
        self.input.update(cx, |input, cx| input.focus(window, cx));
    }

    pub fn text(&self, cx: &App) -> String {
        self.input.read(cx).value().to_string()
    }

    /// Replace the draft and put the caret at its end.
    pub fn set_text(&mut self, text: String, window: &mut Window, cx: &mut Context<Self>) {
        let end = text.len();
        self.input.update(cx, |input, cx| {
            input.set_value(text, window, cx);
            input.set_selected_range(end..end, cx);
            input.focus(window, cx);
        });
        self.on_change(cx);
    }

    fn clear(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        self.input.update(cx, |input, cx| input.clean(window, cx));
        self.mention = None;
        cx.notify();
    }

    fn rotate_hint(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        const HINTS: [&str; 3] = [
            "# mention a file for context",
            "@ delegate to a teammate or group",
            "/ run a skill",
        ];
        self.hint_ix = (self.hint_ix + 1) % (HINTS.len() + 1);
        let hint: SharedString = match self.hint_ix {
            0 => self.placeholder.clone(),
            ix => HINTS[ix - 1].into(),
        };
        self.input
            .update(cx, |input, cx| input.set_placeholder(hint, window, cx));
    }

    fn on_change(&mut self, cx: &mut Context<Self>) {
        let input = self.input.read(cx);
        let value = input.value().to_string();
        let caret = input.selected_range().end.min(value.len());
        let mention = detect_mention(&value, caret);
        if mention != self.mention {
            self.highlighted = 0;
        }
        self.mention = mention;
        cx.notify();
    }

    fn palette_items(&self, cx: &App) -> Vec<PaletteItem> {
        let value = self.input.read(cx).value();
        if let Some(prefix) = value.strip_prefix('/') {
            let prefix = prefix.to_lowercase();
            return self
                .discovery
                .bundles
                .iter()
                .filter(|bundle| bundle.name.to_lowercase().starts_with(&prefix))
                .cloned()
                .map(PaletteItem::Skill)
                .collect();
        }
        let Some(mention) = &self.mention else {
            return Vec::new();
        };
        let query = mention.query.to_lowercase();
        match mention.trigger {
            Trigger::At => {
                let users = self
                    .discovery
                    .users
                    .iter()
                    .filter(|user| {
                        user.display_name.to_lowercase().contains(&query)
                            || user.user_id.to_lowercase().contains(&query)
                    })
                    .cloned()
                    .map(PaletteItem::User);
                let groups = self
                    .discovery
                    .groups
                    .iter()
                    .filter(|group| {
                        group.display_name.to_lowercase().contains(&query)
                            || group.group_id.to_lowercase().contains(&query)
                    })
                    .cloned()
                    .map(PaletteItem::Group);
                users.chain(groups).take(MAX_MENTIONS).collect()
            }
            Trigger::Hash => self
                .discovery
                .files
                .iter()
                .filter(|file| !file.is_dir && file.path.to_lowercase().contains(&query))
                .take(MAX_MENTIONS)
                .map(|file| PaletteItem::File(file.path.clone()))
                .collect(),
        }
    }

    fn palette_up(&mut self, _: &PaletteUp, _: &mut Window, cx: &mut Context<Self>) {
        let count = self.palette_items(cx).len();
        if count > 0 {
            self.highlighted = (self.highlighted + count - 1) % count;
            cx.notify();
        }
    }

    fn palette_down(&mut self, _: &PaletteDown, _: &mut Window, cx: &mut Context<Self>) {
        let count = self.palette_items(cx).len();
        if count > 0 {
            self.highlighted = (self.highlighted + 1) % count;
            cx.notify();
        }
    }

    fn palette_accept(&mut self, _: &PaletteAccept, window: &mut Window, cx: &mut Context<Self>) {
        let mut items = self.palette_items(cx);
        if items.is_empty() {
            return;
        }
        let ix = self.highlighted.min(items.len() - 1);
        let item = items.swap_remove(ix);
        self.choose(item, window, cx);
    }

    fn palette_dismiss(&mut self, _: &PaletteDismiss, _: &mut Window, cx: &mut Context<Self>) {
        self.mention = None;
        cx.notify();
    }

    fn choose(&mut self, item: PaletteItem, window: &mut Window, cx: &mut Context<Self>) {
        match item {
            PaletteItem::Skill(bundle) => {
                self.clear(window, cx);
                cx.emit(ComposerEvent::Submit(Submission::Skill(bundle)));
            }
            PaletteItem::User(user) => {
                let insertion = format!("@{} ", user.display_name);
                self.picked.push(DelegationTarget::User {
                    user_id: user.user_id,
                    display_name: user.display_name,
                });
                self.replace_mention(insertion, window, cx);
            }
            PaletteItem::Group(group) => {
                let insertion = format!("@{} ", group.display_name);
                self.picked.push(DelegationTarget::Group {
                    group_id: group.group_id,
                    display_name: group.display_name,
                    member_count: group.member_count,
                });
                self.replace_mention(insertion, window, cx);
            }
            PaletteItem::File(path) => self.replace_mention(format!("#{path} "), window, cx),
        }
    }

    /// Swap the trigger and query typed so far for `insertion`.
    fn replace_mention(&mut self, insertion: String, window: &mut Window, cx: &mut Context<Self>) {
        let Some(mention) = self.mention.take() else {
            return;
        };
        let end = mention.start + 1 + mention.query.len();
        self.input.update(cx, |input, cx| {
            input.set_selected_range(mention.start..end, cx);
            input.replace(insertion, window, cx);
            input.focus(window, cx);
        });
        cx.notify();
    }

    /// Insert `text` at the caret (an uploaded attachment's mention).
    fn insert_at_caret(&mut self, text: String, window: &mut Window, cx: &mut Context<Self>) {
        self.input.update(cx, |input, cx| {
            input.insert(text, window, cx);
            input.focus(window, cx);
        });
    }

    fn delegation_target(&self, text: &str) -> Option<DelegationTarget> {
        self.picked
            .iter()
            .filter_map(|peer| text.find(&format!("@{}", peer.display_name())).map(|at| (at, peer)))
            .min_by_key(|(at, _)| *at)
            .map(|(_, peer)| peer.clone())
    }

    fn submit(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        if self.running || self.disabled {
            return;
        }
        let text = self.text(cx);
        // An exact "/<skill>" runs the skill even with the palette closed:
        // the raw slash text must never reach the agent as chat prose.
        if let Some(name) = text.strip_prefix('/')
            && let Some(bundle) = self
                .discovery
                .bundles
                .iter()
                .find(|bundle| bundle.name == name.trim())
                .cloned()
        {
            self.clear(window, cx);
            cx.emit(ComposerEvent::Submit(Submission::Skill(bundle)));
            return;
        }
        if text.trim().is_empty() {
            return;
        }
        let submission = match self.delegation_target(&text) {
            Some(target) => Submission::Delegate { text, target },
            None => Submission::Text(text),
        };
        self.clear(window, cx);
        cx.emit(ComposerEvent::Submit(submission));
    }

    fn pick_and_attach(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        cx.spawn_in(window, async move |this, cx| {
            let paths = local_files::pick_files("Attach", cx).await;
            if paths.is_empty() {
                return;
            }
            let _ = this.update_in(cx, |this, window, cx| this.attach_paths(paths, window, cx));
        })
        .detach();
    }

    /// Upload local files to the workspace and mention each one.
    pub fn attach_paths(&mut self, paths: Vec<PathBuf>, window: &mut Window, cx: &mut Context<Self>) {
        for path in paths {
            self.uploading += 1;
            cx.notify();
            let connection = self.connection.clone();
            cx.spawn_in(window, async move |this, cx| {
                let read = cx
                    .background_executor()
                    .spawn(async move { local_files::read_for_upload(&path) })
                    .await;
                let result = match read {
                    Ok(file) => {
                        let target = local_files::attachment_path(&file.name);
                        runtime::spawn(async move {
                            match connection.write_file(&target, &file.bytes).await {
                                Ok(saved) => Ok(saved.map(|f| f.path).unwrap_or(target)),
                                // The broker creates folders, but retry once
                                // after creating references/ in case a backend
                                // ever needs it first (Composer.vue).
                                Err(_) if target.starts_with("references/") => {
                                    let _ = connection.create_dir("references").await;
                                    connection
                                        .write_file(&target, &file.bytes)
                                        .await
                                        .map(|saved| saved.map(|f| f.path).unwrap_or(target))
                                }
                                Err(err) => Err(err),
                            }
                        })
                        .await
                        .map_err(|err| err.to_string())
                    }
                    Err(message) => Err(message),
                };
                let _ = this.update_in(cx, |this, window, cx| {
                    this.uploading -= 1;
                    match result {
                        Ok(path) => this.insert_at_caret(format!("#{path} "), window, cx),
                        Err(message) => app::toast_error(message, window, cx),
                    }
                    cx.notify();
                });
            })
            .detach();
        }
    }

    fn load_workspace(&mut self, cx: &mut Context<Self>) {
        let connection = self.connection.clone();
        cx.spawn(async move |this, cx| {
            let backend = runtime::spawn(async move { connection.workspace_backend().await }).await;
            let _ = this.update(cx, |this, cx| {
                this.workspace = backend.ok();
                cx.notify();
            });
        })
        .detach();
    }

    fn set_backend(&mut self, pref: BackendPref, window: &mut Window, cx: &mut Context<Self>) {
        let connection = self.connection.clone();
        cx.spawn_in(window, async move |this, cx| {
            let result = runtime::spawn(async move { connection.set_workspace_backend(&pref).await }).await;
            let _ = this.update_in(cx, |this, window, cx| match result {
                Ok(pref) => {
                    if let Some(workspace) = &mut this.workspace {
                        workspace.pref = pref;
                    }
                    cx.emit(ComposerEvent::WorkspaceChanged);
                    cx.notify();
                }
                Err(err) => app::report(&err, window, cx),
            });
        })
        .detach();
    }

    fn render_palette(&self, cx: &mut Context<Self>) -> Option<impl IntoElement> {
        let items = self.palette_items(cx);
        if items.is_empty() {
            return None;
        }
        let theme = cx.theme();
        let highlighted = self.highlighted.min(items.len() - 1);
        Some(
            v_flex()
                .id("composer-palette")
                .absolute()
                .bottom_full()
                .left_0()
                .right_0()
                .mb_1()
                .py_1()
                .max_h(rems(13.75))
                .overflow_y_scroll()
                .bg(theme.popover)
                .border_1()
                .border_color(theme.border)
                .rounded(theme.radius_lg)
                .shadow_md()
                .children(items.into_iter().enumerate().map(|(ix, item)| {
                    let (key, name, detail, badge): (SharedString, SharedString, Option<SharedString>, bool) =
                        match &item {
                            PaletteItem::Skill(bundle) => (
                                format!("skill-{}", bundle.id).into(),
                                bundle.name.clone().into(),
                                (!bundle.description.is_empty()).then(|| bundle.description.clone().into()),
                                bundle.personal,
                            ),
                            PaletteItem::User(user) => (
                                format!("user-{}", user.user_id).into(),
                                user.display_name.clone().into(),
                                Some(user.user_id.clone().into()),
                                false,
                            ),
                            PaletteItem::Group(group) => (
                                format!("group-{}", group.group_id).into(),
                                group.display_name.clone().into(),
                                Some(format!("group · {} people", group.member_count).into()),
                                false,
                            ),
                            PaletteItem::File(path) => {
                                (format!("file-{path}").into(), path.clone().into(), None, false)
                            }
                        };
                    let item = std::cell::Cell::new(Some(item));
                    v_flex()
                        .id(key)
                        .px_3()
                        .py(rems(0.45))
                        .gap_0p5()
                        .cursor_pointer()
                        .when(ix == highlighted, |this| this.bg(theme.accent))
                        .hover(|this| this.bg(theme.accent))
                        .on_mouse_down(
                            MouseButton::Left,
                            cx.listener(move |this, _, window, cx| {
                                // Keep the text box focused: choose on press.
                                cx.stop_propagation();
                                window.prevent_default();
                                if let Some(item) = item.take() {
                                    this.choose(item, window, cx);
                                }
                            }),
                        )
                        .child(
                            div()
                                .text_sm()
                                .font_weight(FontWeight::MEDIUM)
                                .text_color(theme.foreground)
                                .child(name),
                        )
                        .when(badge, |this| {
                            this.child(
                                div()
                                    .self_start()
                                    .px_1p5()
                                    .rounded(theme.radius)
                                    .bg(theme.accent)
                                    .text_size(rems(0.625))
                                    .text_color(theme.muted_foreground)
                                    .child("personal"),
                            )
                        })
                        .when_some(detail, |this, detail| {
                            this.child(div().text_xs().text_color(theme.muted_foreground).child(detail))
                        })
                })),
        )
    }

    fn render_workspace_control(&self, cx: &mut Context<Self>) -> Option<impl IntoElement> {
        let workspace = self.workspace.as_ref()?;
        if !workspace.onedrive_available {
            return None;
        }
        let label: SharedString = if workspace.pref.is_onedrive() {
            format!("Working folder: OneDrive · /{}", workspace.pref.onedrive_folder_path).into()
        } else {
            "Working folder: Local".into()
        };
        let composer = cx.weak_entity();
        Some(
            h_flex().mt_1p5().child(
                Button::new("workspace-control")
                    .ghost()
                    .xsmall()
                    .label(label)
                    .dropdown_caret(true)
                    .text_color(cx.theme().muted_foreground)
                    .dropdown_menu(move |menu, _, _| {
                        let local = {
                            let composer = composer.clone();
                            move |_: &ClickEvent, window: &mut Window, cx: &mut App| {
                                let _ = composer.update(cx, |this, cx| {
                                    this.set_backend(
                                        BackendPref {
                                            backend: "local".into(),
                                            onedrive_folder_path: String::new(),
                                        },
                                        window,
                                        cx,
                                    )
                                });
                            }
                        };
                        let onedrive = {
                            let composer = composer.clone();
                            move |_: &ClickEvent, window: &mut Window, cx: &mut App| {
                                let composer = composer.clone();
                                crate::chat::onedrive::pick_folder(window, cx, move |path, window, cx| {
                                    let _ = composer.update(cx, |this, cx| {
                                        this.set_backend(
                                            BackendPref {
                                                backend: "onedrive".into(),
                                                onedrive_folder_path: path,
                                            },
                                            window,
                                            cx,
                                        )
                                    });
                                });
                            }
                        };
                        menu.item(PopupMenuItem::new("Local workspace").on_click(local))
                            .item(PopupMenuItem::new("OneDrive folder…").on_click(onedrive))
                    }),
            ),
        )
    }
}

/// Find an `@` or `#` being typed at `caret`: the nearest trigger before
/// the caret with no whitespace in between, itself at the start or after
/// whitespace (Composer.vue `detectMention`).
fn detect_mention(value: &str, caret: usize) -> Option<Mention> {
    let before = value.get(..caret)?;
    for (ix, ch) in before.char_indices().rev() {
        if ch.is_whitespace() {
            return None;
        }
        if ch == '@' || ch == '#' {
            let preceded_ok = before[..ix].chars().next_back().is_none_or(char::is_whitespace);
            if !preceded_ok {
                return None;
            }
            return Some(Mention {
                trigger: if ch == '@' { Trigger::At } else { Trigger::Hash },
                query: before[ix + 1..].to_owned(),
                start: ix,
            });
        }
    }
    None
}

impl Render for Composer {
    fn render(&mut self, _: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let theme = cx.theme();
        let palette_open = !self.palette_items(cx).is_empty();
        let uploading = self.uploading > 0;
        v_flex()
            .key_context(if palette_open { PALETTE_CONTEXT } else { "Composer" })
            .on_action(cx.listener(Self::palette_up))
            .on_action(cx.listener(Self::palette_down))
            .on_action(cx.listener(Self::palette_accept))
            .on_action(cx.listener(Self::palette_dismiss))
            .child(
                div()
                    .id("composer")
                    .relative()
                    .p_3()
                    .bg(theme.popover)
                    .border_1()
                    .border_color(theme.border)
                    .rounded(theme.radius_lg)
                    .drag_over::<ExternalPaths>(|style, _, _, cx| {
                        style.border_color(cx.theme().primary).bg(console(cx).fill_accent)
                    })
                    .on_drop(cx.listener(|this, paths: &ExternalPaths, window, cx| {
                        this.attach_paths(paths.paths().to_vec(), window, cx);
                    }))
                    .children(self.render_palette(cx))
                    .child(
                        div().pr(rems(4.75)).child(
                            Textarea::new(&self.input)
                                .appearance(false)
                                .disabled(self.running || self.disabled),
                        ),
                    )
                    .child(
                        h_flex()
                            .absolute()
                            .right_3()
                            .bottom_3()
                            .gap_2()
                            .child(
                                Button::new("attach")
                                    .outline()
                                    .icon(Icon::new(AppIcon::Upload))
                                    .loading(uploading)
                                    .disabled(self.disabled)
                                    .tooltip("Attach files from this PC")
                                    .on_click(cx.listener(|this, _, window, cx| this.pick_and_attach(window, cx))),
                            )
                            .child(if self.running {
                                Button::new("stop")
                                    .primary()
                                    .icon(Icon::new(AppIcon::Stop))
                                    .tooltip("Stop")
                                    .on_click(cx.listener(|_, _, _, cx| cx.emit(ComposerEvent::Stop)))
                            } else {
                                Button::new("send")
                                    .primary()
                                    .icon(Icon::new(AppIcon::Send))
                                    .disabled(self.disabled)
                                    .tooltip("Send")
                                    .on_click(cx.listener(|this, _, window, cx| this.submit(window, cx)))
                            }),
                    ),
            )
            .children(self.render_workspace_control(cx))
    }
}

#[cfg(test)]
mod tests {
    use super::{Mention, Trigger, detect_mention};

    #[test]
    fn mentions_need_a_word_start_and_no_space_before_the_caret() {
        assert_eq!(
            detect_mention("ask @al", 7),
            Some(Mention {
                trigger: Trigger::At,
                query: "al".into(),
                start: 4
            })
        );
        assert_eq!(
            detect_mention("#rep", 4),
            Some(Mention {
                trigger: Trigger::Hash,
                query: "rep".into(),
                start: 0
            })
        );
        assert_eq!(detect_mention("mail me@home", 12), None);
        assert_eq!(detect_mention("@bob is", 7), None);
        assert_eq!(
            detect_mention("Grüße @Jü", "Grüße @Jü".len()).map(|m| m.query),
            Some("Jü".into())
        );
    }
}
