//! The one music player state the now-playing views read (`packages/web/src/screens/voice/music.tsx`, `musicView`): the
//! Bot's authenticated music snapshot for one voice channel, read while the Bot is in that channel's call. Nothing here
//! invents a value the snapshot does not carry: the requester is the member the Bot recorded, and the playhead runs on only
//! from the Bot's own stamp (Ruling AI; an older Bot's snapshot stays static).
//!
//! [`view_of`] is the pure part (tested); [`dock`] draws the compact row in the shell's dock slot from a [`MusicFeed`] the
//! shell provides, and [`card`] draws the card variant.

use std::rc::Rc;
use std::sync::Arc;

use dilla_client::state::bot_admin::{MusicSnapshot, MusicState, MusicTrack, playhead_ms};
use dilla_client::state::{AppState, ChannelId, CommunityId, UserId};
use dilla_client::{Client, features::music::Music};
use dilla_desktop_ui::components::bot_admin::now_playing::{
    NowPlaying, NowPlayingLabels, NowPlayingVariant, PlayerTrack, QueueRow, up_next,
};
use dilla_desktop_ui::components::toast::show_toast;
use gpui::{AnyElement, App, Global, IntoElement, SharedString, Window};

use crate::strings::{t, tf};

/// What the dock needs to draw the player, which the shell (Task 8) and the voice area (Task 17) provide with
/// [`provide`] whenever this device is in a voice call with the Bot in it: the call's voice channel, its name, the Bot's
/// name, the clock the playhead runs on and the running client the controls call.
#[derive(Clone)]
pub struct MusicFeed {
    pub state: Arc<AppState>,
    pub client: Option<Rc<Client>>,
    pub community: CommunityId,
    /// The voice channel of this device's call.
    pub channel: ChannelId,
    /// The voice channel's name (the player's room).
    pub room: String,
    /// The Bot's name while it is in that call.
    pub bot: String,
    /// The device clock (unix ms) the playhead runs on: the shell's frozen clock in a fixture.
    pub now_ms: u64,
}

impl Global for MusicFeed {}

/// Hands the dock the call it draws, or takes it away (`None`: no call, or the Bot is not in it).
pub fn provide(feed: Option<MusicFeed>, cx: &mut App) {
    match feed {
        Some(feed) => cx.set_global(feed),
        None => {
            if cx.has_global::<MusicFeed>() {
                cx.remove_global::<MusicFeed>();
            }
        }
    }
}

/// `NowPlaying`'s props for one player, as `musicView` makes them.
#[derive(Clone, Debug, PartialEq)]
pub struct PlayerView {
    pub track: Option<PlayerTrack>,
    pub position_s: f64,
    pub playing: bool,
    pub meta: String,
    pub volume: Option<u8>,
    pub can_control: bool,
    pub blocked: String,
    pub queue: Vec<QueueRow>,
}

/// The member's name for a requester: the display name, else the username, else empty.
pub type NameOf<'a> = &'a dyn Fn(UserId) -> String;

/// `musicView`: the player of one channel. `state` is `None` while the Bot's snapshot is unavailable (it left, or the read
/// failed): the prototype's "{bot} left voice", and a press says the same.
pub fn view_of(
    state: Option<&MusicSnapshot>,
    room: &str,
    bot: &str,
    name_of: NameOf<'_>,
    in_call: bool,
    now_ms: u64,
) -> PlayerView {
    let Some(s) = state else {
        let left = tf("music.left", &[("bot", bot)]);
        return PlayerView {
            track: None,
            position_s: 0.0,
            playing: false,
            meta: left.clone(),
            volume: None,
            can_control: false,
            blocked: left,
            queue: Vec::new(),
        };
    };
    let requester = |track: &MusicTrack| track.requester.map(name_of).unwrap_or_default();
    let source = t("music.source.youtube");
    let current = s.track.as_ref();
    let meta = match current {
        None => tf("music.idleIn", &[("bot", bot), ("room", room)]),
        Some(track) if track.requester.is_none() => tf(
            "music.metaNoRequester",
            &[("source", source), ("room", room)],
        ),
        Some(track) => tf(
            "music.meta",
            &[
                ("source", source),
                ("name", &requester(track)),
                ("room", room),
            ],
        ),
    };
    let queue_row = |track: &MusicTrack, current: bool| QueueRow {
        title: track.title.clone().into(),
        by: requester(track).into(),
        duration_s: Some(track.duration_ms as f64 / 1000.0),
        current,
    };
    PlayerView {
        track: current.map(|track| PlayerTrack {
            title: track.title.clone().into(),
            duration_s: track.duration_ms as f64 / 1000.0,
        }),
        position_s: playhead_ms(s, now_ms) as f64 / 1000.0,
        playing: !s.paused,
        meta,
        volume: Some(s.volume),
        can_control: s.can_control,
        blocked: if in_call {
            t("music.blocked.dj").to_owned()
        } else {
            tf("music.blocked.join", &[("room", room)])
        },
        queue: current
            .map(|track| queue_row(track, true))
            .into_iter()
            .chain(s.queue.iter().map(|track| queue_row(track, false)))
            .collect(),
    }
}

