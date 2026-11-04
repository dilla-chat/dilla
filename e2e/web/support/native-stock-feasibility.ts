// Candidate disposable-runner smoke, not installed-package acceptance.
import { spawn } from 'node:child_process';
import { mkdir, readFile } from 'node:fs/promises';
import { request, type Server } from 'node:https';
import { join } from 'node:path';
import { createNativeHttpsProxy } from './native-https-proxy.mjs';
import { createTestCertificate } from './native-test-cert.mjs';
import { REPO_ROOT, startTestHost } from './host.ts';
import { e2ePort } from './ports.ts';
import { arr, decode } from '../../../packages/client-core/src/cbor/index.ts';

export async function runNativeStockFeasibility(): Promise<void> {
if (process.env.GITHUB_ACTIONS !== 'true' || process.platform !== 'linux' || !process.getuid?.() || !process.env.HOME || !process.env.XDG_CONFIG_HOME)
 throw new Error('Disposable non-root GitHub Linux user required');
const home = process.env.HOME;
await mkdir(join(home, '.pki'), { mode: 0o700 });
const certificate = await createTestCertificate(join(home, 'native-test-certificate'), join(home, '.pki/nssdb'));
const host = await startTestHost({ port: e2ePort(8463), control: e2ePort(8464), webRoot: 'packages/web/dist', sfu: { port: e2ePort(8465), udpPort: e2ePort(8466) } });
let proxy: Server | undefined;
try {
 proxy = await createNativeHttpsProxy({ key: await readFile(certificate.key), cert: await readFile(certificate.cert), upstreamPort: e2ePort(8463) });
 let instanceResponses = 0;
 proxy.on('request', (request, response) => {
  if (request.method === 'GET' && request.url === '/v1/instance') response.once('finish', () => { if (response.statusCode === 200) instanceResponses++; });
 });
 const address = proxy.address();
 if (!address || typeof address === 'string') throw new Error('Unexpected HTTPS proxy address');
 const port = address.port;
 const ca = await readFile(certificate.ca);
 const result = await new Promise<{ status: number; body: Buffer }>((resolve, reject) => {
  const req = request({ hostname: '127.0.0.1', port, path: '/v1/instance', ca, headers: { accept: 'application/cbor' } }, res => {
   const chunks: Buffer[] = []; let size = 0;
   res.on('data', (chunk: Buffer) => { size += chunk.length; if (size > 65536) req.destroy(new Error('Instance response too large')); else chunks.push(chunk); });
   res.on('end', () => resolve({ status: res.statusCode ?? 0, body: Buffer.concat(chunks) }));
  });
  req.on('error', reject); req.setTimeout(10000, () => req.destroy(new Error('HTTPS test host timed out'))); req.end();
 });
 if (result.status !== 200) throw new Error(`HTTPS test host returned ${result.status}`);
 const instanceName = arr(decode(result.body))[5];
 if (typeof instanceName !== 'string' || !instanceName) throw new Error('Invalid test instance name');
 const beforeApp = instanceResponses;
 await new Promise<void>((resolve, reject) => {
  const child = spawn(process.execPath, ['e2e/web/support/native-stock-automation-probe.mjs'], { cwd: REPO_ROOT, stdio: 'inherit', env: { ...process.env, DILLA_NATIVE_TEST_ORIGIN: `https://127.0.0.1:${port}/`, DILLA_NATIVE_TEST_INSTANCE_NAME: instanceName } });
  child.once('error', reject); child.once('exit', (code, signal) => code === 0 ? resolve() : reject(new Error(`Stock probe failed: ${code ?? signal}`)));
 });
 if (instanceResponses <= beforeApp) throw new Error('Stock client made no successful HTTPS instance request');
 console.log('Stock client connected through native confirmation and reached onboarding over HTTPS. Shared account and upgrade acceptance run separately.');
} finally {
 if (proxy) { const server = proxy; await new Promise<void>((resolve, reject) => server.close(error => error ? reject(error) : resolve())); }
 await host.stop();
}

}
