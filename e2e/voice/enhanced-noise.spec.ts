import { expect, test } from './support/peer-browser';
import { watchContexts } from './support/leaks';
import { VoicePeer, WAIT, copy, onboard, openVoiceSettings, closeSettings, joinVoice, noiseOff, pushToTalk, sentCount, heardCount, framesPass, self } from './support/voice';
import { writeFileSync,readFileSync } from 'node:fs';
import os from 'node:os';
// @ts-ignore -- local evaluation utility, never imported by the app
import {processingP99} from '../../experiments/noise/runtime-metrics.mjs';
watchContexts(test);
test.skip(process.env.DILLA_NOISE_MODE!=='evaluation','Requires the explicitly enabled evaluation build');
test('Automatic enhances the local test and encrypted calls, enforces PTT, and remains stable under load',async({peerBrowser,browserName,browser})=>{
  const measuredBuild=JSON.parse(readFileSync('../packages/web/dist/noise-build.json','utf8'));
  const seconds=Number(process.env.DILLA_NOISE_SECONDS??10);
  test.setTimeout((seconds+180)*1000);
  const peer=peerBrowser.track(await VoicePeer.start()),alice=await peerBrowser.open('alice'),bob=await peerBrowser.open('bob');
  const thirdParty:string[]=[];
  alice.context.on('request',r=>{if(!['http://127.0.0.1:8481','ws://127.0.0.1:8481'].includes(new URL(r.url()).origin))thirdParty.push(r.url());});
  try{
    await alice.page.addInitScript(()=>{
      const events:Record<string,unknown>[]=[];
      Object.assign(window,{noiseEvents:events});
      const NativeWorker=window.Worker;
      window.Worker=class extends NativeWorker{constructor(url:string|URL,options?:WorkerOptions){super(url,options);this.addEventListener('message',e=>{if(e.data.kind==='error')console.info('NOISE_PROCESSOR_ERROR '+JSON.stringify(e.data));});}};
      const Native=window.AudioWorkletNode;
      window.AudioWorkletNode=class extends Native {
        constructor(ctx:BaseAudioContext,name:string,options?:AudioWorkletNodeOptions){
          super(ctx,name,options);
          if(name==='dilla-deepfilter-transport')this.port.addEventListener('message',e=>{if(e.data.kind==='metrics')events.push({...e.data,at:performance.now()});});
        }
      };
    });
    alice.page.on('console',message=>{if(message.text().startsWith('NOISE_PROCESSOR_ERROR '))console.log(message.text());});
    await peer.setup();await onboard(alice,peer,'alice');await onboard(bob,peer,'bob');await peer.seat(2);
    let dialog=await openVoiceSettings(alice.page);
    await dialog.getByRole('radio',{name:copy('settings.voice.automatic'),exact:true}).click();
    await dialog.getByRole('button',{name:copy('signalSettings.voice.testStart'),exact:true}).click();
    await expect(dialog.getByText(copy('settings.voice.processor',{processor:'DeepFilterNet3'}),{exact:false})).toBeVisible({timeout:WAIT});
    await expect(dialog.getByText(copy('settings.voice.fallback.evaluation'),{exact:false})).toBeVisible();
    await dialog.getByRole('button',{name:copy('signalSettings.voice.testStop'),exact:true}).click();await closeSettings(alice.page);
    await noiseOff(bob.page);await pushToTalk(alice.page,'Insert');
    await joinVoice(alice.page,peer.communityName,peer.voiceName);await joinVoice(bob.page,peer.communityName,peer.voiceName);
    dialog=await openVoiceSettings(alice.page);
    await expect(dialog.getByText(copy('settings.voice.processor',{processor:'DeepFilterNet3'}),{exact:false})).toBeVisible({timeout:WAIT});
    await closeSettings(alice.page);await alice.page.locator('body').click({position:{x:1,y:1}});
    const a=await self(alice.page);
    await framesPass(bob.page,await sentCount(bob.page),25);
    const idle0=await heardCount(bob.page,a.device);await framesPass(bob.page,await sentCount(bob.page),100);
    const idle=await heardCount(bob.page,a.device)-idle0;
    await alice.page.keyboard.down('Insert');
    const held0=await heardCount(bob.page,a.device);await framesPass(bob.page,await sentCount(bob.page),100);
    const held=await heardCount(bob.page,a.device)-held0;
    await alice.page.keyboard.up('Insert');await framesPass(bob.page,await sentCount(bob.page),25);
    const released0=await heardCount(bob.page,a.device);await framesPass(bob.page,await sentCount(bob.page),100);
    const released=await heardCount(bob.page,a.device)-released0;
    expect(idle).toBeLessThanOrEqual(10);expect(held).toBeGreaterThan(10);expect(released).toBeLessThanOrEqual(10);
    await alice.page.evaluate(()=>{(window as unknown as {noiseEvents:unknown[]}).noiseEvents.length=0;});
    await alice.page.keyboard.down('Insert');
    const started=Date.now();
    alice.page.on('console',message=>{if(message.text().startsWith('NOISE_RUNTIME '))console.log(message.text());});
    await alice.page.evaluate(duration=>new Promise<void>(resolve=>{
      const load=setInterval(()=>{const until=performance.now()+6;while(performance.now()<until){Math.sqrt(Math.random());}},16);
      const start=performance.now();
      const progress=setInterval(()=>{
        const events=(window as unknown as {noiseEvents:Record<string,unknown>[]}).noiseEvents,last=events.at(-1);
        console.info('NOISE_RUNTIME '+JSON.stringify({elapsedSeconds:Math.round((performance.now()-start)/1000),buffers:last?.buffers,frames:last?.frames,underruns:last?.underruns,dropped:last?.dropped}));
      },60000);
      setTimeout(()=>{clearInterval(load);clearInterval(progress);resolve();},duration*1000);
    }),seconds);
    await alice.page.keyboard.up('Insert');
    const events=await alice.page.evaluate(()=>(window as unknown as {noiseEvents:Record<string,unknown>[]}).noiseEvents);
    dialog=await openVoiceSettings(alice.page);
    const effectiveText=await dialog.innerText();
    await closeSettings(alice.page);
    const last=events.filter(e=>e.kind==='metrics').at(-1)??{},p99ProcessingMs=processingP99(last);
    const report={effectiveText,...measuredBuild,engine:'deepfilter',kind:'integrated-encrypted-call',browser:browserName,browserVersion:browser.version(),durationSeconds:(Date.now()-started)/1000,loadedCall:true,load:'two encrypted peers; 6 ms busy main-thread work per 16 ms on sender',cpu:os.cpus()[0]?.model,platform:browserName==='chromium'?'desktop-chrome':'desktop-firefox',os:os.platform(),buffers:last.buffers,underruns:last.underruns,dropped:last.dropped,p99ProcessingMs,frameMs:10,ptt:{idle,held,released},events,thirdPartyRequests:thirdParty,addedDelayMs:null,qualified:false};
    writeFileSync(`../experiments/noise/results/${browserName}-integrated-call.json`,JSON.stringify(report,null,2)+'\n');
    expect(events.length).toBeGreaterThan(0);expect(thirdParty).toEqual([]);
    expect(effectiveText).toContain(copy('settings.voice.processor',{processor:'DeepFilterNet3'}));
    expect(Number(last.underruns)/Number(last.buffers)).toBeLessThan(.001);expect(Number(last.dropped)).toBe(0);
    expect(p99ProcessingMs).not.toBeNull();expect(p99ProcessingMs).toBeLessThan(8);
  }finally{await peerBrowser.close(bob);await peerBrowser.close(alice);await peer.close();}
});
