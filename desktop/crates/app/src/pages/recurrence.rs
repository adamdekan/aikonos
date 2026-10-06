//! The guided editor for a recurring schedule (webui RecurrenceSelector.vue):
//! frequency, interval, time, weekdays and day of month, or a raw cron
//! expression under "Advanced". Guided expressions carry the machine's zone
//! as `CRON_TZ`, so 09:00 means 09:00 where the user is.

use aikonos_client::cron::{self, Recurrence};
use gpui_kit::component::button::{Button, ButtonVariants as _};
use gpui_kit::component::input::{Input, InputEvent, InputState};
use gpui_kit::component::select::{Select, SelectEvent, SelectState};
use gpui_kit::component::{ActiveTheme as _, IndexPath, Selectable as _, Sizable as _, h_flex, v_flex};
use gpui_kit::prelude::FluentBuilder as _;
use gpui_kit::*;

const FREQUENCIES: [&str; 5] = ["Every N minutes", "Hourly", "Daily", "Weekly", "Monthly"];
const MINUTE_INTERVALS: [u32; 5] = [1, 5, 10, 15, 30];
const HOUR_INTERVALS: [u32; 6] = [1, 2, 3, 4, 6, 12];
const DAY_LABELS: [&str; 7] = ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"];
/// What a new schedule starts as: daily at 09:00.
pub const DEFAULT_CRON: &str = "0 9 * * *";

#[derive(Clone, Copy, PartialEq, Eq)]
enum Frequency {
    Minutes,
    Hourly,
    Daily,
    Weekly,
    Monthly,
}

impl Frequency {
    fn index(self) -> usize {
        self as usize
    }

    fn from_label(label: &str) -> Self {
        match FREQUENCIES.iter().position(|f| *f == label) {
            Some(0) => Frequency::Minutes,
            Some(1) => Frequency::Hourly,
            Some(3) => Frequency::Weekly,
            Some(4) => Frequency::Monthly,
            _ => Frequency::Daily,
        }
    }
}

pub struct RecurrenceEditor {
    advanced: bool,
    frequency: Frequency,
    interval: u32,
    weekdays: Vec<u8>,
    frequency_select: Entity<SelectState<Vec<&'static str>>>,
    interval_select: Entity<SelectState<Vec<SharedString>>>,
    minute: Entity<InputState>,
    time: Entity<InputState>,
    day: Entity<InputState>,
    raw: Entity<InputState>,
    /// Replaced with the interval options, which change with the frequency.
    _interval_subscription: Option<Subscription>,
    _subscriptions: Vec<Subscription>,
}

