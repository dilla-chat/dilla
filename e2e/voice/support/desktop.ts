import type { BrowserContext, LaunchOptions } from '@playwright/test';
import { desktopApplication, launchDesktopContext } from '../../web/support/electron.ts';

export const liveDesktopVoiceContexts = new Set<BrowserContext>();

export async function desktopVoiceContext(options: { baseURL?: string; permissions?: string[]; launchOptions: LaunchOptions; viewport?: { width: number; height: number } | null }): Promise<BrowserContext> {
 const context = await launchDesktopContext(null, options, false);
 const application = desktopApplication(context);
 try {
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
  context.close = async options => { try { await close(options); } finally { liveDesktopVoiceContexts.delete(context); } };
  return context;
 } catch (error) { await context.close(); throw error; }
}
