import { test } from 'node:test';
import assert from 'node:assert/strict';
import { EventEmitter } from 'node:events';
import { NativeQuitInstaller } from '../src/quit-install.ts';
const tick = () => new Promise<void>(done => setImmediate(done));

test('native installation waits for accepted will-quit and a cancelled unload removes authorization', async () => {
 const app = new EventEmitter(); let installed = 0;
 const installer = new NativeQuitInstaller(app, error => { throw error; });
 installer.arm(async () => { installed++; });
 assert.equal(installed, 0);
 installer.cancel(); app.emit('will-quit', { preventDefault() { throw new Error('cancelled installer survived'); } });
 await tick(); assert.equal(installed, 0);
 installer.arm(async () => { installed++; });
 let prevented = false; app.emit('will-quit', { preventDefault() { prevented = true; } });
 assert.equal(prevented, true); await tick(); assert.equal(installed, 1);
 // The engine owns the final quit; Dilla's authorization is consumed once.
 app.emit('will-quit', { preventDefault() { throw new Error('installer ran twice'); } });
 assert.equal(installed, 1);
});

test('verification or native-engine errors return control for an explicit retry', async () => {
 const app = new EventEmitter(), failures: unknown[] = [];
 const installer = new NativeQuitInstaller(app, error => failures.push(error));
 const fault = new Error('insufficient disk space');
 installer.arm(async () => { throw fault; });
 app.emit('will-quit', { preventDefault() {} }); await tick();
 assert.deepEqual(failures, [fault]);
 installer.arm(async () => { installer.failed(fault); });
 app.emit('will-quit', { preventDefault() {} }); await tick();
 assert.deepEqual(failures, [fault, fault]);
 installer.arm(async () => {});
});
