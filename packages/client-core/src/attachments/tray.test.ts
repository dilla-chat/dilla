import { describe, expect, it, vi } from 'vitest';
import { toHex } from '../hex';
import { DillaHttpError } from '../http/errors';
import { BROWSER_ATTACHMENT_CAP, openBlob, openThumb } from './crypto';
import type { ThumbDeps } from './thumb';
import { Tray, type TrayEntry } from './tray';

const CHANNEL = new Uint8Array(16).fill(0xc1);

/** A File-shaped object whose reads are counted, so a test can prove nothing was read. */
function spyFile(name: string, size: number, type: string): { file: File; read: ReturnType<typeof vi.fn> } {
  const read = vi.fn(() => Promise.resolve(new ArrayBuffer(3)));
  return { file: { name, size, type, arrayBuffer: read } as unknown as File, read };
}

function harness(opts: { thumb?: ThumbDeps | null; failPut?: Error; failDelete?: boolean } = {}) {
  const puts: { blobId: string; stored: Uint8Array }[] = [];
  const deletes: string[] = [];
  const changes: TrayEntry[][] = [];
  let n = 0;
  const routes = {
    putBlob: (channelId: Uint8Array, blobId: Uint8Array, stored: Uint8Array): Promise<{ created: boolean; size: number }> => {
      expect(channelId).toEqual(CHANNEL);
      if (opts.failPut !== undefined && puts.length === 0) {
        puts.push({ blobId: toHex(blobId), stored });
        return Promise.reject(opts.failPut);
      }
      puts.push({ blobId: toHex(blobId), stored });
      return Promise.resolve({ created: true, size: stored.length });
    },
    deleteBlob: (_channelId: Uint8Array, blobId: Uint8Array): Promise<void> => {
      deletes.push(toHex(blobId));
      return opts.failDelete === true ? Promise.reject(new DillaHttpError({ status: 0, code: 'E_NETWORK', detail: '', retryAfterMs: null, extra: [] })) : Promise.resolve();
    },
  };
  const tray = new Tray({
    channelId: CHANNEL, routes, thumb: opts.thumb ?? null,
    random: (k) => { n += 1; return new Uint8Array(k).fill(n); },
    onChange: (entries) => { changes.push(entries.map((e) => ({ ...e }))); },
  });
  return { tray, puts, deletes, changes };
}

const text = (s: string, name = 'note.txt'): File => new File([new TextEncoder().encode(s)], name, { type: 'text/plain' });

