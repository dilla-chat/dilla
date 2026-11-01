// web3.9 E-GEO: the layout grammar of the alignment pass, measured in a real browser. From an empty host (with its in-process
// LiveKit) Ada starts "Midgard Crew" and gives it two categories, text channels, an announcement channel, a voice channel with
// a live person in it, the built-in AFK channel and a direct message; then every rule of the grammar is read as a rect at
// 1440x900, 1024x768, 390x844 and 360x740 (the phone sizes on a touch phone context where the engine has one):
//   A5  every list's names share one left x           A4  every row's trailing slot shares one right edge
//   B1  every bar's bottom hairline is at y=44+44     R9  phone targets: primary 44x44, secondary 24x24
//   C1  at 1024 the header keeps its name             R22 the rail is 56 wide, its tiles 38
//   R23 a collapsed category keeps its current and unread rows, and nothing in `main` leaves its column sideways.
// Assertions are soft: one run names every broken rule at every size, and a control that is missing counts as a broken rule
// too. The world is always built through the current client; DILLA_E2E_CHECK_WEBROOT, a directory holding another build of
// the web client (the web3.8 build, for the red runs the task's report records), makes the host restart on it, over the same
// data and the same browsers, before the rules are read.
import { tmpdir } from 'node:os';
import { join, relative, resolve } from 'node:path';
import { expect, type BrowserContext, type Page } from '@playwright/test';
import { addServerVia, REPO_ROOT, attachInput, drawImage, messageRow, trayEntries,
  TEST_TIMEOUT, WAIT, channelRow, channelsNav, closeSettings, completeSignUp, copy, dmsNav, escapeRegExp, expectAccessible, expectComposerReady, homeTile, makeInviteLink, newAccount, openSettings, railItem, railNav,
  sendText, settingsDialog, signIn, signUp, test, isExpectedInteractionAdmissionContextRefusal, type Account, membersToggle,
} from './support/app';
import { launchSecond, secondProfileDir } from './support/second-harness';
import { PROFILE_ROOT } from './support/persistent';
import { removeHostData, startTestHost, type HostConfig, type StartedHost } from './support/host';
import { chromiumMediaArgs, FIREFOX_MEDIA_PREFS, writeFixtures } from '../media/support/fixtures';
import { arr, decode, encode } from '../../packages/client-core/src/cbor/index';
import { mkdirSync, rmSync } from 'node:fs';

const CHECK_WEBROOT = process.env.DILLA_E2E_CHECK_WEBROOT === undefined ? null : relative(REPO_ROOT, resolve(process.env.DILLA_E2E_CHECK_WEBROOT));
const LAYOUT_PORT = Number(process.env.DILLA_E2E_LAYOUT_PORT ?? 8555);
const LAYOUT_SFU_PORT = Number(process.env.DILLA_E2E_LAYOUT_SFU_PORT ?? 8559);
const EMPTY: HostConfig = { port: LAYOUT_PORT, control: LAYOUT_PORT + 1, webRoot: 'packages/web/dist', sfu: { port: LAYOUT_SFU_PORT, udpPort: LAYOUT_SFU_PORT + 2 }, ...(CHECK_WEBROOT === null ? {} : { keepData: true }) };
const SERVER = 'Midgard Crew';
const OWNER = 'Ada';
const FRIEND = 'Mira';
/** The two categories and what stands in them. */
const PLAN = 'planning';
const LOUNGE = 'lounge';
const RAID = 'raid-planning-for-the-bridge-troll-and-the-long-winter-campaign';
const NEWS = 'news';
const HARBOUR = 'harbour';
const LONG_WORD = 'unbroken'.repeat(24);
const SIZES = [
  { tag: '1440x900', width: 1440, height: 900, phone: false },
  { tag: '1024x768', width: 1024, height: 768, phone: false },
  { tag: '390x844', width: 390, height: 844, phone: true },
  { tag: '360x740', width: 360, height: 740, phone: true },
  { tag: '320x740', width: 320, height: 740, phone: true },
  { tag: '720x450 keyboard viewport', width: 720, height: 450, phone: true },
] as const;
type Size = (typeof SIZES)[number];
const PHONE = { isMobile: true, hasTouch: true, deviceScaleFactor: 2 } as const;

test('the guard accepts only the expected refusal of an ordinary Welcome context probe', () => {
  const group = '3ee4e8578f407046d67ab993704ad745';
  const url = `http://127.0.0.1:${String(LAYOUT_PORT)}/v1/groups/${group}/interaction-admission-context`;
  const refusal = encode(['E_FORBIDDEN', 'current interaction context unavailable', null]);
  expect(isExpectedInteractionAdmissionContextRefusal(403, url, refusal)).toBe(true);
  expect(isExpectedInteractionAdmissionContextRefusal(403, `${url}?other=1`, refusal)).toBe(false);
  expect(isExpectedInteractionAdmissionContextRefusal(403, url.replace('/v1/groups/', '/v1/channels/'), refusal)).toBe(false);
  expect(isExpectedInteractionAdmissionContextRefusal(403, url.replace('127.0.0.1', 'example.com'), refusal)).toBe(false);
  expect(isExpectedInteractionAdmissionContextRefusal(403, url, encode(['E_FORBIDDEN', 'missing permission', null]))).toBe(false);
  expect(isExpectedInteractionAdmissionContextRefusal(401, url, refusal)).toBe(false);
});

let host: StartedHost | null = null;
let instance = '';

// The two tests are independent (each makes its own accounts on the shared host), so one failing does not skip the other.
test.describe.configure({ mode: 'default', timeout: TEST_TIMEOUT * 4 });
// An action waits as long as one state may (the config leaves actions unbounded), so a missing control fails the check with time left.
test.use({ baseURL: `http://127.0.0.1:${String(EMPTY.port)}`, actionTimeout: WAIT });

test.beforeAll(async () => {
  if (CHECK_WEBROOT !== null) removeHostData(EMPTY.port);
  host = await startTestHost(EMPTY);
  const res = await fetch(`${host.url}/v1/instance`, { headers: { accept: 'application/cbor' } });
  expect(res.status, 'GET /v1/instance').toBe(200);
  const name = arr(decode(new Uint8Array(await res.arrayBuffer())))[5];
  if (typeof name !== 'string' || name === '') throw new Error('GET /v1/instance: element 5 is not the instance name');
  instance = name;
});
test.afterAll(async () => { await host?.stop(); if (CHECK_WEBROOT !== null) removeHostData(EMPTY.port); });

function started(): StartedHost {
  if (host === null) throw new Error('the empty test host did not start');
  return host;
}

// ---------------------------------------------------------------------------------------------------
// Measuring.

interface Box { x: number; y: number; w: number; h: number; r: number; b: number; t: string }
/** The rects of every rendered element matching `selector` (inside the first `root` when given), as the page sees them. */
async function boxes(page: Page, selector: string, root?: string): Promise<Box[]> {
  return page.evaluate(({ selector: sel, root: scope }) => {
    const top = scope === undefined ? document : document.querySelector(scope);
    if (top === null) return [];
    return [...top.querySelectorAll(sel)].filter(e => e.getClientRects().length > 0).map(e => {
      const r = e.getBoundingClientRect();
      return { x: r.x, y: r.y, w: r.width, h: r.height, r: r.right, b: r.bottom, t: (e.getAttribute('aria-label') ?? e.textContent ?? '').trim().slice(0, 32) };
    });
  }, { selector, root });
}

const TOL = 0.5;
/** Every value lies within TOL of the others; the failure names the rule, the size and each value. */
function expectOne(what: string, values: readonly number[], labels: readonly string[]): void {
  expect.soft(values.length, `${what}: something to measure`).toBeGreaterThan(0);
  if (values.length === 0) return;
  const spread = Math.max(...values) - Math.min(...values);
  expect.soft(spread, `${what}: ${values.map((v, i) => `${labels[i] ?? '?'}=${v.toFixed(1)}`).join(' | ')}`).toBeLessThanOrEqual(TOL);
}
function expectAt(what: string, got: number, want: number, tol = 1): void {
  expect.soft(Math.abs(got - want), `${what}: ${got.toFixed(1)}, want ${String(want)}`).toBeLessThanOrEqual(tol);
}

/** A group of rules: a control it needs that is missing, or any other thrown failure, is one more broken rule, not the end of the run. */
async function attempt(what: string, body: () => Promise<void>): Promise<void> {
  try { await body(); } catch (e) { expect.soft(String(e).split('\n').slice(0, 3).join(' ').slice(0, 300), `${what}: could not be measured`).toBe(''); }
}

/** The first list's names share one x, and its trailing slots one right edge (A5, A4). */
async function expectListGrammar(page: Page, tag: string, list: string, names: string, label: string): Promise<void> {
  const n = await boxes(page, names, list);
  expectOne(`A5 ${label} names share one left x at ${tag}`, n.map(b => b.x), n.map(b => b.t));
  const slots = await boxes(page, '.d-slot', list);
  expectOne(`A4 ${label} trailing slots share one right edge at ${tag}`, slots.map(b => b.r), slots.map((b, i) => `${String(i)}:${b.t || '-'}`));
}

/** Every visible control that takes a tap or a click. */
const CONTROLS = 'button, a[href], input:not([type="hidden"]), select, textarea, [role="button"], [role="tab"], [role="menuitem"], [role="switch"], [role="checkbox"], [role="radio"]';
/** The controls a thumb works all day: 44x44 on a phone (R9). Everything else is secondary (24x24, with the 2.5.8 spacing). Jonas
 *  2026-10-27 (pixel parity): the rail's tiles (38) and the phone header's actions (28 high; search 36 x 28) are the prototype's sizes,
 *  so they are secondary; the header's back and call dot stay 44. */
const PRIMARY = [
  // Fix round 2 (rule 0): a category head is the prototype's 26px on a phone too, so it is secondary.
  '.d-tab-bar__link', '.d-phone-header__back', '.d-phone-header__dot', '.d-chrow', '.d-voice-row', '.d-voice-row__join button',
  // Ruling AB: the server head's ⌄ is the prototype's 26px (secondary tier); the DM head's "message someone" stays primary.
  // Task 7 (rule 0): the composer's send (28) and footer tools (26 x 24) are the prototype's sizes, so they are secondary.
  '.d-channel-list__cat-add button', '.d-channel-list__head button:not(.d-channel-list__head-btn)',
  // Task 8 (rule 0): the stage's call bar is the prototype's 44 x 40 controls (Signal §5.6), so it is secondary; the DM call box
  // (no call bar) keeps the 44px check.
  '.d-call-bar button', '.d-call-stage__controls:not([data-bar]) button',
].join(', ');

async function expectTargets(page: Page, tag: string, state: string): Promise<void> {
  const small = await page.evaluate(({ controls, primary }) => {
    const out: { name: string; w: number; h: number; tier: number }[] = [];
    for (const el of document.querySelectorAll<HTMLElement>(controls)) {
      if (el.closest('[inert], [aria-hidden="true"], .d-sr-only') !== null) continue;
      const cs = getComputedStyle(el);
      if (cs.visibility === 'hidden' || cs.display === 'inline' || el.getClientRects().length === 0) continue;
      if (el instanceof HTMLInputElement && ['checkbox', 'radio'].includes(el.type) && el.closest('label') !== null) continue;
      const box = el.getBoundingClientRect();
      if (box.width <= 1 || box.height <= 1) continue;
      // A control that reaches out by an absolutely placed ::after (the author name and the mention pill on a phone) is measured with it.
      const after = getComputedStyle(el, '::after');
      const px = (v: string) => (v.endsWith('px') ? parseFloat(v) : 0);
      const grow = after.content !== 'none' && after.position === 'absolute' && cs.position !== 'static'
        ? { t: Math.max(0, -px(after.top)), b: Math.max(0, -px(after.bottom)), l: Math.max(0, -px(after.left)), r: Math.max(0, -px(after.right)) } : { t: 0, b: 0, l: 0, r: 0 };
      const r = { width: box.width + grow.l + grow.r, height: box.height + grow.t + grow.b, top: box.top, bottom: box.bottom, left: box.left, right: box.right };
      if (r.bottom < 0 || r.top > window.innerHeight || r.right < 0 || r.left > window.innerWidth) continue;
      const tier = el.matches(primary) ? 44 : 24;
      if (r.width + 0.5 < tier || r.height + 0.5 < tier) {
        out.push({ name: `${el.tagName.toLowerCase()}.${[...el.classList].join('.')} "${(el.getAttribute('aria-label') ?? el.textContent ?? '').trim().slice(0, 24)}"`, w: Math.round(r.width * 10) / 10, h: Math.round(r.height * 10) / 10, tier });
      }
    }
    return out;
  }, { controls: CONTROLS, primary: PRIMARY });
  expect.soft(small, `R9 ${state} at ${tag}: controls under their tier (44 primary, 24 secondary)`).toEqual([]);
}

