//! Recurring-schedule expressions, a port of webui/web/src/lib/cron.js.
//!
//! The broker treats `cronExpr` as an opaque robfig/cron v3 string (five
//! fields, no seconds) and validates it. It honours a leading
//! `CRON_TZ=<IANA>` token; guided mode emits one so a schedule's time means
//! the creator's wall clock rather than the broker's UTC.

const DAY_NAMES: [&str; 7] = ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"];
const WEEKDAY_PRESET: [u8; 5] = [1, 2, 3, 4, 5];

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Recurrence {
    Minutes { interval: u32 },
    Hourly { interval: u32, minute: u8 },
    Daily { hour: u8, minute: u8 },
    Weekly { hour: u8, minute: u8, weekdays: Vec<u8> },
    Monthly { hour: u8, minute: u8, dom: u8 },
}

/// The machine's IANA zone, or "" when it can't be determined.
pub fn local_tz() -> String {
    iana_time_zone::get_timezone().unwrap_or_default()
}

fn split_tz(expr: &str) -> (&str, &str) {
    for prefix in ["CRON_TZ=", "TZ="] {
        if let Some(rest) = expr.strip_prefix(prefix)
            && let Some((tz, fields)) = rest.split_once(char::is_whitespace)
            && !tz.is_empty()
            && !fields.trim().is_empty()
        {
            return (tz, fields.trim_start());
        }
    }
    ("", expr)
}

fn build_fields(recurrence: &Recurrence) -> String {
    match recurrence {
        Recurrence::Minutes { interval } => {
            if *interval <= 1 {
                "* * * * *".into()
            } else {
                format!("*/{interval} * * * *")
            }
        }
        Recurrence::Hourly { interval, minute } => {
            if *interval <= 1 {
                format!("{minute} * * * *")
            } else {
                format!("{minute} */{interval} * * *")
            }
        }
        Recurrence::Daily { hour, minute } => format!("{minute} {hour} * * *"),
        Recurrence::Weekly { hour, minute, weekdays } => {
            let mut days = weekdays.clone();
            days.sort_unstable();
            days.dedup();
            let days = if days.is_empty() {
                "*".to_owned()
            } else {
                days.iter().map(u8::to_string).collect::<Vec<_>>().join(",")
            };
            format!("{minute} {hour} * * {days}")
        }
        Recurrence::Monthly { hour, minute, dom } => format!("{minute} {hour} {dom} * *"),
    }
}

/// Build a cron string. A non-empty `tz` prefixes `CRON_TZ=<tz>`.
pub fn build_cron(recurrence: &Recurrence, tz: &str) -> String {
    let fields = build_fields(recurrence);
    if tz.is_empty() {
        fields
    } else {
        format!("CRON_TZ={tz} {fields}")
    }
}

fn number(field: &str) -> Option<u32> {
    (!field.is_empty() && field.bytes().all(|b| b.is_ascii_digit()))
        .then(|| field.parse().ok())
        .flatten()
}

fn step(field: &str) -> Option<u32> {
    number(field.strip_prefix("*/")?)
}

/// Read back the shapes [`build_cron`] emits; anything else is `None` and
/// is shown as a custom expression.
pub fn parse_cron(expr: &str) -> Option<Recurrence> {
    let (_, rest) = split_tz(expr.trim());
    let fields: Vec<&str> = rest.split_whitespace().collect();
    let [min, hr, dom, mon, dow] = fields.as_slice() else {
        return None;
    };
    if *mon != "*" {
        return None;
    }
    if *min == "*" && *hr == "*" && *dom == "*" && *dow == "*" {
        return Some(Recurrence::Minutes { interval: 1 });
    }
    if let Some(interval) = step(min)
        && *hr == "*"
        && *dom == "*"
        && *dow == "*"
    {
        return Some(Recurrence::Minutes { interval });
    }
    let minute = number(min).filter(|m| *m <= 59).map(|m| m as u8);
    if let Some(minute) = minute
        && *dom == "*"
        && *dow == "*"
    {
        if let Some(interval) = step(hr) {
            return Some(Recurrence::Hourly { interval, minute });
        }
        if *hr == "*" {
            return Some(Recurrence::Hourly { interval: 1, minute });
        }
    }
    let minute = minute?;
    let hour = number(hr).filter(|h| *h <= 23)? as u8;
    if *dom == "*" && *dow == "*" {
        return Some(Recurrence::Daily { hour, minute });
    }
    if *dom == "*" {
        let mut weekdays = Vec::new();
        for part in dow.split(',') {
            weekdays.push(number(part).filter(|d| *d <= 6)? as u8);
        }
        weekdays.sort_unstable();
        weekdays.dedup();
        return Some(Recurrence::Weekly { hour, minute, weekdays });
    }
    let dom = number(dom).filter(|d| (1..=31).contains(d))? as u8;
    (*dow == "*").then_some(Recurrence::Monthly { hour, minute, dom })
}

