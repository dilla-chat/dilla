import { execFileSync, spawn, type ChildProcess } from 'node:child_process';
import { chmodSync, copyFileSync, createWriteStream, existsSync, mkdirSync, readFileSync, readdirSync, rmSync, statSync, writeFileSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import type { FullConfig } from '@playwright/test';
import { parseInvite } from './driver.ts';
import { verifyShippingCoreWasmInDist } from './shipping-wasm.mjs';
import { arr, decode } from '../../../packages/client-core/src/cbor/index';

/** web3.8 E2 (pre-flight C-13): `wire` is the one wire version the host's hello offers (`-wire`; 1 or more); the version-reload suite
 *  starts a host that offers a version unsupported by the browser bundle. `keepData` leaves the host data directory in place at start and at stop,
 *  so a second host on the same ports serves the same accounts (the caller removes it with `removeHostData`).
 *  L-E2E-31: `sfu` starts the host's in-process LiveKit (`-sfu -sfu-port -sfu-udp-port`); the voice suite sets it.
 *  `departureTimeout` (seconds, `-sfu-departure-timeout`) lengthens the rooms' departure timeout (task-10 fix review N3). */
export interface HostConfig { port: number; control: number; webRoot: string | null; webauthnOrigin?: string; wire?: number; keepData?: boolean; botRuntime?: boolean; botReconcileInterval?: string; advanceClockWithWall?: boolean; sfu?: { port: number; udpPort: number; departureTimeout?: number } }
export const REPO_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..', '..', '..');
export const TESTHOST_BIN = join(REPO_ROOT, 'target', 'dilla-testhost');
export const DILLABOT_BIN = join(REPO_ROOT, 'target', 'dillabot');
const CARGO_TARGET_ROOT = resolve(REPO_ROOT, process.env.CARGO_TARGET_DIR ?? 'target');
export const BOT_HELPER_BIN = join(CARGO_TARGET_ROOT, 'release', 'dilla-bot-core');
export const WASI_CORE = join(REPO_ROOT, 'internal', 'mlswasi', 'testdata', 'dilla_core_wasi.wasm');
const WAIT_MS = process.env.CI === 'true' ? 90_000 : 60_000;
const HOST_DATA_ROOT = process.env.DILLA_E2E_HOST_DATA_ROOT ?? '/tmp/dw';
const hostDataDir = (port: number): string => join(HOST_DATA_ROOT, `host-${String(port)}`);

function ensureShippingCoreWasm(): void {
  const artifact = join(REPO_ROOT, 'packages', 'core-wasm', 'pkg', 'dilla_core_wasm_bg.wasm');
  if (process.env.CI === 'true') {
    // CI downloads the verified dilla-web-dist artifact into packages/web/dist;
    // the wasm-pack source package is intentionally absent from that artifact.
    verifyShippingCoreWasmInDist(join(REPO_ROOT, 'packages', 'web', 'dist'));
    return;
  }
  const inputs = [join(REPO_ROOT, 'Cargo.toml'), join(REPO_ROOT, 'Cargo.lock')];
  for (const root of ['core/dilla-core', 'core/dilla-core-wasm']) {
    const visit = (dir: string): void => {
      for (const entry of readdirSync(dir, { withFileTypes: true })) {
        const path = join(dir, entry.name);
        if (entry.isDirectory()) visit(path);
        else if (entry.name === 'Cargo.toml' || entry.name.endsWith('.rs')) inputs.push(path);
      }
    };
    visit(join(REPO_ROOT, root));
  }
  const newestInput = Math.max(...inputs.map((path) => statSync(path).mtimeMs));
  if (existsSync(artifact) && statSync(artifact).mtimeMs >= newestInput) return;
  execFileSync(process.env.WASM_PACK ?? 'wasm-pack', [
    'build', 'core/dilla-core-wasm', '--target', 'web', '--profile', 'wasm-release', '--mode', 'no-install',
    '--out-dir', '../../packages/core-wasm/pkg',
  ], { cwd: REPO_ROOT, stdio: 'inherit' });
}

function rebuildShippingWasiCore(): void {
  // The Bot helper executes this module, so rebuild it from the same source snapshot as the browser core.
  execFileSync(process.env.CARGO ?? 'cargo', [
    'build', '-p', 'dilla-core-wasi', '--target', 'wasm32-wasip1', '--release', '--locked',
  ], { cwd: REPO_ROOT, stdio: 'inherit' });
  const targetDir = process.env.CARGO_TARGET_DIR ?? join(REPO_ROOT, 'target');
  copyFileSync(join(targetDir, 'wasm32-wasip1', 'release', 'dilla_core_wasi.wasm'), WASI_CORE);
}

export function hostArgs(c: HostConfig, dataDir: string): string[] {
  return ['-listen', `127.0.0.1:${c.port}`, '-control', `127.0.0.1:${c.control}`,
    '-core', WASI_CORE, '-log-level', 'warn', '-data-dir', dataDir, '-production-acl',
    ...(c.webRoot === null ? [] : ['-web-root', join(REPO_ROOT, c.webRoot)]),
    ...(c.webauthnOrigin === undefined ? [] : ['-webauthn-origin', c.webauthnOrigin]),
    ...(c.botRuntime === true ? ['-bot-runtime-secret', join(dataDir, 'bot-runtime.token')] : []),
    ...(c.wire === undefined ? [] : [`-wire=${String(c.wire)}`]),
    ...(c.sfu === undefined ? [] : ['-sfu', '-sfu-port', String(c.sfu.port), '-sfu-udp-port', String(c.sfu.udpPort),
      ...(c.sfu.departureTimeout === undefined ? [] : ['-sfu-departure-timeout', String(c.sfu.departureTimeout)])]),
  ];
}

export function removeHostData(port: number): void { rmSync(hostDataDir(port), { recursive: true, force: true }); }

function waitForLine(child: ChildProcess, pattern: RegExp, what: string, timeoutMs: number): Promise<string> {
  return new Promise((resolveLine, reject) => {
    let seen = '';
    const timer = setTimeout(() => reject(new Error(`${what} did not print ${pattern} within ${timeoutMs} ms:\n${seen}`)), timeoutMs);
    const onData = (chunk: Buffer) => {
      seen += chunk.toString();
      if (pattern.test(seen)) { clearTimeout(timer); resolveLine(seen); }
    };
    child.stdout?.on('data', onData);
    child.stderr?.on('data', (c: Buffer) => { seen += c.toString(); });
    child.once('exit', (code) => { clearTimeout(timer); reject(new Error(`${what} exited with ${code}:\n${seen}`)); });
  });
}

async function waitForHttp(url: string, timeoutMs: number): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    try { const r = await fetch(url); if (r.ok) return; } catch { /* not up yet */ }
    if (Date.now() > deadline) throw new Error(`${url} did not answer within ${timeoutMs} ms`);
    await new Promise((r) => setTimeout(r, 200));
  }
}

