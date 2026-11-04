import { expect } from '@playwright/test';
import {
  COPY, HEX32, PEER_PASSWORD, SIGNIN, TEST_TIMEOUT, WAIT, closeSettings, copy, deviceRows, expectAccessible,
  expectHeading, expectShell, instanceInvite, joinCommunity, keyField, keySubmit, keyText, messageRow,
  openChannel, openSettings, railNav, sendText, signIn, signUp, splash, tag, wrongKey,
} from './support/app';
import { test } from './support/second';
import { nextCode } from './support/totp';

test.describe.configure({ timeout: TEST_TIMEOUT });

test('a second browser joins the account by password and recovery key, and the first can remove it', async ({ page, peer, second, instanceName }) => {
  const setup = await peer.setup();
  await peer.register();
  const channel = peer.channelName;
  const account = await signUp(page, instanceInvite(), instanceName);
  await joinCommunity(page, peer, setup);
  await openChannel(page, peer.communityName, channel);
  const deviceA = (await peer.waitForDevices(2)).find((d) => d !== setup.device_id);
  expect(deviceA).toMatch(HEX32);

  // B: its own profile, the host login and the key as it was written down (13 groups).
  const b = (await second.open()).page;
  await signIn(b, account, instanceName, keyText(account, ' '));
  await openChannel(b, peer.communityName, channel);
  const roster = await peer.waitForDevices(3);
  const deviceB = roster.find((d) => d !== setup.device_id && d !== deviceA);
  expect(deviceB).toMatch(HEX32);

  // The key crossed into B once, inside one command: none of it is left on the page side.
  const keyChars = account.recoveryKey.join('');
  const shownText = await b.evaluate(() => document.body.innerText.replace(/\s+/g, ''));
  expect(shownText).not.toContain(keyChars.slice(0, 12));
  expect(b.url()).not.toContain(keyChars.slice(0, 12));
  const webStorage = await b.evaluate(() => ({ local: localStorage.length, cookie: document.cookie,
    session: Array.from({ length: sessionStorage.length }, (_, index) => {
      const name = sessionStorage.key(index)!;
      return [name, sessionStorage.getItem(name)];
    }) }));
  expect(webStorage).toEqual({ local: 0, cookie: '',
    session: [['dilla.update.tab', expect.stringMatching(/^[0-9a-f]{32}$/)]] });

  // The peer speaks: both browsers of the account read it.
  const fromPeer = `peer to both ${tag()}`;
  await peer.send(fromPeer);
  await expect(messageRow(page, channel, fromPeer)).toHaveAttribute('data-state', 'ok', { timeout: WAIT });
  await expect(messageRow(b, channel, fromPeer)).toHaveAttribute('data-state', 'ok', { timeout: WAIT });

  // B speaks: A reads it, and the peer's MLS state authenticates B's device of the same user.
  const fromB = `from the second browser ${tag()}`;
  await sendText(b, channel, fromB);
  await expect(messageRow(page, channel, fromB)).toHaveAttribute('data-state', 'ok', { timeout: WAIT });
  const receivedB = await peer.waitFor(fromB);
  expect(receivedB.sender_device).toBe(deviceB);
  expect(receivedB.tier).toBe(1);
  const fromA = `from the first browser ${tag()}`;
  await sendText(page, channel, fromA);
  const receivedA = await peer.waitFor(fromA);
  expect(receivedA.sender_device).toBe(deviceA);
  expect(receivedB.sender_user).toBe(receivedA.sender_user);
  expect(receivedB.sender_user).not.toBe(setup.user_id);

  // Settings → Devices in A: two devices, both in the signed list, one of them this browser.
  await openSettings(page, 'devices');
  const rows = deviceRows(page);
  await expect(rows).toHaveCount(2, { timeout: WAIT });
  await expect(rows.filter({ hasText: copy('devices.thisBrowser') })).toHaveCount(1);
  await expect(rows.filter({ hasText: copy('devices.unlisted') })).toHaveCount(0, { timeout: WAIT });
  await expect(rows.filter({ hasText: copy('devices.revoked') })).toHaveCount(0);
  await expectAccessible(page, 'settings: devices');

  // A removes B with the key: B is signed out, its leaf leaves the group, A keeps talking.
  const rowB = rows.filter({ hasNotText: copy('devices.thisBrowser') });
  await rowB.getByRole('button', { name: copy('devices.revoke'), exact: true }).click();
  const confirm = page.getByRole('dialog', { name: copy('devices.revokeTitle'), exact: true });
  await expect(confirm).toBeVisible({ timeout: WAIT });
  await expectAccessible(page, 'settings: remove a device');
  await confirm.getByRole('textbox', { name: copy('devices.keyLabel'), exact: true }).fill(keyText(account, ' '));
  await confirm.getByRole('button', { name: copy('devices.confirmRevoke'), exact: true }).click();
  await expect(confirm).toBeHidden({ timeout: WAIT });
  await expect(rowB).toContainText(copy('devices.revoked'), { timeout: WAIT });
  await expect(splash(b, 'boot.revoked.status')).toBeVisible({ timeout: WAIT });
  expect((await peer.waitForDevices(2)).sort()).toEqual([setup.device_id, deviceA!].sort());
  await closeSettings(page);
  const afterRemoval = `after the removal ${tag()}`;
  await sendText(page, channel, afterRemoval);
  expect((await peer.waitFor(afterRemoval)).sender_device).toBe(deviceA);
});

