// The shell's voice surfaces (web-3 task 9, flow 06): the voice view of a voice channel route, the DM call strip and
// its stage, the voice rows' nested people in the channel list (ChannelList renders them) and the dock. Everything here is presentational over useCall's CallView and the
// worker's `call` and `voice` slices; nothing here holds media or keys.
import { useEffect, useLayoutEffect, useRef, useState, type RefObject } from 'react';
import type { CallState, CoreClient, MemberSummary, SendBlocksState, TrustSummary, VoiceCall } from '@dilla/client-core';
import { SETTINGS } from '@dilla/client-core';
import {
  Avatar, Button, CallControls, CallStage, ChannelHeader, VoiceDock,
  type CallControlsProps, type CallStageNotice, type CallTileProps, type SharePickerSource, type SharePickerValue, type TileMenu, type VoiceDockProps,
  type VoiceParticipantView,
} from '@dilla/ui';
import { linkQuality } from '../../call/stats.ts';
import { shareEstimate, shareSupport, supportedOptions, type ShareOptions, type ShareSupport, type ShareSurface } from '../../call/share-options.ts';
import { en, t } from '../../strings/index.ts';
import { micNoticeKey, participantView, selfName, userName, voiceStateLabels } from '../shell-model.ts';
import { outgoingText, type CallView } from './useCall.tsx';
import { MusicPlayer, type MusicPlayerView } from './music.tsx';
import { codecStat, frameStat, useTileStats } from './tile-stats.ts';
import type { TileStats } from '../../call/session.ts';
import { usePhoneLayout } from '../../layout.ts';
import { voiceShortcutText } from './shortcuts.ts';

/** What the voice surfaces read besides the CallView. */
export interface VoiceContext {
  client: CoreClient;
  call: CallView;
  slice: CallState | undefined;
  voice: Readonly<Record<string, VoiceCall>> | undefined;
  members: readonly MemberSummary[];
  trust: readonly TrustSummary[] | undefined;
  /** The worker's sendBlocks slice (undefined until published): tells a call-group-only block from a channel block. */
  sendBlocks: SendBlocksState | undefined;
  settings: Readonly<Record<string, string>>;
  self: { id: string; username: string } | null;
  ownDevice: string | null;
  onCompare(userId: string): void;
  /** Opens Settings → Voice (product walk M7: the dock's push to talk with no key set leads there). */
  onVoiceSettings(): void;
  onJoinVoice?(channelId: string): void;
  /**
   * AW6 fix round 1 (Jonas, concern 2): for the built-in AFK channel, its page: the away roster (as its sidebar row lists
   * it) and whether this user is away there; null for any other channel. `onJoinVoice` on it chooses AFK (useCall).
   */
  afkPage?(channelId: string): { people: readonly VoiceParticipantView[]; away: boolean } | null;
  /** Task 8b: the music player of this device's call (useMusicPlayer), null while the Bot plays nothing there; the dock's row and
   *  the stage's bar both read it. */
  music?: MusicPlayerView | null;
}

const VOLUME = /^(0|[1-9]0|1[0-9]0|200)$/;
function volumeOf(settings: Readonly<Record<string, string>>, userId: string): number {
  const v = settings[SETTINGS.voiceVolume(userId)];
  return v !== undefined && VOLUME.test(v) ? Number(v) : 100;
}

function streamVolumeOf(settings: Readonly<Record<string, string>>, userId: string): number {
  const v = settings[SETTINGS.voiceStreamVolume(userId)];
  return v !== undefined && VOLUME.test(v) ? Number(v) : 100;
}

function tileLabels(name: string): CallTileProps['labels'] {
  return { state: voiceStateLabels(), volume: t('voice.volume', { name }), volumeValue: en['voice.volumeValue'] };
}

/** The id of a tile in the stage (R27): one keyed list, `${deviceId}:${source}`. */
const tileId = (deviceId: string, kind: 'person' | 'screen') => `${deviceId}:${kind}`;

type TileBase = Omit<CallTileProps, 'media' | 'id' | 'source' | 'title' | 'fullscreenLabel' | 'mirror' | 'menu'>;

/**
 * Fix round 1 (Jonas): the person menu of a voice occupant or a stage tile (Signal §5.4), with the client-side items only:
 * the volume (0–200 %, `voice.volume.<user>`; a shared screen's own `voice.streamVolume.<user>`), Mute for me (the volume at 0,
 * and back to what it was), Copy username. Not for yourself: no volume, no mute. A screen without sound has no volume.
 * Wave C's Move to, Server mute and Disconnect are not offered.
 */
const beforeMute = new Map<string, number>();
export function personMenu(v: Pick<VoiceContext, 'call' | 'settings' | 'members'>, room: string,
  p: { userId: string; name: string; bot: boolean; own: boolean; title?: string }, source: 'person' | 'screen' = 'person', screenAudio = true): TileMenu {
  const stream = source === 'screen';
  const level = stream ? streamVolumeOf(v.settings, p.userId) : volumeOf(v.settings, p.userId);
  const set = (n: number) => { if (stream) v.call.setStreamVolume(p.userId, n); else v.call.setVolume(p.userId, n); };
  const key = `${stream ? 's' : 'v'}:${p.userId}`;
  const tunable = !p.own && (!stream || screenAudio);
  const username = v.members.find(m => m.userId === p.userId)?.username ?? p.name;
  return {
    person: { name: p.title ?? p.name, avatarName: p.name, where: t('callStage.menu.where', { room }), ...(p.bot ? { bot: true } : {}) },
    label: t('callStage.menu.label', { name: p.name }),
    ...(tunable ? { volume: { label: t(stream ? 'callStage.menu.streamVolume' : 'callStage.menu.volume'), value: level,
      valueText: t('callStage.menu.value', { n: level }), onChange: set } } : {}),
    items: [
      ...(tunable ? [{ id: 'mute', label: t('callStage.menu.muteForMe'), icon: 'volume-x' as const, check: level === 0 }] : []),
      { id: 'copy', label: t('callStage.menu.copy'), icon: 'copy' as const },
    ],
    onSelect: id => {
      if (id === 'mute') {
        if (level === 0) { set(beforeMute.get(key) ?? 100); beforeMute.delete(key); } else { beforeMute.set(key, level); set(0); }
        return;
      }
      void navigator.clipboard?.writeText(`@${username}`).catch(() => undefined);
      v.call.announce(t('callStage.menu.copied', { name: username }));
    },
  };
}

