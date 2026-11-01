import { createElement, type FunctionComponent } from 'react';
import { vi } from 'vitest';
import { act, render } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import type { CallKeys, CallState, ChannelSummary, Command, CoreClient, DmSummary, MemberSummary, Ring, SliceName, SliceTypes,
  SendBlocksState, TrustSummary, VoiceState } from '@dilla/client-core';
import { CALL_IDLE } from '@dilla/client-core';
import type { PageCall, PageCallState, ShareOptions, TileStats } from '../call/session.ts';
import type { UiError } from '../core/errors.ts';
import { CoreProvider } from '../core/context.tsx';
import { Shell } from '../screens/Shell.tsx';
import { VoiceDepsContext, type VoiceDeps, type VoiceModule } from '../screens/voice/useCall.tsx';
import { replaceHistoryState } from '../router.ts';
import { account, ME } from './fixtures.ts';

/** What a refused call rejects with: the four fields of the worker's error, as the bridge's CoreCallError carries them. */
export class FakeCallError extends Error implements UiError {
  readonly code: string;
  readonly detail: string;
  readonly status: number;
  readonly retryAfterMs: number | null;
  constructor(code: string, detail: string, status: number, retryAfterMs: number | null) {
    super(`${code}: ${detail}`);
    this.name = 'FakeCallError';
    this.code = code;
    this.detail = detail;
    this.status = status;
    this.retryAfterMs = retryAfterMs;
  }
}

/** A refusal to script with `Promise.reject(refusal({ … }))`; status 0 and no wait unless given. */
export function refusal(init: { code: string; detail?: string; status?: number; retryAfterMs?: number | null }): FakeCallError {
  return new FakeCallError(init.code, init.detail ?? '', init.status ?? 0, init.retryAfterMs ?? null);
}

/** An in-memory CoreClient: tests set slices and script the answers of calls. */
export class FakeClient implements CoreClient {
  readonly calls: Command[] = [];
  handler: (command: Command) => Promise<unknown> = command => Promise.resolve(command.m === 'getAFKSettings'
    ? { channelId: null, timeoutSeconds: 300 } : null);
  private readonly values = new Map<string, unknown>();
  private readonly listeners = new Map<string, Set<() => void>>();

  call<C extends Command>(command: C): Promise<unknown> {
    this.calls.push(command);
    return this.handler(command);
  }
  get<N extends SliceName>(name: N): SliceTypes[N] | undefined {
    return this.values.get(name) as SliceTypes[N] | undefined;
  }
  subscribe(name: SliceName, listener: () => void): () => void {
    const set = this.listeners.get(name) ?? new Set<() => void>();
    this.listeners.set(name, set);
    set.add(listener);
    return () => { set.delete(listener); };
  }
  /** L-TS-52, as the real bridge answers it: each listener synchronously, then the key buffer zero-filled. */
  private readonly keyListeners = new Set<{ listener: (callId: string, keys: CallKeys) => void }>();
  onCallKeys(listener: (callId: string, keys: CallKeys) => void): () => void {
    const entry = { listener };
    this.keyListeners.add(entry);
    return () => { this.keyListeners.delete(entry); };
  }
  pushCallKeys(callId: string, keys: CallKeys): void {
    try {
      for (const entry of [...this.keyListeners]) {
        try { entry.listener(callId, keys); }
        catch (err) { queueMicrotask(() => { throw err; }); }
      }
    } finally {
      new Uint8Array(keys.baseKey).fill(0);
    }
  }
  dispose(): void {}
  /** set inside act: what a worker push looks like to a rendered tree. */
  publish<N extends SliceName>(name: N, value: SliceTypes[N]): void {
    act(() => { this.set(name, value); });
  }
  set<N extends SliceName>(name: N, value: SliceTypes[N]): void {
    this.values.set(name, value);
    for (const l of [...(this.listeners.get(name) ?? [])]) l();
  }
  callsOf<M extends Command['m']>(m: M): Extract<Command, { m: M }>[] {
    return this.calls.filter((c): c is Extract<Command, { m: M }> => c.m === m);
  }
}

