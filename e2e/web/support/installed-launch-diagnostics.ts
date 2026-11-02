import { constants } from 'node:fs';
import { lstat, open, opendir } from 'node:fs/promises';
import { createHash } from 'node:crypto';
import { execFile } from 'node:child_process';
import { basename, dirname, isAbsolute, join } from 'node:path';
import { homedir } from 'node:os';
import { boundedRegular } from './installed-diagnostics.ts';

const versions = ['1.0.0', '1.0.1'];
const codes = ['MODULE_NOT_FOUND', 'ERR_MODULE_NOT_FOUND', 'ERR_DLOPEN_FAILED', 'EACCES', 'ENOENT', 'EPERM', 'ENOMEM'];
const exceptions = ['Error', 'TypeError', 'SyntaxError', 'RangeError', 'ReferenceError', 'other'];
const validPid = (pid: unknown): pid is number => Number.isSafeInteger(pid) && Number(pid) > 1 && Number(pid) <= 0x7fffffff;
const integer = (value: unknown): value is number => Number.isSafeInteger(value) && Math.abs(Number(value)) <= 0x7fffffff;

/** Fixture-only esbuild banner: executes before Electron/native imports. The
 * monitor observes an uncaught exception without handling it or changing exit. */
export function installedEntrypointBanner(reports: string, version: string): string {
  if (!versions.includes(version) || !isAbsolute(reports)) throw new Error('Invalid fixture entry identity');
  return String.raw`;(()=>{const fs=process.getBuiltinModule('fs'),file=${JSON.stringify(join(reports, `entry-${version}.jsonl`))};let count=0;
const role=process.argv.some(v=>v.startsWith('--update-watchdog'))?'watchdog':process.argv.some(v=>v.startsWith('--update-bootstrap'))?'bootstrap':'ordinary';
const emit=(stage,code='other',exception='other',exitCode=null)=>{if(count++>=16)return;let fd;try{let before;try{before=fs.lstatSync(file);if(!before.isFile()||before.isSymbolicLink())return;}catch(e){if(e.code!=='ENOENT')return;}
fd=fs.openSync(file,fs.constants.O_WRONLY|fs.constants.O_APPEND|fs.constants.O_CREAT|(fs.constants.O_NOFOLLOW??0)|(fs.constants.O_NONBLOCK??0),0o600);const stat=fs.fstatSync(fd);if(!stat.isFile()||(before&&(stat.ino!==before.ino||stat.dev!==before.dev)))return;
const line=JSON.stringify({stage,pid:process.pid,version:${JSON.stringify(version)},role,time:Date.now(),code,exception,exitCode})+'\n';if(stat.size+Buffer.byteLength(line)<=8192)fs.writeSync(fd,line);}catch{}finally{if(fd!==undefined)try{fs.closeSync(fd);}catch{}}};
emit('before-imports');process.on('uncaughtExceptionMonitor',error=>{try{emit('uncaught',${JSON.stringify(codes)}.includes(error?.code)?error.code:'other',${JSON.stringify(exceptions)}.includes(error?.name)?error.name:'other');}catch{emit('uncaught');}});
process.on('exit',code=>emit('exit','other','other',Number.isInteger(code)&&code>=0&&code<=255?code:null));})();`;
}

