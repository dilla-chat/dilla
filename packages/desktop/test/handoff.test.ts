import { once } from 'node:events';
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

test('Linux treats an actually exited zombie parent as exited rather than waiting for its launcher to reap it', { skip: process.platform !== 'linux' }, async () => {
 const { spawn, spawnSync } = await import('node:child_process'), { readFileSync } = await import('node:fs');
 const { previousProcessAlive } = await import('../src/handoff.ts');
 const child = spawn(process.execPath, ['-e', 'process.exit(23)']);
 await once(child, 'spawn');
 spawnSync(process.execPath, ['-e', 'Atomics.wait(new Int32Array(new SharedArrayBuffer(4)),0,0,300)']);
 const stat = readFileSync(`/proc/${child.pid}/stat`, 'utf8');
 assert.equal(stat.slice(stat.lastIndexOf(')') + 2)[0], 'Z');
 assert.equal(previousProcessAlive(child.pid!), false);
 await once(child, 'close');
});
