// CI observation only: no installer, privilege boundary or product API.
import { createHash } from 'node:crypto';
import { createReadStream, readFileSync } from 'node:fs';
import { lstat, mkdir, readFile, readdir, readlink, writeFile } from 'node:fs/promises';
import { execFileSync } from 'node:child_process';
import { basename, join, resolve } from 'node:path';
import { pathToFileURL } from 'node:url';
import { extractFile } from '@electron/asar';
import { buildArtifactNames, buildProvenanceName, readBuildProvenanceFile, verifyBuildProvenance } from '../../../packages/desktop/scripts/build-provenance.mjs';
import { verifyAurRecipe } from '../../../packages/desktop/scripts/build-native-linux-packages.mjs';
import { linuxPackageVersion } from '../../../packages/desktop/scripts/linux-package-version.mjs';
import { verifyShippingCoreWasmInDist } from './shipping-wasm.mjs';

const run = (command, args) => execFileSync(command, args, { encoding: 'utf8', maxBuffer: 8 * 1024 * 1024 }).trim();
const sha = bytes => createHash('sha256').update(bytes).digest('hex');
async function fileRecord(file, algorithm = 'sha256', encoding = 'hex') {
 const before = await lstat(file);
 if (!before.isFile() || before.nlink !== 1 || before.size < 1 || before.size > 4 * 1024 ** 3) throw new Error('Invalid regular input file: ' + basename(file));
 const hash = createHash(algorithm); let size = 0;
 for await (const bytes of createReadStream(file)) { size += bytes.length; hash.update(bytes); }
 const after = await lstat(file);
 if (size !== before.size || after.ino !== before.ino || after.dev !== before.dev || after.size !== before.size || after.mtimeMs !== before.mtimeMs) throw new Error('Input changed during observation');
 return { size, hash: hash.digest(encoding) };
}
export async function verifyNativePackageInputs(directory, expected) {
 const target = 'linux-packages-x64', names = buildArtifactNames(target, expected.version), proofName = buildProvenanceName(target);
 if (JSON.stringify((await readdir(directory)).sort()) !== JSON.stringify([...names, proofName].sort())) throw new Error('Unexpected native package input inventory');
 const record = JSON.parse(await readBuildProvenanceFile(join(directory, proofName))), observed = [];
 for (const name of names) { const { size, hash } = await fileRecord(join(directory, name), 'sha512', 'base64'); observed.push({ name, size, sha512: hash }); }
 verifyBuildProvenance(record, target, expected, observed);
 await verifyAurRecipe(expected.version, join(directory, names[0]), join(directory, names[2]));
 return record;
}
export function comparePayloads(expected, observed) {
 const fields = ['executable', 'asar', 'sandboxHelper', 'audioHelper', 'clientManifest', 'coreWasm'];
 if (fields.some(field => typeof expected[field] !== 'string' || expected[field] !== observed[field])) throw new Error('Native format payload differs from accepted DEB');
}
function context() {
 if (process.env.GITHUB_ACTIONS !== 'true' || process.env.GITHUB_REPOSITORY !== 'jonasthim/dilla' || process.platform !== 'linux' || process.arch !== 'x64' || process.versions.node.split('.')[0] !== '24') throw new Error('Dedicated GitHub Linux x64 / Node 24 acceptance required');
 const commit = run('git', ['rev-parse', 'HEAD']), tree = run('git', ['rev-parse', 'HEAD^{tree}']);
 if (commit !== process.env.GITHUB_SHA || !/^[1-9]\d*$/.test(process.env.GITHUB_RUN_ID ?? '')) throw new Error('Wrong acceptance source/run');
 if (run('git', ['status', '--porcelain', '--untracked-files=all']) !== '') throw new Error('Acceptance requires a clean source checkout');
 return { commit, tree, ciRun: Number(process.env.GITHUB_RUN_ID), version: JSON.parse(readFileSync('packages/desktop/package.json', 'utf8')).version };
}
// bsdtar retains its default traversal/symlink protections. Extraction runs as
// the ordinary CI user, into a fresh private directory, never into the OS root.
async function extract(archive, directory) {
 const names = run('bsdtar', ['-tf', archive]).split('\n');
 if (names.some(name => name.startsWith('/') || name.split('/').includes('..'))) throw new Error('Unsafe package member path');
 await mkdir(directory, { mode: 0o700 });
 run('bsdtar', ['--no-same-owner', '--no-same-permissions', '-xf', archive, '-C', directory]);
}
export async function readNativePayload(root, version, dist = resolve('packages/web/dist')) {
 const app = join(root, 'opt/Dilla'), asar = join(app, 'resources/app.asar');
 if (JSON.parse(extractFile(asar, 'package.json')).version !== version) throw new Error('Installed ASAR version differs from source');
 const core = verifyShippingCoreWasmInDist(dist), manifest = await readFile(join(dist, 'dilla-manifest.json'));
 const embeddedManifest = extractFile(asar, 'dist/client/dilla-manifest.json'), embeddedCore = extractFile(asar, 'dist/client/assets/' + basename(core));
 if (!manifest.equals(embeddedManifest) || !(await readFile(core)).equals(embeddedCore)) throw new Error('Package does not contain shipping client manifest/core');
 const result = { clientManifest: sha(embeddedManifest), coreWasm: sha(embeddedCore) };
 for (const [key, file] of Object.entries({ executable: 'dilla-desktop', asar: 'resources/app.asar', sandboxHelper: 'chrome-sandbox', audioHelper: 'resources/native/dilla-audio' })) result[key] = (await fileRecord(join(app, file))).hash;
 return result;
}
async function json(file, value) { await writeFile(file, JSON.stringify(value, null, 2) + '\n', { flag: 'wx', mode: 0o644 }); }
async function prepare(inputs, output, expected) {
 const producer = await verifyNativePackageInputs(inputs, expected), prefix = `dilla-${expected.version}-linux-x64`;
 const outer = join(output, 'deb-container'); await mkdir(outer, { mode: 0o700 });
 const deb = join(inputs, prefix + '.deb'), members = run('bsdtar', ['-tf', deb]).split('\n').filter(name => /^data\.tar(?:\.(?:gz|xz|zst|bz2))?$/.test(name));
 if (members.length !== 1) throw new Error('DEB must have one standard data archive');
 run('bsdtar', ['-xf', deb, '-C', outer, members[0]]);
 await extract(join(outer, members[0]), join(output, 'deb'));
 await extract(join(inputs, prefix + '.rpm'), join(output, 'rpm'));
 const debPayload = await readNativePayload(join(output, 'deb'), expected.version), rpmPayload = await readNativePayload(join(output, 'rpm'), expected.version);
 comparePayloads(debPayload, rpmPayload);
 await verifyNativePackageInputs(inputs, expected);
 await json(join(output, 'inputs.json'), { producer, payload: debPayload });
}
export async function readInstalledDesktopEntry(root = '/') {
 // The stock DEB/RPM targets honor desktopName with syncDesktopName enabled;
 // the AUR recipe retains that exact DEB entry.
 const desktop = await readFile(join(root, 'usr/share/applications/dev.dilla.desktop.desktop'), 'utf8');
 if (!/^Exec=.*dilla-desktop/m.test(desktop) || !/^MimeType=.*x-scheme-handler\/dilla[;\s]/m.test(desktop)) throw new Error('Missing desktop/protocol registration');
 return desktop;
}
async function installed(inputs, output, format, expected) {
 const producer = await verifyNativePackageInputs(inputs, expected), prepared = JSON.parse(await readFile(join(output, 'inputs.json'), 'utf8'));
 if (JSON.stringify(producer) !== JSON.stringify(prepared.producer)) throw new Error('Producer identity changed');
 comparePayloads(prepared.payload, await readNativePayload('/', expected.version));
 const v = linuxPackageVersion(expected.version, 'x64'); let manager, builtPackage = null;
 if (format === 'deb') { manager = run('dpkg-query', ['-W', '-f=${Package} ${Version} ${Architecture}', 'dilla']); if (manager !== `dilla ${v.deb.version} ${v.deb.arch}`) throw new Error('Wrong DEB installation'); }
 else if (format === 'rpm') { manager = run('rpm', ['-q', '--qf', '%{NAME} %{VERSION} %{RELEASE} %{ARCH}', 'dilla']); if (manager !== `dilla ${v.rpm.version} ${v.rpm.release} ${v.rpm.arch}`) throw new Error('Wrong RPM installation'); }
 else if (format === 'aur') {
  manager = run('pacman', ['-Q', 'dilla-bin']); if (manager !== `dilla-bin ${v.aur.pkgver}-${v.aur.pkgrel}`) throw new Error('Wrong AUR installation');
  const archive = `/home/dilla-ci/recipe/dilla-bin-${v.aur.pkgver}-${v.aur.pkgrel}-x86_64.pkg.tar.zst`;
  if (process.env.DILLA_NATIVE_BUILT_PACKAGE !== archive) throw new Error('Missing actual makepkg output identity');
  const before = await fileRecord(archive);
  await extract(archive, join(output, 'aur-built'));
  comparePayloads(prepared.payload, await readNativePayload(join(output, 'aur-built'), expected.version));
  if (JSON.stringify(before) !== JSON.stringify(await fileRecord(archive))) throw new Error('makepkg output changed during observation');
  builtPackage = { name: basename(archive), size: before.size, sha256: before.hash };
 }
 else throw new Error('Invalid package format');
 const entry = await readlink('/usr/bin/dilla-desktop');
 if (!['/opt/Dilla/dilla-desktop', '/etc/alternatives/dilla-desktop'].includes(entry) || run('readlink', ['-f', '/usr/bin/dilla-desktop']) !== '/opt/Dilla/dilla-desktop') throw new Error('Wrong installed launcher');
 const helper = await lstat('/opt/Dilla/chrome-sandbox'), mode = helper.mode & 0o7777;
 if (!helper.isFile() || helper.uid !== 0 || helper.gid !== 0 || ![0o755, 0o4755].includes(mode) || (format === 'aur' && mode !== 0o4755)) throw new Error('Unexpected standard sandbox helper ownership/mode');
 const desktop = await readInstalledDesktopEntry();
 const userspace = await readFile('/etc/os-release', 'utf8'), kernel = run('uname', ['-sr']);
 const signaturePolicySnapshotSha256 = sha(await readFile(join(output, 'signature-policy-before.txt')));
 await json(join(output, 'installed.json'), { format, manager, builtPackage, userspace, kernel, signaturePolicySnapshotSha256, helper: { uid: helper.uid, gid: helper.gid, mode: mode.toString(8) }, desktopSha256: sha(desktop), payload: prepared.payload });
}
async function finish(inputs, output, format, expected) {
 const producer = await verifyNativePackageInputs(inputs, expected), prepared = JSON.parse(await readFile(join(output, 'inputs.json'), 'utf8'));
 const installation = JSON.parse(await readFile(join(output, 'installed.json'), 'utf8')), runtime = JSON.parse(await readFile(join(output, 'runtime.json'), 'utf8'));
 if (JSON.stringify(producer) !== JSON.stringify(prepared.producer) || installation.format !== format || runtime.httpsOnboarding !== true || !runtime.sandbox?.renderers?.length) throw new Error('Incomplete installed native package observations');
 comparePayloads(prepared.payload, installation.payload);
 await json(join(output, `acceptance-${format}.json`), { kind: 'dilla-native-format-observation', source: expected, producer, installation, runtime, boundary: format === 'deb' ? 'Ubuntu 24.04 hosted runtime' : 'distribution userspace container; Ubuntu hosted kernel; no native SELinux/kernel qualification' });
}
if (process.argv[1] && pathToFileURL(resolve(process.argv[1])).href === import.meta.url) {
 const [command, inputs, output, format] = process.argv.slice(2), expected = context();
 if (!inputs || !output || !['deb', 'rpm', 'aur'].includes(format)) throw new Error('Usage: native-linux-package-acceptance.mjs prepare|installed|finish INPUTS OUTPUT FORMAT');
 if (command === 'prepare') await prepare(resolve(inputs), resolve(output), expected);
 else if (command === 'installed') await installed(resolve(inputs), resolve(output), format, expected);
 else if (command === 'finish') await finish(resolve(inputs), resolve(output), format, expected);
 else throw new Error('Unknown native package observation command');
}