function stop(child: ChildProcess): Promise<void> {
  if (child.exitCode !== null) return Promise.resolve();
  return new Promise((done) => {
    const kill = setTimeout(() => child.kill('SIGKILL'), 10_000);
    child.once('exit', () => { clearTimeout(kill); done(); });
    child.kill('SIGTERM');
  });
}

/** A started test host: its public origin, its control origin, the bootstrap invite it printed, and its stop. */
export interface StartedHost { url: string; control: string; invite: string; runtimeCredentialFile?: string;
  stopBotRuntime(): Promise<void>; startBotRuntimeNow(): Promise<void>; restartBotRuntime(): Promise<void>;
  snapshotServer(name: string): Promise<void>; restoreServer(name: string): Promise<void>;
  snapshotBotRuntime(name: string): Promise<void>; restoreBotRuntime(name: string): Promise<void>;
  snapshotPair(name: string): Promise<void>; restorePair(name: string): Promise<void>; stop(): Promise<void> }

/**
 * Starts target/dilla-testhost (already built) with `c`, in a fresh data directory under `DILLA_E2E_HOST_DATA_ROOT`
 * (default `/tmp/dw`), logging to
 * e2e/test-results/testhost-<port>.log with the invite redacted. The host starts empty: one instance, its bootstrap
 * invite, no account, no server, no channel (v0.1.0-web3.1's create-flow suite starts a second one of these).
 */
