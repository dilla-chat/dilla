import { defineConfig } from '@playwright/test';
import voice from './playwright.voice.config.ts';
import { relative, resolve } from 'node:path';

process.env.DILLA_ELECTRON_E2E = '1';
export default defineConfig({
 ...voice,
 metadata: { ...voice.metadata, dillaHost: { ...voice.metadata!.dillaHost,
  ...(process.env.DILLA_E2E_WEB_ROOT ? { webRoot: relative(resolve(import.meta.dirname, '..'), resolve(process.env.DILLA_E2E_WEB_ROOT)) } : {}),
 } },
 // Native minimum-size/call-dock acceptance replaces browser phone emulation.
 testIgnore: '**/phone-call.spec.ts',
 // Preserve the single worker and file order: the frozen clock cannot be sharded.
 projects: [{ name: 'electron-voice', use: { ...voice.projects![0].use, channel: undefined } }],
 reporter: [['list'], ['html', { open: 'never', outputFolder: 'desktop-voice-report' }]],
 outputDir: 'desktop-voice-results',
});
