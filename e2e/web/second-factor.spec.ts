// v0.1.0-web3.6, the required second factor from an empty instance. Like create.spec.ts this suite starts a test host of its
// own with no account, no server and no channel, and goes from there with the shipping client alone: sign-up step 5 (the
// authenticator app and the backup codes), the new browser's sign-in with a code, with a backup code and with a wrong code,
// and Settings → Sign-in (the only factor cannot be removed; new backup codes take a step-up). Nothing here seeds state: the
// codes are computed from the setup key the page shows, for the host's clock (support/totp.ts). Controls are found by role and
// accessible name, with the copy keys of flow 10 (strings/{factor,factorSignin,factorSettings}.ts) through copy().
import { expect, type Locator, type Page } from '@playwright/test';
import {
  COPY, SIGNIN, TEST_TIMEOUT, WAIT, completeFactorStep, copy, expectAccessible, expectHeading, expectShell, finishWithKey,
  keyField, keySubmit, keyText, newAccount, nextButton, readRecoveryKey, signInToKeyStep, submitButton, test, type Account,
  settingsGear,
} from './support/app';
import { startTestHost, type HostConfig, type StartedHost } from './support/host';
import { launchSecond, secondProfileDir } from './support/second-harness';
import { hostNow, totp } from './support/totp';
import { arr, decode } from '../../packages/client-core/src/cbor/index';
import { rmSync } from 'node:fs';

/** Its own ports (E1 reservation). */
const EMPTY: HostConfig = { port: 8495, control: 8496, webRoot: 'packages/web/dist' };
const ADA = 'Ada';

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
const button = (scope: Page | Locator, key: string, vars?: Record<string, string | number>) =>
  scope.getByRole('button', { name: copy(key, vars), exact: true });
const dialogOf = (page: Page, key: string) => page.getByRole('dialog', { name: copy(key), exact: true });

/**
 * The first two codes' boxes once the list has settled. The Signal Dialog (#121) opens with d-modal, a 240 ms rise and
 * scale; two separate boundingBox calls inside it read two frames of the animation (2026-10-30 CI: 2.6 px apart). So
 * the animations around the list finish first, and both boxes come from one frame.
 */
async function codeBoxes(list: Locator): Promise<[{ x: number; y: number; width: number; height: number }, { x: number; y: number; width: number; height: number }]> {
  await expect(list.nth(1)).toBeVisible({ timeout: WAIT });
  return list.nth(0).evaluate(async (first) => {
    const animations = (first.closest('dialog') ?? document.documentElement).getAnimations({ subtree: true })
      .filter((animation) => Number.isFinite(animation.effect?.getComputedTiming().endTime));
    await Promise.all(animations.map((animation) => animation.finished.catch(() => undefined)));
    const second = first.parentElement?.children[1];
    if (second === undefined) throw new Error('the backup codes are not on screen');
    const box = (el: Element) => { const r = el.getBoundingClientRect(); return { x: r.x, y: r.y, width: r.width, height: r.height }; };
    return [box(first), box(second)] as [ReturnType<typeof box>, ReturnType<typeof box>];
  });
}

/** Flow 10 at 1280 (walk M2): the backup codes stand in two columns, the second code beside the first. */
async function expectTwoColumns(list: Locator): Promise<void> {
  const [first, second] = await codeBoxes(list);
  expect(Math.abs(first.y - second.y), 'the second code stands beside the first').toBeLessThan(2);
  expect(second.x).toBeGreaterThan(first.x + first.width - 1);
  for (const box of [first, second]) expect(box.height, 'a code on one line').toBeLessThan(40);
}

/** Task 10 (README §5.9): the onboarding card is the prototype's 400px, so its backup codes stand in one column, one per line. */
async function expectOneColumn(list: Locator): Promise<void> {
  const [first, second] = await codeBoxes(list);
  expect(second.y, 'the second code stands under the first').toBeGreaterThanOrEqual(first.y + first.height - 1);
  for (const box of [first, second]) expect(box.height, 'a code on one line').toBeLessThan(40);
}

