//! Area **factors** (Task 26): the factor set-up, the forced-factor screen, backup codes, step-up and the Sign-in
//! section. The sign-in screen itself is `screens::sign_in`.
//!
//! The hooks below are where `crate::slots` and the app draw this area: [`settings_section`] (Settings → Sign-in),
//! [`ask_step_up`] (Task 21's `bot_page::StepUp`) with [`step_up_overlay`], and [`register_instance`], which the app
//! calls when it starts a client so this area knows the instance's address for the system-browser ceremony.

pub mod code;
pub mod forced;
pub mod frame;
pub mod keys;
pub mod settings;
pub mod setup;
pub mod step_up;

use std::collections::HashMap;
use std::rc::Rc;

use dilla_client::Client;
use dilla_client::features::factors::StepUpToken;
use gpui::{AnyElement, App, AppContext as _, Empty, Entity, Global, IntoElement, Window};
use zeroize::Zeroizing;

use crate::slots::{SectionId, SlotCx};

/// The instance address of each running client (the app registers it when it starts the client): the system-browser
/// ceremony opens that instance's page. Keyed by the client's allocation, removed when the client is dropped from it.
#[derive(Default)]
struct Instances(HashMap<usize, dilla_client::seams::InstanceConfig>);

impl Global for Instances {}

fn key(client: &Rc<Client>) -> usize {
    Rc::as_ptr(client) as usize
}

/// The app's hook: `client` serves `server` (call when the client starts; the controller hook in `app.rs`).
pub fn register_instance(
    client: &Rc<Client>,
    cfg: dilla_client::seams::InstanceConfig,
    cx: &mut App,
) {
    cx.default_global::<Instances>().0.insert(key(client), cfg);
}

/// The app's hook: the client stopped.
pub fn forget_instance(client: &Rc<Client>, cx: &mut App) {
    if cx.has_global::<Instances>() {
        cx.global_mut::<Instances>().0.remove(&key(client));
    }
}

/// The instance `client` was registered with ([`register_instance`]).
pub fn instance_of(client: &Rc<Client>, cx: &App) -> Option<dilla_client::seams::InstanceConfig> {
    cx.try_global::<Instances>()
        .and_then(|i| i.0.get(&key(client)).cloned())
}

/// Task 26 (ForgetBrowser), the whole of it: the gateway goes down, the device signs out on the instance
/// ([`dilla_client::features::factors::Factors::sign_out_device`]), the client halts through the shared handle (so no
/// view's clone can keep it running), then this device forgets the instance. Answers why it did not, never silently:
/// a refused sign-out leaves everything in place and reconnects.
pub async fn forget_device(
    client: Rc<Client>,
    cfg: dilla_client::seams::InstanceConfig,
    root: Option<std::path::PathBuf>,
    keychain: std::sync::Arc<dyn dilla_client::seams::Keychain>,
) -> Result<(), String> {
    client.disconnect();
    if let Err(e) = client.factors().sign_out_device().await {
        client.reconnect();
        return Err(e.code);
    }
    client.halt();
    let root = root.ok_or_else(|| "E_STORE".to_owned())?;
    let (tx, rx) = tokio::sync::oneshot::channel();
    std::thread::Builder::new()
        .name("dilla-forget".into())
        .spawn(move || {
            let result =
                dilla_client::features::factors::forget_device(&root, &cfg, keychain.as_ref());
            let _ = tx.send(result.map_err(|_| "E_FORGET".to_owned()));
        })
        .map_err(|_| "E_THREAD".to_owned())?;
    rx.await.unwrap_or_else(|_| Err("E_FORGET".to_owned()))
}

fn server_of(client: &Rc<Client>, cx: &App) -> Option<url::Url> {
    cx.try_global::<Instances>()
        .and_then(|i| i.0.get(&key(client)).map(|c| c.server.clone()))
}

/// Draws one of this area's settings sections (`slots::settings_sections` points here): Settings → Sign-in.
pub fn settings_section(
    _section: SectionId,
    ctx: &SlotCx,
    window: &mut Window,
    cx: &mut App,
) -> AnyElement {
    let (Some(client), Some(server)) = (
        ctx.client.clone(),
        ctx.client.as_ref().and_then(|c| server_of(c, cx)),
    ) else {
        return Empty.into_any_element();
    };
    let section: Entity<settings::SignInSection> =
        window.use_keyed_state(("factor-signin", key(&client)), cx, |window, cx| {
            settings::SignInSection::new(client, server, window, cx)
        });
    section.into_any_element()
}

