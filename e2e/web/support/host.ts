import { execFileSync, spawn, type ChildProcess } from 'node:child_process';
import { chmodSync, copyFileSync, createWriteStream, existsSync, mkdirSync, readFileSync, readdirSync, rmSync, statSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import type { FullConfig } from '@playwright/test';
import { parseInvite } from './driver.ts';
import { verifyShippingCoreWasmInDist } from './shipping-wasm.mjs';
import { verifyTestTools } from '../../../scripts/e2e-test-tools.mjs';
import { followWallClock, readFakeClock, wallClockFollower } from './wall-clock.ts';

/** web3.8 E2 (pre-flight C-13): `wire` is the one wire version the host's hello offers (`-wire`; 1 or more); the version-reload suite
 *  starts a host that offers a version unsupported by the browser bundle. `keepData` leaves the host data directory in place at start and at stop,
 *  so a second host on the same ports serves the same accounts (the caller removes it with `removeHostData`).
 *  L-E2E-31: `sfu` starts the host's in-process LiveKit (`-sfu -sfu-port -sfu-udp-port`); the voice suite sets it.
 *  `departureTimeout` (seconds, `-sfu-departure-timeout`) lengthens the rooms' departure timeout (task-10 fix review N3). */
export interface HostConfig { port: number; control: number; webRoot: string | null; webauthnOrigin?: string; wire?: number; keepData?: boolean; botRuntime?: boolean; prebuilt?: boolean; advanceClockWithWall?: boolean; adminInvite?: boolean; wallClockSweep?: boolean; sfu?: { port: number; udpPort: number; departureTimeout?: number } }
export const REPO_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..', '..', '..');
const TOOLING_DIR = process.env.DILLA_E2E_TOOLING_DIR;
const verifyPrebuiltTools = () => TOOLING_DIR === undefined ? undefined : verifyTestTools(TOOLING_DIR, { repoRoot: REPO_ROOT });
const prebuiltTools = verifyPrebuiltTools();
export const TESTHOST_BIN = prebuiltTools?.testhost ?? join(REPO_ROOT, 'target', process.platform === 'win32' ? 'dilla-testhost.exe' : 'dilla-testhost');
export const DILLABOT_BIN = prebuiltTools?.bot ?? join(REPO_ROOT, 'target', process.platform === 'win32' ? 'dillabot.exe' : 'dillabot');
const CARGO_TARGET_ROOT = resolve(REPO_ROOT, process.env.CARGO_TARGET_DIR ?? 'target');
export const BOT_HELPER_BIN = prebuiltTools?.botHelper ?? join(CARGO_TARGET_ROOT, 'release', process.platform === 'win32' ? 'dilla-bot-core.exe' : 'dilla-bot-core');
export const WASI_CORE = join(REPO_ROOT, 'internal', 'mlswasi', 'testdata', 'dilla_core_wasi.wasm');
const WAIT_MS = process.env.CI === 'true' ? 90_000 : 60_000;
const HOST_DATA_ROOT = process.env.DILLA_E2E_HOST_DATA_ROOT ?? '/tmp/dw';
/** The suite's Playwright outputDir (DILLA_E2E_OUTPUT_DIR, as the configs read it): the host and Bot logs go beside the traces. */
export const RESULTS_DIR = process.env.DILLA_E2E_OUTPUT_DIR ?? join(REPO_ROOT, 'e2e', 'test-results');
const hostDataDir = (port: number): string => join(HOST_DATA_ROOT, `host-${String(port)}`);

function ensureShippingCoreWasm(): void {
  const artifact = join(REPO_ROOT, 'packages', 'core-wasm', 'pkg', 'dilla_core_wasm_bg.wasm');
  if (process.env.CI === 'true' || prebuiltTools !== undefined) {
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
    // Bot v2 task 11: the host supervises the Bot as dillad does (a sandboxed child on <data-dir>/bot/gateway.sock, in
    // dev_same_user mode); the specs start and stop it through /debug/bot/… on the control listener.
    ...(c.botRuntime === true ? ['-bot-runtime-secret', botRuntimeToken(dataDir), '-bot', '-bot-executable', DILLABOT_BIN,
      '-bot-helper', BOT_HELPER_BIN, '-bot-wasi', WASI_CORE, '-bot-state-dir', botStateDir(dataDir)] : []),
    ...(c.adminInvite === true ? ['-admin-invite'] : []),
    ...(c.wire === undefined ? [] : [`-wire=${String(c.wire)}`]),
    ...(c.sfu === undefined ? [] : ['-sfu', '-sfu-port', String(c.sfu.port), '-sfu-udp-port', String(c.sfu.udpPort),
      ...(c.sfu.departureTimeout === undefined ? [] : ['-sfu-departure-timeout', String(c.sfu.departureTimeout)])]),
  ];
}

/** The supervised Bot's state directory, and its runtime credential inside it. */
const botStateDir = (dataDir: string): string => join(dataDir, 'bot-state');
const botRuntimeToken = (dataDir: string): string => join(botStateDir(dataDir), 'runtime.token');

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
 * <RESULTS_DIR>/testhost-<port>.log (e2e/test-results by default) with the invite redacted. The host starts empty: one instance, its bootstrap
 * invite, no account, no server, no channel (v0.1.0-web3.1's create-flow suite starts a second one of these).
 */
export async function startTestHost(c: HostConfig): Promise<StartedHost> {
  verifyPrebuiltTools();
  // Browser CI uses the web-build and rust-wasi artifacts from this commit.
  // Local runs rebuild both so acceptance cannot pass on stale UI or core code.
  // `prebuilt` (the ux world): the caller built the web client and the core already, so nothing here rebuilds them and two starts
  // in one tree cannot race on packages/web/dist or the wasm core.
  if (c.botRuntime === true && c.webRoot !== null && c.prebuilt !== true) {
    ensureShippingCoreWasm();
    if (process.env.CI !== 'true' && prebuiltTools === undefined) {
      rebuildShippingWasiCore();
      execFileSync(process.env.NPM ?? 'npm', ['run', 'build', '-w', '@dilla/web'], { cwd: REPO_ROOT, stdio: 'inherit' });
    }
  }
  const dataDir = hostDataDir(c.port);
  if (c.keepData !== true) rmSync(dataDir, { recursive: true, force: true });
  mkdirSync(HOST_DATA_ROOT, { recursive: true });
  mkdirSync(RESULTS_DIR, { recursive: true });
  // Appended, not truncated: tests in one file share a port, so a truncating open let the next test erase the log of the
  // one that failed (PR #121, bot-runtime.spec.ts device 2). Each start writes a header to tell the runs apart.
  const log = createWriteStream(join(RESULTS_DIR, `testhost-${c.port}.log`), { flags: 'a' });
  log.write(`=== testhost start ${new Date().toISOString()} ===\n`);
  // The Bot's binaries exist before the host starts: its supervisor checks them when it is built. Each later build is a
  // no-op.
  if (c.botRuntime === true) await prepareBotRuntimeBinaries();
  const host = spawn(TESTHOST_BIN, hostArgs(c, dataDir), { stdio: ['ignore', 'pipe', 'pipe'] });
  let botRunning = false;
  // The Bot logs into the host's output (it is the host's child): count its "bot service healthy" lines to tell when a
  // start has come up.
  const botHealthy = healthyCounter(host);
  let clockTicker: NodeJS.Timeout | null = null;
  let clockTickInFlight = false;
  const teardown = async () => {
    if (clockTicker !== null) clearInterval(clockTicker);
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
      const follower = followWallClock(clockURL, c.wallClockSweep !== false);
      clockTicker = setInterval(() => {
        if (clockTickInFlight) return;
        clockTickInFlight = true;
        void follower.tick().catch(() => undefined).finally(() => { clockTickInFlight = false; });
      }, 1_000);
    }
    const clockURL = c.advanceClockWithWall === true ? `http://127.0.0.1:${c.control}/debug/clock` : undefined;
    const startBot = async (): Promise<void> => {
      if (botRunning || c.botRuntime !== true) throw new Error('test host Bot runtime is already running or unavailable');
      await startBotRuntime(c.control, dataDir, c.port, botHealthy, clockURL);
      botRunning = true;
    };
    const stopBot = async (): Promise<void> => {
      if (!botRunning || c.botRuntime !== true) throw new Error('test host has no Bot runtime to stop');
      await postControl(c.control, '/debug/bot/stop', {});
      botRunning = false;
    };
    if (c.botRuntime === true) await startBot();
    const stopBotRuntime = async (): Promise<void> => {
      if (c.botRuntime !== true) throw new Error('test host has no Bot runtime');
      if (botRunning) await stopBot();
    };
    const startBotRuntimeNow = async (): Promise<void> => {
      if (c.botRuntime !== true) throw new Error('test host has no Bot runtime');
      if (!botRunning) await startBot();
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
        execFileSync('tar', ['-C', dataDir, '-czf', botArchive(name), '--', 'bot-state']);
        chmodSync(botArchive(name), 0o600);
      } finally {
        await startBot();
      }
    };
    const restoreBotRuntime = async (name: string): Promise<void> => {
      await stopBot();
      rmSync(botStateDir(dataDir), { recursive: true, force: true });
      execFileSync('tar', ['-C', dataDir, '-xzf', botArchive(name), '--no-same-owner']);
      await startBot();
    };
    const snapshotPair = async (name: string): Promise<void> => {
      await stopBot();
      try {
        execFileSync('tar', ['-C', dataDir, '-czf', botArchive(name), '--', 'bot-state']);
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
        rmSync(botStateDir(dataDir), { recursive: true, force: true });
        execFileSync('tar', ['-C', dataDir, '-xzf', botArchive(name), '--no-same-owner']);
      } finally {
        await startBot();
      }
    };
    return { url, control: `http://127.0.0.1:${c.control}`, invite,
      ...(c.botRuntime === true ? { runtimeCredentialFile: botRuntimeToken(dataDir) } : {}),
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
  if (verifyPrebuiltTools() !== undefined) {
    if (prebuiltTools?.bot === undefined || prebuiltTools.botHelper === undefined) throw new Error('Prebuilt Bot runtime tools are only available on Linux');
    return;
  }
  await runInherited(process.env.GO ?? 'go', ['-C', REPO_ROOT, 'build', '-o', DILLABOT_BIN, './cmd/dillabot']);
  await runInherited(process.env.CARGO ?? 'cargo', ['build', '-p', 'dilla-bot-core', '--release', '--locked']);
}

/** Counts "bot service healthy" lines in the host's output (the supervised Bot logs there), across chunk boundaries. */
interface HealthyCounter { count(): number }
function healthyCounter(host: ChildProcess): HealthyCounter {
  const needle = 'bot service healthy';
  let n = 0;
  let tail = '';
  const onData = (chunk: Buffer): void => {
    const text = tail + chunk.toString();
    let at = text.indexOf(needle);
    while (at !== -1) { n += 1; at = text.indexOf(needle, at + needle.length); }
    tail = text.slice(-(needle.length - 1));
    // A needle that ended inside the kept tail was counted already; drop it from the tail.
    if (tail.includes(needle)) tail = '';
  };
  host.stdout?.on('data', onData);
  host.stderr?.on('data', onData);
  return { count: () => n };
}

/** Starts the supervised Bot (POST /debug/bot/start) and waits for it to log "bot service healthy". The runtime
 *  credential is already in its state directory: the host wrote it (-bot-runtime-secret). The Bot reads no other file
 *  (Bot v2 task 14): no reconcile interval (it polls nothing) and built-in rate limits. */
async function startBotRuntime(control: number, dataDir: string, port: number, healthy: HealthyCounter, clockURL?: string): Promise<void> {
  await prepareBotRuntimeBinaries();
  if (clockURL !== undefined) await alignWallClock(clockURL, 'after Bot/helper build and before Bot installation');
  let helperInfo;
  try { helperInfo = statSync(BOT_HELPER_BIN); } catch {
    throw new Error(`${BOT_HELPER_BIN} is missing after cargo build -p dilla-bot-core --release --locked`);
  }
  if (!helperInfo.isFile() || (helperInfo.mode & 0o111) === 0) {
    throw new Error(`${BOT_HELPER_BIN} is not the executable emitted by cargo build -p dilla-bot-core --release --locked`);
  }
  const stateDir = botStateDir(dataDir);
  mkdirSync(stateDir, { recursive: true, mode: 0o700 });
  chmodSync(stateDir, 0o700);
  chmodSync(dataDir, 0o700);
  chmodSync(botRuntimeToken(dataDir), 0o600);
  const before = healthy.count();
  await postControl(control, '/debug/bot/start', {});
  const deadline = Date.now() + WAIT_MS;
  while (healthy.count() <= before) {
    if (Date.now() > deadline) {
      const status = await (await fetch(`http://127.0.0.1:${String(control)}/debug/bot/status`)).text().catch(() => 'unavailable');
      await postControl(control, '/debug/bot/stop', {}).catch(() => undefined);
      throw new Error(`the supervised Bot of test host ${port} did not become healthy within ${WAIT_MS} ms (status ${status}); see the host log`);
    }
    await new Promise((r) => setTimeout(r, 100));
  }
}

async function assertWallClock(clockURL: string, stage: string): Promise<void> {
  const now = await readFakeClock(clockURL, stage);
  if (Math.abs(now - Math.floor(Date.now() / 1000)) > 5) {
    throw new Error(`Bot host fake clock is outside the 5-second wall-clock bound ${stage}`);
  }
}

async function alignWallClock(clockURL: string, stage: string): Promise<void> {
  // Restoring a server snapshot restores its fake clock too, so catch up before starting Bot. The ticker's follower
  // does it and restarts its tick baseline, so the ticker never applies these seconds a second time.
  await wallClockFollower(clockURL).align(stage);
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
  const tools = verifyPrebuiltTools();
  if (tools === undefined) execFileSync(process.env.GO ?? 'go', ['-C', REPO_ROOT, 'build', '-o', TESTHOST_BIN, './cmd/dilla-testhost'], { stdio: 'inherit' });
  else process.env.DILLA_TESTKIT = tools.testkit;
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
