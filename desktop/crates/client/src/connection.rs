//! An authenticated connection to one aikonOS server: the bearer, its
//! renewal, and the request helpers every endpoint module uses.
//!
//! Paths are the web console's: `/agui` and `/audit/stream` go to the
//! server as they are, everything else under `/api`, which the web server
//! (webui/server.mjs) forwards to the gateway. The desktop client therefore
//! needs nothing a browser does not already have.

use std::sync::Arc;
use std::time::Duration;

use futures::StreamExt as _;
use reqwest::Method;
use serde::Serialize;
use serde::de::DeserializeOwned;
use tokio::sync::{Mutex, mpsc};
use url::Url;

use crate::auth::{self, PendingSignIn, Profile, ProviderMetadata, TokenSet};
use crate::error::{ApiError, ApiResult};
use crate::server::{self, ServerConfig};
use crate::sse::{SseEvent, SseParser};

/// Renew this long before the access token expires, as oidc-client-ts does.
const RENEW_MARGIN: Duration = Duration::from_secs(60);
/// A run keeps the bearer it started with for every broker call it makes
/// (agent-gateway agui.ts), so a stream starts with at least this much left.
const STREAM_MARGIN: Duration = Duration::from_secs(10 * 60);
const PASSTHROUGH: [&str; 2] = ["/agui", "/audit/stream"];

pub fn http_client() -> reqwest::Client {
    reqwest::Client::builder()
        .user_agent(concat!("aikonos-desktop/", env!("CARGO_PKG_VERSION")))
        .connect_timeout(Duration::from_secs(15))
        .build()
        .expect("TLS backend initializes")
}

/// The first half of sign-in: a server located and its identity provider
/// resolved, before the browser round trip.
pub struct SignInStart {
    http: reqwest::Client,
    server: ServerConfig,
    provider: ProviderMetadata,
    pending: PendingSignIn,
}

impl SignInStart {
    /// Read the server's client settings, resolve its identity provider and
    /// open the loopback listener. Open [`SignInStart::authorize_url`] in the
    /// browser, then await [`SignInStart::finish`].
    pub async fn begin(server_input: &str) -> ApiResult<Self> {
        let http = http_client();
        let origin = server::normalize_server_url(server_input)?;
        let server = server::discover(&http, &origin).await?;
        let provider = auth::discover_provider(&http, &server.oidc).await?;
        let pending = PendingSignIn::start(&provider, &server.oidc).await?;
        Ok(Self {
            http,
            server,
            provider,
            pending,
        })
    }

    pub fn server(&self) -> &ServerConfig {
        &self.server
    }

    pub fn authorize_url(&self) -> &Url {
        self.pending.authorize_url()
    }

    pub async fn finish(self) -> ApiResult<Arc<Connection>> {
        let tokens = self
            .pending
            .finish(&self.http, &self.provider, &self.server.oidc)
            .await?;
        if tokens.bearer(self.server.oidc.token_kind).is_none() {
            return Err(ApiError::SignIn(
                "The sign-in service returned no ID token, which this server requires.".into(),
            ));
        }
        Ok(Connection::new(self.http, self.server, self.provider, tokens))
    }
}

pub struct Connection {
    http: reqwest::Client,
    server: ServerConfig,
    provider: ProviderMetadata,
    profile: Profile,
    tokens: Mutex<Option<TokenSet>>,
}

impl Connection {
    fn new(http: reqwest::Client, server: ServerConfig, provider: ProviderMetadata, tokens: TokenSet) -> Arc<Self> {
        let profile = tokens.profile();
        Arc::new(Self {
            http,
            server,
            provider,
            profile,
            tokens: Mutex::new(Some(tokens)),
        })
    }

    pub fn server(&self) -> &ServerConfig {
        &self.server
    }

    pub fn profile(&self) -> &Profile {
        &self.profile
    }

    pub fn http(&self) -> &reqwest::Client {
        &self.http
    }

    /// The bearer for the next call, renewed first when it is about to
    /// expire. Holding the lock across the renewal makes it single-flight.
    pub async fn bearer(&self) -> ApiResult<String> {
        self.bearer_valid_for(RENEW_MARGIN).await
    }

