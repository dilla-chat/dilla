import { constants } from 'node:fs';
import { lstat, open } from 'node:fs/promises';
import { createConnection } from 'node:net';
import { dirname, join } from 'node:path';

type Status = 'ok' | 'missing' | 'unsafe' | 'oversized' | 'malformed' | 'unavailable' | 'truncated' | 'not-applicable';
const versions = ['1.0.0', '1.0.1'] as const;
const phases = ['idle', 'checking', 'downloading', 'ready', 'installing', 'error'];
const roles = ['ordinary', 'watchdog', 'bootstrap'];
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
  const [journal, rawUpdates, native, portFile] = await Promise.all([
    boundedRegular(join(input.reports, 'processes.jsonl'), 65536),
    Promise.all(versions.map(value => boundedRegular(join(input.reports, `update-${value}.json`), 16384))),
    boundedRegular(join(input.reports, 'native-error.json'), 16384),
    boundedRegular(join(input.profile, 'DevToolsActivePort'), 512),
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
    processes: { status: processStatus, records }, updates, hasNativeError: native.present,
    installed: { bundleExists: input.platform === 'darwin' && await exists(bundle, true), executableExists: await exists(input.installed), bundleVersion, plistStatus },
    devTools: { present: portFile.present, valid, changed: valid && input.previousPort !== undefined && port !== input.previousPort,
      live: valid && await listening(port), status: portFile.status === 'ok' && !valid ? 'malformed' : portFile.status } };
}
