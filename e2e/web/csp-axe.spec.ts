import { expect } from '@playwright/test';
import { addServerVia,
  COPY, HOST, TEST_TIMEOUT, WAIT, copy, escapeRegExp, expectAccessible, expectHeading, expectShell, instanceInvite,
  joinCommunity, messageRow, nextButton, openChannel, railNav, readManifest, readRecoveryKey, splash, submitButton,
  tag, test, completeFactorStep, newAccount,
} from './support/app';

test.describe.configure({ timeout: TEST_TIMEOUT });

const CSP = `default-src 'self'; script-src 'self' 'wasm-unsafe-eval'; style-src 'self'; img-src 'self' blob:; font-src 'self'; connect-src 'self' ws://${HOST} wss://${HOST}; worker-src 'self'; media-src blob:; object-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'`;
const COMMON = {
  'x-content-type-options': 'nosniff',
  'referrer-policy': 'no-referrer',
  'x-frame-options': 'DENY',
  'permissions-policy': 'camera=(self), microphone=(self), display-capture=(self), geolocation=()',
};
const NEVER = ['cross-origin-opener-policy', 'cross-origin-embedder-policy', 'access-control-allow-origin', 'strict-transport-security'];
const IMMUTABLE = 'public, max-age=31536000, immutable';

test('the host serves the built client with the headers, types, cache rules and policy of the embed', async ({ request }) => {
  const manifest = readManifest();
  expect(manifest.v).toBe(1);
  const index = manifest.files.find((f) => f.path === 'index.html');
  if (!index) throw new Error('the manifest lists no index.html');

  const root = await request.get('/');
  expect(root.status()).toBe(200);
  const rootHeaders = root.headers();
  expect(rootHeaders['content-type']).toBe('text/html; charset=utf-8');
  expect(rootHeaders['content-security-policy']).toBe(CSP);
  expect(rootHeaders['cache-control']).toBe('no-cache');
  expect(rootHeaders['etag']).toBe(`"${index.sha256}"`);
  expect(rootHeaders).toMatchObject(COMMON);
  for (const name of NEVER) expect(rootHeaders[name], name).toBeUndefined();
  const rootBody = await root.body();
  expect(rootBody.length).toBe(index.size);

  // The client router owns extension-less paths.
  const deep = await request.get(`/c/${'ab'.repeat(16)}/${'cd'.repeat(16)}`);
  expect(deep.status()).toBe(200);
  expect(deep.headers()['content-security-policy']).toBe(CSP);
  expect(await deep.body()).toEqual(rootBody);

  // Hashed assets are immutable, typed by extension, carry their ETag and the same CSP as the page.
  const js = manifest.files.find((f) => f.path.startsWith('assets/') && f.path.endsWith('.js'));
  if (!js) throw new Error('the manifest lists no assets/*.js');
  // web-3 task 8: one core wasm (the core and media workers share it) and RNNoise's, as check-entry-chunk.mjs and
  // internal/web/webdist_test.go count them; each is served as application/wasm.
  const wasm = manifest.files.filter((f) => f.path.endsWith('.wasm'));
  expect(wasm.map((f) => f.path).sort()).toEqual([expect.stringMatching(/^assets\/dilla_core_wasm_bg-[^/]+\.wasm$/),
    expect.stringMatching(/^assets\/rnnoise-[^/]+\.wasm$/)]);
  for (const [file, type] of [[js, 'text/javascript; charset=utf-8'], ...wasm.map((w) => [w, 'application/wasm'] as const)] as const) {
    const res = await request.get(`/${file.path}`);
    expect(res.status(), file.path).toBe(200);
    const h = res.headers();
    expect(h['content-type'], file.path).toBe(type);
    expect(h['cache-control'], file.path).toBe(IMMUTABLE);
    expect(h['etag'], file.path).toBe(`"${file.sha256}"`);
    expect(h['content-security-policy'], file.path).toBe(CSP);
    expect(h).toMatchObject(COMMON);
    expect((await res.body()).length, file.path).toBe(file.size);
  }
  const revalidated = await request.get(`/${js.path}`, { headers: { 'if-none-match': `"${js.sha256}"` } });
  expect(revalidated.status()).toBe(304);
  expect((await revalidated.body()).length).toBe(0);

  // The catch-all never answers for the API, and unknown files are 404.
  const api = await request.get('/v1/nope');
  expect(api.status()).toBe(404);
  expect(api.headers()['content-security-policy']).toBeUndefined();
  expect((await request.get('/assets/missing-00000000.js')).status()).toBe(404);
  const post = await request.post('/');
  expect(post.status()).toBe(405);
  expect(post.headers()['allow']).toBe('GET, HEAD');
});

