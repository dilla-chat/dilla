import { createHash, randomBytes } from 'node:crypto';
import { verifyLinuxSandboxRelease } from './linux-sandbox-release.ts';
import type { LinuxSandboxDescriptor, ReleaseAsset } from './update.ts';
import { constants } from 'node:fs';
import { lstat, mkdir, mkdtemp, open, rename, rm } from 'node:fs/promises';
import { isAbsolute, join } from 'node:path';
import { syncUpdateDirectory } from './update-fs.ts';

export interface SandboxSidecars {
 version: string; descriptor: LinuxSandboxDescriptor; assets: ReleaseAsset[]; capsule: Uint8Array; manifest: Uint8Array; signature: Uint8Array; keys: readonly string[]; now: number;
}
export type SandboxSidecarTrust = Omit<SandboxSidecars, 'capsule' | 'manifest' | 'signature'>;
export function authenticateSandboxSidecars(input: SandboxSidecars): { generation: number; compatibilityEpoch: 1 } {
 if (/^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-beta\.(0|[1-9]\d*))?$/.exec(input.version)?.[0] !== input.version) throw Error('Invalid sandbox version');
 const name = `dilla-${input.version}-linux-x64.sandbox`;
 if (input.descriptor.compatibilityEpoch !== 1 || input.descriptor.bundle.url !== `https://github.com/jonasthim/dilla/releases/download/desktop-v${input.version}/${name}`) throw Error('Invalid sandbox descriptor');
 const files = [input.capsule, input.manifest, input.signature], limits = [48 * 1024 ** 2, 65536, 89];
 for (let i = 0; i < files.length; i++) {
  const matches = input.assets.filter(a => a.name === name + ['', '.json', '.sig'][i]);
  if (matches.length !== 1 || files[i].length < 1 || files[i].length > limits[i] || matches[0].size !== files[i].length || matches[0].sha512 !== createHash('sha512').update(files[i]).digest('base64')) throw Error('Sandbox asset verification failed');
 }
 if (input.descriptor.bundle.size !== input.capsule.length || input.descriptor.bundle.sha512 !== createHash('sha512').update(input.capsule).digest('base64')) throw Error('Sandbox bundle verification failed');
 return verifyLinuxSandboxRelease({ capsule: input.capsule, manifest: input.manifest, signature: Buffer.from(input.signature).toString('utf8'), keys: input.keys, now: input.now, minimumGeneration: input.descriptor.minimumGeneration });
}

