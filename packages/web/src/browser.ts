import { saveBlob } from './save-blob.ts';

/** The browser actions the page triggers, behind one object so tests can replace them. */
export const browser = {
  reload(): void { window.location.reload(); },
  print(): void { window.print(); },
  /** Wave B task 3b (Jonas): "Save as file" for the recovery key, through the attachments' saver (save-blob.ts): a text
   *  file handed to the browser's download, its object URL revoked 10 s later. Nothing of it is kept, stored or logged. */
  download(fileName: string, text: string): void {
    saveBlob(new Blob([text], { type: 'text/plain;charset=utf-8' }), fileName);
  },
  /** StorageManager.persist() is exposed on Window only, so the page asks, not the worker (L-TS-09). */
  persistStorage(): Promise<boolean> { return navigator.storage?.persist?.() ?? Promise.resolve(false); },
};
