import { join } from 'node:path';
import assert from 'node:assert/strict';

// Used only by a separately compiled acceptance app; production trust and
// transport source are unchanged. Exported to compile these exact transforms.
export function windowsFixtureSource(name,text,{appId,product,profile,scheme,origin,reports,version}){
 // Git's native Windows checkout uses CRLF. Transform canonical source lines so
 // every required observation attaches identically on all acceptance hosts.
 text=text.replace(/\r\n/g,'\n');

     if(name==='updater')text=text.replace(/const feed = `[^`]+`;/,`const feed=${JSON.stringify(origin+'/release.json')};`).replace('url: new URL(this.artifact.url)',`url: new URL(${JSON.stringify(origin+'/artifact')})`).replace('boundedFetch(target.programManifest.url,1048576)',`boundedFetch(${JSON.stringify(origin+'/manifest')},1048576)`);
     if(name==='windows-installation')text=text.replaceAll("'dev.dilla.desktop'",JSON.stringify(appId));
     if(name==='update-runtime')text=text.replaceAll("stdio: 'ignore'","stdio: 'inherit'");
     if(name==='update-installer')text=text.replace('false,300000)','false,20000)');
     if(name==='update-watchdog')text=text.replace('child = await processes.launch(file, plan.health);',"child = await processes.launch(file, plan.health); if(plan.health.version==='1.0.3')await new Promise(done=>setTimeout(done,10000));");
     if(name==='windows-slots')text=text.replace("const handle=await open(temporary,'wx',0o600);",`if(slot.version==='1.0.7')throw Object.assign(new Error('fixture slot temporary open ENOSPC'),{code:'ENOSPC'});if(slot.version==='1.0.8'){process.getBuiltinModule('fs').writeFileSync(${JSON.stringify(join(reports,'before-slot-publication.json'))},JSON.stringify({pid:process.pid}));await new Promise(done=>setTimeout(done,15000));} const handle=await open(temporary,'wx',0o600);`).replace('attempted=true;await rename(temporary,file);',"attempted=true;await rename(temporary,file);if(slot.version==='1.0.9')throw Object.assign(new Error('fixture publication result lost after rename'),{code:'EIO'});");
     if(name==='main')text=text.replaceAll("app.setName('Dilla');",`app.setName(${JSON.stringify(product)}); app.setPath('userData',${JSON.stringify(profile)});`).replace("app.setAsDefaultProtocolClient('dilla',windowsLauncher,[])",`app.setAsDefaultProtocolClient(${JSON.stringify(scheme)},windowsLauncher,[])`)
      .replace("dialog.showErrorBox('Dilla update failed', previousUpdateFailure);",`if(existsSync(${JSON.stringify(join(reports,'retry-next'))})){process.getBuiltinModule('fs').unlinkSync(${JSON.stringify(join(reports,'retry-next'))});await updater.check();}`)
      .replace('const updater = createUpdater(state => {',`const updater = createUpdater(state => { process.getBuiltinModule('fs').writeFileSync(${JSON.stringify(join(reports,version+'.json'))},JSON.stringify({version:app.getVersion(),pid:process.pid,state,inheritedHealth:process.env.DILLA_UPDATE_STARTUP_NONCE??null}));if(state.phase==='ready')setTimeout(()=>{void requestQuit(true);},200);`)
      .replace('void app.whenReady().then(async () => {',`void app.whenReady().then(async () => { ${version==='1.0.2'?'app.exit(23);return;':''}`)
      .replace('activationPlan = await waitForRegisteredStartup(',`${version==='1.0.3'?`process.getBuiltinModule('fs').writeFileSync(${JSON.stringify(join(reports,'unregistered-main.json'))},JSON.stringify({pid:process.pid,nonce:startupLaunch.nonce,windows:BrowserWindow.getAllWindows().length}));`:''} activationPlan = await waitForRegisteredStartup(`)
      .replace('try { await startupGate.wait(win, abort.signal); owner(event); }',`process.getBuiltinModule('fs').writeFileSync(${JSON.stringify(join(reports,version+'-startup-wait.json'))},JSON.stringify({pid:process.pid,pending:!startupActivated}));try { await startupGate.wait(win, abort.signal); owner(event); }`)
      .replace('const forwarded = target === request.url ? request : new Request(target, request);',`if(new URL(request.url).pathname.startsWith('/v1/')){if(!startupActivated)process.getBuiltinModule('fs').appendFileSync(${JSON.stringify(join(reports,version+'-precommit-api.jsonl'))},JSON.stringify({pid:process.pid})+'\\n');else if(new URL(request.url).pathname==='/v1/instance')process.getBuiltinModule('fs').writeFileSync(${JSON.stringify(join(reports,version+'-activated-discovery.json'))},JSON.stringify({pid:process.pid}));} const forwarded = target === request.url ? request : new Request(target, request);`);

 if(name==='main'){
  const entryFile=JSON.stringify(join(reports,version+'-entry.json')),entryHost='entry-'+product.slice(-12)+'.invalid';
  const gate='const startupGate = new StartupActivationGate(startupActivated);';
  assert.equal(text.split(gate).length,2,'entry observation must attach to the actual production startup gate');
  text=text.replace(gate,gate+`
let fixtureCoreReady=false;
function recordFixtureEntry(){process.getBuiltinModule('fs').writeFileSync(${entryFile},JSON.stringify({pid:process.pid,version:app.getVersion(),execPath:process.execPath,profile:app.getPath('userData'),activated:startupActivated,coreReady:fixtureCoreReady,args:process.argv.filter(arg=>arg==='--hidden'||arg.startsWith('dilla://')),windows:BrowserWindow.getAllWindows().map(win=>({visible:win.isVisible(),enabled:win.isEnabled(),url:win.webContents.getURL()}))}));}`);
  const status="else if (command.type === 'boot-ready') await startupUsability.status(instance, command.usable);";
  assert.equal(text.split(status).length,2,'entry readiness must observe the actual owner-validated worker readiness notification');
  text=text.replace(status,`else if (command.type === 'boot-ready'){await startupUsability.status(instance,command.usable);fixtureCoreReady=command.usable;recordFixtureEntry();}`);
  const boot='void app.whenReady().then(async () => {';
  text=text.replace(boot,boot+`const originalDialog=dialog.showMessageBox.bind(dialog);dialog.showMessageBox=((...args:unknown[])=>{const options=args.at(-1) as {title?:string;message?:string;detail?:string};if(options?.title==='Connect to a Dilla instance'){process.getBuiltinModule('fs').writeFileSync(${JSON.stringify(join(reports,'declined-entry-invite.json'))},JSON.stringify({pid:process.pid,version:app.getVersion(),profile:app.getPath('userData'),message:options.message,detail:options.detail,response:0}));return Promise.resolve({response:0,checkboxChecked:false});}return (originalDialog as (...args:unknown[])=>unknown)(...args);}) as typeof dialog.showMessageBox;`);
  const finish='refreshMenu();\n      if (previousUpdateFailure)';
  assert.equal(text.split(finish).length,2,'entry native-window snapshot must follow actual startup activation');
  text=text.replace(finish,`if(existsSync(${JSON.stringify(join(reports,'enable-autostart'))})){process.getBuiltinModule('fs').unlinkSync(${JSON.stringify(join(reports,'enable-autostart'))});await setAutostart(true);process.getBuiltinModule('fs').writeFileSync(${JSON.stringify(join(reports,'autostart-configured.json'))},JSON.stringify({pid:process.pid,profile:app.getPath('userData')}));}recordFixtureEntry();`+finish);
  text=text.replace('if (app.isPackaged && !previousUpdateFailure) {',`if (app.isPackaged && !previousUpdateFailure) { process.getBuiltinModule('fs').rmSync(${JSON.stringify(join(reports,'retry-next'))},{force:true});`);
  const forwarded='const forwarded = target === request.url ? request : new Request(target, request);';
  text=text.replace(forwarded,`if(new URL(request.url).hostname===${JSON.stringify(entryHost)})process.getBuiltinModule('fs').appendFileSync(${JSON.stringify(join(reports,'unknown-entry-network.jsonl'))},JSON.stringify({pid:process.pid})+'\\n');`+forwarded);
 }
 return text;
}
export function windowsFixtureInstaller(installerScript,stageFault,product){
 if(stageFault){const marker='  CreateDirectory "$INSTDIR\\versions\\${DILLA_PROGRAM_ID}"',injected=stageFault==='once'?`\n  IfFileExists "$APPDATA\\${product}\\stage-failed-once" fixture_stage_continue\n  FileOpen $0 "$APPDATA\\${product}\\stage-failed-once" w\n  FileClose $0\n  FileOpen $0 "$INSTDIR\\versions\\\${DILLA_PROGRAM_ID}\\partial-fixture.txt" w\n  FileWrite $0 "partial inactive NSIS stage"\n  FileClose $0\n  SetErrorLevel 17\n  Quit\nfixture_stage_continue:\n`:`\n  FileOpen $0 "$INSTDIR\\versions\\\${DILLA_PROGRAM_ID}\\partial-fixture.txt" w\n  FileWrite $0 "partial inactive NSIS stage"\n  FileClose $0\n  Sleep 600000\n`;assert.equal(installerScript.split(marker).length,2);installerScript=installerScript.replace(marker,marker+injected);}
 return installerScript;
}
