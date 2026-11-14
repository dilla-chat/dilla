//! The attachments facet of a fixture: `AppState::attachments` (downloads, thumbnails). The items' attachment rows are already set by the world. Owned by Task 12; no other file maps it.
//!
//! A drawn fixture has no client to open a thumbnail, so each image's thumbnail is made here the way the seeding
//! client made the one the web reference shows: the same picture (`fixture_picture`) through the same preview path
//! (`make_thumbnail`). The whole picture is kept too, for the lightbox.

use dilla_client::features::attachments::{RENDERABLE_IMAGE, fixture_picture, make_thumbnail};
use dilla_client::state::AppState;
use dilla_client::testing::spec::WorldSpec;

/// Maps the facet into `state`.
pub(super) fn apply(world: &WorldSpec, _viewer: &str, state: &mut AppState) {
    for m in &world.messages {
        for a in &m.attachments {
            let (Some(w), Some(h)) = (a.width, a.height) else {
                continue;
            };
            if !RENDERABLE_IMAGE.contains(&a.mime.as_str()) {
                continue;
            }
            let picture = fixture_picture(&a.name, w, h);
            if let Some(bytes) = make_thumbnail(&picture).and_then(|t| t.bytes) {
                state
                    .attachments
                    .fixture_thumbs
                    .insert((a.name.clone(), a.size), bytes);
            }
            // The lightbox shows the whole picture, the bytes the seeding client uploaded.
            state
                .attachments
                .fixture_pictures
                .insert((a.name.clone(), a.size), picture.into());
        }
    }
}
