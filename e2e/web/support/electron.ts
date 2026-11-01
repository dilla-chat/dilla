import { canonicalInstance } from '../../../packages/desktop/src/policy.ts';
import { _electron, type BrowserContext, type BrowserContextOptions, type LaunchOptions, type Page, type ElectronApplication } from '@playwright/test';
import { mkdtemp, mkdir, rm, access, writeFile } from 'node:fs/promises';
import { once } from 'node:events';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

export const desktopE2E = () => process.env.DILLA_ELECTRON_E2E === '1';
const root = resolve(dirname(fileURLToPath(import.meta.url)), '../../..');
const applications = new WeakMap<BrowserContext, ElectronApplication>();

/** Each browser-profile fixture becomes an actual sandboxed desktop process.
 * The installed client and its worker keep their normal origin/storage boundary. */
export async function launchDesktopContext(profile: string | null, options: BrowserContextOptions & { launchOptions?: LaunchOptions }, claimWindow = true): Promise<BrowserContext> {
 const temporary = profile === null;
 const directory = profile ?? await mkdtemp(join(tmpdir(), 'dilla-client-e2e-'));
 await mkdir(directory, { recursive: true });
 const instance = options.baseURL ?? `http://127.0.0.1:${process.env.DILLA_E2E_PORT ?? '8463'}/`;
 const canonical = canonicalInstance(instance, true);
 try { await access(join(directory, 'desktop.json')); }
 catch { await writeFile(join(directory, 'desktop.json'), JSON.stringify({ version: 1, instances: [canonical], autostart: false })); }
 const app = await _electron.launch({ chromiumSandbox: true, args: [join(root, 'packages/desktop'), `--profile=${directory}`, `--instance=${instance}`, '--allow-loopback', ...(options.launchOptions?.args ?? [])] });
 const child = app.process();
 const ended = once(child, 'close');
 const terminate = () => { if (child.exitCode === null && child.signalCode === null) child.kill('SIGKILL'); };
 process.once('exit', terminate);
 const context = app.context(), first = await app.firstWindow();
 await first.waitForLoadState('load');
 await first.locator('#root').waitFor({ state: 'attached' });
 await first.waitForFunction(() => (document.getElementById('root')?.childElementCount ?? 0) > 0);
 const goto = first.goto.bind(first);
 first.goto = (url, options) => goto(new URL(url, canonical).href, options);
 applications.set(context, app);
 if (options.viewport) await first.setViewportSize(options.viewport);
 let firstTaken = claimWindow, closed = false;
 context.newPage = async (): Promise<Page> => {
  if (!firstTaken && !first.isClosed()) { firstTaken = true; return first; }
  // One window per instance is the product contract. Tests of extra tabs need
  // the corresponding multi-instance/second-device assertion when ported.
  throw new Error('Desktop test requested a second tab for one instance; use a second device profile');
 };
 context.close = async () => {
  if (closed) return; closed = true;
  const process = app.process();
  if (process.exitCode === null && process.signalCode === null) {
   const fallback = setTimeout(() => terminate(), 10000);
   try {
     try { await app.evaluate(({ app }) => { app.exit(0); }); }
     catch (error) { if (!/Target.*closed|browser.*closed/i.test(String(error))) throw error; }
     await ended;
   } finally { clearTimeout(fallback); }
  }
  globalThis.process.removeListener('exit', terminate);
  applications.delete(context);
  if (temporary) await rm(directory, { recursive: true, force: true });
 };
 return context;
}
