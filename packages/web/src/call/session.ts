// The page call session (L-WEB-50): it owns the LiveKit Room through @dilla/media’s joinCall, and the media keys
// through the key relay (keys.ts). Order matters at both ends:
// - join: subscribe onCallKeys, then send callJoin (the join key is a callKeys push posted just before callJoin’s
//   answer), then joinCall with that key and minEpoch = the join epoch;
// - leave: a person’s leave posts callLeave before the room disconnects; a block or removal stops media first and
//   posts callLeave after, because keys must stop before anything else then.
// No key byte reaches the state, a log or an error here: keys.ts holds the only copies and zero-fills them.
// This module is loaded only through load.ts (L-WEB-51), so livekit-client and RNNoise stay out of the entry chunk.
import type { CallJoinResult, CoreClient, IceServerTuple } from '@dilla/client-core';
import { getBrowser } from 'livekit-client';
import {
  chooseVideoCodec, decodeCaps, DillaInputStage, DillaMicrophoneProcessor, noisePreference, processingConstraints, setBrowserSuppression, joinCall, micOptions, publishDefaults, refreshIceServers,
  screenAudioOptions, SpeakerPolicy, VoiceGate, type CaptureProcessing, type InputStageOptions, type SensitivityConfig, type CallCaps, type CallSession, type JoinCallOptions, type NoisePreference, type ProcessorStatus,
} from '@dilla/media';
import { reportProcessorStatus, reportStageStatus } from './processor-status.ts';
import { acquireCamera, acquireMic, acquireScreen, type CaptureMedia, type DisplayMedia, type ScreenCapture, type ShareAudioKind } from './devices.ts';
import { createKeyRelay, toEpochKeys } from './keys.ts';
import { DEFAULT_SHARE_OPTIONS, shareEncoding, shareVideoConstraints, type ShareOptions } from './share-options.ts';
import { readTileStats, type TileStats } from './tile-stats.ts';
import {
  analyserMeter, LEVEL_PUBLISH_MS, SPEAK_TICK_MS, SpeakingDetector, type LevelMeter,
} from './speaking.ts';
import {
  callStatsReport, connectionOf, EMPTY_REPORT, linkSample, LINK_EVERY_MS, LINK_STALL_MS, mediaTotals, STATS_EVERY_MS, type LinkSample, type LinkTotals, type MediaTotals,
} from './stats.ts';
import { gainOf, isPercent, isVolume, type Volumes } from './volume.ts';

export { isVoiceSupported, localDecodeList } from '@dilla/media';
export type { ShareOptions } from './share-options.ts';
export type { TileStats } from './tile-stats.ts';
export { bindPushToTalk } from './ptt.ts';
export { streamVolumesFromSettings, volumesFromSettings } from './volume.ts';

/** FX2: one audio input offered as a share's sound; `monitor` when its label says it carries the computer's own sound. */
export interface ShareSource { id: string; label: string; monitor: boolean }
export type TileState = 'connecting' | 'not-heard' | 'ok' | 'unverified';
export interface PageCallTile {
  deviceId: string;
  userId: string;
  state: TileState;
  speaking: boolean;
  /** Only for an 'ok' tile and never while paused: unverified media is never shown. */
  video: MediaStream | null;
  screen: MediaStream | null;
  /** #37: this device has a screen_share_audio publication (subscribed or not). Only for an 'ok' tile and never while
   *  paused, as `screen`; false otherwise. It is about the stream, not the viewer's deafen or volume. */
  screenAudio: boolean;
  /** Task 8c fix round 1: the RTT this device says it measures to the SFU (its `dilla.rtt` attribute, 0..5000 ms), or null when
   *  it publishes none or something else. Never this page's own link. */
  rttMs?: number | null;
}

/** Task 8c fix round 1: the one participant attribute a page publishes about itself: its own RTT to the SFU in whole ms. */
export const RTT_ATTRIBUTE = 'dilla.rtt';
/** A published RTT changes only by at least this much (fewer attribute updates for the room). */
export const RTT_STEP_MS = 5;
/** The value another device published, if it is a whole number of ms in 0..5000; anything else is no value. */
export function rttOf(attributes: Readonly<Record<string, string>> | undefined): number | null {
  const v = attributes?.[RTT_ATTRIBUTE];
  if (v === undefined || !/^(0|[1-9]\d{0,3})$/.test(v)) return null;
  const n = Number(v);
  return n <= 5000 ? n : null;
}
export interface PageCallState {
  phase: 'connecting' | 'connected' | 'reconnecting' | 'ended';
  muted: boolean;
  deafened: boolean;
  camera: boolean;
  sharing: boolean;
  paused: boolean;
  /** 'failed': a microphone exists but could not be published (E_MIC_PUBLISH); the call goes on listen-only. */
  mic: 'ok' | 'denied' | 'missing' | 'failed';
  /** The input mode in force (task-9 review I2: it follows Settings → Voice during a call). */
  mode: 'vad' | 'ptt';
  tiles: PageCallTile[];
  /** The local input level, 0..12. */
  level: number;
  /** Product walk M4: the SFU hears this device speaking (its own isSpeaking), never while muted or paused. */
  speaking: boolean;
  /** Product walk I3: what this device sends, for its own tile's self-view: the camera and the screen as local views
   *  (never a received stream), null while not sent. */
  localCamera: MediaStream | null;
  localScreen: MediaStream | null;
  /** #37: the sound this device's share carries: a tab's, the system's, or none (the browser gave no audio, or the grant
   *  did not allow it). null while not sharing. */
  shareAudio: ShareAudioKind | 'device' | null;
  /** FX2: where the share's sound comes from: 'browser' (the tab's or the system's, from getDisplayMedia), the id of an
   *  audio input the person picked ('device' above), or null (no sound, or not sharing). */
  shareSource: string | null;
  /** Product walk M1: noise suppression in force; it follows Settings → Voice during the call. */
  noise: NoisePreference | 'on';
  /** web3.3: the browser holds call audio back (livekit’s canPlaybackAudio is false) until startAudio() is pressed. */
  audioBlocked: boolean;
  /** web3.4 V2: the remote audio tracks this device is subscribed to (expected: pending and attached) and those
   *  attached, so playing (attached stays the same while deafened or paused holds a new track back). */
  audio: { attached: number; expected: number };
  /** MP5: the latest local link read (every 2 s while connected): the ping and what this device sends and receives. Absent
   *  before the first read, while not connected, and after leave. */
  link?: LinkSample | undefined;
  /** Task 8c: the instance's share bitrate cap in bit/s (the calls response's caps), which the share's encoding and the
   *  picker's estimate keep under; null before the join answered. */
  shareCapBps: number | null;
  /**
   * An E_ code, or null. Once the phase is 'ended' because the room ended on its own (WB client I3): E_CALL_ENDED (the
   * room was closed: the call ended for everyone), E_REMOVED (the instance removed this device from the room) or
   * E_DISCONNECTED (the connection ended and LiveKit's reconnect gave up).
   */
  error: string | null;
}
/** 'lost' (WB client I3): the room ended on its own; media stops first and the worker hears callLeave 'lost'. */
export type LeaveReason = 'user' | 'pagehide' | 'switch' | 'blocked' | 'removed' | 'lost';
export interface PageCall {
  readonly state: PageCallState;
  subscribe(fn: () => void): () => void;
  setMuted(on: boolean): void;
  setDeafened(on: boolean): void;
  setCamera(on: boolean): Promise<void>;
  /** Starts (with the picker's `options`, the defaults without) or stops this device's screen share. */
  setSharing(on: boolean, options?: ShareOptions): Promise<void>;
  /**
   * Task 8c: the picker's Apply while sharing. A new resolution or frame rate is applied to the running capture
   * (`applyConstraints`) and to the sender's encoding (`setParameters`: the same transformed sender, no new track); a new source
   * or sound choice needs the browser's picker again, so the share restarts through the same publishTrack path (the dilla transform). Not sharing:
   * the options are kept for the next start.
   */
  setShareOptions(options: ShareOptions): Promise<void>;
  /** Task 8c: one read of the stage's per-tile stats (the own microphone's codec, every screen's frame size and rate). */
  tileStats(): Promise<TileStats>;
  /** FX2: the audio inputs a running share can take its sound from (the call's own microphone left out), and whether the
   *  browser gave them labels. */
  shareSources(): Promise<{ sources: ShareSource[]; labeled: boolean }>;
  /** FX2: publishes audio input `id` as the share's sound (replacing an earlier pick), or unpublishes it with null. Refused
   *  (E_UNAVAILABLE) while no share runs and while the browser's own sound is the share's. */
  setShareSource(id: string | null): Promise<void>;
  /** 0..200 in steps of 10: the person's voice (default) or, with `'stream'`, their shared screen's sound (T5). */
  setVolume(userId: string, volume: number, source?: 'voice' | 'stream'): void;
  /** RV-3: driven by the call slice’s phase 'paused'. */
  setPaused(on: boolean): Promise<void>;
  /** WB security I1: the hold after a refused rekey (phase paused, not a changed identity): every media key goes at
   *  once, so nothing more is sent under the epoch the page held; the next epoch’s key installs on resume. */
  dropKeys(upTo?: bigint): void;
  pushToTalk(down: boolean): void;
  /** Switches the input mode during the call: push to talk closes the microphone at once, until the key is held. */
  setMode(mode: 'vad' | 'ptt'): void;
  /** Product walk M1: switches noise suppression during the call (the microphone's processor is set or stopped). */
  setNoise(noise: NoisePreference | 'on'): Promise<void>;
  /** Signal task 10b: the input volume, 0..200 %, applied by the input stage's gain before the sender. */
  setInputVolume(percent: number): void;
  /** Signal task 10b: the output volume, 0..200 %, multiplied into every remote person's gain. */
  setOutputVolume(percent: number): void;
  /** Signal task 10b: the voice-activity gate's automatic or manual threshold (in force under voice activity only). */
  setSensitivity(sensitivity: { auto: boolean; thresholdDb: number }): void;
  /** Signal task 10b: push to talk keeps sending this long (ms) after the key is let go. */
  setPttRelease(ms: number): void;
  /** Signal task 10b: echo cancellation and automatic gain; the microphone is captured again with them, the call goes on. */
  setProcessing(processing: CaptureProcessing): Promise<void>;
  /** web3.3: the "turn on audio" press: room.startAudio(), then canPlaybackAudio read again; a refusal never throws. */
  startAudio(): Promise<void>;
  /** web3.4 V1: moves the call's sound to output device `id` ('' is the system default) during the call. A no-op where
   *  the browser cannot choose an output, and after the leave; rejects when the browser refuses the device. */
  setOutput(id: string): Promise<void>;
  leave(reason: LeaveReason): Promise<void>;
}

/** What the session needs of an RNNoise processor; DillaRnnoiseProcessor by default. */
export interface NoiseProcessor { setPttActive(active: boolean): void; setCaptureAllowed?(allowed: boolean): void; setMode?(mode: 'vad' | 'ptt'): void; flush?(): void; destroy?(): Promise<void>; setProcessing?(processing: CaptureProcessing): void }
/** Signal task 10b: what the session needs of the input stage (DillaInputStage in the browser): the processor set on the
 *  microphone, wrapping the noise processor (or none), with the input gain and the voice-activity gate. */
export interface InputStage {
  setGain(gain: number): void;
  setGate(gate: SensitivityConfig): void;
  setCaptureAllowed(allowed: boolean): void;
  /** The raw capture the stage was last given. */
  readonly input?: MediaStreamTrack;
  /** Fix round 1 (M1): whether gain and gate run (`active`), or why not. */
  readonly state?: 'pending' | 'active' | 'unsupported' | 'failed';
  /** Its own audio context, which a browser holding audio back starts suspended (resumed by startAudio). */
  readonly context?: AudioContext;
  destroy?(): Promise<void>;
}

export interface PageCallDeps {
  bridge: Pick<CoreClient, 'call' | 'onCallKeys'>;
  joinCall: (o: JoinCallOptions) => Promise<CallSession>;
  now(): number;
  setTimer(fn: () => void, ms: number): unknown;
  clearTimer(handle: unknown): void;
  /** getDisplayMedia is needed only for a screen share. */
  media: CaptureMedia & Partial<DisplayMedia>;
  /** Defaults to a DillaRnnoiseProcessor in the given mode. */
  createNoiseProcessor?: (mode: 'vad' | 'ptt') => NoiseProcessor;
  /** Signal task 10b: the input stage around the noise processor (null: noise suppression off). Absent: no stage (no input
   *  volume, no gate); browserDeps gives DillaInputStage. */
  createInputStage?: (inner: NoiseProcessor | null, options: InputStageOptions) => InputStage;
  /** The key relay’s copy step (tests observe that every copy is zero-filled); a plain copy by default. */
  copyKey?: (buffer: ArrayBuffer) => Uint8Array;
  /** Where pagehide is heard; the page’s window by default. */
  win?: Pick<Window, 'addEventListener' | 'removeEventListener'>;
  /** Product walk I3: a local view of one sent track for the self-view; `new MediaStream([track])` by default (null where
   *  the browser has no MediaStream). */
  viewOf?: (track: MediaStreamTrack) => MediaStream | null;
  /** web3.3: the container remote audio elements are attached into; one hidden `<div data-dilla-call-audio>` on
   *  document.body by default (null where there is no document). */
  audioHost?: () => HTMLElement | null;
  /** Fix round 1 (I1, M2): the fallback maker of the call’s AudioContext, the mixer remote audio plays through
   *  (webAudioMix), used only when PageCallOptions.audioContext is absent; a new AudioContext by default (null where the
   *  browser has none). The session closes the context on every leave. */
  audioContext?: () => AudioContext | null;
  /** web3.8 V1: a level reader on one audio track in the call's context (an AnalyserNode, never connected to the
   *  destination); analyserMeter by default. Null means no local level: the SFU's speaker flag is the fallback. */
  createLevelMeter?(ctx: AudioContext, track: MediaStreamTrack): LevelMeter | null;
  /** web3.9 VOICE-FIX fix round 1 (I1): true where this engine keeps a second capture of one microphone live beside the
   *  first (concurrentCapture), so a parked restart may acquire before it swaps. Anything else stops first (restartTrack). */
  concurrentCapture?: boolean;
}

