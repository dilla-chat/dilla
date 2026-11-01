import { expect, type Page } from '@playwright/test';
import { closeSettings, composerBox, copy, expectAccessible, expectShell, instanceInvite, joinCommunity, openChannel, openSettings, Peer, rowById, sendText, shownName, signUp, WAIT } from './support/app';
import { test } from './support/second';

async function enter(page: Page, peer: Peer, instanceName: string) {
  const setup = await peer.setup({});
  await peer.register();
  await signUp(page, instanceInvite(), instanceName);
  await joinCommunity(page, peer, setup);
  await openChannel(page, peer.communityName, peer.channelName);
  await peer.waitForDevices(2, setup.channel_id);
  return setup;
}

test('rotation keeps already visible text; an independent trusted group still works', async ({ page, peer, instanceName }) => {
  const setup = await enter(page, peer, instanceName);
  const original = await peer.send('before rotation');
  await expect(rowById(page, peer.channelName, original.msg_id)).toContainText('before rotation', { timeout: WAIT });
  const control = await Peer.start();
  try {
    const other = await control.setup({});
    await control.register();
    await joinCommunity(page, control, other);
    await openChannel(page, control.communityName, control.channelName);
    await control.waitForDevices(2, other.channel_id);
    await control.send('trusted baseline');
    await expect(page.getByText('trusted baseline')).toBeVisible({ timeout: WAIT });
    await openChannel(page, peer.communityName, peer.channelName);
    await peer.rotateUmk();
    await peer.send('after rotation');
    await expect(page.getByRole('alert')).toContainText(copy('privacy.changed'), { timeout: WAIT });
    // Requirement 1: sending is blocked in the changed contact's group, and the composer says why.
    await expect(composerBox(page, peer.channelName)).toHaveAttribute('aria-disabled', 'true');
    await expect(rowById(page, peer.channelName, original.msg_id)).toContainText('before rotation');
    await expect(page.getByText('after rotation')).toHaveCount(0);
    await expect(rowById(page, peer.channelName, original.msg_id)).not.toHaveAttribute('data-state', 'cannot-read');
    await openChannel(page, control.communityName, control.channelName);
    await control.send('positive control');
    await expect(page.getByText('positive control')).toBeVisible({ timeout: WAIT });
    await openChannel(page, peer.communityName, peer.channelName);
    await expect(page.getByText('after rotation')).toHaveCount(0);
    await expectAccessible(page, 'rotated contact');
  } finally { await control.close(); }
});

test('Copy alone does not approve; explicit exact-key comparison admits new traffic', async ({ page, peer, instanceName }) => {
  const setup = await enter(page, peer, instanceName);
  const rotated = await peer.rotateUmk();
  await peer.send('pending rotation');
  await expect(page.getByRole('alert')).toContainText(copy('privacy.changed'), { timeout: WAIT });
  await expect(composerBox(page, peer.channelName)).toHaveAttribute('aria-disabled', 'true');
  const settings = await openSettings(page, 'privacy');
  await page.getByRole('button', { name: copy('privacy.compare', { name: shownName(setup) }) }).click();
  const dialog = page.getByRole('dialog', { name: copy('privacy.title') });
  await expect(dialog).toContainText(copy('privacy.instructions'));
  const groups = dialog.locator('[data-safety-group]');
  await expect(groups).toHaveCount(12);
  for (let i = 0; i < 12; i++) await expect(groups.nth(i)).toHaveText(/^\d{5}$/);
  const witness = await peer.trustList(rotated.user);
  expect(witness.pendingUmk).toMatch(/^[0-9a-f]{64}$/);
  await expect(dialog.locator('[data-public-umk]')).toHaveText(witness.pendingUmk!);
  const key = await dialog.locator('[data-public-umk]').textContent();
  expect(key).toMatch(/^[0-9a-f]{64}$/);
  const approve = dialog.getByRole('button', { name: copy('privacy.verify') });
  await dialog.getByRole('button', { name: copy('privacy.copy') }).click();
  await expect(approve).toHaveAttribute('aria-disabled', 'true');
  await dialog.getByRole('checkbox', { name: copy('privacy.confirm') }).check();
  await approve.click();
  await expect(dialog).toHaveCount(0);
  // W-11 / P7: focus lands on the Privacy heading after an approval.
  await expect(settings.locator('[data-doc-state="trust-privacy"]')
    .getByRole('heading', { name: copy('privacy.section'), exact: true, level: 2 })).toBeFocused();
  await page.getByRole('button', { name: copy('settings.close') }).click();
  // P2: what the contact sent under the new key before the comparison was held, and is readable once it is verified.
  await expect(page.getByText('pending rotation')).toBeVisible({ timeout: WAIT });
  await peer.send('validated new text');
  await expect(page.getByText('validated new text')).toBeVisible({ timeout: WAIT });
  await sendText(page, peer.channelName, 'browser after verification');
  await peer.waitFor('browser after verification');
  expect(key).toBe(witness.pendingUmk);
});

