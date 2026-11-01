import { getCurrentFuseWire, FuseV1Options, FuseState } from '@electron/fuses';
import { readdir } from 'node:fs/promises';
import { resolve, join } from 'node:path';

const out = resolve(import.meta.dirname, '../out');
const targets = [];
for (const entry of await readdir(out, { withFileTypes: true })) {
  if (!entry.isDirectory()) continue;
  const path = join(out, entry.name);
  if (entry.name === 'linux-unpacked') targets.push(join(path, 'dilla-desktop'));
  if (entry.name === 'win-unpacked') {
    const executables = (await readdir(path)).filter(name => name.endsWith('.exe'));
    if (executables.length !== 1) throw new Error('Expected one Windows app executable');
    targets.push(join(path, executables[0]));
  }
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
for (const path of targets) {
  const fuses = await getCurrentFuseWire(path);
  for (const key of [FuseV1Options.RunAsNode, FuseV1Options.EnableNodeOptionsEnvironmentVariable, FuseV1Options.EnableNodeCliInspectArguments, FuseV1Options.GrantFileProtocolExtraPrivileges]) {
    if (fuses[key] !== FuseState.DISABLE) throw new Error(`Unsafe fuse ${key}: ${path}`);
  }
  for (const key of [FuseV1Options.EnableCookieEncryption, FuseV1Options.OnlyLoadAppFromAsar, FuseV1Options.EnableEmbeddedAsarIntegrityValidation]) {
    if (fuses[key] !== FuseState.ENABLE) throw new Error(`Required fuse ${key} is disabled: ${path}`);
  }
  console.log(`Desktop fuses verified: ${path}`);
}
