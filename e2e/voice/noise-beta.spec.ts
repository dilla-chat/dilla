import {expect,test} from './support/peer-browser';
import {watchContexts} from './support/leaks';
import {VoicePeer,WAIT,copy,onboard,openVoiceSettings,closeSettings,joinVoice,noiseOff,pushToTalk,sentCount,heardCount,framesPass,self} from './support/voice';
watchContexts(test);
test.skip(process.env.DILLA_NOISE_MODE!=='beta','Requires an explicit beta build');
test('beta requires opt-in, enhances preview and encrypted calls, and can be left during a call',async({peerBrowser})=>{
  const peer=peerBrowser.track(await VoicePeer.start()),alice=await peerBrowser.open('alice'),bob=await peerBrowser.open('bob');
  const assets:string[]=[],thirdParty:string[]=[];
  alice.context.on('request',r=>{
    if(/noise-(model|wasm|glue)-/.test(r.url()))assets.push(r.url());
    if(!['http://127.0.0.1:8481','ws://127.0.0.1:8481'].includes(new URL(r.url()).origin))thirdParty.push(r.url());
  });
  try{
    await peer.setup();await onboard(alice,peer,'alice');await onboard(bob,peer,'bob');await peer.seat(2);
    let dialog=await openVoiceSettings(alice.page);
    await expect(dialog.getByRole('radio',{name:copy('settings.voice.lightweight'),exact:true})).toHaveAttribute('aria-checked','true');
    await expect(dialog.getByText(copy('settings.voice.noiseBeta'),{exact:true})).toBeVisible();
    await dialog.getByRole('button',{name:copy('signalSettings.voice.testStart'),exact:true}).click();
    // Lightweight can legitimately use browser fallback if capture constraints are refused.
    // Neither effective mode may fetch enhancement assets before the user's opt-in.
    await expect(dialog.getByText(copy('settings.voice.processor',{processor:'RNNoise'}),{exact:false})
      .or(dialog.getByText(copy('settings.voice.processor',{processor:copy('settings.voice.processor.browser')}),{exact:false}))).toBeVisible({timeout:WAIT});
    const browserStatus=dialog.getByText(copy('settings.voice.processor',{processor:copy('settings.voice.processor.browser')}),{exact:false});
    if(await browserStatus.isVisible())await expect(browserStatus).toContainText(copy('settings.voice.fallback.browser-refused'));
    await dialog.getByRole('button',{name:copy('signalSettings.voice.testStop'),exact:true}).click();
    expect(assets).toEqual([]);
    await alice.page.screenshot({path:test.info().outputPath('noise-beta-settings.png')});
    await dialog.getByRole('radio',{name:copy('settings.voice.automatic'),exact:true}).click();
    // Signal task 10 fix round 1 (README §5.9): the beta is the row's BETA tag, no longer the option's name.
    await expect(dialog.getByText(copy('signalSettings.voice.noiseTag'),{exact:true})).toBeVisible();
    await dialog.getByRole('button',{name:copy('signalSettings.voice.testStart'),exact:true}).click();
    await expect(dialog.getByText(copy('settings.voice.processor',{processor:'DeepFilterNet3'}),{exact:false})).toBeVisible({timeout:WAIT});
    await expect(dialog.getByText(copy('settings.voice.fallback.beta'),{exact:false})).toBeVisible();
    expect(assets.length).toBeGreaterThan(0);
    await dialog.getByRole('button',{name:copy('signalSettings.voice.testStop'),exact:true}).click();await closeSettings(alice.page);
    await noiseOff(bob.page);await pushToTalk(alice.page,'Insert');
    await joinVoice(alice.page,peer.communityName,peer.voiceName);await joinVoice(bob.page,peer.communityName,peer.voiceName);
    dialog=await openVoiceSettings(alice.page);
    await expect(dialog.getByText(copy('settings.voice.processor',{processor:'DeepFilterNet3'}),{exact:false})).toBeVisible({timeout:WAIT});
    await expect(dialog.getByText(copy('settings.voice.fallback.beta'),{exact:false})).toBeVisible();
    await closeSettings(alice.page);await alice.page.locator('body').click({position:{x:1,y:1}});
    const sender=await self(alice.page),before=await heardCount(bob.page,sender.device);
    await alice.page.keyboard.down('Insert');await framesPass(bob.page,await sentCount(bob.page),100);await alice.page.keyboard.up('Insert');
    expect(await heardCount(bob.page,sender.device)-before).toBeGreaterThan(10);
    dialog=await openVoiceSettings(alice.page);
    await dialog.getByRole('radio',{name:copy('settings.voice.lightweight'),exact:true}).click();
    await expect(dialog.getByText(copy('settings.voice.processor',{processor:'RNNoise'}),{exact:false})).toBeVisible({timeout:WAIT});
    await expect(dialog.getByText(copy('settings.voice.fallback.beta'),{exact:false})).toHaveCount(0);
    expect(thirdParty).toEqual([]);
    await closeSettings(alice.page);
  }finally{await peerBrowser.close(bob);await peerBrowser.close(alice);await peer.close();}
});