/**
 * One device's tiles (R27, pre-flight C-9): a person tile (its `source` is `camera` with a verified stream, else `avatar`, but its
 * id is `:person` either way, so turning a camera on or off keeps a focus on it) with the state glyphs, the person menu and, for
 * the own camera, the mirror, plus a `screen` tile while it shares (never mirrored; its menu has the stream's own volume). A
 * screen-only sharer therefore keeps an avatar tile.
 */
function deviceTiles(base: TileBase, o: {
  camera: MediaStream | null; screen: MediaStream | null; sharing: boolean; mirrorCamera: boolean; name: string;
  /** The person tile's shown name when it is not `name` (the own tile's "(you)", a bot's "· playback only"). */
  title?: string;
  /** The person menu (fix round 1) of the person tile and of the screen tile. */
  menu: (source: 'person' | 'screen') => TileMenu;
  /** #37: a remote screen: whether it carries sound (the mark when not). */
  remoteScreen?: { screenAudio: boolean };
  /** Task 8c fix round 1: the person tile's stat (another device's published ping, "18 ms"). */
  personStat?: string;
}): CallTileProps[] {
  const { view } = base;
  const fullscreenLabel = t('stage.fullscreen');
  const camera = o.camera !== null && view.state !== 'unverified';
  const out: CallTileProps[] = [{
    ...base, media: camera ? o.camera : null, source: camera ? 'camera' : 'avatar', id: tileId(view.deviceId, 'person'),
    title: o.title ?? o.name, fullscreenLabel, mirror: camera && o.mirrorCamera, menu: o.menu('person'),
    ...(o.personStat !== undefined ? { stat: o.personStat } : {}),
  }];
  if (o.screen !== null || o.sharing) {
    out.push({ ...base, media: o.screen, source: 'screen', id: tileId(view.deviceId, 'screen'), title: t('stage.screen', { name: o.name }),
      fullscreenLabel, mirror: false, menu: o.menu('screen'),
      ...(o.remoteScreen !== undefined ? { silent: !o.remoteScreen.screenAudio } : {}) });
  }
  return out;
}

/** The tiles of the call this device is in: the shares first, then the people in the room's order (below). */
function callTiles(v: VoiceContext, channelId: string, roomName: string): CallTileProps[] {
  const { call } = v;
  const state = call.state;
  const room = v.voice?.[channelId];
  // Product walk I3: the own tile is a self-view of what this device sends (the camera mirrored); M4: it is speaking when
  // the SFU hears this device, as the others' tiles are; M13: no tile carries the client tag (the page knows only its own).
  const ownName = selfName(v.self, v.members);
  const self = v.self;
  // Signal §5.6: the own tile is "{name} (you)" and a bot "{name} · playback only", the prototype's tile names.
  const ownTitle = t('callStage.tile.you', { name: ownName });
  const own = self === null ? [] : deviceTiles({
    view: {
      deviceId: v.ownDevice ?? 'own', userId: self.id, name: ownName, state: 'ok' as const,
      speaking: state?.speaking ?? false, muted: state?.muted ?? false, deafened: state?.deafened ?? false, video: state?.camera ?? false,
      screen: state?.sharing ?? false, web: false, bot: false,
    },
    own: true, volume: 100, labels: tileLabels(ownName),
  }, { camera: state?.localCamera ?? null, screen: state?.localScreen ?? null, sharing: state?.sharing ?? false, mirrorCamera: true,
    name: ownName, title: ownTitle,
    menu: source => personMenu(v, roomName, { userId: self.id, name: ownName, bot: false, own: true, title: ownTitle }, source) });
  const others = (state?.tiles ?? []).flatMap(tile => {
    const p = room?.participants.find(r => r.deviceId === tile.deviceId);
    const name = p?.name ?? userName(tile.userId, v.members, v.trust);
    const view = p !== undefined ? participantView(p, tile.state, tile.speaking)
      : { deviceId: tile.deviceId, userId: tile.userId, name, state: tile.state, speaking: tile.speaking, muted: false, deafened: false,
        video: tile.video !== null, screen: tile.screen !== null, web: false, bot: false };
    const title = view.bot ? t('callStage.tile.bot', { name }) : undefined;
    return deviceTiles({ view, own: false, volume: volumeOf(v.settings, tile.userId), labels: tileLabels(name) },
    { camera: tile.video, screen: tile.screen, sharing: false, mirrorCamera: false, name, ...(title !== undefined ? { title } : {}),
      menu: source => personMenu(v, roomName, { userId: tile.userId, name, bot: view.bot, own: false, ...(title !== undefined ? { title } : {}) }, source, tile.screenAudio),
      remoteScreen: { screenAudio: tile.screenAudio },
      // Task 8c fix round 1: the ping that device publishes about itself (its `dilla.rtt`), never this page's own link.
      ...(tile.rttMs != null ? { personStat: t('callStage.stat.ping', { ms: tile.rttMs }) } : {}) });
  });
  // Signal §5.6 (the prototype's stage): the shared screens first, then the people in the room's own order (yours where the room
  // lists you); a device the room does not list yet keeps its place after them. One stable order, keyed by id (R27).
  const order = new Map((room?.participants ?? []).map((p, i) => [p.deviceId, i]));
  const rank = (tile: CallTileProps) => order.get(tile.view.deviceId) ?? order.size;
  return [...own, ...others].map((tile, i) => ({ tile, i }))
    .sort((a, b) => Number(b.tile.source === 'screen') - Number(a.tile.source === 'screen') || rank(a.tile) - rank(b.tile) || a.i - b.i)
    .map(x => x.tile);
}

/**
 * Task 8c (Signal §5.6): the tiles' stats from one read: the own person tile says what its microphone sends ("opus 48k"), a
 * screen what is sent or received ("1440p · 30 fps", "· audio" with the own share's sound). Another person's tile keeps the ping
 * callTiles gave it from that device's own published `dilla.rtt` (fix round 1), never this page's link.
 */
export function withTileStats(tiles: readonly CallTileProps[], stats: TileStats | null): CallTileProps[] {
  if (stats === null) return [...tiles];
  return tiles.map(tile => {
    const stat = tile.own ? (tile.source === 'screen' ? frameStat(stats.ownScreen) : codecStat(stats.ownAudio))
      : tile.source === 'screen' ? frameStat(stats.screens[tile.view.deviceId]) : undefined;
    return stat === undefined ? tile : { ...tile, stat };
  });
}

