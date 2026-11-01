import { spawn, execFile, type ChildProcess } from 'node:child_process';
import { promisify } from 'node:util';
import { join, win32 } from 'node:path';
import { gzipSync } from 'node:zlib';
import { randomUUID, createHash } from 'node:crypto';
import { updateFS, syncUpdateDirectory } from './update-fs.ts';
import { writeUpdateJournal, type UpdateJournal } from './update-journal.ts';
import type { InstalledUpdatePlan } from './update-plan.ts';
const execute = promisify(execFile);
const pause = (ms: number) => new Promise<void>(done => setTimeout(done, ms));
const NONCE = /^[a-f0-9]{64}$/;
const BRAND = '# Dilla authenticated NSIS actor v1\n';
const powerShell = () => win32.join(process.env.SystemRoot ?? 'C:\\Windows', 'System32/WindowsPowerShell/v1.0/powershell.exe');

// C# 5 / Windows PowerShell 5.1 compatible. JOB_LIST assigns the process to the
// non-breakaway job atomically during creation, before any installer thread runs.
// https://learn.microsoft.com/windows/win32/api/processthreadsapi/nf-processthreadsapi-updateprocthreadattribute
export const INSTALLER_JOB_CSHARP = String.raw`
using System;
using System.Text;
using System.ComponentModel;
using System.Runtime.InteropServices;
using System.Threading;
using System.Diagnostics;
public static class DillaInstallerKernel {
 [StructLayout(LayoutKind.Sequential)] public struct PROCESS_INFORMATION { public IntPtr hProcess,hThread; public uint dwProcessId,dwThreadId; }
 [StructLayout(LayoutKind.Sequential, CharSet=CharSet.Unicode)] public struct STARTUPINFO { public int cb; public string lpReserved,lpDesktop,lpTitle; public uint dwX,dwY,dwXSize,dwYSize,dwXCountChars,dwYCountChars,dwFillAttribute,dwFlags; public ushort wShowWindow,cbReserved2; public IntPtr lpReserved2,hStdInput,hStdOutput,hStdError; }
 [StructLayout(LayoutKind.Sequential)] public struct STARTUPINFOEX { public STARTUPINFO StartupInfo; public IntPtr lpAttributeList; }
 [StructLayout(LayoutKind.Sequential)] public struct BASIC_LIMIT { public long PerProcessUserTimeLimit,PerJobUserTimeLimit; public uint LimitFlags; public UIntPtr MinimumWorkingSetSize,MaximumWorkingSetSize; public uint ActiveProcessLimit; public UIntPtr Affinity; public uint PriorityClass,SchedulingClass; }
 [StructLayout(LayoutKind.Sequential)] public struct IO_COUNTERS { public ulong ReadOperationCount,WriteOperationCount,OtherOperationCount,ReadTransferCount,WriteTransferCount,OtherTransferCount; }
 [StructLayout(LayoutKind.Sequential)] public struct EXTENDED_LIMIT { public BASIC_LIMIT BasicLimitInformation; public IO_COUNTERS IoInfo; public UIntPtr ProcessMemoryLimit,JobMemoryLimit,PeakProcessMemoryUsed,PeakJobMemoryUsed; }
 [StructLayout(LayoutKind.Sequential)] public struct ACCOUNTING { public long TotalUserTime,TotalKernelTime,ThisPeriodTotalUserTime,ThisPeriodTotalKernelTime; public uint TotalPageFaultCount,TotalProcesses,ActiveProcesses,TotalTerminatedProcesses; }
 [DllImport("kernel32.dll",CharSet=CharSet.Unicode,SetLastError=true)] static extern IntPtr CreateJobObject(IntPtr attributes,string name);
 [DllImport("kernel32.dll",CharSet=CharSet.Unicode,SetLastError=true)] public static extern IntPtr OpenJobObject(uint access,bool inherit,string name);
 [DllImport("kernel32.dll",SetLastError=true)] static extern bool SetInformationJobObject(IntPtr job,int kind,ref EXTENDED_LIMIT info,uint length);
 [DllImport("kernel32.dll",SetLastError=true)] static extern bool QueryInformationJobObject(IntPtr job,int kind,out ACCOUNTING info,uint length,IntPtr returned);
 [DllImport("kernel32.dll",SetLastError=true)] public static extern bool TerminateJobObject(IntPtr job,uint code);
 [DllImport("kernel32.dll",SetLastError=true)] static extern bool InitializeProcThreadAttributeList(IntPtr list,int count,int flags,ref IntPtr size);
 [DllImport("kernel32.dll",SetLastError=true)] static extern bool UpdateProcThreadAttribute(IntPtr list,uint flags,UIntPtr attribute,IntPtr value,UIntPtr size,IntPtr previous,IntPtr returned);
 [DllImport("kernel32.dll")] static extern void DeleteProcThreadAttributeList(IntPtr list);
 [DllImport("kernel32.dll",CharSet=CharSet.Unicode,SetLastError=true)] static extern bool CreateProcess(string application,StringBuilder command,IntPtr processAttributes,IntPtr threadAttributes,bool inherit,uint flags,IntPtr environment,string directory,ref STARTUPINFOEX startup,out PROCESS_INFORMATION process);
 [DllImport("kernel32.dll",SetLastError=true)] public static extern IntPtr OpenProcess(uint access,bool inherit,uint pid);
 [DllImport("kernel32.dll",SetLastError=true)] public static extern bool TerminateProcess(IntPtr process,uint code);
 [DllImport("kernel32.dll",CharSet=CharSet.Unicode,SetLastError=true)] static extern bool QueryFullProcessImageName(IntPtr process,uint flags,StringBuilder name,ref uint size);
 [DllImport("kernel32.dll",SetLastError=true)] public static extern uint WaitForSingleObject(IntPtr handle,uint milliseconds);
 [DllImport("kernel32.dll",SetLastError=true)] static extern uint WaitForMultipleObjects(uint count,IntPtr[] handles,bool all,uint milliseconds);
 [DllImport("kernel32.dll",SetLastError=true)] static extern bool GetExitCodeProcess(IntPtr process,out uint code);
 [DllImport("kernel32.dll",SetLastError=true)] static extern uint ResumeThread(IntPtr thread);
 [DllImport("kernel32.dll",SetLastError=true)] public static extern bool CloseHandle(IntPtr handle);
 static void Check(bool ok) { if(!ok) throw new Win32Exception(Marshal.GetLastWin32Error()); }
 public static IntPtr CreateJob(string name) {
  IntPtr job=CreateJobObject(IntPtr.Zero,name); int error=Marshal.GetLastWin32Error();
  if(job==IntPtr.Zero) throw new Win32Exception(error);
  if(error==183) { CloseHandle(job); throw new InvalidOperationException("Update job already exists"); }
  try { EXTENDED_LIMIT limits=new EXTENDED_LIMIT(); limits.BasicLimitInformation.LimitFlags=0x2000; Check(SetInformationJobObject(job,9,ref limits,(uint)Marshal.SizeOf(typeof(EXTENDED_LIMIT)))); return job; }
  catch { CloseHandle(job); throw; }
 }
 public static PROCESS_INFORMATION CreateSuspended(IntPtr job,string executable,string directory) {
  IntPtr size=IntPtr.Zero,list=IntPtr.Zero,jobs=IntPtr.Zero; bool initialized=false;
  try {
   InitializeProcThreadAttributeList(IntPtr.Zero,1,0,ref size);
   if(size==IntPtr.Zero) throw new Win32Exception(Marshal.GetLastWin32Error());
   list=Marshal.AllocHGlobal(size); Check(InitializeProcThreadAttributeList(list,1,0,ref size)); initialized=true;
   jobs=Marshal.AllocHGlobal(IntPtr.Size); Marshal.WriteIntPtr(jobs,job);
   Check(UpdateProcThreadAttribute(list,0,new UIntPtr(0x2000D),jobs,new UIntPtr((uint)IntPtr.Size),IntPtr.Zero,IntPtr.Zero));
   STARTUPINFOEX startup=new STARTUPINFOEX(); startup.StartupInfo.cb=Marshal.SizeOf(typeof(STARTUPINFOEX)); startup.lpAttributeList=list;
   PROCESS_INFORMATION child;
   // No ShellExecute/elevation, inherited handles, breakaway, or force-run.
   Check(CreateProcess(executable,new StringBuilder("\""+executable+"\" /S --updated"),IntPtr.Zero,IntPtr.Zero,false,0x08080004,IntPtr.Zero,directory,ref startup,out child));
   return child;
  } finally { if(initialized) DeleteProcThreadAttributeList(list); if(jobs!=IntPtr.Zero) Marshal.FreeHGlobal(jobs); if(list!=IntPtr.Zero) Marshal.FreeHGlobal(list); }
 }
 public static void Resume(PROCESS_INFORMATION child) { if(ResumeThread(child.hThread)==0xFFFFFFFF) throw new Win32Exception(Marshal.GetLastWin32Error()); }
 public static uint WaitInstaller(IntPtr parent,IntPtr child) {
  uint result=WaitForMultipleObjects(2,new IntPtr[]{parent,child},false,300000);
  if(result==0) throw new InvalidOperationException("Update helper exited");
  if(result==258) throw new TimeoutException("Installer exceeded five minutes");
  if(result!=1) throw new Win32Exception(Marshal.GetLastWin32Error());
  uint code; Check(GetExitCodeProcess(child,out code)); return code;
 }
 public static string Image(IntPtr process) { uint size=32768; StringBuilder name=new StringBuilder((int)size); Check(QueryFullProcessImageName(process,0,name,ref size)); return name.ToString(); }
 public static void DrainJob(IntPtr job) {
  Check(TerminateJobObject(job,125)); Stopwatch elapsed=Stopwatch.StartNew();
  for(;;) { ACCOUNTING info; Check(QueryInformationJobObject(job,1,out info,(uint)Marshal.SizeOf(typeof(ACCOUNTING)),IntPtr.Zero)); if(info.ActiveProcesses==0) return; if(elapsed.ElapsedMilliseconds>=30000) throw new TimeoutException("Installer job did not stop"); Thread.Sleep(25); }
 }
}
`;