test('a wrong recovery key is refused in place, and the right one typed in lower case with hyphens works after it', async ({ page, second, instanceName }) => {
  const account = await signUp(page, instanceInvite(), instanceName);
  const b = (await second.open()).page;

  await b.goto('/welcome');
  await expectHeading(b, COPY.connectTitle, { instance: instanceName });
  const entry = b.getByRole('button', { name: copy(SIGNIN.entry), exact: true });
  await expect(entry).toBeVisible();
  await expectAccessible(b, 'onboarding: the sign-in entry');
  await entry.click();

  // web3.8 (R6, R7): the login comes first, step 1; then the code, step 2; then the recovery key, step 3.
  await expectHeading(b, SIGNIN.loginTitle, { instance: instanceName });
  await expect(b.getByText(copy(SIGNIN.step, { n: 1, total: 4 }), { exact: true })).toBeVisible();
  await expectAccessible(b, 'sign-in: login');
  const forward = b.getByRole('button', { name: copy(SIGNIN.loginSubmit), exact: true });
  await b.getByLabel(copy(SIGNIN.username), { exact: true }).fill(account.username);
  await b.getByLabel(copy(SIGNIN.password), { exact: true }).fill(account.password);
  await forward.click();
  await expectHeading(b, 'signin.totp.title');
  await expect(b.getByText(copy(SIGNIN.step, { n: 2, total: 4 }), { exact: true })).toBeVisible();
  // Wave B task 3: the sixth digit in the code boxes sends the code; there is no button to press.
  await b.getByLabel(copy('signin.totp.code'), { exact: true }).fill(await nextCode(account.control, account));

  await expectHeading(b, SIGNIN.keyTitle);
  await expect(b.getByText(copy(SIGNIN.step, { n: 3, total: 4 }), { exact: true })).toBeVisible();
  const field = keyField(b);
  const full = account.recoveryKey.join('');
  await field.fill(full.slice(0, 51));
  await expect(b.getByText(copy(SIGNIN.keyHint, { n: 51 }), { exact: true })).toBeVisible();
  await expect(keySubmit(b)).toHaveAttribute('aria-disabled', 'true');
  await expect(keySubmit(b)).toHaveAccessibleDescription(copy(SIGNIN.keyLength));
  await expectAccessible(b, 'sign-in: key, too short');

  // Well formed, 52 characters of the alphabet, and not this account's key: only the account's root can tell.
  const wrong = wrongKey(account);
  await field.fill(wrong);
  await expect(b.getByText(copy(SIGNIN.keyHint, { n: 52 }), { exact: true })).toBeVisible();
  await expect(keySubmit(b)).not.toHaveAttribute('aria-disabled', 'true');
  await keySubmit(b).click();
  await expect(b.getByRole('alert').filter({ hasText: copy(SIGNIN.wrongKey) })).toBeVisible({ timeout: WAIT });
  await expect(field).toBeFocused();
  await expect(field).toHaveValue(wrong);
  await expectHeading(b, SIGNIN.keyTitle);
  await expectAccessible(b, 'sign-in: wrong key');

  // The right key, as a person might paste it: lower case, hyphens between the groups.
  await field.fill(account.recoveryKey.join('-').toLowerCase());
  await expect(b.getByText(copy(SIGNIN.keyHint, { n: 52 }), { exact: true })).toBeVisible();
  await keySubmit(b).click();
  await expectHeading(b, SIGNIN.doneTitle);
  await expect(b.getByText(copy(SIGNIN.doneBody, { username: account.username, instance: instanceName }), { exact: true })).toBeVisible();
  await expectAccessible(b, 'sign-in: done');
  await b.getByRole('button', { name: copy(SIGNIN.doneNext), exact: true }).click();
  await expectShell(b);
});

