import { describe, expect, it } from 'vitest';
import { decode, encode, type CborInput } from './cbor';
import { CoreError, storeNewerOf, trustBlock, wrapCore, type CoreHandle } from './core-port';
import { fromHex } from './hex';

const G = new Uint8Array(16).fill(0x67);
const COMM = new Uint8Array(16).fill(0xc1);
const CHAN = new Uint8Array(16).fill(0xc2);
const INST = new Uint8Array(16).fill(0xab);
const USER = new Uint8Array(16).fill(0xa2);
const DEV = new Uint8Array(16).fill(0xa1);
const MSG = new Uint8Array(16).fill(0x11);
const MSG2 = new Uint8Array(16).fill(0x12);

type Impl = Partial<Record<keyof CoreHandle, (...args: never[]) => unknown>>;

function stub(impl: Impl) {
  const calls: { method: string; args: unknown[] }[] = [];
  const handle = new Proxy({}, {
    get(_target, prop) {
      return (...args: unknown[]) => {
        calls.push({ method: String(prop), args });
        const f = impl[prop as keyof CoreHandle] as ((...a: unknown[]) => unknown) | undefined;
        if (f === undefined) throw new Error(`stub: ${String(prop)} is not stubbed`);
        return f(...args);
      };
    },
  }) as unknown as CoreHandle;
  return { port: wrapCore(handle), calls };
}

function caught(fn: () => unknown): CoreError {
  try {
    fn();
  } catch (err) {
    if (err instanceof CoreError) return err;
    throw err;
  }
  throw new Error('nothing was thrown');
}

it('adapts the ABI-8 trust and search exports', () => {
  const key = new Uint8Array(32).fill(0x4a);
  const history = encode([[1, new Uint8Array([9]), new Uint8Array(64), new Uint8Array(32)]]);
  const head = encode([1, new Uint8Array([9]), new Uint8Array(64), new Uint8Array(32)]);
  const s = stub({
    pending_leaves: () => encode([[G, 0, USER, 0, 'unknown']]),
    device_list_ingest: () => encode([1, 0, 0]),
    trust_state: () => encode([[USER, key, 10, 0, null, 'E_DEVICE_UNLISTED']]),
    trust_verify: () => undefined,
    safety_number_for: () => Array.from({ length: 12 }, () => '12345').join(' '),
    send_block_code: () => 'E_UMK_CHANGED',
    search: () => encode([[G, 7, 'boat', 0]]),
  });
  expect(s.port.pendingLeaves()).toMatchObject([{ groupId: G, userId: USER, leafIndex: 0, cachedVersion: 0n }]);
  expect(s.port.deviceListIngest(G, 0, USER, history, head, 12n)).toEqual({ version: 1n, changed: false, quarantined: 0 });
  expect(s.port.trustState()).toEqual([{ userId: USER, umk: key, firstSeen: 10n, verified: false, changedUmk: null, blocked: 'E_DEVICE_UNLISTED' }]);
  s.port.trustVerify(USER, key);
  expect(s.port.safetyNumberFor(USER, key)).toBe(Array.from({ length: 12 }, () => '12345').join(' '));
  expect(s.port.sendBlockCode(G)).toBe('E_UMK_CHANGED');
  expect(s.port.search({ kind: 'all', id: null }, 'boat', null, 50)).toEqual([{ groupId: G, seq: 7n, snippet: 'boat', rank: 0n, threadRoot: null }]);
  expect(s.calls.map((c) => c.method)).toEqual([
    'pending_leaves', 'device_list_ingest', 'trust_state', 'trust_verify', 'safety_number_for', 'send_block_code', 'search',
  ]);
  expect(s.calls[1]?.args).toEqual([G, 0, USER, history, head, 12n]);
  // The safety number is bound to the key the page will approve (ABI 8).
  expect(s.calls[4]?.args).toEqual([USER, key]);
  expect(s.calls[5]?.args).toEqual([G]);
});

it('passes moderator-removal signing inputs directly to ClientCore and returns only its signed body', () => {
  const signed = encode([new Uint8Array([1]), new Uint8Array(64).fill(7)]);
  const requestId = new Uint8Array(16).fill(0x41);
  const s = stub({ sign_removal: (...args) => { expect(args).toEqual([requestId, G, 9n, 140n, 110n]); return signed; } });
  expect(s.port.signRemoval(requestId, G, 9n, 140n, 110n)).toBe(signed);
  expect(s.calls).toEqual([{ method: 'sign_removal', args: [requestId, G, 9n, 140n, 110n] }]);
});

  it('bridges the durable manual-removal evidence record and preserves its exact payload fields', () => {
  const proof = new Uint8Array(16).fill(0x41); const evidence = new Uint8Array(16).fill(0x42);
  const body = encode([new Uint8Array([1]), new Uint8Array(64).fill(7)]);
  const row = [proof, evidence, USER, DEV, COMM, G, CHAN, MSG, 9n, 'why', 'excerpt', body, 1, 100n, 130n, 160n];
  const s = stub({
    prepare_manual_removal_evidence: (...args) => { expect(args).toEqual([proof, evidence, COMM, G, CHAN, MSG, 9n, 'why', 'excerpt', 100n, 130n, 100n]); return body; },
    manual_removal_evidence: () => encode([row]),
  });
  expect(s.port.prepareManualRemovalEvidence?.(proof, evidence, COMM, G, CHAN, MSG, 9n, 'why', 'excerpt', 100n, 130n, 100n)).toBe(body);
  expect(s.port.manualRemovalEvidence?.()).toEqual([{ proofRequestId: proof, evidenceRequestId: evidence, actorUserId: USER,
    actorDeviceId: DEV, communityId: COMM, groupId: G, channelId: CHAN, messageId: MSG, targetSequence: 9n,
    reason: 'why', excerpt: 'excerpt', signedBody: body, state: 1, createdAt: 100n, expires: 130n, evidenceExpires: 160n }]);
});

