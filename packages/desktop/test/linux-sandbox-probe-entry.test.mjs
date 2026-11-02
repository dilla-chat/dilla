import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createRequire } from 'node:module';
const require = createRequire(import.meta.url);
const { parseProbeArguments } = require('../src/linux-sandbox-probe-arguments.cjs');
const nonce='0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef';
test('probe entry accepts only the exact isolated role and rejects malformed reserved arguments',()=>{
 assert.deepEqual(parseProbeArguments(['--dilla-sandbox-probe='+nonce,'--dilla-sandbox-profile=/tmp/dilla-sandbox-probe-a']),{nonce,profile:'/tmp/dilla-sandbox-probe-a'});
 assert.equal(parseProbeArguments(['dilla://host?x=--dilla-sandbox-probe']),null);
 for(const args of [ ['--dilla-sandbox-probe='+nonce], ['--dilla-sandbox-probe=bad','--dilla-sandbox-profile=/tmp/dilla-sandbox-probe-a'], ['--dilla-sandbox-probe='+nonce,'--dilla-sandbox-profile=/tmp/dilla-sandbox-probe-a','extra'], ['--dilla-sandbox-profile=/tmp/dilla-sandbox-probe-a'], ['--dilla-sandbox-probe='+nonce,'--dilla-sandbox-probe='+nonce,'--dilla-sandbox-profile=/tmp/dilla-sandbox-probe-a'], ['--dilla-sandbox-unknown'] ]) assert.throws(()=>parseProbeArguments(args));
});

test('early bootstrap selects isolated probe without loading ordinary app', async()=>{
 const fs=require('node:fs');const vm=require('node:vm');
 const source=fs.readFileSync(new URL('../src/linux-sandbox-bootstrap.cjs',import.meta.url),'utf8');
 const loads=[];const exits=[];let started;
 const fakeRequire=name=>{loads.push(name);if(name==='./linux-sandbox-probe-arguments.cjs')return {parseProbeArguments};if(name==='./linux-sandbox-probe-main.cjs')return {start:role=>{started=role;}};if(name==='electron')return {app:{exit:code=>exits.push(code)}};throw Error('unexpected ordinary load '+name);};
 vm.runInNewContext(source,{require:fakeRequire,process:{argv:['dilla','--dilla-sandbox-probe='+nonce,'--dilla-sandbox-profile=/tmp/dilla-sandbox-probe-a']}});
 assert.equal(started.nonce,nonce);assert.equal(loads.includes('./main-app.cjs'),false);assert.deepEqual(exits,[]);
});

test('probe acknowledgement binds the owned frame and sandbox facts',()=>{
 const { validAck }=require('../src/linux-sandbox-probe-main.cjs');
 const frame={url:'dilla-sandbox://probe/index.html'}, owner={mainFrame:frame};
 const event={sender:owner,senderFrame:frame}, value={nonce,sandboxed:true,contextIsolated:true};
 assert.equal(validAck(event,value,owner,nonce),true);
 for(const [ev,v] of [[{sender:{},senderFrame:frame},value],[{sender:owner,senderFrame:{url:frame.url}},value],[event,{...value,nonce:'wrong'}],[event,{...value,sandboxed:false}],[event,{...value,extra:true}]])assert.equal(validAck(ev,v,owner,nonce),false);
});

test('probe starts isolated and denies network and permissions before acknowledging its owned frame',async()=>{
 const fs=require('node:fs'),vm=require('node:vm'),path=require('node:path');
 const source=fs.readFileSync(new URL('../src/linux-sandbox-probe-main.cjs',import.meta.url),'utf8');
 const calls=[],handlers={};let win,request,check,permission,protocolHandler,output='';
 const contents={mainFrame:{url:'dilla-sandbox://probe/index.html'},setWindowOpenHandler:fn=>calls.push(['open',fn()]),on:()=>{},getOSProcessId:()=>45};
 class Window{constructor(options){win=this;this.webContents=contents;calls.push(['window',options]);}async loadURL(url){calls.push(['load',url]);handlers.ack({sender:contents,senderFrame:contents.mainFrame},{nonce,sandboxed:true,contextIsolated:true});}}
 const app={setPath:(...args)=>calls.push(['path',...args]),enableSandbox:()=>calls.push(['sandbox']),whenReady:()=>Promise.resolve(),exit:code=>calls.push(['exit',code])};
 const electron={app,BrowserWindow:Window,ipcMain:{on:(_name,fn)=>{handlers.ack=fn;}},protocol:{registerSchemesAsPrivileged:()=>calls.push(['scheme'])},session:{fromPartition:()=>({protocol:{handle:(_name,fn)=>{protocolHandler=fn;}},setPermissionRequestHandler:fn=>{permission=fn;},setPermissionCheckHandler:fn=>{check=fn;},webRequest:{onBeforeRequest:fn=>{request=fn;}}})}};
 const fakeFS={lstatSync:()=>({isDirectory:()=>true,isSymbolicLink:()=>false,uid:1000,mode:0o700}),realpathSync:x=>x,readdirSync:()=>[],fstatSync:()=>({isFIFO:()=>true}),mkdirSync:()=>{},readFileSync:()=>Buffer.from('<!doctype html>'),writeSync:(_fd,data)=>{output+=data;},readSync:(_fd,b)=>{b[0]=1;return 1;}};
 const module={exports:{}};const fakeRequire=name=>name==='electron'?electron:name==='node:fs'?fakeFS:name==='node:path'?path:require(name);
 vm.runInNewContext(source,{require:fakeRequire,module,exports:module.exports,__dirname:'/bundle',process:{platform:'linux',getuid:()=>1000,geteuid:()=>1000,pid:44},Promise,Buffer,Response});
 module.exports.start({nonce,profile:'/tmp/dilla-sandbox-probe-test'});await new Promise(resolve=>setImmediate(resolve));
 assert.ok(win);assert.equal(calls.find(x=>x[0]==='window')[1].webPreferences.sandbox,true);assert.equal(calls.find(x=>x[0]==='window')[1].webPreferences.nodeIntegration,false);
 let cancelled;request({url:'https://example.invalid/'},value=>{cancelled=value.cancel;});assert.equal(cancelled,true);assert.equal(check(),false);let allowed;permission(null,null,value=>{allowed=value;});assert.equal(allowed,false);
 assert.equal((await protocolHandler({url:'dilla-sandbox://probe/other',method:'GET'})).status,404);
 assert.equal(JSON.parse(output).rendererPid,45);assert.deepEqual(calls.at(-1),['exit',0]);
});
