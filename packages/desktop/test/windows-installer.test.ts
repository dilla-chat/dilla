import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFile, writeFile, mkdir, symlink, mkdtemp, rm, realpath } from 'node:fs/promises';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { createRequire } from 'node:module';
import { spawnSync } from 'node:child_process';
// @ts-expect-error test tooling is JavaScript
import { compileWindowsNSIS, withInitialInstallDiagnostics } from './support/windows-nsis.mjs';
const require = createRequire(import.meta.url);
const sourcePath = new URL('../build/windows-installer.nsi', import.meta.url);
function body(source: string, name: string): string { const match = new RegExp(`Function ${name}\\r?\\n([\\s\\S]*?)FunctionEnd`).exec(source); assert.ok(match, `missing ${name}`); return match[1]; }
async function compileCheckPathProbe(directory: string, target: string, name: string): Promise<string> {
 const original = await readFile(sourcePath, 'utf8'), check = body(original.replaceAll('${Prefix}', ''), 'CheckPath');
 const { nsisEscapeString } = require('app-builder-lib/out/targets/nsis/nsisScriptGenerator');
 return compileWindowsNSIS({ directory, archive: join(directory, 'unused.7z'), name, transform: () => `
Name "Private Dilla path boundary"
RequestExecutionLevel user
SilentInstall silent
!include "LogicLib.nsh"
!include "FileFunc.nsh"
Function Fail
 SetErrorLevel 2
 Quit
FunctionEnd
Function CheckPath
${check}
FunctionEnd
Section
 Push "${nsisEscapeString(target)}"
 Call CheckPath
 SetErrorLevel 0
SectionEnd
` });
}
async function compileDiagnosticBranchProbe(directory: string, equal: boolean): Promise<string> {
 const original = await readFile(sourcePath, 'utf8'), source = withInitialInstallDiagnostics(original).source;
 const guard = /StrCmp \$0 \$INSTDIR \+2\r?\n\s+Call FixtureRefusal\d+/.exec(body(source.replaceAll('${Prefix}', ''), 'CheckOwnedRoot'));
 assert.ok(guard, 'actual relative owned-root guard retained');
 const wrapper = /Call (FixtureRefusal\d+)/.exec(guard[0])![1];
 return compileWindowsNSIS({ directory, archive: join(directory, 'unused.7z'), name: 'diagnostic-branch-' + equal + '.exe', transform: () => `
Name "Private Dilla diagnostic branch"
RequestExecutionLevel user
SilentInstall silent
Var FixtureFailureSite
Function Fail
 SetErrorLevel 2
 Quit
FunctionEnd
Function ${wrapper}
${body(source, wrapper)}
FunctionEnd
Section
 StrCpy $INSTDIR "private-fixed-value"
 StrCpy $0 "${equal ? 'private-fixed-value' : 'private-mismatch'}"
 ${guard[0]}
 SetErrorLevel 0
SectionEnd
` });
}
test('the actual extracted NSIS CheckPath compiles with the pinned System plugin', async () => {
 const root = await realpath(await mkdtemp(join(tmpdir(), 'dilla-check-path-compile-')));
 try {
  const executable = await compileCheckPathProbe(root, join(root, 'missing'), 'check-path.exe');
  assert.equal((await readFile(executable)).subarray(0, 2).toString(), 'MZ');
  assert.equal((await readFile(await compileDiagnosticBranchProbe(root, true))).subarray(0, 2).toString(), 'MZ');
 } finally { await rm(root, { recursive: true, force: true }); }
});
test('native diagnostic refusal wrappers preserve the actual owned-root +2 branch for success and refusal', { skip: process.platform !== 'win32', timeout: 30000 }, async () => {
 const root = await realpath(await mkdtemp(join(tmpdir(), 'dilla-diagnostic-branch-')));
 try {
  for (const equal of [true, false]) {
   const executable = await compileDiagnosticBranchProbe(root, equal), result = spawnSync(executable, ['/S'], { timeout: 10000, windowsHide: true, stdio: 'pipe', maxBuffer: 1024 });
   assert.equal(result.status, equal ? 0 : 2, `relative owned-root branch ${equal ? 'success' : 'refusal'}: status=${result.status}`);
  }
 } finally { await rm(root, { recursive: true, force: true }); }
});
test('native NSIS CheckPath allows owned existing/missing paths and refuses an owned junction', { skip: process.platform !== 'win32', timeout: 30000 }, async () => {
 const root = await realpath(await mkdtemp(join(tmpdir(), 'dilla-check-path-native-')));
 try {
  const owned = join(root, 'owned'), junction = join(root, 'junction'); await mkdir(owned); await writeFile(join(owned, 'owned-file'), 'private scratch');
  await symlink(owned, junction, 'junction');
  for (const [label, path, expected] of [['existing', owned, 0], ['missing-file', join(owned, 'missing'), 0], ['missing-parent', join(owned, 'missing/child'), 0], ['junction', junction, 2]] as const) {
   const executable = await compileCheckPathProbe(root, path, label + '.exe');
   const result = spawnSync(executable, ['/S'], { timeout: 10000, windowsHide: true, stdio: 'pipe', maxBuffer: 1024 });
   assert.equal(result.status, expected, `${label}: read-only CheckPath exit status ${result.status}; signal=${result.signal ?? 'none'}`);
  }
 } finally { await rm(root, { recursive: true, force: true }); }
});
function contract(source: string) {
 source = source.replace(/\r\n/g, '\n');
 assert.match(source, /RequestExecutionLevel user/);
 assert.doesNotMatch(source, /!include "(?:installer|installSection|multiUser|installUtil)\.nsh"|uninstallOldVersion|UAC_RunElevated|ExecShell|taskkill/i);
 const staging = body(source, 'StageInactiveVersion') + body(source, 'CopyInactivePayload');
 assert.doesNotMatch(staging, /WriteReg|DeleteReg|CreateShortCut|WriteUninstaller|Exec|Dilla\.exe|windows-active|initial-slot|RMDir|Delete /);
 assert.match(staging, /Call CheckOwnedRoot/); assert.match(staging, /Call RequireAbsentPath/); assert.match(staging, /IfFileExists "\$INSTDIR\\versions\\\$\{DILLA_PROGRAM_ID\}\\\*" Fail/);
 assert.match(staging, /CopyFiles[^\n]+\\versions\\\$\{DILLA_PROGRAM_ID\}/);
 assert.match(staging, /CopyFiles[^\n]+\\manifests\\\$\{DILLA_MANIFEST_NAME\}/);
 const initial = body(source, 'InstallInitial');
 assert.match(initial, /CopyFiles[^\n]+initial-slot\.json[^\n]+windows-active-0\.json/);
 assert.ok(initial.indexOf('windows-active-0.json') < initial.indexOf('CreateShortCut'));
 assert.match(initial, /Push "\$INSTDIR"\n  Call RequireAbsentPath/);
 assert.match(initial, /CreateShortCut[^\n]+\$INSTDIR\\Dilla\.exe/);
 assert.match(initial, /IfFileExists "\$DataRoot\\windows-active-1.json" Fail/);
 assert.match(body(source, 'un.onInit'), /Call un.CheckOwnedRoot/);
 const uninstall = /Section "Uninstall"([\s\S]*?)SectionEnd/.exec(source)![1];
 assert.match(uninstall, /Push "\$INSTDIR\\Dilla\.exe"\n    Call un.RequireUnlockedFile/);
 assert.ok(uninstall.indexOf('Call un.RequireUnlockedFile') < uninstall.indexOf('Call un.CheckTree'));
 assert.match(body(source, 'un.RequireUnlockedFile'), /CreateFileW[^\n]+0x40010000, i 0/);
 assert.ok(uninstall.indexOf('Call un.ArchiveHistory') > uninstall.indexOf('Call un.CheckTree'));
 assert.ok(uninstall.indexOf('Call un.ArchiveHistory') < uninstall.indexOf('RMDir /r'));
 assert.equal([...uninstall.matchAll(/Call un\.MaintenancePreflight/g)].length, 2);
 assert.ok(uninstall.lastIndexOf('Call un.MaintenancePreflight') > uninstall.lastIndexOf('Call un.CheckTree'));
 assert.ok(uninstall.lastIndexOf('Call un.MaintenancePreflight') < uninstall.indexOf('Call un.HoldStableLauncher'));
 assert.match(uninstall,/Call un.HoldStableLauncher/);
 assert.ok(uninstall.indexOf('Call un.HoldStableLauncher') < uninstall.indexOf('Call un.ArchiveHistory'));
 assert.match(source,/CreateFileW[^\n]+0x40010000, i 4/);
 assert.match(body(source,'un.MaintenancePreflight'),/File \/oname=Dilla\.exe \"\$\{DILLA_BOOTSTRAP_EXE\}\"/);
 assert.doesNotMatch(body(source,'un.MaintenancePreflight'),/CopyFiles/);
 assert.match(body(source, 'un.ArchiveHistory'), /Rename "\$DataRoot" "\$0"/);
 assert.match(body(source, 'un.ArchiveHistory'), /updates-uninstalled-\$GeneratedGUID/);
 assert.match(body(source, '.onInit'), /Call AcquireOperation/);
 assert.match(body(source, 'un.onInit'), /Call un.AcquireOperation/);
 assert.match(body(source, 'StagePreflight'), /--installer-preflight=stage --package-manifest-sha512=\$\{DILLA_MANIFEST_SHA512\}/);
 const prepare = body(source, 'PrepareInitialInstallation');
 assert.equal([...prepare.matchAll(/--installer-preflight=initial/g)].length, 2);
 assert.ok(prepare.lastIndexOf('--installer-preflight=initial') > prepare.lastIndexOf('Call CheckTree'));
 assert.match(prepare, /StrCmp \"?\$0 10/);
 assert.ok(prepare.indexOf('Call RequireUnlockedFile') < prepare.indexOf('Rename \"$INSTDIR'));
 const marker = body(source, 'CommitInitialMarker');
 assert.match(marker, /FlushFileBuffers/); assert.match(marker, /MoveFileExW[^\n]+initial-install\.json[^\n]+i 8/);
 assert.match(initial, /Call CommitInitialMarker/);
 assert.ok(initial.indexOf('Call CommitInitialMarker') < initial.indexOf('Call CopyInactivePayload'));

 assert.match(body(source.replaceAll('${Prefix}', 'un.'), 'un.CheckOwnedRoot'), /ReadRegStr[^\n]+InstallLocation/);
 assert.match(source, /IntOp \$1 \$2 & 0x400/);
 assert.doesNotMatch(source, /RMDir \/r "\$(?:APPDATA|DataRoot)|DeleteRegKey[^\n]+Software\\Dilla/);
 assert.match(source, /StrCpy \$INSTDIR "\$LOCALAPPDATA\\Programs\\\$\{PRODUCT_NAME\}"/);
}
test('custom NSIS stages only inactive immutable program data and seeds the first slot before shell entry points', async () => contract(await readFile(sourcePath, 'utf8')));
test('NSIS contract checks the same operations with Windows checkout line endings', async () => {
 const source = (await readFile(sourcePath, 'utf8')).replace(/\r\n/g, '\n');
 contract(source.replace(/\n/g, '\r\n'));
});
test('installer contract catches staging launcher writes, old-version uninstall and seed order regressions', async () => {
 const source = (await readFile(sourcePath, 'utf8')).replace(/\r\n/g, '\n');
 for (const mutation of [source.replace('Function StageInactiveVersion\n', 'Function StageInactiveVersion\n  CopyFiles "evil" "$INSTDIR\\Dilla.exe"\n'), source + '\nuninstallOldVersion\n', source.replace('Call un.MaintenancePreflight', ''), source.replace('Call un.HoldStableLauncher', ''), source.replace('Call un.ArchiveHistory', ''), source.replace('Call AcquireOperation', ''), source.replace('Call CommitInitialMarker', ''), source.replace('Push \"$INSTDIR\\Dilla.exe\"\n    Call un.RequireUnlockedFile', ''), source.replaceAll('0x40010000', '0x10000'), source.replace('Function StageInactiveVersion\n', 'Function StageInactiveVersion\n  WriteRegStr HKCU \"evil\" \"\" \"value\"\n'), source.replace('IfFileExists \"$DataRoot\\windows-active-1.json\" Fail', ''), source.replace('Call CheckOwnedRoot\n', ''), source.replaceAll('windows-active-0.json', 'slot.json'), source.replace('IntOp $1 $2 & 0x400', 'IntOp $1 $0 & 0')]) assert.throws(() => contract(mutation));
});
test('Windows hooks finalize after signing, assert before archive, and leave other platforms alone', async () => {
 const { createWindowsBuilderHooks } = require('../scripts/windows-builder-hooks.cjs');
 const root = await realpath(await mkdtemp(join(tmpdir(), 'dilla-installer-hooks-')));
 try {
  let changed = false;
  const calls: string[] = [], manifest = join(root, 'a'.repeat(128) + '.json');
  const state = { appOutDir: join(root, 'win-unpacked'), digest: Buffer.alloc(64, 0xaa).toString('base64'), manifestFile: manifest };
  const hooks = createWindowsBuilderHooks({ metadataPublicKeys: ['fixture'], loadPackage: async () => ({ finalizeWindowsProgram: async (options: { signLauncher(file: string): Promise<void> }) => { calls.push('finalize'); await options.signLauncher('native-launcher.exe'); return state; }, assertWindowsProgramUnchanged: async () => { calls.push('assert'); if (changed) throw new Error('late program mutation'); } }) });
  const context = { electronPlatformName: 'win32', arch: 1, appOutDir: join(root, 'win-unpacked'), outDir: root, packager: { config: { protocols: [{ schemes: ['dilla-fixture'] }] }, appInfo: { version: '1.2.3', productName: 'DillaFixture', id: 'dev.dilla.fixture', sanitizedName: 'dilla-fixture' }, getIconPath: async () => 'icon.ico', signIf: async () => { calls.push('sign launcher'); } } };
  await hooks.afterSign({ electronPlatformName: 'linux' }); assert.deepEqual(calls, []);
  await assert.rejects(hooks.artifactBuildStarted({ targetPresentableName: 'nsis', arch: 1, file: join(root, 'setup.exe') }), /finalized/);
  await hooks.afterSign(context); await hooks.artifactBuildStarted({ targetPresentableName: 'nsis', arch: 1, file: join(root, 'setup.exe') });
  assert.deepEqual(calls, ['finalize', 'sign launcher', 'assert']);
  const include = await readFile(join(root, 'dilla-fixture-1.2.3-x64.nsis.7z.bootstrap.nsh'), 'utf8');
  assert.match(include, /DILLA_PROGRAM_ID "v-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"/); assert.match(include, new RegExp(`DILLA_MANIFEST_NAME "${'a'.repeat(128)}\\.json"`));
  changed = true;
  await assert.rejects(hooks.artifactBuildStarted({ targetPresentableName: 'nsis', arch: 1, file: join(root, 'setup.exe') }), /late program mutation/);
 } finally { await rm(root, { recursive: true, force: true }); }
});