export async function startTestHost(c: HostConfig): Promise<StartedHost> {
  // Browser CI uses the web-build and rust-wasi artifacts from this commit.
  // Local runs rebuild both so acceptance cannot pass on stale UI or core code.
  if (c.botRuntime === true && c.webRoot !== null) {
    ensureShippingCoreWasm();
    if (process.env.CI !== 'true') {
      rebuildShippingWasiCore();
      execFileSync(process.env.NPM ?? 'npm', ['run', 'build', '-w', '@dilla/web'], { cwd: REPO_ROOT, stdio: 'inherit' });
    }
  }
  const dataDir = hostDataDir(c.port);
  if (c.keepData !== true) rmSync(dataDir, { recursive: true, force: true });
  mkdirSync(HOST_DATA_ROOT, { recursive: true });
  mkdirSync(join(REPO_ROOT, 'e2e', 'test-results'), { recursive: true });
  // Appended, not truncated: tests in one file share a port, so a truncating open let the next test erase the log of the
  // one that failed (PR #121, bot-runtime.spec.ts device 2). Each start writes a header to tell the runs apart.
  const log = createWriteStream(join(REPO_ROOT, 'e2e', 'test-results', `testhost-${c.port}.log`), { flags: 'a' });
  log.write(`=== testhost start ${new Date().toISOString()} ===\n`);
  const host = spawn(TESTHOST_BIN, hostArgs(c, dataDir), { stdio: ['ignore', 'pipe', 'pipe'] });
  let bot: ChildProcess | null = null;
  let clockTicker: NodeJS.Timeout | null = null;
  let clockTickInFlight = false;
  const teardown = async () => {
    if (clockTicker !== null) clearInterval(clockTicker);
    if (bot !== null) await stop(bot);
    await stop(host);
    log.end();
    if (c.keepData !== true) rmSync(dataDir, { recursive: true, force: true });
  };
  host.stdout?.on('data', (b: Buffer) => log.write(b.toString().replace(/DILLA_TESTKIT_INVITE=\S+/g, 'DILLA_TESTKIT_INVITE=[redacted]')));
  host.stderr?.on('data', (b: Buffer) => log.write(b));
  try {
    const banner = await waitForLine(host, /DILLA_TESTKIT_INVITE=\S+/, 'dilla-testhost', WAIT_MS);
    const invite = parseInvite(banner);
    if (!invite) throw new Error(`dilla-testhost printed no DILLA_TESTKIT_INVITE= line:\n${banner}`);
    const url = `http://127.0.0.1:${c.port}`;
    await waitForHttp(url + '/', WAIT_MS);
    if (c.advanceClockWithWall === true) {
      // Native clients validate signed interaction snapshots against SystemTime. Keep this
      // host's otherwise manually advanced fake clock within the protocol's shared-clock skew.
      // Start ticking before the Bot/helper binaries are built, because those builds happen after
      // the test-host fake clock starts and can take tens of seconds.
      const clockURL = `http://127.0.0.1:${c.control}/debug/clock`;
      await assertWallClock(clockURL, 'before Bot runtime startup');
      // Timer callbacks can be delayed under CI load. Advance by elapsed wall
      // time so the fake host clock stays within the Bot's signed-time window.
      let advancedThrough = Date.now();
      clockTicker = setInterval(() => {
        if (clockTickInFlight) return;
        const seconds = Math.floor((Date.now() - advancedThrough) / 1_000);
        if (seconds < 1) return;
        clockTickInFlight = true;
        void fetch(clockURL, {
          method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ seconds }),
        }).then((response) => {
          if (response.ok) advancedThrough += seconds * 1_000;
        }).catch(() => undefined).finally(() => { clockTickInFlight = false; });
      }, 1_000);
    }
    if (c.botRuntime === true) bot = await startBotRuntime(url, dataDir, c.port, c.advanceClockWithWall === true ? `http://127.0.0.1:${c.control}/debug/clock` : undefined, c.botReconcileInterval);
    const stopBot = async (): Promise<void> => {
      if (bot === null || c.botRuntime !== true) throw new Error('test host has no Bot runtime to stop');
      await stop(bot);
      bot = null;
    };
    const startBot = async (): Promise<void> => {
      if (bot !== null || c.botRuntime !== true) throw new Error('test host Bot runtime is already running or unavailable');
      rmSync(join(dataDir, 'bot.toml'), { force: true });
      bot = await startBotRuntime(url, dataDir, c.port,
        c.advanceClockWithWall === true ? `http://127.0.0.1:${c.control}/debug/clock` : undefined, c.botReconcileInterval);
    };
    const stopBotRuntime = async (): Promise<void> => {
      if (c.botRuntime !== true) throw new Error('test host has no Bot runtime');
      if (bot !== null) await stopBot();
    };
    const startBotRuntimeNow = async (): Promise<void> => {
      if (c.botRuntime !== true) throw new Error('test host has no Bot runtime');
      if (bot === null) await startBot();
    };
    const restartBotRuntime = async (): Promise<void> => { await stopBot(); await startBot(); };
    const snapshotServer = async (name: string): Promise<void> => postControl(c.control, '/debug/snapshot', { name });
    const restoreServer = async (name: string): Promise<void> => postControl(c.control, '/debug/restore', { name });
    const botArchive = (name: string): string => {
      if (!/^[A-Za-z0-9_-]{1,64}$/u.test(name)) throw new Error(`invalid Bot archive name: ${name}`);
      const dir = join(dataDir, 'bot-archives');
      mkdirSync(dir, { recursive: true, mode: 0o700 });
      return join(dir, `${name}.tar.gz`);
    };
    const snapshotBotRuntime = async (name: string): Promise<void> => {
      await stopBot();
      try {
        execFileSync('tar', ['-C', dataDir, '-czf', botArchive(name), '--', 'bot.toml', 'bot-runtime.token', 'bot-state']);
        chmodSync(botArchive(name), 0o600);
      } finally {
        await startBot();
      }
    };
    const restoreBotRuntime = async (name: string): Promise<void> => {
      await stopBot();
      rmSync(join(dataDir, 'bot.toml'), { force: true });
      rmSync(join(dataDir, 'bot-runtime.token'), { force: true });
      rmSync(join(dataDir, 'bot-state'), { recursive: true, force: true });
      execFileSync('tar', ['-C', dataDir, '-xzf', botArchive(name), '--no-same-owner']);
      await startBot();
    };
    const snapshotPair = async (name: string): Promise<void> => {
      await stopBot();
      try {
        execFileSync('tar', ['-C', dataDir, '-czf', botArchive(name), '--', 'bot.toml', 'bot-runtime.token', 'bot-state']);
        chmodSync(botArchive(name), 0o600);
        await postControl(c.control, '/debug/snapshot', { name });
      } finally {
        await startBot();
      }
    };
    const restorePair = async (name: string): Promise<void> => {
      await stopBot();
      try {
        await postControl(c.control, '/debug/restore', { name });
        rmSync(join(dataDir, 'bot.toml'), { force: true });
        rmSync(join(dataDir, 'bot-runtime.token'), { force: true });
        rmSync(join(dataDir, 'bot-state'), { recursive: true, force: true });
        execFileSync('tar', ['-C', dataDir, '-xzf', botArchive(name), '--no-same-owner']);
      } finally {
        await startBot();
      }
    };
    return { url, control: `http://127.0.0.1:${c.control}`, invite,
      ...(c.botRuntime === true ? { runtimeCredentialFile: join(dataDir, 'bot-runtime.token') } : {}),
      stopBotRuntime, startBotRuntimeNow, restartBotRuntime, snapshotServer, restoreServer, snapshotBotRuntime, restoreBotRuntime,
      snapshotPair, restorePair, stop: teardown };
  } catch (e) { await teardown(); throw e; }
}