/// The player's words, from the strings (`MUSIC_LABELS`); the queue toggle is filled with its count.
pub fn labels(up_next_count: usize) -> NowPlayingLabels {
    NowPlayingLabels {
        playing: t("music.playing").into(),
        paused: t("music.paused").into(),
        stopped: t("music.stopped").into(),
        idle: t("music.idle").into(),
        play: t("music.play").into(),
        pause: t("music.pause").into(),
        skip: t("music.skip").into(),
        stop: t("music.stop").into(),
        queue: tf("music.queue", &[("n", &up_next_count.to_string())]).into(),
    }
}

/// The member's name, as the web's `userName` finds it (the display name, else the username).
fn member_name(state: &AppState, community: CommunityId, user: UserId) -> String {
    state
        .members
        .get(&community)
        .and_then(|m| m.iter().find(|m| m.user_id == user))
        .map(|m| {
            if m.display.is_empty() {
                m.username.clone()
            } else {
                m.display.clone()
            }
        })
        .unwrap_or_default()
}

/// The controls of the player of `feed`: each runs the operation on the Bot and, when it is refused, says why in the
/// toast (`MusicPlayer`'s catch).
fn control(
    feed: &MusicFeed,
    run: impl Fn(
        &Music,
        ChannelId,
        bool,
    ) -> tokio::sync::oneshot::Receiver<
        Result<(), dilla_client::features::music::MusicError>,
    > + 'static,
    paused_now: bool,
) -> impl Fn(&mut Window, &mut App) + 'static {
    let (client, channel) = (feed.client.clone(), feed.channel);
    move |window: &mut Window, cx: &mut App| {
        let Some(client) = client.clone() else { return };
        let rx = run(&client.music(), channel, paused_now);
        window
            .spawn(cx, async move |cx| {
                if let Ok(Err(error)) = rx.await {
                    let key = error.toast_key();
                    cx.update(|_, cx| show_toast(t(key), cx)).ok();
                }
            })
            .detach();
    }
}

/// What the card calls when its queue toggle is pressed: whether the queue should now be open.
type QueueToggle = Rc<dyn Fn(bool, &mut Window, &mut App)>;

fn element(
    feed: &MusicFeed,
    variant: NowPlayingVariant,
    id: &'static str,
    queue_open: bool,
    on_queue: Option<QueueToggle>,
) -> Option<AnyElement> {
    let MusicState::Available(snapshot) = feed.state.bot_admin.music.get(&feed.channel)? else {
        return None;
    };
    let name_of = |user: UserId| member_name(&feed.state, feed.community, user);
    let view = view_of(
        Some(snapshot),
        &feed.room,
        &feed.bot,
        &name_of,
        true,
        feed.now_ms,
    );
    let paused = !view.playing;
    let mut player = NowPlaying::new(id, variant, labels(up_next(&view.queue)))
        .track(view.track.clone())
        .position(view.position_s)
        .playing(view.playing)
        .meta(view.meta.clone())
        .bot(feed.bot.clone())
        .can_control(view.can_control, view.blocked.clone())
        .queue(view.queue.clone(), queue_open)
        .on_toggle(control(
            feed,
            |m, ch, paused| m.pause(ch, !paused, None),
            paused,
        ))
        .on_skip(control(feed, |m, ch, _| m.skip(ch, None), paused))
        .on_stop(control(feed, |m, ch, _| m.stop(ch, None), paused));
    if let Some(volume) = view.volume {
        player = player.volume(tf("music.volume", &[("n", &volume.to_string())]));
    }
    if let Some(on_queue) = on_queue {
        player = player.on_queue_toggle(move |open, window, cx| on_queue(open, window, cx));
    }
    Some(player.into_any_element())
}

/// The compact row in the shell's dock (`slots::dock`, after the voice dock): `None` without a call, without the Bot in it
/// or while its music state is unavailable.
pub fn dock(_window: &mut Window, cx: &mut App) -> Option<AnyElement> {
    let feed = cx.try_global::<MusicFeed>()?.clone();
    element(
        &feed,
        NowPlayingVariant::Compact,
        "now-playing",
        false,
        None,
    )
}