    /// The bearer, renewed first unless it stays valid for `margin`.
    async fn bearer_valid_for(&self, margin: Duration) -> ApiResult<String> {
        let mut guard = self.tokens.lock().await;
        let tokens = guard.as_mut().ok_or(ApiError::Unauthorized)?;
        if tokens.expires_within(margin) {
            // A token that cannot be renewed (no refresh token) is still
            // used while it lasts.
            match self.renew_locked(&mut guard).await {
                Ok(()) => {}
                Err(ApiError::Unauthorized) if margin > RENEW_MARGIN => {
                    return Err(ApiError::Unauthorized);
                }
                Err(err) => return Err(err),
            }
        }
        let tokens = guard.as_ref().ok_or(ApiError::Unauthorized)?;
        tokens
            .bearer(self.server.oidc.token_kind)
            .map(str::to_owned)
            .ok_or(ApiError::Unauthorized)
    }

    /// Renew now if the token is close to expiry. The app calls this on a
    /// timer so an idle window keeps its session alive, like the web
    /// console's silent renew.
    pub async fn keep_alive(&self) -> ApiResult<()> {
        self.bearer().await.map(|_| ())
    }

    async fn force_renew(&self) -> ApiResult<()> {
        let mut guard = self.tokens.lock().await;
        self.renew_locked(&mut guard).await
    }

    async fn renew_locked(&self, guard: &mut Option<TokenSet>) -> ApiResult<()> {
        let refresh_token = guard
            .as_ref()
            .and_then(|tokens| tokens.refresh_token.clone())
            .ok_or(ApiError::Unauthorized)?;
        match auth::refresh(&self.http, &self.provider, &self.server.oidc, &refresh_token).await {
            Ok(tokens) => {
                *guard = Some(tokens);
                Ok(())
            }
            Err(ApiError::Unauthorized) => {
                *guard = None;
                Err(ApiError::Unauthorized)
            }
            Err(other) => Err(other),
        }
    }

    /// Drop the tokens and end the identity-provider session.
    pub async fn sign_out(&self) {
        let tokens = self.tokens.lock().await.take();
        if let Some(tokens) = tokens {
            auth::end_session(&self.http, &self.provider, &self.server.oidc, &tokens).await;
        }
    }

    /// The absolute URL for a console path, with the `/api` prefix the web
    /// client adds (webui/web/src/api/client.js `resolveUrl`).
    pub fn url(&self, path: &str) -> Url {
        let is_passthrough = PASSTHROUGH.iter().any(|prefix| {
            path == *prefix || path.starts_with(&format!("{prefix}?")) || path.starts_with(&format!("{prefix}/"))
        });
        let full = if is_passthrough {
            path.to_owned()
        } else {
            format!("/api{path}")
        };
        self.server
            .origin
            .join(full.trim_start_matches('/'))
            .expect("console paths are relative URLs")
    }

    /// Send a request, renewing the bearer once if the server rejects it.
    async fn send(
        &self,
        method: Method,
        path: &str,
        body: Option<(Vec<u8>, &str)>,
        accept: Option<&str>,
    ) -> ApiResult<reqwest::Response> {
        let mut renewed = false;
        loop {
            let bearer = self.bearer().await?;
            let mut request = self.http.request(method.clone(), self.url(path)).bearer_auth(&bearer);
            if let Some(accept) = accept {
                request = request.header("accept", accept);
            }
            if let Some((bytes, content_type)) = &body {
                request = request.header("content-type", *content_type).body(bytes.clone());
            }
            let response = request.send().await.map_err(ApiError::transport)?;
            if response.status().as_u16() == 401 && !renewed {
                renewed = true;
                self.force_renew().await?;
                continue;
            }
            return Ok(response);
        }
    }

    async fn read_json<T: DeserializeOwned>(response: reqwest::Response) -> ApiResult<T> {
        let status = response.status().as_u16();
        let text = response.text().await.map_err(ApiError::transport)?;
        if !(200..300).contains(&status) {
            return Err(ApiError::from_response(status, &text));
        }
        // An empty success body decodes as `{}` (client.js does the same).
        let text = if text.trim().is_empty() { "{}" } else { text.as_str() };
        serde_json::from_str(text).map_err(|err| ApiError::Decode(err.to_string()))
    }

    pub async fn get<T: DeserializeOwned>(&self, path: &str) -> ApiResult<T> {
        Self::read_json(self.send(Method::GET, path, None, None).await?).await
    }

