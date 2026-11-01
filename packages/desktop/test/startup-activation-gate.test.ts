import { test } from 'node:test';
import assert from 'node:assert/strict';
import { StartupActivationGate } from '../src/startup-activation-gate.ts';

test('local health can run while account startup waits for durable activation', async () => {
 const gate = new StartupActivationGate(false), owner = {}, abort = new AbortController();
 let started = false;
 const pending = gate.wait(owner, abort.signal).then(() => { started = true; });
 await Promise.resolve(); assert.equal(started, false);
 gate.open(); await pending; assert.equal(started, true);
 await gate.wait({}, new AbortController().signal);
});
test('only one pending wait per owner is retained, without releasing the first', async () => {
 const gate = new StartupActivationGate(false), owner = {}, abort = new AbortController();
 let released = false;
 const pending = gate.wait(owner, abort.signal).then(() => { released = true; });
 await assert.rejects(gate.wait(owner, new AbortController().signal), /already waiting/);
 assert.equal(released, false); gate.open(); await pending;
});
test('navigation or process destruction rejects the old waiter and permits a fresh owner wait', async () => {
 const gate = new StartupActivationGate(false), owner = {}, abort = new AbortController();
 const pending = assert.rejects(gate.wait(owner, abort.signal), /cancelled/);
 abort.abort(); await pending;
 const next = gate.wait(owner, new AbortController().signal);
 gate.open(); await next;
});
test('ordinary startup is immediately available and a cancelled pending owner never starts', async () => {
 await new StartupActivationGate(true).wait({}, new AbortController().signal);
 const gate = new StartupActivationGate(false), abort = new AbortController(); abort.abort();
 await assert.rejects(gate.wait({}, abort.signal), /cancelled/);
 gate.open();
});
