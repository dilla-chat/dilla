import test from 'node:test';
import assert from 'node:assert/strict';
import { build } from 'esbuild';
import { createRequire } from 'node:module';
import { EventEmitter } from 'node:events';
import { createHash, generateKeyPairSync, sign } from 'node:crypto';
import { mkdtemp, writeFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import vm from 'node:vm';
import type { UpdateCoordinator } from '../src/update.ts';
const load = createRequire(import.meta.url);
const flush = () => new Promise<void>(done => setImmediate(done));

async function fixture(t: { after(callback: () => Promise<void>): void }) {
 const root = await mkdtemp(join(tmpdir(), 'dilla-engine-')); t.after(() => rm(root, { recursive: true, force: true }));
 const file = join(root, 'update.exe'), bytes = Buffer.from('authenticated installer'); await writeFile(file, bytes);
 const keys = generateKeyPairSync('ed25519'), now = Date.now();
 const release = Buffer.from(JSON.stringify({format:1,version:'1.0.1',channel:'stable',publishedAt:new Date(now-1000).toISOString(),expiresAt:new Date(now+3600000).toISOString(),artifacts:[{
  platform:'win32',arch:'x64',url:'https://github.com/dilla-chat/dilla/releases/download/desktop-v1.0.1/dilla-1.0.1-win-x64.exe',size:bytes.length,sha512:createHash('sha512').update(bytes).digest('base64'),
 }]}));
 const signature = sign(null,release,keys.privateKey).toString('base64');
 const compiled = await build({ entryPoints:[resolve(import.meta.dirname,'../src/updater.ts')],bundle:true,write:false,platform:'node',format:'cjs',external:['electron','electron-updater','electron-updater/*'],plugins:[{name:'fixture-key',setup(plugin){
  plugin.onLoad({filter:/[\\/]update-keys\.ts$/},()=>({contents:`export const RELEASE_PUBLIC_KEYS=${JSON.stringify([keys.publicKey.export({type:'spki',format:'pem'}).toString()])};`,loader:'ts'}));
 }}]});
 let installed!: () => void, failed!: () => void;
 const didInstall = new Promise<void>(done => { installed = done; }), didFail = new Promise<void>(done => { failed = done; });
 const calls: boolean[][] = [], engines: Engine[] = []; let failures = 0;
 class Engine extends EventEmitter {
  autoDownload = true; autoInstallOnAppQuit = true; fail = false;
  constructor(){super();engines.push(this);}
  setFeedURL(){} async checkForUpdates(){return {isUpdateAvailable:true};} async downloadUpdate(){return [file];}
  quitAndInstall(silent:boolean,relaunch:boolean){calls.push([silent,relaunch]);installed();if(this.fail)this.emit('error',new Error('native install failed'));}
 }
 const app = Object.assign(new EventEmitter(),{isPackaged:true,getVersion:()=> '1.0.0'}), module={exports:{} as {createUpdater:(change:()=>void,failed:()=>void)=>UpdateCoordinator}};
 const environment:Record<string,string>={APPIMAGE_EXIT_AFTER_INSTALL:'true'};
 vm.runInNewContext(compiled.outputFiles[0].text,{module,exports:module.exports,require(name:string){
  if(name==='electron')return {app};if(name==='electron-updater')return {NsisUpdater:Engine,AppImageUpdater:Engine};if(name.includes('/providers/Provider'))return {Provider:class{}};return load(name);
 },process:{platform:'win32',arch:'x64',env:environment,cwd:()=>process.cwd(),versions:{}},Buffer,TextDecoder,URL,AbortSignal,console,Error,
 fetch:async(url:string)=>new Response(url.endsWith('.sig')?signature:release)});
 const updater=module.exports.createUpdater(()=>{},()=>{failures++;failed();});await updater.check();assert.equal(updater.state.phase,'ready');
 return {app,updater,file,calls,engines,environment,didInstall,didFail,failures:()=>failures};
}

for(const relaunch of [false,true]) test('native engine installs only after accepted quit, relaunch='+relaunch,async t=>{
 const f=await fixture(t);assert.equal(f.engines[0].autoDownload,false);assert.equal(f.engines[0].autoInstallOnAppQuit,false);
 assert.equal(await f.updater.quit(false,relaunch),false);assert.deepEqual(f.calls,[]);
 assert.equal(await f.updater.quit(true,relaunch),true);assert.deepEqual(f.calls,[]);
 let blocked=false;f.app.emit('will-quit',{preventDefault(){blocked=true;}});
 await f.didInstall;
 assert.equal(blocked,true);assert.deepEqual(f.calls,[[true,relaunch]]);assert.equal(f.environment.APPIMAGE_EXIT_AFTER_INSTALL,undefined);
});
test('late renderer unload veto disarms native install',async t=>{
 const f=await fixture(t);await f.updater.quit(true,true);f.updater.cancelledQuit();
 f.app.emit('will-quit',{preventDefault(){assert.fail('vetoed quit retained installer');}});await flush();assert.deepEqual(f.calls,[]);assert.equal(f.updater.state.phase,'error');
});
test('tampered cache after arming is rejected before calling native engine',async t=>{
 const f=await fixture(t);await f.updater.quit(true,true);await writeFile(f.file,'tampered');
 f.app.emit('will-quit',{preventDefault(){}});
 await f.didFail;
 assert.deepEqual(f.calls,[]);assert.equal(f.updater.state.phase,'error');
});
test('native installation errors surface for retry after windows have closed',async t=>{
 const f=await fixture(t);f.engines[0].fail=true;await f.updater.quit(true,true);f.app.emit('will-quit',{preventDefault(){}});
 await f.didFail;
 assert.equal(f.updater.state.phase,'error');assert.match(f.updater.state.message!,/native install failed/);
});
