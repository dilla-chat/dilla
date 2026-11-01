// web3.8, INV: the invite landing page in Signal style. From an empty host, Ada starts "Midgard Crew", opens server settings,
// goes to Invites, makes a link there and reads its URL (never the sidebar's "invite link" action, which WS4 removes).
// The page itself is the instance's static HTML under `default-src 'none'`: the spec asserts what it says, that it has
// one same-origin link, that its headers are the unchanged four, and shoots it in both colour schemes at both sizes.
import { expect, type Page } from '@playwright/test';
import { mkdirSync } from 'node:fs';
import { join } from 'node:path';
import { addServerVia, TEST_TIMEOUT, WAIT, copy, signUp, test } from './support/app';
import { REPO_ROOT, startTestHost, type HostConfig, type StartedHost } from './support/host';
import { arr, decode } from '../../packages/client-core/src/cbor/index';

const EMPTY: HostConfig = { port: 8517, control: 8518, webRoot: 'packages/web/dist' };
const SERVER = 'Midgard Crew';
const SHOTS = join(REPO_ROOT, 'target', 'ui-screens', 'web3.9');

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

/** Ada starts a server, opens server settings → Invites, makes a link and returns its URL. */
async function makeLink(page: Page, h: StartedHost): Promise<string> {
  await signUp(page, h.invite.split(',')[0], instance, 'Ada', h.control);
  await addServerVia(page, 'create');
  const start = page.getByRole('dialog', { name: copy('create.server.title'), exact: true });
  await start.getByRole('textbox', { name: copy('create.server.name'), exact: true }).fill(SERVER);
  await page.keyboard.press('Enter');
  await expect(start).toBeHidden({ timeout: WAIT });
  await expect(page).toHaveURL(/\/c\/[0-9a-f]{32}\/[0-9a-f]{32}$/, { timeout: WAIT });

  await page.getByRole('button', { name: copy('admin.open'), exact: true }).click().then(() => page.getByRole('menuitem', { name: copy('signalSettings.menu.serverSettings'), exact: true }).click());
  const settings = page.getByRole('dialog', { name: copy('admin.title'), exact: true });
  const nav = settings.getByRole('navigation', { name: copy('admin.nav'), exact: true });
  await expect(nav).toBeVisible({ timeout: WAIT });
  await nav.getByRole('button', { name: copy('admin.nav.invites'), exact: true }).click();
  await settings.getByRole('button', { name: copy('serverSettings.invites.make'), exact: true }).click();
  const link = settings.getByRole('textbox', { name: copy('serverSettings.invites.link'), exact: true });
  await expect(link).toBeVisible({ timeout: WAIT });
  const url = await link.inputValue();
  expect(url).toMatch(/^https?:\/\/[^/\s]+\/i\/[^/\s]+$/);
  return url;
}

test('the invite page names the community, keeps its limits and is shot in both schemes', async ({ page, browser }) => {
  const h = host;
  if (h === null) throw new Error('the empty test host did not start');
  const url = await makeLink(page, h);
  const path = new URL(url).pathname;
  const code = path.slice('/i/'.length);

  // A context of its own: the invite page is static HTML under `default-src 'none'`, and Firefox reports its own
  // /favicon.ico request as a CSP refusal, which is that CSP working and not the web client's guard to judge.
  const context = await browser.newContext({ baseURL: h.url, viewport: { width: 1280, height: 800 } });
  try {
    const view = await context.newPage();
    const response = await view.goto(path);
    expect(response?.status()).toBe(200);
    const headers = response?.headers() ?? {};
    expect(headers['cache-control']).toBe('no-store');
    expect(headers['referrer-policy']).toBe('no-referrer');
    expect(headers['x-content-type-options']).toBe('nosniff');
    expect(headers['content-security-policy']).toBe("default-src 'none'; style-src 'unsafe-inline'; font-src 'self'");
    // The same-origin JetBrains Mono under font-src 'self' actually loaded (a moved asset would fall back quietly).
    await view.evaluate(() => document.fonts.ready);
    expect(await view.evaluate(() => document.fonts.check('13.5px "JetBrains Mono"'))).toBe(true);
    expect(await view.evaluate(() => Array.from(document.fonts).some(f => f.family.includes('JetBrains Mono') && f.status === 'loaded'))).toBe(true);

    await expect(view.getByRole('heading', { level: 1, name: `You have been invited to ${SERVER}` })).toBeVisible();
    await expect(view.getByText(`on ${instance}`, { exact: true })).toBeVisible();
    await expect(view.locator('code.code')).toHaveText(code);
    await expect(view.locator('meta[property="og:title"]')).toHaveAttribute('content', `Invitation to ${SERVER} on ${instance}`);
    const links = view.locator('a[href]');
    await expect(links).toHaveCount(1);
    await expect(links).toHaveAttribute('href', `/welcome?invite=${code}`);
    await expect(links).toHaveText('Open dilla in this browser');
    await expect(view.locator('script, form, img, link')).toHaveCount(0);

    mkdirSync(SHOTS, { recursive: true });
    for (const [name, size] of [['desktop', { width: 1280, height: 800 }], ['phone', { width: 390, height: 844 }]] as const) {
      for (const colorScheme of ['dark', 'light'] as const) {
        await view.setViewportSize(size);
        await view.emulateMedia({ colorScheme });
        const open = await view.locator('a.open').boundingBox();
        expect(open?.height).toBe(name === 'phone' ? 44 : 36);
        await view.screenshot({ path: join(SHOTS, `invite-${name}-${colorScheme}.png`) });
      }
    }
  } finally {
    await context.close();
  }
});
