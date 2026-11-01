// VOICE-E2E (web-3 task 10): a poisoned call group stops the call in the browser (decision 4, RV-1, RV-3), through the
// real SFU, against a native peer that poisons it:
// (a) before join: a contact whose identity changed is in the call group, so the join is refused before anything is
//     posted, and the browser never reaches the group or the room; the notice names the contact, and verifying them
//     offers the join again without a reload;
// (b) mid-call: a device that fails the signed-list check joins, so the browser leaves and its leaf goes;
// (c) mid-call identity change (RV-3): the call pauses on each browser that saw it, without ending for anyone, until
//     the contact is verified or leaves;
// DM: a blocked DM call leaves the DM's composer as the text group has it.
// Attackers: (a) and (c) a contact's identity replaced by an instance or account takeover (rotate_umk); (b) a device
// the instance admits but the owner's signed device list does not vouch for (a forged tier, forge_device).
import type { BrowserContext, Page } from '@playwright/test';
import { expect, mediaWorkers, test } from './support/peer-browser';
import { watchContexts } from './support/leaks';
import {
  POLL, VoicePeer, WAIT, callSlice, changedSeen, connected, copy, dock, framesPass, heard, heardCount, introduce, joinVoice, noiseOff,
  onboard, openVoiceOnly, peerName, self, sentCount, stage, stripButton, tile,
} from './support/voice';
import { dmComposer, dmsNav, homeTile, openSettings } from '../web/support/app';

watchContexts(test);

/** The stage's (or DM strip's) notice text, as the person reads it. */
function notice(page: Page, text: string) {
  return page.locator('.dw-voice-stage, .dw-call-strip').getByText(text, { exact: true });
}

/** Every call-path write a context sends from now on (its pages and their workers): a call group's registration or
 *  external commit, a call start (task-10 review M11). */
const CALL_WRITE = /^\/v1\/(groups|groups\/[0-9a-f]{32}\/resync|channels\/[0-9a-f]{32}\/calls)$/;
function callWrites(context: BrowserContext): string[] {
  const seen: string[] = [];
  context.on('request', (r) => {
    const path = new URL(r.url()).pathname;
    if (r.method() === 'POST' && CALL_WRITE.test(path)) seen.push(`POST ${path}`);
  });
  return seen;
}

/** Frames `listener` authenticated from `device` while `speaker` encrypts a fixed number of its own frames (its frame
 *  clock, never a sleep), after a settle window for frames already in flight. The listener's media workers must be the
 *  same ones before and after, and answer both reads (heardCount throws otherwise): a worker that died or restarted never
 *  reads as "nothing heard" (task-10 review I3). */
async function heardOver(listener: Page, speaker: Page, device: string): Promise<number> {
  await framesPass(speaker, await sentCount(speaker), 10);
  const workers = mediaWorkers(listener);
  expect(workers.length, 'the listener runs a media worker').toBeGreaterThan(0);
  const h0 = await heardCount(listener, device);
  await framesPass(speaker, await sentCount(speaker), 25);
  const h1 = await heardCount(listener, device);
  const after = mediaWorkers(listener);
  expect(after.length === workers.length && after.every((w, i) => w === workers[i]), 'the same media workers before and after the sample')
    .toBe(true);
  return h1 - h0;
}

