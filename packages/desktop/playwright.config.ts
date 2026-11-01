import { defineConfig } from '@playwright/test';

export default defineConfig({
  testDir: './test/electron', workers: 1, fullyParallel: false, timeout: 45_000,
  outputDir: './test-results', reporter: [['list'], ['html', { open: 'never', outputFolder: 'test-report' }]],
});