/// What [`ask_step_up`] hands its caller: the token, then the window and the app (Task 21's `StepUpAsk` shape).
pub type StepUpThen = Box<dyn FnOnce(Zeroizing<String>, &mut Window, &mut App)>;

/// The step-up asked for by another area, waiting in its window.
struct Pending {
    dialog: Entity<step_up::StepUpDialog>,
    _subscription: gpui::Subscription,
}

#[derive(Default)]
struct PendingStepUps(HashMap<gpui::WindowId, Pending>);

impl Global for PendingStepUps {}

/// Task 21's seam (`bot_page::StepUp(Rc<StepUpAsk>)`): asks the person in this window for a fresh second factor and hands
/// the token, in a zeroizing buffer, to `then`; `then` is not called when the person cancels. The controller installs
/// it at merge as `cx.set_global(bot_page::StepUp(Rc::new(screens::factor::ask_step_up)))` and draws
/// [`step_up_overlay`] in the shell's overlays slot.
pub fn ask_step_up(then: StepUpThen, window: &mut Window, cx: &mut App) {
    let Some((client, server)) = window_client(window, cx) else {
        return;
    };
    let proof = match client.state().borrow().factors.factors.as_ref() {
        Some(f) if !f.has_password && !f.passkeys.is_empty() => step_up::Proof::Passkey,
        Some(f) if !f.has_password => step_up::Proof::Code,
        _ => step_up::Proof::Password,
    };
    let id = window.window_handle().window_id();
    let dialog =
        cx.new(|cx| step_up::StepUpDialog::new(client.factors(), server, proof, None, window, cx));
    let mut then = Some(then);
    let subscription = window.subscribe(
        &dialog,
        cx,
        move |_, e: &step_up::StepUpEvent, window, cx| {
            if let step_up::StepUpEvent::Token(StepUpToken(token)) = e
                && let Some(then) = then.take()
            {
                then(token.clone(), window, cx);
            }
            cx.default_global::<PendingStepUps>().0.remove(&id);
            window.refresh();
        },
    );
    cx.default_global::<PendingStepUps>().0.insert(
        id,
        Pending {
            dialog,
            _subscription: subscription,
        },
    );
    window.refresh();
}

/// The pending step-up of this window, for the shell's overlays slot (the controller adds it at merge).
pub fn step_up_overlay(window: &mut Window, cx: &mut App) -> Option<AnyElement> {
    let id = window.window_handle().window_id();
    cx.try_global::<PendingStepUps>()
        .and_then(|p| p.0.get(&id))
        .map(|p| p.dialog.clone().into_any_element())
}

/// Task 2's note: what the start screen says when `Client::start` failed (its factor read included: a 5xx there is
/// `ClientError::Sync(code)`, no answer `Network`), in the sign-in screen's words, never the error's own text.
pub fn start_error_copy(e: &dilla_client::ClientError) -> String {
    use dilla_client::ClientError as E;
    match e {
        E::Network(_) => crate::strings::t("signin.error.network").to_owned(),
        E::Revoked => crate::strings::tf("signin.error.other", &[("code", "E_REVOKED")]),
        E::Sync(code) => crate::strings::tf("factor.error.other", &[("code", code)]),
        E::Store(_) => crate::strings::tf("signin.error.other", &[("code", "E_STORE")]),
        other => {
            let code = match other {
                E::FactorRequired => "E_FACTOR_REQUIRED",
                E::Stopped => "E_STOPPED",
                E::Runtime(_) => "E_RUNTIME",
                _ => "E_CLIENT",
            };
            crate::strings::tf("signin.error.other", &[("code", code)])
        }
    }
}

/// The client this window shows, as the app registered it for the window ([`register_window_client`]).
fn window_client(window: &Window, cx: &App) -> Option<(Rc<Client>, url::Url)> {
    let id = window.window_handle().window_id();
    let client = cx.try_global::<WindowClients>()?.0.get(&id)?.upgrade()?;
    let server = server_of(&client, cx)?;
    Some((client, server))
}

#[derive(Default)]
struct WindowClients(HashMap<gpui::WindowId, std::rc::Weak<Client>>);

impl Global for WindowClients {}

/// The app's hook: this window now shows `client` (the controller hook in `app.rs`, beside [`register_instance`]).
pub fn register_window_client(client: &Rc<Client>, window: &Window, cx: &mut App) {
    cx.default_global::<WindowClients>()
        .0
        .insert(window.window_handle().window_id(), Rc::downgrade(client));
}
