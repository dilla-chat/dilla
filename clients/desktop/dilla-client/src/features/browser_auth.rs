//! Area **factors** (Task 26): the system-browser ceremony (get, create, oidc; 2 min timeout; SHA-256 verifier; `desktop-auth/request` and `desktop-auth/<id>/consume`), the browser opened with `open`.
//!
//! Ports: packages/desktop/src/browser-auth.ts, packages/web/src/desktop-auth-browser.ts.
//!
//! It runs before a [`crate::Client`] exists (like [`crate::signin`]), so it has no `Client` accessor.
//!
//! Stub from Task 1 (area scaffold): Task 26 fills it, keeping the names below (clients/desktop/OWNERSHIP.md).

/// The browser auth entry points (Task 26 defines them).
#[derive(Clone, Copy, Debug, Default)]
pub struct BrowserAuth;
