import { test } from 'node:test';
import assert from 'node:assert/strict';
import { EventEmitter } from 'node:events';
import { SparkleInstaller } from '../src/sparkle.ts';

class FakeApp extends EventEmitter { quits = 0; quit() { this.quits++; } }
const willQuit = (app: FakeApp) => { let prevented = false; app.emit('will-quit', { preventDefault() { prevented = true; } }); return prevented; };

test('Sparkle cannot commit until actual will-quit and cancellation removes permission', () => {
 const app = new FakeApp(); let commits = 0, cancels = 0;
 const installer = new SparkleInstaller({ prepare: async version => version, commit: () => { commits++; }, cancel: () => { cancels++; } }, app, 60_000);
 installer.install(); assert.equal(commits, 0);
 installer.cancelledQuit(); assert.equal(willQuit(app), false); assert.equal(commits, 0); assert.equal(cancels, 1);
 installer.install(); assert.equal(commits, 0); assert.equal(willQuit(app), true); assert.equal(commits, 1);
 // The committed host stays alive for Sparkle's installer to terminate it: a second will-quit (its termination) passes.
 assert.equal(willQuit(app), false); assert.equal(commits, 1); assert.equal(app.quits, 0);
});

test('a committed host that Sparkle never terminates quits on its own after the grace', async () => {
 const app = new FakeApp(); let commits = 0;
 const installer = new SparkleInstaller({ prepare: async version => version, commit: () => { commits++; }, cancel: () => {} }, app, 20);
 installer.install(); assert.equal(willQuit(app), true); assert.equal(commits, 1); assert.equal(app.quits, 0);
 await new Promise(done => setTimeout(done, 60));
 assert.equal(app.quits, 1);
});
