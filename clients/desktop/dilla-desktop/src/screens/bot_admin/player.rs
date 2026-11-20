//! The one music player state the now-playing views read (`packages/web/src/screens/voice/music.tsx`, `musicView`): the
//! Bot's authenticated music snapshot for one voice channel, read while the Bot is in that channel's call. Nothing here
//! invents a value the snapshot does not carry: the requester is the member the Bot recorded, and the playhead runs on only
//! from the Bot's own stamp (Ruling AI; an older Bot's snapshot stays static).
//!
//! [`view_of`] is the pure part (tested); [`dock`] draws the compact row in the shell's dock slot from a [`MusicFeed`] the
//! shell provides, and [`music_card`] draws the card variant under the Bot's "started music" message.

use std::collections::{BTreeMap, BTreeSet};
use std::rc::Rc;
use std::sync::Arc;

use dilla_client::state::bot_admin::{MusicSnapshot, MusicState, MusicTrack, playhead_ms};
use dilla_client::state::{AppState, ChannelId, CommunityId, UserId};
use dilla_client::{Client, features::music::Music};
use dilla_desktop_ui::components::bot_admin::now_playing::{
    NowPlaying, NowPlayingLabels, NowPlayingVariant, PlayerTrack, QueueRow, up_next,
};
use dilla_desktop_ui::components::toast::show_toast_in;
use gpui::{AnyElement, App, IntoElement, ParentElement, SharedString, Styled, Window, div, px};

use crate::slots::{Extra, SlotCx};
use crate::strings::{t, tf};

/// What the dock needs to draw the player, which the shell's state loop keeps for its window ([`follow`]) whenever this
/// device is in a voice call with the Bot in it: the call's voice channel, its name, the Bot's name, the clock the
/// playhead runs on and the running client the controls call. The client is held weakly: a feed never keeps a released
/// client alive (final review M3).
#[derive(Clone)]
pub struct MusicFeed {
    pub state: Arc<AppState>,
    pub client: Option<std::rc::Weak<Client>>,
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

/// One window's player (final review C1: per window, as the voice area's state is): its feed and its track-end read.
#[derive(Default)]
struct WindowMusic {
    feed: Option<MusicFeed>,
    track_end: TrackEnd,
}

/// The feed `window`'s dock draws, if it has one.
pub fn feed(window: gpui::WindowId, cx: &App) -> Option<MusicFeed> {
    crate::per_window::get::<WindowMusic>(window, cx).and_then(|m| m.feed.clone())
}

/// The feed of this device's call (integration 3, step 5): `Some` while it is in a call (phase In) whose participants
/// include the community's pinned Bot user; the room is the call's channel name, the Bot its catalog name.
pub fn feed_of(
    state: &Arc<AppState>,
    client: Option<std::rc::Weak<Client>>,
    now_ms: u64,
) -> Option<MusicFeed> {
    let call = &state.voice.call;
    if call.phase != dilla_client::calls::CallPhase::In {
        return None;
    }
    let channel = call.channel?;
    let room = state.channel(channel)?;
    let (community, bot_user) = bot_in_room(state, channel)?;
    Some(MusicFeed {
        state: state.clone(),
        client,
        community,
        channel,
        room: room.name.clone(),
        bot: member_name(state, community, bot_user),
        now_ms,
    })
}

/// The community and the pinned Bot user when that Bot is in the voice channel's call (the web's `botInRoom`: a participant
/// of the call is the catalog's `bot_user`).
fn bot_in_room(state: &AppState, channel: ChannelId) -> Option<(CommunityId, UserId)> {
    let community = state.channel(channel)?.community_id;
    let bot_user = state.bot.catalogs.get(&community)?.bot_user?;
    state
        .voice
        .calls
        .get(&channel)?
        .participants
        .iter()
        .any(|p| p.user_id == bot_user)
        .then_some((community, bot_user))
}

/// When the player is read again for the playing track's end: a second after it should end (music.tsx), `None` while
/// paused, without a track or without a length.
pub fn refresh_after_ms(snapshot: &MusicSnapshot, now_ms: u64) -> Option<u64> {
    let track = snapshot.track.as_ref()?;
    if snapshot.paused || track.duration_ms == 0 {
        return None;
    }
    Some(
        track
            .duration_ms
            .saturating_sub(playhead_ms(snapshot, now_ms))
            + 1_000,
    )
}

/// The track end's read, scheduled for one player's snapshot revision.
#[derive(Default)]
struct TrackEnd {
    key: Option<(ChannelId, u64)>,
    _timer: Option<gpui::Task<()>>,
}

fn device_now_ms() -> u64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map_or(0, |d| d.as_millis() as u64)
}

