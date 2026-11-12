import test from 'node:test';
import assert from 'node:assert/strict';
import { join } from 'node:path';
import { shotDir } from './shoot.mjs';

test('shotDir without --out is the committed references', () => {
  assert.equal(shotDir('/p', 'linux', 'shell/channel-view', null), join('/p', 'references', 'linux', 'shell'));
});

test('shotDir with --out never points into the committed references', () => {
  const dir = shotDir('/p', 'linux', 'shell/channel-view', '/scratch/run');
  assert.equal(dir, join('/scratch/run', 'shots', 'shell'));
  assert.ok(!dir.startsWith('/p'));
});
