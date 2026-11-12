//! The main window's root: which screen shows (sign-in, starting, the shell), and the running [`Client`] behind the
//! shell. `Client::start` blocks until the device session stands, so it runs on a thread of its own; its state (a
//! tokio watch) is forwarded into the shell on the UI thread.

use std::path::PathBuf;
use std::rc::Rc;
use std::sync::Arc;

use dilla_client::instances::{Instances, default_root};
use dilla_client::keychain::OsKeychain;
use dilla_client::seams::{InstanceConfig, InstanceId, Keychain};
use dilla_client::{Client, ClientError};
use gpui::{
    AppContext, Context, Entity, IntoElement, ParentElement, Render, Styled, Subscription, Window,
    div,
};

use dilla_desktop_ui::tokens::{color, text};

use crate::screens::shell::{Clock, ShellView};
use crate::screens::sign_in::{SignInView, SignedIn};
use crate::tracked;
use dilla_desktop_ui::text::line;

/// Where the app keeps its instances and their keys, or the fixture it draws instead.
#[derive(Clone)]
pub struct Launch {
    /// The data root (`<OS data dir>/Dilla Preview`); `None` when the OS has none.
    pub root: Option<PathBuf>,
    pub keychain: Arc<dyn Keychain>,
    /// Start the first listed instance's client at launch (a device already signed in).
    pub resume: bool,
    /// The instance this window runs (`windows::open_instance`); `None`: the first listed instance.
    pub instance: Option<InstanceId>,
    /// The code of an invite the person pasted into the manager: the window's welcome and join flow starts from it (the
    /// GPUI side of the shell's `/welcome?invite=<code>`). Read through [`AppView::pending_invite`].
    pub invite: Option<String>,
    /// The window's size, logical px.
    pub window: (f32, f32),
    #[cfg(feature = "automation")]
    pub fixture: Option<crate::fixture::Fixture>,
}

impl Launch {
    /// The shipping app: the OS data dir and the OS keychain (service `dev.dilla.desktop.preview`).
    pub fn os() -> Self {
        Self::live(default_root().ok(), Arc::new(OsKeychain::new()))
    }

    /// A data root and a keychain of the caller's.
    pub fn live(root: Option<PathBuf>, keychain: Arc<dyn Keychain>) -> Self {
        Self {
            root,
            keychain,
            resume: true,
            instance: None,
            invite: None,
            window: (1280.0, 800.0),
            #[cfg(feature = "automation")]
            fixture: None,
        }
    }

    /// No data root at all: the sign-in screen, which cannot sign in (window tests).
    pub fn empty() -> Self {
        Self {
            root: None,
            keychain: Arc::new(dilla_client::keychain::MemoryKeychain::new()),
            resume: false,
            instance: None,
            invite: None,
            window: (1280.0, 800.0),
            #[cfg(feature = "automation")]
            fixture: None,
        }
    }

    /// The shell drawn from a fixture, without a network.
    #[cfg(feature = "automation")]
    pub fn fixture(fixture: crate::fixture::Fixture) -> Self {
        Self {
            window: fixture.window,
            fixture: Some(fixture),
            ..Self::empty()
        }
    }
}

enum Screen {
    Starting,
    SignIn(Entity<SignInView>),
    Shell(Entity<ShellView>),
}

/// The main window's root view.
pub struct AppView {
    launch: Launch,
    screen: Screen,
    /// The instance whose client this window shows (set when it starts).
    instance: Option<InstanceId>,
    _subscriptions: Vec<Subscription>,
}

impl AppView {
    pub fn new(launch: Launch, window: &mut Window, cx: &mut Context<Self>) -> Self {
        let mut this = Self {
            launch,
            screen: Screen::Starting,
            instance: None,
            _subscriptions: Vec::new(),
        };
        #[cfg(feature = "automation")]
        if let Some(fixture) = this.launch.fixture.clone() {
            // The route decides the screen (`fixture::screen_of`): the fixture loader already refused any route the app
            // cannot draw, so this match is exhaustive over what it can.
            match fixture.screen {
                crate::fixture::FixtureScreen::Shell { channel } => {
                    let clock = Clock::Fixed {
                        now: fixture.now,
                        utc: true,
                    };
                    let shell = cx.new(|cx| {
                        ShellView::new(
                            Arc::new(fixture.state),
                            None,
                            Some(channel),
                            clock,
                            window,
                            cx,
                        )
                    });
                    this.screen = Screen::Shell(shell);
                }
                crate::fixture::FixtureScreen::SignIn => this.show_sign_in(None, window, cx),
            }
            return this;
        }
        let existing = this
            .launch
            .root
            .as_deref()
            .filter(|_| this.launch.resume)
            .and_then(|root| Instances::load(root).ok())
            .and_then(|list| match this.launch.instance {
                Some(id) => list.get(id).cloned(),
                None => list.list().first().cloned(),
            });
        match existing {
            Some(cfg) => this.start_client(cfg, window, cx),
            None => this.show_sign_in(None, window, cx),
        }
        this
    }

