import type { ConsoleMessage, ElectronApplication, Page, TestInfo } from '@playwright/test';
import { writeFile } from 'node:fs/promises';
import { startSignalWire } from './signal-wire.ts';

export const SIGNAL_PREFIX = 'DILLA_SIGNAL_LIFECYCLE ';
type SignalEvent = 'created' | 'open' | 'close-request' | 'closed' | 'error' | 'limit';
interface SignalObservation {
  event: SignalEvent; socket: number; at: number; state: number; clientClose: boolean;
  code?: number; clean?: boolean;
}
interface SdkSignalEvent { event: string; from?: string; to?: string; code?: number; action?: number }

/** Test-only observation of the actual native transport. No messages, URLs, reasons, credentials, or SDP are recorded.
 * The subclass delegates construction and close unchanged; listeners observe native events without intercepting them. */
export function installSignalObserver(): void {
  const root = globalThis as unknown as { WebSocket: typeof WebSocket; __dillaSignalLifecycleInstalled?: boolean };
  if (root.__dillaSignalLifecycleInstalled) return;
  root.__dillaSignalLifecycleInstalled = true;
  const Native = root.WebSocket;
  let next = 0, count = 0;
  const emit = (value: SignalObservation) => {
    if (count >= 128) return;
    count++;
    console.info('DILLA_SIGNAL_LIFECYCLE ' + JSON.stringify(count === 128 ? { ...value, event: 'limit' } : value));
  };
  root.WebSocket = class extends Native {
    private signalId: number | null = null;
    private clientClose = false;
    constructor(url: string | URL, protocols?: string | string[]) {
      super(url, protocols);
      try { if (!/^\/rtc(?:\/v1)?$/.test(new URL(String(url)).pathname)) return; }
      catch { return; }
      this.signalId = ++next;
      this.note('created');
      this.addEventListener('open', () => this.note('open'));
      this.addEventListener('error', () => this.note('error'));
      this.addEventListener('close', event => this.note('closed', { code: event.code, clean: event.wasClean }));
    }
    private note(event: SignalEvent, extra: { code?: number; clean?: boolean } = {}): void {
      if (this.signalId !== null) emit({ event, socket: this.signalId, at: performance.now(), state: this.readyState, clientClose: this.clientClose, ...extra });
    }
    override close(code?: number, reason?: string): void {
      super.close(code, reason);
      this.clientClose = true;
      this.note('close-request', code === undefined ? {} : { code });
    }
  };
}

/** Reconstruct an allowlist rather than copying an arbitrary renderer console object into CI artifacts. */
export function signalObservation(text: string): SignalObservation | null {
  if (!text.startsWith(SIGNAL_PREFIX) || text.length > 4096) return null;
  let value: Record<string, unknown>;
  try { value = JSON.parse(text.slice(SIGNAL_PREFIX.length)) as Record<string, unknown>; } catch { return null; }
  if (!value || typeof value.event !== 'string' || !['created', 'open', 'close-request', 'closed', 'error', 'limit'].includes(value.event)
    || !Number.isInteger(value.socket) || Number(value.socket) < 1 || !Number.isFinite(value.at) || Number(value.at) < 0
    || !Number.isInteger(value.state) || Number(value.state) < 0 || Number(value.state) > 3 || typeof value.clientClose !== 'boolean') return null;
  const out: SignalObservation = { event: value.event as SignalEvent, socket: Number(value.socket), at: Number(value.at), state: Number(value.state), clientClose: value.clientClose };
  if (Number.isInteger(value.code) && Number(value.code) >= 0 && Number(value.code) <= 65535) out.code = Number(value.code);
  if (typeof value.clean === 'boolean') out.clean = value.clean;
  return out;
}

/** The SDK's context objects contain room/device identifiers and its connecting line contains a token-bearing URL. */
export function sdkSignalEvent(text: string): SdkSignalEvent | null {
  const state = /^connection state changed: (disconnected|connecting|connected|reconnecting) -> (disconnected|connecting|connected|reconnecting)(?:\s|$)/.exec(text);
  if (state) return { event: 'state', from: state[1], to: state[2] };
  if (/^disconnect from room(?:\s|$)/.test(text)) return { event: 'disconnect' };
  if (/^signal connected(?:\s|$)/.test(text)) return { event: 'signal-connected' };
  if (text.startsWith('signal connecting to ')) return { event: 'signal-connecting' };
  if (text.startsWith('websocket connection closed:')) return { event: 'transport-closed' };
  const serverLeave = /^client leave request received \(action=([012])\)(?:\s|$)/.exec(text);
  if (serverLeave) return { event: 'server-leave', action: Number(serverLeave[1]) };
  if (/^error reading from signal stream(?:\s|$)/.test(text)) {
    const code = /WS closed unexpectedly with code (\d+)/.exec(text);
    return { event: 'stream-error', ...(code && Number(code[1]) <= 65535 ? { code: Number(code[1]) } : {}) };
  }
  return null;
}

