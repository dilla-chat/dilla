import { test, expect, _electron, type ElectronApplication } from '@playwright/test';
import { createServer } from 'node:http';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';

// "Your instances" is the first window of a profile with no instance. Connecting replaces it: one window, not two.
let app: ElectronApplication; let profile: string; let instance: string;
const server = createServer((_req, res) => { res.writeHead(404, { 'content-type': 'text/html' }); res.end('remote client'); });

test.beforeAll(async () => {
  await new Promise<void>(done => server.listen(0, '127.0.0.1', done));
  instance = `http://127.0.0.1:${(server.address() as { port: number }).port}/`;
});
test.afterAll(async () => { await new Promise<void>(done => server.close(() => done())); });
test.beforeEach(async () => {
  profile = await mkdtemp(join(tmpdir(), 'dilla-picker-'));
  app = await _electron.launch({ chromiumSandbox: true, args: [resolve('.'), `--profile=${profile}`, '--allow-loopback'], timeout: 30_000 });
});
test.afterEach(async () => { await app?.close(); await rm(profile, { recursive: true, force: true }); });

const answer = (response: number) => app.evaluate(({ dialog }, value) => {
  dialog.showMessageBox = (async () => ({ response: value, checkboxChecked: false })) as unknown as typeof dialog.showMessageBox;
}, response);
const urls = () => app.windows().filter(page => !page.isClosed()).map(page => page.url());

test('Connect closes the picker: exactly one window, the instance', async () => {
  const picker = await app.firstWindow();
  await picker.waitForURL('dilla-internal://setup/');
  await answer(1);
  await picker.locator('#instance').fill(instance);
  await picker.getByRole('button', { name: 'Connect', exact: true }).click();
  await expect.poll(urls).toEqual([instance]);
  expect(await app.evaluate(({ BrowserWindow }) => BrowserWindow.getAllWindows().length)).toBe(1);
});

test('cancelling the confirmation keeps the picker', async () => {
  const picker = await app.firstWindow();
  await picker.waitForURL('dilla-internal://setup/');
  await answer(0);
  await picker.locator('#instance').fill(instance);
  await picker.getByRole('button', { name: 'Connect', exact: true }).click();
  await expect.poll(() => app.evaluate(({ BrowserWindow }) => BrowserWindow.getAllWindows().length)).toBe(1);
  expect(urls()).toEqual(['dilla-internal://setup/']);
});
