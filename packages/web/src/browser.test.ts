// Wave B task 3b (Jonas): "Save as file" for the recovery key goes through browser.download.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { browser } from './browser.ts';

describe('browser.download', () => {
  afterEach(() => { vi.restoreAllMocks(); });
  it('hands the text to the browser as one file, then revokes the object URL at once', async () => {
    const blobs: Blob[] = [];
    const create = vi.fn((b: Blob) => { blobs.push(b); return 'blob:dilla/1'; });
    const revoke = vi.fn();
    Object.defineProperty(URL, 'createObjectURL', { value: create, configurable: true });
    Object.defineProperty(URL, 'revokeObjectURL', { value: revoke, configurable: true });
    const clicks: { href: string; download: string }[] = [];
    vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(function (this: HTMLAnchorElement) {
      clicks.push({ href: this.getAttribute('href') ?? '', download: this.download });
    });
    browser.download('dilla-recovery-key-dilla.test.txt', 'hello\nKEY\n');
    expect(clicks).toEqual([{ href: 'blob:dilla/1', download: 'dilla-recovery-key-dilla.test.txt' }]);
    expect(revoke).toHaveBeenCalledWith('blob:dilla/1');
    expect(revoke.mock.invocationCallOrder[0]).toBeGreaterThan(create.mock.invocationCallOrder[0]);
    expect(blobs).toHaveLength(1);
    expect(blobs[0].type).toBe('text/plain;charset=utf-8');
    const read = await new Promise<string>((resolve) => { const r = new FileReader(); r.onload = () => { resolve(typeof r.result === 'string' ? r.result : ''); }; r.readAsText(blobs[0]); });
    expect(read).toBe('hello\nKEY\n');
    // Nothing of it is left in the page.
    expect(document.querySelector('a[download]')).toBeNull();
  });
  it('revokes the URL even when the click throws', () => {
    const revoke = vi.fn();
    Object.defineProperty(URL, 'createObjectURL', { value: () => 'blob:dilla/2', configurable: true });
    Object.defineProperty(URL, 'revokeObjectURL', { value: revoke, configurable: true });
    vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => { throw new Error('blocked'); });
    expect(() => browser.download('f.txt', 'x')).toThrow('blocked');
    expect(revoke).toHaveBeenCalledWith('blob:dilla/2');
  });
});
