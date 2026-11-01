// web3.8 WS6 (item 13, R25) and MUTE: a mute pressed in the user panel is in force from the first frame the microphone
// publishes. Pressed before a call, the next join starts muted; pressed while the join is still starting (the microphone
// prompt held open until the click has landed), the track goes to the publish already disabled. In both, every audio
// sender the page hands a peer connection carries a disabled track, and no audio goes out while she stays muted. What the
// others see (bob's tile for her, the voice_state self_mute bit) is the last test.
import { expect, test } from './support/peer-browser';
import { watchContexts } from './support/leaks';
import type { Person } from './support/peer-browser';
import {
  VoicePeer, WAIT, POLL, callSlice, connected, copy, joinVoice, noiseOff, openVoiceOnly, onboard, self, sendsAudio, sendsNoAudio, slice, stage,
  tile, watchAudio,
} from './support/voice';
import type { VoiceState } from '../../packages/client-core/src/state/types';

watchContexts(test);

const panelMute = (alice: Person) => alice.page.locator('.d-user-panel').getByRole('button', { name: copy('voice.mute'), exact: true });

/** Holds the microphone prompt of the next call start open until `releaseMic`; `micAsked` counts the prompts held. */
async function holdMic(alice: Person): Promise<void> {
  await alice.page.evaluate(() => {
    const w = window as unknown as { __micAsked: number; __releaseMic: () => void };
    const md = navigator.mediaDevices;
    const real = md.getUserMedia.bind(md);
    let release: () => void = () => undefined;
    const gate = new Promise<void>((resolve) => { release = resolve; });
    w.__micAsked = 0;
    w.__releaseMic = () => release();
    md.getUserMedia = async (c?: MediaStreamConstraints) => {
      if (c?.audio !== undefined && c.audio !== false) { w.__micAsked += 1; await gate; }
      return real(c);
    };
  });
}
const micAsked = (alice: Person) => alice.page.evaluate(() => (window as unknown as { __micAsked: number }).__micAsked);
const releaseMic = (alice: Person) => alice.page.evaluate(() => { (window as unknown as { __releaseMic: () => void }).__releaseMic(); });

test('mute pressed in the user panel before joining: alice joins muted, on her tile, her panel and every audio sender', async ({ peerBrowser }) => {
  const peer = peerBrowser.track(await VoicePeer.start());
  const alice = await peerBrowser.open('alice');
  const bob = await peerBrowser.open('bob');
  try {
    await peer.setup();
    await onboard(alice, peer, 'alice');
    await onboard(bob, peer, 'bob');
    await peer.seat(2);

    await test.step('mute in the user panel with no call', async () => {
      const mute = panelMute(alice);
      await expect(mute).toHaveAttribute('aria-pressed', 'false');
      await mute.click();
      await expect(mute).toHaveAttribute('aria-pressed', 'true', { timeout: WAIT });
    });

    await watchAudio(alice);
    await joinVoice(bob.page, peer.communityName, peer.voiceName);
    await joinVoice(alice.page, peer.communityName, peer.voiceName);

    await test.step('her own tile and her panel say muted from the first frame', async () => {
      const own = stage(alice.page).locator('.d-call-tile[data-own]');
      await expect(own.locator('.d-call-tile__state[data-state="muted"]')).toBeVisible({ timeout: WAIT });
      expect((await callSlice(alice.page))?.phase).toBe('in');
      await expect(panelMute(alice)).toHaveAttribute('aria-pressed', 'true');
    });

    await test.step('no audio sender carries an enabled track, and no audio goes out', async () => {
      await sendsNoAudio(alice);
    });
  } finally {
    await peerBrowser.close(bob);
    await peerBrowser.close(alice);
    await peer.close();
  }
});

