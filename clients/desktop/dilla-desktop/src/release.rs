//! One path that lets go of a client app-wide (integration 3, step 0 item 1).
//!
//! Every area that keeps a client (or a handle of it) in app-wide state drops it here, so the last `Rc<Client>` is the
//! caller's and its store can really close: Forget this device (Task 26: `close_and_forget` waits for the store) and
//! the sign-out both go through [`release_client`]. A window's own views (the shell) are the caller's to drop.

use std::rc::Rc;

use dilla_client::Client;
use gpui::App;

use crate::screens;

/// Lets go of `client` everywhere outside the window's own views: voice (and its call's media), attachments, the DM
/// picker and comparison, the message dialogs, the create flows, the ring banner, the search palette, threads, the
/// factor area's step-ups and registration.
pub fn release_client(client: &Rc<Client>, cx: &mut App) {
    screens::voice::release(client, cx);
    screens::attachments::ui::release(client, cx);
    screens::dms::picker::release(client, cx);
    screens::dms::compare::release(client, cx);
    screens::messages::ui::release(client, cx);
    screens::members::release(client, cx);
    screens::create::release(client, cx);
    screens::ring::release(client, cx);
    screens::search::release(client, cx);
    screens::threads::release(client, cx);
    screens::factor::release(client, cx);
    screens::factor::forget_instance(client, cx);
}
