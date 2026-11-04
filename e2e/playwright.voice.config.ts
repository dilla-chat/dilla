import { defineConfig, devices } from '@playwright/test';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { FIREFOX_MEDIA_PREFS, chromiumMediaArgs, writeFixtures } from './media/support/fixtures.ts';
import type { HostConfig } from './web/support/host.ts';
import { e2ePort } from './web/support/ports.ts';

const here = dirname(fileURLToPath(import.meta.url));
// VOICE-E2E (web-3 task 10, L-E2E-31): packages/web/dist served by dilla-testhost on 8481/8482, with its in-process
// LiveKit on 7884 (signalling, reached through the host's /rtc proxy) and 7886 (ICE/UDP). Two browsers of one engine
// talk, ring and play through it, beside a native peer that can poison a call group.
// Task-10 fix review N3: the rooms' departure timeout is 90 s here (20 s in production), so leave-reload's second test
// (a reload alone in the room, then a rejoin that waits for the peer's commit) is never raced by the room's own end on a
// slow runner. No voice test relies on the departure timeout to end a call.
const dillaHost: HostConfig = { port: e2ePort(8481), control: e2ePort(8482), webRoot: 'packages/web/dist', sfu: { port: e2ePort(7884), udpPort: e2ePort(7886), departureTimeout: 90 } };
// Written at config load, before any browser launches: the Chromium switches name the files (a 1 kHz tone, so the
// microphone is never silent and voice activity always sends).
const fixtures = writeFixtures(join(tmpdir(), 'dilla-media-fixtures'));

export default defineConfig({
  testDir: resolve(here, 'voice'),
  globalSetup: resolve(here, 'web', 'support', 'host.ts'),
  // DILLA_E2E_OUTPUT_DIR: a run of its own (scripts/ci-local.mjs) keeps its traces and host logs apart from a parallel run's.
  outputDir: process.env.DILLA_E2E_OUTPUT_DIR ?? resolve(here, 'test-results'),
  metadata: { dillaHost },
  fullyParallel: false,
  // One worker, in file order: the instance clock is frozen (the test host's fake clock), and only that keeps the room
  // sweep and the 30 s admission window from cutting a native peer that holds a call-group leaf outside the room
  // (blocked.spec). ring.spec's last test moves the clock for good; voice.ts refuses a native peer's call join after
  // that in this worker process, so a new spec, a shard or a reorder fails by name (task-10 review M6).
  workers: 1,
  retries: 0,
  maxFailures: process.env.CI === 'true' ? 1 : 0,
  forbidOnly: !!process.env.CI,
  timeout: 240_000,
  expect: { timeout: process.env.CI === 'true' ? 90_000 : 30_000 },
  reporter: [['list']],
  // An action waits as long as one state may (WAIT), so a missing control fails the test with time left for `finally`.
  use: { baseURL: `http://127.0.0.1:${String(e2ePort(8481))}`, trace: 'retain-on-failure', actionTimeout: process.env.CI === 'true' ? 90_000 : 30_000 },
  projects: [
    {
      name: 'chromium-voice',
      use: {
        ...devices['Desktop Chrome'], channel: 'chromium',
        // Clipboard write: Privacy's `copy` (the safety-number comparison of blocked.spec's RV-3 case).
        permissions: ['camera', 'microphone', 'notifications', 'clipboard-read', 'clipboard-write'],
        launchOptions: { args: chromiumMediaArgs(fixtures) },
      },
    },
    {
      name: 'firefox-voice',
      // Firefox takes prefs, never `permissions` ("Unknown permission: camera"); 1 grants desktop notifications.
      use: {
        ...devices['Desktop Firefox'],
        launchOptions: { firefoxUserPrefs: { ...FIREFOX_MEDIA_PREFS, 'permissions.default.desktop-notification': 1 } },
      },
    },
  ],
});
