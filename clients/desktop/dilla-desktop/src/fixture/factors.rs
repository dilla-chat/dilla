//! The factors facet (Task 2d of the docs-screenshots plan): what the second-factor screens draw in a fixture with no
//! client, keyed by the fixture's name as `gifs` and `invites` are (the spec has no factors).
//!
//! - `user-settings/sign-in`: the account's factors slice (`AppState::factors`), as the instance's
//!   `GET /v1/auth/factors` would answer: a password, the authenticator app, one passkey and eight backup codes left.
//! - `onboarding/factor-choose` and `onboarding/factor-app`: a new account owing its factor (the slice, `owed`), and for
//!   the app step the setup key [`totp_key`] the instance would have handed out.
//!
//! Every secret here is fictional: the setup key is a made-up base32 string, never one of a real account.

use dilla_client::features::factors::{InstanceDoc, TotpKey};
use dilla_client::state::AppState;
use dilla_client::state::factors::{Factors, PasskeyRow};
use zeroize::Zeroizing;

use super::pre_account::INSTANCE_NAME;

/// The fictional TOTP setup key the `onboarding/factor-app` fixture shows (base32, 32 characters).
pub const TOTP_SECRET: &str = "DILLAFIXTURETOTPKEYFORDOCSHOTSAA";

const DAY: u64 = 86_400;

/// Maps the facet into `state` (fixtures of a signed-in device).
pub(super) fn apply(name: &str, now: u64, state: &mut AppState) {
    if let Some(f) = factors_of(name, now) {
        state.factors.factors = Some(f);
    }
}

/// The account's factors for fixture `name`, if it names them.
pub fn factors_of(name: &str, now: u64) -> Option<Factors> {
    match name {
        "user-settings/sign-in" => Some(Factors {
            owed: false,
            totp: Some(now - 30 * DAY),
            passkeys: vec![PasskeyRow {
                id: "a1".repeat(16),
                name: "Work laptop".into(),
                created: now - 12 * DAY,
                last_used: Some(now - 2 * DAY),
            }],
            backup_codes_left: 8,
            has_password: true,
            password_changed: Some(now - 45 * DAY),
        }),
        "onboarding/factor-choose" | "onboarding/factor-app" => Some(Factors {
            owed: true,
            has_password: true,
            ..Factors::default()
        }),
        _ => None,
    }
}

/// The setup key of fixture `name`: the app step's only.
pub fn totp_key(name: &str, username: &str) -> Option<TotpKey> {
    (name == "onboarding/factor-app").then(|| TotpKey {
        secret: Zeroizing::new(TOTP_SECRET.to_owned()),
        uri: Zeroizing::new(format!(
            "otpauth://totp/{INSTANCE_NAME}:{username}?secret={TOTP_SECRET}&issuer={INSTANCE_NAME}"
        )),
    })
}

/// The instance's document as the fixtures' instance (dilla-testhost, `dilla.test`) answers it: passkeys on, so the
/// screens offer them as a live instance with WebAuthn does.
pub fn instance_doc() -> InstanceDoc {
    InstanceDoc {
        name: INSTANCE_NAME.to_owned(),
        passkeys: true,
        oidc: false,
    }
}

/// The fixtures' instance address.
pub fn server() -> url::Url {
    url::Url::parse(&format!("https://{INSTANCE_NAME}")).expect("a url")
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn only_the_named_fixtures_get_factors_and_only_the_app_step_a_key() {
        assert!(factors_of("user-settings/devices", 1).is_none());
        let settings = factors_of("user-settings/sign-in", 100 * DAY).unwrap();
        assert!(settings.holds_factor() && !settings.owed);
        assert!(factors_of("onboarding/factor-choose", 1).unwrap().owed);
        assert!(totp_key("onboarding/factor-choose", "ada").is_none());
        let key = totp_key("onboarding/factor-app", "ada").unwrap();
        assert_eq!(key.secret.as_str(), TOTP_SECRET);
        assert!(
            TOTP_SECRET
                .bytes()
                .all(|b| b.is_ascii_uppercase() || (b'2'..=b'7').contains(&b))
        );
    }
}
