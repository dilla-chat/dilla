import { randomBytes } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { expect, type Page } from '@playwright/test';
import {
  CLASS, HEX32, TEST_TIMEOUT, WAIT, attachInput, channelRow, composerBox, copy, deleteDialog, drawImage, editorBox,
  expectAccessible, expectComposerReady, flashSeen, instanceInvite, joinCommunity, lightbox, logRegion, mentionList,
  PERM_PIN, messageRow, openChannel, pinsDialog, reactionChip, rowAction, rowById, rowToolbar, sendText, sha256Hex, shownName,
  signIn, signUp, tag, trayEntries, watchFlash, type Peer, channelRowMenuItem, expectRowMenuAction
} from './support/app';
import { test } from './support/second';

test.describe.configure({ timeout: TEST_TIMEOUT });

const THUMBS_UP = '\u{1F44D}';
const BLOB_PUT = /^\/v1\/channels\/[0-9a-f]{32}\/blobs\/[0-9a-f]{64}$/;

/** A signs up, joins the peer's server and opens its first channel; the peer sees A's device in the group. */
async function enter(page: Page, peer: Peer, instanceName: string, channels = 1) {
  const setup = await peer.setup(channels === 1 ? {} : { channels });
  if (channels === 1) await peer.register();
  const account = await signUp(page, instanceInvite(), instanceName);
  await joinCommunity(page, peer, setup);
  await openChannel(page, peer.communityName, peer.channelName);
  for (const id of setup.channel_ids) await peer.waitForDevices(2, id);
  return { setup, account, channel: peer.channelName };
}

