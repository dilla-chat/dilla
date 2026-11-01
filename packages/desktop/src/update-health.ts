import { constants } from 'node:fs';
import { lstat, mkdir, open, realpath, rename, rm } from 'node:fs/promises';
import { randomBytes } from 'node:crypto';
import { dirname, isAbsolute, join, resolve } from 'node:path';
import { newerVersion } from './update.ts';
import { canonicalUpdateUserData } from './update-fs.ts';

export interface StartupHealth { directory: string; file: string; nonce: string; version: string }
interface LaunchedProcess { pid?: number; exitCode: number | null; signalCode: string | null }
interface HealthClock { now(): number; sleep(): Promise<void>; timeoutMs?: number }
const NONCE = /^[a-f0-9]{64}$/;
const GRACE_MS = 1000;
const defaults: HealthClock = { now: Date.now, sleep: () => new Promise(done => setTimeout(done, 100)), timeoutMs: 60_000 };

type ReportName = 'healthy.json' | 'watchdog-ready.json';
function pathFor(userData: string, nonce: string, name: ReportName = 'healthy.json'): string {
 if (!isAbsolute(userData) || !NONCE.test(nonce)) throw new Error('Invalid update health path');
 return join(resolve(userData), 'updates', `startup-${nonce}`, name);
}
function validVersion(version: string): void {
 if (typeof version !== 'string' || version.length > 64 || !newerVersion(version, '0.0.0')) throw new Error('Invalid update health version');
}
async function privateDirectory(path: string): Promise<void> {
 const info = await lstat(path);
 if (!info.isDirectory() || info.isSymbolicLink() || (process.platform !== 'win32'
  && ((info.mode & 0o077) !== 0 || info.uid !== process.getuid!()))) throw new Error('Invalid private update health directory');
}
async function checkPath(userData: string, file: string, nonce: string, name: ReportName = 'healthy.json'): Promise<void> {
 if (file !== pathFor(userData, nonce, name)) throw new Error('Invalid update health path');
 await privateDirectory(dirname(file));
 const root = await realpath(userData);
 if (await realpath(dirname(file)) !== dirname(pathFor(root, nonce, name))) throw new Error('Invalid private update health directory');
}

/** A private, single-launch acknowledgement. It carries no account data or keys. */
export async function createStartupHealth(userData: string, version: string): Promise<StartupHealth> {
 validVersion(version);
 userData = await canonicalUpdateUserData(userData);
 const nonce = randomBytes(32).toString('hex'), file = pathFor(userData, nonce), directory = dirname(file);
 await mkdir(dirname(directory), { recursive: true, mode: 0o700 });
 await privateDirectory(dirname(directory));
 await mkdir(directory, { mode: 0o700 });
 await checkPath(userData, file, nonce);
 return { nonce, file, directory, version };
}

/** Main reports only after its trusted bundled window and worker are usable.
 * Paths come from the update launcher environment, never renderer commands. */
async function reportHealth(userData: string, version: string, pid: number, file: string, nonce: string, name: ReportName): Promise<void> {
 validVersion(version);
 if (!Number.isSafeInteger(pid) || pid <= 1) throw new Error('Invalid update health process');
 userData = await canonicalUpdateUserData(userData);
 await checkPath(userData, file, nonce, name);
 const stage = file + '.tmp';
 let created = false;
 try {
  const handle = await open(stage, 'wx', 0o600); created = true;
  try { await handle.writeFile(JSON.stringify({ nonce, version, pid })); await handle.sync(); }
  finally { await handle.close(); }
  await rename(stage, file);
 } finally { if (created) await rm(stage, { force: true }); }
}

async function acknowledged(ticket: StartupHealth, pid: number): Promise<boolean> {
 let handle;
 try {
  const link = await lstat(ticket.file);
  if (!link.isFile() || link.isSymbolicLink()) throw new Error('Invalid update health report');
  handle = await open(ticket.file, constants.O_RDONLY | (constants.O_NOFOLLOW ?? 0));
  const info = await handle.stat();
  if (!info.isFile() || info.size > 4096 || (process.platform !== 'win32' && ((info.mode & 0o077) !== 0 || info.uid !== process.getuid!()))) throw new Error('Invalid update health report');
  const bytes = Buffer.alloc(4097), { bytesRead } = await handle.read(bytes, 0, bytes.length, 0);
  if (bytesRead > 4096) throw new Error('Invalid update health report');
  let record;
  try { record = JSON.parse(bytes.subarray(0, bytesRead).toString()); } catch { throw new Error('Invalid update health report'); }
  if (!record || Object.keys(record).length !== 3 || record.nonce !== ticket.nonce || record.version !== ticket.version || record.pid !== pid) throw new Error('Invalid update health report');
  return true;
 } catch (error) {
  if ((error as NodeJS.ErrnoException).code === 'ENOENT') return false;
  throw error;
 } finally { await handle?.close(); }
}

