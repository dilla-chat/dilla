import { getCurrentFuseWire, FuseV1Options, FuseState } from '@electron/fuses';
import { readdir, readFile } from 'node:fs/promises';
import { execFileSync } from 'node:child_process';
import { resolve, join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { canonicalSHA512, strictWindowsJSON, verifyWindowsProgramManifest, verifyWindowsProgramTree, windowsProgramID } from '../src/windows-manifest.ts';

function describeWindows(root) {
 if (process.platform !== 'win32') throw new Error('Inspect the Windows bootstrap on its native build platform');
 // This locally built stable Go mode reads compiled public metadata only; it
 // cannot load mutable Electron resources or start an application/update child.
 return strictWindowsJSON(execFileSync(join(root, 'Dilla.exe'), ['--describe'], { timeout: 30_000, windowsHide: true }));
}
async function windowsFuseTarget(root, readBootstrap) {
 const bootstrap = await readBootstrap(root);
 if (!bootstrap || bootstrap.format !== 1 || bootstrap.arch !== 'x64' || !['stable', 'beta'].includes(bootstrap.channel)
   || typeof bootstrap.initialVersion !== 'string' || !canonicalSHA512(bootstrap.initialManifestSha512)) throw new Error('Invalid compiled Windows bootstrap metadata');
 const seed = strictWindowsJSON(await readFile(join(root, 'initial-slot.json')));
 if (!seed || Object.keys(seed).length !== 5 || seed.format !== 1 || seed.kind !== 'initial' || seed.generation !== 0 || seed.version !== bootstrap.initialVersion || seed.manifestSha512 !== bootstrap.initialManifestSha512) throw new Error('Windows initial slot differs from compiled bootstrap');
 const digest = bootstrap.initialManifestSha512, id = windowsProgramID(digest), manifestName = Buffer.from(digest, 'base64').toString('hex') + '.json';
 const versions = await readdir(join(root, 'versions')), manifests = await readdir(join(root, 'manifests'));
 if (versions.length !== 1 || versions[0] !== id || manifests.length !== 1 || manifests[0] !== manifestName) throw new Error('Expected one unique finalized Windows program version/manifest');
 const context = { version: bootstrap.initialVersion, channel: bootstrap.channel, arch: bootstrap.arch };
 const manifest = verifyWindowsProgramManifest(await readFile(join(root, 'manifests', manifestName)), digest, context);
 if (!manifest.files.some(file => file.path === 'dilla-desktop.exe' && file.size > 0)) throw new Error('No manifest-bound Windows Electron executable');
 const tree = join(root, 'versions', id);
 await verifyWindowsProgramTree(tree, manifest);
 return join(tree, 'dilla-desktop.exe');
}
export async function desktopFuseTargets(out, { describeWindows: readBootstrap = describeWindows } = {}) {
 const targets = [];
 for (const entry of await readdir(out, { withFileTypes: true })) {
  if (!entry.isDirectory()) continue;
  const path = join(out, entry.name);
  if (entry.name === 'linux-unpacked') targets.push(join(path, 'dilla-desktop'));
  if (entry.name === 'win-unpacked') targets.push(await windowsFuseTarget(path, readBootstrap));
  if (entry.name === 'mac' || entry.name === 'mac-arm64') {
   for (const name of (await readdir(path)).filter(name => name.endsWith('.app'))) {
    const bin = join(path, name, 'Contents/MacOS');
    const executables = (await readdir(bin, { withFileTypes: true })).filter(item => item.isFile() && !item.name.startsWith('.'));
    if (executables.length !== 1) throw new Error('Expected one macOS app executable');
    targets.push(join(bin, executables[0].name));
   }
  }
 }
 if (!targets.length) throw new Error('No packaged desktop binary to check');
 return targets;
}
export async function checkDesktopFuses(out = resolve(import.meta.dirname, '../out')) {
 for (const path of await desktopFuseTargets(out)) {
  const fuses = await getCurrentFuseWire(path);
  for (const key of [FuseV1Options.RunAsNode, FuseV1Options.EnableNodeOptionsEnvironmentVariable, FuseV1Options.EnableNodeCliInspectArguments, FuseV1Options.GrantFileProtocolExtraPrivileges]) {
   if (fuses[key] !== FuseState.DISABLE) throw new Error(`Unsafe fuse ${key}: ${path}`);
  }
  for (const key of [FuseV1Options.EnableCookieEncryption, FuseV1Options.OnlyLoadAppFromAsar, FuseV1Options.EnableEmbeddedAsarIntegrityValidation]) {
   if (fuses[key] !== FuseState.ENABLE) throw new Error(`Required fuse ${key} is disabled: ${path}`);
  }
  console.log(`Desktop fuses verified: ${path}`);
 }
}
if (process.argv[1] && pathToFileURL(resolve(process.argv[1])).href === import.meta.url) await checkDesktopFuses();
