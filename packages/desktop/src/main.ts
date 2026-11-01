import { app, BrowserWindow, desktopCapturer, dialog, ipcMain, Menu, nativeImage, Notification, protocol, session, shell, Tray, type IpcMainInvokeEvent } from 'electron';
import { readFile } from 'node:fs/promises';
import { existsSync } from 'node:fs';
import { join } from 'node:path';
import { randomUUID } from 'node:crypto';
import { loadBundle, bundledResponse, type Bundle } from './bundle.ts';
import { canonicalInstance, externalLink, isAppNavigation, parseInvite, partitionFor, trustedFrame, validateCommand, instanceRequestURL } from './policy.ts';
import { Preferences } from './preferences.ts';
import { linuxAutostart } from './autostart.ts';
import { createUpdater } from './updater.ts';
import { waitForPreviousProcess } from './handoff.ts';
import { NativeAudio } from './native-audio.ts';
import type { DesktopEvent, DesktopInfo } from './contract.ts';

protocol.registerSchemesAsPrivileged([{ scheme: 'dilla-internal', privileges: { standard: true, secure: true, supportFetchAPI: true } }]);
app.enableSandbox();
app.setName('Dilla');
const arg = (name: string): string | undefined => process.argv.find(value => value.startsWith(`--${name}=`))?.slice(name.length + 3);
const allowLoopback = !app.isPackaged && process.argv.includes('--allow-loopback');
if (!app.isPackaged && arg('profile')) app.setPath('userData', arg('profile')!);
const hidden = process.argv.includes('--hidden');
let quitting = false;
let askingQuit = false;
let shellReady = false;
let bundle: Bundle;
let preferences: Preferences;
let tray: Tray;
let manager: BrowserWindow | null = null;
const windows = new Map<string, BrowserWindow>();
const permissions = new Map<string, Set<string>>();
const notices = new Map<string, Notification>();
const badges = new Map<string, number>();
const audioExecutable = join(app.isPackaged ? join(process.resourcesPath, 'native') : join(__dirname, '../native-bin'), process.platform === 'win32' ? 'dilla-audio.exe' : 'dilla-audio');
const captures = new Map<string, { id: string; label: string; audio: NativeAudio; inFlight: number }>();
function stopAppAudio(instance: string): void {
  const capture = captures.get(instance); if (!capture) return;
  captures.delete(instance); capture.audio.stop();
  const win = windows.get(instance);
  if (win && !win.isDestroyed()) win.webContents.send('dilla:event', { type: 'app-audio-ended', id: capture.id } satisfies DesktopEvent);
  refreshMenu();
}
let quitProbe: { id: string; waiting: Set<string>; done(): void } | null = null;
const updater = createUpdater(state => {
  for (const win of windows.values()) if (!win.isDestroyed()) win.webContents.send('dilla:event', { type: 'update', ...state } satisfies DesktopEvent);
  refreshMenu();
});

const webPreferences = () => ({ preload: join(__dirname, 'preload.cjs'), sandbox: true, contextIsolation: true, nodeIntegration: false,
  nodeIntegrationInWorker: false, nodeIntegrationInSubFrames: false, webviewTag: false, webSecurity: true,
  allowRunningInsecureContent: false, backgroundThrottling: false, navigateOnDragDrop: false, devTools: !app.isPackaged });

function owner(event: Pick<IpcMainInvokeEvent, 'sender' | 'senderFrame'>): { instance: string; win: BrowserWindow } {
  for (const [instance, win] of windows) {
    if (event.sender === win.webContents && event.senderFrame === win.webContents.mainFrame
      && trustedFrame(event.senderFrame ? { url: event.senderFrame.url, top: event.senderFrame.parent === null } : null, instance)) return { instance, win };
  }
  throw new Error('Untrusted desktop IPC sender');
}

function focus(win: BrowserWindow): void { if (win.isMinimized()) win.restore(); win.show(); win.focus(); }

