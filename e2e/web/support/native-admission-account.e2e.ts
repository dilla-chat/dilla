import {test,expect} from '@playwright/test';
import {access,readFile,rename,stat,writeFile} from 'node:fs/promises';
import {join} from 'node:path';
import {buildFullClientAdmission} from './native-admission-build.ts';
import {NativeAdmissionProcess,captureNewPeerDevice} from './native-admission-app.ts';
import {acquireFixtureExclusive,tryFixtureExclusive} from './native-admission-actor.ts';
import {observeOwnedTree,aliveWithIdentity,verifyOwnedExecutable,inspectOwnedProcess} from './native-admission-process.ts';
import {composerBox,openChannel,readKekRecord} from './app.ts';
import {VoicePeer,onboard,joinVoice,noiseOff,heard,WAIT} from '../../voice/support/voice.ts';

test.describe.configure({timeout:15*60_000});
test('full desktop account retains native admission through window, draft, call and forwarding',async({browser,baseURL},info)=>{
 const instance=baseURL!.replace(/\/$/,'')+'/',fixture=await buildFullClientAdmission(instance),lock=join(fixture.gate,'lock');
 let primary:NativeAdmissionProcess|undefined,secondary:NativeAdmissionProcess|undefined,peer:VoicePeer|undefined,peerContext:Awaited<ReturnType<typeof browser.newContext>>|undefined;
 let phase='startup',failed=false,descendantCount=0,survivorCount=0;const checks:string[]=[];
 const bounded=async<T>(promise:Promise<T>,ms:number):Promise<T>=>{let timer:NodeJS.Timeout|undefined;try{return await Promise.race([promise,new Promise<never>((_,reject)=>{timer=setTimeout(()=>reject(Error('owned process timeout')),ms);})]);}finally{clearTimeout(timer);}};
 const waitExit=(proc:NativeAdmissionProcess)=>bounded(proc.exit,15000);
 const absent=async(path:string)=>expect(access(path)).rejects.toMatchObject({code:'ENOENT'});
 try {
  await fixture.verifySandbox();primary=new NativeAdmissionProcess(fixture.root,fixture.profile,fixture.nonce);const gateStat=await stat(lock);
  const busy=async(name:string)=>{const current=await stat(lock);expect([current.dev,current.ino]).toEqual([gateStat.dev,gateStat.ino]);expect(tryFixtureExclusive(lock)).toBe(false);checks.push(name);};
  await primary.phase('gate');await primary.phase('verified');await verifyOwnedExecutable(primary.child.pid!,join(fixture.root,'app/dilla-desktop'));await busy('verified');
  const page=await primary.page(instance);expect(await page.evaluate(()=>({require:typeof (window as any).require,process:typeof (window as any).process}))).toEqual({require:'undefined',process:'undefined'});const goto=page.goto.bind(page);page.goto=(url,options)=>goto(new URL(url,instance).href,options);await busy('window');
  peer=await VoicePeer.start();await peer.setup();
  const person={name:'desktop',context:page.context(),page};await onboard(person,peer,'desktop');const initialRoster=await peer.seat(1);
  expect((await readKekRecord(page)).count).toBe(1);await busy('account');
  phase='tray';process.kill(primary.child.pid!,'SIGUSR1');await primary.phase('hidden');await busy('tray');
  secondary=new NativeAdmissionProcess(fixture.root,fixture.profile,fixture.nonce);await secondary.phase('gate');await secondary.phase('verified');await primary.phase('forward');
  expect((await waitExit(secondary)).code).toBe(0);await expect.poll(()=>page.evaluate(()=>document.visibilityState),{timeout:WAIT}).toBe('visible');await busy('forward');
  phase='draft';await openChannel(page,peer.communityName,peer.textName);const draft='unsent admission fixture';await composerBox(page,peer.textName).fill(draft);await expect(composerBox(page,peer.textName)).toHaveValue(draft);await busy('draft');
  phase='call';peerContext=await browser.newContext({baseURL:instance,permissions:['microphone','camera']});const other=await peerContext.newPage();await onboard({name:'peer',context:peerContext,page:other},peer,'peer');
  const otherDevice=await captureNewPeerDevice(peer,initialRoster);await noiseOff(page);await noiseOff(other);await joinVoice(page,peer.communityName,peer.voiceName);await joinVoice(other,peer.communityName,peer.voiceName);await heard(page,otherDevice);await busy('call');
  await openChannel(page,peer.communityName,peer.textName);await expect(composerBox(page,peer.textName)).toHaveValue(draft);await expect(page.locator('.d-voice-dock')).toHaveAttribute('data-phase','connected',{timeout:WAIT});await busy('draft-in-call');
  phase='release';await fixture.verifySandbox();const tree=await observeOwnedTree(primary.child.pid!);descendantCount=tree.length-1;expect(tree.some(item=>item.kind==='zygote')).toBe(true);expect(tree.some(item=>item.kind==='renderer'&&item.seccomp===2&&item.noNewPrivs===1)).toBe(true);process.kill(primary.child.pid!,'SIGUSR2');const child=(await primary.phase('child')).childPid!;
  const childIdentity=await inspectOwnedProcess(child,undefined,undefined,primary.child.pid!);expect(childIdentity.ppid).toBe(primary.child.pid);
  process.kill(primary.child.pid!,'SIGTERM');await waitExit(primary);
  await expect.poll(()=>tryFixtureExclusive(lock),{timeout:5000}).toBe(true);
  expect(await aliveWithIdentity(childIdentity)).toBe(true);const survivors=await Promise.all(tree.filter(item=>item.pid!==primary!.child.pid).map(aliveWithIdentity));survivorCount=survivors.filter(Boolean).length;checks.push('release','descendants-observed');
  phase='late';const backup=fixture.profile+'.saved';await rename(fixture.profile,backup);
  const pending=JSON.stringify({...fixture.ready,state:'pending'});const actor=await acquireFixtureExclusive(lock,join(fixture.gate,'status.json'),pending);
  const refuse=async(gate=false)=>{const proc=new NativeAdmissionProcess(fixture.root,fixture.profile,fixture.nonce);try{const result=await waitExit(proc);await bounded(proc.closed,5000);expect(result.code).toBe(1);expect(proc.observed()).toEqual(gate?['gate']:[]);await absent(fixture.profile);}finally{await proc.stop();}};
  try{await refuse();checks.push('late-busy-ready');await actor.publish();await refuse();checks.push('late-busy-pending');}finally{await actor.release();}
  await refuse();checks.push('late-pending-first');await refuse();await absent(fixture.profile);checks.push('late-pending-second');
  const setStatus=async(status:object)=>{const holder=await acquireFixtureExclusive(lock,join(fixture.gate,'status.json'),JSON.stringify(status));try{await holder.publish();}finally{await holder.release();}};
  await setStatus({...fixture.ready,manager:'rpm'});await refuse(true);checks.push('manager-mismatch');
  await setStatus({...fixture.ready,identitySha512:Buffer.alloc(64,1).toString('base64')});await refuse(true);checks.push('identity-mismatch');
  await setStatus(fixture.ready);
  const maintainer=join(fixture.root,'package-maintainer'),original=await readFile(maintainer);
  try{const altered=Buffer.from(original);altered[0]^=1;await writeFile(maintainer,altered);await refuse(true);checks.push('payload-tamper');}finally{await writeFile(maintainer,original);}
  const addon=join(fixture.root,'app/resources/native/dilla-admission.node'),heldAddon=addon+'.held';
  await rename(addon,heldAddon);try{await refuse();checks.push('addon-missing');}finally{await rename(heldAddon,addon);}
  const addonBytes=await readFile(addon);try{const invalid=Buffer.from(addonBytes);invalid[0]=0;await writeFile(addon,invalid);await refuse();checks.push('addon-tamper');}finally{await writeFile(addon,addonBytes);}
  const identityPath=join(fixture.root,'package.json'),identityBytes=await readFile(identityPath);
  try{await writeFile(identityPath,JSON.stringify(fixture.identity.identity,null,2));await refuse(true);checks.push('identity-bytes');}finally{await writeFile(identityPath,identityBytes);}
  await rename(backup,fixture.profile);
  await info.attach('native-admission-proof',{body:JSON.stringify({format:1,status:'passed',checks,descendantCount,survivorCount,transforms:fixture.manifest,cppFixture:fixture.cppFixture,sandboxSha512:fixture.sandboxDigest}),contentType:'application/json'});
 } catch(error){failed=true;await info.attach('native-admission-failure',{body:JSON.stringify({format:1,phase,checks}),contentType:'application/json'});throw error;}
 finally{let cleanupFailed=false;for(const close of [()=>secondary?.stop(),()=>primary?.stop(),()=>peerContext?.close(),()=>peer?.close(),()=>fixture.dispose()])try{await bounded(Promise.resolve(close()),5000);}catch{cleanupFailed=true;}if(cleanupFailed&&!failed)throw Error('Fixture cleanup failed');}
});
