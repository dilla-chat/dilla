import { test } from 'node:test';
import assert from 'node:assert/strict';
import { EventEmitter } from 'node:events';
import { SparkleInstaller } from '../src/sparkle.ts';

test('Sparkle cannot commit until actual will-quit and cancellation removes permission', () => {
 const app = new EventEmitter(); let commits = 0, cancels = 0;
 const installer = new SparkleInstaller({ prepare: async version => version, commit: () => { commits++; }, cancel: () => { cancels++; } }, app);
 installer.install(); assert.equal(commits, 0);
 installer.cancelledQuit(); app.emit('will-quit'); assert.equal(commits, 0); assert.equal(cancels, 1);
 installer.install(); assert.equal(commits, 0); app.emit('will-quit'); assert.equal(commits, 1);
});
