import assert from 'node:assert/strict';
import test from 'node:test';
import { readFile, mkdtemp, mkdir, realpath, symlink, writeFile, rm } from 'node:fs/promises';
import { join, sep } from 'node:path';
import { tmpdir } from 'node:os';
import { generateKeyPairSync } from 'node:crypto';
import { createRequire } from 'node:module';
import { runInNewContext } from 'node:vm';
import { createServer } from 'node:net';
import { installedAccountDiagnostics } from '../web/support/installed-diagnostics.ts';
import { fixtureMainSource, INSTALLED_FUSES } from '../web/support/installed-packages.ts';
import * as fixtures from '../web/support/installed-packages.ts';
import { finalizeWindowsProgram } from '../../packages/desktop/scripts/windows-package.mjs';

test('installed failure diagnostics retain actual process, update, bundle and endpoint facts without private data', async () => {
  const root = await realpath(await mkdtemp(join(tmpdir(), 'dilla-account-diagnostic-'))), reports = join(root, 'reports'), profile = join(root, 'profile');
  const app = join(root, 'Applications/OwnedFixture.app'), installed = join(app, 'Contents/MacOS/dilla-desktop');
  const server = createServer(socket => socket.end());
  try {
    await mkdir(reports); await mkdir(profile); await mkdir(join(app, 'Contents/MacOS'), { recursive: true });
    await writeFile(installed, 'fixture executable');
    await writeFile(join(app, 'Contents/Info.plist'), '<plist><dict><key>CFBundleVersion</key><string>1.0.1</string><key>PRIVATE_SECRET</key><string>hidden</string></dict></plist>');
    await writeFile(join(reports, 'processes.jsonl'), JSON.stringify({ pid: process.pid, role: 'ordinary', version: '1.0.1', executable: '/PRIVATE_SECRET/profile', token: 'PRIVATE_SECRET' }) + '\n');
    await writeFile(join(reports, 'update-1.0.0.json'), JSON.stringify({ version: '1.0.0', state: { phase: 'installing', message: 'https://PRIVATE_SECRET/?token=SECRET' } }));
    await writeFile(join(reports, 'native-error.json'), 'PRIVATE_SECRET raw native error');
    await new Promise(done => server.listen(0, '127.0.0.1', done));
    const port = server.address().port, guid = '01234567-89ab-4cde-8fab-0123456789ab';
    await writeFile(join(profile, 'DevToolsActivePort'), `${port}\n/devtools/browser/${guid}`);
    const result = await installedAccountDiagnostics({ reports, profile, installed, platform: 'darwin', expectedVersion: '1.0.1', previousPort: port === 1 ? 2 : 1 });
    assert.deepEqual(result.processes.records, [{ pid: process.pid, role: 'ordinary', version: '1.0.1', alive: true }]);
    assert.equal(result.processes.status, 'ok');
    assert.deepEqual(result.updates, [{ version: '1.0.0', status: 'ok', phase: 'installing' }, { version: '1.0.1', status: 'missing', phase: null }]);
    assert.equal(result.candidateVersion, '1.0.1'); assert.equal(result.hasNativeError, true);
    assert.deepEqual(result.installed, { bundleExists: true, executableExists: true, bundleVersion: '1.0.1', plistStatus: 'ok' });
    assert.deepEqual(result.devTools, { present: true, valid: true, changed: true, live: true, status: 'ok' });
    const safe = JSON.stringify(result); assert.doesNotMatch(safe, /PRIVATE_SECRET|https:|token|devtools\/browser|profile/); assert.ok(!safe.includes(guid));
    await new Promise(done => server.close(done));
    const stale = await installedAccountDiagnostics({ reports, profile, installed, platform: 'darwin', expectedVersion: '1.0.1', previousPort: port });
    assert.deepEqual(stale.devTools, { present: true, valid: true, changed: false, live: false, status: 'ok' });
  } finally { if (server.listening) await new Promise(done => server.close(done)); await rm(root, { recursive: true, force: true }); }
});

