// VOICE-E2E (web-3 task 10): two browsers join one voice channel through the real SFU and hear each other, verified
// frame by frame as the other's device; and the media key never travels in a `ret` or a `slice` (decision 3).
import type { Page } from '@playwright/test';
import { expect, test } from './support/peer-browser';
import { watchContexts } from './support/leaks';
import {
  VoicePeer, WAIT, audible, audioRunning, heard, joinVoice, keyPath, noiseOff, onboard, openVoiceOnly, self, snapshotDevices, tile, voiceRow,
} from './support/voice';

watchContexts(test);

/** Records every `data-voice-state` each tile of the stage takes, by the name it shows. */
async function watchTiles(page: Page): Promise<void> {
  await page.evaluate(() => {
    const seen: string[] = [];
    (window as unknown as { __dwTiles: string[] }).__dwTiles = seen;
    const note = (el: Element) => {
      const name = el.querySelector('.d-call-tile__name')?.textContent ?? '';
      const entry = `${name}:${el.getAttribute('data-voice-state') ?? ''}`;
      if (seen.at(-1) !== entry) seen.push(entry);
    };
    document.querySelectorAll('.d-call-tile:not([data-own])').forEach(note);
    new MutationObserver((records) => {
      for (const r of records) {
        const target = r.target instanceof Element ? r.target : null;
        // A state the tile left before this callback ran (two commits in one task) is in the record's old value.
        if (target?.matches('.d-call-tile:not([data-own])')) {
          const name = target.querySelector('.d-call-tile__name')?.textContent ?? '';
          if (r.oldValue !== null && seen.at(-1) !== `${name}:${r.oldValue}`) seen.push(`${name}:${r.oldValue}`);
          note(target);
        }
        for (const n of r.addedNodes) {
          if (!(n instanceof Element)) continue;
          if (n.matches('.d-call-tile:not([data-own])')) note(n);
          n.querySelectorAll('.d-call-tile:not([data-own])').forEach(note);
        }
      }
    }).observe(document.body, { subtree: true, childList: true, attributes: true, attributeOldValue: true, attributeFilter: ['data-voice-state'] });
  });
}
const tileHistory = (page: Page) => page.evaluate(() => (window as unknown as { __dwTiles: string[] }).__dwTiles);

test('two browsers join one voice channel, see each other ok and hear each other; no key in a ret or a slice', async ({ peerBrowser, browserName }) => {
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
    // web3.3: RNNoise removes the fake microphone's steady tone, so with it on there is nothing to hear (`audible` below);
    // without it the tone is sent as full frames, as in controls.spec.
    await noiseOff(alice.page);
    await noiseOff(bob.page);

    await joinVoice(alice.page, peer.communityName, peer.voiceName);
    await openVoiceOnly(bob.page, peer.communityName, peer);
    // Bob sees alice in voice from dillad's voice_state before he joins (the presence the UI shows, media card 23).
    await expect.poll(() => snapshotDevices(bob.page, s.voice_channel_id), { timeout: WAIT }).toEqual([a.device]);
    await watchTiles(alice.page);
    await watchTiles(bob.page);
    await joinVoice(bob.page, peer.communityName, peer.voiceName);

    if (browserName === 'firefox') {
      // Headless Firefox plays nothing without an audio server: every analyser and sound would read silent.
      await audioRunning(alice.page);
      await audioRunning(bob.page);
    }
    await expect(tile(alice.page, 'bob')).toHaveAttribute('data-voice-state', 'ok', { timeout: WAIT });
    await expect(tile(bob.page, 'alice')).toHaveAttribute('data-voice-state', 'ok', { timeout: WAIT });
    await heard(alice.page, b.device);
    await heard(bob.page, a.device);
    if (browserName === 'chromium') {
      // web3.3: decrypted is not heard. Each page plays the other's track into its audio graph, with the tone in it
      // (headless Firefox has no audio server, so its graph reads silent whatever plays).
      await audible(alice.page);
      await audible(bob.page);
    }
    // Bob's tile passed through `connecting` on alice's stage before it was ok.
    // Alice's page loaded the member list before bob joined the server: his tile names him once the worker has read it
    // again (task 10, concern 4), so a state recorded before that is under his id's first 8 characters.
    const history = (await tileHistory(alice.page)).map((e) => e.replace(`${b.user.slice(0, 8)}:`, 'bob:'));
    // Task 8: `connecting` lasts only while alice's roster knows bob before the SFU does; when both land in one task React
    // renders only the later state (the old stage's extra layout render used to split them). It is never after ok.
    expect(history.indexOf('bob:ok'), history.join(' ')).toBeGreaterThanOrEqual(0);
    if (history.includes('bob:connecting')) expect(history.indexOf('bob:connecting')).toBeLessThan(history.indexOf('bob:ok'));
    else test.info().annotations.push({ type: 'tile history', description: history.join(' ') });

    // The voice row in the one channel list has both people nested under it, on both sides.
    for (const p of [alice.page, bob.page]) {
      const people = voiceRow(p, peer.voiceName).locator('xpath=ancestor::li[1]').locator('.d-voice-row__person');
      await expect(people.filter({ hasText: 'alice' })).toHaveCount(1, { timeout: WAIT });
      await expect(people.filter({ hasText: 'bob' })).toHaveCount(1, { timeout: WAIT });
    }

    // The key path: both pages received their join keys as `callKeys` pushes (the recorder sees the core worker), and
    // no `ret` or `slice` of any worker carried a 16-byte buffer or a `baseKey` field.
    for (const p of [alice.page, bob.page]) {
      const kp = await keyPath(p);
      expect(kp.callKeys, 'callKeys pushes seen').toBeGreaterThan(0);
      expect(kp.messages, 'ret and slice messages scanned').toBeGreaterThan(0);
      expect(kp.findings).toEqual([]);
    }
  } finally {
    await peerBrowser.close(bob);
    await peerBrowser.close(alice);
    await peer.close();
  }
});
