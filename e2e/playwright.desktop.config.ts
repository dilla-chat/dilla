import { defineConfig } from '@playwright/test';
import web from './playwright.web.config.ts';
// The shared client suite uses actual Electron processes for persistent and
// secondary devices. Existing browser projects remain until parity passes.
process.env.DILLA_ELECTRON_E2E = '1';
export default defineConfig({
 ...web,
 projects: [{ name: 'electron-client', use: { browserName: 'chromium' } }],
 reporter: [['list'], ['html', { open: 'never', outputFolder: 'desktop-client-report' }]],
 outputDir: 'desktop-client-results',
});
