import { defineConfig, devices } from '@playwright/test';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import type { HostConfig } from './web/support/host.ts';
import { e2ePort } from './web/support/ports.ts';

const here = dirname(fileURLToPath(import.meta.url));
// From task 18 the test host serves the headless harness page (npm run build:harness -w @dilla/client-core).
const dillaHost: HostConfig = { port: e2ePort(8453), control: e2ePort(8454), webRoot: 'packages/client-core/harness-dist' };

// The client-core integration suite: the headless harness page against dilla-testhost on 8453/8454
// (production ACL) with a native web-driver peer. Every project runs on a persistent profile
// (e2e/web/support/persistent.ts), because the store lives in OPFS and IndexedDB.
export default defineConfig({
  testDir: resolve(here, 'web'),
  testMatch: /(^|\/)(support|core-worker|spike-joinall)\.spec\.ts$/,
  globalSetup: resolve(here, 'web', 'support', 'host.ts'),
  // DILLA_E2E_OUTPUT_DIR: a run of its own (scripts/ci-local.mjs) keeps its traces and host logs apart from a parallel run's.
  outputDir: process.env.DILLA_E2E_OUTPUT_DIR ?? resolve(here, 'test-results'),
  metadata: { dillaHost },
  fullyParallel: false,
  workers: 1,
  retries: 0,
  forbidOnly: !!process.env.CI,
  timeout: 180_000,
  expect: { timeout: process.env.CI === 'true' ? 90_000 : 30_000 },
  reporter: [['list']],
  use: { baseURL: `http://127.0.0.1:${String(e2ePort(8453))}`, trace: 'retain-on-failure' },
  projects: [
    { name: 'chromium-core', use: { ...devices['Desktop Chrome'], channel: 'chromium' } },
    { name: 'firefox-core', use: { ...devices['Desktop Firefox'] } },
  ],
});
