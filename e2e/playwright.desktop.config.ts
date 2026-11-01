import { defineConfig } from '@playwright/test';
import web from './playwright.web.config.ts';
// The shared client suite uses actual Electron processes for persistent and
// secondary devices. Existing browser projects remain until parity passes.
process.env.DILLA_ELECTRON_E2E = '1';
export default defineConfig({
 ...web,
 // The production Bot service is Linux-only. Its full integration cases run in
 // the Linux Electron lane; every platform still runs the shared client suite.
 metadata: { ...web.metadata, prebuildBotRuntime: process.platform === 'linux' },
 testIgnore: process.platform === 'linux' ? [] : [
  '**/announcement-scheduler.spec.ts', '**/bot-runtime.spec.ts', '**/moderation-evidence.spec.ts', '**/automod.spec.ts', '**/slash-commands.spec.ts',
 ],
 projects: [{ name: 'electron-client', use: { browserName: 'chromium' } }],
 reporter: [['list'], ['html', { open: 'never', outputFolder: 'desktop-client-report' }]],
 outputDir: 'desktop-client-results',
});