/** Signal §5.6: the not-joined preview's line: "Ada is here", "Ada and Björn are here", or that nobody is. */
export function previewText(names: readonly string[]): string {
  if (names.length === 0) return t('callStage.preview.nobody');
  if (names.length === 1) return t('callStage.preview.one', { name: names[0] });
  return t('callStage.preview.many', { names: names.join(t('callStage.preview.and')) });
}

/**
 * The stage notice for a channel (flow 06 § States): the RV-3 pause banner, a block, then a transient notice, then the
 * microphone's listen-only outcome. Only 'paused' and 'blocked' hide media (CallStageNotice.kind).
 */
export function stageNotice(v: VoiceContext, channelId: string): CallStageNotice | null {
  const slice = v.slice;
  if (slice !== undefined && slice.channelId === channelId) {
    const name = slice.changedUser !== null ? userName(slice.changedUser, v.members, v.trust) : null;
    if (slice.phase === 'paused' && slice.error?.code !== 'E_UMK_CHANGED') {
      // WB security I1: the hold after a refused rekey (a device of the call not checked yet); nothing to compare.
      return { kind: 'paused', text: t('voice.held'), focusOnce: false, action: null };
    }
    if (slice.phase === 'paused') {
      const changed = slice.changedUser;
      return { kind: 'paused', text: t('voice.paused', { name: name ?? '' }), focusOnce: v.call.pauseFocusPending,
        action: changed !== null ? { label: t('voice.pausedAction'), onAction: () => v.onCompare(changed) } : null };
    }
    if (slice.phase === 'blocked') {
      // Product walk M10: a block that offers the join again (joinHeld false) never says joining is paused.
      const held = joinHeld(v, channelId);
      const changed = slice.error?.code === 'E_UMK_CHANGED' && name !== null ? slice.changedUser : null;
      const midCall = v.call.blockedMidCall === channelId;
      const text = midCall ? t(held ? 'voice.blocked' : 'voice.blockedLeft')
        : changed !== null && name !== null ? t('voice.blockedChanged', { name }) : t(held ? 'voice.blockedJoin' : 'voice.blockedRetry');
      // Product walk M9: a join refused for a named changed identity offers the comparison, as the pause does.
      const action = !midCall && changed !== null ? { label: t('voice.pausedAction'), onAction: () => v.onCompare(changed) } : null;
      return { kind: 'blocked', text, action, focusOnce: true };
    }
    if (slice.phase === 'failed' && v.call.channelId !== channelId) return { kind: 'info', text: t('voice.failed'), action: null, focusOnce: false };
    // Product walk I8: removed, and the channel is gone for this device (the membership is): the removal says so.
    if (removedFromServer(v, channelId)) return { kind: 'info', text: t('voice.removedNotMember'), action: null, focusOnce: false };
  }
  if (v.call.notice?.channelId === channelId) {
    // Fix round 1 (Jonas): the share's sound-source select is gone (a silent share keeps the default source), so the notice
    // offers no pick.
    return { kind: 'info', text: v.call.notice.text ?? t(v.call.notice.key), action: null, focusOnce: false };
  }
  const mic = v.call.channelId === channelId && v.call.state !== null ? micNoticeKey(v.call.state.mic) : null;
  return mic !== null ? { kind: 'info', text: t(mic), action: null, focusOnce: false } : null;
}

/**
 * True while a block holds joining back for this channel (flow 06: "no join button until the cause is gone"). A block by
 * a changed contact clears once that contact is no longer changed (verified); an unnamed one once no contact is. A block that came only from the call group
 * (the channel's own sendBlocks entry is clear, no changed identity) offers the join again (integration ruling
 * 2026-10-13: the left group cannot show it cleared, call_join_check refuses a staged join before anything is posted, and
 * RV-2 closes a poisoned idle group). Any other block, and any block while sendBlocks is not known yet, holds until the
 * worker's call slice leaves 'blocked' (review m9).
 */
function joinHeld(v: VoiceContext, channelId: string): boolean {
  // Product walk I8: no join of a channel this device may no longer see.
  if (removedFromServer(v, channelId)) return true;
  const s = v.slice;
  if (s?.channelId !== channelId || s.phase !== 'blocked') return false;
  const changed = (r: TrustSummary) => r.changedUmk !== null || r.blocked === 'E_UMK_CHANGED';
  if (s.error?.code === 'E_UMK_CHANGED' && s.changedUser !== null) {
    const row = v.trust?.find(r => r.userId === s.changedUser);
    return row !== undefined && changed(row);
  }
  if (s.error?.code === 'E_UMK_CHANGED') {
    // Unnamed (web-3 task 10, concern 3): the cause is gone once no contact is changed any more and the channel's own
    // send block (a DM's text group) is clear; a trust state or sendBlocks not known yet holds (fail closed). The join
    // itself runs the core's gate again, so offering it never lets a changed tree through.
    if (v.trust === undefined || v.sendBlocks === undefined) return true;
    return v.trust.some(changed) || v.sendBlocks[channelId] !== undefined;
  }
  const callGroupOnly = s.error?.code !== 'E_UMK_CHANGED' && s.changedUser === null
    && v.sendBlocks !== undefined && v.sendBlocks[channelId] === undefined;
  return !callGroupOnly;
}

/** Product walk I8: the call of this channel ended as removed, and the worker's read of the channel answered 404. */
export function removedFromServer(v: Pick<VoiceContext, 'slice'>, channelId: string): boolean {
  return v.slice?.channelId === channelId && v.slice.phase === 'removed' && v.slice.error?.code === 'E_NOT_FOUND';
}

/**
 * Moves focus to the stage's first control once per join (flow 06 § Focus rules: join and answer): as soon as the
 * controls show (WB a11y I1: at join start, while they are still aria-disabled, so focus never falls to the page).
 */
function useStageFocus(ref: RefObject<HTMLDivElement | null>, call: CallView, joined: boolean) {
  // No dependency list: a join may start in any render after the stage mounted.
  useLayoutEffect(() => {
    if (!joined) return;
    const first = ref.current?.querySelector<HTMLButtonElement>('[role="toolbar"] button');
    if (first !== null && first !== undefined && call.takeStageFocus()) first.focus();
  });
}

/**
 * WB a11y I1: keeps focus inside a voice surface when the surface swaps under it (a join starts, a join fails, the call
 * is removed or ends, the connection is lost). When the focused control went away with the swap (focus fell to the
 * page) and focus was inside one of `roots` before, `pick()` names where it goes now.
 */
