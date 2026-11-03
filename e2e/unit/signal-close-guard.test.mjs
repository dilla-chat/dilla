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

test('an explicitly expected security leave permits only its own SDK teardown and is revoked by reconnect', async () => {
  const alice = signalCloseGuard(), bob = signalCloseGuard();
  let expectedClose = false;
  await alice.expectSecurityLeave(async () => {
    assert.equal(alice.observe(closeError), false, 'security intent alone cannot excuse an active transport failure');
    alice.observe(disconnect);
    expectedClose = alice.observe(closeError);
    bob.observe(disconnect);
    assert.equal(bob.observe(closeError), false, 'another context has no expected security leave');
    for (const text of [closeError.replace('1006', '1011'), closeError.replace('1006', '10060'), 'unrelated application error 1006']) {
      assert.equal(alice.observe(text), false);
    }
    // The caller must establish its blocked/no-leaf postconditions before this scope resolves.
  });
  assert.equal(expectedClose, true, 'the known security operation deliberately disconnected this SDK');
  alice.observe('connection state changed: disconnected -> connecting { room: REDACTED }');
  assert.equal(alice.observe(closeError), false, 'a new active call has no authorization from the old security leave');
});

test('a failed security-leave postcondition rejects the scope and revokes its authorization', async () => {
  const guard = signalCloseGuard();
  let rejected = false;
  try {
    await guard.expectSecurityLeave(async () => {
      guard.observe(disconnect);
      throw new Error('the expected blocked or leaf-removal postcondition failed');
    });
  } catch (error) {
    rejected = /postcondition failed/.test(String(error));
  }
  assert.equal(rejected, true, 'failed security postconditions cannot produce a successful scope');
  assert.equal(guard.observe(closeError), false);
});
