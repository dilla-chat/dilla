//! The main window's root: which screen shows (sign-in, starting, the shell), and the running [`Client`] behind the
//! shell. `Client::start` blocks until the device session stands, so it runs on a thread of its own; its state (a
//! tokio watch) is forwarded into the shell on the UI thread.
//!
//! **The routing table.** [`Screen`] is the one list of what the window can show, and [`Screen::mode`] says which window
//! shape each needs (the start window, the compact authentication window, the workspace; `screens/shell/window.rs`). A
//! screen of another area is added in three places, each marked `// ROUTE:` below: a variant of [`Screen`] (with its
//! mode), the `show_*` method that builds it, and its arm in `render`. The start and authentication windows resize in
//! place; the workspace has another minimum size, so the window is opened again in that mode and the client is handed
//! over ([`Launch::handoff`]).

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

use dilla_desktop_ui::tokens::color;

use crate::screens::boot;
use crate::screens::shell::window::{SavedBounds, WindowMode, window_shape};
use crate::screens::shell::{Clock, ShellView};
use crate::screens::sign_in::{SignInView, SignedIn};
use crate::tracked;

/// Where the app keeps its instances and their keys, or the fixture it draws instead.
#[derive(Clone)]
pub struct Launch {
    /// The data root (`<OS data dir>/Dilla Preview`); `None` when the OS has none.
    pub root: Option<PathBuf>,
    pub keychain: Arc<dyn Keychain>,
    /// Start the first listed instance's client at launch (a device already signed in).
    pub resume: bool,
    /// The window's size, logical px (the workspace's, when the window opens in it).
    pub window: (f32, f32),
    /// The shipping window: it opens in the start shape (400x280), takes the authentication shape for the sign-in and
    /// the workspace shape for the shell, and draws the window's own controls. Tests and fixtures draw at `window`.
    pub shipping: bool,
    /// A client that is already running (the start or authentication window handing over to the workspace window).
    pub handoff: Option<Rc<Client>>,
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
            shipping: true,
            handoff: None,
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
            shipping: false,
            handoff: None,
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

/// What the window shows. // ROUTE: a screen of another area is a variant here.
enum Screen {
    Starting,
    /// `Client::start` failed with something sign-in cannot answer: the fatal splash, with a retry (Task 24).
    Fatal(ClientError, InstanceConfig),
    SignIn(Entity<SignInView>),
    Shell(Entity<ShellView>),
}

impl Screen {
    /// The window shape the screen needs.
    fn mode(&self) -> WindowMode {
        match self {
            Screen::Starting | Screen::Fatal(..) => WindowMode::Starting,
            Screen::SignIn(_) => WindowMode::Authentication,
            Screen::Shell(_) => WindowMode::Workspace,
        }
    }
}

/// The main window's root view.
pub struct AppView {
    launch: Launch,
    screen: Screen,
    /// The workspace window's bounds are kept a moment after the last change.
    bounds_task: Option<gpui::Task<()>>,
    bounds_sub: Option<Subscription>,
    _subscriptions: Vec<Subscription>,
}

impl AppView {
    pub fn new(launch: Launch, window: &mut Window, cx: &mut Context<Self>) -> Self {
        let mut this = Self {
            launch,
            screen: Screen::Starting,
            bounds_task: None,
            bounds_sub: None,
            _subscriptions: Vec::new(),
        };
        if this.launch.shipping {
            this.keep_bounds(window, cx);
        }
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
                    this.focus_shell(&shell, window, cx);
                    this.screen = Screen::Shell(shell);
                }
                crate::fixture::FixtureScreen::ShellHome { community } => {
                    // The server's first text channel, by the list's own order.
                    let channel = crate::screens::channel_list::ordered(&fixture.state, community)
                        .into_iter()
                        .find(|c| c.is_text())
                        .map(|c| c.id);
                    let clock = Clock::Fixed {
                        now: fixture.now,
                        utc: true,
                    };
                    let shell = cx.new(|cx| {
                        ShellView::new(Arc::new(fixture.state), None, channel, clock, window, cx)
                    });
                    this.focus_shell(&shell, window, cx);
                    this.screen = Screen::Shell(shell);
                }
                crate::fixture::FixtureScreen::SignIn => this.show_sign_in(None, window, cx),
            }
            return this;
        }
        if let Some(client) = this.launch.handoff.take() {
            this.show_shell(client, window, cx);
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
            None => this.show_sign_in(None, window, cx),
        }
        this
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
        self.fit_window(window, cx);
        cx.notify();
    }

    fn start_client(&mut self, cfg: InstanceConfig, window: &mut Window, cx: &mut Context<Self>) {
        self.screen = Screen::Starting;
        self.fit_window(window, cx);
        cx.notify();
        let retry = cfg.clone();
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
            let _ = this.update_in(cx, |this, window, cx| {
                this.started(retry, result, window, cx);
            });
        })
        .detach();
    }

    fn started(
        &mut self,
        cfg: InstanceConfig,
        result: Result<Client, ClientError>,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let client = match result {
            Ok(client) => Rc::new(client),
            Err(ClientError::NoIdentity) => return self.show_sign_in(None, window, cx),
            // A device the instance no longer knows is the sign-in's to answer (the web's `enterRevoked`); any other failed
            // start is a splash with its stable code (Task 24: `boot::classify`).
            Err(e) if boot::classify(&e) == boot::StartFailure::SignIn => {
                return self.show_sign_in(None, window, cx);
            }
            Err(e) => {
                self.screen = Screen::Fatal(e, cfg);
                self.fit_window(window, cx);
                cx.notify();
                return;
            }
        };
        // The Diagnostics section's instance fact and the connecting line's node: the server's host.
        if let Some(host) = cfg.server.host_str() {
            client.diagnostics().set_instance_name(host);
        }
        if self.launch.shipping && self.screen.mode().is_compact() {
            return self.open_workspace(client, window, cx);
        }
        self.show_shell(client, window, cx);
    }

    /// The workspace has its own minimum size, which a window keeps from the day it opens: the compact window hands the
    /// running client to a new window in the workspace shape and closes.
    fn open_workspace(&mut self, client: Rc<Client>, window: &mut Window, cx: &mut Context<Self>) {
        let mut launch = self.launch.clone();
        launch.handoff = Some(client);
        match crate::open_window_in(cx, launch, WindowMode::Workspace) {
            Ok(_) => window.remove_window(),
            // The workspace window did not open: this one stays, on the sign-in.
            Err(e) => self.show_sign_in(Some(e.to_string()), window, cx),
        }
    }

    // ROUTE: the shell, for a running client.
    fn show_shell(&mut self, client: Rc<Client>, window: &mut Window, cx: &mut Context<Self>) {
        let mut states = client.state();
        let first = states.borrow_and_update().clone();
        let controls = self.launch.shipping;
        let shell = cx.new(|cx| {
            ShellView::new(first, Some(client.clone()), None, Clock::Local, window, cx)
                .with_controls(controls)
        });
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
        self.focus_shell(&shell, window, cx);
        self.screen = Screen::Shell(shell);
        self.fit_window(window, cx);
        cx.notify();
    }

    /// The keyboard works from the first frame: the shell's root holds the focus, so Tab and the shell's keys reach it.
    fn focus_shell(&self, shell: &Entity<ShellView>, window: &mut Window, cx: &mut Context<Self>) {
        let handle = gpui::Focusable::focus_handle(shell.read(cx), cx);
        window.focus(&handle, cx);
    }

    /// Gives a compact screen its window shape (the start and authentication windows resize in place).
    fn fit_window(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let mode = self.screen.mode();
        if !self.launch.shipping || !mode.is_compact() {
            return;
        }
        let shape = window_shape(mode, None, crate::work_area_height(cx));
        window.resize(gpui::size(
            gpui::Pixels::from(shape.width),
            gpui::Pixels::from(shape.height),
        ));
    }

    /// Keeps the workspace window's bounds in the app's preferences, a moment after the last move or resize.
    fn keep_bounds(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let Some(root) = self.launch.root.clone() else {
            return;
        };
        let close_root = root.clone();
        let subscription = cx.observe_window_bounds(window, move |this, window, cx| {
            if this.screen.mode() != WindowMode::Workspace || window.is_maximized() {
                return;
            }
            let bounds = window.bounds();
            let root = root.clone();
            this.bounds_task = Some(cx.spawn(async move |_, cx| {
                cx.background_executor()
                    .timer(std::time::Duration::from_millis(400))
                    .await;
                SavedBounds {
                    x: f32::from(bounds.origin.x),
                    y: f32::from(bounds.origin.y),
                    width: f32::from(bounds.size.width),
                    height: f32::from(bounds.size.height),
                }
                .write(&dilla_client::prefs::Prefs::app(&root));
            }));
        });
        self.bounds_sub = Some(subscription);
        // The last bounds are kept at once when the window closes, not only after the debounce.
        window.on_window_should_close(cx, move |window, _| {
            if window.is_resizable() && !window.is_maximized() {
                let b = window.bounds();
                SavedBounds {
                    x: f32::from(b.origin.x),
                    y: f32::from(b.origin.y),
                    width: f32::from(b.size.width),
                    height: f32::from(b.size.height),
                }
                .write(&dilla_client::prefs::Prefs::app(&close_root));
            }
            true
        });
    }
}

