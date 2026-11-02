import { assertPortableEsbuildFilter } from './support/portable-esbuild-filter.ts';
import test from 'node:test';
import assert from 'node:assert/strict';
import { build } from 'esbuild';
import vm from 'node:vm';
import { createRequire } from 'node:module';
import { EventEmitter } from 'node:events';
import { resolve } from 'node:path';
const load = createRequire(import.meta.url);
const installationFilter = /[\\/]linux-installation\.ts$/;
const nonce = 'a'.repeat(64);
const identityDigest = Buffer.alloc(64).toString('base64');
const flush = async () => { for (let index = 0; index < 8; index++) await new Promise<void>(done => setImmediate(done)); };
async function mainFixture(options: { kind?: 'system-package' | 'appimage'; argv?: string[]; env?: Record<string, string>; packaged?: boolean; validNative?: boolean; holdNative?: boolean; nativeLayout?: boolean; ownsLock?: boolean; gateError?: boolean; admissionManager?: string; admissionDigest?: string; identityManager?: string } = {}) {
 const effects: string[] = [], errors: string[] = [];
 let releaseValidation!: () => void;
 const validation = options.holdNative ? new Promise<void>(done => { releaseValidation = done; }) : Promise.resolve();
 const output = await build({ entryPoints: [resolve(import.meta.dirname, '../src/main.ts')], bundle: true, write: false, platform: 'node', format: 'cjs', external: ['electron', 'electron-updater', 'electron-updater/*'],
  define: options.kind ? { __DILLA_LINUX_INSTALL_KIND__: JSON.stringify(options.kind) } : {}, plugins: options.kind === 'system-package' ? [{ name: 'protected-native-result-only', setup(plugin) {
   plugin.onLoad({ filter: /[\\/]linux-admission\.ts$/ }, () => ({ contents: `export function holdLinuxAdmission() { return globalThis.recordAdmission(); }`, loader: 'ts' }));
   if(options.validNative) plugin.onLoad({ filter: installationFilter }, () => ({ contents: `export async function resolveLinuxInstallation(context) { globalThis.recordNative(context); await globalThis.validation; return {installKind:'system-package',identity:{manager:globalThis.identityManager},identitySha512:globalThis.identityDigest}; }`, loader: 'ts' }));
  } }] : [] });
 const app = Object.assign(new EventEmitter(), { isPackaged: options.packaged ?? true,
  getVersion: () => '0.1.0', getName: () => 'Dilla', getPath: () => { effects.push('profile'); throw Error('profile must not be accessed'); },
  setName: () => effects.push('normal-startup'), setDesktopName() {}, enableSandbox() {}, requestSingleInstanceLock: () => { effects.push('lock'); return options.ownsLock ?? false; },
  quit: () => effects.push('quit'), exit: (code: number) => effects.push('exit:' + code), whenReady: () => new Promise(() => {}),
 });
 const environment: Record<string, string> = { APPIMAGE: '/home/user/spoofed.AppImage', ...options.env };
 const electron = { app, protocol: { registerSchemesAsPrivileged() {} }, nativeTheme: {}, ipcMain: { handle() {}, on() {} } };
 const module = { exports: {} };
 vm.runInNewContext(output.outputFiles![0].text, { module, exports: module.exports, require: (name: string) => {
  if (name === 'electron') return electron;
  if (name === 'electron-updater') return { AppImageUpdater: class { constructor() { effects.push('AppImageUpdater'); } }, NsisUpdater: class {} };
  if (name.includes('/providers/Provider')) return { Provider: class {} };
  if (name === 'node:child_process') return { ...load(name), spawn: () => { effects.push('spawn'); throw Error('forbidden spawn'); } };
  return load(name);
 }, process: { platform: 'linux', arch: 'x64', env: environment, argv: ['dilla', ...(options.argv ?? [])], versions: {}, cwd: () => process.cwd(), pid: process.pid,
  execPath: options.nativeLayout || options.validNative ? '/usr/lib/dilla/native/app/dilla-desktop' : '/tmp/.mount-fixture/dilla-desktop',
  resourcesPath: options.nativeLayout || options.validNative ? '/usr/lib/dilla/native/app/resources' : '/tmp/.mount-fixture/resources' },
 __dirname: '/immutable', Buffer, TextDecoder, URL, AbortController, Error, setTimeout, clearTimeout,
 console: { error: (...values: unknown[]) => errors.push(values.map(String).join(' ')) },
 identityDigest, identityManager:options.identityManager ?? 'deb', recordAdmission: () => { effects.push('admission'); if(options.gateError) throw Error('busy'); return {manager:options.admissionManager ?? 'deb',identitySha512:options.admissionDigest ?? identityDigest}; },
 validation, recordNative: (context: { installKind: string }) => { effects.push('native-validation:' + context.installKind); },
 });
 await flush();
 return { effects, errors, environment, releaseValidation: async () => { releaseValidation(); await flush(); } };
}
test('compiled native main completes protected identity validation before consuming startup markers or touching a profile', async () => {
 const fixture = await mainFixture({ kind: 'system-package', validNative: true, holdNative: true, env: { DILLA_UPDATE_STARTUP_NONCE: nonce } });
 assert.deepEqual(fixture.effects, ['admission', 'native-validation:system-package']);
 assert.equal(fixture.environment.DILLA_UPDATE_STARTUP_NONCE, nonce);
 await fixture.releaseValidation();
 assert.ok(fixture.errors.some(error => /package manager/.test(error)));
 assert.equal(fixture.environment.DILLA_UPDATE_STARTUP_NONCE, nonce);
 assert.deepEqual(fixture.effects, ['admission', 'native-validation:system-package', 'exit:1']);
});
for (const argv of [[`--update-watchdog=${nonce}`], [`--update-watchdog=${nonce}`, '--update-recovery'], [`--update-bootstrap=${nonce}`], ['--sandbox-setup'], ['--setup-sandbox']]) {
 test('compiled native main rejects installation role before profile/spawn: ' + argv.join(' '), async () => {
  const fixture = await mainFixture({ kind: 'system-package', validNative: true, argv });
  assert.ok(fixture.errors.some(error => /package manager/.test(error)));
  assert.deepEqual(fixture.effects, ['admission', 'native-validation:system-package', 'exit:1']);
 });
}
test('unmarked packaged main rejects watchdog authority even when APPIMAGE is spoofed', async () => {
 const fixture = await mainFixture({ argv: [`--update-watchdog=${nonce}`] });
 assert.ok(fixture.errors.some(error => /installation authority/.test(error)));
 assert.deepEqual(fixture.effects, ['exit:1']);
});
test('unmarked normal development still reaches ordinary single-instance startup', async () => {
 const fixture = await mainFixture({ packaged: false, argv: ['--instance=https://chat.test', 'dilla://invite/example'] });
 assert.ok(fixture.effects.includes('lock')); assert.ok(fixture.effects.includes('quit')); assert.deepEqual(fixture.errors, []);
 assert.equal(fixture.effects.includes('profile'), false);
});
test('repacking compiled AppImage ASAR under the native fixed layout refuses startup', async () => {
 const fixture = await mainFixture({ kind: 'appimage', nativeLayout: true });
 assert.ok(fixture.errors.some(error => /layout/.test(error)));
 assert.deepEqual(fixture.effects, ['exit:1']);
});
test('repacking compiled native ASAR under an AppImage layout refuses startup', async () => {
 const fixture = await mainFixture({ kind: 'system-package' });
 assert.ok(fixture.errors.some(error => /layout/.test(error)));
 assert.deepEqual(fixture.effects, ['admission', 'exit:1']);
});

