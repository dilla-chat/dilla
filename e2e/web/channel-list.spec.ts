// web3.8, WS5 (item 12): one channel list. From an empty host with its in-process LiveKit, Ada makes the text channels
// alpha and beta and the voice channel lounge, in that order alpha, lounge, beta. The sidebar lists lounge between them
// with the speaker icon; when she joins, she is listed under its row, indented, with the speaking dot while the fake
// microphone sounds and her glyphs on her name's line. There is no "active voice" heading and no "voice" heading.
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { expect, type Page } from '@playwright/test';
import { addServerVia,
  TEST_TIMEOUT, WAIT, channelRow, channelsNav, closeSettings, copy, expectAccessible, expectComposerReady, newChannelButton, openSettings, railNav,
  signUp, test, type SettingsSection,
} from './support/app';
import { startTestHost, type HostConfig, type StartedHost } from './support/host';
import { chromiumMediaArgs, writeFixtures } from '../media/support/fixtures';
import { arr, decode } from '../../packages/client-core/src/cbor/index';
import { e2ePort } from './support/ports';

const EMPTY: HostConfig = { port: e2ePort(8525), control: e2ePort(8526), webRoot: 'packages/web/dist', sfu: { port: e2ePort(7892), udpPort: e2ePort(7894) } };
const SERVER = 'Midgard Crew';
const OWNER = 'Ada';

let host: StartedHost | null = null;
let instance = '';

test.describe.configure({ mode: 'serial', timeout: TEST_TIMEOUT });
test.use({
  baseURL: `http://127.0.0.1:${String(EMPTY.port)}`,
  permissions: ['microphone'],
  launchOptions: { args: chromiumMediaArgs(writeFixtures(join(tmpdir(), 'dilla-media-fixtures'))) },
});

test.beforeAll(async () => {
  host = await startTestHost(EMPTY);
  const res = await fetch(`${host.url}/v1/instance`, { headers: { accept: 'application/cbor' } });
  expect(res.status, 'GET /v1/instance').toBe(200);
  const name = arr(decode(new Uint8Array(await res.arrayBuffer())))[5];
  if (typeof name !== 'string' || name === '') throw new Error('GET /v1/instance: element 5 is not the instance name');
  instance = name;
});
test.afterAll(async () => { await host?.stop(); });

async function makeChannel(page: Page, kind: 'text' | 'voice', name: string): Promise<void> {
  await newChannelButton(page).click();
  const dialog = page.getByRole('dialog', { name: copy('create.channel.title', { server: SERVER }), exact: true });
  await dialog.getByRole('radio', { name: copy(`create.channel.${kind}`), exact: true }).click();
  await dialog.getByRole('textbox', { name: copy('create.channel.name'), exact: true }).fill(name);
  await page.keyboard.press('Enter');
  await expect(dialog).toBeHidden({ timeout: WAIT });
}

/** Settings → Voice → noise suppression off: RNNoise removes the fake microphone's steady tone, and speaking needs it heard. */
async function noiseOff(page: Page): Promise<void> {
  const dialog = await openSettings(page, 'voice' as SettingsSection);
  const off = dialog.getByRole('radiogroup', { name: copy('settings.voice.noise'), exact: true }).getByRole('radio', { name: copy('settings.voice.off'), exact: true });
  await off.click();
  await expect(off).toHaveAttribute('aria-checked', 'true', { timeout: WAIT });
  await closeSettings(page);
}

