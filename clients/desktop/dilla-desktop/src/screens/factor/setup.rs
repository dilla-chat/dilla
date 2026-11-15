//! Choosing and setting up a second factor (web `factor/FactorSetup.tsx`): one view for the places it is shown — a sign-in
//! on a new device, the forced screen of an account that owes one, the last sign-up step (Task 25 embeds it), and
//! Settings → Sign-in (`Origin::Settings`, in a dialog, starting at the app or the passkey and spending its host's
//! step-up token). There is no skip and no cancel outside Settings; Back only returns to the choice.
//!
//! The TOTP secret, the otpauth URI and the backup codes live in this view only (zeroizing buffers) and are gone when it
//! drops: never a slice, prefs, a log or the store. The passkey ceremony runs in the system browser
//! ([`BrowserAuth`]): the desktop app has no WebAuthn of its own.

use std::rc::Rc;

use dilla_client::Client;
use dilla_client::features::browser_auth::{BrowserAuth, BrowserAuthError, Cancel, CeremonyKind};
use dilla_client::features::factors::{
    BackupCodes, FactorError, Factors, InstanceDoc, StepUpToken, TotpKey, is_six_digits,
    passkey_name_fits,
};
use gpui::prelude::FluentBuilder;
use gpui::{
    AnyElement, AppContext as _, ClickEvent, ClipboardItem, Context, Entity, EventEmitter,
    InteractiveElement, IntoElement, ParentElement, Render, SharedString,
    StatefulInteractiveElement, Styled, Subscription, Window, div, px,
};
use gpui_component::input::{InputEvent, InputState};
use zeroize::Zeroizing;

use dilla_desktop_ui::components::banner::BannerTone;
use dilla_desktop_ui::components::button::{Button, ButtonVariant};
use dilla_desktop_ui::components::choice_cards::{ChoiceCardOption, ChoiceCards};
use dilla_desktop_ui::components::dialog::Dialog;
use dilla_desktop_ui::components::factor::{CodeList, code_list_columns, copy_text};
use dilla_desktop_ui::components::qr_code::QrCode;
use dilla_desktop_ui::components::text_field::TextField;
use dilla_desktop_ui::text::{line, line_height};
use dilla_desktop_ui::tokens::{color, text};
use dilla_desktop_ui::{Icon, IconName};

use super::code::{CodeEvent, CodeField};
use super::frame::{Action, AuthFrame, error_line, link, nav_row, para};
use crate::strings::{t, tf};
use crate::tracked_box;

/// The QR code's side and the gap to the key (FactorSetup.css `.dw-factor-key`).
const QR_SIDE: f32 = 132.0;
const KEY_GAP: f32 = 16.0;
/// The card's content width (400 − 2 × 28), for the code list's columns.
const CARD_CONTENT: f32 = 344.0;
/// The acknowledgement's box (a native 14px check box in a 24px target) and its gap to the label.
const CHECK: f32 = 14.0;
const CHECK_TARGET: f32 = 24.0;
const CHECK_GAP: f32 = 8.0;

/// Which factor the choice offers.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Choice {
    App,
    Passkey,
}

/// Where the set-up is shown.
#[derive(Clone)]
pub enum Origin {
    /// A sign-in on a new device found the account owing a factor: its step line and dots.
    SignIn { step: String, progress: (u8, u8) },
    /// The forced screen: no step line.
    Required,
    /// The last sign-up step (Task 25): its codes go to the host's done step.
    Signup { step: String, progress: (u8, u8) },
    /// Settings → Sign-in's add dialog: the step-up token its host holds, and where it starts.
    Settings { stepup: StepUpToken, start: Choice },
}

/// What was added.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Added {
    Totp,
    Passkey,
}

/// Why a Settings dialog hands back its token.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Lost {
    /// The token was refused (spent, expired).
    Expired,
    /// The add ended without adding anything, and the begin spent the token.
    Again,
}