it('bridges the crash-safe manual-warning outbox and accepted action receipt', () => {
  const request = new Uint8Array(16).fill(0x31); const evidence = new Uint8Array(16).fill(0x32);
  const target = new Uint8Array(16).fill(0x33); const action = new Uint8Array(16).fill(0x34);
  const row = [request, evidence, USER, DEV, COMM, target, 'warning reason', 1, 100n, action, 9n, 101n, 131n];
  const s = stub({
    prepare_manual_warning_evidence: (...args) => { expect(args).toEqual([request, evidence, COMM, target, 'warning reason', 100n, 100n]); },
    manual_warning_evidence: () => encode([row]),
    mark_manual_warning_accepted: (...args) => { expect(args).toEqual([request, action, 9n, 101n]); },
    ensure_manual_warning_evidence_request: (...args) => { expect(args).toEqual([request, evidence, 102n]); return encode([evidence, 132n]); },
    complete_manual_warning_evidence: (...args) => { expect(args).toEqual([request, 103n]); },
  });
  expect(s.port.prepareManualWarningEvidence?.(request, evidence, COMM, target, 'warning reason', 100n, 100n)).toBeUndefined();
  expect(s.port.manualWarningEvidence?.()).toEqual([{ requestId: request, evidenceRequestId: evidence, actorUserId: USER,
    actorDeviceId: DEV, communityId: COMM, targetUserId: target, reason: 'warning reason', state: 1,
    createdAt: 100n, actionId: action, revision: 9n, acceptedAt: 101n, evidenceExpires: 131n }]);
  s.port.markManualWarningAccepted?.(request, action, 9n, 101n);
  expect(s.port.ensureManualWarningEvidenceRequest?.(request, evidence, 102n)).toEqual({ requestId: evidence, expires: 132n });
  s.port.completeManualWarningEvidence?.(request, 103n);
});

it('bridges authoritative rejection of a submitted manual-removal proof', () => {
  const proof = new Uint8Array(16).fill(0x41);
  const s = stub({ reject_manual_removal: (...args) => { expect(args).toEqual([proof, 123n]); } });
  s.port.rejectManualRemoval?.(proof, 123n);
  expect(s.calls).toEqual([{ method: 'reject_manual_removal', args: [proof, 123n] }]);
});

it('passes an opaque human interaction context proof to the dedicated Welcome export', () => {
  const welcomes = encode([[1, G, 2, 9, new Uint8Array([1]), new Uint8Array([2]), new Uint8Array(32)]]);
  const proof = new Uint8Array([0x81, 0x00]);
  const result = encode([[1, G, 0, '']]);
  const s = stub({ human_interaction_welcome_apply: (...args) => {
    expect(args).toEqual([welcomes, proof, 100n]);
    return result;
  } });
  expect(s.port.humanInteractionWelcomeApply(welcomes, proof, 100n)).toEqual([
    { welcomeId: 1n, groupId: G, outcome: 0, reason: '' },
  ]);
  expect(s.calls).toEqual([{ method: 'human_interaction_welcome_apply', args: [welcomes, proof, 100n] }]);
});

it('decodes authenticated human interaction events and passes a durable cursor', () => {
  const event = [5, G, COMM, MSG, 8, 3, 99, USER, DEV, MSG2, 'dilla.bot/1:response', 13, CHAN];
  const page = encode([[event], 5, 7]);
  const s = stub({ human_interaction_events_read: (...args) => { expect(args).toEqual([2n, 10n]); return page; },
    human_interaction_events_ack: (...args) => { expect(args).toEqual([5n]); },
    human_interaction_events_ack_selected: (...args) => { expect(args).toEqual([encode([5n, 9n])]); } });
  expect(s.port.humanInteractionEventsRead(2n, 10)).toEqual({
    events: [{ deliveryId: 5n, groupId: G, communityId: COMM, requestId: MSG, sourceSeq: 8n, epoch: 3n,
      receivedAt: 99n, senderUser: USER, senderDevice: DEV, messageId: MSG2, body: 'dilla.bot/1:response',
      operation: 13, channelId: CHAN }],
    next: 5n, highWater: 7n,
  });
  s.port.humanInteractionEventsAck(5n);
  s.port.humanInteractionEventsAckSelected([5n, 9n]);
  expect(s.calls.map((call) => call.method)).toEqual(['human_interaction_events_read', 'human_interaction_events_ack', 'human_interaction_events_ack_selected']);
});

it('decodes exact archived human interaction receipts without a cursor', () => {
  const event = [5, G, COMM, MSG, 8, 3, 99, USER, DEV, MSG2, 'dilla.bot/1:response', 84, null];
  const requests = [MSG];
  const s = stub({ human_interaction_receipts_read: (...args) => {
    expect(args).toEqual([COMM, CHAN, USER, encode(requests)]);
    return encode([event]);
  } });
  expect(s.port.humanInteractionReceiptsRead(COMM, CHAN, USER, requests)).toEqual([{
    deliveryId: 5n, groupId: G, communityId: COMM, requestId: MSG, sourceSeq: 8n, epoch: 3n,
    receivedAt: 99n, senderUser: USER, senderDevice: DEV, messageId: MSG2,
    body: 'dilla.bot/1:response', operation: 84, channelId: null,
  }]);
});

it('fails closed when the generated WASM handle predates moderator-removal signing', () => {
  const staleHandle = { sign_removal: undefined } as unknown as CoreHandle;
  const error = caught(() => wrapCore(staleHandle).signRemoval(new Uint8Array(16), G, 9n, 140n, 110n));
  expect(error.code).toBe('E_CORE_STATE');
  expect(error.message).toContain('not available in this WASM build');
});

it('reads an open send gate as null and fails closed on a code it does not know', () => {
  expect(stub({ send_block_code: () => undefined }).port.sendBlockCode(G)).toBeNull();
  for (const code of ['E_CREDENTIAL', 'E_DEVICE_UNLISTED', 'E_TIER_MISMATCH'] as const)
    expect(stub({ send_block_code: () => code }).port.sendBlockCode(G)).toBe(code);
  expect(stub({ send_block_code: () => 'E_NEW' }).port.sendBlockCode(G)).toBe('E_CREDENTIAL');
  expect(caught(() => stub({ send_block_code: () => 'x' }).port.sendBlockCode(new Uint8Array(15))).code).toBe('E_CORE_INPUT');
});

it('reads whether a send block is the own account\'s, and checks the group id first (R1)', () => {
  const s = stub({ send_block_own: () => true });
  expect(s.port.sendBlockOwn(G)).toBe(true);
  expect(s.calls.map((c) => [c.method, c.args])).toEqual([['send_block_own', [G]]]);
  expect(stub({ send_block_own: () => false }).port.sendBlockOwn(G)).toBe(false);
  expect(caught(() => stub({ send_block_own: () => true }).port.sendBlockOwn(new Uint8Array(15))).code).toBe('E_CORE_INPUT');
});

// web-3 task 6 (progress note of task 5): the call gate's E_BINDING (a DM call group bound to a made-up community) is a
// trust block like the four verdicts, never folded into E_CREDENTIAL.
it('keeps E_BINDING as a trust block of its own', () => {
  expect(stub({ send_block_code: () => 'E_BINDING' }).port.sendBlockCode(G)).toBe('E_BINDING');
  expect(trustBlock('E_BINDING')).toBe('E_BINDING');
});

