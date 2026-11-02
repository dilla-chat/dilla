import { test, expect, _electron, type ElectronApplication, type TestInfo } from '@playwright/test';
import { build } from 'esbuild';
import { createServer } from 'node:http';
import { createHash } from 'node:crypto';
import { cp, mkdtemp, readFile, realpath, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { once } from 'node:events';
import { spawnSync } from 'node:child_process';
import { nativeTrace } from '../support/trace.ts';

const desktop = resolve(import.meta.dirname, '../..');
interface Receipt { path: string; method: string; cookie?: string; authorization?: string; body: string }
async function fixture(info: TestInfo) {
 const root = await realpath(await mkdtemp(join(tmpdir(), 'dilla-forwarding-'))), project = join(root, 'app');
 const receipts: Receipt[] = [];
 const server = createServer(async (request, response) => {
  const chunks: Buffer[] = []; for await (const chunk of request) chunks.push(Buffer.from(chunk));
  const path = new URL(request.url!, 'http://owned.invalid').pathname;
  receipts.push({ path, method: request.method!, cookie: request.headers.cookie, authorization: request.headers.authorization, body: Buffer.concat(chunks).toString() });
  if (path === '/v1/redirect-contract') { response.writeHead(302, { location: '/v1/redirect-target' }); response.end(); }
  else if (path === '/v1/forwarding-contract' || path === '/v1/redirect-target') { response.writeHead(201, { 'content-type': 'text/plain' }); response.end('owned-response'); }
  else { response.writeHead(404); response.end(); }
 });
 let application: ElectronApplication | undefined, stopTrace: (() => Promise<void>) | undefined;
 try {
  await new Promise<void>(done => server.listen(0, '127.0.0.1', done));
  const address = server.address(); if (!address || typeof address === 'string') throw new Error('Missing owned fixture listener');
  const instance = `http://127.0.0.1:${address.port}/`, profile = join(root, 'profile');
  await cp(join(desktop, 'dist'), join(project, 'dist'), { recursive: true });
  await writeFile(join(project, 'package.json'), JSON.stringify({ name: 'dilla-forwarding-acceptance', version: '1.0.0', main: 'dist/main.cjs' }));
  // This fixed acceptance worker is added to the verified LOCAL bundle. The
  // HTTP fixture supplies only data; it never supplies renderer code.
  const worker = `self.onmessage=async({data})=>{try{const response=await fetch(data.path,{method:data.method,credentials:'omit',cache:'no-store',redirect:'error',headers:{Authorization:'Bearer owned-fixture-authorization','Content-Type':'application/octet-stream'},...(data.method==='POST'?{body:new TextEncoder().encode('owned-request-body')}:{}),signal:AbortSignal.timeout(30000)});self.postMessage({ok:true,status:response.status,body:await response.text()});}catch(error){self.postMessage({ok:false,name:error.name});}};`;
  const client = join(project, 'dist/client'), path = 'assets/forwarding-contract-worker.js';
  await writeFile(join(client, path), worker);
  const manifest = JSON.parse(await readFile(join(client, 'dilla-manifest.json'), 'utf8'));
  manifest.files.push({ path, size: Buffer.byteLength(worker), sha256: createHash('sha256').update(worker).digest('hex') });
  await writeFile(join(client, 'dilla-manifest.json'), JSON.stringify(manifest));
  await build({ entryPoints: [join(desktop, 'src/preload.ts')], outfile: join(project, 'dist/preload.cjs'), bundle: true, platform: 'node', format: 'cjs', external: ['electron'] });
  await build({ entryPoints: [join(desktop, 'src/main.ts')], outfile: join(project, 'dist/main.cjs'), bundle: true, platform: 'node', format: 'cjs', external: ['electron'], plugins: [{ name: 'private-forwarding-profile', setup(plugin) {
   plugin.onLoad({ filter: /[\\/]src[\\/]main\.ts$/ }, async args => {
    const source = await readFile(args.path, 'utf8');
    if (source.split("app.setName('Dilla');").length !== 3) throw new Error('Owned main identity boundary changed');
    return { contents: source.replaceAll("app.setName('Dilla');", `app.setName('DillaForwardingAcceptance');app.setPath('userData',${JSON.stringify(profile)});`), loader: 'ts', resolveDir: join(desktop, 'src') };
   });
  } }] });
  application = await _electron.launch({ chromiumSandbox: true, args: [project, '--instance=' + instance, '--allow-loopback'] });
  const page = await application.firstWindow(); stopTrace = await nativeTrace(application.context(), info);
  await page.waitForLoadState('load'); await page.locator('#root').waitFor();
  await application.evaluate(async ({ BrowserWindow }, instance) => { await BrowserWindow.getAllWindows()[0].webContents.session.cookies.set({ url: instance, name: 'owned_cookie', value: 'owned-fixture-cookie', httpOnly: true, sameSite: 'lax' }); }, instance);
  return { receipts, async send(path: string, method = 'GET') { return page.evaluate(({ path, method }) => new Promise<{ ok: boolean; status?: number; body?: string; name?: string }>((done, fail) => {
   const worker = new Worker('/assets/forwarding-contract-worker.js'); worker.onmessage = event => { worker.terminate(); done(event.data); }; worker.onerror = () => { worker.terminate(); fail(new Error('Owned worker failed')); }; worker.postMessage({ path, method });
  }), { path, method }); }, async close() {
   try { await stopTrace?.(); } finally {
   if (application && application.process().exitCode === null && application.process().signalCode === null) {
    const child = application.process(), closed = once(child, 'close');
    const timer = setTimeout(() => { if (process.platform === 'win32' && child.pid) spawnSync('taskkill', ['/PID', String(child.pid), '/T', '/F'], { stdio: 'ignore' }); else child.kill('SIGKILL'); }, 5000);
    try { await application.evaluate(({ app }) => app.exit(0)).catch(() => {}); await closed; } finally { clearTimeout(timer); }
   }
   server.closeAllConnections(); await new Promise<void>(done => server.close(() => done())); await rm(root, { recursive: true, force: true, maxRetries: 10, retryDelay: 100 });
   }
  } };
 } catch (error) { if (application) await application.evaluate(({ app }) => app.exit(0)).catch(() => {}); server.closeAllConnections(); await new Promise<void>(done => server.close(() => done())); await rm(root, { recursive: true, force: true, maxRetries: 10, retryDelay: 100 }); throw error; }
}

test('actual forwarding keeps worker bearer/body/response and omits the private session cookie', async ({}, info) => {
 const app = await fixture(info);
 try {
  expect(await app.send('/v1/forwarding-contract')).toEqual({ ok: true, status: 201, body: 'owned-response' });
  const get = app.receipts.find(row => row.path === '/v1/forwarding-contract');
  expect(get?.authorization).toBe('Bearer owned-fixture-authorization'); expect(get?.cookie).toBeUndefined();
  expect(await app.send('/v1/forwarding-contract', 'POST')).toEqual({ ok: true, status: 201, body: 'owned-response' });
  const post = app.receipts.find(row => row.path === '/v1/forwarding-contract' && row.method === 'POST');
  expect(post?.body).toBe('owned-request-body'); expect(post?.authorization).toBe('Bearer owned-fixture-authorization'); expect(post?.cookie).toBeUndefined();
 } finally { await app.close(); }
});

test('actual forwarding preserves the worker redirect-error policy', async ({}, info) => {
 const app = await fixture(info);
 try {
  expect(await app.send('/v1/redirect-contract')).toEqual({ ok: false, name: 'TypeError' });
  expect(app.receipts.filter(row => row.path === '/v1/redirect-target')).toHaveLength(0);
 } finally { await app.close(); }
});