/// What the view tells its host.
pub enum SetupEvent {
    /// The factor is in place; the host finishes (the forced and sign-in hosts read the factors and go on). `codes` are
    /// handed over only to the sign-up origin, for its done step: the host takes them out of the cell (an event is
    /// read by reference), so they are held in one place only.
    Done {
        added: Added,
        name: Option<String>,
        codes: std::cell::Cell<Option<BackupCodes>>,
    },
    /// Settings: the host asks for a new step-up.
    StepUpLost(Lost),
    /// Settings: Cancel, Escape or the close button.
    Close,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum Step {
    Choose,
    Totp,
    Passkey,
    Codes,
    Done,
}

pub struct FactorSetupView {
    server: url::Url,
    factors: Factors,
    browser: BrowserAuth,
    origin: Origin,
    instance: InstanceDoc,
    has_password: bool,
    passkey_count: usize,
    step: Step,
    choice: Choice,
    busy: bool,
    problem: Option<String>,
    key: Option<TotpKey>,
    code: Entity<CodeField>,
    code_error: Option<&'static str>,
    name: Entity<InputState>,
    name_error: Option<&'static str>,
    codes: Zeroizing<Vec<String>>,
    ack: bool,
    added: Added,
    added_name: Option<String>,
    /// A newer screen (Back, a newer request) drops an older answer.
    generation: u64,
    completed: bool,
    token_spent: bool,
    /// The setup key as drawn, made once when the key arrives (GPUI keeps its own copy while it is shown).
    secret_text: Option<SharedString>,
    /// The passkey ceremony waiting on the system browser: Back, Close and the view going away end it.
    ceremony: Option<Cancel>,
    _subscriptions: Vec<Subscription>,
}

impl EventEmitter<SetupEvent> for FactorSetupView {}

/// FactorSetup `sharedRefusal`: too many attempts and a dropped connection, by status and code only.
pub fn shared_refusal(e: &FactorError) -> Option<String> {
    if e.status == 429 {
        let seconds = e.retry_after_ms.unwrap_or(1000).div_ceil(1000).max(1);
        return Some(tf(
            "factor.error.tooMany",
            &[("seconds", &seconds.to_string())],
        ));
    }
    if e.status == 0 && (e.code == "E_NETWORK" || e.code == "E_UNAVAILABLE") {
        return Some(t("factor.error.network").to_owned());
    }
    None
}

/// doneRefusal.ts `doneProblem`: nothing when the account still owes a factor (the choice says so), else the shared rows,
/// else the generic line with the code.
pub fn done_problem(e: &FactorError) -> Option<String> {
    if e.is(403, "E_FACTOR_REQUIRED") {
        return None;
    }
    Some(shared_refusal(e).unwrap_or_else(|| tf("factor.error.other", &[("code", &e.code)])))
}

/// FactorSetup `passkeyProblem` for the system browser's ending and the instance's refusals.
fn browser_problem(e: &BrowserAuthError, instance: &str) -> String {
    match e {
        BrowserAuthError::Unsupported => t("factor.error.desktopUnsupported").to_owned(),
        BrowserAuthError::RateLimited => t("factor.error.rateLimited").to_owned(),
        _ => tf("factor.error.other", &[("code", "E_PASSKEY")]),
    }
    .replace("{instance}", instance)
}

fn passkey_refusal(e: &FactorError, instance: &str) -> String {
    if let Some(shared) = shared_refusal(e) {
        return shared;
    }
    if (400..500).contains(&e.status) {
        return tf("factor.error.refused", &[("instance", instance)]);
    }
    tf("factor.error.other", &[("code", &e.code)])
}

/// Tests (feature `test-hooks`): the set-up driven as a person does, step by step.
#[cfg(feature = "test-hooks")]
impl FactorSetupView {
    /// The step's primary button (Continue, Verify, Done).
    pub fn press_primary(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        self.primary(window, cx);
    }

    /// The step on show, by name (`Choose`, `Totp`, `Passkey`, `Codes`, `Done`).
    pub fn step_name(&self) -> String {
        format!("{:?}", self.step)
    }

    /// The authenticator app's secret the step shows (a person's app reads it from the QR code).
    pub fn shown_secret(&self) -> Option<String> {
        self.secret_text.as_ref().map(ToString::to_string)
    }

    /// The code field's error, by its copy key (`factor.error.code`: the instance refused the code).
    pub fn code_problem(&self) -> Option<&'static str> {
        self.code_error
    }

    /// Whether the code boxes are drawn as refused (red).
    pub fn code_invalid(&self, cx: &gpui::App) -> bool {
        self.code.read(cx).is_invalid()
    }

    /// The app step's Back: the choice again.
    pub fn press_back(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        self.show_choice(window, cx);
    }