describe('wrapCore web-3 calls (L-TS-55)', () => {
  const KEY = Uint8Array.from({ length: 16 }, (_, i) => 0xa0 + i);
  const CODE = '12345 67890 12345 67890 12345 67890';
  const epochBody = (): Uint8Array => encode([7, KEY, 1, [[0, DEV, USER], [1, new Uint8Array(16).fill(0x33), COMM]], CODE]);

  it('copies the key into a fresh 16-byte buffer and zero-fills the whole decoded buffer the facade returned', () => {
    const raw = epochBody();
    const s = stub({ call_media_epoch: () => raw });
    const m = s.port.callMediaEpoch(G);
    expect(m.epoch).toBe(7n);
    expect(m.baseKey).toBeInstanceOf(ArrayBuffer);
    expect(m.baseKey.byteLength).toBe(16);
    expect(new Uint8Array(m.baseKey)).toEqual(KEY);
    expect(m.ownLeaf).toBe(1);
    expect(m.roster).toEqual([{ leaf: 0, deviceId: DEV, userId: USER }, { leaf: 1, deviceId: new Uint8Array(16).fill(0x33), userId: COMM }]);
    expect(m.code).toBe(CODE);
    // Nothing of the key is left in the bytes the facade handed over, and the copy shares no memory with them.
    expect(raw.every((b) => b === 0)).toBe(true);
    expect(m.baseKey).not.toBe(raw.buffer);
    expect(s.calls).toEqual([{ method: 'call_media_epoch', args: [G] }]);
  });

  it('zero-fills the decoded buffer also when it does not decode', () => {
    const raw = encode([7, KEY.slice(0, 15), 1, [], CODE]);
    expect(caught(() => stub({ call_media_epoch: () => raw }).port.callMediaEpoch(G)).code).toBe('E_CORE_DECODE');
    expect(raw.every((b) => b === 0)).toBe(true);
    const bad = encode([7, KEY, 1, [], 'not a code']);
    expect(caught(() => stub({ call_media_epoch: () => bad }).port.callMediaEpoch(G)).code).toBe('E_CORE_DECODE');
    expect(bad.every((b) => b === 0)).toBe(true);
  });

  it('maps the call gate refusal to its code', () => {
    const s = stub({ call_media_epoch: () => { throw new Error('E_UMK_CHANGED'); } });
    expect(caught(() => s.port.callMediaEpoch(G)).code).toBe('E_UMK_CHANGED');
  });

  it('forwards the six other call exports in the facade argument order', () => {
    const s = stub({
      call_group_create: () => BODY_OUT, call_group_join_external: () => BODY_OUT, call_join_check: () => undefined,
      call_leave_proposal: () => BODY_OUT, call_rotate: () => BODY_OUT, call_group_forget: () => undefined,
    });
    const info = encode(['info']);
    const tree = encode(['tree']);
    const pub = new Uint8Array(32).fill(2);
    expect(s.port.callGroupCreate(G, null, CHAN, 3n, pub)).toBe(BODY_OUT);
    expect(s.port.callGroupJoinExternal({ groupId: G, communityId: COMM, channelId: CHAN, policyVersion: 3n }, info, tree)).toBe(BODY_OUT);
    s.port.callJoinCheck(G);
    expect(s.port.callLeaveProposal(G)).toBe(BODY_OUT);
    expect(s.port.callRotate(G)).toBe(BODY_OUT);
    s.port.callGroupForget(G);
    expect(s.calls.map((c) => [c.method, c.args])).toEqual([
      ['call_group_create', [G, null, CHAN, 3n, pub]],
      ['call_group_join_external', [G, COMM, CHAN, 3n, info, tree]],
      ['call_join_check', [G]], ['call_leave_proposal', [G]], ['call_rotate', [G]], ['call_group_forget', [G]],
    ]);
  });
});
const BODY_OUT = new Uint8Array([0x82, 0x01, 0x02]);

it('refuses a safety-number key that is not 32 bytes before calling the core', () => {
  const s = stub({ safety_number_for: () => 'never' });
  expect(caught(() => s.port.safetyNumberFor(USER, new Uint8Array(31))).code).toBe('E_CORE_INPUT');
  expect(s.calls).toEqual([]);
});

it('refuses a non-bit changed flag in a wasm ingest reply', () => {
  const s = stub({ device_list_ingest: () => encode([1, 2, 0]) });
  expect(caught(() => s.port.deviceListIngest(G, 0, USER, encode([]), encode([]), 12n)).code).toBe('E_CORE_WASM');
});

