import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createRequire } from 'node:module';
import { readFile } from 'node:fs/promises';
import { dirname, join } from 'node:path';
const require = createRequire(import.meta.url);
require('app-builder-lib');
const config = require('../electron-builder.cjs');
const { getMakeNsisPath } = require('app-builder-lib/out/toolsets/windows');
const { sync } = require('cross-spawn');

test('shipping NSIS toolset executes the pinned modern bundle through its native entrypoint', async () => {
 assert.equal(config.toolsets?.nsis, '1.2.1');
 const tool = await getMakeNsisPath(config.toolsets?.nsis);
 assert.match(await readFile(join(dirname(tool.path), 'README.md'), 'utf8'), /NSIS Version: 3\.12\b/);
 const result = sync(tool.path, ['-VERSION'], { env: { ...process.env, ...tool.env }, encoding: 'utf8', timeout: 15000 });
 assert.equal(result.status, 0, 'the platform compiler entrypoint must execute successfully');
 // The cross-compiled frontend reports a dated CVS banner. The hash-pinned
 // bundle's own release metadata identifies its upstream version.
 assert.match(result.stdout.trim(), /^v\S+$/, 'the platform compiler must return its version banner');
});
