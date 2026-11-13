//! One path that lets go of a client app-wide (integration 3, step 0 item 1).
//!
//! Every area that keeps a client (or a handle of it) in app-wide state drops it here, so the last `Rc<Client>` is the
//! caller's and its store can really close: Forget this device (Task 26: `close_and_forget` waits for the store) and
//! the sign-out both go through [`release_client`]. A window's own views (the shell) are the caller's to drop.

use std::rc::Rc;

use dilla_client::Client;
use gpui::{App, Context, Entity, Global};

use crate::screens;

/// Lets go of `client` everywhere outside the window's own views: voice (and its call's media), attachments, the DM
/// picker and comparison, the message dialogs, the create flows, the ring banner, the search palette, threads, the
/// factor area's step-ups and registration.
pub fn release_client(client: &Rc<Client>, cx: &mut App) {
    // The views a window keeps between frames (its keyed state) let go first: without this they would hold the client
    // until the window draws again, which a hidden or minimized window may not do (review M5).
    let key = Rc::as_ptr(client) as usize;
    if cx.has_global::<Holders>() {
        let (gone, kept): (Vec<Holder>, Vec<Holder>) =
            std::mem::take(&mut cx.global_mut::<Holders>().0)
                .into_iter()
                .partition(|h| h.client == key);
        cx.global_mut::<Holders>().0 = kept;
        for h in gone {
            (h.forget)(cx);
        }
    }
    screens::voice::release(client, cx);
    screens::bot_admin::player::release(client, cx);
    screens::attachments::ui::release(client, cx);
    screens::dms::picker::release(client, cx);
    screens::dms::compare::release(client, cx);
    screens::messages::ui::release(client, cx);
    screens::messages::forget_memos();
    screens::members::release(client, cx);
    screens::create::release(client, cx);
    screens::ring::release(client, cx);
    screens::search::release(client, cx);
    screens::reactions::picker::release(client, cx);
    screens::ring::effects::release_notifier(cx);
    screens::threads::release(client, cx);
    screens::factor::release(client, cx);
}

/// A view kept between frames that holds a client, and how it lets go of it.
struct Holder {
    client: usize,
    entity: gpui::AnyWeakEntity,
    forget: Box<dyn FnOnce(&mut App)>,
}

#[derive(Default)]
struct Holders(Vec<Holder>);

impl Global for Holders {}

/// Registers `entity` as holding `client` (a section view a window keeps between frames): [`release_client`] calls
/// `forget` on it at once. Idempotent per entity and client; entries of views that are gone are dropped.
pub fn hold<T: 'static>(
    client: &Rc<Client>,
    entity: &Entity<T>,
    forget: impl FnOnce(&mut T, &mut Context<T>) + 'static,
    cx: &mut App,
) {
    let key = Rc::as_ptr(client) as usize;
    let id = entity.entity_id();
    let holders = cx.default_global::<Holders>();
    holders.0.retain(|h| h.entity.upgrade().is_some());
    if holders
        .0
        .iter()
        .any(|h| h.client == key && h.entity.entity_id() == id)
    {
        return;
    }
    let weak = entity.downgrade();
    holders.0.push(Holder {
        client: key,
        entity: weak.clone().into(),
        forget: Box::new(move |cx| {
            let _ = weak.update(cx, forget);
        }),
    });
}

/// How many views are registered as holding `client` (tests).
pub fn holders_of(client: &Rc<Client>, cx: &App) -> usize {
    let key = Rc::as_ptr(client) as usize;
    cx.try_global::<Holders>()
        .map_or(0, |h| h.0.iter().filter(|h| h.client == key).count())
}
