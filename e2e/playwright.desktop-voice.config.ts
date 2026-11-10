import { defineConfig } from '@playwright/test';
import voice from './playwright.voice.config.ts';
import { tmpdir } from 'node:os';
import { join, relative, resolve } from 'node:path';
import { writeMusicFixtures } from './media/support/fixtures.ts';

process.env.DILLA_ELECTRON_E2E = '1';
// Bot v2 Task 19: the Bot's fixture stream (a 30 s and a 5 s Ogg Opus file, rendered by ffmpeg) for
// voice/bot-music.spec.ts, which starts a wall-clock host of its own with the supervised Bot (botRuntime: true) and
// copies these into the Bot's state directory. The supervised Bot is Linux only, so only Linux renders them (and only
// the Linux desktop-voice job installs ffmpeg).
if (process.platform === 'linux') {
  process.env.DILLA_BOT_MUSIC_FIXTURES = writeMusicFixtures(join(tmpdir(), 'dilla-bot-music-fixtures')).dir;
}
export default defineConfig({
 ...voice,
 metadata: { ...voice.metadata, dillaHost: { ...voice.metadata!.dillaHost,
  ...(process.env.DILLA_E2E_WEB_ROOT ? { webRoot: relative(resolve(import.meta.dirname, '..'), resolve(process.env.DILLA_E2E_WEB_ROOT)) } : {}),
 } },
 // Native minimum-size/call-dock acceptance replaces browser phone emulation.
 // The supervised Bot is Linux only (Bot v2 Task 19: bot-music.spec.ts).
 testIgnore: ['**/phone-call.spec.ts', ...(process.platform === 'linux' ? [] : ['**/bot-music.spec.ts'])],
 // Preserve the single worker and file order: the frozen clock cannot be sharded.
 projects: [{ name: 'electron-voice', use: { ...voice.projects![0].use, channel: undefined } }],
 reporter: [['list'], ['html', { open: 'never', outputFolder: 'desktop-voice-report' }]],
 outputDir: 'desktop-voice-results',
});
