//! The sign-up ceremony (web `screens/Onboarding.tsx`, the prototype's six steps): 1 the invite, 2 the account (Create
//! account makes the keys), 3 the recovery key (I saved it, once ticked, creates the account on the instance), then the
//! second factor's choice and code (Task 26's factor views, on the running client) and 6 done ([`super::done`]). The
//! account is created only after the key is confirmed, so leaving at any step before that leaves no account; a shown key
//! is never offered again (Back from the key step and a refused creation discard it, and the next Create account makes a
//! new one).
//!
//! The protocol is `dilla_client::features::signup`; each step runs on a thread of its own with a small runtime, so the
//! window never waits on the network.
//!
//! Against the web screen: a browser is already on its instance, the desktop is not, so step 1's invite link also names
//! the instance (its origin). A bare code needs the link; an open instance's sign-up without an invite is reached by the
//! instance manager (Task 27), which hands its address in as `server`.
//!
//! **Secrets.** The recovery key lives in a zeroizing buffer while step 3 shows; its text is drawn and never written
//! anywhere by the app ("Save as file" writes it only to the file the person picks). The password is read from its field
//! into a zeroizing buffer for the one submit, and the field is cleared on Back, on sign-in and after the account is
//! made. Drawing the key makes copies the app cannot zeroize: each frame builds a `SharedString` per group of four, and
//! GPUI's text system shapes (and may cache) those lines. They live only while step 3 is drawn; nothing else holds them.
//!
//! **The instance.** Step 1 checks the link (`parse_invite_link`, the desktop's `canonicalInstance`) and shows an
//! instance this device does not know yet for confirmation ("Connect to <host>?", main.ts `confirmInstance`) before it
//! is added to the device or asked anything.

use std::path::PathBuf;
use std::sync::Arc;

use dilla_client::features::signup::{
    self as su, InstanceInfo, RecoveryKey, RegistrationMode, Resumed, Signup, SignupError,
    Submitted,
};
use dilla_client::instances::Instances;
use dilla_client::seams::{InstanceConfig, Keychain};
use gpui::{
    AnyElement, AppContext as _, Context, Entity, EventEmitter, FocusHandle, Focusable,
    InteractiveElement, IntoElement, ParentElement, Render, SharedString, Styled, Subscription,
    Window, div,
};
use gpui_component::input::{InputEvent, InputState};
use zeroize::Zeroizing;

use dilla_desktop_ui::IconName;
use dilla_desktop_ui::components::banner::{Banner, BannerTone};
use dilla_desktop_ui::components::onboarding::{
    KeyAck, KeyAction, KeyGroups, NavLink, OnboardingField, OnboardingFrame, PrimaryAction,
    RecoveryKeyLayout, frame,
};
use dilla_desktop_ui::tokens::color;

use super::keys::{Back, Submit};
use crate::screens::{FocusNext, FocusPrevious};
use crate::strings::{t, tf};
use crate::{tracked, tracked_box, tracked_focus};

/// A new account has six steps (Onboarding.tsx `TOTAL`).
pub const TOTAL: u8 = 6;

/// The account exists on the instance and its device list is published: the client can start (it owes its second
/// factor, so it starts in `FactorSetup`).
pub struct SignedUp {
    pub cfg: InstanceConfig,
    pub submitted: Submitted,
}

/// "Already have an account? Sign in".
pub struct WantsSignIn;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Step {
    Connect,
    Identity,
    Keys,
    /// The instance takes no new accounts.
    Closed,
    /// "Connect to <host>?": an instance this device does not know yet, shown before anything is stored or sent.
    Confirm,
}

// main.ts `confirmInstance`'s copy: the desktop's own (not in the web strings `strings.rs` is generated from), kept with
// the instance manager's in one module (Task 27's `instances::copy`).
use crate::screens::instances::copy::{
    CANCEL as CONFIRM_CANCEL, CONFIRM_CONNECT, CONFIRM_DETAIL, CONFIRM_INSTANCE, CONFIRM_TITLE,
};

#[derive(Clone)]
struct BannerState {
    tone: BannerTone,
    text: SharedString,
    /// The E_NETWORK row: the account may exist, so only Reload (resume) goes on.
    reload: bool,
}

#[derive(Default)]
struct FieldErrors {
    invite: Option<SharedString>,
    username: Option<SharedString>,
    password: Option<SharedString>,
}

/// The focus handles of the step's pressable parts, for the automation build's `focused` and for focus moves.
struct Handles {
    next: FocusHandle,
    back: FocusHandle,
    sign_in: FocusHandle,
    print: FocusHandle,
    save: FocusHandle,
    ack: FocusHandle,
}

