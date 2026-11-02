import {spawn,spawnSync} from 'node:child_process';
import {isAbsolute} from 'node:path';
import {lstatSync} from 'node:fs';
function identity(path:string) {const s=lstatSync(path);if(!s.isFile()||s.isSymbolicLink()||s.nlink!==1)throw Error('Invalid fixture lock');return {dev:s.dev,ino:s.ino};}
function same(path:string,expected:{dev:number;ino:number}) {const actual=identity(path);if(actual.dev!==expected.dev||actual.ino!==expected.ino)throw Error('Fixture lock inode changed');}
function validPath(path:string) {if(!isAbsolute(path)||path.includes('\0'))throw Error('Invalid fixture actor path');}
export function tryFixtureExclusive(lock:string):boolean {
 validPath(lock);const expected=identity(lock);const result=spawnSync('flock',['--exclusive','--nonblock',lock,'true'],{timeout:3000});
 if(result.error||result.signal||![0,1].includes(result.status!))throw Error('Fixture lock actor failed');same(lock,expected);return result.status===0;
}
/** Parent-owned test actor; lock and pending bytes are fixed before launch. */
export async function acquireFixtureExclusive(lock:string,status:string,pending:string) {
 validPath(lock);validPath(status);const expected=identity(lock);if(Buffer.byteLength(pending)>4096)throw Error('Invalid fixture pending');
 const code=`const fs=require('node:fs'),path=require('node:path');const status=${JSON.stringify(status)},pending=${JSON.stringify(pending)};process.stdout.write('ready\\n');let input='';process.stdin.setEncoding('utf8');process.stdin.on('data',chunk=>{input+=chunk;if(input.length>32)process.exit(2);let i;while((i=input.indexOf('\\n'))>=0){const cmd=input.slice(0,i);input=input.slice(i+1);if(cmd==='publish'){const tmp=status+'.new';const fd=fs.openSync(tmp,'wx',0o644);fs.writeFileSync(fd,pending);fs.fsyncSync(fd);fs.closeSync(fd);fs.renameSync(tmp,status);const dir=fs.openSync(path.dirname(status),'r');fs.fsyncSync(dir);fs.closeSync(dir);process.stdout.write('published\\n');}else if(cmd==='release'){process.exit(0);}else process.exit(2);}});`;
 const child=spawn('flock',['--no-fork','--exclusive','--nonblock',lock,process.execPath,'-e',code],{stdio:['pipe','pipe','ignore']});
 let pendingLine='',queue:string[]=[],waiter:((value:string)=>void)|undefined;
 child.stdout.setEncoding('utf8');child.stdout.on('data',chunk=>{pendingLine+=chunk;if(pendingLine.length>256){child.kill();return;}let i;while((i=pendingLine.indexOf('\n'))>=0){const line=pendingLine.slice(0,i);pendingLine=pendingLine.slice(i+1);if(waiter){const done=waiter;waiter=undefined;done(line);}else queue.push(line);}});
 const exited=new Promise<{code:number|null;signal:NodeJS.Signals|null}>((done,reject)=>{child.once('error',reject);child.once('exit',(code,signal)=>done({code,signal}));});
 const line=async()=>{let timer:NodeJS.Timeout|undefined;try{return await Promise.race([queue.length?Promise.resolve(queue.shift()!):new Promise<string>(done=>{waiter=done;}),exited.then(()=>{throw Error('Fixture lock actor exited');}),new Promise<never>((_,reject)=>{timer=setTimeout(()=>reject(Error('Fixture lock actor timeout')),3000);})]);}finally{clearTimeout(timer);}};
 try{if(await line()!=='ready')throw Error('Fixture lock actor protocol');same(lock,expected);}catch(error){if(child.exitCode===null&&child.signalCode===null)child.kill();await exited.catch(()=>{});throw error;}
 return {async publish(){same(lock,expected);child.stdin.write('publish\n');if(await line()!=='published')throw Error('Fixture lock publish failed');},async release(){child.stdin.end('release\n');let timer:NodeJS.Timeout|undefined;try{const result=await Promise.race([exited,new Promise<never>((_,reject)=>{timer=setTimeout(()=>reject(Error('Fixture lock release timeout')),3000);})]);if(result.code!==0||result.signal!==null)throw Error('Fixture lock release failed');}finally{clearTimeout(timer);if(child.exitCode===null&&child.signalCode===null){child.kill('SIGKILL');await exited.catch(()=>{});}}}};
}
