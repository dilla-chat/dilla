// Stock Linux targets intentionally have no AppImage launcher hooks.
const { readdir, lstat, chmod } = require('node:fs/promises');
const { join } = require('node:path');
async function normalizeModes(path) {
 const info = await lstat(path);
 if (info.isSymbolicLink()) return;
 if (info.isDirectory()) for (const name of await readdir(path)) await normalizeModes(join(path, name));
 if (info.isFile() || info.isDirectory()) await chmod(path, info.mode & 0o755);
}
const override = process.env.DILLA_NATIVE_BUILD_VERSION;
const version = override === undefined ? require('./package.json').version : override;
const match = /^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-beta\.(0|[1-9]\d*))?$/.exec(version);
if (!match || !match.slice(1).filter(value => value !== undefined).every(value => Number.isSafeInteger(Number(value)))) throw new Error('Unsupported native package version');
const base = `${match[1]}.${match[2]}.${match[3]}`, beta = match[4];
module.exports = {
 appId: 'dev.dilla.desktop', productName: 'Dilla', executableName: 'dilla-desktop',
 directories: { output: 'out-native', buildResources: 'assets' },
 files: ['dist/**', 'package.json'], asar: true,
 beforePack: context => { if (process.platform !== 'linux' || process.arch !== 'x64' || context.electronPlatformName !== 'linux' || context.arch !== 1) throw new Error('Native packages require Linux x64'); },
 afterPack: context => normalizeModes(context.appOutDir),
 extraMetadata: { homepage: 'https://github.com/dilla-chat/dilla', ...(override === undefined ? {} : { version }) },
 extraResources: [{ from: 'native-bin', to: 'native', filter: ['dilla-audio*'] }],
 protocols: [{ name: 'Dilla invite', schemes: ['dilla'] }],
 linux: { artifactName: 'dilla-${version}-linux-x64.${ext}', syncDesktopName: true, target: [{ target: 'deb', arch: ['x64'] }, { target: 'rpm', arch: ['x64'] }], category: 'Network', icon: 'assets/icon.png', maintainer: 'Dilla contributors', vendor: 'Dilla contributors' },
 deb: { packageName: 'dilla', fpm: ['--depends', 'libpipewire-0.3-0t64 | libpipewire-0.3-0', '--version', `${base}${beta === undefined ? '' : `~beta.${beta}`}`, '--iteration', '1'] },
 rpm: { packageName: 'dilla', afterRemove: 'packaging/linux/rpm-after-remove.tpl', fpm: ['--depends', 'libpipewire-0.3.so.0()(64bit)', '--version', base, '--iteration', beta === undefined ? '1' : `0.beta.${beta}.1`] },
 electronFuses: { runAsNode: false, enableCookieEncryption: true, enableNodeOptionsEnvironmentVariable: false, enableNodeCliInspectArguments: false, enableEmbeddedAsarIntegrityValidation: true, onlyLoadAppFromAsar: true, grantFileProtocolExtraPrivileges: false },
};
