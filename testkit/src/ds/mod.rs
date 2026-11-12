//! The delivery service a test client talks to, behind one trait with two implementations.
//!
//! `DsStub` is an in-memory **test double**, not a specification: it covers R20's week-1 subset of
//! protocol/02 and nothing else — registration with the binding (invariant 1, minus the
//! `mode_readable` channel check), the tree service (invariant 2), one commit per epoch
//! (invariant 3), the KeyPackage directory (Role 1, not an invariant) and the structural clause of
//! invariant 4. The rest of invariant 4's commit-validity rules, and invariants 5-11, belong to
//! `dillad`; the stub methods that would enforce them succeed without checking, and the two it
//! cannot model at all (`fork_report`, `heal`) refuse with `DsError::Unsupported`.
//!
//! `HttpDs` (`remote.rs`) is the same trait over the real `/v1` surface and the gateway, so one
//! scenario file runs against both. A scenario selects it with `ds <url>` or the CLI's `--ds`.

mod invariants;
pub mod remote;
mod state;

use dilla_core::ids::DeviceId;

pub use remote::{Enrolled, HttpDs, NewAccount};
pub use state::{
    CommitAccepted, CommitUpload, DsError, DsStub, ErrorExtras, Frame, GroupInfoResponse,
    GroupRegistered, HandshakeItem, InstanceConfig, MessageAccepted, MessageItem, RegisterGroup,
    TreeResponse,
};

/// Everything a test client asks of a delivery service. `DsStub` implements it in memory and
/// `HttpDs` over the real `/v1` surface plus the gateway, so one scenario file runs against both.
///
/// Four stub-only calls are deliberately absent, because a real client derives each of them:
/// `attach_public_group` and `add_member_device` (the DS builds its own view and fan-out list),
/// `binding` (the joiner knows the channel it was invited into, and the Welcome's GroupContext is
/// checked against it) and `public_group` for a device-to-leaf lookup (parse `GET /tree` and walk
/// the leaves — the honest thing anyway).
///
/// Interface deviation B16 (§7.1): the names below are the contract's; `DsStub`'s inherent
/// methods keep the names the repository already had, and the `impl DeliveryService for DsStub`
/// in `state.rs` is the adapter between the two.
pub trait DeliveryService {
    fn publish_key_packages(
        &mut self,
        d: &Device,
        kps: Vec<Vec<u8>>,
        last_resort: Option<Vec<u8>>,
    ) -> Result<usize, DsError>;
    fn register_group(&mut self, r: RegisterRequest) -> Result<RegisterResult, DsError>;
    fn group_info(&mut self, g: &GroupId) -> Result<GroupInfoResp, DsError>;
    fn ratchet_tree(&mut self, g: &GroupId) -> Result<TreeResp, DsError>;
    fn handshakes(&mut self, g: &GroupId, from: u64) -> Result<Vec<HandshakeItem>, DsError>;
    fn take_key_package(&mut self, target: &DeviceId) -> Result<KeyPackageResp, DsError>;
    fn post_commit(&mut self, g: &GroupId, c: CommitRequest) -> Result<CommitResult, DsError>;
    fn post_external_commit(
        &mut self,
        g: &GroupId,
        c: ResyncRequest,
    ) -> Result<CommitResult, DsError>;
    fn post_proposal(&mut self, g: &GroupId, epoch: u64, proposal: Vec<u8>)
    -> Result<u64, DsError>;
    /// Row 7. `class` is the upload class (web3.7 SB1, protocol/02 § Upload class): `0` a post,
    /// `1` anything else. Every upload names one (web3.8 S1: the two-element body is refused).
    fn post_message_from(
        &mut self,
        g: &GroupId,
        epoch: u64,
        pm: Vec<u8>,
        class: u8,
    ) -> Result<UploadResult, DsError>;
    fn messages(&mut self, g: &GroupId, from: u64) -> Result<Vec<MessageItem>, DsError>;
    fn welcomes(&mut self) -> Result<Vec<WelcomeItem>, DsError>;
    fn ack_welcome(&mut self, welcome_id: u64) -> Result<(), DsError>;
    fn fork_report(
        &mut self,
        g: &GroupId,
        epoch: u64,
        seq: u64,
        reason: &str,
    ) -> Result<(), DsError>;
    fn heal(&mut self, g: &GroupId, h: HealRequest) -> Result<CommitResult, DsError>;
    fn advance_cursor(&mut self, g: &GroupId, seq: u64, epoch: u64) -> Result<(), DsError>;
    /// Gateway frames received since the last drain.
    fn drain(&mut self) -> Result<Vec<Frame>, DsError>;
    /// One line describing this device's transport state, for a failed `expect_*` step. The stub
    /// has nothing to say; the remote client reports its socket, reconnects and cursors.
    fn diagnostics(&self, _g: &GroupId) -> String {
        String::new()
    }
    /// Close or reopen the gateway connection. The stub flips a flag; `HttpDs` closes the socket,
    /// which is what makes `go_offline` mean the same thing to the DS's online predicate.
    fn set_online(&mut self, online: bool) -> Result<(), DsError>;
    /// Advance the instance's clock. Only the test-control listener offers this; the stub moves
    /// its own counter.
    fn advance_clock(&mut self, secs: u64) -> Result<(), DsError>;
    /// Invariant 7's acknowledgement: the `commit_ack` gateway frame (opcode 12) naming the
    /// `round` of the `mls.commit_needed` it answers. The stub never elects a committer, so it
    /// refuses this with `Unsupported`.
    fn ack_commit(&mut self, g: &GroupId, round: u64) -> Result<(), DsError>;
    /// Row 19: the instance proposals outstanding for the group's current epoch, void ones
    /// included and marked. A committer reads them here rather than trusting that every
    /// proposal's `mls.handshake` frame has already reached its socket: invariant 4 refuses a
    /// commit that misses one. The stub issues no instance proposals and answers none.
    fn proposals(&mut self, g: &GroupId) -> Result<Vec<ProposalItem>, DsError>;
}

