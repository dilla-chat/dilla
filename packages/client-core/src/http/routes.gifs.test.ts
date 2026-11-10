import { describe, expect, it } from 'vitest';
import { encode } from '../cbor';
import { RecordingClient, bytesReply, cborReply, fakeTransport, noContent, refusal } from './fake-fetch';
import { GIF_FULL_MAX_BYTES, GIF_PREVIEW_MAX_BYTES, Routes } from './routes';

const fill = (n: number, b: number): Uint8Array => new Uint8Array(n).fill(b);
const COMMUNITY = fill(16, 0x04);
const CID = '04'.repeat(16);
const INSTANCE_DOC = [[1], [1], [1], fill(16, 1), 7, 'dilla.test', 0, [0, 3], 2, fill(16, 8), fill(32, 9)];

function setup() {
  const t = fakeTransport({});
  const http = new RecordingClient(t.deps);
  return { t, http, routes: new Routes(http) };
}

describe('GET /v1/instance: no gifs element (the Giphy key is a per-server setting)', () => {
  it('decodes the eleven-element document without a gifs field', async () => {
    const { t, routes } = setup();
    t.replies.push(cborReply(200, INSTANCE_DOC));
    expect(await routes.getInstance()).not.toHaveProperty('gifs');
  });
});

describe('the per-server setting', () => {
  it('reads whether the server has a key and its rating', async () => {
    const { t, http, routes } = setup();
    t.replies.push(cborReply(200, [1, 'pg']), cborReply(200, [0, 'pg-13']));
    expect(await routes.gifStatus(COMMUNITY)).toEqual({ on: true, rating: 'pg' });
    expect(await routes.gifStatus(COMMUNITY)).toEqual({ on: false, rating: 'pg-13' });
    expect(t.seen[0].url).toBe(`https://dilla.test/v1/communities/${CID}/gifs`);
    expect(t.seen[0].method).toBe('GET');
    expect(http.requests[0].bucket).toBe('read');
  });

  it('refuses a rating it does not know and a gifs flag that is not 0 or 1', async () => {
    const { t, routes } = setup();
    t.replies.push(cborReply(200, [1, 'x']), cborReply(200, [2, 'pg']));
    await expect(routes.gifStatus(COMMUNITY)).rejects.toMatchObject({ code: 'E_CBOR_RANGE' });
    await expect(routes.gifStatus(COMMUNITY)).rejects.toMatchObject({ code: 'E_CBOR_RANGE' });
  });

  it('sets the key with PUT [key], once, on the write bucket, and answers nothing', async () => {
    const { t, http, routes } = setup();
    t.replies.push(noContent());
    await routes.gifSetKey(COMMUNITY, 'abcdEFGH1234');
    expect(t.seen[0].method).toBe('PUT');
    expect(t.seen[0].url).toBe(`https://dilla.test/v1/communities/${CID}/gifs/key`);
    expect(t.seen[0].body).toEqual(encode(['abcdEFGH1234']));
    expect(http.requests[0].bucket).toBe('write');
    expect(http.requests[0].idempotent).toBe(true);
  });

  it('refuses a key the instance would refuse, before sending it anywhere', async () => {
    const { t, routes } = setup();
    for (const bad of ['', 'short', 'has space 1234', 'abcdefgh&x=1', 'a'.repeat(65), 'abcdefgh\n12', 'abcdefghé123']) {
      await expect(routes.gifSetKey(COMMUNITY, bad)).rejects.toThrow('E_ROUTE_INPUT');
    }
    expect(t.seen).toHaveLength(0);
  });

  it('never puts the key in an error message', async () => {
    const { t, routes } = setup();
    t.replies.push(refusal(403, 'E_FORBIDDEN', 'manage community'));
    const key = 'sekretKEY1234567';
    const error = await routes.gifSetKey(COMMUNITY, key).catch((e: unknown) => e);
    expect(String(error)).not.toContain(key);
    expect(JSON.stringify(error, Object.getOwnPropertyNames(error as object))).not.toContain(key);
  });

  it('removes the key with DELETE and sets the rating with PUT [rating]', async () => {
    const { t, routes } = setup();
    t.replies.push(noContent(), noContent());
    await routes.gifClearKey(COMMUNITY);
    await routes.gifSetRating(COMMUNITY, 'g');
    expect(t.seen.map(s => [s.method, s.url])).toEqual([
      ['DELETE', `https://dilla.test/v1/communities/${CID}/gifs/key`],
      ['PUT', `https://dilla.test/v1/communities/${CID}/gifs/rating`],
    ]);
    expect(t.seen[1].body).toEqual(encode(['g']));
    await expect(routes.gifSetRating(COMMUNITY, 'x-rated' as 'g')).rejects.toThrow('E_ROUTE_INPUT');
  });
});

