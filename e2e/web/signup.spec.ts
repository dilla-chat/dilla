import { expect } from '@playwright/test';
import { readFileSync } from 'node:fs';
import { instanceSlug } from '../../packages/web/src/screens/recovery-file';
import {
  CLASS, COPY, HEX32, PUBLIC_URL, RK_GROUP, TEST_TIMEOUT, WAIT, copy, escapeRegExp, expectHeading, expectShell, nextButton, onboardingFrame,
  completeFactorStep, keysSubmitButton, newAccount, openChannel, railItem, railNav, readKekRecord, readRecoveryKey, submitButton, test,
} from './support/app';

test.describe.configure({ timeout: TEST_TIMEOUT });

test('a server invite, a name and a written-down key make an account that lands in that server', async ({ page, context, peer, instanceName }) => {
  const setup = await peer.setup();
  await peer.register();
  const invite = setup.invite_code;

  await page.goto(`/welcome?invite=${encodeURIComponent(invite)}`);
  await expectHeading(page, COPY.connectTitle, { instance: instanceName });
  await expect(page.getByLabel(copy(COPY.inviteLabel), { exact: true })).toHaveValue(invite);
  await nextButton(page).click();

  // Wave B task 3b: step 2 asks for a username and a password only; Create account makes the keys, nothing more.
  await expectHeading(page, COPY.identityTitle);
  await expect(page.getByLabel(/Display name/)).toHaveCount(0);
  await page.getByLabel(copy(COPY.usernameLabel), { exact: true }).fill(`w${setup.user_id.slice(0, 8)}`);
  await page.getByLabel(copy(COPY.passwordLabel), { exact: true }).fill('e2e-password-1234');
  await submitButton(page).click();

  // The recovery key: 13 groups of 4 Crockford characters, selectable, never offered for copying (F2).
  await expectHeading(page, COPY.keysTitle);
  const groups = await readRecoveryKey(page);
  expect(groups).toHaveLength(13);
  for (const group of groups) expect(group).toMatch(RK_GROUP);
  await expect(page.locator(CLASS.recoveryKey).getByRole('button', { name: /copy/i })).toHaveCount(0);
  // Task 3b security review MEDIUM-1: Save as file reaches the browser's download in this engine (chromium, firefox, webkit),
  // as a file whose bytes are the explanation, a blank line and the key once.
  const [download] = await Promise.all([
    page.waitForEvent('download'),
    page.getByRole('button', { name: copy('onboarding.keys.save'), exact: true }).click(),
  ]);
  expect(download.suggestedFilename()).toBe(`dilla-recovery-key-${instanceSlug(instanceName)}.txt`);
  const saved = readFileSync(await download.path(), 'utf8');
  expect(saved).toBe(`${copy('onboarding.keys.fileBody', { instance: instanceName })}\n\n${groups.join('-')}\n`);
  expect(saved.split(groups.join('-'))).toHaveLength(2);
  await download.delete();
  await expect(keysSubmitButton(page)).toBeDisabled();
  await page.getByRole('checkbox', { name: copy(COPY.keysAcknowledge), exact: true }).check();
  await expect(keysSubmitButton(page)).toBeEnabled();
  // Step 3 registers: its forward button is `I saved it`, and only once the key is ticked (task 3b).
  await keysSubmitButton(page).click();
  const account = newAccount();
  await completeFactorStep(page, account);
  expect(account.backupCodes.length).toBeGreaterThan(0);
  await expectHeading(page, COPY.doneTitle);
  await page.getByRole('button', { name: copy(COPY.finish), exact: true }).click();
  await expectShell(page);

  // The server invite registered the account and joined its server: the person lands in it, with no
  // join dialog and no second paste (ruling 34).
  await expect(page).toHaveURL(new RegExp(`^${escapeRegExp(PUBLIC_URL)}/c/${setup.community_id}(/[0-9a-f]{32})?$`), { timeout: WAIT });
  await expect(railItem(page, peer.communityName)).toBeVisible({ timeout: WAIT });
  await expect(page.getByRole('dialog')).toHaveCount(0);

  // The key crossed to the page once, for display: it is gone from the DOM, the URL and web storage.
  const keyText = groups.join('');
  const bodyText = await page.evaluate(() => document.body.innerText.replace(/\s+/g, ''));
  expect(bodyText).not.toContain(groups.slice(0, 3).join(''));
  expect(page.url()).not.toContain(keyText.slice(0, 12));
  const webStorage = await page.evaluate(() => ({ local: localStorage.length, cookie: document.cookie,
    session: Array.from({ length: sessionStorage.length }, (_, index) => {
      const name = sessionStorage.key(index)!;
      return [name, sessionStorage.getItem(name)];
    }) }));
  expect(webStorage).toEqual({ local: 0, cookie: '',
    session: [['dilla.update.tab', expect.stringMatching(/^[0-9a-f]{32}$/)]] });
  expect(await context.cookies()).toEqual([]);

  // The device KEK is wrapped by a non-extractable WebCrypto key (C7, L-TS-05).
  expect(await readKekRecord(page)).toEqual({
    count: 1,
    keys: [expect.stringMatching(HEX32)],
    v: 1,
    type: 'secret',
    extractable: false,
    algorithm: 'AES-GCM',
    length: 256,
    usages: ['decrypt', 'encrypt'],
    iv: 12,
    ct: 48,
    exportRefused: true,
  });

  // The new device enters the peer's text group: the peer's roster holds both devices.
  await openChannel(page, peer.communityName, peer.channelName);
  const devices = await peer.waitForDevices(2);
  expect(devices).toContain(setup.device_id);
  const browserDevice = devices.find((d) => d !== setup.device_id);
  expect(browserDevice).toMatch(HEX32);
  await expect(onboardingFrame(page)).toHaveCount(0);
});