test('installed failure diagnostics fail closed on malformed, oversized and symlinked private files', async () => {
  const root = await realpath(await mkdtemp(join(tmpdir(), 'dilla-account-diagnostic-invalid-'))), reports = join(root, 'reports'), profile = join(root, 'profile');
  const input = { reports, profile, installed: join(root, 'missing'), platform: 'linux', expectedVersion: '1.0.1' };
  try {
    await mkdir(reports); await mkdir(profile);
    const journal = join(reports, 'processes.jsonl');
    await writeFile(journal, '{PRIVATE_SECRET malformed');
    await writeFile(join(reports, 'update-1.0.0.json'), '{PRIVATE_SECRET malformed');
    await writeFile(join(profile, 'DevToolsActivePort'), 'not-a-port\nPRIVATE_SECRET');
    let result = await installedAccountDiagnostics(input);
    assert.equal(result.processes.status, 'malformed'); assert.deepEqual(result.processes.records, []);
    assert.equal(result.updates[0].status, 'malformed'); assert.equal(result.devTools.valid, false); assert.equal(result.devTools.live, false);
    await writeFile(journal, 'PRIVATE_SECRET'.repeat(6000));
    result = await installedAccountDiagnostics(input); assert.equal(result.processes.status, 'oversized');
    await rm(journal); const secret = join(root, 'PRIVATE_SECRET'); await mkdir(secret);
    await symlink(secret, journal, process.platform === 'win32' ? 'junction' : 'dir');
    result = await installedAccountDiagnostics(input); assert.equal(result.processes.status, 'unsafe'); assert.deepEqual(result.processes.records, []);
    await rm(journal); await writeFile(journal, (JSON.stringify({ pid: process.pid, role: 'ordinary', version: '1.0.1' }) + '\n').repeat(100));
    result = await installedAccountDiagnostics(input); assert.equal(result.processes.status, 'truncated'); assert.equal(result.processes.records.length, 64);
    assert.doesNotMatch(JSON.stringify(result), /PRIVATE_SECRET|not-a-port/);
  } finally { await rm(root, { recursive: true, force: true }); }
});

test('macOS account fixture locates the exact bundle produced by its declared executable name', async () => {
  const require = createRequire(import.meta.url), { AppInfo } = require('app-builder-lib/out/appInfo.js');
  const source = await readFile(new URL('../web/support/installed-packages.ts', import.meta.url), 'utf8');
  const executableName = /executableName: '([^']+)'/.exec(source)?.[1];
  assert.equal(executableName, 'dilla-desktop');
  // Evaluate the fixture's actual bundle lookup and compare it to the pinned
  // builder's naming contract. No macOS executable is launched on this host.
  const lookup = /const app = (join\(out,[^\n]+?\)), bins = await readdir/.exec(source)?.[1];
  assert.ok(lookup, 'the actual bundle lookup must be covered');
  const product = 'DillaAccountAcceptanceIsolatedFixture';
  const base = { productName: product, executableName };
  const info = new AppInfo({ config: base, metadata: { name: 'account-fixture', version: '1.0.0' } });
  assert.equal(info.productName, product);
  assert.equal(info.productFilename, executableName);
  for (const arch of ['arm64', 'x64']) {
    const out = join(tmpdir(), 'owned-account-packaging-output');
    const actual = runInNewContext(lookup, { join, out, product, base, process: { arch } });
    assert.equal(actual, join(out, arch === 'arm64' ? 'mac-arm64' : 'mac', info.productFilename + '.app'));
    assert.notEqual(actual, join(out, arch === 'arm64' ? 'mac-arm64' : 'mac', product + '.app'));
  }
});

test('installed account packaging resolves temporary aliases before strict Windows tree validation', async () => {
  const parent = await realpath(await mkdtemp(join(tmpdir(), 'dilla-account-path-')));
  try {
    const physical = join(parent, 'physical'), alias = join(parent, 'alias'); await mkdir(physical);
    await symlink(physical, alias, process.platform === 'win32' ? 'junction' : 'dir');
    const root = await fixtures.createInstalledFixtureRoot(alias), outDir = join(root, 'out'), appOutDir = join(outDir, 'win-unpacked');
    await mkdir(join(appOutDir, 'resources'), { recursive: true });
    await writeFile(join(appOutDir, 'dilla-desktop.exe'), 'portable packaging fixture'); await writeFile(join(appOutDir, 'resources/app.asar'), 'portable ASAR fixture');
    // Exercise the actual packaging/tree-validation boundary. This portable
    // compiler stand-in makes no claim about native Windows execution.
    const publicKey = generateKeyPairSync('ed25519').publicKey.export({ type: 'spki', format: 'pem' }).toString();
    await finalizeWindowsProgram({ appOutDir, outDir, version: '1.0.0', arch: 'x64', productName: 'DillaAccountAcceptanceFixture', appId: 'dev.dilla.desktop.account-acceptance.fixture', publicKeys: [publicKey],
      compileLauncher: async ({ file }) => { await writeFile(file, 'portable launcher fixture'); } });
    assert.equal(root, await realpath(root));
    assert.ok(root.startsWith(physical + sep), 'fixture identity and cleanup must use its physical root');
  } finally { await rm(parent, { recursive: true, force: true }); }
});

