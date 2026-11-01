// Task 8b (Signal README §6.6, §9): the one music player state the three NowPlaying views read. It is the Bot's authenticated
// music snapshot for one voice channel (client-core's per-community adapter keeps one store per channel), read while the Bot is
// in that channel's call. Nothing here invents a value the snapshot does not carry: the requester is the member the Bot
// recorded, and the playhead runs on only from the Bot's own stamp (Ruling AI; an older Bot's snapshot stays static).
import { useCallback, useEffect, useRef, useState } from 'react';
import { createVoiceMusicAdapter, musicPlayheadMs, type CoreClient, type MemberSummary, type TrustSummary, type VoiceMusicAdapter,
  type VoiceMusicState, type VoiceMusicTrack } from '@dilla/client-core';
import { NowPlaying, type NowPlayingProps } from '@dilla/ui';
import { en, t } from '../../strings/index.ts';
import { userName } from '../shell-model.ts';
import { useSlice, useSlices } from '../../core/use-slice.ts';

/** NowPlaying's props for one player, without the variant; a control returns the adapter's promise (MusicPlayer reports a refusal). */
export type MusicPlayerView = Omit<NowPlayingProps, 'variant' | 'onToggle' | 'onSkip' | 'onStop'> & {
  onToggle: () => Promise<void> | void; onSkip: () => Promise<void> | void; onStop: () => Promise<void> | void;
};

/**
 * Fix rounds 1–2 (item 3): the Bot has no push for music changes (interaction responses answer requests; nothing streams
 * state), so an open player re-reads the snapshot as a fallback: every 25 s while the page is visible, once when it becomes
 * visible again, and once a second after the playing track should end. One schedule per channel, however many views read it.
 */
export const MUSIC_REFRESH_MS = 25_000;

export const MUSIC_LABELS = (): NowPlayingProps['labels'] => ({
  region: t('music.region'), playing: t('music.playing'), paused: t('music.paused'), stopped: t('music.stopped'), idle: t('music.idle'),
  play: t('music.play'), pause: t('music.pause'), skip: t('music.skip'), stop: t('music.stop'),
  // The templates go as they are: NowPlaying fills {pos}, {dur} and {n}.
  position: t('music.position'), positionValue: en['music.positionValue'], queue: en['music.queue'], upNext: en['music.upNext'],
  volume: en['music.volume'], volumeName: t('music.volumeName'), queueList: t('music.queueList'),
});

function adapterFor(client: CoreClient, communityId: string | null): VoiceMusicAdapter | null {
  if (communityId === null) return null;
  try { return createVoiceMusicAdapter(client, communityId); } catch { return null; }
}

interface Refresher { count: number; timer: ReturnType<typeof setInterval>; end: ReturnType<typeof setTimeout> | null; refresh(): void }
const refreshers = new Map<VoiceMusicAdapter, Map<string, Refresher>>();
const hidden = () => typeof document !== 'undefined' && document.hidden;
let visibilityBound = false;
/** A page that becomes visible again re-reads every held channel once. */
function bindVisibility(): void {
  if (visibilityBound || typeof document === 'undefined') return;
  visibilityBound = true;
  document.addEventListener('visibilitychange', () => {
    if (hidden()) return;
    for (const channels of refreshers.values()) for (const row of channels.values()) row.refresh();
  });
}
/** One schedule per (adapter, channel) while any view holds it; returns the release and the track-end scheduler. */
function holdRefresh(adapter: VoiceMusicAdapter, channelId: string): { release(): void; endIn(ms: number | null): void } {
  bindVisibility();
  let channels = refreshers.get(adapter);
  if (channels === undefined) { channels = new Map(); refreshers.set(adapter, channels); }
  let row = channels.get(channelId);
  if (row === undefined) {
    const refresh = () => { void adapter.refresh(channelId).catch(() => undefined); };
    row = { count: 0, end: null, refresh, timer: setInterval(() => { if (!hidden()) refresh(); }, MUSIC_REFRESH_MS) };
    channels.set(channelId, row);
  }
  row.count++;
  const held = row;
  return {
    release() {
      if (--held.count > 0) return;
      clearInterval(held.timer);
      if (held.end !== null) clearTimeout(held.end);
      channels.delete(channelId);
      if (channels.size === 0) refreshers.delete(adapter);
    },
    endIn(ms) {
      if (held.end !== null) clearTimeout(held.end);
      held.end = ms === null ? null : setTimeout(() => { held.end = null; held.refresh(); }, ms);
    },
  };
}

