import { contextBridge, ipcRenderer } from 'electron';
import { captureId, captureOptions } from './screen-capture-contract.ts';

// This preload deliberately exposes no general DesktopBridge or IPC channel selection.
if (process.isMainFrame && location.href === 'dilla-internal://capture/') {
  let id: string | null = null;
  contextBridge.exposeInMainWorld('dillaCaptureBroker', Object.freeze({
    status: (type: unknown, capture: unknown) => {
      if (capture !== id || typeof type !== 'string' || !['started', 'heartbeat', 'ended', 'failed', 'configured'].includes(type)) return;
      ipcRenderer.send('dilla:screen-broker-status', { type, id });
    },
  }));
  ipcRenderer.on('dilla:screen-broker-start', (event, value: unknown) => {
    const data = value && typeof value === 'object' && !Array.isArray(value) ? value as Record<string, unknown> : null;
    if (id || !data || !captureId(data.id) || typeof data.sourceId !== 'string' || data.sourceId.length > 512
      || !/^(screen|window):[^\u0000-\u001f]+$/.test(data.sourceId) || event.ports.length !== 1) {
      for (const port of event.ports) port.close();
      return;
    }
    try {
      const options = captureOptions(data.options);
      id = data.id;
      window.postMessage({ type: 'dilla-screen-broker-start', id, sourceId: data.sourceId, options }, location.origin, [event.ports[0]]);
    } catch { for (const port of event.ports) port.close(); }
  });
  ipcRenderer.on('dilla:screen-broker-configure', (_event, value: unknown) => {
    if (!value || typeof value !== 'object' || !('id' in value) || !('options' in value) || value.id !== id) return;
    try { window.postMessage({ type: 'dilla-screen-broker-configure', id, options: captureOptions(value.options) }, location.origin); }
    catch { ipcRenderer.send('dilla:screen-broker-status', { type: 'failed', id }); }
  });
}
