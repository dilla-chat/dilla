import assert from 'node:assert/strict';
import test from 'node:test';
import { signalCloseGuard } from '../voice/support/signal-close-guard.ts';

const closeError = 'error reading from signal stream { error: ConnectionError: WS closed unexpectedly with code 1006 }';
const disconnect = 'disconnect from room { room: REDACTED }';

test('only an SDK disconnect initiated during explicit fixture cleanup permits its 1006 log', () => {
  const guard = signalCloseGuard();
  assert.equal(guard.observe(closeError), false, 'active call transport loss fails');
  guard.beginLeave();
  assert.equal(guard.observe(closeError), false, 'fixture intent alone cannot excuse transport loss');
  guard.observe(disconnect);
  assert.equal(guard.observe(closeError), true, 'the SDK is intentionally closing this fixture call');
  for (const text of [closeError.replace('1006', '1011'), closeError.replace('1006', '10060'), 'unrelated application error 1006']) {
    assert.equal(guard.observe(text), false, 'unrelated failures remain errors');
  }
});

test('a prior disconnect or a new connection cannot authorize suppression', () => {
  const guard = signalCloseGuard();
  guard.observe(disconnect);
  guard.beginLeave();
  assert.equal(guard.observe(closeError), false);
  guard.observe(disconnect);
  assert.equal(guard.observe(closeError), true);
  guard.observe('connection state changed: disconnected -> connecting { room: REDACTED }');
  assert.equal(guard.observe(closeError), false);
});

test('cleanup in one client never excuses an error in another client', () => {
  const alice = signalCloseGuard(), bob = signalCloseGuard();
  alice.beginLeave(); alice.observe(disconnect);
  assert.equal(alice.observe(closeError), true);
  assert.equal(bob.observe(closeError), false);
});
