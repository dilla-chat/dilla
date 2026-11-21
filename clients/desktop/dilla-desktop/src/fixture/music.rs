//! The music facet of a fixture: a Bot's player (`ServerSpec::bot.music`), as the client holds it once it has read the
//! snapshot of a voice channel (`AppState::bot_admin::music`), and the catalog that pins the Bot user (`AppState::bot`), so
//! the "started music" card (Task 35) finds the Bot in the room. The web has no reference of it: a world with a player is
//! a GPUI-only fixture.
//!
//! The Bot user is the world's Bot person who is a member of the server. The catalog is the announcements facet's when it
//! made one; this facet adds the pin to a server whose Bot has the music capability alone.

use dilla_client::state::AppState;
use dilla_client::state::bot::{BotCatalogView, BotStatus, BotStatusState};
use dilla_client::state::bot_admin::{MusicSnapshot, MusicState, MusicTrack, Repeat};
use dilla_client::testing::spec::{TrackSpec, WorldSpec};

use super::Ids;

/// Maps the facet into `state`.
pub(super) fn apply(world: &WorldSpec, ids: Ids<'_>, state: &mut AppState) {
    for server in &world.servers {
        let Some(music) = server.bot.as_ref().and_then(|b| b.music.as_ref()) else {
            continue;
        };
        let community = ids.community(&server.id);
        let bot_user = world
            .people
            .iter()
            .find(|p| p.bot && server.members.contains(&p.id))
            .map(|p| ids.user(&p.id));
        match state.bot.catalogs.get_mut(&community) {
            Some(catalog) => catalog.bot_user = catalog.bot_user.or(bot_user),
            None => {
                state.bot.catalogs.insert(
                    community,
                    BotCatalogView {
                        revision: 1,
                        commands: Vec::new(),
                        status: BotStatus {
                            state: BotStatusState::Ready,
                            since: None,
                        },
                        bot_channels: None,
                        bot_user,
                        fingerprint: None,
                    },
                );
            }
        }
        let track = |t: &TrackSpec| MusicTrack {
            id: t.id.clone(),
            title: t.title.clone(),
            artist: t.artist.clone(),
            album: None,
            duration_ms: t.duration_ms,
            requester: t.requester.as_deref().map(|r| ids.user(r)),
        };
        state.bot_admin.music.insert(
            ids.channel(&format!("{}/{}", server.id, music.channel)),
            MusicState::Available(MusicSnapshot {
                revision: 1,
                track: music.track.as_ref().map(track),
                queue: music.queue.iter().map(track).collect(),
                queue_total: None,
                position_ms: music.position_ms,
                volume: music.volume,
                paused: music.paused,
                shuffle: false,
                repeat: Repeat::Off,
                can_control: music.can_control,
                // A stopped clock: the playhead stays where the fixture put it.
                clock: None,
            }),
        );
    }
}

#[cfg(test)]
mod tests {
    use dilla_client::state::TimelineItemState;
    use dilla_client::state::bot_admin::{MusicState, music_marker};

    use super::super::Fixture;

    fn load(name: &str) -> Fixture {
        let path = std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("../parity/fixtures/bot-admin")
            .join(format!("{name}.json"));
        Fixture::load(&path, None).unwrap_or_else(|e| panic!("{name}: {e}"))
    }

    /// The card fixtures: the Bot is pinned and in the room, the player is the one of the voice channel, and the Bot's
    /// message names that channel and Ada by the ids the world derives, so the card finds its room and its requester. The
    /// live fixture's marker names the track playing, the ended one's another.
    #[test]
    fn the_card_fixtures_hold_the_bot_its_room_and_its_player() {
        for (name, plays_it) in [("music-card-live", true), ("music-card-ended", false)] {
            let f = load(name);
            let state = &f.state;
            let community = state.communities[0].id;
            let room = state
                .channel_named(community, "harbour")
                .expect("the room")
                .id;
            let bot = state.bot.catalogs[&community]
                .bot_user
                .expect("the pinned Bot");
            assert!(
                state.voice.calls[&room]
                    .participants
                    .iter()
                    .any(|p| p.user_id == bot),
                "{name}: the Bot is in the room"
            );
            let general = state.channel_named(community, "general").unwrap().id;
            let announcement = state.timelines[&general]
                .items
                .iter()
                .find(|i| i.sender_user == Some(bot))
                .expect("the Bot's message");
            assert!(announcement.bot && announcement.state == TimelineItemState::Ok);
            let marker = music_marker(&announcement.body).expect("the marker");
            assert_eq!(marker.voice_channel, room, "{name}");
            let ada = state.members[&community]
                .iter()
                .find(|m| m.username == "ada")
                .unwrap();
            assert_eq!(marker.requester, ada.user_id, "{name}");
            let MusicState::Available(snapshot) = &state.bot_admin.music[&room] else {
                panic!("{name}: the player is read");
            };
            let playing = snapshot.track.as_ref().expect("a track").id == marker.item;
            assert_eq!(playing, plays_it, "{name}");
            assert!(snapshot.can_control && snapshot.queue.len() == 2);
        }
    }
}
