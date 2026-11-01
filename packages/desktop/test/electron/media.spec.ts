import { test, expect, _electron, type ElectronApplication, type Page } from '@playwright/test';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import type { FailClosedResult, FailClosedScenario } from '../../../media/harness/failclosed.ts';

let desktop: ElectronApplication;
let page: Page;
let profile: string;
test.beforeEach(async () => {
  profile = await mkdtemp(join(tmpdir(), 'dilla-media-validation-'));
  desktop = await _electron.launch({ chromiumSandbox: true, args: [resolve('.'), `--profile=${profile}`, '--instance=http://127.0.0.1:1/', '--allow-loopback', '--media-validation'] });
  page = await desktop.firstWindow();
  await page.waitForFunction(() => 'validation' in window);
  await page.bringToFront();
});
test.afterEach(async () => { await desktop?.close(); await rm(profile, { recursive: true, force: true }); });

async function run(scenario: FailClosedScenario): Promise<FailClosedResult> {
  const result = await page.evaluate(s => (window as any).validation.run(s), scenario) as FailClosedResult;
  await test.info().attach('encrypted-media-counters', { body: JSON.stringify(result, null, 2), contentType: 'application/json' });
  console.log('ENCRYPTED_MEDIA_RESULT', JSON.stringify(result));
  return result;
}

test('the real media worker encrypts and decrypts video in Electron', async () => {
  const result = await run({ send: 'dilla', recv: 'dilla', label: 'vp8', ms: 4000 });
  const encrypted = Object.values(result.sender?.encrypted ?? {}).flatMap(Object.values).reduce((a, b) => a + b, 0);
  const decrypted = Object.values(result.receiver?.decrypted ?? {}).reduce((a, b) => a + b, 0);
  expect(encrypted).toBeGreaterThan(10);
  expect(decrypted).toBeGreaterThan(10);
  expect(result.rendered).toBeGreaterThan(0);
  expect(result.sender?.passedThrough).toBe(0);
  expect(result.errors).toEqual([]);
});

test('an unbound sender cannot fall back to plaintext', async () => {
  const result = await run({ send: 'dilla', recv: 'none', label: 'vp8', source: 'unknown', ms: 4000 });
  expect(result.bytesSent).toBe(0);
  expect(result.rendered).toBe(0);
  expect(result.sender?.dropped.blocked).toBeGreaterThan(10);
  expect(result.unpublished).toHaveLength(1);
});

test('a failed media worker stops sending frames', async () => {
  const result = await run({ send: 'dilla', recv: 'none', label: 'vp8', kill: 'sender', killAfterMs: 1500, ms: 5000 });
  expect(result.afterKill?.workerTerminated).toBe(true);
  expect(result.bytesSent).toBeGreaterThan(1000);
  expect(result.afterKill?.bytesSent).toBe(0);
  expect(result.afterKill?.rendered).toBe(0);
});
