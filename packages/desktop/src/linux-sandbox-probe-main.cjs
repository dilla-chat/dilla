'use strict';
const PAGE='dilla-sandbox://probe/index.html';
function validAck(event,value,owner,nonce){
 return !!event && event.sender===owner && event.senderFrame===owner.mainFrame && event.senderFrame?.url===PAGE
  && !!value && typeof value==='object' && !Array.isArray(value) && Object.keys(value).sort().join(',')==='contextIsolated,nonce,sandboxed'
  && value.nonce===nonce && value.sandboxed===true && value.contextIsolated===true;
}
function start({nonce,profile}) {
 const {app,BrowserWindow,ipcMain,protocol,session}=require('electron');
 const fs=require('node:fs'), path=require('node:path');
 try {
  if(process.platform!=='linux'||process.getuid()===0||process.geteuid()!==process.getuid()) throw Error('Invalid probe identity');
  const dir=fs.lstatSync(profile);
  if(!dir.isDirectory()||dir.isSymbolicLink()||dir.uid!==process.getuid()||(dir.mode&0o7777)!==0o700||fs.realpathSync(profile)!==profile||fs.readdirSync(profile).length!==0||!fs.fstatSync(3).isFIFO()||!fs.fstatSync(4).isFIFO())throw Error('Invalid probe resources');
  app.setPath('userData',profile);
  const sessionPath=path.join(profile,'session');fs.mkdirSync(sessionPath,{mode:0o700});app.setPath('sessionData',sessionPath);
  app.enableSandbox();
  protocol.registerSchemesAsPrivileged([{scheme:'dilla-sandbox',privileges:{standard:true,secure:true,supportFetchAPI:true}}]);
  void app.whenReady().then(async()=>{
   const ses=session.fromPartition('dilla-probe-'+nonce);
   ses.protocol.handle('dilla-sandbox',request=>request.url===PAGE&&request.method==='GET'
    ? new Response(fs.readFileSync(path.join(__dirname,'linux-sandbox-probe.html')),{headers:{'content-type':'text/html; charset=utf-8'}})
    : new Response(null,{status:404}));
   ses.setPermissionRequestHandler((_webContents,_permission,callback)=>callback(false));
   ses.setPermissionCheckHandler(()=>false);
   ses.webRequest.onBeforeRequest((details,callback)=>callback({cancel:details.url!==PAGE}));
   const win=new BrowserWindow({show:false,webPreferences:{session:ses,sandbox:true,contextIsolation:true,nodeIntegration:false,preload:path.join(__dirname,'linux-sandbox-probe-preload.cjs'),additionalArguments:['--dilla-sandbox-nonce='+nonce]}});
   win.webContents.setWindowOpenHandler(()=>({action:'deny'}));
   win.webContents.on('will-navigate',event=>event.preventDefault());
   win.webContents.on('will-attach-webview',event=>event.preventDefault());
   let received=false,resolveAck;
   const acknowledged=new Promise(resolve=>{resolveAck=resolve;});
   ipcMain.on('dilla-sandbox-ack',(event,value)=>{if(received||!validAck(event,value,win.webContents,nonce))return;received=true;resolveAck();});
   await win.loadURL(PAGE);await acknowledged;
   const rendererPid=win.webContents.getOSProcessId();if(!Number.isSafeInteger(rendererPid)||rendererPid<2)throw Error('Invalid renderer');
   const report=JSON.stringify({format:1,kind:'dilla-sandbox-probe',nonce,mainPid:process.pid,rendererPid,sandboxed:true,contextIsolated:true})+'\n';
   fs.writeSync(3,report);const ack=Buffer.alloc(1);if(fs.readSync(4,ack,0,1,null)!==1||ack[0]!==1)throw Error('Invalid parent response');
   app.exit(0);
  }).catch(()=>app.exit(1));
 } catch {app.exit(1);}
}
module.exports={start,validAck};