/** web3.9 VOICE-FIX fix round 1 (I1): whether this engine keeps a second capture of one microphone live beside the first.
 *  WebKit (Safari, and every iOS browser) ends the new one (livekit-client LocalTrack.ts:372-374), so there, and on any
 *  engine not on this allowlist, the parked restart stops first. Final review I-1: desktop Chrome, Edge and Firefox only.
 *  getBrowser sets no `os` on Android, so `ua` (the user agent getBrowser read) rules out anything Android or mobile. */
export function concurrentCapture(b: ReturnType<typeof getBrowser>, ua: string): boolean {
  return (b?.name === 'Chrome' || b?.name === 'Firefox') && b.os !== 'iOS' && !/android|mobile/i.test(ua);
}

function defaultAudioContext(): AudioContext | null {
  if (typeof AudioContext === 'undefined') return null;
  try { return new AudioContext(); } catch { return null; }
}

/** The default audio host: one hidden, aria-hidden container on document.body, made on first use. */
function defaultAudioHost(): HTMLElement | null {
  if (typeof document === 'undefined' || document.body === null) return null;
  let host = document.querySelector<HTMLElement>('div[data-dilla-call-audio]');
  if (host === null) {
    host = document.createElement('div');
    host.setAttribute('data-dilla-call-audio', '');
    host.hidden = true;
    host.setAttribute('aria-hidden', 'true');
    document.body.appendChild(host);
  }
  return host;
}

export interface PageCallOptions {
  instanceId: string;
  /** Device ids; '' is the system default. */
  input: string;
  output: string;
  mode: 'vad' | 'ptt';
  noise: NoisePreference | 'on';
  /** user id → 0..200 (the voice.volume.<user> settings). */
  volumes: Volumes;
  /** T5: user id → 0..200 (the voice.streamVolume.<user> settings), the volume of that person's shared screen sound.
   *  0 is the stream mute. Absent = 100 for everyone. */
  streamVolumes?: Volumes;
  /** WB client I1: aborting ends a start that has not connected yet, the microphone prompt included: callLeave is
   *  posted at once (the worker cancels its join), the microphone is stopped, and the start rejects with E_ABORTED. */
  signal?: AbortSignal;
  /** Product walk M5: this browser's own mute or deafen kept from its last call; the call starts in it, so a reload never
   *  reopens a microphone the person had muted. */
  self?: 'muted' | 'deafened' | null;
  /** web3.8 MUTE: the own mute or deafen as it stands now (the page's last press, else the stored setting). `self` is read
   *  before the microphone prompt; a press after it is read here right before the microphone is published (so it goes out
   *  already closed) and again when the call connects. Only ever toward privacy: a value less private than the session's
   *  changes nothing. A getter that throws counts as no answer. */
  selfNow?: () => 'muted' | 'deafened' | null;
  /** web3.8 MUTE: push, not only pull. The session registers `apply` while it starts and the page calls it after each
   *  press (once `selfNow` answers the new value); the returned function unregisters it, which the session does at
   *  connected or when the start fails. */
  watchSelf?: (apply: () => void) => () => void;
  /** Fix round 2 (Minor 1): the call’s AudioContext, made by the shell synchronously inside the person’s press (useCall’s
   *  join, before its first await), so the browser lets it run. From here on the session owns it and closes it on every
   *  way out. Absent: the session makes its own (PageCallDeps.audioContext), a fallback that may start suspended. */
  audioContext?: AudioContext | null;
  /** Signal task 10b (Settings → Voice): input and output volume (0..200 %, 100 by default), the sensitivity (automatic by
   *  default), push to talk's release delay (ms, 0 by default here; the setting's default is 200) and the capture's echo
   *  cancellation and automatic gain (both on by default). */
  inputVolume?: number;
  outputVolume?: number;
  sensitivity?: { auto: boolean; thresholdDb: number };
  pttReleaseMs?: number;
  processing?: CaptureProcessing;
}

/** Fix round 1 (L2): a manual threshold as the gate takes it, a whole −100..0 dB (−50 when not a number). */
const dbThreshold = (db: number | undefined): number => (typeof db === 'number' && Number.isFinite(db) ? Math.max(-100, Math.min(0, Math.round(db))) : -50);
/** A push-to-talk release delay, 0..1000 ms (0 when not a number). */
const releaseMs = (ms: number | undefined): number => (typeof ms === 'number' && Number.isFinite(ms) ? Math.max(0, Math.min(1000, ms)) : 0);

/** A refusal or failure of the session, by code only (copy maps codes, never error text). */
export class PageCallError extends Error {
  readonly code: string;
  constructor(code: string) {
    super(code);
    this.name = 'PageCallError';
    this.code = code;
  }
}

const CODE = /^E_[A-Z0-9_]+/;
export function pageCallError(err: unknown): PageCallError {
  if (err instanceof PageCallError) return err;
  const code = (err as { code?: unknown } | null)?.code;
  if (typeof code === 'string' && CODE.test(code)) return new PageCallError(code);
  const fromMessage = err instanceof Error ? CODE.exec(err.message)?.[0] : undefined;
  return new PageCallError(fromMessage ?? 'E_INTERNAL');
}

/** The browser’s own dependencies, for the shell (task 9). */
export function browserDeps(bridge: PageCallDeps['bridge']): PageCallDeps {
  const md = navigator.mediaDevices;
  return {
    bridge,
    joinCall,
    now: () => Date.now(),
    setTimer: (fn, ms) => setTimeout(fn, ms),
    clearTimer: (h) => clearTimeout(h as ReturnType<typeof setTimeout>),
    concurrentCapture: concurrentCapture(getBrowser(navigator.userAgent), navigator.userAgent),
    createInputStage: (inner, options) => new DillaInputStage(inner as ConstructorParameters<typeof DillaInputStage>[0], options),
    media: {
      getUserMedia: (c) => md.getUserMedia(c),
      enumerateDevices: () => md.enumerateDevices(),
      getDisplayMedia: (o) => md.getDisplayMedia(o),
    },
  };
}

type PublishOptions = ReturnType<typeof micOptions>;
type Source = NonNullable<PublishOptions['source']>;
const SOURCE = {
  microphone: 'microphone' as Source,
  camera: 'camera' as Source,
  screen: 'screen_share' as Source,
  screenAudio: 'screen_share_audio' as Source,
};
/** livekit TrackSource numbers in ParticipantPermission.canPublishSources. */
const PERMISSION: Record<'camera' | 'screen' | 'screenAudio', number> = { camera: 1, screen: 3, screenAudio: 4 };
const EV = {
  participantConnected: 'participantConnected',
  participantDisconnected: 'participantDisconnected',
  trackSubscribed: 'trackSubscribed',
  trackUnsubscribed: 'trackUnsubscribed',
  trackPublished: 'trackPublished',
  trackUnpublished: 'trackUnpublished',
  activeSpeakers: 'activeSpeakersChanged',
  reconnecting: 'reconnecting',
  reconnected: 'reconnected',
  disconnected: 'disconnected',
  permissions: 'participantPermissionsChanged',
  audioPlayback: 'audioPlaybackChanged',
  attributes: 'participantAttributesChanged',
} as const;

/** livekit DisconnectReason values the session reads (livekit protocol): PARTICIPANT_REMOVED, ROOM_DELETED, ROOM_CLOSED. */
const DISCONNECT = { participantRemoved: 4, roomDeleted: 5, roomClosed: 10 } as const;
/** Why a room ended on its own, as the session's error code (WB client I3). */
export function endCodeOf(reason: unknown): 'E_CALL_ENDED' | 'E_REMOVED' | 'E_DISCONNECTED' {
  if (reason === DISCONNECT.roomDeleted || reason === DISCONNECT.roomClosed) return 'E_CALL_ENDED';
  if (reason === DISCONNECT.participantRemoved) return 'E_REMOVED';
  return 'E_DISCONNECTED';
}

/** `p`, or E_ABORTED once `signal` aborts first; `onLate` gets the value of a `p` that settles after the abort. */
function abortable<T>(p: Promise<T>, signal: AbortSignal | undefined, onLate: (v: T) => void): Promise<T> {
  if (signal === undefined) return p;
  if (signal.aborted) { void p.then(onLate, () => undefined); return Promise.reject(new PageCallError('E_ABORTED')); }
  return new Promise<T>((resolve, reject) => {
    let done = false;
    const onAbort = (): void => { if (done) return; done = true; reject(new PageCallError('E_ABORTED')); };
    signal.addEventListener('abort', onAbort, { once: true });
    p.then((v) => {
      signal.removeEventListener('abort', onAbort);
      if (done) { onLate(v); return; }
      done = true;
      resolve(v);
    }, (e: unknown) => {
      signal.removeEventListener('abort', onAbort);
      if (done) return;
      done = true;
      reject(e instanceof Error ? e : new PageCallError('E_INTERNAL'));
    });
  });
}

/** Tiles are polled for verifiedIdentities() every second. */
export const TILE_POLL_MS = 1_000;
/** ICE credentials are refreshed this long before the expiry their username carries (DEV-53). */
export const ICE_EARLY_MS = 300_000;
const ICE_RETRY_MS = 30_000;
/** How long a share waits for the SFU’s new permission. */
export const SHARE_WAIT_MS = 10_000;

// eslint-disable-next-line @typescript-eslint/no-explicit-any -- livekit’s emitter listener type
type AnyListener = (...args: any[]) => void;
interface Events { on(e: string, f: AnyListener): unknown; off(e: string, f: AnyListener): unknown }
interface RemoteView {
  identity: string;
  isSpeaking: boolean;
  setVolume(volume: number, source?: Source): void;
  getTrackPublication(source: Source): { track?: { mediaStream?: MediaStream } } | undefined;
  trackPublications?: Map<string, { track?: unknown; source?: unknown }>;
  /** LiveKit participant attributes; only `dilla.rtt` is read here (rttOf), `dilla.vdec` by the codec choice. */
  attributes?: Record<string, string>;
}
/** What the session needs of a subscribed remote track to play it (livekit’s RemoteAudioTrack). */
interface RemoteAudio {
  kind: string; attach(): HTMLMediaElement; detach(element: HTMLMediaElement): unknown;
  /** The decrypted track the element plays (livekit’s); web3.8 V1 reads its level when the element has no stream. */
  mediaStreamTrack?: MediaStreamTrack;
  /** livekit's Track.Source, when the publication does not say it. */
  source?: unknown;
}
interface LocalPub { track?: { mute(): unknown; unmute(): unknown; setProcessor?(p: unknown): Promise<void>; stopProcessor?(): Promise<void>; restartTrack?(constraints: MediaTrackConstraints): Promise<unknown>; replaceTrack?(track: MediaStreamTrack, userProvidedTrack?: boolean): Promise<unknown>; mediaStreamTrack?: MediaStreamTrack } }
interface Permissions { canPublish?: boolean; canPublishSources?: number[] }

/** The earliest expiry, in ms, among the TURN usernames (`<expiry seconds>:<…>`), or null. */
export function iceExpiryMs(list: IceServerTuple[]): number | null {
  let min: number | null = null;
  for (const [, username] of list) {
    const m = /^(\d{1,12}):/.exec(username);
    if (m) {
      const at = Number(m[1]) * 1000;
      if (min === null || at < min) min = at;
    }
  }
  return min;
}

