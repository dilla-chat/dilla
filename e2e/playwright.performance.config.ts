import { defineConfig } from '@playwright/test';
import voice from './playwright.voice.config.ts';
import { resolve } from 'node:path';
const mode = process.env.DILLA_PERFORMANCE_CLIENT;
if (mode !== 'browser' && mode !== 'desktop') throw new Error('Choose a browser or desktop performance client');
process.env.DILLA_ELECTRON_E2E = mode === 'desktop' ? '1' : '0';
export default defineConfig({
 ...voice, testDir: resolve(import.meta.dirname, 'performance'), workers: 1, retries: 0, timeout: 300_000,
 projects: [{ name: `${mode}-performance`, use: { ...voice.projects![0].use, channel: undefined, launchOptions: { ...voice.projects![0].use!.launchOptions, chromiumSandbox: true }, headless: false, viewport: { width: 1280, height: 800 } } }],
 use: { ...voice.use, trace: 'off' },
 reporter: [['list']], outputDir: `performance-results-${mode}`,
});
