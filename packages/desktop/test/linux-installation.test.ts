import test, { mock } from 'node:test';
import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { mkdtemp, mkdir, writeFile, rm, open, lstat, readdir, readlink } from 'node:fs/promises';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { updateFS } from '../src/update-fs.ts';
import { parseNativePackageIdentity, resolveLinuxInstallation, assertLinuxInstallationRole, type NativePackageIdentity } from '../src/linux-installation.ts';
const root = '/usr/lib/dilla/native';
const context = { installKind: 'system-package' as const, version: '0.1.0-beta.10', channel: 'beta' as const, arch: 'x64', executable: root + '/app/dilla-desktop', resourcesPath: root + '/app/resources' };
const contents: Record<string, string> = { 'app/dilla-desktop': 'electron', 'app/resources/app.asar': 'application', 'chrome-sandbox': 'sandbox', 'package-maintainer': 'maintainer' };
const hash = (value: string) => createHash('sha512').update(value).digest('base64');
function identity(): NativePackageIdentity {
 return { format: 1, installKind: 'system-package', manager: 'deb', upstreamVersion: context.version, packageVersion: '0.1.0~beta.10-1', arch: 'x64', channel: 'beta',
  payload: Object.entries(contents).map(([path, value]) => ({ path, size: Buffer.byteLength(value), sha512: hash(value) })),
  helper: { generation: 1, compatibilityEpoch: 1, sandboxApi: 1, size: 7, sha512: hash('sandbox') } };
}
const encode = (value: unknown) => Buffer.from(JSON.stringify(value));
test('strict identity binds exact schema, versions, channel, helper tuple and payload', () => {
 assert.deepEqual(parseNativePackageIdentity(encode(identity()), context), identity());
 for (const mutate of [
  (value: any) => value.format = 2, (value: any) => value.extra = true,
  (value: any) => value.installKind = 'appimage', (value: any) => value.manager = 'apt',
  (value: any) => value.packageVersion = '0.1.0-1', (value: any) => value.channel = 'stable',
  (value: any) => value.arch = 'arm64', (value: any) => value.helper.generation = -1,
  (value: any) => value.helper.compatibilityEpoch = 0, (value: any) => value.helper.sandboxApi = 2,
  (value: any) => value.helper.sha512 = hash('different'),
  (value: any) => value.helper.size = 8 * 1024 ** 2 + 1,
  (value: any) => value.payload[0].path = '../app/dilla-desktop',
  (value: any) => value.payload[0].path = '/usr/bin/dilla',
  (value: any) => value.payload[0].path = 'app//dilla-desktop',
  (value: any) => value.payload[0].path = 'app/\ud800',
  (value: any) => value.helper.generation = Number.MAX_SAFE_INTEGER + 1,
  (value: any) => { value.helper.size = 8 * 1024 ** 2 + 1; value.payload.find((file: any) => file.path === 'chrome-sandbox').size = value.helper.size; },
  (value: any) => value.payload[0].sha512 = value.payload[0].sha512.slice(0, -3) + 'B==',
  (value: any) => value.payload[0].size = 2 ** 53,
  (value: any) => value.payload.push(value.payload[0]),
  (value: any) => value.payload.reverse(),
  (value: any) => value.payload.splice(0, 1),
  (value: any) => value.payload.push({ path: 'app/resources', size: 0, sha512: hash('') }),
 ]) { const value = identity(); mutate(value); assert.throws(() => parseNativePackageIdentity(encode(value), context)); }
 for (const raw of [Buffer.from('{"format":1,"format":1}'), Buffer.from('{"format":1,"\\u0066ormat":1}'), Buffer.alloc(1_048_577, 32), Buffer.from([0xff]), Buffer.from('null'), Buffer.from('['.repeat(18) + '0' + ']'.repeat(18)), Buffer.from(JSON.stringify(identity()).replace('\"generation\":1', '\"generation\":1,\"generation\":1'))]) assert.throws(() => parseNativePackageIdentity(raw, context));
 for (const [manager, packageVersion] of [['rpm', '0.1.0-0.beta.10.1'], ['alpm', '0.1.0beta10-1']] as const) {
  const value = { ...identity(), manager, packageVersion }; assert.deepEqual(parseNativePackageIdentity(encode(value), context), value);
 }
});

test('manager identity requires a string enum without JavaScript coercion', () => {
 const alpm = { ...identity(), manager: 'alpm', packageVersion: '0.1.0beta10-1' };
 assert.equal(parseNativePackageIdentity(encode(alpm), context).manager, 'alpm');
 for (const manager of [['alpm'], [['alpm']], { toString: 'alpm' }]) {
  assert.throws(() => parseNativePackageIdentity(encode({ ...alpm, manager }), context));
 }
});

test('identity integer tokens cannot round fractions into authenticated fields', () => {
 const valid = JSON.stringify(identity());
 for (const [field, integer] of [['format', 1], ['generation', 1], ['compatibilityEpoch', 1], ['sandboxApi', 1], ['size', 8]] as const) {
  for (const token of [String(integer) + '.0000000000000001', String(integer) + 'e0']) {
   const changed = valid.replace(`"${field}":${integer}`, `"${field}":${token}`);
   assert.notEqual(changed, valid);
   assert.throws(() => parseNativePackageIdentity(Buffer.from(changed), context), field + ': ' + token);
  }
 }
});

