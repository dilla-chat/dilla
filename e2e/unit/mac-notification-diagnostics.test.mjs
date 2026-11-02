import assert from 'node:assert/strict';
import test from 'node:test';
import { appendFile, readFile, stat, access, mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { electronChildEnvironment, notificationLogFacts, macNotificationIdentity, prepareMacNotificationDiagnostic } from '../web/support/mac-notification-diagnostics.ts';

const presenter = message => `[123:1102/200000.000000:INFO:notification_presenter_mac.mm(61)] ${message}\n`;
const cocoa = message => `[123:1102/200000.000000:INFO:cocoa_notification.mm(201)] ${message}\n`;
const execPath = '/Users/runner/work/dilla/node_modules/electron/dist/Electron.app/Contents/MacOS/Electron';
const commandResult = (stdout = '', stderr = '', status = 0) => ({ stdout, stderr, status, signal: null });

test('native notification diagnostics distinguish native denial from scheduling failure without retaining message content', () => {
 const facts = notificationLogFacts(presenter('Notification authorization granted: 0')
  + cocoa('Error scheduling notification (NOTICE_SECRET) The operation couldn’t be completed. (UNErrorDomain error 1.)')
  + '[123:INFO:CONSOLE(1)] Notification authorization granted: 1 TOKEN_SECRET https://private.invalid\n');
 assert.deepEqual(facts, { authorization: 'denied', authorizationErrorCode: null, scheduled: false, schedulingFailed: true, schedulingErrorCode: 1 });
 assert.doesNotMatch(JSON.stringify(facts), /SECRET|private|NOTICE/);
 assert.equal(notificationLogFacts(presenter('Notification authorization granted: 1') + cocoa('Notification scheduled (NOTICE_SECRET)')).scheduled, true);
 assert.equal(notificationLogFacts(presenter('Notification authorization granted: 1')).authorization, 'granted');
});

test('authorization errors preserve only a native numeric code and reject spoofed or oversized log lines', () => {
 assert.equal(notificationLogFacts(presenter('Error requesting notification authorization: The operation couldn’t be completed. (UNErrorDomain error 1.)')).authorizationErrorCode, 1);
 const facts = notificationLogFacts(presenter('Error requesting notification authorization: TOKEN_SECRET https://private.invalid')
  + presenter('Notification authorization granted: 1' + 'x'.repeat(3000))
  + 'Notification authorization granted: 1\n');
 assert.equal(facts.authorization, 'error');
 assert.equal(facts.authorizationErrorCode, null);
 assert.doesNotMatch(JSON.stringify(facts), /SECRET|private|xxx/);
});

test('explicit child environments preserve unset values without modifying the parent environment', () => {
 assert.equal(electronChildEnvironment(undefined), undefined);
 const input = { ONE: 'first', UNSET: undefined, ELECTRON_DEBUG_NOTIFICATIONS: '0' };
 assert.deepEqual(electronChildEnvironment(input), { ONE: 'first', ELECTRON_DEBUG_NOTIFICATIONS: '0' });
 assert.deepEqual(input, { ONE: 'first', UNSET: undefined, ELECTRON_DEBUG_NOTIFICATIONS: '0' });
});

test('the real desktop launcher forwards an explicit environment and leaves the default absent without starting Electron', async t => {
 const { _electron } = await import('@playwright/test');
 const { launchDesktopContext } = await import('../web/support/electron.ts');
 const launches = [];
 const stopped = new Error('Stopped at the native process boundary');
 t.mock.method(_electron, 'launch', async options => { launches.push(options); throw stopped; });
 const profile = await mkdtemp(join(tmpdir(), 'dilla-launch-env-unit-'));
 try {
  await assert.rejects(launchDesktopContext(profile, { launchOptions: { env: { CHILD_ONLY: 'value', OMIT: undefined } } }), error => error === stopped);
  await assert.rejects(launchDesktopContext(profile, { launchOptions: {} }), error => error === stopped);
  assert.deepEqual(launches[0].env, { CHILD_ONLY: 'value' });
  assert.equal(Object.hasOwn(launches[1], 'env'), false);
  assert.equal(launches[0].chromiumSandbox, true);
 } finally { await rm(profile, { recursive: true, force: true }); }
});

test('identity diagnostics inspect only the actual known Electron bundle and reconstruct public signature facts', () => {
 const calls = [];
 const run = (file, args, options) => {
  calls.push([file, args]);
  assert.equal(options.shell, false);
  assert.ok(options.timeout <= 5000);
  assert.ok(options.maxBuffer <= 8192);
  if (file === '/usr/bin/plutil') return commandResult('com.github.Electron\n');
  if (args.includes('--verify')) return commandResult('', 'PRIVATE_ERROR_SECRET', 1);
  return commandResult('', 'Identifier=com.github.Electron\nSignature=adhoc\nTeamIdentifier=not set\nPRIVATE_ERROR_SECRET\n');
 };
 const facts = macNotificationIdentity({ execPath, isPackaged: false }, run);
 assert.equal(facts.execPath, execPath);
 assert.equal(facts.isPackaged, false);
 assert.equal(facts.bundleId, 'com.github.Electron');
 assert.equal(facts.signature.kind, 'adhoc');
 assert.equal(facts.signature.verify, 'failed');
 assert.doesNotMatch(JSON.stringify(facts), /PRIVATE_ERROR_SECRET/);
 const bundle = '/Users/runner/work/dilla/node_modules/electron/dist/Electron.app';
 assert.deepEqual(calls, [
  ['/usr/bin/plutil', ['-extract', 'CFBundleIdentifier', 'raw', '-o', '-', `${bundle}/Contents/Info.plist`]],
  ['/usr/bin/codesign', ['-d', '--verbose=4', bundle]],
  ['/usr/bin/codesign', ['--verify', '--deep', '--strict', bundle]],
 ]);
 for (const bad of ['/tmp/other.app/Contents/MacOS/Electron', `${execPath}\nSECRET`, '/tmp/Electron.app/../SECRET']) {
  assert.equal(macNotificationIdentity({ execPath: bad, isPackaged: false }, () => { throw new Error('Must not execute'); }).execPath, null);
 }
});

test('diagnostic command errors yield unavailable facts without exposing errors', () => {
 const facts = macNotificationIdentity({ execPath, isPackaged: false }, () => { throw new Error('TOKEN_SECRET'); });
 assert.equal(facts.signature.display, 'unavailable');
 assert.equal(facts.signature.verify, 'unavailable');
 assert.equal(facts.bundleId, null);
 assert.doesNotMatch(JSON.stringify(facts), /TOKEN_SECRET/);
});

test('an unsigned bundle is distinguishable from an unavailable codesign command without retaining its error text', () => {
 const facts = macNotificationIdentity({ execPath, isPackaged: false }, () => commandResult('', 'PRIVATE_PATH_SECRET: code object is not signed at all\n', 1));
 assert.equal(facts.signature.kind, 'unsigned');
 assert.equal(facts.signature.display, 'failed');
 assert.doesNotMatch(JSON.stringify(facts), /PRIVATE_PATH_SECRET/);
});

test('diagnostics are opt-in on macOS and leave other child environments untouched', async () => {
 assert.equal(await prepareMacNotificationDiagnostic({ platform: 'linux', enabled: true }), undefined);
 assert.equal(await prepareMacNotificationDiagnostic({ platform: 'darwin', enabled: false }), undefined);
});

test('private startup logs are sanitized before reporting and removed on launch failure or context close', async () => {
 const reports = [];
 const environment = { PATH: '/usr/bin', KEEP: 'value', ELECTRON_ALSO_LOG_TO_STDERR: '1' };
 const diagnostic = await prepareMacNotificationDiagnostic({ platform: 'darwin', enabled: true, environment, report: facts => { reports.push(structuredClone(facts)); } });
 const path = diagnostic.environment.ELECTRON_LOG_FILE;
 assert.equal((await stat(path)).mode & 0o777, 0o600);
 assert.equal((await stat(dirname(path))).mode & 0o777, 0o700);
 assert.equal(diagnostic.environment.ELECTRON_ENABLE_LOGGING, 'file');
 assert.equal(diagnostic.environment.ELECTRON_DEBUG_NOTIFICATIONS, '1');
 assert.equal(diagnostic.environment.ELECTRON_ALSO_LOG_TO_STDERR, '');
 assert.deepEqual(environment, { PATH: '/usr/bin', KEEP: 'value', ELECTRON_ALSO_LOG_TO_STDERR: '1' });
 await appendFile(path, presenter('Notification authorization granted: 0') + 'TITLE_SECRET BODY_SECRET https://private.invalid TOKEN_SECRET\n');
 await diagnostic.capture();
 assert.equal(reports.at(-1).native.authorization, 'denied');
 assert.doesNotMatch(JSON.stringify(reports), /SECRET|private\.invalid/);
 await diagnostic.finish();
 await assert.rejects(access(path), { code: 'ENOENT' });
 // This also exercises the pre-launch failure path: no runtime identity is required.
 assert.equal(reports.at(-1).identity, null);
 assert.equal(reports.at(-1).finished, true);
 await diagnostic.finish();
});

test('untrusted log input is byte bounded and a failing reporter cannot replace the test failure', async () => {
 const reports = [];
 const diagnostic = await prepareMacNotificationDiagnostic({ platform: 'darwin', enabled: true, report: facts => { reports.push(facts); throw new Error('REPORT_SECRET'); } });
 const path = diagnostic.environment.ELECTRON_LOG_FILE;
 await appendFile(path, 'S'.repeat(8 * 1024 * 1024) + '\n' + presenter('Notification authorization granted: 1'));
 await diagnostic.capture();
 assert.equal(reports.at(-1).logTruncated, true);
 assert.equal(reports.at(-1).native.authorization, 'not-observed');
 await diagnostic.finish();
 await assert.rejects(readFile(path), { code: 'ENOENT' });
});