impl Render for AppView {
    fn render(&mut self, window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let body = match &self.screen {
            Screen::Starting => boot::loading(),
            Screen::Fatal(error, cfg) => {
                let (this, cfg) = (cx.entity().downgrade(), cfg.clone());
                let (retry, sign_in) = (this.clone(), this.clone());
                boot::fatal(
                    error,
                    move |window, cx| {
                        let _ =
                            retry.update(cx, |this, cx| this.start_client(cfg.clone(), window, cx));
                    },
                    // "Switch instance": back to the sign-in screen, which lists the instances.
                    move |window, cx| {
                        let _ = sign_in.update(cx, |this, cx| this.show_sign_in(None, window, cx));
                    },
                )
            }
            Screen::SignIn(view) => view.clone().into_any_element(),
            Screen::Shell(view) => view.clone().into_any_element(),
        };
        // A window that draws its own frame is resized from strips at its rim.
        let size = window.viewport_size();
        let strips = (self.launch.shipping
            && window.is_resizable()
            && !window.is_maximized()
            && matches!(
                window.window_decorations(),
                gpui::Decorations::Client { .. }
            ))
        .then(|| {
            crate::screens::shell::resize::render(f32::from(size.width), f32::from(size.height))
        });
        tracked(
            "root",
            div()
                .size_full()
                .bg(color::BG)
                .child(body)
                .children(strips.into_iter().flatten()),
        )
    }
}
