import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createRequire } from 'node:module';
import { mkdtemp, mkdir, writeFile, readFile, rm, realpath } from 'node:fs/promises';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { execFileSync } from 'node:child_process';
const require = createRequire(import.meta.url);
const hook = require('../scripts/linux-appimage-launcher.cjs');
test('production AppRun preserves arguments and removes helper override', {skip:process.platform==='win32'}, async()=>{
 const dir=await realpath(await mkdtemp(join(tmpdir(),'dilla-apprun-')));
 try {
  await writeFile(join(dir,'AppRun'),hook.PRODUCTION_APP_RUN_SCRIPT,{mode:0o755});
  await writeFile(join(dir,'dilla-sandbox-launcher'),`#!${process.execPath}\nconsole.log(JSON.stringify({args:process.argv.slice(2),appdir:process.env.APPDIR,helper:process.env.CHROME_DEVEL_SANDBOX}));\n`,{mode:0o755});
  const args=['argument with spaces','literal-$HOME',';not-a-command',''];
  const out=JSON.parse(execFileSync(join(dir,'AppRun'),args,{encoding:'utf8',env:{...process.env,APPDIR:dir,CHROME_DEVEL_SANDBOX:'/tmp/evil'}}));
  assert.deepEqual(out,{args,appdir:dir});
  assert.doesNotMatch(hook.PRODUCTION_APP_RUN_SCRIPT,/no-sandbox|disable.*sandbox|unshare|eval/);
 } finally {await rm(dir,{recursive:true,force:true});}
});
test('AppImage artifact hook refuses missing or wrong build context',async()=>{
 await hook({electronPlatformName:'win32'});
 await assert.rejects(hook.artifactBuildStarted({targetPresentableName:'AppImage',arch:1,file:'/tmp/x.AppImage'}),/context/i);
 await assert.rejects(hook({electronPlatformName:'linux',arch:3,packager:{executableName:'dilla-desktop'}}),/x64/i);
});
test('production and installed builders select both packaging hooks',async()=>{
 const production=require('../electron-builder.cjs');assert.equal(production.afterPack,hook);assert.deepEqual(production.appImage.executableArgs,[]);
 const smoke=await readFile(new URL('./native/installed-upgrade.test.mjs',import.meta.url),'utf8');
 assert.match(smoke,/createInstalledPackages\(instance\)/);
 for(const file of [new URL('../../../e2e/web/support/installed-packages.ts',import.meta.url)]){
  const source=await readFile(file,'utf8');assert.match(source,/scripts\/linux-appimage-launcher\.cjs/);assert.match(source,/createLinuxAppImageHooks/);
 }
});

test('fixture hooks require an explicit absolute public configuration',()=>{
 assert.equal(typeof hook.createLinuxAppImageHooks,'function');
 assert.throws(()=>hook.createLinuxAppImageHooks({configuration:'relative.json'}),/absolute/i);
 assert.throws(()=>hook.createLinuxAppImageHooks({}),/configuration/i);
 const fixture=hook.createLinuxAppImageHooks({configuration:'/tmp/fixture-public.json'});
 assert.equal(typeof fixture.afterPack,'function');assert.equal(typeof fixture.artifactBuildStarted,'function');
 assert.notEqual(fixture.afterPack,hook);
});

test('fixture hook compiles the supplied public maintenance trust into its image launcher', {skip:process.platform!=='linux'||!process.env.DILLA_INSPECT_FIXTURE_ROOT}, async()=>{
 const {copyFile,readlink}=await import('node:fs/promises'); const {generateKeyPairSync}=await import('node:crypto');
 const root=await realpath(await mkdtemp(join(tmpdir(),'dilla-fixture-hooks-'))), app=join(root,'app'), out=join(root,'out');
 try {
  await mkdir(app);await mkdir(join(app,'resources'));await mkdir(out);
  const extracted=process.env.DILLA_INSPECT_FIXTURE_ROOT!;
  await copyFile(join(extracted,'dilla-desktop'),join(app,'dilla-desktop'));
  await copyFile(join(extracted,'resources/app.asar'),join(app,'resources/app.asar'));
  await copyFile(new URL('../../../node_modules/electron/dist/chrome-sandbox',import.meta.url),join(app,'chrome-sandbox'));
  const config=JSON.parse(await readFile(new URL('../linux-sandbox-component.json',import.meta.url),'utf8'));
  const pair=generateKeyPairSync('ed25519'); const fixtureKey=pair.publicKey.export({type:'spki',format:'der'}).subarray(-32).toString('base64');
  config.publicKeys=[fixtureKey];const configFile=join(root,'fixture-public.json');await writeFile(configFile,JSON.stringify(config));
  const fixture=hook.createLinuxAppImageHooks({configuration:configFile});
  await fixture.afterPack({electronPlatformName:'linux',arch:1,appOutDir:app,outDir:out,packager:{executableName:'dilla-desktop',appInfo:{version:'1.0.0'}}});
  await fixture.artifactBuildStarted({targetPresentableName:'AppImage',arch:1,file:join(out,'fixture.AppImage')});
  const launcher=await readFile(join(app,'dilla-sandbox-launcher'));
  assert.ok(launcher.includes(Buffer.from(fixtureKey)));for(const pinned of JSON.parse(await readFile(new URL('../linux-sandbox-component.json',import.meta.url),'utf8')).publicKeys) assert.ok(!launcher.includes(Buffer.from(pinned)),'the committed sandbox key is not in the fixture launcher');
  assert.equal(await readlink(join(app,'chrome-sandbox')),'/usr/lib/dilla/appimage/chrome-sandbox');
  assert.ok((await readFile(join(out,'fixture.sandbox'))).length>0);
 } finally {await rm(root,{recursive:true,force:true});}
});
