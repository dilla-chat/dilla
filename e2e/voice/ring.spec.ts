// VOICE-E2E (web-3 task 10): DM calls ring (decision 6, R1–R8). Alice calls bob from their DM; bob's open tab shows
// the ring banner, plays the ring and raises one desktop notification while hidden; answer, decline, the ring's end on
// the instance's clock, a busy ring, and a ring of a DM whose identity changed (decision 4 extended to rings: none).
// A tab is reported hidden through peer-browser.ts's switch: a headless page is always visible, and a ring notifies only
// while hidden (flow 06). Every wait is on a state; the 30 s ring is ended by moving the instance's clock, never waited.
import type { Page } from '@playwright/test';
import { expect, test } from './support/peer-browser';
import { watchContexts } from './support/leaks';
import {
  POLL, VoicePeer, WAIT, advanceClock, audioRunning, changedSeen, connected, copy, dmPair, framesPass, introduce,
  joinVoice, noiseOff, sentCount,
  notificationsSeen, onboard, openDm, ringChannelsSeen, ringPings, ringTimerLive, rings, self, setHidden, slice, stripButton, tile, tones,
} from './support/voice';
import { dmComposer, dmLog } from '../web/support/app';
import { desktopE2E } from '../web/support/electron.ts';

watchContexts(test);

/** The ring's desktop notifications, as the recorder saw them. */
const ringNotes = async (page: Page) => (await notificationsSeen(page)).filter((n) => n.title === copy('call.ring.notifyTitle', { name: 'alice' }));
const outgoing = (page: Page) => page.locator('.d-call-stage__outgoing');

/** Product walk I4/I5: at phone width, what overlaps what, as words (empty when the layout is clean). */
const phoneOverlaps = (page: Page) => page.evaluate(() => {
  const box = (el: Element | null) => (el === null ? null : el.getBoundingClientRect());
  const hit = (a: DOMRect | null, b: DOMRect | null) => a !== null && b !== null && a.width > 0 && b.width > 0
    && a.left < b.right - 1 && b.left < a.right - 1 && a.top < b.bottom - 1 && b.top < a.bottom - 1;
  const out: string[] = [];
  // The phone shell has no dock: the stage holds the controls while it is on screen, and the call bar stands in when it is not.
  if (document.querySelector('.d-voice-dock') !== null) out.push('a dock on a phone');
  const callBar = box(document.querySelector('.d-call-bar'));
  const stageBar = box(document.querySelector('.d-call-stage__controls'));
  if (callBar === null && (stageBar === null || stageBar.top < 0 || stageBar.bottom > innerHeight + 1)) out.push('no controls on screen');
  if (callBar !== null && (callBar.top < 0 || callBar.bottom > innerHeight + 1)) out.push('call bar off screen');
  if (hit(callBar, box(document.querySelector('.d-tab-bar')))) out.push('call bar over the tab bar');
  if (hit(callBar, box(document.querySelector('.d-channel-header, .d-phone-header')))) out.push('header over call bar');
  for (const tile of document.querySelectorAll('.d-call-tile')) if (hit(stageBar, tile.getBoundingClientRect())) out.push('toolbar over a tile');
  return out;
});

