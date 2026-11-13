//! Signing in on this device (web `screens/SignIn.tsx`, the Signal auth card): the login first (the server's address,
//! the username and the password; or the instance's identity provider, or a passkey), then the second factor it owes
//! (the six code boxes, a passkey, or a backup code), then the recovery key that adds this device, then done. The
//! protocol is `dilla_client::signin` (enrol.ts, web3.8 order R6): the login and the factor never register this device,
//! and a backup code waits here, in a zeroizing buffer, until the key goes with it.
//!
//! Against the web screen: the desktop asks for the server's address on the first step (a browser is already on its
//! instance) unless the window is pinned to one instance (Task 27), and a passkey or the identity provider runs in the
//! system browser (`features::browser_auth`), as the Electron app does. Each step runs on a thread of its own with a
//! small runtime, so the window never waits on the network.

use std::collections::HashMap;
use std::path::PathBuf;
use std::sync::Arc;
use std::time::Duration;

use dilla_client::features::browser_auth::{BrowserAuth, BrowserAuthError, Cancel, CeremonyKind};
use dilla_client::features::factors::{InstanceDoc, is_backup_code, normalise_backup_code};
use dilla_client::instances::Instances;
use dilla_client::seams::{InstanceConfig, Keychain};
use dilla_client::signin::{Owed, SignIn, SignInError, normalise_recovery_key};
use gpui::{
    AnyElement, AppContext as _, ClickEvent, Context, Entity, EventEmitter, InteractiveElement,
    IntoElement, ParentElement, Render, Styled, Subscription, Window, div,
};
use gpui_component::input::{InputEvent, InputState};
use zeroize::Zeroizing;

use dilla_desktop_ui::components::banner::BannerTone;
use dilla_desktop_ui::components::text_field::TextField;

use crate::screens::factor::code::{CodeEvent, CodeField};
use crate::screens::factor::frame::{
    Action, AuthFrame, error_line, link, links_row, nav_row, note, para,
};
use crate::screens::{FocusNext, FocusPrevious};
use crate::strings::{t, tf};
use crate::tracked_box;

/// The device is signed in: the instance to start the client for.
pub struct SignedIn(pub InstanceConfig);

/// The person chose to make an account instead (web `signInCancel`, which lands on the new-account screen; Task 25).
pub struct CreateInstead;

/// A recovery key's length.
const KEY_LENGTH: usize = 52;
/// How long the key field rests before a backup code is named (R44).
const BACKUP_NOTE: Duration = Duration::from_millis(600);

