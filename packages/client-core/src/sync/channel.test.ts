import { beforeEach, describe, expect, it } from 'vitest';
import type { Id } from '../core-port';
import { toHex } from '../hex';
import { SYNC, SyncError } from './engine';
import { coreCalls, device, readyInfo, type Device } from './testing/harness';
import { CHANNEL, COMMUNITY, ME, ManualClock, ModelDs, PEER, at, deferred, httpError, idOf, settle, type RouteName } from './testing/model';

let ds: ModelDs;
let clock: ManualClock;
let d: Device;

beforeEach(() => {
  ds = new ModelDs();
  clock = new ManualClock();
  ds.addChannel(COMMUNITY, CHANNEL);
  d = device(ds, clock, ME);
});

const open = (textGroupId: Id | null): Promise<{ groupId: Id; state: 0 | 1 | 2 | 3 | 4 }> =>
  d.engine.openChannel({ communityId: COMMUNITY, channelId: CHANNEL, textGroupId });
const count = (route: RouteName): number => ds.callsOf(ME.device, route).length;

describe('opening a channel (rule 6)', () => {
  it('registers a group for a channel that has none', async () => {
    const r = await open(null);
    expect(r.state).toBe(2);
    expect(ds.view(r.groupId).members).toEqual([toHex(ME.device)]);
    const rows = await ds.routesFor(PEER.device).listChannels(COMMUNITY);
    expect(rows.find((c) => toHex(c.id) === toHex(CHANNEL))?.textGroupId).toEqual(r.groupId);
    expect(d.core.group(r.groupId)).toMatchObject({ state: 2, epoch: 0n, nextSeq: 1n });
    expect(at(d.changes, d.changes.length - 1).result.state).toBe(2);
    expect(count('getWelcomes')).toBe(0);
    expect(count('postResync')).toBe(0);
  });

  it('loses a registration race, discards its own group and joins the winner', async () => {
    const winners: Id[] = [];
    ds.inject('postGroup', {
      before: () => {
        winners.push(ds.peerCreate(PEER, CHANNEL));
      },
    });
    const r = await open(null);
    const winner = at(winners, 0);
    expect(r).toEqual({ groupId: winner, state: 2 });
    expect(d.core.groups().map((x) => toHex(x.groupId))).toEqual([toHex(winner)]);
    expect(count('postGroup')).toBe(1);
    expect(count('listChannels')).toBe(1);
    expect(count('postResync')).toBe(1);
    expect(ds.view(winner).members).toContain(toHex(ME.device));
  });

  it('gives up with E_REGISTER_RACE after registerRetryMax registrations', async () => {
    for (let i = 0; i < SYNC.registerRetryMax + 1; i++) ds.inject('postGroup', { fail: httpError(409, 'E_GROUP_EXISTS') });
    const err: unknown = await open(null).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(SyncError);
    expect(err).toMatchObject({ code: 'E_REGISTER_RACE' });
    expect(count('postGroup')).toBe(SYNC.registerRetryMax);
    expect(count('listChannels')).toBe(SYNC.registerRetryMax);
    expect(d.core.groups()).toEqual([]);
  });

  it('propagates a refused registration (400 E_INVALID_REQUEST) once and leaves no group', async () => {
    // The answer to a registration that is not this device's one leaf, or before its device list is published (hardening G).
    ds.inject('postGroup', { fail: httpError(400, 'E_INVALID_REQUEST') });
    const err: unknown = await open(null).catch((e: unknown) => e);
    expect(err).toMatchObject({ status: 400, code: 'E_INVALID_REQUEST' });
    expect(count('postGroup')).toBe(1);
    expect(count('listChannels')).toBe(0);
    expect(d.core.groups()).toEqual([]);
    await clock.advance(60_000);
    expect(count('postGroup')).toBe(1);
  });

  it('adopts a waiting Welcome instead of joining by external commit', async () => {
    const pg = ds.peerCreate(PEER, CHANNEL);
    ds.detach(ME.device);
    ds.peerCommit(pg, PEER, [ME.device]); // seq 1, a Welcome for ME
    ds.peerSend(pg, PEER, 'after welcome'); // seq 2
    ds.reattach(ME.device);
    const r = await open(pg);
    expect(r).toEqual({ groupId: pg, state: 2 });
    await settle();
    expect(count('postResync')).toBe(0);
    expect(count('deleteWelcome')).toBe(1);
    expect(ds.welcomesFor(ME.device)).toEqual([]);
    expect(d.core.bodies(pg)).toEqual(['after welcome']);
  });

  it('joins by external commit, applies the frames that arrived during the join, then catches up', async () => {
    const pg = ds.peerCreate(PEER, CHANNEL);
    ds.peerSend(pg, PEER, 'before'); // seq 1, before this device was a member
    ds.inject('postResync', {
      after: () => {
        ds.peerSend(pg, PEER, 'during'); // seq 3, fanned out before the 200 is read
      },
    });
    const r = await open(pg);
    expect(r).toEqual({ groupId: pg, state: 2 });
    await settle();
    expect(count('getWelcomes')).toBe(1);
    expect(count('postResync')).toBe(1);
    expect(at(d.core.applyCalls, 0)).toEqual({ g: toHex(pg), through: 3n, handshakes: 0, messages: 1 });
    expect(ds.callsOf(ME.device, 'getMessages').map((c) => c.from)).toEqual([4n]);
    expect(d.core.bodies(pg)).toEqual(['during']);
    expect(d.core.group(pg)).toMatchObject({ state: 2, epoch: 1n, nextSeq: 4n });
  });

  it('propagates a refused external join and leaves no local group', async () => {
    const pg = ds.peerCreate(PEER, CHANNEL);
    ds.denyJoin.add(toHex(ME.device));
    const err: unknown = await open(pg).catch((e: unknown) => e);
    expect(err).toMatchObject({ status: 403, code: 'E_FORBIDDEN' });
    expect(d.core.groups()).toEqual([]);
    expect(d.membership).toEqual([]);
  });

  it('keeps an accepted external join when stopped during postResync', async () => {
    const pg = ds.peerCreate(PEER, CHANNEL);
    const gate = deferred();
    ds.inject('postResync', { gate: gate.promise });
    const opening = open(pg).catch((e: unknown) => e);
    await settle();
    expect(count('postResync')).toBe(1);
    d.engine.stop();
    gate.resolve();
    await opening;
    expect(coreCalls(d, pg, ['groupDiscard'])).toEqual([]);
    expect(d.core.group(pg)?.state).toBe(2);
    expect(ds.view(pg).members).toContain(toHex(ME.device));
  });

  it('keeps an accepted registration when stopped during postGroup', async () => {
    const gate = deferred();
    ds.inject('postGroup', { gate: gate.promise });
    const opening = open(null).catch((e: unknown) => e);
    await settle();
    expect(count('postGroup')).toBe(1);
    d.engine.stop();
    gate.resolve();
    const result = await opening;
    expect(d.core.calls.filter((c) => c.m === 'groupDiscard')).toEqual([]);
    expect(result).toMatchObject({ state: 2 });
    expect(d.core.groups()).toMatchObject([{ state: 2 }]);
  });

  it('waits and starts again from the Welcome step when the join meets the 425 freeze', async () => {
    const pg = ds.peerCreate(PEER, CHANNEL);
    ds.inject('postResync', { fail: httpError(425, 'E_COMMIT_REQUIRED', 2000) });
    const opening = open(pg);
    await settle();
    expect(count('postResync')).toBe(1);
    expect(d.core.groups()).toEqual([]);
    ds.detach(ME.device);
    ds.peerCommit(pg, PEER, [ME.device]); // a member commits the Add: the Welcome waits on the server
    ds.reattach(ME.device);
    await clock.advance(SYNC.membershipWaitMs);
    expect(await opening).toEqual({ groupId: pg, state: 2 });
    expect(count('postResync')).toBe(1);
    expect(count('getWelcomes')).toBe(2);
    expect(ds.welcomesFor(ME.device)).toEqual([]);
  });

  it('rejects E_SYNC_STOPPED when stopped during the 425 wait (CLIENT-CORE-TS-05)', async () => {
    const pg = ds.peerCreate(PEER, CHANNEL);
    ds.inject('postResync', { fail: httpError(425, 'E_COMMIT_REQUIRED', 2000) });
    const opening = open(pg).then((ok) => ({ ok }), (err: unknown) => ({ err }));
    await settle();
    expect(count('postResync')).toBe(1);
    d.engine.stop();
    await settle();
    expect(await opening).toEqual({ err: new SyncError('E_SYNC_STOPPED') });
    expect(count('postResync')).toBe(1);
  });

  it('starts the join again at once when it raced a commit (409 E_COMMIT_CONFLICT)', async () => {
    const pg = ds.peerCreate(PEER, CHANNEL);
    ds.inject('postResync', { fail: httpError(409, 'E_COMMIT_CONFLICT') });
    expect(await open(pg)).toEqual({ groupId: pg, state: 2 });
    expect(count('getWelcomes')).toBe(2);
    expect(count('getGroupInfo')).toBe(2);
    expect(count('getGroupTree')).toBe(2);
    expect(count('postResync')).toBe(2);
    expect(coreCalls(d, pg, ['groupDiscard'])).toEqual(['groupDiscard']);
    expect(ds.view(pg).members).toContain(toHex(ME.device));
  });

  it('re-reads info and tree when a commit lands between the two join reads', async () => {
    const pg = ds.peerCreate(PEER, CHANNEL);
    ds.inject('getGroupTree', { before: () => { ds.peerCommit(pg, PEER); } });
    expect(await open(pg)).toEqual({ groupId: pg, state: 2 });
    expect(count('getGroupInfo')).toBe(2);
    expect(count('getGroupTree')).toBe(2);
    expect(count('postResync')).toBe(1);
    expect(d.core.group(pg)).toMatchObject({ state: 2, epoch: 2n });
  });

  it('gives up after commitRetryMax joins that each raced a commit', async () => {
    const pg = ds.peerCreate(PEER, CHANNEL);
    for (let i = 0; i < SYNC.commitRetryMax + 1; i++) ds.inject('postResync', { fail: httpError(409, 'E_COMMIT_CONFLICT') });
    const err: unknown = await open(pg).catch((e: unknown) => e);
    expect(err).toMatchObject({ status: 409, code: 'E_COMMIT_CONFLICT' });
    expect(count('postResync')).toBe(SYNC.commitRetryMax);
    expect(coreCalls(d, pg, ['groupDiscard'])).toHaveLength(SYNC.commitRetryMax);
    expect(d.core.groups()).toEqual([]);
  });

  it('a Welcome is kept while the join is in flight', async () => {
    const pg = ds.peerCreate(PEER, CHANNEL);
    const gate = deferred();
    ds.inject('postResync', { gate: gate.promise });
    const opening = open(pg).catch((e: unknown) => e);
    await settle();
    expect(count('postResync')).toBe(1);
    expect(d.core.group(pg)?.state).toBe(1);
    ds.peerCommit(pg, PEER, [ME.device]); // seq 1, epoch 0 -> 1: a Welcome for ME, fanned out as op 20
    await settle();
    expect(d.gateway.frames.filter((f) => f.op === 20)).toHaveLength(1);
    expect(count('deleteWelcome')).toBe(0);
    expect(ds.welcomesFor(ME.device)).toHaveLength(1);
    ds.denyJoin.add(toHex(ME.device));
    gate.resolve();
    expect(await opening).toMatchObject({ status: 403, code: 'E_FORBIDDEN' });
    expect(coreCalls(d, pg, ['groupDiscard'])).toEqual(['groupDiscard']);
    expect(d.core.group(pg)).toBeUndefined();
    d.gateway.ready(readyInfo(ME));
    await settle();
    expect(d.core.group(pg)).toMatchObject({ state: 2, epoch: 1n, nextSeq: 2n });
    expect(count('deleteWelcome')).toBe(1);
    expect(ds.welcomesFor(ME.device)).toEqual([]);
  });

  it('checks removals without repeating group join for an already active group', async () => {
    const first = await open(null);
    await settle();
    ds.resetCalls();
    expect(await open(first.groupId)).toEqual({ groupId: first.groupId, state: 2 });
    expect(ds.calls.map(call => call.route)).toEqual(['getModerationRemovals']);
  });

  it('registers once when the same channel is opened twice at the same time', async () => {
    const [a, b] = await Promise.all([open(null), open(null)]);
    expect(toHex(a.groupId)).toBe(toHex(b.groupId));
    expect(count('postGroup')).toBe(1);
  });

  it('discards a registration an earlier page never finished, then registers', async () => {
    const stale = idOf(0x77, 1);
    d.core.groupCreate(stale, COMMUNITY, CHANNEL); // ModelCore ignores policy_version and the sender key
    const r = await open(null);
    expect(r.state).toBe(2);
    expect(toHex(r.groupId)).not.toBe(toHex(stale));
    expect(d.core.groups().map((x) => toHex(x.groupId))).toEqual([toHex(r.groupId)]);
  });

  it('resyncs a group in state 3 on open, answering state 3 while refused and 2 once admitted', async () => {
    const r = await open(null);
    await settle();
    ds.join(r.groupId, PEER.device);
    ds.denyJoin.add(toHex(ME.device));
    ds.garbageCommit(r.groupId);
    await settle();
    expect(d.core.group(r.groupId)?.state).toBe(3);
    expect(await open(r.groupId)).toEqual({ groupId: r.groupId, state: 3 });
    expect(d.membership.map((m) => m.status)).toEqual(['resyncing', 'not-member', 'resyncing', 'not-member']);
    ds.denyJoin.clear();
    expect(await open(r.groupId)).toEqual({ groupId: r.groupId, state: 2 });
    d.gateway.ready(readyInfo(ME));
    await settle();
    expect(d.core.group(r.groupId)?.state).toBe(2);
  });

  /** An open whose outcome is read after the gates open, so an early rejection is never unhandled. */
  const outcome = (textGroupId: Id): Promise<{ ok: unknown } | { err: unknown }> =>
    open(textGroupId).then((ok) => ({ ok }), (err: unknown) => ({ err }));

  it('answers state 2 when opened while the group is joining by a background resync (CLIENT-CORE-TS-02)', async () => {
    const r = await open(null);
    await settle();
    ds.join(r.groupId, PEER.device);
    const gate = deferred();
    ds.inject('postResync', { gate: gate.promise });
    ds.garbageCommit(r.groupId);
    await settle();
    expect(d.core.group(r.groupId)?.state).toBe(1);
    const opened = outcome(r.groupId);
    await settle();
    gate.resolve();
    expect(await opened).toEqual({ ok: { groupId: r.groupId, state: 2 } });
    expect(d.core.group(r.groupId)?.state).toBe(2);
    expect(count('postResync')).toBe(1);
    expect(coreCalls(d, r.groupId, ['groupDiscard'])).toEqual([]);
    expect(d.membership.map((m) => m.status)).toEqual(['resyncing']);
  });

  it('answers state 3 when the background resync it waited for is refused (CLIENT-CORE-TS-02)', async () => {
    const r = await open(null);
    await settle();
    ds.join(r.groupId, PEER.device);
    ds.denyJoin.add(toHex(ME.device));
    const gate = deferred();
    ds.inject('postResync', { gate: gate.promise });
    ds.garbageCommit(r.groupId);
    await settle();
    expect(d.core.group(r.groupId)?.state).toBe(1);
    const opened = outcome(r.groupId);
    await settle();
    gate.resolve();
    expect(await opened).toEqual({ ok: { groupId: r.groupId, state: 3 } });
    // The background join, then the open's own resync of the state-3 row (rule 7), both refused.
    expect(count('postResync')).toBe(2);
    expect(d.core.group(r.groupId)?.state).toBe(3);
  });

  it('does not join a second time when a background resync of a state-3 group finishes first (CLIENT-CORE-TS-02)', async () => {
    const r = await open(null);
    await settle();
    ds.join(r.groupId, PEER.device);
    const gate = deferred();
    ds.inject('getGroupInfo', { gate: gate.promise });
    ds.garbageCommit(r.groupId);
    await settle();
    expect(d.core.group(r.groupId)?.state).toBe(3);
    const opened = outcome(r.groupId);
    await settle();
    gate.resolve();
    expect(await opened).toEqual({ ok: { groupId: r.groupId, state: 2 } });
    expect(count('postResync')).toBe(1);
    expect(coreCalls(d, r.groupId, ['groupJoinExternal'])).toEqual(['groupJoinExternal']);
  });
});

  describe('DMs: a null community (L-TS-22 rule 3, Q25)', () => {
    const DM = idOf(0xd3, 1);
    const openDm = (textGroupId: Id | null): Promise<{ groupId: Id; state: 0 | 1 | 2 | 3 | 4 }> =>
      d.engine.openChannel({ communityId: null, channelId: DM, textGroupId });

    beforeEach(() => {
      ds.addDm(DM, [ME, PEER]);
    });

    it('registers the DM group with a null community when the DM has none', async () => {
      const r = await openDm(null);
      expect(r.state).toBe(2);
      expect(d.core.group(r.groupId)).toMatchObject({ state: 2, communityId: null, targetId: DM });
      expect(count('getChannel')).toBe(1);
      expect(count('postGroup')).toBe(1);
      expect(count('listChannels')).toBe(0);
      expect((await ds.routesFor(PEER.device).getChannel(DM)).textGroupId).toEqual(r.groupId);
    });

    it('learns the DM group from getChannel and joins it by external commit when no Welcome waits', async () => {
      const g = ds.peerCreate(PEER, DM);
      expect(await openDm(null)).toEqual({ groupId: g, state: 2 });
      await settle();
      expect([count('getChannel'), count('getWelcomes'), count('postResync'), count('postGroup')]).toEqual([1, 1, 1, 0]);
      expect(d.core.group(g)).toMatchObject({ state: 2, communityId: null, targetId: DM });
    });

    it('joins the DM by its waiting Welcome', async () => {
      const g = ds.peerCreate(PEER, DM);
      ds.detach(ME.device);
      ds.peerCommit(g, PEER, [ME.device]);
      ds.peerSend(g, PEER, 'dm hello');
      ds.reattach(ME.device);
      expect(await openDm(null)).toEqual({ groupId: g, state: 2 });
      await settle();
      expect(count('postResync')).toBe(0);
      expect(count('deleteWelcome')).toBe(1);
      expect(d.core.bodies(g)).toEqual(['dm hello']);
    });

    it('a lost DM registration race is re-read through getChannel, never listChannels', async () => {
      const winners: Id[] = [];
      ds.inject('postGroup', {
        before: () => {
          winners.push(ds.peerCreate(PEER, DM));
        },
      });
      const r = await openDm(null);
      expect(r).toEqual({ groupId: at(winners, 0), state: 2 });
      expect(count('getChannel')).toBe(2);
      expect(count('listChannels')).toBe(0);
      expect(count('postResync')).toBe(1);
    });

    it('a DM the instance does not show is E_CHANNEL_GONE and registers nothing', async () => {
      const err: unknown = await d.engine.openChannel({ communityId: null, channelId: idOf(0xd3, 9), textGroupId: null }).catch((e: unknown) => e);
      expect(err).toBeInstanceOf(SyncError);
      expect(err).toMatchObject({ code: 'E_CHANNEL_GONE' });
      expect(count('postGroup')).toBe(0);
    });

    it('a DM group resyncs with its null community', async () => {
      const r = await openDm(null);
      await settle();
      ds.garbageCommit(r.groupId);
      await settle();
      expect(d.membership).toEqual([{ group: toHex(r.groupId), status: 'resyncing' }]);
      expect(count('postResync')).toBe(1);
      expect(d.core.group(r.groupId)).toMatchObject({ state: 2, communityId: null });
    });
  });

