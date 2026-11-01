// web3.7 W-Phone-4: the phone shell at 390 x 844 (flow 12), from an empty host and through the UI only: the sign-up steps
// keep the page unzoomed (field sizes of 16 px and up, a visual viewport that stays at scale 1), a server starts in the
// tab bar's Home tab, a channel is a pushed view with a back, the other tabs name themselves, the link banner follows
// the network, every tap target is 44 x 44 CSS px, and focus lands where flow 12 says. Chromium and WebKit only: Firefox has
// no mobile emulation (R23).
import { expect, type BrowserContext, type Locator, type Page, type PlaywrightWorkerArgs, type PlaywrightWorkerOptions } from '@playwright/test';
import { mkdirSync, rmSync } from 'node:fs';
import { addServerVia,
  COPY, SIGNIN, TEST_TIMEOUT, WAIT, channelRow, homeTile, newChannelButton, completeFactorStep, composerBox, copy, expectAccessible, expectComposerReady,
  expectHeading, expectShell, nextButton, newAccount, railItem, railNav, readRecoveryKey, sendText, keyField, submitButton, test,
  messageRow, signIn, type Account, membersToggle,
} from './support/app';
import { PROFILE_ROOT } from './support/persistent';
import { nextCode } from './support/totp';
import { startTestHost, type HostConfig, type StartedHost } from './support/host';
import { arr, decode } from '../../packages/client-core/src/cbor/index';

const PHONE_PORT = Number(process.env.DILLA_E2E_PHONE_PORT ?? 8557);
const HOST: HostConfig = { port: PHONE_PORT, control: PHONE_PORT + 1, webRoot: 'packages/web/dist' };
const SERVER = 'Midgard Crew';
const PHONE = { viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true, deviceScaleFactor: 3 } as const;
/** A phone on its side (web3.8 R9): wide, but 390 px tall with a coarse pointer, so it still gets the phone layout. */
const LANDSCAPE = { viewport: { width: 844, height: 390 }, isMobile: true, hasTouch: true, deviceScaleFactor: 3 } as const;

let host: StartedHost | null = null;
let instance = '';
let listPath = '';
let channelPath = '';
// web3.8 (R6): the key step comes after the login and the code, so the sign-in test signs in as the account the first
// test signed up (the suite is serial).
let signedUp: Account | null = null;

test.describe.configure({ mode: 'serial', timeout: TEST_TIMEOUT });
test.use({ baseURL: `http://127.0.0.1:${String(HOST.port)}`, ...PHONE });
test.skip(({ browserName }) => browserName === 'firefox', 'no mobile emulation');

test.beforeAll(async () => {
  host = await startTestHost(HOST);
  const res = await fetch(`${host.url}/v1/instance`, { headers: { accept: 'application/cbor' } });
  expect(res.status, 'GET /v1/instance').toBe(200);
  const name = arr(decode(new Uint8Array(await res.arrayBuffer())))[5];
  if (typeof name !== 'string' || name === '') throw new Error('GET /v1/instance: element 5 is not the instance name');
  instance = name;
});
test.afterAll(async () => { await host?.stop(); });

function started(): StartedHost {
  if (host === null) throw new Error('the empty test host did not start');
  return host;
}

/** A phone context of its own, with touch and mobile emulation. It is persistent (launchPersistentContext on its own profile
 *  dir), as a real phone's browser is: WebKit's ephemeral contexts behave like Safari private browsing, where the app cannot start. */
type PhoneFixtures = Pick<PlaywrightWorkerArgs, 'playwright'> & Pick<PlaywrightWorkerOptions, 'browserName' | 'launchOptions' | 'headless' | 'channel'>;
async function phoneContext(f: PhoneFixtures, dir: string, device: typeof PHONE | typeof LANDSCAPE = PHONE): Promise<BrowserContext> {
  rmSync(dir, { recursive: true, force: true });
  mkdirSync(dir, { recursive: true });
  const { launchOptions, headless, channel } = f;
  return f.playwright[f.browserName].launchPersistentContext(dir, {
    ...launchOptions, headless, channel, ...device, baseURL: `http://127.0.0.1:${String(HOST.port)}`,
  });
}
const phoneDir = (project: string, workerIndex: number, n: number) => `${PROFILE_ROOT}/${project}-${workerIndex}-phone${String(n)}`;

/** Every visible text input and textarea is at least 16 px, so no browser zooms on focus. A checkbox, a radio and a button
 *  take no typing, so no browser zooms to them and their own 13.33 px default is left alone. Task 11 (Jonas, Q6): a select
 *  keeps the prototype's size (iOS may zoom when it opens). */
async function expectFieldSizes(page: Page, state: string, expected = 1): Promise<number> {
  const { checked, small } = await page.evaluate(() => {
    const fields = [...document.querySelectorAll<HTMLElement>('input, textarea')]
    .filter((el) => !(el instanceof HTMLInputElement && ['checkbox', 'radio', 'button', 'submit', 'reset', 'hidden', 'file', 'range', 'color'].includes(el.type)))
    .filter((el) => el.getClientRects().length > 0 && getComputedStyle(el).visibility !== 'hidden')
    .map((el) => ({ what: `${el.tagName.toLowerCase()}${el instanceof HTMLInputElement ? `[${el.type}]` : ''} ${el.id}`, px: parseFloat(getComputedStyle(el).fontSize) }));
    return { checked: fields.length, small: fields.filter((f) => f.px < 16) };
  });
  expect(small, `fields under 16px in "${state}"`).toEqual([]);
  expect(checked, `"${state}" has text fields to measure`).toBeGreaterThanOrEqual(expected);
  return checked;
}
// A smoke check: Chromium's emulation never auto-zooms, so this cannot fail there. The 16px computed sizes are the proof, and
// the real-iPhone walk covers the zoom itself.
const scale = (page: Page) => page.evaluate(() => window.visualViewport?.scale ?? 1);
async function expectUnzoomedOnFocus(page: Page, field: Locator, state: string): Promise<void> {
  await field.focus();
  await expect(field).toBeFocused();
  expect(await scale(page), `visualViewport.scale after focusing ${state}`).toBe(1);
}
/**
 * Simulates the soft keyboard: Playwright's emulation never shrinks the visual viewport, and the shell treats the keyboard as
 * up only when `visualViewport.height` is more than 100 px below `innerHeight`. Up shadows `height` with a getter 300 px short
 * of the window and fires the viewport's resize, as a keyboard does; down deletes the shadow, which restores the real height.
 */
