//! Workspace-level policy that the compiler cannot enforce on its own.

const CARGO_CONFIG: &str = include_str!("../../../.cargo/config.toml");

#[test]
fn cargo_config_sets_no_rustflags() {
    for (n, line) in CARGO_CONFIG.lines().enumerate() {
        let code = line.split('#').next().unwrap_or("").trim();
        assert!(
            !code.starts_with("rustflags"),
            ".cargo/config.toml line {} sets rustflags; \
             getrandom >= 0.3.4 selects its backend from the `wasm_js` crate feature and a \
             workflow-level RUSTFLAGS would override this table outright",
            n + 1
        );
    }
}

#[test]
fn cargo_config_pins_the_node_runner_for_wasm32_unknown_unknown() {
    assert!(CARGO_CONFIG.contains("[target.wasm32-unknown-unknown]"));
    assert!(CARGO_CONFIG.contains("runner = \"wasm-bindgen-test-runner\""));
}

#[test]
fn cargo_config_pins_no_wasip1_runner() {
    // The wasip1 vector check runs under wazero from Go, not under wasmtime (D6, R7).
    assert!(!CARGO_CONFIG.contains("[target.wasm32-wasip1]"));
}

#[test]
fn version_constants_are_the_wire_values() {
    assert_eq!(dilla_core::E2EE_VERSION, 1);
    assert_eq!(dilla_core::MEDIA_VERSION, 1);
    assert_eq!(dilla_core::WIRE_VERSION, 6);
    // 2 since 2026-09-24: `public_group_process` grew the applied-proposal list and
    // `committer_updated`, and `validate_key_package` grows `kp_ref`. The wasi ABI is internal to
    // dillad's host, so it moves on a response-shape change while the wire versions stay at 1.
    // 3 since 2026-09-29: `public_group_process` grew `new_leaf` and `device_list_entries` exists.
    // 4 since 2026-10-05: `validate_key_package` and the applied items carry the leaf's key.
    // 5 since 2026-10-05 (hardening G): `public_group_state` carries the tree's `leaf_count`.
    // 6 since 2026-10-06 (DS-MEMBERSHIP-01): `public_group_state` carries `external_senders`.
    // 7 since 2026-10-21 (DILLA-BOT-04): WASI adds strict `verify_credential`.
    // 8 since 2026-10-21 (DILLA-BOT-04 Task5): validated KeyPackages return the leaf credential.
    // 9 since 2026-10-22 (DILLA-BOT): authenticated public-group roster extraction.
    // 10 since 2026-10-22 (DILLA-BOT): strict Welcome recipient-ref inspection.
    assert_eq!(dilla_core::ABI_VERSION, 10);
    assert_eq!(dilla_core::CORE_VERSION, "0.1.0");
}
