import { test } from 'node:test';
import assert from 'node:assert/strict';
import { generateKeyPairSync, sign, verify } from 'node:crypto';
// @ts-expect-error build tooling is JavaScript
import { appcast, signedAppcast, sparkleVersion } from '../scripts/release-feed.mjs';

test('Sparkle signed feeds authenticate the exact UTF-8 content and archive signature', () => {
 const key = generateKeyPairSync('ed25519'); const bytes = Buffer.from('native application archive');
 const release = { channel: 'beta', version: '1.2.3-beta.4', publishedAt: new Date(0).toISOString() };
 const artifact = { url: 'https://github.com/dilla-chat/dilla/releases/download/desktop-v1.2.3-beta.4/dilla-mac.zip', size: bytes.length };
 const archiveSignature = sign(null, bytes, key.privateKey).toString('base64');
 const xml = appcast(release, artifact, archiveSignature); assert.ok(xml.includes('<sparkle:version>1.2.3b4</sparkle:version>'));
 const feed = signedAppcast(xml, key.privateKey) as Buffer;
 const prefix = Buffer.from('<!-- sparkle-signatures:\n'); const end = feed.indexOf(prefix);
 const block = feed.subarray(end).toString(); const signature = /edSignature: ([A-Za-z0-9+/=]+)/.exec(block)![1];
 assert.equal(Number(/length: (\d+)/.exec(block)![1]), end);
 assert.ok(verify(null, feed.subarray(0, end), key.publicKey, Buffer.from(signature, 'base64')));
 const corrupted = Buffer.from(feed.subarray(0, end)); corrupted[70] ^= 1;
 assert.equal(verify(null, corrupted, key.publicKey, Buffer.from(signature, 'base64')), false);
 assert.ok(verify(null, bytes, key.publicKey, Buffer.from(archiveSignature, 'base64')));
 assert.equal(sparkleVersion('1.2.3'), '1.2.3'); assert.throws(() => sparkleVersion('1.2.3-beta.bad'));
});
