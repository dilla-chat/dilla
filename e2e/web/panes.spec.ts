// web3.8, WS4 (item 10): the resizable panes. From an empty host, Ada starts "Midgard Crew"; the sidebar splitter moves with
// the mouse and with the keyboard, the width is kept across a reload (localStorage `dilla.paneWidths`, read and written only
// in layout.ts), Enter resets it, Home and End go to the limits, and the aside has a splitter only while it is a column.
import { expect, type Page } from '@playwright/test';
import { addServerVia, TEST_TIMEOUT, WAIT, copy, expectAccessible, expectComposerReady, railNav, signUp, test } from './support/app';
import { startTestHost, type HostConfig, type StartedHost } from './support/host';
import { arr, decode } from '../../packages/client-core/src/cbor/index';

const EMPTY: HostConfig = { port: 8521, control: 8522, webRoot: 'packages/web/dist' };
const SERVER = 'Midgard Crew';

let host: StartedHost | null = null;
let instance = '';

test.describe.configure({ mode: 'serial', timeout: TEST_TIMEOUT });
test.use({ baseURL: `http://127.0.0.1:${String(EMPTY.port)}` });

test.beforeAll(async () => {
  host = await startTestHost(EMPTY);
  const res = await fetch(`${host.url}/v1/instance`, { headers: { accept: 'application/cbor' } });
  expect(res.status, 'GET /v1/instance').toBe(200);
  const name = arr(decode(new Uint8Array(await res.arrayBuffer())))[5];
  if (typeof name !== 'string' || name === '') throw new Error('GET /v1/instance: element 5 is not the instance name');
  instance = name;
});
test.afterAll(async () => { await host?.stop(); });

const sidebarSplitter = (page: Page) => page.getByRole('separator', { name: copy('pane.resize.sidebar'), exact: true });
const asideSplitter = (page: Page) => page.getByRole('separator', { name: copy('pane.resize.aside'), exact: true });
const sidebarWidth = (page: Page) => page.locator('.d-app-shell__sidebar').evaluate(el => el.getBoundingClientRect().width);

