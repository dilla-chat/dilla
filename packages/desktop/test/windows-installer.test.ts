import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFile, mkdtemp, rm } from 'node:fs/promises';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { createRequire } from 'node:module';
const require = createRequire(import.meta.url);
const sourcePath = new URL('../build/windows-installer.nsi', import.meta.url);
function body(source: string, name: string): string { const match = new RegExp(`Function ${name}\\r?\\n([\\s\\S]*?)FunctionEnd`).exec(source); assert.ok(match, `missing ${name}`); return match[1]; }
function contract(source: string) {
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
test('installer contract catches staging launcher writes, old-version uninstall and seed order regressions', async () => {
 const source = await readFile(sourcePath, 'utf8');
 for (const mutation of [source.replace('Function StageInactiveVersion\n', 'Function StageInactiveVersion\n  CopyFiles "evil" "$INSTDIR\\Dilla.exe"\n'), source + '\nuninstallOldVersion\n', source.replace('Call un.MaintenancePreflight', ''), source.replace('Call un.HoldStableLauncher', ''), source.replace('Call un.ArchiveHistory', ''), source.replace('Call AcquireOperation', ''), source.replace('Call CommitInitialMarker', ''), source.replace('Push \"$INSTDIR\\Dilla.exe\"\n    Call un.RequireUnlockedFile', ''), source.replaceAll('0x40010000', '0x10000'), source.replace('Function StageInactiveVersion\n', 'Function StageInactiveVersion\n  WriteRegStr HKCU \"evil\" \"\" \"value\"\n'), source.replace('IfFileExists \"$DataRoot\\windows-active-1.json\" Fail', ''), source.replace('Call CheckOwnedRoot\n', ''), source.replaceAll('windows-active-0.json', 'slot.json'), source.replace('IntOp $1 $2 & 0x400', 'IntOp $1 $0 & 0')]) assert.throws(() => contract(mutation));
});
test('Windows hooks finalize after signing, assert before archive, and leave other platforms alone', async () => {
 const { createWindowsBuilderHooks } = require('../scripts/windows-builder-hooks.cjs');
 const root = await mkdtemp(join(tmpdir(), 'dilla-installer-hooks-'));
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
