// First-stage feasibility probe, intentionally not an acceptance proof. Run
// only on a disposable hosted runner after the ordinary DEB installation.
import { chromium } from '@playwright/test';
import { FuseState, FuseV1Options, getCurrentFuseWire } from '@electron/fuses';
import { spawn } from 'node:child_process';
import { readFile, readlink, rm, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { once } from 'node:events';
import { stockManagerPage } from './native-stock-manager.mjs';
import { confirmOwnedConnect } from './native-stock-connect.mjs';
import { observeRendererSandbox } from './native-stock-sandbox.mjs';

if (process.env.GITHUB_ACTIONS !== 'true' || process.platform !== 'linux' || !process.getuid?.() || !process.env.XDG_CONFIG_HOME)
 throw new Error('Probe requires a non-root disposable GitHub Linux runner and dedicated XDG config');
const origin = process.env.DILLA_NATIVE_TEST_ORIGIN, instanceName = process.env.DILLA_NATIVE_TEST_INSTANCE_NAME;
if (!origin || !/^https:\/\/127\.0\.0\.1:[1-9]\d{0,4}\/$/.test(origin) || new URL(origin).port === '' || Number(new URL(origin).port) > 65535 || !instanceName || instanceName.length > 256)
 throw new Error('Expected the disposable HTTPS test instance');
const executable = '/opt/Dilla/dilla-desktop';
const wire = await getCurrentFuseWire(executable);
for (const key of [FuseV1Options.RunAsNode, FuseV1Options.EnableNodeOptionsEnvironmentVariable, FuseV1Options.EnableNodeCliInspectArguments, FuseV1Options.GrantFileProtocolExtraPrivileges]) if (wire[key] !== FuseState.DISABLE) throw new Error('Unsafe installed fuse');
for (const key of [FuseV1Options.EnableCookieEncryption, FuseV1Options.EnableEmbeddedAsarIntegrityValidation, FuseV1Options.OnlyLoadAppFromAsar]) if (wire[key] !== FuseState.ENABLE) throw new Error('Missing installed fuse');
const portFile = join(process.env.XDG_CONFIG_HOME, 'Dilla', 'DevToolsActivePort');
await rm(portFile, { force: true });
const child = spawn('/usr/bin/dilla-desktop', ['--remote-debugging-address=127.0.0.1', '--remote-debugging-port=0'], { stdio: 'ignore' });
let browser;
try {
 await once(child, 'spawn');
 const deadline = Date.now() + 90000; let port;
 while (Date.now() < deadline) {
  if (child.exitCode !== null || child.signalCode !== null) throw new Error('Stock app exited before automation attached');
  try { port = Number((await readFile(portFile, 'utf8')).split('\n')[0]); if (Number.isInteger(port) && port > 0 && port < 65536) break; } catch (error) { if (error.code !== 'ENOENT') throw error; }
  await new Promise(resolve => setTimeout(resolve, 200));
 }
 if (!port) throw new Error('Stock app exposed no DevToolsActivePort');
 if (await readlink(`/proc/${child.pid}/exe`) !== executable) throw new Error('Unexpected installed executable');
 const status = await readFile(`/proc/${child.pid}/status`, 'utf8');
 if (!new RegExp(`^Uid:\\s+${process.getuid()}\\s`, 'm').test(status)) throw new Error('Unexpected app user');
 browser = await chromium.connectOverCDP(`http://127.0.0.1:${port}`, { timeout: 10000 });
 const deadlinePage = Date.now() + 30000; let page;
 while (Date.now() < deadlinePage && !page) { page = stockManagerPage(browser); if (!page) await new Promise(resolve => setTimeout(resolve, 200)); }
 if (!page) throw new Error('Stock manager window did not become observable');
 await page.locator('#instance').waitFor({ timeout: 10000 });
 if (await page.locator('#instances li').count() !== 0) throw new Error('Expected a fresh default instance profile');
 await page.locator('#instance').fill(origin);
 await page.getByRole('button', { name: 'Connect', exact: true }).click({ noWaitAfter: true });
 await confirmOwnedConnect(child.pid);
 const deadlineInstance = Date.now() + 90000; let instance;
 while (Date.now() < deadlineInstance && !instance) {
  const contexts = browser.contexts();
  if (contexts.length !== 1) throw new Error('Unexpected browser context');
  instance = contexts[0].pages().find(candidate => candidate.url() === origin);
  if (!instance) await new Promise(resolve => setTimeout(resolve, 200));
 }
 if (!instance) throw new Error('Stock app did not open the selected instance');
 // Connecting replaces the picker: the instance is the only window left, so the picker page is no longer there to observe.
 if (browser.contexts()[0].pages().length !== 1) throw new Error('Expected the picker to close when the instance opened');
 await instance.getByRole('heading', { name: `Join ${instanceName}`, exact: true }).waitFor({ state: 'visible', timeout: 90000 });
 if (process.env.DILLA_NATIVE_ACCEPTANCE_OUTPUT) {
  const sandbox = await observeRendererSandbox(browser, child.pid, [instance]);
  await writeFile(process.env.DILLA_NATIVE_ACCEPTANCE_OUTPUT, JSON.stringify({ httpsOnboarding: true, sandbox }) + '\n', { flag: 'wx', mode: 0o644 });
  console.log('Installed stock client reached HTTPS onboarding with observed renderer sandboxing.');
 } else console.log('Stock client reached its real test instance onboarding. This is not installed acceptance.');
} finally {
 if (browser) await browser.close().catch(() => {});
 if (child.exitCode === null && child.signalCode === null) { child.kill('SIGTERM'); await Promise.race([once(child, 'exit'), new Promise(resolve => setTimeout(resolve, 5000))]); if (child.exitCode === null && child.signalCode === null) child.kill('SIGKILL'); }
}
