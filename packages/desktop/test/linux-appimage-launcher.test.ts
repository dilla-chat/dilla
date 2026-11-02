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
 for(const file of [new URL('./native/installed-upgrade.test.mjs',import.meta.url),new URL('../../../e2e/web/support/installed-packages.ts',import.meta.url)]){
  const source=await readFile(file,'utf8');assert.match(source,/scripts\/linux-appimage-launcher\.cjs/);assert.match(source,/scripts\/linux-appimage-artifact\.cjs/);
 }
});