describe('wrapCore decoding', () => {
  it('reads identity in every phase', () => {
    expect(stub({ identity: () => encode([0, null, null, null, '', 0]) }).port.identity())
      .toEqual({ phase: 0, instanceId: null, userId: null, deviceId: null, username: '', listPublished: false });
    expect(stub({ identity: () => encode([2, INST, USER, DEV, 'ada', 1]) }).port.identity())
      .toEqual({ phase: 2, instanceId: INST, userId: USER, deviceId: DEV, username: 'ada', listPublished: true });
  });

  it('reads and writes the session record', () => {
    expect(stub({ session: () => encode(null) }).port.session()).toBeNull();
    expect(stub({ session: () => encode(['tok', 10, 5]) }).port.session()).toEqual({ token: 'tok', expires: 10n, idleExpires: 5n });
    const s = stub({ session_store: () => undefined });
    s.port.sessionStore({ token: 'tok', expires: 10n, idleExpires: 5n });
    expect(s.calls).toEqual([{ method: 'session_store', args: ['tok', 10n, 5n] }]);
  });

  it('reads groups', () => {
    const { port } = stub({ groups: () => encode([[G, 0, COMM, CHAN, 2, 7, 12, 1, 1], [MSG, 0, null, CHAN, 4, 0, 3, 0, 0]]) });
    expect(port.groups()).toEqual([
      { groupId: G, kind: 0, communityId: COMM, targetId: CHAN, state: 2, epoch: 7n, nextSeq: 12n, proposalsPending: 1, pendingCommit: true },
      { groupId: MSG, kind: 0, communityId: null, targetId: CHAN, state: 4, epoch: 0n, nextSeq: 3n, proposalsPending: 0, pendingCommit: false },
    ]);
  });

  it('reads apply results and both flag bits', () => {
    const s = stub({ group_apply: () => encode([2, 7, 12, [10, 11], 1, 3]), commit_confirm: () => encode([2, 8, 12, [], 0, 1]) });
    const h = new Uint8Array([1]);
    const m = new Uint8Array([2]);
    expect(s.port.groupApply(G, h, m, 9n)).toEqual({ state: 2, epoch: 7n, nextSeq: 12n, newSeqs: [10n, 11n], proposalsPending: 1, epochChanged: true, ownAdopted: true });
    expect(s.calls[0]).toEqual({ method: 'group_apply', args: [G, h, m, 9n] });
    expect(s.port.commitConfirm(G)).toEqual({ state: 2, epoch: 8n, nextSeq: 12n, newSeqs: [], proposalsPending: 0, epochChanged: true, ownAdopted: false });
  });

  it('reads the result of a deletion with the apply decoder', () => {
    // [2, 1, 3, [2], 0, 0]: state 2, epoch 1, next_seq 3, new_seqs [2], no proposals, no flags
    const s = stub({ message_deleted: () => fromHex('8602010381020000') });
    expect(s.port.messageDeleted(G, 2n)).toEqual({ state: 2, epoch: 1n, nextSeq: 3n, newSeqs: [2n], proposalsPending: 0, epochChanged: false, ownAdopted: false });
    expect(s.calls).toEqual([{ method: 'message_deleted', args: [G, 2n] }]);
  });

  it('passes the expected groups as CBOR and reads welcome outcomes', () => {
    const s = stub({ welcomes_apply: () => encode([[4, G, 0, ''], [5, MSG, 2, 'E_BINDING']]) });
    const out = s.port.welcomesApply(new Uint8Array([9]), [{ groupId: G, communityId: COMM, channelId: CHAN, policyVersion: 3n }]);
    expect(out).toEqual([
      { welcomeId: 4n, groupId: G, outcome: 0, reason: '' },
      { welcomeId: 5n, groupId: MSG, outcome: 2, reason: 'E_BINDING' },
    ]);
    expect(decode(s.calls[0]?.args[1] as Uint8Array)).toEqual([[G, COMM, CHAN, 3n]]);
  });

  it('forwards the external join in ledger order', () => {
    const body = new Uint8Array([7, 7]);
    const s = stub({ group_join_external: () => body });
    const info = new Uint8Array([1]);
    const tree = new Uint8Array([2]);
    expect(s.port.groupJoinExternal({ groupId: G, communityId: COMM, channelId: CHAN, policyVersion: 3n }, info, tree)).toBe(body);
    expect(s.calls[0]?.args).toEqual([G, COMM, CHAN, 3n, info, tree]);
  });

  it('reads the send results, the outbox and the timeline', () => {
    const { port } = stub({
      send_prepare: () => encode([MSG]),
      send_encrypt: () => encode([G, new Uint8Array([5, 6])]),
      send_confirm: () => encode([G, 42]),
      outbox: () => encode([[MSG, 2, 'E_TOO_LARGE', 1_700_000_000, 'hi', 0, null, []]]),
      timeline: () => encode([
        [5, 7, 1_700_000_000, 0, '', USER, DEV, 0, 1, MSG, 0, 'hi', 0, null, [], 0, [], 0],
        [6, 7, 1_700_000_001, 1, 'E_PRUNED', null, DEV, null, null, null, null, '', 0, null, [], 0, [], 0],
        [9, 7, 1_700_000_004, 0, '', USER, DEV, 0, 1, MSG2, 0, 'edited', 8, [MSG, 5, USER, 'hi', 0], [['👍', 2, 1], ['🎉', 1, 0]], 1,
          [[0, 1234, 'image/png', 640, 480, 1, 'map.png'], [1, 70000, 'application/pdf', null, null, 0, '']], 1],
      ]),
    });
    expect(port.sendPrepare(G, { type: 0, replyTo: null, body: 'hi', attachments: [], threadId: null }, 1n)).toEqual(MSG);
    expect(port.sendEncrypt(MSG)).toEqual({ groupId: G, messageBody: new Uint8Array([5, 6]) });
    expect(port.sendConfirm(MSG, new Uint8Array([1]))).toEqual({ groupId: G, seq: 42n });
    expect(port.outbox(G)).toEqual([{ msgId: MSG, state: 2, error: 'E_TOO_LARGE', created: 1_700_000_000n, body: 'hi', type: 0, replyTo: null, attachments: [] }]);
    const plain = { editedSeq: 0n, reply: null, reactions: [], pinned: false, attachments: [], mention: false, thread: null };
    expect(port.timeline(G, 0n, 100)).toEqual([
      { seq: 5n, epoch: 7n, recvTs: 1_700_000_000n, status: 0, reason: '', senderUser: USER, senderDevice: DEV, senderKind: 0, senderTier: 1, msgId: MSG, type: 0, body: 'hi', ...plain },
      { seq: 6n, epoch: 7n, recvTs: 1_700_000_001n, status: 1, reason: 'E_PRUNED', senderUser: null, senderDevice: DEV, senderKind: null, senderTier: null, msgId: null, type: null, body: '', ...plain },
      { seq: 9n, epoch: 7n, recvTs: 1_700_000_004n, status: 0, reason: '', senderUser: USER, senderDevice: DEV, senderKind: 0, senderTier: 1, msgId: MSG2, type: 0, body: 'edited',
        editedSeq: 8n, reply: { replyTo: MSG, targetSeq: 5n, targetUser: USER, excerpt: 'hi', state: 0, threadRoot: null },
        reactions: [{ emoji: '👍', count: 2, mine: true }, { emoji: '🎉', count: 1, mine: false }], pinned: true,
        attachments: [{ index: 0, size: 1234, mime: 'image/png', w: 640, h: 480, hasThumb: true, name: 'map.png' },
          { index: 1, size: 70000, mime: 'application/pdf', w: null, h: null, hasThumb: false, name: '' }],
        mention: true, thread: null },
    ]);
  });

  it('reads the thread summary of a 19-element row, and an 18-element row as no thread (web3.7)', () => {
    const base: CborInput[] = [5, 7, 1_700_000_000, 0, '', USER, DEV, 0, 1, MSG, 0, 'root', 0, null, [], 0, [], 0];
    const { port } = stub({ timeline: () => encode([[...base, [3, 1_700_000_009]], [...base, null], base]) });
    expect(port.timeline(G, 0n, 100).map((r) => r.thread)).toEqual([{ count: 3, lastTs: 1_700_000_009n }, null, null]);
    const decodeOne = (row: CborInput[]) => caught(() => stub({ timeline: () => encode([row]) }).port.timeline(G, 0n, 100)).code;
    expect(decodeOne([...base, [3]])).toBe('E_CORE_DECODE');
    expect(decodeOne([...base, [3, 1, 2]])).toBe('E_CORE_DECODE');
    expect(decodeOne([...base, null, null])).toBe('E_CORE_DECODE');
  });

  it('reads a thread timeline and the threads of a group, in the facade argument order (web3.7)', () => {
    const root: CborInput[] = [5, 7, 1_700_000_000, 0, '', USER, DEV, 0, 1, MSG, 0, 'root', 0, null, [], 0, [], 0, [1, 1_700_000_002]];
    const reply: CborInput[] = [6, 7, 1_700_000_002, 0, '', USER, DEV, 0, 1, MSG2, 0, 'reply', 0, null, [], 0, [], 0, null];
    const s = stub({
      thread_timeline: () => encode([root, [reply]]),
      threads: () => encode([[MSG, 5, 1, 1_700_000_002, 'root'], [MSG2, null, 2, 1_700_000_001, '']]),
    });
    const t = s.port.threadTimeline(G, MSG, 0n, 50);
    expect(s.calls[0]).toEqual({ method: 'thread_timeline', args: [G, MSG, 0n, 50] });
    expect(t.root?.seq).toBe(5n);
    expect(t.root?.thread).toEqual({ count: 1, lastTs: 1_700_000_002n });
    expect(t.rows.map((r) => [r.seq, r.body, r.thread])).toEqual([[6n, 'reply', null]]);
    expect(s.port.threads(G)).toEqual([
      { rootMsgId: MSG, rootSeq: 5n, count: 1, lastTs: 1_700_000_002n, excerpt: 'root' },
      { rootMsgId: MSG2, rootSeq: null, count: 2, lastTs: 1_700_000_001n, excerpt: '' },
    ]);
    expect(s.calls[1]).toEqual({ method: 'threads', args: [G] });
    const unheld = stub({ thread_timeline: () => encode([null, [reply]]) });
    expect(unheld.port.threadTimeline(G, MSG, 0n, 50).root).toBeNull();
    expect(caught(() => stub({ thread_timeline: () => encode([[reply]]) }).port.threadTimeline(G, MSG, 0n, 50)).code).toBe('E_CORE_DECODE');
    expect(caught(() => stub({ threads: () => encode([[MSG, 5, 1, 2]]) }).port.threads(G)).code).toBe('E_CORE_DECODE');
  });

  it('refuses a timeline row of the web-2a shape or with out-of-range values (L-CORE-34)', () => {
    const twelve: CborInput[] = [5, 7, 1_700_000_000, 0, '', USER, DEV, 0, 1, MSG, 0, 'hi'];
    const decodeOne = (row: CborInput[]) => caught(() => stub({ timeline: () => encode([row]) }).port.timeline(G, 0n, 100)).code;
    expect(decodeOne(twelve)).toBe('E_CORE_DECODE');
    const row = (at: number, v: CborInput): CborInput[] => { const r: CborInput[] = [...twelve, 0, null, [], 0, [], 0]; r[at] = v; return r; };
    expect(decodeOne(row(15, 2))).toBe('E_CORE_DECODE');
    expect(decodeOne(row(17, 2))).toBe('E_CORE_DECODE');
    expect(decodeOne(row(13, [MSG, null, null, '', 3]))).toBe('E_CORE_DECODE');
    expect(decodeOne(row(13, [MSG, null, null, '']))).toBe('E_CORE_DECODE');
    expect(decodeOne(row(14, [['👍', 1]]))).toBe('E_CORE_DECODE');
    expect(decodeOne(row(16, [[0, 1, 'x', null, null, 2, '']]))).toBe('E_CORE_DECODE');
    expect(stub({ timeline: () => encode([row(13, [MSG, null, null, '', 1])]) }).port.timeline(G, 0n, 100)[0]?.reply)
      .toEqual({ replyTo: MSG, targetSeq: null, targetUser: null, excerpt: '', state: 1, threadRoot: null });
    // web3.7 fix round 1 (M3): a sixth element names the quoted row's thread root; seven is malformed.
    expect(stub({ timeline: () => encode([row(13, [MSG, 5, USER, 'x', 0, MSG2])]) }).port.timeline(G, 0n, 100)[0]?.reply?.threadRoot)
      .toEqual(MSG2);
    expect(decodeOne(row(13, [MSG, 5, USER, 'x', 0, MSG2, null]))).toBe('E_CORE_DECODE');
  });

  it('returns request bodies and cursor bodies unchanged, and null for a CBOR null cursor', () => {
    const req = new Uint8Array([0x88, 1]);
    const s = stub({ signup_request: () => req, cursor_body: () => encode(null) });
    expect(s.port.signupRequest('INV', 'ada', 'Ada', null)).toBe(req);
    expect(s.calls[0]?.args).toEqual(['INV', 'ada', 'Ada', null]);
    expect(s.port.cursorBody(G)).toBeNull();
    const cursor = encode([4, 2]);
    expect(stub({ cursor_body: () => cursor }).port.cursorBody(G)).toBe(cursor);
  });
});

