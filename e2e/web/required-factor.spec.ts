// v0.1.0-web3.6, a required second factor reaches every device, from an empty instance. Like create.spec.ts this suite starts a
// test host of its own with no account, no server and no channel. Three people meet the forced set-up through the shipping
// client alone: Cleo, who signs up through an invite and closes the page at the factor choice; Ada, whose account the operator
// resets while she is in the shell (POST /debug/reset-factors on the control URL stands in for the operator, never for the
// client); and Ada again on a fresh browser after a second reset. Nothing here seeds state. Controls are found by role and
// accessible name, with the copy keys of flow 10 through copy().
import { expect, type Page, type LaunchOptions, type PlaywrightWorkerArgs, type PlaywrightWorkerOptions, type TestInfo, type ViewportSize } from '@playwright/test';
import { addServerVia,
  COPY, SIGNIN, TEST_TIMEOUT, WAIT, makeInviteLink, completeFactorStep, copy, expectAccessible, expectComposerReady, expectHeading, expectShell, splash,
  keyField, keySubmit, keyText, newAccount, nextButton, openChannel, railItem, railNav, readRecoveryKey, sendText, signInToKeyStep,
  signUp, submitButton, test, type Account, type Guard,
} from './support/app';
import { startTestHost, type HostConfig, type StartedHost } from './support/host';
import { launchSecond, secondProfileDir } from './support/second-harness';
import { arr, decode } from '../../packages/client-core/src/cbor/index';
import { rmSync } from 'node:fs';

/** Its own ports (E3 reservation). */
const EMPTY: HostConfig = { port: 8499, control: 8500, webRoot: 'packages/web/dist' };
const SERVER = 'Midgard Crew';

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

/** The operator's reset of one account's second factors (the test host's twin of `dillad admin user reset-factors`). */
async function resetFactors(control: string, username: string): Promise<void> {
  const res = await fetch(`${control}/debug/reset-factors`, {
    method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ username }),
  });
  expect(res.status, await res.text()).toBe(204);
}

/** Onboarding from its first step to the choice at step 5, however the page got there. Stops on `factor.choose.title`. */
async function signUpToFactor(page: Page, account: Account): Promise<void> {
  await expectHeading(page, COPY.connectTitle, { instance });
  await nextButton(page).click();
  await expectHeading(page, COPY.identityTitle);
  await page.getByLabel(copy(COPY.usernameLabel), { exact: true }).fill(account.username);
  await page.getByLabel(copy(COPY.displayLabel), { exact: true }).fill(account.display);
  await page.getByLabel(copy(COPY.passwordLabel), { exact: true }).fill(account.password);
  await nextButton(page).click();
  await expectHeading(page, COPY.keysTitle);
  account.recoveryKey = await readRecoveryKey(page);
  await page.getByRole('checkbox', { name: copy(COPY.keysAcknowledge), exact: true }).check();
  await nextButton(page).click();
  await expectHeading(page, COPY.deviceTitle);
  await submitButton(page).click();
  await expectHeading(page, 'factor.choose.title');
}

/** The forced screen: the title, the required copy, and no way past it but the set-up (or forgetting the browser). */
async function expectForced(page: Page): Promise<void> {
  await expectHeading(page, 'factor.choose.title');
  await expect(page.getByText(copy('factor.choose.required', { instance }), { exact: true })).toBeVisible({ timeout: WAIT });
  await expect(page.getByRole('button', { name: copy('factor.back'), exact: true })).toHaveCount(0);
  await expect(page.getByRole('button', { name: /skip|later|cancel/i })).toHaveCount(0);
  await expect(page.getByRole('button', { name: copy('devices.forget'), exact: true })).toBeVisible();
  await expect(railNav(page)).toHaveCount(0);
}

/** The set-up from the choice: app, key, code, backup codes; then the done step and the shell (the required origin has one). */
async function setUpAgain(page: Page, account: Account): Promise<void> {
  account.lastCounter = -1;
  await completeFactorStep(page, account);
  await expectHeading(page, 'factor.done.title');
  await page.getByRole('button', { name: copy('factor.done.next'), exact: true }).click();
  await expectShell(page);
}

interface CleoFixtures {
  page: Page; guard: Guard; playwright: PlaywrightWorkerArgs['playwright']; browserName: PlaywrightWorkerOptions['browserName'];
  launchOptions: LaunchOptions; headless: boolean; channel: string | undefined; viewport: ViewportSize | null;
}

/** Ivar's server and invite, Cleo's sign-up left at the choice, her reopening and the forced set-up; `expectMember` also asks
 *  for her place in the invite's server. */
