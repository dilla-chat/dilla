import { win32 } from 'node:path';
import { execFileSync } from 'node:child_process';

export function requireWindowsEntryCI(environment = process.env, platform = process.platform) {
 if (platform !== 'win32') throw new Error('OS entry acceptance requires Windows');
 if (environment.GITHUB_ACTIONS !== 'true' || environment.GITHUB_REPOSITORY !== 'jonasthim/dilla'
  || environment.RUNNER_ENVIRONMENT !== 'github-hosted' || !environment.RUNNER_TEMP) {
  throw new Error('Real dilla association acceptance requires a disposable github-hosted Windows CI user');
 }
}
export function assertProtocolVacant(state) {
 if (state.classes || state.machineClasses || state.userChoice) throw new Error('Refusing an existing dilla protocol association; no user mapping was changed');
}
export function assertOwnedProtocol(state, launcher) {
 if (!state.classes) return;
 if (typeof state.command !== 'string' || state.command.toLowerCase() !== (`"${launcher}" "%1"`).toLowerCase()) throw new Error('Protocol association is no longer owned by this fixture');
}
export const ENTRY_NATIVE_CSHARP = String.raw`
using System;
using System.Text;
using System.ComponentModel;
using System.Runtime.InteropServices;
public static class DillaEntryFixture {
 [StructLayout(LayoutKind.Sequential,CharSet=CharSet.Unicode)] public struct STARTUPINFO { public int cb;public string lpReserved,lpDesktop,lpTitle;public uint dwX,dwY,dwXSize,dwYSize,dwXCountChars,dwYCountChars,dwFillAttribute,dwFlags;public ushort wShowWindow,cbReserved2;public IntPtr lpReserved2,hStdInput,hStdOutput,hStdError; }
 [StructLayout(LayoutKind.Sequential)] public struct PROCESS_INFORMATION { public IntPtr hProcess,hThread;public uint dwProcessId,dwThreadId; }
 [DllImport("shell32.dll",CharSet=CharSet.Unicode,SetLastError=true)] static extern IntPtr CommandLineToArgvW(string command,out int argc);
 [DllImport("kernel32.dll")] static extern IntPtr LocalFree(IntPtr memory);
 [DllImport("kernel32.dll",CharSet=CharSet.Unicode,SetLastError=true)] static extern bool CreateProcessW(string application,StringBuilder command,IntPtr processAttributes,IntPtr threadAttributes,bool inherit,uint flags,IntPtr environment,string directory,ref STARTUPINFO startup,out PROCESS_INFORMATION process);
 [DllImport("kernel32.dll")] static extern bool CloseHandle(IntPtr handle);
 public static string[] Arguments(string command) {
  int count;IntPtr memory=CommandLineToArgvW(command,out count);if(memory==IntPtr.Zero)throw new Win32Exception(Marshal.GetLastWin32Error());
  try{if(count<1||count>16)throw new InvalidOperationException("Invalid registered command");string[] args=new string[count];for(int i=0;i<count;i++)args[i]=Marshal.PtrToStringUni(Marshal.ReadIntPtr(memory,i*IntPtr.Size));return args;}finally{LocalFree(memory);}
 }
 public static uint StartRegistered(string application,string command,string directory) {
  STARTUPINFO startup=new STARTUPINFO();startup.cb=Marshal.SizeOf(typeof(STARTUPINFO));PROCESS_INFORMATION child;
  if(!CreateProcessW(application,new StringBuilder(command),IntPtr.Zero,IntPtr.Zero,false,0,IntPtr.Zero,directory,ref startup,out child))throw new Win32Exception(Marshal.GetLastWin32Error());
  CloseHandle(child.hThread);CloseHandle(child.hProcess);return child.dwProcessId;
 }
}
`;
function context(value) {
 if (!value || !/^[a-f0-9]{12}$/.test(value.id) || value.product !== 'DillaUpdateAcceptance' + value.id
  || !win32.isAbsolute(value.programRoot) || !win32.isAbsolute(value.profile)
  || win32.basename(value.programRoot) !== value.product || win32.basename(win32.dirname(value.programRoot)) !== 'Programs'
  || win32.basename(value.profile) !== value.product || win32.normalize(value.programRoot).toLowerCase() === win32.normalize(value.profile).toLowerCase()) throw new Error('Invalid CI-owned Windows fixture context');
 return value;
}
export function windowsEntryScript(action, input) {
 const c = context(input);
 if (!['preflight','inspect','shortcut','protocol','autostart','cleanup'].includes(action)) throw new Error('Invalid OS entry action');
 const encoded = Buffer.from(JSON.stringify(c)).toString('base64');
 const header = `$ErrorActionPreference='Stop'
$c=[Text.Encoding]::UTF8.GetString([Convert]::FromBase64String('${encoded}'))|ConvertFrom-Json
if($c.product -cnotmatch '^DillaUpdateAcceptance[a-f0-9]{12}$'){throw 'Invalid fixture identity'}
$expectedRoot=[IO.Path]::Combine([Environment]::GetFolderPath('LocalApplicationData'),'Programs',$c.product)
$expectedProfile=[IO.Path]::Combine([Environment]::GetFolderPath('ApplicationData'),$c.product)
if(![String]::Equals([IO.Path]::GetFullPath($c.programRoot),$expectedRoot,[StringComparison]::OrdinalIgnoreCase) -or ![String]::Equals([IO.Path]::GetFullPath($c.profile),$expectedProfile,[StringComparison]::OrdinalIgnoreCase)){throw 'Fixture paths differ from Windows known folders'}
$launcher=[IO.Path]::Combine($expectedRoot,'Dilla.exe')
$classes='Software\\Classes\\dilla'
$userChoice='Software\\Microsoft\\Windows\\Shell\\Associations\\UrlAssociations\\dilla\\UserChoice'
$run='Software\\Microsoft\\Windows\\CurrentVersion\\Run'
function ProtocolState {
 $key=[Microsoft.Win32.Registry]::CurrentUser.OpenSubKey($classes,$false)
 $machine=[Microsoft.Win32.Registry]::LocalMachine.OpenSubKey($classes,$false)
 $choice=[Microsoft.Win32.Registry]::CurrentUser.OpenSubKey($userChoice,$false)
 try{$command=$null;if($key){$sub=$key.OpenSubKey('shell\\open\\command',$false);try{if($sub){$command=$sub.GetValue('',$null,[Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)}}finally{if($sub){$sub.Dispose()}}};return @{classes=($null -ne $key);machineClasses=($null -ne $machine);userChoice=($null -ne $choice);command=$command}}
 finally{if($key){$key.Dispose()};if($machine){$machine.Dispose()};if($choice){$choice.Dispose()}}
}
function RequireOwnedProtocol {
 $state=ProtocolState
 if(!$state.classes -or ![String]::Equals($state.command,('"'+$launcher+'" "%1"'),[StringComparison]::OrdinalIgnoreCase)){throw 'Protocol is not owned by this fixture'}
 return $state
}
`;
 if (action === 'preflight') return header + 'ProtocolState|ConvertTo-Json -Compress\n';
 if (action === 'cleanup') return header + `$state=ProtocolState
if($state.classes){$null=RequireOwnedProtocol;[Microsoft.Win32.Registry]::CurrentUser.DeleteSubKeyTree($classes,$false)}
ProtocolState|ConvertTo-Json -Compress
`;
 const shortcut = `$shortcut=[IO.Path]::Combine([Environment]::GetFolderPath('Programs'),$c.product+'.lnk')
if(!(Test-Path -LiteralPath $shortcut -PathType Leaf)){throw 'Installed shortcut missing'}
$shell=New-Object -ComObject WScript.Shell
$link=$shell.CreateShortcut($shortcut)
if(![String]::Equals($link.TargetPath,$launcher,[StringComparison]::OrdinalIgnoreCase) -or $link.Arguments -ne ''){throw 'Shortcut targets a mutable or unknown program'}
`;
 const native = `Add-Type -TypeDefinition @'\n${ENTRY_NATIVE_CSHARP}\n'@\n`;
 const autostart = `$key=[Microsoft.Win32.Registry]::CurrentUser.OpenSubKey($run,$false)
try{if(!$key -or $key.GetValueKind($c.product) -ne [Microsoft.Win32.RegistryValueKind]::String){throw 'Owned startup command missing'};$command=$key.GetValue($c.product,$null,[Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)}finally{if($key){$key.Dispose()}}
$registeredArguments=[DillaEntryFixture]::Arguments($command)
if($registeredArguments.Length -ne 2 -or ![String]::Equals([IO.Path]::GetFullPath($registeredArguments[0]),$launcher,[StringComparison]::OrdinalIgnoreCase) -or $registeredArguments[1] -cne '--hidden'){throw 'Unexpected registered startup command'}
`;
 if (action === 'inspect') return header + native + shortcut + autostart + `$protocol=RequireOwnedProtocol
@{shortcut=$shortcut;shortcutTarget=$link.TargetPath;shortcutArguments=$link.Arguments;protocol=$protocol;startupCommand=$command;launcher=$launcher;profile=$expectedProfile}|ConvertTo-Json -Compress -Depth 5
`;
 if (action === 'shortcut') return header + shortcut + `$start=New-Object System.Diagnostics.ProcessStartInfo
$start.FileName=$shortcut;$start.UseShellExecute = $true
$child=[System.Diagnostics.Process]::Start($start)
@{dispatch='shortcut';shortcut=$shortcut;useShellExecute=$true;launcherPid=$child.Id}|ConvertTo-Json -Compress
`;
 if (action === 'protocol') return header + `$null=RequireOwnedProtocol
$uri='dilla://invite/entry-'+$c.id+'.invalid/decline_'+$c.id
$start=New-Object System.Diagnostics.ProcessStartInfo
$start.FileName=$uri;$start.UseShellExecute = $true
$child=[System.Diagnostics.Process]::Start($start)
@{dispatch='protocol';uri=$uri;useShellExecute=$true;launcherPid=$child.Id}|ConvertTo-Json -Compress
`;
 return header + native + autostart + `$startedPID=[DillaEntryFixture]::StartRegistered($launcher, $command, $expectedRoot)
@{dispatch='autostart-command';command=$command;launcherPid=$startedPID;logonSimulated=$false}|ConvertTo-Json -Compress
`;
}
export function runWindowsEntry(action, input) {
 requireWindowsEntryCI();
 const script = windowsEntryScript(action, input);
 const powershell = win32.join(process.env.SystemRoot ?? 'C:\\Windows', 'System32/WindowsPowerShell/v1.0/powershell.exe');
 const output = execFileSync(powershell, ['-NoProfile','-NonInteractive','-EncodedCommand',Buffer.from(script,'utf16le').toString('base64')], {encoding:'utf8',windowsHide:true,timeout:30000,maxBuffer:65536});
 return JSON.parse(output.trim());
}
