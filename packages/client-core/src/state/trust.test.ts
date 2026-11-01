import { describe, expect, it, vi } from 'vitest';
import { encode, type CborInput } from '../cbor';
import { readDeviceListHistory } from './trust';
import { CoreError } from '../core-port';
import { chainRow, decodePendingLeaves, decodeTrustState, headAttestation, historyFrom, ingestAccountKind, signedHeadList } from './trust';

const id16 = (n: number): Uint8Array => new Uint8Array(16).fill(n);
const key32 = (n: number): Uint8Array => new Uint8Array(32).fill(n);
const list = (version: number): Uint8Array => encode([version, new Uint8Array([version]), new Uint8Array(64), key32(version)]);
const history = (versions: number[]): Uint8Array => encode(versions.map((v) => [v, new Uint8Array([v]), new Uint8Array(64), key32(v)]));
function routes(heads: (number | null)[], pages: Record<number, number[]>) {
  let reads = 0;
  return {
    getDeviceList: (_user: Uint8Array) => {
      void _user;
      const v = heads[Math.min(reads++, heads.length - 1)];
      return Promise.resolve(v == null ? null : { version: BigInt(v), blob: new Uint8Array([v]), raw: list(v) });
    },
    getDeviceListHistory: (_user: Uint8Array, after: bigint) => {
      void _user;
      const versions = pages[Number(after)] ?? [];
      return Promise.resolve({ raw: history(versions), count: versions.length, lastVersion: versions.length ? BigInt(versions[versions.length - 1]) : null });
    },
  };
}
it('bounds core-owned pending and trust rows before copying', () => {
  expect(decodePendingLeaves(encode([[id16(1), 0, id16(2), 0, 'unknown']]))).toMatchObject([{ leafIndex: 0, reason: 'unknown' }]);
  expect(() => decodePendingLeaves(encode(Array.from({ length: 4097 }, () => [id16(1), 0, id16(2), 0, 'unknown'])))).toThrow(CoreError);
  expect(decodeTrustState(encode([[id16(2), key32(4), 12, 0, null, null]]))).toMatchObject([{ firstSeen: 12n, verified: false, blocked: null }]);
  expect(() => decodeTrustState(encode([[id16(2), new Uint8Array(31), 12, 0, null, null]]))).toThrow(CoreError);
  expect(() => decodePendingLeaves(Uint8Array.of(0x99, 0x10, 0x01))).toThrow('limit');
});
// ABI 8: the sixth element is the stored verdict of the user's leaves still in an active group (E_UMK_CHANGED wins).
it('reads the ABI-8 blocked verdict of a trust row and refuses the ABI-7 five-element row', () => {
  for (const code of ['E_UMK_CHANGED', 'E_CREDENTIAL', 'E_DEVICE_UNLISTED', 'E_TIER_MISMATCH'] as const)
    expect(decodeTrustState(encode([[id16(2), key32(4), 12, 1, null, code]]))).toMatchObject([{ verified: true, blocked: code }]);
  expect(decodeTrustState(encode([[id16(2), key32(4), 12, 0, key32(5), 'E_UMK_CHANGED']]))[0]?.changedUmk).toEqual(key32(5));
  // A code the page has no copy for fails closed as a failed identity check, as the core's own block_code does.
  expect(decodeTrustState(encode([[id16(2), key32(4), 12, 1, null, 'E_SOMETHING_NEW']]))[0]?.blocked).toBe('E_CREDENTIAL');
  expect(() => decodeTrustState(encode([[id16(2), key32(4), 12, 0, null]]))).toThrow(CoreError);
  expect(() => decodeTrustState(encode([[id16(2), key32(4), 12, 0, null, 7]]))).toThrow(CoreError);
});
it('accepts a complete signed history ending at the unchanged head', async () => {
  const result = await readDeviceListHistory(routes([3, 3], { 0: [1, 2, 3] }), id16(2), 0n);
  expect(result.head).toEqual(list(3));
  expect(result.history).toEqual(history([1, 2, 3]));
});
it('slices a history from the version the core cached, and answers null when nothing is left or it does not decode', () => {
  expect(historyFrom(history([1, 2, 3]), 2n)).toEqual(history([2, 3]));
  expect(historyFrom(history([1, 2, 3]), 0n)).toEqual(history([1, 2, 3]));
  expect(historyFrom(history([1, 2, 3]), 4n)).toBeNull();
  expect(historyFrom(new Uint8Array([0xff]), 1n)).toBeNull();
});
it('rejects gap, empty page, short page and a 65-row page', async () => {
  for (const pages of [{ 0: [1, 3] }, { 0: [] }, { 0: [1, 2] }, { 0: Array.from({ length: 65 }, (_, i) => i + 1) }]) {
    await expect(readDeviceListHistory(routes([3, 3], pages), id16(2), 0n))
      .rejects.toMatchObject({ code: 'E_DEVICE_LIST_STALE' });
  }
});
it('retries one changed head and refuses a second change', async () => {
  await expect(readDeviceListHistory(routes([2, 3, 3, 4], { 0: [1, 2, 3] }), id16(2), 0n))
    .rejects.toMatchObject({ code: 'E_DEVICE_LIST_STALE' });
  await expect(readDeviceListHistory(routes([null], { 0: [1] }), id16(2), 0n))
    .rejects.toMatchObject({ code: 'E_DEVICE_LIST_STALE' });
});

