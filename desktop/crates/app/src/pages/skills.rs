//! The user's personal skills and the admin-governed skills they are
//! granted (webui views/Skills.vue and ShareSkillModal.vue). Importing
//! reads a SKILL.md or a .skill/.zip bundle from this PC, by picker or by
//! dropping it on the page; the gateway scans it on the way in.

use std::path::PathBuf;
use std::sync::Arc;

use aikonos_client::Connection;
use aikonos_client::api::chat::{Delegatable, SkillBundle};
use aikonos_client::api::skills::{PersonalSkill, ShareRecipient, SkillsPage as SkillsData, suggested_name};
use gpui_kit::component::WindowExt as _;
use gpui_kit::component::button::{Button, ButtonVariant, ButtonVariants as _};
use gpui_kit::component::dialog::DialogFooter;
use gpui_kit::component::input::{Input, InputEvent, InputState};
use gpui_kit::component::{ActiveTheme as _, Disableable as _, Icon, Sizable as _, h_flex, v_flex};
use gpui_kit::prelude::FluentBuilder as _;
use gpui_kit::*;

use crate::app;
use crate::assets::AppIcon;
use crate::local_files;
use crate::runtime;
use crate::theme::console;
use crate::ui::{self, PillTone};

const ZIP_MAGIC: [u8; 4] = [0x50, 0x4b, 0x03, 0x04];

pub struct SkillsPage {
    connection: Arc<Connection>,
    data: SkillsData,
    forbidden: bool,
    loading: bool,
    error: Option<SharedString>,
    importing: bool,
    import_error: Option<SharedString>,
    filter: Entity<InputState>,
    expanded: Option<String>,
    _subscriptions: Vec<Subscription>,
}

impl SkillsPage {
    pub fn new(window: &mut Window, cx: &mut Context<Self>) -> Self {
        let filter = cx.new(|cx| InputState::new(window, cx).placeholder("Filter skills by name…"));
        let filter_changed = cx.subscribe(&filter, |_, _, event: &InputEvent, cx| {
            if matches!(event, InputEvent::Change) {
                cx.notify();
            }
        });
        let mut this = Self {
            connection: app::connection(cx),
            data: SkillsData::default(),
            forbidden: false,
            loading: true,
            error: None,
            importing: false,
            import_error: None,
            filter,
            expanded: None,
            _subscriptions: vec![filter_changed],
        };
        this.load(cx);
        this
    }

    fn load(&mut self, cx: &mut Context<Self>) {
        self.loading = true;
        let connection = self.connection.clone();
        cx.spawn(async move |this, cx| {
            let page = runtime::spawn(async move { connection.personal_skills().await }).await;
            let _ = this.update(cx, |this, cx| {
                this.loading = false;
                match page {
                    Ok(data) => {
                        this.data = data;
                        this.forbidden = false;
                        this.error = None;
                    }
                    Err(err) if err.is_forbidden() => this.forbidden = true,
                    Err(err) => this.error = Some(err.to_string().into()),
                }
                cx.notify();
            });
        })
        .detach();
    }

    fn pick_and_import(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        cx.spawn_in(window, async move |this, cx| {
            let paths = local_files::pick_files("Import", cx).await;
            if let Some(path) = paths.into_iter().next() {
                let _ = this.update_in(cx, |this, window, cx| this.import(path, window, cx));
            }
        })
        .detach();
    }