/** Test hook: how many channel schedules are held (fix round 2: two views on one channel share one). */
export function heldMusicRefreshers(): number {
  let n = 0;
  for (const channels of refreshers.values()) n += channels.size;
  return n;
}

/**
 * The player state of one channel. `poll` holds the channel's shared refresher (the player views); without it the hook only
 * reads the adapter's cached state and follows its changes, so a passive line (the Bot's member row) adds no polling of its own
 * and is as fresh as whatever view already refreshes that channel.
 */
function useMusicState(client: CoreClient, communityId: string | null, channelId: string | null, poll = true): VoiceMusicState | null {
  const [state, setState] = useState<VoiceMusicState | null>(null);
  useEffect(() => {
    setState(null);
    const adapter = adapterFor(client, communityId);
    if (adapter === null || channelId === null) return;
    if (!poll) {
      const unsubscribe = adapter.subscribe(channelId, () => setState(adapter.state(channelId)));
      setState(adapter.state(channelId));
      return unsubscribe;
    }
    const hold = holdRefresh(adapter, channelId);
    const read = () => {
      const next = adapter.state(channelId);
      setState(next);
      // Re-read a second after the playing track should end (the next one starts, or the queue ran out).
      if (next.status === 'available' && next.track !== null && !next.paused && next.clock !== undefined && next.track.durationMs > 0)
        hold.endIn(Math.max(0, next.track.durationMs - musicPlayheadMs(next, Date.now())) + 1000);
      else hold.endIn(null);
    };
    const unsubscribe = adapter.subscribe(channelId, read);
    read();
    return () => { hold.release(); unsubscribe(); };
  }, [client, communityId, channelId, poll]);
  return state;
}

/** Ruling AI: a 1 s clock while the playhead runs (a playing track with the Bot's stamp); idle otherwise. */
function usePlayheadNow(running: boolean): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!running) return;
    setNow(Date.now());
    const timer = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(timer);
  }, [running]);
  return now;
}

interface ViewInput {
  state: VoiceMusicState | null; room: string; bot: string; members: readonly MemberSummary[]; trust: readonly TrustSummary[] | undefined;
  /** This device is in the player's voice call (else a press says "Join {room} to control playback"). */
  inCall: boolean; now: number;
  onToggle: () => Promise<void> | void; onSkip: () => Promise<void> | void; onStop: () => Promise<void> | void;
}

function musicView({ state, room, bot, members, trust, inCall, now, onToggle, onSkip, onStop }: ViewInput): MusicPlayerView {
  const base = { room, bot: { name: bot }, onToggle, onSkip, onStop, labels: MUSIC_LABELS() };
  if (state === null || state.status !== 'available') {
    // The Bot is not playing in that room any more: the prototype's "Glenn left voice"; a press says the same.
    const left = t('music.left', { bot });
    return { ...base, positionS: 0, playing: false, meta: left, canControl: false, blocked: left, queue: [] };
  }
  const requester = (track: VoiceMusicTrack) => (track.requesterId === undefined ? '' : userName(track.requesterId, members, trust));
  const current = state.track;
  const source = (track: VoiceMusicTrack) => t(track.provider === 'spotify' ? 'music.source.spotify' : 'music.source.youtube');
  const meta = current === null ? t('music.idleIn', { bot, room })
    : current.requesterId === undefined ? t('music.metaNoRequester', { source: source(current), room })
      : t('music.meta', { source: source(current), name: requester(current), room });
  return {
    ...base,
    ...(current === null ? {} : { track: { title: current.title, by: requester(current), durationS: current.durationMs / 1000 } }),
    positionS: musicPlayheadMs(state, now) / 1000, playing: !state.paused, meta, volume: state.volume,
    canControl: state.permissions.control,
    blocked: inCall ? t('music.blocked.dj') : t('music.blocked.join', { room }),
    queue: [
      ...(current === null ? [] : [{ title: current.title, by: requester(current), durationS: current.durationMs / 1000, current: true }]),
      ...state.queue.map(item => ({ title: item.track.title, by: requester(item.track), durationS: item.track.durationMs / 1000 })),
    ],
  };
}

/**
 * Task 11 (Jonas, Q5): the Bot's member-list line while it sits in a voice channel, "playing in {room}" or "idle in {room}",
 * from the same shared player state as the dock and the stage (one refresher per channel while something on screen reads it).
 * Null when nothing is known yet or the player is unavailable, so the caller keeps its own line.
 */