function nativeTypeScript(): string {
 const compressed = gzipSync(Buffer.from(INSTALLER_JOB_CSHARP)).toString('base64');
 return `$compressed=[Convert]::FromBase64String('${compressed}');$memory=[IO.MemoryStream]::new($compressed);$gzip=[IO.Compression.GZipStream]::new($memory,[IO.Compression.CompressionMode]::Decompress);$reader=[IO.StreamReader]::new($gzip,[Text.Encoding]::UTF8);try{$native=$reader.ReadToEnd()}finally{$reader.Dispose();$gzip.Dispose();$memory.Dispose()};Add-Type -TypeDefinition $native\n`;
}
function config(plan: InstalledUpdatePlan, parentPid: number) {
 if (plan.platform !== 'win32' || !NONCE.test(plan.health.nonce) || !Number.isSafeInteger(parentPid) || parentPid <= 1) throw new Error('Invalid installer actor');
 const artifact = plan.release.artifacts.find(file => file.platform === 'win32' && file.arch === plan.arch);
 if (!artifact) throw new Error('Missing authenticated installer artifact');
 return { nonce: plan.health.nonce, version: plan.oldVersion, directory: plan.health.directory, artifact: plan.artifactFile, size: artifact.size, sha512: artifact.sha512, parentPid, parentImage: plan.watchdog,
  parentMarker: `--update-watchdog=${plan.health.nonce}`, job: `Local\\DillaUpdate-${plan.health.nonce}` };
}
const data = (value: unknown) => Buffer.from(JSON.stringify(value)).toString('base64');
const encoded = (script: string) => Buffer.from(script, 'utf16le').toString('base64');
export function installerActorCommand(plan: InstalledUpdatePlan, parentPid: number): string {
 const payload = data(config(plan, parentPid));
 return encoded(BRAND + `$ErrorActionPreference='Stop'\n$c=[Text.Encoding]::UTF8.GetString([Convert]::FromBase64String('${payload}'))|ConvertFrom-Json\n` + nativeTypeScript() + String.raw`
$parent=[IntPtr]::Zero;$job=[IntPtr]::Zero;$child=$null;$artifact=$null;$code=125
try {
 $parent=[DillaInstallerKernel]::OpenProcess(0x100000,$false,[uint32]$c.parentPid)
 if($parent -eq [IntPtr]::Zero){throw 'Cannot hold update helper handle'}
 $owner=Get-CimInstance Win32_Process -Filter ('ProcessId = '+$c.parentPid)
 if(!$owner -or $owner.ExecutablePath -ine $c.parentImage -or !($owner.CommandLine -match ('(?:^|\s)'+[Regex]::Escape($c.parentMarker)+'(?:\s|$)')) -or [DillaInstallerKernel]::WaitForSingleObject($parent,0) -ne 258){throw 'Unexpected update helper'}
 # Hold a read-only, non-write/non-delete shared file while hashing/executing.
 $artifact=[IO.File]::Open($c.artifact,[IO.FileMode]::Open,[IO.FileAccess]::Read,[IO.FileShare]::Read)
 if($artifact.Length -ne $c.size){throw 'Installer size changed'}
 $hash=[Security.Cryptography.SHA512]::Create();try{$digest=[Convert]::ToBase64String($hash.ComputeHash($artifact))}finally{$hash.Dispose()}
 if($digest -cne $c.sha512 -or [DillaInstallerKernel]::WaitForSingleObject($parent,0) -ne 258){throw 'Installer verification failed'}
 $job=[DillaInstallerKernel]::CreateJob($c.job)
 $child=[DillaInstallerKernel]::CreateSuspended($job,$c.artifact,$c.directory)
 $ready=Join-Path $c.directory 'installer-ready.json';$temporary=$ready+'.tmp'
 $bytes=[Text.Encoding]::UTF8.GetBytes((@{format=1;nonce=$c.nonce;version=$c.version;pid=$PID;installerPid=$child.dwProcessId}|ConvertTo-Json -Compress))
 $file=[IO.File]::Open($temporary,[IO.FileMode]::CreateNew,[IO.FileAccess]::Write,[IO.FileShare]::None)
 try{$file.Write($bytes,0,$bytes.Length);$file.Flush($true)}finally{$file.Dispose()}
 [IO.File]::Move($temporary,$ready)
 $go=Join-Path $c.directory 'installer-go.json';$deadline=[Diagnostics.Stopwatch]::StartNew()
 for(;;){
  if([DillaInstallerKernel]::WaitForSingleObject($parent,0) -ne 258){throw 'Update helper exited before go'}
  if([IO.File]::Exists($go)){
   if(([IO.File]::GetAttributes($go) -band [IO.FileAttributes]::ReparsePoint) -ne 0 -or (Get-Item -LiteralPath $go).Length -gt 1024){throw 'Invalid installer go'}
   $grant=[IO.File]::ReadAllText($go)|ConvertFrom-Json
   if(@($grant.PSObject.Properties).Count -ne 4 -or $grant.format -ne 1 -or $grant.nonce -cne $c.nonce -or $grant.pid -ne $PID -or $grant.installerPid -ne $child.dwProcessId){throw 'Invalid installer go'}
   break
  }
  if($deadline.ElapsedMilliseconds -ge 60000){throw 'Installer registration timed out'}
  Start-Sleep -Milliseconds 25
 }
 [DillaInstallerKernel]::Resume($child)
 $code=[DillaInstallerKernel]::WaitInstaller($parent,$child.hProcess)
} finally {
 try{if($job -ne [IntPtr]::Zero){[DillaInstallerKernel]::DrainJob($job)}}finally{
  if($null -ne $child){[void][DillaInstallerKernel]::CloseHandle($child.hThread);[void][DillaInstallerKernel]::CloseHandle($child.hProcess)}
  if($job -ne [IntPtr]::Zero){[void][DillaInstallerKernel]::CloseHandle($job)}
  if($parent -ne [IntPtr]::Zero){[void][DillaInstallerKernel]::CloseHandle($parent)}
  if($null -ne $artifact){$artifact.Dispose()}
 }
}
exit $code
`);
}
export function installerActorParent(plan: InstalledUpdatePlan, command: string): number | null {
 if (!/^[A-Za-z0-9+/=]+$/.test(command) || command.length > 100_000) return null;
 const script = Buffer.from(command, 'base64').toString('utf16le');
 if (!script.startsWith(BRAND)) return null;
 const match = /FromBase64String\('([A-Za-z0-9+/=]+)'\)/.exec(script);
 try {
  const parent = match ? JSON.parse(Buffer.from(match[1], 'base64').toString()).parentPid : null;
  return Number.isSafeInteger(parent) && parent > 1 && installerActorCommand(plan, parent) === command ? parent : null;
 } catch { return null; }
}

