import { describe, expect, it } from 'vitest';
import { arr, bin, decode, encode, type CborInput } from '../../cbor';
import type { AttachmentDescriptor, ExpectedGroup, Id, SendRequest, TimelineRow } from '../../core-port';
import { CHANNEL, CHANNEL_2, COMMUNITY, FOLD_TARGET, FOLD_UNHELD, ME, ModelCore, ModelDs, PEER, THIRD, at, idOf, wire, textRequest, type Peer } from './model';

import { toHex } from '../../hex';
import type { Frame } from '../../gateway/frames';
import { fakeDskPub, fakeListBlob } from '../../testing/fake-list';

const bytes = (n: number, value: number): Uint8Array => new Uint8Array(n).fill(value);
/** ABI 8 (fixA F3): the own row is always present, this device's own credential key, verified, never changed. */
const ownRow = (blocked: string | null = null) =>
  ({ userId: ME.user, umk: fakeDskPub(ME.user), firstSeen: 0n, verified: true, changedUmk: null, blocked });
const signed = (v: number, user = PEER.user): [number, Uint8Array, Uint8Array, Uint8Array] =>
  [v, fakeListBlob(user, [{ deviceId: PEER.device, revokedAt: null, dskPub: fakeDskPub(PEER.device) }], BigInt(v)),
    bytes(64, v), bytes(32, v - 1)];
it('matches client_trust gap and cross-user refusals without a pin', () => {
  // Parity authority: core/dilla-core/src/client/trust.rs::gap_rollback_wrong_signer_cross_user_and_substitute_are_refused
  // and core/dilla-core/tests/client_trust.rs::another_users_requested_id_never_creates_a_pin.
  const group = idOf(0xa0, 1);
  const model = new ModelCore(ME);
  model.addPendingLeaf(group, 0, PEER.user, PEER.device, fakeDskPub(PEER.device), 1);
  expect(() => model.deviceListIngest(group, 0, PEER.user, encode([signed(1), signed(3)]), signed(3)[1], 10n))
    .toThrow('E_DEVICE_LIST_STALE');
  expect(() => model.deviceListIngest(group, 0, PEER.user, encode([signed(1, ME.user)]), signed(1, ME.user)[1], 10n))
    .toThrow('E_CREDENTIAL');
  expect(model.trustState()).toEqual([ownRow()]);
});

it('pins a leaf first listed in the final signed history head', () => {
  // Rust parity: core/dilla-core/tests/client_trust.rs::signed_revocation_retracts_earlier_text_and_search_hits
  // ingests a survivor leaf absent from v1 but present in v2.
  const group = idOf(0xa0, 4);
  const model = new ModelCore(ME);
  model.addPendingLeaf(group, 0, PEER.user, PEER.device, fakeDskPub(PEER.device), 1);
  const first = [1, fakeListBlob(PEER.user, [], 1n), bytes(64, 1), bytes(32, 0)];
  const second = signed(2);
  expect(model.deviceListIngest(group, 0, PEER.user, encode([first, second]), second[1], 10n))
    .toEqual({ version: 2n, changed: false, quarantined: 0 });
  expect(model.trustState()).toMatchObject([ownRow(), { userId: PEER.user, firstSeen: 10n, verified: false, blocked: null }]);
});

it('refuses a substituted modeled leaf key without a pin', () => {
  // Rust parity: client/trust.rs::gap_rollback_wrong_signer_cross_user_and_substitute_are_refused.
  const group = idOf(0xa0, 2);
  const model = new ModelCore(ME);
  model.addPendingLeaf(group, 0, PEER.user, PEER.device, bytes(32, 0x98), 1);
  expect(() => model.deviceListIngest(group, 0, PEER.user, encode([signed(1)]), signed(1)[1], 10n))
    .toThrow('E_CREDENTIAL');
  expect(model.trustState()).toEqual([ownRow()]);
});

it('refuses a leaf whose tier differs from the modeled list entry', () => {
  const group = idOf(0xa0, 3);
  const model = new ModelCore(ME);
  model.addPendingLeaf(group, 0, PEER.user, PEER.device, fakeDskPub(PEER.device), 0);
  expect(() => model.deviceListIngest(group, 0, PEER.user, encode([signed(1)]), signed(1)[1], 10n))
    .toThrow('E_TIER_MISMATCH');
  expect(model.trustState()).toEqual([ownRow()]);
});

it('does not truncate an overfull modeled pending-leaf set', () => {
  const model = new ModelCore(ME);
  for (let n = 0; n < 4097; n++) model.addPendingLeaf(idOf(0xa1, n), 0, PEER.user, PEER.device, fakeDskPub(PEER.device), 1);
  expect(() => model.pendingLeaves()).toThrow('E_CORE_LIMIT');
});

it('refuses a modeled list with the wrong fixture signature', () => {
  const group = idOf(0xa1, 5000);
  const model = new ModelCore(ME);
  model.addPendingLeaf(group, 0, PEER.user, PEER.device, fakeDskPub(PEER.device), 1);
  const wrong = signed(1);
  wrong[2] = bytes(64, 0);
  expect(() => model.deviceListIngest(group, 0, PEER.user, encode([wrong]), wrong[1], 10n))
    .toThrow('E_CREDENTIAL');
  expect(model.trustState()).toEqual([ownRow()]);
});

const G = idOf(0x9b, 1);
const G2 = idOf(0x9b, 2);
const G3 = idOf(0x9b, 3);
const CHANNEL_3 = idOf(0xc1, 3);

function thrown(fn: () => unknown): unknown {
  try {
    fn();
  } catch (e) {
    return e;
  }
  return undefined;
}

function registered(core: ModelCore, groupId: Id = G, channelId: Id = CHANNEL): void {
  core.groupCreate(groupId, COMMUNITY, channelId);
  core.groupRegistered(groupId, 1n);
}

function expectedFor(groupId: Id, channelId: Id = CHANNEL): ExpectedGroup {
  return { groupId, communityId: COMMUNITY, channelId, policyVersion: 1n };
}

/** A GET /v1/welcomes body, labelled truthfully; each entry is [welcome_id, group_id, epoch, commit_seq]. */
function welcomes(...rows: [bigint, Id, bigint, bigint][]): Uint8Array {
  return encode(rows.map(([id, g, epoch, seq]) => [id, g, epoch, seq, wire.welcome(g, epoch), wire.tree(g, epoch), wire.treeHash(g, epoch)]));
}

/** A welcomes item labelled epoch `label` (and that epoch's tree hash) around an arbitrary blob. */
function labelled(id: bigint, g: Id, label: bigint, blob: Uint8Array): CborInput[] {
  return [id, g, label, 1n, blob, wire.tree(g, label), wire.treeHash(g, label)];
}

function peerMsg(seq: bigint, body: string): CborInput[] {
  return [seq, 0n, PEER.device, wire.app(PEER, idOf(0x7e, Number(seq)), body), new Uint8Array(32), new Uint8Array(32), 1_700_000_000n + seq, 0];
}

function peerRow(seq: bigint, body: string): Uint8Array {
  return encode([peerMsg(seq, body)]);
}

/** A self-update commit by PEER (kind 1) framed in `framed`. */
function commitRow(seq: bigint, framed: bigint): CborInput[] {
  return [seq, framed, 1n, 1n, wire.commit(framed + 1n, PEER.device, [], [])];
}

/** The 200 bodies of GET …/info and …/tree for group g at epoch e. */
function infoAt(g: Id, e: bigint): Uint8Array {
  return encode([e, wire.info(g, e), wire.treeHash(g, e), 1n]);
}
function treeAt(g: Id, e: bigint): Uint8Array {
  return encode([e, wire.tree(g, e), wire.treeHash(g, e)]);
}

/** The private message a sendEncrypt framed (element 1 of the message body). */
function blobOf(sent: { messageBody: Uint8Array }): Uint8Array {
  return bin(at(arr(decode(sent.messageBody), 2), 1));
}

/** Registered G, then two peer commits (epoch 2) and one that cannot be processed: state 3, floor 2. */
function stoppedAtEpoch2(core: ModelCore): void {
  registered(core);
  core.groupApply(G, encode([commitRow(1n, 0n), commitRow(2n, 1n), [3n, 2n, 1n, 1n, new Uint8Array([0xff])]]), encode([]), 3n);
}

