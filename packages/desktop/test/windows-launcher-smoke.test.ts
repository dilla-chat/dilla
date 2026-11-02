import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFile, access } from 'node:fs/promises';
import { dirname } from 'node:path';
// @ts-expect-error build tooling is JavaScript
import { runWindowsLauncherSmoke } from '../scripts/windows-launcher-smoke.mjs';

test('smoke converts the production PNG to an ICO and compiles the real launcher config, then cleans up', async () => {
 let path='';
 await runWindowsLauncherSmoke({platform:'win32',compileLauncher:async ({file,config,iconFile}: {file:string,config:any,iconFile:string})=>{
  path=dirname(file);
  const bytes=await readFile(iconFile);assert.deepEqual([...bytes.subarray(0,4)],[0,0,1,0]);
  assert.equal(config.arch,'x64');assert.equal(config.initialVersion,'1.0.0');assert.equal(config.productName,'DillaSmoke');assert.match(config.publicKeys[0],/^-----BEGIN PUBLIC KEY-----/);
 }});
 await assert.rejects(access(path));
});

test('smoke preserves compiler failure and cleans temporary output; other platforms cannot invoke it', async () => {
 const failure=new Error('link failed');let path='';
 await assert.rejects(runWindowsLauncherSmoke({platform:'win32',compileLauncher:async ({file}: {file:string})=>{path=dirname(file);throw failure;}}),(error:unknown)=>error===failure);
 await assert.rejects(access(path));
 await assert.rejects(runWindowsLauncherSmoke({platform:'linux'}),/native Windows/);
});
