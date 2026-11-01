import{test}from'node:test';import assert from'node:assert/strict';import{readFile}from'node:fs/promises';import{join,resolve}from'node:path';import{transform}from'esbuild';
// @ts-expect-error native acceptance build tooling
import{windowsFixtureSource}from'./support/windows-update-fixture.mjs';
test('Windows acceptance compiles the actual production bootstrap with fixed fixture identity/transport and all fault observations',async()=>{
 const desktop=resolve(import.meta.dirname,'..'),context={appId:'dev.dilla.fixture',product:'DillaFixture',profile:'C:/Users/fixture/AppData/Roaming/DillaFixture',scheme:'dilla-fixture',origin:'http://127.0.0.1:1',reports:'C:/fixture-reports',version:'1.0.3'};
 for(const name of ['main','updater','update-runtime','update-watchdog','update-journal','update-installer','windows-installation','windows-slots']){
  const source=await readFile(join(desktop,'src',name+'.ts'),'utf8'),compiled=windowsFixtureSource(name,source,context);await transform(compiled,{loader:'ts',target:'node24'});
  if(name==='main'){assert.match(compiled,/unregistered-main\.json/);assert.match(compiled,/1\.0\.3-startup-wait\.json/);assert.match(compiled,/1\.0\.3-precommit-api\.jsonl/);assert.match(compiled,/dilla-fixture/);assert.match(compiled,/app\.setPath\('userData'/);assert.match(compiled,/recordFixtureEntry/);assert.match(compiled,/fixtureCoreReady=command\.usable/);assert.match(compiled,/await setAutostart\(true\)/);assert.match(compiled,/Connect to a Dilla instance/);assert.match(compiled,/response:0/);assert.match(compiled,/unknown-entry-network\.jsonl/);}
  if(name==='updater'){assert.match(compiled,/127\.0\.0\.1:1\/manifest/);assert.match(compiled,/127\.0\.0\.1:1\/artifact/);}
  if(name==='windows-installation')assert.doesNotMatch(compiled,/'dev\.dilla\.desktop'/);
  if(name==='windows-slots')assert.match(compiled,/before-slot-publication\.json/);
 }
});

test('OS entry fixture uses the real dilla scheme accepted by the unmodified native launcher',async()=>{
 const source=await readFile(resolve(import.meta.dirname,'../src/main.ts'),'utf8');
 const compiled=windowsFixtureSource('main',source,{appId:'dev.dilla.fixture',product:'DillaUpdateAcceptanceabcdef012345',profile:'C:/fixture/DillaUpdateAcceptanceabcdef012345',scheme:'dilla',origin:'http://127.0.0.1:1',reports:'C:/fixture',version:'1.0.1'});
 await transform(compiled,{loader:'ts',target:'node24'});assert.match(compiled,/setAsDefaultProtocolClient\("dilla",windowsLauncher,\[\]\)/);assert.doesNotMatch(compiled,/dilla-update-[a-f0-9]/);
});
