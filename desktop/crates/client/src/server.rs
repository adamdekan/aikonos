//! Locating an aikonOS server and reading the settings it publishes for
//! clients: which identity provider to sign in with, and which desktop
//! release it expects.

use serde::Deserialize;
use url::Url;

use crate::connection::{AUTH_TIMEOUT, read_text};
use crate::error::{ApiError, ApiResult};

/// The client id a server is assumed to have registered for this app when it
/// does not say otherwise (deploy/compose/keycloak-realm.json).
pub const DEFAULT_DESKTOP_CLIENT_ID: &str = "aikonos-desktop";
const DEFAULT_AUTHORITY: &str = "http://localhost:18080/realms/aikonos";
const DEFAULT_SCOPE: &str = "openid profile";
/// desktop.json and runtime-config.js are a few hundred bytes.
const CONFIG_BODY_LIMIT: usize = 256 * 1024;

/// Which token the gateway accepts as the bearer. Mirrors the web console's
/// `AIKONOS_WEBUI_OIDC_TOKEN`: `access` for Keycloak and Entra apps that
/// expose an API, `id` for Entra deployments that do not.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub enum TokenKind {
    #[default]
    Access,
    Id,
}

impl TokenKind {
    fn parse(value: Option<&str>) -> Self {
        match value.map(str::trim) {
            Some("id") => TokenKind::Id,
            _ => TokenKind::Access,
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct OidcSettings {
    pub authority: String,
    pub client_id: String,
    pub scope: String,
    pub token_kind: TokenKind,
}

/// The desktop build a server advertises. `minimum_version` turns the notice
/// into a requirement: an older client refuses to continue.
#[derive(Debug, Clone, PartialEq, Eq, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct ReleaseInfo {
    pub version: String,
    #[serde(default)]
    pub minimum_version: Option<String>,
    #[serde(default)]
    pub url: Option<String>,
    #[serde(default)]
    pub notes: Option<String>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ServerConfig {
    /// The origin the user connects to: the web console's address.
    pub origin: Url,
    pub oidc: OidcSettings,
    pub release: Option<ReleaseInfo>,
}

impl ServerConfig {
    /// Whether the sign-in code, refresh token or bearers travel
    /// unencrypted over a network, to this server or to its identity
    /// provider. Plain HTTP to the local machine is the development stack.
    pub fn is_insecure(&self) -> bool {
        let plain = |url: &Url| url.scheme() == "http" && !is_loopback(url);
        // The authority parsed when the config was read; fail closed anyway.
        plain(&self.origin) || Url::parse(&self.oidc.authority).map_or(true, |authority| plain(&authority))
    }
}

/// Set to `1` on a PC that must reach a plain-HTTP server on its network.
/// Bearer tokens then travel unencrypted; the sign-in screen says so.
pub const ALLOW_HTTP_ENV: &str = "AIKONOS_DESKTOP_ALLOW_HTTP";

fn allow_http() -> bool {
    std::env::var_os(ALLOW_HTTP_ENV).is_some_and(|value| value == "1")
}

/// Whether the client may send sign-in secrets or bearers to `url` when the
/// server is `origin`: HTTPS always, plain HTTP only from the local machine
/// to the local machine (the development stack), or with [`ALLOW_HTTP_ENV`].
pub(crate) fn transport_allowed(url: &Url, origin: &Url) -> bool {
    match url.scheme() {
        "https" => true,
        "http" => (is_loopback(url) && is_loopback(origin)) || allow_http(),
        _ => false,
    }
}

pub(crate) fn is_loopback(url: &Url) -> bool {
    match url.host() {
        Some(url::Host::Domain(domain)) => domain.eq_ignore_ascii_case("localhost"),
        Some(url::Host::Ipv4(ip)) => ip.is_loopback(),
        Some(url::Host::Ipv6(ip)) => ip.is_loopback(),
        None => false,
    }
}

/// Turn what a person types ("aikonos.example.org", "localhost:4200",
/// "https://ai.example.org/chat") into the server origin. A bare host gets
/// HTTPS unless it is the local machine; plain HTTP to anywhere else is
/// refused, and so is a username or password in the address.
pub fn normalize_server_url(input: &str) -> ApiResult<Url> {
    let trimmed = input.trim().trim_end_matches('/');
    if trimmed.is_empty() {
        return Err(ApiError::SignIn("Enter the address of your aikonOS server.".into()));
    }
    let has_scheme = trimmed.contains("://");
    let with_scheme = if has_scheme {
        trimmed.to_owned()
    } else {
        format!("https://{trimmed}")
    };
    let mut url =
        Url::parse(&with_scheme).map_err(|_| ApiError::SignIn(format!("“{trimmed}” is not a server address.")))?;
    if !matches!(url.scheme(), "http" | "https") || url.host().is_none() {
        return Err(ApiError::SignIn(format!("“{trimmed}” is not a server address.")));
    }
    // It would be kept in desktop.json, and sent as a Basic header.
    if !url.username().is_empty() || url.password().is_some() {
        return Err(ApiError::SignIn(
            "Don't put a username or password in the server address.".into(),
        ));
    }
    // Decided on the parsed host, so "127.evil.example" is not local.
    if !has_scheme && is_loopback(&url) {
        url.set_scheme("http").expect("https and http are both special schemes");
    }
    if !transport_allowed(&url, &url) {
        return Err(ApiError::SignIn(format!(
            "{} does not use HTTPS, so your sign-in would travel unencrypted. Use its https:// address, \
             or ask your administrator to enable HTTPS.",
            url.host_str().unwrap_or_default()
        )));
    }
    url.set_path("/");
    url.set_query(None);
    url.set_fragment(None);
    Ok(url)
}

#[derive(Debug, Default, Deserialize)]
#[serde(rename_all = "camelCase")]
struct OidcDocument {
    authority: Option<String>,
    client_id: Option<String>,
    desktop_client_id: Option<String>,
    scope: Option<String>,
    token: Option<String>,
}

#[derive(Debug, Default, Deserialize)]
struct ConfigDocument {
    #[serde(default)]
    oidc: OidcDocument,
    #[serde(default)]
    release: Option<ReleaseInfo>,
}

fn pick(values: &[Option<&String>]) -> Option<String> {
    values
        .iter()
        .flatten()
        .map(|value| value.trim())
        .find(|value| !value.is_empty())
        .map(str::to_owned)
}

impl ConfigDocument {
    fn into_config(self, origin: Url, from_desktop_document: bool) -> ApiResult<ServerConfig> {
        let oidc = &self.oidc;
        // runtime-config.js describes the browser client; its clientId is the
        // web console's and must not be reused for a native redirect.
        let client_id = if from_desktop_document {
            pick(&[oidc.client_id.as_ref(), oidc.desktop_client_id.as_ref()])
        } else {
            pick(&[oidc.desktop_client_id.as_ref()])
        };
        let authority = match pick(&[oidc.authority.as_ref()]) {
            Some(authority) => authority,
            // The development stack's Keycloak, which the web console
            // assumes too when nothing is configured.
            None if is_loopback(&origin) => DEFAULT_AUTHORITY.to_owned(),
            None => {
                return Err(ApiError::SignIn(format!(
                    "{} doesn't say which sign-in service it uses. Ask your administrator to set \
                     AIKONOS_WEBUI_OIDC_AUTHORITY on the web console.",
                    origin.host_str().unwrap_or_default()
                )));
            }
        };
        let authority = authority.trim_end_matches('/').to_owned();
        match Url::parse(&authority) {
            Ok(url) if transport_allowed(&url, &origin) && url.host().is_some() => {}
            _ => {
                return Err(ApiError::SignIn(format!(
                    "{} names a sign-in service that does not use HTTPS ({authority}). Ask your administrator \
                     to set an https:// AIKONOS_WEBUI_OIDC_AUTHORITY.",
                    origin.host_str().unwrap_or_default()
                )));
            }
        }
        Ok(ServerConfig {
            origin,
            oidc: OidcSettings {
                authority,
                client_id: client_id.unwrap_or_else(|| DEFAULT_DESKTOP_CLIENT_ID.into()),
                scope: pick(&[oidc.scope.as_ref()]).unwrap_or_else(|| DEFAULT_SCOPE.into()),
                token_kind: TokenKind::parse(oidc.token.as_deref()),
            },
            release: self.release,
        })
    }
}

/// Pull the JSON object out of `window.__AIKONOS_CONFIG__ = {...};`.
fn parse_runtime_config_js(script: &str) -> Option<ConfigDocument> {
    if !script.contains("__AIKONOS_CONFIG__") {
        return None;
    }
    let start = script.find('{')?;
    let end = script.rfind('}')?;
    serde_json::from_str(script.get(start..=end)?).ok()
}

/// Whether a response is JSON. An older server answers any path it does not
/// know with the web console's page and a 200, so only a JSON answer counts
/// as a published desktop.json.
fn is_json(response: &reqwest::Response) -> bool {
    response
        .headers()
        .get(reqwest::header::CONTENT_TYPE)
        .and_then(|value| value.to_str().ok())
        .is_some_and(|value| value.trim_start().to_ascii_lowercase().starts_with("application/json"))
}

/// Read a server's client settings. Prefers `/desktop.json`, which servers
/// that know about this app publish; falls back to the web console's
/// `/runtime-config.js` so an older server still works.
pub async fn discover(http: &reqwest::Client, origin: &Url) -> ApiResult<ServerConfig> {
    let desktop = origin.join("desktop.json").expect("static path joins");
    let response = http
        .get(desktop)
        .header("accept", "application/json")
        .timeout(AUTH_TIMEOUT)
        .send()
        .await
        .map_err(ApiError::transport)?;
    if response.status().is_success() && is_json(&response) {
        let text = read_text(response, CONFIG_BODY_LIMIT).await?;
        let document: ConfigDocument =
            serde_json::from_str(&text).map_err(|err| ApiError::Decode(format!("desktop.json: {err}")))?;
        return document.into_config(origin.clone(), true);
    }

    let runtime = origin.join("runtime-config.js").expect("static path joins");
    let response = http
        .get(runtime)
        .timeout(AUTH_TIMEOUT)
        .send()
        .await
        .map_err(ApiError::transport)?;
    if response.status().is_success() {
        let text = read_text(response, CONFIG_BODY_LIMIT).await?;
        if let Some(document) = parse_runtime_config_js(&text) {
            return document.into_config(origin.clone(), false);
        }
    }
    // Some other site, or an aikonOS server behind a gate that wants a
    // browser sign-in before anything else.
    Err(ApiError::SignIn(format!(
        "{} doesn't answer like an aikonOS server. Check the address; if it is right, ask your administrator \
         whether the server lets aikonOS for Windows in.",
        origin.host_str().unwrap_or_default()
    )))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn bare_hosts_get_https_and_local_hosts_get_http() {
        assert_eq!(
            normalize_server_url("ai.example.org").unwrap().as_str(),
            "https://ai.example.org/"
        );
        assert_eq!(
            normalize_server_url("localhost:4200/chat?x=1").unwrap().as_str(),
            "http://localhost:4200/"
        );
        assert_eq!(
            normalize_server_url("127.0.0.1:4300").unwrap().as_str(),
            "http://127.0.0.1:4300/"
        );
        assert!(normalize_server_url("  ").is_err());
        assert!(normalize_server_url("ftp://files.example.org").is_err());
    }

    #[test]
    fn only_the_local_machine_gets_plain_http() {
        // A host that merely starts with "127." is a domain, so it gets HTTPS.
        assert_eq!(
            normalize_server_url("127.evil.example").unwrap().as_str(),
            "https://127.evil.example/"
        );
        assert_eq!(
            normalize_server_url("127.0.0.1.evil.example").unwrap().as_str(),
            "https://127.0.0.1.evil.example/"
        );
        assert_eq!(
            normalize_server_url("http://localhost:4200").unwrap().as_str(),
            "http://localhost:4200/"
        );
        assert_eq!(
            normalize_server_url("https://ai.example.org").unwrap().as_str(),
            "https://ai.example.org/"
        );
        assert!(matches!(
            normalize_server_url("http://ai.example.org"),
            Err(ApiError::SignIn(message)) if message.contains("does not use HTTPS")
        ));
    }

    #[test]
    fn credentials_in_the_address_are_refused() {
        for input in [
            "https://alice:S3cret@ai.example.org",
            "alice@ai.example.org",
            "localhost:80@evil.example",
        ] {
            assert!(
                matches!(normalize_server_url(input), Err(ApiError::SignIn(message)) if message.contains("password")),
                "{input}"
            );
        }
    }

    #[test]
    fn the_sign_in_service_must_use_https() {
        let config = |origin: &str, authority: &str| {
            serde_json::from_value::<ConfigDocument>(serde_json::json!({"oidc": {"authority": authority}}))
                .unwrap()
                .into_config(Url::parse(origin).unwrap(), true)
        };
        assert!(config("https://ai.example.org/", "http://id.example.org/realms/a").is_err());
        assert!(config("https://ai.example.org/", "http://localhost:18080/realms/a").is_err());
        assert!(config("https://ai.example.org/", "javascript:alert(1)").is_err());
        assert!(config("http://localhost:4300/", "http://localhost:4300/realms/a").is_ok());
        assert!(config("http://localhost:4300/", "https://id.example.org/realms/a").is_ok());
    }

    #[test]
    fn runtime_config_never_lends_its_browser_client_id() {
        let script = r#"window.__AIKONOS_CONFIG__ = {"oidc":{"authority":"https://id.example.org/realms/a/","clientId":"aikonos-webui","token":"id"}};"#;
        let config = parse_runtime_config_js(script)
            .unwrap()
            .into_config(Url::parse("https://ai.example.org/").unwrap(), false)
            .unwrap();
        assert_eq!(config.oidc.authority, "https://id.example.org/realms/a");
        assert_eq!(config.oidc.client_id, DEFAULT_DESKTOP_CLIENT_ID);
        assert_eq!(config.oidc.token_kind, TokenKind::Id);
        assert_eq!(config.oidc.scope, "openid profile");
    }

    #[test]
    fn desktop_document_supplies_client_and_release() {
        let document: ConfigDocument = serde_json::from_str(
            r#"{"oidc":{"authority":"https://id.example.org/realms/a","clientId":"desk"},"release":{"version":"0.2.0","minimumVersion":"0.1.0","url":"https://ai.example.org/d.exe"}}"#,
        )
        .unwrap();
        let config = document
            .into_config(Url::parse("https://ai.example.org/").unwrap(), true)
            .unwrap();
        assert_eq!(config.oidc.client_id, "desk");
        let release = config.release.unwrap();
        assert_eq!(release.version, "0.2.0");
        assert_eq!(release.minimum_version.as_deref(), Some("0.1.0"));
    }

    /// Serves `(path, content type, body)` routes over plain HTTP until the
    /// test ends; anything else is a 404.
    async fn serve(routes: &'static [(&'static str, &'static str, &'static str)]) -> Url {
        use tokio::io::{AsyncReadExt as _, AsyncWriteExt as _};

        let listener = tokio::net::TcpListener::bind(("127.0.0.1", 0)).await.unwrap();
        let origin = Url::parse(&format!("http://{}/", listener.local_addr().unwrap())).unwrap();
        tokio::spawn(async move {
            while let Ok((mut stream, _)) = listener.accept().await {
                let mut request = Vec::new();
                let mut buf = [0u8; 1024];
                while !request.windows(4).any(|w| w == b"\r\n\r\n") {
                    match stream.read(&mut buf).await {
                        Ok(0) | Err(_) => break,
                        Ok(n) => request.extend_from_slice(&buf[..n]),
                    }
                }
                let request = String::from_utf8_lossy(&request);
                let path = request.split_whitespace().nth(1).unwrap_or_default();
                let reply = match routes.iter().find(|(route, _, _)| *route == path) {
                    Some((_, kind, body)) => format!(
                        "HTTP/1.1 200 OK\r\ncontent-type: {kind}\r\ncontent-length: {}\r\nconnection: close\r\n\r\n{body}",
                        body.len()
                    ),
                    None => "HTTP/1.1 404 Not Found\r\ncontent-length: 0\r\nconnection: close\r\n\r\n".to_owned(),
                };
                let _ = stream.write_all(reply.as_bytes()).await;
            }
        });
        origin
    }

    #[tokio::test]
    async fn the_web_console_page_is_not_a_desktop_document() {
        // An older server: its SPA fallback answers /desktop.json with HTML.
        let origin = serve(&[
            ("/desktop.json", "text/html; charset=utf-8", "<!doctype html><html></html>"),
            (
                "/runtime-config.js",
                "application/javascript",
                r#"window.__AIKONOS_CONFIG__ = {"oidc":{"authority":"https://id.example.org/realms/a","clientId":"aikonos-webui"}};"#,
            ),
        ])
        .await;
        let config = discover(&reqwest::Client::new(), &origin).await.unwrap();
        assert_eq!(config.oidc.authority, "https://id.example.org/realms/a");
        assert_eq!(config.oidc.client_id, DEFAULT_DESKTOP_CLIENT_ID);
        assert_eq!(config.release, None);
    }

    #[tokio::test]
    async fn a_published_desktop_document_wins() {
        let origin = serve(&[(
            "/desktop.json",
            "application/json; charset=utf-8",
            r#"{"oidc":{"authority":"https://id.example.org/realms/b","clientId":"desk"},"release":{"version":"9.0.0"}}"#,
        )])
        .await;
        let config = discover(&reqwest::Client::new(), &origin).await.unwrap();
        assert_eq!(config.oidc.authority, "https://id.example.org/realms/b");
        assert_eq!(config.oidc.client_id, "desk");
        assert_eq!(config.release.map(|release| release.version).as_deref(), Some("9.0.0"));
    }

    #[test]
    fn only_the_development_stack_may_leave_out_its_sign_in_service() {
        let local = ConfigDocument::default()
            .into_config(Url::parse("http://localhost:4200/").unwrap(), true)
            .unwrap();
        assert_eq!(local.oidc.authority, DEFAULT_AUTHORITY);
        let remote = ConfigDocument::default().into_config(Url::parse("https://ai.example.org/").unwrap(), true);
        assert!(matches!(remote, Err(ApiError::SignIn(message)) if message.contains("AIKONOS_WEBUI_OIDC_AUTHORITY")));
    }

    #[tokio::test]
    async fn a_site_with_neither_document_is_not_an_aikonos_server() {
        // A gate's sign-in page, or any other site: HTML everywhere.
        let origin = serve(&[
            (
                "/desktop.json",
                "text/html",
                "<!doctype html><script>var a = {};</script>",
            ),
            (
                "/runtime-config.js",
                "text/html",
                "<!doctype html><script>var a = {};</script>",
            ),
        ])
        .await;
        let refused = discover(&reqwest::Client::new(), &origin).await;
        assert!(
            matches!(refused, Err(ApiError::SignIn(message)) if message.contains("doesn't answer like an aikonOS server"))
        );
    }

    #[test]
    fn plain_http_is_insecure_unless_local() {
        let config = |origin: &str, authority: &str| ServerConfig {
            origin: Url::parse(origin).unwrap(),
            oidc: OidcSettings {
                authority: authority.into(),
                client_id: DEFAULT_DESKTOP_CLIENT_ID.into(),
                scope: DEFAULT_SCOPE.into(),
                token_kind: TokenKind::Access,
            },
            release: None,
        };
        assert!(config("http://ai.example.org/", DEFAULT_AUTHORITY).is_insecure());
        assert!(!config("http://localhost:4200/", DEFAULT_AUTHORITY).is_insecure());
        assert!(!config("https://ai.example.org/", DEFAULT_AUTHORITY).is_insecure());
        // With AIKONOS_DESKTOP_ALLOW_HTTP an HTTPS server may name a
        // plain-HTTP identity provider; the code and tokens go there.
        assert!(config("https://ai.example.org/", "http://id.example.org/realms/a").is_insecure());
        assert!(!config("https://ai.example.org/", "https://id.example.org/realms/a").is_insecure());
    }
}
