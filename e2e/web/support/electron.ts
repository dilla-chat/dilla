import { useNativeViewport } from '../../../packages/desktop/test/support/viewport.ts';
import { canonicalInstance } from '../../../packages/desktop/src/policy.ts';
import { _electron, type BrowserContext, type BrowserContextOptions, type LaunchOptions, type Page, type ElectronApplication } from '@playwright/test';
import { mkdtemp, mkdir, rm, access, writeFile } from 'node:fs/promises';
import { once } from 'node:events';
import { spawnSync } from 'node:child_process';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { en } from '../../../packages/web/src/strings/en.ts';
import { desktopNetwork } from './desktop-network.ts';

export const desktopE2E = () => process.env.DILLA_ELECTRON_E2E === '1';
const root = resolve(dirname(fileURLToPath(import.meta.url)), '../../..');
const applications = new WeakMap<BrowserContext, ElectronApplication>();
const suspensions = new WeakMap<BrowserContext, () => Promise<() => Promise<BrowserContext>>>();
/** Stop the real process, retaining its encrypted device store for a later launch. */
export async function suspendDesktopContext(context: BrowserContext): Promise<() => Promise<BrowserContext>> {
 const suspend = suspensions.get(context); if (!suspend) throw new Error('No desktop profile owns this context');
 return suspend();
}
export function desktopApplication(context: BrowserContext): ElectronApplication {
 const application = applications.get(context); if (!application) throw new Error('No desktop application owns this test context');
 return application;
}

/** Each browser-profile fixture becomes an actual sandboxed desktop process.
 * The installed client and its worker keep their normal origin/storage boundary. */
export async function launchDesktopContext(profile: string | null, options: BrowserContextOptions & { launchOptions?: LaunchOptions }, claimWindow = true, temporary = profile === null): Promise<BrowserContext> {
 const directory = profile ?? await mkdtemp(join(tmpdir(), 'dilla-client-e2e-'));
 await mkdir(directory, { recursive: true });
 const instance = options.baseURL ?? `http://127.0.0.1:${process.env.DILLA_E2E_PORT ?? '8463'}/`;
 const canonical = canonicalInstance(instance, true);
 try { await access(join(directory, 'desktop.json')); }
 catch { await writeFile(join(directory, 'desktop.json'), JSON.stringify({ version: 1, instances: [canonical], autostart: false })); }
 const app = await _electron.launch({ chromiumSandbox: true, args: [join(root, 'packages/desktop'), `--profile=${directory}`, `--instance=${instance}`, '--allow-loopback', ...(options.launchOptions?.args ?? [])] });
 const child = app.process();
 const ended = once(child, 'close');
 const terminate = () => {
  if (child.exitCode !== null || child.signalCode !== null) return;
  // The Windows launcher is cmd.exe, not the Electron PID. Terminate only this
  // owned launcher's tree if graceful test teardown could not reach the app.
  if (process.platform === 'win32' && child.pid) spawnSync('taskkill', ['/PID', String(child.pid), '/T', '/F'], { stdio: 'ignore' });
  else child.kill('SIGKILL');
 };
 process.once('exit', terminate);
 const context = app.context(), first = await app.firstWindow();
 // Navigation in these specs is an explicit person action. Answer the native
 // reload confirmation without racing it through CDP's browser-dialog adapter.
 // Native shell acceptance separately proves both Stay and Reload choices.
 first.on('dialog', () => {});
 await app.evaluate(({ dialog }, permissions) => {
  const original = dialog.showMessageBoxSync.bind(dialog);
  dialog.showMessageBoxSync = ((...args: any[]) => args.at(-1).title === 'Reload Dilla?' ? 1 : (original as any)(...args)) as typeof dialog.showMessageBoxSync;
  const asynchronous = dialog.showMessageBox.bind(dialog);
  dialog.showMessageBox = (async (...args: any[]) => {
   const options = args.at(-1);
   if (options.title === 'Dilla permission') {
    const names = options.message.includes('microphone or camera') ? ['microphone', 'camera']
     : options.message.includes('notifications') ? ['notifications']
     : options.message.includes('local network') ? ['local-network']
     : options.message.includes('local Dilla') ? ['loopback-network'] : [];
    return { response: names.some(name => permissions.includes(name)) ? 1 : 0, checkboxChecked: false };
   }
   return (asynchronous as any)(...args);
  }) as typeof dialog.showMessageBox;
 }, [...(options.permissions ?? []), 'local-network', 'loopback-network', ...(options.launchOptions?.args?.includes('--use-fake-ui-for-media-stream') ? ['microphone', 'camera'] : [])]);
 await first.waitForLoadState('load');
 await first.locator('#root').waitFor({ state: 'attached' });
 // A real desktop already started its worker before the fixture attaches.
 // Do not tear down its first keystore transaction with a second navigation.
 await first.waitForFunction(starting => (document.getElementById('root')?.childElementCount ?? 0) > 0
  && document.querySelector('.d-splash__line')?.textContent !== starting, en['boot.loading.status']);
 const goto = first.goto.bind(first);
 first.goto = (url, options) => goto(new URL(url, canonical).href, options);
 // Comparative performance uses the unwrapped production transport. Fault
 // injection/HTTP event forwarding belong only to the behavioral test suite.
 const stopNetwork = process.env.DILLA_PERFORMANCE_CLIENT === 'desktop' ? async () => {} : await desktopNetwork(context, app, first);
 applications.set(context, app);
 // Startup/authentication keep their real compact native bounds. Specs can
 // resize the workspace explicitly; native minimums remain authoritative.
 await useNativeViewport(app, first);
 let requestedWorkspace = options.viewport ?? null, appliedWorkspace: typeof requestedWorkspace = null;
 const resize = first.setViewportSize.bind(first);
 first.setViewportSize = async requested => {
  requestedWorkspace = requested;
  if (await first.evaluate(() => document.documentElement.dataset.desktopWindow === 'workspace')) {
   await resize(requested); appliedWorkspace = requested;
  }
 };
 await first.addLocatorHandler(first.locator('body'), async () => {
  const win = await app.browserWindow(first);
  try { await win.evaluate(win => { if (win.isMinimized()) win.restore(); win.show(); win.focus(); }); }
  finally { await win.dispose(); }
  await first.waitForFunction(() => document.hasFocus());
  if (requestedWorkspace && appliedWorkspace !== requestedWorkspace
    && await first.evaluate(() => document.documentElement.dataset.desktopWindow === 'workspace')) {
   await resize(requestedWorkspace); appliedWorkspace = requestedWorkspace;
  }
 }, { noWaitAfter: true });
 let firstTaken = claimWindow, closed = false, retainProfile = false;
 suspensions.set(context, async () => {
  retainProfile = true;
  await context.close();
  return () => launchDesktopContext(directory, options, false, temporary);
 });
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
  suspensions.delete(context);
  await stopNetwork();
  if (temporary && !retainProfile) await rm(directory, { recursive: true, force: true });
 };
 return context;
}
