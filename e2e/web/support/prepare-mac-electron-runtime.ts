import { createRequire } from 'node:module';
import { prepareMacElectronRuntime } from './mac-notification-diagnostics.ts';

// Requiring the electron package resolves its installed executable without
// starting Electron. Only the guarded CI helper may modify that test runtime.
const execPath: string = createRequire(import.meta.url)('electron');
console.info('Prepared Mac notification runtime: ' + JSON.stringify(prepareMacElectronRuntime({ execPath })));
