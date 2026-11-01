import * as nodeFS from 'node:fs';
import { createRequire } from 'node:module';
import { basename, dirname, isAbsolute, join, resolve } from 'node:path';

// Electron's normal fs treats app.asar as a directory. Updater backup/hash work
// must preserve the actual authenticated archive bytes. This module is main-only
// and is never exposed through the preload or renderer IPC.
const load = createRequire(join(process.cwd(), '__dilla-update.cjs'));
export const updateFS: typeof nodeFS = process.versions.electron ? load('original-fs') : nodeFS;

/** Resolve profile aliases before containment checks or writes, including when
 * the profile has not been created yet. A dangling alias is not a missing dir. */
export async function canonicalUpdateUserData(path: string): Promise<string> {
 if (!isAbsolute(path)) throw new Error('Invalid update profile path');
 let ancestor = resolve(path);
 const missing: string[] = [];
 for (;;) {
  try { return join(await updateFS.promises.realpath(ancestor), ...missing); }
  catch (error) {
   if ((error as NodeJS.ErrnoException).code !== 'ENOENT') throw error;
   try {
    await updateFS.promises.lstat(ancestor);
    throw new Error('Invalid dangling update profile alias');
   } catch (linkError) {
    if ((linkError as NodeJS.ErrnoException).code !== 'ENOENT') throw linkError;
   }
   const parent = dirname(ancestor);
   if (parent === ancestor) throw error;
   missing.unshift(basename(ancestor)); ancestor = parent;
  }
 }
}

export async function syncUpdateFile(path: string): Promise<void> {
 // Windows FlushFileBuffers requires a handle with write access.
 const access = process.platform === 'win32' ? updateFS.constants.O_RDWR : updateFS.constants.O_RDONLY;
 const handle = await updateFS.promises.open(path, access | (updateFS.constants.O_NOFOLLOW ?? 0));
 try { await handle.sync(); } finally { await handle.close(); }
}

/** Node cannot reliably open/fsync directory handles on Windows. File contents
 * are flushed there, but directory entry persistence across power loss is not
 * guaranteed. On POSIX a directory flush failure aborts preparation. */
export async function syncUpdateDirectory(path: string): Promise<void> {
 if (process.platform === 'win32') return;
 await syncUpdateFile(path);
}
