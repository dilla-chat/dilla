import { realpathSync } from 'node:fs';
import { resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

/** Whether the module at `metaUrl` is the script node was started with. Both sides are compared as real paths, so a
 * symlinked temp or checkout directory (macOS /var → /private/var) still runs the CLI instead of silently doing nothing. */
export function isMain(metaUrl, argv = process.argv) {
 if (!argv[1]) return false;
 try { return realpathSync(resolve(argv[1])) === realpathSync(fileURLToPath(metaUrl)); } catch { return false; }
}
