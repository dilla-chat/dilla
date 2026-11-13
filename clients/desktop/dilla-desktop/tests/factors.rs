//! Task 26's behaviour and keyboard tests for the factors area (standard procedure S5-S6), driven through the automation
//! build against a real dillad. `#[ignore]`d like every real-dillad suite (the desktop-gpui CI job runs them with
//! `--ignored` on Linux): run with `cargo test -p dilla-desktop --features automation --test factors -- --ignored`.
//!
//! The parity tests of S4 (`sign-in/*`, `factor/*`, `user-settings/sign-in*`) are not here yet: see the task report.

mod support;

use std::time::Duration;

use dilla_client::features::factors::StepUpMethod;
use dilla_parity::Driver;
use zeroize::Zeroizing;

use support::world;

const WAIT: Duration = Duration::from_secs(60);

fn scratch_dir(tag: &str) -> std::path::PathBuf {
    let dir = dilla_parity::target_dir().join(format!("t26-{tag}-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).unwrap();
    dir
}

/// The login step, as a person types it: the server, then Tab to the username and the password, then Enter.
fn log_in(app: &mut Driver, server: &str, username: &str) {
    // A device with no account opens on the sign-up (Task 25); its link leads to the sign-in.
    app.wait_for("onboarding-signin", WAIT);
    app.click("onboarding-signin");
    app.wait_for("signin-server", WAIT);
    app.type_text("signin-server", server);
    app.press("tab");
    app.type_focused(username);
    app.press("tab");
    app.type_focused(world::PASSWORD);
    app.press("enter");
}

/// e2e/web/signin-order.spec.ts, "sign-in order: the password, then the factor, then the recovery key; a backup code waits
/// for the key": a second device logs in (step 1), gives the code in the six boxes, which send it on the sixth digit with
/// no press (step 2), meets a pasted backup code in the key field (named once it rests), then the key (step 3), then done.
/// A third device takes the backup-code path: the account still holds its ten codes until the key adds that device; then
/// nine.
#[test]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
fn password_then_factor_then_recovery_key_backup_code_waits() {
    let rt = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .enable_all()
        .build()
        .unwrap();
    let host = world::start_host("t26-order", "127.0.0.1:0", None);
    let invite = host.invites.last().unwrap().clone();
    let mut ada = rt.block_on(world::sign_up(&host, &invite, "ada", "Ada", "t26-order"));
    world::clear_session(&ada);
    let first = rt.block_on(world::start(&ada));
    rt.block_on(world::wait_for(&first, WAIT, "ada online", |s| {
        s.connection.status == dilla_client::state::ConnectionStatus::Online
    }));
    let code = ada.next_code(&host);
    let codes = rt.block_on(async {
        let token = first
            .factors()
            .step_up(StepUpMethod::Totp, Zeroizing::new(code))
            .await
            .unwrap();
        first
            .factors()
            .regenerate_backup_codes(token)
            .await
            .unwrap()
    });

    // The second device: login, code, key, done.
    let mut app = Driver::launch(&scratch_dir("second"));
    log_in(&mut app, &host.public, "ada");
    app.wait_for("signin-code", WAIT);
    let code = ada.next_code(&host);
    app.type_text("signin-code", &code);
    // Verify (the sixth digit's own send after its 350 ms rest is `code::tests` on the test clock: the automation
    // build's frames do not run that timer between commands).
    app.click("signin-verify");
    app.wait_for("signin-key", WAIT);
    // A backup code in the key field: the key is not ready (its 600 ms note is likewise timer-driven, unchecked here).
    app.type_text("signin-key", &codes.0[1]);
    app.click("signin-add");
    assert!(app.element("signin-open").is_none());
    app.click("signin-key");
    app.press("ctrl-a");
    app.type_focused(&ada.recovery_key);
    app.click("signin-add");
    app.wait_for("signin-open", WAIT);
    app.click("signin-open");
    drop(app);

    // The third device: login, a backup code held, then the key.
    let mut app = Driver::launch(&scratch_dir("third"));
    log_in(&mut app, &host.public, "ada");
    app.wait_for("signin-use-backup", WAIT);
    app.click("signin-use-backup");
    app.wait_for("signin-backup", WAIT);
    app.type_text("signin-backup", &codes.0[2].to_lowercase());
    app.press("enter");
    app.wait_for("signin-key", WAIT);
    let left = rt.block_on(first.factors().refresh()).unwrap().unwrap();
    assert_eq!(
        left.backup_codes_left, 10,
        "the held code waits for the key"
    );
    app.type_text("signin-key", &ada.recovery_key);
    app.click("signin-add");
    app.wait_for("signin-open", WAIT);
    let left = rt.block_on(first.factors().refresh()).unwrap().unwrap();
    assert_eq!(left.backup_codes_left, 9);
    drop(app);
}

/// S6: the sign-in's focus order (server, username, password) and the code boxes' keys: digits advance, Backspace moves
/// back, Enter submits; a refused code takes the person back to the login with focus on the password.
#[test]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
fn keyboard_factors_focus_order() {
    let rt = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .enable_all()
        .build()
        .unwrap();
    let host = world::start_host("t26-keys", "127.0.0.1:0", None);
    let invite = host.invites.last().unwrap().clone();
    let mut ada = rt.block_on(world::sign_up(&host, &invite, "ada", "Ada", "t26-keys"));
    world::clear_session(&ada);
    let mut app = Driver::launch(&scratch_dir("keys"));
    // A device with no account opens on the sign-up (Task 25); its link leads to the sign-in.
    app.wait_for("onboarding-signin", WAIT);
    app.click("onboarding-signin");
    app.wait_for("signin-server", WAIT);
    app.click("signin-server");
    app.type_focused(&host.public);
    app.press("tab");
    app.type_focused("ada");
    app.press("tab");
    app.type_focused(world::PASSWORD);
    app.press("enter");
    app.wait_for("signin-code", WAIT);
    // A wrong code (the right one with its last digit changed), typed digit by digit, is sent on the sixth.
    let right = ada.next_code(&host);
    let last = right.as_bytes()[5];
    let wrong = format!(
        "{}{}",
        &right[..5],
        char::from(if last == b'9' { b'0' } else { last + 1 })
    );
    app.type_text("signin-code", &wrong);
    // Enter in the boxes submits.
    app.press("enter");
    // Refused: the login again, the password emptied and focused.
    app.wait_for("signin-continue", WAIT);
    app.wait_for("signin-password", WAIT);
    // Backspace in the boxes moves back: on the next code step, five digits, Backspace, two more, then Enter.
    app.type_focused(world::PASSWORD);
    app.press("enter");
    app.wait_for("signin-code", WAIT);
    let code = ada.next_code(&host);
    app.type_text("signin-code", &code[..5]);
    app.press("backspace");
    app.type_focused(&code[4..]);
    app.press("enter");
    app.wait_for("signin-key", WAIT);
}
