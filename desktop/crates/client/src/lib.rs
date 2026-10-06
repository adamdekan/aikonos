//! The aikonOS user API, as the desktop client speaks it.
//!
//! This crate holds everything that does not draw: locating a server,
//! signing in, the authenticated request helpers, the AG-UI and workflow
//! streams, the endpoint wrappers for every page a member can open, and the
//! session file format shared with the web console. It runs on Tokio; the
//! app crate bridges it to GPUI's executor.
//!
//! Nothing here widens what the server allows. Every call carries the
//! user's own bearer to the same paths the web console uses, and the
//! gateway decides.

pub mod agui;
pub mod api;
pub mod auth;
pub mod connection;
pub mod cron;
pub mod error;
pub mod money;
pub mod server;
pub mod sse;
pub mod tool_labels;
pub mod transcript;
pub mod version;

pub use connection::{Connection, EventStream, SignInStart, StreamItem};
pub use error::{ApiError, ApiResult};
