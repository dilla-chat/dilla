// web3.8 C4: a WebKit private window says why dilla cannot start there, from an empty host and through the UI only.
// WebKit's ephemeral contexts behave like Safari private browsing: navigator.storage.getDirectory() rejects with
// UnknownError (docs/spikes/2026-09-browser.md). The worker used to await that root before the persistence probe ran, so
// the page showed a bare E_INTERNAL; it now shows the unsupported splash (boot.unsupported.status and .detail).
// WebKit only: the chromium and firefox web projects have no testMatch, so they would run this file and are skipped.
// Its host (8515/8516) is reused after E2's serial run.
import { expect, test } from '@playwright/test';
import { copy } from './support/app';
import { startTestHost, type HostConfig, type StartedHost } from './support/host';
import { e2ePort } from './support/ports';

const HOST: HostConfig = { port: e2ePort(8515), control: e2ePort(8516), webRoot: 'packages/web/dist' };
const WAIT = process.env.CI === 'true' ? 90_000 : 30_000;

let host: StartedHost | null = null;

test.skip(({ browserName }) => browserName !== 'webkit', 'only WebKit private windows refuse the storage root this way');
test.beforeAll(async () => { host = await startTestHost(HOST); });
test.afterAll(async () => { await host?.stop(); });

test('an ephemeral WebKit context shows the unsupported screen, not E_INTERNAL', async ({ browser }) => {
  // browser.newContext() is ephemeral: no profile directory, as in a Safari private window.
  const context = await browser.newContext({ baseURL: `http://127.0.0.1:${String(HOST.port)}` });
  try {
    const page = await context.newPage();
    await page.goto('/');
    await expect(page.getByText(copy('boot.unsupported.status'), { exact: true })).toBeVisible({ timeout: WAIT });
    await expect(page.getByText(copy('boot.unsupported.detail'), { exact: true })).toBeVisible({ timeout: WAIT });
    await expect(page.getByText('E_INTERNAL')).toHaveCount(0);
  } finally { await context.close(); }
});