export async function readLaunchEntries(reports: string, version: string) {
  const records: { stage: string; pid: number; version: string; role: string; time: number; code: string; exception: string; exitCode: number | null }[] = [];
  if (!versions.includes(version)) return { status: 'unsafe', records };
  const raw = await boundedRegular(join(reports, `entry-${version}.jsonl`), 8192);
  if (raw.status !== 'ok') return { status: raw.status, records };
  try {
    const lines = raw.text!.trim().split('\n').filter(Boolean);
    if (lines.length > 32) throw new Error();
    for (const line of lines) {
      const v = JSON.parse(line);
      if (!['before-imports', 'uncaught', 'exit'].includes(v.stage) || !validPid(v.pid) || v.version !== version
        || !['ordinary', 'watchdog', 'bootstrap'].includes(v.role) || !Number.isSafeInteger(v.time) || v.time < 0
        || ![...codes, 'other'].includes(v.code) || !exceptions.includes(v.exception)
        || !(v.exitCode === null || Number.isInteger(v.exitCode) && v.exitCode >= 0 && v.exitCode <= 255)) throw new Error();
      records.push({ stage: v.stage, pid: v.pid, version, role: v.role, time: v.time, code: v.code, exception: v.exception, exitCode: v.exitCode });
    }
    return { status: 'ok', records };
  } catch { return { status: 'malformed', records: [] }; }
}

/** Hash exact owned regular files without reading an unbounded or replaced
 * image. Only public program digests and numeric filesystem facts escape. */
export async function launchImageFingerprint(file: string) {
  try {
    const before = await lstat(file);
    if (!before.isFile() || before.isSymbolicLink()) return { status: 'unsafe' };
    if (before.size > 256 * 1024 * 1024) return { status: 'oversized' };
    const handle = await open(file, constants.O_RDONLY | (constants.O_NOFOLLOW ?? 0) | (constants.O_NONBLOCK ?? 0));
    try {
      const opened = await handle.stat();
      if (!opened.isFile() || opened.ino !== before.ino || opened.dev !== before.dev || opened.size !== before.size) return { status: 'unsafe' };
      const hash = createHash('sha512'), bytes = Buffer.alloc(65536); let offset = 0, arch = 'unknown';
      while (offset <= opened.size) {
        const read = await handle.read(bytes, 0, Math.min(bytes.length, opened.size - offset + 1), offset);
        if (!read.bytesRead) break;
        if (offset === 0 && read.bytesRead >= 8) {
          const magic = bytes.readUInt32LE(0), cpu = bytes.readUInt32LE(4);
          arch = magic === 0xfeedfacf ? cpu === 0x01000007 ? 'x64' : cpu === 0x0100000c ? 'arm64' : 'other' : [0xbebafeca, 0xbfbafeca].includes(magic) ? 'universal' : 'unknown';
        }
        offset += read.bytesRead; hash.update(bytes.subarray(0, read.bytesRead));
      }
      const after = await handle.stat();
      if (offset !== opened.size || after.size !== opened.size || after.mtimeMs !== opened.mtimeMs || after.ctimeMs !== opened.ctimeMs) return { status: 'unsafe' };
      return { status: 'ok', size: opened.size, mode: opened.mode & 0o7777, executable: (opened.mode & 0o111) !== 0,
        ownerMatches: typeof process.getuid === 'function' ? opened.uid === process.getuid() : null, sha512: hash.digest('base64'), arch };
    } finally { await handle.close(); }
  } catch (error) { return { status: (error as NodeJS.ErrnoException).code === 'ENOENT' ? 'missing' : 'unavailable' }; }
}

interface Identity { appId: string; product: string; installed: string }
export function parseOwnedCrash(text: string, identity: Identity) {
  if (Buffer.byteLength(text) > 1048576) return null;
  try {
    // Modern .ips files contain a one-line metadata JSON followed by report JSON.
    let v; try { v = JSON.parse(text); } catch { v = JSON.parse(text.slice(text.indexOf('\n') + 1)); }
    if (v.procPath !== identity.installed || v.bundleInfo?.CFBundleIdentifier !== identity.appId || !validPid(v.pid)) return null;
    const exception = ['EXC_CRASH', 'EXC_BAD_ACCESS', 'EXC_BAD_INSTRUCTION', 'EXC_BREAKPOINT', 'EXC_RESOURCE', 'EXC_GUARD', 'EXC_CORPSE_NOTIFY'].includes(v.exception?.type) ? v.exception.type as string : 'other';
    const namespace = ['DYLD', 'CODESIGNING', 'SIGNAL', 'TCC', 'RUNNINGBOARD', 'WATCHDOG', 'LIBSYSTEM'].includes(v.termination?.namespace) ? v.termination.namespace as string : 'other';
    return { pid: v.pid as number, exception, namespace, code: integer(v.termination?.code) ? v.termination.code as number : null };
  } catch { return null; }
}

