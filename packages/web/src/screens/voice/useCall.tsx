// The shell's side of a call (web-3 task 9, flow 06): it starts and ends the page call session (L-WEB-50) through the
// lazy call module (L-WEB-51), drives the session from the worker's `call` slice and binds the voice shortcuts and push to
// talk. The session is the only holder of media and keys; this hook keeps none of either.
//
// The call slice drives the session (task-8b rulings): phase 'paused' (RV-3) calls setPaused(true) and the way back to
// 'in' calls setPaused(false); 'blocked' or 'removed' calls leave() and never setPaused(false), so a block can never
// unpause media on its way out.
import { createContext, useCallback, useContext, useEffect, useRef, useState } from 'react';
import type { CallState, CoreClient } from '@dilla/client-core';
import { SETTINGS } from '@dilla/client-core';
import { isPushToTalkKey, showToast } from '@dilla/ui';
import { shareAudioAdvice } from '../../call/share-audio-advice.ts';
import { desktopBridge } from '../../desktop.ts';
import { DEFAULT_SHARE_OPTIONS, type ShareOptions } from '../../call/share-options.ts';
import { voicePrefs, type VoicePrefs } from '../../call/voice-prefs.ts';
import { loadCallModule, type CallModule } from '../../call/load.ts';
import type { PageCall, PageCallState } from '../../call/session.ts';
import { createSounds, type Sounds } from '../../call/sounds.ts';
import { errorOf } from '../../core/errors.ts';
import { t, type StringKey } from '../../strings/index.ts';
import { joinFailureKey } from '../shell-model.ts';

/** What the shell needs of the lazy call module. */
export type VoiceModule = Pick<CallModule, 'startPageCall' | 'isVoiceSupported' | 'localDecodeList' | 'bindPushToTalk' | 'browserDeps'
  | 'volumesFromSettings' | 'streamVolumesFromSettings'>;

/** The shell's voice dependencies: the call module (loaded on the first join) and the sounds. Tests give doubles. */
export interface VoiceDeps {
  load(): Promise<VoiceModule>;
  createSounds(): Sounds;
  /** web3.3 fix round 2 (Minor 1): the call’s AudioContext, made synchronously in the join’s press so the browser lets it
   *  run (browsers start an AudioContext made outside a user gesture suspended, as after the module load and a switch’s
   *  leave). A new AudioContext by
   *  default; null where the browser has none. */
  createCallContext?(): AudioContext | null;
}

type SelfValue = 'muted' | 'deafened' | null;
const HELD_KEY = 'dilla.voiceSelfHeld';
const RANK: Record<string, number> = { muted: 1, deafened: 2 };
/** The more private of two own states: deafened over muted over none. */
const morePrivate = (a: SelfValue, b: SelfValue): SelfValue => ((RANK[b ?? ''] ?? 0) > (RANK[a ?? ''] ?? 0) ? b : a);
/**
 * Final review m3: an in-call mute or deafen whose `voice.self` write the store refused is kept in this tab's
 * sessionStorage as well as in memory, so a reload (the update banner, a version reload) resumes the call in the state it
 * was in. It is read only toward privacy (`morePrivate`) and dropped as soon as a write is answered; a blocked storage
 * keeps nothing, and then the stored setting alone speaks, as before.
 */
function readCarried(client: CoreClient): SelfValue {
  const id = client.get('account')?.user?.id;
  if (id === undefined) return null;
  // eslint-disable-next-line no-restricted-globals -- the own mute is a per-tab privacy preference, not a key, token or secret
  try { const v = sessionStorage.getItem(`${HELD_KEY}.${id}`); return v === 'muted' || v === 'deafened' ? v : null; } catch { return null; }
}
/**
 * Keyed by the signed-in user's id: another account in the same tab never reads this one's held state (it reads none, so
 * it starts from its own stored setting; no account known means nothing is carried or read, as before this fix).
 */
function carry(client: CoreClient, value: SelfValue): void {
  const id = client.get('account')?.user?.id;
  if (id === undefined) return;
  // eslint-disable-next-line no-restricted-globals -- see readCarried
  try { if (value === null) sessionStorage.removeItem(`${HELD_KEY}.${id}`); else sessionStorage.setItem(`${HELD_KEY}.${id}`, value); } catch { /* blocked storage: nothing carried */ }
}

/** Sign-out and wipe end in phase `cleared` (a revoked device in `revoked`): nothing of any account's held mute stays in the tab. */
function dropAllCarried(): void {
  try {
    // eslint-disable-next-line no-restricted-globals -- see readCarried
    for (const key of Object.keys(sessionStorage)) if (key.startsWith(HELD_KEY)) sessionStorage.removeItem(key);
  } catch { /* blocked storage: nothing was carried */ }
}

let audio: AudioContext | null = null;
/** One AudioContext for the sounds, made on first use; null where the browser has none. */
function soundContext(): AudioContext | null {
  if (audio !== null) return audio;
  const Ctor = (globalThis as { AudioContext?: typeof AudioContext }).AudioContext;
  if (Ctor === undefined) return null;
  try { audio = new Ctor(); } catch { return null; }
  return audio;
}

function callContext(): AudioContext | null {
  const Ctor = (globalThis as { AudioContext?: typeof AudioContext }).AudioContext;
  if (Ctor === undefined) return null;
  try { return new Ctor(); } catch { return null; }
}
/** Closes a call context this hook made but never handed to a session (the session closes the ones it was given). */
function closeContext(ctx: AudioContext | null | undefined): void {
  if (ctx == null) return;
  try { void Promise.resolve(ctx.close()).catch(() => undefined); } catch { /* already closed */ }
}

export const VoiceDepsContext = createContext<VoiceDeps>({ load: loadCallModule, createSounds: () => createSounds(soundContext),
  createCallContext: callContext });

/** A transient notice of a start or a control (micDenied and the like are read from the session's state instead). `text`,
 *  when set, is the notice's words already filled (the outgoing ring's last line, product walk I6). */
export interface CallNotice { channelId: string; key: StringKey; text?: string }

/** A ring lasts 30 s (R5). */
export const RING_MS = 30_000;
/** Product walk I6: a DM call nobody answered ends this long after its outgoing ring ended (the grace covers an answer
 *  that is still connecting). */
export const UNANSWERED_LEAVE_MS = 5_000;

/**
 * The outgoing ring's line (R3, R7): `ringing {names}` while it rings, `{names} not rung: …` for the participants the
 * instance did not ring (product walk I6), `{name} declined.` per decline, `No answer.` once every ring ended (30 s, or
 * nobody left ringing or waiting) with nobody else in the call; null when there is nothing to say.
 */
export function outgoingText(outgoing: CallState['outgoing'], othersInCall: boolean, expired: boolean): string | null {
  if (outgoing === null) return null;
  const parts: string[] = [];
  if (outgoing.ringing.length > 0 && !expired) parts.push(t('call.outgoing.ringing', { names: outgoing.ringing.join(', ') }));
  if (outgoing.notRung.length > 0 && !othersInCall) parts.push(t('call.outgoing.notRung', { names: outgoing.notRung.join(', ') }));
  for (const name of outgoing.declined) parts.push(t('call.outgoing.declined', { name }));
  if (unanswered(outgoing, othersInCall, expired) && outgoing.declined.length === 0) parts.push(t('call.outgoing.noAnswer'));
  return parts.length > 0 ? parts.join(' ') : null;
}

/** Every ring of the outgoing call has ended (30 s, or nobody left ringing or waiting to be rung) with nobody else in it. */
function unanswered(outgoing: NonNullable<CallState['outgoing']>, othersInCall: boolean, expired: boolean): boolean {
  return !othersInCall && (expired || (outgoing.ringing.length === 0 && outgoing.notRung.length === 0));
}

export interface CallView {
  /** The channel or DM this device is in, or is joining. */
  channelId: string | null;
  /** A voice room lost during a service restart, retained until its automatic join succeeds or the person leaves. */
  recoveryChannelId: string | null;
  recoveryCamera: boolean;
  recoverySharing: boolean;
  recentlyEnded: { channelId: string; camera: boolean; sharing: boolean; at: number } | null;
  joining: boolean;
  session: PageCall | null;
  state: PageCallState | null;
  /** The dock's polite announcement (join, mute, deafen, the input mode, the end of a pause, the outgoing ring). */
  announcement: string;
  /**
   * What is said after the dock is gone (task-9 rulings, review I6): "You left {where}.", and a block or a removal while
   * the stage is not on screen. The shell renders it in a status region that outlives the dock.
   */
  lasting: string;
  announce: (text: string) => void;
  /** True until the current pause banner took focus once, wherever it shows (review m4). */
  pauseFocusPending: boolean;
  markPauseFocused: () => void;
  notice: CallNotice | null;
  /** The channel whose call this device left because of a block while in it (voice.blocked rather than blockedJoin). */
  blockedMidCall: string | null;
  /** True once per attached join: the stage that shows it moves focus to its first control and takes the flag. */
  takeStageFocus: () => boolean;
  unsupported: boolean;
  /** The outgoing ring of the DM call this device opened has run its 30 s (product walk I6). */
  ringExpired: boolean;
  join: (channelId: string) => void;
  restore: (channelId: string, camera: boolean, sharing: boolean) => void;
  /** `after`: the notice the stage or strip shows once left (product walk I6: the unanswered ring's last line). */
  leave: (after?: CallNotice) => void;
  closeUnsupported: () => void;
  toggleMute: () => void;
  toggleDeafen: () => void;
  /**
   * The user panel's mute and deafen (web3.8, R25). In a call they are `toggleMute`/`toggleDeafen`; outside one they write the
   * device-local `voice.self` setting, which the next join starts in.
   */
  selfMute: () => void;
  selfDeafen: () => void;
  /** The own state the user panel shows: the session's in a call, else the `voice.self` setting (deafened counts as muted). */
  selfMuted: boolean;
  selfDeafened: boolean;
  toggleCamera: () => void;
  toggleShare: () => void;
  /** Task 8c: the share picker's choices, kept for this page (the next share starts with them). */
  shareOptions: ShareOptions;
  /** Task 8c: the picker's Go live (starts a share with `options`) and Apply while sharing (`setShareOptions`). */
  startShare: (options: ShareOptions) => void;
  applyShare: (options: ShareOptions) => void;
  setVolume: (userId: string, volume: number) => void;
  /** #37 T5: the volume of a person's shared screen sound; 0 is the stream mute. */
  setStreamVolume: (userId: string, volume: number) => void;
  /** web3.3: the "turn on audio" press while the browser holds call audio back (state.audioBlocked). */
  startAudio: () => void;
}