/** Nothing in `main` leaves its column sideways, and the column itself does not scroll sideways. */
async function expectNoOverflow(page: Page, tag: string, state: string): Promise<void> {
  const out = await page.evaluate(() => {
    const main = document.querySelector('main, [role="main"]');
    if (main === null) return { missing: true, wide: [] as string[], scrolls: false };
    const m = main.getBoundingClientRect();
    const wide: string[] = [];
    for (const el of main.querySelectorAll<HTMLElement>('*')) {
      if (el.getClientRects().length === 0) continue;
      // A scroll container (a code block, a table) may be wider than its column; what it holds is its own to scroll.
      let clipped = false;
      for (let p = el.parentElement; p !== null && p !== main; p = p.parentElement) {
        const ox = getComputedStyle(p).overflowX;
        if (ox !== 'visible') { clipped = true; break; }
      }
      if (clipped || el.closest('.d-sr-only') !== null) continue;
      const r = el.getBoundingClientRect();
      if (r.width === 0 && r.height === 0) continue;
      if (r.left < m.left - 0.5 || r.right > m.right + 0.5) wide.push(`${el.tagName.toLowerCase()}.${[...el.classList].join('.')} ${Math.round(r.left)}..${Math.round(r.right)} in ${Math.round(m.left)}..${Math.round(m.right)}`);
    }
    return { missing: false, wide: wide.slice(0, 6), scrolls: main.scrollWidth > main.clientWidth + 1 };
  });
  expect.soft(out.missing, `main exists in ${state} at ${tag}`).toBe(false);
  expect.soft(out.wide, `no element of main leaves its column sideways in ${state} at ${tag}`).toEqual([]);
  expect.soft(out.scrolls, `main does not scroll sideways in ${state} at ${tag}`).toBe(false);
}

/** A long unbroken word wraps inside the log: the text's own rects, not the box that clips them, stay in the log's content box. */
async function expectWrapsInLog(page: Page, tag: string, state: string): Promise<void> {
  await expect(page.getByRole('log').getByText(LONG_WORD).first(), `the long message reaches the log in ${state} at ${tag}`).toBeVisible({ timeout: WAIT });
  const m = await page.evaluate((word) => {
    const log = document.querySelector('[role="log"]');
    if (log === null) return null;
    const walker = document.createTreeWalker(log, NodeFilter.SHOW_TEXT);
    for (let n = walker.nextNode(); n !== null; n = walker.nextNode()) {
      if (!(n.textContent ?? '').includes(word)) continue;
      const range = document.createRange();
      range.selectNodeContents(n);
      const rects = [...range.getClientRects()];
      const box = log.getBoundingClientRect();
      const cs = getComputedStyle(log);
      const scrollbar = (log as HTMLElement).offsetWidth - (log as HTMLElement).clientWidth - parseFloat(cs.borderLeftWidth) - parseFloat(cs.borderRightWidth);
      const left = box.left + parseFloat(cs.borderLeftWidth) + parseFloat(cs.paddingLeft);
      const right = box.right - parseFloat(cs.borderRightWidth) - scrollbar - parseFloat(cs.paddingRight);
      return { lines: rects.length, textLeft: Math.min(...rects.map(r => r.left)), textRight: Math.max(...rects.map(r => r.right)), left, right };
    }
    return undefined;
  }, LONG_WORD);
  expect.soft(m, `the long message is in the log in ${state} at ${tag}`).toBeTruthy();
  if (m === null || m === undefined) return;
  expect.soft(m.lines, `a long word wraps onto more than one line in ${state} at ${tag}`).toBeGreaterThan(1);
  expect.soft(m.textRight, `the text's right edge is inside the log's content box in ${state} at ${tag} (${m.textRight.toFixed(1)} vs ${m.right.toFixed(1)})`).toBeLessThanOrEqual(m.right + TOL);
  expect.soft(m.textLeft, `the text's left edge is inside the log's content box in ${state} at ${tag}`).toBeGreaterThanOrEqual(m.left - TOL);
}

/** On Chromium, a notched phone: with a 47 px top inset the header clears it (52 + 47, Signal §5.10), and where base.css zeroes its
 *  padding-top (the ring or the link banner already cleared the notch) it is exactly 52 in the same inset. Other engines have no override. */
async function expectSafeAreaBar(page: Page, tag: string): Promise<void> {
  if (page.context().browser()?.browserType().name() !== 'chromium') return;
  const cdp = await page.context().newCDPSession(page);
  try {
    await cdp.send('Emulation.setSafeAreaInsetsOverride', { insets: { top: 47, bottom: 0, left: 0, right: 0 } });
    const height = () => page.evaluate(() => document.querySelector('.d-phone-header')?.getBoundingClientRect().height ?? NaN);
    await expect.poll(height, { message: `the phone header with a notch at ${tag}` }).toBeCloseTo(99, 0);
    // The page's CSP forbids a style element; the CSSOM is allowed, and zeroing the padding is what base.css does in those states.
    await page.evaluate(() => { document.querySelector<HTMLElement>('.d-phone-header')?.style.setProperty('padding-top', '0', 'important'); });
    expectAt(`B1 the phone header with a notch but its padding zeroed is 52 high at ${tag}`, await height(), 52, 0.5);
    await page.evaluate(() => { document.querySelector<HTMLElement>('.d-phone-header')?.style.removeProperty('padding-top'); });
  } finally {
    await cdp.send('Emulation.setSafeAreaInsetsOverride', { insets: { top: 0, bottom: 0, left: 0, right: 0 } }).catch(() => undefined);
    await cdp.detach().catch(() => undefined);
  }
}

/** A bar's bottom hairline: its bottom edge, measured from the top of `from` (the page, or the surface the bar stands on). */
async function barBottom(page: Page, bar: string, from?: string): Promise<number | null> {
  return page.evaluate(({ bar: sel, from: top }) => {
    const el = document.querySelector(sel);
    if (el === null || el.getClientRects().length === 0) return null;
    const origin = top === undefined ? 0 : (document.querySelector(top)?.getBoundingClientRect().top ?? 0);
    return el.getBoundingClientRect().bottom - origin;
  }, { bar, from });
}
/** Signal (README §5.1, §5.2): a desktop bar (--bar-h, 44) stands under the 44px top bar, so its hairline is at 88. A phone has no
 *  top bar (§5.10): its list head is one --bar-h (44) from the top, and its PhoneHeader is 52 (§5.10, Task 6). */
const TOP_BAR = 44;
const BAR = 44;
const DESKTOP_BAR = TOP_BAR + BAR;
/** Signal §5.4: the sidebar head is no bar. It is padded 14px over and 8px under its tallest item: the 19px serif server name
 *  (line box 28.5) on a desktop, the 28px new-message icon button beside the 14px mono DM heading (line box 21), and the 44px head buttons
 *  on a phone (Ruling N). */
const SIDEBAR_HEAD = TOP_BAR + 14 + 28.5 + 8;
const SIDEBAR_HEAD_DMS = TOP_BAR + 14 + 28 + 8;
const PHONE_LIST_HEAD = 14 + 44 + 8;
/** Ruling AB: a server's phone head holds only the 26px ⌄, so it is the desktop's head without the top bar: 14 + 28.5 + 8. */
const PHONE_SERVER_HEAD = 14 + 28.5 + 8;
const PHONE_HEADER = 52;
async function expectBar(page: Page, tag: string, name: string, bar: string, want: number, from?: string): Promise<void> {
  const got = await barBottom(page, bar, from);
  expect.soft(got, `B1 ${name} (${bar}) is on screen at ${tag}`).not.toBeNull();
  if (got !== null) expectAt(`B1 ${name} bottom hairline at ${tag}`, got, want, 0.5);
}

// ---------------------------------------------------------------------------------------------------
// The world.

const nav = (page: Page) => channelsNav(page);
/** AW6 fix round 1: the AFK page's one action: go AFK, or the away line once this person is away. */
const AFK_ACTION = '.dw-voice-stage .dw-voice-join, .dw-voice-stage .dw-afk-away';
async function makeCategory(page: Page, name: string): Promise<void> {
  await page.getByRole('button', { name: copy('admin.open'), exact: true }).click().then(() => page.getByRole('menuitem', { name: copy('signalSettings.menu.serverSettings'), exact: true }).click());
  const settings = page.getByRole('dialog', { name: copy('admin.title'), exact: true });
  await settings.getByRole('navigation', { name: copy('admin.nav'), exact: true }).getByRole('button', { name: copy('admin.nav.channels'), exact: true }).click();
  await settings.getByRole('button', { name: copy('admin.channels.newCategory'), exact: true }).click();
  const dialog = page.getByRole('dialog', { name: copy('admin.category.title', { server: SERVER }), exact: true });
  await dialog.getByRole('textbox', { name: copy('admin.category.name'), exact: true }).fill(name);
  await dialog.getByRole('textbox', { name: copy('admin.category.name'), exact: true }).press('Enter');
  await expect(dialog).toBeHidden({ timeout: WAIT });
  await page.keyboard.press('Escape');
  await expect(settings).toBeHidden({ timeout: WAIT });
  await expect(nav(page).getByRole('button', { name: new RegExp(`^${name}$`) })).toBeVisible({ timeout: WAIT });
}
async function makeChannelIn(page: Page, category: string, kind: 'text' | 'voice' | 'announce', name: string): Promise<void> {
  await nav(page).getByRole('button', { name: copy('layout.category.add', { name: category }), exact: true }).click();
  const dialog = page.getByRole('dialog', { name: copy('create.channel.title', { server: SERVER }), exact: true });
  if (kind !== 'text') await dialog.getByRole('radio', { name: copy(kind === 'voice' ? 'create.channel.voice' : 'announce.kind'), exact: true }).click();
  await dialog.getByRole('textbox', { name: copy('create.channel.name'), exact: true }).fill(name);
  await dialog.getByRole('textbox', { name: copy('create.channel.name'), exact: true }).press('Enter');
  await expect(dialog).toBeHidden({ timeout: WAIT });
  await expect(kind === 'voice' ? nav(page).getByRole('button', { name: copy('voice.open', { name }), exact: true }) : channelRow(page, name)).toBeVisible({ timeout: WAIT });
}

/** The friend's browser: Chromium and Firefox have a fake microphone (the voice person); WebKit has none, so no live person there. */
function friendLaunch(browserName: string, launchOptions: object): object {
  if (browserName === 'chromium') return { ...launchOptions, args: chromiumMediaArgs(writeFixtures(join(tmpdir(), 'dilla-media-fixtures'))) };
  if (browserName === 'firefox') return { ...launchOptions, firefoxUserPrefs: FIREFOX_MEDIA_PREFS };
  return launchOptions;
}

