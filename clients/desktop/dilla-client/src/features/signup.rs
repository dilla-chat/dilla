//! Area **onboarding** (Task 25): sign-up: invite, name, recovery key, browser or passkey, factor choice.
//!
//! Ports: packages/client-core/src/account/signup.ts, packages/client-core/src/recovery-key.ts.
//!
//! It runs before a [`crate::Client`] exists (like [`crate::signin`]), so it has no `Client` accessor.
//!
//! Stub from Task 1 (area scaffold): Task 25 fills it, keeping the names below (clients/desktop/OWNERSHIP.md).

/// The signup entry points (Task 25 defines them).
#[derive(Clone, Copy, Debug, Default)]
pub struct Signup;
