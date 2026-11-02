import {open,readFile,readdir,readlink,stat} from 'node:fs/promises';
import {constants} from 'node:fs';
import {createHash} from 'node:crypto';
export type OwnedProcess={pid:number;start:string;kind:'zygote'|'renderer'|'utility'|'other';seccomp:number;noNewPrivs:number};
type ProcessDir={read:(name:string)=>Promise<string>;list:(name:string)=>Promise<string[]>;link:(name:string)=>Promise<string>;info:(name:string)=>Promise<{dev:number;ino:number}>;close:()=>Promise<void>};
type ProcessIO={open:(pid:number)=>Promise<ProcessDir>};
const procIO:ProcessIO={async open(pid){
 const handle=await open('/proc/'+pid,constants.O_RDONLY|constants.O_DIRECTORY|constants.O_NOFOLLOW);
 const root='/proc/self/fd/'+handle.fd;
 return {read:name=>readFile(root+'/'+name,'utf8'),list:name=>readdir(root+'/'+name),link:name=>readlink(root+'/'+name),info:name=>stat(root+'/'+name),close:()=>handle.close()};
}};
function pidValue(pid:number){if(!Number.isSafeInteger(pid)||pid<2)throw Error('Invalid owned process');}
function parseStatus(status:string){
 const uid=/^Uid:\s+(\d+)\s+(\d+)\s+(\d+)\s+(\d+)/m.exec(status);
 if(!uid||!uid.slice(1).every(x=>Number(x)===process.getuid!()))throw Error('Foreign process in fixture tree');
 const parent=/^PPid:\s+(\d+)/m.exec(status),seccomp=/^Seccomp:\s+(\d+)/m.exec(status),noNewPrivs=/^NoNewPrivs:\s+(\d+)/m.exec(status);
 if(!parent||!seccomp||!noNewPrivs)throw Error('Missing owned process status');
 return {ppid:Number(parent[1]),seccomp:Number(seccomp[1]),noNewPrivs:Number(noNewPrivs[1])};
}
function parseStat(raw:string){const at=raw.lastIndexOf(')');if(at<0)throw Error('Invalid owned process state');const fields=raw.slice(at+2).split(' ');if(!/^\d+$/.test(fields[19]??'')||!fields[0])throw Error('Invalid owned process state');return {start:fields[19],state:fields[0]};}
async function identity(dir:ProcessDir,expected?:string){const status=parseStatus(await dir.read('status'));const data=parseStat(await dir.read('stat'));if(expected!==undefined&&data.start!==expected)throw Error('Owned process identity changed');return {...status,...data};}
/** Pin /proc directory first; ownership and start precede any command/exe read. */
export async function inspectOwnedProcess(pid:number,expected?:string,io:ProcessIO=procIO,expectedParent?:number) {
 pidValue(pid);const dir=await io.open(pid);
 try{const first=await identity(dir,expected);if(expectedParent!==undefined&&first.ppid!==expectedParent)throw Error('Owned process parent changed');const cmd=await dir.read('cmdline');const last=await identity(dir,first.start);if(last.ppid!==first.ppid)throw Error('Owned process parent changed');
  const match=/(?:^|\0)--type=(zygote|renderer|utility)(?:\0|$)/.exec(cmd);return {pid,start:first.start,state:last.state,ppid:last.ppid,kind:(match?.[1]??'other') as OwnedProcess['kind'],seccomp:last.seccomp,noNewPrivs:last.noNewPrivs};
 }finally{await dir.close();}
}
async function scan(root:number,io:ProcessIO,deadline:number){
 const queue:{pid:number;parent?:number}[]=[{pid:root}],seen=new Set<number>(),out:{record:OwnedProcess;signature:string}[]=[];
 while(queue.length){if(Date.now()>deadline)throw Error('Fixture process observation timed out');const {pid,parent}=queue.shift()!;pidValue(pid);if(seen.has(pid))throw Error('Duplicate owned process');seen.add(pid);if(seen.size>512)throw Error('Fixture process tree exceeds bound');
  const dir=await io.open(pid);
  try{const before=await identity(dir);if(parent!==undefined&&before.ppid!==parent)throw Error('Owned process parent changed');if(['Z','X'].includes(before.state))throw Error('Incomplete owned process tree');
   const cmd=await dir.read('cmdline'),match=/(?:^|\0)--type=(zygote|renderer|utility)(?:\0|$)/.exec(cmd);
   const tasks=(await dir.list('task')).sort();if(!tasks.length||tasks.length>512)throw Error('Invalid fixture thread count');const links:string[]=[];
   for(const tid of tasks){if(Date.now()>deadline)throw Error('Fixture process observation timed out');if(!/^\d+$/.test(tid))throw Error('Invalid fixture thread');const raw=await dir.read('task/'+tid+'/children');const children=raw.trim().split(/\s+/).filter(Boolean).map(Number).sort((a,b)=>a-b);for(const child of children){pidValue(child);queue.push({pid:child,parent:pid});}links.push(tid+':'+children.join(','));}
   const after=await identity(dir,before.start);if(after.ppid!==before.ppid||['Z','X'].includes(after.state))throw Error('Incomplete owned process tree');
   out.push({record:{pid,start:before.start,kind:(match?.[1]??'other') as OwnedProcess['kind'],seccomp:after.seccomp,noNewPrivs:after.noNewPrivs},signature:[pid,before.start,before.ppid,after.seccomp,after.noNewPrivs,createHash('sha256').update(cmd).digest('hex'),...links].join(';')});
  }finally{await dir.close();}
 }
 return out.sort((a,b)=>a.record.pid-b.record.pid);
}
/** Two identical bounded snapshots; any vanished thread/child fails closed. */
export async function observeOwnedTree(root:number,io:ProcessIO=procIO):Promise<OwnedProcess[]> {
 pidValue(root);const deadline=Date.now()+5000;for(let attempt=0;attempt<3;attempt++){const first=await scan(root,io,deadline),second=await scan(root,io,deadline);if(first.map(x=>x.signature).join('|')===second.map(x=>x.signature).join('|'))return second.map(x=>x.record);}
 throw Error('Unstable owned process tree');
}
export async function aliveWithIdentity(entry:OwnedProcess):Promise<boolean> {
 pidValue(entry.pid);let dir:ProcessDir;try{dir=await procIO.open(entry.pid);}catch(error){if(['ENOENT','ESRCH'].includes((error as NodeJS.ErrnoException).code??''))return false;throw error;}
 try{const status=parseStatus(await dir.read('status'));void status;const data=parseStat(await dir.read('stat'));return data.start===entry.start&&!['Z','X'].includes(data.state);}
 catch(error){if(['ENOENT','ESRCH'].includes((error as NodeJS.ErrnoException).code??''))return false;throw error;}finally{await dir.close();}
}
export async function verifyOwnedExecutable(pid:number,expectedPath:string):Promise<void> {
 pidValue(pid);const dir=await procIO.open(pid);
 try{const before=await identity(dir);const actual=await dir.link('exe');const actualStat=await dir.info('exe'),expected=await stat(expectedPath);await identity(dir,before.start);
  if(actual!==expectedPath||actualStat.dev!==expected.dev||actualStat.ino!==expected.ino)throw Error('Owned executable does not match fixture');
 }finally{await dir.close();}
}
