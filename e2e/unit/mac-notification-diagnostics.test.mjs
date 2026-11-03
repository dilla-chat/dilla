import assert from 'node:assert/strict';
import test from 'node:test';
import { appendFile, readFile, stat, access, mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { electronChildEnvironment, notificationLogFacts, macNotificationIdentity, prepareMacNotificationDiagnostic } from '../web/support/mac-notification-diagnostics.ts';
import * as diagnostics from '../web/support/mac-notification-diagnostics.ts';

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

test('Chromium 156 source paths preserve native authorization facts without accepting unrelated sources', () => {
 const prefix = '[123:1103/003021.000000:INFO:electron/shell/browser/notifications/mac/';
 const facts = notificationLogFacts(prefix + 'notification_presenter_mac.mm:61] Error requesting notification authorization: Notifications are not allowed for this application\n'
  + prefix + 'cocoa_notification.mm:200] Error scheduling notification (NOTICE_SECRET) Notifications are not allowed for this application\n'
  + '[123:1103/003021.000000:INFO:electron/shell/renderer/notification_presenter_mac.mm:61] Notification authorization granted: 1\n');
 assert.equal(facts.authorization, 'error');
 assert.equal(facts.authorizationErrorCode, null);
 assert.equal(facts.schedulingFailed, true);
 assert.equal(facts.schedulingErrorCode, null);
 assert.doesNotMatch(JSON.stringify(facts), /NOTICE_SECRET|not allowed/);
 assert.equal(notificationLogFacts(prefix + 'notification_presenter_mac.mm:64] Notification authorization granted: 1\n').authorization, 'granted');
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

test('stock linker signatures are distinguished from a sealed ad-hoc app identity', () => {
 const inspect = display => macNotificationIdentity({ execPath, isPackaged: false }, (file, args) =>
  file === '/usr/bin/plutil' ? commandResult('com.github.Electron\n')
   : args.includes('--verify') ? commandResult()
   : commandResult('', display)).signature;
 const linker = inspect('Identifier=Electron\nCodeDirectory v=20400 size=492 flags=0x20002(adhoc,linker-signed) hashes=12+0 location=embedded\nSignature=adhoc\nInfo.plist=not bound\nSealed Resources=none\n');
 assert.equal(linker.linkerSigned, true);
 assert.equal(linker.infoPlistBound, false);
 assert.equal(linker.resourcesSealed, false);
 const sealed = inspect('Identifier=com.github.Electron\nCodeDirectory v=20400 size=492 flags=0x2(adhoc) hashes=12+7 location=embedded\nSignature=adhoc\nInfo.plist entries=29\nSealed Resources version=2 rules=13 files=1\n');
 assert.equal(sealed.linkerSigned, false);
 assert.equal(sealed.infoPlistBound, true);
 assert.equal(sealed.resourcesSealed, true);
});

test('signature verification failures retain an allowlisted cause without exposing paths or error text', () => {
 for (const [error, expected] of [
  ['PRIVATE_PATH_SECRET: invalid Info.plist (plist or signature have been modified)\n', 'invalid-info-plist'],
  ['PRIVATE_PATH_SECRET: a sealed resource is missing or invalid\n', 'invalid-resource-seal'],
  ['PRIVATE_PATH_SECRET: code object is not signed at all\n', 'unsigned'],
  ['PRIVATE_PATH_SECRET: UNKNOWN_SECRET\n', 'other'],
 ]) {
  const facts = macNotificationIdentity({ execPath, isPackaged: false }, (file, args) =>
   file === '/usr/bin/plutil' ? commandResult('com.github.Electron\n')
    : args.includes('--verify') ? commandResult('', error, 1) : commandResult());
  assert.equal(facts.signature.verifyReason, expected);
  assert.doesNotMatch(JSON.stringify(facts), /SECRET/);
 }
});

test('macOS CI preparation seals the npm runtime as an app before accepting its identity', () => {
 assert.equal(typeof diagnostics.prepareMacElectronRuntime, 'function');
 const calls = [];
 const run = (file, args, options) => {
  calls.push([file, args]);
  assert.equal(options.shell, false);
  if (args.includes('--sign')) return commandResult();
  if (file === '/usr/bin/plutil') return commandResult('com.github.Electron\n');
  if (args.includes('--verify')) return commandResult();
  return commandResult('', 'Identifier=com.github.Electron\nCodeDirectory v=20400 size=492 flags=0x2(adhoc)\nSignature=adhoc\nInfo.plist entries=29\nSealed Resources version=2 rules=13 files=1\n');
 };
 const identity = diagnostics.prepareMacElectronRuntime({ execPath, platform: 'darwin', environment: { CI: 'true', GITHUB_ACTIONS: 'true', RUNNER_OS: 'macOS' } }, run);
 assert.equal(identity.signature.verify, 'passed');
 assert.equal(identity.signature.identifier, identity.bundleId);
 assert.deepEqual(calls[0], ['/usr/bin/codesign', ['--force', '--deep', '--sign', '-', '/Users/runner/work/dilla/node_modules/electron/dist/Electron.app']]);
});

test('macOS runtime preparation refuses local hosts and cannot accept an unsealed or mismatched identity', () => {
 assert.equal(typeof diagnostics.prepareMacElectronRuntime, 'function');
 const environment = { CI: 'true', GITHUB_ACTIONS: 'true', RUNNER_OS: 'macOS' };
 const options = { execPath, platform: 'darwin', environment };
 for (const input of [ { ...options, environment: {} }, { ...options, platform: 'linux' }, { ...options, execPath: '/tmp/other.app/Contents/MacOS/Electron' } ]) {
  assert.throws(() => diagnostics.prepareMacElectronRuntime(input, () => { assert.fail('No command allowed'); }), /macOS CI npm Electron/);
 }
 const inspect = (id, bound, verified) => (file, args) => args.includes('--sign') ? commandResult()
  : file === '/usr/bin/plutil' ? commandResult('com.github.Electron\n')
  : args.includes('--verify') ? commandResult('', 'PRIVATE_ERROR_SECRET', verified ? 0 : 1)
  : commandResult('', `Identifier=${id}\nCodeDirectory v=20400 size=492 flags=0x2(adhoc)\nSignature=adhoc\nInfo.plist${bound ? ' entries=29' : '=not bound'}\nSealed Resources version=2 rules=13 files=1\n`);
 for (const input of [['Electron', true, true], ['com.github.Electron', false, true], ['com.github.Electron', true, false]]) {
  assert.throws(() => diagnostics.prepareMacElectronRuntime(options, inspect(...input)), /valid sealed Electron app/);
 }
 assert.throws(() => diagnostics.prepareMacElectronRuntime(options, () => commandResult('', 'PRIVATE_ERROR_SECRET', 1)), error => /seal the Electron app/.test(error.message) && !error.message.includes('PRIVATE_ERROR_SECRET'));
});

test('CI registers only its exact npm Electron bundle and preserves bounded before/after installation facts', () => {
 assert.equal(typeof diagnostics.registerMacElectronRuntime, 'function');
 const execPath = fileURLToPath(new URL('../../node_modules/electron/dist/Electron.app/Contents/MacOS/Electron', import.meta.url));
 const reports = [], calls = [];
 const expected = { before: { registered: false, candidates: 0 }, after: { registered: true, candidates: 1 }, registrationStatus: 0 };
 const result = diagnostics.registerMacElectronRuntime({ execPath, platform: 'darwin', environment: { CI: 'true', GITHUB_ACTIONS: 'true', RUNNER_OS: 'macOS' }, report: facts => reports.push(facts) }, (file, args, options) => {
  calls.push([file, args]); assert.equal(options.shell, false); assert.ok(options.timeout <= 60000); assert.ok(options.maxBuffer <= 8192);
  return commandResult(JSON.stringify(expected), 'PRIVATE_WARNING_SECRET');
 });
 assert.deepEqual(result, expected); assert.deepEqual(reports, [expected]);
 assert.deepEqual(calls, [['/usr/bin/xcrun', ['swift', fileURLToPath(new URL('../web/support/register-mac-electron-runtime.swift', import.meta.url)), execPath.slice(0, -'/Contents/MacOS/Electron'.length)]]]);
 for (const input of [{ execPath: '/tmp/Electron.app/Contents/MacOS/Electron' }, { platform: 'linux' }, { environment: {} }]) {
  assert.throws(() => diagnostics.registerMacElectronRuntime({ execPath, platform: 'darwin', environment: { CI: 'true', GITHUB_ACTIONS: 'true', RUNNER_OS: 'macOS' }, ...input }, () => assert.fail('No installation action allowed')), /macOS CI npm Electron/);
 }
});

test('registration rejects malformed evidence and reports a real OS failure without leaking command output', () => {
 assert.equal(typeof diagnostics.registerMacElectronRuntime, 'function');
 const execPath = fileURLToPath(new URL('../../node_modules/electron/dist/Electron.app/Contents/MacOS/Electron', import.meta.url));
 const reports = [];
 const options = { execPath, platform: 'darwin', environment: { CI: 'true', GITHUB_ACTIONS: 'true', RUNNER_OS: 'macOS' }, report: facts => reports.push(facts) };
 for (const output of ['PRIVATE_SECRET', JSON.stringify({ before: { registered: false, candidates: 0 }, after: { registered: true, candidates: 1 }, registrationStatus: 0, PRIVATE_SECRET: 'raw' }),
  JSON.stringify({ before: { registered: false, candidates: -1 }, after: { registered: true, candidates: 1 }, registrationStatus: 0 }),
  JSON.stringify({ before: { registered: false, candidates: 0 }, after: { registered: true, candidates: 0 }, registrationStatus: 0 })]) {
  assert.throws(() => diagnostics.registerMacElectronRuntime(options, () => commandResult(output)), error => /Launch Services/.test(error.message) && !error.message.includes('PRIVATE_SECRET'));
 }
 const failure = { before: { registered: false, candidates: 1 }, after: { registered: false, candidates: 1 }, registrationStatus: -10811 };
 assert.throws(() => diagnostics.registerMacElectronRuntime(options, () => commandResult(JSON.stringify(failure))), /Launch Services.*-10811/);
 assert.deepEqual(reports.at(-1), failure);
 const already = { before: { registered: true, candidates: 1 }, after: { registered: true, candidates: 1 }, registrationStatus: 0 };
 assert.deepEqual(diagnostics.registerMacElectronRuntime(options, () => commandResult(JSON.stringify(already))), already);
 assert.doesNotMatch(JSON.stringify(reports), /PRIVATE_SECRET/);
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
