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

// Linux zygote children rewrite argv to one space-separated process title.
// Observed from a real sandboxed CDP renderer; executable normalized, IDs omitted.
const rendererTitle = '/opt/Dilla/dilla-desktop --type=renderer --renderer-client-id=4\0';
test('accepts the Linux zygote process title while retaining renderer sandbox checks', () => {
 const browser = identity(41, 1), renderer = identity(42, 2);
 assert.doesNotThrow(() => assertRendererSandbox(browser, renderer, renderer, rendererTitle));
 for (const bypass of ['--no-sandbox', '--disable-setuid-sandbox', '--disable-seccomp-filter-sandbox', '--disable-namespace-sandbox']) {
  for (const cmdline of [`/opt/Dilla/dilla-desktop\0--type=renderer\0${bypass}\0`, `${rendererTitle.slice(0, -1)} ${bypass}\0`, `${rendererTitle.slice(0, -1)} ${bypass}=true\0`])
   assert.throws(() => assertRendererSandbox(browser, renderer, renderer, cmdline), /sandbox bypass/);
 }
 assert.throws(() => assertRendererSandbox(browser, identity(42, 1), identity(42, 1), rendererTitle), /filter/);
 assert.throws(() => assertRendererSandbox(browser, renderer, identity(42, 2, 999), rendererTitle), /identity/);
 for (const [field, value] of [['noNewPrivs', 0], ['seccomp', 0], ['capEff', '0000000000000001']]) {
  const unsandboxed = { ...renderer, [field]: value };
  assert.throws(() => assertRendererSandbox(browser, unsandboxed, unsandboxed, rendererTitle), /kernel sandbox/);
 }
});
test('does not infer a renderer from embedded values or conflicting process types', () => {
 const browser = identity(41, 1), renderer = identity(42, 2);
 for (const cmdline of [
  '/opt/Dilla/dilla-desktop --type=utility --name=--type=renderer\0',
  '/opt/Dilla/dilla-desktop --type=utility --type=renderer\0',
  '/opt/Dilla/dilla-desktop\0--type=utility\0--type=renderer\0',
  '/opt/Dilla/dilla-desktop\0--type=utility\0--name= --type=renderer\0',
  '/opt/Dilla/dilla-desktop --name= --type=renderer\0',
  '/opt/Dilla/dilla-desktop --type=renderer-extra\0',
  '',
 ]) assert.throws(() => assertRendererSandbox(browser, renderer, renderer, cmdline), /renderer/);
});
