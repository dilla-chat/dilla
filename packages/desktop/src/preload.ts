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
    const bridge: DesktopBridge = {
      info: () => ipcRenderer.invoke('dilla:info'),
      send: (command: DesktopCommand) => ipcRenderer.invoke('dilla:command', command),
      browserAuth: request => ipcRenderer.invoke('dilla:browser-auth', request),
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
