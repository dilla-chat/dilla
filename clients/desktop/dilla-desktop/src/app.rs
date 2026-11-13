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
use dilla_client::seams::{InstanceConfig, InstanceId, Keychain};
use dilla_client::{Client, ClientError};
use gpui::{
    AppContext, Context, Entity, InteractiveElement, IntoElement, ParentElement, Render, Styled,
    Subscription, Window, div,
};

use dilla_desktop_ui::tokens::color;

use crate::screens::boot;
use crate::screens::shell::window::{SavedBounds, WindowMode, window_shape};
use dilla_client::features::signup::SignupResult;
use dilla_client::state::AccountPhase;

use crate::router::{Route, UserSection};
use crate::screens::create::Navigate;
use crate::screens::onboarding::{DoneView, OpenDilla, SignedUp, SignupView, WantsSignIn};
use crate::screens::settings_host::{self, Dest, SettingsEvent, UserSettingsHost};
use crate::screens::shell::{Clock, OpenSettings, ShellView};
use crate::screens::sign_in::{CreateInstead, SignInView, SignedIn};
use crate::slots::SlotCx;
use crate::tracked;

/// Where the app keeps its instances and their keys, or the fixture it draws instead.
#[derive(Clone)]
pub struct Launch {
    /// The data root (`<OS data dir>/Dilla Preview`); `None` when the OS has none.
    pub root: Option<PathBuf>,
    pub keychain: Arc<dyn Keychain>,
    /// Start the first listed instance's client at launch (a device already signed in).
    pub resume: bool,
    /// The instance this window runs (`windows::open_instance`, Task 27); `None`: the first listed instance.
    pub instance: Option<InstanceId>,
    /// The code of an invite the person pasted into the manager: the window's welcome and join flow starts from it (the
    /// GPUI side of the shell's `/welcome?invite=<code>`). Read through [`AppView::pending_invite`].
    pub invite: Option<String>,
    /// The window's size, logical px (the workspace's, when the window opens in it).
    pub window: (f32, f32),
    /// The shipping window: it opens in the start shape (400x280), takes the authentication shape for the sign-in and
    /// the workspace shape for the shell, and draws the window's own controls. Tests and fixtures draw at `window`.
    pub shipping: bool,
    /// A client that is already running (the start or authentication window handing over to the workspace window).
    pub handoff: Option<Rc<Client>>,
    /// The handed-over client is a new account's from the sign-up (Task 25): the workspace window finishes its join and
    /// shows the done step.
    pub after_signup: bool,
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
            shipping: true,
            handoff: None,
            after_signup: false,
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
            shipping: false,
            handoff: None,
            after_signup: false,
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

/// Why a client's local data goes ([`AppView::wipe`]).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum Wipe {
    /// Forget this device: the call left and the session signed out first.
    Forget,
    /// The device signed itself out and removed itself (the server half is done).
    SignedOut,
}

/// What the window shows. // ROUTE: a screen of another area is a variant here.
enum Screen {
    Starting,
    /// `Client::start` failed with something sign-in cannot answer: the fatal splash, with a retry (Task 24).
    Fatal(ClientError, InstanceConfig),
    SignIn(Entity<SignInView>),
    /// The sign-up's steps 1 to 3 (Task 25).
    Signup(Entity<SignupView>),
    /// The sign-up's step 6, over the started client (Task 25).
    Done(Entity<DoneView>),
    Shell(Entity<ShellView>),
    /// Task 26: the account owes a second factor.
    Factor(Entity<crate::screens::factor::forced::ForcedFactorView>),
    /// The user settings over the screen they were opened from, which comes back on close.
    /// `behind` is authoritative for where Close goes: the host's own `from` is not used (it is `None`), and the
    /// route it emits on close is only the signal to take `behind` back. The host's subscription lives here, so it
    /// goes with the screen.
    Settings {
        host: Entity<UserSettingsHost>,
        behind: Box<Screen>,
        _subscription: Subscription,
    },
    /// A server's settings (Task 15's `ServerSettingsView`, the server settings host), over the screen they were opened
    /// from (none for a fixture), which comes back on close.
    ServerSettings {
        view: Entity<crate::screens::server_settings::ServerSettingsView>,
        behind: Option<Box<Screen>>,
        _subscription: Option<Subscription>,
    },
}

impl Screen {
    /// The window shape the screen needs.
    fn mode(&self) -> WindowMode {
        match self {
            Screen::Starting | Screen::Fatal(..) => WindowMode::Starting,
            Screen::SignIn(_) | Screen::Signup(_) | Screen::Factor(_) => WindowMode::Authentication,
            Screen::Shell(_) | Screen::Done(_) | Screen::ServerSettings { .. } => {
                WindowMode::Workspace
            }
            // Settings keeps the shape of the screen it was opened over.
            Screen::Settings { behind, .. } => behind.mode(),
        }
    }
}

