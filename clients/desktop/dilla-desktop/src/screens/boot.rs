//! The boot splash and the fatal screen (packages/web/src/screens/Boot.tsx, packages/ui/src/Splash).
//!
//! The full-window surface of the app while it starts and when it cannot: the brand, one status line (`› ` in the accent,
//! then the text) with an optional detail under it, and an optional action. The browser's `unsupported` and `other-tab`
//! phases have no counterpart here (the store is the OS keychain's, and an instance has no second tab).
//!
//! - [`loading`]: "Starting dilla" while `Client::start` runs.
//! - [`fatal`]: "dilla could not start" with the stable code of what failed and a Try again action. The code is
//!   [`fatal_code`], never the error's text: that can name a path or a server.
//! - `revoked` (Package 10d): the instance no longer accepts this device (its start was refused as revoked, unlisted or
//!   out of its session's scope). The web's words, and Forget this browser behind a confirmation, which wipes this
//!   device's data for the instance and leaves the account's device list alone (Boot.tsx `forgetBrowser`).
//! - `store-lost`: the key that opens the instance's store is gone from the keychain. Reset this browser behind a
//!   confirmation wipes the data the key opened (Boot.tsx `resetDevice`); `resetting` shows while it runs.
//!
//! The words are the web's (`boot.*`): "Reload the page to try again" is what the web says, and parity keeps it; the
//! action restarts the client.

use std::rc::Rc;

use dilla_client::ClientError;
use dilla_client::features::diagnostics::stable_code;
use gpui::{AnyElement, App, ElementId, IntoElement, ParentElement, Styled, Window, div};

use dilla_desktop_ui::components::button::{Button, ButtonSize, ButtonVariant};
use dilla_desktop_ui::components::dialog::{Dialog, DialogKind};
use dilla_desktop_ui::metrics::LABEL_TRACKING_WIDE;
use dilla_desktop_ui::text::{MONO_ADVANCE, line, line_height, paragraph};
use dilla_desktop_ui::tokens::{color, font, radius, text};

use crate::strings::{t, tf};
use crate::tracked;

/// What the splash draws.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Boot {
    Loading,
    Fatal {
        code: String,
    },
    /// The store was written by a newer dilla (`E_STORE_NEWER`): explained, never offered a reset.
    StoreNewer {
        found: u32,
        supported: u32,
    },
    /// The instance no longer accepts this device: Forget this browser.
    Revoked,
    /// The store's key is gone from the keychain: Reset this browser.
    StoreLost,
    /// A Forget or a Reset is wiping this device's data.
    Resetting,
}

impl Boot {
    /// The status line's `strings.rs` key.
    pub fn status_key(&self) -> &'static str {
        match self {
            Self::Loading => "boot.loading.status",
            Self::Fatal { .. } => "boot.error.status",
            Self::StoreNewer { .. } => "boot.storeNewer.status",
            Self::Revoked => "boot.revoked.status",
            Self::StoreLost => "boot.storeLost.status",
            Self::Resetting => "boot.resetting.status",
        }
    }

    /// The detail under the status, if any.
    pub fn detail(&self) -> Option<String> {
        match self {
            Self::Loading | Self::Resetting => None,
            Self::Revoked => Some(t("boot.revoked.detail").to_owned()),
            Self::StoreLost => Some(t("boot.storeLost.detail").to_owned()),
            Self::Fatal { code } => Some(tf("boot.error.detail", &[("code", code)])),
            Self::StoreNewer { found, supported } => Some(tf(
                "boot.storeNewer.detail",
                &[
                    ("found", &found.to_string()),
                    ("supported", &supported.to_string()),
                ],
            )),
        }
    }

    /// The action's label, if any.
    pub fn action_key(&self) -> Option<&'static str> {
        match self {
            Self::Loading | Self::Resetting => None,
            Self::Revoked => Some("boot.revoked.action"),
            Self::StoreLost => Some("boot.storeLost.action"),
            Self::Fatal { .. } => Some("boot.error.action"),
            Self::StoreNewer { .. } => Some("boot.storeNewer.action"),
        }
    }

    /// The quieter second action's label: back to the instance picker and sign-in, as the mobile splash's
    /// `secondary` (`MobileBootError`). Only the failure splash has one.
    pub fn secondary_key(&self) -> Option<&'static str> {
        match self {
            Self::Fatal { .. } => Some("mobile.switchInstance"),
            _ => None,
        }
    }

    /// The confirmation behind the action (Boot.tsx's dialogs): its title, body and confirm keys. Only the two resets ask.
    pub fn confirm_keys(&self) -> Option<(&'static str, &'static str, &'static str)> {
        match self {
            Self::Revoked => Some((
                "boot.revoked.confirmTitle",
                "boot.revoked.confirmBody",
                "boot.revoked.confirmAction",
            )),
            Self::StoreLost => Some((
                "boot.storeLost.confirmTitle",
                "boot.storeLost.confirmBody",
                "boot.storeLost.confirmAction",
            )),
            _ => None,
        }
    }
}