test('edit, reply, react, pin and delete between the browser and the peer', async ({ page, peer, instanceName }) => {
  const { setup, account, channel } = await enter(page, peer, instanceName);

  const original = `original ${tag()}`;
  await sendText(page, channel, original);
  const mine = await peer.waitFor(original);
  expect(mine.msg_id).toMatch(HEX32);
  expect(mine.type).toBe(0);
  const user = mine.sender_user;
  const own = rowById(page, channel, mine.msg_id);
  await expect(own).toHaveAttribute('data-seq', String(mine.seq));

  for (const width of [360, 320]) { // 320 CSS px also represents a 1280 px window at 400% zoom.
    await page.setViewportSize({ width, height: 740 });
    // Below 48rem the shell is the phone shell; it re-mounts the panes once, and a focus given before that is lost.
    await expect(page.locator('.d-app-shell')).toHaveAttribute('data-layout', 'phone');
    await own.focus();
    await expect(rowToolbar(own)).toBeVisible();
    const narrowActionsFit = await rowToolbar(own).evaluate((bar) => [...bar.querySelectorAll('button')].every((button) => {
      const rect = button.getBoundingClientRect();
      return rect.left >= 0 && rect.right <= document.documentElement.clientWidth;
    }));
    expect(narrowActionsFit).toBe(true);
  }
  await page.setViewportSize({ width: 1280, height: 800 });
  await expect(page.locator('.d-app-shell')).not.toHaveAttribute('data-layout', 'phone'); // the panes re-mount once; wait for it

  // Edit in place from the empty composer.
  await composerBox(page, channel).focus();
  await expect(composerBox(page, channel)).toHaveValue('');
  await page.keyboard.press('ArrowUp');
  await expect(editorBox(page)).toBeFocused();
  await expect(editorBox(page)).toHaveValue(original);
  const edited = `edited ${tag()}`;
  await editorBox(page).fill(edited);
  await page.keyboard.press('Enter');
  await expect(own).toHaveAttribute('data-edited', 'true', { timeout: WAIT });
  await expect(own).toContainText(edited);
  await expect(own).not.toContainText(original);
  const editRow = await peer.waitForRow((m) => m.type === 1 && m.reply_to === mine.msg_id && m.body === edited, 'the browser’s edit');
  expect(editRow.sender_user).toBe(user);

  // The peer's message and its own edit.
  const theirs = `from the peer ${tag()}`;
  const sent = await peer.send(theirs);
  expect(sent.msg_id).toMatch(HEX32);
  const target = rowById(page, channel, sent.msg_id);
  await expect(target).toContainText(theirs, { timeout: WAIT });
  const theirsEdited = `the peer edited ${tag()}`;
  await peer.edit(sent.msg_id, theirsEdited);
  await expect(target).toHaveAttribute('data-edited', 'true', { timeout: WAIT });
  await expect(target).toContainText(theirsEdited);

  await target.focus();
  await expect(rowToolbar(target)).toBeVisible();
  await expectAccessible(page, 'conversation with the toolbar open');

  // A reply: the chip, the peer's reply_to, the reply line that jumps to the original and flashes it.
  await rowAction(target, 'reply');
  await expect(page.locator(CLASS.replyChip)).toContainText(copy('shell.composer.replying', { name: shownName(setup) }));
  await expect(composerBox(page, channel)).toBeFocused();
  const answer = `my answer ${tag()}`;
  await page.keyboard.type(answer);
  await page.keyboard.press('Enter');
  const answerRow = messageRow(page, channel, answer);
  await expect(answerRow).toHaveAttribute('data-state', 'ok', { timeout: WAIT });
  await expect(page.locator(CLASS.replyChip)).toHaveCount(0);
  const replied = await peer.waitForRow((m) => m.type === 0 && m.body === answer, 'the browser’s reply');
  expect(replied.reply_to).toBe(sent.msg_id);
  // The reply line's button is named by its visible text (SLICE-UX-07), so it is found by its place.
  const jump = answerRow.locator('.d-message-row__reply').getByRole('button');
  await expect(jump).toContainText(shownName(setup));
  await expect(jump).toContainText(theirsEdited);
  await watchFlash(page, sent.msg_id);
  await jump.click();
  await expect(target).toBeFocused();
  await expect.poll(() => flashSeen(page), { timeout: WAIT }).toBe(true);

  // Reactions are per user.
  const thumbs = copy('shell.emoji.01');
  await peer.react(sent.msg_id, THUMBS_UP, true);
  await expect(reactionChip(target, thumbs, 1)).toHaveAttribute('aria-pressed', 'false', { timeout: WAIT });
  await reactionChip(target, thumbs, 1).click();
  await expect(reactionChip(target, thumbs, 2)).toHaveAttribute('aria-pressed', 'true', { timeout: WAIT });
  const reacted = await peer.waitForRow((m) => m.type === 3 && m.reply_to === sent.msg_id && m.sender_user === user, 'the browser’s reaction');
  expect(reacted.body).toBe(THUMBS_UP);

  // A pin, listed with who pinned it. Like Discord's @everyone, a plain member may not pin (flow 09): the peer, the
  // server's owner, first gives everyone that right, and the browser's toolbar then offers it.
  await peer.allowEveryone(PERM_PIN);
  await target.focus();
  await expectRowMenuAction(target, 'pin', true);
  await rowAction(target, 'pin');
  await expect(target).toHaveAttribute('data-pinned', 'true', { timeout: WAIT });
  await channelRowMenuItem(page, channel, 'shell.pins.open');
  const pins = pinsDialog(page, channel);
  await expect(pins).toBeVisible({ timeout: WAIT });
  await expect(pins).toContainText(copy('shell.pins.by', { name: account.display }), { timeout: WAIT });
  await expect(pins).toContainText(theirsEdited);
  await expectAccessible(page, 'pins dialog');
  await pins.getByRole('button', { name: copy('shell.pins.jump'), exact: true }).click();
  await expect(target).toBeFocused();
  await channelRowMenuItem(page, channel, 'shell.pins.open');
  await expect(pins).toBeVisible({ timeout: WAIT });
  await page.keyboard.press('Escape');
  await expect(pins).toBeHidden();
  await peer.waitForRow((m) => m.type === 5 && m.reply_to === sent.msg_id && m.sender_user === user, 'the browser’s pin');

  // Unpin from the dialog: the row loses the pin and the peer receives type 6.
  await channelRowMenuItem(page, channel, 'shell.pins.open');
  await expect(pins).toBeVisible({ timeout: WAIT });
  await pins.getByRole('button', { name: copy('shell.message.unpin'), exact: true }).click();
  await expect(target).not.toHaveAttribute('data-pinned', 'true', { timeout: WAIT });
  await peer.waitForRow((m) => m.type === 6 && m.reply_to === sent.msg_id && m.sender_user === user, 'the browser’s unpin');
  await expect(pins).toBeVisible();
  await page.keyboard.press('Escape');
  await expect(pins).toBeHidden();

  // Delete for everyone, only through the dialog.
  await rowAction(own, 'delete');
  await expect(deleteDialog(page)).toBeVisible();
  await deleteDialog(page).getByRole('button', { name: copy('shell.delete.confirm'), exact: true }).click();
  await expect(own).toHaveAttribute('data-state', 'deleted', { timeout: WAIT });
  await expect(own).toBeFocused();
  await expect(own).not.toContainText(edited);
  await peer.waitForRow((m) => m.type === 2 && m.reply_to === mine.msg_id && m.body === '', 'the browser’s delete');
  await peer.waitForRow((m) => m.seq === mine.seq && m.deleted, 'the delivery service’s tombstone of the original');

  // The peer deletes its own message: A shows it deleted, without its text.
  await peer.deleteMessage(sent.msg_id, sent.seq);
  await expect(target).toHaveAttribute('data-state', 'deleted', { timeout: WAIT });
  await expect(target).not.toContainText(theirsEdited);
});