export interface InstallerActorHost {
 spawn(command: string): Promise<ChildProcess>;
 stop(child: ChildProcess): Promise<void>;
 timeoutMs?: number;
}
async function privateRecord(plan: InstalledUpdatePlan, name: 'installer-ready.json'): Promise<{ pid: number; installerPid: number } | null> {
 const { lstat, realpath, open } = updateFS.promises, directory = plan.health.directory;
 const info = await lstat(directory);
 if (!info.isDirectory() || info.isSymbolicLink() || await realpath(directory) !== directory
  || (process.platform !== 'win32' && ((info.mode & 0o077) !== 0 || info.uid !== process.getuid!()))) throw new Error('Invalid installer actor directory');
 const file = join(directory, name);
 try {
  const entry = await lstat(file);
  if (!entry.isFile() || entry.isSymbolicLink()) throw new Error('Invalid installer actor report');
  const handle = await open(file, updateFS.constants.O_RDONLY | (updateFS.constants.O_NOFOLLOW ?? 0));
  try {
   const stat = await handle.stat();
   if (!stat.isFile() || stat.size > 1024 || (process.platform !== 'win32' && ((stat.mode & 0o077) !== 0 || stat.uid !== process.getuid!()))) throw new Error('Invalid installer actor report');
   const buffer = Buffer.alloc(1025), { bytesRead } = await handle.read(buffer, 0, buffer.length, 0);
   if (bytesRead > 1024) throw new Error('Invalid installer actor report');
   const value = JSON.parse(buffer.subarray(0, bytesRead).toString());
   if (!value || Object.keys(value).length !== 5 || value.format !== 1 || value.nonce !== plan.health.nonce || value.version !== plan.oldVersion
    || !Number.isSafeInteger(value.pid) || value.pid <= 1 || !Number.isSafeInteger(value.installerPid) || value.installerPid <= 1 || value.installerPid === value.pid) throw new Error('Invalid installer actor report');
   return value;
  } finally { await handle.close(); }
 } catch (error) { if ((error as NodeJS.ErrnoException).code === 'ENOENT') return null; throw error; }
}
async function grantInstaller(plan: InstalledUpdatePlan, pid: number, installerPid: number): Promise<void> {
 const file = join(plan.health.directory, 'installer-go.json'), temporary = file + '.' + randomUUID();
 let created = false;
 try {
  const handle = await updateFS.promises.open(temporary, 'wx', 0o600); created = true;
  try { await handle.writeFile(JSON.stringify({ format: 1, nonce: plan.health.nonce, pid, installerPid })); await handle.sync(); } finally { await handle.close(); }
  await updateFS.promises.rename(temporary, file); await syncUpdateDirectory(plan.health.directory);
 } finally { if (created) await updateFS.promises.rm(temporary, { force: true }); }
}
async function waitExit(child: ChildProcess, timeoutMs: number): Promise<number | null> {
 if (child.exitCode !== null || child.signalCode !== null) return child.exitCode;
 return new Promise((done, reject) => {
  const timer = setTimeout(() => { cleanup(); reject(new Error('Windows installer actor timed out')); }, timeoutMs);
  const exited = (code: number | null) => { cleanup(); done(code); };
  const failed = (error: Error) => { cleanup(); reject(error); };
  const cleanup = () => { clearTimeout(timer); child.removeListener('exit', exited); child.removeListener('error', failed); };
  child.once('exit', exited); child.once('error', failed);
 });
}

