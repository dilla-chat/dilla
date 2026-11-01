// VOICE-E2E (web-3 task 10): no securitypolicyviolation in any page or worker during a call and a ring (the guard of
// peer-browser.ts collects both; RV-5 media-src blob:, the Permissions-Policy of task 3), and axe (serious and critical,
// target-size on) on the ring banner, the call stage and the dock, an inline player and Settings → Voice, each in the
// three themes and at 360×740 (WCAG 2.2 AA, Global Constraints).
import { expect, test } from './support/peer-browser';
import { watchContexts } from './support/leaks';
import {
  VoicePeer, WAIT, axeEverywhere, buildWav, closeSettings, connected, copy, dmPair, dock, openVoiceSettings, rings, stage,
  stripButton,
} from './support/voice';
import { attachInput, dmComposer, dmLog, trayEntries } from '../web/support/app';

watchContexts(test);

test('the ring, the call, a player and Settings → Voice pass axe in three themes and at 360×740, with no CSP violation', async ({ peerBrowser }) => {
  const peer = peerBrowser.track(await VoicePeer.start());
  const alice = await peerBrowser.open('alice');
  const bob = await peerBrowser.open('bob');
  try {
    await dmPair(alice, bob, peer);

    await test.step('an inline player, idle and ready', async () => {
      await attachInput(alice.page).setInputFiles({ name: 'tone.wav', mimeType: 'audio/wav', buffer: await buildWav(alice.page) });
      await expect(trayEntries(alice.page).first()).toHaveAttribute('data-phase', 'ready', { timeout: WAIT });
      await dmComposer(alice.page).fill('a tone');
      await dmComposer(alice.page).press('Enter');
      const player = dmLog(bob.page).locator('.d-media-player');
      await expect(player).toHaveAttribute('data-media-state', 'idle', { timeout: WAIT });
      await axeEverywhere(bob.page, 'player idle');
      await player.getByRole('button', { name: copy('media.play', { name: 'tone.wav' }), exact: true }).click();
      await expect(player).toHaveAttribute('data-media-state', 'ready', { timeout: WAIT });
      await axeEverywhere(bob.page, 'player ready');
    });

    await test.step('the ring banner', async () => {
      await stripButton(alice.page, 'call.start').click();
      await connected(alice.page);
      await expect(rings(bob.page)).toHaveCount(1, { timeout: WAIT });
      // Alice's stage first, then bob's banner, then the answer at once (task-10 review M7): bob's ring ends locally 30 s
      // after it arrived, and eight axe passes on a loaded runner can outlast that; the ring is re-read before the answer
      // so a timing loss reads as one.
      await axeEverywhere(alice.page, 'call stage while ringing');
      await axeEverywhere(bob.page, 'ring banner');
    });

    await test.step('the call stage and the dock', async () => {
      await expect(rings(bob.page), 'bob\'s ring is still on screen after the axe passes (its 30 s run from its arrival)')
        .toHaveCount(1);
      await rings(bob.page).getByRole('button', { name: copy('call.ring.answer'), exact: true }).click();
      await connected(bob.page);
      await expect(stage(bob.page)).toBeVisible();
      await expect(dock(bob.page)).toBeVisible();
      await axeEverywhere(bob.page, 'call stage and dock');
    });

    await test.step('Settings → Voice during the call', async () => {
      const settings = await openVoiceSettings(bob.page);
      // Task 10b: the call holds the microphone, so Let's check waits and says why; the meter rests (Not testing).
      await expect(settings.getByRole('button', {name:copy('signalSettings.voice.testStart'),exact:true})).toBeDisabled();
      await expect(settings.getByText(copy('settings.voice.testCall'), {exact:true})).toBeVisible();
      await expect(settings.getByText(copy('signalSettings.voice.notTesting'), {exact:true})).toBeVisible();
      await axeEverywhere(bob.page, 'Settings → Voice');
      await closeSettings(bob.page);
    });

    await peerBrowser.guard.collectWorkers();
    expect(peerBrowser.guard.cspViolations, 'securitypolicyviolation events during the call and the ring').toEqual([]);
  } finally {
    await peerBrowser.close(bob);
    await peerBrowser.close(alice);
    await peer.close();
  }
});
