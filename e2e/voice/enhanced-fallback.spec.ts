import {expect,test} from './support/peer-browser';
import {watchContexts} from './support/leaks';
import {VoicePeer,WAIT,copy,onboard,openVoiceSettings,closeSettings} from './support/voice';
watchContexts(test);
test.skip(!['evaluation','beta'].includes(process.env.DILLA_NOISE_MODE??''),'Requires an explicit evaluation or beta build');
test('a missing enhanced model visibly falls back; Off avoids all enhancement assets',async({peerBrowser})=>{
  const peer=peerBrowser.track(await VoicePeer.start()),alice=await peerBrowser.open('alice');
  const assets:string[]=[];
  try{
    await peer.setup();await onboard(alice,peer,'alice');
    await alice.context.route('**/assets/noise-model-*.bin',route=>route.fulfill({status:404,body:'fixture: model unavailable'}));
    alice.context.on('request',r=>{if(/noise-(model|wasm|glue)-/.test(r.url()))assets.push(r.url());});
    const dialog=await openVoiceSettings(alice.page);
    await dialog.getByRole('radio',{name:copy('settings.voice.automatic'),exact:true}).click();
    await dialog.getByRole('button',{name:copy('signalSettings.voice.testStart'),exact:true}).click();
    await expect(dialog.getByText(copy('settings.voice.processor',{processor:'RNNoise'}),{exact:false})).toBeVisible({timeout:WAIT});
    await expect(dialog.getByText(copy('settings.voice.fallback.enhanced-failed'),{exact:false})).toBeVisible();
    await dialog.getByRole('button',{name:copy('signalSettings.voice.testStop'),exact:true}).click();
    expect(assets.length).toBeGreaterThan(0);assets.length=0;
    await dialog.getByRole('radio',{name:copy('settings.voice.off'),exact:true}).click();
    await dialog.getByRole('button',{name:copy('signalSettings.voice.testStart'),exact:true}).click();
    await expect(dialog.getByText(copy('settings.voice.processor',{processor:copy('settings.voice.processor.off')}),{exact:false})).toBeVisible({timeout:WAIT});
    await dialog.getByRole('button',{name:copy('signalSettings.voice.testStop'),exact:true}).click();
    expect(assets).toEqual([]);await closeSettings(alice.page);
  }finally{await peerBrowser.close(alice);await peer.close();}
});