// Portable fixtures model protected Linux metadata while hashing actual open files.
// They cannot prove installed root ownership, ACL enforcement, package hooks or sandbox startup.
async function fixture(t: any, alteration: { path?: string; uid?: number; gid?: number; mode?: number; nlink?: number; symlink?: boolean; extra?: boolean; contents?: string } = {}) {
 const directory = await mkdtemp(join(tmpdir(), 'dilla-native-'));
 for (const [path, value] of Object.entries({ ...contents, 'package.json': JSON.stringify(identity()) })) {
  await mkdir(join(directory, path, '..'), { recursive: true }); await writeFile(join(directory, path), alteration.path === root + '/' + path && alteration.contents !== undefined ? alteration.contents : value);
 }
 t.after(async () => { mock.restoreAll(); await rm(directory, { recursive: true, force: true }); });
 const fixed = (path: string) => {
  assert.ok(path === '/' || path === '/usr' || path === '/usr/lib' || path === '/usr/lib/dilla' || path.startsWith(root), 'resolver selected unexpected path: ' + path);
  return path.startsWith(root) ? join(directory, path.slice(root.length)) : directory;
 };
 function metadata(path: string, stat: any) {
  const mode = path === root + '/chrome-sandbox' ? 0o4755 : stat.isDirectory() || path === context.executable || path === root + '/package-maintainer' ? 0o755 : 0o644;
  const change = path === alteration.path ? alteration : {};
  return Object.assign(Object.create(Object.getPrototypeOf(stat)), stat, { uid: change.uid ?? 0, gid: change.gid ?? 0, nlink: change.nlink ?? stat.nlink, mode: (stat.mode & ~0o7777) | (change.mode ?? mode), isSymbolicLink: () => change.symlink ?? false });
 }
 mock.method(updateFS.promises, 'lstat', async (path: string) => {
  if (path === root + '/app/chrome-sandbox') return { uid: 0, gid: 0, isSymbolicLink: () => true };
  return metadata(path, await lstat(fixed(path)));
 });
 mock.method(updateFS.promises, 'open', async (path: string, flags: number) => {
  const handle = await open(fixed(path), flags), stat = handle.stat.bind(handle);
  handle.stat = async () => metadata(path, await stat());
  return handle;
 });
 mock.method(updateFS.promises, 'readdir', async (path: string) => {
  const result = await readdir(fixed(path));
  if (path === root + '/app') result.push('chrome-sandbox');
  if (alteration.extra && path === root + '/app') result.push('unlisted.js');
  return result;
 });
 mock.method(updateFS.promises, 'readlink', async (path: string) => path === root + '/app/chrome-sandbox' ? root + '/chrome-sandbox' : readlink(fixed(path)));
 return directory;
}
test('resolves only the fixed complete root-owned native tree', async t => {
 await fixture(t);
 const installation = await resolveLinuxInstallation(context);
 assert.equal(installation.installKind, 'system-package');
 assert.deepEqual(installation.identity, identity());
 assertLinuxInstallationRole(installation, 'check');
 for (const role of ['download', 'install', 'recovery', 'watchdog', 'sandbox-setup'] as const) assert.throws(() => assertLinuxInstallationRole(installation, role), /package manager/);
});
for (const alteration of [
 { path: '/usr/lib/dilla', mode: 0o777 }, { path: root, mode: 0o775 }, { path: root, uid: 1000 },
 { path: root + '/package.json', uid: 1000 }, { path: root + '/package.json', mode: 0o664 },
 { path: root + '/package.json', symlink: true }, { path: root + '/package.json', nlink: 2 },
 { path: context.executable, nlink: 2 },
 { path: context.executable, gid: 1000 }, { path: context.executable, mode: 0o775 },
 { path: root + '/app/resources/app.asar', contents: 'tampered!!!' },
 { path: root + '/chrome-sandbox', mode: 0o755 },
 { path: root + '/package-maintainer', mode: 0o4755 },
 { path: root + '/app/resources', symlink: true }, { extra: true },
]) test('holds native startup for untrusted tree: ' + JSON.stringify(alteration), async t => {
 await fixture(t, alteration); await assert.rejects(resolveLinuxInstallation(context));
});
test('native build cannot be converted to AppImage by environment, argv or marker', async t => {
 t.mock.method(updateFS.promises, 'lstat', async () => { throw new Error('no native package'); });
 const previous = process.env.APPIMAGE; process.env.APPIMAGE = '/home/user/Dilla.AppImage';
 process.argv.push('--appimage', '--update-recovery', '/home/user/forged-marker.json');
 try { await assert.rejects(resolveLinuxInstallation(context), /no native package/); }
 finally { process.argv.splice(-3); if (previous === undefined) delete process.env.APPIMAGE; else process.env.APPIMAGE = previous; }
});
test('unknown builds and wrong native executable/resources fail before touching disk', async t => {
 const read = t.mock.method(updateFS.promises, 'lstat', async () => { throw new Error('unexpected filesystem read'); });
 for (const value of [{ ...context, installKind: 'unknown' as const }, { ...context, executable: '/home/user/dilla' }, { ...context, resourcesPath: '/home/user/resources' }]) await assert.rejects(resolveLinuxInstallation(value), /installation|layout/);
 await assert.rejects(resolveLinuxInstallation({ ...context, installKind: 'appimage' }), /installation|layout/);
 assert.equal(read.mock.callCount(), 0);
});