    /// Whether the code field has the keyboard.
    pub fn code_focused(&self, window: &Window, cx: &gpui::App) -> bool {
        gpui::Focusable::focus_handle(self.code.read(cx), cx).is_focused(window)
    }

    /// The codes step's "I have saved these codes".
    pub fn tick_saved(&mut self, cx: &mut Context<Self>) {
        self.ack = true;
        cx.notify();
    }
}

impl FactorSetupView {
    pub fn new(
        client: Rc<Client>,
        server: url::Url,
        origin: Origin,
        _username: String,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) -> Self {
        let factors = client.factors();
        let code = cx.new(|cx| CodeField::new("factor-totp-code", cx));
        let name = cx.new(|cx| InputState::new(window, cx));
        let subscriptions = vec![
            cx.subscribe_in(
                &code,
                window,
                |this, _, e: &CodeEvent, window, cx| match e {
                    CodeEvent::Changed => {
                        this.code_error = None;
                        this.code.update(cx, |c, cx| c.set_invalid(false, cx));
                        cx.notify();
                    }
                    CodeEvent::Complete | CodeEvent::Submit => this.submit_totp(window, cx),
                },
            ),
            cx.subscribe_in(
                &name,
                window,
                |this, _, e: &InputEvent, window, cx| match e {
                    InputEvent::PressEnter { .. } => this.submit_passkey(window, cx),
                    InputEvent::Change => {
                        this.name_error = None;
                        cx.notify();
                    }
                    _ => {}
                },
            ),
        ];
        let (step, choice) = match &origin {
            Origin::Settings { start, .. } => (
                match start {
                    Choice::App => Step::Totp,
                    Choice::Passkey => Step::Passkey,
                },
                *start,
            ),
            _ => (Step::Choose, Choice::App),
        };
        let snapshot = client.state().borrow().factors.factors.clone();
        let start_passkey = matches!(
            &origin,
            Origin::Settings {
                start: Choice::Passkey,
                ..
            }
        );
        let mut this = Self {
            server,
            factors,
            browser: BrowserAuth::default(),
            origin,
            instance: InstanceDoc::default(),
            has_password: snapshot.as_ref().is_none_or(|f| f.has_password),
            passkey_count: snapshot.as_ref().map_or(0, |f| f.passkeys.len()),
            step,
            choice,
            busy: false,
            problem: None,
            key: None,
            code,
            code_error: None,
            name,
            name_error: None,
            codes: Zeroizing::new(Vec::new()),
            ack: false,
            added: Added::Totp,
            added_name: None,
            generation: 0,
            completed: false,
            token_spent: false,
            secret_text: None,
            ceremony: None,
            _subscriptions: subscriptions,
        };
        this.load(window, cx);
        if this.step == Step::Totp {
            this.begin_totp(window, cx);
        }
        if start_passkey {
            this.open_passkey(window, cx);
        }
        this
    }

    /// The system browser the passkey ceremonies open (tests give their own).
    pub fn with_browser(mut self, browser: BrowserAuth) -> Self {
        self.browser = browser;
        self
    }

    fn embedded(&self) -> bool {
        matches!(self.origin, Origin::Settings { .. })
    }

    /// The instance's name and passkeys, and the factors (a reload after a factor was confirmed: the set-up ends as
    /// already in place).
    fn load(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let instance = self.factors.instance();
        let read = self.factors.refresh();
        cx.spawn_in(window, async move |this, cx| {
            let instance = instance.await;
            let read = read.await;
            let _ = this.update_in(cx, |this, window, cx| {
                if let Ok(doc) = instance {
                    this.instance = doc;
                }
                if let Ok(Some(f)) = read {
                    this.has_password = f.has_password;
                    this.passkey_count = f.passkeys.len();
                    // Only a read that says the account owes nothing is trusted; when in doubt, the choice.
                    if this.step == Step::Choose
                        && !this.embedded()
                        && !f.owed
                        && f.holds_factor()
                        && !this.completed
                    {
                        let added = if f.totp.is_some() {
                            Added::Totp
                        } else {
                            Added::Passkey
                        };
                        this.finish(added, window, cx);
                    }
                }
                cx.notify();
            });
        })
        .detach();
    }

