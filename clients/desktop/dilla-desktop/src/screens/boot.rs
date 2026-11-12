//! The boot splash and the fatal screen (packages/web/src/screens/Boot.tsx, packages/ui/src/Splash).
//!
//! The full-window surface of the app while it starts and when it cannot: the brand, one status line (`› ` in the accent,
//! then the text) with an optional detail under it, and an optional action. Only the two phases a native app reaches are
//! drawn; the browser's `unsupported`, `other-tab`, `store-lost` and `resetting` phases have no counterpart here (the
//! store is the OS keychain's, and an instance has no second tab), and `revoked` is the sign-in flow's (Task 26).
//!
//! - [`loading`]: "Starting dilla" while `Client::start` runs.
//! - [`fatal`]: "dilla could not start" with the stable code of what failed and a Try again action. The code is
//!   [`fatal_code`], never the error's text: that can name a path or a server.
//!
//! The words are the web's (`boot.*`): "Reload the page to try again" is what the web says, and parity keeps it; the
//! action restarts the client.

use std::rc::Rc;

use dilla_client::ClientError;
use dilla_client::features::diagnostics::stable_code;
use gpui::{AnyElement, App, ElementId, IntoElement, ParentElement, Styled, Window, div};

use dilla_desktop_ui::components::button::{Button, ButtonVariant};
use dilla_desktop_ui::metrics::LABEL_TRACKING_WIDE;
use dilla_desktop_ui::text::{MONO_ADVANCE, line, line_height, paragraph};
use dilla_desktop_ui::tokens::{color, font, radius, text};

use crate::strings::{t, tf};
use crate::tracked;

/// What the splash draws.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Boot {
    Loading,
    Fatal { code: String },
}

impl Boot {
    /// The status line's `strings.rs` key.
    pub fn status_key(&self) -> &'static str {
        match self {
            Self::Loading => "boot.loading.status",
            Self::Fatal { .. } => "boot.error.status",
        }
    }

    /// The detail under the status, if any.
    pub fn detail(&self) -> Option<String> {
        match self {
            Self::Loading => None,
            Self::Fatal { code } => Some(tf("boot.error.detail", &[("code", code)])),
        }
    }

    /// The action's label, if any.
    pub fn action_key(&self) -> Option<&'static str> {
        match self {
            Self::Loading => None,
            Self::Fatal { .. } => Some("boot.error.action"),
        }
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

type Press = Rc<dyn Fn(&mut Window, &mut App)>;

/// The splash: the brand, the status and detail, and the action when there is one. It fills its parent.
pub fn splash(boot: &Boot, on_action: Option<Press>) -> AnyElement {
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
            let width = 46.0 * f32::from(text::SM) * MONO_ADVANCE;
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
            .children(action),
    )
    .into_any_element()
}

/// "Starting dilla".
pub fn loading() -> AnyElement {
    splash(&Boot::Loading, None)
}

/// "dilla could not start", with `on_retry` behind its action.
pub fn fatal(
    error: &ClientError,
    on_retry: impl Fn(&mut Window, &mut App) + 'static,
) -> AnyElement {
    splash(
        &Boot::Fatal {
            code: fatal_code(error),
        },
        Some(Rc::new(on_retry)),
    )
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