pub struct SignupView {
    root: Option<PathBuf>,
    keychain: Arc<dyn Keychain>,
    step: Step,
    busy: bool,
    stalled: bool,
    banner: Option<BannerState>,
    errors: FieldErrors,
    invite: Entity<InputState>,
    username: Entity<InputState>,
    password: Entity<InputState>,
    flow: Option<Signup>,
    instance: Option<InstanceInfo>,
    key: Option<RecoveryKey>,
    acknowledged: bool,
    /// The invite link step 1 checked, waiting for "Connect" (nothing stored or sent yet).
    confirming: Option<su::InviteLink>,
    handles: Handles,
    _subscriptions: Vec<Subscription>,
}

impl EventEmitter<SignedUp> for SignupView {}
impl EventEmitter<WantsSignIn> for SignupView {}

/// The server an invite link names: its origin. `None` for a bare code.
pub fn server_of(invite: &str) -> Option<url::Url> {
    let url = url::Url::parse(invite.trim()).ok()?;
    if !matches!(url.scheme(), "http" | "https") || url.host_str().is_none() {
        return None;
    }
    url::Url::parse(&url.origin().ascii_serialization()).ok()
}

/// refusalView's row for `e`: the step to show, the field error, the banner.
fn refusal_view(
    e: &SignupError,
    instance: &str,
) -> (Option<Step>, FieldErrors, Option<BannerState>) {
    let banner = |tone, text: String, reload| {
        Some(BannerState {
            tone,
            text: text.into(),
            reload,
        })
    };
    match e {
        SignupError::InviteInvalid => (
            Some(Step::Connect),
            FieldErrors {
                invite: Some(t("onboarding.error.inviteInvalid").into()),
                ..FieldErrors::default()
            },
            None,
        ),
        SignupError::UsernameTaken => (
            Some(Step::Identity),
            FieldErrors {
                username: Some(t("onboarding.error.usernameTaken").into()),
                ..FieldErrors::default()
            },
            None,
        ),
        SignupError::DetailsRefused => (
            Some(Step::Identity),
            FieldErrors::default(),
            banner(
                BannerTone::Danger,
                tf("onboarding.error.detailsRefused", &[("instance", instance)]),
                false,
            ),
        ),
        SignupError::Closed => (Some(Step::Closed), FieldErrors::default(), None),
        SignupError::RateLimited { retry_after_ms } => (
            None,
            FieldErrors::default(),
            banner(
                BannerTone::Warn,
                match retry_after_ms {
                    Some(ms) => tf(
                        "onboarding.error.rateLimited",
                        &[("seconds", &ms.div_ceil(1000).to_string())],
                    ),
                    None => t("onboarding.error.rateLimitedNoWait").to_owned(),
                },
                false,
            ),
        ),
        SignupError::Stalled | SignupError::Network => (
            None,
            FieldErrors::default(),
            banner(
                BannerTone::Danger,
                t("onboarding.error.network").to_owned(),
                true,
            ),
        ),
        other => (
            None,
            FieldErrors::default(),
            banner(
                BannerTone::Danger,
                tf("onboarding.error.other", &[("code", other.code())]),
                false,
            ),
        ),
    }
}

impl SignupView {
    /// `invite`: an invite handed in (Task 27's `Launch.invite`, the GPUI form of `/welcome?invite=`), shown in step 1.
    pub fn new(
        root: Option<PathBuf>,
        keychain: Arc<dyn Keychain>,
        invite: Option<String>,
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
        let invite_field = field(t("onboarding.connect.invitePlaceholder"), false, window, cx);
        let username = field("", false, window, cx);
        let password = field(
            t("onboarding.identity.passwordPlaceholder"),
            true,
            window,
            cx,
        );
        if let Some(code) = invite.filter(|i| !i.trim().is_empty()) {
            invite_field.update(cx, |s, cx| s.set_value(code, window, cx));
        }
        let mut subscriptions = Vec::new();
        for (input, which) in [(&invite_field, 0u8), (&username, 1), (&password, 2)] {
            subscriptions.push(cx.subscribe_in(
                input,
                window,
                move |this: &mut Self, _, event: &InputEvent, window, cx| match event {
                    InputEvent::PressEnter { .. } => this.submit(window, cx),
                    InputEvent::Change => {
                        // Typing clears that field's refusal.
                        let slot = match which {
                            0 => &mut this.errors.invite,
                            1 => &mut this.errors.username,
                            _ => &mut this.errors.password,
                        };
                        if slot.take().is_some() {
                            cx.notify();
                        }
                    }
                    _ => {}
                },
            ));
        }
        invite_field.update(cx, |s, cx| s.focus(window, cx));
        Self {
            root,
            keychain,
            step: Step::Connect,
            busy: false,
            stalled: false,
            banner: None,
            errors: FieldErrors::default(),
            invite: invite_field,
            username,
            password,
            flow: None,
            instance: None,
            key: None,
            acknowledged: false,
            confirming: None,
            handles: Handles {
                next: cx.focus_handle().tab_stop(true),
                back: cx.focus_handle().tab_stop(true),
                sign_in: cx.focus_handle().tab_stop(true),
                print: cx.focus_handle().tab_stop(true),
                save: cx.focus_handle().tab_stop(true),
                ack: cx.focus_handle().tab_stop(true),
            },
            _subscriptions: subscriptions,
        }
    }