test('the layout grammar holds at desktop, tablet, phone, narrow and keyboard-height viewports', async ({ page, playwright, browserName, launchOptions, headless, channel, guard }, testInfo) => {
  const h = started();
  const t0 = Date.now();
  const lap = (what: string) => console.log(`LAP ${String(Math.round((Date.now() - t0) / 1000))}s ${what}`);
  await page.setViewportSize({ width: 1440, height: 900 });

  // 1. Ada starts the server and gives it two categories, each with channels made from the category's own "+".
  const ada: Account = await signUp(page, h.invite.split(',')[0], instance, OWNER, h.control);
  await addServerVia(page, 'create');
  const start = page.getByRole('dialog', { name: copy('create.server.title'), exact: true });
  await start.getByRole('textbox', { name: copy('create.server.name'), exact: true }).fill(SERVER);
  await page.keyboard.press('Enter');
  await expect(start).toBeHidden({ timeout: WAIT });
  await expectComposerReady(page, 'general');
  await makeCategory(page, PLAN);
  await makeCategory(page, LOUNGE);
  await makeChannelIn(page, PLAN, 'text', RAID);
  await makeChannelIn(page, PLAN, 'announce', NEWS);
  await makeChannelIn(page, LOUNGE, 'voice', HARBOUR);
  await channelRow(page, 'general').click();
  await expectComposerReady(page, 'general');
  await sendText(page, 'general', `the map is in the chest ${LONG_WORD}`);
  const link = await makeInviteLink(page, SERVER);

  lap('world: server and channels');
  // 2. Mira, in a browser of her own: she reads #raid, writes in it (Ada has unread there), writes to Ada, then joins the voice channel.
  const secondDir = secondProfileDir(testInfo.project.name, testInfo.workerIndex);
  const second = await launchSecond(playwright, browserName, { launchOptions: friendLaunch(browserName, launchOptions), headless, channel, viewport: { width: 1440, height: 900 }, baseURL: h.url }, secondDir);
  const phoneDir = `${PROFILE_ROOT}/${testInfo.project.name}-${testInfo.workerIndex}-phone`;
  let phoneContext: BrowserContext | null = null;
  try {
    if (browserName === 'chromium') await second.grantPermissions(['microphone']);
    const mira = second.pages()[0] ?? (await second.newPage());
    await mira.goto(new URL(link).pathname);
    const openLink = mira.getByRole('link', { name: 'Open dilla in this browser', exact: true });
    await expect(openLink).toBeVisible({ timeout: WAIT });
    // The guard watches the web client, not the instance's static invite page (Firefox asks it for /favicon.ico under its default-src 'none').
    await guard.attach(second);
    await openLink.click();
    await completeSignUp(mira, newAccount(FRIEND, h.control), instance);
    await expect(railItem(mira, SERVER)).toBeVisible({ timeout: WAIT });
    await railItem(mira, SERVER).click();
    await channelRow(mira, RAID).click();
    await expectComposerReady(mira, RAID);
    await sendText(mira, RAID, '@Ada can you take the left flank?');
    await homeTile(mira).click();
    await mira.getByRole('button', { name: copy('shell.dms.new'), exact: true }).click();
    await mira.getByRole('dialog', { name: copy('shell.dms.newTitle'), exact: true }).getByRole('button', { name: copy('shell.dms.open'), exact: true }).click();
    const dmBox = mira.getByRole('textbox', { name: copy('shell.dm.composer', { name: OWNER }), exact: true });
    await expect(dmBox).toBeEnabled({ timeout: WAIT });
    await dmBox.fill('see you at the bridge, Ada');
    await dmBox.press('Enter');
    await expect(mira.getByRole('log').locator('.d-message-row').filter({ hasText: 'see you at the bridge' })).toHaveAttribute('data-state', 'ok', { timeout: WAIT });
    let voicePath = '';
    if (browserName !== 'webkit') {
      await railItem(mira, SERVER).click();
      await nav(mira).getByRole('button', { name: copy('voice.open', { name: HARBOUR }), exact: true }).click();
      await expect(mira.locator('[data-call-phase]').first()).toHaveAttribute('data-call-phase', 'in', { timeout: WAIT });
      voicePath = new URL(mira.url()).pathname;
      // Ada sees her under the row.
      await expect(nav(page).locator('.d-voice-row__person').filter({ hasText: FRIEND })).toBeVisible({ timeout: WAIT });
    }
    const serverPath = new URL(page.url()).pathname;
    await expect(page.locator('.d-chrow', { hasText: RAID })).toHaveAttribute('data-unread', /^[1-9]/, { timeout: WAIT });

    lap('world: Mira, DM, voice');
    if (CHECK_WEBROOT !== null) {
      // The red runs: the same server data and the same browsers, served the other build (the live call does not survive the restart).
      await h.stop();
      host = await startTestHost({ ...EMPTY, webRoot: CHECK_WEBROOT });
    }
    // 3. Desktop: 1440 and 1024.
    for (const size of SIZES.filter(s => !s.phone)) {
      await page.setViewportSize({ width: size.width, height: size.height });
      await test.step(`desktop ${size.tag}`, async () => { await attempt(`desktop ${size.tag}`, () => desktopGrammar(page, size, serverPath, voicePath)); });
    }
    // Each opens the server itself: the desktop pass ends on a DM where no voice stage follows it (WebKit has no call).
    await test.step('search is a modal that traps focus', async () => { await page.setViewportSize({ width: 1440, height: 900 }); await attempt('search', () => searchModal(page, serverPath)); });
    await test.step('a collapsed category keeps its current and unread rows', async () => { await attempt('collapsed category', () => collapsedCategory(page, serverPath)); });

    lap('desktop and search and collapse done');
    // 4. Phone: a touch phone where the engine has one (Chromium, WebKit), Ada's second device; Firefox has no mobile emulation,
    // and 390 and 360 wide is a phone layout there too, so its own page is resized.
    let phone: Page = page;
    if (browserName !== 'firefox') {
      rmSync(phoneDir, { recursive: true, force: true });
      mkdirSync(phoneDir, { recursive: true });
      phoneContext = await playwright[browserName].launchPersistentContext(phoneDir, {
        ...launchOptions, headless, channel, ...PHONE, viewport: { width: 390, height: 844 }, baseURL: h.url,
      });
      await guard.attach(phoneContext);
      phone = await phoneContext.newPage();
      await signIn(phone, ada, instance);
    }
    lap('phone signed in');
    // The phone joined after the first long message, so it cannot read it: Ada writes another for the phone's log.
    await openPath(page, serverPath);
    await channelRow(page, 'general').click();
    if (phone !== page) {
      // A new device joins each group on its own after it signs in (one external commit per group), so Ada writes only
      // once the phone holds #general and her desktop has its join: the phone's composer opens when it holds the group,
      // and the desktop reading a line the phone wrote there is past that join. Written earlier, Ada's line raced the
      // join (a 422 for her stale epoch, or a line the phone never read, CI run 39664262787), and the phone's first
      // view reloaded its page while it was still syncing. Now the phone stays on #general until the line is in.
      await openPath(phone, serverPath);
      await expectComposerReady(phone, 'general');
      const here = `the phone is in ${String(Date.now())}`;
      await sendText(phone, 'general', here);
      await expect(messageRow(page, 'general', here)).toHaveAttribute('data-state', 'ok', { timeout: WAIT });
    }
    const again = `and again ${LONG_WORD}`;
    await sendText(page, 'general', again);
    if (phone !== page) await expect(messageRow(phone, 'general', again)).toHaveAttribute('data-state', 'ok', { timeout: WAIT });
    for (const size of SIZES.filter(s => s.phone)) {
      await phone.setViewportSize({ width: size.width, height: size.height });
      await test.step(`phone ${size.tag}`, async () => { await attempt(`phone ${size.tag}`, () => phoneGrammar(phone, size, serverPath, voicePath)); });
    }
  } finally {
    await guard.collectWorkers();
    await phoneContext?.close();
    await second.close();
    rmSync(secondDir, { recursive: true, force: true });
    rmSync(phoneDir, { recursive: true, force: true });
  }
});

// ---------------------------------------------------------------------------------------------------
// Desktop.

async function desktopGrammar(page: Page, size: Size, serverPath: string, voicePath: string): Promise<void> {
  const { tag } = size;
  await openPath(page, serverPath);
  await expect(nav(page).getByRole('button', { name: new RegExp(`^${RAID}`) })).toBeVisible({ timeout: WAIT });

  // The rail (R22).
  const rail = await boxes(page, '.d-app-shell__rail');
  // README §5.3: --rail-w 56, --rail-tile 38.
  expectAt(`R22 the rail is 56 wide at ${tag}`, rail[0]?.w ?? 0, 56, 0.5);
  for (const tile of await boxes(page, '.d-community-rail__item, .d-community-rail__join')) {
    expectAt(`R22 rail tile "${tile.t}" is 38 wide at ${tag}`, tile.w, 38, 0.5);
    expectAt(`R22 rail tile "${tile.t}" is 38 high at ${tag}`, tile.h, 38, 0.5);
  }

  // The channel list (A5, A4): channel rows and voice rows, and every slot in it. Signal §5.4: a category label follows its 11px
  // chevron (8 + 11 + 6), so the category labels share one x of their own.
  await expectListGrammar(page, tag, '.d-channel-list__list', '.d-chrow__name, .d-voice-row__name', 'channel list');
  const cats = await boxes(page, '.d-channel-list__cat-label', '.d-channel-list__list');
  expectOne(`A5 category labels share one left x at ${tag}`, cats.map(b => b.x), cats.map(b => b.t));

  // Bars (B1): the channel header and the members head; the sidebar head (Signal §5.4) is the padded head over the stat line.
  await expectBar(page, tag, 'the sidebar head', '.d-channel-list__head', SIDEBAR_HEAD);
  await expectBar(page, tag, 'the channel header', '.d-channel-header', DESKTOP_BAR);
  if (size.width >= 1440) await expectBar(page, tag, 'the members head', '.d-member-list__bar', DESKTOP_BAR);
  await expectNoOverflow(page, tag, 'a channel');
  await expectReminderInbox(page, `channel at ${tag}`);

  // C1: at 1024 with the members aside open the header keeps its name and every action stays inside the header.
  if (size.width === 1024) {
    const toggle = membersToggle(page);
    if (await page.locator('.d-app-shell__aside').count() === 0 && await toggle.count() > 0) await toggle.click();
    await expect(page.locator('.d-app-shell__aside')).toBeVisible({ timeout: WAIT });
    await expectBar(page, tag, 'the members head', '.d-member-list__bar', DESKTOP_BAR);
  }
  const header = (await boxes(page, '.d-channel-header'))[0];
  const name = (await boxes(page, '.d-channel-header__name'))[0];
  expect.soft(name?.w ?? 0, `C1 the header's name is visible at ${tag}`).toBeGreaterThan(24);
  if (header !== undefined) {
    for (const action of await boxes(page, '.d-channel-header__actions button')) {
      expect.soft(action.x, `C1 action "${action.t}" starts inside the header at ${tag}`).toBeGreaterThanOrEqual(header.x - TOL);
      expect.soft(action.r, `C1 action "${action.t}" ends inside the header at ${tag}`).toBeLessThanOrEqual(header.r + TOL);
    }
  }

  // The direct messages (A5, A4, B1): the `@` tile shows the DM list.
  await homeTile(page).click();
  await expect(dmsNav(page).getByRole('button', { name: new RegExp(`^${FRIEND}`) })).toBeVisible({ timeout: WAIT });
  // Task 11 (Jonas, Q2): a DM row is the prototype's person row (avatar, name over status, unread dot), without a trailing slot,
  // so only the names' shared x is measured.
  const dmNames = await boxes(page, '.d-chrow__name', '.d-channel-list__list');
  expectOne(`A5 direct messages names share one left x at ${tag}`, dmNames.map(b => b.x), dmNames.map(b => b.t));
  await expectBar(page, tag, 'the sidebar head (direct messages)', '.d-channel-list__head, .d-channel-list__title', SIDEBAR_HEAD_DMS);
  await dmsNav(page).getByRole('button', { name: new RegExp(`^${FRIEND}`) }).click();
  await expect(page.getByRole('log')).toBeVisible({ timeout: WAIT });
  await expectBar(page, tag, 'the conversation header', '.d-channel-header', DESKTOP_BAR);
  await expectNoOverflow(page, tag, 'a direct message');
  await expectReminderInbox(page, `direct message at ${tag}`);

  // The voice stage (B1), opened by its path so that Ada does not join (a click on a desktop voice row joins).
  if (voicePath !== '') {
    await openPath(page, voicePath);
    await expect(page.locator('.dw-voice-stage')).toBeVisible({ timeout: WAIT });
    await expectBar(page, tag, 'the stage header', '.d-call-stage__bar', DESKTOP_BAR);
    await expectListGrammar(page, tag, '.d-channel-list__list', '.d-chrow__name, .d-voice-row__name', 'channel list with a live person');
    const avatars = await boxes(page, '.d-voice-row__person .d-voice-row__avatar', '.d-channel-list__list');
    const rowNames = await boxes(page, '.d-voice-row__name', '.d-channel-list__list');
    if (avatars.length > 0 && rowNames.length > 0) expectAt(`A7 a person's avatar starts at the names' x at ${tag}`, avatars[0].x, rowNames[0].x, TOL);
    await expectNoOverflow(page, tag, 'the voice stage');
    await expectReminderInbox(page, `idle voice at ${tag}`);
  }

  // Settings is a full-screen modal at every viewport. On desktop/tablet its inner workspace is centered and
  // capped at 80rem with a 15rem sidebar; on phone it covers the tab strip and shows an index → section flow.
  const settings = await openSettings(page, 'profile');
  const settingsGeometry = await settings.evaluate(dialog => {
    const rect = (selector: string) => {
      const el = dialog.querySelector<HTMLElement>(selector);
      if (!el) return null;
      const r = el.getBoundingClientRect();
      return { x: r.x, y: r.y, width: r.width, height: r.height, right: r.right, bottom: r.bottom };
    };
    const viewport = { width: innerWidth, height: innerHeight };
    const frame = rect('.d-settings-frame__panel')!;
    const side = rect('.d-settings-frame__side')!;
    const content = rect('.d-settings-frame__body > section')!;
    return { viewport, frame, side, content, layout: dialog.getAttribute('data-layout') };
  });
  expect(settingsGeometry.frame.x).toBeGreaterThanOrEqual(0);
  expect(settingsGeometry.frame.right).toBeLessThanOrEqual(settingsGeometry.viewport.width + 1);
  expect(settingsGeometry.frame.y).toBeGreaterThanOrEqual(0);
  expect(settingsGeometry.frame.bottom).toBeLessThanOrEqual(settingsGeometry.viewport.height + 1);
  if (settingsGeometry.layout === null) {
    expect(settingsGeometry.frame.width).toBe(Math.min(settingsGeometry.viewport.width, 1280));
    expect(settingsGeometry.side.width).toBe(240);
    expect(settingsGeometry.content.width).toBeLessThanOrEqual(768);
    expect(settingsGeometry.content.x).toBeGreaterThanOrEqual(settingsGeometry.side.right - 1);
  } else {
    expect(settingsGeometry.frame.width).toBe(settingsGeometry.viewport.width);
    expect(settingsGeometry.frame.height).toBeLessThanOrEqual(settingsGeometry.viewport.height + 1);
    expect(settingsGeometry.side.width).toBe(settingsGeometry.viewport.width);
    // Task 10 (README §5.10): the phone section shows the nav as chips above the content, the close button at the top right.
    await expect(settings.locator('.d-settings-nav__item[aria-current="page"]')).toHaveText(copy('workspace.profile'));
    await expect(settings.getByRole('button', { name: copy('settings.close'), exact: true })).toBeVisible();
    await expect(page.locator('.d-app-shell').getByRole('navigation', { name: copy('nav.tabs.label'), exact: true })).toHaveCount(0);
  }
  await page.keyboard.press('Escape');
  await expect(settings).toBeHidden({ timeout: WAIT });
}

