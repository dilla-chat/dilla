import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createSocket } from 'node:dgram';
import { once } from 'node:events';
import { bindingResponse, startLoopbackStun } from './support/loopback-stun.ts';

const request = Buffer.from('000100002112a4420102030405060708090a0b0c', 'hex');

test('Binding success preserves the complete transaction and encodes the real IPv4 endpoint and RFC fingerprint', () => {
 const response = bindingResponse(request, '127.0.0.1', 3478)!;
 assert.equal(response.toString('hex'), '010100142112a4420102030405060708090a0b0c0020000800012c845e12a443802800042b3571db');
 const second = Buffer.from(request); second.fill(0xff, 8, 20);
 assert.deepEqual(bindingResponse(second, '127.0.0.1', 3478)!.subarray(8, 20), Buffer.alloc(12, 0xff));
});

test('malformed headers, truncated transactions and attributes cannot produce a Binding response', () => {
 const declaredPayload = Buffer.from(request); declaredPayload.writeUInt16BE(4, 2);
 const wrongType = Buffer.from(request); wrongType.writeUInt16BE(0x0101, 0);
 const wrongCookie = Buffer.from(request); wrongCookie[4] = 0;
 const truncatedAttribute = Buffer.concat([request, Buffer.from('8022000561626364', 'hex')]); truncatedAttribute.writeUInt16BE(8, 2);
 for (const invalid of [Buffer.alloc(0), request.subarray(0, 19), declaredPayload, wrongType, wrongCookie, truncatedAttribute]) {
  assert.equal(bindingResponse(invalid, '127.0.0.1', 3478), null);
 }
 for (const [address, port] of [['::1', 3478], ['127.0.0.1', 0], ['127.0.0.1', 65536]] as const) assert.equal(bindingResponse(request, address, port), null);
});

test('the owned UDP fixture reflects the socket endpoint actually observed and ignores malformed datagrams', { timeout: 2000 }, async () => {
 const server = await startLoopbackStun(), client = createSocket('udp4');
 try {
  client.bind(0, '127.0.0.1'); await once(client, 'listening');
  const received = once(client, 'message');
  const port = Number(server.urls.split(':').at(-1));
  client.send(request.subarray(0, 19), port, '127.0.0.1');
  client.send(request, port, '127.0.0.1');
  const [response] = await received as [Buffer, unknown];
  assert.deepEqual(response.subarray(8, 20), request.subarray(8, 20));
  assert.equal(response.readUInt16BE(26) ^ 0x2112, client.address().port);
  assert.deepEqual([...response.subarray(28, 32)].map((octet, index) => octet ^ request[4 + index]), [127, 0, 0, 1]);
  assert.equal(server.requests(), 1);
 } finally { client.close(); await server.close(); }
});
