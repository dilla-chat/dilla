import { execFileSync } from 'node:child_process';
import type { ElectronApplication, Locator, Page } from '@playwright/test';

/** Exercise the OS hit test as well as the renderer: CDP mouse events alone
 * bypass Electron's draggable/non-client regions on Linux. */
export async function clickNative(application: ElectronApplication, page: Page, target: Locator): Promise<void> {
 if (process.platform !== 'linux') { await target.click(); return; }
 const window = await application.browserWindow(page);
 // Keep unrelated always-on-top windows (for example picture-in-picture)
 // from intercepting fixture clicks; restore the original state afterwards.
 const wasAbove = await window.evaluate(win => { const previous = win.isAlwaysOnTop(); win.setAlwaysOnTop(true); win.moveTop(); win.show(); win.focus(); return previous; });
 try {
 await page.waitForFunction(() => document.hasFocus());
 const id = await window.evaluate(win => win.getNativeWindowHandle().readUInt32LE());
 // Let Chromium publish the post-dialog native hit-test regions.
 await page.evaluate(() => new Promise<void>(done => requestAnimationFrame(() => requestAnimationFrame(() => done()))));
 const box = await target.boundingBox(); if (!box) throw new Error('Native click target is absent');
 const scale = await page.evaluate(() => devicePixelRatio);
 const input = (action: 'move' | 'click', dx = 0, dy = 0) => execFileSync('python3', ['-c', `
import ctypes as c, sys
x=c.CDLL('libX11.so.6'); t=c.CDLL('libXtst.so.6')
x.XOpenDisplay.restype=c.c_void_p
d=x.XOpenDisplay(None)
if not d: raise RuntimeError('X11 display unavailable')
x.XDefaultRootWindow.argtypes=[c.c_void_p]; x.XDefaultRootWindow.restype=c.c_ulong
root=x.XDefaultRootWindow(d)
x.XTranslateCoordinates.argtypes=[c.c_void_p,c.c_ulong,c.c_ulong,c.c_int,c.c_int,c.POINTER(c.c_int),c.POINTER(c.c_int),c.POINTER(c.c_ulong)]
px=c.c_int(); py=c.c_int(); child=c.c_ulong()
if not x.XTranslateCoordinates(d,int(sys.argv[1]),root,int(sys.argv[2]),int(sys.argv[3]),c.byref(px),c.byref(py),c.byref(child)): raise RuntimeError('Native window disappeared')
t.XTestFakeMotionEvent.argtypes=[c.c_void_p,c.c_int,c.c_int,c.c_int,c.c_ulong]
t.XTestFakeButtonEvent.argtypes=[c.c_void_p,c.c_uint,c.c_int,c.c_ulong]
x.XSync.argtypes=[c.c_void_p,c.c_int]; x.XCloseDisplay.argtypes=[c.c_void_p]
if sys.argv[4] == 'move':
 t.XTestFakeMotionEvent(d,-1,px.value,py.value,0)
else:
 t.XTestFakeButtonEvent(d,1,1,0); t.XTestFakeButtonEvent(d,1,0,0)
x.XSync(d,0); x.XCloseDisplay(d)
`, String(id), String(Math.round((box.x + box.width / 2 + dx) * scale)), String(Math.round((box.y + box.height / 2 + dy) * scale)), action]);
 const point = { x: Math.round(box.x + box.width / 2), y: Math.round(box.y + box.height / 2) };
 await page.evaluate(() => {
  const w = window as any; w.__dillaNativePointer = null;
  w.__dillaNativePointerListener = (event: PointerEvent) => { w.__dillaNativePointer = { x: event.clientX, y: event.clientY }; };
  document.addEventListener('pointermove', w.__dillaNativePointerListener, true);
 });
 try {
  let dx = 0, dy = 0, last: { x: number; y: number } | null = null;
  for (let attempt = 0; attempt < 20; attempt++) {
   const jiggle = attempt % 2 ? 2 : -2;
   input('move', dx + jiggle, dy);
   await page.evaluate(() => new Promise<void>(done => requestAnimationFrame(() => requestAnimationFrame(() => done()))));
   const actual = await page.evaluate(() => (window as any).__dillaNativePointer as { x: number; y: number } | null);
   last = actual;
   if (actual && Math.abs(actual.x - point.x - jiggle) <= 1 && Math.abs(actual.y - point.y) <= 1) { input('click'); return; }
   if (actual) { dx += point.x + jiggle - actual.x; dy += point.y - actual.y; }
  }
  throw new Error('OS pointer did not reach the native click target: ' + JSON.stringify({ point, last, dx, dy, scale, box }));
 } finally {
  await page.evaluate(() => { const w = window as any; document.removeEventListener('pointermove', w.__dillaNativePointerListener, true); delete w.__dillaNativePointerListener; delete w.__dillaNativePointer; });
 }
 } finally { await window.evaluate((win, previous) => win.setAlwaysOnTop(previous), wasAbove).catch(() => {}); await window.dispose(); }
}
