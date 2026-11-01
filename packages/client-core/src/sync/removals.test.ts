import { describe, expect, it, vi } from 'vitest';
import { encode } from '../cbor';
import { type CorePort } from '../core-port';
import { encodeRemovalProof } from '../bot/protocol';
import { recoverModerationRemovals } from './removals';

const id = (n: number) => new Uint8Array(16).fill(n);
const GROUP = id(1);
const ACTOR = id(2);
const LIST = new Uint8Array([1, 2, 3]);
const SIG = new Uint8Array(64).fill(4);
const HASH = new Uint8Array(32).fill(5);
const rawList = encode([1, LIST, SIG, HASH]);
const proof = encodeRemovalProof({ requestId: id(6), actorUser: ACTOR, actorDevice: id(7), communityId: id(8), groupId: GROUP,
  targetSeq: 9n, expires: 30n });

describe('moderation removal recovery', () => {
  it('refreshes the proof actor history and persists tombstones before returning for timeline publication', async () => {
    const order: string[] = [];
    const entry = { revision: 1n, proof, signature: SIG, acceptedAt: 20n };
    const ingest = vi.fn((_group: Uint8Array, value: { revision: bigint }) => { order.push('ingest'); return value.revision; });
    const core = {
      removalCursor: vi.fn(() => 0n),
      removalActorHistoryVersion: vi.fn(() => 1n),
      refreshRemovalActorHistory: vi.fn(() => { order.push('refresh'); return 2n; }),
      ingestRemoval: ingest,
    } as unknown as CorePort;
    const routes = {
      getModerationRemovals: vi.fn(() => Promise.resolve({ rows: [entry], nextRevision: 1n })),
      getDeviceList: vi.fn(() => Promise.resolve({ version: 1n, blob: LIST, raw: rawList })),
      getDeviceListHistory: vi.fn(),
    } as never;
    await recoverModerationRemovals(core, routes, GROUP);
    order.push('publish');
    expect(order).toEqual(['refresh', 'ingest', 'publish']);
    expect(ingest).toHaveBeenCalledWith(GROUP, entry);
  });

  it('refuses to publish when the feed has a revision gap', async () => {
    const core = { removalCursor: () => 0n } as unknown as CorePort;
    const routes = { getModerationRemovals: () => Promise.resolve({ rows: [{ revision: 2n, proof, signature: SIG, acceptedAt: 20n }], nextRevision: 2n }) } as never;
    await expect(recoverModerationRemovals(core, routes, GROUP)).rejects.toThrow(/revision range/);
  });
});

// Task S1 fix round 2: no attestation outcome stalls the removal feed. The actor's wire-4 head carries an attestation the
// core refuses or stages; the actor history refresh and the removal ingest still run.
describe('moderation removal recovery on a wire-4 head', () => {
  for (const outcome of ['staged', 'refused'] as const) {
    it(`refreshes the actor and ingests the removal when the attestation is ${outcome}`, async () => {
      const order: string[] = [];
      const entry = { revision: 1n, proof, signature: SIG, acceptedAt: 20n };
      const attestation = encode([1, id(3), 0, new Uint8Array(64).fill(9)]);
      const core = {
        removalCursor: vi.fn(() => 0n),
        removalActorHistoryVersion: vi.fn(() => 1n),
        accountKindIngest: vi.fn(() => {
          order.push('kind');
          if (outcome === 'refused') throw new Error('E_CORE_INPUT');
        }),
        refreshRemovalActorHistory: vi.fn(() => { order.push('refresh'); return 2n; }),
        ingestRemoval: vi.fn((_group: Uint8Array, value: { revision: bigint }) => { order.push('ingest'); return value.revision; }),
      } as unknown as CorePort;
      const routes = {
        getModerationRemovals: vi.fn(() => Promise.resolve({ rows: [entry], nextRevision: 1n })),
        getDeviceList: vi.fn(() => Promise.resolve({ version: 1n, blob: LIST, raw: encode([1, LIST, SIG, HASH, attestation]) })),
        getDeviceListHistory: vi.fn(),
      } as never;
      await recoverModerationRemovals(core, routes, GROUP);
      expect(order).toEqual(['kind', 'refresh', 'ingest']);
    });
  }
});
