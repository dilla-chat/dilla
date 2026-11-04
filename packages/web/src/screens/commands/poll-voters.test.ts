import { describe, expect, it, vi } from 'vitest';
import { walkPollVoters } from './poll-voters.ts';
const ids = (n: number, start = 0) => Array.from({ length: n }, (_, i) => String(start + i));
describe('bounded poll voters', () => {
  it('caps one answer at ten pages and reports the lower bound', async () => {
    let page = 0; const read = vi.fn(() => { const p = ++page; return Promise.resolve({ voters: ids(20, (p - 1) * 20), nextCursor: new Uint8Array([p]) }); });
    expect(await walkPollVoters([1000n], true, read)).toMatchObject({ distinct: 200, capped: true }); expect(read).toHaveBeenCalledTimes(10);
  });
  it('shares a total ten-page budget across answers and walks at most two concurrently', async () => {
    let running = 0, max = 0, page = 0;
    const read = vi.fn(async () => { running++; max = Math.max(max, running); const p = ++page; await Promise.resolve(); running--; return { voters: ids(20, p * 20), nextCursor: new Uint8Array([p]) }; });
    const result = await walkPollVoters(Array<bigint>(10).fill(1000n), true, read);
    expect(read).toHaveBeenCalledTimes(10); expect(max).toBe(2); expect(result).toMatchObject({ distinct: 200, capped: true });
  });
  it('stops an answer at its count even if a cursor remains', async () => {
    const read = vi.fn(() => Promise.resolve({ voters: ['one'], nextCursor: new Uint8Array([1]) }));
    expect(await walkPollVoters([1n], true, read)).toMatchObject({ distinct: 1, capped: false }); expect(read).toHaveBeenCalledOnce();
  });
  it('stops the entire walk when distinct ids reach sum(counts)', async () => {
    const read = vi.fn(() => Promise.resolve({ voters: ['one', 'two', 'three'], nextCursor: new Uint8Array([1]) }));
    expect(await walkPollVoters([1n, 1n, 1n], true, read)).toMatchObject({ distinct: 3, capped: false }); expect(read).toHaveBeenCalledTimes(2);
  });
  it.each([0, 1])('rejects a cursor that goes backwards or repeats (%s)', async next => {
    let page = 0; const read = vi.fn(() => Promise.resolve({ voters: [String(page++)], nextCursor: new Uint8Array([page === 1 ? 1 : next]) }));
    await expect(walkPollVoters([100n], true, read)).rejects.toThrow('Voter pagination made no progress'); expect(read).toHaveBeenCalledTimes(2);
  });
  it('rejects an empty page with a cursor', async () => { await expect(walkPollVoters([1n], true, () => Promise.resolve({ voters: [], nextCursor: new Uint8Array([1]) }))).rejects.toThrow('Voter pagination made no progress'); });
  it('skips zero counts and keeps only first-page previews', async () => {
    const read = vi.fn((_option: number, cursor: Uint8Array | null) => Promise.resolve({ voters: cursor ? ['later'] : ids(6), nextCursor: cursor ? null : new Uint8Array([1]) }));
    const result = await walkPollVoters([7n, 0n], true, read); expect(result.previews).toEqual([ids(5), []]); expect(read).toHaveBeenCalledTimes(2);
  });
});

it('drains the other in-flight answer after an error without dispatching more pages', async () => {
  let release!: (page: { voters: string[]; nextCursor: Uint8Array | null }) => void; let settled = false;
  const read = vi.fn((option: number) => option === 0 ? Promise.resolve({ voters: [], nextCursor: new Uint8Array([1]) })
    : new Promise<{ voters: string[]; nextCursor: Uint8Array | null }>(resolve => { release = resolve; }));
  const walk = walkPollVoters([100n, 100n], true, read); void walk.catch(() => { settled = true; });
  await Promise.resolve(); await Promise.resolve(); expect(settled).toBe(false);
  release({ voters: ['one'], nextCursor: new Uint8Array([1]) });
  await expect(walk).rejects.toThrow('Voter pagination made no progress'); expect(read).toHaveBeenCalledTimes(2);
});
