//! Task 28: the pre-account screens against the web client's references, and the routing between them. A `signed_out`
//! fixture is a device with no account: `shoot.mjs` opens the route in a fresh browser without signing in and drives
//! the steps against the parity world's dillad; the app draws the same route with no client (`fixture::screen_of`),
//! starting where the instance's answers left the web (`fixture::pre_account`). The `create/*` fixtures are a viewer in
//! no server, signed in: the shell's empty view and its two dialogs.

#![cfg(feature = "automation")]

use std::path::{Path, PathBuf};
use std::time::Duration;

use dilla_desktop::fixture::pre_account::PreAccount;
use dilla_desktop::fixture::{Fixture, FixtureScreen};
use dilla_parity::Driver;

const WAIT: Duration = Duration::from_secs(30);

fn fixture(name: &str) -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../parity/fixtures")
        .join(format!("{name}.json"))
}

fn parity(name: &str) {
    if let Err(e) = dilla_parity::screen::assert_screen(&fixture(name)) {
        panic!("{name}: {e}");
    }
}

/// Each pre-account fixture names the screen it starts on; `/` signed in is the shell's home.
#[test]
fn the_pre_account_fixtures_start_where_the_web_is() {
    for (name, start) in [
        ("sign-in/welcome", PreAccount::SignUp),
        ("sign-in/form", PreAccount::SignIn),
        ("sign-in/factor", PreAccount::SignInFactor),
        ("sign-in/error", PreAccount::SignInError),
        ("onboarding/identity", PreAccount::SignUpIdentity),
        ("onboarding/keys", PreAccount::SignUpKeys),
        ("onboarding/done", PreAccount::SignUpDone),
    ] {
        let f = Fixture::load(&fixture(name), None).unwrap_or_else(|e| panic!("{name}: {e}"));
        assert_eq!(f.screen, FixtureScreen::Welcome(start), "{name}");
        assert!(f.state.communities.is_empty(), "{name}: no account");
    }
    for name in ["create/empty", "create/server", "create/join"] {
        let f = Fixture::load(&fixture(name), None).unwrap_or_else(|e| panic!("{name}: {e}"));
        assert_eq!(f.screen, FixtureScreen::Home, "{name}");
        assert!(
            f.state.communities.is_empty(),
            "{name}: the viewer is in no server"
        );
    }
}

/// Onboarding.tsx / SignIn.tsx routing: `/welcome` on a device with no account is the sign-up's first step ("Join with an
/// invite"), not the sign-in; "Already have an account? Sign in" leads to the sign-in, and its "Create an account
/// instead" back to step 1.
#[test]
#[ignore = "launches the automation build (the desktop-gpui CI job runs it with --ignored on Linux)"]
fn welcome_is_the_sign_up_and_its_link_leads_to_the_sign_in_and_back() {
    let mut app = Driver::launch_fixture(&fixture("sign-in/welcome"), None, &[]);
    app.wait_for("onboarding-invite", WAIT);
    assert!(
        app.element("onboarding-next").is_some(),
        "step 1's Continue"
    );
    assert!(
        app.element("signin-username").is_none(),
        "not the sign-in form"
    );
    app.click("onboarding-signin");
    app.wait_for("signin-username", WAIT);
    assert!(
        app.element("onboarding-invite").is_none(),
        "the sign-up has gone"
    );
    app.click("signin-create");
    app.wait_for("onboarding-invite", WAIT);
    assert!(app.element("signin-username").is_none());
}

/// The probes of the pre-account screens: the sign-in's fields, its factor step and its refusal, each sign-up step.
#[test]
#[ignore = "launches the automation build (the desktop-gpui CI job runs it with --ignored on Linux)"]
fn the_pre_account_screens_answer_their_probes() {
    let expect = |name: &str, present: &[&str], absent: &[&str]| {
        let mut app = Driver::launch_fixture(&fixture(name), None, &[]);
        app.wait_for(present[0], WAIT);
        for id in present {
            assert!(app.element(id).is_some(), "{name}: {id}");
        }
        for id in absent {
            assert!(app.element(id).is_none(), "{name}: no {id}");
        }
    };
    expect(
        "sign-in/form",
        &[
            "signin-username",
            "signin-password",
            "signin-continue",
            "signin-create",
        ],
        &["signin-server", "signin-password-error", "auth-banner"],
    );
    expect(
        "sign-in/factor",
        &[
            "signin-code",
            "signin-verify",
            "signin-use-backup",
            "signin-back",
        ],
        &["signin-username"],
    );
    expect(
        "sign-in/error",
        &[
            "signin-password-error",
            "signin-username",
            "signin-continue",
        ],
        &["signin-code"],
    );
    expect(
        "onboarding/identity",
        &[
            "onboarding-username",
            "onboarding-password",
            "onboarding-back",
        ],
        &["onboarding-invite", "onboarding-username-error"],
    );
    expect(
        "onboarding/keys",
        &[
            "onboarding-key",
            "onboarding-ack",
            "onboarding-save",
            "onboarding-next",
        ],
        &["onboarding-username"],
    );
    expect(
        "onboarding/done",
        &[
            "onboarding-open",
            "onboarding-codes-show",
            "onboarding-codes-ack",
        ],
        &["onboarding-codes"],
    );
    expect(
        "create/empty",
        &["create-empty", "create-empty-start", "create-empty-join"],
        &["create-dialog"],
    );
}

/// Step 1 refuses an empty invite under its field (`onboarding-invite-error`) and stays.
#[test]
#[ignore = "launches the automation build (the desktop-gpui CI job runs it with --ignored on Linux)"]
fn an_empty_invite_is_refused_under_its_field() {
    let mut app = Driver::launch_fixture(&fixture("sign-in/welcome"), None, &[]);
    app.wait_for("onboarding-invite", WAIT);
    assert!(app.element("onboarding-invite-error").is_none());
    app.click("onboarding-next");
    app.wait_for("onboarding-invite-error", WAIT);
    assert!(app.element("onboarding-username").is_none(), "still step 1");
}

#[test]
fn parity_sign_in_welcome() {
    parity("sign-in/welcome");
}

#[test]
fn parity_sign_in_form() {
    parity("sign-in/form");
}

#[test]
fn parity_sign_in_factor() {
    parity("sign-in/factor");
}

#[test]
fn parity_sign_in_error() {
    parity("sign-in/error");
}

#[test]
fn parity_onboarding_identity() {
    parity("onboarding/identity");
}

#[test]
fn parity_onboarding_keys() {
    parity("onboarding/keys");
}

#[test]
fn parity_onboarding_done() {
    parity("onboarding/done");
}

#[test]
fn parity_create_empty() {
    parity("create/empty");
}

#[test]
fn parity_create_server() {
    parity("create/server");
}

#[test]
fn parity_create_join() {
    parity("create/join");
}

/// Package 10b: the create-channel dialog of an owner, private on, one role ticked (Create.tsx).
#[test]
fn parity_create_channel_private() {
    parity("create/channel-private");
}
