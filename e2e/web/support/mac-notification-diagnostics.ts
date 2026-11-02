import { spawnSync, type SpawnSyncOptionsWithStringEncoding, type SpawnSyncReturns } from 'node:child_process';
import { constants } from 'node:fs';
import { chmod, mkdtemp, open, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

/** Preserve Playwright's normal explicit-environment semantics, including unset keys. */
export function electronChildEnvironment(environment?: NodeJS.ProcessEnv): Record<string, string> | undefined {
 return environment === undefined ? undefined : Object.fromEntries(Object.entries(environment).filter((entry): entry is [string, string] => typeof entry[1] === 'string'));
}

const emptyNative = () => ({ authorization: 'not-observed' as 'not-observed' | 'granted' | 'denied' | 'error', authorizationErrorCode: null as number | null,
 scheduled: false, schedulingFailed: false, schedulingErrorCode: null as number | null });

/** Exact messages from Electron v45.2.1; never retain descriptions, notification IDs or arbitrary log lines. */
export function notificationLogFacts(input: string, previous = emptyNative()) {
 const facts = { ...previous };
 for (const line of input.split('\n')) {
  if (line.length > 2048) continue;
  const match = /^\[[\d:./ -]+:INFO:(notification_presenter_mac|cocoa_notification)\.mm(?:\(\d+\)|:\d+)\] (.*)$/.exec(line);
  if (!match) continue;
  const [, source, message] = match;
  const code = /\(UNErrorDomain error (\d{1,4})\.\)$/.exec(message);
  if (source === 'notification_presenter_mac') {
   if (/^Notification authorization granted: [01]$/.test(message)) {
    facts.authorization = message.endsWith('1') ? 'granted' : 'denied';
    facts.authorizationErrorCode = null;
   } else if (message.startsWith('Error requesting notification authorization: ')) {
    facts.authorization = 'error'; facts.authorizationErrorCode = code ? Number(code[1]) : null;
   }
  } else if (/^Error scheduling notification \([^()\r\n]{1,256}\) /.test(message)) {
   facts.schedulingFailed = true; facts.schedulingErrorCode = code ? Number(code[1]) : null;
  } else if (/^Notification scheduled \([^()\r\n]{1,256}\)$/.test(message)) facts.scheduled = true;
 }
 return facts;
}

type Run = (file: string, args: string[], options: SpawnSyncOptionsWithStringEncoding) => SpawnSyncReturns<string>;
const status = (result: SpawnSyncReturns<string> | undefined) => !result || result.error || result.signal || result.status === null ? 'unavailable' : result.status === 0 ? 'passed' : 'failed';
const identifier = (value: string | undefined) => value && /^[A-Za-z0-9][A-Za-z0-9.-]{0,199}$/.test(value) ? value : null;

export function macNotificationIdentity(runtime: { execPath: string; isPackaged: boolean }, run: Run = spawnSync) {
 // This fixture launches the stock npm Electron executable. Do not inspect arbitrary app paths.
 const valid = runtime.execPath.length <= 4096 && /^\/(?:[^/\x00-\x1f\x7f]+\/)*Electron\.app\/Contents\/MacOS\/Electron$/.test(runtime.execPath)
  && !runtime.execPath.split('/').some(segment => segment === '.' || segment === '..');
 const bundle = valid ? runtime.execPath.slice(0, -'/Contents/MacOS/Electron'.length) : null;
 const command = (file: string, args: string[]) => {
  if (!bundle) return undefined;
  try { return run(file, args, { shell: false, encoding: 'utf8', timeout: 5000, maxBuffer: 8192, windowsHide: true }); }
  catch { return undefined; }
 };
 const plist = command('/usr/bin/plutil', ['-extract', 'CFBundleIdentifier', 'raw', '-o', '-', `${bundle}/Contents/Info.plist`]);
 const display = command('/usr/bin/codesign', ['-d', '--verbose=4', bundle!]);
 const verify = command('/usr/bin/codesign', ['--verify', '--deep', '--strict', bundle!]);
 const publicFields = display && status(display) === 'passed' ? `${display.stdout}\n${display.stderr}` : '';
 return { execPath: valid ? runtime.execPath : null, isPackaged: typeof runtime.isPackaged === 'boolean' ? runtime.isPackaged : null,
  bundleId: status(plist) === 'passed' ? identifier(plist!.stdout.trim()) : null,
  signature: { display: status(display), verify: status(verify),
   kind: /^Signature=adhoc$/m.test(publicFields) ? 'adhoc' : /^Authority=.+$/m.test(publicFields) ? 'identity'
    : status(display) === 'failed' && /(?:^|: )code object is not signed at all\r?$/m.test(display!.stderr) ? 'unsigned' : 'unknown',
   identifier: identifier(/^Identifier=(.+)$/m.exec(publicFields)?.[1]),
   authorityPresent: /^Authority=.+$/m.test(publicFields), teamIdentifierPresent: /^TeamIdentifier=[A-Z0-9]{10}$/m.test(publicFields) } };
}

type Facts = { version: number; platform: 'darwin'; finished: boolean; logUnavailable: boolean; logTruncated: boolean;
 native: ReturnType<typeof emptyNative>; identity: ReturnType<typeof macNotificationIdentity> | null };

/** Opt-in CI evidence only. Raw input stays in a private temporary directory outside artifacts and is always removed. */
export async function prepareMacNotificationDiagnostic(options: { platform?: string; enabled?: boolean; environment?: NodeJS.ProcessEnv;
 report?: (facts: Facts) => void | Promise<void> } = {}) {
 if ((options.platform ?? process.platform) !== 'darwin' || !options.enabled) return undefined;
 const facts: Facts = { version: 1, platform: 'darwin', finished: false, logUnavailable: false, logTruncated: false, native: emptyNative(), identity: null };
 const environment = electronChildEnvironment(options.environment ?? process.env)!;
 let directory: string | undefined, logPath: string | undefined, offset = 0, pending = '', lastReport = '', finished = false;
 const report = async () => {
  const json = JSON.stringify(facts);
  if (json === lastReport) return;
  lastReport = json;
  try { await options.report?.(JSON.parse(json)); } catch { /* Diagnostics cannot replace the original test result. */ }
 };
 try {
  directory = await mkdtemp(join(tmpdir(), 'dilla-mac-notification-'));
  await chmod(directory, 0o700);
  logPath = join(directory, 'electron.log');
  await writeFile(logPath, '', { mode: 0o600 });
  // Electron deletes/reopens this file at preinit; the containing directory remains 0700 throughout.
  Object.assign(environment, { ELECTRON_DEBUG_NOTIFICATIONS: '1', ELECTRON_ENABLE_LOGGING: 'file', ELECTRON_LOG_FILE: logPath, ELECTRON_ALSO_LOG_TO_STDERR: '' });
 } catch { facts.logUnavailable = true; }
 const capture = async () => {
  if (logPath && !finished) {
   let file;
   try {
    file = await open(logPath, constants.O_RDONLY | constants.O_NOFOLLOW);
    const metadata = await file.stat();
    if (!metadata.isFile()) throw new Error('Diagnostic input is not a regular file');
    await file.chmod(0o600);
    const size = metadata.size, limit = 8 * 1024 * 1024;
    if (size > limit) facts.logTruncated = true;
    const length = Math.max(0, Math.min(size, limit) - offset);
    if (length) {
     const buffer = Buffer.alloc(length);
     const { bytesRead } = await file.read(buffer, 0, length, offset);
     offset += bytesRead;
     const text = pending + buffer.subarray(0, bytesRead).toString('utf8'), end = text.lastIndexOf('\n');
     facts.native = notificationLogFacts(text.slice(0, end + 1), facts.native);
     pending = end < text.length - 2049 ? '' : text.slice(end + 1);
    }
   } catch { facts.logUnavailable = true; }
   finally { try { await file?.close(); } catch { facts.logUnavailable = true; } }
  }
  await report();
 };
 // Serialize captures so context-close cleanup cannot race a periodic read or report.
 let current = Promise.resolve();
 const queuedCapture = () => current = current.then(capture).catch(() => { facts.logUnavailable = true; });
 const timer = setInterval(() => { void queuedCapture(); }, 500); timer.unref();
 await report();
 return { environment, capture: queuedCapture,
  async runtime(runtime: { execPath: string; isPackaged: boolean }) { facts.identity = macNotificationIdentity(runtime); await queuedCapture(); },
  async finish() {
   if (finished) return;
   clearInterval(timer);
   try { await queuedCapture(); }
   finally {
    finished = true; facts.finished = true;
    try { if (directory) await rm(directory, { recursive: true, force: true }); } catch { facts.logUnavailable = true; }
    await report();
   }
  },
 };
}
