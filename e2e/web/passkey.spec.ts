// v0.1.0-web3.6, passkeys from an empty instance (Chromium only: a CDP virtual authenticator stands in for the person's
// device). The test host runs with -webauthn-origin so the relying party matches the page's hostname, localhost. Björn signs up
// with a passkey, a second browser holding a copy of it signs in with no password typed, and Settings → Sign-in adds the
// authenticator app and removes the passkey, each with a step-up. Nothing here seeds state; the virtual authenticator is the
// only stand-in, and the person's real device would answer the same ceremony. Controls are found by role and accessible name,
// with the copy keys of flow 10 through copy().
import { expect, type Locator, type Page } from '@playwright/test';
import {
  COPY, SIGNIN, TEST_TIMEOUT, WAIT, copy, escapeRegExp, expectAccessible, expectHeading, expectShell, keyField, keySubmit, keyText,
  newAccount, nextButton, readRecoveryKey, submitButton, test,
  settingsGear,
} from './support/app';
import { startTestHost, type HostConfig, type StartedHost } from './support/host';
import { launchSecond, secondProfileDir } from './support/second-harness';
import { nextCode } from './support/totp';
import { addAuthenticator, copyCredentials } from './support/webauthn';
import { arr, decode } from '../../packages/client-core/src/cbor/index';
import { rmSync } from 'node:fs';

/** Its own ports (E2 reservation). The page is opened at localhost, the relying party's origin. */
const EMPTY: HostConfig = { port: 8497, control: 8498, webRoot: 'packages/web/dist', webauthnOrigin: 'http://localhost:8497' };
const BJORN = 'Björn';
const PASSKEY = 'Laptop';

let host: StartedHost | null = null;
let instance = '';

test.describe.configure({ mode: 'serial', timeout: TEST_TIMEOUT });
test.use({ baseURL: 'http://localhost:8497' });
test.skip(({ browserName }) => browserName !== 'chromium', 'CDP virtual authenticator');

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
const button = (scope: Page | Locator, key: string, vars?: Record<string, string | number>) =>
  scope.getByRole('button', { name: copy(key, vars), exact: true });
const dialogOf = (page: Page, key: string, vars?: Record<string, string | number>) =>
  page.getByRole('dialog', { name: copy(key, vars), exact: true });

/** Settings → Sign-in: the user panel's gear, then the section from the settings navigation. */
async function openSignIn(page: Page): Promise<Locator> {
  await settingsGear(page).click();
  const dialog = page.getByRole('dialog', { name: copy('settings.title'), exact: true });
  await expect(dialog).toBeVisible({ timeout: WAIT });
  await dialog.getByRole('navigation', { name: copy('settings.nav.label'), exact: true })
    .getByRole('button', { name: copy('factorSettings.nav'), exact: true }).click();
  await expect(page).toHaveURL(/\/settings\/signin$/, { timeout: WAIT });
  await expect(dialog.getByRole('heading', { name: copy('factorSettings.title'), exact: true })).toBeVisible({ timeout: WAIT });
  return dialog;
}

/** The step-up dialog: the password, Confirm. Resolves once the dialog is gone. */
async function stepUp(page: Page, password: string): Promise<void> {
  const dialog = dialogOf(page, 'factorSettings.stepUp.title');
  await expect(dialog).toBeVisible({ timeout: WAIT });
  await dialog.getByLabel(copy('factorSettings.stepUp.password'), { exact: true }).fill(password);
  await button(dialog, 'factorSettings.stepUp.confirm').click();
  await expect(dialog).toBeHidden({ timeout: WAIT });
}