test('Windows account fixture replaces both actual compiled identity boundaries and rejects missing boundaries', async () => {
  const source = await readFile(new URL('../../packages/desktop/src/windows-installation.ts', import.meta.url), 'utf8');
  const identity = 'dev.dilla.desktop.account-acceptance.fixture';
  const compiled = fixtures.fixtureWindowsInstallationSource(source, identity);
  assert.equal((compiled.match(/description\.appId\s*!==\s*"dev\.dilla\.desktop\.account-acceptance\.fixture"/g) ?? []).length, 2);
  assert.doesNotMatch(compiled, /description\.appId\s*!==\s*'dev\.dilla\.desktop'/);
  assert.throws(() => fixtures.fixtureWindowsInstallationSource(source.replaceAll('description.appId', 'description.changed'), identity), /identity boundary/);
});

test('installed account automation retains production fuses and cannot enable Node inspection', async () => {
  assert.deepEqual(INSTALLED_FUSES, { runAsNode: false, enableCookieEncryption: true,
    enableNodeOptionsEnvironmentVariable: false, enableNodeCliInspectArguments: false,
    enableEmbeddedAsarIntegrityValidation: true, onlyLoadAppFromAsar: true, grantFileProtocolExtraPrivileges: false });
  const input = await readFile(new URL('../../packages/desktop/src/main.ts', import.meta.url), 'utf8');
  const compiled = fixtureMainSource(input, { product: 'DillaAccountFixture', profile: '/private/fixture-profile', reports: '/private/reports', scheme: 'dilla-account-fixture', version: '1.0.0' });
  assert.match(compiled, /remote-debugging-port/);
  assert.match(compiled, /--update-watchdog/);
  assert.match(compiled, /const allowLoopback = true/);
  assert.doesNotMatch(compiled, /devTools:\s*true|--inspect|no-sandbox|nodeIntegration:\s*true/);
  assert.throws(() => fixtureMainSource(input.replace('const updater =', 'const renamed ='), { product: 'DillaAccountFixture', profile: '/private/fixture-profile', reports: '/private/reports', scheme: 'dilla-account-fixture', version: '1.0.0' }), /boundary/);
});

