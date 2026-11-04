// Wave B task 3b (Jonas): "Save as file" for the recovery key goes through browser.download, which uses the attachments'
// saver (save-blob.ts): the anchor is in the document while clicked, and the object URL is revoked 10 s later
// (security review MEDIUM-1: WebKit and some Firefox builds start a blob download asynchronously).
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { browser } from './browser.ts';
import { REVOKE_SAVED_AFTER_MS } from './save-blob.ts';

const readText = (blob: Blob) => new Promise<string>((resolve) => {
  const r = new FileReader();
  r.onload = () => { resolve(typeof r.result === 'string' ? r.result : ''); };
  r.readAsText(blob);
});

describe('browser.download', () => {
  beforeEach(() => { vi.useFakeTimers(); });
  afterEach(() => { vi.useRealTimers(); vi.restoreAllMocks(); });
  it('hands the text to the browser as one file from an attached anchor, and revokes the URL 10 s later', async () => {
    const blobs: Blob[] = [];
    const create = vi.fn((b: Blob) => { blobs.push(b); return 'blob:dilla/1'; });
    const revoke = vi.fn();
    Object.defineProperty(URL, 'createObjectURL', { value: create, configurable: true });
    Object.defineProperty(URL, 'revokeObjectURL', { value: revoke, configurable: true });
    const clicks: { href: string; download: string; attached: boolean }[] = [];
    vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(function (this: HTMLAnchorElement) {
      clicks.push({ href: this.getAttribute('href') ?? '', download: this.download, attached: this.isConnected });
    });
    browser.download('dilla-recovery-key-dilla.test.txt', 'hello\nKEY\n');
    expect(clicks).toEqual([{ href: 'blob:dilla/1', download: 'dilla-recovery-key-dilla.test.txt', attached: true }]);
    expect(document.querySelector('a[download]')).toBeNull();
    expect(revoke).not.toHaveBeenCalled();
    vi.advanceTimersByTime(REVOKE_SAVED_AFTER_MS - 1);
    expect(revoke).not.toHaveBeenCalled();
    vi.advanceTimersByTime(1);
    expect(revoke).toHaveBeenCalledWith('blob:dilla/1');
    expect(blobs).toHaveLength(1);
    expect(blobs[0]?.type).toBe('text/plain;charset=utf-8');
    vi.useRealTimers();
    expect(await readText(blobs[0])).toBe('hello\nKEY\n');
  });
  it('removes the anchor and still revokes the URL when the click throws', () => {
    const revoke = vi.fn();
    Object.defineProperty(URL, 'createObjectURL', { value: () => 'blob:dilla/2', configurable: true });
    Object.defineProperty(URL, 'revokeObjectURL', { value: revoke, configurable: true });
    vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => { throw new Error('blocked'); });
    expect(() => { browser.download('f.txt', 'x'); }).toThrow('blocked');
    expect(document.querySelector('a[download]')).toBeNull();
    vi.advanceTimersByTime(REVOKE_SAVED_AFTER_MS);
    expect(revoke).toHaveBeenCalledWith('blob:dilla/2');
  });
});
