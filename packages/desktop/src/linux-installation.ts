import { createHash } from 'node:crypto';
import type { Stats } from 'node:fs';
import { updateFS } from './update-fs.ts';
// @ts-ignore Shared pure mapping is also consumed directly by release scripts.
import { linuxPackageVersion } from '../scripts/linux-package-version.mjs';

const ROOT = '/usr/lib/dilla/native';
const IDENTITY = ROOT + '/package.json';
const EXECUTABLE = ROOT + '/app/dilla-desktop';
const RESOURCES = ROOT + '/app/resources';
const MAX_IDENTITY = 1_048_576;
const MAX_FILE = 4 * 1024 ** 3;
export type LinuxInstallKind = 'appimage' | 'system-package' | 'unknown';
export interface NativePackageFile { path: string; size: number; sha512: string }
export interface NativePackageIdentity {
 format: 1; installKind: 'system-package'; manager: 'deb' | 'rpm' | 'alpm';
 upstreamVersion: string; packageVersion: string; arch: 'x64'; channel: 'stable' | 'beta';
 payload: NativePackageFile[];
 helper: { generation: number; compatibilityEpoch: 1; sandboxApi: 1; size: number; sha512: string };
}
export interface LinuxInstallationContext {
 /** Caller must supply the compiled build enum, never environment/argv/renderer state. */
 installKind: LinuxInstallKind; version: string; channel: 'stable' | 'beta'; arch: string;
 executable: string; resourcesPath: string;
}
export type LinuxInstallation = { installKind: 'system-package'; identity: NativePackageIdentity } | { installKind: 'appimage' };
function exact(value: unknown, keys: readonly string[]): value is Record<string, unknown> {
 return value !== null && typeof value === 'object' && !Array.isArray(value) && Object.keys(value).length === keys.length && Object.keys(value).every(key => keys.includes(key));
}
function digest(value: unknown): value is string {
 return typeof value === 'string' && /^[A-Za-z0-9+/]{86}==$/.test(value) && Buffer.from(value, 'base64').toString('base64') === value;
}
/** Reject ambiguous duplicate keys (including escaped names), excessive nesting,
 * malformed UTF-8 and oversized records before interpreting their identity. */