test('voice channels stand in order in the one channel list, with their people nested under them', async ({ page, browserName }) => {
  test.skip(browserName !== 'chromium', 'the fake microphone is a Chromium switch');
  const h = host;
  if (h === null) throw new Error('the empty test host did not start');
  await page.setViewportSize({ width: 1280, height: 800 });

  // 1. Ada creates the server, then text alpha, voice lounge and text beta, in that order.
  await signUp(page, h.invite.split(',')[0], instance, OWNER, h.control);
  await addServerVia(page, 'create');
  const start = page.getByRole('dialog', { name: copy('create.server.title'), exact: true });
  await start.getByRole('textbox', { name: copy('create.server.name'), exact: true }).fill(SERVER);
  await page.keyboard.press('Enter');
  await expect(start).toBeHidden({ timeout: WAIT });
  await expectComposerReady(page, 'general');
  await makeChannel(page, 'text', 'alpha');
  await expectComposerReady(page, 'alpha');
  await makeChannel(page, 'voice', 'lounge');
  await expect(page.getByRole('button', { name: copy('voice.open', { name: 'lounge' }), exact: true })).toBeVisible({ timeout: WAIT });
  await makeChannel(page, 'text', 'beta');
  await expectComposerReady(page, 'beta');

  const nav = channelsNav(page);
  const lounge = nav.getByRole('button', { name: copy('voice.open', { name: 'lounge' }), exact: true });

  await test.step('lounge sits between alpha and beta, with the speaker icon', async () => {
    await expect(lounge).toBeVisible({ timeout: WAIT });
    const y = async (l: ReturnType<typeof channelRow>) => (await l.boundingBox())?.y ?? Number.NaN;
    const [a, v, b] = [await y(channelRow(page, 'alpha')), await y(lounge), await y(channelRow(page, 'beta'))];
    expect(a, 'alpha above lounge').toBeLessThan(v);
    expect(v, 'lounge above beta').toBeLessThan(b);
    await expect(lounge.locator('[data-icon="volume-2"]')).toHaveCount(1);
  });

  await test.step('no "active voice" and no "voice" heading', async () => {
    await expect(page.getByRole('heading', { name: copy('voice.heading.active'), exact: true })).toHaveCount(0);
    await expect(page.getByRole('heading', { name: copy('voice.heading.channels'), exact: true })).toHaveCount(0);
  });

  await test.step('Ada joins lounge: she is nested under its row, speaking, with her glyphs on her name’s line', async () => {
    await noiseOff(page);
    // web3.9 (R24): a click on a voice row opens it and joins.
    await lounge.click();
    await expect(page.locator('[data-call-phase]').first()).toHaveAttribute('data-call-phase', 'in', { timeout: WAIT });
    const item = lounge.locator('xpath=ancestor::li[1]');
    const person = item.locator('.d-voice-row__person').filter({ hasText: OWNER });
    await expect(person).toBeVisible({ timeout: WAIT });
    await expect(person).toHaveAttribute('data-speaking', 'true', { timeout: WAIT });
    const row = await lounge.boundingBox();
    const name = await person.locator('.d-voice-row__person-name').boundingBox();
    if (row === null || name === null) throw new Error('lounge or its person has no box');
    expect(name.x, 'the person is indented under the row').toBeGreaterThan(row.x);
    expect(name.y, 'the person is below the row').toBeGreaterThan(row.y);
    // web3.9 (WEB-DOCK, FA3): speaking is the person's ring, not a dot or a visible state; its word stays for assistive technology
    // only, in the row's states span. A mute or deafen glyph is a `.d-voice-row__state` (Ada is neither here), checked on the name's line.
    await expect(person.locator('.d-state-dot')).toHaveCount(0);
    const speakingWord = person.locator('.d-voice-row__states .d-sr-only').filter({ hasText: copy('voice.state.speaking') });
    await expect(speakingWord).toHaveCount(1);
    await expect(speakingWord).toBeAttached();
    const glyphs = person.locator('.d-voice-row__state');
    for (const box of await glyphs.evaluateAll(els => els.map(el => { const r = el.getBoundingClientRect(); return { y: r.y, h: r.height }; }))) {
      expect(Math.abs((box.y + box.h / 2) - (name.y + name.height / 2)), 'a glyph shares the name’s line').toBeLessThan(name.height);
    }
    // Still no heading after the join.
    await expect(page.getByRole('heading', { name: copy('voice.heading.active'), exact: true })).toHaveCount(0);
    await expect(page.getByRole('heading', { name: copy('voice.heading.channels'), exact: true })).toHaveCount(0);
  });

  await test.step('accessible', async () => { await expectAccessible(page, 'one channel list with Ada in lounge'); });
});
