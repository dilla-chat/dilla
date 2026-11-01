import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createUpdateArming, sendUpdateArm } from '../src/update-arm.ts';
import { activateInstalledUpdate, activeInstalledUpdate } from '../src/update-state.ts';
import { installedFixture } from './support/installed-update.ts';
import { updateFS } from '../src/update-fs.ts';
const supported = ['linux', 'win32'].includes(process.platform);

test('helper readiness and a visible pointer never authorize installation without a parent-authenticated post-flush arm', { skip: !supported }, async () => installedFixture(async plan => {
 let alive = true, accepted = false;
 const server = await createUpdateArming(plan, process.pid + 1000, () => Promise.resolve(), () => alive, 1000);
 const waiting = server.wait().then(value => { accepted = value; });
 try {
  await activateInstalledUpdate(plan); await new Promise(done => setTimeout(done, 50)); assert.equal(accepted, false);
  alive = false; await waiting; assert.equal(accepted, false);
 } finally { await server.close(); }
}));

test('an authenticated arm acknowledges exact parent/helper/version/nonce only after active publication succeeds', { skip: !supported }, async () => installedFixture(async plan => {
 const helper = process.pid + 1000;
 const server = await createUpdateArming(plan, helper, async () => { assert.equal((await activeInstalledUpdate(plan.userData))?.nonce, plan.health.nonce); }, () => true, 1000);
 try {
  await activateInstalledUpdate(plan); const waiting = server.wait(); await sendUpdateArm(plan, helper, 1000); assert.equal(await waiting, true);
 } finally { await server.close(); }
}));

test('a wrong private arm secret cannot authorize the known live helper', { skip: !supported }, async () => installedFixture(async plan => {
 const helper = process.pid + 1000, server = await createUpdateArming(plan, helper, () => Promise.resolve(), () => true, 300);
 try { await assert.rejects(sendUpdateArm({ ...plan, activationSecret: '0'.repeat(64) }, helper, 100)); await server.close(); assert.equal(await server.wait(), false); }
 finally { await server.close(); }
}));

test('publication rename visible while directory flush fails cannot send arm or let parent death authorize mutation', { skip: process.platform !== 'linux' }, async t => installedFixture(async plan => {
 let parent = true, sent = false, entered!: () => void, release!: () => void;
 const atSync = new Promise<void>(done => { entered = done; }), held = new Promise<void>(done => { release = done; }), original = updateFS.promises.open;
 const server = await createUpdateArming(plan, process.pid + 1000, () => Promise.resolve(), () => parent, 1000);
 t.mock.method(updateFS.promises, 'open', async (...args: Parameters<typeof original>) => {
  const handle = await original(...args);
  if (String(args[0]) === plan.userData + '/updates') handle.sync = async () => { entered(); await held; throw Object.assign(new Error('pointer flush ENOSPC'), { code: 'ENOSPC' }); };
  return handle;
 });
 try {
  const publishing = (async () => { await activateInstalledUpdate(plan); sent = true; await sendUpdateArm(plan, process.pid + 1000); })();
  const rejected = assert.rejects(publishing, { code: 'ENOSPC' });
  await atSync; assert.equal((await activeInstalledUpdate(plan.userData))?.nonce, plan.health.nonce);
  parent = false; release(); await rejected; assert.equal(await server.wait(), false); assert.equal(sent, false);
 } finally { t.mock.restoreAll(); release(); await server.close(); }
}));