test('compiled installed startup recorder preserves actual fetch receiver, arguments, response and errors without private data', async () => {
  assert.equal(typeof fixtures.fixtureStartupDiagnosticsSource, 'function');
  const root = await realpath(await mkdtemp(join(tmpdir(), 'dilla-account-startup-events-')));
  try {
    const fs = createRequire(import.meta.url)('node:fs');
    const diagnostic = runInNewContext(createRequire(import.meta.url)('esbuild').transformSync(fixtures.fixtureStartupDiagnosticsSource() + ';globalThis.compiled={fetch:__accountFetch,observeSession:__accountObserveSession,observeWindow:__accountObserveWindow}', { loader: 'ts' }).code + ';compiled', { __accountFS: fs, __accountReports: root, __accountRole: 'ordinary', Buffer, URL, app: { getVersion: () => '1.0.1' }, Date });
    const request = new Request('http://127.0.0.1:12345/v1/instance?credential=PRIVATE_SECRET'), options = { bypassCustomProtocolHandlers: true };
    const response = new Response('PRIVATE_SECRET response body', { status: 200 });
    let calls = 0;
    const session = { fetch(actualRequest, actualOptions) { assert.equal(this, session); assert.equal(actualRequest, request); assert.equal(actualOptions, options); calls++; return Promise.resolve(response); } };
    assert.equal(await diagnostic.fetch(session, request, options), response); assert.equal(calls, 1); assert.equal(response.bodyUsed, false);
    assert.equal(await response.text(), 'PRIVATE_SECRET response body');
    const original = new Error('net::ERR_CONNECTION_REFUSED http://PRIVATE_SECRET/?token=SECRET');
    const rejected = { fetch() { return Promise.reject(original); } };
    await assert.rejects(diagnostic.fetch(rejected, new Request('http://127.0.0.1/v1/session'), options), error => error === original);
    const thrown = { fetch() { throw original; } };
    assert.throws(() => diagnostic.fetch(thrown, request, options), error => error === original);
    const callbacks = {}, lifecycle = {};
    diagnostic.observeSession({ webRequest: { onCompleted(cb) { callbacks.completed = cb; }, onErrorOccurred(cb) { callbacks.error = cb; } } });
    callbacks.error({ url: 'http://PRIVATE_SECRET/v1/instance/limits?secret=SECRET', method: 'GET', error: 'net::ERR_TIMED_OUT' });
    diagnostic.observeWindow({ webContents: { on(event, cb) { lifecycle[event] = cb; } } });
    lifecycle['did-fail-load']({}, -102, 'PRIVATE_SECRET', 'http://PRIVATE_SECRET');
    lifecycle['render-process-gone']({}, { reason: 'crashed', exitCode: 17, private: 'PRIVATE_SECRET' });
    const raw = await readFile(join(root, 'startup-1.0.1.jsonl'), 'utf8'), events = raw.trim().split('\n').map(line => JSON.parse(line));
    assert.deepEqual(events.slice(0, 4).map(e => e.stage), ['fetch-begin', 'fetch-response', 'fetch-begin', 'fetch-reject']);
    assert.equal(events[0].route, 'instance'); assert.equal(events[3].code, -102);
    assert.ok(events.some(e => e.stage === 'request-error' && e.route === 'limits' && e.code === -7));
    assert.ok(events.some(e => e.stage === 'load-failed' && e.code === -102));
    assert.ok(events.some(e => e.stage === 'renderer-gone' && e.code === 17));
    assert.doesNotMatch(raw, /PRIVATE_SECRET|SECRET|credential|token|https?:|response body/);
    const diskFull = { ...fs, writeSync() { throw Object.assign(new Error('PRIVATE_SECRET disk failure'), { code: 'ENOSPC' }); } };
    const unavailable = runInNewContext(createRequire(import.meta.url)('esbuild').transformSync(fixtures.fixtureStartupDiagnosticsSource() + ';globalThis.compiled=__accountFetch', { loader: 'ts' }).code + ';compiled', { __accountFS: diskFull, __accountReports: root, __accountRole: 'ordinary', Buffer, URL, app: { getVersion: () => '1.0.1' }, Date });
    const untouched = new Response('still private');
    assert.equal(await unavailable({ fetch() { return Promise.resolve(untouched); } }, request, options), untouched); assert.equal(untouched.bodyUsed, false);
    await assert.rejects(unavailable(rejected, request, options), error => error === original);
  } finally { await rm(root, { recursive: true, force: true }); }
});

test('installed startup events retain the first pending request and cap writes before later noise', async () => {
  assert.equal(typeof fixtures.fixtureStartupDiagnosticsSource, 'function');
  const root = await realpath(await mkdtemp(join(tmpdir(), 'dilla-account-pending-events-')));
  try {
    const fs = createRequire(import.meta.url)('node:fs');
    const diagnostic = runInNewContext(createRequire(import.meta.url)('esbuild').transformSync(fixtures.fixtureStartupDiagnosticsSource() + ';globalThis.compiled={fetch:__accountFetch,emit:__accountEmit}', { loader: 'ts' }).code + ';compiled', { __accountFS: fs, __accountReports: root, __accountRole: 'ordinary', Buffer, URL, app: { getVersion: () => '1.0.1' }, Date });
    let reject;
    const pending = diagnostic.fetch({ fetch() { return new Promise((_, fail) => { reject = fail; }); } }, new Request('http://127.0.0.1/v1/instance'), { bypassCustomProtocolHandlers: true });
    let events = (await readFile(join(root, 'startup-1.0.1.jsonl'), 'utf8')).trim().split('\n').map(JSON.parse);
    assert.equal(events.length, 1); assert.equal(events[0].stage, 'fetch-begin'); assert.equal(events[0].route, 'instance');
    const aborted = new Error('net::ERR_ABORTED PRIVATE_SECRET'); reject(aborted); await assert.rejects(pending, e => e === aborted);
    for (let i = 0; i < 500; i++) diagnostic.emit('request-blocked', 'http://PRIVATE_SECRET/v1/instance', 'GET', 0, -20);
    const raw = await readFile(join(root, 'startup-1.0.1.jsonl'), 'utf8'); events = raw.trim().split('\n').map(JSON.parse);
    assert.equal(events.length, 96); assert.equal(events[1].stage, 'fetch-reject'); assert.equal(events[1].code, -3);
    assert.ok(Buffer.byteLength(raw) <= 32768); assert.doesNotMatch(raw, /PRIVATE_SECRET|http:/);
  } finally { await rm(root, { recursive: true, force: true }); }
});

