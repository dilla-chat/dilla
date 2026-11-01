import { join } from 'node:path';
import { randomUUID } from 'node:crypto';
import { canonicalSHA512, strictWindowsJSON } from './windows-manifest.ts';
import { verifyWindowsRelease } from './windows-release.ts';
import { newerVersion } from './update.ts';
import { verifyArtifactFile } from './appimage.ts';
import { updateFS, syncUpdateDirectory } from './update-fs.ts';
export interface WindowsSlot { format:1;kind:'initial'|'release';generation:number;version:string;manifestSha512:string;nonce?:string;acceptedAt?:number }
export interface WindowsSlotTrust { publicKeys:readonly string[];channel:'stable'|'beta';arch:string;initialVersion:string;initialManifestSha512:string;now?:number }
export class WindowsCommitUncertain extends Error { override name='WindowsCommitUncertain'; }
const { lstat,realpath,open,rename,rm }=updateFS.promises;
export function parseWindowsSlot(raw:Uint8Array):WindowsSlot {
 if(raw.byteLength>1024)throw new Error('Windows slot too large');
 const value=strictWindowsJSON(raw) as WindowsSlot;
 if(!value||typeof value!=='object'||Array.isArray(value)||value.format!==1||!['initial','release'].includes(value.kind)
  ||!Number.isSafeInteger(value.generation)||value.generation<0||typeof value.version!=='string'||!newerVersion(value.version,'0.0.0')||!canonicalSHA512(value.manifestSha512))throw new Error('Invalid Windows slot');
 const fields=value.kind==='initial'?['format','kind','generation','version','manifestSha512']:['format','kind','generation','version','manifestSha512','nonce','acceptedAt'];
 if(Object.keys(value).length!==fields.length||fields.some(key=>!Object.hasOwn(value,key))
  ||(value.kind==='initial'?value.generation!==0:value.generation<1||typeof value.nonce!=='string'||!/^[a-f0-9]{64}$/.test(value.nonce)||!Number.isSafeInteger(value.acceptedAt)||value.acceptedAt!<1))throw new Error('Invalid Windows slot fields');
 return value;
}
export function sameWindowsSlot(a:WindowsSlot,b:WindowsSlot):boolean{return a.format===b.format&&a.kind===b.kind&&a.generation===b.generation&&a.version===b.version&&a.manifestSha512===b.manifestSha512&&a.nonce===b.nonce&&a.acceptedAt===b.acceptedAt;}
export function selectWindowsSlot(slots:WindowsSlot[]):WindowsSlot {
 if(!slots.length||slots.length>2)throw new Error('A valid committed Windows slot is required');
 const sorted=[...slots].sort((a,b)=>b.generation-a.generation),first=sorted[0];
 if(sorted.length===2&&first.generation===sorted[1].generation&&!sameWindowsSlot(first,sorted[1]))throw new Error('Conflicting Windows slot generations');
 return first;
}
/** Main-only bounded content reads. The native launcher additionally holds the
 * Windows owner/DACL/reparse guards through CreateProcess. */
export async function readWindowsRecord(file:string,limit:number):Promise<Buffer> {
 const info=await lstat(file);if(!info.isFile()||info.isSymbolicLink()||info.nlink!==1||await realpath(file)!==file)throw new Error('Invalid Windows provenance file');
 const handle=await open(file,updateFS.constants.O_RDONLY|(updateFS.constants.O_NOFOLLOW??0));
 try { const stat=await handle.stat();if(!stat.isFile()||stat.nlink!==1||stat.size>limit)throw new Error('Windows provenance bound exceeded');const bytes=Buffer.alloc(limit+1),{bytesRead}=await handle.read(bytes,0,bytes.length,0);if(bytesRead>limit)throw new Error('Windows provenance bound exceeded');return bytes.subarray(0,bytesRead); }
 finally{await handle.close();}
}
export async function verifyWindowsSlot(slot:WindowsSlot,trust:WindowsSlotTrust,provenance:string):Promise<void> {
 parseWindowsSlot(Buffer.from(JSON.stringify(slot)));
 if(slot.kind==='initial'){if(slot.version!==trust.initialVersion||slot.manifestSha512!==trust.initialManifestSha512)throw new Error('Untrusted initial Windows slot');return;}
 const acceptedAt=slot.acceptedAt!;if(acceptedAt>(trust.now??Date.now())+300000)throw new Error('Invalid Windows slot acceptance time');
 const directory=join(provenance,'startup-'+slot.nonce),entry=await lstat(directory);if(!entry.isDirectory()||entry.isSymbolicLink()||await realpath(directory)!==directory)throw new Error('Invalid Windows slot provenance directory');
 const metadata=await readWindowsRecord(join(directory,'release.json'),1048576),signature=(await readWindowsRecord(join(directory,'release.json.sig'),128)).toString().trim();
 const release=verifyWindowsRelease(metadata,signature,{publicKeys:trust.publicKeys,channel:trust.channel,platform:'win32',arch:trust.arch,currentVersion:trust.initialVersion,now:acceptedAt}),artifact=release.artifacts.find(file=>file.platform==='win32'&&file.arch===trust.arch)!;
 if(release.version!==slot.version||artifact.programManifest?.sha512!==slot.manifestSha512)throw new Error('Windows slot differs from signed release');
 await verifyArtifactFile(join(directory,'update.exe'),artifact);
 const journal=strictWindowsJSON(await readWindowsRecord(join(directory,'journal.json'),1024)) as Record<string,unknown>;
 if(!journal||journal.format!==1||journal.nonce!==slot.nonce||journal.phase!=='complete'||journal.version!==slot.version||!Number.isSafeInteger(journal.childPid)||(journal.childPid as number)<=1||(journal.childPid as number)>0xFFFFFFFF
  ||Object.keys(journal).some(key=>!['format','nonce','phase','version','childPid','watchdogPid','installerActorPid','installerPid','failure'].includes(key)))throw new Error('Windows slot requires its completed registered journal');
}
/** Caller has ALREADY awaited the complete journal's file flush. The rename is
 * the one-way activation point; any later uncertainty must never restore old. */
export async function publishWindowsSlot(root:string,slot:WindowsSlot,faults?:{beforeRename?():Promise<void>;afterRename?():Promise<void>}):Promise<'committed'> {
 parseWindowsSlot(Buffer.from(JSON.stringify(slot)));if(slot.kind!=='release')throw new Error('Cannot republish initial Windows slot');
 const file=join(root,`windows-active-${slot.generation%2}.json`),temporary=file+'.'+randomUUID(),bytes=Buffer.from(JSON.stringify(slot));let attempted=false;
 try {
  const handle=await open(temporary,'wx',0o600);try{await handle.writeFile(bytes);await handle.sync();}finally{await handle.close();}
  await faults?.beforeRename?.();attempted=true;await rename(temporary,file);
  await faults?.afterRename?.();await syncUpdateDirectory(root);return 'committed';
 }catch(error){
  if(!attempted)throw error;
  try{if((await readWindowsRecord(file,1024)).equals(bytes))return 'committed';}catch{/* Visible publication could be durable; never grant rollback on unreadable state. */}
  throw new WindowsCommitUncertain('Windows activation publication is uncertain; retain both programs and restart through Dilla');
 }finally{try{await rm(temporary,{force:true});}catch{/* Cleanup cannot undo or misreport a visible commit. */}}
}
