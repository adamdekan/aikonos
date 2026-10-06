//! Endpoint wrappers, one module per console page. Each adds methods to
//! [`Connection`](crate::Connection) and mirrors a web client module in
//! webui/web/src/api.

pub mod chat;
pub mod connectors;
pub mod files;
pub mod inbox;
pub mod memory;
pub mod schedules;
pub mod sessions;
pub mod skills;
pub mod workflows;
pub mod workspace;

/// Percent-encode a query or path component exactly as JavaScript's
/// `encodeURIComponent` does, so the gateway sees what the web console sends.
pub fn encode(component: &str) -> String {
    let mut out = String::with_capacity(component.len());
    for byte in component.bytes() {
        match byte {
            b'A'..=b'Z' | b'a'..=b'z' | b'0'..=b'9' | b'-' | b'_' | b'.' | b'!' | b'~' | b'*' | b'\'' | b'(' | b')' => {
                out.push(byte as char)
            }
            _ => out.push_str(&format!("%{byte:02X}")),
        }
    }
    out
}

#[cfg(test)]
mod tests {
    #[test]
    fn encodes_like_encode_uri_component() {
        assert_eq!(super::encode(".agent/Sessions a&b"), ".agent%2FSessions%20a%26b");
        assert_eq!(super::encode("Grüße"), "Gr%C3%BC%C3%9Fe");
        assert_eq!(super::encode("it's (ok)!*~"), "it's%20(ok)!*~");
    }
}
