import { windowShape, type WindowMode } from './window-layout.ts';
import { windowAppearance } from './window-appearance.ts';
import { browserAuth } from './browser-auth.ts';
import { app, BrowserWindow, dialog, ipcMain, Menu, nativeImage, nativeTheme, Notification, protocol, screen, session, shell, Tray, type IpcMainInvokeEvent } from 'electron';
import { readFile } from 'node:fs/promises';
import { existsSync } from 'node:fs';
import { join } from 'node:path';
import { randomUUID } from 'node:crypto';
import { loadBundle, bundledResponse, type Bundle } from './bundle.ts';
import { canonicalInstance, externalLink, isAppNavigation, parseInvite, partitionFor, trustedFrame, validateCommand, instanceRequestURL, permissionOriginMatches } from './policy.ts';
import { Preferences } from './preferences.ts';
import { fetchInstanceData } from './request-forwarding.ts';
import { linuxAutostart } from './autostart.ts';
import { createUpdater } from './updater.ts';
import { LINUX_INSTALL_KIND, assertLinuxSelfUpdate } from './linux-build.ts';
import { resolveLinuxInstallation } from './linux-installation.ts';
import { desktopReleasePage } from './update.ts';
import { waitForPreviousProcess } from './handoff.ts';
import { NativeAudio } from './native-audio.ts';
import { ScreenCaptureBroker } from './screen-capture.ts';
import { captureOptions } from './screen-capture-contract.ts';
import type { DesktopEvent, DesktopInfo } from './contract.ts';

import { RELEASE_PUBLIC_KEYS } from './update-keys.ts';
import { windowsRegistrationWarning } from './windows-diagnostic.ts';
import { currentWindowsInstallation } from './windows-installation.ts';
import { bootstrapNonce,runWindowsBootstrap,verifyRestoredWindowsStartup,watchdogNonce, watchdogRecovery, runUpdateWatchdog, takeStartupLaunch, resumeInterruptedUpdate, UpdateStartupHeld, verifyRestoredLaunch, waitForRegisteredStartup, waitForStartupActivation } from './update-runtime.ts';
import type { InstalledUpdatePlan } from './update-plan.ts';
import { pendingStartupCommand } from './update-startup-ui.ts';
import { pendingInstalledUpdate } from './update-state.ts';
import { StartupUsability } from './update-usability.ts';
import { StartupActivationGate } from './startup-activation-gate.ts';
import { startupHealthFile, reportStartupHealth, withdrawStartupHealth } from './update-health.ts';

// Role selection and health-environment consumption precede ordinary app boot,
// single-instance acquisition, renderers, tray and unrelated process launches.
protocol.registerSchemesAsPrivileged([{ scheme: 'dilla-internal', privileges: { standard: true, secure: true, supportFetchAPI: true } }]);
app.enableSandbox();
const linuxApplicationUpdates = process.platform !== 'linux' || LINUX_INSTALL_KIND === 'appimage';
void initializeDesktop().catch(error => { console.error('Dilla could not initialize:', error); app.exit(1); });

async function initializeDesktop(): Promise<void> {
 if (process.platform === 'linux') {
  if (LINUX_INSTALL_KIND !== 'unknown') await resolveLinuxInstallation({
   installKind: LINUX_INSTALL_KIND, version: app.getVersion(), channel: app.getVersion().includes('-beta.') ? 'beta' : 'stable', arch: process.arch,
   executable: process.execPath, resourcesPath: process.resourcesPath,
  });
  const installationRole = process.argv.some(value => value.startsWith('--update-') || value.startsWith('--sandbox-') || value === '--setup-sandbox' || value.startsWith('--setup-sandbox='))
   || ['DILLA_UPDATE_STARTUP_NONCE', 'DILLA_UPDATE_PREVIOUS_PID', 'DILLA_UPDATE_RECOVERED_NONCE'].some(key => process.env[key] !== undefined);
  if (installationRole) assertLinuxSelfUpdate();
 }
 app.setName('Dilla');
try {
 const startupLaunch = takeStartupLaunch(process.env);
 const nonce = watchdogNonce(process.argv), recovery = watchdogRecovery(process.argv), bootstrap=bootstrapNonce(process.argv);
 if(bootstrap){
  if(!app.isPackaged||process.platform!=='win32'||startupLaunch.nonce||startupLaunch.recoveredNonce||startupLaunch.previousPid)throw new Error('Windows bootstrap requires its installed old program');
  void runWindowsBootstrap(app.getPath('userData'),process.execPath,bootstrap,{publicKeys:RELEASE_PUBLIC_KEYS,platform:'win32',arch:process.arch,version:app.getVersion(),channel:app.getVersion().includes('-beta.')?'beta':'stable'},()=>app.requestSingleInstanceLock()).then(code=>app.exit(code),error=>{console.error('Dilla Windows bootstrap failed:',error);app.exit(1);});
 } else if (nonce) {
  if (!app.isPackaged || (process.platform !== 'linux' && process.platform !== 'win32')) throw new Error('Update watchdog requires an installed app');
  const platform = process.platform;
  void runUpdateWatchdog(app.getPath('userData'), nonce, { publicKeys: RELEASE_PUBLIC_KEYS, platform, arch: process.arch,
   version: app.getVersion(), channel: app.getVersion().includes('-beta.') ? 'beta' : 'stable',
   executable: platform === 'linux' ? process.env.APPIMAGE! : process.execPath }, startupLaunch.previousPid, recovery).then(() => app.exit(0), error => { console.error('Dilla update recovery failed:', error); app.exit(1); });
 } else {
  void (async () => {
   let recoveryError: string | undefined;
   let activationPlan: InstalledUpdatePlan | undefined;
   let restoredPlan:InstalledUpdatePlan|undefined;
   if (startupLaunch.nonce) {
    if (!app.isPackaged || (process.platform !== 'linux' && process.platform !== 'win32')) throw new Error('Updated startup requires an installed app');
    activationPlan = await waitForRegisteredStartup(app.getPath('userData'), process.platform === 'linux' ? process.env.APPIMAGE! : process.execPath, startupLaunch.nonce, process.pid,
     { publicKeys: RELEASE_PUBLIC_KEYS, platform: process.platform, arch: process.arch, version: app.getVersion(), channel: app.getVersion().includes('-beta.') ? 'beta' : 'stable' });
   }
   if (startupLaunch.recoveredNonce) {
    if (!app.isPackaged || (process.platform !== 'linux' && process.platform !== 'win32')) throw new Error('Restored update requires an installed app');
    restoredPlan=await verifyRestoredLaunch(app.getPath('userData'), process.platform === 'linux' ? process.env.APPIMAGE! : process.execPath, startupLaunch.recoveredNonce,
     { publicKeys: RELEASE_PUBLIC_KEYS, platform: process.platform, arch: process.arch, version: app.getVersion(), channel: app.getVersion().includes('-beta.') ? 'beta' : 'stable' });
    recoveryError = 'The update did not finish. The working version and your device data are retained. Check for updates to retry.';
   }
   startDesktop(startupLaunch, recoveryError, activationPlan,restoredPlan);
  })().catch(error => { console.error('Dilla could not start:', error); app.exit(1); });
 }
} catch (error) { console.error('Dilla could not initialize:', error); app.exit(1); }
}

