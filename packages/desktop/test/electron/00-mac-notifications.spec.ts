import { test, expect, _electron, type ElectronApplication } from '@playwright/test';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';

async function exitOwned(desktop: ElectronApplication | undefined): Promise<void> {
 if (!desktop) return;
 const child = desktop.process();
 if (child.exitCode !== null || child.signalCode !== null) return;
 let deadline: ReturnType<typeof setTimeout> | undefined;
 const exited = new Promise<void>((done, reject) => {
  child.once('exit', () => done());
  deadline = setTimeout(() => reject(new Error('Owned macOS notification smoke process did not exit')), 10000);
 });
 let forced = false;
 const timer = setTimeout(() => { forced = true; child.kill('SIGKILL'); }, 5000);
 try {
  // Exercise teardown, not the product's guarded Quit workflow. Only this
  // disposable acceptance process can be terminated by the fallback.
  const requested = desktop.evaluate(() => { setImmediate(() => process.exit(0)); }).catch(() => {});
  await Promise.race([requested, exited]); await exited;
 } finally { clearTimeout(timer); clearTimeout(deadline); }
 if (forced) throw new Error('Owned macOS notification smoke process required forced cleanup');
}

test('macOS accepts a real native notification from the shipping sandboxed app', async () => {
 test.skip(process.platform !== 'darwin', 'Native macOS notification prerequisite');
 const profile = await mkdtemp(join(tmpdir(), 'dilla-mac-notification-smoke-'));
 let desktop: ElectronApplication | undefined, bodyFailed = false;
 try {
  console.info('Mac notification smoke phase: launch');
  desktop = await test.step('launch shipping sandboxed app', () => _electron.launch({ chromiumSandbox: true,
   args: [resolve(import.meta.dirname, '../..'), `--profile=${profile}`] }));
  console.info('Mac notification smoke phase: main readiness');
  await test.step('await main-process app readiness', () => desktop!.evaluate(async ({ app }) => { await app.whenReady(); }));
  console.info('Mac notification smoke phase: native delivery');
  const result = await test.step('await actual native notification show or failure', () => desktop!.evaluate(({ Notification }) => new Promise<'shown'>((resolve, reject) => {
   const notice = new Notification({ title: 'Dilla native notification acceptance',
    body: 'Checking the macOS notification delivery prerequisite.', silent: true });
   let settled = false;
   const finish = (error?: Error) => {
    if (settled) return;
    settled = true; clearTimeout(timer);
    try { notice.close(); } catch (closeError) { error ??= closeError instanceof Error ? closeError : new Error('Native notification cleanup failed'); }
    if (error) reject(error); else resolve('shown');
   };
   const timer = setTimeout(() => finish(new Error('macOS native notification did not emit show within 10 seconds')), 10000);
   // Electron emits show after the real UNUserNotificationCenter scheduling
   // completion succeeds. No permission or notification implementation is replaced.
   notice.once('show', () => finish());
   notice.once('failed', (_event, error) => finish(new Error('macOS native notification delivery failed: ' + error)));
   try { notice.show(); } catch (error) { finish(error instanceof Error ? error : new Error('Native notification show threw')); }
  })));
  expect(result).toBe('shown');
 } catch (error) { bodyFailed = true; throw error;
 } finally {
  console.info('Mac notification smoke phase: owned cleanup');
  try { await test.step('clean up owned notification smoke process and profile', async () => {
   try { await exitOwned(desktop); }
   finally { await rm(profile, { recursive: true, force: true }); }
  }); }
  catch (error) { if (!bodyFailed) throw error; console.error('Owned notification smoke cleanup also failed; preserving the original failure'); }
 }
});