export function parseOwnedLaunchLogs(text: string, product: string) {
  const records: { stage: string; code: number | null }[] = []; let unknownMessages = 0;
  if (!/^DillaAccountAcceptance[0-9a-f]{12}$/.test(product) || Buffer.byteLength(text) > 1048576) return { records, unknownMessages, status: 'unsafe' };
  for (const line of text.split('\n').slice(0, 2048)) {
    try {
      const v = JSON.parse(line), message = v.eventMessage;
      if (typeof message !== 'string' || message.length > 16384 || !message.includes(product)) continue;
      const stage = /failed to relaunch/i.test(message) ? 'relaunch-failed' : /failed to (?:launch|open)/i.test(message) ? 'launch-failed' : /(?:code.?sign|signature)/i.test(message) ? 'signature-issue' : null;
      if (!stage) { unknownMessages++; continue; }
      const token = /\bOSStatus\s*[=:]\s*(-?\d{1,10})\b/.exec(message), code = token ? Number(token[1]) : null;
      if (records.length < 16) records.push({ stage, code: integer(code) ? code : null });
    } catch { /* Unrecognized OS log text is never retained. */ }
  }
  return { status: 'ok', records, unknownMessages };
}

async function command(executable: string, args: string[]) {
  return new Promise<{ status: number | null; stdout: string; stderr: string }>(resolve => {
    execFile(executable, args, { timeout: 2000, killSignal: 'SIGKILL', maxBuffer: 1048576, encoding: 'utf8' }, (error, stdout, stderr) => {
      resolve({ status: error ? typeof error.code === 'number' ? error.code : null : 0, stdout, stderr });
    });
  });
}

async function crashReports(identity: Identity, started: number, directory: string) {
  const records: NonNullable<ReturnType<typeof parseOwnedCrash>>[] = [];
  let examined = 0;
  try {
    const stat = await lstat(directory);
    if (!stat.isDirectory() || stat.isSymbolicLink()) return { status: 'unsafe', examined, records };
    let seen = 0;
    for await (const entry of await opendir(directory)) {
      if (seen++ >= 512 || examined >= 8) break;
      if (!entry.name.endsWith('.ips') || !(entry.name.startsWith('dilla-desktop') || entry.name.startsWith(identity.product))) continue;
      const file = join(directory, entry.name), stat = await lstat(file);
      if (stat.mtimeMs < started || !stat.isFile() || stat.isSymbolicLink()) continue;
      examined++;
      const raw = await boundedRegular(file, 1048576);
      if (raw.status === 'ok') { const record = parseOwnedCrash(raw.text!, identity); if (record) records.push(record); }
    }
    return { status: 'ok', examined, records };
  } catch { return { status: 'unavailable', examined, records }; }
}

interface Input extends Identity { reports: string; expectedExecutable: string; expectedApp: string; started: number; platform: string; failureObserved: boolean }
/** Read-only and failure-only: no fallback launch, OS settings, or networking.
 * Runs alongside existing network probes within their SAME 10s teardown budget. */
