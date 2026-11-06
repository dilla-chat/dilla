// E-reload (web3.7, R31): a reload in a call leaves exactly one tile for the reloaded person, and a reload that is
// never followed by a rejoin leaves no ghost either. The second half blocks both page-leave events, `pagehide` and
// `beforeunload` (an init script that stops them before any other listener sees them), so it models a tab that vanishes
// with no leave at all, whatever any listener on the page (the app's, or LiveKit's `disconnectOnPageLeave`) would do.
// FX-RELOAD (web3.9): with `beforeunload` let through, LiveKit's page-leave disconnect ended the room, the old worker
// sent its own self-Remove, and when that won the race (Firefox in CI, run 39393759704) the new page had nothing to
// sweep. Locally the SFU still drops the reloaded participant within about 300 ms, so without help the peer-visible
// checks (bob's roster and tiles) could not tell the sweep from the SFU. The second half therefore holds back the SFU's
// `participant_left` webhook (S3's test-host hook, `withholdLeft`): the instance never hears of the departure, and only
// alice's boot sweep (her self-Remove, one read and one proposal) can take her old device out of bob's roster. That
// roster check is red with the sweep's Remove stubbed (web3.8 E1, recorded in E1-report.md). The request check still
// pins that the sweep reads the call, and that nothing of alice's is proposed before it; sweep.test.ts covers the
// self-Remove.
import type { Page } from '@playwright/test';
import { expect, test } from './support/peer-browser';
import { watchContexts } from './support/leaks';
import { watchSocketRequests } from '../web/support/socket-requests';
import {
  VoicePeer, WAIT, callSlice, expectShellReady, heard, joinVoice, onboard, self, tile, withholdLeft,
} from './support/voice';

watchContexts(test);

/** The sweep must beat the room's 90 s departure timeout by a wide margin; it is one read and one proposal. */
const SWEEP_BOUND = 30_000;

/** The devices a page's call roster lists, in order (the worker's call slice). */
async function rosterDevices(page: Page): Promise<string[]> {
  const s = await callSlice(page);
  return (s?.roster ?? []).map((r) => r.deviceId).sort();
}

/** The page-leave events whose listeners must never run on a suppressed page: both of them, whichever of the page's
 *  listeners (the app's leave, a library's page-leave disconnect) would hear one. */
const PAGE_LEAVE = ['pagehide', 'beforeunload'] as const;

/** Stops every page-leave event before any listener below the capture phase on `window` can see it (every listener
 *  the page adds there without capture): no leave of any kind is sent as the page goes away. The init script
 *  reaches documents loaded after it is added; it takes the event names as its argument. */
function suppressPageLeave(types: readonly string[]): void {
  for (const type of types) window.addEventListener(type, (e) => e.stopImmediatePropagation(), { capture: true });
}

/** True when each page-leave event dispatched on the page reaches no listener below the capture phase (the suppression
 *  holds for both). */
async function pageLeaveIsSuppressed(page: Page): Promise<boolean> {
  return page.evaluate((types) => types.every((type) => {
    let reached = false;
    const probe = (): void => { reached = true; };
    window.addEventListener(type, probe);
    window.dispatchEvent(new Event(type, { cancelable: true }));
    window.removeEventListener(type, probe);
    return !reached;
  }), [...PAGE_LEAVE]);
}

test('a reload mid-call leaves one tile for alice, and a reload without a rejoin leaves none within the sweep bound', async ({ peerBrowser }) => {
  const peer = peerBrowser.track(await VoicePeer.start());
  const alice = await peerBrowser.open('alice');
  const bob = await peerBrowser.open('bob');
  try {
    const s = await peer.setup();
    await onboard(alice, peer, 'alice');
    await onboard(bob, peer, 'bob');
    await peer.seat(2);
    const a = await self(alice.page);
    const b = await self(bob.page);

    await test.step('alice and bob are in the call and bob hears alice', async () => {
      await joinVoice(alice.page, peer.communityName, peer.voiceName);
      await joinVoice(bob.page, peer.communityName, peer.voiceName);
      await expect(tile(bob.page, 'alice')).toHaveAttribute('data-voice-state', 'ok', { timeout: WAIT });
      await heard(bob.page, a.device);
      await expect.poll(() => rosterDevices(bob.page), { timeout: WAIT }).toEqual([a.device, b.device].sort());
    });

    await test.step('alice reloads and rejoins: bob shows exactly one tile for her and lists her device once', async () => {
      await alice.page.addInitScript(suppressPageLeave, [...PAGE_LEAVE]);
      await alice.page.reload();
      await expectShellReady(alice.page);
      await expect.poll(() => pageLeaveIsSuppressed(alice.page), { timeout: WAIT, message: 'the reloaded page carries the suppressing listeners' }).toBe(true);
      await joinVoice(alice.page, peer.communityName, peer.voiceName);
      await expect(tile(bob.page, 'alice')).toHaveAttribute('data-voice-state', 'ok', { timeout: WAIT });
      await expect(tile(bob.page, 'alice')).toHaveCount(1, { timeout: WAIT });
      await expect.poll(() => rosterDevices(bob.page), { timeout: WAIT }).toEqual([a.device, b.device].sort());
      await expect.poll(async () => (await callSlice(bob.page))?.roster.filter((r) => r.deviceId === a.device).length, { timeout: WAIT }).toBe(1);
    });

    await test.step('alice reloads again with every page-leave event suppressed, the SFU\'s departure withheld and no rejoin: the boot sweep alone removes her old device', async () => {
      expect(await pageLeaveIsSuppressed(alice.page), 'the page that is about to reload suppresses its page-leave events').toBe(true);
      // A withheld departure is dropped for good, not queued: the instance keeps alice's device until her own sweep removes it.
      await withholdLeft(true);
      // Pins that the boot sweep runs: alice's worker reads the call of the channel it was in, with no join pressed. And
      // that the page going away sent nothing of its own (FX-RELOAD): no proposal of alice's goes out before that read.
      // The reads and proposals are gateway request frames: the app's authenticated requests are not HTTP.
      const seen = watchSocketRequests(alice.page);
      const isRead = (r: { method: string; path: string }): boolean => r.method === 'GET' && r.path === `/v1/channels/${s.voice_channel_id}/call`;
      const reads = (): number => seen.filter(isRead).length;
      const proposalsBeforeRead = (): string[] => {
        const firstRead = seen.findIndex(isRead);
        return seen.slice(0, firstRead < 0 ? seen.length : firstRead)
          .filter((r) => r.method === 'POST' && /^\/v1\/groups\/[0-9a-f]{32}\/proposal$/.test(r.path)).map((r) => r.path);
      };
      await alice.page.reload();
      await expectShellReady(alice.page);
      await expect.poll(() => reads(), { timeout: SWEEP_BOUND, message: 'the reloaded page sweeps the call it left behind' }).toBeGreaterThan(0);
      expect(proposalsBeforeRead(), 'the page that went away sent no self-Remove of its own: the sweep writes first').toEqual([]);
      await expect.poll(() => rosterDevices(bob.page), { timeout: SWEEP_BOUND, message: 'bob\'s roster drops alice\'s old device' }).toEqual([b.device]);
      await expect(tile(bob.page, 'alice')).toHaveCount(0, { timeout: SWEEP_BOUND });
    });
  } finally {
    await withholdLeft(false);
    await peerBrowser.close(bob);
    await peerBrowser.close(alice);
    await peer.close();
  }
});
