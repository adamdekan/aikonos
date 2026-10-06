//! The landing page (webui views/Home.vue): a greeting, a box to start a
//! conversation, and starter chips.

use gpui_kit::component::button::{Button, ButtonVariants as _};
use gpui_kit::component::input::{InputEvent, Textarea, TextareaState};
use gpui_kit::component::{ActiveTheme as _, Icon, h_flex, v_flex};
use gpui_kit::*;

use crate::app;
use crate::assets::AppIcon;
use crate::shell;

const CHIPS: [(&str, AppIcon); 5] = [
    ("Write", AppIcon::Design),
    ("Learn", AppIcon::Star),
    ("Code", AppIcon::Code),
    ("Life stuff", AppIcon::Home),
    ("From Drive", AppIcon::Drive),
];

pub struct HomePage {
    draft: Entity<TextareaState>,
    _subscriptions: Vec<Subscription>,
}

impl HomePage {
    pub fn new(window: &mut Window, cx: &mut Context<Self>) -> Self {
        let draft = cx.new(|cx| {
            TextareaState::new(window, cx)
                .auto_grow(3, 8)
                .submit_on_enter(true)
                .placeholder("How can I help you today?")
        });
        let subscription = cx.subscribe_in(&draft, window, |this, _, event, window, cx| {
            if let InputEvent::PressEnter { shift: false, .. } = event {
                this.submit(window, cx);
            }
        });
        draft.update(cx, |draft, cx| draft.focus(window, cx));
        Self {
            draft,
            _subscriptions: vec![subscription],
        }
    }

    fn submit(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let text = self.draft.read(cx).value().trim().to_owned();
        if !text.is_empty() {
            shell::start_chat(text, window, cx);
        }
    }
}

impl Render for HomePage {
    fn render(&mut self, _: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let theme = cx.theme();
        let name = app::connection(cx).profile().display_name();
        v_flex()
            .id("home")
            .size_full()
            .overflow_y_scroll()
            .items_center()
            .justify_center()
            .px_6()
            .py_12()
            .gap_6()
            // Segoe UI Symbol carries the text form of the mark, the one a
            // browser on Windows falls back to; left to font fallback,
            // DirectWrite picks the colour emoji instead.
            .child(
                div()
                    .font_family("Segoe UI Symbol")
                    .text_size(rems(3.))
                    .line_height(relative(1.))
                    .text_color(theme.primary)
                    .child("\u{2733}"),
            )
            .child(
                div()
                    .text_size(rems(2.))
                    .text_color(theme.foreground)
                    .text_center()
                    .child(format!("Hey there, {name}")),
            )
            .child(
                h_flex()
                    .w_full()
                    .max_w(rems(40.))
                    .items_end()
                    .gap_2()
                    .p_3()
                    .bg(theme.popover)
                    .border_1()
                    .border_color(theme.border)
                    .rounded(theme.radius_lg)
                    .child(div().flex_1().child(Textarea::new(&self.draft).appearance(false)))
                    .child(
                        Button::new("home-send")
                            .primary()
                            .icon(Icon::new(AppIcon::Send))
                            .tooltip("Send")
                            .on_click(cx.listener(|this, _, window, cx| this.submit(window, cx))),
                    ),
            )
            .child(
                h_flex()
                    .flex_wrap()
                    .justify_center()
                    .gap_2()
                    .max_w(rems(40.))
                    .children(CHIPS.iter().map(|(label, icon)| {
                        let label = *label;
                        Button::new(label)
                            .outline()
                            .rounded(gpui_kit::component::button::ButtonRounded::Size(px(999.)))
                            .icon(Icon::new(*icon))
                            .label(label)
                            .text_color(theme.muted_foreground)
                            .bg(theme.popover)
                            .on_click(move |_, window, cx| shell::start_chat(label.to_owned(), window, cx))
                    })),
            )
    }
}