    pub async fn delete<T: DeserializeOwned>(&self, path: &str) -> ApiResult<T> {
        Self::read_json(self.send(Method::DELETE, path, None, None).await?).await
    }

    pub async fn post<B: Serialize + ?Sized, T: DeserializeOwned>(&self, path: &str, body: &B) -> ApiResult<T> {
        self.with_json(Method::POST, path, body).await
    }

    /// POST with no body and no content type: an empty body labelled JSON
    /// is a 400 at the web server (Fastify `FST_ERR_CTP_EMPTY_JSON_BODY`).
    pub async fn post_empty<T: DeserializeOwned>(&self, path: &str) -> ApiResult<T> {
        Self::read_json(self.send(Method::POST, path, None, None).await?).await
    }

    pub async fn put<B: Serialize + ?Sized, T: DeserializeOwned>(&self, path: &str, body: &B) -> ApiResult<T> {
        self.with_json(Method::PUT, path, body).await
    }

    pub async fn patch<B: Serialize + ?Sized, T: DeserializeOwned>(&self, path: &str, body: &B) -> ApiResult<T> {
        self.with_json(Method::PATCH, path, body).await
    }

    async fn with_json<B: Serialize + ?Sized, T: DeserializeOwned>(
        &self,
        method: Method,
        path: &str,
        body: &B,
    ) -> ApiResult<T> {
        let bytes = serde_json::to_vec(body).map_err(|err| ApiError::Decode(err.to_string()))?;
        Self::read_json(self.send(method, path, Some((bytes, "application/json")), None).await?).await
    }

    /// Send a raw body (a SKILL.md as `text/markdown`, a bundle as
    /// `application/zip`) where JSON encoding would corrupt it.
    pub async fn upload<T: DeserializeOwned>(
        &self,
        method: Method,
        path: &str,
        bytes: Vec<u8>,
        content_type: &str,
    ) -> ApiResult<T> {
        Self::read_json(self.send(method, path, Some((bytes, content_type)), None).await?).await
    }

    /// Fetch an attachment's exact text, unparsed.
    pub async fn download_text(&self, path: &str) -> ApiResult<String> {
        let response = self.send(Method::GET, path, None, None).await?;
        let status = response.status().as_u16();
        let text = response.text().await.map_err(ApiError::transport)?;
        if !(200..300).contains(&status) {
            return Err(ApiError::from_response(status, &text));
        }
        Ok(text)
    }

    /// POST a JSON body and stream the server-sent events of the response
    /// into a channel. The reader stops when the stream ends or the
    /// returned [`EventStream`] is dropped.
    ///
    /// Through the web server a rejected request still answers 200 with
    /// the gateway's JSON error as its body (webui/server.mjs `pipeSSE`),
    /// so the first bytes decide: a real stream starts with `retry:`, a
    /// rejection with `{`.
    pub async fn post_event_stream<B: Serialize + ?Sized>(
        self: &Arc<Self>,
        path: &str,
        body: &B,
    ) -> ApiResult<EventStream> {
        let bytes = serde_json::to_vec(body).map_err(|err| ApiError::Decode(err.to_string()))?;
        let mut renewed = false;
        loop {
            let bearer = self.bearer_valid_for(STREAM_MARGIN).await?;
            let response = self
                .http
                .post(self.url(path))
                .bearer_auth(&bearer)
                .header("content-type", "application/json")
                .header("accept", "text/event-stream")
                .body(bytes.clone())
                .send()
                .await
                .map_err(ApiError::transport)?;
            let status = response.status().as_u16();
            let mut stream = response.bytes_stream();
            let mut head = Vec::new();
            while !head.iter().any(|b: &u8| !b.is_ascii_whitespace()) {
                match stream.next().await {
                    Some(Ok(chunk)) => head.extend_from_slice(&chunk),
                    Some(Err(err)) => return Err(ApiError::transport(err)),
                    None => break,
                }
            }
            let first = head.iter().copied().find(|b| !b.is_ascii_whitespace());
            if !(200..300).contains(&status) || first == Some(b'{') {
                while let Some(chunk) = stream.next().await {
                    head.extend_from_slice(&chunk.map_err(ApiError::transport)?);
                }
                let err = stream_rejection(status, &String::from_utf8_lossy(&head));
                if err.is_unauthorized() && !renewed {
                    renewed = true;
                    self.force_renew().await?;
                    continue;
                }
                return Err(err);
            }
            let (tx, rx) = mpsc::unbounded_channel();
            let task = tokio::spawn(async move {
                let mut parser = SseParser::new();
                for event in parser.push(&head) {
                    if tx.send(StreamItem::Event(event)).is_err() {
                        return;
                    }
                }
                while let Some(chunk) = stream.next().await {
                    match chunk {
                        Ok(bytes) => {
                            for event in parser.push(&bytes) {
                                if tx.send(StreamItem::Event(event)).is_err() {
                                    return;
                                }
                            }
                        }
                        Err(err) => {
                            let _ = tx.send(StreamItem::Failed(ApiError::transport(err)));
                            return;
                        }
                    }
                }
                let _ = tx.send(StreamItem::Ended);
            });
            return Ok(EventStream { rx, task });
        }
    }
}

