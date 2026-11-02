import { chromium, expect, type Browser, type BrowserContext, type Page } from '@playwright/test';
import { spawn, execFileSync, type ChildProcess } from 'node:child_process';
import { readFile, rm } from 'node:fs/promises';
import { dirname, join } from 'node:path';
import type { DesktopBridge } from '../../../packages/desktop/src/contract.ts';
import { canonicalInstance } from '../../../packages/desktop/src/policy.ts';
import { test as appTest, WAIT, copy } from './app.ts';
import { assertInstalledFuses, createInstalledPackages, type InstalledPackages } from './installed-packages.ts';
import { installedAccountDiagnostics } from './installed-diagnostics.ts';

const pause = () => new Promise(done => setTimeout(done, 100));
interface ProcessReport { pid: number; role: string; start: string | null; version: string; executable: string }
export class InstalledAccountApp {
  readonly packages: InstalledPackages;
  private readonly instance: string;
  private readonly browsers = new Map<number, Browser>();
  private readonly launchers: ChildProcess[] = [];
  private expectedVersion = '1.0.0';
  context!: BrowserContext;
  page!: Page;
  constructor(packages: InstalledPackages, instance: string) { this.packages = packages; this.instance = instance; }
  async launch(version: string): Promise<BrowserContext> {
    await rm(join(this.packages.profile, 'DevToolsActivePort'), { force: true });
    const child = spawn(this.packages.installed, [], { detached: true, stdio: 'ignore', env: { ...process.env, ...(process.platform === 'linux' ? { APPIMAGE: this.packages.installed, APPIMAGE_SILENT_INSTALL: 'true' } : {}) } });
    await new Promise<void>((done, fail) => { child.once('spawn', done); child.once('error', fail); });
    child.unref(); this.launchers.push(child);
    return this.waitForVersion(version);
  }
  async waitForVersion(version: string): Promise<BrowserContext> {
    this.expectedVersion = version;
    const deadline = Date.now() + 180000; let last: unknown;
    while (Date.now() < deadline) {
      try {
        const port = Number((await readFile(join(this.packages.profile, 'DevToolsActivePort'), 'utf8')).split('\n')[0]);
        if (!Number.isInteger(port) || port < 1 || port > 65535) throw new Error('Invalid owned renderer port');
        let browser = this.browsers.get(port);
        if (!browser?.isConnected()) { browser = await chromium.connectOverCDP('http://127.0.0.1:' + port, { timeout: 3000 }); this.browsers.set(port, browser); }
        const page = browser.contexts()[0].pages().find(page => page.url().startsWith(this.instance));
        if (!page) throw new Error('Installed instance window is not ready');
        const info = await page.evaluate(() => (window as unknown as { dillaDesktop: DesktopBridge }).dillaDesktop.info());
        if (info.version !== version) throw new Error('Waiting for installed version ' + version);
        await page.locator('#root').waitFor({ state: 'attached', timeout: 1000 });
        // Attaching CDP must not navigate away from the first worker's storage
        // transaction. This is the same readiness boundary as the native adapter.
        await page.waitForFunction(starting => (document.getElementById('root')?.childElementCount ?? 0) > 0
          && document.querySelector('.d-splash__line')?.textContent !== starting, copy('boot.loading.status'), { timeout: 60000 });
        const sandbox = await page.evaluate(() => ({ require: typeof (window as unknown as { require?: unknown }).require, process: typeof (window as unknown as { process?: unknown }).process }));
        expect(sandbox).toEqual({ require: 'undefined', process: 'undefined' });
        const runtime = (await this.reports()).reverse().find(report => report.role === 'ordinary' && report.version === version);
        if (!runtime || !await this.isOwned(runtime)) throw new Error('No actual owned installed runtime');
        await assertInstalledFuses(process.platform === 'linux' ? '/proc/' + runtime.pid + '/exe' : runtime.executable);
        const goto = page.goto.bind(page);
        page.goto = (url, options) => goto(new URL(url, this.instance).href, options);
        this.context = page.context(); this.page = page;
        return this.context;
      } catch (error) { last = error; }
      await pause();
    }
    throw new Error('Installed renderer did not reach ' + version + ': ' + String(last));
  }
  async failureDiagnostics() {
    return installedAccountDiagnostics({ reports: this.packages.reports, profile: this.packages.profile, installed: this.packages.installed,
      platform: process.platform, expectedVersion: this.expectedVersion, previousPort: this.browsers.keys().next().value });
  }
  async prepareUpdate(): Promise<void> {
    await this.packages.setNext();
    await this.page.evaluate(() => (window as unknown as { dillaDesktop: DesktopBridge }).dillaDesktop.send({ type: 'check-update' }));
    await expect.poll(async () => JSON.parse(await readFile(join(this.packages.reports, 'update-1.0.0.json'), 'utf8')).state.phase, { timeout: 180000 }).toBe('ready');
  }
  async requestUpdate(): Promise<void> {
    // This is the shipping validated renderer capability, not a replaced IPC
    // handler or a direct call into the updater implementation.
    await this.page.evaluate(() => { void (window as unknown as { dillaDesktop: DesktopBridge }).dillaDesktop.send({ type: 'restart-update' }); });
  }
  async expectDraftBlocked(): Promise<void> {
    await rm(join(this.packages.reports, 'protected-quit.json'), { force: true });
    await this.requestUpdate();
    await expect.poll(async () => JSON.parse(await readFile(join(this.packages.reports, 'protected-quit.json'), 'utf8')).version, { timeout: WAIT }).toBe('1.0.0');
    expect((await this.page.evaluate(() => (window as unknown as { dillaDesktop: DesktopBridge }).dillaDesktop.info())).version).toBe('1.0.0');
  }
  private async reports(): Promise<ProcessReport[]> {
    try { return (await readFile(join(this.packages.reports, 'processes.jsonl'), 'utf8')).trim().split('\n').filter(Boolean).map(line => JSON.parse(line) as ProcessReport); }
    catch (error) { if ((error as NodeJS.ErrnoException).code === 'ENOENT') return []; throw error; }
  }
  private async isOwned(report: ProcessReport): Promise<boolean> {
    if (!Number.isSafeInteger(report.pid) || report.pid <= 1 || report.pid === process.pid) return false;
    try {
      if (process.platform === 'linux') {
        const stat = await readFile('/proc/' + report.pid + '/stat', 'utf8'), fields = stat.slice(stat.lastIndexOf(')') + 2).split(' ');
        return report.start === fields[19] && !['Z', 'X'].includes(fields[0]);
      }
      if (process.platform === 'win32') {
        const raw = execFileSync(join(process.env.SystemRoot!, 'System32/WindowsPowerShell/v1.0/powershell.exe'), ['-NoProfile', '-NonInteractive', '-Command', `(Get-CimInstance Win32_Process -Filter 'ProcessId = ${report.pid}').ExecutablePath`], { encoding: 'utf8', windowsHide: true }).trim().toLowerCase();
        return raw.startsWith((dirname(this.packages.installed) + '\\').toLowerCase()) || raw.startsWith((join(this.packages.profile, 'updates') + '\\').toLowerCase());
      }
      return execFileSync('ps', ['-p', String(report.pid), '-o', 'comm='], { encoding: 'utf8' }).includes(this.packages.product);
    } catch (error) { if (['ESRCH', 'ENOENT'].includes((error as NodeJS.ErrnoException).code ?? '')) return false; return false; }
  }
  async stop(): Promise<void> {
    const reports = [...new Map((await this.reports()).map(report => [report.pid, report])).values()].reverse();
    for (const report of reports) {
      if (!await this.isOwned(report)) continue;
      try {
        if (process.platform === 'win32') execFileSync(join(process.env.SystemRoot!, 'System32/taskkill.exe'), ['/PID', String(report.pid), '/T', '/F'], { stdio: 'ignore' });
        else process.kill(report.pid, 'SIGTERM');
      } catch (error) { if (await this.isOwned(report)) throw error; }
    }
    const deadline = Date.now() + 5000;
    while (Date.now() < deadline && (await Promise.all(reports.map(report => this.isOwned(report)))).some(Boolean)) await pause();
    for (const report of reports) if (await this.isOwned(report)) {
      try { process.kill(report.pid, 'SIGKILL'); }
      catch (error) { if (await this.isOwned(report)) throw error; }
    }
    for (const browser of this.browsers.values()) await browser.close().catch(() => {});
    this.browsers.clear();
  }
}

