//! "Confirm it is you" (web `settings/StepUpDialog.tsx`): turns the account's password (or, with no password, the code
//! from the authenticator app or a backup code, or else a passkey in the system browser) into a step-up token, one use
//! and short-lived, and hands it to the caller, who spends it on exactly one change. The typed secret goes once into
//! `Factors::step_up` and the field is emptied the moment it is sent; the token is never kept here.
//!
//! Each press is an attempt; Cancel, Escape or a newer press ends it, and a token that arrives for an ended attempt is
//! dropped, so closing the dialog grants nothing.

use dilla_client::features::browser_auth::{BrowserAuth, BrowserAuthError, Cancel, CeremonyKind};
use dilla_client::features::factors::{
    FactorError, Factors, StepUpMethod, StepUpToken, is_backup_code, is_six_digits,
    normalise_backup_code,
};
use gpui::{
    AppContext as _, Context, Entity, EventEmitter, IntoElement, ParentElement, Render, Styled,
    Subscription, Window, div,
};
use gpui_component::input::{InputEvent, InputState};
use zeroize::Zeroizing;

use dilla_desktop_ui::components::banner::{Banner, BannerTone};
use dilla_desktop_ui::components::confirm_dialog::{ConfirmDialog, Tone};
use dilla_desktop_ui::components::text_field::TextField;

use super::setup::shared_refusal;
use crate::strings::t;

/// Why the person is asked again (shown above the field until the next attempt).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Notice {
    /// The last token ran out.
    Expired,
    /// An add that spent it ended without adding anything.
    Again,
}

/// What the dialog proves the person with.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Proof {
    Password,
    /// No password and no passkey: the app's code or a backup code.
    Code,
    /// No password: a passkey.
    Passkey,
}

/// What the dialog tells its host.
pub enum StepUpEvent {
    Token(StepUpToken),
    Close,
}

pub struct StepUpDialog {
    factors: Factors,
    browser: BrowserAuth,
    server: url::Url,
    proof: Proof,
    notice: Option<Notice>,
    field: Entity<InputState>,
    busy: bool,
    error: Option<String>,
    attempt: u64,
    /// The passkey ceremony waiting on the system browser: Cancel, Escape and the dialog going away end it.
    ceremony: Option<Cancel>,
    _subscription: Subscription,
}

impl EventEmitter<StepUpEvent> for StepUpDialog {}

/// StepUpDialog `say`: a refused step-up by status and code only.
fn say(e: &FactorError, proof: Proof) -> String {
    if e.is_step_up() {
        return t(match proof {
            Proof::Password => "factorSettings.stepUp.failed",
            Proof::Code => "factorSettings.stepUp.codeFailed",
            Proof::Passkey => "factorSettings.stepUp.passkeyFailed",
        })
        .to_owned();
    }
    shared_refusal(e)
        .unwrap_or_else(|| crate::strings::tf("factorSettings.error.other", &[("code", &e.code)]))
}

impl StepUpDialog {
    pub fn new(
        factors: Factors,
        server: url::Url,
        proof: Proof,
        notice: Option<Notice>,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) -> Self {
        let field = cx.new(|cx| InputState::new(window, cx).masked(proof == Proof::Password));
        let subscription =
            cx.subscribe_in(&field, window, |this, _, e: &InputEvent, window, cx| {
                if matches!(e, InputEvent::PressEnter { .. }) {
                    this.submit(window, cx);
                }
            });
        field.update(cx, |s, cx| s.focus(window, cx));
        Self {
            factors,
            browser: BrowserAuth::default(),
            server,
            proof,
            notice,
            field,
            busy: false,
            error: None,
            attempt: 0,
            ceremony: None,
            _subscription: subscription,
        }
    }

    /// The system browser the passkey proof opens (tests give their own).
    pub fn with_browser(mut self, browser: BrowserAuth) -> Self {
        self.browser = browser;
        self
    }

    fn refused(&mut self, message: String, window: &mut Window, cx: &mut Context<Self>) {
        self.busy = false;
        self.error = Some(message);
        self.field.update(cx, |s, cx| s.focus(window, cx));
        cx.notify();
    }

    pub fn submit(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        if self.busy {
            return;
        }
        let typed = Zeroizing::new(self.field.read(cx).value().to_string());
        let reply = match self.proof {
            Proof::Password | Proof::Code if typed.is_empty() => {
                return self.refused(t("factor.error.required").to_owned(), window, cx);
            }
            Proof::Password => self.factors.step_up(StepUpMethod::Password, typed),
            Proof::Code => {
                let digits: String = typed.chars().filter(|c| !c.is_whitespace()).collect();
                let digits = Zeroizing::new(digits);
                if is_six_digits(&digits) {
                    self.factors.step_up(StepUpMethod::Totp, digits)
                } else if is_backup_code(&normalise_backup_code(&typed)) {
                    self.factors.step_up(StepUpMethod::Backup, typed)
                } else {
                    return self.refused(
                        t("factorSettings.stepUp.codeFailed").to_owned(),
                        window,
                        cx,
                    );
                }
            }
            Proof::Passkey => {
                self.passkey(window, cx);
                return;
            }
        };
        self.field.update(cx, |s, cx| s.set_value("", window, cx));
        self.notice = None;
        self.busy = true;
        self.error = None;
        self.attempt += 1;
        let attempt = self.attempt;
        let proof = self.proof;
        cx.spawn_in(window, async move |this, cx| {
            let answer = reply.await;
            let _ = this.update_in(cx, |this, window, cx| {
                if this.attempt != attempt {
                    return;
                }
                match answer {
                    Ok(token) => {
                        this.busy = false;
                        cx.emit(StepUpEvent::Token(token));
                    }
                    Err(e) => this.refused(say(&e, proof), window, cx),
                }
            });
        })
        .detach();
    }

