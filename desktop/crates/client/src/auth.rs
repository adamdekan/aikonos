//! OpenID Connect sign-in for a native app (RFC 8252): the system browser
//! opens the identity provider, which redirects to a one-shot listener on
//! the loopback interface; the code is exchanged with PKCE (S256).
//!
//! Tokens are held in memory only and never written to disk, matching the
//! web console, whose tokens live in sessionStorage and end with the tab.

use std::net::Ipv4Addr;
use std::time::{Duration, SystemTime};

use base64::Engine as _;
use base64::engine::general_purpose::URL_SAFE_NO_PAD;
use rand::RngCore as _;
use serde::Deserialize;
use sha2::{Digest as _, Sha256};
use tokio::io::{AsyncReadExt as _, AsyncWriteExt as _};
use tokio::net::{TcpListener, TcpStream};
use url::Url;

use crate::connection::{AUTH_TIMEOUT, read_text};
use crate::error::{ApiError, ApiResult};
use crate::server::{OidcSettings, TokenKind};

/// How long the loopback listener waits for the browser to come back.
pub const SIGN_IN_TIMEOUT: Duration = Duration::from_secs(300);
/// The identity provider's answers are small JSON documents.
const PROVIDER_BODY_LIMIT: usize = 1024 * 1024;
/// The longest token lifetime believed; a larger `expires_in` is clamped.
const MAX_TOKEN_LIFETIME: u64 = 24 * 60 * 60;
const CALLBACK_PATH: &str = "/callback";

/// The endpoints published at `<authority>/.well-known/openid-configuration`.
#[derive(Debug, Clone, Deserialize, PartialEq, Eq)]
pub struct ProviderMetadata {
    pub issuer: String,
    pub authorization_endpoint: String,
    pub token_endpoint: String,
    #[serde(default)]
    pub end_session_endpoint: Option<String>,
    #[serde(default)]
    pub revocation_endpoint: Option<String>,
}

pub async fn discover_provider(http: &reqwest::Client, settings: &OidcSettings) -> ApiResult<ProviderMetadata> {
    let url = format!("{}/.well-known/openid-configuration", settings.authority);
    let response = http.get(&url).timeout(AUTH_TIMEOUT).send().await.map_err(|err| {
        ApiError::SignIn(format!(
            "Can't reach the sign-in service at {}: {}",
            settings.authority,
            ApiError::transport(err)
        ))
    })?;
    if !response.status().is_success() {
        return Err(ApiError::SignIn(format!(
            "The sign-in service at {} answered {}.",
            settings.authority,
            response.status().as_u16()
        )));
    }
    let text = read_text(response, PROVIDER_BODY_LIMIT).await?;
    let metadata: ProviderMetadata =
        serde_json::from_str(&text).map_err(|err| ApiError::Decode(format!("openid-configuration: {err}")))?;
    validate_provider(settings, &metadata)?;
    Ok(metadata)
}

/// The published metadata must be the configured provider's own: its
/// issuer is the authority, and every endpoint the code, verifier or
/// refresh token goes to is on the authority's scheme and host. Otherwise a
/// rewritten discovery document could send them anywhere.
fn validate_provider(settings: &OidcSettings, metadata: &ProviderMetadata) -> ApiResult<()> {
    let refuse = |what: &str| {
        Err(ApiError::SignIn(format!(
            "The sign-in service at {} published {what}, so sign-in was stopped. Ask your administrator to check \
             AIKONOS_WEBUI_OIDC_AUTHORITY.",
            settings.authority
        )))
    };
    if metadata.issuer.trim_end_matches('/') != settings.authority {
        return refuse("another issuer");
    }
    let Ok(authority) = Url::parse(&settings.authority) else {
        return refuse("an invalid address");
    };
    let endpoints = [
        Some(&metadata.authorization_endpoint),
        Some(&metadata.token_endpoint),
        metadata.end_session_endpoint.as_ref(),
        metadata.revocation_endpoint.as_ref(),
    ];
    for endpoint in endpoints.into_iter().flatten() {
        let same_site =
            Url::parse(endpoint).is_ok_and(|url| url.scheme() == authority.scheme() && url.host() == authority.host());
        if !same_site {
            return refuse("an endpoint on another host");
        }
    }
    Ok(())
}

