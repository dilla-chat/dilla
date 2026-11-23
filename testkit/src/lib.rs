//! dilla-testkit: N headless native `dilla-core` clients against a delivery service, driven by a
//! line-oriented scenario language. The delivery service is a trait (`DeliveryService`) with two
//! implementations: the in-memory `DsStub`, which a scenario gets by default, and `HttpDs`, the
//! real `/v1` surface and gateway of a running `dillad`, which `ds <url>` or `--ds <url>` selects.
//!
//! The stub covers R20's week-1 subset of `protocol/02-delivery-service.md`: registration with the
//! `dilla_binding` (invariant 1, minus the `mode_readable` channel check), the tree service
//! (invariant 2), one commit per epoch (invariant 3), the KeyPackage directory (Role 1, not an
//! invariant) and the *structural* clause of invariant 4 — what the `PublicGroup` validates.
//! Invariant 4's remaining commit-validity rules (the ACL and device-list check on every `Add`, no
//! `Update` from the committer, member-originated `Remove`s confined to the committer's own user,
//! every outstanding non-void DS proposal referenced, the uploaded GroupInfo at epoch `n + 1`) and
//! invariants 5-11 belong to the dillad plan; the methods that would enforce them here succeed
//! without checking.

mod client;
mod ds;
mod fixtures;
mod media_driver;
pub mod nested_cargo;
mod netprobe;
mod scenario;
mod totp;
mod web_driver;

pub use client::{Received, TestClient};
pub use ds::{
    CommitAccepted, CommitRequest, CommitResult, CommitUpload, DeliveryService, Device, DsError,
    DsStub, Enrolled, ErrorExtras, Frame, GroupId, GroupInfoResp, GroupInfoResponse,
    GroupRegistered, HandshakeItem, HealRequest, HttpDs, InstanceConfig, KeyPackageResp,
    MessageAccepted, MessageItem, NewAccount, RegisterGroup, RegisterRequest, RegisterResult,
    ResyncRequest, TreeResp, TreeResponse, UploadResult, WelcomeItem,
};
pub use fixtures::{
    FixtureFile, FixtureManifest, FixtureSpec, KeyPackageSetEntry, KeyPackageSetManifest,
    KeyPackageSetSpec, RegistrationGroup, RegistrationManifest, RegistrationSpec, gen_key_packages,
    gen_public_group, gen_registration_groups,
};
pub use media_driver::MediaDriver;
pub use netprobe::{
    Armed, Prepared, arm as netprobe_arm, end as netprobe_end, prepare as netprobe_prepare,
    start as netprobe_start,
};
pub use scenario::{
    DeviceListMode, ParseError, RunReport, Runner, Scenario, StepResult, Stmt, parse,
};
pub use web_driver::WebDriver;

#[derive(Debug, thiserror::Error)]
#[non_exhaustive]
pub enum TestkitError {
    #[error(transparent)]
    Ds(#[from] DsError),
    #[error(transparent)]
    Core(#[from] dilla_core::CoreError),
    #[error("scenario: {0}")]
    Scenario(String),
    #[error("assertion: {0}")]
    Assertion(String),
}

impl From<dilla_core::ProtocolError> for TestkitError {
    fn from(e: dilla_core::ProtocolError) -> Self {
        TestkitError::Core(e.into())
    }
}

impl From<dilla_core::mls::MlsError> for TestkitError {
    fn from(e: dilla_core::mls::MlsError) -> Self {
        TestkitError::Core(e.into())
    }
}

impl From<dilla_core::mls::StorageError> for TestkitError {
    fn from(e: dilla_core::mls::StorageError) -> Self {
        TestkitError::Core(e.into())
    }
}
