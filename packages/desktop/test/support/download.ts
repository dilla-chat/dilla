import type { ElectronApplication, Page } from '@playwright/test';
import { mkdir } from 'node:fs/promises';
import { join } from 'node:path';
import { randomUUID } from 'node:crypto';

/** Native Electron partitions are not CDP-created browser contexts, so
 * Playwright cannot emit its browser Download object for them. Automate only
 * the save-path choice, then wait for the actual DownloadItem completion. */
export async function captureNativeDownload(application: ElectronApplication, page: Page, action: () => Promise<unknown>, timeoutMs = 30000): Promise<{ path: string; filename: string }> {
 const key = randomUUID();
 const profile = await application.evaluate(({ app }) => app.getPath('userData'));
 const directory = join(profile, 'acceptance-downloads'); await mkdir(directory, { recursive: true });
 const path = join(directory, key);
 const window = await application.browserWindow(page), windowID = await window.evaluate(win => win.id); await window.dispose();
 await application.evaluate(({ BrowserWindow }, { key, path, windowID }) => {
  const win = BrowserWindow.fromId(windowID); if (!win) throw new Error('Download window closed');
  const session = win.webContents.session, origin = new URL(win.webContents.getURL()).origin;
  const entries: Map<string, any> = (globalThis as any).__dillaAcceptanceDownloads ??= new Map();
  const entry: any = { result: null };
  const handler = (_event: Electron.Event, item: Electron.DownloadItem, contents: Electron.WebContents) => {
   if (entry.item || contents !== win.webContents || !item.getURL().startsWith(`blob:${origin}/`)) return;
   entry.item = item; item.setSavePath(path);
   item.once('done', (_event, state) => { entry.result = { state, path: item.getSavePath(), filename: item.getFilename() }; });
  };
  session.on('will-download', handler);
  entry.dispose = () => { session.removeListener('will-download', handler); if (entry.item && !entry.result) entry.item.cancel(); };
  entries.set(key, entry);
 }, { key, path, windowID });
 try {
  await action();
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
   const result = await application.evaluate((_electron, key) => (globalThis as any).__dillaAcceptanceDownloads.get(key)?.result, key) as { state: string; path: string; filename: string } | null;
   if (result) {
    if (result.state !== 'completed' || result.path !== path) throw new Error('Native attachment download failed: ' + result.state);
    return { path: result.path, filename: result.filename };
   }
   await new Promise(done => setTimeout(done, 50));
  }
  throw new Error('Native attachment download did not complete');
 } finally {
  await application.evaluate((_electron, key) => { const entries = (globalThis as any).__dillaAcceptanceDownloads; entries.get(key)?.dispose(); entries.delete(key); }, key).catch(() => {});
 }
}