/** On a desktop the top bar's bell opens the reminders popover (README §6.7): non-modal, under the bell; Escape closes it and
 *  gives focus back to the bell. A phone has no reminders button anywhere (Jonas 2026-10-27): a due reminder is an Activity row. */
async function expectReminderInbox(page: Page, state: string): Promise<void> {
  await test.step(`reminder inbox on ${state}`, async () => {
    const before = page.url();
    const opener = page.getByRole('button', { name: 'Reminders', exact: true });
    await expect(opener).toHaveCount(1);
    await expect(opener).toBeVisible();
    await opener.click();
    const inbox = page.getByRole('dialog', { name: 'Reminders', exact: true });
    await expect(inbox).toHaveCount(1);
    await expect(inbox).toBeVisible();
    await expect(page.locator('.d-topbar__bell')).toContainText('Reminders');
    await page.keyboard.press('Escape');
    await expect(inbox).toBeHidden();
    await expect(opener).toBeFocused();
    await expect(page).toHaveURL(before);
  });
}
async function expectNoPhoneReminders(page: Page, state: string): Promise<void> {
  await test.step(`no reminders button on ${state}`, async () => {
    await expect(page.getByRole('button', { name: 'Reminders', exact: true })).toHaveCount(0);
  });
}

/** Opens a path in the app without a click that would join a call. */
async function openPath(page: Page, path: string): Promise<void> {
  if (new URL(page.url()).pathname !== path) await page.goto(path);
  await expect(page.locator('.d-app-shell')).toBeVisible({ timeout: WAIT });
}
// ---------------------------------------------------------------------------------------------------
// Search (controller addition, UI-5).

async function searchModal(page: Page, serverPath: string): Promise<void> {
  await openPath(page, serverPath);
  await channelRow(page, 'general').click();
  const opener = channelRow(page, 'general');
  await opener.focus();
  await page.keyboard.press('Control+k');
  const dialog = page.locator('dialog[open]');
  await expect(dialog, 'the search palette is a modal dialog').toBeVisible({ timeout: WAIT });
  // Focus is in the dialog or out of the page (the browser's own chrome, one stop of a Tab cycle: activeElement is the body); a shell control never takes it.
  const stray = () => page.evaluate(() => {
    const el = document.activeElement;
    return el === null || el === document.body ? null : el.closest('dialog[open]') !== null ? null : `${el.tagName.toLowerCase()}.${[...el.classList].join('.')}`;
  });
  // More presses than the dialog has controls: Tab cycles inside it.
  for (let i = 0; i < 14; i++) {
    await page.keyboard.press('Tab');
    expect.soft(await stray(), `Tab press ${String(i + 1)} never reaches a shell control`).toBeNull();
  }
  await page.keyboard.press('Shift+Tab');
  expect.soft(await stray(), 'Shift+Tab never reaches a shell control').toBeNull();
  // The shell behind it is inert: a channel row is not hit-testable, and a click on it does not focus it (it may dismiss the palette, which returns focus to the opener, never to the row).
  const target = channelRow(page, RAID);
  const spot = await target.boundingBox();
  expect.soft(spot, 'a shell row stands behind the dialog').not.toBeNull();
  if (spot !== null) {
    const x = spot.x + 8;
    const y = spot.y + spot.height / 2;
    const hit = await page.evaluate(({ x: px, y: py }) => {
      const el = document.elementFromPoint(px, py);
      return el === null ? 'nothing' : el.closest('dialog[open]') !== null ? 'dialog' : el.closest('.d-chrow') !== null ? 'row' : `${el.tagName.toLowerCase()}.${[...el.classList].join('.')}`;
    }, { x, y });
    expect.soft(hit, 'the point over a shell row hits the dialog, not the row').toBe('dialog');
    await page.mouse.click(x, y);
    expect.soft(await target.evaluate(el => el === document.activeElement), 'a click outside the dialog does not focus the shell row').toBe(false);
  }
  if (await page.locator('dialog[open]').count() === 0) {
    // The click on the scrim dismissed it; open it again for the Escape check.
    await opener.focus();
    await page.keyboard.press('Control+k');
    await expect(page.locator('dialog[open]')).toBeVisible({ timeout: WAIT });
  }
  await page.keyboard.press('Escape');
  await expect(page.locator('dialog[open]')).toHaveCount(0, { timeout: WAIT });
  await expect(opener, 'Escape returns focus to the opener').toBeFocused();
}

// ---------------------------------------------------------------------------------------------------
// Collapsed categories (R23).

async function collapsedCategory(page: Page, serverPath: string): Promise<void> {
  const head = (name: string) => nav(page).locator('.d-channel-list__cat-head').filter({ hasText: name });
  await openPath(page, serverPath);
  await channelRow(page, 'general').click();
  // Collapse "planning": #news is read and not current, so it folds away; #raid has Mira's unread message, so it stays.
  await head(PLAN).click();
  await expect(head(PLAN)).toHaveAttribute('aria-expanded', 'false');
  await expect.soft(channelRow(page, RAID), 'an unread row stays in a collapsed category').toBeVisible();
  await expect.soft(channelRow(page, NEWS), 'a read row folds away').toHaveCount(0);
  // Open #raid, which reads it: it is now current, so it stays; #news is still folded.
  await channelRow(page, RAID).click();
  await expect.soft(channelRow(page, RAID), 'the current row stays in a collapsed category').toBeVisible();
  await expect.soft(channelRow(page, NEWS)).toHaveCount(0);
  // The read lands once the open timeline has loaded (Shell's markRead effect); leaving before then leaves it unread, so
  // the row rightly stays (2026-10-30 CI, Firefox: 'general' clicked 56 ms after #raid, its log still aria-busy).
  await expect(nav(page).locator('.d-chrow', { hasText: RAID }), '#raid is read while open').toHaveAttribute('data-unread', '0', { timeout: WAIT });
  // Leave it: the read row folds too.
  await channelRow(page, 'general').click();
  await expect.soft(channelRow(page, RAID), 'once read and left, the row folds away').toHaveCount(0);
  await head(PLAN).click();
  await expect(head(PLAN)).toHaveAttribute('aria-expanded', 'true');
  await expect(channelRow(page, NEWS)).toBeVisible();
}

// ---------------------------------------------------------------------------------------------------
// Phone.

async function phoneGrammar(page: Page, size: Size, serverPath: string, voicePath: string): Promise<void> {
  const { tag } = size;
  const tabs = page.getByRole('navigation', { name: copy('nav.tabs.label'), exact: true });
  const tab = (key: string) => tabs.getByRole('link', { name: new RegExp(`^${escapeRegExp(copy(key))}`) });
  const back = page.getByRole('button', { name: copy('nav.back'), exact: true });
  const pane = (want: 'list' | 'view') => expect(page.locator('.d-app-shell')).toHaveAttribute('data-pane', want, { timeout: WAIT });
  /** Back out of a pushed view to the list it came from. */
  const toList = async () => { if (await back.isVisible().catch(() => false)) await back.click(); await pane('list'); };

  // The home tab: the rail beside the server's channel list.
  await openPath(page, serverPath);
  await toList();
  await tab('home.tab').click();
  await expect(nav(page).getByRole('button', { name: new RegExp(`^${escapeRegExp(RAID)}`) })).toBeVisible({ timeout: WAIT });
  await expectListGrammar(page, tag, '.d-channel-list__list', '.d-chrow__name, .d-voice-row__name', 'channel list');
  await expectBar(page, tag, 'the sidebar head', '.d-channel-list__head', PHONE_SERVER_HEAD);
  await expectTargets(page, tag, 'the home tab');
  for (const tile of await boxes(page, '.d-community-rail__item, .d-community-rail__join')) {
    // The prototype's 38px tiles on a phone too (Jonas 2026-10-27, supersedes Ruling N).
    expectAt(`R22 rail tile "${tile.t}" is 38 wide at ${tag}`, tile.w, 38, 0.5);
    expectAt(`R22 rail tile "${tile.t}" is 38 high at ${tag}`, tile.h, 38, 0.5);
  }
  // The controller addition (UI-2 review): in the real app the voice join button and the category "+" are 44 on a phone.
  for (const [selector, what] of [['.d-voice-row__join button', 'the voice join button'], ['.d-channel-list__cat-add button', 'the category "+"']] as const) {
    const found = await boxes(page, selector);
    expect.soft(found.length, `R9 ${what} is on the home tab at ${tag}`).toBeGreaterThan(0);
    for (const b of found) {
      expect.soft(b.w, `R9 ${what} is 44 wide at ${tag}`).toBeGreaterThanOrEqual(44 - TOL);
      expect.soft(b.h, `R9 ${what} is 44 high at ${tag}`).toBeGreaterThanOrEqual(44 - TOL);
    }
  }

  // A channel is a pushed view under the phone header.
  await channelRow(page, 'general').click();
  await pane('view');
  await expectBar(page, tag, 'the phone header (channel)', '.d-phone-header', PHONE_HEADER);
  await expectSafeAreaBar(page, tag);
  await expectTargets(page, tag, 'a channel');
  await expectNoOverflow(page, tag, 'a channel');
  await expectWrapsInLog(page, tag, 'a channel');
  await toList();

  // The voice stage, by its path (a tap on a voice row only opens it, but its join icon joins).
  if (voicePath !== '') {
    await openPath(page, voicePath);
    await pane('view');
    await expect(page.locator('.dw-voice-stage')).toBeVisible({ timeout: WAIT });
    await expectBar(page, tag, 'the phone header (voice)', '.d-phone-header', PHONE_HEADER);
    await expectTargets(page, tag, 'the voice stage');
    await expectNoOverflow(page, tag, 'the voice stage');
    await expectNoPhoneReminders(page, `idle voice at ${tag}`);
    await toList();
  }

  // The direct messages: the rail's `@` tile, then a conversation.
  await homeTile(page).click();
  await expect(dmsNav(page).getByRole('button', { name: new RegExp(`^${FRIEND}`) })).toBeVisible({ timeout: WAIT });
  // Task 11 (Jonas, Q2): a DM row is the prototype's person row (avatar, name over status, unread dot), without a trailing slot,
  // so only the names' shared x is measured.
  const dmNames = await boxes(page, '.d-chrow__name', '.d-channel-list__list');
  expectOne(`A5 direct messages names share one left x at ${tag}`, dmNames.map(b => b.x), dmNames.map(b => b.t));
  await expectBar(page, tag, 'the direct messages head', '.d-channel-list__head', PHONE_LIST_HEAD);
  await expectTargets(page, tag, 'the direct messages');
  await dmsNav(page).getByRole('button', { name: new RegExp(`^${FRIEND}`) }).click();
  await pane('view');
  await expectBar(page, tag, 'the phone header (direct message)', '.d-phone-header', PHONE_HEADER);
  await expectNoOverflow(page, tag, 'a direct message');
  await expectNoPhoneReminders(page, `direct message at ${tag}`);
  await toList();

  // The activity and You tabs remain app destinations; You now has one settings entry point.
  await tab('nav.tab.activity').click();
  // Task 11 (Jonas, Q3): Activity opens with the prototype's heading (18px over the 14px title), not a 52px bar.
  const activityHead = page.locator('.dw-activity .d-phone-header');
  await expect(activityHead).toBeVisible({ timeout: WAIT });
  const headBox = (await activityHead.boundingBox())!;
  const titleBox = (await activityHead.locator('.d-phone-header__title').boundingBox())!;
  expectAt(`B1 the Activity title sits 18px into its heading at ${tag}`, titleBox.y - headBox.y, 18, 1);
  expect.soft(await activityHead.locator('.d-phone-header__title').evaluate(e => getComputedStyle(e).fontSize), `B1 the Activity title is 14px at ${tag}`).toBe('14px');
  await expectTargets(page, tag, 'the activity tab');
  await expectNoPhoneReminders(page, `the activity tab at ${tag}`);
  await tab('nav.tab.you').click();
  await expect(page.locator('[data-settings-section="index"]')).toBeVisible({ timeout: WAIT });
  // Task 11 (Jonas, Q3): You starts with the account (its heading is for screen readers only), not a 52px bar: the 56px avatar
  // 24px below the pane's top.
  const youBox = (await page.locator('.dw-you').boundingBox())!;
  const accountAvatar = (await page.locator('.dw-you__account .d-avatar').boundingBox())!;
  expectAt(`B1 You's account starts 24px down at ${tag}`, accountAvatar.y - youBox.y, 24, 1);
  expectAt(`B1 You's avatar is 56px at ${tag}`, accountAvatar.height, 56, 0.5);
  await expectTargets(page, tag, 'the you tab');
  await page.locator('[data-settings-section="index"]').click();
  const userSettings = settingsDialog(page);
  await expect(userSettings).toBeVisible({ timeout: WAIT });
  await expect(page).toHaveURL('/settings', { timeout: WAIT });
  await expectTargets(page, tag, 'the user settings index');
  await expect(userSettings.getByRole('button', { name: copy('workspace.profile'), exact: true })).toBeVisible();
  await expect(page.locator('.d-app-shell').getByRole('navigation', { name: copy('nav.tabs.label'), exact: true })).toHaveCount(0);
  const frameBox = await userSettings.boundingBox();
  expect(frameBox?.x).toBe(0);
  expect(frameBox?.width).toBe(size.width);
  await userSettings.getByRole('button', { name: copy('workspace.profile'), exact: true }).click();
  await expect(page).toHaveURL('/settings/profile', { timeout: WAIT });
  await expectTargets(page, tag, 'a user settings section');
  await expect(userSettings.getByRole('button', { name: copy('workspace.back'), exact: true })).toHaveCount(0);
  await expect(userSettings.getByRole('button', { name: copy('workspace.profile'), exact: true })).toHaveAttribute('aria-current', 'page');
  await page.goBack();
  await expect(page).toHaveURL('/settings', { timeout: WAIT });
  await page.keyboard.press('Escape');
  await expect(userSettings).toBeHidden({ timeout: WAIT });

  // Server settings uses the same phone index → section flow, with a section route under the selected community.
  await openPath(page, serverPath);
  await toList();
  const serverSettingsOrigin = page.url();
  await page.getByRole('button', { name: copy('admin.open'), exact: true }).click().then(() => page.getByRole('menuitem', { name: copy('signalSettings.menu.serverSettings'), exact: true }).click());
  const admin = page.getByRole('dialog', { name: copy('admin.title'), exact: true });
  await expect(admin).toBeVisible({ timeout: WAIT });
  await expectTargets(page, tag, 'the server settings index');
  const side = (await boxes(page, '.d-settings-frame__side'))[0];
  expect.soft(side !== undefined, `the server settings index has its grouped navigation at ${tag}`).toBe(true);
  const close = admin.getByRole('button', { name: copy('settings.close'), exact: true });
  await expect(close).toBeVisible({ timeout: WAIT });
  const closeBox = await close.boundingBox();
  const viewport = page.viewportSize();
  expect.soft(closeBox !== null && viewport !== null && closeBox.x >= 0 && closeBox.y >= 0
    && closeBox.x + closeBox.width <= viewport.width && closeBox.y + closeBox.height <= viewport.height,
  `the settings index Close control stays inside ${tag} viewport`).toBe(true);
  const overview = admin.getByRole('navigation', { name: copy('admin.nav'), exact: true })
    .getByRole('button', { name: copy('workspace.overview'), exact: true });
  await overview.click();
  await expect(page).toHaveURL(/\/c\/[0-9a-f]{32}\/settings\/overview$/, { timeout: WAIT });
  await expectTargets(page, tag, 'a server settings section');
  await expect(admin.getByRole('button', { name: copy('workspace.back'), exact: true })).toHaveCount(0);
  await page.goBack();
  await expect(page).toHaveURL(/\/c\/[0-9a-f]{32}\/settings$/, { timeout: WAIT });
  await close.click();
  await expect(admin).toBeHidden({ timeout: WAIT });
  await expect(page).toHaveURL(serverSettingsOrigin, { timeout: WAIT });
}