    /// The invite code this window was opened with, until the welcome and join flow takes it.
    pub fn pending_invite(&self) -> Option<&str> {
        self.launch.invite.as_deref()
    }

    /// The instance this window runs, once it has one.
    pub fn instance(&self) -> Option<InstanceId> {
        self.instance
    }

    fn show_sign_in(&mut self, error: Option<String>, window: &mut Window, cx: &mut Context<Self>) {
        let (root, keychain) = (self.launch.root.clone(), self.launch.keychain.clone());
        let view = cx.new(|cx| SignInView::new(root, keychain, error, window, cx));
        self._subscriptions = vec![cx.subscribe_in(
            &view,
            window,
            |this, _, SignedIn(cfg): &SignedIn, window, cx| {
                this.start_client(cfg.clone(), window, cx);
            },
        )];
        self.screen = Screen::SignIn(view);
        cx.notify();
    }

    fn start_client(&mut self, cfg: InstanceConfig, window: &mut Window, cx: &mut Context<Self>) {
        // An instance window runs its own instance only: a sign-in that ended on another one (the sign-in view takes
        // any server address) is refused here, so this window never starts, or holds, a different instance.
        if self.launch.instance.is_some_and(|pinned| pinned != cfg.id) {
            let wrong = crate::screens::instances::copy::ERROR_WRONG_INSTANCE;
            return self.show_sign_in(Some(wrong.into()), window, cx);
        }
        self.instance = Some(cfg.id);
        // A window opened again on an instance whose client kept running shows that client (windows.rs).
        if let Some(client) = crate::windows::running_client(cx, cfg.id) {
            return self.started(Ok(client), window, cx);
        }
        self.screen = Screen::Starting;
        cx.notify();
        // Another window's start of this instance is running: wait for it instead of starting a second client.
        let crate::windows::Starting::Go(token) = crate::windows::mark_starting(cx, cfg.id) else {
            let id = cfg.id;
            cx.spawn_in(window, async move |this, cx| {
                loop {
                    cx.background_executor()
                        .timer(std::time::Duration::from_millis(100))
                        .await;
                    let done = this.update_in(cx, |this, window, cx| {
                        let over = !crate::windows::is_starting(cx, id);
                        if over {
                            this.start_client(cfg.clone(), window, cx);
                        }
                        over
                    });
                    if done.unwrap_or(true) {
                        break;
                    }
                }
            })
            .detach();
            return;
        };
        let keychain = self.launch.keychain.clone();
        let (tx, rx) = tokio::sync::oneshot::channel();
        std::thread::Builder::new()
            .name("dilla-client-start".into())
            .spawn(move || {
                let _ = tx.send(Client::start(cfg, keychain));
            })
            .expect("a thread starts");
        cx.spawn_in(window, async move |this, cx| {
            // Held until this start ends, or the window goes (a released instance waits on it).
            let _token = token;
            let result = rx
                .await
                .unwrap_or_else(|_| Err(ClientError::Runtime("the start thread ended".into())));
            let _ = this.update_in(cx, |this, window, cx| {
                this.started(result.map(Rc::new), window, cx)
            });
        })
        .detach();
    }

    fn started(
        &mut self,
        result: Result<Rc<Client>, ClientError>,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let client = match result {
            Ok(client) => client,
            Err(e) => {
                if let Some(id) = self.instance {
                    crate::windows::finish_starting(cx, id);
                }
                return match e {
                    ClientError::NoIdentity => self.show_sign_in(None, window, cx),
                    // A key lost from the keychain (an interrupted removal, a reset keychain): say what to do.
                    ClientError::Store(m) if m.contains("key is missing") => self.show_sign_in(
                        Some(crate::screens::instances::copy::KEY_MISSING.into()),
                        window,
                        cx,
                    ),
                    e => self.show_sign_in(Some(e.to_string()), window, cx),
                };
            }
        };
        if let Some(id) = self.instance {
            crate::windows::keep_client(cx, id, client.clone());
        }
        let mut states = client.state();
        let first = states.borrow_and_update().clone();
        let shell = cx
            .new(|cx| ShellView::new(first, Some(client.clone()), None, Clock::Local, window, cx));
        let weak = shell.downgrade();
        cx.spawn(async move |_, cx| {
            while states.changed().await.is_ok() {
                let next = states.borrow_and_update().clone();
                if weak
                    .update(cx, |shell, cx| shell.set_state(next, cx))
                    .is_err()
                {
                    break;
                }
            }
        })
        .detach();
        self.screen = Screen::Shell(shell);
        cx.notify();
    }
}

impl Render for AppView {
    fn render(&mut self, _window: &mut Window, _cx: &mut Context<Self>) -> impl IntoElement {
        let body = match &self.screen {
            Screen::Starting => div()
                .size_full()
                .bg(color::BG)
                .flex()
                .items_center()
                .justify_center()
                .child(line("Opening dilla…", text::SM, 18.75, color::FG_3))
                .into_any_element(),
            Screen::SignIn(view) => view.clone().into_any_element(),
            Screen::Shell(view) => view.clone().into_any_element(),
        };
        tracked("root", div().size_full().bg(color::BG).child(body))
    }
}
