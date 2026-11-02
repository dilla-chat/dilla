import test from 'node:test';
import assert from 'node:assert/strict';
import {spawn} from 'node:child_process';
import {observeOwnedTree,aliveWithIdentity} from '../web/support/native-admission-process.ts';
test('owned process observation pins direct child identity',{skip:process.platform!=='linux'},async()=>{
 const child=spawn(process.execPath,['-e','setInterval(()=>{},1000)'],{stdio:'ignore'});
 try{const all=await observeOwnedTree(process.pid);const entry=all.find(x=>x.pid===child.pid);assert.ok(entry);assert.equal(await aliveWithIdentity(entry),true);child.kill();await new Promise(done=>child.once('exit',done));assert.equal(await aliveWithIdentity(entry),false);}
 finally{if(child.exitCode===null&&child.signalCode===null)child.kill();}
});
test('process observation checks ownership and start identity before command or executable data',async()=>{
 const {inspectOwnedProcess}=await import('../web/support/native-admission-process.ts');
 const reads=[];const foreign={open:async()=>({read:async name=>{reads.push(name);return 'Uid:\t99999\t99999\t99999\t99999\nPPid:\t1\n';},list:async()=>[],link:async()=>{reads.push('exe');return '/x';},close:async()=>{}})};
 await assert.rejects(inspectOwnedProcess(200,undefined,foreign));assert.deepEqual(reads,['status']);
});
test('incomplete descendant observations fail rather than returning a partial tree',async()=>{
 const {observeOwnedTree}=await import('../web/support/native-admission-process.ts');
 const reads=[];const fake={open:async()=>({read:async name=>{reads.push(name);if(name==='status')return `Uid:\t${process.getuid()}\t${process.getuid()}\t${process.getuid()}\t${process.getuid()}\nPPid:\t1\nSeccomp:\t2\nNoNewPrivs:\t1\n`;if(name==='stat')return '200 (x) S '+Array(18).fill('0').join(' ')+' 123';if(name==='cmdline')return '';throw Object.assign(Error(),{code:'ENOENT'});},list:async()=>['200'],link:async()=>'/x',close:async()=>{}})};
 await assert.rejects(observeOwnedTree(200,fake));assert.ok(reads.includes('task/200/children'));
});
test('direct ELF identity is checked against the owned pinned executable',{skip:process.platform!=='linux'},async()=>{
 const {verifyOwnedExecutable}=await import('../web/support/native-admission-process.ts');
 const {realpath}=await import('node:fs/promises');const actual=await realpath('/proc/self/exe');
 await verifyOwnedExecutable(process.pid,actual);await assert.rejects(verifyOwnedExecutable(process.pid,actual+'.wrong'));
});

test('reused process identity is rejected before command data is read',async()=>{
 const {inspectOwnedProcess}=await import('../web/support/native-admission-process.ts');const reads=[];
 const uid=process.getuid();const fake={open:async()=>({read:async name=>{reads.push(name);if(name==='status')return `Uid:\t${uid}\t${uid}\t${uid}\t${uid}\nPPid:\t1\nSeccomp:\t2\nNoNewPrivs:\t1\n`;if(name==='stat')return '200 (x) S '+Array(18).fill('0').join(' ')+' 456';throw Error('unexpected read');},close:async()=>{}})};
 await assert.rejects(inspectOwnedProcess(200,'123',fake),/identity changed/);assert.deepEqual(reads,['status','stat']);
});
test('unexpected same-owner parent is rejected before command data is read',async()=>{
 const {inspectOwnedProcess}=await import('../web/support/native-admission-process.ts');const reads=[];const uid=process.getuid();
 const fake={open:async()=>({read:async name=>{reads.push(name);if(name==='status')return `Uid:\t${uid}\t${uid}\t${uid}\t${uid}\nPPid:\t7\nSeccomp:\t2\nNoNewPrivs:\t1\n`;if(name==='stat')return '200 (x) S '+Array(18).fill('0').join(' ')+' 456';throw Error('unexpected read');},close:async()=>{}})};
 await assert.rejects(inspectOwnedProcess(200,undefined,fake,8),/parent changed/);assert.deepEqual(reads,['status','stat']);
});
test('a vanished enumerated child fails the tree observation',{skip:process.platform!=='linux'},async()=>{
 const {observeOwnedTree}=await import('../web/support/native-admission-process.ts');const opened=[];const uid=process.getuid();
 const fake={open:async pid=>{opened.push(pid);if(pid===201)throw Object.assign(Error(),{code:'ESRCH'});return {read:async name=>{if(name==='status')return `Uid:\t${uid}\t${uid}\t${uid}\t${uid}\nPPid:\t1\nSeccomp:\t2\nNoNewPrivs:\t1\n`;if(name==='stat')return '200 (x) S '+Array(18).fill('0').join(' ')+' 123';if(name==='cmdline')return '';if(name==='task/200/children')return '201';throw Error('unexpected read');},list:async()=>['200'],close:async()=>{}};}};
 await assert.rejects(observeOwnedTree(200,fake),{code:'ESRCH'});assert.deepEqual(opened,[200,201]);
});