// The shell's voice doubles (web-3 task 9): a fake call module in place of the lazy call chunk, a fake PageCall that
// records what the shell asks of it, fake sounds and a fake Notification. The worker behaviour comes through FakeClient.
export const A = 'a1'.repeat(16);
export const GENV = 'e5'.repeat(16);
export const MUSIC = 'e6'.repeat(16);
export const TEXT = 'c3'.repeat(16);
export const DM = '9a'.repeat(16);
export const BOB = '28'.repeat(16);
export const BOB_DEV = '29'.repeat(16);
export const CAROL = '3c'.repeat(16);
export const CAROL_DEV = '3d'.repeat(16);
export const OWN_DEV = 'cc'.repeat(16);
export const CALL_ID = 'f0'.repeat(16);

export const CHANNELS: ChannelSummary[] = [
  { id: TEXT, communityId: A, kind: 0, mode: 0, name: 'lobby', topic: '', parentId: null, position: 0, slowmode: 0, announcement: false, group: 'active' },
  { id: GENV, communityId: A, kind: 1, mode: 0, name: 'general', topic: '', parentId: null, position: 1, slowmode: 0, announcement: false, group: 'unsupported' },
  { id: MUSIC, communityId: A, kind: 1, mode: 0, name: 'music', topic: '', parentId: null, position: 2, slowmode: 0, announcement: false, group: 'unsupported' },
];
export const MEMBERS: MemberSummary[] = [
  { userId: ME.id, username: 'ada', display: '', kind: 0, roleIds: [] },
  { userId: BOB, username: 'bob', display: '', kind: 0, roleIds: [] },
  { userId: CAROL, username: 'carol', display: '', kind: 0, roleIds: [] },
];
export const DMS: DmSummary[] = [{ id: DM, kind: 3, members: [ME.id, BOB], name: 'bob', group: 'active' }];

export function callState(over: Partial<CallState> = {}): CallState {
  return { ...CALL_IDLE, ...over };
}
export function ring(over: Partial<Ring> = {}): Ring {
  return { callId: DM, channelId: DM, callerUser: BOB, callerName: 'bob', ringTs: 1_900_000_030, expiresAt: Date.now() + 30_000,
    busy: false, quiet: false, ...over };
}
export function bobChanged(): TrustSummary {
  return { userId: BOB, name: 'bob', shared: true, umk: 'ab'.repeat(32), firstSeen: 1, verified: false, changedUmk: 'cd'.repeat(32) };
}

