import type { ElectronApplication, Page } from '@playwright/test';

type FocusFacts = { native: object; renderer: object; timedOut: boolean };
type FocusOptions = {
  /** Raise the window above every other one for the request. Windows refuses a plain focus() while
   * another process, such as the second Electron device of the same test, owns the foreground. */
  steal?: boolean;
  /** How often the request is repeated while the renderer proof is still pending. */
  repeatMs?: number;
};

/** Failure-only evidence: no URLs, titles, process arguments, page text or exception strings. */
async function focusFacts(application: ElectronApplication, page: Page): Promise<FocusFacts> {
  const facts: FocusFacts = { native: { available: false }, renderer: { available: false }, timedOut: false };
  const native = (async () => {
    const window = await application.browserWindow(page);
    try {
      facts.native = await window.evaluate(win => {
        const destroyed = Boolean(win.isDestroyed());
        if (destroyed) return { available: true, destroyed };
        const webContentsDestroyed = Boolean(win.webContents.isDestroyed());
        return { available: true, destroyed, focused: Boolean(win.isFocused()), visible: Boolean(win.isVisible()),
          minimized: Boolean(win.isMinimized()), fullScreen: Boolean(win.isFullScreen()), webContentsDestroyed,
          ...(!webContentsDestroyed ? { webContentsFocused: Boolean(win.webContents.isFocused()) } : {}) };
      });
    } finally { await window.dispose(); }
  })().catch(() => {});
  const renderer = page.evaluate(() => ({ available: true, hasFocus: Boolean(document.hasFocus()),
    visibility: document.visibilityState === 'visible' ? 'visible' : document.visibilityState === 'hidden' ? 'hidden' : 'other',
  })).then(value => { facts.renderer = value; }).catch(() => {});
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    await Promise.race([Promise.all([native, renderer]), new Promise<void>(resolve => {
      timer = setTimeout(() => { facts.timedOut = true; resolve(); }, 500);
    })]);
    return { ...facts };
  } finally { clearTimeout(timer); }
}

/** The native focus request. With `steal`, the window is raised on top for the request and put back
 * afterwards, the same approach clickNative takes on Linux against always-on-top windows. */
async function requestNativeFocus(application: ElectronApplication, page: Page, steal: boolean): Promise<void> {
  const win = await application.browserWindow(page);
  try {
    await win.evaluate((win, steal) => {
      if (win.isMinimized()) win.restore();
      if (!steal) { win.show(); win.focus(); return; }
      const previous = win.isAlwaysOnTop();
      win.setAlwaysOnTop(true);
      try { win.moveTop(); win.show(); win.focus(); }
      finally { win.setAlwaysOnTop(previous); }
    }, steal);
  } finally { await win.dispose(); }
}

/** Request native focus and require the renderer's proof of it. The request is repeated while the proof is
 * pending, since the OS can refuse one request and grant a later one; a repeat is bounded and never replaces
 * the proof's own failure. A failed proof gets bounded evidence and rethrows the exact original error. */
export async function focusNativePage(application: ElectronApplication, page: Page,
  report: (facts: FocusFacts) => void = facts => console.log('Native focus diagnostic: ' + JSON.stringify(facts)),
  options: FocusOptions = {},
): Promise<void> {
  const steal = options.steal ?? process.platform === 'win32';
  const repeatMs = options.repeatMs ?? 2000;
  await requestNativeFocus(application, page, steal);
  let settled = false, timer: ReturnType<typeof setTimeout> | undefined;
  void (async () => {
    while (!settled) {
      await new Promise<void>(resolve => { timer = setTimeout(resolve, repeatMs); });
      if (settled) return;
      await Promise.race([requestNativeFocus(application, page, steal).catch(() => {}),
        new Promise<void>(resolve => setTimeout(resolve, 500))]);
    }
  })();
  try { await page.waitForFunction(() => document.hasFocus()); }
  catch (error) {
    try { report(await focusFacts(application, page)); } catch { /* Evidence must not replace the original focus failure. */ }
    throw error;
  } finally { settled = true; clearTimeout(timer); }
}