/// What a failed `Client::start` leads to.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum StartFailure {
    /// No account here yet, or a factor owed: the sign-up or the sign-in screen answers it.
    SignIn,
    /// The instance no longer accepts this device (revoked, unlisted, session scope): the web's `enterRevoked`.
    Revoked,
    /// The store's key is gone from the keychain: the web's `store-lost`.
    StoreLost,
    /// The store is newer than this build.
    StoreNewer,
    /// Anything else: the fatal splash with its stable code.
    Fatal,
}

/// Sorts a failed start. `E_STORE_NEWER` arrives as a `Sync` code until the store reports its schema versions.
pub fn classify(error: &ClientError) -> StartFailure {
    match error {
        ClientError::FactorRequired | ClientError::NoIdentity => StartFailure::SignIn,
        ClientError::Revoked => StartFailure::Revoked,
        // dilla-client's `StoreError::KeyMissing`, as the start reports it.
        ClientError::Store(m) if m.contains("key is missing") => StartFailure::StoreLost,
        ClientError::Sync(code) => match code.as_str() {
            "E_SESSION_SCOPE" | "E_DEVICE_UNLISTED" | "E_DEVICE_REVOKED" => StartFailure::Revoked,
            "E_STORE_NEWER" => StartFailure::StoreNewer,
            _ => StartFailure::Fatal,
        },
        _ => StartFailure::Fatal,
    }
}

/// The splash a failed start shows. The versions are not reported by the client yet, so they read 0 as the web's
/// `?? 0` does.
pub fn failure_boot(error: &ClientError) -> Boot {
    match classify(error) {
        StartFailure::StoreNewer => Boot::StoreNewer {
            found: 0,
            supported: 0,
        },
        StartFailure::Revoked => Boot::Revoked,
        StartFailure::StoreLost => Boot::StoreLost,
        _ => Boot::Fatal {
            code: fatal_code(error),
        },
    }
}

/// The stable code a failed start shows: the client's own code where it has one, else a fixed one per kind. Never the
/// error's text.
pub fn fatal_code(error: &ClientError) -> String {
    let code = match error {
        ClientError::Store(_) => "E_STORE",
        ClientError::NoIdentity => "E_NO_IDENTITY",
        ClientError::Revoked => "E_REVOKED",
        ClientError::FactorRequired => "E_FACTOR_REQUIRED",
        ClientError::Network(_) => "E_NETWORK",
        ClientError::Sync(code) => code.as_str(),
        ClientError::Runtime(_) => "E_RUNTIME",
        _ => "E_UNKNOWN",
    };
    stable_code(code).to_owned()
}

/// The detail's `max-width` in characters (Splash.css `46ch`).
const DETAIL_COLUMNS: f32 = 46.0;

pub type Press = Rc<dyn Fn(&mut Window, &mut App)>;
/// Opens (`true`) or closes a confirmation.
pub type Toggle = Rc<dyn Fn(bool, &mut Window, &mut App)>;