fn hhmm(hour: u8, minute: u8) -> String {
    format!("{hour:02}:{minute:02}")
}

/// A plain-language description, naming the zone when it is not the
/// viewer's own.
pub fn describe_cron(expr: &str) -> String {
    describe_cron_in(expr, &local_tz())
}

pub fn describe_cron_in(expr: &str, viewer_tz: &str) -> String {
    let Some(recurrence) = parse_cron(expr) else {
        return format!("Custom ({expr})");
    };
    let base = match &recurrence {
        Recurrence::Minutes { interval: 1 } => "Every minute".to_owned(),
        Recurrence::Minutes { interval } => format!("Every {interval} minutes"),
        Recurrence::Hourly { interval: 1, minute } => format!("Hourly at :{minute:02}"),
        Recurrence::Hourly { interval, minute } => {
            format!("Every {interval} hours at :{minute:02}")
        }
        Recurrence::Daily { hour, minute } => format!("Every day at {}", hhmm(*hour, *minute)),
        Recurrence::Weekly { hour, minute, weekdays } => {
            if weekdays.as_slice() == WEEKDAY_PRESET {
                format!("Every weekday at {}", hhmm(*hour, *minute))
            } else {
                let names: Vec<_> = weekdays.iter().map(|d| DAY_NAMES[*d as usize]).collect();
                format!("Every {} at {}", names.join(", "), hhmm(*hour, *minute))
            }
        }
        Recurrence::Monthly { hour, minute, dom } => {
            format!("Monthly on day {dom} at {}", hhmm(*hour, *minute))
        }
    };
    let (tz, _) = split_tz(expr.trim());
    if !tz.is_empty() && tz != viewer_tz {
        format!("{base} ({tz})")
    } else {
        base
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn build_and_parse_round_trip() {
        let cases = [
            Recurrence::Minutes { interval: 1 },
            Recurrence::Minutes { interval: 15 },
            Recurrence::Hourly { interval: 1, minute: 5 },
            Recurrence::Hourly { interval: 3, minute: 0 },
            Recurrence::Daily { hour: 9, minute: 30 },
            Recurrence::Weekly {
                hour: 8,
                minute: 0,
                weekdays: vec![1, 3, 5],
            },
            Recurrence::Monthly {
                hour: 6,
                minute: 45,
                dom: 28,
            },
        ];
        for case in cases {
            let expr = build_cron(&case, "Europe/Vienna");
            assert!(expr.starts_with("CRON_TZ=Europe/Vienna "));
            assert_eq!(parse_cron(&expr), Some(case));
        }
    }

    #[test]
    fn weekdays_are_sorted_and_deduplicated() {
        let expr = build_cron(
            &Recurrence::Weekly {
                hour: 7,
                minute: 0,
                weekdays: vec![5, 1, 1, 3],
            },
            "",
        );
        assert_eq!(expr, "0 7 * * 1,3,5");
    }

    #[test]
    fn descriptions_match_the_web_console() {
        assert_eq!(describe_cron_in("0 9 * * 1,2,3,4,5", ""), "Every weekday at 09:00");
        assert_eq!(describe_cron_in("*/10 * * * *", ""), "Every 10 minutes");
        assert_eq!(
            describe_cron_in("CRON_TZ=Asia/Kolkata 30 9 * * *", "Europe/Vienna"),
            "Every day at 09:30 (Asia/Kolkata)"
        );
        assert_eq!(
            describe_cron_in("CRON_TZ=Europe/Vienna 30 9 * * *", "Europe/Vienna"),
            "Every day at 09:30"
        );
        assert_eq!(describe_cron_in("0 0 1 1 *", ""), "Custom (0 0 1 1 *)");
        assert_eq!(describe_cron_in("", ""), "Custom ()");
    }

    #[test]
    fn rejects_out_of_range_and_bare_tokens() {
        assert_eq!(parse_cron("61 * * * *"), None);
        assert_eq!(parse_cron("0 0 * * 7"), None);
        assert_eq!(parse_cron("CRON_TZ=UTC"), None);
    }
}
