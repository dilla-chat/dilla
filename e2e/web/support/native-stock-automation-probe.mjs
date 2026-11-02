// First-stage feasibility probe, intentionally not an acceptance proof. Run
// only on a disposable hosted runner after the ordinary DEB installation.
import { chromium } from '@playwright/test';
import { FuseState, FuseV1Options, getCurrentFuseWire } from '@electron/fuses';
import { spawn } from 'node:child_process';
import { readFile, readlink, rm } from 'node:fs/promises';
import { join } from 'node:path';
import { once } from 'node:events';
import { stockManagerPage } from './native-stock-manager.mjs';

if (process.env.GITHUB_ACTIONS !== 'true' || process.platform !== 'linux' || !process.getuid?.() || !process.env.XDG_CONFIG_HOME)
 throw new Error('Probe requires a non-root disposable GitHub Linux runner and dedicated XDG config');
const executable = '/opt/Dilla/dilla-desktop';
const wire = await getCurrentFuseWire(executable);
for (const key of [FuseV1Options.RunAsNode, FuseV1Options.EnableNodeOptionsEnvironmentVariable, FuseV1Options.EnableNodeCliInspectArguments, FuseV1Options.GrantFileProtocolExtraPrivileges]) if (wire[key] !== FuseState.DISABLE) throw new Error('Unsafe installed fuse');
for (const key of [FuseV1Options.EnableCookieEncryption, FuseV1Options.EnableEmbeddedAsarIntegrityValidation, FuseV1Options.OnlyLoadAppFromAsar]) if (wire[key] !== FuseState.ENABLE) throw new Error('Missing installed fuse');
const portFile = join(process.env.XDG_CONFIG_HOME, 'Dilla', 'DevToolsActivePort');
await rm(portFile, { force: true });
const child = spawn(executable, ['--remote-debugging-address=127.0.0.1', '--remote-debugging-port=0'], { stdio: 'ignore' });
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
 console.log('Stock manager attached; fuse, PID and non-root checks passed. This is not installed acceptance.');
} finally {
 if (browser) await browser.close().catch(() => {});
 if (child.exitCode === null && child.signalCode === null) { child.kill('SIGTERM'); await Promise.race([once(child, 'exit'), new Promise(resolve => setTimeout(resolve, 5000))]); if (child.exitCode === null && child.signalCode === null) child.kill('SIGKILL'); }
}
