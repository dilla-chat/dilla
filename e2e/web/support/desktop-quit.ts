import { expect, type BrowserContext } from '@playwright/test';
import { desktopApplication } from './electron.ts';

/** Request an actual native quit. A fresh renderer guard must refuse it while
 * a call or draft is active; merely closing to tray proves a different behavior. */
export async function expectDesktopQuitBlocked(context: BrowserContext): Promise<void> {
 const application = desktopApplication(context);
 await application.evaluate(({ app, dialog }) => {
  const root = globalThis as any;
  root.__dillaProtectedQuit = false;
  root.__dillaProtectedQuitDialog = dialog.showMessageBox.bind(dialog);
  dialog.showMessageBox = (async (...args: any[]) => {
   if (args.at(-1).title === 'Dilla is still in use') { root.__dillaProtectedQuit = true; return { response: 0, checkboxChecked: false }; }
   return root.__dillaProtectedQuitDialog(...args);
  }) as typeof dialog.showMessageBox;
  app.quit();
 });
 try {
  await expect.poll(() => application.evaluate(() => (globalThis as any).__dillaProtectedQuit)).toBe(true);
  expect(context.pages().length).toBeGreaterThan(0);
 } finally {
  await application.evaluate(({ dialog }) => { dialog.showMessageBox = (globalThis as any).__dillaProtectedQuitDialog; delete (globalThis as any).__dillaProtectedQuitDialog; delete (globalThis as any).__dillaProtectedQuit; });
 }
}