// Signal task 10: Forget left Settings → Devices (the prototype's rows); it is tested where it lives, on the forced-factor
// screen (required-factor.spec.ts, "the forced screen's Forget this browser…").

test('sign out and remove takes this browser out of the account and out of the group', async ({ page, peer, second, instanceName }) => {
  const setup = await peer.setup();
  await peer.register();
  const channel = peer.channelName;
  const account = await signUp(page, instanceInvite(), instanceName);
  await joinCommunity(page, peer, setup);
  await openChannel(page, peer.communityName, channel);
  const deviceA = (await peer.waitForDevices(2)).find((d) => d !== setup.device_id);
  expect(deviceA).toMatch(HEX32);

  const c = (await second.open()).page;
  await signIn(c, account, instanceName);
  await openChannel(c, peer.communityName, channel);
  const deviceC = (await peer.waitForDevices(3)).find((d) => d !== setup.device_id && d !== deviceA);
  expect(deviceC).toMatch(HEX32);

  const settingsC = await openSettings(c, 'devices');
  // Signal task 10 fix round 1 (README §5.9): Sign out is on this browser's own device row.
  await settingsC.getByRole('button', { name: copy('workspace.signOut'), exact: true }).click();
  const confirm = c.getByRole('dialog', { name: copy('devices.signOutTitle'), exact: true });
  await expect(confirm).toBeVisible({ timeout: WAIT });
  await expectAccessible(c, 'settings: sign out and remove');
  await confirm.getByRole('textbox', { name: copy('devices.keyLabel'), exact: true }).fill(keyText(account, ' '));
  await confirm.getByRole('button', { name: copy('devices.confirmSignOut'), exact: true }).click();
  // The cleared phase reloads the page itself (L-TS-27, ruling 26): the connect step shows before any reload of ours.
  await expectHeading(c, COPY.connectTitle, { instance: instanceName });
  // An explicit second check: a reload of our own stays on the connect step (nothing of the account is left).
  await c.reload();
  await expectHeading(c, COPY.connectTitle, { instance: instanceName });

  // The instance acted on the revoking list: C's leaf was proposed for removal and committed away.
  expect((await peer.waitForDevices(2)).sort()).toEqual([setup.device_id, deviceA!].sort());

  await openSettings(page, 'devices');
  const rows = deviceRows(page);
  await expect(rows).toHaveCount(2, { timeout: WAIT });
  await expect(rows.filter({ hasText: copy('devices.revoked') })).toHaveCount(1, { timeout: WAIT });
  await expect(rows.filter({ hasText: copy('devices.thisBrowser') }).filter({ hasText: copy('devices.revoked') })).toHaveCount(0);
  await closeSettings(page);
  const after = `after the sign-out ${tag()}`;
  await sendText(page, channel, after);
  expect((await peer.waitFor(after)).sender_device).toBe(deviceA);
});

test('the peer’s enrolled device enters and leaves the group while the browser keeps talking', async ({ page, peer, instanceName }) => {
  const setup = await peer.setup({ password: PEER_PASSWORD });
  await peer.register();
  const channel = peer.channelName;
  await signUp(page, instanceInvite(), instanceName);
  await joinCommunity(page, peer, setup);
  await openChannel(page, peer.communityName, channel);
  const deviceA = (await peer.waitForDevices(2)).find((d) => d !== setup.device_id);
  expect(deviceA).toMatch(HEX32);

  const enrolled = await peer.enrol();
  expect(enrolled.version).toBe(2);
  expect(enrolled.scopes).toEqual([1, 0]);
  expect(enrolled.device_id).toMatch(HEX32);
  expect(enrolled.device_id).not.toBe(setup.device_id);
  expect((await peer.waitForDevices(3)).sort()).toEqual([setup.device_id, deviceA!, enrolled.device_id].sort());

  const one = `with three devices ${tag()}`;
  await peer.send(one);
  await expect(messageRow(page, channel, one)).toHaveAttribute('data-state', 'ok', { timeout: WAIT });
  const two = `browser with three devices ${tag()}`;
  await sendText(page, channel, two);
  expect((await peer.waitFor(two)).sender_device).toBe(deviceA);

  const revoked = await peer.revoke(enrolled.device_id);
  expect(revoked.version).toBe(3);
  expect((await peer.waitForDevices(2)).sort()).toEqual([setup.device_id, deviceA!].sort());

  const three = `after the revocation ${tag()}`;
  await peer.send(three);
  await expect(messageRow(page, channel, three)).toHaveAttribute('data-state', 'ok', { timeout: WAIT });
  const four = `browser after the revocation ${tag()}`;
  await sendText(page, channel, four);
  expect((await peer.waitFor(four)).sender_device).toBe(deviceA);
});
