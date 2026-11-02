import { constants } from 'node:fs';
import { open } from 'node:fs/promises';

/** The Linux sandbox cache publishes sidecars only after its directory flush.
 * Node does not support this directory flush on Windows fixture hosts. */
export async function syncUpdateDirectory(path: string): Promise<void> {
 if (process.platform === 'win32') return;
 const handle = await open(path, constants.O_RDONLY | (constants.O_NOFOLLOW ?? 0));
 try { await handle.sync(); } finally { await handle.close(); }
}