test('startup snapshot reads only bounded regular event files and reconstructs fixed public fields', async () => {
  const root = await realpath(await mkdtemp(join(tmpdir(), 'dilla-account-event-reader-'))), reports = join(root, 'reports'), profile = join(root, 'profile');
  const input = { reports, profile, installed: join(root, 'missing'), platform: 'linux', expectedVersion: '1.0.1' };
  try {
    await mkdir(reports); await mkdir(profile);
    const path = join(reports, 'startup-1.0.1.jsonl');
    const event = { seq: 1, stage: 'fetch-begin', route: 'instance', method: 'GET', status: 0, code: 0, elapsedMs: 0, secret: 'PRIVATE_SECRET' };
    await writeFile(path, JSON.stringify(event) + '\n');
    let result = await installedAccountDiagnostics(input);
    assert.equal(result.startup[1].status, 'ok'); assert.equal(result.startup[1].records[0].route, 'instance');
    assert.doesNotMatch(JSON.stringify(result), /PRIVATE_SECRET|secret/);
    await writeFile(path, JSON.stringify({ ...event, route: 'http://PRIVATE_SECRET' }) + '\n');
    result = await installedAccountDiagnostics(input); assert.equal(result.startup[1].status, 'malformed'); assert.deepEqual(result.startup[1].records, []);
    await writeFile(path, '{PRIVATE_SECRET malformed'); result = await installedAccountDiagnostics(input); assert.equal(result.startup[1].status, 'malformed');
    await writeFile(path, 'PRIVATE_SECRET'.repeat(3000)); result = await installedAccountDiagnostics(input); assert.equal(result.startup[1].status, 'oversized');
    await rm(path); const secret = join(root, 'PRIVATE_SECRET'); await mkdir(secret); await symlink(secret, path, process.platform === 'win32' ? 'junction' : 'dir');
    result = await installedAccountDiagnostics(input); assert.equal(result.startup[1].status, 'unsafe'); assert.deepEqual(result.startup[1].records, []);
  } finally { await rm(root, { recursive: true, force: true }); }
});

test('actual installed main source records startup before requests without changing protocol options or production listeners', async () => {
  const source = await readFile(new URL('../../packages/desktop/src/main.ts', import.meta.url), 'utf8');
  const transformed = fixtureMainSource(source, { product: 'OwnedFixture', profile: '/fixture/profile', reports: '/fixture/reports', scheme: 'dilla-owned', version: '1.0.1' });
  assert.equal(transformed.split('__accountObserveSession(ses, instance);').length - 1, 1);
  assert.equal(transformed.split('__accountObserveWindow(win);').length - 1, 1);
  assert.ok(transformed.indexOf('__accountObserveSession(ses, instance);') < transformed.indexOf('await ses.protocol.handle('));
  for (const request of ['request', 'forwarded']) assert.ok(transformed.includes(`return __accountFetch(ses, ${request}, { bypassCustomProtocolHandlers: true });`));
  assert.equal(transformed.split('ses.webRequest.onBeforeRequest(').length, source.split('ses.webRequest.onBeforeRequest(').length);
  const require = createRequire(import.meta.url); assert.doesNotThrow(() => require('esbuild').transformSync(transformed, { loader: 'ts', format: 'cjs' }));
  assert.throws(() => fixtureMainSource(source.replace('const ses = session.fromPartition(partitionFor(instance));', 'const ses = changed();'), { product: 'OwnedFixture', profile: '/fixture/profile', reports: '/fixture/reports', scheme: 'dilla-owned', version: '1.0.1' }), /boundary changed/);
});