describe('ModelCore follows L-CORE-07..09', () => {
  it('sendPrepare counts the body limit in UTF-8 bytes, as the core does', () => {
    const core = new ModelCore(ME);
    registered(core);
    expect(thrown(() => core.sendPrepare(G, textRequest('€'.repeat(1334)), 1n))).toMatchObject({ code: 'E_ENVELOPE_LIMIT' }); // 4002 bytes
    expect(core.outbox(G)).toEqual([]);
    const id = core.sendPrepare(G, textRequest('€'.repeat(1333)), 1n); // 3999 bytes
    expect(core.outbox(G).map((r) => r.msgId)).toEqual([id]);
  });

  it('sendEncrypt refuses with E_CORE_STATE while a proposal is pending, and changes nothing', () => {
    const core = new ModelCore(ME);
    registered(core);
    core.groupApply(G, encode([[1n, 0n, 0n, null, wire.prop(idOf(0x5e, 1), 'add', THIRD.device)]]), encode([]), 1n);
    expect(core.group(G)?.proposalsPending).toBe(1);
    const id = core.sendPrepare(G, textRequest('held'), 1n);
    expect(thrown(() => core.sendEncrypt(id))).toMatchObject({ code: 'E_CORE_STATE', detail: 'proposals are pending' });
    expect(core.outbox(G).map((r) => r.state)).toEqual([0]);
  });

  it('sendEncrypt refuses with E_CORE_STATE while a commit is pending', () => {
    const core = new ModelCore(ME);
    registered(core);
    core.commitBuild(G, encode([]));
    expect(core.group(G)).toMatchObject({ pendingCommit: true, proposalsPending: 0 });
    const id = core.sendPrepare(G, textRequest('held'), 1n);
    expect(thrown(() => core.sendEncrypt(id))).toMatchObject({ code: 'E_CORE_STATE', detail: 'a commit is pending' });
    expect(core.outbox(G).map((r) => r.state)).toEqual([0]);
  });

  it('welcomesApply answers outcome 1 for a row in state 0, 1 or 2 and leaves it untouched', () => {
    const core = new ModelCore(ME);
    core.groupCreate(G, COMMUNITY, CHANNEL); // state 0
    core.groupJoinExternal(
      expectedFor(G2, CHANNEL_2),
      encode([0n, wire.info(G2, 0n), new Uint8Array(32), 1n]),
      encode([0n, wire.tree(G2, 0n), new Uint8Array(32)]),
    ); // state 1
    registered(core, G3, CHANNEL_3); // state 2
    const out = core.welcomesApply(welcomes([1n, G, 1n, 1n], [2n, G2, 1n, 1n], [3n, G3, 1n, 1n]), [
      expectedFor(G), expectedFor(G2, CHANNEL_2), expectedFor(G3, CHANNEL_3),
    ]);
    expect(out.map((o) => o.outcome)).toEqual([1, 1, 1]);
    expect([core.group(G)?.state, core.group(G2)?.state, core.group(G3)?.state]).toEqual([0, 1, 2]);
  });

  it('welcomesApply joins over a row in state 3 and keeps its stored rows', () => {
    const core = new ModelCore(ME);
    registered(core);
    core.groupApply(G, encode([]), peerRow(1n, 'kept'), 1n);
    core.groupApply(G, encode([[2n, 0n, 1n, 1n, new Uint8Array([0xff])]]), encode([]), 2n);
    expect(core.group(G)?.state).toBe(3);
    expect(core.welcomesApply(welcomes([4n, G, 2n, 5n]), [expectedFor(G)])).toEqual([{ welcomeId: 4n, groupId: G, outcome: 0, reason: '' }]);
    expect(core.group(G)).toMatchObject({ state: 2, epoch: 2n, nextSeq: 6n });
    expect(core.bodies(G)).toEqual(['kept']);
  });

  it('welcomesApply refuses a Welcome while another group holds the channel (outcome 2, E_CORE_STATE)', () => {
    const core = new ModelCore(ME);
    registered(core); // G holds CHANNEL
    expect(core.welcomesApply(welcomes([7n, G2, 1n, 1n]), [expectedFor(G2)])).toEqual([
      { welcomeId: 7n, groupId: G2, outcome: 2, reason: 'E_CORE_STATE' },
    ]);
    expect(core.group(G2)).toBeUndefined();
  });

  it('messageDeleted clears a stored row once and ignores a seq it does not hold', () => {
    const core = new ModelCore(ME);
    registered(core);
    core.groupApply(G, encode([]), peerRow(1n, 'bye'), 1n);
    expect(core.messageDeleted(G, 1n)).toEqual({
      state: 2, epoch: 0n, nextSeq: 2n, newSeqs: [1n], proposalsPending: 0, epochChanged: false, ownAdopted: false,
    });
    expect(core.timeline(G, 0n, 200)).toMatchObject([{ seq: 1n, status: 2, body: '' }]);
    expect(core.messageDeleted(G, 1n).newSeqs).toEqual([]);
    expect(core.messageDeleted(G, 9n).newSeqs).toEqual([]);
  });

  it('messageDeleted refuses an unknown group and a group that removed this device', () => {
    const core = new ModelCore(ME);
    expect(thrown(() => core.messageDeleted(G, 1n))).toMatchObject({ code: 'E_CORE_NOT_FOUND' });
    registered(core);
    core.groupApply(G, encode([[1n, 0n, 1n, 1n, wire.commit(1n, PEER.device, [], [ME.device])]]), encode([]), 1n);
    expect(core.group(G)?.state).toBe(4);
    expect(thrown(() => core.messageDeleted(G, 1n))).toMatchObject({ code: 'E_CORE_STATE' });
  });

  // --- amended after the core hardening (L-CORE-07..09 as amended) ---

  it('groupApply skips a live row served at a stored seq: not listed, not processed, nextSeq moves past it', () => {
    const core = new ModelCore(ME);
    registered(core);
    const m = core.sendPrepare(G, textRequest('mine'), 1n);
    core.sendEncrypt(m);
    expect(core.sendConfirm(m, encode([3n, new Uint8Array(32), 1_700_000_003n]))).toEqual({ groupId: G, seq: 3n }); // ahead of nextSeq
    const r = core.groupApply(G, encode([]), encode([peerMsg(2n, 'two'), peerMsg(3n, 'served twice')]), 3n);
    expect(r).toMatchObject({ newSeqs: [2n], nextSeq: 4n, ownAdopted: false });
    expect(core.timeline(G, 0n, 200).map((x) => [x.seq, x.status, x.body])).toEqual([[2n, 0, 'two'], [3n, 0, 'mine']]);
  });

  it('adopts a failed row and a queued row whose commitment the echo carries', () => {
    const core = new ModelCore(ME);
    registered(core);
    const failed = core.sendPrepare(G, textRequest('failed'), 1n);
    const failedBlob = blobOf(core.sendEncrypt(failed)); // stored by the server; its answer was lost
    core.sendFail(failed, 'E_NETWORK');
    const queued = core.sendPrepare(G, textRequest('queued'), 2n);
    const queuedBlob = blobOf(core.sendEncrypt(queued)); // stored too; requeued after the echo wait
    core.sendRequeue(queued);
    const r = core.groupApply(G, encode([]), encode([ModelDs.row.ownEcho(1n, 0n, failedBlob), ModelDs.row.ownEcho(2n, 0n, queuedBlob)]), 2n);
    expect(r).toMatchObject({ newSeqs: [1n, 2n], ownAdopted: true });
    expect(core.outbox(G)).toEqual([]);
    expect(core.timeline(G, 0n, 200).map((x) => [x.seq, x.status, x.msgId, x.body])).toEqual([[1n, 0, failed, 'failed'], [2n, 0, queued, 'queued']]);
  });

  it('adopts nothing on a commitment mismatch (E_OWN_UNKNOWN, the row in flight stays), then adopts by the blob alone', () => {
    const core = new ModelCore(ME);
    registered(core);
    const m = core.sendPrepare(G, textRequest('in flight'), 1n);
    const blob = blobOf(core.sendEncrypt(m));
    const unplaced = wire.app(ME, idOf(0x6d, 99), 'an upload this device cannot place');
    const r = core.groupApply(G, encode([]), encode([
      ModelDs.row.ownEcho(1n, 0n, unplaced),
      [2n, 0n, ME.device, blob, wire.commitment(idOf(0x6d, 98)), new Uint8Array(32), 1_700_000_002n, 0], // field and blob disagree
    ]), 2n);
    expect(r).toMatchObject({ newSeqs: [1n, 2n], ownAdopted: false });
    expect(core.unreadable(G)).toEqual([{ seq: 1n, reason: 'E_OWN_UNKNOWN' }, { seq: 2n, reason: 'E_OWN_UNKNOWN' }]);
    expect(core.outbox(G).map((o) => [o.msgId, o.state])).toEqual([[m, 1]]);
    // A live op-19 row carries no commitment field: the blob's own commitment decides.
    const live = core.groupApply(G, encode([]), encode([[3n, 0n, ME.device, blob, null, new Uint8Array(32), 1_700_000_003n, 0]]), 3n);
    expect(live).toMatchObject({ newSeqs: [3n], ownAdopted: true });
    expect(core.outbox(G)).toEqual([]);
  });

  it('a deleted upload of an outbox row is stored as its own deleted marker and adopts it; without a commitment the outbox stays', () => {
    const core = new ModelCore(ME);
    registered(core);
    const m = core.sendPrepare(G, textRequest('gone soon'), 1n);
    core.sendEncrypt(m);
    const r = core.groupApply(G, encode([]), encode([[1n, 0n, ME.device, null, wire.commitment(m), new Uint8Array(32), 1_700_000_001n, 1]]), 1n);
    expect(r).toMatchObject({ newSeqs: [1n], ownAdopted: true });
    expect(core.timeline(G, 0n, 200)).toMatchObject([{ seq: 1n, status: 2, body: '', senderUser: ME.user, senderDevice: ME.device, msgId: m }]);
    expect(core.outbox(G)).toEqual([]);
    expect(core.sendConfirm(m, encode([1n, new Uint8Array(32), 1_700_000_001n]))).toEqual({ groupId: G, seq: 1n }); // read back
    const m2 = core.sendPrepare(G, textRequest('unplaced'), 2n);
    core.sendEncrypt(m2);
    core.groupApply(G, encode([]), encode([[2n, 0n, ME.device, null, null, new Uint8Array(32), 1_700_000_002n, 1]]), 2n);
    expect(at(core.timeline(G, 0n, 200), 1)).toMatchObject({ seq: 2n, status: 2, senderUser: null, msgId: null });
    expect(core.outbox(G).map((o) => [o.msgId, o.state])).toEqual([[m2, 1]]);
    // The answer for that upload names seq 2: the confirm completes at its own deleted marker.
    expect(core.sendConfirm(m2, encode([2n, new Uint8Array(32), 1_700_000_002n]))).toEqual({ groupId: G, seq: 2n });
    expect(core.outbox(G)).toEqual([]);
    expect(at(core.timeline(G, 0n, 200), 1)).toMatchObject({ seq: 2n, status: 2, msgId: m2 });
  });

  it('sendConfirm at a seq that holds another row fails the row, throws E_CORE_STATE, and the group sends on', () => {
    const core = new ModelCore(ME);
    registered(core);
    core.groupApply(G, encode([]), peerRow(1n, 'theirs'), 1n);
    const m = core.sendPrepare(G, textRequest('mine'), 1n);
    core.sendEncrypt(m);
    expect(thrown(() => core.sendConfirm(m, encode([1n, new Uint8Array(32), 1_700_000_001n])))).toMatchObject({
      code: 'E_CORE_STATE', detail: 'message seq exists',
    });
    expect(core.outbox(G)).toMatchObject([{ msgId: m, state: 2, error: 'E_CORE_STATE', body: 'mine' }]);
    expect(core.bodies(G)).toEqual(['theirs']);
    expect(thrown(() => core.sendFail(m, 'E_NETWORK'))).toMatchObject({ code: 'E_CORE_STATE' });
    expect(core.sendEncrypt(core.sendPrepare(G, textRequest('next'), 2n)).groupId).toEqual(G);
  });

  it('groupJoined and welcomesApply never lower nextSeq', () => {
    const core = new ModelCore(ME);
    registered(core);
    core.groupApply(G, encode([]), peerRow(5n, 'five'), 5n); // nextSeq 6
    core.groupJoinExternal(expectedFor(G), infoAt(G, 0n), treeAt(G, 0n)); // a resync
    core.groupJoined(G, 2n);
    expect(core.group(G)).toMatchObject({ state: 2, epoch: 1n, nextSeq: 6n });
    core.groupApply(G, encode([[6n, 1n, 1n, 1n, new Uint8Array([0xff])]]), encode([]), 6n);
    expect(core.group(G)?.state).toBe(3);
    expect(core.welcomesApply(welcomes([1n, G, 2n, 2n]), [expectedFor(G)])).toMatchObject([{ outcome: 0 }]);
    expect(core.group(G)).toMatchObject({ state: 2, epoch: 2n, nextSeq: 6n });
    expect(core.bodies(G)).toEqual(['five']);
  });

  it('refuses a Welcome at or below the floor over an existing row, and a mislabelled one, raising nothing; one above joins', () => {
    const core = new ModelCore(ME);
    stoppedAtEpoch2(core);
    expect(core.group(G)).toMatchObject({ state: 3, epoch: 2n });
    expect(core.floor(G)).toBe(2n);
    expect(core.welcomesApply(welcomes([1n, G, 1n, 4n], [2n, G, 2n, 4n]), [expectedFor(G)])).toEqual([
      { welcomeId: 1n, groupId: G, outcome: 2, reason: 'E_CORE_INPUT' },
      { welcomeId: 2n, groupId: G, outcome: 2, reason: 'E_CORE_INPUT' },
    ]);
    // A Welcome into epoch 9 labelled 3 is refused (3f26670) and leaves the floor at 2.
    expect(core.welcomesApply(encode([labelled(9n, G, 3n, wire.welcome(G, 9n))]), [expectedFor(G)])).toEqual([
      { welcomeId: 9n, groupId: G, outcome: 2, reason: 'E_CORE_INPUT' },
    ]);
    expect(core.group(G)?.state).toBe(3);
    expect(core.floor(G)).toBe(2n);
    expect(core.welcomesApply(welcomes([3n, G, 3n, 4n]), [expectedFor(G)])).toMatchObject([{ outcome: 0 }]);
    expect(core.group(G)).toMatchObject({ state: 2, epoch: 3n });
    expect(core.floor(G)).toBe(3n);
  });

  it('refuses a GroupInfo below the floor and rejoins from one at it; a discarded resync keeps the floor and can be repeated', () => {
    const core = new ModelCore(ME);
    stoppedAtEpoch2(core);
    expect(thrown(() => core.groupJoinExternal(expectedFor(G), infoAt(G, 1n), treeAt(G, 1n)))).toMatchObject({
      code: 'E_CORE_INPUT', detail: 'GroupInfo would rejoin at or below an epoch this device has held',
    });
    expect(core.group(G)?.state).toBe(3);
    core.groupJoinExternal(expectedFor(G), infoAt(G, 2n), treeAt(G, 2n));
    core.groupDiscard(G); // the server refused the external commit
    // CORE-ENGINE-03: as groups.rs writes it, epoch 0 (the old group was already replaced); the floor stays.
    expect(core.group(G)).toMatchObject({ state: 3, epoch: 0n });
    expect(core.floor(G)).toBe(2n);
    core.groupJoinExternal(expectedFor(G), infoAt(G, 2n), treeAt(G, 2n));
    core.groupJoined(G, 9n);
    expect(core.group(G)).toMatchObject({ state: 2, epoch: 3n, nextSeq: 10n });
    expect(core.floor(G)).toBe(3n);
  });

  it('a discarded rejoin of a gone row returns it to state 4 with its timeline; a join that created its row leaves nothing', () => {
    const core = new ModelCore(ME);
    registered(core);
    core.groupApply(G, encode([]), peerRow(1n, 'kept'), 1n);
    core.groupApply(G, encode([[2n, 0n, 1n, 1n, wire.commit(1n, PEER.device, [], [ME.device])]]), encode([]), 2n);
    expect(core.group(G)).toMatchObject({ state: 4, nextSeq: 3n });
    core.groupJoinExternal(expectedFor(G), infoAt(G, 1n), treeAt(G, 1n));
    expect(core.group(G)?.state).toBe(1);
    core.groupDiscard(G);
    expect(core.group(G)).toMatchObject({ state: 4, nextSeq: 3n });
    expect(core.bodies(G)).toEqual(['kept']);
    // A Welcome never rebinds the row to another channel.
    expect(core.welcomesApply(welcomes([5n, G, 2n, 3n]), [expectedFor(G, CHANNEL_2)])).toEqual([
      { welcomeId: 5n, groupId: G, outcome: 2, reason: 'E_CORE_STATE' },
    ]);
    expect(core.group(G)).toMatchObject({ state: 4, targetId: CHANNEL });
    core.groupJoinExternal(expectedFor(G2, CHANNEL_2), infoAt(G2, 0n), treeAt(G2, 0n));
    core.groupDiscard(G2);
    expect(core.group(G2)).toBeUndefined();
  });

  it('groupJoinExternal refuses another group id, a row bound to another channel and another holder of the channel, writing nothing', () => {
    const core = new ModelCore(ME);
    registered(core); // G holds CHANNEL
    expect(thrown(() => core.groupJoinExternal(expectedFor(G2, CHANNEL_2), infoAt(G3, 0n), treeAt(G3, 0n)))).toMatchObject({
      code: 'E_CORE_INPUT', detail: 'GroupInfo group id does not match',
    });
    expect(core.group(G2)).toBeUndefined();
    expect(thrown(() => core.groupJoinExternal(expectedFor(G, CHANNEL_2), infoAt(G, 0n), treeAt(G, 0n)))).toMatchObject({
      code: 'E_CORE_STATE', detail: 'group is bound to another channel',
    });
    expect(core.group(G)).toMatchObject({ state: 2, targetId: CHANNEL });
    expect(thrown(() => core.groupJoinExternal(expectedFor(G2), infoAt(G2, 0n), treeAt(G2, 0n)))).toMatchObject({ code: 'E_CORE_STATE' });
    expect(core.group(G2)).toBeUndefined();
  });

  it('the floor is the last epoch reached: a removal does not raise it to the epoch of the removing commit', () => {
    const core = new ModelCore(ME);
    registered(core);
    core.groupApply(G, encode([commitRow(1n, 0n), commitRow(2n, 1n)]), encode([]), 2n); // epoch 2 reached
    core.groupApply(G, encode([[3n, 2n, 1n, 1n, wire.commit(3n, PEER.device, [], [ME.device])]]), encode([]), 3n);
    expect(core.group(G)).toMatchObject({ state: 4, nextSeq: 4n });
    expect(core.floor(G)).toBe(2n);
    expect(thrown(() => core.groupJoinExternal(expectedFor(G), infoAt(G, 1n), treeAt(G, 1n)))).toMatchObject({ code: 'E_CORE_INPUT' });
    core.groupJoinExternal(expectedFor(G), infoAt(G, 2n), treeAt(G, 2n)); // lands at 3, above the floor
    expect(core.group(G)?.state).toBe(1);
    expect(core.floor(G)).toBe(2n); // the join's own epoch is recorded only by groupJoined
    core.groupJoined(G, 5n);
    expect(core.floor(G)).toBe(3n);
  });

  it('welcomesApply refuses a Welcome whose joined state is not its labels, and one into another group, storing nothing', () => {
    // 3f26670: the group id is checked first (E_BINDING), then the label (E_CORE_INPUT).
    const core = new ModelCore(ME);
    const out = core.welcomesApply(encode([
      labelled(1n, G, 1n, wire.welcome(G, 7n)), // the joined epoch is not the label
      labelled(2n, G, 1n, wire.welcome(G, 1n, wire.treeHash(G2, 1n))), // the joined tree hash is not the label
      labelled(3n, G, 1n, wire.welcome(G2, 1n)), // a Welcome into another group
    ]), [expectedFor(G)]);
    expect(out.map((o) => [o.outcome, o.reason])).toEqual([[2, 'E_CORE_INPUT'], [2, 'E_CORE_INPUT'], [2, 'E_BINDING']]);
    expect(core.group(G)).toBeUndefined();
    expect(core.welcomesApply(welcomes([4n, G, 1n, 1n]), [expectedFor(G)])).toMatchObject([{ outcome: 0 }]);
    expect(core.group(G)).toMatchObject({ state: 2, epoch: 1n, nextSeq: 2n });
  });
  it('sendConfirm keeps the epoch framed by sendEncrypt after the group advances', () => {
    const core = new ModelCore(ME);
    registered(core);
    const msg = core.sendPrepare(G, textRequest('framed first'), 1n);
    core.sendEncrypt(msg);
    core.groupApply(G, encode([commitRow(1n, 0n)]), encode([]), 1n);
    expect(core.group(G)?.epoch).toBe(1n);
    core.sendConfirm(msg, encode([2n, new Uint8Array(32), 3n]));
    expect(core.timeline(G, 0n, 10).find((row) => row.seq === 2n)?.epoch).toBe(0n);
  });

  it('cursorAcked refuses a future seq and never moves its cursor backward', () => {
    const core = new ModelCore(ME);
    registered(core);
    core.groupApply(G, encode([]), peerRow(1n, 'one'), 1n);
    expect(thrown(() => core.cursorAcked(G, 2n, 0n))).toMatchObject({ code: 'E_CORE_INPUT' });
    core.cursorAcked(G, 1n, 0n);
    core.cursorAcked(G, 0n, 0n);
    expect(core.cursorBody(G)).toBeNull();
  });

  it('commitConfirm refuses a non-active group', () => {
    const core = new ModelCore(ME);
    core.groupCreate(G, COMMUNITY, CHANNEL);
    expect(thrown(() => core.commitConfirm(G))).toMatchObject({ code: 'E_CORE_STATE' });
  });

  it('groupApply refuses duplicate seqs within a single input array', () => {
    const core = new ModelCore(ME);
    registered(core);
    expect(thrown(() => core.groupApply(G, encode([]), encode([peerMsg(1n, 'first'), peerMsg(1n, 'second')]), 1n)))
      .toMatchObject({ code: 'E_CORE_INPUT' });
    expect(thrown(() => core.groupApply(G, encode([commitRow(1n, 0n), commitRow(1n, 0n)]), encode([]), 1n)))
      .toMatchObject({ code: 'E_CORE_INPUT' });
    expect(core.group(G)?.nextSeq).toBe(1n);
  });

});