/// The main window's root view.
pub struct AppView {
    launch: Launch,
    screen: Screen,
    /// The instance whose client this window shows (set when it starts; Task 27).
    instance: Option<InstanceId>,
    /// The workspace window's bounds are kept a moment after the last change.
    bounds_task: Option<gpui::Task<()>>,
    bounds_sub: Option<Subscription>,
    /// The client about to start is a new account's: once its factor is in place, the invite's server is joined and
    /// the done step shows (Task 25).
    after_signup: bool,
    shell: Option<Entity<ShellView>>,
    /// The shell's way to the user settings (`ShellView`'s `OpenSettings`): the host opens over it.
    shell_sub: Option<Subscription>,
    /// The window's root: it takes the focus when nothing else holds it, so every key reaches the root's handlers
    /// (push to talk, review I1).
    root_focus: gpui::FocusHandle,
    /// The window going inactive lets push to talk go at once (review B).
    _activation: Subscription,
    /// The focused control went away (a drawn frame no longer has it): push to talk lets go (review B, re-review B2).
    _focus_lost: Subscription,
    _subscriptions: Vec<Subscription>,
    /// Task 26: why the last Forget did not finish, for the forced screen the client comes back to.
    forget_note: Option<String>,
}

impl AppView {
    pub fn new(launch: Launch, window: &mut Window, cx: &mut Context<Self>) -> Self {
        let mut this = Self {
            instance: launch.instance,
            launch,
            screen: Screen::Starting,
            bounds_task: None,
            bounds_sub: None,
            after_signup: false,
            shell: None,
            shell_sub: None,
            root_focus: cx.focus_handle(),
            _focus_lost: cx.on_focus_lost(window, |this: &mut Self, window, cx| {
                let root = this.root_focus.clone();
                crate::screens::voice::focus_lost(
                    crate::screens::voice::wid(window),
                    &root,
                    window,
                    cx,
                );
            }),
            _activation: cx.observe_window_activation(window, |_, window, cx| {
                if !window.is_window_active() {
                    crate::screens::voice::window_deactivated(
                        crate::screens::voice::wid(window),
                        cx,
                    );
                }
            }),
            _subscriptions: Vec::new(),
            forget_note: None,
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
                    this.attach_shell(&shell, window, cx);
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
                    this.attach_shell(&shell, window, cx);
                    this.focus_shell(&shell, window, cx);
                    this.screen = Screen::Shell(shell);
                }
                crate::fixture::FixtureScreen::SignIn => this.show_sign_in(None, window, cx),
                crate::fixture::FixtureScreen::Settings { section } => {
                    let ctx = SlotCx {
                        state: Arc::new(fixture.state),
                        clock: Clock::Fixed {
                            now: fixture.now,
                            utc: true,
                        },
                        ..SlotCx::default()
                    };
                    this.show_settings(Some(section), ctx, window, cx);
                }
                crate::fixture::FixtureScreen::ServerSettings { community, section } => {
                    let clock = Clock::Fixed {
                        now: fixture.now,
                        utc: true,
                    };
                    let view = cx.new(|cx| {
                        crate::screens::server_settings::ServerSettingsView::new(
                            community,
                            Arc::new(fixture.state),
                            None,
                            clock,
                            section,
                            window,
                            cx,
                        )
                    });
                    this.screen = Screen::ServerSettings {
                        view,
                        behind: None,
                        _subscription: None,
                    };
                }
            }
            return this;
        }
        if let Some(client) = this.launch.handoff.take() {
            this.after_signup = std::mem::take(&mut this.launch.after_signup);
            // Task 26: the factor area follows the client into the workspace window.
            crate::screens::factor::register_window_client(&client, window, cx);
            this.show_shell(client, window, cx);
            return this;
        }
        let existing = this
            .launch
            .root
            .as_deref()
            .filter(|_| this.launch.resume)
            .and_then(|root| Instances::load(root).ok())
            .and_then(|list| match this.launch.instance {
                // An instance window runs its own instance (Task 27).
                Some(id) => list.get(id).cloned(),
                None => list.list().first().cloned(),
            });
        match existing {
            Some(cfg) => this.start_client(cfg, window, cx),
            // A device with no account opens on the sign-up, as the web's /welcome does; its link leads to sign-in.
            None => {
                let invite = this.launch.invite.clone();
                this.show_signup(invite, window, cx);
            }
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

    /// The sign-up (Task 25), with the invite this window was opened with (Task 27's `Launch.invite`) in step 1.
    fn show_signup(&mut self, invite: Option<String>, window: &mut Window, cx: &mut Context<Self>) {
        let (root, keychain) = (self.launch.root.clone(), self.launch.keychain.clone());
        let view = cx.new(|cx| SignupView::new(root, keychain, invite, window, cx));
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
        self.fit_window(window, cx);
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
                        // A join refused after the factor is offered again, with its invite and why (App.tsx).
                        crate::screens::create::offer_refused_join(&open.result, cx);
                        cx.notify();
                    }
                }),
            ];
        self.screen = Screen::Done(view);
        cx.notify();
    }

    /// The live route `Route::Settings(section)`: the user settings host over the current screen, or, when settings
    /// already shows, a switch of its section (Task 1b). Any other route is the shell's (Task 8, who owns history).
    pub fn open_route(&mut self, route: &Route, window: &mut Window, cx: &mut Context<Self>) {
        if let Route::ServerSettings { community, section } = route {
            return self.show_server_settings(*community, *section, window, cx);
        }
        let Route::Settings(section) = route else {
            return;
        };
        if let Screen::Settings { host, .. } = &self.screen {
            let section = section.unwrap_or(UserSection::Profile);
            host.update(cx, |host, cx| host.request(Dest::Section(section), cx));
            return;
        }
        // Over the shell, the host's sections draw with the shell's view (its state and client; Task 1b's merge item).
        let ctx = self
            .shell
            .as_ref()
            .map(|s| s.read(cx).slot_cx())
            .unwrap_or_default();
        self.show_settings(*section, ctx, window, cx);
    }

    /// Integration 3 (Task 15): a server's settings over the shell, in Task 15's `ServerSettingsView` (THE server
    /// settings host: its own sections, Task 14's and Task 21's through the slots), with the shell's state and client.
    /// Close takes the shell back; a person's own section in its nav opens the user settings host over the shell.
    fn show_server_settings(
        &mut self,
        community: dilla_client::state::CommunityId,
        section: Option<crate::router::ServerSection>,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        use crate::screens::server_settings::{ServerSettingsView, SettingsEvent as ServerEvent};
        let Some(shell) = self.shell.clone() else {
            return;
        };
        // One settings screen at a time: over the shell, never over another settings screen.
        if !matches!(self.screen, Screen::Shell(_)) {
            return;
        }
        let ctx = shell.read(cx).slot_cx();
        let view = cx.new(|cx| {
            ServerSettingsView::new(
                community,
                ctx.state.clone(),
                ctx.client.clone(),
                ctx.clock,
                section,
                window,
                cx,
            )
        });
        let subscription =
            cx.subscribe_in(&view, window, |this, _, e: &ServerEvent, window, cx| {
                match e {
                    ServerEvent::Announce(message) => {
                        dilla_desktop_ui::components::toast::show_toast(message.clone(), cx);
                        return;
                    }
                    ServerEvent::Close | ServerEvent::OpenUser(_) => {}
                }
                if let Screen::ServerSettings {
                    behind: Some(behind),
                    ..
                } = std::mem::replace(&mut this.screen, Screen::Starting)
                {
                    this.screen = *behind;
                }
                if let Screen::Shell(shell) = &this.screen {
                    let shell = shell.clone();
                    this.focus_shell(&shell, window, cx);
                }
                if let ServerEvent::OpenUser(section) = e {
                    this.open_route(&Route::Settings(Some(*section)), window, cx);
                }
                cx.notify();
            });
        let focus = gpui::Focusable::focus_handle(view.read(cx), cx);
        let behind = std::mem::replace(&mut self.screen, Screen::Starting);
        self.screen = Screen::ServerSettings {
            view,
            behind: Some(Box::new(behind)),
            _subscription: Some(subscription),
        };
        // The settings take the keyboard from their first frame (review M8).
        window.focus(&focus, cx);
        cx.notify();
    }

    /// The route the app shows while settings is open (the host's section, live, so a section switch is reflected);
    /// `None` otherwise: the shell's route is Task 8's.
    pub fn settings_route(&self, cx: &gpui::App) -> Option<Route> {
        match &self.screen {
            Screen::Settings { host, .. } => Some(Route::Settings(Some(host.read(cx).section()))),
            _ => None,
        }
    }

    fn show_settings(
        &mut self,
        section: Option<UserSection>,
        ctx: SlotCx,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let host = settings_host::open(section, None, ctx, cx);
        // A section switch is the host's own; any other route it emits is Close, which takes `behind` back.
        let subscription = cx.subscribe_in(
            &host,
            window,
            |this, _, SettingsEvent::Navigate(route), window, cx| {
                if matches!(route, Route::Settings(_)) {
                    cx.notify();
                    return;
                }
                if let Screen::Settings { behind, .. } =
                    std::mem::replace(&mut this.screen, Screen::Starting)
                {
                    this.screen = *behind;
                }
                // The shell the settings were opened over takes the keyboard back.
                if let Screen::Shell(shell) = &this.screen {
                    let shell = shell.clone();
                    this.focus_shell(&shell, window, cx);
                }
                cx.notify();
            },
        );
        let focus = gpui::Focusable::focus_handle(host.read(cx), cx);
        let behind = std::mem::replace(&mut self.screen, Screen::Starting);
        self.screen = Screen::Settings {
            host,
            behind: Box::new(behind),
            _subscription: subscription,
        };
        // The host holds the keyboard from its first frame (Tab walks its nav and its section).
        window.focus(&focus, cx);
        cx.notify();
    }

    fn show_sign_in(&mut self, error: Option<String>, window: &mut Window, cx: &mut Context<Self>) {
        let (root, keychain) = (self.launch.root.clone(), self.launch.keychain.clone());
        // An instance window's sign-in is pinned to its instance (Task 27, Task 26's `pin`): it signs in nowhere else.
        let pin = self.launch.instance.and_then(|id| {
            root.as_deref()
                .and_then(|r| Instances::load(r).ok())
                .and_then(|list| list.get(id).cloned())
        });
        let view = cx.new(|cx| SignInView::new(root, keychain, pin, error, window, cx));
        self._subscriptions = vec![
            cx.subscribe_in(
                &view,
                window,
                |this, _, SignedIn(cfg): &SignedIn, window, cx| {
                    this.start_client(cfg.clone(), window, cx);
                },
            ),
            // "Create an account instead": the sign-up (Task 25), with the window's invite.
            cx.subscribe_in(&view, window, |this, _, _: &CreateInstead, window, cx| {
                let invite = this.launch.invite.clone();
                this.show_signup(invite, window, cx);
            }),
        ];
        self.screen = Screen::SignIn(view);
        self.fit_window(window, cx);
        cx.notify();
    }

    fn start_client(&mut self, cfg: InstanceConfig, window: &mut Window, cx: &mut Context<Self>) {
        // An instance window runs its own instance only: a sign-in that ended on another one (the sign-in view takes
        // any server address) is refused here, so this window never starts, or holds, a different instance (Task 27).
        if self.launch.instance.is_some_and(|pinned| pinned != cfg.id) {
            let wrong = crate::screens::instances::copy::ERROR_WRONG_INSTANCE;
            return self.show_sign_in(Some(wrong.into()), window, cx);
        }
        // Re-review round 2, C: a Sign out's wipe of this instance still waits for its old client (whose store is
        // open): no start of it, the auto-start of a reopened window included. The sign-in says why, until the wipe
        // ends.
        if crate::windows::wipe_pending(cx, cfg.id) {
            return self.wait_for_wipe(cfg.id, window, cx);
        }
        // A wipe that waits for an older client of this instance deletes nothing once it starts again (re-review C).
        crate::windows::note_start(cx, cfg.id);
        self.instance = Some(cfg.id);
        // A window opened again on an instance whose client kept running shows that client (windows.rs).
        if let Some(client) = crate::windows::running_client(cx, cfg.id) {
            return self.started(cfg, Ok(client), window, cx);
        }
        self.screen = Screen::Starting;
        self.fit_window(window, cx);
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
        // The instance's played-video directory is wiped at open, and remembered for the wipe at quit (Task 12). Only
        // the window that starts the client wipes: a second window on a live instance (above) must not, while its
        // player may hold a file there.
        crate::screens::attachments::ui::instance_opened(&cfg.data_dir, cx);
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
            // Held until this start ends, or the window goes (a released instance waits on it).
            let _token = token;
            let result = rx
                .await
                .unwrap_or_else(|_| Err(ClientError::Runtime("the start thread ended".into())));
            let _ = this.update_in(cx, |this, window, cx| {
                this.started(retry, result.map(Rc::new), window, cx);
            });
        })
        .detach();
    }

    fn started(
        &mut self,
        cfg: InstanceConfig,
        result: Result<Rc<Client>, ClientError>,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        if result.is_err()
            && let Some(id) = self.instance
        {
            crate::windows::finish_starting(cx, id);
        }
        let client = match result {
            Ok(client) => client,
            // No account on this instance yet: the sign-up, as the web's /welcome (Task 25); an invite this window
            // was opened with names this instance, so step 1 gets its link.
            Err(ClientError::NoIdentity) => {
                let invite = self.launch.invite.clone().map(|code| {
                    cfg.server
                        .join(&format!("i/{code}"))
                        .map_or(code, |u| u.to_string())
                });
                return self.show_signup(invite, window, cx);
            }
            // A key lost from the keychain (an interrupted removal, a reset keychain): say what to do (Task 27).
            Err(ClientError::Store(m)) if m.contains("key is missing") => {
                return self.show_sign_in(
                    Some(crate::screens::instances::copy::KEY_MISSING.into()),
                    window,
                    cx,
                );
            }
            // A device the instance no longer knows is the sign-in's to answer (the web's `enterRevoked`); any other failed
            // start is a splash with its stable code (Task 24: `boot::classify`).
            // Task 26 (Task 2's note): with the start's error in the sign-in screen's words.
            Err(e) if boot::classify(&e) == boot::StartFailure::SignIn => {
                let copy = crate::screens::factor::start_error_copy(&e);
                return self.show_sign_in(Some(copy), window, cx);
            }
            Err(e) => {
                self.screen = Screen::Fatal(e, cfg);
                self.fit_window(window, cx);
                cx.notify();
                return;
            }
        };
        // The registry keeps the client: closing the window does not stop it (Task 27).
        if let Some(id) = self.instance {
            crate::windows::keep_client(cx, id, client.clone());
        }
        // The Diagnostics section's instance fact and the connecting line's node: the server's host.
        if let Some(host) = cfg.server.host_str() {
            client.diagnostics().set_instance_name(host);
        }
        // Task 26: the factor area knows this window's client and its instance (the system-browser ceremony, step-up,
        // Forget this device).
        crate::screens::factor::register_instance(&client, cfg, cx);
        crate::screens::factor::register_window_client(&client, window, cx);
        if client.state().borrow().account.phase == dilla_client::state::AccountPhase::FactorSetup {
            return self.show_factor(client, window, cx);
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
        launch.after_signup = std::mem::take(&mut self.after_signup);
        match crate::open_window_in(cx, launch, WindowMode::Workspace) {
            Ok(handle) => {
                // The instance's window is the workspace window from now on (Task 27's registry).
                if let Some(id) = self.instance {
                    crate::windows::moved_window(cx, id, handle);
                }
                // The automation session follows the client into the new window, as a person's eyes do.
                #[cfg(feature = "automation")]
                crate::automation::handed_over(window.window_handle(), handle.into());
                window.remove_window();
            }
            // The workspace window did not open: this one stays, on the sign-in.
            Err(e) => self.show_sign_in(Some(e.to_string()), window, cx),
        }
    }

    /// Task 26: the forced-factor screen; it goes on to the shell once the factor is set, or forgets the device.
    fn show_factor(&mut self, client: Rc<Client>, window: &mut Window, cx: &mut Context<Self>) {
        use crate::screens::factor::forced::{ForcedEvent, ForcedFactorView};
        use crate::screens::factor::setup::Origin;
        let Some(cfg) = crate::screens::factor::instance_of(&client, cx) else {
            let copy = crate::strings::tf("factor.error.other", &[("code", "E_INSTANCE")]);
            return self.show_sign_in(Some(copy), window, cx);
        };
        // A new account's factor is the sign-up's step 4 (Task 25: its step line, dots and codes); any other owed factor
        // is the forced screen.
        let origin = if self.after_signup {
            use crate::screens::onboarding::signup::TOTAL;
            Origin::Signup {
                step: crate::strings::tf(
                    "onboarding.step",
                    &[("n", "4"), ("total", &TOTAL.to_string())],
                ),
                progress: (4, TOTAL),
            }
        } else {
            Origin::Required
        };
        let view = cx.new(|cx| {
            ForcedFactorView::new(client.clone(), cfg.server.clone(), origin, window, cx)
        });
        let held = client.clone();
        self._subscriptions = vec![cx.subscribe_in(
            &view,
            window,
            move |this, _, e: &ForcedEvent, window, cx| match e {
                ForcedEvent::Completed => {
                    let client = held.clone();
                    if this.launch.shipping && this.screen.mode().is_compact() {
                        this.open_workspace(client, window, cx);
                    } else {
                        this.show_shell(client, window, cx);
                    }
                }
                ForcedEvent::Forget => this.forget(held.clone(), cfg.clone(), window, cx),
            },
        )];
        if let Some(note) = self.forget_note.take() {
            view.update(cx, |v, cx| v.forget_failed(note, cx));
        }
        self.screen = Screen::Factor(view);
        self.fit_window(window, cx);
        cx.notify();
    }

    /// Task 26 (ForgetBrowser): the device signs out on the instance and forgets it, then the sign-in. Every view that
    /// held the client is dropped first; the client halts through the shared handle, so a clone left anywhere cannot
    /// keep it running. A refusal is shown on the forced screen's banner, never swallowed.
    fn forget(
        &mut self,
        client: Rc<Client>,
        cfg: InstanceConfig,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        self.wipe(client, cfg, Wipe::Forget, window, cx);
    }

    /// The one way a client's local data goes (integration 3, step 0 item 1): Forget this device (Task 26: its call
    /// left and its session signed out first), or a device that signed itself out and removed itself (Task 16's Devices
    /// "Sign out", `profile.signed_out`: the server half is done). Both release the client everywhere
    /// ([`AppView::release`]), wait for the last handle, then close the store and wipe it.
    fn wipe(
        &mut self,
        client: Rc<Client>,
        cfg: InstanceConfig,
        how: Wipe,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let (root, keychain) = (self.launch.root.clone(), self.launch.keychain.clone());
        // Re-review round 2, C: a Sign out's wipe is pending from now until it ends: a start of the instance (a sign-in,
        // or a reopened window's auto-start) is refused meanwhile. The pending flag, the start count and the decision
        // to delete live on the App, never on this window: the wipe ends, and the flag clears, though the window
        // closed while it waited.
        let generation = (how == Wipe::SignedOut).then(|| crate::windows::wipe_begin(cx, cfg.id));
        cx.spawn_in(window, async move |this, cx| {
            // A call this device is in is left first (Task 17; review M3: a Sign out too): its media and its
            // self-Remove go before the session and the store do. After a Sign out the engine may already be stopped:
            // the leave then ends with an error, which is fine (the device is gone from the account anyway).
            let _ = client.voice().leave().await;
            if how == Wipe::Forget
                && let Err(message) = crate::screens::factor::sign_out(&client).await
            {
                let _ = this.update(cx, |this, cx| this.forget_failed(message, cx));
                return;
            }
            // Every view that held the client goes first; then the last handle stops it and the wipe waits for the
            // store to close.
            let released = this.update(cx, |this, cx| {
                this.release(&client, cx);
                cx.notify();
            });
            // The window went meanwhile (its views with it): every other holder still lets go.
            if released.is_err() {
                gpui::AsyncApp::update(cx, |cx| crate::release::release_client(&client, cx));
            }
            let mut client = client;
            let mut frames = 0;
            let owned = loop {
                match Rc::try_unwrap(client) {
                    Ok(owned) => break Ok(owned),
                    Err(shared) if frames < 250 => {
                        client = shared;
                        frames += 1;
                        cx.background_executor()
                            .timer(std::time::Duration::from_millis(20))
                            .await;
                    }
                    Err(shared) => break Err(shared),
                }
            };
            let store = || crate::strings::tf("factor.error.other", &[("code", "E_FORGET_STORE")]);
            let id = cfg.id;
            // The pending wipe's end, and whether it may still delete: on the App, whether or not the window lives.
            let may_delete = |cx: &gpui::AsyncWindowContext, generation: u64| {
                gpui::AsyncApp::update(cx, |cx| crate::windows::wipe_may_delete(cx, id, generation))
            };
            let wipe_end = |cx: &gpui::AsyncWindowContext| {
                gpui::AsyncApp::update(cx, |cx| crate::windows::wipe_end(cx, id))
            };
            match owned {
                Ok(owned) => {
                    // Defence in depth: a client of the instance that started meanwhile keeps its store.
                    let result = if generation.is_none_or(|g| may_delete(cx, g)) {
                        crate::screens::factor::close_and_wipe(owned, cfg.clone(), root, keychain)
                            .await
                    } else {
                        drop(owned);
                        Err(store())
                    };
                    if generation.is_some() {
                        wipe_end(cx);
                    }
                    let _ = this.update_in(cx, |this, window, cx| match result {
                        Ok(()) => this.show_sign_in(None, window, cx),
                        // Never a halted client: it starts again on the forced screen, which says why and can retry
                        // (the sessions are gone, so the retry's sign-out is a 401, which counts as done). A device
                        // that removed itself starts on the sign-in, which says the device is gone.
                        Err(message) => {
                            this.forget_note = Some(message);
                            this.start_client(cfg, window, cx);
                        }
                    });
                }
                // A view still holds the client: it was never stopped, nothing was wiped; back on the forced screen (a
                // device that removed itself: the sign-in, saying the store could not go).
                // A device that removed itself: the sign-in says the store has not gone yet (review M4), and the wipe is
                // tried again whenever the last other holder lets go (every second, for as long as the app runs), then
                // the sign-in's note goes. The halted client sends nothing meanwhile.
                Err(shared) if how == Wipe::SignedOut => {
                    shared.halt();
                    // Re-review C: until this wipe ends (its flag was set when it began), a start of the instance is
                    // refused (its old store is still open), and a start of it after the wipe began means it deletes
                    // nothing. The window may close meanwhile: the wipe goes on without it.
                    let _ = this.update_in(cx, |this, window, cx| {
                        this.show_sign_in(Some(store()), window, cx);
                    });
                    let mut client = shared;
                    let owned = loop {
                        match Rc::try_unwrap(client) {
                            Ok(owned) => break owned,
                            Err(again) => {
                                client = again;
                                cx.background_executor()
                                    .timer(std::time::Duration::from_secs(1))
                                    .await;
                            }
                        }
                    };
                    // Defence in depth: a client of the instance runs, starts or started since this wipe began (a new
                    // session on the same data dir): the old client just goes, nothing is deleted.
                    let result = if generation.is_some_and(|g| may_delete(cx, g)) {
                        crate::screens::factor::close_and_wipe(owned, cfg.clone(), root, keychain)
                            .await
                    } else {
                        drop(owned);
                        Err(store())
                    };
                    wipe_end(cx);
                    let _ = this.update_in(cx, |this, window, cx| {
                        if result.is_ok() && this.shows_sign_in_screen() {
                            this.show_sign_in(None, window, cx);
                        }
                    });
                }
                Err(shared) => {
                    let _ = this.update_in(cx, |this, window, cx| {
                        crate::screens::factor::register_instance(&shared, cfg, cx);
                        crate::screens::factor::register_window_client(&shared, window, cx);
                        if let Some(id) = this.instance {
                            crate::windows::keep_client(cx, id, shared.clone());
                        }
                        this.forget_note = Some(store());
                        shared.reconnect();
                        this.show_factor(shared, window, cx);
                    });
                }
            }
        })
        .detach();
    }

    /// Tests (feature `test-hooks`, review M6): the shell's client comes to owe a factor (the forced screen, as
    /// `show_shell`'s state loop shows it), and the person confirms Forget this device there.
    #[cfg(feature = "test-hooks")]
    pub fn forget_from_forced(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let Some(client) = self
            .shell
            .as_ref()
            .and_then(|s| s.read(cx).client().cloned())
        else {
            return;
        };
        let Some(cfg) = crate::screens::factor::instance_of(&client, cx) else {
            return;
        };
        self.show_factor(client.clone(), window, cx);
        self.forget(client, cfg, window, cx);
    }

    /// Automation: the window shows the sign-in.
    #[cfg(feature = "test-hooks")]
    pub fn shows_sign_in(&self) -> bool {
        matches!(self.screen, Screen::SignIn(_))
    }

    /// Automation: the sign-in's banner (an error or a note), while the window shows the sign-in.
    #[cfg(feature = "test-hooks")]
    pub fn sign_in_banner(&self, cx: &gpui::App) -> Option<String> {
        match &self.screen {
            Screen::SignIn(view) => view.read(cx).banner_text().map(str::to_owned),
            _ => None,
        }
    }

    /// Automation: what the window shows, by name (`settings` while the host is open, whatever is behind it).
    #[cfg(feature = "test-hooks")]
    pub fn screen_name(&self) -> &'static str {
        match &self.screen {
            Screen::Starting => "starting",
            Screen::Fatal(..) => "fatal",
            Screen::SignIn(_) => "sign-in",
            Screen::Signup(_) => "signup",
            Screen::Done(_) => "done",
            Screen::Shell(_) => "shell",
            Screen::Factor(_) => "factor",
            Screen::Settings { .. } => "settings",
            Screen::ServerSettings { .. } => "server-settings",
        }
    }

    /// Automation: the open server settings and whether they sit over a screen that comes back on close.
    #[cfg(feature = "test-hooks")]
    pub fn server_settings(
        &self,
    ) -> Option<(
        Entity<crate::screens::server_settings::ServerSettingsView>,
        bool,
    )> {
        match &self.screen {
            Screen::ServerSettings { view, behind, .. } => Some((view.clone(), behind.is_some())),
            _ => None,
        }
    }

    /// Automation: the open settings host, and how many settings hosts are stacked (one at most).
    #[cfg(feature = "test-hooks")]
    pub fn settings_host(&self) -> Option<(Entity<UserSettingsHost>, usize)> {
        let Screen::Settings { host, behind, .. } = &self.screen else {
            return None;
        };
        let mut depth = 1;
        let mut under: &Screen = behind;
        while let Screen::Settings { behind, .. } = under {
            depth += 1;
            under = behind;
        }
        Some((host.clone(), depth))
    }

    /// Integration 3, step 0 item 1: the window and the app let go of `client` (Forget, sign-out), so the caller's
    /// handle is the last and its store can close: the shell this window kept (a shell shown earlier still held it
    /// behind the forced screen), the screen, the subscriptions, the instance registry's hold (Task 27) and every
    /// app-wide holder ([`crate::release::release_client`]).
    fn release(&mut self, client: &Rc<Client>, cx: &mut Context<Self>) {
        self.shell = None;
        self.shell_sub = None;
        self._subscriptions.clear();
        self.screen = Screen::Starting;
        // An instance window's registry holds the client too (Task 27): it lets go, so the store can close.
        if let Some(id) = self.instance {
            crate::windows::drop_client(cx, id);
        }
        crate::release::release_client(client, cx);
    }

    /// Task 16's Devices "Sign out" landed (`profile.signed_out`): the store goes through the one release path.
    fn signed_out(&mut self, client: Rc<Client>, window: &mut Window, cx: &mut Context<Self>) {
        let Some(cfg) = crate::screens::factor::instance_of(&client, cx) else {
            return self.show_sign_in(None, window, cx);
        };
        self.wipe(client, cfg, Wipe::SignedOut, window, cx);
    }

    /// The start of instance `id` was refused while a Sign out's wipe of it is pending: the sign-in says why, and its
    /// note goes once the wipe ended (from whichever window it ran in, or none).
    fn wait_for_wipe(&mut self, id: InstanceId, window: &mut Window, cx: &mut Context<Self>) {
        let store = crate::strings::tf("factor.error.other", &[("code", "E_FORGET_STORE")]);
        self.show_sign_in(Some(store), window, cx);
        cx.spawn_in(window, async move |this, cx| {
            loop {
                cx.background_executor()
                    .timer(std::time::Duration::from_millis(250))
                    .await;
                let Ok(pending) = this.update(cx, |_, cx| crate::windows::wipe_pending(cx, id))
                else {
                    return;
                };
                if !pending {
                    break;
                }
            }
            let _ = this.update_in(cx, |this, window, cx| {
                if this.shows_sign_in_screen() {
                    this.show_sign_in(None, window, cx);
                }
            });
        })
        .detach();
    }

    fn shows_sign_in_screen(&self) -> bool {
        matches!(self.screen, Screen::SignIn(_))
    }

    /// A Forget that did not go through: said on the forced screen.
    fn forget_failed(&mut self, message: String, cx: &mut Context<Self>) {
        if let Screen::Factor(view) = &self.screen {
            view.update(cx, |v, cx| v.forget_failed(message, cx));
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
        // Held weakly: a Forget or a sign-out releases the client while this loop waits (integration 3, step 0 item 1).
        let factor_client = Rc::downgrade(&client);
        cx.spawn_in(window, async move |this, cx| {
            while states.changed().await.is_ok() {
                let next = states.borrow_and_update().clone();
                // Task 16: this device signed itself out and removed itself: its store is wiped (the one release
                // path), then the sign-in.
                if next.profile.signed_out {
                    if let Some(client) = factor_client.upgrade() {
                        let _ = this
                            .update_in(cx, |this, window, cx| this.signed_out(client, window, cx));
                    }
                    break;
                }
                // Task 26: a renewal or a refusal found a factor owed: the forced screen.
                if next.account.phase == dilla_client::state::AccountPhase::FactorSetup {
                    if let Some(client) = factor_client.upgrade() {
                        let _ = this
                            .update_in(cx, |this, window, cx| this.show_factor(client, window, cx));
                    }
                    break;
                }
                // The settings host over the shell draws its sections with the same state (Task 1b).
                let _ = this.update(cx, |this, cx| {
                    // Task 21's dock player: fed while this device is in a call with the Bot in it.
                    if let Some(client) = factor_client.upgrade() {
                        crate::screens::bot_admin::player::follow(&next, &client, cx);
                    }
                    if let Screen::Settings { host, .. } = &this.screen {
                        let next = next.clone();
                        host.update(cx, |h, cx| h.set_state(next, cx));
                    }
                    if let Screen::ServerSettings { view, .. } = &this.screen {
                        let next = next.clone();
                        view.update(cx, |v, cx| v.set_state(next, cx));
                    }
                });
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
        self.attach_shell(&shell, window, cx);
        self.focus_shell(&shell, window, cx);
        self.screen = Screen::Shell(shell);
        self.fit_window(window, cx);
        let after_signup = std::mem::take(&mut self.after_signup);
        // An invite this window was opened with (Task 27), for a running client: the join dialog takes it (a sign-up's
        // own join is done by the sign-up).
        if !after_signup && let Some(invite) = self.launch.invite.take() {
            crate::screens::create::open_join_in(window, Some(invite), cx);
        }
        let starts_ready = client.state().borrow().account.phase == AccountPhase::Ready;
        if after_signup || !starts_ready {
            // A new account, or any client that starts before it is ready (the app closed at the factor step and opened
            // again): once its factor is in place (Task 26's screens) it is ready, and if a sign-up's join is kept in the
            // store, the invite's server is joined (a refused join is the result, not a failure) and the done step shows.
            let mut ready = client.state();
            // Held weakly while it waits: a Forget or a sign-out meanwhile releases the client (step 0 item 1).
            let held = Rc::downgrade(&client);
            cx.spawn_in(window, async move |this, cx| {
                let onboarding = match held.upgrade() {
                    Some(client) => client.onboarding(),
                    None => return,
                };
                let pending = onboarding.has_pending_join().await;
                drop(onboarding);
                if !after_signup && !pending {
                    return;
                }
                loop {
                    let phase = ready.borrow_and_update().account.phase;
                    if phase == AccountPhase::Ready {
                        break;
                    }
                    if ready.changed().await.is_err() {
                        return;
                    }
                }
                let Some(client) = held.upgrade() else {
                    return;
                };
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

    /// The window's shell: kept, and its routes to the settings followed (the title bar's gear, Ctrl+, the DM banners'
    /// Privacy and Devices, the server menu: the user settings host of Task 1b, the server settings of Task 15).
    fn attach_shell(
        &mut self,
        shell: &Entity<ShellView>,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        self.shell = Some(shell.clone());
        self.shell_sub = Some(cx.subscribe_in(
            shell,
            window,
            |this, _, OpenSettings(route), window, cx| this.open_route(route, window, cx),
        ));
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
            Screen::Signup(view) => view.clone().into_any_element(),
            Screen::Done(view) => view.clone().into_any_element(),
            Screen::Shell(view) => view.clone().into_any_element(),
            Screen::Factor(view) => view.clone().into_any_element(),
            Screen::Settings { host, .. } => host.clone().into_any_element(),
            Screen::ServerSettings { view, .. } => view.clone().into_any_element(),
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
        // Push to talk is heard at the root, whatever screen shows, and lets go when the screen is not the shell
        // (review I1: the settings replacing the shell's dock left a held key's microphone open).
        let w = crate::screens::voice::wid(window);
        if !matches!(self.screen, Screen::Shell(_)) {
            crate::screens::voice::release_ptt(w, cx);
        }
        // The window's outermost element carries push to talk's keys: it is on every key's dispatch path (it takes the
        // focus itself when nothing holds it).
        // The focus outside the root (none, or a stale one on a control that went away) lets push to talk go and, while
        // the call is on, comes back to the root (review B; a stale focus is judged on the next frame, re-review B2).
        crate::screens::voice::guard_focus(w, &self.root_focus, window, cx);
        crate::screens::voice::ptt_keys(
            w,
            div()
                .track_focus(&self.root_focus)
                .size_full()
                .child(tracked(
                    "root",
                    div()
                        .size_full()
                        .bg(color::BG)
                        .child(body)
                        .children(strips.into_iter().flatten()),
                )),
        )
    }
}
