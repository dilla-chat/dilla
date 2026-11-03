import { test } from 'node:test';
import assert from 'node:assert/strict';
import { generateKeyPairSync, sign } from 'node:crypto';
import { mkdtemp, writeFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { sandboxReleaseFixture } from './support/linux-sandbox-release-fixture.ts';
import { verifyRelease } from '../src/update.ts';
import { verifyLinuxSandboxRelease } from '../src/linux-sandbox-release.ts';
// @ts-expect-error fixture helper is JavaScript
import { fixtureSandboxFeed } from './support/linux-sandbox-fixture-feed.mjs';
test('installed fixture factory binds actual capsule sidecars into accepted signed metadata',async()=>{
 const dir=await mkdtemp(join(tmpdir(),'dilla-feed-fixture-')), f=sandboxReleaseFixture();
 try {const image=join(dir,'fixture-1.2.3.AppImage'); await writeFile(image.replace('.AppImage','.sandbox'),f.capsule); await writeFile(image.replace('.AppImage','.sandbox.json'),f.manifest);
  const output=await fixtureSandboxFeed(image,'1.2.3',f.pair,f.config,Date.parse('2026-11-02T00:00:00Z')); const pair=generateKeyPairSync('ed25519');
  const doc={format:1,version:'1.2.3',channel:'stable',publishedAt:'2026-11-01T00:00:00Z',expiresAt:'2026-12-01T00:00:00Z',assets:output.assets,artifacts:[{platform:'linux',arch:'x64',url:'https://github.com/jonasthim/dilla/releases/download/desktop-v1.2.3/dilla-1.2.3-linux-x64.AppImage',size:1,sha512:Buffer.alloc(64).toString('base64'),linuxSandbox:output.linuxSandbox}]};
  const bytes=Buffer.from(JSON.stringify(doc)), sig=sign(null,bytes,pair.privateKey).toString('base64'); assert.equal(verifyRelease(bytes,sig,{publicKeys:[pair.publicKey.export({format:'pem',type:'spki'}).toString()],channel:'stable',platform:'linux',arch:'x64',currentVersion:'1.0.0',now:Date.parse('2026-11-02T00:00:00Z')}).version,'1.2.3');
  assert.equal(verifyLinuxSandboxRelease({capsule:f.capsule,manifest:f.manifest,signature:output.signature.toString(),keys:[output.fixturePublicKey],now:Date.parse('2026-11-02T00:00:00Z'),minimumGeneration:1}).generation,1);
  await assert.rejects(fixtureSandboxFeed(image,'1.2.3',generateKeyPairSync('ed25519'),f.config,Date.parse('2026-11-02T00:00:00Z')));
  await writeFile(image.replace('.AppImage','.sandbox.json'),Buffer.from('wrong')); await assert.rejects(fixtureSandboxFeed(image,'1.2.3',f.pair,f.config,Date.parse('2026-11-02T00:00:00Z')));
 } finally {await rm(dir,{recursive:true,force:true});}
});