    fn import(&mut self, path: PathBuf, window: &mut Window, cx: &mut Context<Self>) {
        if self.importing {
            return;
        }
        self.importing = true;
        self.import_error = None;
        cx.notify();
        let connection = self.connection.clone();
        cx.spawn_in(window, async move |this, cx| {
            let read = cx
                .background_executor()
                .spawn(async move { local_files::read_for_upload(&path) })
                .await;
            let imported = match read {
                Ok(file) => {
                    // A zip is recognised by its bytes, as on the web.
                    let is_zip = file.bytes.starts_with(&ZIP_MAGIC);
                    runtime::spawn(async move { connection.import_skill(file.bytes, is_zip).await })
                        .await
                        .map_err(|err| match suggested_name(&err) {
                            Some(name) => format!(
                                "{err} Suggested name: \"{name}\" (rename inside the bundle and re-import to use it)."
                            ),
                            None => err.to_string(),
                        })
                }
                Err(message) => Err(message),
            };
            let _ = this.update_in(cx, |this, window, cx| {
                this.importing = false;
                match imported {
                    Ok(_) => {
                        app::toast_ok("Skill imported.", window, cx);
                        this.load(cx);
                    }
                    Err(message) => this.import_error = Some(message.into()),
                }
                cx.notify();
            });
        })
        .detach();
    }

    fn confirm_delete(&mut self, skill: &PersonalSkill, window: &mut Window, cx: &mut Context<Self>) {
        let name = skill.name.clone();
        let page = cx.weak_entity();
        window.open_alert_dialog(cx, move |alert, _, _| {
            let name = name.clone();
            let page = page.clone();
            alert
                .title(format!("Delete “{name}”?"))
                .description(format!(
                    "This removes the whole Skills/{name}/ folder and cannot be undone."
                ))
                .confirm()
                .ok_text("Delete")
                .ok_variant(ButtonVariant::Danger)
                .on_ok(move |_, window, cx| {
                    let name = name.clone();
                    let _ = page.update(cx, |this, cx| this.delete(name, window, cx));
                    true
                })
        });
    }

    fn delete(&mut self, name: String, window: &mut Window, cx: &mut Context<Self>) {
        let connection = self.connection.clone();
        cx.spawn_in(window, async move |this, cx| {
            let deleted = {
                let name = name.clone();
                runtime::spawn(async move { connection.delete_skill(&name).await }).await
            };
            let _ = this.update_in(cx, |this, window, cx| {
                match deleted {
                    Ok(()) => app::toast_ok(format!("Skill \"{name}\" deleted."), window, cx),
                    Err(err) => app::report(&err, window, cx),
                }
                this.load(cx);
            });
        })
        .detach();
    }

    fn open_share(&mut self, skill: &PersonalSkill, window: &mut Window, cx: &mut Context<Self>) {
        let share = cx.new(|cx| ShareDialog::new(self.connection.clone(), skill.name.clone(), window, cx));
        window.open_dialog(cx, move |dialog, _, cx| {
            let footer = share.clone();
            dialog
                .title(format!("Share “{}”", share.read(cx).skill))
                .w(px(480.))
                .bg(cx.theme().popover)
                .child(share.clone())
                .footer(
                    DialogFooter::new()
                        .child(footer.update(cx, |share, cx| share.render_footer(cx).into_any_element())),
                )
        });
    }

    fn render_skill(&self, skill: &PersonalSkill, cx: &mut Context<Self>) -> AnyElement {
        let theme = cx.theme().clone();
        let (for_share, for_delete) = (skill.clone(), skill.clone());
        h_flex()
            .gap_3()
            .p_4()
            .bg(theme.popover)
            .border_1()
            .border_color(theme.border)
            .rounded(theme.radius)
            .child(
                v_flex()
                    .flex_1()
                    .min_w_0()
                    .gap_1()
                    .child(
                        div()
                            .text_sm()
                            .font_weight(FontWeight::MEDIUM)
                            .child(skill.name.clone()),
                    )
                    .child(
                        h_flex()
                            .gap_1p5()
                            .flex_wrap()
                            .child(ui::pill(
                                if skill.valid { "valid" } else { "invalid" },
                                if skill.valid { PillTone::Ok } else { PillTone::Danger },
                                cx,
                            ))
                            .when(!skill.valid && !skill.warning.is_empty(), |this| {
                                this.child(div().text_xs().text_color(theme.danger).child(skill.warning.clone()))
                            })
                            .child(ui::pill(ui::file_size(skill.size_bytes), PillTone::Neutral, cx)),
                    ),
            )
            .child(
                Button::new(SharedString::from(format!("share-{}", skill.name)))
                    .outline()
                    .small()
                    .label("Share")
                    .on_click(cx.listener(move |this, _, window, cx| this.open_share(&for_share, window, cx))),
            )
            .child(
                Button::new(SharedString::from(format!("delete-{}", skill.name)))
                    .outline()
                    .small()
                    .label("Delete")
                    .text_color(theme.danger)
                    .on_click(cx.listener(move |this, _, window, cx| this.confirm_delete(&for_delete, window, cx))),
            )
            .into_any_element()
    }

