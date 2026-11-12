import assert from 'node:assert/strict';
import { test } from 'node:test';
import { createHash } from 'node:crypto';
import { execFileSync } from 'node:child_process';
import { chmodSync, lstatSync, mkdirSync, mkdtempSync, readFileSync, rmSync, symlinkSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { pathToFileURL } from 'node:url';
import { createServer } from 'node:net';
import { build } from 'esbuild';

const repoRoot = resolve(import.meta.dirname, '..');
const sourceSha = execFileSync('git', ['rev-parse', 'HEAD'], { cwd: repoRoot, encoding: 'utf8' }).trim();
const env = { GITHUB_SHA: sourceSha, GITHUB_RUN_ID: '12345', GITHUB_RUN_ATTEMPT: '2' };
const moduleURL = new URL('./e2e-test-tools.mjs', import.meta.url);
const names = ['dilla-testhost', 'dilla-testkit', 'dillabot', 'dilla-bot-core', 'dilla-gpui-peer'];

function fixture(t, platform = 'linux', arch = 'x64') {
  const root = mkdtempSync(join(tmpdir(), 'dilla-e2e-tools-'));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const filenames = platform === 'linux' ? names : ['dilla-testhost', 'dilla-testkit'].map(name => name + (platform === 'win32' ? '.exe' : ''));
  const manifest = { v: 1, sourceSha, runId: '12345', producerAttempt: '1', platform, arch, files: {} };
  for (const name of filenames) {
    const bytes = Buffer.from(`test fixture ${name}`);
    writeFileSync(join(root, name), bytes, { mode: 0o755 });
    manifest.files[name] = { size: bytes.length, sha256: createHash('sha256').update(bytes).digest('hex') };
  }
  const save = () => writeFileSync(join(root, 'manifest.json'), JSON.stringify(manifest));
  save();
  return { root, manifest, save };
}

async function loadHost(t, tooling, extraEnv = {}, workspace = repoRoot) {
  const saved = { ...process.env };
  Object.assign(process.env, env, extraEnv);
  if (tooling) process.env.DILLA_E2E_TOOLING_DIR = tooling.root;
  else delete process.env.DILLA_E2E_TOOLING_DIR;
  t.after(() => { process.env = saved; });
  const hostPath = join(repoRoot, 'e2e/web/support/host.ts');
  const result = await build({ entryPoints: [hostPath], bundle: true, platform: 'node', format: 'esm', packages: 'external', write: false,
    define: { 'import.meta.url': JSON.stringify(pathToFileURL(join(workspace, 'e2e/web/support/host.ts')).href) },
  });
  const compiled = mkdtempSync(join(tmpdir(), 'dilla-host-module-'));
  t.after(() => rmSync(compiled, { recursive: true, force: true }));
  const output = join(compiled, 'host.mjs');
  writeFileSync(output, result.outputFiles[0].text);
  return import(pathToFileURL(output).href);
}

test('verified tooling selects bundle executables and repeated Bot preparation never compiles', { skip: process.platform !== 'linux' }, async t => {
  const tools = fixture(t);
  const host = await loadHost(t, tools, { GO: '/missing-go-must-not-run', CARGO: '/missing-cargo-must-not-run' });
  assert.equal(host.TESTHOST_BIN, join(tools.root, 'dilla-testhost'));
  assert.equal(host.DILLABOT_BIN, join(tools.root, 'dillabot'));
  assert.equal(host.BOT_HELPER_BIN, join(tools.root, 'dilla-bot-core'));
  await host.prepareBotRuntimeBinaries();
  await host.prepareBotRuntimeBinaries();
  writeFileSync(join(tools.root, 'dilla-bot-core'), 'tampered');
  await assert.rejects(host.prepareBotRuntimeBinaries(), /digest|hash|size/i);
});

test('global setup and Bot restarts launch verified tools without Go or Cargo', { skip: process.platform !== 'linux' }, async t => {
  const tools = fixture(t);
  const hostProgram = `#!${process.execPath}
const fs = require('node:fs');
const args = process.argv.slice(2), value = flag => args[args.indexOf(flag) + 1];
const token = args.includes('-bot-runtime-secret') && value('-bot-runtime-secret');
if (token) { fs.mkdirSync(require('node:path').dirname(token), { recursive: true }); fs.writeFileSync(token, 'fixture-secret', { mode: 0o600 }); }
require('node:http').createServer((req, res) => {
  res.end(req.url === '/v1/instance' ? Buffer.from([0x84,0xf6,0xf6,0xf6,0x50,...Array(16).fill(0)]) : 'ready');
}).listen(Number(value('-listen').split(':').pop()), '127.0.0.1', () => console.log('DILLA_TESTKIT_INVITE=fixture-invite'));
// The host supervises the Bot (-bot): the control listener's /debug/bot/start and /stop run the -bot-executable it was
// given, whose output is the host's own, as dilla-testhost's supervisor does.
let bot = null;
const stopBot = () => { if (bot) { bot.kill(); bot = null; } };
require('node:http').createServer((req, res) => {
  if (req.url === '/debug/bot/start' && args.includes('-bot')) {
    if (!bot) bot = require('node:child_process').spawn(value('-bot-executable'), [], { stdio: 'inherit' });
    res.statusCode = 204; return res.end();
  }
  if (req.url === '/debug/bot/stop') { stopBot(); res.statusCode = 204; return res.end(); }
  res.end(JSON.stringify({ running: bot !== null }));
}).listen(Number(value('-control').split(':').pop()), '127.0.0.1');
process.on('SIGTERM', () => { stopBot(); process.exit(0); });
`;
  writeFileSync(join(tools.root, 'dilla-testhost'), hostProgram);
  writeFileSync(join(tools.root, 'dillabot'), `#!${process.execPath}\nconsole.log('bot service healthy'); setInterval(() => {}, 1000);\n`);
  for (const name of ['dilla-testhost', 'dillabot']) {
    const bytes = readFileSync(join(tools.root, name));
    tools.manifest.files[name] = { size: bytes.length, sha256: createHash('sha256').update(bytes).digest('hex') };
  }
  tools.save();
  const workspace = mkdtempSync(join(tmpdir(), 'dilla-host-workspace-'));
  t.after(() => rmSync(workspace, { recursive: true, force: true }));
  writeFileSync(join(workspace, '.git'), 'gitdir: ' + execFileSync('git', ['rev-parse', '--absolute-git-dir'], { cwd: repoRoot, encoding: 'utf8' }).trim() + '\n');
  mkdirSync(join(workspace, 'internal/mlswasi/testdata'), { recursive: true });
  writeFileSync(join(workspace, 'internal/mlswasi/testdata/dilla_core_wasi.wasm'), 'fixture core; subprocess fixtures do not read it');
  const webRoot = join(workspace, 'packages/web/dist');
  mkdirSync(join(webRoot, 'assets'), { recursive: true });
  writeFileSync(join(webRoot, 'index.html'), 'fixture client');
  const wasm = Buffer.from([0, 97, 115, 109, 1, 0, 0, 0]);
  writeFileSync(join(webRoot, 'assets/dilla_core_wasm_bg-fixture.wasm'), wasm);
  writeFileSync(join(webRoot, 'dilla-manifest.json'), JSON.stringify({ v: 1, files: [{ path: 'assets/dilla_core_wasm_bg-fixture.wasm', size: 8, sha256: createHash('sha256').update(wasm).digest('hex') }] }));
  const host = await loadHost(t, tools, { CI: 'false', GO: '/missing-go-must-not-run', CARGO: '/missing-cargo-must-not-run', WASM_PACK: '/missing-wasm-pack-must-not-run', NPM: '/missing-npm-must-not-run', DILLA_E2E_HOST_DATA_ROOT: join(workspace, 'data') }, workspace);
  const server = createServer();
  await new Promise(done => server.listen(0, '127.0.0.1', done));
  const port = server.address().port;
  await new Promise(done => server.close(done));
  const config = { port, control: port + 1, webRoot: null };
  const stop = await host.default({ metadata: { dillaHost: config } });
  assert.equal(process.env.DILLA_TESTKIT, join(tools.root, 'dilla-testkit'));
  assert.equal(process.env.DILLA_TEST_HOST, `http://127.0.0.1:${port}`);
  await stop();
  const running = await host.startTestHost({ ...config, botRuntime: true, webRoot: 'packages/web/dist' });
  try {
    await running.restartBotRuntime();
    await running.stopBotRuntime();
    await running.startBotRuntimeNow();
    writeFileSync(join(tools.root, 'dilla-bot-core'), 'changed after startup');
    await assert.rejects(running.restartBotRuntime(), /digest|hash|size/i);
  } finally { await running.stop(); }
});

test('local Bot preparation still runs both real build commands when no bundle is configured', { skip: process.platform !== 'linux' }, async t => {
  const tools = fixture(t);
  const log = join(tools.root, 'builds.jsonl');
  const recorder = join(tools.root, 'compiler');
  writeFileSync(recorder, `#!${process.execPath}\nrequire('node:fs').appendFileSync(${JSON.stringify(log)}, JSON.stringify(process.argv.slice(2)) + '\\n');\n`, { mode: 0o755 });
  const host = await loadHost(t, undefined, { GO: recorder, CARGO: recorder });
  await host.prepareBotRuntimeBinaries();
  assert.deepEqual(readFileSync(log, 'utf8').trim().split('\n').map(JSON.parse), [
    ['-C', repoRoot, 'build', '-tags', 'dilla_testhost', '-o', join(repoRoot, 'target/dillabot'), './cmd/dillabot'],
    ['build', '-p', 'dilla-bot-core', '--release', '--locked'],
  ]);
});

test('bundle verification binds source, workflow run, platform, architecture and bytes', async t => {
  const { verifyTestTools } = await import(moduleURL);
  for (const [platform, arch] of [['linux', 'x64'], ['win32', 'x64'], ['darwin', 'x64'], ['darwin', 'arm64']]) {
    const tools = fixture(t, platform, arch);
    const options = { repoRoot, env, platform, arch };
    const paths = verifyTestTools(tools.root, options);
    assert.equal(paths.testhost, join(tools.root, platform === 'win32' ? 'dilla-testhost.exe' : 'dilla-testhost'));
    for (const [field, replacement, pattern] of [
      ['sourceSha', '0'.repeat(40), /source/i], ['runId', '999', /run/i], ['platform', 'wrong', /platform/i], ['arch', 'wrong', /arch/i],
    ]) {
      const original = tools.manifest[field]; tools.manifest[field] = replacement; tools.save();
      assert.throws(() => verifyTestTools(tools.root, options), pattern);
      tools.manifest[field] = original; tools.save();
    }
    const bin = paths.testhost;
    const original = readFileSync(bin);
    const corrupted = Buffer.from(original); corrupted[0] ^= 1;
    writeFileSync(bin, corrupted);
    assert.throws(() => verifyTestTools(tools.root, options), /digest|hash/i, 'same-length corruption must fail by digest');
    writeFileSync(bin, original);
    const file = tools.manifest.files[platform === 'win32' ? 'dilla-testhost.exe' : 'dilla-testhost'];
    file.size++; tools.save();
    assert.throws(() => verifyTestTools(tools.root, options), /size/i);
  }
});

test('missing files, extra paths, symlinks and absent run/source bindings fail closed', async t => {
  const { verifyTestTools } = await import(moduleURL);
  const options = { repoRoot, env, platform: 'linux', arch: 'x64' };
  for (const mutate of [
    tools => { tools.manifest.v = 999; tools.save(); },
    tools => writeFileSync(join(tools.root, 'manifest.json'), 'not json'),
    tools => rmSync(join(tools.root, 'manifest.json')),
    tools => rmSync(join(tools.root, 'dilla-testkit')),
    tools => { tools.manifest.files['../escape'] = tools.manifest.files['dilla-testkit']; tools.save(); },
    tools => { rmSync(join(tools.root, 'dilla-testkit')); symlinkSync(join(tools.root, 'dilla-testhost'), join(tools.root, 'dilla-testkit')); },
    tools => { rmSync(join(tools.root, 'manifest.json')); symlinkSync(join(tools.root, 'dilla-testhost'), join(tools.root, 'manifest.json')); },
  ]) {
    const tools = fixture(t); mutate(tools);
    assert.throws(() => verifyTestTools(tools.root, options));
  }
  const tools = fixture(t);
  assert.throws(() => verifyTestTools(tools.root, { ...options, env: {} }), /source|GITHUB_SHA/i);
  assert.throws(() => verifyTestTools(tools.root, { ...options, env: { GITHUB_SHA: sourceSha } }), /run/i);
  assert.throws(() => verifyTestTools(tools.root, { ...options, env: { ...env, GITHUB_SHA: '0'.repeat(40) } }), /checkout|source/i);
  const linked = join(tools.root, 'linked-root'); symlinkSync(tools.root, linked);
  assert.throws(() => verifyTestTools(linked, options), /symlink|directory/i);
});

test('verified downloads restore Unix executable permission and allow earlier producer attempts', async t => {
  const { verifyTestTools } = await import(moduleURL);
  const tools = fixture(t);
  chmodSync(join(tools.root, 'dilla-testkit'), 0o644);
  const paths = verifyTestTools(tools.root, { repoRoot, env, platform: 'linux', arch: 'x64' });
  assert.ok(lstatSync(paths.testkit).mode & 0o111);
  assert.equal(tools.manifest.producerAttempt, '1');
});

test('producer always builds current tooling, keeps Cargo feature scopes separate and publishes verifiable bytes', { skip: process.platform !== 'linux' }, async t => {
  const { buildTestTools, verifyTestTools } = await import(moduleURL);
  const tools = fixture(t);
  const log = join(tools.root, 'builds.jsonl');
  const compiler = join(tools.root, 'compiler');
  writeFileSync(compiler, `#!${process.execPath}
const fs = require('node:fs'), path = require('node:path'), args = process.argv.slice(2);
fs.appendFileSync(${JSON.stringify(log)}, JSON.stringify(args) + '\\n');
const output = args.includes('-o') ? args[args.indexOf('-o') + 1] : (args.includes('--example') ? path.join(process.env.CARGO_TARGET_DIR, 'debug', 'examples', args[args.indexOf('--example') + 1]) : path.join(process.env.CARGO_TARGET_DIR, 'release', args[args.indexOf('-p') + 1]));
fs.mkdirSync(path.dirname(output), { recursive: true }); fs.writeFileSync(output, 'compiled ' + args.join(' '), { mode: 0o755 });
`, { mode: 0o755 });
  const output = join(tools.root, 'bundle');
  const buildEnv = { ...process.env, ...env, GO: compiler, CARGO: compiler, CARGO_TARGET_DIR: join(tools.root, 'cargo-target') };
  const options = { repoRoot, env: buildEnv, platform: 'linux', arch: 'x64' };
  buildTestTools(output, options);
  const paths = verifyTestTools(output, options);
  assert.match(readFileSync(paths.testhost, 'utf8'), /compiled .*build/);
  assert.equal(readFileSync(paths.testkit, 'utf8'), 'compiled build -p dilla-testkit --release --locked');
  assert.equal(readFileSync(paths.botHelper, 'utf8'), 'compiled build -p dilla-bot-core --release --locked');
  const calls = readFileSync(log, 'utf8').trim().split('\n').map(JSON.parse);
  assert.deepEqual(calls, [
    ['-C', repoRoot, 'build', '-o', join(output, 'dilla-testhost'), './cmd/dilla-testhost'],
    ['build', '-p', 'dilla-testkit', '--release', '--locked'],
    ['-C', repoRoot, 'build', '-tags', 'dilla_testhost', '-o', join(output, 'dillabot'), './cmd/dillabot'],
    ['build', '-p', 'dilla-bot-core', '--release', '--locked'],
    ['build', '-p', 'dilla-client', '--example', 'peer', '--locked'],
  ]);
  assert.equal(readFileSync(paths.gpuiPeer, 'utf8'), 'compiled build -p dilla-client --example peer --locked');
  buildTestTools(output, options);
  assert.equal(readFileSync(log, 'utf8').trim().split('\n').length, 10, 'an existing bundle cannot bypass source builds');
});