    pub fn step(&self) -> Step {
        self.step
    }

    /// `/welcome?signin=race|evicted` (Onboarding.tsx `RACE_BANNER`, `EVICTED_BANNER`): the welcome shown again after a
    /// sign-in's list race or eviction says why. No shipping path opens `/welcome?signin=` yet, so only the automation
    /// build's pre-account fixtures set it (wave 2 review, lane F minor).
    #[cfg(feature = "automation")]
    pub fn set_notice(
        &mut self,
        notice: Option<crate::router::SignInNotice>,
        cx: &mut Context<Self>,
    ) {
        use crate::router::SignInNotice;
        self.banner = notice.map(|n| match n {
            SignInNotice::Race => BannerState {
                tone: BannerTone::Warn,
                text: t("signin.error.listRace").into(),
                reload: false,
            },
            SignInNotice::Evicted => BannerState {
                tone: BannerTone::Danger,
                text: t("signin.error.evicted").into(),
                reload: false,
            },
        });
        cx.notify();
    }

    /// Automation (parity fixtures, `fixture::pre_account`): the step the instance's answers left the web on, drawn
    /// offline: `instance` is what step 1's instance read returned, `key` the keys Create account made.
    #[cfg(feature = "automation")]
    pub fn fixture_at(
        &mut self,
        step: Step,
        instance: InstanceInfo,
        key: Option<RecoveryKey>,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        self.instance = Some(instance);
        self.key = key;
        self.go(step, window, cx);
    }

    fn instance_name(&self) -> String {
        self.instance
            .as_ref()
            .map_or_else(String::new, |i| i.name.clone())
    }

    fn blocked(&self) -> bool {
        self.busy || self.stalled
    }

