import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, mkdir, readFile, realpath, rm, writeFile } from 'node:fs/promises';
import { join, win32 } from 'node:path';
import { tmpdir } from 'node:os';
import { createRequire } from 'node:module';
import { execFileSync, spawnSync } from 'node:child_process';
// @ts-expect-error test tooling is JavaScript
import { compileWindowsNSIS } from './support/windows-nsis.mjs';

const require = createRequire(import.meta.url);
const { nsisEscapeString: escape } = require('app-builder-lib/out/targets/nsis/nsisScriptGenerator');
const sourcePath = new URL('../build/windows-installer.nsi', import.meta.url);
const cases = ['ancestor', 'delete-open', 'traverse-only'] as const;
type Case = typeof cases[number];

function extract(source: string, name: string): string {
 const match = new RegExp(`^Function ${name}\\r?\\n([\\s\\S]*?)^FunctionEnd`, 'm').exec(source);
 assert.ok(match, `shipping function ${name} must exist`);
 return `Function ${name}\n${match[1]}FunctionEnd\n`;
}

async function compileProbe(root: string, kind: Case, crlf: boolean): Promise<string> {
 const source = (await readFile(sourcePath, 'utf8')).replaceAll('\r\n', '\n').replaceAll('\n', crlf ? '\r\n' : '\n').replaceAll('${Prefix}', '');
 let functions = ['InitializePrivatePayload', 'PinPayloadDirectory', 'VerifyPayloadPluginOwner', 'FreePayloadSecurity', 'ReleasePrivatePayload']
  .map(name => extract(source, name)).join('\n');
 // Observe the actual shipping CreateFile result without changing its arguments,
 // branches or access mask. GetLastError must be captured by System itself.
 const open = /System::Call 'kernel32::CreateFileW\(w r0,[^\r\n]+\)p\.r1'/g;
 assert.equal([...functions.matchAll(open)].length, 1);
 functions = functions.replace(open, call => call.slice(0, -1) + " ?e'\n  Pop $ProbePinError");
 const macros = [...source.matchAll(/^!macro FreePayload(?:Local|Buffer) Variable\r?\n[\s\S]*?^!macroend/gm)].map(match => match[0]).join('\n');
 const globals = [...source.matchAll(/^Var (Payload[^\r\n]+)\r?$/gm)]
  .filter(match => functions.includes('$' + match[1].trim()))
  .map(match => 'Var ' + match[1].trim()).join('\n');
 const nativeRoot = process.platform === 'win32' ? root : 'C:\\DillaPinCompile';
 const target = escape(win32.join(nativeRoot, 'ancestor')), moved = escape(win32.join(nativeRoot, 'renamed')), report = escape(win32.join(nativeRoot, 'result.txt'));
 return compileWindowsNSIS({ directory: root, archive: join(root, 'unused.7z'), name: `${kind}-${crlf ? 'crlf' : 'lf'}.exe`, transform: () => `
Name "Private Dilla directory pin contract"
RequestExecutionLevel user
SilentInstall silent
!include "LogicLib.nsh"
${globals}
Var ProbePhase
Var ProbePinError
Var ProbeDeleteHandle
${macros}
Function Report
 FileOpen $0 "${report}" w
 IfErrors 0 +2
  Quit
 FileWrite $0 "$ProbePhase:$ProbePinError"
 FileClose $0
FunctionEnd
Function Fail
 Call Report
 Call ReleasePrivatePayload
 \${If} $ProbeDeleteHandle != 0
  System::Call 'kernel32::CloseHandle(p $ProbeDeleteHandle)'
 \${EndIf}
 SetErrorLevel 2
 Quit
FunctionEnd
${functions}
Section
 Call InitializePrivatePayload
 ; The target is an independent ancestor, never the NSIS plug-in directory.
 StrCpy $PayloadUserSID ""
 StrCpy $ProbeDeleteHandle 0
 StrCpy $ProbePinError 0
 StrCpy $ProbePhase "allocate"
 System::Alloc 52
 Pop $PayloadInfo
 StrCmp $PayloadInfo 0 0 +2
  Call Fail
 ${kind === 'traverse-only' ? `
 StrCpy $ProbePhase "list-must-be-denied"
 System::Call 'kernel32::CreateFileW(w "${target}", i 1, i 7, p 0, i 3, i 0x02000000, p 0)p.r1 ?e'
 Pop $2
 StrCmp $1 -1 +3
  System::Call 'kernel32::CloseHandle(p r1)'
  Call Fail
 StrCmp $2 5 +2
  Call Fail
 ` : ''}
 ${kind === 'delete-open' ? `
 StrCpy $ProbePhase "open-delete"
 System::Call 'kernel32::CreateFileW(w "${target}", i 0x10000, i 7, p 0, i 3, i 0x02000000, p 0)p.r1'
 StrCmp $1 -1 0 +2
  Call Fail
 StrCpy $ProbeDeleteHandle $1
 ` : ''}
 StrCpy $ProbePhase "pin"
 Push "${target}"
 Call PinPayloadDirectory
 StrCpy $ProbePinError 0
 ${kind === 'delete-open' ? 'StrCpy $ProbePhase "unexpected-pin-success"\n Call Fail' : `
 ; Only the ancestor is pinned: no child handle can accidentally block rename.
 StrCpy $ProbePhase "rename-must-be-blocked"
 ClearErrors
 Rename "${target}" "${moved}"
 IfErrors +2
  Call Fail
 StrCpy $ProbePhase "release"
 Call ReleasePrivatePayload
 IfErrors 0 +2
  Call Fail
 ; Same source/destination and nonempty directory; prove the earlier refusal
 ; came from our retained pin rather than an unrelated permission or image lock.
 StrCpy $ProbePhase "rename-after-release"
 ClearErrors
 Rename "${target}" "${moved}"
 IfErrors 0 +2
  Call Fail
 StrCpy $ProbePhase "complete"
 Call Report
 SetErrorLevel 0
 `}
SectionEnd
` });
}

