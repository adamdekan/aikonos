//! Incremental server-sent-events framing, independent of transport.
//!
//! Follows the WHATWG event-stream rules that matter for the gateway's
//! streams: events end at a blank line, `data:` lines join with `\n`, one
//! leading space after the colon is dropped, `:` lines are comments, and
//! `\n`, `\r\n` and `\r` all end a line.

/// One dispatched event.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct SseEvent {
    /// The `event:` field, empty when the event carried none.
    pub event: String,
    /// The joined `data:` lines.
    pub data: String,
}

/// Feeds bytes in, takes complete events out. Bytes are buffered until a
/// line ends, so a multi-byte character split across reads is reassembled
/// before it is decoded.
#[derive(Debug, Default)]
pub struct SseParser {
    pending: Vec<u8>,
    event: String,
    data: String,
    has_data: bool,
    /// A `\r` ended the previous read; a `\n` starting this read belongs to it.
    last_was_cr: bool,
}

impl SseParser {
    pub fn new() -> Self {
        Self::default()
    }

    /// Append `chunk` and return every event it completed, in order.
    pub fn push(&mut self, chunk: &[u8]) -> Vec<SseEvent> {
        let mut out = Vec::new();
        let mut bytes = chunk;
        if self.last_was_cr && bytes.first() == Some(&b'\n') {
            bytes = &bytes[1..];
        }
        self.last_was_cr = false;
        self.pending.extend_from_slice(bytes);

        let mut start = 0;
        let mut ix = 0;
        while ix < self.pending.len() {
            let byte = self.pending[ix];
            if byte == b'\n' || byte == b'\r' {
                let line = String::from_utf8_lossy(&self.pending[start..ix]).into_owned();
                if let Some(event) = self.line(&line) {
                    out.push(event);
                }
                if byte == b'\r' {
                    if ix + 1 < self.pending.len() {
                        if self.pending[ix + 1] == b'\n' {
                            ix += 1;
                        }
                    } else {
                        self.last_was_cr = true;
                    }
                }
                start = ix + 1;
            }
            ix += 1;
        }
        self.pending.drain(..start);
        out
    }

    fn line(&mut self, line: &str) -> Option<SseEvent> {
        if line.is_empty() {
            return self.dispatch();
        }
        if line.starts_with(':') {
            return None;
        }
        let (field, value) = match line.find(':') {
            Some(colon) => {
                let value = &line[colon + 1..];
                (&line[..colon], value.strip_prefix(' ').unwrap_or(value))
            }
            None => (line, ""),
        };
        match field {
            "event" => self.event = value.to_owned(),
            "data" => {
                if self.has_data {
                    self.data.push('\n');
                }
                self.data.push_str(value);
                self.has_data = true;
            }
            _ => {}
        }
        None
    }

    fn dispatch(&mut self) -> Option<SseEvent> {
        let event = std::mem::take(&mut self.event);
        let data = std::mem::take(&mut self.data);
        let had_data = std::mem::replace(&mut self.has_data, false);
        had_data.then_some(SseEvent { event, data })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn splits_events_on_blank_lines() {
        let mut parser = SseParser::new();
        let events = parser.push(b"data: {\"a\":1}\n\ndata: {\"b\":2}\n\n");
        assert_eq!(events.len(), 2);
        assert_eq!(events[0].data, "{\"a\":1}");
        assert_eq!(events[1].data, "{\"b\":2}");
    }

    #[test]
    fn reassembles_across_reads_including_split_utf8() {
        let mut parser = SseParser::new();
        let text = "data: Grüße\n\n".as_bytes();
        let (head, tail) = text.split_at(9); // inside the two-byte "ü"
        assert!(parser.push(head).is_empty());
        let events = parser.push(tail);
        assert_eq!(
            events,
            vec![SseEvent {
                event: String::new(),
                data: "Grüße".into()
            }]
        );
    }

    #[test]
    fn keeps_event_names_and_joins_data_lines() {
        let mut parser = SseParser::new();
        let events = parser.push(b"event: step\r\ndata: one\r\ndata: two\r\n\r\n");
        assert_eq!(
            events,
            vec![SseEvent {
                event: "step".into(),
                data: "one\ntwo".into()
            }]
        );
    }

    #[test]
    fn ignores_comments_and_events_without_data() {
        let mut parser = SseParser::new();
        assert!(parser.push(b": keepalive\n\nevent: ping\n\n").is_empty());
    }

    #[test]
    fn handles_cr_split_from_lf_across_reads() {
        let mut parser = SseParser::new();
        assert!(parser.push(b"data: x\r").is_empty());
        let events = parser.push(b"\n\r\n");
        assert_eq!(events.len(), 1);
        assert_eq!(events[0].data, "x");
    }
}
