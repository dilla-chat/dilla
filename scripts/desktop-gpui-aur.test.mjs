import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test, { after } from 'node:test';
import { gzipSync } from 'node:zlib';
import { aurRecipe, linuxPackageVersion, recipeArchive, verifyAurRecipe, writeAurRecipe } from './desktop-gpui-aur.mjs';

const dir = mkdtempSync(join(tmpdir(), 'dilla-gpui-aur-'));
after(() => rmSync(dir, { recursive: true, force: true }));
const V = '0.2.0-alpha.1';
const SHA = 'a'.repeat(64);
let n = 0;
function tarball(bytes = 'release tarball bytes', version = V) {
  const d = join(dir, `t${n++}`);
  mkdirSync(d);
  const path = join(d, `dilla-${version}-linux-x64.tar.gz`);
  writeFileSync(path, bytes);
  return path;
}
const newDir = () => { const d = join(dir, `o${n++}`); mkdirSync(d); return d; };

test('release versions map to package versions that order above beta.5', () => {
  assert.deepEqual(linuxPackageVersion('0.2.0-alpha.1'), {
    deb: { version: '0.2.0~alpha.1', release: '1' },
    rpm: { version: '0.2.0', release: '0.alpha.1.1' },
    aur: { pkgver: '0.2.0alpha1', pkgrel: '1' },
  });
  assert.equal(linuxPackageVersion('1.10.3-alpha.12').aur.pkgver, '1.10.3alpha12');
  for (const bad of ['0.2.0', '0.2.0-beta.5', '0.2.0-alpha', '0.2.0-alpha.01', 'v0.2.0-alpha.1', '0.2.0-alpha.1\n', '0.2-alpha.1', 7]) {
    assert.throws(() => linuxPackageVersion(bad), /unsupported release version/, String(bad));
  }
});

// dpkg --compare-versions and rpmdev-vercmp are not on every machine; the smoke tests run the real tools. These are
// the orderings those tools apply, checked on the strings the mapping produces.
test('the deb version sorts above beta.5 by dpkg\'s tilde rule, the rpm release by its leading 0. part', () => {
  const v = linuxPackageVersion(V);
  assert.equal(`${v.deb.version}-${v.deb.release}`, '0.2.0~alpha.1-1');
  assert.equal(`${v.rpm.version}-${v.rpm.release}`, '0.2.0-0.alpha.1.1');
  // The upstream part 0.2.0 > 0.1.0 decides every format before the pre-release part is compared.
  assert.ok(v.deb.version.startsWith('0.2.0') && v.rpm.version === '0.2.0' && v.aur.pkgver.startsWith('0.2.0'));
});

