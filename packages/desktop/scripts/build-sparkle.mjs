import { createHash } from 'node:crypto';
import { execFileSync } from 'node:child_process';
import { cp, mkdir, readFile, writeFile, access } from 'node:fs/promises';
import { dirname, resolve } from 'node:path';
if (process.platform !== 'darwin') throw new Error('Sparkle must be built on macOS');
const root = resolve(import.meta.dirname, '..');
const version = '2.10.1';
const digest = '1a856d2a9edb8e628de88121d7be04cf0e2167f8641650b78fca00b14244cf27';
const cache = resolve(root, '.native-tools', `Sparkle-${version}`);
await mkdir(cache, { recursive: true });
const archive = resolve(cache, 'Sparkle.tar.xz');
let bytes;
try { bytes = await readFile(archive); } catch {
  const response = await fetch(`https://github.com/sparkle-project/Sparkle/releases/download/${version}/Sparkle-${version}.tar.xz`, { signal: AbortSignal.timeout(120000) });
  if (!response.ok) throw new Error('Unable to download pinned Sparkle');
  bytes = Buffer.from(await response.arrayBuffer());
}
if (createHash('sha256').update(bytes).digest('hex') !== digest) throw new Error('Sparkle archive digest mismatch');
await writeFile(archive, bytes);
execFileSync('tar', ['-xf', archive, '-C', cache], { stdio: 'inherit' });
await cp(resolve(cache, 'Sparkle.framework'), resolve(root, 'native-bin/Sparkle.framework'), { recursive: true, verbatimSymlinks: true });
const headers = resolve(dirname(dirname(process.execPath)), 'include/node');
await access(resolve(headers, 'node_api.h'));
execFileSync('clang++', ['-std=c++17', '-fobjc-arc', '-fblocks', '-O2', '-bundle', '-undefined', 'dynamic_lookup', '-DNODE_GYP_MODULE_NAME=dilla_sparkle', '-I', headers, '-F', resolve(cache), '-framework', 'Cocoa', '-framework', 'Sparkle', '-Wl,-rpath,@loader_path/../../Frameworks', '-mmacosx-version-min=13.0', resolve(root, 'native/SparkleBridge.mm'), '-o', resolve(root, 'native-bin/dilla-sparkle.node')], { stdio: 'inherit' });
