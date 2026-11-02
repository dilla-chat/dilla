import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFile, access } from 'node:fs/promises';
import { dirname } from 'node:path';
// @ts-expect-error build tooling is JavaScript
import { runWindowsLauncherSmoke } from '../scripts/windows-launcher-smoke.mjs';

test('smoke converts the production PNG to an ICO and compiles the real launcher config, then cleans up', async () => {
 let path='';
 await runWindowsLauncherSmoke({platform:'win32',compileLauncher:async ({file,config,iconFile,onCoffLinkFailure}: {file:string,config:any,iconFile:string,onCoffLinkFailure:unknown})=>{
  path=dirname(file);
  assert.equal(onCoffLinkFailure,undefined);
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

test('capture is opt-in, fixed-path and preserves failing compiler error', async () => {
 const expected=Error('link failed');let captured: {path:string,bytes:Buffer}|undefined;
 await assert.rejects(runWindowsLauncherSmoke({platform:'win32',captureCoff:true,writeCapture:async (path:string,bytes:Buffer)=>{captured={path,bytes};},compileLauncher:async ({onCoffLinkFailure}:any)=>{await onCoffLinkFailure(Buffer.from('object'));throw expected;}}),(error:unknown)=>error===expected);
 assert.match(captured!.path,/packages[\\/]desktop[\\/]test-results[\\/]windows-launcher-smoke-failure\.syso$/);
 assert.equal(captured!.bytes.toString(),'object');
});

test('CLI accepts only an optional capture flag', async () => {
 // @ts-expect-error build tooling is JavaScript
 const { parseSmokeArguments }=await import('../scripts/windows-launcher-smoke.mjs');
 assert.equal(parseSmokeArguments([]),false);assert.equal(parseSmokeArguments(['--capture-coff']),true);
 for(const args of [['--capture-coff','--capture-coff'],['--output','elsewhere'],['unknown']])assert.throws(()=>parseSmokeArguments(args));
});
