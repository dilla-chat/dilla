// The web e2e suite's page-level support (task 25): the native peer, the page contract the specs drive
// (the copy keys of en.ts, landmarks, the L-UI root classes, data-state), the CSP/console guard every test
// carries (every page and the core worker), the onboarding and shell drivers, and the probes of the
// device-key record and the instance clock. Every wait waits for a state, never for a duration (lesson c).
import { createHash, randomInt } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { expect, type BrowserContext, type ConsoleMessage, type Locator, type Page, type Response, type Worker } from '@playwright/test';
import { test as persistentTest } from './persistent';
import { WebDriver, type PeerAttachRequest, type PeerAttached, type PeerDm, type PeerEnrolled, type PeerFetched, type PeerReceived, type PeerSendOptions, type PeerSetup } from './driver';
export type { PeerAttachment, PeerAttachRequest, PeerAttached, PeerDm, PeerEnrolled, PeerFetched, PeerOpenedDm, PeerReceived, PeerSendOptions, PeerSetup } from './driver';
import { runAxe } from './axe';
import { nextCode } from './totp';
import { peerSeed } from './peer-seed';
import { ABORT_WINDOW_MS, isCancelledFetchLine, isNavigationAbortLine } from './abort-line';
import { arr, decode } from '../../../packages/client-core/src/cbor/index';
import { en } from '../../../packages/web/src/strings/en';
import { t } from '../../../packages/web/src/strings/index';