function useFocusRescue(roots: readonly RefObject<HTMLElement | null>[], mode: string, pick: () => HTMLElement | null | undefined) {
  const inside = useRef(false);
  /** AW5 fix round 3: the element this rescue last put focus on; a later swap may move it again while it is still there
   *  (a held removal notice arrives after the cut put focus on the join button). */
  const placed = useRef<Element | null>(null);
  const live = useRef({ roots, pick });
  live.current = { roots, pick };
  useEffect(() => {
    const onIn = (e: FocusEvent) => {
      const target = e.target;
      inside.current = target instanceof Node && live.current.roots.some(r => r.current?.contains(target) === true);
      if (target !== placed.current) placed.current = null;
    };
    document.addEventListener('focusin', onIn);
    return () => document.removeEventListener('focusin', onIn);
  }, []);
  useLayoutEffect(() => {
    const active = document.activeElement;
    if (!inside.current) return;
    if (active !== null && active !== document.body && active.isConnected && active !== placed.current) return;
    const next = live.current.pick();
    if (next == null || next === active) return;
    placed.current = next;
    next.focus();
  }, [mode]);
}

/** Where focus goes in a stage that swapped under it: its first control in a call, else the notice (or its action),
 *  else the join button. */
function stageTarget(root: HTMLElement | null, joined: boolean): HTMLElement | null {
  if (root === null) return null;
  if (joined) return root.querySelector<HTMLElement>('[role="toolbar"] button');
  return root.querySelector<HTMLElement>('.d-call-stage__notice-action, .d-call-stage__notice[tabindex]')
    ?? root.querySelector<HTMLElement>('.d-call-stage__join, .dw-voice-join');
}

/** The mute and deafen shortcuts as the tooltips write them; the platform cannot change during a session. */
const VOICE_SHORTCUT_TEXT = voiceShortcutText();

/** Why an inert camera or share is inert, the same for the stage and the dock (founder, 2026-10-15). */
const VOICE_WHY = () => ({ paused: t('voice.why.paused'), joining: t('voice.why.joining'), noShare: t('voice.why.noShare') });

/** Whether this browser can capture a screen; the stage and the dock offer share screen only then. */
function canCapture(): boolean {
  return typeof navigator !== 'undefined' && typeof navigator.mediaDevices?.getDisplayMedia === 'function';
}

/** The DM call box has no call bar: its hearing line stands under its controls (fix round 1: no push-to-talk line any more). */
function StageLines({ v }: { v: VoiceContext }) {
  const s = v.call.state;
  if (!hears(s)) return null;
  return <div className="dw-voice-stage__lines"><p><HearingLine audio={s?.audio} silent={false} /></p></div>;
}

/** Whether the hearing count shows: remote audio is expected, and nothing holds playback back (deafened, paused). */
function hears(s: CallView['state']): boolean {
  return s?.audio !== undefined && s.audio.expected > 0 && s.deafened !== true && s.paused !== true;
}

/**
 * Signal §5.6: the call bar's stats line, "24 ms · ↑ 38 ↓ 71 kb/s · hearing 2 of 2": this device's link (as the dock reads it,
 * call.state.link; "— ms" before the first read) and, while any is expected, how many remote tracks it plays.
 */
function StatsLine({ v }: { v: VoiceContext }) {
  const s = v.call.state;
  const link = s?.link;
  const ping = link === undefined ? en['voice.dock.pingNone'] : link.rttMs < 1 ? en['voice.dock.pingSub'] : t('voice.dock.ping', { ms: link.rttMs });
  const rate = link !== undefined && link.upKbps !== null && link.downKbps !== null ? t('voice.dock.rate', { up: link.upKbps, down: link.downKbps }) : null;
  return (
    <>
      {[ping, rate].filter((x): x is string => x !== null).join(' · ')}
      {hears(s) ? <>{' · '}<HearingLine audio={s?.audio} silent={false} /></> : null}
    </>
  );
}

/** Task 8c: what this browser's screen capture can act on, read once (the share picker draws only those controls). */
const SHARE_SUPPORT: ShareSupport = (() => {
  if (typeof navigator === 'undefined') return shareSupport({ ua: '' });
  const data = (navigator as Navigator & { userAgentData?: { platform?: string; brands?: Array<{ brand: string }> } }).userAgentData;
  return shareSupport({ ua: navigator.userAgent, uaPlatform: data?.platform, brands: data?.brands?.map(b => b.brand) });
})();
const PICKER_SOURCE: Record<ShareSurface, SharePickerSource> = { monitor: 'screen', window: 'window', browser: 'tab' };
const SURFACE: Record<SharePickerSource, ShareSurface> = { screen: 'monitor', window: 'window', tab: 'browser' };
const toPicker = (o: ShareOptions): SharePickerValue => ({ source: PICKER_SOURCE[o.surface], resolution: o.resolution, fps: o.fps, audio: o.systemAudio });
const fromPicker = (p: SharePickerValue): ShareOptions => ({ surface: SURFACE[p.source], resolution: p.resolution, fps: p.fps, systemAudio: p.audio });

/** The display's size in device pixels, as the share would capture it ("Display · 2560×1440"). */
function displaySize(): { w: number; h: number } {
  const ratio = typeof window === 'undefined' ? 1 : window.devicePixelRatio || 1;
  const scr = typeof screen === 'undefined' ? { width: 0, height: 0 } : screen;
  return { w: Math.round(scr.width * ratio), h: Math.round(scr.height * ratio) };
}

/**
 * Task 8c (Signal §5.6): the call bar's share picker. It edits a draft of the call's share options (only what this browser can
 * act on: SHARE_SUPPORT) and shows the upload estimate from the same table the session encodes with, under the instance's cap.
 * Go live starts the share with them; Apply (while sharing) hands them to the running share.
 */