export function useBotRoomLine(client: CoreClient, communityId: string | null, channelId: string | null, room: string): string | null {
  // Fix round 2: the member row only reads (no refresher of its own).
  const state = useMusicState(client, communityId, channelId, false);
  if (channelId === null || state === null || state.status !== 'available') return null;
  return t(state.track !== null && !state.paused ? 'members.playingIn' : 'members.idleIn', { room });
}

function useChannelPlayer(client: CoreClient, communityId: string | null, channelId: string | null, read: boolean) {
  const state = useMusicState(client, read ? communityId : null, read ? channelId : null);
  const adapter = adapterFor(client, communityId);
  const paused = state?.status === 'available' ? state.paused : true;
  const running = state?.status === 'available' && state.track !== null && !state.paused && state.clock !== undefined;
  const now = usePlayheadNow(running);
  const onToggle = useCallback(() => (adapter !== null && channelId !== null ? adapter.pause(channelId, !paused) : undefined), [adapter, channelId, paused]);
  const onSkip = useCallback(() => (adapter !== null && channelId !== null ? adapter.skip(channelId) : undefined), [adapter, channelId]);
  const onStop = useCallback(() => (adapter !== null && channelId !== null ? adapter.stop(channelId) : undefined), [adapter, channelId]);
  return { state, now, onToggle, onSkip, onStop };
}

export interface MusicPlayerInput {
  client: CoreClient;
  /** The community of the call's voice channel; null for a DM call or no call. */
  communityId: string | null;
  /** The call's voice channel; null without a call. */
  channelId: string | null;
  /** The voice channel's name (the player's room). */
  room: string;
  /** The Bot's name while it is in that call; null while it is not (then there is no player to read). */
  bot: string | null;
  members: readonly MemberSummary[];
  trust: readonly TrustSummary[] | undefined;
}

/**
 * The player for this device's call (the dock row and the stage bar): null without a call in a community, without the Bot in
 * it, or while the Bot's music state is unavailable.
 */
export function useMusicPlayer({ client, communityId, channelId, room, bot, members, trust }: MusicPlayerInput): MusicPlayerView | null {
  const player = useChannelPlayer(client, communityId, channelId, bot !== null);
  if (player.state === null || player.state.status !== 'available' || bot === null) return null;
  return musicView({ ...player, room, bot, members, trust, inCall: true });
}

export interface MusicCardInput extends Omit<MusicPlayerInput, 'bot'> {
  /** The Bot's name (the message's author). */
  bot: string;
  /** The queue item this message announced (fix round 2): the card is live only while that track plays. */
  item: string;
  /** The Bot is in that voice channel's call now (else the card says it left voice). */
  botInRoom: boolean;
  /** The card is on screen (fix round 2: an off-screen card reads nothing). */
  visible: boolean;
  /** This device is in that voice channel's call. */
  inCall: boolean;
}

/**
 * The card on Glenn's "started music" message: the same player state, read while the Bot is in that room and the card is on
 * screen, and live only while the announced track is the one playing; after that it says the track finished.
 */
export function useMusicCard({ client, communityId, channelId, room, bot, item, botInRoom, visible, inCall, members, trust }: MusicCardInput): MusicPlayerView {
  const player = useChannelPlayer(client, communityId, channelId, botInRoom && visible);
  if (!botInRoom) return musicView({ ...player, state: null, room, bot, members, trust, inCall });
  const state = player.state;
  const controls = { onToggle: player.onToggle, onSkip: player.onSkip, onStop: player.onStop };
  // Task S1 (8b leftover): the Bot is in the room but nothing has been read yet — the card is not on screen yet, or the first
  // snapshot is on its way. A neutral card, never "left voice" for a moment.
  if (state === null) {
    return { room, bot: { name: bot }, positionS: 0, playing: false, meta: room, canControl: false, queue: [], ...controls,
      labels: { ...MUSIC_LABELS(), idle: t('music.region') } };
  }
  // The announced track is no longer the one playing: another plays, or (Task S1) the queue ran out after it.
  if (state.status === 'available' && (state.track === null || state.track.id !== item)) {
    const ended = t('music.ended', { bot, room });
    return { room, bot: { name: bot }, positionS: 0, playing: false, meta: ended, canControl: false, blocked: ended, queue: [],
      ...controls, labels: { ...MUSIC_LABELS(), idle: t('music.finished') } };
  }
  return musicView({ ...player, room, bot, members, trust, inCall });
}

