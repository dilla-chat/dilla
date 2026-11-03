import type { BrowserContext, Page } from '@playwright/test';
import { desktopApplication } from '../../web/support/electron.ts';

/** Observe real Electron Notification objects and OS delivery errors. Neither
 * the constructor nor show/close delivery is substituted with a fake toast. */
export async function observeNativeNotifications(context: BrowserContext): Promise<void> {
 await desktopApplication(context).evaluate(({ Notification }) => {
  const records: { title: string; tag: string; closed: boolean; error?: string }[] = [];
  (globalThis as any).__dillaNativeNotices = records;
  const entries = new WeakMap<Electron.Notification, (typeof records)[number]>();
  const show = Notification.prototype.show;
  const close = Notification.prototype.close;
  Notification.prototype.show = function () {
   const record = { title: this.title, tag: '', closed: false, error: undefined as string | undefined };
   records.push(record);
   entries.set(this, record);
   this.once('close', () => { record.closed = true; });
   this.once('failed', (_event, error) => { record.error = error; });
   return show.call(this);
  };
  Notification.prototype.close = function () {
   const result = close.call(this), record = entries.get(this);
   if (record) record.closed = true;
   return result;
  };
 });
}

export async function nativeNotificationsSeen(page: Page): Promise<{ title: string; tag: string; closed: boolean }[]> {
 return desktopApplication(page.context()).evaluate(() => {
  const records = (globalThis as any).__dillaNativeNotices as { title: string; tag: string; closed: boolean; error?: string }[];
  const failed = records.find(record => record.error);
  if (failed) throw new Error('Native notification delivery failed: ' + failed.error);
  return records.map(({ title, tag, closed }) => ({ title, tag, closed }));
 });
}