export function useSharePicker(v: VoiceContext, support: ShareSupport = SHARE_SUPPORT): NonNullable<CallControlsProps['picker']> {
  const [open, setOpen] = useState(false);
  const [draft, setDraft] = useState<ShareOptions>(v.call.shareOptions);
  const sharing = v.call.state?.sharing ?? false;
  const onOpenChange = (next: boolean) => {
    if (next) setDraft(supportedOptions(v.call.shareOptions, support));
    setOpen(next);
  };
  const value = supportedOptions(draft, support);
  const tab = value.surface === 'browser';
  const audio: 'toggle' | 'note' | 'none' = !support.surface ? 'none' : support.audio[value.surface] ? 'toggle' : 'note';
  const estimate = shareEstimate(value, v.call.state?.shareCapBps ?? null);
  const size = displaySize();
  return {
    open, onOpenChange, quality: t('callStage.share.quality'),
    value: toPicker(value), onChange: p => { setDraft(fromPicker(p)); },
    sources: support.surface ? ['screen', 'window', 'tab'] : [],
    audio,
    preview: value.surface === 'monitor' ? t('callStage.share.previewScreen', { w: size.w, h: size.h })
      : t(tab ? 'callStage.share.previewTab' : 'callStage.share.previewWindow'),
    estimate: { text: t(estimate.heavy ? 'callStage.share.estimateHeavy' : 'callStage.share.estimate', { mbps: estimate.mbps }), heavy: estimate.heavy },
    labels: {
      title: t('callStage.share.title'), titleSharing: t('callStage.share.titleSharing'), close: t('callStage.share.close'),
      source: t('callStage.share.source'), resolution: t('callStage.share.resolution'), frameRate: t('callStage.share.frameRate'),
      sources: { screen: t('callStage.share.screen'), window: t('callStage.share.window'), tab: t('callStage.share.tab') },
      native: t('callStage.share.native'), audio: t('callStage.share.audio'),
      audioNote: t(tab ? 'callStage.share.audioTab' : support.tabOnly ? 'callStage.share.audioTabOnly' : 'callStage.share.audioScreen'),
      go: t('callStage.share.go'), apply: t('callStage.share.apply'),
    },
    // CallControls closes the picker first and hands focus back to its control.
    onSubmit: () => {
      if (sharing) v.call.applyShare(value); else v.call.startShare(value);
    },
  };
}

function Controls({ v, paused, dm }: { v: VoiceContext; paused: boolean; dm: boolean }) {
  const s = v.call.state;
  const canShare = canCapture();
  const phone = usePhoneLayout();
  const hideShare = phone && !canShare;
  const picker = useSharePicker(v);
  // WB a11y I4: the toggles keep one label; WB a11y m6: *call* names a DM call only, so a voice channel's toolbar has
  // its own name (the dock's is voice.dock.region).
  return (
    <CallControls muted={s?.muted ?? false} deafened={s?.deafened ?? false} camera={s?.camera ?? false} sharing={s?.sharing ?? false}
      canCamera={!paused} canShare={!paused && canShare} hideShare={hideShare} busy={v.call.session === null}
      paused={paused} shortcuts={VOICE_SHORTCUT_TEXT} keyshortcuts={phone}
      labels={{ region: t(dm ? 'voice.controls.region' : 'voice.controls.voice'), mute: t('voice.mute'), deafen: t('voice.deafen'),
        camera: t('voice.cameraOn'), share: t('voice.shareOn'), leave: t('voice.leave'), why: VOICE_WHY(),
        // Signal §5.6: a channel's call bar shows the prototype's words (the DM call box keeps the icon toolbar).
        ...(dm ? {} : { bar: { mute: t('callStage.bar.mute'), unmute: t('callStage.bar.unmute'), deafen: t('callStage.bar.deafen'), undeafen: t('callStage.bar.undeafen'),
          camera: t('callStage.bar.camera'), share: t('callStage.bar.share'), stopShare: t('callStage.bar.stopShare'), leave: t('callStage.bar.leave') } }) }}
      // Task 8c: a channel's call bar opens the share picker; the DM call box (no prototype counterpart) shares at once with the
      // options last chosen there.
      {...(dm ? {} : { picker })}
      onMute={v.call.toggleMute} onDeafen={v.call.toggleDeafen} onCamera={v.call.toggleCamera} onShare={v.call.toggleShare}
      onLeave={v.call.leave} />
  );
}

/** The screen tiles of other people that are really being shared: the room says so, or their stream is on the stage. */
function liveScreenIds(v: VoiceContext, channelId: string, tiles: readonly CallTileProps[]): string[] {
  const ids = new Set(tiles.filter(t => t.source === 'screen' && !t.own).map(t => t.id ?? t.view.deviceId));
  for (const p of v.voice?.[channelId]?.participants ?? []) if (!p.own && participantView(p).screen) ids.add(tileId(p.deviceId, 'screen'));
  return [...ids];
}

/**
 * R26: the one rule that focuses a tile by itself. A new screen tile of another person takes focus while no tile is
 * focused; an id the stage already offered in this call is never offered again until leave (a tile the person unfocused
 * stays unfocused, also across a pause that withholds every remote stream); the own screen never does here (Stage focuses it when this device starts it, Ruling AV). An id is forgotten
 * early only when its person's room flag says the share really ended, so a restarted share may focus again. Focus is
 * dropped when the focused tile goes away. The DM call box has no focus (`dm`): it stays the compact grid.
 */
function useAutoFocus(tiles: readonly CallTileProps[], live: readonly string[], joined: boolean, dm: boolean, focusedId: string | null,
  setFocusedId: (id: string | null) => void) {
  const offered = useRef(new Set<string>());
  const idsKey = tiles.map(t => t.id ?? t.view.deviceId).join('\n');
  const screensKey = tiles.filter(t => t.source === 'screen' && !t.own).map(t => t.id ?? t.view.deviceId).join('\n');
  const liveKey = live.join('\n');
  useEffect(() => {
    if (dm) return;
    if (!joined) { offered.current.clear(); setFocusedId(null); return; }
    const liveNow = liveKey === '' ? [] : liveKey.split('\n');
    for (const id of [...offered.current]) if (!liveNow.includes(id)) offered.current.delete(id);
    const now = screensKey === '' ? [] : screensKey.split('\n');
    const fresh = now.filter(id => !offered.current.has(id));
    for (const id of fresh) offered.current.add(id);
    // The focused tile going away drops the focus first, so a share that arrives in the same update can take it.
    const kept = focusedId !== null && idsKey.split('\n').includes(focusedId) ? focusedId : null;
    if (kept === null && fresh.length > 0) setFocusedId(fresh[0]);
    else if (kept !== focusedId) setFocusedId(null);
  }, [dm, joined, idsKey, screensKey, liveKey, focusedId, setFocusedId]);
}

