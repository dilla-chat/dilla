import { createRequire } from 'node:module';
import { lstatSync } from 'node:fs';
import { strictWindowsJSON } from './windows-manifest.ts';
export interface LinuxAdmission { format: 1; manager: 'deb' | 'rpm' | 'alpm'; state: 'ready'; transaction: string; identitySha512: string; alpmHooks: null | { upgradeSha512: string; removeSha512: string } }
function exact(value: unknown, keys: string[]): value is Record<string, unknown> {
 return value !== null && typeof value === 'object' && !Array.isArray(value) && Object.keys(value).length === keys.length && Object.keys(value).every(key => keys.includes(key));
}
function digest(value: unknown): value is string {
 return typeof value === 'string' && /^[A-Za-z0-9+/]{86}==$/.test(value) && Buffer.from(value, 'base64').toString('base64') === value;
}
export function parseLinuxAdmission(raw: string | Uint8Array): LinuxAdmission {
 const bytes = typeof raw === 'string' ? Buffer.from(raw) : raw;
 if (bytes.byteLength < 1 || bytes.byteLength > 4096) throw new Error('Invalid native admission status size');
 const text = new TextDecoder('utf-8', { fatal: true }).decode(bytes);
 const value = strictWindowsJSON(bytes);
 // The sole numeric field is format:1. Reject fractions/exponents before they
 // can round to 1; quoted values/escaped property names contain no numeric tokens.
 const numbers = text.replace(/"(?:[^"\\]|\\.)*"/g, '""').match(/-?\d[\deE+.-]*/g);
 if (numbers?.length !== 1 || numbers[0] !== '1' || !exact(value, ['format', 'manager', 'state', 'transaction', 'identitySha512', 'alpmHooks'])
  || value.format !== 1 || typeof value.manager !== 'string' || !['deb', 'rpm', 'alpm'].includes(value.manager) || value.state !== 'ready'
  || typeof value.transaction !== 'string' || !/^[a-f0-9]{64}$/.test(value.transaction) || !digest(value.identitySha512)) throw new Error('Native admission is not ready; repair using your package manager');
 if (value.manager === 'alpm' ? !exact(value.alpmHooks, ['upgradeSha512', 'removeSha512']) || !digest(value.alpmHooks.upgradeSha512) || !digest(value.alpmHooks.removeSha512) : value.alpmHooks !== null) throw new Error('Invalid native admission hook identity');
 return value as unknown as LinuxAdmission;
}
const ADDON = '/usr/lib/dilla/native/app/resources/native/dilla-admission.node';
let bridge: { hold(): string } | undefined;
/** Main-only synchronous admission. No caller-selected addon/path, FD, environment
 * override, renderer IPC, unlock or cleanup capability is exposed. */
export function holdLinuxAdmission(): LinuxAdmission {
 if (arguments.length || process.platform !== 'linux') throw new Error('Invalid native admission invocation');
 if (!bridge) {
  const ancestors = ['/', '/usr', '/usr/lib', '/usr/lib/dilla', '/usr/lib/dilla/native', '/usr/lib/dilla/native/app', '/usr/lib/dilla/native/app/resources', '/usr/lib/dilla/native/app/resources/native'];
  for (const path of ancestors) {
   const s = lstatSync(path);
   if (!s.isDirectory() || s.isSymbolicLink() || s.uid !== 0 || s.gid !== 0 || (s.mode & 0o7777) !== 0o755) throw new Error('Unsafe native admission addon parent');
  }
  const s = lstatSync(ADDON);
  if (!s.isFile() || s.isSymbolicLink() || s.nlink !== 1 || s.uid !== 0 || s.gid !== 0 || ![0o644, 0o755].includes(s.mode & 0o7777) || s.size < 1 || s.size > 8 * 1024 ** 2) throw new Error('Unsafe native admission addon');
  const loaded: unknown = createRequire(ADDON)(ADDON);
  if (!exact(loaded, ['hold']) || typeof loaded.hold !== 'function') throw new Error('Invalid native admission bridge');
  bridge = loaded as { hold(): string };
 }
 const raw: unknown = bridge.hold();
 if (typeof raw !== 'string') throw new Error('Invalid native admission snapshot');
 return parseLinuxAdmission(raw);
}
