// web3.8 E1 (V1): speaking is computed in the page from the audio itself, not read from the SFU's active-speaker flag
// (which lags by its 400 ms update interval plus a second of smoothing, so a tile used to light about a second late).
// The detector is on within about 50-75 ms of a sound (25 ms ticks, two samples) and holds 300 ms before it goes off, and
// it is never on while the microphone is muted, deafened, or closed by push to talk (V1-report.md).
//
// The fake microphone plays a steady 1 kHz tone. Noise suppression is off for alice (as in controls.spec): RNNoise would
// remove a steady tone, and what is measured here is the speaking indicator, not the suppressor.
//
// Bounds. MUTED_BOUND: a mute closes the microphone at the press and the detector is reset at once, so the own tile is
// not speaking within 400 ms (pre-flight C-20; the 300 ms hold does not apply to a mute, only to silence in the signal).
// ONSET_BOUND: the detector's own onset is at most about 75 ms after the sound reaches the analyser; unmute also reopens
// the microphone, so 400 ms leaves several times the detector's worst case. The browser owns the assertion's retry loop,
// keeping driver round trips out of that short budget, well under the old SFU flag's 1 s. Bob's view of alice adds
// the network and the receive-side jitter buffer to that, so it gets REMOTE_BOUND, 600 ms: still clearly under 1 s.
import { expect, test } from './support/peer-browser';
import { watchContexts } from './support/leaks';
import { VoicePeer, WAIT, copy, heard, joinVoice, noiseOff, onboard, self, stage, tile } from './support/voice';
import type { Page } from '@playwright/test';

watchContexts(test);

const MUTED_BOUND = 400;
const ONSET_BOUND = 400;
const REMOTE_BOUND = 600;

/** The `data-speaking` of this page's own tile, or null when there is none. */
async function ownSpeaking(page: Page): Promise<boolean | null> {
  const v = await stage(page).locator('.d-call-tile[data-own]:not([data-source="screen"])').first().getAttribute('data-speaking', { timeout: 1000 }).catch(() => null);
  return v === null ? null : v === 'true';
}

test('alice speaking lights her tile and bob\'s tile for her within a beat, and a mute puts it out', async ({ peerBrowser, browserName }) => {
  const peer = peerBrowser.track(await VoicePeer.start());
  const alice = await peerBrowser.open('alice');
  const bob = await peerBrowser.open('bob');
  try {
    await peer.setup();
    await onboard(alice, peer, 'alice');
    await onboard(bob, peer, 'bob');
    await peer.seat(2); // no hello: a voice tile names a member the page loaded after it
    const a = await self(alice.page);
    await noiseOff(alice.page);
    await joinVoice(alice.page, peer.communityName, peer.voiceName);
    await joinVoice(bob.page, peer.communityName, peer.voiceName);
    await expect(tile(bob.page, 'alice')).toHaveAttribute('data-voice-state', 'ok', { timeout: WAIT });
    await heard(bob.page, a.device);

    const seen = tile(bob.page, 'alice');
    const controls = alice.page.getByRole('toolbar', { name: copy('voice.controls.voice'), exact: true });
    // Since web3.8 the user panel owns mute; the stage toolbar keeps its own toggle (C-20).
    const mute = controls.locator('[data-control="mute"]');

    if (browserName === 'firefox') {
      // Firefox's fake microphone is not a tone on every build; where it is silent there is nothing to light.
      const lit = await expect.poll(() => ownSpeaking(alice.page), { timeout: 5_000 }).toBe(true).then(() => true, () => false);
      test.skip(!lit, 'the Firefox fake microphone is silent here, so no tile can speak');
    }

    await test.step('the fake tone lights alice\'s own tile and bob\'s tile for her', async () => {
      await expect.poll(() => ownSpeaking(alice.page), { timeout: WAIT, message: 'alice\'s own tile is speaking' }).toBe(true);
      await expect(seen).toHaveAttribute('data-speaking', 'true', { timeout: WAIT });
    });

    await test.step('a mute through the stage toolbar puts alice\'s own tile out within 400 ms', async () => {
      await mute.click();
      await expect(mute).toHaveAttribute('data-on', 'true');
      // Signal §5.6: the call bar's word carries the state (the prototype's Unmute), so the role and name say it too.
      await expect(controls.getByRole('button', { name: copy('callStage.bar.unmute'), exact: true })).toBeVisible();
      await expect.poll(() => ownSpeaking(alice.page), { timeout: MUTED_BOUND, message: 'alice is muted: her own tile is not speaking' }).toBe(false);
      // Bob's tile follows once the last audio has played out (the 300 ms hold and the jitter buffer), within a state's wait.
      await expect(seen).toHaveAttribute('data-speaking', 'false', { timeout: WAIT });
    });

    await test.step('an unmute lights them again within a beat, far under the old second', async () => {
      const t0 = Date.now();
      await mute.click();
      await expect(mute).not.toHaveAttribute('data-on');
      // Retry in the browser: driver-side polling spends this short budget on round trips,
      // and its last probe can observe true yet arrive after the driver's deadline.
      await expect(stage(alice.page).locator('.d-call-tile[data-own]:not([data-source="screen"])').first(),
        'alice is speaking again on her own tile').toHaveAttribute('data-speaking', 'true', { timeout: ONSET_BOUND });
      const own = Date.now() - t0;
      await expect(seen).toHaveAttribute('data-speaking', 'true', { timeout: REMOTE_BOUND });
      const remote = Date.now() - t0;
      const msg = `unmute to speaking: own ${own} ms, bob's tile ${remote} ms`;
      test.info().annotations.push({ type: 'speaking', description: msg });
      console.log(`speaking (${browserName}): ${msg}`);
    });
  } finally {
    await peerBrowser.close(bob);
    await peerBrowser.close(alice);
    await peer.close();
  }
});