function lockWindow(win: BrowserWindow, permits: (url: string) => boolean): void {
  win.webContents.on('will-prevent-unload', () => {
    // A last-moment draft/recovery transition still cancels an unload; never leave the quit latch set.
    quitting = false;
    updater.cancelledQuit();
    for (const client of windows.values()) client.setEnabled(true);
  });
  win.webContents.on('will-attach-webview', event => { event.preventDefault(); });
  win.webContents.on('will-navigate', (event, url) => { if (!permits(url)) event.preventDefault(); });
  win.webContents.on('will-redirect', (event, url) => { if (!permits(url)) event.preventDefault(); });
  win.webContents.on('will-frame-navigate', event => { if (!event.isMainFrame || !permits(event.url)) event.preventDefault(); });
  win.webContents.setWindowOpenHandler(({ url }) => {
    const link = externalLink(url);
    if (link) void shell.openExternal(link).catch(() => {});
    return { action: 'deny' };
  });
  win.on('close', event => { if (!quitting) { event.preventDefault(); win.hide(); } });
}

async function openInstance(input: string, path = '/', show = true): Promise<BrowserWindow> {
  if (askingQuit || quitting) throw new Error('Dilla is preparing to quit; open this instance after restarting');
  const instance = canonicalInstance(input, allowLoopback);
  const existing = windows.get(instance);
  if (existing && !existing.isDestroyed()) {
    if (path !== '/') {
      if (updater.protected()) throw new Error('Finish your call or save your draft before opening this invite');
      await existing.loadURL(new URL(path, instance).href);
    }
    if (show) focus(existing);
    return existing;
  }
  const ses = session.fromPartition(partitionFor(instance));
  const win = new BrowserWindow({ width: 1240, height: 820, minWidth: 640, minHeight: 480, show: false, title: 'Dilla', webPreferences: { ...webPreferences(), session: ses } });
  windows.set(instance, win);
  updater.expectGuard(instance);
  const origin = new URL(instance).origin;
  const handler = (request: Request): Response | Promise<Response> => {
    if (new URL(request.url).origin !== origin) return ses.fetch(request, { bypassCustomProtocolHandlers: true });
    const bundled = bundledResponse(bundle, request, instance);
    if (bundled) return bundled;
    const target = instanceRequestURL(request.url, instance);
    const forwarded = target === request.url ? request : new Request(target, request);
    return ses.fetch(forwarded, { bypassCustomProtocolHandlers: true });
  };
  // One session and protocol handler per complete canonical URL, not a host-only partition.
  await ses.protocol.handle(new URL(instance).protocol.slice(0, -1), handler);
  ses.webRequest.onBeforeRequest((details, callback) => {
    const isCode = ['script', 'stylesheet', 'font'].includes(details.resourceType);
    const url = new URL(details.url);
    const cancel = details.resourceType === 'subFrame' || (isCode && (url.origin !== origin || !url.pathname.startsWith('/assets/')))
      || (details.resourceType === 'mainFrame' && !isAppNavigation(details.url, instance));
    callback({ cancel });
  });
  const allowed = permissions.get(instance) ?? new Set<string>();
  permissions.set(instance, allowed);
  ses.setPermissionCheckHandler((contents, permission, requestingOrigin) => {
    return contents === win.webContents && requestingOrigin === origin && allowed.has(permission)
      && trustedFrame({ url: contents.getURL(), top: true }, instance);
  });
  ses.setPermissionRequestHandler((contents, permission, callback, details) => {
    if (contents !== win.webContents || details.isMainFrame === false || !trustedFrame({ url: details.requestingUrl, top: true }, instance)
      || !['media', 'notifications', 'speaker-selection'].includes(permission)) { callback(false); return; }
    if (allowed.has(permission)) { callback(true); return; }
    const label = permission === 'media' ? 'microphone or camera' : permission === 'notifications' ? 'notifications' : 'audio output selection';
    void dialog.showMessageBox(win, { type: 'question', title: 'Dilla permission', message: `Allow ${new URL(instance).host} to use ${label}?`, buttons: ['Deny', 'Allow'], defaultId: 0, cancelId: 0 })
      .then(result => { if (result.response === 1) allowed.add(permission); callback(result.response === 1); }).catch(() => callback(false));
  });
  ses.setDisplayMediaRequestHandler((request, callback) => {
    if (!request.userGesture || request.frame !== win.webContents.mainFrame || request.securityOrigin !== origin) { callback(null); return; }
    // The Linux capturer delegates selection to xdg-desktop-portal on Wayland. No whole-system audio fallback.
    void desktopCapturer.getSources({ types: ['screen', 'window'], thumbnailSize: { width: 0, height: 0 } }).then(async sources => {
      if (sources.length === 0) { callback(null); return; }
      const choice = await dialog.showMessageBox(win, { title: 'Share your screen', message: 'Choose what to share', buttons: ['Cancel', ...sources.map(source => source.name)], cancelId: 0, defaultId: 0 });
      if (choice.response === 0 || !sources[choice.response - 1]) callback(null);
      else callback({ video: sources[choice.response - 1] });
    }).catch(() => callback(null));
  }, { useSystemPicker: true });
  ses.on('will-download', (event, item) => { if (!item.getURL().startsWith(`blob:${origin}/`)) event.preventDefault(); });
  lockWindow(win, url => isAppNavigation(url, instance));
  win.webContents.on('render-process-gone', () => { updater.expectGuard(instance); stopAppAudio(instance); });
  win.webContents.on('did-start-navigation', (_event, _url, inPlace, mainFrame) => { if (mainFrame && !inPlace) { updater.expectGuard(instance); stopAppAudio(instance); } });
  win.on('closed', () => { windows.delete(instance); updater.removeGuard(instance); });
  await win.loadURL(new URL(path, origin).href);
  if (show) focus(win);
  refreshMenu();
  return win;
}

