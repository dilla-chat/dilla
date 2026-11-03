import assert from 'node:assert/strict';
import test from 'node:test';
import { readFile, mkdtemp, mkdir, realpath, symlink, writeFile, rm } from 'node:fs/promises';
import { join, sep } from 'node:path';
import { tmpdir } from 'node:os';
import { generateKeyPairSync } from 'node:crypto';
import { createRequire } from 'node:module';
import { fileURLToPath } from 'node:url';
import { runInNewContext } from 'node:vm';
import { createServer } from 'node:net';
import { installedAccountDiagnostics } from '../web/support/installed-diagnostics.ts';
import { fixtureMainSource, INSTALLED_FUSES } from '../web/support/installed-packages.ts';
import * as fixtures from '../web/support/installed-packages.ts';

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

test('installed account packaging resolves temporary aliases before fixture cleanup', async () => {
  const parent = await realpath(await mkdtemp(join(tmpdir(), 'dilla-account-path-')));
  try {
    const physical = join(parent, 'physical'), alias = join(parent, 'alias'); await mkdir(physical);
    await symlink(physical, alias, process.platform === 'win32' ? 'junction' : 'dir');
    const root = await fixtures.createInstalledFixtureRoot(alias);
    assert.equal(root, await realpath(root));
    assert.ok(root.startsWith(physical + sep));
  } finally { await rm(parent, { recursive: true, force: true }); }
});