/** Unit injection only; the native caller always uses the fixed OS commands and crash directory below. */
interface Dependencies { runCommand?: typeof command; crashDirectory?: string }
export async function installedNativeLaunchDiagnostics(input: Input, dependencies: Dependencies = {}) {
  if (input.failureObserved !== true) return { format: 1, status: 'not-requested' };
  if (input.platform !== 'darwin') return { format: 1, status: 'not-applicable' };
  const bundle = dirname(dirname(dirname(input.installed)));
  if (!/^DillaAccountAcceptance[0-9a-f]{12}$/.test(input.product) || input.appId !== 'dev.dilla.desktop.update-acceptance'
    || !isAbsolute(input.installed) || !isAbsolute(input.expectedApp) || !isAbsolute(input.expectedExecutable) || !isAbsolute(input.reports)
    || basename(bundle) !== input.product + '.app' || dirname(input.installed) !== join(bundle, 'Contents/MacOS')
    || dirname(input.expectedExecutable) !== join(input.expectedApp, 'Contents/MacOS') || !Number.isSafeInteger(input.started) || input.started <= 0) return { format: 1, status: 'unsafe' };
  const runCommand = dependencies.runCommand ?? command;
  const [installed, expected, installedAsar, expectedAsar, entries, verify, signature, processes, logs, crashes, plist] = await Promise.all([
    launchImageFingerprint(input.installed), launchImageFingerprint(input.expectedExecutable),
    launchImageFingerprint(join(bundle, 'Contents/Resources/app.asar')), launchImageFingerprint(join(input.expectedApp, 'Contents/Resources/app.asar')),
    Promise.all(versions.map(version => readLaunchEntries(input.reports, version))),
    runCommand('/usr/bin/codesign', ['--verify', '--deep', '--strict', bundle]), runCommand('/usr/bin/codesign', ['--display', '--verbose=4', bundle]),
    runCommand('/bin/ps', ['-A', '-ww', '-o', 'pid=,uid=,stat=,comm=']),
    runCommand('/usr/bin/log', ['show', '--style', 'ndjson', '--last', '10m', '--info', '--predicate', `eventMessage CONTAINS "${input.product}"`]),
    crashReports(input, input.started, dependencies.crashDirectory ?? join(homedir(), 'Library/Logs/DiagnosticReports')), boundedRegular(join(bundle, 'Contents/Info.plist'), 131072),
  ]);
  const ownedProcesses: { pid: number; state: string }[] = [];
  for (const line of processes.stdout.split('\n')) {
    const match = /^\s*(\d+)\s+(\d+)\s+(\S+)\s+(.+)$/.exec(line);
    if (!match || Number(match[2]) !== process.getuid?.() || match[4] !== input.installed || !validPid(Number(match[1]))) continue;
    if (ownedProcesses.length < 16) ownedProcesses.push({ pid: Number(match[1]), state: ['R', 'S', 'I', 'T', 'U', 'Z'].includes(match[3][0]) ? match[3][0] : 'other' });
  }
  const cdHash = /^CDHash=([0-9a-f]{40}|[0-9a-f]{64})$/mi.exec(signature.stderr)?.[1].toLowerCase() ?? null;
  const plistVersion = /<key>CFBundleVersion<\/key>\s*<string>([^<]{1,32})<\/string>/.exec(plist.text ?? '')?.[1];
  const declaredExecutable = /<key>CFBundleExecutable<\/key>\s*<string>([^<]{1,128})<\/string>/.exec(plist.text ?? '')?.[1];
  return { format: 1, status: 'ok', collectedAt: Date.now(), installed, expected, installedAsar, expectedAsar,
    executableMatches: installed.status === 'ok' && expected.status === 'ok' && installed.sha512 === expected.sha512,
    asarMatches: installedAsar.status === 'ok' && expectedAsar.status === 'ok' && installedAsar.sha512 === expectedAsar.sha512,
    plist: { status: plist.status, version: versions.includes(plistVersion ?? '') ? plistVersion : null, executableMatches: declaredExecutable === basename(input.expectedExecutable) },
    entries, codeSignature: { verifyStatus: verify.status, displayStatus: signature.status, adHoc: /^Signature=adhoc$/mi.test(signature.stderr), cdHash },
    processes: { status: processes.status, records: ownedProcesses }, crashes,
    logs: { commandStatus: logs.status, ...parseOwnedLaunchLogs(logs.stdout, input.product) } };
}
