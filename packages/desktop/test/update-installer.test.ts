import { test } from 'node:test';
import assert from 'node:assert/strict';
import { spawn, spawnSync } from 'node:child_process';
import { readFile, writeFile } from 'node:fs/promises';
import { join, resolve } from 'node:path';
import { installedFixture } from './support/installed-update.ts';
import { installerActorCommand, installerActorParent, runWindowsInstaller, type InstallerActorHost } from '../src/update-installer.ts';

function actorProgram(plan: Parameters<typeof runWindowsInstaller>[0]): string {
 const config = JSON.stringify({ directory: plan.health.directory, installed: plan.installed, nonce: plan.health.nonce, version: plan.oldVersion });
 return `const fs=require('node:fs'),{spawn}=require('node:child_process'),c=${config};
 const worker=spawn(process.execPath,['-e',\`const fs=require('node:fs');const c=\${JSON.stringify(c)};let timer=setInterval(()=>{try{const go=JSON.parse(fs.readFileSync(c.directory+'/installer-go.json','utf8'));if(go.nonce!==c.nonce||go.pid!==process.ppid||go.installerPid!==process.pid)return;const j=JSON.parse(fs.readFileSync(c.directory+'/journal.json','utf8'));if(j.installerPid!==process.pid||j.installerActorPid!==process.ppid)process.exit(7);fs.writeFileSync(c.installed,'installer-ran-after-registration');clearInterval(timer);process.exit(0)}catch{}},10);\`],{stdio:'ignore'});
 fs.writeFileSync(c.directory+'/installer-ready.json',JSON.stringify({format:1,nonce:c.nonce,pid:process.pid,installerPid:worker.pid,version:c.version}),{mode:0o600});worker.on('exit',code=>process.exit(code??8));`;
}
const stopped = async (child: { pid?: number }) => {
 if (child.pid) { try { process.kill(-child.pid, 'SIGKILL'); } catch (error) { if ((error as NodeJS.ErrnoException).code !== 'ESRCH') throw error; } }
};

test('installer actor data is encoded without shell interpolation and identity cannot select another plan', async () => installedFixture(async plan => {
 const hostile = "C:\\Users\\fixture';Start-Process calc;#\\AppData\\update.exe";
 const modified = { ...plan, artifactFile: hostile }, encoded = installerActorCommand(modified, process.pid);
 assert.equal(Buffer.from(encoded, 'base64').toString('utf16le').includes(hostile), false);
 assert.equal(installerActorParent(modified, encoded), process.pid);
 assert.equal(installerActorParent(plan, encoded), null);
 assert.equal(installerActorParent(modified, Buffer.from('Start-Process calc', 'utf16le').toString('base64')), null);
}, 'win32'));

test('real installer actor cannot mutate the program before its actual PID has been durably registered', { skip: process.platform === 'win32' }, async () => installedFixture(async plan => {
 const host: InstallerActorHost = { spawn: async () => spawn(process.execPath, ['-e', actorProgram(plan)], { detached: true, stdio: 'ignore' }), stop: stopped, timeoutMs: 3000 };
 await runWindowsInstaller(plan, host);
 assert.equal(await readFile(plan.installed, 'utf8'), 'installer-ran-after-registration');
 const journal = JSON.parse(await readFile(join(plan.health.directory, 'journal.json'), 'utf8'));
 assert.ok(journal.installerActorPid > 1 && journal.installerPid > 1 && journal.installerActorPid !== journal.installerPid);
}, 'win32'));

for (const fault of ['open', 'write', 'sync']) test(`installer ${fault} ENOSPC before PID registration prevents go and stops its real owned processes`, { skip: process.platform === 'win32' }, async () => installedFixture(async (plan, _trust, root) => {
 const control = join(root, 'options.json'), result = join(root, 'result.json');
 await writeFile(control, JSON.stringify({ plan, fault, result, actor: actorProgram(plan), module: resolve(import.meta.dirname, '../src/update-installer.ts') }));
 const script = `import fs from 'node:fs/promises';import{spawn}from'node:child_process';const o=JSON.parse(await fs.readFile(process.argv[1],'utf8'));const original=fs.open;let started=false;
 fs.open=async function(path,...args){const fail=started&&String(path).includes('journal.json.');const error=()=>Object.assign(new Error('registration quota ENOSPC'),{code:'ENOSPC'});if(fail&&o.fault==='open')throw error();const h=await original(path,...args);if(fail&&o.fault==='write')h.writeFile=async()=>{throw error()};if(fail&&o.fault==='sync')h.sync=async()=>{throw error()};return h};
 const{runWindowsInstaller}=await import(o.module);let error,stops=0;try{await runWindowsInstaller(o.plan,{spawn:async()=>{started=true;return spawn(process.execPath,['-e',o.actor],{detached:true,stdio:'ignore'})},stop:async child=>{stops++;try{process.kill(-child.pid,'SIGKILL')}catch(e){if(e.code!=='ESRCH')throw e}},timeoutMs:3000})}catch(e){error=String(e)}
 let go=false;try{await fs.stat(o.plan.health.directory+'/installer-go.json');go=true}catch{}await fs.writeFile(o.result,JSON.stringify({error,stops,go,installed:await fs.readFile(o.plan.installed,'utf8')}));`;
 const child = spawnSync(process.execPath, ['--input-type=module', '--eval', script, control], { encoding: 'utf8', timeout: 10000 });
 assert.equal(child.status, 0, child.stderr);
 const actual = JSON.parse(await readFile(result, 'utf8')); assert.match(actual.error, /ENOSPC/); assert.equal(actual.go, false); assert.equal(actual.stops, 1); assert.equal(actual.installed, 'working-old-app');
}, 'win32'));

test('Windows PowerShell 5.1 compiles the C# actor and atomically contains a suspended process with kill-on-close', { skip: process.platform !== 'win32' }, async () => {
 const { INSTALLER_JOB_CSHARP } = await import('../src/update-installer.ts'), { win32 } = await import('node:path');
 const script = `$ErrorActionPreference='Stop'\nAdd-Type -TypeDefinition @'\n${INSTALLER_JOB_CSHARP}\n'@\n$job=[IntPtr]::Zero;$child=$null\ntry{$job=[DillaInstallerKernel]::CreateJob('Local\\DillaKernelTest-'+[Guid]::NewGuid().ToString('N'));$child=[DillaInstallerKernel]::CreateSuspended($job,(Join-Path $PSHOME 'powershell.exe'),[IO.Path]::GetTempPath());if([DillaInstallerKernel]::WaitForSingleObject($child.hProcess,0) -ne 258){throw 'Suspended process missing'};if(![DillaInstallerKernel]::CloseHandle($job)){throw 'Job close failed'};$job=[IntPtr]::Zero;if([DillaInstallerKernel]::WaitForSingleObject($child.hProcess,10000) -ne 0){throw 'Process escaped kill-on-close'};Write-Output 'contained'}finally{if($null -ne $child){[void][DillaInstallerKernel]::CloseHandle($child.hThread);[void][DillaInstallerKernel]::CloseHandle($child.hProcess)};if($job -ne [IntPtr]::Zero){[void][DillaInstallerKernel]::CloseHandle($job)}}`;
 const result = spawnSync(win32.join(process.env.SystemRoot ?? 'C:\\Windows', 'System32/WindowsPowerShell/v1.0/powershell.exe'), ['-NoProfile', '-NonInteractive', '-EncodedCommand', Buffer.from(script, 'utf16le').toString('base64')], { encoding: 'utf8', timeout: 60000, windowsHide: true });
 assert.equal(result.status, 0, result.stderr); assert.match(result.stdout, /contained/);
});