test('an edit or a delete by someone else changes nothing', async ({ page, peer, instanceName }) => {
  const { channel } = await enter(page, peer, instanceName);
  const body = `mine alone ${tag()}`;
  await sendText(page, channel, body);
  const mine = await peer.waitFor(body);
  const forged = `forged ${tag()}`;
  await peer.edit(mine.msg_id, forged);
  await peer.send('', undefined, { type: 2, replyTo: mine.msg_id });
  // Attacker statement: only the target's author's type 1/2 folds (L-CORE-33); the control below proves both forgeries were applied before the assertions.
  const control = `after the forgery ${tag()}`;
  await peer.send(control);
  await expect(messageRow(page, channel, control)).toHaveAttribute('data-state', 'ok', { timeout: WAIT });
  const own = rowById(page, channel, mine.msg_id);
  await expect(own).toContainText(body);
  await expect(own).toHaveAttribute('data-state', 'ok');
  await expect(own).not.toHaveAttribute('data-edited', 'true');
  await expect(logRegion(page, channel)).not.toContainText(forged);
});

test('mentions', async ({ page, peer, second, instanceName }) => {
  const { setup, account: a } = await enter(page, peer, instanceName, 2);
  const [one, two] = peer.channelNames;
  const [id1, id2] = setup.channel_ids;
  const hello = `hello from a ${tag()}`;
  await sendText(page, one, hello);
  const aUser = (await peer.waitFor(hello)).sender_user;

  const b = (await second.open()).page;
  const bAccount = await signUp(b, instanceInvite(), instanceName, 'second tester');
  await joinCommunity(b, peer, setup);
  await openChannel(b, peer.communityName, one);
  await peer.waitForDevices(3, id1);
  await peer.waitForDevices(3, id2);

  // A sits in the second channel, so the mention B picks must badge A's first channel (the whole chain).
  await channelRow(page, two).click();
  await expectComposerReady(page, two);
  await composerBox(b, one).focus();
  await b.keyboard.type(`@${a.username.slice(0, 6)}`);
  const list = mentionList(b);
  await expect(list).toBeVisible({ timeout: WAIT });
  await expect(list.getByRole('option')).toHaveCount(1);
  await expect(list.getByRole('option')).toContainText(`@${a.username}`);
  await expectAccessible(b, 'mention list');
  await b.keyboard.press('Enter');
  await expect(list).toBeHidden();
  await expect(composerBox(b, one)).toHaveValue(`@${a.username} `);
  const words = `hi ${tag()}`;
  await b.keyboard.type(words);
  await b.keyboard.press('Enter');
  await expect(messageRow(b, one, words)).toHaveAttribute('data-state', 'ok', { timeout: WAIT });
  const token = await peer.waitFor(`<@${aUser}> ${words}`);
  const bUser = token.sender_user;
  expect(bUser).toMatch(HEX32);
  expect(bUser).not.toBe(aUser);

  await expect(channelRow(page, one)).toHaveAttribute('data-mentions', '1', { timeout: WAIT });
  await channelRow(page, one).click();
  await expectComposerReady(page, one);
  const inA = messageRow(page, one, words);
  await expect(inA).toHaveAttribute('data-mention', 'true', { timeout: WAIT });
  await expect(inA).toContainText(`@${a.display}`);
  await expect(inA).not.toContainText('<@');
  await expect(messageRow(b, one, words)).not.toContainText('<@');

  const look = `look ${tag()}`;
  await peer.send(`<@${bUser}> ${look}`, id2);
  await expect(channelRow(b, two)).toHaveAttribute('data-mentions', '1', { timeout: WAIT });
  await channelRow(b, two).click();
  await expectComposerReady(b, two);
  const inB = messageRow(b, two, look);
  await expect(inB).toHaveAttribute('data-mention', 'true', { timeout: WAIT });
  await expect(inB).toContainText(`@${bAccount.display}`);
  await expect(inB).not.toContainText('<@');
});