async function postControl(port: number, path: string, body: unknown): Promise<void> {
  const response = await fetch(`http://127.0.0.1:${String(port)}${path}`, {
    method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify(body),
  });
  if (response.status !== 204) throw new Error(`${path} returned HTTP ${response.status}: ${await response.text()}`);
}

// Suites with time-dependent behavior can warm these builds in a setup hook,
// giving cold compilation its own budget instead of consuming delivery time.
export async function prepareBotRuntimeBinaries(): Promise<void> {
  await runInherited(process.env.GO ?? 'go', ['-C', REPO_ROOT, 'build', '-o', DILLABOT_BIN, './cmd/dillabot']);
  await runInherited(process.env.CARGO ?? 'cargo', ['build', '-p', 'dilla-bot-core', '--release', '--locked']);
}

async function startBotRuntime(baseURL: string, dataDir: string, port: number, clockURL?: string, reconcileInterval = '1s'): Promise<ChildProcess> {
  const runtimeToken = join(dataDir, 'bot-runtime.token');
  const stateDir = join(dataDir, 'bot-state');
  const configPath = join(dataDir, 'bot.toml');
  const runtimeLogPath = join(REPO_ROOT, 'e2e', 'test-results', `dillabot-${port}.log`);
  const discoveryResponse = await fetch(`${baseURL}/v1/instance`, { headers: { accept: 'application/cbor' } });
  if (!discoveryResponse.ok) throw new Error(`bot test discovery failed: HTTP ${discoveryResponse.status}`);
  const instanceIdBytes = arr(decode(new Uint8Array(await discoveryResponse.arrayBuffer())))[3];
  if (!(instanceIdBytes instanceof Uint8Array) || instanceIdBytes.length !== 16) throw new Error('bot test discovery returned an invalid instance ID');
  const instanceId = Buffer.from(instanceIdBytes).toString('hex');

  await prepareBotRuntimeBinaries();
  if (clockURL !== undefined) await alignWallClock(clockURL, 'after Bot/helper build and before Bot installation');
  let helperInfo;
  try { helperInfo = statSync(BOT_HELPER_BIN); } catch {
    throw new Error(`${BOT_HELPER_BIN} is missing after cargo build -p dilla-bot-core --release --locked`);
  }
  if (!helperInfo.isFile() || (helperInfo.mode & 0o111) === 0) {
    throw new Error(`${BOT_HELPER_BIN} is not the executable emitted by cargo build -p dilla-bot-core --release --locked`);
  }
  mkdirSync(stateDir, { recursive: true, mode: 0o700 });
  chmodSync(dataDir, 0o700);
  const quote = (value: string): string => JSON.stringify(value);
  const config = [
    `base_url = ${quote(baseURL)}`,
    `instance_id = ${quote(instanceId)}`,
    `runtime_credential_file = ${quote(runtimeToken)}`,
    `helper_executable = ${quote(BOT_HELPER_BIN)}`,
    `wasi_module = ${quote(WASI_CORE)}`,
    `state_dir = ${quote(stateDir)}`,
    `reconcile_interval = ${quote(reconcileInterval)}`,
    'allow_loopback_http = true',
    // The test server does not meter; production pacing is covered by the Go tests.
    '[rate_limits]',
    'write_per_second = 1000000',
    'write_burst = 1000000',
    'read_per_second = 1000000',
    'read_burst = 1000000',
    'message_per_second = 1000000',
    'message_burst = 1000000',
    'commit_per_second = 1000000',
    'commit_burst = 1000000',
    'proposal_per_second = 1000000',
    'proposal_burst = 1000000',
    'keypackage_per_second = 1000000',
    'keypackage_burst = 1000000',
    '',
  ].join('\n');
  writeFileSync(configPath, config, { mode: 0o600, flag: 'wx' });
  chmodSync(runtimeToken, 0o600);
  // Appended like the host's log: a test that restarts the Bot, and the next test on this port, keep the earlier runs.
  const log = createWriteStream(runtimeLogPath, { flags: 'a' });
  log.write(`=== dillabot start ${new Date().toISOString()} ===\n`);
  const child = spawn(DILLABOT_BIN, ['-config', configPath], {
    stdio: ['ignore', 'pipe', 'pipe'], env: { ...process.env, DILLA_BOT_PRIVATE_DIAGNOSTICS: '1' },
  });
  child.stdout?.pipe(log);
  child.stderr?.pipe(log);
  try {
    await waitForBotHealthy(child, log, port);
    return child;
  } catch (error) {
    await stop(child);
    log.end();
    throw error;
  }
}

