// VOICE-E2E (web-3 task 10): a reload mid-call is a leave the other side sees, and the rejoin takes a fresh leaf of the
// call group; a person's leave hands focus back to the voice row (flow 06 § Focus rules). Every step waits for the call
// slice, the voice snapshot or the DOM, never for a duration.
import type { Page } from '@playwright/test';
import { expect, leaveControl, test } from './support/peer-browser';
import { watchContexts } from './support/leaks';
import {
  POLL, VoicePeer, WAIT, callSlice, connected, copy, dock, expectShellReady, heard, joinVoice, onboard, openVoiceOnly, self,
  snapshotDevices, stage, tile, voiceRow,
} from './support/voice';

watchContexts(test);

/** A page's call roster (the worker's call slice): each device with its leaf, and the epoch. */
async function roster(page: Page): Promise<{ epoch: number; devices: { deviceId: string; leaf: number }[] }> {
  const s = await callSlice(page);
  return { epoch: Number(s?.epoch ?? -1), devices: (s?.roster ?? []).map((r) => ({ deviceId: r.deviceId, leaf: r.leaf })) };
}
const rosterDevices = async (page: Page) => (await roster(page)).devices.map((d) => d.deviceId).sort();

test('a reload mid-call is a leave bob sees, the rejoin is a fresh leaf, and leave returns focus to the voice row', async ({ peerBrowser }) => {
  const peer = peerBrowser.track(await VoicePeer.start());
  const alice = await peerBrowser.open('alice');
  const bob = await peerBrowser.open('bob');
  try {
    const s = await peer.setup();
    await onboard(alice, peer, 'alice');
    await onboard(bob, peer, 'bob');
    await peer.seat(2); // no hello: a voice tile names a member the page loaded after it (task 10, concern 4)
    const a = await self(alice.page);
    const b = await self(bob.page);
    const call = s.voice_channel_id;

    await joinVoice(alice.page, peer.communityName, peer.voiceName);
    await joinVoice(bob.page, peer.communityName, peer.voiceName);
    await expect(tile(bob.page, 'alice')).toHaveAttribute('data-voice-state', 'ok', { timeout: WAIT });
    await heard(bob.page, a.device);
    await expect.poll(() => rosterDevices(bob.page), { timeout: WAIT }).toEqual([a.device, b.device].sort());
    const before = await roster(bob.page);

    await test.step('a reload mid-call: bob sees alice leave the room', async () => {
      await alice.page.reload();
      // The pagehide leave reaches the room (LiveKit's participant_left, the snapshot).
      await expect(tile(bob.page, 'alice')).toHaveCount(0, { timeout: WAIT });
      await expect.poll(() => snapshotDevices(bob.page, call), { timeout: WAIT }).toEqual([b.device]);
    });

    await test.step('alice rejoins at once with a fresh leaf, her Remove committed or not yet (task 10, concern 2; the wait itself is proven below)', async () => {
      await expectShellReady(alice.page);
      // No wait for bob to commit the Remove her leave caused: a resync refused while it stands waits for the commit.
      await joinVoice(alice.page, peer.communityName, peer.voiceName);
      await expect.poll(() => rosterDevices(bob.page), { timeout: WAIT }).toEqual([a.device, b.device].sort());
      const back = await roster(bob.page);
      // One leaf of alice's device, taken in an epoch after her Remove: a new membership, not the old one kept.
      expect(back.devices.filter((d) => d.deviceId === a.device)).toHaveLength(1);
      expect(back.epoch).toBeGreaterThan(before.epoch + 1);
      await expect(tile(bob.page, 'alice')).toHaveAttribute('data-voice-state', 'ok', { timeout: WAIT });
      await expect(tile(alice.page, 'bob')).toHaveAttribute('data-voice-state', 'ok', { timeout: WAIT });
    });

    await test.step('leave from the call returns focus to the voice row', async () => {
      await leaveControl(alice.page).click();
      await expect(dock(alice.page)).toHaveCount(0, { timeout: WAIT });
      await expect(voiceRow(alice.page, peer.voiceName)).toBeFocused({ timeout: WAIT });
      await expect(tile(bob.page, 'alice')).toHaveCount(0, { timeout: WAIT });
    });

    await test.step('alone in voice: a leave and an immediate rejoin connect at once (task 10, concern 2)', async () => {
      await leaveControl(bob.page).click();
      await expect(dock(bob.page)).toHaveCount(0, { timeout: WAIT });
      await joinVoice(alice.page, peer.communityName, peer.voiceName);
      await expect.poll(() => snapshotDevices(alice.page, call), { timeout: WAIT }).toEqual([a.device]);
      await leaveControl(alice.page).click();
      await expect(dock(alice.page)).toHaveCount(0, { timeout: WAIT });
      // The dock goes at once, but the stage keeps its in-call layout while the worker's leave runs (phase 'leaving'); when
      // that ends the stage settles idle and its join button moves. A press during the move was lost (Firefox, PR #65 run
      // 39671998576), so the press waits for the settled stage: idle, and the ended call gone from the room's snapshot.
      await expect(stage(alice.page)).toHaveAttribute('data-call-phase', 'idle', { timeout: WAIT });
      await expect.poll(() => snapshotDevices(alice.page, call), { timeout: WAIT }).toEqual([]);
      // Nobody can commit the Remove of the only leaf; the leave ended the call, so the rejoin registers a fresh group
      // at once: the room's departure timeout (90 s in this suite) plays no part.
      await stage(alice.page).getByRole('button', { name: copy('callStage.preview.join'), exact: true }).click();
      await expect(stage(alice.page)).toHaveAttribute('data-call-phase', 'in', { timeout: 10_000 });
    });
  } finally {
    await peerBrowser.close(bob);
    await peerBrowser.close(alice);
    await peer.close();
  }
});