    /// Runs one step of the flow off the UI thread and hands the flow back with its result.
    fn run<T: Send + 'static>(
        &mut self,
        flow: Signup,
        step: impl FnOnce(&mut Signup, &tokio::runtime::Runtime) -> Result<T, SignupError>
        + Send
        + 'static,
        then: impl FnOnce(&mut Self, Result<T, SignupError>, &mut Window, &mut Context<Self>) + 'static,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        self.busy = true;
        cx.notify();
        let (tx, rx) = tokio::sync::oneshot::channel();
        std::thread::Builder::new()
            .name("dilla-sign-up".into())
            .spawn(move || {
                let mut flow = flow;
                let result = tokio::runtime::Builder::new_current_thread()
                    .enable_all()
                    .build()
                    .map_err(|_| SignupError::Other("E_RUNTIME".into()))
                    .and_then(|rt| step(&mut flow, &rt));
                let _ = tx.send((flow, result));
            })
            .expect("a thread starts");
        cx.spawn_in(window, async move |this, cx| {
            if let Ok((flow, result)) = rx.await {
                let _ = this.update_in(cx, |this, window, cx| {
                    this.busy = false;
                    this.flow = Some(flow);
                    then(this, result, window, cx);
                    cx.notify();
                });
            }
        })
        .detach();
    }

    fn apply_refusal(&mut self, e: &SignupError, window: &mut Window, cx: &mut Context<Self>) {
        let (step, errors, banner) = refusal_view(e, &self.instance_name());
        if matches!(e, SignupError::Stalled) {
            self.stalled = true;
        }
        self.banner = banner;
        self.errors = errors;
        if let Some(step) = step {
            self.go(step, window, cx);
        }
        self.focus_first_error(window, cx);
        cx.notify();
    }

    fn focus_first_error(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        if self.errors.invite.is_some() {
            self.invite.update(cx, |s, cx| s.focus(window, cx));
        } else if self.errors.username.is_some() {
            self.username.update(cx, |s, cx| s.focus(window, cx));
        } else if self.errors.password.is_some() {
            self.password.update(cx, |s, cx| s.focus(window, cx));
        }
    }

    fn go(&mut self, step: Step, window: &mut Window, cx: &mut Context<Self>) {
        if self.step == Step::Keys && step != Step::Keys {
            // The shown key goes with the step.
            self.key = None;
            self.acknowledged = false;
        }
        self.step = step;
        match step {
            Step::Connect => self.invite.update(cx, |s, cx| s.focus(window, cx)),
            Step::Identity => self.username.update(cx, |s, cx| s.focus(window, cx)),
            Step::Keys => window.focus(&self.handles.ack, cx),
            Step::Closed => window.focus(&self.handles.sign_in, cx),
            Step::Confirm => window.focus(&self.handles.next, cx),
        }
        cx.notify();
    }

    /// Enter outside a field. In a field the field's own Enter (`PressEnter`) submits, and the frame's binding sees the
    /// same keystroke: it is ignored there, so one Enter never takes two steps (step 1 then straight past "Connect").
    fn submit_from_key(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let in_field = [&self.invite, &self.username, &self.password]
            .iter()
            .any(|f| f.focus_handle(cx).contains_focused(window, cx));
        if !in_field {
            self.submit(window, cx);
        }
    }

    /// Enter in a field, the step's action, or Enter anywhere in the frame.
    pub fn submit(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        if self.blocked() {
            return;
        }
        match self.step {
            Step::Connect => self.connect_next(window, cx),
            Step::Identity => self.identity_next(window, cx),
            Step::Keys => self.keys_next(window, cx),
            Step::Confirm => self.connect_confirmed(window, cx),
            Step::Closed => {}
        }
    }

    fn flow_for(&mut self, server: url::Url) -> Result<Signup, SignupError> {
        if let Some(flow) = self.flow.take() {
            if flow.config().server == server {
                return Ok(flow);
            }
            // Another instance: keys left pending there are reset by its next begin.
            self.instance = None;
        }
        let root = self
            .root
            .as_deref()
            .ok_or_else(|| SignupError::Store("this system has no data directory".into()))?;
        let mut instances = Instances::load(root)?;
        let cfg = match instances.list().iter().find(|c| c.server == server) {
            Some(cfg) => cfg.clone(),
            None => instances.add(server)?,
        };
        Signup::new(cfg, self.keychain.clone())
    }

    /// Step 1's Continue: the link is checked (`parse_invite_link`, policy.ts `canonicalInstance`) and nothing is stored
    /// or sent yet. An instance already on this device goes on at once; any other is shown for confirmation first
    /// (main.ts `confirmInstance`).
    fn connect_next(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        self.banner = None;
        let typed = self.invite.read(cx).value().to_string();
        let link = match su::parse_invite_link(&typed, cfg!(debug_assertions)) {
            Ok(link) => link,
            Err(e) => {
                let key = if typed.trim().is_empty()
                    || e == su::InviteLinkError::Invalid && server_of(&typed).is_none()
                {
                    "onboarding.error.inviteRequired"
                } else {
                    "onboarding.error.inviteInvalid"
                };
                self.errors.invite = Some(t(key).into());
                self.focus_first_error(window, cx);
                cx.notify();
                return;
            }
        };
        let known = self
            .root
            .as_deref()
            .and_then(|root| Instances::load(root).ok())
            .is_some_and(|list| list.list().iter().any(|c| c.server == link.instance));
        self.confirming = Some(link);
        if known {
            self.connect_confirmed(window, cx);
        } else {
            self.go(Step::Confirm, window, cx);
        }
    }

    /// "Connect": only now is the instance added to this device and asked anything.
    fn connect_confirmed(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let Some(link) = self.confirming.clone() else {
            return self.go(Step::Connect, window, cx);
        };
        let (server, code) = (link.instance, link.code);
        let flow = match self.flow_for(server) {
            Ok(flow) => flow,
            Err(e) => return self.apply_refusal(&e, window, cx),
        };
        self.run(
            flow,
            |f, rt| {
                rt.block_on(async {
                    let resumed = f.resume().await?;
                    let info = f.instance().await?;
                    Ok((resumed, info))
                })
            },
            move |this, result, window, cx| match result {
                Ok((Resumed::Done(submitted), info)) => {
                    // A sign-up finished earlier on this device (its answer lost, the app closed): go on to the client.
                    this.instance = Some(info);
                    this.finish(submitted, cx);
                }
                Ok((Resumed::Revoked, info)) => {
                    this.instance = Some(info);
                    this.banner = Some(BannerState {
                        tone: BannerTone::Danger,
                        text: tf("onboarding.error.other", &[("code", "E_SESSION_REVOKED")]).into(),
                        reload: false,
                    });
                }
                Ok((Resumed::Fresh, info)) => {
                    let closed = info.registration_mode == RegistrationMode::Closed;
                    let required = info.registration_mode == RegistrationMode::InviteOnly;
                    this.instance = Some(info);
                    if closed {
                        this.go(Step::Closed, window, cx);
                    } else if required && code.is_empty() {
                        this.errors.invite = Some(t("onboarding.error.inviteRequired").into());
                        this.focus_first_error(window, cx);
                    } else {
                        this.go(Step::Identity, window, cx);
                    }
                }
                Err(e) => this.apply_refusal(&e, window, cx),
            },
            window,
            cx,
        );
    }

    fn identity_next(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        self.banner = None;
        let username = self.username.read(cx).value().to_string();
        let password = Zeroizing::new(self.password.read(cx).value().to_string());
        let password_signup = self.instance.as_ref().is_some_and(|i| i.password_signup);
        self.errors = FieldErrors::default();
        if !su::username_ok(&username) {
            self.errors.username = Some(t("onboarding.error.usernameRule").into());
        }
        if password_signup && !su::password_ok(&password) {
            self.errors.password = Some(t("onboarding.error.passwordShort").into());
        }
        if self.errors.username.is_some() || self.errors.password.is_some() {
            self.focus_first_error(window, cx);
            cx.notify();
            return;
        }
        let Some(flow) = self.flow.take() else {
            return self.go(Step::Connect, window, cx);
        };
        self.run(
            flow,
            |f, rt| rt.block_on(f.begin()),
            |this, result, window, cx| match result {
                Ok(key) => {
                    this.key = Some(key);
                    this.acknowledged = false;
                    this.go(Step::Keys, window, cx);
                }
                Err(e) => this.apply_refusal(&e, window, cx),
            },
            window,
            cx,
        );
    }

    fn keys_next(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        if !self.acknowledged || self.key.is_none() {
            return;
        }
        self.banner = None;
        let code = su::extract_invite_code(&self.invite.read(cx).value());
        let username = self.username.read(cx).value().to_string();
        let password_signup = self.instance.as_ref().is_some_and(|i| i.password_signup);
        let password = Zeroizing::new(self.password.read(cx).value().to_string());
        let Some(flow) = self.flow.take() else {
            return;
        };
        self.run(
            flow,
            move |f, rt| {
                let password = password_signup.then_some(password.as_str());
                let done = rt.block_on(f.submit(&code, &username, password));
                if let Err(e) = &done
                    && !matches!(e, SignupError::Stalled)
                {
                    // Any other refusal created nothing: this attempt's key is discarded.
                    let _ = f.reset();
                }
                done
            },
            |this, result, window, cx| match result {
                Ok(submitted) => {
                    this.password
                        .update(cx, |s, cx| s.set_value("", window, cx));
                    this.finish(submitted, cx);
                }
                Err(SignupError::Stalled) => this.apply_refusal(&SignupError::Stalled, window, cx),
                Err(e) => {
                    let (step, ..) = refusal_view(&e, &this.instance_name());
                    if step.is_none() {
                        this.go(Step::Identity, window, cx);
                    }
                    this.apply_refusal(&e, window, cx);
                }
            },
            window,
            cx,
        );
    }

    /// Reload, after a stalled creation: `resume` finishes the account (signup.ts `resume`), never a second POST.
    fn reload(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        if self.busy {
            return;
        }
        let Some(flow) = self.flow.take() else { return };
        self.run(
            flow,
            |f, rt| rt.block_on(f.resume()),
            |this, result, window, cx| match result {
                Ok(Resumed::Done(submitted)) => {
                    this.stalled = false;
                    this.banner = None;
                    this.finish(submitted, cx);
                }
                Ok(Resumed::Fresh) => {
                    // Nothing was created: back to the account step, with a new key next time.
                    this.stalled = false;
                    this.banner = None;
                    this.go(Step::Identity, window, cx);
                }
                Ok(Resumed::Revoked) => {
                    this.apply_refusal(&SignupError::Other("E_SESSION_REVOKED".into()), window, cx)
                }
                Err(e) => this.apply_refusal(&e, window, cx),
            },
            window,
            cx,
        );
    }

    fn finish(&mut self, submitted: Submitted, cx: &mut Context<Self>) {
        self.key = None;
        self.acknowledged = false;
        if let Some(flow) = self.flow.take() {
            let cfg = flow.config().clone();
            drop(flow);
            cx.emit(SignedUp { cfg, submitted });
        }
    }

    fn back(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        if self.blocked() {
            return;
        }
        self.banner = None;
        self.clear_password(window, cx);
        match self.step {
            Step::Identity => self.go(Step::Connect, window, cx),
            Step::Confirm => {
                self.confirming = None;
                self.go(Step::Connect, window, cx);
            }
            Step::Keys => {
                // The shown key is never offered again.
                if let Some(mut flow) = self.flow.take() {
                    let _ = flow.reset();
                    self.flow = Some(flow);
                }
                self.go(Step::Identity, window, cx);
            }
            Step::Connect | Step::Closed => {}
        }
    }

    fn clear_password(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        self.password
            .update(cx, |s, cx| s.set_value("", window, cx));
    }

    fn sign_in(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        if self.blocked() {
            return;
        }
        self.clear_password(window, cx);
        if self.step == Step::Keys
            && let Some(mut flow) = self.flow.take()
        {
            // A sign-in starts only from a device that holds no pending keys.
            let _ = flow.reset();
            self.flow = Some(flow);
        }
        self.key = None;
        cx.emit(WantsSignIn);
    }

    fn toggle_ack(&mut self, cx: &mut Context<Self>) {
        if !self.blocked() {
            self.acknowledged = !self.acknowledged;
            cx.notify();
        }
    }

    /// Save as file: the key once, in the file the person picks (recovery-file.ts), never anywhere else.
    fn save_key(&mut self, cx: &mut Context<Self>) {
        let Some(key) = &self.key else { return };
        let instance = self.instance_name();
        let file = su::recovery_key_file(
            tf(
                "onboarding.keys.fileName",
                &[("instance", &su::instance_slug(&instance))],
            ),
            &tf("onboarding.keys.fileBody", &[("instance", &instance)]),
            key,
        );
        let dir = std::env::home_dir()
            .map(|h| h.join("Downloads"))
            .filter(|d| d.is_dir())
            .or_else(std::env::home_dir)
            .unwrap_or_default();
        let chosen = cx.prompt_for_new_path(&dir, Some(&file.name));
        cx.spawn(async move |this, cx| {
            let Ok(Ok(Some(path))) = chosen.await else {
                return;
            };
            // Off the UI thread: a temporary file beside it (0600, never through a link), synced, renamed over it.
            let written = cx
                .background_spawn(async move { su::write_key_file(&path, file.text.as_bytes()) })
                .await;
            if written.is_err() {
                let _ = this.update(cx, |this, cx| {
                    this.banner = Some(BannerState {
                        tone: BannerTone::Danger,
                        text: t(SAVE_FAILED).into(),
                        reload: false,
                    });
                    cx.notify();
                });
            }
        })
        .detach();
    }

    fn primary_label(&self) -> String {
        match self.step {
            Step::Connect => t("onboarding.next").into(),
            Step::Identity => t("onboarding.identity.submit").into(),
            Step::Keys if self.busy => t("onboarding.keys.submitting").into(),
            Step::Keys => t("onboarding.keys.submit").into(),
            Step::Closed => String::new(),
            Step::Confirm => CONFIRM_CONNECT.into(),
        }
    }
}

