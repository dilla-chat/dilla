// Portable signed-file fixture. The read-only launcher stand-in below provides
// compiled public description data; it is NOT Windows kernel/NSIS acceptance.
import{mkdtemp,mkdir,writeFile,rm,rename}from'node:fs/promises';
import{join}from'node:path';import{tmpdir}from'node:os';import{createHash,generateKeyPairSync,sign}from'node:crypto';
import{createWindowsProgramManifest,windowsProgramID}from'../../src/windows-manifest.ts';
import{prepareInstalledUpdate,type InstalledUpdatePlan,type UpdateTrust}from'../../src/update-plan.ts';
import{activateInstalledUpdate}from'../../src/update-state.ts';import{writeUpdateJournal}from'../../src/update-journal.ts';
import type{WindowsSlot}from'../../src/windows-slots.ts';
const hash=(value:Uint8Array)=>createHash('sha512').update(value).digest('base64');
export async function windowsInstalledFixture(run:(fixture:{plan:InstalledUpdatePlan;trust:UpdateTrust;root:string;nextSlot:WindowsSlot;nextProgram:string;nextManifest:string;publish():Promise<void>})=>Promise<void>){
 const root=await mkdtemp(join(tmpdir(),'dilla-windows-files-'));
 try{
  const stable=join(root,'Programs/Dilla'),profile=join(root,'profile/Dilla'),old=join(root,'old'),next=join(root,'next');
  for(const directory of[stable,profile,old,next,join(stable,'versions'),join(stable,'manifests'),join(profile,'updates')])await mkdir(directory,{recursive:true,mode:0o700});
  for(const[directory,name]of[[old,'old'],[next,'new']]){await mkdir(join(directory,'resources'));await writeFile(join(directory,'dilla-desktop.exe'),name+'-exe');await writeFile(join(directory,'resources/app.asar'),name+'-asar');}
  const context={version:'1.0.0',channel:'stable' as const,arch:'x64' as const},oldRaw=await createWindowsProgramManifest(old,context),oldHash=hash(oldRaw),oldID=windowsProgramID(oldHash);
  await rename(old,join(stable,'versions',oldID));await writeFile(join(stable,'manifests',Buffer.from(oldHash,'base64').toString('hex')+'.json'),oldRaw);
  const oldSlot={format:1 as const,kind:'initial' as const,generation:0,version:'1.0.0',manifestSha512:oldHash};await writeFile(join(profile,'updates/windows-active-0.json'),JSON.stringify(oldSlot),{mode:0o600});
  const keys=generateKeyPairSync('ed25519'),publicKeys=[keys.publicKey.export({type:'spki',format:'pem'}).toString()],description={format:1,appId:'dev.dilla.desktop',productName:'Dilla',channel:'stable',arch:'x64',initialVersion:'1.0.0',initialManifestSha512:oldHash,trustHash:createHash('sha256').update(JSON.stringify(publicKeys)).digest('hex')},launcher=join(stable,'Dilla.exe');
  await writeFile(launcher,`#!${process.execPath}\nconsole.log(${JSON.stringify(JSON.stringify(description))});\n`,{mode:0o700});
  const installed=join(stable,'versions',oldID,'dilla-desktop.exe'),newRaw=await createWindowsProgramManifest(next,{...context,version:'1.0.1'}),installer=Buffer.from('authenticated NSIS bytes'),downloaded=join(root,'download.exe');await writeFile(downloaded,installer);
  const now=Date.now(),metadata=Buffer.from(JSON.stringify({format:1,version:'1.0.1',channel:'stable',publishedAt:new Date(now-1000).toISOString(),expiresAt:new Date(now+3600000).toISOString(),artifacts:[{platform:'win32',arch:'x64',url:'https://github.com/jonasthim/dilla/releases/download/desktop-v1.0.1/dilla-1.0.1-win-x64.exe',size:installer.length,sha512:hash(installer),programManifest:{url:'https://github.com/jonasthim/dilla/releases/download/desktop-v1.0.1/dilla-1.0.1-win-x64.manifest.json',size:newRaw.length,sha512:hash(newRaw)}}]})),trust:UpdateTrust={publicKeys,platform:'win32',arch:'x64',version:'1.0.0',channel:'stable',now};
  const plan=await prepareInstalledUpdate({userData:profile,installed,downloaded,metadata,signature:sign(null,metadata,keys.privateKey).toString('base64'),parentPid:process.pid,windows:{root:stable,launcher,oldSlot,initialVersion:'1.0.0',initialManifestSha512:oldHash},programManifest:newRaw},trust);
  await activateInstalledUpdate(plan);await writeUpdateJournal(plan,{phase:'complete',childPid:0xFFFFFFFF});
  const nextSlot:WindowsSlot={format:1,kind:'release',generation:1,version:'1.0.1',manifestSha512:hash(newRaw),nonce:plan.health.nonce,acceptedAt:now},nextProgram=join(stable,'versions',windowsProgramID(hash(newRaw))),nextManifest=join(stable,'manifests',Buffer.from(hash(newRaw),'base64').toString('hex')+'.json');
  await run({plan,trust,root,nextSlot,nextProgram,nextManifest,publish:async()=>{await rename(next,nextProgram);await writeFile(nextManifest,newRaw);await writeFile(join(profile,'updates/windows-active-1.json'),JSON.stringify(nextSlot),{mode:0o600});}});
 }finally{await rm(root,{recursive:true,force:true,maxRetries:10,retryDelay:100});}
}
