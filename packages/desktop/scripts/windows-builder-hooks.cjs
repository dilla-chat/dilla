const { writeFile } = require('node:fs/promises');
const { join, dirname, resolve } = require('node:path');
/** Pinned electron-builder 26.18.0 emits afterSign after PE/ASAR/fuse edits,
 * and artifactBuildStarted before AppPackageHelper creates the NSIS archive. */
exports.createWindowsBuilderHooks = ({ metadataPublicKeys, loadPackage = () => import('./windows-package.mjs') }) => {
  const finalized = new Map();
  return {
    afterSign: async context => {
      if (context.electronPlatformName !== 'win32') return;
      if (context.arch !== 1) throw new Error('Windows bootstrap supports native x64 only');
      const { finalizeWindowsProgram } = await loadPackage();
      const { appInfo } = context.packager;
      const schemes = (context.packager.config.protocols ?? []).flatMap(protocol => protocol.schemes);
      if (schemes.length !== 1 || typeof schemes[0] !== 'string' || !/^dilla(?:-[a-z0-9][a-z0-9-]{0,63})?$/.test(schemes[0])
        || (appInfo.id === 'dev.dilla.desktop' && schemes[0] !== 'dilla')) throw new Error('Invalid compiled Windows protocol identity');
      const state = await finalizeWindowsProgram({ appOutDir: context.appOutDir, outDir: context.outDir,
        version: appInfo.version, arch: 'x64', productName: appInfo.productName, appId: appInfo.id,
        publicKeys: metadataPublicKeys, iconFile: await context.packager.getIconPath(),
        signLauncher: file => context.packager.signIf(file) });
      finalized.set(context.arch, { state, appInfo, scheme: schemes[0], outDir: resolve(context.outDir) });
    },
    artifactBuildStarted: async event => {
      if (event.targetPresentableName !== 'nsis') return;
      const record = finalized.get(event.arch ?? 1);
      if (!record) throw new Error('Windows program must be finalized after signing before NSIS archive');
      if (resolve(dirname(event.file)) !== record.outDir) throw new Error('Windows NSIS output directory changed');
      const { assertWindowsProgramUnchanged } = await loadPackage();
      await assertWindowsProgramUnchanged(record.state);
      const digest = Buffer.from(record.state.digest, 'base64').toString('hex');
      if (!/^[a-f0-9]{128}$/.test(digest)) throw new Error('Invalid finalized program digest');
      // The custom script includes the companion of APP_64, supplied by the
      // pinned builder. Neither a runtime argument nor an install path chooses it.
      const archive = `${record.appInfo.sanitizedName}-${record.appInfo.version}-x64.nsis.7z`;
      const { nsisEscapeString } = require('app-builder-lib/out/targets/nsis/nsisScriptGenerator');
      const bootstrapExe = nsisEscapeString(join(record.state.appOutDir, 'Dilla.exe'));
      await writeFile(join(record.outDir, archive + '.bootstrap.nsh'),
        `!ifndef DILLA_PROTOCOL_SCHEME\n!define DILLA_PROTOCOL_SCHEME "${record.scheme}"\n!endif\n!define DILLA_BOOTSTRAP_EXE "${bootstrapExe}"\n!define DILLA_MANIFEST_SHA512 "${record.state.digest}"\n!define DILLA_PROGRAM_ID "v-${digest.slice(0, 32)}"\n!define DILLA_MANIFEST_NAME "${digest}.json"\n`);
    },
  };
};
