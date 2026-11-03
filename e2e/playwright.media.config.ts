import { defineConfig, devices } from '@playwright/test';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { FIREFOX_MEDIA_PREFS, chromiumMediaArgs, writeFixtures } from './media/support/fixtures.ts';

const here = dirname(fileURLToPath(import.meta.url));
// Written at config load, before any browser launches: the Chromium switches name the files.
const fixtures = writeFixtures(join(tmpdir(), 'dilla-media-fixtures'));

const chromium = {
  ...devices['Desktop Chrome'],
  channel: 'chromium',
  permissions: ['camera', 'microphone'],
  launchOptions: { args: chromiumMediaArgs(fixtures) },
};
const firefox = { ...devices['Desktop Firefox'], launchOptions: { firefoxUserPrefs: FIREFOX_MEDIA_PREFS } };
const chromiumMedia = { name: 'chromium-media', testDir: resolve(here, 'media'), use: chromium };

// The media suite: a dilla-testhost with an in-process LiveKit and the packages/media harness,
// both started by globalSetup. The persistence matrix keeps playwright.config.ts unchanged.
export default defineConfig({
  globalSetup: resolve(here, 'media', 'support', 'testhost.ts'),
  fullyParallel: false,
  workers: 1,
  retries: 0,
  // DILLA_E2E_FAIL_FAST=0 (desktop.yml stabilisation mode) reports every failure of a CI run instead of
  // stopping at the first; CI otherwise stops at one failure.
  maxFailures: process.env.CI === 'true' && process.env.DILLA_E2E_FAIL_FAST !== '0' ? 1 : 0,
  forbidOnly: !!process.env.CI,
  timeout: 120_000,
  reporter: [['list']],
  use: { baseURL: 'http://127.0.0.1:5179', trace: 'retain-on-failure' },
  projects: [
    chromiumMedia,
    { name: 'firefox-media', testDir: resolve(here, 'media'), use: firefox },
    // Spike code: committed with its result document, never run in CI (plan MD-17).
    { name: 'spike-chromium', testDir: resolve(here, 'spikes'), use: chromium },
    { name: 'spike-firefox', testDir: resolve(here, 'spikes'), use: firefox },
    // Run only harness.spec.ts under the default chromium-headless-shell build.
    { ...chromiumMedia, name: 'spike-chromium-shell', testMatch: /harness\.spec\.ts$/, use: { ...chromium, channel: undefined } },
  ],
});
