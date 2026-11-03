import type { ElectronApplication, Page } from '@playwright/test';

/** Electron owns native window geometry. CDP viewport emulation alone leaves
 * an unpainted strip inside a larger BrowserWindow, so never use it here. */
export async function useNativeViewport(application: ElectronApplication, page: Page): Promise<void> {
 let size = await page.evaluate(() => ({ width: innerWidth, height: innerHeight }));
 const binding = '__dillaAcceptanceViewport';
 await page.exposeBinding(binding, (_source, next: { width: number; height: number }) => { size = next; });
 const observe = () => {
  const report = () => { void (window as any).__dillaAcceptanceViewport({ width: innerWidth, height: innerHeight }).catch(() => {}); };
  window.addEventListener('resize', report); report();
 };
 await page.addInitScript(observe); await page.evaluate(observe);
 page.viewportSize = () => size;
 page.setViewportSize = async requested => {
  const window = await application.browserWindow(page);
  const actual = await window.evaluate((win, requested) => {
   const minimum = win.getMinimumSize();
   win.setContentSize(Math.max(minimum[0], requested.width), Math.max(minimum[1], requested.height));
   return win.getContentSize();
  }, requested);
  await window.dispose();
  await page.waitForFunction(([width, height]) => innerWidth === width && innerHeight === height, actual);
  size = { width: actual[0], height: actual[1] };
 };
}
