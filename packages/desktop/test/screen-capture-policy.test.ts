import { test } from 'node:test';
import assert from 'node:assert/strict';
import { validateCommand } from '../src/policy.ts';

const id = '28c64cf0-746c-4de2-9e0b-e484029648c0';
const options = { width: 1920, height: 1080, frameRate: 30 };

test('screen commands admit only a capture id and bounded numeric configuration', () => {
 for (const command of [{ type: 'stop-screen-capture', id }, { type: 'configure-screen-capture', id, options }]) {
  assert.deepEqual(validateCommand(command), command);
 }
 for (const command of [
  { type: 'stop-screen-capture', id: '../another-instance' },
  { type: 'stop-screen-capture', id, source: 'screen:0:0' },
  { type: 'configure-screen-capture', id, options: { ...options, source: 'screen:0:0' } },
  { type: 'configure-screen-capture', id, options: { ...options, audio: true } },
  { type: 'configure-screen-capture', id, options: { ...options, width: 3841 } },
  { type: 'configure-screen-capture', id, options: { ...options, height: -1 } },
  { type: 'configure-screen-capture', id, options: { ...options, frameRate: Infinity } },
  { type: 'configure-screen-capture', id, options: { ...options, frameRate: 30.5 } },
 ]) assert.throws(() => validateCommand(command));
});
