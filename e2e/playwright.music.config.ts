import { defineConfig, devices } from '@playwright/test';
import { chromiumMediaArgs, writeFixtures } from './media/support/fixtures.ts';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { dirname } from 'node:path';
import { FIREFOX_MEDIA_PREFS } from './media/support/fixtures.ts';
import type { HostConfig } from './web/support/host.ts';

const here = dirname(fileURLToPath(import.meta.url));
const fixtures = writeFixtures(join(tmpdir(), 'dilla-music-fixtures'));
const port = Number(process.env.DILLA_MUSIC_PORT ?? 8721);
const xdgRuntimeDir = process.env.XDG_RUNTIME_DIR ?? `/run/user/${process.getuid?.() ?? 1000}`;
const pulseServer = process.env.PULSE_SERVER ?? `unix:${xdgRuntimeDir}/pulse/native`;
const dillaHost: HostConfig = {
  port, control: port + 1, webRoot: 'packages/web/dist', botRuntime: true, advanceClockWithWall: true,
  sfu: { port: port + 3, udpPort: port + 5, departureTimeout: 90 },
};

export default defineConfig({
  testDir: resolve(here, 'music'),
  globalSetup: resolve(here, 'web', 'support', 'host.ts'),
  metadata: { dillaHost, prebuildBotRuntime: true }, // see playwright.web.config.ts
  fullyParallel: false,
  workers: 1,
  retries: 0,
  forbidOnly: !!process.env.CI,
  timeout: 360_000,
  expect: { timeout: process.env.CI === 'true' ? 90_000 : 30_000 },
  reporter: [['list']],
  use: {
    baseURL: `http://127.0.0.1:${port}`, trace: 'retain-on-failure', actionTimeout: process.env.CI === 'true' ? 90_000 : 30_000,
  },
  projects: [{
    name: 'chromium-music',
    use: {
      ...devices['Desktop Chrome'], channel: 'chromium',
      permissions: ['camera', 'microphone', 'notifications'],
      launchOptions: { args: chromiumMediaArgs(fixtures) },
    },
  }, {
    name: 'firefox-music',
    use: {
      ...devices['Desktop Firefox'],
      launchOptions: {
        firefoxUserPrefs: { ...FIREFOX_MEDIA_PREFS, 'permissions.default.desktop-notification': 1 },
        env: { ...process.env, XDG_RUNTIME_DIR: xdgRuntimeDir, PULSE_SERVER: pulseServer },
      },
    },
  }],
});
