import test from 'node:test';
import assert from 'node:assert/strict';
import { parseLinuxAdmission } from '../src/linux-admission.ts';
const sha = Buffer.alloc(64).toString('base64');
const ready = { format: 1, manager: 'deb', state: 'ready', transaction: 'a'.repeat(64), identitySha512: sha, alpmHooks: null };
test('native admission accepts only exact ready snapshots and canonical manager/component bindings', () => {
 assert.deepEqual(parseLinuxAdmission(JSON.stringify(ready)), ready);
 const alpm = { ...ready, manager: 'alpm', alpmHooks: { upgradeSha512: sha, removeSha512: sha } };
 assert.deepEqual(parseLinuxAdmission(JSON.stringify(alpm)), alpm);
 for (const mutate of [
  (v: any) => v.format = 2, (v: any) => v.manager = ['deb'], (v: any) => v.manager = 'apt',
  (v: any) => v.state = 'pending', (v: any) => v.state = 'removed', (v: any) => v.state = 'unknown',
  (v: any) => v.transaction = 'A'.repeat(64), (v: any) => v.transaction += '\n', (v: any) => v.identitySha512 = null,
  (v: any) => v.identitySha512 = sha.slice(0, -3) + 'B==', (v: any) => v.extra = true,
  (v: any) => v.manager = 'alpm', (v: any) => v.alpmHooks = { upgradeSha512: sha, removeSha512: sha },
 ]) { const value = structuredClone(ready); mutate(value); assert.throws(() => parseLinuxAdmission(JSON.stringify(value))); }
 for (const hooks of [{ upgradeSha512: sha }, { upgradeSha512: sha, removeSha512: sha, extra: true }, { upgradeSha512: sha, removeSha512: 'bad' }, [sha, sha]]) assert.throws(() => parseLinuxAdmission(JSON.stringify({ ...alpm, alpmHooks: hooks })));
});
test('admission snapshots reject duplicate/escaped keys, fractional format, malformed UTF8 and unbounded input', () => {
 const raw = JSON.stringify(ready);
 for (const value of [raw.replace('"format":1', '"format":1,"format":1'), raw.replace('"format":1', '"format":1,"\\u0066ormat":1'), raw.replace('"format":1', '"format":1.0000000000000000001'), raw.replace('"format":1', '"format":1e0'), 'null', '['.repeat(70) + '0' + ']'.repeat(70), ' '.repeat(4097), Buffer.from([0xff])]) assert.throws(() => parseLinuxAdmission(value));
});

