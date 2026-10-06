# 16 — Aikonos for Windows

> **Purpose.** Aikonos for Windows is the member console as a native Windows
> app: the same screens and the same server as the web console, plus moving
> files between the PC and the workspace. This page covers what it does, how it
> signs in, what a server publishes for it, how it learns about new builds, what
> it keeps on the PC, and how to build and test it.
>
> The code is [`desktop/`](../desktop/): `crates/client` speaks the server's
> API (sign-in, requests, streams, the session file format) and has no UI;
> `crates/app` is the UI, drawn with [GPUI Kit](https://github.com/longbridge/gpui-kit).

---

## What it is

The app offers what a member sees in the web console: Home, Chat, Files,
Connections, Schedules, Workflows, My Skills, Inbox and personal settings.
The admin screens stay in the web console.

It is a client of the same server, not a second one. It calls the web server's
`/api` and `/agui` exactly as the browser does, with the user's own bearer
token, so every agent action still passes the broker's gates and every
approval is the same approval: the app shows the approval dialog, and closing
it or pressing Escape denies, as in the web console. Conversations are the same
workspace files (`.agent/Sessions/`), so a chat started in one client carries
on in the other.

The UI is native, not a web view. It is drawn from the web console's design
tokens ([`tokens.css`](../webui/web/src/styles/tokens.css)) and icon set
([`Icon.vue`](../webui/web/src/components/Icon.vue)), and the app's tests fail
when either drifts from the web console's.

## Local files

The user moves files; the agent never reads the PC's disk.

| Direction | How |
|---|---|
| PC → workspace | Files: Upload, or drop files onto the list or onto a folder row. Chat: the attach button, or drop files onto the composer; the file is uploaded (images into `references/`) and mentioned as `#path`. 10 MiB per file, as in the web console. |
| Workspace → PC | Files: Save to this PC writes the file where the user chooses, then shows it in Explorer. |

An uploaded file is an ordinary workspace file from then on: any agent action
on it passes the gates like any other.

Links leave the app only for the browser: it opens `http` and `https` links,
whether in a reply or from the server, and refuses anything else, such as a
file path or another program's protocol, which Windows would otherwise start.

## Signing in

The app signs in as a native OIDC client ([RFC 8252](https://www.rfc-editor.org/rfc/rfc8252)):
the authorization code flow with PKCE (S256) in the system browser, returning
to a listener on `http://127.0.0.1:<random port>/callback` that checks `state`
and ignores anything else. It sends the access or ID token as its bearer,
following `AIKONOS_WEBUI_OIDC_TOKEN` as the web console does.

Tokens live in memory only. The app renews them with the refresh token before
they expire (and before starting a long stream), forgets them when it closes,
and on sign-out asks the identity provider to end the session and revoke the
refresh token, where the provider supports those calls.

TLS uses the Windows certificate store, so an internal CA distributed by group
policy works without configuration. Signing in to a non-local server over plain
HTTP shows a warning.

### What the server publishes

When someone enters a server's address, the app reads `/desktop.json` from the
web server ([`webui/server.mjs`](../webui/server.mjs)):

```json
{
  "oidc": {
    "authority": "https://id.example.org/realms/aikonos",
    "scope": "openid profile",
    "token": "access",
    "clientId": "aikonos-desktop"
  },
  "release": {
    "version": "0.2.0",
    "minimumVersion": "0.1.0",
    "url": "https://software.example.org/aikonos/aikonos-0.2.0.exe",
    "notes": "Faster uploads."
  }
}
```

It comes from the web server's environment:

| Variable | Default | Meaning |
|---|---|---|
| `AIKONOS_WEBUI_OIDC_AUTHORITY`, `_SCOPE`, `_TOKEN` | — | Shared with the web console: the same identity provider, scope and token kind. Must be set at runtime for the app; values baked into the web console's build don't reach it. |
| `AIKONOS_DESKTOP_OIDC_CLIENT` | `aikonos-desktop` | The app's own client id. Never the web console's unless set to it (see Entra below). |
| `AIKONOS_DESKTOP_VERSION` | unset | The current desktop build. Older builds show an update notice. |
| `AIKONOS_DESKTOP_MINIMUM_VERSION` | unset | Older builds stop at sign-in until updated. Enough on its own. |
| `AIKONOS_DESKTOP_URL` | unset | Where the build is downloaded. The app opens only `http` and `https` links. |
| `AIKONOS_DESKTOP_NOTES` | unset | One line shown with the update notice. |

[`compose.yaml`](../compose.yaml) passes all of them to the `webui` service.
A server without `/desktop.json` still works: the app reads the identity
provider from `/runtime-config.js` and uses its default client id, never the
web console's. A site that publishes neither is refused with a message saying
it doesn't answer like an Aikonos server.

### Registering the app with the identity provider

**Keycloak.** The development realm
([`keycloak-realm.json`](../deploy/compose/keycloak-realm.json)) has the
`aikonos-desktop` client: public, standard flow, PKCE S256, redirect URI
`http://127.0.0.1/callback` (Keycloak accepts any port on that loopback
address), and the same `aikonos-broker` audience and `tenant_id` mappers as the
web console's client. Keycloak imports the realm only when it does not exist
yet, so a running development stack picks the client up after
`docker compose up -d --force-recreate keycloak`; another realm needs the same
client added in its admin console.

**Microsoft Entra ID.** Let the app sign in as the web console's own app
registration: add `http://127.0.0.1/callback` to it as a *Mobile and desktop
applications* (public client) redirect URI, and set
`AIKONOS_DESKTOP_OIDC_CLIENT` to the web console's client id. Entra ignores the
port of a loopback redirect, but its portal refuses `http` loopback addresses
in the redirect URI box; add it in the app manifest instead
([Microsoft's guidance](https://learn.microsoft.com/en-us/entra/identity-platform/reply-url#prefer-127001-over-localhost)).
Sharing the registration is what makes both models in
[12-entra-login.md](12-entra-login.md) work: an ID token's audience is the
client that asked for it, and the OneDrive exchange keys on the web console's
client id.

### In front of the server

- **Broker `oidc.authorized_party`.** When set, the broker accepts one `azp`.
  Tokens from a separate desktop client carry `azp` `aikonos-desktop` and are
  refused. Leave it unset, or have the app share the web console's client id.
- **The Azure overlay's oauth2-proxy gate**
  ([`compose.azure.yaml`](../deploy/compose/compose.azure.yaml)) wants its
  browser cookie on every request, so it turns the app away. The app needs the
  gate to accept bearer tokens from the identity provider (oauth2-proxy's
  `skip_jwt_bearer_tokens` with `extra_jwt_issuers`), or a route that bypasses
  the gate. The on-prem overlay has no such gate.

## Staying up to date

The app can't change with the server the way a web page does, so it stays
current in two ways. Everything it shows comes from the server live: chat
streams, the inbox count (checked every 15 seconds), each page loaded fresh when
opened, and the session list, agents and permitted pages refreshed when the
app comes back to the front. And the server says which build it expects: a
newer one shows in the sidebar footer and in Settings → Account with a
download link; below the minimum, sign-in stops with the download link and the
notes.

The app never replaces itself. Installing a new build is the organisation's
own software distribution's job; the server only says what to install. A
shortcut that starts `aikonos.exe --server https://aikonos.example.org` skips
the address step and goes straight to sign-in.

## What stays on the PC

`%APPDATA%\Aikonos\desktop.json` holds the server address, theme, sidebar
state, the user's chat instructions, the debug toggle, and the last folders
used in open and save dialogs.

Nothing else is kept: no tokens, no conversations, no copies of workspace
files except those the user saves. The Windows notification shown when an
approval waits while the app is in the background names the tool only.

## Building and testing

The toolchain is pinned in
[`desktop/rust-toolchain.toml`](../desktop/rust-toolchain.toml); building
needs the MSVC build tools and the Windows SDK.

```sh
cd desktop
cargo test --workspace
cargo build --release        # target\release\aikonos.exe
```

[`.github/workflows/desktop.yml`](../.github/workflows/desktop.yml) checks
formatting, runs clippy and the tests on Windows, and keeps the release build
as an artifact.

To click through the app without a stack, run the stand-in server
[`desktop/dev/mock-server.mjs`](../desktop/dev/mock-server.mjs). It signs
anyone in and enforces nothing, so it is for development only.

```sh
node desktop/dev/mock-server.mjs                  # http://localhost:4300
cargo run -- --server http://localhost:4300       # from desktop/
```

`MOCK_DESKTOP_VERSION`, `MOCK_DESKTOP_MINIMUM_VERSION` and `MOCK_DESKTOP_NOTES`
make it advertise a release. A debug build started with
`AIKONOS_DEV_HEADLESS_SIGNIN=1` against the mock started with
`MOCK_AUTO_SIGNIN=1` signs in without opening a browser; release builds ignore
the variable.

## Known gaps

- **Stop** ends the stream in the app, as in the web console. The web server
  doesn't pass the disconnect on to the gateway, so the run finishes there.
- There is no installer, and the executable isn't code-signed yet.
- Admin screens are web-only.
