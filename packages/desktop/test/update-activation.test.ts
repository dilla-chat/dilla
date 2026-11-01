import { test } from 'node:test';
import assert from 'node:assert/strict';
import { writeUpdateJournal, readUpdateJournal } from '../src/update-journal.ts';
import { updateFS } from '../src/update-fs.ts';
import { createStartupActivation, receiveStartupActivation } from '../src/update-activation.ts';
import { installedFixture } from './support/installed-update.ts';
const native = process.platform === 'linux' || process.platform === 'win32';

test('only authenticated durable activation releases the exact new main PID', { skip: !native }, async () => installedFixture(async plan => {
 const gate = await createStartupActivation(plan, 1000); gate.expect(process.pid);
 let enabled = false;
 const waiting = receiveStartupActivation(plan, process.pid, 1000).then(() => { enabled = true; });
 try {
  await new Promise(done => setTimeout(done, 50)); assert.equal(enabled, false);
  await writeUpdateJournal(plan, { phase: 'complete', childPid: process.pid });
  await gate.activate(process.pid); await waiting; assert.equal(enabled, true);
 } finally { await gate.close(); }
}));

test('a visible complete rename cannot activate while directory fsync is pending or after it fails', { skip: process.platform !== 'linux' }, async t => installedFixture(async plan => {
 const gate = await createStartupActivation(plan, 300); gate.expect(process.pid);
 let enabled = false, release!: () => void, entered!: () => void;
 const syncing = new Promise<void>(done => { entered = done; }), blocked = new Promise<void>(done => { release = done; });
 const original = updateFS.promises.open;
 t.mock.method(updateFS.promises, 'open', async (...args: Parameters<typeof original>) => {
  const handle = await original(...args);
  if (String(args[0]) === plan.health.directory) handle.sync = async () => { entered(); await blocked; throw Object.assign(new Error('directory full'), { code: 'ENOSPC' }); };
  return handle;
 });
 const waiting = receiveStartupActivation(plan, process.pid, 300).then(() => { enabled = true; });
 const failed = assert.rejects(waiting, /activation|closed|timed out/i);
 try {
  const writing = writeUpdateJournal(plan, { phase: 'complete', childPid: process.pid });
  const rejected = assert.rejects(writing, { code: 'ENOSPC' });
  await syncing; assert.equal((await readUpdateJournal(plan)).phase, 'complete');
  await new Promise(done => setTimeout(done, 50)); assert.equal(enabled, false);
  release(); await rejected; await gate.close(); await failed; assert.equal(enabled, false);
 } finally { t.mock.restoreAll(); release(); await gate.close(); }
}));

test('a wrong private activation secret cannot authenticate even with public nonce and PID', { skip: !native }, async () => installedFixture(async plan => {
 const gate = await createStartupActivation(plan, 300); gate.expect(process.pid);
 try {
  await writeUpdateJournal(plan, { phase: 'complete', childPid: process.pid });
  await assert.rejects(receiveStartupActivation({ ...plan, activationSecret: '0'.repeat(64) }, process.pid, 150), /activation|closed|timed out/i);
 } finally { await gate.close(); }
}));

test('authenticated local readiness may take longer than the acknowledgment deadline without dropping its pending activation channel',{skip:!native},async()=>installedFixture(async plan=>{
 const gate=await createStartupActivation(plan,200);gate.expect(process.pid);let enabled=false;
 const waiting=receiveStartupActivation(plan,process.pid,2000).then(()=>{enabled=true;});const observed=waiting.catch(error=>error);
 try{
  await new Promise(done=>setTimeout(done,450));assert.equal(enabled,false);
  await writeUpdateJournal(plan,{phase:'complete',childPid:process.pid});await gate.activate(process.pid);await observed;await waiting;assert.equal(enabled,true);
 }finally{await gate.close();}
}));
