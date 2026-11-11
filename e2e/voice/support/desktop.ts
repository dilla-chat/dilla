import { test as playwrightTest, type BrowserContext, type LaunchOptions, type TestInfo } from '@playwright/test';
import { writeFile } from 'node:fs/promises';
import { desktopApplication, launchDesktopContext } from '../../web/support/electron.ts';
import { prepareMacNotificationDiagnostic } from '../../web/support/mac-notification-diagnostics.ts';

export const liveDesktopVoiceContexts = new Set<BrowserContext>();
let diagnosticNumber = 0;

export async function desktopVoiceContext(options: { baseURL?: string; permissions?: string[]; launchOptions: LaunchOptions; viewport?: { width: number; height: number } | null }): Promise<BrowserContext> {
 let info: TestInfo | undefined;
 try { info = playwrightTest.info(); } catch { /* Standalone callers have no artifact owner. */ }
 const enabled = process.env.DILLA_E2E_MAC_NOTIFICATION_DIAGNOSTICS === '1' && !process.env.DILLA_PERFORMANCE_CLIENT;
 const path = enabled && process.platform === 'darwin' ? info?.outputPath(`mac-native-notification-${++diagnosticNumber}.json`) : undefined;
 const diagnostic = await prepareMacNotificationDiagnostic({ enabled, environment: options.launchOptions.env, report: async facts => {
  console.info('Mac native notification diagnostic: ' + JSON.stringify(facts));
  if (path) {
   await writeFile(path, JSON.stringify(facts));
   if (facts.finished) await info?.attach('Mac native notification diagnostic', { path, contentType: 'application/json' });
  }
 } });
 let context: BrowserContext | undefined;
 try {
  context = await launchDesktopContext(null, diagnostic ? { ...options, launchOptions: { ...options.launchOptions, env: diagnostic.environment } } : options, false);
  const application = desktopApplication(context);
  if (diagnostic) {
   try { await diagnostic.runtime(await application.evaluate(({ app }) => ({ execPath: process.execPath, isPackaged: app.isPackaged }))); }
   catch { await diagnostic.capture(); } // Evidence collection cannot replace a product/test error.
  }
  await application.evaluate(({ dialog }, permissions) => {
   const original = dialog.showMessageBox.bind(dialog);
   const synchronous = dialog.showMessageBoxSync.bind(dialog);
   dialog.showMessageBoxSync = ((...args: any[]) => {
    if (args.at(-1).title === 'Reload Dilla?') return 1;
    return (synchronous as any)(...args);
   }) as typeof dialog.showMessageBoxSync;
   dialog.showMessageBox = (async (...args: any[]) => {
    const options = args.at(-1);
    // Respond to the shipping permission boundary; no permission map or handler
    // is replaced, so frame/origin/gesture validation continues to run.
    if (options.title === 'Dilla permission') {
     const names = options.message.includes('microphone or camera') ? ['microphone', 'camera']
      : options.message.includes('notifications') ? ['notifications']
      : options.message.includes('local network') ? ['local-network']
      : options.message.includes('local Dilla') ? ['loopback-network'] : [];
     return { response: names.some(name => permissions.includes(name)) ? 1 : 0, checkboxChecked: false };
    }
    // Choose a real capturer source from the shipping picker, not synthetic PCM
    // or a substituted getDisplayMedia implementation.
    if (options.title === 'Share your screen' && options.buttons.length > 1) return { response: 1, checkboxChecked: false };
    return (original as any)(...args);
   }) as typeof dialog.showMessageBox;
  }, [...(options.permissions ?? []), 'local-network', 'loopback-network']);
  context.pages()[0].on('dialog', () => {}); // The native confirmation handles protected reloads.
  if (options.permissions?.includes('notifications')) {
   const permission = await context.pages()[0].evaluate(() => Notification.requestPermission());
   if (permission !== 'granted') throw new Error('The native notification permission boundary denied acceptance consent');
  }
  liveDesktopVoiceContexts.add(context);
  const close = context.close.bind(context);
  const ownedContext = context;
  context.close = async options => { try { await close(options); } finally { liveDesktopVoiceContexts.delete(ownedContext); await diagnostic?.finish(); } };
  return context;
 } catch (error) {
  try { await context?.close(); } catch { /* Preserve the original launch or permission failure. */ }
  await diagnostic?.finish(); throw error;
 }
}

export interface NativeScreenRequest {
 fields: string[]; width: number; height: number; frameRate: number; owningMainFrame: boolean;
}
export interface NativeScreenSelection { source: string; cancelled: boolean }

/** Observe the real typed invoke after Electron dispatches it, and answer only
 * the real native picker. No bridge, IPC handler, capture API or response is replaced. */