impl RecurrenceEditor {
    pub fn new(cron_expr: &str, window: &mut Window, cx: &mut Context<Self>) -> Self {
        let frequency_select = cx.new(|cx| {
            SelectState::new(
                FREQUENCIES.to_vec(),
                Some(IndexPath::new(Frequency::Daily.index())),
                window,
                cx,
            )
        });
        let interval_select = cx.new(|cx| SelectState::new(Vec::<SharedString>::new(), None, window, cx));
        let minute = cx.new(|cx| InputState::new(window, cx).placeholder("Minute"));
        let time = cx.new(|cx| InputState::new(window, cx).placeholder("09:00"));
        let day = cx.new(|cx| InputState::new(window, cx).placeholder("1"));
        let raw = cx.new(|cx| InputState::new(window, cx).placeholder("Cron expression (e.g. 0 9 * * *)"));
        let mut subscriptions = vec![cx.subscribe_in(
            &frequency_select,
            window,
            |this, _, event: &SelectEvent<Vec<&'static str>>, window, cx| {
                let SelectEvent::Confirm(Some(label)) = event else {
                    return;
                };
                this.frequency = Frequency::from_label(label);
                this.interval = 1;
                this.reset_interval_options(window, cx);
                cx.notify();
            },
        )];
        for input in [&minute, &time, &day, &raw] {
            subscriptions.push(cx.subscribe(input, |_, _, event: &InputEvent, cx| {
                if matches!(event, InputEvent::Change) {
                    cx.notify();
                }
            }));
        }
        let mut this = Self {
            advanced: false,
            frequency: Frequency::Daily,
            interval: 1,
            weekdays: vec![1, 2, 3, 4, 5],
            frequency_select,
            interval_select,
            minute,
            time,
            day,
            raw,
            _interval_subscription: None,
            _subscriptions: subscriptions,
        };
        this.seed(cron_expr, window, cx);
        this
    }

    /// Show `cron_expr` in the guided controls, or under Advanced when the
    /// guided controls cannot express it.
    fn seed(&mut self, cron_expr: &str, window: &mut Window, cx: &mut Context<Self>) {
        let set = |input: &Entity<InputState>, value: String, window: &mut Window, cx: &mut Context<Self>| {
            input.update(cx, |input, cx| input.set_value(value, window, cx));
        };
        match cron::parse_cron(cron_expr) {
            Some(recurrence) => {
                self.advanced = false;
                let (frequency, interval, hour, minute, weekdays, dom) = match recurrence {
                    Recurrence::Minutes { interval } => (Frequency::Minutes, interval, 9, 0, None, 1),
                    Recurrence::Hourly { interval, minute } => (Frequency::Hourly, interval, 9, minute, None, 1),
                    Recurrence::Daily { hour, minute } => (Frequency::Daily, 1, hour, minute, None, 1),
                    Recurrence::Weekly { hour, minute, weekdays } => {
                        (Frequency::Weekly, 1, hour, minute, Some(weekdays), 1)
                    }
                    Recurrence::Monthly { hour, minute, dom } => (Frequency::Monthly, 1, hour, minute, None, dom),
                };
                self.frequency = frequency;
                self.interval = interval;
                if let Some(weekdays) = weekdays {
                    self.weekdays = weekdays;
                }
                set(&self.minute, minute.to_string(), window, cx);
                set(&self.time, format!("{hour:02}:{minute:02}"), window, cx);
                set(&self.day, dom.to_string(), window, cx);
                self.frequency_select.update(cx, |select, cx| {
                    select.set_selected_index(Some(IndexPath::new(frequency.index())), window, cx)
                });
                self.reset_interval_options(window, cx);
            }
            None => {
                self.advanced = !cron_expr.is_empty();
                set(&self.raw, cron_expr.to_owned(), window, cx);
                set(&self.time, "09:00".into(), window, cx);
                set(&self.minute, "0".into(), window, cx);
                set(&self.day, "1".into(), window, cx);
                self.reset_interval_options(window, cx);
            }
        }
    }

    fn reset_interval_options(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let options: Vec<SharedString> = match self.frequency {
            Frequency::Minutes => MINUTE_INTERVALS
                .iter()
                .map(|n| format!("Every {n} min").into())
                .collect(),
            Frequency::Hourly => HOUR_INTERVALS.iter().map(|n| format!("Every {n}h").into()).collect(),
            _ => Vec::new(),
        };
        let selected = match self.frequency {
            Frequency::Minutes => MINUTE_INTERVALS.iter().position(|n| *n == self.interval),
            Frequency::Hourly => HOUR_INTERVALS.iter().position(|n| *n == self.interval),
            _ => None,
        };
        self.interval_select = cx.new(|cx| SelectState::new(options, selected.map(IndexPath::new), window, cx));
        self._interval_subscription = Some(cx.subscribe_in(
            &self.interval_select,
            window,
            |this, _, event: &SelectEvent<Vec<SharedString>>, _, cx| {
                let SelectEvent::Confirm(Some(label)) = event else {
                    return;
                };
                this.interval = label
                    .split(|c: char| !c.is_ascii_digit())
                    .find(|part| !part.is_empty())
                    .and_then(|digits| digits.parse().ok())
                    .unwrap_or(1);
                cx.notify();
            },
        ));
    }

    fn number(input: &Entity<InputState>, range: std::ops::RangeInclusive<u32>, fallback: u32, cx: &App) -> u32 {
        input
            .read(cx)
            .value()
            .trim()
            .parse::<u32>()
            .ok()
            .filter(|n| range.contains(n))
            .unwrap_or(fallback)
    }

    fn time_of_day(&self, cx: &App) -> (u8, u8) {
        let value = self.time.read(cx).value();
        let mut parts = value.trim().split(':');
        let hour = parts
            .next()
            .and_then(|h| h.trim().parse::<u8>().ok())
            .filter(|h| *h <= 23)
            .unwrap_or(9);
        let minute = parts
            .next()
            .and_then(|m| m.trim().parse::<u8>().ok())
            .filter(|m| *m <= 59)
            .unwrap_or(0);
        (hour, minute)
    }

    fn recurrence(&self, cx: &App) -> Recurrence {
        let (hour, minute) = self.time_of_day(cx);
        match self.frequency {
            Frequency::Minutes => Recurrence::Minutes {
                interval: self.interval,
            },
            Frequency::Hourly => Recurrence::Hourly {
                interval: self.interval,
                minute: Self::number(&self.minute, 0..=59, 0, cx) as u8,
            },
            Frequency::Daily => Recurrence::Daily { hour, minute },
            Frequency::Weekly => Recurrence::Weekly {
                hour,
                minute,
                weekdays: self.weekdays.clone(),
            },
            Frequency::Monthly => Recurrence::Monthly {
                hour,
                minute,
                dom: Self::number(&self.day, 1..=31, 1, cx) as u8,
            },
        }
    }

    /// The expression to send.
    pub fn cron(&self, cx: &App) -> String {
        if self.advanced {
            self.raw.read(cx).value().trim().to_owned()
        } else {
            cron::build_cron(&self.recurrence(cx), &cron::local_tz())
        }
    }

    fn toggle_advanced(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        if self.advanced {
            let raw = self.raw.read(cx).value().to_string();
            self.seed(&raw, window, cx);
            self.advanced = false;
        } else {
            let expr = self.cron(cx);
            self.raw.update(cx, |raw, cx| raw.set_value(expr, window, cx));
            self.advanced = true;
        }
        cx.notify();
    }

    fn toggle_weekday(&mut self, day: u8, cx: &mut Context<Self>) {
        if let Some(ix) = self.weekdays.iter().position(|d| *d == day) {
            self.weekdays.remove(ix);
        } else {
            self.weekdays.push(day);
            self.weekdays.sort_unstable();
        }
        cx.notify();
    }
}

