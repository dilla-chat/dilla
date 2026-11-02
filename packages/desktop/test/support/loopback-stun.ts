import { createSocket } from 'node:dgram';
import { crc32 } from 'node:zlib';
import { isIPv4 } from 'node:net';

const COOKIE = 0x2112a442;

/** RFC 5389 Binding discovery only. Reflect the real UDP source endpoint: no
 * fabricated ICE candidates, relay, credentials, media or permission grants. */
export function bindingResponse(request: Buffer, address: string, port: number): Buffer | null {
 if (request.length < 20 || request.readUInt16BE(0) !== 0x0001 || request.readUInt32BE(4) !== COOKIE
  || request.readUInt16BE(2) !== request.length - 20 || request.length % 4 !== 0
  || !isIPv4(address) || !Number.isInteger(port) || port < 1 || port > 65535) return null;
 for (let offset = 20; offset < request.length;) {
  if (offset + 4 > request.length) return null;
  const length = request.readUInt16BE(offset + 2);
  offset += 4 + Math.ceil(length / 4) * 4;
  if (offset > request.length) return null;
 }
 const response = Buffer.alloc(40);
 response.writeUInt16BE(0x0101, 0); response.writeUInt16BE(20, 2);
 request.copy(response, 4, 4, 20); // cookie and the caller's exact 96-bit transaction ID
 response.writeUInt16BE(0x0020, 20); response.writeUInt16BE(8, 22); // XOR-MAPPED-ADDRESS
 response[25] = 1; response.writeUInt16BE(port ^ (COOKIE >>> 16), 26);
 address.split('.').forEach((octet, index) => { response[28 + index] = Number(octet) ^ request[4 + index]; });
 response.writeUInt16BE(0x8028, 32); response.writeUInt16BE(4, 34); // FINGERPRINT
 response.writeUInt32BE((crc32(response.subarray(0, 32)) ^ 0x5354554e) >>> 0, 36);
 return response;
}

export async function startLoopbackStun(): Promise<{ urls: string; requests(): number; close(): Promise<void> }> {
 const socket = createSocket('udp4'); let requests = 0;
 socket.on('message', (request, remote) => {
  const response = bindingResponse(request, remote.address, remote.port);
  if (response) { requests++; socket.send(response, remote.port, remote.address); }
 });
 await new Promise<void>((resolve, reject) => {
  socket.once('error', reject); socket.bind(0, '127.0.0.1', () => { socket.removeListener('error', reject); resolve(); });
 });
 return { urls: `stun:127.0.0.1:${socket.address().port}`, requests: () => requests, close: () => new Promise<void>(resolve => socket.close(() => resolve())) };
}