test('the recipe is dilla-bin, provides and conflicts dilla, and pins the release tarball', () => {
  const { PKGBUILD, '.SRCINFO': srcinfo } = aurRecipe(V, SHA);
  for (const line of ['pkgname=dilla-bin', 'pkgver=0.2.0alpha1', 'pkgrel=1', "pkgdesc='Dilla desktop client'", "provides=('dilla')", "conflicts=('dilla')", "options=('!strip' '!debug')", `sha256sums=('${SHA}')`,
    "source=('dilla-0.2.0-alpha.1-linux-x64.tar.gz::https://github.com/dilla-chat/dilla/releases/download/desktop-gpui-v0.2.0-alpha.1/dilla-0.2.0-alpha.1-linux-x64.tar.gz')",
    "depends=('glibc' 'gcc-libs' 'libxcb' 'libxkbcommon' 'libxkbcommon-x11' 'alsa-lib' 'libpipewire' 'glib2' 'vulkan-icd-loader' 'wayland' 'libx11' 'libdrm' 'mesa' 'xdg-utils' 'hicolor-icon-theme')"]) {
    assert.ok(PKGBUILD.split('\n').includes(line), line);
  }
  assert.match(PKGBUILD, /'gnome-keyring: the Secret Service/);
  assert.match(PKGBUILD, /install -Dm755 dilla-desktop "\$pkgdir\/opt\/Dilla\/dilla-desktop"/);
  assert.match(PKGBUILD, /ln -s \/opt\/Dilla\/dilla-desktop "\$pkgdir\/usr\/bin\/dilla-desktop"/);
  assert.doesNotMatch(PKGBUILD, /chrome-sandbox|aur\.archlinux\.org|git push/);
  for (const line of ['pkgbase = dilla-bin', '\tpkgver = 0.2.0alpha1', '\tprovides = dilla', '\tconflicts = dilla', `\tsha256sums = ${SHA}`, 'pkgname = dilla-bin']) {
    assert.ok(srcinfo.split('\n').includes(line), line);
  }
  assert.throws(() => aurRecipe(V, 'A'.repeat(64)), /invalid tarball SHA-256/);
});

test('the recipe installs the desktop entry, metainfo and icons under chat.dilla.Dilla, never dev.dilla.desktop', () => {
  const { PKGBUILD } = aurRecipe(V, SHA);
  assert.match(PKGBUILD, /install -Dm644 chat\.dilla\.Dilla\.desktop "\$pkgdir\/usr\/share\/applications\/chat\.dilla\.Dilla\.desktop"/);
  assert.match(PKGBUILD, /install -Dm644 chat\.dilla\.Dilla\.metainfo\.xml "\$pkgdir\/usr\/share\/metainfo\/chat\.dilla\.Dilla\.metainfo\.xml"/);
  assert.ok(!PKGBUILD.includes('dev.dilla.desktop'));
});

test('the recipe archive is reproducible and holds exactly .SRCINFO and PKGBUILD', () => {
  const files = aurRecipe(V, SHA);
  assert.deepEqual(recipeArchive(files), recipeArchive(files));
  const out = newDir();
  writeFileSync(join(out, 'r.tar.gz'), recipeArchive(files));
  const listed = spawnSync('tar', ['-tvzf', join(out, 'r.tar.gz')], { encoding: 'utf8' });
  assert.equal(listed.status, 0, listed.stderr);
  const lines = listed.stdout.trim().split('\n');
  assert.equal(lines.length, 2);
  assert.match(lines[0], /^-rw-r--r-- root\/root .* 1970-01-01 \S+ \.SRCINFO$/);
  assert.match(lines[1], /^-rw-r--r-- root\/root .* 1970-01-01 \S+ PKGBUILD$/);
  const extracted = newDir();
  assert.equal(spawnSync('tar', ['-xzf', join(out, 'r.tar.gz'), '-C', extracted]).status, 0);
  assert.equal(readFileSync(join(extracted, 'PKGBUILD'), 'utf8'), files.PKGBUILD);
});

test('writeAurRecipe pins the tarball it is given, and verify accepts only that tarball', async () => {
  const t = tarball();
  const out = newDir();
  const archive = await writeAurRecipe(V, t, out);
  assert.equal(archive, join(out, 'dilla-0.2.0-alpha.1-linux-x64.aur.tar.gz'));
  const sha = createHash('sha256').update(readFileSync(t)).digest('hex');
  const extracted = newDir();
  spawnSync('tar', ['-xzf', archive, '-C', extracted]);
  assert.match(readFileSync(join(extracted, 'PKGBUILD'), 'utf8'), new RegExp(`sha256sums=\\('${sha}'\\)`));
  await verifyAurRecipe(V, t, archive);
  await assert.rejects(verifyAurRecipe(V, tarball('another tarball'), archive), /does not pin/);
  await assert.rejects(writeAurRecipe(V, t, out), /EEXIST/, 'never overwrites');
  await assert.rejects(writeAurRecipe(V, tarball('x', '0.2.0-alpha.2'), newDir()), /does not match the version/);
});

test('verify refuses an extra member, a missing member and an oversized archive', async () => {
  const t = tarball();
  const sha = createHash('sha256').update(readFileSync(t)).digest('hex');
  const files = aurRecipe(V, sha);
  const cases = {
    extra: recipeArchive({ ...files, 'evil.install': 'rm -rf /' }),
    missing: recipeArchive({ PKGBUILD: files.PKGBUILD }),
    edited: recipeArchive({ ...files, PKGBUILD: files.PKGBUILD.replace('!strip', 'strip') }),
    oversized: gzipSync(Buffer.alloc(70000), { level: 0 }),
    garbage: Buffer.from('not gzip'),
  };
  for (const [name, bytes] of Object.entries(cases)) {
    const archive = join(newDir(), 'r.aur.tar.gz');
    writeFileSync(archive, bytes);
    await assert.rejects(verifyAurRecipe(V, t, archive), /AUR recipe/i, name);
  }
});

test('the CLI prints the package versions', () => {
  const r = spawnSync(process.execPath, [new URL('./desktop-gpui-aur.mjs', import.meta.url).pathname, 'versions', V], { encoding: 'utf8' });
  assert.equal(r.status, 0, r.stderr);
  assert.equal(r.stdout, 'DEB_VERSION=0.2.0~alpha.1\nDEB_RELEASE=1\nRPM_VERSION=0.2.0\nRPM_RELEASE=0.alpha.1.1\nAUR_PKGVER=0.2.0alpha1\n');
});