/// The error a stream request was refused with. The gateway's own errors
/// carry only `{error}`; Fastify's carry `statusCode` and `message`.
fn stream_rejection(status: u16, text: &str) -> ApiError {
    let body: serde_json::Value = serde_json::from_str(text).unwrap_or(serde_json::Value::Null);
    let field = |key: &str| body.get(key).and_then(serde_json::Value::as_str).map(str::to_owned);
    let status = body
        .get("statusCode")
        .and_then(serde_json::Value::as_u64)
        .map(|code| code as u16)
        .filter(|_| (200..300).contains(&status))
        .unwrap_or(status);
    let error = field("error");
    let bearer_rejected = error
        .as_deref()
        .is_some_and(|error| error == "invalid or expired bearer token" || error.starts_with("Authorization: Bearer"));
    if status == 401 || bearer_rejected {
        return ApiError::Unauthorized;
    }
    let message = match (error.as_deref(), field("message")) {
        (Some("gateway_overloaded"), _) => "The server is busy right now. Try again in a moment.".to_owned(),
        // Fastify: `error` is the reason phrase, `message` the detail.
        (_, Some(message)) if body.get("statusCode").is_some() => message,
        (Some(error), _) => error.to_owned(),
        (None, _) if text.trim().is_empty() => "The server closed the stream without answering.".to_owned(),
        (None, _) => text.chars().take(300).collect(),
    };
    if status == 403 {
        return ApiError::Forbidden(Some(message));
    }
    ApiError::Status {
        status: if (200..300).contains(&status) { 502 } else { status },
        message,
        body,
    }
}

#[derive(Debug)]
pub enum StreamItem {
    Event(SseEvent),
    /// The connection dropped mid-stream.
    Failed(ApiError),
    /// The server closed the stream.
    Ended,
}

/// A live server-sent-event response. Dropping it aborts the read.
pub struct EventStream {
    rx: mpsc::UnboundedReceiver<StreamItem>,
    task: tokio::task::JoinHandle<()>,
}

impl EventStream {
    /// The next item; `None` once the reader has stopped.
    pub async fn next(&mut self) -> Option<StreamItem> {
        self.rx.recv().await
    }

    /// A handle that stops the read when the user presses Stop.
    pub fn abort_handle(&self) -> tokio::task::AbortHandle {
        self.task.abort_handle()
    }
}

impl Drop for EventStream {
    fn drop(&mut self) {
        self.task.abort();
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn stream_rejections_keep_the_gateway_reason() {
        let err = stream_rejection(200, r#"{"error":"unknown agent: 42"}"#);
        assert_eq!(err.to_string(), "unknown agent: 42");
        assert!(matches!(
            stream_rejection(200, r#"{"error":"invalid or expired bearer token"}"#),
            ApiError::Unauthorized
        ));
        assert_eq!(
            stream_rejection(200, r#"{"error":"gateway_overloaded"}"#).to_string(),
            "The server is busy right now. Try again in a moment."
        );
        let fastify = stream_rejection(
            200,
            r#"{"statusCode":413,"code":"FST_ERR_CTP_BODY_TOO_LARGE","error":"Payload Too Large","message":"Request body is too large"}"#,
        );
        assert_eq!(fastify.status(), Some(413));
        assert_eq!(fastify.to_string(), "Request body is too large");
    }
}
