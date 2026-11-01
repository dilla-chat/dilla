// web3.9 E-AFK: the built-in AFK channel, from an empty instance and from a web3.8 one. This suite starts a test host of its
// own with its in-process LiveKit, and runs a server through the shipping clients: Ada the owner and Björn a member, each in
// a desktop browser of their own, and Björn again in a phone-emulated browser (a second device of his). The instance's API
// is called by the spec only to read (ids, the AFK snapshot) and for the refusals the UI never offers (a delete, a topic
// patch and a permission overwrite of the AFK channel, each with its control on an ordinary channel); the instance clock is
// moved through the control listener, and the upgrade case points the community at an administrator's pick as web3.8
// stored it (POST /debug/afk/legacy, the only legacy hook the test host has) before the host restarts on the same data.
// That community already holds a built-in AFK (Idle) when the pointer moves; a database with no built-in at all, a missing
// row and a NULL pointer are SRV-AFK's TestEnsureAFKChannels.
// An idle move relies on noise suppression silencing the fake microphone's steady tone: speech counts as activity
// (Shell.tsx), so noise suppression stays on until step 9.
//
// What it proves (Jonas's decisions, wave plan R1-R5, brief R33-R35; AW5/AW6, R-AW1-R-AW7: AFK means disconnected):
// - every server has exactly one built-in "AFK" voice channel, listed as the very last row of the list (below every category, MP6);
// - an administrator renames it and moves it into a category, but finds no delete and no permissions for it, and the
//   instance refuses its delete (403 E_FORBIDDEN; an ordinary channel's delete passes); it has no reorder controls and
//   stays last at 2^30 in settings when a sibling moves (R-AFK8);
// - the inactive timeout is 1, 5, 15, 30 or 60 minutes, 5 by default, with no off;
// - the AFK channel never hosts a call: an idle desktop is cut by itself, `afk.moved` is said once and no removal line
//   ever, on the cut person's screen reader and on that of the other person in the call he left, and it joins nothing;
//   both lists show him under AFK as away (`afk.away`), from the away roster;
// - joining voice takes him off the roster; a click on AFK leaves the current call and lists the person away, a second
//   click does nothing, and a message in that server takes them off it (Q1);
// - a phone is never cut (its desktop twin, idle beside it in the same call, is, and is not listed away: R-AW3);
// - a member whose roles leave out view_channel community-wide still sees AFK and a click lists him away there;
// - his last connection closing takes him off the roster once a 30 s grace ends (Q2, revised), and coming back joins nothing;
// - a web3.8 server whose administrator picked an AFK gets a new one, and the old one becomes ordinary.
import { mkdirSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import {
  expect, type BrowserContext, type ConsoleMessage, type Locator, type Page, type PlaywrightWorkerArgs, type PlaywrightWorkerOptions, type Request,
} from '@playwright/test';
import {
  ABORTED_FONT_LINE, ABORTED_JUGGLER_LINE, BY_DESIGN_RESOURCE_LINE, FIREFOX_FAVICON_LINE, TEST_TIMEOUT, WAIT, addServerVia, channelRow, channelsNav, completeSignUp, copy,
  expectAccessible, expectComposerReady, makeInviteLink, newAccount, openChannel, railItem, sendText, signIn, signUp, test, type Account,
 selectRole, setRolePermission,
} from './support/app';
import { removeHostData, startTestHost, type HostConfig, type StartedHost } from './support/host';
import { PROFILE_ROOT } from './support/persistent';
import { FIREFOX_MEDIA_PREFS, chromiumMediaArgs, writeFixtures } from '../media/support/fixtures';
import { connected, dock, micLive, noiseOff, phaseOf, sendsAudio, stage, voiceRow, watchAudio } from '../voice/support/voice';
import { arr, decode, encode, type CborInput, type CborValue } from '../../packages/client-core/src/cbor/index';

/** Its own ports (web3.9 reservation): the host, its control listener, and its LiveKit's signalling and ICE ports. */
const HOST: HostConfig = { port: 8613, control: 8614, webRoot: 'packages/web/dist', keepData: true, sfu: { port: 7896, udpPort: 7898 } };
const ORIGIN = `http://127.0.0.1:${String(HOST.port)}`;
const SERVER = 'Midgard Crew';
const OWNER = 'Ada';
const FRIEND = 'Björn';
const RENAMED = 'Idle';
const CATEGORY = 'Lounge';
/** The shortest timeout (R2): one minute; the clock is moved past it by this much. */
const PAST_TIMEOUT_S = 61;
const DESKTOP = { width: 1280, height: 800 } as const;

let host: StartedHost | null = null;
let instance = '';
let communityId = '';
let afkId = '';
let generalVoiceId = '';
let bjornAccount: Account | null = null;

/** One person's browser: a persistent context of its own (the project's engine, with fake media), its page, the bearer the
 *  page's worker last sent (for the spec's reads), and what it printed that the guard refuses. */
interface Person { name: string; context: BrowserContext; page: Page; bearer: string; errors: string[]; afkPosts: number; awayPuts: number }
let ada: Person | null = null;
let bjorn: Person | null = null;
let phone: Person | null = null;
/** While the host restarts, a browser's refused connections are expected; nothing else is. */
let restarting = false;
/** While a move's cut is expected (steps 4, 6 and 8), LiveKit's lines about it are; nowhere else. */
let cutting = false;

test.describe.configure({ mode: 'serial', timeout: TEST_TIMEOUT });
test.use({ baseURL: ORIGIN });

const fixtures = writeFixtures(join(tmpdir(), 'dilla-media-fixtures'));
/** What LiveKit's client logs when the SFU removes this participant while its data channels are open: the move's cut (R4,
 *  RemoveParticipants) lands so, at random, a few times in three runs. The library's, not the app's; nothing else is let through. */
const CUT_LINE = /^(DataChannel error on (lossy|reliable): User-Initiated Abort, reason= (\{|JSHandle@object)|publisher data channel '(LOSSY|RELIABLE|DATA_TRACK_LOSSY)' closed unexpectedly (\{|JSHandle@object))/;
const RESTART_LINE = /ERR_CONNECTION_REFUSED|ERR_CONNECTION_RESET|ERR_EMPTY_RESPONSE|WebSocket connection to .* failed|NetworkError|Firefox can.t establish a connection/;

function started(): StartedHost {
  if (host === null) throw new Error('the empty test host did not start');
  return host;
}
function person(p: Person | null, who: string): Person {
  if (p === null) throw new Error(`${who}'s browser is not open`);
  return p;
}

/** Opens a persistent browser of the project's engine with fake media: the microphone is a steady 1 kHz tone. */
type Launch = Pick<PlaywrightWorkerArgs, 'playwright'> & Pick<PlaywrightWorkerOptions, 'browserName' | 'headless' | 'channel'>;
async function openPerson(name: string, f: Launch, projectName: string, workerIndex: number, device: 'desktop' | 'phone'): Promise<Person> {
  const dir = `${PROFILE_ROOT}/${projectName}-${String(workerIndex)}-afk-${name.toLowerCase()}`;
  rmSync(dir, { recursive: true, force: true });
  mkdirSync(dir, { recursive: true });
  const firefox = f.browserName === 'firefox';
  // Firefox takes prefs and has no mobile emulation (phone.spec R23): its phone is the 390 px window and a touch screen.
  const shape = device === 'desktop' ? { viewport: DESKTOP }
    : firefox ? { viewport: { width: 390, height: 844 }, hasTouch: true }
      : { viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true, deviceScaleFactor: 3 };
  const context = await f.playwright[f.browserName].launchPersistentContext(dir, {
    headless: f.headless, channel: f.channel, baseURL: ORIGIN, ...shape,
    ...(firefox ? { firefoxUserPrefs: FIREFOX_MEDIA_PREFS } : { args: chromiumMediaArgs(fixtures), permissions: ['microphone'] }),
  });
  const p: Person = { name, context, page: context.pages()[0] ?? (await context.newPage()), bearer: '', errors: [], afkPosts: 0, awayPuts: 0 };
  context.on('request', (r: Request) => {
    if (!r.url().startsWith(`${ORIGIN}/v1/`)) return;
    // AW5 (R-AW1): any call start or token mint this client asks for in the AFK channel, and each away mark it sends.
    if (afkId !== '' && r.method() === 'POST' && r.url().startsWith(`${ORIGIN}/v1/channels/${afkId}/`)) p.afkPosts += 1;
    if (r.method() === 'PUT' && r.url() === `${ORIGIN}/v1/communities/${communityId}/afk/away`) p.awayPuts += 1;
    void r.allHeaders().then((h) => { const a = h.authorization; if (a?.startsWith('Bearer ') === true) p.bearer = a; }).catch(() => undefined);
  });
  context.on('console', (m: ConsoleMessage) => {
    if (m.type() !== 'error') return;
    const text = m.text();
    if (!firefox && BY_DESIGN_RESOURCE_LINE.test(text)) return;
    if (firefox && (ABORTED_FONT_LINE.test(text) || ABORTED_JUGGLER_LINE.test(text) || FIREFOX_FAVICON_LINE.test(text))) return;
    if (restarting && RESTART_LINE.test(text)) return;
    if (cutting && CUT_LINE.test(text)) return;
    p.errors.push(`${name}: ${text}`);
  });
  context.on('weberror', (e) => { p.errors.push(`${name}: ${String(e.error())}`); });
  // Every live region's text as it changes: what a screen reader would be given to say, in order.
  await context.addInitScript(() => {
    const w = window as unknown as { __said: string[] };
    w.__said = [];
    const last = new WeakMap<Element, string>();
    const scan = () => {
      for (const el of document.querySelectorAll('[role="status"], [role="alert"], [role="log"], [aria-live]:not([aria-live="off"])')) {
        const text = (el.textContent ?? '').trim();
        if (last.get(el) === text) continue;
        last.set(el, text);
        if (text !== '') w.__said.push(text);
      }
    };
    new MutationObserver(scan).observe(document, { subtree: true, childList: true, characterData: true });
  });
  return p;
}
async function closePerson(p: Person | null): Promise<void> {
  if (p === null) return;
  await p.context.close().catch(() => undefined);
}

/** What this page's live regions said since `resetSaid`. */
const said = (p: Person) => p.page.evaluate(() => (window as unknown as { __said: string[] }).__said.slice());
const resetSaid = (p: Person) => p.page.evaluate(() => { (window as unknown as { __said: string[] }).__said = []; });

const hex = (b: unknown): string => {
  if (!(b instanceof Uint8Array)) throw new Error('not a byte string');
  return [...b].map((x) => x.toString(16).padStart(2, '0')).join('');
};
/** A read of the instance's API with this person's session (the client sends the same request). */
async function read(p: Person, path: string): Promise<CborValue> {
  expect(p.bearer, `${p.name}'s session bearer`).toMatch(/^Bearer /);
  const res = await fetch(`${ORIGIN}${path}`, { headers: { accept: 'application/cbor', authorization: p.bearer } });
  expect(res.status, `GET ${path}`).toBe(200);
  return decode(new Uint8Array(await res.arrayBuffer()));
}
/** GET /v1/communities/{id}/afk (R33, AW3): [channel_id|null, timeout_seconds, [], pending, [[user_id, since_ts]]]; `away`
 *  is how many people the roster lists. */
async function afkSnapshot(p: Person): Promise<{ channel: string | null; timeout: number; pending: boolean; away: number }> {
  const a = arr(await read(p, `/v1/communities/${communityId}/afk`));
  return { channel: a[0] === null ? null : hex(a[0]), timeout: Number(a[1]), pending: Number(a[3]) === 1, away: arr(a[4] ?? []).length };
}
/** GET /v1/communities/{id}/channels: [id, kind, mode, visibility, parent|null, name, …] per channel. */
async function channelsOf(p: Person): Promise<{ id: string; kind: number; parent: string | null; name: string }[]> {
  return arr(await read(p, `/v1/communities/${communityId}/channels`)).map((v) => {
    const a = arr(v);
    return { id: hex(a[0]), kind: Number(a[1]), parent: a[4] === null ? null : hex(a[4]), name: String(a[5]) };
  });
}
/** A write to the instance's API with this person's session, as the client sends it; answers the status and the error code. */
async function write(p: Person, method: 'PATCH' | 'PUT' | 'DELETE', path: string, body?: CborInput): Promise<{ status: number; code: string | null }> {
  const res = await fetch(`${ORIGIN}${path}`, {
    method, headers: { accept: 'application/cbor', 'content-type': 'application/cbor', authorization: p.bearer },
    ...(body === undefined ? {} : { body: Buffer.from(encode(body)) }),
  });
  const bytes = new Uint8Array(await res.arrayBuffer());
  const code = res.status >= 400 && bytes.length > 0 ? String(arr(decode(bytes))[0]) : null;
  return { status: res.status, code };
}
/** The next activity report of this person's device that the instance accepts (204): arm it before the action that sends it. */
function activityAccepted(p: Person): Promise<unknown> {
  return p.context.waitForEvent('requestfinished', { timeout: WAIT, predicate: (r) => r.method() === 'POST'
    && r.url() === `${ORIGIN}/v1/communities/${communityId}/afk/activity` }).then(async (r) => {
    expect((await r.response())?.status(), `${p.name}'s activity report`).toBe(204);
  });
}
/** Moves the instance clock forward (POST /debug/clock); the AFK sweep runs on its own 5 s tick. */
async function advanceClock(seconds: number): Promise<void> {
  const res = await fetch(`${started().control}/debug/clock`, {
    method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ seconds }),
  });
  expect(res.status, await res.text()).toBe(204);
}
/** Waits out the AFK sweep's tick (StartRetries, every 5 s of wall time) twice over, so a sweep has run since the call. */
const sweepRan = (p: Person) => p.page.waitForTimeout(11_000);

const button = (scope: Page | Locator, key: string, vars?: Record<string, string>) =>
  scope.getByRole('button', { name: copy(key, vars), exact: true });
const dialogOf = (page: Page, key: string, vars?: Record<string, string>) => page.getByRole('dialog', { name: copy(key, vars), exact: true });
const navItem = (dialog: Locator, key: string) =>
  dialog.getByRole('navigation', { name: copy('admin.nav'), exact: true }).getByRole('button', { name: copy(key), exact: true });
const panelMute = (p: Person) => p.page.locator('.d-user-panel').getByRole('button', { name: copy('voice.mute'), exact: true });

/** Opens server settings and its `section` tab; returns the dialog. */
async function serverSettings(page: Page, section: 'admin.nav.overview' | 'admin.nav.channels' | 'roles.tab'): Promise<Locator> {
  await button(page, 'admin.open').click().then(() => page.getByRole('menuitem', { name: copy('signalSettings.menu.serverSettings'), exact: true }).click());
  const dialog = dialogOf(page, 'admin.title');
  await expect(dialog.getByRole('navigation', { name: copy('admin.nav'), exact: true })).toBeVisible({ timeout: WAIT });
  const tab = navItem(dialog, section);
  if ((await tab.getAttribute('aria-current')) !== 'page') await tab.click();
  return dialog;
}
async function closeServerSettings(page: Page): Promise<void> {
  await page.keyboard.press('Escape');
  await expect(dialogOf(page, 'admin.title')).toBeHidden();
}

/** The rows of the channel list in order: a category head by its label, a channel by its row's accessible name. */
async function listOrder(page: Page): Promise<string[]> {
  return channelsNav(page).locator('ul.d-channel-list__list > li').evaluateAll((items) => items.map((li) => {
    const cat = li.querySelector('.d-channel-list__cat-label');
    if (cat !== null) return `category ${(cat.textContent ?? '').trim()}`;
    const row = li.querySelector('button');
    return (row?.getAttribute('aria-label') ?? row?.textContent ?? '').trim();
  }));
}
/** The AFK row is the last of the list's rows that stand in no category (R42: the built-in position is below every channel a
 *  client makes), a voice row with the moon glyph (Signal README §5.4). It is always the list's very last row, below every category (MP6). */
async function expectAfkLast(page: Page, name: string): Promise<void> {
  await expect(voiceRow(page, name)).toBeVisible({ timeout: WAIT });
  await expect(voiceRow(page, name).locator('[data-icon="moon"]')).toHaveCount(1);
  await expect(voiceRow(page, name).locator('[data-icon="volume-2"]')).toHaveCount(0);
  await expect.poll(async () => {
    return (await listOrder(page)).at(-1);
  }, { timeout: WAIT, message: `${name} is the very last row of the list (MP6)` }).toBe(copy('voice.open', { name }));
}
/** A new voice channel `name` in Lounge, from that category's own "+" (with categories the list head has none, A1). */
async function makeVoice(page: Page, name: string): Promise<void> {
  await button(channelsNav(page), 'layout.category.add', { name: CATEGORY }).click();
  const made = dialogOf(page, 'create.channel.title', { server: SERVER });
  await made.getByRole('radio', { name: copy('create.channel.voice'), exact: true }).click();
  await made.getByRole('textbox', { name: copy('create.channel.name'), exact: true }).fill(name);
  await page.keyboard.press('Enter');
  await expect(made).toBeHidden({ timeout: WAIT });
  await expect(voiceRow(page, name)).toBeVisible({ timeout: WAIT });
}
/** The voice room as the list shows it: the names nested under the row of `name`. */
const roomPeople = (page: Page, name: string) => voiceRow(page, name).locator('xpath=ancestor::li[1]').locator('.d-voice-row__person');

/** AW6: the people the AFK row lists away (the away roster, no voice connection). */
const awayPeople = (page: Page, name: string) => roomPeople(page, name).and(page.locator('[data-away="true"]'));

test.beforeAll(async ({ playwright, browserName, headless, channel }, testInfo) => {
  removeHostData(HOST.port);
  host = await startTestHost(HOST);
  const res = await fetch(`${host.url}/v1/instance`, { headers: { accept: 'application/cbor' } });
  expect(res.status, 'GET /v1/instance').toBe(200);
  const name = arr(decode(new Uint8Array(await res.arrayBuffer())))[5];
  if (typeof name !== 'string' || name === '') throw new Error('GET /v1/instance: element 5 is not the instance name');
  instance = name;
  const f = { playwright, browserName, headless, channel };
  ada = await openPerson(OWNER, f, testInfo.project.name, testInfo.workerIndex, 'desktop');
  bjorn = await openPerson(FRIEND, f, testInfo.project.name, testInfo.workerIndex, 'desktop');
});
test.afterAll(async () => {
  await closePerson(phone);
  await closePerson(bjorn);
  await closePerson(ada);
  await host?.stop();
  removeHostData(HOST.port);
});
// eslint-disable-next-line no-empty-pattern
test.afterEach(async ({}, testInfo) => {
  cutting = false;
  // The runner traces only its own context: on a failure the people's pages are attached as they stand.
  if (testInfo.status !== testInfo.expectedStatus) {
    for (const p of [ada, bjorn, phone]) {
      if (p === null || p.page.isClosed()) continue;
      await p.page.screenshot({ path: testInfo.outputPath(`${p.name}.png`) }).catch(() => undefined);
      writeFileSync(testInfo.outputPath(`${p.name}.aria.txt`), await p.page.locator('body').ariaSnapshot().catch(() => ''));
    }
  }

  const errors = [...(ada?.errors ?? []), ...(bjorn?.errors ?? []), ...(phone?.errors ?? [])];
  for (const p of [ada, bjorn, phone]) if (p !== null) p.errors.length = 0;
  expect(errors, 'console and page errors in the people’s browsers').toEqual([]);
});

test('1. a new server has one AFK voice channel, listed last, for the owner and for a member', async () => {
  const h = started();
  const a = person(ada, OWNER);
  const b = person(bjorn, FRIEND);
  await signUp(a.page, h.invite.split(',')[0], instance, OWNER, h.control);
  await addServerVia(a.page, 'create');
  const start = dialogOf(a.page, 'create.server.title');
  await start.getByRole('textbox', { name: copy('create.server.name'), exact: true }).fill(SERVER);
  await a.page.keyboard.press('Enter');
  await expect(start).toBeHidden({ timeout: WAIT });
  await expect(a.page).toHaveURL(/\/c\/[0-9a-f]{32}\/[0-9a-f]{32}$/, { timeout: WAIT });
  communityId = /\/c\/([0-9a-f]{32})\//.exec(new URL(a.page.url()).pathname)?.[1] ?? '';
  expect(communityId).toMatch(/^[0-9a-f]{32}$/);
  await expectComposerReady(a.page, 'general');

  await expectAfkLast(a.page, 'AFK');
  const listed = await channelsOf(a);
  expect(listed.filter((c) => c.name === 'AFK'), 'one AFK channel').toHaveLength(1);
  const afk = listed.find((c) => c.name === 'AFK');
  expect(afk?.kind, 'AFK is a voice channel').toBe(1);
  afkId = afk?.id ?? '';
  generalVoiceId = listed.find((c) => c.kind === 1 && c.name === 'general')?.id ?? '';
  expect(generalVoiceId).toMatch(/^[0-9a-f]{32}$/);
  expect((await afkSnapshot(a)).channel, 'the AFK snapshot names it').toBe(afkId);

  const link = await makeInviteLink(a.page, SERVER);
  await b.page.goto(new URL(link).pathname);
  await b.page.getByRole('link', { name: 'Open dilla in this browser', exact: true }).click();
  bjornAccount = await completeSignUp(b.page, newAccount(FRIEND, h.control), instance);
  await railItem(b.page, SERVER).click();
  await expectAfkLast(b.page, 'AFK');
  expect((await channelsOf(b)).filter((c) => c.name === 'AFK'), 'one AFK channel for Björn').toHaveLength(1);
});

test('2. Ada renames AFK "Idle" and moves it into the new category "Lounge"; Björn sees both without a reload', async () => {
  const a = person(ada, OWNER);
  const b = person(bjorn, FRIEND);
  const settings = await serverSettings(a.page, 'admin.nav.channels');
  await button(settings, 'admin.channels.newCategory').click();
  const cat = dialogOf(a.page, 'admin.category.title', { server: SERVER });
  await cat.getByRole('textbox', { name: copy('admin.category.name'), exact: true }).fill(CATEGORY);
  await button(cat, 'admin.category.submit').click();
  await expect(cat).toBeHidden({ timeout: WAIT });

  await button(settings, 'admin.channels.editLabel', { name: 'AFK' }).click();
  const edit = dialogOf(a.page, 'admin.channel.titleVoice', { name: 'AFK' });
  await expect(edit).toBeVisible({ timeout: WAIT });
  await edit.getByRole('textbox', { name: copy('admin.channel.name'), exact: true }).fill(RENAMED);
  await edit.getByRole('combobox', { name: copy('admin.channel.category'), exact: true }).selectOption({ label: CATEGORY });
  await button(edit, 'admin.channel.save').click();
  await expect(edit).toBeHidden({ timeout: WAIT });
  await closeServerSettings(a.page);

  for (const p of [a, b]) {
    await expect(voiceRow(p.page, RENAMED)).toBeVisible({ timeout: WAIT });
    await expect(voiceRow(p.page, 'AFK')).toHaveCount(0);
    // Under "Lounge": the last category head above the row is Lounge's.
    await expect.poll(async () => {
      const order = await listOrder(p.page);
      const at = order.indexOf(copy('voice.open', { name: RENAMED }));
      return order.slice(0, at).filter((r) => r.startsWith('category ')).at(-1) ?? null;
    }, { timeout: WAIT, message: `${p.name}: the category above ${RENAMED}` }).toBe(`category ${CATEGORY}`);
  }
  const moved = (await channelsOf(b)).find((c) => c.id === afkId);
  const lounge = (await channelsOf(b)).find((c) => c.kind === 2 && c.name === CATEGORY);
  expect(moved?.name).toBe(RENAMED);
  expect(moved?.parent, 'Idle is in Lounge').toBe(lounge?.id);
  expect((await afkSnapshot(b)).channel, 'still the AFK channel').toBe(afkId);
});

test('3. no delete, no permissions and no reorder for AFK in server settings, the instance refuses its delete, and it stays last when a sibling moves', async () => {
  const a = person(ada, OWNER);
  const settings = await serverSettings(a.page, 'admin.nav.channels');
  await expect(button(settings, 'admin.channels.editLabel', { name: RENAMED })).toBeVisible({ timeout: WAIT });
  await expect(button(settings, 'admin.channels.deleteLabel', { name: RENAMED })).toHaveCount(0);
  // The control: the ordinary voice channel general has its delete (text general is listed first).
  await expect(button(settings, 'admin.channels.deleteLabel', { name: 'general' }).last()).toBeVisible();

  await button(settings, 'admin.channels.editLabel', { name: RENAMED }).click();
  const edit = dialogOf(a.page, 'admin.channel.titleVoice', { name: RENAMED });
  await expect(edit.getByText(copy('afk.channel.note'), { exact: true })).toBeVisible({ timeout: WAIT });
  await expect(edit.getByRole('textbox', { name: copy('admin.channel.name'), exact: true })).toBeVisible();
  await expect(edit.getByRole('combobox', { name: copy('admin.channel.category'), exact: true })).toBeVisible();
  await expect(edit.getByRole('heading', { name: copy('chperm.title'), exact: true })).toHaveCount(0);
  await expect(edit.getByRole('switch', { name: copy('chperm.private'), exact: true })).toHaveCount(0);
  await expectAccessible(a.page, 'the AFK channel’s settings');
  await button(edit, 'admin.cancel').click();
  await expect(edit).toBeHidden();

  // The control dialog: the ordinary voice channel general shows its permissions and no AFK note.
  await settings.getByRole('button', { name: copy('admin.channels.editLabel', { name: 'general' }), exact: true }).last().click();
  const other = dialogOf(a.page, 'admin.channel.titleVoice', { name: 'general' });
  await expect(other.getByRole('heading', { name: copy('chperm.title'), exact: true })).toBeVisible({ timeout: WAIT });
  await expect(other.getByText(copy('afk.channel.note'), { exact: true })).toHaveCount(0);
  await button(other, 'admin.cancel').click();
  await expect(other).toBeHidden();
  await closeServerSettings(a.page);

  // The instance refuses the delete with Ada's own session.
  const refused = await a.page.request.delete(`/v1/channels/${afkId}`, { headers: { authorization: a.bearer, accept: 'application/cbor' } });
  expect(refused.status(), 'DELETE of the AFK channel').toBe(403);
  expect(arr(decode(new Uint8Array(await refused.body())))[0]).toBe('E_FORBIDDEN');
  // Its control, one input apart: an ordinary voice channel she makes deletes.
  await makeVoice(a.page, 'spare');
  const spare = (await channelsOf(a)).find((c) => c.name === 'spare');
  const deleted = await a.page.request.delete(`/v1/channels/${spare?.id ?? ''}`, { headers: { authorization: a.bearer, accept: 'application/cbor' } });
  expect(deleted.ok(), `DELETE of an ordinary voice channel (${String(deleted.status())})`).toBe(true);
  await expect(voiceRow(a.page, 'spare')).toHaveCount(0, { timeout: WAIT });
  expect((await channelsOf(a)).filter((c) => c.id === afkId), 'AFK is still there').toHaveLength(1);

  // R2: its settings and its permissions are refused by the instance too, each beside the same write on voice general.
  // PATCH /v1/channels/{id}: [name, topic, mode, visibility, parent, position, slowmode].
  const topic = [null, 'a topic', null, null, null, null, null];
  expect(await write(a, 'PATCH', `/v1/channels/${afkId}`, topic), 'a topic on the AFK channel').toEqual({ status: 403, code: 'E_FORBIDDEN' });
  expect(await write(a, 'PATCH', `/v1/channels/${generalVoiceId}`, topic), 'the same topic on voice general').toEqual({ status: 204, code: null });
  const everyone = arr(await read(a, `/v1/communities/${communityId}/roles`)).map((v) => arr(v)).find((row) => row[1] === '@everyone');
  const everyoneId = hex(everyone?.[0]);
  const overwrite = `/overwrites/0/${everyoneId}`;
  // [allow, deny]: allow view channels, which @everyone already holds, so the control changes nothing anyone sees.
  expect(await write(a, 'PUT', `/v1/channels/${afkId}${overwrite}`, [1, 0]), 'an overwrite on the AFK channel').toEqual({ status: 403, code: 'E_FORBIDDEN' });
  expect(await write(a, 'PUT', `/v1/channels/${generalVoiceId}${overwrite}`, [1, 0]), 'the same overwrite on voice general')
    .toEqual({ status: 204, code: null });
  expect(await write(a, 'DELETE', `/v1/channels/${afkId}${overwrite}`), 'an overwrite delete on the AFK channel').toEqual({ status: 403, code: 'E_FORBIDDEN' });
  expect(await write(a, 'DELETE', `/v1/channels/${generalVoiceId}${overwrite}`), 'the control overwrite goes again').toEqual({ status: 204, code: null });

  // R-AFK8: Idle has no reorder controls and stays last at 2^30 in Lounge's settings run when a sibling moves; the sidebar shows it as the very last row. Two ordinary voice channels
  // join it there; the last of them cannot move down past Idle, and the other moves up.
  await makeVoice(a.page, 'spare');
  await makeVoice(a.page, 'extra');
  const order = await serverSettings(a.page, 'admin.nav.channels');
  const lounge = order.getByRole('list', { name: CATEGORY, exact: true });
  const idleRow = lounge.getByRole('listitem').filter({ has: button(a.page, 'admin.channels.editLabel', { name: RENAMED }) });
  await expect(idleRow).toHaveCount(1, { timeout: WAIT });
  await expect(button(idleRow, 'admin.channels.upLabel', { name: RENAMED })).toHaveCount(0);
  await expect(button(idleRow, 'admin.channels.downLabel', { name: RENAMED })).toHaveCount(0);
  await expect(idleRow.locator('[data-move]')).toHaveCount(0);
  await expect(idleRow.locator('.d-slot'), 'Idle keeps its four trailing slots').toHaveCount(4);
  // The controls: the ordinary rows have theirs; the last one above Idle cannot move down.
  await expect(button(lounge, 'admin.channels.downLabel', { name: 'extra' })).toBeDisabled();
  await expect(button(lounge, 'admin.channels.downLabel', { name: 'spare' })).toBeEnabled();
  await button(lounge, 'admin.channels.upLabel', { name: 'extra' }).click();
  const loungeRows = () => lounge.locator('.dw-admin-name').allTextContents();
  await expect.poll(loungeRows, { timeout: WAIT, message: 'extra moved above spare, Idle last' }).toEqual(['extra', 'spare', RENAMED]);
  await closeServerSettings(a.page);
  await expect.poll(async () => {
    const rows = await listOrder(a.page);
    return rows.slice(rows.indexOf(`category ${CATEGORY}`) + 1);
  }, { timeout: WAIT, message: 'the channel list: Idle the very last row, after Lounge (the only category)' }).toEqual(['extra', 'spare', RENAMED].map((name) => copy('voice.open', { name })));
  const positions = new Map(arr(await read(a, `/v1/communities/${communityId}/channels`)).map((v) => [hex(arr(v)[0]), Number(arr(v)[7])]));
  expect(positions.get(afkId), 'Idle keeps the reserved position 2^30').toBe(2 ** 30);
  const made = (await channelsOf(a)).filter((c) => c.name === 'spare' || c.name === 'extra');
  expect(made.map((c) => positions.get(c.id) ?? Infinity).every((p) => p < 2 ** 30), 'the ordinary channels stay below 2^30').toBe(true);
  // Both go again, so the later steps see Lounge as before.
  for (const c of made) expect(await write(a, 'DELETE', `/v1/channels/${c.id}`), `DELETE ${c.name}`).toEqual({ status: 204, code: null });
  await expect(voiceRow(a.page, 'spare')).toHaveCount(0, { timeout: WAIT });
  await expect(voiceRow(a.page, 'extra')).toHaveCount(0, { timeout: WAIT });
});

test('4. one timeout row: an idle desktop in general is cut by itself, told it was moved, joins no call, and is listed away under Idle; the peer in general hears no removal', async () => {
  const a = person(ada, OWNER);
  const b = person(bjorn, FRIEND);
  const settings = await serverSettings(a.page, 'admin.nav.overview');
  // Task 11 (Jonas, Q7): the prototype's segmented 1m / 5m / 15m / 30m / 1h.
  const timeout = settings.getByRole('radiogroup', { name: copy('admin.overview.afk'), exact: true });
  await expect(timeout.getByRole('radio').first()).not.toHaveAttribute('aria-disabled', 'true', { timeout: WAIT });
  await expect(settings.locator('.d-settings-row').filter({ has: a.page.getByRole('radiogroup', { name: copy('admin.overview.afk'), exact: true }) })).toHaveCount(1);
  await expect(timeout.getByRole('radio')).toHaveText(['admin.overview.afk.60', 'admin.overview.afk.300', 'admin.overview.afk.900', 'admin.overview.afk.1800', 'admin.overview.afk.3600'].map((k) => copy(k)));
  await expect(timeout.getByRole('radio', { name: copy('admin.overview.afk.300'), exact: true })).toHaveAttribute('aria-checked', 'true');
  await timeout.getByRole('radio', { name: copy('admin.overview.afk.60'), exact: true }).click();
  const saves = dialogOf(a.page, 'admin.title').getByRole('button', { name: copy('admin.overview.save'), exact: true });
  await expect(saves).toHaveCount(1);
  await saves.click();
  await expect(dialogOf(a.page, 'admin.title').locator('.d-settings-frame__footer').getByRole('button', { name: copy('admin.overview.save'), exact: true }))
    .toHaveCount(0, { timeout: WAIT });
  await expectAccessible(a.page, 'server settings with the inactive timeout');
  await closeServerSettings(a.page);
  await expect.poll(async () => (await afkSnapshot(a)).timeout, { timeout: WAIT }).toBe(60);

  // Ada joins voice general and stays active; Björn joins it with one click and does nothing. Each device's first activity
  // report (sent when its session attaches) has landed before the clock moves (M3).
  const adaFirst = activityAccepted(a);
  await voiceRow(a.page, 'general').click();
  await connected(a.page);
  await adaFirst;
  const bjornFirst = activityAccepted(b);
  await voiceRow(b.page, 'general').click();
  await connected(b.page);
  await bjornFirst;
  await expect(dock(b.page)).toContainText('general');
  await expect(roomPeople(a.page, 'general').filter({ hasText: FRIEND })).toBeVisible({ timeout: WAIT });
  await resetSaid(a);
  await resetSaid(b);
  const posts = b.afkPosts;
  cutting = true;
  // Half the timeout, then Ada's own input (an activity report is sent at most every 5 s), then past it for Björn only.
  await advanceClock(30);
  await a.page.waitForTimeout(5_500);
  const adaActive = activityAccepted(a);
  await a.page.mouse.move(400, 300);
  await a.page.mouse.move(420, 320);
  await adaActive;
  await advanceClock(PAST_TIMEOUT_S - 30);

  // AW5: the cut leaves him with no call: no dock, and the stage of general (still open) says where he went.
  const moved = copy('afk.moved', { channel: RENAMED });
  await expect(stage(b.page)).toContainText(moved, { timeout: WAIT });
  await expect(dock(b.page).locator('.d-voice-dock')).toHaveCount(0);
  await expect(phaseOf(b.page)).not.toHaveAttribute('data-call-phase', 'in');
  // AW6: both lists show him under Idle as away, from the away roster, and no longer under general.
  for (const p of [a, b]) {
    await expect(awayPeople(p.page, RENAMED).filter({ hasText: FRIEND })).toHaveCount(1, { timeout: WAIT });
    await expect(roomPeople(p.page, 'general').filter({ hasText: FRIEND })).toHaveCount(0, { timeout: WAIT });
  }
  await expect(roomPeople(a.page, RENAMED).filter({ hasText: FRIEND })).toContainText(copy('afk.away'));
  await sweepRan(b);
  expect(b.afkPosts - posts, 'Björn’s client asks for no call or token in the AFK channel').toBe(0);

  // Neither removal line (the hold of WEB-AFK's fix changes when a removal is said, never whether the cut says one).
  const removedLines = [copy('voice.removed'), copy('voice.removedNotMember')];
  const bSaid = await said(b);
  expect(bSaid.filter((s) => s.includes(moved)), `Björn's screen reader says the move once (heard: ${JSON.stringify(bSaid)})`).toHaveLength(1);
  expect(bSaid.filter((s) => removedLines.some((r) => s.includes(r))), 'Björn is never told he was removed').toEqual([]);
  for (const r of removedLines) await expect(b.page.getByText(r)).toHaveCount(0);
  // Ada, the other person in the call he left, is still in general (her input kept her) and hears no removal and no move.
  await expect(dock(a.page)).toContainText('general');
  await expect(dock(a.page).locator('.d-voice-dock')).toHaveAttribute('data-phase', 'connected');
  const aSaid = await said(a);
  expect(aSaid.filter((s) => removedLines.some((r) => s.includes(r))), `Ada's screen reader never says removed (heard: ${JSON.stringify(aSaid)})`).toEqual([]);
  expect(aSaid.filter((s) => s.includes(moved)), 'Björn’s move is not said to Ada').toEqual([]);
  for (const r of removedLines) await expect(a.page.getByText(r)).toHaveCount(0);
  const snap = await afkSnapshot(b);
  expect(snap.pending, 'the cut is still pending for his device: he joined nothing').toBe(true);
  expect(snap.away, 'the snapshot lists one person away').toBe(1);
});

test('5. Björn goes back to general with his own mute state and leaves the roster; Ada clicks Idle, leaves her call and is listed away until she sends a message', async () => {
  const a = person(ada, OWNER);
  const b = person(bjorn, FRIEND);
  // Re-watched before the click: what follows is the return alone. His microphone is captured again, handed over enabled
  // and sent live (he was unmuted before the cut), before any reload.
  await watchAudio(b);
  await voiceRow(b.page, 'general').click();
  await expect(dock(b.page)).toContainText('general', { timeout: WAIT });
  await connected(b.page);
  await micLive(b);
  await expect(panelMute(b)).toHaveAttribute('aria-pressed', 'false');
  await expect(stage(b.page).locator('.d-call-tile[data-own] .d-call-tile__state[data-state="muted"]')).toHaveCount(0, { timeout: WAIT });
  const mute = stage(b.page).locator('[data-control="mute"]');
  await expect(mute).not.toHaveAttribute('aria-disabled', 'true');
  // R-AW4: his admission cleared his away row everywhere.
  for (const p of [a, b]) await expect(awayPeople(p.page, RENAMED)).toHaveCount(0, { timeout: WAIT });

  // R-AW3: Ada chooses AFK. Her call is left first (the dock goes), and no call starts in Idle.
  const posts = a.afkPosts;
  await voiceRow(a.page, RENAMED).click();
  await expect(dock(a.page).locator('.d-voice-dock')).toHaveCount(0, { timeout: WAIT });
  for (const p of [a, b]) await expect(awayPeople(p.page, RENAMED).filter({ hasText: OWNER })).toHaveCount(1, { timeout: WAIT });
  await expect(roomPeople(b.page, 'general').filter({ hasText: OWNER })).toHaveCount(0, { timeout: WAIT });
  await expect(stage(a.page)).not.toContainText(copy('voice.removed'));
  expect(a.afkPosts - posts, 'Ada’s client asks for no call or token in the AFK channel').toBe(0);
  // The AFK page (Jonas, concern 2): it lists the roster with her in it, and, away, the line in place of go AFK.
  await expect(stage(a.page).locator('.dw-afk-roster .d-voice-row__person').filter({ hasText: OWNER })).toHaveCount(1, { timeout: WAIT });
  await expect(stage(a.page).getByText(copy('afk.youAway'), { exact: true })).toBeVisible();
  await expect(button(stage(a.page), 'afk.go')).toHaveCount(0);
  await expect(button(stage(a.page), 'callStage.preview.join')).toHaveCount(0);
  await expectAccessible(a.page, 'the AFK channel with its away roster');
  // A second click while away does nothing (no second PUT, no call).
  const puts = a.awayPuts;
  await voiceRow(a.page, RENAMED).click();
  await a.page.waitForTimeout(1_000);
  expect(a.awayPuts, 'no second away mark').toBe(puts);

  // Q1: a message in this server clears her away; Björn's list follows.
  await openChannel(a.page, SERVER, 'general');
  await sendText(a.page, 'general', 'back at the keyboard');
  for (const p of [a, b]) await expect(awayPeople(p.page, RENAMED)).toHaveCount(0, { timeout: WAIT });
});

test('6. a phone in general is never cut, its desktop twin cut beside it is not listed away, and the phone going AFK cuts the twin calmly', async ({ playwright, browserName, headless, channel }, testInfo) => {
  const a = person(ada, OWNER);
  const b = person(bjorn, FRIEND);
  if (bjornAccount === null) throw new Error('Björn has no account');
  phone = await openPerson(`${FRIEND} phone`, { playwright, browserName, headless, channel }, testInfo.project.name, testInfo.workerIndex, 'phone');
  const ph = phone;
  await signIn(ph.page, bjornAccount, instance);
  await ph.page.goto(`/c/${communityId}/${generalVoiceId}`);
  // R5: the phone's first activity report after the join says it is exempt ([channel, 1]). The clock moves only once the
  // instance has it; before it the phone is tracked like any admission (the report follows the session's start).
  const exempt = ph.context.waitForEvent('requestfinished', { timeout: WAIT, predicate: (r) => {
    const body = r.postDataBuffer();
    return r.method() === 'POST' && r.url() === `${ORIGIN}/v1/communities/${communityId}/afk/activity`
      && body !== null && body.length === 19 && body[0] === 0x82 && body[18] === 0x01;
  } });
  // On a phone a voice row opens the stage (no one-click join, R24): join from the stage.
  await button(stage(ph.page), 'callStage.preview.join').click();
  expect((await (await exempt).response())?.status(), 'the exempt report').toBe(204);
  // A phone has no dock (flow 12): the stage's phase says it is in.
  await expect(phaseOf(ph.page)).toHaveAttribute('data-call-phase', 'in', { timeout: WAIT });
  // A voice channel lists each person once, whatever their devices (67fcebb7): the phone's phase and the desktop's dock
  // say both are in.
  await expect(roomPeople(a.page, 'general').filter({ hasText: FRIEND })).toHaveCount(1, { timeout: WAIT });
  await expect(dock(b.page)).toContainText('general');

  await resetSaid(ph);
  cutting = true;
  await advanceClock(PAST_TIMEOUT_S);
  // The control: the desktop, idle in the same call, is cut; the phone stays. R-AW3: another device of his is in a call,
  // so he is not marked away.
  await expect(dock(b.page).locator('.d-voice-dock')).toHaveCount(0, { timeout: WAIT });
  await sweepRan(ph);
  cutting = false;
  await advanceClock(PAST_TIMEOUT_S);
  await sweepRan(ph);
  await expect(phaseOf(ph.page)).toHaveAttribute('data-call-phase', 'in');
  await expect(roomPeople(a.page, 'general').filter({ hasText: FRIEND })).toHaveCount(1);
  await expect(awayPeople(a.page, RENAMED)).toHaveCount(0);
  expect((await afkSnapshot(ph)).pending, 'no cut is pending for the phone').toBe(false);
  expect((await afkSnapshot(ph)).away, 'nobody is away').toBe(0);
  expect((await said(ph)).filter((s) => s.includes(copy('afk.moved', { channel: RENAMED }))), 'the phone is never told of a move').toEqual([]);

  // M2 (Jonas): one device is active at a time. The desktop joins general again beside the phone; the phone then goes AFK
  // from Idle's page: its own call is left first, and the instance cuts the desktop's, which says so calmly.
  await voiceRow(b.page, 'general').click();
  await connected(b.page);
  await resetSaid(b);
  cutting = true;
  await ph.page.evaluate((path) => { window.history.pushState(null, '', path); window.dispatchEvent(new PopStateEvent('popstate')); },
    `/c/${communityId}/${afkId}`);
  await button(stage(ph.page), 'afk.go').click();
  await expect(stage(b.page)).toContainText(copy('afk.elsewhere'), { timeout: WAIT });
  await expect(dock(b.page).locator('.d-voice-dock')).toHaveCount(0);
  await expect(stage(b.page).locator('.d-call-stage__notice')).toHaveAttribute('data-kind', 'info');
  for (const r of [copy('voice.removed'), copy('voice.removedNotMember')]) await expect(b.page.getByText(r)).toHaveCount(0);
  await expect(stage(ph.page).getByText(copy('afk.youAway'), { exact: true })).toBeVisible({ timeout: WAIT });
  await expect(awayPeople(a.page, RENAMED).filter({ hasText: FRIEND })).toHaveCount(1, { timeout: WAIT });
  await expect(roomPeople(a.page, 'general').filter({ hasText: FRIEND })).toHaveCount(0, { timeout: WAIT });
  await sweepRan(b);
  cutting = false;
  await closePerson(ph);
  phone = null;
});

test('7. with view_channel left out of his roles community-wide, Björn still sees Idle, and a click lists him away there with no call', async () => {
  const a = person(ada, OWNER);
  const b = person(bjorn, FRIEND);
  await expect(dock(b.page).locator('.d-voice-dock')).toHaveCount(0, { timeout: WAIT });
  // The role editor: @everyone loses "view channels", so none of Björn's roles grants it anywhere (R35).
  await setEveryone(a, 'perm.view', false);
  await expect(channelRow(b.page, 'general')).toHaveCount(0, { timeout: WAIT });
  await expect(voiceRow(b.page, 'general')).toHaveCount(0);
  // Idle stands alone: its category is not his to see, so it is listed at the top level (R44).
  await expect.poll(() => listOrder(b.page), { timeout: WAIT }).toEqual([copy('voice.open', { name: RENAMED })]);
  expect((await afkSnapshot(b)).channel, 'his snapshot still names AFK').toBe(afkId);
  expect((await channelsOf(b)).map((c) => c.id), 'the instance lists him Idle alone').toEqual([afkId]);

  const posts = b.afkPosts;
  await voiceRow(b.page, RENAMED).click();
  for (const p of [a, b]) await expect(awayPeople(p.page, RENAMED).filter({ hasText: FRIEND })).toHaveCount(1, { timeout: WAIT });
  await expect(dock(b.page).locator('.d-voice-dock')).toHaveCount(0);
  expect(b.afkPosts - posts, 'no call or token is asked for in Idle').toBe(0);

  // Restored for what follows: the control, general is his again.
  await setEveryone(a, 'perm.view', true);
  await expect(voiceRow(b.page, 'general')).toBeVisible({ timeout: WAIT });
});

test('8. Q2 (revised, AW2 final): his last connection closing keeps Björn away through the 30 s grace, then takes him off the roster; back, nothing joins', async () => {
  const a = person(ada, OWNER);
  const b = person(bjorn, FRIEND);
  await expect(awayPeople(a.page, RENAMED).filter({ hasText: FRIEND })).toHaveCount(1, { timeout: WAIT });
  const posts = b.afkPosts;
  await b.page.goto('about:blank');
  // A reload or a short drop keeps him away: the grace (afkOfflineGrace, 30 s) runs on the host's clock, which stands still
  // here, so he stays listed while the close lands.
  await a.page.waitForTimeout(3_000);
  await expect(awayPeople(a.page, RENAMED).filter({ hasText: FRIEND }), 'still away inside the 30 s grace').toHaveCount(1);
  await advanceClock(20);
  await a.page.waitForTimeout(1_000);
  await expect(awayPeople(a.page, RENAMED).filter({ hasText: FRIEND }), 'still away 20 s into the grace').toHaveCount(1);
  // Once the grace ends with no connection, the roster drops him.
  await advanceClock(11);
  await expect(awayPeople(a.page, RENAMED)).toHaveCount(0, { timeout: WAIT });
  await b.page.goto(`/c/${communityId}`);
  await expect(railItem(b.page, SERVER)).toBeVisible({ timeout: WAIT });
  await expect(voiceRow(b.page, RENAMED)).toBeVisible({ timeout: WAIT });
  await sweepRan(b);
  await expect(dock(b.page).locator('.d-voice-dock')).toHaveCount(0);
  await expect(awayPeople(b.page, RENAMED)).toHaveCount(0);
  expect(b.afkPosts - posts, 'the reload asks for no call or token in Idle').toBe(0);
});

test('9. a web3.8 pick: after the upgrade the server has a new AFK, and lounge and Idle are ordinary', async () => {
  const a = person(ada, OWNER);
  const b = person(bjorn, FRIEND);
  for (const p of [a, b]) await expect(dock(p.page).locator('.d-voice-dock')).toHaveCount(0, { timeout: WAIT });
  await noiseOff(b.page);
  await makeVoice(a.page, 'lounge');
  const lounge = (await channelsOf(a)).find((c) => c.name === 'lounge' && c.kind === 1)?.id ?? '';
  expect(lounge).toMatch(/^[0-9a-f]{32}$/);
  // A web3.8 administrator's pick, as web3.8 stored it.
  const res = await fetch(`${started().control}/debug/afk/legacy`, {
    method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ community: communityId, channel: lounge }),
  });
  expect(res.status, await res.text()).toBe(204);

  await restart([a, b]);
  const after = await channelsOf(a);
  const afks = after.filter((c) => c.name === 'AFK');
  expect(afks, 'one new AFK channel').toHaveLength(1);
  const fresh = afks[0]?.id ?? '';
  expect(fresh).not.toBe(afkId);
  expect(fresh).not.toBe(lounge);
  expect((await afkSnapshot(a)).channel, 'the pointer moved to the new channel').toBe(fresh);
  expect((await afkSnapshot(a)).timeout, 'the stored timeout is kept').toBe(60);
  for (const p of [a, b]) {
    await railItem(p.page, SERVER).click();
    await expectAfkLast(p.page, 'AFK');
    await expect(voiceRow(p.page, 'lounge')).toBeVisible();
    await expect(voiceRow(p.page, RENAMED)).toBeVisible();
  }

  // lounge is ordinary: Björn joins it and his microphone sends (the control for steps 4 and 5).
  await watchAudio(b);
  await voiceRow(b.page, 'lounge').click();
  await connected(b.page);
  await sendsAudio(b);
  // Signal §5.6: on the joined channel's stage the dock draws nothing; leave from the call bar.
  await stage(b.page).locator('[data-control="leave"]').click();
  await expect(dock(b.page).locator('.d-voice-dock')).toHaveCount(0, { timeout: WAIT });

  // The old Idle is a plain voice channel too: it has its delete and its permissions; only the new AFK has neither.
  const settings = await serverSettings(a.page, 'admin.nav.channels');
  await expect(button(settings, 'admin.channels.deleteLabel', { name: RENAMED })).toBeVisible({ timeout: WAIT });
  await expect(button(settings, 'admin.channels.deleteLabel', { name: 'lounge' })).toBeVisible();
  await expect(button(settings, 'admin.channels.deleteLabel', { name: 'AFK' })).toHaveCount(0);
  await button(settings, 'admin.channels.editLabel', { name: RENAMED }).click();
  const edit = dialogOf(a.page, 'admin.channel.titleVoice', { name: RENAMED });
  await expect(edit.getByRole('heading', { name: copy('chperm.title'), exact: true })).toBeVisible({ timeout: WAIT });
  await expect(edit.getByText(copy('afk.channel.note'), { exact: true })).toHaveCount(0);
  await button(edit, 'admin.cancel').click();
  await expect(edit).toBeHidden();
  await button(settings, 'admin.channels.editLabel', { name: 'lounge' }).click();
  const picked = dialogOf(a.page, 'admin.channel.titleVoice', { name: 'lounge' });
  await expect(picked.getByRole('heading', { name: copy('chperm.title'), exact: true })).toBeVisible({ timeout: WAIT });
  await expect(picked.getByText(copy('afk.channel.note'), { exact: true })).toHaveCount(0);
  await button(picked, 'admin.cancel').click();
  await expect(picked).toBeHidden();
  await closeServerSettings(a.page);

  // A second start adds nothing.
  const listed = (await channelsOf(a)).map((c) => c.id).sort();
  await restart([a, b]);
  expect((await channelsOf(a)).map((c) => c.id).sort(), 'the same channels after a second start').toEqual(listed);
  expect((await afkSnapshot(a)).channel).toBe(fresh);
  afkId = fresh;
});

