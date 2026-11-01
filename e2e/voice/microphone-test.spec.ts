import { expect, test } from './support/peer-browser';
import { watchContexts } from './support/leaks';
import { VoicePeer, WAIT, copy, onboard, openVoiceSettings } from './support/voice';
watchContexts(test);
// Signal task 10b (README §5.9): Let's check is a live loopback through the call's own chain; the meter and the pill follow
// the level while it runs, Stop testing (or the ten seconds) releases every capture, and nothing is recorded.
test('Let’s check plays you back live, shows the level and releases the microphone when it stops',async({peerBrowser})=>{
  const peer=peerBrowser.track(await VoicePeer.start()),alice=await peerBrowser.open('alice');
  try {
    await peer.setup();await onboard(alice,peer,'alice');
    await alice.page.evaluate(()=>{
      const streams:MediaStream[]=[];
      (window as unknown as {previewStreams:MediaStream[]}).previewStreams=streams;
      const gum=navigator.mediaDevices.getUserMedia.bind(navigator.mediaDevices);
      navigator.mediaDevices.getUserMedia=async constraints=>{const stream=await gum(constraints);streams.push(stream);return stream;};
    });
    const dialog=await openVoiceSettings(alice.page);
    // At rest nothing is captured: the pill says Not testing and the level is a dash.
    await expect(dialog.getByText(copy('signalSettings.voice.notTesting'),{exact:true})).toBeVisible();
    await expect(dialog.getByText(copy('signalSettings.voice.levelValue',{value:copy('signalSettings.voice.noLevel')}),{exact:true})).toBeVisible();
    // Noise suppression off, so the fake microphone's steady tone (about −9 dBFS) reaches the meter.
    await dialog.getByRole('radio',{name:copy('settings.voice.off'),exact:true}).click();
    await dialog.getByRole('button',{name:copy('signalSettings.voice.testStart'),exact:true}).click();
    const stop=dialog.getByRole('button',{name:copy('signalSettings.voice.testStop'),exact:true});
    await expect(stop).toBeVisible({timeout:WAIT});
    await expect(dialog.getByText(copy('signalSettings.voice.heard'),{exact:true})).toBeVisible({timeout:WAIT});
    await expect(dialog.getByText(/^level -\d+ dB$/)).toBeVisible();
    expect(await dialog.locator('.d-sens__seg[data-state="on"], .d-sens__seg[data-state="hot"]').count()).toBeGreaterThan(0);
    // Nothing recorded: no object URL, no audio element.
    await expect(dialog.locator('audio')).toHaveCount(0);
    await stop.click();
    await expect(dialog.getByRole('button',{name:copy('signalSettings.voice.testStart'),exact:true})).toBeVisible();
    await expect(dialog.getByText(copy('signalSettings.voice.notTesting'),{exact:true})).toBeVisible();
    await expect.poll(()=>alice.page.evaluate(()=>{
      const streams=(window as unknown as {previewStreams:MediaStream[]}).previewStreams;
      return streams.length>0&&streams.every(s=>s.getTracks().every(t=>t.readyState==='ended'));
    })).toBe(true);
    // The ten seconds end it by themselves.
    await dialog.getByRole('button',{name:copy('signalSettings.voice.testStart'),exact:true}).click();
    await expect(stop).toBeVisible({timeout:WAIT});
    await expect(dialog.getByRole('button',{name:copy('signalSettings.voice.testStart'),exact:true})).toBeVisible({timeout:15_000});
  }finally{await peerBrowser.close(alice);await peer.close();}
});
