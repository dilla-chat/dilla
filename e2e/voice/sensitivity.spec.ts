// Signal task 10b (README §5.9 Input sensitivity): the threshold drives a real voice-activity gate in the call. The fake
// microphone sends a steady 1 kHz tone at about −9 dBFS: with the line at 0 dB it is under the threshold and nothing is heard
// (the gate sends the mute path's silence, still encrypted); with the line at −100 dB it is heard again. The settings persist
// across a reload.
import { expect, test } from './support/peer-browser';
import { watchContexts } from './support/leaks';
import { AUDIBLE_RMS, VoicePeer, WAIT, audible, audibleLevel, closeSettings, copy, heard, joinVoice, noiseOff, onboard,
  openVoiceSettings, self } from './support/voice';

watchContexts(test);

test('a voice under the threshold is not sent; the threshold and the levels persist', async ({ peerBrowser }) => {
  const peer = peerBrowser.track(await VoicePeer.start());
  const alice = await peerBrowser.open('alice');
  const bob = await peerBrowser.open('bob');
  try {
    await peer.setup();
    await onboard(alice, peer, 'alice');
    await onboard(bob, peer, 'bob');
    await peer.seat(2);
    const a = await self(alice.page);
    await noiseOff(alice.page);
    await noiseOff(bob.page);

    // Manual sensitivity, the line at 0 dB (End on the slider), before the call.
    let dialog = await openVoiceSettings(alice.page);
    await dialog.getByRole('switch', { name: copy('signalSettings.voice.auto'), exact: true }).click();
    const line = dialog.getByRole('slider', { name: copy('signalSettings.voice.threshold'), exact: true });
    await expect(line).toBeEnabled();
    await line.focus();
    await alice.page.keyboard.press('End');
    await expect(dialog.getByText(copy('signalSettings.voice.thresholdValue', { value: '0 dB' }), { exact: true })).toBeVisible({ timeout: WAIT });
    // A range is driven by the keyboard, as a person would: End (200 %), then fifty steps down; each key up writes.
    const volume = dialog.getByRole('slider', { name: copy('signalSettings.voice.inputVolume'), exact: true });
    await volume.focus();
    await alice.page.keyboard.press('End');
    for (let i = 0; i < 50; i++) await alice.page.keyboard.press('ArrowLeft');
    await expect(volume).toHaveAttribute('aria-valuetext', '150%');
    await closeSettings(alice.page);

    await joinVoice(alice.page, peer.communityName, peer.voiceName);
    await joinVoice(bob.page, peer.communityName, peer.voiceName);
    // The call runs (frames arrive, encrypted and verified as alice's device), but they carry silence.
    await heard(bob.page, a.device);
    await expect.poll(() => audibleLevel(bob.page), { timeout: 5000 }).toBeLessThan(AUDIBLE_RMS);
    await bob.page.waitForTimeout(1500);
    expect(await audibleLevel(bob.page)).toBeLessThan(AUDIBLE_RMS);

    // Lowering the line during the call opens the gate without rejoining.
    dialog = await openVoiceSettings(alice.page);
    await dialog.getByRole('slider', { name: copy('signalSettings.voice.threshold'), exact: true }).focus();
    await alice.page.keyboard.press('Home');
    await expect(dialog.getByText(copy('signalSettings.voice.thresholdValue', { value: '-100 dB' }), { exact: true })).toBeVisible({ timeout: WAIT });
    await closeSettings(alice.page);
    await audible(bob.page);

    // The settings survive a reload.
    await alice.page.reload();
    dialog = await openVoiceSettings(alice.page);
    await expect(dialog.getByRole('switch', { name: copy('signalSettings.voice.auto'), exact: true })).toHaveAttribute('aria-checked', 'false');
    await expect(dialog.getByRole('slider', { name: copy('signalSettings.voice.threshold'), exact: true })).toHaveValue('-100');
    await expect(dialog.getByRole('slider', { name: copy('signalSettings.voice.inputVolume'), exact: true })).toHaveValue('150');
    await closeSettings(alice.page);
  } finally {
    await peerBrowser.close(alice);
    await peerBrowser.close(bob);
    await peer.close();
  }
});