/// The splash: the brand, the status and detail, and the action when there is one. It fills its parent.
pub fn splash(boot: &Boot, on_action: Option<Press>, on_secondary: Option<Press>) -> AnyElement {
    let status = div()
        .flex()
        .flex_col()
        .items_center()
        .gap_2()
        .child(
            div()
                .flex()
                .child(line(
                    "› ",
                    text::MD,
                    line_height(text::MD, 1.5),
                    color::ACCENT,
                ))
                .child(line(
                    t(boot.status_key()),
                    text::MD,
                    line_height(text::MD, 1.5),
                    color::FG,
                )),
        )
        .children(boot.detail().map(|detail| {
            // `max-width: 46ch` at the detail's size, centred.
            let width = DETAIL_COLUMNS * f32::from(text::SM) * MONO_ADVANCE;
            div().flex().flex_col().items_center().children(
                paragraph(
                    &detail,
                    text::SM,
                    line_height(text::SM, 1.5),
                    color::FG_2,
                    width,
                )
                .into_iter()
                .map(IntoElement::into_any_element),
            )
        }));
    let action = boot.action_key().zip(on_action).map(|(key, on_action)| {
        Button::new(ElementId::from("boot-action"), t(key))
            .variant(ButtonVariant::Accent)
            .on_click(move |_, window, cx| on_action(window, cx))
    });
    let secondary = boot.secondary_key().zip(on_secondary).map(|(key, press)| {
        Button::new(ElementId::from("boot-secondary"), t(key))
            .variant(ButtonVariant::Ghost)
            .on_click(move |_, window, cx| press(window, cx))
    });
    tracked(
        "boot-splash",
        div()
            .size_full()
            .bg(color::BG)
            .flex()
            .flex_col()
            .items_center()
            .justify_center()
            .gap_5()
            .p_6()
            .child(brand())
            .child(status)
            .children(action)
            .children(secondary),
    )
    .into_any_element()
}

/// "Starting dilla".
pub fn loading() -> AnyElement {
    splash(&Boot::Loading, None, None)
}

/// What the failure splash's buttons do: the action (Try again, or open the reset's confirmation), the secondary (back
/// to the sign-in), and the confirmation's own buttons.
pub struct FailureActions {
    pub on_retry: Press,
    pub on_sign_in: Press,
    /// Opens (`true`) or closes the reset's confirmation.
    pub on_confirming: Toggle,
    /// The confirmed Forget or Reset.
    pub on_reset: Press,
}

/// The splash for a failed start: "dilla could not start" with Try again, the revoked device's Forget this browser, or the
/// lost store's Reset this browser, each reset behind its confirmation (open while `confirming`).
pub fn fatal(
    error: &ClientError,
    confirming: bool,
    viewport_width: f32,
    actions: FailureActions,
) -> AnyElement {
    let boot = failure_boot(error);
    let FailureActions {
        on_retry,
        on_sign_in,
        on_confirming,
        on_reset,
    } = actions;
    let Some((title, body, confirm)) = boot.confirm_keys() else {
        return splash(&boot, Some(on_retry), Some(on_sign_in));
    };
    let open = on_confirming.clone();
    let page = splash(
        &boot,
        Some(Rc::new(move |window, cx| open(true, window, cx))),
        None,
    );
    let close = on_confirming;
    let dialog = Dialog::new("boot-confirm", t(title), t("dialog.close"))
        .open(confirming)
        .body(
            div().flex().flex_col().children(
                paragraph(
                    t(body),
                    text::UI,
                    line_height(text::UI, 1.5),
                    color::FG_2,
                    DialogKind::Form.body_width(viewport_width),
                )
                .into_iter()
                .map(IntoElement::into_any_element),
            ),
        )
        .footer(
            Button::new(ElementId::from("boot-confirm-action"), t(confirm))
                .variant(ButtonVariant::Danger)
                .size(ButtonSize::Dialog)
                .on_click(move |_, window, cx| on_reset(window, cx)),
        )
        .on_close(move |window, cx| close(false, window, cx));
    div()
        .size_full()
        .relative()
        .child(page)
        .child(dialog)
        .into_any_element()
}

/// "Clearing this browser's data", while a Forget or a Reset wipes the instance's data.
pub fn resetting() -> AnyElement {
    splash(&Boot::Resetting, None, None)
}