export async function observeDesktopScreen(ownerContext: BrowserContext, sourceContext: BrowserContext, cancel = false): Promise<{
 requests(): Promise<NativeScreenRequest[]>;
 selections(): Promise<NativeScreenSelection[]>;
 brokers(): Promise<number>;
 dispose(): Promise<void>;
}> {
 const application = desktopApplication(ownerContext), sourceApplication = desktopApplication(sourceContext);
 const sourceWindow = await sourceApplication.browserWindow(sourceContext.pages()[0]);
 const title = `Dilla voice screen source ${globalThis.crypto.randomUUID()}`;
 const previousTitle = await sourceWindow.evaluate((window, title) => { const previous = window.getTitle(); window.setTitle(title); return previous; }, title);
 await sourceWindow.dispose();
 await application.evaluate(({ BrowserWindow, dialog }, { title, cancel }) => {
  const owner = BrowserWindow.getAllWindows().find(window => window.webContents.getURL().startsWith('http:'))!;
  const requests: NativeScreenRequest[] = [], selections: NativeScreenSelection[] = [];
  // Electron 45 preserves this Session inspector event after invoking its normal
  // handler. Merely listening cannot grant permission, change a reply or stop delivery.
  const observe = (event: Electron.IpcMainInvokeEvent, channel: string, args: unknown[]) => {
   if (channel !== 'dilla:screen-capture') return;
   const input = args[0] as { width: number; height: number; frameRate: number };
   requests.push({ fields: Object.keys(input).sort(), width: input.width, height: input.height, frameRate: input.frameRate,
    owningMainFrame: event.sender === owner.webContents && event.senderFrame === owner.webContents.mainFrame });
  };
  (owner.webContents.session as any).on('-ipc-invoke', observe);
  const original = dialog.showMessageBox;
  const choose: typeof dialog.showMessageBox = (async (...args: any[]) => {
   const options = args.at(-1);
   if (options.title !== 'Share your screen') return (original as any).apply(dialog, args);
   const matches = options.buttons.flatMap((name: string, index: number) => name === title ? [index] : []);
   if (matches.length !== 1 || matches[0] === 0 || options.buttons[0] !== 'Cancel' || options.defaultId !== 0 || options.cancelId !== 0) throw new Error('The real named source must be offered by the shipping screen picker');
   selections.push({ source: options.buttons[matches[0]], cancelled: cancel });
   return { response: cancel ? 0 : matches[0], checkboxChecked: false };
  }) as typeof dialog.showMessageBox;
  dialog.showMessageBox = choose;
  (globalThis as any).__voiceNativeScreen = { requests, selections, dispose: () => {
   (owner.webContents.session as any).removeListener('-ipc-invoke', observe);
   if (dialog.showMessageBox === choose) dialog.showMessageBox = original;
  } };
 }, { title, cancel });
 // macOS, Windows and Linux X11 open the main-owned thumbnail picker window instead of the native list dialog
 // (Wayland keeps the portal and the dialog above). Answer it the same way: the real named source, or Cancel.
 const pickerSelections: NativeScreenSelection[] = [];
 const onPicker = (picker: import('@playwright/test').Page) => {
  void (async () => {
   await picker.waitForLoadState('domcontentloaded');
   if (!picker.url().startsWith('dilla-internal://picker/')) return;
   if (cancel) {
    await picker.getByRole('button', { name: 'Cancel', exact: true }).click();
   } else {
    await picker.getByRole('tab', { name: 'Windows' }).click();
    await picker.getByRole('option', { name: title, exact: true }).click();
    await picker.getByRole('button', { name: 'Share', exact: true }).click();
   }
   pickerSelections.push({ source: title, cancelled: cancel });
  })().catch(error => { pickerSelections.push({ source: `error: ${String(error)}`, cancelled: cancel }); });
 };
 application.on('window', onPicker);
 return {
  requests: () => application.evaluate(() => (globalThis as any).__voiceNativeScreen.requests),
  selections: async () => [...await application.evaluate(() => (globalThis as any).__voiceNativeScreen.selections as NativeScreenSelection[]), ...pickerSelections],
  brokers: () => application.evaluate(({ BrowserWindow }) => BrowserWindow.getAllWindows().filter(window => window.webContents.getURL() === 'dilla-internal://capture/').length),
  dispose: async () => {
   application.off('window', onPicker);
   try { await application.evaluate(() => { (globalThis as any).__voiceNativeScreen.dispose(); delete (globalThis as any).__voiceNativeScreen; }); }
   finally {
    const source = await sourceApplication.browserWindow(sourceContext.pages()[0]);
    try { await source.evaluate((window, title) => window.setTitle(title), previousTitle); } finally { await source.dispose(); }
   }
  },
 };
}
