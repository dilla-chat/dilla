import { readFileSync, readdirSync } from 'node:fs';
import { join } from 'node:path';
import { REPO_ROOT } from './support/host';
import { PROFILE_ROOT, expect, test } from './support/persistent';
import { WebDriver, testHostUrl, testkitEnv } from './support/driver';
import { injectAxe, runAxe } from './support/axe';
import { totp } from './support/totp';
import { e2ePort } from './support/ports';

const HEX32 = /^[0-9a-f]{32}$/;
const escapeRegExp = (text: string): string => text.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
const WAIT = process.env.CI === 'true' ? 90_000 : 30_000;
// L-HTTP-10, exactly, with HOST = 127.0.0.1:8453.
const CSP =
  "default-src 'self'; script-src 'self' 'wasm-unsafe-eval'; style-src 'self'; img-src 'self' blob:; font-src 'self'; " +
  `connect-src 'self' ws://127.0.0.1:${String(e2ePort(8453))} wss://127.0.0.1:${String(e2ePort(8453))}; worker-src 'self'; media-src blob:; ` +
  "object-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'";
// The invite landing's own policy (internal/api/invites.go:107), set on every answer of GET /i/{code}.
const LANDING_CSP = "default-src 'none'; style-src 'unsafe-inline'; font-src 'self'";

test('the test host serves the client root same-origin, with its headers, to a persistent profile', async ({ page, profileDir }) => {
  expect(profileDir).toMatch(new RegExp(`^${escapeRegExp(PROFILE_ROOT)}/(chromium|firefox)-core-\\d+$`));
  const response = await page.goto('/');
  // A persistent context: the browser wrote its profile into the directory the fixture created empty.
  expect(readdirSync(profileDir).length).toBeGreaterThan(0);
  expect(response?.status()).toBe(200);
  const headers = response!.headers();
  expect(headers['content-security-policy']).toBe(CSP);
  expect(headers['content-type']).toBe('text/html; charset=utf-8');
  expect(headers['cache-control']).toBe('no-cache');
  expect(headers['x-content-type-options']).toBe('nosniff');
  expect(headers['referrer-policy']).toBe('no-referrer');
  expect(headers['x-frame-options']).toBe('DENY');
  expect(headers['permissions-policy']).toBe('camera=(self), microphone=(self), display-capture=(self), geolocation=()');
  expect(headers['cross-origin-opener-policy']).toBeUndefined();
  expect(headers['cross-origin-embedder-policy']).toBeUndefined();
  expect(headers['access-control-allow-origin']).toBeUndefined();
  await expect(page.locator('meta[name="dilla-harness"]')).toHaveAttribute('content', 'core-worker', { timeout: WAIT });
  await expect(page.locator('#status')).toHaveText('ready', { timeout: WAIT });
});

test('axe evaluates under the served CSP without a policy violation', async ({ page }) => {
  await page.goto('/');
  await page.evaluate(() => {
    const w = window as Window & { cspViolations?: string[] };
    w.cspViolations = [];
    document.addEventListener('securitypolicyviolation', (e) => w.cspViolations!.push(e.violatedDirective));
  });
  await injectAxe(page);
  expect(await page.evaluate(() => (window as Window & { axe?: { version: string } }).axe?.version)).toBe('4.13.0');
  const findings = await runAxe(page);
  for (const f of findings) expect(['serious', 'critical']).toContain(f.impact);
  await expect.poll(() => page.evaluate(() => (window as Window & { cspViolations?: string[] }).cspViolations), { timeout: WAIT }).toEqual([]);
});

test('the native peer enrols and builds its community through the public routes', async ({ request }) => {
  const peer = await WebDriver.start(testHostUrl(), testkitEnv());
  try {
    const s = await peer.setup({ community: 'support community', channel: 'general' });
    expect(s.username).toBe(`peer${(peer.seed & 0xff_ffff).toString(16).padStart(6, '0')}`);
    expect(s.display).toBe(s.username);
    for (const key of ['user_id', 'device_id', 'community_id', 'channel_id'] as const) expect(s[key]).toMatch(HEX32);
    expect(s.invite_code.length).toBeGreaterThan(0);
    const landing = await request.get(`/i/${s.invite_code}`, { headers: { accept: 'application/cbor' } });
    expect(landing.status()).toBe(200);
    expect(landing.headers()['content-type']).toBe('application/cbor');
    expect(landing.headers()['content-security-policy']).toBe(LANDING_CSP);
    const registered = await peer.register();
    expect(registered.group_id).toMatch(HEX32);
    expect(registered.epoch).toBe(0);
    expect((await peer.members()).devices).toEqual([s.device_id]);
    expect(await peer.sync()).toEqual({ epoch: 0, members: 1, received: [] });
  } finally {
    await peer.close();
  }
});

test('the harness build is served by its manifest: the wasm as application/wasm, immutable, and unknown paths fall back to index.html', async ({ request }) => {
  const manifest = JSON.parse(
    readFileSync(join(REPO_ROOT, 'packages', 'client-core', 'harness-dist', 'dilla-manifest.json'), 'utf8'),
  ) as { v: number; files: { path: string; sha256: string; size: number }[] };
  expect(manifest.v).toBe(1);
  const wasm = manifest.files.filter((f) => f.path.endsWith('.wasm'));
  expect(wasm).toHaveLength(1);
  expect(wasm[0]!.path).toMatch(/^assets\//);
  const response = await request.get(`/${wasm[0]!.path}`);
  expect(response.status()).toBe(200);
  expect(response.headers()['content-type']).toBe('application/wasm');
  expect(response.headers()['cache-control']).toBe('public, max-age=31536000, immutable');
  expect(response.headers()['etag']).toBe(`"${wasm[0]!.sha256}"`);
  // The policy is on every file the web handler serves, the worker's script and the wasm included (L-HTTP-10, ruling 31).
  expect(response.headers()['content-security-policy']).toBe(CSP);
  const workerScript = manifest.files.filter((f) => f.path.startsWith('assets/') && f.path.endsWith('.js'));
  expect(workerScript.length).toBeGreaterThan(0);
  for (const f of workerScript) {
    expect((await request.get(`/${f.path}`)).headers()['content-security-policy']).toBe(CSP);
  }
  expect((await response.body()).length).toBe(wasm[0]!.size);
  const fallback = await request.get('/c/some/client/route');
  expect(fallback.status()).toBe(200);
  expect(await fallback.text()).toContain('name="dilla-harness"');
  expect((await request.get('/dilla-manifest.json')).status()).toBe(404);
});

// RFC 6238 appendix B, SHA-1, secret "12345678901234567890", 8 digits.
test('totp() reproduces the RFC 6238 SHA-1 vectors', () => {
  const secret = 'GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ';
  expect(totp(secret, 59, 8)).toBe('94287082');
  expect(totp(secret, 1111111109, 8)).toBe('07081804');
  expect(totp(secret, 1234567890, 8)).toBe('89005924');
  expect(totp(secret, 59)).toBe('287082');
});
