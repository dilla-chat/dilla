import { defineConfig, devices } from '@playwright/test';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import type { HostConfig } from './web/support/host.ts';

const here = dirname(fileURLToPath(import.meta.url));
// The web app suite (task 25): packages/web/dist served by dilla-testhost on 8463/8464. DILLA_E2E_PORT and DILLA_E2E_PORT2 move the
// pair (a lane that shares its machine); web/support/app.ts reads the same two.
const hostPort = Number(process.env.DILLA_E2E_PORT ?? 8463);
const controlPort = Number(process.env.DILLA_E2E_PORT2 ?? 8464);
const dillaHost: HostConfig = { port: hostPort, control: controlPort, webRoot: 'packages/web/dist' };

export default defineConfig({
  testDir: resolve(here, 'web'),
  // A project's own testIgnore replaces this one, so a project that ignores more repeats these names.
  testIgnore: /(^|\/)(support|core-worker|spike-joinall)\.spec\.ts$/,
  globalSetup: resolve(here, 'web', 'support', 'host.ts'),
  // prebuildBotRuntime: the global setup builds dillabot and dilla-bot-core once, so no Bot test pays a cold build.
  metadata: { dillaHost, prebuildBotRuntime: true },
  fullyParallel: false,
  workers: 1,
  retries: 0,
  forbidOnly: !!process.env.CI,
  timeout: 180_000,
  expect: { timeout: process.env.CI === 'true' ? 90_000 : 30_000 },
  reporter: [['list']],
  use: { baseURL: `http://127.0.0.1:${String(hostPort)}`, trace: 'retain-on-failure' },
  projects: [
    { name: 'chromium-web', use: { ...devices['Desktop Chrome'], channel: 'chromium' } },
    // channel-list.spec starts an SFU and grants the `microphone` permission to a fake-microphone context; both are
    // Chromium-only (Firefox rejects "Unknown permission: microphone"), so Firefox does not load it.
    { name: 'firefox-web', testIgnore: /(^|\/)(support|core-worker|spike-joinall|channel-list)\.spec\.ts$/, use: { ...devices['Desktop Firefox'] } },
    // Keep the WebKit set explicit: these specs use the shipping web client without enabling
    // microphone-only Chromium fixtures, and slash commands are part of the cross-engine contract.
    { name: 'webkit-web', testMatch: /(^|\/)(signup|reload|phone|signin-order|private-window|layout|search-settings|reload-mid-sync|slash-commands|realtime-transport)\.spec\.ts$/, use: { ...devices['Desktop Safari'] } },
  ],
});