for (const kind of ['system-package', undefined] as const) test('normal managed/unmarked packaged startup skips AppImage recovery discovery: ' + String(kind), async () => {
 const fixture = await mainFixture({ kind, validNative: kind === 'system-package', ownsLock: true });
 assert.ok(fixture.effects.includes('lock')); assert.equal(fixture.effects.includes('profile'), false); assert.deepEqual(fixture.errors, []);
});

test('linux-installation fixture intercepts both POSIX and Windows module paths', async () => {
 await assertPortableEsbuildFilter(installationFilter, 'linux-installation');
});

test('compiled native main rejects a busy gate before identity or role work', async () => {
 const fixture=await mainFixture({kind:'system-package',validNative:true,gateError:true,env:{DILLA_UPDATE_STARTUP_NONCE:nonce}});
 assert.deepEqual(fixture.effects,['admission','exit:1']);assert.equal(fixture.environment.DILLA_UPDATE_STARTUP_NONCE,nonce);
});
for(const change of [{admissionManager:'rpm'}, {admissionDigest:Buffer.alloc(64,1).toString('base64')}, {admissionManager:'alpm'}]) test('compiled native main refuses mismatched or unqualified admission: '+JSON.stringify(change),async()=>{
 const fixture=await mainFixture({kind:'system-package',validNative:true,...change});
 assert.deepEqual(fixture.effects,['admission','native-validation:system-package','exit:1']);
});

test('compiled native main holds even matching ALPM admission until hook verification is available',async()=>{
 const fixture=await mainFixture({kind:'system-package',validNative:true,admissionManager:'alpm',identityManager:'alpm'});
 assert.deepEqual(fixture.effects,['admission','native-validation:system-package','exit:1']);
});