#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash)]
enum Field {
    Server,
    Username,
    Password,
    Code,
    Backup,
    Key,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum Shown {
    Login,
    Totp,
    Passkey,
    Backup,
    Key,
    NoBackup,
    Done,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum Command {
    Login,
    Oidc,
    Totp,
    Passkey,
    Key,
    Back,
}

pub struct SignInView {
    root: Option<PathBuf>,
    keychain: Arc<dyn Keychain>,
    /// The instance this window is pinned to (Task 27): no server field, and no other instance.
    pin: Option<InstanceConfig>,
    browser: BrowserAuth,
    shown: Shown,
    busy: Option<Command>,
    banner: Option<(BannerTone, String)>,
    errors: HashMap<Field, String>,
    no_backup: Option<&'static str>,
    server: Entity<InputState>,
    username: Entity<InputState>,
    password: Entity<InputState>,
    code: Entity<CodeField>,
    backup: Entity<InputState>,
    key: Entity<InputState>,
    /// The backup code the factor step took, waiting for the key.
    backup_held: Option<Zeroizing<String>>,
    /// R7: a factor step was shown in this ceremony, so the key is step 3.
    factor_shown: bool,
    /// The key field rested long enough to name a backup code.
    note_settled: bool,
    key_edits: u64,
    flow: Option<SignIn>,
    instance: Option<InstanceDoc>,
    signed_in: Option<InstanceConfig>,
    /// The system-browser ceremony under way: Cancel, Back, Escape and the view going away end it.
    ceremony: Option<Cancel>,
    _subscriptions: Vec<Subscription>,
}

impl EventEmitter<SignedIn> for SignInView {}
impl EventEmitter<CreateInstead> for SignInView {}

/// What a refusal shows (SignIn.tsx `signInRefusal`, first match wins), by the error's kind only.
struct Refusal {
    field: Option<(Field, String)>,
    banner: Option<(BannerTone, String)>,
    /// Back at the login: the password is cleared and takes focus.
    back_to_login: bool,
    no_backup: Option<&'static str>,
}

fn refusal(e: &SignInError, command: Command, instance: &str) -> Refusal {
    let banner = |tone, text: String| Refusal {
        field: None,
        banner: Some((tone, text)),
        back_to_login: false,
        no_backup: None,
    };
    let inst = [("instance", instance)];
    match e {
        SignInError::NoBackup => Refusal {
            no_backup: Some("signin.error.noBackup"),
            ..banner(BannerTone::Danger, String::new())
        },
        SignInError::WrongKey => Refusal {
            field: Some((Field::Key, t("signin.error.wrongKey").to_owned())),
            banner: None,
            back_to_login: false,
            no_backup: None,
        },
        SignInError::KeyLength => Refusal {
            field: Some((Field::Key, t("signin.error.keyLength").to_owned())),
            banner: None,
            back_to_login: false,
            no_backup: None,
        },
        SignInError::Expired => Refusal {
            back_to_login: true,
            ..banner(BannerTone::Warn, t("enrol.expired").to_owned())
        },
        SignInError::PasskeyOtherAccount => banner(
            BannerTone::Danger,
            t("factorSignin.passkey.otherAccount").to_owned(),
        ),
        SignInError::PasskeyUnknown => banner(
            BannerTone::Danger,
            tf("factorSignin.passkey.unknown", &inst),
        ),
        SignInError::BackupFailed | SignInError::NoAssertion if command == Command::Key => {
            Refusal {
                back_to_login: true,
                ..banner(BannerTone::Warn, t("factorSignin.backup.failed").to_owned())
            }
        }
        SignInError::CodeFailed | SignInError::NoAssertion if command == Command::Totp => Refusal {
            back_to_login: true,
            ..banner(BannerTone::Warn, t("signin.error.totpFailed").to_owned())
        },
        SignInError::LoginFailed if command == Command::Login => Refusal {
            field: Some((Field::Password, t("signin.error.loginFailed").to_owned())),
            banner: None,
            back_to_login: true,
            no_backup: None,
        },
        SignInError::DeviceCap => banner(BannerTone::Danger, tf("signin.error.deviceCap", &inst)),
        SignInError::Evicted => banner(BannerTone::Danger, t("signin.error.evicted").to_owned()),
        SignInError::ListRace => banner(BannerTone::Danger, t("signin.error.listRace").to_owned()),
        SignInError::TooMany(Some(ms)) => banner(
            BannerTone::Warn,
            tf(
                "signin.error.tooMany",
                &[("seconds", &ms.div_ceil(1000).to_string())],
            ),
        ),
        SignInError::TooMany(None) => banner(
            BannerTone::Warn,
            t("onboarding.error.rateLimitedNoWait").to_owned(),
        ),
        SignInError::Network(_) => banner(BannerTone::Danger, t("signin.error.network").to_owned()),
        SignInError::Browser(b) => banner(
            BannerTone::Danger,
            match (b, command) {
                (BrowserAuthError::Unsupported, Command::Oidc) => {
                    t("factorSignin.oidc.unsupported").to_owned()
                }
                (BrowserAuthError::Unsupported, _) => {
                    t("factorSignin.passkey.unsupported").to_owned()
                }
                (BrowserAuthError::RateLimited, _) => {
                    t("factorSignin.passkey.rateLimited").to_owned()
                }
                _ => t("factorSignin.passkey.failed").to_owned(),
            },
        ),
        _ if command == Command::Passkey => banner(
            BannerTone::Danger,
            t("factorSignin.passkey.failed").to_owned(),
        ),
        other => banner(
            BannerTone::Danger,
            tf("signin.error.other", &[("code", &other.code())]),
        ),
    }
}

impl SignInView {
    /// `pin`: the window's instance (Task 27): the server field is hidden and every step goes to that instance.
    pub fn new(
        root: Option<PathBuf>,
        keychain: Arc<dyn Keychain>,
        pin: Option<InstanceConfig>,
        error: Option<String>,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) -> Self {
        let field =
            |placeholder: &str, masked: bool, window: &mut Window, cx: &mut Context<Self>| {
                let placeholder = placeholder.to_owned();
                cx.new(|cx| {
                    InputState::new(window, cx)
                        .placeholder(placeholder)
                        .masked(masked)
                })
            };
        let server = field("https://", false, window, cx);
        let username = field("", false, window, cx);
        let password = field("", true, window, cx);
        let backup = field("", false, window, cx);
        let key = field(t("signin.key.placeholder"), false, window, cx);
        let code = cx.new(|cx| CodeField::new("signin-code-field", cx));
        let mut subscriptions = Vec::new();
        for (input, field) in [
            (&server, Field::Server),
            (&username, Field::Username),
            (&password, Field::Password),
            (&backup, Field::Backup),
            (&key, Field::Key),
        ] {
            subscriptions.push(cx.subscribe_in(
                input,
                window,
                move |this: &mut Self, _, event: &InputEvent, window, cx| match event {
                    InputEvent::PressEnter { .. } => this.submit(window, cx),
                    InputEvent::Change => {
                        if this.errors.remove(&field).is_some() {
                            cx.notify();
                        }
                        if field == Field::Key {
                            this.key_changed(cx);
                        }
                    }
                    _ => {}
                },
            ));
        }
        subscriptions.push(
            cx.subscribe_in(
                &code,
                window,
                |this, _, e: &CodeEvent, window, cx| match e {
                    CodeEvent::Changed => {
                        if this.errors.remove(&Field::Code).is_some() {
                            this.code.update(cx, |c, cx| c.set_invalid(false, cx));
                            cx.notify();
                        }
                    }
                    CodeEvent::Complete => {
                        if this.busy.is_none() && this.shown == Shown::Totp {
                            this.banner = None;
                            this.submit_totp(window, cx);
                        }
                    }
                    CodeEvent::Submit => this.submit(window, cx),
                },
            ),
        );
        let mut this = Self {
            root,
            keychain,
            pin: pin.clone(),
            browser: BrowserAuth::default(),
            shown: Shown::Login,
            busy: None,
            banner: error.map(|e| (BannerTone::Danger, e)),
            errors: HashMap::new(),
            no_backup: None,
            server,
            username,
            password,
            code,
            backup,
            key,
            backup_held: None,
            factor_shown: false,
            note_settled: false,
            key_edits: 0,
            flow: None,
            instance: None,
            signed_in: None,
            ceremony: None,
            _subscriptions: subscriptions,
        };
        match pin {
            Some(cfg) => {
                match SignIn::new(cfg, this.keychain.clone()) {
                    Ok(flow) => this.read_instance(flow, window, cx),
                    Err(e) => {
                        this.banner = Some((
                            BannerTone::Danger,
                            tf("signin.error.other", &[("code", &e.code())]),
                        ))
                    }
                }
                this.username.update(cx, |s, cx| s.focus(window, cx));
            }
            None => this.server.update(cx, |s, cx| s.focus(window, cx)),
        }
        this
    }

    /// The system browser a passkey or the identity provider opens (tests give their own).
    pub fn with_browser(mut self, browser: BrowserAuth) -> Self {
        self.browser = browser;
        self
    }

    fn instance_name(&self) -> String {
        if let Some(doc) = &self.instance {
            return doc.name.clone();
        }
        self.pin
            .as_ref()
            .and_then(|c| c.server.host_str().map(str::to_owned))
            .unwrap_or_default()
    }

    /// The pinned instance's name and sign-in methods, before the first step.
    fn read_instance(&mut self, flow: SignIn, window: &mut Window, cx: &mut Context<Self>) {
        self.run(
            flow,
            Command::Back,
            |f, rt| rt.block_on(f.instance_doc()),
            |this, result, _, cx| {
                if let Ok(doc) = result {
                    this.instance = Some(doc);
                }
                this.busy = None;
                cx.notify();
            },
            window,
            cx,
        );
    }

    /// Runs one step of the flow off the UI thread and hands the flow back with its result.
    fn run<T: Send + 'static>(
        &mut self,
        flow: SignIn,
        command: Command,
        step: impl FnOnce(&mut SignIn, &tokio::runtime::Runtime) -> Result<T, SignInError>
        + Send
        + 'static,
        then: impl FnOnce(&mut Self, Result<T, SignInError>, &mut Window, &mut Context<Self>) + 'static,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        self.busy = Some(command);
        cx.notify();
        let (tx, rx) = tokio::sync::oneshot::channel();
        let spawned = std::thread::Builder::new()
            .name("dilla-sign-in".into())
            .spawn(move || {
                let mut flow = flow;
                let result = tokio::runtime::Builder::new_current_thread()
                    .enable_all()
                    .build()
                    .map_err(|e| SignInError::Other(e.to_string()))
                    .and_then(|rt| step(&mut flow, &rt));
                let _ = tx.send((flow, result));
            });
        if spawned.is_err() {
            self.busy = None;
            self.banner = Some((
                BannerTone::Danger,
                tf("signin.error.other", &[("code", "E_THREAD")]),
            ));
            return;
        }
        cx.spawn_in(window, async move |this, cx| {
            if let Ok((flow, result)) = rx.await {
                let _ = this.update_in(cx, |this, window, cx| {
                    this.busy = None;
                    this.flow = Some(flow);
                    then(this, result, window, cx);
                    cx.notify();
                });
            }
        })
        .detach();
    }

    fn apply(
        &mut self,
        e: &SignInError,
        command: Command,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let view = refusal(e, command, &self.instance_name());
        if let Some(key) = view.no_backup {
            self.no_backup = Some(key);
            self.shown = Shown::NoBackup;
        }
        if let Some((field, text)) = view.field {
            if field == Field::Key {
                self.shown = Shown::Key;
            }
            self.errors.insert(field, text);
            self.focus(field, window, cx);
        }
        if view.back_to_login {
            self.back_to_login(window, cx);
        }
        self.banner = view.banner.filter(|(_, b)| !b.is_empty());
        cx.notify();
    }

    fn focus(&mut self, field: Field, window: &mut Window, cx: &mut Context<Self>) {
        match field {
            Field::Server => self.server.update(cx, |s, cx| s.focus(window, cx)),
            Field::Username => self.username.update(cx, |s, cx| s.focus(window, cx)),
            Field::Password => self.password.update(cx, |s, cx| s.focus(window, cx)),
            Field::Backup => self.backup.update(cx, |s, cx| s.focus(window, cx)),
            Field::Key => self.key.update(cx, |s, cx| s.focus(window, cx)),
            Field::Code => self.code.update(cx, |c, cx| c.focus(window, cx)),
        }
    }

    /// Back at the login: the factor, the key and any held backup code are dropped; the username is kept.
    fn back_to_login(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        if let Some(flow) = self.flow.as_mut() {
            flow.back();
        }
        self.shown = Shown::Login;
        self.factor_shown = false;
        self.backup_held = None;
        for input in [&self.password, &self.key, &self.backup] {
            input.update(cx, |s, cx| s.set_value("", window, cx));
        }
        self.code.update(cx, |c, cx| c.clear(cx));
        self.focus(Field::Password, window, cx);
    }

    fn go(&mut self, shown: Shown, window: &mut Window, cx: &mut Context<Self>) {
        self.shown = shown;
        self.errors.clear();
        if matches!(shown, Shown::Totp | Shown::Passkey | Shown::Backup) {
            self.factor_shown = true;
        }
        match shown {
            Shown::Totp => self.focus(Field::Code, window, cx),
            Shown::Backup => self.focus(Field::Backup, window, cx),
            Shown::Key => self.focus(Field::Key, window, cx),
            _ => {}
        }
        cx.notify();
    }

    fn after_owed(&mut self, owes: Owed, window: &mut Window, cx: &mut Context<Self>) {
        match owes {
            Owed::Code => self.go(Shown::Totp, window, cx),
            Owed::Passkey => {
                let usable = self.instance.as_ref().is_some_and(|d| d.passkeys);
                self.go(
                    if usable {
                        Shown::Passkey
                    } else {
                        Shown::Backup
                    },
                    window,
                    cx,
                );
            }
            Owed::Nothing => self.go(Shown::Key, window, cx),
        }
    }

    fn submit(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        if self.busy.is_some() {
            return;
        }
        self.banner = None;
        match self.shown {
            Shown::Login => self.login(window, cx),
            Shown::Totp => self.submit_totp(window, cx),
            Shown::Passkey => self.passkey(window, cx),
            Shown::Backup => self.hold_backup(window, cx),
            Shown::Key => self.submit_key(window, cx),
            Shown::NoBackup => {}
            Shown::Done => {
                if let Some(cfg) = self.signed_in.take() {
                    cx.emit(SignedIn(cfg));
                }
            }
        }
    }

    fn login(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let username = self.username.read(cx).value().trim().to_lowercase();
        let password = Zeroizing::new(self.password.read(cx).value().to_string());
        let mut errors = HashMap::new();
        if self.pin.is_none() && self.server.read(cx).value().trim().is_empty() {
            errors.insert(Field::Server, t("signin.error.required").to_owned());
        }
        if username.is_empty() {
            errors.insert(Field::Username, t("signin.error.required").to_owned());
        }
        if password.is_empty() {
            errors.insert(Field::Password, t("signin.error.required").to_owned());
        }
        if let Some(first) = [Field::Server, Field::Username, Field::Password]
            .into_iter()
            .find(|f| errors.contains_key(f))
        {
            self.errors = errors;
            self.focus(first, window, cx);
            cx.notify();
            return;
        }
        let flow = match self.flow_for_server(cx) {
            Ok(flow) => flow,
            Err(e) => {
                self.errors.insert(Field::Server, e);
                self.focus(Field::Server, window, cx);
                cx.notify();
                return;
            }
        };
        self.password
            .update(cx, |s, cx| s.set_value("", window, cx));
        self.run(
            flow,
            Command::Login,
            move |f, rt| {
                let doc = rt.block_on(f.instance_doc()).ok();
                rt.block_on(f.login(&username, &password))
                    .map(|owes| (owes, doc))
            },
            |this, result, window, cx| match result {
                Ok((owes, doc)) => {
                    if doc.is_some() {
                        this.instance = doc;
                    }
                    this.after_owed(owes, window, cx);
                }
                Err(e) => this.apply(&e, Command::Login, window, cx),
            },
            window,
            cx,
        );
    }

    /// The flow for the typed server (or the pinned instance): the one already listed for it, else a new one.
    fn flow_for_server(&mut self, cx: &gpui::App) -> Result<SignIn, String> {
        if let Some(cfg) = &self.pin {
            wipe_not_pending(cx, cfg.id)?;
            return match self.flow.take() {
                Some(flow) => Ok(flow),
                None => SignIn::new(cfg.clone(), self.keychain.clone()).map_err(|e| e.to_string()),
            };
        }
        let typed = self.server.read(cx).value().trim().to_owned();
        let url = url::Url::parse(&typed)
            .ok()
            .filter(|u| u.has_host())
            .or_else(|| url::Url::parse(&format!("https://{typed}")).ok())
            .ok_or_else(|| tf("signin.error.other", &[("code", "E_SERVER")]))?;
        if let Some(flow) = self.flow.take()
            && flow.config().server == url
        {
            return Ok(flow);
        }
        let root = self
            .root
            .as_deref()
            .ok_or_else(|| tf("signin.error.other", &[("code", "E_STORE")]))?;
        let mut instances = Instances::load(root).map_err(|e| e.to_string())?;
        let cfg = match instances.list().iter().find(|c| c.server == url) {
            Some(cfg) => cfg.clone(),
            None => instances.add(url).map_err(|e| e.to_string())?,
        };
        wipe_not_pending(cx, cfg.id)?;
        SignIn::new(cfg, self.keychain.clone()).map_err(|e| e.to_string())
    }

    fn submit_totp(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let code: String = self
            .code
            .read(cx)
            .value()
            .chars()
            .filter(|c| !c.is_whitespace())
            .collect();
        let code = Zeroizing::new(code);
        if code.len() != 6 {
            let key = if code.is_empty() {
                "signin.error.required"
            } else {
                "factor.error.codeFormat"
            };
            self.errors.insert(Field::Code, t(key).to_owned());
            self.code.update(cx, |c, cx| {
                c.set_invalid(true, cx);
                c.focus(window, cx);
            });
            cx.notify();
            return;
        }
        let Some(flow) = self.flow.take() else { return };
        self.run(
            flow,
            Command::Totp,
            move |f, rt| rt.block_on(f.code(&code)),
            |this, result, window, cx| {
                this.code.update(cx, |c, cx| c.clear(cx));
                match result {
                    Ok(()) => this.go(Shown::Key, window, cx),
                    Err(e) => this.apply(&e, Command::Totp, window, cx),
                }
            },
            window,
            cx,
        );
    }

    /// The passkey on its own, or for a login that owes it: the instance's options go to the system browser and its
    /// answer back (bound to the held login when there is one).
    fn passkey(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        if self.busy.is_some() {
            return;
        }
        self.password
            .update(cx, |s, cx| s.set_value("", window, cx));
        self.banner = None;
        let flow = match self.flow_for_server(cx) {
            Ok(flow) => flow,
            Err(e) => {
                self.errors.insert(Field::Server, e);
                cx.notify();
                return;
            }
        };
        let (server, browser) = (flow.config().server.clone(), self.browser.clone());
        let cancel = Cancel::new();
        self.ceremony = Some(cancel.clone());
        self.run(
            flow,
            Command::Passkey,
            move |f, rt| {
                let doc = rt.block_on(f.instance_doc()).ok();
                let ceremony = rt.block_on(f.passkey_begin())?;
                let result = rt
                    .block_on(browser.run_cancellable(
                        &server,
                        CeremonyKind::Get,
                        &ceremony.options,
                        &cancel,
                    ))
                    .map_err(SignInError::Browser)?;
                let response =
                    result
                        .response
                        .ok_or(SignInError::Browser(BrowserAuthError::Invalid(
                            "browser completion",
                        )))?;
                // A reply that landed just before a cancel is not applied.
                if cancel.is_cancelled() {
                    return Err(SignInError::Browser(BrowserAuthError::Cancelled));
                }
                rt.block_on(f.passkey_finish(ceremony.id, &response))
                    .map(|owes| (owes, doc))
            },
            |this, result, window, cx| {
                this.ceremony = None;
                match result {
                    Ok((owes, doc)) => {
                        if doc.is_some() {
                            this.instance = doc;
                        }
                        this.after_owed(owes, window, cx)
                    }
                    // The person ended it: nothing to say.
                    Err(SignInError::Browser(BrowserAuthError::Cancelled)) => {}
                    Err(e) => this.apply(&e, Command::Passkey, window, cx),
                }
            },
            window,
            cx,
        );
    }

    fn oidc(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        if self.busy.is_some() {
            return;
        }
        self.banner = None;
        self.errors.clear();
        let Ok(flow) = self.flow_for_server(cx) else {
            return;
        };
        let (server, browser) = (flow.config().server.clone(), self.browser.clone());
        let cancel = Cancel::new();
        self.ceremony = Some(cancel.clone());
        self.run(
            flow,
            Command::Oidc,
            move |f, rt| {
                let doc = rt.block_on(f.instance_doc()).ok();
                let result = rt
                    .block_on(browser.run_cancellable(&server, CeremonyKind::Oidc, "", &cancel))
                    .map_err(SignInError::Browser)?;
                let assertion =
                    result
                        .assertion
                        .ok_or(SignInError::Browser(BrowserAuthError::Invalid(
                            "browser completion",
                        )))?;
                // A reply that landed just before a cancel is not applied.
                if cancel.is_cancelled() {
                    return Err(SignInError::Browser(BrowserAuthError::Cancelled));
                }
                Ok((f.oidc(assertion, result.needs), doc))
            },
            |this, result, window, cx| {
                this.ceremony = None;
                match result {
                    Ok((owes, doc)) => {
                        if doc.is_some() {
                            this.instance = doc;
                        }
                        this.after_owed(owes, window, cx)
                    }
                    Err(SignInError::Browser(BrowserAuthError::Cancelled)) => {}
                    Err(e) => this.apply(&e, Command::Oidc, window, cx),
                }
            },
            window,
            cx,
        );
    }

    /// The backup code is checked for its form and held; the key step sends it (R6).
    fn hold_backup(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let code = normalise_backup_code(self.backup.read(cx).value().as_ref());
        if !is_backup_code(&code) {
            self.errors
                .insert(Field::Backup, t("factorSignin.backup.format").to_owned());
            self.focus(Field::Backup, window, cx);
            cx.notify();
            return;
        }
        self.backup.update(cx, |s, cx| s.set_value("", window, cx));
        self.backup_held = Some(code);
        self.go(Shown::Key, window, cx);
    }

    fn key_changed(&mut self, cx: &mut Context<Self>) {
        self.note_settled = false;
        self.key_edits += 1;
        let edit = self.key_edits;
        let looks = is_backup_code(&normalise_backup_code(self.key.read(cx).value().as_ref()));
        if looks {
            cx.spawn(async move |this, cx| {
                cx.background_executor().timer(BACKUP_NOTE).await;
                let _ = this.update(cx, |this, cx| {
                    if this.key_edits == edit {
                        this.note_settled = true;
                        cx.notify();
                    }
                });
            })
            .detach();
        }
        cx.notify();
    }

    fn submit_key(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let key = Zeroizing::new(self.key.read(cx).value().to_string());
        if normalise_recovery_key(&key).chars().count() != KEY_LENGTH {
            self.errors
                .insert(Field::Key, t("signin.error.keyLength").to_owned());
            self.focus(Field::Key, window, cx);
            cx.notify();
            return;
        }
        let Some(flow) = self.flow.take() else { return };
        let held = self.backup_held.take();
        self.run(
            flow,
            Command::Key,
            move |f, rt| match held {
                Some(code) => rt.block_on(f.add_device_with_backup(&code, &key)),
                None => rt.block_on(f.add_device(&key)),
            },
            |this, result, window, cx| match result {
                Ok(()) => {
                    this.key.update(cx, |s, cx| s.set_value("", window, cx));
                    this.signed_in = this.flow.as_ref().map(|f| f.config().clone());
                    this.go(Shown::Done, window, cx);
                }
                Err(e) => this.apply(&e, Command::Key, window, cx),
            },
            window,
            cx,
        );
    }

    /// Ends a system-browser ceremony under way; true when there was one (its step answers Cancelled and stays).
    fn cancel_ceremony(&mut self) -> bool {
        match self.ceremony.take() {
            Some(c) => {
                c.cancel();
                true
            }
            None => false,
        }
    }

    fn back(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        if self.cancel_ceremony() || self.busy.is_some() {
            return;
        }
        self.banner = None;
        self.errors.clear();
        self.back_to_login(window, cx);
        cx.notify();
    }

    fn create_instead(&mut self, cx: &mut Context<Self>) {
        if self.cancel_ceremony() || self.busy.is_some() {
            return;
        }
        if let Some(flow) = self.flow.as_mut() {
            flow.back();
        }
        self.backup_held = None;
        cx.emit(CreateInstead);
    }

    /// A labelled field; a press anywhere on it (its label included) focuses its input, as a `<label for>` does.
    fn field(
        &self,
        cx: &mut Context<Self>,
        id: &'static str,
        label: &str,
        input: &Entity<InputState>,
        field: Field,
    ) -> AnyElement {
        let mut f = TextField::new(id, label.to_owned(), input);
        if let Some(e) = self.errors.get(&field) {
            f = f.error(e.clone());
        }
        tracked_box(
            id,
            div()
                .on_mouse_down(
                    gpui::MouseButton::Left,
                    cx.listener(move |this, _: &gpui::MouseDownEvent, window, cx| {
                        this.focus(field, window, cx)
                    }),
                )
                .child(f),
        )
        .into_any_element()
    }

    fn nav(&self, cx: &mut Context<Self>) -> AnyElement {
        nav_row(
            link(
                "signin-back",
                t("factor.back"),
                true,
                cx.listener(|this, _: &ClickEvent, window, cx| this.back(window, cx)),
            ),
            Some(link(
                "signin-create",
                t("signin.login.createInstead"),
                false,
                cx.listener(|this, _: &ClickEvent, _, cx| this.create_instead(cx)),
            )),
        )
    }
}

impl Render for SignInView {
    fn render(&mut self, _window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let instance = self.instance_name();
        let total: u8 = if self.shown == Shown::Login || self.factor_shown {
            4
        } else {
            3
        };
        let key_step: u8 = if self.factor_shown { 3 } else { 2 };
        let step = |n: u8| {
            tf(
                "signin.step",
                &[("n", &n.to_string()), ("total", &total.to_string())],
            )
        };
        let submit = |id: &'static str, label: &str, ready: bool, cx: &mut Context<Self>| {
            Action::new(
                id,
                label.to_owned(),
                cx.listener(|this, _: &ClickEvent, window, cx| this.submit(window, cx)),
            )
            .ready(ready)
        };
        let busy = self.busy;
        let passkeys = self.instance.as_ref().is_some_and(|d| d.passkeys);
        let frame = match self.shown {
            Shown::Login => {
                let mut frame =
                    AuthFrame::new(tf("signin.login.title", &[("instance", &instance)]))
                        .step(step(1))
                        .progress(1, total)
                        .child(para(tf("signin.login.body", &[("instance", &instance)])));
                if self.pin.is_none() {
                    frame = frame.child(self.field(
                        cx,
                        "signin-server",
                        t("diagnostics.instance"),
                        &self.server,
                        Field::Server,
                    ));
                }
                frame = frame
                    .child(self.field(
                        cx,
                        "signin-username",
                        t("signin.login.username"),
                        &self.username,
                        Field::Username,
                    ))
                    .child(self.field(
                        cx,
                        "signin-password",
                        t("signin.login.password"),
                        &self.password,
                        Field::Password,
                    ));
                let mut links = Vec::new();
                if self.instance.as_ref().is_some_and(|d| d.oidc) {
                    links.push(link(
                        "signin-oidc",
                        t(if busy == Some(Command::Oidc) {
                            "factorSignin.oidc.working"
                        } else {
                            "factorSignin.oidc.submit"
                        }),
                        false,
                        cx.listener(|this, _: &ClickEvent, window, cx| this.oidc(window, cx)),
                    ));
                }
                // The desktop has no passkey autofill (webauthn.ts refuses a conditional request in the shell): the button.
                if passkeys {
                    links.push(link(
                        "signin-passkey",
                        t(if busy == Some(Command::Passkey) {
                            "factorSignin.passkey.working"
                        } else {
                            "factorSignin.usePasskey"
                        }),
                        false,
                        cx.listener(|this, _: &ClickEvent, window, cx| this.passkey(window, cx)),
                    ));
                }
                if !links.is_empty() {
                    frame = frame.child(links_row(links));
                }
                frame
                    .action(submit(
                        "signin-continue",
                        t(if busy == Some(Command::Login) {
                            "signin.login.working"
                        } else {
                            "signin.login.submit"
                        }),
                        busy.is_none(),
                        cx,
                    ))
                    .below(link(
                        "signin-create",
                        t("signin.login.createInstead"),
                        false,
                        cx.listener(|this, _: &ClickEvent, _, cx| this.create_instead(cx)),
                    ))
            }
            Shown::Totp => {
                let complete = self.code.read(cx).is_complete();
                let mut frame = AuthFrame::new(t("signin.totp.title"))
                    .step(step(2))
                    .progress(2, total);
                if self.flow.as_ref().is_some_and(SignIn::used_passkey) {
                    frame = frame.child(para(tf(
                        "factorSignin.passkey.needsCode",
                        &[("instance", &instance)],
                    )));
                }
                frame = frame
                    .child(para(t("signin.totp.body")))
                    .child(tracked_box("signin-code", self.code.clone()));
                if let Some(e) = self.errors.get(&Field::Code) {
                    frame = frame.child(error_line(e.clone()));
                }
                let mut links = Vec::new();
                if passkeys && self.flow.as_ref().is_some_and(SignIn::holds_passkey) {
                    links.push(link(
                        "signin-passkey",
                        t(if busy == Some(Command::Passkey) {
                            "factorSignin.passkey.working"
                        } else {
                            "factorSignin.code.usePasskey"
                        }),
                        false,
                        cx.listener(|this, _: &ClickEvent, window, cx| this.passkey(window, cx)),
                    ));
                }
                links.push(link(
                    "signin-use-backup",
                    t("factorSignin.useBackup"),
                    false,
                    cx.listener(|this, _: &ClickEvent, window, cx| {
                        if this.busy.is_none() {
                            this.banner = None;
                            this.code.update(cx, |c, cx| c.clear(cx));
                            this.go(Shown::Backup, window, cx);
                        }
                    }),
                ));
                frame
                    .child(links_row(links))
                    .action(submit(
                        "signin-verify",
                        t(if busy == Some(Command::Totp) {
                            "signin.login.working"
                        } else {
                            "signin.totp.submit"
                        }),
                        busy.is_none() && complete,
                        cx,
                    ))
                    .below(self.nav(cx))
            }
            Shown::Passkey => AuthFrame::new(t("factorSignin.passkey.title"))
                .step(step(2))
                .progress(2, total)
                .child(para(t("factorSignin.passkey.body")))
                .child(links_row(vec![link(
                    "signin-use-backup",
                    t("factorSignin.useBackup"),
                    false,
                    cx.listener(|this, _: &ClickEvent, window, cx| {
                        if this.busy.is_none() {
                            this.banner = None;
                            this.go(Shown::Backup, window, cx);
                        }
                    }),
                )]))
                .action(submit(
                    "signin-passkey-submit",
                    t(if busy == Some(Command::Passkey) {
                        "factorSignin.passkey.working"
                    } else {
                        "factorSignin.passkey.submit"
                    }),
                    busy.is_none(),
                    cx,
                ))
                .below(self.nav(cx)),
            Shown::Backup => {
                let mut frame = AuthFrame::new(t("factorSignin.backup.title"))
                    .step(step(2))
                    .progress(2, total);
                if !passkeys && self.flow.as_ref().and_then(SignIn::owes) == Some(Owed::Passkey) {
                    frame = frame.child(para(t("factorSignin.passkey.unusable")));
                }
                frame
                    .child(para(t("factorSignin.backup.body")))
                    .child(self.field(
                        cx,
                        "signin-backup",
                        t("factorSignin.backup.label"),
                        &self.backup,
                        Field::Backup,
                    ))
                    .action(submit(
                        "signin-backup-submit",
                        t("signin.login.submit"),
                        busy.is_none(),
                        cx,
                    ))
                    .below(nav_row(
                        link(
                            "signin-backup-back",
                            t("factor.back"),
                            true,
                            cx.listener(|this, _: &ClickEvent, window, cx| {
                                if this.busy.is_none() {
                                    this.backup.update(cx, |s, cx| s.set_value("", window, cx));
                                    let owes = this.flow.as_ref().and_then(SignIn::owes);
                                    this.go(
                                        if owes == Some(Owed::Passkey) {
                                            Shown::Passkey
                                        } else {
                                            Shown::Totp
                                        },
                                        window,
                                        cx,
                                    );
                                }
                            }),
                        ),
                        None,
                    ))
            }
            Shown::Key => {
                let typed = self.key.read(cx).value().to_string();
                let n = normalise_recovery_key(&typed).chars().count();
                let looks = is_backup_code(&normalise_backup_code(&typed));
                let mut f = TextField::new("signin-key", t("signin.key.label"), &self.key)
                    .hint(tf("signin.key.hint", &[("n", &n.to_string())]));
                if let Some(e) = self.errors.get(&Field::Key) {
                    f = f.error(e.clone());
                }
                let mut frame = AuthFrame::new(t("signin.key.title"))
                    .step(step(key_step))
                    .progress(key_step, total)
                    .child(para(t("signin.key.body")))
                    .child(tracked_box(
                        "signin-key",
                        div()
                            .on_mouse_down(
                                gpui::MouseButton::Left,
                                cx.listener(|this, _: &gpui::MouseDownEvent, window, cx| {
                                    this.focus(Field::Key, window, cx)
                                }),
                            )
                            .child(f),
                    ));
                if looks && self.note_settled {
                    frame = frame.child(tracked_box(
                        "signin-key-backup",
                        para(t("signIn.key.isBackupCode")),
                    ));
                }
                if n != KEY_LENGTH {
                    frame = frame.child(note(t("signin.error.keyLength")));
                }
                if busy == Some(Command::Key) {
                    frame = frame.child(para(t("signin.key.working")));
                }
                frame
                    .action(submit(
                        "signin-add",
                        t("signin.key.submit"),
                        busy.is_none() && n == KEY_LENGTH,
                        cx,
                    ))
                    .below(self.nav(cx))
            }
            Shown::NoBackup => AuthFrame::new(t("signin.key.title"))
                .step(step(key_step))
                .progress(key_step, total)
                .banner(
                    self.no_backup
                        .map(|k| (BannerTone::Danger, tf(k, &[("instance", &instance)]))),
                )
                .below(link(
                    "signin-cancel",
                    t("signin.cancel"),
                    false,
                    cx.listener(|this, _: &ClickEvent, window, cx| this.back(window, cx)),
                )),
            Shown::Done => {
                let who = self
                    .flow
                    .as_ref()
                    .map(|f| f.username().to_owned())
                    .filter(|u| !u.is_empty())
                    .unwrap_or_else(|| self.username.read(cx).value().trim().to_lowercase());
                AuthFrame::new(t("signin.done.title"))
                    .step(step(total))
                    .progress(total, total)
                    .child(para(tf(
                        "signin.done.body",
                        &[("username", &who), ("instance", &instance)],
                    )))
                    .action(submit("signin-open", t("signin.done.next"), true, cx))
            }
        };
        let frame = if self.shown == Shown::NoBackup {
            frame
        } else {
            frame.banner(self.banner.clone())
        };
        div()
            .id("signin")
            .on_key_down(cx.listener(|this, e: &gpui::KeyDownEvent, _, cx| {
                // Escape ends a ceremony waiting on the system browser.
                if e.keystroke.key == "escape" && this.cancel_ceremony() {
                    cx.stop_propagation();
                }
            }))
            .on_action(|_: &FocusNext, window, cx| window.focus_next(cx))
            .on_action(|_: &FocusPrevious, window, cx| window.focus_prev(cx))
            .size_full()
            .child(frame.render())
    }
}