describe('ModelCore web-2a: rows, DMs, read markers, activity, settings', () => {
  it('groupRow answers one row of groups() and null for an unknown id', () => {
    const core = new ModelCore(ME);
    registered(core);
    expect(core.groupRow(G)).toEqual(at(core.groups(), 0));
    expect(core.groupRow(G2)).toBeNull();
  });

  it('a DM group has a null community after create, Welcome and external join', () => {
    const core = new ModelCore(ME);
    core.groupCreate(G, null, CHANNEL);
    expect(core.groupRow(G)?.communityId).toBeNull();
    const dm: ExpectedGroup = { groupId: G2, communityId: null, channelId: CHANNEL_2, policyVersion: 1n };
    expect(core.welcomesApply(welcomes([7n, G2, 1n, 2n]), [dm])).toEqual([{ welcomeId: 7n, groupId: G2, outcome: 0, reason: '' }]);
    expect(core.groupRow(G2)).toMatchObject({ communityId: null, targetId: CHANNEL_2, state: 2, epoch: 1n });
    core.groupJoinExternal({ groupId: G3, communityId: null, channelId: CHANNEL_3, policyVersion: 1n }, infoAt(G3, 4n), treeAt(G3, 4n));
    core.groupJoined(G3, 9n);
    expect(core.groupRow(G3)).toMatchObject({ communityId: null, state: 2, epoch: 5n });
  });
  it('reports the joined epoch while an external join is still in state 1', () => {
    const core = new ModelCore(ME);
    core.groupJoinExternal({ groupId: G, communityId: null, channelId: CHANNEL, policyVersion: 1n }, infoAt(G, 4n), treeAt(G, 4n));
    expect(core.groupRow(G)).toMatchObject({ state: 1, epoch: 5n });
  });

  it('a DM row and a community row are bound to different channels for a rejoin', () => {
    const core = new ModelCore(ME);
    core.groupCreate(G, null, CHANNEL);
    core.groupRegistered(G, 1n);
    expect(thrown(() => core.groupJoinExternal(expectedFor(G), infoAt(G, 3n), treeAt(G, 3n))))
      .toMatchObject({ code: 'E_CORE_STATE', detail: 'group is bound to another channel' });
    registered(core, G2, CHANNEL_2);
    expect(thrown(() => core.groupJoinExternal({ groupId: G2, communityId: null, channelId: CHANNEL_2, policyVersion: 1n }, infoAt(G2, 3n), treeAt(G2, 3n))))
      .toMatchObject({ code: 'E_CORE_STATE', detail: 'group is bound to another channel' });
  });

  it('markRead never lowers the marker, clamps it to nextSeq - 1, and refuses state 4 and an unknown group', () => {
    const core = new ModelCore(ME);
    registered(core);
    core.groupApply(G, encode([]), encode([peerMsg(1n, 'a'), peerMsg(2n, 'b'), peerMsg(3n, 'c')]), 3n);
    core.markRead(G, 2n, 1_800_000_000n);
    expect(at(core.activity(), 0)).toMatchObject({ unread: 1, lastReadSeq: 2n, lastSeq: 3n });
    core.markRead(G, 1n, 1_800_000_001n);
    expect(at(core.activity(), 0).lastReadSeq).toBe(2n);
    core.markRead(G, 99n, 1_800_000_002n);
    expect(at(core.activity(), 0)).toMatchObject({ unread: 0, lastReadSeq: 3n, lastSeq: 0n, lastTs: 0n });
    expect(thrown(() => core.markRead(G2, 1n, 1n))).toMatchObject({ code: 'E_CORE_NOT_FOUND' });
    core.groupApply(G, encode([[4n, 0n, 1n, 1n, wire.commit(1n, PEER.device, [], [ME.device])]]), encode([]), 4n);
    expect(core.groupRow(G)?.state).toBe(4);
    expect(thrown(() => core.markRead(G, 1n, 1n))).toMatchObject({ code: 'E_CORE_STATE', detail: 'group state 4' });
  });

  it('activity counts unread type-0 rows of other devices and mentions by the literal rule', () => {
    const core = new ModelCore(ME);
    registered(core, G, CHANNEL);
    registered(core, G2, CHANNEL_2);
    core.groupCreate(G3, COMMUNITY, CHANNEL_3);
    const me = toHex(ME.user);
    const own = core.sendPrepare(G, textRequest('mine <@everyone>'), 1n);
    core.sendEncrypt(own);
    core.sendConfirm(own, encode([1n, new Uint8Array(32), 1_700_000_001n]));
    core.groupApply(G, encode([]), encode([
      peerMsg(2n, `hi <@${me}>`), peerMsg(3n, 'plain'), peerMsg(4n, `<@${me.toUpperCase()}>`), peerMsg(5n, 'all <@here>'),
      [6n, 0n, PEER.device, new Uint8Array([0xff]), null, new Uint8Array(32), 1_700_000_006n, 0],
    ]), 6n);
    expect(core.activity()).toEqual([
      { groupId: G, unread: 4, mentions: 2, lastSeq: 5n, lastTs: 1_700_000_005n, lastReadSeq: 0n },
      { groupId: G2, unread: 0, mentions: 0, lastSeq: 0n, lastTs: 0n, lastReadSeq: 0n },
    ]);
  });

  it('activity excludes a row from another device of the own user (ruling 29)', () => {
    const core = new ModelCore(ME);
    registered(core);
    const sibling = { device: idOf(0xd0, 7), user: ME.user };
    core.groupApply(G, encode([]), encode([
      [1n, 0n, sibling.device, wire.app(sibling, idOf(0x7e, 1), `typed in my other browser <@${toHex(ME.user)}>`), new Uint8Array(32), new Uint8Array(32), 1_700_000_001n, 0],
      peerMsg(2n, 'from the peer'),
    ]), 2n);
    expect(at(core.activity(), 0)).toMatchObject({ unread: 1, mentions: 0, lastSeq: 2n, lastTs: 1_700_000_002n });
  });

  it('settings upsert, delete and check their bounds in bytes', () => {
    const core = new ModelCore(ME);
    const mute = 'mute.channel.' + 'a'.repeat(32);
    core.settingPut('notify.default', 'everything');
    core.settingPut(mute, '1');
    core.settingPut('notify.default', 'nothing');
    expect(core.settings()).toEqual({ [mute]: '1', 'notify.default': 'nothing' });
    core.settingDelete(mute);
    core.settingDelete('absent');
    expect(core.settings()).toEqual({ 'notify.default': 'nothing' });
    for (const [k, v] of [['', 'x'], ['k'.repeat(129), 'x'], ['é'.repeat(65), 'x'], ['k', 'v'.repeat(1025)]] as const) {
      expect(thrown(() => core.settingPut(k, v))).toMatchObject({ code: 'E_CORE_INPUT' });
    }
    core.settingPut('k'.repeat(128), 'v'.repeat(1024));
  });
});

describe('ModelDs web-2a: DMs, leafless reads and the device-list gate', () => {
  const DM = idOf(0xd3, 1);
  const NEW = idOf(0xd0, 9);

  it('serves a patched slow mode back in the channel list and in getChannel (element 8)', async () => {
    const ds = new ModelDs();
    ds.own(ME.user, ME.device);
    const routes = ds.routesFor(ME.device);
    const community = await routes.createCommunity('friends');
    const channel = await routes.createChannel(community.id, { kind: 'text', name: 'news', position: 0 });
    expect((await routes.listChannels(community.id)).map((c) => c.slowmode)).toEqual([0]);
    await routes.patchChannel(channel.id, { slowmode: 30 });
    expect((await routes.listChannels(community.id)).map((c) => c.slowmode)).toEqual([30]);
    expect((await routes.getChannel(channel.id)).slowmode).toBe(30);
    await routes.patchChannel(channel.id, { name: 'renamed' });
    expect((await routes.getChannel(channel.id)).slowmode).toBe(30);
  });

  it('sends a Welcome to the registrant’s other device when that device has KeyPackages', async () => {
    const ds = new ModelDs();
    ds.addDm(DM, [ME, PEER]);
    ds.own(ME.user, NEW);
    const core = new ModelCore(ME);
    const routes = ds.routesFor(ME.device);
    await routes.postGroup(core.groupCreate(G, null, DM));
    core.groupRegistered(G, 1n);
    const proposals = await routes.getProposals(G);
    expect(arr(decode(proposals))).toHaveLength(2);
    await routes.postCommit(G, core.commitBuild(G, proposals));
    expect(ds.welcomesFor(NEW)).toEqual([{ id: 1n, groupId: toHex(G) }]);
  });

  it('a DM group registered by one participant gets the instance Add of the other, who joins by its Welcome', async () => {
    const ds = new ModelDs();
    ds.addDm(DM, [ME, PEER]);
    const meCore = new ModelCore(ME);
    const routes = ds.routesFor(ME.device);
    expect(await routes.getChannel(DM)).toEqual({ id: DM, kind: 3, mode: 0, visibility: 0, parentId: null, name: '', topic: '', position: 0, slowmode: 0, seq: 0n, textGroupId: null });
    await routes.postGroup(meCore.groupCreate(G, null, DM));
    meCore.groupRegistered(G, 1n);
    expect((await routes.getChannel(DM)).textGroupId).toEqual(G);
    const proposals = await routes.getProposals(G);
    const rows = arr(decode(proposals));
    expect(rows).toHaveLength(1);
    const prop = arr(decode(bin(at(arr(at(rows, 0), 5), 3))), 4);
    expect([at(prop, 2), bin(at(prop, 3), 16)]).toEqual(['add', PEER.device]);
    // PEER has not registered and does not know the group: its expected set is empty (head ruling 37).
    const frames: Frame[] = [];
    ds.attach(PEER.device, (f) => frames.push(f));
    await routes.postCommit(G, meCore.commitBuild(G, proposals));
    expect(frames.filter((f) => f.op === 20).map((f) => f.groupId)).toEqual([G]);
    expect(ds.welcomesFor(PEER.device)).toEqual([{ id: 1n, groupId: toHex(G) }]);
    expect(ds.view(G).members).toEqual([toHex(ME.device), toHex(PEER.device)].sort());
    const peerCore = new ModelCore(PEER);
    const first = await ds.routesFor(PEER.device).getWelcomes(0n);
    expect(peerCore.welcomesApply(first.raw, [])).toEqual([{ welcomeId: 1n, groupId: G, outcome: 3, reason: '' }]);
    expect(peerCore.groupRow(G)).toBeNull();
    // The controller learns the DM (onUnexpectedWelcome → loadDms → setExpected): the kept Welcome is served again.
    const again = await ds.routesFor(PEER.device).getWelcomes(0n);
    expect(peerCore.welcomesApply(again.raw, [{ groupId: G, communityId: null, channelId: DM, policyVersion: 1n }]))
      .toEqual([{ welcomeId: 1n, groupId: G, outcome: 0, reason: '' }]);
  });

  it('postDm finds or opens a DM; listDms and getChannel answer participants only', async () => {
    const ds = new ModelDs();
    ds.own(ME.user, ME.device);
    ds.own(PEER.user, PEER.device);
    ds.own(THIRD.user, THIRD.device);
    const mine = ds.routesFor(ME.device);
    const opened = await mine.postDm([PEER.user]);
    expect(opened.created).toBe(true);
    expect(await mine.postDm([PEER.user])).toEqual({ channelId: opened.channelId, created: false });
    expect(await ds.routesFor(PEER.device).listDms()).toEqual([{ channelId: opened.channelId, kind: 3, members: [ME.user, PEER.user] }]);
    expect(await ds.routesFor(THIRD.device).listDms()).toEqual([]);
    await expect(ds.routesFor(THIRD.device).getChannel(opened.channelId)).rejects.toMatchObject({ status: 404, code: 'E_NOT_FOUND' });
    await expect(mine.postDm([ME.user])).rejects.toMatchObject({ status: 400, code: 'E_INVALID_REQUEST' });
  });

  it('a device that holds no leaf reads 404 from messages, handshakes and proposals', async () => {
    const ds = new ModelDs();
    const g = ds.peerCreate(PEER, CHANNEL);
    const stranger = ds.routesFor(ME.device);
    await expect(stranger.getMessages(g, 1n, 10)).rejects.toMatchObject({ status: 404, code: 'E_NOT_FOUND' });
    await expect(stranger.getHandshakes(g, 1n, 10)).rejects.toMatchObject({ status: 404, code: 'E_NOT_FOUND' });
    await expect(stranger.getProposals(g)).rejects.toMatchObject({ status: 404, code: 'E_NOT_FOUND' });
    ds.join(g, ME.device);
    expect((await stranger.getMessages(g, 1n, 10)).count).toBe(0);
    expect(arr(decode(await stranger.getProposals(g)))).toEqual([]);
  });

  it('refuses a registration and an external join by a device its user has not listed', async () => {
    const ds = new ModelDs();
    ds.own(ME.user, ME.device);
    ds.own(ME.user, NEW);
    expect(ds.publishList(ME.user, [{ device: ME.device }])).toBe(1n);
    const g = ds.peerCreate(PEER, CHANNEL_2);
    const newRoutes = ds.routesFor(NEW);
    const core = new ModelCore({ device: NEW, user: ME.user });
    await expect(newRoutes.postGroup(core.groupCreate(G, COMMUNITY, CHANNEL))).rejects.toMatchObject({ status: 400, code: 'E_INVALID_REQUEST' });
    const resync = encode([wire.commit(1n, NEW, [NEW], []), wire.info(g, 1n)]);
    await expect(newRoutes.postResync(g, resync)).rejects.toMatchObject({ status: 422, code: 'E_COMMIT_INVALID', extra: ['external_joiner'] });
    expect(ds.view(g).members).toEqual([toHex(PEER.device)]);
    expect(ds.publishList(ME.user, [{ device: ME.device }, { device: NEW }])).toBe(2n);
    expect((await newRoutes.postResync(g, resync)).epoch).toBe(1n);
    // A device whose owner the model does not know is admitted, as in web-1.
    const g2 = ds.peerCreate(PEER, CHANNEL_3);
    expect((await ds.routesFor(THIRD.device).postResync(g2, encode([wire.commit(1n, THIRD.device, [THIRD.device], []), wire.info(g2, 1n)]))).epoch).toBe(1n);
  });

  it('putDeviceList takes only the next version, and a revocation removes the device from every group', async () => {
    const ds = new ModelDs();
    ds.own(ME.user, ME.device);
    ds.own(ME.user, NEW);
    const body = (v: bigint, entries: { deviceId: Id; revokedAt: bigint | null }[]) =>
      encode([v, fakeListBlob(ME.user, entries, 1_800_000_000n), new Uint8Array(64), new Uint8Array(32)]);
    const mine = ds.routesFor(ME.device);
    await expect(mine.putDeviceList(ME.user, body(2n, []))).rejects.toMatchObject({ status: 409, code: 'E_INVALID_REQUEST' });
    await mine.putDeviceList(ME.user, body(1n, [{ deviceId: ME.device, revokedAt: null }, { deviceId: NEW, revokedAt: null }]));
    await expect(mine.putDeviceList(PEER.user, body(1n, []))).rejects.toMatchObject({ status: 403, code: 'E_FORBIDDEN' });
    const g = ds.peerCreate(PEER, CHANNEL);
    ds.join(g, NEW);
    await mine.putDeviceList(ME.user, body(2n, [{ deviceId: ME.device, revokedAt: null }, { deviceId: NEW, revokedAt: 1_800_000_100n }]));
    expect((await mine.getDeviceList(ME.user))?.version).toBe(2n);
    const history = await mine.getDeviceListHistory(ME.user, 0n);
    expect([history.count, history.lastVersion]).toEqual([2, 2n]);
    expect((await mine.getDeviceListHistory(ME.user, 2n)).count).toBe(0);
    const props = arr(decode(await ds.routesFor(PEER.device).getProposals(g)));
    expect(props).toHaveLength(1);
    const p = arr(decode(bin(at(arr(at(props, 0), 5), 3))), 4);
    expect([at(p, 2), bin(at(p, 3), 16)]).toEqual(['remove', NEW]);
    await expect(ds.routesFor(NEW).getMessages(g, 1n, 10)).rejects.toMatchObject({ status: 401, code: 'E_UNAUTHENTICATED' });
  });
  it('answers the web-2b timeline row: fold fields at their defaults and the mention flag (L-CORE-34)', () => {
    const core = new ModelCore(ME);
    registered(core);
    core.groupApply(G, encode([]), encode([peerMsg(1n, 'plain'), peerMsg(2n, `hi <@${toHex(ME.user)}>`)]), 2n);
    expect(core.timeline(G, 0n, 200).map((x) => [x.seq, x.editedSeq, x.reply, x.reactions, x.pinned, x.attachments, x.mention])).toEqual([
      [1n, 0n, null, [], false, [], false],
      [2n, 0n, null, [], false, [], true],
    ]);
  });

});