    fn render_granted(&self, bundle: &SkillBundle, cx: &mut Context<Self>) -> AnyElement {
        let theme = cx.theme().clone();
        let id = bundle.id.clone();
        let open = self.expanded.as_deref() == Some(id.as_str());
        v_flex()
            .gap_2()
            .child(
                h_flex()
                    .gap_3()
                    .p_4()
                    .bg(theme.popover)
                    .border_1()
                    .border_color(theme.border)
                    .rounded(theme.radius)
                    .child(
                        v_flex()
                            .flex_1()
                            .min_w_0()
                            .gap_1()
                            .child(
                                div()
                                    .text_sm()
                                    .font_weight(FontWeight::MEDIUM)
                                    .child(bundle.name.clone()),
                            )
                            .child(
                                div()
                                    .text_xs()
                                    .text_color(theme.muted_foreground)
                                    .child(bundle.description.clone()),
                            ),
                    )
                    .child(
                        Button::new(SharedString::from(format!("view-{id}")))
                            .outline()
                            .small()
                            .label(if open { "Hide" } else { "View" })
                            .on_click(cx.listener(move |this, _, _, cx| {
                                this.expanded = if this.expanded.as_deref() == Some(id.as_str()) {
                                    None
                                } else {
                                    Some(id.clone())
                                };
                                cx.notify();
                            })),
                    ),
            )
            .when(open, |this| {
                this.child(
                    div()
                        .id(SharedString::from(format!("granted-body-{}", bundle.id)))
                        .max_h(rems(16.))
                        .overflow_y_scroll()
                        .p_3()
                        .rounded(theme.radius)
                        .bg(theme.background)
                        .border_1()
                        .border_color(theme.border)
                        .font_family(theme.mono_font_family.clone())
                        .text_xs()
                        .child(bundle.body.clone()),
                )
            })
            .into_any_element()
    }
}

