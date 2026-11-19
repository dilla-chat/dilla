// scripts/desktop-gpui-package-linux.sh over a stand-in binary and stand-in tools: the asset set and its names, the
// tarball's layout, the guards' order, and the refusals. The real appimagetool and nfpm run in desktop-gpui-release.yml
// (and the smoke job installs what they make).
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { chmodSync, copyFileSync, existsSync, mkdirSync, mkdtempSync, readdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import test, { after } from 'node:test';
import { fileURLToPath } from 'node:url';

const root = join(dirname(fileURLToPath(import.meta.url)), '..');
const script = join(root, 'scripts', 'desktop-gpui-package-linux.sh');
const dir = mkdtempSync(join(tmpdir(), 'dilla-gpui-package-'));
after(() => rmSync(dir, { recursive: true, force: true }));
const V = '0.2.0-alpha.1';
const ASSETS = ['AppImage', 'aur.tar.gz', 'deb', 'rpm', 'tar.gz'].map((ext) => `dilla-${V}-linux-x64.${ext}`).sort();

// Stand-ins: appimagetool and nfpm log their arguments and write their target; the binary is a real dynamic glibc
// executable (the glibc check reads its symbol versions) with no test-only markers.
const tools = join(dir, 'tools');
mkdirSync(tools);
const log = join(dir, 'tools.log');
writeFileSync(join(tools, 'appimagetool'), `#!/bin/sh\necho "appimagetool $*" >> '${log}'\nfor last; do :; done\necho appimage > "$last"\n`);
writeFileSync(join(tools, 'nfpm'), `#!/bin/sh\necho "nfpm $* DILLA_PKG_VERSION=$DILLA_PKG_VERSION DILLA_PKG_RELEASE=$DILLA_PKG_RELEASE cwd=$(pwd)" >> '${log}'\nwhile [ "$#" -gt 0 ]; do [ "$1" = --target ] && echo pkg > "$2"; shift; done\n`);
chmodSync(join(tools, 'appimagetool'), 0o755);
chmodSync(join(tools, 'nfpm'), 0o755);
const runtime = join(dir, 'runtime');
writeFileSync(runtime, 'runtime');
const webrtcLicense = join(dir, 'webrtc-LICENSE.md');
writeFileSync(webrtcLicense, 'libwebrtc licence');
const metadata = join(dir, 'metadata.json');
writeFileSync(metadata, JSON.stringify({
  workspace_members: ['d'], packages: [{ id: 'd', name: 'dilla-desktop', version: V }, { id: 's', name: 'serde', version: '1.0.0', license: 'MIT' }],
  resolve: { nodes: [{ id: 'd', deps: [{ pkg: 's', dep_kinds: [{ kind: null }] }] }, { id: 's', deps: [] }] },
}));
const binary = join(dir, 'dilla-desktop');
copyFileSync('/usr/bin/true', binary);

let n = 0;
function run({ version = V, bin = binary, out = join(dir, `out${n++}`), env = {} } = {}) {
  rmSync(log, { force: true });
  const result = spawnSync('bash', [script, version, bin, out], {
    encoding: 'utf8',
    env: { ...process.env, DILLA_APPIMAGETOOL: join(tools, 'appimagetool'), DILLA_NFPM: join(tools, 'nfpm'), DILLA_APPIMAGE_RUNTIME: runtime, DILLA_WEBRTC_LICENSE: webrtcLicense, DILLA_CARGO_METADATA: metadata, ...env },
  });
  return { ...result, out, log: existsSync(log) ? readFileSync(log, 'utf8') : '' };
}

test('the script writes exactly the five assets, named dilla-<v>-linux-x64.*', () => {
  const r = run();
  assert.equal(r.status, 0, r.stderr);
  assert.deepEqual(readdirSync(r.out).sort(), ASSETS);
  assert.deepEqual(r.stdout.trim().split('\n').slice(-5).sort(), ASSETS);
});

test('the tarball is dilla-<v>/ with the binary, desktop entry, metainfo, icons and licences, root-owned, mtime 0', () => {
  const r = run();
  const listing = spawnSync('tar', ['-tvzf', join(r.out, `dilla-${V}-linux-x64.tar.gz`)], { encoding: 'utf8' }).stdout.trim().split('\n');
  const names = listing.map((l) => l.split(/\s+/).at(-1));
  for (const want of [`dilla-${V}/dilla-desktop`, `dilla-${V}/chat.dilla.Dilla.desktop`, `dilla-${V}/chat.dilla.Dilla.metainfo.xml`,
    `dilla-${V}/icons/hicolor/512x512/apps/chat.dilla.Dilla.png`, `dilla-${V}/icons/hicolor/scalable/apps/chat.dilla.Dilla.svg`,
    `dilla-${V}/licenses/LICENSE`, `dilla-${V}/licenses/THIRD-PARTY.md`, `dilla-${V}/licenses/libwebrtc-LICENSE.md`, `dilla-${V}/licenses/OFL-JetBrainsMono.txt`]) {
    assert.ok(names.includes(want), want);
  }
  assert.ok(names.every((name) => !name.includes('dev.dilla.desktop')), names.join('\n'));
  assert.ok(names.every((name) => name.startsWith(`dilla-${V}/`)), names.join('\n'));
  assert.ok(listing.every((l) => /^\S+ 0\/0 .* 1970-01-01 /.test(l)), listing.join('\n'));
  assert.match(listing.find((l) => l.endsWith('/dilla-desktop')), /^-rwxr-xr-x /);
  // Reproducible: the same inputs give the same bytes.
  const again = run();
  assert.deepEqual(readFileSync(join(again.out, `dilla-${V}-linux-x64.tar.gz`)), readFileSync(join(r.out, `dilla-${V}-linux-x64.tar.gz`)));
});

test('nfpm gets each format\'s versions, and appimagetool the pinned runtime', () => {
  const r = run();
  assert.match(r.log, /^nfpm package --config \S+\/clients\/desktop\/packaging\/linux\/nfpm\.yaml --packager deb --target \S+\.deb DILLA_PKG_VERSION=0\.2\.0~alpha\.1 DILLA_PKG_RELEASE=1 cwd=\S+\/dilla-0\.2\.0-alpha\.1$/m);
  assert.match(r.log, /^nfpm package --config \S+ --packager rpm --target \S+\.rpm DILLA_PKG_VERSION=0\.2\.0 DILLA_PKG_RELEASE=0\.alpha\.1\.1 cwd=/m);
  assert.match(r.log, new RegExp(`^appimagetool --runtime-file ${runtime} \\S+/Dilla\\.AppDir \\S+\\.AppImage$`, 'm'));
});

test('the AUR recipe in the assets pins the tarball', () => {
  const r = run();
  const verify = spawnSync(process.execPath, [join(root, 'scripts', 'desktop-gpui-aur.mjs'), 'verify', V, join(r.out, `dilla-${V}-linux-x64.tar.gz`), join(r.out, `dilla-${V}-linux-x64.aur.tar.gz`)], { encoding: 'utf8' });
  assert.equal(verify.status, 0, verify.stderr);
});

test('a binary with test-only code is refused before anything is packaged', () => {
  // The marker in an allocated read-only section, as a string literal sits in .rodata: strip keeps it.
  const dirty = join(dir, 'dirty');
  const marker = join(dir, 'marker');
  writeFileSync(marker, '\0--automation\0');
  const added = spawnSync('objcopy', ['--add-section', `.dillatest=${marker}`, '--set-section-flags', '.dillatest=alloc,readonly,contents', '/usr/bin/true', dirty], { encoding: 'utf8' });
  assert.equal(added.status, 0, added.stderr);
  chmodSync(dirty, 0o755);
  const r = run({ bin: dirty });
  assert.notEqual(r.status, 0);
  assert.match(r.stderr, /test-only feature is compiled in/);
  assert.equal(r.log, '', 'no tool ran');
  assert.deepEqual(readdirSync(r.out), []);
});

test('a non-glibc binary, a bad version, a non-empty out-dir and a missing runtime are refused', () => {
  const script = join(dir, 'script.sh');
  writeFileSync(script, '#!/bin/sh\n');
  assert.notEqual(run({ bin: script }).status, 0);
  for (const version of ['0.2.0', '0.2.0-beta.1', 'v0.2.0-alpha.1']) assert.notEqual(run({ version }).status, 0, version);
  const busy = join(dir, 'busy');
  mkdirSync(busy);
  writeFileSync(join(busy, 'stale'), 'x');
  assert.match(run({ out: busy }).stderr, /is not empty/);
  assert.notEqual(run({ env: { DILLA_APPIMAGE_RUNTIME: join(dir, 'absent') } }).status, 0);
});

test('the desktop entry, metainfo and nfpm recipe use chat.dilla.Dilla (KDE strips a trailing .desktop from the app id)', () => {
  const linux = join(root, 'clients', 'desktop', 'packaging', 'linux');
  const entry = readFileSync(join(linux, 'chat.dilla.Dilla.desktop'), 'utf8').split('\n');
  for (const line of ['Icon=chat.dilla.Dilla', 'StartupWMClass=chat.dilla.Dilla', 'MimeType=x-scheme-handler/dilla;', 'Exec=dilla-desktop %U']) assert.ok(entry.includes(line), line);
  const metainfo = readFileSync(join(linux, 'chat.dilla.Dilla.metainfo.xml'), 'utf8');
  assert.match(metainfo, /<id>chat\.dilla\.Dilla<\/id>/);
  assert.match(metainfo, /<launchable type="desktop-id">chat\.dilla\.Dilla\.desktop<\/launchable>/);
  const nfpm = readFileSync(join(linux, 'nfpm.yaml'), 'utf8');
  assert.ok(!nfpm.includes('dev.dilla.desktop'));
  for (const dst of ['usr/share/applications/chat.dilla.Dilla.desktop', 'usr/share/metainfo/chat.dilla.Dilla.metainfo.xml',
    ...['16x16', '22x22', '24x24', '32x32', '48x48', '64x64', '128x128', '256x256', '512x512'].map((s) => `usr/share/icons/hicolor/${s}/apps/chat.dilla.Dilla.png`),
    'usr/share/icons/hicolor/scalable/apps/chat.dilla.Dilla.svg']) assert.ok(nfpm.includes(`/${dst}`), dst);
});
