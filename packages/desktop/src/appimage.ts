import { createReadStream } from 'node:fs';
import { stat } from 'node:fs/promises';
import { createHash } from 'node:crypto';
import type { Artifact } from './update.ts';

type Integrity = Pick<Artifact, 'size' | 'sha512'>;
export async function verifyArtifactFile(path: string, artifact: Integrity): Promise<void> {
  const info = await stat(path);
  if (!info.isFile() || info.size !== artifact.size) throw new Error('Update verification failed: size');
  const hash = createHash('sha512');
  for await (const chunk of createReadStream(path)) hash.update(chunk);
  if (hash.digest('base64') !== artifact.sha512) throw new Error('Update verification failed: hash');
}