/** A successful spawn is insufficient. Require the exact launched process to
 * acknowledge the expected version and remain alive through a grace interval. */
async function waitForHealth(ticket: StartupHealth, child: LaunchedProcess, deps: HealthClock, name: ReportName): Promise<void> {
 const timeout = deps.timeoutMs ?? 60_000;
 if (!child.pid || child.pid <= 1 || !NONCE.test(ticket.nonce) || !Number.isFinite(timeout) || timeout < 1 || timeout > 300_000) throw new Error('Invalid update health launch');
 const deadline = deps.now() + timeout;
 let healthyAt: number | null = null;
 for (;;) {
  if (child.exitCode !== null || child.signalCode !== null) throw new Error('The updated app exited before startup completed');
  if (ticket.directory !== dirname(ticket.file)) throw new Error('Invalid update health directory');
  await checkPath(dirname(dirname(ticket.directory)), ticket.file, ticket.nonce, name);
  if (await acknowledged(ticket, child.pid)) healthyAt ??= deps.now();
  else healthyAt = null;
  await checkPath(dirname(dirname(ticket.directory)), ticket.file, ticket.nonce, name);
  if (child.exitCode !== null || child.signalCode !== null) throw new Error('The updated app exited before startup completed');
  if (healthyAt !== null && deps.now() - healthyAt >= GRACE_MS) return;
  if (deps.now() >= deadline) throw new Error('Updated app startup timed out');
  await deps.sleep();
 }
}

export async function reportStartupHealth(userData: string, version: string, pid: number, file: string, nonce: string): Promise<void> {
 await reportHealth(userData, version, pid, file, nonce, 'healthy.json');
}
export async function waitForStartupHealth(ticket: StartupHealth, child: LaunchedProcess, deps: HealthClock = defaults): Promise<void> {
 await waitForHealth(ticket, child, deps, 'healthy.json');
}
export async function reportWatchdogReady(userData: string, oldVersion: string, pid: number, nonce: string): Promise<void> {
 userData = await canonicalUpdateUserData(userData);
 await reportHealth(userData, oldVersion, pid, pathFor(userData, nonce, 'watchdog-ready.json'), nonce, 'watchdog-ready.json');
}
export async function waitForWatchdogReady(ticket: StartupHealth, oldVersion: string, child: LaunchedProcess, deps: HealthClock = defaults): Promise<void> {
 await waitForHealth({ ...ticket, file: pathFor(dirname(dirname(ticket.directory)), ticket.nonce, 'watchdog-ready.json'), version: oldVersion }, child, deps, 'watchdog-ready.json');
}

export async function withdrawStartupHealth(userData: string, nonce: string): Promise<void> {
 userData = await canonicalUpdateUserData(userData);
 const file = pathFor(userData, nonce); await checkPath(userData, file, nonce);
 await rm(file, { force: true });
}
export async function startupHealthFile(userData: string, nonce: string): Promise<string> {
 return pathFor(await canonicalUpdateUserData(userData), nonce);
}

/** Readiness discovery cannot supply paths; callers still verify process
 * ownership from its exact unpredictable watchdog command-line marker. */
export async function watchdogReadyPid(ticket: StartupHealth, oldVersion: string): Promise<number | null> {
 const file = pathFor(dirname(dirname(ticket.directory)), ticket.nonce, 'watchdog-ready.json');
 await checkPath(dirname(dirname(ticket.directory)), file, ticket.nonce, 'watchdog-ready.json');
 try {
  const entry = await lstat(file);
  if (!entry.isFile() || entry.isSymbolicLink()) throw new Error('Invalid watchdog readiness report');
  const handle = await open(file, constants.O_RDONLY | (constants.O_NOFOLLOW ?? 0));
  try {
   const info = await handle.stat();
   if (!info.isFile() || info.size > 4096 || (process.platform !== 'win32' && ((info.mode & 0o077) !== 0 || info.uid !== process.getuid!()))) throw new Error('Invalid watchdog readiness report');
   const bytes = Buffer.alloc(4097), { bytesRead } = await handle.read(bytes, 0, bytes.length, 0);
   if (bytesRead > 4096) throw new Error('Invalid watchdog readiness report');
   const value = JSON.parse(bytes.subarray(0, bytesRead).toString());
   if (!value || Object.keys(value).length !== 3 || value.nonce !== ticket.nonce || value.version !== oldVersion || !Number.isSafeInteger(value.pid) || value.pid <= 1) throw new Error('Invalid watchdog readiness report');
   return value.pid;
  } finally { await handle.close(); }
 } catch (error) { if ((error as NodeJS.ErrnoException).code === 'ENOENT') return null; throw error; }
}

export async function clearWatchdogReady(ticket: StartupHealth): Promise<void> {
 const root = dirname(dirname(ticket.directory)), file = pathFor(root, ticket.nonce, 'watchdog-ready.json');
 await checkPath(root, file, ticket.nonce, 'watchdog-ready.json'); await rm(file, { force: true });
}