const STAGE_LABELS = () => ({ code: t('voice.code'), codeHelp: t('voice.codeHelp'), empty: t('voice.empty'), tiles: t('voice.tiles'), grid: t('callStage.grid') });

/**
 * The stage of a voice channel or a DM call, whether this device is in its call, joining it or not (WB a11y I1, I2):
 * one element and one CallStage instance throughout, so its notice region is never remounted with its text (that
 * would not be announced) and focus can be kept inside it when the controls swap for the join button or back.
 */
function Stage({ v, channelId, title, name, outgoing, dm }: { v: VoiceContext; channelId: string; title: string; name?: string; outgoing: { text: string } | null; dm: boolean }) {
  const ref = useRef<HTMLDivElement>(null);
  const phone = usePhoneLayout();
  const joined = v.call.channelId === channelId;
  useStageFocus(ref, v.call, joined);
  const notice = stageNotice(v, channelId);
  // AW5 fix round 3: a removal's notice may come after the cut (it waits for the pending reason), so its arrival is a
  // swap of its own: focus that fell with the controls goes to it then.
  useFocusRescue([ref], joined ? 'joined' : notice !== null ? 'notice' : 'idle', () => stageTarget(ref.current, joined));
  const mine = v.slice?.channelId === channelId;
  const paused = joined && mine && v.slice?.phase === 'paused';
  const code = joined && mine && v.slice?.phase === 'in' ? v.slice.code : null;
  // CallStage focused the pause action in its own effect (children run first): the pause has had its one focus move.
  const { pauseFocusPending, markPauseFocused } = v.call;
  useEffect(() => { if (paused && pauseFocusPending) markPauseFocused(); }, [paused, pauseFocusPending, markPauseFocused]);
  const held = !joined && joinHeld(v, channelId);
  const docState = !joined && notice?.kind === 'blocked' ? 'voice-blocked' : 'voice-stage';
  const allTiles = joined ? callTiles(v, channelId, name ?? title) : [];
  // Signal §5.6: a channel this device is not in shows the preview (who is inside, Join voice); a DM shows its strip instead.
  const people = v.voice?.[channelId]?.participants ?? [];
  const preview = !joined && !dm ? {
    people: people.map(p => ({ id: p.deviceId, name: p.name, speaking: false, bot: p.bot })), name: name ?? title,
    text: previewText(people.map(p => p.name)),
    join: held ? null : { label: t('callStage.preview.join'), onJoin: () => { if (v.onJoinVoice) v.onJoinVoice(channelId); else v.call.join(channelId); } },
  } : undefined;
  // The DM call box is the compact grid: its tiles have no full-screen button (that is positioned for stage tiles).
  // Task 8c: a channel's stage samples its tiles' stats about once a second while it shows (not on a phone, which draws none,
  // and not while paused, when no stream is shown).
  const tileStats = useTileStats(v.call.session, joined && !dm && !phone && !paused);
  const tiles = dm ? allTiles.map(t => ({ ...t, fullscreenLabel: undefined })) : withTileStats(allTiles, tileStats);
  // The state setter is stable, so the stage's pause effect does not re-run on every render (U4 review, M5).
  const [focusedId, setFocusedId] = useState<string | null>(null);
  useAutoFocus(tiles, liveScreenIds(v, channelId, tiles), joined, dm, focusedId, setFocusedId);
  // Fix round 2 (Ruling AV): a share this device starts (Go live, or an Apply that restarts it) takes the focus, as the
  // prototype's goLive does; other people's shares keep R26's rule.
  const ownSharing = joined && !dm && (v.call.state?.sharing ?? false);
  const wasSharing = useRef(ownSharing);
  const ownScreen = tileId(v.ownDevice ?? 'own', 'screen');
  useEffect(() => {
    if (ownSharing && !wasSharing.current) setFocusedId(ownScreen);
    wasSharing.current = ownSharing;
  }, [ownSharing, ownScreen]);
  return (
    <div ref={ref} className="dw-voice-stage" data-doc-state={docState}
      data-call-phase={mine ? v.slice?.phase : joined ? 'joining' : 'idle'}>
      <CallStage title={title} tiles={tiles} {...(name !== undefined && !phone ? { header: <ChannelHeader kind="voice" name={name}
        // Ruling Z: the prototype's voice header: the name and its topic ("{n} in voice" once joined, else "voice channel"), no controls.
        topic={joined ? t('voice.count', { n: v.voice?.[channelId]?.participants.length ?? 0 }) : t('voice.header.idle')} /> } : {})} {...(dm ? {} : { focusedId, onFocus: setFocusedId })} code={code} notice={notice}
        outgoing={joined ? outgoing : null}
        {...(joined && !dm && v.music != null && v.music.track !== undefined ? { nowPlaying: <MusicPlayer view={v.music} variant="stage" /> } : {})}
        {...(preview !== undefined ? { preview } : {})}
        {...(joined && !dm ? { bar: { name: name ?? title, stats: <StatsLine v={v} /> } } : {})}
        controls={joined ? <>{<Controls v={v} paused={paused} dm={dm} />}{dm ? <StageLines v={v} /> : null}</>
          : null}
        labels={STAGE_LABELS()} />
    </div>
  );
}

/**
 * AW6 fix round 1 (Jonas, concern 2): the AFK channel's page. It hosts no call (R-AW1): it lists who is away, on the
 * sidebar row's grammar (`afk.away`, or `afk.empty`), and offers `afk.go`, which runs the AFK row's path (leave the call,
 * then mark away). Once this user is away the button goes and `afk.youAway` says how to come back (Q3: no leave action).
 */
