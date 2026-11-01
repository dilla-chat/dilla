import type { Page } from '@playwright/test';
import { captureNativeDownload } from '../../../packages/desktop/test/support/download.ts';
import { desktopApplication, desktopE2E } from './electron.ts';

/** Both paths save the actual decrypted attachment; desktop automation chooses
 * the native dialog's destination inside its disposable test profile. */
export async function saveDownload(page: Page, action: () => Promise<unknown>): Promise<{ path: string; filename: string }> {
 const timeout = process.env.CI === 'true' ? 90000 : 30000;
 if (desktopE2E()) return captureNativeDownload(desktopApplication(page.context()), page, action, timeout);
 const [download] = await Promise.all([page.waitForEvent('download', { timeout }), action()]);
 const path = await download.path(); if (!path) throw new Error('Attachment download did not produce a local file');
 return { path, filename: download.suggestedFilename() };
}
