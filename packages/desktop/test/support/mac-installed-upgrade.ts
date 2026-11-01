import { createHash } from 'node:crypto';
import { createReadStream } from 'node:fs';
import { lstat, readdir, readlink } from 'node:fs/promises';
import type { ServerResponse } from 'node:http';
import { join } from 'node:path';

// Include the complete bundle, modes and symlink targets. Plist version alone
// misses a partially replaced resource or executable after a failed update.
export async function bundleDigest(root: string): Promise<string> {
  const hash = createHash('sha256');
  const visit = async (relative: string): Promise<void> => {
    const file = join(root, relative), info = await lstat(file);
    hash.update(JSON.stringify([relative, info.mode, info.isDirectory() ? null : info.size, info.isSymbolicLink() ? 'link' : info.isDirectory() ? 'dir' : 'file']));
    if (info.isSymbolicLink()) hash.update(JSON.stringify(await readlink(file)));
    else if (info.isDirectory()) {
      for (const entry of (await readdir(file)).sort()) await visit(join(relative, entry));
    } else {
      for await (const chunk of createReadStream(file)) hash.update(chunk);
    }
  };
  await visit('');
  return hash.digest('hex');
}

// The valid signed bytes stay immutable so the next attempt can use exactly the
// same release. A short body with a full Content-Length must fail at transport,
// rather than accidentally testing only Sparkle's archive verifier again.
export function serveArchive(response: ServerResponse, archive: Buffer, fault: 'none' | 'archive' | 'download'): void {
  response.writeHead(200, { 'Content-Type': 'application/zip', 'Content-Length': archive.length, 'Cache-Control': 'no-store' });
  if (fault === 'download') {
    response.flushHeaders();
    response.write(archive.subarray(0, Math.max(1, Math.min(65536, Math.floor(archive.length / 2)))));
    setTimeout(() => response.destroy(), 25);
  } else {
    const bytes = fault === 'archive' ? Buffer.from(archive) : archive;
    if (fault === 'archive') bytes[Math.floor(bytes.length / 2)] ^= 1;
    response.end(bytes);
  }
}
