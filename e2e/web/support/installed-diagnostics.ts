import { constants } from 'node:fs';
import { lstat, open, writeFile } from 'node:fs/promises';
import { request } from 'node:http';
import { createConnection } from 'node:net';
import { dirname, join } from 'node:path';

type Status = 'ok' | 'missing' | 'unsafe' | 'oversized' | 'malformed' | 'unavailable' | 'truncated' | 'not-applicable';
const versions = ['1.0.0', '1.0.1'] as const;
const phases = ['idle', 'checking', 'downloading', 'ready', 'installing', 'error'];
const roles = ['ordinary', 'watchdog', 'bootstrap'];
const startupStages = ['fetch-begin', 'fetch-response', 'fetch-reject', 'request-complete', 'request-error', 'request-blocked', 'document-loaded', 'load-failed', 'renderer-gone'];
const startupRoutes = ['instance', 'limits', 'session', 'factor', 'backup', 'device', 'account', 'other-api', 'asset-wasm', 'asset-script', 'asset-style', 'other-asset', 'other'];
const startupMethods = ['GET', 'POST', 'PUT', 'PATCH', 'DELETE', 'HEAD', 'OPTIONS', 'OTHER'];
interface StartupEvent { seq: number; stage: string; route: string; method: string; status: number; code: number; elapsedMs: number }
interface Input { reports: string; profile: string; installed: string; platform: string; expectedVersion: string; previousPort?: number }
interface PrivateRead { status: Status; present: boolean; text?: string }

/** Never follow a report symlink or read an unbounded/racing replacement. No
 * private file text, exception or pathname is returned by the public snapshot. */
async function boundedRegular(file: string, limit: number): Promise<PrivateRead> {
  let present = false;
  try {
    const before = await lstat(file); present = true;
    if (!before.isFile() || before.isSymbolicLink()) return { status: 'unsafe', present };
    if (before.size > limit) return { status: 'oversized', present };
    const handle = await open(file, constants.O_RDONLY | (constants.O_NOFOLLOW ?? 0) | (constants.O_NONBLOCK ?? 0));
    try {
      const opened = await handle.stat();
      if (!opened.isFile() || opened.ino !== before.ino || opened.dev !== before.dev) return { status: 'unsafe', present };
      if (opened.size > limit) return { status: 'oversized', present };
      const bytes = Buffer.alloc(limit + 1); let size = 0;
      while (size <= limit) {
        const read = await handle.read(bytes, size, bytes.length - size, size);
        if (!read.bytesRead) break;
        size += read.bytesRead;
      }
      return size > limit ? { status: 'oversized', present } : { status: 'ok', present, text: bytes.subarray(0, size).toString('utf8') };
    } finally { await handle.close(); }
  } catch (error) {
    const code = (error as NodeJS.ErrnoException).code;
    return { status: code === 'ENOENT' ? 'missing' : code === 'ELOOP' ? 'unsafe' : 'unavailable', present };
  }
}
function object(value: unknown): Record<string, unknown> | null { return value !== null && typeof value === 'object' && !Array.isArray(value) ? value as Record<string, unknown> : null; }
function version(value: unknown): value is typeof versions[number] { return versions.some(known => known === value); }
function alive(pid: number): boolean | null {
  try { process.kill(pid, 0); return true; }
  catch (error) { return (error as NodeJS.ErrnoException).code === 'ESRCH' ? false : null; }
}
async function exists(file: string, directory = false): Promise<boolean> {
  try { const stat = await lstat(file); return !stat.isSymbolicLink() && (directory ? stat.isDirectory() : stat.isFile()); }
  catch { return false; }
}
async function listening(port: number): Promise<boolean> {
  return new Promise(resolve => {
    const socket = createConnection({ host: '127.0.0.1', port });
    let settled = false;
    const done = (live: boolean) => { if (settled) return; settled = true; socket.destroy(); resolve(live); };
    socket.once('connect', () => done(true)); socket.once('error', () => done(false)); socket.setTimeout(400, () => done(false));
  });
}

/** Fixture-only, read-only failure evidence. All inputs are the harness's fixed
 * owned paths. Only local ordinals/enums/booleans cross into CI artifacts. */
