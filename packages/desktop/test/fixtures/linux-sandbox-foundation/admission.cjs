// Fixture-only entry. The bridge and parser are fixed packaged resources; no
// renderer or argument can select a module, gate or status path.
const { app, BrowserWindow, Tray, nativeImage } = require('electron');
const fs = require('node:fs');
const path = require('node:path');
const args = process.argv.filter(x => x.startsWith('--admission-'));
const values = Object.fromEntries(args.map(x => { const i = x.indexOf('='); return [x.slice(0, i), x.slice(i + 1)]; }));
const nonce = values['--admission-nonce'], profile = values['--admission-profile'];
if (args.length !== 3 || Object.keys(values).sort().join(',') !== '--admission-mode,--admission-nonce,--admission-profile'
 || values['--admission-mode'] !== 'proof' || !/^[a-f0-9]{64}$/.test(nonce || '') || !path.isAbsolute(profile || '') || process.getuid() === 0) app.exit(41);
else {
 try {
  const addon = require(path.join(process.resourcesPath, 'admission-proof', 'admission.node'));
  const parser = require(path.join(process.resourcesPath, 'admission-proof', 'parser.cjs'));
  parser.parseLinuxAdmission(addon.hold());
 } catch { process.stderr.write('DILLA_ADMISSION_PROOF_FAILURE admission\n'); app.exit(40); return; }
 // Only after the synchronous admission gate may the profile be touched.
 app.on('window-all-closed', () => {});
 app.setPath('userData', profile);
 fs.mkdirSync(path.join(profile, 'session'), { recursive: true, mode: 0o700 });
 app.setPath('sessionData', path.join(profile, 'session'));
 const report = (phase,extra={}) => fs.writeSync(3, JSON.stringify({format:1, nonce, phase, pid:process.pid,...extra})+'\n');
 let win, tray;
 app.whenReady().then(async () => {
  win = new BrowserWindow({show:false,webPreferences:{sandbox:true,contextIsolation:true,nodeIntegration:false}});
  await win.loadURL('data:text/html,<!doctype html><title>Admission proof</title>');
  report('window');
  let input='';
  const control=fs.createReadStream(null,{fd:4,autoClose:false,encoding:'utf8'});
  control.on('data',chunk=>{input+=chunk; if(input.length>64){app.exit(43);return;} let n;
   while((n=input.indexOf('\n'))>=0){const command=input.slice(0,n);input=input.slice(n+1);
    if(command==='tray' && win){win.destroy();win=null;tray=new Tray(nativeImage.createEmpty());const child=require('node:child_process').spawn('/usr/bin/sleep',['60'],{stdio:'ignore'});child.unref();report('tray',{childPid:child.pid});}
    else if(command==='exit' && tray){tray.destroy();app.exit(0);}
    else app.exit(43);
   }
  });
 }).catch(()=>{process.stderr.write('DILLA_ADMISSION_PROOF_FAILURE runtime\n');app.exit(42);});
}
