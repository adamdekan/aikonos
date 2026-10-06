//! The Tokio runtime the network layer runs on, and the bridge to GPUI.
//!
//! GPUI drives the UI on its own executor; reqwest needs Tokio. Requests are
//! spawned onto a small Tokio runtime and awaited from GPUI tasks: a Tokio
//! `JoinHandle` and its channels can be polled from any executor.
//!
//! [`spawn`] ties a request to whoever awaits it: when the view that started
//! it goes away and drops its GPUI task, the request is aborted too.
//! [`detach`] is for writes that must finish regardless (saving a session,
//! recording an approval decision).

use std::future::Future;
use std::pin::Pin;
use std::sync::OnceLock;
use std::task::{Context, Poll};

use aikonos_client::{ApiError, ApiResult};

fn runtime() -> &'static tokio::runtime::Runtime {
    static RUNTIME: OnceLock<tokio::runtime::Runtime> = OnceLock::new();
    RUNTIME.get_or_init(|| {
        tokio::runtime::Builder::new_multi_thread()
            .worker_threads(2)
            .thread_name("aikonos-net")
            .enable_all()
            .build()
            .expect("the network runtime starts")
    })
}

/// A request running on the network runtime. Awaiting it yields its result;
/// dropping it aborts the request.
pub struct NetTask<T> {
    handle: tokio::task::JoinHandle<ApiResult<T>>,
}

impl<T> Future for NetTask<T> {
    type Output = ApiResult<T>;

    fn poll(mut self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Self::Output> {
        Pin::new(&mut self.handle)
            .poll(cx)
            .map(|joined| joined.unwrap_or_else(|err| Err(ApiError::Transport(format!("request task ended: {err}")))))
    }
}

impl<T> Drop for NetTask<T> {
    fn drop(&mut self) {
        self.handle.abort();
    }
}

/// Run `future` on the network runtime, cancelled if the result is dropped.
pub fn spawn<T, F>(future: F) -> NetTask<T>
where
    T: Send + 'static,
    F: Future<Output = ApiResult<T>> + Send + 'static,
{
    NetTask {
        handle: runtime().spawn(future),
    }
}

/// Run `future` to completion whatever happens to the caller.
pub fn detach<F>(future: F)
where
    F: Future<Output = ()> + Send + 'static,
{
    runtime().spawn(future);
}

/// Start the runtime before the first window opens, so the first request
/// does not pay for it.
pub fn init() {
    let _ = runtime();
}
