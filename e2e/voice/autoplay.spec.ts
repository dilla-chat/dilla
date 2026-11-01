// VOICE-E2E (web3.4 V3): the browser holds call audio back (its autoplay rule), and one press brings it back. Bob's
// page runs under a stricter rule than the suite's Chromium flags allow (`--autoplay-policy=no-user-gesture-required`):
// an init script makes every new AudioContext start suspended, and lets resume() through only inside a press (a
// transient activation, Safari-like). That is stricter than Chromium, which starts a context made outside a gesture on any
// later activation of the page (sticky activation). Alice's microphone tone then reaches
// bob's media worker (`heard`) but not his ears, until he presses `turn on audio` (`audible`). Chromium only (the plan's
// scope for V3; the suite's Chromium switch is the rule this script tightens).
import { expect, test } from './support/peer-browser';
import { watchContexts } from './support/leaks';
import { AUDIBLE_RMS, VoicePeer, WAIT, audible, audibleLevel, copy, dock, heard, joinVoice, noiseOff, onboard, self, tile } from './support/voice';

watchContexts(test);

/**
 * The stricter autoplay rule (an init script: it may close over nothing). A new AudioContext is suspended at once, and
 * resume() starts it only inside a person's press: while a trusted pointer or key event is being dispatched. Outside one
 * the promise waits, as Chromium's does, until the context runs. `navigator.userActivation` cannot be the test here:
 * every `page.evaluate` of the suite's recorders runs as a user gesture and would lift the hold for seconds, and LiveKit
 * calls resume() itself on connect and on each attach.
 */
function holdAudioBack(): void {
  const Native = window.AudioContext as typeof AudioContext | undefined;
  if (Native === undefined) return;
  const resume = Native.prototype.resume;
  const suspend = Native.prototype.suspend;
  let pressing = false;
  for (const type of ['pointerdown', 'pointerup', 'mousedown', 'mouseup', 'click', 'keydown', 'keyup']) {
    window.addEventListener(type, (e) => {
      if (!e.isTrusted) return;
      pressing = true;
      setTimeout(() => { pressing = false; }, 0);
    }, { capture: true });
    window.addEventListener(type, () => { pressing = false; });
  }
  class HeldBack extends Native {
    constructor(options?: AudioContextOptions) {
      super(options);
      void suspend.call(this).catch(() => undefined);
    }
    override resume(): Promise<void> {
      if (pressing) return resume.call(this);
      return new Promise<void>((done) => {
        const running = (): void => {
          if (this.state !== 'running') return;
          this.removeEventListener('statechange', running);
          done();
        };
        this.addEventListener('statechange', running);
      });
    }
  }
  Object.defineProperty(window, 'AudioContext', { value: HeldBack, configurable: true, writable: true });
}

test('call audio the browser holds back comes back with one press of turn on audio', async ({ peerBrowser, browserName }, testInfo) => {
  test.skip(browserName !== 'chromium', 'web3.4 V3: the held-back rule tightens the suite’s Chromium autoplay switch');
  const peer = peerBrowser.track(await VoicePeer.start());
  const alice = await peerBrowser.open('alice');
  const bob = await peerBrowser.open('bob');
  await bob.context.addInitScript(holdAudioBack);
  try {
    await peer.setup();
    await onboard(alice, peer, 'alice');
    await onboard(bob, peer, 'bob');
    await peer.seat(2); // no hello: a voice tile names a member the page loaded after it (task 10, concern 4)
    const a = await self(alice.page);
    // The fake microphone's steady tone is what RNNoise removes: off, so alice's tone is sound at bob's end.
    await noiseOff(alice.page);
    await joinVoice(alice.page, peer.communityName, peer.voiceName);
    await joinVoice(bob.page, peer.communityName, peer.voiceName);
    await expect(tile(bob.page, 'alice')).toHaveAttribute('data-voice-state', 'ok', { timeout: WAIT });

    const blocked = dock(bob.page).getByText(copy('voice.audioBlocked'), { exact: true });
    const turnOn = dock(bob.page).getByRole('button', { name: copy('voice.audioStart'), exact: true });
    await test.step('held back: bob decrypts alice’s frames, hears nothing, and is offered turn on audio', async () => {
      await expect(blocked).toBeVisible({ timeout: WAIT });
      await expect(turnOn).toBeVisible();
      await heard(bob.page, a.device);
      // Positive control for `audible` below: the frames arrive, and the held-back context plays none of them.
      expect(await audibleLevel(bob.page), 'the level bob’s page plays while the browser holds audio back').toBeLessThan(AUDIBLE_RMS);
    });

    await test.step('one press: alice is heard and the line goes', async () => {
      await turnOn.click();
      await audible(bob.page);
      await expect(blocked).toHaveCount(0, { timeout: WAIT });
      await expect(turnOn).toHaveCount(0);
    });
  } finally {
    // A test's finally runs before Playwright finalizes its failed status. Keep
    // the actual receiver graph and worker counters before closing either peer.
    for (const person of [alice, bob]) {
      const audio = await person.page.evaluate(() => {
        const rec = (window as unknown as { __dw?: { contexts: AudioContext[]; remoteTracks: Set<string>; remoteAnalysers: AnalyserNode[] } }).__dw;
        return {
          contexts: rec?.contexts.map(context => ({ state: context.state, time: context.currentTime, sampleRate: context.sampleRate })),
          remoteTracks: rec ? [...rec.remoteTracks] : [],
          analysers: rec?.remoteAnalysers.map(analyser => {
            const samples = new Float32Array(analyser.fftSize); analyser.getFloatTimeDomainData(samples);
            return { state: analyser.context.state, time: analyser.context.currentTime, rms: Math.sqrt(samples.reduce((sum, sample) => sum + sample * sample, 0) / samples.length) };
          }),
        };
      }).catch(error => ({ error: String(error) }));
      const workers = await Promise.all(person.page.workers().map(worker => worker.evaluate(() => {
        const stats = (globalThis as unknown as { __dillaMediaStats?: Record<string, unknown> }).__dillaMediaStats;
        if (!stats) return null;
        const { encrypted, decrypted, verified, emptyFrames, dropped, passedThrough } = stats;
        return { encrypted, decrypted, verified, emptyFrames, dropped, passedThrough };
      }).catch(error => ({ error: String(error) }))));
      await testInfo.attach(`autoplay-${person.name}`, { body: JSON.stringify({ audio, workers }), contentType: 'application/json' });
    }
    await peerBrowser.close(bob);
    await peerBrowser.close(alice);
    await peer.close();
  }
});