/// Automation: `id` is there while its field shows a refusal (a probe over the field); nothing otherwise.
fn error_probe(id: &'static str, refused: bool) -> Option<AnyElement> {
    #[cfg(feature = "automation")]
    if refused {
        return Some(crate::automation::bounds_probe(id).into_any_element());
    }
    let _ = (id, refused);
    None
}

/// The refusal a failed "Save as file" shows. The web has no `onboarding.keys.saveFailed` (its download cannot fail
/// in the page), so the generated strings carry none yet: the settings' save failure says it until one is added.
const SAVE_FAILED: &str = "settings.editing.saveFailed";

impl Render for SignupView {
    fn render(&mut self, _window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let name = self.instance_name();
        let step_label = |n: u8| {
            tf(
                "onboarding.step",
                &[("n", &n.to_string()), ("total", &TOTAL.to_string())],
            )
        };
        let banner = self.banner.clone().map(|b| {
            let mut el = Banner::new("onboarding-banner", b.tone, b.text);
            if b.reload {
                el = el.action(
                    t("onboarding.error.reload"),
                    cx.listener(|this, _, window, cx| this.reload(window, cx)),
                );
            }
            tracked_box("onboarding-banner", el)
        });
        let sign_in = (self.instance.as_ref().is_none_or(|i| i.password_signup)).then(|| {
            tracked_focus(
                "onboarding-signin",
                &self.handles.sign_in,
                NavLink::new("onboarding-signin", t("onboarding.connect.signIn"))
                    .focus(&self.handles.sign_in)
                    .on_click(cx.listener(|this, _, window, cx| this.sign_in(window, cx))),
            )
            .into_any_element()
        });
        let back_label = if self.step == Step::Confirm {
            CONFIRM_CANCEL
        } else {
            t("onboarding.back")
        };
        let back = matches!(self.step, Step::Identity | Step::Keys | Step::Confirm).then(|| {
            tracked_focus(
                "onboarding-back",
                &self.handles.back,
                NavLink::new("onboarding-back", back_label)
                    .back()
                    .focus(&self.handles.back)
                    .on_click(cx.listener(|this, _, window, cx| this.back(window, cx))),
            )
            .into_any_element()
        });
        let ready = !self.blocked() && (self.step != Step::Keys || self.acknowledged);
        let primary = tracked_focus(
            "onboarding-next",
            &self.handles.next,
            PrimaryAction::new("onboarding-next", self.primary_label())
                .ready(ready)
                .focus(&self.handles.next)
                .on_click(cx.listener(|this, _, window, cx| this.submit(window, cx))),
        );
        let frame_el: AnyElement = match self.step {
            Step::Confirm => {
                let (host, instance) = self
                    .confirming
                    .as_ref()
                    .map(|l| {
                        (
                            l.instance.host_str().unwrap_or_default().to_owned()
                                + &l.instance
                                    .port()
                                    .map(|p| format!(":{p}"))
                                    .unwrap_or_default(),
                            l.instance.to_string(),
                        )
                    })
                    .unwrap_or_default();
                let mut f =
                    OnboardingFrame::new("onboarding", CONFIRM_TITLE.replace("{host}", &host))
                        .step(step_label(1))
                        .progress(1, TOTAL)
                        .child(frame::body_paragraph(
                            &CONFIRM_INSTANCE.replace("{instance}", &instance),
                            color::FG_2,
                        ))
                        .child(frame::body_paragraph(CONFIRM_DETAIL, color::FG_2));
                if let Some(b) = banner {
                    f = f.banner(b);
                }
                tracked("onboarding-confirm", f.primary(primary).nav(back, None)).into_any_element()
            }
            Step::Closed => OnboardingFrame::new(
                "onboarding",
                tf("onboarding.closed.title", &[("instance", &name)]),
            )
            .child(frame::body_paragraph(
                &tf("onboarding.closed.body", &[("instance", &name)]),
                color::FG_2,
            ))
            .nav(None, sign_in)
            .into_any_element(),
            Step::Connect => {
                let open = self
                    .instance
                    .as_ref()
                    .is_some_and(|i| i.registration_mode == RegistrationMode::Open);
                let body = if open {
                    tf("onboarding.connect.bodyOpen", &[("instance", &name)])
                } else {
                    t("onboarding.connect.body").to_owned()
                };
                let label = if open {
                    "onboarding.connect.inviteOptional"
                } else {
                    "onboarding.connect.invite"
                };
                let mut f = OnboardingFrame::new("onboarding", t("onboarding.connect.title"))
                    .step(step_label(1))
                    .progress(1, TOTAL)
                    .child(frame::body_paragraph(&body, color::FG_2));
                if let Some(b) = banner {
                    f = f.banner(b);
                }
                f.child(tracked_focus(
                    "onboarding-invite",
                    &self.invite.focus_handle(cx),
                    div()
                        .relative()
                        .child(
                            OnboardingField::new("onboarding-invite-field", t(label), &self.invite)
                                .error(self.errors.invite.clone()),
                        )
                        .children(error_probe(
                            "onboarding-invite-error",
                            self.errors.invite.is_some(),
                        )),
                ))
                .primary(primary)
                .nav(None, sign_in)
                .into_any_element()
            }
            Step::Identity => {
                let password_signup = self.instance.as_ref().is_none_or(|i| i.password_signup);
                let mut f = OnboardingFrame::new("onboarding", t("onboarding.identity.title"))
                    .step(step_label(2))
                    .progress(2, TOTAL)
                    .child(frame::body_paragraph(
                        t("onboarding.identity.body"),
                        color::FG_2,
                    ));
                if let Some(b) = banner {
                    f = f.banner(b);
                }
                f = f.child(tracked_focus(
                    "onboarding-username",
                    &self.username.focus_handle(cx),
                    div()
                        .relative()
                        .child(
                            OnboardingField::new(
                                "onboarding-username-field",
                                t("onboarding.identity.username"),
                                &self.username,
                            )
                            .error(self.errors.username.clone()),
                        )
                        .children(error_probe(
                            "onboarding-username-error",
                            self.errors.username.is_some(),
                        )),
                ));
                if password_signup {
                    f = f.child(tracked_focus(
                        "onboarding-password",
                        &self.password.focus_handle(cx),
                        div()
                            .relative()
                            .child(
                                OnboardingField::new(
                                    "onboarding-password-field",
                                    t("onboarding.identity.password"),
                                    &self.password,
                                )
                                .error(self.errors.password.clone()),
                            )
                            .children(error_probe(
                                "onboarding-password-error",
                                self.errors.password.is_some(),
                            )),
                    ));
                }
                f.primary(primary).nav(back, sign_in).into_any_element()
            }
            Step::Keys => {
                let groups: Vec<SharedString> = self
                    .key
                    .as_ref()
                    .map(|k| {
                        k.groups()
                            .map(|g| SharedString::from(g.to_owned()))
                            .collect()
                    })
                    .unwrap_or_default();
                let mut f = OnboardingFrame::new("onboarding", t("onboarding.keys.title"))
                    .step(step_label(3))
                    .progress(3, TOTAL)
                    .child(frame::body_paragraph(
                        t("onboarding.keys.body"),
                        color::FG_2,
                    ));
                if let Some(b) = banner {
                    f = f.banner(b);
                }
                let print = tracked_focus(
                    "onboarding-print",
                    &self.handles.print,
                    KeyAction::new(
                        "onboarding-print",
                        IconName::Printer,
                        t("onboarding.keys.print"),
                    )
                    .focus(&self.handles.print)
                    // Printing is not offered by the desktop yet: the press does nothing (reported to Jonas).
                    .on_click(|_, _, _| {}),
                );
                let save = tracked_focus(
                    "onboarding-save",
                    &self.handles.save,
                    KeyAction::new(
                        "onboarding-save",
                        IconName::Download,
                        t("onboarding.keys.save"),
                    )
                    .focus(&self.handles.save)
                    .on_click(cx.listener(|this, _, _, cx| this.save_key(cx))),
                );
                let ack = tracked_focus(
                    "onboarding-ack",
                    &self.handles.ack,
                    KeyAck::new(
                        "onboarding-ack",
                        t("onboarding.keys.acknowledge"),
                        self.acknowledged,
                    )
                    .focus(&self.handles.ack)
                    .on_click(cx.listener(|this, _, _, cx| this.toggle_ack(cx))),
                );
                f = f.child(RecoveryKeyLayout::new(
                    tracked_box("onboarding-key", KeyGroups::new(groups)),
                    print,
                    save,
                    ack,
                ));
                if self.busy {
                    f = f.child(frame::body_paragraph(
                        &tf("onboarding.keys.registering", &[("instance", &name)]),
                        color::FG_2,
                    ));
                }
                f.primary(primary).nav(back, sign_in).into_any_element()
            }
        };
        tracked(
            "onboarding",
            div()
                .id("onboarding-root")
                .key_context(super::keys::CONTEXT)
                .on_action(|_: &FocusNext, window, cx| window.focus_next(cx))
                .on_action(|_: &FocusPrevious, window, cx| window.focus_prev(cx))
                .on_action(
                    cx.listener(|this, _: &Submit, window, cx| this.submit_from_key(window, cx)),
                )
                .on_action(cx.listener(|this, _: &Back, window, cx| this.back(window, cx)))
                .size_full()
                .bg(color::BG)
                .child(frame_el),
        )
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_invite_link_names_the_instance() {
        assert_eq!(
            server_of(" https://dilla.test/i/AB12 ").map(|u| u.to_string()),
            Some("https://dilla.test/".into())
        );
        assert_eq!(
            server_of("http://127.0.0.1:8453/welcome?invite=x").map(|u| u.to_string()),
            Some("http://127.0.0.1:8453/".into())
        );
        assert_eq!(server_of("AB12"), None);
        assert_eq!(server_of("javascript:alert(1)"), None);
    }

    #[test]
    fn refusals_land_on_their_step() {
        assert_eq!(
            refusal_view(&SignupError::InviteInvalid, "x").0,
            Some(Step::Connect)
        );
        assert!(
            refusal_view(&SignupError::InviteInvalid, "x")
                .1
                .invite
                .is_some()
        );
        assert_eq!(
            refusal_view(&SignupError::UsernameTaken, "x").0,
            Some(Step::Identity)
        );
        assert_eq!(
            refusal_view(&SignupError::Closed, "x").0,
            Some(Step::Closed)
        );
        assert!(
            refusal_view(&SignupError::Stalled, "x")
                .2
                .is_some_and(|b| b.reload)
        );
        let (step, _, banner) = refusal_view(&SignupError::Other("E_TEAPOT".into()), "x");
        assert_eq!(step, None);
        assert!(banner.unwrap().text.contains("E_TEAPOT"));
    }
}
