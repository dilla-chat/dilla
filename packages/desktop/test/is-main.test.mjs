import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, mkdir, rm, symlink, writeFile } from 'node:fs/promises';
import { spawnSync } from 'node:child_process';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { isMain } from '../scripts/is-main.mjs';

const helper = fileURLToPath(new URL('../scripts/is-main.mjs', import.meta.url));

test('a CLI started through a symlinked directory still runs (macOS /var → /private/var)', async () => {
 const root = await mkdtemp(join(tmpdir(), 'dilla-is-main-'));
 try {
  const real = join(root, 'real'), linked = join(root, 'linked');
  await mkdir(real);
  await writeFile(join(real, 'cli.mjs'), `import { isMain } from ${JSON.stringify(helper)};\nif (isMain(import.meta.url)) process.stdout.write('ran');\n`);
  await symlink(real, linked);
  const run = spawnSync(process.execPath, [join(linked, 'cli.mjs')], { encoding: 'utf8' });
  assert.equal(run.status, 0, run.stderr);
  assert.equal(run.stdout, 'ran');
 } finally { await rm(root, { recursive: true, force: true }); }
});

test('an imported module is not main, and a missing argv[1] is not main', () => {
 assert.equal(isMain(import.meta.url, [process.execPath, helper]), false);
 assert.equal(isMain(import.meta.url, [process.execPath]), false);
 assert.equal(isMain(import.meta.url, [process.execPath, '/no/such/file.mjs']), false);
});