async function privateDirectory(path: string): Promise<void> {
 const s = await lstat(path);
 if (!s.isDirectory() || s.isSymbolicLink() || s.uid !== process.getuid?.() || (s.mode & 0o077) !== 0) throw Error('Unsafe sandbox cache directory');
}
export async function saveManualSignature(root: string, input: SandboxSidecars): Promise<string> {
 authenticateSandboxSidecars(input);
 if (!isAbsolute(root)) throw Error('Invalid sandbox signature cache');
 await mkdir(root, { recursive: true, mode: 0o700 });
 const dilla = join(root, 'dilla'), directory = join(dilla, 'sandbox-signatures');
 for (const path of [dilla, directory]) { try { await mkdir(path, { mode: 0o700 }); } catch (error) { if ((error as NodeJS.ErrnoException).code !== 'EEXIST') throw error; } await privateDirectory(path); }
 const destination = join(directory, createHash('sha512').update(input.manifest).digest('hex'));
 const temporary = join(directory, `.new-${randomBytes(16).toString('hex')}`);
 try {
  const file = await open(temporary, constants.O_WRONLY | constants.O_CREAT | constants.O_EXCL | (constants.O_NOFOLLOW ?? 0), 0o600);
  try { await file.writeFile(Buffer.from(Buffer.from(input.signature).toString('utf8').trim(), 'base64')); await file.sync(); } finally { await file.close(); }
  await rename(temporary, destination); await syncUpdateDirectory(directory); await syncUpdateDirectory(dilla);
  return destination;
 } finally { await rm(temporary, { force: true }); }
}
export async function saveSandboxSidecars(userData: string, input: SandboxSidecars): Promise<string> {
 authenticateSandboxSidecars(input);
 const root = join(userData, 'sandbox-sidecars');
 try { await mkdir(root, { mode: 0o700 }); } catch (error) { if ((error as NodeJS.ErrnoException).code !== 'EEXIST') throw error; }
 await privateDirectory(root);
 const directory = await mkdtemp(join(root, 'candidate-'));
 try {
  for (const [name, bytes] of [['capsule',input.capsule],['manifest',input.manifest],['signature',input.signature]] as const) {
   const file = await open(join(directory, name), constants.O_WRONLY | constants.O_CREAT | constants.O_EXCL | (constants.O_NOFOLLOW ?? 0), 0o600);
   try { await file.writeFile(bytes); await file.sync(); } finally { await file.close(); }
  }
  await syncUpdateDirectory(directory); await syncUpdateDirectory(root);
  await readSandboxSidecars(directory, input);
  return directory;
 } catch (error) { await rm(directory, { recursive: true, force: true }); throw error; }
}
export async function readSandboxSidecars(directory: string, expected: SandboxSidecarTrust): Promise<{ generation: number; compatibilityEpoch: 1; capsule: Buffer; manifest: Buffer; signature: Buffer }> {
 await privateDirectory(directory);
 const read = async (name: string, limit: number): Promise<Buffer> => {
  const file = await open(join(directory, name), constants.O_RDONLY | (constants.O_NOFOLLOW ?? 0));
  try { const s = await file.stat(); if (!s.isFile() || s.nlink !== 1 || s.uid !== process.getuid?.() || (s.mode & 0o077) !== 0 || s.size < 1 || s.size > limit) throw Error('Unsafe sandbox cache file'); const bytes = Buffer.alloc(s.size + 1); let n = 0; while (n < bytes.length) { const result = await file.read(bytes, n, bytes.length-n, n); if (!result.bytesRead) break; n += result.bytesRead; } if (n !== s.size) throw Error('Sandbox cache changed while reading'); return bytes.subarray(0,n); }
  finally { await file.close(); }
 };
 const [capsule, manifest, signature] = await Promise.all([read('capsule',48*1024**2),read('manifest',65536),read('signature',89)]);
 return { ...authenticateSandboxSidecars({ ...expected, capsule, manifest, signature }), capsule, manifest, signature };
}

export async function copySandboxSidecars(source: string, destination: string, expected: SandboxSidecarTrust): Promise<void> {
 const bytes = await readSandboxSidecars(source, expected);
 await mkdir(destination, { mode: 0o700 });
 try {
  for (const name of ['capsule', 'manifest', 'signature'] as const) {
   const file = await open(join(destination, name), constants.O_WRONLY | constants.O_CREAT | constants.O_EXCL | (constants.O_NOFOLLOW ?? 0), 0o600);
   try { await file.writeFile(bytes[name]); await file.sync(); } finally { await file.close(); }
  }
  await syncUpdateDirectory(destination);
  await readSandboxSidecars(destination, expected);
 } catch (error) { await rm(destination, { recursive: true, force: true }); throw error; }
}

/** Follow only GitHub's HTTPS asset hosts, with one deadline across redirects. */
export async function fetchSandboxAsset(url: string, maxBytes: number, signal: AbortSignal): Promise<Uint8Array> {
 let current = new URL(url);
 for (let redirects = 0; ; redirects++) {
  if (current.protocol !== 'https:' || current.port || current.username || current.password || !['github.com', 'release-assets.githubusercontent.com', 'objects.githubusercontent.com', 'github-releases.githubusercontent.com'].includes(current.hostname)) throw Error('Invalid sandbox download destination');
  const response = await fetch(current, { signal, redirect: 'manual', credentials: 'omit', cache: 'no-store' });
  if ([301, 302, 303, 307, 308].includes(response.status)) {
   const location = response.headers.get('location'); await response.body?.cancel();
   if (!location || redirects >= 3) throw Error('Invalid sandbox download redirect');
   current = new URL(location, current); continue;
  }
  if (!response.ok || !response.body) throw Error('Sandbox download failed');
  const reader = response.body.getReader(), chunks: Uint8Array[] = []; let size = 0;
  try { for (;;) { const { done, value } = await reader.read(); if (done) break; size += value.length; if (size > maxBytes) throw Error('Sandbox download is too large'); chunks.push(value); } }
  finally { await reader.cancel(); }
  return Buffer.concat(chunks);
 }
}
