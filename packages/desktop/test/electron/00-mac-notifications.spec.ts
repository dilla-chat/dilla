import { test, expect, _electron, type ElectronApplication } from '@playwright/test';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';

test('macOS accepts a real native notification from the shipping sandboxed app', async () => {
 test.skip(process.platform !== 'darwin', 'Native macOS notification prerequisite');
 const profile = await mkdtemp(join(tmpdir(), 'dilla-mac-notification-smoke-'));
 let desktop: ElectronApplication | undefined;
 try {
  desktop = await _electron.launch({ chromiumSandbox: true,
   args: [resolve(import.meta.dirname, '../..'), `--profile=${profile}`] });
  await desktop.firstWindow(); // The shipping connection window requires no server.
  const result = await desktop.evaluate(({ Notification }) => new Promise<'shown'>((resolve, reject) => {
   const notice = new Notification({ title: 'Dilla native notification acceptance',
    body: 'Checking the macOS notification delivery prerequisite.', silent: true });
   let settled = false;
   const finish = (error?: Error) => {
    if (settled) return;
    settled = true; clearTimeout(timer); notice.close();
    if (error) reject(error); else resolve('shown');
   };
   const timer = setTimeout(() => finish(new Error('macOS native notification did not emit show within 10 seconds')), 10000);
   // Electron emits show after the real UNUserNotificationCenter scheduling
   // completion succeeds. No permission or notification implementation is replaced.
   notice.once('show', () => finish());
   notice.once('failed', (_event, error) => finish(new Error('macOS native notification delivery failed: ' + error)));
   try { notice.show(); } catch (error) { finish(error instanceof Error ? error : new Error('Native notification show threw')); }
  }));
  expect(result).toBe('shown');
 } finally {
  try { await desktop?.close(); }
  finally { await rm(profile, { recursive: true, force: true }); }
 }
});