export async function startPageCall(deps: PageCallDeps, target: { channelId: string; vdec: string }, opts: PageCallOptions): Promise<PageCall> {
  // The microphone prompt comes before the join, so the room never shows this device while the prompt is open. An abort
  // while it is open ends the start at once (WB client I1); a microphone granted after that is stopped.
  // Fix round 1 (I1, M2): the call’s own AudioContext. The shell makes it inside the person’s press (useCall’s join, before
  // its first await; fix round 2, Minor 1) and passes it in; only without one is it made here, after the shell’s awaits,
  // where Safari may start it suspended (audioBlocked then offers the press). livekit mixes remote audio into it
  // (webAudioMix) and never closes a context it did not make, so every way out of this call closes it here. The output
  // device is set on it: with webAudioMix the elements stay muted and everything plays through the context’s destination.
  const ctx = opts.audioContext !== undefined ? opts.audioContext : (deps.audioContext ?? defaultAudioContext)();
  let ctxClosed = false;
  // web3.8 V1: every level meter registers its close here, so every way out that closes the context closes them first.
  const meterClosers = new Set<() => void>();
  /** Makes a meter on `track` in the call's context, or null (no context, a refusal, a closed context): the SFU's flag
   *  is then the fallback. Never throws. The returned meter's close is idempotent and unregisters itself. */
  const makeMeter = (track: MediaStreamTrack | null | undefined): LevelMeter | null => {
    if (ctx === null || ctxClosed || track == null) return null;
    let made: LevelMeter | null;
    try { made = (deps.createLevelMeter ?? analyserMeter)(ctx, track); } catch { return null; }
    if (made === null) return null;
    const inner = made;
    let closed = false;
    const close = (): void => {
      if (closed) return;
      closed = true;
      meterClosers.delete(close);
      try { inner.close(); } catch { /* the node is gone with the context */ }
    };
    meterClosers.add(close);
    return { read: () => inner.read(), close };
  };
  const closeCtx = (): void => {
    if (ctx === null || ctxClosed) return;
    for (const close of [...meterClosers]) close();
    ctxClosed = true;
    try { void Promise.resolve(ctx.close()).catch(() => undefined); } catch { /* already closed */ }
  };
  /** Sends the call's sound to output device `id` ('' is the system default). Null where the context cannot choose an
   *  output (Firefox): the system default plays. web3.4 V1: also the mid-call switch (setOutput). */
  const setSink = ctx !== null && typeof (ctx as { setSinkId?: unknown }).setSinkId === 'function'
    ? async (id: string): Promise<void> => { await (ctx as unknown as { setSinkId(id: string): Promise<void> }).setSinkId(id); }
    : null;
  // A refused device at the start leaves the system default playing.
  if (setSink !== null && opts.output !== '') void setSink(opts.output).catch(() => undefined);
  let mic: Awaited<ReturnType<typeof acquireMic>>;
  try {
    mic = await abortable(acquireMic(deps.media, opts.input, false, opts.processing), opts.signal, (late) => { if ('track' in late) late.track.stop(); });
  } catch (err) {
    closeCtx();
    throw err;
  }
  if (opts.signal?.aborted === true) {
    if ('track' in mic) mic.track.stop();
    closeCtx();
    throw new PageCallError('E_ABORTED');
  }
  let roster = new Map<string, { userId: string; leaf: number }>();
  let selfLeaf = -1;
  let afterRoster: () => void = () => undefined;
  let installError: (code: string) => void = () => undefined;
  const relay = createKeyRelay({
    onRoster: (m, leaf) => { roster = new Map(m); selfLeaf = leaf; afterRoster(); },
    onInstallError: (code) => installError(code),
    ...(deps.copyKey !== undefined ? { copy: deps.copyKey } : {}),
  });
  // Subscribed before callJoin is sent: the join key is pushed just before callJoin’s answer.
  const unsubscribe = deps.bridge.onCallKeys(relay.listener);
  // pagehide is heard from here on (review m1). During startup it ends the attempt at once: no install afterwards,
  // callLeave('pagehide') posted, media released without waiting; once the call is up it becomes leave('pagehide').
  const win = deps.win ?? (typeof window === 'undefined' ? undefined : window);
  const statusOwner = Symbol('microphone');
  let aborted = false;
  let joinedSession: CallSession | null = null;
  const stopMic = (): void => { if ('track' in mic) mic.track.stop(); };
  /** A pagehide, or the page's abort (WB client I1), during startup: everything stops now, nothing waits. */
  const startupHide = (reason: 'pagehide' | 'user' = 'pagehide'): void => {
    if (aborted) return;
    aborted = true;
    reportProcessorStatus(statusOwner, null); reportStageStatus(statusOwner, null);
    relay.close();
    unsubscribe();
    win?.removeEventListener('pagehide', pageHide);
    opts.signal?.removeEventListener('abort', onAbort);
    void deps.bridge.call({ m: 'callLeave', reason }).catch(() => undefined);
    const live = joinedSession;
    if (live !== null) {
      try { live.manager.dispose(); } catch { /* released below */ }
      void Promise.resolve(live.room.disconnect()).catch(() => undefined);
      void live.release().catch(() => undefined);
    }
    stopMic();
    closeCtx();
  };
  let onPageHide: () => void = () => startupHide('pagehide');
  const pageHide = (): void => onPageHide();
  win?.addEventListener('pagehide', pageHide);
  // Until the call is up, the page's abort ends the start as a pagehide does, as the person's own leave.
  const onAbort = (): void => startupHide('user');
  opts.signal?.addEventListener('abort', onAbort, { once: true });
  /** Ends a failed start. After a pagehide everything is already torn down and the attempt ends E_ABORTED. */
  const abandon = async (err: unknown, posted: boolean): Promise<PageCallError> => {
    if (aborted) return new PageCallError('E_ABORTED');
    aborted = true;
    reportProcessorStatus(statusOwner, null); reportStageStatus(statusOwner, null);
    relay.close();
    unsubscribe();
    win?.removeEventListener('pagehide', pageHide);
    opts.signal?.removeEventListener('abort', onAbort);
    stopMic();
    const live = joinedSession;
    // Review M3: a failure after the room listeners were registered unregisters them and lets every attached track go.
    teardownRoom();
    offMisroute();
    detachEarly();
    closeCtx();
    // Keys stop first: media is released before the worker is told to leave.
    if (live !== null) await live.release().catch(() => undefined);
    if (posted) await deps.bridge.call({ m: 'callLeave', reason: 'user' }).catch(() => undefined);
    return pageCallError(err);
  };
  /** WB client m2: the room's own end, heard from the moment joinCall resolved; until the call is up it ends the start. */
  let endedBy: string | null = null;
  let detachEarly: () => void = () => undefined;
  let teardownRoom: () => void = () => undefined;
  /** fix/firefox-misrouted-audio: stops noting the manager's recovered misroutes; at the leave or a failed start. */
  let offMisroute: () => void = () => undefined;
  const checkAborted = (): void => {
    if (aborted) throw new PageCallError('E_ABORTED');
    if (endedBy !== null) throw new PageCallError(endedBy);
  };

  let join: CallJoinResult;
  try {
    join = (await deps.bridge.call({ m: 'callJoin', channelId: target.channelId, vdec: target.vdec })) as CallJoinResult;
  } catch (err) {
    throw await abandon(err, false);
  }
  if (aborted) throw new PageCallError('E_ABORTED');
  let joinEpoch: bigint;
  let caps: CallCaps;
  try {
    joinEpoch = BigInt(join.epoch);
    caps = decodeCaps(join.caps);
  } catch (err) {
    throw await abandon(err instanceof SyntaxError ? new PageCallError('E_NO_EPOCH') : err, true);
  }
  const joinKey = relay.takeJoin(join.callId, joinEpoch);
  if (joinKey === null) throw await abandon(new PageCallError('E_NO_EPOCH'), true);

  let session: CallSession | null = null;
  let joinError: unknown = null;
  try {
    session = await deps.joinCall({
      livekitUrl: join.livekitUrl,
      token: join.token,
      iceServers: join.iceServers,
      epoch: toEpochKeys(joinKey, joinEpoch),
      lock: { instanceId: opts.instanceId, callGroupId: join.groupId },
      roomOptions: {
        webAudioMix: ctx !== null ? { audioContext: ctx } : true,
        publishDefaults: publishDefaults(caps),
        ...(opts.output !== '' ? { audioOutput: { deviceId: opts.output } } : {}),
      },
    });
  } catch (err) {
    joinError = err;
  } finally {
    joinKey.baseKey.fill(0);
  }
  if (session === null) throw await abandon(joinError ?? new PageCallError('E_INTERNAL'), true);
  joinedSession = session;
  // WB client m2: the room's end and its reconnects are heard from here on, not only once the microphone is published.
  let onRoomEnd: (reason: unknown) => void = (reason) => { endedBy ??= endCodeOf(reason); };
  let onRoomReconnecting: () => void = () => undefined;
  let onRoomReconnected: () => void = () => undefined;
  {
    const early = session.room as unknown as Events;
    const listeners: Array<[string, AnyListener]> = [
      [EV.disconnected, (reason: unknown) => onRoomEnd(reason)], [EV.reconnecting, () => onRoomReconnecting()],
      [EV.reconnected, () => onRoomReconnected()],
    ];
    for (const [e, f] of listeners) early.on(e, f);
    detachEarly = () => { for (const [e, f] of listeners) early.off(e, f); };
  }
  if (aborted) {
    // pagehide arrived while joinCall ran: what startupHide could not reach is released now.
    try { session.manager.dispose(); } catch { /* released below */ }
    void Promise.resolve(session.room.disconnect()).catch(() => undefined);
    await session.release().catch(() => undefined);
    closeCtx();
    throw new PageCallError('E_ABORTED');
  }
  // Only the call id outlives the start; the token and TURN credentials stay with joinCall (review m5).
  const callIdHex = join.callId;
  const firstIce = join.iceServers;

  const joined = session;
  const { room, manager } = joined;
  const release = (): Promise<void> => joined.release();
  /** web3.8 MUTE: stops listening for the page's presses (opts.watchSelf); at connected, or when the start fails. */
  let unwatch: () => void = () => undefined;
  try {
    relay.attach(manager);
    // fix/firefox-misrouted-audio: a peer whose audio Firefox put on a spare receiver was re-subscribed by the manager;
    // Diagnostics shows the stable code (callNote), nothing else of it.
    const managerEvents = manager as unknown as Partial<Events>;
    const onMisroute = (): void => { void deps.bridge.call({ m: 'callNote', code: 'E_MEDIA_MISROUTE' }).catch(() => undefined); };
    managerEvents.on?.('misrouteRecovered', onMisroute);
    offMisroute = () => { managerEvents.off?.('misrouteRecovered', onMisroute); };
    const events = room as unknown as Events;
    const local = room.localParticipant as unknown as Events & {
      identity: string; audioLevel?: number; isSpeaking?: boolean; permissions?: Permissions;
      publishTrack(track: MediaStreamTrack, o: PublishOptions): Promise<LocalPub>;
      unpublishTrack(track: MediaStreamTrack): Promise<unknown>;
    };
    const remotes = (): RemoteView[] => [...(room.remoteParticipants.values() as Iterable<unknown>)] as RemoteView[];

    let state: PageCallState = {
      phase: 'connecting', camera: false, sharing: false, paused: false,
      mic: 'ok', tiles: [], level: 0, error: null, mode: opts.mode, speaking: false, localCamera: null, localScreen: null,
      shareAudio: null, shareSource: null, noise: opts.noise, muted: opts.self === 'muted' || opts.self === 'deafened', deafened: opts.self === 'deafened',
      audioBlocked: false, audio: { attached: 0, expected: 0 }, shareCapBps: caps.maxShareBitrateBps,
    };
    const viewOf = deps.viewOf ?? ((track: MediaStreamTrack) => (typeof MediaStream === 'undefined' ? null : new MediaStream([track])));
    const listeners = new Set<{ fn: () => void }>();
    const set = (patch: Partial<PageCallState>): void => {
      state = { ...state, ...patch };
      for (const l of [...listeners]) {
        try { l.fn(); } catch (err) { queueMicrotask(() => { throw err; }); }
      }
    };
    let ended = false;
    const volumes: Volumes = { ...opts.volumes };
    const streamVolumes: Volumes = { ...opts.streamVolumes };
    // Signal task 10b: Settings → Voice's levels and switches, as the call applies them now.
    // Fix round 1 (L2): every level is checked here, so nothing non-finite or out of range reaches the stage or a timer.
    let inputVolume = opts.inputVolume !== undefined && isPercent(opts.inputVolume) ? opts.inputVolume : 100;
    let outputVolume = opts.outputVolume !== undefined && isPercent(opts.outputVolume) ? opts.outputVolume : 100;
    let sensitivity = { auto: opts.sensitivity?.auto !== false, thresholdDb: dbThreshold(opts.sensitivity?.thresholdDb) };
    let pttReleaseMs = releaseMs(opts.pttReleaseMs);
    let processing: CaptureProcessing = opts.processing ?? { echo: true, agc: true };
    // Fix round 3 (m1): a call started deafened (voice.self) was muted before it, as far as anyone knows: an undeafen keeps
    // the microphone muted (privacy-safe), never opens it after a reload.
    let mutedBeforeDeafen = opts.self === 'deafened';
    /** web3.8 MUTE: a mute or deafen pressed while the start runs (selfNow) joins the session's state before it is in use,
     *  never the other way. A deafen is applied as a mute first and then a deafen, so an undeafen keeps the microphone muted
     *  (R25: deafened goes back to muted, never to an open microphone). True when it changed anything. */
    const catchUp = (): boolean => {
      let now: 'muted' | 'deafened' | null = null;
      try { now = opts.selfNow?.() ?? null; } catch { now = null; }
      if (now === 'deafened' && !state.deafened) {
        mutedBeforeDeafen = true;
        state = { ...state, muted: true, deafened: true, speaking: false };
        return true;
      }
      if (now === 'muted' && !state.muted) {
        state = { ...state, muted: true, speaking: false };
        return true;
      }
      return false;
    };
    let verified = new Set<string>();
    const timers = new Set<unknown>();
    const later = (fn: () => void, ms: number): unknown => {
      const h = deps.setTimer(() => { timers.delete(h); fn(); }, ms);
      timers.add(h);
      return h;
    };
    const cancel = (h: unknown): void => { if (timers.delete(h)) deps.clearTimer(h); };
    installError = (code) => { if (!ended) set({ error: code }); };

    // ---- tiles and remote audio
    const streamOf = (p: RemoteView, source: Source): MediaStream | null => p.getTrackPublication(source)?.track?.mediaStream ?? null;
    // web3.8 V1: speaking from local levels. A meter's reading is trusted only while the context runs (a suspended one
    // reads silence); otherwise, and for every track without a meter, the SFU's flag is the fallback.
    const levelsLive = (): boolean => ctx !== null && ctx.state === 'running';
    let micSending: boolean | null = null;
    /** True unless push to talk is in force and the key is up. RNNoise's ptt worklet keeps the track "sending" with the
     *  key up, so speaking is gated on the key itself as well as on micSending. */
    let keyOpen = opts.mode !== 'ptt';
    const ownGate = (): boolean => micSending === true && keyOpen;
    /** Arms the sampler once a meter exists (set at connect); it idles with none. */
    let armSample: () => void = () => undefined;
    let ownMeter: LevelMeter | null = null;
    const ownDetector = new SpeakingDetector();
    let ownOn = false;
    let ownRms = 0;
    /** The remote person's speaking from their attached tracks' meters, or null when none has a live meter and a gain. */
    const remoteLocal = (identity: string): boolean | null => {
      if (!levelsLive() || gainFor(identity) <= 0) return null;
      let any: boolean | null = null;
      for (const entry of playing.values()) {
        if (entry.identity !== identity || entry.meter === null) continue;
        any = (any ?? false) || entry.on;
      }
      return any;
    };
    const tiles = (): PageCallTile[] => {
      const out: PageCallTile[] = [];
      const inRoom = new Set<string>();
      for (const p of remotes()) {
        inRoom.add(p.identity);
        const r = roster.get(p.identity);
        const tileState: TileState = r === undefined ? 'unverified' : verified.has(p.identity) ? 'ok' : 'not-heard';
        const show = tileState === 'ok' && !state.paused;
        const speaking = !state.paused && (remoteLocal(p.identity) ?? p.isSpeaking === true);
        out.push({ deviceId: p.identity, userId: r?.userId ?? '', state: tileState, speaking,
          video: show ? streamOf(p, SOURCE.camera) : null, screen: show ? streamOf(p, SOURCE.screen) : null,
          screenAudio: show && p.getTrackPublication(SOURCE.screenAudio) !== undefined, rttMs: rttOf(p.attributes) });
      }
      for (const [deviceId, r] of roster) {
        if (r.leaf === selfLeaf || deviceId === local.identity || inRoom.has(deviceId)) continue;
        out.push({ deviceId, userId: r.userId, state: 'connecting', speaking: false, video: null, screen: null, screenAudio: false, rttMs: null });
      }
      return out;
    };
    // The own mic counts only while it is sending (micSending === true: not muted, deafened, paused, or push to talk up).
    const ownLocal = (): boolean => ownMeter !== null && levelsLive();
    const level = (): number => {
      if (state.muted || state.paused) return 0;
      if (ownLocal()) return ownGate() ? Math.max(0, Math.min(12, Math.round(ownRms * 48))) : 0;
      return Math.max(0, Math.min(12, Math.round((local.audioLevel ?? 0) * 12)));
    };
    const ownSpeaking = (): boolean => {
      if (ownLocal()) return ownGate() && ownOn;
      return ownGate() && local.isSpeaking === true && !state.muted && !state.deafened && !state.paused;
    };
    let lastSetAt = Number.NEGATIVE_INFINITY;
    const refresh = (): void => {
      if (ended) return;
      lastSetAt = deps.now();
      set({ tiles: tiles(), level: level(), speaking: ownSpeaking() });
    };
    /** T5: one gain path. A microphone follows the person's voice volume, a screen's sound their stream volume; deafen
     *  and pause hold both at 0. */
    const gainFor = (identity: string, kind: 'voice' | 'stream' = 'voice'): number =>
      gainOf(roster.get(identity)?.userId ?? '', kind === 'voice' ? volumes : streamVolumes,
        { deafened: state.deafened, paused: state.paused, output: outputVolume });
    /** The gains of every remote person; first, the held-back tracks whose gain rose above 0 are attached (fix round 1,
     *  I3), so each new gain node takes its gain at once. */
    const applyGains = (): void => {
      attachPending();
      for (const p of remotes()) {
        p.setVolume(gainFor(p.identity, 'voice'), SOURCE.microphone);
        p.setVolume(gainFor(p.identity, 'stream'), SOURCE.screenAudio);
      }
    };
    afterRoster = () => { if (!ended) { applyGains(); refresh(); } };

    // web3.3: livekit plays a remote audio track only once it is attached. With webAudioMix, attach() is also what routes
    // it through the room’s AudioContext and its gain node: a setVolume before the attach has no gain node to act on (and
    // a 0 is not re-applied by the attach), so applyGains runs after every attach. One element per track, in the hidden
    // audio host; each is detached and removed on unsubscribe, on the participant’s leave and on every leave of the call.
    // Fix round 1 (I3): attach makes a gain node at 1, and a later setVolume(0) only ramps it down (a 100 ms time
    // constant), so a track whose gain is 0 (deafened, paused, or a volume of 0) is not attached at all: it waits in
    // `pending` until its gain rises (applyGains). Decryption and the tile’s verification are the receiver’s media
    // worker’s, so an unattached track still turns its tile ok.
    const audioHost = deps.audioHost ?? defaultAudioHost;
    const playing = new Map<RemoteAudio, {
      el: HTMLMediaElement; identity: string; meter: LevelMeter | null; detector: SpeakingDetector; on: boolean;
    }>();
    /** A held-back track's identity, and whether it is a microphone (web3.8 FX I1: only a microphone gets a meter). */
    const pending = new Map<RemoteAudio, { identity: string; mic: boolean }>();
    /** Takes a subscribed remote audio track in: it is attached by the next applyGains once its gain is above 0. A no-op
     *  for a track already attached, for video, and after the end. `pub` is its publication, which names its source. */
    const queueAudio = (track: unknown, identity: string, pub?: unknown): void => {
      const t = track as RemoteAudio | null | undefined;
      if (ended || t == null || t.kind !== 'audio' || playing.has(t)) return;
      const source = (pub as { source?: unknown } | null | undefined)?.source ?? t.source;
      pending.set(t, { identity, mic: source === SOURCE.microphone });
    };
    /** Fix round 2 (Minor 2): a device not in the roster yet has no known gain (its user, and so its volume, is unknown):
     *  it waits, and the roster change that names it (afterRoster → applyGains) attaches it. Nit 1: a track leaves
     *  `pending` only once it is attached; no host yet, or an attach that throws, keeps it for the next applyGains. */
    const attachPending = (): void => {
      for (const [t, { identity, mic }] of [...pending]) {
        if (!roster.has(identity) || gainFor(identity, mic ? 'voice' : 'stream') <= 0) continue;
        if (attachAudio(t, identity, mic)) pending.delete(t);
      }
      countAudio();
    };
    /** web3.4 V2: state.audio follows `pending` and `playing`, said only on a change. A queued track is counted by the
     *  applyGains that follows every queueAudio (onSubscribed, the sweep at connect). */
    const countAudio = (): void => {
      const attached = playing.size;
      const expected = attached + pending.size;
      if (state.audio.attached !== attached || state.audio.expected !== expected) set({ audio: { attached, expected } });
    };
    const attachAudio = (t: RemoteAudio, identity: string, mic: boolean): boolean => {
      if (playing.has(t)) return true;
      if (ended) return false;
      const host = audioHost();
      if (host === null) return false;
      let el: HTMLMediaElement;
      try { el = t.attach(); } catch { return false; }
      el.controls = false;
      el.tabIndex = -1;
      el.setAttribute('aria-hidden', 'true');
      host.appendChild(el);
      // web3.8 V1: the level is read from the element's own (decrypted, played) track; Chromium routes WebRTC audio to
      // WebAudio only through the element livekit attached. A failure to make a meter is the fallback, never an error.
      // web3.8 FX I1: only a microphone is metered. Screen-share audio is played but never counts as speaking, as the SFU's
      // flag (livekit-server sums only TrackSource_MICROPHONE) never counted it.
      let tap: MediaStreamTrack | undefined;
      if (mic) {
        try { tap = (el.srcObject as MediaStream | null)?.getAudioTracks()[0] ?? t.mediaStreamTrack; } catch { tap = t.mediaStreamTrack; }
      }
      playing.set(t, { el, identity, meter: mic ? makeMeter(tap) : null, detector: new SpeakingDetector(), on: false });
      armSample();
      return true;
    };
    const detachAudio = (track: unknown): void => {
      const t = track as RemoteAudio;
      pending.delete(t);
      const entry = playing.get(t);
      if (entry !== undefined) {
        playing.delete(t);
        entry.meter?.close();
        try { t.detach(entry.el); } catch { /* the element is removed all the same */ }
        entry.el.remove();
      }
      countAudio();
    };
    /** Every attached or held-back track, or those of one participant. */
    const detachAudioOf = (identity?: string): void => {
      for (const [t, held] of [...pending]) if (identity === undefined || held.identity === identity) pending.delete(t);
      for (const [t, entry] of [...playing]) if (identity === undefined || entry.identity === identity) detachAudio(t);
      countAudio();
    };
    /** Review Focus 1: tracks subscribed before the listeners existed (while joinCall or the publish was in flight). */
    const sweepAudio = (): void => {
      for (const p of remotes()) for (const pub of p.trackPublications?.values() ?? []) queueAudio(pub.track, p.identity, pub);
    };
    /** Fix round 1 (M2): call audio is held back while the session’s context is not running. Fix round 2 (Minor 3): the
     *  context alone decides when there is one (the room’s canPlaybackAudio lags it); the room’s flag only without one. */
    const playbackBlocked = (): boolean => (ctx !== null ? ctx.state !== 'running'
      : (room as unknown as { canPlaybackAudio?: boolean }).canPlaybackAudio === false);

    // ---- the microphone
    const gate = new VoiceGate({ mode: opts.mode === 'ptt' ? 'ptt' : 'open' });
    // Track mute/PTT enforcement is independent of the processor's internal gate.
    let mode: 'vad' | 'ptt' = opts.mode;
    let processorMode: 'vad' | 'ptt' = opts.mode;
    let micPub: LocalPub | null = null;
    let micTrack: MediaStreamTrack | null = null;
    let processor: NoiseProcessor | null = null;
    /** Signal task 10b: the input stage set on the microphone (around `processor`, or alone), or null. */
    let stage: InputStage | null = null;
    /** The capture the session holds: the raw one, which the stage (when set) was given; LiveKit's mediaStreamTrack is the
     *  processed output whenever a processor is set. */
    const rawOf = (track: NonNullable<LocalPub['track']>): MediaStreamTrack | undefined => stage?.input ?? track.mediaStreamTrack;
    /** The gate in force: voice activity only (push to talk is governed by its key). */
    const gateConfig = (): SensitivityConfig => ({ enabled: mode !== 'ptt', auto: sensitivity.auto, thresholdDb: sensitivity.thresholdDb });
    // Fix round 1 (L1): the stage starts closed, as the noise processor does; applyMic opens it.
    const stageOptions = (): InputStageOptions => ({ gain: inputVolume / 100, gate: gateConfig(), allowed: false });
    const reportStage = (): void => { if (!ended && !aborted) reportStageStatus(statusOwner, stage === null || stage.state === undefined || stage.state === 'pending' ? null : stage.state); };
    const dropStage = async (wrap: InputStage | null, made: NoiseProcessor | null): Promise<void> => {
      wrap?.setCaptureAllowed(false);
      made?.setCaptureAllowed?.(false);
      if (wrap?.destroy !== undefined) await wrap.destroy().catch(() => undefined);
      else await made?.destroy?.().catch(() => undefined);
    };
    /** Fix round 2: while noise suppression is switched under voice activity with a stage maker, the capture stays closed
     *  until the new stage holds it, so no ungated microphone goes out mid-switch (and a failed stage's fail-closed holds). */
    let holding = false;
    /** Echo cancellation and automatic gain changed while the microphone could not send and no stage holds it closed:
     *  the new capture waits for the next open (as R2's restart). */
    let processingWaiting = false;
    const reportStatus = (status: ProcessorStatus): void => { if (!ended && !aborted) reportProcessorStatus(statusOwner, status); };
    /** web3.8 MUTE (R2): a capture restart wanted while the microphone must not send waits here; the next open runs it.
     *  LiveKit puts the new capture on the sender before it disables it, so a restart while muted would send it briefly. */
    let restartWaiting: { reason: ProcessorStatus['reason'] } | null = null;
    /** web3.9 VOICE-FIX: the parked restart is a pre-acquire. LiveKit's restartTrack stops the old capture before it asks
     *  for the new one, which lost the first words after the open. Here the new capture is acquired first, guarded (it
     *  cannot be enabled while the microphone must not send, so a mute during the hand-over never puts an open capture on
     *  the sender), handed to the sender with replaceTrack, and the old capture, which kept sending until then, is stopped
     *  only after the replace resolved. False when nothing was handed over: the old capture stays, and nothing retries. */
    /** A capture's state read now (it changes across an await; a plain read would be narrowed). */
    const isEnded = (t: MediaStreamTrack): boolean => t.readyState === 'ended';
    /** Fix round 2 (N1): the session's own restartTrack calls in flight. One queued behind the hand-over's replace stops
     *  `next` itself and brings its own capture, so an ended `next` is then no reason to give the sender back. */
    let restarting = 0;
    const restartCapture = async (track: NonNullable<LocalPub['track']>, constraints: MediaTrackConstraints): Promise<void> => {
      restarting += 1;
      try { await track.restartTrack?.(constraints); } finally { restarting -= 1; }
    };
    const handOver = async (track: NonNullable<LocalPub['track']>, constraints: MediaTrackConstraints): Promise<boolean> => {
      if (track.replaceTrack === undefined) return false;
      const old = micTrack;
      let next: MediaStreamTrack | undefined;
      try { next = (await deps.media.getUserMedia({ audio: constraints })).getAudioTracks()[0]; } catch { return false; }
      if (next === undefined) return false;
      guardMic(next);
      next.enabled = false;
      // A leave meanwhile, or a capture that already failed (Safari can end it while the old one runs): the old one stays.
      if (ended || isEnded(next)) { next.stop(); return false; }
      try {
        await track.replaceTrack(next, true);
      } catch {
        // Not on the sender: the old capture stays. (One LiveKit already took is the session's from here on.)
        if (track.mediaStreamTrack !== next) { next.stop(); return false; }
      }
      // Fix round 1 (M1): a new capture that ended during the replace gives the sender back the one the session holds,
      // which is still live and still guarded (applyMic), so LiveKit's `enabled = !isMuted` stays fail-closed.
      const held = micTrack;
      if (isEnded(next) && restarting === 0 && held !== null && held !== next && !isEnded(held)) {
        try { await track.replaceTrack(held, true); } catch { /* the sender keeps the ended track; the session's stays held */ }
        next.stop();
        return false;
      }
      // Fix round 1 (I2): a restart that ran meanwhile (a noise toggle) swapped in its own capture, which LiveKit does not
      // stop for a user-provided track; it is stopped here with the old one, so no capture outlives the call.
      micTrack = next;
      for (const t of new Set([old, held])) if (t !== null && t !== next) t.stop();
      return true;
    };
    const browserStatus = async (reason: ProcessorStatus['reason'] = null, parked = false): Promise<void> => {
      if (!micTrack) return;
      let browserNoise = await setBrowserSuppression(micTrack, state.noise !== 'off', processing);
      const track = micPub?.track;
      const wanted = track?.restartTrack !== undefined && browserNoise !== 'unsupported' && browserNoise !== (state.noise === 'off' ? 'off' : 'on');
      restartWaiting = wanted && !sendNow() ? { reason } : null;
      if (track?.restartTrack && wanted && restartWaiting === null) {
        try {
          const constraints: MediaTrackConstraints = { ...micTrack.getConstraints(), deviceId:{exact:micTrack.getSettings().deviceId || opts.input || 'default'}, noiseSuppression:state.noise !== 'off', ...processingConstraints(processing), channelCount:1, sampleRate:48000 };
          let swapped = true;
          // Fix round 1 (I1): only where a second capture of the microphone stays live beside the first (an allowlist,
          // concurrentCapture); everywhere else, and with no answer, the stop-first restartTrack of web3.8.
          if (parked && track.replaceTrack !== undefined && deps.concurrentCapture === true) swapped = await handOver(track, constraints);
          else {
            await restartCapture(track, constraints);
            const raw = rawOf(track);
            if (raw) micTrack = raw;
          }
          // A failed hand-over keeps the old capture and reports what it observed, as a failed restart does.
          if (swapped) {
            if (ended) { micTrack.stop(); return; }
            applyMic();
            // web3.8 FX I3: the old capture is stopped, so the own meter moves to the new one (own speaking, AFK).
            restartOwnMeter();
            browserNoise = await setBrowserSuppression(micTrack, state.noise !== 'off', processing);
          }
        } catch { /* Report the observed constraint state if capture cannot be reopened. */ }
      }
      reportStatus({requested: noisePreference(state.noise), effective: state.noise === 'off' ? 'off' : 'browser', browserNoise, reason, vad: 'unavailable'});
    };
    /** Sets an RNNoise processor on the microphone in the mode in force; a failure goes on without it. */
    const startNoise = async (): Promise<void> => {
      const track = micPub?.track;
      if (!track?.setProcessor) { await browserStatus('initialization-failed'); return; }
      if (processor !== null) return;
      processorMode = mode;
      const made = (deps.createNoiseProcessor ?? ((m) => new DillaMicrophoneProcessor({ mode: m, preference: noisePreference(state.noise), allowed: false, onStatus: reportStatus, processing })))(processorMode);
      const wrap = deps.createInputStage?.(made, stageOptions()) ?? null;
      try {
        await track.setProcessor(wrap ?? made);
        if (ended) { await dropStage(wrap, made); return; }
        processor = made;
        stage = wrap;
        reportStage();
        const status = (made as {status?:ProcessorStatus}).status;
        if (status?.effective === 'browser' && status.browserNoise !== 'on' && track.restartTrack && micTrack) {
          await restartCapture(track, {...micTrack.getConstraints(),deviceId:{exact:micTrack.getSettings().deviceId || opts.input || 'default'},noiseSuppression:true,...processingConstraints(processing),channelCount:1,sampleRate:48000});
          // web3.8 MUTE: the restart captures a new track; the mute and push to talk govern that one from here on.
          const raw = rawOf(track);
          if (raw) micTrack = raw;
          if (ended) { await dropStage(stage, made); return; }
          applyMic();
        }
        // Settings and held keys may change while LiveKit initializes the processor.
        if (made.setMode) { made.setMode(mode); processorMode = mode; }
        if (processorMode === 'ptt') made.setPttActive(mode !== 'ptt' || gate.update(0, deps.now()));
      } catch {
        await dropStage(wrap, made);
        processor = null;
        stage = null;
        await browserStatus('initialization-failed');
        await startStage();
      }
    };
    /** Signal task 10b: the input stage on its own (noise suppression off, or its processor failed), so the input volume and
     *  the gate still apply. Without a stage maker, or when it cannot be set, the microphone goes on as before. */
    const startStage = async (): Promise<void> => {
      const track = micPub?.track;
      if (deps.createInputStage === undefined || !track?.setProcessor || stage !== null || processor !== null || ended) return;
      const wrap = deps.createInputStage(null, stageOptions());
      try {
        await track.setProcessor(wrap);
        if (ended) { await dropStage(wrap, null); return; }
        stage = wrap;
        reportStage();
        applyMic();
      } catch { stage = null; await dropStage(wrap, null); }
    };
    /**
     * Signal task 10b: the microphone captured again with the echo cancellation and automatic gain in force, through LiveKit's
     * restartTrack (the same publication and sender: the call goes on; the processors are restarted on the new capture). With
     * no stage to hold the new capture closed, a restart while the microphone must not send waits for the next open (R2).
     */
    const reacquire = async (): Promise<void> => {
      const track = micPub?.track;
      if (ended || !track?.restartTrack || micTrack === null) return;
      if (!sendNow() && stage === null) { processingWaiting = true; return; }
      const now = micTrack.getSettings?.() ?? {};
      const constraints: MediaTrackConstraints = { ...micTrack.getConstraints(), deviceId: { exact: now.deviceId || opts.input || 'default' },
        noiseSuppression: now.noiseSuppression ?? false, ...processingConstraints(processing), channelCount: 1, sampleRate: 48000 };
      try {
        await restartCapture(track, constraints);
        const raw = rawOf(track);
        if (raw) micTrack = raw;
        if (ended) { micTrack?.stop(); return; }
        applyMic();
        restartOwnMeter();
      } catch { /* the capture in force stays */ }
    };
    /** Push to talk's key, applied (the release delay's timer calls it with false). */
    let releaseTimer: unknown = null;
    const applyKey = (down: boolean): void => {
      gate.setPtt(down);
      if (processorMode === 'ptt') processor?.setPttActive(down);
      keyOpen = down;
      applyMic();
      if (!down) { ownDetector.reset(); ownOn = false; ownRms = 0; refresh(); } // no hold after the release
    };
    /** Whether the microphone may send now: not muted, deafened, paused or ended, and push to talk's key down. */
    const sendNow = (): boolean => !ended && !state.muted && !state.deafened && !state.paused && (mode !== 'ptt' || gate.update(0, deps.now()));
    /** web3.8 MUTE: LiveKit enables a track it is handed, and again after a capture restart (its LocalTrack sets
     *  `enabled = !isMuted`, and isMuted stays false until the session's mute() lands, which is asynchronous). The microphone
     *  track the session holds therefore refuses to be enabled while the session must not send: a call started muted (or in
     *  push to talk with the key up) sends no frame during its publish. Idempotent; a track that cannot be guarded is still
     *  closed by applyMic. */
    const guardMic = (t: MediaStreamTrack | null): void => {
      if (t === null || Object.prototype.hasOwnProperty.call(t, 'enabled')) return;
      let proto = Object.getPrototypeOf(t) as object | null;
      let native: { get?: (this: void) => unknown; set?: (this: void, v: boolean) => void } | undefined;
      while (proto !== null && native === undefined) {
        native = Object.getOwnPropertyDescriptor(proto, 'enabled');
        proto = Object.getPrototypeOf(proto) as object | null;
      }
      const read = native?.get;
      const write = native?.set;
      let plain = (t as { enabled?: boolean }).enabled !== false;
      try {
        Object.defineProperty(t, 'enabled', {
          configurable: true,
          get: () => (read !== undefined ? Boolean(Reflect.apply(read, t, [])) : plain),
          set: (v: boolean) => {
            const on = Boolean(v) && sendNow() && !holding;
            if (write !== undefined) Reflect.apply(write, t, [on]); else plain = on;
          },
        });
      } catch { /* not guardable: applyMic still closes it */ }
    };
    const applyMic = (): void => {
      guardMic(micTrack);
      const track = micPub?.track;
      // Close the track and processor together, including fallback and initialization transitions. The capture track is
      // written even before the publish returned (web3.8 MUTE: a press during the publish closes it at once).
      const send = sendNow();
      const open = send && !holding;
      if (micTrack) micTrack.enabled = open;
      if (!track) return;
      processor?.setCaptureAllowed?.(open);
      stage?.setCaptureAllowed(open);
      if (send && processingWaiting) { processingWaiting = false; void reacquire().catch(() => undefined); }
      // R2: a capture restart that waited while the microphone could not send runs now that it can.
      if (send && restartWaiting !== null && processor === null) {
        const waited = restartWaiting;
        restartWaiting = null;
        void browserStatus(waited.reason, true).catch(() => undefined);
      }
      if (send === micSending) return;
      micSending = send;
      void Promise.resolve(send ? track.unmute() : track.mute()).catch(() => undefined);
    };
    /** web3.8 V1: the own meter reads the track livekit publishes (the processed one, when a processor is set); it is made
     *  again whenever that track changes. No microphone, or a refusal, leaves the SFU's flag as the fallback. */
    const restartOwnMeter = (): void => {
      ownMeter?.close();
      ownMeter = null;
      ownDetector.reset();
      ownOn = false;
      ownRms = 0;
      if (!ended) ownMeter = makeMeter((micPub?.track as { mediaStreamTrack?: MediaStreamTrack } | undefined)?.mediaStreamTrack);
      armSample();
    };
    // web3.8 MUTE: the page pushes each press while the start runs (watchSelf), so a mute or deafen is in force at the press:
    // the capture track closes at once (guarded, so LiveKit cannot reopen it), and LiveKit's own mute follows once the
    // track is published; before that the state is recorded and the publish starts disabled. Only toward privacy.
    try {
      unwatch = opts.watchSelf?.(() => { if (!ended && !aborted && catchUp()) applyMic(); }) ?? unwatch;
    } catch { /* no push: the pulls before the publish, after it and at connected remain */ }
    if ('track' in mic) {
      micTrack = mic.track;
      // web3.8 MUTE: a mute or deafen pressed since the start read `self` (the microphone prompt, the connect) is in force
      // before the publish.
      catchUp();
      // Product walk M5: a start muted sends no frame before the mute lands. web3.8 MUTE: LiveKit enables the track it is
      // handed while it publishes, so the track is guarded first (see guardMic).
      guardMic(mic.track);
      if (state.muted || mode === 'ptt') mic.track.enabled = false;
      try {
        micPub = await local.publishTrack(mic.track, micOptions(caps));
      } catch {
        mic.track.stop();
        micTrack = null;
        state = { ...state, mic: 'failed', error: 'E_MIC_PUBLISH' };
      }
      // web3.8 MUTE: a press during the publish is read again, and the published track takes the state at once, LiveKit's
      // mute included (a later restartTrack enables the new capture track only when LiveKit's track is unmuted).
      catchUp();
      applyMic();
      if (micPub !== null) restartOwnMeter();
      checkAborted();
      if (opts.noise !== 'off') {
        await startNoise();
        restartOwnMeter();
        checkAborted();
      } else {
        await browserStatus();
        await startStage();
        if (stage !== null) restartOwnMeter();
        checkAborted();
      }
    } else {
      state = { ...state, mic: mic.mic };
    }
    applyMic();

    // ---- sharing: callShare on, then the SFU’s permission, then the publish; callShare off once nothing is shared
    let shareOn = false;
    /** The settle of a share waiting for the SFU's permission (WB client m3): a leave settles it with false. */
    let waitingShare: ((ok: boolean) => void) | null = null;
    let camera: { track: MediaStreamTrack } | null = null;
    /** Task 8c: the share options in force, or kept for the next start (the picker's Apply while not sharing). */
    let shareOpts: ShareOptions = DEFAULT_SHARE_OPTIONS;
    let screen: { video: MediaStreamTrack; audio: MediaStreamTrack | null; onEnded: () => void; onAudioEnded: () => void;
      /** Task 8c: the video's publication, whose sender takes a new encoding on Apply; its options. */
      pub: LocalPub | null; opts: ShareOptions;
      /** FX2: the audio input the person picked as the share's sound, while one is published. */
      device: { track: MediaStreamTrack; pub: LocalPub | null; onEnded: () => void } | null } | null = null;
    const permitted = (what: keyof typeof PERMISSION): boolean => {
      const p = local.permissions;
      if (!p || p.canPublish === false) return false;
      const list = p.canPublishSources ?? [];
      return list.length === 0 || list.includes(PERMISSION[what]);
    };
    const ensureShare = async (what: 'camera' | 'screen'): Promise<void> => {
      if (shareOn && permitted(what)) return;
      let settle: (ok: boolean) => void = () => undefined;
      const granted = new Promise<boolean>((resolve) => { settle = resolve; });
      waitingShare = settle;
      const onPermissions = (_prev: unknown, participant: unknown): void => {
        if (participant === room.localParticipant && permitted(what)) settle(true);
      };
      events.on(EV.permissions, onPermissions);
      const timer = later(() => settle(false), SHARE_WAIT_MS);
      try {
        shareOn = true;
        await deps.bridge.call({ m: 'callShare', on: true });
        if (permitted(what)) settle(true);
        if (!(await granted)) throw new PageCallError('E_UNAVAILABLE');
      } catch (err) {
        await shareOffIfIdle();
        throw pageCallError(err);
      } finally {
        events.off(EV.permissions, onPermissions);
        cancel(timer);
        if (waitingShare === settle) waitingShare = null;
      }
    };
    const shareOffIfIdle = async (): Promise<void> => {
      if (camera !== null || screen !== null || !shareOn) return;
      shareOn = false;
      await deps.bridge.call({ m: 'callShare', on: false }).catch(() => undefined);
    };
    const stopCamera = async (): Promise<void> => {
      const c = camera;
      if (c === null) return;
      camera = null;
      set({ camera: false, localCamera: null });
      await local.unpublishTrack(c.track).catch(() => undefined);
      c.track.stop();
    };
    const stopScreen = async (): Promise<void> => {
      const s = screen;
      if (s === null) return;
      screen = null;
      set({ sharing: false, localScreen: null, shareAudio: null, shareSource: null });
      s.video.removeEventListener('ended', s.onEnded);
      if (s.device !== null) {
        s.device.track.removeEventListener('ended', s.device.onEnded);
        await local.unpublishTrack(s.device.track).catch(() => undefined);
        s.device.track.stop();
      }
      s.audio?.removeEventListener('ended', s.onAudioEnded);
      await local.unpublishTrack(s.video).catch(() => undefined);
      if (s.audio) await local.unpublishTrack(s.audio).catch(() => undefined);
      s.video.stop();
      s.audio?.stop();
    };
    /** Camera and screen changes run one at a time. */
    let lane: Promise<unknown> = Promise.resolve();
    const inLane = <T>(fn: () => Promise<T>): Promise<T> => {
      const run = lane.then(fn, fn);
      lane = run.catch(() => undefined);
      return run;
    };
    const usable = (): void => { if (ended || state.paused) throw new PageCallError('E_UNAVAILABLE'); };

    const setCamera = (on: boolean): Promise<void> => inLane(async () => {
      if (!on) { await stopCamera(); await shareOffIfIdle(); return; }
      usable();
      if (camera !== null) return;
      await ensureShare('camera');
      let track: MediaStreamTrack;
      try {
        usable();
        track = await acquireCamera(deps.media);
      } catch (err) {
        await shareOffIfIdle();
        throw pageCallError(err);
      }
      try {
        usable();
        await local.publishTrack(track, { source: SOURCE.camera, videoCodec: chooseVideoCodec(room, caps) });
      } catch (err) {
        track.stop();
        await shareOffIfIdle();
        throw pageCallError(err);
      }
      // WB client I2: a pause (or the end) that came while the publish was in flight: nothing stays published.
      if (ended || state.paused) {
        await local.unpublishTrack(track).catch(() => undefined);
        track.stop();
        await shareOffIfIdle();
        throw new PageCallError('E_UNAVAILABLE');
      }
      camera = { track };
      set({ camera: true, localCamera: viewOf(track) });
    });

    const setSharing = (on: boolean, options?: ShareOptions): Promise<void> => inLane(async () => {
      if (!on) { await stopScreen(); await shareOffIfIdle(); return; }
      usable();
      if (screen !== null) return;
      await startScreen(options ?? shareOpts);
    });
    /** Captures and publishes a screen with `o` (in the lane, not sharing). The encoding goes with the publish, so the
     *  screen's sender gets the dilla transform as every sender does (attachSender); no option takes another path. */
    const startScreen = async (o: ShareOptions, captured?: ScreenCapture): Promise<void> => {
      shareOpts = o;
      const getDisplayMedia = deps.media.getDisplayMedia?.bind(deps.media);
      if (!getDisplayMedia && captured === undefined) throw new PageCallError('E_UNAVAILABLE');
      let tracks: ScreenCapture;
      if (captured !== undefined) {
        tracks = captured;
      } else {
        await ensureShare('screen');
        try {
          usable();
          tracks = await acquireScreen({ getDisplayMedia: getDisplayMedia! }, o);
        } catch (err) {
          await shareOffIfIdle();
          throw pageCallError(err);
        }
      }
      // #37: the sound the share carries, as published: none when the browser gave no audio or the grant did not allow it.
      let shareAudio: ShareAudioKind = 'none';
      let videoPub: LocalPub | null = null;
      try {
        usable();
        videoPub = await local.publishTrack(tracks.video, { source: SOURCE.screen, screenShareEncoding: shareEncoding(o, caps.maxShareBitrateBps) });
        if (tracks.audio !== null && permitted('screenAudio')) {
          await local.publishTrack(tracks.audio, screenAudioOptions(caps));
          shareAudio = tracks.audioKind;
        } else tracks.audio?.stop();
      } catch (err) {
        await local.unpublishTrack(tracks.video).catch(() => undefined);
        tracks.video.stop();
        tracks.audio?.stop();
        await shareOffIfIdle();
        throw pageCallError(err);
      }
      // WB client I2: a pause (or the end) that came while the publish was in flight: nothing stays published.
      if (ended || state.paused) {
        await local.unpublishTrack(tracks.video).catch(() => undefined);
        if (tracks.audio !== null) await local.unpublishTrack(tracks.audio).catch(() => undefined);
        tracks.video.stop();
        tracks.audio?.stop();
        await shareOffIfIdle();
        throw new PageCallError('E_UNAVAILABLE');
      }
      // The browser’s own "stop sharing" ends the video track.
      const onEnded = (): void => { void setSharing(false).catch(() => undefined); };
      tracks.video.addEventListener('ended', onEnded);
      // #37 m1: the share's own audio track ends while the video goes on (an audio device change, say): the share is silent
      // from then on, so the viewers' mark and the sharer's notice follow. The dead publication is withdrawn.
      const audioTrack = shareAudio !== 'none' ? tracks.audio : null;
      const onAudioEnded = (): void => {
        if (ended || screen === null || screen.audio !== audioTrack || audioTrack === null) return;
        audioTrack.removeEventListener('ended', onAudioEnded);
        set({ shareAudio: 'none', shareSource: null });
        void local.unpublishTrack(audioTrack).catch(() => undefined).then(() => { if (!ended) refresh(); });
      };
      audioTrack?.addEventListener('ended', onAudioEnded);
      screen = { video: tracks.video, audio: tracks.audio, onEnded, onAudioEnded, device: null, pub: videoPub, opts: o };
      set({ sharing: true, localScreen: viewOf(tracks.video), shareAudio, shareSource: shareAudio === 'none' ? null : 'browser' });
    };
    const setShareOptions = (o: ShareOptions): Promise<void> => inLane(async () => {
      const s = screen;
      if (s === null) { shareOpts = o; return; }
      usable();
      // A new source or sound choice is a new capture: the browser's picker again (the press is its activation). The new capture
      // is taken first, so a cancelled picker (or a refusal) leaves the running share as it was; then the old one is swapped
      // out and the new one published through the same publishTrack path.
      if (o.surface !== s.opts.surface || o.systemAudio !== s.opts.systemAudio) {
        const getDisplayMedia = deps.media.getDisplayMedia?.bind(deps.media);
        if (!getDisplayMedia) throw new PageCallError('E_UNAVAILABLE');
        let next: ScreenCapture;
        try {
          next = await acquireScreen({ getDisplayMedia }, o);
        } catch (err) {
          throw pageCallError(err);
        }
        if (ended || state.paused || screen !== s) {
          next.video.stop();
          next.audio?.stop();
          throw new PageCallError('E_UNAVAILABLE');
        }
        await stopScreen();
        try {
          await startScreen(o, next);
        } catch (err) {
          await shareOffIfIdle();
          throw err;
        }
        return;
      }
      shareOpts = o;
      s.opts = o;
      // A browser that cannot reshape a running capture keeps the frame it has; the encoding below still caps the rate.
      await s.video.applyConstraints?.(shareVideoConstraints(o, false)).catch(() => undefined);
      const sender = (s.pub?.track as { sender?: RTCRtpSender } | undefined)?.sender;
      if (sender === undefined) return;
      const params = sender.getParameters();
      const layers = params.encodings ?? [];
      if (layers.length === 0) return;
      // The top layer (the least scaled) takes the encoding; a simulcast layer under it keeps its bitrate and takes the rate cap.
      const top = layers.reduce((a, b) => ((b.scaleResolutionDownBy ?? 1) < (a.scaleResolutionDownBy ?? 1) ? b : a));
      const enc = shareEncoding(o, caps.maxShareBitrateBps);
      for (const layer of layers) {
        if (layer === top) { layer.maxBitrate = enc.maxBitrate; layer.maxFramerate = enc.maxFramerate; } else layer.maxFramerate = Math.min(layer.maxFramerate ?? enc.maxFramerate, enc.maxFramerate);
      }
      await sender.setParameters(params).catch(() => undefined);
    });

    // FX2: the share's sound from an audio input the person picked (a PipeWire/Pulse monitor, a virtual source).
    const clearDevice = async (s: NonNullable<typeof screen>): Promise<void> => {
      const d = s.device;
      if (d === null) return;
      s.device = null;
      d.track.removeEventListener('ended', d.onEnded);
      await local.unpublishTrack(d.track).catch(() => undefined);
      d.track.stop();
    };
    const setShareSource = (id: string | null): Promise<void> => inLane(async () => {
      usable();
      const s = screen;
      if (s === null || (s.audio !== null && state.shareSource === 'browser')) throw new PageCallError('E_UNAVAILABLE');
      if (id === null) {
        await clearDevice(s);
        set({ shareAudio: 'none', shareSource: null });
        return;
      }
      if (!permitted('screenAudio')) throw new PageCallError('E_FORBIDDEN');
      let track: MediaStreamTrack;
      try {
        const stream = await deps.media.getUserMedia({ audio: { deviceId: { exact: id }, echoCancellation: false, noiseSuppression: false,
          autoGainControl: false, channelCount: 2 } });
        const got = stream.getAudioTracks()[0];
        if (got === undefined) { for (const t of stream.getTracks()) t.stop(); throw new PageCallError('E_UNAVAILABLE'); }
        track = got;
      } catch (err) {
        throw pageCallError(err);
      }
      // M4: a device switch hands the new capture to the existing publication (replaceTrack), so there is never a second
      // sound track; without that (or when it refuses), the new one is published and the old one withdrawn after.
      const prior = s.device;
      let pub: LocalPub | null = null;
      let replaced = false;
      try {
        usable();
        if (screen !== s) throw new PageCallError('E_UNAVAILABLE');
        const priorTrack = prior?.pub?.track;
        if (prior !== null && prior.pub !== null && priorTrack?.replaceTrack !== undefined) {
          try { await priorTrack.replaceTrack(track, true); replaced = true; } catch {
            // N-N2: a throw after the sender took the new capture still counts as replaced (as the mic hand-over reads it).
            replaced = prior.pub.track?.mediaStreamTrack === track;
          }
          if (replaced) pub = prior.pub;
        }
        if (!replaced) pub = await local.publishTrack(track, screenAudioOptions(caps));
      } catch (err) {
        if (!replaced) await local.unpublishTrack(track).catch(() => undefined);
        track.stop();
        throw pageCallError(err);
      }
      if (ended || state.paused || screen !== s) {
        if (replaced && prior !== null) {
          // N-M2: the publication carries the new capture now, enabled by LiveKit; stopping it at once makes it send silence,
          // and the share's own stop (the pause or the end queues one) still finds the publication by it and withdraws it.
          prior.track.removeEventListener('ended', prior.onEnded);
          prior.track.stop();
          track.stop();
          s.device = screen === s ? { track, pub, onEnded: () => undefined } : null;
        } else {
          await local.unpublishTrack(track).catch(() => undefined);
          track.stop();
        }
        throw new PageCallError('E_UNAVAILABLE');
      }
      if (replaced && prior !== null) {
        s.device = null;
        prior.track.removeEventListener('ended', prior.onEnded);
        prior.track.stop();
      } else await clearDevice(s);
      const onEnded = (): void => {
        if (ended || screen !== s || s.device?.track !== track) return;
        track.removeEventListener('ended', onEnded);
        s.device = null;
        set({ shareAudio: 'none', shareSource: null });
        void local.unpublishTrack(track).catch(() => undefined).then(() => { if (!ended) refresh(); });
      };
      track.addEventListener('ended', onEnded);
      s.device = { track, pub, onEnded };
      set({ shareAudio: 'device', shareSource: id });
    });
    const shareSources = async (): Promise<{ sources: ShareSource[]; labeled: boolean }> => {
      let all: MediaDeviceInfo[] = [];
      try { all = await deps.media.enumerateDevices(); } catch { all = []; }
      const raw = all.filter((d) => d.kind === 'audioinput');
      const settings = micTrack?.getSettings?.();
      const own = settings?.deviceId;
      // I1: with the default input the track says 'default' and its real entry has another id but the same group. A monitor
      // can share the card's group, so only the entries that are not monitors go with the microphone.
      const isMonitor = (label: string): boolean => /monitor/i.test(label);
      const isAlias = (id: string | undefined): boolean => id === 'default' || id === 'communications';
      // N-M1: a real id hides only itself. Behind an alias the entry whose label is the alias's without its "Default - " /
      // "Communications - " prefix goes; only when none matches, the non-monitor entries of the mic's group go.
      const group = settings?.groupId ?? raw.find((d) => d.deviceId === own)?.groupId;
      const aliasLabel = isAlias(own) ? (raw.find((d) => d.deviceId === own)?.label ?? '').replace(/^(Default|Communications) - /, '') : '';
      const byLabel = aliasLabel === '' ? [] : raw.filter((d) => !isAlias(d.deviceId) && d.deviceId !== '' && d.label === aliasLabel);
      const byGroup = (d: MediaDeviceInfo): boolean => isAlias(own) && byLabel.length === 0 && group !== undefined && group !== ''
        && d.groupId === group && !isMonitor(d.label);
      const inputs = raw.filter((d) => !isAlias(d.deviceId) && d.deviceId !== '' && d.deviceId !== own && !byLabel.includes(d) && !byGroup(d));
      // M1: Chromium without permission lists one input with an empty id and label; labels are judged before ids are filtered.
      return { labeled: raw.every((d) => d.label !== ''),
        sources: inputs.map((d) => ({ id: d.deviceId, label: d.label, monitor: isMonitor(d.label) && !/dilla-share/i.test(d.label) })) };
    };

    // ---- self flags, sent only after callJoin (the PageCall exists only once it answered)
    const sendSelf = (): void => {
      const flags = (state.muted ? 1 : 0) | (state.deafened ? 2 : 0);
      void deps.bridge.call({ m: 'voiceSelf', callId: callIdHex, flags }).catch(() => undefined);
    };

    // ---- timers: tiles, stats, ICE
    const poll = (): void => {
      if (ended) return;
      void manager.verifiedIdentities().then((v) => { verified = v; }, () => undefined).finally(() => {
        if (ended) return;
        refresh();
        later(poll, TILE_POLL_MS);
      });
    };
    let totals: MediaTotals = { sent: 0, failed: 0 };
    let statsBusy = false;
    const reportStats = async (): Promise<void> => {
      if (statsBusy) return;
      statsBusy = true;
      const started = deps.now();
      try {
      const publisher = (room.engine as unknown as { pcManager?: { publisher?: { getStats?: () => Promise<RTCStatsReport> | undefined } } } | undefined)?.pcManager?.publisher;
      const [rtc, media] = await Promise.all([
        Promise.resolve(publisher?.getStats?.()).catch(() => undefined),
        manager.stats().catch(() => null),
      ]);
      if (ended || state.phase !== 'connected' || deps.now() - started >= STATS_EVERY_MS) return;
      const next = callStatsReport(connectionOf(rtc), mediaTotals(media), totals);
      totals = next.totals;
      await deps.bridge.call({ m: 'callStats', report: next.report });
      } finally { statsBusy = false; }
    };
    const statsTick = (): void => {
      if (ended) return;
      if (state.phase === 'connected') void reportStats().catch(() => undefined); // a 429 or a 404 is not the person’s problem
      later(statsTick, STATS_EVERY_MS);
    };
    // MP5: the dock’s own link read, every LINK_EVERY_MS while connected; it never goes to the server.
    let linkTotals: LinkTotals | null = null;
    let linkAt = 0;
    let linkBusy = false;
    const carried = { up: false, down: false };
    let linkStartedAt = 0;
    const sameLink = (a: LinkSample | undefined, b: LinkSample | undefined): boolean =>
      a === b || (a !== undefined && b !== undefined && a.rttMs === b.rttMs && a.upKbps === b.upKbps && a.downKbps === b.downKbps && a.lossPct === b.lossPct);
    const readLink = async (): Promise<void> => {
      const pcs = (room.engine as unknown as { pcManager?: Record<string, { getStats?: () => Promise<RTCStatsReport> | undefined } | undefined> } | undefined)?.pcManager;
      // No subscriber connection (LiveKit's default single-peer-connection mode never makes one) means nothing to read there:
      // an empty report, so down is 0. Only a getStats that rejects is "no read" (undefined), which the dock shows as a dash.
      const getSub = pcs?.subscriber?.getStats;
      const [pub, sub] = await Promise.all([
        Promise.resolve(pcs?.publisher?.getStats?.()).catch(() => undefined),
        getSub === undefined ? EMPTY_REPORT : Promise.resolve(pcs?.subscriber?.getStats?.()).catch(() => undefined),
      ]);
      if (ended || state.phase !== 'connected' || state.paused) return;
      const at = deps.now();
      const next = linkSample(linkTotals, pub, sub, at - linkAt);
      linkTotals = next.totals;
      linkAt = at;
      // No sample (no RTT on the selected pair, or nothing readable) clears the link: the dock says "— ms", not the last numbers.
      // A direction that could not be read keeps its previous value for one tick, then is null.
      let link = next.sample ?? undefined;
      if (link !== undefined) {
        const prev = state.link;
        const keep = (now: number | null, was: number | null | undefined, side: 'up' | 'down'): number | null => {
          if (now !== null) { carried[side] = false; return now; }
          if (was == null || carried[side]) return null;
          carried[side] = true;
          return was;
        };
        link = { ...link, upKbps: keep(link.upKbps, prev?.upKbps, 'up'), downKbps: keep(link.downKbps, prev?.downKbps, 'down') };
      } else { carried.up = false; carried.down = false; }
      if (!sameLink(link, state.link)) set({ link });
      if (link !== undefined) publishRtt(link.rttMs);
    };
    // Task 8c: this device's RTT to the SFU, so the others' tiles can show it (the prototype's per-person ping). Read with the
    // link (every LINK_EVERY_MS) and reported to the instance (callRtt → POST /v1/calls/{id}/rtt), which writes it as this
    // device's `dilla.rtt` attribute: no call token may write its own attributes (fix round 2, security review). Sent only when
    // it moved by RTT_STEP_MS or more, one report in flight at most; cleared (null) while paused or reconnecting. A refused
    // report (the worker answers false: 429 or 404) is not counted as sent, so the next sample tries again, and a reconnect
    // forgets what was sent, so a fast one that no tick saw still re-sends (Task 11). It says nothing the SFU does not already
    // measure.
    let rttSent: number | null = null;
    let rttBusy = false;
    const reportRtt = (ms: number | null): void => {
      if (ended || rttBusy) return;
      rttBusy = true;
      void deps.bridge.call({ m: 'callRtt', ms }).then(stored => { if (stored !== false) rttSent = ms; }, () => undefined)
        .finally(() => { rttBusy = false; });
    };
    const publishRtt = (rtt: number): void => {
      const ms = Math.max(0, Math.min(5000, Math.round(rtt)));
      if (rttSent !== null && Math.abs(ms - rttSent) < RTT_STEP_MS) return;
      reportRtt(ms);
    };
    // Not connected, paused or held: no read, no stats; the first read after is a fresh baseline. A getStats that does not
    // settle within LINK_STALL_MS takes the stats away (the dock falls back to its no-sample text) and no second read starts meanwhile.
    const linkTick = (): void => {
      if (ended) return;
      if (state.phase !== 'connected' || state.paused) {
        linkTotals = null;
        if (state.link !== undefined) set({ link: undefined });
        if (rttSent !== null && state.phase !== 'ended') reportRtt(null);
      } else if (!linkBusy) {
        linkBusy = true;
        linkStartedAt = deps.now();
        void readLink().catch(() => undefined).finally(() => { linkBusy = false; });
      } else if (deps.now() - linkStartedAt >= LINK_STALL_MS && state.link !== undefined) {
        set({ link: undefined });
      }
      later(linkTick, LINK_EVERY_MS);
    };
    const scheduleIce = (list: IceServerTuple[], retry = false): void => {
      if (ended) return;
      const expiry = iceExpiryMs(list);
      if (expiry === null && !retry) return;
      const delay = retry || expiry === null ? ICE_RETRY_MS : Math.max(ICE_RETRY_MS, expiry - ICE_EARLY_MS - deps.now());
      later(() => { void refreshIce(list); }, delay);
    };
    const refreshIce = async (current: IceServerTuple[]): Promise<void> => {
      try {
        const answer = (await deps.bridge.call({ m: 'callRefresh' })) as { token: string; iceServers: IceServerTuple[] };
        if (ended) return;
        refreshIceServers(room, answer.iceServers);
        scheduleIce(answer.iceServers);
      } catch {
        scheduleIce(current, true);
      }
    };

    // ---- room events
    const onPeople = (): void => { applyGains(); refresh(); };
    const onSpeakers = (): void => refresh();
    // WB client I3: a room that ends on its own says why (state.error) and tells the worker it was lost.
    onRoomReconnecting = () => { if (!ended) { linkTotals = null; set({ phase: 'reconnecting', link: undefined }); } };
    onRoomReconnected = () => { if (!ended) { rttSent = null; set({ phase: 'connected' }); } };
    onRoomEnd = (reason) => {
      if (ended) return;
      set({ error: endCodeOf(reason) });
      void leave('lost');
    };
    const onSubscribed = (track: unknown, pub: unknown, participant: unknown): void => {
      if (ended) return;
      queueAudio(track, (participant as RemoteView | undefined)?.identity ?? '', pub);
      onPeople(); // applyGains attaches it (unless its gain is 0), then the new gain node takes the person’s volume
    };
    const onUnsubscribed = (track: unknown): void => { detachAudio(track); if (!ended) onPeople(); };
    const onLeft = (participant: unknown): void => {
      const identity = (participant as RemoteView | undefined)?.identity;
      if (typeof identity === 'string') detachAudioOf(identity);
      if (!ended) onPeople();
    };
    const onPlayback = (): void => { if (!ended) set({ audioBlocked: playbackBlocked() }); };
    // #37: a publication not subscribed yet (or gone) still turns a tile's screenAudio mark at once. Nothing is attached here.
    const onPublications = (): void => refresh();
    const roomListeners: Array<[string, AnyListener]> = [
      [EV.participantConnected, onPeople], [EV.participantDisconnected, onLeft], [EV.trackSubscribed, onSubscribed],
      [EV.trackUnsubscribed, onUnsubscribed], [EV.activeSpeakers, onSpeakers], [EV.audioPlayback, onPlayback],
      [EV.trackPublished, onPublications], [EV.trackUnpublished, onPublications], [EV.attributes, onPublications],
    ];
    for (const [e, f] of roomListeners) events.on(e, f);
    ctx?.addEventListener('statechange', onPlayback);
    const policy = new SpeakerPolicy(room);
    // Review M3: what a failed start (abandon) or a leave undoes of the room: its listeners, the policy, the audio.
    teardownRoom = () => {
      for (const [e, f] of roomListeners) events.off(e, f);
      ctx?.removeEventListener('statechange', onPlayback);
      detachAudioOf();
      try { policy.dispose(); } catch { /* the room is going away */ }
    };
    onPageHide = () => { void leave('pagehide'); };

    // ---- leave
    let leaving: Promise<void> | null = null;
    const stopEverything = (): void => {
      ended = true;
      processor?.setCaptureAllowed?.(false);
      processor?.flush?.();
      void processor?.destroy?.().catch(() => undefined);
      reportProcessorStatus(statusOwner, null); reportStageStatus(statusOwner, null);
      relay.close();
      unsubscribe();
      for (const h of [...timers]) cancel(h);
      teardownRoom();
      offMisroute();
      detachEarly();
      win?.removeEventListener('pagehide', pageHide);
      opts.signal?.removeEventListener('abort', onAbort);
      waitingShare?.(false);
      closeCtx();
    };
    const stopCaptures = (): void => {
      micTrack?.stop();
      camera?.track.stop();
      screen?.video.stop();
      screen?.audio?.stop();
      screen?.device?.track.stop();
      camera = null;
      screen = null;
    };
    const leave = (reason: LeaveReason): Promise<void> => {
      if (leaving !== null) return leaving;
      stopEverything();
      const post = (r: 'user' | 'pagehide' | 'switch' | 'lost') => deps.bridge.call({ m: 'callLeave', reason: r }).then(() => undefined, () => undefined);
      const releaseAll = () => release().catch(() => undefined).finally(stopCaptures);
      if (reason === 'pagehide') {
        // The page is going away: nothing here may wait. callLeave is posted, then media stops in this same task.
        void post('pagehide');
        try { manager.dispose(); } catch { /* released below */ }
        void Promise.resolve(room.disconnect()).catch(() => undefined);
        leaving = releaseAll();
      } else if (reason === 'blocked' || reason === 'removed' || reason === 'lost') {
        // Keys must stop before anything else: media first, then the worker’s leave.
        leaving = releaseAll().then(() => post(reason === 'lost' ? 'lost' : 'user'));
      } else {
        const posted = post(reason);
        leaving = releaseAll().then(() => posted);
      }
      set({ phase: 'ended', camera: false, sharing: false, tiles: [], level: 0, speaking: false, localCamera: null, localScreen: null,
        shareAudio: null, shareSource: null, link: undefined });
      return leaving;
    };

    // ---- web3.8 V1: the sampler. One timer reads every meter each SPEAK_TICK_MS and feeds its detector. A speaking flip
    // (own or remote) reaches the state at once; a level change only every LEVEL_PUBLISH_MS. Nothing here leaves the page.
    let sampler: unknown = null;
    let sampling = false;
    const hasMeter = (): boolean => ownMeter !== null || [...playing.values()].some((e) => e.meter !== null);
    const sample = (): void => {
      sampler = null;
      if (ended) return;
      const now = deps.now();
      let flipped = false;
      if (ownMeter !== null && levelsLive()) {
        if (ownGate()) {
          ownRms = ownMeter.read();
          const on = ownDetector.update(ownRms, now);
          if (on !== ownOn) flipped = true;
          ownOn = on;
        } else {
          if (ownOn) flipped = true;
          ownRms = 0;
          ownDetector.reset();
          ownOn = false;
        }
      }
      if (levelsLive()) {
        for (const entry of playing.values()) {
          if (entry.meter === null) continue;
          const on = gainFor(entry.identity) > 0 && !state.paused ? entry.detector.update(entry.meter.read(), now) : false;
          if (on !== entry.on) flipped = true;
          entry.on = on;
          if (!on && (gainFor(entry.identity) <= 0 || state.paused)) entry.detector.reset();
        }
      }
      if (flipped || (level() !== state.level && now - lastSetAt >= LEVEL_PUBLISH_MS)) refresh();
      armSample();
    };
    // The tick idles while no meter exists (a call with no meter on the SFU's flags) and is armed again by the next meter.
    armSample = () => {
      if (!sampling || sampler !== null || ended || !hasMeter()) return;
      sampler = later(sample, SPEAK_TICK_MS);
    };

    // ---- connected (WB client m2: only while the room is still up)
    checkAborted();
    if ((room as unknown as { state?: string }).state === 'disconnected') throw new PageCallError('E_DISCONNECTED');
    opts.signal?.removeEventListener('abort', onAbort);
    // web3.8 MUTE: a mute or deafen pressed while the microphone was set up (the noise processor, the room) is in force
    // before the call counts as connected; the page's attach applies anything later.
    if (catchUp()) applyMic();
    // From here the session is the page's: its own setMuted and setDeafened (and the attach reconcile) take the presses.
    unwatch();
    unwatch = () => undefined;
    sweepAudio();
    applyGains();
    state = { ...state, phase: 'connected', tiles: tiles(), audioBlocked: playbackBlocked() };
    // Product walk M5: a call started muted or deafened says so to the others at once.
    if (state.muted || state.deafened) sendSelf();
    later(poll, TILE_POLL_MS);
    sampling = true;
    armSample();
    later(statsTick, STATS_EVERY_MS);
    linkAt = deps.now();
    later(linkTick, LINK_EVERY_MS);
    scheduleIce(firstIce);

    return {
      get state() { return state; },
      subscribe(fn) {
        const entry = { fn };
        listeners.add(entry);
        return () => { listeners.delete(entry); };
      },
      setMuted(on) {
        if (ended) return;
        const patch: Partial<PageCallState> = { muted: on };
        if (!on && state.deafened) patch.deafened = false;
        // With a local meter the own mic starts silent after an unmute (the detector needs its 50 ms again); without one
        // the SFU's flag is the fallback.
        patch.speaking = ownLocal() ? false : local.isSpeaking === true && !on && !state.paused;
        if (on) { ownDetector.reset(); ownOn = false; ownRms = 0; }
        set(patch);
        applyMic();
        applyGains();
        sendSelf();
      },
      setDeafened(on) {
        if (ended || on === state.deafened) return;
        if (on) {
          mutedBeforeDeafen = state.muted;
          ownDetector.reset(); ownOn = false; ownRms = 0;
          set({ deafened: true, muted: true, speaking: false });
        } else {
          set({ deafened: false, muted: mutedBeforeDeafen });
        }
        applyMic();
        applyGains();
        sendSelf();
      },
      setCamera,
      setSharing,
      setShareOptions,
      tileStats: () => readTileStats({ ended: () => ended, micPub: () => micPub, screenPub: () => screen?.pub ?? null,
        shareAudio: () => state.shareAudio !== null && state.shareAudio !== 'none', remotes, screenSource: SOURCE.screen }),
      shareSources,
      setShareSource,
      setVolume(userId, volume, source = 'voice') {
        if (!isVolume(volume)) throw new PageCallError('E_INVALID_REQUEST');
        (source === 'stream' ? streamVolumes : volumes)[userId] = volume;
        if (!ended) applyGains();
      },
      async setPaused(on) {
        if (ended || on === state.paused) return;
        // WB client I2: a camera or screen goes quiet at once, before any queued camera or share operation (the lane may
        // be held by the screen picker or a permission prompt); the lane then unpublishes and stops them.
        if (on) for (const track of [camera?.track, screen?.video, screen?.audio, screen?.device?.track]) if (track) track.enabled = false;
        relay.setPaused(on);
        set({ paused: on });
        applyMic();
        applyGains();
        refresh();
        if (on) {
          // RV-3: nothing is sent while paused; camera and screen stay off after the pause until turned on again.
          await inLane(async () => {
            await stopCamera();
            await stopScreen();
            await shareOffIfIdle();
          });
        }
      },
      dropKeys(upTo) {
        if (ended) return;
        // Re-review N-m1: the hold names the epoch whose key it drops; a newer epoch's key that arrived first (it ends the
        // hold) is kept, so the drop never silences the call it was meant to resume.
        const newest = relay.newestEpoch();
        if (upTo !== undefined && newest !== null && newest > upTo) return;
        relay.dropHeld();
        try { manager.clearKeys(); } catch { /* a failed or disposed manager holds no keys */ }
      },
      pushToTalk(down) {
        if (ended || mode !== 'ptt') return;
        if (releaseTimer !== null) { cancel(releaseTimer); releaseTimer = null; }
        // Signal task 10b: the release delay keeps the key open a moment after it is let go, so word endings are sent.
        if (!down && keyOpen && pttReleaseMs > 0) {
          releaseTimer = later(() => { releaseTimer = null; if (!ended && mode === 'ptt') applyKey(false); }, pttReleaseMs);
          return;
        }
        applyKey(down);
      },
      setMode(next) {
        if (ended || next === mode) return;
        if (releaseTimer !== null) { cancel(releaseTimer); releaseTimer = null; }
        mode = next;
        stage?.setGate(gateConfig());
        gate.setMode(next === 'ptt' ? 'ptt' : 'open');
        gate.setPtt(false);
        // A processor built for push to talk is held open under voice activity and closed again for push to talk.
        if (processor?.setMode) { processor.setMode(next); processorMode = next; }
        else if (processorMode === 'ptt') processor?.setPttActive(next !== 'ptt');
        keyOpen = next !== 'ptt';
        set({ mode: next });
        applyMic();
        if (!keyOpen) { ownDetector.reset(); ownOn = false; ownRms = 0; refresh(); }
      },
      // Fix round 3 (m2): one switch at a time (the camera and screen lane), so a quick on-then-off never leaves a
      // processor set after the setting says off.
      setNoise: (next) => inLane(async () => {
        if (ended || next === state.noise) return;
        set({ noise: next });
        const track = micPub?.track;
        processor?.setCaptureAllowed?.(false);
        processor?.flush?.();
        stage?.setCaptureAllowed(false);
        holding = deps.createInputStage !== undefined && mode !== 'ptt';
        try {
          if (processor !== null || stage !== null) {
            processor = null;
            stage = null;
            reportStage();
            applyMic();
            await Promise.resolve(track?.stopProcessor?.()).catch(() => undefined);
          }
          if (next !== 'off') await startNoise();
          else { if (track?.mediaStreamTrack) micTrack = track.mediaStreamTrack; await browserStatus(); await startStage(); }
        } finally { holding = false; }
        restartOwnMeter(); // the processor or a restarted capture swaps the published track
        applyMic();
      }),
      setInputVolume(percent) {
        if (ended || !isPercent(percent)) return;
        inputVolume = percent;
        stage?.setGain(percent / 100);
      },
      setOutputVolume(percent) {
        if (ended || !isPercent(percent)) return;
        outputVolume = percent;
        applyGains();
      },
      setSensitivity(next) {
        if (ended) return;
        sensitivity = { auto: next.auto, thresholdDb: dbThreshold(next.thresholdDb) };
        stage?.setGate(gateConfig());
      },
      setPttRelease(ms) {
        pttReleaseMs = releaseMs(ms);
      },
      setProcessing: (next) => inLane(async () => {
        if (ended || (next.echo === processing.echo && next.agc === processing.agc)) return;
        processing = { echo: next.echo, agc: next.agc };
        processor?.setProcessing?.(processing);
        await reacquire();
      }),
      // Fix round 1 (M2): both run synchronously inside the "turn on audio" press (the dock’s button, the user activation
      // the browser asks for); nothing is awaited before them. A refusal leaves the line on offer. (The context itself is
      // made in the join's press, by useCall; see PageCallOptions.audioContext.)
      startAudio() {
        if (ended) return Promise.resolve();
        const tries: unknown[] = [];
        try { if (ctx !== null) tries.push(ctx.resume()); } catch { /* still held back */ }
        // Merge of Task 10b: the microphone's input stage runs in a context of its own; the same press lets it run.
        try { const c = stage?.context; if (c !== undefined && c.state === 'suspended') tries.push(c.resume()); } catch { /* still held back */ }
        try { tries.push(room.startAudio()); } catch { /* still held back */ }
        return Promise.allSettled(tries).then(() => { if (!ended) set({ audioBlocked: playbackBlocked() }); });
      },
      // web3.4 V1: Settings → Voice's output during the call. With webAudioMix everything plays through the context's
      // destination, so the context's sink is the one to move. A refusal rejects (the shell says so); the call goes on
      // where it played before.
      setOutput(id) {
        if (ended || setSink === null) return Promise.resolve();
        return setSink(id);
      },
      leave,
    };
  } catch (err) {
    unwatch();
    // Anything that fails after joinCall resolved tears the call down: no install, media released, then callLeave.
    throw await abandon(err, true);
  }
}
