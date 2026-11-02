import test from 'node:test';
import assert from 'node:assert/strict';
import {mkdtemp,writeFile,readFile,rm} from 'node:fs/promises';
import {join} from 'node:path';
import {tmpdir} from 'node:os';
import {acquireFixtureExclusive,tryFixtureExclusive} from '../web/support/native-admission-actor.ts';
test('actor serializes exclusive lock and publishes pending only while held',{skip:process.platform!=='linux'},async t=>{
 const root=await mkdtemp(join(tmpdir(),'dilla-actor-'));t.after(()=>rm(root,{recursive:true,force:true}));
 const lock=join(root,'lock'),status=join(root,'status.json');await writeFile(lock,'');await writeFile(status,'ready');
 assert.equal(tryFixtureExclusive(lock),true);
 const actor=await acquireFixtureExclusive(lock,status,'pending');
 try{assert.equal(tryFixtureExclusive(lock),false);assert.equal(await readFile(status,'utf8'),'ready');await actor.publish();assert.equal(await readFile(status,'utf8'),'pending');assert.equal(tryFixtureExclusive(lock),false);}
 finally{await actor.release();}
 assert.equal(tryFixtureExclusive(lock),true);
});
test('actor refuses when another holder has the lock',{skip:process.platform!=='linux'},async t=>{
 const root=await mkdtemp(join(tmpdir(),'dilla-actor-busy-'));t.after(()=>rm(root,{recursive:true,force:true}));
 const lock=join(root,'lock'),status=join(root,'status.json');await writeFile(lock,'');await writeFile(status,'ready');
 const held=await acquireFixtureExclusive(lock,status,'pending');try{await assert.rejects(acquireFixtureExclusive(lock,status,'pending'));assert.equal(await readFile(status,'utf8'),'ready');}finally{await held.release();}
});
test('actor refuses publication when the named lock inode changes',{skip:process.platform!=='linux'},async t=>{
 const {rename}=await import('node:fs/promises');const root=await mkdtemp(join(tmpdir(),'dilla-actor-inode-'));t.after(()=>rm(root,{recursive:true,force:true}));
 const lock=join(root,'lock'),status=join(root,'status.json');await writeFile(lock,'');await writeFile(status,'ready');
 const held=await acquireFixtureExclusive(lock,status,'pending');try{await rename(lock,lock+'.old');await writeFile(lock,'');await assert.rejects(held.publish());assert.equal(await readFile(status,'utf8'),'ready');}finally{await held.release();}
});
