// The voice suite's browsers (L-E2E-31 `peerBrowser`): each person is a fresh, non-persistent context of the worker's
// own browser, so `browser.contexts()` counts them (leaks.ts). Every context carries:
// - the guard: no securitypolicyviolation in any page or worker, no uncaught page error, no console error;
// - the recorders the specs read back: the core worker's slices and the key-path scan of every `ret` and `slice`
//   message, every AudioContext and every oscillator tone, desktop notifications, `blob:` URLs minted and revoked, and a
//   switch that reports the page as hidden (a ring notifies only while the tab is hidden, flow 06);
// - the project's media permissions (Chromium) and fake devices (both engines, from the config).
// Init scripts are injected by the protocol, so the page's CSP never governs them.
import { test as base, expect, type Browser, type BrowserContext, type ConsoleMessage, type Locator, type Page, type Worker } from '@playwright/test';
import { copy, FIREFOX_FAVICON_LINE, isFirefoxBrowserFaviconViolation, recordNotifications } from '../../web/support/app';
import { desktopE2E } from '../../web/support/electron.ts';
import { desktopVoiceContext } from './desktop.ts';
import { observeNativeNotifications } from './native-notifications.ts';

/** One wait for one state (Global Constraints: at least 30 s locally, 90 s in CI). voice.ts re-exports it. */
export const WAIT = process.env.CI === 'true' ? 90_000 : 30_000;

/** A console line that reports a Content-Security-Policy refusal, in either engine's wording (Chromium: "Refused to …";
 *  Firefox: "Content-Security-Policy: The page’s settings blocked …"), never Firefox's notes on ignored sources. */
const CSP_LINE = /Refused to |Content.Security.Policy: The page.s settings blocked/;