/** The actor has created a suspended, contained NSIS process before readiness.
 * No primary installer thread resumes until both actual PIDs are durable. */
export async function runWindowsInstaller(plan: InstalledUpdatePlan, host: InstallerActorHost = nativeActorHost(plan)): Promise<void> {
 if (plan.platform !== 'win32') throw new Error('Invalid Windows installer target');
 await writeUpdateJournal(plan, { phase: 'installing', watchdogPid: process.pid });
 const actor = await host.spawn(installerActorCommand(plan, process.pid));
 try {
  if (!actor.pid || actor.pid <= 1) throw new Error('Invalid installer actor process');
  const deadline = Date.now() + (host.timeoutMs ?? 60_000);
  let report;
  for (;;) {
   if (actor.exitCode !== null || actor.signalCode !== null) throw new Error('The Windows installer actor exited before readiness');
   report = await privateRecord(plan, 'installer-ready.json');
   if (report) { if (report.pid !== actor.pid) throw new Error('Unexpected Windows installer actor'); break; }
   if (Date.now() >= deadline) throw new Error('Windows installer actor readiness timed out');
   await pause(25);
  }
  await writeUpdateJournal(plan, { phase: 'installing', watchdogPid: process.pid, installerActorPid: actor.pid, installerPid: report.installerPid });
  await grantInstaller(plan, actor.pid, report.installerPid);
  const code = await waitExit(actor, host.timeoutMs ?? 330_000);
  if (code !== 0) throw new Error(`The Windows installer failed (${code ?? 'signal'}); the working version will be restored`);
 } catch (error) { await host.stop(actor); throw error; }
}