export interface UseCallOptions {
  client: CoreClient;
  call: CallState | undefined;
  settings: Readonly<Record<string, string>>;
  /** The channel's or DM's name for {where}. */
  whereOf(channelId: string): string;
  /** Called once a leave the person asked for finished: focus goes back to where the call came from. */
  onLeft(channelId: string): void;
  /** Whether the call's stage is on screen (its notices then speak for themselves). */
  stageShown(channelId: string): boolean;
  /** A community voice room closed during a known app upgrade may be opened again. */
  recoverEnded?(channelId: string): boolean;
  /** The desktop shell shows the user panel, so Control/⌘+Shift+M and D also work outside a call (web3.8, pre-flight C-10). */
  selfShortcuts?: boolean;
  /** The shell's own polite announcer: outside a call there is no dock status region to say a mute or deafen. */
  announce?(text: string): void;
  /**
   * AW5: the built-in AFK channels. Nobody joins a call there (R-AW1): the person's press on one leaves the current call
   * and marks them away in its community (R-AW3). The `afk:` slice of a channel's community says so too, so a channel
   * this set does not name yet is still never joined.
   */
  afk?: { channels: ReadonlySet<string> };
}

/** The mute and deafen shortcut letters (WB a11y m1: matched on the letter the key types, so AZERTY's M is M). */
const KEY_M = 'm';
const KEY_D = 'd';
/** A session that ended on its own: the notice that says why (WB client I3; flow 06 § States). */
const END_NOTICE: Record<string, StringKey> = { E_CALL_ENDED: 'voice.ended', E_REMOVED: 'voice.removed' };
/** web3.9 WEB-AFK, AW5: the longest a removal's notice is held while the `afk:` read that says the sweep cut this device
 *  (`pending`) may still be coming. The server's cut can land before that read. */
export const REMOVED_HOLD_MS = 2_000;
/** AW5 fix round 1 (I1): the longest a restore or a recovery waits for its channel's community and a fresh AFK read. */
export const AFK_READ_WAIT_MS = 5_000;
/** Fix round 2 (B-M2): the longest an upgrade restore keeps asking for its channel's AFK state: the shell's upgrade
 *  rejoin window. */
export const RESTORE_WAIT_MS = 120_000;
/** Late rejoin (Jonas 2026-10-21): a gap between two of the call's liveness ticks (one a second) longer than this is a
 *  suspend (a closed lid, a sleep); the device has been gone since the tick before it. */
export const SUSPEND_GAP_MS = 5_000;
/** Fix round 1 (review I1): a suspend stays part of the absence until the call shows it came through: the session went
 *  reconnecting (or connecting) after the wake and then connected again, or it stayed connected, never reconnecting, for
 *  this long after the wake (the connection survived a short suspend). A reconnect that is still trying holds it however
 *  long it takes (LiveKit's retries take about a minute). */
export const SUSPEND_SETTLE_MS = 60_000;

/** One moment on both of the page's clocks. */
interface Stamp { perf: number; wall: number }
const stampNow = (): Stamp => ({ perf: performance.now(), wall: Date.now() });
/** How long ago `s` was: the larger of the monotonic and the wall clock's answer, since performance.now stops during a
 *  suspend in some browsers (it is not specified to count it) while the wall clock always moves on. */
const elapsedSince = (s: Stamp): number => Math.max(performance.now() - s.perf, Date.now() - s.wall);
/** How long a gap between two stamps lasted, by the same larger-clock rule. */
const gapLength = (g: { from: Stamp; to: Stamp }): number => Math.max(g.to.perf - g.from.perf, g.to.wall - g.from.wall);