impl Render for SkillsPage {
    fn render(&mut self, _: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let theme = cx.theme().clone();
        let query = self.filter.read(cx).value().trim().to_lowercase();
        let matches = |name: &str| query.is_empty() || name.to_lowercase().contains(&query);
        let skills: Vec<PersonalSkill> = self.data.skills.iter().filter(|s| matches(&s.name)).cloned().collect();
        let granted: Vec<SkillBundle> = self.data.granted.iter().filter(|g| matches(&g.name)).cloned().collect();
        let section = |title: &'static str| {
            div()
                .text_sm()
                .font_weight(FontWeight::SEMIBOLD)
                .text_color(theme.muted_foreground)
                .child(title)
        };
        let page = ui::page("skills-page", ui::PAGE_WIDTH).child(ui::view_header(AppIcon::Book, "Skills", cx));
        if self.forbidden {
            return page.child(ui::empty_state(
                Some(AppIcon::Admin),
                "You do not have access to personal skills. Ask an admin to grant skill:personal-skills.",
                cx,
            ));
        }
        page.when_some(self.error.clone(), |this, error| {
            this.child(ui::error_banner(error, None, cx))
        })
        .when(!self.data.skills.is_empty() || !self.data.granted.is_empty(), |this| {
            this.child(Input::new(&self.filter).small())
        })
        .child(
            v_flex()
                .id("my-skills")
                .gap_2()
                .rounded(theme.radius)
                .drag_over::<ExternalPaths>(|style, _, _, cx| style.bg(console(cx).fill_muted))
                .on_drop(cx.listener(|this, paths: &ExternalPaths, window, cx| {
                    if let Some(path) = paths.paths().first().cloned() {
                        this.import(path, window, cx);
                    }
                }))
                .child(section("My skills"))
                .child(
                    h_flex().child(
                        Button::new("import-skill")
                            .outline()
                            .small()
                            .icon(Icon::new(AppIcon::Upload))
                            .label(if self.importing { "Importing…" } else { "Import" })
                            .loading(self.importing)
                            .disabled(self.importing)
                            .tooltip("Import a SKILL.md or a .skill/.zip bundle from this PC")
                            .on_click(cx.listener(|this, _, window, cx| this.pick_and_import(window, cx))),
                    ),
                )
                .when_some(self.import_error.clone(), |this, error| {
                    this.child(div().text_sm().text_color(theme.danger).child(error))
                })
                .when(self.loading && self.data.skills.is_empty(), |this| {
                    this.child(ui::empty_state(None, "Loading…", cx))
                })
                .when(!self.loading && skills.is_empty(), |this| {
                    this.child(
                        div()
                            .text_sm()
                            .text_color(console(cx).text_faint)
                            .child("No skills yet. Author a SKILL.md under Skills/<name>/ or import a bundle above."),
                    )
                })
                .children(skills.iter().map(|skill| self.render_skill(skill, cx))),
        )
        .when(!self.data.granted.is_empty() || self.data.granted_unavailable, |this| {
            this.child(
                v_flex()
                    .gap_2()
                    .child(section("Granted skills"))
                    .when(self.data.granted_unavailable, |this| {
                        this.child(ui::error_banner(
                            "Granted skills are temporarily unavailable.",
                            None,
                            cx,
                        ))
                    })
                    .children(granted.iter().map(|bundle| self.render_granted(bundle, cx))),
            )
        })
    }
}

/// Sharing a personal skill: pick a colleague or group, confirm, done.
struct ShareDialog {
    connection: Arc<Connection>,
    skill: String,
    targets: Delegatable,
    query: Entity<InputState>,
    chosen: Option<ShareTarget>,
    submitting: bool,
    done: Option<usize>,
    error: Option<SharedString>,
    _subscription: Subscription,
}

#[derive(Clone)]
struct ShareTarget {
    recipient: ShareRecipient,
    label: String,
}

impl ShareDialog {
    fn new(connection: Arc<Connection>, skill: String, window: &mut Window, cx: &mut Context<Self>) -> Self {
        let query = cx.new(|cx| InputState::new(window, cx).placeholder("Search people and groups…"));
        let subscription = cx.subscribe(&query, |_, _, event: &InputEvent, cx| {
            if matches!(event, InputEvent::Change) {
                cx.notify();
            }
        });
        let loader = connection.clone();
        cx.spawn(async move |this, cx| {
            let targets = runtime::spawn(async move { loader.delegatable().await }).await;
            let _ = this.update(cx, |this, cx| {
                match targets {
                    Ok(targets) => this.targets = targets,
                    Err(err) => this.error = Some(err.to_string().into()),
                }
                cx.notify();
            });
        })
        .detach();
        Self {
            connection,
            skill,
            targets: Delegatable::default(),
            query,
            chosen: None,
            submitting: false,
            done: None,
            error: None,
            _subscription: subscription,
        }
    }

