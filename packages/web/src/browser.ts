/** The browser actions the page triggers, behind one object so tests can replace them. */
export const browser = {
  reload(): void { window.location.reload(); },
  print(): void { window.print(); },
  /** Wave B task 3b (Jonas): "Save as file" for the recovery key. A text file the person places where they choose; the Blob's
   *  object URL is revoked at once, and nothing of it is kept, stored or logged here. */
  download(fileName: string, text: string): void {
    const url = URL.createObjectURL(new Blob([text], { type: 'text/plain;charset=utf-8' }));
    try {
      const a = document.createElement('a');
      a.href = url;
      a.download = fileName;
      a.rel = 'noopener';
      a.click();
    } finally {
      URL.revokeObjectURL(url);
    }
  },
  /** StorageManager.persist() is exposed on Window only, so the page asks, not the worker (L-TS-09). */
  persistStorage(): Promise<boolean> { return navigator.storage?.persist?.() ?? Promise.resolve(false); },
};