/** A code of a window far from the host's clock: well formed, never accepted, and it spends no counter of the account. */
async function wrongCode(secret: string, control: string): Promise<string> {
  return totp(secret, (await hostNow(control)) + 3600);
}
function secret(account: Account): string {
  if (account.totpSecret === null) throw new Error('the account has no authenticator secret yet');
  return account.totpSecret;
}

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

/** A second browser's way to the code step: its own profile, the entry, then the username and the password. */
async function toLogin(page: Page, account: Account): Promise<void> {
  await page.goto('/welcome');
  await expectHeading(page, COPY.connectTitle, { instance });
  await page.getByRole('button', { name: copy(SIGNIN.entry), exact: true }).click();
  await expectHeading(page, SIGNIN.loginTitle, { instance });
  await page.getByLabel(copy(SIGNIN.username), { exact: true }).fill(account.username);
  await page.getByLabel(copy(SIGNIN.password), { exact: true }).fill(account.password);
  await page.getByRole('button', { name: copy(SIGNIN.loginSubmit), exact: true }).click();
  await expectHeading(page, 'signin.totp.title');
}

test('second factor: sign-up step 5, a code and a backup code on new browsers, a wrong code, Settings → Sign-in', async ({
  page, guard, playwright, browserName, launchOptions, headless, channel, viewport,
}, testInfo) => {
  const h = started();

  // 1. Ada signs up. Step 5 is the factor: axe on the choice and on the app step, a wrong code, the right code, ten backup codes.
  const ada = newAccount(ADA, h.control);
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
  await completeFactorStep(page, ada, async (state) => {
    if (state === 'choose') {
      await expectAccessible(page, 'second factor: choose');
    } else if (state === 'totp') {
      const code = page.getByLabel(copy('factor.totp.code'), { exact: true });
      await expectAccessible(page, 'second factor: authenticator app');
      await code.fill(await wrongCode(await page.getByLabel(copy('factor.totp.key'), { exact: true }).inputValue(), h.control));
      await page.getByRole('button', { name: copy('factor.totp.submit'), exact: true }).click();
      await expect(page.getByText(copy('factor.error.code'), { exact: true })).toBeVisible({ timeout: WAIT });
      await expect(code).toBeFocused();
    } else {
      await expect(page.getByText(copy('factor.codes.notKey'), { exact: true })).toBeVisible();
      await expect(page.getByRole('list', { name: copy('factor.codes.label'), exact: true }).locator('li')).toHaveCount(10);
      await expectOneColumn(page.getByRole('list', { name: copy('factor.codes.label'), exact: true }).locator('li'));
      await expectAccessible(page, 'second factor: backup codes');
    }
  });
  expect(ada.backupCodes).toHaveLength(10);
  await expectHeading(page, COPY.doneTitle);
  await page.getByRole('button', { name: copy(COPY.finish), exact: true }).click();
  await expectShell(page);

  // 2. Settings → Sign-in: the app is on, no passkeys, ten codes; the app's remove is aria-disabled with the reason.
  const settings = await openSignIn(page);
  await expect(settings.getByText(copy('factorSettings.totp.off'), { exact: true })).toHaveCount(0);
  await expect(settings.getByText(copy('factorSettings.passkeys.none'), { exact: true })).toBeVisible();
  await expect(settings.getByText(copy('factorSettings.codes.other', { count: 10 }), { exact: true })).toBeVisible();
  const removeApp = settings.getByRole('button', { name: copy('factorSettings.totp.removeLabel'), exact: true });
  await expect(removeApp).toHaveAttribute('aria-disabled', 'true');
  await expect(settings.getByText(copy('factorSettings.only'), { exact: true })).toBeVisible();
  await removeApp.click({ force: true });
  await expect(dialogOf(page, 'factorSettings.stepUp.title')).toHaveCount(0);
  await page.keyboard.press('Escape');
  await expect(settings).toBeHidden({ timeout: WAIT });

  /** One more browser with a profile of its own, closed and removed whatever the body did. */
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

  // 3. A second browser: the login, the code, the key, the shell.
  await another(async (p) => {
    await signInToKeyStep(p, ada, instance);
    await finishWithKey(p, ada, instance, keyText(ada));
  });

  // 4. A third: the login, a backup code instead, then the key; the done step counts nine left.
  await another(async (p) => {
    await toLogin(p, ada);
    await button(p, 'factorSignin.useBackup').click();
    await expectHeading(p, 'factorSignin.backup.title');
    await p.getByLabel(copy('factorSignin.backup.label'), { exact: true }).fill(ada.backupCodes[0] ?? '');
    await p.getByRole('button', { name: copy('signin.login.submit'), exact: true }).click();
    await expectHeading(p, SIGNIN.keyTitle);
    await keyField(p).fill(keyText(ada));
    await keySubmit(p).click();
    await expectHeading(p, SIGNIN.doneTitle);
    await expect(p.getByText(copy('factorSignin.left.other', { count: 9 }), { exact: true })).toBeVisible({ timeout: WAIT });
    await p.getByRole('button', { name: copy(SIGNIN.doneNext), exact: true }).click();
    await expectShell(p);
  });

  // 5. A wrong code on a fourth: back at the login with the refusal.
  await another(async (p) => {
    await toLogin(p, ada);
    await p.getByLabel(copy('signin.totp.code'), { exact: true }).fill(await wrongCode(secret(ada), h.control));
    await p.getByRole('button', { name: copy(SIGNIN.loginSubmit), exact: true }).click();
    await expect(p.getByText(copy('signin.error.totpFailed'), { exact: true })).toBeVisible({ timeout: WAIT });
    await expectHeading(p, SIGNIN.loginTitle, { instance });
  });

  // 6. Ada makes new codes: "are you sure" first (walk M6), then the step-up asks for the password (a wrong one puts focus
  // back in the field, walk M3), then the new list; focus comes back to the button.
  const dialog = await openSignIn(page);
  const make = button(dialog, 'factorSettings.codes.make');
  await expect(dialog.getByText(copy('factorSettings.codes.other', { count: 9 }), { exact: true })).toBeVisible({ timeout: WAIT });
  const confirm = dialogOf(page, 'factorSettings.codes.makeTitle');
  const stepUp = dialogOf(page, 'factorSettings.stepUp.title');
  // Escape in the step-up dialog closes it only: Settings stays open and the button gets focus back.
  await make.click();
  await expect(confirm).toBeVisible({ timeout: WAIT });
  await expect(stepUp).toHaveCount(0);
  await button(confirm, 'factorSettings.codes.makeConfirm').click();
  await expect(stepUp).toBeVisible({ timeout: WAIT });
  await page.keyboard.press('Escape');
  await expect(stepUp).toBeHidden({ timeout: WAIT });
  await expect(dialog).toBeVisible();
  await expect(make).toBeFocused();
  // The real run.
  await make.click();
  await button(confirm, 'factorSettings.codes.makeConfirm').click();
  await expect(stepUp).toBeVisible({ timeout: WAIT });
  await expectAccessible(page, 'second factor: step-up dialog');
  const password = stepUp.getByLabel(copy('factorSettings.stepUp.password'), { exact: true });
  await password.fill(`${ada.password}-wrong`);
  await button(stepUp, 'factorSettings.stepUp.confirm').click();
  await expect(stepUp.getByText(copy('factorSettings.stepUp.failed'), { exact: true })).toBeVisible({ timeout: WAIT });
  await expect(password).toBeFocused();
  await password.fill(ada.password);
  await button(stepUp, 'factorSettings.stepUp.confirm').click();
  const fresh = dialogOf(page, 'factorSettings.codes.newTitle');
  await expect(fresh).toBeVisible({ timeout: WAIT });
  const list = fresh.getByRole('list', { name: copy('factor.codes.label'), exact: true }).locator('li');
  await expect(list).toHaveCount(10);
  await expectTwoColumns(list);
  const madeCodes = (await list.allInnerTexts()).map((c) => c.replace(/\s+/g, ''));
  expect(new Set(madeCodes).size).toBe(10);
  expect(madeCodes.some((c) => ada.backupCodes.includes(c))).toBe(false);
  await page.keyboard.press('Escape');
  await expect(fresh).toBeHidden({ timeout: WAIT });
  await expect(dialog).toBeVisible();
  await expect(make).toBeFocused();
  await expect(dialog.getByText(copy('factorSettings.codes.other', { count: 10 }), { exact: true })).toBeVisible({ timeout: WAIT });
});
