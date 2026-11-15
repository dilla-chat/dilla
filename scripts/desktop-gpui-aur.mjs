#!/usr/bin/env node
// The AUR recipe of the native Dilla desktop app, as a release asset only (GPUI release plan, Task 4; D4: nothing
// publishes it to aur.archlinux.org, by hand or automatically). Ported from beta.5's build-native-linux-packages.mjs
// (`git show desktop-v0.1.0-beta.5:packages/desktop/scripts/build-native-linux-packages.mjs`).
//
// The recipe is `dilla-bin` (provides and conflicts `dilla`, as beta.5's did) and installs the release's own tar.gz,
// pinned by its SHA-256, into the same layout as the deb and rpm: /opt/Dilla/dilla-desktop, /usr/bin/dilla-desktop,
// the desktop entry, metainfo and icons under /usr/share.
//
// The release version also maps to the deb and rpm versions here, so each orders above beta.5's (0.1.0-beta.5):
//   0.2.0-alpha.N -> deb 0.2.0~alpha.N-1, rpm 0.2.0-0.alpha.N.1, AUR 0.2.0alphaN-1
//
// Usage:
//   desktop-gpui-aur.mjs versions <version>                 prints DEB_VERSION, DEB_RELEASE, RPM_VERSION, RPM_RELEASE
//   desktop-gpui-aur.mjs recipe <version> <tarball> <out-dir>  writes <out-dir>/dilla-<v>-linux-x64.aur.tar.gz
//   desktop-gpui-aur.mjs verify <version> <tarball> <recipe>   checks a recipe archive pins exactly that tarball
import { createHash } from 'node:crypto';
import { lstat, open, readFile, writeFile } from 'node:fs/promises';
import { basename, join, resolve } from 'node:path';
import { gunzipSync, gzipSync } from 'node:zlib';

export const REPOSITORY = 'https://github.com/dilla-chat/dilla';
export const TAG_PREFIX = 'desktop-gpui-v';
export const DEPENDS = ['glibc', 'gcc-libs', 'libxcb', 'libxkbcommon', 'libxkbcommon-x11', 'alsa-lib', 'vulkan-icd-loader', 'wayland', 'libx11', 'libdrm', 'mesa', 'xdg-utils', 'hicolor-icon-theme'];
export const OPTDEPENDS = [
  'gnome-keyring: the Secret Service Dilla stores its database key in (required unless kwallet or keepassxc provides it)',
  'libpulse: calls through PulseAudio or PipeWire',
  'xdg-desktop-portal: file dialogs and printing',
  'noto-fonts-emoji: colour emoji',
];

/** The release version's package versions; only X.Y.Z-alpha.N (D1) is accepted. */
export function linuxPackageVersion(version) {
  const match = typeof version === 'string' && /^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)-alpha\.(0|[1-9]\d*)$/.exec(version);
  if (!match || !match.slice(1).every((v) => Number.isSafeInteger(Number(v)))) throw new Error(`unsupported release version: ${version}`);
  const base = `${match[1]}.${match[2]}.${match[3]}`;
  const alpha = match[4];
  return {
    deb: { version: `${base}~alpha.${alpha}`, release: '1' },
    rpm: { version: base, release: `0.alpha.${alpha}.1` },
    aur: { pkgver: `${base}alpha${alpha}`, pkgrel: '1' },
  };
}

export const tarballName = (version) => `dilla-${version}-linux-x64.tar.gz`;
export const recipeName = (version) => `dilla-${version}-linux-x64.aur.tar.gz`;