describe('GET …/gifs/search', () => {
  it('sends q and offset to the community, on the read bucket, and reads ids, titles, sizes and the next offset', async () => {
    const { t, http, routes } = setup();
    t.replies.push(cborReply(200, [[['abc123', 'cat typing', 200, 113, 'x'], ['def456', '', 200, 200, 'y']], 48]));
    const page = await routes.gifSearch(COMMUNITY, 'cat typing & more', 24);
    expect(page).toEqual({ items: [{ id: 'abc123', title: 'cat typing', width: 200, height: 113 },
      { id: 'def456', title: '', width: 200, height: 200 }], next: 48 });
    expect(t.seen[0].url).toBe(`https://dilla.test/v1/communities/${CID}/gifs/search?q=cat%20typing%20%26%20more&offset=24`);
    expect(t.seen[0].socket).toBe(true); // a socket request carries no headers: the connection is the session
    expect(t.seen[0].headers.get('authorization')).toBeNull();
    expect(http.requests[0].bucket).toBe('read');
  });

  it('keeps a preview size only as small positive integers: a size out of range or half a size reads as 0 by 0', async () => {
    const { t, routes } = setup();
    t.replies.push(cborReply(200, [[['a1', '', 200, 113, 'x'], ['a2', '', 0, 113, 'x'], ['a3', '', 200, 4001, 'x'], ['a4', '', 4000, 4000, 'x'], ['a5', '', 5_000_000, 1, 'x']], null]));
    const page = await routes.gifSearch(COMMUNITY, '', 0);
    expect(page.items.map(i => [i.id, i.width, i.height])).toEqual([['a1', 200, 113], ['a2', 0, 0], ['a3', 0, 0], ['a4', 4000, 4000], ['a5', 0, 0]]);
  });

  it('reads a null next offset as the end, and ignores any URL in the answer', async () => {
    const { t, routes } = setup();
    t.replies.push(cborReply(200, [[['abc123', 't', 1, 1, 'https://media.giphy.com/x.gif']], null]));
    const page = await routes.gifSearch(COMMUNITY, '', 0);
    expect(page.next).toBeNull();
    expect(JSON.stringify(page)).not.toContain('giphy.com');
    expect(t.seen[0].url).toBe(`https://dilla.test/v1/communities/${CID}/gifs/search?q=&offset=0`);
  });

  it('drops a result whose id is not letters and digits, rather than building a path from it', async () => {
    const { t, routes } = setup();
    t.replies.push(cborReply(200, [[['../x', 't', 1, 1, ''], ['ok1', 't', 1, 1, '']], null]));
    expect((await routes.gifSearch(COMMUNITY, 'x', 0)).items.map(i => i.id)).toEqual(['ok1']);
  });

  it('refuses a query over 100 bytes, a control character and a bad offset before sending', async () => {
    const { t, routes } = setup();
    await expect(routes.gifSearch(COMMUNITY, 'a'.repeat(101), 0)).rejects.toThrow('E_ROUTE_INPUT');
    await expect(routes.gifSearch(COMMUNITY, 'é'.repeat(51), 0)).rejects.toThrow('E_ROUTE_INPUT');
    await expect(routes.gifSearch(COMMUNITY, 'a\u0000b', 0)).rejects.toThrow('E_ROUTE_INPUT');
    await expect(routes.gifSearch(COMMUNITY, 'a', -1)).rejects.toThrow('E_ROUTE_INPUT');
    await expect(routes.gifSearch(COMMUNITY, 'a', 5000)).rejects.toThrow('E_ROUTE_INPUT');
    await expect(routes.gifSearch(COMMUNITY, 'a', 1.5)).rejects.toThrow('E_ROUTE_INPUT');
    expect(t.seen).toHaveLength(0);
  });

  it('carries the instance refusal (no key on this server: 404) to the caller', async () => {
    const { t, routes } = setup();
    t.replies.push(refusal(404, 'E_NOT_FOUND', 'no such object'));
    await expect(routes.gifSearch(COMMUNITY, 'x', 0)).rejects.toMatchObject({ status: 404, code: 'E_NOT_FOUND' });
  });

  it('keeps the wait the instance asks for on a 503, so the caller can back off', async () => {
    const { t, routes } = setup();
    // The transport itself retries a busy read a few times, then gives the last refusal.
    for (let i = 0; i < 5; i++) t.replies.push(refusal(503, 'E_UNAVAILABLE', 'busy', 1000));
    await expect(routes.gifSearch(COMMUNITY, 'x', 0)).rejects.toMatchObject({ status: 503, retryAfterMs: 1000 });
  });
});

