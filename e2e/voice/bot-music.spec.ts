// Bot v2 Task 19 (G3, music without lag), Electron voice lane, Linux only (the supervised Bot). The Bot plays the
// fixture stream (writeMusicFixtures: a 30 s 440 Hz and a 5 s 660 Hz Ogg Opus file, through the same single ffmpeg as
// a real track) into a call of two accounts. Every timing runs from the sender's Enter (a keydown listener in the page)
// to the receiver's first decoded audio frame (its call audio graph, sampled every few milliseconds), both on the one
// machine's clock (performance.timeOrigin + now()).
//
// The spec starts its own test host: the voice lane's shared host has a frozen clock and no Bot, and the supervised Bot
// needs the wall-clock host (advanceClockWithWall). The voice config writes the fixtures and names their directory in
// DILLA_BOT_MUSIC_FIXTURES; the Bot's environment is fixed by bot-launch, so the spec copies them into the Bot's state
// directory (<state>/music-fixtures), where a dilla_testhost Bot looks too.
import { copyFileSync, mkdirSync, readdirSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import type { Locator, Page } from '@playwright/test';
import { expect, test, type Person } from './support/peer-browser';
import { AUDIBLE_RMS, audibleLevel, joinVoice, WAIT } from './support/voice';
import {
  addServerVia, channelRow, completeSignUp, composerBox, copy, makeInviteLink, makeRole, newAccount, newChannelButton, openChannel, signIn, signUp, type Account,
} from '../web/support/app';
import { prepareBotRuntimeBinaries, startTestHost, type HostConfig, type StartedHost } from '../web/support/host';
import { e2ePort } from '../web/support/ports';
import { openInviteInClient } from '../web/support/invite';
import { arr, decode } from '../../packages/client-core/src/cbor/index';
import { MUSIC_FIXTURES } from '../media/support/fixtures';

const PORT = Number(process.env.DILLA_E2E_BOT_MUSIC_PORT ?? e2ePort(8571));
const HOST: HostConfig = {
  port: PORT, control: PORT + 1, webRoot: 'packages/web/dist', botRuntime: true, advanceClockWithWall: true,
  sfu: { port: PORT + 3, udpPort: PORT + 5, departureTimeout: 90 },
};
const ORIGIN = `http://127.0.0.1:${String(PORT)}`;
const SERVER = 'Music without lag';
const VOICE = 'general';
const DJ_ROLE = 'Music DJ';
const COLD_RUNS = 10;
const REPEAT_RUNS = 10;
const GAP_RUNS = 3;

test.use({ baseURL: ORIGIN });
test.describe.configure({ mode: 'serial' });

let host: StartedHost | undefined;
let instance = '';
let ada: Account | undefined;
let bea: Account | undefined;
const measured: Record<string, number[]> = {};

const dialog = (page: Page, name: string, vars?: Record<string, string | number>) =>
  page.getByRole('dialog', { name: copy(name, vars), exact: true });
const settingsNav = (settings: Locator, key: string) =>
  settings.getByRole('navigation', { name: copy('admin.nav'), exact: true }).getByRole('button', { name: copy(key), exact: true });

function percentile(values: number[], p: number): number {
  const sorted = [...values].sort((a, b) => a - b);
  return sorted[Math.min(sorted.length - 1, Math.max(0, Math.ceil((p / 100) * sorted.length) - 1))] ?? Number.NaN;
}

function record(name: string, values: number[]): void {
  measured[name] = values;
  const summary = { runs: values.length, p50: percentile(values, 50), p95: percentile(values, 95), max: Math.max(...values), values };
  console.info(`G3 ${name}: ${JSON.stringify(summary)}`);
  const out = test.info().outputPath(`g3-${name}.json`);
  mkdirSync(dirname(out), { recursive: true });
  writeFileSync(out, JSON.stringify(summary, null, 2));
}

test.beforeAll(async () => {
  // A cold build of the Bot and its helper has a budget of its own, outside the measured tests.
  test.setTimeout(900_000);
  await prepareBotRuntimeBinaries();
  host = await startTestHost(HOST);
  const fixtures = process.env.DILLA_BOT_MUSIC_FIXTURES;
  if (fixtures === undefined || host.runtimeCredentialFile === undefined) throw new Error('the voice config writes the music fixtures (DILLA_BOT_MUSIC_FIXTURES)');
  // Before the Bot first builds its music backend: that happens when the owner enables music below.
  const target = join(dirname(host.runtimeCredentialFile), 'music-fixtures');
  mkdirSync(target, { recursive: true, mode: 0o700 });
  for (const name of readdirSync(fixtures)) if (name.endsWith('.opus')) copyFileSync(join(fixtures, name), join(target, name));
  const discovery = await fetch(`${ORIGIN}/v1/instance`, { headers: { accept: 'application/cbor' } });
  const name = arr(decode(new Uint8Array(await discovery.arrayBuffer())))[5];
  if (typeof name !== 'string' || name === '') throw new Error('the music host has no instance name');
  instance = name;
});

test.afterAll(async () => { await host?.stop(); });

/** The owner's first session: the server, music on the Bot, a DJ role for Bea and her invite. */
async function createCommunity(adaPage: Page, beaPage: Page): Promise<void> {
  if (host === undefined) throw new Error('no host');
  ada = await signUp(adaPage, host.invite.split(',')[0] ?? '', instance, 'Ada', host.control);
  await addServerVia(adaPage, 'create');
  const create = dialog(adaPage, 'create.server.title');
  await create.getByRole('textbox', { name: copy('create.server.name'), exact: true }).fill(SERVER);
  await adaPage.keyboard.press('Enter');
  await expect(create).toBeHidden({ timeout: WAIT });
  await expect(adaPage.getByRole('button', { name: copy('voice.open', { name: VOICE }), exact: true })).toBeVisible({ timeout: WAIT });
  // One voice room per cold run, made before the Bot is enabled, so each cold /play joins a call the Bot was never in.
  const rooms = [...Array.from({ length: COLD_RUNS - 1 }, (_, i) => coldRoom(i + 1)), ...Object.values(ROOMS)];
  for (const room of rooms) {
    await newChannelButton(adaPage).click();
    const made = dialog(adaPage, 'create.channel.title', { server: SERVER });
    await made.getByRole('radio', { name: copy('create.channel.voice'), exact: true }).click();
    await made.getByRole('textbox', { name: copy('create.channel.name'), exact: true }).fill(room);
    await adaPage.keyboard.press('Enter');
    await expect(made).toBeHidden({ timeout: WAIT });
  }
  await channelRow(adaPage, 'general').click();
  await adaPage.getByRole('button', { name: copy('admin.open'), exact: true }).click()
    .then(() => adaPage.getByRole('menuitem', { name: copy('signalSettings.menu.serverSettings'), exact: true }).click());
  const settings = dialog(adaPage, 'admin.title');
  await settingsNav(settings, 'roles.tab').click();
  await makeRole(adaPage, settings, DJ_ROLE, { perms: ['music'] });
  await settingsNav(settings, 'admin.nav.bot').click();
  await settings.getByRole('switch', { name: copy('serverSettings.bot.cap.music'), exact: true }).click();
  await settings.getByRole('button', { name: copy('serverSettings.bot.enable'), exact: true }).click();
  const stepUp = dialog(adaPage, 'factorSettings.stepUp.title');
  await stepUp.getByLabel(copy('factorSettings.stepUp.password'), { exact: true }).fill(ada.password);
  await stepUp.getByRole('button', { name: copy('factorSettings.stepUp.confirm'), exact: true }).click();
  await expect(settings.getByRole('status').filter({ hasText: copy('admin.bot.status.ready') }))
    .toHaveText(copy('admin.bot.status.ready'), { timeout: 90_000 });
  await adaPage.keyboard.press('Escape');

  const link = await makeInviteLink(adaPage, SERVER);
  await openInviteInClient(beaPage, link);
  bea = await completeSignUp(beaPage, newAccount('Bea', host.control), instance);
  await expect(beaPage.getByRole('navigation', { name: copy('shell.rail.label'), exact: true })
    .getByRole('button', { name: SERVER, exact: true })).toBeVisible({ timeout: WAIT });

  const member = adaPage.getByRole('complementary', { name: copy('members.region', { server: SERVER }), exact: true })
    .getByRole('listitem').filter({ hasText: 'Bea' });
  await expect(member).toBeVisible({ timeout: WAIT });
  await member.locator('.d-member-list__open').click();
  await adaPage.getByRole('dialog', { name: 'Bea', exact: true }).getByRole('button', { name: copy('layout.profile.manageRoles'), exact: true }).click();
  const assign = dialog(adaPage, 'roles.assign.title', { name: 'Bea' });
  await assign.getByRole('checkbox', { name: DJ_ROLE, exact: true }).click();
  await expect(assign.getByRole('checkbox', { name: DJ_ROLE, exact: true })).toBeChecked({ timeout: WAIT });
  await assign.getByRole('button', { name: copy('roles.assign.close'), exact: true }).click();
}

interface Seat { person: Person }
let seatings = 0;

/** Ada and Bea in the voice room, muted (only the Bot is heard); the first test creates them, later tests sign in on
 *  new devices. */
async function seatBoth(peerBrowser: { open(name: string): Promise<Person> }, room: string = VOICE): Promise<{ ada: Seat; bea: Seat }> {
  seatings += 1;
  const adaPerson = await peerBrowser.open(`ada-${String(seatings)}`);
  const beaPerson = await peerBrowser.open(`bea-${String(seatings)}`);
  const seats = { ada: { person: adaPerson }, bea: { person: beaPerson } };
  if (ada === undefined || bea === undefined) await createCommunity(adaPerson.page, beaPerson.page);
  else {
    await signIn(adaPerson.page, ada, instance);
    await signIn(beaPerson.page, bea, instance);
  }
  for (const seat of [seats.ada, seats.bea]) await seatInVoice(seat.person.page, room);
  return seats;
}

async function seatInVoice(page: Page, room = VOICE, mute = true): Promise<void> {
  await joinVoice(page, SERVER, room);
  if (mute) await page.keyboard.press('Control+Shift+M');
}

/** Samples the page's call audio graph every few milliseconds and keeps the edges (quiet to loud and back), over the
 *  last 10 ms of each analyser so an edge is placed within a frame or two. */
async function listen(page: Page): Promise<void> {
  await page.evaluate((threshold) => {
    const w = window as unknown as { __g3?: { edges: { t: number; loud: boolean }[]; loud: boolean }; __dw: { remoteAnalysers: AnalyserNode[] } };
    if (w.__g3 !== undefined) { w.__g3.edges = []; w.__g3.loud = false; return; }
    const state = { edges: [] as { t: number; loud: boolean }[], loud: false };
    w.__g3 = state;
    setInterval(() => {
      let max = 0;
      for (const a of w.__dw.remoteAnalysers) {
        const buf = new Float32Array(a.fftSize);
        a.getFloatTimeDomainData(buf);
        const tail = buf.subarray(Math.max(0, buf.length - 480));
        let sum = 0;
        for (const x of tail) sum += x * x;
        max = Math.max(max, Math.sqrt(sum / tail.length));
      }
      const loud = max > threshold;
      if (loud !== state.loud) { state.loud = loud; state.edges.push({ t: performance.timeOrigin + performance.now(), loud }); }
    }, 4);
  }, AUDIBLE_RMS);
}

const edges = (page: Page) => page.evaluate(() => (window as unknown as { __g3: { edges: { t: number; loud: boolean }[] } }).__g3.edges);

/** The first time after `after` the page's call audio became loud. */
async function firstAudio(page: Page, after: number): Promise<number> {
  let at = 0;
  await expect.poll(async () => {
    at = (await edges(page)).find(e => e.loud && e.t >= after)?.t ?? 0;
    return at;
  }, { timeout: WAIT, intervals: [50, 100, 250], message: 'the Bot was heard after Enter' }).toBeGreaterThan(0);
  return at;
}

/** Waits until the page hears nothing. */
async function quiet(page: Page): Promise<void> {
  await expect.poll(() => audibleLevel(page), { timeout: WAIT, intervals: [100, 250, 500] }).toBeLessThan(AUDIBLE_RMS);
}

/** Runs one Bot command from the text channel's composer; answers the Enter time and the result row. */
async function command(page: Page, id: string, argument?: string): Promise<{ enter: number; result: Locator; previous: string }> {
  await openChannel(page, SERVER, 'general');
  // The private result row is reused by each run: this run's answer is the first text that differs from the last one.
  const row = page.locator('.d-bot-result .d-message-row__body').last();
  const previous = await row.count() > 0 ? await row.innerText() : '';
  await page.evaluate(() => {
    const w = window as unknown as { __g3Enter?: number; __g3EnterHooked?: boolean };
    w.__g3Enter = 0;
    if (w.__g3EnterHooked === true) return;
    w.__g3EnterHooked = true;
    const mark = (): void => { w.__g3Enter = performance.timeOrigin + performance.now(); };
    window.addEventListener('keydown', (e) => { if (e.key === 'Enter') mark(); }, true);
    window.addEventListener('click', mark, true);
  });
  const composer = composerBox(page, 'general');
  const region = page.getByRole('region', { name: 'Command composer', exact: true });
  if (argument === undefined) {
    // A command with no arguments, typed whole and sent (as slash-commands.spec.ts runs them).
    await composer.fill(`/${id}`);
    await expect(region).toBeVisible({ timeout: WAIT });
    await region.getByRole('button', { name: copy('shell.composer.send'), exact: true }).click();
  } else {
    await composer.fill('/');
    const picker = region.getByRole('textbox', { name: 'Command', exact: true });
    await picker.fill(`/${id}`);
    await expect(region.getByRole('option', { name: new RegExp(`^/${id},`, 'u') })).toBeVisible({ timeout: WAIT });
    await picker.press('Enter');
    const field = region.getByRole('combobox', { name: 'query-or-url', exact: true });
    await field.fill(argument);
    await expect(field).toHaveValue(argument);
    await page.evaluate(() => { (window as unknown as { __g3Enter: number }).__g3Enter = 0; });
    await field.press('Enter');
  }
  const enter = await page.evaluate(() => (window as unknown as { __g3Enter: number }).__g3Enter);
  expect(enter, 'the Enter was seen').toBeGreaterThan(0);
  return { enter, result: row, previous };
}

async function settle(page: Page): Promise<void> {
  const region = page.getByRole('region', { name: 'Command composer', exact: true });
  if (await region.count() > 0) { await region.locator('input, textarea').first().focus(); await page.keyboard.press('Escape'); }
}

/** Waits for the run's answer: a text other than the previous run's that matches `expected`. */
async function answer(result: Locator, previous: string, expected: RegExp): Promise<string> {
  const deadline = Date.now() + WAIT;
  for (;;) {
    const text = await result.innerText().catch(() => '');
    if (text !== previous && expected.test(text)) return text;
    if (Date.now() > deadline) throw new Error(`no answer matching ${String(expected)}: the result row shows ${JSON.stringify(text)} (before: ${JSON.stringify(previous)})`);
    await new Promise((r) => setTimeout(r, 100));
  }
}

/** Runs a command and waits for its final answer. */
async function run(page: Page, id: string, expected: RegExp, argument?: string): Promise<string> {
  const { result, previous } = await command(page, id, argument);
  const text = await answer(result, previous, expected);
  await settle(page);
  return text;
}

/** /play at Ada; answers Enter → first audio at Bea, in ms, after "Now playing" was answered. */
async function timedPlay(sender: Page, receiver: Page, track: string): Promise<number> {
  await listen(receiver);
  const { enter, result, previous } = await command(sender, 'play', track);
  const heard = await firstAudio(receiver, enter).catch(async (error: unknown) => {
    throw new Error(`${String(error)}\nthe /play's result row shows ${JSON.stringify(await result.innerText().catch(() => ''))}`);
  });
  await answer(result, previous, /Now playing: /u);
  await settle(sender);
  console.info(`G3 run /play ${track}: ${String(Math.round(heard - enter))} ms`);
  return heard - enter;
}

/** The voice room of cold run `i`: a channel whose call the Bot has never been in (the first run uses general). */
const coldRoom = (i: number): string => (i === 0 ? VOICE : `cold-${String(i)}`);
/** The rooms of the later tests, one each: their people are seated before the Bot first joins. */
const ROOMS = { gap: 'gap-room', repeat: 'repeat-room', device: 'device-room', restart: 'restart-room', late: 'late-room' } as const;

test('play from Enter to first audio frame meets G3', async ({ peerBrowser }) => {
  test.setTimeout(1_200_000);
  const seats = await seatBoth(peerBrowser);
  const sender = seats.ada.person.page;
  const receiver = seats.bea.person.page;
  await quiet(receiver);
  const runs: number[] = [];
  for (let i = 0; i < COLD_RUNS; i++) {
    if (i > 0) {
      // Stop the last room's music, then both move (the app's own leave and join; mute stays on) to a room whose call
      // the Bot has never been in.
      await run(sender, 'stop', /Music stopped\./u);
      for (const page of [sender, receiver]) await seatInVoice(page, coldRoom(i), false);
      await quiet(receiver);
    }
    runs.push(await timedPlay(sender, receiver, MUSIC_FIXTURES.long));
  }
  record('cold-play', runs);
  expect(percentile(runs, 50), 'cold /play p50 (ms)').toBeLessThanOrEqual(1000);
  expect(percentile(runs, 95), 'cold /play p95 (ms)').toBeLessThanOrEqual(2000);
});

test('next queued track gap is under 150 ms', async ({ peerBrowser }) => {
  test.setTimeout(600_000);
  const seats = await seatBoth(peerBrowser, ROOMS.gap);
  const sender = seats.ada.person.page;
  const receiver = seats.bea.person.page;
  const gaps: number[] = [];
  for (let i = 0; i < GAP_RUNS; i++) {
    await run(sender, 'stop', /Music stopped\./u);
    await quiet(receiver);
    await timedPlay(sender, receiver, MUSIC_FIXTURES.short);
    const started = (await edges(receiver)).filter(e => e.loud).at(-1)?.t ?? 0;
    expect(await run(sender, 'play', /Track added to the music queue\./u, MUSIC_FIXTURES.long)).toContain('Track added');
    // The 5 s track ends, then the 30 s track starts. Once the 5 s track must have ended (with a second to spare), the
    // gap is the receiver's longest quiet span since the first track started; a page that never went quiet heard no gap
    // (shorter than its 10 ms window), and one that is quiet now never heard the next track start.
    const pageNow = () => receiver.evaluate(() => performance.timeOrigin + performance.now());
    await expect.poll(async () => (await pageNow()) - started, { timeout: WAIT, intervals: [500] }).toBeGreaterThan(6_000);
    const after = (await edges(receiver)).filter(e => e.t > started);
    let gap = 0;
    for (const [i, edge] of after.entries()) {
      const next = after[i + 1];
      if (!edge.loud && next !== undefined) gap = Math.max(gap, next.t - edge.t);
    }
    const last = after.at(-1);
    if (last !== undefined && !last.loud) throw new Error(`the next track never started: edges ${JSON.stringify(after.map(e => [Math.round(e.t - started), e.loud]))}`);
    console.info(`G3 gap run ${String(i)}: ${String(Math.round(gap))} ms, edges ${JSON.stringify(after.map(e => [Math.round(e.t - started), e.loud]))}`);
    gaps.push(Math.max(0, gap));
  }
  record('next-track-gap', gaps);
  for (const gap of gaps) expect(gap, 'gap between queued tracks (ms)').toBeLessThan(150);
});

test('a repeated track starts within 500 ms at p95', async ({ peerBrowser }) => {
  test.setTimeout(900_000);
  const seats = await seatBoth(peerBrowser, ROOMS.repeat);
  const sender = seats.ada.person.page;
  const receiver = seats.bea.person.page;
  const runs: number[] = [];
  // The first /play of this session joins the room and fills the lookup cache; every later one repeats the track.
  await run(sender, 'stop', /Music stopped\./u);
  await quiet(receiver);
  await timedPlay(sender, receiver, MUSIC_FIXTURES.long);
  for (let i = 0; i < REPEAT_RUNS; i++) {
    await run(sender, 'stop', /Music stopped\./u);
    await quiet(receiver);
    runs.push(await timedPlay(sender, receiver, MUSIC_FIXTURES.long));
  }
  record('repeat-play', runs);
  expect(percentile(runs, 95), 'repeated track p95 (ms)').toBeLessThanOrEqual(500);
});

// Runs before the tests that sign in more devices: each TOTP sign-in moves the host's clock to a fresh code window, and
// the Bot's restart checks that clock against the wall.
test('a Bot restart mid-queue resumes', async ({ peerBrowser }) => {
  test.setTimeout(600_000);
  if (host === undefined) throw new Error('no host');
  const seats = await seatBoth(peerBrowser, ROOMS.restart);
  const sender = seats.ada.person.page;
  const receiver = seats.bea.person.page;
  await run(sender, 'stop', /Music stopped\./u);
  await quiet(receiver);
  await timedPlay(sender, receiver, MUSIC_FIXTURES.long);
  await run(sender, 'play', /Track added to the music queue\./u, MUSIC_FIXTURES.short);
  const before = await run(sender, 'queue', /Now playing: /u);
  expect(before).toContain(MUSIC_FIXTURES.long);
  expect(before).toContain(`1. ${MUSIC_FIXTURES.short}`);
  // The host's control endpoint stops and starts the supervised Bot (/debug/bot/stop, /debug/bot/start).
  await host.restartBotRuntime();
  await quiet(receiver);
  // The queue is intact: the playing track (restored paused, as every restart restores it) and the one queued after it.
  const after = await run(sender, 'queue', /fixture/u);
  expect(after).toContain(MUSIC_FIXTURES.long);
  expect(after).toContain(`1. ${MUSIC_FIXTURES.short}`);
  await listen(receiver);
  const { enter, result, previous } = await command(sender, 'resume');
  const heard = await firstAudio(receiver, enter).catch(async (error: unknown) => {
    // Name the Bot's side of a silent resume: its answer (resumed, or a start failure) and any refusal.
    throw new Error(`${String(error)}\nthe /resume's result row shows ${JSON.stringify(await result.innerText().catch(() => ''))}`
      + ` (before: ${JSON.stringify(previous)})\nBot refusals: ${JSON.stringify(host?.botRefusals() ?? [])}`);
  });
  await answer(result, previous, /Music resumed\./u);
  await settle(sender);
  console.info(`G3 resume after a Bot restart: ${String(heard - enter)} ms`);
  const resumed = await run(sender, 'queue', /Now playing: /u);
  expect(resumed).toContain(MUSIC_FIXTURES.long);
  expect(resumed).toContain(`1. ${MUSIC_FIXTURES.short}`);
  await expect.poll(() => audibleLevel(receiver), { timeout: WAIT }).toBeGreaterThan(AUDIBLE_RMS);
});

// Fix round 1 (C1, I4): a device that joins a call where the Bot already plays hears it. The join's commit moves the
// call to a new epoch; the Bot applies it and re-keys its sender, so the newcomer (who never held the old epoch's key)
// can decrypt the Bot's frames.
test('a device that joins while the Bot plays hears it', async ({ peerBrowser }) => {
  test.setTimeout(600_000);
  if (ada === undefined || bea === undefined) {
    // This test can run alone (-g): the community is made by the first seating.
    const seats = await seatBoth(peerBrowser);
    await peerBrowser.close(seats.ada.person);
    await peerBrowser.close(seats.bea.person);
  }
  // Bea's new device signs in first, outside the call, so it joins while the Bot is still playing.
  const late = await peerBrowser.open(`bea-late-${String(++seatings)}`);
  await signIn(late.page, bea!, instance);
  const first = await peerBrowser.open(`ada-late-${String(++seatings)}`);
  await signIn(first.page, ada!, instance);
  await seatInVoice(first.page, ROOMS.late);
  await quiet(first.page);
  await timedPlay(first.page, first.page, MUSIC_FIXTURES.long);
  // A second track queued behind it keeps the Bot playing through the join.
  await run(first.page, 'play', /Track added to the music queue\./u, MUSIC_FIXTURES.long);
  // Bea's device joins the room the Bot is playing in.
  await listen(late.page);
  const joinedAt = await late.page.evaluate(() => performance.timeOrigin + performance.now());
  await seatInVoice(late.page, ROOMS.late);
  const heard = await firstAudio(late.page, joinedAt);
  console.info(`G3 late joiner heard the Bot ${String(Math.round(heard - joinedAt))} ms after starting to join`);
  await expect.poll(() => audibleLevel(late.page), { timeout: WAIT }).toBeGreaterThan(AUDIBLE_RMS);
  // The first listener still hears it too (the Bot re-keyed in place, without stopping).
  await expect.poll(() => audibleLevel(first.page), { timeout: WAIT }).toBeGreaterThan(AUDIBLE_RMS);
});

test('a new device uses /play within its first session', async ({ peerBrowser }) => {
  test.setTimeout(600_000);
  const seats = await seatBoth(peerBrowser, ROOMS.device);
  const listener = seats.ada.person.page;
  await quiet(listener);
  // A fresh device of Bea, enrolled here: it signs in, joins the room and plays, with no registration step between.
  if (bea === undefined) throw new Error('no Bea');
  const device = await peerBrowser.open('bea-new-device');
  await signIn(device.page, bea, instance);
  await seatInVoice(device.page, ROOMS.device);
  const elapsed = await timedPlay(device.page, listener, MUSIC_FIXTURES.long);
  console.info(`G3 new-device first /play: ${String(elapsed)} ms`);
  expect(elapsed, 'a new device is heard within the cold budget').toBeLessThanOrEqual(2000);
});
