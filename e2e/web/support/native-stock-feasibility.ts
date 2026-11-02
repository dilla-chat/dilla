// Candidate disposable-runner smoke, not installed-package acceptance.
import { spawn } from 'node:child_process';
import { mkdir, readFile } from 'node:fs/promises';
import { request, type Server } from 'node:https';
import { join } from 'node:path';
import { createNativeHttpsProxy } from './native-https-proxy.mjs';
import { createTestCertificate } from './native-test-cert.mjs';
import { REPO_ROOT, startTestHost } from './host.ts';

export async function runNativeStockFeasibility(): Promise<void> {
if (process.env.GITHUB_ACTIONS !== 'true' || process.platform !== 'linux' || !process.getuid?.() || !process.env.HOME || !process.env.XDG_CONFIG_HOME)
 throw new Error('Disposable non-root GitHub Linux user required');
const home = process.env.HOME;
await mkdir(join(home, '.pki'), { mode: 0o700 });
const certificate = await createTestCertificate(join(home, 'native-test-certificate'), join(home, '.pki/nssdb'));
const host = await startTestHost({ port: 8463, control: 8464, webRoot: 'packages/web/dist', sfu: { port: 8465, udpPort: 8466 } });
let proxy: Server | undefined;
try {
 proxy = await createNativeHttpsProxy({ key: await readFile(certificate.key), cert: await readFile(certificate.cert), upstreamPort: 8463 });
 const address = proxy.address();
 if (!address || typeof address === 'string') throw new Error('Unexpected HTTPS proxy address');
 const port = address.port;
 const ca = await readFile(certificate.ca);
 const status = await new Promise<number>((resolve, reject) => {
  const req = request({ hostname: '127.0.0.1', port, path: '/v1/instance', ca: ca }, res => { res.resume(); res.on('end', () => resolve(res.statusCode ?? 0)); });
  req.on('error', reject); req.setTimeout(10000, () => req.destroy(new Error('HTTPS test host timed out'))); req.end();
 });
 if (status !== 200) throw new Error(`HTTPS test host returned ${status}`);
 await new Promise<void>((resolve, reject) => {
  const child = spawn(process.execPath, ['e2e/web/support/native-stock-automation-probe.mjs'], { cwd: REPO_ROOT, stdio: 'inherit', env: process.env });
  child.once('error', reject); child.once('exit', (code, signal) => code === 0 ? resolve() : reject(new Error(`Stock probe failed: ${code ?? signal}`)));
 });
 console.log('Node HTTPS test-host transport and stock manager CDP smoke passed; Chromium HTTPS trust and account acceptance were not tested.');
} finally {
 if (proxy) { const server = proxy; await new Promise<void>((resolve, reject) => server.close(error => error ? reject(error) : resolve())); }
 await host.stop();
}

}
