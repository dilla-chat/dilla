import { defineConfig } from '@playwright/test';
export default defineConfig({
 testDir: './web/support', testMatch: 'native-stock-feasibility.case.ts',
 workers: 1, retries: 0, timeout: 180_000, reporter: [['list']],
});
