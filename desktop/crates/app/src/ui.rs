//! Small presentational pieces the console repeats on every page, drawn
//! from the theme: section labels, banners, empty states, pills, the
//! "working" dots, and the page frame.

use std::time::Duration;

use gpui_kit::component::button::{Button, ButtonVariants as _};
use gpui_kit::component::{ActiveTheme as _, Icon, Sizable as _, h_flex, v_flex};
use gpui_kit::prelude::FluentBuilder as _;
use gpui_kit::*;

use crate::assets::AppIcon;
use crate::theme::console;

/// The uppercase micro-label above a sidebar or page section.
pub fn section_label(text: impl Into<SharedString>, cx: &App) -> Div {
    div()
        .text_size(rems(0.625))
        .font_weight(FontWeight::SEMIBOLD)
        .text_color(cx.theme().muted_foreground)
        .child(text.into().to_uppercase())
}

/// A red banner for a failed load, with an optional trailing action.
pub fn error_banner(message: impl Into<SharedString>, action: Option<AnyElement>, cx: &App) -> Div {
    h_flex()
        .w_full()
        .gap_2()
        .px_4()
        .py_3()
        .rounded(cx.theme().radius)
        .bg(console(cx).fill_danger)
        .border_1()
        .border_color(cx.theme().danger)
        .text_color(cx.theme().danger)
        .text_sm()
        .child(Icon::new(AppIcon::Close).xsmall())
        .child(div().flex_1().child(message.into()))
        .children(action)
}

/// A neutral line for an empty list.
pub fn empty_state(icon: Option<AppIcon>, message: impl Into<SharedString>, cx: &App) -> Div {
    v_flex()
        .items_center()
        .justify_center()
        .gap_2()
        .py_8()
        .px_4()
        .text_color(console(cx).text_faint)
        .when_some(icon, |this, icon| this.child(Icon::new(icon).size_8()))
        .child(div().text_sm().child(message.into()))
}

#[derive(Clone, Copy, PartialEq, Eq)]
pub enum PillTone {
    Neutral,
    Accent,
    Ok,
    Danger,
}

/// A small rounded status label.
pub fn pill(text: impl Into<SharedString>, tone: PillTone, cx: &App) -> Div {
    let theme = cx.theme();
    let tokens = console(cx);
    let (bg, fg, border) = match tone {
        PillTone::Neutral => (tokens.fill_muted, theme.muted_foreground, theme.border),
        PillTone::Accent => (tokens.fill_accent, tokens.accent_text, tokens.accent_text),
        PillTone::Ok => (tokens.fill_ok, theme.success, theme.success),
        PillTone::Danger => (tokens.fill_danger, theme.danger, theme.danger),
    };
    div()
        .flex_none()
        .px_2()
        .rounded_full()
        .border_1()
        .border_color(border)
        .bg(bg)
        .text_color(fg)
        .text_size(rems(0.6875))
        .font_weight(FontWeight::SEMIBOLD)
        .child(text.into())
}

/// Three bouncing dots: the console's "working" indicator.
pub fn working_dots(id: impl Into<ElementId>, cx: &App) -> impl IntoElement {
    let color = cx.theme().muted_foreground;
    let id: ElementId = id.into();
    h_flex().gap_0p5().py_0p5().children((0..3).map(move |ix| {
        div().size_1().rounded_full().bg(color).with_animation(
            ElementId::NamedInteger(format!("{id:?}-dot").into(), ix),
            Animation::new(Duration::from_millis(1200)).repeat(),
            move |dot, t| {
                // The web's keyframes: rise a quarter rem at 30%, 0.2s apart.
                let phase = (t - ix as f32 * 0.1667).rem_euclid(1.0);
                let lift = if phase < 0.3 {
                    phase / 0.3
                } else if phase < 0.6 {
                    1.0 - (phase - 0.3) / 0.3
                } else {
                    0.0
                };
                dot.mt(rems(-0.25 * lift)).mb(rems(0.25 * lift))
            },
        )
    }))
}

/// A page in the main pane: the pane is the scroll owner, the content a
/// centred column (the web console's `.view`: 2rem inset, max 720px, 1.5rem
/// between sections). Children go into the column.
pub fn page(id: impl Into<ElementId>, width: Rems) -> PageColumn {
    PageColumn {
        outer: div().id(id).size_full().overflow_y_scroll(),
        column: v_flex().w_full().max_w(width).mx_auto().p_8().gap_6(),
    }
}

/// The column width most console pages use (720px).
pub const PAGE_WIDTH: Rems = Rems(45.);

#[derive(IntoElement)]
pub struct PageColumn {
    outer: Stateful<Div>,
    column: Div,
}

impl ParentElement for PageColumn {
    fn extend(&mut self, elements: impl IntoIterator<Item = AnyElement>) {
        self.column.extend(elements);
    }
}

impl RenderOnce for PageColumn {
    fn render(self, _: &mut Window, _: &mut App) -> impl IntoElement {
        self.outer.child(self.column)
    }
}

/// A page's header row: icon and title, then trailing actions.
pub fn view_header(icon: AppIcon, title: impl Into<SharedString>, cx: &App) -> Div {
    h_flex()
        .gap_2()
        .text_color(cx.theme().foreground)
        .child(Icon::new(icon).size(rems(1.25)))
        .child(
            div()
                .flex_1()
                .text_xl()
                .font_weight(FontWeight::MEDIUM)
                .child(title.into()),
        )
}

/// A full-width ghost button with its content at the leading edge, for
/// rails and pick lists (a Button centres its own label).
pub fn row_button(id: impl Into<ElementId>, label: impl Into<SharedString>, icon: Option<AppIcon>) -> Button {
    Button::new(id).ghost().w_full().child(
        h_flex()
            .w_full()
            .gap_2()
            .when_some(icon, |this, icon| this.child(Icon::new(icon).small()))
            .child(div().text_sm().text_ellipsis().whitespace_nowrap().child(label.into())),
    )
}

/// Format an ISO timestamp as local "YYYY-MM-DD HH:MM", or the input when it
/// does not parse.
pub fn local_time(iso: &str) -> String {
    chrono::DateTime::parse_from_rfc3339(iso)
        .map(|time| time.with_timezone(&chrono::Local).format("%Y-%m-%d %H:%M").to_string())
        .unwrap_or_else(|_| iso.to_owned())
}

/// A human file size: "512 B", "3.4 KB", "12.0 MB".
pub fn file_size(bytes: u64) -> String {
    const UNITS: [&str; 4] = ["KB", "MB", "GB", "TB"];
    if bytes < 1024 {
        return format!("{bytes} B");
    }
    let mut value = bytes as f64 / 1024.0;
    let mut unit = 0;
    while value >= 1024.0 && unit < UNITS.len() - 1 {
        value /= 1024.0;
        unit += 1;
    }
    format!("{value:.1} {}", UNITS[unit])
}

#[cfg(test)]
mod tests {
    use super::file_size;

    #[test]
    fn file_sizes_read_naturally() {
        assert_eq!(file_size(0), "0 B");
        assert_eq!(file_size(1023), "1023 B");
        assert_eq!(file_size(1536), "1.5 KB");
        assert_eq!(file_size(5 * 1024 * 1024), "5.0 MB");
    }
}