/// The person the identity provider says signed in. Read from the ID token
/// for display only; the gateway verifies the bearer on every call.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Profile {
    pub sub: String,
    /// The email, or the closest stand-in the provider sent (Entra personal
    /// accounts carry no `email` claim), as the web console resolves it.
    pub email: String,
    pub name: Option<String>,
}

impl Profile {
    /// The `name` claim, else the email's local part capitalized — the web
    /// console's `displayName`.
    pub fn display_name(&self) -> String {
        if let Some(name) = self.name.as_deref().filter(|n| !n.trim().is_empty()) {
            return name.to_owned();
        }
        let local = self.email.split('@').next().unwrap_or_default();
        let mut chars = local.chars();
        match chars.next() {
            Some(first) => first.to_uppercase().chain(chars).collect(),
            None => String::new(),
        }
    }
}

/// Decode the claims of a JWT without verifying it.
pub fn jwt_claims(token: &str) -> Option<serde_json::Value> {
    let payload = token.split('.').nth(1)?;
    let bytes = URL_SAFE_NO_PAD.decode(payload.trim_end_matches('=')).ok()?;
    serde_json::from_slice(&bytes).ok()
}

pub fn profile_from_claims(claims: &serde_json::Value) -> Profile {
    let text = |key: &str| {
        claims
            .get(key)
            .and_then(serde_json::Value::as_str)
            .map(str::to_owned)
            .filter(|value| !value.is_empty())
    };
    let sub = text("sub").unwrap_or_default();
    Profile {
        email: text("email")
            .or_else(|| text("preferred_username"))
            .or_else(|| text("name"))
            .unwrap_or_else(|| sub.clone()),
        name: text("name"),
        sub,
    }
}

#[derive(Clone)]
pub struct TokenSet {
    pub access_token: String,
    pub id_token: Option<String>,
    pub refresh_token: Option<String>,
    pub expires_at: SystemTime,
}

/// Never prints the tokens, so a stray `{:?}` cannot leak them into a log.
impl std::fmt::Debug for TokenSet {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("TokenSet")
            .field("access_token", &"[REDACTED]")
            .field("id_token", &self.id_token.as_ref().map(|_| "[REDACTED]"))
            .field("refresh_token", &self.refresh_token.as_ref().map(|_| "[REDACTED]"))
            .field("expires_at", &self.expires_at)
            .finish()
    }
}

impl TokenSet {
    pub fn bearer(&self, kind: TokenKind) -> Option<&str> {
        match kind {
            TokenKind::Access => Some(self.access_token.as_str()),
            TokenKind::Id => self.id_token.as_deref(),
        }
    }

    /// Whether the token expires within `margin`.
    pub fn expires_within(&self, margin: Duration) -> bool {
        match self.expires_at.duration_since(SystemTime::now()) {
            Ok(left) => left <= margin,
            Err(_) => true,
        }
    }

    pub fn profile(&self) -> Profile {
        self.id_token
            .as_deref()
            .or(Some(self.access_token.as_str()))
            .and_then(jwt_claims)
            .map(|claims| profile_from_claims(&claims))
            .unwrap_or_default()
    }
}

#[derive(Deserialize)]
struct TokenResponse {
    access_token: String,
    #[serde(default)]
    id_token: Option<String>,
    #[serde(default)]
    refresh_token: Option<String>,
    #[serde(default)]
    expires_in: Option<u64>,
}

#[derive(Deserialize)]
struct TokenError {
    error: String,
    #[serde(default)]
    error_description: Option<String>,
}