test('installed account automation retains production fuses and cannot enable Node inspection', async () => {
  assert.deepEqual(INSTALLED_FUSES, { runAsNode: false, enableCookieEncryption: true,
    enableNodeOptionsEnvironmentVariable: false, enableNodeCliInspectArguments: false,
    enableEmbeddedAsarIntegrityValidation: true, onlyLoadAppFromAsar: true, grantFileProtocolExtraPrivileges: false });
  const input = await readFile(new URL('../../packages/desktop/src/main.ts', import.meta.url), 'utf8');
  const compiled = fixtureMainSource(input, { product: 'DillaAccountFixture', profile: '/private/fixture-profile', reports: '/private/reports', scheme: 'dilla-account-fixture', version: '1.0.0' });
  assert.match(compiled, /remote-debugging-port/);
  assert.doesNotMatch(compiled, /--update-watchdog|--update-bootstrap/);
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
    // The installed fixture records around the actual production helper, so
    // tracing cannot accidentally restore the old ambient-cookie behavior.
    const { fetchInstanceData } = await import('../../packages/desktop/src/request-forwarding.ts'); let forwardedCalls = 0;
    const forwardingSession = { fetch(input, init) { assert.equal(this, forwardingSession); assert.equal(input, request); assert.deepEqual(init, { bypassCustomProtocolHandlers: true, credentials: 'omit', redirect: 'error', cache: 'no-store' }); forwardedCalls++; return Promise.resolve(response); } };
    assert.equal(await diagnostic.fetch(forwardingSession, request, undefined, () => fetchInstanceData(forwardingSession, request)), response); assert.equal(forwardedCalls, 1);
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
  for (const request of ['request', 'forwarded']) assert.ok(transformed.includes(`return __accountFetch(ses, ${request}, undefined, () => fetchInstanceData(ses, ${request}));`));
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

test('native launch fingerprints report exact owned bytes, modes and architecture without following unsafe files', async () => {
  const diagnostics = await import('../web/support/installed-launch-diagnostics.ts');
  const root = await realpath(await mkdtemp(join(tmpdir(), 'dilla-launch-image-'))), file = join(root, 'dilla-desktop');
  try {
    const bytes = Buffer.alloc(64); bytes.writeUInt32LE(0xfeedfacf, 0); bytes.writeUInt32LE(0x01000007, 4); bytes.write('PRIVATE_SECRET', 16);
    await writeFile(file, bytes, { mode: 0o700 });
    const image = await diagnostics.launchImageFingerprint(file);
    assert.equal(image.status, 'ok'); assert.equal(image.arch, 'x64'); assert.equal(image.size, 64);
    assert.equal(image.sha512, createRequire(import.meta.url)('node:crypto').createHash('sha512').update(bytes).digest('base64'));
    if (process.platform !== 'win32') assert.equal(image.executable, true);
    assert.doesNotMatch(JSON.stringify(image), /PRIVATE_SECRET|dilla-desktop|http:/);
    await rm(file); const directory = join(root, 'PRIVATE_SECRET'); await mkdir(directory); await symlink(directory, file, process.platform === 'win32' ? 'junction' : 'dir');
    assert.equal((await diagnostics.launchImageFingerprint(file)).status, 'unsafe');
  } finally { await rm(root, { recursive: true, force: true }); }
});

test('compiled fixture entry banner records before actual main imports and never swallows an import failure', async () => {
  const diagnostics = await import('../web/support/installed-launch-diagnostics.ts');
  const root = await realpath(await mkdtemp(join(tmpdir(), 'dilla-launch-entry-')));
  try {
    const source = await readFile(new URL('../../packages/desktop/src/main.ts', import.meta.url), 'utf8');
    const esbuild = createRequire(import.meta.url)('esbuild'), fs = createRequire(import.meta.url)('node:fs');
    const result = await esbuild.build({ stdin: { contents: source, loader: 'ts', resolveDir: fileURLToPath(new URL('../../packages/desktop/src/', import.meta.url)) }, bundle: true, platform: 'node', format: 'cjs', packages: 'external', write: false, banner: { js: diagnostics.installedEntrypointBanner(root, '1.0.1') } });
    const original = new Error('PRIVATE_SECRET require failure'), handlers = {};
    const childProcess = Object.create(process); childProcess.on = (name, fn) => { handlers[name] = fn; return childProcess; };
    assert.throws(() => runInNewContext(result.outputFiles[0].text, { process: childProcess, Buffer, Date, console, require(name) { if (name === 'electron') throw original; return createRequire(import.meta.url)(name); } }), e => e === original);
    let events = (await readFile(join(root, 'entry-1.0.1.jsonl'), 'utf8')).trim().split('\n').map(JSON.parse);
    assert.equal(events[0].stage, 'before-imports'); assert.equal(events[0].version, '1.0.1');
    assert.equal(handlers.uncaughtException, undefined); assert.equal(typeof handlers.uncaughtExceptionMonitor, 'function');
    original.code = 'MODULE_NOT_FOUND'; handlers.uncaughtExceptionMonitor(original); handlers.exit(1);
    events = (await readFile(join(root, 'entry-1.0.1.jsonl'), 'utf8')).trim().split('\n').map(JSON.parse);
    assert.equal(events[1].stage, 'uncaught'); assert.equal(events[1].code, 'MODULE_NOT_FOUND'); assert.equal(events[2].exitCode, 1);
    assert.doesNotMatch(JSON.stringify(events), /PRIVATE_SECRET|require failure|http:/);
    assert.equal(fs.statSync(join(root, 'entry-1.0.1.jsonl')).size < 8192, true);
  } finally { await rm(root, { recursive: true, force: true }); }
});

test('native crash and OS log decoders retain only exact owned fixture facts and fixed codes', async () => {
  const diagnostics = await import('../web/support/installed-launch-diagnostics.ts');
  const identity = { appId: 'dev.dilla.desktop.update-acceptance', product: 'DillaAccountAcceptance012345abcdef', installed: '/owned/private/Dilla.app/Contents/MacOS/dilla-desktop' };
  const crash = { pid: 123, procPath: identity.installed, bundleInfo: { CFBundleIdentifier: identity.appId }, exception: { type: 'EXC_CRASH', private: 'PRIVATE_SECRET' }, termination: { namespace: 'DYLD', code: 1, reasons: ['PRIVATE_SECRET Library missing'] }, threads: ['PRIVATE_SECRET'] };
  const record = diagnostics.parseOwnedCrash(JSON.stringify({ private: 'PRIVATE_SECRET' }) + '\n' + JSON.stringify(crash), identity);
  assert.deepEqual(record, { pid: 123, exception: 'EXC_CRASH', namespace: 'DYLD', code: 1 });
  assert.equal(diagnostics.parseOwnedCrash(JSON.stringify({ ...crash, procPath: '/unrelated/private' }), identity), null);
  const logs = diagnostics.parseOwnedLaunchLogs(JSON.stringify({ eventMessage: `Failed to relaunch bundle at ${identity.product} PRIVATE_SECRET OSStatus=-10810` }) + '\n' + JSON.stringify({ eventMessage: 'unrelated PRIVATE_SECRET' }), identity.product);
  assert.deepEqual(logs.records, [{ stage: 'relaunch-failed', code: -10810 }]);
  assert.doesNotMatch(JSON.stringify({ record, logs }), /PRIVATE_SECRET|owned\/private|http:|threads|eventMessage/);
  // Sparkle's installer names what it did and did not do under its own process names; only fixed labels and numbers remain.
  const sparkle = diagnostics.parseSparkleLogs([
    { timestamp: '2026-11-03 09:21:02.123456+0000', process: 'Autoupdate', messageType: 'Default', eventMessage: 'Installation of PRIVATE_SECRET finished; terminating target' },
    { timestamp: '2026-11-03 09:21:03.000000+0000', process: 'Autoupdate', messageType: 'Error', eventMessage: 'Failed to relaunch application at <private>: status=-600' },
    { timestamp: 'later', process: identity.product, messageType: 'Info', eventMessage: 'Invalidating XPC connection PRIVATE_SECRET' },
    { process: 'PRIVATE_SECRET other', messageType: 'Weird', eventMessage: 'unrelated PRIVATE_SECRET' },
  ].map(v => JSON.stringify(v)).join('\n'), identity.product);
  assert.deepEqual(sparkle, { status: 'ok', unknownMessages: 1, records: [
    { process: 'Autoupdate', type: 'Default', stage: 'termination', code: null, atMs: Date.parse('2026-11-03T09:21:02.123+00:00') },
    { process: 'Autoupdate', type: 'Error', stage: 'relaunch-failed', code: -600, atMs: Date.parse('2026-11-03T09:21:03.000+00:00') },
    { process: 'host', type: 'Info', stage: 'connection', code: null, atMs: null },
  ] });
  assert.doesNotMatch(JSON.stringify(sparkle), /PRIVATE_SECRET|private|eventMessage/);
});

test('entry monitor observes actual process import failure without handling it or changing its exit', async () => {
  const diagnostics = await import('../web/support/installed-launch-diagnostics.ts');
  const root = await realpath(await mkdtemp(join(tmpdir(), 'dilla-launch-exit-')));
  try {
    const file = join(root, 'fixture.cjs');
    await writeFile(file, diagnostics.installedEntrypointBanner(root, '1.0.1') + "\nrequire('dilla-fixture-intentionally-missing-PRIVATE_SECRET');");
    const child = createRequire(import.meta.url)('node:child_process').spawnSync(process.execPath, [file], { encoding: 'utf8', timeout: 5000 });
    assert.equal(child.status, 1); assert.match(child.stderr, /MODULE_NOT_FOUND/);
    const report = await diagnostics.readLaunchEntries(root, '1.0.1');
    assert.equal(report.status, 'ok'); assert.deepEqual(report.records.map(record => record.stage), ['before-imports', 'uncaught', 'exit']);
    assert.equal(report.records[1].code, 'MODULE_NOT_FOUND'); assert.equal(report.records[2].exitCode, 1);
    assert.doesNotMatch(JSON.stringify(report), /PRIVATE_SECRET|fixture\.cjs|Cannot find/);
  } finally { await rm(root, { recursive: true, force: true }); }
});

test('native launch reports reject malformed oversized and symlinked fixture entry files', async () => {
  const diagnostics = await import('../web/support/installed-launch-diagnostics.ts');
  const root = await realpath(await mkdtemp(join(tmpdir(), 'dilla-launch-private-'))), file = join(root, 'entry-1.0.1.jsonl');
  try {
    await writeFile(file, '{PRIVATE_SECRET'); assert.equal((await diagnostics.readLaunchEntries(root, '1.0.1')).status, 'malformed');
    await writeFile(file, 'PRIVATE_SECRET'.repeat(1000)); assert.equal((await diagnostics.readLaunchEntries(root, '1.0.1')).status, 'oversized');
    await rm(file); const directory = join(root, 'PRIVATE_SECRET'); await mkdir(directory); await symlink(directory, file, process.platform === 'win32' ? 'junction' : 'dir');
    const result = await diagnostics.readLaunchEntries(root, '1.0.1'); assert.equal(result.status, 'unsafe'); assert.deepEqual(result.records, []);
  } finally { await rm(root, { recursive: true, force: true }); }
});

test('failure-only native collector compares actual fixture images and discards unrelated OS evidence', async () => {
  const diagnostics = await import('../web/support/installed-launch-diagnostics.ts');
  const root = await realpath(await mkdtemp(join(tmpdir(), 'dilla-launch-collector-')));
  const product = 'DillaAccountAcceptance012345abcdef', app = join(root, product + '.app'), expectedApp = join(root, 'next.app'), reports = join(root, 'reports'), crashDirectory = join(root, 'crashes');
  const installed = join(app, 'Contents/MacOS/dilla-desktop'), expectedExecutable = join(expectedApp, 'Contents/MacOS/dilla-desktop');
  try {
    await mkdir(reports); await mkdir(crashDirectory);
    for (const bundle of [app, expectedApp]) {
      await mkdir(join(bundle, 'Contents/MacOS'), { recursive: true }); await mkdir(join(bundle, 'Contents/Resources'));
      await writeFile(join(bundle, 'Contents/MacOS/dilla-desktop'), Buffer.from('PRIVATE_SECRET executable'), { mode: 0o700 });
      await writeFile(join(bundle, 'Contents/Resources/app.asar'), 'PRIVATE_SECRET asar');
    }
    await writeFile(join(app, 'Contents/Info.plist'), '<key>CFBundleVersion</key><string>1.0.1</string><key>CFBundleExecutable</key><string>dilla-desktop</string><key>private</key><string>PRIVATE_SECRET</string>');
    const input = { installed, expectedExecutable, expectedApp, reports, product, appId: 'dev.dilla.desktop.update-acceptance', started: Date.now() - 1000, platform: 'darwin', failureObserved: true };
    const crash = { pid: 123, procPath: installed, bundleInfo: { CFBundleIdentifier: input.appId }, exception: { type: 'EXC_CRASH' }, termination: { namespace: 'DYLD', code: 1, reasons: ['PRIVATE_SECRET'] } };
    await writeFile(join(crashDirectory, 'dilla-desktop-owned.ips'), JSON.stringify(crash));
    await writeFile(join(crashDirectory, 'dilla-desktop-unrelated.ips'), JSON.stringify({ ...crash, procPath: '/unrelated/PRIVATE_SECRET' }));
    const calls = [];
    const runCommand = async (executable, args) => {
      calls.push([executable, args]);
      if (executable === '/bin/ps') return { status: 0, stdout: `123 ${process.getuid?.() ?? 0} S ${installed}\n124 0 R /unrelated/PRIVATE_SECRET`, stderr: '' };
      if (executable === '/usr/bin/log') return { status: 0, stdout: JSON.stringify({ eventMessage: `Failed to relaunch ${product} PRIVATE_SECRET OSStatus=-10810` }), stderr: '' };
      assert.equal(executable, '/usr/bin/codesign'); assert.equal(args.at(-1), app);
      return { status: 0, stdout: '', stderr: 'Signature=adhoc\nCDHash=' + 'ab'.repeat(20) + '\nAuthority=PRIVATE_SECRET\n' };
    };
    const dependencies = { runCommand, crashDirectory };
    assert.equal((await diagnostics.installedNativeLaunchDiagnostics({ ...input, failureObserved: false }, dependencies)).status, 'not-requested');
    assert.equal((await diagnostics.installedNativeLaunchDiagnostics({ ...input, product: 'unsafe PRIVATE_SECRET' }, dependencies)).status, 'unsafe');
    assert.equal(calls.length, 0, 'no process/log probe before the original assertion failed or for an unsafe identity');
    const report = await diagnostics.installedNativeLaunchDiagnostics(input, dependencies);
    assert.equal(report.status, 'ok'); assert.equal(report.executableMatches, true); assert.equal(report.asarMatches, true);
    assert.deepEqual(report.plist, { status: 'ok', version: '1.0.1', executableMatches: true });
    assert.equal(report.codeSignature.adHoc, true); assert.equal(report.codeSignature.cdHash, 'ab'.repeat(20));
    assert.deepEqual(report.crashes.records, [{ pid: 123, exception: 'EXC_CRASH', namespace: 'DYLD', code: 1 }]);
    assert.deepEqual(report.logs.records, [{ stage: 'relaunch-failed', code: -10810 }]);
    assert.equal(report.sparkle.commandStatus, 0);
    assert.deepEqual(report.sparkle.records, [{ process: 'other', type: 'other', stage: 'relaunch-failed', code: -10810, atMs: null }]);
    assert.equal(calls.length, 5); assert.ok(calls.every(([executable]) => ['/usr/bin/codesign', '/usr/bin/log', '/bin/ps'].includes(executable)));
    assert.ok(calls.some(([executable, args]) => executable === '/usr/bin/log' && args.at(-1).includes('org.sparkle-project.Sparkle')), 'the Sparkle processes are read by their own names');
    assert.doesNotMatch(JSON.stringify(report), /PRIVATE_SECRET|unrelated|procPath|eventMessage|Authority|dilla-launch-collector/);
    await writeFile(installed, 'different actual executable');
    assert.equal((await diagnostics.installedNativeLaunchDiagnostics(input, dependencies)).executableMatches, false);
  } finally { await rm(root, { recursive: true, force: true }); }
});

test('native launch instrumentation is compiled before main imports and shares the existing postfailure budget', async () => {
  const packages = await readFile(new URL('../web/support/installed-packages.ts', import.meta.url), 'utf8');
  const app = await readFile(new URL('../web/support/installed-app.ts', import.meta.url), 'utf8');
  assert.match(packages, /banner: \{ js: installedEntrypointBanner\(reports, version\) \}/);
  assert.match(app, /Promise\.all\(\[collect\('network'\), collect\('native'\)\]\)/);
  assert.equal(app.match(/setTimeout\(resolve, 10000\)/g)?.length, 1);
  assert.ok(app.indexOf("console.log('INSTALLED_STARTUP_DIAGNOSTICS '") < app.indexOf("collect('native')"));
  assert.match(app, /if \(info\.status !== info\.expectedStatus\)/);
  assert.match(app, /if \(!acceptingDiagnostics\) return/);
});

test('installed fixture passively reports the loaded renderer PID and effective security preferences', async () => {
  const root = await realpath(await mkdtemp(join(tmpdir(), 'dilla-runtime-report-')));
  try {
    const fs = createRequire(import.meta.url)('node:fs'), callbacks = {};
    const observe = runInNewContext(createRequire(import.meta.url)('esbuild').transformSync(fixtures.fixtureStartupDiagnosticsSource() + ';globalThis.observe=__accountObserveWindow', { loader: 'ts' }).code + ';observe', {
      __accountFS: fs, __accountReports: root, __accountRole: 'ordinary', Buffer, URL, Date,
      app: { getVersion: () => '1.0.1' }, process: { pid: 123, execPath: '/tmp/.mount_owned/dilla-desktop' },
    });
    const preferences = { sandbox: true, contextIsolation: true, nodeIntegration: false, private: 'PRIVATE_SECRET' };
    observe({ webContents: { on(event, cb) { callbacks[event] = cb; }, getOSProcessId: () => 456, getLastWebPreferences: () => preferences } });
    callbacks['did-finish-load']();
    const raw = await readFile(join(root, 'runtime-1.0.1.json'), 'utf8');
    assert.deepEqual(JSON.parse(raw), { format: 1, version: '1.0.1', mainPid: 123, rendererPid: 456, executable: '/tmp/.mount_owned/dilla-desktop', sandbox: true, contextIsolation: true, nodeIntegration: false });
    assert.doesNotMatch(raw, /PRIVATE_SECRET|private/);
    preferences.sandbox = false; callbacks['did-finish-load']();
    assert.equal(JSON.parse(await readFile(join(root, 'runtime-1.0.1.json'), 'utf8')).sandbox, false, 'reports effective state rather than a constant acceptance claim');
  } finally { await rm(root, { recursive: true, force: true }); }
});