async function softKeyboard(page: Page, up: boolean): Promise<void> {
  await page.evaluate((on) => {
    const vv = window.visualViewport;
    if (vv === null) throw new Error('no visualViewport to simulate a keyboard on');
    if (on) Object.defineProperty(vv, 'height', { configurable: true, get: () => window.innerHeight - 300 });
    else Reflect.deleteProperty(vv, 'height');
    vv.dispatchEvent(new Event('resize'));
  }, up);
}
const noFocusMoved =(page: Page) => page.evaluate(() => document.activeElement === document.body || document.activeElement === null);
const tabBar = (page: Page) => page.getByRole('navigation', { name: copy('nav.tabs.label'), exact: true });
const tab = (page: Page, key: string) => tabBar(page).getByRole('link', { name: copy(key), exact: true });
const back = (page: Page) => page.getByRole('button', { name: copy('nav.back'), exact: true });
const viewTitle = (page: Page) => page.locator('.d-app-shell__main .d-phone-header__title');
const listHeading = (page: Page, key: string) => page.getByRole('heading', { name: copy(key), exact: true });

/** The element ends at or above the top of the tab bar, so nothing sits under it. */
async function expectAbove(el: Locator, bar: Locator, what: string): Promise<void> {
  const a = await el.boundingBox();
  const b = await bar.boundingBox();
  expect(a, `${what} has a box`).not.toBeNull();
  expect(b, 'the tab bar has a box').not.toBeNull();
  expect(a!.y + a!.height, `${what} ends above the tab bar`).toBeLessThanOrEqual(b!.y + 1);
}

async function expectTapTargets(controls: Locator, state: string): Promise<void> {
  const count = await controls.count();
  expect(count, `${state} has controls to measure`).toBeGreaterThan(0);
  const small: string[] = [];
  for (let i = 0; i < count; i++) {
    const control = controls.nth(i);
    if (!(await control.isVisible())) continue;
    const box = await control.boundingBox();
    if (box === null) continue;
    if (box.width < 44 || box.height < 44) small.push(`${(await control.getAttribute('aria-label')) ?? (await control.innerText()).trim()} ${box.width}x${box.height}`);
  }
  expect(small, `${state}: controls under 44x44 CSS px`).toEqual([]);
}

/** Jonas 2026-10-27 (pixel parity, supersedes Ruling N): a few phone controls are the prototype's size, not 44 (the rail's
 *  38px tiles; the phone header's Threads and Members 28 high, search 36 x 28). Each is measured at that size exactly. */
async function expectPrototypeSize(control: Locator, what: string, want: { w?: number; h: number }): Promise<void> {
  const box = await control.boundingBox();
  expect(box, `${what} has a box`).not.toBeNull();
  if (want.w !== undefined) expect(Math.abs(box!.width - want.w), `${what} is ${String(want.w)} wide (got ${String(box!.width)})`).toBeLessThanOrEqual(0.5);
  expect(Math.abs(box!.height - want.h), `${what} is ${String(want.h)} high (got ${String(box!.height)})`).toBeLessThanOrEqual(0.5);
}

