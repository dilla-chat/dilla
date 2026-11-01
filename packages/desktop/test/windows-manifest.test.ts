import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, mkdir, writeFile, rm, link, symlink, realpath } from 'node:fs/promises';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { createHash } from 'node:crypto';
import { createWindowsProgramManifest, verifyWindowsProgramManifest, verifyWindowsProgramTree, windowsProgramID } from '../src/windows-manifest.ts';
const context = { version: '1.0.1', channel: 'stable' as const, arch: 'x64' as const };
const hash = (raw: Uint8Array) => createHash('sha512').update(raw).digest('base64');
async function tree(run: (root: string) => Promise<void>) {
 const root = await realpath(await mkdtemp(join(tmpdir(), 'dilla-windows-manifest-')));
 try { await mkdir(join(root, 'resources')); await writeFile(join(root, 'dilla-desktop.exe'), 'final fused edited executable'); await writeFile(join(root, 'resources/app.asar'), Buffer.from([0, 1, 2, 255])); await run(root); }
 finally { await rm(root, { recursive: true, force: true, maxRetries: 10, retryDelay: 100 }); }
}
test('final program manifest binds raw executable and ASAR bytes, accepts empty regular files and refuses post-build mutation', async () => tree(async root => {
 await writeFile(join(root, 'resources/empty'), '');
 const raw = await createWindowsProgramManifest(root, context), digest = hash(raw), manifest = verifyWindowsProgramManifest(raw, digest, context);
 assert.deepEqual(manifest.files.map(file => file.path), ['dilla-desktop.exe', 'resources/app.asar', 'resources/empty']);
 assert.equal(windowsProgramID(digest), 'v-' + Buffer.from(digest, 'base64').toString('hex').slice(0, 32));
 await verifyWindowsProgramTree(root, manifest);
 await writeFile(join(root, 'resources/app.asar'), 'changed after fuse/sign hook');
 await assert.rejects(verifyWindowsProgramTree(root, manifest), /manifest|changed/i);
 assert.throws(() => verifyWindowsProgramManifest(raw, hash(Buffer.from('another descriptor')), context), /hash/);
}));
test('manifest paths cannot select a different executable, ADS, device, traversal, case alias or unbounded work', async () => tree(async root => {
 const valid = JSON.parse((await createWindowsProgramManifest(root, context)).toString());
 for (const path of ['../outside', '/absolute', 'C:/other', 'resources\\other', 'a:stream', 'CON', 'con.txt', 'CON .txt', 'aux/X', 'name.', 'dir /file', 'dir/ file', 'dir//file', 'a\u0000b', 'é.dll', 'x'.repeat(101)]) {
  const doc = structuredClone(valid); doc.files[0].path = path; const raw = Buffer.from(JSON.stringify(doc));
  assert.throws(() => verifyWindowsProgramManifest(raw, hash(raw), context), /path|manifest/i, path);
 }
 const changes: Array<(doc: { files: Array<Record<string, unknown>>; [key: string]: unknown }) => void> = [doc => { doc.files.push({ ...doc.files[0], path: 'DILLA-DESKTOP.EXE' }); }, doc => { doc.files[0].size = 2 ** 53; }, doc => { doc.command = '/arbitrary'; }, doc => { doc.files[0].extra = true; }, doc => { doc.files.reverse(); }];
 for (const change of changes) {
  const doc = structuredClone(valid); change(doc); const raw = Buffer.from(JSON.stringify(doc));
  assert.throws(() => verifyWindowsProgramManifest(raw, hash(raw), context), /manifest|path|order/i);
 }
}));
test('exact program trees reject extra files, empty directories, hardlinks and linked directory escapes', async () => tree(async root => {
 const raw = await createWindowsProgramManifest(root, context), manifest = verifyWindowsProgramManifest(raw, hash(raw), context);
 await writeFile(join(root, 'extra.dll'), 'unlisted'); await assert.rejects(verifyWindowsProgramTree(root, manifest), /manifest|tree/i); await rm(join(root, 'extra.dll'));
 await mkdir(join(root, 'empty-directory')); await assert.rejects(verifyWindowsProgramTree(root, manifest), /manifest|directory/i); await rm(join(root, 'empty-directory'), { recursive: true });
 await link(join(root, 'dilla-desktop.exe'), join(root, 'alias.exe')); await assert.rejects(createWindowsProgramManifest(root, context), /link/i); await rm(join(root, 'alias.exe'));
 await symlink(join(root, 'resources'), join(root, 'redirected'), process.platform === 'win32' ? 'junction' : 'dir');
 await assert.rejects(createWindowsProgramManifest(root, context), /link|directory/i);
}));

test('manifest JSON rejects duplicate keys, invalid UTF8 and directory aliases before any filesystem action', async () => tree(async root => {
 const raw = await createWindowsProgramManifest(root, context), original = raw.toString();
 for (const bytes of [Buffer.from(original.replace('"format":1', '"format":1,"format":1')), Buffer.from([255, 254]), Buffer.from(original.replace('"resources/app.asar"', '"Resources/app.asar"').replace(']}', ',{"path":"resources/other","size":1,"sha512":"' + hash(Buffer.from('x')) + '"}]}'))]) {
  assert.throws(() => verifyWindowsProgramManifest(bytes, hash(bytes), context));
 }
}));

test('a noncanonical program-root alias remains rejected after fixture setup canonicalization', async () => tree(async root => {
 const raw = await createWindowsProgramManifest(root, context), manifest = verifyWindowsProgramManifest(raw, hash(raw), context);
 const alias = join(root, 'root-alias');
 await symlink(root, alias, process.platform === 'win32' ? 'junction' : 'dir');
 await assert.rejects(createWindowsProgramManifest(alias, context), /program tree root/);
 await assert.rejects(verifyWindowsProgramTree(alias, manifest), /program tree root/);
}));
