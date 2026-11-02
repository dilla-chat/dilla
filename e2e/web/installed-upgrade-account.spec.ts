import { expect } from '@playwright/test';
import { createHash } from 'node:crypto';
import { readFile, writeFile } from 'node:fs/promises';
import { test } from './support/installed-app.ts';
import { PAST_BROWSER_SESSION_S, WAIT, advanceClock, attachInput, composerBox, copy, deviceRows, expectHeading, expectShell, instanceInvite, joinCommunity,
  keyText, messageRow, openChannel, openSettings, readKekRecord, sendText, signIn, signUp, tag, trayEntries } from './support/app.ts';

test.describe.configure({ timeout: 15 * 60_000 });
test('installed upgrade preserves the actual device keystore, encrypted history, draft guard and recovery enrollment', async ({ page, peer, instanceName, installed, guard, relaunch }, info) => {
  const setup = await peer.setup(); await peer.register();
  const account = await signUp(page, instanceInvite(), instanceName);
  await joinCommunity(page, peer, setup); await openChannel(page, peer.communityName, peer.channelName);
  const deviceA = (await peer.waitForDevices(2)).find(device => device !== setup.device_id)!;
  expect(deviceA).toMatch(/^[0-9a-f]{32}$/);
  const kek = await readKekRecord(page);
  expect(kek).toMatchObject({ count: 1, extractable: false, exportRefused: true, algorithm: 'AES-GCM', iv: 12, ct: 48 });
  const wrappedFingerprint = async () => installed.page.evaluate(async () => {
    const db = await new Promise<IDBDatabase>((done, fail) => { const r = indexedDB.open('dilla-device', 1); r.onsuccess = () => done(r.result); r.onerror = () => fail(r.error); });
    try {
      const rows = await new Promise<{ iv: Uint8Array; ct: Uint8Array }[]>((done, fail) => { const r = db.transaction('kek', 'readonly').objectStore('kek').getAll(); r.onsuccess = () => done(r.result); r.onerror = () => fail(r.error); });
      if (rows.length !== 1) throw new Error('Expected this real device wrapper');
      const bytes = new Uint8Array(rows[0].iv.length + rows[0].ct.length); bytes.set(rows[0].iv); bytes.set(rows[0].ct, rows[0].iv.length);
      return [...new Uint8Array(await crypto.subtle.digest('SHA-256', bytes))].map(byte => byte.toString(16).padStart(2, '0')).join('');
    } finally { db.close(); }
  });
  const beforeWrapper = await wrappedFingerprint(), beforeBinary = createHash('sha512').update(await readFile(installed.packages.installed)).digest('hex');
  const historical = 'encrypted before installed update ' + tag(); await peer.send(historical);
  await expect(messageRow(page, peer.channelName, historical)).toHaveAttribute('data-state', 'ok', { timeout: WAIT });
  const draft = 'work that blocks this installed update ' + tag();
  await composerBox(page, peer.channelName).fill(draft);
  await expect(trayEntries(page)).toHaveCount(0);
  await installed.prepareUpdate(); await installed.expectDraftBlocked();
  expect(createHash('sha512').update(await readFile(installed.packages.installed)).digest('hex')).toBe(beforeBinary);
  await expect(composerBox(page, peer.channelName)).toHaveValue(draft);
  await attachInput(page).setInputFiles({ name: 'account-upgrade.txt', mimeType: 'text/plain', buffer: Buffer.from('a real selected attachment') });
  await expect(trayEntries(page).first()).toHaveAttribute('data-phase', 'ready', { timeout: WAIT });
  await composerBox(page, peer.channelName).fill('');
  await installed.expectDraftBlocked();
  expect(createHash('sha512').update(await readFile(installed.packages.installed)).digest('hex')).toBe(beforeBinary);
  await expect(composerBox(page, peer.channelName)).toHaveValue('');
  await expect(trayEntries(page).first()).toContainText('account-upgrade.txt');
  // Native updates protect unsaved work. The person sends it before accepting
  // the update; no fake checkpoint or fabricated account record is injected.
  await composerBox(page, peer.channelName).fill(draft);
  await composerBox(page, peer.channelName).press('Enter');
  const sentDraft = await peer.waitFor(draft);
  expect(sentDraft.sender_device).toBe(deviceA); expect(sentDraft.tier).toBe(1); expect(sentDraft.attachments).toHaveLength(1);
  expect((await peer.fetchAttachment(sentDraft.seq, 0)).sha256).toBe(createHash('sha256').update('a real selected attachment').digest('hex'));
  await expect(trayEntries(page)).toHaveCount(0); await expect(composerBox(page, peer.channelName)).toHaveValue('');
  await guard.collectWorkers(); await installed.requestUpdate();
  const upgraded = await installed.waitForVersion('1.0.1'); await guard.attach(upgraded);
  let current = installed.page; await expectShell(current); await openChannel(current, peer.communityName, peer.channelName);
  expect(await readKekRecord(current)).toEqual(kek); expect(await wrappedFingerprint()).toBe(beforeWrapper);
  await expect(messageRow(current, peer.channelName, historical)).toHaveAttribute('data-state', 'ok', { timeout: WAIT });
  await expect(messageRow(current, peer.channelName, draft)).toHaveAttribute('data-state', 'ok', { timeout: WAIT });
  const after = 'encrypted after installed update ' + tag(); await sendText(current, peer.channelName, after);
  const verifiedAfter = await peer.waitFor(after); expect(verifiedAfter.sender_device).toBe(deviceA); expect(verifiedAfter.tier).toBe(1);
  // Force actual device-key reauthentication; a still-valid bearer cannot make
  // this assertion pass accidentally after losing the stored keystore.
  await advanceClock(PAST_BROWSER_SESSION_S); ({ page: current } = await relaunch()); await expectShell(current);
  await openChannel(current, peer.communityName, peer.channelName);
  expect(await wrappedFingerprint()).toBe(beforeWrapper); expect(await peer.devices()).toHaveLength(2);
  const reauthenticated = 'same device after session expiry ' + tag(); await sendText(current, peer.channelName, reauthenticated);
  expect((await peer.waitFor(reauthenticated)).sender_device).toBe(deviceA);
  await openSettings(current, 'devices');
  const own = deviceRows(current).filter({ hasText: copy('devices.thisBrowser') });
  await own.getByRole('button', { name: copy('workspace.signOut'), exact: true }).click();
  const confirm = current.getByRole('dialog', { name: copy('devices.signOutTitle'), exact: true });
  await confirm.getByRole('textbox', { name: copy('devices.keyLabel'), exact: true }).fill(keyText(account, ' '));
  await confirm.getByRole('button', { name: copy('devices.confirmSignOut'), exact: true }).click();
  await expectHeading(current, 'onboarding.connect.title', { instance: instanceName });
  await peer.waitForDevices(1);
  await signIn(current, account, instanceName);
  const deviceB = (await peer.waitForDevices(2)).find(device => device !== setup.device_id)!; expect(deviceB).not.toBe(deviceA);
  await openChannel(current, peer.communityName, peer.channelName);
  const recovered = 'recovery-enrolled after installed update ' + tag(); await sendText(current, peer.channelName, recovered);
  const received = await peer.waitFor(recovered); expect(received.sender_device).toBe(deviceB); expect(received.tier).toBe(1);
  expect(await wrappedFingerprint()).not.toBe(beforeWrapper);
  expect(await readKekRecord(current)).toMatchObject({ count: 1, extractable: false, exportRefused: true });
  const proof = info.outputPath('installed-account-proof.json');
  await writeFile(proof, JSON.stringify({ format: 1, platform: process.platform, arch: process.arch, from: '1.0.0', to: '1.0.1', productionFuses: true, actualSignup: true,
    sameDeviceAfterUpgrade: true, wrappedRecordUnchanged: true, encryptedHistoryReadable: true, draftBlockedInstallation: true, selectedFileBlockedInstallation: true, selectedFileVerifiedByPeer: true,
    expiredSessionReauthenticated: true, recoveryEnrolledNewDevice: true }));
  await info.attach('installed-account-proof', { path: proof, contentType: 'application/json' });
});
