import { createHash } from 'node:crypto';
import { isAbsolute, join, resolve } from 'node:path';
import { updateFS } from './update-fs.ts';
export interface WindowsProgramFile { path: string; size: number; sha512: string }
export interface WindowsProgramManifest { format: 1; kind: 'dilla-desktop-program'; version: string; channel: 'stable' | 'beta'; platform: 'win32'; arch: 'x64'; files: WindowsProgramFile[] }
export interface WindowsManifestContext { version: string; channel: 'stable' | 'beta'; arch: 'x64' }
const LIMIT = 1_048_576, FILE_LIMIT = 4 * 1024 ** 3, TOTAL_LIMIT = 8 * 1024 ** 3;
export function canonicalSHA512(value: unknown): value is string {
 return typeof value === 'string' && /^[A-Za-z0-9+/]{86}==$/.test(value) && Buffer.from(value, 'base64').toString('base64') === value;
}
/** JSON.parse alone silently accepts duplicate object keys. These records share
 * one deliberately small grammar with the native Windows launcher. */
export function strictWindowsJSON(raw: Uint8Array): unknown {
 if (raw.byteLength > LIMIT) throw new Error('Windows manifest/record too large');
 const text = new TextDecoder('utf-8', { fatal: true }).decode(raw), parsed: unknown = JSON.parse(text);
 let offset = 0;
 const whitespace = () => { while (/\s/.test(text[offset] ?? '') && offset < text.length) offset++; };
 const string = (): string => {
  const start = offset++;
  while (offset < text.length) { const value = text[offset++]; if (value === '\\') offset++; else if (value === '"') return JSON.parse(text.slice(start, offset)) as string; }
  throw new Error('Invalid Windows manifest JSON');
 };
 const value = (depth: number): void => {
  if (depth > 64) throw new Error('Windows manifest JSON nesting limit');
  whitespace();
  if (text[offset] === '{') {
   offset++; whitespace(); const keys = new Set<string>(); if (text[offset] === '}') { offset++; return; }
   for (;;) {
    whitespace(); const key = string(); if (keys.has(key)) throw new Error('Duplicate Windows manifest JSON key'); keys.add(key);
    whitespace(); if (text[offset++] !== ':') throw new Error('Invalid Windows manifest JSON'); value(depth + 1); whitespace();
    const next = text[offset++]; if (next === '}') return; if (next !== ',') throw new Error('Invalid Windows manifest JSON');
   }
  }
  if (text[offset] === '[') {
   offset++; whitespace(); if (text[offset] === ']') { offset++; return; }
   for (;;) { value(depth + 1); whitespace(); const next = text[offset++]; if (next === ']') return; if (next !== ',') throw new Error('Invalid Windows manifest JSON'); }
  }
  if (text[offset] === '"') { string(); return; }
  while (offset < text.length && !/[\s,\]}]/.test(text[offset])) offset++;
 };
 value(0); whitespace(); if (offset !== text.length) throw new Error('Invalid Windows manifest JSON'); return parsed;
}
function exact(value: object, keys: string[]): boolean { const actual = Object.keys(value); return actual.length === keys.length && actual.every(key => keys.includes(key)); }
export function windowsManifestPath(path: unknown): path is string {
 if (typeof path !== 'string' || !path || path.length > 240 || /[^\x20-\x7e]|[<>:"\\|?*]/.test(path)) return false;
 return path.split('/').every(part => part.length > 0 && part.length <= 100 && part === part.trim() && !part.endsWith('.') && part !== '.' && part !== '..'
  && !/^(CON|PRN|AUX|NUL|COM[1-9]|LPT[1-9])$/i.test(part.split('.')[0].trimEnd()));
}
function manifest(value: unknown, context: WindowsManifestContext): WindowsProgramManifest {
 const doc = value as WindowsProgramManifest;
 const version = /^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-beta\.(0|[1-9]\d*))?$/.exec(context.version);
 if (!version || !version.slice(1).filter(part => part !== undefined).every(part => Number.isSafeInteger(Number(part))) || (context.channel === 'beta') !== context.version.includes('-beta.')
  || !doc || typeof doc !== 'object' || !exact(doc, ['format', 'kind', 'version', 'channel', 'platform', 'arch', 'files'])
  || doc.format !== 1 || doc.kind !== 'dilla-desktop-program' || doc.version !== context.version || doc.channel !== context.channel || doc.platform !== 'win32' || doc.arch !== context.arch || doc.arch !== 'x64'
  || !Array.isArray(doc.files) || doc.files.length < 1 || doc.files.length > 16384) throw new Error('Invalid Windows program manifest');
 let previous = '', total = 0; const names = new Set<string>(), nodes = new Map<string, string>();
 for (const file of doc.files) {
  if (!file || typeof file !== 'object' || !exact(file, ['path', 'size', 'sha512']) || !windowsManifestPath(file.path)
   || file.path <= previous || names.has(file.path.toLowerCase()) || !Number.isSafeInteger(file.size) || file.size < 0 || file.size > FILE_LIMIT || !canonicalSHA512(file.sha512)) throw new Error('Invalid Windows program manifest path, order, size or hash');
  const parts = file.path.split('/');
  for (let count = 1; count <= parts.length; count++) {
   const name = parts.slice(0, count).join('/'), key = name.toLowerCase(), existing = nodes.get(key);
   if ((existing !== undefined && existing !== name) || names.has(key) || (count === parts.length && existing !== undefined)) throw new Error('Invalid Windows program manifest path collision');
   nodes.set(key, name);
  }
  previous = file.path; names.add(file.path.toLowerCase()); total += file.size;
  if (total > TOTAL_LIMIT) throw new Error('Windows program manifest size limit');
 }
 return doc;
}
export function verifyWindowsProgramManifest(raw: Uint8Array, expectedSHA512: string, context: WindowsManifestContext): WindowsProgramManifest {
 if (!canonicalSHA512(expectedSHA512) || createHash('sha512').update(raw).digest('base64') !== expectedSHA512) throw new Error('Windows program manifest hash mismatch');
 return manifest(strictWindowsJSON(raw), context);
}
export function windowsProgramID(sha512: string): string {
 if (!canonicalSHA512(sha512)) throw new Error('Invalid Windows program manifest hash');
 return 'v-' + Buffer.from(sha512, 'base64').toString('hex').slice(0, 32);
}
async function programFiles(root: string): Promise<WindowsProgramFile[]> {
 const { lstat, realpath, readdir } = updateFS.promises;
 if (!isAbsolute(root) || await realpath(root) !== resolve(root)) throw new Error('Invalid Windows program tree root');
 const entries: WindowsProgramFile[] = [];
 const walk = async (directory: string, prefix: string): Promise<void> => {
  const info = await lstat(directory); if (!info.isDirectory() || info.isSymbolicLink() || await realpath(directory) !== directory) throw new Error('Linked Windows program directory');
  const children = await readdir(directory); if (!children.length) throw new Error('Empty Windows program directory');
  for (const child of children) {
   const path = prefix + child, absolute = join(directory, child), stat = await lstat(absolute);
   if (!windowsManifestPath(path)) throw new Error('Invalid Windows program manifest path');
   if (stat.isSymbolicLink()) throw new Error('Linked Windows program file');
   if (stat.isDirectory()) await walk(absolute, path + '/');
   else {
    if (!stat.isFile() || stat.nlink !== 1 || !Number.isSafeInteger(stat.size) || stat.size > FILE_LIMIT) throw new Error('Invalid or hardlinked Windows program file');
    const hash = createHash('sha512'); for await (const chunk of updateFS.createReadStream(absolute)) hash.update(chunk);
    entries.push({ path, size: stat.size, sha512: hash.digest('base64') });
   }
  }
 };
 await walk(root, ''); return entries.sort((a, b) => a.path < b.path ? -1 : a.path > b.path ? 1 : 0);
}
export async function createWindowsProgramManifest(root: string, context: WindowsManifestContext): Promise<Buffer> {
 const value: WindowsProgramManifest = { format: 1, kind: 'dilla-desktop-program', version: context.version, channel: context.channel, platform: 'win32', arch: context.arch, files: await programFiles(root) };
 manifest(value, context); const raw = Buffer.from(JSON.stringify(value) + '\n'); if (raw.length > LIMIT) throw new Error('Windows program manifest too large'); return raw;
}
export async function verifyWindowsProgramTree(root: string, value: WindowsProgramManifest): Promise<void> {
 const actual = await programFiles(root);
 if (JSON.stringify(actual) !== JSON.stringify(value.files)) throw new Error('Windows program tree changed from its authenticated manifest');
}