// Fail closed (WS6 fix round 1, MUTE): a mute pressed while the join is still starting. The microphone prompt is held open
// until the click has landed, so the press is provably inside the start; the track then goes to the publish disabled.
// It runs with noise suppression off (the sender carries the capture track itself, and the unmuted control hears the
// tone) and on, the default (the sender carries the noise processor's output).
for (const noise of ['off', 'on'] as const) test(`mute pressed while the join is pending (noise ${noise}): the call starts muted and no audio sender is ever enabled`, async ({ peerBrowser }) => {
  const peer = peerBrowser.track(await VoicePeer.start());
  const alice = await peerBrowser.open('alice');
  try {
    await peer.setup();
    await onboard(alice, peer, 'alice');
    await peer.seat(1);
    // Noise off: the sender carries the capture track itself. Noise on (the default): it carries the processor's output.
    if (noise === 'off') await noiseOff(alice.page);
    await watchAudio(alice);
    await holdMic(alice);
    await openVoiceOnly(alice.page, peer.communityName, peer);
    await stage(alice.page).getByRole('button', { name: copy('callStage.preview.join'), exact: true }).click();
    await expect.poll(() => micAsked(alice), { ...POLL, message: 'the start asked for the microphone' }).toBe(1);
    await panelMute(alice).click();
    await expect(panelMute(alice)).toHaveAttribute('aria-pressed', 'true', { timeout: WAIT });
    expect(await alice.page.evaluate(() => (window as unknown as { __watch: { pcs: unknown[] } }).__watch.pcs.length), 'no peer connection before the prompt answers').toBe(0);
    await releaseMic(alice);
    await connected(alice.page);
    await expect(panelMute(alice)).toHaveAttribute('aria-pressed', 'true');
    await expect(stage(alice.page).locator('.d-call-tile[data-own] .d-call-tile__state[data-state="muted"]')).toBeVisible({ timeout: WAIT });
    await sendsNoAudio(alice);
    // The measure tells the two apart: unmuted, her tone goes out. Only with noise suppression off: RNNoise removes the
    // fake microphone's steady tone, so the noise-on path has no audible control.
    if (noise === 'off') {
      await panelMute(alice).click();
      await expect(panelMute(alice)).toHaveAttribute('aria-pressed', 'false', { timeout: WAIT });
      await sendsAudio(alice);
    }
  } finally {
    await peerBrowser.close(alice);
    await peer.close();
  }
});

test('a join that starts muted shows bob the muted glyph, and the voice_state carries self_mute', async ({ peerBrowser }) => {
  const peer = peerBrowser.track(await VoicePeer.start());
  const alice = await peerBrowser.open('alice');
  const bob = await peerBrowser.open('bob');
  try {
    await peer.setup();
    await onboard(alice, peer, 'alice');
    await onboard(bob, peer, 'bob');
    await peer.seat(2);
    const a = await self(alice.page);
    await panelMute(alice).click();
    await expect(panelMute(alice)).toHaveAttribute('aria-pressed', 'true', { timeout: WAIT });
    // Bob is in the room first: the self-mute is a voice_state broadcast, which a later joiner would not replay.
    await joinVoice(bob.page, peer.communityName, peer.voiceName);
    await joinVoice(alice.page, peer.communityName, peer.voiceName);
    await expect.poll(async () => {
      const voice = await slice<VoiceState>(alice.page, 'voice');
      const mine = Object.values(voice ?? {}).flatMap((c) => c.participants).find((p) => p.deviceId === a.device);
      // voice_state flags (protocol/02): bit 0 in_call, bit 1 self_mute.
      return mine === undefined ? null : (mine.flags & 2) === 2;
    }, { ...POLL, message: 'her own participant says self_mute' }).toBe(true);
    await expect(tile(bob.page, 'alice').locator('.d-call-tile__state[data-state="muted"]')).toBeVisible({ timeout: WAIT });

    await test.step('web3.8 FX I2: bob sees her muted, never "not heard yet", while she sends nothing; unmuted she is heard', async () => {
      const hers = tile(bob.page, 'alice');
      // Nothing of hers was verified (no frame went out), and the stage does not pretend otherwise.
      await expect(hers).toHaveAttribute('data-voice-state', 'not-heard', { timeout: WAIT });
      await bob.page.waitForTimeout(3_000);
      await expect(hers.locator('.d-call-tile__state[data-state="not-heard"]')).toHaveCount(0);
      await expect(hers).not.toContainText(copy('voice.state.notHeard'));
      await panelMute(alice).click();
      await expect(panelMute(alice)).toHaveAttribute('aria-pressed', 'false', { timeout: WAIT });
      await expect(hers).toHaveAttribute('data-voice-state', 'ok', { timeout: WAIT });
    });
  } finally {
    await peerBrowser.close(bob);
    await peerBrowser.close(alice);
    await peer.close();
  }
});