/// One item of row 19: `[ref, kind, target_leaf|null, blob, void]`.
#[derive(Clone, Debug)]
pub struct ProposalItem {
    pub proposal_ref: Vec<u8>,
    pub kind: u8,
    pub target_leaf: Option<u32>,
    pub blob: Vec<u8>,
    pub void: bool,
}

/// A group identifier as the trait takes it. `[u8]`, not the contract's `Vec<u8>`, so a caller
/// holding a slice passes it without an allocation and the trait's `&GroupId` parameters are
/// `&[u8]` — which is what every `DsStub` inherent method already took (deviation B16).
pub type GroupId = [u8];
pub type RegisterRequest = RegisterGroup;

/// The group id inside a serialized GroupInfo message, or None when the bytes are not one. The instance stores a
/// group under this id (row 1 refuses any other), so both delivery services here derive it the same way.
pub(crate) fn group_id_of(group_info: &[u8]) -> Option<Vec<u8>> {
    use openmls::prelude::{MlsMessageBodyIn, MlsMessageIn};
    use tls_codec::Deserialize as _;
    match MlsMessageIn::tls_deserialize_exact(group_info)
        .ok()?
        .extract()
    {
        MlsMessageBodyIn::GroupInfo(info) => Some(info.group_id().as_slice().to_vec()),
        _ => None,
    }
}
pub type RegisterResult = GroupRegistered;
pub type GroupInfoResp = GroupInfoResponse;
pub type TreeResp = TreeResponse;

/// One device's signing identity: the id the instance knows it by and its Ed25519 device key,
/// which signs the 81-byte session preimage of protocol/02 § Device sessions.
pub struct Device {
    id: DeviceId,
    key: ed25519_dalek::SigningKey,
}

impl Device {
    pub fn new(id: DeviceId, key: ed25519_dalek::SigningKey) -> Self {
        Self { id, key }
    }

    pub fn id(&self) -> DeviceId {
        self.id
    }

    /// `dsk_pub`, the key the instance verifies session signatures with.
    pub fn public(&self) -> [u8; 32] {
        self.key.verifying_key().to_bytes()
    }

    pub fn sign(&self, message: &[u8]) -> [u8; 64] {
        use ed25519_dalek::Signer as _;
        self.key.sign(message).to_bytes()
    }
}

/// What `take_key_package` answers. `DsStub::take_key_package` returns `(Vec<u8>, bool)`; the
/// third element is the kp_ref ABI v2 added, which the stub fills with the SHA-256 of the blob so
/// the shape is honest even where the stub does not compute a real KeyPackageRef.
#[derive(Clone, Debug)]
pub struct KeyPackageResp {
    pub blob: Vec<u8>,
    pub last_resort: bool,
    pub kp_ref: Vec<u8>,
}