it('retries a page that overtakes its candidate head once', async () => {
  let reads = 0;
  let pages = 0;
  const route = {
    getDeviceList: () => {
      const version = [2, 3, 3, 3][Math.min(reads++, 3)];
      return Promise.resolve({ version: BigInt(version), blob: new Uint8Array([version]), raw: list(version) });
    },
    getDeviceListHistory: () => {
      pages += 1;
      const versions = [1, 2, 3];
      return Promise.resolve({ raw: history(versions), count: 3, lastVersion: 3n });
    },
  };
  const result = await readDeviceListHistory(route, id16(2), 0n);
  expect(result.head).toEqual(list(3));
  expect(result.history).toEqual(history([1, 2, 3]));
  expect(pages).toBe(2);
});

it('uses the unchanged candidate as the cached signed anchor without paging', async () => {
  let pageCalls = 0;
  const route = {
    getDeviceList: () => Promise.resolve({ version: 3n, blob: new Uint8Array([3]), raw: list(3) }),
    getDeviceListHistory: () => {
      pageCalls += 1;
      return Promise.resolve({ raw: history([3]), count: 1, lastVersion: 3n });
    },
  };
  const result = await readDeviceListHistory(route, id16(2), 3n);
  expect(result.head).toEqual(list(3));
  expect(result.history).toEqual(history([3]));
  expect(pageCalls).toBe(0);
});

const USER = new Uint8Array(16).fill(7);
const body = (version: number): Uint8Array => new Uint8Array([version]);
const row = (version: number): CborInput[] => [version, body(version), new Uint8Array(64), new Uint8Array(32)];

describe('readDeviceListHistory', () => {
  it('rejects an oversized page before decoding its rows', async () => {
    const routes = {
      getDeviceList: vi.fn(() => Promise.resolve({ version: 1n, blob: body(1), raw: encode(row(1)) })),
      getDeviceListHistory: vi.fn(() => Promise.resolve({ raw: new Uint8Array(4 * 1024 * 1024 + 1), count: 1, lastVersion: 1n })),
    };
    await expect(readDeviceListHistory(routes as never, USER, 0n)).rejects.toMatchObject({ code: 'E_DEVICE_LIST_STALE' });
  });
  it('rejects a page whose advertised last version disagrees with its signed rows', async () => {
    const routes = {
      getDeviceList: vi.fn(() => Promise.resolve({ version: 1n, blob: body(1), raw: encode(row(1)) })),
      getDeviceListHistory: vi.fn(() => Promise.resolve({ raw: encode([row(1)]), count: 1, lastVersion: 2n })),
    };
    await expect(readDeviceListHistory(routes as never, USER, 0n)).rejects.toMatchObject({ code: 'E_DEVICE_LIST_STALE' });
  });
});

