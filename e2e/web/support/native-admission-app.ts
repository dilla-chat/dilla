import {spawn,type ChildProcess} from 'node:child_process';
import {readFile} from 'node:fs/promises';
import {join} from 'node:path';
import {chromium,type Browser,type Page} from '@playwright/test';
import type {Readable} from 'node:stream';

type Phase={format:1;phase:string;pid:number;childPid:number|null;nonce:string};
const sleep=()=>new Promise(done=>setTimeout(done,100));
export class NativeAdmissionProcess {
 readonly root:string;readonly profile:string;readonly nonce:string;readonly child:ChildProcess;readonly exit:Promise<{code:number|null;signal:NodeJS.Signals|null}>;readonly closed:Promise<void>;private phases:Phase[]=[];private buffer='';private invalid=false;private browser?:Browser;
 constructor(root:string,profile:string,nonce:string,args:string[]=[]) {
  this.root=root;this.profile=profile;this.nonce=nonce;
  const env={...process.env};for(const key of ['APPIMAGE','APPDIR','ELECTRON_RUN_AS_NODE','NODE_OPTIONS'])delete env[key];
  this.child=spawn(join(root,'app/dilla-desktop'),args,{env,detached:true,stdio:['ignore','ignore','ignore','pipe']});
  this.closed=new Promise(done=>this.child.once('close',()=>done()));
  this.exit=new Promise((done,reject)=>{this.child.once('error',reject);this.child.once('exit',(code,signal)=>done({code,signal}));});
  const stream=this.child.stdio[3] as Readable;stream.setEncoding('utf8');stream.on('data',(part:string)=>{this.buffer+=part;if(this.buffer.length>4096){this.invalid=true;return;}let i;
   while((i=this.buffer.indexOf('\n'))>=0){try{const value=JSON.parse(this.buffer.slice(0,i));if(Object.keys(value).sort().join(',')!=='childPid,format,nonce,phase,pid'||value.format!==1||value.nonce!==nonce||value.pid!==this.child.pid||!['gate','verified','forward','child','hidden'].includes(value.phase)||!(value.childPid===null||Number.isSafeInteger(value.childPid)&&value.childPid>1))throw Error();this.phases.push(value);}catch{this.invalid=true;}this.buffer=this.buffer.slice(i+1);}
  });
 }
 observed(){if(this.invalid||this.buffer.length)throw Error('Invalid fixture phase stream');return this.phases.map(p=>p.phase);}
 async phase(name:string,timeout=15000):Promise<Phase>{const deadline=Date.now()+timeout;while(Date.now()<deadline){if(this.invalid)throw Error('Invalid fixture phase');const index=this.phases.findIndex(p=>p.phase===name);if(index>=0)return this.phases.splice(index,1)[0];if(this.child.exitCode!==null||this.child.signalCode!==null)throw Error('Fixture exited before phase');await sleep();}throw Error('Fixture phase timeout: '+name);}
 async page(instance:string,timeout=90000):Promise<Page>{const end=Date.now()+timeout;while(Date.now()<end){try{const port=Number((await readFile(join(this.profile,'DevToolsActivePort'),'utf8')).split('\n')[0]);if(!Number.isInteger(port)||port<1||port>65535)throw Error();if(!this.browser?.isConnected())this.browser=await chromium.connectOverCDP('http://127.0.0.1:'+port,{timeout:3000});const page=this.browser.contexts()[0]?.pages().find(p=>p.url().startsWith(instance));if(page)return page;}catch{}await sleep();}throw Error('Fixture client did not start');}
 async stop(){try{if(this.child.pid)process.kill(-this.child.pid,'SIGKILL');}catch(error){if((error as NodeJS.ErrnoException).code!=='ESRCH')throw error;}if(this.child.exitCode===null&&this.child.signalCode===null){let timer:NodeJS.Timeout|undefined;try{await Promise.race([this.exit,new Promise<never>((_,reject)=>{timer=setTimeout(()=>reject(Error('Owned process cleanup timed out')),5000);})]);}finally{clearTimeout(timer);}}await this.browser?.close().catch(()=>{});}
}
/** The native peer's real text-group roster identifies the one newly seated
 * browser device without observing worker messages or exposing account data. */
export async function captureNewPeerDevice(peer:{seat(count:number):Promise<string[]>},before:string[]):Promise<string>{
 const valid=(ids:string[],length:number)=>ids.length===length&&new Set(ids).size===length&&ids.every(id=>/^[a-f0-9]{32}$/.test(id));
 if(!valid(before,2))throw Error('Invalid initial fixture roster');
 const after=await peer.seat(2);if(!valid(after,3)||!before.every(id=>after.includes(id)))throw Error('Fixture roster changed unexpectedly');
 const added=after.filter(id=>!before.includes(id));if(added.length!==1)throw Error('Ambiguous fixture peer device');return added[0];
}