export async function installedAccountDiagnostics(input: Input) {
  const [journal, rawUpdates, native, portFile, rawStartup] = await Promise.all([
    boundedRegular(join(input.reports, 'processes.jsonl'), 65536),
    Promise.all(versions.map(value => boundedRegular(join(input.reports, `update-${value}.json`), 16384))),
    boundedRegular(join(input.reports, 'native-error.json'), 16384),
    boundedRegular(join(input.profile, 'DevToolsActivePort'), 512),
    Promise.all(versions.map(value => boundedRegular(join(input.reports, `startup-${value}.jsonl`), 32768))),
  ]);
  const records: { pid: number; role: string; version: string; alive: boolean | null }[] = [];
  let processStatus = journal.status;
  if (journal.status === 'ok') {
    const lines = journal.text!.split('\n').filter(Boolean);
    if (lines.length > 64) processStatus = 'truncated';
    for (const line of lines.slice(-64)) {
      try {
        const value = object(JSON.parse(line));
        if (!value || !Number.isSafeInteger(value.pid) || Number(value.pid) <= 1 || Number(value.pid) > 0x7fffffff
          || typeof value.role !== 'string' || !roles.includes(value.role) || !version(value.version)) throw new Error();
        records.push({ pid: Number(value.pid), role: value.role, version: value.version, alive: alive(Number(value.pid)) });
      } catch { processStatus = 'malformed'; }
    }
  }
  const updates = rawUpdates.map((raw, i) => {
    let phase: string | null = null, status = raw.status;
    if (status === 'ok') {
      try {
        const value = object(JSON.parse(raw.text!)), state = object(value?.state);
        if (value?.version !== versions[i] || typeof state?.phase !== 'string' || !phases.includes(state.phase)) throw new Error();
        phase = state.phase;
      } catch { status = 'malformed'; }
    }
    return { version: versions[i], status, phase };
  });
  const startup = rawStartup.map((raw, i) => {
    const records: StartupEvent[] = []; let status = raw.status;
    if (status === 'ok') {
      const lines = raw.text!.split('\n').filter(Boolean); if (lines.length > 96) status = 'truncated';
      // Preserve the earliest startup operation; later abort/teardown noise may
      // not displace the first pending request or its observed outcome.
      for (const line of lines.slice(0, 96)) {
        try {
          const value = object(JSON.parse(line));
          const integer = (field: unknown, min: number, max: number): field is number => Number.isSafeInteger(field) && Number(field) >= min && Number(field) <= max;
          if (!value || !integer(value.seq, 1, 96) || typeof value.stage !== 'string' || !startupStages.includes(value.stage)
            || typeof value.route !== 'string' || !startupRoutes.includes(value.route) || typeof value.method !== 'string' || !startupMethods.includes(value.method)
            || !integer(value.status, 0, 599) || (value.status !== 0 && value.status < 100) || !integer(value.code, -2147483648, 2147483647)
            || !integer(value.elapsedMs, 0, 600000)) throw new Error();
          records.push({ seq: value.seq, stage: value.stage, route: value.route, method: value.method, status: value.status, code: value.code, elapsedMs: value.elapsedMs });
        } catch { status = 'malformed'; }
      }
    }
    return { version: versions[i], status, records };
  });
  const match = portFile.status === 'ok' ? /^([1-9][0-9]{0,4})\n\/devtools\/browser\/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\n?$/.exec(portFile.text!) : null;
  const port = match ? Number(match[1]) : 0, valid = port >= 1 && port <= 65535;
  const bundle = dirname(dirname(dirname(input.installed)));
  const plist = input.platform === 'darwin' ? await boundedRegular(join(bundle, 'Contents/Info.plist'), 131072) : { status: 'not-applicable' as Status, present: false };
  let bundleVersion: string | null = null, plistStatus = plist.status;
  if (plist.status === 'ok') {
    // The pinned fixture builder writes XML. Do not parse arbitrary XML/DTD or
    // copy any other plist field into a diagnostic.
    const found = /<key>CFBundleVersion<\/key>\s*<string>([^<]{1,64})<\/string>/.exec(plist.text!);
    if (found && version(found[1])) bundleVersion = found[1]; else plistStatus = 'malformed';
  }
  return { format: 1, candidateVersion: version(input.expectedVersion) ? input.expectedVersion : null,
    processes: { status: processStatus, records }, updates, startup, hasNativeError: native.present,
    installed: { bundleExists: input.platform === 'darwin' && await exists(bundle, true), executableExists: await exists(input.installed), bundleVersion, plistStatus },
    devTools: { present: portFile.present, valid, changed: valid && input.previousPort !== undefined && port !== input.previousPort,
      live: valid && await listening(port), status: portFile.status === 'ok' && !valid ? 'malformed' : portFile.status } };
}

interface ProbeOutcome { status: 'ok' | 'failed'; httpStatus: number; code: number; elapsedMs: number }
interface CounterSnapshot { status: Status; begun?: number; ended?: number; canceled?: number; lastBeginUnixMillis?: number; lastEndUnixMillis?: number }
function safeInteger(value: unknown, max: number): value is number { return Number.isSafeInteger(value) && Number(value) >= 0 && Number(value) <= max; }
function loopbackPort(value: unknown): number | null {
  try {
    if (typeof value !== 'string') return null;
    const url = new URL(value), port = Number(url.port);
    return url.protocol === 'http:' && url.hostname === '127.0.0.1' && url.pathname === '/' && !url.username && !url.password && !url.search && !url.hash
      && Number.isInteger(port) && port >= 1 && port <= 65535 ? port : null;
  } catch { return null; }
}

/** No network request is permitted until the harness observed the original
 * failure. Each request is bounded to two seconds and discards bodies, except
 * the fixed control endpoint's <=512-byte numeric counter document. */