// ---------------------------------------------------------------------------------------------------
// The built-in AFK channel (WEB-AFK): its row is a voice row on the list's grammar, and its timeout is a settings row under
// the name row in the Overview. The shared Save footer appears only after an edit.

test('the AFK row and the AFK timeout setting keep the grammar', async ({ page }) => {
  const h = started();
  await page.setViewportSize({ width: 1440, height: 900 });
  await signUp(page, h.invite.split(',')[0], instance, 'Bea', h.control);
  await addServerVia(page, 'create');
  const start = page.getByRole('dialog', { name: copy('create.server.title'), exact: true });
  await start.getByRole('textbox', { name: copy('create.server.name'), exact: true }).fill('Bifrost');
  await page.keyboard.press('Enter');
  await expect(start).toBeHidden({ timeout: WAIT });
  await expectComposerReady(page, 'general');
  const afk = nav(page).getByRole('button', { name: copy('voice.open', { name: 'AFK' }), exact: true });
  await expect(afk, 'the built-in AFK channel is a voice row').toBeVisible({ timeout: WAIT });
  await expectListGrammar(page, 'AFK 1440x900', '.d-channel-list__list', '.d-chrow__name, .d-voice-row__name', 'channel list with the AFK row');
  await page.getByRole('button', { name: copy('admin.open'), exact: true }).click().then(() => page.getByRole('menuitem', { name: copy('signalSettings.menu.serverSettings'), exact: true }).click());
  const settings = page.getByRole('dialog', { name: copy('admin.title'), exact: true });
  const nameRow = settings.locator('.d-settings-row').filter({ hasText: copy('admin.overview.name') });
  const timeout = settings.locator('.d-settings-row').filter({ hasText: copy('admin.overview.afk') });
  await expect(timeout, 'the AFK timeout is a settings row in the Overview').toBeVisible({ timeout: WAIT });
  const [nameBox, timeoutBox] = [await nameRow.boundingBox(), await timeout.boundingBox()];
  expect.soft(nameBox !== null && timeoutBox !== null && timeoutBox.y > nameBox.y, 'the timeout row stands under the name row').toBe(true);
  const labels = await boxes(page, '.d-settings-row__label', '.d-settings-frame__body');
  expect.soft(labels.length, 'the Overview has its name and timeout rows').toBeGreaterThanOrEqual(2);
  expectOne('A5 the Overview rows share one label x', labels.map(b => b.x), labels.map(b => b.t));
  const controls = await boxes(page, '.d-settings-row__control', '.d-settings-frame__body');
  expectOne('A4 the Overview rows share one control right edge', controls.map(b => b.r), controls.map(b => b.t));
  const save = settings.locator('.d-settings-frame__footer').getByRole('button', { name: copy('admin.overview.save'), exact: true });
  await expect(save, 'a clean Overview has no Save footer').toHaveCount(0);
  await settings.getByRole('radiogroup', { name: copy('admin.overview.afk'), exact: true }).getByRole('radio', { name: copy('admin.overview.afk.900'), exact: true }).click();
  await expect(save, 'one Save appears in the shared footer after a setting changes').toHaveCount(1);
  expect.soft(await settings.getByRole('button', { name: copy('admin.overview.save'), exact: true }).count(), 'the shared footer owns the only Save').toBe(1);
  await save.click();
  await expect(save, 'the footer clears after the changed value is saved').toHaveCount(0, { timeout: WAIT });
});

// ---------------------------------------------------------------------------------------------------
// AUDIT-2 FA1 (H4, N3, I1): on a phone every button, select and toggle switch is at least 44 high, outside the shell too
// (onboarding, sign-in), and the invites section is on the settings grammar.

/** Every visible button, select and switch inside `root` (the whole page when omitted) is at least 44 high on a phone. */
/**
 * Signal task 10 (Jonas 2026-10-27, pixel parity): settings and the sign-in card keep the prototype's sizes on a phone, not the
 * 44px tier. Each [selector, height] pair names a control and the height the prototype draws it (content plus its 1px edges).
 */
async function expectPrototypeSizes(page: Page, what: string, sizes: readonly (readonly [string, number])[], root?: string): Promise<void> {
  for (const [selector, height] of sizes) {
    const found = await boxes(page, selector, root);
    expect.soft(found.length, `${what}: ${selector} is drawn`).toBeGreaterThan(0);
    for (const b of found) expect.soft(Math.abs(b.h - height), `${what}: "${b.t}" (${selector}) is ${b.h.toFixed(1)} high, want ${height}`).toBeLessThanOrEqual(1);
  }
}
/** The sign-in card: 40px fields and the 40px primary button. */
const AUTH_SIZES = [['.d-onboarding-frame .d-text-field__input', 40], ['.d-onboarding-frame__footer .d-btn[data-variant="accent"]', 40]] as const;
/** A settings button is 32 (30 of content); a device select and the push-to-talk key 38. Merge of Task 10b: the Voice page has
 *  no .d-btn under voice activity; its Let's check is the prototype's 32px of content and its 1px border (34). */
const SETTINGS_SIZES = [['.d-settings-frame__body .d-btn:not(.d-voice-settings__capture)', 32], ['.d-voice-settings__select', 38]] as const;
const VOICE_SIZES = [['.d-voice-settings__check', 34], ['.d-voice-settings__select', 38]] as const;