export function useCall(o: UseCallOptions): CallView {
  const deps = useContext(VoiceDepsContext);
  const { client } = o;
  const [session, setSession] = useState<PageCall | null>(null);
  const [state, setState] = useState<PageCallState | null>(null);
  const [channelId, setChannelId] = useState<string | null>(null);
  const [recoveryChannelId, setRecoveryChannelId] = useState<string | null>(null);
  const [recoveryCamera, setRecoveryCamera] = useState(false);
  const [recoverySharing, setRecoverySharing] = useState(false);
  const [recentlyEnded, setRecentlyEnded] = useState<CallView['recentlyEnded']>(null);
  const recoveryRef = useRef<string | null>(null);
  const recoveryCameraRef = useRef(false);
  const [joining, setJoining] = useState(false);
  const [announcement, setAnnouncement] = useState('');
  const [lasting, setLasting] = useState('');
  const [pauseFocusPending, setPauseFocusPending] = useState(false);
  const [notice, setNotice] = useState<CallNotice | null>(null);
  const [blockedMidCall, setBlockedMidCall] = useState<string | null>(null);
  const stageFocus = useRef(false);
  const [unsupported, setUnsupported] = useState(false);
  const sessionRef = useRef<PageCall | null>(null);
  const channelRef = useRef<string | null>(null);
  const unsubscribe = useRef<(() => void) | null>(null);
  const starting = useRef(false);
  /** WB client I1: the start in progress can be cancelled (a leave during it, or a join of another channel). */
  const startAbort = useRef<AbortController | null>(null);
  /** WB client I1: a join (or a ring's answer) of another channel asked for while a start ran: it starts once that one
   *  has been cancelled. */
  /** The join that waits for a cancelled start to end, with the call context made in its own press. */
  const queuedJoin = useRef<{ id: string; ctx: AudioContext | null } | null>(null);
  /** AW5: the start in progress, settled once it ended (a cancelled one included); choosing AFK waits for it. */
  const startDone = useRef<Promise<void> | null>(null);
  /** WB client m1: the leave in flight; a join waits for it, so leave-then-join is not refused as a call in progress. */
  const leaving = useRef<Promise<unknown> | null>(null);
  const mounted = useRef(true);
  const paused = useRef(false);
  /** The current hold (WB security I1) already dropped the session's media keys. */
  const keysDropped = useRef(false);
  const module = useRef<VoiceModule | null>(null);
  /** web3.4 V1: the output device the session is asked to play to: the one it started with, then each one Settings moved
   *  it to. `outputPlaying` is the last one the session took (fix round 1): a refusal falls back to it. */
  const outputInForce = useRef('');
  /** Signal task 10b: Settings → Voice's levels and switches the session started with or was last given. */
  const prefsInForce = useRef<VoicePrefs | null>(null);
  const outputPlaying = useRef('');
  // The join button the unsupported dialog was opened from: focus returns there when it closes (flow 06).
  const opener = useRef<HTMLElement | null>(null);
  const live = useRef(o);
  live.current = o;
  /** When the current call attached (R-AW7: an away mark at or after it means the sweep cut this call). */
  const attachedAt = useRef(0);
  /** AW5: the call whose start found its community's `afk:` slice already saying pending (that read's seq). The server
   *  cleared that pending at this call's admission, so the same read never makes this call's cut the sweep's. */
  const stalePending = useRef<{ channelId: string; cid: string; seq: number | undefined } | null>(null);
  /** Fix round 1 (M1): the away row of this user that the call's community held when the call attached (server
   *  seconds, null for none); a different one later is after the call (R-AW7). */
  const awayAtAttach = useRef<{ channelId: string; since: number | null } | null>(null);
  /** The mark a recovery compares against: the call's own (E_DISCONNECTED), or null for a restore (any away row counts). */
  const recoveryMark = useRef<{ since: number | null } | null>(null);
  /** Fix round 3 (B2-M4, B2-n2): a restore's recovery ends as lost at this time (the upgrade's rejoin window); null for
   *  a recovery after a lost connection, which only the join's refusals, a pending reason or the person end. */
  const recoveryUntil = useRef<number | null>(null);
  /** Late rejoin (Jonas 2026-10-21): since when the device has been gone, for a recovery after a lost connection (null for
   *  a restore and an upgrade's recovery, which keep RESTORE_WAIT_MS). A recovery gone longer than its community's AFK
   *  timeout joins nothing. */
  const recoveryGoneFrom = useRef<Stamp | null>(null);
  /** The attached call's liveness: its last tick (one a second) and the suspend found between two ticks that the call has
   *  not yet shown it came through (`reconnecting`: the session left 'connected' after the wake). A later gap extends a held
   *  one only while it is reconnecting; otherwise the longer of the two is kept. */
  const alive = useRef<{ last: Stamp; gap: { from: Stamp; to: Stamp; reconnecting: boolean } | null } | null>(null);
  const tickAlive = useCallback((): void => {
    const a = alive.current;
    if (a === null) return;
    const stalled = elapsedSince(a.last);
    if (stalled > SUSPEND_GAP_MS) {
      const held = a.gap;
      // Fix round 2: a held gap whose call went reconnecting is one absence still going on, so a new gap extends it. One
      // still settling (connected all along) is not: stalls of a background tab or a long task never add up. The new gap
      // replaces it, unless the held one was longer (a long sleep then a stall right after the wake keeps the sleep).
      if (held !== null && held.reconnecting) held.to = stampNow();
      else if (held === null || stalled >= gapLength(held)) a.gap = { from: a.last, to: stampNow(), reconnecting: false };
    }
    const phase = sessionRef.current?.state.phase;
    if (a.gap !== null && (phase === 'reconnecting' || phase === 'connecting')) {
      a.gap.reconnecting = true;
    } else if (a.gap !== null && !a.gap.reconnecting && phase === 'connected'
      && elapsedSince(a.gap.to) > SUSPEND_SETTLE_MS) {
      // The connection stayed up through the suspend and well after it: the suspend is over.
      a.gap = null;
    }
    a.last = stampNow();
  }, []);
  /** The session's phase as the call's subscriber sees it: leaving 'connected' after a suspend marks it reconnecting, and
   *  connected again after that clears the suspend (the call came through it). */
  const phaseAlive = useCallback((phase: PageCallState['phase']): void => {
    const gap = alive.current?.gap;
    if (gap == null) return;
    if (phase === 'reconnecting' || phase === 'connecting') gap.reconnecting = true;
    else if (phase === 'connected' && gap.reconnecting && alive.current !== null) alive.current.gap = null;
  }, []);
  /** When the device was last known to be here, as its connection is found lost: before a suspend the call has not come
   *  through, else now (a drop while awake is counted from when it is noticed). A suspend the call went reconnecting
   *  after counts all of its time since (the reconnect never came through). One still settling (connected all along
   *  since the wake) counts only its own length: the awake, connected time after it is not an absence, and counting it
   *  made the kept gap's age, not its length, decide (flake fix, PR #67: a 10 s stall kept from 70 s back read as 70 s). */
  const goneSince = useCallback((): Stamp => {
    tickAlive();
    const a = alive.current;
    const gap = a?.gap;
    if (gap != null && gap.reconnecting) return gap.from;
    const now = stampNow();
    if (gap != null) { const len = gapLength(gap); return { perf: now.perf - len, wall: now.wall - len }; }
    return now;
  }, [tickAlive]);
  /** Sets the absence a recovery of `id` is measured from: a lost connection's (`lost`), kept when the same recovery is
   *  armed again (the session's end and the worker's slice both arm it); none for an upgrade's recovery. */
  const armGoneFrom = useCallback((id: string, lost: boolean): void => {
    if (!lost) { recoveryGoneFrom.current = null; return; }
    if (recoveryRef.current === id && recoveryGoneFrom.current !== null) return;
    recoveryGoneFrom.current = goneSince();
  }, [goneSince]);
  /** Fix round 1 (I3, N2): the person's choices in order (a join, a restore, AFK); an away mark overtaken is dropped. */
  const intent = useRef(0);
  /** Fix round 1 (I3): the away mark in flight, settled (never rejecting); a join waits for it. */
  const awayPut = useRef<Promise<void> | null>(null);
  /** WEB-AFK: a removal notice held for the sweep's read: its timer and its watch of the `afk:` slice; any join, a later
   *  removal or an unmount drops it. */
  const removedHold = useRef<{ timer: ReturnType<typeof setTimeout>; off: () => void } | null>(null);
  /** M2: a removal's decision in flight (its fresh read) is dropped by any later hold, join or removal. */
  const removedSeq = useRef(0);
  const dropHold = useCallback((): void => {
    removedSeq.current += 1;
    const hold = removedHold.current;
    if (hold === null) return;
    removedHold.current = null;
    clearTimeout(hold.timer);
    hold.off();
  }, []);
  /** The community whose channel list holds `id`, or null while no loaded list does. */
  const communityOf = useCallback((id: string): string | null => {
    for (const c of client.get('communities') ?? []) {
      if ((client.get(`channels:${c.id}`) ?? []).some((ch) => ch.id === id)) return c.id;
    }
    return null;
  }, [client]);
  /** AW5 (R-AW1): whether `id` is a built-in AFK channel, by the shell's set or by its community's `afk:` slice. */
  const isAfk = useCallback((id: string): boolean => {
    if (live.current.afk?.channels.has(id) === true) return true;
    const cid = communityOf(id);
    return cid !== null && client.get(`afk:${cid}`)?.channelId === id;
  }, [client, communityOf]);
  /** AW5: the unix second this user became away in community `cid`, or null while they are not (or it is unread). */
  const awaySince = useCallback((cid: string): number | null => {
    const me = client.get('account')?.user?.id;
    if (me === undefined) return null;
    return client.get(`afk:${cid}`)?.away?.find((row) => row.userId === me)?.since ?? null;
  }, [client]);
  /** AW5 fix round 3: the community whose `afk:` read tells this device's pending reason for call `id`: its own, or for a
   *  DM call any listed community (the reason is device-scoped: every community's read says it); null when there is none. */
  const reasonCommunity = useCallback((id: string): string | null => communityOf(id) ?? client.get('communities')?.[0]?.id ?? null,
    [client, communityOf]);
  /** The name of the AFK channel for `afk.moved`. Round 3b: pending is device-scoped (every community reports it), so the
   *  community is the cut call's own; a DM call (no own community) falls back to a community whose away roster lists this
   *  user; else 'AFK'. */
  const afkNameFor = useCallback((id: string): string => {
    const nameIn = (c: string): string | undefined => {
      const afkId = client.get(`afk:${c}`)?.channelId ?? null;
      return afkId === null ? undefined : (client.get(`channels:${c}`) ?? []).find((ch) => ch.id === afkId)?.name;
    };
    const own = communityOf(id);
    if (own !== null) return nameIn(own) ?? 'AFK';
    for (const c of client.get('communities') ?? []) {
      if (awaySince(c.id) !== null) return nameIn(c.id) ?? 'AFK';
    }
    return 'AFK';
  }, [client, communityOf, awaySince]);
  /**
   * AW5 fix round 3 (B2-I1, B2-M2, B2-M3, B2-n1): why the call `id` was cut, from the instance's pending reason for this
   * device (R-AW6): 1 the idle sweep moved it ('Moved to AFK'), 2 this user chose AFK on another device or tab ('You went
   * AFK on another device.'); null for none. A read the call started under is stale (the instance cleared it at this
   * admission). Nothing is inferred from away rows.
   */
  const cutReason = useCallback((id: string): { reason: 1 | 2; cid: string } | null => {
    const cid = reasonCommunity(id);
    if (cid === null) return null;
    const afk = client.get(`afk:${cid}`);
    if (afk == null) return null;
    const reason = afk.pendingReason ?? (afk.pending ? 1 : 0);
    if (reason === 0) return null;
    const stale = stalePending.current;
    if (stale !== null && stale.channelId === id && stale.cid === cid && stale.seq === afk.seq) return null;
    return { reason, cid };
  }, [client, reasonCommunity]);
  /** A fresh `afkRead` of `cid`, bounded by AFK_READ_WAIT_MS: 'ok', 'gone' (the community is not listed: E_BAD_INPUT) or
   *  'failed' (a failed or late read). It never rejects. */
  const readAfk = useCallback(async (cid: string): Promise<'ok' | 'gone' | 'failed'> => {
    let timer: ReturnType<typeof setTimeout> | undefined;
    const answer = await Promise.race([
      client.call({ m: 'afkRead', communityId: cid, timeoutMs: AFK_READ_WAIT_MS })
        .then(() => 'ok' as const, (e: unknown) => (errorOf(e).code === 'E_BAD_INPUT' ? 'gone' as const : 'failed' as const)),
      new Promise<'failed'>((r) => { timer = setTimeout(() => r('failed'), AFK_READ_WAIT_MS); }),
    ]);
    clearTimeout(timer);
    return answer;
  }, [client]);
  /** AW5: says a removal. A cut with a pending reason (R-AW6) says `afk.moved` or `afk.elsewhere` instead, and nothing
   *  joins or recovers. The reason may land after the cut, so the notice waits for a fresh read of it (at most
   *  AFK_READ_WAIT_MS) and at least REMOVED_HOLD_MS for the instance's own re-read; with no reason it says `say`. */
  const sayRemoved = useCallback((id: string, say: () => void): void => {
    dropHold();
    const settle = (): boolean => {
      const cut = cutReason(id);
      if (cut === null) return false;
      if (cut.reason === 1) {
        const text = t('afk.moved', { channel: afkNameFor(id) });
        setNotice({ channelId: id, key: 'afk.moved', text });
        if (!live.current.stageShown(id)) setLasting(text);
      } else {
        // M2: AFK chosen on another device or tab cut this call: said calmly, never as a removal.
        setNotice({ channelId: id, key: 'afk.elsewhere' });
        if (!live.current.stageShown(id)) setLasting(t('afk.elsewhere'));
      }
      return true;
    };
    if (settle()) return;
    const cid = reasonCommunity(id);
    if (cid === null) { say(); return; }
    const mine = removedSeq.current;
    let reading = true;
    let holding = true;
    const finish = (): void => {
      if (mine !== removedSeq.current || reading || holding) return;
      dropHold();
      if (mounted.current && !settle()) say();
    };
    const off = client.subscribe(`afk:${cid}`, () => {
      if (mine !== removedSeq.current || cutReason(id) === null) return;
      dropHold();
      if (mounted.current) settle();
    });
    const timer = setTimeout(() => { holding = false; finish(); }, REMOVED_HOLD_MS);
    removedHold.current = { timer, off };
    void readAfk(cid).then(() => { reading = false; finish(); });
  }, [client, reasonCommunity, cutReason, afkNameFor, readAfk, dropHold]);
  /** R25: outside a call the own mute and deafen are the device-local `voice.self` setting, which the next join starts in.
   *  `written` is the last value sent and not yet answered, so two quick presses read one source (the second sees the first)
   *  and never the stale slice. A refused press outside a call drops it, and the panel (which reads the stored slice) never
   *  claimed it. web3.8 MUTE: an in-call toggle whose write is refused is `held` instead: the session was in that state, so
   *  a rejoin (after a disconnect) starts in it and the panel shows it, until a later write is answered. */
  const written = useRef<{ seq: number; value: string | null; held?: true } | null>(null);
  const writeSeq = useRef(0);
  // A reload passes through `loading` with no user and must keep the held state, so only the end of an account clears it.
  useEffect(() => client.subscribe('account', () => {
    const phase = client.get('account')?.phase;
    if (phase === 'cleared' || phase === 'revoked') dropAllCarried();
  }), [client]);
  /** web3.8 MUTE: the own mute or deafen as it stands now: the last press not yet answered (or held), else the stored setting.
   *  The join starts in it, the session reads it again before it publishes and at connected, and the attach reconcile
   *  reads it last; all three apply it only toward privacy. */
  const selfNow = useCallback((): 'muted' | 'deafened' | null => {
    const value = written.current !== null ? written.current.value : (client.get('settings') ?? {})[SETTINGS.voiceSelf];
    const stored: SelfValue = value === 'deafened' ? 'deafened' : value === 'muted' ? 'muted' : null;
    return written.current !== null ? stored : morePrivate(stored, readCarried(client));
  }, [client]);
  /** web3.8 MUTE: push, not only pull. A starting session registers here (watchSelf) and is told of each press at once,
   *  after `written` holds it, so selfNow already answers the new value. */
  const selfWatchers = useRef(new Set<() => void>());
  const watchSelf = useCallback((apply: () => void) => {
    const entry = (): void => { apply(); };
    selfWatchers.current.add(entry);
    return () => { selfWatchers.current.delete(entry); };
  }, []);
  const pushSelf = useCallback(() => {
    for (const apply of [...selfWatchers.current]) {
      try { apply(); } catch { /* a session that throws keeps the pulls and the attach reconcile */ }
    }
  }, []);

  // WB a11y I3: the same text again (a second "You joined harbour.") is said again: the region is emptied first and the
  // text set a moment later, since setting identical text changes nothing a screen reader hears.
  const announceTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  /** Product walk I6: someone else was in this call at some point (it was answered); it then never ends by itself. */
  const answered = useRef(false);
  const announce = useCallback((text: string) => {
    if (announceTimer.current !== null) { clearTimeout(announceTimer.current); announceTimer.current = null; }
    setAnnouncement(prev => {
      if (prev !== text || text === '') return text;
      announceTimer.current = setTimeout(() => { announceTimer.current = null; setAnnouncement(text); }, 100);
      return '';
    });
  }, []);

  /** Forgets the session (its leave is the caller's); nothing of it is used after this. */
  const detach = useCallback(() => {
    unsubscribe.current?.();
    unsubscribe.current = null;
    sessionRef.current = null;
    channelRef.current = null;
    paused.current = false;
    keysDropped.current = false;
    setSession(null);
    setState(null);
    setChannelId(null);
    // WB a11y I3: the dock's region goes with the dock; a later dock starts empty, so its first announcement is heard.
    setAnnouncement('');
  }, []);

  const attach = useCallback((next: PageCall, id: string) => {
    answered.current = false;
    attachedAt.current = Date.now();
    // AW5: a pending read this call starts under is the server's earlier one, cleared at this admission (the read of its
    // own community, or for a DM call the one its pending reason is read from).
    const home = communityOf(id);
    const readCid = reasonCommunity(id);
    const afkNow = readCid === null ? null : client.get(`afk:${readCid}`);
    stalePending.current = readCid !== null && afkNow?.pending === true ? { channelId: id, cid: readCid, seq: afkNow.seq } : null;
    awayAtAttach.current = { channelId: id, since: home === null ? null : awaySince(home) };
    sessionRef.current = next;
    channelRef.current = id;
    setSession(next);
    setState(next.state);
    setChannelId(id);
    // web3.3 review M1: the browser holding call audio back is said once, in the dock’s status region, on each change
    // from playing to held back (a call that starts held back says it with the join).
    let audioBlocked = next.state.audioBlocked;
    let cameraBeforeLoss = next.state.camera;
    let sharingBeforeLoss = next.state.sharing;
    // Fix round 2 (Nit 3): a hold that arrives in the same tick as the join’s announcement joins it, never replaces it.
    const joined = t('voice.announce.joined', { where: live.current.whereOf(id) });
    let sharedSilent = false;
    let joinTick = true;
    setTimeout(() => { joinTick = false; }, 0);
    unsubscribe.current = next.subscribe(() => {
      if (sessionRef.current !== next) return;
      setState(next.state);
      phaseAlive(next.state.phase);
      // #37: a share that went out without sound tells its sharer once, in the stage's status region (D3, D4: only after
      // the share started); the notice goes when the share gets sound again or stops.
      const silentShare = next.state.shareAudio === 'none';
      if (silentShare && !sharedSilent) setNotice({ channelId: id, key: shareAudioKey() });
      else if (!silentShare && sharedSilent) setNotice(n => (n?.key.startsWith('voice.shareAudio.none') ? null : n));
      sharedSilent = silentShare;
      if (next.state.phase !== 'ended') {
        cameraBeforeLoss = next.state.camera;
        sharingBeforeLoss = next.state.sharing;
      }
      if (next.state.audioBlocked && !audioBlocked) announce(joinTick ? `${joined} ${t('voice.audioBlocked')}` : t('voice.audioBlocked'));
      audioBlocked = next.state.audioBlocked;
      // The room ended on its own (the session already left; a person's own leave detaches first, so it never gets
      // here). WB client I3: say why, in the stage's notice and, off the stage, where it outlives the dock.
      if (next.state.phase === 'ended') {
        const key = END_NOTICE[next.state.error ?? ''] ?? 'voice.disconnected';
        const recoverEnded = next.state.error === 'E_CALL_ENDED' && live.current.recoverEnded?.(id) === true;
        if (next.state.error === 'E_CALL_ENDED') {
          setRecentlyEnded({ channelId: id, camera: cameraBeforeLoss, sharing: sharingBeforeLoss, at: Date.now() });
        }
        if (next.state.error === 'E_DISCONNECTED' || recoverEnded) {
          armGoneFrom(id, next.state.error === 'E_DISCONNECTED');
          recoveryRef.current = id;
          recoveryMark.current = { since: awayAtAttach.current?.channelId === id ? awayAtAttach.current.since : null };
          recoveryUntil.current = null;
          setRecoveryChannelId(id);
          recoveryCameraRef.current = cameraBeforeLoss;
          setRecoveryCamera(cameraBeforeLoss);
          setRecoverySharing(sharingBeforeLoss);
        }
        detach();
        // AW5: the sweep's cut says `afk.moved` in place of the removal (sayRemoved).
        if (next.state.error !== 'E_DISCONNECTED' && !recoverEnded) {
          const say = (): void => {
            setNotice({ channelId: id, key });
            if (!live.current.stageShown(id)) setLasting(t(key));
          };
          if (next.state.error === 'E_REMOVED') sayRemoved(id, say); else say();
        }
      }
    });
    announce(audioBlocked ? `${joined} ${t('voice.audioBlocked')}` : joined);
    if (recoveryRef.current === id && recoveryCameraRef.current) {
      void next.setCamera(true).catch(() => undefined);
    }
  }, [announce, detach, sayRemoved, client, communityOf, reasonCommunity, awaySince, armGoneFrom, phaseAlive]);

  /** `made`: the call context a queued join's own press made (it waited for the cancelled start to end). AW5 (R-AW1): an
   *  AFK channel is never joined; the person's press on one is `goAway` (joinOnly), and nothing else asks for one. */
  const join = useCallback((id: string, made?: AudioContext | null) => {
    dropHold();
    if (isAfk(id)) { closeContext(made); return; }
    if (recoveryRef.current !== null && recoveryRef.current !== id) {
      recoveryRef.current = null;
      setRecoveryChannelId(null);
      recoveryCameraRef.current = false;
      setRecoveryCamera(false);
      setRecoverySharing(false);
    }
    setRecentlyEnded(null);
    if (channelRef.current === id && (starting.current || sessionRef.current !== null)) {
      closeContext(made);
      return;
    }
    // web3.3 fix round 2 (Minor 1): the call’s AudioContext is made here, synchronously in the press, before any await
    // (the leave in flight, the module load, a switch’s leave); the session it is handed to closes it on every way out,
    // and every path below that never hands it over (unsupported, cancelled, superseded, a failed load) closes it here.
    const ctx = made !== undefined ? made : (deps.createCallContext?.() ?? null);
    if (starting.current) {
      // WB client I1: another channel (a ring's answer) while a start runs: that start is cancelled and this one follows.
      closeContext(queuedJoin.current?.ctx);
      queuedJoin.current = { id, ctx };
      startAbort.current?.abort();
      return;
    }
    let handed = false;
    starting.current = true;
    const from = document.activeElement;
    const abort = new AbortController();
    startAbort.current = abort;
    const run = (async () => {
      try {
        // WB client m1: a leave still in flight (the worker refuses a join while it runs) is waited for first.
        await leaving.current?.catch(() => undefined);
        // Fix round 1 (I3): an away mark in flight lands first, so this join's admission follows and clears it.
        await awayPut.current;
        const mod = await deps.load();
        module.current = mod;
        if (!mod.isVoiceSupported().ok) {
          opener.current = from instanceof HTMLElement && from !== document.body ? from : null;
          setUnsupported(true);
          return;
        }
        if (abort.signal.aborted) return;
        // One call at a time: the current one is left first, and the next starts only once that leave is done.
        const previous = sessionRef.current;
        if (previous !== null) {
          detach();
          await previous.leave('switch');
        }
        if (abort.signal.aborted) return;
        setNotice(null);
        setBlockedMidCall(null);
        // WB a11y I3: what was said for the last call is not said for this one; a second identical text is heard again.
        setLasting('');
        setAnnouncement('');
        // Flow 06 § Focus rules: the join moves focus to the stage's first control as soon as it shows (WB a11y I1).
        stageFocus.current = true;
        channelRef.current = id;
        setChannelId(id);
        setJoining(true);
        const settings = client.get('settings') ?? {};
        // The start's arguments are built before the context counts as handed: a throw while building them closes it below.
        const callDeps = mod.browserDeps(client);
        const target = { channelId: id, vdec: mod.localDecodeList() };
        outputInForce.current = settings[SETTINGS.voiceOutput] ?? '';
        outputPlaying.current = outputInForce.current;
        const prefs = voicePrefs(settings);
        prefsInForce.current = prefs;
        const opts: Parameters<VoiceModule['startPageCall']>[2] = {
          instanceId: client.get('account')?.instance?.id ?? '',
          input: settings[SETTINGS.voiceInput] ?? '',
          output: outputInForce.current,
          mode: settings[SETTINGS.voiceMode] === 'ptt' ? 'ptt' : 'vad',
          noise: settings[SETTINGS.voiceNoise] === 'off' ? 'off' : settings[SETTINGS.voiceNoise] === 'automatic' ? 'automatic' : settings[SETTINGS.voiceNoise] === 'lightweight' ? 'lightweight' : 'on',
          volumes: mod.volumesFromSettings(settings),
          streamVolumes: mod.streamVolumesFromSettings(settings),
          inputVolume: prefs.inputVolume,
          outputVolume: prefs.outputVolume,
          sensitivity: prefs.sensitivity,
          pttReleaseMs: prefs.pttReleaseMs,
          processing: prefs.processing,
          signal: abort.signal,
          // Product walk M5: the mute or deafen this browser kept (never a live microphone after a reload of a muted call).
          self: selfNow(),
          // web3.8 MUTE: a press while the start runs is pushed at once (watchSelf), and read again before the publish,
          // after it and at connected.
          selfNow,
          watchSelf,
          ...(ctx !== null ? { audioContext: ctx } : {}),
        };
        handed = true;
        const next = await mod.startPageCall(callDeps, target, opts);
        // The shell unmounted while the start was pending: nobody drives this call, so it ends at once.
        if (!mounted.current) { void next.leave('user'); return; }
        attach(next, id);
        setRecentlyEnded(null);
        if (recoveryRef.current === id) {
          recoveryRef.current = null;
          setRecoveryChannelId(null);
          recoveryCameraRef.current = false;
          setRecoveryCamera(false);
        }
      } catch (e) {
        if (sessionRef.current === null) { channelRef.current = null; setChannelId(null); }
        // A start the person cancelled says nothing (WB client I1). The worker's side of it (its join cancelled, its
        // self-Remove sent) is awaited by the next join through a leave of its own, which answers once that is done.
        if (abort.signal.aborted) leaving.current = client.call({ m: 'callLeave', reason: 'user' }).catch(() => undefined);
        const code = errorOf(e).code;
        // A disconnected room may come back after a network or media restart. A refusal that
        // cannot improve by waiting ends the automatic attempt and leaves its reason visible.
        if (recoveryRef.current === id && ['E_FORBIDDEN', 'E_REMOVED', 'E_CALL_FULL', 'E_CALL_IN_OTHER_TAB',
          'E_CREDENTIAL', 'E_DEVICE_UNLISTED', 'E_DEVICE_REVOKED', 'E_TIER_MISMATCH', 'E_BINDING'].includes(code)) {
          recoveryRef.current = null;
          setRecoveryChannelId(null);
          recoveryCameraRef.current = false;
          setRecoveryCamera(false);
          setRecoverySharing(false);
        }
        const key = abort.signal.aborted ? null : joinFailureKey(code);
        if (key !== null) setNotice({ channelId: id, key });
      } finally {
        if (!handed) closeContext(ctx);
        if (startAbort.current === abort) startAbort.current = null;
        starting.current = false;
        setJoining(false);
        const next = queuedJoin.current;
        queuedJoin.current = null;
        if (next !== null && mounted.current) joinRef.current(next.id, next.ctx);
        else closeContext(next?.ctx);
      }
    })();
    startDone.current = run;
    void run;
  }, [deps, client, attach, detach, selfNow, watchSelf, dropHold, isAfk]);
  const joinRef = useRef(join);
  joinRef.current = join;
  /**
   * The person's own leave. `quiet` (choosing AFK, fix round 1 M6): focus stays on the control they used, so the left
   * call's row does not take it.
   */
  const leaveCall = useCallback((after?: CallNotice, quiet = false) => {
    recoveryRef.current = null;
    setRecoveryChannelId(null);
    recoveryCameraRef.current = false;
    setRecoveryCamera(false);
    setRecoverySharing(false);
    setRecentlyEnded(null);
    const current = sessionRef.current;
    const id = channelRef.current;
    if (current === null && starting.current) {
      // WB client I1: a leave while the start runs cancels it (the microphone prompt and the rejoin wait included); the
      // shell returns to where it was, and nothing is said of a failure.
      closeContext(queuedJoin.current?.ctx);
      queuedJoin.current = null;
      startAbort.current?.abort();
      channelRef.current = null;
      setChannelId(null);
      return;
    }
    if (current === null || id === null) return;
    detach();
    if (after !== undefined) setNotice(after);
    const done = current.leave('user').finally(() => {
      if (leaving.current === done) leaving.current = null;
      setLasting(t('voice.announce.left', { where: live.current.whereOf(id) }));
      if (!quiet) live.current.onLeft(id);
    });
    leaving.current = done;
  }, [detach]);
  const leave = useCallback((after?: CallNotice) => { leaveCall(after); }, [leaveCall]);
  const leaveRef = useRef<(after?: CallNotice) => void>(() => undefined);
  /**
   * AW5 (R-AW3): choosing AFK. The person's own leave runs first, always: it ends a call in progress (or a start) and stops
   * the microphone, and it cancels a pending recovery (fix round 1, I2). Once that leave is done the instance is told
   * (PUT …/afk/away), and it cuts any other device of this user still in a room. A press while already away there does
   * nothing; nothing ever joins the AFK channel (R-AW1).
   * Fix round 1 (I3, N2): every choice of the person (a join, a restore, AFK) takes the next `intent`; an away mark whose
   * intent was overtaken while it waited for the leave is dropped, never sent, and a join waits for a mark in flight, so
   * its admission always follows (and clears) it.
   */
  const goAway = useCallback(async (id: string): Promise<void> => {
    const cid = communityOf(id);
    if (cid === null || awaySince(cid) !== null) return;
    const mine = ++intent.current;
    leaveCall(undefined, true);
    await startDone.current;
    await leaving.current?.catch(() => undefined);
    await awayPut.current;
    // Fix round 2 (B-n1): a mark that landed meanwhile (its op 55) made this press redundant.
    if (mine !== intent.current || !mounted.current || awaySince(cid) !== null) return;
    const put = client.call({ m: 'afkAway', communityId: cid });
    const settled = put.then(() => undefined, () => undefined);
    awayPut.current = settled;
    try {
      await put;
    } catch {
      // Fix round 1 (M3): the leave is done and the person is not away; the line says what failed.
      if (!mounted.current) return;
      setNotice({ channelId: id, key: 'afk.failed' });
      if (!live.current.stageShown(id)) setLasting(t('afk.failed'));
    } finally {
      if (awayPut.current === settled) awayPut.current = null;
    }
  }, [client, communityOf, awaySince, leaveCall]);
  /** The shell’s join: a channel only (a second argument from a caller never stands for a call context). A press on an
   *  AFK channel chooses AFK (goAway). */
  const joinOnly = useCallback((id: string) => {
    if (isAfk(id)) { void goAway(id); return; }
    intent.current += 1;
    join(id);
  }, [join, isAfk, goAway]);
  /**
   * R-AW7, a second guard beside the pending reason: whether the person is away in the call's own community after the call
   * began. Fix round 1 (M1): no client clock. `mark` is the away row that community held at attach (server seconds, null
   * for none): a row that is there now and was not, or whose since differs, is after the call. `mark` null (a restore,
   * whose call began before this page) counts any away row.
   */
  const awayAfter = useCallback((id: string, mark: { since: number | null } | null): boolean => {
    const cid = communityOf(id);
    if (cid === null) return false;
    const since = awaySince(cid);
    return since !== null && (mark === null || mark.since !== since);
  }, [communityOf, awaySince]);
  /**
   * Fix round 1 (I1), round 3: what a restore or a recovery may do with `id`, from a fresh read (the worker's `afkRead`),
   * never from a slice left from before a sleep or a reload. It waits (at most AFK_READ_WAIT_MS) for the servers, their
   * channel lists and the DMs, then reads the call's own community (a DM call: any listed one, the reason being
   * device-scoped; with none, nothing can have cut it for AFK):
   * - 'clear' joins; 'unknown' (the wait ran out, the read failed) skips this attempt (B-I1);
   * - 'afk' (the AFK channel), 'moved' and 'elsewhere' (the pending reason, 1 and 2), and 'away' (an away row since the
   *   call in its own community) join nothing;
   * - 'gone' (B2-M1): no loaded list or DM holds `id`, or its community is no longer listed: the recovery is over;
   * - 'leftAway' (late rejoin, Jonas 2026-10-21): a recovery after a lost connection (`goneFrom`) of a community call whose
   *   device was gone longer than that community's AFK timeout (the fresh read's `timeoutSeconds`) joins nothing. A pending
   *   reason wins over it. With no timeout (the community has no AFK channel, so its sweep is off) or no read, and for a
   *   DM call, the recovery goes on as before.
   */
  const afkFresh = useCallback(async (id: string, mark: { since: number | null } | null, goneFrom: Stamp | null = null)
    : Promise<'clear' | 'afk' | 'moved' | 'elsewhere' | 'away' | 'leftAway' | 'gone' | 'unknown'> => {
    const deadline = Date.now() + AFK_READ_WAIT_MS;
    const isDm = (): boolean => (client.get('dms') ?? []).some((d) => d.id === id);
    const listsLoaded = (): boolean => {
      const communities = client.get('communities');
      return communities !== undefined && client.get('dms') !== undefined
        && communities.every((c) => client.get(`channels:${c.id}`) !== undefined);
    };
    while (!isDm() && communityOf(id) === null) {
      if (listsLoaded()) return 'gone';
      if (Date.now() >= deadline || !mounted.current) return 'unknown';
      await new Promise<void>((r) => { setTimeout(r, 100); });
    }
    const own = communityOf(id);
    const cid = reasonCommunity(id);
    if (cid === null) return 'clear';
    const read = await readAfk(cid);
    if (read === 'gone') return own !== null ? 'gone' : 'unknown';
    if (read === 'failed') return 'unknown';
    if (own !== null && isAfk(id)) return 'afk';
    const cut = cutReason(id);
    if (cut !== null) return cut.reason === 1 ? 'moved' : 'elsewhere';
    if (own !== null && goneFrom !== null) {
      const afk = client.get(`afk:${own}`);
      const timeout = afk != null && afk.channelId !== null && afk.timeoutSeconds > 0 ? afk.timeoutSeconds * 1_000 : null;
      if (timeout !== null && elapsedSince(goneFrom) > timeout) return 'leftAway';
    }
    return awayAfter(id, mark) ? 'away' : 'clear';
  }, [client, communityOf, reasonCommunity, readAfk, isAfk, cutReason, awayAfter]);
  /** Fix round 1 (I1), round 3: a restore or a recovery that will not join: the recovery ends and its notice says why:
   *  the pending reason's line ('Moved to AFK', or the calm 'You went AFK on another device.'), else the lost call; the
   *  AFK channel itself says nothing. A device gone longer than the AFK timeout says 'You left the call while away.'; the
   *  stage's join button joins again as any join. */
  const endRecovery = useCallback((id: string, verdict: 'afk' | 'moved' | 'elsewhere' | 'away' | 'leftAway' | 'gone') => {
    recoveryRef.current = null;
    recoveryUntil.current = null;
    recoveryGoneFrom.current = null;
    setRecoveryChannelId(null);
    recoveryCameraRef.current = false;
    setRecoveryCamera(false);
    setRecoverySharing(false);
    if (verdict === 'afk' || !mounted.current) return;
    if (verdict === 'elsewhere') {
      setNotice({ channelId: id, key: 'afk.elsewhere' });
      if (!live.current.stageShown(id)) setLasting(t('afk.elsewhere'));
      return;
    }
    if (verdict === 'leftAway') {
      setNotice({ channelId: id, key: 'voice.leftWhileAway' });
      if (!live.current.stageShown(id)) setLasting(t('voice.leftWhileAway'));
      return;
    }
    const cid = reasonCommunity(id);
    const text = verdict === 'moved' && cid !== null ? t('afk.moved', { channel: afkNameFor(id) }) : t('voice.disconnected');
    setNotice(verdict === 'moved' && cid !== null ? { channelId: id, key: 'afk.moved', text } : { channelId: id, key: 'voice.disconnected' });
    if (!live.current.stageShown(id)) setLasting(text);
  }, [reasonCommunity, afkNameFor]);
  // Fix round 2 (B-n2): the recovery effect reads these through refs, so a slice change does not re-arm its backoff.
  const afkFreshRef = useRef(afkFresh);
  afkFreshRef.current = afkFresh;
  const endRecoveryRef = useRef(endRecovery);
  endRecoveryRef.current = endRecovery;
  const restore = useCallback((id: string, camera: boolean, sharing: boolean) => {
    // AW5 (R-AW7), fix rounds 1-3: the restore is a recovery that may take until RESTORE_WAIT_MS (the upgrade's rejoin
    // window), with the rejoining banner and its Leave from the start (B2-M4). It joins only on a fresh clear read; the AFK
    // channel, a pending reason, an away row or a gone channel join nothing, and past the window it ends as lost.
    // Final review N2: a channel already known to be AFK (a web3.9 or web3.10 call saved before the upgrade) arms nothing,
    // so its rejoining banner never flashes; one whose slices are not read yet still ends as 'afk' on the first retry.
    if (isAfk(id)) return;
    intent.current += 1;
    recoveryRef.current = id;
    recoveryMark.current = null;
    recoveryGoneFrom.current = null;
    recoveryUntil.current = Date.now() + RESTORE_WAIT_MS;
    recoveryCameraRef.current = camera;
    setRecoveryChannelId(id);
    setRecoveryCamera(camera);
    setRecoverySharing(sharing);
  }, [isAfk]);

  /** Product walk M5: the own mute and deafen are kept per browser (a device-local setting), for the next call. */
  const keepSelf = useCallback((s: PageCall) => {
    const value = s.state.deafened ? 'deafened' : s.state.muted ? 'muted' : null;
    // web3.8 MUTE: kept in memory as well (the same ref as writeSelf), and still kept when the write is refused, so a
    // rejoin after a disconnect starts in the state the session was in.
    const mine: { seq: number; value: string | null; held?: true } = { seq: ++writeSeq.current, value };
    written.current = mine;
    pushSelf();
    client.call({ m: 'setSetting', key: SETTINGS.voiceSelf, value }).then(
      () => { if (written.current === mine) { written.current = null; carry(client, null); } },
      () => { if (written.current === mine) { mine.held = true; carry(client, value); } },
    );
  }, [client, pushSelf]);
  const toggleMute = useCallback(() => {
    const s = sessionRef.current;
    if (s === null) return;
    const on = !s.state.muted;
    s.setMuted(on);
    keepSelf(s);
    announce(t(on ? 'voice.announce.muted' : 'voice.announce.unmuted'));
  }, [announce, keepSelf]);
  const toggleDeafen = useCallback(() => {
    const s = sessionRef.current;
    if (s === null) return;
    const on = !s.state.deafened;
    s.setDeafened(on);
    keepSelf(s);
    announce(t(on ? 'voice.announce.deafened' : 'voice.announce.undeafened'));
  }, [announce, keepSelf]);
  const writeSelf = useCallback((pick: (current: string | undefined) => 'muted' | 'deafened' | null) => {
    const current = written.current !== null ? written.current.value ?? undefined : morePrivate(((client.get('settings') ?? {})[SETTINGS.voiceSelf] as SelfValue | undefined) ?? null, readCarried(client)) ?? undefined;
    const value = pick(current);
    const mine = { seq: ++writeSeq.current, value };
    written.current = mine;
    pushSelf();
    const done = () => { if (written.current === mine) written.current = null; };
    const answered = () => { if (written.current === mine) carry(client, null); done(); };
    const key = value === 'deafened' ? 'voice.announce.deafened' : current === 'deafened' && value === 'muted' ? 'voice.announce.undeafened'
      : value === 'muted' ? 'voice.announce.muted' : 'voice.announce.unmuted';
    client.call({ m: 'setSetting', key: SETTINGS.voiceSelf, value }).then(() => { answered(); live.current.announce?.(t(key)); }, done);
  }, [client, pushSelf]);
  const selfMute = useCallback(() => {
    if (sessionRef.current !== null) { toggleMute(); return; }
    // null -> muted -> null; from deafened, unmuting also undeafens (setMuted(false) in the session).
    writeSelf(current => (current === 'muted' || current === 'deafened' ? null : 'muted'));
  }, [toggleMute, writeSelf]);
  const selfDeafen = useCallback(() => {
    if (sessionRef.current !== null) { toggleDeafen(); return; }
    // null or muted -> deafened; from deafened back to muted, never to an open microphone.
    writeSelf(current => (current === 'deafened' ? 'muted' : 'deafened'));
  }, [toggleDeafen, writeSelf]);
  const shareRefused = useCallback((e: unknown) => {
    const id = channelRef.current;
    const code = errorOf(e).code;
    if (id !== null && code === 'E_CALL_SHARERS_FULL') setNotice({ channelId: id, key: 'voice.sharersFull' });
  }, []);
  const toggleCamera = useCallback(() => {
    const s = sessionRef.current;
    if (s !== null) s.setCamera(!s.state.camera).catch(shareRefused);
  }, [shareRefused]);
  const [shareOptions, setShareOptions] = useState<ShareOptions>(DEFAULT_SHARE_OPTIONS);
  const shareOptionsRef = useRef(shareOptions);
  shareOptionsRef.current = shareOptions;
  const toggleShare = useCallback(() => {
    const s = sessionRef.current;
    if (s !== null) s.setSharing(!s.state.sharing, shareOptionsRef.current).catch(shareRefused);
  }, [shareRefused]);
  // Task 8c fix round 2 (Ruling AV): the prototype's toasts once the share is live, and once Apply took.
  const startShare = useCallback((options: ShareOptions) => {
    setShareOptions(options);
    const s = sessionRef.current;
    if (s !== null && !s.state.sharing) s.setSharing(true, options).then(() => { if (s.state.sharing) showToast(t('callStage.share.live')); }, shareRefused);
  }, [shareRefused]);
  const applyShare = useCallback((options: ShareOptions) => {
    setShareOptions(options);
    const s = sessionRef.current;
    s?.setShareOptions(options).then(() => {
      if (s.state.sharing) showToast(t('callStage.share.applied', { res: options.resolution === 'source' ? t('callStage.share.native') : options.resolution, fps: options.fps }));
    }, shareRefused);
  }, [shareRefused]);
  // Fix round 1 (Jonas): no sound-source pick for a share any more (a silent share keeps the default source; the session's
  // setShareSource stays for wave C's capture work).
  const setVolume = useCallback((userId: string, volume: number) => {
    sessionRef.current?.setVolume(userId, volume);
    client.call({ m: 'setSetting', key: SETTINGS.voiceVolume(userId), value: String(volume) }).catch(() => undefined);
  }, [client]);
  const setStreamVolume = useCallback((userId: string, volume: number) => {
    sessionRef.current?.setVolume(userId, volume, 'stream');
    client.call({ m: 'setSetting', key: SETTINGS.voiceStreamVolume(userId), value: String(volume) }).catch(() => undefined);
  }, [client]);
  const startAudio = useCallback(() => {
    // The press is the user activation the browser asks for; a refusal leaves the line on offer (the session never throws).
    sessionRef.current?.startAudio().catch(() => undefined);
  }, []);

  const closeUnsupported = useCallback(() => {
    setUnsupported(false);
    const target = opener.current;
    opener.current = null;
    queueMicrotask(() => { if (target?.isConnected === true) target.focus(); });
  }, []);

  // The worker's call slice drives the session (RV-3 and the block rule above), on every slice change and again when a
  // session attaches, reading the newest slice the client holds rather than one a render captured (security-scanner
  // leads, fail-open state drift; defensive: React already renders the newest slice, so no test tells the two reads
  // apart, review m6). The call id of a call is its channel's id (protocol/09 § Voice), so the channel decides
  // whether a slice is this session's. The worker publishes phase 'in' before callJoin answers (L-TS-56 step 1), so an
  // attached session must always match a slice that says 'in' or 'paused' for its channel. Fail closed:
  // - 'paused' pauses it (once); only 'in' for this channel unpauses it, and no cleanup ever does;
  // - 'blocked' or 'removed' leaves with that reason;
  // - anything else (idle, failed, joining, leaving, or another channel: the worker is not in this call) tears it down;
  // - a setPaused or leave that throws or rejects tears the session down; nothing of it escapes.
  const teardown = useCallback((s: PageCall, reason: 'blocked' | 'removed') => {
    const id = channelRef.current;
    if (sessionRef.current === s) {
      // Focus inside the dock would fall to the page with it: it goes to the voice row the call came from (review I6).
      const inDock = document.activeElement?.closest('.dw-voice-dock') != null;
      detach();
      if (inDock && id !== null) live.current.onLeft(id);
    }
    try {
      const done = s.leave(reason).catch(() => undefined).finally(() => { if (leaving.current === done) leaving.current = null; });
      leaving.current = done;
    } catch {
      // The session's leave closes its key relay first; nothing more can be done from here.
    }
  }, [detach]);
  const safely = useCallback((s: PageCall, run: () => Promise<void>) => {
    try {
      run().catch(() => teardown(s, 'removed'));
    } catch {
      teardown(s, 'removed');
    }
  }, [teardown]);
  const slice = o.call;
  /** Fix round 3 (m5): the channel whose call this device was just removed from; the worker's re-read may then say the
   *  channel is gone (E_NOT_FOUND), and off the stage the region that outlives the dock says so too. */
  const removedFrom = useRef<string | null>(null);
  const goneFrom = slice?.phase === 'removed' && slice.error?.code === 'E_NOT_FOUND' ? slice.channelId : null;
  useEffect(() => {
    if (goneFrom === null || goneFrom !== removedFrom.current) return;
    removedFrom.current = null;
    if (!live.current.stageShown(goneFrom)) setLasting(t('voice.removedNotMember'));
  }, [goneFrom]);
  // f10app m2: the rejoin's wait for this device's earlier Remove (up to 30 s, the dock says "connecting") is said once in
  // the dock's status region, so a screen reader user knows why nothing happens yet.
  const rejoining = slice?.phase === 'joining' && slice.rejoining && slice.channelId === channelId && channelId !== null;
  useEffect(() => {
    if (rejoining && channelId !== null) announce(t('voice.announce.rejoining', { where: live.current.whereOf(channelId) }));
  }, [rejoining, channelId, announce]);
  useEffect(() => {
    const s = sessionRef.current;
    const id = channelRef.current;
    if (s === null || id === null) return;
    const now = client.get('call') ?? slice;
    const phase = now?.channelId === id ? now.phase : null;
    if (phase === 'paused') {
      if (!paused.current) {
        paused.current = true;
        setPauseFocusPending(true);
        safely(s, () => s.setPaused(true));
      }
      // WB security I1: a pause that is not a changed identity is the hold after a refused rekey. The page's keys are an
      // epoch a removed leaf may still hold, so they go at once (once per hold); the next epoch's key ends it.
      if (now?.error?.code !== 'E_UMK_CHANGED' && !keysDropped.current) {
        keysDropped.current = true;
        let upTo: bigint | undefined;
        try { upTo = now?.epoch == null ? undefined : BigInt(now.epoch); } catch { upTo = undefined; }
        safely(s, () => { s.dropKeys(upTo); return Promise.resolve(); });
      }
    } else if (phase === 'in') {
      keysDropped.current = false;
      if (paused.current) {
        paused.current = false;
        setPauseFocusPending(false);
        safely(s, () => s.setPaused(false));
        announce(t('voice.resumed'));
      }
    } else if (phase === 'blocked' || phase === 'removed') {
      const recoverEnded = phase === 'removed' && now?.error?.code === 'E_CALL_ENDED' && live.current.recoverEnded?.(id) === true;
      if (phase === 'removed' && now?.error?.code === 'E_CALL_ENDED') {
        setRecentlyEnded({ channelId: id, camera: s.state.camera, sharing: s.state.sharing, at: Date.now() });
      }
      if (phase === 'removed' && (now?.error?.code === 'E_DISCONNECTED' || recoverEnded)) {
        armGoneFrom(id, now?.error?.code === 'E_DISCONNECTED');
        recoveryRef.current = id;
        recoveryMark.current = { since: awayAtAttach.current?.channelId === id ? awayAtAttach.current.since : null };
        recoveryUntil.current = null;
        setRecoveryChannelId(id);
        recoveryCameraRef.current = s.state.camera;
        setRecoveryCamera(s.state.camera);
        setRecoverySharing(s.state.sharing);
        setPauseFocusPending(false);
        teardown(s, phase);
        return;
      }
      // AW5: the sweep's cut says `afk.moved` in place of the removal (sayRemoved).
      if (phase === 'blocked') {
        setBlockedMidCall(id);
        if (!live.current.stageShown(id)) setLasting(t('voice.blocked'));
      } else {
        // Off the stage, nothing else would say why the call ended (review I6).
        // The bookkeeping is never held; only the visible notice and the spoken line wait (the hold changes timing, not words).
        removedFrom.current = id;
        sayRemoved(id, () => {
          setNotice({ channelId: id, key: 'voice.removed' });
          // The worker's re-read may have said the channel is gone for this device meanwhile: that line is the right one.
          const now = client.get('call');
          const gone = now?.channelId === id && now.phase === 'removed' && now.error?.code === 'E_NOT_FOUND';
          if (!live.current.stageShown(id)) setLasting(t(gone ? 'voice.removedNotMember' : 'voice.removed'));
        });
      }
      setPauseFocusPending(false);
      teardown(s, phase);
    } else {
      teardown(s, 'removed');
    }
  }, [slice, session, client, announce, safely, teardown, sayRemoved, armGoneFrom]);
  // Late rejoin (Jonas 2026-10-21): while a call is attached the page ticks once a second, so a suspend (no ticks) shows as
  // a gap; the last tick is kept after the call is lost, for goneSince.
  useEffect(() => {
    if (session === null) return;
    alive.current = { last: stampNow(), gap: null };
    const timer = setInterval(tickAlive, 1_000);
    return () => { clearInterval(timer); };
  }, [session, tickAlive]);
  useEffect(() => {
    if (recoveryChannelId === null) return;
    let delay = 1_000;
    let timer: ReturnType<typeof setTimeout>;
    let cancelled = false;
    const retry = async (): Promise<void> => {
      if (cancelled || recoveryRef.current !== recoveryChannelId) return;
      const phase = client.get('call')?.phase;
      const removedCode = client.get('call')?.error?.code;
      if (phase === 'blocked' || (phase === 'removed' && removedCode !== 'E_DISCONNECTED'
        && !(removedCode === 'E_CALL_ENDED' && live.current.recoverEnded?.(recoveryChannelId)))) {
        // Fix round 3 (B2-n2): an upgrade's recovery that outlived its window ends as a lost call, not silently.
        if (phase === 'removed' && removedCode === 'E_CALL_ENDED') { endRecoveryRef.current(recoveryChannelId, 'gone'); return; }
        recoveryRef.current = null;
        setRecoveryChannelId(null);
        return;
      }
      // Fix round 3 (B2-M4): a restore's recovery past the upgrade's rejoin window ends as a lost call.
      const until = recoveryUntil.current;
      if (until !== null && Date.now() > until) { endRecoveryRef.current(recoveryChannelId, 'gone'); return; }
      if (navigator.onLine && sessionRef.current === null && !starting.current) {
        // Fix round 1 (I1), AW5 (R-AW7): only on a fresh read of its community's AFK state, and only while the person is
        // not away there since the call (nor cut there by the sweep); a read that does not come joins nothing.
        // Fix round 2 (B-I1): a read that fails or does not come in time skips this attempt only; the backoff and the
        // join's permanent refusals end a recovery, as before. Only the AFK channel or an away answer ends it here.
        // Fix round 3: a pending reason (moved, elsewhere) or a gone channel (B2-M1) ends it too.
        // Late rejoin (Jonas 2026-10-21): a device gone longer than its community's AFK timeout ends it too ('leftAway').
        const verdict = await afkFreshRef.current(recoveryChannelId, recoveryMark.current, recoveryGoneFrom.current);
        if (cancelled || recoveryRef.current !== recoveryChannelId) return;
        if (verdict !== 'clear' && verdict !== 'unknown') { endRecoveryRef.current(recoveryChannelId, verdict); return; }
        if (verdict === 'clear' && sessionRef.current === null && !starting.current) joinRef.current(recoveryChannelId);
      }
      // A restore asks every second through its window (B-M2); a lost connection backs off.
      delay = recoveryUntil.current !== null ? 1_000 : Math.min(delay * 2, 30_000);
      timer = setTimeout(() => { void retry(); }, delay);
    };
    timer = setTimeout(() => { void retry(); }, delay);
    return () => { cancelled = true; clearTimeout(timer); };
  }, [recoveryChannelId, client]);

  // Product walk I6: the outgoing ring of a DM call this device opened. It lasts 30 s from the call's start, whoever was
  // rung; once every ring ended with nobody else ever in the call, the caller leaves by itself after a short grace, and the
  // strip keeps the ring's last line. A call someone answered never ends by itself.
  const outgoing = slice !== undefined && channelId !== null && slice.channelId === channelId
    && (slice.phase === 'in' || slice.phase === 'paused') ? slice.outgoing : null;
  const hasOutgoing = outgoing !== null;
  const [ringExpired, setRingExpired] = useState(false);
  useEffect(() => {
    setRingExpired(false);
    if (!hasOutgoing) return;
    const timer = setTimeout(() => setRingExpired(true), RING_MS);
    return () => clearTimeout(timer);
  }, [hasOutgoing, channelId]);
  const othersNow = (state?.tiles.length ?? 0) > 0;
  if (othersNow) answered.current = true;
  const ended = outgoing !== null && !answered.current && unanswered(outgoing, othersNow, ringExpired);
  const endText = ended ? outgoingText(outgoing, false, ringExpired) : null;
  useEffect(() => {
    if (endText === null || channelId === null) return;
    const id = channelId;
    const timer = setTimeout(() => {
      if (!answered.current) leaveRef.current({ channelId: id, key: 'call.outgoing.noAnswer', text: endText });
    }, UNANSWERED_LEAVE_MS);
    return () => clearTimeout(timer);
  }, [endText, channelId]);

  // Control/⌘+Shift+M and Control/⌘+Shift+D while in voice, and on the desktop shell outside a call too (decision 1; the user panel advertises them); no single-key shortcut, and none during an IME
  // composition. Either modifier works on every platform, as the Control/⌘+K entry does.
  // Fail closed: a mute or deafen pressed while the join was starting is in `voice.self` but not in the session it started
  // with (it read the setting before). When the session attaches, the more private of the two wins; never the other way.
  // A deafen is applied as a mute first, then the deafen, so an undeafen keeps the microphone muted (R25: deafened goes back
  // to muted, never to an open microphone). The session's own setMuted and setDeafened tell the others (voiceSelf).
  useEffect(() => {
    if (session === null) return;
    const kept = selfNow();
    if (kept === null) return;
    if (!session.state.muted) session.setMuted(true);
    if (kept === 'deafened' && !session.state.deafened) session.setDeafened(true);
  }, [session, selfNow]);
  const selfShortcuts = o.selfShortcuts === true;
  useEffect(() => {
    if (session === null && !selfShortcuts) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.isComposing || e.keyCode === 229) return;
      if (!(e.ctrlKey || e.metaKey) || !e.shiftKey || e.altKey) return;
      // WB a11y m1: the letter the key types (AZERTY's M is code Semicolon; Dvorak's physical M types another letter), as
      // Control/⌘+K matches e.key; the physical key only when the key types no letter at all.
      const letter = /^[a-z]$/i.test(e.key) ? e.key.toLowerCase() : e.code === 'KeyM' ? KEY_M : e.code === 'KeyD' ? KEY_D : '';
      if (letter === KEY_M) { e.preventDefault(); selfMute(); }
      else if (letter === KEY_D) { e.preventDefault(); selfDeafen(); }
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [session, selfShortcuts, selfMute, selfDeafen]);
  // web3.8 MUTE: an in-call toggle whose write was refused is what the next join starts in, so the panel shows it.
  const kept = written.current?.held === true ? (written.current.value ?? undefined)
    : morePrivate((o.settings[SETTINGS.voiceSelf] as SelfValue | undefined) ?? null, readCarried(client)) ?? undefined;
  const selfMuted = state !== null && session !== null ? state.muted : kept === 'muted' || kept === 'deafened';
  const selfDeafened = state !== null && session !== null ? state.deafened : kept === 'deafened';

  // The input mode follows Settings → Voice during a call (review I2): the session switches first (push to talk closes the
  // microphone at once), the dock says which mode is in force, and the key binds only to the mode the session applied.
  const wanted = o.settings[SETTINGS.voiceMode] === 'ptt' ? 'ptt' : 'vad';
  useEffect(() => {
    if (session === null || session.state.mode === wanted) return;
    session.setMode(wanted);
    announce(t(wanted === 'ptt' ? 'settings.voice.ptt' : 'settings.voice.vad'));
  }, [session, wanted, announce]);
  // Product walk M1: noise suppression follows Settings → Voice during the call too.
  const wantedNoise = o.settings[SETTINGS.voiceNoise] === 'off' ? 'off' : o.settings[SETTINGS.voiceNoise] === 'automatic' ? 'automatic' : o.settings[SETTINGS.voiceNoise] === 'lightweight' ? 'lightweight' : 'on';
  useEffect(() => {
    if (session === null || session.state.noise === wantedNoise) return;
    session.setNoise(wantedNoise).catch(() => undefined);
  }, [session, wantedNoise]);
  // web3.4 V1: the output device follows Settings → Voice during the call too. A device the browser refuses is said
  // once; the sound goes on where it played before. Fix round 1: a refusal of a device the person has since moved away
  // from is not said, and a refusal puts the device that still plays back in force, so choosing the refused one again
  // tries it again.
  const wantedOutput = o.settings[SETTINGS.voiceOutput] ?? '';
  useEffect(() => {
    if (session === null || outputInForce.current === wantedOutput) return;
    outputInForce.current = wantedOutput;
    const id = wantedOutput;
    session.setOutput(id).then(() => {
      if (sessionRef.current === session) outputPlaying.current = id;
    }, () => {
      if (sessionRef.current !== session || (live.current.settings[SETTINGS.voiceOutput] ?? '') !== id) return;
      outputInForce.current = outputPlaying.current;
      announce(t('voice.outputFailed'));
    });
  }, [session, wantedOutput, announce]);
  // Signal task 10b: input and output volume, the sensitivity, push to talk's release delay and echo cancellation / automatic
  // gain follow Settings → Voice during the call; only what changed since the session was given it is applied.
  const prefs = voicePrefs(o.settings);
  const { inputVolume, outputVolume, pttReleaseMs } = prefs;
  const { auto: sensitivityAuto, thresholdDb } = prefs.sensitivity;
  const { echo, agc } = prefs.processing;
  useEffect(() => {
    const was = prefsInForce.current;
    if (session === null || was === null) return;
    const now: VoicePrefs = { inputVolume, outputVolume, pttReleaseMs, sensitivity: { auto: sensitivityAuto, thresholdDb }, processing: { echo, agc } };
    prefsInForce.current = now;
    if (was.inputVolume !== inputVolume) session.setInputVolume(inputVolume);
    if (was.outputVolume !== outputVolume) session.setOutputVolume(outputVolume);
    if (was.pttReleaseMs !== pttReleaseMs) session.setPttRelease(pttReleaseMs);
    if (was.sensitivity.auto !== sensitivityAuto || was.sensitivity.thresholdDb !== thresholdDb) session.setSensitivity(now.sensitivity);
    if (was.processing.echo !== echo || was.processing.agc !== agc) session.setProcessing(now.processing).catch(() => undefined);
  }, [session, inputVolume, outputVolume, pttReleaseMs, sensitivityAuto, thresholdDb, echo, agc]);
  // Push to talk (decision 2): only in ptt mode with a key that is one non-text key on its own; a stored key that is
  // not (an older rule saved KeyV) binds nothing, and Settings shows it as unset.
  const applied = state?.mode;
  const pttKey = o.settings[SETTINGS.voicePttKey];
  useEffect(() => {
    const mod = module.current;
    if (session === null || mod === null || applied !== 'ptt' || pttKey === undefined || !isPushToTalkKey(pttKey)) return;
    return mod.bindPushToTalk({ code: pttKey, onChange: down => session.pushToTalk(down) });
  }, [session, applied, pttKey]);

  // Leaving the shell (sign-out, a lost account) ends the call as the person's own leave.
  useEffect(() => {
    mounted.current = true;
    return () => {
      recoveryRef.current = null;
      mounted.current = false;
      const hold = removedHold.current;
      removedHold.current = null;
      if (hold !== null) { clearTimeout(hold.timer); hold.off(); }
      const s = sessionRef.current;
      unsubscribe.current?.();
      startAbort.current?.abort();
      if (announceTimer.current !== null) clearTimeout(announceTimer.current);
      if (s !== null) void s.leave('user');
    };
  }, []);

  leaveRef.current = leave;
  return {
    channelId, recoveryChannelId, recoveryCamera, recoverySharing, recentlyEnded, joining, session, state, announcement, lasting, announce,
    notice, blockedMidCall, unsupported, ringExpired,
    pauseFocusPending, markPauseFocused: () => setPauseFocusPending(false),
    takeStageFocus: () => { const want = stageFocus.current; stageFocus.current = false; return want; },
    join: joinOnly, restore, leave, closeUnsupported, toggleMute, toggleDeafen, selfMute, selfDeafen, selfMuted, selfDeafened, toggleCamera, toggleShare, shareOptions, startShare, applyShare, setVolume, setStreamVolume, startAudio,
  };
}

/** #37: the silent-share notice that names what this browser and system can do. */
function shareAudioKey(): StringKey {
  const data = (navigator as Navigator & { userAgentData?: { platform?: string; brands?: Array<{ brand: string }> } }).userAgentData;
  const advice = shareAudioAdvice({ ua: navigator.userAgent, uaPlatform: data?.platform, brands: data?.brands?.map(b => b.brand), desktop: desktopBridge() !== undefined });
  if (advice === 'app') return 'voice.shareAudio.none.application';
  return advice === 'unsupported' ? 'voice.shareAudio.none.unsupported'
    : advice === 'tab-or-system' ? 'voice.shareAudio.none.tabOrSystem' : 'voice.shareAudio.none.tabOnly';
}