/// The shell's state loop (integration 3, step 5), for its own window: keeps the dock's feed for `state` ([`feed_of`]),
/// reads the player when this window's call with the Bot starts (op 63 reads it again on the client), and again a second
/// after its track should end. Another window's loop never touches this window's feed (final review C1). Everything here
/// holds the client weakly: a release never waits for it.
pub fn follow(window: gpui::WindowId, state: &Arc<AppState>, client: &Rc<Client>, cx: &mut App) {
    let weak = Rc::downgrade(client);
    let mut read = |channel: ChannelId, _: &mut App| {
        if let Some(client) = weak.upgrade() {
            drop(client.music().refresh(channel));
        }
    };
    follow_in(
        window,
        state,
        Some(Rc::downgrade(client)),
        device_now_ms(),
        &mut read,
        cx,
    );
}

/// [`follow`] with the read it sends (`read`) and the clock it reads, so a test can count the reads.
fn follow_in(
    window: gpui::WindowId,
    state: &Arc<AppState>,
    client: Option<std::rc::Weak<Client>>,
    now_ms: u64,
    read: &mut dyn FnMut(ChannelId, &mut App),
    cx: &mut App,
) {
    let next = feed_of(state, client.clone(), now_ms);
    let before = feed(window, cx).map(|f| f.channel);
    let Some(channel) = next.as_ref().map(|f| f.channel) else {
        crate::per_window::set::<WindowMusic>(window, None, cx);
        return;
    };
    crate::per_window::get_mut::<WindowMusic>(window, cx).feed = next;
    if before != Some(channel) {
        read(channel, cx);
    }
    let snapshot = match state.bot_admin.music.get(&channel) {
        Some(MusicState::Available(s)) => Some(s),
        _ => None,
    };
    let key = snapshot.map(|s| (channel, s.revision));
    if crate::per_window::get_mut::<WindowMusic>(window, cx)
        .track_end
        .key
        == key
    {
        return;
    }
    let timer = snapshot
        .and_then(|s| refresh_after_ms(s, now_ms))
        .and_then(|ms| client.clone().map(|weak| (ms, weak)))
        .map(|(ms, weak)| {
            cx.spawn(async move |cx| {
                cx.background_executor()
                    .timer(std::time::Duration::from_millis(ms))
                    .await;
                if let Some(client) = weak.upgrade() {
                    drop(client.music().refresh(channel));
                }
            })
        });
    crate::per_window::get_mut::<WindowMusic>(window, cx).track_end =
        TrackEnd { key, _timer: timer };
}

