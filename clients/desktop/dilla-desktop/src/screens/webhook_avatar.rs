//! A webhook post's avatar (Task 28 item 15; web `conversation/webhook.ts` `useWebhookAvatar`): loaded only from the
//! instance's member route (`Bot::webhook_avatar`), never from a sender's URL, once per client, community and avatar id,
//! the newest [`KEPT`] kept. A miss (the instance no longer holds it, a failed read) is not kept, so the row shows the
//! placeholder avatar and a later draw asks again; a read in flight is not asked for twice.

use std::collections::{HashMap, VecDeque};
use std::rc::Rc;
use std::sync::Arc;

use dilla_client::Client;
use dilla_client::state::CommunityId;
use gpui::{App, Global, Image};

/// web `READ_KEPT`: avatars kept per page.
pub const KEPT: usize = 64;

/// A client's (its [`Client::serial`]) avatar of a community.
pub type Key = (u64, CommunityId, String);

/// What a read answers: the PNG's bytes, `None` when the instance holds none.
pub type Answer =
    tokio::sync::oneshot::Receiver<Result<Option<Vec<u8>>, dilla_client::ClientError>>;

#[derive(Default)]
struct Avatars {
    ready: HashMap<Key, Arc<Image>>,
    order: VecDeque<Key>,
    loading: std::collections::HashSet<Key>,
}

impl Global for Avatars {}

/// The avatar `avatar` of `community` as `client` reads it, once it is in; asks for it on the first call.
pub fn image(
    client: &Rc<Client>,
    community: CommunityId,
    avatar: &str,
    cx: &mut App,
) -> Option<Arc<Image>> {
    let key = (client.serial(), community, avatar.to_owned());
    if let Some(image) = cx.default_global::<Avatars>().ready.get(&key) {
        return Some(image.clone());
    }
    if !cx.global::<Avatars>().loading.contains(&key) {
        let rx = client.bot().webhook_avatar(community, avatar.to_owned());
        load(key, rx, cx);
    }
    None
}

/// The read of `key` has started (`rx`); its answer lands when it comes. The tests drive it.
pub fn load(key: Key, rx: Answer, cx: &mut App) {
    cx.default_global::<Avatars>().loading.insert(key.clone());
    cx.spawn(async move |cx| {
        let picture = match rx.await {
            Ok(Ok(Some(bytes))) => crate::screens::attachments::ui::picture(&bytes),
            _ => None,
        };
        cx.update(|cx| {
            let a = cx.default_global::<Avatars>();
            // Released meanwhile: nothing lands.
            if !a.loading.remove(&key) {
                return;
            }
            if let Some(picture) = picture {
                a.ready.insert(key.clone(), picture);
                a.order.push_back(key);
                while a.order.len() > KEPT {
                    if let Some(old) = a.order.pop_front() {
                        a.ready.remove(&old);
                    }
                }
                cx.refresh_windows();
            }
        });
    })
    .detach();
}

/// Whether `key`'s avatar is held.
pub fn held(key: &Key, cx: &App) -> bool {
    cx.try_global::<Avatars>()
        .is_some_and(|a| a.ready.contains_key(key))
}

/// Lets go of what `client` read (`release::release_client`).
pub fn release(client: &Rc<Client>, cx: &mut App) {
    forget_owner(client.serial(), cx);
}

/// [`release`] by the client's serial.
pub fn forget_owner(owner: u64, cx: &mut App) {
    if !cx.has_global::<Avatars>() {
        return;
    }
    let a = cx.global_mut::<Avatars>();
    a.ready.retain(|k, _| k.0 != owner);
    a.order.retain(|k| k.0 != owner);
    a.loading.retain(|k| k.0 != owner);
}

#[cfg(test)]
mod tests {
    use super::*;

    fn png() -> Vec<u8> {
        let img = image::RgbImage::from_pixel(4, 4, image::Rgb([9, 9, 9]));
        let mut out = std::io::Cursor::new(Vec::new());
        img.write_to(&mut out, image::ImageFormat::Png).unwrap();
        out.into_inner()
    }

    fn key(owner: u64, n: u8) -> Key {
        (owner, CommunityId([1; 16]), format!("{n:02x}").repeat(32))
    }

    /// Task 28 item 15: an avatar read lands and is kept; a miss is not kept; a release drops what that client read,
    /// a late answer included.
    #[gpui::test]
    fn an_avatar_lands_once_and_goes_with_its_client(cx: &mut gpui::TestAppContext) {
        let (tx, rx) = tokio::sync::oneshot::channel();
        cx.update(|cx| load(key(7, 1), rx, cx));
        tx.send(Ok(Some(png()))).unwrap();
        cx.run_until_parked();
        assert!(cx.update(|cx| held(&key(7, 1), cx)));

        let (tx, rx) = tokio::sync::oneshot::channel();
        cx.update(|cx| load(key(7, 2), rx, cx));
        tx.send(Ok(None)).unwrap();
        cx.run_until_parked();
        assert!(!cx.update(|cx| held(&key(7, 2), cx)), "a miss is not kept");

        let (tx, rx) = tokio::sync::oneshot::channel();
        cx.update(|cx| load(key(7, 3), rx, cx));
        cx.update(|cx| forget_owner(7, cx));
        tx.send(Ok(Some(png()))).unwrap();
        cx.run_until_parked();
        assert!(!cx.update(|cx| held(&key(7, 1), cx)), "released");
        assert!(
            !cx.update(|cx| held(&key(7, 3), cx)),
            "a late answer after the release lands nothing"
        );
    }
}