async function confirmInstance(input: string): Promise<boolean> {
  const instance = canonicalInstance(input, allowLoopback);
  if (preferences.instances.includes(instance)) return true;
  const result = await dialog.showMessageBox({ type: 'question', title: 'Connect to a Dilla instance', message: `Connect to ${new URL(instance).host}?`, detail: `Instance: ${instance}\nOnly continue if you recognize this address.`, buttons: ['Cancel', 'Connect'], defaultId: 0, cancelId: 0, noLink: true });
  if (result.response !== 1) return false;
  await preferences.addInstance(instance);
  return true;
}

async function openInvite(input: string): Promise<void> {
  if (askingQuit || quitting) return;
  try {
    const invite = parseInvite(input);
    if (await confirmInstance(invite.instance)) await openInstance(invite.instance, `/welcome?invite=${encodeURIComponent(invite.code)}`);
  } catch (error) { dialog.showErrorBox('Could not open this invite', error instanceof Error ? error.message : 'Invalid Dilla invite'); }
}

async function showManager(): Promise<void> {
  if (!shellReady || askingQuit || quitting) return;
  if (manager && !manager.isDestroyed()) { focus(manager); return; }
  manager = new BrowserWindow({ width: 720, height: 760, minWidth: 480, title: 'Dilla — your instances', webPreferences: webPreferences() });
  lockWindow(manager, url => url === 'dilla-internal://setup/');
  manager.on('closed', () => { manager = null; });
  await manager.loadURL('dilla-internal://setup/');
}

async function setAutostart(enabled: boolean): Promise<void> {
  if (!app.isPackaged) throw new Error('Autostart is available in installed builds');
  if (process.platform === 'linux') await linuxAutostart(app.getPath('appData'), process.env.APPIMAGE ?? process.execPath, enabled);
  else app.setLoginItemSettings({ openAtLogin: enabled, path: process.execPath, args: ['--hidden'] });
  await preferences.setAutostart(enabled);
  refreshMenu();
}

