use serde_json::Value;

/// A failed call to the Aikonos server, classified the way the web console
/// classifies it (webui/web/src/api/client.js): a 403 is an answer, not a
/// crash, so callers can render an empty or no-access state.
#[derive(Debug, Clone, thiserror::Error)]
pub enum ApiError {
    /// The gateway refused the caller (HTTP 403). Carries the server's reason
    /// when it sent one.
    #[error("{}", .0.as_deref().unwrap_or("You don't have access to this."))]
    Forbidden(Option<String>),
    /// The bearer was rejected and could not be renewed; the user has to sign
    /// in again.
    #[error("Your session has ended. Sign in again.")]
    Unauthorized,
    /// Any other non-success status. `message` is the server's `error` field
    /// when present, else "request failed (<status>)".
    #[error("{message}")]
    Status { status: u16, message: String, body: Value },
    /// The request never produced a response.
    #[error("Can't reach the server: {0}")]
    Transport(String),
    /// The response arrived but was not what the contract promises.
    #[error("Unexpected response from the server: {0}")]
    Decode(String),
    /// A sign-in step failed before any API call was possible.
    #[error("{0}")]
    SignIn(String),
}

impl ApiError {
    pub fn status(&self) -> Option<u16> {
        match self {
            ApiError::Forbidden(_) => Some(403),
            ApiError::Unauthorized => Some(401),
            ApiError::Status { status, .. } => Some(*status),
            _ => None,
        }
    }

    pub fn is_forbidden(&self) -> bool {
        matches!(self, ApiError::Forbidden(_))
    }

    pub fn is_unauthorized(&self) -> bool {
        matches!(self, ApiError::Unauthorized)
    }

    /// The body of a non-success response, for callers that read extra
    /// fields such as the broker's `suggested_name` on a 409.
    pub fn body(&self) -> Option<&Value> {
        match self {
            ApiError::Status { body, .. } => Some(body),
            _ => None,
        }
    }

    /// reqwest renders the request URL into its errors, and during sign-in
    /// that URL can carry an OAuth `code` or `state`. Keep the kind and the
    /// underlying cause only.
    pub(crate) fn transport(err: reqwest::Error) -> Self {
        let err = err.without_url();
        let mut text = err.to_string();
        let mut source = std::error::Error::source(&err);
        while let Some(inner) = source {
            let inner_text = inner.to_string();
            if !text.contains(&inner_text) {
                text.push_str(": ");
                text.push_str(&inner_text);
            }
            source = inner.source();
        }
        ApiError::Transport(text)
    }

    /// Build the error for a non-success response from its status and body
    /// text, mirroring client.js: `{ error }` is the message when present.
    pub(crate) fn from_response(status: u16, text: &str) -> Self {
        let body: Value = serde_json::from_str(text).unwrap_or(Value::Null);
        let server_message = body.get("error").and_then(Value::as_str).map(str::to_owned);
        match status {
            401 => ApiError::Unauthorized,
            403 => ApiError::Forbidden(server_message),
            _ => ApiError::Status {
                status,
                message: server_message.unwrap_or_else(|| format!("request failed ({status})")),
                body,
            },
        }
    }
}

pub type ApiResult<T> = Result<T, ApiError>;

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn forbidden_keeps_the_server_reason() {
        let err = ApiError::from_response(403, r#"{"error":"no grant"}"#);
        assert!(err.is_forbidden());
        assert_eq!(err.to_string(), "no grant");
    }

    #[test]
    fn status_message_falls_back_to_the_code() {
        let err = ApiError::from_response(502, "gateway down");
        assert_eq!(err.to_string(), "request failed (502)");
        assert_eq!(err.status(), Some(502));
    }

    #[test]
    fn conflict_body_is_kept_for_extra_fields() {
        let err = ApiError::from_response(409, r#"{"error":"taken","suggested_name":"mine-2"}"#);
        assert_eq!(
            err.body().and_then(|b| b.get("suggested_name")).and_then(Value::as_str),
            Some("mine-2")
        );
    }
}
