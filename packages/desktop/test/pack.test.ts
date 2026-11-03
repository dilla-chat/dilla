import { test } from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { readFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import { fileURLToPath } from 'node:url';
const require = createRequire(import.meta.url);
const config = require('../electron-builder.cjs');

test('Mac archives and DMGs use only the current native architecture; the matrix supplies both builds', async () => {
 for (const target of config.mac.target) assert.deepEqual(target.arch, [process.arch]);
 const arch = process.arch === 'x64' ? 1 : process.arch === 'arm64' ? 3 : undefined;
 assert.notEqual(arch, undefined);
 await config.beforePack({ electronPlatformName: process.platform, arch });
 await assert.rejects(config.beforePack({ electronPlatformName: process.platform, arch: arch === 1 ? 3 : 1 }), /matching OS and architecture/);
});

test('Mac signing follows the release credentials: ad-hoc without them, Developer ID and notarization with them', () => {
 const read = (env: Record<string, string>) => {
  const result = spawnSync(process.execPath, ['-e', "const c = require('./electron-builder.cjs'); console.log(JSON.stringify({ identity: c.mac.identity ?? null, notarize: c.mac.notarize, force: c.forceCodeSigning, hardened: c.mac.hardenedRuntime }))"], {
   cwd: fileURLToPath(new URL('..', import.meta.url)), encoding: 'utf8', env: { PATH: process.env.PATH, ...env },
  });
  assert.equal(result.status, 0, result.stderr);
  return JSON.parse(result.stdout);
 };
 assert.deepEqual(read({}), { identity: '-', notarize: false, force: false, hardened: true });
 assert.deepEqual(read({ CSC_LINK: 'base64', CSC_KEY_PASSWORD: 'x' }), { identity: null, notarize: false, force: true, hardened: true });
 assert.deepEqual(read({ CSC_LINK: 'base64', CSC_KEY_PASSWORD: 'x', APPLE_API_KEY: '/tmp/AuthKey.p8', APPLE_API_KEY_ID: 'ABC', APPLE_API_ISSUER: 'issuer' }).notarize, true);
 // A partial notarization credential set is not a half-notarized build: electron-builder would refuse it.
 assert.equal(read({ CSC_LINK: 'base64', APPLE_API_KEY_ID: 'ABC' }).notarize, false);
 // electron-builder reads both entitlement files from the build resources (assets) by these exact names: the app's
 // and the helper processes', each with Electron's hardened-runtime exceptions and the microphone and camera.
 assert.equal(config.directories.buildResources, 'assets');
 for (const name of ['entitlements.mac.plist', 'entitlements.mac.inherit.plist']) {
  const plist = readFileSync(new URL(`../assets/${name}`, import.meta.url), 'utf8');
  for (const key of ['com.apple.security.cs.allow-jit', 'com.apple.security.cs.allow-unsigned-executable-memory', 'com.apple.security.cs.disable-library-validation', 'com.apple.security.device.audio-input', 'com.apple.security.device.camera']) {
   assert.ok(plist.includes(`<key>${key}</key>\n    <true/>`), `${name}: ${key}`);
  }
 }
});