impl Drop for SignInView {
    /// A ceremony left waiting on the system browser ends with the screen.
    fn drop(&mut self) {
        if let Some(c) = self.ceremony.take() {
            c.cancel();
        }
    }
}

impl SignInView {
    /// Automation: the banner the sign-in shows (an error or a note), if any.
    #[cfg(feature = "test-hooks")]
    pub fn banner_text(&self) -> Option<&str> {
        self.banner.as_ref().map(|(_, text)| text.as_str())
    }
}

/// Re-review C: a Sign out's wipe of instance `id` is still pending (its old store is open, and the wipe would delete a
/// new session's store): the sign-in for it waits, saying so (`E_FORGET_STORE`).
fn wipe_not_pending(cx: &gpui::App, id: dilla_client::seams::InstanceId) -> Result<(), String> {
    if crate::windows::wipe_pending(cx, id) {
        Err(tf("factor.error.other", &[("code", "E_FORGET_STORE")]))
    } else {
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Re-review C: while a Sign out's wipe of an instance is pending, a sign-in for it is refused with the
    /// E_FORGET_STORE notice (both the pinned and the typed-server paths of `flow_for_server` ask this first).
    #[gpui::test]
    fn a_sign_in_waits_for_a_pending_wipe(cx: &mut gpui::TestAppContext) {
        cx.update(crate::install);
        let id = dilla_client::seams::InstanceId(5);
        cx.update(|cx| {
            assert!(wipe_not_pending(cx, id).is_ok());
            crate::windows::wipe_begin(cx, id);
            assert_eq!(
                wipe_not_pending(cx, id),
                Err(tf("factor.error.other", &[("code", "E_FORGET_STORE")]))
            );
            crate::windows::wipe_end(cx, id);
            assert!(
                wipe_not_pending(cx, id).is_ok(),
                "the wipe ended: the sign-in goes on"
            );
        });
    }

    /// SignIn.tsx `signInRefusal`: the step a refusal shows on and what it says, by kind only.
    #[test]
    fn refusals_follow_the_web_table() {
        let r = refusal(&SignInError::WrongKey, Command::Key, "Team");
        assert_eq!(r.field.unwrap().0, Field::Key);
        let r = refusal(&SignInError::CodeFailed, Command::Totp, "Team");
        assert!(r.back_to_login);
        assert_eq!(r.banner.unwrap().1, t("signin.error.totpFailed"));
        let r = refusal(&SignInError::BackupFailed, Command::Key, "Team");
        assert!(r.back_to_login);
        assert_eq!(r.banner.unwrap().1, t("factorSignin.backup.failed"));
        let r = refusal(&SignInError::Expired, Command::Key, "Team");
        assert_eq!(r.banner.unwrap().1, t("enrol.expired"));
        let r = refusal(&SignInError::DeviceCap, Command::Key, "Team");
        assert!(r.banner.unwrap().1.contains("Team"));
        let r = refusal(&SignInError::PasskeyUnknown, Command::Passkey, "Team");
        assert!(r.banner.unwrap().1.starts_with("Team"));
        let r = refusal(&SignInError::Other("E_X".into()), Command::Passkey, "Team");
        assert_eq!(r.banner.unwrap().1, t("factorSignin.passkey.failed"));
        let r = refusal(&SignInError::Other("E_X".into()), Command::Login, "Team");
        assert!(r.banner.unwrap().1.contains("E_X"));
        let r = refusal(&SignInError::NoBackup, Command::Key, "Team");
        assert_eq!(r.no_backup, Some("signin.error.noBackup"));
        let r = refusal(
            &SignInError::Browser(BrowserAuthError::Unsupported),
            Command::Oidc,
            "Team",
        );
        assert_eq!(r.banner.unwrap().1, t("factorSignin.oidc.unsupported"));
    }
}