async function assertWallClock(clockURL: string, stage: string): Promise<void> {
  const response = await fetch(clockURL);
  if (!response.ok) throw new Error(`Bot host clock probe failed ${stage}: HTTP ${response.status}`);
  const now = (await response.json() as { now?: unknown }).now;
  if (typeof now !== 'number' || !Number.isSafeInteger(now) || Math.abs(now - Math.floor(Date.now() / 1000)) > 5) {
    throw new Error(`Bot host fake clock is outside the 5-second wall-clock bound ${stage}`);
  }
}

async function alignWallClock(clockURL: string, stage: string): Promise<void> {
  // Restoring a server snapshot restores its fake clock too. The periodic wall
  // ticker cannot account for that rewind, so catch up before starting Bot.
  const response = await fetch(clockURL);
  if (!response.ok) throw new Error(`Bot host clock probe failed ${stage}: HTTP ${response.status}`);
  const now = (await response.json() as { now?: unknown }).now;
  if (typeof now !== 'number' || !Number.isSafeInteger(now)) throw new Error(`Bot host clock probe returned invalid time ${stage}`);
  const seconds = Math.floor(Date.now() / 1_000) - now;
  if (seconds > 0) {
    const advanced = await fetch(clockURL, {
      method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ seconds }),
    });
    if (!advanced.ok) throw new Error(`Bot host clock catch-up failed ${stage}: HTTP ${advanced.status}`);
  }
  await assertWallClock(clockURL, stage);
}