test('10. accessible: the channel list, the AFK row with its away roster and server settings', async () => {
  const a = person(ada, OWNER);
  const b = person(bjorn, FRIEND);
  await railItem(b.page, SERVER).click();
  await expectAfkLast(b.page, 'AFK');
  await expectAccessible(b.page, 'the channel list with the AFK channel');
  // The AFK page opened by its path (no click on the row): nobody is away yet, and go AFK stands in place of join voice.
  await a.page.goto(`/c/${communityId}/${afkId}`);
  await expect(stage(a.page).getByText(copy('afk.empty'), { exact: true })).toBeVisible({ timeout: WAIT });
  await expect(button(stage(a.page), 'callStage.preview.join')).toHaveCount(0);
  await expectAccessible(a.page, 'the AFK page with nobody away');
  const posts = a.afkPosts;
  await button(stage(a.page), 'afk.go').click();
  for (const p of [a, b]) await expect(awayPeople(p.page, 'AFK').filter({ hasText: OWNER })).toHaveCount(1, { timeout: WAIT });
  await expect(stage(a.page).getByText(copy('afk.youAway'), { exact: true })).toBeFocused({ timeout: WAIT });
  await expect(button(stage(a.page), 'afk.go')).toHaveCount(0);
  await expect(dock(a.page).locator('.d-voice-dock')).toHaveCount(0);
  expect(a.afkPosts - posts, 'go AFK asks for no call or token').toBe(0);
  await expectAccessible(a.page, 'the AFK channel with Ada away');
  await expectAccessible(b.page, 'the channel list with Ada away under AFK');
  await serverSettings(a.page, 'admin.nav.overview');
  await expect(a.page.getByRole('radiogroup', { name: copy('admin.overview.afk'), exact: true }).getByRole('radio').first()).not.toHaveAttribute('aria-disabled', 'true', { timeout: WAIT });
  await expectAccessible(a.page, 'server settings, overview');
  await closeServerSettings(a.page);
});