export const REPO_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..', '..', '..');
/** The suite's shared host. A lane that shares its machine with others moves it with DILLA_E2E_PORT and DILLA_E2E_PORT2 (playwright.web.config.ts reads the same two). */
export const HOST_PORT = Number(process.env.DILLA_E2E_PORT ?? 8463);
export const CONTROL_PORT = Number(process.env.DILLA_E2E_PORT2 ?? 8464);
export const HOST = `127.0.0.1:${String(HOST_PORT)}`;
export const PUBLIC_URL = `http://${HOST}`;
export const CONTROL_URL = `http://127.0.0.1:${String(CONTROL_PORT)}`;
/** One wait for one state. The CI runner has 4 vCPUs and is 3-5x slower than the dev machine. */
export const WAIT = process.env.CI === 'true' ? 90_000 : 30_000;
export const TEST_TIMEOUT = process.env.CI === 'true' ? 480_000 : 180_000;
/** dillad's browser session lifetime (internal/config/defaults.go:78, BrowserLifetime 168h) plus one hour. */
export const PAST_BROWSER_SESSION_S = 169 * 3600;
/** One recovery-key group: four Crockford base32 characters (protocol/03 "Recovery"). */
export const RK_GROUP = /^[0-9A-HJKMNP-TV-Z]{4}$/;
export const HEX32 = /^[0-9a-f]{32}$/;
/** The console line Chromium and WebKit print for a response the client answers or retries by design (E2E-CI-01). */
export const BY_DESIGN_RESOURCE_LINE = /^Failed to load resource: the server responded with a status of (401|404|409|410|429|503) \(/;
const INTERACTION_CONTEXT_REFUSAL = 'current interaction context unavailable';
const INTERACTION_CONTEXT_PATH = /^\/v1\/groups\/[0-9a-f]{32}\/interaction-admission-context$/;
const FORBIDDEN_RESOURCE_LINE = /^Failed to load resource: the server responded with a status of 403 \(Forbidden\)$/;

/** This read-only probe intentionally refuses ordinary Welcomes; only its exact denial is expected by the browser guard. */
export function isExpectedInteractionAdmissionContextRefusal(status: number, url: string, body: Uint8Array): boolean {
  let parsed: URL;
  try { parsed = new URL(url); } catch { return false; }
  if (status !== 403 || parsed.protocol !== 'http:' || parsed.hostname !== '127.0.0.1' || parsed.search !== '' || parsed.hash !== '' ||
      !INTERACTION_CONTEXT_PATH.test(parsed.pathname)) return false;
  try {
    const error = arr(decode(body), 3);
    return error[0] === 'E_FORBIDDEN' && error[1] === INTERACTION_CONTEXT_REFUSAL && error[2] === null;
  } catch { return false; }
}
/**
 * The line Firefox prints when it cancels a font download because the page went away mid-load (a tab taken
 * over or a context closed): status 2152398850 is 0x804B0002, NS_BINDING_ABORTED. A font the host fails to
 * serve prints another status and still fails the test.
 */
export const ABORTED_FONT_LINE = /^\[JavaScript Error: "downloadable font: download failed \(.*\): status=2152398850 source: /;
/** Firefox's privileged favicon loader reports this refusal from the static invite page's default-src 'none'. */
export const FIREFOX_FAVICON_LINE = /^\[JavaScript Error: "Content-Security-Policy: The page.s settings blocked the loading of a resource \(img-src\) at http:\/\/127\.0\.0\.1:\d+\/favicon\.ico .*\{file: "resource:\/\/\/modules\/FaviconLoader\.sys\.mjs" line: \d+\}\]$/;
// WebKit\'s cancelled-fetch line (support/abort-line.ts) is dropped only within ABORT_WINDOW_MS of a main-frame
// navigation or a window resize of the same page; anywhere else it still fails the test.

/** Firefox's Playwright juggler worker can log this when a test closes its second browser context. */
/** Firefox's own line when a reload closes the open LiveKit signal socket (port-free; the query is a token). */
export const FIREFOX_SOCKET_INTERRUPTED = /^\[JavaScript Error: "The connection to wss?:\/\/[^/"]+\/rtc\/v1\?[^"]* was interrupted while the page was loading\."/;
export const ABORTED_JUGGLER_LINE =/^\[JavaScript Error: "NS_BINDING_ABORTED: " \{file: "chrome:\/\/juggler\/content\/content\/WorkerMain\.js" line: 6\}\]$/;

// The copy keys the e2e drives: L-COPY-01 (onboarding) and the keys tasks 22 and 24 add to en.ts. The
// values always come from the app's own strings, so a reworded text never breaks the suite.
export const COPY = {
  otherTab: 'boot.otherTab.status',
  storeLost: 'boot.storeLost.status',
  storeLostAction: 'boot.storeLost.action',
  storeLostConfirmTitle: 'boot.storeLost.confirmTitle',
  storeLostConfirm: 'boot.storeLost.confirmAction',
  connectTitle: 'onboarding.connect.title',
  inviteLabel: 'onboarding.connect.invite',
  identityTitle: 'onboarding.identity.title',
  usernameLabel: 'onboarding.identity.username',
  displayLabel: 'onboarding.identity.display',
  passwordLabel: 'onboarding.identity.password',
  keysTitle: 'onboarding.keys.title',
  keysAcknowledge: 'onboarding.keys.acknowledge',
  deviceTitle: 'onboarding.browser.title',
  submit: 'onboarding.browser.submit',
  doneTitle: 'onboarding.done.title',
  next: 'onboarding.next',
  finish: 'onboarding.done.next',
  skipLink: 'shell.skip',
  railLabel: 'shell.rail.label',
  joinTitle: 'join.title',
  joinInvite: 'join.invite',
  joinSubmit: 'join.submit',
  channelsLabel: 'shell.channels.label',
  logLabel: 'shell.log.label',
  composerLabel: 'shell.composer.label',
  statusBarLabel: 'shell.status.label',
} as const;

export const SIGNIN = {
  entry: 'onboarding.connect.signIn', step: 'signin.step',
  loginTitle: 'signin.login.title', username: 'signin.login.username',
  password: 'signin.login.password', loginSubmit: 'signin.login.submit',
  keyTitle: 'signin.key.title', keyLabel: 'signin.key.label',
  keyHint: 'signin.key.hint', keySubmit: 'signin.key.submit',
  keyLength: 'signin.error.keyLength', wrongKey: 'signin.error.wrongKey',
  doneTitle: 'signin.done.title', doneBody: 'signin.done.body', doneNext: 'signin.done.next',
} as const;

// The root classes of five L-UI components (plan head, L-UI class rule: d-<kebab-case component name>).
export const CLASS = {
  onboardingFrame: '.d-onboarding-frame',
  recoveryKey: '.d-recovery-key',
  channelHeader: '.d-channel-header',
  messageRow: '.d-message-row',
  composer: '.d-composer',
  deviceRow: '.d-device-row',
  channelRow: '.d-chrow',
  lightbox: 'dialog.d-lightbox',
  mention: '.d-mention',
  replyChip: '.d-reply-chip',
} as const;

export function copy(key: string, vars?: Record<string, string | number>): string {
  if (!Object.hasOwn(en, key)) {
    throw new Error(`packages/web/src/strings/en.ts has no key "${key}"; the web e2e page contract (task 25) drives it`);
  }
  // Existing capability flows still name the section they exercise with the older copy key. Resolve those
  // names through the workspace registry so the tests keep targeting the same capability after regrouping.
  const workspaceSection: Record<string, string> = {
    'settings.nav.profile': 'workspace.profile',
    'factorSettings.nav': 'workspace.signin',
    'settings.nav.devices': 'workspace.devices',
    'settings.nav.appearance': 'workspace.appearance',
    'settings.nav.notifications': 'workspace.notifications',
    'settings.nav.voice': 'workspace.voice',
    'settings.nav.keyboard': 'workspace.keyboard',
    'settings.nav.privacy': 'workspace.privacy',
    'settings.nav.diagnostics': 'workspace.diagnostics',
    'admin.nav.overview': 'workspace.overview',
    'admin.nav.channels': 'workspace.channels',
    'admin.nav.invites': 'workspace.invites',
    'admin.nav.selfRoles': 'workspace.selfRoles',
    'admin.nav.bans': 'workspace.bans',
    'admin.nav.bot': 'workspace.bot',
    'admin.nav.leave': 'workspace.danger',
    'roles.tab': 'workspace.roles',
  };
  return t((workspaceSection[key] ?? key) as keyof typeof en, vars);
}

// ---------------------------------------------------------------------------------------------------
// The native peer: one web-driver process per test.

export interface PeerSetupOptions { password?: string; channels?: number; }
export const PEER_PASSWORD = 'peer-password-1234';
/** The "pin messages" permission bit (protocol/02's vocabulary, client-core's PERM.pinMessages). */
export const PERM_PIN = 1 << 3;
export type PeerMessage = PeerReceived & { sender_user: string; sender_device: string; tier: number; msg_id: string; type: number };
export function isPeerMessage(m: PeerReceived): m is PeerMessage {
  return m.msg_id !== null && m.type !== null && m.sender_user !== null && m.sender_device !== null && m.tier !== null;
}
interface SyncAnswer { epoch: number; members: number; received: PeerReceived[]; }

let nextPeerSequence = 0;

export class Peer {
  /** Every message the peer decrypted, across all syncs of this test. */
  readonly inbox: PeerReceived[] = [];
  /** The one text channel setup creates; the specs name it when they address the log and the composer. */
  readonly channelName = 'general';
  channelNames: string[] = ['general'];
  communityName = '';

  private constructor(private readonly driver: WebDriver, readonly seed: number) {}

  static async start(): Promise<Peer> {
    const invite = process.env.DILLA_TESTKIT_INVITE;
    if (!invite) throw new Error('DILLA_TESTKIT_INVITE is unset: the web global setup exports it from the test host banner');
    const seed = peerSeed(persistentTest.info().workerIndex, nextPeerSequence++);
    const driver = await persistentTest.step(`Start native peer seed ${seed.toString(16)}`, () =>
      WebDriver.start(PUBLIC_URL, { DILLA_TESTKIT_INVITE: invite, DILLA_TESTKIT_CONTROL: CONTROL_URL }, seed));
    return new Peer(driver, seed);
  }

  async setup(opts: PeerSetupOptions = {}): Promise<PeerSetup> {
    this.communityName = `e2e-${this.seed.toString(16)}`;
    const channels = opts.channels ?? 1;
    const s = await this.driver.setup({
      community: this.communityName, channel: this.channelName,
      ...(opts.password === undefined ? {} : { password: opts.password }),
      ...(opts.channels === undefined ? {} : { channels: opts.channels }),
    });
    for (const id of [s.user_id, s.device_id, s.community_id, s.channel_id, ...s.channel_ids]) expect(id).toMatch(HEX32);
    expect(s.channel_ids).toHaveLength(channels);
    expect(s.channel_ids[0]).toBe(s.channel_id);
    // L-E2E-10: the channels are named <channel>, <channel>-2 … <channel>-<n> in creation order.
    this.channelNames = [this.channelName, ...Array.from({ length: channels - 1 }, (_, i) => `${this.channelName}-${i + 2}`)];
    return s;
  }

  register(): Promise<{ group_id: string; epoch: number }> { return this.driver.register(); }
  send(body: string, channelId?: string, opts?: PeerSendOptions): Promise<{ seq: number; msg_id: string }> { return this.driver.send(body, channelId, opts); }
  edit(msgId: string, body: string, channelId?: string): Promise<{ seq: number; msg_id: string }> {
    return this.driver.send(body, channelId, { type: 1, replyTo: msgId });
  }
  react(msgId: string, emoji: string, on: boolean, channelId?: string): Promise<{ seq: number; msg_id: string }> {
    return this.driver.send(emoji, channelId, { type: on ? 3 : 4, replyTo: msgId });
  }
  deleteMessage(msgId: string, seq: number, channelId?: string): Promise<{ seq: number }> {
    return this.driver.deleteMessage({ msgId, seq, ...(channelId === undefined ? {} : { channelId }) });
  }
  attach(a: PeerAttachRequest): Promise<PeerAttached> { return this.driver.attach(a); }
  fetchAttachment(seq: number, index: number, channelId?: string): Promise<PeerFetched> {
    return this.driver.fetchAttachment({ seq, index, ...(channelId === undefined ? {} : { channelId }) });
  }
  async blobStatus(blobId: string, channelId?: string): Promise<number> {
    return (await this.driver.blobStatus({ blobId, ...(channelId === undefined ? {} : { channelId }) })).status;
  }
  async waitForRow(match: (m: PeerMessage) => boolean, what: string, channelId?: string): Promise<PeerMessage> {
    let found: PeerMessage | undefined;
    await expect.poll(async () => {
      await this.sync(channelId);
      found = [...this.inbox].reverse().filter(isPeerMessage).find(match);
      return found !== undefined;
    }, { timeout: WAIT, intervals: [250, 500, 1000, 2000], message: `the peer never saw ${what}` }).toBe(true);
    if (found === undefined) throw new Error(`the peer never saw ${what}`);
    return found;
  }
  /** L-E2E-01 `update`: drains and applies, then commits a self Update and answers the new epoch. */
  update(): Promise<{ epoch: number }> { return this.driver.update(); }
  /** L-E2E-10 `enrol` through the driver's wrapper (driver.ts, this task). */
  enrol(): Promise<PeerEnrolled> { return this.driver.enrol(); }
  /** L-E2E-10 `revoke` through the driver's wrapper (driver.ts, this task). */
  revoke(deviceId: string): Promise<{ version: number }> { return this.driver.revoke(deviceId); }
  trustList(userId: string): Promise<{ version: number; pin: string | null; verified: boolean; changed: string | null; pendingUmk: string | null }> { return this.driver.trustList(userId); }
  rotateUmk(userId?: string): Promise<{ user: string; version: number }> { return this.driver.rotateUmk(userId); }
  /** Joins another peer's server and its first channel's group (the browser sees this peer arrive in that channel). */
  join(other: PeerSetup, groupId: string): Promise<{ group_id: string; epoch: number }> {
    return this.driver.join({ community_id: other.community_id, channel_id: other.channel_id, group_id: groupId, invite_code: other.invite_code });
  }
  /** The acting device proposes its own removal from the group; another member (the browser) commits it. */
  leave(channelId?: string): Promise<{ group_id: string }> { return this.driver.leave(channelId); }
  typing(channelId: string, on: boolean): Promise<void> { return this.driver.typing(channelId, on); }
  /** The peer, the server's owner, gives everyone in it `bits` (e.g. PERM_PIN) through the roles API, as its roles tab would. */
  allowEveryone(bits: number): Promise<{ allow: number }> { return this.driver.allowEveryone(bits); }
  searchSeed(channelId: string, body: string): Promise<{ seq: number }> { return this.driver.searchSeed(channelId, body); }

  async sync(channelId?: string): Promise<SyncAnswer> {
    const answer = await this.driver.sync(channelId);
    this.inbox.push(...answer.received);
    return answer;
  }

  async devices(channelId?: string): Promise<string[]> {
    return (await this.driver.members(channelId)).devices;
  }

  /** Syncs until a message with exactly this body has been decrypted, and returns it. */
  async waitFor(body: string): Promise<PeerMessage> {
    await expect.poll(async () => {
      await this.sync();
      return this.inbox.some((m) => isPeerMessage(m) && m.body === body);
    }, { timeout: WAIT, intervals: [250, 500, 1000, 2000] }).toBe(true);
    const found = this.inbox.filter(isPeerMessage).find((m) => m.body === body);
    if (!found) throw new Error(`the peer never decrypted ${JSON.stringify(body)}`);
    return found;
  }

  /** Syncs until the text group's roster holds `count` devices, and returns them. */
  async waitForDevices(count: number, channelId?: string): Promise<string[]> {
    let devices: string[] = [];
    await expect.poll(async () => {
      await this.sync(channelId);
      devices = await this.devices(channelId);
      return devices.length;
    }, { timeout: WAIT, intervals: [250, 500, 1000, 2000] }).toBe(count);
    return devices;
  }

  readonly dmInbox: PeerReceived[] = [];
  async dms(): Promise<PeerDm[]> { return (await this.driver.dms()).dms; }
  async waitForDm(channelId: string): Promise<PeerDm> {
    let found: PeerDm | undefined;
    await expect.poll(async () => {
      found = (await this.dms()).find((d) => d.channel_id === channelId);
      return found !== undefined;
    }, { timeout: WAIT, intervals: [250, 500, 1000, 2000] }).toBe(true);
    return found as PeerDm;
  }
  async openDm(userId: string): Promise<{ channelId: string; groupId: string; epoch: number; created: boolean }> {
    const o = await this.driver.openDm(userId);
    return { channelId: o.channel_id, groupId: o.group_id, epoch: o.epoch, created: o.created };
  }
  sendDm(channelId: string, body: string): Promise<{ seq: number }> { return this.driver.sendDm(channelId, body); }
  async syncDm(channelId: string): Promise<PeerReceived[]> {
    const answer = await this.driver.syncDm(channelId);
    this.dmInbox.push(...answer.received);
    return answer.received;
  }
  async waitForDmMembers(channelId: string, count: number): Promise<number> {
    let members = 0;
    await expect.poll(async () => {
      const answer = await this.driver.syncDm(channelId);
      this.dmInbox.push(...answer.received);
      members = answer.members;
      return members;
    }, { timeout: WAIT, intervals: [250, 500, 1000, 2000] }).toBe(count);
    return members;
  }
  async waitForDmMessage(channelId: string, body: string): Promise<PeerMessage> {
    await expect.poll(async () => {
      await this.syncDm(channelId);
      return this.dmInbox.some((m) => isPeerMessage(m) && m.body === body);
    }, { timeout: WAIT, intervals: [250, 500, 1000, 2000] }).toBe(true);
    const found = this.dmInbox.filter(isPeerMessage).find((m) => m.body === body);
    if (!found) throw new Error(`the peer never decrypted ${JSON.stringify(body)} in its DM`);
    return found;
  }

  close(): Promise<void> { return this.driver.close(); }
}

export function instanceInvite(): string {
  const codes = process.env.DILLA_TESTKIT_INVITE;
  if (!codes) throw new Error('DILLA_TESTKIT_INVITE is unset: the web global setup exports it from the test host banner');
  return codes.split(',')[0];
}

/** A short random suffix that makes a message body unique within the run. */
export function tag(): string { return randomInt(0, 0xff_ffff).toString(16).padStart(6, '0'); }

/** The author name the shell shows for the peer: its display name, else its username (task 24). */
export function shownName(s: PeerSetup): string { return s.display !== '' ? s.display : s.username; }

// ---------------------------------------------------------------------------------------------------
// The guard: no CSP violation, no console error, no uncaught error, in any page of the context and in
// its core worker. A dedicated worker takes its policy from its own script response (ruling 31), and its
// violations fire on the worker global, which the page's init script never reaches.

export interface CspViolation { directive: string; blockedURI: string; sourceFile: string; sample: string; }
/** Firefox reports its browser-chrome favicon request as a page CSP event with sourceFile="resource". */
export function isFirefoxBrowserFaviconViolation(projectName: string, v: CspViolation): boolean {
  if (!projectName.startsWith('firefox') || v.directive !== 'img-src' || v.sourceFile !== 'resource') return false;
  try {
    const url = new URL(v.blockedURI);
    return url.protocol === 'http:' && url.hostname === '127.0.0.1' && url.pathname === '/favicon.ico';
  } catch {
    return false;
  }
}
export interface Guard {
  readonly cspViolations: CspViolation[];
  readonly consoleErrors: string[];
  readonly pageErrors: string[];
  /** Refusals of an ordinary Welcome by the signed kind-3 context probe, with their exact request URLs. */
  readonly interactionAdmissionRefusals: string[];
  attach(context: BrowserContext): Promise<void>;
  /** Moves every violation the tracked workers recorded into cspViolations (and empties their lists). */
  collectWorkers(): Promise<void>;
  /** Waits for async response-body checks and console handling before teardown assertions. */
  flush(): Promise<void>;
}

function newGuard(projectName: string): Guard {
  const cspViolations: CspViolation[] = [];
  const consoleErrors: string[] = [];
  const pageErrors: string[] = [];
  const interactionAdmissionRefusals: string[] = [];
  const expectedForbidden = new WeakMap<Page, Map<string, { at: number; valid: Promise<boolean> }[]>>();
  const pending = new Set<Promise<unknown>>();
  const track = <T>(work: Promise<T>): Promise<T> => {
    pending.add(work);
    void work.then(() => pending.delete(work), () => pending.delete(work));
    return work;
  };
  const workers: Worker[] = [];
  // Per page: when its main frame last navigated or its window last resized, and the cancelled-fetch lines
  // counted in consoleErrors so far, so a navigation that follows the line (WebKit may log it first) can take it back.
  const disturbed = new WeakMap<Page, number>();
  const cancelled: { page: Page; at: number; text: string }[] = [];
  const disturb = (p: Page): void => {
    const now = Date.now();
    disturbed.set(p, now);
    for (let i = cancelled.length - 1; i >= 0; i--) {
      const c = cancelled[i];
      if (c.page !== p || now - c.at > ABORT_WINDOW_MS) continue;
      const at = consoleErrors.lastIndexOf(c.text);
      if (at >= 0) consoleErrors.splice(at, 1);
      cancelled.splice(i, 1);
    }
  };
  const watchWorker = (w: Worker): void => {
    workers.push(w);
    void w.evaluate(() => {
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
    p.on('framenavigated', (f) => { if (f === p.mainFrame()) disturb(p); });
    for (const w of p.workers()) watchWorker(w);
  };
  const consumeExpectedForbidden = async (page: Page, url: string): Promise<boolean> => {
    const responses = expectedForbidden.get(page)?.get(url);
    if (responses === undefined) return false;
    const now = Date.now();
    while (responses.length > 0 && now - responses[0]!.at > 3000) responses.shift();
    if (responses.length === 0) return false;
    return (await responses.shift()!.valid);
  };
  const handleConsole = async (m: ConsoleMessage): Promise<void> => {
    // Chromium and WebKit print every 4xx/5xx response as "Failed to load resource: …" (WebKit
    // measured on CI's first run: the expired session's 401 of the reload spec). Only the statuses the
    // client answers or retries by design are exempt: 401, 404, 409 and 410 (L-TS-02, L-TS-06) and the
    // paced 429 and 503. The 403 below is matched to its exact request URL and response body; any other
    // status or a line of another shape still fails the test (E2E-CI-01).
    // Firefox printed nothing for them and keeps the full guard.
    const resourceLine = (projectName === 'chromium-web' || projectName === 'webkit-web') && BY_DESIGN_RESOURCE_LINE.test(m.text());
    const interactionContextRefusal = (projectName === 'chromium-web' || projectName === 'webkit-web') && FORBIDDEN_RESOURCE_LINE.test(m.text());
    // web3.9 V-2, extended to the web suite: on a reload in a call, Firefox closes the signal socket before the
    // session's pagehide leave runs, and LiveKit logs this one line; the leave is then seen through the socket close.
    const firefoxBrowserAbort = projectName.startsWith('firefox') &&
      (ABORTED_FONT_LINE.test(m.text()) || ABORTED_JUGGLER_LINE.test(m.text()) ||
        FIREFOX_FAVICON_LINE.test(m.text()) ||
        m.text() === 'cannot send signal request before connected, type: leave JSHandle@object' ||
        // and Firefox's own line for the same reload closing the open signal socket (as the voice suite allows)
        FIREFOX_SOCKET_INTERRUPTED.test(m.text()));
    if (m.type() === 'error' && !resourceLine && !firefoxBrowserAbort) {
      const page = m.page();
      const requestURL = m.location().url;
      if (interactionContextRefusal && page !== null && requestURL !== '' &&
          await consumeExpectedForbidden(page, requestURL)) return;
      if (page !== null && isCancelledFetchLine(m.text(), projectName)) {
        if (isNavigationAbortLine(m.text(), projectName, Date.now(), disturbed.get(page) ?? null)) return;
        cancelled.push({ page, at: Date.now(), text: m.text() });
      }
      consoleErrors.push(m.text());
    }
  };
  return {
    cspViolations, consoleErrors, pageErrors, interactionAdmissionRefusals,
    async attach(context: BrowserContext): Promise<void> {
      await context.exposeBinding('__dillaCspViolation', (_source, v: CspViolation) => {
        if (!isFirefoxBrowserFaviconViolation(projectName, v)) cspViolations.push(v);
      });
      await context.exposeBinding('__dillaResized', (source) => { disturb(source.page); });
      await context.addInitScript(() => {
        window.addEventListener('resize', () => { (window as unknown as { __dillaResized(): void }).__dillaResized(); });
        document.addEventListener('securitypolicyviolation', (e) => {
          (window as unknown as { __dillaCspViolation(v: unknown): void }).__dillaCspViolation({
            directive: e.violatedDirective, blockedURI: e.blockedURI, sourceFile: e.sourceFile, sample: e.sample,
          });
        });
      });
      context.on('console', (m) => {
        track(handleConsole(m).catch((error: unknown) => { consoleErrors.push(`console guard failed: ${String(error)}`); }));
      });
      context.on('response', (response: Response) => {
        if ((projectName !== 'chromium-web' && projectName !== 'webkit-web') || response.status() !== 403) return;
        let page: Page;
        try { page = response.request().frame().page(); } catch { return; }
        const refusal = { at: Date.now(), valid: track(response.body().then((body) => {
          const valid = isExpectedInteractionAdmissionContextRefusal(response.status(), response.url(), body);
          if (valid) interactionAdmissionRefusals.push(response.url());
          return valid;
        }).catch(() => false)) };
        const byURL = expectedForbidden.get(page) ?? new Map<string, { at: number; valid: Promise<boolean> }[]>();
        const responses = byURL.get(response.url()) ?? [];
        responses.push(refusal);
        byURL.set(response.url(), responses);
        expectedForbidden.set(page, byURL);
      });
      context.on('weberror', (e) => { pageErrors.push(String(e.error())); });
      context.on('page', watchPage);
      for (const p of context.pages()) watchPage(p);
    },
    async collectWorkers(): Promise<void> {
      for (const w of workers) {
        const found = await w.evaluate(() => {
          const g = self as unknown as { __dillaCsp?: unknown[] };
          const out = g.__dillaCsp ?? [];
          g.__dillaCsp = [];
          return out;
        }).catch(() => [] as unknown[]);
        cspViolations.push(...(found as CspViolation[]));
      }
    },
    async flush(): Promise<void> {
      while (pending.size > 0) await Promise.allSettled([...pending]);
    },
  };
}

interface AppFixtures { guard: Guard; peer: Peer; }
interface AppWorkerFixtures { instanceName: string; }

export const test = persistentTest.extend<AppFixtures, AppWorkerFixtures>({
  // The instance name the onboarding shows in `Join {instance}` (L-COPY-01): element 5 of GET /v1/instance.
  // eslint-disable-next-line no-empty-pattern
  instanceName: [async ({}, use) => {
    const res = await fetch(`${PUBLIC_URL}/v1/instance`, { headers: { accept: 'application/cbor' } });
    expect(res.status, 'GET /v1/instance').toBe(200);
    const name = arr(decode(new Uint8Array(await res.arrayBuffer())))[5];
    if (typeof name !== 'string' || name === '') throw new Error('GET /v1/instance: element 5 is not the instance name');
    await use(name);
  }, { scope: 'worker' }],
  guard: [async ({ context }, use, testInfo) => {
    const guard = newGuard(testInfo.project.name);
    await guard.attach(context);
    await use(guard);
    await guard.collectWorkers();
    await guard.flush();
    expect(guard.cspViolations, 'securitypolicyviolation events').toEqual([]);
    expect(guard.pageErrors, 'uncaught page errors').toEqual([]);
    expect(guard.consoleErrors, 'console errors').toEqual([]);
  }, { auto: true }],
  // Task 17's relaunch, with the guard carried across it: the old context's workers are collected
  // before it closes, and the new context is watched from its first page on.
  relaunch: async ({ relaunch, guard }, use) => {
    await use(async () => {
      await guard.collectWorkers();
      const next = await relaunch();
      await guard.attach(next.context);
      return next;
    });
  },
  // eslint-disable-next-line no-empty-pattern
  peer: async ({}, use) => {
    const peer = await Peer.start();
    await use(peer);
    await peer.close();
  },
});

// ---------------------------------------------------------------------------------------------------
// Page handles (L-COPY-01, the L-UI DOM contract, the Global Constraints landmarks).

export function heading(page: Page, key: string, vars?: Record<string, string | number>): Locator {
  return page.getByRole('heading', { level: 1, name: copy(key, vars), exact: true });
}
export async function expectHeading(page: Page, key: string, vars?: Record<string, string | number>): Promise<void> {
  await expect(heading(page, key, vars)).toBeVisible({ timeout: WAIT });
}
/** Any onboarding step (all five render inside OnboardingFrame). */
export function onboardingFrame(page: Page): Locator { return page.locator(CLASS.onboardingFrame); }
/** `Continue`, the forward button of steps 1-3. */
export function nextButton(page: Page): Locator { return page.getByRole('button', { name: copy(COPY.next), exact: true }); }
/** `Create account`, the forward button of step 4 and the only control that registers (L-COPY-01). */
export function submitButton(page: Page): Locator { return page.getByRole('button', { name: copy(COPY.submit), exact: true }); }
export function railNav(page: Page): Locator { return page.getByRole('navigation', { name: copy(COPY.railLabel), exact: true }); }
export function channelsNav(page: Page): Locator { return page.getByRole('navigation', { name: copy(COPY.channelsLabel), exact: true }); }
export function logRegion(page: Page, channel: string): Locator {
  return page.getByRole('log', { name: copy(COPY.logLabel, { channel }), exact: true });
}
export function composerBox(page: Page, channel: string): Locator {
  return page.getByRole('textbox', { name: copy(COPY.composerLabel, { channel }), exact: true });
}
export function splash(page: Page, key: string): Locator { return page.getByRole('status').filter({ hasText: copy(key) }); }
export function messageRow(page: Page, channel: string, text: string): Locator {
  return logRegion(page, channel).locator(CLASS.messageRow).filter({ hasText: text });
}

// ---------------------------------------------------------------------------------------------------
// Flows.

export interface Account {
  username: string; display: string; password: string; recoveryKey: string[];
  /** web3.6: the control URL of the host the account lives on, the authenticator secret, the backup codes and the last TOTP counter used. */
  control: string; totpSecret: string | null; backupCodes: string[]; lastCounter: number;
}

export async function readRecoveryKey(page: Page): Promise<string[]> {
  return page.locator(`${CLASS.recoveryKey} li`).allInnerTexts();
}

/** A new account's details: a random username, the given display name and the suite's password. */
export function newAccount(display = 'web tester', control = CONTROL_URL): Account {
  return {
    username: `w${randomInt(0, 0xffff_ffff).toString(16).padStart(8, '0')}`, display, password: 'e2e-password-1234', recoveryKey: [],
    control, totpSecret: null, backupCodes: [], lastCounter: -1,
  };
}

/** The whole onboarding with the pointer; returns what the person would have written down. */
export async function signUp(page: Page, invite: string, instanceName: string, display = 'web tester', control = CONTROL_URL): Promise<Account> {
  const account = newAccount(display, control);
  await page.goto(`/welcome?invite=${encodeURIComponent(invite)}`);
  return completeSignUp(page, account, instanceName);
}

/** The five onboarding steps from the first one (`Join {instance}`), however the page got there: the invite link's
 *  landing page, or /welcome?invite=. Fills in `account` with what the person writes down. */
export async function completeSignUp(page: Page, account: Account, instanceName: string): Promise<Account> {
  await expectHeading(page, COPY.connectTitle, { instance: instanceName });
  await nextButton(page).click();
  await expectHeading(page, COPY.identityTitle);
  await page.getByLabel(copy(COPY.usernameLabel), { exact: true }).fill(account.username);
  await page.getByLabel(copy(COPY.displayLabel), { exact: true }).fill(account.display);
  await page.getByLabel(copy(COPY.passwordLabel), { exact: true }).fill(account.password);
  await nextButton(page).click();
  await expectHeading(page, COPY.keysTitle);
  account.recoveryKey = await readRecoveryKey(page);
  await page.getByRole('checkbox', { name: copy(COPY.keysAcknowledge), exact: true }).check();
  await nextButton(page).click();
  await expectHeading(page, COPY.deviceTitle);
  await submitButton(page).click();
  await completeFactorStep(page, account);
  await expectHeading(page, COPY.doneTitle);
  await page.getByRole('button', { name: copy(COPY.finish), exact: true }).click();
  await expectShell(page);
  return account;
}

/** web3.6 sign-up step 5: choose the authenticator app, copy the setup key, confirm with a code, tick the backup codes. */
export async function completeFactorStep(page: Page, account: Account, onStep?: (state: 'choose' | 'totp' | 'codes') => Promise<void>): Promise<void> {
  await expectHeading(page, 'factor.choose.title');
  await onStep?.('choose');
  await page.getByRole('radio', { name: new RegExp(escapeRegExp(copy('factor.choose.app'))) }).check();
  await page.getByRole('button', { name: copy('factor.choose.next'), exact: true }).click();
  await expectHeading(page, 'factor.totp.title');
  const key = page.getByLabel(copy('factor.totp.key'), { exact: true });
  await expect(key).toHaveValue(/^[A-Z2-7]{16,}$/, { timeout: WAIT });
  await onStep?.('totp');
  account.totpSecret = await key.inputValue();
  await page.getByLabel(copy('factor.totp.code'), { exact: true }).fill(await nextCode(account.control, account));
  await page.getByRole('button', { name: copy('factor.totp.submit'), exact: true }).click();
  await expectHeading(page, 'factor.codes.title');
  await onStep?.('codes');
  account.backupCodes = (await page.getByRole('list', { name: copy('factor.codes.label'), exact: true }).locator('li').allInnerTexts())
    .map((c) => c.replace(/\s+/g, ''));
  await page.getByRole('checkbox', { name: copy('factor.codes.ack'), exact: true }).check();
  await page.getByRole('button', { name: copy('factor.codes.next'), exact: true }).click();
}

/** The shell is up (the rail is present, even with no server) and no onboarding step is showing. */
export async function expectShell(page: Page): Promise<void> {
  await expect(railNav(page)).toBeVisible({ timeout: WAIT });
  await expect(onboardingFrame(page)).toHaveCount(0);
}

/** The rail's one "+" tile opens the add dialog (web3.9); its choice opens the existing join or create flow. */
export async function addServerVia(page: Page, choice: 'join' | 'create'): Promise<void> {
  await railNav(page).getByRole('button', { name: copy('layout.rail.add'), exact: true }).click();
  await page.getByRole('dialog', { name: copy('layout.add.title'), exact: true })
    .getByRole('button', { name: copy(choice === 'join' ? 'layout.add.join' : 'layout.add.create'), exact: true }).click();
}

export async function joinCommunity(page: Page, peer: Peer, setup: PeerSetup): Promise<void> {
  await addServerVia(page, 'join');
  const dialog = page.getByRole('dialog', { name: copy(COPY.joinTitle), exact: true });
  await expect(dialog).toBeVisible({ timeout: WAIT });
  await dialog.getByLabel(copy(COPY.joinInvite), { exact: true }).fill(setup.invite_code);
  await dialog.getByRole('button', { name: copy(COPY.joinSubmit), exact: true }).click();
  await expect(dialog).toBeHidden({ timeout: WAIT });
  await expect(railItem(page, peer.communityName)).toBeVisible({ timeout: WAIT });
}

/**
 * Opens a channel and waits until its composer has settled: ready to send, or blocked because a member's identity
 * changed. Web-2c requirement 1 keeps that block until the contact is verified, so a changed group never becomes
 * ready; any other refusal (a group that is not active yet) is still waited out, as before.
 */
export async function openChannel(page: Page, community: string, channel: string): Promise<void> {
  await railItem(page, community).click();
  await channelRow(page, channel).click();
  await expectComposerSettled(page, channel);
}

async function expectComposerSettled(page: Page, channel: string): Promise<void> {
  const box = composerBox(page, channel);
  await expect(box).toBeVisible({ timeout: WAIT });
  await expect.poll(async () => {
    if (await box.isEnabled()) return 'ready';
    const reason = await box.evaluate((el) => (el.getAttribute('aria-describedby') ?? '').split(/\s+/)
      .map((id) => (id === '' ? '' : document.getElementById(id)?.textContent ?? '')).join(' ').trim());
    return reason === copy('privacy.changed') || reason === copy('privacy.leafFailed') ? 'blocked by a trust verdict' : 'waiting';
  }, { timeout: WAIT }).not.toBe('waiting');
}

/** The composer is enabled only while the channel's group is active (task 24; aria-disabled otherwise). */
export async function expectComposerReady(page: Page, channel: string): Promise<void> {
  await expect(composerBox(page, channel)).toBeEnabled({ timeout: WAIT });
}

/** Sends through the composer and waits until the row is confirmed; a duplicate row fails strict mode. */
export async function sendText(page: Page, channel: string, text: string): Promise<void> {
  const box = composerBox(page, channel);
  await box.fill(text);
  await box.press('Enter');
  await expect(messageRow(page, channel, text)).toHaveAttribute('data-state', 'ok', { timeout: WAIT });
}

export async function expectAccessible(page: Page, state: string): Promise<void> {
  await page.evaluate(async () => {
    await document.fonts.ready;
    await Promise.all((document as unknown as { getAnimations(o: { subtree: boolean }): Animation[] }).getAnimations({ subtree: true })
      .filter((animation) => Number.isFinite(animation.effect?.getComputedTiming().endTime))
      .map((animation) => animation.finished.catch(() => undefined)));
  });
  const findings = await runAxe(page);
  expect(findings, `axe (serious and critical, target-size on) in state "${state}"`).toEqual([]);
}

/** Presses Tab until `target` is the focused element; fails after `maxPresses`. Keyboard only. */
export async function tabTo(page: Page, target: Locator, maxPresses = 40): Promise<void> {
  await expect(target).toBeVisible({ timeout: WAIT });
  for (let i = 0; i <= maxPresses; i++) {
    if (await target.evaluate((el) => el === document.activeElement)) return;
    if (i < maxPresses) await page.keyboard.press('Tab');
  }
  throw new Error(`Tab did not reach the target within ${maxPresses} presses`);
}

// web-2a (task 21): rows by name, the sign-in ceremony, Settings.
export function escapeRegExp(text: string): string { return text.replace(/[.*+?^${}()|[\]\\]/g, '\\$&'); }

/** A rail item or channel row by the name it shows: its accessible name is the name alone, or the name followed
 *  by the row's badge ("general slow mode", slow.badge) and by ", " and the counts (shell.rail.itemLabel, shell.channels.rowLabel, shell.channels.rowLabelMuted). */
export function namedRow(scope: Locator, name: string): Locator {
  return scope.getByRole('button', { name: new RegExp(`^${escapeRegExp(name)}( ${escapeRegExp(copy('slow.badge'))})?(, |$)`) });
}
export function railItem(page: Page, community: string): Locator { return namedRow(railNav(page), community); }
export function channelRow(page: Page, channel: string): Locator { return namedRow(channelsNav(page), channel); }

/** The "+" at the right of the channel list's own header (web3.8, item 11): "new channel" for a person who manages channels. */
export function newChannelButton(page: Page): Locator {
  return channelsNav(page).getByRole('button', { name: copy('create.channel.open'), exact: true });
}

/**
 * Server settings → Invites → "Make invite link" (Signal task 10c: the link shows once in the Invites card, with Copy, and
 * no dialog opens). Returns the settings dialog, which holds the link field ("Invite link") and Copy. `server` names the
 * server for the callers; the card does not repeat it.
 */
export async function openInviteDialog(page: Page, server: string): Promise<Locator> {
  void server;
  await page.getByRole('button', { name: copy('admin.open'), exact: true }).click().then(() => page.getByRole('menuitem', { name: copy('signalSettings.menu.serverSettings'), exact: true }).click());
  const settings = page.getByRole('dialog', { name: copy('admin.title'), exact: true });
  const nav = settings.getByRole('navigation', { name: copy('admin.nav'), exact: true });
  // Until the permissions are read the dialog shows no tab; the nav fills in when they arrive.
  await expect(nav).toBeVisible({ timeout: WAIT });
  const tab = nav.getByRole('button', { name: copy('admin.nav.invites'), exact: true });
  if ((await tab.getAttribute('aria-current')) !== 'page') await tab.click();
  await settings.getByRole('button', { name: copy('serverSettings.invites.make'), exact: true }).click();
  await expect(settings.getByRole('textbox', { name: copy('serverSettings.invites.link'), exact: true })).toBeVisible({ timeout: WAIT });
  return settings;
}

/** Makes an invite link under server settings → Invites, reads it from the card's field, and closes settings. */
export async function makeInviteLink(page: Page, server: string): Promise<string> {
  const settings = await openInviteDialog(page, server);
  const field = settings.getByRole('textbox', { name: copy('serverSettings.invites.link'), exact: true });
  await expect.poll(() => field.inputValue(), { timeout: WAIT }).toMatch(/^https?:\/\/[^/\s]+\/i\/[^/\s]+$/);
  const link = await field.inputValue();
  await page.keyboard.press('Escape');
  await expect(page.getByRole('dialog', { name: copy('admin.title'), exact: true })).toBeHidden();
  return link;
}

// Signal task 10c: server settings → Roles is an inline editor and Bot is a page of switches.
/** The role list's button for `name` ("{name}, {count} members"). */
export function roleButton(settings: Locator, name: string): Locator {
  return settings.getByRole('list', { name: copy('serverSettings.roles.list'), exact: true })
    .getByRole('button', { name: new RegExp(`^${escapeRegExp(name)}, \\d+ members?`) });
}
/** Picks the role `name` in the editor. */
export async function selectRole(settings: Locator, name: string): Promise<void> {
  await roleButton(settings, name).click();
  await expect(roleButton(settings, name)).toHaveAttribute('aria-current', 'true', { timeout: WAIT });
}
/** Sets a role permission switch (perm.<key>), which saves at once, and waits for it to stick. */
export async function setRolePermission(settings: Locator, key: string, on: boolean): Promise<void> {
  const toggle = settings.getByRole('switch', { name: copy(`perm.${key}`), exact: true });
  if ((await toggle.getAttribute('aria-checked')) !== String(on)) await toggle.click();
  await expect(toggle).toHaveAttribute('aria-checked', String(on), { timeout: WAIT });
}
/** "New role", then renames the new role (Enter saves) and sets its permissions and options; leaves it selected. */
export async function makeRole(page: Page, settings: Locator, name: string,
  options: { color?: string; perms?: readonly string[]; hoist?: boolean; mentionable?: boolean } = {}): Promise<void> {
  await settings.getByRole('button', { name: copy('serverSettings.roles.new'), exact: true }).click();
  const field = settings.getByRole('textbox', { name: copy('roles.editor.name'), exact: true });
  await expect(field).toHaveValue(copy('serverSettings.roles.newName'), { timeout: WAIT });
  await field.fill(name);
  await field.press('Enter');
  await expect(roleButton(settings, name)).toBeVisible({ timeout: WAIT });
  if (options.color !== undefined) await settings.getByRole('radio', { name: copy(`roles.color.${options.color}`), exact: true }).click();
  for (const key of options.perms ?? []) await setRolePermission(settings, key, true);
  for (const [item, on] of [['serverSettings.roles.menu.hoist', options.hoist], ['serverSettings.roles.menu.mentionable', options.mentionable]] as const) {
    if (on !== true) continue;
    await roleButton(settings, name).focus();
    await page.keyboard.press('Shift+F10');
    const check = page.getByRole('menuitemcheckbox', { name: copy(item), exact: true });
    await check.click();
    await expect(check).toHaveAttribute('aria-checked', 'true', { timeout: WAIT });
    await page.keyboard.press('Escape');
  }
}
/** Bot page: switches the capabilities on (bot.cap.<name>), then Enable (step-up follows in the caller). */
export async function chooseBotCapabilities(settings: Locator, names: readonly string[]): Promise<void> {
  for (const name of names) {
    const toggle = settings.getByRole('switch', { name: copy(`serverSettings.bot.cap.${name}`), exact: true });
    if ((await toggle.getAttribute('aria-checked')) !== 'true') await toggle.click();
    await expect(toggle).toHaveAttribute('aria-checked', 'true');
  }
}
/** The Bot page's Enable (on Glenn's row). */
export function enableBotButton(settings: Locator): Locator {
  return settings.getByRole('button', { name: copy('serverSettings.bot.enable'), exact: true });
}
/** Glenn's row menu (Shift+F10 on the row): picks `itemKey` (serverSettings.bot.menu.*; Disable carries the bot's name). */
export async function botRowMenu(page: Page, settings: Locator, itemKey: string): Promise<void> {
  const row = settings.locator('[role="group"][aria-keyshortcuts="Shift+F10"]').first();
  await row.focus();
  await page.keyboard.press('Shift+F10');
  const [before, after] = (en as Record<string, string>)[itemKey].split('{bot}');
  const name = after === undefined ? copy(itemKey) : new RegExp(`^${escapeRegExp(before)}.+${escapeRegExp(after)}$`);
  await page.getByRole('menuitem', { name }).click();
}

/** The recovery key as written down: its 13 groups joined by `separator`. */
export function keyText(account: Account, separator = ' '): string { return account.recoveryKey.join(separator); }
/** A well-formed key that is not this account's: the first character replaced by another character of the alphabet. */
export function wrongKey(account: Account): string {
  const key = account.recoveryKey.join('');
  return `${key[0] === '0' ? '1' : '0'}${key.slice(1)}`;
}
export function keyField(page: Page): Locator { return page.getByLabel(copy(SIGNIN.keyLabel), { exact: true }); }
export function keySubmit(page: Page): Locator { return page.getByRole('button', { name: copy(SIGNIN.keySubmit), exact: true }); }

/** From a fresh profile to the key step (web3.8, R6): onboarding step 1's entry, the login step (username and password),
 *  then the code when the account has an app (web3.6); ends on the key step. Nothing registers before the key. */
export async function signInToKeyStep(page: Page, account: Account, instanceName: string): Promise<void> {
  await page.goto('/welcome');
  await expectHeading(page, COPY.connectTitle, { instance: instanceName });
  await page.getByRole('button', { name: copy(SIGNIN.entry), exact: true }).click();
  await expectHeading(page, SIGNIN.loginTitle, { instance: instanceName });
  await page.getByLabel(copy(SIGNIN.username), { exact: true }).fill(account.username);
  await page.getByLabel(copy(SIGNIN.password), { exact: true }).fill(account.password);
  await page.getByRole('button', { name: copy(SIGNIN.loginSubmit), exact: true }).click();
  await expect(heading(page, 'signin.totp.title').or(heading(page, SIGNIN.keyTitle))).toBeVisible({ timeout: WAIT });
  if (await heading(page, 'signin.totp.title').isVisible()) {
    await page.getByLabel(copy('signin.totp.code'), { exact: true }).fill(await nextCode(account.control, account));
    await page.getByRole('button', { name: copy(SIGNIN.loginSubmit), exact: true }).click();
  }
  await expectHeading(page, SIGNIN.keyTitle);
}
/** The key step to the shell: the key as typed, `Add this browser` (which registers and enrols), the done step,
 *  `Open dilla`. */
export async function finishWithKey(page: Page, account: Account, instanceName: string, typed = keyText(account)): Promise<void> {
  await keyField(page).fill(typed);
  await keySubmit(page).click();
  await expectHeading(page, SIGNIN.doneTitle);
  await expect(page.getByText(copy(SIGNIN.doneBody, { username: account.username, instance: instanceName }), { exact: true })).toBeVisible();
  await page.getByRole('button', { name: copy(SIGNIN.doneNext), exact: true }).click();
  await expectShell(page);
}
export async function signIn(page: Page, account: Account, instanceName: string, typed = keyText(account)): Promise<void> {
  await signInToKeyStep(page, account, instanceName);
  await finishWithKey(page, account, instanceName, typed);
}

export type SettingsSection = 'profile' | 'signin' | 'devices' | 'notifications' | 'appearance' | 'voice' | 'privacy' | 'keyboard' | 'diagnostics';
export function settingsDialog(page: Page): Locator { return page.getByRole('dialog', { name: copy('settings.title'), exact: true }); }
/** The user panel (desktop) or the You tab settings row (phone). */
export function userPanel(page: Page): Locator { return page.locator('.d-user-panel'); }
/** Desktop Settings opener: the top bar's gear (Signal §5.2; the user panel has none, §5.4). Phone settings are opened from You → Open settings. */
export function settingsGear(page: Page): Locator { return page.locator('.d-topbar').getByRole('button', { name: copy('workspace.open'), exact: true }); }
/** Ruling X (Jonas 2026-10-27, pixel parity): the channel header has no "⋯"; a text channel's pins and edit channel are in its
 *  row's context menu. This right-clicks the row and picks the item by its copy key; focus returns to the row afterwards. */
export async function channelRowMenuItem(page: Page, channel: string, key: string): Promise<void> {
  await channelRow(page, channel).click({ button: 'right' });
  await page.getByRole('menu', { name: copy('nav.channel.menuOf', { name: channel }), exact: true }).getByRole('menuitem', { name: copy(key), exact: true }).click();
}
/** The channel header's Members button (named "Members" or "Members, {n}"), on a desktop or a phone. */
export function membersToggle(page: Page): Locator { return page.locator('.d-channel-header [data-members-toggle], .d-phone-header [data-members-toggle]'); }
/** Opens the requested section through the visible workspace navigation at the current viewport. */
export async function openSettings(page: Page, section: SettingsSection): Promise<Locator> {
  const phone = await page.evaluate(() => matchMedia('(max-width: 47.99rem), (pointer: coarse) and (max-height: 31.99rem)').matches);
  if (phone) {
    const tabs = page.getByRole('navigation', { name: copy('nav.tabs.label'), exact: true });
    const you = tabs.getByRole('link', { name: copy('nav.tab.you'), exact: true });
    if (await you.count() > 0 && await you.getAttribute('aria-current') !== 'page') {
      await you.click();
      await expect(page).toHaveURL(/\/you$/, { timeout: WAIT });
    }
    // Task 11 (Q3): the You tab's Settings row.
    const open = page.getByRole('button', { name: copy('you.settings'), exact: true });
    await expect(open).toBeVisible({ timeout: WAIT });
    await open.click();
  } else {
    await settingsGear(page).click();
  }
  const dialog = settingsDialog(page);
  await expect(dialog).toBeVisible({ timeout: WAIT });
  const names: Record<SettingsSection, string> = {
    profile: 'workspace.profile', signin: 'workspace.signin', devices: 'workspace.devices',
    notifications: 'workspace.notifications', appearance: 'workspace.appearance', voice: 'workspace.voice',
    privacy: 'workspace.privacy', keyboard: 'workspace.keyboard', diagnostics: 'workspace.diagnostics',
  };
  await dialog.getByRole('navigation', { name: copy('settings.nav.label'), exact: true })
    .getByRole('button', { name: copy(names[section]), exact: true }).click();
  await expect(page).toHaveURL(new RegExp(`/settings/${section}$`), { timeout: WAIT });
  return dialog;
}
/** Escape closes Settings and returns to the route it was opened from (L-TS-27). */
export async function closeSettings(page: Page): Promise<void> {
  // A viewport change reaches the app a tick late: Escape waits until the shell's data-layout agrees with the window.
  await page.waitForFunction(() => {
    const shell = document.querySelector('.d-app-shell');
    const phone = window.matchMedia('(max-width: 47.99rem), (pointer: coarse) and (max-height: 31.99rem)').matches;
    return shell === null || (shell.getAttribute('data-layout') === 'phone') === phone;
  }, undefined, { timeout: WAIT });
  await page.keyboard.press('Escape');
  await expect(settingsDialog(page)).toBeHidden({ timeout: WAIT });
}
export function deviceRows(page: Page): Locator { return settingsDialog(page).locator(CLASS.deviceRow); }

// ---------------------------------------------------------------------------------------------------
// Probes.

export interface KekSummary {
  count: number; keys: string[]; v: number; type: string; extractable: boolean;
  algorithm: string; length: number; usages: string[]; iv: number; ct: number; exportRefused: boolean;
}

/** Reads the L-TS-05 record from the page's own origin and tries to export its wrap key. */
export async function readKekRecord(page: Page): Promise<KekSummary> {
  return page.evaluate(async () => {
    const db = await new Promise<IDBDatabase>((done, fail) => {
      const open = indexedDB.open('dilla-device', 1);
      open.onsuccess = () => done(open.result);
      open.onerror = () => fail(open.error);
    });
    const read = <T,>(req: IDBRequest<T>) => new Promise<T>((done, fail) => {
      req.onsuccess = () => done(req.result);
      req.onerror = () => fail(req.error);
    });
    const store = db.transaction('kek', 'readonly').objectStore('kek');
    const [keys, records] = await Promise.all([read(store.getAllKeys()), read(store.getAll())]);
    db.close();
    const rec = records[0] as { v: number; wrapKey: CryptoKey; iv: Uint8Array; ct: Uint8Array };
    let exportRefused = false;
    try { await crypto.subtle.exportKey('raw', rec.wrapKey); } catch { exportRefused = true; }
    return {
      count: records.length,
      keys: keys.map(String),
      v: rec.v,
      type: rec.wrapKey.type,
      extractable: rec.wrapKey.extractable,
      algorithm: rec.wrapKey.algorithm.name,
      length: (rec.wrapKey.algorithm as AesKeyAlgorithm).length,
      usages: [...rec.wrapKey.usages].sort(),
      iv: rec.iv.byteLength,
      ct: rec.ct.byteLength,
      exportRefused,
    };
  });
}

/** Deletes every KEK record; the encrypted OPFS store stays behind (the store-lost condition). */
export async function clearKek(page: Page): Promise<void> {
  await page.evaluate(() => new Promise<void>((done, fail) => {
    const open = indexedDB.open('dilla-device', 1);
    open.onerror = () => fail(open.error);
    open.onsuccess = () => {
      const db = open.result;
      const tx = db.transaction('kek', 'readwrite');
      tx.objectStore('kek').clear();
      tx.oncomplete = () => { db.close(); done(); };
      tx.onerror = () => fail(tx.error);
    };
  }));
}

/** Moves the test host's fake clock forward (internal/dillad/dilladtest/seed.go:35-51). */
export async function advanceClock(seconds: number): Promise<void> {
  const res = await fetch(`${CONTROL_URL}/debug/clock`, {
    method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ seconds }),
  });
  expect(res.status, await res.text()).toBe(204);
}

export interface ManifestFile { path: string; sha256: string; size: number; }
/** The manifest of the build the test host serves (-web-root packages/web/dist). */
export function readManifest(): { v: number; files: ManifestFile[] } {
  return JSON.parse(readFileSync(resolve(REPO_ROOT, 'packages', 'web', 'dist', 'dilla-manifest.json'), 'utf8')) as { v: number; files: ManifestFile[] };
}

// web3.9: the `@` Home tile, the rail's first item, is the way to the direct messages (there are no sidebar tabs).
/** The `@` tile. Its name is `home.tile`, followed by ", unread N, mentions M" while it carries a count; `aria-current="page"`
 *  while the DM list shows, and `data-unread` / `data-mentions` hold its counts. */
export function homeTile(page: Page): Locator {
  return railNav(page).getByRole('button', { name: new RegExp(`^${escapeRegExp(copy('home.tile'))}(,|$)`) });
}
export function dmsNav(page: Page): Locator { return page.getByRole('navigation', { name: copy('shell.dms.label'), exact: true }); }
/** The one message log on screen (a DM's log is named by the DM's name, which the spec does not fix). */
export function dmLog(page: Page): Locator { return page.getByRole('log'); }
export function dmComposer(page: Page): Locator { return page.locator(CLASS.composer).getByRole('textbox'); }

export interface ShownNotification { title: string; body: string; tag: string; closed: boolean; }
type Recorded = EventTarget & ShownNotification & { onclick: ((this: Recorded, e: Event) => unknown) | null; close(): void; click(): void };
type RecorderWindow = Window & { __dillaNotifications: Recorded[]; __dillaPermissionRequests: number };

/** Replaces window.Notification, before any page script runs, by a recorder that shows nothing: each
 *  `new Notification(title, options)` returns a stand-in with `onclick` and `close()` and is recorded;
 *  `permission` reads the browser's; `requestPermission` is counted and forwarded. An init script is
 *  injected by the protocol, so the page's CSP does not govern it. Call before the first navigation. */
export async function recordNotifications(context: BrowserContext): Promise<void> {
  await context.addInitScript(() => {
    const native = window.Notification;
    const w = window as unknown as RecorderWindow;
    w.__dillaNotifications = [];
    w.__dillaPermissionRequests = 0;
    const Recording = function (title: string, options?: NotificationOptions): Recorded {
      const n = new EventTarget() as Recorded;
      n.title = title;
      n.body = options?.body ?? '';
      n.tag = options?.tag ?? '';
      n.closed = false;
      n.onclick = null;
      n.close = () => { n.closed = true; };
      n.click = () => { const e = new Event('click'); n.onclick?.call(n, e); n.dispatchEvent(e); };
      w.__dillaNotifications.push(n);
      return n;
    };
    Object.defineProperty(Recording, 'permission', { get: () => native.permission });
    Object.defineProperty(Recording, 'requestPermission', {
      value: (...args: Parameters<typeof Notification.requestPermission>) => {
        w.__dillaPermissionRequests += 1;
        return native.requestPermission(...args);
      },
    });
    Object.defineProperty(window, 'Notification', { value: Recording, configurable: true, writable: true });
  });
}
export async function notificationsShown(page: Page): Promise<ShownNotification[]> {
  return page.evaluate(() => (window as unknown as RecorderWindow).__dillaNotifications
    .map((n) => ({ title: n.title, body: n.body, tag: n.tag, closed: n.closed })));
}
export async function permissionRequests(page: Page): Promise<number> {
  return page.evaluate(() => (window as unknown as RecorderWindow).__dillaPermissionRequests);
}
/** What a click on the desktop notification does: the page's own onclick handler runs. */
export async function clickNotification(page: Page, index: number): Promise<void> {
  await page.evaluate((i) => { (window as unknown as RecorderWindow).__dillaNotifications[i].click(); }, index);
}

export type NotifyDefault = 'dmsMentions' | 'everything' | 'nothing';
/**
 * Settings → Notifications → the selected server's mode (Signal task 10 fix round 1: the prototype's row replaced the device
 * default; for the server's channels the two mean the same). Waits for the radio to be checked, then closes.
 */
export async function setNotifyDefault(page: Page, mode: NotifyDefault): Promise<void> {
  const dialog = await openSettings(page, 'notifications');
  const key = mode === 'everything' ? 'notify.mode.all' : mode === 'nothing' ? 'notify.mode.nothing' : 'notify.mode.mentions';
  const radio = dialog.getByRole('radiogroup', { name: new RegExp(`^${copy('notify.server.hint')} `) })
    .getByRole('radio', { name: copy(key), exact: true });
  await radio.click();
  await expect(radio).toHaveAttribute('aria-checked', 'true', { timeout: WAIT });
  await closeSettings(page);
}

// web-2b conversation page contract.
export function rowById(page: Page, channel: string, msgId: string): Locator {
  return logRegion(page, channel).locator(`${CLASS.messageRow}[data-msg-id="${msgId}"]`);
}
export function rowToolbar(row: Locator): Locator {
  return row.getByRole('toolbar', { name: copy('shell.message.toolbar'), exact: true });
}
/** Task 7 (Signal §5.5, Ruling U): edit, pin, unpin, delete and remove-for-everyone sit in the toolbar's "more" menu. */
export type RowMenuAction = 'edit' | 'pin' | 'unpin' | 'delete' | 'moderateRemove';
const MENU_ACTIONS: ReadonlySet<string> = new Set<RowMenuAction>(['edit', 'pin', 'unpin', 'delete', 'moderateRemove']);
export function rowMore(row: Locator): Locator {
  return rowToolbar(row).getByRole('button', { name: copy('shell.message.more'), exact: true });
}
export function rowMenuItem(row: Locator, action: RowMenuAction): Locator {
  return rowToolbar(row).getByRole('menuitem', { name: copy(`shell.message.${action}`), exact: true });
}
export async function rowAction(row: Locator, action: 'react' | 'reply' | RowMenuAction): Promise<void> {
  await row.focus();
  if (!MENU_ACTIONS.has(action)) {
    await rowToolbar(row).getByRole('button', { name: copy(`shell.message.${action}`), exact: true }).click();
    return;
  }
  if (await rowMore(row).getAttribute('aria-expanded') !== 'true') await rowMore(row).click();
  await rowMenuItem(row, action as RowMenuAction).click();
}
/** Whether the row's "more" menu offers `action` (retried until `timeout`); the menu is closed again after. */
export async function expectRowMenuAction(row: Locator, action: RowMenuAction, offered: boolean, timeout = WAIT): Promise<void> {
  await expect(async () => {
    await row.focus();
    if (await rowMore(row).count() === 0) { expect(offered).toBe(false); return; }
    if (await rowMore(row).getAttribute('aria-expanded') !== 'true') await rowMore(row).click();
    await expect(rowMenuItem(row, action)).toHaveCount(offered ? 1 : 0, { timeout: 1_000 });
  }).toPass({ timeout });
  if (await rowMore(row).count() > 0 && await rowMore(row).getAttribute('aria-expanded') === 'true') await row.page().keyboard.press('Escape');
}
export function reactionChip(row: Locator, emojiName: string, count: number): Locator {
  return row.getByRole('button', { name: copy('shell.message.reaction', { name: emojiName, count }), exact: true });
}
export function editorBox(page: Page): Locator { return page.getByRole('textbox', { name: copy('shell.edit.label'), exact: true }); }
export function mentionList(page: Page): Locator { return page.getByRole('listbox', { name: copy('shell.composer.mentions'), exact: true }); }
export function trayEntries(page: Page): Locator {
  return page.getByRole('list', { name: copy('shell.tray.label'), exact: true }).getByRole('listitem');
}
export function attachInput(page: Page): Locator { return page.locator('input.dw-attach-input'); }
export function lightbox(page: Page): Locator { return page.locator(CLASS.lightbox); }
export function pinsDialog(page: Page, channel: string): Locator {
  return page.getByRole('dialog', { name: copy('shell.pins.title', { channel }), exact: true });
}
export function deleteDialog(page: Page): Locator { return page.getByRole('dialog', { name: copy('shell.delete.title'), exact: true }); }
export function sha256Hex(bytes: Uint8Array): string { return createHash('sha256').update(bytes).digest('hex'); }

export interface DrawnImage { bytes: Buffer; sha256: string; type: string }
export async function drawImage(page: Page, o: { width: number; height: number; type: 'image/png' | 'image/webp'; quality?: number; seed: number }): Promise<DrawnImage> {
  const r = await page.evaluate(async ({ width, height, type, quality, seed }) => {
    const canvas = document.createElement('canvas');
    canvas.width = width;
    canvas.height = height;
    const g = canvas.getContext('2d');
    if (g === null) throw new Error('no 2d context');
    for (let y = 0; y < height; y += 20) {
      for (let x = 0; x < width; x += 20) {
        g.fillStyle = `hsl(${(x * 7 + y * 3 + seed * 41) % 360} 60% 50%)`;
        g.fillRect(x, y, 20, 20);
      }
    }
    const blob = await new Promise<Blob | null>((done) => canvas.toBlob(done, type, quality));
    if (blob === null) throw new Error('canvas.toBlob returned null');
    const bytes = new Uint8Array(await blob.arrayBuffer());
    const digest = new Uint8Array(await crypto.subtle.digest('SHA-256', bytes));
    let bin = '';
    for (let i = 0; i < bytes.length; i += 0x8000) bin += String.fromCharCode(...bytes.subarray(i, i + 0x8000));
    return { b64: btoa(bin), sha256: Array.from(digest, (b) => b.toString(16).padStart(2, '0')).join(''), type: blob.type };
  }, o);
  if (r.type !== o.type) throw new Error(`the browser encoded ${r.type}, not ${o.type}`);
  return { bytes: Buffer.from(r.b64, 'base64'), sha256: r.sha256, type: r.type };
}

type FlashWindow = Window & { __dillaFlash?: { seen: boolean; stop(): void } };
export async function watchFlash(page: Page, msgId: string): Promise<void> {
  await page.evaluate((id) => {
    const w = window as unknown as FlashWindow;
    w.__dillaFlash?.stop();
    const state = { seen: false, stop: () => undefined as void };
    const observer = new MutationObserver(() => {
      if (document.querySelector(`.d-message-row[data-msg-id="${id}"][data-flash="true"]`) !== null) state.seen = true;
    });
    state.stop = () => observer.disconnect();
    observer.observe(document.body, { subtree: true, attributes: true, attributeFilter: ['data-flash'] });
    w.__dillaFlash = state;
  }, msgId);
}
export async function flashSeen(page: Page): Promise<boolean> {
  return page.evaluate(() => {
    const w = window as unknown as FlashWindow;
    const seen = w.__dillaFlash?.seen ?? false;
    if (seen) w.__dillaFlash?.stop();
    return seen;
  });
}