impl Render for RecurrenceEditor {
    fn render(&mut self, _: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let theme = cx.theme();
        let description = cron::describe_cron(&self.cron(cx));
        let frequency = self.frequency;
        v_flex()
            .gap_2()
            .when(!self.advanced, |this| {
                this.child(
                    h_flex()
                        .gap_2()
                        .flex_wrap()
                        .child(div().w(rems(11.)).child(Select::new(&self.frequency_select).small()))
                        .when(matches!(frequency, Frequency::Minutes | Frequency::Hourly), |this| {
                            this.child(div().w(rems(9.)).child(Select::new(&self.interval_select).small()))
                        })
                        .when(frequency == Frequency::Hourly, |this| {
                            this.child(div().w(rems(5.)).child(Input::new(&self.minute).small()))
                        })
                        .when(
                            matches!(frequency, Frequency::Daily | Frequency::Weekly | Frequency::Monthly),
                            |this| this.child(div().w(rems(5.5)).child(Input::new(&self.time).small())),
                        )
                        .when(frequency == Frequency::Monthly, |this| {
                            this.child(div().w(rems(4.)).child(Input::new(&self.day).small()))
                        }),
                )
                .when(frequency == Frequency::Weekly, |this| {
                    this.child(
                        h_flex()
                            .gap_1()
                            .flex_wrap()
                            .children(DAY_LABELS.iter().enumerate().map(|(day, label)| {
                                let day = day as u8;
                                let on = self.weekdays.contains(&day);
                                Button::new(("weekday", day as usize))
                                    .xsmall()
                                    .outline()
                                    .label(*label)
                                    .selected(on)
                                    .when(on, |this| this.bg(theme.primary).text_color(theme.primary_foreground))
                                    .on_click(cx.listener(move |this, _, _, cx| this.toggle_weekday(day, cx)))
                            }))
                            .child(
                                Button::new("weekday-preset")
                                    .ghost()
                                    .xsmall()
                                    .label("Weekdays")
                                    .on_click(cx.listener(|this, _, _, cx| {
                                        this.weekdays = vec![1, 2, 3, 4, 5];
                                        cx.notify();
                                    })),
                            ),
                    )
                })
            })
            .when(self.advanced, |this| this.child(Input::new(&self.raw).small()))
            .child(
                h_flex()
                    .gap_2()
                    .child(
                        div()
                            .flex_1()
                            .text_xs()
                            .text_color(theme.muted_foreground)
                            .child(description),
                    )
                    .child(
                        Button::new("cron-advanced")
                            .ghost()
                            .xsmall()
                            .label(if self.advanced { "Guided" } else { "Advanced" })
                            .on_click(cx.listener(|this, _, window, cx| this.toggle_advanced(window, cx))),
                    ),
            )
    }
}