test('images and files go both ways', async ({ page, peer, instanceName }) => {
  const { channel } = await enter(page, peer, instanceName);

  const drawn = await drawImage(page, { width: 320, height: 240, type: 'image/png', seed: 1 });
  const notes = randomBytes(70_000);
  await attachInput(page).setInputFiles([
    { name: 'drawn.png', mimeType: 'image/png', buffer: drawn.bytes },
    { name: 'notes.bin', mimeType: 'application/octet-stream', buffer: notes },
  ]);
  const entries = trayEntries(page);
  await expect(entries).toHaveCount(2, { timeout: WAIT });
  await expect(entries.nth(0)).toHaveAttribute('data-phase', 'ready', { timeout: WAIT });
  await expect(entries.nth(1)).toHaveAttribute('data-phase', 'ready', { timeout: WAIT });
  await expectAccessible(page, 'tray with two files');
  const text = `two files ${tag()}`;
  await composerBox(page, channel).fill(text);
  await composerBox(page, channel).press('Enter');
  await expect(messageRow(page, channel, text)).toHaveAttribute('data-state', 'ok', { timeout: WAIT });
  await expect(entries).toHaveCount(0);
  await expect(messageRow(page, channel, text).getByRole('img', { name: 'drawn.png', exact: true }))
    .toHaveAttribute('data-thumb', 'ready', { timeout: WAIT });

  const received = await peer.waitForRow((m) => m.body === text && m.attachments.length === 2, 'the browser’s two files');
  expect(received.attachments.map((x) => [x.index, x.name, x.mime, x.size, x.thumb])).toEqual([
    [0, 'drawn.png', 'image/png', drawn.bytes.length, true],
    [1, 'notes.bin', 'application/octet-stream', 70_000, false],
  ]);
  const png = await peer.fetchAttachment(received.seq, 0);
  expect(png.sha256).toBe(drawn.sha256);
  expect(png.thumb_sha256).toMatch(/^[0-9a-f]{64}$/);
  const bin = await peer.fetchAttachment(received.seq, 1);
  expect(bin.sha256).toBe(sha256Hex(notes));
  expect(bin.thumb_sha256).toBeNull();
  expect([bin.size, bin.mime, bin.name]).toEqual([70_000, 'application/octet-stream', 'notes.bin']);

  // The peer's image with its own WebP thumbnail, and a file.
  const map = await drawImage(page, { width: 320, height: 240, type: 'image/png', seed: 2 });
  const thumb = await drawImage(page, { width: 80, height: 60, type: 'image/webp', quality: 0.5, seed: 2 });
  expect(map.bytes.length).toBeLessThanOrEqual(1_048_576);
  expect(thumb.bytes.length).toBeLessThanOrEqual(8_176);
  const mapBody = `map ${tag()}`;
  const mapSent = await peer.attach({ name: 'map.png', mime: 'image/png', body: mapBody, bytesHex: map.bytes.toString('hex'), w: 320, h: 240, thumbHex: thumb.bytes.toString('hex') });
  expect(mapSent.sha256).toBe(map.sha256);
  const dataBody = `data ${tag()}`;
  const dataSent = await peer.attach({ name: 'data.bin', mime: 'application/octet-stream', body: dataBody, size: 70_000, seed: 7 });

  const mapRow = rowById(page, channel, mapSent.msg_id);
  const mapImg = mapRow.getByRole('img', { name: 'map.png', exact: true });
  await expect(mapImg).toHaveAttribute('data-thumb', 'ready', { timeout: WAIT });
  await expect.poll(() => mapImg.evaluate((el: HTMLImageElement) => (el.complete ? el.naturalWidth : 0)), { timeout: WAIT }).toBeGreaterThan(0);
  await mapRow.getByRole('button', { name: copy('shell.attachment.open', { name: 'map.png' }), exact: true }).click();
  const box = lightbox(page);
  await expect(box).toBeVisible({ timeout: WAIT });
  const full = box.getByRole('img', { name: 'map.png', exact: true });
  await expect.poll(() => full.evaluate((el: HTMLImageElement) => (el.complete ? el.naturalWidth : 0)), { timeout: WAIT }).toBe(320);
  await expectAccessible(page, 'lightbox');
  const [mapDownload] = await Promise.all([
    page.waitForEvent('download', { timeout: WAIT }),
    box.getByRole('button', { name: copy('shell.lightbox.save'), exact: true }).click(),
  ]);
  expect(sha256Hex(readFileSync(await mapDownload.path()))).toBe(map.sha256);
  await page.keyboard.press('Escape');
  await expect(box).toHaveCount(0);

  const save = messageRow(page, channel, dataBody).getByRole('button', { name: copy('shell.attachment.save', { name: 'data.bin' }), exact: true });
  await expect(save).toBeVisible({ timeout: WAIT });
  const [download] = await Promise.all([page.waitForEvent('download', { timeout: WAIT }), save.click()]);
  expect(download.suggestedFilename()).toBe('data.bin');
  expect(sha256Hex(readFileSync(await download.path()))).toBe(dataSent.sha256);

  // Deleting the browser's message removes its references from the instance.
  for (const x of received.attachments) expect(await peer.blobStatus(x.blob_id)).toBe(200);
  const own = rowById(page, channel, received.msg_id);
  await rowAction(own, 'delete');
  await deleteDialog(page).getByRole('button', { name: copy('shell.delete.confirm'), exact: true }).click();
  await expect(own).toHaveAttribute('data-state', 'deleted', { timeout: WAIT });
  for (const x of received.attachments) await expect.poll(() => peer.blobStatus(x.blob_id), { timeout: WAIT }).toBe(404);
  await peer.deleteMessage(mapSent.msg_id, mapSent.seq);
  await expect(mapRow).toHaveAttribute('data-state', 'deleted', { timeout: WAIT });
  await expect(mapRow.getByRole('img', { name: 'map.png', exact: true })).toHaveCount(0);
  await expect.poll(() => peer.blobStatus(mapSent.blob_id), { timeout: WAIT }).toBe(404);
});

