import { BrowserWindow, desktopCapturer, dialog, ipcMain, MessageChannelMain, session, type Session } from 'electron';
import { randomUUID } from 'node:crypto';
import { readFile } from 'node:fs/promises';
import { join } from 'node:path';
import type { ScreenCaptureOptions, ScreenCaptureSource } from './contract.ts';
import { captureId, captureOptions, videoOnlyPermission } from './screen-capture-contract.ts';

const CAPTURE_URL = 'dilla-internal://capture/';
const SCRIPT_URL = `${CAPTURE_URL}screen-capture-broker.js`;
const CSP = "default-src 'none'; script-src 'self'; connect-src 'none'; worker-src 'none'; frame-src 'none'; object-src 'none'; base-uri 'none'; form-action 'none'";
interface Capture {
  instance: string;
  id: string;
  owner: BrowserWindow;
  window: BrowserWindow | null;
  session: Session | null;
  abort: AbortController;
  dispose: (() => void)[];
  announced: boolean;
  settled: boolean;
  surface: 'monitor' | 'window';
  resolve: (source: ScreenCaptureSource | null) => void;
  reject: (error: Error) => void;
  timer: ReturnType<typeof setTimeout> | null;
  heartbeat: ReturnType<typeof setInterval> | null;
  lastHeartbeat: number;
  configuring: boolean;
  pendingOptions: ScreenCaptureOptions | null;
  selecting: boolean;
}