// Concern 2's wait itself (task-10 fix round, f10app m6): the other member holds its sync, so the Remove that alice's
// reload caused is still outstanding when she rejoins. The instance refuses her resync (409 E_REMOVE_PENDING, guard 1);
// her join waits, says so in the dock's status region, and lands in the same call group once the member commits the
// Remove. The member is the native peer, which commits only when told to; it holds a leaf of the call group and never
// enters the room.
test('a rejoin whose Remove nobody has committed yet waits, says so, and joins the same call once it is committed', async ({ peerBrowser }) => {
  const peer = peerBrowser.track(await VoicePeer.start());
  const alice = await peerBrowser.open('alice');
  try {
    const s = await peer.setup();
    await onboard(alice, peer, 'alice');
    await peer.seat(1);
    const a = await self(alice.page);
    const call = s.voice_channel_id;
    const joined = await peer.callJoin(call);
    await joinVoice(alice.page, peer.communityName, peer.voiceName);
    await peer.waitForCallMembers(call, (d) => d.includes(a.device), 'lists alice');
    await expect.poll(async () => (await peer.callRead(call)).participants.map((p) => p.device), POLL).toEqual([a.device]);

    await alice.page.reload();
    await expectShellReady(alice.page);
    // The room saw her leave (its participant_left made the instance propose the Remove of her leaf); the peer has not
    // synced since, so nobody has committed it.
    await expect.poll(async () => (await peer.callRead(call)).participants.length, POLL).toBe(0);
    await openVoiceOnly(alice.page, peer.communityName, peer);
    await stage(alice.page).getByRole('button', { name: copy('callStage.preview.join'), exact: true }).click();
    await expect.poll(async () => {
      const c = await callSlice(alice.page);
      return c?.phase === 'joining' && c.rejoining;
    }, { ...POLL, message: 'alice\'s join waits for the Remove (call.rejoining)' }).toBe(true);
    await expect(dock(alice.page).locator('[role="status"]'))
      .toHaveText(copy('voice.announce.rejoining', { where: peer.voiceName }), { timeout: WAIT });

    // The peer syncs and commits the Remove: alice's wait sees the group move on and her join lands in it.
    await expect.poll(async () => {
      await peer.sync();
      await peer.callMembers(call);
      return (await callSlice(alice.page))?.phase;
    }, { ...POLL, message: 'alice is in once the peer committed her Remove' }).toBe('in');
    await connected(alice.page);
    expect((await callSlice(alice.page))?.roster.map((r) => r.deviceId).sort()).toEqual([a.device, s.device_id].sort());
    expect((await peer.callRead(call)).group, 'the call group the peer joined is still the call\'s').toBe(joined.group);
    expect((await callSlice(alice.page))?.rejoining).toBe(false);
  } finally {
    await peerBrowser.close(alice);
    await peer.close();
  }
});