function startDesktop(startupLaunch: { nonce?: string; previousPid?: string }, recoveryError?: string, activationPlan?: InstalledUpdatePlan,restoredPlan?:InstalledUpdatePlan): void {
app.setName('Dilla');
if (process.platform === 'linux') app.setDesktopName('dev.dilla.desktop.desktop');
nativeTheme.themeSource = 'dark';
if (process.platform === 'win32') app.setAppUserModelId('dev.dilla.desktop');
const arg = (name: string): string | undefined => process.argv.find(value => value.startsWith(`--${name}=`))?.slice(name.length + 3);
const allowLoopback = !app.isPackaged && process.argv.includes('--allow-loopback');
if (!app.isPackaged && arg('profile')) app.setPath('userData', arg('profile')!);
const hidden = process.argv.includes('--hidden');
let quitting = false;
let askingQuit = false;
let shellReady = false;
let startupActivated = activationPlan === undefined;
let windowsLauncher=activationPlan?.windows?.launcher??restoredPlan?.windows?.launcher;
const startupGate = new StartupActivationGate(startupActivated);
let bundle: Bundle;
let preferences: Preferences;
let tray: Tray;
let manager: BrowserWindow | null = null;
const windows = new Map<string, BrowserWindow>();
const windowModes = new Map<string, WindowMode>();
const workspaceBounds = new WeakMap<BrowserWindow, Electron.Rectangle>();
const permissions = new Map<string, Set<string>>();
const notices = new Map<string, Notification>();
const ringActions = new Map<string, { title: string; run(action: 'answer' | 'decline'): void }>();
const noticeKey = (instance: string, id: string) => `${partitionFor(instance)}:${id}`;
function closeNotices(instance: string): void {
 const prefix = partitionFor(instance) + ':';
 for (const [key, note] of notices) if (key.startsWith(prefix)) { ringActions.delete(key); notices.delete(key); note.close(); }
 refreshMenu();
}
const badges = new Map<string, number>();
const audioExecutable = join(app.isPackaged ? join(process.resourcesPath, 'native') : join(__dirname, '../native-bin'), process.platform === 'win32' ? 'dilla-audio.exe' : 'dilla-audio');
const captures = new Map<string, { id: string; label: string; audio: NativeAudio; inFlight: number; dispose(): void }>();
const audioSelections = new Map<string, AbortController>();
const screenCapture = new ScreenCaptureBroker({ directory: __dirname, onEnded: (instance, id) => {
 const win = windows.get(instance);
 if (win && !win.isDestroyed() && !win.webContents.isDestroyed()) win.webContents.send('dilla:event', { type: 'screen-capture-ended', id } satisfies DesktopEvent);
} });
function stopAppAudio(instance: string): void {
  audioSelections.get(instance)?.abort();
  stopActiveAppAudio(instance);
}
function stopActiveAppAudio(instance: string, id?: string): void {
  const capture = captures.get(instance); if (!capture || (id !== undefined && capture.id !== id)) return;
  captures.delete(instance); capture.dispose(); capture.audio.stop();
  const win = windows.get(instance);
  if (win && !win.isDestroyed() && !win.webContents.isDestroyed()) win.webContents.send('dilla:event', { type: 'app-audio-ended', id: capture.id } satisfies DesktopEvent);
  refreshMenu();
}
const startupUsability = new StartupUsability(async usable => {
 if (!startupLaunch.nonce) return;
 if (usable) await reportStartupHealth(app.getPath('userData'), app.getVersion(), process.pid,
  await startupHealthFile(app.getPath('userData'), startupLaunch.nonce), startupLaunch.nonce);
 else await withdrawStartupHealth(app.getPath('userData'), startupLaunch.nonce);
});
let quitProbe: { id: string; waiting: Set<string>; done(): void } | null = null;
const updater = createUpdater(state => {
  for (const win of windows.values()) if (!win.isDestroyed()) win.webContents.send('dilla:event', { type: 'update', ...state } satisfies DesktopEvent);
  refreshMenu();
}, () => {
  quitting = false; askingQuit = false;
  void (async () => {
    for (const instance of preferences.instances) await openInstance(instance);
    if (windows.size === 0) await showManager();
    dialog.showErrorBox('Dilla update failed', updater.state.message ?? 'The working version is still available. Check for updates to retry.');
  })().catch(error => { dialog.showErrorBox('Dilla could not reopen', String(error)); });
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

const windowChrome = () => ({ icon: process.platform === 'linux' ? nativeImage.createFromPath(join(__dirname, 'icon.png')).resize({ width: 128, height: 128 }) : join(__dirname, 'icon.png'), titleBarStyle: 'hidden' as const, ...windowAppearance('signal'), trafficLightPosition: { x: 12, y: 10 }, autoHideMenuBar: true });
function showWindowMenu(win: BrowserWindow): void { Menu.getApplicationMenu()?.popup({ window: win, x: process.platform === 'darwin' ? 80 : 8, y: 32 }); }

function setWindowMode(instance: string, win: BrowserWindow, mode: WindowMode, height: number | null): void {
  const previous = windowModes.get(instance) ?? 'starting';
  // A transient reconnect/error must not collapse an existing workspace.
  if (previous === 'workspace' && mode === 'starting') return;
  if (previous === 'workspace' && mode !== 'workspace' && updater.protected()) return;
  if (previous === mode && (mode !== 'authentication' || height === null)) return;
  if (previous === 'workspace') workspaceBounds.set(win, win.getNormalBounds());
  const shape = windowShape(mode, height, screen.getDisplayMatching(win.getBounds()).workAreaSize.height);
  if (win.isMaximized() && mode !== 'workspace') win.unmaximize();
  // Toggling resizability restores native size constraints on Linux; apply
  // the mode minimum afterwards so the compact dialog minimum cannot survive.
  win.setResizable(shape.resizable); win.setMinimumSize(shape.minWidth, shape.minHeight);
  if (process.platform !== 'linux') win.setMaximizable(shape.resizable);
  if (process.platform === 'darwin') { win.setWindowButtonVisibility(mode === 'workspace'); if (mode === 'workspace') win.setWindowButtonPosition({ x: 12, y: 16 }); }
  const saved = mode === 'workspace' ? workspaceBounds.get(win) : undefined;
  if (saved) win.setBounds(saved);
  else if (win.getContentSize()[0] !== shape.width || win.getContentSize()[1] !== shape.height) {
    win.setContentSize(shape.width, shape.height);
    if (previous !== mode) win.center();
  }
  windowModes.set(instance, mode);
}

function focus(win: BrowserWindow): void { if (!startupActivated) return; if (win.isMinimized()) win.restore(); win.show(); win.focus(); }

function lockWindow(win: BrowserWindow, permits: (url: string) => boolean): void {
  win.webContents.on('will-prevent-unload', event => {
    if (!startupActivated) return;
    // A last-moment draft/recovery transition still cancels an unload; never leave the quit latch set.
    const wasQuitting = quitting;
    quitting = false;
    updater.cancelledQuit();
    for (const client of windows.values()) client.setEnabled(true);
    if (!wasQuitting && dialog.showMessageBoxSync(win, { type: 'question', title: 'Reload Dilla?',
      message: 'Reloading ends any active call. Unsaved changes may be lost.',
      buttons: ['Stay', 'Reload'], defaultId: 0, cancelId: 0, noLink: true }) === 1) event.preventDefault();
  });
  win.webContents.on('will-attach-webview', event => { event.preventDefault(); });
  win.webContents.on('will-navigate', (event, url) => { if (!permits(url)) event.preventDefault(); });
  win.webContents.on('will-redirect', (event, url) => { if (!permits(url)) event.preventDefault(); });
  win.webContents.on('will-frame-navigate', event => { if (!event.isMainFrame || !permits(event.url)) event.preventDefault(); });
  win.webContents.setWindowOpenHandler(({ url }) => {
    const link = externalLink(url);
    if (link && startupActivated) void shell.openExternal(link).catch(() => {});
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
  const win = new BrowserWindow({ ...windowShape('starting'), useContentSize: true, ...(process.platform === 'linux' ? {} : { maximizable: false }), show: false, title: 'Dilla', ...windowChrome(), titleBarOverlay: false, webPreferences: { ...webPreferences(), session: ses } });
  if (process.platform === 'darwin') win.setWindowButtonVisibility(false);
  win.setEnabled(startupActivated);
  windows.set(instance, win); windowModes.set(instance, 'starting');
  updater.expectGuard(instance); startupUsability.expect(instance);
  const origin = new URL(instance).origin;
  const handler = (request: Request): Response | Promise<Response> => {
    if (new URL(request.url).origin !== origin) return fetchInstanceData(ses, request);
    const bundled = bundledResponse(bundle, request, instance);
    if (bundled) return bundled;
    const target = instanceRequestURL(request.url, instance);
    const forwarded = target === request.url ? request : new Request(target, request);
    return fetchInstanceData(ses, forwarded);
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
  ses.setPermissionCheckHandler((contents, permission, requestingOrigin, details) => {
    if (!startupActivated) return false;
    const trusted = contents === win.webContents && details.isMainFrame
      && permissionOriginMatches(requestingOrigin, instance) && trustedFrame({ url: contents.getURL(), top: true }, instance);
    // Copy uses Chromium's sanitized clipboard-write permission. Never grant reads,
    // background writes, or clipboard access to a frame/another instance.
    // Screen video belongs to the isolated capture broker. A display-capture
    // grant here also permits legacy capture that bypasses source selection.
    if (permission === 'display-capture') return false;
    if (permission === 'clipboard-sanitized-write' || permission === 'fullscreen') return trusted && win.isVisible() && win.isFocused();
    return trusted && allowed.has(permission);
  });
  ses.setPermissionRequestHandler((contents, permission, callback, details) => {
    if (!startupActivated) { callback(false); return; }
    if (contents !== win.webContents || details.isMainFrame === false || !trustedFrame({ url: details.requestingUrl, top: true }, instance)) { callback(false); return; }
    if (permission === 'display-capture') { callback(false); return; }
    if (permission === 'clipboard-sanitized-write' || permission === 'fullscreen') { callback(win.isVisible() && win.isFocused()); return; }
    if (!['media', 'notifications', 'speaker-selection', 'local-network', 'loopback-network'].includes(permission)) { callback(false); return; }
    if (allowed.has(permission)) { callback(true); return; }
    const label = permission === 'media' ? 'microphone or camera' : permission === 'notifications' ? 'notifications' : permission === 'local-network' ? 'local network connections for calls' : permission === 'loopback-network' ? 'connections to a local Dilla service' : 'audio output selection';
    void dialog.showMessageBox(win, { type: 'question', title: 'Dilla permission', message: `Allow ${new URL(instance).host} to use ${label}?`, buttons: ['Deny', 'Allow'], defaultId: 0, cancelId: 0 })
      .then(result => { if (result.response === 1) allowed.add(permission); callback(result.response === 1); }).catch(() => callback(false));
  });
  ses.setDisplayMediaRequestHandler((_request, callback) => { callback(null); }, { useSystemPicker: false });
  ses.on('will-download', (event, item) => { if (!item.getURL().startsWith(`blob:${origin}/`)) event.preventDefault(); });
  win.webContents.on('did-start-navigation', (_event, _url, _inPlace, mainFrame) => { if (mainFrame) closeNotices(instance); });
  win.webContents.on('render-process-gone', () => closeNotices(instance));
  win.on('closed', () => closeNotices(instance));
  lockWindow(win, url => isAppNavigation(url, instance));
  win.webContents.on('render-process-gone', () => { void startupUsability.status(instance, false); updater.expectGuard(instance); stopAppAudio(instance); });
  win.webContents.on('did-start-navigation', (_event, _url, inPlace, mainFrame) => { if (mainFrame && !inPlace) { void startupUsability.status(instance, false); updater.expectGuard(instance); stopAppAudio(instance); } });
  win.on('closed', () => { windows.delete(instance); windowModes.delete(instance); updater.removeGuard(instance); void startupUsability.status(instance, false); });
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
  if (!startupActivated) { pendingInvites.push(input); return; }
  if (askingQuit || quitting) return;
  try {
    const invite = parseInvite(input);
    if (await confirmInstance(invite.instance)) await openInstance(invite.instance, `/welcome?invite=${encodeURIComponent(invite.code)}`);
  } catch (error) { dialog.showErrorBox('Could not open this invite', error instanceof Error ? error.message : 'Invalid Dilla invite'); }
}

async function showManager(): Promise<void> {
  if (!shellReady || askingQuit || quitting) return;
  if (manager && !manager.isDestroyed()) { focus(manager); return; }
  manager = new BrowserWindow({ show: startupActivated, width: 720, height: 760, minWidth: 480, title: 'Dilla — your instances', ...windowChrome(), backgroundColor: '#0c1218', titleBarOverlay: { color: '#0c1218', symbolColor: '#a5b5c2', height: 32 }, webPreferences: webPreferences() });
  manager.setEnabled(startupActivated);
  lockWindow(manager, url => url === 'dilla-internal://setup/');
  manager.on('closed', () => { manager = null; void startupUsability.managerUnloaded(); });
  manager.webContents.on('render-process-gone', () => { void startupUsability.managerUnloaded(); });
  manager.webContents.on('did-start-navigation', (_event, _url, inPlace, mainFrame) => { if (mainFrame && !inPlace) void startupUsability.managerUnloaded(); });
  manager.webContents.on('did-finish-load', () => { if (manager && !manager.isDestroyed() && manager.webContents.getURL() === 'dilla-internal://setup/') void startupUsability.managerLoaded(); });
  await manager.loadURL('dilla-internal://setup/');
  if (manager.webContents.getURL() === 'dilla-internal://setup/') await startupUsability.managerLoaded();
}

async function setAutostart(enabled: boolean): Promise<void> {
  if (!app.isPackaged) throw new Error('Autostart is available in installed builds');
  if (process.platform === 'linux') await linuxAutostart(app.getPath('appData'), LINUX_INSTALL_KIND === 'system-package' ? '/usr/bin/dilla' : process.env.APPIMAGE ?? process.execPath, enabled);
  else app.setLoginItemSettings({ openAtLogin: enabled, path: process.platform==='win32'&&app.isPackaged?windowsLauncher:process.execPath, args: ['--hidden'] });
  await preferences.setAutostart(enabled);
  refreshMenu();
}

async function requestQuit(restart = false): Promise<void> {
  if (!startupActivated) { quitting = true; app.exit(1); return; }
  if (askingQuit || quitting) return;
  askingQuit = true;
  for (const selection of audioSelections.values()) selection.abort();
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
    const prepared = await updater.quit(true);
    if (ready && !prepared) {
      for (const win of windows.values()) win.setEnabled(true);
      dialog.showErrorBox('Dilla update failed', updater.state.message ?? 'The working version is still available. Check for updates to retry.');
      return;
    }
    quitting = true; app.quit();
  } finally { askingQuit = false; }
}

function refreshMenu(): void {
  if (!tray || !preferences) return;
  const count = [...badges.values()].reduce((a, b) => a + b, 0);
  app.setBadgeCount(count);
  tray.setToolTip(updater.state.phase === 'error' ? 'Dilla — update failed; open the tray to retry' : count ? `Dilla — ${count} unread` : 'Dilla');
  const template: Electron.MenuItemConstructorOptions[] = [
    ...[...captures].map(([instance, capture]) => ({ label: `Sharing application sound: ${capture.label} — stop`, click: () => stopAppAudio(instance) })),
    ...[...ringActions.values()].map(ring => ({ label: ring.title, submenu: [{ label: 'Answer', click: () => ring.run('answer') }, { label: 'Decline', click: () => ring.run('decline') }] })),
    { label: 'Your instances…', click: () => { void showManager(); } },
    ...[...windows].map(([instance, win]) => ({ label: new URL(instance).host + (new URL(instance).pathname === '/' ? '' : new URL(instance).pathname), click: () => focus(win) })),
    { type: 'separator' },
    { label: 'Start Dilla at login', type: 'checkbox', checked: preferences.autostart, click: item => { void setAutostart(item.checked).catch(error => dialog.showErrorBox('Could not change startup preference', String(error))); } },
    { label: updater.state.phase === 'checking' ? 'Checking for updates…' : updater.state.phase === 'error' ? 'Update failed — retry' : 'Check for updates', enabled: !['checking', 'downloading', 'installing'].includes(updater.state.phase), click: () => { void updater.check().then(() => {
      if (updater.state.phase === 'error') dialog.showErrorBox('Could not update Dilla', updater.state.message ?? 'Try again');
      else if (updater.state.phase === 'idle') void dialog.showMessageBox({ message: 'Dilla is up to date.', buttons: ['OK'] });
    }); } },
    ...(updater.state.phase === 'available' ? [{ label: updater.state.message, enabled: false }, { label: 'Update using your package manager', click: () => {
      if (updater.state.phase === 'available') void shell.openExternal(desktopReleasePage(updater.state.version));
    } }] : []),
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
  return { version: app.getVersion(), platform: process.platform, instance, capabilities: { applicationAudio: existsSync(audioExecutable), browserAuth: true, notifications: Notification.isSupported(), screenCapture: true } } satisfies DesktopInfo;
});
const browserCeremonies = new Map<string, AbortController>();
ipcMain.handle('dilla:startup-ready', async (event, ...args: unknown[]) => {
 const { win } = owner(event);
 if (args.length) throw new Error('Startup wait takes no arguments');
 const abort = new AbortController();
 const cancel = () => abort.abort();
 const navigation = (_event: unknown, _url: string, inPlace: boolean, mainFrame: boolean) => { if (mainFrame && !inPlace) cancel(); };
 win.once('closed', cancel);
 win.webContents.once('destroyed', cancel);
 win.webContents.once('render-process-gone', cancel);
 win.webContents.on('did-start-navigation', navigation);
 try { await startupGate.wait(win, abort.signal); owner(event); }
 finally {
  win.removeListener('closed', cancel);
  win.webContents.removeListener('destroyed', cancel);
  win.webContents.removeListener('render-process-gone', cancel);
  win.webContents.removeListener('did-start-navigation', navigation);
 }
});
ipcMain.handle('dilla:screen-capture', async (event, options: unknown, ...args: unknown[]) => {
 const { instance, win } = owner(event);
 if (!startupActivated || args.length || askingQuit || quitting || !win.isVisible() || !win.isFocused()) throw new Error('Screen capture unavailable');
 return screenCapture.request(instance, win, captureOptions(options));
});
ipcMain.handle('dilla:browser-auth', async (event, request: unknown, ...args: unknown[]) => {
 const { instance, win } = owner(event);
 if (!startupActivated || args.length || browserCeremonies.has(instance) || askingQuit || quitting) throw new Error('A browser sign-in is already running or Dilla is quitting');
 const controller = new AbortController(); browserCeremonies.set(instance, controller);
 const cancel = () => controller.abort();
 win.webContents.once('did-start-navigation', cancel);
 win.webContents.once('render-process-gone', cancel);
 win.once('closed', cancel);
 try { return await browserAuth(instance, request, { open: url => shell.openExternal(url), signal: controller.signal, allowLoopback }); }
 finally {
  browserCeremonies.delete(instance);
  win.webContents.removeListener('did-start-navigation', cancel);
  win.webContents.removeListener('render-process-gone', cancel);
  win.removeListener('closed', cancel);
 }
});
ipcMain.handle('dilla:app-audio', async (event, ...args: unknown[]) => {
 if (!startupActivated) throw new Error('Dilla is validating its installed update');
  const { instance, win } = owner(event);
  if (args.length || !existsSync(audioExecutable) || askingQuit || quitting || !win.isVisible() || !win.isFocused()) throw new Error('Application audio is unavailable');
  if (audioSelections.has(instance)) throw new Error('Application audio selection already open');
  const selection = new AbortController();
  audioSelections.set(instance, selection);
  const audio = new NativeAudio(audioExecutable);
  let id: string | undefined, promoted = false;
  const current = () => {
    if (selection.signal.aborted || win.isDestroyed() || win.webContents.isDestroyed() || windows.get(instance) !== win) return false;
    try { return owner(event).win === win; } catch { return false; }
  };
  const cancel = () => { selection.abort(); if (id) stopActiveAppAudio(instance, id); };
  const navigation = (_event: unknown, _url: string, inPlace: boolean, mainFrame: boolean) => { if (mainFrame && !inPlace) cancel(); };
  const dispose = () => {
    win.removeListener('closed', cancel);
    win.webContents.removeListener('destroyed', cancel);
    win.webContents.removeListener('render-process-gone', cancel);
    win.webContents.removeListener('did-start-navigation', navigation);
  };
  win.once('closed', cancel);
  win.webContents.once('destroyed', cancel);
  win.webContents.once('render-process-gone', cancel);
  win.webContents.on('did-start-navigation', navigation);
  win.once('hide', cancel);
  try {
    const sources = await audio.sources();
    if (!current() || askingQuit || quitting || !win.isVisible() || !win.isFocused()) return null;
    if (sources.length === 0) { await dialog.showMessageBox(win, { message: 'No playback applications are available. Start audio in the application, then try again.', buttons: ['OK'], signal: selection.signal }); return null; }
    const choice = await dialog.showMessageBox(win, { title: 'Share application sound', message: 'Choose the application others will hear', detail: 'Dilla’s own call audio is excluded.', buttons: ['Cancel', ...sources.map(source => source.label)], defaultId: 0, cancelId: 0, signal: selection.signal });
    if (!current() || askingQuit || quitting || !win.isVisible() || !win.isFocused() || !choice.response || !sources[choice.response - 1]) return null;
    // Replacing an active capture must not cancel this pending selection.
    stopActiveAppAudio(instance);
    const source = sources[choice.response - 1], captureId = randomUUID(); id = captureId;
    captures.set(instance, { id: captureId, label: source.label, audio, inFlight: 0, dispose });
    audio.start(source, samples => {
      if (!current()) { stopActiveAppAudio(instance, captureId); return; }
      const active = captures.get(instance);
      if (active?.id === captureId && active.inFlight < 8) { active.inFlight++; win.webContents.send('dilla:pcm', captureId, samples); }
    }, () => stopActiveAppAudio(instance, captureId));
    promoted = captures.get(instance)?.id === captureId;
    refreshMenu(); return { id: captureId, label: source.label };
  } catch (error) {
    if (id) stopActiveAppAudio(instance, id);
    if (selection.signal.aborted) return null;
    throw error;
  } finally {
    // Native enumeration cannot be cancelled: retain the slot until it truly settles.
    if (audioSelections.get(instance) === selection) audioSelections.delete(instance);
    win.removeListener('hide', cancel);
    if (!promoted) dispose();
  }
});
ipcMain.on('dilla:pcm-ack', (event, id: unknown) => {
  try { const { instance } = owner(event); const capture = captures.get(instance); if (capture && id === capture.id) capture.inFlight = Math.max(0, capture.inFlight - 1); }
  catch { /* Untrusted frames cannot acknowledge or control capture. */ }
});
ipcMain.handle('dilla:command', async (event, value: unknown) => {
  const { instance, win } = owner(event);
  const command = validateCommand(value);
  if (!startupActivated && !pendingStartupCommand(command.type)) throw new Error('Dilla is validating its installed update');
  if (command.type === 'guard') {
    const wasActive = updater.guards.get(instance)?.activeCall === true;
    updater.setGuard(instance, command);
    if (wasActive && !command.activeCall) stopAppAudio(instance);
  }
  else if (command.type === 'quit-ack') {
    if (quitProbe?.id !== command.id || !quitProbe.waiting.has(instance)) throw new Error('Expired quit acknowledgment');
    updater.setGuard(instance, command);
    quitProbe.waiting.delete(instance);
    if (quitProbe.waiting.size === 0) quitProbe.done();
  }
  else if (command.type === 'badge') { badges.set(instance, command.count); refreshMenu(); }
  else if (command.type === 'stop-app-audio') { if (captures.get(instance)?.id === command.id) stopAppAudio(instance); }
  else if (command.type === 'stop-screen-capture') screenCapture.stop(instance, command.id);
  else if (command.type === 'configure-screen-capture') await screenCapture.configure(instance, command.id, command.options);
  else if (command.type === 'autostart') await setAutostart(command.enabled);
  else if (command.type === 'boot-ready') await startupUsability.status(instance, command.usable);
  else if (command.type === 'window-mode') setWindowMode(instance, win, command.mode, command.height);
  else if (command.type === 'desktop-theme') {
    const appearance = windowAppearance(command.theme);
    win.setBackgroundColor(appearance.backgroundColor);
  }
  else if (command.type === 'window-menu') showWindowMenu(win);
  else if (command.type === 'window-control') {
    if (command.action === 'close') win.close();
    else if (command.action === 'minimize') win.minimize();
    else if (windowModes.get(instance) === 'workspace') { if (win.isMaximized()) win.unmaximize(); else win.maximize(); }
  }
  else if (command.type === 'check-update') await updater.check();
  else if (command.type === 'restart-update') await requestQuit(true);
  else if (command.type === 'dismiss-notification') { const key = noticeKey(instance, command.id); notices.get(key)?.close(); notices.delete(key); ringActions.delete(key); refreshMenu(); }
  else if (command.type === 'notify' || command.type === 'notify-ring') {
    if (!Notification.isSupported() || !permissions.get(instance)?.has('notifications')) return;
    const key = noticeKey(instance, command.id);
    const previous = notices.get(key);
    if (notices.size >= 64 && !notices.has(key)) return;
    const ring = command.type === 'notify-ring';
    const note = new Notification({ title: command.title, body: command.body, silent: ring, ...(ring ? {
      actions: [{ type: 'button', text: 'Answer' }, { type: 'button', text: 'Decline' }], timeoutType: 'never', urgency: 'critical',
    } as const : {}) });
    const current = () => notices.get(key) === note && !win.isDestroyed() && !win.webContents.isDestroyed();
    const action = (choice: 'answer' | 'decline') => {
      if (!current() || !ring) return;
      ringActions.delete(key);
      win.webContents.send('dilla:event', { type: 'notification-action', id: command.id, action: choice } satisfies DesktopEvent);
      notices.delete(key); note.close(); refreshMenu();
      if (choice === 'answer') focus(win);
    };
    note.on('action', event => { if (event.actionIndex === 0) action('answer'); else if (event.actionIndex === 1) action('decline'); });
    if (ring) ringActions.set(key, { title: command.title, run: action });
    else ringActions.delete(key);
    note.on('click', () => { if (!current()) return; focus(win); win.webContents.send('dilla:event', { type: 'notification-click', id: command.id } satisfies DesktopEvent); });
    note.on('close', () => {
      if (notices.get(key) !== note) return;
      notices.delete(key); ringActions.delete(key); refreshMenu();
      if (!win.isDestroyed()) win.webContents.send('dilla:event', { type: 'notification-close', id: command.id } satisfies DesktopEvent);
    });
    notices.set(key, note); previous?.close(); note.show(); refreshMenu();
  }
});
ipcMain.handle('dilla:manager', async (event, value: unknown) => {
  if (!manager || event.sender !== manager.webContents || event.senderFrame !== manager.webContents.mainFrame || event.senderFrame?.url !== 'dilla-internal://setup/') throw new Error('Untrusted instance chooser');
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error('Invalid manager request');
  const command = value as Record<string, unknown>;
  if (!startupActivated && command.type !== 'list') throw new Error('Dilla is validating its installed update');
  if (command.type === 'window-menu' && Object.keys(command).length === 1) { showWindowMenu(manager); return; }
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
const previousProcess = startupLaunch.previousPid;
void waitForPreviousProcess(previousProcess).then(async () => {
  if (!app.requestSingleInstanceLock()) { quitting = true; app.quit(); }
  else {
    // The lock serializes discovery/recovery and closes an early no-active
    // snapshot racing the quitting owner's final publication and exit.
    if (linuxApplicationUpdates && app.isPackaged && !startupLaunch.nonce && !('recoveredNonce' in startupLaunch) && (process.platform === 'linux' || process.platform === 'win32')) {
     try {
      if (await resumeInterruptedUpdate(app.getPath('userData'), process.platform === 'linux' ? process.env.APPIMAGE! : process.execPath,
       { publicKeys: RELEASE_PUBLIC_KEYS, platform: process.platform, arch: process.arch, version: app.getVersion(), channel: app.getVersion().includes('-beta.') ? 'beta' : 'stable' })) { quitting = true; app.exit(0); return; }
     } catch (error) { if (error instanceof UpdateStartupHeld) throw error; throw new UpdateStartupHeld('The interrupted update could not be verified. Restart Dilla after recovery finishes.'); }
    }
    if(restoredPlan?.windows)await verifyRestoredWindowsStartup(restoredPlan,{publicKeys:RELEASE_PUBLIC_KEYS,platform:'win32',arch:process.arch,version:app.getVersion(),channel:app.getVersion().includes('-beta.')?'beta':'stable'});
    if(app.isPackaged&&process.platform==='win32'&&!windowsLauncher)windowsLauncher=(await currentWindowsInstallation(app.getPath('userData'),process.execPath,{publicKeys:RELEASE_PUBLIC_KEYS,version:app.getVersion(),channel:app.getVersion().includes('-beta.')?'beta':'stable',arch:process.arch,productName:app.getName()})).launcher;
    app.on('second-instance', (_event, argv) => {
      const link = argv.find(value => value.startsWith('dilla://'));
      if (!shellReady || !startupActivated) { if (link) pendingInvites.push(link); return; }
      if (link) void openInvite(link);
      else { const win = [...windows.values()][0]; if (win) focus(win); else void showManager(); }
    });
    app.on('before-quit', event => { if (!quitting) { event.preventDefault(); void requestQuit(); } });
    app.on('will-quit', () => { for (const selection of audioSelections.values()) selection.abort(); for (const instance of captures.keys()) stopAppAudio(instance); screenCapture.stopAll(); });
    app.on('window-all-closed', () => { /* Tray owns the application's lifetime. */ });
    app.on('activate', () => { if (!startupActivated) return; const win = [...windows.values()][0]; if (win) focus(win); else void showManager(); });
    void app.whenReady().then(async () => {
      if (process.platform === 'darwin') app.dock?.setIcon(join(__dirname, 'icon.png'));
      bundle = await loadBundle(!app.isPackaged && process.argv.includes('--media-validation') ? join(__dirname, '../validation-dist') : join(__dirname, 'client'));
      let previousUpdateFailure = recoveryError;
      if (linuxApplicationUpdates && app.isPackaged && !startupLaunch.nonce && (process.platform === 'linux' || process.platform === 'win32')) {
       try {
        const pending = await pendingInstalledUpdate(app.getPath('userData'), app.getVersion());
        if (pending && !previousUpdateFailure) previousUpdateFailure = pending.failure === 'metadata'
         ? 'The working program was restored, but its update registration could not be restored. Check for updates to retry.'
         : 'The update did not finish. The working version and your device data are retained. Check for updates to retry.';
       } catch { previousUpdateFailure ??= 'The update status could not be read. Check for updates to retry.'; }
      }
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
      const activateTray = () => {
       tray = new Tray(nativeImage.createFromPath(join(__dirname, 'tray.png')));
       tray.on('click', () => { const win = [...windows.values()][0]; if (win) focus(win); else void showManager(); });
       if(app.isPackaged){if(process.platform==='win32'){if(!windowsLauncher)throw new Error('Missing verified Windows shell entry point');app.setAsDefaultProtocolClient('dilla',windowsLauncher,[]);}else app.setAsDefaultProtocolClient('dilla');}
      };
      shellReady = true;
      if (startupActivated) activateTray();
      const initial = arg('instance');
      if (initial && startupActivated) {
        const instance = canonicalInstance(initial, allowLoopback);
        if (allowLoopback && new URL(instance).protocol === 'http:') await preferences.addInstance(instance);
        if (await confirmInstance(instance)) await openInstance(instance, '/', !hidden);
      }
      for (const instance of preferences.instances) if (!windows.has(instance)) await openInstance(instance, '/', !hidden);
      if (startupActivated) for (const link of [...pendingInvites.splice(0), ...process.argv.filter(value => value.startsWith('dilla://'))]) await openInvite(link);
      if (windows.size === 0) await showManager();
      await startupUsability.seal();
      if (activationPlan) {
       await waitForStartupActivation(activationPlan, process.pid);
       startupActivated = true;
       startupGate.open();
       for (const win of windows.values()) { win.setEnabled(true); if (!hidden) focus(win); }
       if (manager && !manager.isDestroyed()) { manager.setEnabled(true); if (!hidden) focus(manager); }
       activateTray();
       for (const link of [...pendingInvites.splice(0), ...process.argv.filter(value => value.startsWith('dilla://'))]) await openInvite(link);
      }
      if(activationPlan?.windows&&await windowsRegistrationWarning(activationPlan))previousUpdateFailure='Dilla updated successfully, but its Apps & Features version could not be refreshed. Your device data is retained; retry setup to repair its registration.';
      refreshMenu();
      if (previousUpdateFailure) {
       updater.installationFailed(new Error(previousUpdateFailure));
       dialog.showErrorBox('Dilla update failed', previousUpdateFailure);
      }
      if (app.isPackaged && !previousUpdateFailure) {
        void updater.check();
        setInterval(() => { void updater.check(); }, 4 * 60 * 60 * 1000).unref();
      }
    }).catch(error => {
      if (!startupActivated) { quitting = true; app.exit(1); return; }
      dialog.showErrorBox('Dilla could not start', String(error)); quitting = true; app.quit();
    });
  }
}).catch(error => { if (error instanceof UpdateStartupHeld) { console.error('Dilla update startup is held:', error); quitting = true; app.exit(1); return; } dialog.showErrorBox('Dilla could not finish restarting', String(error)); quitting = true; app.quit(); });

}