test('answer: bob rings (banner, sound, one notification while hidden), answers, every ring artefact goes and both are ok', async ({ peerBrowser, browserName }) => {
  const peer = peerBrowser.track(await VoicePeer.start());
  const alice = await peerBrowser.open('alice');
  const bob = await peerBrowser.open('bob');
  try {
    await dmPair(alice, bob, peer);
    await setHidden(bob.page, true);
    await stripButton(alice.page, 'call.start').click();
    await connected(alice.page);
    await expect(outgoing(alice.page)).toHaveText(copy('call.outgoing.ringing', { names: 'bob' }), { timeout: WAIT });

    const ring = rings(bob.page);
    await expect(ring).toHaveCount(1, { timeout: WAIT });
    await expect(ring).toHaveAttribute('data-ring', 'incoming');
    await expect(ring.getByRole('alert')).toHaveText(copy('call.ring.incoming', { name: 'alice' }));
    if (browserName === 'firefox') await audioRunning(bob.page);
    // A ping on a running context (task-10 review M2): Firefox's first ping finds its context suspended and is silent
    // (report concern 6), so a ping counts only once its context runs.
    await expect.poll(async () => (await tones(bob.page)).some((t) => t.hz === 1000 && t.state === 'running'),
      { ...POLL, message: 'a ring ping on a running AudioContext' }).toBe(true);
    await expect.poll(() => ringTimerLive(bob.page), POLL).toBe(true);
    await expect.poll(async () => (await ringNotes(bob.page)).length, POLL).toBe(1);
    expect((await ringNotes(bob.page))[0].closed).toBe(false);

    await setHidden(bob.page, false);
    await ring.getByRole('button', { name: copy('call.ring.answer'), exact: true }).click();
    await connected(bob.page);
    await expect(rings(bob.page)).toHaveCount(0, { timeout: WAIT });
    await expect.poll(() => ringTimerLive(bob.page), POLL).toBe(false);
    const notes = await ringNotes(bob.page);
    expect(notes).toHaveLength(1);
    expect(notes[0].closed).toBe(true);
    await expect(tile(alice.page, 'bob')).toHaveAttribute('data-voice-state', 'ok', { timeout: WAIT });
    await expect(tile(bob.page, 'alice')).toHaveAttribute('data-voice-state', 'ok', { timeout: WAIT });
    // Alice's stage: from "ringing bob" to the live call (no outgoing line once bob is in it).
    await expect(outgoing(alice.page)).toHaveCount(0, { timeout: WAIT });
    // Product walk I5: the DM call at 360 px stacks without overlap: the stage's toolbar sits under the tiles, never over
    // one; off the stage, the phone's call bar is on screen and clear of the tab bar and the header.
    await bob.page.setViewportSize({ width: 360, height: 740 });
    if (desktopE2E()) {
      expect(bob.page.viewportSize()!.width).toBeGreaterThanOrEqual(900);
      await expect(bob.page.locator('.d-app-shell')).not.toHaveAttribute('data-layout', 'phone');
      await expect(bob.page.locator('.d-call-bar')).toHaveCount(0);
      return;
    }
    await expect.poll(() => phoneOverlaps(bob.page), POLL).toEqual([]);
    await bob.page.getByRole('button', { name: copy('nav.back'), exact: true }).click();
    await expect(bob.page.locator('.d-call-bar')).toBeVisible({ timeout: WAIT });
    await expect.poll(() => phoneOverlaps(bob.page), POLL).toEqual([]);
  } finally {
    await peerBrowser.close(bob);
    await peerBrowser.close(alice);
    await peer.close();
  }
});

test('decline: alice sees "bob declined." and bob\'s banner goes', async ({ peerBrowser }) => {
  const peer = peerBrowser.track(await VoicePeer.start());
  const alice = await peerBrowser.open('alice');
  const bob = await peerBrowser.open('bob');
  try {
    await dmPair(alice, bob, peer);
    await stripButton(alice.page, 'call.start').click();
    await connected(alice.page);
    const ring = rings(bob.page);
    await expect(ring).toHaveCount(1, { timeout: WAIT });
    await ring.getByRole('button', { name: copy('call.ring.decline'), exact: true }).click();
    await expect(rings(bob.page)).toHaveCount(0, { timeout: WAIT });
    await expect(outgoing(alice.page)).toHaveText(copy('call.outgoing.declined', { name: 'bob' }), { timeout: WAIT });
    await expect.poll(() => ringTimerLive(bob.page), POLL).toBe(false);
  } finally {
    await peerBrowser.close(bob);
    await peerBrowser.close(alice);
    await peer.close();
  }
});

test('busy: a ring while bob is in voice shows the busy text, with no ring sound and no notification', async ({ peerBrowser }) => {
  const peer = peerBrowser.track(await VoicePeer.start());
  const alice = await peerBrowser.open('alice');
  const bob = await peerBrowser.open('bob');
  try {
    await dmPair(alice, bob, peer);
    // Without noise suppression bob's steady fake microphone is a 50 frames-per-second clock of his own.
    await noiseOff(bob.page);
    await joinVoice(bob.page, peer.communityName, peer.voiceName);
    await setHidden(bob.page, true);
    const pings = await ringPings(bob.page);
    const notesBefore = (await notificationsSeen(bob.page)).length;
    await stripButton(alice.page, 'call.start').click();
    await connected(alice.page);
    const ring = rings(bob.page);
    await expect(ring).toHaveCount(1, { timeout: WAIT });
    await expect(ring).toHaveAttribute('data-ring', 'busy');
    await expect(ring.getByRole('alert')).toHaveText(copy('call.ring.busy', { name: 'alice', where: peer.voiceName }));
    // The banner is the positive control (the ring was taken in); then 150 of bob's own frames, longer than the ring's
    // 2 s period, so a ring sound would have pinged by now even had its first ping found the AudioContext not running.
    await framesPass(bob.page, await sentCount(bob.page), 150);
    expect(await ringPings(bob.page)).toBe(pings);
    expect(await ringTimerLive(bob.page)).toBe(false);
    // No notification of any title (task-10 review M1): a busy variant or a wrongly named one would show here too.
    expect((await notificationsSeen(bob.page)).slice(notesBefore)).toEqual([]);
    await setHidden(bob.page, false);
  } finally {
    await peerBrowser.close(bob);
    await peerBrowser.close(alice);
    await peer.close();
  }
});