test('compiled native main uses the real addon gate before protected identity and keeps its lock on refusal', {skip:process.platform!=='linux'}, async t=>{
 const {mkdtemp,mkdir,writeFile,chmod,readFile,rm}=await import('node:fs/promises');
 const {tmpdir}=await import('node:os'),{join}=await import('node:path'),{spawn,spawnSync}=await import('node:child_process');
 // @ts-ignore Build tooling also runs directly in the Linux acceptance suite.
 const {buildLinuxAdmission}=await import('../scripts/build-linux-admission.mjs');
 const root=await mkdtemp(join(tmpdir(),'dilla-main-gate-'));await chmod(root,0o755);
 t.after(()=>rm(root,{recursive:true,force:true}));
 const gate=join(root,'gate'),addon=join(root,'admission.node');await mkdir(gate,{mode:0o755});
 await writeFile(join(gate,'lock'),'');await buildLinuxAdmission({output:addon,fixture:{directory:gate,uid:process.getuid!(),gid:process.getgid!()}});
 const source=(await readFile(new URL('../src/linux-admission.ts',import.meta.url),'utf8'))
  .replace(/const ADDON = '[^']+';/,'const ADDON = '+JSON.stringify(addon)+';')
  .replace(/const ancestors = \[[^\n]+\];/,'const ancestors = '+JSON.stringify([root,gate])+';')
  .replaceAll('s.uid !== 0','s.uid !== '+process.getuid!()).replaceAll('s.gid !== 0','s.gid !== '+process.getgid!());
 const output=await build({entryPoints:[resolve(import.meta.dirname,'../src/main.ts')],bundle:true,write:false,platform:'node',format:'cjs',external:['electron','electron-updater','electron-updater/*'],define:{__DILLA_LINUX_INSTALL_KIND__:JSON.stringify('system-package')},plugins:[{name:'owned-gate-fixture',setup(plugin){
  plugin.onLoad({filter:/[\\/]linux-admission\.ts$/},()=>({contents:source,loader:'ts',resolveDir:resolve(import.meta.dirname,'../src')}));
  plugin.onLoad({filter:installationFilter},()=>({contents:`export async function resolveLinuxInstallation(){globalThis.effects.push('identity');return {installKind:'system-package',identity:{manager:'deb'},identitySha512:${JSON.stringify(identityDigest)}}}`,loader:'ts'}));
 }}]});
 const compiled=join(root,'main.cjs');await writeFile(compiled,output.outputFiles![0].contents);
 const script=`const vm=require('node:vm'),fs=require('node:fs'),events=require('node:events'),effects=[];const app=Object.assign(new events.EventEmitter(),{isPackaged:true,getVersion:()=> '0.1.0',getName:()=> 'Dilla',setName(){},setDesktopName(){},enableSandbox(){},requestSingleInstanceLock:()=>{effects.push('lock');return false},quit(){effects.push('quit')},exit:code=>effects.push('exit:'+code),whenReady:()=>new Promise(()=>{})});const electron={app,protocol:{registerSchemesAsPrivileged(){}},nativeTheme:{},ipcMain:{handle(){},on(){}}};const module={exports:{}};vm.runInNewContext(fs.readFileSync(process.argv[1],'utf8'),{module,exports:module.exports,require:name=>name==='electron'?electron:name==='electron-updater'?{}:name.includes('/providers/Provider')?{Provider:class{}}:require(name),process:{platform:'linux',arch:'x64',env:{},argv:['dilla'],versions:{},cwd:()=>process.cwd(),pid:process.pid,execPath:'/usr/lib/dilla/native/app/dilla-desktop',resourcesPath:'/usr/lib/dilla/native/app/resources'},__dirname:'/immutable',effects,Buffer,TextDecoder,URL,AbortController,Error,setTimeout,clearTimeout,console:{error(){}}});setTimeout(()=>process.send(effects),30);process.on('message',()=>process.exit(0));`;
 const launch=async(state:string)=>{
  await writeFile(join(gate,'status.json'),JSON.stringify({format:1,manager:'deb',state,transaction:'a'.repeat(64),identitySha512:identityDigest,alpmHooks:null}));
  const child=spawn(process.execPath,['-e',script,compiled],{stdio:['ignore','ignore','pipe','ipc']});
  const exited=new Promise(done=>child.once('exit',done));
  try {
   const response=await new Promise<unknown>((done,reject)=>{const timer=setTimeout(()=>reject(Error('main gate timeout')),3000);child.once('message',m=>{clearTimeout(timer);done(m);});child.once('error',error=>{clearTimeout(timer);reject(error);});});
   assert.deepEqual(response,state==='ready'?['identity','lock','quit']:['exit:1']);
   assert.equal(spawnSync('flock',['-x','-n',join(gate,'lock'),'true']).status,1);
  } finally {if(child.exitCode===null && child.signalCode===null)child.kill();await exited;}
  assert.equal(spawnSync('flock',['-x','-n',join(gate,'lock'),'true']).status,0);
 };
 await launch('ready');await launch('pending');
});