// Product P1: after a reload Privacy names the contact and offers Compare, and a contact whose key changed while this
// tab was closed can still be compared and verified from the conversation's alert.
test('after a reload Privacy has names and Compare; a changed contact can be compared and verified', async ({ page, peer, instanceName }) => {
  const setup = await enter(page, peer, instanceName);
  const name = shownName(setup);
  const first = await peer.send('before reload');
  await expect(rowById(page, peer.channelName, first.msg_id)).toContainText('before reload', { timeout: WAIT });
  await page.reload();
  await expectShell(page);
  let dialog = await openSettings(page, 'privacy');
  await expect(dialog.getByText(name, { exact: true })).toBeVisible({ timeout: WAIT });
  await expect(dialog.getByRole('button', { name: copy('privacy.compare', { name }) })).toBeVisible({ timeout: WAIT });
  await closeSettings(page);
  const rotated = await peer.rotateUmk();
  await peer.send('changed while away');
  // Reload at once, in the middle of the browser's ingest of the rotation and the message: an interrupted write
  // must roll back on the next open, never leave the store unreadable (opfs-report).
  await page.reload();
  await expectShell(page);
  await openChannel(page, peer.communityName, peer.channelName);
  const alert = page.getByRole('alert').filter({ hasText: copy('privacy.changed') });
  await expect(alert).toContainText(name, { timeout: WAIT });
  await expect(composerBox(page, peer.channelName)).toHaveAttribute('aria-disabled', 'true');
  await alert.getByRole('button', { name: copy('privacy.compare', { name }) }).click();
  dialog = page.getByRole('dialog', { name: copy('settings.title'), exact: true });
  await dialog.getByRole('button', { name: copy('privacy.compare', { name }) }).click();
  const compare = page.getByRole('dialog', { name: copy('privacy.title') });
  const witness = await peer.trustList(rotated.user);
  await expect(compare.locator('[data-public-umk]')).toHaveText(witness.pendingUmk!, { timeout: WAIT });
  await expect(compare.locator('[data-safety-group]')).toHaveCount(12);
  await compare.getByRole('checkbox', { name: copy('privacy.confirm') }).check();
  await compare.getByRole('button', { name: copy('privacy.verify') }).click();
  await expect(compare).toHaveCount(0);
  await closeSettings(page);
  await expect(page.getByText('changed while away')).toBeVisible({ timeout: WAIT });
  await sendText(page, peer.channelName, 'browser after reload verification');
  await peer.waitFor('browser after reload verification');
});

// Flow 05 "validated Remove" (product P4): a contact whose key changed leaves this conversation with the changed device;
// once that Remove is committed, sending resumes here, the removed note says so, and the contact stays changed.
test('a validated Remove of the changed device restores sending here only; the contact stays changed', async ({ page, peer, instanceName }) => {
  const setup = await peer.setup({});
  const registered = await peer.register();
  await signUp(page, instanceInvite(), instanceName);
  await joinCommunity(page, peer, setup);
  await openChannel(page, peer.communityName, peer.channelName);
  await peer.waitForDevices(2, setup.channel_id);
  const contact = await Peer.start();
  try {
    const own = await contact.setup({});
    await contact.join(setup, registered.group_id);
    await peer.waitForDevices(3, setup.channel_id);
    const hello = await contact.send('hello from the contact');
    await expect(rowById(page, peer.channelName, hello.msg_id)).toContainText('hello from the contact', { timeout: WAIT });
    const name = shownName(own);
    await contact.rotateUmk();
    await contact.send('contact under a new key');
    const alert = page.getByRole('alert').filter({ hasText: name });
    await expect(alert).toContainText(copy('privacy.changed'), { timeout: WAIT });
    await expect(composerBox(page, peer.channelName)).toHaveAttribute('aria-disabled', 'true');
    // The changed device leaves; the browser commits that Remove as any member would.
    await contact.leave();
    await expect(composerBox(page, peer.channelName)).not.toHaveAttribute('aria-disabled', 'true', { timeout: WAIT });
    await expect(page.getByText(copy('privacy.removed'))).toBeVisible({ timeout: WAIT });
    await sendText(page, peer.channelName, 'sending again after the Remove');
    await peer.waitFor('sending again after the Remove');
    // The contact is still changed and unverified: Privacy keeps the changed alert, with no verified state.
    const dialog = await openSettings(page, 'privacy');
    const row = dialog.getByRole('listitem').filter({ hasText: name });
    await expect(row).toContainText(copy('privacy.changed').split('.')[0], { timeout: WAIT });
    await expect(row).not.toContainText(copy('privacy.verifiedLabel'));
    await closeSettings(page);
  } finally { await contact.close(); }
});
