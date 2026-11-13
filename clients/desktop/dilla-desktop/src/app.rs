//! The main window's root: which screen shows (sign-in, starting, the shell), and the running [`Client`] behind the
//! shell. `Client::start` blocks until the device session stands, so it runs on a thread of its own; its state (a
//! tokio watch) is forwarded into the shell on the UI thread.

use std::path::PathBuf;
use std::rc::Rc;
use std::sync::Arc;

use dilla_client::instances::{Instances, default_root};
use dilla_client::keychain::OsKeychain;
use dilla_client::seams::{InstanceConfig, Keychain};
use dilla_client::{Client, ClientError};
use gpui::{
    AppContext, Context, Entity, IntoElement, ParentElement, Render, Styled, Subscription, Window,
    div,
};

use dilla_desktop_ui::tokens::{color, text};

use dilla_client::features::signup::SignupResult;
use dilla_client::state::AccountPhase;

use crate::screens::create::Navigate;
use crate::screens::onboarding::{DoneView, OpenDilla, SignedUp, SignupView, WantsSignIn};
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
    /// The sign-up's steps 1 to 3 (Task 25).
    Signup(Entity<SignupView>),
    /// The sign-up's step 6, over the started client (Task 25).
    Done(Entity<DoneView>),
    Shell(Entity<ShellView>),
}

/// The main window's root view.
pub struct AppView {
    launch: Launch,
    screen: Screen,
    /// The client about to start is a new account's: once its factor is in place, the invite's server is joined and
    /// the done step shows (Task 25).
    after_signup: bool,
    shell: Option<Entity<ShellView>>,
    _subscriptions: Vec<Subscription>,
}

impl AppView {
    pub fn new(launch: Launch, window: &mut Window, cx: &mut Context<Self>) -> Self {
        let mut this = Self {
            launch,
            screen: Screen::Starting,
            after_signup: false,
            shell: None,
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
            .and_then(|list| list.list().first().cloned());
        match existing {
            Some(cfg) => this.start_client(cfg, window, cx),
            // A device with no account opens on the sign-up, as the web's /welcome does; its link leads to sign-in.
            None => this.show_signup(window, cx),
        }
        this
    }

    /// The sign-up (Task 25). Task 27's pasted invite (`Launch.invite`) is handed in here once it lands.
    fn show_signup(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let (root, keychain) = (self.launch.root.clone(), self.launch.keychain.clone());
        let view = cx.new(|cx| SignupView::new(root, keychain, None, window, cx));
        self._subscriptions = vec![
            cx.subscribe_in(&view, window, |this, _, done: &SignedUp, window, cx| {
                this.after_signup = true;
                this.start_client(done.cfg.clone(), window, cx);
            }),
            cx.subscribe_in(&view, window, |this, _, _: &WantsSignIn, window, cx| {
                this.show_sign_in(None, window, cx);
            }),
        ];
        self.screen = Screen::Signup(view);
        cx.notify();
    }

    /// The sign-up's done step, once the new account is ready and the invite's server joined (Task 25).
    fn show_done(
        &mut self,
        client: Rc<Client>,
        result: SignupResult,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let first = client.state().borrow().clone();
        let view = cx.new(|cx| {
            let mut done = DoneView::new(result, window, cx);
            done.set_state(&first, cx);
            done
        });
        let weak = view.downgrade();
        let mut states = client.state();
        cx.spawn(async move |_, cx| {
            while states.changed().await.is_ok() {
                let next = states.borrow_and_update().clone();
                if weak
                    .update(cx, |done, cx| done.set_state(&next, cx))
                    .is_err()
                {
                    break;
                }
            }
        })
        .detach();
        self._subscriptions =
            vec![
                cx.subscribe_in(&view, window, |this, _, open: &OpenDilla, _, cx| {
                    if let Some(shell) = this.shell.clone() {
                        if let Some(community) = open.result.community {
                            shell.update(cx, |s, cx| s.pick_community(community, cx));
                        }
                        this.screen = Screen::Shell(shell);
                        cx.notify();
                    }
                }),
            ];
        self.screen = Screen::Done(view);
        cx.notify();
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
        self.screen = Screen::Starting;
        cx.notify();
        let keychain = self.launch.keychain.clone();
        let (tx, rx) = tokio::sync::oneshot::channel();
        std::thread::Builder::new()
            .name("dilla-client-start".into())
            .spawn(move || {
                let _ = tx.send(Client::start(cfg, keychain));
            })
            .expect("a thread starts");
        cx.spawn_in(window, async move |this, cx| {
            let result = rx
                .await
                .unwrap_or_else(|_| Err(ClientError::Runtime("the start thread ended".into())));
            let _ = this.update_in(cx, |this, window, cx| this.started(result, window, cx));
        })
        .detach();
    }

    fn started(
        &mut self,
        result: Result<Client, ClientError>,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let client = match result {
            Ok(client) => Rc::new(client),
            Err(ClientError::NoIdentity) => return self.show_signup(window, cx),
            Err(e) => return self.show_sign_in(Some(e.to_string()), window, cx),
        };
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
        // Task 25: the create, join and invite dialogs act on this client and move the shell to what they made.
        let to_shell = shell.downgrade();
        crate::screens::create::attach(
            window,
            client.clone(),
            Rc::new(move |to: Navigate, _: &mut Window, cx: &mut gpui::App| {
                let _ = to_shell.update(cx, |s, cx| {
                    s.pick_community(to.community, cx);
                    if let Some(channel) = to.channel {
                        s.select(channel, cx);
                    }
                });
            }),
            cx,
        );
        self.shell = Some(shell.clone());
        self.screen = Screen::Shell(shell);
        if std::mem::take(&mut self.after_signup) {
            // A new account: once its factor is in place (Task 26's screens) it is ready, the invite's server is joined
            // (a refused join is the result, not a failure) and the done step shows.
            let mut ready = client.state();
            let client = client.clone();
            cx.spawn_in(window, async move |this, cx| {
                loop {
                    let phase = ready.borrow_and_update().account.phase;
                    if phase == AccountPhase::Ready {
                        break;
                    }
                    if ready.changed().await.is_err() {
                        return;
                    }
                }
                let result = client.onboarding().finish_signup().await;
                client.refresh();
                let _ = this.update_in(cx, |this, window, cx| {
                    this.show_done(client, result, window, cx)
                });
            })
            .detach();
        }
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
            Screen::Signup(view) => view.clone().into_any_element(),
            Screen::Done(view) => view.clone().into_any_element(),
            Screen::Shell(view) => view.clone().into_any_element(),
        };
        tracked("root", div().size_full().bg(color::BG).child(body))
    }
}