describe('GET …/gifs/media/{gif_id}', () => {
  const GIF = new Uint8Array([0x47, 0x49, 0x46, 0x38, 0x39, 0x61, 0, 0]);

  it('fetches the preview or the full GIF of a community by id and variant and returns the bytes', async () => {
    const { t, http, routes } = setup();
    t.replies.push(bytesReply(200, GIF, { 'Content-Type': 'image/gif' }), bytesReply(200, GIF, { 'Content-Type': 'image/gif' }));
    expect(await routes.gifMedia(COMMUNITY, 'abc123', 'preview')).toEqual(GIF);
    expect(await routes.gifMedia(COMMUNITY, 'abc123', 'full')).toEqual(GIF);
    expect(t.seen.map(s => s.url)).toEqual([`https://dilla.test/v1/communities/${CID}/gifs/media/abc123?r=preview`,
      `https://dilla.test/v1/communities/${CID}/gifs/media/abc123?r=full`]);
    expect(http.requests[0].bucket).toBe('read');
  });

  it('caps what it reads at 5 MiB for a preview and 25 MiB for a full GIF', async () => {
    const { t, http, routes } = setup();
    t.replies.push(bytesReply(200, GIF), bytesReply(200, GIF));
    await routes.gifMedia(COMMUNITY, 'a', 'preview');
    await routes.gifMedia(COMMUNITY, 'a', 'full');
    expect(http.requests[0].maxBodyBytes).toBe(GIF_PREVIEW_MAX_BYTES);
    expect(http.requests[1].maxBodyBytes).toBe(GIF_FULL_MAX_BYTES);
    expect(GIF_PREVIEW_MAX_BYTES).toBe(5 * 1024 * 1024);
    expect(GIF_FULL_MAX_BYTES).toBe(25 * 1024 * 1024);
  });

  it('refuses a bad id or variant before sending', async () => {
    const { t, routes } = setup();
    for (const bad of ['', '../x', 'a/b', 'a.b', 'a:80', 'a b', 'x'.repeat(65)]) {
      await expect(routes.gifMedia(COMMUNITY, bad, 'full')).rejects.toThrow('E_ROUTE_INPUT');
    }
    await expect(routes.gifMedia(COMMUNITY, 'abc', 'original' as 'full')).rejects.toThrow('E_ROUTE_INPUT');
    expect(t.seen).toHaveLength(0);
  });

  it('refuses a body that does not start with the GIF magic', async () => {
    const { t, routes } = setup();
    t.replies.push(bytesReply(200, encode(['not a gif'])));
    await expect(routes.gifMedia(COMMUNITY, 'abc', 'full')).rejects.toMatchObject({ status: 503, code: 'E_UNAVAILABLE' });
  });
});