// Negative cases of the trust refresh after PR #72: the read the slices are cut from, and the slicing itself.
describe('trust refresh: the history read and its slices', () => {
  it('refuses a reordered, repeated or gapped history before anything is sliced', async () => {
    for (const [head, pages] of [
      [3, { 0: [2, 1, 3] }],
      [3, { 0: [1, 1, 2] }],
      [3, { 0: [1, 3] }],
      // Across pages: a gap, and a page that starts again below where the last one ended.
      [4, { 0: [1, 2], 2: [4] }],
      [3, { 0: [1, 2], 2: [2, 3] }],
    ] as [number, Record<number, number[]>][]) {
      await expect(readDeviceListHistory(routes([head, head], pages), id16(2), 0n), JSON.stringify(pages))
        .rejects.toMatchObject({ code: 'E_DEVICE_LIST_STALE' });
    }
    // From a cached version: the first page must start right after the version below it.
    await expect(readDeviceListHistory(routes([3, 3], { 1: [3] }), id16(2), 2n)).rejects.toMatchObject({ code: 'E_DEVICE_LIST_STALE' });
  });
  it('refuses a head below the cached version without reading a page', async () => {
    const r = routes([1, 1], { 0: [1], 1: [2] });
    const pages = vi.spyOn(r, 'getDeviceListHistory');
    await expect(readDeviceListHistory(r, id16(2), 2n)).rejects.toMatchObject({ code: 'E_DEVICE_LIST_STALE' });
    expect(pages).not.toHaveBeenCalled();
  });
  it('accepts a head exactly at the cached version: a refresh that finds nothing new', async () => {
    const result = await readDeviceListHistory(routes([2, 2], {}), id16(2), 2n);
    expect(result).toEqual({ head: list(2), history: history([2]) });
  });
  it('slices [v1, v2] at a cached v2 to the served v2 row itself, and nothing else', () => {
    expect(historyFrom(history([1, 2]), 2n)).toEqual(history([2]));
    expect(historyFrom(history([2]), 2n)).toEqual(history([2]));
  });
  it('answers null, never a partial or empty slice, for an input that leaves nothing to slice', () => {
    expect(historyFrom(encode([]), 0n)).toBeNull();
    expect(historyFrom(history([1, 2]), 3n)).toBeNull();
    // One malformed row makes the whole slice null, not the rows around it.
    expect(historyFrom(encode([[1, new Uint8Array([1]), new Uint8Array(64), key32(1)], [2, new Uint8Array([2])]]), 1n)).toBeNull();
    expect(historyFrom(encode(7), 0n)).toBeNull();
  });
});

