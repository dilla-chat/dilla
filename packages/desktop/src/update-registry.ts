import { newerVersion } from './update.ts';
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { win32 } from 'node:path';
const execute = promisify(execFile);
const PREFIX = 'HKCU\\Software\\Microsoft\\Windows\\CurrentVersion\\Uninstall\\';
interface RegistryValue { type: 'REG_SZ' | 'REG_DWORD'; data: string }
export interface UninstallSnapshot { key: string; location: string; displayVersion: RegistryValue | null; estimatedSize: RegistryValue | null }
export interface RegistryCommands {
 list(): Promise<string[]>;
 read(key: string, name: string): Promise<RegistryValue | null>;
 write(key: string, name: string, value: RegistryValue | null): Promise<void>;
}
const reg = () => win32.join(process.env.SystemRoot ?? 'C:\\Windows', 'System32', 'reg.exe');
async function query(args: string[]): Promise<string> { return (await execute(reg(), args, { encoding: 'utf8', windowsHide: true, maxBuffer: 1024 * 1024 })).stdout; }
const commands: RegistryCommands = {
 list: async () => (await query(['query', PREFIX.slice(0, -1)])).split(/\r?\n/).map(value => value.trim().replace(/^HKEY_CURRENT_USER\\/i, 'HKCU\\')).filter(value => value.startsWith(PREFIX)),
 read: async (key, name) => {
  try {
   const match = (await query(['query', key, '/v', name])).split(/\r?\n/).map(line => /^\s+([^\s]+)\s+(REG_SZ|REG_DWORD)\s+(.+?)\s*$/.exec(line)).find(value => value?.[1] === name);
   return match ? { type: match[2] as RegistryValue['type'], data: match[3] } : null;
  } catch (error) { if ((error as { code?: number }).code === 1) return null; throw error; }
 },
 write: async (key, name, value) => {
  if (value) await query(['add', key, '/v', name, '/t', value.type, '/d', value.data, '/f']);
  else { try { await query(['delete', key, '/v', name, '/f']); } catch (error) { if ((error as { code?: number }).code !== 1) throw error; } }
 },
};
function sameLocation(a: string, b: string): boolean { return win32.isAbsolute(a) && win32.normalize(a).replace(/\\$/, '').toLowerCase() === win32.normalize(b).replace(/\\$/, '').toLowerCase(); }
function keyValid(key: string): boolean { return key.startsWith(PREFIX) && /^[A-Za-z0-9{}_.-]{1,128}$/.test(key.slice(PREFIX.length)); }
function valueValid(value: RegistryValue | null, type: RegistryValue['type']): boolean {
 return value === null || (!!value && Object.keys(value).length === 2 && value.type === type && typeof value.data === 'string'
  && (type === 'REG_DWORD' ? /^(?:0x[0-9a-f]{1,8}|[0-9]{1,10})$/i.test(value.data) : /^[0-9A-Za-z.+-]{1,64}$/.test(value.data)));
}
function validate(root: string, snapshot: UninstallSnapshot): void {
 if (!snapshot || Object.keys(snapshot).length !== 4 || typeof snapshot.key !== 'string' || !keyValid(snapshot.key)
  || typeof snapshot.location !== 'string' || !sameLocation(root, snapshot.location)
  || !valueValid(snapshot.displayVersion, 'REG_SZ') || !valueValid(snapshot.estimatedSize, 'REG_DWORD')) throw new Error('Invalid owned uninstall snapshot');
}
/** Discover the per-user NSIS entry only by the exact original program root. */
export async function snapshotUninstall(root: string, registry: RegistryCommands = commands): Promise<UninstallSnapshot> {
 const matches: string[] = [];
 for (const key of await registry.list()) {
  if (!keyValid(key)) continue;
  const location = await registry.read(key, 'InstallLocation');
  if (location?.type === 'REG_SZ' && sameLocation(location.data, root)) matches.push(key);
 }
 if (matches.length !== 1) throw new Error('Could not identify one owned per-user uninstall entry');
 const key = matches[0], snapshot = { key, location: root, displayVersion: await registry.read(key, 'DisplayVersion'), estimatedSize: await registry.read(key, 'EstimatedSize') };
 validate(root, snapshot); return snapshot;
}
/** Restore only these two values, never imports, arbitrary keys, or HKLM. */
export async function restoreUninstall(root: string, snapshot: UninstallSnapshot, registry: RegistryCommands = commands): Promise<void> {
 validate(root, snapshot);
 const current = await registry.read(snapshot.key, 'InstallLocation');
 if (!current || current.type !== 'REG_SZ' || !sameLocation(current.data, root)) throw new Error('The uninstall entry is no longer owned by this installation');
 await registry.write(snapshot.key, 'DisplayVersion', snapshot.displayVersion);
 await registry.write(snapshot.key, 'EstimatedSize', snapshot.estimatedSize);
}

/** Called only after the signed program slot has irreversibly committed. A
 * registration failure is a diagnostic; it can never undo program activation. */
export async function updateUninstallVersion(root:string,snapshot:UninstallSnapshot,version:string,programBytes:number,registry:RegistryCommands=commands):Promise<void>{
 if(!newerVersion(version,'0.0.0')||!Number.isSafeInteger(programBytes)||programBytes<1||programBytes>8*1024**3)throw new Error('Invalid committed Windows version/size');
 await restoreUninstall(root,{...snapshot,displayVersion:{type:'REG_SZ',data:version},estimatedSize:{type:'REG_DWORD',data:String(Math.ceil(programBytes/1024))}},registry);
}