test('FA1: phone controls outside the shell keep the prototype sizes, and the invites section keeps the settings grammar', async ({ page }) => {
  const h = started();
  await page.setViewportSize({ width: 390, height: 844 });
  const account = newAccount('Fia', h.control);
  const invite = h.invite.split(',')[0];
  await page.goto(`/welcome?invite=${encodeURIComponent(invite)}`);
  await expect(page.getByRole('heading', { name: copy('onboarding.connect.title', { instance }), exact: true })).toBeVisible({ timeout: WAIT });
  await expectPrototypeSizes(page, 'onboarding connect step at 390', AUTH_SIZES);
  await page.getByRole('button', { name: copy('onboarding.connect.signIn'), exact: true }).click();
  await expect(page.getByRole('heading', { name: copy('signin.login.title', { instance }), exact: true })).toBeVisible({ timeout: WAIT });
  await expectPrototypeSizes(page, 'sign-in at 390', AUTH_SIZES);
  await page.goto(`/welcome?invite=${encodeURIComponent(invite)}`);
  await completeSignUp(page, account, instance);

  // Task 10 fix round 1: Notifications is four switch and segmented rows with no button, so voice and devices carry the sizes.
  for (const section of ['voice', 'devices'] as const) {
    await page.goto(`/settings/${section}`);
    // Task 10 fix round 1: Devices draws only device rows (no settings rows), so a device row also says the section is in.
    await expect(page.locator('.d-settings-row, .d-voice-settings, .d-device-row').first()).toBeVisible({ timeout: WAIT });
    await expectPrototypeSizes(page, `settings, ${section} at 390`, section === 'voice' ? VOICE_SIZES : SETTINGS_SIZES.slice(0, 1));
  }
  // Signal task 10 fix round 1: Settings → Devices has no refresh button (the prototype's rows); the list refreshes on open.

  // The invites section on a desktop (Signal task 10c, merge s1+10c): the prototype's Invites card, two choices side by side (Expires
  // after, Max uses), each a small label over its segmented control; then the card's Copy at 390.
  await page.setViewportSize({ width: 1440, height: 900 });
  await page.goto('/');
  await expect(page.locator('.d-app-shell')).toBeVisible({ timeout: WAIT });
  await addServerVia(page, 'create');
  const start = page.getByRole('dialog', { name: copy('create.server.title'), exact: true });
  await start.getByRole('textbox', { name: copy('create.server.name'), exact: true }).fill('Invitation');
  await page.keyboard.press('Enter');
  await expect(start).toBeHidden({ timeout: WAIT });
  await expectComposerReady(page, 'general');
  await page.getByRole('button', { name: copy('admin.open'), exact: true }).click().then(() => page.getByRole('menuitem', { name: copy('signalSettings.menu.serverSettings'), exact: true }).click());
  const admin = page.getByRole('dialog', { name: copy('admin.title'), exact: true });
  await admin.getByRole('navigation', { name: copy('admin.nav'), exact: true }).getByRole('button', { name: copy('admin.nav.invites'), exact: true }).click();
  const choices = admin.locator('.d-invite-panel__choice');
  await expect(choices.first(), 'I1 the invites section is the Invites card').toBeVisible({ timeout: WAIT });
  expect.soft(await choices.count(), 'I1 expires after and max uses are two choices').toBe(2);
  expect.soft(await admin.locator('.d-select__label').count(), 'I1 no stacked micro-uppercase label is left').toBe(0);
  const labels = await boxes(page, '.d-invite-panel__label', '.d-invite-panel__card');
  expectOne('I1 the two choice labels share one top', labels.map(b => b.y), labels.map(b => b.t));
  const controls = await boxes(page, '.d-invite-panel__choice > :last-child', '.d-invite-panel__card');
  expect(controls.length).toBe(labels.length);
  for (const [i, label] of labels.entries()) {
    expect(Math.abs(controls[i].x - label.x), `I1 ${label.t}: the control starts under its label`).toBeLessThan(1);
    expect(controls[i].y, `I1 ${label.t}: the control sits below its label`).toBeGreaterThanOrEqual(label.b);
  }

  // On a phone: the same section's buttons and the link dialog's field and Copy keep the prototype's sizes (32; 36 and 36).
  await page.keyboard.press('Escape');
  await expect(admin).toBeHidden({ timeout: WAIT });
  await page.setViewportSize({ width: 390, height: 844 });
  await expect(page.locator('.d-app-shell')).toHaveAttribute('data-layout', 'phone', { timeout: WAIT });
  const back = page.getByRole('button', { name: copy('nav.back'), exact: true });
  if (await back.isVisible().catch(() => false)) await back.click();
  await expect(page.locator('.d-app-shell')).toHaveAttribute('data-pane', 'list', { timeout: WAIT });
  await page.getByRole('button', { name: copy('admin.open'), exact: true }).click().then(() => page.getByRole('menuitem', { name: copy('signalSettings.menu.serverSettings'), exact: true }).click());
  await expect(admin).toBeVisible({ timeout: WAIT });
  await admin.getByRole('navigation', { name: copy('admin.nav'), exact: true }).getByRole('button', { name: copy('admin.nav.invites'), exact: true }).click();
  await expect(admin.getByRole('button', { name: copy('serverSettings.invites.make'), exact: true })).toBeVisible({ timeout: WAIT });
  // Signal task 10c: the Invites card has no .d-btn; its accent "Make invite link" is the prototype's 36px button.
  await expectPrototypeSizes(page, 'server settings, invites at 390', [['.d-invite-panel__make', 36]]);
  await admin.getByRole('button', { name: copy('serverSettings.invites.make'), exact: true }).click();
  // Signal task 10c: the link shows once in the Invites card: a 38px field (36px and its 1px ring) beside a 36px Copy.
  await expect(admin.getByRole('button', { name: copy('serverSettings.invites.copy'), exact: true }), 'the card shows its Copy').toBeVisible({ timeout: WAIT });
  await expectPrototypeSizes(page, 'the invite link at 390', [['.d-invite-panel__link', 38], ['.d-invite-panel__made .d-invite-panel__primary', 36]]);

  // Task 11 (merge 9+10 minor): with a server selected, Notifications has its segmented choice and no button, so the choice
  // carries the size there: each option is the prototype's 26px (README §5.9; the Sg option's height:26px), not 44px.
  await page.keyboard.press('Escape');
  await expect(admin).toBeHidden({ timeout: WAIT });
  // Opened from the app (You → Open settings), so Settings knows the server; a reload of /settings/notifications would not.
  await page.getByRole('navigation', { name: copy('nav.tabs.label'), exact: true }).getByRole('link', { name: new RegExp(`^${copy('nav.tab.you')}`) }).click();
  await page.getByRole('button', { name: copy('you.settings'), exact: true }).click();
  await page.getByRole('navigation', { name: copy('settings.nav.label'), exact: true }).getByRole('button', { name: copy('workspace.notifications'), exact: true }).click();
  await expect(page.locator('.d-settings-frame__body .d-segmented__option').first()).toBeVisible({ timeout: WAIT });
  await expectPrototypeSizes(page, 'settings, notifications at 390', [['.d-settings-frame__body .d-segmented__option', 26]]);
});


// ---------------------------------------------------------------------------------------------------
// web3.9 FA2 (AUDIT-2 N1, N2, N4, D4): the DM and voice layout. Each visual element has its room: the desktop call is an icon in
// the DM header (no band under it), "message someone" an icon in the DM list's head (no footer), the voice preview keeps its
// title, people and join in one column at the top, and a phone's call controls pin directly above the tab bar.

test('the DM and the voice layout give each element its room', async ({ page, playwright, browserName, launchOptions, headless, channel, guard }, testInfo) => {
  test.skip(browserName === 'webkit', 'a call needs the media stack; this check measures Chromium and Firefox');
  const h = started();
  await page.setViewportSize({ width: 1440, height: 900 });
  await signUp(page, h.invite.split(',')[0], instance, 'Bea', h.control);
  await addServerVia(page, 'create');
  const start = page.getByRole('dialog', { name: copy('create.server.title'), exact: true });
  await start.getByRole('textbox', { name: copy('create.server.name'), exact: true }).fill('Ydalir');
  await page.keyboard.press('Enter');
  await expect(start).toBeHidden({ timeout: WAIT });
  await expectComposerReady(page, 'general');
  const link = await makeInviteLink(page, 'Ydalir');
  const secondDir = secondProfileDir(testInfo.project.name, testInfo.workerIndex);
  const second = await launchSecond(playwright, browserName, { launchOptions: friendLaunch(browserName, launchOptions) as typeof launchOptions, headless, channel, viewport: { width: 1440, height: 900 }, baseURL: h.url }, secondDir);
  try {
    const cy = second.pages()[0] ?? (await second.newPage());
    await cy.goto(new URL(link).pathname);
    const openLink = cy.getByRole('link', { name: 'Open dilla in this browser', exact: true });
    await expect(openLink).toBeVisible({ timeout: WAIT });
    await guard.attach(second);
    await openLink.click();
    await completeSignUp(cy, newAccount('Cy', h.control), instance);
    await expect(railItem(cy, 'Ydalir')).toBeVisible({ timeout: WAIT });
    await homeTile(cy).click();
    await cy.getByRole('button', { name: copy('shell.dms.new'), exact: true }).click();
    await cy.getByRole('dialog', { name: copy('shell.dms.newTitle'), exact: true }).getByRole('button', { name: copy('shell.dms.open'), exact: true }).click();
    const dmBox = cy.getByRole('textbox', { name: copy('shell.dm.composer', { name: 'Bea' }), exact: true });
    await expect(dmBox).toBeEnabled({ timeout: WAIT });
    await dmBox.fill('hello Bea');
    await dmBox.press('Enter');
    await expect(cy.getByRole('log').locator('.d-message-row').filter({ hasText: 'hello Bea' })).toHaveAttribute('data-state', 'ok', { timeout: WAIT });

    // The AFK channel's path: a click on its desktop row opens it and marks the person away, with no call (AW5); its stage is the preview.
    await nav(page).getByRole('button', { name: copy('voice.open', { name: 'AFK' }), exact: true }).click();
    await expect(page.locator('.dw-voice-stage')).toBeVisible({ timeout: WAIT });
    const afkPath = new URL(page.url()).pathname;

    // N1, N2: the DM list and the DM.
    await homeTile(page).click();
    await expect(dmsNav(page).getByRole('button', { name: /^Cy/ })).toBeVisible({ timeout: WAIT });
    await expect.soft(page.locator('.d-channel-list__head').getByRole('button', { name: copy('shell.dms.new'), exact: true }), 'N2 "message someone" is a button in the DM list head').toBeVisible({ timeout: WAIT });
    expect.soft(await boxes(page, '.d-channel-list__footer'), 'N2 the DM list has no footer row').toEqual([]);
    expect.soft(await page.locator('.d-channel-list').getByRole('button', { name: copy('shell.dms.new'), exact: true }).count(), 'N2 one "message someone" action').toBe(1);
    const dmHead = (await boxes(page, '.d-channel-list__head'))[0];
    const dmNew = (await boxes(page, '.d-channel-list__head button'))[0];
    if (dmHead !== undefined && dmNew !== undefined) {
      expect.soft(dmNew.y >= dmHead.y - TOL && dmNew.b <= dmHead.b + TOL, 'N2 the action stands inside the 48 head').toBe(true);
    }
    await dmsNav(page).getByRole('button', { name: /^Cy/ }).click();
    await expect(page.getByRole('log')).toBeVisible({ timeout: WAIT });
    const header = (await boxes(page, '.d-channel-header'))[0];
    // Ruling Z (Jonas 2026-10-27): the DM header is the prototype's (name and topic); the call is the DM row's menu item.
    await expect.soft(page.locator('.d-channel-header').getByRole('button'), 'N1 the DM header has no buttons').toHaveCount(0);
    await dmsNav(page).getByRole('button', { name: /^Cy/ }).click({ button: 'right' });
    await expect.soft(page.getByRole('menuitem', { name: copy('call.start'), exact: true }), 'N1 the call is in the DM row menu').toBeVisible({ timeout: WAIT });
    await page.keyboard.press('Escape');
    expect.soft((await boxes(page, '.dw-call-strip')).map(b => b.h), 'N1 no band under the header: the call strip is collapsed').toEqual((await boxes(page, '.dw-call-strip')).map(() => 0));
    const log = (await boxes(page, '[role="log"]'))[0];
    if (header !== undefined && log !== undefined) expect.soft(log.y - header.b, 'N1 the log starts directly under the header').toBeLessThanOrEqual(8);

    // N4: the AFK preview, one column at the top.
    await page.goto(afkPath);
    await expect(page.locator(AFK_ACTION)).toBeVisible({ timeout: WAIT });
    // AW5: the AFK channel hosts no call, so its preview has no tiles.
    await expect(page.locator('.dw-voice-stage .d-call-stage__tile'), 'N4 the AFK preview is empty').toHaveCount(0, { timeout: WAIT });
    const join = (await boxes(page, AFK_ACTION))[0];
    const texts = await boxes(page, '.d-call-stage__outgoing, .d-call-stage__empty, .d-call-stage__title');
    expect.soft(texts.length, 'N4 the preview has its text').toBeGreaterThan(0);
    if (join !== undefined && texts.length > 0) {
      const lowest = Math.max(...texts.map(b => b.b));
      expect.soft(join.y - lowest, `N4 the join sits within 120 of its text (${texts.map(b => `${b.t}@${b.x.toFixed(0)},${b.y.toFixed(0)}`).join(' | ')} join@${join.x.toFixed(0)},${join.y.toFixed(0)})`).toBeLessThanOrEqual(120);
      // The empty room is one centred group: each text and the join share the main area's horizontal centre.
      const main = (await boxes(page, '.d-app-shell__content'))[0];
      if (main !== undefined) {
        const mid = main.x + main.w / 2;
        for (const b of [...texts, join]) {
          expect.soft(Math.abs(b.x + b.w / 2 - mid), `N4 "${b.t}" is centred on the main area (centre ${(b.x + b.w / 2).toFixed(1)}, main ${mid.toFixed(1)})`).toBeLessThanOrEqual(0.5);
        }
      }
    }
    await expectNoOverflow(page, '1440x900', 'the AFK preview');

    // D4: the phone's call stage fills the view and its controls end directly above the tab bar.
    await page.setViewportSize({ width: 390, height: 844 });
    await expect(page.locator('.d-app-shell')).toHaveAttribute('data-layout', 'phone', { timeout: WAIT });
    const afkAction = page.locator(AFK_ACTION);
    await expect(afkAction).toBeVisible({ timeout: WAIT });
    const pjoin = (await boxes(page, AFK_ACTION))[0];
    const ptexts = await boxes(page, '.d-call-stage__outgoing, .d-call-stage__empty');
    if (pjoin !== undefined && ptexts.length > 0) {
      expect.soft(pjoin.y - Math.max(...ptexts.map(b => b.b)), 'N4 on a phone the join sits within 120 of its text').toBeLessThanOrEqual(120);
    }
    // AW5: the AFK channel hosts no call, so D4 measures the call stage of voice general, joined with a microphone. Cy's browser
    // has the fake one (Firefox's default page has none and would hold the join at the permission prompt): the phone's
    // server list, a tap on its row (a phone's tap only opens the stage, R24), then its join.
    await cy.setViewportSize({ width: 390, height: 844 });
    await cy.goto(afkPath.split('/').slice(0, 3).join('/'));
    await expect(cy.locator('.d-app-shell')).toHaveAttribute('data-layout', 'phone', { timeout: WAIT });
    await nav(cy).getByRole('button', { name: copy('voice.open', { name: 'general' }), exact: true }).click();
    const phoneJoin = cy.locator('.d-call-stage__join');
    await expect(phoneJoin).toBeVisible({ timeout: WAIT });
    await phoneJoin.click();
    await expect(cy.locator('.dw-voice-stage')).toHaveAttribute('data-call-phase', 'in', { timeout: WAIT });
    await expectNoPhoneReminders(cy, 'joined voice at 390x844');
    const tabs = (await boxes(cy, '.d-tab-bar'))[0];
    const controls = (await boxes(cy, '.d-call-stage__controls'))[0];
    expect.soft(tabs !== undefined && controls !== undefined, 'D4 the voice controls and tab bar are on screen').toBe(true);
    await expect(cy.locator('.dw-voice-music')).toHaveCount(0);
    if (tabs !== undefined && controls !== undefined) {
      expect.soft(tabs.y - controls.b, `D4 the voice controls end directly above the tab bar (controls ${controls.y.toFixed(0)}..${controls.b.toFixed(0)}, tab bar ${tabs.y.toFixed(0)})`).toBeLessThanOrEqual(8);
      expect.soft(tabs.y - controls.b, 'D4 the voice controls do not overlap the tab bar').toBeGreaterThanOrEqual(-TOL);
    }
  } finally {
    await second.close();
    rmSync(secondDir, { recursive: true, force: true });
  }
});