test('(a) a changed contact in the call group refuses the join before any call write is sent; the room never sees the browser', async ({ peerBrowser }) => {
  const peer = peerBrowser.track(await VoicePeer.start());
  const alice = await peerBrowser.open('alice');
  const bob = await peerBrowser.open('bob');
  try {
    const s = await peer.setup();
    const call = s.voice_channel_id;
    await onboard(alice, peer, 'alice');
    await introduce(peer, [alice]);
    const a = await self(alice.page);

    // The peer's identity is replaced (a new UMK and device) and alice's text group shows it; then the new device opens
    // the voice's call group. The peer first takes in what the browsers committed (its rotation commits in that group).
    await peer.sync();
    await peer.rotateUmk();
    await changedSeen(alice.page, s.user_id);
    await peer.callJoin(call);
    await openVoiceOnly(alice.page, peer.communityName, peer);
    const aliceWrites = callWrites(alice.context);
    await stage(alice.page).getByRole('button', { name: copy('callStage.preview.join'), exact: true }).click();
    await expect(stage(alice.page)).toHaveAttribute('data-call-phase', 'blocked', { timeout: WAIT });
    expect((await callSlice(alice.page))?.error?.code).toBe('E_UMK_CHANGED');
    // Nothing of the join left the browser: no registration, no external commit, no call start.
    expect(aliceWrites, 'call writes alice sent for the refused join').toEqual([]);
    // The native peer holds a leaf of the group but never enters the room: the core's refusal names it from the call
    // tree, so the notice names the changed contact (task 10, concern 3).
    const name = peerName(s);
    await expect(notice(alice.page, copy('voice.blockedChanged', { name }))).toBeVisible({ timeout: WAIT });
    await expect(dock(alice.page)).toHaveCount(0);
    // The join stays held while the contact is changed (flow 06: no join button until the cause is gone).
    const join = stage(alice.page).getByRole('button', { name: copy('callStage.preview.join'), exact: true });
    await expect(join).toHaveCount(0);
    expect((await callSlice(alice.page))?.error?.code).toBe('E_UMK_CHANGED');

    await test.step('alice verifies the peer: the join is offered again without a reload', async () => {
      await openSettings(alice.page, 'privacy');
      await alice.page.getByRole('button', { name: copy('privacy.compare', { name }) }).click();
      const dialog = alice.page.getByRole('dialog', { name: copy('privacy.title') });
      await expect(dialog).toBeVisible({ timeout: WAIT });
      await dialog.getByRole('button', { name: copy('privacy.copy') }).click();
      await dialog.getByRole('checkbox', { name: copy('privacy.confirm') }).check();
      await dialog.getByRole('button', { name: copy('privacy.verify') }).click();
      await expect(dialog).toHaveCount(0, { timeout: WAIT });
      await alice.page.getByRole('button', { name: copy('settings.close') }).click();
      await expect(join).toHaveCount(1, { timeout: WAIT });
    });

    // Positive control: bob, who met the peer only after the rotation, joins after alice's attempt; the peer's group
    // and the room both see him, so a device that had reached either would be seen too.
    await onboard(bob, peer, 'bob');
    await introduce(peer, [bob], 2);
    const b = await self(bob.page);
    const bobWrites = callWrites(bob.context);
    await joinVoice(bob.page, peer.communityName, peer.voiceName);
    await peer.waitForCallMembers(call, (d) => d.includes(b.device), 'lists bob');
    // The recorder sees a join's writes (worker requests included): bob's external commit and his call start.
    expect(bobWrites.some((w) => w.endsWith('/resync')), `bob's call writes ${JSON.stringify(bobWrites)}`).toBe(true);
    expect(bobWrites.some((w) => w.endsWith('/calls')), `bob's call writes ${JSON.stringify(bobWrites)}`).toBe(true);
    expect(aliceWrites, 'call writes alice sent at any time').toEqual([]);
    await expect.poll(async () => (await peer.callRead(call)).participants.map((p) => p.device), POLL).toContain(b.device);
    const members = (await peer.callMembers(call)).devices;
    const room = (await peer.callRead(call)).participants.map((p) => p.device);
    expect(members, 'the call group after bob joined').not.toContain(a.device);
    expect(room, 'the room after bob joined').not.toContain(a.device);
    await expect(stage(alice.page)).toHaveAttribute('data-call-phase', 'blocked');
  } finally {
    await peerBrowser.close(bob);
    await peerBrowser.close(alice);
    await peer.close();
  }
});