async function fixedProbe(port: number, path: '/healthz' | '/v1/instance' | '/debug/instance-requests'): Promise<{ outcome: ProbeOutcome; counters?: CounterSnapshot }> {
  const started = Date.now();
  return new Promise(resolve => {
    let settled = false;
    const finish = (status: 'ok' | 'failed', httpStatus: number, code: number, counters?: CounterSnapshot) => {
      if (settled) return; settled = true; clearTimeout(timer);
      resolve({ outcome: { status, httpStatus, code, elapsedMs: Math.min(10000, Math.max(0, Date.now() - started)) }, ...(counters ? { counters } : {}) });
    };
    const req = request({ hostname: '127.0.0.1', port, path, method: 'GET', agent: false }, response => {
      const status = response.statusCode ?? 0;
      if (path !== '/debug/instance-requests') { finish('ok', status, 0); response.destroy(); return; }
      const chunks: Buffer[] = []; let bytes = 0;
      response.on('data', (chunk: Buffer) => {
        bytes += chunk.length;
        if (bytes > 512) { finish('failed', status, 0, { status: 'oversized' }); response.destroy(); return; }
        chunks.push(chunk);
      });
      response.once('error', () => finish('failed', status, 0, { status: 'unavailable' }));
      response.once('end', () => {
        try {
          const value = object(JSON.parse(Buffer.concat(chunks).toString('utf8')));
          if (status !== 200 || !value || !safeInteger(value.begun, Number.MAX_SAFE_INTEGER) || !safeInteger(value.ended, Number.MAX_SAFE_INTEGER) || !safeInteger(value.canceled, Number.MAX_SAFE_INTEGER) || !safeInteger(value.lastBeginUnixMillis, Number.MAX_SAFE_INTEGER) || !safeInteger(value.lastEndUnixMillis, Number.MAX_SAFE_INTEGER)) throw new Error();
          finish('ok', status, 0, { status: 'ok', begun: value.begun, ended: value.ended, canceled: value.canceled, lastBeginUnixMillis: value.lastBeginUnixMillis, lastEndUnixMillis: value.lastEndUnixMillis });
        } catch { finish('failed', status, 0, { status: 'malformed' }); }
      });
    });
    const timer = setTimeout(() => { finish('failed', 0, -7); req.destroy(); }, 2000);
    req.once('error', error => {
      const codes: Record<string, number> = { ECONNREFUSED: -102, ECONNRESET: -101, ETIMEDOUT: -7, ENOTFOUND: -105 };
      finish('failed', 0, codes[(error as NodeJS.ErrnoException).code ?? ''] ?? 0);
    });
    req.end();
  });
}

export async function readInstalledProbeResult(reports: string, expectedVersion: string): Promise<{ status: Status } | ProbeOutcome> {
  if (!version(expectedVersion)) return { status: 'unsafe' };
  const raw = await boundedRegular(join(reports, `probe-result-${expectedVersion}.json`), 2048);
  if (raw.status !== 'ok') return { status: raw.status };
  try {
    const value = object(JSON.parse(raw.text!));
    if (!value || value.format !== 1 || (value.status !== 'ok' && value.status !== 'failed') || !safeInteger(value.httpStatus, 599)
      || (value.httpStatus !== 0 && value.httpStatus < 100) || !Number.isSafeInteger(value.code) || Number(value.code) < -2147483648 || Number(value.code) > 2147483647
      || !safeInteger(value.elapsedMs, 10000)) throw new Error();
    return { status: value.status, httpStatus: value.httpStatus, code: Number(value.code), elapsedMs: value.elapsedMs };
  } catch { return { status: 'malformed' }; }
}

export async function installedPostFailureDiagnostics(input: { reports: string; instance: string; control: string | undefined; expectedVersion: string; failureObserved: boolean }) {
  if (input.failureObserved !== true) return { format: 1, status: 'not-requested' };
  const port = loopbackPort(input.instance), control = loopbackPort(input.control);
  if (port === null || control === null || !version(input.expectedVersion)) return { format: 1, status: 'unsafe' };
  const before = (await fixedProbe(control, '/debug/instance-requests')).counters ?? { status: 'unavailable' };
  let requested = false;
  try { await writeFile(join(input.reports, `probe-request-${input.expectedVersion}`), '1', { flag: 'wx', mode: 0o600 }); requested = true; } catch { /* No overwrite or symlink following. */ }
  const main = async () => {
    if (!requested) return { status: 'unavailable' as Status };
    const deadline = Date.now() + 2400;
    do {
      const result = await readInstalledProbeResult(input.reports, input.expectedVersion);
      if (result.status !== 'missing') return result;
      await new Promise(done => setTimeout(done, 50));
    } while (Date.now() < deadline);
    return { status: 'unavailable' as Status };
  };
  const [health, instance, mainResult] = await Promise.all([fixedProbe(port, '/healthz'), fixedProbe(port, '/v1/instance'), main()]);
  const after = (await fixedProbe(control, '/debug/instance-requests')).counters ?? { status: 'unavailable' };
  return { format: 1, status: 'ok', before, probes: [{ target: 'health', ...health.outcome }, { target: 'instance', ...instance.outcome }], main: mainResult, after };
}
