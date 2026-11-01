import { createReadStream } from 'node:fs';
import { constants, chmod, copyFile, open, rename, rm, stat } from 'node:fs/promises';
import { createHash, randomUUID } from 'node:crypto';
import { dirname, isAbsolute } from 'node:path';
import type { Artifact } from './update.ts';

type Integrity = Pick<Artifact, 'size' | 'sha512'>;
export async function verifyArtifactFile(path: string, artifact: Integrity): Promise<void> {
  const info = await stat(path);
  if (!info.isFile() || info.size !== artifact.size) throw new Error('Update verification failed: size');
  const hash = createHash('sha512');
  for await (const chunk of createReadStream(path)) hash.update(chunk);
  if (hash.digest('base64') !== artifact.sha512) throw new Error('Update verification failed: hash');
}

async function sync(path: string): Promise<void> {
  const file = await open(path, 'r');
  try { await file.sync(); } finally { await file.close(); }
}

/** Stage on the destination filesystem before changing the working file. Keep its stable name for autostart. */
export async function replaceAppImage(source: string, destination: string, artifact: Integrity, launch: (path: string) => Promise<void>, beforeRollback: () => Promise<void> = async () => {}): Promise<void> {
  if (!isAbsolute(source) || !isAbsolute(destination) || source === destination) throw new Error('Invalid AppImage paths');
  const stage = `${destination}.update-${randomUUID()}`;
  const backupStage = `${stage}.previous`;
  const backup = destination + '.previous';
  let replaced = false;
  try {
    await verifyArtifactFile(source, artifact);
    await copyFile(source, stage, constants.COPYFILE_EXCL);
    await verifyArtifactFile(stage, artifact);
    await chmod(stage, (await stat(destination)).mode & 0o777);
    await sync(stage);
    await copyFile(destination, backupStage, constants.COPYFILE_EXCL);
    await sync(backupStage);
    await rename(backupStage, backup);
    await sync(dirname(destination));
    await rename(stage, destination);
    replaced = true;
    await sync(dirname(destination));
    await launch(destination);
  } catch (error) {
    if (replaced) { await beforeRollback(); await rename(backup, destination); await sync(dirname(destination)); }
    throw error;
  } finally {
    await rm(stage, { force: true });
    await rm(backupStage, { force: true });
  }
}