async fn token_request(
    http: &reqwest::Client,
    provider: &ProviderMetadata,
    form: &[(&str, &str)],
    previous_refresh: Option<&str>,
) -> ApiResult<TokenSet> {
    let response = http
        .post(&provider.token_endpoint)
        .form(form)
        .timeout(AUTH_TIMEOUT)
        .send()
        .await
        .map_err(ApiError::transport)?;
    let status = response.status();
    let text = read_text(response, PROVIDER_BODY_LIMIT).await?;
    if !status.is_success() {
        let detail = serde_json::from_str::<TokenError>(&text)
            .map(|err| err.error_description.unwrap_or(err.error))
            .unwrap_or_else(|_| format!("status {}", status.as_u16()));
        return Err(if status.as_u16() == 400 || status.as_u16() == 401 {
            // invalid_grant: the refresh token or code is dead.
            ApiError::Unauthorized
        } else {
            ApiError::SignIn(format!("The sign-in service refused the request: {detail}"))
        });
    }
    let body: TokenResponse =
        serde_json::from_str(&text).map_err(|err| ApiError::Decode(format!("token response: {err}")))?;
    Ok(TokenSet {
        access_token: body.access_token,
        id_token: body.id_token,
        // Providers that do not rotate refresh tokens omit it on refresh.
        refresh_token: body.refresh_token.or_else(|| previous_refresh.map(str::to_owned)),
        expires_at: expiry_from(body.expires_in),
    })
}

/// When a token with this `expires_in` runs out. Clamped, so a huge value
/// from the provider cannot overflow the clock.
fn expiry_from(expires_in: Option<u64>) -> SystemTime {
    SystemTime::now() + Duration::from_secs(expires_in.unwrap_or(300).min(MAX_TOKEN_LIFETIME))
}

/// Trade a refresh token for a fresh token set.
pub async fn refresh(
    http: &reqwest::Client,
    provider: &ProviderMetadata,
    settings: &OidcSettings,
    refresh_token: &str,
) -> ApiResult<TokenSet> {
    token_request(
        http,
        provider,
        &[
            ("grant_type", "refresh_token"),
            ("refresh_token", refresh_token),
            ("client_id", &settings.client_id),
        ],
        Some(refresh_token),
    )
    .await
}

/// End the session at the identity provider, best effort. Keycloak accepts a
/// refresh token at its end-session endpoint from a public client, which
/// ends the SSO session the same way the web console's sign-out redirect
/// does; providers that publish a revocation endpoint also get the token
/// revoked. Failures are ignored: local tokens are dropped regardless.
pub async fn end_session(
    http: &reqwest::Client,
    provider: &ProviderMetadata,
    settings: &OidcSettings,
    tokens: &TokenSet,
) {
    let Some(refresh_token) = tokens.refresh_token.as_deref() else {
        return;
    };
    if let Some(endpoint) = provider.end_session_endpoint.as_deref() {
        let _ = http
            .post(endpoint)
            .form(&[
                ("client_id", settings.client_id.as_str()),
                ("refresh_token", refresh_token),
            ])
            .timeout(AUTH_TIMEOUT)
            .send()
            .await;
    }
    if let Some(endpoint) = provider.revocation_endpoint.as_deref() {
        let _ = http
            .post(endpoint)
            .form(&[
                ("client_id", settings.client_id.as_str()),
                ("token", refresh_token),
                ("token_type_hint", "refresh_token"),
            ])
            .timeout(AUTH_TIMEOUT)
            .send()
            .await;
    }
}

fn random_token(bytes: usize) -> String {
    let mut buf = vec![0u8; bytes];
    rand::rng().fill_bytes(&mut buf);
    URL_SAFE_NO_PAD.encode(buf)
}

pub fn pkce_challenge(verifier: &str) -> String {
    URL_SAFE_NO_PAD.encode(Sha256::digest(verifier.as_bytes()))
}

