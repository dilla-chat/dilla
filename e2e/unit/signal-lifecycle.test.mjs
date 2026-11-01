import assert from 'node:assert/strict';
import test from 'node:test';
import { once } from 'node:events';
import { runInNewContext } from 'node:vm';
import { WebSocketServer } from 'ws';
import { SIGNAL_PREFIX, installSignalObserver, signalObservation, sdkSignalEvent } from '../voice/support/signal-lifecycle.ts';

async function server(body) {
  const server = new WebSocketServer({ port: 0, host: '127.0.0.1' });
  await once(server, 'listening');
  try { await body(server, `ws://127.0.0.1:${server.address().port}`); }
  finally { for (const socket of server.clients) socket.terminate(); await new Promise(resolve => server.close(resolve)); }
}

function observed() {
  const lines = [];
  const context = { WebSocket, URL, performance, console: { info: line => lines.push(line) } };
  runInNewContext(`(${installSignalObserver.toString()})()`, context);
  return { context, lines };
}

test('a real intentional signal close retains its native clean code and records client intent without URL, reason or payload', async () => {
  await server(async (server, origin) => {
    const { context, lines } = observed();
    const socket = new context.WebSocket(`${origin}/rtc/v1?access_token=TOKEN_SECRET`);
    await once(socket, 'open');
    assert.throws(() => socket.close(1006, 'INVALID_REASON_SECRET'), { name: 'InvalidAccessError' });
    const closed = once(socket, 'close');
    socket.send('SDP_AND_KEY_SECRET');
    socket.close(1000, 'ROOM_AND_REASON_SECRET');
    const [event] = await closed;
    assert.equal(event.code, 1000);
    assert.equal(event.wasClean, true);
    const events = lines.map(signalObservation).filter(Boolean);
    assert.equal(events.find(event => event.event === 'closed')?.clientClose, true);
    assert.equal(events.find(event => event.event === 'closed')?.code, 1000);
    assert.equal(events.find(event => event.event === 'closed')?.clean, true);
    assert.equal(/SECRET|access_token|127\.0\.0\.1|SDP/.test(JSON.stringify(events)), false);
  });
});

test('a real server transport loss reports unclean 1006 without inventing client close intent', async () => {
  await server(async (server, origin) => {
    const { context, lines } = observed();
    const connected = once(server, 'connection');
    const socket = new context.WebSocket(`${origin}/rtc`);
    await once(socket, 'open');
    const [remote] = await connected;
    const closed = once(socket, 'close');
    remote.terminate();
    const [event] = await closed;
    assert.equal(event.code, 1006);
    const seen = lines.map(signalObservation).filter(Boolean).find(event => event.event === 'closed');
    assert.equal(seen?.clientClose, false);
    assert.equal(seen?.code, 1006);
    assert.equal(seen?.clean, false);
  });
});

test('observation ignores gateway credentials and bounds a burst of repeated native close calls', async () => {
  await server(async (_server, origin) => {
    const { context, lines } = observed();
    const gateway = new context.WebSocket(`${origin}/gateway?ticket=GATEWAY_SECRET`);
    await once(gateway, 'open');
    const gatewayClosed = once(gateway, 'close'); gateway.close(1000); await gatewayClosed;
    assert.deepEqual(lines, []);
    const socket = new context.WebSocket(`${origin}/rtc/v1?access_token=TOKEN_SECRET`);
    await once(socket, 'open');
    const closed = once(socket, 'close');
    for (let i = 0; i < 1000; i++) socket.close(1000);
    await closed;
    assert.ok(lines.length > 0 && lines.length <= 128);
    assert.equal(lines.every(line => line.startsWith(SIGNAL_PREFIX)), true);
  });
});

test('allowlists retain lifecycle facts while dropping arbitrary console fields, identities and tokens', () => {
  const observed = signalObservation(SIGNAL_PREFIX + JSON.stringify({ event: 'closed', socket: 1, at: 3, state: 3, clientClose: true, code: 1006, clean: false, token: 'SECRET' }));
  assert.deepEqual(observed, { event: 'closed', socket: 1, at: 3, state: 3, clientClose: true, code: 1006, clean: false });
  assert.equal(signalObservation(SIGNAL_PREFIX + JSON.stringify({ event: 'SECRET', socket: 1, at: 3, state: 3, clientClose: true })), null);
  assert.equal(signalObservation(SIGNAL_PREFIX + JSON.stringify({ event: { toString: null } })), null);
  assert.deepEqual(sdkSignalEvent('connection state changed: connected -> disconnected {room: SECRET, participant: SECRET}'), { event: 'state', from: 'connected', to: 'disconnected' });
  assert.deepEqual(sdkSignalEvent('error reading from signal stream {error: WS closed unexpectedly with code 1006, token: SECRET}'), { event: 'stream-error', code: 1006 });
  assert.deepEqual(sdkSignalEvent('client leave request received (action=0) {reason: 1, room: SECRET}'), { event: 'server-leave', action: 0 });
  assert.deepEqual(sdkSignalEvent('signal connecting to ws://SECRET/rtc/v1?access_token=SECRET'), { event: 'signal-connecting' });
  assert.equal(sdkSignalEvent('unrelated PRIVATE_SECRET'), null);
});
