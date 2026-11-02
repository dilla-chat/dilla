import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, mkdir, writeFile, readFile, rm, cp } from 'node:fs/promises';
import { execFileSync } from 'node:child_process';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { createHash } from 'node:crypto';
import { verifyNativePackageInputs, comparePayloads, readNativePayload } from '../web/support/native-linux-package-acceptance.mjs';
import { createPackage, uncache } from '@electron/asar';
import { writeAurRecipe } from '../../packages/desktop/scripts/build-native-linux-packages.mjs';
const expected = { commit: 'a'.repeat(40), tree: 'b'.repeat(40), ciRun: 123, version: '1.2.3' };
test('acceptance binds actual same-run package bytes and canonical recipe before manager execution', async () => {
 const root = await mkdtemp(join(tmpdir(), 'dilla-format-input-'));
 try {
  const deb = join(root, 'dilla-1.2.3-linux-x64.deb'); await writeFile(deb, 'deb fixture');
  await writeFile(join(root, 'dilla-1.2.3-linux-x64.rpm'), 'rpm fixture');
  const recipe = await writeAurRecipe('1.2.3', deb, join(root, 'recipe'));
  await writeFile(join(root, 'dilla-1.2.3-linux-x64.aur.tar.gz'), await readFile(recipe));
  await rm(join(root, 'recipe'), { recursive: true });
  const artifacts = [];
  for (const ext of ['deb', 'rpm', 'aur.tar.gz']) { const name = `dilla-1.2.3-linux-x64.${ext}`, bytes = await readFile(join(root, name)); artifacts.push({ name, size: bytes.length, sha512: createHash('sha512').update(bytes).digest('base64') }); }
  const record = { format: 1, kind: 'dilla-desktop-build', repository: 'jonasthim/dilla', target: 'linux-packages-x64', version: expected.version, sourceCommit: expected.commit, sourceTree: expected.tree, ciRun: 123, sourceClean: true, artifacts };
  await writeFile(join(root, 'build-provenance-linux-packages-x64.json'), JSON.stringify(record));
  assert.equal((await verifyNativePackageInputs(root, expected)).sourceCommit, expected.commit);
  const copied = root + '-handoff';
  try {
   await cp(root, copied, { recursive: true });
   assert.deepEqual(await verifyNativePackageInputs(copied, expected), record, 'dedicated-user copy must retain exact source/run/package binding');
   await writeFile(join(copied, 'dilla-1.2.3-linux-x64.deb'), 'changed after handoff');
   await assert.rejects(verifyNativePackageInputs(copied, expected), /mismatch/);
  } finally { await rm(copied, { recursive: true, force: true }); }
  await assert.rejects(verifyNativePackageInputs(root, { ...expected, ciRun: 124 }), /binding/);
  await assert.rejects(verifyNativePackageInputs(root, { ...expected, tree: 'c'.repeat(40) }), /binding/);
  await writeFile(deb, 'tampered deb');
  await assert.rejects(verifyNativePackageInputs(root, expected), /mismatch/);
 } finally { await rm(root, { recursive: true, force: true }); }
});
test('runtime UID handoff clears runner HOME/XDG/temp and inherited credentials', async () => {
 const root = await mkdtemp(join(tmpdir(), 'dilla-format-env-'));
 try {
  const script = await readFile(new URL('../web/support/native-linux-package-run.sh', import.meta.url), 'utf8');
  const fn = /^run_user\(\) \{\n[\s\S]*?^\}/m.exec(script)?.[0]; assert.ok(fn);
  // Exercise the actual environment launch; replace only the forbidden local
  // UID transition/directory setup and the final test process.
  await writeFile(join(root, 'bash'), `#!${process.execPath}\nprocess.stdout.write(JSON.stringify(Object.fromEntries(['HOME','USER','LOGNAME','XDG_CONFIG_HOME','XDG_CACHE_HOME','XDG_DATA_HOME','XDG_RUNTIME_DIR','TMPDIR','RUNNER_TEMP','RUNNER_TOKEN','GITHUB_SHA','DILLA_NATIVE_ACCEPTANCE_OUTPUT'].map(k=>[k,process.env[k]??null]))));\n`, { mode: 0o755 });
  const command = `${fn}\ninstall() { :; }\nrunuser() { shift 3; USER=dilla-ci LOGNAME=dilla-ci "$@"; }\ninputs=/home/dilla-ci/native-linux-package-inputs\noutput=/home/dilla-ci/native-linux-package-acceptance\nformat=deb\nrun_user`;
  const observed = JSON.parse(execFileSync('/bin/bash', ['-euc', command], { encoding: 'utf8', env: { ...process.env, PATH: `${root}:${process.env.PATH}`, HOME: '/home/runner', XDG_CONFIG_HOME: '/home/runner/.config', XDG_RUNTIME_DIR: '/run/user/runner', TMPDIR: '/private/runner-temp', RUNNER_TEMP: '/private/runner-temp', RUNNER_TOKEN: 'poison-not-a-credential', GITHUB_REPOSITORY: 'jonasthim/dilla', GITHUB_SHA: expected.commit, GITHUB_RUN_ID: '123', GITHUB_RUN_ATTEMPT: '1' } }));
  assert.equal(observed.HOME, '/home/dilla-ci');
  assert.equal(observed.XDG_CONFIG_HOME, '/home/dilla-ci/.config');
  assert.equal(observed.USER, 'dilla-ci'); assert.equal(observed.LOGNAME, 'dilla-ci');
  assert.equal(observed.XDG_CACHE_HOME, '/home/dilla-ci/.cache');
  assert.equal(observed.XDG_DATA_HOME, '/home/dilla-ci/.local/share');
  assert.equal(observed.XDG_RUNTIME_DIR, '/home/dilla-ci/.run');
  assert.equal(observed.TMPDIR, '/home/dilla-ci/tmp');
  assert.equal(observed.RUNNER_TEMP, null); assert.equal(observed.RUNNER_TOKEN, null);
  assert.equal(observed.GITHUB_SHA, expected.commit);
  assert.equal(observed.DILLA_NATIVE_ACCEPTANCE_OUTPUT, '/home/dilla-ci/native-linux-package-acceptance/runtime.json');
  const installedBranch = /if \[\[ "\$\{1:-\}" == --host-installed \]\]; then\n([\s\S]*?)\nfi/.exec(script)?.[1];
  assert.ok(installedBranch); assert.doesNotMatch(installedBranch, /RUNNER_TEMP/);
  assert.match(installedBranch, /inputs=\/home\/dilla-ci\/native-linux-package-inputs/);
  assert.match(installedBranch, /output=\/home\/dilla-ci\/native-linux-package-acceptance/);
 } finally { await rm(root, { recursive: true, force: true }); }
});
test('payload equivalence rejects an RPM or installed format with different ASAR/core bytes', () => {
 const payload = { executable: 'a', asar: 'b', sandboxHelper: 'c', audioHelper: 'd', clientManifest: 'e', coreWasm: 'f' };
 assert.doesNotThrow(() => comparePayloads(payload, { ...payload }));
 for (const field of Object.keys(payload)) assert.throws(() => comparePayloads(payload, { ...payload, [field]: 'different' }), /payload/);
});
test('payload observation reads actual ASAR version and embedded shipping manifest/core', async () => {
 const root = await mkdtemp(join(tmpdir(), 'dilla-format-payload-'));
 try {
  const unpacked = join(root, 'asar-source'), app = join(root, 'opt/Dilla'), dist = join(root, 'web-dist');
  await mkdir(join(unpacked, 'dist/client/assets'), { recursive: true }); await mkdir(join(app, 'resources/native'), { recursive: true }); await mkdir(join(dist, 'assets'), { recursive: true });
  const name = 'dilla_core_wasm_bg-deadbeef.wasm', core = Buffer.from([0,97,115,109,1,0,0,0]);
  const manifest = JSON.stringify({ v: 1, files: [{ path: `assets/${name}`, size: core.length, sha256: createHash('sha256').update(core).digest('hex') }] });
  for (const dir of [dist, join(unpacked, 'dist/client')]) { await writeFile(join(dir, 'assets', name), core); await writeFile(join(dir, 'dilla-manifest.json'), manifest); }
  await writeFile(join(unpacked, 'package.json'), JSON.stringify({ version: '1.2.3' }));
  for (const file of ['dilla-desktop', 'chrome-sandbox', 'resources/native/dilla-audio']) await writeFile(join(app, file), file);
  const asar = join(app, 'resources/app.asar'); await createPackage(unpacked, asar);
  const observed = await readNativePayload(root, '1.2.3', dist);
  assert.equal(observed.coreWasm, createHash('sha256').update(core).digest('hex'));
  await assert.rejects(readNativePayload(root, '1.2.4', dist), /version/);
  await writeFile(join(unpacked, 'dist/client/assets', name), 'wrong core'); await createPackage(unpacked, asar); uncache(asar);
  await assert.rejects(readNativePayload(root, '1.2.3', dist), /shipping/);
 } finally { await rm(root, { recursive: true, force: true }); }
});