/// The card variant of the same player (the message Glenn posts where `/play` ran).
pub fn card(window: &mut Window, cx: &mut App) -> Option<AnyElement> {
    let feed = cx.try_global::<MusicFeed>()?.clone();
    let open = window.use_keyed_state("now-playing-card-queue", cx, |_, _| false);
    let shown = *open.read(cx);
    let handle = open.clone();
    element(
        &feed,
        NowPlayingVariant::Card,
        "now-playing-card",
        shown,
        Some(Rc::new(move |next, _, cx| {
            handle.update(cx, |o, cx| {
                *o = next;
                cx.notify();
            })
        })),
    )
}

#[allow(dead_code)]
fn _shared(_: SharedString) {}

#[cfg(test)]
mod tests {
    use super::*;
    use dilla_client::state::bot_admin::{PlayheadClock, Repeat};

    fn track(id: &str, title: &str, requester: Option<UserId>) -> MusicTrack {
        MusicTrack {
            id: id.to_owned(),
            title: title.to_owned(),
            artist: "Artist".to_owned(),
            album: None,
            duration_ms: 214_000,
            requester,
        }
    }

    fn snapshot() -> MusicSnapshot {
        MusicSnapshot {
            revision: 3,
            track: Some(track("a", "Harbour Lights", Some(UserId([1; 16])))),
            queue: vec![track("b", "Second", None)],
            queue_total: None,
            position_ms: 37_000,
            volume: 75,
            paused: false,
            shuffle: false,
            repeat: Repeat::Off,
            can_control: true,
            clock: Some(PlayheadClock {
                elapsed_ms: 1_000,
                received_at_ms: 10_000,
            }),
        }
    }

    fn names(user: UserId) -> String {
        if user == UserId([1; 16]) {
            "Ada".to_owned()
        } else {
            String::new()
        }
    }

    #[test]
    fn a_playing_track_names_its_requester_and_runs_on_from_the_bots_stamp() {
        let v = view_of(Some(&snapshot()), "harbour", "Glenn", &names, true, 12_000);
        assert_eq!(v.meta, "YouTube · requested by Ada · harbour");
        assert!(v.playing && v.can_control);
        // 37 s + the Bot's 1 s + 2 s of local time since the snapshot arrived.
        assert_eq!(v.position_s, 40.0);
        assert_eq!(v.volume, Some(75));
        assert_eq!(v.queue.len(), 2);
        assert!(v.queue[0].current && !v.queue[1].current);
        assert_eq!(up_next(&v.queue), 1);
    }

    #[test]
    fn a_track_with_no_recorded_requester_leaves_the_name_out() {
        let mut s = snapshot();
        s.track = Some(track("a", "T", None));
        let v = view_of(Some(&s), "harbour", "Glenn", &names, true, 12_000);
        assert_eq!(v.meta, "YouTube · harbour");
    }

    #[test]
    fn an_idle_player_says_it_is_idle_in_the_room() {
        let mut s = snapshot();
        s.track = None;
        s.queue.clear();
        let v = view_of(Some(&s), "harbour", "Glenn", &names, true, 0);
        assert_eq!(
            v.meta,
            "Glenn is idle in harbour. /play starts the next one."
        );
        assert_eq!(v.track, None);
        assert!(v.queue.is_empty());
    }

    #[test]
    fn without_a_snapshot_the_bot_left_voice_and_a_press_says_so() {
        let v = view_of(None, "harbour", "Glenn", &names, true, 0);
        assert_eq!(v.meta, "Glenn left voice");
        assert_eq!(v.blocked, "Glenn left voice");
        assert!(!v.can_control && v.track.is_none());
    }

    #[test]
    fn without_control_a_press_says_join_the_room_or_ask_a_dj() {
        let mut s = snapshot();
        s.can_control = false;
        let out = view_of(Some(&s), "harbour", "Glenn", &names, false, 0);
        assert_eq!(out.blocked, "Join harbour to control playback");
        let inside = view_of(Some(&s), "harbour", "Glenn", &names, true, 0);
        assert_eq!(
            inside.blocked,
            "A DJ, owner, or administrator must control music in this voice channel."
        );
        assert!(!inside.can_control);
    }

    #[test]
    fn a_paused_player_holds_its_position() {
        let mut s = snapshot();
        s.paused = true;
        let v = view_of(Some(&s), "harbour", "Glenn", &names, true, 99_000);
        assert!(!v.playing);
        assert_eq!(v.position_s, 37.0);
    }

    #[test]
    fn the_queue_toggle_counts_what_is_up_next() {
        assert_eq!(labels(3).queue.as_ref(), "Up next · 3");
    }
}