    fn passkey(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        self.busy = true;
        self.error = None;
        self.notice = None;
        self.attempt += 1;
        let attempt = self.attempt;
        let (factors, browser, server) = (
            self.factors.clone(),
            self.browser.clone(),
            self.server.clone(),
        );
        let cancel = Cancel::new();
        if let Some(old) = self.ceremony.replace(cancel.clone()) {
            old.cancel();
        }
        cx.spawn_in(window, async move |this, cx| {
            enum Ended {
                Instance(FactorError),
                Browser(BrowserAuthError),
            }
            let run = async {
                let finish_guard = cancel.clone();
                let ceremony = factors
                    .step_up_passkey_begin()
                    .await
                    .map_err(Ended::Instance)?;
                let (tx, rx) = tokio::sync::oneshot::channel();
                let options = ceremony.options.clone();
                std::thread::spawn(move || {
                    let result = tokio::runtime::Builder::new_current_thread()
                        .enable_all()
                        .build()
                        .map_err(|e| BrowserAuthError::Network(e.to_string()))
                        .and_then(|rt| {
                            rt.block_on(browser.run_cancellable(
                                &server,
                                CeremonyKind::Get,
                                &options,
                                &cancel,
                            ))
                        });
                    let _ = tx.send(result);
                });
                let result = rx
                    .await
                    .unwrap_or(Err(BrowserAuthError::Unavailable))
                    .map_err(Ended::Browser)?;
                let response = result
                    .response
                    .ok_or(Ended::Browser(BrowserAuthError::Invalid(
                        "browser completion",
                    )))?;
                // A reply that landed just before a cancel is not applied.
                if finish_guard.is_cancelled() {
                    return Err(Ended::Browser(BrowserAuthError::Cancelled));
                }
                factors
                    .step_up_passkey_finish(ceremony.id, response)
                    .await
                    .map_err(Ended::Instance)
            };
            let answer = run.await;
            let _ = this.update_in(cx, |this, window, cx| {
                if this.attempt != attempt {
                    return;
                }
                match answer {
                    Ok(token) => {
                        this.busy = false;
                        cx.emit(StepUpEvent::Token(token));
                    }
                    Err(Ended::Instance(e)) => this.refused(say(&e, Proof::Passkey), window, cx),
                    Err(Ended::Browser(e)) => {
                        let key = match e {
                            BrowserAuthError::Unsupported => {
                                "factorSettings.stepUp.passkeyUnsupported"
                            }
                            BrowserAuthError::RateLimited => "factor.error.rateLimited",
                            _ => "factorSettings.stepUp.passkeyFailed",
                        };
                        this.refused(t(key).to_owned(), window, cx);
                    }
                }
            });
        })
        .detach();
    }

    fn close(&mut self, cx: &mut Context<Self>) {
        // A token for this attempt is dropped from now on, and a ceremony under way ends.
        self.attempt += 1;
        if let Some(c) = self.ceremony.take() {
            c.cancel();
        }
        self.busy = false;
        cx.emit(StepUpEvent::Close);
    }
}

impl Render for StepUpDialog {
    fn render(&mut self, _window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let (body, confirm) = match self.proof {
            Proof::Password => (
                "factorSettings.stepUp.body",
                "factorSettings.stepUp.confirm",
            ),
            Proof::Code => (
                "factorSettings.stepUp.codeBody",
                "factorSettings.stepUp.confirm",
            ),
            Proof::Passkey => (
                "factorSettings.stepUp.passkeyBody",
                "factorSettings.stepUp.usePasskey",
            ),
        };
        let mut fields = div().flex().flex_col();
        if let Some(notice) = self.notice {
            fields = fields.child(Banner::new(
                "stepup-notice",
                BannerTone::Warn,
                t(match notice {
                    Notice::Again => "factorSettings.stepUp.again",
                    Notice::Expired => "factorSettings.stepUp.expired",
                }),
            ));
        }
        match self.proof {
            Proof::Password => {
                fields = fields.child(TextField::new(
                    "stepup-password",
                    t("factorSettings.stepUp.password"),
                    &self.field,
                ));
            }
            Proof::Code => {
                fields = fields.child(TextField::new(
                    "stepup-password",
                    t("factorSettings.stepUp.code"),
                    &self.field,
                ));
            }
            Proof::Passkey => {}
        }
        let this = cx.entity().downgrade();
        let close = this.clone();
        ConfirmDialog::new(
            "stepup",
            t("factorSettings.stepUp.title"),
            t(body),
            t(confirm),
            t("factorSettings.stepUp.working"),
            t("factorSettings.cancel"),
        )
        .tone(Tone::Accent)
        .busy(self.busy)
        .error(self.error.clone().map(Into::into))
        .fields(fields, None)
        .on_confirm(move |window, cx| {
            let _ = this.update(cx, |this, cx| this.submit(window, cx));
        })
        .on_close(move |_, cx| {
            let _ = close.update(cx, |this, cx| this.close(cx));
        })
    }
}

impl Drop for StepUpDialog {
    fn drop(&mut self) {
        if let Some(c) = self.ceremony.take() {
            c.cancel();
        }
    }
}