/// Row 5's body. `ratchet_tree` (element 4) is always sent as null: the instance serves the tree
/// from its own view (invariant 2).
pub struct CommitRequest {
    pub epoch: u64,
    pub commit: Vec<u8>,
    pub group_info: Vec<u8>,
    pub welcomes: Vec<(DeviceId, Vec<u8>)>,
}

#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub struct CommitResult {
    pub seq: u64,
    pub epoch: u64,
}

/// Row 8's body. It carries no epoch: a device out of step does not know it, and the instance
/// supplies its own under the group lock.
pub struct ResyncRequest {
    pub external_commit: Vec<u8>,
    pub group_info: Vec<u8>,
}

#[derive(Clone, Debug)]
pub struct UploadResult {
    pub seq: u64,
    pub franking_tag: Vec<u8>,
    pub recv_ts: u64,
}

/// One row of row 15. `ratchet_tree` is the tree **as of the welcoming epoch**.
#[derive(Clone, Debug)]
pub struct WelcomeItem {
    pub welcome_id: u64,
    pub group_id: Vec<u8>,
    pub epoch: u64,
    pub commit_seq: u64,
    pub blob: Vec<u8>,
    pub ratchet_tree: Vec<u8>,
    pub tree_hash: Vec<u8>,
}

/// Row 14's body: the member-signed GroupInfo, the handshake tail (at most 64 items) and — only
/// when the instance holds no usable state blob — the ratchet tree.
pub struct HealRequest {
    pub group_info: Vec<u8>,
    pub tail: Vec<HandshakeItem>,
    pub ratchet_tree: Option<Vec<u8>>,
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The stub must satisfy the trait unchanged: every existing scenario runs against it, and a
    /// trait that only the HTTP client can implement would silently fork the two.
    #[test]
    fn the_stub_satisfies_the_delivery_service_trait() {
        fn assert_impl<T: DeliveryService>() {}
        assert_impl::<DsStub>();
    }

    /// The four calls the week-1 stub adds for the trait: fork reports and heal are refused
    /// outright (so a scenario that needs them cannot pass vacuously), the cursor is recorded per
    /// device and group, and the clock moves.
    #[test]
    fn the_stub_refuses_what_it_does_not_model_and_records_the_rest() {
        let mut ds = DsStub::new(InstanceConfig {
            instance_id: dilla_core::ids::InstanceId::from_bytes([0x11; 16]),
            signing_key: [0x77; 32],
            policy_version: 1,
            k_frank: [0x09; 32],
        });
        let group = [0x44u8; 16];
        // No device named yet: every per-device call says so instead of guessing.
        assert!(matches!(
            DeliveryService::drain(&mut ds),
            Err(DsError::Protocol(_))
        ));
        let device = DeviceId::from_bytes([0xd1; 16]);
        ds.act_as(device);
        let refused = ds.fork_report(&group, 1, 2, "fork").unwrap_err();
        assert!(matches!(refused, DsError::Unsupported(_)), "{refused:?}");
        assert!(refused.to_string().contains("ds <url>"), "{refused}");
        let refused = ds
            .heal(
                &group,
                HealRequest {
                    group_info: Vec::new(),
                    tail: Vec::new(),
                    ratchet_tree: None,
                },
            )
            .unwrap_err();
        assert!(matches!(refused, DsError::Unsupported(_)), "{refused:?}");

        // A cursor for a group the stub does not know is E_NOT_FOUND, as on the instance.
        assert!(matches!(
            ds.advance_cursor(&group, 3, 1),
            Err(DsError::NotFound)
        ));
        assert_eq!(ds.cursor(&device, &group), None);

        let before = ds.clock();
        DeliveryService::advance_clock(&mut ds, 86_400).unwrap();
        assert_eq!(ds.clock(), before + 86_400);

        // A welcome id nobody was sent is E_NOT_FOUND, and an empty queue is an empty list.
        assert!(ds.welcomes().unwrap().is_empty());
        assert!(matches!(ds.ack_welcome(7), Err(DsError::NotFound)));
        // An upload that is not a PrivateMessage carries no commitment.
        assert!(matches!(
            DeliveryService::post_message_from(&mut ds, &group, 0, vec![1, 2, 3], 0),
            Err(DsError::CommitmentInvalid)
        ));
    }