/**
 * One view of the player. A control the Bot refuses (or that fails) says why in the player's toast; the control's promise
 * settles when the Bot's confirmed snapshot has arrived (the adapter refreshes after every mutation).
 */
export function MusicPlayer({ view, variant }: { view: MusicPlayerView; variant: NowPlayingProps['variant'] }) {
  const [toast, setToast] = useState<{ text: string; id: number } | undefined>(undefined);
  const run = (action: () => unknown) => () => {
    void Promise.resolve().then(action).catch((error: unknown) => {
      setToast(prev => ({ text: error instanceof Error && 'code' in error && error.code === 'operation-unavailable' ? t('music.error.operation-unavailable') : error instanceof Error && 'code' in error && error.code === 'unavailable' ? t('music.error.unavailable') : error instanceof Error ? error.message : t('music.blocked.dj'), id: (prev?.id ?? 0) + 1 }));
    });
  };
  return <NowPlaying {...view} variant={variant} {...(toast === undefined ? {} : { toast })}
    onToggle={run(view.onToggle)} onSkip={run(view.onSkip)} onStop={run(view.onStop)} />;
}

/** The player marker Glenn's public "started music" message ends with: the voice channel, the requester and the queue item. */
const MARKER = /\n?\[dilla-music:v1:([0-9a-f]{32}):([0-9a-f]{32}):([0-9a-f]{32})\]$/u;
export function musicMarker(body: string): { voiceChannelId: string; requesterId: string; item: string } | null {
  const match = MARKER.exec(body);
  return match === null ? null : { voiceChannelId: match[1], requesterId: match[2], item: match[3] };
}

/** An element's visibility on screen (IntersectionObserver); without one (tests, old engines) it counts as visible. */
function useOnScreen(ref: { current: Element | null }): boolean {
  const [visible, setVisible] = useState(typeof IntersectionObserver === 'undefined');
  useEffect(() => {
    const el = ref.current;
    if (el === null || typeof IntersectionObserver === 'undefined') return;
    const observer = new IntersectionObserver(entries => { setVisible(entries.some(e => e.isIntersecting)); });
    observer.observe(el);
    return () => observer.disconnect();
  }, [ref]);
  return visible;
}

/**
 * Jonas 2026-10-28: the card on Glenn's public "started music" message in the channel where /play ran. It reads the same player
 * state as the dock row and the stage bar, for the voice channel and track the marker names, while the Bot is in that room.
 */
export function MusicMessageCard({ client, communityId, voiceChannelId, item, bot }: { client: CoreClient; communityId: string | null; voiceChannelId: string; item: string; bot: string }) {
  const voice = useSlice('voice');
  const call = useSlice('call');
  const trust = useSlice('trust');
  const members = useSlice(`members:${communityId ?? ''}`) ?? [];
  const ref = useRef<HTMLDivElement>(null);
  const visible = useOnScreen(ref);
  const room = useMusicRoomName(communityId, voiceChannelId);
  const botInRoom = voice?.[voiceChannelId]?.participants.some(p => p.bot) === true;
  const view = useMusicCard({ client, communityId, channelId: voiceChannelId, room, bot, item, botInRoom, visible, inCall: call?.channelId === voiceChannelId, members, trust });
  return <div ref={ref} className="dw-music-card"><MusicPlayer view={view} variant="card" /></div>;
}

/** The voice channel's name in a channel list; "a voice channel" when this viewer cannot see it (or the list is not here yet). */
export function musicRoomNameIn(channels: readonly { id: string; name: string }[] | undefined, voiceChannelId: string): string {
  return channels?.find(c => c.id === voiceChannelId)?.name ?? t('music.someRoom');
}

/**
 * Task S1 (8b leftover): the voice channel's name, re-read when the community's channel list arrives or changes. It subscribes
 * only while there is a room to name (voiceChannelId non-null), so a message row that is no music announcement holds nothing.
 */
export function useMusicRoomName(communityId: string | null, voiceChannelId: string | null): string {
  const [channels] = useSlices(communityId === null || voiceChannelId === null ? [] : [`channels:${communityId}` as const]);
  return voiceChannelId === null ? '' : musicRoomNameIn(channels, voiceChannelId);
}

/**
 * Security review M1: the card is drawn only for an unedited message from this community's managed Bot account (a member row
 * of account kind 1, the server's account kind, not the sender's credential).
 */
export { isCommunityBot } from '../bot-member.ts';