test('(b) a device that fails the signed-list check joins mid-call: alice leaves, her dock goes and so does her leaf', async ({ peerBrowser }) => {
  const peer = peerBrowser.track(await VoicePeer.start());
  const alice = await peerBrowser.open('alice');
  try {
    const s = await peer.setup({ password: 'peer-password-1234' });
    const call = s.voice_channel_id;
    await onboard(alice, peer, 'alice');
    await introduce(peer, [alice]);
    const a = await self(alice.page);
    await peer.callJoin(call);
    await joinVoice(alice.page, peer.communityName, peer.voiceName);
    await peer.waitForCallMembers(call, (d) => d.includes(a.device), 'lists alice');

    // A device of the peer that the instance admits but whose signed-list entry carries the wrong tier.
    await peer.forgeDevice();
    await peer.callJoin(call, 'second');
    await expect(notice(alice.page, copy('voice.blockedLeft'))).toBeVisible({ timeout: WAIT });
    await expect(dock(alice.page)).toHaveCount(0, { timeout: WAIT });
    await expect(stage(alice.page)).toHaveAttribute('data-call-phase', 'blocked');
    // Product walk M10: the block came from the call group only, so the join is offered again, and the notice says so.
    await expect(stage(alice.page).getByRole('button', { name: copy('callStage.preview.join'), exact: true })).toBeVisible();
    await peer.waitForCallMembers(call, (d) => !d.includes(a.device), 'drops alice');
    await expect.poll(async () => (await peer.callRead(call)).participants.map((p) => p.device), POLL).not.toContain(a.device);
  } finally {
    await peerBrowser.close(alice);
    await peer.close();
  }
});

test('(c) a contact whose identity changes mid-call pauses the call for each browser until verified or gone (RV-3)', async ({ peerBrowser }) => {
  const peer = peerBrowser.track(await VoicePeer.start());
  const alice = await peerBrowser.open('alice');
  const bob = await peerBrowser.open('bob');
  try {
    const s = await peer.setup();
    const call = s.voice_channel_id;
    await onboard(alice, peer, 'alice');
    await onboard(bob, peer, 'bob');
    await introduce(peer, [alice, bob]);
    const a = await self(alice.page);
    const b = await self(bob.page);
    const name = peerName(s);
    await noiseOff(alice.page);
    await noiseOff(bob.page);
    await peer.callJoin(call);
    await joinVoice(alice.page, peer.communityName, peer.voiceName);
    await joinVoice(bob.page, peer.communityName, peer.voiceName);
    await heard(alice.page, b.device);
    await heard(bob.page, a.device);

    // Positive control for the pause's frame check below: while nothing is paused, the same sample sees frames.
    await peer.waitForCallMembers(call, (d) => d.includes(a.device) && d.includes(b.device), 'lists alice and bob');
    expect(await heardOver(alice.page, bob.page, b.device), 'frames alice verified from bob before the pause').toBeGreaterThan(0);
    expect(await heardOver(bob.page, alice.page, a.device), 'frames bob verified from alice before the pause').toBeGreaterThan(0);

    // The peer's identity is replaced while its old device is still in the call group (the plan's RV-3 order: an
    // attacker does not first remove the old leaf), and its new device rejoins. A list refresh through the old-key leaf
    // no longer ends the pending change (51135d9), so the call pauses rather than failing with E_CREDENTIAL.
    await peer.sync();
    await peer.rotateUmk();
    await changedSeen(alice.page, s.user_id);
    await changedSeen(bob.page, s.user_id);
    await peer.callJoin(call);
    const paused = copy('voice.paused', { name });
    for (const p of [alice.page, bob.page]) {
      await expect(stage(p)).toHaveAttribute('data-call-phase', 'paused', { timeout: WAIT });
      await expect(stage(p).getByText(paused, { exact: true })).toBeVisible({ timeout: WAIT });
    }
    // The call does not end for anyone: both stay in the group.
    await peer.waitForCallMembers(call, (d) => d.includes(a.device) && d.includes(b.device), 'keeps alice and bob');

    // Nothing is heard while paused. Each side's own frames are the clock (a muted sender still encodes); after a settle
    // window for frames already in flight, neither counter moves for a fixed number of them.
    expect(await heardOver(alice.page, bob.page, b.device), 'frames alice verified from bob while paused').toBe(0);
    expect(await heardOver(bob.page, alice.page, a.device), 'frames bob verified from alice while paused').toBe(0);

    await test.step('alice compares and verifies the peer: her call resumes', async () => {
      await stage(alice.page).getByRole('button', { name: copy('voice.pausedAction'), exact: true }).click();
      const dialog = alice.page.getByRole('dialog', { name: copy('privacy.title') });
      await expect(dialog).toBeVisible({ timeout: WAIT });
      await dialog.getByRole('button', { name: copy('privacy.copy') }).click();
      await dialog.getByRole('checkbox', { name: copy('privacy.confirm') }).check();
      await dialog.getByRole('button', { name: copy('privacy.verify') }).click();
      await expect(dialog).toHaveCount(0, { timeout: WAIT });
      await alice.page.keyboard.press('Escape');
      await expect(alice.page.getByRole('dialog', { name: copy('settings.title'), exact: true })).toBeHidden({ timeout: WAIT });
      await expect(stage(alice.page)).toHaveAttribute('data-call-phase', 'in', { timeout: WAIT });
      await expect(stage(bob.page)).toHaveAttribute('data-call-phase', 'paused');
    });

    await test.step('the peer leaves: bob resumes, and both hear each other again', async () => {
      const h0 = await heardCount(alice.page, b.device);
      const h1 = await heardCount(bob.page, a.device);
      await peer.callLeave(call);
      await expect(stage(bob.page)).toHaveAttribute('data-call-phase', 'in', { timeout: WAIT });
      await connected(bob.page);
      await expect.poll(() => heardCount(alice.page, b.device), POLL).toBeGreaterThan(h0);
      await expect.poll(() => heardCount(bob.page, a.device), POLL).toBeGreaterThan(h1);
      await expect(tile(alice.page, 'bob')).toHaveAttribute('data-voice-state', 'ok', { timeout: WAIT });
    });
  } finally {
    await peerBrowser.close(bob);
    await peerBrowser.close(alice);
    await peer.close();
  }
});