function runInherited(command: string, args: string[]): Promise<void> {
  return new Promise((resolveRun, rejectRun) => {
    const child = spawn(command, args, { cwd: REPO_ROOT, stdio: 'inherit' });
    child.once('error', rejectRun);
    child.once('close', (code, signal) => {
      if (code === 0) resolveRun();
      else rejectRun(new Error(`${command} ${args.join(' ')} exited with ${signal ?? code ?? 'unknown status'}`));
    });
  });
}

function waitForBotHealthy(child: ChildProcess, log: ReturnType<typeof createWriteStream>, port: number): Promise<void> {
  return new Promise((resolveHealthy, reject) => {
    let seen = '';
    const timer = setTimeout(() => reject(new Error(`dillabot for test host ${port} did not become healthy:\n${seen}`)), WAIT_MS);
    const onData = (chunk: Buffer): void => {
      seen += chunk.toString();
      if (seen.includes('bot service healthy')) {
        clearTimeout(timer);
        child.stdout?.off('data', onData);
        child.stderr?.off('data', onData);
        resolveHealthy();
      }
    };
    child.stdout?.on('data', onData);
    child.stderr?.on('data', onData);
    child.once('exit', (code) => {
      clearTimeout(timer);
      log.end();
      reject(new Error(`dillabot for test host ${port} exited with ${code}:\n${seen}`));
    });
  });
}

export default async function globalSetup(config: FullConfig): Promise<() => Promise<void>> {
  const c = config.metadata.dillaHost as HostConfig | undefined;
  if (!c || !Number.isInteger(c.port) || !Number.isInteger(c.control) ||
      (c.webRoot !== null && typeof c.webRoot !== 'string') ||
      (c.sfu !== undefined && (!Number.isInteger(c.sfu.port) || !Number.isInteger(c.sfu.udpPort)))) {
    throw new Error('playwright config metadata.dillaHost is not a HostConfig');
  }
  if (!existsSync(WASI_CORE)) throw new Error(`${WASI_CORE} is missing: cargo build -p dilla-core-wasi --target wasm32-wasip1 --release --locked, then copy it there`);
  if (c.webRoot !== null && !existsSync(join(REPO_ROOT, c.webRoot, 'index.html'))) {
    throw new Error(`${join(REPO_ROOT, c.webRoot)} has no index.html: build it first (npm run build:harness -w @dilla/client-core, or npm run build -w @dilla/web)`);
  }
  execFileSync(process.env.GO ?? 'go', ['-C', REPO_ROOT, 'build', '-o', TESTHOST_BIN, './cmd/dilla-testhost'], { stdio: 'inherit' });
  // The Bot binaries are built here, before any test, not by the first Bot test: a cold release build of dilla-bot-core
  // took 2m52s on a CI runner, inside that test's 180 s timeout (PR #121, moderation-evidence.spec.ts in a web shard).
  // Each later startBotRuntime rebuild is a no-op.
  if (config.metadata.prebuildBotRuntime === true) await prepareBotRuntimeBinaries();
  const host = await startTestHost(c);
  process.env.DILLA_TESTKIT_INVITE = host.invite;
  process.env.DILLA_TESTKIT_CONTROL = host.control;
  process.env.DILLA_TEST_HOST = host.url;
  return host.stop;
}