/// A sign-in waiting for the browser to return to the loopback listener.
pub struct PendingSignIn {
    listener: TcpListener,
    authorize_url: Url,
    redirect_uri: String,
    verifier: String,
    state: String,
    nonce: String,
}

impl PendingSignIn {
    /// Bind the loopback listener and build the authorization URL. The port
    /// is chosen by the OS; RFC 8252 §7.3 has the provider ignore it when
    /// matching the registered `http://127.0.0.1/callback`.
    pub async fn start(provider: &ProviderMetadata, settings: &OidcSettings) -> ApiResult<Self> {
        let listener = TcpListener::bind((Ipv4Addr::LOCALHOST, 0))
            .await
            .map_err(|err| ApiError::SignIn(format!("Can't open the sign-in listener: {err}")))?;
        let port = listener
            .local_addr()
            .map_err(|err| ApiError::SignIn(format!("Can't open the sign-in listener: {err}")))?
            .port();
        let redirect_uri = format!("http://127.0.0.1:{port}{CALLBACK_PATH}");
        let verifier = random_token(32);
        let state = random_token(16);
        let nonce = random_token(16);
        let mut authorize_url = Url::parse(&provider.authorization_endpoint)
            .map_err(|_| ApiError::SignIn("The sign-in service published an invalid address.".into()))?;
        authorize_url
            .query_pairs_mut()
            .append_pair("response_type", "code")
            .append_pair("client_id", &settings.client_id)
            .append_pair("redirect_uri", &redirect_uri)
            .append_pair("scope", &settings.scope)
            .append_pair("state", &state)
            .append_pair("nonce", &nonce)
            .append_pair("code_challenge", &pkce_challenge(&verifier))
            .append_pair("code_challenge_method", "S256");
        Ok(Self {
            listener,
            authorize_url,
            redirect_uri,
            verifier,
            state,
            nonce,
        })
    }

    /// The page to open in the system browser.
    pub fn authorize_url(&self) -> &Url {
        &self.authorize_url
    }

    /// Wait for the redirect, then exchange its code for tokens. Requests
    /// for other paths, or carrying another `state`, are answered and
    /// ignored, so nothing else on the machine can end the sign-in early.
    pub async fn finish(
        self,
        http: &reqwest::Client,
        provider: &ProviderMetadata,
        settings: &OidcSettings,
    ) -> ApiResult<TokenSet> {
        let code = tokio::time::timeout(SIGN_IN_TIMEOUT, self.wait_for_code())
            .await
            .map_err(|_| ApiError::SignIn("Sign-in timed out. Try again.".into()))??;
        let tokens = token_request(
            http,
            provider,
            &[
                ("grant_type", "authorization_code"),
                ("code", &code),
                ("redirect_uri", &self.redirect_uri),
                ("client_id", &settings.client_id),
                ("code_verifier", &self.verifier),
            ],
            None,
        )
        .await
        .map_err(|err| match err {
            ApiError::Unauthorized => {
                ApiError::SignIn("The sign-in service rejected the sign-in code. Try again.".into())
            }
            other => other,
        })?;
        check_nonce(&tokens, &self.nonce)?;
        Ok(tokens)
    }

    async fn wait_for_code(&self) -> ApiResult<String> {
        loop {
            let (mut stream, _) = self
                .listener
                .accept()
                .await
                .map_err(|err| ApiError::SignIn(format!("Sign-in listener failed: {err}")))?;
            let Some(target) = read_request_target(&mut stream).await else {
                continue;
            };
            match parse_callback(&target, &self.state) {
                Callback::Code(code) => {
                    respond(&mut stream, 200, &callback_page(true, "You're signed in.")).await;
                    return Ok(code);
                }
                Callback::Denied(reason) => {
                    respond(&mut stream, 200, &callback_page(false, &reason)).await;
                    return Err(ApiError::SignIn(format!("Sign-in was not completed: {reason}")));
                }
                Callback::Ignore(status) => {
                    respond(
                        &mut stream,
                        status,
                        &callback_page(false, "This page is not part of the sign-in."),
                    )
                    .await;
                }
            }
        }
    }
}

