import { test } from 'node:test';
import assert from 'node:assert/strict';
import { StartupUsability } from '../src/update-usability.ts';
test('startup requires every expected instance to report actual local core readiness, not window load or mode', async () => {
 const reports: boolean[] = [], gate = new StartupUsability(async ready => { reports.push(ready); });
 gate.expect('https://one/'); gate.expect('https://two/');
 await gate.status('https://one/', true); await gate.seal(); assert.deepEqual(reports, []);
 await gate.status('https://two/', true); assert.deepEqual(reports, [true]);
 await gate.status('https://two/', false); assert.deepEqual(reports, [true, false]);
 await gate.status('https://two/', true); assert.deepEqual(reports, [true, false, true]);
});
test('manager-only readiness needs a trusted internal document load after bootstrap completes', async () => {
 const reports: boolean[] = [], gate = new StartupUsability(async ready => { reports.push(ready); });
 await gate.managerLoaded(); assert.deepEqual(reports, []);
 await gate.seal(); assert.deepEqual(reports, [true]);
 gate.expect('https://one/'); await gate.status('https://one/', false); assert.deepEqual(reports, [true, false]);
 await gate.status('https://unknown/', true); assert.deepEqual(reports, [true, false]);
});

test('a manager crash or reload withdraws readiness even when its earlier ready-file write is still in flight', async () => {
 const reports: boolean[] = []; let finish!: () => void;
 const gate = new StartupUsability(async ready => { if (ready) await new Promise<void>(done => { finish = done; }); reports.push(ready); });
 await gate.seal();
 const loading = gate.managerLoaded(); await new Promise<void>(done => setImmediate(done));
 const crashed = gate.managerUnloaded(); finish(); await loading; await crashed;
 assert.deepEqual(reports, [true, false]);
 const reloaded = gate.managerLoaded(); await new Promise<void>(done => setImmediate(done)); finish(); await reloaded;
 assert.deepEqual(reports, [true, false, true]);
});

test('a failed readiness write cannot poison a later withdrawal of a possibly visible report', async () => {
 const writes: boolean[] = [], gate = new StartupUsability(async ready => { writes.push(ready); if (ready) throw new Error('flush failed after report publication'); });
 await gate.seal(); await assert.rejects(gate.managerLoaded(), /flush failed/);
 await gate.managerUnloaded(); assert.deepEqual(writes, [true, false]);
});