test('the phone shell: sign-up unzoomed, the tab bar, a pushed channel, the tabs, the link, tap targets and focus', async ({ playwright, browserName, launchOptions, headless, channel, guard }, testInfo) => {
  const h = started();
  const dir = phoneDir(testInfo.project.name, testInfo.workerIndex, 1);
  const context = await phoneContext({ playwright, browserName, launchOptions, headless, channel }, dir);
  await guard.attach(context);
  const page = await context.newPage();
  const ada: Account = newAccount('Ada', h.control);
  let offlineLogged: number | null = null;
  try {
    // 1. The sign-up steps: no field under 16 px, and the focused fields leave the page at scale 1.
    await test.step('sign up with the bootstrap invite', async () => {
      await page.goto(`/welcome?invite=${encodeURIComponent(h.invite.split(',')[0])}`);
      await expectHeading(page, COPY.connectTitle, { instance });
      await expectFieldSizes(page, 'connect');
      await nextButton(page).click();
      await expectHeading(page, COPY.identityTitle);
      await expectFieldSizes(page, 'identity', 3);
      await expectUnzoomedOnFocus(page, page.getByLabel(copy(COPY.usernameLabel), { exact: true }), 'the username');
      await page.getByLabel(copy(COPY.usernameLabel), { exact: true }).fill(ada.username);
      await page.getByLabel(copy(COPY.displayLabel), { exact: true }).fill(ada.display);
      await expectUnzoomedOnFocus(page, page.getByLabel(copy(COPY.passwordLabel), { exact: true }), 'the password');
      await page.getByLabel(copy(COPY.passwordLabel), { exact: true }).fill(ada.password);
      await nextButton(page).click();
      await expectHeading(page, COPY.keysTitle);
      ada.recoveryKey = await readRecoveryKey(page);
      await expectFieldSizes(page, 'recovery key', 0);
      await page.getByRole('checkbox', { name: copy(COPY.keysAcknowledge), exact: true }).check();
      await nextButton(page).click();
      await expectHeading(page, COPY.deviceTitle);
      await expectFieldSizes(page, 'browser', 0);
      await submitButton(page).click();
      await completeFactorStep(page, ada, async (state) => { await expectFieldSizes(page, `factor ${state}`, state === 'totp' ? 1 : 0); });
      await expectHeading(page, COPY.doneTitle);
      await page.getByRole('button', { name: copy(COPY.finish), exact: true }).click();
      await expectShell(page);
      signedUp = ada;
    });

    // 2. A server from Home; the tab bar names three places (home, activity, you) and the status bar is gone.
    await test.step('start the server', async () => {
      await addServerVia(page, 'create');
      const dialog = page.getByRole('dialog', { name: copy('create.server.title'), exact: true });
      await dialog.getByRole('textbox', { name: copy('create.server.name'), exact: true }).fill(SERVER);
      await dialog.getByRole('textbox', { name: copy('create.server.name'), exact: true }).press('Enter');
      await expect(dialog).toBeHidden({ timeout: WAIT });
      // A new server opens its #general as a view (the tab bar stays, and nothing focuses the composer: script focus does not
      // open a phone's keyboard); back from it is the server's list.
      await expect(page).toHaveURL(/\/c\/[0-9a-f]{32}\/[0-9a-f]{32}$/, { timeout: WAIT });
      await expect(tabBar(page)).toBeVisible();
      await expect(page.locator('.d-composer textarea')).not.toBeFocused();
      await back(page).click();
      await expect(tabBar(page)).toBeVisible({ timeout: WAIT });
      await expect(tabBar(page).getByRole('link')).toHaveCount(3);
      expect(await tabBar(page).getByRole('link').evaluateAll((links) => links.map((l) => l.textContent?.trim()))).toEqual([copy('home.tab'), copy('nav.tab.activity'), copy('nav.tab.you')]);
      await expect(tab(page, 'home.tab')).toHaveAttribute('aria-current', 'page');
      // Home shows the rail with the @ tile first, and the list pane.
      await expect(railNav(page)).toBeVisible();
      await expect(railNav(page).getByRole('button').first()).toHaveAccessibleName(copy('home.tile'));
      await expect(homeTile(page)).not.toHaveAttribute('aria-current', 'page');
      await expect(page.getByRole('region', { name: copy(COPY.statusBarLabel), exact: true })).toHaveCount(0);
      await expect(channelRow(page, 'general')).toBeVisible({ timeout: WAIT });
      listPath = new URL(page.url()).pathname;
    });

    // 3. Tap #general: a pushed view, focus on the title, the tab bar under it, a composer that does not zoom and hides the bar.
    await test.step('open #general', async () => {
      await channelRow(page, 'general').tap();
      await expect(back(page)).toBeVisible({ timeout: WAIT });
      await expect(viewTitle(page)).toBeFocused();
      await expect(tabBar(page)).toBeVisible();
      channelPath = new URL(page.url()).pathname;
      await expectComposerReady(page, 'general');
      const composer = composerBox(page, 'general');
      expect(await composer.evaluate((el) => parseFloat(getComputedStyle(el).fontSize))).toBeGreaterThanOrEqual(16);
      // Task 7 (rule 0, Jonas 2026-10-27): the composer's footer tools are the prototype's 26 x 24 on a phone too.
      await expectPrototypeSize(page.getByRole('button', { name: copy('shell.composer.attach'), exact: true }), 'the attach button', { w: 26, h: 24 });
      await composer.tap();
      await expect(composer).toBeFocused();
      expect(await scale(page), 'visualViewport.scale after tapping into the composer').toBe(1);
      // Typing: the tab bar steps aside only when the soft keyboard is up (the visual viewport 100 px+ below the window), and
      // Playwright's emulation never shrinks it, so the keyboard is simulated until the composer is left.
      await softKeyboard(page, true);
      await expect(tabBar(page)).toBeHidden();
      await expect(page.locator('.d-app-shell')).toHaveAttribute('data-typing', 'true');
      const typing = await composer.boundingBox();
      expect(typing!.y + typing!.height, 'the composer ends inside the viewport while typing').toBeLessThanOrEqual(await page.evaluate(() => window.visualViewport?.height ?? innerHeight));
      // H1/H3: with the keyboard up the 48 header and the composer are both on screen and no call bar is (no call is up here).
      const viewport = await page.evaluate(() => window.visualViewport?.height ?? innerHeight);
      const header = await page.locator('.d-app-shell__main .d-phone-header').boundingBox();
      expect(header!.y, 'the header is at the top of the viewport').toBeGreaterThanOrEqual(0);
      expect(header!.height, 'the header is a 48 bar').toBeGreaterThanOrEqual(48);
      expect(header!.y + header!.height, 'the header sits above the composer').toBeLessThanOrEqual(typing!.y);
      expect(typing!.y, 'the composer is in the viewport').toBeLessThan(viewport);
      await expect(page.locator('.d-call-bar')).toHaveCount(0);
      await expect(page.locator('.d-phone-header__dot')).toHaveCount(0);
      await sendText(page, 'general', 'hej');
      await expect(messageRow(page, 'general', 'hej')).toBeVisible();
      // Leaving the composer: the tab bar comes back and the scale is still 1.
      await composer.evaluate(el => { el.blur(); });
      await expect(composer).not.toBeFocused();
      await softKeyboard(page, false);
      await expect(tabBar(page)).toBeVisible();
      expect(await scale(page), 'visualViewport.scale after leaving the composer').toBe(1);
      // R30: a message's actions are not on screen until a tap on it, and a second tap puts them away.
      const row = messageRow(page, 'general', 'hej');
      const actions = row.getByRole('toolbar', { name: copy('shell.message.toolbar'), exact: true });
      await expect(actions).toHaveCount(0);
      await row.locator('.d-message-row__body').tap();
      await expect(actions).toBeVisible();
      await row.locator('.d-message-row__body').tap();
      await expect(actions).toHaveCount(0);
      // Task 7 (Signal §5.10, the prototype's phone rows): the gutter is a bare 4px column that never shows a time, even on a
      // revealed follow-up (the head keeps it for a screen reader; a first row shows it beside the name). Nothing clips.
      await sendText(page, 'general', 'tack');
      const grouped = messageRow(page, 'general', 'tack');
      await expect(grouped).toHaveAttribute('data-grouped', 'true');
      await grouped.locator('.d-message-row__body').tap();
      for (const width of [390, 360]) {
        await page.setViewportSize({ width, height: 844 });
        const g = await grouped.locator('.d-message-row__gutter').evaluate(el => ({ width: el.getBoundingClientRect().width, visibility: getComputedStyle(el).visibility }));
        expect(g.visibility, `the phone gutter shows no time at ${String(width)}`).toBe('hidden');
        expect(Math.round(g.width), `the phone gutter is the prototype's 4px at ${String(width)}`).toBe(4);
      }
      await page.setViewportSize({ width: 390, height: 844 });
      await grouped.locator('.d-message-row__body').tap();
      await expect(grouped.getByRole('toolbar')).toHaveCount(0);
      await expectAccessible(page, 'the channel view');
      // The back stays a 44 target; the actions are the prototype's (Threads 30 x 28, Members 28 high, search 36 x 28).
      await expectTapTargets(page.locator('.d-phone-header__back'), 'the channel view header back');
      const headerActions = page.locator('.d-phone-header__actions');
      await expectPrototypeSize(headerActions.getByRole('button', { name: copy('layout.header.threads'), exact: true }), 'the header Threads', { w: 30, h: 28 });
      await expectPrototypeSize(headerActions.locator('[data-members-toggle]'), 'the header Members', { h: 28 });
      await expectPrototypeSize(headerActions.getByRole('button', { name: copy('search.open'), exact: true }), 'the header search', { w: 36, h: 28 });
      await expect(page.locator('.d-phone-header').getByRole('button', { name: copy('nav.channel.menu'), exact: true })).toHaveCount(0);
    });

    // 3a. web3.7.2: the shell is fixed and follows the visual viewport's offset (--d-vvtop stands in for the iOS pan, which
    // Playwright cannot produce), and a muted call bar button reads as off, not as the accent fill.
    await test.step('the shell follows --d-vvtop and a muted call bar is not accent-filled', async () => {
      const shell = page.locator('.d-app-shell');
      await expect(shell).toHaveCSS('position', 'fixed');
      await page.evaluate(() => document.documentElement.style.setProperty('--d-vvtop', '95px'));
      expect((await shell.boundingBox())!.y, 'the shell sits at the visual viewport top').toBe(95);
      const ringY = await page.evaluate(() => {
        const ring = document.createElement('div');
        ring.className = 'd-app-shell__ring';
        document.querySelector('.d-app-shell')!.append(ring);
        const y = ring.getBoundingClientRect().y;
        ring.remove();
        return y;
      });
      expect(ringY, 'the ring sits at the visual viewport top, not below it').toBe(95);
      await page.evaluate(() => document.documentElement.style.setProperty('--d-vvtop', '0px'));
      expect((await shell.boundingBox())!.y).toBe(0);
      const muted = await page.evaluate(() => {
        const host = document.querySelector('.d-app-shell')!;
        const bar = document.createElement('div');
        bar.className = 'd-call-bar';
        bar.innerHTML = '<button type="button" class="d-btn d-call-bar__mute" aria-pressed="true" data-size="md"></button><i></i><b></b>';
        host.append(bar);
        // Signal (task 8): the toggled-on danger tint, the --danger-soft fill and the --danger-border edge.
        bar.querySelector('i')!.style.setProperty('background', 'var(--danger-soft)');
        bar.querySelector('i')!.style.setProperty('color', 'var(--danger)');
        bar.querySelector('b')!.style.setProperty('color', 'var(--danger-border)');
        const button = getComputedStyle(bar.querySelector('button')!);
        const probe = getComputedStyle(bar.querySelector('i')!);
        const edge = getComputedStyle(bar.querySelector('b')!);
        const out = { bg: button.backgroundColor, color: button.color, edge: button.borderTopColor, surface: probe.backgroundColor, danger: probe.color, border: edge.color };
        bar.remove();
        return out;
      });
      expect(muted, 'a muted mic: the danger glyph on the danger tint, never the accent fill').toEqual({ bg: muted.surface, color: muted.danger, edge: muted.border, surface: muted.surface, danger: muted.danger, border: muted.border });
    });

    // 3b. A thread is a sheet: opened from the toolbar, closed by the system back, then reopened by a real click on the chip.
    await test.step('a thread sheet opens from the chip on a real click', async () => {
      const row = messageRow(page, 'general', 'hej');
      await row.focus();
      await page.keyboard.press('Enter'); // Enter on the focused row reveals its actions (R30)
      await row.getByRole('button', { name: copy('thread.open'), exact: true }).click();
      const reply = page.getByRole('textbox', { name: copy('thread.composer'), exact: true });
      await expect(reply).toBeVisible({ timeout: WAIT });
      await reply.fill('svar');
      await reply.press('Enter');
      await expect(page.getByRole('complementary', { name: copy('thread.title') }).getByText('svar')).toBeVisible({ timeout: WAIT });
      // Task 7: the textarea sits in the Signal box's input row over its footer, so the composer (its form) is measured: it ends where
      // the sheet does.
      const box = await reply.locator('xpath=ancestor::form[1]').boundingBox();
      const sheetBox = await page.getByRole('complementary', { name: copy('thread.title') }).boundingBox();
      expect(box!.y + box!.height, 'the thread composer sits at the bottom of the sheet').toBeGreaterThanOrEqual(sheetBox!.y + sheetBox!.height - 16);
      await page.goBack();
      await expect(page.getByRole('complementary', { name: copy('thread.title') })).toBeHidden({ timeout: WAIT });
      await row.getByRole('button', { name: copy('thread.summary.one'), exact: true }).click();
      await expect(page.getByRole('complementary', { name: copy('thread.title') })).toBeVisible({ timeout: WAIT });
      await page.goBack();
      await expect(page.getByRole('complementary', { name: copy('thread.title') })).toBeHidden({ timeout: WAIT });
    });

    // The tab bar is the way out of a view: the current tab goes to its list, and the tabs keep their memory.
    await test.step('tapping the current tab in a channel view shows the channel list', async () => {
      await tab(page, 'home.tab').tap();
      await expect(channelRow(page, 'general')).toBeVisible({ timeout: WAIT });
      await expect(page).toHaveURL(new RegExp(`${listPath}$`));
      // The @ tile stays on Home: the DM list shows in the list pane, and the server's tile brings its channels back.
      await homeTile(page).tap();
      await expect(homeTile(page)).toHaveAttribute('aria-current', 'page');
      await expect(tab(page, 'home.tab')).toHaveAttribute('aria-current', 'page');
      await expect(page).toHaveURL(/\/dm$/);
      await railItem(page, SERVER).tap();
      await expect(channelRow(page, 'general')).toBeVisible({ timeout: WAIT });
      await channelRow(page, 'general').tap();
      await expect(back(page)).toBeVisible({ timeout: WAIT });
    });

    // 4. Back: the list, with #general's row focused.
    await test.step('back to the list', async () => {
      await back(page).click();
      await expect(tabBar(page)).toBeVisible({ timeout: WAIT });
      await expect(channelRow(page, 'general')).toBeFocused();
      await expect(tab(page, 'home.tab')).toHaveAttribute('aria-current', 'page');
    });

    await test.step('the list is accessible and its tap targets are 44 px', async () => {
      await expectAccessible(page, 'the servers list');
      await expectTapTargets(tabBar(page).getByRole('link'), 'the tab bar');
      await expectPrototypeSize(homeTile(page), 'the @ tile', { w: 38, h: 38 });
      // Ruling AB (Jonas 2026-10-27): beside the server's name sits only the prototype's ⌄ (server settings, 26 x 26), plus the
      // "+" while there are no categories; no search, no reminders. "leave" is in server settings.
      const menu = page.getByRole('button', { name: copy('admin.open'), exact: true });
      await expect(page.locator('.d-channel-list__head').getByRole('button', { name: copy('search.open'), exact: true })).toHaveCount(0);
      await expect(page.getByRole('button', { name: 'Reminders', exact: true })).toHaveCount(0);
      await expect(page.getByRole('button', { name: copy('nav.server.menu'), exact: true })).toHaveCount(0);
      await expect(page.getByRole('menuitem')).toHaveCount(0);
      await expectPrototypeSize(menu, 'the server ⌄', { w: 26, h: 26 });
      await expect(newChannelButton(page)).toBeVisible();
    });

    // Walk 1: a phone has no Control+K, so search opens from a conversation's phone header (Ruling AB: the list head has none),
    // full screen; a hit opens its channel.
    await test.step('search from the channel header opens full screen and a hit opens its message', async () => {
      await channelRow(page, 'general').tap();
      await page.locator('.d-phone-header').getByRole('button', { name: copy('search.open'), exact: true }).tap();
      const palette = page.getByRole('dialog', { name: copy('search.placeholder'), exact: true });
      await expect(palette).toBeVisible({ timeout: WAIT });
      const box = await palette.boundingBox();
      expect(box!.width, 'the palette covers the phone').toBeGreaterThanOrEqual(389);
      expect(box!.height, 'the palette covers the phone').toBeGreaterThanOrEqual(843);
      const input = page.getByRole('combobox', { name: copy('search.placeholder'), exact: true });
      await expect(input).toBeFocused();
      await input.fill('hej');
      const hit = palette.getByRole('option').filter({ hasText: 'hej' }).first();
      await expect(hit).toBeVisible({ timeout: WAIT });
      await hit.tap();
      await expect(palette).toBeHidden({ timeout: WAIT });
      await expect(back(page)).toBeVisible({ timeout: WAIT });
      await expectComposerReady(page, 'general');
      await back(page).click();
      await expect(tabBar(page)).toBeVisible({ timeout: WAIT });
    });

    // Ruling AB: the direct messages list (the @ tile's Home) has no search in its head, only "message someone".
    await test.step('the direct messages list head holds only "message someone"', async () => {
      await homeTile(page).tap();
      const head = page.locator('.d-channel-list__head');
      await expect(head.getByRole('button', { name: copy('shell.dms.new'), exact: true })).toBeVisible({ timeout: WAIT });
      await expect(head.getByRole('button', { name: copy('search.open'), exact: true })).toHaveCount(0);
      await railItem(page, SERVER).tap();
      await expect(channelRow(page, 'general')).toBeVisible({ timeout: WAIT });
    });

    // The channel menu's sheet: closed by the system back and by its own back, focus returns to the menu trigger.
    await test.step('the members sheet returns focus to the Members button', async () => {
      await channelRow(page, 'general').tap();
      await expect(back(page)).toBeVisible({ timeout: WAIT });
      const menu = membersToggle(page);
      await menu.click();
      const close = page.getByRole('button', { name: copy('members.close'), exact: true });
      await expect(close).toBeVisible({ timeout: WAIT });
      // Task 11 (Jonas, Q4): the members sheet covers the whole screen, the tab bar included (the prototype's).
      const sheet = await page.getByRole('complementary', { name: /members/ }).boundingBox();
      expect(sheet!.y + sheet!.height, 'the members sheet reaches the bottom of the screen').toBeGreaterThanOrEqual(844 - 1);
      await page.goBack();
      await expect(close).toBeHidden({ timeout: WAIT });
      await expect(menu).toBeFocused();
      await menu.click();
      await expect(close).toBeVisible({ timeout: WAIT });
      await close.click();
      await expect(close).toBeHidden({ timeout: WAIT });
      await expect(menu).toBeFocused();
      await back(page).click();
      await expect(tabBar(page)).toBeVisible({ timeout: WAIT });
    });

    // 5. The other tabs name themselves, and a tap on a tab puts focus on its heading.
    await test.step('the @ tile on Home, then the Activity and You tabs', async () => {
      await homeTile(page).tap();
      await expect(homeTile(page)).toHaveAttribute('aria-current', 'page');
      await expect(tab(page, 'home.tab')).toHaveAttribute('aria-current', 'page');
      await expect(listHeading(page, 'shell.dms.title')).toBeVisible();
      await expect(page.getByText(copy('shell.dms.empty'), { exact: true })).toBeVisible({ timeout: WAIT });
      await expectTapTargets(page.getByRole('button', { name: copy('shell.dms.new'), exact: true }), 'message someone');
      await expectAccessible(page, 'Home with @ selected');
      // Home remembers where it was left; leave it on the server list for the steps below.
      await railItem(page, SERVER).tap();
      await expect(channelRow(page, 'general')).toBeVisible({ timeout: WAIT });

      await tab(page, 'nav.tab.activity').tap();
      await expect(tab(page, 'nav.tab.activity')).toHaveAttribute('aria-current', 'page');
      await expect(listHeading(page, 'activity.title')).toBeFocused();
      await expect(page.getByRole('region', { name: copy('activity.emptyTitle'), exact: true })).toBeVisible();
      // Task 11 (Jonas, Q3): the prototype's Activity heading (18px over the 14px title), not a 48px bar.
      const activityHead = page.locator('.d-app-shell__sidebar .dw-activity .d-phone-header');
      await expect(activityHead).toBeVisible();
      expect(await activityHead.evaluate(e => getComputedStyle(e).paddingTop), 'Activity: 18px above the title').toBe('18px');
      expect(await activityHead.locator('.d-phone-header__title').evaluate(e => getComputedStyle(e).fontSize), 'Activity: a 14px title').toBe('14px');
      await expectAccessible(page, 'Activity');

      await tab(page, 'nav.tab.you').tap();
      await expect(tab(page, 'nav.tab.you')).toHaveAttribute('aria-current', 'page');
      await expect(page.getByRole('heading', { level: 2, name: copy('you.title'), exact: true })).toBeFocused();
      // Task 11 (Jonas, Q3): the prototype's You: the account, the connection box, and the rows Settings, Status and Sign out
      // (48px each, no native select); Diagnostics keeps the node, link, generation and devices.
      await expect(page.locator('.dw-you select')).toHaveCount(0);
      // The box's first line in full: "Connected to {host} · {ms} ms" (or without the ms before the first heartbeat's ack).
      const pattern = (key: 'you.connected' | 'you.connectedNoPing') =>
        copy(key, { host: 'HOSTX', ms: 999 }).replace(/[.*+?^${}()|[\]\\]/g, '\\$&').replace('HOSTX', '\\S+').replace('999', '\\d+');
      await expect(page.locator('.dw-you__facts')).toContainText(new RegExp(`^(${pattern('you.connected')}|${pattern('you.connectedNoPing')})`), { timeout: WAIT });
      for (const name of [copy('you.settings'), copy('workspace.signOut')]) {
        expect((await page.getByRole('button', { name, exact: true }).boundingBox())!.height, `${name} is a 48px row`).toBeGreaterThanOrEqual(47);
      }
      await expectTapTargets(page.getByRole('button', { name: new RegExp(`^${copy('you.statusRow')}`) }), 'the status row');
      await expectAccessible(page, 'You');
    });

    // The You tab has one settings entry. The modal index selects a section, and Back returns to the index.
    await test.step('phone settings follows index → section → index', async () => {
      const row = page.locator('[data-settings-section="index"]');
      await expect(row).toContainText(copy('you.settings'));
      await row.tap();
      const settings = page.getByRole('dialog', { name: copy('settings.title'), exact: true });
      await expect(settings).toBeVisible({ timeout: WAIT });
      await expect(page).toHaveURL('/settings', { timeout: WAIT });
      await expect(settings.getByRole('navigation', { name: copy('settings.nav.label'), exact: true })).toBeVisible();
      const privacy = settings.getByRole('button', { name: copy('workspace.privacy'), exact: true });
      await expect(privacy).toBeVisible({ timeout: WAIT });
      await privacy.tap();
      await expect(page).toHaveURL(/\/settings\/privacy$/, { timeout: WAIT });
      // Task 10 (README §5.10): a phone section keeps the nav as chips above it and has no Back; the browser's back returns.
      await expect(settings.getByRole('button', { name: copy('workspace.back'), exact: true })).toHaveCount(0);
      await expect(settings.getByRole('button', { name: copy('workspace.privacy'), exact: true })).toHaveAttribute('aria-current', 'page');
      await expect(tabBar(page)).toHaveCount(0);
      await page.goBack();
      await expect(page).toHaveURL('/settings', { timeout: WAIT });
      await expect(settings.getByRole('navigation', { name: copy('settings.nav.label'), exact: true })).toBeVisible();
      await page.keyboard.press('Escape');
      await expect(settings).toBeHidden({ timeout: WAIT });
      await expect(row).toBeFocused();
    });

    // Server settings is a fullscreen modal; section URLs are scoped under the community.
    await test.step('server settings follows index → section and closes on Escape', async () => {
      await tab(page, 'home.tab').tap();
      await page.getByRole('button', { name: copy('admin.open'), exact: true }).click().then(() => page.getByRole('menuitem', { name: copy('signalSettings.menu.serverSettings'), exact: true }).click());
      const dialog = page.getByRole('dialog', { name: copy('admin.title'), exact: true });
      await expect(dialog).toBeVisible({ timeout: WAIT });
      await expect(page).toHaveURL(/\/c\/[0-9a-f]{32}\/settings$/, { timeout: WAIT });
      await expect(tabBar(page)).toHaveCount(0);
      await dialog.getByRole('button', { name: copy('workspace.overview'), exact: true }).tap();
      await expect(page).toHaveURL(/\/c\/[0-9a-f]{32}\/settings\/overview$/, { timeout: WAIT });
      await expect(dialog.getByRole('button', { name: copy('workspace.overview'), exact: true })).toHaveAttribute('aria-current', 'page');
      await page.goBack();
      await expect(page).toHaveURL(/\/c\/[0-9a-f]{32}\/settings$/, { timeout: WAIT });
      await page.keyboard.press('Escape');
      await expect(dialog).toBeHidden({ timeout: WAIT });
      await expect(tabBar(page)).toBeVisible({ timeout: WAIT });
    });

    // A deep link to /dm is Home with the @ tile selected, on a fresh page.
    await test.step('a /dm deep link lands on Home with @ selected', async () => {
      await page.goto('/dm');
      await expect(tabBar(page)).toBeVisible({ timeout: WAIT });
      await expect(tab(page, 'home.tab')).toHaveAttribute('aria-current', 'page');
      await expect(homeTile(page)).toHaveAttribute('aria-current', 'page', { timeout: WAIT });
      await expect(listHeading(page, 'shell.dms.title')).toBeVisible();
      await expect(page).toHaveURL(/\/dm$/);
      await expectAccessible(page, '/dm deep link');
      await railItem(page, SERVER).tap();
      await expect(channelRow(page, 'general')).toBeVisible({ timeout: WAIT });
    });

    // 6. The link banner follows the network.
    await test.step('offline shows the link banner, online removes it', async () => {
      await tab(page, 'home.tab').tap();
      offlineLogged = guard.consoleErrors.length;
      await context.setOffline(true);
      await expect(page.getByText(copy('nav.link.offline'), { exact: true })).toBeVisible({ timeout: WAIT });
      await context.setOffline(false);
      await expect(page.getByText(copy('nav.link.offline'), { exact: true })).toBeHidden({ timeout: WAIT });
      // WebKit can report an offline fetch after this step ends. Clear the known browser wording after closing the context.
    });

    // A full-screen settings modal covers the phone tab bar and shell; the offline state remains announced to the user.
    await test.step('offline state remains available while fullscreen Settings is open', async () => {
      await tab(page, 'nav.tab.you').tap();
      await page.locator('[data-settings-section="index"]').tap();
      const settings = page.getByRole('dialog', { name: copy('settings.title'), exact: true });
      await settings.getByRole('button', { name: copy('workspace.privacy'), exact: true }).tap();
      await expect(page).toHaveURL(/\/settings\/privacy$/, { timeout: WAIT });
      await context.setOffline(true);
      const text = settings.getByRole('status').filter({ hasText: copy('nav.link.offline') });
      await expect(text).toBeVisible({ timeout: WAIT });
      await expect(settings).toHaveAttribute('aria-modal', 'true');
      await expect(settings.getByRole('button', { name: copy('settings.close'), exact: true })).toBeVisible();
      await expect(tabBar(page)).toHaveCount(0);
      await context.setOffline(false);
      await expect(text).toBeHidden({ timeout: WAIT });
      await page.keyboard.press('Escape');
      await expect(settings).toBeHidden({ timeout: WAIT });
      await tab(page, 'home.tab').tap();
      await expect(channelRow(page, 'general')).toBeVisible({ timeout: WAIT });
    });

    // A deep link or a first load moves no focus.
    await test.step('a reload on a channel moves no focus', async () => {
      await channelRow(page, 'general').tap();
      await expect(back(page)).toBeVisible({ timeout: WAIT });
      await page.reload();
      await expect(back(page)).toBeVisible({ timeout: WAIT });
      await expectComposerReady(page, 'general');
      expect(await noFocusMoved(page), 'nothing focused after a reload').toBe(true);
    });

    // A cold deep link to a channel (a fresh page, no in-app history): back is the server's list, never off site, with that
    // channel's row focused.
    await test.step('a cold deep link, then back, lands on the list with the channel row focused', async () => {
      expect(channelPath).toMatch(/^\/c\/[0-9a-f]{32}\/[0-9a-f]{32}$/);
      // Not a second tab (the app lives in one tab per profile): a fresh load of the same page, behind its earlier history.
      await page.goto(channelPath);
      await expect(back(page)).toBeVisible({ timeout: WAIT });
      await expectComposerReady(page, 'general');
      await back(page).click();
      await expect(page).toHaveURL(new RegExp(`${listPath}$`), { timeout: WAIT });
      expect(page.url().startsWith(`http://127.0.0.1:${String(HOST.port)}/`)).toBe(true);
      await expect(tabBar(page)).toBeVisible({ timeout: WAIT });
      await expect(channelRow(page, 'general')).toBeFocused();
    });
  } finally {
    await guard.collectWorkers();
    await context.close();
    if (offlineLogged !== null) {
      const offlineLine = (text: string): boolean => text.includes('ERR_INTERNET_DISCONNECTED')
        || text === 'Failed to load resource: WebKit encountered an internal error'
        || (text.startsWith(`Fetch API cannot load http://127.0.0.1:${String(HOST.port)}/v1/`)
          && text.endsWith(' due to access control checks.'));
      for (let i = guard.consoleErrors.length - 1; i >= offlineLogged; i--) {
        if (offlineLine(guard.consoleErrors[i])) guard.consoleErrors.splice(i, 1);
      }
    }
    rmSync(dir, { recursive: true, force: true });
  }
});

