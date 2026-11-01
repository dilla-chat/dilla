import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createHash, generateKeyPairSync, sign } from 'node:crypto';
import { mkdtemp, mkdir, writeFile, readFile, rm } from 'node:fs/promises';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { parseWindowsSlot, selectWindowsSlot, verifyWindowsSlot, publishWindowsSlot, WindowsCommitUncertain } from '../src/windows-slots.ts';
const digest = (raw: Uint8Array | string) => createHash('sha512').update(raw).digest('base64');
const initial = { format:1,kind:'initial',generation:0,version:'1.0.0',manifestSha512:digest('old') };
const raw = (value: unknown) => Buffer.from(JSON.stringify(value));
test('slots require an authenticated committed predecessor, reject conflicts and never synthesize missing initial state', () => {
 const parsed = parseWindowsSlot(raw(initial)); assert.deepEqual(selectWindowsSlot([parsed]), parsed);
 assert.throws(()=>selectWindowsSlot([]), /slot|committed/);
 const next = parseWindowsSlot(raw({...initial,kind:'release',generation:1,nonce:'a'.repeat(64),acceptedAt:Date.now(),version:'1.0.1'}));
 assert.equal(selectWindowsSlot([parsed,next]).version,'1.0.1');
 assert.throws(()=>selectWindowsSlot([next,{...next,manifestSha512:digest('other')}]), /conflict/i);
 for(const changed of [{...initial,generation:1},{...initial,command:'outside'},{...next,nonce:'../escape'},{...next,generation:2**53},{...initial,manifestSha512:'A'.repeat(85)+'B=='}]) assert.throws(()=>parseWindowsSlot(raw(changed)));
});
test('release slot authentication binds original accepted signature, exact installer and same completed registered journal', async () => {
 const root=await mkdtemp(join(tmpdir(),'dilla-slots-')),keys=generateKeyPairSync('ed25519'),nonce='a'.repeat(64),directory=join(root,'startup-'+nonce),now=Date.now();
 try {
  await mkdir(directory);const installer=Buffer.from('signed exact NSIS'),manifest=digest('next manifest');
  const release={format:1,version:'1.0.1',channel:'stable',publishedAt:new Date(now-1000).toISOString(),expiresAt:new Date(now+10000).toISOString(),artifacts:[{platform:'win32',arch:'x64',url:'https://github.com/jonasthim/dilla/releases/download/desktop-v1.0.1/dilla-1.0.1-win-x64.exe',size:installer.length,sha512:digest(installer),programManifest:{url:'https://github.com/jonasthim/dilla/releases/download/desktop-v1.0.1/dilla-1.0.1-win-x64.manifest.json',size:30,sha512:manifest}}]};
  const metadata=raw(release),slot=parseWindowsSlot(raw({format:1,kind:'release',generation:1,version:'1.0.1',manifestSha512:manifest,nonce,acceptedAt:now}));
  const trust={publicKeys:[keys.publicKey.export({type:'spki',format:'pem'}).toString()],channel:'stable' as const,arch:'x64',initialVersion:initial.version,initialManifestSha512:initial.manifestSha512,now:now+100000};
  await writeFile(join(directory,'release.json'),metadata);await writeFile(join(directory,'release.json.sig'),sign(null,metadata,keys.privateKey).toString('base64'));await writeFile(join(directory,'update.exe'),installer);
  for(const journal of [{phase:'launching',childPid:111},{phase:'complete',version:'1.0.2',childPid:111},{phase:'complete',version:'1.0.1'}]) {
   await writeFile(join(directory,'journal.json'),raw({format:1,nonce,...journal}));await assert.rejects(verifyWindowsSlot(slot,trust,root),/journal|complete/);
  }
  await writeFile(join(directory,'journal.json'),raw({format:1,nonce,phase:'complete',version:'1.0.1',childPid:111}));await verifyWindowsSlot(slot,trust,root);
  await writeFile(join(directory,'update.exe'),'tampered');await assert.rejects(verifyWindowsSlot(slot,trust,root),/sha512|size|checksum/);
 }finally{await rm(root,{recursive:true,force:true});}
});
test('slot publication flushes bytes before its one-way rename; visible uncertain commit never permits rollback', async () => {
 const root=await mkdtemp(join(tmpdir(),'dilla-slot-commit-')),next=parseWindowsSlot(raw({...initial,kind:'release',generation:1,nonce:'a'.repeat(64),acceptedAt:Date.now(),version:'1.0.1'}));
 try {
  let renamed=false;
  await assert.rejects(publishWindowsSlot(root,next,{beforeRename:async()=>{throw Object.assign(new Error('full disk'),{code:'ENOSPC'});},afterRename:async()=>{renamed=true;}}),/full disk/);assert.equal(renamed,false);
  await assert.rejects(readFile(join(root,'windows-active-1.json')),{code:'ENOENT'});
  assert.equal(await publishWindowsSlot(root,next,{afterRename:async()=>{throw new Error('directory flush uncertain');}}),'committed');
  assert.deepEqual(parseWindowsSlot(await readFile(join(root,'windows-active-1.json'))),next);
  await assert.rejects(publishWindowsSlot(root,next,{afterRename:async()=>{await rm(join(root,'windows-active-1.json'));throw new Error('commit read failed');}}),WindowsCommitUncertain);
 }finally{await rm(root,{recursive:true,force:true});}
});