/** A PageCall double: the toggles change its state, and every call is recorded in order. */
export class FakeSession implements PageCall {
  state: PageCallState = { phase: 'connected', muted: false, deafened: false, camera: false, sharing: false, paused: false, mic: 'ok',
    tiles: [], level: 0, error: null, mode: 'vad', speaking: false, localCamera: null, localScreen: null, shareAudio: null, shareSource: null, noise: 'on', audioBlocked: false,
    audio: { attached: 0, expected: 0 }, shareCapBps: 2_500_000 };
  readonly log: string[];
  private readonly fns = new Set<() => void>();
  readonly setPaused = vi.fn((on: boolean) => { this.log.push(`setPaused:${String(on)}`); this.update({ paused: on }); return Promise.resolve(); });
  readonly dropKeys = vi.fn((upTo?: bigint) => { this.log.push(upTo === undefined ? 'dropKeys' : `dropKeys:${upTo}`); });
  // As the real session: a person's leave posts callLeave with its reason; a block or removal posts 'user' after stopping.
  readonly leave = vi.fn((reason: 'user' | 'pagehide' | 'switch' | 'blocked' | 'removed') => {
    this.log.push(`leave:${reason}`);
    const posted = reason === 'blocked' || reason === 'removed' ? 'user' : reason;
    return this.leaveGate.then(() => this.bridge?.call({ m: 'callLeave', reason: posted })).then(() => {
      this.log.push(`left:${reason}`); this.update({ phase: 'ended' });
    });
  });
  leaveGate: Promise<void> = Promise.resolve();
  readonly pushToTalk = vi.fn();
  readonly setVolume = vi.fn();
  readonly startAudio = vi.fn(() => { this.log.push('startAudio'); return Promise.resolve(); });
  sharePool: { sources: Array<{ id: string; label: string; monitor: boolean }>; labeled: boolean } = { sources: [], labeled: true };
  readonly shareSources = vi.fn(() => Promise.resolve(this.sharePool));
  readonly setShareSource = vi.fn((id: string | null) => {
    this.log.push(`shareSource:${id}`);
    this.update({ shareSource: id, shareAudio: id === null ? 'none' : 'device' });
    return Promise.resolve();
  });
  readonly setOutput = vi.fn((id: string) => { this.log.push(`output:${id}`); return Promise.resolve(); });
  private readonly bridge: Pick<CoreClient, 'call'> | null;
  constructor(log: string[], bridge: Pick<CoreClient, 'call'> | null = null) { this.log = log; this.bridge = bridge; }
  get muted(): boolean { return this.state.muted; }
  get deafened(): boolean { return this.state.deafened; }
  subscribe(fn: () => void): () => void { this.fns.add(fn); return () => { this.fns.delete(fn); }; }
  update(over: Partial<PageCallState>): void {
    this.state = { ...this.state, ...over };
    for (const fn of [...this.fns]) fn();
  }
  readonly setMode = vi.fn((mode: 'vad' | 'ptt') => { this.log.push(`mode:${mode}`); this.update({ mode }); });
  readonly setNoise = vi.fn((noise: 'on' | 'off') => { this.log.push(`noise:${noise}`); this.update({ noise }); return Promise.resolve(); });
  // Signal task 10b: Settings → Voice's levels and switches during the call.
  readonly setInputVolume = vi.fn((p: number) => { this.log.push(`inputVolume:${p}`); });
  readonly setOutputVolume = vi.fn((p: number) => { this.log.push(`outputVolume:${p}`); });
  readonly setSensitivity = vi.fn((s: { auto: boolean; thresholdDb: number }) => { this.log.push(`sensitivity:${s.auto ? 'auto' : s.thresholdDb}`); });
  readonly setPttRelease = vi.fn((ms: number) => { this.log.push(`pttRelease:${ms}`); });
  readonly setProcessing = vi.fn((p: { echo: boolean; agc: boolean }) => { this.log.push(`processing:${p.echo ? 'echo' : '-'}/${p.agc ? 'agc' : '-'}`); return Promise.resolve(); });
  setMuted(on: boolean): void { this.log.push(`muted:${String(on)}`); this.update({ muted: on }); }
  setDeafened(on: boolean): void { this.log.push(`deafened:${String(on)}`); this.update({ deafened: on, muted: on || this.state.muted }); }
  readonly setCamera = vi.fn((on: boolean): Promise<void> => { this.update({ camera: on }); return Promise.resolve(); });
  /** Task 8c: the options of the last share start and Apply. */
  shareOptions: ShareOptions | undefined;
  setSharing(on: boolean, options?: ShareOptions): Promise<void> {
    if (on) this.shareOptions = options;
    this.update({ sharing: on });
    return Promise.resolve();
  }
  readonly setShareOptions = vi.fn((options: ShareOptions): Promise<void> => { this.shareOptions = options; return Promise.resolve(); });
  tileStatsValue: TileStats = { ownAudio: null, ownScreen: null, screens: {} };
  readonly tileStats = vi.fn((): Promise<TileStats> => Promise.resolve(this.tileStatsValue));
}

export interface FakeSounds { played: ('join' | 'leave')[]; ringing: boolean; starts: number }

/** The fake Notification constructor's instances. */
export interface FakeNote { title: string; options: NotificationOptions; closed: boolean; onclick: ((ev: Event) => unknown) | null; close(): void }

