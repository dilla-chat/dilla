//! Task 24's client tests for the diagnostics area (standard procedure S2), against a real dillad.
//!
//! Needs `target/dilla-testhost` and the WASI core (see `tests/sync.rs`); `#[ignore]`d so a plain `cargo test` needs no Go
//! toolchain. The pure parts (the route round trips, the stable code shape, Copy, the app build hint) are unit
//! tests in `src/state/diagnostics.rs` and `src/features/diagnostics.rs`; the three behaviour tests of the web e2e suite
//! this area replaces are in `dilla-desktop/tests/diagnostics.rs` (they count HTTP requests, which needs the proxy).

use std::time::Duration;

use dilla_client::features::diagnostics::copy_text;
use dilla_client::state::ConnectionStatus;
use dilla_client::state::diagnostics::{ERROR_LIMIT, SLOW_ROUTE_LIMIT};
use dilla_client::testing::host::{clear_session, community, sign_up, start, start_host, wait_for};

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn a_running_client_records_codes_routes_and_facts() {
    let host = tokio::task::spawn_blocking(|| start_host("diagnostics", "127.0.0.1:0", None))
        .await
        .unwrap();
    let invite = host.invites.last().unwrap().clone();
    let alice = sign_up(&host, &invite, "alice", "Alice", "diagnostics").await;
    let (host, alice) = tokio::task::spawn_blocking(move || {
        community(&host, &alice, "diag", &[]);
        clear_session(&alice);
        (host, alice)
    })
    .await
    .unwrap();
    let client = start(&alice).await;
    let state = wait_for(&client, Duration::from_secs(60), "online", |s| {
        s.connection.status == ConnectionStatus::Online
    })
    .await;
    // The connection facts the section shows come from the connection slice.
    assert_eq!(state.connection.wire_version, Some(6));
    assert!(state.connection.generation.is_some());

    let diag = client.diagnostics();
    // The last twenty stable codes, oldest first; a detail text is never kept.
    for i in 0..ERROR_LIMIT + 5 {
        diag.record_error(&format!("E_CODE_{i}"));
    }
    diag.record_error("the token is abc123");
    let s = wait_for(&client, Duration::from_secs(5), "the codes", |s| {
        s.diagnostics.errors.len() == ERROR_LIMIT
            && s.diagnostics
                .errors
                .last()
                .is_some_and(|e| e.code == "E_UNKNOWN")
    })
    .await;
    assert_eq!(s.diagnostics.errors[0].code, "E_CODE_6");
    let copied = copy_text(&s.diagnostics);
    assert_eq!(copied.lines().count(), ERROR_LIMIT);
    assert!(!copied.contains("token") && !copied.contains("abc123"));

    // Routes: only the slowest five are published, slowest first, by pattern.
    for i in 0..SLOW_ROUTE_LIMIT + 3 {
        let name = char::from(b'a' + u8::try_from(i).unwrap());
        diag.record_route(
            &format!("GET /v1/route-{name}/{{id}}"),
            10.0 * (i as f64 + 1.0),
        );
    }
    diag.record_route("GET /v1/with/a/real/id?x=1", 5000.0);
    let s = wait_for(&client, Duration::from_secs(5), "the routes", |s| {
        s.diagnostics.slow_routes.len() == SLOW_ROUTE_LIMIT
            && s.diagnostics.slow_routes[0].pattern == "GET /v1/route-h/{id}"
    })
    .await;
    let patterns: Vec<_> = s
        .diagnostics
        .slow_routes
        .iter()
        .map(|r| r.pattern.as_str())
        .collect();
    assert_eq!(
        patterns[0], "GET /v1/route-h/{id}",
        "slowest first, a path with a query is not recorded"
    );

    client.stop();
    drop(host);
}