test('a 25 MB file goes through and one byte more is refused', async ({ page, context, peer, instanceName }) => {
  const puts: string[] = [];
  context.on('request', (r) => {
    if (r.method() === 'PUT' && BLOB_PUT.test(new URL(r.url()).pathname)) puts.push(r.url());
  });
  const { channel } = await enter(page, peer, instanceName);

  const big = randomBytes(26_214_400);
  await attachInput(page).setInputFiles({ name: 'big.bin', mimeType: 'application/octet-stream', buffer: big });
  await expect(trayEntries(page)).toHaveCount(1, { timeout: WAIT });
  await expect(trayEntries(page).first()).toHaveAttribute('data-phase', 'ready', { timeout: WAIT });
  expect(puts).toHaveLength(1);
  const text = `big ${tag()}`;
  await composerBox(page, channel).fill(text);
  await composerBox(page, channel).press('Enter');
  await expect(messageRow(page, channel, text)).toHaveAttribute('data-state', 'ok', { timeout: WAIT });
  const got = await peer.waitForRow((m) => m.body === text && m.attachments.length === 1, 'the 25 MB file');
  expect(got.attachments[0].size).toBe(26_214_400);
  const fetched = await peer.fetchAttachment(got.seq, 0);
  expect(fetched.sha256).toBe(sha256Hex(big));
  expect(fetched.size).toBe(26_214_400);

  await attachInput(page).setInputFiles({ name: 'over.bin', mimeType: 'application/octet-stream', buffer: Buffer.alloc(26_214_401, 7) });
  await expect(page.getByRole('alert').filter({ hasText: copy('shell.tray.tooLarge', { name: 'over.bin' }) })).toBeVisible({ timeout: WAIT });
  await expect(trayEntries(page)).toHaveCount(0);

  // The positive control: the next upload is seen, so the refused one made no request.
  await attachInput(page).setInputFiles({ name: 'small.bin', mimeType: 'application/octet-stream', buffer: Buffer.from('ten bytes!') });
  await expect(trayEntries(page).first()).toHaveAttribute('data-phase', 'ready', { timeout: WAIT });
  expect(puts).toHaveLength(2);
  await trayEntries(page).first().getByRole('button', { name: copy('shell.tray.remove', { name: 'small.bin' }), exact: true }).click();
  await expect(trayEntries(page)).toHaveCount(0, { timeout: WAIT });
});

