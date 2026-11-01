import { join } from 'node:path';
import { randomUUID } from 'node:crypto';
import { canonicalUpdateUserData, updateFS, syncUpdateDirectory } from './update-fs.ts';
import { readUpdateJournal, type UpdatePhase, type UpdateFailure } from './update-journal.ts';
import type { InstalledUpdatePlan } from './update-plan.ts';
const { lstat, realpath, open, rename, rm } = updateFS.promises;
async function privateUpdates(userData: string): Promise<string> {
 const root = join(await canonicalUpdateUserData(userData), 'updates'), info = await lstat(root);
 if (!info.isDirectory() || info.isSymbolicLink() || await realpath(root) !== root
  || (process.platform !== 'win32' && ((info.mode & 0o077) !== 0 || info.uid !== process.getuid!()))) throw new Error('Invalid active update directory');
 return root;
}
export async function activateInstalledUpdate(plan: InstalledUpdatePlan): Promise<void> {
 const directory = await privateUpdates(plan.userData), file = join(directory, 'active.json'), temporary = file + '.' + randomUUID();
 let created = false;
 try {
  const handle = await open(temporary, 'wx', 0o600); created = true;
  try { await handle.writeFile(JSON.stringify({ format: 1, nonce: plan.health.nonce })); await handle.sync(); } finally { await handle.close(); }
  await rename(temporary, file); await syncUpdateDirectory(directory);
 } finally { if (created) await rm(temporary, { force: true }); }
}
export async function activeInstalledUpdate(userData: string): Promise<{ nonce: string; phase: UpdatePhase; version?: string; failure?: UpdateFailure } | null> {
 const root = await privateUpdates(userData).catch((error: unknown) => { if ((error as NodeJS.ErrnoException).code === 'ENOENT') return null; throw error; });
 if (root === null) return null;
 const file = join(root, 'active.json');
 const info = await lstat(file).catch((error: unknown) => { if ((error as NodeJS.ErrnoException).code === 'ENOENT') return null; throw error; });
 if (info === null) return null;
 // Once the pointer has been observed, a missing referenced ticket is an
 // uncertain transaction, never equivalent to absence of an update.
  if (!info.isFile() || info.isSymbolicLink()) throw new Error('Invalid active update record');
  const handle = await open(file, updateFS.constants.O_RDONLY | (updateFS.constants.O_NOFOLLOW ?? 0));
  let nonce: string;
  try {
   const stat = await handle.stat();
   if (!stat.isFile() || stat.size > 256 || (process.platform !== 'win32' && ((stat.mode & 0o077) !== 0 || stat.uid !== process.getuid!()))) throw new Error('Invalid active update record');
   const bytes = Buffer.alloc(257), { bytesRead } = await handle.read(bytes, 0, bytes.length, 0);
   if (bytesRead > 256) throw new Error('Invalid active update record');
   let value;
   try { value = JSON.parse(bytes.subarray(0, bytesRead).toString()); } catch { throw new Error('Invalid active update record'); }
   if (!value || Object.keys(value).length !== 2 || value.format !== 1 || typeof value.nonce !== 'string' || !/^[a-f0-9]{64}$/.test(value.nonce)) throw new Error('Invalid active update record');
   nonce = value.nonce;
  } finally { await handle.close(); }
  const journal = await readUpdateJournal({ health: { directory: join(root, `startup-${nonce}`), nonce } });
  return { nonce, phase: journal.phase, ...(journal.version ? { version: journal.version } : {}), ...(journal.failure ? { failure: journal.failure } : {}) };
}


export async function pendingInstalledUpdate(userData: string, installedVersion?: string): ReturnType<typeof activeInstalledUpdate> {
 const active = await activeInstalledUpdate(userData);
 // A complete rename can survive a failed directory flush and subsequent
 // quota-blocked failure writes. A different running version must keep the
 // retry diagnostic, never silently redownload that failed transaction. This
 // status cannot authorize recovery or choose any program/path.
 return active?.phase === 'complete' && (installedVersion === undefined || active.version === installedVersion) ? null : active;
}
