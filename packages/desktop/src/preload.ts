import { contextBridge, ipcRenderer } from 'electron';
import type { DesktopBridge, DesktopCommand, DesktopEvent } from './contract.ts';

if (process.isMainFrame) {
  if (location.protocol === 'dilla-internal:') {
    contextBridge.exposeInMainWorld('dillaManager', Object.freeze({
      platform: process.platform,
      menu: () => ipcRenderer.invoke('dilla:manager', { type: 'window-menu' }),
      list: () => ipcRenderer.invoke('dilla:manager', { type: 'list' }),
      connect: (instance: string) => ipcRenderer.invoke('dilla:manager', { type: 'connect', instance }),
    }));
  } else {
    const audioListeners = new Map<string, (samples: Float32Array) => void>();
    ipcRenderer.on('dilla:pcm', (_event, id: string, samples: Float32Array) => {
      try { if (samples instanceof Float32Array && samples.length === 960) audioListeners.get(id)?.(samples); }
      finally { ipcRenderer.send('dilla:pcm-ack', id); }
    });
    ipcRenderer.on('dilla:screen-port', (event, value: unknown) => {
      const id = value && typeof value === 'object' && 'id' in value ? value.id : null;
      if (typeof id !== 'string' || !/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(id) || event.ports.length !== 1) {
        for (const port of event.ports) port.close();
        return;
      }
      window.postMessage({ type: 'dilla-screen-capture-port', id }, location.origin, [event.ports[0]]);
    });
    const bridge: DesktopBridge = {
      // The OS this shell runs on, fixed here: the page reads it to label its own device "Desktop · macOS". A display hint
      // only, never a trust signal (the device's custody tier, 1 for this app, is in its signed credential).
      platform: process.platform,
      info: () => ipcRenderer.invoke('dilla:info'),
      send: (command: DesktopCommand) => ipcRenderer.invoke('dilla:command', command),
      browserAuth: request => ipcRenderer.invoke('dilla:browser-auth', request),
      requestScreenCapture: options => ipcRenderer.invoke('dilla:screen-capture', options),
      requestAppAudio: () => ipcRenderer.invoke('dilla:app-audio'),
      onAppAudio: (id, listener) => {
        audioListeners.set(id, listener);
        return () => { if (audioListeners.get(id) === listener) audioListeners.delete(id); };
      },
      onEvent: (listener: (event: DesktopEvent) => void) => {
        const handler = (_event: Electron.IpcRendererEvent, data: DesktopEvent) => listener(data);
        ipcRenderer.on('dilla:event', handler);
        return () => { ipcRenderer.removeListener('dilla:event', handler); };
      },
    };
    contextBridge.exposeInMainWorld('dillaDesktop', Object.freeze(bridge));
  }
}