    fn share(&mut self, cx: &mut Context<Self>) {
        let Some(target) = self.chosen.clone() else {
            return;
        };
        if self.submitting {
            return;
        }
        self.submitting = true;
        self.error = None;
        cx.notify();
        let connection = self.connection.clone();
        let skill = self.skill.clone();
        cx.spawn(async move |this, cx| {
            let shared = runtime::spawn(async move { connection.share_skill(&skill, &target.recipient).await }).await;
            let _ = this.update(cx, |this, cx| {
                this.submitting = false;
                match shared {
                    Ok(result) => this.done = Some(result.skipped_user_ids.len()),
                    Err(err) => this.error = Some(err.to_string().into()),
                }
                cx.notify();
            });
        })
        .detach();
    }

    fn render_footer(&mut self, cx: &mut Context<Self>) -> impl IntoElement {
        let confirming = self.chosen.is_some() && self.done.is_none();
        h_flex()
            .w_full()
            .justify_end()
            .gap_2()
            .when(confirming, |this| {
                this.child(
                    Button::new("share-back")
                        .ghost()
                        .label("Back")
                        .disabled(self.submitting)
                        .on_click(cx.listener(|this, _, _, cx| {
                            this.chosen = None;
                            cx.notify();
                        })),
                )
                .child(
                    Button::new("share-confirm")
                        .primary()
                        .label(if self.submitting { "Sharing…" } else { "Share" })
                        .loading(self.submitting)
                        .on_click(cx.listener(|this, _, _, cx| this.share(cx))),
                )
            })
            .when(!confirming, |this| {
                this.child(
                    Button::new("share-close")
                        .ghost()
                        .label("Close")
                        .on_click(|_, window, cx| window.close_dialog(cx)),
                )
            })
    }
}

impl Render for ShareDialog {
    fn render(&mut self, _: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let theme = cx.theme().clone();
        if let Some(skipped) = self.done {
            let mut text = format!(
                "“{}” was sent. They can review and accept it from their inbox.",
                self.skill
            );
            if skipped > 0 {
                text.push_str(&format!(" {skipped} recipient(s) were skipped."));
            }
            return v_flex().child(div().text_sm().child(text)).into_any_element();
        }
        if let Some(target) = &self.chosen {
            return v_flex()
                .gap_2()
                .child(
                    div()
                        .text_sm()
                        .child(format!("Share “{}” with {}?", self.skill, target.label)),
                )
                .child(
                    div()
                        .text_xs()
                        .text_color(theme.muted_foreground)
                        .child("They receive a copy to review in their inbox; your skill stays as it is."),
                )
                .when_some(self.error.clone(), |this, error| {
                    this.child(div().text_sm().text_color(theme.danger).child(error))
                })
                .into_any_element();
        }
        let query = self.query.read(cx).value().trim().to_lowercase();
        let mut targets: Vec<ShareTarget> = self
            .targets
            .users
            .iter()
            .map(|u| ShareTarget {
                recipient: ShareRecipient::User(u.user_id.clone()),
                label: u.display_name.clone(),
            })
            .chain(self.targets.groups.iter().map(|g| ShareTarget {
                recipient: ShareRecipient::Group(g.group_id.clone()),
                label: format!("{} (group · {} people)", g.display_name, g.member_count),
            }))
            .collect();
        targets.retain(|t| query.is_empty() || t.label.to_lowercase().contains(&query));
        v_flex()
            .gap_2()
            .child(Input::new(&self.query).small())
            .when(targets.is_empty(), |this| {
                this.child(
                    div()
                        .text_sm()
                        .text_color(console(cx).text_faint)
                        .child("No people or groups to share with."),
                )
            })
            .children(targets.into_iter().enumerate().map(|(ix, target)| {
                let label = target.label.clone();
                ui::row_button(("share-target", ix), label, None).on_click(cx.listener(move |this, _, _, cx| {
                    this.chosen = Some(target.clone());
                    cx.notify();
                }))
            }))
            .when_some(self.error.clone(), |this, error| {
                this.child(div().text_sm().text_color(theme.danger).child(error))
            })
            .into_any_element()
    }
}