async function cleoReopens(
  { page, guard, playwright, browserName, launchOptions, headless, channel, viewport }: CleoFixtures, testInfo: TestInfo, expectMember: boolean,
): Promise<void> {
  const h = started();

  // The inviter: a server and its invite link, made through the shipping client.
  await signUp(page, h.invite.split(',')[0] ?? '', instance, 'Ivar', h.control);
  await addServerVia(page, 'create');
  const start = page.getByRole('dialog', { name: copy('create.server.title'), exact: true });
  await start.getByRole('textbox', { name: copy('create.server.name'), exact: true }).fill(SERVER);
  await page.keyboard.press('Enter');
  await expect(start).toBeHidden({ timeout: WAIT });
  await expectComposerReady(page, 'general');
  // web3.8 (item 11): the link is made under server settings → Invites.
  const link = await makeInviteLink(page, SERVER);

  // 1. Cleo signs up through that invite and closes the page on the factor choice.
  const dir = secondProfileDir(testInfo.project.name, testInfo.workerIndex);
  const cleo = newAccount('Cleo', h.control);
  const first = await launchSecond(playwright, browserName, { launchOptions, headless, channel, viewport, baseURL: h.url }, dir);
  try {
    const p1 = first.pages()[0] ?? (await first.newPage());
    await p1.goto(new URL(link).pathname);
    // As admin.spec.ts does: the guard watches from the app on. Firefox asks the server-rendered invite page for /favicon.ico,
    // which the page's img-src refuses (the page declares no icon link).
    await guard.attach(first);
    await p1.getByRole('link', { name: 'Open dilla in this browser', exact: true }).click();
    await signUpToFactor(p1, cleo);
    // She closes the page (a blank tab keeps the browser, and so the profile, open) and opens the app again in it.
    const p2 = await first.newPage();
    await p1.close();
    await p2.goto('/welcome');
    await expectForced(p2);
    await expectAccessible(p2, 'forced second factor: choose');
    await p2.setViewportSize({ width: 360, height: 740 });
    await expectAccessible(p2, 'forced second factor at 360x740');
    await p2.setViewportSize(viewport ?? { width: 1280, height: 720 });
    await setUpAgain(p2, cleo);
    if (expectMember) {
      await expect(railItem(p2, SERVER)).toBeVisible({ timeout: WAIT });
      await openChannel(p2, SERVER, 'general');
    }
  } finally {
    await first.close();
    rmSync(dir, { recursive: true, force: true });
  }
}

test('a sign-up left at the factor choice meets the forced screen on reopening', async ({ page, guard, playwright, browserName, launchOptions, headless, channel, viewport }, testInfo) => {
  await cleoReopens({ page, guard, playwright, browserName, launchOptions, headless, channel, viewport }, testInfo, false);
});

// Web3.6 E3 finding, fixed: the invite a sign-up was to join lived in the worker's memory only, so a page closed at the factor
// choice lost it. The worker now keeps it in the account's store until the join after the set-up.
test('a sign-up left at the factor choice joins the invited server after the forced set-up', async ({ page, guard, playwright, browserName, launchOptions, headless, channel, viewport }, testInfo) => {
  await cleoReopens({ page, guard, playwright, browserName, launchOptions, headless, channel, viewport }, testInfo, true);
});

/** Ada signs up and starts a server in the page; used by both reset tests. */
async function adaInTheShell(page: Page, h: StartedHost): Promise<Account> {
  const ada = await signUp(page, h.invite.split(',')[0] ?? '', instance, 'Ada', h.control);
  await addServerVia(page, 'create');
  const start = page.getByRole('dialog', { name: copy('create.server.title'), exact: true });
  await start.getByRole('textbox', { name: copy('create.server.name'), exact: true }).fill('Asgard Crew');
  await page.keyboard.press('Enter');
  await expect(start).toBeHidden({ timeout: WAIT });
  await expectComposerReady(page, 'general');
  await sendText(page, 'general', 'before the reset');
  return ada;
}

// Web3.6 E3 finding, fixed: after an operator reset the live page reached the forced screen with the `factors` slice still
// holding the old authenticator app, which FactorSetup took for a factor already in place. The worker now publishes the owed
// state before the phase, and FactorSetup trusts only a slice that says nothing is owed.
test('an operator reset reaches a live page with no reload', async ({ page }) => {
  const h = started();
  const ada = await adaInTheShell(page, h);
  await resetFactors(h.control, ada.username);
  // No reload: the gateway closes with 4003 and the page lands on the forced screen.
  await expectForced(page);
  await setUpAgain(page, ada);
  await expectComposerReady(page, 'general');
  await sendText(page, 'general', 'after the set-up');
});