describe('ModelCore sends a plain message as web-2a did', () => {
  it('lists it with the eight-field outbox row', () => {
    const core = new ModelCore(ME);
    registered(core);
    const id = core.sendPrepare(G, textRequest('plain'), 5n);
    expect(core.outbox(G)).toEqual([{ msgId: id, state: 0, error: '', created: 5n, body: 'plain', type: 0, replyTo: null, attachments: [] }]);
  });
});

const MY_OTHER: Peer = { device: idOf(0xd0, 8), user: ME.user };
const PEER_OTHER: Peer = { device: idOf(0xd0, 9), user: PEER.user };
const THIRD_OTHER: Peer = { device: idOf(0xd0, 10), user: THIRD.user };
const T = FOLD_TARGET;
const T2 = new Uint8Array(16).fill(0x72);
const ROLE = idOf(0x40, 1);
const FILE: AttachmentDescriptor = {
  blobId: new Uint8Array(32).fill(0x43), key: new Uint8Array(32).fill(0x44), nonce: new Uint8Array(12).fill(0x45), size: 1234,
  mime: 'image/png', w: 640, h: 480, thumb: new Uint8Array(40).fill(0x46), name: 'map.png',
};
const fold = (seq: bigint): Id => idOf(0x7f, Number(seq));

function app(seq: bigint, from: Peer, msgId: Id, body: string, opts: { type?: number; replyTo?: Id | null; attachments?: AttachmentDescriptor[] } = {}): CborInput[] {
  return ModelDs.row.app(seq, 0n, from, msgId, body, opts);
}
function apply(core: ModelCore, ...rows: CborInput[][]): void {
  const through = rows.reduce((m, r) => ((r[0] as bigint) > m ? (r[0] as bigint) : m), 0n);
  core.groupApply(G, encode([]), encode(rows), through);
}
function request(type: SendRequest['type'], replyTo: Id | null, body = '', attachments: AttachmentDescriptor[] = []): SendRequest {
  return { type, replyTo, body, attachments, threadId: null };
}
/** Prepares and frames an own row and applies its echo at `seq`, as the delivery service serves it. */
function own(core: ModelCore, seq: bigint, r: SendRequest): Id {
  const msgId = core.sendPrepare(G, r, seq);
  apply(core, ModelDs.row.ownEcho(seq, 0n, blobOf(core.sendEncrypt(msgId))));
  return msgId;
}
function shown(core: ModelCore, seq: bigint): TimelineRow {
  const r = core.timeline(G, 0n, 200).find((x) => x.seq === seq);
  if (r === undefined) throw new Error(`no displayable row at seq ${String(seq)}`);
  return r;
}
function fresh(): ModelCore {
  const core = new ModelCore(ME);
  registered(core);
  return core;
}

// web3.7 TB1: the thread rule, held to core/dilla-core/tests/client_threads.rs.
describe('ModelCore keeps public threads as the core does (web3.7)', () => {
  const R = T;
  const R2 = T2;
  const threadApp = (seq: bigint, msgId: Id, body: string, threadId: Id | null, opts: { type?: number; replyTo?: Id | null } = {}) =>
    ModelDs.row.app(seq, 0n, PEER, msgId, body, { ...opts, threadId });
  const reply = (n: number): Id => idOf(0x5e, n);

  it('takes a reply whose root is held out of the main timeline and sums it on the root', () => {
    const core = fresh();
    apply(core, threadApp(1n, R, 'the root', null), threadApp(2n, reply(1), 'one', R), threadApp(3n, reply(2), 'two', R));
    expect(core.timeline(G, 0n, 200).map((x) => [x.seq, x.thread])).toEqual([[1n, { count: 2, lastTs: 1_700_000_003n }]]);
  });

  it('keeps an unheld root, a nested thread and a fold in the channel', () => {
    const core = fresh();
    apply(core, threadApp(1n, R, 'the root', null), threadApp(2n, reply(1), 'a reply', R),
      threadApp(3n, reply(2), 'nested', reply(1)), threadApp(4n, reply(3), 'orphan', FOLD_UNHELD),
      threadApp(5n, fold(5n), '👍', R, { type: 3, replyTo: R }));
    expect(core.timeline(G, 0n, 200).map((x) => [x.seq, x.thread?.count ?? null])).toEqual([[1n, 1], [3n, null], [4n, null]]);
    expect(shown(core, 1n).reactions).toEqual([{ emoji: '👍', count: 1, mine: false }]);
  });

  it('does not count a deleted reply', () => {
    const core = fresh();
    apply(core, threadApp(1n, R, 'the root', null), threadApp(2n, reply(1), 'one', R), threadApp(3n, reply(2), 'two', R),
      threadApp(4n, fold(4n), '', null, { type: 2, replyTo: reply(2) }));
    expect(shown(core, 1n).thread).toEqual({ count: 1, lastTs: 1_700_000_002n });
    expect(core.timeline(G, 0n, 200).map((x) => x.seq)).toEqual([1n]);
  });

  it('reads a thread newest first with its root, pages it, and lists the threads by their last reply', () => {
    const core = fresh();
    apply(core, threadApp(1n, R, 'root a', null), threadApp(2n, R2, 'root b', null), threadApp(3n, reply(1), 'a1', R),
      threadApp(4n, reply(2), 'a2', R), threadApp(5n, reply(3), 'b1', R2), threadApp(6n, reply(4), 'orphan', FOLD_UNHELD),
      threadApp(7n, reply(5), 'nested', reply(1)), threadApp(8n, fold(8n), '👍', null, { type: 3, replyTo: R }));
    const t = core.threadTimeline(G, R, 0n, 50);
    expect([t.root?.seq, t.rows.map((x) => x.seq)]).toEqual([1n, [4n, 3n]]);
    expect(core.threadTimeline(G, R, 4n, 1).rows.map((x) => x.seq)).toEqual([3n]);
    expect(core.threadTimeline(G, FOLD_UNHELD, 0n, 50).root).toBeNull();
    expect(() => core.threadTimeline(G, R, 0n, 0)).toThrow('E_CORE_INPUT');
    // Fix round 1 (M1): an unheld root is no thread.
    expect(core.threads(G)).toEqual([
      { rootMsgId: R2, rootSeq: 2n, count: 1, lastTs: 1_700_000_005n, excerpt: 'root b' },
      { rootMsgId: R, rootSeq: 1n, count: 2, lastTs: 1_700_000_004n, excerpt: 'root a' },
    ]);
  });

  it('refuses a thread id on a reply, a non-post, an unheld id and types 1 to 6, and frames an accepted one', () => {
    const core = fresh();
    apply(core, threadApp(1n, R, 'the root', null), threadApp(2n, reply(1), 'a reply', R),
      threadApp(3n, fold(3n), '👍', null, { type: 3, replyTo: R }));
    const thread = (threadId: Id, type: SendRequest['type'] = 0, replyTo: Id | null = null, body = 'x'): SendRequest =>
      ({ ...request(type, replyTo, body), threadId });
    expect(() => core.sendPrepare(G, thread(reply(1)), 9n)).toThrow('the thread root is a thread reply');
    expect(() => core.sendPrepare(G, thread(fold(3n)), 9n)).toThrow('E_CORE_INPUT');
    expect(() => core.sendPrepare(G, thread(FOLD_UNHELD), 9n)).toThrow('target is not held');
    expect(() => core.sendPrepare(G, thread(R, 3, R, '👍'), 9n)).toThrow('only a message starts or joins a thread');
    expect(core.outbox(G)).toEqual([]);
    const id = own(core, 4n, thread(R));
    expect(core.threadTimeline(G, R, 0n, 50).rows.map((x) => [x.seq, x.msgId])).toEqual([[4n, id], [2n, reply(1)]]);
    expect(core.timeline(G, 0n, 200).map((x) => x.seq)).toEqual([1n]);
  });

  // Wave C: threads apply to DMs and group DMs too.
  const dm = (): ModelCore => {
    const core = new ModelCore(ME);
    core.groupCreate(G, null, CHANNEL);
    core.groupRegistered(G, 1n);
    return core;
  };

  it('holds a DM thread reply out of the DM timeline, counts it and lists the root', () => {
    const core = dm();
    apply(core, threadApp(1n, R, 'the root', null), threadApp(2n, reply(1), 'a reply', R));
    expect(core.timeline(G, 0n, 200).map((x) => [x.seq, x.thread?.count ?? null])).toEqual([[1n, 1]]);
    expect(core.threads(G).map((x) => [x.rootSeq, x.count])).toEqual([[1n, 1]]);
    expect(core.threadTimeline(G, R, 0n, 50).rows.map((x) => x.seq)).toEqual([2n]);
  });

  it('accepts a thread id on a DM send', () => {
    const core = dm();
    apply(core, threadApp(1n, R, 'the root', null));
    core.sendPrepare(G, { ...request(0, null, 'x'), threadId: R }, 9n);
    expect(core.outbox(G)).toHaveLength(1);
  });

  it('shows a DM reply before its root flat and holds the one after the root', () => {
    const core = dm();
    apply(core, threadApp(1n, reply(1), 'early', R), threadApp(2n, R, 'root', null), threadApp(3n, reply(3), 'reply', R));
    expect(core.timeline(G, 0n, 200).map((x) => [x.seq, x.thread?.count ?? null])).toEqual([[1n, null], [2n, 1]]);
    expect(core.threadTimeline(G, R, 0n, 50).rows.map((x) => x.seq)).toEqual([3n]);
  });

  it('keeps a pre-release DM row without a thread id in the timeline', () => {
    const core = dm();
    apply(core, threadApp(1n, R, 'the root', null), threadApp(2n, reply(1), 'old reply', null));
    expect(core.timeline(G, 0n, 200).map((x) => [x.seq, x.thread])).toEqual([[1n, null], [2n, null]]);
  });

  it('gives a thread to the first root before it, never to a later row with its id (fix round 1, I1)', () => {
    const core = fresh();
    apply(core, threadApp(1n, reply(1), 'early', R), threadApp(2n, R, 'root', null), threadApp(3n, reply(2), 'r1', R),
      threadApp(4n, R, 'repeat', null), threadApp(5n, reply(3), 'r2', R), threadApp(6n, reply(4), 'self', reply(4)));
    expect(core.timeline(G, 0n, 200).map((x) => [x.seq, x.thread?.count ?? null])).toEqual([[1n, null], [2n, 2], [4n, null], [6n, null]]);
    expect(core.threadTimeline(G, R, 0n, 50).rows.map((x) => x.seq)).toEqual([5n, 3n]);
    // M3: a pin and a quote of a thread reply name its root.
    apply(core, threadApp(7n, fold(7n), '', null, { type: 5, replyTo: reply(2) }), threadApp(8n, reply(5), 'quote', null, { replyTo: reply(2) }));
    expect(core.pins(G).map((p) => [p.targetSeq, p.threadRoot])).toEqual([[3n, R]]);
    expect(shown(core, 8n).reply?.threadRoot).toEqual(R);
  });

  it('keeps a thread listed when a fold reuses its root id (fix round 2, R1-I1)', () => {
    const core = fresh();
    apply(core, threadApp(1n, R, 'root', null), threadApp(2n, reply(1), 'r1', R), threadApp(3n, R2, 'other', null),
      threadApp(4n, R, 'x', null, { type: 3, replyTo: R2 }));
    expect(core.timeline(G, 0n, 200).map((x) => [x.seq, x.thread?.count ?? null])).toEqual([[1n, 1], [3n, null]]);
    expect(core.threads(G).map((t) => [t.rootSeq, t.count])).toEqual([[1n, 1]]);
  });

  it('counts a thread reply toward unread and mentions', () => {
    const core = fresh();
    const root = own(core, 1n, textRequest('my root'));
    apply(core, threadApp(2n, reply(1), `hey <@${toHex(ME.user)}>`, root));
    expect(core.activity().map((a) => [a.unread, a.mentions])).toEqual([[1, 1]]);
  });
});

