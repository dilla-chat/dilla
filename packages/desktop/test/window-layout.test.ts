import { test } from 'node:test';
import assert from 'node:assert/strict';
import { windowShape } from '../src/window-layout.ts';

test('startup and authentication stay compact while workspace resizing has a desktop minimum', () => {
 assert.deepEqual(windowShape('starting'), { width: 440, height: 280, minWidth: 440, minHeight: 220, resizable: false });
 assert.deepEqual(windowShape('authentication', 500), { width: 440, height: 500, minWidth: 440, minHeight: 220, resizable: false });
 const workspace = windowShape('workspace');
 assert.equal(workspace.resizable, true); assert.ok(workspace.minWidth >= 800); assert.ok(workspace.minHeight >= 480);
 assert.ok(workspace.width >= workspace.minWidth); assert.ok(workspace.height >= workspace.minHeight);
 assert.equal(windowShape('authentication', 1600, 700).height, 620);
});