async function requestQuit(restart = false): Promise<void> {
  if (askingQuit || quitting) return;
  askingQuit = true;
  try {
    // Ask every live renderer for a fresh snapshot. A hung, crashed or uninitialised client blocks installation.
    await new Promise<void>(done => {
      const id = randomUUID();
      const waiting = new Set(windows.keys());
      const timer = setTimeout(() => { quitProbe = null; done(); }, 3000);
      quitProbe = { id, waiting, done: () => { clearTimeout(timer); quitProbe = null; done(); } };
      for (const [instance, win] of windows) {
        updater.expectGuard(instance);
        if (!win.isDestroyed()) win.webContents.send('dilla:event', { type: 'prepare-quit', id } satisfies DesktopEvent);
      }
      if (waiting.size === 0) quitProbe.done();
    });
    if (updater.protected()) {
      await dialog.showMessageBox({ type: 'info', title: 'Dilla is still in use', message: 'Finish your call and save your drafts before quitting or updating.', detail: 'You can close the window to keep Dilla running in the tray.', buttons: ['Keep Dilla open'] });
      return;
    }
    if (restart && updater.state.phase !== 'ready') return;
    const ready = updater.state.phase === 'ready';
    for (const win of windows.values()) win.setEnabled(false);
    const installed = await updater.quit(true);
    if (ready && !installed) {
      for (const win of windows.values()) win.setEnabled(true);
      dialog.showErrorBox('Dilla update failed', updater.state.message ?? 'The working version is still available. Check for updates to retry.');
      return;
    }
    quitting = true; for (const instance of captures.keys()) stopAppAudio(instance); app.quit();
  } finally { askingQuit = false; }
}

function refreshMenu(): void {
  if (!tray || !preferences) return;
  const count = [...badges.values()].reduce((a, b) => a + b, 0);
  app.setBadgeCount(count);
  tray.setToolTip(updater.state.phase === 'error' ? 'Dilla — update failed; open the tray to retry' : count ? `Dilla — ${count} unread` : 'Dilla');
  const template: Electron.MenuItemConstructorOptions[] = [
    ...[...captures].map(([instance, capture]) => ({ label: `Sharing application sound: ${capture.label} — stop`, click: () => stopAppAudio(instance) })),
    { label: 'Your instances…', click: () => { void showManager(); } },
    ...[...windows].map(([instance, win]) => ({ label: new URL(instance).host + (new URL(instance).pathname === '/' ? '' : new URL(instance).pathname), click: () => focus(win) })),
    { type: 'separator' },
    { label: 'Start Dilla at login', type: 'checkbox', checked: preferences.autostart, click: item => { void setAutostart(item.checked).catch(error => dialog.showErrorBox('Could not change startup preference', String(error))); } },
    { label: updater.state.phase === 'checking' ? 'Checking for updates…' : updater.state.phase === 'error' ? 'Update failed — retry' : 'Check for updates', enabled: !['checking', 'downloading', 'installing'].includes(updater.state.phase), click: () => { void updater.check().then(() => {
      if (updater.state.phase === 'error') dialog.showErrorBox('Could not update Dilla', updater.state.message ?? 'Try again');
      else if (updater.state.phase === 'idle') void dialog.showMessageBox({ message: 'Dilla is up to date.', buttons: ['OK'] });
    }); } },
    ...(updater.state.phase === 'ready' ? [{ label: 'Restart to update', click: () => { void requestQuit(true); } }] : []),
    { type: 'separator' }, { label: 'Quit Dilla', click: () => { void requestQuit(); } },
  ];
  tray.setContextMenu(Menu.buildFromTemplate(template));
  Menu.setApplicationMenu(Menu.buildFromTemplate([
    { label: 'Dilla', submenu: template }, { role: 'editMenu' },
    { label: 'View', submenu: [{ role: 'resetZoom' }, { role: 'zoomIn' }, { role: 'zoomOut' }, { type: 'separator' }, { role: 'togglefullscreen' }] },
    { role: 'windowMenu' },
  ]));
}

