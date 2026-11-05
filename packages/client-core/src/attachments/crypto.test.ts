import { readFileSync } from 'node:fs';
import { describe, expect, it } from 'vitest';
import { fromHex, toHex } from '../hex';
import {
  AAD_BLOB, AAD_THUMB, AttachmentError, BROWSER_ATTACHMENT_CAP, MAX_ATTACHMENTS, MAX_THUMB_PLAINTEXT,
  newKeyAndNonce, openBlob, openThumb, sealBlob, sealThumb, thumbNonce,
} from './crypto';

interface VectorCase { name: string; key: string; nonce: string; plaintext: string; size: number; stored: string; blob_id: string;
  thumb_nonce: string; thumb_plaintext: string | null; thumb: string | null; }
interface VectorReject { name: string; kind: 'blob' | 'thumb'; key: string; nonce: string; blob_id?: string; size: number;
  data: string; error: 'E_BLOB_HASH' | 'E_BLOB_OPEN'; }
interface Vectors { version: number; description: string; aad_blob: string; aad_thumb: string; cases: VectorCase[]; rejects: VectorReject[]; }

const vectors = JSON.parse(
  readFileSync(new URL('../../../../protocol/vectors/attachment.json', import.meta.url), 'utf8'),
) as Vectors;
const utf8 = (s: string): string => toHex(new TextEncoder().encode(s));

async function codeOf(p: Promise<unknown>): Promise<string> {
  const e = await p.then(() => null, (err: unknown) => err);
  if (!(e instanceof AttachmentError)) throw new Error(`expected an AttachmentError, got ${String(e)}`);
  return e.code;
}

describe('the attachment AEAD reproduces protocol/vectors/attachment.json (L-CORE-31, Q15)', () => {
  it('is version 1 with four cases and six rejects, under the two AAD strings', () => {
    expect(vectors.version).toBe(1);
    expect(vectors.cases).toHaveLength(4);
    expect(vectors.rejects).toHaveLength(6);
    expect(vectors.aad_blob).toBe(utf8(AAD_BLOB));
    expect(vectors.aad_thumb).toBe(utf8(AAD_THUMB));
    expect([AAD_BLOB, AAD_THUMB]).toEqual(['dilla attachment v1', 'dilla thumb v1']);
    expect([MAX_THUMB_PLAINTEXT, BROWSER_ATTACHMENT_CAP, MAX_ATTACHMENTS]).toEqual([8176, 26_214_400, 10]);
  });

  for (const c of vectors.cases) {
    it(`case: ${c.name}`, async () => {
      const key = fromHex(c.key);
      const nonce = fromHex(c.nonce);
      const sealed = await sealBlob(key, nonce, fromHex(c.plaintext));
      expect(toHex(sealed.stored)).toBe(c.stored);
      expect(toHex(sealed.blobId)).toBe(c.blob_id);
      expect(sealed.stored.length).toBe(c.size + 16);
      const opened = await openBlob({ key, nonce, blobId: fromHex(c.blob_id), size: c.size }, fromHex(c.stored));
      expect(toHex(opened)).toBe(c.plaintext);
      expect(toHex(thumbNonce(nonce))).toBe(c.thumb_nonce);
      if (c.thumb !== null && c.thumb_plaintext !== null) {
        expect(toHex(await sealThumb(key, nonce, fromHex(c.thumb_plaintext)))).toBe(c.thumb);
        expect(toHex(await openThumb(key, nonce, fromHex(c.thumb)))).toBe(c.thumb_plaintext);
      } else {
        expect([c.thumb, c.thumb_plaintext]).toEqual([null, null]);
      }
    });
  }

  for (const r of vectors.rejects) {
    it(`reject: ${r.name} → ${r.error}`, async () => {
      const key = fromHex(r.key);
      const nonce = fromHex(r.nonce);
      const outcome = r.kind === 'blob'
        ? openBlob({ key, nonce, blobId: fromHex(r.blob_id ?? ''), size: r.size }, fromHex(r.data))
        : openThumb(key, nonce, fromHex(r.data));
      expect(await codeOf(outcome)).toBe(r.error);
    });
  }
});

describe('the attachment AEAD around the vectors', () => {
  const KEY = new Uint8Array(32).fill(0x61);
  const NONCE = new Uint8Array(12).fill(0x62);

  it('checks the hash before the AEAD: a wrong key under the right name is E_BLOB_OPEN, the right key under a wrong name E_BLOB_HASH', async () => {
    const { stored, blobId } = await sealBlob(KEY, NONCE, new Uint8Array([1, 2, 3]));
    expect(await codeOf(openBlob({ key: new Uint8Array(32).fill(0x63), nonce: NONCE, blobId, size: 3 }, stored))).toBe('E_BLOB_OPEN');
    const otherName = blobId.slice();
    otherName[31] ^= 0x80;
    expect(await codeOf(openBlob({ key: KEY, nonce: NONCE, blobId: otherName, size: 3 }, stored))).toBe('E_BLOB_HASH');
    expect(await codeOf(openBlob({ key: KEY, nonce: NONCE, blobId, size: 4 }, stored))).toBe('E_BLOB_OPEN');
  });

  it('derives the thumbnail nonce without touching the file nonce', () => {
    const nonce = fromHex('000102030405060708090a0b');
    expect(toHex(thumbNonce(nonce))).toBe('000102030405060708090a0a');
    expect(toHex(nonce)).toBe('000102030405060708090a0b');
  });

  it('draws 32 key bytes and then 12 nonce bytes', () => {
    const asked: number[] = [];
    const out = newKeyAndNonce((n) => { asked.push(n); return new Uint8Array(n).fill(n); });
    expect(asked).toEqual([32, 12]);
    expect(out).toEqual({ key: new Uint8Array(32).fill(32), nonce: new Uint8Array(12).fill(12) });
    expect(() => newKeyAndNonce((n) => new Uint8Array(n - 1))).toThrow(RangeError);
  });

  it('refuses a thumbnail plaintext over 8 176 bytes and accepts exactly 8 176', async () => {
    await expect(sealThumb(KEY, NONCE, new Uint8Array(8177))).rejects.toThrow('E_ENVELOPE_LIMIT');
    expect((await sealThumb(KEY, NONCE, new Uint8Array(8176))).length).toBe(8192);
  });

  it('refuses a thumbnail shorter than the tag', async () => {
    expect(await codeOf(openThumb(KEY, NONCE, new Uint8Array(15)))).toBe('E_BLOB_OPEN');
  });
});