test('passkeys: sign up with one, sign in with a copy of it, add the app, remove the passkey', async ({
  page, guard, playwright, browserName, launchOptions, headless, channel, viewport,
}, testInfo) => {
  const h = started();

  // 1. Björn signs up with a passkey named "Laptop", then the backup codes, then the shell.
  const authenticator = await addAuthenticator(page);
  const bjorn = newAccount(BJORN, h.control);
  await page.goto(`/welcome?invite=${encodeURIComponent(h.invite.split(',')[0] ?? '')}`);
  await expectHeading(page, COPY.connectTitle, { instance });
  await nextButton(page).click();
  await expectHeading(page, COPY.identityTitle);
  await page.getByLabel(copy(COPY.usernameLabel), { exact: true }).fill(bjorn.username);
  await page.getByLabel(copy(COPY.displayLabel), { exact: true }).fill(bjorn.display);
  await page.getByLabel(copy(COPY.passwordLabel), { exact: true }).fill(bjorn.password);
  await nextButton(page).click();
  await expectHeading(page, COPY.keysTitle);
  bjorn.recoveryKey = await readRecoveryKey(page);
  await page.getByRole('checkbox', { name: copy(COPY.keysAcknowledge), exact: true }).check();
  await nextButton(page).click();
  await expectHeading(page, COPY.deviceTitle);
  await submitButton(page).click();
  await expectHeading(page, 'factor.choose.title');
  await page.getByRole('radio', { name: new RegExp(escapeRegExp(copy('factor.choose.passkey'))) }).check();
  await page.getByRole('button', { name: copy('factor.choose.next'), exact: true }).click();
  await expectHeading(page, 'factor.passkey.title');
  const name = page.getByLabel(copy('factor.passkey.name'), { exact: true });
  await name.fill(PASSKEY);
  await expectAccessible(page, 'passkey: name');
  await button(page, 'factor.passkey.submit').click();
  await expectHeading(page, 'factor.codes.title');
  bjorn.backupCodes = (await page.getByRole('list', { name: copy('factor.codes.label'), exact: true }).locator('li').allInnerTexts())
    .map((c) => c.replace(/\s+/g, ''));
  expect(bjorn.backupCodes).toHaveLength(10);
  await page.getByRole('checkbox', { name: copy('factor.codes.ack'), exact: true }).check();
  await page.getByRole('button', { name: copy('factor.codes.next'), exact: true }).click();
  await expectHeading(page, COPY.doneTitle);
  await page.getByRole('button', { name: copy(COPY.finish), exact: true }).click();
  await expectShell(page);

  // 2. A second browser holding a copy of the passkey: the passkey on the login step, no password typed; then the key.
  const secondDir = secondProfileDir(testInfo.project.name, testInfo.workerIndex);
  const second = await launchSecond(playwright, browserName, { launchOptions, headless, channel, viewport, baseURL: 'http://localhost:8497' }, secondDir);
  try {
    await guard.attach(second);
    const page2 = second.pages()[0] ?? (await second.newPage());
    const authenticator2 = await addAuthenticator(page2);
    expect(await copyCredentials(authenticator, authenticator2)).toBe(1);
    await page2.goto('/welcome');
    await expectHeading(page2, COPY.connectTitle, { instance });
    await page2.getByRole('button', { name: copy(SIGNIN.entry), exact: true }).click();
    await expectHeading(page2, SIGNIN.loginTitle, { instance });
    await expect(page2.getByLabel(copy(SIGNIN.password), { exact: true })).toHaveValue('');
    await button(page2, 'factorSignin.usePasskey').click();
    // web3.8 (R7): the passkey was used on the login step, so the key is step 2.
    await expectHeading(page2, SIGNIN.keyTitle);
    await expect(page2.getByText(copy(SIGNIN.step, { n: 2, total: 3 }), { exact: true })).toBeVisible();
    await keyField(page2).fill(keyText(bjorn));
    await keySubmit(page2).click();
    await expectHeading(page2, SIGNIN.doneTitle);
    await page2.getByRole('button', { name: copy(SIGNIN.doneNext), exact: true }).click();
    await expectShell(page2);
  } finally {
    await second.close();
    rmSync(secondDir, { recursive: true, force: true });
  }

  // 3. Settings: add the authenticator app (step-up with the password), then remove "Laptop" (step-up).
  const settings = await openSignIn(page);
  await expect(settings.getByText(copy('factorSettings.only'), { exact: true })).toBeVisible();
  await button(settings, 'factorSettings.totp.add').click();
  await stepUp(page, bjorn.password);
  const adding = dialogOf(page, 'factor.totp.title');
  await expect(adding).toBeVisible({ timeout: WAIT });
  const key = adding.getByLabel(copy('factor.totp.key'), { exact: true });
  await expect(key).toHaveValue(/^[A-Z2-7]{16,}$/, { timeout: WAIT });
  bjorn.totpSecret = await key.inputValue();
  await adding.getByLabel(copy('factor.totp.code'), { exact: true }).fill(await nextCode(bjorn.control, bjorn));
  await button(adding, 'factor.totp.submit').click();
  // The account already holds backup codes, so the add ends here with no list of new ones.
  await expect(adding).toBeHidden({ timeout: WAIT });
  await expect(page.getByText(copy('factorSettings.added.totp'), { exact: true })).toBeAttached();
  // Focus goes back to the app's row, whose button was "add" and is now "remove".
  const removeApp = settings.getByRole('button', { name: copy('factorSettings.totp.removeLabel'), exact: true });
  await expect(removeApp).toBeFocused();
  await expect(removeApp).not.toHaveAttribute('aria-disabled', 'true');
  // Walk I3 (WEB-SET): each passkey is a settings row of its own, and the passkeys row, headed "Passkeys", follows them with the add button.
  const passkeys = settings.getByRole('group', { name: copy('factorSettings.passkeys'), exact: true });
  await expect(passkeys.getByRole('heading', { name: copy('factorSettings.passkeys'), exact: true })).toBeVisible();
  // Task 11 (Q7): each passkey row is "Passkey" over its name.
  await expect(settings.getByRole('group', { name: copy('factorSettings.passkey'), exact: true }).getByText(PASSKEY, { exact: true })).toBeVisible();
  await expect(button(passkeys, 'factorSettings.passkeys.add')).toBeVisible();
  await settings.getByRole('button', { name: copy('factorSettings.passkeys.removeLabel', { name: PASSKEY }), exact: true }).click();
  await stepUp(page, bjorn.password);
  const confirm = dialogOf(page, 'factorSettings.removeTitle.passkey', { name: PASSKEY });
  await expect(confirm).toBeVisible({ timeout: WAIT });
  await button(confirm, 'factorSettings.removeConfirm').click();
  await expect(confirm).toBeHidden({ timeout: WAIT });
  await expect(page.getByText(copy('factorSettings.removed.passkey', { name: PASSKEY }), { exact: true })).toBeAttached();
  // The passkeys' add button is the focus target now that the group is empty.
  await expect(button(settings, 'factorSettings.passkeys.add')).toBeFocused();
  await expect(settings.getByText(copy('factorSettings.passkeys.none'), { exact: true })).toBeVisible();
  // The app is now the only factor.
  await expect(removeApp).toHaveAttribute('aria-disabled', 'true');
  await expect(settings.getByText(copy('factorSettings.only'), { exact: true })).toBeVisible();
});
