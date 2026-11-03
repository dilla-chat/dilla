import { defineConfig } from '@playwright/test';

export default defineConfig({
  testDir: './test/electron', workers: 1, fullyParallel: false, timeout: 45_000,
  // DILLA_E2E_FAIL_FAST=0 (desktop.yml stabilisation mode) reports every failure instead of stopping at the first.
  maxFailures: process.env.CI === 'true' && process.env.DILLA_E2E_FAIL_FAST !== '0' ? 1 : 0,
  outputDir: './test-results', reporter: [['list'], ['html', { open: 'never', outputFolder: 'test-report' }]],
});