test('real slot open/write/flush/close faults before rename leave the committed predecessor unchanged',async()=>{
 const{spawnSync}=await import('node:child_process'),{resolve}=await import('node:path');
 const root=await mkdtemp(join(tmpdir(),'dilla-slot-io-'));
 try{
  await writeFile(join(root,'windows-active-0.json'),raw(initial));
  for(const fault of ['open','writeFile','sync','close','rename-after']){
   const next={...initial,kind:'release',generation:1,nonce:'a'.repeat(64),acceptedAt:Date.now(),version:'1.0.1'},control=join(root,'control.json');
   await writeFile(control,JSON.stringify({root,fault,next,module:resolve(import.meta.dirname,'../src/windows-slots.ts')}),{mode:0o600});
   const script=`import fs from 'node:fs/promises';import{syncBuiltinESMExports}from'node:module';const o=JSON.parse(await fs.readFile(process.argv[1],'utf8'));const originalOpen=fs.open,originalRename=fs.rename;fs.open=async(file,...args)=>{const temp=String(file).includes('windows-active-1.json.');if(temp&&o.fault==='open')throw Object.assign(new Error('ENOSPC open'),{code:'ENOSPC'});const h=await originalOpen(file,...args);if(temp&&['writeFile','sync','close'].includes(o.fault)){const original=h[o.fault].bind(h);h[o.fault]=async(...a)=>{if(o.fault==='close')await original(...a);throw Object.assign(new Error('ENOSPC '+o.fault),{code:'ENOSPC'});};}return h;};if(o.fault==='rename-after')fs.rename=async(...a)=>{await originalRename(...a);throw Object.assign(new Error('rename result lost'),{code:'EIO'});};syncBuiltinESMExports();const{publishWindowsSlot}=await import(o.module);try{process.stdout.write(JSON.stringify({result:await publishWindowsSlot(o.root,o.next)}));}catch(e){process.stdout.write(JSON.stringify({code:e.code,name:e.name}));}`;
   const result=spawnSync(process.execPath,['--input-type=module','--eval',script,control],{encoding:'utf8',timeout:10000});assert.equal(result.status,0,result.stderr);
   assert.deepEqual(parseWindowsSlot(await readFile(join(root,'windows-active-0.json'))),initial);
   if(fault==='rename-after'){assert.deepEqual(JSON.parse(result.stdout),{result:'committed'});assert.equal(parseWindowsSlot(await readFile(join(root,'windows-active-1.json'))).version,'1.0.1');}
   else{assert.equal(JSON.parse(result.stdout).code,'ENOSPC');await assert.rejects(readFile(join(root,'windows-active-1.json')),{code:'ENOENT'});}
  }
 }finally{await rm(root,{recursive:true,force:true});}
});
