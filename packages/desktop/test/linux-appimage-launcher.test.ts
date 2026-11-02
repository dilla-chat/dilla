import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createRequire } from 'node:module';
import { mkdtemp,mkdir,writeFile,readFile,rm,realpath } from 'node:fs/promises';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { execFileSync } from 'node:child_process';
const require=createRequire(import.meta.url);
const afterPack=require('../scripts/linux-appimage-launcher.cjs');
test('Linux packaging writes one fixed AppRun and leaves other platforms unchanged',async()=>{
 const root=await realpath(await mkdtemp(join(tmpdir(),'dilla-apprun-')));
 try{
  await afterPack({electronPlatformName:'win32',appOutDir:root});await assert.rejects(readFile(join(root,'AppRun')),{code:'ENOENT'});
  await afterPack({electronPlatformName:'linux',appOutDir:root,packager:{executableName:'dilla-desktop'}});assert.equal(await readFile(join(root,'AppRun'),'utf8'),afterPack.APP_RUN_SCRIPT);
  assert.doesNotMatch(afterPack.APP_RUN_SCRIPT,/no-sandbox|disable.*sandbox|unshare|eval/);
  assert.match(afterPack.APP_RUN_SCRIPT,/exec "\$APPDIR\/dilla-desktop" "\$@"/);
  await assert.rejects(afterPack({electronPlatformName:'linux',appOutDir:root,packager:{executableName:'different'}}),/executable/);
 }finally{await rm(root,{recursive:true,force:true});}
});
test('the production AppRun preserves exact argv/environment without calling available, blocked or missing unshare',{skip:process.platform==='win32'},async()=>{
 const root=await realpath(await mkdtemp(join(tmpdir(),'dilla-apprun-argv-')));
 try{
  for(const state of ['available','blocked','missing']){
   const directory=join(root,state);await mkdir(directory);await afterPack({electronPlatformName:'linux',appOutDir:directory,packager:{executableName:'dilla-desktop'}});
   await writeFile(join(directory,'dilla-desktop'),`#!${process.execPath}\nconsole.log(JSON.stringify({args:process.argv.slice(2),appdir:process.env.APPDIR,path:process.env.PATH,ld:process.env.LD_LIBRARY_PATH,xdg:process.env.XDG_DATA_DIRS,schemas:process.env.GSETTINGS_SCHEMA_DIR}));\n`,{mode:0o755});
   if(state!=='missing')await writeFile(join(directory,'unshare'),`#!/bin/sh\nprintf called > '${join(directory,'probe-called')}'\nexit ${state==='blocked'?1:0}\n`,{mode:0o755});
   const args=['argument with spaces','literal-$HOME',';not-a-command','',"single'quote"];
   const result=JSON.parse(execFileSync(join(directory,'AppRun'),args,{encoding:'utf8',env:{...process.env,APPDIR:directory,PATH:directory,LD_LIBRARY_PATH:'/fixture-libraries',XDG_DATA_DIRS:'/fixture-data',GSETTINGS_SCHEMA_DIR:'/fixture-schemas'}}));
   assert.deepEqual(result.args,args);assert.equal(result.appdir,directory);assert.equal(result.path,`${directory}:${directory}/usr/sbin:${directory}`);assert.equal(result.ld,`${directory}/usr/lib:/fixture-libraries`);assert.ok(result.xdg.startsWith(`${directory}/usr/share:/fixture-data:`));assert.equal(result.schemas,`${directory}/usr/share/glib-2.0/schemas:/fixture-schemas`);
   await assert.rejects(readFile(join(directory,'probe-called')),{code:'ENOENT'});
  }
 }finally{await rm(root,{recursive:true,force:true});}
});
test('the pinned AppImage builder copies the afterPack AppRun over its generated staging launcher',async()=>{
 require('app-builder-lib');const{copyDir}=require('builder-util');
 const root=await realpath(await mkdtemp(join(tmpdir(),'dilla-apprun-stage-'))),app=join(root,'app'),stage=join(root,'stage');
 try{await mkdir(app);await mkdir(stage);await afterPack({electronPlatformName:'linux',appOutDir:app,packager:{executableName:'dilla-desktop'}});await writeFile(join(stage,'AppRun'),'old generated launcher');await copyDir(app,stage);assert.equal(await readFile(join(stage,'AppRun'),'utf8'),afterPack.APP_RUN_SCRIPT);}
 finally{await rm(root,{recursive:true,force:true});}
});


test('production and both installed Linux acceptance builders share the secure launcher and empty desktop executable args',async()=>{
 const production=require('../electron-builder.cjs');assert.equal(production.afterPack,afterPack);assert.deepEqual(production.appImage.executableArgs,[]);
 for(const file of [new URL('./native/installed-upgrade.test.mjs',import.meta.url),new URL('../../../e2e/web/support/installed-packages.ts',import.meta.url)]){
  const source=await readFile(file,'utf8');assert.match(source,/afterPack:\s*join\(desktop,\s*'scripts\/linux-appimage-launcher\.cjs'\)/);assert.match(source,/appImage:\s*\{\s*executableArgs:\s*\[\]\s*\}/);
 }
});