describe('wrapCore errors', () => {
  it('turns "<code>: <detail>" into a CoreError', () => {
    const e = caught(() => stub({ identity: () => { throw new Error('E_CORE_STATE: signup pending'); } }).port.identity());
    expect([e.code, e.detail, e.message]).toEqual(['E_CORE_STATE', 'signup pending', 'E_CORE_STATE: signup pending']);
    const bare = caught(() => stub({ groups: () => { throw new Error('E_CORE_NOT_FOUND'); } }).port.groups());
    expect([bare.code, bare.detail, bare.message]).toEqual(['E_CORE_NOT_FOUND', '', 'E_CORE_NOT_FOUND']);
  });

  it('maps a glue or panic error to E_CORE_WASM', () => {
    const e = caught(() => stub({ session_clear: () => { throw new TypeError('null pointer passed to rust'); } }).port.sessionClear());
    expect([e.code, e.detail]).toEqual(['E_CORE_WASM', 'null pointer passed to rust']);
  });

  it('maps a result of the wrong shape to E_CORE_DECODE', () => {
    expect(caught(() => stub({ identity: () => encode([0]) }).port.identity()).code).toBe('E_CORE_DECODE');
    expect(caught(() => stub({ identity: () => encode([7, null, null, null, '', 0]) }).port.identity()).code).toBe('E_CORE_DECODE');
  });

  it('keeps an existing CoreError', () => {
    const original = new CoreError('E_CORE_MLS', 'x');
    expect(CoreError.from(original)).toBe(original);
    expect(CoreError.from('boom')).toMatchObject({ code: 'E_CORE_WASM', detail: 'boom' });
  });

  it('reads both schema numbers of E_STORE_NEWER from a plain Error or a CoreError, and nothing else (C4x)', () => {
    expect(storeNewerOf(new Error('E_STORE_NEWER: found 5, supported 4'))).toEqual({ found: 5, supported: 4 });
    expect(storeNewerOf(CoreError.from(new Error('E_STORE_NEWER: found 7, supported 4')))).toEqual({ found: 7, supported: 4 });
    expect(storeNewerOf(new Error('E_STORE_NEWER'))).toBeNull();
    expect(storeNewerOf(new Error('E_CORE_STATE: found 5, supported 4'))).toBeNull();
    expect(storeNewerOf('E_STORE_NEWER: found 5, supported 4')).toBeNull();
  });
});