interface ActorIdentity { pid: number; command: string }
function commandArgument(commandLine: string): string | null {
 const match = /(?:^|\s)-EncodedCommand\s+"?([A-Za-z0-9+/=]+)"?(?:\s|$)/i.exec(commandLine);
 return match?.[1] ?? null;
}
async function actorIdentities(plan: InstalledUpdatePlan): Promise<ActorIdentity[]> {
 const image = Buffer.from(powerShell()).toString('base64'), prefix = encoded(BRAND).slice(0, 40);
 const script = `$image=[Text.Encoding]::UTF8.GetString([Convert]::FromBase64String('${image}'));ConvertTo-Json -Compress -InputObject @(Get-CimInstance Win32_Process | Where-Object {$_.ExecutablePath -ieq $image -and $_.CommandLine.Contains('${prefix}')} | ForEach-Object {@{pid=$_.ProcessId;command=$_.CommandLine}})`;
 const output = await execute(powerShell(), ['-NoProfile', '-NonInteractive', '-Command', script], { encoding: 'utf8', windowsHide: true, timeout: 30_000, maxBuffer: 2 * 1024 * 1024 });
 const rows: unknown = JSON.parse(output.stdout || '[]');
 if (!Array.isArray(rows)) throw new Error('Invalid installer actor discovery');
 const found: ActorIdentity[] = [];
 for (const row of rows) {
  if (!row || !Number.isSafeInteger(row.pid) || row.pid <= 1 || typeof row.command !== 'string') throw new Error('Invalid installer actor discovery');
  const argument = commandArgument(row.command);
  if (argument && installerActorParent(plan, argument) !== null) found.push({ pid: row.pid, command: argument });
 }
 return found;
}
function controlCommand(plan: InstalledUpdatePlan, actors: ActorIdentity[]): string {
 const payload = data({ job: `Local\\DillaUpdate-${plan.health.nonce}`, artifact: plan.artifactFile, image: powerShell(), actors: actors.map(actor => ({ pid: actor.pid, hash: createHash('sha256').update(actor.command).digest('hex') })) });
 return encoded(`# Dilla authenticated NSIS recovery v1\n$ErrorActionPreference='Stop'\n$c=[Text.Encoding]::UTF8.GetString([Convert]::FromBase64String('${payload}'))|ConvertFrom-Json\n` + nativeTypeScript() + String.raw`
$job=[DillaInstallerKernel]::OpenJobObject(0x0C,$false,$c.job)
if($job -eq [IntPtr]::Zero){$errorCode=[Runtime.InteropServices.Marshal]::GetLastWin32Error();if($errorCode -ne 2){throw 'Cannot inspect installer job'}}
try {
 if($job -ne [IntPtr]::Zero){[DillaInstallerKernel]::DrainJob($job)}
 foreach($actor in $c.actors){
  $handle=[DillaInstallerKernel]::OpenProcess(0x101001,$false,[uint32]$actor.pid)
  if($handle -eq [IntPtr]::Zero){$errorCode=[Runtime.InteropServices.Marshal]::GetLastWin32Error();if($errorCode -eq 87){continue};throw 'Cannot hold installer actor handle'}
  try{
   if([DillaInstallerKernel]::WaitForSingleObject($handle,0) -ne 258){continue}
   $actual=Get-CimInstance Win32_Process -Filter ('ProcessId = '+$actor.pid)
   if(!$actual -or [DillaInstallerKernel]::Image($handle) -ine $c.image -or !($actual.CommandLine -match '(?:^|\s)-EncodedCommand\s+"?([A-Za-z0-9+/=]+)"?(?:\s|$)') -or [DillaInstallerKernel]::WaitForSingleObject($handle,0) -ne 258){throw 'Installer actor identity changed'}
   $hash=[Security.Cryptography.SHA256]::Create();try{$actualHash=[BitConverter]::ToString($hash.ComputeHash([Text.Encoding]::ASCII.GetBytes($Matches[1]))).Replace('-','').ToLowerInvariant()}finally{$hash.Dispose()}
   if($actualHash -cne $actor.hash){throw 'Installer actor command changed'}
   if(![DillaInstallerKernel]::TerminateProcess($handle,125)){throw 'Cannot stop owned installer actor'}
   if([DillaInstallerKernel]::WaitForSingleObject($handle,10000) -ne 0){throw 'Installer actor did not stop'}
  }finally{[void][DillaInstallerKernel]::CloseHandle($handle)}
 }
 # Creation is atomic with JOB_LIST. An unexpected process at this exact
 # private, nonce-bound artifact path is never permission to restore over it.
 $deadline=[Diagnostics.Stopwatch]::StartNew()
 for(;;){
  $writers=@(Get-CimInstance Win32_Process | Where-Object {$_.ExecutablePath -ieq $c.artifact})
  if($writers.Count -eq 0){break}
  if($deadline.ElapsedMilliseconds -ge 10000){throw 'An installer is still using the authenticated artifact'}
  Start-Sleep -Milliseconds 25
 }
}finally{if($job -ne [IntPtr]::Zero){[void][DillaInstallerKernel]::CloseHandle($job)}}
`);
}
/** Recovery always checks the private named job and exact fixed actor identity,
 * including the spawn-to-journal gap. Missing PID fields never imply no writer. */
