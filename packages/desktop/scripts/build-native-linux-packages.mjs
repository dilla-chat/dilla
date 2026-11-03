import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { mkdir, readFile, writeFile, lstat, open } from 'node:fs/promises';
import { basename, resolve, join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { gunzipSync } from 'node:zlib';
import { linuxPackageVersion } from './linux-package-version.mjs';
import { isMain } from './is-main.mjs';

export function aurRecipe(version, sha256) {
 const v = linuxPackageVersion(version, 'x64');
 if (!/^[a-f0-9]{64}$/.test(sha256)) throw new Error('Invalid DEB SHA256');
 const name = `dilla-${version}-linux-x64.deb`;
 const url = `https://github.com/dilla-chat/dilla/releases/download/desktop-v${version}/${name}`;
 const dependencies = ['alsa-lib', 'gtk3', 'libnotify', 'nss', 'libxss', 'libxtst', 'xdg-utils', 'at-spi2-core', 'libdrm', 'libxkbcommon', 'mesa', 'libsecret', 'libpipewire'];
 const pkgbuild = `# Generated from the exact Dilla release DEB. Review before publishing.\npkgname=dilla-bin\npkgver=${v.aur.pkgver}\npkgrel=1\npkgdesc='Dilla desktop client'\narch=('x86_64')\nurl='https://github.com/dilla-chat/dilla'\nlicense=('AGPL-3.0-or-later')\ndepends=(${dependencies.map(x => `'${x}'`).join(' ')})\nprovides=('dilla')\nconflicts=('dilla')\noptions=('!strip')\nsource=('${name}::${url}')\nsha256sums=('${sha256}')\n\npackage() {\n  bsdtar -xf "$srcdir/${name}" -C "$srcdir" 'data.tar.*'\n  bsdtar -xf "$srcdir"/data.tar.* -C "$pkgdir"\n  chmod 4755 "$pkgdir/opt/Dilla/chrome-sandbox"\n  install -dm755 "$pkgdir/usr/bin"\n  ln -s /opt/Dilla/dilla-desktop "$pkgdir/usr/bin/dilla-desktop"\n}\n`;
 const srcinfo = `pkgbase = dilla-bin\n\tpkgdesc = Dilla desktop client\n\tpkgver = ${v.aur.pkgver}\n\tpkgrel = 1\n\turl = https://github.com/dilla-chat/dilla\n\tarch = x86_64\n\tlicense = AGPL-3.0-or-later\n${dependencies.map(x => `\tdepends = ${x}\n`).join('')}\tprovides = dilla\n\tconflicts = dilla\n\toptions = !strip\n\tsource = ${name}::${url}\n\tsha256sums = ${sha256}\n\npkgname = dilla-bin\n`;
 return { PKGBUILD: pkgbuild, '.SRCINFO': srcinfo };
}

// Observe only the two bounded regular tar members; never extract or execute a
// recipe from a candidate. Canonical content pins the accepted release DEB.
export async function verifyAurRecipe(version, deb, archive) {
 const expected = aurRecipe(version, createHash('sha256').update(await readFile(deb)).digest('hex'));
 const info = await lstat(archive);
 if (!info.isFile() || info.isSymbolicLink() || info.nlink !== 1 || info.size < 1 || info.size > 65536) throw new Error('Invalid AUR recipe archive');
 const handle = await open(archive, 'r'); let compressed;
 try {
  const actual = await handle.stat();
  if (actual.ino !== info.ino || actual.dev !== info.dev || actual.size !== info.size) throw new Error('AUR recipe archive changed');
  compressed = Buffer.alloc(info.size + 1); let bytesRead = 0;
  while (bytesRead < compressed.length) { const chunk = await handle.read(compressed, bytesRead, compressed.length - bytesRead, bytesRead); if (!chunk.bytesRead) break; bytesRead += chunk.bytesRead; }
  const after = await handle.stat();
  if (bytesRead !== info.size || after.size !== info.size || after.mtimeMs !== info.mtimeMs) throw new Error('AUR recipe archive changed');
  compressed = compressed.subarray(0, bytesRead);
 } finally { await handle.close(); }
 let bytes;
 try { bytes = gunzipSync(compressed, { maxOutputLength: 65536 }); } catch { throw new Error('Invalid bounded AUR recipe archive'); }
 const seen = new Set(); let offset = 0;
 const field = (header, start, size) => header.subarray(start, start + size).toString('utf8').replace(/\0.*$/s, '');
 const octal = value => { if (!/^[0-7]+$/.test(value.trim())) throw new Error('Invalid AUR recipe tar number'); return Number.parseInt(value.trim(), 8); };
 while (offset + 512 <= bytes.length && bytes.subarray(offset, offset + 512).some(byte => byte !== 0)) {
  const header = bytes.subarray(offset, offset + 512), name = field(header, 0, 100), size = octal(field(header, 124, 12));
  const checksum = header.reduce((sum, byte, index) => sum + (index >= 148 && index < 156 ? 32 : byte), 0);
  if (checksum !== octal(field(header, 148, 8)) || !Object.hasOwn(expected, name) || seen.has(name) || ![0, 48].includes(header[156])
   || field(header, 157, 100) !== '' || field(header, 345, 155) !== '' || size < 1 || size > 16384 || offset + 512 + size > bytes.length) throw new Error('Invalid AUR recipe member');
  const content = bytes.subarray(offset + 512, offset + 512 + size);
  if (!content.equals(Buffer.from(expected[name]))) throw new Error('AUR recipe does not pin the accepted version and DEB SHA256');
  seen.add(name); offset += 512 + Math.ceil(size / 512) * 512;
 }
 if (seen.size !== 2 || bytes.length < offset + 1024 || bytes.subarray(offset).some(byte => byte !== 0)) throw new Error('Invalid AUR recipe inventory');
}

// Takes a previously built DEB; never downloads or claims it was accepted.
export async function writeAurRecipe(version, deb, directory) {
 linuxPackageVersion(version, 'x64');
 if (basename(deb) !== `dilla-${version}-linux-x64.deb`) throw new Error('DEB filename does not match version');
 const hash = createHash('sha256').update(await readFile(deb)).digest('hex');
 const files = aurRecipe(version, hash);
 await mkdir(directory, { recursive: true });
 for (const [name, value] of Object.entries(files)) await writeFile(join(directory, name), value, { flag: 'wx' });
 const archive = join(directory, `dilla-${version}-linux-x64.aur.tar.gz`);
 execFileSync('tar', ['--sort=name', '--mtime=@0', '--owner=0', '--group=0', '--numeric-owner', '-czf', archive, '-C', directory, '.SRCINFO', 'PKGBUILD']);
 return archive;
}
if (isMain(import.meta.url)) {
 if (process.argv[2] === 'recipe' && process.argv.length === 6) {
  await writeAurRecipe(process.argv[3], resolve(process.argv[4]), resolve(process.argv[5]));
 } else if (process.argv[2] === 'build' && (process.argv.length === 4 || process.argv.length === 5)) {
  if (process.platform !== 'linux' || process.arch !== 'x64') throw new Error('Native packages require Linux x64');
  const root = resolve(import.meta.dirname, '..'), output = resolve(process.argv[3]), version = process.argv[4];
  if (version !== undefined) linuxPackageVersion(version, 'x64');
  const env = { ...process.env }; delete env.DILLA_NATIVE_BUILD_VERSION;
  if (version !== undefined) env.DILLA_NATIVE_BUILD_VERSION = version;
  execFileSync('npm', ['run', 'build', '-w', '@dilla/desktop', '--', '--linux-install-kind=system-package', ...(version === undefined ? [] : ['--existing-client'])], { cwd: root, stdio: 'inherit', env });
  execFileSync(resolve(root, '../../node_modules/.bin/electron-builder'), ['--config', 'electron-builder.native-linux.cjs', '--linux', 'deb', 'rpm', '--x64', '--publish', 'never', `-c.directories.output=${output}`], { cwd: root, stdio: 'inherit', env });
 } else throw new Error('Usage: build-native-linux-packages.mjs build OUTPUT_DIRECTORY [VERSION] | recipe VERSION DEB RECIPE_DIRECTORY');
}