/// The wordmark, page size: the 20px tile, DILLA and the caret (`BrandMark`, `size="page"`).
fn brand() -> impl IntoElement {
    let tile = div()
        .flex_none()
        .size_5()
        .flex()
        .items_center()
        .justify_center()
        .rounded(radius::SM)
        .bg(color::BRAND)
        .child(
            line("D", text::UI, line_height(text::UI, 1.0), color::BRAND_INK)
                .weight(font::WEIGHT_DISPLAY),
        );
    div()
        .flex()
        .items_center()
        .gap_2()
        .child(tile)
        .child(
            line("DILLA", text::XS, line_height(text::XS, 1.5), color::FG)
                .weight(font::WEIGHT_DISPLAY)
                .tracking(LABEL_TRACKING_WIDE),
        )
        .child(div().flex_none().w_1p5().h_3().bg(color::BRAND))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_fatal_code_is_stable_and_never_the_errors_text() {
        let path_error =
            ClientError::Store("/home/ada/.local/share/dilla: permission denied".into());
        assert_eq!(fatal_code(&path_error), "E_STORE");
        assert_eq!(
            fatal_code(&ClientError::Network(
                "https://secret.example refused".into()
            )),
            "E_NETWORK"
        );
        assert_eq!(
            fatal_code(&ClientError::Sync("E_COMMIT_REQUIRED".into())),
            "E_COMMIT_REQUIRED"
        );
        assert_eq!(
            fatal_code(&ClientError::Sync("some detail text".into())),
            "E_UNKNOWN"
        );
    }

    #[test]
    fn failed_starts_are_sorted_like_the_webs_boot() {
        use StartFailure::*;
        let sync = |c: &str| ClientError::Sync(c.into());
        assert_eq!(classify(&ClientError::Revoked), Revoked);
        assert_eq!(classify(&ClientError::FactorRequired), SignIn);
        assert_eq!(classify(&ClientError::NoIdentity), SignIn);
        assert_eq!(classify(&sync("E_SESSION_SCOPE")), Revoked);
        assert_eq!(classify(&sync("E_DEVICE_UNLISTED")), Revoked);
        assert_eq!(classify(&sync("E_DEVICE_REVOKED")), Revoked);
        assert_eq!(
            classify(&ClientError::Store(
                "the database key is missing from the keychain".into()
            )),
            StoreLost
        );
        assert_eq!(classify(&ClientError::Store("disk full".into())), Fatal);
        assert_eq!(classify(&sync("E_STORE_NEWER")), StoreNewer);
        assert_eq!(classify(&sync("E_COMMIT_REQUIRED")), Fatal);
        assert_eq!(classify(&ClientError::Network("x".into())), Fatal);
        assert!(matches!(
            failure_boot(&sync("E_STORE_NEWER")),
            Boot::StoreNewer { .. }
        ));
    }

    #[test]
    fn the_failure_splashes_have_their_words_and_a_way_out() {
        let fatal = Boot::Fatal { code: "E_X".into() };
        assert_eq!(fatal.secondary_key(), Some("mobile.switchInstance"));
        let newer = Boot::StoreNewer {
            found: 9,
            supported: 7,
        };
        assert!(newer.detail().unwrap().contains("data version 9"));
        assert_eq!(newer.secondary_key(), None);
        for key in [
            newer.status_key(),
            newer.action_key().unwrap(),
            "mobile.switchInstance",
        ] {
            assert!(crate::strings::try_t(key).is_some(), "{key}");
        }
    }

    /// Boot.tsx: the revoked and store-lost splashes say what happened and offer their reset, each behind its
    /// confirmation; neither offers Try again, and every word is the web's.
    #[test]
    fn the_reset_splashes_explain_and_ask_first() {
        for (boot, action) in [
            (Boot::Revoked, "boot.revoked.action"),
            (Boot::StoreLost, "boot.storeLost.action"),
        ] {
            assert!(boot.detail().is_some());
            assert_eq!(boot.action_key(), Some(action));
            assert_eq!(boot.secondary_key(), None);
            let (title, body, confirm) = boot.confirm_keys().unwrap();
            for key in [boot.status_key(), action, title, body, confirm] {
                assert!(crate::strings::try_t(key).is_some(), "{key}");
            }
        }
        assert_eq!(Boot::Resetting.action_key(), None);
        assert_eq!(Boot::Resetting.detail(), None);
        assert_eq!(Boot::Fatal { code: "E_X".into() }.confirm_keys(), None);
    }

    #[test]
    fn loading_has_no_detail_or_action_and_fatal_names_its_code() {
        assert_eq!(Boot::Loading.detail(), None);
        assert_eq!(Boot::Loading.action_key(), None);
        let fatal = Boot::Fatal {
            code: "E_NETWORK".into(),
        };
        assert_eq!(
            fatal.detail().as_deref(),
            Some("Error E_NETWORK. Reload the page to try again.")
        );
        assert_eq!(fatal.action_key(), Some("boot.error.action"));
        for key in [Boot::Loading.status_key(), fatal.status_key()] {
            assert!(crate::strings::try_t(key).is_some());
        }
    }
}