function AfkPage({ v, channelId, name, people, away }: { v: VoiceContext; channelId: string; name: string; people: readonly VoiceParticipantView[];
  away: boolean }) {
  const phone = usePhoneLayout();
  const notice = stageNotice(v, channelId);
  // Fix round 1 (M6): the press that made this user away keeps focus here: the away line that replaces the button takes it.
  const pressed = useRef(false);
  const line = useRef<HTMLParagraphElement>(null);
  useEffect(() => {
    if (!away || !pressed.current) return;
    pressed.current = false;
    line.current?.focus();
  }, [away]);
  return (
    <div className="dw-voice-stage" data-doc-state="voice-stage" data-call-phase="idle" data-afk="true">
      <CallStage title={t('voice.open', { name })} tiles={[]} {...(!phone ? { header: <ChannelHeader kind="voice" name={name} /> } : {})}
        code={null} notice={notice} outgoing={null}
        controls={<div className="dw-afk-page">
          {people.length > 0 ? (
            <ul className="d-voice-row__people dw-afk-roster" aria-label={t('voice.open', { name })}>
              {people.map(p => (
                <li key={p.userId} className="d-voice-row__person" data-away="true">
                  <span className="d-voice-row__avatar" aria-hidden="true"><Avatar name={p.name} size="xs" /></span>
                  <span className="d-voice-row__person-name">{p.name}</span>
                  <span className="d-voice-row__states">{' '}<span className="d-voice-row__state" data-state="away">{t('afk.away')}</span></span>
                </li>
              ))}
            </ul>
          ) : null}
          {away ? <p ref={line} className="dw-afk-away" tabIndex={-1}>{t('afk.youAway')}</p>
            : <Button variant="accent" className="dw-voice-join" onClick={() => {
              pressed.current = true;
              if (v.onJoinVoice) v.onJoinVoice(channelId); else v.call.join(channelId);
            }}>{t('afk.go')}</Button>}
        </div>}
        labels={{ ...STAGE_LABELS(), empty: people.length === 0 ? t('afk.empty') : '' }} />
    </div>
  );
}

/** The voice view of a voice channel route (`/c/<community>/<voice channel>`). */
export function VoiceView({ v, channelId, name }: { v: VoiceContext; channelId: string; name: string }): React.JSX.Element {
  const afk = v.afkPage?.(channelId) ?? null;
  if (afk !== null) return <AfkPage v={v} channelId={channelId} name={name} people={afk.people} away={afk.away} />;
  return <div className="dw-voice-channel">
    <Stage v={v} channelId={channelId} title={t('voice.open', { name })} name={name} outgoing={null} dm={false} />
  </div>;
}

/**
 * The DM strip's notice: a `role="status"` that is always rendered (empty without a notice), so a notice that appears
 * is announced (WB a11y I2); focus moves to it once per text when asked.
 */
function StripNotice({ notice, refObj }: { notice: CallStageNotice | null; refObj: RefObject<HTMLParagraphElement | null> }) {
  const focused = useRef<string | null>(null);
  const text = notice?.text ?? null;
  const focusOnce = notice?.focusOnce ?? false;
  useEffect(() => {
    if (text === null) { focused.current = null; return; }
    if (!focusOnce || focused.current === text) return;
    focused.current = text;
    refObj.current?.focus();
  }, [text, focusOnce, refObj]);
  return (
    <p ref={refObj} className="dw-call-strip__notice" role="status" tabIndex={notice === null ? undefined : -1}
      data-kind={notice?.kind} data-empty={notice === null ? 'true' : undefined}>{text}</p>
  );
}

/**
 * The DM call (flow 06): the call button is an icon in the conversation header (N1); a notice about the call is a strip under
 * the header, and none stands there otherwise. While in the DM's call, its stage sits above the log. A call block never reaches the composer: the strip says it, the composer stays as the text group's gate has it.
 */
/**
 * What a DM's call button does and says (`call.start`, or `call.join` while a call is live), or null where there is nothing to
 * start: this device is in the call, or the join is held. It is an icon in the DM header on every layout (H2, N1).
 */
export function dmCallStart(v: VoiceContext, channelId: string): { label: string; onStart: () => void } | null {
  if (v.call.channelId === channelId || joinHeld(v, channelId)) return null;
  const live = (v.voice?.[channelId]?.participants.length ?? 0) > 0;
  return { label: t(live ? 'call.join' : 'call.start'), onStart: () => { if (v.onJoinVoice) v.onJoinVoice(channelId); else v.call.join(channelId); } };
}

export function DmCall({ v, channelId, name }: { v: VoiceContext; channelId: string; name: string }): React.JSX.Element {
  const outgoing = v.slice?.channelId === channelId ? v.slice.outgoing : null;
  // A ring lasts 30 s (R5, useCall's clock): once that long has passed with nobody answering, the strip says so.
  const inThisCall = v.call.channelId === channelId;
  const others = (v.voice?.[channelId]?.participants ?? []).some(p => p.userId !== v.self?.id) || (v.call.state?.tiles.length ?? 0) > 0;
  const text = inThisCall ? outgoingText(outgoing, others, v.call.ringExpired) : null;
  // The outgoing ring is spoken through the dock's status region, once per change (flow 06 § States, review I3).
  const { announce } = v.call;
  useEffect(() => { if (text !== null) announce(text); }, [text, announce]);
  // On a phone the stage fills the pane and `messages` swaps it for the log and composer (flow 06 § 360×740, review I7);
  // the toggle shows only there (app.css).
  const [view, setView] = useState<'stage' | 'messages'>('stage');
  const stripRef = useRef<HTMLDivElement>(null);
  const callRef = useRef<HTMLDivElement>(null);
  const noticeRef = useRef<HTMLParagraphElement>(null);
  const notice = inThisCall ? null : stageNotice(v, channelId);
  // WB a11y I1: a failed call, a removal or an end swaps the stage for the strip under focus; focus goes to the notice
  // that says why, else the call button (the stage's own focus rule covers the join).
  useFocusRescue([stripRef, callRef], inThisCall ? 'joined' : 'idle', () => (inThisCall ? null
    : notice !== null ? noticeRef.current : document.querySelector<HTMLElement>('[data-dm-call-start]')));
  return (
    <>
      {/* The call button is in the header (N1). The strip is always the first element, empty and collapsed (no band, no hairline)
          until there is a notice, so its status region is never mounted with its text (WB a11y I2). */}
      <div ref={stripRef} className="dw-call-strip" data-doc-state={notice?.kind === 'blocked' ? 'voice-blocked' : undefined}
        data-call-phase={inThisCall ? undefined : v.slice?.channelId === channelId ? v.slice.phase : 'idle'}
        data-empty={notice === null ? 'true' : undefined}>
        <StripNotice notice={notice} refObj={noticeRef} />
        {notice?.action != null ? <Button className="dw-call-strip__action" onClick={() => notice.action?.onAction()}>{notice.action.label}</Button> : null}
      </div>
      {inThisCall ? (
        <div ref={callRef} className="dw-dm-call" data-view={view}>
          <div className="dw-dm-call__bar">
            <Button size="md" pressed={view === 'messages'} onClick={() => setView(view === 'messages' ? 'stage' : 'messages')}>{t('voice.messages')}</Button>
          </div>
          <Stage v={v} channelId={channelId} title={name} outgoing={text === null ? null : { text }} dm />
        </div>
      ) : null}
    </>
  );
}


