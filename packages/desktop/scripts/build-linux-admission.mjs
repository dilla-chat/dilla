import { execFileSync } from 'node:child_process';
import { access, mkdir, readFile, realpath } from 'node:fs/promises';
import { dirname, isAbsolute, resolve } from 'node:path';
import { pathToFileURL } from 'node:url';
const desktop = resolve(import.meta.dirname, '..');
/** Compile-time fixture constants never enter production runtime selection. */
export async function buildLinuxAdmission({ output, fixture } = {}) {
 if (process.platform !== 'linux' || process.versions.node.split('.')[0] !== '24') throw new Error('Linux admission requires the Node 24 Linux build toolchain');
 const config = JSON.parse(await readFile(resolve(desktop, 'package.json'), 'utf8'));
 if (config.devDependencies.electron !== '45.2.1') throw new Error('Requalify Linux admission for the changed pinned Electron version');
 if (typeof output !== 'string' || !isAbsolute(output)) throw new Error('Admission output must be absolute');
 const headers = resolve(dirname(dirname(process.execPath)), 'include/node');
 await access(resolve(headers, 'node_api.h'));
 const flags = ['-std=c++17', '-O2', '-Wall', '-Wextra', '-Werror', '-shared', '-fPIC', '-pthread', '-DNAPI_VERSION=8', '-DNODE_GYP_MODULE_NAME=dilla_admission', '-I', headers];
 if (fixture !== undefined) {
  if (!fixture || typeof fixture.directory !== 'string' || !/^\/[A-Za-z0-9_./-]+$/.test(fixture.directory) || resolve(fixture.directory) !== fixture.directory
   || !Number.isInteger(fixture.uid) || fixture.uid < 0 || fixture.uid > 0xffffffff || !Number.isInteger(fixture.gid) || fixture.gid < 0 || fixture.gid > 0xffffffff
   || await realpath(dirname(fixture.directory)) !== dirname(fixture.directory)) throw new Error('Invalid compile-only admission fixture');
  flags.push('-DDILLA_ADMISSION_FIXTURE=1', '-DDILLA_ADMISSION_FIXTURE_DIRECTORY=' + JSON.stringify(fixture.directory), '-DDILLA_ADMISSION_FIXTURE_BASE=' + JSON.stringify(dirname(fixture.directory)), '-DDILLA_ADMISSION_FIXTURE_UID=' + fixture.uid, '-DDILLA_ADMISSION_FIXTURE_GID=' + fixture.gid);
 }
 await mkdir(dirname(output), { recursive: true });
 execFileSync('c++', [...flags, resolve(desktop, 'native/linux-admission.cpp'), '-o', output], { stdio: 'pipe' });
 return output;
}
if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
 if (process.argv.length !== 3) throw new Error('Usage: node build-linux-admission.mjs ABSOLUTE_OUTPUT');
 await buildLinuxAdmission({ output: process.argv[2] });
}