test('the sign-in fields stay at 16 px and unzoomed on a phone', async ({ playwright, browserName, launchOptions, headless, channel, guard }, testInfo) => {
  started();
  const dir = phoneDir(testInfo.project.name, testInfo.workerIndex, 2);
  const context = await phoneContext({ playwright, browserName, launchOptions, headless, channel }, dir);
  await guard.attach(context);
  const page = await context.newPage();
  if (signedUp === null) throw new Error('the phone shell test did not sign up an account');
  const account = signedUp;
  try {
    // web3.8 (R6, R7): the login first, then the code, then the key.
    await page.goto('/welcome');
    await expectHeading(page, COPY.connectTitle, { instance });
    await page.getByRole('button', { name: copy(SIGNIN.entry), exact: true }).click();
    await expectHeading(page, SIGNIN.loginTitle, { instance });
    await expectFieldSizes(page, 'sign-in login');
    await expectUnzoomedOnFocus(page, page.getByLabel(copy(SIGNIN.username), { exact: true }), 'the username');
    await expectUnzoomedOnFocus(page, page.getByLabel(copy(SIGNIN.password), { exact: true }), 'the password');
    await page.getByLabel(copy(SIGNIN.username), { exact: true }).fill(account.username);
    await page.getByLabel(copy(SIGNIN.password), { exact: true }).fill(account.password);
    await page.getByRole('button', { name: copy(SIGNIN.loginSubmit), exact: true }).click();
    await expectHeading(page, 'signin.totp.title');
    await expectFieldSizes(page, 'sign-in code');
    await expectUnzoomedOnFocus(page, page.getByLabel(copy('signin.totp.code'), { exact: true }), 'the code');
    await page.getByLabel(copy('signin.totp.code'), { exact: true }).fill(await nextCode(account.control, account));
    await page.getByRole('button', { name: copy(SIGNIN.loginSubmit), exact: true }).click();
    await expectHeading(page, SIGNIN.keyTitle);
    await expectFieldSizes(page, 'sign-in key');
    await expectUnzoomedOnFocus(page, keyField(page), 'the recovery key');
    // web3.7.2: no shell here, so the page lock is off and a short window (the keyboard up) still scrolls the form.
    await page.setViewportSize({ width: 390, height: 300 });
    const scrolled = await page.evaluate(() => {
      const el = document.scrollingElement!;
      el.scrollTo(0, 40);
      return { tall: el.scrollHeight > el.clientHeight, y: el.scrollTop };
    });
    expect(scrolled, 'the sign-in page scrolls with a short window').toEqual({ tall: true, y: expect.any(Number) });
    expect(scrolled.y).toBeGreaterThan(0);
  } finally {
    await guard.collectWorkers();
    await context.close();
    rmSync(dir, { recursive: true, force: true });
  }
});