test('every state of the slice passes axe with target-size, in all three themes, without a CSP violation', async ({ page, context, peer, guard, instanceName }) => {
  const setup = await peer.setup();
  await peer.register();
  const channel = peer.channelName;

  await page.goto(`/welcome?invite=${encodeURIComponent(instanceInvite())}`);
  await expectHeading(page, COPY.connectTitle, { instance: instanceName });
  await expectAccessible(page, 'onboarding: connect');
  await nextButton(page).click();

  // One field in error, and only one: the display name and the password are valid when the step is
  // submitted, so the single alert is the username's.
  await expectHeading(page, COPY.identityTitle);
  await expectAccessible(page, 'onboarding: identity');
  await page.getByLabel(copy(COPY.displayLabel), { exact: true }).fill('axe tester');
  await page.getByLabel(copy(COPY.passwordLabel), { exact: true }).fill('e2e-password-1234');
  await page.getByLabel(copy(COPY.usernameLabel), { exact: true }).fill('A');
  await nextButton(page).click();
  await expect(page.getByRole('alert')).toHaveCount(1, { timeout: WAIT });
  await expect(page.getByRole('alert')).toBeVisible();
  await expectAccessible(page, 'onboarding: identity with a field error');
  await page.getByLabel(copy(COPY.usernameLabel), { exact: true }).fill(`a${setup.user_id.slice(0, 8)}`);
  await nextButton(page).click();

  await expectHeading(page, COPY.keysTitle);
  expect(await readRecoveryKey(page)).toHaveLength(13);
  await expectAccessible(page, 'onboarding: keys');
  await page.getByRole('checkbox', { name: copy(COPY.keysAcknowledge), exact: true }).check();
  await nextButton(page).click();
  await expectHeading(page, COPY.deviceTitle);
  await expectAccessible(page, 'onboarding: what this browser keeps');
  await submitButton(page).click();
  await completeFactorStep(page, newAccount(), (state) => expectAccessible(page, `onboarding: second factor ${state}`));
  await expectHeading(page, COPY.doneTitle);
  await expectAccessible(page, 'onboarding: done');
  await page.getByRole('button', { name: copy(COPY.finish), exact: true }).click();

  await expectShell(page);
  await expectAccessible(page, 'shell without a community');

  // The core worker's script carries the page's policy (ruling 31): a dedicated worker takes its policy
  // from its own script response, and every secret of the device lives in that worker.
  await expect.poll(() => page.workers().length, { timeout: WAIT }).toBeGreaterThan(0);
  const workerUrl = page.workers()[0].url();
  expect(workerUrl).toMatch(new RegExp(`^${escapeRegExp(`http://${HOST}`)}/assets/[^/?#]+\\.js$`));
  const workerScript = await context.request.get(workerUrl);
  expect(workerScript.status(), workerUrl).toBe(200);
  expect(workerScript.headers()['content-type'], workerUrl).toBe('text/javascript; charset=utf-8');
  expect(workerScript.headers()['content-security-policy'], workerUrl).toBe(CSP);

  await addServerVia(page, 'join');
  await expect(page.getByRole('dialog', { name: copy(COPY.joinTitle), exact: true })).toBeVisible({ timeout: WAIT });
  await expectAccessible(page, 'join dialog');
  await page.keyboard.press('Escape');
  await joinCommunity(page, peer, setup);
  await openChannel(page, peer.communityName, channel);
  await peer.waitForDevices(2);
  const body = `for axe ${tag()}`;
  await peer.send(body);
  await expect(messageRow(page, channel, body)).toHaveAttribute('data-state', 'ok', { timeout: WAIT });
  await expectAccessible(page, 'conversation');

  // The three themes, set where the app sets them (on <html>, L-UI DOM contract); --bg proves each one took effect.
  const backgrounds = new Set<string>();
  for (const theme of ['signal', 'light', 'high-contrast'] as const) {
    await page.evaluate((name) => { document.documentElement.dataset.theme = name; }, theme);
    backgrounds.add(await page.evaluate(() => getComputedStyle(document.documentElement).getPropertyValue('--bg').trim()));
    await expectAccessible(page, `conversation in the ${theme} theme`);
  }
  expect(backgrounds.size).toBe(3);

  const second = await context.newPage();
  await second.goto('/');
  await expect(splash(second, COPY.otherTab)).toBeVisible({ timeout: WAIT });
  await expectAccessible(second, 'other-tab');
  await second.close();

  await guard.collectWorkers();
  expect(guard.cspViolations).toEqual([]);
});
