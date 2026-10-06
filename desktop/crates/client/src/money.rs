//! Amounts as the broker stores them: integer micro-units (1 major unit =
//! 1,000,000 micros), currency-agnostic. A port of webui/web/src/lib/money.js.

pub fn to_micros(amount: f64) -> i64 {
    if !amount.is_finite() || amount <= 0.0 {
        return 0;
    }
    (amount * 1_000_000.0).round() as i64
}

pub fn from_micros(micros: i64) -> f64 {
    micros as f64 / 1_000_000.0
}

/// Two decimals, for caps and per-million pricing.
pub fn fmt_amount(micros: i64) -> String {
    format!("{:.2}", from_micros(micros))
}

/// Small amounts without collapsing them to 0.00: a chat session usually
/// costs a few hundredths of a unit.
pub fn fmt_amount_precise(micros: i64) -> String {
    let n = from_micros(micros);
    if n == 0.0 {
        "0".into()
    } else if n < 0.01 {
        format!("{n:.4}")
    } else if n < 1.0 {
        format!("{n:.3}")
    } else {
        format!("{n:.2}")
    }
}

/// Group thousands the way `Number.toLocaleString()` does in en-US.
pub fn fmt_count(n: i64) -> String {
    let digits = n.unsigned_abs().to_string();
    let mut out = String::with_capacity(digits.len() + digits.len() / 3);
    for (ix, ch) in digits.chars().enumerate() {
        if ix > 0 && (digits.len() - ix).is_multiple_of(3) {
            out.push(',');
        }
        out.push(ch);
    }
    if n < 0 { format!("-{out}") } else { out }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn precise_amounts_scale_their_decimals() {
        assert_eq!(fmt_amount_precise(0), "0");
        assert_eq!(fmt_amount_precise(4_200), "0.0042");
        assert_eq!(fmt_amount_precise(123_456), "0.123");
        assert_eq!(fmt_amount_precise(2_500_000), "2.50");
        assert_eq!(fmt_amount(2_500_000), "2.50");
    }

    #[test]
    fn micros_round_and_reject_negatives() {
        assert_eq!(to_micros(0.1 + 0.2), 300_000);
        assert_eq!(to_micros(-1.0), 0);
        assert_eq!(to_micros(f64::NAN), 0);
    }

    #[test]
    fn counts_group_thousands() {
        assert_eq!(fmt_count(0), "0");
        assert_eq!(fmt_count(1234), "1,234");
        assert_eq!(fmt_count(1_234_567), "1,234,567");
    }
}