function setListDenied(path: string, deny: boolean): void {
 const literal = "'" + path.replaceAll("'", "''") + "'";
 const script = `$ErrorActionPreference='Stop'; $sid=[Security.Principal.WindowsIdentity]::GetCurrent().User;
 $acl=New-Object Security.AccessControl.DirectorySecurity; $acl.SetOwner($sid); $acl.SetAccessRuleProtection($true,$false);
 $inherit=[Security.AccessControl.InheritanceFlags]::ContainerInherit -bor [Security.AccessControl.InheritanceFlags]::ObjectInherit;
 $acl.AddAccessRule((New-Object Security.AccessControl.FileSystemAccessRule($sid,[Security.AccessControl.FileSystemRights]::FullControl,$inherit,[Security.AccessControl.PropagationFlags]::None,[Security.AccessControl.AccessControlType]::Allow)));
 ${deny ? '$acl.AddAccessRule((New-Object Security.AccessControl.FileSystemAccessRule($sid,[Security.AccessControl.FileSystemRights]::ListDirectory,[Security.AccessControl.AccessControlType]::Deny)));' : ''}
 [IO.Directory]::SetAccessControl(${literal},$acl);`;
 execFileSync('powershell.exe', ['-NoLogo', '-NoProfile', '-NonInteractive', '-EncodedCommand', Buffer.from(script, 'utf16le').toString('base64')], { windowsHide: true, timeout: 15000, stdio: 'pipe' });
}

for (const crlf of [false, true]) {
 test(`shipping directory pin probes compile with -WX from ${crlf ? 'CRLF' : 'LF'} source`, async () => {
  const root = await realpath(await mkdtemp(join(tmpdir(), 'dilla-pin-compile-')));
  try { for (const kind of cases) await compileProbe(root, kind, crlf); }
  finally { await rm(root, { recursive: true, force: true }); }
 });
}