test('post-failure probes never start before failure and retain only fixed counts and numeric outcomes', async () => {
  const diagnostics = await import('../web/support/installed-diagnostics.ts');
  assert.equal(typeof diagnostics.installedPostFailureDiagnostics, 'function');
  const { createServer: httpServer } = createRequire(import.meta.url)('node:http');
  const root = await realpath(await mkdtemp(join(tmpdir(), 'dilla-after-failure-')));
  let begun = 7, ended = 7, probeRequests = 0, invalidCounterTime = false;
  const server = httpServer((request, response) => {
    probeRequests++;
    if (request.url === '/debug/instance-requests') response.end(JSON.stringify({ begun, ended, canceled: 0, lastBeginUnixMillis: invalidCounterTime ? 'PRIVATE_SECRET' : 1700000000000, lastEndUnixMillis: 1700000000001, secret: 'PRIVATE_SECRET' }));
    else if (request.url === '/healthz') response.end('PRIVATE_SECRET health body');
    else if (request.url === '/v1/instance') { begun++; ended++; response.end('PRIVATE_SECRET account body'); }
    else assert.fail('diagnostic chose an unexpected path');
  });
  try {
    await new Promise(done => server.listen(0, '127.0.0.1', done));
    const origin = `http://127.0.0.1:${server.address().port}/`;
    const input = { reports: root, instance: origin, control: origin, expectedVersion: '1.0.1', failureObserved: false };
    assert.deepEqual(await diagnostics.installedPostFailureDiagnostics(input), { format: 1, status: 'not-requested' }); assert.equal(probeRequests, 0);
    assert.deepEqual(await diagnostics.installedPostFailureDiagnostics({ ...input, instance: 'http://PRIVATE_SECRET/', failureObserved: true }), { format: 1, status: 'unsafe' }); assert.equal(probeRequests, 0);
    let watcher;
    watcher = setInterval(async () => {
      try { if ((await readFile(join(root, 'probe-request-1.0.1'), 'utf8')) !== '1') return; clearInterval(watcher);
        await writeFile(join(root, 'probe-result-1.0.1.json'), JSON.stringify({ format: 1, status: 'ok', httpStatus: 200, code: 0, elapsedMs: 3, secret: 'PRIVATE_SECRET' }));
      } catch { /* The marker has not been created yet. */ }
    }, 5);
    const result = await diagnostics.installedPostFailureDiagnostics({ ...input, failureObserved: true }); clearInterval(watcher);
    assert.equal(result.before.begun, 7); assert.equal(result.before.lastBeginUnixMillis, 1700000000000); assert.equal(result.before.lastEndUnixMillis, 1700000000001); assert.equal(result.after.begun, 8); assert.equal(result.after.ended, 8);
    assert.deepEqual(result.probes.map(p => [p.target, p.httpStatus]), [['health', 200], ['instance', 200]]); assert.equal(result.main.httpStatus, 200);
    assert.doesNotMatch(JSON.stringify(result), /PRIVATE_SECRET|secret|https?:|token|body|127\.0\.0\.1/);
    assert.equal(probeRequests, 4);
    invalidCounterTime = true; const malformed = await diagnostics.installedPostFailureDiagnostics({ ...input, failureObserved: true });
    assert.equal(malformed.before.status, 'malformed'); assert.equal(malformed.after.status, 'malformed'); assert.doesNotMatch(JSON.stringify(malformed), /PRIVATE_SECRET|lastBeginUnixMillis/);
  } finally { await new Promise(done => server.close(done)); await rm(root, { recursive: true, force: true }); }
});

test('post-failure main result rejects malformed oversized and symlinked private reports', async () => {
  const diagnostics = await import('../web/support/installed-diagnostics.ts');
  assert.equal(typeof diagnostics.readInstalledProbeResult, 'function');
  const root = await realpath(await mkdtemp(join(tmpdir(), 'dilla-after-failure-invalid-'))), path = join(root, 'probe-result-1.0.1.json');
  try {
    await writeFile(path, '{PRIVATE_SECRET'); assert.equal((await diagnostics.readInstalledProbeResult(root, '1.0.1')).status, 'malformed');
    await writeFile(path, 'PRIVATE_SECRET'.repeat(300)); assert.equal((await diagnostics.readInstalledProbeResult(root, '1.0.1')).status, 'oversized');
    await rm(path); const secret = join(root, 'PRIVATE_SECRET'); await mkdir(secret); await symlink(secret, path, process.platform === 'win32' ? 'junction' : 'dir');
    const result = await diagnostics.readInstalledProbeResult(root, '1.0.1'); assert.equal(result.status, 'unsafe'); assert.doesNotMatch(JSON.stringify(result), /PRIVATE_SECRET/);
  } finally { await rm(root, { recursive: true, force: true }); }
});