/** Product walk M3: the devices of this device's call that are speaking now, its own included. */
export function speakingDevices(state: CallView['state'], ownDevice: string | null): ReadonlySet<string> {
  const out = new Set<string>();
  for (const tile of state?.tiles ?? []) if (tile.speaking) out.add(tile.deviceId);
  if (state?.speaking === true && ownDevice !== null) out.add(ownDevice);
  return out;
}

export function rowLabels() {
  return { open: en['voice.open'], count: en['voice.count'], playbackOnly: t('voice.bot.playbackOnly'), state: voiceStateLabels(), away: t('afk.away') };
}

/** web3.4 V2: how many of the remote audio tracks this device is sent it plays ("hearing 1 of 2": one held back, as for a
 *  person whose volume is 0). Shown only while any is expected, and never while deafened or paused; a diagnostic line, so
 *  no status region of its own. The counts are on the element for support and the e2e suite: a small line under the
 *  meter, after the mode line. */
function HearingLine({ audio, silent }: { audio: { attached: number; expected: number } | undefined; silent: boolean }) {
  // Fix round 1: deafened or paused nothing plays, whatever is attached, so no line says it is heard.
  if (audio === undefined || audio.expected <= 0 || silent) return null;
  return (
    <span className="dw-voice-dock__hearing" data-audio-attached={audio.attached} data-audio-expected={audio.expected}>
      {t('voice.diag.tracks', { attached: audio.attached, expected: audio.expected })}
    </span>
  );
}

/** The call's phase as the dock and the phone's call bar show it: paused on a held group, else connecting, reconnecting or connected. */
export function callPhase(v: Pick<VoiceContext, 'call' | 'slice'>): VoiceDockProps['phase'] {
  const { call } = v;
  const id = call.channelId;
  const paused = id !== null && v.slice?.channelId === id && v.slice.phase === 'paused';
  return paused ? 'paused' : call.session === null || call.state?.phase === 'connecting' ? 'connecting'
    : call.state?.phase === 'reconnecting' ? 'reconnecting' : 'connected';
}

/**
 * The call panel above the user panel while this device is in voice (flow 06; Signal §5.4, Ruling U): the channel name (the
 * server is its tooltip and accessible name), the link stats ({ping} · ↑ {up} ↓ {down} kb/s, from call.state.link), and leave,
 * as the prototype's dock. Camera and share are the stage's controls; Diagnostics is in Settings. No mute or deafen (the user
 * panel owns them, R3), and the one polite status region. While the call is paused and its stage is not on screen, the pause
 * banner and its action stand above the dock.
 */
export function Dock({ v, where, server, stageShown }: {
  v: VoiceContext; where: string; /** The community of the call's channel; null for a DM call. */ server: string | null; stageShown: boolean;
}): React.JSX.Element | null {
  const { call } = v;
  const bannerRef = useRef<HTMLDivElement>(null);
  const id = call.channelId;
  const paused = id !== null && v.slice?.channelId === id && v.slice.phase === 'paused';
  const banner = paused && !stageShown ? stageNotice(v, id) : null;
  const bannerText = banner?.text ?? null;
  const { pauseFocusPending, markPauseFocused } = call;
  useEffect(() => {
    if (bannerText === null || !pauseFocusPending) return;
    bannerRef.current?.querySelector('button')?.focus();
    markPauseFocused();
  }, [bannerText, pauseFocusPending, markPauseFocused]);
  if (id === null) return null;
  const phase = callPhase(v);
  const link = call.state?.link;
  return (
    // Signal §5.6 (the prototype's showDock): while this channel's own stage is on screen, its call bar says where and how
    // the call is, so the dock draws nothing; its status region stays (the one polite region of the call). A DM keeps it.
    <div className="dw-voice-dock" data-doc-state="voice-dock" data-on-stage={stageShown && server !== null ? 'true' : undefined}>
      {banner !== null ? (
        <div ref={bannerRef} className="dw-voice-dock__paused" role="status">
          <p id="dw-voice-dock-paused">{banner.text}</p>
          {banner.action !== null ? (
            <Button aria-describedby="dw-voice-dock-paused" onClick={() => banner.action?.onAction()}>{banner.action.label}</Button>
          ) : null}
        </div>
      ) : null}
      {/* web3.3: the browser holds call audio back (its autoplay rule); one press starts it, right above the controls.
          Review M1: no status region of its own; useCall says the hint once through the dock’s announcement. */}
      {call.state?.audioBlocked === true ? (
        <div className="dw-voice-dock__audio">
          <p id="dw-voice-dock-audio">{t('voice.audioBlocked')}</p>
          <Button aria-describedby="dw-voice-dock-audio" onClick={call.startAudio}>{t('voice.audioStart')}</Button>
        </div>
      ) : null}
      <VoiceDock phase={phase} where={where} muted={call.state?.muted ?? false} deafened={call.state?.deafened ?? false}
        camera={call.state?.camera ?? false} sharing={call.state?.sharing ?? false} canCamera={!paused}
        canShare={!paused && canCapture()} busy={call.session === null} paused={paused}
        {...(link === undefined ? {} : { stats: { rttMs: link.rttMs, upKbps: link.upKbps, downKbps: link.downKbps, quality: linkQuality(link.rttMs, link.lossPct) } })}
        announcement={call.announcement}
        {...(server !== null && !stageShown && v.music != null && v.music.track !== undefined ? { music: <MusicPlayer view={v.music} variant="compact" /> } : {})}
        labels={{ region: t('voice.dock.region'), connecting: t('voice.dock.connecting'),
          ping: en['voice.dock.ping'], pingSub: en['voice.dock.pingSub'], pingNone: en['voice.dock.pingNone'], rate: en['voice.dock.rate'], stats: en['voice.dock.stats'],
          quality: { good: en['voice.dock.quality.good'], poor: en['voice.dock.quality.poor'], bad: en['voice.dock.quality.bad'] },
          where: server === null ? where : t('layout.dock.where', { channel: where, server }),
          reconnecting: t('voice.dock.reconnecting'), paused: en['voice.dock.paused'], mute: t('voice.mute'),
          deafen: t('voice.deafen'), leave: t('voice.leave'), camera: t('voice.cameraOn'), share: t('voice.shareOn'), why: VOICE_WHY() }}
        onLeave={call.leave} />
    </div>
  );
}
