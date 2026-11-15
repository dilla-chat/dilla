//! Area **factors** (Task 26): the factor set-up, the forced-factor screen, backup codes, step-up and the Sign-in
//! section. The sign-in screen itself is `screens::sign_in`.
//!
//! The hooks below are where `crate::slots` and the app draw this area: [`settings_section`] (Settings → Sign-in),
//! [`ask_step_up`] (Task 21's `bot_page::StepUp`) with [`step_up_overlay`], and [`register_window_client`], which the
//! app calls when a window shows a client. The instance's address (the system-browser ceremony) is the client's own
//! (`Client::instance`).

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

fn key(client: &Rc<Client>) -> usize {
    Rc::as_ptr(client) as usize
}

/// Lets go of `client` (Forget, sign-out: `crate::release::release_client`): a step-up waiting in a window that shows
/// it is dropped (its dialog holds the client's factors handle), and the window map forgets those windows.
pub fn release(client: &Rc<Client>, cx: &mut App) {
    let windows: Vec<gpui::WindowId> = cx
        .try_global::<WindowClients>()
        .map(|w| {
            w.0.iter()
                .filter(|(_, c)| c.upgrade().is_some_and(|c| Rc::ptr_eq(&c, client)))
                .map(|(id, _)| *id)
                .collect()
        })
        .unwrap_or_default();
    for id in &windows {
        cx.default_global::<PendingStepUps>().0.remove(id);
        cx.default_global::<WindowClients>().0.remove(id);
    }
    let k = key(client);
    cx.default_global::<SignInSections>()
        .0
        .retain(|(_, c), _| *c != k);
}

/// `window` closed: its pending step-up, its client and its Sign-in sections go (final review C1).
pub fn window_closed(window: gpui::WindowId, cx: &mut App) {
    if cx.has_global::<PendingStepUps>() {
        cx.global_mut::<PendingStepUps>().0.remove(&window);
    }
    if cx.has_global::<WindowClients>() {
        cx.global_mut::<WindowClients>().0.remove(&window);
    }
    if cx.has_global::<SignInSections>() {
        cx.global_mut::<SignInSections>()
            .0
            .retain(|(w, _), _| *w != window);
    }
}

/// The instance `client` serves.
pub fn instance_of(client: &Rc<Client>) -> dilla_client::seams::InstanceConfig {
    client.instance().clone()
}

/// Task 26 (ForgetBrowser), the server half: the gateway goes down and the device signs out on the instance
/// ([`dilla_client::features::factors::Factors::sign_out_device`]). A refusal reconnects and answers what to say; the
/// local half ([`close_and_wipe`]) follows only after this succeeded.
pub async fn sign_out(client: &Rc<Client>) -> Result<(), String> {
    client.disconnect();
    match client.factors().sign_out_device().await {
        Ok(()) => Ok(()),
        Err(e) => {
            client.reconnect();
            // Offline: the web's own words for a dropped connection (`factor.error.network`).
            Err(
                if e.status == 0 && (e.code == "E_NETWORK" || e.code == "E_UNAVAILABLE") {
                    crate::strings::t("factor.error.network").to_owned()
                } else {
                    crate::strings::tf("factor.error.other", &[("code", &e.code)])
                },
            )
        }
    }
}

/// Task 26 (ForgetBrowser), the local half, with the last handle of the client (every view that held it dropped):
/// the client stops, the wipe waits until the store is closed, then the data dir and its key go. Off the UI thread.
/// On an error the store is left as it was and the caller starts the client again (a retry signs out again, where a
/// 401 counts as done).
pub async fn close_and_wipe(
    client: Client,
    cfg: dilla_client::seams::InstanceConfig,
    root: Option<std::path::PathBuf>,
    keychain: std::sync::Arc<dyn dilla_client::seams::Keychain>,
) -> Result<(), String> {
    let failed = || crate::strings::tf("factor.error.other", &[("code", "E_FORGET_STORE")]);
    let Some(root) = root else {
        return Err(failed());
    };
    let (tx, rx) = tokio::sync::oneshot::channel();
    std::thread::Builder::new()
        .name("dilla-forget".into())
        .spawn(move || {
            let result = dilla_client::features::factors::close_and_forget(
                client,
                &root,
                &cfg,
                keychain.as_ref(),
                std::time::Duration::from_secs(15),
            );
            let _ = tx.send(result);
        })
        .map_err(|_| failed())?;
    match rx.await {
        Ok(Ok(())) => Ok(()),
        _ => Err(failed()),
    }
}

fn server_of(client: &Rc<Client>) -> url::Url {
    client.instance().server.clone()
}

/// Draws one of this area's settings sections (`slots::settings_sections` points here): Settings → Sign-in.
pub fn settings_section(
    _section: SectionId,
    ctx: &SlotCx,
    window: &mut Window,
    cx: &mut App,
) -> AnyElement {
    let (Some(client), Some(server)) = (ctx.client.clone(), ctx.client.as_ref().map(server_of))
    else {
        return Empty.into_any_element();
    };
    // One per window and client, kept in a map release empties at once (review M5: the window's keyed state would hold
    // the client until the window next draws).
    let slot = (window.window_handle().window_id(), key(&client));
    let live: std::collections::HashSet<gpui::WindowId> =
        cx.windows().iter().map(|w| w.window_id()).collect();
    let sections = cx.default_global::<SignInSections>();
    sections.0.retain(|(w, _), _| live.contains(w));
    let section = match sections.0.get(&slot) {
        Some(s) => s.clone(),
        None => {
            let s = cx.new(|cx| settings::SignInSection::new(client, server, window, cx));
            cx.default_global::<SignInSections>()
                .0
                .insert(slot, s.clone());
            s
        }
    };
    section.into_any_element()
}

/// Each window's Sign-in section, by window and client.
#[derive(Default)]
struct SignInSections(HashMap<(gpui::WindowId, usize), Entity<settings::SignInSection>>);

impl Global for SignInSections {}

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
    let server = server_of(&client);
    Some((client, server))
}

#[derive(Default)]
struct WindowClients(HashMap<gpui::WindowId, std::rc::Weak<Client>>);

impl Global for WindowClients {}

/// The app's hook: this window now shows `client` (the controller hook in `app.rs`).
pub fn register_window_client(client: &Rc<Client>, window: &Window, cx: &mut App) {
    cx.default_global::<WindowClients>()
        .0
        .insert(window.window_handle().window_id(), Rc::downgrade(client));
}