/// An ID token answering this sign-in carries the nonce it was asked with;
/// one minted for another sign-in is refused.
fn check_nonce(tokens: &TokenSet, nonce: &str) -> ApiResult<()> {
    let Some(id_token) = tokens.id_token.as_deref() else {
        return Ok(());
    };
    let sent_back = jwt_claims(id_token).and_then(|claims| claims.get("nonce")?.as_str().map(str::to_owned));
    if sent_back.as_deref() != Some(nonce) {
        return Err(ApiError::SignIn(
            "The sign-in service answered for another sign-in. Try again.".into(),
        ));
    }
    Ok(())
}

#[derive(Debug, PartialEq, Eq)]
enum Callback {
    Code(String),
    Denied(String),
    Ignore(u16),
}

fn parse_callback(target: &str, expected_state: &str) -> Callback {
    let Ok(url) = Url::parse(&format!("http://127.0.0.1{target}")) else {
        return Callback::Ignore(400);
    };
    if url.path() != CALLBACK_PATH {
        return Callback::Ignore(404);
    }
    let query = |key: &str| {
        url.query_pairs()
            .find(|(name, _)| name == key)
            .map(|(_, value)| value.into_owned())
    };
    if query("state").as_deref() != Some(expected_state) {
        return Callback::Ignore(400);
    }
    if let Some(error) = query("error") {
        return Callback::Denied(query("error_description").unwrap_or(error));
    }
    match query("code") {
        Some(code) if !code.is_empty() => Callback::Code(code),
        _ => Callback::Ignore(400),
    }
}

/// Read the request line's target ("/callback?code=…") from a loopback
/// connection. Bounded: a request line longer than 16 KiB is dropped.
async fn read_request_target(stream: &mut TcpStream) -> Option<String> {
    let mut buf = Vec::with_capacity(2048);
    let mut chunk = [0u8; 2048];
    let read = async {
        loop {
            let n = stream.read(&mut chunk).await.ok()?;
            if n == 0 {
                return None;
            }
            buf.extend_from_slice(&chunk[..n]);
            if let Some(end) = buf.windows(2).position(|w| w == b"\r\n") {
                let line = String::from_utf8_lossy(&buf[..end]).into_owned();
                let mut parts = line.split(' ');
                let method = parts.next()?;
                let target = parts.next()?;
                return (method == "GET").then(|| target.to_owned());
            }
            if buf.len() > 16 * 1024 {
                return None;
            }
        }
    };
    tokio::time::timeout(Duration::from_secs(10), read).await.ok().flatten()
}

async fn respond(stream: &mut TcpStream, status: u16, body: &str) {
    let reason = match status {
        200 => "OK",
        404 => "Not Found",
        _ => "Bad Request",
    };
    let response = format!(
        "HTTP/1.1 {status} {reason}\r\ncontent-type: text/html; charset=utf-8\r\ncontent-length: {}\r\ncache-control: no-store\r\nconnection: close\r\n\r\n{body}",
        body.len()
    );
    let _ = stream.write_all(response.as_bytes()).await;
    let _ = stream.shutdown().await;
}

fn html_escape(text: &str) -> String {
    text.replace('&', "&amp;")
        .replace('<', "&lt;")
        .replace('>', "&gt;")
        .replace('"', "&quot;")
}

