import { expect, type Page } from '@playwright/test';
import { desktopE2E } from './electron.ts';

/** Browser invitations enter through the instance's static landing page.
 * Installed clients enter the bundled welcome route, as a validated deep link
 * does; instance-provided HTML never executes inside Electron. */
export async function openInviteInClient(page: Page, link: string, beforeEnter?: () => Promise<void>): Promise<void> {
 const path = new URL(link).pathname;
 const match = /^\/i\/([A-Za-z0-9_-]{1,128})$/.exec(path);
 if (!match) throw new Error('Invalid invitation in client acceptance fixture');
 const welcome = `/welcome?invite=${encodeURIComponent(match[1])}`;
 if (desktopE2E()) {
  await beforeEnter?.();
  await page.goto(welcome);
  await expect(page).toHaveURL(new RegExp('/welcome\\?invite=' + match[1] + '$'));
 } else {
  await page.goto(path);
  const open = page.getByRole('link', { name: 'Open dilla in this browser', exact: true });
  await expect(open).toHaveAttribute('href', welcome);
  await beforeEnter?.();
  await open.click();
 }
}