export function aurRecipe(version, sha256) {
  const v = linuxPackageVersion(version);
  if (!/^[a-f0-9]{64}$/.test(sha256)) throw new Error('invalid tarball SHA-256');
  const name = tarballName(version);
  const url = `${REPOSITORY}/releases/download/${TAG_PREFIX}${version}/${name}`;
  const q = (s) => `'${s}'`;
  const pkgbuild = `# Generated from the exact Dilla release tarball by scripts/desktop-gpui-aur.mjs. A release asset only:
# nothing publishes it to the AUR. Review before you build it.
pkgname=dilla-bin
pkgver=${v.aur.pkgver}
pkgrel=${v.aur.pkgrel}
pkgdesc='Dilla desktop client'
arch=('x86_64')
url='${REPOSITORY}'
license=('AGPL-3.0-or-later')
depends=(${DEPENDS.map(q).join(' ')})
optdepends=(${OPTDEPENDS.map(q).join('\n            ')})
provides=('dilla')
conflicts=('dilla')
options=('!strip' '!debug')
source=('${name}::${url}')
sha256sums=('${sha256}')

package() {
  cd "$srcdir/dilla-${version}"
  install -Dm755 dilla-desktop "$pkgdir/opt/Dilla/dilla-desktop"
  install -dm755 "$pkgdir/usr/bin"
  ln -s /opt/Dilla/dilla-desktop "$pkgdir/usr/bin/dilla-desktop"
  install -Dm644 dev.dilla.desktop.desktop "$pkgdir/usr/share/applications/dev.dilla.desktop.desktop"
  install -Dm644 dev.dilla.desktop.metainfo.xml "$pkgdir/usr/share/metainfo/dev.dilla.desktop.metainfo.xml"
  local f
  for f in icons/hicolor/*/apps/*; do
    install -Dm644 "$f" "$pkgdir/usr/share/$f"
  done
  for f in licenses/*; do
    install -Dm644 "$f" "$pkgdir/opt/Dilla/$f"
  done
}
`;
  const srcinfo = `pkgbase = dilla-bin
\tpkgdesc = Dilla desktop client
\tpkgver = ${v.aur.pkgver}
\tpkgrel = ${v.aur.pkgrel}
\turl = ${REPOSITORY}
\tarch = x86_64
\tlicense = AGPL-3.0-or-later
${DEPENDS.map((d) => `\tdepends = ${d}\n`).join('')}${OPTDEPENDS.map((d) => `\toptdepends = ${d}\n`).join('')}\tprovides = dilla
\tconflicts = dilla
\toptions = !strip
\toptions = !debug
\tsource = ${name}::${url}
\tsha256sums = ${sha256}

pkgname = dilla-bin
`;
  return { PKGBUILD: pkgbuild, '.SRCINFO': srcinfo };
}

const sha256Of = async (file) => createHash('sha256').update(await readFile(file)).digest('hex');

// One 512-byte ustar header for a regular file owned by root with mtime 0, mode 0644.
function tarHeader(name, size) {
  const h = Buffer.alloc(512);
  const put = (text, at, len) => h.write(text, at, len, 'utf8');
  const oct = (n, len) => n.toString(8).padStart(len - 1, '0');
  put(name, 0, 100);
  put(oct(0o644, 8), 100, 8);
  put(oct(0, 8), 108, 8);
  put(oct(0, 8), 116, 8);
  put(oct(size, 12), 124, 12);
  put(oct(0, 12), 136, 12);
  h.fill(0x20, 148, 156);
  h[156] = 0x30;
  put('ustar\0', 257, 6);
  put('00', 263, 2);
  put('root', 265, 32);
  put('root', 297, 32);
  const sum = h.reduce((a, b) => a + b, 0);
  put(`${sum.toString(8).padStart(6, '0')}\0 `, 148, 8);
  return h;
}

/** The recipe archive's exact bytes: .SRCINFO then PKGBUILD, sorted, root-owned, mtime 0, gzip with no name or time. */
export function recipeArchive(files) {
  const parts = [];
  for (const name of Object.keys(files).sort()) {
    const body = Buffer.from(files[name]);
    parts.push(tarHeader(name, body.length), body, Buffer.alloc((512 - (body.length % 512)) % 512));
  }
  parts.push(Buffer.alloc(1024));
  return gzipSync(Buffer.concat(parts), { level: 9 });
}

