import { execFileSync } from 'node:child_process';

// Match the actual native confirmation, not another window with a similar title.
export function selectConnectDialog(windows, pid) {
 if (!Number.isSafeInteger(pid) || pid < 2) throw new Error('Invalid owned app PID');
 const matches = [];
 for (const { id, properties } of windows) {
  if (!/^0x[0-9a-f]{1,16}$/i.test(id)) throw new Error('Invalid X11 window ID');
  const owner = /^_NET_WM_PID\(CARDINAL\) = (\d+)\s*$/m.exec(properties);
  const title = /^_NET_WM_NAME\(UTF8_STRING\) = "Connect to a Dilla instance"\s*$/m.test(properties)
   || /^WM_NAME\(STRING\) = "Connect to a Dilla instance"\s*$/m.test(properties);
  if (owner && Number(owner[1]) === pid && title) matches.push(id);
 }
 if (matches.length > 1) throw new Error('Ambiguous owned Connect dialogs');
 return matches[0];
}

export async function confirmOwnedConnect(pid) {
 const deadline = Date.now() + 15000;
 while (Date.now() < deadline) {
  const list = execFileSync('xprop', ['-root', '_NET_CLIENT_LIST'], { encoding: 'utf8', timeout: 3000 });
  const ids = [...list.matchAll(/\b0x[0-9a-f]+\b/gi)].map(match => match[0]);
  const windows = ids.map(id => ({ id, properties: execFileSync('xprop', ['-id', id, '_NET_WM_PID', '_NET_WM_NAME', 'WM_NAME'], { encoding: 'utf8', timeout: 3000 }) }));
  const id = selectConnectDialog(windows, pid);
  if (id) {
   // Keep the normal native confirmation, targeting only the matching owned
   // X11 window on this test's private display. Tab moves from the default
   // Cancel button to Connect; a successful client flow is asserted afterward.
   execFileSync('python3', ['-c', `
import ctypes as c, sys
x=c.CDLL('libX11.so.6'); t=c.CDLL('libXtst.so.6')
x.XOpenDisplay.restype=c.c_void_p
x.XSetInputFocus.argtypes=[c.c_void_p,c.c_ulong,c.c_int,c.c_ulong]
x.XStringToKeysym.argtypes=[c.c_char_p]; x.XStringToKeysym.restype=c.c_ulong
x.XKeysymToKeycode.argtypes=[c.c_void_p,c.c_ulong]; x.XKeysymToKeycode.restype=c.c_ubyte
t.XTestFakeKeyEvent.argtypes=[c.c_void_p,c.c_uint,c.c_int,c.c_ulong]
x.XSync.argtypes=[c.c_void_p,c.c_int]; x.XCloseDisplay.argtypes=[c.c_void_p]
d=x.XOpenDisplay(None)
if not d: raise RuntimeError('No private X display')
x.XSetInputFocus(d,int(sys.argv[1],16),1,0); x.XSync(d,0)
for key in (b'Tab',b'Return'):
 code=x.XKeysymToKeycode(d,x.XStringToKeysym(key))
 if not code: raise RuntimeError('Missing X11 keycode')
 t.XTestFakeKeyEvent(d,code,1,0); t.XTestFakeKeyEvent(d,code,0,0); x.XSync(d,0)
x.XCloseDisplay(d)
`, id], { timeout: 5000, stdio: 'ignore' });
   return;
  }
  await new Promise(resolve => setTimeout(resolve, 150));
 }
 throw new Error('Owned native Connect confirmation was not observed');
}