export interface VoiceHarnessOptions {
  update?: import('../screens/Shell.tsx').ShellUpdateDeps;
  path?: string;
  settings?: Record<string, string>;
  permission?: NotificationPermission | null;
  supported?: boolean;
  activated?: boolean;
  voice?: VoiceState;
  call?: CallState;
  trust?: TrustSummary[];
  /** The worker's sendBlocks slice; absent: not published yet. */
  sendBlocks?: SendBlocksState;
  /** callJoin leaves the call slice as it is (the worker's 'in' is published by the test). */
  joinLeavesSlice?: boolean;
  /** The page is hidden (document.visibilityState): ring notifications show only then (flow 06). */
  hidden?: boolean;
  /** Also render Settings over the shell on a settings route, as App does. */
  withSettings?: boolean;
  /** The call module fails to load (a lost chunk). */
  loadFails?: boolean;
  /** selectCommunity refuses with this code (a server whose channels did not load). */
  refuseSelect?: string;
  /** The session starts with the browser holding call audio back (audioBlocked), or turns so in the tick it attaches. */
  audioBlocked?: 'at-start' | 'same-tick';
}

/** A call AudioContext double (VoiceDeps.createCallContext): which one, and whether it was closed. */
export interface FakeCallContext { n: number; closed: boolean; close(): Promise<void> }

function Root({ update }: { update?: import('../screens/Shell.tsx').ShellUpdateDeps }) {
  // The shipping Shell owns both settings workspaces and keeps the media runtime mounted beneath them.
  return createElement(Shell as FunctionComponent<{ update?: import('../screens/Shell.tsx').ShellUpdateDeps }>, { update });
}

