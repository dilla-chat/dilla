import { test } from 'node:test';
import assert from 'node:assert/strict';
import { waitForPreviousProcess } from '../src/handoff.ts';

test('a replacement process waits for the previous process and refuses a cancelled shutdown', async () => {
  let clock = 0;
  let alive = true;
  const deps = { isAlive: () => alive, now: () => clock, sleep: async () => { clock += 100; if (clock === 300) alive = false; } };
  await waitForPreviousProcess('1234', deps);
  assert.equal(clock, 300);
  clock = 0; alive = true;
  await assert.rejects(waitForPreviousProcess('1234', { ...deps, sleep: async () => { clock += 1000; } }), /previous.*running/);
  await assert.rejects(waitForPreviousProcess('not-a-pid', deps), /handoff/);
});
