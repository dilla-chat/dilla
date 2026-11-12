// Task 2 proof: the e2e media suite's own setup (e2e/media/support/testhost.ts builds and starts
// dilla-testhost with its in-process LiveKit on 7880 and the packages/media harness on 5179), and
// the media suite's Chromium switches. Run from the repository root:
//   npx playwright test -c experiments/gpui-voice-proof/browser/playwright.config.ts
import { defineConfig, devices } from '@playwright/test';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { chromiumMediaArgs, writeFixtures } from '../../../e2e/media/support/fixtures.ts';

const here = dirname(fileURLToPath(import.meta.url));
const fixtures = writeFixtures(join(tmpdir(), 'dilla-media-fixtures'));

export default defineConfig({
  globalSetup: resolve(here, '..', '..', '..', 'e2e', 'media', 'support', 'testhost.ts'),
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
