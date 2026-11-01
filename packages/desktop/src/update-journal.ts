import { join } from 'node:path';
import { updateFS, syncUpdateDirectory } from './update-fs.ts';
import { newerVersion } from './update.ts';
import { randomUUID } from 'node:crypto';
type JournalPlan = { health: { directory: string; nonce: string; version?: string } };
const { open, rename, rm, lstat, realpath } = updateFS.promises;
export type UpdatePhase = 'prepared' | 'installing' | 'launching' | 'recovering' | 'failed' | 'complete';
export type UpdateFailure = 'installer' | 'startup' | 'interrupted' | 'metadata';
export interface UpdateJournal { format: 1; nonce: string; phase: UpdatePhase; childPid?: number; watchdogPid?: number; installerActorPid?: number; installerPid?: number; failure?: UpdateFailure; version?: string }

async function journalDirectory(plan: JournalPlan): Promise<void> {
 const directory = plan.health.directory, info = await lstat(directory);
 if (!info.isDirectory() || info.isSymbolicLink() || await realpath(directory) !== directory
  || (process.platform !== 'win32' && ((info.mode & 0o077) !== 0 || info.uid !== process.getuid!()))) throw new Error('Invalid private update journal directory');
}
function validate(value: UpdateJournal, plan: JournalPlan): UpdateJournal {
 if (!value || value.format !== 1 || value.nonce !== plan.health.nonce || !['prepared', 'installing', 'launching', 'recovering', 'failed', 'complete'].includes(value.phase)
  || Object.keys(value).some(key => !['format', 'nonce', 'phase', 'childPid', 'watchdogPid', 'installerActorPid', 'installerPid', 'failure', 'version'].includes(key))
  || [value.childPid, value.watchdogPid, value.installerActorPid, value.installerPid].some(pid => pid !== undefined && (!Number.isSafeInteger(pid) || pid <= 1 || pid > 0xFFFFFFFF))
  || (value.phase === 'complete' && (value.childPid === undefined || typeof value.version !== 'string' || !newerVersion(value.version, '0.0.0') || (plan.health.version !== undefined && value.version !== plan.health.version)))
  || (value.phase !== 'complete' && value.version !== undefined)
  || (value.failure !== undefined && !['installer', 'startup', 'interrupted', 'metadata'].includes(value.failure))) throw new Error('Invalid private update journal');
 return value;
}
/** Records contain bounded enum/status data only; they cannot choose paths or actions. */
export async function writeUpdateJournal(plan: JournalPlan, state: Omit<UpdateJournal, 'format' | 'nonce'>): Promise<void> {
 await journalDirectory(plan);
 const value = validate({ format: 1, nonce: plan.health.nonce, ...state, ...(state.phase === 'complete' ? { version: plan.health.version } : {}) }, plan), file = join(plan.health.directory, 'journal.json'), temporary = file + '.' + randomUUID();
 let created = false;
 try {
  const handle = await open(temporary, 'wx', 0o600); created = true;
  try { await handle.writeFile(JSON.stringify(value)); await handle.sync(); } finally { await handle.close(); }
  await rename(temporary, file); await syncUpdateDirectory(plan.health.directory);
 } finally { if (created) await rm(temporary, { force: true }); }
}
export async function readUpdateJournal(plan: JournalPlan): Promise<UpdateJournal> {
 await journalDirectory(plan);
 const file = join(plan.health.directory, 'journal.json');
 try {
  const info = await lstat(file);
  if (!info.isFile() || info.isSymbolicLink()) throw new Error('Invalid private update journal');
  const handle = await open(file, updateFS.constants.O_RDONLY | (updateFS.constants.O_NOFOLLOW ?? 0));
  try {
   const stat = await handle.stat();
   if (!stat.isFile() || stat.size > 1024 || (process.platform !== 'win32' && ((stat.mode & 0o077) !== 0 || stat.uid !== process.getuid!()))) throw new Error('Invalid private update journal');
   const bytes = Buffer.alloc(1025), { bytesRead } = await handle.read(bytes, 0, bytes.length, 0);
   if (bytesRead > 1024) throw new Error('Invalid private update journal');
   return validate(JSON.parse(bytes.subarray(0, bytesRead).toString()), plan);
  } finally { await handle.close(); }
 } catch (error) {
  if ((error as NodeJS.ErrnoException).code === 'ENOENT') return { format: 1, nonce: plan.health.nonce, phase: 'prepared' };
  throw error;
 }
}
