import { test } from 'node:test';
import { strict as assert } from 'node:assert';
import { mkdtemp, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { execFileSync } from 'node:child_process';
import { createServer } from 'node:http';
import { request } from 'node:https';
import { createNativeHttpsProxy } from '../web/support/native-https-proxy.mjs';

test('HTTPS proxy forwards to loopback with certificate verification', async () => {
 const dir = await mkdtemp(join(tmpdir(), 'dilla-native-https-test-'));
 const backend = createServer((req,res) => { res.writeHead(200, { 'content-type': 'text/plain' }); res.end(req.url); });
 let proxy;
 try {
  execFileSync('openssl', ['req','-x509','-newkey','rsa:2048','-nodes','-keyout',join(dir,'key.pem'),'-out',join(dir,'cert.pem'),'-days','1','-subj','/CN=127.0.0.1','-addext','subjectAltName=IP:127.0.0.1'], { stdio:'ignore' });
  await new Promise(resolve => backend.listen(0,'127.0.0.1',resolve));
  proxy = await createNativeHttpsProxy({ key: await readFile(join(dir,'key.pem')), cert: await readFile(join(dir,'cert.pem')), upstreamPort: backend.address().port });
  const awaitableCert = await readFile(join(dir,'cert.pem'));
  const response = await new Promise((resolve,reject) => { const req = request({ hostname:'127.0.0.1', port:proxy.address().port, path:'/v1/test?x=1', ca:awaitableCert }, res => { let s='';res.on('data',c=>s+=c);res.on('end',()=>resolve(s)); }); req.on('error',reject);req.end(); });
  assert.equal(response,'/v1/test?x=1');
 } finally { if(proxy) await new Promise(resolve=>proxy.close(resolve)); await new Promise(resolve=>backend.close(resolve)); await rm(dir,{recursive:true,force:true}); }
});

test('HTTPS proxy forwards the WebSocket upgrade and data', async () => {
 const { connect } = await import('node:tls');
 const dir = await mkdtemp(join(tmpdir(), 'dilla-native-websocket-test-'));
 const backend = createServer(); let proxy; const sockets = new Set();
 backend.on('connection', socket => { sockets.add(socket); socket.on('close', () => sockets.delete(socket)); });
 backend.on('upgrade', (_req, socket, head) => { socket.write('HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n'); if (head.length) socket.write(head); socket.on('data', chunk => socket.write(chunk)); });
 try {
  execFileSync('openssl', ['req','-x509','-newkey','rsa:2048','-nodes','-keyout',join(dir,'key.pem'),'-out',join(dir,'cert.pem'),'-days','1','-subj','/CN=127.0.0.1','-addext','subjectAltName=IP:127.0.0.1'], { stdio:'ignore' });
  await new Promise(resolve => backend.listen(0,'127.0.0.1',resolve));
  const cert = await readFile(join(dir,'cert.pem'));
  proxy = await createNativeHttpsProxy({ key: await readFile(join(dir,'key.pem')), cert, upstreamPort: backend.address().port });
  await new Promise((resolve,reject) => {
   const client = connect({ host:'127.0.0.1', port:proxy.address().port, ca:cert }, () => client.write('GET /gateway HTTP/1.1\r\nHost: 127.0.0.1\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n'));
   let data = ''; client.setTimeout(5000, () => client.destroy(new Error('upgrade timeout'))); client.on('error',reject);
   client.on('data', chunk => { data += chunk; if (data.includes('\r\n\r\n') && !data.includes('probe')) client.write('probe'); if (data.includes('probe')) { try { assert.match(data,/^HTTP\/1\.1 101 /); client.destroy(); resolve(); } catch(e) { reject(e); } } });
  });
 } finally { for (const socket of sockets) socket.destroy(); if(proxy) await new Promise(resolve=>proxy.close(resolve)); await new Promise(resolve=>backend.close(resolve)); await rm(dir,{recursive:true,force:true}); }
});