// Observe only the two bounded regular tar members; never extract or execute a recipe from a candidate. Canonical
// content pins the accepted release tarball.
export async function verifyAurRecipe(version, tarball, archive) {
  const expected = aurRecipe(version, await sha256Of(tarball));
  const info = await lstat(archive);
  if (!info.isFile() || info.isSymbolicLink() || info.nlink !== 1 || info.size < 1 || info.size > 65536) throw new Error('invalid AUR recipe archive');
  const handle = await open(archive, 'r');
  let compressed;
  try {
    const actual = await handle.stat();
    if (actual.ino !== info.ino || actual.dev !== info.dev || actual.size !== info.size) throw new Error('AUR recipe archive changed');
    compressed = Buffer.alloc(info.size + 1);
    let bytesRead = 0;
    while (bytesRead < compressed.length) {
      const chunk = await handle.read(compressed, bytesRead, compressed.length - bytesRead, bytesRead);
      if (!chunk.bytesRead) break;
      bytesRead += chunk.bytesRead;
    }
    if (bytesRead !== info.size) throw new Error('AUR recipe archive changed');
    compressed = compressed.subarray(0, bytesRead);
  } finally { await handle.close(); }
  let bytes;
  try { bytes = gunzipSync(compressed, { maxOutputLength: 65536 }); } catch { throw new Error('invalid bounded AUR recipe archive'); }
  const seen = new Set();
  let offset = 0;
  const field = (header, start, size) => header.subarray(start, start + size).toString('utf8').replace(/\0.*$/s, '');
  const octal = (value) => { if (!/^[0-7]+$/.test(value.trim())) throw new Error('invalid AUR recipe tar number'); return Number.parseInt(value.trim(), 8); };
  while (offset + 512 <= bytes.length && bytes.subarray(offset, offset + 512).some((b) => b !== 0)) {
    const header = bytes.subarray(offset, offset + 512);
    const name = field(header, 0, 100);
    const size = octal(field(header, 124, 12));
    const checksum = header.reduce((sum, b, i) => sum + (i >= 148 && i < 156 ? 32 : b), 0);
    if (checksum !== octal(field(header, 148, 8)) || !Object.hasOwn(expected, name) || seen.has(name) || ![0, 48].includes(header[156])
      || field(header, 157, 100) !== '' || field(header, 345, 155) !== '' || size < 1 || size > 16384 || offset + 512 + size > bytes.length) throw new Error('invalid AUR recipe member');
    const content = bytes.subarray(offset + 512, offset + 512 + size);
    if (!content.equals(Buffer.from(expected[name]))) throw new Error('AUR recipe does not pin the accepted version and tarball SHA-256');
    seen.add(name);
    offset += 512 + Math.ceil(size / 512) * 512;
  }
  if (seen.size !== 2 || bytes.length < offset + 1024 || bytes.subarray(offset).some((b) => b !== 0)) throw new Error('invalid AUR recipe inventory');
}

// Takes the release's own tarball; never downloads anything or pushes anywhere.
export async function writeAurRecipe(version, tarball, outDir) {
  linuxPackageVersion(version);
  if (basename(tarball) !== tarballName(version)) throw new Error('tarball file name does not match the version');
  const archive = join(outDir, recipeName(version));
  await writeFile(archive, recipeArchive(aurRecipe(version, await sha256Of(tarball))), { flag: 'wx' });
  await verifyAurRecipe(version, tarball, archive);
  return archive;
}

if (import.meta.url === `file://${process.argv[1]}`) {
  const [command, ...args] = process.argv.slice(2);
  if (command === 'versions' && args.length === 1) {
    const v = linuxPackageVersion(args[0]);
    console.log(`DEB_VERSION=${v.deb.version}\nDEB_RELEASE=${v.deb.release}\nRPM_VERSION=${v.rpm.version}\nRPM_RELEASE=${v.rpm.release}\nAUR_PKGVER=${v.aur.pkgver}`);
  } else if (command === 'recipe' && args.length === 3) {
    console.log(await writeAurRecipe(args[0], resolve(args[1]), resolve(args[2])));
  } else if (command === 'verify' && args.length === 3) {
    await verifyAurRecipe(args[0], resolve(args[1]), resolve(args[2]));
    console.log('the AUR recipe pins the release tarball');
  } else {
    console.error('usage: desktop-gpui-aur.mjs versions VERSION | recipe VERSION TARBALL OUT_DIR | verify VERSION TARBALL RECIPE');
    process.exit(1);
  }
}
