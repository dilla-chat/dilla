import assert from 'node:assert/strict';
import test from 'node:test';
import { spawnSync } from 'node:child_process';
import { mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { createRequire } from 'node:module';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const require = createRequire(import.meta.url);
const directory = dirname(fileURLToPath(import.meta.url));
const sentinel = 'UNUSED_BROWSER_FIXTURE_RESOLVED';

// Run the actual fixture engine with a browser dependency that refuses to start.
// No people are opened, so neither an Electron process nor a test host is needed.
for (const desktop of [true, false]) {
  test(`${desktop ? 'desktop' : 'browser'} voice fixture dependency graph`, async () => {
    const temporary = await mkdtemp(join(directory, '.voice-fixture-'));
    try {
      const reportPath = join(temporary, 'report.json');
      const configPath = join(temporary, 'playwright.config.ts');
      await writeFile(configPath, `export default {
        testDir: '.', testMatch: 'fixture.spec.ts', workers: 1, retries: 0, timeout: 10000,
        outputDir: ${JSON.stringify(join(temporary, 'results'))},
        reporter: [['json', { outputFile: ${JSON.stringify(reportPath)} }]],
      };`);
      await writeFile(join(temporary, 'fixture.spec.ts'), `
        import { test as voiceTest, expect } from ${JSON.stringify(fileURLToPath(new URL('../voice/support/peer-browser.ts', import.meta.url)))};
        import { watchContexts } from ${JSON.stringify(fileURLToPath(new URL('../voice/support/leaks.ts', import.meta.url)))};
        const test = voiceTest.extend({
          browser: async ({}, use) => { throw new Error(${JSON.stringify(sentinel)}); },
        });
        test('peerBrowser without people', async ({ peerBrowser }) => {
          expect(peerBrowser.guard.cspViolations).toEqual([]);
          expect(peerBrowser.guard.consoleErrors).toEqual([]);
          expect(peerBrowser.guard.pageErrors).toEqual([]);
        });
        test.describe('with context leak hooks', () => {
          watchContexts(test);
          test('hooks without people', async ({}) => {});
          test('peerBrowser and hooks without people', async ({ peerBrowser }) => {
            expect(typeof peerBrowser.open).toBe('function');
          });
        });
      `);
      const result = spawnSync(process.execPath, [require.resolve('@playwright/test/cli'), 'test', '--config', configPath], {
        env: { ...process.env, DILLA_ELECTRON_E2E: desktop ? '1' : '0' },
        encoding: 'utf8', timeout: 60_000,
      });
      assert.ifError(result.error);
      assert.equal(result.signal, null, result.stderr);
      const report = JSON.parse(await readFile(reportPath, 'utf8'));
      assert.deepEqual(report.errors, [], `${result.stdout}\n${result.stderr}`);
      const specs = (suites = []) => suites.flatMap(suite => [...suite.specs, ...specs(suite.suites)]);
      const results = specs(report.suites).flatMap(spec => spec.tests.flatMap(test => test.results));
      assert.equal(results.length, 3);
      if (desktop) {
        assert.equal(result.status, 0, JSON.stringify(results));
        assert.ok(results.every(result => result.status === 'passed'));
      } else {
        assert.equal(result.status, 1);
        for (const result of results) {
          assert.equal(result.status, 'failed');
          assert.ok(result.errors.some(error => error.message.includes(sentinel)), JSON.stringify(result));
        }
      }
    } finally {
      await rm(temporary, { recursive: true, force: true });
    }
  });
}