const DSK = new Uint8Array(32).fill(0xd5);

describe('wrapCore web-2a', () => {
  it('passes the group ID bytes to the deterministic heal snapshot export', () => {
    const snapshot = encode([new Uint8Array([1]), [], new Uint8Array([2])]);
    const s = stub({ group_heal_snapshot: (...args) => { expect(args).toEqual([G]); return snapshot; } });
    expect(s.port.groupHealSnapshot(G)).toBe(snapshot);
    expect(s.calls).toEqual([{ method: 'group_heal_snapshot', args: [G] }]);
  });
  it('reads phase 3 in identity', () => {
    expect(stub({ identity: () => encode([3, INST, null, DEV, '', 0]) }).port.identity())
      .toEqual({ phase: 3, instanceId: INST, userId: null, deviceId: DEV, username: '', listPublished: false });
  });

  it('passes a null community to group_create and group_join_external and into the expected groups', () => {
    const body = new Uint8Array([3]);
    const s = stub({ group_create: () => body, group_join_external: () => body, welcomes_apply: () => encode([]) });
    expect(s.port.groupCreate(G, null, CHAN, 1n, new Uint8Array(32))).toBe(body);
    expect(s.port.groupJoinExternal({ groupId: G, communityId: null, channelId: CHAN, policyVersion: 1n }, new Uint8Array([1]), new Uint8Array([2]))).toBe(body);
    s.port.welcomesApply(new Uint8Array([9]), [{ groupId: G, communityId: null, channelId: CHAN, policyVersion: 1n }]);
    expect(s.calls[0]?.args[1]).toBeNull();
    expect(s.calls[1]?.args[1]).toBeNull();
    expect(decode(s.calls[2]?.args[1] as Uint8Array)).toEqual([[G, null, CHAN, 1n]]);
  });

  it('reads one group row, and null for an unknown id', () => {
    expect(stub({ group_row: () => encode(null) }).port.groupRow(G)).toBeNull();
    const s = stub({ group_row: () => encode([G, 0, null, CHAN, 2, 4, 9, 0, 0]) });
    expect(s.port.groupRow(G)).toEqual({ groupId: G, kind: 0, communityId: null, targetId: CHAN, state: 2, epoch: 4n, nextSeq: 9n, proposalsPending: 0, pendingCommit: false });
    expect(s.calls).toEqual([{ method: 'group_row', args: [G] }]);
  });

  it('forwards markRead and reads activity', () => {
    const s = stub({ mark_read: () => undefined, activity: () => encode([[G, 3, 1, 12, 1_800_000_012, 9], [MSG, 0, 0, 0, 0, 4]]) });
    s.port.markRead(G, 12n, 1_800_000_100n);
    expect(s.calls[0]).toEqual({ method: 'mark_read', args: [G, 12n, 1_800_000_100n] });
    expect(s.port.activity()).toEqual([
      { groupId: G, unread: 3, mentions: 1, lastSeq: 12n, lastTs: 1_800_000_012n, lastReadSeq: 9n },
      { groupId: MSG, unread: 0, mentions: 0, lastSeq: 0n, lastTs: 0n, lastReadSeq: 4n },
    ]);
  });

  it('folds settings into a record and forwards puts and deletes', () => {
    const s = stub({
      settings: () => encode([['mute.channel.aa', '1'], ['notify.default', 'everything']]),
      setting_put: () => undefined, setting_delete: () => undefined,
    });
    expect(s.port.settings()).toEqual({ 'mute.channel.aa': '1', 'notify.default': 'everything' });
    s.port.settingPut('notify.default', 'nothing');
    s.port.settingDelete('mute.channel.aa');
    expect(s.calls.slice(1)).toEqual([
      { method: 'setting_put', args: ['notify.default', 'nothing'] },
      { method: 'setting_delete', args: ['mute.channel.aa'] },
    ]);
  });

  it('reads the three sealed objects and forwards stateSealedUploaded', () => {
    const root = new Uint8Array(103).fill(1);
    const state = new Uint8Array(40).fill(2);
    expect(stub({ sealed_objects: () => encode([root, state, 1]) }).port.sealedObjects()).toEqual({ root, state, stateUploaded: true });
    expect(stub({ sealed_objects: () => encode([null, null, 0]) }).port.sealedObjects()).toEqual({ root: null, state: null, stateUploaded: false });
    const s = stub({ state_sealed_uploaded: () => undefined, state_sealed_current: () => true });
    s.port.stateSealedUploaded();
    expect(s.port.stateSealedCurrent()).toBe(true);
    expect(s.calls).toEqual([{ method: 'state_sealed_uploaded', args: [] }, { method: 'state_sealed_current', args: [] }]);
  });

  it('walks the enrolment exports in the facade argument order', () => {
    const body = new Uint8Array([0x85, 1]);
    const s = stub({
      enrol_begin: () => encode([DEV, DSK]), enrol_session_sign: () => body, enrol_registered: () => undefined,
      enrol_complete: () => encode([new Uint8Array([4]), new Uint8Array([5]), null]), enrol_reset: () => undefined,
    });
    expect(s.port.enrolBegin(INST)).toEqual({ deviceId: DEV, dskPub: DSK });
    const nonce = new Uint8Array(32).fill(9);
    const login = new TextEncoder().encode('asrt-1');
    expect(s.port.enrolSessionSign(nonce, login)).toBe(body);
    s.port.enrolRegistered(USER);
    const input = { recoveryKey: 'abcd-efgh', rootSealed: new Uint8Array([1]), stateSealed: new Uint8Array([2]), listBody: new Uint8Array([3]), username: 'ada', now: 1_800_000_000n };
    expect(s.port.enrolComplete(input)).toEqual({ deviceListBody: new Uint8Array([4]), stateSealed: new Uint8Array([5]), interrupted: null });
    s.port.enrolReset();
    expect(s.calls).toEqual([
      { method: 'enrol_begin', args: [INST] },
      { method: 'enrol_session_sign', args: [nonce, login] },
      { method: 'enrol_registered', args: [USER] },
      { method: 'enrol_complete', args: ['abcd-efgh', input.rootSealed, input.stateSealed, input.listBody, 'ada', 1_800_000_000n] },
      { method: 'enrol_reset', args: [] },
    ]);
  });

  it('forwards the drop of an unpublished candidate (BACKUPS-RECOVERY-02)', () => {
    const s = stub({ device_list_drop: () => undefined });
    s.port.deviceListDrop();
    expect(s.calls).toEqual([{ method: 'device_list_drop', args: [] }]);
  });

  it('forwards the recovery key form check and maps its refusal (REGISTRATION-DEVICES-02)', () => {
    const s = stub({ recovery_key_check: () => undefined });
    s.port.recoveryKeyCheck('abcd-efgh');
    expect(s.calls).toEqual([{ method: 'recovery_key_check', args: ['abcd-efgh'] }]);
    const refused = caught(() => stub({ recovery_key_check: () => { throw new Error('E_RECOVERY_KEY'); } }).port.recoveryKeyCheck('x'));
    expect([refused.code, refused.detail]).toEqual(['E_RECOVERY_KEY', '']);
  });

  it('concatenates the device ids of a revocation and refuses a malformed list before the call', () => {
    const s = stub({ device_list_revoke: () => encode([new Uint8Array([6]), new Uint8Array([7]), new Uint8Array([5])]) });
    const a = new Uint8Array(16).fill(0x0a);
    const b = new Uint8Array(16).fill(0x0b);
    const input = { recoveryKey: 'K', rootSealed: new Uint8Array([1]), stateSealed: new Uint8Array([2]), listBody: new Uint8Array([3]), deviceIds: [a, b], now: 9n };
    expect(s.port.deviceListRevoke(input)).toEqual({ deviceListBody: new Uint8Array([6]), stateSealed: new Uint8Array([7]), interrupted: new Uint8Array([5]) });
    const ids = new Uint8Array(32);
    ids.set(a, 0);
    ids.set(b, 16);
    expect(s.calls[0]).toEqual({ method: 'device_list_revoke', args: ['K', input.rootSealed, input.stateSealed, input.listBody, ids, 9n] });
    expect(caught(() => s.port.deviceListRevoke({ ...input, deviceIds: [a, new Uint8Array(15)] })).code).toBe('E_CORE_INPUT');
    expect(caught(() => s.port.deviceListRevoke({ ...input, deviceIds: [] })).code).toBe('E_CORE_INPUT');
    expect(caught(() => s.port.deviceListRevoke({ ...input, deviceIds: Array.from({ length: 65 }, () => a) })).code).toBe('E_CORE_INPUT');
    expect(s.calls).toHaveLength(1);
  });

  it('reads the own device list and the result of an update', () => {
    const OTHER = new Uint8Array(16).fill(0x0c);
    const s = stub({
      own_device_list: () => encode([3, 0, [[DEV, DSK, 1, 1_800_000_000, null], [OTHER, DSK, 0, 1_799_000_000, 1_800_000_500]]]),
      own_device_list_update: () => encode([3, 1]),
    });
    expect(s.port.ownDeviceList()).toEqual({ version: 3n, published: false, entries: [
      { deviceId: DEV, dskPub: DSK, tier: 1, addedAt: 1_800_000_000n, revokedAt: null },
      { deviceId: OTHER, dskPub: DSK, tier: 0, addedAt: 1_799_000_000n, revokedAt: 1_800_000_500n },
    ] });
    const history = encode([]);
    expect(s.port.ownDeviceListUpdate(history)).toEqual({ version: 3n, listed: true });
    expect(s.calls[1]).toEqual({ method: 'own_device_list_update', args: [history] });
  });

  it('maps a web-2a result of the wrong shape to E_CORE_DECODE', () => {
    expect(caught(() => stub({ activity: () => encode([[G, 1, 0, 1, 1]]) }).port.activity()).code).toBe('E_CORE_DECODE');
    expect(caught(() => stub({ sealed_objects: () => encode([null, null, 2]) }).port.sealedObjects()).code).toBe('E_CORE_DECODE');
    expect(caught(() => stub({ own_device_list_update: () => encode([1]) }).port.ownDeviceListUpdate(new Uint8Array([0x80]))).code).toBe('E_CORE_DECODE');
    expect(caught(() => stub({ identity: () => encode([4, null, null, null, '', 0]) }).port.identity()).code).toBe('E_CORE_DECODE');
  });
});

