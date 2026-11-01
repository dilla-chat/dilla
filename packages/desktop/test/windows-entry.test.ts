import { test } from 'node:test';
import assert from 'node:assert/strict';
// @ts-expect-error native fixture orchestration
import { requireWindowsEntryCI, windowsEntryScript, assertProtocolVacant, assertOwnedProtocol, ENTRY_NATIVE_CSHARP } from './support/windows-entry.mjs';
const id='abcdef012345',product='DillaUpdateAcceptance'+id;
const context={id,product,programRoot:'C:\\Users\\runner\\AppData\\Local\\Programs\\'+product,profile:'C:\\Users\\runner\\AppData\\Roaming\\'+product};
test('real dilla association acceptance refuses local users and every preexisting or changed protocol mapping',()=>{
 const environment={GITHUB_ACTIONS:'true',GITHUB_REPOSITORY:'jonasthim/dilla',RUNNER_ENVIRONMENT:'github-hosted',RUNNER_TEMP:'C:\\runner-temp'};
 assert.doesNotThrow(()=>requireWindowsEntryCI(environment,'win32'));
 for(const changed of [{},{...environment,GITHUB_ACTIONS:'false'},{...environment,RUNNER_ENVIRONMENT:'self-hosted'}])assert.throws(()=>requireWindowsEntryCI(changed,'win32'),/disposable|CI/);
 assert.throws(()=>requireWindowsEntryCI(environment,'linux'),/Windows/);
 assertProtocolVacant({classes:false,userChoice:false});
 for(const state of [{classes:true,userChoice:false},{classes:false,userChoice:true},{classes:false,machineClasses:true,userChoice:false}])assert.throws(()=>assertProtocolVacant(state),/existing/);
 const command='"'+context.programRoot+'\\Dilla.exe" "%1"';assertOwnedProtocol({classes:true,command},context.programRoot+'\\Dilla.exe');
 assert.throws(()=>assertOwnedProtocol({classes:true,command:'"C:\\other\\Dilla.exe" "%1"'},context.programRoot+'\\Dilla.exe'),/owned/);
});
test('OS entry scripts use real shell dispatch and execute only the literal validated owned Run command',()=>{
 for(const action of ['preflight','inspect','shortcut','protocol','autostart','cleanup']){
  const source=windowsEntryScript(action,context);assert.match(source,/DillaUpdateAcceptance/);assert.doesNotMatch(source,/ExecutionPolicy|taskkill|cmd\.exe|ShellExecute\s*\(|\$PID\s*=/i);
  if(action==='shortcut')assert.match(source,/UseShellExecute = \$true/);
  if(action==='protocol'){assert.match(source,/UseShellExecute = \$true/);assert.match(source,/dilla:\/\/invite\/entry-/);}
  if(action==='autostart'){assert.match(source,/GetValue\(\$c.product/);assert.match(source,/StartRegistered\(\$launcher, \$command/);assert.match(source,/--hidden/);}
 }
 assert.throws(()=>windowsEntryScript('delete-everything',context),/action/);
 assert.throws(()=>windowsEntryScript('shortcut',{...context,product:'Dilla'}),/fixture/);
 assert.match(ENTRY_NATIVE_CSHARP,/CommandLineToArgvW/);assert.match(ENTRY_NATIVE_CSHARP,/CreateProcessW/);assert.doesNotMatch(ENTRY_NATIVE_CSHARP,/ShellExecute|CREATE_BREAKAWAY|runas/);
});
