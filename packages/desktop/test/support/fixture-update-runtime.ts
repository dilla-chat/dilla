// Compiled test-only source graph. The random public key is embedded by
// esbuild in the same module that reads update plans; production trust is never
// changed and no runtime key override is exposed by the application.
import { build } from 'esbuild';
import { mkdtemp, readFile, writeFile } from 'node:fs/promises';
import { rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { pathToFileURL } from 'node:url';
import { sign } from 'node:crypto';
import { sandboxReleaseFixture } from './linux-sandbox-release-fixture.ts';
import { fixtureCacheSource } from './fixture-cache-portability.ts';

const fixture = sandboxReleaseFixture();
const now=Date.now(), doc=JSON.parse(fixture.manifest.toString());
doc.publishedAt=new Date(now-60_000).toISOString(); doc.expiresAt=new Date(now+60*86400_000).toISOString();
const manifest=Buffer.from(JSON.stringify(doc)), oldLength=fixture.capsule.readUInt32LE(8), header=Buffer.from(fixture.capsule.subarray(0,32)); header.writeUInt32LE(manifest.length,8);
const capsule=Buffer.concat([header,manifest,fixture.capsule.subarray(32+oldLength)]), signature=Buffer.from(sign(null,Buffer.concat([Buffer.from('Dilla Linux sandbox capsule v1\0'),manifest]),fixture.pair.privateKey).toString('base64')+'\n');
export const fixtureComponent={...fixture,manifest,capsule,signature};
export function fixtureSandbox(version:string) {
 const base=`dilla-${version}-linux-x64.sandbox`, files=[capsule,manifest,signature];
 const assets=files.map((bytes,i)=>({name:base+['','.json','.sig'][i],size:bytes.length,sha512:fixture.hash(bytes)}));
 const descriptor={compatibilityEpoch:1 as const,minimumGeneration:1,bundle:{url:`https://github.com/dilla-chat/dilla/releases/download/desktop-v${version}/${base}`,size:capsule.length,sha512:fixture.hash(capsule)}};
 return {version,descriptor,assets,capsule,manifest,signature,keys:fixture.config.publicKeys,now:Date.now()};
}
const desktop=resolve(import.meta.dirname,'../..');
const code=`export * as cache from ${JSON.stringify(join(desktop,'src/linux-sandbox-cache.ts'))};`;
const root=await mkdtemp(join(tmpdir(),'dilla-fixture-compiled-')); process.once('exit',()=>rmSync(root,{recursive:true,force:true}));
export const fixtureRuntimePath=join(root,'runtime.mjs');
await build({stdin:{contents:code,resolveDir:desktop,loader:'ts'},outfile:fixtureRuntimePath,bundle:true,platform:'node',format:'esm',target:'node24',plugins:[{name:'compile-fixture-sandbox-trust',setup(plugin){
 plugin.onLoad({filter:/[\\/]linux-sandbox-component\.json$/},()=>({contents:JSON.stringify({...fixture.config,publishedAt:doc.publishedAt,expiresAt:doc.expiresAt}),loader:'json'}));
 if (process.platform === 'win32') plugin.onLoad({filter:/[\\/]src[\\/]linux-sandbox-cache\.ts$/},async args=>({contents:fixtureCacheSource(await readFile(args.path,'utf8'),process.platform),loader:'ts',resolveDir:join(desktop,'src')}));
}}]});
export const fixtureRuntime=await import(pathToFileURL(fixtureRuntimePath).href) as {
 cache: typeof import('../../src/linux-sandbox-cache.ts');
};
export async function fixtureSandboxCache(userData:string,version='1.0.1') {return fixtureRuntime.cache.saveSandboxSidecars(userData,fixtureSandbox(version));}