/// The release path (integration 3, step 0 item 1): every window's feed of `client` goes, with its track-end read, and so
/// do the cards' reads.
pub fn release(client: &Rc<Client>, cx: &mut App) {
    crate::per_window::retain::<WindowCards>(cx, |_, c| {
        !c.seen
            .as_ref()
            .and_then(|s| s.client.as_ref())
            .is_some_and(|w| std::ptr::eq(w.as_ptr(), Rc::as_ptr(client)))
    });
    crate::per_window::retain::<WindowMusic>(cx, |_, m| {
        !m.feed
            .as_ref()
            .and_then(|f| f.client.as_ref())
            .is_some_and(|c| std::ptr::eq(c.as_ptr(), Rc::as_ptr(client)))
    });
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

/// The controls of the player of `channel`: each runs the operation on the Bot and, when it is refused, says why in the
/// toast (`MusicPlayer`'s catch).
fn control(
    client: Option<std::rc::Weak<Client>>,
    channel: ChannelId,
    run: impl Fn(
        &Music,
        ChannelId,
        bool,
    ) -> tokio::sync::oneshot::Receiver<
        Result<(), dilla_client::features::music::MusicError>,
    > + 'static,
    paused_now: bool,
) -> impl Fn(&mut Window, &mut App) + 'static {
    move |window: &mut Window, cx: &mut App| {
        let Some(client) = client.as_ref().and_then(std::rc::Weak::upgrade) else {
            return;
        };
        let rx = run(&client.music(), channel, paused_now);
        window
            .spawn(cx, async move |cx| {
                if let Ok(Err(error)) = rx.await {
                    let key = error.toast_key();
                    cx.update(|window, cx| {
                        show_toast_in(Some(window.window_handle().window_id()), t(key), cx)
                    })
                    .ok();
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
            feed.client.clone(),
            feed.channel,
            |m, ch, paused| m.pause(ch, !paused, None),
            paused,
        ))
        .on_skip(control(
            feed.client.clone(),
            feed.channel,
            |m, ch, _| m.skip(ch, None),
            paused,
        ))
        .on_stop(control(
            feed.client.clone(),
            feed.channel,
            |m, ch, _| m.stop(ch, None),
            paused,
        ));
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
pub fn dock(window: &mut Window, cx: &mut App) -> Option<AnyElement> {
    let feed = feed(window.window_handle().window_id(), cx)?;
    element(
        &feed,
        NowPlayingVariant::Compact,
        "now-playing",
        false,
        None,
    )
}

/// What the card shows of one player, as `useMusicCard` tells them apart.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum CardPhase {
    /// The Bot is not in that voice channel's call (or its player cannot be read): "{bot} left voice".
    Left,
    /// The Bot is in the room but no snapshot has been read yet: neutral, never "left voice" for a moment.
    Neutral,
    /// The announced track is no longer the one playing.
    Ended,
    /// The announced track is the one playing: the live player.
    Live,
}

/// `useMusicCard`: the card's phase and view for the announcement of queue item `item`, in `room`. `state` is what the
/// client holds of the player of that voice channel (`None`: nothing read yet).
#[allow(clippy::too_many_arguments)]
pub fn card_view_of(
    bot_in_room: bool,
    state: Option<&MusicState>,
    item: &str,
    room: &str,
    bot: &str,
    name_of: NameOf<'_>,
    in_call: bool,
    now_ms: u64,
) -> (CardPhase, PlayerView) {
    let quiet = |meta: String, blocked: String| PlayerView {
        track: None,
        position_s: 0.0,
        playing: false,
        meta,
        volume: None,
        can_control: false,
        blocked,
        queue: Vec::new(),
    };
    if !bot_in_room {
        return (
            CardPhase::Left,
            view_of(None, room, bot, name_of, in_call, now_ms),
        );
    }
    match state {
        None => (CardPhase::Neutral, quiet(room.to_owned(), String::new())),
        // `musicView` with a status that is not `available`.
        Some(MusicState::Unavailable) => (
            CardPhase::Left,
            view_of(None, room, bot, name_of, in_call, now_ms),
        ),
        Some(MusicState::Available(s)) if s.track.as_ref().is_none_or(|t| t.id != item) => {
            let ended = tf("music.ended", &[("bot", bot), ("room", room)]);
            (CardPhase::Ended, quiet(ended.clone(), ended))
        }
        Some(MusicState::Available(s)) => (
            CardPhase::Live,
            view_of(Some(s), room, bot, name_of, in_call, now_ms),
        ),
    }
}

/// The card's gap above it (MessageRow.css `.d-message-row__card`'s `margin-top`).
const CARD_MARGIN: f32 = 6.0;

/// The card's width: NowPlaying.css's 440px inside its 1px edge.
const CARD_WIDTH: f32 = 442.0;

/// One window's cards: what the last draw of a "started music" row saw, and the players this window reads for the cards on
/// its screen.
#[derive(Default)]
struct WindowCards {
    seen: Option<CardsSeen>,
    /// The voice channels with a card on screen and the Bot in the room, as the last frame had them.
    reading: BTreeSet<ChannelId>,
    /// Each read channel's read again for the track's end.
    track_end: BTreeMap<ChannelId, TrackEnd>,
}

/// What a card's draw leaves for [`card_on_screen`], which the timeline calls after the frame's rows: the state they were
/// drawn from, the client they act on and the clock the playhead runs on.
#[derive(Clone)]
struct CardsSeen {
    state: Arc<AppState>,
    client: Option<std::rc::Weak<Client>>,
    now_ms: u64,
}

/// The device clock the playhead runs on: the shell's frozen one in a fixture.
fn clock_ms(clock: crate::screens::shell::Clock) -> u64 {
    match clock {
        crate::screens::shell::Clock::Local => device_now_ms(),
        crate::screens::shell::Clock::Fixed { now, .. } => now * 1000,
    }
}

/// The card under the Bot's "started music" message (MessageItem.tsx `MusicMessageCard`): `None` for any other message or
/// one that is not shown (the card shows for a readable row only). `last` is whether the row is the log's last, which hides
/// its empty attachments slot: the web's card is a grid item with the grid's gap and its own 6px margin in front of that
/// slot, so the card sits where it does whichever the row is.
pub fn music_card(
    item: &dilla_client::state::TimelineItem,
    ctx: &SlotCx,
    last: bool,
    window: &mut Window,
    cx: &mut App,
) -> Option<Extra> {
    use dilla_client::state::TimelineItemState as S;
    if !matches!(item.state, S::Ok | S::Pending | S::Failed) {
        return None;
    }
    let community = ctx.community?;
    let marker = crate::screens::timeline::announcement_in(&ctx.state, community, item)?;
    let state = &ctx.state;
    let channel = marker.voice_channel;
    let now_ms = clock_ms(ctx.clock);
    crate::per_window::get_mut::<WindowCards>(window.window_handle().window_id(), cx).seen =
        Some(CardsSeen {
            state: state.clone(),
            client: ctx.client.as_ref().map(Rc::downgrade),
            now_ms,
        });
    let room = state
        .channels
        .iter()
        .find(|c| c.id == channel && c.community_id == community)
        .map_or_else(|| t("music.someRoom").to_owned(), |c| c.name.clone());
    let members = state.members.get(&community).map_or(&[][..], Vec::as_slice);
    let bot = item
        .sender_user
        .map(|u| crate::screens::timeline::user_name(state, members, u))
        .unwrap_or_default();
    let name_of = |user: UserId| member_name(state, community, user);
    let call = &state.voice.call;
    let in_call =
        call.phase != dilla_client::calls::CallPhase::Idle && call.channel == Some(channel);
    let (phase, view) = card_view_of(
        bot_in_room(state, channel).is_some(),
        state.bot_admin.music.get(&channel),
        &marker.item,
        &room,
        &bot,
        &name_of,
        in_call,
        now_ms,
    );
    let key = SharedString::from(item.key.clone());
    let open = window.use_keyed_state(
        (gpui::ElementId::from("now-playing-card-queue"), key.clone()),
        cx,
        |_, _| false,
    );
    let queue_open = *open.read(cx);
    let mut card_labels = labels(up_next(&view.queue));
    match phase {
        CardPhase::Neutral => card_labels.idle = t("music.region").into(),
        CardPhase::Ended => card_labels.idle = t("music.finished").into(),
        CardPhase::Left | CardPhase::Live => {}
    }
    let paused = !view.playing;
    let client = ctx.client.as_ref().map(Rc::downgrade);
    let mut player = NowPlaying::new(
        (gpui::ElementId::from("music-card"), key),
        NowPlayingVariant::Card,
        card_labels,
    )
    .track(view.track.clone())
    .position(view.position_s)
    .playing(view.playing)
    .meta(view.meta.clone())
    .bot(bot)
    .can_control(view.can_control, view.blocked.clone())
    .queue(view.queue.clone(), queue_open)
    .on_toggle(control(
        client.clone(),
        channel,
        |m, ch, paused| m.pause(ch, !paused, None),
        paused,
    ))
    .on_skip(control(
        client.clone(),
        channel,
        |m, ch, _| m.skip(ch, None),
        paused,
    ))
    .on_stop(control(
        client,
        channel,
        |m, ch, _| m.stop(ch, None),
        paused,
    ))
    .on_queue_toggle(move |next, _, cx| {
        open.update(cx, |o, cx| {
            *o = next;
            cx.notify();
        })
    });
    if let Some(volume) = view.volume {
        player = player.volume(tf("music.volume", &[("n", &volume.to_string())]));
    }
    // The part's box starts under the attachments slot (and the grid's gap): the card's own margin less the slot.
    let pad = CARD_MARGIN - crate::screens::timeline::attachments_slot(last);
    Some(Extra {
        // A block, which takes the card's 442px whatever its text (the extras sit in an absolute box that shrinks to its
        // content, so the width is set; a log narrower than that is not handled).
        element: div()
            .flex()
            .flex_col()
            .w(px(CARD_WIDTH))
            .pt(px(pad))
            .child(player)
            .into_any_element(),
        height: CARD_MARGIN + NowPlaying::card_height(view.queue.len(), queue_open),
    })
}

/// Called once per frame by the timeline of `window` with the voice channels of the "started music" rows that intersect its
/// viewport: the card reads the player only while the Bot is in that room and the card is on screen (Decision D10, as
/// music.tsx's `botInRoom && visible`). A channel that has just come on screen is read; each one is read again a second
/// after its track should end ([`refresh_after_ms`]); and the client is told which channels the windows watch, for the Bot's
/// hints of them (`set_watched`, the union over this client's windows).
pub fn card_on_screen(window: gpui::WindowId, channels: BTreeSet<ChannelId>, cx: &mut App) {
    let Some(seen) = crate::per_window::get::<WindowCards>(window, cx).and_then(|c| c.seen.clone())
    else {
        return;
    };
    let Some(client) = seen.client.as_ref().and_then(std::rc::Weak::upgrade) else {
        return;
    };
    let reading: BTreeSet<ChannelId> = channels
        .into_iter()
        .filter(|ch| bot_in_room(&seen.state, *ch).is_some())
        .collect();
    let before = std::mem::replace(
        &mut crate::per_window::get_mut::<WindowCards>(window, cx).reading,
        reading.clone(),
    );
    for channel in reading.difference(&before) {
        drop(client.music().refresh(*channel));
    }
    for channel in &reading {
        let snapshot = match seen.state.bot_admin.music.get(channel) {
            Some(MusicState::Available(s)) => Some(s),
            _ => None,
        };
        let key = snapshot.map(|s| (*channel, s.revision));
        let kept = crate::per_window::get_mut::<WindowCards>(window, cx)
            .track_end
            .get(channel)
            .is_some_and(|t| t.key == key);
        if kept {
            continue;
        }
        let weak = Rc::downgrade(&client);
        let channel = *channel;
        let timer = snapshot
            .and_then(|s| refresh_after_ms(s, seen.now_ms))
            .map(|ms| {
                cx.spawn(async move |cx| {
                    cx.background_executor()
                        .timer(std::time::Duration::from_millis(ms))
                        .await;
                    if let Some(client) = weak.upgrade() {
                        drop(client.music().refresh(channel));
                    }
                })
            });
        crate::per_window::get_mut::<WindowCards>(window, cx)
            .track_end
            .insert(channel, TrackEnd { key, _timer: timer });
    }
    crate::per_window::get_mut::<WindowCards>(window, cx)
        .track_end
        .retain(|ch, _| reading.contains(ch));
    let mut watched = BTreeSet::new();
    crate::per_window::each_mut::<WindowCards>(cx, |_, cards| {
        let ours = cards
            .seen
            .as_ref()
            .and_then(|s| s.client.as_ref())
            .is_some_and(|c| std::ptr::eq(c.as_ptr(), Rc::as_ptr(&client)));
        if ours {
            watched.extend(cards.reading.iter().copied());
        }
    });
    client.music().set_watched(watched);
}

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

    /// A community with a voice channel "Lounge" whose call holds this device and, with `bot_in`, the pinned Bot "Mr. Kim".
    fn in_call(bot_in: bool, phase: dilla_client::calls::CallPhase) -> (AppState, ChannelId) {
        use dilla_client::state::bot::{BotCatalogView, BotStatus, BotStatusState};
        use dilla_client::state::voice::{VoiceCall, VoiceParticipant};
        let (community, channel, bot) = (CommunityId([7; 16]), ChannelId([8; 16]), UserId([9; 16]));
        let mut s = AppState::default();
        s.channels.push(dilla_client::state::channels::Channel {
            id: channel,
            community_id: community,
            kind: 1,
            mode: 0,
            name: "Lounge".into(),
            topic: String::new(),
            parent_id: None,
            position: 0,
            slowmode: 0,
            text_group_id: None,
            group: Default::default(),
        });
        s.bot.catalogs.insert(
            community,
            BotCatalogView {
                revision: 1,
                commands: Vec::new(),
                status: BotStatus {
                    state: BotStatusState::Ready,
                    since: None,
                },
                bot_channels: None,
                bot_user: Some(bot),
                fingerprint: None,
            },
        );
        let me = VoiceParticipant {
            user_id: UserId([1; 16]),
            device_id: dilla_client::state::DeviceId([1; 16]),
            flags: 0,
            own: true,
        };
        let mut participants = vec![me];
        if bot_in {
            participants.push(VoiceParticipant {
                user_id: bot,
                device_id: dilla_client::state::DeviceId([9; 16]),
                flags: 0,
                own: false,
            });
        }
        s.voice.calls.insert(
            channel,
            VoiceCall {
                call_id: [3; 16],
                participants,
            },
        );
        // The renamed Bot (`PATCH /bot/profile`): its member row names it.
        s.members.insert(
            community,
            vec![dilla_client::state::Member {
                user_id: bot,
                username: "bot-7f3a".into(),
                display: "Mr. Kim".into(),
                kind: 1,
                role_ids: Vec::new(),
                status_msg: String::new(),
                bio: String::new(),
            }],
        );
        s.voice.call.phase = phase;
        s.voice.call.channel = Some(channel);
        s.voice.call.community = Some(community);
        (s, channel)
    }

    /// Integration 3, step 5: the shell provides the dock's feed exactly while this device is in a call with the
    /// community's pinned Bot in it: the call's channel, its name and the Bot's name.
    #[test]
    fn the_feed_follows_a_call_with_the_bot_in_it() {
        use dilla_client::calls::CallPhase;
        let (s, channel) = in_call(true, CallPhase::In);
        let feed = feed_of(&Arc::new(s), None, 5).expect("in a call with the Bot");
        assert_eq!(feed.channel, channel);
        assert_eq!(feed.community, CommunityId([7; 16]));
        assert_eq!(
            (feed.room.as_str(), feed.bot.as_str()),
            ("Lounge", "Mr. Kim")
        );
        assert_eq!(feed.now_ms, 5);
        let (s, _) = in_call(false, CallPhase::In);
        assert!(
            feed_of(&Arc::new(s), None, 5).is_none(),
            "the Bot is not in the call"
        );
        let (s, _) = in_call(true, CallPhase::Joining);
        assert!(
            feed_of(&Arc::new(s), None, 5).is_none(),
            "not in the call yet"
        );
        let (mut s, _) = in_call(true, CallPhase::In);
        s.bot.catalogs.clear();
        assert!(feed_of(&Arc::new(s), None, 5).is_none(), "no pinned Bot");
    }

    /// Integration 3, final review C1: each window keeps its own feed and track-end read. Window B's publishes (no call,
    /// or a call of its own) never touch window A's feed, so A's dock keeps its room and its controls the client of A's
    /// feed, and A's player is read once when its call starts, however often B publishes.
    #[gpui::test]
    fn each_window_keeps_its_own_music_feed(cx: &mut gpui::TestAppContext) {
        use dilla_client::calls::CallPhase;
        let a = cx
            .add_empty_window()
            .update(|w, _| w.window_handle().window_id());
        let b = cx
            .add_empty_window()
            .update(|w, _| w.window_handle().window_id());
        let (in_a, a_channel) = in_call(true, CallPhase::In);
        let in_a = Arc::new(in_a);
        let idle_b = Arc::new(AppState::default());
        let (mut in_b, _) = in_call(true, CallPhase::In);
        let b_channel = ChannelId([5; 16]);
        let lounge = in_b.voice.calls.remove(&ChannelId([8; 16])).unwrap();
        in_b.voice.calls.insert(b_channel, lounge);
        in_b.channels[0].id = b_channel;
        in_b.channels[0].name = "Den".into();
        in_b.voice.call.channel = Some(b_channel);
        let in_b = Arc::new(in_b);
        let reads = std::cell::RefCell::new(Vec::new());
        cx.update(|cx| {
            let mut read = |ch: ChannelId, _: &mut App| reads.borrow_mut().push(ch);
            follow_in(a, &in_a, None, 1, &mut read, cx);
            for _ in 0..5 {
                follow_in(b, &idle_b, None, 1, &mut read, cx);
                follow_in(a, &in_a, None, 1, &mut read, cx);
            }
            let fa = feed(a, cx).expect("A's feed");
            assert_eq!((fa.channel, fa.room.as_str()), (a_channel, "Lounge"));
            assert!(feed(b, cx).is_none(), "B has no call");
            follow_in(b, &in_b, None, 1, &mut read, cx);
            let fa = feed(a, cx).expect("A's feed");
            let fb = feed(b, cx).expect("B's feed");
            assert_eq!((fa.channel, fa.room.as_str()), (a_channel, "Lounge"));
            assert_eq!((fb.channel, fb.room.as_str()), (b_channel, "Den"));
        });
        assert_eq!(
            *reads.borrow(),
            vec![a_channel, b_channel],
            "one read per call start"
        );
    }

    /// Integration 3, step 5: the player is read again a second after the playing track should end (music.tsx), never
    /// while paused or without a track.
    #[test]
    fn the_player_is_read_again_a_second_after_the_track_ends() {
        let s = snapshot();
        // At 10 s the playhead is 37 s + 1 s; the track is 214 s long.
        assert_eq!(refresh_after_ms(&s, 10_000), Some(214_000 - 38_000 + 1_000));
        let mut paused = snapshot();
        paused.paused = true;
        assert_eq!(refresh_after_ms(&paused, 10_000), None);
        let mut none = snapshot();
        none.track = None;
        assert_eq!(refresh_after_ms(&none, 10_000), None);
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

    /// The card of a Bot that is not in the voice channel's call: "{bot} left voice", no control.
    #[test]
    fn the_card_says_the_bot_left_when_it_is_not_in_the_room() {
        let (phase, v) = card_view_of(false, None, "i1", "harbour", "Glenn", &names, false, 0);
        assert_eq!(phase, CardPhase::Left);
        assert_eq!(v.meta, "Glenn left voice");
        assert!(!v.can_control);
        // A player that cannot be read while the Bot is in the room counts as the same (`status !== 'available'`).
        let unreadable = MusicState::Unavailable;
        let (phase, v) = card_view_of(
            true,
            Some(&unreadable),
            "i1",
            "harbour",
            "Glenn",
            &names,
            false,
            0,
        );
        assert_eq!(
            (phase, v.meta.as_str()),
            (CardPhase::Left, "Glenn left voice")
        );
    }

    #[test]
    fn the_card_is_neutral_before_the_first_read() {
        let (phase, v) = card_view_of(true, None, "i1", "harbour", "Glenn", &names, false, 0);
        assert_eq!(phase, CardPhase::Neutral);
        assert_eq!(v.meta, "harbour");
        assert!(v.track.is_none() && v.queue.is_empty() && !v.can_control);
    }

    #[test]
    fn a_card_whose_track_no_longer_plays_says_it_ended() {
        let mut s = snapshot();
        s.track = Some(track("i2", "Another", None));
        let other = MusicState::Available(s);
        let (phase, v) = card_view_of(
            true,
            Some(&other),
            "i1",
            "harbour",
            "Glenn",
            &names,
            true,
            0,
        );
        assert_eq!(phase, CardPhase::Ended);
        assert_eq!(v.meta, "Glenn played this in harbour");
        assert_eq!(v.blocked, v.meta);
        assert!(v.queue.is_empty() && v.track.is_none());
        let mut s = snapshot();
        s.track = None;
        let none = MusicState::Available(s);
        let (phase, v) = card_view_of(true, Some(&none), "i1", "harbour", "Glenn", &names, true, 0);
        assert_eq!(phase, CardPhase::Ended);
        assert_eq!(v.meta, "Glenn played this in harbour");
    }

    #[test]
    fn the_announced_track_playing_is_live_and_gated_by_dj() {
        let mut s = snapshot();
        s.track = Some(track("i1", "Harbour Lights", Some(UserId([1; 16]))));
        s.can_control = false;
        let playing = MusicState::Available(s);
        let (phase, v) = card_view_of(
            true,
            Some(&playing),
            "i1",
            "harbour",
            "Glenn",
            &names,
            false,
            12_000,
        );
        assert_eq!(phase, CardPhase::Live);
        assert_eq!(v.blocked, "Join harbour to control playback");
        assert!(!v.can_control && v.track.is_some());
        let (_, v) = card_view_of(
            true,
            Some(&playing),
            "i1",
            "harbour",
            "Glenn",
            &names,
            true,
            12_000,
        );
        assert_eq!(v.blocked, t("music.blocked.dj"));
    }

    /// Decision D10: the card's Bot-in-room test is the dock's (a participant of the call is the pinned Bot user).
    #[test]
    fn the_bot_is_in_the_room_when_the_pinned_bot_is_in_its_call() {
        use dilla_client::calls::CallPhase;
        let (s, channel) = in_call(true, CallPhase::In);
        assert_eq!(
            bot_in_room(&s, channel),
            Some((CommunityId([7; 16]), UserId([9; 16])))
        );
        let (s, channel) = in_call(false, CallPhase::In);
        assert_eq!(bot_in_room(&s, channel), None);
    }
}
