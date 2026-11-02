import { test } from 'node:test';
import assert from 'node:assert/strict';
import { processIdentity, assertRendererSandbox } from '../web/support/native-stock-sandbox.mjs';

const status = (pid, filters, extras = '') => `Name:\tdilla-desktop\nPid:\t${pid}\nPPid:\t41\nUid:\t1001\t1001\t1001\t1001\nCapEff:\t0000000000000000\nNoNewPrivs:\t1\nSeccomp:\t2\nSeccomp_filters:\t${filters}\n${extras}`;
const stat = (pid, start) => `${pid} (dilla desktop) S 41 ${Array(17).fill('0').join(' ')} ${start} 0`;
const identity = (pid, filters, start = 123) => processIdentity(pid, status(pid, filters), stat(pid, start), 1001);
test('renderer needs its own seccomp filter, not just inherited container confinement', () => {
 const browser = identity(41, 1), renderer = identity(42, 2);
 assert.doesNotThrow(() => assertRendererSandbox(browser, renderer, renderer, '--type=renderer\0'));
 assert.throws(() => assertRendererSandbox(browser, identity(42, 1), identity(42, 1), '--type=renderer\0'), /filter/);
 for (const [field, value] of [['noNewPrivs', 0], ['seccomp', 0], ['capEff', '0000000000000001']]) {
  assert.throws(() => assertRendererSandbox(browser, { ...renderer, [field]: value }, renderer, '--type=renderer\0'));
 }
 assert.throws(() => assertRendererSandbox(browser, renderer, identity(42, 2, 999), '--type=renderer\0'), /identity/);
 assert.throws(() => assertRendererSandbox(browser, renderer, renderer, '--type=utility\0'), /renderer/);
 assert.throws(() => assertRendererSandbox(browser, renderer, renderer, '--type=renderer\0--no-sandbox\0'), /sandbox/);
 assert.throws(() => processIdentity(42, status(42, 2).replace(/1001/g, '0'), stat(42, 123), 1001), /UID/);
});
