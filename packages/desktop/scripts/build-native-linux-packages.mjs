import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { mkdir, readFile, writeFile } from 'node:fs/promises';
import { basename, resolve, join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { linuxPackageVersion } from './linux-package-version.mjs';

export function aurRecipe(version, sha256) {
 const v = linuxPackageVersion(version, 'x64');
 if (!/^[a-f0-9]{64}$/.test(sha256)) throw new Error('Invalid DEB SHA256');
 const name = `dilla-${version}-linux-x64.deb`;
 const url = `https://github.com/jonasthim/dilla/releases/download/desktop-v${version}/${name}`;
 const dependencies = ['alsa-lib', 'gtk3', 'libnotify', 'nss', 'libxss', 'libxtst', 'xdg-utils', 'at-spi2-core', 'libdrm', 'libxkbcommon', 'mesa', 'libsecret', 'libpipewire'];
 const pkgbuild = `# Generated from the exact Dilla release DEB. Review before publishing.\npkgname=dilla-bin\npkgver=${v.aur.pkgver}\npkgrel=1\npkgdesc='Dilla desktop client'\narch=('x86_64')\nurl='https://github.com/jonasthim/dilla'\nlicense=('AGPL-3.0-or-later')\ndepends=(${dependencies.map(x => `'${x}'`).join(' ')})\nprovides=('dilla')\nconflicts=('dilla')\nsource=('${name}::${url}')\nsha256sums=('${sha256}')\n\npackage() {\n  bsdtar -xf "$srcdir/${name}" -C "$srcdir" 'data.tar.*'\n  bsdtar -xf "$srcdir"/data.tar.* -C "$pkgdir"\n  install -dm755 "$pkgdir/usr/bin"\n  ln -s /opt/Dilla/dilla-desktop "$pkgdir/usr/bin/dilla-desktop"\n}\n`;
 const srcinfo = `pkgbase = dilla-bin\n\tpkgdesc = Dilla desktop client\n\tpkgver = ${v.aur.pkgver}\n\tpkgrel = 1\n\turl = https://github.com/jonasthim/dilla\n\tarch = x86_64\n\tlicense = AGPL-3.0-or-later\n${dependencies.map(x => `\tdepends = ${x}\n`).join('')}\tprovides = dilla\n\tconflicts = dilla\n\tsource = ${name}::${url}\n\tsha256sums = ${sha256}\n\npkgname = dilla-bin\n`;
 return { PKGBUILD: pkgbuild, '.SRCINFO': srcinfo };
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
if (process.argv[1] && pathToFileURL(resolve(process.argv[1])).href === import.meta.url) {
 if (process.argv[2] === 'recipe' && process.argv.length === 6) {
  await writeAurRecipe(process.argv[3], resolve(process.argv[4]), resolve(process.argv[5]));
 } else if (process.argv[2] === 'build' && process.argv.length === 4) {
  if (process.platform !== 'linux' || process.arch !== 'x64') throw new Error('Native packages require Linux x64');
  const root = resolve(import.meta.dirname, '..'), output = resolve(process.argv[3]);
  execFileSync('npm', ['run', 'build', '-w', '@dilla/desktop', '--', '--linux-install-kind=system-package'], { cwd: root, stdio: 'inherit' });
  execFileSync(resolve(root, '../../node_modules/.bin/electron-builder'), ['--config', 'electron-builder.native-linux.cjs', '--linux', 'deb', 'rpm', '--x64', '--publish', 'never', `-c.directories.output=${output}`], { cwd: root, stdio: 'inherit' });
 } else throw new Error('Usage: build-native-linux-packages.mjs build OUTPUT_DIRECTORY | recipe VERSION DEB RECIPE_DIRECTORY');
}