export const test = appTest.extend<{ installed: InstalledAccountApp }>({
  installed: async ({ baseURL }, use, info) => {
    const instance = canonicalInstance(baseURL!, true), packages = await createInstalledPackages(instance), installed = new InstalledAccountApp(packages, instance);
    try { await use(installed); }
    finally {
      try {
        if (info.status !== info.expectedStatus) {
          let timer: ReturnType<typeof setTimeout> | undefined;
          try {
            const snapshot = await Promise.race([installed.failureDiagnostics(), new Promise(resolve => {
              timer = setTimeout(() => resolve({ format: 1, status: 'unavailable' }), 2000);
            })]);
            await info.attach('Installed startup diagnostics', { body: JSON.stringify(snapshot), contentType: 'application/json' });
          } catch { /* Optional diagnostics never replace the original test failure. */ }
          finally { clearTimeout(timer); }
        }
        await installed.stop();
        if (info.status !== info.expectedStatus) {
          for (const [name, file] of [['Installed packaging log', join(packages.root, 'package.log')]]) {
            try { await info.attach(name, { body: await readFile(file), contentType: name.endsWith('error') ? 'application/json' : 'text/plain' }); } catch { /* A report may not have been produced. */ }
          }
        }
      } finally { await packages.dispose(); }
    }
  },
  context: async ({ installed }, use) => { await use(await installed.launch('1.0.0')); },
  relaunch: async ({ installed, guard }, use) => {
    await use(async () => { await guard.collectWorkers(); await installed.stop(); const context = await installed.launch('1.0.1'); await guard.attach(context); return { context, page: installed.page }; });
  },
});