// ---------------------------------------------------------------------------------------------------
// web3.9 FA4 (AUDIT-2 J1, H5, H6, I6, I7): search results with #channel, author and time and a KeyHint footer; the You tab's icons
// with sign out; one phone settings entry point and an index/back traversal; the admin reorder as arrow icon buttons with truncated names; privacy
// "Compare with ..." as ghost buttons. Read as rects and computed styles, with axe in all three themes where the surface is new.

const THEMES = ['signal', 'light', 'high-contrast'] as const;
async function expectAccessibleInThemes(page: Page, state: string): Promise<void> {
  const before = await page.evaluate(() => document.documentElement.dataset.theme ?? '');
  try {
    for (const theme of THEMES) {
      await page.evaluate(t => { document.documentElement.dataset.theme = t; }, theme);
      await expectAccessible(page, `${state} in the ${theme} theme`);
    }
  } finally { await page.evaluate(t => { if (t === '') delete document.documentElement.dataset.theme; else document.documentElement.dataset.theme = t; }, before); }
}

test('FA4: search rows, phone settings index, channel reorder and privacy Compare', async ({ page, playwright, browserName, launchOptions, headless, channel }, testInfo) => {
  const h = started();
  const CHAN = 'raid-planning-for-the-bridge-troll-and-the-long-winter-campaign';
  await page.setViewportSize({ width: 1440, height: 900 });
  await signUp(page, h.invite.split(',')[0], instance, 'Cleo', h.control);
  await addServerVia(page, 'create');
  const start = page.getByRole('dialog', { name: copy('create.server.title'), exact: true });
  await start.getByRole('textbox', { name: copy('create.server.name'), exact: true }).fill(SERVER);
  await page.keyboard.press('Enter');
  await expect(start).toBeHidden({ timeout: WAIT });
  await expectComposerReady(page, 'general');
  await makeCategory(page, PLAN);
  await makeChannelIn(page, PLAN, 'text', CHAN);
  await makeChannelIn(page, PLAN, 'text', 'second');
  await channelRow(page, 'general').click();
  await expectComposerReady(page, 'general');
  await sendText(page, 'general', 'lamp from Cleo');
  const link = await makeInviteLink(page, SERVER);
  const secondDir = secondProfileDir(testInfo.project.name, testInfo.workerIndex);
  const second = await launchSecond(playwright, browserName, { launchOptions, headless, channel, viewport: { width: 1440, height: 900 }, baseURL: h.url }, secondDir);
  try {
    const mira = second.pages()[0] ?? (await second.newPage());
    await mira.goto(new URL(link).pathname);
    const openLink = mira.getByRole('link', { name: 'Open dilla in this browser', exact: true });
    await expect(openLink).toBeVisible({ timeout: WAIT });
    await openLink.click();
    await completeSignUp(mira, newAccount(FRIEND, h.control), instance);
    await railItem(mira, SERVER).click();
    await channelRow(mira, 'general').click();
    await expectComposerReady(mira, 'general');
    await sendText(mira, 'general', 'lamp from Mira');
    await expect(page.getByRole('log').getByText('lamp from Mira')).toBeVisible({ timeout: WAIT });

    // J1: each result names #channel, author and time; a footer lists the three keys; the palette is at most 40rem.
    await page.keyboard.press('Control+k');
    await page.getByRole('combobox', { name: copy('search.placeholder'), exact: true }).fill('lamp');
    const mine = page.getByRole('option', { name: /lamp from Mira/ });
    await expect(mine).toBeVisible({ timeout: WAIT });
    await attempt('J1 search result', async () => {
      const text = (await mine.innerText()).replace(/\s+/g, ' ');
      expect.soft(text, 'J1 the result names its channel with a hash').toContain('#general');
      expect.soft(text, 'J1 the result names its author').toContain(FRIEND);
      expect.soft(text, 'J1 the result shows a time').toMatch(/\d{1,2}:\d{2}/);
      const own = (await page.getByRole('option', { name: /lamp from Cleo/ }).innerText()).replace(/\s+/g, ' ');
      expect.soft(own, 'J1 the own result names its author').toContain('Cleo');
      // Task 9 (Signal §5.8): the footer is the prototype's "↑↓ move  ↵ open … Searches messages held in this browser"; esc is the input row's cap.
      const hints = await boxes(page, '.d-search-palette__hints > span');
      expect.soft(hints.map(k => k.t), 'J1 the footer lists the keys').toEqual([`↑↓ ${copy('layout.search.hint.move')}`, `↵ ${copy('layout.search.hint.open')}`]);
      await expect.soft(page.locator('.d-search-palette__note')).toHaveText(copy('search.note'));
      const list = (await boxes(page, '.d-search-palette [role="listbox"]'))[0];
      const dialog = (await boxes(page, '.d-search-palette'))[0];
      expect.soft(hints.length > 0 && list !== undefined && hints[0].y >= list.b - TOL, 'J1 the footer stands under the results').toBe(true);
      expect.soft(dialog?.w ?? 999, 'J1 the palette is at most 600px (Signal §5.8)').toBeLessThanOrEqual(600 + TOL);
      const meta = (await boxes(page, '.d-search-palette__meta'))[0];
      expect.soft(meta !== undefined && meta.h < 24, `J1 the meta line is one line (${String(meta?.h)})`).toBe(true);
    });
    await expectAccessibleInThemes(page, 'the search palette with results');
    await page.keyboard.press('Escape');
    await expect(page.locator('dialog.d-search-palette-dialog')).toBeHidden({ timeout: WAIT });

    // I6: move up/down are 32px arrow icon buttons in trailing slots; one right edge; the long name is cut to one line.
    await page.getByRole('button', { name: copy('admin.open'), exact: true }).click().then(() => page.getByRole('menuitem', { name: copy('signalSettings.menu.serverSettings'), exact: true }).click());
    const admin = page.getByRole('dialog', { name: copy('admin.title'), exact: true });
    await admin.getByRole('navigation', { name: copy('admin.nav'), exact: true }).getByRole('button', { name: copy('admin.nav.channels'), exact: true }).click();
    await expect(admin.locator('.dw-admin-row').first()).toBeVisible({ timeout: WAIT });
    await attempt('I6 channel reorder', async () => {
      expect.soft(await admin.locator('[data-move$=":up"]').count(), 'I6 a move-up control per channel and category').toBeGreaterThan(2);
      for (const kind of ['up', 'down']) {
        const all = await boxes(page, `[data-move$=":${kind}"]`, '.d-settings-frame__body');
        for (const b of all) { expectAt(`I6 move ${kind} is 32 wide`, b.w, 32, 0.5); expectAt(`I6 move ${kind} is 32 high`, b.h, 32, 0.5); }
        const glyphs = await admin.locator(`[data-move$=":${kind}"] svg`).count();
        expect.soft(glyphs, `I6 every move ${kind} is an icon`).toBe(all.length);
      }
      expect.soft(await admin.locator('[data-move]').evaluateAll(els => els.map(e => (e.textContent ?? '').trim()).filter(Boolean)), 'I6 the moves carry no visible text').toEqual([]);
      // Task 10 (README §5.9): ManageList rows are the prototype's 48 (8px 16px around the 32px slots) in one hairline group; a row
      // under another carries the 1px hairline, so heights are 48 or 49 and the row pitch is one value.
      const rows = await boxes(page, '.dw-admin-row', '.d-settings-frame__body');
      const ownH = (bs: readonly { h: number }[]) => bs.map(b => Math.floor(b.h));
      expect.soft(Math.max(...ownH(rows)) - Math.min(...ownH(rows)), 'I6 every channel row is as high as the others (hairline aside): ' + rows.map(b => b.t + '=' + b.h.toFixed(1)).join(' | ')).toBeLessThanOrEqual(1);
      expect.soft(rows[0]?.h ?? 99, 'I6 the long-named row is 48 high (+1 hairline), not wrapped').toBeLessThanOrEqual(49.5);
      const trail = await boxes(page, '.dw-admin-row .d-manage-list__trail', '.d-settings-frame__body');
      expectOne('I6 the trailing icons share one right edge', trail.map(b => b.r), trail.map(b => b.t));
      // The longest name fits a wide dialog; at a phone's width it must be cut to one line, with the row still 44 high.
      await page.setViewportSize({ width: 390, height: 844 });
      const name = admin.locator('.dw-admin-row .dw-admin-name', { hasText: CHAN });
      await expect.poll(() => name.evaluate(e => {
        const row = e.closest('.dw-admin-row');
        return e.getBoundingClientRect().height > 0 && (row?.getBoundingClientRect().height ?? 0) > 0
          && e.scrollWidth > e.clientWidth;
      }), { message: 'I6 the visible long name is cut at 390', timeout: WAIT }).toBe(true);
      const cut = await name.evaluate(e => ({ h: e.getBoundingClientRect().height, ellipsis: getComputedStyle(e).textOverflow, ws: getComputedStyle(e).whiteSpace, row: e.closest('.dw-admin-row')?.getBoundingClientRect().height ?? 0 }));
      expect.soft(cut.h, 'I6 the long name is one line high at 390').toBeLessThan(26);
      const rowsPhone = await boxes(page, '.dw-admin-row', '.d-settings-frame__body');
      expect.soft(Math.max(...ownH(rowsPhone)) - Math.min(...ownH(rowsPhone)), 'I6 every channel row is as high as the others at 390 (one line each): ' + rowsPhone.map(b => b.t + '=' + b.h.toFixed(1)).join(' | ')).toBeLessThanOrEqual(1);
      expect.soft(cut.row, 'I6 the long-named row is the 44 slots plus the 8px paddings and a hairline (<= 61) at 390').toBeLessThanOrEqual(61);
      expect.soft(cut.ellipsis === 'ellipsis' && cut.ws === 'nowrap', `I6 the long name is cut with an ellipsis (${JSON.stringify(cut)})`).toBe(true);
      const trailPhone = await boxes(page, '.dw-admin-row .d-manage-list__trail', '.d-settings-frame__body');
      expectOne('I6 the trailing icons share one right edge at 390', trailPhone.map(b => b.r), trailPhone.map(b => b.t));
      await page.setViewportSize({ width: 1440, height: 900 });
    });
    await expectAccessibleInThemes(page, 'the admin channel list');
    await page.keyboard.press('Escape');
    await expect(admin).toBeHidden({ timeout: WAIT });

    // I7: every Compare is a ghost button; at most one accent action in the section.
    const settings = await openSettings(page, 'privacy');
    const compare = settings.getByRole('button', { name: copy('privacy.compare', { name: FRIEND }), exact: true });
    await expect(compare).toBeVisible({ timeout: WAIT });
    await attempt('I7 privacy compare', async () => {
      expect.soft(await compare.getAttribute('data-variant'), 'I7 Compare is a ghost button').toBe('ghost');
      expect.soft(await settings.locator('.dw-settings-section [data-variant="accent"]').count(), 'I7 at most one accent action in the section').toBeLessThanOrEqual(1);
      const look = await compare.evaluate(e => ({ bg: getComputedStyle(e).backgroundColor, h: e.getBoundingClientRect().height }));
      expect.soft(look.bg, 'I7 a ghost button has no fill').toMatch(/rgba\(0, 0, 0, 0\)|transparent/);
      expect.soft(look.h, 'I7 Compare is one line high').toBeLessThanOrEqual(44);
    });
    await expectAccessibleInThemes(page, 'privacy');
    await closeSettings(page);

    // On phone, You has one settings entry point. The modal index and section replace the shell's tab bar.
    await page.setViewportSize({ width: 390, height: 844 });
    const tabs = page.getByRole('navigation', { name: copy('nav.tabs.label'), exact: true });
    await tabs.getByRole('link', { name: copy('nav.tab.you'), exact: true }).click();
    const settingsEntry = page.locator('[data-settings-section="index"]');
    await expect(settingsEntry).toBeVisible({ timeout: WAIT });
    await expect(settingsEntry).toContainText(copy('you.settings'));
    await expect(page.locator('[data-settings-section]')).toHaveCount(1);
    await expectAccessibleInThemes(page, 'the You tab');
    await settingsEntry.click();
    const phoneSettings = settingsDialog(page);
    await expect(phoneSettings).toBeVisible({ timeout: WAIT });
    await expect(page).toHaveURL('/settings', { timeout: WAIT });
    await expect(phoneSettings.getByRole('button', { name: copy('settings.close'), exact: true })).toBeVisible();
    await expect(phoneSettings.getByRole('button', { name: copy('workspace.privacy'), exact: true })).toBeVisible();
    await phoneSettings.getByRole('button', { name: copy('workspace.privacy'), exact: true }).click();
    await expect(page).toHaveURL('/settings/privacy', { timeout: WAIT });
    // Task 10 (README §5.10): chips above the section, no Back.
    await expect(phoneSettings.getByRole('button', { name: copy('workspace.back'), exact: true })).toHaveCount(0);
    await expect(phoneSettings.getByRole('button', { name: copy('workspace.privacy'), exact: true })).toHaveAttribute('aria-current', 'page');
    await expect(page.locator('.d-app-shell').getByRole('navigation', { name: copy('nav.tabs.label'), exact: true })).toHaveCount(0);
    await expectAccessibleInThemes(page, 'a phone settings section');
    await page.goBack();
    await expect(page).toHaveURL('/settings', { timeout: WAIT });
    await expectAccessibleInThemes(page, 'the phone settings index');
    await page.keyboard.press('Escape');
    await expect(phoneSettings).toBeHidden({ timeout: WAIT });
  } finally {
    await second.close();
    rmSync(secondDir, { recursive: true, force: true });
  }
});