test('B sees what A makes', async ({ page, peer, second, instanceName }) => {
  const { setup, channel } = await enter(page, peer, instanceName);
  const b = (await second.open()).page;
  await signUp(b, instanceInvite(), instanceName, 'second tester');
  await joinCommunity(b, peer, setup);
  await openChannel(b, peer.communityName, channel);
  await peer.waitForDevices(3);

  const body = `fold me ${tag()}`;
  await sendText(page, channel, body);
  const mine = await peer.waitFor(body);
  const inB = rowById(b, channel, mine.msg_id);
  await expect(inB).toContainText(body, { timeout: WAIT });

  const own = rowById(page, channel, mine.msg_id);
  await rowAction(own, 'edit');
  await expect(editorBox(page)).toBeFocused();
  const edited = `folded ${tag()}`;
  await editorBox(page).fill(edited);
  await page.keyboard.press('Enter');
  await expect(inB).toHaveAttribute('data-edited', 'true', { timeout: WAIT });
  await expect(inB).toContainText(edited);

  await rowAction(own, 'react');
  const grid = page.getByRole('dialog', { name: copy('shell.emoji.label'), exact: true });
  await expect(grid).toBeVisible();
  await expectAccessible(page, 'emoji grid');
  await grid.getByRole('button', { name: copy('shell.emoji.01'), exact: true }).click();
  await expect(grid).toBeHidden();
  await expect(reactionChip(own, copy('shell.emoji.01'), 1)).toHaveAttribute('aria-pressed', 'true', { timeout: WAIT });
  await expect(reactionChip(inB, copy('shell.emoji.01'), 1)).toHaveAttribute('aria-pressed', 'false', { timeout: WAIT });

  // A plain member may not pin (flow 09): the peer, the server's owner, gives everyone that right first.
  await peer.allowEveryone(PERM_PIN);
  await own.focus();
  await expectRowMenuAction(own, 'pin', true);
  await rowAction(own, 'pin');
  await expect(inB).toHaveAttribute('data-pinned', 'true', { timeout: WAIT });

  // A's image and file, seen, opened and saved by another user's browser under the served CSP.
  const drawn = await drawImage(page, { width: 320, height: 240, type: 'image/png', seed: 3 });
  const notes = randomBytes(70_000);
  await attachInput(page).setInputFiles([
    { name: 'drawn.png', mimeType: 'image/png', buffer: drawn.bytes },
    { name: 'notes.bin', mimeType: 'application/octet-stream', buffer: notes },
  ]);
  await expect(trayEntries(page)).toHaveCount(2, { timeout: WAIT });
  await expect(trayEntries(page).nth(0)).toHaveAttribute('data-phase', 'ready', { timeout: WAIT });
  await expect(trayEntries(page).nth(1)).toHaveAttribute('data-phase', 'ready', { timeout: WAIT });
  const text = `for b ${tag()}`;
  await composerBox(page, channel).fill(text);
  await composerBox(page, channel).press('Enter');
  const rowB = messageRow(b, channel, text);
  const img = rowB.getByRole('img', { name: 'drawn.png', exact: true });
  await expect(img).toHaveAttribute('data-thumb', 'ready', { timeout: WAIT });
  await expect.poll(() => img.evaluate((el: HTMLImageElement) => (el.complete ? el.naturalWidth : 0)), { timeout: WAIT }).toBeGreaterThan(0);
  await rowB.getByRole('button', { name: copy('shell.attachment.open', { name: 'drawn.png' }), exact: true }).click();
  const box = lightbox(b);
  await expect(box).toBeVisible({ timeout: WAIT });
  const full = box.getByRole('img', { name: 'drawn.png', exact: true });
  await expect.poll(() => full.evaluate((el: HTMLImageElement) => (el.complete ? el.naturalWidth : 0)), { timeout: WAIT }).toBe(320);
  const [imageDownload] = await Promise.all([
    b.waitForEvent('download', { timeout: WAIT }),
    box.getByRole('button', { name: copy('shell.lightbox.save'), exact: true }).click(),
  ]);
  expect(sha256Hex(readFileSync(await imageDownload.path()))).toBe(drawn.sha256);
  await b.keyboard.press('Escape');
  await expect(box).toHaveCount(0);
  const save = rowB.getByRole('button', { name: copy('shell.attachment.save', { name: 'notes.bin' }), exact: true });
  const [download] = await Promise.all([b.waitForEvent('download', { timeout: WAIT }), save.click()]);
  expect(sha256Hex(readFileSync(await download.path()))).toBe(sha256Hex(notes));
  expect(drawn.sha256).toMatch(/^[0-9a-f]{64}$/);
});