function strictJSON(raw: Uint8Array): unknown {
 if (!raw.byteLength || raw.byteLength > MAX_IDENTITY) throw new Error('Invalid native package identity size');
 const text = new TextDecoder('utf-8', { fatal: true }).decode(raw);
 let offset = 0;
 const space = () => { while (offset < text.length && /[\x20\t\r\n]/.test(text[offset])) offset++; };
 const string = (): string => {
  if (text[offset] !== '"') throw new Error('Invalid native package JSON string');
  const start = offset++;
  while (offset < text.length) {
   const token = text[offset++];
   if (token === '\\') offset++;
   else if (token === '"') return JSON.parse(text.slice(start, offset));
  }
  throw new Error('Invalid native package JSON string');
 };
 const value = (depth: number): void => {
  if (depth > 16) throw new Error('Native package JSON nesting limit');
  space();
  if (text[offset] === '{') {
   offset++; space(); if (text[offset] === '}') { offset++; return; }
   const keys = new Set<string>();
   for (;;) {
    space(); const key = string(); if (keys.has(key)) throw new Error('Duplicate native package JSON key'); keys.add(key);
    space(); if (text[offset++] !== ':') throw new Error('Invalid native package JSON');
    value(depth + 1); space(); const next = text[offset++];
    if (next === '}') return; if (next !== ',') throw new Error('Invalid native package JSON');
   }
  }
  if (text[offset] === '[') {
   offset++; space(); if (text[offset] === ']') { offset++; return; }
   for (;;) { value(depth + 1); space(); const next = text[offset++]; if (next === ']') return; if (next !== ',') throw new Error('Invalid native package JSON'); }
  }
  if (text[offset] === '"') { string(); return; }
  const start = offset; while (offset < text.length && !/[\s,\]}]/.test(text[offset])) offset++;
  if (offset === start) throw new Error('Invalid native package JSON');
  const token = text.slice(start, offset);
  // Check numeric spelling before JSON.parse can round a fractional token
  // into a safe integer. All numeric identity fields use unsigned integers.
  if (!['true', 'false', 'null'].includes(token) && !/^(0|[1-9]\d*)$/.test(token)) throw new Error('Invalid native package JSON integer');
 };
 value(0); space(); if (offset !== text.length) throw new Error('Invalid native package JSON');
 return JSON.parse(text);
}
function payloadPath(path: unknown): path is string {
 if (typeof path !== 'string' || path.length > 240) return false;
 if (path === 'chrome-sandbox' || path === 'package-maintainer') return true;
 return path.startsWith('app/') && path !== 'app/chrome-sandbox' && path.split('/').every(part => /^[A-Za-z0-9_@+.,-]+$/.test(part) && part !== '.' && part !== '..' && part.length <= 100);
}
export function parseNativePackageIdentity(raw: Uint8Array, context: Pick<LinuxInstallationContext, 'version' | 'channel' | 'arch'>): NativePackageIdentity {
 const doc = strictJSON(raw);
 const mapped = linuxPackageVersion(context.version, context.arch);
 if (!exact(doc, ['format', 'installKind', 'manager', 'upstreamVersion', 'packageVersion', 'arch', 'channel', 'payload', 'helper'])
  || doc.format !== 1 || doc.installKind !== 'system-package' || typeof doc.manager !== 'string' || !['deb', 'rpm', 'alpm'].includes(doc.manager)
  || doc.upstreamVersion !== context.version || doc.arch !== 'x64' || doc.arch !== context.arch || doc.channel !== context.channel || doc.channel !== mapped.channel) throw new Error('Invalid native package identity');
 const packageVersion = doc.manager === 'deb' ? mapped.deb.version : doc.manager === 'rpm' ? `${mapped.rpm.version}-${mapped.rpm.release}` : `${mapped.aur.pkgver}-${mapped.aur.pkgrel}`;
 if (doc.packageVersion !== packageVersion || !Array.isArray(doc.payload) || doc.payload.length < 4 || doc.payload.length > 16384) throw new Error('Invalid native package version or payload');
 let previous = '', total = 0;
 const names = new Set<string>();
 for (const file of doc.payload) {
  if (!exact(file, ['path', 'size', 'sha512']) || !payloadPath(file.path) || file.path <= previous || !Number.isSafeInteger(file.size)
   || Number(file.size) < 0 || Number(file.size) > MAX_FILE || !digest(file.sha512)) throw new Error('Invalid native package payload entry');
  const parts = file.path.split('/');
  for (let count = 1; count < parts.length; count++) if (names.has(parts.slice(0, count).join('/'))) throw new Error('Native package file/directory collision');
  names.add(file.path); previous = file.path; total += Number(file.size);
  if (total > 8 * 1024 ** 3) throw new Error('Native package payload size limit');
 }
 for (const required of ['app/dilla-desktop', 'app/resources/app.asar', 'chrome-sandbox', 'package-maintainer']) if (!doc.payload.some(file => file.path === required && file.size > 0)) throw new Error('Missing native package authority file');
 const helper = doc.helper;
 const helperFile = doc.payload.find(file => file.path === 'chrome-sandbox')!;
 if (!exact(helper, ['generation', 'compatibilityEpoch', 'sandboxApi', 'size', 'sha512']) || !Number.isSafeInteger(helper.generation) || Number(helper.generation) < 1
  || helper.compatibilityEpoch !== 1 || helper.sandboxApi !== 1 || !Number.isSafeInteger(helper.size) || Number(helper.size) < 1 || Number(helper.size) > 8 * 1024 ** 2
  || helper.size !== helperFile.size || !digest(helper.sha512) || helper.sha512 !== helperFile.sha512) throw new Error('Invalid native package helper tuple');
 return doc as unknown as NativePackageIdentity;
}
function protectedOwner(stat: Stats): void {
 // On Linux POSIX ACL effective named-user/group writes are bounded by the
 // group class mask reflected in st_mode. Exact non-writable modes reject them.
 if (stat.uid !== 0 || stat.gid !== 0) throw new Error('Native package path must be root-owned');
}
async function directory(path: string): Promise<void> {
 const stat = await updateFS.promises.lstat(path); protectedOwner(stat);
 if (stat.isSymbolicLink() || !stat.isDirectory() || (stat.mode & 0o7777) !== 0o755) throw new Error('Invalid native package directory protection');
}
function fileProtection(path: string, stat: Stats): void {
 protectedOwner(stat);
 const mode = stat.mode & 0o7777;
 if (stat.isSymbolicLink() || !stat.isFile() || stat.nlink !== 1 || !Number.isSafeInteger(stat.size) || stat.size < 0 || stat.size > MAX_FILE
  || (path === ROOT + '/chrome-sandbox' ? mode !== 0o4755 : path === EXECUTABLE || path === ROOT + '/package-maintainer' ? mode !== 0o755 : path === IDENTITY ? mode !== 0o644 : mode !== 0o644 && mode !== 0o755)) throw new Error('Invalid native package file protection');
}
async function readProtected(path: string, expected?: NativePackageFile): Promise<Buffer | undefined> {
 const before = await updateFS.promises.lstat(path); fileProtection(path, before);
 if (!expected && before.size > MAX_IDENTITY) throw new Error('Native package identity too large');
 const handle = await updateFS.promises.open(path, updateFS.constants.O_RDONLY | updateFS.constants.O_NOFOLLOW);
 try {
  const opened = await handle.stat(); fileProtection(path, opened);
  if (opened.dev !== before.dev || opened.ino !== before.ino || opened.size !== before.size || (expected && opened.size !== expected.size)) throw new Error('Native package file changed');
  const hash = createHash('sha512'), chunks: Buffer[] = [], chunk = Buffer.alloc(65536);
  let size = 0;
  for (;;) {
   const result = await handle.read(chunk, 0, chunk.length, null); if (!result.bytesRead) break;
   size += result.bytesRead;
   if (size > opened.size) throw new Error('Native package file grew during verification');
   hash.update(chunk.subarray(0, result.bytesRead)); if (!expected) chunks.push(Buffer.from(chunk.subarray(0, result.bytesRead)));
  }
  const after = await handle.stat(); fileProtection(path, after);
  if (size !== opened.size || after.size !== opened.size || after.mtimeMs !== opened.mtimeMs || after.ctimeMs !== opened.ctimeMs || (expected && hash.digest('base64') !== expected.sha512)) throw new Error('Native package payload differs from identity');
  return expected ? undefined : Buffer.concat(chunks);
 } finally { await handle.close(); }
}
/** Read-only ownership/integrity classification. This does not certify the native
 * receipt, generation floor, admission lock or actual sandbox startup (Task 2). */
