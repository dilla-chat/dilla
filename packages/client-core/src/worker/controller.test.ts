import { afterEach, describe, expect, it, vi } from 'vitest';
import { decode, encode } from '../cbor';
import { encodeBotRequestBytes, encodeBotResponse, encodeRemovalProof, validateBotContext } from '../bot/protocol';
import { Enrol } from '../account/enrol';
import { FakeCore } from '../testing/fake-core';
import { FakeServer, NOW_S as FAKE_NOW_S, sessionFor } from '../testing/fake-server';
import { sealBlob, sealThumb, BROWSER_ATTACHMENT_CAP } from '../attachments/crypto';
import { CoreError, type ThreadRow, type ActivityRow, type ApplyResult, type CorePort, type HumanInteractionEvent, type IdentityInfo, type OutboxRow, type OwnDeviceList, type PurgeRow, type SessionRecord, type TimelineRow, type TrustBlock, type TrustRecord } from '../core-port';
import { CLIENT_CLOSE, GATEWAY, type GatewayEvent } from '../gateway/gateway';
import { Op } from '../gateway/frames';
import { toHex } from '../hex';
import { DillaHttpError } from '../http/errors';
import type { AFKSnapshot, DeviceRow } from '../http/routes';
import type { AccountState, ChannelSummary, CommunitySummary, DeviceSummary, DiagnosticsState, DmSummary, MemberSummary, PinnedItem, ThreadState, ThreadSummary, TimelineState, TrayItem, TrustSummary } from '../state/types';
import { SyncError } from '../sync/errors';
import { HELD_LIVE_MAX, HELD_READ_HOLD_MS } from './controller';
import { LEADER } from './leader';
import type { FromWorker } from './protocol';
import { INSTANCE, INSTANCE_ID, INSTANCE_HEX, LOCK, cbor, fakeFetch, stubCore, harness, opened, USER, DEVICE, COMMUNITY, CHANNEL, GROUP, STRANGER, ROLE_EVERYONE, NEW_COMMUNITY, READY_INFO, refusal, SUBMIT, textGroup, applied, strangerRow, DM_CHANNEL, DM_GROUP, OTHER_DEVICE, ORPHAN_GROUP, ORPHAN_CHANNEL, RECOVERY_KEY, PASSWORD, ROOT, STATE_OBJECT, LOCAL_STATE, LIST_RAW, FETCHED, NOW_S, world, World, toReady, type SliceMessage } from './world';

function interactionLease(w: World, id: number): string {
  const result = w.ret(id);
  if (result?.t !== 'ret' || !result.ok || typeof result.value !== 'object' || result.value === null
    || !('through' in result.value)) throw new Error('expected a successful response with an ACK lease');
  const lease = result.value.through;
  if (typeof lease !== 'string' || !/^[0-9a-f]{32}$/u.test(lease)) throw new Error('expected a 32-byte hexadecimal ACK lease');
  return lease;
}

describe('Controller', () => {
  it('resumes accepted removal evidence after restart without posting another removal', async () => {
    const w = world();
    const proofRequestId = new Uint8Array(16).fill(0x71);
    const evidenceRequestId = new Uint8Array(16).fill(0x72);
    const signedBody = encode([new Uint8Array([1]), new Uint8Array(64).fill(7)]);
    w.state.groups.push(textGroup(2), { ...textGroup(2), groupId: new Uint8Array(16).fill(0x52), kind: 3,
      targetId: new Uint8Array(16).fill(0x53) });
    w.state.manualEvidence.push({ proofRequestId, evidenceRequestId, actorUserId: USER, actorDeviceId: DEVICE,
      communityId: COMMUNITY, groupId: GROUP, channelId: CHANNEL, messageId: new Uint8Array(16).fill(0x73),
      targetSequence: 3n, reason: 'persisted reason', excerpt: 'persisted excerpt', signedBody, state: 2,
      createdAt: 1_700_000_000n, expires: 1_700_000_030n, evidenceExpires: 0n });
    w.core.humanInteractionSendPrepare.mockImplementation((requestId) => {
      w.state.outbox.push({ msgId: requestId, state: 0, error: '', created: NOW_S, body: 'evidence request', type: 0, replyTo: null, attachments: [] });
      return requestId;
    });
    await toReady(w);
    await vi.waitFor(() => expect(w.core.humanInteractionSendPrepare).toHaveBeenCalledTimes(1));
    expect(w.routes.postModerationRemoval).not.toHaveBeenCalled();
    const request = decode(w.core.humanInteractionSendPrepare.mock.calls[0][3]) as unknown[];
    expect(request[1]).toEqual(evidenceRequestId);
    expect(request[7]).toEqual([proofRequestId, 'persisted reason', 'persisted excerpt']);
    expect(w.core.completeManualRemovalEvidence).toHaveBeenCalledWith(proofRequestId, 1_700_000_000n);
  });

  it('rotates an expired op61 envelope after restart while preserving proof and exact evidence', async () => {
    const w = world({ now: () => 1_700_000_040_000 });
    const proofRequestId = new Uint8Array(16).fill(0x77);
    const oldEvidenceRequestId = new Uint8Array(16).fill(0x78);
    const signedBody = encode([new Uint8Array([1]), new Uint8Array(64).fill(7)]);
    w.state.groups.push(textGroup(2), { ...textGroup(2), groupId: new Uint8Array(16).fill(0x52), kind: 3,
      targetId: new Uint8Array(16).fill(0x53) });
    w.state.manualEvidence.push({ proofRequestId, evidenceRequestId: oldEvidenceRequestId, actorUserId: USER, actorDeviceId: DEVICE,
      communityId: COMMUNITY, groupId: GROUP, channelId: CHANNEL, messageId: new Uint8Array(16).fill(0x79),
      targetSequence: 3n, reason: 'persisted reason', excerpt: 'persisted excerpt', signedBody, state: 2,
      createdAt: 1_700_000_000n, expires: 1_700_000_030n, evidenceExpires: 1_700_000_030n });
    w.core.humanInteractionSendPrepare.mockImplementation((requestId) => {
      w.state.outbox.push({ msgId: requestId, state: 0, error: '', created: NOW_S, body: 'evidence request', type: 0, replyTo: null, attachments: [] });
      return requestId;
    });
    await toReady(w);
    await vi.waitFor(() => expect(w.core.humanInteractionSendPrepare).toHaveBeenCalledTimes(1));
    expect(w.routes.postModerationRemoval).not.toHaveBeenCalled();
    const [proof, candidate, now] = w.core.ensureManualRemovalEvidenceRequest.mock.calls[0];
    expect(proof).toEqual(proofRequestId);
    expect(now).toBe(1_700_000_040n);
    expect(candidate).not.toEqual(oldEvidenceRequestId);
    const request = decode(w.core.humanInteractionSendPrepare.mock.calls[0][3]) as unknown[];
    expect(request[1]).not.toEqual(oldEvidenceRequestId);
    expect(request[6]).toBe(1_700_000_070n);
    expect(request[7]).toEqual([proofRequestId, 'persisted reason', 'persisted excerpt']);
  });

  it('uses the authenticated removal feed to resolve an interrupted POST before op61 and avoids replaying the removal', async () => {
    const w = world();
    const proofRequestId = new Uint8Array(16).fill(0x74);
    const evidenceRequestId = new Uint8Array(16).fill(0x75);
    const signedBody = encode([new Uint8Array([1]), new Uint8Array(64).fill(7)]);
    const saved = { proofRequestId, evidenceRequestId, actorUserId: USER, actorDeviceId: DEVICE, communityId: COMMUNITY,
      groupId: GROUP, channelId: CHANNEL, messageId: new Uint8Array(16).fill(0x76), targetSequence: 3n,
      reason: 'recovered reason', excerpt: 'recovered excerpt', signedBody, state: 1 as 0 | 1 | 2,
      createdAt: 1_700_000_000n, expires: 1_700_000_030n, evidenceExpires: 0n };
    w.state.groups.push(textGroup(2), { ...textGroup(2), groupId: new Uint8Array(16).fill(0x52), kind: 3,
      targetId: new Uint8Array(16).fill(0x53) });
    w.state.manualEvidence.push(saved);
    const proof = encodeRemovalProof({ requestId: proofRequestId, actorUser: USER, actorDevice: DEVICE,
      communityId: COMMUNITY, groupId: GROUP, targetSeq: 3n, expires: 1_700_000_030n });
    const getModerationRemovals = vi.fn(() => Promise.resolve({
      rows: [{ revision: 1n, proof, signature: new Uint8Array(64).fill(7), acceptedAt: 1_700_000_001n }], nextRevision: 1n,
    }));
    Object.assign(w.routes, { getModerationRemovals });
    const signedList = encode([1, new Uint8Array([9]), new Uint8Array(64), new Uint8Array(32)]);
    w.routes.getDeviceList.mockResolvedValue({ version: 1n, blob: new Uint8Array([9]), raw: signedList });
    w.routes.getDeviceListHistory.mockResolvedValue({ raw: encode([[1, new Uint8Array([9]), new Uint8Array(64), new Uint8Array(32)]]), count: 1, lastVersion: 1n });
    w.core.ingestRemoval.mockImplementation(() => { saved.state = 2; return 1n; });
    w.core.humanInteractionSendPrepare.mockImplementation((requestId) => {
      w.state.outbox.push({ msgId: requestId, state: 0, error: '', created: NOW_S, body: 'evidence request', type: 0, replyTo: null, attachments: [] });
      return requestId;
    });
    await toReady(w);
    await vi.waitFor(() => expect(w.core.humanInteractionSendPrepare).toHaveBeenCalledTimes(1));
    expect(getModerationRemovals).toHaveBeenCalledWith(GROUP, 0n, 250);
    expect(w.routes.postModerationRemoval).not.toHaveBeenCalled();
    const request = decode(w.core.humanInteractionSendPrepare.mock.calls[0][3]) as unknown[];
    expect(request[1]).toEqual(evidenceRequestId);
    expect(request[7]).toEqual([proofRequestId, 'recovered reason', 'recovered excerpt']);
  });

  it('returns unknown after a transport failure and retries the exact proof for the same target', async () => {
    const w = world();
    w.routes.listRoles.mockResolvedValue([{ id: ROLE_EVERYONE, name: '@everyone', color: 0, position: 0, allow: 1, deny: 0,
      hoist: false, mentionable: false }]);
    await toReady(w);
    w.call(10_200, { m: 'selectCommunity', communityId: toHex(COMMUNITY) });
    await vi.waitFor(() => expect(w.ret(10_200)).toMatchObject({ ok: true, value: null }));
    w.state.groups.push(textGroup(2), { ...textGroup(2), groupId: new Uint8Array(16).fill(0x52), kind: 3, targetId: new Uint8Array(16).fill(0x53) });
    w.state.rows.push(strangerRow(3));
    w.routes.postModerationRemoval.mockRejectedValue(new Error('connection closed after request'));
    const command = { m: 'moderateRemoveMessage', channelId: toHex(CHANNEL), msgId: toHex(w.state.rows[0].msgId!),
      seq: '3', reason: 'keep exact reason', excerpt: 'keep exact excerpt' } as const;
    w.call(10_201, command);
    await vi.waitFor(() => expect(w.ret(10_201)).toMatchObject({ ok: true, value: { removal: 'unknown' } }));
    const first = w.ret(10_201) as { ok: true; value: { proofRequestId: string } };
    expect(first.value.proofRequestId).toMatch(/^[0-9a-f]{32}$/u);
    expect(w.state.manualEvidence).toHaveLength(1);
    const exactBody = w.state.manualEvidence[0].signedBody;
    w.call(10_202, command);
    await vi.waitFor(() => expect(w.ret(10_202)).toMatchObject({ ok: true, value: { removal: 'unknown' } }));
    expect((w.ret(10_202) as { ok: true; value: { proofRequestId: string } }).value.proofRequestId).toBe(first.value.proofRequestId);
    expect(w.core.prepareManualRemovalEvidence).toHaveBeenCalledTimes(1);
    expect(w.routes.postModerationRemoval).toHaveBeenCalledTimes(2);
    expect(w.routes.postModerationRemoval.mock.calls[0][1]).toBe(exactBody);
    expect(w.routes.postModerationRemoval.mock.calls[1][1]).toBe(exactBody);
  });

  it('checks or initializes the instance signer pin on restart before entering ready', async () => {
    const w = world();
    await toReady(w);
    expect(w.core.instanceSignerKeyPin).toHaveBeenCalledTimes(1);
    expect(w.core.instanceSignerKeyPin).toHaveBeenCalledWith(INSTANCE.instanceId, INSTANCE.externalSenderPub);
  });

  it('pins authenticated discovery for an upgraded browser with no saved signer pin', async () => {
    const w = world();
    let pinned = false;
    w.core.instanceSignerKeyPin.mockImplementation(() => {
      if (pinned) return;
      pinned = true;
    });
    await toReady(w);
    expect(w.account()).toMatchObject({ phase: 'ready' });
    expect(w.core.instanceSignerKeyPin).toHaveBeenCalledWith(INSTANCE.instanceId, INSTANCE.externalSenderPub);
  });

  it('refuses ready when authenticated discovery reports a rotated instance signer', async () => {
    const w = world();
    w.core.instanceSignerKeyPin.mockImplementation(() => {
      throw new CoreError('E_CORE_STATE', 'instance external-sender key changed');
    });
    w.call(1, { m: 'start' });
    await vi.waitFor(() => expect(w.account()).toMatchObject({ phase: 'error', error: { code: 'E_CORE_STATE' } }));
    expect(w.core.instanceSignerKeyPin).toHaveBeenCalledWith(INSTANCE.instanceId, INSTANCE.externalSenderPub);
    expect(w.gateway.starts).toBe(0);
  });

  it('does not enter ready if the legacy first pin refuses authenticated discovery', async () => {
    const w = world();
    w.core.instanceSignerKeyPin.mockImplementation(() => {
      throw new CoreError('E_CORE_STATE', 'authenticated instance discovery does not match identity');
    });
    w.call(1, { m: 'start' });
    await vi.waitFor(() => expect(w.account()).toMatchObject({ phase: 'error', error: { code: 'E_CORE_STATE' } }));
    expect(w.gateway.starts).toBe(0);
  });

  it('stops reconnect when authenticated discovery no longer matches the pin', async () => {
    const w = world();
    await toReady(w);
    const starts = w.gateway.starts;
    w.core.instanceSignerKeyPin.mockImplementation(() => {
      throw new CoreError('E_CORE_STATE', 'instance external-sender key changed');
    });
    w.gateway.emit({ type: 'revoked' });
    await vi.waitFor(() => expect(w.session.establish).toHaveBeenCalled());
    await vi.waitFor(() => expect(w.core.instanceSignerKeyPin).toHaveBeenCalledTimes(2));
    await vi.waitFor(() => expect(w.account()).toMatchObject({ phase: 'error', error: { code: 'E_CORE_STATE' } }));
    expect(w.gateway.starts).toBe(starts);
    expect(w.gateway.stops).toBeGreaterThan(0);
  });

  it('retries a discovery outage but rechecks the pin before restarting the socket', async () => {
    const w = world();
    await toReady(w);
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    try {
      const starts = w.gateway.starts;
      const reads = w.routes.getInstance.mock.calls.length;
      w.routes.getInstance.mockRejectedValueOnce(new Error('offline'));
      w.gateway.emit({ type: 'revoked' });
      await vi.waitFor(() => expect(w.routes.getInstance).toHaveBeenCalledTimes(reads + 1));
      expect(w.gateway.starts).toBe(starts);

      w.routes.getInstance.mockResolvedValueOnce(INSTANCE);
      await vi.advanceTimersByTimeAsync(GATEWAY.reconnectCapMs);
      await vi.waitFor(() => expect(w.routes.getInstance).toHaveBeenCalledTimes(reads + 2));
      expect(w.core.instanceSignerKeyPin).toHaveBeenCalledTimes(2);
      expect(w.gateway.starts).toBe(starts + 1);
    } finally {
      vi.useRealTimers();
    }
  });

  it('fails closed on malformed authenticated instance signer data', async () => {
    const w = world();
    await toReady(w);
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    try {
      const starts = w.gateway.starts;
      w.core.instanceSignerKeyPin.mockImplementation(() => { throw new CoreError('E_CORE_INPUT', 'weak key'); });
      w.gateway.emit({ type: 'revoked' });
      await vi.waitFor(() => expect(w.account()).toMatchObject({ phase: 'error', error: { code: 'E_CORE_STATE' } }));
      await vi.advanceTimersByTimeAsync(GATEWAY.reconnectCapMs * 2);
      expect(w.gateway.starts).toBe(starts);
      expect(w.gateway.stops).toBeGreaterThan(0);
    } finally {
      vi.useRealTimers();
    }
  });

  it('checks the signer pin before every gateway ticket, including automatic reconnects', async () => {
    const w = world();
    await toReady(w);
    const reads = w.routes.getInstance.mock.calls.length;
    const tickets = w.routes.postTicket.mock.calls.length;
    w.core.instanceSignerKeyPin.mockImplementation(() => {
      throw new CoreError('E_CORE_STATE', 'instance external-sender key changed');
    });

    await expect(w.gatewayDeps.value!.mintTicket()).rejects.toMatchObject({ code: 'E_CORE_STATE' });
    expect(w.routes.getInstance).toHaveBeenCalledTimes(reads + 1);
    expect(w.routes.postTicket).toHaveBeenCalledTimes(tickets);
    expect(w.gateway.stops).toBeGreaterThan(0);
    expect(w.account()).toMatchObject({ phase: 'error', error: { code: 'E_CORE_STATE' } });
  });

  it('does not mint a reconnect ticket while signer discovery is unavailable', async () => {
    const w = world();
    await toReady(w);
    const tickets = w.routes.postTicket.mock.calls.length;
    w.routes.getInstance.mockRejectedValueOnce(new Error('offline'));

    await expect(w.gatewayDeps.value!.mintTicket()).rejects.toThrow('offline');
    expect(w.routes.postTicket).toHaveBeenCalledTimes(tickets);
  });

  it('sends a typed moderation request through the authenticated kind-3 group and returns only its id and status', async () => {
    const w = world();
    await toReady(w);
    const groupId = new Uint8Array(16).fill(0x52);
    const requestId = new Uint8Array(16).fill(0x54);
    w.state.groups.push({ ...textGroup(2), groupId, kind: 3, targetId: new Uint8Array(16).fill(0x53) });
    w.state.outbox.push({ msgId: requestId, state: 0, error: '', created: 1n, body: 'secret request', type: 0, replyTo: null, attachments: [] });
    w.call(10_199, { m: 'listModerationCases', communityId: toHex(COMMUNITY), requestId: toHex(requestId), offset: 0 });
    await vi.waitFor(() => expect(w.ret(10_199)).toMatchObject({ ok: true, value: { requestId: toHex(requestId), status: 'accepted' } }));
    expect(w.sync.runOnGroup).toHaveBeenCalledWith(groupId, expect.any(Function));
    expect(w.core.humanInteractionSendPrepare).toHaveBeenCalledWith(requestId, COMMUNITY, groupId, expect.any(Uint8Array), NOW_S);
    expect(w.core.sendEncrypt).toHaveBeenCalledWith(requestId);
    expect(w.routes.postMessage).toHaveBeenCalledWith(groupId, expect.any(Uint8Array));
    expect(w.core.sendConfirm).toHaveBeenCalledWith(requestId, expect.any(Uint8Array));
    const value = (w.ret(10_199) as { ok: true; value: object }).value;
    expect(Object.keys(value).sort()).toEqual(['requestId', 'status']);
    w.call(10_194, { m: 'listModerationCases', communityId: toHex(COMMUNITY), requestId: toHex(requestId), offset: 0 });
    await vi.waitFor(() => expect(w.ret(10_194)).toMatchObject({ ok: true, value: { requestId: toHex(requestId), status: 'accepted' } }));
    expect(w.routes.postMessage).toHaveBeenCalledTimes(1);
    w.call(10_198, { m: 'listModerationCases', communityId: toHex(COMMUNITY), requestId: '54'.repeat(15), offset: 0 });
    await vi.waitFor(() => expect(w.ret(10_198)).toMatchObject({ ok: false, error: { code: 'E_BAD_INPUT' } }));
  });

  it('sends a revision-bound human interaction mutation and reports only the ciphertext upload receipt', async () => {
    const w = world();
    w.routes.listChannels.mockResolvedValue([{ id: CHANNEL, kind: 1, mode: 0, visibility: 0, parentId: null,
      name: 'voice', topic: '', position: 0, slowmode: 0, seq: 1n, textGroupId: GROUP }]);
    await toReady(w);
    const groupId = new Uint8Array(16).fill(0x52);
    const requestId = new Uint8Array(16).fill(0x54);
    w.state.groups.push({ ...textGroup(2), groupId, kind: 3, targetId: new Uint8Array(16).fill(0x53) });
    w.state.outbox.push({ msgId: requestId, state: 0, error: '', created: 1n, body: 'secret request', type: 0, replyTo: null, attachments: [] });
    w.call(10_193, { m: 'humanInteractionRequest', communityId: toHex(COMMUNITY), channelId: toHex(CHANNEL),
      requestId: toHex(requestId), operation: 13, args: [], revision: '7' });
    await vi.waitFor(() => expect(w.ret(10_193)).toMatchObject({ ok: true,
      value: { requestId: toHex(requestId), upload: 'accepted' } }));
    const prepared = w.core.humanInteractionSendPrepare.mock.calls[0];
    const wire = decode(prepared[3]);
    expect(wire).toEqual([1n, requestId, COMMUNITY, CHANNEL, 13n, 7n, NOW_S + 30n, []]);
    expect(w.routes.postMessage).toHaveBeenCalledWith(groupId, expect.any(Uint8Array));
    expect(w.core.sendConfirm).toHaveBeenCalledWith(requestId, expect.any(Uint8Array));
  });


  it.each([[], ['ab'.repeat(16)], ['']])('preserves optional skip guard %j through the worker CBOR boundary', async (...args) => {
    const w = world();
    w.routes.listChannels.mockResolvedValue([{ id: CHANNEL, kind: 1, mode: 0, visibility: 0, parentId: null,
      name: 'voice', topic: '', position: 0, slowmode: 0, seq: 1n, textGroupId: GROUP }]);
    await toReady(w);
    const groupId = new Uint8Array(16).fill(0x52); const requestId = new Uint8Array(16).fill(0x54);
    w.state.groups.push({ ...textGroup(2), groupId, kind: 3, targetId: new Uint8Array(16).fill(0x53) });
    w.state.outbox.push({ msgId: requestId, state: 0, error: '', created: 1n, body: 'skip request', type: 0, replyTo: null, attachments: [] });
    w.call(10_199, { m: 'humanInteractionRequest', communityId: toHex(COMMUNITY), channelId: toHex(CHANNEL),
      requestId: toHex(requestId), operation: 15, args, revision: '0' });
    await vi.waitFor(() => expect(w.ret(10_199)).toMatchObject({ ok: true, value: { upload: 'accepted' } }));
    expect(decode(w.core.humanInteractionSendPrepare.mock.calls[0][3])).toEqual([1n, requestId, COMMUNITY, CHANNEL, 15n, 0n, NOW_S + 30n, args]);
  });

  it('fetches a late interaction Welcome before refusing a Bot request with no local group', async () => {
    const w = world();
    w.routes.listChannels.mockResolvedValue([{ id: CHANNEL, kind: 1, mode: 0, visibility: 0, parentId: null,
      name: 'voice', topic: '', position: 0, slowmode: 0, seq: 1n, textGroupId: GROUP }]);
    await toReady(w);
    const groupId = new Uint8Array(16).fill(0x52);
    const requestId = new Uint8Array(16).fill(0x54);
    w.sync.pollWelcomes.mockImplementation(() => {
      w.state.groups.push({ ...textGroup(2), groupId, kind: 3, targetId: new Uint8Array(16).fill(0x53) });
      return Promise.resolve();
    });
    w.state.outbox.push({ msgId: requestId, state: 0, error: '', created: 1n, body: 'request', type: 0, replyTo: null, attachments: [] });
    w.call(10_195, { m: 'humanInteractionRequest', communityId: toHex(COMMUNITY), channelId: toHex(CHANNEL),
      requestId: toHex(requestId), operation: 10, args: [], revision: null });
    await vi.waitFor(() => expect(w.ret(10_195)).toEqual({ t: 'ret', id: 10_195, ok: true, value: { requestId: toHex(requestId), upload: 'accepted' } }), { timeout: 1000 });
    expect(w.sync.pollWelcomes).toHaveBeenCalledTimes(1);
    expect(w.routes.postMessage).toHaveBeenCalledWith(groupId, expect.any(Uint8Array));
  });

  it('arms a step-up grant with only the deterministic config digest and binds op 63 to the same group and request', async () => {
    const w=world(); await toReady(w);
    const groupId=new Uint8Array(16).fill(0x52), requestId=new Uint8Array(16).fill(0x54);
    w.state.groups.push({...textGroup(2),groupId,kind:3,targetId:new Uint8Array(16).fill(0x53)});
    w.state.outbox.push({msgId:requestId,state:0,error:'',created:1n,body:'secret request',type:0,replyTo:null,attachments:[]});
    const document=encode([1n,1n,'remove','log','warn',600000n,[]]);
    w.call(10_190,{m:'humanInteractionRequest',communityId:toHex(COMMUNITY),channelId:null,requestId:toHex(requestId),
      operation:63,args:['',document],revision:'7',stepUp:'one-use-step-up'});
    await vi.waitFor(()=>expect(w.ret(10_190)).toMatchObject({ok:true,value:{upload:'accepted'}}));
    const arm=w.routes.armAutoModerationGrant.mock.calls[0];
    expect(arm).toMatchObject([groupId,requestId,COMMUNITY,7n,NOW_S+30n,expect.any(Uint8Array),'one-use-step-up']);
    expect([...arm[5]]).not.toEqual([...document]);
    const digestInput=new Uint8Array(document.length);digestInput.set(document);
    const digest=new Uint8Array(await crypto.subtle.digest('SHA-256',digestInput.buffer));
    expect(arm[5]).toEqual(digest);
    const wire=decode(w.core.humanInteractionSendPrepare.mock.calls[0][3]);
    expect(wire).toEqual([1n,requestId,COMMUNITY,null,63n,7n,NOW_S+30n,['opaque-grant-token',document]]);
    expect(w.sync.runOnGroup).toHaveBeenCalledWith(groupId,expect.any(Function));
  });

  it('refuses an automatic moderation mutation without step-up before grant creation or ciphertext preparation', async () => {
    const w=world(); await toReady(w); const document=encode([1n,0n,'log','log','log',600000n,[]]);
    w.call(10_189,{m:'humanInteractionRequest',communityId:toHex(COMMUNITY),channelId:null,requestId:'54'.repeat(16),
      operation:63,args:['',document],revision:'7'});
    await vi.waitFor(()=>expect(w.ret(10_189)).toMatchObject({ok:false,error:{code:'E_BAD_INPUT'}}));
    expect(w.routes.armAutoModerationGrant).not.toHaveBeenCalled();
    expect(w.core.humanInteractionSendPrepare).not.toHaveBeenCalled();
    expect(w.routes.postMessage).not.toHaveBeenCalled();
  });

  it('reuses the armed grant and stable outbox request while an automatic moderation upload is pending', async () => {
    const w=world();await toReady(w);const groupId=new Uint8Array(16).fill(0x52),requestId=new Uint8Array(16).fill(0x54);
    w.state.groups.push({...textGroup(2),groupId,kind:3,targetId:new Uint8Array(16).fill(0x53)});
    w.state.outbox.push({msgId:requestId,state:1,error:'',created:1n,body:'pending',type:0,replyTo:null,attachments:[]});
    const document=encode([1n,1n,'remove','log','warn',600000n,[]]);
    const command={m:'humanInteractionRequest' as const,communityId:toHex(COMMUNITY),channelId:null,requestId:toHex(requestId),
      operation:63,args:['',document] as const,revision:'7',stepUp:'one-use-step-up'};
    w.call(10_188,command);await vi.waitFor(()=>expect(w.ret(10_188)).toMatchObject({ok:true,value:{upload:'pending'}}));
    w.call(10_187,command);await vi.waitFor(()=>expect(w.ret(10_187)).toMatchObject({ok:true,value:{upload:'pending'}}));
    expect(w.routes.armAutoModerationGrant).toHaveBeenCalledTimes(1);
    expect(w.core.humanInteractionSendPrepare).toHaveBeenCalledTimes(2);
    expect(w.core.sendEncrypt).not.toHaveBeenCalled();expect(w.routes.postMessage).not.toHaveBeenCalled();
  });

  it('encodes the destination as an ID and expiry for a role-panel publish', async () => {
    const w = world(); await toReady(w);
    const groupId = new Uint8Array(16).fill(0x52); const requestId = new Uint8Array(16).fill(0x54);
    const destination = new Uint8Array(16).fill(0x55); const options = encode([['✅', new Uint8Array(16).fill(0x56), 'Readers']]);
    w.state.groups.push({ ...textGroup(2), groupId, kind: 3, targetId: new Uint8Array(16).fill(0x53) });
    w.state.outbox.push({ msgId: requestId, state: 0, error: '', created: 1n, body: 'role panel request', type: 0, replyTo: null, attachments: [] });
    w.call(10_191, { m: 'humanInteractionRequest', communityId: toHex(COMMUNITY), channelId: null, requestId: toHex(requestId),
      operation: 31, args: [toHex(destination), options], revision: '0' });
    await vi.waitFor(() => expect(w.ret(10_191)).toMatchObject({ ok: true, value: { requestId: toHex(requestId), upload: 'accepted' } }));
    const wire = decode(w.core.humanInteractionSendPrepare.mock.calls[0][3]);
    expect(wire).toEqual([1n, requestId, COMMUNITY, null, 31n, 0n, NOW_S + 30n, [destination, options]]);
    expect(w.routes.postMessage).toHaveBeenCalledWith(groupId, expect.any(Uint8Array));
  });

  it('sends an authenticated announcement schedule mutation through the core worker', async () => {
    const w = world(); await toReady(w);
    const groupId = new Uint8Array(16).fill(0x52);
    const requestId = new Uint8Array(16).fill(0x54);
    const announcementId = new Uint8Array(16).fill(0x55);
    const channelId = new Uint8Array(16).fill(0x56);
    const schedule = encode([1n, announcementId, channelId, 'scheduled notice', 'one_off', 'UTC', '2026-10-23', 12n, 0n, 0n, 1n, 1n]);
    w.state.groups.push({ ...textGroup(2), groupId, kind: 3, targetId: new Uint8Array(16).fill(0x53) });
    w.state.outbox.push({ msgId: requestId, state: 0, error: '', created: 1n, body: 'announcement request', type: 0, replyTo: null, attachments: [] });
    w.call(10_184, { m: 'humanInteractionRequest', communityId: toHex(COMMUNITY), channelId: null, requestId: toHex(requestId),
      operation: 51, args: [null, new Uint8Array()], revision: '0' });
    await vi.waitFor(() => expect(w.ret(10_184)).toMatchObject({ ok: false, error: { code: 'E_BAD_INPUT' } }));
    expect(w.core.humanInteractionSendPrepare).not.toHaveBeenCalled();
    w.call(10_186, { m: 'humanInteractionRequest', communityId: toHex(COMMUNITY), channelId: null, requestId: toHex(requestId),
      operation: 51, args: [null, schedule], revision: '0' });
    await vi.waitFor(() => expect(w.ret(10_186)).toMatchObject({ ok: true, value: { requestId: toHex(requestId), upload: 'accepted' } }));
    expect(decode(w.core.humanInteractionSendPrepare.mock.calls[0][3])).toEqual(
      [1n, requestId, COMMUNITY, null, 51n, 0n, NOW_S + 30n, [null, schedule]]);
    expect(w.routes.postMessage).toHaveBeenCalledWith(groupId, expect.any(Uint8Array));
    w.call(10_185, { m: 'readBotInteractionResponses', communityId: toHex(COMMUNITY), operations: [50, 51, 52] });
    await vi.waitFor(() => expect(w.ret(10_185)).toMatchObject({ ok: true }));
  });

  it('reads matching Bot responses and leases acknowledgements to only those delivery ids', async () => {
    const w = world();
    await toReady(w);
    const requestId = new Uint8Array(16).fill(0x54);
    const channelId = new Uint8Array(16).fill(0x55);
    const responseBody = encodeBotResponse({ requestId, communityId: COMMUNITY, status: 0, revision: 8n, data: encode([]) });
    const musicEvent = { deliveryId: 2n, groupId: GROUP, communityId: COMMUNITY, requestId, sourceSeq: 4n,
      epoch: 1n, receivedAt: NOW_S, senderUser: USER, senderDevice: DEVICE, messageId: new Uint8Array(16).fill(0x61),
      body: responseBody, operation: 13, channelId };
    w.core.humanInteractionEventsRead.mockReturnValue({ events: [musicEvent], next: 2n, highWater: 2n });
    w.call(10_192, { m: 'readBotInteractionResponses', communityId: toHex(COMMUNITY), operations: [13] });
    await vi.waitFor(() => expect(w.ret(10_192)).toMatchObject({ ok: true, value: { blocked: false,
      responses: [{ requestId: toHex(requestId), operation: 13, channelId: toHex(channelId), status: 0, revision: '8', data: encode([]) }] } }));
    const leaseA = interactionLease(w, 10_192);
    w.call(10_191, { m: 'ackBotInteractionResponses', communityId: toHex(COMMUNITY), through: leaseA });
    await vi.waitFor(() => expect(w.ret(10_191)).toMatchObject({ ok: true, value: null }));
    expect(w.core.humanInteractionEventsAckSelected).toHaveBeenCalledWith([2n]);

    const moderationEvent = { ...musicEvent, deliveryId: 3n, operation: 60, channelId: null };
    w.core.humanInteractionEventsRead.mockReturnValue({ events: [moderationEvent, { ...musicEvent, deliveryId: 4n }], next: 4n, highWater: 4n });
    w.call(10_190, { m: 'readBotInteractionResponses', communityId: toHex(COMMUNITY), operations: [13] });
    await vi.waitFor(() => expect(w.ret(10_190)).toMatchObject({ ok: true, value: { blocked: false,
      responses: [{ requestId: toHex(requestId), operation: 13 }] } }));
    interactionLease(w, 10_190);
    w.call(10_189, { m: 'ackBotInteractionResponses', communityId: toHex(COMMUNITY), through: '4'.padStart(32, '0') });
    await vi.waitFor(() => expect(w.ret(10_189)).toMatchObject({ ok: false, error: { code: 'E_BAD_INPUT' } }));
    expect(w.core.humanInteractionEventsAckSelected).toHaveBeenCalledTimes(1);
  });

  it('recovers an exact archived receipt after worker recreation without creating an ACK lease', async () => {
    const w = world(); await toReady(w);
    const requestId = new Uint8Array(16).fill(0x5c);
    const body = encodeBotResponse({ requestId, communityId: COMMUNITY, status: 0, revision: 4n, data: encode([1n]) });
    w.core.humanInteractionEventsRead.mockReturnValue({ events: [], next: 0n, highWater: 9n });
    const archivedEvent: HumanInteractionEvent = { deliveryId: 9n, groupId: GROUP, communityId: COMMUNITY,
      requestId, sourceSeq: 8n, epoch: 2n, receivedAt: 30n, senderUser: USER, senderDevice: DEVICE,
      messageId: new Uint8Array(16).fill(0x5d), body, operation: 84, channelId: null };
    w.core.humanInteractionReceiptsRead.mockReturnValue([archivedEvent]);
    w.call(10_188, { m: 'readBotInteractionResponses', communityId: toHex(COMMUNITY), operations: [84], requestIds: [toHex(requestId)] });
    await vi.waitFor(() => expect(w.ret(10_188)).toMatchObject({ ok: true, value: {
      through: null, responses: [{ deliveryId: '9', requestId: toHex(requestId), operation: 84, revision: '4', data: encode([1n]) }],
    } }));
    expect(w.core.humanInteractionReceiptsRead).toHaveBeenCalledWith(COMMUNITY, new Uint8Array(16).fill(0x73), USER, [requestId]);
    expect(w.core.humanInteractionEventsAckSelected).not.toHaveBeenCalled();
  });

  it('does not query archived receipts when the current Bot installation is disabled', async () => {
    const w = world(); await toReady(w);
    w.routes.getBotCommandStatus.mockResolvedValue({ installationId: new Uint8Array(16).fill(0x73), botUserId: USER, state: 4, revision: 2n });
    w.call(10_187, { m: 'readBotInteractionResponses', communityId: toHex(COMMUNITY), operations: [84], requestIds: ['5c'.repeat(16)] });
    await vi.waitFor(() => expect(w.ret(10_187)).toMatchObject({ ok: false, error: { code: 'E_FORBIDDEN' } }));
    expect(w.core.humanInteractionReceiptsRead).not.toHaveBeenCalled();
  });

  it('reads member-safe Bot command status without requiring management settings authority', async () => {
    const w = world(); await toReady(w);
    w.routes.getBotCommandStatus.mockResolvedValue({ installationId: new Uint8Array(16).fill(0x73), botUserId: USER, state: 3, revision: 9n });
    w.call(10_176, { m: 'getBotCommandStatus', communityId: toHex(COMMUNITY) });
    await vi.waitFor(() => expect(w.ret(10_176)).toMatchObject({ ok: true, value: {
      installationId: '73'.repeat(16), botUserId: toHex(USER), state: 3, revision: '9',
    } }));
    expect(w.routes.getBotInstallation).not.toHaveBeenCalled();
  });

  it('reads operation 0 and music provider status 24 through exact request IDs and selected ACK leases', async () => {
    const w = world(); await toReady(w);
    const capabilityRequest = new Uint8Array(16).fill(0x69);
    const statusRequest = new Uint8Array(16).fill(0x6a);
    const event = (deliveryId: bigint, requestId: Uint8Array, operation: number) => ({
      deliveryId, groupId: GROUP, communityId: COMMUNITY, requestId, sourceSeq: deliveryId, epoch: 1n,
      receivedAt: 1n, senderUser: USER, senderDevice: DEVICE, messageId: requestId,
      body: encodeBotResponse({ requestId, communityId: COMMUNITY, status: 0, revision: null, data: encode([1n]) }),
      operation, channelId: null,
    });
    w.core.humanInteractionEventsRead.mockReturnValue({ events: [event(1n, capabilityRequest, 0), event(2n, statusRequest, 24)], next: 2n, highWater: 2n });
    w.call(10_174, { m: 'readBotInteractionResponses', communityId: toHex(COMMUNITY), operations: [0, 24],
      requestIds: [toHex(capabilityRequest), toHex(statusRequest)] });
    await vi.waitFor(() => expect(w.ret(10_174)).toMatchObject({ ok: true, value: {
      responses: [{ operation: 0, requestId: toHex(capabilityRequest), channelId: null }, { operation: 24, requestId: toHex(statusRequest), channelId: null }],
    } }));
    const lease = interactionLease(w, 10_174);
    w.call(10_173, { m: 'ackBotInteractionResponses', communityId: toHex(COMMUNITY), through: lease });
    await vi.waitFor(() => expect(w.ret(10_173)).toMatchObject({ ok: true, value: null }));
    expect(w.core.humanInteractionEventsAckSelected).toHaveBeenCalledWith([1n, 2n]);
  });

  it('invalidates read-bound ACK capabilities when the worker forgets an account/core', async () => {
    const w = world(); await toReady(w);
    const requestId = new Uint8Array(16).fill(0x5a);
    const body = encodeBotResponse({ requestId, communityId: COMMUNITY, status: 0, revision: null, data: encode([]) });
    w.core.humanInteractionEventsRead.mockReturnValue({ events: [{ deliveryId: 9n, groupId: GROUP, communityId: COMMUNITY,
      requestId, sourceSeq: 9n, epoch: 1n, receivedAt: 1n, senderUser: USER, senderDevice: DEVICE,
      messageId: requestId, body, operation: 3, channelId: null }], next: 9n, highWater: 9n });
    w.call(10_177, { m: 'readBotInteractionResponses', communityId: toHex(COMMUNITY), operations: [3], requestIds: [toHex(requestId)] });
    await vi.waitFor(() => expect(w.ret(10_177)).toMatchObject({ ok: true, value: {} }));
    const lease = interactionLease(w, 10_177);
    (w.controller as unknown as { forgetAccount: () => void }).forgetAccount();
    w.call(10_178, { m: 'ackBotInteractionResponses', communityId: toHex(COMMUNITY), through: lease });
    await vi.waitFor(() => expect(w.ret(10_178)).toMatchObject({ ok: false, error: { code: 'E_BAD_INPUT' } }));
    expect(w.core.humanInteractionEventsAckSelected).not.toHaveBeenCalled();
  });

  it('normalizes and sends a poll mutation through the real worker request path with its fixed 30-second expiry', async () => {
    const w = world();
    await toReady(w);
    const requestId = new Uint8Array(16).fill(0x71);
    const groupId = new Uint8Array(16).fill(0x52);
    const channelId = new Uint8Array(16).fill(0x73);
    const payload = encode([1n, channelId, null, 'Lunch?', ['Pizza', 'Salad'], 0n, 3600n]);
    expect(() => encodeBotRequestBytes({ requestId, communityId: COMMUNITY, channelId: null, operation: 70,
      revision: 0n, expires: NOW_S + 30n, args: [payload] })).not.toThrow();
    expect(() => validateBotContext({ requestId, communityId: COMMUNITY, channelId: null, operation: 70,
      revision: 0n, expires: NOW_S + 30n, args: [payload] }, { community: COMMUNITY, now: NOW_S })).not.toThrow();
    w.state.groups.push({ ...textGroup(2), groupId, kind: 3, targetId: new Uint8Array(16).fill(0x74) });
    w.core.humanInteractionSendPrepare.mockImplementation((rid) => {
      w.state.outbox.push({ msgId: rid, state: 0, error: '', created: 1n, body: 'poll request', type: 0, replyTo: null, attachments: [] });
      return rid;
    });
    w.call(10_181, { m: 'humanInteractionRequest', communityId: toHex(COMMUNITY), channelId: null,
      requestId: toHex(requestId), operation: 70, args: [payload], revision: '0' });
    await vi.waitFor(() => expect(w.core.humanInteractionSendPrepare).toHaveBeenCalledTimes(1));
    await vi.waitFor(() => expect(w.ret(10_181)).toMatchObject({ ok: true, value: { requestId: toHex(requestId), upload: 'accepted' } }));
    expect(decode(w.core.humanInteractionSendPrepare.mock.calls[0][3])).toEqual(
      [1n, requestId, COMMUNITY, null, 70n, 0n, NOW_S + 30n, [payload]]);
    expect(w.routes.postMessage).toHaveBeenCalledWith(groupId, expect.any(Uint8Array));
  });

  it.each([['threaded', '76'.repeat(16), new Uint8Array(16).fill(0x76)], ['unthreaded', null, null]])(
    'normalizes op72 %s request IDs through the real worker path', async (_label, threadText, threadBytes) => {
      const w = world(); await toReady(w);
      const groupId = new Uint8Array(16).fill(0x52);
      const channelId = new Uint8Array(16).fill(0x75);
      const requestId = new Uint8Array(16).fill(threadText === null ? 0x78 : 0x77);
      w.state.groups.push({ ...textGroup(2), groupId, kind: 3, targetId: new Uint8Array(16).fill(0x74) });
      w.core.humanInteractionSendPrepare.mockImplementation(rid => {
        w.state.outbox.push({ msgId: rid, state: 0, error: '', created: 1n, body: 'poll list', type: 0, replyTo: null, attachments: [] });
        return rid;
      });
      w.call(10_175, { m: 'humanInteractionRequest', communityId: toHex(COMMUNITY), channelId: null,
        requestId: toHex(requestId), operation: 72, args: [toHex(channelId), threadText, null], revision: null });
      await vi.waitFor(() => expect(w.ret(10_175)).toMatchObject({ ok: true, value: { upload: 'accepted' } }));
      expect(decode(w.core.humanInteractionSendPrepare.mock.calls[0][3])).toEqual(
        [1n, requestId, COMMUNITY, null, 72n, null, null, [channelId, threadBytes, null]]);
    });

  it('finds exact same-operation requests beyond the first hundred rows and gives concurrent consumers separate leases', async () => {
    const w = world();
    await toReady(w);
    const requestA = new Uint8Array(16).fill(0xa1);
    const requestB = new Uint8Array(16).fill(0xb2);
    const channelId = new Uint8Array(16).fill(0x55);
    const noise = Array.from({ length: 100 }, (_, i) => ({ deliveryId: BigInt(i + 1), groupId: GROUP,
      communityId: NEW_COMMUNITY, requestId: new Uint8Array(16).fill(i + 1), sourceSeq: BigInt(i + 1), epoch: 1n,
      receivedAt: 1n, senderUser: USER, senderDevice: DEVICE, messageId: new Uint8Array(16).fill(0x61),
      body: 'opaque', operation: 13, channelId }));
    const event = (deliveryId: bigint, requestId: Uint8Array) => ({ deliveryId, groupId: GROUP, communityId: COMMUNITY,
      requestId, sourceSeq: deliveryId, epoch: 1n, receivedAt: 1n, senderUser: USER, senderDevice: DEVICE,
      messageId: new Uint8Array(16).fill(Number(deliveryId)),
      body: encodeBotResponse({ requestId, communityId: COMMUNITY, status: 0, revision: null, data: encode([]) }),
      operation: 13, channelId });
    w.core.humanInteractionEventsRead.mockImplementation((after) => after === 0n
      ? { events: noise, next: 100n, highWater: 102n }
      : { events: [event(101n, requestA), event(102n, requestB)], next: 102n, highWater: 102n });
    w.call(10_182, { m: 'readBotInteractionResponses', communityId: toHex(COMMUNITY), operations: [13], requestIds: [toHex(requestA)] });
    await vi.waitFor(() => expect(w.ret(10_182)).toMatchObject({ ok: true, value: { responses: [{ deliveryId: '101', requestId: toHex(requestA) }] } }));
    const leaseA = interactionLease(w, 10_182);
    w.call(10_183, { m: 'readBotInteractionResponses', communityId: toHex(COMMUNITY), operations: [13], requestIds: [toHex(requestB)] });
    await vi.waitFor(() => expect(w.ret(10_183)).toMatchObject({ ok: true, value: { responses: [{ deliveryId: '102', requestId: toHex(requestB) }] } }));
    const leaseB = interactionLease(w, 10_183);
    expect(w.core.humanInteractionEventsRead.mock.calls.filter(([after]) => after === 100n)).toHaveLength(2);
    w.call(10_184, { m: 'ackBotInteractionResponses', communityId: toHex(COMMUNITY), through: leaseA });
    w.call(10_185, { m: 'ackBotInteractionResponses', communityId: toHex(COMMUNITY), through: leaseB });
    await vi.waitFor(() => expect(w.core.humanInteractionEventsAckSelected).toHaveBeenCalledTimes(2));
    expect(w.core.humanInteractionEventsAckSelected.mock.calls).toEqual([[[101n]], [[102n]]]);
  });

  it.each([
    ['no kind-3 group', [], COMMUNITY],
    ['wrong community', [{ ...textGroup(2), groupId: new Uint8Array(16).fill(0x52), kind: 3, communityId: new Uint8Array(16).fill(0x77) }], COMMUNITY],
  ] as const)('refuses a moderation request for %s before preparing or uploading', async (_label, groups, community) => {
    const w = world();
    await toReady(w);
    w.state.groups.push(...groups);
    w.call(10_197, { m: 'listModerationCases', communityId: toHex(community), requestId: '54'.repeat(16), offset: 0 });
    await vi.waitFor(() => expect(w.ret(10_197)).toMatchObject({ ok: false, error: { code: 'E_CORE_STATE' } }));
    expect(w.core.humanInteractionSendPrepare).not.toHaveBeenCalled();
    expect(w.routes.postMessage).not.toHaveBeenCalled();
  });

  it('keeps an uncertain send pending on transport failure and never uploads it again on retry', async () => {
    const w = world();
    await toReady(w);
    const groupId = new Uint8Array(16).fill(0x52);
    const requestId = new Uint8Array(16).fill(0x54);
    w.state.groups.push({ ...textGroup(2), groupId, kind: 3 });
    w.state.outbox.push({ msgId: requestId, state: 0, error: '', created: 1n, body: 'secret request', type: 0, replyTo: null, attachments: [] });
    w.routes.postMessage.mockRejectedValueOnce(new Error('connection lost'));
    const command = { m: 'listModerationCases', communityId: toHex(COMMUNITY), requestId: toHex(requestId), offset: 0 } as const;
    w.call(10_196, command);
    await vi.waitFor(() => expect(w.ret(10_196)).toMatchObject({ ok: true, value: { requestId: toHex(requestId), status: 'pending' } }));
    w.state.outbox[0].state = 1;
    w.call(10_195, command);
    await vi.waitFor(() => expect(w.ret(10_195)).toMatchObject({ ok: true, value: { requestId: toHex(requestId), status: 'pending' } }));
    expect(w.routes.postMessage).toHaveBeenCalledTimes(1);
    expect(w.core.sendEncrypt).toHaveBeenCalledTimes(1);
  });

  it('freshly authorizes, decodes, and leaves moderation responses pending until the page acknowledges them', async () => {
    const w = world();
    await toReady(w);
    const requestId = new Uint8Array(16).fill(0x54);
    const groupId = new Uint8Array(16).fill(0x52);
    const row = [toHex(COMMUNITY), 'b2'.repeat(16), 'c3'.repeat(16), 'd4'.repeat(16), 'e5'.repeat(16), 7n,
      'duplicate', 'remove', 'bounded excerpt', 1_800_000_000n];
    const body = encodeBotResponse({ requestId, communityId: COMMUNITY, status: 0, revision: null,
      data: encode([1, [row], null]) });
    w.core.humanInteractionEventsRead.mockReturnValue({ events: [{ deliveryId: 2n, groupId, communityId: COMMUNITY,
      requestId, sourceSeq: 4n, epoch: 1n, receivedAt: 1_800_000_000n, senderUser: USER, senderDevice: DEVICE,
      messageId: new Uint8Array(16).fill(0x61), body, operation: 60, channelId: null }], next: 2n, highWater: 2n });
    w.call(90, { m: 'readHumanInteractionResponses', communityId: toHex(COMMUNITY), requestIds: [toHex(requestId)] });
    await vi.waitFor(() => expect(w.ret(90)).toMatchObject({ ok: true, value: { authorized: true, responses: [
      { deliveryId: '2', requestId: toHex(requestId), status: 0, hasMore: false,
        cases: [{ messageId: 'e5'.repeat(16), revision: '7', excerpt: 'bounded excerpt' }] },
    ] } }));
    expect(w.routes.getPermissions.mock.invocationCallOrder[0]).toBeLessThan(w.core.humanInteractionEventsRead.mock.invocationCallOrder[0]);
    expect(w.core.humanInteractionEventsAckSelected).not.toHaveBeenCalled();
    const lease = interactionLease(w, 90);
    w.call(91, { m: 'ackHumanInteractionResponses', communityId: toHex(COMMUNITY), through: lease });
    await vi.waitFor(() => expect(w.ret(91)).toMatchObject({ ok: true, value: null }));
    expect(w.core.humanInteractionEventsAckSelected).toHaveBeenCalledWith([2n]);
    expect(w.routes.getPermissions.mock.calls.length).toBeGreaterThanOrEqual(2);
  });

  it('does not read or acknowledge moderation response bodies without a fresh audit permission', async () => {
    const w = world();
    await toReady(w);
    w.routes.getPermissions.mockResolvedValue({ bits: 0n, top: 0 });
    w.call(92, { m: 'readHumanInteractionResponses', communityId: toHex(COMMUNITY) });
    await vi.waitFor(() => expect(w.ret(92)).toMatchObject({ ok: true, value: { authorized: false, responses: [], through: null } }));
    expect(w.core.humanInteractionEventsRead).not.toHaveBeenCalled();
    expect(w.core.humanInteractionEventsAckSelected).not.toHaveBeenCalled();
  });

  it('decodes op64 manual evidence rows and returns the continuation cursor', async () => {
    const w = world();
    await toReady(w);
    const requestId = new Uint8Array(16).fill(0x56);
    const evidenceId = new Uint8Array(16).fill(0x57);
    const body = encodeBotResponse({ requestId, communityId: COMMUNITY, status: 0, revision: null,
      data: encode([1, [[evidenceId, 'warn', 'reviewed reason', '', 1_800_000_000n]], new Uint8Array([1, 2])]) });
    w.core.humanInteractionEventsRead.mockReturnValue({ events: [{ deliveryId: 3n, groupId: GROUP, communityId: COMMUNITY,
      requestId, sourceSeq: 5n, epoch: 1n, receivedAt: 1_800_000_000n, senderUser: USER, senderDevice: DEVICE,
      messageId: new Uint8Array(16).fill(0x61), body, operation: 64, channelId: null }], next: 3n, highWater: 3n });
    w.call(96, { m: 'readHumanInteractionResponses', communityId: toHex(COMMUNITY) });
    await vi.waitFor(() => expect(w.ret(96)).toMatchObject({ ok: true, value: { authorized: true, responses: [{
      requestId: toHex(requestId), hasMore: true, nextCursor: '0102',
      cases: [{ key: `manual:${toHex(evidenceId)}`, reason: 'reviewed reason', action: 'warn', excerpt: '' }],
    }] } }));
  });

  it('sends a community-bound op64 request with the opaque continuation cursor', async () => {
    const w = world();
    await toReady(w);
    const requestId = new Uint8Array(16).fill(0x58);
    const cursor = new Uint8Array([1, 2]);
    const groupId = new Uint8Array(16).fill(0x52);
    w.state.groups.push({ ...textGroup(2), groupId, kind: 3 });
    w.core.humanInteractionSendPrepare.mockImplementation((rid) => {
      w.state.outbox.push({ msgId: rid, state: 0, error: '', created: 1n, body: 'evidence page request', type: 0, replyTo: null, attachments: [] });
      return rid;
    });
    w.call(97, { m: 'listManualModerationEvidence', communityId: toHex(COMMUNITY), requestId: toHex(requestId), cursor: '0102' });
    await vi.waitFor(() => expect(w.core.humanInteractionSendPrepare).toHaveBeenCalledTimes(1));
    const request = decode(w.core.humanInteractionSendPrepare.mock.calls[0][3]) as unknown[];
    expect(request).toEqual([1n, requestId, COMMUNITY, null, 64n, null, null, [cursor]]);
    expect(w.routes.postMessage).toHaveBeenCalledWith(groupId, expect.any(Uint8Array));
  });

  it('retries Welcome catch-up until the late private interaction group is available', async () => {
    const w = world();
    await toReady(w);
    const groupId = new Uint8Array(16).fill(0x52);
    const requestId = new Uint8Array(16).fill(0x59);
    let welcomePolls = 0;
    w.sync.pollWelcomes.mockImplementation(() => {
      welcomePolls++;
      if (welcomePolls === 20) w.state.groups.push({ ...textGroup(2), groupId, kind: 3 });
      return Promise.resolve();
    });
    w.core.humanInteractionSendPrepare.mockImplementation((rid) => {
      w.state.outbox.push({ msgId: rid, state: 0, error: '', created: 1n, body: 'evidence page request', type: 0, replyTo: null, attachments: [] });
      return rid;
    });
    w.call(98, { m: 'listManualModerationEvidence', communityId: toHex(COMMUNITY), requestId: toHex(requestId), cursor: null });
    await vi.waitFor(() => expect(w.core.humanInteractionSendPrepare).toHaveBeenCalledTimes(1));
    expect(w.sync.pollWelcomes).toHaveBeenCalledTimes(20);
    expect(w.routes.postMessage).toHaveBeenCalledWith(groupId, expect.any(Uint8Array));
  });

  it('selectively acknowledges this community response while preserving another community response', async () => {
    const w = world();
    await toReady(w);
    const requestId = new Uint8Array(16).fill(0x54);
    const row = [toHex(COMMUNITY), 'b2'.repeat(16), 'c3'.repeat(16), 'd4'.repeat(16), 'e5'.repeat(16), 7n,
      'duplicate', 'remove', 'bounded excerpt', 1_800_000_000n];
    const body = encodeBotResponse({ requestId, communityId: COMMUNITY, status: 0, revision: null, data: encode([1, [row], null]) });
    w.core.humanInteractionEventsRead.mockReturnValue({ events: [
      { deliveryId: 2n, groupId: GROUP, communityId: COMMUNITY, requestId, sourceSeq: 4n, epoch: 1n,
        receivedAt: 1_800_000_000n, senderUser: USER, senderDevice: DEVICE, messageId: new Uint8Array(16).fill(0x61), body, operation: 60, channelId: null },
      { deliveryId: 3n, groupId: GROUP, communityId: NEW_COMMUNITY, requestId, sourceSeq: 5n, epoch: 1n,
        receivedAt: 1_800_000_000n, senderUser: USER, senderDevice: DEVICE, messageId: new Uint8Array(16).fill(0x62), body, operation: 60, channelId: null },
    ], next: 3n, highWater: 3n });
    w.call(93, { m: 'readHumanInteractionResponses', communityId: toHex(COMMUNITY), requestIds: [toHex(requestId)] });
    await vi.waitFor(() => expect(w.ret(93)).toMatchObject({ ok: true, value: { blocked: false, responses: [{
      cases: [{ excerpt: 'bounded excerpt' }],
    }] } }));
    const lease = interactionLease(w, 93);
    w.call(94, { m: 'ackHumanInteractionResponses', communityId: toHex(COMMUNITY), through: '3'.padStart(32, '0') });
    await vi.waitFor(() => expect(w.ret(94)).toMatchObject({ ok: false, error: { code: 'E_BAD_INPUT' } }));
    expect(w.core.humanInteractionEventsAckSelected).not.toHaveBeenCalled();
    w.call(95, { m: 'ackHumanInteractionResponses', communityId: toHex(COMMUNITY), through: lease });
    await vi.waitFor(() => expect(w.ret(95)).toMatchObject({ ok: true, value: null }));
    expect(w.core.humanInteractionEventsAckSelected).toHaveBeenCalledWith([2n]);
  });

  it('finds this community response after another community event with an exact read lease', async () => {
    const w = world();
    await toReady(w);
    const requestId = new Uint8Array(16).fill(0x54);
    const row = [toHex(COMMUNITY), 'b2'.repeat(16), 'c3'.repeat(16), 'd4'.repeat(16), 'e5'.repeat(16), 7n,
      'duplicate', 'remove', 'bounded excerpt', 1_800_000_000n];
    const body = encodeBotResponse({ requestId, communityId: COMMUNITY, status: 0, revision: null, data: encode([1, [row], null]) });
    w.core.humanInteractionEventsRead.mockReturnValue({ events: [
      { deliveryId: 2n, groupId: GROUP, communityId: NEW_COMMUNITY, requestId, sourceSeq: 4n, epoch: 1n,
        receivedAt: 1_800_000_000n, senderUser: USER, senderDevice: DEVICE, messageId: new Uint8Array(16).fill(0x62), body, operation: 60, channelId: null },
      { deliveryId: 3n, groupId: GROUP, communityId: COMMUNITY, requestId, sourceSeq: 5n, epoch: 1n,
        receivedAt: 1_800_000_000n, senderUser: USER, senderDevice: DEVICE, messageId: new Uint8Array(16).fill(0x61), body, operation: 60, channelId: null },
    ], next: 3n, highWater: 3n });
    w.call(97, { m: 'readHumanInteractionResponses', communityId: toHex(COMMUNITY), requestIds: [toHex(requestId)] });
    await vi.waitFor(() => expect(w.ret(97)).toMatchObject({ ok: true, value: { blocked: false, responses: [
      { deliveryId: '3', cases: [{ excerpt: 'bounded excerpt' }] },
    ] } }));
    interactionLease(w, 97);
    w.call(98, { m: 'ackHumanInteractionResponses', communityId: toHex(COMMUNITY), through: '3'.padStart(32, '0') });
    await vi.waitFor(() => expect(w.ret(98)).toMatchObject({ ok: false, error: { code: 'E_BAD_INPUT' } }));
    expect(w.core.humanInteractionEventsAckSelected).not.toHaveBeenCalled();
  });

  it('refuses to acknowledge a previously authorized range after the server revokes audit access', async () => {
    const w = world();
    await toReady(w);
    const requestId = new Uint8Array(16).fill(0x66);
    const body = encodeBotResponse({ requestId, communityId: COMMUNITY, status: 0, revision: null, data: encode([1, [], null]) });
    w.core.humanInteractionEventsRead.mockReturnValue({ events: [{ deliveryId: 5n, groupId: GROUP, communityId: COMMUNITY,
      requestId, sourceSeq: 1n, epoch: 1n, receivedAt: 1n,
      senderUser: USER, senderDevice: DEVICE, messageId: new Uint8Array(16).fill(0x67), body, operation: 60, channelId: null }], next: 5n, highWater: 5n });
    w.call(96, { m: 'readHumanInteractionResponses', communityId: toHex(COMMUNITY) });
    await vi.waitFor(() => expect(w.ret(96)).toMatchObject({ ok: true, value: { authorized: true } }));
    const lease = interactionLease(w, 96);
    w.routes.getPermissions.mockResolvedValue({ bits: 0n, top: 0 });
    w.call(97, { m: 'ackHumanInteractionResponses', communityId: toHex(COMMUNITY), through: lease });
    await vi.waitFor(() => expect(w.ret(97)).toMatchObject({ ok: false, error: { code: 'E_FORBIDDEN' } }));
    expect(w.core.humanInteractionEventsAckSelected).not.toHaveBeenCalled();
  });

  it('signs a sequence confirmed in the active community timeline and preserves server moderation refusals', async () => {
    const w = world();
    w.routes.listRoles.mockResolvedValue([{ id: ROLE_EVERYONE, name: '@everyone', color: 0, position: 0, allow: 1, deny: 0,
      hoist: false, mentionable: false }]);
    await toReady(w);
    w.call(10_200, { m: 'selectCommunity', communityId: toHex(COMMUNITY) });
    await vi.waitFor(() => expect(w.ret(10_200)).toMatchObject({ ok: true, value: null }));
    w.state.groups.push(textGroup(2), { ...textGroup(2), groupId: new Uint8Array(16).fill(0x52), kind: 3, targetId: new Uint8Array(16).fill(0x53) });
    w.core.humanInteractionSendPrepare.mockImplementation((requestId) => {
      w.state.outbox.push({ msgId: requestId, state: 0, error: '', created: NOW_S, body: 'evidence request', type: 0, replyTo: null, attachments: [] });
      return requestId;
    });
    w.state.rows.push(strangerRow(3));
    const msgId = toHex(w.state.rows[0].msgId!);
    w.call(10_201, { m: 'moderateRemoveMessage', channelId: toHex(CHANNEL), msgId, seq: '3', reason: 'reviewed by moderator', excerpt: 'bounded excerpt' });
    await vi.waitFor(() => expect(w.ret(10_201)).toMatchObject({ ok: true, value: { removal: 'accepted', evidenceUpload: 'accepted' } }));
    const accepted = w.ret(10_201) as { ok: true; value: { proofRequestId: string; evidenceRequestId: string } };
    expect(accepted.value.proofRequestId).toMatch(/^[0-9a-f]{32}$/u);
    expect(accepted.value.evidenceRequestId).toMatch(/^[0-9a-f]{32}$/u);
    expect(w.core.prepareManualRemovalEvidence).toHaveBeenCalledWith(expect.any(Uint8Array), expect.any(Uint8Array), COMMUNITY,
      GROUP, CHANNEL, w.state.rows[0].msgId, 3n, 'reviewed by moderator', 'bounded excerpt', 1_700_000_000n, 1_700_000_025n, 1_700_000_000n);
    expect(w.routes.postModerationRemoval).toHaveBeenCalledWith(GROUP, expect.any(Uint8Array));
    const prepared = w.core.humanInteractionSendPrepare.mock.calls[0];
    const evidenceRequest = decode(prepared[3]) as unknown[];
    expect(evidenceRequest[4]).toBe(61n);
    expect(evidenceRequest[5]).toBe(0n);
    expect(evidenceRequest[6]).toBe(1_700_000_030n);
    expect(evidenceRequest[1]).toEqual(prepared[0]);
    const removalRequest = w.core.prepareManualRemovalEvidence.mock.calls[0];
    expect(evidenceRequest[1]).toEqual(prepared[0]);
    expect((w.ret(10_201) as { ok: true; value: { proofRequestId: string; evidenceRequestId: string } }).value)
      .toMatchObject({ proofRequestId: toHex(removalRequest[0]), evidenceRequestId: toHex(removalRequest[1]) });
    expect(evidenceRequest[7]).toEqual([removalRequest[0], 'reviewed by moderator', 'bounded excerpt']);
    expect(w.core.markManualRemovalSubmitted.mock.invocationCallOrder[0]).toBeLessThan(w.routes.postModerationRemoval.mock.invocationCallOrder[0]);
    expect(w.routes.postModerationRemoval.mock.invocationCallOrder[0]).toBeLessThan(w.core.humanInteractionSendPrepare.mock.invocationCallOrder[0]);
    expect(w.routes.postMessage).toHaveBeenCalledWith(new Uint8Array(16).fill(0x52), expect.any(Uint8Array));

    w.routes.postModerationRemoval.mockRejectedValue(refusal(403, 'E_FORBIDDEN'));
    w.call(10_202, { m: 'moderateRemoveMessage', channelId: toHex(CHANNEL), msgId, seq: '3', reason: 'reviewed by moderator', excerpt: 'bounded excerpt' });
    await vi.waitFor(() => expect(w.ret(10_202)).toMatchObject({ ok: true, value: { removal: 'rejected' } }));
    expect(w.state.manualEvidence).toHaveLength(0);
    expect(w.core.rejectManualRemoval).toHaveBeenCalledTimes(1);
    expect(w.core.humanInteractionSendPrepare).toHaveBeenCalledTimes(1);
  });

  it.each([
    ['DM channel', DM_CHANNEL, 'E_BAD_INPUT'],
    ['mismatched sequence', CHANNEL, 'E_BAD_INPUT'],
    ['mismatched message id', CHANNEL, 'E_BAD_INPUT'],
  ] as const)('rejects moderator removal for a %s before signing or HTTP', async (label, channelId, code) => {
    const w = world();
    w.routes.listRoles.mockResolvedValue([{ id: ROLE_EVERYONE, name: '@everyone', color: 0, position: 0, allow: 1, deny: 0,
      hoist: false, mentionable: false }]);
    await toReady(w);
    w.call(10_210, { m: 'selectCommunity', communityId: toHex(COMMUNITY) });
    await vi.waitFor(() => expect(w.ret(10_210)).toMatchObject({ ok: true, value: null }));
    w.state.groups.push(textGroup(2));
    w.state.rows.push(strangerRow(3));
    const actualMsgId = toHex(w.state.rows[0].msgId!);
    const request = label === 'mismatched sequence' ? { channelId: toHex(channelId), msgId: actualMsgId, seq: '4' }
      : { channelId: toHex(channelId), msgId: label === 'mismatched message id' ? 'ff'.repeat(16) : actualMsgId, seq: '3' };
    w.call(10_211, { m: 'moderateRemoveMessage', ...request, reason: 'reviewed by moderator', excerpt: 'bounded excerpt' });
    await vi.waitFor(() => expect(w.ret(10_211)).toMatchObject({ ok: false, error: { code } }));
    expect(w.core.prepareManualRemovalEvidence).not.toHaveBeenCalled();
    expect(w.routes.postModerationRemoval).not.toHaveBeenCalled();
  });

  it('reports an accepted removal separately when op61 is refused, so the page can retry evidence only', async () => {
    const w = world();
    w.routes.listRoles.mockResolvedValue([{ id: ROLE_EVERYONE, name: '@everyone', color: 0, position: 0, allow: 1, deny: 0,
      hoist: false, mentionable: false }]);
    await toReady(w);
    w.call(10_213, { m: 'selectCommunity', communityId: toHex(COMMUNITY) });
    await vi.waitFor(() => expect(w.ret(10_213)).toMatchObject({ ok: true, value: null }));
    w.state.groups.push(textGroup(2), { ...textGroup(2), groupId: new Uint8Array(16).fill(0x52), kind: 3, targetId: new Uint8Array(16).fill(0x53) });
    w.core.humanInteractionSendPrepare.mockImplementation((requestId) => {
      w.state.outbox.push({ msgId: requestId, state: 0, error: '', created: NOW_S, body: 'evidence request', type: 0, replyTo: null, attachments: [] });
      return requestId;
    });
    w.state.rows.push(strangerRow(3));
    w.routes.postMessage.mockRejectedValue(new DillaHttpError({ status: 403, code: 'E_FORBIDDEN', detail: '', retryAfterMs: null, extra: [] }));
    w.call(10_214, { m: 'moderateRemoveMessage', channelId: toHex(CHANNEL), msgId: toHex(w.state.rows[0].msgId!), seq: '3',
      reason: 'reviewed by moderator', excerpt: 'bounded excerpt' });
    await vi.waitFor(() => expect(w.ret(10_214)).toMatchObject({ ok: true, value: { removal: 'accepted', evidenceUpload: 'rejected' } }));
    const rejectedEvidence = w.ret(10_214) as { ok: true; value: { proofRequestId: string; evidenceRequestId: string } };
    expect(rejectedEvidence.value.proofRequestId).toMatch(/^[0-9a-f]{32}$/u);
    expect(rejectedEvidence.value.evidenceRequestId).toMatch(/^[0-9a-f]{32}$/u);
    expect(w.routes.postModerationRemoval).toHaveBeenCalledTimes(1);
    expect(w.core.humanInteractionSendPrepare).toHaveBeenCalledTimes(1);
    expect(w.core.sendFail).toHaveBeenCalledWith(expect.any(Uint8Array), 'E_FORBIDDEN');
  });

  it.each([
    ['reason', '🧪'.repeat(257), 'excerpt'],
    ['excerpt', 'reviewed by moderator', '🧪'.repeat(129)],
  ])('rejects a moderator removal with a %s over the UTF-8 byte limit before signing', async (_label, reason, excerpt) => {
    const w = world();
    await toReady(w);
    w.state.groups.push(textGroup(2));
    w.state.rows.push(strangerRow(3));
    w.call(10_212, { m: 'moderateRemoveMessage', channelId: toHex(CHANNEL), msgId: toHex(w.state.rows[0].msgId!), seq: '3', reason, excerpt });
    await vi.waitFor(() => expect(w.ret(10_212)).toMatchObject({ ok: false, error: { code: 'E_BAD_INPUT' } }));
    expect(w.core.prepareManualRemovalEvidence).not.toHaveBeenCalled();
    expect(w.routes.postModerationRemoval).not.toHaveBeenCalled();
  });

  it.each(['unloaded permissions', 'denied ManageMessages'] as const)('fails closed on %s before signing or HTTP', async (label) => {
    const w = world();
    w.routes.listCommunities.mockResolvedValue([{ id: COMMUNITY, name: 'friends', owner: STRANGER, policyVersion: 1n }]);
    w.routes.listRoles.mockResolvedValue([{ id: ROLE_EVERYONE, name: '@everyone', color: 0, position: 0, allow: 1, deny: 0,
      hoist: false, mentionable: false }]);
    if (label === 'unloaded permissions') w.routes.listOverwrites.mockRejectedValue(new Error('not loaded'));
    await toReady(w);
    w.state.groups.push(textGroup(2));
    w.state.rows.push(strangerRow(3));
    if (label === 'denied ManageMessages') {
      w.call(10_220, { m: 'selectCommunity', communityId: toHex(COMMUNITY) });
      await vi.waitFor(() => expect(w.ret(10_220)).toMatchObject({ ok: true, value: null }));
    }
    w.call(10_221, { m: 'moderateRemoveMessage', channelId: toHex(CHANNEL), msgId: toHex(w.state.rows[0].msgId!), seq: '3', reason: 'reviewed by moderator', excerpt: 'bounded excerpt' });
    await vi.waitFor(() => expect(w.ret(10_221)).toMatchObject({ ok: false,
      error: { code: label === 'denied ManageMessages' ? 'E_FORBIDDEN' : 'E_NOT_READY' } }));
    expect(w.core.prepareManualRemovalEvidence).not.toHaveBeenCalled();
    expect(w.routes.postModerationRemoval).not.toHaveBeenCalled();
  });

  it('bridges self-role policy reads, configuration, and member selections', async () => {
    const w = world();
    const listSelfRoles = vi.fn(() => Promise.resolve([{ id: STRANGER, name: 'Readers', position: 2, selfSelected: false }]));
    const configureSelfRoles = vi.fn(() => Promise.resolve());
    const selectSelfRole = vi.fn(() => Promise.resolve());
    const registerRolePanel = vi.fn(() => Promise.resolve());
    Object.assign(w.routes, { listSelfRoles, configureSelfRoles, selectSelfRole, registerRolePanel });
    await toReady(w);
    const communityId = toHex(COMMUNITY);
    const roleId = toHex(STRANGER);
    const requestId = toHex(new Uint8Array(16).fill(0x55));
    w.call(990, { m: 'listSelfRoles', communityId });
    await vi.waitFor(() => expect(w.ret(990)).toMatchObject({ ok: true, value: [{ id: roleId, name: 'Readers', selfSelected: false }] }));
    expect(listSelfRoles).toHaveBeenCalledWith(COMMUNITY);
    w.call(991, { m: 'configureSelfRoles', communityId, requestId, roleIds: [roleId] });
    await vi.waitFor(() => expect(w.ret(991)).toMatchObject({ ok: true, value: null }));
    expect(configureSelfRoles).toHaveBeenCalledWith(COMMUNITY, new Uint8Array(16).fill(0x55), [STRANGER]);
    w.call(992, { m: 'selectSelfRole', communityId, roleId, requestId, enabled: true });
    await vi.waitFor(() => expect(w.ret(992)).toMatchObject({ ok: true, value: null }));
    expect(selectSelfRole).toHaveBeenCalledWith(COMMUNITY, STRANGER, new Uint8Array(16).fill(0x55), true);
    w.call(994, { m: 'registerRolePanel', communityId, requestId, channelId: toHex(CHANNEL), messageSeq: '9',
      options: [{ emoji: '✅', roleId }] });
    await vi.waitFor(() => expect(w.ret(994)).toMatchObject({ ok: true, value: null }));
    expect(registerRolePanel).toHaveBeenCalledWith(COMMUNITY, new Uint8Array(16).fill(0x55), CHANNEL, 9n,
      [{ emoji: '✅', roleId: STRANGER }]);
    w.call(995, { m: 'registerRolePanel', communityId, requestId, channelId: toHex(CHANNEL), messageSeq: '9',
      options: [{ emoji: '✅', roleId }, { emoji: '✅', roleId: toHex(USER) }] });
    await vi.waitFor(() => expect(w.ret(995)).toMatchObject({ ok: false, error: { code: 'E_BAD_INPUT' } }));
    expect(registerRolePanel).toHaveBeenCalledTimes(1);
    w.call(993, { m: 'configureSelfRoles', communityId, requestId, roleIds: [roleId, roleId] });
    await vi.waitFor(() => expect(w.ret(993)).toMatchObject({ ok: false, error: { code: 'E_BAD_INPUT' } }));
  });
  it('bridges member timeout reads and writes with decimal Unix seconds', async () => {
    const w = world();
    const getMemberTimeout = vi.fn(() => Promise.resolve(1_800_000_000n));
    const putMemberTimeout = vi.fn(() => Promise.resolve(1_800_000_000n));
    const deleteMemberTimeout = vi.fn(() => Promise.resolve());
    Object.assign(w.routes, { getMemberTimeout, putMemberTimeout, deleteMemberTimeout });
    await toReady(w);
    w.call(991, { m: 'getMemberTimeout', communityId: toHex(COMMUNITY), userId: toHex(STRANGER) });
    await vi.waitFor(() => expect(w.ret(991)).toMatchObject({ ok: true, value: '1800000000' }));
    expect(getMemberTimeout).toHaveBeenCalledWith(COMMUNITY, STRANGER);

    const requestId = new Uint8Array(16).fill(0x55);
    w.call(992, { m: 'setMemberTimeout', communityId: toHex(COMMUNITY), userId: toHex(STRANGER),
      requestId: toHex(requestId), expires: '1800000000' });
    await vi.waitFor(() => expect(w.ret(992)).toMatchObject({ ok: true, value: null }));
    expect(putMemberTimeout).toHaveBeenCalledWith(COMMUNITY, STRANGER, requestId, 1_800_000_000n);

    w.call(993, { m: 'clearMemberTimeout', communityId: toHex(COMMUNITY), userId: toHex(STRANGER), requestId: toHex(requestId) });
    await vi.waitFor(() => expect(w.ret(993)).toMatchObject({ ok: true, value: null }));
    expect(deleteMemberTimeout).toHaveBeenCalledWith(COMMUNITY, STRANGER, requestId);
  });

  it('rejects malformed timeout timestamps before an HTTP route call', async () => {
    const w = world();
    const putMemberTimeout = vi.fn();
    Object.assign(w.routes, { putMemberTimeout });
    await toReady(w);
    w.call(994, { m: 'setMemberTimeout', communityId: toHex(COMMUNITY), userId: toHex(STRANGER),
      requestId: '55'.repeat(16), expires: '01' });
    await vi.waitFor(() => expect(w.ret(994)).toMatchObject({ ok: false, error: { code: 'E_BAD_INPUT' } }));
    expect(putMemberTimeout).not.toHaveBeenCalled();
  });

  it('returns the accepted manual warning action metadata', async () => {
    const w = world();
    const requestId = new Uint8Array(16).fill(0x55);
    const actionId = new Uint8Array(16).fill(0x66);
    const warnMember = vi.fn(() => Promise.resolve({ requestId, actionId, revision: 7, acceptedAt: 1_800_000_000 }));
    Object.assign(w.routes, { warnMember });
    await toReady(w);
    const groupId = new Uint8Array(16).fill(0x52);
    w.state.groups.push({ ...textGroup(2), groupId, kind: 3, targetId: new Uint8Array(16).fill(0x53) });
    warnMember.mockImplementation(() => {
      const row = w.state.manualWarnings[0];
      if (row !== undefined) w.state.outbox.push({ msgId: row.evidenceRequestId, state: 0, error: '', created: NOW_S,
        body: 'sealed interaction', type: 0, replyTo: null, attachments: [] });
      return Promise.resolve({ requestId, actionId, revision: 7, acceptedAt: 1_800_000_000 });
    });
    w.call(996, { m: 'warnMember', communityId: toHex(COMMUNITY), userId: toHex(STRANGER), requestId: toHex(requestId), reason: 'follow the rules' });
    await vi.waitFor(() => expect(w.ret(996)).toMatchObject({ ok: true, value: {
      requestId: toHex(requestId), actionId: toHex(actionId), revision: '7', acceptedAt: 1_800_000_000, evidence: 'accepted',
    } }));
    expect(w.core.prepareManualWarningEvidence).toHaveBeenCalledWith(requestId, expect.any(Uint8Array), COMMUNITY, STRANGER, 'follow the rules', expect.any(BigInt), expect.any(BigInt));
    expect(w.core.markManualWarningAccepted).toHaveBeenCalledWith(requestId, actionId, 7n, 1_800_000_000n);
    expect(warnMember).toHaveBeenCalledWith(COMMUNITY, STRANGER, requestId);
    const requestBody = w.core.humanInteractionSendPrepare.mock.calls[0]?.[3];
    expect(requestBody).toBeInstanceOf(Uint8Array);
    if (requestBody === undefined) throw new Error('manual warning did not prepare an interaction request');
    const wire = decode(requestBody) as unknown[];
    expect(wire[7]).toEqual([requestId, 'follow the rules', '']);
  });

  it('returns full AFK settings through the authenticated settings route', async () => {
    const w = world();
    await toReady(w);
    w.routes.getAFKSettings.mockResolvedValue({ channelId: CHANNEL, timeoutSeconds: 900 });
    w.call(993, { m: 'getAFKSettings', communityId: toHex(COMMUNITY) });
    await vi.waitFor(() => expect(w.ret(993)).toEqual({ t: 'ret', id: 993, ok: true,
      value: { channelId: toHex(CHANNEL), timeoutSeconds: 900 } }));
  });
  it('loads the AFK snapshot on ready and refreshes it after a community change and reconnect', async () => {
    const w = world();
    await toReady(w);
    const key = `afk:${toHex(COMMUNITY)}` as const;
    await vi.waitFor(() => expect(w.last(key)).toMatchObject({ channelId: null, timeoutSeconds: 300, pending: false }));
    w.routes.getAFK.mockResolvedValue({ channelId: CHANNEL, timeoutSeconds: 60, pending: true, away: [] });
    w.gateway.emit({ type: 'frame', frame: { op: Op.communityChanged, n: 0n, groupId: null, payload: [COMMUNITY] } });
    await vi.waitFor(() => expect(w.last(key)).toMatchObject({ channelId: toHex(CHANNEL), timeoutSeconds: 60, pending: true }));
    w.gateway.emit({ type: 'status', status: 'waiting', closeCode: null });
    expect(w.last(key)).toBeNull();
    w.routes.getAFK.mockResolvedValue({ channelId: CHANNEL, timeoutSeconds: 60, pending: false, away: [] });
    w.gateway.emit({ type: 'status', status: 'ready', closeCode: null });
    w.gateway.emit({ type: 'ready', info: READY_INFO });
    await vi.waitFor(() => expect(w.last(key)).toMatchObject({ channelId: toHex(CHANNEL), timeoutSeconds: 60, pending: false }));
  });
  it('publishes each community_changed wakeup even when the member list itself is unchanged', async () => {
    const w = world();
    await toReady(w);
    const key = `communityChanges:${toHex(COMMUNITY)}` as const;
    expect(w.last(key) ?? 0).toBe(0);
    const changed: GatewayEvent = { type: 'frame', frame: { op: Op.communityChanged, n: 0n, groupId: null, payload: [COMMUNITY] } };
    w.gateway.emit(changed);
    await vi.waitFor(() => expect(w.last(key)).toBe(1));
    w.gateway.emit(changed);
    await vi.waitFor(() => expect(w.last(key)).toBe(2));
  });
  it('a community re-read keeps the previous AFK snapshot until the new one lands (R-AFK3 a)', async () => {
    const w = world();
    w.routes.getAFK.mockResolvedValue({ channelId: CHANNEL, timeoutSeconds: 300, pending: false, away: [] });
    await toReady(w);
    const key = `afk:${toHex(COMMUNITY)}` as const;
    await vi.waitFor(() => expect(w.last(key)).toMatchObject({ channelId: toHex(CHANNEL), pending: false }));
    const before = w.last(key);
    const nulls = () => w.posted.filter((m) => m.t === 'slice' && m.name === key && m.value === null).length;
    const nullsBefore = nulls();
    let land: (v: AFKSnapshot) => void = () => undefined;
    w.routes.getAFK.mockImplementationOnce(() => new Promise((resolve) => { land = resolve; }));
    const reads = w.routes.getAFK.mock.calls.length;
    w.gateway.emit({ type: 'frame', frame: { op: Op.communityChanged, n: 0n, groupId: null, payload: [COMMUNITY] } });
    await vi.waitFor(() => expect(w.routes.getAFK.mock.calls.length).toBe(reads + 1));
    // The read is in flight: the slice still holds the last snapshot, and was never set to null for it.
    expect(w.last(key)).toEqual(before);
    expect(nulls()).toBe(nullsBefore);
    land({ channelId: CHANNEL, timeoutSeconds: 60, pending: true, away: [] });
    await vi.waitFor(() => expect(w.last(key)).toMatchObject({ channelId: toHex(CHANNEL), timeoutSeconds: 60, pending: true }));
  });
  it('every landed AFK read gets a higher seq, also when it says the same (R-AFK4)', async () => {
    const w = world();
    w.routes.getAFK.mockResolvedValue({ channelId: CHANNEL, timeoutSeconds: 300, pending: true, away: [] });
    await toReady(w);
    const key = `afk:${toHex(COMMUNITY)}` as const;
    await vi.waitFor(() => expect(w.last<{ seq?: number }>(key)?.seq).toBeGreaterThan(0));
    const first = w.last<{ seq: number; pending: boolean }>(key)!;
    w.gateway.emit({ type: 'frame', frame: { op: Op.communityChanged, n: 0n, groupId: null, payload: [COMMUNITY] } });
    await vi.waitFor(() => expect(w.last<{ seq: number }>(key)?.seq).toBeGreaterThan(first.seq));
    expect(w.last(key)).toMatchObject({ channelId: toHex(CHANNEL), pending: true });
  });
  // AW4: op 55 afk_away keeps the away roster of a landed snapshot current.
  describe('the away roster (AW4)', () => {
    const key = `afk:${toHex(COMMUNITY)}` as const;
    const OTHER = new Uint8Array(16).fill(0x0b);
    const away = (user: Uint8Array, since: number): GatewayEvent =>
      ({ type: 'frame', frame: { op: Op.afkAway, n: 0n, groupId: null, payload: [COMMUNITY, user, BigInt(since)] } });
    type Held = { seq?: number; away?: { userId: string; since: number }[] };

    it('the snapshot carries the roster, and a web3.9 4-element snapshot has no one away', async () => {
      const w = world();
      // M2: the instance's order is not trusted; the slice is sorted by user id (0x0b before 0x5a).
      w.routes.getAFK.mockResolvedValue({ channelId: CHANNEL, timeoutSeconds: 300, pending: false,
        away: [{ userId: STRANGER, since: 100 }, { userId: OTHER, since: 120 }] });
      await toReady(w);
      await vi.waitFor(() => expect(w.last<Held>(key)?.away).toEqual([{ userId: toHex(OTHER), since: 120 }, { userId: toHex(STRANGER), since: 100 }]));
      const w9 = world();
      await toReady(w9);
      await vi.waitFor(() => expect(w9.last(key)).toMatchObject({ channelId: null, away: [] }));
    });

    it('patches the roster in place, sorted by user, and keeps seq steady', async () => {
      const w = world();
      w.routes.getAFK.mockResolvedValue({ channelId: CHANNEL, timeoutSeconds: 300, pending: true, away: [] });
      await toReady(w);
      await vi.waitFor(() => expect(w.last<Held>(key)?.seq).toBeGreaterThan(0));
      const { seq } = w.last<Held>(key)!;
      w.gateway.emit(away(STRANGER, 200));
      w.gateway.emit(away(OTHER, 150));
      expect(w.last(key)).toEqual({ channelId: toHex(CHANNEL), timeoutSeconds: 300, pending: true, pendingReason: 1, seq,
        away: [{ userId: toHex(OTHER), since: 150 }, { userId: toHex(STRANGER), since: 200 }] });
      w.gateway.emit(away(OTHER, 0));
      expect(w.last(key)).toEqual({ channelId: toHex(CHANNEL), timeoutSeconds: 300, pending: true, pendingReason: 1, seq,
        away: [{ userId: toHex(STRANGER), since: 200 }] });
      // A clear of a member who is not away, and a frame for a community this client does not list, change nothing.
      const posts = w.posted.length;
      w.gateway.emit(away(OTHER, 0));
      w.gateway.emit({ type: 'frame', frame: { op: Op.afkAway, n: 0n, groupId: null, payload: [NEW_COMMUNITY, OTHER, 5n] } });
      expect(w.posted.slice(posts).filter((m) => m.t === 'slice')).toEqual([]);
    });

    // Fix round 1, I1: an op 55 that arrives while a GET /afk is in flight is buffered and replayed onto the read's
    // roster when it lands, in order and with the stale-since rule; without a read in flight and no snapshot, it is dropped.
    const pendingRead = (w: World) => {
      let land: (v: AFKSnapshot) => void = () => undefined;
      w.routes.getAFK.mockImplementationOnce(() => new Promise((resolve) => { land = resolve; }));
      return (v: AFKSnapshot) => land(v);
    };
    const snapshot = (away: AFKSnapshot['away']): AFKSnapshot => ({ channelId: CHANNEL, timeoutSeconds: 300, pending: false, away });

    it('a set that arrives during the first read survives the read that was served before it', async () => {
      const w = world();
      const land = pendingRead(w);
      await toReady(w);
      await vi.waitFor(() => expect(w.routes.getAFK).toHaveBeenCalledTimes(1));
      w.gateway.emit(away(OTHER, 150));
      expect(w.last(key) ?? null).toBeNull();
      land(snapshot([{ userId: STRANGER, since: 100 }]));
      await vi.waitFor(() => expect(w.last<Held>(key)?.away).toEqual([{ userId: toHex(OTHER), since: 150 }, { userId: toHex(STRANGER), since: 100 }]));
    });

    it('a clear that arrives during the first read removes the member the read still lists', async () => {
      const w = world();
      const land = pendingRead(w);
      await toReady(w);
      await vi.waitFor(() => expect(w.routes.getAFK).toHaveBeenCalledTimes(1));
      w.gateway.emit(away(STRANGER, 0));
      land(snapshot([{ userId: STRANGER, since: 100 }]));
      await vi.waitFor(() => expect(w.last<Held>(key)?.away).toEqual([]));
    });

    it('during a re-read, a set and a clear patch the held roster and are replayed onto the read in order', async () => {
      const w = world();
      w.routes.getAFK.mockResolvedValueOnce(snapshot([{ userId: STRANGER, since: 100 }]));
      await toReady(w);
      await vi.waitFor(() => expect(w.last<Held>(key)?.away).toEqual([{ userId: toHex(STRANGER), since: 100 }]));
      const land = pendingRead(w);
      const reads = w.routes.getAFK.mock.calls.length;
      w.gateway.emit({ type: 'frame', frame: { op: Op.communityChanged, n: 0n, groupId: null, payload: [COMMUNITY] } });
      await vi.waitFor(() => expect(w.routes.getAFK.mock.calls.length).toBe(reads + 1));
      const { seq } = w.last<Held>(key)!;
      w.gateway.emit(away(OTHER, 150));
      w.gateway.emit(away(STRANGER, 0));
      w.gateway.emit(away(OTHER, 140)); // stale against the buffered 150: ignored on replay too
      expect(w.last(key)).toMatchObject({ seq, away: [{ userId: toHex(OTHER), since: 150 }] });
      // The re-read was served before both changes: it still lists STRANGER and not OTHER.
      land(snapshot([{ userId: STRANGER, since: 100 }]));
      await vi.waitFor(() => expect(w.last<Held>(key)?.seq).toBeGreaterThan(seq!));
      expect(w.last<Held>(key)?.away).toEqual([{ userId: toHex(OTHER), since: 150 }]);
    });

    it('frames buffered for a read that a newer read superseded, or that offline discarded, are not replayed', async () => {
      const w = world();
      const land = pendingRead(w);
      await toReady(w);
      await vi.waitFor(() => expect(w.routes.getAFK).toHaveBeenCalledTimes(1));
      w.gateway.emit(away(OTHER, 150));
      // Offline discards the read and its buffer; the reconnect's read starts while the first is still in flight.
      w.gateway.emit({ type: 'status', status: 'waiting', closeCode: null });
      const land2 = pendingRead(w);
      w.gateway.emit({ type: 'status', status: 'ready', closeCode: null });
      w.gateway.emit({ type: 'ready', info: READY_INFO });
      await vi.waitFor(() => expect(w.routes.getAFK).toHaveBeenCalledTimes(2));
      land(snapshot([]));
      land2(snapshot([]));
      await vi.waitFor(() => expect(w.last(key)).toMatchObject({ away: [] }));
    });

    it('a set older than the held since is ignored; a later one replaces it', async () => {
      const w = world();
      w.routes.getAFK.mockResolvedValue({ channelId: CHANNEL, timeoutSeconds: 300, pending: false, away: [{ userId: STRANGER, since: 200 }] });
      await toReady(w);
      await vi.waitFor(() => expect(w.last<Held>(key)?.away).toEqual([{ userId: toHex(STRANGER), since: 200 }]));
      const posts = w.posted.length;
      w.gateway.emit(away(STRANGER, 150));
      w.gateway.emit(away(STRANGER, 200));
      expect(w.posted.slice(posts).filter((m) => m.t === 'slice')).toEqual([]);
      w.gateway.emit(away(STRANGER, 300));
      expect(w.last<Held>(key)?.away).toEqual([{ userId: toHex(STRANGER), since: 300 }]);
    });

    it('going offline clears the roster with the snapshot, and an op 55 while it is null is dropped', async () => {
      const w = world();
      w.routes.getAFK.mockResolvedValue({ channelId: CHANNEL, timeoutSeconds: 300, pending: false, away: [{ userId: STRANGER, since: 200 }] });
      await toReady(w);
      await vi.waitFor(() => expect(w.last<Held>(key)?.away).toHaveLength(1));
      w.gateway.emit({ type: 'status', status: 'waiting', closeCode: null });
      expect(w.last(key)).toBeNull();
      w.gateway.emit(away(OTHER, 150));
      expect(w.last(key)).toBeNull();
    });

    it('afkAway sends PUT …/afk/away for a valid community and refuses a bad id', async () => {
      const w = world();
      await toReady(w);
      w.call(998, { m: 'afkAway', communityId: toHex(COMMUNITY) });
      await vi.waitFor(() => expect(w.ret(998)).toMatchObject({ ok: true, value: null }));
      expect(w.routes.putAFKAway).toHaveBeenCalledWith(COMMUNITY);
      w.call(999, { m: 'afkAway', communityId: 'nope' });
      await vi.waitFor(() => expect(w.ret(999)).toMatchObject({ ok: false, error: { code: 'E_BAD_INPUT' } }));
      expect(w.routes.putAFKAway).toHaveBeenCalledTimes(1);
    });

    // AW5 fix round 3: the slice carries the read's pending reason (0 none, 1 the sweep, 2 AFK chosen elsewhere).
    it('a read lands its pending reason in the slice', async () => {
      const w = world();
      await toReady(w);
      const key = `afk:${toHex(COMMUNITY)}` as const;
      w.routes.getAFK.mockResolvedValueOnce({ channelId: CHANNEL, timeoutSeconds: 60, pending: true, reason: 2, away: [] });
      w.call(1040, { m: 'afkRead', communityId: toHex(COMMUNITY) });
      await vi.waitFor(() => expect(w.ret(1040)).toMatchObject({ ok: true }));
      expect(w.last(key)).toMatchObject({ pending: true, pendingReason: 2 });
      w.routes.getAFK.mockResolvedValueOnce({ channelId: CHANNEL, timeoutSeconds: 60, pending: true, away: [] });
      w.call(1041, { m: 'afkRead', communityId: toHex(COMMUNITY) });
      await vi.waitFor(() => expect(w.ret(1041)).toMatchObject({ ok: true }));
      expect(w.last(key)).toMatchObject({ pending: true, pendingReason: 1 });
      w.routes.getAFK.mockResolvedValueOnce({ channelId: CHANNEL, timeoutSeconds: 60, pending: false, reason: 0, away: [] });
      w.call(1042, { m: 'afkRead', communityId: toHex(COMMUNITY) });
      await vi.waitFor(() => expect(w.ret(1042)).toMatchObject({ ok: true }));
      expect(w.last(key)).toMatchObject({ pending: false, pendingReason: 0 });
    });

    // AW5 fix round 2 (B-M1): a newer read of the same community that replaces this one's is waited for, not a failure.
    it('afkRead answers with a newer read that superseded its own', async () => {
      const w = world();
      await toReady(w);
      const key = `afk:${toHex(COMMUNITY)}` as const;
      await vi.waitFor(() => expect(w.last(key)).not.toBeUndefined());
      const reads = w.routes.getAFK.mock.calls.length;
      let land: (v: Awaited<ReturnType<typeof w.routes.getAFK>>) => void = () => undefined;
      w.routes.getAFK.mockImplementationOnce(() => new Promise((resolve) => { land = resolve; }));
      w.call(1020, { m: 'afkRead', communityId: toHex(COMMUNITY) });
      await vi.waitFor(() => expect(w.routes.getAFK).toHaveBeenCalledTimes(reads + 1));
      // A second read (a reload, setAFK or another afkRead) starts while the first is in flight. The first lands first
      // and is discarded as superseded; the first afkRead waits for the second, which lands later.
      let land2: (v: Awaited<ReturnType<typeof w.routes.getAFK>>) => void = () => undefined;
      w.routes.getAFK.mockImplementationOnce(() => new Promise((resolve) => { land2 = resolve; }));
      w.call(1021, { m: 'afkRead', communityId: toHex(COMMUNITY) });
      await vi.waitFor(() => expect(w.routes.getAFK).toHaveBeenCalledTimes(reads + 2));
      land({ channelId: CHANNEL, timeoutSeconds: 60, pending: false, away: [] });
      await new Promise((r) => setTimeout(r, 50));
      expect(w.ret(1020)).toBeUndefined();
      land2({ channelId: CHANNEL, timeoutSeconds: 60, pending: true, away: [] });
      await vi.waitFor(() => expect(w.ret(1020)).toMatchObject({ ok: true, value: null }));
      await vi.waitFor(() => expect(w.ret(1021)).toMatchObject({ ok: true, value: null }));
      expect(w.last(key)).toMatchObject({ pending: true });
    });

    // AW5 fix round 2 (nit): the worker's own bound matches the page's wait.
    it('afkRead answers E_UNAVAILABLE once its timeoutMs passes with no read landed', async () => {
      const w = world();
      await toReady(w);
      w.routes.getAFK.mockImplementationOnce(() => new Promise(() => undefined));
      w.call(1030, { m: 'afkRead', communityId: toHex(COMMUNITY), timeoutMs: 50 });
      await vi.waitFor(() => expect(w.ret(1030)).toMatchObject({ ok: false, error: { code: 'E_UNAVAILABLE' } }));
    });

    // AW5 fix round 1 (I1): the page's restore and recovery ask for a fresh read and act only once it landed.
    it('afkRead answers once a fresh read landed, and refuses when the read fails or the community is unlisted', async () => {
      const w = world();
      await toReady(w);
      const key = `afk:${toHex(COMMUNITY)}` as const;
      w.routes.getAFK.mockResolvedValueOnce({ channelId: CHANNEL, timeoutSeconds: 60, pending: true, away: [] });
      const reads = w.routes.getAFK.mock.calls.length;
      w.call(1010, { m: 'afkRead', communityId: toHex(COMMUNITY) });
      await vi.waitFor(() => expect(w.ret(1010)).toMatchObject({ ok: true, value: null }));
      expect(w.routes.getAFK.mock.calls.length).toBe(reads + 1);
      expect(w.last(key)).toMatchObject({ pending: true });
      w.routes.getAFK.mockRejectedValueOnce(new DillaHttpError({ status: 503, code: 'E_UNAVAILABLE', detail: '', retryAfterMs: null, extra: [] }));
      w.call(1011, { m: 'afkRead', communityId: toHex(COMMUNITY) });
      await vi.waitFor(() => expect(w.ret(1011)).toMatchObject({ ok: false, error: { code: 'E_UNAVAILABLE' } }));
      w.call(1012, { m: 'afkRead', communityId: toHex(NEW_COMMUNITY) });
      await vi.waitFor(() => expect(w.ret(1012)).toMatchObject({ ok: false, error: { code: 'E_BAD_INPUT' } }));
    });

    // Fix round 1, M1: an unlisted community is refused locally, as setAFK refuses it.
    it('afkAway refuses a community this client does not list, without a request', async () => {
      const w = world();
      await toReady(w);
      w.call(1001, { m: 'afkAway', communityId: toHex(NEW_COMMUNITY) });
      await vi.waitFor(() => expect(w.ret(1001)).toMatchObject({ ok: false, error: { code: 'E_BAD_INPUT' } }));
      expect(w.routes.putAFKAway).not.toHaveBeenCalled();
    });

    // Fix round 1, M1: a refused PUT reaches the page with its status and code, never as success.
    it('afkAway passes a 403 or a 404 from the instance to the page', async () => {
      const w = world();
      await toReady(w);
      for (const [n, status, code] of [[1002, 403, 'E_FORBIDDEN'], [1003, 404, 'E_NOT_FOUND']] as const) {
        w.routes.putAFKAway.mockRejectedValueOnce(new DillaHttpError({ status, code, detail: 'secret', retryAfterMs: null, extra: [] }));
        w.call(n, { m: 'afkAway', communityId: toHex(COMMUNITY) });
        await vi.waitFor(() => expect(w.ret(n)).toMatchObject({ ok: false, error: { status, code, detail: '' } }));
      }
    });
  });
  it('a held snapshot with no AFK pointer is unknown before a re-read publishes the channels (fix round 3, N-3)', async () => {
    const w = world();
    const AFK = new Uint8Array(16).fill(0x61);
    await toReady(w);
    const key = `afk:${toHex(COMMUNITY)}` as const;
    const channelsKey = `channels:${toHex(COMMUNITY)}`;
    await vi.waitFor(() => expect(w.last(key)).toMatchObject({ channelId: null }));
    // A later ensure made the built-in AFK channel; the op 54 re-read lists it before its AFK read lands.
    w.routes.listChannels.mockResolvedValue([
      { id: CHANNEL, kind: 0, mode: 0, visibility: 0, parentId: null, name: 'general', topic: '', position: 0, slowmode: 0, seq: 1n, textGroupId: GROUP },
      { id: AFK, kind: 1, mode: 0, visibility: 0, parentId: null, name: 'AFK', topic: '', position: 2 ** 30, slowmode: 0, seq: 0n, textGroupId: null },
    ]);
    let land: (v: AFKSnapshot) => void = () => undefined;
    w.routes.getAFK.mockImplementationOnce(() => new Promise((resolve) => { land = resolve; }));
    w.gateway.emit({ type: 'frame', frame: { op: Op.communityChanged, n: 0n, groupId: null, payload: [COMMUNITY] } });
    await vi.waitFor(() => expect(w.channels()?.some((c) => c.id === toHex(AFK))).toBe(true));
    // Whenever the list naming AFK was published, the snapshot the page held for the community was null (unknown).
    const listedAt = w.posted.findIndex((m) => m.t === 'slice' && m.name === channelsKey
      && (m.value as ChannelSummary[]).some((c) => c.id === toHex(AFK)));
    const afkBefore = w.posted.slice(0, listedAt).filter((m): m is SliceMessage => m.t === 'slice' && m.name === key).at(-1);
    expect(afkBefore?.value).toBeNull();
    land({ channelId: AFK, timeoutSeconds: 300, pending: false, away: [] });
    await vi.waitFor(() => expect(w.last(key)).toMatchObject({ channelId: toHex(AFK) }));
  });
  it('a failed AFK read retries at 1 s, 2 s, 4 s … until it lands (R-AFK3 b)', async () => {
    const w = world();
    w.routes.getAFK.mockRejectedValueOnce(new Error('E_NETWORK')).mockRejectedValueOnce(new Error('E_NETWORK'))
      .mockRejectedValueOnce(new Error('E_NETWORK'));
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'], shouldAdvanceTime: true });
    try {
      await toReady(w);
      const key = `afk:${toHex(COMMUNITY)}` as const;
      await vi.waitFor(() => expect(w.routes.getAFK).toHaveBeenCalledTimes(1));
      await vi.advanceTimersByTimeAsync(10);
      expect(w.last(key) ?? null).toBeNull();
      const calls = () => w.routes.getAFK.mock.calls.length;
      await vi.advanceTimersByTimeAsync(1_000);
      expect(calls()).toBe(2);
      await vi.advanceTimersByTimeAsync(2_000);
      expect(calls()).toBe(3);
      await vi.advanceTimersByTimeAsync(4_000);
      expect(calls()).toBe(4);
      await vi.waitFor(() => expect(w.last(key)).toMatchObject({ channelId: null, timeoutSeconds: 300, pending: false }));
      // Landed: no more reads.
      await vi.advanceTimersByTimeAsync(120_000);
      expect(calls()).toBe(4);
    } finally { vi.useRealTimers(); }
  });
  it('the AFK retry waits at most 30 s, and stops when the community is unloaded (R-AFK3 b)', async () => {
    const w = world();
    w.routes.getAFK.mockRejectedValue(new Error('E_NETWORK'));
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'], shouldAdvanceTime: true });
    try {
      await toReady(w);
      await vi.waitFor(() => expect(w.routes.getAFK).toHaveBeenCalledTimes(1));
      const calls = () => w.routes.getAFK.mock.calls.length;
      // 1 + 2 + 4 + 8 + 16 s, then 30 s steps (never 32 s).
      await vi.advanceTimersByTimeAsync(31_100);
      expect(calls()).toBe(6);
      await vi.advanceTimersByTimeAsync(29_000);
      expect(calls()).toBe(6);
      await vi.advanceTimersByTimeAsync(1_100);
      expect(calls()).toBe(7);
      w.call(31, { m: 'leaveCommunity', communityId: toHex(COMMUNITY) });
      await vi.waitFor(() => expect(w.ret(31)).toMatchObject({ ok: true }));
      const reads = calls();
      await vi.advanceTimersByTimeAsync(300_000);
      expect(calls()).toBe(reads);
    } finally { vi.useRealTimers(); }
  });
  it('setAFK sends only the timeout, reloads the slice, and refuses 120 with E_BAD_INPUT', async () => {
    const w = world();
    await toReady(w);
    w.call(994, { m: 'setAFK', communityId: toHex(COMMUNITY), timeoutSeconds: 900 });
    await vi.waitFor(() => expect(w.ret(994)).toMatchObject({ ok: true }));
    expect(w.routes.setAFKSettings).toHaveBeenCalledWith(COMMUNITY, 900);
    w.routes.setAFKSettings.mockClear();
    w.call(995, { m: 'setAFK', communityId: toHex(COMMUNITY), timeoutSeconds: 120 });
    await vi.waitFor(() => expect(w.ret(995)).toMatchObject({ ok: false, error: { code: 'E_BAD_INPUT' } }));
    expect(w.routes.setAFKSettings).not.toHaveBeenCalled();
  });
  it('afkActivity forwards exempt', async () => {
    const w = world();
    await toReady(w);
    w.call(996, { m: 'afkActivity', communityId: toHex(COMMUNITY), channelId: toHex(CHANNEL) });
    await vi.waitFor(() => expect(w.ret(996)).toMatchObject({ ok: true }));
    expect(w.routes.sendAFKActivity).toHaveBeenLastCalledWith(COMMUNITY, CHANNEL, undefined);
    w.call(997, { m: 'afkActivity', communityId: toHex(COMMUNITY), channelId: toHex(CHANNEL), exempt: true });
    await vi.waitFor(() => expect(w.ret(997)).toMatchObject({ ok: true }));
    expect(w.routes.sendAFKActivity).toHaveBeenLastCalledWith(COMMUNITY, CHANNEL, true);
  });

  it('loads a presence snapshot on ready, applies frames, and reloads after reconnect', async () => {
    const w = world();
    await toReady(w);
    w.gateway.emit({ type: 'status', status: 'ready', closeCode: null });
    w.gateway.emit({ type: 'ready', info: READY_INFO });
    await vi.waitFor(() => expect(w.last(`presence:${toHex(COMMUNITY)}`)).toEqual({ [toHex(USER)]: 'online' }));
    w.gateway.emit({ type: 'frame', frame: { op: Op.presence, n: 0n, groupId: null,
      payload: [USER, 2n, 1n, ''] } });
    expect(w.last(`presence:${toHex(COMMUNITY)}`)).toEqual({ [toHex(USER)]: 'idle' });
    w.gateway.emit({ type: 'status', status: 'waiting', closeCode: null });
    expect(w.last(`presence:${toHex(COMMUNITY)}`)).toBeNull();
    w.routes.getPresenceSnapshot.mockResolvedValue([{ userId: USER, status: 3 }]);
    w.gateway.emit({ type: 'status', status: 'ready', closeCode: null });
    w.gateway.emit({ type: 'ready', info: READY_INFO });
    await vi.waitFor(() => expect(w.last(`presence:${toHex(COMMUNITY)}`)).toEqual({ [toHex(USER)]: 'dnd' }));
  });
  it('saves a status choice and re-reads one changed on another device', async () => {
    const w = world();
    await toReady(w);
    w.call(991, { m: 'setPresenceChoice', choice: 'invisible' });
    await vi.waitFor(() => expect(w.ret(991)).toMatchObject({ ok: true }));
    expect(w.routes.setPresenceChoice).toHaveBeenCalledWith(4);
    expect(w.account()?.presenceChoice).toBe('invisible');
    w.routes.getAccountMe.mockResolvedValue({ userId: USER, username: 'web', display: 'Web', kind: 0, flags: 0n, presenceChoice: 3 });
    w.gateway.emit({ type: 'frame', frame: { op: Op.presence, n: 0n, groupId: null, payload: [USER, 3n, 1n, ''] } });
    await vi.waitFor(() => expect(w.account()?.presenceChoice).toBe('dnd'));
  });
  it('updateProfile sends only the changed fields and shows the saved (trimmed) profile on the account and the own member row, without reading the lists again', async () => {
    const w = world();
    await toReady(w);
    await vi.waitFor(() => expect(w.last<MemberSummary[]>(`members:${toHex(COMMUNITY)}`)?.length).toBeGreaterThan(0));
    const listReads = w.routes.listMembers.mock.calls.length;
    const meReads = w.routes.getAccountMe.mock.calls.length;
    w.call(1001, { m: 'updateProfile', display: 'Ada', statusMsg: ' on a call ', bio: 'hi' });
    await vi.waitFor(() => expect(w.ret(1001)).toMatchObject({ ok: true }));
    expect(w.routes.patchMe).toHaveBeenCalledWith({ display: 'Ada', statusMsg: 'on a call', bio: 'hi' });
    expect(w.account()?.user).toMatchObject({ display: 'Ada', statusMsg: 'on a call', bio: 'hi' });
    expect(w.last<MemberSummary[]>(`members:${toHex(COMMUNITY)}`)?.find((m) => m.userId === toHex(USER))).toMatchObject({ display: 'Ada', statusMsg: 'on a call', bio: 'hi' });
    expect(w.routes.listMembers.mock.calls.length).toBe(listReads);
    expect(w.routes.getAccountMe.mock.calls.length).toBe(meReads);
    // Clearing a field drops it from the slices.
    w.call(1005, { m: 'updateProfile', statusMsg: '' });
    await vi.waitFor(() => expect(w.ret(1005)).toMatchObject({ ok: true }));
    expect(w.account()?.user?.statusMsg).toBeUndefined();
    expect(w.account()?.user?.bio).toBe('hi');
  });
  it('an account read that began before a save does not undo it', async () => {
    const w = world();
    await toReady(w);
    const stale = deferred<{ userId: Uint8Array; username: string; display: string; kind: number; flags: bigint; presenceChoice: number }>();
    w.routes.getAccountMe.mockReturnValueOnce(stale.promise);
    const refresh = (w.controller as unknown as { refreshPresenceChoice(): Promise<void> }).refreshPresenceChoice();
    w.call(1010, { m: 'updateProfile', display: 'Ada', statusMsg: 'saved status' });
    await vi.waitFor(() => expect(w.ret(1010)).toMatchObject({ ok: true }));
    stale.resolve({ userId: USER, username: 'web', display: 'Old name', kind: 0, flags: 0n, presenceChoice: 3 });
    await refresh;
    expect(w.account()?.user).toMatchObject({ display: 'Ada', statusMsg: 'saved status' });
    // the read still brings what it is for: the presence choice
    expect(w.account()?.presenceChoice).toBe('dnd');
  });
  it('a member list read that began before a save does not undo it on the own row', async () => {
    const w = world();
    await toReady(w);
    await vi.waitFor(() => expect(w.last<MemberSummary[]>(`members:${toHex(COMMUNITY)}`)?.length).toBeGreaterThan(0));
    const stale = deferred<{ userId: Uint8Array; username: string; display: string; kind: 0; nick: string; roleIds: Uint8Array[] }[]>();
    w.routes.listMembers.mockReturnValueOnce(stale.promise);
    const reload = (w.controller as unknown as { reloadMembers(id: string): Promise<void> }).reloadMembers(toHex(COMMUNITY));
    w.call(1011, { m: 'updateProfile', display: 'Ada', bio: 'saved bio' });
    await vi.waitFor(() => expect(w.ret(1011)).toMatchObject({ ok: true }));
    stale.resolve([{ userId: USER, username: 'web', display: 'Old name', kind: 0, nick: '', roleIds: [] }]);
    await reload;
    expect(w.last<MemberSummary[]>(`members:${toHex(COMMUNITY)}`)?.find((m) => m.userId === toHex(USER))).toMatchObject({ display: 'Ada', bio: 'saved bio' });
  });
  it('updateProfile with nothing to change makes no request, and a field of the wrong type is E_BAD_INPUT', async () => {
    const w = world();
    await toReady(w);
    w.call(1002, { m: 'updateProfile' });
    await vi.waitFor(() => expect(w.ret(1002)).toMatchObject({ ok: true }));
    w.call(1003, { m: 'updateProfile', bio: 5 as unknown as string });
    await vi.waitFor(() => expect(w.ret(1003)).toMatchObject({ ok: false, error: { code: 'E_BAD_INPUT' } }));
    expect(w.routes.patchMe).not.toHaveBeenCalled();
  });
  it('updateProfile reports a refusal and leaves the account as it was', async () => {
    const w = world();
    await toReady(w);
    w.routes.patchMe.mockRejectedValueOnce(new Error('refused'));
    w.call(1004, { m: 'updateProfile', statusMsg: 'x' });
    await vi.waitFor(() => expect(w.ret(1004)).toMatchObject({ ok: false }));
    expect(w.account()?.user?.statusMsg).toBeUndefined();
  });
  it('carries the account display name on the user, for a person who has joined no server yet', async () => {
    const w = world();
    w.routes.getAccountMe.mockResolvedValue({ userId: USER, username: 'web', display: 'Alice', kind: 0, flags: 0n, presenceChoice: 1 });
    await toReady(w);
    w.gateway.emit({ type: 'status', status: 'ready', closeCode: null });
    w.gateway.emit({ type: 'ready', info: READY_INFO });
    await vi.waitFor(() => expect(w.account()?.user?.display).toBe('Alice'));
    expect(w.account()?.user?.username).toBe('web');
  });
  describe('the cached display name never crosses accounts (security fix)', () => {
    type Cache = { knownDisplay(userId: string): { display?: string } };
    const cache = (w: ReturnType<typeof world>) => w.controller as unknown as Cache;
    const B = 'b'.repeat(32);
    async function readyAs(display: string) {
      const w = world();
      w.routes.getAccountMe.mockResolvedValue({ userId: USER, username: 'web', display, kind: 0, flags: 0n, presenceChoice: 1 });
      await toReady(w);
      w.gateway.emit({ type: 'status', status: 'ready', closeCode: null });
      w.gateway.emit({ type: 'ready', info: READY_INFO });
      await vi.waitFor(() => expect(w.account()?.user?.display).toBe(display));
      return w;
    }
    it('is returned only for the user it was read for', async () => {
      const w = await readyAs('Alice');
      expect(cache(w).knownDisplay(toHex(USER))).toEqual({ display: 'Alice' });
      expect(cache(w).knownDisplay(B)).toEqual({});
    });
    it('is dropped by a sign-out wipe, so the next account never sees it, not even for the same id', async () => {
      const w = await readyAs('Alice');
      w.call(2, { m: 'signOutRevoke', recoveryKey: RECOVERY_KEY });
      await vi.waitFor(() => expect(w.ret(2)).toEqual({ t: 'ret', id: 2, ok: true, value: null }));
      expect(w.account()?.user).toBeNull();
      expect(cache(w).knownDisplay(toHex(USER))).toEqual({});
      expect(cache(w).knownDisplay(B)).toEqual({});
    });
    it('is dropped when the account state is cleared by a lost lock (dropStore)', async () => {
      const w = await readyAs('Alice');
      (w.controller as unknown as { dropStore(): void }).dropStore();
      await vi.waitFor(() => expect(w.account()?.user).toBeNull());
      expect(cache(w).knownDisplay(toHex(USER))).toEqual({});
    });
    it('the setAccount clear: any state that sets user null, or revokes, drops the cache (m-d, kept as defence in depth)', async () => {
      const set = (w: ReturnType<typeof world>, patch: object) => (w.controller as unknown as { setAccount(p: object): void }).setAccount(patch);
      const a = await readyAs('Alice');
      set(a, { user: null });
      expect(cache(a).knownDisplay(toHex(USER))).toEqual({});
      const b = await readyAs('Alice');
      set(b, { phase: 'revoked' });
      expect(cache(b).knownDisplay(toHex(USER))).toEqual({});
      const c = await readyAs('Alice');
      set(c, { phase: 'loading' });
      expect(cache(c).knownDisplay(toHex(USER))).toEqual({ display: 'Alice' });
    });
    it('the status choice is reset with the account (wipe and dropStore), so the next account never sees the last one\'s status (m-a)', async () => {
      const w = world();
      w.routes.getAccountMe.mockResolvedValue({ userId: USER, username: 'web', display: 'Web', kind: 0, flags: 0n, presenceChoice: 3 });
      await toReady(w);
      w.gateway.emit({ type: 'status', status: 'ready', closeCode: null });
      w.gateway.emit({ type: 'ready', info: READY_INFO });
      await vi.waitFor(() => expect(w.account()?.presenceChoice).toBe('dnd'));
      (w.controller as unknown as { dropStore(): void }).dropStore();
      expect(w.account()?.presenceChoice).toBeUndefined();
      const x = world();
      x.routes.getAccountMe.mockResolvedValue({ userId: USER, username: 'web', display: 'Web', kind: 0, flags: 0n, presenceChoice: 3 });
      await toReady(x);
      x.gateway.emit({ type: 'status', status: 'ready', closeCode: null });
      x.gateway.emit({ type: 'ready', info: READY_INFO });
      await vi.waitFor(() => expect(x.account()?.presenceChoice).toBe('dnd'));
      x.call(2, { m: 'signOutRevoke', recoveryKey: RECOVERY_KEY });
      await vi.waitFor(() => expect(x.ret(2)).toEqual({ t: 'ret', id: 2, ok: true, value: null }));
      expect(x.account()?.presenceChoice).toBeUndefined();
    });
    it('a read that names another user is not applied to the current one', async () => {
      const w = world();
      w.routes.getAccountMe.mockResolvedValue({ userId: new Uint8Array(16).fill(0xbb), username: 'other', display: 'Mallory', kind: 0, flags: 0n, presenceChoice: 1 });
      await toReady(w);
      w.gateway.emit({ type: 'status', status: 'ready', closeCode: null });
      w.gateway.emit({ type: 'ready', info: READY_INFO });
      await vi.waitFor(() => expect(w.routes.getAccountMe).toHaveBeenCalled());
      await new Promise(r => setTimeout(r, 20));
      expect(w.account()?.user?.display).toBeUndefined();
      expect(cache(w).knownDisplay(toHex(USER))).toEqual({});
    });
  });
  it('keeps a successful choice save when the own frame refresh fails', async () => {
    const w = world();
    await toReady(w);
    w.routes.getAccountMe.mockRejectedValueOnce(new Error('temporarily unavailable'));
    w.routes.setPresenceChoice.mockImplementation(async () => {
      w.gateway.emit({ type: 'frame', frame: { op: Op.presence, n: 0n, groupId: null, payload: [USER, 3n, 1n, ''] } });
      await Promise.resolve();
    });
    w.call(992, { m: 'setPresenceChoice', choice: 'dnd' });
    await vi.waitFor(() => expect(w.ret(992)).toMatchObject({ ok: true }));
    expect(w.account()?.presenceChoice).toBe('dnd');
  });
  it('buffers live updates during one in-flight snapshot', async () => {
    const w = world();
    await toReady(w);
    let resolveSnapshot!: (rows: { userId: typeof USER; status: number }[]) => void;
    w.routes.getPresenceSnapshot.mockImplementation(() => new Promise(resolve => { resolveSnapshot = resolve; }));
    w.routes.getPresenceSnapshot.mockClear();
    w.gateway.emit({ type: 'status', status: 'ready', closeCode: null });
    w.gateway.emit({ type: 'ready', info: READY_INFO });
    await vi.waitFor(() => expect(w.routes.getPresenceSnapshot).toHaveBeenCalledTimes(1));
    for (const status of [2n, 1n, 3n]) w.gateway.emit({ type: 'frame', frame: { op: Op.presence, n: 0n, groupId: null,
      payload: [USER, status, 1n, ''] } });
    expect(w.routes.getPresenceSnapshot).toHaveBeenCalledTimes(1);
    resolveSnapshot([{ userId: USER, status: 1 }]);
    await vi.waitFor(() => expect(w.last(`presence:${toHex(COMMUNITY)}`)).toEqual({ [toHex(USER)]: 'dnd' }));
  });
  it('start reads the instance, waits as other-tab while another tab holds the lock, and boots when it is released', async () => {
    const h = harness({ boot: () => Promise.resolve({ kind: 'unsupported', reason: 'no OPFS in this test' }) });
    const release = h.locks.holdElsewhere(LOCK);
    h.call(1, { m: 'start' });
    // Pre-flight ruling 4: other-tab is published only once the lock is still held LEADER.otherTabGraceMs
    // after the failed attempt, so this wait outlasts the grace (vi.waitFor's default is 1000 ms).
    await vi.waitFor(() => expect(h.account()?.phase).toBe('other-tab'), { timeout: LEADER.otherTabGraceMs + 3_000 });
    expect(h.ret(1)).toEqual({ t: 'ret', id: 1, ok: true, value: null });
    expect(h.account()?.instance).toEqual({ id: INSTANCE_HEX, name: 'dilla.test', registrationMode: 0, passwordSignup: true, totp: false,
      passkeys: false, oidc: false });
    expect(h.connection()).toEqual({ status: 'offline', generation: null });
    release();
    await vi.waitFor(() => expect(h.account()?.phase).toBe('unsupported'));
    expect(h.account()?.error).toEqual({ code: 'E_UNSUPPORTED', detail: '', status: 0, retryAfterMs: null });
    expect(h.locks.requests).toEqual([{ name: LOCK, ifAvailable: true }, { name: LOCK, ifAvailable: false }]);
  });

  it('start is idempotent', async () => {
    const h = harness();
    h.call(1, { m: 'start' });
    h.call(2, { m: 'start' });
    await vi.waitFor(() => expect(h.account()?.phase).toBe('unsupported'));
    expect(h.ret(1)).toEqual({ t: 'ret', id: 1, ok: true, value: null });
    expect(h.ret(2)).toEqual({ t: 'ret', id: 2, ok: true, value: null });
    expect(h.paths.filter((p) => p === '/v1/instance')).toHaveLength(1);
    expect(h.locks.requests).toEqual([{ name: LOCK, ifAvailable: true }]);
  });

  it('a failed instance read is the start error and the account error', async () => {
    // eslint-disable-next-line @typescript-eslint/no-unnecessary-type-assertion -- the brief's double, kept as written
    const h = harness({ fetch: (() => Promise.resolve(new Response('proxy says no', { status: 502 }))) as typeof fetch });
    h.call(1, { m: 'start' });
    await vi.waitFor(() => expect(h.ret(1)).toBeDefined());
    expect(h.ret(1)).toEqual({ t: 'ret', id: 1, ok: false, error: { code: 'E_HTTP', detail: '', status: 502, retryAfterMs: null } });
    expect(h.account()).toMatchObject({ phase: 'error', error: { code: 'E_HTTP', detail: '', status: 502, retryAfterMs: null } });
    expect(h.locks.requests).toEqual([]);
  });

  it('a store written by a newer client is E_STORE_NEWER with both schema numbers (C4x)', async () => {
    // core_open rejects with a plain Error carrying the core's ClientError display (dilla-core-wasm facade.rs).
    const h = harness({ boot: () => Promise.reject(new Error('E_STORE_NEWER: found 5, supported 4')) });
    h.call(1, { m: 'start' });
    await vi.waitFor(() => expect(h.account()?.phase).toBe('error'));
    expect(h.account()?.error).toEqual({ code: 'E_STORE_NEWER', detail: '', status: 0, retryAfterMs: null, store: { found: 5, supported: 4 } });
  });

  it('a store without an identity asks for signup; signupBegin shows the key and signupReset withdraws it', async () => {
    const { core, resets } = stubCore();
    const h = harness({ boot: opened(core) });
    h.call(1, { m: 'start' });
    await vi.waitFor(() => expect(h.account()?.phase).toBe('needs-signup'));
    expect(h.account()?.recoveryKey).toBeNull();
    h.call(2, { m: 'signupBegin' });
    await vi.waitFor(() => expect(h.ret(2)).toEqual({ t: 'ret', id: 2, ok: true, value: null }));
    expect(h.account()).toMatchObject({ phase: 'signup-keys', recoveryKey: Array.from({ length: 13 }, () => 'ABCD'), error: null });
    h.call(3, { m: 'signupReset' });
    await vi.waitFor(() => expect(h.ret(3)).toEqual({ t: 'ret', id: 3, ok: true, value: null }));
    expect(h.account()).toMatchObject({ phase: 'needs-signup', recoveryKey: null });
    expect(resets()).toBe(1);
  });

  it('refuses a command its phase does not accept', async () => {
    const { core } = stubCore();
    const h = harness({ boot: opened(core) });
    h.call(1, { m: 'send', channelId: 'a'.repeat(32), text: 'too early' });
    h.call(2, { m: 'start' });
    await vi.waitFor(() => expect(h.account()?.phase).toBe('needs-signup'));
    h.call(3, { m: 'joinCommunity', invite: 'code' });
    h.call(4, { m: 'openChannel', channelId: 'a'.repeat(32) });
    h.call(5, { m: 'signupSubmit', invite: 'code', username: 'web', display: 'Web', password: null, recoveryKeyAcknowledged: true });
    h.call(6, { m: 'resetDevice' });
    await vi.waitFor(() => expect(h.ret(6)).toBeDefined());
    for (const id of [1, 3, 4, 5, 6]) {
      expect(h.ret(id)).toMatchObject({ t: 'ret', id, ok: false, error: { code: 'E_NOT_READY' } });
    }
    expect(h.ret(1)).toMatchObject({ error: { code: 'E_NOT_READY', detail: '' } });
    expect(h.ret(3)).toMatchObject({ error: { code: 'E_NOT_READY', detail: '' } });
  });

  it('refuses malformed input before anything runs, and answers nothing to a message that is not a call', async () => {
    const h = harness();
    h.call(1, { m: 'selectCommunity', communityId: 'XYZ' });
    h.call(2, { m: 'send', channelId: 'a'.repeat(32), text: ' \n ' });
    h.call(3, { m: 'nope' });
    h.call(4, { m: 'openChannel', channelId: 'A'.repeat(32) });
    h.call(5, { m: 'joinCommunity', invite: '   ' });
    h.controller.handle({ t: 'hello' });
    h.controller.handle({ t: 'call', id: 'x', command: { m: 'start' } });
    h.controller.handle({ t: 'test', op: 'gateway-stop' });
    await vi.waitFor(() => expect(h.ret(5)).toBeDefined());
    for (const id of [1, 2, 3, 4, 5]) expect(h.ret(id)).toMatchObject({ ok: false, error: { code: 'E_BAD_INPUT' } });
    expect(h.ret(1)).toMatchObject({ error: { code: 'E_BAD_INPUT', detail: '' } });
    expect(h.ret(3)).toMatchObject({ error: { code: 'E_BAD_INPUT', detail: '' } });
    expect(h.posted.filter((m) => m.t === 'ret')).toHaveLength(5);
    expect(h.paths).toEqual([]);
  });

  it("a lost store offers resetDevice, which wipes this browser's device and boots again", async () => {
    const { core } = stubCore();
    const wiped: string[] = [];
    let boots = 0;
    const h = harness({
      boot: (instance) => {
        boots += 1;
        return Promise.resolve(boots === 1 ? { kind: 'store-lost' } : { kind: 'opened', core, instance });
      },
      resetDevice: (instance) => { wiped.push(toHex(instance.instanceId)); return Promise.resolve(); },
    });
    h.call(1, { m: 'start' });
    await vi.waitFor(() => expect(h.account()?.phase).toBe('store-lost'));
    h.call(2, { m: 'resetDevice' });
    await vi.waitFor(() => expect(h.ret(2)).toEqual({ t: 'ret', id: 2, ok: true, value: null }));
    expect(h.account()?.phase).toBe('needs-signup');
    expect(wiped).toEqual([INSTANCE_HEX]);
    expect(boots).toBe(2);
  });
});


it('uses admission request ids rather than rank to discard an old search', async () => {
  const { SearchGate } = await import('./barrier');
  const gate = new SearchGate();
  const older = gate.next();
  const newer = gate.next();
  const hits: string[] = [];
  if (gate.current(older)) hits.push('older rank 0');
  if (gate.current(newer)) hits.push('newer rank 0');
  expect(hits).toEqual(['newer rank 0']);
  gate.invalidate();
  expect(gate.current(newer)).toBe(false);
});

it('scrubs HTTP details and unexpected exception text before any page reply', async () => {
  const { sanitizeWorkerError } = await import('./controller');
  const http = new DillaHttpError({ code: 'E_NOT_FOUND', detail: 'Bearer SECRET-123', status: 404,
    retryAfterMs: 90_000_000, extra: [] });
  expect(sanitizeWorkerError(http)).toEqual({ code: 'E_NOT_FOUND', detail: '', status: 404, retryAfterMs: null });
  expect(sanitizeWorkerError(new Error('recovery key SECRET-456'))).toEqual({
    code: 'E_INTERNAL', detail: '', status: 0, retryAfterMs: null,
  });
  const h = harness();
  h.call(71, { m: 'unknown SECRET-789' });
  expect(JSON.stringify(h.posted)).not.toContain('SECRET-789');
  expect(h.ret(71)).toMatchObject({ ok: false, error: { code: 'E_BAD_INPUT', detail: '' } });
});

it('rejects every extra release-notice field and wrong instance or nonce', async () => {
  const { isReleaseNotice } = await import('./protocol');
  const instance = '42'.repeat(16);
  const nonce = 'aa'.repeat(16);
  expect(isReleaseNotice({ t: 'released', instance, nonce }, instance, nonce)).toBe(true);
  expect(isReleaseNotice({ t: 'released', instance, nonce, token: 'SECRET' }, instance, nonce)).toBe(false);
  expect(isReleaseNotice({ t: 'released', instance, nonce }, '43'.repeat(16), nonce)).toBe(false);
  expect(isReleaseNotice({ t: 'released', instance, nonce }, instance, 'bb'.repeat(16))).toBe(false);
});

it('refreshes only core-owned pending leaves and never publishes list bytes', async () => {
  const w = world();
  const signedHead = encode([1, new Uint8Array([9]), new Uint8Array(64), new Uint8Array(32)]);
  w.routes.getDeviceList.mockResolvedValue({ version: 1n, blob: new Uint8Array([9]), raw: signedHead });
  await toReady(w);
  w.core.pendingLeaves.mockReturnValue([{ groupId: GROUP, leafIndex: 0, userId: STRANGER, cachedVersion: 0n, reason: 'unknown' }]);
  w.gateway.emit({ type: 'ready', info: {
    deviceId: DEVICE, userId: USER, generation: 7n, versions: { wire: 2n, e2ee: 1n, media: 1n }, keypackagesRemaining: 1, groups: [],
    heartbeatMs: 30_000, maxFrameBytes: 131_584, backoffMs: 500, backoffJitterMs: 250,
  } });
  await vi.waitFor(() => expect(w.core.deviceListIngest).toHaveBeenCalled());
  expect(w.core.pendingLeaves).toHaveBeenCalled();
  expect(w.routes.getDeviceList).toHaveBeenCalledWith(STRANGER);
  expect(JSON.stringify(w.posted)).not.toContain(Array.from(signedHead).join(','));
});

it('runs the trust refresh that a gateway ready arriving before phase ready could not (client I3)', async () => {
  const w = world();
  const signedHead = encode([1, new Uint8Array([9]), new Uint8Array(64), new Uint8Array(32)]);
  w.routes.getDeviceList.mockResolvedValue({ version: 1n, blob: new Uint8Array([9]), raw: signedHead });
  w.core.pendingLeaves.mockReturnValue([{ groupId: GROUP, leafIndex: 0, userId: STRANGER, cachedVersion: 0n, reason: 'unknown' }]);
  const dms = deferred<{ channelId: Uint8Array; kind: 3 | 4; members: Uint8Array[] }[]>();
  w.routes.listDms.mockImplementationOnce(() => dms.promise);
  w.call(1, { m: 'start' });
  await vi.waitFor(() => expect(w.routes.listDms).toHaveBeenCalled());
  w.gateway.emit({ type: 'ready', info: READY_INFO });
  await vi.waitFor(() => expect(w.core.pendingLeaves).toHaveBeenCalled());
  expect(w.core.deviceListIngest).not.toHaveBeenCalled();
  dms.resolve([]);
  await vi.waitFor(() => expect(w.account()?.phase).toBe('ready'));
  await vi.waitFor(() => expect(w.core.deviceListIngest).toHaveBeenCalled());
});

it('republishes trust when a member slice or the DMs land after it, so a reloaded Privacy has names and Compare (product P1)', async () => {
  const w = world();
  w.state.groups = [textGroup(2)];
  w.core.trustState.mockReturnValue(strangerTrust(null));
  await toReady(w);
  w.call(70, { m: 'refreshTrust' });
  await vi.waitFor(() => expect(w.ret(70)).toMatchObject({ ok: true }));
  expect(w.last<TrustSummary[]>('trust')?.[0]).toMatchObject({ name: toHex(STRANGER).slice(0, 8), shared: false });
  // The next member read names the contact; nothing else touches trust.
  w.routes.listMembers.mockResolvedValue([
    { userId: USER, username: 'web', display: 'Web', kind: 0, nick: '', roleIds: [] },
    { userId: STRANGER, username: 'bjorn', display: 'Björn', kind: 0, nick: '', roleIds: [] },
  ]);
  w.call(71, { m: 'selectCommunity', communityId: toHex(COMMUNITY) });
  await vi.waitFor(() => expect(w.last<TrustSummary[]>('trust')?.[0]).toMatchObject({ name: 'Björn', shared: true }));
  // A DM with a person no community lists: the DM list landing republishes too.
  const dmPeer = new Uint8Array(16).fill(0x3d);
  w.core.trustState.mockReturnValue([...strangerTrust(null), { userId: dmPeer, umk: STRANGER_UMK, firstSeen: 1n, verified: false, changedUmk: null, blocked: null }]);
  w.state.groups = [textGroup(2), { ...textGroup(2), groupId: DM_GROUP, communityId: null, targetId: DM_CHANNEL }];
  w.state.dms = [{ channelId: DM_CHANNEL, kind: 3, members: [USER, dmPeer] }];
  w.gateway.emit({ type: 'ready', info: READY_INFO });
  await vi.waitFor(() => expect(w.last<TrustSummary[]>('trust')?.find((t) => t.userId === toHex(dmPeer))).toMatchObject({ shared: true }));
});

it('a second ready during trust ingestion reads its newly pending leaf before the refresh settles', async () => {
  const w = world();
  await toReady(w);
  const first = deferred<Awaited<ReturnType<typeof w.routes.getDeviceList>>>();
  w.routes.getDeviceList.mockImplementationOnce(() => first.promise);
  w.core.pendingLeaves.mockReturnValue([{ groupId: GROUP, leafIndex: 1, userId: STRANGER, cachedVersion: 0n, reason: 'unknown' }]);
  w.gateway.emit({ type: 'ready', info: READY_INFO });
  await vi.waitFor(() => expect(w.routes.getDeviceList).toHaveBeenCalledTimes(1));
  w.core.pendingLeaves.mockReturnValue([{ groupId: GROUP, leafIndex: 2, userId: STRANGER, cachedVersion: 0n, reason: 'unknown' }]);
  w.gateway.emit({ type: 'ready', info: READY_INFO });
  first.resolve({ version: 1n, blob: new Uint8Array([9]), raw: encode([1, new Uint8Array([9]), new Uint8Array(64), new Uint8Array(32)]) });
  await vi.waitFor(() => expect(w.core.deviceListIngest).toHaveBeenCalledTimes(2));
  expect(w.core.deviceListIngest.mock.calls.map((args) => args[1])).toEqual([1, 2]);
});

it('reads one signed history per user and ingests it for each pending leaf', async () => {
  const w = world();
  await toReady(w);
  w.core.pendingLeaves.mockReturnValue([
    { groupId: GROUP, leafIndex: 1, userId: STRANGER, cachedVersion: 0n, reason: 'unknown' },
    { groupId: GROUP, leafIndex: 2, userId: STRANGER, cachedVersion: 0n, reason: 'unknown' },
  ]);
  w.call(991, { m: 'refreshTrust' });
  await vi.waitFor(() => expect(w.ret(991)).toMatchObject({ ok: true }));
  expect(w.core.deviceListIngest).toHaveBeenCalledTimes(2);
  expect(w.routes.getDeviceList).toHaveBeenCalledTimes(2);
  expect(w.core.deviceListIngest.mock.calls[0][3]).toEqual(w.core.deviceListIngest.mock.calls[1][3]);
});

// CI trace of PR #59 (webkit-web): a second device of the account reloads mid-sync, before its own signed list arrived.
// The pass after the reload holds every own leaf at cached version 0 and reads one history, [v1, v2]. The core caches v2
// on the first leaf's ingest and anchors every later ingest on the cached row (trust.rs ingest_leaf: the history's first
// row is the cached version), so the full history is refused E_DEVICE_LIST_STALE for every other leaf of the account. A
// message of the other device, held for its sender's list, was never released and stayed "could not be read".
it('ingests the rest of a user\'s leaves from the version the first ingest cached, so every held row is released', async () => {
  const w = world();
  await toReady(w);
  const row = (v: number) => [v, new Uint8Array([v]), new Uint8Array(64), new Uint8Array(32)];
  const headRaw = encode(row(2));
  w.routes.getDeviceList.mockImplementation(() => Promise.resolve({ version: 2n, blob: new Uint8Array([2]), raw: headRaw }));
  w.routes.getDeviceListHistory.mockImplementation(() => Promise.resolve({ raw: encode([row(1), row(2)]), count: 2, lastVersion: 2n }));
  // The core's anchor rule: once a version is cached, a history must start at it.
  let cached = 0n;
  w.core.deviceListIngest.mockImplementation((_g: Uint8Array, _l: number, _u: Uint8Array, history: Uint8Array) => {
    const first = BigInt((decode(history) as [[number | bigint]])[0][0]);
    if (cached !== 0n && first !== cached) throw new CoreError('E_DEVICE_LIST_STALE', '');
    cached = 2n;
    return { version: 2n, changed: false, quarantined: 0 };
  });
  w.core.pendingLeaves.mockReturnValue([
    { groupId: GROUP, leafIndex: 0, userId: STRANGER, cachedVersion: 0n, reason: 'unknown' },
    { groupId: DM_GROUP, leafIndex: 1, userId: STRANGER, cachedVersion: 0n, reason: 'unknown' },
  ]);
  w.call(993, { m: 'refreshTrust' });
  await vi.waitFor(() => expect(w.ret(993)).toMatchObject({ ok: true }));
  expect((w.core.revalidatePending.mock.calls as unknown as [Uint8Array, number][]).map(([g, leaf]) => [toHex(g), leaf])).toEqual([[toHex(GROUP), 0], [toHex(DM_GROUP), 1]]);
  // Still one read of the history for the user in the pass.
  expect(w.routes.getDeviceListHistory).toHaveBeenCalledTimes(1);
});

// The core decodes head_list as the signed DeviceList itself (core/dilla-core/src/client/trust.rs bounded_list, a
// six-element array) and stores it as the cached anchor; the native peer hands it head.blob (testkit web_driver.rs
// signed_history). The served four-element row [version, blob, ssk_signature, prev_hash] is refused as E_DEVICE_LIST_STALE,
// which left every contact's leaf pending and every message unreadable in a real browser (task 10).
it('hands the core the signed list of the candidate head, never the served row around it', async () => {
  const w = world();
  await toReady(w);
  w.core.pendingLeaves.mockReturnValue([{ groupId: GROUP, leafIndex: 1, userId: STRANGER, cachedVersion: 0n, reason: 'unknown' }]);
  w.call(992, { m: 'refreshTrust' });
  await vi.waitFor(() => expect(w.ret(992)).toMatchObject({ ok: true }));
  expect(w.core.deviceListIngest).toHaveBeenCalledTimes(1);
  expect(w.core.deviceListIngest.mock.calls[0][4]).toEqual(new Uint8Array([9]));
});

// Controller ruling 2 (89923a6): the core refuses sending to a group holding a failed leaf. A device that joined
// before this device ingested the list naming it fails against the cached list until a trust refresh ingests the newer
// one, which ends that verdict (trust.rs apply_verdict). Found in core-worker.spec.ts: A's send right after its own
// second device B joined was refused E_DEVICE_UNLISTED while the refresh the join started was still in flight.
it('a send refused by a leaf verdict waits for one trust refresh and is tried once more', async () => {
  const w = world();
  await toReady(w);
  await openGeneral(w);
  w.core.pendingLeaves.mockReturnValue([{ groupId: GROUP, leafIndex: 1, userId: STRANGER, cachedVersion: 1n, reason: 'ready-refresh' }]);
  const order: string[] = [];
  w.core.deviceListIngest.mockImplementation(() => { order.push('ingest'); return { version: 2n, changed: false, quarantined: 0 }; });
  w.sync.sendRequest
    .mockImplementationOnce(() => { order.push('send'); throw new CoreError('E_DEVICE_UNLISTED', ''); })
    .mockImplementationOnce(() => { order.push('send'); return new Uint8Array(16).fill(0x77); });
  w.call(51, { m: 'send', channelId: toHex(CHANNEL), text: 'hello' });
  await vi.waitFor(() => expect(w.ret(51)).toBeDefined());
  expect(w.ret(51)).toEqual({ t: 'ret', id: 51, ok: true, value: { msgId: '77'.repeat(16) } });
  expect(order).toEqual(['send', 'ingest', 'send']);
});

it('a reaction or an edit refused by a leaf verdict waits for one trust refresh and is tried once more (client I2)', async () => {
  const w = world();
  await toReady(w);
  await openGeneral(w);
  w.core.pendingLeaves.mockReturnValue([{ groupId: GROUP, leafIndex: 1, userId: STRANGER, cachedVersion: 1n, reason: 'ready-refresh' }]);
  const order: string[] = [];
  w.core.deviceListIngest.mockImplementation(() => { order.push('ingest'); return { version: 2n, changed: false, quarantined: 0 }; });
  w.sync.sendRequest
    .mockImplementationOnce(() => { order.push('react'); throw new CoreError('E_DEVICE_UNLISTED', ''); })
    .mockImplementationOnce(() => { order.push('react'); return new Uint8Array(16).fill(0x77); });
  const msgId = '66'.repeat(16);
  w.call(54, { m: 'react', channelId: toHex(CHANNEL), msgId, emoji: '👍', on: true });
  await vi.waitFor(() => expect(w.ret(54)).toBeDefined());
  expect(w.ret(54)).toEqual({ t: 'ret', id: 54, ok: true, value: null });
  expect(order).toEqual(['react', 'ingest', 'react']);
  w.sync.sendRequest.mockClear();
  w.sync.sendRequest.mockImplementation(() => { throw new CoreError('E_UMK_CHANGED', ''); });
  w.call(55, { m: 'editMessage', channelId: toHex(CHANNEL), msgId, text: 'edited' });
  await vi.waitFor(() => expect(w.ret(55)).toBeDefined());
  expect(w.ret(55)).toMatchObject({ ok: false, error: { code: 'E_UMK_CHANGED' } });
  expect(w.sync.sendRequest).toHaveBeenCalledTimes(1);
});

it('a send still refused after the trust refresh answers the verdict, and a changed key is not retried', async () => {
  const w = world();
  await toReady(w);
  await openGeneral(w);
  w.sync.sendRequest.mockImplementation(() => { throw new CoreError('E_DEVICE_UNLISTED', ''); });
  w.call(52, { m: 'send', channelId: toHex(CHANNEL), text: 'hello' });
  await vi.waitFor(() => expect(w.ret(52)).toBeDefined());
  expect(w.ret(52)).toMatchObject({ ok: false, error: { code: 'E_DEVICE_UNLISTED' } });
  expect(w.sync.sendRequest).toHaveBeenCalledTimes(2);
  w.sync.sendRequest.mockClear();
  w.sync.sendRequest.mockImplementation(() => { throw new CoreError('E_UMK_CHANGED', ''); });
  w.call(53, { m: 'send', channelId: toHex(CHANNEL), text: 'hello' });
  await vi.waitFor(() => expect(w.ret(53)).toBeDefined());
  expect(w.ret(53)).toMatchObject({ ok: false, error: { code: 'E_UMK_CHANGED' } });
  expect(w.sync.sendRequest).toHaveBeenCalledTimes(1);
});

it('clears an old search slice when a newer core search refuses', async () => {
  const w = world();
  await toReady(w);
  await openGeneral(w);
  w.core.search.mockReturnValueOnce([{ groupId: GROUP, seq: 1n, snippet: 'old', rank: 0n, threadRoot: null }]);
  w.call(992, { m: 'search', scope: 'all', id: null, query: 'old', cursor: null });
  await vi.waitFor(() => expect(w.ret(992)).toMatchObject({ ok: true }));
  expect(w.last<{ hits: unknown[] }>('search')?.hits).toHaveLength(1);
  w.core.search.mockImplementationOnce(() => { throw new CoreError('E_CORE_INPUT', 'bad query'); });
  w.call(993, { m: 'search', scope: 'all', id: null, query: 'bad', cursor: null });
  await vi.waitFor(() => expect(w.ret(993)).toMatchObject({ ok: false, error: { code: 'E_CORE_INPUT', detail: '' } }));
  expect(w.last<{ hits: unknown[] }>('search')?.hits).toEqual([]);
});

it('publishes channel destinations for community and DM hits and drops unknown groups', async () => {
  const w = world();
  await toReady(w);
  await openGeneral(w);
  w.state.dms = [{ channelId: DM_CHANNEL, kind: 3, members: [USER, STRANGER] }];
  w.state.groups.push({ ...textGroup(2), groupId: DM_GROUP, communityId: null, targetId: DM_CHANNEL });
  w.sync.deps!.onUnexpectedWelcome(DM_GROUP);
  await vi.waitFor(() => expect(w.last<DmSummary[]>('dms')?.[0]?.id).toBe(toHex(DM_CHANNEL)));
  w.core.search.mockReturnValueOnce([
    { groupId: GROUP, seq: 1n, snippet: 'community', rank: 0n, threadRoot: null },
    { groupId: DM_GROUP, seq: 2n, snippet: 'dm', rank: 0n, threadRoot: null },
    { groupId: ORPHAN_GROUP, seq: 3n, snippet: 'orphan', rank: 0n, threadRoot: null },
  ]);
  w.call(994, { m: 'search', scope: 'all', id: null, query: 'a', cursor: null });
  await vi.waitFor(() => expect(w.ret(994)).toMatchObject({ ok: true }));
  expect(w.last<{ hits: unknown[] }>('search')?.hits).toEqual([
    { channelId: toHex(CHANNEL), communityId: toHex(COMMUNITY), seq: '1', snippet: 'community', rank: '0', threadRoot: null },
    { channelId: toHex(DM_CHANNEL), communityId: null, seq: '2', snippet: 'dm', rank: '0', threadRoot: null },
  ]);
  const page = w.last<{ hits: unknown[]; cursor: unknown }>('search');
  expect(JSON.stringify(page)).not.toContain('groupId');
  expect(page?.cursor).toEqual({ token: '1' });
  const continuation = { m: 'search', scope: 'all', id: null, query: 'a', cursor: page?.cursor };
  expect(JSON.stringify(continuation)).not.toContain('groupId');
  w.call(995, continuation);
  await vi.waitFor(() => expect(w.ret(995)).toMatchObject({ ok: true }));
  expect(w.core.search).toHaveBeenLastCalledWith({ kind: 'all', id: null }, 'a',
    { rank: 0n, groupId: ORPHAN_GROUP, seq: 3n }, 50);
});

// J1: a hit carries who wrote the row and when, read from the stored row; a row the core cannot find leaves both out.
it('adds the author and the time of the stored row to a search hit', async () => {
  const w = world();
  await toReady(w);
  await openGeneral(w);
  w.state.rows = [strangerRow(1)];
  w.core.search.mockReturnValueOnce([
    { groupId: GROUP, seq: 1n, snippet: 'known', rank: 0n, threadRoot: null },
    { groupId: GROUP, seq: 9n, snippet: 'unread', rank: 0n, threadRoot: null },
  ]);
  w.call(997, { m: 'search', scope: 'all', id: null, query: 'a', cursor: null });
  await vi.waitFor(() => expect(w.ret(997)).toMatchObject({ ok: true }));
  const hits = w.last<{ hits: { seq: string; author?: string; ts?: number }[] }>('search')?.hits ?? [];
  expect(hits[0]).toMatchObject({ seq: '1', ts: 1_700_000_000, author: toHex(STRANGER).slice(0, 8) });
  expect(hits[1]).not.toHaveProperty('ts');
  expect(hits[1]).not.toHaveProperty('author');
});

// Ruling (c) of 2026-10-13: the unlisted and validated-Remove Privacy states are derived in the worker from what the core
// already answers (pendingLeaves reasons, deviceListIngest's quarantine count and verdict codes); no group id reaches the page.
const STRANGER_UMK = new Uint8Array(32).fill(0xbb);
const CHANGED_UMK = new Uint8Array(32).fill(0xcc);
const strangerTrust = (changed: Uint8Array | null): TrustRecord[] =>
  [{ userId: STRANGER, umk: STRANGER_UMK, firstSeen: 1n, verified: false, changedUmk: changed, blocked: null }];

// Task 10 found Privacy listing the browser's own user as a contact ("Compare with web tester"): the core pins the
// user's own key too, and a person never compares a safety number with themselves.
it('leaves the own user out of the trust slice', async () => {
  const w = world();
  await toReady(w);
  w.core.trustState.mockReturnValue([
    { userId: USER, umk: new Uint8Array(32).fill(0xaa), firstSeen: 1n, verified: false, changedUmk: null, blocked: null },
    ...strangerTrust(null),
  ]);
  w.call(980, { m: 'refreshTrust' });
  await vi.waitFor(() => expect(w.ret(980)).toMatchObject({ ok: true }));
  expect(w.last<TrustSummary[]>('trust')?.map((row) => row.userId)).toEqual([toHex(STRANGER)]);
  expect(w.account()?.ownTrust).toBeNull();
});

// Leaving the own row out of the contacts must not hide what the core says about the own account. ABI 8 (F3): the own row
// is always verified and never changed; its `blocked` reports an own-user leaf under another key (E_UMK_CHANGED) or an
// unlisted own device. The banner is raised from that, never from changedUmk or the network-filled sets.
const ownRow = (blocked: TrustBlock | null): TrustRecord =>
  ({ userId: USER, umk: new Uint8Array(32).fill(0xaa), firstSeen: 1n, verified: true, changedUmk: null, blocked });

it('raises ownTrust from the own row\'s blocked verdict: changed for E_UMK_CHANGED, unlisted for any other code', async () => {
  const w = world();
  await toReady(w);
  w.core.trustState.mockReturnValue([ownRow('E_UMK_CHANGED'), ...strangerTrust(null)]);
  w.call(9801, { m: 'refreshTrust' });
  await vi.waitFor(() => expect(w.ret(9801)).toMatchObject({ ok: true }));
  expect(w.last<TrustSummary[]>('trust')?.map((row) => row.userId)).toEqual([toHex(STRANGER)]);
  expect(w.account()?.ownTrust).toBe('changed');
  for (const [blocked, want] of [['E_DEVICE_UNLISTED', 'unlisted'], ['E_CREDENTIAL', 'unlisted'], ['E_TIER_MISMATCH', 'unlisted'], [null, null]] as const) {
    w.core.trustState.mockReturnValue([ownRow(blocked), ...strangerTrust(null)]);
    w.core.trustGeneration.mockReturnValue(w.core.trustGeneration() + 1n);
    w.sync.deps!.onTrustChanged();
    expect(w.account()?.ownTrust).toBe(want);
  }
});

it('reads the own banner straight from the store after a reload, before any list is fetched', async () => {
  const w = world();
  w.core.trustState.mockReturnValue([ownRow('E_UMK_CHANGED')]);
  await toReady(w);
  await vi.waitFor(() => expect(w.account()?.ownTrust).toBe('changed'));
  expect(w.routes.getDeviceList).not.toHaveBeenCalled();
});

// fixA: an own-user history under another UMK throws E_UMK_CHANGED from the ingest. It is an own-trust signal, not an
// unlisted contact, and it is not fetched again in a loop.
it('raises ownTrust on an own-user E_UMK_CHANGED ingest without a crash or a retry, and drops it with the leaf', async () => {
  const w = world();
  await toReady(w);
  w.core.trustState.mockReturnValue([ownRow(null), ...strangerTrust(null)]);
  w.routes.getDeviceList.mockResolvedValue({ version: 1n, blob: new Uint8Array([9]),
    raw: encode([1, new Uint8Array([9]), new Uint8Array(64), new Uint8Array(32)]) });
  w.core.pendingLeaves.mockReturnValue([{ groupId: GROUP, leafIndex: 4, userId: USER, cachedVersion: 0n, reason: 'unknown' }]);
  w.core.deviceListIngest.mockImplementation(() => { throw new CoreError('E_UMK_CHANGED', ''); });
  w.call(9802, { m: 'refreshTrust' });
  await vi.waitFor(() => expect(w.ret(9802)).toMatchObject({ ok: true }));
  expect(w.account()?.ownTrust).toBe('changed');
  expect(w.account()?.phase).toBe('ready');
  w.call(9803, { m: 'refreshTrust' });
  await vi.waitFor(() => expect(w.ret(9803)).toMatchObject({ ok: true }));
  expect(w.core.deviceListIngest).toHaveBeenCalledTimes(1);
  expect(w.account()?.ownTrust).toBe('changed');
  // A validated Remove takes the leaf out of the group: the core no longer reports it, and nothing else holds the banner.
  w.core.pendingLeaves.mockReturnValue([]);
  w.call(9804, { m: 'refreshTrust' });
  await vi.waitFor(() => expect(w.ret(9804)).toMatchObject({ ok: true }));
  expect(w.account()?.ownTrust).toBeNull();
});

// Ruling M6 revised: no own-key compare, so the ownKey command (7bf0bfe) is gone.
it('no longer accepts the ownKey command', async () => {
  const w = world();
  await toReady(w);
  w.call(9805, { m: 'ownKey' });
  await vi.waitFor(() => expect(w.ret(9805)).toMatchObject({ ok: false, error: { code: 'E_BAD_INPUT' } }));
});

it('keeps an own leaf the core refused against the own signed list on the account slice', async () => {
  const w = world();
  await toReady(w);
  w.core.trustState.mockReturnValue([
    { userId: USER, umk: new Uint8Array(32).fill(0xaa), firstSeen: 1n, verified: false, changedUmk: null, blocked: null },
  ]);
  w.routes.getDeviceList.mockResolvedValue({ version: 1n, blob: new Uint8Array([9]),
    raw: encode([1, new Uint8Array([9]), new Uint8Array(64), new Uint8Array(32)]) });
  w.core.pendingLeaves.mockReturnValue([{ groupId: GROUP, leafIndex: 1, userId: USER, cachedVersion: 1n, reason: 'ready-refresh' }]);
  w.core.deviceListIngest.mockImplementationOnce(() => { throw new CoreError('E_DEVICE_UNLISTED', 'leaf not on the list'); });
  w.call(9803, { m: 'refreshTrust' });
  await vi.waitFor(() => expect(w.ret(9803)).toMatchObject({ ok: true }));
  expect(w.last<TrustSummary[]>('trust')).toEqual([]);
  expect(w.account()?.ownTrust).toBe('unlisted');
  w.core.pendingLeaves.mockReturnValue([{ groupId: GROUP, leafIndex: 2, userId: USER, cachedVersion: 1n, reason: 'ready-refresh' }]);
  w.core.deviceListIngest.mockReturnValueOnce({ version: 1n, changed: false, quarantined: 0 });
  w.call(9804, { m: 'refreshTrust' });
  await vi.waitFor(() => expect(w.ret(9804)).toMatchObject({ ok: true }));
  expect(w.account()?.ownTrust).toBeNull();
});

// web3.8 C3 (walk #8): a device of the own user that joins changes the own list; the device count follows it without a
// visit to Settings. One read per trust pass, however many own leaves changed in it.
// C3 fix I1: the core never answers `changed` for the own user (a new own device keeps the same key, and a new key
// throws E_UMK_CHANGED), so the trigger is an ingest whose version is newer than the own list's at the pass start (1).
describe('the device count follows an own leaf the trust pass ingests', () => {
  const THIRD = new Uint8Array(16).fill(0x0d);
  async function ownWorld(): Promise<World> {
    const w = world();
    await toReady(w);
    await settle();
    w.routes.getDeviceList.mockResolvedValue({ version: 1n, blob: new Uint8Array([9]),
      raw: encode([1, new Uint8Array([9]), new Uint8Array(64), new Uint8Array(32)]) });
    w.routes.listDevices.mockResolvedValue([
      { id: DEVICE, tier: 1, signerTier: 1, verifiedAt: null, revokedAt: null, lastSeen: 1_700_000_000n, label: null },
      { id: OTHER_DEVICE, tier: 1, signerTier: 1, verifiedAt: null, revokedAt: null, lastSeen: 1_699_999_000n, label: null },
      { id: THIRD, tier: 1, signerTier: 1, verifiedAt: null, revokedAt: null, lastSeen: 1_699_998_000n, label: null },
    ]);
    return w;
  }
  const slicesAfter = (w: World, from: number): string[] =>
    w.posted.slice(from).filter((m): m is SliceMessage => m.t === 'slice').map((m) => m.name);

  it('an own leaf ingested at a newer list version reads the devices once more and republishes devices and diagnostics', async () => {
    const w = await ownWorld();
    w.core.pendingLeaves.mockReturnValue([{ groupId: GROUP, leafIndex: 3, userId: USER, cachedVersion: 1n, reason: 'ready-refresh' }]);
    w.core.deviceListIngest.mockReturnValueOnce({ version: 2n, changed: false, quarantined: 0 });
    const reads = w.routes.listDevices.mock.calls.length;
    const from = w.posted.length;
    w.call(9820, { m: 'refreshTrust' });
    await vi.waitFor(() => expect(w.ret(9820)).toMatchObject({ ok: true }));
    expect(w.routes.listDevices).toHaveBeenCalledTimes(reads + 1);
    expect(slicesAfter(w, from)).toEqual(expect.arrayContaining(['devices', 'diagnostics']));
    expect(w.last<DeviceSummary[]>('devices')?.map((d) => d.id)).toEqual([toHex(DEVICE), toHex(OTHER_DEVICE), toHex(THIRD)]);
    expect(w.last<DiagnosticsState>('diagnostics')?.devices).toBe(3);
  });

  it('two own leaves at a newer version in one pass make one read', async () => {
    const w = await ownWorld();
    w.core.pendingLeaves.mockReturnValue([
      { groupId: GROUP, leafIndex: 3, userId: USER, cachedVersion: 1n, reason: 'ready-refresh' },
      { groupId: DM_GROUP, leafIndex: 2, userId: USER, cachedVersion: 1n, reason: 'ready-refresh' },
    ]);
    w.core.deviceListIngest
      .mockReturnValueOnce({ version: 2n, changed: false, quarantined: 0 })
      .mockReturnValueOnce({ version: 3n, changed: false, quarantined: 0 });
    const reads = w.routes.listDevices.mock.calls.length;
    w.call(9821, { m: 'refreshTrust' });
    await vi.waitFor(() => expect(w.ret(9821)).toMatchObject({ ok: true }));
    expect(w.core.deviceListIngest).toHaveBeenCalledTimes(2);
    expect(w.routes.listDevices).toHaveBeenCalledTimes(reads + 1);
  });

  it('control: an own leaf at the version already held, or a contact\'s newer list, reads no devices', async () => {
    const w = await ownWorld();
    w.core.pendingLeaves.mockReturnValue([
      { groupId: GROUP, leafIndex: 3, userId: USER, cachedVersion: 1n, reason: 'ready-refresh' },
      { groupId: GROUP, leafIndex: 4, userId: STRANGER, cachedVersion: 1n, reason: 'ready-refresh' },
    ]);
    w.core.deviceListIngest
      .mockReturnValueOnce({ version: 1n, changed: false, quarantined: 0 })
      .mockReturnValueOnce({ version: 5n, changed: true, quarantined: 0 });
    const reads = w.routes.listDevices.mock.calls.length;
    w.call(9822, { m: 'refreshTrust' });
    await vi.waitFor(() => expect(w.ret(9822)).toMatchObject({ ok: true }));
    expect(w.core.deviceListIngest).toHaveBeenCalledTimes(2);
    expect(w.routes.listDevices).toHaveBeenCalledTimes(reads);
  });

  it('a core with no own list to read still runs the pass, ingests the leaf and reads no devices', async () => {
    const w = await ownWorld();
    Object.defineProperty(w.state, 'ownList', { get: () => { throw new CoreError('E_CORE_STATE', ''); } });
    w.core.pendingLeaves.mockReturnValue([{ groupId: GROUP, leafIndex: 3, userId: USER, cachedVersion: 1n, reason: 'ready-refresh' }]);
    w.core.deviceListIngest.mockReturnValueOnce({ version: 2n, changed: false, quarantined: 0 });
    const reads = w.routes.listDevices.mock.calls.length;
    w.call(9825, { m: 'refreshTrust' });
    await vi.waitFor(() => expect(w.ret(9825)).toMatchObject({ ok: true }));
    expect(w.core.deviceListIngest).toHaveBeenCalledTimes(1);
    expect(w.core.revalidatePending).toHaveBeenCalledWith(GROUP, 3, 256);
    expect(w.routes.listDevices).toHaveBeenCalledTimes(reads);
  });

  it('diagnostics counts only live devices and is set after ready without a page command (web3.9)', async () => {
    const w = world();
    const rows: DeviceRow[] = [
      { id: DEVICE, tier: 1, signerTier: 1, verifiedAt: null, revokedAt: null, lastSeen: 1_700_000_000n, label: null },
      { id: OTHER_DEVICE, tier: 1, signerTier: 1, verifiedAt: null, revokedAt: 1_699_000_000n, lastSeen: 1_699_999_000n, label: null },
      { id: THIRD, tier: 1, signerTier: 1, verifiedAt: null, revokedAt: null, lastSeen: 1_699_998_000n, label: null },
    ];
    w.routes.listDevices.mockResolvedValue(rows);
    await toReady(w);
    w.gateway.emit({ type: 'ready', info: READY_INFO });
    await vi.waitFor(() => expect(w.last<DiagnosticsState>('diagnostics')?.devices).toBe(2));
  });

  it('an older device read that answers last does not publish over a newer one', async () => {
    const w = await ownWorld();
    const older = deferred<Awaited<ReturnType<typeof w.routes.listDevices>>>();
    const newer = deferred<Awaited<ReturnType<typeof w.routes.listDevices>>>();
    w.routes.listDevices.mockImplementationOnce(() => older.promise).mockImplementationOnce(() => newer.promise);
    w.call(9830, { m: 'refreshDevices' });
    await vi.waitFor(() => expect(w.routes.listDevices).toHaveBeenCalledTimes(1));
    w.call(9831, { m: 'refreshDevices' });
    await vi.waitFor(() => expect(w.routes.listDevices).toHaveBeenCalledTimes(2));
    const row = (id: Uint8Array, revokedAt: DeviceRow['revokedAt']): DeviceRow =>
      ({ id, tier: 1, signerTier: 1, verifiedAt: null, revokedAt, lastSeen: 1_700_000_000n, label: null });
    newer.resolve([row(DEVICE, null), row(OTHER_DEVICE, 1_699_000_000n), row(THIRD, null)]);
    await vi.waitFor(() => expect(w.ret(9831)).toMatchObject({ ok: true }));
    expect(w.last<DiagnosticsState>('diagnostics')?.devices).toBe(2);
    older.resolve([row(DEVICE, null), row(OTHER_DEVICE, null), row(THIRD, null)]);
    await vi.waitFor(() => expect(w.ret(9830)).toMatchObject({ ok: true }));
    expect(w.last<DiagnosticsState>('diagnostics')?.devices).toBe(2);
    expect(w.last<DeviceSummary[]>('devices')?.filter((d) => d.revokedAt === null)).toHaveLength(2);
  });

  // C3 fix m1: a failed device read neither fails the pass nor clears the count.
  it('a device read that fails keeps the last count, and the trust pass still answers ok', async () => {
    const w = await ownWorld();
    w.call(9823, { m: 'refreshDevices' });
    await vi.waitFor(() => expect(w.ret(9823)).toMatchObject({ ok: true }));
    expect(w.last<DiagnosticsState>('diagnostics')?.devices).toBe(3);
    w.routes.listDevices.mockRejectedValueOnce(refusal(503, 'E_UNAVAILABLE'));
    w.core.pendingLeaves.mockReturnValue([{ groupId: GROUP, leafIndex: 3, userId: USER, cachedVersion: 1n, reason: 'ready-refresh' }]);
    w.core.deviceListIngest.mockReturnValueOnce({ version: 2n, changed: false, quarantined: 0 });
    const reads = w.routes.listDevices.mock.calls.length;
    w.call(9824, { m: 'refreshTrust' });
    await vi.waitFor(() => expect(w.ret(9824)).toMatchObject({ ok: true }));
    expect(w.routes.listDevices).toHaveBeenCalledTimes(reads + 1);
    expect(w.last<DiagnosticsState>('diagnostics')?.devices).toBe(3);
    expect(w.last<DeviceSummary[]>('devices')).toHaveLength(3);
  });
});

// The trust patch (89923a6) answers `changed` only for the ingest that starts a key change; a later ingest of the same
// user still counts the changed-key leaf as quarantined. Reading that count as "unlisted" left the changed contact
// without a Verify (trust-polish.spec.ts "Copy alone does not approve" timed out on the disabled approve button).
it('does not call a contact with a pending key change unlisted from the changed-key quarantine count', async () => {
  const w = world();
  await toReady(w);
  w.core.trustState.mockReturnValue(strangerTrust(CHANGED_UMK));
  w.core.pendingLeaves.mockReturnValue([
    { groupId: GROUP, leafIndex: 1, userId: STRANGER, cachedVersion: 1n, reason: 'ready-refresh' },
    { groupId: GROUP, leafIndex: 2, userId: STRANGER, cachedVersion: 1n, reason: 'ready-refresh' },
  ]);
  w.core.deviceListIngest
    .mockReturnValueOnce({ version: 2n, changed: true, quarantined: 1 })
    .mockReturnValueOnce({ version: 2n, changed: false, quarantined: 1 });
  w.call(978, { m: 'refreshTrust' });
  await vi.waitFor(() => expect(w.ret(978)).toMatchObject({ ok: true }));
  expect(w.last<TrustSummary[]>('trust')?.[0]).not.toHaveProperty('unlisted');
  w.core.pendingLeaves.mockReturnValue([{ groupId: GROUP, leafIndex: 3, userId: STRANGER, cachedVersion: 1n, reason: 'changed' }]);
  w.core.deviceListIngest.mockReturnValueOnce({ version: 2n, changed: false, quarantined: 1 });
  w.call(979, { m: 'refreshTrust' });
  await vi.waitFor(() => expect(w.ret(979)).toMatchObject({ ok: true }));
  expect(w.core.deviceListIngest).toHaveBeenCalledTimes(3);
  expect(w.last<TrustSummary[]>('trust')?.[0]).not.toHaveProperty('unlisted');
});

// F4 (lead s): the published verified/failed/changed state is the core's trust_state row, read from the store; the
// quarantine count of an ingest is not read for it. An approval is published from the row trust_verify left behind.
it('publishes an approved contact from the core\'s row: verified only while none of its leaves is blocked', async () => {
  const w = world();
  w.state.dms = [{ channelId: DM_CHANNEL, kind: 3, members: [USER, STRANGER] }];
  await toReady(w);
  w.state.groups = [{ ...textGroup(2), groupId: DM_GROUP, communityId: null, targetId: DM_CHANNEL }];
  w.core.trustState.mockReturnValue(strangerTrust(CHANGED_UMK));
  w.call(9781, { m: 'refreshTrust' });
  await vi.waitFor(() => expect(w.ret(9781)).toMatchObject({ ok: true }));
  expect(w.last<TrustSummary[]>('trust')?.[0]).toMatchObject({ shared: true, changedUmk: toHex(CHANGED_UMK), verified: false });
  // The approval re-judged the leaves in the core: one of them is still off the approved key's list.
  w.core.trustVerify.mockImplementation(() => {
    w.core.trustState.mockReturnValue([{ userId: STRANGER, umk: CHANGED_UMK, firstSeen: 1n, verified: true, changedUmk: null, blocked: 'E_DEVICE_UNLISTED' }]);
    w.core.trustGeneration.mockReturnValue(5n);
  });
  w.call(9782, { m: 'verifyContact', userId: toHex(STRANGER), umk: toHex(CHANGED_UMK) });
  await vi.waitFor(() => expect(w.ret(9782)).toMatchObject({ ok: true }));
  expect(w.core.trustVerify).toHaveBeenCalledWith(STRANGER, CHANGED_UMK);
  expect(w.last<TrustSummary[]>('trust')?.[0]).toMatchObject({ verified: false, changedUmk: null, unlisted: true, blocked: 'E_DEVICE_UNLISTED' });
  // Once the core has nothing blocked, the approval stands, with no extra round of fetches.
  w.core.trustState.mockReturnValue([{ userId: STRANGER, umk: CHANGED_UMK, firstSeen: 1n, verified: true, changedUmk: null, blocked: null }]);
  w.core.trustGeneration.mockReturnValue(6n);
  w.sync.deps!.onTrustChanged();
  const row = w.last<TrustSummary[]>('trust')?.[0];
  expect(row).toMatchObject({ verified: true, changedUmk: null });
  expect(row).not.toHaveProperty('unlisted');
  expect(row).not.toHaveProperty('blocked');
});

it('never reads a contact with a stored blocked verdict as verified, right after a reload and before any refetch', async () => {
  const w = world();
  w.core.trustState.mockReturnValue([{ userId: STRANGER, umk: STRANGER_UMK, firstSeen: 1n, verified: true, changedUmk: null, blocked: 'E_CREDENTIAL' }]);
  await toReady(w);
  await vi.waitFor(() => expect(w.last<TrustSummary[]>('trust')?.[0]).toMatchObject({ verified: false, unlisted: true, blocked: 'E_CREDENTIAL' }));
  expect(w.routes.getDeviceList).not.toHaveBeenCalled();
  // A changed-key block reads as changed even when no validated list presented the key (changedUmk is null).
  w.core.trustState.mockReturnValue([{ userId: STRANGER, umk: STRANGER_UMK, firstSeen: 1n, verified: true, changedUmk: null, blocked: 'E_UMK_CHANGED' }]);
  w.core.trustGeneration.mockReturnValue(1n);
  w.sync.deps!.onTrustChanged();
  const row = w.last<TrustSummary[]>('trust')?.[0];
  expect(row).toMatchObject({ verified: false, blocked: 'E_UMK_CHANGED' });
  // Its Verify is the compare dialog's, not "unlisted": the changed key is not an unlisted device.
  expect(row).not.toHaveProperty('unlisted');
});

// fix/firefox-misrouted-audio: the page's media layer notes a recovered misroute; Diagnostics shows its stable code.
// Only codes on the call-note list are taken, so the page cannot write arbitrary text into Diagnostics.
it('callNote records E_MEDIA_MISROUTE in diagnostics and refuses any other code', async () => {
  const w = world();
  await toReady(w);
  await settle();
  w.call(7400, { m: 'callNote', code: 'E_MEDIA_MISROUTE' });
  await vi.waitFor(() => expect(w.ret(7400)).toMatchObject({ ok: true, value: null }));
  expect(w.last<DiagnosticsState>('diagnostics')?.errors.map((e) => e.code)).toEqual(['E_MEDIA_MISROUTE']);
  const bad: unknown[] = [{ m: 'callNote', code: 'E_INTERNAL' }, { m: 'callNote', code: 'hello' }, { m: 'callNote' }, { m: 'callNote', code: 1 }];
  bad.forEach((command, i) => { w.call(7410 + i, command); });
  await vi.waitFor(() => { for (let i = 0; i < bad.length; i++) expect(w.ret(7410 + i)).toBeDefined(); });
  bad.forEach((command, i) => {
    expect(w.ret(7410 + i), JSON.stringify(command)).toMatchObject({ ok: false, error: { code: 'E_BAD_INPUT', detail: '' } });
  });
  // A refused note is a refused command (recorded as E_BAD_INPUT), never the code it carried.
  expect(w.last<DiagnosticsState>('diagnostics')?.errors.map((e) => e.code).filter((c) => c !== 'E_BAD_INPUT')).toEqual(['E_MEDIA_MISROUTE']);
});

it('never keeps a verified contact when trust_state fails: the last rows read pending until the core answers (R3)', async () => {
  const w = world();
  w.core.trustState.mockReturnValue([{ userId: STRANGER, umk: STRANGER_UMK, firstSeen: 1n, verified: true, changedUmk: null, blocked: null }]);
  await toReady(w);
  await vi.waitFor(() => expect(w.last<TrustSummary[]>('trust')?.[0]).toMatchObject({ verified: true }));
  w.core.trustState.mockImplementation(() => { throw new CoreError('E_CORE_STORAGE', 'codec'); });
  w.core.trustGeneration.mockReturnValue(1n);
  w.sync.deps!.onTrustChanged();
  expect(w.last<TrustSummary[]>('trust')?.[0]).toMatchObject({ userId: toHex(STRANGER), verified: false, unlisted: true });
  expect(w.last<DiagnosticsState>('diagnostics')?.errors.map((e) => e.code)).toContain('E_CORE_STORAGE');
  w.core.trustState.mockReturnValue([{ userId: STRANGER, umk: STRANGER_UMK, firstSeen: 1n, verified: true, changedUmk: null, blocked: null }]);
  w.core.trustGeneration.mockReturnValue(2n);
  w.sync.deps!.onTrustChanged();
  expect(w.last<TrustSummary[]>('trust')?.[0]).toMatchObject({ verified: true });
  expect(w.last<TrustSummary[]>('trust')?.[0]).not.toHaveProperty('unlisted');
});

it('marks a contact unlisted from the core\'s failed verdict or a leaf verdict the core refused with, never from the count', async () => {
  const w = world();
  await toReady(w);
  w.core.trustState.mockReturnValue(strangerTrust(null));
  w.core.pendingLeaves.mockReturnValue([{ groupId: GROUP, leafIndex: 1, userId: STRANGER, cachedVersion: 1n, reason: 'ready-refresh' }]);
  // A count with nothing stored in the core says nothing about this contact.
  w.core.deviceListIngest.mockReturnValueOnce({ version: 1n, changed: false, quarantined: 1 });
  w.call(981, { m: 'refreshTrust' });
  await vi.waitFor(() => expect(w.ret(981)).toMatchObject({ ok: true }));
  expect(w.last<TrustSummary[]>('trust')?.[0]).not.toHaveProperty('unlisted');
  // The core's stored verdict does.
  w.core.trustState.mockReturnValue([{ ...strangerTrust(null)[0], blocked: 'E_TIER_MISMATCH' }]);
  w.core.trustGeneration.mockReturnValue(1n);
  w.sync.deps!.onTrustChanged();
  expect(w.last<TrustSummary[]>('trust')?.[0]).toMatchObject({ unlisted: true, blocked: 'E_TIER_MISMATCH', verified: false });
  w.core.trustState.mockReturnValue(strangerTrust(null));
  w.core.trustGeneration.mockReturnValue(2n);
  w.sync.deps!.onTrustChanged();
  expect(w.last<TrustSummary[]>('trust')?.[0]).toEqual({ userId: toHex(STRANGER), name: expect.any(String) as string, shared: false,
    umk: toHex(STRANGER_UMK), firstSeen: 1, verified: false, changedUmk: null });
  // A leaf verdict the ingest threw is stored nowhere in the core: the worker keeps it until a clean ingest.
  w.core.pendingLeaves.mockReturnValue([{ groupId: GROUP, leafIndex: 3, userId: STRANGER, cachedVersion: 1n, reason: 'ready-refresh' }]);
  w.core.deviceListIngest.mockImplementationOnce(() => { throw new CoreError('E_DEVICE_UNLISTED', 'leaf not on the list'); });
  w.call(983, { m: 'refreshTrust' });
  await vi.waitFor(() => expect(w.ret(983)).toMatchObject({ ok: true }));
  expect(w.last<TrustSummary[]>('trust')?.[0]).toMatchObject({ unlisted: true });
  w.core.pendingLeaves.mockReturnValue([{ groupId: GROUP, leafIndex: 4, userId: STRANGER, cachedVersion: 1n, reason: 'ready-refresh' }]);
  w.core.deviceListIngest.mockReturnValueOnce({ version: 1n, changed: false, quarantined: 0 });
  w.call(984, { m: 'refreshTrust' });
  await vi.waitFor(() => expect(w.ret(984)).toMatchObject({ ok: true }));
  expect(w.last<TrustSummary[]>('trust')?.[0]).not.toHaveProperty('unlisted');
});

// Item 6: trust_verify releases held messages at the same seq and moves the generation; the open timeline is read again.
it('re-reads an open timeline after a verify so the released held messages show', async () => {
  const w = world();
  await toReady(w);
  w.state.rows = [{ ...strangerRow(1), status: 1, reason: 'E_UMK_CHANGED', body: '' }];
  await openGeneral(w);
  w.state.groups = [textGroup(2)];
  w.core.trustState.mockReturnValue(strangerTrust(CHANGED_UMK));
  w.routes.listMembers.mockResolvedValue([{ userId: STRANGER, username: 's', display: 'S', kind: 0 as const, nick: '', roleIds: [] }]);
  w.call(9790, { m: 'selectCommunity', communityId: toHex(COMMUNITY) });
  await vi.waitFor(() => expect(w.ret(9790)).toMatchObject({ ok: true }));
  expect(w.timeline()?.items[0]?.body).toBe('');
  w.core.trustVerify.mockImplementation(() => {
    w.state.rows = [strangerRow(1)];
    w.core.trustGeneration.mockReturnValue(9n);
  });
  w.call(9791, { m: 'verifyContact', userId: toHex(STRANGER), umk: toHex(CHANGED_UMK) });
  await vi.waitFor(() => expect(w.ret(9791)).toMatchObject({ ok: true }));
  expect(w.timeline()?.items[0]?.body).toBe('from a stranger 1');
});

// Item 4: the safety number is bound to the key the page compares (ABI 8 safety_number_for(user, expected_umk)).
it('passes the compared key to the core\'s safety number, and refuses a malformed one', async () => {
  const w = world();
  w.state.dms = [{ channelId: DM_CHANNEL, kind: 3, members: [USER, STRANGER] }];
  await toReady(w);
  w.state.groups = [{ ...textGroup(2), groupId: DM_GROUP, communityId: null, targetId: DM_CHANNEL }];
  w.core.trustState.mockReturnValue(strangerTrust(CHANGED_UMK));
  w.call(9792, { m: 'safetyNumber', userId: toHex(STRANGER), umk: toHex(CHANGED_UMK) });
  await vi.waitFor(() => expect(w.ret(9792)).toMatchObject({ ok: true }));
  expect(w.core.safetyNumberFor).toHaveBeenLastCalledWith(STRANGER, CHANGED_UMK);
  w.core.safetyNumberFor.mockImplementationOnce(() => { throw new CoreError('E_DEVICE_LIST_STALE', 'not the comparison key'); });
  w.call(9793, { m: 'safetyNumber', userId: toHex(STRANGER), umk: toHex(STRANGER_UMK) });
  await vi.waitFor(() => expect(w.ret(9793)).toMatchObject({ ok: false, error: { code: 'E_DEVICE_LIST_STALE', detail: '' } }));
  // Wave C2: the page always names the key it compares; a command without one is refused, never bound to the core's key.
  const calls = w.core.safetyNumberFor.mock.calls.length;
  w.call(9794, { m: 'safetyNumber', userId: toHex(STRANGER) });
  await vi.waitFor(() => expect(w.ret(9794)).toMatchObject({ ok: false, error: { code: 'E_BAD_INPUT' } }));
  expect(w.core.safetyNumberFor.mock.calls.length).toBe(calls);
  w.call(9795, { m: 'safetyNumber', userId: toHex(STRANGER), umk: 'AB' });
  await vi.waitFor(() => expect(w.ret(9795)).toMatchObject({ ok: false, error: { code: 'E_BAD_INPUT' } }));
});

// Item 5: the per-group send block (send_block_code) for the composer, by channel; no group id reaches the page.
it('publishes the core\'s send block per channel and re-reads it on every trust generation change', async () => {
  const w = world();
  w.state.dms = [{ channelId: DM_CHANNEL, kind: 3, members: [USER, STRANGER] }];
  await toReady(w);
  await openGeneral(w);
  w.state.groups = [textGroup(2), { ...textGroup(2), groupId: DM_GROUP, communityId: null, targetId: DM_CHANNEL }];
  w.core.sendBlockCode.mockImplementation((g: Uint8Array) => (toHex(g) === toHex(GROUP) ? 'E_UMK_CHANGED' : null));
  w.core.trustGeneration.mockReturnValue(1n);
  w.sync.deps!.onTrustChanged();
  expect(w.last('sendBlocks')).toEqual({ [toHex(CHANNEL)]: { code: 'E_UMK_CHANGED', own: false } });
  expect(JSON.stringify(w.last('sendBlocks'))).not.toContain(toHex(GROUP));
  // R1: a block held only by own-user leaves is the own account's, so the composer names Devices, not a contact.
  w.core.sendBlockOwn.mockImplementation((g: Uint8Array) => toHex(g) === toHex(GROUP));
  w.core.trustGeneration.mockReturnValue(4n);
  w.sync.deps!.onTrustChanged();
  expect(w.last('sendBlocks')).toEqual({ [toHex(CHANNEL)]: { code: 'E_UMK_CHANGED', own: true } });
  w.core.sendBlockCode.mockImplementation((g: Uint8Array) => (toHex(g) === toHex(DM_GROUP) ? 'E_DEVICE_UNLISTED' : null));
  w.core.sendBlockOwn.mockImplementation(() => false);
  w.core.trustGeneration.mockReturnValue(2n);
  w.sync.deps!.onTrustChanged();
  expect(w.last('sendBlocks')).toEqual({ [toHex(DM_CHANNEL)]: { code: 'E_DEVICE_UNLISTED', own: false } });
  w.core.sendBlockCode.mockImplementation(() => null);
  w.core.trustGeneration.mockReturnValue(3n);
  w.sync.deps!.onTrustChanged();
  expect(w.last('sendBlocks')).toEqual({});
});

// Flow 05 "history pending": while the list read is not complete the contact shows privacy.unlisted with no Verify,
// because a comparison then would approve only the cached list.
it('keeps a contact history-pending while its list read fails or its history stays unknown', async () => {
  const w = world();
  await toReady(w);
  w.core.trustState.mockReturnValue(strangerTrust(null));
  w.core.pendingLeaves.mockReturnValue([{ groupId: GROUP, leafIndex: 1, userId: STRANGER, cachedVersion: 1n, reason: 'ready-refresh' }]);
  w.routes.getDeviceList.mockRejectedValueOnce(refusal(503, 'E_UNAVAILABLE'));
  w.call(984, { m: 'refreshTrust' });
  await vi.waitFor(() => expect(w.ret(984)).toMatchObject({ ok: true }));
  expect(w.core.deviceListIngest).not.toHaveBeenCalled();
  expect(w.last<TrustSummary[]>('trust')?.[0]).toMatchObject({ userId: toHex(STRANGER), unlisted: true });
  // The failed leaf is not read again before the next ready; it stays pending meanwhile.
  w.call(9841, { m: 'refreshTrust' });
  await vi.waitFor(() => expect(w.ret(9841)).toMatchObject({ ok: true }));
  expect(w.last<TrustSummary[]>('trust')?.[0]).toMatchObject({ unlisted: true });
  // A complete read and a clean ingest end it.
  w.core.pendingLeaves.mockReturnValue([{ groupId: GROUP, leafIndex: 2, userId: STRANGER, cachedVersion: 1n, reason: 'ready-refresh' }]);
  w.core.deviceListIngest.mockReturnValueOnce({ version: 1n, changed: false, quarantined: 0 });
  w.call(9842, { m: 'refreshTrust' });
  await vi.waitFor(() => expect(w.ret(9842)).toMatchObject({ ok: true }));
  expect(w.last<TrustSummary[]>('trust')?.[0]).not.toHaveProperty('unlisted');
  // A leaf whose history the core still calls unknown (the ingest did not complete) is pending too.
  w.core.pendingLeaves.mockReturnValue([{ groupId: GROUP, leafIndex: 3, userId: STRANGER, cachedVersion: 0n, reason: 'unknown' }]);
  w.core.deviceListIngest.mockImplementationOnce(() => { throw new CoreError('E_DEVICE_LIST_STALE', 'older list'); });
  w.call(9843, { m: 'refreshTrust' });
  await vi.waitFor(() => expect(w.ret(9843)).toMatchObject({ ok: true }));
  expect(w.last<TrustSummary[]>('trust')?.[0]).toMatchObject({ unlisted: true });
});

it('names the channel whose validated Remove dropped a changed leaf, and only while the key stays changed', async () => {
  const w = world();
  await toReady(w);
  await openGeneral(w);
  w.core.trustState.mockReturnValue(strangerTrust(CHANGED_UMK));
  w.core.pendingLeaves.mockReturnValue([{ groupId: GROUP, leafIndex: 1, userId: STRANGER, cachedVersion: 2n, reason: 'changed' }]);
  w.call(985, { m: 'refreshTrust' });
  await vi.waitFor(() => expect(w.ret(985)).toMatchObject({ ok: true }));
  expect(w.last<TrustSummary[]>('trust')?.[0]).toMatchObject({ changedUmk: toHex(CHANGED_UMK) });
  expect(w.last<TrustSummary[]>('trust')?.[0]).not.toHaveProperty('removedIn');
  w.core.pendingLeaves.mockReturnValue([]);
  w.call(986, { m: 'refreshTrust' });
  await vi.waitFor(() => expect(w.ret(986)).toMatchObject({ ok: true }));
  const removed = w.last<TrustSummary[]>('trust')?.[0];
  expect(removed).toMatchObject({ changedUmk: toHex(CHANGED_UMK), verified: false, removedIn: [toHex(CHANNEL)] });
  expect(JSON.stringify(w.last('trust'))).not.toContain(toHex(GROUP));
  w.core.trustState.mockReturnValue([{ ...strangerTrust(null)[0], umk: CHANGED_UMK, verified: true }]);
  w.call(987, { m: 'refreshTrust' });
  await vi.waitFor(() => expect(w.ret(987)).toMatchObject({ ok: true }));
  expect(w.last<TrustSummary[]>('trust')?.[0]).not.toHaveProperty('removedIn');
});

it('does not call a group removed when this device left it', async () => {
  const w = world();
  await toReady(w);
  await openGeneral(w);
  w.core.trustState.mockReturnValue(strangerTrust(CHANGED_UMK));
  w.core.pendingLeaves.mockReturnValue([{ groupId: GROUP, leafIndex: 1, userId: STRANGER, cachedVersion: 2n, reason: 'changed' }]);
  w.call(988, { m: 'refreshTrust' });
  await vi.waitFor(() => expect(w.ret(988)).toMatchObject({ ok: true }));
  w.state.groups = [textGroup(4)];
  w.core.pendingLeaves.mockReturnValue([]);
  w.call(989, { m: 'refreshTrust' });
  await vi.waitFor(() => expect(w.ret(989)).toMatchObject({ ok: true }));
  expect(w.last<TrustSummary[]>('trust')?.[0]).not.toHaveProperty('removedIn');
});

it('stops typing on lost membership and refuses typing until viewable and online', async () => {
  const w = world();
  await toReady(w);
  await openGeneral(w);
  w.call(997, { m: 'typing', channelId: toHex(CHANNEL), on: true });
  await vi.waitFor(() => expect(w.ret(997)).toMatchObject({ ok: false, error: { code: 'E_NOT_READY', detail: '' } }));
  expect(w.gateway.sendTyping).not.toHaveBeenCalled();
  w.gateway.emit({ type: 'status', status: 'ready', closeCode: null });
  w.call(994, { m: 'typing', channelId: toHex(CHANNEL), on: true });
  await vi.waitFor(() => expect(w.ret(994)).toMatchObject({ ok: true }));
  w.sync.deps!.onMembership(GROUP, 'not-member');
  expect(w.gateway.sendTyping).toHaveBeenLastCalledWith(CHANNEL, false);
  w.call(995, { m: 'typing', channelId: toHex(CHANNEL), on: true });
  await vi.waitFor(() => expect(w.ret(995)).toMatchObject({ ok: false, error: { code: 'E_NOT_READY', detail: '' } }));
  expect(w.gateway.sendTyping.mock.calls.filter((args) => args[1] === true)).toHaveLength(1);
});

// WB product walk I1: the page's typing-off for the channel it just left reaches the worker after closeChannel; an off with
// no signal on is a no-op that resolves (closeChannel, a membership loss or the link's end already sent it), never E_NOT_READY.
it('a typing off for a channel with no signal on resolves and sends nothing', async () => {
  const w = world();
  await toReady(w);
  await openGeneral(w);
  w.gateway.emit({ type: 'status', status: 'ready', closeCode: null });
  w.call(981, { m: 'typing', channelId: toHex(CHANNEL), on: true });
  await vi.waitFor(() => expect(w.ret(981)).toMatchObject({ ok: true }));
  w.call(982, { m: 'closeChannel', channelId: toHex(CHANNEL) });
  await vi.waitFor(() => expect(w.ret(982)).toMatchObject({ ok: true }));
  expect(w.gateway.sendTyping).toHaveBeenCalledTimes(2);
  w.call(983, { m: 'typing', channelId: toHex(CHANNEL), on: false });
  await vi.waitFor(() => expect(w.ret(983)).toMatchObject({ ok: true }));
  expect(w.gateway.sendTyping).toHaveBeenCalledTimes(2);
  // A channel never opened, and an off while offline, resolve the same way.
  w.call(984, { m: 'typing', channelId: 'ab'.repeat(16), on: false });
  await vi.waitFor(() => expect(w.ret(984)).toMatchObject({ ok: true }));
  // An on for a closed channel is still refused.
  w.call(985, { m: 'typing', channelId: toHex(CHANNEL), on: true });
  await vi.waitFor(() => expect(w.ret(985)).toMatchObject({ ok: false, error: { code: 'E_NOT_READY' } }));
  expect(w.gateway.sendTyping).toHaveBeenCalledTimes(2);
});

it('ignores incoming typing frames after an open channel loses membership', async () => {
  const w = world();
  await toReady(w);
  await openGeneral(w);
  w.gateway.emit({ type: 'status', status: 'ready', closeCode: null });
  w.call(998, { m: 'typing', channelId: toHex(CHANNEL), on: true });
  await vi.waitFor(() => expect(w.ret(998)).toMatchObject({ ok: true }));
  const frame: GatewayEvent = { type: 'frame', frame: {
    op: 49, n: 0n, groupId: null, payload: [CHANNEL, STRANGER, OTHER_DEVICE, 1n, NOW_S + 5n],
  } };
  w.gateway.emit(frame);
  expect(w.last<{ userId: string }[]>(`typing:${toHex(CHANNEL)}`)).toMatchObject([{ userId: toHex(STRANGER) }]);

  w.sync.deps!.onMembership(GROUP, 'not-member');
  expect(w.last<{ userId: string }[]>(`typing:${toHex(CHANNEL)}`)).toEqual([]);
  w.gateway.emit(frame);
  expect(w.last<{ userId: string }[]>(`typing:${toHex(CHANNEL)}`)).toEqual([]);
});

// expires_ts is on the instance's clock (L-HTTP-96: now + 10 s there). Read against this browser's clock, a lagging
// instance made every indicator expire on arrival (task 10: the test host's clock lags, so no browser ever showed one).
it('reads a typing expiry on the instance clock and holds it at most the relay window here', async () => {
  const w = world();
  await toReady(w);
  await openGeneral(w);
  w.routes.postTicket.mockImplementationOnce(() => Promise.resolve({ ticket: 'ticket', expires: NOW_S - 600n + 30n }));
  expect(await w.gatewayDeps.value!.mintTicket()).toBe('ticket');
  w.gateway.emit({ type: 'status', status: 'ready', closeCode: null });
  w.gateway.emit({ type: 'frame', frame: {
    op: 49, n: 0n, groupId: null, payload: [CHANNEL, STRANGER, OTHER_DEVICE, 1n, NOW_S - 600n + 10n],
  } });
  expect(w.last<{ userId: string; expiresAt: number }[]>(`typing:${toHex(CHANNEL)}`))
    .toMatchObject([{ userId: toHex(STRANGER), expiresAt: 1_700_000_010_000 }]);
  // Already over on the instance clock: dropped.
  w.gateway.emit({ type: 'frame', frame: {
    op: 49, n: 0n, groupId: null, payload: [CHANNEL, new Uint8Array(16).fill(0x6b), OTHER_DEVICE, 1n, NOW_S - 600n],
  } });
  expect(w.last<{ userId: string }[]>(`typing:${toHex(CHANNEL)}`)).toMatchObject([{ userId: toHex(STRANGER) }]);
});

// The ticket shows the instance clock once, at the mint. Time passes on both clocks after it, so the instance's now is the
// mint time plus what elapsed here; a frame whose expires_ts is already behind that estimate is stale and never shown.
it('advances the instance clock from the ticket by the time elapsed here, and drops a frame already expired on it', async () => {
  let nowMs = 1_700_000_000_000;
  const w = world({ now: () => nowMs });
  await toReady(w);
  await openGeneral(w);
  // The instance is ten minutes behind this browser at the mint.
  w.routes.postTicket.mockImplementationOnce(() => Promise.resolve({ ticket: 'ticket', expires: NOW_S - 600n + 30n }));
  expect(await w.gatewayDeps.value!.mintTicket()).toBe('ticket');
  w.gateway.emit({ type: 'status', status: 'ready', closeCode: null });
  nowMs += 20_000;                                            // 20 s later on both clocks: the instance is at mint + 20
  // Expired 5 s ago on the instance clock (mint + 15): dropped, never held for a fresh window.
  w.gateway.emit({ type: 'frame', frame: {
    op: 49, n: 0n, groupId: null, payload: [CHANNEL, new Uint8Array(16).fill(0x6b), OTHER_DEVICE, 1n, NOW_S - 600n + 15n],
  } });
  expect(w.last<{ userId: string }[]>(`typing:${toHex(CHANNEL)}`) ?? []).toEqual([]);
  // Four seconds left on the instance clock (mint + 24): shown for those four seconds here.
  w.gateway.emit({ type: 'frame', frame: {
    op: 49, n: 0n, groupId: null, payload: [CHANNEL, STRANGER, OTHER_DEVICE, 1n, NOW_S - 600n + 24n],
  } });
  expect(w.last<{ userId: string; expiresAt: number }[]>(`typing:${toHex(CHANNEL)}`)?.map(row => [row.userId, row.expiresAt]))
    .toEqual([[toHex(STRANGER), 1_700_000_024_000]]);
});

// A timer can fire a millisecond before Date.now() reaches the deadline it was set for (Firefox does, under load). The
// expiry sweep then finds the row not yet over; it must look again at the deadline, or the row outlives its window for good
// (CI run 39828967265: the indicator was still up 90 s after the signal's expiry).
it('drops a typing row whose expiry timer fired a millisecond early', async () => {
  vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
  try {
    let nowMs = 1_700_000_000_000;
    const w = world({ now: () => nowMs });
    await toReady(w);
    await openGeneral(w);
    w.gateway.emit({ type: 'status', status: 'ready', closeCode: null });
    w.gateway.emit({ type: 'frame', frame: {
      op: 49, n: 0n, groupId: null, payload: [CHANNEL, STRANGER, OTHER_DEVICE, 1n, NOW_S + 4n],
    } });
    const rows = (): unknown[] => w.last<unknown[]>(`typing:${toHex(CHANNEL)}`) ?? [];
    expect(rows()).toHaveLength(1);
    nowMs += 3_999;                                           // the 4 s timer fires with 1 ms still to go on the clock
    vi.advanceTimersByTime(4_000);
    nowMs += 1;
    vi.advanceTimersByTime(1);
    expect(rows()).toEqual([]);
  } finally { vi.useRealTimers(); }
});

// One sweep timer per channel: a member who floods typing frames must not pile timers up in every reader's worker.
describe('incoming typing keeps one sweep timer per channel', () => {
  const user = (n: number): Uint8Array => new Uint8Array(16).fill(n);
  const typingFrame = (who: Uint8Array, expires: bigint): GatewayEvent => ({ type: 'frame', frame: {
    op: 49, n: 0n, groupId: null, payload: [CHANNEL, who, OTHER_DEVICE, 1n, expires],
  } });
  const ready = async (nowMs: () => number) => {
    const w = world({ now: nowMs });
    await toReady(w);
    await openGeneral(w);
    w.gateway.emit({ type: 'status', status: 'ready', closeCode: null });
    return w;
  };
  const names = (w: ReturnType<typeof world>): string[] =>
    (w.last<{ userId: string }[]>(`typing:${toHex(CHANNEL)}`) ?? []).map(row => row.userId);

  it('1000 frames in one channel leave exactly one pending timer', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    try {
      const nowMs = 1_700_000_000_000;
      const w = await ready(() => nowMs);
      const before = vi.getTimerCount();
      for (let i = 0; i < 1000; i += 1) w.gateway.emit(typingFrame(user(1 + (i % 200)), NOW_S + 5n));
      expect(names(w).length).toBeGreaterThan(100);
      expect(vi.getTimerCount() - before).toBe(1);
    } finally { vi.useRealTimers(); }
  });

  it('a later frame with an earlier deadline moves the timer forward', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    try {
      let nowMs = 1_700_000_000_000;
      const w = await ready(() => nowMs);
      const before = vi.getTimerCount();
      w.gateway.emit(typingFrame(user(1), NOW_S + 8n));
      w.gateway.emit(typingFrame(user(2), NOW_S + 3n));
      expect(vi.getTimerCount() - before).toBe(1);
      nowMs += 3_000; vi.advanceTimersByTime(3_000);
      expect(names(w)).toEqual([toHex(user(1))]);
      expect(vi.getTimerCount() - before).toBe(1);          // re-armed for the remaining row
      nowMs += 5_000; vi.advanceTimersByTime(5_000);
      expect(names(w)).toEqual([]);
      expect(vi.getTimerCount() - before).toBe(0);
    } finally { vi.useRealTimers(); }
  });

  // Only an earlier deadline may move the timer. Always re-arming to the latest frame would leave the earlier row on
  // screen until the later one is due.
  it('a later or equal deadline keeps the pending timer and does not re-arm', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    try {
      let nowMs = 1_700_000_000_000;
      const w = await ready(() => nowMs);
      const before = vi.getTimerCount();
      const armed = vi.spyOn(globalThis, 'setTimeout');
      w.gateway.emit(typingFrame(user(1), NOW_S + 3n));
      expect(armed).toHaveBeenCalledTimes(1);
      w.gateway.emit(typingFrame(user(2), NOW_S + 8n));       // later
      w.gateway.emit(typingFrame(user(3), NOW_S + 3n));       // equal
      expect(armed).toHaveBeenCalledTimes(1);
      expect(vi.getTimerCount() - before).toBe(1);
      nowMs += 3_000; vi.advanceTimersByTime(3_000);
      expect(names(w)).toEqual([toHex(user(2))]);             // the 3 s rows went on time, the 8 s one stays
      expect(vi.getTimerCount() - before).toBe(1);
      armed.mockRestore();
    } finally { vi.useRealTimers(); }
  });

  it('a stop frame for a channel with no rows arms nothing', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    try {
      const w = await ready(() => 1_700_000_000_000);
      const before = vi.getTimerCount();
      w.gateway.emit({ type: 'frame', frame: { op: 49, n: 0n, groupId: null, payload: [CHANNEL, user(1), OTHER_DEVICE, 0n, NOW_S + 3n] } });
      expect(vi.getTimerCount() - before).toBe(0);
    } finally { vi.useRealTimers(); }
  });

  it('losing the channel view clears the pending timer', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    try {
      const w = await ready(() => 1_700_000_000_000);
      const before = vi.getTimerCount();
      w.gateway.emit(typingFrame(user(1), NOW_S + 8n));
      expect(vi.getTimerCount() - before).toBe(1);
      w.sync.deps!.onMembership(GROUP, 'not-member');
      expect(vi.getTimerCount() - before).toBe(0);
    } finally { vi.useRealTimers(); }
  });

  it('a worker reset (sign-out wipe) clears the pending timer', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    try {
      const w = await ready(() => 1_700_000_000_000);
      w.gateway.emit(typingFrame(user(1), NOW_S + 8n));
      w.call(961, { m: 'signOutRevoke', recoveryKey: RECOVERY_KEY });
      await vi.waitFor(() => expect(w.ret(961)).toMatchObject({ ok: true }));
      expect(w.account()?.phase).toBe('cleared');
      expect(vi.getTimerCount()).toBe(0);
    } finally { vi.useRealTimers(); }
  });

  it('an earlier row is swept on time when a later deadline arrives after it', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    try {
      let nowMs = 1_700_000_000_000;
      const w = await ready(() => nowMs);
      const before = vi.getTimerCount();
      w.gateway.emit(typingFrame(user(1), NOW_S + 3n));
      w.gateway.emit(typingFrame(user(2), NOW_S + 8n));
      nowMs += 3_000; vi.advanceTimersByTime(3_000);
      expect(names(w)).toEqual([toHex(user(2))]);
      expect(vi.getTimerCount() - before).toBe(1);
    } finally { vi.useRealTimers(); }
  });

  it('closing the channel clears the pending timer', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    try {
      const nowMs = 1_700_000_000_000;
      const w = await ready(() => nowMs);
      const before = vi.getTimerCount();
      w.gateway.emit(typingFrame(user(1), NOW_S + 8n));
      expect(vi.getTimerCount() - before).toBe(1);
      w.call(960, { m: 'closeChannel', channelId: toHex(CHANNEL) });
      await vi.waitFor(() => expect(w.ret(960)).toMatchObject({ ok: true }));
      expect(vi.getTimerCount() - before).toBe(0);
    } finally { vi.useRealTimers(); }
  });
});

it('clears incoming typing on membership loss without local typing', async () => {
  const w = world();
  await toReady(w);
  await openGeneral(w);
  w.gateway.emit({ type: 'status', status: 'ready', closeCode: null });
  w.gateway.emit({ type: 'frame', frame: {
    op: 49, n: 0n, groupId: null, payload: [CHANNEL, STRANGER, OTHER_DEVICE, 1n, NOW_S + 5n],
  } });
  expect(w.last<{ userId: string }[]>(`typing:${toHex(CHANNEL)}`)).toMatchObject([{ userId: toHex(STRANGER) }]);
  expect(w.gateway.sendTyping).not.toHaveBeenCalled();

  w.sync.deps!.onMembership(GROUP, 'not-member');
  expect(w.last<{ userId: string }[]>(`typing:${toHex(CHANNEL)}`)).toEqual([]);
  expect(w.gateway.sendTyping).not.toHaveBeenCalled();
});

it('stops typing when the open channel disappears from the refreshed channel list', async () => {
  const w = world();
  await toReady(w);
  await openGeneral(w);
  w.gateway.emit({ type: 'status', status: 'ready', closeCode: null });
  w.call(996, { m: 'typing', channelId: toHex(CHANNEL), on: true });
  await vi.waitFor(() => expect(w.ret(996)).toMatchObject({ ok: true }));
  w.routes.listChannels.mockResolvedValueOnce([]);
  w.gateway.emit({ type: 'ready', info: READY_INFO });
  await vi.waitFor(() => expect(w.channels()).toEqual([]));
  expect(w.gateway.sendTyping).toHaveBeenLastCalledWith(CHANNEL, false);
});

it('rejects a search cursor with no worker-owned pagination state and never calls core search', async () => {
  const w = world();
  await toReady(w);
  w.call(91, { m: 'search', scope: 'all', id: null, query: 'boat',
    cursor: { token: '1' } });
  await vi.waitFor(() => expect(w.ret(91)).toBeDefined());
  expect(w.ret(91)).toMatchObject({ ok: false, error: { code: 'E_CORE_INPUT', detail: '' } });
  expect(w.core.search).not.toHaveBeenCalled();
});

it('refuses malformed key approval and sends typing off on channel close', async () => {
  const w = world();
  await toReady(w);
  await openGeneral(w);
  w.gateway.emit({ type: 'status', status: 'ready', closeCode: null });
  w.call(94, { m: 'verifyContact', userId: toHex(STRANGER), umk: 'not a public key' });
  await vi.waitFor(() => expect(w.ret(94)).toBeDefined());
  expect(w.ret(94)).toMatchObject({ ok: false, error: { code: 'E_BAD_INPUT', detail: '' } });
  expect(w.core.trustVerify).not.toHaveBeenCalled();
  w.call(95, { m: 'typing', channelId: toHex(CHANNEL), on: true });
  await vi.waitFor(() => expect(w.ret(95)).toBeDefined());
  expect(w.gateway.sendTyping).toHaveBeenCalledWith(CHANNEL, true);
  w.call(96, { m: 'closeChannel', channelId: toHex(CHANNEL) });
  await vi.waitFor(() => expect(w.ret(96)).toMatchObject({ ok: true }));
  expect(w.gateway.sendTyping).toHaveBeenCalledWith(CHANNEL, false);
});

it('keeps the old lock and gives no release notice when pause fails', async () => {
  const w = world();
  await toReady(w);
  w.core.pause.mockImplementation(() => { throw new CoreError('E_STORE_BUSY', 'SECRET busy'); });
  w.call(92, { m: 'resign', nonce: 'aa'.repeat(16) });
  await vi.waitFor(() => expect(w.ret(92)).toBeDefined());
  expect(w.ret(92)).toMatchObject({ ok: false, error: { code: 'E_STORE_BUSY', detail: '' } });
  expect(w.posted.some((m) => m.t === 'released')).toBe(false);
  expect(w.locks.isHeld(LOCK)).toBe(true);
  expect(w.core.resume).toHaveBeenCalled();
  expect(w.calls.indexOf('gateway.stop')).toBeLessThan(w.calls.lastIndexOf('gateway.start'));
});

it('a resign whose rollback fails too gives up the lock, so the asking tab leads (M1)', async () => {
  const w = world();
  await toReady(w);
  w.core.pause.mockImplementationOnce(() => { throw new CoreError('E_STORE_BUSY', ''); });
  w.core.resume.mockImplementationOnce(() => Promise.reject(new CoreError('E_STORE_BUSY', '')));
  const tabB = otherTab(w);
  w.call(92, { m: 'resign', nonce: 'aa'.repeat(16) });
  await vi.waitFor(() => expect(w.ret(92)).toBeDefined());
  expect(w.ret(92)).toMatchObject({ ok: false });
  expect(w.account()).toMatchObject({ phase: 'paused-error', error: { code: 'E_STORE_BUSY' } });
  await tabB.granted;
  // And this tab waits behind it, as a released one does.
  tabB.release();
  await vi.waitFor(() => expect(w.account()?.phase).toBe('ready'));
});

it('a resign rolled back after a failed pause gives the fresh engine its expected groups before the socket starts (M2)', async () => {
  const w = world();
  await toReady(w);
  w.core.pause.mockImplementationOnce(() => { throw new CoreError('E_STORE_BUSY', ''); });
  const builds = w.sync.builds;
  w.sync.setExpected.mockClear();
  w.call(92, { m: 'resign', nonce: 'aa'.repeat(16) });
  await vi.waitFor(() => expect(w.ret(92)).toBeDefined());
  expect(w.sync.builds).toBe(builds + 1);
  expect(w.sync.setExpected).toHaveBeenCalledWith([expect.objectContaining({ groupId: GROUP })]);
});

/** Tab B's worker: a blocking lock request queued behind the current holder; `release` is B resigning. */
function otherTab(w: World): { granted: Promise<void>; release: () => void } {
  let release: () => void = () => {};
  const granted = new Promise<void>((resolve) => {
    void w.locks.request(LOCK, {}, () => { resolve(); return new Promise<void>((done) => { release = done; }); });
  });
  return { granted, release: () => { release(); } };
}

// Wave C2, found by the browser hand-back (A -> B -> A): the OPFS pool lives in this worker and stays paused after a
// release, and a fresh open here finds that paused pool and fails ("unable to open database file"). A tab that leads
// again resumes the store it paused instead, also when its rollback had failed; a resume that fails falls back to boot.
it('leads again by resuming the store it paused, not by opening it again', async () => {
  for (const rollbackFails of [false, true]) {
    const w = world();
    await toReady(w);
    if (rollbackFails) {
      w.core.pause.mockImplementationOnce(() => { throw new CoreError('E_STORE_BUSY', ''); });
      w.core.resume.mockImplementationOnce(() => Promise.reject(new CoreError('E_STORE_BUSY', '')));
    }
    const boots = w.boots;
    const tabB = otherTab(w);
    w.call(92, { m: 'resign', nonce: 'aa'.repeat(16) });
    await vi.waitFor(() => expect(w.ret(92)).toBeDefined());
    await tabB.granted;
    expect(w.core.close).not.toHaveBeenCalled();
    const resumes = w.core.resume.mock.calls.length;
    tabB.release();
    await vi.waitFor(() => expect(w.account()?.phase).toBe('ready'));
    expect(w.core.resume.mock.calls.length, `rollback fails: ${String(rollbackFails)}`).toBe(resumes + 1);
    expect(w.boots).toBe(boots);
  }
});

it('falls back to a fresh open when the paused store cannot resume', async () => {
  const w = world();
  await toReady(w);
  const boots = w.boots;
  const tabB = otherTab(w);
  w.call(92, { m: 'resign', nonce: 'aa'.repeat(16) });
  await vi.waitFor(() => expect(w.ret(92)).toMatchObject({ ok: true }));
  await tabB.granted;
  w.core.resume.mockImplementationOnce(() => Promise.reject(new CoreError('E_STORE_RESUME', '')));
  tabB.release();
  await vi.waitFor(() => expect(w.account()?.phase).toBe('ready'));
  expect(w.core.close).toHaveBeenCalledOnce();
  expect(w.boots).toBe(boots + 1);
});

it('a tab that resigned leads again when the new leader hands back, with a fresh engine (A -> B -> A -> B)', async () => {
  const w = world();
  await toReady(w);
  const tabB = otherTab(w);
  w.call(92, { m: 'resign', nonce: 'aa'.repeat(16) });
  await vi.waitFor(() => expect(w.ret(92)).toMatchObject({ ok: true }));
  await tabB.granted;
  expect(w.account()?.phase).toBe('other-tab');
  // The paused store stays with this worker, to be resumed when it leads again.
  expect(w.core.close).not.toHaveBeenCalled();
  const builds = w.sync.builds;
  const starts = w.sync.start.mock.calls.length;
  const gatewayStarts = w.calls.filter((c) => c === 'gateway.start').length;
  // "Use this tab" on A: B resigns, and A's page calls start, which only waits for the lock A already asked for.
  tabB.release();
  w.call(93, { m: 'start' });
  await vi.waitFor(() => expect(w.account()?.phase).toBe('ready'));
  expect(w.ret(93)).toMatchObject({ ok: true });
  expect(w.locks.isHeld(LOCK)).toBe(true);
  expect(w.sync.builds).toBe(builds + 1);
  expect(w.sync.start.mock.calls.length).toBe(starts + 1);
  expect(w.calls.filter((c) => c === 'gateway.start').length).toBe(gatewayStarts + 1);
  // And back to B once more.
  const again = otherTab(w);
  w.call(94, { m: 'resign', nonce: 'bb'.repeat(16) });
  await vi.waitFor(() => expect(w.ret(94)).toMatchObject({ ok: true }));
  await again.granted;
  expect(w.account()?.phase).toBe('other-tab');
  again.release();
  await vi.waitFor(() => expect(w.account()?.phase).toBe('ready'));
});

it('a tab that leads again rebuilds from the store: nothing of the account before carries over (B switched account)', async () => {
  const w = world();
  await toReady(w);
  await openGeneral(w);
  expect(w.last<MemberSummary[]>(`members:${toHex(COMMUNITY)}`)).not.toEqual([]);
  // The old account's state: a live message notice, and a contact the core refused by a leaf verdict.
  w.core.trustState.mockReturnValue(strangerTrust(null));
  w.core.pendingLeaves.mockReturnValue([{ groupId: GROUP, leafIndex: 1, userId: STRANGER, cachedVersion: 1n, reason: 'ready-refresh' }]);
  w.core.deviceListIngest.mockImplementation(() => { throw new CoreError('E_DEVICE_UNLISTED', ''); });
  w.call(96, { m: 'refreshTrust' });
  await vi.waitFor(() => expect(w.ret(96)).toMatchObject({ ok: true }));
  expect(w.last<TrustSummary[]>('trust')?.[0]).toMatchObject({ unlisted: true });
  w.core.pendingLeaves.mockReturnValue([]);
  w.state.groups = [textGroup(2)];
  w.state.rows = [{ ...strangerRow(1), recvTs: NOW_S + 61n }];
  w.state.activity = [act(GROUP, 1, 0)];
  w.sync.deps!.onGroupChanged(GROUP, appliedSeqs([1n]));
  expect(w.notices()?.items).toHaveLength(1);
  w.state.groups = []; w.state.rows = []; w.state.activity = [];
  const tabB = otherTab(w);
  w.call(92, { m: 'resign', nonce: 'aa'.repeat(16) });
  await vi.waitFor(() => expect(w.ret(92)).toMatchObject({ ok: true }));
  await tabB.granted;
  // Meanwhile tab B signed out and signed in to another account on this instance, in no community.
  const other = new Uint8Array(16).fill(0x77);
  w.core.identity = (): IdentityInfo => ({ phase: 2, instanceId: INSTANCE_ID, userId: other, deviceId: OTHER_DEVICE,
    username: 'other', listPublished: true });
  w.state.joined = false;
  tabB.release();
  await vi.waitFor(() => expect(w.account()?.phase).toBe('ready'));
  expect(w.account()?.user).toEqual({ id: toHex(other), username: 'other' });
  expect(w.last('communities')).toEqual([]);
  expect(w.last(`channels:${toHex(COMMUNITY)}`)).toEqual([]);
  expect(w.last(`members:${toHex(COMMUNITY)}`)).toEqual([]);
  expect(w.last<TimelineState>(`timeline:${toHex(CHANNEL)}`)?.items).toEqual([]);
  expect(w.notices()?.items).toEqual([]);
  // The same person is a contact of the new account too: what the old account's refresh said about them is gone.
  w.call(97, { m: 'refreshTrust' });
  await vi.waitFor(() => expect(w.ret(97)).toMatchObject({ ok: true }));
  expect(w.last<TrustSummary[]>('trust')?.[0]).toMatchObject({ userId: toHex(STRANGER) });
  expect(w.last<TrustSummary[]>('trust')?.[0]).not.toHaveProperty('unlisted');
  // The old account's channel is unknown to this one.
  w.call(95, { m: 'openChannel', channelId: toHex(CHANNEL) });
  await vi.waitFor(() => expect(w.ret(95)).toBeDefined());
  expect(w.ret(95)).toMatchObject({ ok: false, error: { code: 'E_BAD_INPUT' } });
});

it('resign waits for a DM reload spawned by an unexpected Welcome', async () => {
  const w = world();
  await toReady(w);
  const gate = deferred<World['state']['dms']>();
  w.routes.listDms.mockImplementationOnce(() => gate.promise);
  w.sync.deps!.onUnexpectedWelcome(GROUP);
  await vi.waitFor(() => expect(w.routes.listDms).toHaveBeenCalledTimes(2));
  w.call(92, { m: 'resign', nonce: 'aa'.repeat(16) });
  await settle();
  expect(w.core.pause).not.toHaveBeenCalled();
  gate.resolve([]);
  await vi.waitFor(() => expect(w.ret(92)).toMatchObject({ ok: true, value: { released: true } }));
});

it('resign waits for socket reauthentication before pausing the core', async () => {
  const w = world();
  await toReady(w);
  const gate = deferred<boolean>();
  w.session.establish.mockImplementationOnce(() => gate.promise);
  w.gateway.emit({ type: 'revoked' });
  await vi.waitFor(() => expect(w.session.establish).toHaveBeenCalled());
  w.call(92, { m: 'resign', nonce: 'aa'.repeat(16) });
  await settle();
  expect(w.core.pause).not.toHaveBeenCalled();
  gate.resolve(false);
  await vi.waitFor(() => expect(w.ret(92)).toMatchObject({ ok: true, value: { released: true } }));
});

it('a socket re-establish timer armed before a resign never starts a socket afterwards (client I4)', async () => {
  const w = world();
  await toReady(w);
  vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
  try {
    w.session.establish.mockImplementationOnce(() => Promise.reject(new Error('E_NETWORK')));
    w.gateway.emit({ type: 'revoked' });
    await vi.advanceTimersByTimeAsync(0);
    expect(w.session.establish).toHaveBeenCalled();
    const tabB = otherTab(w);
    w.call(92, { m: 'resign', nonce: 'aa'.repeat(16) });
    await vi.advanceTimersByTimeAsync(0);
    expect(w.ret(92)).toMatchObject({ ok: true });
    await tabB.granted;
    // B hands back at once: A leads again with a fresh socket, started once.
    tabB.release();
    for (let i = 0; i < 20 && w.account()?.phase !== 'ready'; i++) await vi.advanceTimersByTimeAsync(10);
    expect(w.account()?.phase).toBe('ready');
    const starts = w.calls.filter((c) => c === 'gateway.start').length;
    await vi.advanceTimersByTimeAsync(GATEWAY.reconnectCapMs);
    expect(w.calls.filter((c) => c === 'gateway.start').length).toBe(starts);
  } finally {
    vi.useRealTimers();
  }
});

// f10app m3: a timeline sender is authenticated by its group, so it still reads a member list whose first reads failed (only the
// unauthenticated voice frames leave an unloaded list to its load).
it('an unknown timeline sender reads the member list also when that list never loaded; a voice participant does not', async () => {
  const w = world();
  await toReady(w);
  await settle();
  // A community whose member list has not loaded here (no public command leaves one unloaded with its channels known, so
  // the lookup is driven directly).
  const unloaded = 'e'.repeat(32);
  const lookUp = (w.controller as unknown as { lookUpUsers(c: string, u: string[], f: 'timeline' | 'voice'): void }).lookUpUsers
    .bind(w.controller);
  const before = w.routes.listMembers.mock.calls.length;
  lookUp(unloaded, [toHex(STRANGER)], 'voice');
  await settle();
  expect(w.routes.listMembers.mock.calls.length).toBe(before);
  lookUp(unloaded, [toHex(STRANGER)], 'timeline');
  await vi.waitFor(() => expect(w.routes.listMembers.mock.calls.length).toBe(before + 1));
  expect(toHex(w.routes.listMembers.mock.calls.at(-1)![0])).toBe(unloaded);
});

it('resign waits for the member lookup started by an unknown timeline sender', async () => {
  const w = world();
  await toReady(w);
  await settle();
  await openGeneral(w);
  w.state.rows = [strangerRow(1)];
  const gate = deferred<Awaited<ReturnType<typeof w.routes.listMembers>>>();
  w.routes.listMembers.mockImplementationOnce(() => gate.promise);
  w.sync.deps!.onGroupChanged(GROUP, applied(2));
  await vi.waitFor(() => expect(w.routes.listMembers).toHaveBeenCalledTimes(3));
  w.call(92, { m: 'resign', nonce: 'aa'.repeat(16) });
  await settle();
  expect(w.core.pause).not.toHaveBeenCalled();
  gate.resolve([]);
  await vi.waitFor(() => expect(w.ret(92)).toMatchObject({ ok: true, value: { released: true } }));
});

it('resign waits for the trust refresh spawned by a group change', async () => {
  const w = world();
  await toReady(w);
  await settle();
  w.core.pendingLeaves.mockReturnValue([{ groupId: GROUP, leafIndex: 1, userId: STRANGER, cachedVersion: 0n, reason: 'unknown' }]);
  const gate = deferred<Awaited<ReturnType<typeof w.routes.getDeviceList>>>();
  w.routes.getDeviceList.mockImplementationOnce(() => gate.promise);
  w.sync.deps!.onGroupChanged(GROUP, applied(2));
  await vi.waitFor(() => expect(w.routes.getDeviceList).toHaveBeenCalled());
  w.call(92, { m: 'resign', nonce: 'aa'.repeat(16) });
  await settle();
  expect(w.core.pause).not.toHaveBeenCalled();
  gate.resolve({ version: 1n, blob: new Uint8Array([9]), raw: LIST_RAW });
  await vi.waitFor(() => expect(w.ret(92)).toMatchObject({ ok: true, value: { released: true } }));
});

it('resign waits for a follow-up trust refresh scheduled while the first one drains', async () => {
  const w = world();
  await toReady(w);
  await settle();
  const pending = (userId: Uint8Array) => [{ groupId: GROUP, leafIndex: 1, userId, cachedVersion: 0n, reason: 'unknown' }];
  w.core.pendingLeaves.mockReturnValue(pending(STRANGER));
  const first = deferred<Awaited<ReturnType<typeof w.routes.getDeviceList>>>();
  const second = deferred<Awaited<ReturnType<typeof w.routes.getDeviceList>>>();
  w.routes.getDeviceList.mockImplementationOnce(() => first.promise).mockImplementationOnce(() => second.promise);
  w.sync.deps!.onGroupChanged(GROUP, applied(2));
  await vi.waitFor(() => expect(w.routes.getDeviceList).toHaveBeenCalledTimes(1));
  w.core.pendingLeaves.mockReturnValue(pending(OTHER_DEVICE));
  w.sync.deps!.onGroupChanged(GROUP, applied(2));
  w.call(92, { m: 'resign', nonce: 'aa'.repeat(16) });
  first.resolve({ version: 1n, blob: new Uint8Array([9]), raw: LIST_RAW });
  await vi.waitFor(() => expect(w.routes.getDeviceList).toHaveBeenCalledTimes(2));
  await settle();
  expect(w.core.pause).not.toHaveBeenCalled();
  second.resolve({ version: 1n, blob: new Uint8Array([9]), raw: LIST_RAW });
  await vi.waitFor(() => expect(w.ret(92)).toMatchObject({ ok: true, value: { released: true } }));
});

it('resign waits for ready backup maintenance', async () => {
  const w = world();
  await toReady(w);
  await settle();
  const gate = deferred<void>();
  w.account.ensureBackups.mockImplementationOnce(() => gate.promise);
  w.gateway.emit({ type: 'ready', info: {
    deviceId: DEVICE, userId: USER, generation: 7n, versions: { wire: 2n, e2ee: 1n, media: 1n }, keypackagesRemaining: 32, groups: [],
    heartbeatMs: 30_000, maxFrameBytes: 131_584, backoffMs: 500, backoffJitterMs: 250,
  } });
  await vi.waitFor(() => expect(w.account.ensureBackups).toHaveBeenCalledTimes(2));
  w.call(92, { m: 'resign', nonce: 'aa'.repeat(16) });
  await settle();
  expect(w.core.pause).not.toHaveBeenCalled();
  gate.resolve();
  await vi.waitFor(() => expect(w.ret(92)).toMatchObject({ ok: true, value: { released: true } }));
});

it('resign waits for ready keypackage refill', async () => {
  const w = world();
  await toReady(w);
  await settle();
  const gate = deferred<void>();
  w.routes.postKeyPackages.mockImplementationOnce(() => gate.promise);
  w.gateway.emit({ type: 'ready', info: {
    deviceId: DEVICE, userId: USER, generation: 7n, versions: { wire: 2n, e2ee: 1n, media: 1n }, keypackagesRemaining: 1, groups: [],
    heartbeatMs: 30_000, maxFrameBytes: 131_584, backoffMs: 500, backoffJitterMs: 250,
  } });
  await vi.waitFor(() => expect(w.routes.postKeyPackages).toHaveBeenCalled());
  w.call(92, { m: 'resign', nonce: 'aa'.repeat(16) });
  await settle();
  expect(w.core.pause).not.toHaveBeenCalled();
  gate.resolve();
  await vi.waitFor(() => expect(w.ret(92)).toMatchObject({ ok: true, value: { released: true } }));
});

it('resign waits for ready trust ingestion', async () => {
  const w = world();
  await toReady(w);
  await settle();
  w.core.pendingLeaves.mockReturnValue([{ groupId: GROUP, leafIndex: 1, userId: STRANGER, cachedVersion: 0n, reason: 'unknown' }]);
  const gate = deferred<Awaited<ReturnType<typeof w.routes.getDeviceList>>>();
  w.routes.getDeviceList.mockImplementationOnce(() => gate.promise);
  w.gateway.emit({ type: 'ready', info: {
    deviceId: DEVICE, userId: USER, generation: 7n, versions: { wire: 2n, e2ee: 1n, media: 1n }, keypackagesRemaining: 32, groups: [],
    heartbeatMs: 30_000, maxFrameBytes: 131_584, backoffMs: 500, backoffJitterMs: 250,
  } });
  await vi.waitFor(() => expect(w.routes.getDeviceList).toHaveBeenCalled());
  w.call(92, { m: 'resign', nonce: 'aa'.repeat(16) });
  await settle();
  expect(w.core.pause).not.toHaveBeenCalled();
  gate.resolve({ version: 1n, blob: new Uint8Array([9]), raw: LIST_RAW });
  await vi.waitFor(() => expect(w.ret(92)).toMatchObject({ ok: true, value: { released: true } }));
});

it('resign waits for ready own-list maintenance', async () => {
  const w = world();
  await toReady(w);
  await settle();
  const gate = deferred<{ version: bigint; listed: boolean }>();
  w.account.refreshOwnDeviceList.mockImplementationOnce(() => gate.promise);
  w.gateway.emit({ type: 'ready', info: {
    deviceId: DEVICE, userId: USER, generation: 7n, versions: { wire: 2n, e2ee: 1n, media: 1n }, keypackagesRemaining: 32, groups: [],
    heartbeatMs: 30_000, maxFrameBytes: 131_584, backoffMs: 500, backoffJitterMs: 250,
  } });
  await vi.waitFor(() => expect(w.account.refreshOwnDeviceList).toHaveBeenCalledTimes(2));
  w.call(92, { m: 'resign', nonce: 'aa'.repeat(16) });
  await settle();
  expect(w.core.pause).not.toHaveBeenCalled();
  gate.resolve({ version: 1n, listed: true });
  await vi.waitFor(() => expect(w.ret(92)).toMatchObject({ ok: true, value: { released: true } }));
});

it('resign waits for ready backup repair after the own-list refresh', async () => {
  const w = world();
  await toReady(w);
  await settle();
  const gate = deferred<boolean>();
  w.account.repairBackupState.mockImplementationOnce(() => gate.promise);
  w.gateway.emit({ type: 'ready', info: {
    deviceId: DEVICE, userId: USER, generation: 7n, versions: { wire: 2n, e2ee: 1n, media: 1n }, keypackagesRemaining: 32, groups: [],
    heartbeatMs: 30_000, maxFrameBytes: 131_584, backoffMs: 500, backoffJitterMs: 250,
  } });
  await vi.waitFor(() => expect(w.account.repairBackupState).toHaveBeenCalledTimes(2));
  w.call(92, { m: 'resign', nonce: 'aa'.repeat(16) });
  await settle();
  expect(w.core.pause).not.toHaveBeenCalled();
  gate.resolve(false);
  await vi.waitFor(() => expect(w.ret(92)).toMatchObject({ ok: true, value: { released: true } }));
});

it('resign waits for the ready community refresh', async () => {
  const w = world();
  await toReady(w);
  await settle();
  const gate = deferred<Awaited<ReturnType<typeof w.routes.listCommunities>>>();
  w.routes.listCommunities.mockImplementationOnce(() => gate.promise);
  w.gateway.emit({ type: 'ready', info: {
    deviceId: DEVICE, userId: USER, generation: 7n, versions: { wire: 2n, e2ee: 1n, media: 1n }, keypackagesRemaining: 32, groups: [],
    heartbeatMs: 30_000, maxFrameBytes: 131_584, backoffMs: 500, backoffJitterMs: 250,
  } });
  await vi.waitFor(() => expect(w.routes.listCommunities).toHaveBeenCalledTimes(2));
  w.call(92, { m: 'resign', nonce: 'aa'.repeat(16) });
  await settle();
  expect(w.core.pause).not.toHaveBeenCalled();
  gate.resolve([{ id: COMMUNITY, name: 'friends', owner: USER, policyVersion: 1n }]);
  await vi.waitFor(() => expect(w.ret(92)).toMatchObject({ ok: true, value: { released: true } }));
});

it('resign waits for an outgoing attachment reference deletion', async () => {
  const w = world();
  await toReady(w);
  await openGeneral(w);
  const gate = deferred<void>();
  w.routes.deleteBlob.mockImplementationOnce(() => gate.promise);
  const row: OutboxRow = { msgId: MSG, state: 0, error: '', created: NOW_S, body: '', type: 0, replyTo: null,
    attachments: [{ blobId: B1, size: 1, mime: 'text/plain', name: 'a' }] };
  w.sync.deps!.onDiscarded(GROUP, row);
  await vi.waitFor(() => expect(w.routes.deleteBlob).toHaveBeenCalled());
  w.call(92, { m: 'resign', nonce: 'aa'.repeat(16) });
  await settle();
  expect(w.core.pause).not.toHaveBeenCalled();
  gate.resolve();
  await vi.waitFor(() => expect(w.ret(92)).toMatchObject({ ok: true, value: { released: true } }));
});

it('resign waits for an asynchronous recorded-message purge', async () => {
  const w = world();
  await toReady(w);
  await openGeneral(w);
  w.state.purges = [purge(5n, [])];
  const gate = deferred<'deleted' | 'gone'>();
  w.routes.deleteGroupMessage.mockImplementationOnce(() => gate.promise);
  w.sync.deps!.onOutboxChanged(GROUP);
  await vi.waitFor(() => expect(w.routes.deleteGroupMessage).toHaveBeenCalled());
  w.call(92, { m: 'resign', nonce: 'aa'.repeat(16) });
  await settle();
  expect(w.core.pause).not.toHaveBeenCalled();
  gate.resolve('deleted');
  await vi.waitFor(() => expect(w.ret(92)).toMatchObject({ ok: true, value: { released: true } }));
});

it('resign waits for cleanup after a refused send takes attachment references', async () => {
  const w = world();
  await toReady(w);
  await openGeneral(w);
  const [trayId] = await attachReady(w, 4, [file('oops.txt', 'text/plain', 4)]);
  const gate = deferred<void>();
  w.routes.deleteBlob.mockImplementationOnce(() => gate.promise);
  w.sync.sendRequest.mockImplementationOnce(() => { throw new CoreError('E_CORE_INPUT', 'refused send'); });
  w.call(5, { m: 'send', channelId: CH_HEX, text: 'hello', attachments: [trayId] });
  await vi.waitFor(() => expect(w.ret(5)).toMatchObject({ ok: false, error: { code: 'E_CORE_INPUT', detail: '' } }));
  await vi.waitFor(() => expect(w.routes.deleteBlob).toHaveBeenCalled());
  w.call(92, { m: 'resign', nonce: 'aa'.repeat(16) });
  await settle();
  expect(w.core.pause).not.toHaveBeenCalled();
  gate.resolve();
  await vi.waitFor(() => expect(w.ret(92)).toMatchObject({ ok: true, value: { released: true } }));
});

it('publishes only bounded diagnostics and code-only errors', async () => {
  const w = world();
  await toReady(w);
  w.call(93, { m: 'refreshDevices' });
  await vi.waitFor(() => expect(w.ret(93)).toBeDefined());
  const diagnostics = w.last<{ devices: number; errors: { code: string; at: number }[] }>('diagnostics');
  expect(diagnostics?.devices).toBe(2);
  expect(diagnostics?.errors).toHaveLength(0);
  expect(JSON.stringify(w.posted)).not.toContain('session-token');
  expect(JSON.stringify(w.posted)).not.toContain('SECRET busy');
});

it('refreshTrust fetches a pending leaf once and re-renders already open timelines after ingestion', async () => {
  const w = world();
  await toReady(w);
  w.state.rows = [strangerRow(1)];
  await openGeneral(w);
  w.core.pendingLeaves.mockReturnValue([{ groupId: GROUP, leafIndex: 1, userId: STRANGER, cachedVersion: 0n, reason: 'unknown' }]);
  w.core.deviceListIngest.mockImplementation(() => {
    w.state.rows = [{ ...strangerRow(1), status: 1, reason: 'E_UMK_CHANGED', body: '' }];
    w.core.trustGeneration.mockReturnValue(1n);
    return { version: 1n, changed: true, quarantined: 1 };
  });
  const before = w.posted.filter((m) => m.t === 'slice' && m.name === `timeline:${toHex(CHANNEL)}`).length;
  w.call(882, { m: 'refreshTrust' });
  await vi.waitFor(() => expect(w.ret(882)).toMatchObject({ ok: true }));
  expect(w.core.deviceListIngest).toHaveBeenCalledOnce();
  expect(w.core.revalidatePending).toHaveBeenCalledWith(GROUP, 1, 256);
  expect(w.posted.filter((m) => m.t === 'slice' && m.name === `timeline:${toHex(CHANNEL)}`).length).toBeGreaterThan(before);
});

it('an epoch transition refetches the same leaf and cached version once', async () => {
  const w = world();
  await toReady(w);
  w.core.pendingLeaves.mockReturnValue([{ groupId: GROUP, leafIndex: 1, userId: STRANGER, cachedVersion: 1n, reason: 'ready-refresh' }]);
  w.call(882, { m: 'refreshTrust' });
  await vi.waitFor(() => expect(w.ret(882)).toMatchObject({ ok: true }));
  expect(w.core.deviceListIngest).toHaveBeenCalledTimes(1);

  w.sync.deps!.onGroupChanged(GROUP, applied(2));
  await vi.waitFor(() => expect(w.core.deviceListIngest).toHaveBeenCalledTimes(2));
  w.sync.deps!.onGroupChanged(GROUP, { ...applied(2), epochChanged: false });
  await settle();
  expect(w.core.deviceListIngest).toHaveBeenCalledTimes(2);
});

it('a changed pending reason refetches the same leaf without an epoch change', async () => {
  const w = world();
  await toReady(w);
  w.core.pendingLeaves.mockReturnValue([{ groupId: GROUP, leafIndex: 1, userId: STRANGER, cachedVersion: 1n, reason: 'ready-refresh' }]);
  w.call(883, { m: 'refreshTrust' });
  await vi.waitFor(() => expect(w.ret(883)).toMatchObject({ ok: true }));
  expect(w.core.deviceListIngest).toHaveBeenCalledTimes(1);
  w.core.pendingLeaves.mockReturnValue([{ groupId: GROUP, leafIndex: 1, userId: STRANGER, cachedVersion: 1n, reason: 'changed' }]);
  w.sync.deps!.onGroupChanged(GROUP, { ...applied(2), epochChanged: false });
  await vi.waitFor(() => expect(w.core.deviceListIngest).toHaveBeenCalledTimes(2));
});

it('a later trust generation invalidates an open timeline from another group', async () => {
  const w = world();
  await toReady(w);
  w.state.rows = [strangerRow(1)];
  await openGeneral(w);
  w.state.rows = [{ ...strangerRow(1), status: 1, reason: 'E_UMK_CHANGED', body: '' }];
  w.core.trustGeneration.mockReturnValue(1n);
  w.sync.deps!.onGroupChanged(new Uint8Array(16).fill(0xfe), applied(2));
  expect(w.timeline()?.items[0]?.body).toBe('');
});

it('candidate list ingestion invalidates a published timeline before commit confirmation', async () => {
  const w = world();
  await toReady(w);
  w.state.rows = [strangerRow(1)];
  await openGeneral(w);
  w.state.rows = [{ ...strangerRow(1), status: 1, reason: 'E_UMK_CHANGED', body: '' }];
  w.core.trustGeneration.mockReturnValue(1n);
  w.sync.deps!.onTrustChanged();
  expect(w.timeline()?.items[0]?.body).toBe('');
});

it('a trust generation change posts each open timeline once, never an empty one first; a timeline that cannot be read is emptied (M4)', async () => {
  const w = world();
  await toReady(w);
  w.state.rows = [strangerRow(1)];
  await openGeneral(w);
  const name = `timeline:${toHex(CHANNEL)}`;
  const posts = () => w.posted.filter((m): m is SliceMessage => m.t === 'slice' && m.name === name);
  const before = posts().length;
  w.state.rows = [{ ...strangerRow(1), status: 1, reason: 'E_UMK_CHANGED', body: '' }];
  w.core.trustGeneration.mockReturnValue(1n);
  w.sync.deps!.onTrustChanged();
  expect(posts().slice(before).map((m) => (m.value as TimelineState).items.length)).toEqual([1]);
  const timeline = w.core.timeline;
  w.core.timeline = () => { throw new CoreError('E_CORE_STORAGE', ''); };
  w.core.trustGeneration.mockReturnValue(2n);
  w.sync.deps!.onTrustChanged();
  w.core.timeline = timeline;
  expect(w.timeline()?.items).toEqual([]);
});

it('a trust generation change empties an open timeline it cannot re-read, never leaving the old text (lead on 68194e7)', async () => {
  const w = world();
  await toReady(w);
  w.state.rows = [strangerRow(1)];
  await openGeneral(w);
  expect(w.timeline()?.items[0]?.body).not.toBe('');
  // A generation change that leaves this timeline's rows as they were keeps it (an equal slice is not posted again).
  w.core.trustGeneration.mockReturnValue(1n);
  w.sync.deps!.onTrustChanged();
  expect(w.timeline()?.items).toHaveLength(1);
  // The channel leaves the community's list while its timeline is still open, then a trust write retracts its rows.
  w.routes.listChannels.mockResolvedValueOnce([]);
  w.call(72, { m: 'selectCommunity', communityId: toHex(COMMUNITY) });
  await vi.waitFor(() => expect(w.ret(72)).toMatchObject({ ok: true }));
  w.state.rows = [{ ...strangerRow(1), status: 1, reason: 'E_UMK_CHANGED', body: '' }];
  w.core.trustGeneration.mockReturnValue(2n);
  w.sync.deps!.onTrustChanged();
  expect(w.timeline()?.items).toEqual([]);
});

async function toKeys(w: World): Promise<void> {
  w.call(1, { m: 'start' });
  await vi.waitFor(() => expect(w.account()?.phase).toBe('needs-signup'));
  w.call(2, { m: 'signupBegin' });
  await vi.waitFor(() => expect(w.ret(2)).toEqual({ t: 'ret', id: 2, ok: true, value: null }));
}

/** Selects the community and opens #general, whose text group is active (commands 2 and 3). */
async function openGeneral(w: World): Promise<void> {
  w.state.groups = [textGroup(2)];
  w.call(2, { m: 'selectCommunity', communityId: toHex(COMMUNITY) });
  await vi.waitFor(() => expect(w.ret(2)).toEqual({ t: 'ret', id: 2, ok: true, value: null }));
  w.call(3, { m: 'openChannel', channelId: toHex(CHANNEL) });
  await vi.waitFor(() => expect(w.ret(3)).toEqual({ t: 'ret', id: 3, ok: true, value: null }));
  expect(w.channels()?.[0]?.group).toBe('active');
}

/** Lets every queued promise job and the next timer turn run, so a late publish would be seen. */
const settle = (): Promise<void> => new Promise((resolve) => { setTimeout(resolve, 0); });

describe('Controller in the ready phase', () => {
  it('an HTTP refusal crosses with its status and wait', async () => {
    const w = world({ phase: 0 });
    // Signup.submit propagates postAccount's DillaHttpError unchanged (L-TS-06); the double throws what it would.
    w.signup.submit.mockImplementationOnce(() => Promise.reject(refusal(429, 'E_RATE_LIMITED', 4200)));
    await toKeys(w);
    w.call(3, SUBMIT);
    await vi.waitFor(() => expect(w.ret(3)).toBeDefined());
    const want = { code: 'E_RATE_LIMITED', detail: '', status: 429, retryAfterMs: 4200 };
    expect(w.ret(3)).toEqual({ t: 'ret', id: 3, ok: false, error: want });
    expect(w.account()?.error).toEqual(want);
    expect(w.account()?.phase).toBe('signup-keys');
    expect(w.account()?.recoveryKey).toHaveLength(13);
  });

  it('a sync error keeps its code', async () => {
    const w = world();
    await toReady(w);
    w.sync.openChannel.mockImplementationOnce(() => Promise.reject(new SyncError('E_REGISTER_RACE')));
    w.call(2, { m: 'selectCommunity', communityId: toHex(COMMUNITY) });
    await vi.waitFor(() => expect(w.ret(2)).toBeDefined());
    w.call(3, { m: 'openChannel', channelId: toHex(CHANNEL) });
    await vi.waitFor(() => expect(w.ret(3)).toBeDefined());
    expect(w.ret(3)).toEqual({ t: 'ret', id: 3, ok: false, error: { code: 'E_REGISTER_RACE', detail: '', status: 0, retryAfterMs: null } });
  });

  it('a refused resync shows not-member', async () => {
    const w = world();
    await toReady(w);
    await openGeneral(w);
    w.sync.deps!.onMembership(GROUP, 'resyncing');
    expect(w.channels()?.[0]?.group).toBe('resync');
    expect(w.timeline()?.group).toBe('resync');
    w.state.groups = [textGroup(3)];
    w.sync.deps!.onMembership(GROUP, 'not-member');
    expect(w.channels()?.[0]?.group).toBe('not-member');
    expect(w.timeline()?.group).toBe('not-member');
  });

  // The docs shoot's removal after a refused voice join (task-11 shots report, concern 3): a gateway frame applied to the
  // group after the refused resync (state 2) proves nothing about this device's leaf. Not-member stays until the engine
  // reports the membership confirmed (a catch-up the DS served) or a rejoin starts; the composer never reopens on a frame.
  it('a refused resync of an active row shows not-member until the engine confirms the membership again', async () => {
    const w = world();
    await toReady(w);
    await openGeneral(w);
    w.state.groups = [textGroup(2)];
    // The engine's order on a refusal before the join: resyncing, the row's snapshot (still state 2), not-member.
    w.sync.deps!.onMembership(GROUP, 'resyncing');
    w.sync.deps!.onGroupChanged(GROUP, applied(2));
    w.sync.deps!.onMembership(GROUP, 'not-member');
    expect(w.channels()?.[0]?.group).toBe('not-member');
    expect(w.timeline()?.group).toBe('not-member');
    w.sync.deps!.onGroupChanged(GROUP, applied(2));
    expect(w.channels()?.[0]?.group).toBe('not-member');
    expect(w.timeline()?.group).toBe('not-member');
    w.sync.deps!.onMembership(GROUP, 'member');
    expect(w.channels()?.[0]?.group).toBe('active');
    expect(w.timeline()?.group).toBe('active');
  });

  it('a successful resync clears it', async () => {
    const w = world();
    await toReady(w);
    await openGeneral(w);
    w.state.groups = [textGroup(3)];
    w.sync.deps!.onMembership(GROUP, 'resyncing');
    w.sync.deps!.onMembership(GROUP, 'not-member');
    expect(w.channels()?.[0]?.group).toBe('not-member');
    // The next ready resyncs again (a resync starts with its own report), and this time it joins.
    w.sync.deps!.onMembership(GROUP, 'resyncing');
    w.state.groups = [textGroup(2)];
    w.sync.deps!.onGroupChanged(GROUP, applied(2));
    expect(w.channels()?.[0]?.group).toBe('active');
    expect(w.timeline()?.group).toBe('active');
    // The old refusal is forgotten: the group falling back to state 3 reads as a resync, not as not-member.
    w.state.groups = [textGroup(3)];
    w.sync.deps!.onGroupChanged(GROUP, applied(3));
    expect(w.channels()?.[0]?.group).toBe('resync');
  });

  it('the first connect shows connecting', async () => {
    const w = world();
    await toReady(w);
    expect(w.gateway.starts).toBe(1);
    expect(w.connection()?.status).toBe('offline');
    w.gateway.emit({ type: 'status', status: 'connecting', closeCode: null });
    expect(w.connection()?.status).toBe('connecting');
  });

  it('ready → waiting → connecting → ready publishes online, offline, offline, online', async () => {
    const w = world();
    await toReady(w);
    const published = (): number => w.posted.filter((m) => m.t === 'slice' && m.name === 'connection').length;
    const before = published();
    const seen: (string | undefined)[] = [];
    for (const status of ['ready', 'waiting', 'connecting', 'ready'] as const) {
      w.gateway.emit({ type: 'status', status, closeCode: status === 'waiting' ? 1006 : null });
      seen.push(w.connection()?.status);
    }
    expect(seen).toEqual(['online', 'offline', 'offline', 'online']);
    // The unchanged third value is not published again.
    expect(published() - before).toBe(3);
  });

  it("a version refusal publishes connection reason 'version' and no reconnect", async () => {
    const w = world();
    await toReady(w);
    w.gateway.emit({ type: 'status', status: 'ready', closeCode: null });
    w.gateway.emit({ type: 'ready', info: READY_INFO });
    expect(w.connection()).toEqual({ status: 'online', generation: '7' });
    const starts = w.gateway.starts;
    w.gateway.emit({ type: 'status', status: 'idle', closeCode: CLIENT_CLOSE.version });
    expect(w.connection()).toStrictEqual({ status: 'offline', generation: '7', reason: 'version' });
    await settle();
    // The controller does not start the gateway again: the refusal stands until the page is reloaded.
    expect(w.gateway.starts).toBe(starts);
    // The next connecting or online clears the reason; a plain idle never carries one.
    w.gateway.emit({ type: 'status', status: 'connecting', closeCode: null });
    expect(w.connection()).toStrictEqual({ status: 'connecting', generation: '7' });
    w.gateway.emit({ type: 'status', status: 'idle', closeCode: CLIENT_CLOSE.version });
    w.gateway.emit({ type: 'status', status: 'ready', closeCode: null });
    expect(w.connection()).toStrictEqual({ status: 'online', generation: '7' });
    w.gateway.emit({ type: 'status', status: 'idle', closeCode: null });
    expect(w.connection()).toStrictEqual({ status: 'offline', generation: '7' });
  });

  it('a request that cannot re-authenticate ends in revoked', async () => {
    const paths: string[] = [];
    const instanceAndLimits = fakeFetch(paths);
    const fetch401 = ((input: RequestInfo | URL, init?: RequestInit) => {
      const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
      if (path === '/v1/communities') {
        paths.push(path);
        return Promise.resolve(cbor(['E_UNAUTHENTICATED', '', null], 401));
      }
      return instanceAndLimits(input, init);
    }) as typeof fetch;
    const w = world({ realRoutes: true, fetch: fetch401 });
    w.session.establish.mockImplementation(() => Promise.resolve(false));
    w.call(1, { m: 'start' });
    await vi.waitFor(() => expect(w.account()?.phase).toBe('revoked'));
    await settle();
    expect(paths).toContain('/v1/communities');
    expect(w.session.establish).toHaveBeenCalledTimes(1);
    expect(w.gateway.stops).toBe(1);
    expect(w.sync.stop).toHaveBeenCalledTimes(1);
    // The 401 that ended enter-ready is the revocation's consequence: no 'error' and no 'ready' after it.
    expect(w.account()?.phase).toBe('revoked');
    const phases = w.posted.filter((m): m is SliceMessage => m.t === 'slice' && m.name === 'account').map((m) => (m.value as AccountState).phase);
    expect(phases).not.toContain('ready');
    expect(phases).not.toContain('error');
  });

  it('a resumed signup whose device is refused shows revoked and deletes nothing', async () => {
    const w = world({ phase: 1 });
    w.signup.resume.mockImplementationOnce(() => Promise.resolve('revoked' as const));
    w.call(1, { m: 'start' });
    await vi.waitFor(() => expect(w.account()?.phase).toBe('revoked'));
    expect(w.signup.resume).toHaveBeenCalledTimes(1);
    expect(w.state.resets).toBe(0);
    expect(w.state.phase).toBe(1);
    expect(w.gateway.starts).toBe(0);
  });

  it('a revoked socket is re-established, or ends in revoked', async () => {
    const w = world();
    await toReady(w);
    w.gateway.emit({ type: 'revoked' });
    await vi.waitFor(() => expect(w.gateway.starts).toBe(2));
    expect(w.account()?.phase).toBe('ready');
    w.session.establish.mockImplementationOnce(() => Promise.resolve(false));
    w.gateway.emit({ type: 'revoked' });
    await vi.waitFor(() => expect(w.account()?.phase).toBe('revoked'));
    expect(w.gateway.stops).toBe(1);
    expect(w.sync.stop).toHaveBeenCalledTimes(1);
  });

  it('a spent invite is refused before anything is registered', async () => {
    const w = world({ phase: 0 });
    w.routes.getInvite.mockImplementationOnce(() => Promise.reject(refusal(410, 'E_INVITE_INVALID')));
    await toKeys(w);
    w.call(3, SUBMIT);
    await vi.waitFor(() => expect(w.ret(3)).toBeDefined());
    const want = { code: 'E_INVITE_INVALID', detail: '', status: 410, retryAfterMs: null };
    expect(w.ret(3)).toEqual({ t: 'ret', id: 3, ok: false, error: want });
    expect(w.routes.getInvite).toHaveBeenCalledWith('friends-code');
    // Signup.submit is the only caller of postAccount (L-TS-06): nothing was registered.
    expect(w.signup.submit).not.toHaveBeenCalled();
    expect(w.account()).toMatchObject({ phase: 'signup-keys', error: want });
    expect(w.account()?.recoveryKey).toHaveLength(13);
  });

  it('a server invite lands in the server', async () => {
    const w = world({ phase: 0 });
    w.routes.getInvite.mockImplementationOnce(() => Promise.resolve({ communityId: COMMUNITY, communityName: 'friends', expires: 0n }));
    const ordering: string[] = [];
    const negotiation = vi.fn(() => { ordering.push('negotiate'); return Promise.resolve({ wire: 3, e2ee: 1, media: 1, botAvailable: true }); });
    (w.routes as unknown as { negotiateSession: typeof negotiation }).negotiateSession = negotiation;
    w.routes.joinCommunity.mockImplementationOnce(() => { ordering.push('join'); w.state.joined = true; return Promise.resolve(); });
    await toKeys(w);
    w.call(3, SUBMIT);
    await vi.waitFor(() => expect(w.ret(3)).toBeDefined());
    expect(w.ret(3)).toEqual({ t: 'ret', id: 3, ok: true, value: { communityId: toHex(COMMUNITY), joinError: null } });
    expect(w.signup.submit).toHaveBeenCalledWith({ invite: 'friends-code', username: 'web', display: 'Web', password: null });
    expect(ordering).toEqual(['negotiate', 'join']);
    expect(negotiation).toHaveBeenCalledTimes(1);
    expect(w.routes.joinCommunity).toHaveBeenCalledWith(COMMUNITY, 'friends-code');
    expect(w.account()?.phase).toBe('ready');
    const retAt = w.posted.findIndex((m) => m.t === 'ret' && m.id === 3);
    const listedAt = w.posted.findIndex((m) => m.t === 'slice' && m.name === 'communities' && (m.value as CommunitySummary[]).length === 1);
    expect(listedAt).toBeGreaterThan(-1);
    expect(listedAt).toBeLessThan(retAt);
    expect(w.last<CommunitySummary[]>('communities')).toEqual([{ id: toHex(COMMUNITY), name: 'friends', owner: toHex(USER) }]);
  });

  it('joins once when an older server does not implement session negotiation', async () => {
    const w = world({ phase: 0 });
    w.routes.getInvite.mockImplementationOnce(() => Promise.resolve({ communityId: COMMUNITY, communityName: 'friends', expires: 0n }));
    const negotiation = vi.fn(() => Promise.reject(refusal(404, 'E_NOT_FOUND')));
    (w.routes as unknown as { negotiateSession: typeof negotiation }).negotiateSession = negotiation;
    await toKeys(w);
    w.call(3, SUBMIT);
    await vi.waitFor(() => expect(w.ret(3)).toBeDefined());
    expect(w.ret(3)).toMatchObject({ t: 'ret', id: 3, ok: true, value: { communityId: toHex(COMMUNITY), joinError: null } });
    expect(negotiation).toHaveBeenCalledTimes(1);
    expect(w.routes.joinCommunity).toHaveBeenCalledTimes(1);
  });

  it('a refused join is not a failed signup', async () => {
    const w = world({ phase: 0 });
    w.routes.getInvite.mockImplementationOnce(() => Promise.resolve({ communityId: COMMUNITY, communityName: 'friends', expires: 0n }));
    w.routes.joinCommunity.mockImplementationOnce(() => Promise.reject(refusal(410, 'E_INVITE_INVALID')));
    await toKeys(w);
    w.call(3, SUBMIT);
    await vi.waitFor(() => expect(w.ret(3)).toBeDefined());
    expect(w.ret(3)).toEqual({
      t: 'ret', id: 3, ok: true,
      value: { communityId: null, joinError: { code: 'E_INVITE_INVALID', detail: '', status: 410, retryAfterMs: null } },
    });
    expect(w.account()).toMatchObject({ phase: 'ready', error: null });
  });

  it('a publish failure after registration does not return to the keys', async () => {
    const w = world({ phase: 0 });
    await toKeys(w);
    const from = w.posted.length;
    // The account exists (the core reached phase 2), then publishing the device list failed.
    w.signup.submit.mockImplementationOnce(() => { w.state.phase = 2; return Promise.reject(refusal(503, 'E_UNAVAILABLE', 1000)); });
    w.call(3, SUBMIT);
    await vi.waitFor(() => expect(w.ret(3)).toBeDefined());
    expect(w.account()?.phase).toBe('ready');
    expect(w.ret(3)).toEqual({ t: 'ret', id: 3, ok: true, value: { communityId: null, joinError: null } });
    const phases = w.posted.slice(from).filter((m): m is SliceMessage => m.t === 'slice' && m.name === 'account').map((m) => (m.value as AccountState).phase);
    expect(phases).not.toContain('signup-keys');
  });

  it('a ready after a reconnect refreshes the lists', async () => {
    const w = world();
    await toReady(w);
    await openGeneral(w);
    expect(w.routes.listCommunities).toHaveBeenCalledTimes(1);
    expect(w.routes.listChannels).toHaveBeenCalledTimes(2);
    expect(w.routes.listMembers).toHaveBeenCalledTimes(2);
    w.gateway.emit({ type: 'ready', info: READY_INFO });
    await vi.waitFor(() => expect(w.routes.listMembers).toHaveBeenCalledTimes(3));
    await vi.waitFor(() => expect(w.routes.listChannels).toHaveBeenCalledTimes(3));
    await vi.waitFor(() => expect(w.routes.listDms).toHaveBeenCalledTimes(2));
    expect(w.routes.listCommunities).toHaveBeenCalledTimes(2);
    expect(w.routes.listChannels).toHaveBeenLastCalledWith(COMMUNITY);
    expect(w.connection()?.generation).toBe('7');
  });

  it('a member who joined later gets a name', async () => {
    const w = world();
    await toReady(w);
    await openGeneral(w);
    expect(w.routes.listMembers).toHaveBeenCalledTimes(2);
    w.state.rows = [strangerRow(1)];
    w.sync.deps!.onGroupChanged(GROUP, applied(2));
    await vi.waitFor(() => expect(w.routes.listMembers).toHaveBeenCalledTimes(3));
    expect(w.routes.listMembers).toHaveBeenLastCalledWith(COMMUNITY);
    w.state.rows = [strangerRow(1), strangerRow(2)];
    w.sync.deps!.onGroupChanged(GROUP, applied(2));
    await settle();
    expect(w.routes.listMembers).toHaveBeenCalledTimes(3);
    expect(w.timeline()?.items.map((i) => i.body)).toEqual(['from a stranger 1', 'from a stranger 2']);
  });
});

// ---- Pre-flight ruling 1: the command edge cases, one test each. ----

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (error: unknown) => void;
  const promise = new Promise<T>((res, rej) => { resolve = res; reject = rej; });
  return { promise, resolve, reject };
}

async function selectFriends(w: World, id: number): Promise<void> {
  w.call(id, { m: 'selectCommunity', communityId: toHex(COMMUNITY) });
  await vi.waitFor(() => expect(w.ret(id)).toEqual({ t: 'ret', id, ok: true, value: null }));
}

describe('Controller command edge cases', () => {
  it('(i) a second openChannel of a channel whose open is in flight awaits the first', async () => {
    const w = world();
    await toReady(w);
    await selectFriends(w, 2);
    const join = deferred<{ groupId: typeof GROUP; state: 2 }>();
    w.sync.openChannel.mockImplementationOnce(() => join.promise);
    w.call(3, { m: 'openChannel', channelId: toHex(CHANNEL) });
    w.call(4, { m: 'openChannel', channelId: toHex(CHANNEL) });
    await settle();
    expect(w.sync.openChannel).toHaveBeenCalledTimes(1);
    expect(w.ret(3)).toBeUndefined();
    expect(w.ret(4)).toBeUndefined();
    w.state.groups = [textGroup(2)];
    join.resolve({ groupId: GROUP, state: 2 });
    await vi.waitFor(() => expect(w.ret(4)).toEqual({ t: 'ret', id: 4, ok: true, value: null }));
    expect(w.ret(3)).toEqual({ t: 'ret', id: 3, ok: true, value: null });
    expect(w.sync.openChannel).toHaveBeenCalledTimes(1);
    expect(w.timeline()?.group).toBe('active');
  });

  it('(ii) closeChannel during an open refreshes the slices when the join resolves but does not re-open the channel', async () => {
    const w = world();
    await toReady(w);
    await selectFriends(w, 2);
    const join = deferred<{ groupId: typeof GROUP; state: 2 }>();
    w.sync.openChannel.mockImplementationOnce(() => join.promise);
    w.call(3, { m: 'openChannel', channelId: toHex(CHANNEL) });
    await vi.waitFor(() => expect(w.sync.openChannel).toHaveBeenCalledTimes(1));
    expect(w.timeline()?.group).toBe('none');
    w.call(4, { m: 'closeChannel', channelId: toHex(CHANNEL) });
    await vi.waitFor(() => expect(w.ret(4)).toEqual({ t: 'ret', id: 4, ok: true, value: null }));
    w.state.groups = [textGroup(2)];
    w.state.rows = [strangerRow(1)];
    join.resolve({ groupId: GROUP, state: 2 });
    await vi.waitFor(() => expect(w.ret(3)).toEqual({ t: 'ret', id: 3, ok: true, value: null }));
    expect(w.channels()?.[0]?.group).toBe('active');
    expect(w.timeline()).toMatchObject({ group: 'active', items: [{ body: 'from a stranger 1' }] });
    // Not open: a later change of the group leaves the timeline as it was, and sending is refused.
    w.state.rows = [strangerRow(1), strangerRow(2)];
    w.sync.deps!.onGroupChanged(GROUP, applied(2));
    expect(w.timeline()?.items).toHaveLength(1);
    w.call(5, { m: 'send', channelId: toHex(CHANNEL), text: 'hello' });
    await vi.waitFor(() => expect(w.ret(5)).toBeDefined());
    expect(w.ret(5)).toMatchObject({ ok: false, error: { code: 'E_NOT_READY' } });
    expect(w.sync.sendRequest).not.toHaveBeenCalled();
  });

  it('(i)+(ii) an open, a close and a reopen while the join is in flight leave the channel open', async () => {
    const w = world();
    await toReady(w);
    await selectFriends(w, 2);
    const join = deferred<{ groupId: typeof GROUP; state: 2 }>();
    w.sync.openChannel.mockImplementationOnce(() => join.promise);
    w.call(3, { m: 'openChannel', channelId: toHex(CHANNEL) });
    await vi.waitFor(() => expect(w.sync.openChannel).toHaveBeenCalledTimes(1));
    w.call(4, { m: 'closeChannel', channelId: toHex(CHANNEL) });
    await vi.waitFor(() => expect(w.ret(4)).toEqual({ t: 'ret', id: 4, ok: true, value: null }));
    w.call(5, { m: 'openChannel', channelId: toHex(CHANNEL) });
    await settle();
    expect(w.ret(5)).toBeUndefined();
    w.state.groups = [textGroup(2)];
    w.state.rows = [strangerRow(1)];
    join.resolve({ groupId: GROUP, state: 2 });
    await vi.waitFor(() => expect(w.ret(5)).toEqual({ t: 'ret', id: 5, ok: true, value: null }));
    expect(w.ret(3)).toEqual({ t: 'ret', id: 3, ok: true, value: null });
    expect(w.sync.openChannel).toHaveBeenCalledTimes(1);
    expect(w.timeline()).toMatchObject({ group: 'active', items: [{ body: 'from a stranger 1' }] });
    // Open again: a later change of the group refreshes the timeline, and sending is accepted.
    w.state.rows = [strangerRow(1), strangerRow(2)];
    w.sync.deps!.onGroupChanged(GROUP, applied(2));
    expect(w.timeline()?.items.map((i) => i.body)).toEqual(['from a stranger 1', 'from a stranger 2']);
    w.sync.sendRequest.mockReturnValueOnce(new Uint8Array(16).fill(0x77));
    w.call(6, { m: 'send', channelId: toHex(CHANNEL), text: 'hello' });
    await vi.waitFor(() => expect(w.ret(6)).toBeDefined());
    expect(w.ret(6)).toEqual({ t: 'ret', id: 6, ok: true, value: { msgId: '77'.repeat(16) } });
    expect(w.sync.sendRequest).toHaveBeenCalledWith(GROUP, { type: 0, replyTo: null, body: 'hello', attachments: [], threadId: null });
  });

  it('(i)+(ii) a reopen while the join is in flight leaves the channel closed when the join fails', async () => {
    const w = world();
    await toReady(w);
    await selectFriends(w, 2);
    const join = deferred<{ groupId: typeof GROUP; state: 2 }>();
    w.sync.openChannel.mockImplementationOnce(() => join.promise);
    w.call(3, { m: 'openChannel', channelId: toHex(CHANNEL) });
    await vi.waitFor(() => expect(w.sync.openChannel).toHaveBeenCalledTimes(1));
    w.call(4, { m: 'closeChannel', channelId: toHex(CHANNEL) });
    await vi.waitFor(() => expect(w.ret(4)).toEqual({ t: 'ret', id: 4, ok: true, value: null }));
    w.call(5, { m: 'openChannel', channelId: toHex(CHANNEL) });
    await settle();
    // The group is active locally, so only the missing open mark can refuse the send below.
    w.state.groups = [textGroup(2)];
    join.reject(new SyncError('E_REGISTER_RACE'));
    const failed = { code: 'E_REGISTER_RACE', detail: '', status: 0, retryAfterMs: null };
    await vi.waitFor(() => expect(w.ret(5)).toEqual({ t: 'ret', id: 5, ok: false, error: failed }));
    expect(w.ret(3)).toEqual({ t: 'ret', id: 3, ok: false, error: failed });
    w.call(6, { m: 'send', channelId: toHex(CHANNEL), text: 'hello' });
    await vi.waitFor(() => expect(w.ret(6)).toBeDefined());
    expect(w.ret(6)).toMatchObject({ ok: false, error: { code: 'E_NOT_READY', detail: '' } });
    expect(w.sync.sendRequest).not.toHaveBeenCalled();
  });

  it('(iii) loadEarlier and closeChannel of a channel that is not open resolve null and publish nothing', async () => {
    const w = world();
    await toReady(w);
    await selectFriends(w, 2);
    const from = w.posted.length;
    w.call(3, { m: 'loadEarlier', channelId: toHex(CHANNEL) });
    w.call(4, { m: 'closeChannel', channelId: toHex(CHANNEL) });
    w.call(5, { m: 'loadEarlier', channelId: 'e'.repeat(32) });
    await vi.waitFor(() => expect(w.ret(5)).toBeDefined());
    for (const id of [3, 4, 5]) expect(w.ret(id)).toEqual({ t: 'ret', id, ok: true, value: null });
    expect(w.posted.slice(from).filter((m) => m.t === 'slice')).toEqual([]);
  });

  it('(iv) retrySend and discardSend of an unknown message resolve null and touch nothing', async () => {
    const w = world();
    await toReady(w);
    await openGeneral(w);
    w.call(4, { m: 'retrySend', msgId: 'd'.repeat(32) });
    w.call(5, { m: 'discardSend', msgId: 'd'.repeat(32) });
    await vi.waitFor(() => expect(w.ret(5)).toBeDefined());
    expect(w.ret(4)).toEqual({ t: 'ret', id: 4, ok: true, value: null });
    expect(w.ret(5)).toEqual({ t: 'ret', id: 5, ok: true, value: null });
    expect(w.sync.retry).not.toHaveBeenCalled();
    expect(w.sync.discard).not.toHaveBeenCalled();
  });

  it('(v) selectCommunity of a community that is not listed is refused', async () => {
    const w = world();
    await toReady(w);
    w.call(2, { m: 'selectCommunity', communityId: 'e'.repeat(32) });
    await vi.waitFor(() => expect(w.ret(2)).toBeDefined());
    expect(w.ret(2)).toEqual({ t: 'ret', id: 2, ok: false, error: { code: 'E_BAD_INPUT', detail: '', status: 0, retryAfterMs: null } });
    expect(w.routes.listChannels).toHaveBeenCalledTimes(1);
    expect(w.routes.listMembers).toHaveBeenCalledTimes(1);
  });

  it('(vi) a selected community that is no longer listed is dropped with its slices', async () => {
    const w = world();
    await toReady(w);
    await openGeneral(w);
    const members = () => w.last<MemberSummary[]>(`members:${toHex(COMMUNITY)}`);
    expect(members()).toHaveLength(1);
    w.state.joined = false;
    w.gateway.emit({ type: 'ready', info: READY_INFO });
    await vi.waitFor(() => expect(w.last<CommunitySummary[]>('communities')).toEqual([]));
    await settle();
    expect(w.channels()).toEqual([]);
    expect(members()).toEqual([]);
    expect(w.sync.setExpected).toHaveBeenLastCalledWith([]);
    expect(w.routes.listChannels).toHaveBeenCalledTimes(2);
    // Nothing is selected any more: the next ready reads no channel list, and a send to its channel is refused.
    w.gateway.emit({ type: 'ready', info: READY_INFO });
    await vi.waitFor(() => expect(w.routes.listCommunities).toHaveBeenCalledTimes(3));
    await settle();
    expect(w.routes.listChannels).toHaveBeenCalledTimes(2);
    w.call(4, { m: 'send', channelId: toHex(CHANNEL), text: 'hello' });
    await vi.waitFor(() => expect(w.ret(4)).toBeDefined());
    expect(w.ret(4)).toMatchObject({ ok: false, error: { code: 'E_NOT_READY' } });
  });

  it('(vii) a join whose community list refresh fails still resolves the community', async () => {
    const w = world();
    await toReady(w);
    w.routes.getInvite.mockImplementationOnce(() => Promise.resolve({ communityId: COMMUNITY, communityName: 'friends', expires: 0n }));
    w.routes.listCommunities.mockImplementationOnce(() => Promise.reject(refusal(503, 'E_UNAVAILABLE', 1000)));
    w.call(2, { m: 'joinCommunity', invite: '  friends-code  ' });
    await vi.waitFor(() => expect(w.ret(2)).toBeDefined());
    expect(w.ret(2)).toEqual({ t: 'ret', id: 2, ok: true, value: { communityId: toHex(COMMUNITY) } });
    expect(w.routes.getInvite).toHaveBeenCalledWith('friends-code');
    expect(w.routes.joinCommunity).toHaveBeenCalledWith(COMMUNITY, 'friends-code');
  });

  // web3.4: a server joined again in the same tab (after a kick, a lifted ban or a leave) lets the engine resync its groups
  // at once; a join the instance refused does not.
  it('a join tells the engine the community was joined again; a refused join does not', async () => {
    const w = world();
    await toReady(w);
    w.routes.getInvite.mockImplementation(() => Promise.resolve({ communityId: COMMUNITY, communityName: 'friends', expires: 0n }));
    w.routes.joinCommunity.mockImplementationOnce(() => Promise.reject(refusal(403, 'E_FORBIDDEN')));
    w.call(2, { m: 'joinCommunity', invite: 'friends-code' });
    await vi.waitFor(() => expect(w.ret(2)).toBeDefined());
    expect(w.ret(2)).toMatchObject({ ok: false, error: { code: 'E_FORBIDDEN' } });
    expect(w.sync.rejoined).not.toHaveBeenCalled();
    w.call(3, { m: 'joinCommunity', invite: 'friends-code' });
    await vi.waitFor(() => expect(w.ret(3)).toBeDefined());
    expect(w.ret(3)).toMatchObject({ ok: true });
    expect(w.sync.rejoined).toHaveBeenCalledTimes(1);
    expect(w.sync.rejoined).toHaveBeenCalledWith(COMMUNITY);
  });

  it('(viii) a network failure while entering ready at boot is the error phase', async () => {
    const w = world();
    const down = new DillaHttpError({ status: 0, code: 'E_NETWORK', detail: 'fetch failed', retryAfterMs: null, extra: [] });
    w.session.ensure.mockImplementationOnce(() => Promise.reject(down));
    w.call(1, { m: 'start' });
    await vi.waitFor(() => expect(w.account()?.phase).toBe('error'));
    expect(w.account()?.error).toEqual({ code: 'E_NETWORK', detail: '', status: 0, retryAfterMs: null });
    expect(w.gateway.starts).toBe(0);
    w.call(2, { m: 'selectCommunity', communityId: toHex(COMMUNITY) });
    await vi.waitFor(() => expect(w.ret(2)).toBeDefined());
    expect(w.ret(2)).toMatchObject({ ok: false, error: { code: 'E_NOT_READY', detail: '' } });
  });
});

const act = (g: Uint8Array, unread: number, mentions: number): ActivityRow =>
  ({ groupId: g, unread, mentions, lastSeq: BigInt(unread), lastTs: NOW_S, lastReadSeq: 0n });
const appliedSeqs = (seqs: bigint[]): ApplyResult =>
  ({ state: 2, epoch: 2n, nextSeq: (seqs.at(-1) ?? 0n) + 1n, newSeqs: seqs, proposalsPending: 0, epochChanged: false, ownAdopted: false });
const phasesOf = (w: World): string[] =>
  w.posted.filter((m): m is SliceMessage => m.t === 'slice' && m.name === 'account').map((m) => (m.value as AccountState).phase);

/** The login as the page sends it. web3.8 (R6): the recovery key comes last, with signInKey (KEY). */
const LOGIN = { m: 'signInLogin', username: ' web ', password: PASSWORD } as const;
const KEY = { m: 'signInKey', recoveryKey: RECOVERY_KEY } as const;

/** From needs-signup through signInBegin (id 2) to signin-login, nothing sent to the instance yet. */
async function toLoginStep(w: World): Promise<void> {
  w.call(1, { m: 'start' });
  await vi.waitFor(() => expect(w.account()?.phase).toBe('needs-signup'));
  w.call(2, { m: 'signInBegin' });
  await vi.waitFor(() => expect(w.ret(2)).toEqual({ t: 'ret', id: 2, ok: true, value: null }));
}

/** toLoginStep, then the login (id 3), answered. */
async function toKeyStep(w: World): Promise<void> {
  await toLoginStep(w);
  w.call(3, LOGIN);
  await vi.waitFor(() => expect(w.ret(3)).toBeDefined());
}

// Attacker statements (lesson e) for the refusals these blocks test, from the brief's requirements:
// - E_BAD_INPUT / E_NOT_READY are raised only for the owning page's own malformed or mistimed command; the worker
//   answers only its own page (deps.post), so no other party can trigger or observe them.
// - A refused TOTP or E_NO_ASSERTION: only the instance (it can always refuse service) or a password holder racing
//   the login (one assertion per login) can cause it; the person logs in again with the username kept, nothing stored.
// - A refused registration (403 device cap, a short meter 429): past the cap or rate the instance replaces the oldest
//   unlisted row, so a password holder cannot keep the owner out; the 403 only the owner's own list produces.
// - E_SESSION_SCOPE / E_DEVICE_UNLISTED in phase 2: a hostile or broken instance; the worst is this browser showing
//   revoked with its store untouched.
// - E_NO_BACKUP / E_LIST_RACE on a revocation: an instance withholding objects (denial of service it can always do)
//   or a concurrent publication by another device of the same user, cured by repeating the command; a missing state
//   object is no refusal (ruling 28). The key-less DELETE removes only a row no unrevoked entry names, decided from a
//   freshly refreshed own list (pre-flight ruling b); another user's row answers 404.
// - wiping/storeCleared are set only by this page's own signOutRevoke/forgetBrowser or E_LIST_RACE during its own
//   enrolment; a refused server call clears wiping before the command rejects.
// - E_SETTING_KEY: only the owning page reaches it; it keeps the settings table from becoming a general store.
// - Pre-flight rulings (c) and (e): an older served list or an unusable state object is the instance serving stale or
//   withheld backups; the client retries once (a list PUT racing the read), then shows the conflict or the missing
//   backup and signs nothing.

describe('Controller sign-in (L-TS-23)', () => {
  // REGISTRATION-DEVICES-02 as web3.8 (R6) keeps it: the login holds its settled assertion, and the key step spends it
  // after the key's form check, by the registration, the fetch, the enrolment and the list PUT, back to back.
  it('signs in without a second factor: the login, then the key\'s form, register, fetch and enrol back to back, then ready', async () => {
    const w = world({ phase: 0 });
    await toLoginStep(w);
    expect(w.account()).toMatchObject({ phase: 'signin-login', signIn: { username: null }, error: null });
    expect(w.calls.filter((c) => c.startsWith('enrol.'))).toEqual([]);
    w.call(3, LOGIN);
    await vi.waitFor(() => expect(w.ret(3)).toEqual({ t: 'ret', id: 3, ok: true, value: { needs: 0 } }));
    expect(w.account()).toMatchObject({ phase: 'signin-key', signIn: { username: 'web' }, error: null });
    w.call(4, KEY);
    await vi.waitFor(() => expect(w.ret(4)).toEqual({ t: 'ret', id: 4, ok: true, value: null }));
    expect(w.core.recoveryKeyCheck).toHaveBeenCalledWith(RECOVERY_KEY);
    expect(w.enrol.login).toHaveBeenCalledWith('web', PASSWORD);
    expect(w.enrol.register).toHaveBeenCalledWith(INSTANCE_ID);
    expect(w.enrol.fetch).toHaveBeenCalledWith(USER);
    expect(w.calls.filter((c) => c.startsWith('enrol.') || c === 'core.recoveryKeyCheck')).toEqual([
      'enrol.login', 'core.recoveryKeyCheck', 'enrol.register', 'enrol.fetch', 'enrol.complete',
    ]);
    expect(w.enrol.complete).toHaveBeenCalledWith(RECOVERY_KEY, FETCHED, 'web');
    expect(w.account()).toMatchObject({ phase: 'ready', user: { id: toHex(USER), username: 'web' }, deviceId: toHex(DEVICE), signIn: null, error: null });
    const phases = phasesOf(w);
    expect(phases.indexOf('enrolling')).toBeGreaterThan(phases.indexOf('signin-key'));
    expect(phases.indexOf('ready')).toBeGreaterThan(phases.indexOf('enrolling'));
  });

  it('asks for the second factor when the login needs one', async () => {
    const w = world({ phase: 0 });
    w.enrol.login.mockImplementationOnce(() => { w.calls.push('enrol.login'); return Promise.resolve({ needs: 1 as const }); });
    await toKeyStep(w);
    expect(w.ret(3)).toEqual({ t: 'ret', id: 3, ok: true, value: { needs: 1 } });
    expect(w.account()).toMatchObject({ phase: 'signin-totp', signIn: { username: 'web' } });
    expect(w.enrol.register).not.toHaveBeenCalled();
    w.call(4, { m: 'signInTotp', code: '12345' });
    await vi.waitFor(() => expect(w.ret(4)).toBeDefined());
    expect(w.ret(4)).toMatchObject({ ok: false, error: { code: 'E_BAD_INPUT', detail: '' } });
    w.call(5, { m: 'signInTotp', code: '123456' });
    await vi.waitFor(() => expect(w.ret(5)).toEqual({ t: 'ret', id: 5, ok: true, value: null }));
    expect(w.enrol.totp).toHaveBeenCalledWith('123456');
    expect(w.account()?.phase).toBe('signin-key');
    w.call(6, KEY);
    await vi.waitFor(() => expect(w.ret(6)).toEqual({ t: 'ret', id: 6, ok: true, value: null }));
    expect(w.calls.filter((c) => c.startsWith('enrol.'))).toEqual(['enrol.login', 'enrol.totp', 'enrol.register', 'enrol.fetch', 'enrol.complete']);
    expect(w.account()?.phase).toBe('ready');
  });

  it('a refused login stays at the login step with the error', async () => {
    const w = world({ phase: 0 });
    w.enrol.login.mockImplementationOnce(() => Promise.reject(refusal(401, 'E_UNAUTHENTICATED')));
    await toKeyStep(w);
    const want = { code: 'E_UNAUTHENTICATED', detail: '', status: 401, retryAfterMs: null };
    expect(w.ret(3)).toEqual({ t: 'ret', id: 3, ok: false, error: want });
    expect(w.account()).toMatchObject({ phase: 'signin-login', error: want });
    expect(w.enrol.register).not.toHaveBeenCalled();
  });

  it('a 429 from the login stays E_RATE_LIMITED', async () => {
    const w = world({ phase: 0 });
    w.enrol.login.mockImplementationOnce(() => Promise.reject(refusal(429, 'E_RATE_LIMITED', 5_000)));
    await toKeyStep(w);
    expect(w.ret(3)).toEqual({ t: 'ret', id: 3, ok: false, error: { code: 'E_RATE_LIMITED', detail: '', status: 429, retryAfterMs: 5_000 } });
    expect(w.account()).toMatchObject({ phase: 'signin-login', error: { code: 'E_RATE_LIMITED' } });
  });

  it('a register refused at the device cap keeps the enrol record and returns to the login step with the 403', async () => {
    const w = world({ phase: 0 });
    w.enrol.register.mockImplementationOnce(() => { w.state.phase = 3; return Promise.reject(refusal(403, 'E_FORBIDDEN')); });
    await toKeyStep(w);
    w.call(4, KEY);
    await vi.waitFor(() => expect(w.ret(4)).toBeDefined());
    expect(w.ret(4)).toEqual({ t: 'ret', id: 4, ok: false, error: { code: 'E_FORBIDDEN', detail: '', status: 403, retryAfterMs: null } });
    expect(w.enrol.fetch).not.toHaveBeenCalled();
    expect(w.enrol.reset).not.toHaveBeenCalled();
    expect(w.core.identity().phase).toBe(3);
    expect(w.account()).toMatchObject({ phase: 'signin-login', signIn: { username: 'web' }, error: { code: 'E_FORBIDDEN', status: 403 } });
    // Once a device was removed elsewhere, the next login and key register the kept enrolment again.
    w.call(5, LOGIN);
    await vi.waitFor(() => expect(w.ret(5)).toEqual({ t: 'ret', id: 5, ok: true, value: { needs: 0 } }));
    w.call(6, KEY);
    await vi.waitFor(() => expect(w.ret(6)).toEqual({ t: 'ret', id: 6, ok: true, value: null }));
    expect(w.enrol.register).toHaveBeenCalledTimes(2);
    expect(w.account()?.phase).toBe('ready');
  });

  // Head ruling 38 as amended: registration answers no per-user 429, so there is no E_ENROL_RATE; the establish
  // meter's short 429 crosses as E_RATE_LIMITED like the login's.
  it('a 429 from register crosses as E_RATE_LIMITED with its retry and keeps the enrol record', async () => {
    const w = world({ phase: 0 });
    w.enrol.register.mockImplementationOnce(() => { w.state.phase = 3; return Promise.reject(refusal(429, 'E_RATE_LIMITED', 1_000)); });
    await toKeyStep(w);
    w.call(4, KEY);
    await vi.waitFor(() => expect(w.ret(4)).toBeDefined());
    const want = { code: 'E_RATE_LIMITED', detail: '', status: 429, retryAfterMs: 1_000 };
    expect(w.ret(4)).toEqual({ t: 'ret', id: 4, ok: false, error: want });
    expect(w.account()).toMatchObject({ phase: 'signin-login', error: want });
    expect(w.enrol.reset).not.toHaveBeenCalled();
    expect(w.core.identity().phase).toBe(3);
  });

  it('a refused TOTP returns to signin-login with the username kept', async () => {
    const w = world({ phase: 0 });
    w.enrol.login.mockImplementationOnce(() => { w.calls.push('enrol.login'); return Promise.resolve({ needs: 1 as const }); });
    w.enrol.totp.mockImplementationOnce(() => Promise.reject(refusal(401, 'E_UNAUTHENTICATED')));
    await toKeyStep(w);
    expect(w.account()?.phase).toBe('signin-totp');
    w.call(4, { m: 'signInTotp', code: '123456' });
    await vi.waitFor(() => expect(w.ret(4)).toBeDefined());
    const want = { code: 'E_UNAUTHENTICATED', detail: '', status: 401, retryAfterMs: null };
    expect(w.ret(4)).toEqual({ t: 'ret', id: 4, ok: false, error: want });
    expect(w.account()).toMatchObject({ phase: 'signin-login', signIn: { username: 'web' }, error: want });
    expect(w.enrol.register).not.toHaveBeenCalled();
    w.call(5, { m: 'signInTotp', code: '123456' });
    await vi.waitFor(() => expect(w.ret(5)).toBeDefined());
    expect(w.ret(5)).toMatchObject({ ok: false, error: { code: 'E_NOT_READY', detail: '' } });
    expect(w.enrol.totp).toHaveBeenCalledTimes(1);
  });

  it('a second factor without a held assertion crosses as E_NO_ASSERTION and returns to signin-login', async () => {
    const w = world({ phase: 0 });
    w.enrol.login.mockImplementationOnce(() => { w.calls.push('enrol.login'); return Promise.resolve({ needs: 1 as const }); });
    w.enrol.totp.mockImplementationOnce(() => Promise.reject(new Error('E_NO_ASSERTION')));
    await toKeyStep(w);
    w.call(4, { m: 'signInTotp', code: '123456' });
    await vi.waitFor(() => expect(w.ret(4)).toBeDefined());
    const want = { code: 'E_NO_ASSERTION', detail: '', status: 0, retryAfterMs: null };
    expect(w.ret(4)).toEqual({ t: 'ret', id: 4, ok: false, error: want });
    expect(w.account()).toMatchObject({ phase: 'signin-login', signIn: { username: 'web' }, error: want });
  });

  it('a boot in phase 3 whose backup is missing publishes signin-key with E_NO_BACKUP', async () => {
    const w = world({ phase: 3, enrolUser: true });
    w.enrol.fetch.mockImplementationOnce(() => Promise.reject(new Error('E_NO_BACKUP')));
    w.call(1, { m: 'start' });
    await vi.waitFor(() => expect(w.account()?.phase).toBe('signin-key'));
    expect(w.account()?.error).toEqual({ code: 'E_NO_BACKUP', detail: '', status: 0, retryAfterMs: null });
    expect(w.enrol.reset).not.toHaveBeenCalled();
  });

  it('a missing backup keeps the key step with the error, and the key reads the backups again without registering again', async () => {
    const w = world({ phase: 0 });
    w.enrol.fetch.mockImplementationOnce(() => Promise.reject(new Error('E_NO_BACKUP')));
    await toKeyStep(w);
    w.call(4, KEY);
    await vi.waitFor(() => expect(w.ret(4)).toBeDefined());
    expect(w.ret(4)).toEqual({ t: 'ret', id: 4, ok: false, error: { code: 'E_NO_BACKUP', detail: '', status: 0, retryAfterMs: null } });
    expect(w.account()).toMatchObject({ phase: 'signin-key', error: { code: 'E_NO_BACKUP' } });
    w.call(5, { m: 'signInKey', recoveryKey: RECOVERY_KEY });
    await vi.waitFor(() => expect(w.ret(5)).toEqual({ t: 'ret', id: 5, ok: true, value: null }));
    expect(w.enrol.register).toHaveBeenCalledTimes(1);
    expect(w.enrol.fetch).toHaveBeenCalledTimes(2);
    expect(w.calls.filter((c) => c.startsWith('enrol.')).slice(-2)).toEqual(['enrol.fetch', 'enrol.complete']);
    expect(w.account()?.phase).toBe('ready');
  });

  it('a wrong recovery key returns to the key step and keeps the enrolment', async () => {
    const w = world({ phase: 0 });
    w.enrol.complete.mockImplementationOnce(() => Promise.reject(new CoreError('E_RECOVERY_KEY', '')));
    await toKeyStep(w);
    w.call(4, KEY);
    await vi.waitFor(() => expect(w.ret(4)).toBeDefined());
    expect(w.ret(4)).toEqual({ t: 'ret', id: 4, ok: false, error: { code: 'E_RECOVERY_KEY', detail: '', status: 0, retryAfterMs: null } });
    expect(w.account()).toMatchObject({ phase: 'signin-key', error: { code: 'E_RECOVERY_KEY' } });
    expect(w.enrol.reset).not.toHaveBeenCalled();
    expect(w.state.phase).toBe(3);
    w.call(5, { m: 'signInKey', recoveryKey: RECOVERY_KEY });
    await vi.waitFor(() => expect(w.ret(5)).toEqual({ t: 'ret', id: 5, ok: true, value: null }));
    expect(w.enrol.register).toHaveBeenCalledTimes(1);
    expect(w.enrol.fetch).toHaveBeenCalledTimes(1);
  });

  it('the password and the code come first; the key last, and a malformed key is refused before the registration spends the held assertion', async () => {
    const w = world({ phase: 0 });
    w.enrol.login.mockImplementationOnce(() => { w.calls.push('enrol.login'); return Promise.resolve({ needs: 1 as const }); });
    await toKeyStep(w);
    expect(w.ret(3)).toEqual({ t: 'ret', id: 3, ok: true, value: { needs: 1 } });
    w.call(4, { m: 'signInTotp', code: '123456' });
    await vi.waitFor(() => expect(w.ret(4)).toEqual({ t: 'ret', id: 4, ok: true, value: null }));
    expect(w.enrol.register).not.toHaveBeenCalled();
    w.core.recoveryKeyCheck.mockImplementationOnce(() => { w.calls.push('core.recoveryKeyCheck'); throw new CoreError('E_RECOVERY_KEY', ''); });
    w.call(5, { m: 'signInKey', recoveryKey: 'Z'.repeat(52) });
    await vi.waitFor(() => expect(w.ret(5)).toBeDefined());
    expect(w.ret(5)).toEqual({ t: 'ret', id: 5, ok: false, error: { code: 'E_RECOVERY_KEY', detail: '', status: 0, retryAfterMs: null } });
    expect(w.account()).toMatchObject({ phase: 'signin-key', error: { code: 'E_RECOVERY_KEY' } });
    expect(w.enrol.register).not.toHaveBeenCalled();
    w.call(6, KEY);
    await vi.waitFor(() => expect(w.ret(6)).toEqual({ t: 'ret', id: 6, ok: true, value: null }));
    expect(w.calls.filter((c) => c.startsWith('enrol.') || c === 'core.recoveryKeyCheck')).toEqual([
      'enrol.login', 'enrol.totp', 'core.recoveryKeyCheck', 'core.recoveryKeyCheck',
      'enrol.register', 'enrol.fetch', 'enrol.complete',
    ]);
  });

  it('a row evicted before the enrolment was written: the fetch answers 401, the enrolment is dropped and step 1 says why', async () => {
    const w = world({ phase: 0 });
    w.enrol.fetch.mockImplementationOnce(() => Promise.reject(refusal(401, 'E_UNAUTHENTICATED')));
    await toKeyStep(w);
    w.call(4, KEY);
    await vi.waitFor(() => expect(w.ret(4)).toBeDefined());
    const want = { code: 'E_SIGNIN_EVICTED', detail: '', status: 0, retryAfterMs: null };
    expect(w.ret(4)).toEqual({ t: 'ret', id: 4, ok: false, error: want });
    expect(w.enrol.reset).toHaveBeenCalledTimes(1);
    expect(w.account()).toMatchObject({ phase: 'signin-login', signIn: { username: 'web' }, error: want });
    expect(phasesOf(w)).not.toContain('revoked');
  });

  it('a row evicted after the enrolment was written: the store is wiped to cleared with E_SIGNIN_EVICTED, never revoked', async () => {
    const w = world({ phase: 0 });
    w.enrol.complete.mockImplementationOnce(() => { w.state.phase = 2; return Promise.reject(new Error('E_SIGNIN_EVICTED')); });
    await toKeyStep(w);
    w.call(4, KEY);
    await vi.waitFor(() => expect(w.ret(4)).toBeDefined());
    const want = { code: 'E_SIGNIN_EVICTED', detail: '', status: 0, retryAfterMs: null };
    expect(w.ret(4)).toEqual({ t: 'ret', id: 4, ok: false, error: want });
    expect(w.wiped).toEqual([INSTANCE_HEX]);
    expect(w.account()).toMatchObject({ phase: 'cleared', error: want });
    expect(phasesOf(w)).not.toContain('revoked');
  });

  it('a 401 on the list PUT and on its re-establish during the enrolment is the eviction, not a revocation', async () => {
    const paths: string[] = [];
    const base = fakeFetch(paths);
    const user = toHex(USER);
    const evicting = ((input: RequestInfo | URL, init?: RequestInit) => {
      const request = new Request(input, init);
      const path = new URL(request.url).pathname;
      paths.push(`${request.method} ${path}`);
      if (path === '/v1/backups/0/0') return Promise.resolve(cbor([ROOT, 1]));
      if (path === '/v1/backups/1/0') return Promise.resolve(cbor([STATE_OBJECT, 1]));
      if (path === `/v1/users/${user}/device-list` && request.method === 'GET') return Promise.resolve(cbor([1, new Uint8Array([9]), new Uint8Array(64), new Uint8Array(32)]));
      if (path.endsWith('/sessions/challenge')) return Promise.resolve(cbor([new Uint8Array(32), 1_700_000_060], 201));
      if (path === `/v1/users/${user}/device-list` || path.endsWith('/sessions')) return Promise.resolve(cbor(['E_UNAUTHENTICATED', '', null], 401));
      return base(input, init);
    }) as typeof fetch;
    const w = world({ phase: 3, enrolUser: true, realRoutes: true, realAccount: true, fetch: evicting });
    w.state.session = { token: 'pending', expires: NOW_S + 604_800n, idleExpires: NOW_S + 43_200n };
    w.call(1, { m: 'start' });
    await vi.waitFor(() => expect(w.account()?.phase).toBe('signin-key'));
    w.call(2, { m: 'signInKey', recoveryKey: RECOVERY_KEY });
    await vi.waitFor(() => expect(w.ret(2)).toBeDefined());
    const want = { code: 'E_SIGNIN_EVICTED', detail: '', status: 0, retryAfterMs: null };
    expect(w.ret(2)).toEqual({ t: 'ret', id: 2, ok: false, error: want });
    expect(paths).toContain(`PUT /v1/users/${user}/device-list`);
    expect(paths.filter((p) => p.endsWith('/sessions') && p.startsWith('POST')).length).toBeGreaterThanOrEqual(1);
    expect(w.account()).toMatchObject({ phase: 'cleared', error: want });
    expect(phasesOf(w)).not.toContain('revoked');
  });

  /** Fix-wave review NEW-1: another enrolled session cuts this browser's pending session after enrolComplete, before
   *  the list PUT. The row is live and unlisted, so the re-establish answers a pending scope; while the own list is
   *  unpublished that is this enrolment's own session, never a revocation. `cuts` is how many list PUTs answer 401. */
  function cutDuringEnrolment(cuts: number) {
    const paths: string[] = [];
    const base = fakeFetch(paths);
    const user = toHex(USER);
    let left = cuts;
    let published = false;
    const cutting = ((input: RequestInfo | URL, init?: RequestInit) => {
      const request = new Request(input, init);
      const path = new URL(request.url).pathname;
      paths.push(`${request.method} ${path}`);
      const list = `/v1/users/${user}/device-list`;
      if (path === '/v1/backups/0/0') return Promise.resolve(cbor([ROOT, 1]));
      if (path === '/v1/backups/1/0' && request.method === 'GET') return Promise.resolve(cbor([STATE_OBJECT, 1]));
      if (path === '/v1/backups/1/0') return Promise.resolve(cbor([new Uint8Array(32), 3]));
      if (path === list && request.method === 'GET') return Promise.resolve(cbor([1, new Uint8Array([9]), new Uint8Array(64), new Uint8Array(32)]));
      if (path === list) {
        if (left > 0) { left -= 1; return Promise.resolve(cbor(['E_UNAUTHENTICATED', '', null], 401)); }
        published = true;
        return Promise.resolve(new Response(null, { status: 204 }));
      }
      if (path.endsWith('/sessions/challenge')) return Promise.resolve(cbor([new Uint8Array(32), 1_700_000_060], 201));
      if (path.endsWith('/sessions')) {
        // The instance enrols the session only once the list names this device; before that the row is pending.
        return Promise.resolve(cbor([published ? 'enrolled' : 'pending-again', published ? 0 : 1, USER, DEVICE,
          1_700_604_800, 1_700_043_200, 7], 201));
      }
      if (path === '/v1/accounts/me') return Promise.resolve(cbor([USER, 'web', 'Web', 0, 0]));
      if (path === '/v1/communities' || path === '/v1/dms') return Promise.resolve(cbor([]));
      return base(input, init);
    }) as typeof fetch;
    const w = world({ phase: 3, enrolUser: true, realRoutes: true, realAccount: true, fetch: cutting });
    w.state.session = { token: 'pending', expires: NOW_S + 604_800n, idleExpires: NOW_S + 43_200n };
    return { w, paths, user };
  }

  it('a pending session cut once during the enrolment re-establishes pending, publishes and reaches ready, never revoked', async () => {
    const { w, paths, user } = cutDuringEnrolment(1);
    w.call(1, { m: 'start' });
    await vi.waitFor(() => expect(w.account()?.phase).toBe('signin-key'));
    w.call(2, { m: 'signInKey', recoveryKey: RECOVERY_KEY });
    await vi.waitFor(() => expect(w.ret(2)).toBeDefined());
    expect(w.ret(2)).toEqual({ t: 'ret', id: 2, ok: true, value: null });
    expect(paths.filter((p) => p === `PUT /v1/users/${user}/device-list`)).toHaveLength(2);
    expect(w.state.session?.token).toBe('enrolled');
    expect(w.account()?.phase).toBe('ready');
    expect(phasesOf(w)).not.toContain('revoked');
  });

  it('a pending session cut again after its pending re-establish is the eviction (E_SIGNIN_EVICTED), never revoked', async () => {
    const { w, paths, user } = cutDuringEnrolment(5);
    w.call(1, { m: 'start' });
    await vi.waitFor(() => expect(w.account()?.phase).toBe('signin-key'));
    w.call(2, { m: 'signInKey', recoveryKey: RECOVERY_KEY });
    await vi.waitFor(() => expect(w.ret(2)).toBeDefined());
    const want = { code: 'E_SIGNIN_EVICTED', detail: '', status: 0, retryAfterMs: null };
    expect(w.ret(2)).toEqual({ t: 'ret', id: 2, ok: false, error: want });
    // One re-establish, one retried PUT: the second 401 is not chased further.
    expect(paths.filter((p) => p === `PUT /v1/users/${user}/device-list`)).toHaveLength(2);
    expect(paths.filter((p) => p.endsWith('/sessions') && p.startsWith('POST'))).toHaveLength(1);
    expect(w.account()).toMatchObject({ phase: 'cleared', error: want });
    expect(phasesOf(w)).not.toContain('revoked');
  });

  it('a list race wipes the store and ends in cleared; nothing reopens it in this worker', async () => {
    const w = world({ phase: 0 });
    w.enrol.complete.mockImplementationOnce(() => { w.state.phase = 2; return Promise.reject(new Error('E_LIST_RACE')); });
    await toKeyStep(w);
    w.call(4, KEY);
    await vi.waitFor(() => expect(w.ret(4)).toBeDefined());
    const want = { code: 'E_LIST_RACE', detail: '', status: 0, retryAfterMs: null };
    expect(w.ret(4)).toEqual({ t: 'ret', id: 4, ok: false, error: want });
    expect(w.core.pause).toHaveBeenCalledTimes(1);
    expect(w.wiped).toEqual([INSTANCE_HEX]);
    expect(w.calls.indexOf('core.pause')).toBeLessThan(w.calls.indexOf('resetDevice'));
    expect(w.account()).toMatchObject({ phase: 'cleared', user: null, deviceId: null, recoveryKey: null, signIn: null, error: want });
    w.call(5, { m: 'signInBegin' });
    w.call(6, { m: 'signupBegin' });
    await vi.waitFor(() => expect(w.ret(6)).toBeDefined());
    expect(w.ret(5)).toMatchObject({ ok: false, error: { code: 'E_NOT_READY', detail: '' } });
    expect(w.ret(6)).toMatchObject({ ok: false, error: { code: 'E_NOT_READY', detail: '' } });
  });

  it('wave B task 3: signInBack from the code step drops the held assertion and returns to the login, username kept', async () => {
    const w = world({ phase: 0 });
    w.enrol.login.mockImplementationOnce(() => { w.calls.push('enrol.login'); return Promise.resolve({ needs: 1 as const, passkey: true }); });
    await toKeyStep(w);
    expect(w.account()).toMatchObject({ phase: 'signin-totp', signIn: { username: 'web', factor: 'totp', passkey: true } });
    w.call(4, { m: 'signInBack' });
    await vi.waitFor(() => expect(w.ret(4)).toEqual({ t: 'ret', id: 4, ok: true, value: null }));
    expect(w.enrol.reset).toHaveBeenCalledTimes(1);
    expect(w.account()).toMatchObject({ phase: 'signin-login', signIn: { username: 'web', factor: null }, error: null });
    expect(w.account()?.signIn).not.toHaveProperty('passkey');
    // Back at the login a code is refused: nothing half-signed-in is left to finish.
    w.call(5, { m: 'signInTotp', code: '123456' });
    await vi.waitFor(() => expect(w.ret(5)).toBeDefined());
    expect(w.ret(5)).toMatchObject({ ok: false });
    expect(w.enrol.totp).not.toHaveBeenCalled();
  });

  it('wave B task 3: signInBack from the key step drops the assertion; it is refused at the login itself', async () => {
    const w = world({ phase: 0 });
    await toKeyStep(w);
    expect(w.account()?.phase).toBe('signin-key');
    w.call(4, { m: 'signInBack' });
    await vi.waitFor(() => expect(w.ret(4)).toEqual({ t: 'ret', id: 4, ok: true, value: null }));
    expect(w.enrol.reset).toHaveBeenCalledTimes(1);
    expect(w.account()).toMatchObject({ phase: 'signin-login', error: null });
    w.call(5, { m: 'signInBack' });
    await vi.waitFor(() => expect(w.ret(5)).toBeDefined());
    expect(w.ret(5)).toMatchObject({ ok: false });
    expect(w.enrol.reset).toHaveBeenCalledTimes(1);
  });

  it('a login with no passkey flag leaves signIn without one', async () => {
    const w = world({ phase: 0 });
    w.enrol.login.mockImplementationOnce(() => Promise.resolve({ needs: 1 as const }));
    await toKeyStep(w);
    expect(w.account()?.signIn).toEqual({ username: 'web', factor: 'totp' });
  });

  it('wave B task 3 fix round 1: Back and Cancel are refused while a sign-in step runs, and change nothing', async () => {
    const w = world({ phase: 0 });
    let release!: () => void;
    w.enrol.login.mockImplementationOnce(() => Promise.resolve({ needs: 1 as const }));
    w.enrol.totp.mockImplementationOnce(() => new Promise<void>((resolve) => { release = resolve; }));
    await toKeyStep(w);
    expect(w.account()?.phase).toBe('signin-totp');
    w.call(4, { m: 'signInTotp', code: '123456' });
    await vi.waitFor(() => expect(w.enrol.totp).toHaveBeenCalledTimes(1));
    w.call(5, { m: 'signInBack' });
    w.call(6, { m: 'signInCancel' });
    await vi.waitFor(() => expect(w.ret(6)).toBeDefined());
    expect(w.ret(5)).toMatchObject({ ok: false, error: { code: 'E_NOT_READY' } });
    expect(w.ret(6)).toMatchObject({ ok: false, error: { code: 'E_NOT_READY' } });
    expect(w.enrol.reset).not.toHaveBeenCalled();
    release();
    await vi.waitFor(() => expect(w.ret(4)).toBeDefined());
    expect(w.ret(4)).toMatchObject({ ok: true });
    expect(w.account()?.phase).toBe('signin-key');
  });

  it('signInCancel resets the enrolment and returns to needs-signup', async () => {
    const w = world({ phase: 0 });
    w.enrol.login.mockImplementationOnce(() => { w.calls.push('enrol.login'); return Promise.resolve({ needs: 1 as const }); });
    await toKeyStep(w);
    w.call(4, { m: 'signInCancel' });
    await vi.waitFor(() => expect(w.ret(4)).toEqual({ t: 'ret', id: 4, ok: true, value: null }));
    expect(w.enrol.reset).toHaveBeenCalledTimes(1);
    expect(w.account()).toMatchObject({ phase: 'needs-signup', signIn: null, error: null });
  });

  it('a sign-in step in flight refuses a second one', async () => {
    const w = world({ phase: 0 });
    let release!: () => void;
    w.enrol.login.mockImplementationOnce(() => new Promise((resolve) => { release = () => resolve({ needs: 0 }); }));
    w.call(1, { m: 'start' });
    await vi.waitFor(() => expect(w.account()?.phase).toBe('needs-signup'));
    w.call(2, { m: 'signInBegin' });
    await vi.waitFor(() => expect(w.ret(2)).toBeDefined());
    w.call(3, LOGIN);
    w.call(4, LOGIN);
    await vi.waitFor(() => expect(w.ret(4)).toBeDefined());
    expect(w.ret(4)).toMatchObject({ ok: false, error: { code: 'E_NOT_READY', detail: '' } });
    release();
    await vi.waitFor(() => expect(w.ret(3)).toEqual({ t: 'ret', id: 3, ok: true, value: { needs: 0 } }));
  });

  it('a boot in phase 3 with a recorded user resumes at the key step and takes the username from the account', async () => {
    const w = world({ phase: 3, enrolUser: true });
    w.state.username = '';
    w.call(1, { m: 'start' });
    await vi.waitFor(() => expect(w.account()?.phase).toBe('signin-key'));
    expect(w.enrol.fetch).toHaveBeenCalledWith(USER);
    expect(w.account()?.signIn).toEqual({ username: null, factor: null });
    w.call(2, { m: 'signInKey', recoveryKey: RECOVERY_KEY });
    await vi.waitFor(() => expect(w.ret(2)).toEqual({ t: 'ret', id: 2, ok: true, value: null }));
    expect(w.enrol.complete).toHaveBeenCalledWith(RECOVERY_KEY, FETCHED, '');
    expect(w.routes.getAccountMe).toHaveBeenCalledTimes(1);
    expect(w.account()).toMatchObject({ phase: 'ready', user: { id: toHex(USER), username: 'web' } });
  });

  it('a boot in phase 3 without a recorded user resets to needs-signup', async () => {
    const w = world({ phase: 3 });
    w.call(1, { m: 'start' });
    await vi.waitFor(() => expect(w.account()?.phase).toBe('needs-signup'));
    expect(w.enrol.reset).toHaveBeenCalledTimes(1);
    expect(w.enrol.fetch).not.toHaveBeenCalled();
  });

  it('the password and the recovery key reach no slice, ret or error', async () => {
    const w = world({ phase: 0 });
    w.enrol.complete.mockImplementationOnce(() => Promise.reject(new CoreError('E_RECOVERY_KEY', '')));
    await toKeyStep(w);
    w.call(4, KEY);
    await vi.waitFor(() => expect(w.ret(4)).toBeDefined());
    w.call(5, { m: 'signInKey', recoveryKey: RECOVERY_KEY });
    await vi.waitFor(() => expect(w.ret(5)).toEqual({ t: 'ret', id: 5, ok: true, value: null }));
    const everything = JSON.stringify(w.posted);
    expect(everything).not.toContain(PASSWORD);
    expect(everything).not.toContain(RECOVERY_KEY);
    expect(everything).not.toContain(RECOVERY_KEY.slice(0, 13));
  });
});

// web3.8 C1 (R6): the password and the factor first, the recovery key last. The login and the factor never register;
// the worker holds the settled assertion, and the registration runs only inside signInKey, after the key's form check,
// back to back with the fetch, the enrolment and the list PUT (REGISTRATION-DEVICES-02 kept).
const BACKUP = 'A'.repeat(26);
const EXPIRED = { code: 'E_SIGNIN_EXPIRED', detail: '', status: 0, retryAfterMs: null };
const enrolCalls = (w: World): string[] => w.calls.filter((c) => c.startsWith('enrol.') || c === 'core.recoveryKeyCheck');

/** The instance's side of the 5-minute assertion (the fake server's TTL), over the real Enrol: an account whose first
 *  browser published list v1, and this browser enrolling. */
function instanceAccount(opts: { totp?: string; passkey?: boolean; codes?: string[] } = {}) {
  const server = new FakeServer();
  const first = new Uint8Array(16).fill(0xa1);
  const userId = server.register('ada', first);
  server.setPassword('ada', PASSWORD, opts.totp ?? null);
  server.publishList(userId, [{ deviceId: first, revokedAt: null }], 1n);
  if (opts.passkey === true) server.addPasskey('ada', 'Laptop');
  if (opts.codes !== undefined) server.factorsOf('ada').codes = [...opts.codes];
  const device = new Uint8Array(16).fill(0xb2);
  const core = new FakeCore({ deviceId: device });
  const { routes, session } = sessionFor(server, core);
  const enrol = new Enrol({ core, routes, session, now: () => FAKE_NOW_S * 1000 });
  return { server, enrol, device: toHex(device), userId };
}
async function statusOf(p: Promise<unknown>): Promise<number> {
  try { await p; } catch (e) { if (e instanceof DillaHttpError) return e.status; throw e; }
  return 0;
}

describe('Controller sign-in order (web3.8 C1, R6)', () => {
  it('signInLogin never registers', async () => {
    const w = world({ phase: 0 });
    await toLoginStep(w);
    w.call(3, LOGIN);
    await vi.waitFor(() => expect(w.ret(3)).toEqual({ t: 'ret', id: 3, ok: true, value: { needs: 0 } }));
    expect(enrolCalls(w)).toEqual(['enrol.login']);
    expect(w.enrol.register).not.toHaveBeenCalled();
    expect(w.account()).toMatchObject({ phase: 'signin-key', signIn: { username: 'web', factor: null }, error: null });
    expect(phasesOf(w)).not.toContain('enrolling');
    expect(w.core.recoveryKeyCheck).not.toHaveBeenCalled();
  });

  it('signInTotp holds the assertion and never registers', async () => {
    const w = world({ phase: 0 });
    w.enrol.login.mockImplementationOnce(() => { w.calls.push('enrol.login'); return Promise.resolve({ needs: 1 as const }); });
    await toLoginStep(w);
    w.call(3, LOGIN);
    await vi.waitFor(() => expect(w.ret(3)).toEqual({ t: 'ret', id: 3, ok: true, value: { needs: 1 } }));
    w.call(4, { m: 'signInTotp', code: '123456' });
    await vi.waitFor(() => expect(w.ret(4)).toEqual({ t: 'ret', id: 4, ok: true, value: null }));
    expect(enrolCalls(w)).toEqual(['enrol.login', 'enrol.totp']);
    expect(w.enrol.register).not.toHaveBeenCalled();
    expect(w.enrol.reset).not.toHaveBeenCalled();
    expect(w.account()).toMatchObject({ phase: 'signin-key', signIn: { username: 'web', factor: null }, error: null });
    expect(phasesOf(w)).not.toContain('enrolling');
  });

  it('signInPasskey with needs 0 goes to signin-key', async () => {
    const w = world({ phase: 0 });
    await toLoginStep(w);
    w.call(3, { m: 'signInPasskey', ceremonyId: 'ce'.repeat(16), response: '{"id":"x"}' });
    await vi.waitFor(() => expect(w.ret(3)).toEqual({ t: 'ret', id: 3, ok: true, value: { needs: 0 } }));
    expect(enrolCalls(w)).toEqual(['enrol.passkey']);
    expect(w.account()).toMatchObject({ phase: 'signin-key', signIn: { username: null, factor: null }, error: null });
  });

  it('signInKey registers, fetches, enrols and publishes back to back', async () => {
    const w = world({ phase: 0 });
    w.enrol.login.mockImplementationOnce(() => { w.calls.push('enrol.login'); return Promise.resolve({ needs: 1 as const }); });
    await toLoginStep(w);
    w.call(3, LOGIN);
    await vi.waitFor(() => expect(w.ret(3)).toBeDefined());
    w.call(4, { m: 'signInTotp', code: '123456' });
    await vi.waitFor(() => expect(w.ret(4)).toBeDefined());
    expect(w.account()?.phase).toBe('signin-key');
    const before = w.calls.length;
    w.call(5, { m: 'signInKey', recoveryKey: RECOVERY_KEY });
    await vi.waitFor(() => expect(w.ret(5)).toEqual({ t: 'ret', id: 5, ok: true, value: null }));
    expect(w.calls.slice(before).filter((c) => c.startsWith('enrol.') || c === 'core.recoveryKeyCheck')).toEqual([
      'core.recoveryKeyCheck', 'enrol.register', 'enrol.fetch', 'enrol.complete',
    ]);
    expect(w.enrol.register).toHaveBeenCalledWith(INSTANCE_ID);
    expect(w.enrol.complete).toHaveBeenCalledWith(RECOVERY_KEY, FETCHED, 'web');
    expect(w.account()).toMatchObject({ phase: 'ready', user: { id: toHex(USER), username: 'web' }, signIn: null, error: null });
    const phases = phasesOf(w);
    expect(phases.slice(phases.indexOf('signin-key'))).toEqual(expect.arrayContaining(['signin-key', 'enrolling', 'ready']));
  });

  it('a backup code is sent only with the key', async () => {
    const w = world({ phase: 0 });
    w.enrol.login.mockImplementationOnce(() => { w.calls.push('enrol.login'); return Promise.resolve({ needs: 2 as const }); });
    await toLoginStep(w);
    w.call(3, LOGIN);
    await vi.waitFor(() => expect(w.ret(3)).toEqual({ t: 'ret', id: 3, ok: true, value: { needs: 2 } }));
    expect(w.account()?.phase).toBe('signin-passkey');
    // Without the key the code is refused before anything is sent, and nothing is spent.
    w.call(4, { m: 'signInBackupCode', code: BACKUP });
    await vi.waitFor(() => expect(w.ret(4)).toMatchObject({ ok: false, error: { code: 'E_BAD_INPUT' } }));
    expect(w.enrol.backupCode).not.toHaveBeenCalled();
    w.call(5, { m: 'signInBackupCode', code: BACKUP, recoveryKey: RECOVERY_KEY });
    await vi.waitFor(() => expect(w.ret(5)).toEqual({ t: 'ret', id: 5, ok: true, value: null }));
    expect(enrolCalls(w)).toEqual(['enrol.login', 'core.recoveryKeyCheck', 'enrol.backupCode', 'enrol.register', 'enrol.fetch', 'enrol.complete']);
    expect(w.account()?.phase).toBe('ready');
  });

  it('an expired assertion at the key step returns to signin-login and spends nothing', async () => {
    const w = world({ phase: 0 });
    w.enrol.login.mockImplementationOnce(() => { w.calls.push('enrol.login'); return Promise.resolve({ needs: 1 as const }); });
    // The instance answers 401 to POST /v1/devices/{id}/sessions: the settled assertion ran out during key entry.
    w.enrol.register.mockImplementationOnce(() => { w.calls.push('enrol.register'); w.state.phase = 3; return Promise.reject(refusal(401, 'E_UNAUTHENTICATED')); });
    await toLoginStep(w);
    w.call(3, LOGIN);
    await vi.waitFor(() => expect(w.ret(3)).toBeDefined());
    w.call(4, { m: 'signInTotp', code: '123456' });
    await vi.waitFor(() => expect(w.ret(4)).toBeDefined());
    w.call(5, { m: 'signInKey', recoveryKey: RECOVERY_KEY });
    await vi.waitFor(() => expect(w.ret(5)).toBeDefined());
    expect(w.ret(5)).toEqual({ t: 'ret', id: 5, ok: false, error: EXPIRED });
    expect(w.account()).toMatchObject({ phase: 'signin-login', signIn: { username: 'web', factor: null }, error: EXPIRED });
    expect(w.enrol.fetch).not.toHaveBeenCalled();
    expect(w.enrol.complete).not.toHaveBeenCalled();
    // The kept enrolment (its device id) registers on the next sign-in.
    expect(w.enrol.reset).not.toHaveBeenCalled();
    // The instance's side: an assertion past its 5 minutes registers no row; one inside them does (control).
    const late = instanceAccount({ totp: '123456' });
    await late.enrol.login('ada', PASSWORD);
    await late.enrol.totp('123456');
    late.server.nowS += 301;
    expect(await statusOf(late.enrol.register(INSTANCE_ID))).toBe(401);
    expect(late.server.count('POST', `/v1/devices/${late.device}/sessions`)).toBe(1);
    expect(late.server.devices.has(late.device)).toBe(false);
    const prompt = instanceAccount({ totp: '123456' });
    await prompt.enrol.login('ada', PASSWORD);
    await prompt.enrol.totp('123456');
    prompt.server.nowS += 299;
    expect((await prompt.enrol.register(INSTANCE_ID)).userId).toEqual(prompt.userId);
  });

  it('an expired assertion on the backup path spends nothing and says enrol.expired', async () => {
    let clock = 1_700_000_000_000;
    const w = world({ phase: 0, now: () => clock });
    w.enrol.login.mockImplementationOnce(() => { w.calls.push('enrol.login'); return Promise.resolve({ needs: 2 as const }); });
    // The instance checks the assertion before the code (verifySecondFactor): an expired one is a 401 and no code is spent.
    w.enrol.backupCode.mockImplementationOnce(() => { w.calls.push('enrol.backupCode'); return Promise.reject(refusal(401, 'E_UNAUTHENTICATED')); });
    await toLoginStep(w);
    w.call(3, LOGIN);
    await vi.waitFor(() => expect(w.ret(3)).toBeDefined());
    clock += 300_001;   // the key read off paper for more than the assertion's 5 minutes
    w.call(4, { m: 'signInBackupCode', code: BACKUP, recoveryKey: RECOVERY_KEY });
    await vi.waitFor(() => expect(w.ret(4)).toBeDefined());
    expect(w.ret(4)).toEqual({ t: 'ret', id: 4, ok: false, error: EXPIRED });
    expect(w.account()).toMatchObject({ phase: 'signin-login', signIn: { username: 'web', factor: null }, error: EXPIRED });
    expect(w.enrol.register).not.toHaveBeenCalled();
    // The instance's side: past 5 minutes the code is not looked at and stays unspent; inside them it is spent (control).
    const late = instanceAccount({ passkey: true, codes: [BACKUP] });
    await late.enrol.login('ada', PASSWORD);
    late.server.nowS += 301;
    expect(await statusOf(late.enrol.backupCode(BACKUP))).toBe(401);
    expect(late.server.backupCodesOf('ada')).toEqual([BACKUP]);
    const prompt = instanceAccount({ passkey: true, codes: [BACKUP] });
    await prompt.enrol.login('ada', PASSWORD);
    prompt.server.nowS += 299;
    await prompt.enrol.backupCode(BACKUP);
    expect(prompt.server.backupCodesOf('ada')).toEqual([]);
  });

  it('a refused backup code inside the 5 minutes stays the refusal, not the expiry (control)', async () => {
    let clock = 1_700_000_000_000;
    const w = world({ phase: 0, now: () => clock });
    w.enrol.login.mockImplementationOnce(() => { w.calls.push('enrol.login'); return Promise.resolve({ needs: 2 as const }); });
    w.enrol.backupCode.mockImplementationOnce(() => Promise.reject(refusal(401, 'E_UNAUTHENTICATED')));
    await toLoginStep(w);
    w.call(3, LOGIN);
    await vi.waitFor(() => expect(w.ret(3)).toBeDefined());
    clock += 299_999;   // fix m1: one millisecond inside the 5 minutes pins the threshold at 300 000
    w.call(4, { m: 'signInBackupCode', code: BACKUP, recoveryKey: RECOVERY_KEY });
    await vi.waitFor(() => expect(w.ret(4)).toBeDefined());
    expect(w.ret(4)).toMatchObject({ ok: false, error: { code: 'E_UNAUTHENTICATED', status: 401 } });
    expect(w.account()).toMatchObject({ phase: 'signin-login', error: { code: 'E_UNAUTHENTICATED' } });
  });

  // Fix m2: a passkey that owes a code (needs 1) holds the assertion the backup code then spends; its age starts there.
  it('a passkey owing a code, then a backup code: a 401 past 5 minutes is the expiry, inside them the refusal', async () => {
    for (const [elapsed, want] of [[300_001, EXPIRED], [299_999, { code: 'E_UNAUTHENTICATED', detail: '', status: 401, retryAfterMs: null }]] as const) {
      let clock = 1_700_000_000_000;
      const w = world({ phase: 0, now: () => clock });
      w.enrol.passkey.mockImplementationOnce(() => { w.calls.push('enrol.passkey'); return Promise.resolve({ needs: 1 as const }); });
      w.enrol.backupCode.mockImplementationOnce(() => Promise.reject(refusal(401, 'E_UNAUTHENTICATED')));
      await toLoginStep(w);
      w.call(3, { m: 'signInPasskey', ceremonyId: 'ce'.repeat(16), response: '{"id":"x"}' });
      await vi.waitFor(() => expect(w.ret(3)).toEqual({ t: 'ret', id: 3, ok: true, value: { needs: 1 } }));
      expect(w.enrol.login).not.toHaveBeenCalled();
      clock += elapsed;
      w.call(4, { m: 'signInBackupCode', code: BACKUP, recoveryKey: RECOVERY_KEY });
      await vi.waitFor(() => expect(w.ret(4)).toBeDefined());
      expect(w.ret(4)).toEqual({ t: 'ret', id: 4, ok: false, error: want });
      expect(w.account()).toMatchObject({ phase: 'signin-login', error: want });
      expect(w.enrol.register).not.toHaveBeenCalled();
    }
  });

  // Fix m3: the age is elapsed time (deps.monotonic); a wall clock stepped forward does not age the assertion.
  it('a forward wall-clock jump does not turn a mistyped backup code into the expiry; monotonic time does', async () => {
    for (const [wall, mono, want] of [[600_000, 1_000, 'E_UNAUTHENTICATED'], [0, 300_001, 'E_SIGNIN_EXPIRED']] as const) {
      let clock = 1_700_000_000_000;
      let elapsed = 5_000;
      const w = world({ phase: 0, now: () => clock, monotonic: () => elapsed });
      w.enrol.login.mockImplementationOnce(() => { w.calls.push('enrol.login'); return Promise.resolve({ needs: 2 as const }); });
      w.enrol.backupCode.mockImplementationOnce(() => Promise.reject(refusal(401, 'E_UNAUTHENTICATED')));
      await toLoginStep(w);
      w.call(3, LOGIN);
      await vi.waitFor(() => expect(w.ret(3)).toBeDefined());
      clock += wall;
      elapsed += mono;
      w.call(4, { m: 'signInBackupCode', code: BACKUP, recoveryKey: RECOVERY_KEY });
      await vi.waitFor(() => expect(w.ret(4)).toBeDefined());
      expect(w.ret(4)).toMatchObject({ ok: false, error: { code: want } });
      expect(w.account()).toMatchObject({ phase: 'signin-login', error: { code: want } });
    }
  });

  // Fix m4: the key's form is checked before the code goes out, so a malformed key spends no backup code.
  it('a malformed key on the backup path leaves the code unspent and the step in place', async () => {
    const w = world({ phase: 0 });
    w.enrol.login.mockImplementationOnce(() => { w.calls.push('enrol.login'); return Promise.resolve({ needs: 2 as const }); });
    await toLoginStep(w);
    w.call(3, LOGIN);
    await vi.waitFor(() => expect(w.ret(3)).toBeDefined());
    expect(w.account()?.phase).toBe('signin-passkey');
    w.core.recoveryKeyCheck.mockImplementationOnce(() => { throw new CoreError('E_RECOVERY_KEY', ''); });
    w.call(4, { m: 'signInBackupCode', code: BACKUP, recoveryKey: 'Z'.repeat(52) });
    await vi.waitFor(() => expect(w.ret(4)).toBeDefined());
    expect(w.ret(4)).toEqual({ t: 'ret', id: 4, ok: false, error: { code: 'E_RECOVERY_KEY', detail: '', status: 0, retryAfterMs: null } });
    expect(w.enrol.backupCode).not.toHaveBeenCalled();
    expect(w.enrol.register).not.toHaveBeenCalled();
    expect(w.account()).toMatchObject({ phase: 'signin-passkey', signIn: { username: 'web' }, error: { code: 'E_RECOVERY_KEY' } });
    // Control: the same code with the right key is spent once and signs in.
    w.call(5, { m: 'signInBackupCode', code: BACKUP, recoveryKey: RECOVERY_KEY });
    await vi.waitFor(() => expect(w.ret(5)).toEqual({ t: 'ret', id: 5, ok: true, value: null }));
    expect(w.enrol.backupCode).toHaveBeenCalledTimes(1);
  });

  it('a malformed key at signin-key stays there with E_RECOVERY_KEY and keeps the assertion', async () => {
    const w = world({ phase: 0 });
    await toLoginStep(w);
    w.call(3, LOGIN);
    await vi.waitFor(() => expect(w.ret(3)).toBeDefined());
    w.core.recoveryKeyCheck.mockImplementationOnce(() => { w.calls.push('core.recoveryKeyCheck'); throw new CoreError('E_RECOVERY_KEY', ''); });
    w.call(4, { m: 'signInKey', recoveryKey: 'Z'.repeat(52) });
    await vi.waitFor(() => expect(w.ret(4)).toBeDefined());
    expect(w.ret(4)).toEqual({ t: 'ret', id: 4, ok: false, error: { code: 'E_RECOVERY_KEY', detail: '', status: 0, retryAfterMs: null } });
    expect(w.account()).toMatchObject({ phase: 'signin-key', signIn: { username: 'web' }, error: { code: 'E_RECOVERY_KEY' } });
    expect(w.enrol.register).not.toHaveBeenCalled();
    expect(w.enrol.reset).not.toHaveBeenCalled();
    w.call(5, { m: 'signInKey', recoveryKey: RECOVERY_KEY });
    await vi.waitFor(() => expect(w.ret(5)).toEqual({ t: 'ret', id: 5, ok: true, value: null }));
    expect(enrolCalls(w)).toEqual(['enrol.login', 'core.recoveryKeyCheck', 'core.recoveryKeyCheck', 'enrol.register', 'enrol.fetch', 'enrol.complete']);
    expect(w.account()?.phase).toBe('ready');
  });

  it('a cancel at signin-key drops the held assertion', async () => {
    const w = world({ phase: 0 });
    await toLoginStep(w);
    w.call(3, LOGIN);
    await vi.waitFor(() => expect(w.ret(3)).toBeDefined());
    expect(w.account()?.phase).toBe('signin-key');
    w.call(4, { m: 'signInCancel' });
    await vi.waitFor(() => expect(w.ret(4)).toEqual({ t: 'ret', id: 4, ok: true, value: null }));
    expect(w.enrol.reset).toHaveBeenCalledTimes(1);
    expect(w.enrol.register).not.toHaveBeenCalled();
    expect(w.account()).toMatchObject({ phase: 'needs-signup', signIn: null, error: null });
    // Enrol.reset is what drops it: the registration then has no assertion to spend.
    const a = instanceAccount();
    await a.enrol.login('ada', PASSWORD);
    a.enrol.reset();
    await expect(a.enrol.register(INSTANCE_ID)).rejects.toThrow('E_NO_ASSERTION');
  });
});

describe('Controller ready (L-TS-23, L-TS-24)', () => {
  it('enters ready in order: session, list, backups, own list, engine, gateway, every community, then the DMs', async () => {
    const w = world();
    await toReady(w);
    expect(w.calls.slice(0, 11)).toEqual([
      'session.ensure', 'publishDeviceList', 'ensureBackups', 'refreshOwnDeviceList', 'repairBackupState', 'sync.start', 'gateway.start',
      'listCommunities', 'listChannels', 'listMembers', 'listDms',
    ]);
    expect(w.session.establish).not.toHaveBeenCalled();
    expect(w.channels()).toHaveLength(1);
    expect(w.last('settings')).toEqual({});
    expect(w.notices()).toEqual({ nextId: 1, items: [] });
    expect(w.badges()).toEqual({});
  });

  it('a list unpublished at entry is followed by a fresh session', async () => {
    const w = world();
    const identity = w.core.identity;
    let published = false;
    w.core.identity = (): IdentityInfo => ({ ...identity(), listPublished: published });
    w.account.publishDeviceList.mockImplementationOnce(() => { w.calls.push('publishDeviceList'); published = true; return Promise.resolve(); });
    await toReady(w);
    expect(w.calls.slice(0, 3)).toEqual(['session.ensure', 'publishDeviceList', 'session.establish']);
  });

  it('an own list that no longer names this device ends in revoked', async () => {
    const w = world();
    w.account.refreshOwnDeviceList.mockImplementationOnce(() => Promise.resolve({ version: 3n, listed: false }));
    w.call(1, { m: 'start' });
    await vi.waitFor(() => expect(w.account()?.phase).toBe('revoked'));
    expect(w.gateway.starts).toBe(0);
    expect(phasesOf(w)).not.toContain('ready');
  });

  it('a pending scope in phase 2 is a revocation, not an error', async () => {
    const w = world();
    w.session.ensure.mockImplementationOnce(() => Promise.reject(new Error('E_SESSION_SCOPE')));
    w.call(1, { m: 'start' });
    await vi.waitFor(() => expect(w.account()?.phase).toBe('revoked'));
    expect(phasesOf(w)).not.toContain('error');
  });

  it('an unlisted answer to the list publication is a revocation', async () => {
    const w = world();
    w.account.publishDeviceList.mockImplementationOnce(() => Promise.reject(new Error('E_DEVICE_UNLISTED')));
    w.call(1, { m: 'start' });
    await vi.waitFor(() => expect(w.account()?.phase).toBe('revoked'));
  });

  it('a revoked socket whose re-establish answers a pending scope ends in revoked', async () => {
    const w = world();
    await toReady(w);
    w.session.establish.mockImplementationOnce(() => Promise.reject(new Error('E_SESSION_SCOPE')));
    w.gateway.emit({ type: 'revoked' });
    await vi.waitFor(() => expect(w.account()?.phase).toBe('revoked'));
    expect(w.sync.stop).toHaveBeenCalledTimes(1);
  });

  it('expects every channel group first, then every DM group with a null community', async () => {
    const w = world();
    w.state.dms = [{ channelId: DM_CHANNEL, kind: 3, members: [USER, STRANGER] }];
    await toReady(w);
    expect(w.sync.setExpected).toHaveBeenLastCalledWith([
      { groupId: GROUP, communityId: COMMUNITY, channelId: CHANNEL, policyVersion: 1n },
      { groupId: DM_GROUP, communityId: null, channelId: DM_CHANNEL, policyVersion: 1n },
    ]);
    expect(w.routes.getChannel).toHaveBeenCalledWith(DM_CHANNEL);
    expect(w.last<DmSummary[]>('dms')).toEqual([{
      id: toHex(DM_CHANNEL), kind: 3, members: [toHex(USER), toHex(STRANGER)], name: '5a5a5a5a', group: 'none',
    }]);
  });

  it('a ready reloads every community and the DMs and refetches the own list and the backups', async () => {
    const w = world();
    await toReady(w);
    w.gateway.emit({ type: 'ready', info: READY_INFO });
    await vi.waitFor(() => expect(w.routes.listDms).toHaveBeenCalledTimes(2));
    expect(w.routes.listChannels).toHaveBeenCalledTimes(2);
    expect(w.account.ensureBackups).toHaveBeenCalledTimes(2);
    expect(w.account.refreshOwnDeviceList).toHaveBeenCalledTimes(2);
    await vi.waitFor(() => expect(w.account.repairBackupState).toHaveBeenCalledTimes(2));
  });

  // BACKUPS-RECOVERY-03: the repair needs the refreshed own list, so it runs after it; a refusal waits for the next ready.
  it('repairs the state object after the own list at each ready, and a failed repair is not an error', async () => {
    const w = world();
    w.account.repairBackupState.mockImplementationOnce(() => { w.calls.push('repairBackupState'); return Promise.reject(refusal(429, 'E_RATE_LIMITED', 3_000)); });
    await toReady(w);
    const from = w.calls.length;
    w.gateway.emit({ type: 'ready', info: READY_INFO });
    await vi.waitFor(() => expect(w.account.repairBackupState).toHaveBeenCalledTimes(2));
    const after = w.calls.slice(from);
    expect(after.indexOf('refreshOwnDeviceList')).toBeLessThan(after.indexOf('repairBackupState'));
    w.account.refreshOwnDeviceList.mockImplementationOnce(() => Promise.resolve({ version: 2n, listed: false }));
    w.gateway.emit({ type: 'ready', info: READY_INFO });
    await vi.waitFor(() => expect(w.account()?.phase).toBe('revoked'));
    expect(w.account.repairBackupState).toHaveBeenCalledTimes(2);
  });

  it('badges: a group bound to no known channel contributes nothing', async () => {
    const w = world();
    await toReady(w);
    w.state.groups = [textGroup(2), { ...textGroup(2), groupId: ORPHAN_GROUP, targetId: ORPHAN_CHANNEL }];
    w.state.activity = [act(GROUP, 2, 1), act(ORPHAN_GROUP, 7, 3)];
    w.sync.deps!.onGroupChanged(GROUP, applied(2));
    expect(w.badges()).toEqual({ [toHex(CHANNEL)]: { unread: 2, mentions: 1 } });
  });

  it('notices: one per new message from another device; a counted mention is a mention', async () => {
    const w = world();
    await toReady(w);
    w.state.groups = [textGroup(2)];
    w.state.rows = [strangerRow(1)];
    w.state.activity = [act(GROUP, 1, 0)];
    w.sync.deps!.onGroupChanged(GROUP, appliedSeqs([1n]));
    expect(w.notices()).toEqual({ nextId: 2, items: [{
      id: 1, channelId: toHex(CHANNEL), communityId: toHex(COMMUNITY), kind: 'message', senderUser: toHex(STRANGER),
      senderName: '5a5a5a5a', body: 'from a stranger 1', ts: 1_700_000_000, threadRoot: null,
    }] });
    const mention: TimelineRow = { ...strangerRow(2), body: `<@${toHex(USER)}> look`, mention: true };
    const own: TimelineRow = { ...strangerRow(3), senderUser: USER, senderDevice: DEVICE, body: 'mine' };
    w.state.rows = [strangerRow(1), mention, own];
    w.state.activity = [act(GROUP, 2, 1)];
    w.sync.deps!.onGroupChanged(GROUP, appliedSeqs([2n, 3n]));
    expect(w.notices()?.items.map((n) => [n.id, n.kind, n.body])).toEqual([[1, 'message', 'from a stranger 1'], [2, 'mention', `<@${toHex(USER)}> look`]]);
    expect(w.notices()?.nextId).toBe(3);
    expect(w.badges()).toEqual({ [toHex(CHANNEL)]: { unread: 2, mentions: 1 } });
  });

  it('keeps a new core-authenticated mention when badges were refreshed before its notice', async () => {
    const w = world();
    await toReady(w);
    w.state.groups = [textGroup(2)];
    w.state.rows = [{ ...strangerRow(1), body: `<@${toHex(USER)}> look`, mention: true }];
    w.state.activity = [act(GROUP, 1, 1)];
    // A separate slice update can observe the new aggregate before the group
    // callback builds its notice. The row's authenticated flag must still win.
    (w.controller as unknown as { publishBadges(): void }).publishBadges();
    w.sync.deps!.onGroupChanged(GROUP, appliedSeqs([1n]));
    expect(w.notices()?.items.map((n) => n.kind)).toEqual(['mention']);
  });

  it("a message from this user's other device raises no notice and no badge", async () => {
    const w = world();
    await toReady(w);
    w.state.groups = [textGroup(2)];
    w.state.rows = [{ ...strangerRow(1), senderUser: USER, senderDevice: OTHER_DEVICE, body: 'typed in the other browser' }];
    w.state.activity = [act(GROUP, 0, 0)];   // L-CORE-24's counts exclude the own user's rows (task 1); the stub core answers as the real one
    w.sync.deps!.onGroupChanged(GROUP, appliedSeqs([1n]));
    expect(w.notices()).toEqual({ nextId: 1, items: [] });
    expect(w.badges()).toEqual({ [toHex(CHANNEL)]: { unread: 0, mentions: 0 } });
  });

  it('a group change posts badges, then notices, before it returns to the engine', async () => {
    const w = world();
    await toReady(w);
    w.state.groups = [textGroup(2)];
    w.state.rows = [strangerRow(1)];
    w.state.activity = [act(GROUP, 1, 0)];
    const from = w.posted.length;
    w.sync.deps!.onGroupChanged(GROUP, appliedSeqs([1n]));
    // No await between the report and the read: both slices were posted inside the synchronous handler.
    const names = w.posted.slice(from).filter((m): m is SliceMessage => m.t === 'slice').map((m) => m.name);
    expect(names.filter((n) => n === 'badges' || n === 'notices')).toEqual(['badges', 'notices']);
  });

  it('a live message held for its sender\'s trust badges and notifies once, when the trust refresh releases it (CI1)', async () => {
    const w = world();
    await toReady(w);
    const ch = toHex(CHANNEL);
    w.state.groups = [textGroup(2)];
    // The general channel is never opened: only the badges and notices slices can show the message.
    w.state.rows = [{ ...strangerRow(1), status: 1, reason: 'E_DEVICE_LIST_STALE', senderUser: null, msgId: null, type: null, body: '' }];
    w.state.activity = [act(GROUP, 0, 0)];
    w.sync.deps!.onGroupChanged(GROUP, appliedSeqs([1n]));
    expect(w.badges()).toEqual({ [ch]: { unread: 0, mentions: 0 } });
    expect(w.notices()).toEqual({ nextId: 1, items: [] });
    await settle();   // the ready's own trust refresh has ended

    w.core.pendingLeaves.mockReturnValue([{ groupId: GROUP, leafIndex: 1, userId: STRANGER, cachedVersion: 0n, reason: 'unknown' }]);
    w.core.revalidatePending.mockImplementation(() => {
      w.state.rows = [strangerRow(1)];
      w.state.activity = [act(GROUP, 1, 0)];
      w.core.trustGeneration.mockReturnValue(1n);
      return 1;
    });
    w.call(883, { m: 'refreshTrust' });
    await vi.waitFor(() => expect(w.ret(883)).toMatchObject({ ok: true }));
    expect(w.core.revalidatePending).toHaveBeenCalledWith(GROUP, 1, 256);
    expect(w.badges()).toEqual({ [ch]: { unread: 1, mentions: 0 } });
    expect(w.notices()).toEqual({ nextId: 2, items: [{
      id: 1, channelId: ch, communityId: toHex(COMMUNITY), kind: 'message', senderUser: toHex(STRANGER),
      senderName: '5a5a5a5a', body: 'from a stranger 1', ts: 1_700_000_000, threadRoot: null,
    }] });

    // A later trust change re-reads nothing it already noticed.
    w.core.trustGeneration.mockReturnValue(2n);
    w.sync.deps!.onTrustChanged();
    expect(w.notices()?.items).toHaveLength(1);
  });

  it(`the worker remembers at most ${HELD_LIVE_MAX} held live rows, dropping the oldest (CI1)`, async () => {
    const w = world();
    await toReady(w);
    w.state.groups = [textGroup(2)];
    const held = (seq: number): TimelineRow => ({ ...strangerRow(seq), status: 1, reason: 'E_DEVICE_LIST_STALE', senderUser: null, msgId: null, type: null, body: '' });
    const all = Array.from({ length: HELD_LIVE_MAX + 1 }, (_, i) => i + 1);
    for (const seq of all) {
      w.state.rows = all.filter((s) => s <= seq).map(held);
      w.sync.deps!.onGroupChanged(GROUP, appliedSeqs([BigInt(seq)]));
    }
    // Seq 1 is released first: it fell out of the remembered list, so it raises nothing; seq 2 is still remembered.
    w.state.rows = [strangerRow(1), ...all.slice(1).map(held)];
    w.core.trustGeneration.mockReturnValue(1n);
    w.sync.deps!.onTrustChanged();
    expect(w.notices()?.items).toEqual([]);
    w.state.rows = [strangerRow(1), strangerRow(2), ...all.slice(2).map(held)];
    w.core.trustGeneration.mockReturnValue(2n);
    w.sync.deps!.onTrustChanged();
    expect(w.notices()?.items.map((n) => n.body)).toEqual(['from a stranger 2']);
  });

  // web3.8 R8: the marker is the group head, but stops one below the oldest live-held row of that group that arrived
  // less than HELD_READ_HOLD_MS ago, so a row released later is unread, as a live arrival would be.
  const heldRow = (seq: number): TimelineRow => ({ ...strangerRow(seq), status: 1, reason: 'E_DEVICE_LIST_STALE', senderUser: null, msgId: null, type: null, body: '' });

  it('markRead stops one below a live-held row, and reaches the head once the row is released (R8)', async () => {
    const elapsed = { t: 5_000 };
    const w = world({ monotonic: () => elapsed.t });
    await toReady(w);
    const ch = toHex(CHANNEL);
    w.state.groups = [{ ...textGroup(2), nextSeq: 10n }];
    w.state.rows = [strangerRow(6), heldRow(7)];
    w.sync.deps!.onGroupChanged(GROUP, appliedSeqs([7n]));
    w.state.rows = [strangerRow(6), heldRow(7), strangerRow(8), heldRow(9)];
    w.sync.deps!.onGroupChanged(GROUP, appliedSeqs([8n, 9n]));
    elapsed.t += HELD_READ_HOLD_MS - 1;
    w.call(950, { m: 'markRead', channelId: ch });
    await vi.waitFor(() => expect(w.ret(950)).toEqual({ t: 'ret', id: 950, ok: true, value: null }));
    expect(w.core.markRead).toHaveBeenLastCalledWith(GROUP, 6n, NOW_S);

    // The trust refresh releases both rows: the next markRead goes to the head.
    w.state.rows = [strangerRow(6), strangerRow(7), strangerRow(8), strangerRow(9)];
    w.core.trustGeneration.mockReturnValue(1n);
    w.sync.deps!.onTrustChanged();
    w.call(951, { m: 'markRead', channelId: ch });
    await vi.waitFor(() => expect(w.ret(951)).toEqual({ t: 'ret', id: 951, ok: true, value: null }));
    expect(w.core.markRead).toHaveBeenLastCalledWith(GROUP, 9n, NOW_S);
  });

  it(`a row held ${String(HELD_READ_HOLD_MS)} ms or longer no longer holds the marker (R8)`, async () => {
    const elapsed = { t: 5_000 };
    const w = world({ monotonic: () => elapsed.t });
    await toReady(w);
    const ch = toHex(CHANNEL);
    w.state.groups = [{ ...textGroup(2), nextSeq: 10n }];
    w.state.rows = [strangerRow(6), heldRow(7)];
    w.sync.deps!.onGroupChanged(GROUP, appliedSeqs([7n]));
    // A second row held later, still young, holds the marker on its own once the first has aged out.
    elapsed.t += 30_000;
    w.state.rows = [strangerRow(6), heldRow(7), heldRow(8)];
    w.sync.deps!.onGroupChanged(GROUP, appliedSeqs([8n]));
    elapsed.t += HELD_READ_HOLD_MS - 30_000;
    w.call(952, { m: 'markRead', channelId: ch });
    await vi.waitFor(() => expect(w.ret(952)).toMatchObject({ ok: true }));
    expect(w.core.markRead).toHaveBeenLastCalledWith(GROUP, 7n, NOW_S);
    elapsed.t += 30_000;
    w.call(953, { m: 'markRead', channelId: ch });
    await vi.waitFor(() => expect(w.ret(953)).toMatchObject({ ok: true }));
    expect(w.core.markRead).toHaveBeenLastCalledWith(GROUP, 9n, NOW_S);
  });

  it('a row held in another group does not hold this group\'s marker (R8)', async () => {
    const elapsed = { t: 5_000 };
    const w = world({ monotonic: () => elapsed.t });
    w.state.dms = [{ channelId: DM_CHANNEL, kind: 3, members: [USER, STRANGER] }];
    await toReady(w);
    w.state.groups = [{ ...textGroup(2), nextSeq: 10n }, { ...textGroup(2), groupId: DM_GROUP, communityId: null, targetId: DM_CHANNEL, nextSeq: 10n }];
    w.state.rows = [heldRow(3)];
    w.sync.deps!.onGroupChanged(DM_GROUP, appliedSeqs([3n]));
    w.state.rows = [strangerRow(9)];
    w.call(954, { m: 'markRead', channelId: toHex(CHANNEL) });
    await vi.waitFor(() => expect(w.ret(954)).toMatchObject({ ok: true }));
    expect(w.core.markRead).toHaveBeenLastCalledWith(GROUP, 9n, NOW_S);
    // Control: the DM's own marker is held.
    w.call(955, { m: 'markRead', channelId: toHex(DM_CHANNEL) });
    await vi.waitFor(() => expect(w.ret(955)).toMatchObject({ ok: true }));
    expect(w.core.markRead).toHaveBeenLastCalledWith(DM_GROUP, 2n, NOW_S);
  });

  // C3 fix I2: the page sends markRead only when the unread count changes, and a held row counts as nothing, so the
  // worker re-applies a held-back markRead itself on one timer per group, at the moment its oldest young held row
  // reaches HELD_READ_HOLD_MS. It never reads past the head it was asked. Fix round 2 (C1): a release drops the held-back
  // read and its timer instead; the released row is unread, and the page marks it (the release raises the count).
  it('a readable row above a long-held row loses its badge at age-out, with no new message and no reopen (R8, I2)', async () => {
    const elapsed = { t: 5_000 };
    const w = world({ monotonic: () => elapsed.t });
    await toReady(w);
    const ch = toHex(CHANNEL);
    w.state.groups = [{ ...textGroup(2), nextSeq: 9n }];
    w.state.rows = [strangerRow(6), heldRow(7)];
    w.sync.deps!.onGroupChanged(GROUP, appliedSeqs([7n]));
    w.state.rows = [strangerRow(6), heldRow(7), strangerRow(8)];
    w.state.activity = [act(GROUP, 1, 0)];
    w.sync.deps!.onGroupChanged(GROUP, appliedSeqs([8n]));
    expect(w.badges()).toEqual({ [ch]: { unread: 1, mentions: 0 } });
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    try {
      w.call(960, { m: 'markRead', channelId: ch });
      await vi.advanceTimersByTimeAsync(0);
      expect(w.ret(960)).toEqual({ t: 'ret', id: 960, ok: true, value: null });
      expect(w.core.markRead).toHaveBeenLastCalledWith(GROUP, 6n, NOW_S);
      // A second markRead while still held keeps one timer for the group.
      w.call(961, { m: 'markRead', channelId: ch });
      await vi.advanceTimersByTimeAsync(0);
      expect(vi.getTimerCount()).toBe(1);
      const marks = w.core.markRead.mock.calls.length;
      w.core.markRead.mockImplementation(() => { w.state.activity = [act(GROUP, 0, 0)]; });
      elapsed.t += HELD_READ_HOLD_MS - 1;
      await vi.advanceTimersByTimeAsync(HELD_READ_HOLD_MS - 1);
      expect(w.core.markRead).toHaveBeenCalledTimes(marks);
      expect(w.badges()).toEqual({ [ch]: { unread: 1, mentions: 0 } });
      elapsed.t += 1;
      await vi.advanceTimersByTimeAsync(1);
      expect(w.core.markRead).toHaveBeenCalledTimes(marks + 1);
      expect(w.core.markRead).toHaveBeenLastCalledWith(GROUP, 8n, NOW_S);
      expect(w.badges()).toEqual({ [ch]: { unread: 0, mentions: 0 } });
      expect(vi.getTimerCount()).toBe(0);
    } finally {
      vi.useRealTimers();
    }
  });

  /** A ready world with row 7 held live and row 8 readable (head 8), and the page's markRead held back at 6. */
  async function heldBack(elapsed: { t: number }): Promise<World> {
    const w = world({ monotonic: () => elapsed.t });
    await toReady(w);
    w.state.groups = [{ ...textGroup(2), nextSeq: 9n }];
    w.state.rows = [strangerRow(6), heldRow(7)];
    w.sync.deps!.onGroupChanged(GROUP, appliedSeqs([7n]));
    w.state.rows = [strangerRow(6), heldRow(7), strangerRow(8)];
    w.state.activity = [act(GROUP, 1, 0)];
    w.sync.deps!.onGroupChanged(GROUP, appliedSeqs([8n]));
    w.call(962, { m: 'markRead', channelId: toHex(CHANNEL) });
    await vi.advanceTimersByTimeAsync(0);
    expect(w.ret(962)).toMatchObject({ ok: true });
    expect(w.core.markRead).toHaveBeenLastCalledWith(GROUP, 6n, NOW_S);
    return w;
  }
  const heldReadCount = (w: World): number => (w.controller as unknown as { heldReads: Map<string, unknown> }).heldReads.size;
  const release = (w: World): void => {
    w.state.rows = [strangerRow(6), strangerRow(7), strangerRow(8)];
    w.state.activity = [act(GROUP, 2, 0)];
    w.core.trustGeneration.mockReturnValue(1n);
    w.sync.deps!.onTrustChanged();
  };

  it('a release marks nothing: the released row stays unread, and no held-back read or timer is left (R8, C1)', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    try {
      const w = await heldBack({ t: 5_000 });
      expect(vi.getTimerCount()).toBe(1);
      const marks = w.core.markRead.mock.calls.length;
      release(w);
      expect(w.core.markRead).toHaveBeenCalledTimes(marks);
      expect(w.badges()).toEqual({ [toHex(CHANNEL)]: { unread: 2, mentions: 0 } });
      expect(heldReadCount(w)).toBe(0);
      expect(vi.getTimerCount()).toBe(0);
    } finally {
      vi.useRealTimers();
    }
  });

  it('a row released before its age-out leaves nothing for the timer to do (R8, C1)', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    try {
      const elapsed = { t: 5_000 };
      const w = await heldBack(elapsed);
      elapsed.t += 30_000;
      await vi.advanceTimersByTimeAsync(30_000);
      release(w);
      const marks = w.core.markRead.mock.calls.length;
      elapsed.t += HELD_READ_HOLD_MS;
      await vi.advanceTimersByTimeAsync(HELD_READ_HOLD_MS);
      expect(w.core.markRead).toHaveBeenCalledTimes(marks);
      expect(w.badges()).toEqual({ [toHex(CHANNEL)]: { unread: 2, mentions: 0 } });
    } finally {
      vi.useRealTimers();
    }
  });

  it('forgetting the account (a sign-out wipe) drops the held-back reads (C3 m5)', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    try {
      const w = await heldBack({ t: 5_000 });
      expect(heldReadCount(w)).toBe(1);
      w.call(963, { m: 'signOutRevoke', recoveryKey: RECOVERY_KEY });
      await vi.waitFor(() => expect(w.ret(963)).toMatchObject({ ok: true }));
      expect(w.account()?.phase).toBe('cleared');
      expect(heldReadCount(w)).toBe(0);
    } finally {
      vi.useRealTimers();
    }
  });

  it('a revocation drops the held-back reads (C3 m6)', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    try {
      const w = await heldBack({ t: 5_000 });
      expect(heldReadCount(w)).toBe(1);
      vi.mocked(w.parts.refreshOwnDeviceList!).mockResolvedValueOnce({ version: 1n, listed: false });
      w.call(964, { m: 'refreshDevices' });
      await vi.waitFor(() => expect(w.ret(964)).toMatchObject({ ok: true }));
      expect(w.account()?.phase).toBe('revoked');
      expect(heldReadCount(w)).toBe(0);
    } finally {
      vi.useRealTimers();
    }
  });

  it('an unexpected Welcome reloads the DMs, coalesced, and expects the new DM group', async () => {
    const w = world();
    await toReady(w);
    const before = w.routes.listDms.mock.calls.length;
    let release!: () => void;
    w.routes.listDms.mockImplementationOnce(() => {
      w.calls.push('listDms');
      return new Promise<typeof w.state.dms>((resolve) => { release = () => resolve([]); });
    });
    w.sync.deps!.onUnexpectedWelcome(DM_GROUP);
    w.sync.deps!.onUnexpectedWelcome(DM_GROUP);
    w.sync.deps!.onUnexpectedWelcome(DM_GROUP);
    expect(w.routes.listDms).toHaveBeenCalledTimes(before + 1);
    w.state.dms = [{ channelId: DM_CHANNEL, kind: 3, members: [USER, STRANGER] }];
    release();
    await vi.waitFor(() => expect(w.routes.listDms).toHaveBeenCalledTimes(before + 2));
    await vi.waitFor(() => expect(w.sync.setExpected).toHaveBeenLastCalledWith(expect.arrayContaining([
      { groupId: DM_GROUP, communityId: null, channelId: DM_CHANNEL, policyVersion: 1n },
    ])));
    await new Promise((resolve) => { setTimeout(resolve, 20); });
    expect(w.routes.listDms).toHaveBeenCalledTimes(before + 2);
  });

  it('notices: the ring keeps the newest 32', async () => {
    const w = world();
    await toReady(w);
    w.state.groups = [textGroup(2)];
    for (let batch = 0; batch < 5; batch++) {
      const seqs = Array.from({ length: 8 }, (_, i) => batch * 8 + i + 1);
      w.state.rows = seqs.map((s) => strangerRow(s));
      w.state.activity = [act(GROUP, seqs.at(-1)!, 0)];
      w.sync.deps!.onGroupChanged(GROUP, appliedSeqs(seqs.map(BigInt)));
    }
    const notices = w.notices()!;
    expect(notices.nextId).toBe(41);
    expect(notices.items).toHaveLength(32);
    expect(notices.items[0]).toMatchObject({ id: 9, body: 'from a stranger 9' });
    expect(notices.items.at(-1)).toMatchObject({ id: 40, body: 'from a stranger 40' });
  });

  it('markRead marks the newest stored seq and republishes the badges', async () => {
    const w = world();
    await toReady(w);
    await openGeneral(w);
    w.state.rows = [strangerRow(1), strangerRow(3)];
    w.state.activity = [act(GROUP, 2, 0)];
    w.sync.deps!.onGroupChanged(GROUP, applied(2));
    expect(w.badges()).toEqual({ [toHex(CHANNEL)]: { unread: 2, mentions: 0 } });
    w.core.markRead.mockImplementationOnce(() => { w.state.activity = [act(GROUP, 0, 0)]; });
    w.call(4, { m: 'markRead', channelId: toHex(CHANNEL) });
    await vi.waitFor(() => expect(w.ret(4)).toEqual({ t: 'ret', id: 4, ok: true, value: null }));
    expect(w.core.markRead).toHaveBeenCalledWith(GROUP, 3n, NOW_S);
    expect(w.badges()).toEqual({ [toHex(CHANNEL)]: { unread: 0, mentions: 0 } });
    w.call(5, { m: 'markRead', channelId: 'e'.repeat(32) });
    await vi.waitFor(() => expect(w.ret(5)).toBeDefined());
    expect(w.ret(5)).toMatchObject({ ok: false, error: { code: 'E_BAD_INPUT', detail: '' } });
  });

  it('setSetting stores a known key, deletes on null, and refuses everything else', async () => {
    const w = world();
    await toReady(w);
    w.call(2, { m: 'setSetting', key: 'notify.default', value: 'everything' });
    await vi.waitFor(() => expect(w.ret(2)).toEqual({ t: 'ret', id: 2, ok: true, value: null }));
    expect(w.last('settings')).toEqual({ 'notify.default': 'everything' });
    w.call(3, { m: 'setSetting', key: 'notify.default', value: null });
    await vi.waitFor(() => expect(w.ret(3)).toEqual({ t: 'ret', id: 3, ok: true, value: null }));
    expect(w.last('settings')).toEqual({});
    w.call(4, { m: 'setSetting', key: 'theme', value: 'mesh' });
    w.call(5, { m: 'setSetting', key: 'notify.default', value: 'loud' });
    w.call(6, { m: 'setSetting', key: 'notify.default', value: 3 });
    await vi.waitFor(() => expect(w.ret(6)).toBeDefined());
    await vi.waitFor(() => expect(w.ret(5)).toBeDefined());
    expect(w.ret(4)).toEqual({ t: 'ret', id: 4, ok: false, error: { code: 'E_SETTING_KEY', detail: '', status: 0, retryAfterMs: null } });
    expect(w.ret(5)).toMatchObject({ ok: false, error: { code: 'E_BAD_INPUT', detail: '' } });
    expect(w.ret(6)).toMatchObject({ ok: false, error: { code: 'E_BAD_INPUT', detail: '' } });
    expect(w.core.settingPut).toHaveBeenCalledTimes(1);
  });

  it('openDm registers the conversation, publishes it, expects its group and opens it like a channel', async () => {
    const w = world();
    await toReady(w);
    w.call(2, { m: 'openDm', userId: toHex(STRANGER) });
    await vi.waitFor(() => expect(w.ret(2)).toEqual({ t: 'ret', id: 2, ok: true, value: { channelId: toHex(DM_CHANNEL) } }));
    expect(w.routes.postDm).toHaveBeenCalledWith([STRANGER]);
    expect(w.last<DmSummary[]>('dms')?.map((d) => d.id)).toEqual([toHex(DM_CHANNEL)]);
    expect(w.sync.setExpected).toHaveBeenLastCalledWith(expect.arrayContaining([
      { groupId: DM_GROUP, communityId: null, channelId: DM_CHANNEL, policyVersion: 1n },
    ]));
    w.sync.openChannel.mockImplementationOnce(() => Promise.resolve({ groupId: DM_GROUP, state: 2 as const }));
    w.state.groups = [{ ...textGroup(2), groupId: DM_GROUP, communityId: null, targetId: DM_CHANNEL }];
    w.call(3, { m: 'openChannel', channelId: toHex(DM_CHANNEL) });
    await vi.waitFor(() => expect(w.ret(3)).toEqual({ t: 'ret', id: 3, ok: true, value: null }));
    expect(w.sync.openChannel).toHaveBeenLastCalledWith({ communityId: null, channelId: DM_CHANNEL, textGroupId: DM_GROUP });
    expect(w.last<TimelineState>(`timeline:${toHex(DM_CHANNEL)}`)).toMatchObject({ channelId: toHex(DM_CHANNEL), group: 'active' });
    expect(w.last<DmSummary[]>('dms')?.[0]?.group).toBe('active');
  });
});

describe('Controller devices (L-TS-23, Q13)', () => {
  const LISTED_BOTH: OwnDeviceList = { version: 2n, published: true, entries: [
    { deviceId: DEVICE, dskPub: new Uint8Array(32), tier: 1, addedAt: 1n, revokedAt: null },
    { deviceId: OTHER_DEVICE, dskPub: new Uint8Array(32), tier: 1, addedAt: 2n, revokedAt: null },
  ] };

  it('refreshDevices refetches the own list first and publishes the account devices, this browser first', async () => {
    const w = world();
    await toReady(w);
    const from = w.calls.length;
    w.call(2, { m: 'refreshDevices' });
    await vi.waitFor(() => expect(w.ret(2)).toEqual({ t: 'ret', id: 2, ok: true, value: null }));
    const after = w.calls.slice(from);
    expect(after.indexOf('refreshOwnDeviceList')).toBeGreaterThanOrEqual(0);
    expect(after.indexOf('refreshOwnDeviceList')).toBeLessThan(after.indexOf('listDevices'));
    expect(w.last<DeviceSummary[]>('devices')).toEqual([
      { id: toHex(DEVICE), tier: 1, signerTier: 1, lastSeen: 1_700_000_000, revokedAt: null, listed: true, own: true, label: 'work laptop' },
      { id: toHex(OTHER_DEVICE), tier: 1, signerTier: 1, lastSeen: 1_699_999_000, revokedAt: null, listed: false, own: false, label: null },
    ]);
  });

  it('renameDevice names the device on the server, then republishes the device list with the name', async () => {
    const w = world();
    await toReady(w);
    w.call(2, { m: 'renameDevice', deviceId: toHex(OTHER_DEVICE), label: '  phone  ' });
    w.call(4, { m: 'renameDevice', deviceId: toHex(OTHER_DEVICE), label: 'cafe\u0301' });
    await vi.waitFor(() => expect(w.ret(2)).toEqual({ t: 'ret', id: 2, ok: true, value: null }));
    expect(w.routes.patchDevice).toHaveBeenCalledWith(OTHER_DEVICE, 'phone');
    await vi.waitFor(() => expect(w.ret(4)).toEqual({ t: 'ret', id: 4, ok: true, value: null }));
    expect(w.routes.patchDevice).toHaveBeenCalledWith(OTHER_DEVICE, 'caf\u00e9');
    expect(w.calls.lastIndexOf('listDevices')).toBeGreaterThan(w.calls.indexOf('patchDevice'));
    w.call(3, { m: 'renameDevice', deviceId: toHex(OTHER_DEVICE), label: null });
    await vi.waitFor(() => expect(w.ret(3)).toEqual({ t: 'ret', id: 3, ok: true, value: null }));
    expect(w.routes.patchDevice).toHaveBeenLastCalledWith(OTHER_DEVICE, null);
  });

  it('renameDevice refuses a blank, a too long, a control-character or a non-string label, and a bad id, before any request', async () => {
    const w = world();
    await toReady(w);
    const bad: unknown[] = ['', '   ', 'x'.repeat(65), 'two\nlines', 7, undefined, 'a\u202eb', 'a\u2066b', 'a\u200fb', 'a\u200eb',
      'a\u2028b', 'a\u061cb', 'a\u200bb', 'a\u200db', 'a\ufeffb', 'a\u2060b', 'a\u2061b', 'a\u2065b', 'a\u206ab', 'a\u206fb', 'phone\u0085', 'a\u{E0041}b', 'a\u2800b'];
    bad.forEach((label, i) => w.call(10 + i, { m: 'renameDevice', deviceId: toHex(OTHER_DEVICE), label }));
    w.call(99, { m: 'renameDevice', deviceId: 'nothex', label: 'ok' });
    await vi.waitFor(() => expect(w.ret(99)).toBeDefined());
    for (let i = 0; i < bad.length; i++) {
      await vi.waitFor(() => expect(w.ret(10 + i)).toBeDefined());
      expect(w.ret(10 + i)).toMatchObject({ ok: false, error: { code: 'E_BAD_INPUT' } });
    }
    expect(w.ret(99)).toMatchObject({ ok: false, error: { code: 'E_BAD_INPUT' } });
    expect(w.routes.patchDevice).not.toHaveBeenCalled();
  });

  it('Task S1 fix round 1: a wire-4 head (five elements) revokes with its four-element chain row, never the raw head', async () => {
    const w = world();
    await toReady(w);
    const head5 = encode([1, new Uint8Array([9]), new Uint8Array(64), new Uint8Array(32), new Uint8Array([0x84])]);
    w.routes.getDeviceList.mockImplementation(() => Promise.resolve({ version: 1n, blob: new Uint8Array([9]), raw: head5 }));
    w.call(2, { m: 'signOutRevoke', recoveryKey: RECOVERY_KEY });
    await vi.waitFor(() => expect(w.ret(2)).toEqual({ t: 'ret', id: 2, ok: true, value: null }));
    expect(w.core.deviceListRevoke).toHaveBeenCalledWith(expect.objectContaining({
      listBody: encode([1, new Uint8Array([9]), new Uint8Array(64), new Uint8Array(32)]) }));
  });

  it('signOutRevoke writes the state object, stops the engine and the gateway, publishes the list, then wipes to cleared', async () => {
    const w = world();
    await toReady(w);
    const from = w.calls.length;
    w.call(2, { m: 'signOutRevoke', recoveryKey: RECOVERY_KEY });
    await vi.waitFor(() => expect(w.ret(2)).toEqual({ t: 'ret', id: 2, ok: true, value: null }));
    expect(w.calls.slice(from)).toEqual([
      'getBackup(0)', 'getBackup(1)', 'getDeviceList', 'core.deviceListRevoke', 'putBackup(1)',
      'sync.stop', 'gateway.stop', 'putDeviceList', 'core.pause', 'resetDevice',
    ]);
    expect(w.core.deviceListRevoke).toHaveBeenCalledWith({
      recoveryKey: RECOVERY_KEY, rootSealed: ROOT, stateSealed: STATE_OBJECT, listBody: LIST_RAW, deviceIds: [DEVICE], now: NOW_S,
    });
    expect(w.routes.putBackup).toHaveBeenCalledWith(1, new Uint8Array([8]));
    expect(w.routes.putDeviceList).toHaveBeenCalledWith(USER, new Uint8Array([7]));
    expect(w.account()).toMatchObject({ phase: 'cleared', user: null, deviceId: null, recoveryKey: null, signIn: null, error: null });
    expect(w.last('communities')).toEqual([]);
    expect(w.channels()).toEqual([]);
    expect(w.badges()).toEqual({});
    expect(w.last('settings')).toEqual({});
    expect(w.last('dms')).toEqual([]);
    expect(w.notices()).toEqual({ nextId: 1, items: [] });
  });

  it('a missing state object is not a refusal: the revocation signs over empty bytes', async () => {
    const w = world();
    await toReady(w);
    w.routes.getBackup.mockImplementation((kind: 0 | 1) => {
      w.calls.push(`getBackup(${kind})`);
      return Promise.resolve(kind === 0 ? { object: ROOT, created: 1n } : null as unknown as { object: Uint8Array; created: bigint });
    });
    w.call(2, { m: 'signOutRevoke', recoveryKey: RECOVERY_KEY });
    await vi.waitFor(() => expect(w.ret(2)).toEqual({ t: 'ret', id: 2, ok: true, value: null }));
    expect(w.core.deviceListRevoke).toHaveBeenCalledWith(expect.objectContaining({ rootSealed: ROOT, stateSealed: new Uint8Array(0) }));
  });

  it('a missing root refuses a revocation before anything is signed or stopped', async () => {
    const w = world();
    await toReady(w);
    w.routes.getBackup.mockImplementationOnce(() => Promise.resolve(null as unknown as { object: Uint8Array; created: bigint }));
    w.call(2, { m: 'signOutRevoke', recoveryKey: RECOVERY_KEY });
    await vi.waitFor(() => expect(w.ret(2)).toBeDefined());
    expect(w.ret(2)).toEqual({ t: 'ret', id: 2, ok: false, error: { code: 'E_NO_BACKUP', detail: '', status: 0, retryAfterMs: null } });
    expect(w.core.deviceListRevoke).not.toHaveBeenCalled();
    expect(w.gateway.stops).toBe(0);
    expect(w.account()?.phase).toBe('ready');
  });

  it('a refused sign-out leaves a working client: the engine is rebuilt and the gateway restarted', async () => {
    const w = world();
    await toReady(w);
    expect(w.sync.builds).toBe(1);
    w.routes.putDeviceList.mockImplementationOnce(() => { w.calls.push('putDeviceList'); return Promise.reject(refusal(500, 'E_INTERNAL')); });
    const from = w.calls.length;
    w.call(2, { m: 'signOutRevoke', recoveryKey: RECOVERY_KEY });
    await vi.waitFor(() => expect(w.ret(2)).toBeDefined());
    expect(w.ret(2)).toEqual({ t: 'ret', id: 2, ok: false, error: { code: 'E_INTERNAL', detail: '', status: 500, retryAfterMs: null } });
    const after = w.calls.slice(from);
    // BACKUPS-RECOVERY-02: the self-revoking candidate is dropped, so no later ready publishes it silently.
    // Changed by fix-wave review NEW-2: the instance's state object from before the sign-out is put back.
    expect(after.slice(after.indexOf('putDeviceList'))).toEqual(['putDeviceList', 'core.deviceListDrop', 'putBackup(1)', 'sync.start', 'gateway.start']);
    expect(w.sync.builds).toBe(2);
    expect(w.sync.setExpected).toHaveBeenLastCalledWith([{ groupId: GROUP, communityId: COMMUNITY, channelId: CHANNEL, policyVersion: 1n }]);
    expect(w.core.pause).not.toHaveBeenCalled();
    expect(w.account()?.phase).toBe('ready');
    // wiping is false again: a later revoked socket re-establishes as in web-1.
    w.gateway.emit({ type: 'revoked' });
    await vi.waitFor(() => expect(w.session.establish).toHaveBeenCalledTimes(1));
  });

  it('a sign-out whose state object is refused sends nothing more and drops its candidate (BACKUPS-RECOVERY-02)', async () => {
    const w = world();
    await toReady(w);
    w.routes.putBackup.mockImplementationOnce(() => { w.calls.push('putBackup(1)'); return Promise.reject(refusal(429, 'E_RATE_LIMITED', 70_000)); });
    const from = w.calls.length;
    w.call(2, { m: 'signOutRevoke', recoveryKey: RECOVERY_KEY });
    await vi.waitFor(() => expect(w.ret(2)).toBeDefined());
    expect(w.ret(2)).toMatchObject({ ok: false, error: { code: 'E_RATE_LIMITED' } });
    expect(w.calls.slice(from)).toEqual(['getBackup(0)', 'getBackup(1)', 'getDeviceList', 'core.deviceListRevoke', 'putBackup(1)', 'core.deviceListDrop']);
    expect(w.routes.putDeviceList).not.toHaveBeenCalled();
    expect(w.gateway.stops).toBe(0);
    expect(w.account()?.phase).toBe('ready');
  });

  /** Fix-wave review NEW-2: the instance keeps one state object. A sign-out whose state PUT landed and whose list PUT
   *  failed left the self-revoking state there, the served list's successor, so this browser's next revocation read it
   *  back as an interrupted publication and revoked itself. The failed sign-out puts the previous object back. */
  it('a revoke of another device after a failed sign-out revokes the target and keeps this browser listed', async () => {
    const w = world();
    await toReady(w);
    w.state.ownList = LISTED_BOTH;
    let stored: Uint8Array = STATE_OBJECT;
    w.routes.getBackup.mockImplementation((kind: 0 | 1) => {
      w.calls.push(`getBackup(${kind})`); return Promise.resolve({ object: kind === 0 ? ROOT : stored, created: 1n });
    });
    w.routes.putBackup.mockImplementation((kind: 0 | 1, object: Uint8Array) => {
      w.calls.push(`putBackup(${kind})`); stored = object; return Promise.resolve({ blobId: new Uint8Array(32), size: object.length, created: false });
    });
    const SELF_REVOKING = new Uint8Array([0x5e]);
    w.core.deviceListRevoke.mockImplementationOnce(() => {
      w.calls.push('core.deviceListRevoke'); return { deviceListBody: new Uint8Array([7]), stateSealed: SELF_REVOKING, interrupted: null };
    });
    w.routes.putDeviceList.mockImplementationOnce(() => { w.calls.push('putDeviceList'); return Promise.reject(refusal(500, 'E_INTERNAL')); });
    w.call(2, { m: 'signOutRevoke', recoveryKey: RECOVERY_KEY });
    await vi.waitFor(() => expect(w.ret(2)).toBeDefined());
    expect(w.ret(2)).toMatchObject({ ok: false, error: { code: 'E_INTERNAL' } });
    expect(w.routes.putBackup.mock.calls.map((c) => c[1])).toEqual([SELF_REVOKING, STATE_OBJECT]);
    expect(stored).toEqual(STATE_OBJECT);
    expect(w.core.deviceListDrop).toHaveBeenCalledTimes(1);

    w.call(3, { m: 'revokeDevice', deviceId: toHex(OTHER_DEVICE), recoveryKey: RECOVERY_KEY });
    await vi.waitFor(() => expect(w.ret(3)).toEqual({ t: 'ret', id: 3, ok: true, value: null }));
    expect(w.core.deviceListRevoke).toHaveBeenLastCalledWith(expect.objectContaining({ stateSealed: STATE_OBJECT, deviceIds: [OTHER_DEVICE] }));
    expect(w.account()?.phase).toBe('ready');
    expect(phasesOf(w)).not.toContain('revoked');
  });

  it('an interrupted publication the core signed on is published first: on a revocation, then the list, then the state', async () => {
    const w = world();
    await toReady(w);
    w.state.ownList = LISTED_BOTH;
    const interrupted = new Uint8Array([6]);
    w.core.deviceListRevoke.mockImplementationOnce(() => {
      w.calls.push('core.deviceListRevoke'); return { deviceListBody: new Uint8Array([7]), stateSealed: new Uint8Array([8]), interrupted };
    });
    const from = w.calls.length;
    w.call(2, { m: 'revokeDevice', deviceId: toHex(OTHER_DEVICE), recoveryKey: RECOVERY_KEY });
    await vi.waitFor(() => expect(w.ret(2)).toEqual({ t: 'ret', id: 2, ok: true, value: null }));
    expect(w.calls.slice(from)).toEqual([
      'refreshOwnDeviceList', 'getBackup(0)', 'getBackup(1)', 'getDeviceList', 'core.deviceListRevoke',
      'putDeviceList', 'putDeviceList', 'core.deviceListPublished', 'putBackup(1)', 'core.stateSealedUploaded', 'refreshOwnDeviceList', 'listDevices',
    ]);
    expect(w.routes.putDeviceList.mock.calls.map((c) => c[1])).toEqual([interrupted, new Uint8Array([7])]);
  });

  it('an interrupted publication that another device published meanwhile (409) is not an error', async () => {
    const w = world();
    await toReady(w);
    w.state.ownList = LISTED_BOTH;
    w.core.deviceListRevoke.mockImplementationOnce(() => {
      w.calls.push('core.deviceListRevoke'); return { deviceListBody: new Uint8Array([7]), stateSealed: new Uint8Array([8]), interrupted: new Uint8Array([6]) };
    });
    w.routes.putDeviceList.mockImplementationOnce(() => { w.calls.push('putDeviceList'); return Promise.reject(refusal(409, 'E_INVALID_REQUEST')); });
    w.call(2, { m: 'revokeDevice', deviceId: toHex(OTHER_DEVICE), recoveryKey: RECOVERY_KEY });
    await vi.waitFor(() => expect(w.ret(2)).toEqual({ t: 'ret', id: 2, ok: true, value: null }));
    expect(w.routes.putDeviceList).toHaveBeenCalledTimes(2);
    expect(w.core.deviceListPublished).toHaveBeenCalledTimes(1);
  });

  it('an interrupted publication refused otherwise fails the revocation and drops the candidate to it', async () => {
    const w = world();
    await toReady(w);
    w.state.ownList = LISTED_BOTH;
    w.core.deviceListRevoke.mockImplementationOnce(() => {
      w.calls.push('core.deviceListRevoke'); return { deviceListBody: new Uint8Array([7]), stateSealed: new Uint8Array([8]), interrupted: new Uint8Array([6]) };
    });
    w.routes.putDeviceList.mockImplementationOnce(() => { w.calls.push('putDeviceList'); return Promise.reject(refusal(0, 'E_NETWORK')); });
    w.call(2, { m: 'revokeDevice', deviceId: toHex(OTHER_DEVICE), recoveryKey: RECOVERY_KEY });
    await vi.waitFor(() => expect(w.ret(2)).toBeDefined());
    expect(w.ret(2)).toMatchObject({ ok: false, error: { code: 'E_NETWORK' } });
    expect(w.routes.putDeviceList).toHaveBeenCalledTimes(1);
    expect(w.core.deviceListDrop).toHaveBeenCalledTimes(1);
    expect(w.routes.putBackup).not.toHaveBeenCalled();
  });

  it('a sign-out over an interrupted publication publishes it first, then the state object, then the self-revoking list', async () => {
    const w = world();
    await toReady(w);
    w.core.deviceListRevoke.mockImplementationOnce(() => {
      w.calls.push('core.deviceListRevoke'); return { deviceListBody: new Uint8Array([7]), stateSealed: new Uint8Array([8]), interrupted: new Uint8Array([6]) };
    });
    const from = w.calls.length;
    w.call(2, { m: 'signOutRevoke', recoveryKey: RECOVERY_KEY });
    await vi.waitFor(() => expect(w.ret(2)).toEqual({ t: 'ret', id: 2, ok: true, value: null }));
    expect(w.calls.slice(from)).toEqual([
      'getBackup(0)', 'getBackup(1)', 'getDeviceList', 'core.deviceListRevoke', 'putDeviceList', 'putBackup(1)',
      'sync.stop', 'gateway.stop', 'putDeviceList', 'core.pause', 'resetDevice',
    ]);
  });

  it('a revocation that loses the race reports E_LIST_RACE, restarts the gateway and keeps the store', async () => {
    const w = world();
    await toReady(w);
    w.routes.putDeviceList.mockImplementationOnce(() => Promise.reject(refusal(409, 'E_INVALID_REQUEST')));
    const refreshes = w.account.refreshOwnDeviceList.mock.calls.length;
    w.call(2, { m: 'signOutRevoke', recoveryKey: RECOVERY_KEY });
    await vi.waitFor(() => expect(w.ret(2)).toBeDefined());
    expect(w.ret(2)).toEqual({ t: 'ret', id: 2, ok: false, error: { code: 'E_LIST_RACE', detail: '', status: 0, retryAfterMs: null } });
    expect(w.account.refreshOwnDeviceList).toHaveBeenCalledTimes(refreshes + 1);
    expect(w.gateway.starts).toBe(2);
    expect(w.core.pause).not.toHaveBeenCalled();
    expect(w.account()?.phase).toBe('ready');
  });

  // Pre-flight ruling (b): revokeDevice refetches the own list before it decides listed or unlisted, so the
  // brief's two expected call lists below begin with that one refreshOwnDeviceList (pre-flight row 1.8).
  it('revokeDevice revokes another listed device with the key and refreshes the list without a wipe', async () => {
    const w = world();
    await toReady(w);
    w.state.ownList = LISTED_BOTH;
    w.call(2, { m: 'revokeDevice', deviceId: toHex(DEVICE), recoveryKey: RECOVERY_KEY });
    await vi.waitFor(() => expect(w.ret(2)).toBeDefined());
    expect(w.ret(2)).toMatchObject({ ok: false, error: { code: 'E_BAD_INPUT', detail: '' } });
    expect(w.routes.getBackup).not.toHaveBeenCalled();
    const from = w.calls.length;
    w.call(3, { m: 'revokeDevice', deviceId: toHex(OTHER_DEVICE), recoveryKey: RECOVERY_KEY });
    await vi.waitFor(() => expect(w.ret(3)).toEqual({ t: 'ret', id: 3, ok: true, value: null }));
    // BACKUPS-RECOVERY-01: the revoking list goes first, so the shared upload meter or quota cannot hold it back.
    expect(w.calls.slice(from)).toEqual([
      'refreshOwnDeviceList',
      'getBackup(0)', 'getBackup(1)', 'getDeviceList', 'core.deviceListRevoke',
      'putDeviceList', 'core.deviceListPublished', 'putBackup(1)', 'core.stateSealedUploaded', 'refreshOwnDeviceList', 'listDevices',
    ]);
    expect(w.core.deviceListRevoke).toHaveBeenCalledWith(expect.objectContaining({ deviceIds: [OTHER_DEVICE] }));
    expect(w.routes.deleteDevice).not.toHaveBeenCalled();
    expect(w.core.pause).not.toHaveBeenCalled();
    expect(w.gateway.stops).toBe(0);
    expect(w.account()?.phase).toBe('ready');
  });

  it('a state object refused after the revoking list (a 429 on the shared meter, a 507 at the quota) still revokes; the next ready uploads it', async () => {
    const w = world();
    await toReady(w);
    w.state.ownList = LISTED_BOTH;
    for (const refused of [refusal(429, 'E_RATE_LIMITED', 3_000), refusal(507, 'E_STORAGE_FULL')]) {
      w.routes.putBackup.mockImplementationOnce(() => { w.calls.push('putBackup(1)'); return Promise.reject(refused); });
      w.core.stateSealedUploaded.mockClear();
      const id = w.posted.length + 10;
      const from = w.calls.length;
      w.call(id, { m: 'revokeDevice', deviceId: toHex(OTHER_DEVICE), recoveryKey: RECOVERY_KEY });
      await vi.waitFor(() => expect(w.ret(id)).toEqual({ t: 'ret', id, ok: true, value: null }));
      expect(w.calls.slice(from)).toEqual([
        'refreshOwnDeviceList', 'getBackup(0)', 'getBackup(1)', 'getDeviceList', 'core.deviceListRevoke',
        'putDeviceList', 'core.deviceListPublished', 'putBackup(1)', 'refreshOwnDeviceList', 'listDevices',
      ]);
      // Left unmarked: ensureBackups at the next ready uploads it (the floor accepts a state behind the list).
      expect(w.core.stateSealedUploaded).not.toHaveBeenCalled();
    }
    expect(w.account()?.phase).toBe('ready');
  });

  it('a revoking list refused before the state object: nothing is uploaded', async () => {
    const w = world();
    await toReady(w);
    w.state.ownList = LISTED_BOTH;
    w.routes.putDeviceList.mockImplementationOnce(() => { w.calls.push('putDeviceList'); return Promise.reject(refusal(0, 'E_NETWORK')); });
    w.call(2, { m: 'revokeDevice', deviceId: toHex(OTHER_DEVICE), recoveryKey: RECOVERY_KEY });
    await vi.waitFor(() => expect(w.ret(2)).toBeDefined());
    expect(w.ret(2)).toMatchObject({ ok: false, error: { code: 'E_NETWORK' } });
    expect(w.routes.putBackup).not.toHaveBeenCalled();
    expect(w.core.deviceListPublished).not.toHaveBeenCalled();
  });

  it('a listed device needs the key', async () => {
    const w = world();
    await toReady(w);
    w.state.ownList = LISTED_BOTH;
    w.call(2, { m: 'revokeDevice', deviceId: toHex(OTHER_DEVICE), recoveryKey: null });
    w.call(3, { m: 'revokeDevice', deviceId: toHex(OTHER_DEVICE), recoveryKey: '   ' });
    w.call(4, { m: 'revokeDevice', deviceId: toHex(OTHER_DEVICE), recoveryKey: 7 });
    await vi.waitFor(() => expect(w.ret(4)).toBeDefined());
    await vi.waitFor(() => expect(w.ret(3)).toBeDefined());
    await vi.waitFor(() => expect(w.ret(2)).toBeDefined());
    expect(w.ret(2)).toMatchObject({ ok: false, error: { code: 'E_BAD_INPUT', detail: '' } });
    expect(w.ret(3)).toMatchObject({ ok: false, error: { code: 'E_BAD_INPUT', detail: '' } });
    expect(w.ret(4)).toMatchObject({ ok: false, error: { code: 'E_BAD_INPUT', detail: '' } });
    expect(w.routes.getBackup).not.toHaveBeenCalled();
    expect(w.routes.deleteDevice).not.toHaveBeenCalled();
  });

  it('an unlisted row is removed with DELETE and no core method', async () => {
    const w = world();
    await toReady(w);
    const from = w.calls.length;
    w.call(2, { m: 'revokeDevice', deviceId: toHex(OTHER_DEVICE), recoveryKey: null });
    await vi.waitFor(() => expect(w.ret(2)).toEqual({ t: 'ret', id: 2, ok: true, value: null }));
    expect(w.routes.deleteDevice).toHaveBeenCalledWith(OTHER_DEVICE);
    expect(w.calls.slice(from)).toEqual(['refreshOwnDeviceList', 'deleteDevice', 'refreshOwnDeviceList', 'listDevices']);
    expect(w.core.deviceListRevoke).not.toHaveBeenCalled();
    expect(w.routes.putBackup).not.toHaveBeenCalled();
    expect(w.routes.putDeviceList).not.toHaveBeenCalled();
    w.routes.deleteDevice.mockImplementationOnce(() => Promise.reject(refusal(404, 'E_NOT_FOUND')));
    w.call(3, { m: 'revokeDevice', deviceId: toHex(OTHER_DEVICE), recoveryKey: RECOVERY_KEY });
    await vi.waitFor(() => expect(w.ret(3)).toBeDefined());
    expect(w.ret(3)).toEqual({ t: 'ret', id: 3, ok: false, error: { code: 'E_NOT_FOUND', detail: '', status: 404, retryAfterMs: null } });
    expect(w.core.deviceListRevoke).not.toHaveBeenCalled();
  });

  it('forgetBrowser stops the gateway first, ignores a 401 on the sessions delete and wipes to cleared', async () => {
    const w = world();
    await toReady(w);
    w.routes.deleteSessions.mockImplementationOnce(() => { w.calls.push('deleteSessions'); return Promise.reject(refusal(401, 'E_UNAUTHENTICATED')); });
    const from = w.calls.length;
    w.call(2, { m: 'forgetBrowser' });
    await vi.waitFor(() => expect(w.ret(2)).toEqual({ t: 'ret', id: 2, ok: true, value: null }));
    expect(w.calls.slice(from)).toEqual(['sync.stop', 'gateway.stop', 'deleteSessions', 'core.pause', 'resetDevice']);
    expect(w.routes.deleteSessions).toHaveBeenCalledWith(DEVICE);
    expect(w.wiped).toEqual([INSTANCE_HEX]);
    expect(w.account()).toMatchObject({ phase: 'cleared', user: null, deviceId: null, error: null });
    expect(w.routes.putDeviceList).not.toHaveBeenCalled();
  });

  it('forgetBrowser publishes an unpublished candidate before it deletes the sessions, and wipes even if that fails', async () => {
    for (const fails of [false, true]) {
      const w = world();
      await toReady(w);
      w.state.listPublished = false;
      if (fails) w.account.publishDeviceList.mockImplementationOnce(() => { w.calls.push('publishDeviceList'); return Promise.reject(refusal(0, 'E_NETWORK')); });
      const from = w.calls.length;
      w.call(2, { m: 'forgetBrowser' });
      await vi.waitFor(() => expect(w.ret(2)).toEqual({ t: 'ret', id: 2, ok: true, value: null }));
      expect(w.calls.slice(from)).toEqual(['sync.stop', 'gateway.stop', 'publishDeviceList', 'deleteSessions', 'core.pause', 'resetDevice']);
      expect(w.account()?.phase).toBe('cleared');
    }
  });

  it('a 4004 during forgetBrowser mints nothing', async () => {
    const w = world();
    await toReady(w);
    let release!: () => void;
    w.routes.deleteSessions.mockImplementationOnce(() => {
      w.calls.push('deleteSessions');
      return new Promise<void>((resolve) => { release = resolve; });
    });
    w.call(2, { m: 'forgetBrowser' });
    await vi.waitFor(() => expect(w.routes.deleteSessions).toHaveBeenCalledTimes(1));
    w.gateway.emit({ type: 'revoked' });     // the server's 4004 while the DELETE is in flight
    release();
    await vi.waitFor(() => expect(w.ret(2)).toEqual({ t: 'ret', id: 2, ok: true, value: null }));
    expect(w.session.establish).not.toHaveBeenCalled();
    expect(w.gateway.starts).toBe(1);
    expect(phasesOf(w)).not.toContain('revoked');
    expect(w.account()?.phase).toBe('cleared');
  });

  it('forgetBrowser wipes nothing when the delete fails otherwise, and restores the gateway', async () => {
    const w = world();
    await toReady(w);
    w.routes.deleteSessions.mockImplementationOnce(() => Promise.reject(refusal(503, 'E_UNAVAILABLE', 1000)));
    w.call(2, { m: 'forgetBrowser' });
    await vi.waitFor(() => expect(w.ret(2)).toBeDefined());
    expect(w.ret(2)).toEqual({ t: 'ret', id: 2, ok: false, error: { code: 'E_UNAVAILABLE', detail: '', status: 503, retryAfterMs: 1000 } });
    expect(w.core.pause).not.toHaveBeenCalled();
    expect(w.gateway.starts).toBe(2);
    expect(w.sync.builds).toBe(2);
    expect(w.account()?.phase).toBe('ready');
  });
});

// ---- The controller's pre-flight rulings for task 14, one test each (they add to the brief's tests). ----

describe('Controller pre-flight rulings (task 14)', () => {
  const OLDER_LIST = (): CoreError => new CoreError('E_CORE_INPUT', 'the instance served an older device list');

  it('(a) a row the server received before this session was ready badges but raises no notice', async () => {
    const w = world();
    await toReady(w);
    w.state.groups = [textGroup(2)];
    w.state.rows = [{ ...strangerRow(1), recvTs: NOW_S - 1n }];
    w.state.activity = [act(GROUP, 1, 0)];
    w.sync.deps!.onGroupChanged(GROUP, appliedSeqs([1n]));
    expect(w.badges()).toEqual({ [toHex(CHANNEL)]: { unread: 1, mentions: 0 } });
    expect(w.notices()).toEqual({ nextId: 1, items: [] });
  });

  it('(a) after a reconnect, rows caught up from before it raise no notice; a live row after it does', async () => {
    let nowMs = 1_700_000_000_000;
    const w = world({ now: () => nowMs });
    await toReady(w);
    nowMs += 60_000;                                          // the gateway was down for a minute
    w.gateway.emit({ type: 'ready', info: READY_INFO });
    w.state.groups = [textGroup(2)];
    w.state.rows = [{ ...strangerRow(1), recvTs: NOW_S + 30n }];
    w.state.activity = [act(GROUP, 1, 0)];
    w.sync.deps!.onGroupChanged(GROUP, appliedSeqs([1n]));     // the catch-up after the reconnect
    expect(w.notices()).toEqual({ nextId: 1, items: [] });
    expect(w.badges()).toEqual({ [toHex(CHANNEL)]: { unread: 1, mentions: 0 } });
    w.state.rows = [{ ...strangerRow(1), recvTs: NOW_S + 30n }, { ...strangerRow(2), recvTs: NOW_S + 61n }];
    w.state.activity = [act(GROUP, 2, 0)];
    w.sync.deps!.onGroupChanged(GROUP, appliedSeqs([2n]));     // a live frame
    expect(w.notices()?.items.map((n) => n.body)).toEqual(['from a stranger 2']);
  });

  it('(a) the floor is read on the instance clock (the ticket minted for the connection), not on this browser\'s', async () => {
    const w = world();
    await toReady(w);
    // The instance clock is ten minutes behind this browser: the ticket expires 30 s after the instance's now.
    w.routes.postTicket.mockImplementationOnce(() => Promise.resolve({ ticket: 'ticket', expires: NOW_S - 600n + 30n }));
    expect(await w.gatewayDeps.value!.mintTicket()).toBe('ticket');
    w.gateway.emit({ type: 'ready', info: READY_INFO });
    w.state.groups = [textGroup(2)];
    w.state.rows = [{ ...strangerRow(1), recvTs: NOW_S - 700n }, { ...strangerRow(2), recvTs: NOW_S - 500n }];
    w.state.activity = [act(GROUP, 2, 0)];
    w.sync.deps!.onGroupChanged(GROUP, appliedSeqs([1n, 2n]));
    expect(w.notices()?.items.map((n) => n.body)).toEqual(['from a stranger 2']);
  });

  it('(b) revokeDevice decides listed from the refreshed own list, never from a stale one', async () => {
    const w = world();
    await toReady(w);
    // The stored list names only this device; the instance's newer list (adopted by the refresh) names the other too.
    w.account.refreshOwnDeviceList.mockImplementationOnce(() => {
      w.calls.push('refreshOwnDeviceList');
      w.state.ownList = { version: 2n, published: true, entries: [
        { deviceId: DEVICE, dskPub: new Uint8Array(32), tier: 1, addedAt: 1n, revokedAt: null },
        { deviceId: OTHER_DEVICE, dskPub: new Uint8Array(32), tier: 1, addedAt: 2n, revokedAt: null },
      ] };
      return Promise.resolve({ version: 2n, listed: true });
    });
    w.call(2, { m: 'revokeDevice', deviceId: toHex(OTHER_DEVICE), recoveryKey: null });
    await vi.waitFor(() => expect(w.ret(2)).toBeDefined());
    expect(w.ret(2)).toMatchObject({ ok: false, error: { code: 'E_BAD_INPUT', detail: '' } });
    expect(w.routes.deleteDevice).not.toHaveBeenCalled();
  });

  it('(c) a revocation refused for an older served list refreshes the own list, reads the objects again and retries once', async () => {
    const w = world();
    await toReady(w);
    w.core.deviceListRevoke.mockImplementationOnce(() => { w.calls.push('core.deviceListRevoke'); throw OLDER_LIST(); });
    const from = w.calls.length;
    w.call(2, { m: 'signOutRevoke', recoveryKey: RECOVERY_KEY });
    await vi.waitFor(() => expect(w.ret(2)).toEqual({ t: 'ret', id: 2, ok: true, value: null }));
    expect(w.calls.slice(from)).toEqual([
      'getBackup(0)', 'getBackup(1)', 'getDeviceList', 'core.deviceListRevoke', 'refreshOwnDeviceList',
      'getBackup(0)', 'getBackup(1)', 'getDeviceList', 'core.deviceListRevoke', 'putBackup(1)',
      'sync.stop', 'gateway.stop', 'putDeviceList', 'core.pause', 'resetDevice',
    ]);
  });

  it('(c) a second older-list refusal is the list conflict, with nothing uploaded or stopped', async () => {
    const w = world();
    await toReady(w);
    w.state.ownList = { version: 2n, published: true, entries: [
      { deviceId: DEVICE, dskPub: new Uint8Array(32), tier: 1, addedAt: 1n, revokedAt: null },
      { deviceId: OTHER_DEVICE, dskPub: new Uint8Array(32), tier: 1, addedAt: 2n, revokedAt: null },
    ] };
    w.core.deviceListRevoke.mockImplementation(() => { throw OLDER_LIST(); });
    w.call(2, { m: 'revokeDevice', deviceId: toHex(OTHER_DEVICE), recoveryKey: RECOVERY_KEY });
    await vi.waitFor(() => expect(w.ret(2)).toBeDefined());
    expect(w.ret(2)).toEqual({ t: 'ret', id: 2, ok: false, error: { code: 'E_LIST_RACE', detail: '', status: 0, retryAfterMs: null } });
    expect(w.core.deviceListRevoke).toHaveBeenCalledTimes(2);
    expect(w.routes.putBackup).not.toHaveBeenCalled();
    expect(w.routes.putDeviceList).not.toHaveBeenCalled();
    expect(w.gateway.stops).toBe(0);
    expect(w.account()?.phase).toBe('ready');
  });

  it('(c) an enrolment refused for an older served list reads the backups again and completes', async () => {
    const w = world({ phase: 0 });
    w.enrol.complete.mockImplementationOnce(() => { w.calls.push('enrol.complete'); return Promise.reject(OLDER_LIST()); });
    await toKeyStep(w);
    w.call(4, KEY);
    await vi.waitFor(() => expect(w.ret(4)).toEqual({ t: 'ret', id: 4, ok: true, value: null }));
    expect(w.calls.filter((c) => c.startsWith('enrol.'))).toEqual([
      'enrol.login', 'enrol.register', 'enrol.fetch', 'enrol.complete', 'enrol.fetch', 'enrol.complete',
    ]);
    expect(w.account()?.phase).toBe('ready');
  });

  it('(c) a second older-list refusal at enrolment is the list race: the store is wiped to cleared', async () => {
    const w = world({ phase: 0 });
    w.enrol.complete.mockImplementation(() => Promise.reject(OLDER_LIST()));
    await toKeyStep(w);
    w.call(4, KEY);
    await vi.waitFor(() => expect(w.ret(4)).toBeDefined());
    const want = { code: 'E_LIST_RACE', detail: '', status: 0, retryAfterMs: null };
    expect(w.ret(4)).toEqual({ t: 'ret', id: 4, ok: false, error: want });
    expect(w.enrol.complete).toHaveBeenCalledTimes(2);
    expect(w.account()).toMatchObject({ phase: 'cleared', error: want });
    expect(w.wiped).toEqual([INSTANCE_HEX]);
  });

  it('(e) an enrolment whose backup state is missing or unreadable is E_NO_BACKUP at the key step, without a retry', async () => {
    for (const detail of ['the backup state is missing', 'the backup state could not be read']) {
      const w = world({ phase: 0 });
      w.enrol.complete.mockImplementationOnce(() => Promise.reject(new CoreError('E_CORE_INPUT', detail)));
      await toKeyStep(w);
      w.call(4, KEY);
      await vi.waitFor(() => expect(w.ret(4)).toBeDefined());
      const want = { code: 'E_NO_BACKUP', detail: '', status: 0, retryAfterMs: null };
      expect(w.ret(4)).toEqual({ t: 'ret', id: 4, ok: false, error: want });
      expect(w.account()).toMatchObject({ phase: 'signin-key', error: want });
      expect(w.enrol.complete).toHaveBeenCalledTimes(1);
      expect(w.state.phase).toBe(3);
      // The next key reads the backups again: another device may have uploaded the state meanwhile.
      w.call(5, { m: 'signInKey', recoveryKey: RECOVERY_KEY });
      await vi.waitFor(() => expect(w.ret(5)).toEqual({ t: 'ret', id: 5, ok: true, value: null }));
      expect(w.enrol.fetch).toHaveBeenCalledTimes(2);
    }
  });

  it('(d) a revoked browser can forget itself: sessions deleted, store wiped, cleared', async () => {
    const w = world();
    w.account.refreshOwnDeviceList.mockImplementationOnce(() => Promise.resolve({ version: 3n, listed: false }));
    w.call(1, { m: 'start' });
    await vi.waitFor(() => expect(w.account()?.phase).toBe('revoked'));
    const from = w.calls.length;
    w.call(2, { m: 'forgetBrowser' });
    await vi.waitFor(() => expect(w.ret(2)).toEqual({ t: 'ret', id: 2, ok: true, value: null }));
    expect(w.calls.slice(from)).toEqual(['deleteSessions', 'core.pause', 'resetDevice']);
    expect(w.routes.deleteSessions).toHaveBeenCalledWith(DEVICE);
    expect(w.account()).toMatchObject({ phase: 'cleared', user: null, deviceId: null, error: null });
    expect(w.sync.builds).toBe(0);
    expect(w.gateway.starts).toBe(0);
  });

  it('(d) a revoked browser whose sessions delete fails stays revoked and starts nothing', async () => {
    const w = world();
    w.account.refreshOwnDeviceList.mockImplementationOnce(() => Promise.resolve({ version: 3n, listed: false }));
    w.call(1, { m: 'start' });
    await vi.waitFor(() => expect(w.account()?.phase).toBe('revoked'));
    w.routes.deleteSessions.mockImplementationOnce(() => Promise.reject(refusal(503, 'E_UNAVAILABLE', 1000)));
    w.call(2, { m: 'forgetBrowser' });
    await vi.waitFor(() => expect(w.ret(2)).toBeDefined());
    expect(w.ret(2)).toMatchObject({ ok: false, error: { code: 'E_UNAVAILABLE', status: 503 } });
    expect(w.core.pause).not.toHaveBeenCalled();
    expect(w.account()?.phase).toBe('revoked');
    expect(w.sync.builds).toBe(0);
    expect(w.gateway.starts).toBe(0);
  });

  it('pre-flight row 2.14(d): a failed first DM load still publishes an empty DM list', async () => {
    const w = world();
    w.routes.listDms.mockImplementationOnce(() => Promise.reject(refusal(503, 'E_UNAVAILABLE', 1000)));
    await toReady(w);
    expect(w.last<DmSummary[]>('dms')).toEqual([]);
  });
});

// ---- Task-14 fix round 1: a root mismatch is not dropped; an unusable served state object cannot block a revocation. ----

describe('Controller task-14 fix round 1', () => {
  /** BACKUPS-RECOVERY-04: a root mismatch reaches the page on the account slice. */
  const rootMismatch = (w: World): unknown => w.account()?.rootMismatch;
  const STATE_MISSING = 'the backup state is missing';
  const STATE_UNREADABLE = 'the backup state could not be read';
  const listedBoth = (w: World): void => {
    w.state.ownList = { version: 2n, published: true, entries: [
      { deviceId: DEVICE, dskPub: new Uint8Array(32), tier: 1, addedAt: 1n, revokedAt: null },
      { deviceId: OTHER_DEVICE, dskPub: new Uint8Array(32), tier: 1, addedAt: 2n, revokedAt: null },
    ] };
  };

  it('a root mismatch from ensureBackups at ready is recorded, the client still reaches ready, and the wipe drops it', async () => {
    const w = world();
    w.account.ensureBackups.mockImplementationOnce(() => { w.calls.push('ensureBackups'); return Promise.reject(new Error('E_ROOT_MISMATCH')); });
    await toReady(w);
    expect(rootMismatch(w)).toBe(true);
    w.call(2, { m: 'forgetBrowser' });
    await vi.waitFor(() => expect(w.ret(2)).toEqual({ t: 'ret', id: 2, ok: true, value: null }));
    expect(rootMismatch(w)).toBe(false);
  });

  it('every other ensureBackups rejection is still swallowed and records nothing; a gateway ready records a mismatch; a later success clears it', async () => {
    const w = world();
    w.account.ensureBackups.mockImplementationOnce(() => Promise.reject(refusal(503, 'E_UNAVAILABLE', 1000)));
    await toReady(w);
    expect(rootMismatch(w)).toBe(false);
    w.account.ensureBackups.mockImplementationOnce(() => Promise.reject(new Error('E_ROOT_MISMATCH')));
    w.gateway.emit({ type: 'ready', info: READY_INFO });
    await vi.waitFor(() => expect(rootMismatch(w)).toBe(true));
    w.account.ensureBackups.mockImplementationOnce(() => Promise.reject(refusal(503, 'E_UNAVAILABLE', 1000)));
    w.gateway.emit({ type: 'ready', info: READY_INFO });
    await settle();
    expect(rootMismatch(w)).toBe(true);
    w.gateway.emit({ type: 'ready', info: READY_INFO });         // the operator removed the planted root: the upload succeeds
    await vi.waitFor(() => expect(rootMismatch(w)).toBe(false));
  });

  it('a served state object the core refuses is replaced by this device\'s own sealed state, and the re-sealed one repairs the instance\'s copy', async () => {
    for (const detail of [STATE_MISSING, STATE_UNREADABLE]) {
      const w = world();
      await toReady(w);
      listedBoth(w);
      w.core.deviceListRevoke.mockImplementationOnce(() => { w.calls.push('core.deviceListRevoke'); throw new CoreError('E_CORE_INPUT', detail); });
      const from = w.calls.length;
      w.call(2, { m: 'revokeDevice', deviceId: toHex(OTHER_DEVICE), recoveryKey: RECOVERY_KEY });
      await vi.waitFor(() => expect(w.ret(2)).toEqual({ t: 'ret', id: 2, ok: true, value: null }));
      expect(w.calls.slice(from)).toEqual([
        'refreshOwnDeviceList', 'getBackup(0)', 'getBackup(1)', 'getDeviceList', 'core.deviceListRevoke', 'core.deviceListRevoke',
        // BACKUPS-RECOVERY-01: the list first, then the re-sealed state object.
        'putDeviceList', 'core.deviceListPublished', 'putBackup(1)', 'core.stateSealedUploaded', 'refreshOwnDeviceList', 'listDevices',
      ]);
      expect(w.core.deviceListRevoke).toHaveBeenNthCalledWith(1, expect.objectContaining({ stateSealed: STATE_OBJECT, deviceIds: [OTHER_DEVICE] }));
      expect(w.core.deviceListRevoke).toHaveBeenNthCalledWith(2, {
        recoveryKey: RECOVERY_KEY, rootSealed: ROOT, stateSealed: LOCAL_STATE, listBody: LIST_RAW, deviceIds: [OTHER_DEVICE], now: NOW_S,
      });
      expect(w.routes.putBackup).toHaveBeenCalledWith(1, new Uint8Array([8]));
    }
  });

  it('a sign-out over a refused served state object signs with this device\'s own state and wipes to cleared', async () => {
    const w = world();
    await toReady(w);
    w.core.deviceListRevoke.mockImplementationOnce(() => { w.calls.push('core.deviceListRevoke'); throw new CoreError('E_CORE_INPUT', STATE_MISSING); });
    const from = w.calls.length;
    w.call(2, { m: 'signOutRevoke', recoveryKey: RECOVERY_KEY });
    await vi.waitFor(() => expect(w.ret(2)).toEqual({ t: 'ret', id: 2, ok: true, value: null }));
    expect(w.calls.slice(from)).toEqual([
      'getBackup(0)', 'getBackup(1)', 'getDeviceList', 'core.deviceListRevoke', 'core.deviceListRevoke', 'putBackup(1)',
      'sync.stop', 'gateway.stop', 'putDeviceList', 'core.pause', 'resetDevice',
    ]);
    expect(w.core.deviceListRevoke).toHaveBeenLastCalledWith(expect.objectContaining({ stateSealed: LOCAL_STATE, deviceIds: [DEVICE] }));
    expect(w.account()?.phase).toBe('cleared');
  });

  it('when this device\'s own state is refused too, or there is none, the revocation is E_NO_BACKUP with nothing uploaded or stopped', async () => {
    const want = { code: 'E_NO_BACKUP', detail: '', status: 0, retryAfterMs: null };
    // (i) the own state is refused as well: two core calls, then E_NO_BACKUP.
    const w = world();
    await toReady(w);
    listedBoth(w);
    w.core.deviceListRevoke.mockImplementation(() => { throw new CoreError('E_CORE_INPUT', STATE_UNREADABLE); });
    w.call(2, { m: 'revokeDevice', deviceId: toHex(OTHER_DEVICE), recoveryKey: RECOVERY_KEY });
    await vi.waitFor(() => expect(w.ret(2)).toBeDefined());
    expect(w.ret(2)).toEqual({ t: 'ret', id: 2, ok: false, error: want });
    expect(w.core.deviceListRevoke).toHaveBeenCalledTimes(2);
    expect(w.routes.putBackup).not.toHaveBeenCalled();
    expect(w.routes.putDeviceList).not.toHaveBeenCalled();
    expect(w.account()?.phase).toBe('ready');
    // (ii) this device holds no state object: one core call, then E_NO_BACKUP; a sign-out stops nothing.
    const v = world();
    await toReady(v);
    v.core.sealedObjects.mockImplementation(() => ({ root: ROOT, state: null, stateUploaded: false }));
    v.core.deviceListRevoke.mockImplementation(() => { throw new CoreError('E_CORE_INPUT', STATE_MISSING); });
    v.call(2, { m: 'signOutRevoke', recoveryKey: RECOVERY_KEY });
    await vi.waitFor(() => expect(v.ret(2)).toBeDefined());
    expect(v.ret(2)).toEqual({ t: 'ret', id: 2, ok: false, error: want });
    expect(v.core.deviceListRevoke).toHaveBeenCalledTimes(1);
    expect(v.routes.putBackup).not.toHaveBeenCalled();
    expect(v.gateway.stops).toBe(0);
    expect(v.account()?.phase).toBe('ready');
  });

  it('any other core refusal of the revocation is not retried with the own state', async () => {
    const w = world();
    await toReady(w);
    w.core.deviceListRevoke.mockImplementationOnce(() => { throw new CoreError('E_RECOVERY_KEY', ''); });
    w.call(2, { m: 'signOutRevoke', recoveryKey: RECOVERY_KEY });
    await vi.waitFor(() => expect(w.ret(2)).toBeDefined());
    expect(w.ret(2)).toEqual({ t: 'ret', id: 2, ok: false, error: { code: 'E_RECOVERY_KEY', detail: '', status: 0, retryAfterMs: null } });
    expect(w.core.deviceListRevoke).toHaveBeenCalledTimes(1);
    expect(w.core.sealedObjects).not.toHaveBeenCalled();
  });
});

// ---- WORKER-WEB-01: an enrolment interrupted after the list publish must not keep the pending session. ----

describe('Controller enrolment interrupted after the list publish (WORKER-WEB-01)', () => {
  const PENDING: SessionRecord = { token: 'pending', expires: NOW_S + 604_800n, idleExpires: NOW_S + 43_200n };
  const order = (w: World): string[] =>
    w.calls.filter((c) => ['putDeviceList', 'core.sessionClear', 'postSession', 'gateway.start'].includes(c));

  it('an establish that fails after the publish: the same tab establishes an enrolled session before the gateway starts', async () => {
    const w = world({ phase: 3, enrolUser: true, realAccount: true });
    w.state.session = PENDING;
    w.routes.postSession.mockImplementationOnce(() => { w.calls.push('postSession'); return Promise.reject(refusal(0, 'E_NETWORK')); });
    w.call(1, { m: 'start' });
    await vi.waitFor(() => expect(w.account()?.phase).toBe('signin-key'));
    w.call(2, { m: 'signInKey', recoveryKey: RECOVERY_KEY });
    await vi.waitFor(() => expect(w.ret(2)).toEqual({ t: 'ret', id: 2, ok: true, value: null }));
    expect(w.account()?.phase).toBe('ready');
    expect(order(w)).toEqual(['putDeviceList', 'core.sessionClear', 'postSession', 'postSession', 'gateway.start']);
    expect(w.state.session?.token).toBe('enrolled');
  });

  it('a reload before the establish answered: the next boot establishes an enrolled session before the gateway starts', async () => {
    const w = world({ phase: 3, enrolUser: true, realAccount: true });
    w.state.session = PENDING;
    // The tab goes away while the establish after the publish is in flight: it never answers.
    w.routes.postSession.mockImplementationOnce(() => { w.calls.push('postSession'); return new Promise(() => {}); });
    w.call(1, { m: 'start' });
    await vi.waitFor(() => expect(w.account()?.phase).toBe('signin-key'));
    w.call(2, { m: 'signInKey', recoveryKey: RECOVERY_KEY });
    await vi.waitFor(() => expect(w.calls).toContain('postSession'));
    expect(w.state).toMatchObject({ phase: 2, listPublished: true });
    w.calls.length = 0;
    // The reload: a new worker over the same store.
    const again = harness({
      boot: (instance) => Promise.resolve({ kind: 'opened', core: w.core as unknown as CorePort, instance }),
      parts: w.parts,
    });
    again.call(1, { m: 'start' });
    await vi.waitFor(() => expect(again.account()?.phase).toBe('ready'));
    expect(order(w)).toEqual(['postSession', 'gateway.start']);
    expect(w.state.session?.token).toBe('enrolled');
  });
});

// ---- Pre-flight ruling 4: other-tab only after a grace, so an in-place reload does not flash it. ----

describe('Controller other-tab grace', () => {
  // The ruled grace is 1500 ms (LEADER.otherTabGraceMs); the times below are written out so a changed grace fails here.
  afterEach(() => { vi.useRealTimers(); });

  const phases = (w: World): string[] =>
    w.posted.filter((m): m is SliceMessage => m.t === 'slice' && m.name === 'account').map((m) => (m.value as AccountState).phase);

  it('a lock granted within the grace never publishes other-tab', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    const w = world({ phase: 0 });
    const release = w.locks.holdElsewhere(LOCK);
    w.call(1, { m: 'start' });
    await vi.advanceTimersByTimeAsync(0);
    expect(w.locks.requests).toEqual([{ name: LOCK, ifAvailable: true }, { name: LOCK, ifAvailable: false }]);
    await vi.advanceTimersByTimeAsync(1_499);
    expect(w.account()?.phase).toBe('loading');
    release();
    await vi.advanceTimersByTimeAsync(0);
    expect(w.account()?.phase).toBe('needs-signup');
    await vi.advanceTimersByTimeAsync(3_000);
    expect(phases(w)).not.toContain('other-tab');
    expect(w.account()?.phase).toBe('needs-signup');
  });

  it('a lock still held after the grace publishes other-tab', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    const w = world({ phase: 0 });
    w.locks.holdElsewhere(LOCK);
    w.call(1, { m: 'start' });
    await vi.advanceTimersByTimeAsync(1_499);
    expect(w.account()?.phase).toBe('loading');
    await vi.advanceTimersByTimeAsync(1);
    expect(w.account()?.phase).toBe('other-tab');
  });

  // Task 9 (L-WEB-46): the waiting tab's page asks for hand-over by the instance the diagnostics slice names.
  it('names the instance in diagnostics while another tab holds the lock', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    const w = world({ phase: 0 });
    w.locks.holdElsewhere(LOCK);
    w.call(1, { m: 'start' });
    await vi.advanceTimersByTimeAsync(1_500);
    expect(w.account()?.phase).toBe('other-tab');
    expect(w.last<{ instance: string }>('diagnostics')?.instance).toBe(INSTANCE_HEX);
  });

  it('a lock granted after the grace moves on to boot', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    const w = world({ phase: 0 });
    const release = w.locks.holdElsewhere(LOCK);
    w.call(1, { m: 'start' });
    await vi.advanceTimersByTimeAsync(1_500);
    expect(w.account()?.phase).toBe('other-tab');
    release();
    await vi.advanceTimersByTimeAsync(0);
    expect(w.account()?.phase).toBe('needs-signup');
    expect(phases(w).slice(phases(w).indexOf('other-tab'))).toEqual(['other-tab', 'loading', 'needs-signup']);
  });
});

// ---- web-2b: say more (L-TS-34, L-TS-35, L-TS-36) ----

const MSG = new Uint8Array(16).fill(0x31);
const MSG_HEX = toHex(MSG);
const ROLE = new Uint8Array(16).fill(0x7a);
const CH_HEX = toHex(CHANNEL);
/** The 26-byte WebP header stub of attachment.json case 3 (L-CORE-31). */
const WEBP_STUB = Uint8Array.from([0x52, 0x49, 0x46, 0x46, 0x12, 0, 0, 0, 0x57, 0x45, 0x42, 0x50, ...new Array<number>(14).fill(0)]);
type Ok = Extract<FromWorker, { t: 'ret'; ok: true }>;
type Refused = Extract<FromWorker, { t: 'ret'; ok: false }>;

function file(name: string, type: string, size: number): File {
  return new File([new Uint8Array(size).map((_, i) => (i * 7) & 0xff)], name, { type });
}
const trayOf = (w: World): TrayItem[] | undefined => w.last<TrayItem[]>(`tray:${CH_HEX}`);
const pinsOf = (w: World): PinnedItem[] | undefined => w.last<PinnedItem[]>(`pins:${CH_HEX}`);
const okValue = <T>(w: World, id: number): T => (w.ret(id) as Ok).value as T;
const purge = (seq: bigint, blobs: Uint8Array[]): PurgeRow => ({ groupId: GROUP, seq, channelId: CHANNEL, blobIds: blobs });
const B1 = new Uint8Array(32).fill(0xb1);
const B2 = new Uint8Array(32).fill(0xb2);

async function attachReady(w: World, id: number, files: File[]): Promise<string[]> {
  w.call(id, { m: 'attachFiles', channelId: CH_HEX, files });
  await vi.waitFor(() => expect(w.ret(id)).toBeDefined());
  expect(w.ret(id)).toMatchObject({ ok: true });
  await vi.waitFor(() => expect(trayOf(w)?.map((t) => t.phase)).toEqual(files.map(() => 'ready')));
  return okValue<{ trayIds: string[] }>(w, id).trayIds;
}

/** Every Uint8Array or ArrayBuffer anywhere inside a value. */
function bytesIn(value: unknown, found: Uint8Array[] = []): Uint8Array[] {
  if (value instanceof Uint8Array) { found.push(value); return found; }
  if (value instanceof ArrayBuffer) { found.push(new Uint8Array(value)); return found; }
  if (typeof value === 'object' && value !== null) for (const inner of Object.values(value)) bytesIn(inner, found);
  return found;
}

describe('Controller say more (L-TS-34, L-TS-35, L-TS-36)', () => {
  it('refuses malformed say-more commands before anything runs', async () => {
    const h = harness();
    h.call(1, { m: 'editMessage', channelId: CH_HEX, msgId: 'XYZ', text: 'x' });
    h.call(2, { m: 'editMessage', channelId: CH_HEX, msgId: MSG_HEX, text: '  ' });
    h.call(3, { m: 'react', channelId: CH_HEX, msgId: MSG_HEX, emoji: '', on: true });
    h.call(4, { m: 'react', channelId: CH_HEX, msgId: MSG_HEX, emoji: 'x'.repeat(33), on: true });
    h.call(5, { m: 'pin', channelId: CH_HEX, msgId: MSG_HEX, on: 'yes' });
    h.call(6, { m: 'attachFiles', channelId: CH_HEX, files: [] });
    h.call(7, { m: 'attachFiles', channelId: CH_HEX, files: ['not a file'] });
    h.call(8, { m: 'openAttachment', channelId: CH_HEX, seq: '01', index: 0, thumb: false });
    h.call(9, { m: 'openAttachment', channelId: CH_HEX, seq: '7', index: -1, thumb: false });
    h.call(10, { m: 'send', channelId: CH_HEX, text: 'x', replyTo: 'nope' });
    h.call(11, { m: 'send', channelId: CH_HEX, text: 'x', attachments: ['a', 'b', 'c', 'd', 'e'] });
    h.call(12, { m: 'discardAttachment', channelId: CH_HEX, trayId: '' });
    h.call(13, { m: 'react', channelId: CH_HEX, msgId: MSG_HEX, emoji: '👍', on: 1 });
    h.call(14, { m: 'openAttachment', channelId: CH_HEX, seq: '7', index: 0, thumb: 'no' });
    await vi.waitFor(() => expect(h.ret(14)).toBeDefined());
    const details = Array.from({ length: 14 }, (_, i) => (h.ret(i + 1) as Refused).error);
    expect(details.map((e) => e.code)).toEqual(new Array<string>(14).fill('E_BAD_INPUT'));
    expect(details.map((e) => e.detail)).toEqual(new Array<string>(14).fill(''));
    expect(h.paths).toEqual([]);
  });

  it('attachFiles uploads into the channel tray and publishes the tray without descriptors', async () => {
    const w = world();
    await toReady(w);
    await openGeneral(w);
    const ids = await attachReady(w, 4, [file('notes.txt', 'text/plain', 5), file('data.bin', 'application/octet-stream', 70)]);
    expect(ids).toHaveLength(2);
    expect(trayOf(w)).toEqual([
      { id: ids[0], name: 'notes.txt', size: 5, mime: 'text/plain', image: false, phase: 'ready', reason: '' },
      { id: ids[1], name: 'data.bin', size: 70, mime: 'application/octet-stream', image: false, phase: 'ready', reason: '' },
    ]);
    expect(w.routes.putBlob).toHaveBeenCalledTimes(2);
    expect(w.routes.putBlob.mock.calls.map((c) => toHex(c[0]))).toEqual([CH_HEX, CH_HEX]);
    // The stored bytes are the file sealed: 16 bytes longer than the plaintext (L-CORE-31).
    expect(w.routes.putBlob.mock.calls.map((c) => c[2].length)).toEqual([5 + 16, 70 + 16]);
  });

  it('exports selected files for a local update checkpoint without attachment keys', async () => {
    const w = world();
    await toReady(w);
    await openGeneral(w);
    const selected = file('update.txt', 'text/plain', 5);
    await attachReady(w, 4, [selected]);
    w.call(5, { m: 'checkpointFiles', channelId: CH_HEX });
    await vi.waitFor(() => expect(w.ret(5)).toBeDefined());
    expect(okValue<File[]>(w, 5)).toEqual([selected]);
    expect(bytesIn(w.ret(5))).toEqual([]);
  });

  it('send takes the ready descriptors, sends one type-0 request, and no slice ever carries a key, a nonce or a blob id', async () => {
    const w = world();
    await toReady(w);
    await openGeneral(w);
    const ids = await attachReady(w, 4, [file('a.txt', 'text/plain', 3), file('b.txt', 'text/plain', 4)]);
    w.call(5, { m: 'send', channelId: CH_HEX, text: 'two files', attachments: ids });
    await vi.waitFor(() => expect(w.ret(5)).toBeDefined());
    expect(w.ret(5)).toEqual({ t: 'ret', id: 5, ok: true, value: { msgId: '77'.repeat(16) } });
    expect(w.sync.sendRequest).toHaveBeenCalledTimes(1);
    const [group, request] = w.sync.sendRequest.mock.calls[0];
    expect(toHex(group)).toBe(toHex(GROUP));
    expect(request).toMatchObject({ type: 0, replyTo: null, body: 'two files' });
    expect(request.attachments.map((a) => [a.name, a.size, a.mime])).toEqual([['a.txt', 3, 'text/plain'], ['b.txt', 4, 'text/plain']]);
    expect(request.attachments.map((a) => toHex(a.blobId))).toEqual(w.routes.putBlob.mock.calls.map((c) => toHex(c[1])));
    await vi.waitFor(() => expect(trayOf(w)).toEqual([]));
    // The queued row as the core would list it, so the timeline slice is built with its files too.
    w.state.outbox = [{ msgId: new Uint8Array(16).fill(0x77), state: 0, error: '', created: NOW_S, body: 'two files', type: 0, replyTo: null,
      attachments: request.attachments.map((a) => ({ blobId: a.blobId, size: a.size, mime: a.mime, name: a.name })) }];
    w.sync.deps!.onOutboxChanged(GROUP);
    expect(w.timeline()?.items.at(-1)?.attachments.map((a) => a.name)).toEqual(['a.txt', 'b.txt']);
    const slices = w.posted.filter((m) => m.t === 'slice').map((m) => m.value);
    expect(bytesIn(slices)).toEqual([]);
    const text = JSON.stringify(slices);
    for (const a of request.attachments) {
      for (const secret of [a.key, a.nonce, a.blobId]) {
        expect(text).not.toContain(toHex(secret));
        expect(text).not.toContain(Array.from(secret).join(','));
      }
    }
  });

  it('an attachment-only message is accepted; a blank text with no attachments is refused', async () => {
    const w = world();
    await toReady(w);
    await openGeneral(w);
    const ids = await attachReady(w, 4, [file('only.txt', 'text/plain', 2)]);
    w.call(5, { m: 'send', channelId: CH_HEX, text: '', attachments: ids });
    await vi.waitFor(() => expect(w.ret(5)).toBeDefined());
    expect(w.ret(5)).toMatchObject({ ok: true });
    expect(w.sync.sendRequest.mock.calls[0][1]).toMatchObject({ type: 0, body: '' });
    w.call(6, { m: 'send', channelId: CH_HEX, text: ' ', attachments: [] });
    await vi.waitFor(() => expect(w.ret(6)).toBeDefined());
    expect(w.ret(6)).toMatchObject({ ok: false, error: { code: 'E_BAD_INPUT', detail: '' } });
  });

  it('a tray id that is not ready refuses the send and sends nothing', async () => {
    const w = world();
    await toReady(w);
    await openGeneral(w);
    const gate = deferred<{ created: boolean; size: number }>();
    w.routes.putBlob.mockImplementationOnce(() => gate.promise);
    w.call(4, { m: 'attachFiles', channelId: CH_HEX, files: [file('slow.txt', 'text/plain', 3)] });
    await vi.waitFor(() => expect(trayOf(w)?.[0]?.phase).toBe('uploading'));
    const id = trayOf(w)![0].id;
    w.call(5, { m: 'send', channelId: CH_HEX, text: 'too soon', attachments: [id] });
    await vi.waitFor(() => expect(w.ret(5)).toBeDefined());
    expect(w.ret(5)).toEqual({ t: 'ret', id: 5, ok: false, error: { code: 'E_TRAY_NOT_READY', detail: '', status: 0, retryAfterMs: null } });
    expect(w.sync.sendRequest).not.toHaveBeenCalled();
    gate.resolve({ created: true, size: 19 });
    await vi.waitFor(() => expect(trayOf(w)?.[0]?.phase).toBe('ready'));
  });

  it('attachFiles refuses five files and keeps an oversize failed card without upload', async () => {
    const w = world();
    await toReady(w);
    await openGeneral(w);
    w.call(4, { m: 'attachFiles', channelId: CH_HEX, files: [1, 2, 3, 4, 5].map((n) => file(`${String(n)}.txt`, 'text/plain', 1)) });
    w.call(5, { m: 'attachFiles', channelId: CH_HEX, files: [file('big.bin', 'application/octet-stream', BROWSER_ATTACHMENT_CAP + 1)] });
    w.call(6, { m: 'attachFiles', channelId: 'e'.repeat(32), files: [file('x.txt', 'text/plain', 1)] });
    await vi.waitFor(() => expect(w.ret(6)).toBeDefined());
    await vi.waitFor(() => expect(w.ret(5)).toBeDefined());
    expect((w.ret(4) as Refused).error).toMatchObject({ code: 'E_ATTACHMENT_COUNT', status: 0 });
    expect(w.ret(5)).toMatchObject({ ok: true });
    expect(w.last(`tray:${CH_HEX}`)).toEqual([expect.objectContaining({ name: 'big.bin', phase: 'failed', reason: 'E_ATTACHMENT_TOO_LARGE' })]);
    w.call(7, { m: 'checkpointFiles', channelId: CH_HEX });
    await vi.waitFor(() => expect(w.ret(7)).toMatchObject({ ok: true, value: [] }));
    expect((w.ret(6) as Refused).error).toEqual({ code: 'E_BAD_INPUT', detail: '', status: 0, retryAfterMs: null });
    expect(w.routes.putBlob).not.toHaveBeenCalled();
  });

  it('discardAttachment deletes an uploaded reference and drops the entry', async () => {
    const w = world();
    await toReady(w);
    await openGeneral(w);
    const [id] = await attachReady(w, 4, [file('oops.txt', 'text/plain', 4)]);
    w.call(5, { m: 'discardAttachment', channelId: CH_HEX, trayId: id });
    await vi.waitFor(() => expect(w.ret(5)).toEqual({ t: 'ret', id: 5, ok: true, value: null }));
    expect(w.routes.deleteBlob).toHaveBeenCalledTimes(1);
    expect(toHex(w.routes.deleteBlob.mock.calls[0][1])).toBe(toHex(w.routes.putBlob.mock.calls[0][1]));
    expect(trayOf(w)).toEqual([]);
  });

  it('beforeSend confirms each attachment of the row in order; onDiscarded deletes each and swallows failures', async () => {
    const w = world();
    await toReady(w);
    await openGeneral(w);
    const row: OutboxRow = { msgId: MSG, state: 0, error: '', created: NOW_S, body: '', type: 0, replyTo: null,
      attachments: [{ blobId: B1, size: 1, mime: 'text/plain', name: 'a' }, { blobId: B2, size: 2, mime: 'text/plain', name: 'b' }] };
    await w.sync.deps!.beforeSend(GROUP, row);
    expect(w.routes.confirmBlob.mock.calls.map((c) => [toHex(c[0]), toHex(c[1])])).toEqual([[CH_HEX, toHex(B1)], [CH_HEX, toHex(B2)]]);
    w.routes.confirmBlob.mockImplementationOnce(() => Promise.reject(refusal(404, 'E_NOT_FOUND')));
    await expect(w.sync.deps!.beforeSend(GROUP, row)).rejects.toMatchObject({ status: 404, code: 'E_NOT_FOUND' });
    w.routes.deleteBlob.mockImplementationOnce(() => Promise.reject(refusal(0, 'E_NETWORK')));
    w.sync.deps!.onDiscarded(GROUP, row);
    await vi.waitFor(() => expect(w.routes.deleteBlob).toHaveBeenCalledTimes(2));
    expect(w.routes.deleteBlob.mock.calls.map((c) => toHex(c[1]))).toEqual([toHex(B1), toHex(B2)]);
    await expect(w.sync.deps!.beforeSend(new Uint8Array(16).fill(0x99), row)).rejects.toMatchObject({ code: 'E_CORE_STATE', detail: 'unknown group' });
  });

  it('editMessage, deleteMessage, react and pin send the fold types aimed at the message', async () => {
    const w = world();
    await toReady(w);
    await openGeneral(w);
    w.call(4, { m: 'editMessage', channelId: CH_HEX, msgId: MSG_HEX, text: 'better words' });
    w.call(5, { m: 'deleteMessage', channelId: CH_HEX, msgId: MSG_HEX, seq: '5' });
    w.call(6, { m: 'react', channelId: CH_HEX, msgId: MSG_HEX, emoji: '👍', on: true });
    w.call(7, { m: 'react', channelId: CH_HEX, msgId: MSG_HEX, emoji: '👍', on: false });
    w.call(8, { m: 'pin', channelId: CH_HEX, msgId: MSG_HEX, on: true });
    w.call(9, { m: 'pin', channelId: CH_HEX, msgId: MSG_HEX, on: false });
    await vi.waitFor(() => expect(w.ret(9)).toBeDefined());
    for (const id of [4, 5, 6, 7, 8, 9]) expect(w.ret(id)).toEqual({ t: 'ret', id, ok: true, value: null });
    expect(w.sync.sendRequest.mock.calls.map(([g, r]) => [toHex(g), r.type, toHex(r.replyTo!), r.body, r.attachments])).toEqual([
      [toHex(GROUP), 1, MSG_HEX, 'better words', []],
      [toHex(GROUP), 3, MSG_HEX, '👍', []],
      [toHex(GROUP), 4, MSG_HEX, '👍', []],
      [toHex(GROUP), 5, MSG_HEX, '', []],
      [toHex(GROUP), 6, MSG_HEX, '', []],
      [toHex(GROUP), 2, MSG_HEX, '', []],
    ]);
    expect(w.routes.deleteGroupMessage).not.toHaveBeenCalled();
  });

  it("a core refusal of a fold crosses with its code and empty detail; a reply's msg id reaches the core", async () => {
    const w = world();
    await toReady(w);
    await openGeneral(w);
    w.sync.sendRequest.mockImplementationOnce(() => { throw new CoreError('E_CORE_INPUT', 'only the author may edit or delete'); });
    w.call(4, { m: 'editMessage', channelId: CH_HEX, msgId: MSG_HEX, text: 'not mine' });
    await vi.waitFor(() => expect(w.ret(4)).toBeDefined());
    expect(w.ret(4)).toEqual({ t: 'ret', id: 4, ok: false,
      error: { code: 'E_CORE_INPUT', detail: '', status: 0, retryAfterMs: null } });
    w.call(5, { m: 'send', channelId: CH_HEX, text: 'answer', replyTo: MSG_HEX });
    await vi.waitFor(() => expect(w.ret(5)).toBeDefined());
    expect(w.sync.sendRequest.mock.calls.at(-1)![1]).toEqual({ type: 0, replyTo: MSG, body: 'answer', attachments: [], threadId: null });
  });

  it('uses the uploader-only tombstone route to delete own content during an active timeout', async () => {
    const w = world();
    await toReady(w);
    await openGeneral(w);
    w.routes.getMemberTimeout.mockResolvedValue(1_800_000_000n);
    w.call(51, { m: 'deleteMessage', channelId: CH_HEX, msgId: MSG_HEX, seq: '5' });
    await vi.waitFor(() => expect(w.ret(51)).toEqual({ t: 'ret', id: 51, ok: true, value: null }));
    expect(w.routes.getMemberTimeout).toHaveBeenCalledWith(COMMUNITY, USER);
    expect(w.routes.deleteGroupMessage).toHaveBeenCalledWith(GROUP, 5n);
    expect(w.sync.sendRequest).not.toHaveBeenCalled();
  });

  it('a recorded purge runs after the outbox changed: the message, then each reference, then done', async () => {
    const w = world();
    await toReady(w);
    await openGeneral(w);
    w.call(4, { m: 'deleteMessage', channelId: CH_HEX, msgId: MSG_HEX });
    await vi.waitFor(() => expect(w.ret(4)).toBeDefined());
    await settle();
    expect(w.routes.deleteGroupMessage).not.toHaveBeenCalled();
    // The core records the purge when the type-2 row's confirm folds it (L-CORE-33); the engine then reports the outbox.
    w.state.purges = [purge(5n, [B1, B2])];
    w.sync.deps!.onOutboxChanged(GROUP);
    await vi.waitFor(() => expect(w.core.purgeDone).toHaveBeenCalledTimes(1));
    expect(w.calls.filter((c) => /^(deleteGroupMessage|deleteBlob|core\.purgeDone)/.test(c))).toEqual([
      'deleteGroupMessage(5)', `deleteBlob(${toHex(B1).slice(0, 8)})`, `deleteBlob(${toHex(B2).slice(0, 8)})`, 'core.purgeDone(5)',
    ]);
    expect(w.routes.deleteGroupMessage.mock.calls.map((c) => [toHex(c[0]), c[1]])).toEqual([[toHex(GROUP), 5n]]);
    expect(w.routes.deleteBlob.mock.calls.map((c) => toHex(c[0]))).toEqual([CH_HEX, CH_HEX]);
  });

  it('a failed purge waits for the next trigger; a 403 on a reference and a gone message count as done', async () => {
    const w = world();
    await toReady(w);
    await openGeneral(w);
    w.state.purges = [purge(6n, [B1, B2])];
    w.routes.deleteGroupMessage.mockImplementationOnce(() => Promise.reject(refusal(0, 'E_NETWORK')));
    w.sync.deps!.onOutboxChanged(GROUP);
    await vi.waitFor(() => expect(w.routes.deleteGroupMessage).toHaveBeenCalledTimes(1));
    await settle();
    expect(w.core.purgeDone).not.toHaveBeenCalled();
    expect(w.state.purges).toHaveLength(1);
    w.routes.deleteGroupMessage.mockImplementationOnce(() => Promise.resolve('gone'));
    w.routes.deleteBlob.mockImplementationOnce(() => Promise.reject(refusal(403, 'E_NOT_UPLOADER')));
    w.gateway.emit({ type: 'ready', info: READY_INFO });
    await vi.waitFor(() => expect(w.core.purgeDone).toHaveBeenCalledTimes(1));
    expect(w.routes.deleteGroupMessage).toHaveBeenCalledTimes(2);
    expect(w.routes.deleteBlob).toHaveBeenCalledTimes(2);
    expect(w.state.purges).toEqual([]);
  });

  it('a purge that fails on a reference with a server error stays for the next trigger', async () => {
    const w = world();
    await toReady(w);
    await openGeneral(w);
    w.state.purges = [purge(7n, [B1])];
    w.routes.deleteBlob.mockImplementationOnce(() => Promise.reject(refusal(503, 'E_UNAVAILABLE', 1000)));
    w.sync.deps!.onOutboxChanged(GROUP);
    await vi.waitFor(() => expect(w.routes.deleteBlob).toHaveBeenCalledTimes(1));
    await settle();
    expect(w.core.purgeDone).not.toHaveBeenCalled();
    w.sync.deps!.onOutboxChanged(GROUP);
    await vi.waitFor(() => expect(w.core.purgeDone).toHaveBeenCalledTimes(1));
  });

  it('purges run one at a time: triggers during a run make it go once more', async () => {
    const w = world();
    await toReady(w);
    await openGeneral(w);
    w.state.purges = [purge(8n, [])];
    const gate = deferred<'deleted' | 'gone'>();
    w.routes.deleteGroupMessage.mockImplementationOnce(() => gate.promise);
    w.sync.deps!.onOutboxChanged(GROUP);
    w.sync.deps!.onOutboxChanged(GROUP);
    w.sync.deps!.onOutboxChanged(GROUP);
    await settle();
    expect(w.routes.deleteGroupMessage).toHaveBeenCalledTimes(1);
    gate.resolve('deleted');
    await vi.waitFor(() => expect(w.core.purgeDone).toHaveBeenCalledTimes(1));
    await settle();
    expect(w.routes.deleteGroupMessage).toHaveBeenCalledTimes(1);
    expect(w.core.purges.mock.calls.length).toBeGreaterThanOrEqual(2);
  });

  it('stops a purge after an in-flight message delete when this device is revoked', async () => {
    const w = world();
    await toReady(w);
    await openGeneral(w);
    w.state.purges = [purge(10n, [B1, B2])];
    const gate = deferred<'deleted' | 'gone'>();
    w.routes.deleteGroupMessage.mockImplementationOnce(() => gate.promise);
    w.sync.deps!.onOutboxChanged(GROUP);
    await vi.waitFor(() => expect(w.routes.deleteGroupMessage).toHaveBeenCalledTimes(1));
    w.session.establish.mockResolvedValueOnce(false);
    w.gateway.emit({ type: 'revoked' });
    await vi.waitFor(() => expect(w.account()?.phase).toBe('revoked'));
    gate.resolve('deleted');
    await settle();
    expect(w.routes.deleteBlob).not.toHaveBeenCalled();
    expect(w.core.purgeDone).not.toHaveBeenCalled();
    expect(w.state.purges).toHaveLength(1);
  });

  it('stops a purge after an in-flight blob delete when this device is revoked', async () => {
    const w = world();
    await toReady(w);
    await openGeneral(w);
    w.state.purges = [purge(11n, [B1, B2])];
    const gate = deferred<void>();
    w.routes.deleteBlob.mockImplementationOnce(() => gate.promise);
    w.sync.deps!.onOutboxChanged(GROUP);
    await vi.waitFor(() => expect(w.routes.deleteBlob).toHaveBeenCalledTimes(1));
    w.session.establish.mockResolvedValueOnce(false);
    w.gateway.emit({ type: 'revoked' });
    await vi.waitFor(() => expect(w.account()?.phase).toBe('revoked'));
    gate.resolve();
    await settle();
    expect(w.routes.deleteBlob).toHaveBeenCalledTimes(1);
    expect(w.core.purgeDone).not.toHaveBeenCalled();
    expect(w.state.purges).toHaveLength(1);
  });

  it('loadPins publishes the pins and keeps them fresh until closePins', async () => {
    const w = world();
    await toReady(w);
    await openGeneral(w);
    w.state.pins = [{ targetSeq: 4n, msgId: MSG, pinnedSeq: 9n, byUser: USER, author: STRANGER, excerpt: 'pin me', targetTs: 1700000005n, threadRoot: null }];
    w.call(4, { m: 'loadPins', channelId: CH_HEX });
    await vi.waitFor(() => expect(w.ret(4)).toEqual({ t: 'ret', id: 4, ok: true, value: null }));
    expect(pinsOf(w)).toEqual([{ msgId: MSG_HEX, seq: '4', senderUser: toHex(STRANGER), excerpt: 'pin me', pinnedBy: toHex(USER), pinnedSeq: '9', ts: 1700000005, threadRoot: null }]);
    w.state.pins = [];
    w.sync.deps!.onGroupChanged(GROUP, applied(2));
    expect(pinsOf(w)).toEqual([]);
    w.call(5, { m: 'closePins', channelId: CH_HEX });
    await vi.waitFor(() => expect(w.ret(5)).toBeDefined());
    w.state.pins = [{ targetSeq: 4n, msgId: MSG, pinnedSeq: 10n, byUser: USER, author: null, excerpt: 'again', targetTs: 1700000005n, threadRoot: null }];
    w.sync.deps!.onGroupChanged(GROUP, applied(2));
    expect(pinsOf(w)).toEqual([]);
    w.call(6, { m: 'loadPins', channelId: 'e'.repeat(32) });
    await vi.waitFor(() => expect(w.ret(6)).toBeDefined());
    expect((w.ret(6) as Refused).error).toMatchObject({ code: 'E_BAD_INPUT', detail: '' });
  });

  it('openAttachment returns the decrypted file and its thumbnail as Blobs; a tampered file crosses E_BLOB_HASH', async () => {
    const w = world();
    await toReady(w);
    await openGeneral(w);
    const key = new Uint8Array(32).fill(0x55);
    const nonce = new Uint8Array(12).fill(0x56);
    const plain = new Uint8Array(1000).map((_, i) => i % 251);
    const { stored, blobId } = await sealBlob(key, nonce, plain);
    const thumb = await sealThumb(key, nonce, WEBP_STUB);
    w.state.descriptors.set('7:0', { blobId, key, nonce, size: 1000, mime: 'image/png', w: 640, h: 480, thumb, name: 'map\u0000.png' });
    w.state.descriptors.set('7:1', { blobId, key, nonce, size: 1000, mime: 'application/pdf', w: null, h: null, thumb: null, name: 'r/s.pdf' });
    w.state.blobs.set(toHex(blobId), stored);
    w.call(4, { m: 'openAttachment', channelId: CH_HEX, seq: '7', index: 0, thumb: false });
    w.call(5, { m: 'openAttachment', channelId: CH_HEX, seq: '7', index: 0, thumb: true });
    w.call(6, { m: 'openAttachment', channelId: CH_HEX, seq: '7', index: 1, thumb: false });
    w.call(7, { m: 'openAttachment', channelId: CH_HEX, seq: '7', index: 1, thumb: true });
    w.call(8, { m: 'openAttachment', channelId: CH_HEX, seq: '7', index: 2, thumb: false });
    await vi.waitFor(() => { for (const id of [4, 5, 6, 7, 8]) expect(w.ret(id)).toBeDefined(); });
    const full = okValue<{ blob: Blob; name: string; mime: string }>(w, 4);
    expect([full.name, full.mime, full.blob.type]).toEqual(['map.png', 'image/png', 'image/png']);
    expect(new Uint8Array(await full.blob.arrayBuffer())).toEqual(plain);
    const small = okValue<{ blob: Blob; name: string; mime: string }>(w, 5);
    expect(small.blob.type).toBe('image/webp');
    expect(new Uint8Array(await small.blob.arrayBuffer())).toEqual(WEBP_STUB);
    const doc = okValue<{ blob: Blob; name: string; mime: string }>(w, 6);
    expect([doc.name, doc.mime, doc.blob.type]).toEqual(['r_s.pdf', 'application/pdf', 'application/octet-stream']);
    expect((w.ret(7) as Refused).error).toMatchObject({ code: 'E_ATTACHMENT_MISSING' });
    expect((w.ret(8) as Refused).error).toMatchObject({ code: 'E_CORE_NOT_FOUND', detail: '' });
    expect(w.routes.getBlob).toHaveBeenCalledTimes(2);
    const tampered = stored.slice(); tampered[0] = (tampered[0] ?? 0) ^ 1;
    w.state.blobs.set(toHex(blobId), tampered);
    w.call(9, { m: 'openAttachment', channelId: CH_HEX, seq: '7', index: 0, thumb: false });
    await vi.waitFor(() => expect(w.ret(9)).toBeDefined());
    expect((w.ret(9) as Refused).error).toEqual({ code: 'E_BLOB_HASH', detail: '', status: 0, retryAfterMs: null });
  });

  it("roles: the own member row's roles reach the core once per change, and a role mention raises a mention notice", async () => {
    const w = world();
    w.state.roleIds = [ROLE];
    await toReady(w);
    await vi.waitFor(() => expect(w.core.ownRolesSet).toHaveBeenCalled());
    expect(w.core.ownRolesSet.mock.calls.map((c) => [toHex(c[0]), c[1].map(toHex)])).toEqual([[toHex(COMMUNITY), [toHex(ROLE)]]]);
    w.call(2, { m: 'selectCommunity', communityId: toHex(COMMUNITY) });
    await vi.waitFor(() => expect(w.ret(2)).toBeDefined());
    expect(w.core.ownRolesSet).toHaveBeenCalledTimes(1);
    w.state.groups = [textGroup(2)];
    w.state.rows = [{ ...strangerRow(1), body: `<@${toHex(ROLE)}> standup`, mention: true }];
    w.state.activity = [act(GROUP, 1, 1)];
    w.sync.deps!.onGroupChanged(GROUP, appliedSeqs([1n]));
    expect(w.notices()?.items.map((n) => [n.kind, n.body])).toEqual([['mention', `<@${toHex(ROLE)}> standup`]]);
    w.state.roleIds = [];
    w.gateway.emit({ type: 'ready', info: READY_INFO });
    await vi.waitFor(() => expect(w.core.ownRolesSet).toHaveBeenCalledTimes(2));
    expect(w.core.ownRolesSet.mock.calls[1][1]).toEqual([]);
  });

  it('publishes each member\'s role ids as hex on the members slice (INTERFACES-05)', async () => {
    const w = world();
    w.state.roleIds = [ROLE];
    await toReady(w);
    await vi.waitFor(() => expect(w.last<MemberSummary[]>(`members:${toHex(COMMUNITY)}`)).toBeDefined());
    const own = w.last<MemberSummary[]>(`members:${toHex(COMMUNITY)}`)!.find((m) => m.roleIds.length > 0);
    expect(own?.roleIds).toEqual([toHex(ROLE)]);
  });

  it('a notice is a mention only by the core\'s flag, never by a body an edit changed (FACTS-SECURITY-07)', async () => {
    const w = world();
    w.state.roleIds = [ROLE];
    await toReady(w);
    await vi.waitFor(() => expect(w.core.ownRolesSet).toHaveBeenCalled());
    w.state.groups = [textGroup(2)];
    w.state.rows = [{ ...strangerRow(1), body: `<@${toHex(ROLE)}> standup`, editedSeq: 2n, mention: false }];
    w.state.activity = [act(GROUP, 1, 1)];
    w.sync.deps!.onGroupChanged(GROUP, appliedSeqs([1n, 2n]));
    expect(w.notices()?.items.map((n) => n.kind)).toEqual(['message']);
  });

  it('a purge of a group this device is not a member of waits, and runs once the group is back (FACTS-SECURITY-08)', async () => {
    const w = world();
    await toReady(w);
    await openGeneral(w);
    w.state.groups = [textGroup(4)];
    w.state.purges = [purge(7n, [B1])];
    w.sync.deps!.onOutboxChanged(GROUP);
    await settle();
    expect(w.routes.deleteGroupMessage).not.toHaveBeenCalled();
    expect(w.state.purges).toHaveLength(1);
    w.state.groups = [textGroup(2)];
    w.gateway.emit({ type: 'ready', info: READY_INFO });
    await vi.waitFor(() => expect(w.core.purgeDone).toHaveBeenCalledTimes(1));
  });

  // Pre-flight ruling F6: a purge the core records while it adopts the own type-2 row in a catch-up (an apply, with no
  // outbox report of its own) runs after that group change, not only at the next outbox change or ready.
  it('a purge recorded by an apply runs after the group change (F6)', async () => {
    const w = world();
    await toReady(w);
    await openGeneral(w);
    w.state.purges = [purge(9n, [B1])];
    w.sync.deps!.onGroupChanged(GROUP, applied(2));
    await vi.waitFor(() => expect(w.core.purgeDone).toHaveBeenCalledTimes(1));
    expect(w.calls.filter((c) => /^(deleteGroupMessage|deleteBlob|core\.purgeDone)/.test(c))).toEqual([
      'deleteGroupMessage(9)', `deleteBlob(${toHex(B1).slice(0, 8)})`, 'core.purgeDone(9)',
    ]);
  });
});

// L-TS-50 and L-TS-53 (web-3 task 1): the seven call commands are in the contract with their shape rules, and answer
// E_UNAVAILABLE until task 6 implements them. Attacker: the owning page itself (a compromised or buggy page script);
// a malformed command is refused E_BAD_INPUT before the worker makes any route call on its behalf.
describe('call commands in the worker (task 6)', () => {
  const HEXID = 'ab'.repeat(16);
  const routeCalls = (w: World): number =>
    Object.values(w.routes).reduce((n, f) => n + (vi.isMockFunction(f) ? f.mock.calls.length : 0), 0);

  it('answers each well-formed call command with no call and no known channel, and names no changed user', async () => {
    const w = world();
    // eslint-disable-next-line @typescript-eslint/no-unused-vars -- the typed parameters give the double its call signature
    const declineCall = vi.fn((_call: Uint8Array, _ts: number) => Promise.resolve());
    Object.assign(w.routes, { declineCall });
    await toReady(w);
    await settle();
    const before = routeCalls(w);
    const commands = [
      { m: 'callJoin', channelId: HEXID, vdec: 'vp8,h264,vp9' },
      { m: 'callJoin', channelId: HEXID, vdec: '' },
      { m: 'callLeave', reason: 'user' }, { m: 'callLeave', reason: 'pagehide' }, { m: 'callLeave', reason: 'switch' },
      { m: 'callShare', on: true }, { m: 'callShare', on: false },
      { m: 'callRefresh' },
      { m: 'callStats', report: [3, null, 40, 0, 0, 1_048_576] },
      { m: 'callStats', report: [0, 2, 0, 1000, 7, 0] },
      { m: 'loadVoice', communityId: HEXID, channelId: null },
      { m: 'loadVoice', communityId: null, channelId: HEXID },
      { m: 'callDecline', callId: HEXID, ringTs: 1_758_659_030 },
      { m: 'callDecline', callId: HEXID, ringTs: 0 },
    ] as const;
    commands.forEach((command, i) => { w.call(7100 + i, command); });
    await vi.waitFor(() => { for (let i = 0; i < commands.length; i++) expect(w.ret(7100 + i)).toBeDefined(); });
    const expected: Record<string, string | null> = {
      callJoin: 'E_BAD_INPUT', loadVoice: 'E_BAD_INPUT', callShare: 'E_NO_CALL', callRefresh: 'E_NO_CALL', callStats: 'E_NO_CALL',
      callLeave: null, callDecline: null,
    };
    commands.forEach((command, i) => {
      const code = expected[command.m];
      expect(w.ret(7100 + i), JSON.stringify(command)).toMatchObject(code === null ? { ok: true, value: null } : { ok: false, error: { code, detail: '' } });
    });
    // Only the two declines reach the instance (L-HTTP-104: the ring timestamp as the body).
    expect(routeCalls(w)).toBe(before + 2);
    expect(declineCall.mock.calls.map((c) => [toHex(c[0]), c[1]])).toEqual([[HEXID, 1_758_659_030], [HEXID, 0]]);
    // I2: no call refused for a changed contact here, so every call slice keeps changedUser null; the worker does post them.
    const callSlices = w.posted.filter((p): p is SliceMessage => p.t === 'slice' && p.name === 'call');
    expect(callSlices.length).toBeGreaterThan(0);
    for (const s of callSlices) expect((s.value as { changedUser: unknown }).changedUser).toBeNull();
  });

  // Ruling I2 made to bite: a DM call refused for a changed contact names that contact in call.changedUser.
  it('a DM call refused E_UMK_CHANGED names the changed contact, before any route call', async () => {
    const w = world();
    w.state.dms = [{ channelId: DM_CHANNEL, kind: 3, members: [USER, STRANGER] }];
    w.state.groups = [{ groupId: DM_GROUP, kind: 0, communityId: null, targetId: DM_CHANNEL, state: 2, epoch: 1n, nextSeq: 1n,
      proposalsPending: 0, pendingCommit: false }];
    await toReady(w);
    await settle();
    w.core.sendBlockCode.mockImplementation((g: Uint8Array) => (toHex(g) === toHex(DM_GROUP) ? 'E_UMK_CHANGED' : null));
    w.core.pendingLeaves.mockReturnValue([{ groupId: DM_GROUP, leafIndex: 1, userId: STRANGER, cachedVersion: 1n, reason: 'changed' }]);
    const before = routeCalls(w);
    w.call(7200, { m: 'callJoin', channelId: toHex(DM_CHANNEL), vdec: '' });
    await vi.waitFor(() => { expect(w.ret(7200)).toBeDefined(); });
    expect(w.ret(7200)).toMatchObject({ ok: false, error: { code: 'E_UMK_CHANGED' } });
    expect(routeCalls(w)).toBe(before);
    expect(w.last<{ phase: string; changedUser: string | null }>('call')).toMatchObject({ phase: 'blocked', changedUser: toHex(STRANGER) });
  });

  it('refuses each malformed call command E_BAD_INPUT before any route call', async () => {
    const w = world();
    await toReady(w);
    await settle();
    const before = routeCalls(w);
    const bad: Record<string, unknown>[] = [
      { m: 'callJoin', channelId: 'AB'.repeat(16), vdec: '' },
      { m: 'callJoin', channelId: HEXID, vdec: 'vp8,vp8' },
      { m: 'callJoin', channelId: HEXID, vdec: 'vp8,h264,vp9,vp8' },
      { m: 'callJoin', channelId: HEXID, vdec: 'VP8' },
      { m: 'callJoin', channelId: HEXID, vdec: 'av1' },
      { m: 'callJoin', channelId: HEXID, vdec: 'vp8,' },
      { m: 'callJoin', channelId: HEXID },
      { m: 'callLeave', reason: 'bye' },
      { m: 'callLeave' },
      { m: 'callShare', on: 1 },
      { m: 'callStats', report: [3, null, 40, 0, 0] },
      { m: 'callStats', report: [3, null, 1_048_577, 0, 0, 0] },
      { m: 'callStats', report: [null, 0, 40, 0, 0, 0] },
      { m: 'callStats', report: [3, null, 40, -1, 0, 0] },
      { m: 'callStats', report: [3, 1.5, 40, 0, 0, 0] },
      { m: 'callStats', report: [3, null, 40, 0, 0, 0, 0] },
      { m: 'callStats', report: 'x' },
      { m: 'loadVoice', communityId: HEXID, channelId: HEXID },
      { m: 'loadVoice', communityId: null, channelId: null },
      { m: 'loadVoice', communityId: 'x', channelId: null },
      { m: 'callDecline', callId: 'ab', ringTs: 1 },
      { m: 'callDecline', callId: HEXID, ringTs: -1 },
      { m: 'callDecline', callId: HEXID, ringTs: 1.5 },
      { m: 'callDecline', callId: HEXID, ringTs: 2 ** 53 },
      { m: 'callDecline', callId: HEXID, ringTs: '1' },
    ];
    bad.forEach((command, i) => { w.call(7200 + i, command); });
    bad.forEach((command, i) => {
      expect(w.ret(7200 + i), JSON.stringify(command)).toMatchObject({ ok: false, error: { code: 'E_BAD_INPUT', detail: '' } });
    });
    expect(routeCalls(w)).toBe(before);
  });

  it('lets the six call codes through sanitizeWorkerError', async () => {
    const { sanitizeWorkerError } = await import('./controller');
    for (const code of ['E_CALL_FULL', 'E_CALL_SHARERS_FULL', 'E_CALL_ACTIVE', 'E_REMOVE_PENDING', 'E_NO_CALL', 'E_DISCONNECTED']) {
      const e = new DillaHttpError({ code, detail: 'secret detail', status: 409, retryAfterMs: null, extra: [] });
      expect(sanitizeWorkerError(e)).toEqual({ code, detail: '', status: 409, retryAfterMs: null });
    }
  });
});

// v0.1.0-web3.1: a fresh instance is usable from the web client alone. The three create commands go through the same
// input checks, phase gate and refusal codes as every other command, and the state a page selects next is published
// before the command answers.
describe('Controller create flow (createCommunity, createChannel, createInvite)', () => {
  const sliceAt = (w: World, name: string, pred: (v: unknown) => boolean): number =>
    w.posted.findIndex((m) => m.t === 'slice' && m.name === name && pred(m.value));
  const retAt = (w: World, id: number): number => w.posted.findIndex((m) => m.t === 'ret' && m.id === id);

  it('createCommunity posts the name, lists the new server with its owner before it answers, and answers its id', async () => {
    const w = world();
    await toReady(w);
    w.call(80, { m: 'createCommunity', name: 'Midgard Crew' });
    await vi.waitFor(() => expect(w.ret(80)).toBeDefined());
    expect(w.ret(80)).toEqual({ t: 'ret', id: 80, ok: true, value: { communityId: toHex(NEW_COMMUNITY) } });
    expect(w.routes.createCommunity).toHaveBeenCalledWith('Midgard Crew');
    expect(w.last<CommunitySummary[]>('communities')).toContainEqual({ id: toHex(NEW_COMMUNITY), name: 'Midgard Crew', owner: toHex(USER) });
    const listed = sliceAt(w, 'communities', (v) => (v as CommunitySummary[]).some((c) => c.id === toHex(NEW_COMMUNITY)));
    expect(listed).toBeGreaterThan(-1);
    expect(listed).toBeLessThan(retAt(w, 80));
  });

  it('a server created while the list cannot be read is listed all the same, owned by this account', async () => {
    const w = world();
    await toReady(w);
    w.routes.listCommunities.mockImplementationOnce(() => Promise.reject(refusal(503, 'E_UNAVAILABLE', 1000)));
    w.call(81, { m: 'createCommunity', name: 'Valhalla' });
    await vi.waitFor(() => expect(w.ret(81)).toMatchObject({ ok: true, value: { communityId: toHex(NEW_COMMUNITY) } }));
    expect(w.last<CommunitySummary[]>('communities')).toEqual([
      { id: toHex(COMMUNITY), name: 'friends', owner: toHex(USER) },
      { id: toHex(NEW_COMMUNITY), name: 'Valhalla', owner: toHex(USER) },
    ]);
    // So the page's next command, the starter channel, is accepted for it.
    w.call(82, { m: 'createChannel', communityId: toHex(NEW_COMMUNITY), kind: 'text', name: 'general' });
    await vi.waitFor(() => expect(w.ret(82)).toMatchObject({ ok: true }));
  });

  it('createChannel posts the kind and name at the next position and lists the channel before it answers', async () => {
    const w = world();
    await toReady(w);
    w.call(83, { m: 'createChannel', communityId: toHex(COMMUNITY), kind: 'voice', name: 'lounge' });
    await vi.waitFor(() => expect(w.ret(83)).toBeDefined());
    const channelId = toHex(new Uint8Array(16).fill(0x60));
    expect(w.ret(83)).toEqual({ t: 'ret', id: 83, ok: true, value: { channelId } });
    // general holds position 0, so the new channel goes after it.
    expect(w.routes.createChannel).toHaveBeenCalledWith(COMMUNITY, { kind: 'voice', name: 'lounge', position: 1 });
    expect(w.channels()?.find((c) => c.id === channelId)).toMatchObject({ kind: 1, mode: 0, name: 'lounge', position: 1, communityId: toHex(COMMUNITY) });
    const listed = sliceAt(w, `channels:${toHex(COMMUNITY)}`, (v) => (v as ChannelSummary[]).some((c) => c.id === channelId));
    expect(listed).toBeGreaterThan(-1);
    expect(listed).toBeLessThan(retAt(w, 83));
  });

  it('a new channel skips the AFK channel at 2^30 when it takes the next position (R42)', async () => {
    const w = world();
    const AFK = new Uint8Array(16).fill(0x61);
    w.routes.listChannels.mockResolvedValue([
      { id: CHANNEL, kind: 0, mode: 0, visibility: 0, parentId: null, name: 'general', topic: '', position: 0, slowmode: 0, seq: 1n, textGroupId: GROUP },
      { id: AFK, kind: 1, mode: 0, visibility: 0, parentId: null, name: 'AFK', topic: '', position: 2 ** 30, slowmode: 0, seq: 0n, textGroupId: null },
    ]);
    w.routes.getAFK.mockResolvedValue({ channelId: AFK, timeoutSeconds: 300, pending: false, away: [] });
    await toReady(w);
    await vi.waitFor(() => expect(w.last<{ channelId: string | null }>(`afk:${toHex(COMMUNITY)}`)?.channelId).toBe(toHex(AFK)));
    await vi.waitFor(() => expect(w.channels()?.some((c) => c.id === toHex(AFK))).toBe(true));
    w.call(88, { m: 'createChannel', communityId: toHex(COMMUNITY), kind: 'text', name: 'next' });
    await vi.waitFor(() => expect(w.ret(88)).toMatchObject({ ok: true }));
    expect(w.routes.createChannel).toHaveBeenCalledWith(COMMUNITY, { kind: 'text', name: 'next', position: 1 });
  });

  it('a create right after the community skips the AFK position 2^30 before the afk: slice is read (R-AFK6)', async () => {
    const w = world();
    const AFK = new Uint8Array(16).fill(0x61);
    w.routes.listChannels.mockResolvedValue([
      { id: CHANNEL, kind: 0, mode: 0, visibility: 0, parentId: null, name: 'general', topic: '', position: 0, slowmode: 0, seq: 1n, textGroupId: GROUP },
      { id: AFK, kind: 1, mode: 0, visibility: 0, parentId: null, name: 'AFK', topic: '', position: 2 ** 30, slowmode: 0, seq: 0n, textGroupId: null },
    ]);
    // The AFK snapshot never answers: the slice stays unread while the create runs.
    w.routes.getAFK.mockReturnValue(new Promise(() => undefined));
    await toReady(w);
    await vi.waitFor(() => expect(w.channels()?.some((c) => c.id === toHex(AFK))).toBe(true));
    expect(w.last(`afk:${toHex(COMMUNITY)}`) ?? null).toBeNull();
    w.call(89, { m: 'createChannel', communityId: toHex(COMMUNITY), kind: 'voice', name: 'general' });
    await vi.waitFor(() => expect(w.ret(89)).toMatchObject({ ok: true }));
    expect(w.routes.createChannel).toHaveBeenCalledWith(COMMUNITY, { kind: 'voice', name: 'general', position: 1 });
  });

  it('createChannel with a parentId creates in that category in one request, with no follow-up move', async () => {
    const w = world();
    await toReady(w);
    const parent = toHex(new Uint8Array(16).fill(0x71));
    w.call(84, { m: 'createChannel', communityId: toHex(COMMUNITY), kind: 'text', name: 'plans', parentId: parent });
    await vi.waitFor(() => expect(w.ret(84)).toMatchObject({ ok: true }));
    expect(w.routes.createChannel).toHaveBeenCalledWith(COMMUNITY, { kind: 'text', name: 'plans', position: 1, parentId: new Uint8Array(16).fill(0x71) });
  });

  it('createChannel refuses a parentId that is not 32 lowercase hex, and passes none when it is absent', async () => {
    const w = world();
    await toReady(w);
    w.call(85, { m: 'createChannel', communityId: toHex(COMMUNITY), kind: 'text', name: 'x', parentId: 'nope' });
    await vi.waitFor(() => expect(w.ret(85)).toMatchObject({ ok: false, error: { code: 'E_BAD_INPUT' } }));
    expect(w.routes.createChannel).not.toHaveBeenCalled();
  });

  it('a listed channel carries its slow mode into the channels slice, and a created one starts at 0', async () => {
    const w = world();
    w.routes.listChannels.mockResolvedValue([{ id: CHANNEL, kind: 0, mode: 0, visibility: 0, parentId: null, name: 'general', topic: '',
      position: 0, slowmode: 30, seq: 1n, textGroupId: GROUP }]);
    await toReady(w);
    await vi.waitFor(() => expect(w.channels()?.find((c) => c.id === toHex(CHANNEL))?.slowmode).toBe(30));
    w.call(87, { m: 'createChannel', communityId: toHex(COMMUNITY), kind: 'text', name: 'new' });
    await vi.waitFor(() => expect(w.ret(87)).toMatchObject({ ok: true }));
    const created = sliceAt(w, `channels:${toHex(COMMUNITY)}`, (v) => (v as ChannelSummary[]).some((c) => c.name === 'new' && c.slowmode === 0));
    expect(created).toBeGreaterThan(-1);
  });

  it('the starter channels of a new server go to positions 0 and 1', async () => {
    const w = world();
    await toReady(w);
    w.call(84, { m: 'createCommunity', name: 'Midgard Crew' });
    await vi.waitFor(() => expect(w.ret(84)).toMatchObject({ ok: true }));
    w.call(85, { m: 'createChannel', communityId: toHex(NEW_COMMUNITY), kind: 'text', name: 'general' });
    await vi.waitFor(() => expect(w.ret(85)).toMatchObject({ ok: true }));
    w.call(86, { m: 'createChannel', communityId: toHex(NEW_COMMUNITY), kind: 'voice', name: 'general' });
    await vi.waitFor(() => expect(w.ret(86)).toMatchObject({ ok: true }));
    expect(w.routes.createChannel.mock.calls.map((c) => c[1])).toEqual([
      { kind: 'text', name: 'general', position: 0 }, { kind: 'voice', name: 'general', position: 1 },
    ]);
    expect(w.last<ChannelSummary[]>(`channels:${toHex(NEW_COMMUNITY)}`)?.map((c) => [c.name, c.kind])).toEqual([['general', 0], ['general', 1]]);
  });

  it('a list read that began before a channel was created does not hide that channel; it is read again', async () => {
    const w = world();
    await toReady(w);
    const held = deferred<Awaited<ReturnType<typeof w.routes.listChannels>>>();
    w.routes.listChannels.mockImplementationOnce(() => held.promise);
    w.call(87, { m: 'selectCommunity', communityId: toHex(COMMUNITY) });
    await vi.waitFor(() => expect(w.routes.listChannels).toHaveBeenCalledTimes(2));
    w.call(88, { m: 'createChannel', communityId: toHex(COMMUNITY), kind: 'text', name: 'random' });
    await vi.waitFor(() => expect(w.ret(88)).toMatchObject({ ok: true }));
    const channelId = toHex(new Uint8Array(16).fill(0x60));
    // The stale answer: the list as it was before the create.
    held.resolve([{ id: CHANNEL, kind: 0, mode: 0, visibility: 0, parentId: null, name: 'general', topic: '', position: 0, slowmode: 0, seq: 1n, textGroupId: GROUP }]);
    await vi.waitFor(() => expect(w.ret(87)).toMatchObject({ ok: true }));
    await settle();
    expect(w.channels()?.map((c) => c.id)).toContain(channelId);
  });

  it('createInvite passes the uses and the lifetime and answers the link, the code and the expiry', async () => {
    const w = world();
    await toReady(w);
    w.call(89, { m: 'createInvite', communityId: toHex(COMMUNITY), maxUses: 10, ttlSeconds: 604_800 });
    await vi.waitFor(() => expect(w.ret(89)).toBeDefined());
    expect(w.routes.createInvite).toHaveBeenCalledWith(COMMUNITY, { maxUses: 10, ttlSeconds: 604_800 });
    expect(w.ret(89)).toEqual({ t: 'ret', id: 89, ok: true,
      value: { url: 'https://dilla.test/i/AB12CD34', code: 'AB12CD34', expires: Number(NOW_S + 604_800n) } });
  });

  it('createInvite stores nothing: no slice changes, and no other route or core call', async () => {
    const w = world();
    await toReady(w);
    await settle();
    const posted = w.posted.length;
    const calls = w.calls.length;
    w.call(95, { m: 'createInvite', communityId: toHex(COMMUNITY), maxUses: 10, ttlSeconds: 604_800 });
    await vi.waitFor(() => expect(w.ret(95)).toMatchObject({ ok: true }));
    await settle();
    expect(w.posted.slice(posted).filter((m) => m.t === 'slice')).toEqual([]);
    expect(w.calls.slice(calls)).toEqual([]);
  });

  it('a server name of exactly 255 bytes of emoji is accepted; 256 bytes is refused before any request', async () => {
    const w = world();
    await toReady(w);
    const edge = '😀'.repeat(63) + 'abc';                         // 63×4 + 3 = 255 UTF-8 bytes
    w.call(96, { m: 'createCommunity', name: edge });
    await vi.waitFor(() => expect(w.ret(96)).toMatchObject({ ok: true }));
    expect(w.routes.createCommunity).toHaveBeenCalledWith(edge);
    w.call(97, { m: 'createCommunity', name: '😀'.repeat(64) });
    expect(w.ret(97)).toMatchObject({ ok: false, error: { code: 'E_BAD_INPUT' } });
    expect(w.routes.createCommunity).toHaveBeenCalledTimes(1);
  });

  it('a refused create crosses with its code and status; an unknown server is refused before any request', async () => {
    const w = world();
    await toReady(w);
    w.routes.createChannel.mockImplementationOnce(() => Promise.reject(refusal(403, 'E_FORBIDDEN')));
    w.call(90, { m: 'createChannel', communityId: toHex(COMMUNITY), kind: 'text', name: 'random' });
    await vi.waitFor(() => expect(w.ret(90)).toBeDefined());
    expect(w.ret(90)).toEqual({ t: 'ret', id: 90, ok: false, error: { code: 'E_FORBIDDEN', detail: '', status: 403, retryAfterMs: null } });
    w.routes.createInvite.mockImplementationOnce(() => Promise.reject(refusal(403, 'E_FORBIDDEN')));
    w.call(91, { m: 'createInvite', communityId: toHex(COMMUNITY), maxUses: 10, ttlSeconds: 604_800 });
    await vi.waitFor(() => expect(w.ret(91)).toMatchObject({ ok: false, error: { code: 'E_FORBIDDEN', status: 403 } }));
    w.routes.createCommunity.mockImplementationOnce(() => Promise.reject(refusal(400, 'E_INVALID_REQUEST')));
    w.call(92, { m: 'createCommunity', name: 'x' });
    await vi.waitFor(() => expect(w.ret(92)).toMatchObject({ ok: false, error: { code: 'E_INVALID_REQUEST', status: 400 } }));
    const unknown = 'ab'.repeat(16);
    w.call(93, { m: 'createChannel', communityId: unknown, kind: 'text', name: 'random' });
    w.call(94, { m: 'createInvite', communityId: unknown, maxUses: 10, ttlSeconds: 604_800 });
    await vi.waitFor(() => expect(w.ret(94)).toBeDefined());
    expect(w.ret(93)).toMatchObject({ ok: false, error: { code: 'E_BAD_INPUT' } });
    expect(w.ret(94)).toMatchObject({ ok: false, error: { code: 'E_BAD_INPUT' } });
    expect(w.routes.createChannel).toHaveBeenCalledTimes(1);
    expect(w.routes.createInvite).toHaveBeenCalledTimes(1);
  });

  it('refuses malformed create commands as E_BAD_INPUT, and every create outside the ready phase as E_NOT_READY', async () => {
    const w = world();
    await toReady(w);
    const bad: [number, Record<string, unknown>][] = [
      [100, { m: 'createCommunity', name: '' }],
      [101, { m: 'createCommunity', name: '   ' }],
      [102, { m: 'createCommunity', name: 'x'.repeat(256) }],
      [103, { m: 'createCommunity', name: 'bell\u0007' }],
      [104, { m: 'createCommunity', name: 7 }],
      [105, { m: 'createChannel', communityId: toHex(COMMUNITY), kind: 'category', name: 'x' }],
      [106, { m: 'createChannel', communityId: toHex(COMMUNITY), kind: 'text', name: 'x'.repeat(101) }],
      [107, { m: 'createChannel', communityId: 'C0'.repeat(16), kind: 'text', name: 'x' }],
      [108, { m: 'createChannel', communityId: toHex(COMMUNITY), kind: 'text', name: ' ' }],
      [109, { m: 'createInvite', communityId: toHex(COMMUNITY), maxUses: -1, ttlSeconds: 60 }],
      [110, { m: 'createInvite', communityId: toHex(COMMUNITY), maxUses: 10, ttlSeconds: 30 * 24 * 3600 + 1 }],
      [111, { m: 'createInvite', communityId: toHex(COMMUNITY), maxUses: '10', ttlSeconds: 60 }],
    ];
    for (const [id, command] of bad) w.call(id, command);
    for (const [id] of bad) expect(w.ret(id), String(id)).toMatchObject({ ok: false, error: { code: 'E_BAD_INPUT', detail: '' } });
    expect(w.routes.createCommunity).not.toHaveBeenCalled();
    expect(w.routes.createChannel).not.toHaveBeenCalled();
    expect(w.routes.createInvite).not.toHaveBeenCalled();

    const fresh = world({ phase: 0 });
    fresh.call(1, { m: 'start' });
    await vi.waitFor(() => expect(fresh.account()?.phase).toBe('needs-signup'));
    fresh.call(2, { m: 'createCommunity', name: 'Midgard' });
    fresh.call(3, { m: 'createChannel', communityId: toHex(COMMUNITY), kind: 'text', name: 'x' });
    fresh.call(4, { m: 'createInvite', communityId: toHex(COMMUNITY), maxUses: 10, ttlSeconds: 60 });
    for (const id of [2, 3, 4]) expect(fresh.ret(id)).toMatchObject({ ok: false, error: { code: 'E_NOT_READY' } });
  });

  // WB I1: a hung list read after the create no longer holds the answer, so the dialog is never left busy by it.
  it('createChannel answers once the row is published, without waiting for the list read after it', async () => {
    const w = world();
    await toReady(w);
    await settle();
    const held = deferred<Awaited<ReturnType<typeof w.routes.listChannels>>>();
    w.routes.listChannels.mockImplementationOnce(() => held.promise);
    w.call(120, { m: 'createChannel', communityId: toHex(COMMUNITY), kind: 'text', name: 'random' });
    await vi.waitFor(() => expect(w.ret(120)).toMatchObject({ ok: true, value: { channelId: toHex(new Uint8Array(16).fill(0x60)) } }));
    expect(w.channels()?.map((c) => c.name)).toEqual(['general', 'random']);
    held.resolve([{ id: CHANNEL, kind: 0, mode: 0, visibility: 0, parentId: null, name: 'general', topic: '', position: 0, slowmode: 0, seq: 1n, textGroupId: GROUP },
      w.state.newChannels[0].row]);
    await settle();
    expect(w.channels()?.map((c) => c.name)).toEqual(['general', 'random']);
  });

  // dilla-web-3.1: op 54 community_changed. Another member made a channel; this open tab reads the list again, once for a
  // burst of notices, and only for a server it lists.
  it('community_changed reloads a listed server\'s channels once, after a short wait; an unknown server is ignored', async () => {
    const w = world();
    await toReady(w);
    await settle();
    const before = w.routes.listChannels.mock.calls.length;
    w.state.newChannels.push({ communityId: COMMUNITY, row: { id: new Uint8Array(16).fill(0x66), kind: 0, mode: 0, visibility: 0,
      parentId: null, name: 'planering', topic: '', position: 1, slowmode: 0, seq: 0n, textGroupId: null } });
    const changed = (community: Uint8Array): GatewayEvent => ({ type: 'frame', frame: { op: Op.communityChanged, n: 0n, groupId: null, payload: [community] } });
    w.gateway.emit(changed(COMMUNITY));
    w.gateway.emit(changed(COMMUNITY));
    w.gateway.emit(changed(new Uint8Array(16).fill(0xee)));
    expect(w.routes.listChannels).toHaveBeenCalledTimes(before);
    await vi.waitFor(() => expect(w.channels()?.map((c) => c.name)).toEqual(['general', 'planering']));
    await new Promise((resolve) => { setTimeout(resolve, 600); });
    expect(w.routes.listChannels.mock.calls.slice(before).map((c) => toHex(c[0]))).toEqual([toHex(COMMUNITY)]);
  });

  // A renamed server: op 54 also reads the server list again (the same debounce, one read for a burst), so the rail's
  // name follows without a reload.
  it('community_changed reads the server list again too, once for a burst, so a new name shows', async () => {
    const w = world();
    await toReady(w);
    await settle();
    const before = w.routes.listCommunities.mock.calls.length;
    const list = w.routes.listCommunities.getMockImplementation()!;
    w.routes.listCommunities.mockImplementation(async () => (await list()).map((c) => ({ ...c, name: c.name === 'friends' ? 'vänner' : c.name })));
    const changed: GatewayEvent = { type: 'frame', frame: { op: Op.communityChanged, n: 0n, groupId: null, payload: [COMMUNITY] } };
    w.gateway.emit(changed);
    w.gateway.emit(changed);
    expect(w.routes.listCommunities).toHaveBeenCalledTimes(before);
    await vi.waitFor(() => expect(w.last<CommunitySummary[]>('communities')?.find((c) => c.id === toHex(COMMUNITY))?.name).toBe('vänner'));
    await new Promise((resolve) => { setTimeout(resolve, 600); });
    expect(w.routes.listCommunities).toHaveBeenCalledTimes(before + 1);
  });

  // Lane B's lost createCommunity answer: the page asks for the server list again, with the socket still up, and finds
  // the server the lost answer made. Ready only; the list is published before the (empty) answer.
  it('refreshCommunities reads the server list again and publishes it before it answers null', async () => {
    const w = world();
    await toReady(w);
    await settle();
    const before = w.routes.listCommunities.mock.calls.length;
    w.state.created.push({ id: NEW_COMMUNITY, name: 'Lost answer' });
    const held = deferred<void>();
    const list = w.routes.listCommunities.getMockImplementation()!;
    w.routes.listCommunities.mockImplementationOnce(async () => { await held.promise; return list(); });
    w.call(122, { m: 'refreshCommunities' });
    await settle();
    expect(w.ret(122)).toBeUndefined();
    held.resolve();
    await vi.waitFor(() => expect(w.ret(122)).toBeDefined());
    expect(w.ret(122)).toEqual({ t: 'ret', id: 122, ok: true, value: null });
    expect(w.routes.listCommunities).toHaveBeenCalledTimes(before + 1);
    const listed = sliceAt(w, 'communities', (v) => (v as CommunitySummary[]).some((c) => c.id === toHex(NEW_COMMUNITY)));
    expect(listed).toBeGreaterThan(-1);
    expect(listed).toBeLessThan(retAt(w, 122));

    const fresh = world({ phase: 0 });
    fresh.call(1, { m: 'start' });
    await vi.waitFor(() => expect(fresh.account()?.phase).toBe('needs-signup'));
    fresh.call(2, { m: 'refreshCommunities' });
    expect(fresh.ret(2)).toMatchObject({ ok: false, error: { code: 'E_NOT_READY' } });
  });

  // The cheap belt beside op 54: a Welcome this device did not expect can be a new channel's group, so the selected
  // server's channels are read again too.
  it('an unexpected Welcome also reads the selected server\'s channels again', async () => {
    const w = world();
    await toReady(w);
    w.call(121, { m: 'selectCommunity', communityId: toHex(COMMUNITY) });
    await vi.waitFor(() => expect(w.ret(121)).toMatchObject({ ok: true }));
    await settle();
    const before = w.routes.listChannels.mock.calls.length;
    w.state.newChannels.push({ communityId: COMMUNITY, row: { id: new Uint8Array(16).fill(0x67), kind: 0, mode: 0, visibility: 0,
      parentId: null, name: 'nytt', topic: '', position: 1, slowmode: 0, seq: 0n, textGroupId: null } });
    w.sync.deps!.onUnexpectedWelcome(GROUP);
    await vi.waitFor(() => expect(w.channels()?.map((c) => c.name)).toEqual(['general', 'nytt']));
    expect(w.routes.listChannels.mock.calls.slice(before).map((c) => toHex(c[0]))).toEqual([toHex(COMMUNITY)]);
  });
});

// CB2: the engine's onSlow and onPosted become a `slow:<channelId>` slice, only for a channel with slow mode on (R6).
describe('slow mode slices (CB2)', () => {
  const T0 = 1_700_000_000_000;
  const slowSlices = (w: World): SliceMessage[] =>
    w.posted.filter((m): m is SliceMessage => m.t === 'slice' && m.name.startsWith('slow:'));
  async function slowWorld(slowmode: number): Promise<{ w: World; clock: { t: number } }> {
    const clock = { t: T0 };
    const w = world({ now: () => clock.t });
    w.routes.listChannels.mockResolvedValue([{ id: CHANNEL, kind: 0, mode: 0, visibility: 0, parentId: null, name: 'general', topic: '',
      position: 0, slowmode, seq: 1n, textGroupId: GROUP }]);
    await toReady(w);
    await vi.waitFor(() => expect(w.channels()?.find((c) => c.id === toHex(CHANNEL))?.slowmode).toBe(slowmode));
    return { w, clock };
  }

  it('a 429 on a channel with slow mode publishes the wait; on a channel without it nothing', async () => {
    const { w } = await slowWorld(10);
    w.sync.deps!.onSlow(GROUP, T0 + 5000, true);
    expect(w.last(`slow:${toHex(CHANNEL)}`)).toEqual({ until: T0 + 5000, queued: true });
    const off = await slowWorld(0);
    off.w.sync.deps!.onSlow(GROUP, T0 + 5000, true);
    expect(slowSlices(off.w)).toEqual([]);
  });

  it('a confirmed own post starts the countdown; a reaction publishes nothing', async () => {
    const { w, clock } = await slowWorld(10);
    clock.t = T0 + 123;
    w.sync.deps!.onPosted(GROUP, 3);
    expect(slowSlices(w)).toEqual([]);
    w.sync.deps!.onPosted(GROUP, 0);
    expect(w.last(`slow:${toHex(CHANNEL)}`)).toEqual({ until: T0 + 123 + 10_000, queued: false });
  });

  it('the clear publishes until 0; a queued post that then sends ends at its own countdown', async () => {
    const { w, clock } = await slowWorld(10);
    w.sync.deps!.onSlow(GROUP, T0 + 5000, true);
    clock.t = T0 + 5000;
    w.sync.deps!.onSlow(GROUP, 0, false);
    expect(w.last(`slow:${toHex(CHANNEL)}`)).toEqual({ until: 0, queued: false });
    w.sync.deps!.onPosted(GROUP, 0);
    expect(slowSlices(w).map((m) => m.value)).toEqual([
      { until: T0 + 5000, queued: true }, { until: 0, queued: false }, { until: T0 + 5000 + 10_000, queued: false },
    ]);
  });

  it('a DM group never publishes', async () => {
    const { w } = await slowWorld(10);
    w.state.dms = [{ channelId: DM_CHANNEL, kind: 3, members: [USER, STRANGER] }];
    w.state.groups.push({ ...textGroup(2), groupId: DM_GROUP, communityId: null, targetId: DM_CHANNEL });
    w.sync.deps!.onUnexpectedWelcome(DM_GROUP);
    await vi.waitFor(() => expect(w.last<DmSummary[]>('dms')?.[0]?.id).toBe(toHex(DM_CHANNEL)));
    w.sync.deps!.onSlow(DM_GROUP, T0 + 5000, true);
    w.sync.deps!.onPosted(DM_GROUP, 0);
    w.sync.deps!.onSlow(DM_GROUP, 0, false);
    expect(slowSlices(w)).toEqual([]);
  });

  it('sign-out resets the slow slice', async () => {
    const { w } = await slowWorld(10);
    w.sync.deps!.onSlow(GROUP, T0 + 5000, true);
    w.call(2, { m: 'signOutRevoke', recoveryKey: RECOVERY_KEY });
    await vi.waitFor(() => expect(w.ret(2)).toMatchObject({ ok: true }));
    expect(w.last('communities')).toEqual([]);
    expect(w.last(`slow:${toHex(CHANNEL)}`)).toEqual({ until: 0, queued: false });
  });

});

describe('pending role-panel actions', () => {
  afterEach(() => { vi.useRealTimers(); });

  it('retries a transient accepted-action failure online with backoff, then clears the journal', async () => {
    const w = world();
    await toReady(w);
    w.gateway.emit({ type: 'status', status: 'ready', closeCode: null });
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    const key = `rolePanel.pending.${'42'.repeat(16)}`;
    const action = {
      communityId: toHex(COMMUNITY), channelId: toHex(CHANNEL), messageSeq: '7', groupId: toHex(GROUP), eventSeq: '9',
      emoji: '👍', enabled: true, userId: toHex(USER), deviceId: toHex(DEVICE),
    };
    w.core.settingPut(key, JSON.stringify(action));
    w.routes.applyRolePanelReaction
      .mockRejectedValueOnce(new DillaHttpError({ status: 503, code: 'E_INTERNAL', detail: '', retryAfterMs: null, extra: [] }))
      .mockResolvedValueOnce();
    const retry = w.controller as unknown as { retryPendingRolePanelActions(core: CorePort, me: { userId: Uint8Array; deviceId: Uint8Array }): void };

    retry.retryPendingRolePanelActions(w.core as unknown as CorePort, { userId: USER, deviceId: DEVICE });
    await vi.waitFor(() => expect(w.routes.applyRolePanelReaction).toHaveBeenCalledTimes(1));
    await vi.advanceTimersByTimeAsync(499);
    expect(w.routes.applyRolePanelReaction).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(1);
    await vi.waitFor(() => expect(w.routes.applyRolePanelReaction).toHaveBeenCalledTimes(2));
    expect(w.state.settings[key]).toBeUndefined();
  });

  it('cancels a scheduled retry when the worker quiesces', async () => {
    const w = world();
    await toReady(w);
    w.gateway.emit({ type: 'status', status: 'ready', closeCode: null });
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    const key = `rolePanel.pending.${'43'.repeat(16)}`;
    const action = {
      communityId: toHex(COMMUNITY), channelId: toHex(CHANNEL), messageSeq: '7', groupId: toHex(GROUP), eventSeq: '9',
      emoji: '👍', enabled: true, userId: toHex(USER), deviceId: toHex(DEVICE),
    };
    w.core.settingPut(key, JSON.stringify(action));
    w.routes.applyRolePanelReaction.mockRejectedValue(new DillaHttpError({
      status: 503, code: 'E_INTERNAL', detail: '', retryAfterMs: null, extra: [],
    }));
    const internal = w.controller as unknown as {
      retryPendingRolePanelActions(core: CorePort, me: { userId: Uint8Array; deviceId: Uint8Array }): void;
      quiesce(): void;
    };

    internal.retryPendingRolePanelActions(w.core as unknown as CorePort, { userId: USER, deviceId: DEVICE });
    await vi.waitFor(() => expect(w.routes.applyRolePanelReaction).toHaveBeenCalledTimes(1));
    internal.quiesce();
    await vi.advanceTimersByTimeAsync(5_000);
    expect(w.routes.applyRolePanelReaction).toHaveBeenCalledTimes(1);
    expect(w.state.settings[key]).toBeDefined();
  });

  it('caps online retry attempts and preserves the journal after exhaustion', async () => {
    const w = world();
    await toReady(w);
    w.gateway.emit({ type: 'status', status: 'ready', closeCode: null });
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    const key = `rolePanel.pending.${'44'.repeat(16)}`;
    const action = {
      communityId: toHex(COMMUNITY), channelId: toHex(CHANNEL), messageSeq: '7', groupId: toHex(GROUP), eventSeq: '9',
      emoji: '👍', enabled: true, userId: toHex(USER), deviceId: toHex(DEVICE),
    };
    w.core.settingPut(key, JSON.stringify(action));
    w.routes.applyRolePanelReaction.mockRejectedValue(new DillaHttpError({
      status: 503, code: 'E_INTERNAL', detail: '', retryAfterMs: null, extra: [],
    }));
    const retry = w.controller as unknown as { retryPendingRolePanelActions(core: CorePort, me: { userId: Uint8Array; deviceId: Uint8Array }): void };
    retry.retryPendingRolePanelActions(w.core as unknown as CorePort, { userId: USER, deviceId: DEVICE });
    await vi.waitFor(() => expect(w.routes.applyRolePanelReaction).toHaveBeenCalledTimes(1));
    for (const [index, delay] of [500, 1_000, 2_000, 4_000, 8_000].entries()) {
      await vi.advanceTimersByTimeAsync(delay);
      await vi.waitFor(() => expect(w.routes.applyRolePanelReaction).toHaveBeenCalledTimes(index + 2));
    }
    await vi.advanceTimersByTimeAsync(60_000);
    expect(w.routes.applyRolePanelReaction).toHaveBeenCalledTimes(6);
    expect(w.state.settings[key]).toBeDefined();
  });
});

describe('threads (web3.7 TB2)', () => {
  const CH = toHex(CHANNEL);
  const ROOT_ID = new Uint8Array(16).fill(0x31);
  const ROOT_HEX = toHex(ROOT_ID);
  const rootRow = (): TimelineRow => ({ ...strangerRow(1), thread: { count: 1, lastTs: 1_700_000_005n } });
  const replyRow = (seq: number): TimelineRow => ({ ...strangerRow(seq), body: `reply ${seq}` });
  const thread = (w: World) => w.last<ThreadState | null>(`thread:${CH}`);

  async function threadWorld(): Promise<World> {
    const w = world();
    await toReady(w);
    await openGeneral(w);
    w.state.rows = [rootRow()];
    w.state.threadRows.set(ROOT_HEX, { root: rootRow(), rows: [replyRow(3), replyRow(2)] });   // newest first, as the core reads them
    return w;
  }

  it('openThread publishes the root and its replies, newest last, before the ret; closeThread publishes null', async () => {
    const w = await threadWorld();
    w.call(10, { m: 'openThread', channelId: CH, rootMsgId: ROOT_HEX });
    await vi.waitFor(() => expect(w.ret(10)).toEqual({ t: 'ret', id: 10, ok: true, value: null }));
    const at = w.posted.findIndex((m) => m.t === 'slice' && m.name === `thread:${CH}`);
    expect(at).toBeGreaterThan(-1);
    expect(at).toBeLessThan(w.posted.findIndex((m) => m.t === 'ret' && m.id === 10));
    const state = thread(w);
    expect(state).toMatchObject({ channelId: CH, rootMsgId: ROOT_HEX, hasEarlier: false });
    expect(state?.root).toMatchObject({ msgId: ROOT_HEX, seq: '1', thread: { count: 1, lastTs: 1_700_000_005 } });
    expect(state?.items.map((i) => i.body)).toEqual(['reply 2', 'reply 3']);
    w.call(11, { m: 'closeThread', channelId: CH });
    await vi.waitFor(() => expect(w.ret(11)).toEqual({ t: 'ret', id: 11, ok: true, value: null }));
    expect(thread(w)).toBeNull();
  });

  it('a root that is not held publishes root null with the replies', async () => {
    const w = await threadWorld();
    w.state.threadRows.set(ROOT_HEX, { root: null, rows: [replyRow(2)] });
    w.call(10, { m: 'openThread', channelId: CH, rootMsgId: ROOT_HEX });
    await vi.waitFor(() => expect(w.ret(10)).toMatchObject({ ok: true }));
    expect(thread(w)?.root).toBeNull();
    expect(thread(w)?.items).toHaveLength(1);
  });

  it('a reply sent with threadId reaches the core with it, leaves the main timeline and shows in the open thread', async () => {
    const w = await threadWorld();
    w.state.threadRows.set(ROOT_HEX, { root: rootRow(), rows: [] });
    w.state.rows = [{ ...rootRow(), thread: null }];
    w.call(10, { m: 'openThread', channelId: CH, rootMsgId: ROOT_HEX });
    await vi.waitFor(() => expect(w.ret(10)).toMatchObject({ ok: true }));
    expect(thread(w)?.items).toEqual([]);
    const pending: OutboxRow = { msgId: new Uint8Array(16).fill(0x77), state: 0, error: '', created: 1_700_000_001n, body: 'in thread', type: 0, replyTo: null, attachments: [] };
    w.sync.sendRequest.mockImplementationOnce(() => { w.state.outbox = [pending]; return pending.msgId; });
    w.call(11, { m: 'send', channelId: CH, text: 'in thread', threadId: ROOT_HEX });
    await vi.waitFor(() => expect(w.ret(11)).toEqual({ t: 'ret', id: 11, ok: true, value: { msgId: '77'.repeat(16) } }));
    expect(w.sync.sendRequest).toHaveBeenCalledWith(GROUP, expect.objectContaining({ type: 0, threadId: ROOT_ID }));
    expect(w.timeline()?.items.map((i) => i.body)).toEqual(['from a stranger 1']);
    expect(thread(w)?.items.map((i) => [i.body, i.state])).toEqual([['in thread', 'pending']]);
    // The core confirms it: the root's count is 1 after the refresh, the reply is in the thread only.
    w.state.outbox = [];
    w.state.threadRows.set(ROOT_HEX, { root: rootRow(), rows: [{ ...replyRow(2), body: 'in thread' }] });
    w.state.rows = [rootRow()];
    w.sync.deps!.onGroupChanged(GROUP, appliedSeqs([2n]));
    expect(w.timeline()?.items.map((i) => [i.body, i.thread])).toEqual([['from a stranger 1', { count: 1, lastTs: 1_700_000_005 }]]);
    expect(thread(w)?.items.map((i) => i.body)).toEqual(['in thread']);
  });

  it('a send without threadId reaches the core with a null one', async () => {
    const w = await threadWorld();
    w.call(10, { m: 'send', channelId: CH, text: 'plain' });
    await vi.waitFor(() => expect(w.ret(10)).toMatchObject({ ok: true }));
    expect(w.sync.sendRequest).toHaveBeenCalledWith(GROUP, expect.objectContaining({ threadId: null }));
  });

  it("a peer's thread reply refreshes both the timeline and the open thread", async () => {
    const w = await threadWorld();
    w.call(10, { m: 'openThread', channelId: CH, rootMsgId: ROOT_HEX });
    await vi.waitFor(() => expect(w.ret(10)).toMatchObject({ ok: true }));
    expect(thread(w)?.items).toHaveLength(2);
    w.state.rows = [{ ...rootRow(), thread: { count: 3, lastTs: 1_700_000_009n } }];
    w.state.threadRows.set(ROOT_HEX, { root: rootRow(), rows: [replyRow(4), replyRow(3), replyRow(2)] });
    w.sync.deps!.onGroupChanged(GROUP, appliedSeqs([4n]));
    expect(w.timeline()?.items[0]?.thread).toEqual({ count: 3, lastTs: 1_700_000_009 });
    expect(thread(w)?.items.map((i) => i.body)).toEqual(['reply 2', 'reply 3', 'reply 4']);
  });

  it('closeChannel closes its thread', async () => {
    const w = await threadWorld();
    w.call(10, { m: 'openThread', channelId: CH, rootMsgId: ROOT_HEX });
    await vi.waitFor(() => expect(w.ret(10)).toMatchObject({ ok: true }));
    w.call(11, { m: 'closeChannel', channelId: CH });
    await vi.waitFor(() => expect(w.ret(11)).toMatchObject({ ok: true }));
    expect(thread(w)).toBeNull();
  });

  it('loadThreads publishes the summaries, with an unheld excerpt empty', async () => {
    const w = await threadWorld();
    const rows: ThreadRow[] = [
      { rootMsgId: ROOT_ID, rootSeq: 1n, count: 2, lastTs: 1_700_000_005n, excerpt: 'from a stranger 1' },
      { rootMsgId: new Uint8Array(16).fill(0x55), rootSeq: null, count: 1, lastTs: 1_700_000_001n, excerpt: '' },
    ];
    w.state.threadList = rows;
    w.call(10, { m: 'loadThreads', channelId: CH });
    await vi.waitFor(() => expect(w.ret(10)).toEqual({ t: 'ret', id: 10, ok: true, value: null }));
    expect(w.last<ThreadSummary[]>(`threads:${CH}`)).toEqual([
      { rootMsgId: ROOT_HEX, rootSeq: '1', count: 2, lastTs: 1_700_000_005, excerpt: 'from a stranger 1' },
      { rootMsgId: '55'.repeat(16), rootSeq: null, count: 1, lastTs: 1_700_000_001, excerpt: '' },
    ]);
  });

  it('refuses ids that are not 32 lowercase hex, allows threadId with attachments, and refuses an unknown channel', async () => {
    const w = await threadWorld();
    w.call(10, { m: 'openThread', channelId: CH, rootMsgId: 'nope' });
    w.call(11, { m: 'send', channelId: CH, text: 'x', threadId: 'A'.repeat(32) });
    w.call(12, { m: 'send', channelId: CH, text: 'x', threadId: 'abc' });
    w.call(13, { m: 'openThread', channelId: 'e'.repeat(32), rootMsgId: ROOT_HEX });
    w.call(14, { m: 'loadThreads', channelId: 'e'.repeat(32) });
    await vi.waitFor(() => expect(w.ret(14)).toBeDefined());
    for (const id of [10, 11, 12]) expect(w.ret(id)).toMatchObject({ ok: false, error: { code: 'E_BAD_INPUT' } });
    expect(w.ret(13)).toMatchObject({ ok: false, error: { code: 'E_BAD_INPUT' } });
    expect(w.ret(14)).toMatchObject({ ok: false, error: { code: 'E_BAD_INPUT' } });
    w.call(15, { m: 'send', channelId: CH, text: 'x', threadId: ROOT_HEX, attachments: [] });
    await vi.waitFor(() => expect(w.ret(15)).toMatchObject({ ok: true }));
  });

  it('markRead reaches past the newest channel row to a later thread reply', async () => {
    const w = await threadWorld();
    w.state.groups = [{ ...textGroup(2), nextSeq: 6n }];
    w.call(10, { m: 'markRead', channelId: CH });
    await vi.waitFor(() => expect(w.ret(10)).toMatchObject({ ok: true }));
    expect(w.core.markRead).toHaveBeenCalledWith(GROUP, 5n, NOW_S);
  });

  it('a pin and a quote carry the thread root', async () => {
    const w = await threadWorld();
    w.state.pins = [{ targetSeq: 2n, msgId: new Uint8Array(16).fill(0x32), pinnedSeq: 9n, byUser: USER, author: STRANGER, excerpt: 'x', targetTs: 1_700_000_005n, threadRoot: ROOT_ID }];
    w.call(10, { m: 'loadPins', channelId: CH });
    await vi.waitFor(() => expect(w.ret(10)).toMatchObject({ ok: true }));
    expect(w.last<PinnedItem[]>(`pins:${CH}`)?.[0]?.threadRoot).toBe(ROOT_HEX);
    w.state.rows = [{ ...strangerRow(2), reply: { replyTo: ROOT_ID, targetSeq: 2n, targetUser: STRANGER, excerpt: 'q', state: 0, threadRoot: ROOT_ID } }];
    w.sync.deps!.onGroupChanged(GROUP, appliedSeqs([2n]));
    expect(w.timeline()?.items[0]?.reply?.threadRoot).toBe(ROOT_HEX);
  });

  // FX-core (final review I1): since TB1 the main timeline leaves out a reply whose root is held, so the notice read finds
  // such a reply in its thread.
  const mentionReply = (seq: number): TimelineRow => ({ ...replyRow(seq), body: `<@${toHex(USER)}> look`, mention: true });
  const listed = (lastTs: bigint): ThreadRow[] => [{ rootMsgId: ROOT_ID, rootSeq: 1n, count: 3, lastTs, excerpt: 'from a stranger 1' }];

  it('a thread reply that mentions me raises a mention notice that names its thread', async () => {
    const w = await threadWorld();
    w.state.rows = [{ ...rootRow(), thread: { count: 3, lastTs: 1_700_000_009n } }];
    w.state.threadRows.set(ROOT_HEX, { root: rootRow(), rows: [mentionReply(4), replyRow(3), replyRow(2)] });
    w.state.threadList = listed(1_700_000_009n);
    w.state.activity = [act(GROUP, 1, 1)];
    w.sync.deps!.onGroupChanged(GROUP, appliedSeqs([4n]));
    expect(w.notices()?.items).toEqual([{
      id: 1, channelId: CH, communityId: toHex(COMMUNITY), kind: 'mention', senderUser: toHex(STRANGER),
      senderName: '5a5a5a5a', body: `<@${toHex(USER)}> look`, ts: 1_700_000_000, threadRoot: ROOT_HEX,
    }]);
  });

  it('a thread reply and a channel post in one apply both raise a notice; the post names no thread', async () => {
    const w = await threadWorld();
    w.state.rows = [strangerRow(5), { ...rootRow(), thread: { count: 3, lastTs: 1_700_000_009n } }];
    w.state.threadRows.set(ROOT_HEX, { root: rootRow(), rows: [replyRow(4), replyRow(3), replyRow(2)] });
    w.state.threadList = listed(1_700_000_009n);
    w.state.activity = [act(GROUP, 2, 0)];
    w.sync.deps!.onGroupChanged(GROUP, appliedSeqs([4n, 5n]));
    expect(w.notices()?.items.map((n) => [n.body, n.threadRoot])).toEqual([['reply 4', ROOT_HEX], ['from a stranger 5', null]]);
  });

  async function withThreadsOpen(): Promise<World> {
    const w = await threadWorld();
    w.state.threadList = [{ rootMsgId: ROOT_ID, rootSeq: 1n, count: 2, lastTs: 1_700_000_005n, excerpt: 'from a stranger 1' }];
    w.call(20, { m: 'openThread', channelId: CH, rootMsgId: ROOT_HEX });
    w.call(21, { m: 'loadThreads', channelId: CH });
    await vi.waitFor(() => expect(w.ret(21)).toMatchObject({ ok: true }));
    expect(thread(w)).not.toBeNull();
    expect(w.last<ThreadSummary[]>(`threads:${CH}`)).toHaveLength(1);
    return w;
  }

  it('a pending thread reply never shows in the main timeline, even for the publish the outbox change makes', async () => {
    const w = await threadWorld();
    w.state.threadRows.set(ROOT_HEX, { root: rootRow(), rows: [] });
    w.state.rows = [{ ...rootRow(), thread: null }];
    w.call(10, { m: 'openThread', channelId: CH, rootMsgId: ROOT_HEX });
    await vi.waitFor(() => expect(w.ret(10)).toMatchObject({ ok: true }));
    const pending: OutboxRow = { msgId: new Uint8Array(16).fill(0x77), state: 0, error: '', created: 1_700_000_001n, body: 'in thread', type: 0, replyTo: null, attachments: [] };
    // Like the real sendRequest: the outbox row exists and onOutboxChanged runs before it returns.
    w.sync.sendRequest.mockImplementationOnce(() => { w.state.outbox = [pending]; w.sync.deps!.onOutboxChanged(GROUP); return pending.msgId; });
    w.call(11, { m: 'send', channelId: CH, text: 'in thread', threadId: ROOT_HEX });
    await vi.waitFor(() => expect(w.ret(11)).toMatchObject({ ok: true }));
    const timelines = w.posted.filter((m): m is SliceMessage => m.t === 'slice' && m.name === `timeline:${CH}`);
    for (const m of timelines) expect((m.value as TimelineState).items.map((i) => i.body)).not.toContain('in thread');
    const threads = w.posted.filter((m): m is SliceMessage => m.t === 'slice' && m.name === `thread:${CH}`);
    expect((threads.at(-1)?.value as ThreadState).items.map((i) => i.body)).toEqual(['in thread']);
  });

  it('a plain send beside a thread reply is not taken for a thread reply', async () => {
    const w = await threadWorld();
    const plain: OutboxRow = { msgId: new Uint8Array(16).fill(0x66), state: 0, error: '', created: 1_700_000_001n, body: 'plain', type: 0, replyTo: null, attachments: [] };
    w.sync.sendRequest.mockImplementationOnce(() => { w.state.outbox = [plain]; w.sync.deps!.onOutboxChanged(GROUP); return plain.msgId; });
    w.call(11, { m: 'send', channelId: CH, text: 'plain' });
    await vi.waitFor(() => expect(w.ret(11)).toMatchObject({ ok: true }));
    expect(w.timeline()?.items.map((i) => i.body)).toContain('plain');
  });

  it('the threads list is read again after a frame (a new root gets its first reply)', async () => {
    const w = await withThreadsOpen();
    w.state.threadList = [
      { rootMsgId: new Uint8Array(16).fill(0x32), rootSeq: 2n, count: 1, lastTs: 1_700_000_009n, excerpt: 'new root' },
      ...w.state.threadList,
    ];
    w.sync.deps!.onGroupChanged(GROUP, appliedSeqs([4n]));
    expect(w.last<ThreadSummary[]>(`threads:${CH}`)?.map((t) => t.excerpt)).toEqual(['new root', 'from a stranger 1']);
  });

  it('sign-out clears the thread slices', async () => {
    const w = await withThreadsOpen();
    w.call(30, { m: 'signOutRevoke', recoveryKey: RECOVERY_KEY });
    await vi.waitFor(() => expect(w.ret(30)).toMatchObject({ ok: true }));
    expect(thread(w)).toBeNull();
    expect(w.last(`threads:${CH}`)).toEqual([]);
  });

  it('removing the channel closes its thread and clears both slices; reopening does not resurrect it', async () => {
    const w = await withThreadsOpen();
    w.call(30, { m: 'deleteChannel', channelId: CH });
    await vi.waitFor(() => expect(w.ret(30)).toMatchObject({ ok: true }));
    expect(thread(w)).toBeNull();
    expect(w.last(`threads:${CH}`)).toEqual([]);
    w.sync.deps!.onGroupChanged(GROUP, appliedSeqs([4n]));
    expect(thread(w)).toBeNull();
  });

  it('leaving the community closes the threads of its channels', async () => {
    const w = await withThreadsOpen();
    w.call(30, { m: 'leaveCommunity', communityId: toHex(COMMUNITY) });
    await vi.waitFor(() => expect(w.ret(30)).toMatchObject({ ok: true }));
    expect(thread(w)).toBeNull();
    expect(w.last(`threads:${CH}`)).toEqual([]);
  });

  it('closeChannel clears the threads list too', async () => {
    const w = await withThreadsOpen();
    w.call(30, { m: 'closeChannel', channelId: CH });
    await vi.waitFor(() => expect(w.ret(30)).toMatchObject({ ok: true }));
    expect(w.last(`threads:${CH}`)).toEqual([]);
  });

  it('openThread and loadThreads refuse a channel that is not open', async () => {
    const w = world();
    await toReady(w);
    w.state.groups = [textGroup(2)];
    w.call(10, { m: 'selectCommunity', communityId: toHex(COMMUNITY) });
    await vi.waitFor(() => expect(w.ret(10)).toMatchObject({ ok: true }));
    w.call(11, { m: 'openThread', channelId: CH, rootMsgId: ROOT_HEX });
    w.call(12, { m: 'loadThreads', channelId: CH });
    await vi.waitFor(() => expect(w.ret(12)).toBeDefined());
    expect(w.ret(11)).toMatchObject({ ok: false, error: { code: 'E_NOT_READY' } });
    expect(w.ret(12)).toMatchObject({ ok: false, error: { code: 'E_NOT_READY' } });
  });

  it('a quote of a thread reply sent from the open thread names the thread', async () => {
    const w = await threadWorld();
    w.state.threadRows.set(ROOT_HEX, { root: rootRow(), rows: [replyRow(2)] });
    const pending: OutboxRow = { msgId: new Uint8Array(16).fill(0x77), state: 0, error: '', created: 1_700_000_001n, body: 'q', type: 0, replyTo: replyRow(2).msgId, attachments: [] };
    w.sync.sendRequest.mockImplementationOnce(() => { w.state.outbox = [pending]; w.sync.deps!.onOutboxChanged(GROUP); return pending.msgId; });
    w.call(10, { m: 'openThread', channelId: CH, rootMsgId: ROOT_HEX });
    await vi.waitFor(() => expect(w.ret(10)).toMatchObject({ ok: true }));
    w.call(11, { m: 'send', channelId: CH, text: 'q', threadId: ROOT_HEX, replyTo: toHex(replyRow(2).msgId as Uint8Array) });
    await vi.waitFor(() => expect(w.ret(11)).toMatchObject({ ok: true }));
    expect(thread(w)?.items.at(-1)?.reply?.threadRoot).toBe(ROOT_HEX);
  });
});

it('privacy refresh replaces a cached contact username with their current display name', async () => {
  const w = world();
  let display = '';
  const original = w.routes.listMembers.getMockImplementation()!;
  w.routes.listMembers.mockImplementation(async community => [...await original(community), { userId: STRANGER, username: 'mira-user', display, kind: 0, nick: '', roleIds: [] }]);
  w.core.trustState.mockReturnValue([{ userId: STRANGER, umk: new Uint8Array(32).fill(7), firstSeen: 1n, verified: false, changedUmk: null, blocked: null }]);
  await toReady(w);
  await vi.waitFor(() => expect(w.last<TrustSummary[]>('trust')?.find(row => row.userId === toHex(STRANGER))?.name).toBe('mira-user'));
  display = 'Mira';
  w.call(99_881, { m: 'refreshTrust' });
  await vi.waitFor(() => expect(w.ret(99_881)).toMatchObject({ ok: true }));
  expect(w.last<TrustSummary[]>('trust')?.find(row => row.userId === toHex(STRANGER))?.name).toBe('Mira');
});

it('sends input activity only over WSS and drops unavailable samples with no HTTP', async () => {
  let now = 100_000;
  const w = world({ now: () => now }); await toReady(w);
  w.gateway.sendPresenceActivity.mockReturnValue(true);
  w.call(91, { m: 'presenceActivity' }); await settle();
  w.call(92, { m: 'presenceActivity' }); await settle();
  expect(w.routes.sendPresenceActivity).not.toHaveBeenCalled();
  expect(w.gateway.sendPresenceActivity).toHaveBeenCalledTimes(1);
  // Fresh input during an outage is dropped rather than rerouted through HTTP.
  w.gateway.sendPresenceActivity.mockReturnValue(false);
  now += 5000;
  w.call(93, { m: 'presenceActivity' }); await settle();
  expect(w.routes.sendPresenceActivity).not.toHaveBeenCalled();
});


describe('Desktop OIDC assertion adoption', () => {
 it.each([0, 1, 2] as const)('keeps factor debt %i and defers registration until the key step', async needs => {
  const w = world({ phase: 0 });
  await toLoginStep(w);
  w.call(3, { m: 'signInOIDC', assertion: 'A'.repeat(43), needs });
  await vi.waitFor(() => expect(w.ret(3)).toMatchObject({ ok: true, value: { needs } }));
  expect(w.account()?.phase).toBe(needs === 1 ? 'signin-totp' : needs === 2 ? 'signin-passkey' : 'signin-key');
  expect(w.enrol.oidc).toHaveBeenCalledWith('A'.repeat(43), needs);
  expect(w.enrol.register).not.toHaveBeenCalled();
  expect(w.core.recoveryKeyCheck).not.toHaveBeenCalled();
 });
 it('rejects malformed OIDC input before touching enrollment', async () => {
  const w = world({ phase: 0 }); await toLoginStep(w);
  w.call(3, { m: 'signInOIDC', assertion: 'https://evil.test', needs: 0 });
  await vi.waitFor(() => expect(w.ret(3)).toMatchObject({ ok: false, error: { code: 'E_BAD_INPUT' } }));
  expect(w.enrol.oidc).not.toHaveBeenCalled();
 });
});
