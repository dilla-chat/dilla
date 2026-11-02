import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { transform } from 'esbuild';
import vm from 'node:vm';
import { desktopReleasePage } from '../src/update.ts';

test('the actual tray availability action opens only the validated immutable release tag and offers no restart', async () => {
 const main = await readFile(new URL('../src/main.ts', import.meta.url), 'utf8');
 const body = main.slice(main.indexOf('function refreshMenu(): void {'), main.indexOf("\nipcMain.handle('dilla:info'", main.indexOf('function refreshMenu(): void {')));
 const opened: string[] = [], menus: any[][] = [];
 const updater = { state: { phase: 'available', version: '0.1.0-beta.10' }, check: async () => {} };
 const context = vm.createContext({ updater, desktopReleasePage, preferences: { autostart: false }, badges: new Map(), captures: new Map(), ringActions: new Map(), windows: new Map(),
  app: { setBadgeCount() {} }, tray: { setToolTip() {}, setContextMenu() {} },
  Menu: { buildFromTemplate: (template: any[]) => { menus.push(template); return template; }, setApplicationMenu() {} },
  shell: { openExternal: async (url: string) => { opened.push(url); } },
 });
 vm.runInContext((await transform(body + '\nrefreshMenu();', { loader: 'ts' })).code, context);
 const update = menus[0].find(item => item.label === 'Update using your package manager');
 assert.ok(update); assert.equal(menus[0].some(item => item.label === 'Restart to update'), false);
 update.click(); assert.deepEqual(opened, ['https://github.com/jonasthim/dilla/releases/tag/desktop-v0.1.0-beta.10']);
 updater.state.version = 'https://evil.test/'; assert.throws(() => update.click(), /version/);
 assert.equal(opened.length, 1);
});

test('native autostart uses the fixed package entry even when APPIMAGE is spoofed', async () => {
 const main = await readFile(new URL('../src/main.ts', import.meta.url), 'utf8');
 const body = main.slice(main.indexOf('async function setAutostart('), main.indexOf('\nasync function requestQuit('));
 const paths: string[] = [], preferences: boolean[] = [];
 const context = vm.createContext({
  app: { isPackaged: true, getPath: () => '/profile/appdata' },
  process: { platform: 'linux', execPath: '/usr/lib/dilla/native/app/dilla-desktop', env: { APPIMAGE: '/home/user/spoofed.AppImage' } },
  LINUX_INSTALL_KIND: 'system-package', linuxAutostart: async (_appData: string, executable: string) => { paths.push(executable); },
  preferences: { setAutostart: async (enabled: boolean) => { preferences.push(enabled); } }, refreshMenu() {},
 });
 await vm.runInContext((await transform(body + '\nsetAutostart(true);', { loader: 'ts' })).code, context);
 assert.deepEqual(paths, ['/usr/bin/dilla-desktop']); assert.deepEqual(preferences, [true]);
});