test('blocked DM: a ring of a DM whose contact\'s identity changed never shows; a trusted typing and ring after it do', async ({ peerBrowser }) => {
  const peer = peerBrowser.track(await VoicePeer.start());
  const other = peerBrowser.track(await VoicePeer.start());
  const bob = await peerBrowser.open('bob');
  try {
    const s = await peer.setup();
    await onboard(bob, peer, 'bob');
    await introduce(peer, [bob]);
    const b = await self(bob.page);

    // The peer's identity is replaced (bob pinned the old one in their server's group), and the replaced device opens
    // a DM with bob: its leaf carries the new key, so bob's copy of the DM is send-blocked (E_UMK_CHANGED).
    await peer.sync();
    await peer.rotateUmk();
    await changedSeen(bob.page, s.user_id);
    const dm = await peer.openDm(b.user);
    await expect.poll(async () => (await peer.syncDm(dm.channel_id)).members, POLL).toBeGreaterThanOrEqual(2);
    await expect.poll(async () => (await slice<Record<string, { code: string }>>(bob.page, 'sendBlocks'))?.[dm.channel_id]?.code, POLL)
      .toBe('E_UMK_CHANGED');
    // A second, trusted peer's DM with bob, open on his screen (the control). Bob shares no server with it: its DM and
    // its typing are named by its id's first 8 characters.
    const o = await other.setup();
    const odm = await other.openDm(b.user);
    await expect.poll(async () => (await other.syncDm(odm.channel_id)).members, POLL).toBe(2);
    await openDm(bob.page, odm.channel_id, o.user_id.slice(0, 8));
    await setHidden(bob.page, true);
    await peer.callJoin(dm.channel_id);
    const started = await peer.callStart(dm.channel_id);
    expect(started.created).toBe(true);
    // Positive controls, both sent after the start (whose op 53 the instance queued for bob's gateway before it
    // answered): a typing indicator of the trusted peer, and then a ring of the trusted peer's own DM call, which takes
    // the very path the blocked ring would have (the worker's rings slice, the banner, the notification while hidden).
    await other.typing(odm.channel_id, true);
    await expect(bob.page.getByText(copy('typing.one', { name: o.user_id.slice(0, 8) }), { exact: true }))
      .toBeVisible({ timeout: WAIT });
    await other.callJoin(odm.channel_id);
    expect((await other.callStart(odm.channel_id)).created).toBe(true);
    await expect(rings(bob.page)).toHaveCount(1, { timeout: WAIT });
    await expect(rings(bob.page).getByRole('alert')).toHaveText(copy('call.ring.incoming', { name: o.user_id.slice(0, 8) }));
    await expect.poll(async () => (await notificationsSeen(bob.page)).length, POLL).toBe(1);
    // The blocked DM never had a ring listed, banner or notification, at any moment.
    expect(await ringChannelsSeen(bob.page)).toEqual([odm.channel_id]);
    expect((await notificationsSeen(bob.page))[0].title).toBe(copy('call.ring.notifyTitle', { name: o.user_id.slice(0, 8) }));
    await setHidden(bob.page, false);
    await expect(dmComposer(bob.page)).toBeVisible();
    await expect(dmLog(bob.page)).toBeVisible();
  } finally {
    await peerBrowser.close(bob);
    await other.close();
    await peer.close();
  }
});

// Last in the suite's order on purpose: it moves the instance clock for good, and no native peer may join a call after
// that (voice.ts, M6).
test('timeout: the ring ends when the instance\'s 30 s pass (op 53 state 0), waited for, never timed', async ({ peerBrowser }) => {
  const peer = peerBrowser.track(await VoicePeer.start());
  const alice = await peerBrowser.open('alice');
  const bob = await peerBrowser.open('bob');
  try {
    await dmPair(alice, bob, peer);
    await stripButton(alice.page, 'call.start').click();
    await connected(alice.page);
    await expect(rings(bob.page)).toHaveCount(1, { timeout: WAIT });
    // The instance's ring lasts 30 s on its own clock (R5): move it past them; the end reaches bob as state 0.
    await advanceClock(31);
    await expect(rings(bob.page)).toHaveCount(0, { timeout: WAIT });
    await expect.poll(() => ringTimerLive(bob.page), POLL).toBe(false);
  } finally {
    await peerBrowser.close(bob);
    await peerBrowser.close(alice);
    await peer.close();
  }
});
