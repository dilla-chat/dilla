import { test } from 'node:test';
import assert from 'node:assert/strict';
import { EventEmitter } from 'node:events';
import { NativeQuitInstaller, UnstoppedUpdateHelper } from '../src/quit-install.ts';

class Application extends EventEmitter {
 exits: number[] = [];
 exit(code: number) { this.exits.push(code); }
}
const tick = () => new Promise<void>(done => setImmediate(done));

test('native installation waits for actual will-quit and a cancelled unload removes authorization', async () => {
 const app = new Application(); let installed = 0;
 const installer = new NativeQuitInstaller(app, error => { throw error; });
 installer.arm(async () => { installed++; });
 assert.equal(installed, 0);
 installer.cancel(); app.emit('will-quit', { preventDefault() { throw new Error('cancelled installer survived'); } });
 await tick(); assert.equal(installed, 0); assert.deepEqual(app.exits, []);
 installer.arm(async () => { installed++; });
 let prevented = false; app.emit('will-quit', { preventDefault() { prevented = true; } });
 assert.equal(prevented, true);
 await tick(); assert.equal(installed, 1); assert.deepEqual(app.exits, [0]);
});

test('a verified native install finishes before exit and failure keeps the working process available for retry', async () => {
 const app = new Application(); const failures: unknown[] = [];
 const installer = new NativeQuitInstaller(app, error => { failures.push(error); });
 const fault = new Error('insufficient disk space');
 installer.arm(async () => { throw fault; });
 app.emit('will-quit', { preventDefault() {} }); await tick();
 assert.deepEqual(failures, [fault]); assert.deepEqual(app.exits, []);
 let finish!: () => void;
 installer.arm(() => new Promise<void>(done => { finish = done; }));
 app.emit('will-quit', { preventDefault() {} }); await tick();
 assert.deepEqual(app.exits, []); finish(); await tick(); assert.deepEqual(app.exits, [0]);
});

test('an uncertain armed helper that cannot be drained never reopens old UI', async () => {
 const app = new Application(); let reopened = false;
 const installer = new NativeQuitInstaller(app, () => { reopened = true; });
 installer.arm(() => Promise.reject(new UnstoppedUpdateHelper('helper drain failed')));
 app.emit('will-quit', { preventDefault() {} }); await tick();
 assert.equal(reopened, false); assert.deepEqual(app.exits, [1]);
});
