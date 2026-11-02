import { test } from 'node:test';
import { strict as assert } from 'node:assert';
import { mkdtemp, writeFile, chmod, stat, symlink, rm, readFile, mkdir }  from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { execFileSync } from 'node:child_process';
import { createRequire } from 'node:module';
import { aurRecipe, writeAurRecipe } from '../scripts/build-native-linux-packages.mjs';
const require = createRequire(import.meta.url);
test('native builder keeps stock packages separate from AppImage hooks', () => {
 const config = require('../electron-builder.native-linux.cjs');
 assert.deepEqual(config.linux.target.map(x => x.target), ['deb', 'rpm']);
 assert.equal(typeof config.afterPack, 'function');
 assert.equal(config.electronFuses.onlyLoadAppFromAsar, true);
 assert.ok(config.deb.fpm.includes('libpipewire-0.3-0t64 | libpipewire-0.3-0'));
 assert.ok(config.rpm.fpm.includes('libpipewire-0.3.so.0()(64bit)'));
});
test('AUR recipe pins exact immutable DEB and package version', () => {
 const hash = 'a'.repeat(64);
 const files = aurRecipe('0.1.0-beta.1', hash);
 assert.match(files.PKGBUILD, /pkgver=0\.1\.0beta1/);
 assert.match(files.PKGBUILD, /desktop-v0\.1\.0-beta\.1\/dilla-0\.1\.0-beta\.1-linux-x64\.deb/);
 assert.ok(files.PKGBUILD.includes(hash));
 assert.ok(files['.SRCINFO'].includes(hash));
 assert.match(files['.SRCINFO'], /depends = libpipewire\n/);
 assert.throws(() => aurRecipe('0.1.0-beta.1', 'SKIP'));
});

test('native packaging removes write access for non-owner without following links', { skip: process.platform !== 'linux' }, async () => {
 const root = await mkdtemp(join(tmpdir(), 'dilla-native-mode-'));
 try {
  const file = join(root, 'executable'), external = join(root, '..', 'dilla-mode-external-' + process.pid);
  await writeFile(file, 'x'); await chmod(file, 0o777);
  await writeFile(external, 'x'); await chmod(external, 0o666);
  try {
   await symlink(external, join(root, 'link'));
   await require('../electron-builder.native-linux.cjs').afterPack({ appOutDir: root });
   assert.equal((await stat(file)).mode & 0o777, 0o755);
   assert.equal((await stat(external)).mode & 0o777, 0o666);
  } finally { await rm(external); }
 } finally { await rm(root, { recursive: true, force: true }); }
});

test('RPM rendered postun preserves registrations on upgrade and removes them on final erase', { skip: process.platform !== 'linux' }, async () => {
 const root = await mkdtemp(join(tmpdir(), 'dilla-rpm-postun-'));
 try {
  const template = await readFile(new URL('../packaging/linux/rpm-after-remove.tpl', import.meta.url), 'utf8');
  const bin = join(root, 'bin'), profile = join(root, 'profile'), calls = join(root, 'calls');
  await mkdir(bin); await writeFile(profile, 'profile');
  await writeFile(join(bin, 'update-alternatives'), `#!/bin/sh\necho "$*" >> "${calls}"\n`, {mode: 0o755});
  await writeFile(join(bin, 'apparmor_status'), '#!/bin/sh\nexit 1\n', {mode: 0o755});
  const script = template.replaceAll('${executable}', 'dilla-desktop').replaceAll('${sanitizedProductName}', 'Dilla').replace('/etc/apparmor.d/dilla-desktop', profile);
  const path = join(root, 'postun'); await writeFile(path, script);
  const options = { env: { PATH: `${bin}:/usr/bin:/bin` } };
  execFileSync('bash', [path, '1'], options);
  assert.equal(await readFile(profile, 'utf8'), 'profile');
  await assert.rejects(readFile(calls));
  execFileSync('bash', [path, '0'], options);
  assert.match(await readFile(calls, 'utf8'), /--remove dilla-desktop \/opt\/Dilla\/dilla-desktop/);
  await assert.rejects(readFile(profile));
 } finally { await rm(root, { recursive: true, force: true }); }
});

test('recipe archive binds the supplied exact-named source bytes', { skip: process.platform !== 'linux' }, async () => {
 const root = await mkdtemp(join(tmpdir(), 'dilla-recipe-'));
 try {
  const deb = join(root, 'dilla-0.1.0-beta.1-linux-x64.deb');
  await writeFile(deb, 'fixture');
  const archive = await writeAurRecipe('0.1.0-beta.1', deb, join(root, 'recipe'));
  assert.deepEqual(execFileSync('tar', ['-tzf', archive], {encoding: 'utf8'}).trim().split('\n'), ['.SRCINFO', 'PKGBUILD']);
  assert.match(await readFile(join(root, 'recipe', 'PKGBUILD'), 'utf8'), /f16d05ec6b29248d2c61adb1e9263f78e4f7bace1b955014a2d17872cfe4064d/);
  await assert.rejects(writeAurRecipe('0.1.0-beta.1', join(root, 'wrong.deb'), join(root, 'bad')), /filename/);
 } finally { await rm(root, { recursive: true, force: true }); }
});

test('audio helper ELF dependency is represented in all native package metadata', { skip: process.platform !== 'linux' || !process.env.DILLA_NATIVE_AUDIO || !process.env.DILLA_NATIVE_DEB_CONTROL || !process.env.DILLA_NATIVE_RPM_REQUIRES }, async () => {
 const elf = execFileSync('readelf', ['-d', process.env.DILLA_NATIVE_AUDIO], { encoding: 'utf8' });
 assert.match(elf, /NEEDED.*\[libpipewire-0\.3\.so\.0\]/);
 const deb = await readFile(process.env.DILLA_NATIVE_DEB_CONTROL, 'utf8');
 const rpm = await readFile(process.env.DILLA_NATIVE_RPM_REQUIRES, 'utf8');
 assert.match(deb, /Depends:.*libpipewire-0\.3-0t64 \| libpipewire-0\.3-0/);
 assert.match(deb, /Depends:.*libgtk-3-0/);
 assert.match(rpm, /^libpipewire-0\.3\.so\.0\(\)\(64bit\)$/m);
 assert.match(rpm, /^gtk3$/m);
 assert.match(aurRecipe('0.1.0-beta.1', 'a'.repeat(64))['.SRCINFO'], /depends = libpipewire\n/);
});