describe('ModelCore folds envelope types 1–6 as the core does (L-CORE-33, L-CORE-34)', () => {
  it('stores and lists fold rows but shows only displayable rows', () => {
    const core = fresh();
    const r = core.groupApply(G, encode([]), encode([app(1n, PEER, T, 'the target'), app(2n, PEER, fold(2n), '👍', { type: 3, replyTo: T })]), 2n);
    expect(r.newSeqs).toEqual([1n, 2n]);
    expect(core.timeline(G, 0n, 200).map((x) => x.seq)).toEqual([1n]);
    expect(shown(core, 1n).reactions).toEqual([{ emoji: '👍', count: 1, mine: false }]);
  });

  it('applies an edit from the author\'s other device', () => {
    const core = fresh();
    apply(core, app(1n, PEER, T, 'v1'), app(2n, PEER_OTHER, fold(2n), 'v2', { type: 1, replyTo: T }));
    expect(shown(core, 1n)).toMatchObject({ status: 0, body: 'v2', editedSeq: 2n });
  });

  it('applies the author\'s highest-seq edit, and no edit sequenced before its target (lead-fold-spoofing)', () => {
    const core = fresh();
    apply(core, app(1n, PEER, fold(1n), 'early', { type: 1, replyTo: T }), app(2n, PEER, fold(2n), 'later', { type: 1, replyTo: T }),
      app(3n, THIRD, fold(3n), 'not the author', { type: 1, replyTo: T }));
    expect(core.timeline(G, 0n, 200)).toEqual([]);
    apply(core, app(4n, PEER, T, 'original'));
    // The security ruling of 2026-10-07 binds over the plan's { body: 'later', editedSeq: 2n }: a fold counts only when
    // its seq is above its target's, so the edits at seq 1 and 2 never apply to the target at seq 4.
    expect(shown(core, 4n)).toMatchObject({ body: 'original', editedSeq: 0n });
    apply(core, app(5n, PEER, T2, 'second'), app(6n, PEER, fold(6n), 'early', { type: 1, replyTo: T2 }),
      app(7n, PEER, fold(7n), 'later', { type: 1, replyTo: T2 }), app(8n, THIRD, fold(8n), 'not the author', { type: 1, replyTo: T2 }));
    expect(shown(core, 5n)).toMatchObject({ body: 'later', editedSeq: 7n });
  });

  it('ignores a delete by another user', () => {
    const core = fresh();
    apply(core, app(1n, PEER, T, 'kept'), app(2n, THIRD, fold(2n), '', { type: 2, replyTo: T }));
    expect(shown(core, 1n)).toMatchObject({ status: 0, body: 'kept' });
  });

  it('lets a delete by the author remove the target with its edits, reactions and pin, and records no purge for a peer', () => {
    const core = fresh();
    apply(core, app(1n, PEER, T, 'gone soon'), app(2n, THIRD, fold(2n), '👍', { type: 3, replyTo: T }),
      app(3n, THIRD, fold(3n), '', { type: 5, replyTo: T }), app(4n, PEER, fold(4n), 'edited', { type: 1, replyTo: T }),
      app(5n, PEER_OTHER, fold(5n), '', { type: 2, replyTo: T }));
    expect(shown(core, 1n)).toMatchObject({ status: 2, body: '', editedSeq: 0n, reactions: [], pinned: false, attachments: [], reply: null });
    expect(core.pins(G)).toEqual([]);
    expect(core.purges()).toEqual([]);
  });

  it('keeps reactions per user: two devices of one user make one chip', () => {
    const core = fresh();
    apply(core, app(1n, PEER, T, 't'), app(2n, THIRD, fold(2n), '👍', { type: 3, replyTo: T }), app(3n, THIRD_OTHER, fold(3n), '👍', { type: 3, replyTo: T }));
    expect(shown(core, 1n).reactions).toEqual([{ emoji: '👍', count: 1, mine: false }]);
  });

  it('lets only a reaction\'s own user remove it', () => {
    const core = fresh();
    apply(core, app(1n, PEER, T, 't'), app(2n, THIRD, fold(2n), '👍', { type: 3, replyTo: T }), app(3n, PEER, fold(3n), '👍', { type: 4, replyTo: T }));
    expect(shown(core, 1n).reactions).toEqual([{ emoji: '👍', count: 1, mine: false }]);
    apply(core, app(4n, THIRD_OTHER, fold(4n), '👍', { type: 4, replyTo: T }));
    expect(shown(core, 1n).reactions).toEqual([]);
  });

  it('orders chips by first appearance and marks the own reaction', () => {
    const core = fresh();
    apply(core, app(1n, PEER, T, 't'), app(2n, PEER, fold(2n), '🔥', { type: 3, replyTo: T }),
      app(3n, THIRD, fold(3n), '👍', { type: 3, replyTo: T }), app(4n, THIRD, fold(4n), '🔥', { type: 3, replyTo: T }));
    own(core, 5n, request(3, T, '👍'));
    expect(shown(core, 1n).reactions).toEqual([{ emoji: '🔥', count: 2, mine: false }, { emoji: '👍', count: 2, mine: true }]);
  });

  it('answers a reply held, not held and deleted', () => {
    const core = fresh();
    apply(core, app(1n, PEER, T, 'line one\nline two'), app(2n, THIRD, fold(2n), 'a reply', { replyTo: T }),
      app(3n, THIRD, fold(3n), 'to nothing here', { replyTo: FOLD_UNHELD }), app(4n, PEER, T2, 'second'),
      app(5n, THIRD, fold(5n), 'to the second', { replyTo: T2 }), app(6n, PEER, fold(6n), '', { type: 2, replyTo: T2 }));
    expect(shown(core, 2n).reply).toEqual({ replyTo: T, targetSeq: 1n, targetUser: PEER.user, excerpt: 'line one line two', state: 0, threadRoot: null });
    expect(shown(core, 3n).reply).toEqual({ replyTo: FOLD_UNHELD, targetSeq: null, targetUser: null, excerpt: '', state: 1, threadRoot: null });
    expect(shown(core, 5n).reply).toEqual({ replyTo: T2, targetSeq: 4n, targetUser: PEER.user, excerpt: '', state: 2, threadRoot: null });
    expect(shown(core, 1n).reply).toBeNull();
  });

  it('cuts the excerpt at 120 scalar values of the shown body, with line breaks as spaces', () => {
    const core = fresh();
    apply(core, app(1n, PEER, T, '😀'.repeat(130)), app(2n, THIRD, fold(2n), 'r', { replyTo: T }), app(3n, PEER, T2, 'old'),
      app(4n, PEER, fold(4n), 'new\r\nbody', { type: 1, replyTo: T2 }), app(5n, THIRD, fold(5n), 'r2', { replyTo: T2 }));
    expect(shown(core, 2n).reply?.excerpt).toBe('😀'.repeat(120));
    expect(shown(core, 5n).reply?.excerpt).toBe('new  body');
  });

  it('accepts a pin from any member; the latest pin or unpin decides; pins list newest first', () => {
    const core = fresh();
    apply(core, app(1n, PEER, T, 'first'), app(2n, THIRD, T2, 'second'), app(3n, THIRD, fold(3n), '', { type: 5, replyTo: T }),
      app(4n, PEER, fold(4n), '', { type: 5, replyTo: T2 }));
    expect(core.pins(G)).toEqual([
      { targetSeq: 2n, msgId: T2, pinnedSeq: 4n, byUser: PEER.user, author: THIRD.user, excerpt: 'second', targetTs: shown(core, 2n).recvTs, threadRoot: null },
      { targetSeq: 1n, msgId: T, pinnedSeq: 3n, byUser: THIRD.user, author: PEER.user, excerpt: 'first', targetTs: shown(core, 1n).recvTs, threadRoot: null },
    ]);
    expect([shown(core, 1n).pinned, shown(core, 2n).pinned]).toEqual([true, true]);
    apply(core, app(5n, PEER, fold(5n), '', { type: 6, replyTo: T }));
    expect(core.pins(G).map((p) => p.targetSeq)).toEqual([2n]);
    expect(shown(core, 1n).pinned).toBe(false);
    expect(thrown(() => core.pins(idOf(0x9b, 99)))).toMatchObject({ code: 'E_CORE_NOT_FOUND' });
  });

  it('shows a row that repeats a msg id but never makes it a target, nor gives it the target\'s folds', () => {
    const core = fresh();
    apply(core, app(1n, PEER, T, 'original'), app(2n, THIRD, T, 'a replay of the id'), app(3n, PEER, fold(3n), 'edited', { type: 1, replyTo: T }),
      app(4n, THIRD, fold(4n), '👍', { type: 3, replyTo: T }), app(5n, THIRD, fold(5n), '', { type: 5, replyTo: T }));
    expect(shown(core, 1n)).toMatchObject({ body: 'edited', editedSeq: 3n, reactions: [{ emoji: '👍', count: 1, mine: false }], pinned: true });
    expect(shown(core, 2n)).toMatchObject({ body: 'a replay of the id', editedSeq: 0n, reactions: [], pinned: false });
  });

  it('shows at most twenty reactions, the most counted first (FACTS-SECURITY-02)', () => {
    const core = fresh();
    const rows: CborInput[][] = [app(1n, PEER, T, 't')];
    for (let i = 0; i < 25; i++) rows.push(app(BigInt(2 + i), PEER, fold(BigInt(2 + i)), `r${String(i).padStart(2, '0')}`, { type: 3, replyTo: T }));
    rows.push(app(27n, THIRD, fold(27n), 'r24', { type: 3, replyTo: T }));
    apply(core, ...rows);
    const want = Array.from({ length: 19 }, (_, i) => ({ emoji: `r${String(i).padStart(2, '0')}`, count: 1, mine: false }));
    want.push({ emoji: 'r24', count: 2, mine: false });
    expect(shown(core, 1n).reactions).toEqual(want);
  });

  it('drops the author\'s edits when the author deletes a target this device never held (FACTS-SECURITY-03)', () => {
    const core = fresh();
    apply(core, ModelDs.row.deleted(1n, 0n), app(2n, PEER, fold(2n), 'secret words', { type: 1, replyTo: T }),
      app(3n, THIRD, fold(3n), 'third words', { type: 1, replyTo: T }), app(4n, PEER, fold(4n), '', { type: 2, replyTo: T }));
    expect(core.storedRow(G, 2n)).toEqual({ type: 1, status: 0, body: '', hasEnvelope: false });
    expect(core.storedRow(G, 3n)).toEqual({ type: 1, status: 0, body: 'third words', hasEnvelope: true });
  });

  it('drops a reaction whose row the delivery service deleted by seq', () => {
    const core = fresh();
    apply(core, app(1n, PEER, T, 't'), app(2n, THIRD, fold(2n), '👍', { type: 3, replyTo: T }));
    core.messageDeleted(G, 2n);
    expect(shown(core, 1n).reactions).toEqual([]);
  });

  it('blanks a target the delivery service deleted by seq and drops what folded onto it', () => {
    const core = fresh();
    apply(core, app(1n, PEER, T, 't'), app(2n, THIRD, fold(2n), '👍', { type: 3, replyTo: T }), app(3n, THIRD, fold(3n), '', { type: 5, replyTo: T }));
    core.messageDeleted(G, 1n);
    expect(shown(core, 1n)).toMatchObject({ status: 2, body: '', reactions: [], pinned: false });
    expect(core.pins(G)).toEqual([]);
  });

  it('flags role mentions for roles known when the row arrives, never backfilled, and refuses a bad role list', () => {
    const core = fresh();
    const role = toHex(ROLE);
    core.ownRolesSet(COMMUNITY, [ROLE]);
    apply(core, app(1n, PEER, idOf(0x7e, 1), `<@${role}> look`));
    core.ownRolesSet(COMMUNITY, []);
    apply(core, app(2n, PEER, idOf(0x7e, 2), `<@${role}> again`));
    expect([shown(core, 1n).mention, shown(core, 2n).mention]).toEqual([true, false]);
    expect(at(core.activity(), 0).mentions).toBe(1);
    const tooMany = Array.from({ length: 65 }, (_, i) => idOf(0x40, i));
    expect(thrown(() => core.ownRolesSet(COMMUNITY, tooMany))).toMatchObject({ code: 'E_CORE_INPUT', detail: 'role_ids must be 0..=64 ids of 16 bytes' });
    expect(thrown(() => core.ownRolesSet(COMMUNITY, [new Uint8Array(15)]))).toMatchObject({ code: 'E_CORE_INPUT', detail: 'role_ids must be 0..=64 ids of 16 bytes' });
  });

  it('counts a pin or unpin only from a pinner once the role view is known, and refolds when it changes (web3.5 R1)', () => {
    const core = fresh();
    core.authoritySet(G, [PEER.user], [], []);
    apply(core, app(1n, PEER, T, 'first'), app(2n, PEER, fold(2n), '', { type: 5, replyTo: T }),
      app(3n, THIRD, fold(3n), '', { type: 6, replyTo: T }), app(4n, PEER, T2, 'second'), app(5n, THIRD, fold(5n), '', { type: 5, replyTo: T2 }));
    expect(core.pins(G).map((p) => [p.targetSeq, p.pinnedSeq])).toEqual([[1n, 2n]]);
    expect(shown(core, 4n).pinned).toBe(false);
    core.authoritySet(G, [], [], []);
    expect(core.pins(G)).toEqual([]);
    core.authoritySet(G, [PEER.user], [], []);
    expect(core.pins(G).map((p) => p.targetSeq)).toEqual([1n]);
    core.authoritySet(G, [PEER.user, THIRD.user], [], []);
    expect(core.pins(G).map((p) => [p.targetSeq, p.pinnedSeq])).toEqual([[4n, 5n]]);
    const tooMany = Array.from({ length: 4097 }, (_, i) => idOf(0x40, i));
    expect(thrown(() => core.authoritySet(G, tooMany, [], []))).toMatchObject({ code: 'E_CORE_INPUT', detail: 'pinners, broadcasters and mentionable must be 0..=4096 ids of 16 bytes' });
    expect(thrown(() => core.authoritySet(G, [], [new Uint8Array(15)], []))).toMatchObject({ code: 'E_CORE_INPUT' });
  });

  it('refuses a role view for a DM group as the core does (web3.5 C4: a worker that pushes one is caught)', () => {
    const core = fresh();
    const dm = idOf(0xd2, 1);
    core.seedGroup(dm, 0, null, idOf(0xd1, 1));
    expect(thrown(() => core.authoritySet(dm, [PEER.user], [], []))).toMatchObject({ code: 'E_CORE_STATE', detail: 'a DM group has no role view' });
    core.authoritySet(G, [PEER.user], [], []);
  });

  it('counts @everyone/@here only from a broadcaster once the view is known, recomputing held rows (web3.5 R1)', () => {
    const core = fresh();
    apply(core, app(1n, PEER, idOf(0x7e, 1), '<@everyone> standup'));
    expect(shown(core, 1n).mention).toBe(true);
    core.authoritySet(G, [], [], []);
    expect(shown(core, 1n).mention).toBe(false);
    apply(core, app(2n, PEER, idOf(0x7e, 2), '<@here> ping'), app(3n, PEER, idOf(0x7e, 3), `<@everyone> <@${toHex(ME.user)}>`));
    expect([shown(core, 2n).mention, shown(core, 3n).mention]).toEqual([false, true]);
    expect(at(core.activity(), 0).mentions).toBe(1);
    core.authoritySet(G, [], [PEER.user], []);
    expect([1n, 2n, 3n].map((s) => shown(core, s).mention)).toEqual([true, true, true]);
    expect(at(core.activity(), 0).mentions).toBe(3);
  });

  it('counts an own-role token only if the role is mentionable or the sender may broadcast (W2a review finding 1)', () => {
    const core = fresh();
    const other = idOf(0x3d, 1);
    const unheld = idOf(0x3e, 1);
    core.ownRolesSet(COMMUNITY, [ROLE, other]);
    core.authoritySet(G, [], [], []);
    apply(core, app(1n, PEER, idOf(0x7e, 1), `<@${toHex(ROLE)}> ops`), app(2n, PEER, idOf(0x7e, 2), `<@${toHex(other)}> ops`),
      app(3n, PEER, idOf(0x7e, 3), `<@${toHex(unheld)}> ops`));
    const flags = (): boolean[] => [1n, 2n, 3n].map((s) => shown(core, s).mention);
    expect(flags()).toEqual([false, false, false]);
    core.authoritySet(G, [], [], [ROLE, unheld]);
    expect(flags()).toEqual([true, false, false]);
    core.authoritySet(G, [], [PEER.user], [ROLE]);
    expect(flags()).toEqual([true, true, false]);
    core.authoritySet(G, [], [], [ROLE]);
    expect(flags()).toEqual([true, false, false]);
    expect(at(core.activity(), 0).mentions).toBe(1);
    expect(thrown(() => core.authoritySet(G, [], [], [new Uint8Array(15)]))).toMatchObject({ code: 'E_CORE_INPUT' });
  });

  it('never raises a mention through an edit', () => {
    const core = fresh();
    apply(core, app(1n, PEER, T, 'hello'), app(2n, PEER, fold(2n), `<@${toHex(ME.user)}> hello`, { type: 1, replyTo: T }));
    expect(shown(core, 1n)).toMatchObject({ body: `<@${toHex(ME.user)}> hello`, mention: false });
    expect(at(core.activity(), 0).mentions).toBe(0);
  });

  it('reads a fold row without a target or with files as a row it cannot read (E_ENVELOPE_SHAPE)', () => {
    const core = fresh();
    apply(core, app(1n, PEER, T, 't'), app(2n, PEER, fold(2n), 'x', { type: 1 }), app(3n, PEER, fold(3n), '👍', { type: 3, replyTo: T, attachments: [FILE] }));
    expect(core.unreadable(G)).toEqual([{ seq: 2n, reason: 'E_ENVELOPE_SHAPE' }, { seq: 3n, reason: 'E_ENVELOPE_SHAPE' }]);
    expect(shown(core, 1n)).toMatchObject({ body: 't', reactions: [] });
  });
});

