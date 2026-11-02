import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createRequire } from 'node:module';
const require = createRequire(import.meta.url);
const config = require('../electron-builder.cjs');

test('Mac archives and DMGs use only the current native architecture; the matrix supplies both builds', async () => {
 for (const target of config.mac.target) assert.deepEqual(target.arch, [process.arch]);
 const arch = process.arch === 'x64' ? 1 : process.arch === 'arm64' ? 3 : undefined;
 assert.notEqual(arch, undefined);
 await config.beforePack({ electronPlatformName: process.platform, arch });
 await assert.rejects(config.beforePack({ electronPlatformName: process.platform, arch: arch === 1 ? 3 : 1 }), /matching OS and architecture/);
});