export function renderVoiceShell(o: VoiceHarnessOptions = {}) {
  replaceHistoryState(null, '', o.path ?? `/c/${A}/${GENV}`);
  const log: string[] = [];
  const sessions: FakeSession[] = [];
  const notifications: FakeNote[] = [];
  const requestPermission = vi.fn(() => Promise.resolve<NotificationPermission>('granted'));
  if (o.permission !== null) {
    const permission = o.permission ?? 'default';
    class FakeNotification implements FakeNote {
      static readonly permission = permission;
      static readonly requestPermission = requestPermission;
      closed = false;
      onclick: ((ev: Event) => unknown) | null = null;
      constructor(readonly title: string, readonly options: NotificationOptions) { notifications.push(this); }
      close(): void { this.closed = true; }
    }
    vi.stubGlobal('Notification', FakeNotification);
  }
  const activation = { hasBeenActive: o.activated ?? true };
  const visibility = { state: o.hidden === true ? 'hidden' : 'visible' };
  Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => visibility.state });
  Object.defineProperty(document, 'hidden', { configurable: true, get: () => visibility.state === 'hidden' });
  Object.defineProperty(navigator, 'userActivation', { configurable: true, get: () => activation });

  const fake = new FakeClient();
  fake.handler = command => {
    if (command.m === 'callJoin') {
      log.push(`callJoin:${command.channelId}`);
      // As the worker: phase 'in' is published before callJoin answers (L-TS-56 step 1), unless a test holds it.
      if (!o.joinLeavesSlice) fake.set('call', callState({ phase: 'in', channelId: command.channelId, callId: command.channelId }));
    }
    if (command.m === 'selectCommunity' && o.refuseSelect !== undefined) return Promise.reject(refusal({ code: o.refuseSelect }));
    return Promise.resolve(null);
  };
  fake.set('account', account());
  fake.set('connection', { status: 'online', generation: '1' });
  fake.set('communities', [{ id: A, name: 'Midgard', owner: 'ee'.repeat(16) }]);
  // A refused selectCommunity leaves the server's channels unloaded (the sidebar says so).
  if (o.refuseSelect === undefined) fake.set(`channels:${A}`, CHANNELS);
  fake.set(`afk:${A}`, { channelId: null, timeoutSeconds: 300, pending: false });
  fake.set(`members:${A}`, MEMBERS);
  fake.set('dms', DMS);
  fake.set('settings', o.settings ?? {});
  fake.set('trust', o.trust ?? []);
  fake.set('voice', o.voice ?? {});
  fake.set('call', o.call ?? callState());
  fake.set('rings', []);
  if (o.sendBlocks !== undefined) fake.set('sendBlocks', o.sendBlocks);

  // holdStart(): the next start stays pending (after its callJoin) until the returned release runs.
  let startGate: Promise<void> = Promise.resolve();
  const holdStart = (): (() => void) => {
    let release: () => void = () => undefined;
    startGate = new Promise<void>(resolve => { release = resolve; });
    return release;
  };
  const sounds: FakeSounds = { played: [], ringing: false, starts: 0 };
  const module: VoiceModule = {
    isVoiceSupported: vi.fn(() => (o.supported === false ? { ok: false as const, reason: 'no-transform-api' as const }
      : { ok: true as const, path: 'insertable-streams' as const })),
    localDecodeList: () => 'vp8',
    browserDeps: bridge => ({ bridge } as unknown as ReturnType<VoiceModule['browserDeps']>),
    startPageCall: vi.fn<VoiceModule['startPageCall']>(async (deps, target, opts) => {
      await deps.bridge.call({ m: 'callJoin', channelId: target.channelId, vdec: target.vdec });
      // As the real session: the page's abort ends a start that has not connected yet (WB client I1).
      await new Promise<void>((resolve, reject) => {
        void startGate.then(resolve);
        const onAbort = () => {
          log.push(`aborted:${target.channelId}`);
          void deps.bridge.call({ m: 'callLeave', reason: 'user' });
          reject(Object.assign(new Error('E_ABORTED'), { code: 'E_ABORTED' }));
        };
        if (opts.signal?.aborted === true) onAbort(); else opts.signal?.addEventListener('abort', onAbort, { once: true });
      });
      startedWith.push((opts.audioContext ?? null) as unknown as FakeCallContext | null);
      const session = new FakeSession(log, deps.bridge);
      if (o.audioBlocked === 'at-start') session.state = { ...session.state, audioBlocked: true };
      if (o.audioBlocked === 'same-tick') {
        const subscribe = session.subscribe.bind(session);
        session.subscribe = (fn) => {
          const off = subscribe(fn);
          queueMicrotask(() => session.update({ audioBlocked: true }));
          return off;
        };
      }
      // As the real session: it starts in the mode it was started with, and in the kept mute or deafen (`self`). It does not
      // read `selfNow`, so the shell's attach reconcile is what a press during the start is tested against.
      session.state = { ...session.state, mode: opts.mode, muted: opts.self === 'muted' || opts.self === 'deafened', deafened: opts.self === 'deafened' };
      sessions.push(session);
      return session;
    }),
    bindPushToTalk: vi.fn(() => () => undefined),
    volumesFromSettings: () => ({}),
    streamVolumesFromSettings: () => ({}),
  };
  const callContexts: FakeCallContext[] = [];
  const startedWith: (FakeCallContext | null)[] = [];
  const deps: VoiceDeps = {
    load: () => (o.loadFails === true ? Promise.reject(new Error('E_CHUNK')) : Promise.resolve(module)),
    createCallContext: () => {
      const ctx: FakeCallContext = { n: callContexts.length + 1, closed: false, close: () => { ctx.closed = true; return Promise.resolve(); } };
      callContexts.push(ctx);
      log.push(`ctx:${ctx.n}`);
      return ctx as unknown as AudioContext;
    },
    createSounds: () => ({
      play: kind => { sounds.played.push(kind); },
      startRing: () => { sounds.ringing = true; sounds.starts += 1; },
      stopRing: () => { sounds.ringing = false; },
      dispose: () => { sounds.ringing = false; },
    }),
  };
  const user = userEvent.setup();
  const tree = createElement(VoiceDepsContext.Provider, { value: deps }, createElement(Root, { update: o.update }));
  const view = render(createElement('div', { className: 'd-root' }, createElement(CoreProvider, { client: fake, children: tree })));
  return {
    client: fake,
    user, view, log, sessions, sounds, notifications, module, requestPermission, activation, holdStart, callContexts, startedWith,
    /** Hides or shows the page, as the browser does, with its visibilitychange event. */
    setHidden(hidden: boolean): void {
      visibility.state = hidden ? 'hidden' : 'visible';
      act(() => { document.dispatchEvent(new Event('visibilitychange')); });
    },
    get session(): FakeSession { const s = sessions.at(-1); if (s === undefined) throw new Error('no session'); return s; },
  };
}