    /// web3.8 S1: as on the instance, an upload without a class (the two-element body web3.7
    /// accepted) is `400 E_INVALID_REQUEST`, before anything else is looked at. The control is the
    /// same upload with class 0, which passes that check and meets the next one (no such group).
    #[test]
    fn the_stub_refuses_an_upload_without_a_class() {
        let mut ds = DsStub::new(InstanceConfig {
            instance_id: dilla_core::ids::InstanceId::from_bytes([0x11; 16]),
            signing_key: [0x77; 32],
            policy_version: 1,
            k_frank: [0x09; 32],
        });
        let group = [0x44u8; 16];
        let device = DeviceId::from_bytes([0xd1; 16]);
        let Err(refused) =
            ds.post_message_from(&group, 0, device, vec![1, 2, 3], Some([0x0c; 32]), None)
        else {
            panic!("an upload without a class was accepted");
        };
        assert!(
            matches!(
                &refused,
                DsError::Remote { status: 400, code: "E_INVALID_REQUEST", detail }
                    if detail == "body must be [epoch, private_message, class], got 2 elements"
            ),
            "{refused:?}"
        );
        // The contract's own post_message names no class either.
        assert!(matches!(
            ds.post_message(&group, 0, vec![1, 2, 3]),
            Err(DsError::Remote {
                status: 400,
                code: "E_INVALID_REQUEST",
                ..
            })
        ));
        // S1 fix round 1 (m4): a class outside 0 and 1 is refused the same way, as DS.Upload does.
        for class in [2u8, 255] {
            assert!(
                matches!(
                    &ds.post_message_from(
                        &group,
                        0,
                        device,
                        vec![1, 2, 3],
                        Some([0x0c; 32]),
                        Some(class)
                    ),
                    Err(DsError::Remote { status: 400, code: "E_INVALID_REQUEST", detail })
                        if detail == "class must be 0 or 1"
                ),
                "class {class}"
            );
        }
        // Control: class 0 and class 1 pass the check and meet the next one (no such group).
        for class in [0u8, 1] {
            assert!(matches!(
                ds.post_message_from(
                    &group,
                    0,
                    device,
                    vec![1, 2, 3],
                    Some([0x0c; 32]),
                    Some(class)
                ),
                Err(DsError::NotFound)
            ));
        }
    }

    /// No WebSocket TLS stack may enter the testkit's own dependency tree: those crates pull licences
    /// the allow-list rejects, and the harness only ever speaks to loopback. The tree is asked of
    /// `cargo tree -p dilla-testkit`, which resolves features for the testkit alone: Cargo.lock is
    /// shared with the GPUI desktop client (clients/desktop), which legitimately carries rustls and
    /// so puts it in the lock's union of every member's features.
    ///
    /// `openssl-sys` is deliberately NOT on this list. It is in the testkit's tree through
    /// `rusqlite = { features = ["bundled-sqlcipher-vendored-openssl"] }` - it is SQLCipher's, not
    /// a WebSocket TLS stack, and forbidding it would make this test fail on its first run and
    /// never pass. The four that remain are genuinely absent from that tree today.
    #[test]
    fn no_websocket_tls_stack_is_in_the_lock_file() {
        let cargo = std::env::var("CARGO").unwrap_or_else(|_| "cargo".to_owned());
        let out = std::process::Command::new(cargo)
            .args(["tree", "-p", "dilla-testkit", "-e", "normal,build,dev"])
            .args(["--prefix", "none", "--locked", "--offline"])
            .current_dir(env!("CARGO_MANIFEST_DIR"))
            .output()
            .expect("cargo tree runs");
        assert!(
            out.status.success(),
            "cargo tree failed: {}",
            String::from_utf8_lossy(&out.stderr)
        );
        let tree = String::from_utf8(out.stdout).expect("cargo tree prints UTF-8");
        let packages: Vec<&str> = tree.lines().filter_map(|l| l.split(' ').next()).collect();
        assert!(
            packages.contains(&"rusqlite"),
            "the tree is the testkit's own ({} lines)",
            packages.len()
        );
        for forbidden in ["rustls", "native-tls", "webpki", "ring"] {
            assert!(
                !packages.contains(&forbidden),
                "{forbidden} entered the testkit's dependency tree: a TLS feature was enabled on ureq or tungstenite"
            );
        }
    }
}