test('a second browser of the same account edits and deletes the first browser’s message', async ({ page, peer, second, instanceName }) => {
  const { account, channel } = await enter(page, peer, instanceName);
  const b = (await second.open()).page;
  await signIn(b, account, instanceName);
  await openChannel(b, peer.communityName, channel);
  await peer.waitForDevices(3);

  const original = `from first browser ${tag()}`;
  await sendText(page, channel, original);
  const sent = await peer.waitFor(original);
  const onA = rowById(page, channel, sent.msg_id);
  const onB = rowById(b, channel, sent.msg_id);
  await expect(onB).toContainText(original, { timeout: WAIT });
  await rowAction(onB, 'edit');
  const edited = `edited on second ${tag()}`;
  await editorBox(b).fill(edited);
  await editorBox(b).press('Enter');
  await expect(onA).toContainText(edited, { timeout: WAIT });
  await expect(onA).toHaveAttribute('data-edited', 'true');
  const editRow = await peer.waitForRow((m) => m.type === 1 && m.reply_to === sent.msg_id && m.body === edited, 'second browser edit');
  expect(editRow.sender_user).toBe(sent.sender_user);

  await rowAction(onB, 'delete');
  await deleteDialog(b).getByRole('button', { name: copy('shell.delete.confirm'), exact: true }).click();
  await expect(onA).toHaveAttribute('data-state', 'deleted', { timeout: WAIT });
  await expect(onA).not.toContainText(edited);
  const deleteRow = await peer.waitForRow((m) => m.type === 2 && m.reply_to === sent.msg_id, 'second browser delete');
  expect(deleteRow.sender_user).toBe(sent.sender_user);
});