async function nativeProbe(kind: Case, crlf: boolean): Promise<void> {
  const started = performance.now();
  // Fixed case/phase labels and elapsed time only: no paths, native errors,
  // command lines or ACL contents enter these first-failure diagnostics.
  const phase = (name: string) => console.log(`WINDOWS_PIN_PROBE ${kind} ${crlf ? 'CRLF' : 'LF'} ${name} ${Math.round(performance.now() - started)}ms`);
  phase('setup-start');
  const root = await realpath(await mkdtemp(join(tmpdir(), 'dilla-pin-native-'))), target = join(root, 'ancestor'), moved = join(root, 'renamed');
  try {
   await mkdir(join(target, 'child'), { recursive: true }); await writeFile(join(target, 'child', 'identity.txt'), 'original');
   phase('compile-start');
   const executable = await compileProbe(root, kind, crlf);
   phase('compile-end');
   if (kind === 'traverse-only') { phase('deny-list-start'); setListDenied(target, true); phase('deny-list-end'); }
   phase('native-start');
   const child = spawnSync(executable, ['/S'], { windowsHide: true, timeout: 15000, encoding: 'utf8', maxBuffer: 4096 });
   phase('native-end');
   assert.ifError(child.error);
   phase('report-read-start');
   const result = await readFile(join(root, 'result.txt'), 'utf8');
   phase('report-read-end');
   phase('assertions-start');
   if (kind === 'delete-open') {
    assert.equal(child.status, 2, result);
    assert.equal(result, 'pin:32', 'the actual pin must refuse the already-open DELETE handle with ERROR_SHARING_VIOLATION');
    // Its owning process has exited; the otherwise identical pin and rename
    // must now succeed, proving the conflict was the preexisting DELETE handle.
    phase('retry-compile-start');
    const retryExecutable = await compileProbe(root, 'ancestor', crlf);
    phase('retry-native-start');
    const retry = spawnSync(retryExecutable, ['/S'], { windowsHide: true, timeout: 15000, encoding: 'utf8', maxBuffer: 4096 });
    phase('retry-native-end');
    assert.ifError(retry.error); assert.equal(retry.status, 0, await readFile(join(root, 'result.txt'), 'utf8'));
   } else { assert.equal(child.status, 0, result); assert.equal(result, 'complete:0'); }
   phase('identity-read-start');
   assert.equal(await readFile(join(moved, 'child', 'identity.txt'), 'utf8'), 'original');
   phase('identity-read-end');
   phase('old-path-check-start');
   await assert.rejects(realpath(target), { code: 'ENOENT' });
   phase('assertions-end');
  } catch (error) {
   const code = (error as NodeJS.ErrnoException | null)?.code;
   phase('failure-' + (typeof code === 'string' && ['EACCES', 'EPERM', 'ENOENT', 'ERR_ASSERTION'].includes(code) ? code : 'other'));
   throw error;
  } finally {
   if (kind === 'traverse-only') for (const [name, path] of [['original', target], ['renamed', moved]]) {
    try {
     phase(`restore-${name}-realpath-start`); await realpath(path); phase(`restore-${name}-realpath-end`);
     phase(`restore-${name}-acl-start`); setListDenied(path, false); phase(`restore-${name}-acl-end`);
    } catch (error) {
     if ((error as NodeJS.ErrnoException).code !== 'ENOENT') throw error;
     phase(`restore-${name}-missing`);
    }
   }
   phase('remove-start');
   await rm(root, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 });
   phase('remove-end');
  }
}

test('native shipping directory pins retain all six LF/CRLF contracts', { skip: process.platform !== 'win32', timeout: 60000 }, async context => {
 // A failure stops the matrix. Timeout cancellation also prevents an eventual
 // completion of the current case from starting another expensive native probe.
 for (const crlf of [false, true]) for (const kind of cases) {
  context.signal.throwIfAborted();
  await nativeProbe(kind, crlf);
 }
});