export async function stopWindowsInstaller(plan: InstalledUpdatePlan, _journal?: UpdateJournal): Promise<void> {
 if (plan.platform !== 'win32' || process.platform !== 'win32') return;
 const actors = await actorIdentities(plan);
 await execute(powerShell(), ['-NoProfile', '-NonInteractive', '-EncodedCommand', controlCommand(plan, actors)], { windowsHide: true, timeout: 60_000, maxBuffer: 128 * 1024 });
}
function nativeActorHost(plan: InstalledUpdatePlan): InstallerActorHost {
 return {
  spawn: async command => {
   if (process.platform !== 'win32') throw new Error('Windows installer requires its native OS');
   const environment = { ...process.env };
   delete environment.DILLA_UPDATE_STARTUP_NONCE; delete environment.DILLA_UPDATE_PREVIOUS_PID; delete environment.DILLA_UPDATE_RECOVERED_NONCE;
   const child = spawn(powerShell(), ['-NoProfile', '-NonInteractive', '-EncodedCommand', command], { cwd: plan.health.directory, stdio: 'ignore', windowsHide: true, env: environment });
   await new Promise<void>((done, reject) => { child.once('spawn', done); child.once('error', reject); });
   return child;
  },
  stop: async child => { await stopWindowsInstaller(plan); await waitExit(child, 10_000); },
 };
}