describe('wrapCore web-2b', () => {
  const BLOB = new Uint8Array(32).fill(0xb1);
  const KEY = new Uint8Array(32).fill(0xb2);
  const NONCE = new Uint8Array(12).fill(0xb3);
  const THUMB = new Uint8Array([0x52, 0x49, 0x46, 0x46]);
  const ROLE_A = new Uint8Array(16).fill(0x0a);
  const ROLE_B = new Uint8Array(16).fill(0x0b);
  const descriptor = { blobId: BLOB, key: KEY, nonce: NONCE, size: 1234, mime: 'image/png', w: 640, h: 480, thumb: THUMB, name: 'map.png' };

  it('encodes a send request as the L-CORE-36 array, attachment elements in order', () => {
    const s = stub({ send_prepare: () => encode([MSG]) });
    expect(s.port.sendPrepare(G, { type: 0, replyTo: MSG, body: 'look', attachments: [descriptor], threadId: null }, 7n)).toEqual(MSG);
    expect(s.calls[0]?.method).toBe('send_prepare');
    expect(s.calls[0]?.args[0]).toBe(G);
    expect(s.calls[0]?.args[2]).toBe(7n);
    expect(decode(s.calls[0]?.args[1] as Uint8Array)).toEqual([0n, MSG, 'look', [[BLOB, KEY, NONCE, 1234n, 'image/png', 640n, 480n, THUMB, 'map.png']], null]);
    s.port.sendPrepare(G, { type: 3, replyTo: MSG, body: '👍', attachments: [], threadId: null }, 8n);
    expect(decode(s.calls[1]?.args[1] as Uint8Array)).toEqual([3n, MSG, '👍', [], null]);
    s.port.sendPrepare(G, { type: 0, replyTo: null, body: 'x', attachments: [{ ...descriptor, w: null, h: null, thumb: null, name: '' }], threadId: null }, 9n);
    expect(decode(s.calls[2]?.args[1] as Uint8Array)).toEqual([0n, null, 'x', [[BLOB, KEY, NONCE, 1234n, 'image/png', null, null, null, '']], null]);
    // web3.7: the fifth element is the thread root.
    s.port.sendPrepare(G, { type: 0, replyTo: null, body: 'in a thread', attachments: [], threadId: MSG2 }, 10n);
    expect(decode(s.calls[3]?.args[1] as Uint8Array)).toEqual([0n, null, 'in a thread', [], MSG2]);
  });

  it('reads the eight-element outbox row', () => {
    const { port } = stub({
      outbox: () => encode([
        [MSG, 0, '', 1_800_000_000, '', 0, null, [[BLOB, 1234, 'image/png', 'map.png']]],
        [G, 2, 'E_CORE_STATE', 1_800_000_001, '👍', 3, MSG, []],
      ]),
    });
    expect(port.outbox(G)).toEqual([
      { msgId: MSG, state: 0, error: '', created: 1_800_000_000n, body: '', type: 0, replyTo: null,
        attachments: [{ blobId: BLOB, size: 1234, mime: 'image/png', name: 'map.png' }] },
      { msgId: G, state: 2, error: 'E_CORE_STATE', created: 1_800_000_001n, body: '👍', type: 3, replyTo: MSG, attachments: [] },
    ]);
  });

  it('reads pins, an attachment descriptor and purges', () => {
    const s = stub({
      pins: () => encode([[5, MSG, 9, USER, null, 'the excerpt', 1700000005], [3, G, 7, DEV, USER, '', 1700000003, MSG2]]),
      attachment_get: () => encode([BLOB, KEY, NONCE, 1234, 'image/png', 640, 480, THUMB, 'map.png']),
      purges: () => encode([[G, 12, CHAN, [BLOB, KEY]]]),
    });
    expect(s.port.pins(G)).toEqual([
      { targetSeq: 5n, msgId: MSG, pinnedSeq: 9n, byUser: USER, author: null, excerpt: 'the excerpt', targetTs: 1700000005n, threadRoot: null },
      { targetSeq: 3n, msgId: G, pinnedSeq: 7n, byUser: DEV, author: USER, excerpt: '', targetTs: 1700000003n, threadRoot: MSG2 },
    ]);
    expect(s.port.attachmentGet(G, 12n, 1)).toEqual(descriptor);
    expect(s.calls[1]).toEqual({ method: 'attachment_get', args: [G, 12n, 1] });
    expect(s.port.purges()).toEqual([{ groupId: G, seq: 12n, channelId: CHAN, blobIds: [BLOB, KEY] }]);
  });

  it('forwards purgeDone and concatenates own role ids, refusing a malformed set before the call', () => {
    const s = stub({ purge_done: () => undefined, own_roles_set: () => undefined });
    s.port.purgeDone(G, 12n);
    s.port.ownRolesSet(COMM, [ROLE_A, ROLE_B]);
    s.port.ownRolesSet(COMM, []);
    const both = new Uint8Array(32);
    both.set(ROLE_A, 0);
    both.set(ROLE_B, 16);
    expect(s.calls).toEqual([
      { method: 'purge_done', args: [G, 12n] },
      { method: 'own_roles_set', args: [COMM, both] },
      { method: 'own_roles_set', args: [COMM, new Uint8Array(0)] },
    ]);
    expect(caught(() => s.port.ownRolesSet(COMM, [new Uint8Array(15)])).code).toBe('E_CORE_INPUT');
    expect(caught(() => s.port.ownRolesSet(COMM, Array.from({ length: 65 }, () => ROLE_A))).code).toBe('E_CORE_INPUT');
    expect(caught(() => s.port.ownRolesSet(COMM, [new Uint8Array(15)])).detail).toBe('role_ids must be 0..=64 ids of 16 bytes');
    expect(s.calls).toHaveLength(3);
  });

  it('concatenates the authority id lists, refusing a malformed list before the call (web3.5 R1)', () => {
    const s = stub({ authority_set: () => undefined });
    const A = new Uint8Array(16).fill(0xa1);
    const B = new Uint8Array(16).fill(0xb2);
    s.port.authoritySet(G, [A, B], [B], []);
    s.port.authoritySet(G, [], [], [A]);
    const both = new Uint8Array(32);
    both.set(A, 0);
    both.set(B, 16);
    expect(s.calls).toEqual([
      { method: 'authority_set', args: [G, both, B, new Uint8Array(0)] },
      { method: 'authority_set', args: [G, new Uint8Array(0), new Uint8Array(0), A] },
    ]);
    const detail = 'pinners, broadcasters and mentionable must be 0..=4096 ids of 16 bytes';
    expect(caught(() => s.port.authoritySet(G, [new Uint8Array(15)], [], [])).code).toBe('E_CORE_INPUT');
    expect(caught(() => s.port.authoritySet(G, [], [new Uint8Array(15)], [])).detail).toBe(detail);
    expect(caught(() => s.port.authoritySet(G, [], [], [new Uint8Array(15)])).detail).toBe(detail);
    expect(caught(() => s.port.authoritySet(G, [], [], Array.from({ length: 4097 }, () => A))).code).toBe('E_CORE_INPUT');
    expect(caught(() => s.port.authoritySet(G, Array.from({ length: 4097 }, () => A), [], [])).code).toBe('E_CORE_INPUT');
    expect(caught(() => s.port.authoritySet(G, [], Array.from({ length: 4097 }, () => A), [])).code).toBe('E_CORE_INPUT');
    expect(s.calls).toHaveLength(2);
    s.port.authoritySet(G, Array.from({ length: 4096 }, () => A), [], []);
    expect(s.calls).toHaveLength(3);
    expect((s.calls[2].args[1] as Uint8Array).length).toBe(4096 * 16);
  });

  it('maps a web-2b result of the wrong shape to E_CORE_DECODE', () => {
    expect(caught(() => stub({ outbox: () => encode([[MSG, 0, '', 1, 'hi']]) }).port.outbox(G)).code).toBe('E_CORE_DECODE');
    expect(caught(() => stub({ outbox: () => encode([[MSG, 0, '', 1, 'hi', 7, null, []]]) }).port.outbox(G)).code).toBe('E_CORE_DECODE');
    expect(caught(() => stub({ pins: () => encode([[5, MSG, 9, USER, null, 'x']]) }).port.pins(G)).code).toBe('E_CORE_DECODE');
    expect(caught(() => stub({ pins: () => encode([[5, MSG, 9, USER, null, 'x', 1, null, null]]) }).port.pins(G)).code).toBe('E_CORE_DECODE');
    expect(caught(() => stub({ attachment_get: () => encode([BLOB, KEY, new Uint8Array(11), 1, 'x', null, null, null, '']) })
      .port.attachmentGet(G, 1n, 0)).code).toBe('E_CORE_DECODE');
    expect(caught(() => stub({ purges: () => encode([[G, 1, CHAN, [new Uint8Array(31)]]]) }).port.purges()).code).toBe('E_CORE_DECODE');
  });
});