// web3.8 (R9, R10, pre-flight C-7): a phone on its side is 844 x 390. The width alone would give it the desktop layout; the
// coarse pointer under 32rem tall gives it the phone one, the container rules' data-layout twins apply, and the page lock is
// the attribute the phone shell sets.
test('a landscape phone (844 x 390) gets the phone layout', async ({ playwright, browserName, launchOptions, headless, channel, guard }, testInfo) => {
  started();
  const dir = phoneDir(testInfo.project.name, testInfo.workerIndex, 3);
  const context = await phoneContext({ playwright, browserName, launchOptions, headless, channel }, dir, LANDSCAPE);
  await guard.attach(context);
  const page = await context.newPage();
  if (signedUp === null) throw new Error('the phone shell test did not sign up an account');
  const account = signedUp;
  try {
    await signIn(page, account, instance);
    await expect(page.locator('.d-app-shell')).toHaveAttribute('data-layout', 'phone');
    await expect(tabBar(page)).toBeVisible({ timeout: WAIT });
    await test.step('three tabs, the @ tile, and a /dm deep link at 844 x 390', async () => {
      await expect(tabBar(page).getByRole('link')).toHaveCount(3);
      await expectTapTargets(tabBar(page).getByRole('link'), 'the tab bar at 844 x 390');
      await expectPrototypeSize(homeTile(page), 'the @ tile at 844 x 390', { w: 38, h: 38 });
      await homeTile(page).tap();
      await expect(homeTile(page)).toHaveAttribute('aria-current', 'page');
      await expectAccessible(page, 'Home with @ selected at 844 x 390');
      await page.goto('/dm');
      await expect(tab(page, 'home.tab')).toHaveAttribute('aria-current', 'page', { timeout: WAIT });
      await expect(homeTile(page)).toHaveAttribute('aria-current', 'page', { timeout: WAIT });
      await railItem(page, SERVER).tap();
      await expect(channelRow(page, 'general')).toBeVisible({ timeout: WAIT });
    });
    expect(await page.evaluate(() => document.documentElement.hasAttribute('data-phone-shell')), 'the phone shell locks the page by an attribute').toBe(true);
    expect(await page.evaluate(() => getComputedStyle(document.documentElement).overflow)).toBe('hidden');

    await test.step('a message action is a 44 px target', async () => {
      await channelRow(page, 'general').tap();
      await expect(back(page)).toBeVisible({ timeout: WAIT });
      // A new browser does not see earlier messages: send one, reply in its thread, and the chip appears under it.
      await expectComposerReady(page, 'general');
      await sendText(page, 'general', 'hej');
      const row = messageRow(page, 'general', 'hej');
      await row.locator('.d-message-row__body').tap();
      await row.getByRole('button', { name: copy('thread.open'), exact: true }).tap();
      const reply = page.getByRole('textbox', { name: copy('thread.composer'), exact: true });
      await expect(reply).toBeVisible({ timeout: WAIT });
      await reply.fill('svar');
      await reply.press('Enter');
      await expect(page.getByRole('complementary', { name: copy('thread.title') }).getByText('svar')).toBeVisible({ timeout: WAIT });
      await page.goBack();
      await expect(page.getByRole('complementary', { name: copy('thread.title') })).toBeHidden({ timeout: WAIT });
      const chip = row.getByRole('button', { name: copy('thread.summary.one'), exact: true });
      await expect(chip).toBeVisible({ timeout: WAIT });
      // Task 7 (rule 0, Jonas 2026-10-27): the thread chip is the prototype's 24px high on a phone too.
      await expectPrototypeSize(chip, 'the thread chip of a message at 844 x 390', { h: 24 });
    });

    // A call needs a live SFU, which this host does not run, so this step does not measure a real call's controls. It puts
    // the elements the twinned rules select (the DM call pane, a call-strip button) in the shell and
    // reads what the data-layout twins do to them (the same technique as the call bar probe in test 1).
    await test.step('the DM call pane fills the layout, and the call-strip buttons are 44 px', async () => {
      const probe = await page.evaluate(() => {
        const content = document.querySelector('.d-app-shell__content')!;
        const call = document.createElement('div');
        call.className = 'dw-dm-call';
        const strip = document.createElement('div');
        strip.className = 'dw-call-strip';
        strip.innerHTML = '<button type="button" class="d-btn" data-size="sm"></button>';
        content.append(call, strip);
        const out = {
          maxHeight: getComputedStyle(call).maxHeight,
          strip: strip.querySelector('button')!.getBoundingClientRect().height,
        };
        call.remove();
        strip.remove();
        return out;
      });
      expect(probe.maxHeight, 'the phone DM call fills the pane (the desktop one is capped at 40%)').toBe('none');
      expect(probe.strip, 'a call-strip button is a 44 px target').toBeGreaterThanOrEqual(44);
    });
  } finally {
    await guard.collectWorkers();
    await context.close();
    rmSync(dir, { recursive: true, force: true });
  }
});
