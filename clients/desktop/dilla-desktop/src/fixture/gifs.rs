//! The GIF settings facet of a server-settings fixture: `AppState::server_admin.gifs`. Owned by Task 15; no other file maps
//! it. `server-settings/gifs-b` has a key set and the rating `r` (the same as the parity world's seed); every other
//! fixture has no key and the instance's default rating.

use dilla_client::state::AppState;
use dilla_client::state::server_admin::{GifRating, GifSettings};

pub(super) fn apply(name: &str, state: &mut AppState) {
    let settings = if name == "server-settings/gifs-b" {
        GifSettings {
            on: true,
            rating: GifRating::R,
        }
    } else {
        GifSettings {
            on: false,
            rating: GifRating::Pg13,
        }
    };
    let ids: Vec<_> = state.communities.iter().map(|c| c.id).collect();
    for id in ids {
        state.server_admin.gifs.insert(id, settings);
    }
}
