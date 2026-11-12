//! Area **onboarding** (Task 25): enrolling this device by password and recovery key.
//!
//! Ports: packages/client-core/src/account/enrol.ts.
//!
//! It runs before a [`crate::Client`] exists (like [`crate::signin`]), so it has no `Client` accessor.
//!
//! Stub from Task 1 (area scaffold): Task 25 fills it, keeping the names below (clients/desktop/OWNERSHIP.md).

/// The enrol entry points (Task 25 defines them).
#[derive(Clone, Copy, Debug, Default)]
pub struct Enrol;
