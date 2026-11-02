import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFile, writeFile, mkdir, symlink, mkdtemp, rm, realpath } from 'node:fs/promises';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { createRequire } from 'node:module';
import { spawnSync, execFileSync } from 'node:child_process';
import { createHash, generateKeyPairSync } from 'node:crypto';
// @ts-expect-error test tooling is JavaScript
import { compileWindowsNSIS, withInitialInstallDiagnostics, withInitialRetryNativeDiagnostic, initialRetryNativeDiagnostic } from './support/windows-nsis.mjs';
const require = createRequire(import.meta.url);
const sourcePath = new URL('../build/windows-installer.nsi', import.meta.url);
function body(source: string, name: string): string { const match = new RegExp(`Function ${name}\\r?\\n([\\s\\S]*?)FunctionEnd`).exec(source); assert.ok(match, `missing ${name}`); return match[1].replaceAll('\r\n', '\n'); }
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
async function compileDiagnosticFlagProbe(directory: string, hooked: boolean, errorFlag: boolean, status: number): Promise<string> {
 const source = `
Name "Private Dilla diagnostic flag boundary"
RequestExecutionLevel user
SilentInstall silent
!include "LogicLib.nsh"
Var FixtureNativeExit
Function FixtureInitialPreflight1
  StrCpy $FixtureNativeExit $0
FunctionEnd
Function FixtureInitialPreflight2
  StrCpy $FixtureNativeExit $0
FunctionEnd
Section
 Call FixtureInitialPreflight2
 StrCpy $0 ${status}
 ${errorFlag ? 'SetErrors' : 'ClearErrors'}
 Call FixtureInitialPreflight1
 IfErrors error_seen no_error
 error_seen:
 StrCpy $R0 1
 Goto observed
 no_error:
 StrCpy $R0 0
 observed:
 StrCmp $R0 ${errorFlag ? 1 : 0} 0 fail
 StrCmp $0 ${status} 0 fail
 SetErrorLevel 0
 Goto done
 fail:
 SetErrorLevel 2
 done:
SectionEnd
`;
 return compileWindowsNSIS({ directory, archive: join(directory, 'unused.7z'), name: `flag-${hooked}-${errorFlag}-${status}.exe`, transform: () => hooked ? withInitialRetryNativeDiagnostic(source, join(directory, 'unused-diagnostic.exe')) : source });
}
test('the actual extracted NSIS CheckPath compiles with the pinned System plugin', async () => {
 const root = await realpath(await mkdtemp(join(tmpdir(), 'dilla-check-path-compile-')));
 try {
  const executable = await compileCheckPathProbe(root, join(root, 'missing'), 'check-path.exe');
  assert.equal((await readFile(executable)).subarray(0, 2).toString(), 'MZ');
  assert.equal((await readFile(await compileDiagnosticBranchProbe(root, true))).subarray(0, 2).toString(), 'MZ');
  assert.equal((await readFile(await compileDiagnosticFlagProbe(root, true, true, 1))).subarray(0, 2).toString(), 'MZ');
 } finally { await rm(root, { recursive: true, force: true }); }
});
test('native retry diagnostic hook preserves original NSIS error flags and exit statuses', { skip: process.platform !== 'win32', timeout: 30000 }, async () => {
 const root = await realpath(await mkdtemp(join(tmpdir(), 'dilla-diagnostic-flags-')));
 try {
  const env = { ...process.env }; delete env.DILLA_INITIAL_RETRY_DIAGNOSTIC_REQUEST;
  for (const hooked of [false, true]) for (const errorFlag of [false, true]) for (const status of [0, 1]) {
   const executable = await compileDiagnosticFlagProbe(root, hooked, errorFlag, status), result = spawnSync(executable, ['/S'], { timeout: 10000, windowsHide: true, stdio: 'pipe', maxBuffer: 1024, env });
   assert.equal(result.status, 0, `hooked=${hooked}, originalError=${errorFlag}, originalStatus=${status}: status=${result.status}`);
  }
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
test('installer creates a new protected payload before extraction and retains non-delete-sharing ancestor pins', async () => {
 const source = await readFile(sourcePath, 'utf8'), privateSource = source.replaceAll('${Prefix}', ''), extract = body(source, 'ExtractPayload'), prepare = body(privateSource, 'PreparePrivatePayload');
 assert.ok(extract.indexOf('Call PreparePrivatePayload') >= 0 && extract.indexOf('Call PreparePrivatePayload') < extract.indexOf('SetOutPath'));
 assert.match(extract, /File \/oname=\$PLUGINSDIR\\payload\\payload\.7z/);
 assert.match(extract, /Delete "\$PLUGINSDIR\\payload\\payload\.7z"/);
 assert.match(prepare, /GetTokenInformation/); assert.match(prepare, /O:\$PayloadUserSID[\s\S]*D:P.*OICI;FA/);
 assert.match(prepare, /NtCreateFile[^\n]+i 3, i 2, i 0x200021/); assert.match(prepare, /StrCmp \$1 2 0 Fail ; FILE_CREATED/);
 assert.doesNotMatch(prepare, /CreateDirectoryW|SetNamedSecurityInfo|SetFileSecurity|SetSecurityInfo/);
 assert.match(prepare, /GetDriveTypeW/); assert.match(prepare, /StrCmp \$R1 \$R3 normalize_root/); assert.match(prepare, /normalize_root:\r?\n\s+StrCpy \$R0 \$PayloadDriveRoot/);
 assert.match(body(privateSource, 'PinPayloadDirectory'), /CreateFileW[^\n]+i 3[^\n]+0x02200000/);
 assert.match(body(privateSource, 'PinPayloadDirectory'), /GetFileInformationByHandle/);
 assert.match(prepare, /Call VerifyPrivatePayload/);
 assert.match(body(privateSource, 'VerifyPrivatePayload'), /EqualSid/);
 assert.match(body(privateSource, 'VerifyPrivatePayload'), /GetSecurityDescriptorControl/);
 assert.match(body(privateSource, 'VerifyPrivatePayload'), /ConvertSecurityDescriptorToStringSecurityDescriptorW/);
 assert.match(body(privateSource, 'RequireEmptyPayload'), /FindFirstFileW[^\n]+\?e/);
 assert.match(body(privateSource, 'RequireEmptyPayload'), /FindNextFileW[^\n]+\?e/);
 assert.match(body(source, 'Fail'), /Call ReleasePrivatePayload/);
 assert.match(body(source, '.onGUIEnd'), /Call ReleasePrivatePayload/);
});
test('private payload fault boundaries survive actual LF and CRLF NSIS checkouts', async () => {
 const source = (await readFile(sourcePath, 'utf8')).replaceAll('\r\n', '\n');
 const boundary = '  Push "$PLUGINSDIR\\payload"\n  Call PinPayloadDirectory\n  ; The atomic handle';
 for (const input of [source, source.replaceAll('\n', '\r\n')]) {
  const prepare = body(input.replaceAll('${Prefix}', ''), 'PreparePrivatePayload');
  assert.equal(prepare.split(boundary).length, 2, 'the actual post-creation fault boundary must match exactly once');
  assert.equal(prepare.includes('\r'), false, 'extracted fixture functions use normalized newlines');
 }
});
test('native private NSIS payload passes unchanged Go ACL guards and refuses unsafe or preexisting namespaces', { skip: process.platform !== 'win32', timeout: 120000 }, async () => {
 const root = await realpath(await mkdtemp(join(tmpdir(), 'dilla-private-payload-')));
 try {
  const hash = (raw: Uint8Array) => createHash('sha512').update(raw).digest('base64'), source = await readFile(sourcePath, 'utf8');
  const runtime = Buffer.from('program'), manifest = Buffer.from(JSON.stringify({ format: 1, kind: 'dilla-desktop-program', version: '1.2.3', channel: 'stable', platform: 'win32', arch: 'x64', files: [{ path: 'dilla-desktop.exe', size: runtime.length, sha512: hash(runtime) }] }));
  const digest = hash(manifest), hex = Buffer.from(digest, 'base64').toString('hex'), id = 'v-' + hex.slice(0, 32), payload = join(root, 'archive-source');
  await mkdir(join(payload, 'versions', id), { recursive: true }); await mkdir(join(payload, 'manifests'));
  await writeFile(join(payload, 'versions', id, 'dilla-desktop.exe'), runtime); await writeFile(join(payload, 'manifests', hex + '.json'), manifest);
  const archive = join(root, 'payload.7z'); await require('app-builder-lib/out/targets/archive').archive('7z', archive, payload, { withoutDir: true, compression: 'normal', installTimeDecodable: true });
  const binary = join(root, 'private-acl.test.exe'); execFileSync('go', ['test', '-c', '-o', binary, './packages/desktop/native/windows-launcher'], { cwd: join(import.meta.dirname, '../../..'), env: { ...process.env, CGO_ENABLED: '0' }, stdio: 'pipe', timeout: 60000 });
  const config = { format: 1, appId: 'dev.dilla.privatepayload.fixture', productName: 'DillaPrivatePayloadFixture', channel: 'stable', arch: 'x64', initialVersion: '1.2.3', initialManifestSha512: digest, publicKeys: [generateKeyPairSync('ed25519').publicKey.export({ type: 'spki', format: 'pem' }).toString()] };
  const request = join(root, 'request.json'), report = join(root, 'initial-retry-native.json'); await writeFile(request, JSON.stringify({ config, report }));
  const other = join(root, 'other'), junction = join(root, 'junction'); await mkdir(other); await symlink(other, junction, 'junction');
  const { nsisEscapeString } = require('app-builder-lib/out/targets/nsis/nsisScriptGenerator');
  for (const kind of ['default', 'unsafe-acl', 'protected', 'preexisting', 'reparse', 'api-error', 'substituted-acl', 'substituted-nonempty', 'dangling-payload']) {
   await rm(report, { force: true });
   const substituted = join(root, kind + '-substituted');
   const danglingTarget = join(root, 'must-remain-absent');
   let functions = ['InitializePrivatePayload','PreparePrivatePayload','PinPayloadDirectory','VerifyPrivatePayload','RequireEmptyPayload','VerifyPayloadPluginOwner','FreePayloadSecurity','ReleasePrivatePayload','CheckPath','CheckTree','RequireUnlockedFile','ExtractPayload'].map(name => `Function ${name}\n${body(source.replaceAll('${Prefix}', ''), name)}\nFunctionEnd\n`).join('\n');
   functions = functions.replace(/Function PinPayloadDirectory\r?\n  Exch \$0/, 'Function PinPayloadDirectory\n  Exch $0\n  StrCmp $0 $PayloadDriveRoot 0 +2\n    StrCpy $ProbeRootSeen 1');
   const freeMacros = [...source.matchAll(/^!macro FreePayload(?:Local|Buffer) Variable\r?\n[\s\S]*?^!macroend/gm)].map(match => match[0]).join('\n');
   if (kind === 'default') functions = functions.replace('  Call PreparePrivatePayload', '  Nop');
   if (kind === 'unsafe-acl') functions = functions.replace('D:P(A;OICI;FA;', 'D:P(A;OICI;0x00130041;;;WD)(A;OICI;FA;');
   if (kind === 'reparse') functions = functions.replaceAll('$PLUGINSDIR', nsisEscapeString(junction));
   if (kind === 'api-error') functions = functions.replace(/System::Call 'ntdll::NtCreateFile[^\n]+/, 'StrCpy $1 -1');
   if (kind.startsWith('substituted-')) {
    const boundary = '  Push "$PLUGINSDIR\\payload"\n  Call PinPayloadDirectory\n  ; The atomic handle';
    assert.equal(functions.split(boundary).length, 2, 'inject exactly after actual atomic creation and before its second pin');
    const marker = `  FileOpen $0 "${nsisEscapeString(substituted)}" w
  IfErrors Fail
  FileWrite $0 "observed"
  FileClose $0`;
    const injection = kind === 'substituted-acl' ? `
  ClearErrors
  Rename "$PLUGINSDIR\\payload" "$PLUGINSDIR\\original-payload"
  IfErrors blocked
  ; A successful rename is a security regression, even if a later check fails.
  Call Fail
  blocked:
${marker}
  Call Fail
` : `
  FileOpen $0 "$PLUGINSDIR\\payload\\substituted" w
  IfErrors Fail
  FileWrite $0 "unexpected content"
  FileClose $0
${marker}
`;
    functions = functions.replace(boundary, injection + boundary);
   }
   const globals = [...source.matchAll(/^Var Payload[^\r\n]+/gm)].map(match => match[0]).join('\n');
   const executable = await compileWindowsNSIS({ directory: root, archive, name: kind + '.exe', transform: () => `
Name "Private Dilla ACL probe"
RequestExecutionLevel user
SilentInstall silent
!include "LogicLib.nsh"
!include "FileFunc.nsh"
!define DILLA_PROGRAM_ID "${id}"
!define DILLA_MANIFEST_NAME "${hex}.json"
!define /math DILLA_OBJECT_ATTRIBUTES_SIZE \${NSIS_PTR_SIZE} * 6
${globals}
Var ProbeRootSeen
${freeMacros}
Function .onInit
 Call InitializePrivatePayload
 StrCpy $ProbeRootSeen 0
 InitPluginsDir
FunctionEnd
Function Fail
 Call ReleasePrivatePayload
 SetErrorLevel 2
 Quit
FunctionEnd
${functions}
Section
 GetFunctionAddress $R9 PreparePrivatePayload
 ${kind === 'preexisting' ? 'CreateDirectory "$PLUGINSDIR\\payload"' : 'Nop'}
 ${kind === 'dangling-payload' ? `nsExec::ExecToStack /TIMEOUT=10000 '\"$SYSDIR\\cmd.exe\" /d /c mklink /J \"$PLUGINSDIR\\payload\" \"${nsisEscapeString(danglingTarget)}\"'
 Pop $R0
 Pop $R1
 StrCmp $R0 0 +2
  Call Fail
 FileOpen $0 "${nsisEscapeString(substituted)}" w
 IfErrors 0 +2
  Call Fail
 FileWrite $0 "observed"
 FileClose $0` : 'Nop'}
 Call ExtractPayload
 ${kind === 'default' ? 'Nop' : 'StrCmp $ProbeRootSeen 1 +2\n  Call Fail'}
 nsExec::ExecToStack /TIMEOUT=15000 '"${nsisEscapeString(binary)}" -test.run=TestInitialRetryNativeDiagnostic -test.count=1 -dilla-acl-only "-dilla-initial-payload=$PLUGINSDIR\\payload"'
 Pop $R0
 Pop $R1
 StrCmp $R0 0 +2
  Call Fail
 Call ReleasePrivatePayload
 SetErrorLevel 0
SectionEnd
` });
   const child = spawnSync(executable, ['/S'], { encoding: 'utf8', timeout: 30000, windowsHide: true, env: { ...process.env, DILLA_INITIAL_RETRY_DIAGNOSTIC_REQUEST: request }, maxBuffer: 1024 });
   if (['preexisting','reparse','api-error','substituted-acl','substituted-nonempty','dangling-payload'].includes(kind)) {
    assert.equal(child.status, 2, kind + ' must fail before helper execution'); await assert.rejects(readFile(report), { code: 'ENOENT' });
    if (kind.startsWith('substituted-')) assert.equal(await readFile(substituted, 'utf8'), 'observed', 'the live atomic handle blocked replacement, or the actual pinned-empty guard refused injected content');
    if (kind === 'dangling-payload') { assert.equal(await readFile(substituted, 'utf8'), 'observed', 'the dangling final junction was created before the actual FILE_CREATE call'); await assert.rejects(realpath(danglingTarget), { code: 'ENOENT' }); }
    continue;
   }
   assert.equal(child.status, 0, kind + ': bounded payload probe must complete');
   const result = initialRetryNativeDiagnostic(JSON.parse(await readFile(report, 'utf8')));
   if (kind === 'protected') assert.equal(result.stage, 'ready', 'real private descendants must pass unchanged Go ownership/DACL/file/tree guards');
   else if (kind === 'unsafe-acl') assert.equal(result.reason, 'untrusted-acl', kind + ': unsafe inherited mutation rights remain rejected');
   else assert.ok(result.stage === 'ready' || result.reason === 'untrusted-acl', 'observe the modern toolchain default without assuming its ACL policy');
  }
 } finally { await rm(root, { recursive: true, force: true, maxRetries: 10, retryDelay: 100 }); }
});
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