test('compiled main post-failure sentinel performs one bounded fixed-session operation without reading bodies', async () => {
  const root = await realpath(await mkdtemp(join(tmpdir(), 'dilla-main-probe-')));
  try {
    const fs = createRequire(import.meta.url)('node:fs'); let poll, calls = 0;
    const diagnostic = runInNewContext(createRequire(import.meta.url)('esbuild').transformSync(fixtures.fixtureStartupDiagnosticsSource() + ';globalThis.compiled=__accountObserveSession', { loader: 'ts' }).code + ';compiled', {
      __accountFS: fs, __accountReports: root, __accountRole: 'ordinary', Buffer, URL, AbortSignal, app: { getVersion: () => '1.0.1' }, Date,
      setInterval(callback) { poll = callback; return { unref() {} }; }, clearInterval() {}, setTimeout, clearTimeout,
    });
    let canceled = false; const response = new Response(new ReadableStream({ cancel() { canceled = true; } }));
    const session = { webRequest: { onCompleted() {}, onErrorOccurred() {} }, fetch(url, options) { assert.equal(this, session); assert.equal(url, 'http://127.0.0.1:12345/v1/instance'); assert.equal(options.bypassCustomProtocolHandlers, true); assert.equal(options.credentials, 'omit'); assert.equal(options.redirect, 'error'); assert.ok(options.signal); calls++; return Promise.resolve(response); } };
    diagnostic(session, 'http://127.0.0.1:12345/'); assert.equal(typeof poll, 'function'); poll(); assert.equal(calls, 0);
    await writeFile(join(root, 'probe-request-1.0.1'), 'PRIVATE_SECRET invalid marker'); poll(); assert.equal(calls, 0);
    await rm(join(root, 'probe-request-1.0.1')); const secret = join(root, 'PRIVATE_SECRET'); await mkdir(secret); await symlink(secret, join(root, 'probe-request-1.0.1'), process.platform === 'win32' ? 'junction' : 'dir'); poll(); assert.equal(calls, 0);
    await rm(join(root, 'probe-request-1.0.1')); await writeFile(join(root, 'probe-request-1.0.1'), '1'); poll(); await new Promise(done => setImmediate(done)); poll();
    assert.equal(calls, 1); assert.equal(canceled, true);
    const result = JSON.parse(await readFile(join(root, 'probe-result-1.0.1.json'), 'utf8')); assert.equal(result.httpStatus, 200); assert.doesNotMatch(JSON.stringify(result), /PRIVATE_SECRET|http:|body/);
  } finally { await rm(root, { recursive: true, force: true }); }
});

test('stalled post-failure probes are bounded and cannot replace the original failed assertion', async () => {
  const diagnostics = await import('../web/support/installed-diagnostics.ts');
  const { createServer: httpServer } = createRequire(import.meta.url)('node:http');
  const root = await realpath(await mkdtemp(join(tmpdir(), 'dilla-after-failure-stalled-')));
  const server = httpServer((request, response) => { if (request.url === '/debug/instance-requests') response.end('{"begun":0,"ended":0,"canceled":0,"lastBeginUnixMillis":0,"lastEndUnixMillis":0}'); });
  try {
    await new Promise(done => server.listen(0, '127.0.0.1', done)); const origin = `http://127.0.0.1:${server.address().port}/`;
    const original = new Error('original failed assertion'); let result;
    const started = Date.now();
    await assert.rejects((async () => { try { throw original; } finally { result = await diagnostics.installedPostFailureDiagnostics({ reports: root, instance: origin, control: origin, expectedVersion: '1.0.1', failureObserved: true }); } })(), error => error === original);
    assert.deepEqual(result.probes.map(probe => [probe.status, probe.code]), [['failed', -7], ['failed', -7]]);
    assert.equal(result.main.status, 'unavailable'); assert.ok(Date.now() - started < 8000, 'optional diagnostics must not wait indefinitely');
  } finally { server.closeAllConnections(); await new Promise(done => server.close(done)); await rm(root, { recursive: true, force: true }); }
});