/** Main selects the source; the account renderer never receives capture permission or a native source ID. */
export class ScreenCaptureBroker {
  // Capture IDs keep the published and incoming tracks independently owned during a bounded handover.
  private captures = new Map<string, Capture>();
  // A stopped native portal retains this slot until the uncancelable platform operation really settles.
  private pending = new Map<string, Capture>();
  private directory: string;
  private onEnded: (instance: string, id: string) => void;
  constructor(options: { directory: string; onEnded: (instance: string, id: string) => void }) {
    this.directory = options.directory;
    this.onEnded = options.onEnded;
  }
  async request(instance: string, owner: BrowserWindow, input: ScreenCaptureOptions): Promise<ScreenCaptureSource | null> {
    const options = captureOptions(input);
    if (!instance || owner.isDestroyed() || owner.webContents.isDestroyed()) throw new Error('Screen capture owner unavailable');
    if (this.pending.has(instance)) throw new Error('Screen capture selection already open');
    if ([...this.captures.values()].filter(capture => capture.instance === instance).length >= 2) {
      throw new Error('Screen capture handover limit reached. Finish the previous replacement first.');
    }
    return new Promise((resolve, reject) => {
      const capture: Capture = { instance, id: randomUUID(), owner, window: null, session: null, abort: new AbortController(),
        dispose: [], announced: false, settled: false, surface: 'monitor', resolve, reject, timer: null, heartbeat: null, lastHeartbeat: 0, configuring: false, pendingOptions: null, selecting: true };
      this.captures.set(capture.id, capture);
      this.pending.set(instance, capture);
      const finish = () => this.finish(capture, 'stopped');
      const navigation = (_event: unknown, _url: string, inPlace: boolean, mainFrame: boolean) => { if (mainFrame && !inPlace) finish(); };
      owner.once('closed', finish);
      owner.webContents.once('destroyed', finish);
      owner.webContents.once('render-process-gone', finish);
      owner.webContents.on('did-start-navigation', navigation);
      capture.dispose.push(() => {
        owner.removeListener('closed', finish);
        owner.webContents.removeListener('destroyed', finish);
        owner.webContents.removeListener('render-process-gone', finish);
        owner.webContents.removeListener('did-start-navigation', navigation);
      });
      void this.select(capture, options).catch(() => this.finish(capture, 'failed'));
    });
  }
  async configure(instance: string, id: string, input: ScreenCaptureOptions): Promise<void> {
    const options = captureOptions(input);
    const capture = this.captures.get(id);
    if (!captureId(id) || capture?.instance !== instance || !capture.window || capture.window.isDestroyed() || !capture.settled) {
      throw new Error('Screen capture unavailable');
    }
    // A hostile renderer cannot build an unbounded native constraint/IPC queue. Keep only the latest request.
    capture.pendingOptions = options;
    this.sendConfiguration(capture);
  }
  private sendConfiguration(capture: Capture): void {
    if (capture.configuring || !capture.pendingOptions || !capture.window) return;
    const options = capture.pendingOptions;
    capture.pendingOptions = null;
    capture.configuring = true;
    try { capture.window.webContents.send('dilla:screen-broker-configure', { id: capture.id, options }); }
    catch { this.finish(capture, 'failed'); }
  }
  stop(instance: string, id?: string): void {
    if (id !== undefined) {
      const capture = this.captures.get(id);
      if (capture?.instance === instance) this.finish(capture, 'stopped');
      return;
    }
    for (const capture of this.captures.values()) if (capture.instance === instance) this.finish(capture, 'stopped');
  }
  stopAll(): void { for (const capture of this.captures.values()) this.finish(capture, 'stopped'); }
  private current(capture: Capture): boolean { return this.captures.get(capture.id) === capture && !capture.owner.isDestroyed() && !capture.owner.webContents.isDestroyed(); }
  private async select(capture: Capture, options: ScreenCaptureOptions): Promise<void> {
    try {
      // On Wayland this call opens the interactive portal. It cannot be aborted or given an acquisition deadline.
      const sources = await desktopCapturer.getSources({ types: ['screen', 'window'], thumbnailSize: { width: 0, height: 0 }, fetchWindowIcons: false });
      if (!this.current(capture)) return;
      if (!sources.length) throw new Error('No screen capture sources');
      const choice = await dialog.showMessageBox(capture.owner, { type: 'question', title: 'Share your screen', message: 'Choose what to share',
        buttons: ['Cancel', ...sources.map(source => source.name)], defaultId: 0, cancelId: 0, noLink: true, signal: capture.abort.signal });
      if (!this.current(capture)) return;
      if (choice.response === 0) { this.finish(capture, 'stopped'); return; }
      const source = sources[choice.response - 1];
      if (!source || !/^(screen|window):/.test(source.id)) throw new Error('Invalid native screen capture selection');
      capture.surface = source.id.startsWith('window:') ? 'window' : 'monitor';
      await this.start(capture, source.id, options);
    } finally {
      capture.selecting = false;
      // An aborted uncancelable portal still owns its instance's selection slot until the real operation settles.
      if (!this.current(capture) && this.pending.get(capture.instance) === capture) this.pending.delete(capture.instance);
    }
  }
  private async start(capture: Capture, sourceId: string, options: ScreenCaptureOptions): Promise<void> {
    const isolated = session.fromPartition(`dilla-screen-${capture.id}`, { cache: false });
    capture.session = isolated;
    isolated.protocol.handle('dilla-internal', async request => {
      const file = request.url === CAPTURE_URL ? 'screen-capture.html' : request.url === SCRIPT_URL ? 'screen-capture-broker.js' : null;
      if (!this.current(capture) || !file || request.method !== 'GET') return new Response(null, { status: 404 });
      try {
        return new Response(await readFile(join(this.directory, file)), { headers: {
          'Content-Type': file.endsWith('.html') ? 'text/html; charset=utf-8' : 'text/javascript; charset=utf-8',
          'Content-Security-Policy': CSP, 'X-Content-Type-Options': 'nosniff', 'Cache-Control': 'no-store',
        } });
      } catch { return new Response(null, { status: 404 }); }
    });
    isolated.setPermissionRequestHandler((contents, permission, callback, details) => {
      callback(this.current(capture) && contents === capture.window?.webContents && contents.getURL() === CAPTURE_URL
        && videoOnlyPermission(permission, details, CAPTURE_URL));
    });
    isolated.setPermissionCheckHandler((contents, permission, _origin, details) =>
      this.current(capture) && contents === capture.window?.webContents && contents?.getURL() === CAPTURE_URL
        && permission === 'display-capture' && details.isMainFrame === true && details.requestingUrl === CAPTURE_URL && details.mediaType === 'video');
    // The fixed broker uses only legacy selected-source video. Modern capture is always denied.
    isolated.setDisplayMediaRequestHandler((_request, callback) => callback(null));
    isolated.webRequest.onBeforeRequest({ urls: ['<all_urls>'] }, (details, callback) => {
      callback({ cancel: !this.current(capture) || (details.url !== CAPTURE_URL && details.url !== SCRIPT_URL) });
    });
    const download = (event: Electron.Event) => event.preventDefault();
    isolated.on('will-download', download);
    capture.dispose.push(() => isolated.removeListener('will-download', download));
    const broker = new BrowserWindow({ show: false, focusable: false, skipTaskbar: true, width: 16, height: 16,
      webPreferences: { session: isolated, preload: join(this.directory, 'screen-capture-preload.cjs'), sandbox: true,
        contextIsolation: true, nodeIntegration: false, nodeIntegrationInWorker: false, nodeIntegrationInSubFrames: false,
        webSecurity: true, webviewTag: false, devTools: false, disableDialogs: true, spellcheck: false, backgroundThrottling: false } });
    capture.window = broker;
    broker.webContents.setWindowOpenHandler(() => ({ action: 'deny' }));
    broker.webContents.on('will-attach-webview', event => event.preventDefault());
    const fail = () => this.finish(capture, 'failed');
    broker.once('closed', fail);
    broker.webContents.once('destroyed', fail);
    broker.webContents.once('render-process-gone', fail);
    broker.webContents.on('will-frame-navigate', event => { event.preventDefault(); fail(); });
    let initialNavigation = true;
    broker.webContents.on('did-start-navigation', (_event, url, inPlace, mainFrame) => {
      if (initialNavigation && mainFrame && !inPlace && url === CAPTURE_URL) { initialNavigation = false; return; }
      fail();
    });
    const status = (event: Electron.IpcMainEvent, value: unknown) => {
      if (!this.current(capture) || event.sender !== broker.webContents || event.senderFrame !== broker.webContents.mainFrame
        || event.senderFrame?.url !== CAPTURE_URL) return;
      if (!value || typeof value !== 'object' || Array.isArray(value)) { fail(); return; }
      const data = value as Record<string, unknown>;
      if (Object.keys(data).sort().join(',') !== 'id,type' || data.id !== capture.id) { fail(); return; }
      if (data.type === 'failed' || data.type === 'ended') { this.finish(capture, data.type === 'ended' && capture.settled ? 'stopped' : 'failed'); return; }
      if (data.type === 'configured') {
        if (!capture.configuring) { fail(); return; }
        capture.configuring = false;
        this.sendConfiguration(capture);
        return;
      }
      if (data.type !== 'started' && data.type !== 'heartbeat') { fail(); return; }
      capture.lastHeartbeat = Date.now();
      if (data.type === 'started' && !capture.settled) {
        capture.settled = true;
        if (this.pending.get(capture.instance) === capture) this.pending.delete(capture.instance);
        if (capture.timer) clearTimeout(capture.timer); capture.timer = null;
        capture.resolve({ id: capture.id, displaySurface: capture.surface });
      }
    };
    ipcMain.on('dilla:screen-broker-status', status);
    capture.dispose.push(() => ipcMain.removeListener('dilla:screen-broker-status', status));
    capture.timer = setTimeout(fail, 15000);
    await broker.loadURL(CAPTURE_URL);
    if (!this.current(capture)) return;
    const { port1, port2 } = new MessageChannelMain();
    // Main only hands off the endpoints; pixel messages never enter main JavaScript.
    try {
      capture.owner.webContents.postMessage('dilla:screen-port', { id: capture.id }, [port2]);
      capture.announced = true;
      broker.webContents.postMessage('dilla:screen-broker-start', { id: capture.id, sourceId, options }, [port1]);
    } catch { port1.close(); port2.close(); throw new Error('Screen capture port unavailable'); }
    capture.lastHeartbeat = Date.now();
    capture.heartbeat = setInterval(() => { if (Date.now() - capture.lastHeartbeat >= 5000) fail(); }, 1000);
  }
  private finish(capture: Capture, reason: 'stopped' | 'failed'): void {
    if (this.captures.get(capture.id) !== capture) return;
    this.captures.delete(capture.id);
    if (!capture.selecting && this.pending.get(capture.instance) === capture) this.pending.delete(capture.instance);
    capture.abort.abort();
    if (capture.timer) clearTimeout(capture.timer);
    if (capture.heartbeat) clearInterval(capture.heartbeat);
    for (const dispose of capture.dispose) dispose();
    capture.dispose = [];
    const broker = capture.window;
    capture.window = null;
    // Destruction releases every capture track and transferred port even if the page is hung.
    if (broker && !broker.isDestroyed()) broker.destroy();
    const isolated = capture.session;
    capture.session = null;
    if (isolated) {
      isolated.setPermissionCheckHandler(() => false);
      isolated.setPermissionRequestHandler((_contents, _permission, callback) => callback(false));
      isolated.setDisplayMediaRequestHandler((_request, callback) => callback(null));
      isolated.webRequest.onBeforeRequest({ urls: ['<all_urls>'] }, (_details, callback) => callback({ cancel: true }));
      isolated.protocol.unhandle('dilla-internal');
      // Electron owns Session lifetime; a nonpersistent partition has no public destroy API.
      void Promise.allSettled([isolated.closeAllConnections(), isolated.clearStorageData(), isolated.clearCache()]);
    }
    if (!capture.settled) {
      capture.settled = true;
      if (reason === 'failed') capture.reject(new Error('Screen capture failed. Choose a source and try again.'));
      else capture.resolve(null);
    }
    if (capture.announced) this.onEnded(capture.instance, capture.id);
  }
}
