import { test } from 'node:test';
import assert from 'node:assert/strict';
import { join } from 'node:path';
import { writeFile, readFile } from 'node:fs/promises';
import { activateInstalledUpdate, pendingInstalledUpdate } from '../src/update-state.ts';
import { writeUpdateJournal } from '../src/update-journal.ts';
import { installedFixture } from './support/installed-update.ts';
test('restart discovery reads one private nonce and a bounded phase/status, never paths or actions from a report', async () => installedFixture(async plan => {
 assert.equal(await pendingInstalledUpdate(plan.userData), null);
 await activateInstalledUpdate(plan);
 assert.deepEqual(await pendingInstalledUpdate(plan.userData), { nonce: plan.health.nonce, phase: 'prepared' });
 await writeUpdateJournal(plan, { phase: 'failed', failure: 'startup' });
 assert.deepEqual(await pendingInstalledUpdate(plan.userData), { nonce: plan.health.nonce, phase: 'failed', failure: 'startup' });
 await assert.rejects(writeUpdateJournal(plan, { phase: 'complete' }), /journal/);
 await writeUpdateJournal(plan, { phase: 'complete', childPid: process.pid });
 assert.equal(await pendingInstalledUpdate(plan.userData), null);
 assert.equal(await pendingInstalledUpdate(plan.userData, plan.health.version), null);
 assert.deepEqual(await pendingInstalledUpdate(plan.userData, plan.oldVersion), { nonce: plan.health.nonce, phase: 'complete', version: plan.health.version });
 for (const json of [{ format: 1, nonce: '../../arbitrary' }, { format: 1, nonce: plan.health.nonce, path: '/tmp/app' }, 'x'.repeat(3000)]) {
  await writeFile(join(plan.userData, 'updates/active.json'), typeof json === 'string' ? json : JSON.stringify(json), { mode: 0o600 });
  await assert.rejects(pendingInstalledUpdate(plan.userData), /active update/);
 }
 assert.equal(await readFile(plan.installed, 'utf8'), 'working-old-app');
}));

test('an existing active pointer cannot disappear semantically when its referenced ticket is missing', async () => installedFixture(async plan => {
 const { rename } = await import('node:fs/promises');
 await activateInstalledUpdate(plan);
 const moved = plan.health.directory + '.missing'; await rename(plan.health.directory, moved);
 try { await assert.rejects(pendingInstalledUpdate(plan.userData), { code: 'ENOENT' }); }
 finally { await rename(moved, plan.health.directory); }
}));
