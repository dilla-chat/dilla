import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createRequire } from 'node:module';
const require = createRequire(import.meta.url);
const config = require('../electron-builder.cjs');

test('Mac archives and DMGs use only the current native architecture; the matrix supplies both builds', async () => {
 for (const target of config.mac.target) assert.deepEqual(target.arch, [process.arch]);
 const arch = { x64: 1, arm64: 3 }[process.arch];
 await config.beforePack({ electronPlatformName: process.platform, arch });
 await assert.rejects(config.beforePack({ electronPlatformName: process.platform, arch: arch === 1 ? 3 : 1 }), /matching OS and architecture/);
});
