//! Signing in on this device (web `screens/SignIn.tsx`, Signal onboarding frame): the instance's address with the
//! username and password, the authenticator code, the recovery key that adds this device, then "Open dilla". The
//! protocol is `dilla_client::signin` (enrol.ts); each step runs on a thread of its own with a small runtime, so the
//! window never waits on the network.
//!
//! Against the web screen: the desktop asks for the server's address on the first step (a browser is already on its
//! instance), the code is one six-digit field instead of six boxes, and a passkey or backup code is not offered yet.

use std::path::PathBuf;
use std::sync::Arc;

use dilla_client::instances::Instances;
use dilla_client::seams::{InstanceConfig, Keychain};
use dilla_client::signin::{Owed, SignIn, SignInError};
use gpui::{
    AppContext as _, ClickEvent, Context, Entity, EventEmitter, InteractiveElement, IntoElement,
    ParentElement, Render, SharedString, StatefulInteractiveElement, Styled, Subscription, Window,
    div, px,
};
use gpui_component::input::{Input, InputEvent, InputState};

use dilla_desktop_ui::tokens::{color, font, radius, text};
use dilla_desktop_ui::{Icon, IconName};

use crate::screens::{FocusNext, FocusPrevious};
use crate::tracked_box;
use dilla_desktop_ui::text::line;

/// The device is signed in: the instance to start the client for.
pub struct SignedIn(pub InstanceConfig);

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum Step {
    Login,
    Code,
    Key,
    Done,
}

impl Step {
    fn number(self) -> u8 {
        match self {
            Self::Login => 1,
            Self::Code => 2,
            Self::Key => 3,
            Self::Done => 4,
        }
    }
}

pub struct SignInView {
    root: Option<PathBuf>,
    keychain: Arc<dyn Keychain>,
    step: Step,
    busy: bool,
    error: Option<SharedString>,
    server: Entity<InputState>,
    username: Entity<InputState>,
    password: Entity<InputState>,
    code: Entity<InputState>,
    key: Entity<InputState>,
    flow: Option<SignIn>,
    signed_in: Option<InstanceConfig>,
    _subscriptions: Vec<Subscription>,
}

impl EventEmitter<SignedIn> for SignInView {}

/// The web screen's words for a failed step (`signin.error.*`).
fn message(e: &SignInError) -> String {
    match e {
        SignInError::LoginFailed => "That username and password did not work.".into(),
        SignInError::CodeFailed => {
            "That code did not work. Sign in again with a fresh code.".into()
        }
        SignInError::WrongKey => {
            "This is not the recovery key of this account. Check every character.".into()
        }
        SignInError::KeyLength => "A recovery key has 52 characters.".into(),
        SignInError::NoBackup => "This account has no recovery data on this instance.".into(),
        SignInError::ListRace => {
            "The account’s devices changed while you were signing in. Sign in again.".into()
        }
        SignInError::Evicted => "Someone else is signing in to this account.".into(),
        SignInError::TooMany => "Too many sign-in attempts. Try again in a minute.".into(),
        SignInError::Network(_) => "The connection dropped. Check it and try again.".into(),
        SignInError::PasskeyOnly => "This account signs in with a passkey.".into(),
        other => format!("Signing in did not work ({other}). Try again."),
    }
}

impl SignInView {
    pub fn new(
        root: Option<PathBuf>,
        keychain: Arc<dyn Keychain>,
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
        let server = field("https://dilla.example", false, window, cx);
        let username = field("", false, window, cx);
        let password = field("", true, window, cx);
        let code = field("123456", false, window, cx);
        let key = field("XXXX XXXX XXXX …", false, window, cx);
        let mut subscriptions = Vec::new();
        for input in [&server, &username, &password, &code, &key] {
            subscriptions.push(cx.subscribe_in(
                input,
                window,
                |this: &mut Self, _, event: &InputEvent, window, cx| {
                    if matches!(event, InputEvent::PressEnter { .. }) {
                        this.submit(window, cx);
                    }
                },
            ));
        }
        server.update(cx, |s, cx| s.focus(window, cx));
        Self {
            root,
            keychain,
            step: Step::Login,
            busy: false,
            error: error.map(Into::into),
            server,
            username,
            password,
            code,
            key,
            flow: None,
            signed_in: None,
            _subscriptions: subscriptions,
        }
    }

    fn value(input: &Entity<InputState>, cx: &gpui::App) -> String {
        input.read(cx).value().trim().to_owned()
    }

