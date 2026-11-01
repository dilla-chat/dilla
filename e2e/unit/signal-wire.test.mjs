import assert from 'node:assert/strict';
import test from 'node:test';
import { signalWireSummary } from '../voice/support/signal-wire.ts';

function fixture() {
  const names = ['REQUEST_ALIVE', 'HTTP_STREAM_REQUEST_BOUND_TO_JOB', 'SOCKET_POOL_BOUND_TO_SOCKET', 'WEBSOCKET_SENT_FRAME_HEADER', 'WEBSOCKET_RECV_FRAME_HEADER', 'SOCKET_READ_ERROR'];
  const log = { constants: { logEventTypes: Object.fromEntries(names.map((name, i) => [name, i])), logSourceType: { URL_REQUEST: 1, SOCKET: 2 } }, events: [] };
  const event = (name, id, time, params, type = 2) => log.events.push({ type: log.constants.logEventTypes[name], time: String(time), source: { id, type }, params });
  event('REQUEST_ALIVE', 999, 10, { url: 'wss://PRIVATE_SECRET/rtc/v1?access_token=TOKEN_SECRET' }, 1);
  event('HTTP_STREAM_REQUEST_BOUND_TO_JOB', 999, 11, { source_dependency: { id: 801 } });
  event('SOCKET_POOL_BOUND_TO_SOCKET', 801, 12, { source_dependency: { id: 703 } });
  event('REQUEST_ALIVE', 123, 12, { url: 'wss://PRIVATE_SECRET/gateway?ticket=GATEWAY_SECRET' }, 1);
  event('WEBSOCKET_SENT_FRAME_HEADER', 123, 13, { opcode: 8 });
  event('SOCKET_READ_ERROR', 123, 14, { net_error: -101 });
  return { log, event };
}

test('RTC close headers and associated socket errors retain only directions, relative time, local ordinals and error enums', () => {
  const { log, event } = fixture();
  event('WEBSOCKET_SENT_FRAME_HEADER', 999, 20, { opcode: 8, payload: 'REASON_SECRET', payload_length: 888, headers: 'AUTH_SECRET' });
  event('WEBSOCKET_RECV_FRAME_HEADER', 999, 25, { opcode: 8, payload: 'REASON_SECRET' });
  event('SOCKET_READ_ERROR', 703, 26, { net_error: -101, address: 'ADDRESS_SECRET' });
  event('WEBSOCKET_RECV_FRAME_HEADER', 999, 27, { opcode: 2, payload: 'FRAME_SECRET' });
  assert.deepEqual(signalWireSummary(log), [{ socket: 1, sentClose: 1, receivedClose: 1,
    frames: [{ direction: 'sent', ms: 10 }, { direction: 'received', ms: 15 }], errors: [{ kind: 'read', code: -101, ms: 16 }] }]);
  assert.doesNotMatch(JSON.stringify(signalWireSummary(log)), /SECRET|999|801|703|123|888|payload|headers|address|url/);
});

test('abrupt RTC socket loss stays distinct from a close handshake; unrelated request and gateway events cannot provide its close', () => {
  const { log, event } = fixture();
  event('HTTP_STREAM_REQUEST_BOUND_TO_JOB', 123, 15, { source_dependency: { id: 801 } });
  event('SOCKET_READ_ERROR', 703, 20, { net_error: -101 });
  assert.deepEqual(signalWireSummary(log), [{ socket: 1, sentClose: 0, receivedClose: 0, frames: [], errors: [{ kind: 'read', code: -101, ms: 10 }] }]);
});

test('a burst cannot produce an unbounded wire diagnostic', () => {
  const { log, event } = fixture();
  for (let i = 0; i < 2000; i++) {
    event('WEBSOCKET_SENT_FRAME_HEADER', 999, 20 + i, { opcode: 8 });
    event('SOCKET_READ_ERROR', 703, 20 + i, { net_error: -101 });
  }
  const [summary] = signalWireSummary(log);
  assert.equal(summary.frames.length, 32); assert.equal(summary.errors.length, 32);
  assert.equal(summary.sentClose, 2000);
});