export async function resolveLinuxInstallation(context: LinuxInstallationContext): Promise<LinuxInstallation> {
 if (context.installKind === 'appimage') {
  if (context.executable === EXECUTABLE || context.executable.startsWith(ROOT + '/') || context.resourcesPath.startsWith(ROOT + '/')) throw new Error('AppImage installation layout conflicts with native package');
  return { installKind: 'appimage' };
 }
 if (context.installKind !== 'system-package' || context.executable !== EXECUTABLE || context.resourcesPath !== RESOURCES) throw new Error('Unsupported Linux installation layout');
 for (const ancestor of ['/', '/usr', '/usr/lib', '/usr/lib/dilla', ROOT]) await directory(ancestor);
 const identity = parseNativePackageIdentity((await readProtected(IDENTITY))!, context);
 const files = new Map(identity.payload.map(file => [ROOT + '/' + file.path, file]));
 const seen = new Set<string>(); let adjacent = false;
 const walk = async (path: string): Promise<void> => {
  await directory(path);
  for (const name of await updateFS.promises.readdir(path)) {
   if (name.includes('/') || name === '.' || name === '..') throw new Error('Invalid native package directory entry');
   const child = path + '/' + name;
   if (child === IDENTITY) continue;
   const stat = await updateFS.promises.lstat(child);
   if (child === ROOT + '/app/chrome-sandbox') {
    protectedOwner(stat);
    if (!stat.isSymbolicLink() || await updateFS.promises.readlink(child) !== ROOT + '/chrome-sandbox') throw new Error('Invalid native adjacent sandbox helper');
    adjacent = true; continue;
   }
   if (stat.isDirectory() && !stat.isSymbolicLink()) {
    if (![...files.keys()].some(file => file.startsWith(child + '/'))) throw new Error('Unlisted native package directory');
    await walk(child);
   } else {
    const entry = files.get(child); if (!entry) throw new Error('Unlisted native package file');
    await readProtected(child, entry); seen.add(child);
   }
  }
 };
 await walk(ROOT);
 if (!adjacent || seen.size !== files.size) throw new Error('Incomplete native package payload');
 // Detect replacement of the identity during the full read-only verification.
 const finalIdentity = await readProtected(IDENTITY);
 if (!finalIdentity!.equals(Buffer.from(JSON.stringify(identity))) && JSON.stringify(parseNativePackageIdentity(finalIdentity!, context)) !== JSON.stringify(identity)) throw new Error('Native package identity changed');
 return { installKind: 'system-package', identity };
}
export type LinuxInstallationRole = 'check' | 'download' | 'install' | 'recovery' | 'watchdog' | 'sandbox-setup';
export function assertLinuxInstallationRole(installation: LinuxInstallation, role: LinuxInstallationRole): void {
 if (installation.installKind === 'system-package' && role === 'check') return;
 if (installation.installKind !== 'appimage') throw new Error('Update using your package manager');
}