    /// Runs one step of the flow off the UI thread and hands the flow back with its result.
    fn run<T: Send + 'static>(
        &mut self,
        flow: SignIn,
        step: impl FnOnce(&mut SignIn, &tokio::runtime::Runtime) -> Result<T, SignInError>
        + Send
        + 'static,
        then: impl FnOnce(&mut Self, Result<T, SignInError>, &mut Window, &mut Context<Self>) + 'static,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        self.busy = true;
        self.error = None;
        cx.notify();
        let (tx, rx) = tokio::sync::oneshot::channel();
        std::thread::Builder::new()
            .name("dilla-sign-in".into())
            .spawn(move || {
                let mut flow = flow;
                let result = tokio::runtime::Builder::new_current_thread()
                    .enable_all()
                    .build()
                    .map_err(|e| SignInError::Other(e.to_string()))
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

    fn fail(&mut self, e: &SignInError, cx: &mut Context<Self>) {
        self.error = Some(message(e).into());
        cx.notify();
    }

    fn submit(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        if self.busy {
            return;
        }
        match self.step {
            Step::Login => self.login(window, cx),
            Step::Code => {
                let code = Self::value(&self.code, cx);
                if code.len() != 6 || !code.bytes().all(|b| b.is_ascii_digit()) {
                    self.error = Some("Six digits from your authenticator app.".into());
                    cx.notify();
                    return;
                }
                let Some(flow) = self.flow.take() else { return };
                self.run(
                    flow,
                    move |f: &mut SignIn, rt: &tokio::runtime::Runtime| rt.block_on(f.code(&code)),
                    |this, result, window, cx| match result {
                        Ok(()) => this.go(Step::Key, window, cx),
                        Err(e) => {
                            // A refused code spent the login: start over, as the web does.
                            this.fail(&e, cx);
                            this.step = Step::Login;
                        }
                    },
                    window,
                    cx,
                );
            }
            Step::Key => {
                let key = zeroize::Zeroizing::new(self.key.read(cx).value().to_string());
                let Some(flow) = self.flow.take() else { return };
                self.run(
                    flow,
                    move |f: &mut SignIn, rt: &tokio::runtime::Runtime| {
                        rt.block_on(f.add_device(&key))
                    },
                    |this, result, window, cx| match result {
                        Ok(()) => {
                            this.signed_in = this.flow.as_ref().map(|f| f.config().clone());
                            this.go(Step::Done, window, cx);
                        }
                        Err(e) => this.fail(&e, cx),
                    },
                    window,
                    cx,
                );
                self.key.update(cx, |s, cx| s.set_value("", window, cx));
            }
            Step::Done => {
                if let Some(cfg) = self.signed_in.take() {
                    cx.emit(SignedIn(cfg));
                }
            }
        }
    }

    fn login(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let server = Self::value(&self.server, cx);
        let username = Self::value(&self.username, cx);
        let password = zeroize::Zeroizing::new(self.password.read(cx).value().to_string());
        if server.is_empty() || username.is_empty() || password.is_empty() {
            self.error = Some("Fill in this field.".into());
            cx.notify();
            return;
        }
        let Some(url) = url::Url::parse(&server)
            .ok()
            .or_else(|| url::Url::parse(&format!("https://{server}")).ok())
        else {
            self.error = Some("That is not a server address.".into());
            cx.notify();
            return;
        };
        let flow = match self.flow.take() {
            Some(flow) if flow.config().server == url => flow,
            _ => match self.new_flow(url) {
                Ok(flow) => flow,
                Err(e) => return self.fail(&e, cx),
            },
        };
        self.password
            .update(cx, |s, cx| s.set_value("", window, cx));
        self.run(
            flow,
            move |f: &mut SignIn, rt: &tokio::runtime::Runtime| {
                rt.block_on(f.login(&username, &password))
            },
            |this, result, window, cx| match result {
                Ok(Owed::Code) => this.go(Step::Code, window, cx),
                Ok(Owed::Nothing) => this.go(Step::Key, window, cx),
                Ok(Owed::Passkey) => this.fail(&SignInError::PasskeyOnly, cx),
                Err(e) => this.fail(&e, cx),
            },
            window,
            cx,
        );
    }

    /// The instance for `server`: the one already listed for it, else a new one.
    fn new_flow(&self, server: url::Url) -> Result<SignIn, SignInError> {
        let root = self
            .root
            .as_deref()
            .ok_or_else(|| SignInError::Store("this system has no data directory".into()))?;
        let mut instances = Instances::load(root)?;
        let cfg = match instances.list().iter().find(|c| c.server == server) {
            Some(cfg) => cfg.clone(),
            None => instances.add(server)?,
        };
        SignIn::new(cfg, self.keychain.clone())
    }

    fn go(&mut self, step: Step, window: &mut Window, cx: &mut Context<Self>) {
        self.step = step;
        self.error = None;
        match step {
            Step::Login => self.server.update(cx, |s, cx| s.focus(window, cx)),
            Step::Code => self.code.update(cx, |s, cx| s.focus(window, cx)),
            Step::Key => self.key.update(cx, |s, cx| s.focus(window, cx)),
            Step::Done => {}
        }
        cx.notify();
    }
}

fn label(text_: &str) -> impl IntoElement {
    div()
        .pb(px(6.))
        .child(line(text_.to_owned(), text::XS, 16.5, color::FG_2))
}

fn field(id: &'static str, caption: &str, input: &Entity<InputState>) -> impl IntoElement {
    div()
        .flex()
        .flex_col()
        .pb(px(14.))
        .child(label(caption))
        .child(tracked_box(
            id,
            div()
                .w(px(342.))
                .h(px(40.))
                .rounded(radius::MD)
                .bg(color::SURFACE_2)
                .border_1()
                .border_color(color::CONTROL_EDGE_FIELD)
                .px(px(12.))
                .flex()
                .items_center()
                .text_size(text::INPUT)
                .font_family(font::MONO)
                .text_color(color::FG)
                .child(Input::new(input).appearance(false)),
        ))
}

impl Render for SignInView {
    fn render(&mut self, _window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let (title, body): (&str, &str) = match self.step {
            Step::Login => ("Sign in to dilla", "Use your dilla username and password."),
            Step::Code => ("Enter your code", "Six digits from your authenticator app."),
            Step::Key => (
                "Add this device",
                "This device is new. Paste your recovery key to read your conversations here.",
            ),
            Step::Done => ("You’re in", "This device is now one of your devices."),
        };
        let (action, button_id): (&str, &'static str) = match self.step {
            Step::Login => ("Continue", "signin-continue"),
            Step::Code => ("Verify", "signin-verify"),
            Step::Key => ("Add device", "signin-add"),
            Step::Done => ("Open dilla", "signin-open"),
        };
        let action = if self.busy { "Working…" } else { action };
        let fields = match self.step {
            Step::Login => vec![
                field("signin-server", "Server", &self.server).into_any_element(),
                field("signin-username", "Username", &self.username).into_any_element(),
                field("signin-password", "Password", &self.password).into_any_element(),
            ],
            Step::Code => vec![field("signin-code", "Code", &self.code).into_any_element()],
            Step::Key => {
                vec![field("signin-key", "Recovery key", &self.key).into_any_element()]
            }
            Step::Done => Vec::new(),
        };
        let progress = (1..=4u8).map(|n| {
            let current = n == self.step.number();
            div()
                .w(px(if current { 18. } else { 6. }))
                .h(px(4.))
                .rounded(radius::PILL)
                .bg(if n <= self.step.number() {
                    color::ACCENT
                } else {
                    color::SURFACE_HI
                })
        });
        let button = div()
            .id(button_id)
            .w(px(342.))
            .h(px(40.))
            .rounded(radius::MD)
            .bg(color::ACCENT)
            .flex()
            .items_center()
            .justify_center()
            .gap(px(8.))
            .cursor_pointer()
            .on_click(cx.listener(|this, _: &ClickEvent, window, cx| this.submit(window, cx)))
            .child(
                line(action.to_owned(), text::INPUT, 19.5, color::ACCENT_INK)
                    .weight(font::WEIGHT_LABEL),
            )
            .child(
                Icon::new(IconName::ArrowRight)
                    .size(px(14.))
                    .color(color::ACCENT_INK),
            );
        let card = div()
            .w(px(400.))
            .p(px(28.))
            .bg(color::SURFACE)
            .border_1()
            .border_color(color::HAIRLINE_2)
            .rounded(radius::XXL)
            .flex()
            .flex_col()
            .child(
                div()
                    .flex()
                    .items_center()
                    .justify_between()
                    .pb(px(24.))
                    .child(
                        line(
                            format!("SIGN IN · STEP {} OF 4", self.step.number()),
                            text::MICRO,
                            15.0,
                            color::FG_3,
                        )
                        .tracking(0.08),
                    )
                    .child(div().flex().gap(px(4.)).children(progress)),
            )
            .child(line(title, text::TITLE, 28.5, color::FG).weight(font::WEIGHT_DISPLAY))
            .children(self.error.clone().map(|e| {
                div()
                    .my(px(8.))
                    .pl(px(12.))
                    .border_l_2()
                    .border_color(color::DANGER)
                    .text_size(text::UI)
                    .line_height(px(18.))
                    .font_family(font::MONO)
                    .text_color(color::FG)
                    .child(e)
            }))
            .child(
                div()
                    .pt(px(6.))
                    .pb(px(18.))
                    .text_size(text::UI)
                    .line_height(px(18.))
                    .font_family(font::MONO)
                    .text_color(color::FG_2)
                    .child(body),
            )
            .children(fields)
            .child(tracked_box(button_id, button));
        div()
            .id("signin")
            .on_action(|_: &FocusNext, window, cx| window.focus_next(cx))
            .on_action(|_: &FocusPrevious, window, cx| window.focus_prev(cx))
            .size_full()
            .bg(color::BG)
            .relative()
            .flex()
            .items_center()
            .justify_center()
            .child(
                div()
                    .absolute()
                    .left(px(24.))
                    .top(px(22.))
                    .flex()
                    .items_center()
                    .gap(px(8.))
                    .child(
                        div()
                            .size(px(20.))
                            .rounded(radius::SM)
                            .bg(color::BRAND)
                            .flex()
                            .items_center()
                            .justify_center()
                            .child(
                                line("D", text::UI, 12.0, color::BRAND_INK)
                                    .weight(font::WEIGHT_DISPLAY),
                            ),
                    )
                    .child(
                        line("DILLA", text::XS, 16.5, color::FG)
                            .weight(font::WEIGHT_DISPLAY)
                            .tracking(crate::screens::LABEL_TRACKING_WIDE),
                    ),
            )
            .child(card)
    }
}