/** Stops the host and starts it again on the same data, then reloads the people's pages into the server. */
async function restart(people: Person[]): Promise<void> {
  restarting = true;
  try {
    await started().stop();
    host = await startTestHost(HOST);
    for (const p of people) {
      await p.page.reload();
      await expect(railItem(p.page, SERVER)).toBeVisible({ timeout: WAIT });
    }
    // The page's worker sends its bearer again once it is up.
    await expect.poll(async () => (await fetch(`${ORIGIN}/v1/communities/${communityId}/afk`, {
      headers: { accept: 'application/cbor', authorization: people[0]?.bearer ?? '' } })).status, { timeout: WAIT }).toBe(200);
  } finally {
    restarting = false;
  }
}

/** Server settings → roles → edit @everyone → the right `perm` (a `perm.*` key) on or off → save. */
async function setEveryone(a: Person, perm: string, on: boolean): Promise<void> {
  const settings = await serverSettings(a.page, 'roles.tab');
  // Signal task 10c: the inline role editor; a switch saves at once.
  await selectRole(settings, copy('roles.everyone'));
  await setRolePermission(settings, perm.replace(/^perm\./, ''), on);
  await expect(settings.locator('.d-role-editor__status')).toHaveText(copy('roles.editor.saved', { name: copy('roles.everyone') }), { timeout: WAIT });
  await closeServerSettings(a.page);
}