/// The page the browser shows after redirecting back. Colors are the web
/// console's dark tokens (webui/web/src/styles/tokens.css).
fn callback_page(ok: bool, message: &str) -> String {
    let detail = if ok {
        "You can close this tab and return to aikonOS."
    } else {
        "Return to aikonOS and try again."
    };
    format!(
        "<!doctype html><html lang=\"en\"><head><meta charset=\"utf-8\"><title>aikonOS</title>\
<style>body{{margin:0;height:100vh;display:flex;align-items:center;justify-content:center;\
background:#2b2b2b;color:#f0eee9;font-family:Inter,'Segoe UI',sans-serif}}\
main{{background:#323232;border:1px solid #3a3a3a;border-radius:12px;padding:32px 40px;text-align:center}}\
h1{{font-size:20px;font-weight:600;margin:0 0 8px}}p{{color:#b9b6bf;margin:0;font-size:14px}}\
.mark{{color:#8d90d8;font-size:32px;margin-bottom:12px}}</style></head>\
<body><main><div class=\"mark\">{}</div><h1>{}</h1><p>{}</p></main></body></html>",
        if ok { "&#10035;" } else { "&#9888;" },
        html_escape(message),
        detail
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn pkce_challenge_matches_rfc7636_example() {
        // RFC 7636 appendix B.
        assert_eq!(
            pkce_challenge("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"),
            "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
        );
    }

    #[test]
    fn callback_requires_path_and_state() {
        assert_eq!(
            parse_callback("/callback?code=abc&state=s1", "s1"),
            Callback::Code("abc".into())
        );
        assert_eq!(parse_callback("/favicon.ico", "s1"), Callback::Ignore(404));
        assert_eq!(
            parse_callback("/callback?code=abc&state=other", "s1"),
            Callback::Ignore(400)
        );
        assert_eq!(
            parse_callback(
                "/callback?error=access_denied&error_description=User%20cancelled&state=s1",
                "s1"
            ),
            Callback::Denied("User cancelled".into())
        );
    }

    #[test]
    fn profile_prefers_email_then_username() {
        let profile = profile_from_claims(&serde_json::json!({
            "sub": "u-1", "preferred_username": "alice@example.com"
        }));
        assert_eq!(profile.email, "alice@example.com");
        assert_eq!(profile.display_name(), "Alice");
        let named = profile_from_claims(&serde_json::json!({
            "sub": "u-2", "email": "bob@example.com", "name": "Bob Builder"
        }));
        assert_eq!(named.display_name(), "Bob Builder");
    }

    #[test]
    fn jwt_claims_decodes_the_payload() {
        let payload = URL_SAFE_NO_PAD.encode(br#"{"sub":"x","email":"e@x.org"}"#);
        let token = format!("h.{payload}.s");
        assert_eq!(jwt_claims(&token).unwrap()["email"], "e@x.org");
    }

    fn settings(authority: &str) -> OidcSettings {
        OidcSettings {
            authority: authority.into(),
            client_id: "aikonos-desktop".into(),
            scope: "openid profile".into(),
            token_kind: TokenKind::Access,
        }
    }

    fn metadata(issuer: &str, token_endpoint: &str) -> ProviderMetadata {
        ProviderMetadata {
            issuer: issuer.into(),
            authorization_endpoint: "https://id.example.org/realms/a/auth".into(),
            token_endpoint: token_endpoint.into(),
            end_session_endpoint: None,
            revocation_endpoint: None,
        }
    }

    #[test]
    fn provider_metadata_must_belong_to_the_authority() {
        let settings = settings("https://id.example.org/realms/a");
        assert!(
            validate_provider(
                &settings,
                &metadata(
                    "https://id.example.org/realms/a/",
                    "https://id.example.org/realms/a/token"
                )
            )
            .is_ok()
        );
        // A rewritten document: another issuer, or the code sent elsewhere.
        assert!(
            validate_provider(
                &settings,
                &metadata(
                    "https://id.example.org/realms/b",
                    "https://id.example.org/realms/a/token"
                )
            )
            .is_err()
        );
        assert!(
            validate_provider(
                &settings,
                &metadata("https://id.example.org/realms/a", "https://attacker.example/token")
            )
            .is_err()
        );
        assert!(
            validate_provider(
                &settings,
                &metadata(
                    "https://id.example.org/realms/a",
                    "http://id.example.org/realms/a/token"
                )
            )
            .is_err()
        );
        let mut logout_elsewhere = metadata(
            "https://id.example.org/realms/a",
            "https://id.example.org/realms/a/token",
        );
        logout_elsewhere.end_session_endpoint = Some("https://attacker.example/logout".into());
        assert!(validate_provider(&settings, &logout_elsewhere).is_err());
    }

    #[test]
    fn a_huge_lifetime_does_not_overflow_the_clock() {
        let expiry = expiry_from(Some(u64::MAX));
        assert!(expiry <= SystemTime::now() + Duration::from_secs(MAX_TOKEN_LIFETIME));
        assert!(expiry_from(None) > SystemTime::now());
    }

    #[test]
    fn token_sets_never_print_their_tokens() {
        let tokens = TokenSet {
            access_token: "access-secret".into(),
            id_token: Some("id-secret".into()),
            refresh_token: Some("refresh-secret".into()),
            expires_at: SystemTime::now(),
        };
        let printed = format!("{tokens:?}");
        assert!(!printed.contains("secret"), "{printed}");
        assert!(printed.contains("[REDACTED]"));
    }

    #[test]
    fn the_id_token_must_answer_this_sign_in() {
        let id_token = |claims: serde_json::Value| format!("h.{}.s", URL_SAFE_NO_PAD.encode(claims.to_string()));
        let tokens = |id: Option<String>| TokenSet {
            access_token: "a".into(),
            id_token: id,
            refresh_token: None,
            expires_at: SystemTime::now(),
        };
        assert!(check_nonce(&tokens(Some(id_token(serde_json::json!({"nonce": "n1"})))), "n1").is_ok());
        assert!(check_nonce(&tokens(Some(id_token(serde_json::json!({"nonce": "n2"})))), "n1").is_err());
        assert!(check_nonce(&tokens(Some(id_token(serde_json::json!({})))), "n1").is_err());
        assert!(check_nonce(&tokens(None), "n1").is_ok());
    }

    #[test]
    fn callback_page_escapes_provider_text() {
        assert!(callback_page(false, "<script>").contains("&lt;script&gt;"));
    }

    #[tokio::test]
    async fn loopback_listener_returns_the_code_and_ignores_strays() {
        let provider = ProviderMetadata {
            issuer: "https://id.example.org".into(),
            authorization_endpoint: "https://id.example.org/auth".into(),
            token_endpoint: "https://id.example.org/token".into(),
            end_session_endpoint: None,
            revocation_endpoint: None,
        };
        let settings = OidcSettings {
            authority: "https://id.example.org".into(),
            client_id: "aikonos-desktop".into(),
            scope: "openid profile".into(),
            token_kind: TokenKind::Access,
        };
        let pending = PendingSignIn::start(&provider, &settings).await.unwrap();
        let url = pending.authorize_url().clone();
        let redirect = url
            .query_pairs()
            .find(|(k, _)| k == "redirect_uri")
            .unwrap()
            .1
            .into_owned();
        let state = url.query_pairs().find(|(k, _)| k == "state").unwrap().1.into_owned();
        assert!(redirect.starts_with("http://127.0.0.1:"));

        let client = tokio::spawn(async move {
            let base = redirect.trim_end_matches(CALLBACK_PATH).to_owned();
            let http = reqwest::Client::new();
            let stray = http.get(format!("{base}/favicon.ico")).send().await.unwrap();
            assert_eq!(stray.status().as_u16(), 404);
            let forged = http
                .get(format!("{base}{CALLBACK_PATH}?code=evil&state=wrong"))
                .send()
                .await
                .unwrap();
            assert_eq!(forged.status().as_u16(), 400);
            let ok = http
                .get(format!("{base}{CALLBACK_PATH}?code=the-code&state={state}"))
                .send()
                .await
                .unwrap();
            assert_eq!(ok.status().as_u16(), 200);
        });
        let code = pending.wait_for_code().await.unwrap();
        client.await.unwrap();
        assert_eq!(code, "the-code");
    }
}
