// Task 11: the desktop's `dilla_media::Call` (examples/call-peer.rs) and the web client's media stack (the
// packages/media harness page: the real DillaE2EEManager, media worker and dilla-core-wasm) in one LiveKit room,
// through the e2e media suite's own setup (e2e/media/support/testhost.ts: dilla-testhost with its in-process
// LiveKit, the harness on 5179). Run from the repository root:
//   npx playwright test -c clients/desktop/dilla-media/browser/playwright.config.ts
// or through `cargo test -p dilla-media --test call desktop_and_web_in_one_call -- --ignored`.
import { defineConfig, devices } from '@playwright/test';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { chromiumMediaArgs, writeFixtures } from '../../../../e2e/media/support/fixtures.ts';

const here = dirname(fileURLToPath(import.meta.url));
const fixtures = writeFixtures(join(tmpdir(), 'dilla-media-fixtures'));

export default defineConfig({
  globalSetup: resolve(here, '..', '..', '..', '..', 'e2e', 'media', 'support', 'testhost.ts'),
  testDir: here,
  fullyParallel: false,
  workers: 1,
  retries: 0,
  timeout: 180_000,
  reporter: [['list']],
  use: { trace: 'retain-on-failure' },
  projects: [
    {
      name: 'chromium-media',
      use: {
        ...devices['Desktop Chrome'],
        channel: 'chromium',
        permissions: ['camera', 'microphone'],
        launchOptions: { args: chromiumMediaArgs(fixtures) },
      },
    },
  ],
});