// Signal task 10 fix round 3: Forget this browser lives on the forced-factor screen (Settings → Devices draws the prototype's rows).
// It clears this browser and signs it out; it sends forgetBrowser only, never a removal, so the device stays in the account's list
// (the worker's contract, ForgetBrowser.tsx). The page lands on the connect step and stays there after a reload: nothing is left.
test('the forced screen\u2019s Forget this browser clears this browser and leaves the account\u2019s device listed', async ({ page }) => {
  const h = started();
  const ada = await adaInTheShell(page, h);
  await resetFactors(h.control, ada.username);
  await expectForced(page);
  const removals: string[] = [];
  // A device's removal is DELETE /v1/devices/{id}; forget ends this browser's own sessions (DELETE …/{id}/sessions), which
  // signs it out and leaves the device listed (internal/api/api.go), so only the bare device path counts.
  page.on('request', request => { if (request.method() === 'DELETE' && /\/v1\/devices\/[^/?]+$/.test(new URL(request.url()).pathname)) removals.push(request.url()); });
  await page.getByRole('button', { name: copy('devices.forget'), exact: true }).click();
  const confirm = page.getByRole('dialog', { name: copy('devices.forgetTitle'), exact: true });
  await expect(confirm).toContainText(copy('devices.forgetBody'));
  await expectAccessible(page, 'forced screen: forget this browser');
  await confirm.getByRole('button', { name: copy('devices.confirmForget'), exact: true }).click();
  await expectHeading(page, COPY.connectTitle, { instance });
  await page.reload();
  await expectHeading(page, COPY.connectTitle, { instance });
  await expect(splash(page, COPY.storeLost)).toHaveCount(0);
  await expect(railNav(page)).toHaveCount(0);
  expect(removals, 'forget removes no device from the account').toEqual([]);
});

// Walk M9, fixed: "Use this tab" in a second tab answered "Close the other tab" while the first was on the forced screen,
// which did not answer the hand-over. The forced screen now hands dilla over as the shell does.
test('a second tab takes dilla over from a tab on the forced screen', async ({ page, context }) => {
  const h = started();
  const ada = await adaInTheShell(page, h);
  await resetFactors(h.control, ada.username);
  await expectForced(page);
  const second = await context.newPage();
  await second.goto('/');
  await expect(splash(second, COPY.otherTab)).toBeVisible({ timeout: WAIT });
  await second.getByRole('button', { name: copy('tabs.use'), exact: true }).click();
  await expectForced(second);
  await expect(page.getByRole('heading', { name: copy('tabs.paused'), exact: true })).toBeVisible({ timeout: WAIT });
  await setUpAgain(second, ada);
  await openChannel(second, 'Asgard Crew', 'general');
  await expectComposerReady(second, 'general');
  await second.close();
});

test('after a reset, a fresh browser takes the password and then the set-up, not a code', async ({
  page, guard, playwright, browserName, launchOptions, headless, channel, viewport,
}, testInfo) => {
  const h = started();
  const ada = await adaInTheShell(page, h);
  await resetFactors(h.control, ada.username);
  const dir = secondProfileDir(testInfo.project.name, testInfo.workerIndex);
  const context = await launchSecond(playwright, browserName, { launchOptions, headless, channel, viewport, baseURL: h.url }, dir);
  try {
    await guard.attach(context);
    const p = context.pages()[0] ?? (await context.newPage());
    // web3.8 (R7): the login, then the key as step 2 (no code was asked for), then the set-up as step 3.
    await signInToKeyStep(p, ada, instance);
    await expect(p.getByText(copy(SIGNIN.step, { n: 2, total: 3 }), { exact: true })).toBeVisible();
    await keyField(p).fill(keyText(ada));
    await keySubmit(p).click();
    await expectHeading(p, 'factor.choose.title');
    await expect(p.getByText(copy(SIGNIN.step, { n: 3, total: 4 }), { exact: true })).toBeVisible();
    await expect(p.getByRole('heading', { level: 1, name: copy('signin.totp.title'), exact: true })).toHaveCount(0);
    ada.lastCounter = -1;
    await completeFactorStep(p, ada);
    await expectHeading(p, SIGNIN.doneTitle);
    await p.getByRole('button', { name: copy(SIGNIN.doneNext), exact: true }).click();
    await expectShell(p);
    await expect(railItem(p, 'Asgard Crew')).toBeVisible({ timeout: WAIT });
    await openChannel(p, 'Asgard Crew', 'general');
    await expectComposerReady(p, 'general');
    await sendText(p, 'general', 'from the new browser');
  } finally {
    await context.close();
    rmSync(dir, { recursive: true, force: true });
  }
});
