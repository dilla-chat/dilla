import { expect, type BrowserContext } from '@playwright/test';
import { desktopApplication } from './electron.ts';

/** Request an actual native quit while a call or draft is active. A fresh renderer guard report makes the app ask once
 * ("Quit Dilla?", Cancel / Quit, Cancel the default); the person answers Cancel, and the app, its windows, the call and the
 * draft all stay. A quit that does not ask (the guard missed the call or draft) closes the app and fails here; merely closing
 * to tray proves a different behavior. Quit-then-quits is covered by packages/desktop/test/quit-confirm.test.ts. */
export async function expectDesktopQuitCancelled(context: BrowserContext): Promise<void> {
 const application = desktopApplication(context);
 await application.evaluate(({ app, dialog }) => {
  const root = globalThis as any;
  root.__dillaQuitAsked = null;
  root.__dillaQuitDialog = dialog.showMessageBox.bind(dialog);
  dialog.showMessageBox = (async (...args: any[]) => {
   const options = args.at(-1);
   if (options.title === 'Quit Dilla?') {
    root.__dillaQuitAsked = { buttons: options.buttons, defaultId: options.defaultId, cancelId: options.cancelId };
    return { response: options.cancelId, checkboxChecked: false };
   }
   return root.__dillaQuitDialog(...args);
  }) as typeof dialog.showMessageBox;
  app.quit();
 });
 try {
  await expect.poll(() => application.evaluate(() => (globalThis as any).__dillaQuitAsked)).toEqual({ buttons: ['Cancel', 'Quit'], defaultId: 0, cancelId: 0 });
  expect(context.pages().length).toBeGreaterThan(0);
 } finally {
  await application.evaluate(({ dialog }) => { dialog.showMessageBox = (globalThis as any).__dillaQuitDialog; delete (globalThis as any).__dillaQuitDialog; delete (globalThis as any).__dillaQuitAsked; });
 }
}
