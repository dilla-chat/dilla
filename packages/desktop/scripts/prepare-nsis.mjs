import { createRequire } from 'node:module';

const require = createRequire(import.meta.url);
require('app-builder-lib');
const config = require('../electron-builder.cjs');
const { getMakeNsisPath } = require('app-builder-lib/out/toolsets/windows');

// Resolve and finish the authenticated extraction before parallel test workers
// access electron-builder's shared compiler cache.
await getMakeNsisPath(config.toolsets?.nsis);
console.log('Pinned NSIS compiler cache prepared.');