test('DM: a blocked DM call leaves the DM composer enabled', async ({ peerBrowser }) => {
  const peer = peerBrowser.track(await VoicePeer.start());
  const alice = await peerBrowser.open('alice');
  try {
    await peer.setup({ password: 'peer-password-1234' });
    await onboard(alice, peer, 'alice');
    await introduce(peer, [alice]);
    const a = await self(alice.page);
    const dm = await peer.openDm(a.user);
    await expect.poll(async () => (await peer.syncDm(dm.channel_id)).members, POLL).toBe(2);
    await homeTile(alice.page).click();
    await dmsNav(alice.page).locator('.d-chrow').first().click();
    await expect(alice.page).toHaveURL(new RegExp(`/dm/${dm.channel_id}$`), { timeout: WAIT });
    await expect(dmComposer(alice.page)).toBeEnabled({ timeout: WAIT });

    await peer.callJoin(dm.channel_id);
    // The peer holds the call group but is in no room: no live call, so the DM row menu offers `call` (Ruling Z).
    await stripButton(alice.page, 'call.start').click();
    await connected(alice.page);
    await peer.waitForCallMembers(dm.channel_id, (d) => d.includes(a.device), 'lists alice');
    await peer.forgeDevice();
    await peer.callJoin(dm.channel_id, 'second');
    await expect(notice(alice.page, copy('voice.blockedLeft'))).toBeVisible({ timeout: WAIT });
    await expect(dock(alice.page)).toHaveCount(0, { timeout: WAIT });
    // The call group's block is the call's only: the DM's text group is unaffected and so is its composer.
    await expect(dmComposer(alice.page)).toBeEnabled();
    await expect(dmComposer(alice.page)).not.toHaveAttribute('aria-disabled', 'true');
  } finally {
    await peerBrowser.close(alice);
    await peer.close();
  }
});