describe('ModelCore sends every type as the core does (L-CORE-36…38)', () => {
  it('sendPrepare accepts ten attachments and refuses eleven, as the Rust core does', () => {
    const core = fresh();
    expect(() => core.sendPrepare(G, request(0, null, 'x', Array.from({ length: 10 }, () => FILE)), 1n)).not.toThrow();
    expect(thrown(() => core.sendPrepare(G, request(0, null, 'x', Array.from({ length: 11 }, () => FILE)), 2n))).toMatchObject({ code: 'E_ENVELOPE_LIMIT' });
  });

  it('refuses every send_prepare rule with its code and detail and writes nothing', () => {
    const core = fresh();
    apply(core, app(1n, PEER, T, 'theirs'), app(2n, PEER, T2, 'deleted'), app(3n, PEER, fold(3n), '', { type: 2, replyTo: T2 }));
    const mine = own(core, 4n, request(0, null, 'mine'));
    const cases: [SendRequest, string, string][] = [
      [request(0, null, 'x', [{ ...FILE, key: new Uint8Array(31) }]), 'E_CORE_INPUT', 'request is malformed'],
      [request(7 as unknown as SendRequest['type'], null, 'x'), 'E_CORE_INPUT', 'type is out of range'],
      [request(1, mine, 'x', [FILE]), 'E_CORE_INPUT', 'only a message carries attachments'],
      [request(0, null, '  \n'), 'E_CORE_INPUT', 'body is empty'],
      [request(0, FOLD_UNHELD, 'x'), 'E_CORE_NOT_FOUND', 'target is not held'],
      [request(3, null, '👍'), 'E_CORE_INPUT', 'reply_to is required'],
      [request(3, FOLD_UNHELD, '👍'), 'E_CORE_NOT_FOUND', 'target is not held'],
      [request(3, T2, '👍'), 'E_CORE_STATE', 'the target is deleted'],
      [request(1, T, 'mine now'), 'E_CORE_INPUT', 'only the author may edit or delete'],
      [request(2, T), 'E_CORE_INPUT', 'only the author may edit or delete'],
      [request(1, mine, ' '), 'E_CORE_INPUT', 'body is empty'],
      [request(3, T, ''), 'E_CORE_INPUT', 'emoji must be 1..=32 bytes'],
      [request(4, T, 'x'.repeat(33)), 'E_CORE_INPUT', 'emoji must be 1..=32 bytes'],
      [request(5, T, 'x'), 'E_CORE_INPUT', 'body must be empty'],
      [request(6, T, 'x'), 'E_CORE_INPUT', 'body must be empty'],
      [request(2, mine, 'x'), 'E_CORE_INPUT', 'body must be empty'],
      [request(0, null, 'x', Array.from({ length: 11 }, () => FILE)), 'E_ENVELOPE_LIMIT', ''],
      [request(0, null, 'x', [{ ...FILE, name: 'n'.repeat(256) }]), 'E_ENVELOPE_LIMIT', ''],
      [request(0, null, 'x', [{ ...FILE, mime: 'm'.repeat(256) }]), 'E_ENVELOPE_LIMIT', ''],
      [request(0, null, '€'.repeat(1334)), 'E_ENVELOPE_LIMIT', ''],
    ];
    for (const [r, code, detail] of cases) {
      expect(thrown(() => core.sendPrepare(G, r, 9n)), `${String(r.type)} ${code} ${detail}`).toMatchObject({ code, detail });
      expect(core.outbox(G)).toEqual([]);
    }
    for (const r of [request(0, null, '', [FILE]), request(3, T, '👍'), request(5, T), request(1, mine, 'better'), request(2, mine), request(0, T, 'a reply')]) {
      core.sendPrepare(G, r, 9n);
    }
    expect(core.outbox(G).map((o) => o.type)).toEqual([0, 3, 5, 1, 2, 0]);
    // ADJ-01 (lesson f): the type check precedes the group check, as in the core (L-CORE-36 as amended, A8).
    const unknownGroup = new ModelCore(ME);
    expect(thrown(() => unknownGroup.sendPrepare(G, request(9 as unknown as SendRequest['type'], null, 'x'), 9n)))
      .toMatchObject({ code: 'E_CORE_INPUT', detail: 'type is out of range' });
    const notJoined = new ModelCore(ME);
    notJoined.groupCreate(G, COMMUNITY, CHANNEL);
    expect(thrown(() => notJoined.sendPrepare(G, request(9 as unknown as SendRequest['type'], null, 'x'), 9n)))
      .toMatchObject({ code: 'E_CORE_INPUT', detail: 'type is out of range' });
    expect(notJoined.outbox(G)).toEqual([]);
    expect(thrown(() => notJoined.sendPrepare(G, request(0, null, 'x'), 9n))).toMatchObject({ code: 'E_CORE_STATE' });
  });

  it('keeps type, reply and attachment summaries in the outbox, never a key', () => {
    const core = fresh();
    apply(core, app(1n, PEER, T, 'theirs'));
    const id = core.sendPrepare(G, request(0, T, 'with a file', [FILE]), 5n);
    expect(core.outbox(G)).toEqual([{ msgId: id, state: 0, error: '', created: 5n, body: 'with a file', type: 0, replyTo: T,
      attachments: [{ blobId: FILE.blobId, size: 1234, mime: 'image/png', name: 'map.png' }] }]);
    expect(Object.keys(at(at(core.outbox(G), 0).attachments, 0))).toEqual(['blobId', 'size', 'mime', 'name']);
  });

  it('answers the full descriptor of a held message and refuses anything else', () => {
    const core = fresh();
    apply(core, app(1n, PEER, T, 't'), app(2n, PEER, fold(2n), '👍', { type: 3, replyTo: T }));
    own(core, 3n, request(0, null, '', [FILE]));
    expect(core.attachmentGet(G, 3n, 0)).toEqual(FILE);
    expect(shown(core, 3n).attachments).toEqual([{ index: 0, size: 1234, mime: 'image/png', w: 640, h: 480, hasThumb: true, name: 'map.png' }]);
    for (const [seq, index] of [[3n, 1], [2n, 0], [1n, 0], [99n, 0]] as const) {
      expect(thrown(() => core.attachmentGet(G, seq, index))).toMatchObject({ code: 'E_CORE_NOT_FOUND', detail: 'no such attachment' });
    }
  });

  it('records one purge for the sending device\'s own delete, adopted from its echo, and purgeDone clears it', () => {
    const core = fresh();
    const target = own(core, 1n, request(0, null, 'with a file', [FILE]));
    expect(core.purges()).toEqual([]);
    own(core, 2n, request(2, target));
    expect(shown(core, 1n)).toMatchObject({ status: 2, body: '', attachments: [] });
    expect(core.purges()).toEqual([{ groupId: G, seq: 1n, channelId: CHANNEL, blobIds: [FILE.blobId] }]);
    core.messageDeleted(G, 1n);
    expect(core.purges()).toHaveLength(1);
    core.purgeDone(G, 1n);
    expect(core.purges()).toEqual([]);
    core.purgeDone(G, 1n);
    expect(core.purges()).toEqual([]);
  });

  it('records the purge when the delete is confirmed by the upload answer', () => {
    const core = fresh();
    const target = own(core, 1n, request(0, null, 'x', [FILE]));
    const del = core.sendPrepare(G, request(2, target), 2n);
    core.sendEncrypt(del);
    core.sendConfirm(del, encode([2n, new Uint8Array(32), 1_700_000_002n]));
    expect(core.purges()).toEqual([{ groupId: G, seq: 1n, channelId: CHANNEL, blobIds: [FILE.blobId] }]);
  });

  it('records no purge for a delete from the own user\'s other device', () => {
    const core = fresh();
    const target = own(core, 1n, request(0, null, 'mine', [FILE]));
    apply(core, app(2n, MY_OTHER, fold(2n), '', { type: 2, replyTo: target }));
    expect(shown(core, 1n).status).toBe(2);
    expect(core.purges()).toEqual([]);
  });
});

describe('ModelDs answers the message delete as the delivery service does (L-HTTP-80)', () => {
  function world() {
    const ds = new ModelDs();
    for (const p of [ME, MY_OTHER, PEER, THIRD]) ds.own(p.user, p.device);
    ds.addChannel(COMMUNITY, CHANNEL);
    const g = ds.peerCreate(PEER, CHANNEL);
    ds.join(g, ME.device);
    ds.join(g, MY_OTHER.device);
    const frames: Frame[] = [];
    ds.attach(PEER.device, (f) => { frames.push(f); });
    const mine = ds.peerSend(g, ME, 'mine');
    const theirs = ds.peerSend(g, PEER, 'theirs');
    return { ds, g, frames, mine, theirs };
  }

  it('deletes for any member device of the uploading user, tombstones the row and fans out op 21', async () => {
    const { ds, g, frames, mine, theirs } = world();
    expect(await ds.routesFor(MY_OTHER.device).deleteGroupMessage(g, mine)).toBe('deleted');
    expect(ds.view(g).messages.map((m) => m.seq)).toEqual([theirs]);
    const op21 = frames.filter((f) => f.op === 21);
    expect(op21).toHaveLength(1);
    expect(at(at(op21, 0).payload, 0)).toBe(mine);
    const page = arr(decode((await ds.routesFor(PEER.device).getMessages(g, mine, 10)).raw));
    expect(at(arr(at(page, 0), 8), 7)).toBe(1n);
    // A second delete of the tombstoned row is 204 and fans out again, as dillad's no-op tombstone update does.
    expect(await ds.routesFor(ME.device).deleteGroupMessage(g, mine)).toBe('deleted');
    expect(frames.filter((f) => f.op === 21)).toHaveLength(2);
  });

  it('refuses another user\'s upload 403 E_NOT_UPLOADER and answers an unknown seq as gone', async () => {
    const { ds, g, theirs } = world();
    await expect(ds.routesFor(ME.device).deleteGroupMessage(g, theirs)).rejects.toMatchObject({ status: 403, code: 'E_NOT_UPLOADER' });
    expect(ds.view(g).messages.map((m) => m.seq)).toContain(theirs);
    expect(await ds.routesFor(ME.device).deleteGroupMessage(g, 99n)).toBe('gone');
  });

  // Attacker statement (L-HTTP-80): this 404 hits only a device that is no longer in the group; the uploader's other
  // member devices can still delete, and no honest delete comes from a removed device.
  it('answers a device that is not a member 404 (gone), even when its own user uploaded the message', async () => {
    const { ds, g, frames, mine } = world();
    ds.evict(g, MY_OTHER.device);
    expect(await ds.routesFor(MY_OTHER.device).deleteGroupMessage(g, mine)).toBe('gone');
    expect(await ds.routesFor(THIRD.device).deleteGroupMessage(g, mine)).toBe('gone');
    expect(ds.view(g).messages.map((m) => m.seq)).toContain(mine);
    expect(frames.filter((f) => f.op === 21)).toEqual([]);
    expect(ds.deleteAs(g, MY_OTHER.device, mine)).toBe(404);
    expect(ds.deleteAs(idOf(0x9a, 77), ME.device, mine)).toBe(404);
  });
});

