import { contextBridge, ipcRenderer } from 'electron';
import { pickerSources, type ScreenPickerSource } from './screen-capture-contract.ts';

// The screen-share picker window's preload. It exposes only the picker: tiles in, one tile id or Cancel out. No DesktopBridge,
// no channel selection, and nothing at all outside the picker's own top-level document.
if (process.isMainFrame && location.href === 'dilla-internal://picker/') {
  // Main sends the first tiles at did-finish-load, before React's effect subscribes: keep the latest and replay it on subscribe.
  let latest: ScreenPickerSource[] | null = null;
  const listeners = new Set<(sources: ScreenPickerSource[]) => void>();
  ipcRenderer.on('dilla:screen-picker-sources', (_event: unknown, value: unknown) => {
    const sources = pickerSources(value);
    if (!sources) return;
    latest = sources;
    for (const listener of listeners) listener(sources);
  });
  contextBridge.exposeInMainWorld('dillaPicker', Object.freeze({
    onSources: (listener: (sources: ScreenPickerSource[]) => void) => {
      listeners.add(listener);
      if (latest) listener(latest);
      return () => { listeners.delete(listener); };
    },
    choose: (id: unknown) => { if (typeof id === 'string' && id.length <= 64) ipcRenderer.send('dilla:screen-picker-choice', { id }); },
    cancel: () => { ipcRenderer.send('dilla:screen-picker-choice', { id: null }); },
  }));
}