ipcMain.handle('dilla:info', event => {
  const { instance } = owner(event);
  return { version: app.getVersion(), instance, capabilities: { applicationAudio: existsSync(audioExecutable), browserAuth: false, notifications: Notification.isSupported(), screenCapture: true } } satisfies DesktopInfo;
});
ipcMain.handle('dilla:app-audio', async (event, ...args: unknown[]) => {
  const { instance, win } = owner(event);
  if (args.length || !existsSync(audioExecutable) || askingQuit || quitting) throw new Error('Application audio is unavailable');
  const audio = new NativeAudio(audioExecutable);
  const sources = await audio.sources();
  if (sources.length === 0) { await dialog.showMessageBox(win, { message: 'No playback applications are available. Start audio in the application, then try again.', buttons: ['OK'] }); return null; }
  const choice = await dialog.showMessageBox(win, { title: 'Share application sound', message: 'Choose the application others will hear', detail: 'Dilla’s own call audio is excluded.', buttons: ['Cancel', ...sources.map(source => source.label)], defaultId: 0, cancelId: 0 });
  if (!choice.response || !sources[choice.response - 1] || win.isDestroyed() || askingQuit || quitting) return null;
  stopAppAudio(instance);
  const source = sources[choice.response - 1], id = randomUUID();
  captures.set(instance, { id, label: source.label, audio, inFlight: 0 });
  audio.start(source, samples => {
    const active = captures.get(instance);
    if (active?.id === id && active.inFlight < 8 && !win.isDestroyed()) { active.inFlight++; win.webContents.send('dilla:pcm', id, samples); }
  }, () => stopAppAudio(instance));
  refreshMenu(); return { id, label: source.label };
});
ipcMain.on('dilla:pcm-ack', (event, id: unknown) => {
  try { const { instance } = owner(event); const capture = captures.get(instance); if (capture && id === capture.id) capture.inFlight = Math.max(0, capture.inFlight - 1); }
  catch { /* Untrusted frames cannot acknowledge or control capture. */ }
});
ipcMain.handle('dilla:command', async (event, value: unknown) => {
  const { instance, win } = owner(event);
  const command = validateCommand(value);
  if (command.type === 'guard') updater.setGuard(instance, command);
  else if (command.type === 'quit-ack') {
    if (quitProbe?.id !== command.id || !quitProbe.waiting.has(instance)) throw new Error('Expired quit acknowledgment');
    updater.setGuard(instance, command);
    quitProbe.waiting.delete(instance);
    if (quitProbe.waiting.size === 0) quitProbe.done();
  }
  else if (command.type === 'badge') { badges.set(instance, command.count); refreshMenu(); }
  else if (command.type === 'stop-app-audio') { if (captures.get(instance)?.id === command.id) stopAppAudio(instance); }
  else if (command.type === 'autostart') await setAutostart(command.enabled);
  else if (command.type === 'check-update') await updater.check();
  else if (command.type === 'restart-update') await requestQuit(true);
  else if (command.type === 'dismiss-notification') { notices.get(`${instance}:${command.id}`)?.close(); notices.delete(`${instance}:${command.id}`); }
  else if (command.type === 'notify') {
    if (!Notification.isSupported() || !permissions.get(instance)?.has('notifications')) return;
    const key = `${instance}:${command.id}`;
    const previous = notices.get(key);
    if (notices.size >= 64 && !notices.has(key)) return;
    const note = new Notification({ title: command.title, body: command.body, silent: false });
    note.on('click', () => { focus(win); win.webContents.send('dilla:event', { type: 'notification-click', id: command.id } satisfies DesktopEvent); });
    note.on('close', () => {
      if (notices.get(key) !== note) return;
      notices.delete(key);
      if (!win.isDestroyed()) win.webContents.send('dilla:event', { type: 'notification-close', id: command.id } satisfies DesktopEvent);
    });
    notices.set(key, note); previous?.close(); note.show();
  }
});
ipcMain.handle('dilla:manager', async (event, value: unknown) => {
  if (!manager || event.sender !== manager.webContents || event.senderFrame !== manager.webContents.mainFrame || event.senderFrame?.url !== 'dilla-internal://setup/') throw new Error('Untrusted instance chooser');
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error('Invalid manager request');
  const command = value as Record<string, unknown>;
  if (command.type === 'list' && Object.keys(command).length === 1) return preferences.instances;
  if (command.type === 'connect' && Object.keys(command).length === 2 && typeof command.instance === 'string') {
    const instance = canonicalInstance(command.instance, allowLoopback);
    if (await confirmInstance(instance)) await openInstance(instance);
    return;
  }
  throw new Error('Invalid manager request');
});

