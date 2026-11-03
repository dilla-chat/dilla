import type { ElectronApplication, Page } from '@playwright/test';

type FocusFacts = { native: object; renderer: object; timedOut: boolean };

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

/** Preserve the existing native focus request and renderer proof verbatim. A failed proof gets bounded evidence. */
export async function focusNativePage(application: ElectronApplication, page: Page,
  report: (facts: FocusFacts) => void = facts => console.log('Native focus diagnostic: ' + JSON.stringify(facts)),
): Promise<void> {
  const win = await application.browserWindow(page);
  try { await win.evaluate(win => { if (win.isMinimized()) win.restore(); win.show(); win.focus(); }); }
  finally { await win.dispose(); }
  try { await page.waitForFunction(() => document.hasFocus()); }
  catch (error) {
    try { report(await focusFacts(application, page)); } catch { /* Evidence must not replace the original focus failure. */ }
    throw error;
  }
}