test('the sidebar splitter drags and takes keys, the width survives a reload, and the aside splitter exists only as a column', async ({ page }) => {
  const h = host;
  if (h === null) throw new Error('the empty test host did not start');
  await page.setViewportSize({ width: 1280, height: 800 });
  await signUp(page, h.invite.split(',')[0], instance, 'Ada', h.control);
  await addServerVia(page, 'create');
  const start = page.getByRole('dialog', { name: copy('create.server.title'), exact: true });
  await start.getByRole('textbox', { name: copy('create.server.name'), exact: true }).fill(SERVER);
  await page.keyboard.press('Enter');
  await expect(start).toBeHidden({ timeout: WAIT });
  await expect(page).toHaveURL(/\/c\/[0-9a-f]{32}\/[0-9a-f]{32}$/, { timeout: WAIT });
  await expectComposerReady(page, 'general');

  // 0. web3.9 (controller addition, UI-4): at 1440x900 the user panel is 64 px high (--panel-h, README §5.4) and the composer's resting top
  // meets the panel's top, within 1 px (--d-composer-band, set on the shell).
  await page.setViewportSize({ width: 1440, height: 900 });
  const tops = async () => page.evaluate(() => {
    const panel = document.querySelector('.d-user-panel')?.getBoundingClientRect();
    const composer = document.querySelector('.d-composer')?.getBoundingClientRect();
    return { panelTop: panel?.top ?? NaN, panelBottom: panel?.bottom ?? NaN, panelHeight: panel?.height ?? NaN, composerTop: composer?.top ?? NaN, composerBottom: composer?.bottom ?? NaN };
  });
  // Task 7 (Signal §5.5): the prototype's two-row composer is taller than the 64 px panel, so the two share their bottom edge, not
  // their top: the composer's top stands at or above the panel's.
  await expect.poll(async () => { const m = await tops(); return Math.abs(m.composerBottom - m.panelBottom); }, { message: 'composer and user panel bottoms at 1440x900' }).toBeLessThanOrEqual(1);
  expect((await tops()).composerTop, 'the composer reaches at least the panel top').toBeLessThanOrEqual((await tops()).panelTop + 1);
  expect((await tops()).panelHeight, 'the user panel is 64 px high').toBeCloseTo(64, 0);
  await page.setViewportSize({ width: 1280, height: 800 });

  const splitter = sidebarSplitter(page);
  await expect(splitter).toBeVisible({ timeout: WAIT });

  // 1. A drag of +80 px with the mouse makes the sidebar 80 ±1 px wider.
  const before = await sidebarWidth(page);
  const box = await splitter.boundingBox();
  if (box === null) throw new Error('the sidebar splitter has no box');
  const x = box.x + box.width / 2;
  const y = box.y + box.height / 2;
  await page.mouse.move(x, y);
  await page.mouse.down();
  await page.mouse.move(x + 40, y, { steps: 5 });
  await page.mouse.move(x + 80, y, { steps: 5 });
  await page.mouse.up();
  await expect.poll(() => sidebarWidth(page), { message: 'the sidebar after the drag' }).toBeGreaterThanOrEqual(before + 79);
  expect(await sidebarWidth(page)).toBeLessThanOrEqual(before + 81);
  const dragged = await sidebarWidth(page);

  // 2. A reload keeps the width.
  await page.reload();
  await expect(sidebarSplitter(page)).toBeVisible({ timeout: WAIT });
  await expect.poll(() => sidebarWidth(page), { message: 'the sidebar after a reload' }).toBeGreaterThanOrEqual(dragged - 1);
  expect(await sidebarWidth(page)).toBeLessThanOrEqual(dragged + 1);

  // 3. The keyboard: Enter resets to 244 (--sidebar-w), ArrowRight moves by 1rem (16 px), Home goes to the minimum, End to the maximum.
  const keys = sidebarSplitter(page);
  await keys.focus();
  await page.keyboard.press('Enter');
  await expect.poll(() => sidebarWidth(page), { message: 'Enter resets to the default' }).toBeCloseTo(244, 0);
  await page.keyboard.press('ArrowRight');
  await page.keyboard.press('ArrowRight');
  await expect.poll(() => sidebarWidth(page), { message: 'two ArrowRights' }).toBeCloseTo(276, 0);
  await page.keyboard.press('Home');
  await expect.poll(() => sidebarWidth(page), { message: 'Home' }).toBeCloseTo(192, 0);
  await page.keyboard.press('End');
  const max = await keys.getAttribute('aria-valuemax');
  await expect(keys).toHaveAttribute('aria-valuenow', max ?? '');
  await page.keyboard.press('Enter');
  await expect.poll(() => sidebarWidth(page), { message: 'Enter again' }).toBeCloseTo(244, 0);

  // 4. With the members aside open at 1280 px the aside has a splitter; at 900 px it is an overlay and has none.
  await expect(asideSplitter(page)).toBeVisible();
  await page.setViewportSize({ width: 900, height: 800 });
  await expect(asideSplitter(page)).toHaveCount(0);
  await expect(sidebarSplitter(page)).toBeVisible();

  // 4b. FX-misc round 2 (I2): at a normal desktop width the header stays one row (the wrap is for a main under 28rem only).
  await expect(sidebarSplitter(page)).toBeVisible();
  expect(await page.locator('.d-channel-header').evaluate(el => el.getBoundingClientRect().height), 'the header at 900 px').toBeLessThanOrEqual(44.5) /* one --bar-h */;

  // 5. FX-misc m6, as web3.9 UI-3 designed it: with the sidebar at its widest on an 800 px window main is 24rem (384 px). The header
  // stays one 48 px bar (it no longer wraps); below 36rem of header width the foldable actions give way to the "more" menu, and every
  // action still shown sits whole inside main.
  await page.setViewportSize({ width: 800, height: 800 });
  await sidebarSplitter(page).focus();
  await page.keyboard.press('End');
  const mainBox = async () => page.getByRole('main').evaluate(el => { const r = el.getBoundingClientRect(); return { left: r.left, right: r.right, width: r.width }; });
  await expect.poll(async () => Math.round((await mainBox()).width), { message: 'main at the 24rem floor' }).toBe(384);
  const header = page.locator('.d-channel-header');
  expect(await header.evaluate(el => el.getBoundingClientRect().height), 'the header stays one bar at 24rem').toBeLessThanOrEqual(44.5) /* one --bar-h */;
  // A folded action is display:none (no box); what is left, the "more" menu among it, is measured.
  const shown = await header.locator('.d-channel-header__actions button').evaluateAll(els => els.map(el => { const r = el.getBoundingClientRect(); return { name: el.getAttribute('aria-label') ?? el.textContent ?? '', left: r.left, right: r.right, width: r.width }; }));
  const cells = shown.filter(c => c.width > 0);
  expect(cells.length, 'the header actions still shown').toBeGreaterThanOrEqual(2);
  const bounds = await mainBox();
  for (const c of cells) {
    expect(c.width, `${c.name} keeps its width`).toBeGreaterThan(24);
    expect(c.left, `${c.name} starts inside main`).toBeGreaterThanOrEqual(bounds.left - 1);
    expect(c.right, `${c.name} ends inside main`).toBeLessThanOrEqual(bounds.right + 1);
  }
  expect(await header.evaluate(el => el.scrollWidth <= el.clientWidth + 1), 'the header does not scroll sideways').toBe(true);
  await expect(header.getByRole('heading', { level: 2 })).toBeVisible();
  const name = await header.getByRole('heading', { level: 2 }).evaluate(el => el.getBoundingClientRect().width);
  expect(name, 'the channel name keeps some width').toBeGreaterThan(40);
  await sidebarSplitter(page).focus();
  await page.keyboard.press('Enter');

  // 6. FX-misc (DOCS concern 2): after the window goes phone -> desktop in one session the sidebar is at its own width, not
  // stuck at the 192 px minimum, and a drag of its splitter widens it as before the flip.
  await page.setViewportSize({ width: 1280, height: 800 });
  await expect.poll(() => sidebarWidth(page), { message: 'the default width before the flip' }).toBeCloseTo(244, 0);
  await page.setViewportSize({ width: 390, height: 844 });
  await expect(sidebarSplitter(page)).toHaveCount(0);
  await page.setViewportSize({ width: 1280, height: 800 });
  await expect(sidebarSplitter(page)).toBeVisible({ timeout: WAIT });
  await expect.poll(() => sidebarWidth(page), { message: 'the sidebar after phone -> desktop' }).toBeCloseTo(244, 0);
  const flipped = await sidebarWidth(page);
  const flipBox = await sidebarSplitter(page).boundingBox();
  if (flipBox === null) throw new Error('the sidebar splitter has no box after the flip');
  const fx = flipBox.x + flipBox.width / 2;
  const fy = flipBox.y + flipBox.height / 2;
  await page.mouse.move(fx, fy);
  await page.mouse.down();
  await page.mouse.move(fx + 48, fy, { steps: 5 });
  await page.mouse.move(fx + 96, fy, { steps: 5 });
  await page.mouse.up();
  await expect.poll(() => sidebarWidth(page), { message: 'the sidebar after a drag following the flip' }).toBeGreaterThanOrEqual(flipped + 95);
  expect(await sidebarWidth(page)).toBeLessThanOrEqual(flipped + 97);
  await sidebarSplitter(page).focus();
  await page.keyboard.press('Enter');

  // 7. The status menu behind the user panel's avatar and name opens upward over the sidebar, whole and on screen, and takes a choice.
  const who = page.locator('.d-user-panel').getByRole('button', { name: new RegExp(`${copy('panel.status', { status: copy('presence.online') })}$`) });
  await who.click();
  const menu = page.getByRole('menu');
  await expect(menu.getByRole('menuitemradio')).toHaveCount(4);
  for (const item of await menu.getByRole('menuitemradio').all()) await expect(item).toBeInViewport({ ratio: 1 });
  await menu.getByRole('menuitemradio', { name: copy('presence.idle'), exact: true }).click();
  await expect(page.locator('.d-user-panel').getByRole('button', { name: new RegExp(`${copy('panel.status', { status: copy('presence.idle') })}$`) })).toBeFocused({ timeout: WAIT });
  await expect(page.locator('.d-user-panel .d-avatar__presence')).toHaveAttribute('data-presence', 'idle');

  // 8. Accessible.
  await expectAccessible(page, 'the panes with their splitters');
});
