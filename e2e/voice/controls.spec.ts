// VOICE-E2E (web-3 task 10): the mute and deafen shortcuts (decision 1, Control+Shift+M and Control+Shift+D) as the
// other browser sees them, and push to talk (decision 2): the other side verifies alice's frames only while her key is
// held. Frame counts are compared over a fixed number of bob's own encrypted frames, never over a sleep (lesson c).
import { expect, test } from './support/peer-browser';
import { watchContexts } from './support/leaks';
import {
  VoicePeer, WAIT, audioRunning, copy, framesPass, heard, heardCount, joinVoice, noiseOff, onboard, pushToTalk, self,
  openText, sentCount, tile,
} from './support/voice';

watchContexts(test);

/** Bob's own frames per measuring window: 100 Opus frames, about 2 s of his steady tone. */
const WINDOW = 100;

test('Control+Shift+M and Control+Shift+D show alice muted and deafened to bob; push to talk sends only while held', async ({ peerBrowser, browserName }) => {
  const peer = peerBrowser.track(await VoicePeer.start());
  const alice = await peerBrowser.open('alice');
  const bob = await peerBrowser.open('bob');
  try {
    await peer.setup();
    await onboard(alice, peer, 'alice');
    await onboard(bob, peer, 'bob');
    await peer.seat(2); // no hello: a voice tile names a member the page loaded after it (task 10, concern 4)
    const a = await self(alice.page);
    // Without noise suppression the fake microphone's steady tone is sent as full frames, a frame clock of its own.
    await noiseOff(alice.page);
    await noiseOff(bob.page);
    await joinVoice(alice.page, peer.communityName, peer.voiceName);
    await joinVoice(bob.page, peer.communityName, peer.voiceName);
    if (browserName === 'firefox') await audioRunning(bob.page);
    await expect(tile(bob.page, 'alice')).toHaveAttribute('data-voice-state', 'ok', { timeout: WAIT });
    await heard(bob.page, a.device);
    const word = (key: string) => tile(bob.page, 'alice').locator(`.d-call-tile__state[data-state="${key}"]`);

    await test.step('Control+Shift+M mutes alice as bob sees it, and again unmutes', async () => {
      await expect(word('muted')).toHaveCount(0);
      await alice.page.keyboard.press('Control+Shift+M');
      await expect(word('muted')).toBeVisible({ timeout: WAIT });
      // WB a11y I4: the toggle keeps its label; aria-pressed says it is on. Since web3.8 the user panel at the bottom left
      // holds the mute and deafen toggles (the call panel above it has none), and it carries the shortcuts.
      const panelMute = alice.page.locator('.d-user-panel').getByRole('button', { name: copy('voice.mute'), exact: true });
      await expect(panelMute).toHaveAttribute('aria-pressed', 'true');
      await expect(panelMute).toHaveAttribute('aria-keyshortcuts', /Shift\+M/);
      await alice.page.keyboard.press('Control+Shift+M');
      await expect(word('muted')).toHaveCount(0, { timeout: WAIT });
    });

    await test.step('Control+Shift+D deafens alice as bob sees it, and again undeafens', async () => {
      await alice.page.keyboard.press('Control+Shift+D');
      await expect(word('deafened')).toBeVisible({ timeout: WAIT });
      await alice.page.keyboard.press('Control+Shift+D');
      await expect(word('deafened')).toHaveCount(0, { timeout: WAIT });
    });

    await test.step('push to talk: bob verifies alice only while her key is held', async () => {
      // Insert: a non-text key push to talk accepts on its own (task-8b ruling I2).
      await pushToTalk(alice.page, 'Insert');
      await alice.page.locator('body').click({ position: { x: 1, y: 1 } });
      // The key is up: the microphone closes. Let frames in flight land, then measure one window.
      await framesPass(bob.page, await sentCount(bob.page), WINDOW / 4);
      const idle0 = await heardCount(bob.page, a.device);
      await framesPass(bob.page, await sentCount(bob.page), WINDOW);
      const idle = (await heardCount(bob.page, a.device)) - idle0;

      await alice.page.keyboard.down('Insert');
      let held = 0;
      try {
        const held0 = await heardCount(bob.page, a.device);
        await framesPass(bob.page, await sentCount(bob.page), WINDOW);
        held = (await heardCount(bob.page, a.device)) - held0;
      } finally {
        await alice.page.keyboard.up('Insert');
      }

      await framesPass(bob.page, await sentCount(bob.page), WINDOW / 4);
      const after0 = await heardCount(bob.page, a.device);
      await framesPass(bob.page, await sentCount(bob.page), WINDOW);
      const after = (await heardCount(bob.page, a.device)) - after0;

      const deltas = `idle ${idle}, held ${held}, after ${after} frames per ${WINDOW} of bob's own`;
      test.info().annotations.push({ type: 'push to talk', description: deltas });
      console.log(`push to talk (${browserName}): ${deltas}`);
      expect(held, deltas).toBeGreaterThanOrEqual(WINDOW / 2);
      expect(idle, deltas).toBeLessThanOrEqual(WINDOW / 10);
      expect(after, deltas).toBeLessThanOrEqual(WINDOW / 10);
    });

    await test.step('product walk M7 and I4: the stage status line names the key (web3.9: not the dock, which has no meter or lines), and at 360 px the call bar stays on screen over a text channel', async () => {
      // Fix round 1 (Jonas): no push-to-talk line on the stage or in the dock; the dock has no meter.
      await expect(alice.page.locator('.dw-voice-dock__mode, .dw-voice-dock [role="meter"]')).toHaveCount(0);
      await alice.page.setViewportSize({ width: 360, height: 740 });
      // The phone shell holds the call in a bar and shows no dock: step off the stage, onto the text channel.
      await alice.page.getByRole('button', { name: copy('nav.back'), exact: true }).click();
      await openText(alice.page, peer.communityName, peer.textName);
      const bar = alice.page.locator('.d-call-bar');
      await expect(alice.page.locator('.d-voice-dock')).toHaveCount(0);
      await expect(bar).toBeInViewport({ ratio: 1 });
      const [b, composer] = await Promise.all([bar.boundingBox(),
        alice.page.getByRole('textbox', { name: copy('shell.composer.label', { channel: peer.textName }), exact: true }).boundingBox()]);
      expect(b !== null && composer !== null && (b.y >= composer.y + composer.height - 1 || b.y + b.height <= composer.y + 1),
        'the call bar does not cover the composer').toBe(true);
    });
  } finally {
    await peerBrowser.close(bob);
    await peerBrowser.close(alice);
    await peer.close();
  }
});
