import { contextBridge, ipcRenderer } from 'electron';
import { pickerSources, type ScreenPickerSource } from './screen-capture-contract.ts';

// The screen-share picker window's preload. It exposes only the picker: tiles in, one tile id or Cancel out. No DesktopBridge,
// no channel selection, and nothing at all outside the picker's own top-level document.
if (process.isMainFrame && location.href === 'dilla-internal://picker/') {
  contextBridge.exposeInMainWorld('dillaPicker', Object.freeze({
    onSources: (listener: (sources: ScreenPickerSource[]) => void) => {
      const handler = (_event: unknown, value: unknown) => { const sources = pickerSources(value); if (sources) listener(sources); };
      ipcRenderer.on('dilla:screen-picker-sources', handler);
      return () => { ipcRenderer.removeListener('dilla:screen-picker-sources', handler); };
    },
    choose: (id: unknown) => { if (typeof id === 'string' && id.length <= 64) ipcRenderer.send('dilla:screen-picker-choice', { id }); },
    cancel: () => { ipcRenderer.send('dilla:screen-picker-choice', { id: null }); },
  }));
}
