// v0.1.0-web3.8, the sign-in order of R6 and R7 from an empty instance: the password and the second factor first, the
// recovery key last. Like second-factor.spec.ts this suite starts a test host of its own with no account, and goes from there
// with the shipping client alone. Ada signs up with an authenticator app; a second browser signs in with the code, meets a
// pasted backup code in the key field (R44 of web3.7), then the key; a third takes the backup-code path, and the account's
// backup codes count one fewer only once the key has added that browser (the code waits in the page until then). Nothing
// here seeds state: the codes are computed from the setup key the page shows, for the host's clock (support/totp.ts).
// Controls are found by role and accessible name, with the copy keys of flows 03, 10 and 13 through copy().
import { expect, type Locator, type Page } from '@playwright/test';
import {
  COPY, SIGNIN, TEST_TIMEOUT, WAIT, completeFactorStep, copy, expectAccessible, expectHeading, expectShell, keyField, keySubmit,
  keyText, newAccount, nextButton, readRecoveryKey, submitButton, test, type Account,
  settingsGear,
} from './support/app';
import { startTestHost, type HostConfig, type StartedHost } from './support/host';
import { launchSecond, secondProfileDir } from './support/second-harness';
import { nextCode } from './support/totp';
import { arr, decode } from '../../packages/client-core/src/cbor/index';
import { rmSync } from 'node:fs';

/** Its own ports (web3.8 reservation). */
const EMPTY: HostConfig = { port: 8513, control: 8514, webRoot: 'packages/web/dist' };

let host: StartedHost | null = null;
let instance = '';

test.describe.configure({ mode: 'serial', timeout: TEST_TIMEOUT });
test.use({ baseURL: `http://127.0.0.1:${String(EMPTY.port)}` });

test.beforeAll(async () => {
  host = await startTestHost(EMPTY);
  const res = await fetch(`${host.url}/v1/instance`, { headers: { accept: 'application/cbor' } });
  expect(res.status, 'GET /v1/instance').toBe(200);
  const name = arr(decode(new Uint8Array(await res.arrayBuffer())))[5];
  if (typeof name !== 'string' || name === '') throw new Error('GET /v1/instance: element 5 is not the instance name');
  instance = name;
});
test.afterAll(async () => { await host?.stop(); });

function started(): StartedHost {
  if (host === null) throw new Error('the empty test host did not start');
  return host;
}
const button = (scope: Page | Locator, key: string) => scope.getByRole('button', { name: copy(key), exact: true });
// Signal task 10: the step line counts the steps this ceremony shows (four with a code step).
const stepLine = (page: Page, n: number) => page.getByText(copy(SIGNIN.step, { n, total: 4 }), { exact: true });

/** Settings → Sign-in on the first browser: the section reads the account's factors again each time it opens. */
async function backupCodesLeft(page: Page, count: number): Promise<void> {
  await settingsGear(page).click();
  const dialog = page.getByRole('dialog', { name: copy('settings.title'), exact: true });
  await expect(dialog).toBeVisible({ timeout: WAIT });
  await dialog.getByRole('navigation', { name: copy('settings.nav.label'), exact: true })
    .getByRole('button', { name: copy('factorSettings.nav'), exact: true }).click();
  await expect(page).toHaveURL(/\/settings\/signin$/, { timeout: WAIT });
  await expect(dialog.getByText(copy('factorSettings.codes.other', { count }), { exact: true })).toBeVisible({ timeout: WAIT });
  await page.keyboard.press('Escape');
  await expect(dialog).toBeHidden({ timeout: WAIT });
}

/** From a fresh profile: onboarding step 1's entry, then the login step, which comes first (R6). */
async function toLoginStep(p: Page, account: Account): Promise<void> {
  await p.goto('/welcome');
  await expectHeading(p, COPY.connectTitle, { instance });
  await button(p, SIGNIN.entry).click();
  await expectHeading(p, SIGNIN.loginTitle, { instance });
  await expect(stepLine(p, 1)).toBeVisible();
  await expect(p.getByRole('textbox', { name: copy(SIGNIN.keyLabel), exact: true })).toHaveCount(0);
  await expectAccessible(p, 'sign-in order: login');
  await p.getByLabel(copy(SIGNIN.username), { exact: true }).fill(account.username);
  await p.getByLabel(copy(SIGNIN.password), { exact: true }).fill(account.password);
  await button(p, SIGNIN.loginSubmit).click();
}