describe('trust send block (trust.rs leaf_verdict/send_block, messages.rs send_prepare/send_encrypt)', () => {
  const PEER_2 = idOf(0xd0, 22);
  const listOf = (v: number, devices: Id[], user = PEER.user): [number, Uint8Array, Uint8Array, Uint8Array] =>
    [v, fakeListBlob(user, devices.map((deviceId) => ({ deviceId, revokedAt: null, dskPub: fakeDskPub(deviceId) })), BigInt(v)),
      bytes(64, v), bytes(32, v - 1)];
  const proposals = (...devices: Id[]): Uint8Array =>
    encode(devices.map((device, n) => [idOf(0x5e, n), 0, null, wire.prop(idOf(0x5e, n), 'add', device), 0]));
  function trusted(): ModelCore {
    const core = new ModelCore(ME);
    registered(core);
    core.addPendingLeaf(G, 1, PEER.user, PEER.device, fakeDskPub(PEER.device), 1);
    core.addPendingLeaf(G, 2, PEER.user, PEER_2, fakeDskPub(PEER_2), 1);
    return core;
  }

  it('records a failed verdict for a leaf the ingested list leaves out, bumps the generation and blocks sending', () => {
    const core = trusted();
    const queued = core.sendPrepare(G, textRequest('queued before'), 1n);
    const v1 = listOf(1, [PEER.device]);
    expect(core.deviceListIngest(G, 1, PEER.user, encode([v1]), v1[1], 10n)).toEqual({ version: 1n, changed: false, quarantined: 1 });
    expect(core.trustGeneration()).toBe(1n);
    expect(thrown(() => core.sendPrepare(G, textRequest('after'), 2n))).toMatchObject({ code: 'E_DEVICE_UNLISTED' });
    expect(thrown(() => core.sendEncrypt(queued))).toMatchObject({ code: 'E_DEVICE_UNLISTED' });
    // Refused before anything changed: the row is still queued.
    expect(core.outbox(G).map((row) => row.state)).toEqual([0]);
  });

  it('a later list naming the device clears its own failed verdict, and sending resumes', () => {
    const core = trusted();
    const v1 = listOf(1, [PEER.device]);
    core.deviceListIngest(G, 1, PEER.user, encode([v1]), v1[1], 10n);
    const v2 = listOf(2, [PEER.device, PEER_2]);
    expect(core.deviceListIngest(G, 1, PEER.user, encode([v1, v2]), v2[1], 11n)).toEqual({ version: 2n, changed: false, quarantined: 0 });
    expect(core.trustGeneration()).toBe(2n);
    const id = core.sendPrepare(G, textRequest('now'), 3n);
    expect(core.sendEncrypt(id).groupId).toEqual(G);
  });

  it('a changed-key verdict wins over other failures and no later valid verdict releases it', () => {
    const core = trusted();
    const v1 = listOf(1, [PEER.device]);
    core.deviceListIngest(G, 1, PEER.user, encode([v1]), v1[1], 10n);
    core.setLeafVerdict(G, 1, 'E_UMK_CHANGED');
    expect(core.trustGeneration()).toBe(2n);
    expect(thrown(() => core.sendPrepare(G, textRequest('x'), 2n))).toMatchObject({ code: 'E_UMK_CHANGED' });
    expect(core.pendingLeaves().find((leaf) => leaf.leafIndex === 1)?.reason).toBe('changed');
    const v2 = listOf(2, [PEER.device, PEER_2]);
    core.deviceListIngest(G, 2, PEER.user, encode([v1, v2]), v2[1], 11n);
    expect(thrown(() => core.sendPrepare(G, textRequest('x'), 3n))).toMatchObject({ code: 'E_UMK_CHANGED' });
  });

  it('a verdict of a leaf in another group blocks only that group', () => {
    const core = trusted();
    registered(core, G2, CHANNEL_2);
    core.setLeafVerdict(G, 2, 'E_CREDENTIAL');
    expect(thrown(() => core.sendPrepare(G, textRequest('x'), 2n))).toMatchObject({ code: 'E_CREDENTIAL' });
    expect(core.sendPrepare(G2, textRequest('y'), 2n)).toHaveLength(16);
  });

  it('answers proposedAdds and refuses a failed Add candidate by its verdict, without a generation bump', () => {
    const core = trusted();
    const v1 = listOf(1, [PEER.device]);
    core.deviceListIngest(G, 1, PEER.user, encode([v1]), v1[1], 10n);
    const generation = core.trustGeneration();
    const FORGED = idOf(0xd0, 23);
    core.addKeyPackage(PEER.user, FORGED, fakeDskPub(FORGED), 1);
    const body = proposals(FORGED);
    expect(core.proposedAdds(G, body)).toEqual([{ index: 0n, userId: PEER.user, cachedVersion: 1n }]);
    expect(thrown(() => core.deviceListIngestAdd(G, body, 0n, PEER.user, encode([v1]), v1[1], 12n)))
      .toMatchObject({ code: 'E_DEVICE_UNLISTED' });
    expect(thrown(() => core.deviceListIngestAdd(G, body, 1n, PEER.user, encode([v1]), v1[1], 12n)))
      .toMatchObject({ code: 'E_DEVICE_LIST_STALE' });
    expect(core.trustGeneration()).toBe(generation);
  });

  it('commitBuild leaves a failed Add out and refuses one whose signed history is unknown', () => {
    const core = trusted();
    const v1 = listOf(1, [PEER.device]);
    core.deviceListIngest(G, 1, PEER.user, encode([v1]), v1[1], 10n);
    const FORGED = idOf(0xd0, 23);
    core.addKeyPackage(PEER.user, FORGED, fakeDskPub(FORGED), 1);
    core.addKeyPackage(THIRD.user, THIRD.device, fakeDskPub(THIRD.device), 1);
    expect(thrown(() => core.commitBuild(G, proposals(FORGED, THIRD.device)))).toMatchObject({ code: 'E_DEVICE_LIST_STALE' });
    const built = arr(decode(core.commitBuild(G, proposals(FORGED))), 5);
    expect(arr(at(arr(decode(bin(at(built, 1))), 5), 3))).toEqual([]);
    expect(arr(at(built, 3))).toEqual([]);
  });

  it('a valid Add candidate is ingested and committed', () => {
    const core = trusted();
    const v1 = listOf(1, [PEER.device]);
    core.deviceListIngest(G, 1, PEER.user, encode([v1]), v1[1], 10n);
    const v2 = listOf(2, [PEER.device, PEER_2]);
    core.addKeyPackage(PEER.user, PEER_2, fakeDskPub(PEER_2), 1);
    const body = proposals(PEER_2);
    expect(core.deviceListIngestAdd(G, body, 0n, PEER.user, encode([v1, v2]), v2[1], 12n)).toEqual({ version: 2n, changed: false, quarantined: 0 });
    const built = arr(decode(core.commitBuild(G, body)), 5);
    expect(arr(at(arr(decode(bin(at(built, 1))), 5), 3))).toEqual([PEER_2]);
  });

  // ---- ABI 8 (fixA-abi.md): the double mirrors trust_state's blocked, the own row, send_block_code, the bound safety
  // number, held messages released at the same seq, the recorded failed Add and the own-user foreign key.
  const CHANGED = bytes(32, 0xc4);
  const peerTwo = { user: PEER.user, device: PEER_2 };
  const msgFrom = (seq: bigint, from: { user: Id; device: Id }, body: string): Uint8Array =>
    encode([[seq, 0n, from.device, wire.app(from, idOf(0x7e, Number(seq)), body), new Uint8Array(32), new Uint8Array(32), 1_700_000_000n + seq, 0]]);
  const rowOf = (core: ModelCore, seq: bigint) => core.timeline(G, 0n, 200).find((row) => row.seq === seq);

  it('reports the stored verdict of a user\'s current leaves as blocked, a changed key winning', () => {
    const core = trusted();
    const v1 = listOf(1, [PEER.device]);
    core.deviceListIngest(G, 1, PEER.user, encode([v1]), v1[1], 10n);
    expect(core.trustState()).toEqual([ownRow(),
      { userId: PEER.user, umk: fakeDskPub(PEER.user), firstSeen: 10n, verified: false, changedUmk: null, blocked: 'E_DEVICE_UNLISTED' }]);
    core.setLeafVerdict(G, 1, 'E_UMK_CHANGED');
    expect(core.trustState()[1]).toMatchObject({ changedUmk: CHANGED, blocked: 'E_UMK_CHANGED' });
  });

  it('counts only leaves of an active group, and a Remove of the failed leaf clears its verdict', () => {
    const core = trusted();
    const OTHER_G = idOf(0x9b, 40);
    core.addPendingLeaf(OTHER_G, 1, THIRD.user, THIRD.device, fakeDskPub(THIRD.device), 1);
    const third = listOf(1, [THIRD.device], THIRD.user);
    core.deviceListIngest(OTHER_G, 1, THIRD.user, encode([third]), third[1], 10n);
    core.setLeafVerdict(OTHER_G, 1, 'E_CREDENTIAL');
    expect(toHex(core.trustState().at(-1)?.userId ?? new Uint8Array())).toBe(toHex(THIRD.user));
    expect(core.trustState().at(-1)?.blocked).toBeNull();
    const v1 = listOf(1, [PEER.device]);
    core.deviceListIngest(G, 1, PEER.user, encode([v1]), v1[1], 10n);
    const generation = core.trustGeneration();
    core.removeLeaf(G, 2);
    expect(core.trustGeneration()).toBe(generation + 1n);
    expect(core.trustState()[1]?.blocked).toBeNull();
    expect(core.sendBlockCode(G)).toBeNull();
  });

  it('answers send_block_code per group as send_prepare refuses, open as null, and fails closed for an unloaded group', () => {
    const core = trusted();
    expect(core.sendBlockCode(G)).toBeNull();
    const v1 = listOf(1, [PEER.device]);
    core.deviceListIngest(G, 1, PEER.user, encode([v1]), v1[1], 10n);
    expect(core.sendBlockCode(G)).toBe('E_DEVICE_UNLISTED');
    core.setLeafVerdict(G, 1, 'E_UMK_CHANGED');
    expect(core.sendBlockCode(G)).toBe('E_UMK_CHANGED');
    expect(thrown(() => core.sendPrepare(G, textRequest('x'), 2n))).toMatchObject({ code: 'E_UMK_CHANGED' });
    // A group whose state is not held answers from any stored verdict row of the group (fail closed).
    const UNLOADED = idOf(0x9b, 41);
    expect(core.sendBlockCode(UNLOADED)).toBeNull();
    core.addPendingLeaf(UNLOADED, 1, PEER.user, PEER_2, fakeDskPub(PEER_2), 1);
    core.setLeafVerdict(UNLOADED, 1, 'E_TIER_MISMATCH');
    expect(core.sendBlockCode(UNLOADED)).toBe('E_TIER_MISMATCH');
  });

  it('binds the safety number to the comparison key: the pending changed key, else the pinned one', () => {
    const core = trusted();
    expect(thrown(() => core.safetyNumberFor(PEER.user, fakeDskPub(PEER.user)))).toMatchObject({ code: 'E_CORE_NOT_FOUND' });
    const v1 = listOf(1, [PEER.device, PEER_2]);
    core.deviceListIngest(G, 1, PEER.user, encode([v1]), v1[1], 10n);
    const digits = core.safetyNumberFor(PEER.user, fakeDskPub(PEER.user));
    expect(digits).toMatch(/^\d{5}( \d{5}){11}$/);
    expect(thrown(() => core.safetyNumberFor(PEER.user, CHANGED))).toMatchObject({ code: 'E_DEVICE_LIST_STALE' });
    expect(thrown(() => core.safetyNumberFor(PEER.user, bytes(31, 1)))).toMatchObject({ code: 'E_CORE_INPUT' });
    core.setLeafVerdict(G, 1, 'E_UMK_CHANGED');
    expect(thrown(() => core.safetyNumberFor(PEER.user, fakeDskPub(PEER.user)))).toMatchObject({ code: 'E_DEVICE_LIST_STALE' });
    expect(core.safetyNumberFor(PEER.user, CHANGED)).not.toBe(digits);
  });

  it('holds a failed leaf\'s message unreadable and releases it at the same seq when a later list names the leaf', () => {
    const core = trusted();
    const v1 = listOf(1, [PEER.device]);
    core.deviceListIngest(G, 1, PEER.user, encode([v1]), v1[1], 10n);
    core.groupApply(G, encode([]), msgFrom(1n, peerTwo, 'held back'), 1n);
    expect(rowOf(core, 1n)).toMatchObject({ status: 1, reason: 'E_DEVICE_UNLISTED', body: '' });
    expect(core.revalidatePending(G, 2, 256)).toBe(0);
    const v2 = listOf(2, [PEER.device, PEER_2]);
    core.deviceListIngest(G, 2, PEER.user, encode([v1, v2]), v2[1], 11n);
    const generation = core.trustGeneration();
    expect(core.revalidatePending(G, 2, 256)).toBe(1);
    expect(core.trustGeneration()).toBe(generation + 1n);
    expect(rowOf(core, 1n)).toMatchObject({ status: 0, reason: '', body: 'held back', senderUser: PEER.user, senderDevice: PEER_2 });
    expect(core.revalidatePending(G, 2, 256)).toBe(0);
    expect(thrown(() => core.revalidatePending(G, 2, 0))).toMatchObject({ code: 'E_CORE_INPUT' });
  });

  it('trust_verify approves only the comparison key and releases the changed key\'s held messages at their seq', () => {
    const core = trusted();
    const v1 = listOf(1, [PEER.device, PEER_2]);
    core.deviceListIngest(G, 1, PEER.user, encode([v1]), v1[1], 10n);
    core.setLeafVerdict(G, 1, 'E_UMK_CHANGED');
    core.groupApply(G, encode([]), msgFrom(1n, PEER, 'under the new key'), 1n);
    expect(rowOf(core, 1n)).toMatchObject({ status: 1, reason: 'E_UMK_CHANGED' });
    expect(thrown(() => core.trustVerify(PEER.user, fakeDskPub(PEER.user)))).toMatchObject({ code: 'E_DEVICE_LIST_STALE' });
    const generation = core.trustGeneration();
    core.trustVerify(PEER.user, CHANGED);
    expect(core.trustGeneration()).toBeGreaterThan(generation);
    // PEER_2's leaf still carries the replaced key: under the approved pin it fails (leaf_verdict: another key).
    expect(core.trustState()[1]).toMatchObject({ umk: CHANGED, verified: true, changedUmk: null, blocked: 'E_CREDENTIAL' });
    expect(rowOf(core, 1n)).toMatchObject({ status: 0, body: 'under the new key' });
    expect(core.sendBlockCode(G)).toBe('E_CREDENTIAL');
    core.removeLeaf(G, 2);
    expect(core.trustState()[1]?.blocked).toBeNull();
    expect(core.sendBlockCode(G)).toBeNull();
    expect(thrown(() => core.trustVerify(THIRD.user, CHANGED))).toMatchObject({ code: 'E_DEVICE_LIST_STALE' });
  });

  it('records a fetched Add verdict so commitBuild leaves the Add out, also for a never-cached user', () => {
    const core = trusted();
    core.addKeyPackage(THIRD.user, THIRD.device, fakeDskPub(THIRD.device), 1);
    const body = proposals(THIRD.device);
    expect(thrown(() => core.commitBuild(G, body))).toMatchObject({ code: 'E_DEVICE_LIST_STALE' });
    const third = listOf(1, [], THIRD.user);
    expect(thrown(() => core.deviceListIngestAdd(G, body, 0n, THIRD.user, encode([third]), third[1], 12n)))
      .toMatchObject({ code: 'E_DEVICE_UNLISTED' });
    const built = arr(decode(core.commitBuild(G, body)), 5);
    expect(arr(at(arr(decode(bin(at(built, 1))), 5), 3))).toEqual([]);
    expect(arr(at(built, 3))).toEqual([]);
  });

  it('refuses an own-user leaf under another key as E_UMK_CHANGED on the own row; only its Remove clears it', () => {
    const core = trusted();
    const IMPOSTOR = idOf(0xd0, 50);
    core.addPendingLeaf(G, 3, ME.user, IMPOSTOR, fakeDskPub(IMPOSTOR), 1);
    core.setLeafVerdict(G, 3, 'E_UMK_CHANGED');
    // The own pin never takes a pending change.
    expect(core.trustState()[0]).toEqual(ownRow('E_UMK_CHANGED'));
    expect(core.pendingLeaves().find((leaf) => leaf.leafIndex === 3)?.reason).not.toBe('changed');
    expect(core.sendBlockCode(G)).toBe('E_UMK_CHANGED');
    const own = listOf(1, [ME.device, IMPOSTOR], ME.user);
    expect(thrown(() => core.deviceListIngest(G, 3, ME.user, encode([own]), own[1], 10n))).toMatchObject({ code: 'E_UMK_CHANGED' });
    expect(thrown(() => core.trustVerify(ME.user, CHANGED))).toMatchObject({ code: 'E_DEVICE_LIST_STALE' });
    core.removeLeaf(G, 3);
    expect(core.trustState()[0]).toEqual(ownRow());
    expect(core.sendBlockCode(G)).toBeNull();
  });

  it('says a send block is the own account\'s only when own-user leaves alone hold its code (trust.rs send_block_own, R1)', () => {
    const core = trusted();
    expect(core.sendBlockOwn(G)).toBe(false);
    const v1 = listOf(1, [PEER.device]);
    core.deviceListIngest(G, 1, PEER.user, encode([v1]), v1[1], 10n);
    expect(core.sendBlockCode(G)).toBe('E_DEVICE_UNLISTED');
    expect(core.sendBlockOwn(G)).toBe(false);
    const IMPOSTOR = idOf(0xd0, 52);
    core.addPendingLeaf(G, 3, ME.user, IMPOSTOR, fakeDskPub(IMPOSTOR), 1);
    core.setLeafVerdict(G, 3, 'E_UMK_CHANGED');
    expect(core.sendBlockCode(G)).toBe('E_UMK_CHANGED');
    expect(core.sendBlockOwn(G)).toBe(true);
    core.setLeafVerdict(G, 1, 'E_UMK_CHANGED');
    expect(core.sendBlockOwn(G)).toBe(false);
    // A group whose state is not held is never read as the own account's.
    const UNLOADED = idOf(0x9b, 42);
    core.addPendingLeaf(UNLOADED, 1, ME.user, IMPOSTOR, fakeDskPub(IMPOSTOR), 1);
    core.setLeafVerdict(UNLOADED, 1, 'E_UMK_CHANGED');
    expect(core.sendBlockOwn(UNLOADED)).toBe(false);
    expect(thrown(() => core.sendBlockOwn(new Uint8Array(15)))).toMatchObject({ code: 'E_CORE_INPUT' });
  });

  it('records an own-user Add candidate under another key as E_UMK_CHANGED and leaves it out of the commit', () => {
    const core = trusted();
    const IMPOSTOR = idOf(0xd0, 51);
    core.addKeyPackage(ME.user, IMPOSTOR, fakeDskPub(IMPOSTOR), 1, CHANGED);
    const body = proposals(IMPOSTOR);
    const own = listOf(1, [ME.device, IMPOSTOR], ME.user);
    expect(thrown(() => core.deviceListIngestAdd(G, body, 0n, ME.user, encode([own]), own[1], 12n))).toMatchObject({ code: 'E_UMK_CHANGED' });
    const built = arr(decode(core.commitBuild(G, body)), 5);
    expect(arr(at(built, 3))).toEqual([]);
  });
});