import { before, after } from 'node:test';
import { mkdtemp, mkdir, readFile, writeFile, chmod, rm, symlink, link, rename } from 'node:fs/promises';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { spawn, spawnSync, type ChildProcess } from 'node:child_process';
// @ts-ignore Build tooling is consumed directly by acceptance scripts.
import { buildLinuxAdmission } from '../scripts/build-linux-admission.mjs';
let root: string, directory: string, addon: string;
before(async () => {
 if (process.platform !== 'linux') return;
 root = await mkdtemp(join(tmpdir(), 'dilla-admission-')); await chmod(root, 0o755);
 directory = join(root, 'gate'); addon = join(root, 'admission.node');
 await buildLinuxAdmission({ output: addon, fixture: { directory, uid: process.getuid!(), gid: process.getgid!() } });
});
after(async () => { if (root) await rm(root, { recursive: true, force: true }); });
async function reset() {
 await rm(directory, { recursive: true, force: true }); await mkdir(directory, { mode: 0o755 });
 await writeFile(join(directory, 'lock'), '', { mode: 0o644 }); await writeFile(join(directory, 'status.json'), JSON.stringify(ready), { mode: 0o644 });
}
function client(arguments_: unknown[] = [], module = addon, method = 'hold') {
 const worker = `try{require(${JSON.stringify(module)}).hold();require('node:worker_threads').parentPort.postMessage({ok:true})}catch(e){require('node:worker_threads').parentPort.postMessage({ok:false,code:e.code})}`;
 const source = `const bridge=require(${JSON.stringify(module)});try{const raw=bridge[${JSON.stringify(method)}](...${JSON.stringify(arguments_)});process.send({ok:true,raw,exports:Object.getOwnPropertyNames(bridge)});}catch(e){process.send({ok:false,code:e.code,message:e.message});}process.on('message',m=>{if(m==='exit')process.exit(0);if(m==='child'){const p=require('node:child_process').spawn(process.execPath,['-e','setInterval(()=>{},1000)'],{stdio:'ignore'});p.unref();process.send({pid:p.pid});}if(m==='worker'){const w=new(require('node:worker_threads').Worker)(${JSON.stringify(worker)},{eval:true});w.on('message',r=>process.send(r));}if(m==='again'){try{process.send({ok:true,raw:bridge[${JSON.stringify(method)}]()})}catch(e){process.send({ok:false,code:e.code,message:e.message})}}});`;
 return spawn(process.execPath, ['-e', source], { stdio: ['ignore', 'ignore', 'pipe', 'ipc'], env: { ...process.env, APPIMAGE: '/spoofed.AppImage', DILLA_ADMISSION_DIRECTORY: directory } });
}
async function message(child: ChildProcess): Promise<any> {
 return new Promise((resolve, reject) => {
  const timer = setTimeout(() => reject(Error('admission child response timeout')), 3000);
  child.once('message', value => { clearTimeout(timer); resolve(value); }); child.once('error', error => { clearTimeout(timer); reject(error); });
 });
}
async function stop(child: ChildProcess) { child.kill(); await new Promise(done => child.once('exit', done)); }
function exclusive() { return spawnSync('flock', ['--exclusive', '--nonblock', join(directory, 'lock'), 'true']).status; }
const linuxOnly = { skip: process.platform !== 'linux' };
test('actual addon holds a kernel SH lock until process death, exposes only hold, and rereads protected snapshots', linuxOnly, async () => {
 await reset(); assert.equal(exclusive(), 0);
 const child = client();
 try {
  const first = await message(child); assert.equal(first.ok, true); assert.deepEqual(parseLinuxAdmission(first.raw), ready); assert.deepEqual(first.exports, ['hold']);
  assert.notEqual(exclusive(), 0);
  await writeFile(join(directory, 'status.json'), JSON.stringify({ ...ready, transaction: 'b'.repeat(64) }));
  child.send('again'); const next = await message(child); assert.equal(next.ok, true); assert.equal(parseLinuxAdmission(next.raw).transaction, 'b'.repeat(64));
  assert.notEqual(exclusive(), 0);
 } finally { await stop(child); }
 assert.equal(exclusive(), 0);
});

test('actual synchronous TS wrapper consumes the native capability and rejects pending before returning authority', linuxOnly, async () => {
 await reset();
 const { build } = await import('esbuild');
 const source = (await readFile(new URL('../src/linux-admission.ts', import.meta.url), 'utf8'))
  .replace(/const ADDON = '[^']+';/, 'const ADDON = ' + JSON.stringify(addon) + ';')
  .replace(/const ancestors = \[[^\n]+\];/, 'const ancestors = ' + JSON.stringify([root, directory]) + ';')
  .replaceAll('s.uid !== 0', 's.uid !== ' + process.getuid!()).replaceAll('s.gid !== 0', 's.gid !== ' + process.getgid!());
 const wrapper = join(root, 'wrapper.cjs');
 await build({ stdin: { contents: source, loader: 'ts', resolveDir: join(import.meta.dirname, '../src') }, outfile: wrapper, bundle: true, platform: 'node', format: 'cjs' });
 const child = client([], wrapper, 'holdLinuxAdmission');
 try { const result = await message(child); assert.equal(result.ok, true, result.message); assert.deepEqual(result.raw, ready); }
 finally { await stop(child); }
 await writeFile(join(directory, 'status.json'), JSON.stringify({ ...ready, state: 'pending' }));
 const held = client([], wrapper, 'holdLinuxAdmission');
 try { const result = await message(held); assert.equal(result.ok, false); assert.notEqual(exclusive(), 0, 'even invalid snapshot retains SH until process death'); }
 finally { await stop(held); }
 assert.equal(exclusive(), 0);
});