// Task S1 part 2: a wire-4 head carries the instance-signed account-kind attestation as a fifth element.
describe('the account-kind attestation on a wire-4 head', () => {
  const ATTESTATION = encode([1, new Uint8Array(16).fill(3), 0, new Uint8Array(64).fill(4)]);
  const head4 = (version: number): Uint8Array => encode([version, new Uint8Array([version]), new Uint8Array(64), key32(version), ATTESTATION]);
  const routes4 = (version: number, pages: Record<number, number[]>) => ({
    getDeviceList: () => Promise.resolve({ version: BigInt(version), blob: new Uint8Array([version]), raw: head4(version) }),
    getDeviceListHistory: (_user: Uint8Array, after: bigint) => {
      void _user;
      const versions = pages[Number(after)] ?? [];
      return Promise.resolve({ raw: history(versions), count: versions.length, lastVersion: versions.length ? BigInt(versions[versions.length - 1]) : null });
    },
  });

  it('reads the history against the four-element chain row and keeps the full head', async () => {
    const result = await readDeviceListHistory(routes4(3, { 0: [1, 2, 3] }), id16(2), 0n);
    expect(result.history).toEqual(history([1, 2, 3]));
    expect(result.head).toEqual(head4(3));
    expect(signedHeadList(result.head)).toEqual(new Uint8Array([3]));
    expect(headAttestation(result.head)).toEqual(ATTESTATION);
    // The cached anchor path too: the candidate row in the history is the four-element row.
    const anchored = await readDeviceListHistory(routes4(3, {}), id16(2), 3n);
    expect(anchored.history).toEqual(history([3]));
  });

  // Fix round 1 (H1): enrolComplete and deviceListRevoke decode a four-element list body; a wire-4 head's attestation is
  // no part of it.
  it('chainRow is the four-element list body of a served head', () => {
    expect(chainRow(head4(3))).toEqual(list(3));
    expect(chainRow(list(3))).toEqual(list(3));
    // Anything that is not a five-element head is passed as served: the core's decoder judges it.
    expect(chainRow(encode([3, new Uint8Array([3])]))).toEqual(encode([3, new Uint8Array([3])]));
  });

  it('a four-element head has no attestation, and any other length is refused', () => {
    expect(headAttestation(list(3))).toBeNull();
    expect(signedHeadList(list(3))).toEqual(new Uint8Array([3]));
    const six = encode([3, new Uint8Array([3]), new Uint8Array(64), key32(3), ATTESTATION, 0]);
    expect(() => signedHeadList(six)).toThrow(CoreError);
    expect(() => headAttestation(six)).toThrow(CoreError);
  });

  it('hands the attestation to the core before ingest; a core without a pinned key keeps the earlier rule', () => {
    const seen: Uint8Array[] = [];
    ingestAccountKind({ accountKindIngest: (_u, a) => { seen.push(a); } }, id16(2), head4(3));
    expect(seen).toEqual([ATTESTATION]);
    ingestAccountKind({ accountKindIngest: () => { throw new Error('not called'); } }, id16(2), list(3));
    // Fix round 2: every refusal is a withheld attestation: nothing thrown, never a leaf verdict, and the caller's history
    // ingest runs (the security finding's "no steering" holds; a key change is still recorded; removals never stall).
    for (const code of ['E_CORE_STATE', 'E_CORE_INPUT', 'E_CREDENTIAL', 'E_CORE_STORAGE']) {
      expect(() => ingestAccountKind({ accountKindIngest: () => { throw new CoreError(code, 'refused'); } }, id16(2), head4(3)), code)
        .not.toThrow();
    }
    expect(() => ingestAccountKind({ accountKindIngest: () => { throw new Error('boom'); } }, id16(2), head4(3))).not.toThrow();
    const six = encode([3, new Uint8Array([3]), new Uint8Array(64), key32(3), ATTESTATION, 0]);
    expect(() => ingestAccountKind({ accountKindIngest: () => { throw new Error('not called'); } }, id16(2), six)).not.toThrow();
  });

  // Task 11 (S1 minor): the expected refusals stay quiet; anything else (a storage failure) reaches onError, still withheld.
  it('reports a refusal other than E_CORE_INPUT or E_CORE_STATE to onError, without throwing', () => {
    const logged: string[] = [];
    for (const code of ['E_CORE_STATE', 'E_CORE_INPUT', 'E_CORE_STORAGE', 'E_CREDENTIAL']) {
      ingestAccountKind({ accountKindIngest: () => { throw new CoreError(code, 'refused'); } }, id16(2), head4(3), c => logged.push(c));
    }
    ingestAccountKind({ accountKindIngest: () => { throw new Error('boom'); } }, id16(2), head4(3), c => logged.push(c));
    expect(logged).toEqual(['E_CORE_STORAGE', 'E_CREDENTIAL', 'E_CORE_UNKNOWN']);
  });
});