// web-3 task 6 (lesson f): ModelCore's call groups answer what client/calls.rs answers (core/dilla-core/tests/client_calls.rs).
describe('ModelCore call groups (L-CORE-50..52)', () => {
  const VOICE_CH = idOf(0xc5, 9);
  const CALL = idOf(0x9c, 9);
  const codeOf = (fn: () => unknown): string => {
    try { fn(); } catch (e) { return (e as { code: string }).code; }
    return 'ok';
  };
  /** A model that knows bob's list and joins bob's call group of VOICE_CH; `tree` adds leaves to the served tree. */
  function joined(tree: { leafIndex: number; user: Id; device: Id; code?: string }[] = [], community: Id | null = COMMUNITY) {
    const ds = new ModelDs();
    ds.publishList(PEER.user, [{ device: PEER.device }]);
    const row = ds.listRow(PEER.user)!;
    const core = new ModelCore(ME);
    core.trustList(PEER.user, row.version, row.blob);
    core.setCallTree(CALL, [{ leafIndex: 0, user: PEER.user, device: PEER.device }, ...tree]);
    const eg: ExpectedGroup = { groupId: CALL, communityId: community, channelId: VOICE_CH, policyVersion: 1n };
    return { core, eg, info: encode([3, wire.info(CALL, 3n), bytes(32, 0), 4]), tree: encode([3, wire.tree(CALL, 3n), bytes(32, 0)]) };
  }

  it('creates a call group as kind 1 beside a text group of the same target; a second call group of the target is refused', () => {
    const core = new ModelCore(ME);
    core.groupCreate(idOf(0x9a, 1), null, VOICE_CH);
    const body = arr(decode(core.callGroupCreate(CALL, null, VOICE_CH)), 4);
    expect(arr(decode(bin(at(body, 1))))).toHaveLength(4);
    expect(core.groupRow(CALL)).toMatchObject({ kind: 1, state: 0 });
    expect(codeOf(() => core.callGroupCreate(CALL, null, VOICE_CH))).toBe('E_CORE_STATE');
    expect(codeOf(() => core.callGroupCreate(idOf(0x9c, 10), null, VOICE_CH))).toBe('E_CORE_STATE');
  });

  it('joins staged, keys only once active, and the key is copied out and its bytes zero-filled', () => {
    const { core, eg, info, tree } = joined();
    core.callGroupJoinExternal(eg, info, tree);
    expect(codeOf(() => core.callMediaEpoch(CALL))).toBe('E_CORE_STATE');
    core.callJoinCheck(CALL);
    core.groupJoined(CALL, 7n);
    const m = core.callMediaEpoch(CALL);
    expect(m.epoch).toBe(4n);
    expect(toHex(new Uint8Array(m.baseKey))).toBe(core.lastBaseKeyHex(CALL));
    expect(core.lastEpochBytes(CALL).every((b) => b === 0)).toBe(true);
    expect(m.roster.map((r) => r.leaf)).toEqual([0, 1]);
    expect(m.ownLeaf).toBe(1);
    expect(m.code).toMatch(/^(\d{5} ){5}\d{5}$/);
    expect(codeOf(() => core.callJoinCheck(CALL))).toBe('E_CORE_STATE');
  });

  it('refuses a join over a failed leaf and rolls it back (joining_a_call_group_that_already_holds_a_failed_leaf…)', () => {
    const { core, eg, info, tree } = joined([{ leafIndex: 1, user: THIRD.user, device: THIRD.device, code: 'E_DEVICE_UNLISTED' }]);
    expect(codeOf(() => core.callGroupJoinExternal(eg, info, tree))).toBe('E_DEVICE_UNLISTED');
    expect(core.group(CALL)).toBeUndefined();
    expect(core.pendingLeaves().filter((l) => toHex(l.groupId) === toHex(CALL))).toEqual([]);
  });

  it('tolerates a pending leaf at the join and refuses it at the check (a_staged_join_with_a_pending_leaf_posts_nothing_until_checked)', () => {
    const { core, eg, info, tree } = joined([{ leafIndex: 1, user: THIRD.user, device: THIRD.device }]);
    core.callGroupJoinExternal(eg, info, tree);
    expect(codeOf(() => core.callJoinCheck(CALL))).toBe('E_DEVICE_LIST_STALE');
    core.groupDiscard(CALL);
    expect(core.group(CALL)).toBeUndefined();
  });

  it('refuses a DM call while the DM text group is blocked, and a call group whose community disagrees with it (E_BINDING)', () => {
    const blocked = joined([], null);
    const text = idOf(0x9a, 2);
    blocked.core.seedGroup(text, 0, null, VOICE_CH);
    blocked.core.blockGroup(text, 'E_UMK_CHANGED');
    expect(codeOf(() => blocked.core.callGroupJoinExternal(blocked.eg, blocked.info, blocked.tree))).toBe('E_UMK_CHANGED');
    const bound = joined([], COMMUNITY);
    bound.core.seedGroup(text, 0, null, VOICE_CH);
    expect(codeOf(() => bound.core.callGroupJoinExternal(bound.eg, bound.info, bound.tree))).toBe('E_BINDING');
  });

  it('no media key after a changed identity, nor while a leaf fails; skips messages and refuses sending', () => {
    const { core, eg, info, tree } = joined();
    core.callGroupJoinExternal(eg, info, tree);
    core.groupJoined(CALL, 7n);
    expect(codeOf(() => core.sendPrepare(CALL, textRequest('hello'), 1n))).toBe('E_CORE_INPUT');
    core.changeIdentity(PEER.user);
    expect(codeOf(() => core.callMediaEpoch(CALL))).toBe('E_UMK_CHANGED');
    expect(codeOf(() => core.callMediaEpoch(idOf(0x9a, 77)))).toBe('E_CORE_NOT_FOUND');
    core.seedGroup(idOf(0x9a, 3), 0, COMMUNITY, CHANNEL);
    expect(codeOf(() => core.callMediaEpoch(idOf(0x9a, 3)))).toBe('E_CORE_INPUT');
  });

  it('leave, rotate and forget: the rotation withdraws the own Remove; a forgotten group answers E_CORE_STATE', () => {
    const { core, eg, info, tree } = joined();
    core.callGroupJoinExternal(eg, info, tree);
    core.groupJoined(CALL, 7n);
    expect(arr(decode(core.callLeaveProposal(CALL)), 2)[0]).toBe(4n);
    expect(core.groupRow(CALL)?.proposalsPending).toBe(1);
    const built = arr(decode(core.callRotate(CALL)), 5);
    expect(arr(decode(bin(at(built, 1))), 5)[4]).toEqual([]);
    core.commitConfirm(CALL);
    core.callGroupForget(CALL);
    expect(core.groupRow(CALL)?.state).toBe(4);
    expect(codeOf(() => core.callMediaEpoch(CALL))).toBe('E_CORE_STATE');
    expect(codeOf(() => core.callGroupForget(CALL))).toBe('ok');
  });
});

// v0.1.0-web3.1: the model answers the create routes as internal/api does, so a suite builds its server and its channels
// through the client path instead of seeding them (the standing rule of the create-flow incident).
describe('ModelDs creates servers and channels as internal/api does', () => {
  const codeOf = async (p: Promise<unknown>): Promise<string> => p.then(() => 'ok', (e: unknown) => (e as { code: string }).code);

  it('lists a created server for its owner, and its channels by name, kind and position', async () => {
    const ds = new ModelDs();
    ds.own(ME.user, ME.device);
    const me = ds.routesFor(ME.device);
    expect(await me.listCommunities()).toEqual([]);
    const created = await me.createCommunity('Midgard Crew');
    expect(created.policyVersion).toBe(1n);
    expect(await me.listCommunities()).toEqual([{ id: created.id, name: 'Midgard Crew', owner: ME.user, policyVersion: 1n }]);
    const text = await me.createChannel(created.id, { kind: 'text', name: 'general', position: 0 });
    const voice = await me.createChannel(created.id, { kind: 'voice', name: 'general', position: 1 });
    expect(text).toMatchObject({ mode: 0, visibility: 0 });
    expect((await me.listChannels(created.id)).map((c) => [toHex(c.id), c.kind, c.name, c.position, c.textGroupId])).toEqual([
      [toHex(text.id), 0, 'general', 0, null], [toHex(voice.id), 1, 'general', 1, null],
    ]);
    // A seeded channel of another server keeps its old rows.
    expect(await me.listChannels(COMMUNITY)).toEqual([]);
  });

  it('refuses a channel to a member who is not the owner (403), an unknown server (404) and a bad name (400)', async () => {
    const ds = new ModelDs();
    ds.own(ME.user, ME.device);
    ds.own(PEER.user, PEER.device);
    const created = await ds.routesFor(ME.device).createCommunity('Midgard Crew');
    ds.addMember(created.id, PEER.user);
    const peer = ds.routesFor(PEER.device);
    expect((await peer.listCommunities()).map((c) => c.name)).toEqual(['Midgard Crew']);
    expect(await codeOf(peer.createChannel(created.id, { kind: 'text', name: 'mine', position: 0 }))).toBe('E_FORBIDDEN');
    expect(await codeOf(peer.createChannel(idOf(0xc9, 9), { kind: 'text', name: 'x', position: 0 }))).toBe('E_NOT_FOUND');
    expect(await codeOf(ds.routesFor(ME.device).createChannel(created.id, { kind: 'text', name: 'a\u0000b', position: 0 }))).toBe('E_INVALID_REQUEST');
    expect(await codeOf(ds.routesFor(ME.device).createCommunity(''))).toBe('E_INVALID_REQUEST');
    // Every member may mint an invite (@everyone holds CreateInvite by default); a stranger may not see the server.
    expect((await peer.createInvite(created.id, { maxUses: 10, ttlSeconds: 604_800 })).url).toMatch(/^https:\/\/model\.test\/i\/\S+$/);
    expect(await codeOf(ds.routesFor(THIRD.device).createInvite(created.id, { maxUses: 10, ttlSeconds: 60 }))).toBe('E_NOT_FOUND');
  });

  it('answers the owner every permission at the top rank, any other member the @everyone base at rank 0, and a stranger 404', async () => {
    const ds = new ModelDs();
    ds.own(ME.user, ME.device);
    ds.own(PEER.user, PEER.device);
    const created = await ds.routesFor(ME.device).createCommunity('Midgard Crew');
    ds.addMember(created.id, PEER.user);
    expect(await ds.routesFor(ME.device).getPermissions(created.id)).toEqual({ bits: (1n << 30n) - 1n, top: 2 ** 31 });
    const base = await ds.routesFor(PEER.device).getPermissions(created.id);
    expect(base.top).toBe(0);
    expect(base.bits & (1n << 11n)).not.toBe(0n);   // CreateInvite
    expect(base.bits & (1n << 12n)).toBe(0n);       // KickMembers
    const roles = await ds.routesFor(PEER.device).listRoles(created.id);
    expect(roles.map((r) => [r.name, r.position, r.allow])).toEqual([['@everyone', 0, Number(base.bits)]]);
    expect(await codeOf(ds.routesFor(THIRD.device).getPermissions(created.id))).toBe('E_NOT_FOUND');
    expect(await codeOf(ds.routesFor(THIRD.device).listRoles(created.id))).toBe('E_NOT_FOUND');
  });
});