/** Console lines Chromium prints for a response the client answers or retries by design (as the web suite, E2E-CI-01). */
const BY_DESIGN_RESOURCE_LINE = /^Failed to load resource: the server responded with a status of (401|404|409|410|429|503) \(/;

/**
 * The one line Firefox 155 prints itself when a page in a call reloads: its media stack aborts a pending capture or
 * device request of the document being unloaded. It names no script (empty location), fires no `unhandledrejection`
 * and no `error` event the page could catch (measured in leave-reload, task 10), so it is the browser's, not the app's.
 */
function firefoxUnloadLine(projectName: string, text: string, url: string): boolean {
  return projectName === 'firefox-voice' && ((url === '' && text === '[JavaScript Error: "InvalidStateError: Navigated away from page"]')
    // web3.9 ruling V-2 (on V-1, disconnectOnPageLeave off): on a Firefox reload the signal socket is already closed when the
    // session's pagehide leave disconnects the room, so LiveKit logs this and drops the signal leave. Benign: the SFU sees
    // the close, the worker's self-Remove goes out, and the peer sees the leave in time.
    || text === 'cannot send signal request before connected, type: leave JSHandle@object'
    // Firefox itself prints this when a reload closes the open signal socket while the next document loads (the same class as
    // V-2: a navigation tearing the call down). Matched without the port, so it holds for any test host; the query string is a token.
    || FIREFOX_SOCKET_INTERRUPTED.test(text));
}
const FIREFOX_SOCKET_INTERRUPTED = /^\[JavaScript Error: "The connection to wss?:\/\/[^/"]+\/rtc\/v1\?[^"]* was interrupted while the page was loading\."/;

export interface CspViolation { directive: string; blockedURI: string; sourceFile: string; sample: string }
export interface Person { name: string; context: BrowserContext; page: Page }

export interface Guard {
  readonly cspViolations: CspViolation[];
  readonly consoleErrors: string[];
  readonly pageErrors: string[];
  collectWorkers(): Promise<void>;
}

/** The page-side recorders (one function, serialised into an init script: it may close over nothing). */
function installRecorders(): void {
  type Rec = {
    slices: Record<string, unknown>;
    keyPath: { messages: number; callKeys: number; findings: string[] };
    contexts: AudioContext[];
    tones: { hz: number; state: string }[];
    ringTimers: Set<number>;
    blobs: { created: string[]; revoked: string[] };
    hidden: boolean;
    ringChannels: Set<string>;
    remoteTracks: Set<string>;
    remoteAnalysers: AnalyserNode[];
  };
  const w = window as unknown as { __dw?: Rec; __dwSetHidden?: (h: boolean) => void };
  if (w.__dw !== undefined) return;
  const rec: Rec = { slices: {}, keyPath: { messages: 0, callKeys: 0, findings: [] }, contexts: [], tones: [], ringTimers: new Set(),
    blobs: { created: [], revoked: [] }, hidden: false, ringChannels: new Set(), remoteTracks: new Set(), remoteAnalysers: [] };
  w.__dw = rec;

  // The key path (plan task 10, "Key path in a real browser"): no `ret` or `slice` message between the page and a
  // worker may carry a 16-byte buffer or a `baseKey` field; the base key travels only in `callKeys` and `installEpoch`.
  const KEPT = new Set(['account', 'call', 'voice', 'rings', 'sendBlocks', 'trust']);
  const scan = (value: unknown, path: string, seen: Set<unknown>, depth: number): void => {
    if (value === null || typeof value !== 'object' || seen.has(value) || depth > 12) return;
    seen.add(value);
    if (value instanceof ArrayBuffer) {
      if (value.byteLength === 16) rec.keyPath.findings.push(`${path}: a 16-byte ArrayBuffer`);
      return;
    }
    if (ArrayBuffer.isView(value)) {
      if (value.byteLength === 16) rec.keyPath.findings.push(`${path}: a 16-byte ${value.constructor.name}`);
      return;
    }
    const entries: [string, unknown][] = value instanceof Map ? [...value.entries()].map(([k, v]) => [String(k), v])
      : Array.isArray(value) ? value.map((v, i) => [String(i), v]) : Object.entries(value);
    for (const [k, v] of entries) {
      if (k === 'baseKey') rec.keyPath.findings.push(`${path}.baseKey`);
      scan(v, `${path}.${k}`, seen, depth + 1);
    }
  };
  const observe = (data: unknown, direction: string): void => {
    const m = data as { t?: unknown; name?: unknown; value?: unknown } | null;
    if (m === null || typeof m !== 'object') return;
    if (m.t === 'callKeys') rec.keyPath.callKeys += 1;
    if (m.t !== 'ret' && m.t !== 'slice') return;
    rec.keyPath.messages += 1;
    scan(m, `${direction}:${String(m.t)}${typeof m.name === 'string' ? `:${m.name}` : ''}`, new Set(), 0);
    if (m.t === 'slice' && typeof m.name === 'string' && KEPT.has(m.name)) rec.slices[m.name] = m.value;
    // Every DM a ring was ever listed for, so a ring that came and went between two reads is still seen.
    if (m.t === 'slice' && m.name === 'rings' && Array.isArray(m.value)) {
      for (const r of m.value as { channelId?: unknown }[]) if (typeof r.channelId === 'string') rec.ringChannels.add(r.channelId);
    }
  };
  const NativeWorker = window.Worker;
  const nativePost = NativeWorker.prototype.postMessage;
  NativeWorker.prototype.postMessage = function (this: Worker, message: unknown, ...rest: unknown[]) {
    observe(message, 'to-worker');
    return (nativePost as (...a: unknown[]) => void).call(this, message, ...rest);
  } as typeof NativeWorker.prototype.postMessage;
  // The first listener of every worker the page starts: it sees each message before the app's own listeners, so
  // before the bridge zero-fills a key buffer.
  class RecordedWorker extends NativeWorker {
    constructor(url: string | URL, options?: WorkerOptions) {
      super(url, options);
      this.addEventListener('message', (e) => observe((e as MessageEvent).data, 'from-worker'));
    }
  }
  Object.defineProperty(window, 'Worker', { value: RecordedWorker, configurable: true, writable: true });

  // Audio: every AudioContext the page makes, and every oscillator tone with its frequency and its context's state
  // when it starts (the call sounds are synthesised, L-WEB-52: the ring is 1000 Hz, join 660→880, leave 880→660).
  for (const name of ['AudioContext', 'webkitAudioContext'] as const) {
    const Native = (window as unknown as Record<string, typeof AudioContext | undefined>)[name];
    if (Native === undefined) continue;
    class RecordedContext extends Native {
      constructor(options?: AudioContextOptions) {
        super(options);
        rec.contexts.push(this);
      }
    }
    Object.defineProperty(window, name, { value: RecordedContext, configurable: true, writable: true });
  }
  // web3.3: sound, not only decryption. Every track a peer connection receives is recorded by id; a WebAudio source made
  // from a stream (or from an element whose srcObject) that holds one is tapped by an AnalyserNode as well, so `audible`
  // (voice.ts) reads whether a remote call track reaches the page's audio graph with a signal in it. The media worker's
  // counters (`heard`) stay green even when nothing plays the decrypted track.
  const NativePC = window.RTCPeerConnection as typeof RTCPeerConnection | undefined;
  if (NativePC !== undefined) {
    class RecordedPC extends NativePC {
      constructor(config?: RTCConfiguration) {
        super(config);
        this.addEventListener('track', (ev) => { rec.remoteTracks.add((ev as RTCTrackEvent).track.id); });
      }
    }
    Object.defineProperty(window, 'RTCPeerConnection', { value: RecordedPC, configurable: true, writable: true });
  }
  const tap = (ctx: BaseAudioContext, node: AudioNode, stream: unknown): void => {
    if (!(stream instanceof MediaStream) || !stream.getAudioTracks().some((t) => rec.remoteTracks.has(t.id))) return;
    const analyser = ctx.createAnalyser();
    analyser.fftSize = 2048;
    node.connect(analyser);
    rec.remoteAnalysers.push(analyser);
  };
  // createMediaStreamSource and createMediaElementSource live on AudioContext (an OfflineAudioContext has neither).
  const createStreamSource = AudioContext.prototype.createMediaStreamSource;
  AudioContext.prototype.createMediaStreamSource = function (this: AudioContext, stream: MediaStream) {
    const node = createStreamSource.call(this, stream);
    tap(this, node, stream);
    return node;
  };
  const createElementSource = AudioContext.prototype.createMediaElementSource;
  AudioContext.prototype.createMediaElementSource = function (this: AudioContext, element: HTMLMediaElement) {
    const node = createElementSource.call(this, element);
    tap(this, node, element.srcObject);
    return node;
  };
  const createOscillator = BaseAudioContext.prototype.createOscillator;
  BaseAudioContext.prototype.createOscillator = function (this: BaseAudioContext) {
    const osc = createOscillator.call(this);
    const set = osc.frequency.setValueAtTime.bind(osc.frequency);
    osc.frequency.setValueAtTime = (v: number, t: number) => {
      (osc as unknown as { __hz: number }).__hz = v;
      return set(v, t);
    };
    return osc;
  };
  // The ring repeats on a 2 s interval (sounds.ts startRing): that interval is the ring's timer, and a ring has stopped
  // once it is cleared (a state, not a wait for silence). It is the interval started in the same task right after a
  // 1000 Hz ping, or (when the first ping found its AudioContext not running yet) the one whose callback pings.
  const start = OscillatorNode.prototype.start;
  let lastTone = { hz: 0, at: -1 };
  let running: number | null = null;
  OscillatorNode.prototype.start = function (this: OscillatorNode, when?: number) {
    const hz = (this as unknown as { __hz?: number }).__hz ?? this.frequency.value;
    rec.tones.push({ hz, state: this.context.state });
    lastTone = { hz, at: performance.now() };
    if (hz === 1000 && running !== null) rec.ringTimers.add(running);
    return start.call(this, when);
  };
  const setIntervalNative = window.setInterval.bind(window);
  const clearIntervalNative = window.clearInterval.bind(window);
  window.setInterval = ((fn: TimerHandler, ms?: number, ...args: unknown[]) => {
    if (typeof fn !== 'function') return setIntervalNative(fn, ms, ...args);
    const holder = { id: -1 };
    holder.id = setIntervalNative((...a: unknown[]) => {
      running = holder.id;
      try { (fn as (...x: unknown[]) => void)(...a); } finally { running = null; }
    }, ms, ...args);
    if (ms === 2000 && lastTone.hz === 1000 && performance.now() - lastTone.at < 50) rec.ringTimers.add(holder.id);
    return holder.id;
  }) as typeof window.setInterval;
  window.clearInterval = ((id?: number) => {
    if (id !== undefined) rec.ringTimers.delete(id);
    clearIntervalNative(id);
  }) as typeof window.clearInterval;

  // blob: URLs (attachment players): minted and revoked.
  const create = URL.createObjectURL.bind(URL);
  const revoke = URL.revokeObjectURL.bind(URL);
  URL.createObjectURL = (obj: Blob | MediaSource) => {
    const url = create(obj);
    rec.blobs.created.push(url);
    return url;
  };
  URL.revokeObjectURL = (url: string) => {
    rec.blobs.revoked.push(url);
    revoke(url);
  };

  // A hidden tab, on demand: a headless page is always visible, and a ring notifies only while the tab is hidden.
  Object.defineProperty(Document.prototype, 'visibilityState', { configurable: true, get: () => (rec.hidden ? 'hidden' : 'visible') });
  Object.defineProperty(Document.prototype, 'hidden', { configurable: true, get: () => rec.hidden });
  w.__dwSetHidden = (h: boolean) => {
    rec.hidden = h;
    document.dispatchEvent(new Event('visibilitychange'));
  };
}

function newGuard(projectName: string): Guard & { attach(context: BrowserContext): Promise<void> } {
  const cspViolations: CspViolation[] = [];
  const consoleErrors: string[] = [];
  const pageErrors: string[] = [];
  const workers: Worker[] = [];
  // A worker's own securitypolicyviolation listener is installed by an evaluate that runs after the worker has started
  // (Playwright has no init script for workers), so a violation in its first tick (wasm instantiation, an early fetch)
  // can be missed by it (task-10 review M8). The worker's console is watched from the moment Playwright reports the
  // worker, and a CSP refusal line there fails the test too.
  const watchWorker = (wk: Worker): void => {
    workers.push(wk);
    wk.on('console', (m: ConsoleMessage) => {
      if (CSP_LINE.test(m.text())) consoleErrors.push(`worker ${wk.url()}: ${m.text()}`);
    });
    void wk.evaluate(() => {
      const g = self as unknown as { __dillaCsp?: unknown[] };
      g.__dillaCsp = [];
      self.addEventListener('securitypolicyviolation', (e) => {
        const v = e as SecurityPolicyViolationEvent;
        g.__dillaCsp!.push({ directive: v.violatedDirective, blockedURI: v.blockedURI, sourceFile: v.sourceFile, sample: v.sample });
      });
    }).catch(() => undefined);
  };
  const watchPage = (p: Page): void => {
    p.on('worker', watchWorker);
    for (const wk of p.workers()) watchWorker(wk);
  };
  return {
    cspViolations, consoleErrors, pageErrors,
    async attach(context: BrowserContext): Promise<void> {
      await context.exposeBinding('__dillaCspViolation', (_source, v: CspViolation) => {
        if (!isFirefoxBrowserFaviconViolation(projectName, v)) cspViolations.push(v);
      });
      await context.addInitScript(() => {
        document.addEventListener('securitypolicyviolation', (e) => {
          (window as unknown as { __dillaCspViolation(v: unknown): void }).__dillaCspViolation({
            directive: e.violatedDirective, blockedURI: e.blockedURI, sourceFile: e.sourceFile, sample: e.sample,
          });
        });
      });
      context.on('console', (m) => {
        const resourceLine = projectName === 'chromium-voice' && BY_DESIGN_RESOURCE_LINE.test(m.text());
        const firefoxFavicon = projectName.startsWith('firefox') && FIREFOX_FAVICON_LINE.test(m.text());
        if (m.type() === 'error' && !resourceLine && !firefoxFavicon && !firefoxUnloadLine(projectName, m.text(), m.location().url)) consoleErrors.push(m.text());
      });
      context.on('weberror', (e) => { pageErrors.push(String(e.error())); });
      context.on('page', watchPage);
      for (const p of context.pages()) watchPage(p);
    },
    async collectWorkers(): Promise<void> {
      for (const wk of workers) {
        const found = await wk.evaluate(() => {
          const g = self as unknown as { __dillaCsp?: unknown[] };
          const out = g.__dillaCsp ?? [];
          g.__dillaCsp = [];
          return out;
        }).catch(() => [] as unknown[]);
        cspViolations.push(...(found as CspViolation[]));
      }
    },
  };
}

export interface PeerBrowsers {
  /** Opens one person's context and page; at most a few per test, each closed by `close` (or the fixture's net). `browser`
   *  (web3.4 V4): a browser the spec launched with switches of its own, instead of the worker's; the spec closes it after
   *  closing the people it opened there. */
  open(name: string, opts?: { viewport?: { width: number; height: number }; browser?: Browser }): Promise<Person>;
  /** Leaves the call through the app's own leave, waits for the media worker to end, then closes the context. */
  close(person: Person): Promise<void>;
  /** Hands a native peer (anything with an idempotent close) to the fixture, which closes it after the people, whatever
   *  threw before (task-10 review M5): a peer started before a browser failed to open, or left behind by a close that
   *  threw, never outlives its test. Answers the peer. */
  track<T extends { close(): Promise<void> }>(peer: T): T;
  readonly guard: Guard;
}

/** The control that leaves the call from where this page shows one. Both the stage's toolbar and the dock hold a leave
 *  (web3.8), so this names one: the stage's while the stage has its leave button, the dock's only when it does not (off
 *  the stage), else the phone's call bar. */
export function leaveControl(page: Page): Locator {
  const leave = copy('voice.leave');
  // Signal §5.6: a channel's call bar names it Leave (the DM box keeps voice.leave); data-control names it either way.
  const stageLeave = page.locator('.dw-voice-stage [data-control="leave"]');
  const dockLeave = page.locator('body:not(:has(.dw-voice-stage [data-control="leave"])) .dw-voice-dock').getByRole('button', { name: leave, exact: true });
  const barLeave = page.locator('.d-call-bar').getByRole('button', { name: leave, exact: true });
  return stageLeave.or(dockLeave).or(barLeave);
}

/** Leaves voice through the app's own leave (a person's leave), and waits until the page's media worker has ended. */
export async function leaveIfInCall(page: Page): Promise<void> {
  if (page.isClosed()) return;
  // The call's strip: the dock on a wide window, the call bar on a phone (neither while the call is over).
  const strip = page.locator('.dw-voice-dock, .d-call-bar');
  if (await strip.count() > 0) {
    // A leave acts on an attached session: wait out `connecting` first.
    await expect(strip.locator('xpath=descendant-or-self::*[@data-phase]').first()).not.toHaveAttribute('data-phase', 'connecting', { timeout: WAIT });
    await leaveControl(page).click();
    await expect(strip).toHaveCount(0, { timeout: WAIT });
  } else {
    // A phone on the call stage has no strip: leave through the stage's own Leave.
    const stageLeave = page.locator('.dw-voice-stage [data-control="leave"]');
    if (await stageLeave.count() > 0) {
      await stageLeave.first().click();
      await expect(stageLeave).toHaveCount(0, { timeout: WAIT });
    }
  }
  await expect.poll(() => mediaWorkers(page).length, { timeout: WAIT, message: 'the media worker ends after the leave' }).toBe(0);
}

/** The page's E2EE media workers (`dilla-media-worker`, packages/media/src/manager.ts): every dedicated worker but the
 *  core worker, whose script is the web build's `entry-*.js` chunk (packages/web/src/core/worker entry). */
export function mediaWorkers(page: Page): Worker[] {
  return page.workers().filter((wk) => !/\/assets\/entry-[\w-]+\.js$/.test(wk.url()));
}

async function openContext(browser: Browser, o: { baseURL: string | undefined; permissions: string[] | undefined; viewport: { width: number; height: number } | null }): Promise<BrowserContext> {
  return browser.newContext({
    ...(o.baseURL === undefined ? {} : { baseURL: o.baseURL }),
    ...(o.permissions === undefined ? {} : { permissions: o.permissions }),
    ...(o.viewport === null ? {} : { viewport: o.viewport }),
  });
}

/** L-E2E-31 `peerBrowser`: the people of a test, each a browser context of their own. */
export const test = base.extend<{ peerBrowser: PeerBrowsers }>({
  peerBrowser: async ({ browser, baseURL, permissions, viewport, launchOptions }, use, testInfo) => {
    const guard = newGuard(testInfo.project.name);
    const open: Person[] = [];
    const peers: { close(): Promise<void> }[] = [];
    const leaveFailures: string[] = [];
    const people: PeerBrowsers = {
      guard,
      track(peer) {
        peers.push(peer);
        return peer;
      },
      async open(name, opts) {
        if (desktopE2E() && opts?.browser) throw new Error('Browser tab capture requires a native application-audio counterpart');
        const context = desktopE2E()
          ? await desktopVoiceContext({ baseURL, permissions, launchOptions, viewport: opts?.viewport ?? viewport })
          : await openContext(opts?.browser ?? browser, { baseURL, permissions, viewport: opts?.viewport ?? viewport });
        await guard.attach(context);
        await context.addInitScript(installRecorders);
        if (desktopE2E()) await observeNativeNotifications(context);
        else await recordNotifications(context);
        const page = await context.newPage();
        const person = { name, context, page };
        open.push(person);
        return person;
      },
      async close(person) {
        const i = open.indexOf(person);
        if (i < 0) return;
        open.splice(i, 1);
        // A failed leave is recorded and fails the test at the end, after the context is closed anyway: thrown from a
        // spec's `finally` it would hide the error that sent the test there.
        try {
          await leaveIfInCall(person.page);
        } catch (e) {
          leaveFailures.push(`${person.name}: ${String(e).split('\n')[0]}`);
        } finally {
          await guard.collectWorkers();
          await person.context.close();
        }
      },
    };
    try {
      await use(people);
    } finally {
      // The net under each spec's own `finally`: whatever is still open is left and closed, then every native peer.
      try {
        for (const person of [...open].reverse()) await people.close(person).catch(() => undefined);
      } finally {
        for (const peer of [...peers].reverse()) await peer.close().catch(() => undefined);
      }
    }
    await guard.collectWorkers();
    expect(leaveFailures, 'a leave through the app, then the media worker ended, before each context closed').toEqual([]);
    expect(guard.cspViolations, 'securitypolicyviolation events').toEqual([]);
    expect(guard.pageErrors, 'uncaught page errors').toEqual([]);
    expect(guard.consoleErrors, 'console errors').toEqual([]);
  },
});
export { expect };