test('sign-in order: the password, then the factor, then the recovery key; a backup code waits for the key', async ({
  page, guard, playwright, browserName, launchOptions, headless, channel, viewport,
}, testInfo) => {
  const h = started();

  // 1. Ada signs up with the bootstrap invite and an authenticator app.
  const ada = newAccount('Ada', h.control);
  await page.goto(`/welcome?invite=${encodeURIComponent(h.invite.split(',')[0] ?? '')}`);
  await expectHeading(page, COPY.connectTitle, { instance });
  await nextButton(page).click();
  await expectHeading(page, COPY.identityTitle);
  await page.getByLabel(copy(COPY.usernameLabel), { exact: true }).fill(ada.username);
  await page.getByLabel(copy(COPY.displayLabel), { exact: true }).fill(ada.display);
  await page.getByLabel(copy(COPY.passwordLabel), { exact: true }).fill(ada.password);
  await nextButton(page).click();
  await expectHeading(page, COPY.keysTitle);
  ada.recoveryKey = await readRecoveryKey(page);
  await page.getByRole('checkbox', { name: copy(COPY.keysAcknowledge), exact: true }).check();
  await nextButton(page).click();
  await expectHeading(page, COPY.deviceTitle);
  await submitButton(page).click();
  await completeFactorStep(page, ada);
  expect(ada.backupCodes).toHaveLength(10);
  await expectHeading(page, COPY.doneTitle);
  await page.getByRole('button', { name: copy(COPY.finish), exact: true }).click();
  await expectShell(page);
  await backupCodesLeft(page, 10);

  /** One more browser with a persistent profile of its own, closed and removed whatever the body did. */
  const another = async (body: (p: Page) => Promise<void>): Promise<void> => {
    const dir = secondProfileDir(testInfo.project.name, testInfo.workerIndex);
    const context = await launchSecond(playwright, browserName, { launchOptions, headless, channel, viewport, baseURL: h.url }, dir);
    try {
      await guard.attach(context);
      await body(context.pages()[0] ?? (await context.newPage()));
    } finally {
      await context.close();
      rmSync(dir, { recursive: true, force: true });
    }
  };

  // 2-6. A second browser: the login first, then the code (step 2), then the key (step 3), then done.
  await another(async (p) => {
    await toLoginStep(p, ada);
    await expectHeading(p, 'signin.totp.title');
    await expect(stepLine(p, 2)).toBeVisible();
    await expectAccessible(p, 'sign-in order: code');
    await p.getByLabel(copy('signin.totp.code'), { exact: true }).fill(await nextCode(ada.control, ada));
    await button(p, SIGNIN.loginSubmit).click();

    await expectHeading(p, SIGNIN.keyTitle);
    await expect(stepLine(p, 3)).toBeVisible();
    await expect(p.getByText(copy('signin.key.body'), { exact: true })).toBeVisible();
    await expectAccessible(p, 'sign-in order: key');

    // 5. A backup code pasted into the key field is named, once it has rested 600 ms (R44 of web3.7).
    // The note under the field (the polite announcer repeats it once, by design).
    const note = p.locator('#signin-key-backup').filter({ hasText: copy('signIn.key.isBackupCode') });
    const field = keyField(p);
    await field.focus();
    const pasted = Date.now();
    await field.fill(ada.backupCodes[1] ?? '');
    await expect(note).toBeVisible({ timeout: WAIT });
    expect(Date.now() - pasted, 'the note waits 600 ms of rest').toBeGreaterThanOrEqual(600);
    await expect(field).toBeFocused();
    await expect(keySubmit(p)).toHaveAttribute('aria-disabled', 'true');
    await expectAccessible(p, 'sign-in order: a backup code in the key field');

    // 6. The key, then done.
    await field.fill(keyText(ada));
    await expect(note).toBeHidden();
    await keySubmit(p).click();
    await expectHeading(p, SIGNIN.doneTitle);
    await expect(stepLine(p, 4)).toBeVisible();
    await expectAccessible(p, 'sign-in order: done');
    await button(p, SIGNIN.doneNext).click();
    await expectShell(p);
  });

  // 7. A third browser takes the backup-code path: login, a backup code, then the key. The code waits in the page while the
  //    key is typed, so the account still holds ten codes until the key adds this browser; then nine.
  await another(async (p) => {
    await toLoginStep(p, ada);
    await expectHeading(p, 'signin.totp.title');
    await button(p, 'factorSignin.useBackup').click();
    await expectHeading(p, 'factorSignin.backup.title');
    await expect(stepLine(p, 2)).toBeVisible();
    await expectAccessible(p, 'sign-in order: backup code');
    await p.getByLabel(copy('factorSignin.backup.label'), { exact: true }).fill(ada.backupCodes[0] ?? '');
    await button(p, SIGNIN.loginSubmit).click();

    await expectHeading(p, SIGNIN.keyTitle);
    await expect(stepLine(p, 3)).toBeVisible();
    await expectAccessible(p, 'sign-in order: key after a backup code');
    await backupCodesLeft(page, 10);

    await keyField(p).fill(keyText(ada));
    await keySubmit(p).click();
    await expectHeading(p, SIGNIN.doneTitle);
    await expect(p.getByText(copy('factorSignin.left.other', { count: 9 }), { exact: true })).toBeVisible({ timeout: WAIT });
    await backupCodesLeft(page, 9);
    await button(p, SIGNIN.doneNext).click();
    await expectShell(p);
  });
});