    /// The host's refusal of the finish (doneRefusal.ts): shown in this view's banner; a still-owed account goes back to
    /// the choice.
    pub fn finish_refused(&mut self, e: &FactorError, window: &mut Window, cx: &mut Context<Self>) {
        self.completed = false;
        self.busy = false;
        match done_problem(e) {
            None => {
                self.codes = Zeroizing::new(Vec::new());
                self.ack = false;
                self.show_choice(window, cx);
            }
            Some(problem) => self.problem = Some(problem),
        }
        cx.notify();
    }

    fn go(&mut self, step: Step, cx: &mut Context<Self>) {
        self.generation += 1;
        self.problem = None;
        self.busy = false;
        self.step = step;
        cx.notify();
    }

    fn show_choice(&mut self, _window: &mut Window, cx: &mut Context<Self>) {
        self.cancel_ceremony();
        self.key = None;
        self.secret_text = None;
        self.code_error = None;
        self.name_error = None;
        self.code.update(cx, |c, cx| c.clear(cx));
        self.go(Step::Choose, cx);
    }

    fn stepup(&self) -> Option<StepUpToken> {
        match &self.origin {
            Origin::Settings { stepup, .. } => Some(stepup.clone()),
            _ => None,
        }
    }

    /// A refused token in Settings hands back to the host; in the set-up origins (which send none) a 403 `E_STEP_UP`
    /// means another device added a factor: the factors are read again and the choice shows.
    fn step_up_refused(
        &mut self,
        e: &FactorError,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) -> bool {
        if !e.is_step_up() {
            return false;
        }
        if self.embedded() {
            cx.emit(SetupEvent::StepUpLost(Lost::Expired));
        } else {
            self.show_choice(window, cx);
            self.load(window, cx);
        }
        true
    }

    fn begin_totp(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        self.go(Step::Totp, cx);
        self.busy = true;
        let g = self.generation;
        let reply = self.factors.totp_begin(self.stepup());
        cx.spawn_in(window, async move |this, cx| {
            let answer = reply.await;
            let _ = this.update_in(cx, |this, window, cx| {
                if this.generation != g {
                    return;
                }
                this.busy = false;
                match answer {
                    Ok(key) => {
                        this.secret_text = Some(SharedString::from(key.secret.as_str().to_owned()));
                        this.key = Some(key);
                        this.code.update(cx, |c, cx| c.focus(window, cx));
                    }
                    Err(e) => {
                        if this.step_up_refused(&e, window, cx) {
                            return;
                        }
                        if this.embedded() {
                            cx.emit(SetupEvent::StepUpLost(Lost::Again));
                            return;
                        }
                        this.problem = Some(
                            shared_refusal(&e)
                                .unwrap_or_else(|| tf("factor.error.other", &[("code", &e.code)])),
                        );
                    }
                }
                cx.notify();
            });
        })
        .detach();
    }

    fn submit_totp(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        if self.busy || self.key.is_none() || self.step != Step::Totp {
            return;
        }
        let code = Zeroizing::new(self.code.read(cx).value().to_owned());
        if code.is_empty() || !is_six_digits(&code) {
            self.code_error = Some(if code.is_empty() {
                "factor.error.required"
            } else {
                "factor.error.codeFormat"
            });
            self.code.update(cx, |c, cx| {
                c.set_invalid(true, cx);
                c.focus(window, cx);
            });
            cx.notify();
            return;
        }
        self.code_error = None;
        self.problem = None;
        self.busy = true;
        let g = self.generation;
        let reply = self.factors.totp_confirm(&code);
        cx.spawn_in(window, async move |this, cx| {
            let answer = reply.await;
            let _ = this.update_in(cx, |this, window, cx| {
                if this.generation != g {
                    return;
                }
                this.busy = false;
                match answer {
                    Ok(codes) => this.added(Added::Totp, codes, window, cx),
                    Err(e) => {
                        if this.step_up_refused(&e, window, cx) {
                            return;
                        }
                        if e.is(400, "E_INVALID_REQUEST") {
                            this.code_error = Some("factor.error.code");
                            this.code.update(cx, |c, cx| {
                                c.clear(cx);
                                c.set_invalid(true, cx);
                                c.focus(window, cx);
                            });
                        } else {
                            this.problem =
                                Some(shared_refusal(&e).unwrap_or_else(|| {
                                    tf("factor.error.other", &[("code", &e.code)])
                                }));
                        }
                    }
                }
                cx.notify();
            });
        })
        .detach();
    }