type Mark = 'body-complete' | 'before-leave' | 'after-leave' | 'before-context-close' | 'after-context-close'
  | 'before-host-stop' | 'after-host-stop' | 'before-host-start' | 'after-host-start'
  | 'before-page-reload' | 'after-page-reload' | 'before-context-suspend' | 'after-context-resume' | 'case-start';
type Actor = 'alice' | 'bob' | 'ada' | 'bjorn' | 'phone';
type Recorder = (mark: Mark) => Promise<void>;
const recorders = new WeakMap<Page, Recorder>();

/** Only explicitly diagnosed cases register a recorder. Other tests retain their existing teardown verbatim. */
export async function markSignalLifecycle(page: Page, mark: Mark): Promise<void> { await recorders.get(page)?.(mark); }

export function signalLifecycleDiagnostics(info: TestInfo, suite: 'csp-axe' | 'afk' | 'autoplay' | 'blocked' = 'csp-axe'): {
  watch(person: { name: string; page: Page }, application?: ElectronApplication): Promise<void>; save(): Promise<void>;
} {
  const started = Date.now();
  const events: object[] = [];
  const dispose: (() => void)[] = [];
  const finishWire: (() => Promise<void>)[] = [];
  let dropped = 0;
  const append = (actor: Actor, source: string, facts: object) => {
    if (events.length >= 256) { dropped++; return; }
    const event = { actor, ms: Date.now() - started, source, ...facts };
    events.push(event);
    // Persist each fact in the job log immediately, before a teardown failure or fast workflow cancellation.
    console.log(`Signal lifecycle diagnostic: ${JSON.stringify(event)}`);
  };
  return {
    async watch(person, application) {
      if (!['alice', 'bob', 'ada', 'bjorn', 'phone'].includes(person.name)) throw new Error('Unknown signal diagnostic actor');
      const actor = person.name as Actor;
      let wire: Awaited<ReturnType<typeof startSignalWire>> | undefined;
      try { if (application) wire = await startSignalWire(application); }
      catch { append(actor, 'wire', { event: 'capture-unavailable' }); }
      let wireDone: Promise<void> | undefined;
      const stopWire = () => wireDone ??= (async () => {
        if (!wire) return;
        try { for (const facts of await wire()) append(actor, 'wire', facts); }
        catch { append(actor, 'wire', { event: 'capture-unavailable' }); }
      })();
      finishWire.push(stopWire);
      const listener = (message: ConsoleMessage) => {
        const text = message.text(), socket = signalObservation(text), sdk = sdkSignalEvent(text);
        if (socket) append(actor, 'websocket', socket);
        else if (sdk) append(actor, 'sdk', sdk);
      };
      person.page.on('console', listener);
      dispose.push(() => { person.page.off('console', listener); recorders.delete(person.page); });
      recorders.set(person.page, async mark => {
        let timer: ReturnType<typeof setTimeout> | undefined;
        const unavailable = { phase: person.page.isClosed() ? 'closed' : 'unavailable', hasLeave: false };
        const read = person.page.isClosed() ? Promise.resolve(unavailable) : person.page.evaluate(() => {
          const value = document.querySelector('[data-call-phase]')?.getAttribute('data-call-phase');
          const phase = value && ['in', 'preview', 'outgoing', 'connecting', 'reconnecting', 'ended', 'removed', 'blocked', 'paused'].includes(value) ? value : 'other';
          return { phase, hasLeave: document.querySelector('[data-control="leave"], .dw-voice-dock, .d-call-bar') !== null };
        }).catch(() => unavailable);
        try {
          const snapshot = await Promise.race([read, new Promise<typeof unavailable>(resolve => { timer = setTimeout(() => resolve(unavailable), 500); })]);
          append(actor, 'mark', { mark, ...snapshot, workers: person.page.isClosed() ? 0 : person.page.workers().length });
        } finally {
          clearTimeout(timer);
          if (mark === 'before-context-close') await stopWire();
        }
      });
      await person.page.context().addInitScript(installSignalObserver);
      await person.page.evaluate(installSignalObserver);
    },
    async save() {
      for (const finish of finishWire) await finish();
      const path = info.outputPath('signal-lifecycle.json');
      await writeFile(path, JSON.stringify({ version: 1, suite, platform: process.platform, dropped, events }));
      await info.attach('Signal lifecycle diagnostic', { path, contentType: 'application/json' });
      for (const release of dispose) release();
    },
  };
}
