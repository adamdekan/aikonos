# aikonOS for Windows

The member console as a native Windows app, drawn with
[GPUI Kit](https://github.com/longbridge/gpui-kit). It signs in to an aikonOS
server and offers what a member sees in the web console, plus moving files
between the PC and the workspace. How it signs in, what a server publishes for
it, and what it keeps on the PC: [docs/16-desktop-client.md](../docs/16-desktop-client.md).

| Path | Contents |
|---|---|
| `crates/client` | `aikonos-client`: the server's API as the app speaks it (sign-in, requests, AG-UI streams, the session file format). No UI. |
| `crates/app` | `aikonos-desktop`, the `aikonos.exe` binary: the screens, theme and local-file handling |
| `assets` | The web console's theme as a GPUI Kit theme, and its fonts (Inter, Space Grotesk; SIL OFL) |
| `dev/mock-server.mjs` | A stand-in server for development. It signs anyone in and enforces nothing. |

## Build and test

The toolchain is pinned in `rust-toolchain.toml`; building needs the MSVC build
tools and the Windows SDK.

```sh
cargo test --workspace
cargo clippy --workspace --all-targets -- -D warnings
cargo build --release        # target\release\aikonos.exe
```

The app's tests compare its theme with `webui/web/src/styles/tokens.css` and its
icons with `webui/web/src/components/Icon.vue`, so a change to either shows up
here.

## Run it against the stand-in server

```sh
node dev/mock-server.mjs                   # http://localhost:4300
cargo run -- --server http://localhost:4300
```

Words in a prompt pick the mock's script: `approve` (with `stepup` for a
high-risk approval), `tool`, `skill`, `memory`, `fanout`, `fail`, `error`; see
the top of `dev/mock-server.mjs`.