for (const [name, mutate, code] of [
 ['group-write directory ACL mask', async () => chmod(directory, 0o775), 'DILLA_ADMISSION_UNSAFE'],
 ['group-write lock ACL mask', async () => chmod(join(directory, 'lock'), 0o664), 'DILLA_ADMISSION_UNSAFE'],
 ['setuid status', async () => chmod(join(directory, 'status.json'), 0o4644), 'DILLA_ADMISSION_UNSAFE'],
 ['lock symlink', async () => { await rename(join(directory, 'lock'), join(directory, 'real-lock')); await symlink('real-lock', join(directory, 'lock')); }, 'DILLA_ADMISSION_UNSAFE'],
 ['status symlink', async () => { await rename(join(directory, 'status.json'), join(directory, 'real-status')); await symlink('real-status', join(directory, 'status.json')); }, 'DILLA_ADMISSION_UNSAFE'],
 ['hardlinked lock', async () => link(join(directory, 'lock'), join(directory, 'lock-alias')), 'DILLA_ADMISSION_UNSAFE'],
 ['hardlinked status', async () => link(join(directory, 'status.json'), join(directory, 'status-alias')), 'DILLA_ADMISSION_UNSAFE'],
 ['nonregular status', async () => { await rm(join(directory, 'status.json')); await mkdir(join(directory, 'status.json')); }, 'DILLA_ADMISSION_UNSAFE'],
 ['oversized status', async () => writeFile(join(directory, 'status.json'), ' '.repeat(4097)), 'DILLA_ADMISSION_UNSAFE'],
 ['invalid UTF8 status', async () => writeFile(join(directory, 'status.json'), Buffer.from([0xed, 0xa0, 0x80])), 'DILLA_ADMISSION_STATUS'],
] as const) test('actual addon refuses ' + name, linuxOnly, async () => {
 await reset(); await mutate(); const child = client();
 try { const result = await message(child); assert.equal(result.ok, false); assert.equal(result.code, code); }
 finally { await stop(child); }
});
test('actual addon refuses a gate with the wrong expected owner or group', linuxOnly, async () => {
 await reset();
 for (const [name, uid, gid] of [
  ['uid', process.getuid!() + 1, process.getgid!()],
  ['gid', process.getuid!(), process.getgid!() + 1],
 ] as const) {
  const mismatched = join(root, `wrong-${name}.node`);
  await buildLinuxAdmission({ output: mismatched, fixture: { directory, uid, gid } });
  const child = client([], mismatched);
  try { assert.equal((await message(child)).code, 'DILLA_ADMISSION_UNSAFE'); }
  finally { await stop(child); }
 }
});
test('actual addon refuses caller arguments and worker-thread ownership', linuxOnly, async () => {
 await reset(); const invalid = client([directory]);
 try { assert.equal((await message(invalid)).code, 'DILLA_ADMISSION_ARGUMENT'); }
 finally { await stop(invalid); }
 const child = client();
 try {
  assert.equal((await message(child)).ok, true); child.send('worker');
  const result = await message(child); assert.equal(result.ok, false); assert.equal(result.code, 'DILLA_ADMISSION_THREAD');
  assert.notEqual(exclusive(), 0, 'a worker rejection never releases the main lock');
 } finally { await stop(child); }
});
test('actual addon refuses an exclusively held gate without blocking and may retry after the actor exits', linuxOnly, async () => {
 await reset();
 const actor = spawn('flock', ['--no-fork', '--exclusive', '--nonblock', join(directory, 'lock'), process.execPath, '-e', "process.stdout.write('locked');setInterval(()=>{},1000)"], { stdio: ['ignore', 'pipe', 'ignore'] });
 await new Promise(done => actor.stdout!.once('data', done));
 const child = client();
 try {
  assert.equal((await message(child)).code, 'DILLA_ADMISSION_BUSY');
 } finally { await stop(child); await stop(actor); }
 assert.equal(exclusive(), 0);
});
test('CLOEXEC prevents an unrelated live child retaining admission after main exits', linuxOnly, async () => {
 await reset(); const child = client(); let descendant: number | undefined;
 try {
  assert.equal((await message(child)).ok, true); child.send('child'); descendant = (await message(child)).pid;
  assert.ok(descendant); assert.notEqual(exclusive(), 0); await stop(child);
  assert.equal(exclusive(), 0); process.kill(descendant!, 0);
 } finally { if (child.exitCode === null && child.signalCode === null) await stop(child); if (descendant) process.kill(descendant); }
});
test('idempotent admission refuses replacement of the never-replaced lock inode', linuxOnly, async () => {
 await reset(); const child = client();
 try {
  assert.equal((await message(child)).ok, true);
  await rename(join(directory, 'lock'), join(directory, 'retired-lock'));
  await writeFile(join(directory, 'lock'), '', { mode: 0o644 });
  child.send('again'); assert.equal((await message(child)).code, 'DILLA_ADMISSION_CHANGED');
  assert.notEqual(spawnSync('flock', ['--exclusive', '--nonblock', join(directory, 'retired-lock'), 'true']).status, 0);
 } finally { await stop(child); }
});
