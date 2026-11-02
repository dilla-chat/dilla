import { getCurrentFuseWire, FuseV1Options, FuseState } from '@electron/fuses';
import { readdir } from 'node:fs/promises';
import { resolve, join } from 'node:path';
import { pathToFileURL } from 'node:url';
export async function desktopFuseTargets(out) {
 const targets = [];
 for (const entry of await readdir(out, { withFileTypes: true })) {
  if (!entry.isDirectory()) continue;
  const path = join(out, entry.name);
  if (entry.name === 'linux-unpacked') targets.push(join(path, 'dilla-desktop'));
  if (entry.name === 'win-unpacked') targets.push(join(path, 'dilla-desktop.exe'));
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
