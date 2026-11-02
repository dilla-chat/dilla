const { copyFile, lstat, mkdtemp, readFile, rm, symlink, writeFile } = require('node:fs/promises');
const { constants } = require('node:fs');
const { createHash } = require('node:crypto');
const { execFileSync } = require('node:child_process');
const { tmpdir } = require('node:os');
const { dirname, join, resolve } = require('node:path');
const repo = resolve(__dirname, '../../..');
const components = join(repo, 'packages/desktop/linux-sandbox-component.json');
const pending = new Map();
const key = (out, arch) => `${resolve(out)}:${arch}`;
const APP_RUN_SCRIPT = `#!/bin/sh
set -eu
if [ -z "\${APPDIR:-}" ]; then
  APPDIR="$(CDPATH= cd -- "$(dirname -- "$(readlink -f -- "$0")")" && pwd -P)"
fi
export APPDIR
export PATH="$APPDIR:$APPDIR/usr/sbin\${PATH:+:$PATH}"
export XDG_DATA_DIRS="$APPDIR/usr/share\${XDG_DATA_DIRS:+:$XDG_DATA_DIRS}:/usr/share/gnome:/usr/local/share:/usr/share"
export LD_LIBRARY_PATH="$APPDIR/usr/lib\${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
export GSETTINGS_SCHEMA_DIR="$APPDIR/usr/share/glib-2.0/schemas\${GSETTINGS_SCHEMA_DIR:+:$GSETTINGS_SCHEMA_DIR}"
exec "$APPDIR/dilla-desktop" "$@"
`;
const PRODUCTION_APP_RUN_SCRIPT = APP_RUN_SCRIPT.replace('exec "$APPDIR/dilla-desktop"', 'unset CHROME_DEVEL_SANDBOX\nexec "$APPDIR/dilla-sandbox-launcher"');

// Foundation is a separate content-free renderer fixture, not a shipping build.
async function foundation(context) {
 if (context.electronPlatformName !== 'linux') return;
 if (context.packager.executableName !== 'dilla-desktop') throw new Error('Unexpected Linux packaged executable');
 await writeFile(join(context.appOutDir, 'AppRun'), APP_RUN_SCRIPT, { mode: 0o755 });
}

// app-builder-lib invokes afterPack before fuses are changed. Capture context;
// finalize only when AppImageTarget signals its artifact after the fuse step.
async function afterPack(context) {
 if (context.electronPlatformName !== 'linux') return;
 if (context.packager.executableName !== 'dilla-desktop') throw new Error('Unexpected Linux packaged executable');
 if (context.arch !== 1) throw new Error('AppImage sandbox requires Linux x64');
 const id = key(context.outDir, context.arch);
 if (pending.has(id)) throw new Error('Duplicate AppImage packaging context');
 pending.set(id, { appOutDir: resolve(context.appOutDir), outDir: resolve(context.outDir) });
}
async function artifactBuildStarted(event) {
 if (event.targetPresentableName !== 'AppImage') return;
 const id = key(dirname(event.file), event.arch), record = pending.get(id);
 if (!record || event.arch !== 1) throw new Error('Missing Linux x64 AppImage packaging context');
 pending.delete(id);
 if (!event.file.endsWith('.AppImage')) throw new Error('Unexpected AppImage artifact name');
 const dir = record.appOutDir, elf = join(dir, 'dilla-desktop'), asar = join(dir, 'resources/app.asar'), helper = join(dir, 'chrome-sandbox');
 for (const path of [elf, asar, helper]) { const st = await lstat(path); if (!st.isFile() || st.isSymbolicLink()) throw new Error('Missing regular AppImage component'); }
 const hash = b => createHash('sha512').update(b).digest('base64');
 const elfHash = hash(await readFile(elf)), asarHash = hash(await readFile(asar));
 const temp = await mkdtemp(join(tmpdir(), 'dilla-appimage-'));
 try {
  const build = join(temp, 'components');
  execFileSync(process.execPath, [join(__dirname, 'linux-sandbox-package.mjs'), components, helper, build], { cwd: repo, stdio: 'inherit' });
  const config = JSON.parse(await readFile(components, 'utf8'));
  const env = { ...process.env, GOOS: 'linux', GOARCH: 'amd64', GOAMD64: 'v1', GOENV: 'off', GOWORK: 'off', GOFLAGS: '', GOEXPERIMENT: '', GOTOOLCHAIN: config.goVersion, CGO_ENABLED: '1' };
  execFileSync('go', ['build', '-mod=readonly', '-trimpath', '-buildvcs=false', '-overlay', join(build, 'launcher-overlay.json'), '-o', join(dir, 'dilla-sandbox-launcher'), './packages/desktop/native/linux-sandbox/launcher'], { cwd: repo, env, stdio: 'inherit' });
  const suffix = event.file.slice(0, -'.AppImage'.length);
  await copyFile(join(build, 'component.sandbox'), suffix + '.sandbox', constants.COPYFILE_EXCL);
  await copyFile(join(build, 'component.sandbox.json'), suffix + '.sandbox.json', constants.COPYFILE_EXCL);
  const values = JSON.parse(await readFile(join(build, 'constants.json'), 'utf8'));
  await writeFile(join(dir, 'resources/linux-sandbox-build.json'), JSON.stringify({ format: 1, electronSha512: elfHash, asarSha512: asarHash, capsuleSha512: values.capsuleSha512, generation: values.generation }) + '\n', { flag: 'wx' });
  await rm(helper); await symlink('/usr/lib/dilla/appimage/chrome-sandbox', helper);
  await writeFile(join(dir, 'AppRun'), PRODUCTION_APP_RUN_SCRIPT, { mode: 0o755 });
  if (hash(await readFile(elf)) !== elfHash || hash(await readFile(asar)) !== asarHash) throw new Error('AppImage ELF or ASAR changed during packaging');
 } finally { await rm(temp, { recursive: true, force: true }); }
}
module.exports = afterPack;
module.exports.artifactBuildStarted = artifactBuildStarted;
module.exports.foundation = foundation;
module.exports.APP_RUN_SCRIPT = APP_RUN_SCRIPT;
module.exports.PRODUCTION_APP_RUN_SCRIPT = PRODUCTION_APP_RUN_SCRIPT;