    /// The passkey step, its name field holding the default name (web `passkeyName`).
    fn open_passkey(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let default = tf(
            "factor.passkey.defaultName",
            &[("n", &(self.passkey_count + 1).to_string())],
        );
        self.name
            .update(cx, |s, cx| s.set_value(default, window, cx));
        self.go(Step::Passkey, cx);
    }

    fn passkey_name(&self, cx: &gpui::App) -> String {
        let typed = self.name.read(cx).value().to_string();
        if typed.is_empty() {
            tf(
                "factor.passkey.defaultName",
                &[("n", &(self.passkey_count + 1).to_string())],
            )
        } else {
            typed
        }
    }

    fn submit_passkey(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        if self.busy || self.step != Step::Passkey {
            return;
        }
        // Settings: an earlier begin spent the token, so this try needs a new one.
        if self.embedded() && self.token_spent {
            cx.emit(SetupEvent::StepUpLost(Lost::Again));
            return;
        }
        let name = self.passkey_name(cx).trim().to_owned();
        if name.is_empty() {
            self.name_error = Some("factor.error.required");
            cx.notify();
            return;
        }
        if !passkey_name_fits(&name) {
            self.name_error = Some("factor.error.nameLong");
            cx.notify();
            return;
        }
        self.name_error = None;
        self.problem = None;
        self.busy = true;
        self.added_name = Some(name.clone());
        self.token_spent = self.embedded();
        let g = self.generation;
        let factors = self.factors.clone();
        let browser = self.browser.clone();
        let server = self.server.clone();
        let cancel = Cancel::new();
        self.ceremony = Some(cancel.clone());
        let begin = factors.passkey_begin(self.stepup());
        let instance = self.instance.name.clone();
        cx.spawn_in(window, async move |this, cx| {
            enum Ended {
                Instance(FactorError),
                Browser(BrowserAuthError),
            }
            let run = async {
                let finish_guard = cancel.clone();
                let ceremony = begin.await.map_err(Ended::Instance)?;
                let (tx, rx) = tokio::sync::oneshot::channel();
                let options = ceremony.options.clone();
                // The ceremony waits on the browser for up to two minutes, on a thread of its own.
                std::thread::spawn(move || {
                    let result = tokio::runtime::Builder::new_current_thread()
                        .enable_all()
                        .build()
                        .map_err(|e| BrowserAuthError::Network(e.to_string()))
                        .and_then(|rt| {
                            rt.block_on(browser.run_cancellable(
                                &server,
                                CeremonyKind::Create,
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
                    .passkey_finish(ceremony.id, response, &name)
                    .await
                    .map_err(Ended::Instance)
            };
            let answer = run.await;
            let _ = this.update_in(cx, |this, window, cx| {
                if this.generation != g {
                    return;
                }
                this.busy = false;
                match answer {
                    Ok(codes) => this.added(Added::Passkey, codes, window, cx),
                    Err(Ended::Instance(e)) => {
                        if this.step_up_refused(&e, window, cx) {
                            return;
                        }
                        if this.embedded() {
                            cx.emit(SetupEvent::StepUpLost(Lost::Again));
                            return;
                        }
                        this.problem = Some(passkey_refusal(&e, &instance));
                    }
                    Err(Ended::Browser(e)) => this.problem = Some(browser_problem(&e, &instance)),
                }
                cx.notify();
            });
        })
        .detach();
    }

    /// The factor is in place: the codes, or (none issued) the end.
    fn added(
        &mut self,
        added: Added,
        codes: BackupCodes,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        self.added = added;
        self.generation += 1;
        self.code.update(cx, |c, cx| c.clear(cx));
        if matches!(self.origin, Origin::Signup { .. }) {
            self.busy = true;
            self.complete(Some(codes), cx);
            return;
        }
        self.key = None;
        self.codes = codes.0;
        if self.codes.is_empty() {
            self.finish(added, window, cx);
        } else {
            self.ack = false;
            self.step = Step::Codes;
        }
        cx.notify();
    }

    fn finish(&mut self, added: Added, _window: &mut Window, cx: &mut Context<Self>) {
        self.added = added;
        if matches!(self.origin, Origin::Required) {
            self.codes = Zeroizing::new(Vec::new());
            self.step = Step::Done;
            cx.notify();
            return;
        }
        self.complete(None, cx);
    }

    /// Hands the result to the host once.
    fn complete(&mut self, codes: Option<BackupCodes>, cx: &mut Context<Self>) {
        if self.completed {
            return;
        }
        self.completed = true;
        let name = (self.added == Added::Passkey)
            .then(|| self.added_name.clone())
            .flatten();
        cx.emit(SetupEvent::Done {
            added: self.added,
            name,
            codes: std::cell::Cell::new(codes),
        });
    }

    fn primary(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        match self.step {
            Step::Choose => {
                let choice = if self.has_password {
                    self.choice
                } else {
                    Choice::Passkey
                };
                match choice {
                    Choice::App => self.begin_totp(window, cx),
                    Choice::Passkey if self.instance.passkeys => self.open_passkey(window, cx),
                    Choice::Passkey => {}
                }
            }
            Step::Totp => self.submit_totp(window, cx),
            Step::Passkey => self.submit_passkey(window, cx),
            Step::Codes => {
                if self.ack {
                    let added = self.added;
                    self.finish(added, window, cx);
                }
            }
            Step::Done => self.complete(None, cx),
        }
    }

    fn cancel_ceremony(&mut self) {
        if let Some(c) = self.ceremony.take() {
            c.cancel();
        }
    }

    fn copy(&self, text_: String, cx: &mut Context<Self>) {
        cx.write_to_clipboard(ClipboardItem::new_string(text_));
    }

    fn title(&self) -> &'static str {
        match self.step {
            Step::Choose => t("factor.choose.title"),
            Step::Totp => t(if matches!(self.origin, Origin::Signup { .. }) {
                "factor.totp.titleSignup"
            } else {
                "factor.totp.title"
            }),
            Step::Passkey => t("factor.passkey.title"),
            Step::Codes => t("factor.codes.title"),
            Step::Done => t("factor.done.title"),
        }
    }

    fn body(
        &mut self,
        cx: &mut Context<Self>,
    ) -> (Vec<AnyElement>, Option<Action>, Vec<AnyElement>) {
        let instance = self.instance.name.clone();
        let mut body = Vec::new();
        let mut below = Vec::new();
        let back = |cx: &mut Context<Self>| {
            link(
                "factor-back",
                t("factor.back"),
                true,
                cx.listener(|this, _: &ClickEvent, window, cx| this.show_choice(window, cx)),
            )
        };
        let primary = |label: &str, ready: bool, cx: &mut Context<Self>| {
            Action::new(
                "factor-submit",
                label.to_owned(),
                cx.listener(|this, _: &ClickEvent, window, cx| this.primary(window, cx)),
            )
            .ready(ready)
        };
        let action = match self.step {
            Step::Choose => {
                body.push(para(match self.origin {
                    Origin::Required => tf("factor.choose.required", &[("instance", &instance)]),
                    Origin::Signup { .. } => t("factor.choose.bodySignup").to_owned(),
                    _ => tf("factor.choose.body", &[("instance", &instance)]),
                }));
                if !self.has_password {
                    body.push(para(t("factor.choose.noPassword")));
                }
                let mut options = Vec::new();
                if self.has_password {
                    options.push(
                        ChoiceCardOption::new(
                            "app",
                            t("factor.choose.app"),
                            t("factor.choose.appHint"),
                        )
                        .icon(IconName::Smartphone),
                    );
                }
                let mut passkey = ChoiceCardOption::new(
                    "passkey",
                    t("factor.choose.passkey"),
                    t("factor.choose.passkeyHint"),
                )
                .icon(IconName::Fingerprint);
                if !self.instance.passkeys {
                    passkey = passkey.disabled(t("factor.choose.passkeyOff"));
                }
                options.push(passkey);
                let effective = if self.has_password {
                    self.choice
                } else {
                    Choice::Passkey
                };
                let value = match effective {
                    Choice::App => "app",
                    Choice::Passkey => "passkey",
                };
                let this = cx.entity().downgrade();
                body.push(
                    tracked_box(
                        "factor-kind",
                        ChoiceCards::new("factor-kind", options, value).on_change(
                            move |id, _, cx| {
                                let choice = if id == "passkey" {
                                    Choice::Passkey
                                } else {
                                    Choice::App
                                };
                                let _ = this.update(cx, |this, cx| {
                                    this.choice = choice;
                                    cx.notify();
                                });
                            },
                        ),
                    )
                    .into_any_element(),
                );
                let cannot = effective == Choice::Passkey && !self.instance.passkeys;
                primary(t("factor.choose.next"), !cannot, cx)
            }
            Step::Totp => {
                match &self.key {
                    None => {
                        if self.busy {
                            body.push(para(t("factor.totp.loading")));
                        }
                    }
                    Some(key) => {
                        body.push(para(t("factor.totp.body")));
                        let secret = self.secret_text.clone().unwrap_or_default();
                        let copy_secret = Zeroizing::new(key.secret.as_str().to_owned());
                        body.push(
                            div()
                                .flex()
                                .gap(px(KEY_GAP))
                                .items_start()
                                .child(
                                    QrCode::new("factor-totp-qr", key.uri.as_str())
                                        .size(px(QR_SIDE)),
                                )
                                .child(
                                    div()
                                        .flex_1()
                                        .min_w_0()
                                        .flex()
                                        .flex_col()
                                        .gap(px(KEY_GAP / 2.0))
                                        .child(line(
                                            t("factor.totp.key"),
                                            text::XS,
                                            line_height(text::XS, 1.5),
                                            color::FG_2,
                                        ))
                                        // The setup key is never in the accessibility tree; "Read aloud" (a Tab
                                        // stop, drawn while focused) reads it out once on request (Jonas 2026-11-15).
                                        .child(
                                            div()
                                                .relative()
                                                .text_size(text::SM)
                                                .font_family(dilla_desktop_ui::tokens::font::MONO)
                                                .text_color(color::FG)
                                                .child(secret)
                                                .child({
                                                    let key = Zeroizing::new(
                                                        key.secret.as_str().to_owned(),
                                                    );
                                                    crate::screens::read_aloud::button(
                                                        "factor-totp-read-key",
                                                        "readAloud.setupKey",
                                                        move || {
                                                            crate::screens::read_aloud::spoken(
                                                                t("factor.totp.key"),
                                                                key.as_str(),
                                                            )
                                                        },
                                                    )
                                                }),
                                        )
                                        .child(tracked_box(
                                            "factor-totp-copy",
                                            Button::new(
                                                "factor-totp-copy",
                                                t("factor.totp.copyKey"),
                                            )
                                            .on_click(
                                                cx.listener(move |this, _: &ClickEvent, _, cx| {
                                                    this.copy(copy_secret.as_str().to_owned(), cx);
                                                }),
                                            ),
                                        )),
                                )
                                .into_any_element(),
                        );
                        if matches!(self.origin, Origin::Signup { .. }) {
                            body.push(para(t("factor.totp.codeHint")));
                        }
                        let focus = gpui::Focusable::focus_handle(self.code.read(cx), cx);
                        body.push(
                            crate::tracked_focus("factor-totp-code", &focus, self.code.clone())
                                .into_any_element(),
                        );
                        if !matches!(self.origin, Origin::Signup { .. }) {
                            body.push(para(t("factor.totp.codeHint")));
                        }
                        if let Some(e) = self.code_error {
                            body.push(
                                tracked_box("factor-code-error", error_line(t(e)))
                                    .into_any_element(),
                            );
                        }
                    }
                }
                if !self.embedded() {
                    below.push(nav_row(back(cx), None));
                }
                let ready = !self.busy && self.key.is_some() && self.code.read(cx).is_complete();
                let label = if self.busy && self.key.is_some() {
                    t("factor.totp.working")
                } else if matches!(self.origin, Origin::Signup { .. }) {
                    t("factor.totp.verify")
                } else {
                    t("factor.totp.submit")
                };
                primary(label, ready, cx)
            }
            Step::Passkey => {
                body.push(para(t("factor.passkey.body")));
                let mut field =
                    TextField::new("factor-passkey-name", t("factor.passkey.name"), &self.name)
                        .hint(t("factor.passkey.nameHint"));
                if let Some(e) = self.name_error {
                    field = field.error(t(e));
                }
                body.push(tracked_box("factor-passkey-name", field).into_any_element());
                if !self.embedded() {
                    below.push(nav_row(back(cx), None));
                }
                let label = if self.busy {
                    t("factor.passkey.working")
                } else {
                    t("factor.passkey.submit")
                };
                primary(label, !self.busy, cx)
            }
            Step::Codes => {
                body.push(para(t("factor.codes.body")));
                body.push(para(t("factor.codes.notKey")));
                let all = copy_text(&self.codes);
                body.push(
                    CodeList::new("factor-codes", t("factor.codes.label"), &self.codes)
                        .columns(code_list_columns(CARD_CONTENT))
                        .action(tracked_box(
                            "factor-codes-copy",
                            Button::new("factor-codes-copy", t("factor.codes.copy")).on_click(
                                cx.listener(move |this, _: &ClickEvent, _, cx| {
                                    this.copy(all.clone(), cx)
                                }),
                            ),
                        ))
                        .into_any_element(),
                );
                let ack = self.ack;
                body.push(
                    tracked_box(
                        "factor-ack",
                        check_row("factor-ack", t("factor.codes.ack"), ack).on_click(cx.listener(
                            |this, _: &ClickEvent, _, cx| {
                                this.ack = !this.ack;
                                cx.notify();
                            },
                        )),
                    )
                    .into_any_element(),
                );
                if !ack {
                    body.push(para(t("factor.codes.ackHint")));
                }
                primary(t("factor.codes.next"), ack, cx)
            }
            Step::Done => {
                body.push(para(t(match self.added {
                    Added::Totp => "factor.done.totp",
                    Added::Passkey => "factor.done.passkey",
                })));
                primary(t("factor.done.next"), true, cx)
            }
        };
        (body, Some(action), below)
    }
}

impl Render for FactorSetupView {
    fn render(&mut self, _window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let title = self.title();
        let (body, action, below) = self.body(cx);
        let banner = self.problem.clone().map(|p| (BannerTone::Danger, p));
        if self.embedded() {
            let close_label = if self.step == Step::Codes {
                t("dialog.close")
            } else {
                t("factorSettings.cancel")
            };
            let footer = action.map(|a| a.into_button(ButtonVariant::Accent));
            let this = cx.entity().downgrade();
            let mut dialog = Dialog::new("factor-dialog", title, close_label)
                .body(
                    div()
                        .flex()
                        .flex_col()
                        .gap(px(KEY_GAP))
                        .children(banner.map(|(tone, p)| {
                            dilla_desktop_ui::components::banner::Banner::new(
                                "factor-banner",
                                tone,
                                p,
                            )
                        }))
                        .children(body),
                )
                .on_close(move |_, cx| {
                    let _ = this.update(cx, |this, cx| {
                        this.cancel_ceremony();
                        cx.emit(SetupEvent::Close)
                    });
                });
            if let Some(footer) = footer {
                dialog = dialog.footer(footer);
            }
            return dialog.into_any_element();
        }
        let (step, progress) = match &self.origin {
            Origin::SignIn { step, progress } | Origin::Signup { step, progress } => {
                (step.clone(), Some(*progress))
            }
            _ => (String::new(), None),
        };
        let mut frame = AuthFrame::new(title).step(step).banner(banner);
        if let Some((n, total)) = progress {
            frame = frame.progress(n, total);
        }
        for b in body {
            frame = frame.child(b);
        }
        if let Some(action) = action {
            frame = frame.action(action);
        }
        for b in below {
            frame = frame.below(b);
        }
        div()
            .id("factor-setup")
            .size_full()
            .child(frame.render())
            .into_any_element()
    }
}

/// The codes step's acknowledgement: a check box and its label, one target.
fn check_row(id: &'static str, label: &'static str, checked: bool) -> gpui::Stateful<gpui::Div> {
    let edge = if checked {
        color::ACCENT
    } else {
        color::CONTROL_EDGE_KEY
    };
    div()
        .id(id)
        .flex()
        .items_center()
        .gap(px(CHECK_GAP))
        .cursor_pointer()
        .child(
            div()
                .size(px(CHECK_TARGET))
                .flex()
                .items_center()
                .justify_center()
                .child(
                    div()
                        .size(px(CHECK))
                        .rounded(dilla_desktop_ui::tokens::radius::TAG)
                        .border_1()
                        .border_color(edge)
                        .when(checked, |d| d.bg(color::ACCENT))
                        .flex()
                        .items_center()
                        .justify_center()
                        .when(checked, |d| {
                            d.child(
                                Icon::new(IconName::Check)
                                    .size(px(CHECK - 4.0))
                                    .color(color::ACCENT_INK),
                            )
                        }),
                ),
        )
        .child(line(label, text::SM, line_height(text::SM, 1.5), color::FG))
}

impl Drop for FactorSetupView {
    /// A ceremony left waiting on the system browser ends with the view.
    fn drop(&mut self) {
        self.cancel_ceremony();
    }
}