const pendingInvites: string[] = [];
app.on('open-url', (event, url) => { event.preventDefault(); if (preferences) void openInvite(url); else pendingInvites.push(url); });
const previousProcess = process.env.DILLA_UPDATE_PREVIOUS_PID;
delete process.env.DILLA_UPDATE_PREVIOUS_PID;
void waitForPreviousProcess(previousProcess).then(() => {
  if (!app.requestSingleInstanceLock()) { quitting = true; app.quit(); }
  else {
    app.on('second-instance', (_event, argv) => {
      const link = argv.find(value => value.startsWith('dilla://'));
      if (!shellReady) { if (link) pendingInvites.push(link); return; }
      if (link) void openInvite(link);
      else void showManager();
    });
    app.on('before-quit', event => { if (!quitting) { event.preventDefault(); void requestQuit(); } });
    app.on('window-all-closed', () => { /* Tray owns the application's lifetime. */ });
    app.on('activate', () => { const win = [...windows.values()][0]; if (win) focus(win); else void showManager(); });
    void app.whenReady().then(async () => {
      bundle = await loadBundle(!app.isPackaged && process.argv.includes('--media-validation') ? join(__dirname, '../validation-dist') : join(__dirname, 'client'));
      preferences = new Preferences(app.getPath('userData'), allowLoopback);
      await preferences.load();
      await protocol.handle('dilla-internal', async request => {
        const url = new URL(request.url);
        const names: Record<string, string> = { '/': 'manager.html', '/manager.js': 'manager.js', '/manager.css': 'manager.css' };
        if (url.hostname !== 'setup' || !names[url.pathname] || request.method !== 'GET') return new Response('Not found', { status: 404 });
        return new Response(new Uint8Array(await readFile(join(__dirname, names[url.pathname]))), { headers: {
          'content-type': url.pathname.endsWith('.js') ? 'text/javascript' : url.pathname.endsWith('.css') ? 'text/css' : 'text/html',
          'content-security-policy': "default-src 'none'; script-src 'self'; style-src 'self'; base-uri 'none'; form-action 'none'; frame-src 'none'; frame-ancestors 'none'",
          'x-content-type-options': 'nosniff',
        } });
      });
      session.defaultSession.setPermissionRequestHandler((_contents, _permission, callback) => callback(false));
      session.defaultSession.setPermissionCheckHandler(() => false);
      tray = new Tray(nativeImage.createFromPath(join(__dirname, 'tray.png')));
      shellReady = true;
      tray.on('click', () => { const win = [...windows.values()][0]; if (win) focus(win); else void showManager(); });
      if (app.isPackaged) app.setAsDefaultProtocolClient('dilla');
      const initial = arg('instance');
      if (initial) {
        const instance = canonicalInstance(initial, allowLoopback);
        if (allowLoopback && new URL(instance).protocol === 'http:') await preferences.addInstance(instance);
        if (await confirmInstance(instance)) await openInstance(instance, '/', !hidden);
      }
      for (const instance of preferences.instances) if (!windows.has(instance)) await openInstance(instance, '/', !hidden);
      for (const link of [...pendingInvites, ...process.argv.filter(value => value.startsWith('dilla://'))]) await openInvite(link);
      if (windows.size === 0) await showManager();
      refreshMenu();
      if (app.isPackaged) {
        void updater.check();
        setInterval(() => { void updater.check(); }, 4 * 60 * 60 * 1000).unref();
      }
    }).catch(error => { dialog.showErrorBox('Dilla could not start', String(error)); quitting = true; app.quit(); });
  }
}).catch(error => { dialog.showErrorBox('Dilla could not finish restarting', String(error)); quitting = true; app.quit(); });