// v0.1.0-web3.1, the brief's check that a new channel needs no client work beyond the create: a text channel created
// through the client path has no group until the first device that opens it registers one (postGroup with the core's
// groupCreate); a member who opens it later joins that group by external commit.
describe('a channel created through the client path', () => {
  it('is registered by its first opener and joined by external commit by the next member', async () => {
    ds.own(ME.user, ME.device);
    ds.own(PEER.user, PEER.device);
    const routes = ds.routesFor(ME.device);
    const server = await routes.createCommunity('Midgard Crew');
    ds.addMember(server.id, PEER.user);
    const { id: channelId } = await routes.createChannel(server.id, { kind: 'text', name: 'general', position: 0 });
    expect((await routes.listChannels(server.id))[0]?.textGroupId).toBeNull();

    const first = await d.engine.openChannel({ communityId: server.id, channelId, textGroupId: null });
    expect(first.state).toBe(2);
    expect(ds.callsOf(ME.device, 'postGroup')).toHaveLength(1);
    const listed = (await ds.routesFor(PEER.device).listChannels(server.id))[0];
    expect(listed?.textGroupId).toEqual(first.groupId);

    const peer = device(ds, clock, PEER);
    const second = await peer.engine.openChannel({ communityId: server.id, channelId, textGroupId: listed?.textGroupId ?? null });
    expect(second).toEqual({ groupId: first.groupId, state: 2 });
    expect(ds.callsOf(PEER.device, 'postGroup')).toHaveLength(0);
    expect(ds.callsOf(PEER.device, 'postResync')).toHaveLength(1);
    expect(ds.view(first.groupId).members.sort()).toEqual([toHex(ME.device), toHex(PEER.device)].sort());
  });
});
