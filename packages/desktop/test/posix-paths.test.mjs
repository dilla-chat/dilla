import { test } from 'node:test';
import assert from 'node:assert/strict';
import { win32, posix } from 'node:path';
import { mkdtemp, mkdir, writeFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { posixTree, posixRelative, toPosix } from './support/posix-paths.mjs';

test('posixTree turns win32 readdir entries into the site layout', async () => {
 const list = async () => ['stable', 'stable\\desktop-stable.json', 'stable\\appcast-arm64.xml', 'beta\\x\\y'];
 assert.deepEqual(await posixTree('C:\\site', { pathModule: win32, list }), ['beta/x/y', 'stable', 'stable/appcast-arm64.xml', 'stable/desktop-stable.json']);
});

test('posixTree leaves POSIX entries alone', async () => {
 assert.deepEqual(await posixTree('/site', { pathModule: posix, list: async () => ['b/c', 'a'] }), ['a', 'b/c']);
});

test('posixRelative gives repo paths with / under a win32 path module', () => {
 assert.equal(posixRelative('C:\\r', 'C:\\r\\src\\update-keys.ts', win32), 'src/update-keys.ts');
 assert.equal(toPosix('a\\b', win32), 'a/b');
 assert.equal(posixRelative('/r', '/r/src/update-keys.ts', posix), 'src/update-keys.ts');
});

test('posixTree lists a real directory with / separators on this platform', async () => {
 const dir = await mkdtemp(join(tmpdir(), 'dilla-posix-tree-'));
 try { await mkdir(join(dir, 'stable')); await writeFile(join(dir, 'stable', 'f'), ''); assert.deepEqual(await posixTree(dir), ['stable', 'stable/f']); } finally { await rm(dir, { recursive: true, force: true }); }
});