describe('Tray (L-TS-32, ruling 7)', () => {
  it('exports only files still selected for an update checkpoint', async () => {
    const h = harness();
    const first = text('one', 'one.txt');
    const second = text('two', 'two.txt');
    const [one] = await h.tray.add([first, second]);
    expect(h.tray.files()).toEqual([first, second]);
    await h.tray.discard(one);
    expect(h.tray.files()).toEqual([second]);
  });
  it('takes a file through reading, preparing, uploading and ready, and its descriptor opens the uploaded bytes', async () => {
    const h = harness();
    const [id] = await h.tray.add([text('hello tray')]);
    expect(id).toBe('1');
    expect(h.changes.map((c) => c[0]?.phase)).toEqual(['reading', 'preparing', 'uploading', 'ready']);
    const entry = h.tray.entries()[0];
    expect(entry).toMatchObject({ id: '1', name: 'note.txt', size: 10, mime: 'text/plain', image: false, phase: 'ready', reason: '' });
    const d = entry.descriptor!;
    expect(d).toMatchObject({ size: 10, mime: 'text/plain', w: null, h: null, thumb: null, name: 'note.txt' });
    expect(d.key).toEqual(new Uint8Array(32).fill(1));
    expect(d.nonce).toEqual(new Uint8Array(12).fill(2));
    expect(h.puts).toHaveLength(1);
    expect(h.puts[0].blobId).toBe(toHex(d.blobId));
    expect(new TextDecoder().decode(await openBlob(d, h.puts[0].stored))).toBe('hello tray');
  });

  it('refuses more than four files in the tray before reading any', async () => {
    const h = harness();
    await h.tray.add([text('a'), text('b'), text('c')]);
    const extra = [spyFile('d', 1, 'text/plain'), spyFile('e', 1, 'text/plain')];
    await expect(h.tray.add(extra.map((x) => x.file))).rejects.toMatchObject({ code: 'E_ATTACHMENT_COUNT' });
    expect(extra.map((x) => x.read.mock.calls.length)).toEqual([0, 0]);
    expect(h.tray.entries()).toHaveLength(3);
    const five = Array.from({ length: 5 }, (_, i) => spyFile(String(i), 1, 'text/plain'));
    await expect(harness().tray.add(five.map((x) => x.file))).rejects.toMatchObject({ code: 'E_ATTACHMENT_COUNT' });
    expect(five.every((x) => x.read.mock.calls.length === 0)).toBe(true);
  });

  it('retains an oversize failed card without reading it, uploads its valid sibling and admits exactly the cap', async () => {
    const h = harness();
    const over = spyFile('big.bin', BROWSER_ATTACHMENT_CAP + 1, 'application/octet-stream');
    const small = spyFile('small.txt', 1, 'text/plain');
    await h.tray.add([small.file, over.file]);
    expect(h.tray.entries()).toEqual([expect.objectContaining({ name: 'small.txt', phase: 'ready' }), expect.objectContaining({ name: 'big.bin', phase: 'failed', reason: 'E_ATTACHMENT_TOO_LARGE' })]);
    expect([small.read.mock.calls.length, over.read.mock.calls.length]).toEqual([1, 0]);
    expect(h.puts).toHaveLength(1);
    expect(() => h.tray.take(['2'])).toThrow('E_TRAY_NOT_READY');
    const exact = spyFile('exact.bin', BROWSER_ATTACHMENT_CAP, 'application/octet-stream');
    await h.tray.add([exact.file]);
    expect(exact.read).toHaveBeenCalledTimes(1);
  });

  it('seals a thumbnail for an image when thumbnail deps are given, and none without them', async () => {
    const deps: ThumbDeps = {
      createImageBitmap: () => Promise.resolve({ width: 640, height: 480, close: () => undefined } as unknown as ImageBitmap),
      offscreen: (w, hgt) => ({ width: w, height: hgt, getContext: () => ({ drawImage: () => undefined }),
        convertToBlob: () => Promise.resolve(new Blob([new Uint8Array(100).fill(7)], { type: 'image/webp' })) }) as unknown as OffscreenCanvas,
    };
    const png = new File([new Uint8Array(20)], 'map.png', { type: 'image/png' });
    const withThumb = harness({ thumb: deps });
    await withThumb.tray.add([png]);
    const d = withThumb.tray.entries()[0].descriptor!;
    expect(d).toMatchObject({ w: 640, h: 480, mime: 'image/png' });
    expect(withThumb.tray.entries()[0].image).toBe(true);
    expect(await openThumb(d.key, d.nonce, d.thumb!)).toEqual(new Uint8Array(100).fill(7));
    const without = harness();
    await without.tray.add([png]);
    expect(without.tray.entries()[0].descriptor).toMatchObject({ w: null, h: null, thumb: null });
  });

  it('marks a failed upload with its code and goes on with the next file', async () => {
    const h = harness({ failPut: new DillaHttpError({ status: 507, code: 'E_STORAGE_FULL', detail: '', retryAfterMs: null, extra: [] }) });
    const ids = await h.tray.add([text('one', 'one.txt'), text('two', 'two.txt')]);
    expect(ids).toEqual(['1', '2']);
    expect(h.tray.entries().map((e) => [e.phase, e.reason])).toEqual([['failed', 'E_STORAGE_FULL'], ['ready', '']]);
    expect(h.tray.entries()[0].descriptor).toBeNull();
  });

  it('names an untyped file octet-stream and sanitises the name', async () => {
    const h = harness();
    await h.tray.add([new File([new Uint8Array(2)], 'a/b‮.txt', { type: '' })]);
    expect(h.tray.entries()[0]).toMatchObject({ name: 'a_b.txt', mime: 'application/octet-stream', image: false });
  });

  it('discard deletes an uploaded reference, swallows a failed delete, and leaves an upload never made alone', async () => {
    const h = harness({ failDelete: true });
    const [id] = await h.tray.add([text('x')]);
    const blob = h.puts[0].blobId;
    await h.tray.discard(id);
    expect(h.deletes).toEqual([blob]);
    expect(h.tray.entries()).toEqual([]);
    await h.tray.discard(id);
    expect(h.deletes).toEqual([blob]);
    const failed = harness({ failPut: new DillaHttpError({ status: 413, code: 'E_TOO_LARGE', detail: '', retryAfterMs: null, extra: [] }) });
    const [bad] = await failed.tray.add([text('y')]);
    await failed.tray.discard(bad);
    expect(failed.deletes).toEqual([]);
  });

  it('take answers ready descriptors in the order asked and refuses a tray that is not ready, removing nothing', async () => {
    const h = harness({ failPut: new DillaHttpError({ status: 507, code: 'E_STORAGE_FULL', detail: '', retryAfterMs: null, extra: [] }) });
    const [bad, a, b] = await h.tray.add([text('bad'), text('a'), text('b')]);
    expect(() => h.tray.take([a, bad])).toThrow('E_TRAY_NOT_READY');
    expect(h.tray.entries()).toHaveLength(3);
    const taken = h.tray.take([b, a]);
    expect(taken.map((d) => toHex(d.blobId))).toEqual([h.puts[2].blobId, h.puts[1].blobId]);
    expect(h.tray.entries().map((e) => e.id)).toEqual([bad]);
    expect(() => h.tray.take(['99'])).toThrow('E_TRAY_NOT_READY');
  });

  it('refuses a duplicate tray id without consuming another ready file', async () => {
    const h = harness();
    const [a, b] = await h.tray.add([text('one'), text('two')]);
    expect(() => h.tray.take([a, a])).toThrow('E_TRAY_NOT_READY');
    expect(h.tray.entries().map((entry) => entry.id)).toEqual([a, b]);
    expect(h.tray.take([b]).map((d) => toHex(d.blobId))).toEqual([h.puts[1].blobId]);
    expect(h.tray.entries().map((entry) => entry.id)).toEqual([a]);
  });
});
