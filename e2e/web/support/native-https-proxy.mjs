// Disposable CI TLS endpoint for the real local test host. It does not change
// certificate validation in Dilla and never accepts a non-loopback upstream.
import { createServer } from 'node:https';
import { request } from 'node:http';

export async function createNativeHttpsProxy({ key, cert, upstreamPort, port = 0 }) {
 if (!Number.isInteger(upstreamPort) || upstreamPort < 1 || upstreamPort > 65535 || !Number.isInteger(port) || port < 0 || port > 65535) throw new Error('Invalid local proxy port');
 const server = createServer({ key, cert }, (incoming, outgoing) => {
  const upstream = request({ host: '127.0.0.1', port: upstreamPort, method: incoming.method, path: incoming.url, headers: incoming.headers }, response => {
   outgoing.writeHead(response.statusCode, response.headers); response.pipe(outgoing);
  });
  upstream.on('error', () => { if (!outgoing.headersSent) outgoing.writeHead(502); outgoing.end(); });
  incoming.pipe(upstream);
 });
 server.on('upgrade', (incoming, socket, head) => {
  const upstream = request({ host: '127.0.0.1', port: upstreamPort, method: incoming.method, path: incoming.url, headers: incoming.headers });
  upstream.on('upgrade', (response, remote, remoteHead) => {
   socket.write(`HTTP/${response.httpVersion} ${response.statusCode} ${response.statusMessage}\r\n`);
   for (let i = 0; i < response.rawHeaders.length; i += 2) socket.write(`${response.rawHeaders[i]}: ${response.rawHeaders[i + 1]}\r\n`);
   socket.write('\r\n'); if (remoteHead.length) socket.write(remoteHead); if (head.length) remote.write(head);
   remote.on('error', () => socket.destroy()); socket.on('error', () => remote.destroy());
   remote.on('end', () => socket.destroy()); socket.on('end', () => remote.destroy());
   remote.on('close', () => socket.destroy()); socket.on('close', () => remote.destroy()); remote.pipe(socket).pipe(remote);
  });
  upstream.on('response', response => { socket.end(`HTTP/1.1 ${response.statusCode} ${response.statusMessage}\r\nConnection: close\r\n\r\n`); response.resume(); });
  upstream.on('error', () => socket.destroy()); upstream.end();
 });
 await new Promise((resolve, reject) => { server.once('error', reject); server.listen(port, '127.0.0.1', () => { server.off('error', reject); resolve(); }); });
 return server;
}