// FB: the Home sidebar of a desktop is titled for the DMs, and at 360 a file card under images keeps its name inside the log.
test('FB1, FB2: the desktop Home title is not the server name; a file card stacks under the images at 360 and keeps its name', async ({ page }) => {
  const h = started();
  await page.setViewportSize({ width: 1440, height: 900 });
  await signUp(page, h.invite.split(',')[0], instance, 'Fia', h.control);
  await addServerVia(page, 'create');
  const start = page.getByRole('dialog', { name: copy('create.server.title'), exact: true });
  await start.getByRole('textbox', { name: copy('create.server.name'), exact: true }).fill('Vanaheim');
  await page.keyboard.press('Enter');
  await expect(start).toBeHidden({ timeout: WAIT });
  await expectComposerReady(page, 'general');
  const fileName = 'quarterly-planning-notes-for-the-long-winter-campaign.bin';
  const drawn = await drawImage(page, { width: 320, height: 240, type: 'image/png', seed: 3 });
  const drawn2 = await drawImage(page, { width: 320, height: 240, type: 'image/png', seed: 4 });
  await attachInput(page).setInputFiles([
    { name: 'one.png', mimeType: 'image/png', buffer: drawn.bytes },
    { name: 'two.png', mimeType: 'image/png', buffer: drawn2.bytes },
    { name: fileName, mimeType: 'application/octet-stream', buffer: Buffer.alloc(70_000, 7) },
  ]);
  await expect(trayEntries(page)).toHaveCount(3, { timeout: WAIT });
  for (let i = 0; i < 3; i++) await expect(trayEntries(page).nth(i)).toHaveAttribute('data-phase', 'ready', { timeout: WAIT });
  const text = `mixed ${String(Date.now())}`;
  await sendText(page, 'general', text);
  await expect(messageRow(page, 'general', text)).toHaveAttribute('data-state', 'ok', { timeout: WAIT });

  const channelPath = new URL(page.url()).pathname;
  await homeTile(page).click();
  const title = dmsNav(page).getByRole('heading', { level: 1 });
  await expect.soft(title, 'FB1 the Home sidebar is titled for the direct messages').toHaveText(copy('shell.dms.title'), { timeout: WAIT });
  await expect.soft(title, 'FB1 not the server name').not.toHaveText('Vanaheim');

  await page.setViewportSize({ width: 360, height: 740 });
  await page.goto(channelPath);
  const row = messageRow(page, 'general', text);
  await expect(row).toBeVisible({ timeout: WAIT });
  await expect(row.locator('.d-attachment-card__file .d-attachment-card__name')).toBeVisible({ timeout: WAIT });
  // FB2 compares rects with each other, so all of them are read in one frame, once the row has settled: both thumbnails
  // loaded and decoded (a thumbnail takes its card's place) and no rect moving across two frames (the log keeps its end
  // in view while it fills). Read one call apart, the card was taken before such a move and the images after it: CI run
  // 39727747643 put the card at y=545 and, a moment later, its own name at y=592.
  type Settled = { log: Box | undefined; card: Box | undefined; name: Box | undefined; images: Box[] };
  const got: { v: Settled | null } = { v: null };
  await expect.poll(async () => {
    got.v = await row.evaluate(async (el): Promise<Settled | null> => {
      const thumbs = [...el.querySelectorAll<HTMLImageElement>('.d-attachment-card[data-kind="image"] img')];
      if (thumbs.length < 2 || thumbs.some(i => !i.complete || i.naturalWidth === 0)) return null;
      await Promise.all(thumbs.map(i => i.decode().catch(() => undefined)));
      const box = (e: Element): Box => {
        const r = e.getBoundingClientRect();
        return { x: r.x, y: r.y, w: r.width, h: r.height, r: r.right, b: r.bottom, t: (e.getAttribute('aria-label') ?? e.textContent ?? '').trim().slice(0, 32) };
      };
      const read = (): Settled => {
        const log = document.querySelector('[role="log"]');
        const card = el.querySelector('.d-attachment-card[data-kind="file"]');
        const name = el.querySelector('.d-attachment-card__file .d-attachment-card__name');
        return { log: log === null ? undefined : box(log), card: card === null ? undefined : box(card), name: name === null ? undefined : box(name),
          images: [...el.querySelectorAll('.d-attachment-card[data-kind="image"]')].map(box) };
      };
      const frame = () => new Promise<void>(done => { requestAnimationFrame(() => { done(); }); });
      const before = read();
      await frame();
      await frame();
      const after = read();
      return JSON.stringify(before) === JSON.stringify(after) ? after : null;
    });
    return got.v !== null;
  }, { message: 'FB2 the row has both thumbnails decoded and holds still', timeout: WAIT }).toBe(true);
  const { log, card, name, images } = got.v ?? { log: undefined, card: undefined, name: undefined, images: [] as Box[] };
  expect(log !== undefined && card !== undefined && name !== undefined && images.length > 0, 'FB2 the log, an image card and the file card are drawn').toBe(true);
  if (log === undefined || card === undefined || name === undefined) return;
  expect.soft(name.w, 'FB2 the file name has room').toBeGreaterThan(40);
  expect.soft(card.r, 'FB2 the file card is not wider than the log').toBeLessThanOrEqual(log.r + TOL);
  expect.soft(card.x, 'FB2 the file card starts inside the log').toBeGreaterThanOrEqual(log.x - TOL);
  expect.soft(card.y, 'FB2 the file card stands under the images').toBeGreaterThanOrEqual(Math.max(...images.map(b => b.b)) - TOL);
  const ellipsed = await page.locator('.d-attachment-card__file .d-attachment-card__name').first().evaluate(e => getComputedStyle(e).textOverflow);
  expect.soft(ellipsed, 'FB2 a long name is cut with an ellipsis').toBe('ellipsis');
});

// FC: "each visual element has its room". A file card beside an image keeps its save button's text whole at 1280 and at 1024
// with the members aside open, and the card stays inside the log; the button's accessible name still carries the file name.
test('FC: the save button of a file card beside an image is not clipped at 1280 and at 1024 with the members open', async ({ page }) => {
  const h = started();
  await page.setViewportSize({ width: 1280, height: 800 });
  await signUp(page, h.invite.split(',')[0], instance, 'Fia', h.control);
  await addServerVia(page, 'create');
  const start = page.getByRole('dialog', { name: copy('create.server.title'), exact: true });
  await start.getByRole('textbox', { name: copy('create.server.name'), exact: true }).fill('Vanaheim');
  await page.keyboard.press('Enter');
  await expect(start).toBeHidden({ timeout: WAIT });
  await expectComposerReady(page, 'general');
  const drawn = await drawImage(page, { width: 320, height: 240, type: 'image/png', seed: 5 });
  await attachInput(page).setInputFiles([
    { name: 'saturday-map.png', mimeType: 'image/png', buffer: drawn.bytes },
    { name: 'loot-list.txt', mimeType: 'text/plain', buffer: Buffer.from('rope\ntorches\nshields\n') },
  ]);
  await expect(trayEntries(page)).toHaveCount(2, { timeout: WAIT });
  for (let i = 0; i < 2; i++) await expect(trayEntries(page).nth(i)).toHaveAttribute('data-phase', 'ready', { timeout: WAIT });
  const text = `loot ${String(Date.now())}`;
  await sendText(page, 'general', text);
  await expect(messageRow(page, 'general', text)).toHaveAttribute('data-state', 'ok', { timeout: WAIT });

  const measure = async (tag: string): Promise<void> => {
    const row = messageRow(page, 'general', text);
    const save = row.getByRole('button', { name: copy('shell.attachment.save', { name: 'loot-list.txt' }), exact: true });
    await expect(save, `${tag} the save button is named for the file`).toBeVisible({ timeout: WAIT });
    // Task 7 (§6.8): the prototype's 28px download icon; its word is the tooltip, its name the file's.
    await expect.soft(save, `${tag} its tooltip is the save word`).toHaveAttribute('title', copy('media.save'));
    const log = (await boxes(page, '[role="log"]'))[0];
    const card = (await boxes(page, '.d-attachment-card[data-kind="file"]'))[0];
    const btn = (await boxes(page, '.d-attachment-card__file .d-attachment-card__download'))[0];
    expect(log !== undefined && card !== undefined && btn !== undefined, `${tag} the log, the file card and its save are drawn`).toBe(true);
    if (log === undefined || card === undefined || btn === undefined) return;
    expect.soft(card.r, `${tag} the card is not wider than the log`).toBeLessThanOrEqual(log.r + TOL);
    expect.soft(card.x, `${tag} the card starts inside the log`).toBeGreaterThanOrEqual(log.x - TOL);
    expect.soft(btn.r, `${tag} the save button ends inside its card`).toBeLessThanOrEqual(card.r + TOL);
  };
  await measure('1280');

  await page.setViewportSize({ width: 1024, height: 768 });
  const toggle = membersToggle(page);
  if (await page.locator('.d-app-shell__aside').count() === 0 && await toggle.count() > 0) await toggle.click();
  await expect(page.locator('.d-app-shell__aside')).toBeVisible({ timeout: WAIT });
  await measure('1024 with the members aside');
});

test('a grouped row from an earlier day shows only its time of day in the gutter, which stays left of the content', async ({ page }) => {
  const h = started();
  await page.setViewportSize({ width: 1280, height: 800 });
  await signUp(page, h.invite.split(',')[0], instance, 'Gry', h.control);
  await addServerVia(page, 'create');
  const start = page.getByRole('dialog', { name: copy('create.server.title'), exact: true });
  await start.getByRole('textbox', { name: copy('create.server.name'), exact: true }).fill('Midgard');
  await page.keyboard.press('Enter');
  await expect(start).toBeHidden({ timeout: WAIT });
  await expectComposerReady(page, 'general');
  const first = `one ${String(Date.now())}`;
  const second = `two ${String(Date.now())}`;
  await sendText(page, 'general', first);
  await expect(messageRow(page, 'general', first)).toHaveAttribute('data-state', 'ok', { timeout: WAIT });
  await sendText(page, 'general', second);
  await expect(messageRow(page, 'general', second)).toHaveAttribute('data-state', 'ok', { timeout: WAIT });
  // The page clock moves two days on, so both rows are from an earlier day; the next render shows them so.
  await page.evaluate(() => { const real = Date.now.bind(Date); Date.now = () => real() + 2 * 86_400_000; });
  const third = `three ${String(Date.now())}`;
  await sendText(page, 'general', third);
  await expect(messageRow(page, 'general', third)).toHaveAttribute('data-state', 'ok', { timeout: WAIT });
  const row = messageRow(page, 'general', second);
  await expect(row).toHaveAttribute('data-grouped', 'true');
  await row.hover();
  const m = await row.evaluate(el => {
    const g = el.querySelector('.d-message-row__gutter')!;
    const main = el.querySelector('.d-message-row__main')!.getBoundingClientRect();
    return { right: g.getBoundingClientRect().right, mainLeft: main.left, text: g.textContent ?? '', title: g.getAttribute('title') ?? '', sw: g.scrollWidth, cw: g.clientWidth };
  });
  expect(m.text, 'the gutter shows a time of day with no year').not.toMatch(/\d{4}/);
  expect(m.title.length, 'the title carries the longer, dated time').toBeGreaterThan(m.text.length);
  expect(m.right, `the gutter's right edge (${m.text}) is not past the content column`).toBeLessThanOrEqual(m.mainLeft + 0.5);
  expect(m.sw, 'the gutter text is not clipped').toBeLessThanOrEqual(m.cw);
});
