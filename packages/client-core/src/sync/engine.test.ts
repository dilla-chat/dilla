import { beforeEach, describe, expect, it } from 'vitest';
import { decode } from '../cbor';
import { CoreError } from '../core-port';
import { toHex } from '../hex';
import { SYNC, throughOf, type PageInfo } from './engine';
import { catchSync, coreCalls, device, openRegistered, readyInfo, type Device } from './testing/harness';
import { fakeDskPub } from '../testing/fake-list';
import { readDeviceListHistory, signedHeadList } from '../state/trust';
import {
  CHANNEL_2, COMMUNITY, CHANNEL, FOURTH, ME, ManualClock, ModelDs, PEER, THIRD, at, deferred, httpError, idOf, settle, type RouteName,
} from './testing/model';

let ds: ModelDs;
let clock: ManualClock;

beforeEach(() => {
  ds = new ModelDs();
  clock = new ManualClock();
  ds.addChannel(COMMUNITY, CHANNEL);
  ds.addChannel(COMMUNITY, CHANNEL_2);
});

function count(route: RouteName): number {
  return ds.callsOf(ME.device, route).length;
}

function fromArgs(route: RouteName): (bigint | null)[] {
  return ds.callsOf(ME.device, route).map((c) => c.from);
}

function cursorOf(groupId: Uint8Array): readonly [bigint, bigint] | undefined {
  return ds.cursors.get(`${toHex(ME.device)}/${toHex(groupId)}`);
}

const COMMIT_CALLS = ['commitBuild', 'commitAbort', 'commitConfirm', 'groupApply'] as const;

describe('constants and the through bound', () => {
  it('fixes the sync constants', () => {
    expect(SYNC).toEqual({
      handshakePage: 512, messagePage: 256, commitRetryMax: 5, commitJitterMs: 400, membershipWaitMs: 2000, echoWaitMs: 5000,
      registerRetryMax: 3, cursorDebounceMs: 30000, joinAllConcurrency: 1, joinAllRetryMs: 60000,
    });
  });

  it('setChannels is setExpected under its web-1 name, and neither starts anything before a ready', async () => {
    const d = device(ds, clock, ME);
    const w = ds.peerCreate(PEER, CHANNEL_2);
    const eg = [{ groupId: w, communityId: COMMUNITY, channelId: CHANNEL_2, policyVersion: 1n }];
    d.engine.setChannels(eg);
    expect(d.engine.expected()).toEqual(eg);
    d.engine.setExpected([]);
    expect(d.engine.expected()).toEqual([]);
    d.engine.setExpected(eg);
    await settle();
    expect(ds.calls).toEqual([]);
    expect(d.joinAll).toEqual([]);
  });

  // [name, message page (256), handshake page (512), carry, through]
  const cases: [string, PageInfo, PageInfo, bigint | null, bigint][] = [
    ['both short, only messages', { count: 1, lastSeq: 10n }, { count: 0, lastSeq: null }, null, 10n],
    ['both short, only a handshake, first round', { count: 0, lastSeq: null }, { count: 1, lastSeq: 11n }, null, 0n],
    ['both short, only a handshake, carried', { count: 0, lastSeq: null }, { count: 1, lastSeq: 11n }, 11n, 11n],
    ['both short, handshake above the messages', { count: 3, lastSeq: 10n }, { count: 1, lastSeq: 12n }, null, 10n],
    ['both short, carried up to the handshake', { count: 1, lastSeq: 11n }, { count: 1, lastSeq: 12n }, 12n, 12n],
    ['messages full, handshakes short', { count: 256, lastSeq: 300n }, { count: 1, lastSeq: 5n }, null, 300n],
    ['messages short, handshakes full', { count: 40, lastSeq: 40n }, { count: 512, lastSeq: 600n }, null, 40n],
    ['both full', { count: 256, lastSeq: 300n }, { count: 512, lastSeq: 600n }, null, 300n],
    ['nothing', { count: 0, lastSeq: null }, { count: 0, lastSeq: null }, null, 0n],
  ];
  it.each(cases)('%s', (_name, ms, hs, carry, want) => {
    expect(throughOf(ms, hs, carry)).toBe(want);
  });
});

it('validates an authenticated Add candidate before posting its commit', async () => {
  const d = device(ds, clock, ME);
  const g = await openRegistered(ds, d);
  ds.publishList(THIRD.user, [{ device: THIRD.device }]);
  ds.propose(g, 'add', THIRD.device);
  d.core.proposedAdds = () => [{ index: 0n, userId: THIRD.user, cachedVersion: 0n }];
  let ingested = 0;
  d.core.deviceListIngestAdd = () => { ingested += 1; return { version: 1n, changed: false, quarantined: 0 }; };
  const observedPostCounts: number[] = [];
  d.engine.deps.onTrustChanged = () => { observedPostCounts.push(count('postCommit')); };
  expect(await d.engine.commitNow(g, 0)).toBe(true);
  expect(ingested).toBe(1);
  expect(observedPostCounts).toEqual([0]);
  expect(count('postCommit')).toBe(1);
});

it('hands the Add check the signed list of the candidate head, as the core decodes it', async () => {
  const d = device(ds, clock, ME);
  const g = await openRegistered(ds, d);
  ds.publishList(THIRD.user, [{ device: THIRD.device }]);
  ds.propose(g, 'add', THIRD.device);
  d.core.proposedAdds = () => [{ index: 0n, userId: THIRD.user, cachedVersion: 0n }];
  const heads: Uint8Array[] = [];
  d.core.deviceListIngestAdd = (...args: unknown[]) => { heads.push(args[5] as Uint8Array); return { version: 1n, changed: false, quarantined: 0 }; };
  expect(await d.engine.commitNow(g, 0)).toBe(true);
  const served = await d.engine.deps.routes.getDeviceList(THIRD.user);
  expect(heads).toEqual([served!.blob]);
});

describe('a failed Add candidate (security F2, controller ruling 3)', () => {
  const FORGED = idOf(0xd0, 23);
  /** ME's core caches PEER's list v1 (naming only PEER.device), as a trust refresh would. */
  async function cachedPeerList(d: Device, g: Uint8Array): Promise<void> {
    ds.publishList(PEER.user, [{ device: PEER.device }]);
    d.core.addPendingLeaf(g, 1, PEER.user, PEER.device, fakeDskPub(PEER.device), 1);
    const { head, history } = await readDeviceListHistory(d.engine.deps.routes, PEER.user, 0n);
    d.core.deviceListIngest(g, 1, PEER.user, history, signedHeadList(head), 1n);
  }

  it('is left out and the commit is still posted', async () => {
    const d = device(ds, clock, ME);
    const g = await openRegistered(ds, d);
    await cachedPeerList(d, g);
    d.core.addKeyPackage(PEER.user, FORGED, fakeDskPub(FORGED), 1);
    ds.propose(g, 'add', FORGED);
    expect(await d.engine.commitNow(g, 0)).toBe(true);
    expect(count('postCommit')).toBe(1);
    expect(ds.welcomesFor(FORGED)).toEqual([]);
    expect(d.core.groupRow(g)?.epoch).toBe(1n);
  });

  // ABI 8: device_list_ingest_add records the failed verdict, so the commit leaves the Add out even for a user this device
  // never cached (before, commit_build answered E_DEVICE_LIST_STALE and the device could not commit).
  it('of a never-cached user whose fetched list leaves it out is left out, and the commit is posted', async () => {
    const d = device(ds, clock, ME);
    const g = await openRegistered(ds, d);
    ds.publishList(THIRD.user, []);
    d.core.addKeyPackage(THIRD.user, THIRD.device, fakeDskPub(THIRD.device), 1);
    ds.propose(g, 'add', THIRD.device);
    expect(await d.engine.commitNow(g, 0)).toBe(true);
    expect(count('postCommit')).toBe(1);
    expect(ds.welcomesFor(THIRD.device)).toEqual([]);
  });

  it('of the own user under another key (E_UMK_CHANGED) is left out without a retry, and the commit is posted', async () => {
    const d = device(ds, clock, ME);
    const g = await openRegistered(ds, d);
    ds.publishList(ME.user, [{ device: ME.device }, { device: FORGED }]);
    d.core.addKeyPackage(ME.user, FORGED, fakeDskPub(FORGED), 1, new Uint8Array(32).fill(0xc4));
    ds.propose(g, 'add', FORGED);
    let refusals = 0;
    const ingest = d.core.deviceListIngestAdd.bind(d.core);
    d.core.deviceListIngestAdd = (...args: Parameters<typeof ingest>) => {
      try { return ingest(...args); } catch (e) { if (e instanceof CoreError) refusals += e.code === 'E_UMK_CHANGED' ? 1 : 0; throw e; }
    };
    expect(await d.engine.commitNow(g, 0)).toBe(true);
    expect(refusals).toBe(1);
    expect(count('postCommit')).toBe(1);
    expect(ds.welcomesFor(FORGED)).toEqual([]);
  });

  it('a candidate whose history is unknown still refuses, aborts and posts nothing', async () => {
    const d = device(ds, clock, ME);
    const g = await openRegistered(ds, d);
    d.core.addKeyPackage(THIRD.user, THIRD.device, fakeDskPub(THIRD.device), 1);
    ds.propose(g, 'add', THIRD.device);
    expect(await d.engine.commitNow(g, 0)).toBe(false);
    expect(count('postCommit')).toBe(0);
    // Once the list is served, the next attempt fetches it and commits the Add.
    ds.publishList(THIRD.user, [{ device: THIRD.device }]);
    expect(await d.engine.commitNow(g, 0)).toBe(true);
    expect(ds.welcomesFor(THIRD.device)).toHaveLength(1);
  });
});

describe('catch-up on every ready (rule 2)', () => {
  it('allows a newly registered group to send when opened after the ready sweep', async () => {
    const d = device(ds, clock, ME);
    d.gateway.ready(readyInfo(ME));
    await settle();
    const opened = await d.engine.openChannel({ communityId: COMMUNITY, channelId: CHANNEL, textGroupId: null });
    expect(opened.state).toBe(2);
    ds.join(opened.groupId, PEER.device);
    d.engine.send(opened.groupId, 'created after ready');
    await settle();
    expect(ds.view(opened.groupId).messages.map((m) => m.body)).toEqual(['created after ready']);
  });

  it('repairs only when requested, verifies the repair, then catches up and drains', async () => {
    const d = device(ds, clock, ME);
    const g = await openRegistered(ds, d);
    ds.healNeeded.set(toHex(g), true);
    ds.resetCalls();
    d.gateway.ready(readyInfo(ME));
    await settle();
    const routes = ds.calls.filter((c) => c.group === toHex(g)).map((c) => c.route);
    const before = routes.indexOf('getGroupHealState');
    const post = routes.indexOf('postGroupHeal');
    const verify = routes.indexOf('getGroupHealState', before + 1);
    const catchup = routes.indexOf('getHandshakes');
    expect(before).toBeGreaterThanOrEqual(0);
    expect(post).toBeGreaterThan(before);
    expect(verify).toBeGreaterThan(post);
    expect(catchup).toBeGreaterThan(verify);
    expect(ds.healNeeded.get(toHex(g))).toBe(false);
    expect(d.core.calls.filter((c) => c.m === 'groupHealSnapshot' && c.g === toHex(g))).toHaveLength(1);
  });
  it('holds an already queued send until the ready heal check succeeds', async () => {
    const d = device(ds, clock, ME);
    const g = await openRegistered(ds, d);
    const lane = deferred();
    const heal = deferred();
    const blockedLane = d.engine.runOnGroup(g, () => lane.promise);
    d.engine.send(g, 'queued through restore');
    ds.healNeeded.set(toHex(g), true);
    ds.inject('getGroupHealState', { gate: heal.promise });
    ds.resetCalls();
    d.gateway.ready(readyInfo(ME));
    await settle();
    lane.resolve();
    await blockedLane;
    await settle();
    expect(ds.view(g).messages.map((m) => m.body)).toEqual([]);
    heal.resolve();
    await settle();
    expect(ds.view(g).messages.map((m) => m.body)).toEqual(['queued through restore']);
    expect(ds.calls.findIndex((c) => c.route === 'postGroupHeal' && c.group === toHex(g)))
      .toBeLessThan(ds.calls.findIndex((c) => c.route === 'postMessage' && c.group === toHex(g)));
  });
  it('keeps one failed repair from blocking another group on the same ready', async () => {
    const d = device(ds, clock, ME);
    const groupA = await openRegistered(ds, d);
    const groupB = await openRegistered(ds, d, idOf(0x28, 1));
    const [failed, healthy] = [groupA, groupB].sort((a, b) => toHex(a).localeCompare(toHex(b)));
    ds.healNeeded.set(toHex(failed), true);
    ds.healNeeded.set(toHex(healthy), true);
    ds.inject('postGroupHeal', { fail: new Error('repair refused') });
    ds.resetCalls();
    d.gateway.ready(readyInfo(ME));
    await settle();
    expect(ds.calls.some((c) => c.route === 'getHandshakes' && c.group === toHex(failed))).toBe(false);
    expect(ds.calls.some((c) => c.route === 'getHandshakes' && c.group === toHex(healthy))).toBe(true);
  });
  it('does not catch up a group when the heal response disagrees with its local snapshot', async () => {
    const d = device(ds, clock, ME);
    const g = await openRegistered(ds, d);
    ds.healNeeded.set(toHex(g), true);
    d.engine.deps.routes.postGroupHeal = () => Promise.resolve({ epoch: 99n, nextSeq: 99n });
    ds.resetCalls();
    d.gateway.ready(readyInfo(ME));
    await settle();
    expect(ds.calls.some((c) => c.route === 'getHandshakes' && c.group === toHex(g))).toBe(false);
    expect(ds.calls.some((c) => c.route === 'getMessages' && c.group === toHex(g))).toBe(false);
  });

  it('reports a persisted trust quarantine even if groupApply then refuses the batch', async () => {
    const d = device(ds, clock, ME);
    const g = await openRegistered(ds, d);
    ds.detach(ME.device);
    ds.peerSend(g, PEER, 'bad leaf');
    const before = d.changes.length;
    d.core.groupApply = () => { throw new CoreError('E_CREDENTIAL'); };
    expect(await d.engine.catchUpNow(g, 'catch-up')).toBe(false);
    expect(d.changes).toHaveLength(before + 1);
    expect(d.changes.at(-1)?.group).toBe(toHex(g));
  });
  it('applies a commit before the messages of the epoch it opens when only the message page is full', async () => {
    const d = device(ds, clock, ME);
    const g = await openRegistered(ds, d);
    ds.detach(ME.device);
    for (let i = 0; i < 200; i++) ds.peerSend(g, PEER, `a${String(i)}`); // seq 1..200, epoch 0
    expect(ds.peerCommit(g, PEER)).toBe(201n); // epoch 0 -> 1
    for (let i = 0; i < 100; i++) ds.peerSend(g, PEER, `b${String(i)}`); // seq 202..301, epoch 1
    d.gateway.ready(readyInfo(ME));
    await settle();
    // Round 1 (from 1): messages full, m = 257 → boundM 257; handshakes short, h = 201 → boundH max(201, 257) = 257;
    //   through 257, carry 201. Round 2 (from 258): m = 301 short → boundM max(301, 201) = 301; h null → boundH 301;
    //   through 301; neither page full and no handshake above 301 → done.
    expect(d.core.applyCalls.map((c) => c.through)).toEqual([257n, 301n]);
    expect(fromArgs('getHandshakes')).toEqual([1n, 258n]);
    expect(fromArgs('getMessages')).toEqual([1n, 258n]);
    expect(d.core.bodies(g)).toEqual([
      ...Array.from({ length: 200 }, (_, i) => `a${String(i)}`),
      ...Array.from({ length: 100 }, (_, i) => `b${String(i)}`),
    ]);
    expect(d.core.unreadable(g)).toEqual([]);
    expect(d.core.group(g)).toMatchObject({ state: 2, epoch: 1n, nextSeq: 302n });
    expect(cursorOf(g)).toEqual([301n, 1n]);
  });

  it('takes the lower lastSeq when both pages are full, so no handshake is skipped', async () => {
    const d = device(ds, clock, ME);
    const g = await openRegistered(ds, d);
    ds.detach(ME.device);
    for (let i = 0; i < 520; i++) ds.propose(g, 'add', idOf(0xaa, i + 1)); // seq 1..520
    for (let i = 0; i < 260; i++) ds.peerSend(g, PEER, `m${String(i)}`); // seq 521..780
    d.gateway.ready(readyInfo(ME));
    await settle();
    // Round 1 (from 1): messages full, m = 776; handshakes full, h = 512 → through min(776, 512) = 512, carry 512.
    // Round 2 (from 513): messages full, m = 776; handshakes short, h = 520 → boundH max(520, 776) = 776 → through 776,
    //   carry 520. Round 3 (from 777): m = 780 short → boundM max(780, 520) = 780; h null → boundH 780 → through 780.
    expect(d.core.applyCalls.map((c) => c.through)).toEqual([512n, 776n, 780n]);
    expect(fromArgs('getHandshakes')).toEqual([1n, 513n, 777n]);
    expect(d.core.group(g)?.proposalsPending).toBe(520);
    expect(d.core.bodies(g)).toHaveLength(260);
    expect(d.core.group(g)?.nextSeq).toBe(781n);
  });

  it('a message stored between the two reads, below a later commit, is not skipped', async () => {
    const d = device(ds, clock, ME);
    const g = await openRegistered(ds, d);
    for (let i = 0; i < 10; i++) ds.peerSend(g, PEER, `live${String(i)}`); // seq 1..10, applied live
    await settle();
    expect(d.core.group(g)).toMatchObject({ state: 2, epoch: 0n, nextSeq: 11n });
    ds.resetCalls();
    ds.afterNextMessagesRead(g, () => {
      ds.peerSend(g, PEER, 'between'); // seq 11, epoch 0: stored after the message read
      ds.peerCommit(g, PEER); // seq 12: a self-update, epoch 0 -> 1, seen by the handshake read
    });
    d.gateway.ready(readyInfo(ME));
    await settle();
    // Round 1: m null, h 12, carry null → through 0, nothing applied, carry 12; h 12 > 0 → round 2:
    // m 11, h 12 → boundM max(11, 12) = 12, boundH max(12, 11) = 12 → through 12.
    expect(d.core.timeline(g, 0n, 200).find((r) => r.seq === 11n)).toMatchObject({ seq: 11n, status: 0, body: 'between' });
    expect(d.core.group(g)).toMatchObject({ state: 2, epoch: 1n, nextSeq: 13n });
    expect(count('getMessages')).toBe(2);
    expect(count('getHandshakes')).toBe(2);
  });

  it('a commit stored between the two reads is applied before the message of its epoch', async () => {
    const d = device(ds, clock, ME);
    const g = await openRegistered(ds, d);
    for (let i = 0; i < 10; i++) ds.peerSend(g, PEER, `live${String(i)}`); // seq 1..10, applied live
    await settle();
    expect(d.core.group(g)).toMatchObject({ state: 2, epoch: 0n, nextSeq: 11n });
    d.core.resetLog();
    ds.resetCalls();
    ds.afterNextMessagesRead(g, () => {
      ds.peerCommit(g, PEER); // seq 11: epoch 0 -> 1
      ds.peerSend(g, PEER, 'new epoch'); // seq 12, framed in epoch 1
    });
    d.gateway.ready(readyInfo(ME));
    await settle();
    // Round 1: m null, h 11 → through 0: no groupApply. Round 2: m 12, h 11, carry 11 → through 12.
    expect(d.core.applyCalls).toEqual([{ g: toHex(g), through: 12n, handshakes: 1, messages: 1 }]);
    expect(d.core.timeline(g, 0n, 200).find((r) => r.seq === 12n)).toMatchObject({ seq: 12n, status: 0, body: 'new epoch' });
    expect(d.core.group(g)).toMatchObject({ state: 2, epoch: 1n, nextSeq: 13n });
    expect(count('getMessages')).toBe(2);
    expect(count('getHandshakes')).toBe(2);
  });

  it('acknowledges the cursor after a catch-up and only when something new was applied', async () => {
    const d = device(ds, clock, ME);
    const g = await openRegistered(ds, d);
    ds.detach(ME.device);
    ds.peerSend(g, PEER, 'x');
    ds.peerSend(g, PEER, 'y');
    ds.peerSend(g, PEER, 'z');
    d.gateway.ready(readyInfo(ME));
    await settle();
    expect(cursorOf(g)).toEqual([3n, 0n]);
    expect(count('postCursor')).toBe(1);
    const routes = ds.calls.filter((c) => c.dev === toHex(ME.device)).map((c) => c.route);
    expect(routes.lastIndexOf('getMessages')).toBeLessThan(routes.lastIndexOf('postCursor'));
    d.gateway.ready(readyInfo(ME));
    await settle();
    expect(count('postCursor')).toBe(1);
    ds.peerSend(g, PEER, 'w');
    d.gateway.ready(readyInfo(ME));
    await settle();
    expect(cursorOf(g)).toEqual([4n, 0n]);
    expect(count('postCursor')).toBe(2);
    expect(d.core.cursorBody(g)).toBeNull();
  });

  it('polls Welcomes first, joins by a waiting Welcome, deletes it and catches the new group up', async () => {
    const d = device(ds, clock, ME);
    const w = ds.peerCreate(PEER, CHANNEL_2);
    ds.detach(ME.device);
    ds.peerCommit(w, PEER, [ME.device]); // seq 1, epoch 1, a Welcome for ME
    ds.peerSend(w, PEER, 'after'); // seq 2, epoch 1
    d.engine.setChannels([{ groupId: w, communityId: COMMUNITY, channelId: CHANNEL_2, policyVersion: 1n }]);
    ds.resetCalls();
    d.gateway.ready(readyInfo(ME));
    await settle();
    expect(at(ds.calls.filter((c) => c.dev === toHex(ME.device)), 0).route).toBe('getWelcomes');
    expect(d.core.group(w)).toMatchObject({ state: 2, epoch: 1n, nextSeq: 3n });
    expect(ds.welcomesFor(ME.device)).toEqual([]);
    expect(count('deleteWelcome')).toBe(1);
    expect(count('postResync')).toBe(0);
    expect(d.core.bodies(w)).toEqual(['after']);
  });
});

describe('live frames (rule 3)', () => {
  // web3.7 RB1: op 19's 7th element, the upload class, becomes the catch-up row's 9th, so the core applies its
  // class/type rule to a live frame as to a fetched row. A web3.6 host's six elements keep the 8-element row.
  // The class-1 case shows its type-0 post only because ModelCore ignores the class: it does not model the mismatch
  // rule, which the real core applies (client_fold.rs class_mismatch_is_neither_shown_nor_folded). Here only the
  // re-encoded row is under test.
  it.each([
    ['class 1', [1n], 9],
    ['a null class', [null], 9],
    ['no class (a web3.6 host)', [], 8],
  ] as const)('re-encodes an op 19 with %s as the row the core applies', async (_name, tail, length) => {
    const d = device(ds, clock, ME);
    const g = await openRegistered(ds, d);
    const sent: unknown[][] = [];
    ds.attach(ME.device, (f) => {
      if (f.op === 19) sent.push(f.payload);
      d.gateway.frame(f.op === 19 ? { ...f, payload: [...f.payload, ...tail] } : f);
    });
    const rows: unknown[] = [];
    const apply = d.core.groupApply.bind(d.core);
    d.core.groupApply = (gid, hs, ms, through) => { rows.push(...(decode(ms) as unknown[])); return apply(gid, hs, ms, through); };
    ds.peerSend(g, PEER, 'live');
    await settle();
    expect(d.core.bodies(g)).toEqual(['live']);
    const p = at(sent, 0);
    const row = [p[0], p[1], p[2], p[3], null, p[4], p[5], 0n];
    expect(rows).toEqual([length === 9 ? [...row, tail[0]] : row]);
  });
  it('applies the next frame with through = seq and drops a frame it already applied', async () => {
    const d = device(ds, clock, ME);
    const g = await openRegistered(ds, d);
    const s = ds.peerSend(g, PEER, 'live');
    await settle();
    expect(s).toBe(1n);
    expect(d.core.applyCalls).toEqual([{ g: toHex(g), through: 1n, handshakes: 0, messages: 1 }]);
    expect(d.core.bodies(g)).toEqual(['live']);
    expect(count('getMessages')).toBe(0);
    d.gateway.frame(at(d.gateway.frames, d.gateway.frames.length - 1));
    await settle();
    expect(d.core.applyCalls).toHaveLength(1);
  });

  it('catches up over HTTP when a frame arrives ahead of next_seq, instead of applying it', async () => {
    const d = device(ds, clock, ME);
    const g = await openRegistered(ds, d);
    ds.dropNext(ME.device, 2);
    ds.peerSend(g, PEER, 'one');
    ds.peerSend(g, PEER, 'two');
    ds.peerSend(g, PEER, 'three');
    await settle();
    expect(d.core.bodies(g)).toEqual(['one', 'two', 'three']);
    expect(fromArgs('getMessages')).toEqual([1n]);
    expect(d.core.applyCalls).toEqual([{ g: toHex(g), through: 3n, handshakes: 0, messages: 3 }]);
    expect(cursorOf(g)).toEqual([3n, 0n]);
  });

  it('ignores op 18 and frames of unknown groups, and catches up after a malformed frame', async () => {
    const d = device(ds, clock, ME);
    const g = await openRegistered(ds, d);
    d.gateway.frame({ op: 18, n: 51n, groupId: g, payload: [1n, 1n] });
    d.gateway.frame({ op: 19, n: 52n, groupId: idOf(0x99, 9), payload: [1n, 0n, PEER.device, new Uint8Array([1]), new Uint8Array(32), 1n] });
    await settle();
    expect(d.core.calls).toEqual([]);
    expect(ds.calls).toEqual([]);
    d.gateway.frame({ op: 19, n: 53n, groupId: g, payload: [1n] });
    await settle();
    expect(d.core.applyCalls).toEqual([]);
    expect(count('getHandshakes')).toBe(1);
  });

  it('drops a held frame with an invalid sequence and replays the remaining frames on activation', async () => {
    const d = device(ds, clock, ME);
    const g = ds.peerCreate(PEER, CHANNEL);
    d.core.groupCreate(g, COMMUNITY, CHANNEL);
    ds.join(g, ME.device);
    d.gateway.frame({ op: 19, n: 50n, groupId: g, payload: ['bad-seq', 0n, PEER.device, new Uint8Array([1]), new Uint8Array(32), 1n] });
    ds.peerSend(g, PEER, 'valid');
    await settle();
    expect(d.core.group(g)?.state).toBe(0);
    d.core.groupRegistered(g, 1n);
    await d.engine.activate(g);
    await settle();
    expect(d.core.bodies(g)).toEqual(['valid']);
    expect(count('getMessages')).toBeGreaterThan(0);
  });

  it('holds back the frames of a group while its queue is busy, but not those of another group', async () => {
    const d = device(ds, clock, ME);
    const g1 = await openRegistered(ds, d, CHANNEL);
    const g2 = await openRegistered(ds, d, CHANNEL_2);
    const gate = deferred();
    ds.inject('postMessage', { gate: gate.promise });
    d.engine.send(g1, 'mine');
    await settle();
    ds.peerSend(g1, PEER, 'held back');
    ds.peerSend(g2, PEER, 'not held');
    await settle();
    expect(d.core.bodies(g2)).toEqual(['not held']);
    expect(d.core.bodies(g1)).toEqual([]);
    gate.resolve();
    await settle();
    expect(d.core.bodies(g1)).toEqual(['held back', 'mine']);
    expect(coreCalls(d, g1, ['sendEncrypt', 'sendConfirm', 'groupApply']).slice(0, 3)).toEqual(['sendEncrypt', 'sendConfirm', 'groupApply']);
  });

  it('stop() unsubscribes, clears its timers and refuses to send', async () => {
    const d = device(ds, clock, ME);
    const g = await openRegistered(ds, d);
    ds.propose(g, 'add', THIRD.device);
    await settle();
    expect(clock.pending).toBe(2); // the volunteer timer and the cursor timer of the live frame
    d.engine.stop();
    expect(clock.pending).toBe(0);
    expect(d.gateway.listenerCount).toBe(0);
    ds.peerSend(g, PEER, 'late');
    await clock.advance(10_000);
    expect(d.core.bodies(g)).toEqual([]);
    expect(count('postCommit')).toBe(0);
    expect(catchSync(() => d.engine.send(g, 'after stop'))).toMatchObject({ code: 'E_SYNC_STOPPED' });
    await expect(d.engine.openChannel({ communityId: COMMUNITY, channelId: CHANNEL_2, textGroupId: null })).rejects.toMatchObject({
      code: 'E_SYNC_STOPPED',
    });
  });

  it('a deleted frame clears a stored message', async () => {
    const d = device(ds, clock, ME);
    const g = await openRegistered(ds, d);
    const s = ds.peerSend(g, PEER, 'soon gone');
    await settle();
    expect(d.core.timeline(g, 0n, 200)).toMatchObject([{ seq: s, status: 0, body: 'soon gone' }]);
    const before = d.changes.length;
    d.gateway.frame({ op: 21, n: 90n, groupId: g, payload: [s, 1_700_000_000n] });
    await settle();
    expect(d.core.timeline(g, 0n, 200)).toMatchObject([{ seq: s, status: 2, body: '' }]);
    expect(d.changes).toHaveLength(before + 1);
    expect(at(d.changes, before)).toMatchObject({ group: toHex(g), result: { newSeqs: [s], epochChanged: false, ownAdopted: false } });
  });

  it('a deleted frame for a seq this device does not hold changes nothing', async () => {
    const d = device(ds, clock, ME);
    const g = await openRegistered(ds, d);
    ds.peerSend(g, PEER, 'kept'); // seq 1
    await settle();
    const before = d.changes.length;
    d.gateway.frame({ op: 21, n: 91n, groupId: g, payload: [99n, 1_700_000_000n] });
    d.gateway.frame({ op: 21, n: 92n, groupId: g, payload: [1n] }); // malformed: dropped before the core
    await settle();
    expect(coreCalls(d, g, ['messageDeleted'])).toEqual(['messageDeleted']);
    expect(d.changes).toHaveLength(before);
    expect(d.core.bodies(g)).toEqual(['kept']);
  });

  it('posts the cursor once, cursorDebounceMs after the first live frame, and not per frame', async () => {
    const d = device(ds, clock, ME);
    const g = await openRegistered(ds, d);
    for (let i = 0; i < 3; i++) {
      ds.peerSend(g, PEER, `f${String(i)}`); // seq 1..3, three live frames within one second
      await clock.advance(400);
    }
    expect(d.core.bodies(g)).toEqual(['f0', 'f1', 'f2']);
    expect(count('postCursor')).toBe(0);
    await clock.advance(SYNC.cursorDebounceMs);
    expect(count('postCursor')).toBe(1);
    expect(cursorOf(g)).toEqual([3n, 0n]);
    expect(coreCalls(d, g, ['cursorBody', 'cursorAcked'])).toEqual(['cursorBody', 'cursorAcked']);
    await clock.advance(60_000);
    expect(count('postCursor')).toBe(1);
  });

  it('a ready before the cursor timer fires posts the cursor through the catch-up and cancels the timer', async () => {
    const d = device(ds, clock, ME);
    const g = await openRegistered(ds, d);
    ds.peerSend(g, PEER, 'one'); // seq 1, applied live: the cursor timer is armed
    await settle();
    expect(clock.pending).toBe(1);
    d.gateway.ready(readyInfo(ME));
    await settle();
    expect(count('postCursor')).toBe(1);
    expect(cursorOf(g)).toEqual([1n, 0n]);
    expect(clock.pending).toBe(0);
    await clock.advance(60_000);
    expect(count('postCursor')).toBe(1);
  });
});

describe('commit duty (rule 4)', () => {
  it('acknowledges commit_needed at once, commits once and cancels the volunteer timer', async () => {
    const d = device(ds, clock, ME);
    const g = await openRegistered(ds, d);
    ds.propose(g, 'add', THIRD.device);
    await settle();
    expect(d.core.group(g)?.proposalsPending).toBe(1);
    ds.elect(g, ME.device, 7n);
    expect(d.gateway.acks).toEqual([{ group: toHex(g), round: 7n }]);
    await settle();
    expect(count('postCommit')).toBe(1);
    expect(ds.view(g).epoch).toBe(1n);
    expect(d.core.group(g)).toMatchObject({ state: 2, epoch: 1n, proposalsPending: 0, pendingCommit: false });
    expect(ds.welcomesFor(THIRD.device)).toHaveLength(1);
    await clock.advance(10_000);
    expect(count('postCommit')).toBe(1);
  });

  it('volunteers after backoff_ms + random() x jitter when a ready reports outstanding proposals', async () => {
    const d = device(ds, clock, ME);
    const g = await openRegistered(ds, d);
    ds.detach(ME.device);
    ds.propose(g, 'add', THIRD.device);
    ds.reattach(ME.device);
    d.gateway.ready(readyInfo(ME, [{ groupId: g, epoch: 0n, lastSeq: 0n, proposalsOutstanding: 1 }]));
    await settle();
    expect(d.core.group(g)?.proposalsPending).toBe(1);
    await clock.advance(449);
    expect(count('postCommit')).toBe(0);
    await clock.advance(1);
    expect(count('postCommit')).toBe(1);
    expect(ds.view(g).epoch).toBe(1n);
    expect(d.gateway.acks).toEqual([]);
  });

  it('does not volunteer when commit_needed arrives inside the back-off window', async () => {
    const d = device(ds, clock, ME);
    const g = await openRegistered(ds, d);
    ds.detach(ME.device);
    ds.propose(g, 'add', THIRD.device);
    ds.reattach(ME.device);
    d.gateway.ready(readyInfo(ME, [{ groupId: g, epoch: 0n, lastSeq: 0n, proposalsOutstanding: 1 }]));
    await settle();
    await clock.advance(200);
    ds.elect(g, ME.device, 1n);
    await settle();
    expect(count('postCommit')).toBe(1);
    await clock.advance(5_000);
    expect(count('postCommit')).toBe(1);
  });

  it('on a lost commit race aborts first, catches up, and commits again after random() x commitJitterMs', async () => {
    const d = device(ds, clock, ME);
    const g = await openRegistered(ds, d);
    ds.propose(g, 'add', THIRD.device); // seq 1
    await settle();
    d.core.resetLog();
    ds.inject('postCommit', {
      before: () => {
        ds.peerCommit(g, PEER); // seq 2: the winner, epoch 0 -> 1
        ds.propose(g, 'add', FOURTH.device); // seq 3, epoch 1
      },
    });
    ds.elect(g, ME.device, 1n);
    await settle();
    expect(count('postCommit')).toBe(1);
    expect(coreCalls(d, g, COMMIT_CALLS)).toEqual(['commitBuild', 'commitAbort', 'groupApply']);
    expect(d.core.group(g)).toMatchObject({ epoch: 1n, proposalsPending: 1, pendingCommit: false });
    await clock.advance(199);
    expect(count('postCommit')).toBe(1);
    await clock.advance(1);
    expect(count('postCommit')).toBe(2);
    expect(coreCalls(d, g, COMMIT_CALLS)).toEqual(['commitBuild', 'commitAbort', 'groupApply', 'commitBuild', 'commitConfirm', 'groupApply']);
    expect(ds.view(g).commits).toEqual([
      { seq: 2n, kind: 1, committer: toHex(PEER.device) },
      { seq: 4n, kind: 1, committer: toHex(ME.device) },
    ]);
    expect(d.core.group(g)).toMatchObject({ epoch: 2n, proposalsPending: 0, pendingCommit: false });
  });

  it('gives up after commitRetryMax retries and stays quiet until the next ready', async () => {
    const d = device(ds, clock, ME);
    const g = await openRegistered(ds, d);
    ds.propose(g, 'add', THIRD.device);
    await settle();
    for (let i = 0; i < 7; i++) {
      ds.inject('postCommit', {
        before: () => {
          ds.peerCommit(g, PEER);
          ds.propose(g, 'add', idOf(0xbb, i + 1));
        },
      });
    }
    ds.elect(g, ME.device, 1n);
    await settle();
    for (let i = 0; i < SYNC.commitRetryMax; i++) await clock.advance(200);
    expect(count('postCommit')).toBe(1 + SYNC.commitRetryMax);
    await clock.advance(10_000);
    expect(count('postCommit')).toBe(1 + SYNC.commitRetryMax);
    expect(d.core.group(g)?.pendingCommit).toBe(false);
  });

  const failures: [number, string, boolean][] = [
    [425, 'E_COMMIT_REQUIRED', true],
    [422, 'E_COMMIT_INVALID', true],
    [500, 'E_INTERNAL', false],
    [0, 'E_NETWORK', false],
  ];
  it.each(failures)('aborts the pending commit on %i %s (catch-up: %s)', async (status, code, catchesUp) => {
    const d = device(ds, clock, ME);
    const g = await openRegistered(ds, d);
    ds.propose(g, 'add', THIRD.device);
    await settle();
    d.core.resetLog();
    ds.resetCalls();
    ds.inject('postCommit', { fail: httpError(status, code) });
    ds.elect(g, ME.device, 1n);
    await settle();
    expect(coreCalls(d, g, COMMIT_CALLS).slice(0, 2)).toEqual(['commitBuild', 'commitAbort']);
    expect(d.core.group(g)?.pendingCommit).toBe(false);
    expect(count('getHandshakes')).toBe(catchesUp ? 1 : 0);
    expect(ds.view(g).epoch).toBe(0n);
  });

  // Re-review N-m4: a core refusal before anything is built (commit_build's "a commit is pending", while a rotation's
  // accepted commit awaits its echo) aborts nothing: that pending commit is not this attempt's, and aborting it would make
  // its echo a stranger's commit (the call group lost, the call ended as removed).
  it('a core refusal before the build aborts no pending commit', async () => {
    const d = device(ds, clock, ME);
    const g = await openRegistered(ds, d);
    ds.propose(g, 'add', THIRD.device);
    await settle();
    d.core.resetLog();
    d.core.commitBuild = (): never => { throw new CoreError('E_CORE_STATE', 'a commit is pending'); };
    expect(await d.engine.commitNow(g, 0)).toBe(false);
    expect(coreCalls(d, g, ['commitAbort'])).toEqual([]);
  });

  it('a 422 refuses the commit for good: the group is commit-quiet until op 17 asks again', async () => {
    const d = device(ds, clock, ME);
    const g = await openRegistered(ds, d);
    ds.propose(g, 'add', THIRD.device);
    await settle();
    ds.inject('postCommit', { fail: httpError(422, 'E_COMMIT_INVALID') }); // e.g. rule group_info (hardening G)
    ds.elect(g, ME.device, 1n);
    await settle();
    expect(count('postCommit')).toBe(1);
    ds.propose(g, 'add', FOURTH.device); // applied live with proposals pending: no volunteer while quiet
    await settle();
    await clock.advance(10_000);
    expect(count('postCommit')).toBe(1);
    ds.elect(g, ME.device, 2n);
    await settle();
    expect(count('postCommit')).toBe(2);
    expect(ds.view(g).epoch).toBe(1n);
  });

  it('answers 403 E_LEAF_NOT_CURRENT on a commit with a resync', async () => {
    const d = device(ds, clock, ME);
    const g = await openRegistered(ds, d);
    ds.propose(g, 'add', THIRD.device);
    await settle();
    ds.afterNextProposalsRead(g, () => ds.evict(g, ME.device));
    ds.elect(g, ME.device, 1n);
    await settle();
    expect(coreCalls(d, g, COMMIT_CALLS).slice(-2)).toEqual(['commitBuild', 'commitAbort']);
    expect(d.membership).toEqual([{ group: toHex(g), status: 'resyncing' }]);
    expect(count('postResync')).toBe(1);
    expect(d.core.group(g)).toMatchObject({ state: 2, epoch: 1n });
  });

  it('a leaf removed before the election: getProposals answers 404 as dillad does, and the duty resyncs without building', async () => {
    const d = device(ds, clock, ME);
    const g = await openRegistered(ds, d);
    ds.propose(g, 'add', THIRD.device);
    await settle();
    ds.evict(g, ME.device);
    ds.elect(g, ME.device, 1n);
    await settle();
    expect(count('getProposals')).toBe(1);
    expect(count('postCommit')).toBe(0);
    expect(coreCalls(d, g, ['commitBuild', 'commitConfirm', 'commitAbort'])).toEqual([]);
    expect(d.membership).toEqual([{ group: toHex(g), status: 'resyncing' }]);
    expect(count('postResync')).toBe(1);
  });
});

describe('resync (rule 7)', () => {
  it('resyncs a group whose commit cannot be processed and keeps receiving', async () => {
    const d = device(ds, clock, ME);
    const g = await openRegistered(ds, d);
    ds.garbageCommit(g); // seq 1, epoch 0 -> 1
    await settle();
    expect(d.membership).toEqual([{ group: toHex(g), status: 'resyncing' }]);
    expect(count('postResync')).toBe(1);
    expect(d.core.group(g)).toMatchObject({ state: 2, epoch: 2n, nextSeq: 3n });
    ds.peerSend(g, PEER, 'after resync');
    await settle();
    expect(d.core.bodies(g)).toEqual(['after resync']);
  });

  it('leaves a refused group in state 3, reports not-member, and tries again on the next ready', async () => {
    const d = device(ds, clock, ME);
    const g = await openRegistered(ds, d);
    ds.denyJoin.add(toHex(ME.device));
    ds.garbageCommit(g);
    await settle();
    expect(d.core.group(g)?.state).toBe(3);
    expect(d.membership).toEqual([
      { group: toHex(g), status: 'resyncing' },
      { group: toHex(g), status: 'not-member' },
    ]);
    expect(count('postResync')).toBe(1);
    expect(at(d.changes, d.changes.length - 1).result.state).toBe(3);
    ds.denyJoin.clear();
    d.gateway.ready(readyInfo(ME));
    await settle();
    expect(count('postResync')).toBe(2);
    expect(d.core.group(g)?.state).toBe(2);
  });

  it('resyncs a group whose history below next_seq was pruned (410 E_PRUNED)', async () => {
    const d = device(ds, clock, ME);
    const g = await openRegistered(ds, d);
    ds.detach(ME.device);
    for (let i = 0; i < 6; i++) ds.peerSend(g, PEER, `old${String(i)}`);
    ds.prune(g, 5n);
    ds.reattach(ME.device);
    d.gateway.ready(readyInfo(ME));
    await settle();
    expect(d.membership).toEqual([{ group: toHex(g), status: 'resyncing' }]);
    expect(count('postResync')).toBe(1);
    expect(d.core.group(g)).toMatchObject({ state: 2, epoch: 1n, nextSeq: 8n });
    expect(d.core.bodies(g)).toEqual([]);
  });
});

describe('membership loss: a kicked device (rule 7, INTEGRATION-SEAMS-01)', () => {
  /** The kick as the delivery service runs it: the instance's Remove reaches the device while it still holds
   *  its leaf, then the leaf goes and a peer commits the Remove, which the device never sees. */
  async function kicked(): Promise<{ d: Device; g: Uint8Array }> {
    const d = device(ds, clock, ME);
    const g = await openRegistered(ds, d);
    ds.propose(g, 'remove', ME.device);
    await settle();
    expect(d.core.group(g)).toMatchObject({ state: 2, proposalsPending: 1 });
    ds.kick(g, ME.device);
    ds.peerCommit(g, PEER);
    await settle();
    d.core.resetLog();
    ds.resetCalls();
    return { d, g };
  }

  it('a 404 on the catch-up of an active group is a membership loss: re-admitted, the device rejoins and hears again', async () => {
    const { d, g } = await kicked();
    ds.readmit(ME.device);
    d.gateway.ready(readyInfo(ME));
    await settle();
    expect(count('getMessages')).toBeGreaterThanOrEqual(1);
    expect(d.membership).toEqual([{ group: toHex(g), status: 'resyncing' }]);
    expect(count('postResync')).toBe(1);
    expect(d.core.group(g)).toMatchObject({ state: 2, proposalsPending: 0 });
    expect(at(d.changes, d.changes.length - 1).result.state).toBe(2);
    ds.peerSend(g, PEER, 'welcome back');
    await settle();
    expect(d.core.bodies(g)).toEqual(['welcome back']);
    d.engine.send(g, 'here again');
    await settle();
    expect(ds.view(g).messages.map((m) => m.body)).toEqual(['welcome back', 'here again']);
    expect(d.core.outbox(g)).toEqual([]);
  });

  it('a 404 on the open-time moderation feed resyncs a stale active row before retrying the feed', async () => {
    const d = device(ds, clock, ME);
    const g = await openRegistered(ds, d);
    ds.kick(g, ME.device); // the local group row is still state 2; no catch-up has observed the removal
    ds.readmit(ME.device);
    ds.inject('getModerationRemovals', { fail: httpError(404, 'E_NOT_FOUND') });

    await expect(d.engine.openChannel({ communityId: COMMUNITY, channelId: CHANNEL, textGroupId: g }))
      .resolves.toEqual({ groupId: g, state: 2 });
    expect(count('getModerationRemovals')).toBe(2);
    expect(count('postResync')).toBe(1);
    expect(d.core.group(g)).toMatchObject({ state: 2 });
    expect(d.membership).toEqual([{ group: toHex(g), status: 'resyncing' }]);
  });

  it('does not repeat a repaired membership loss from a delayed moderation-feed refusal', async () => {
    const d = device(ds, clock, ME);
    const g = await openRegistered(ds, d);
    ds.kick(g, ME.device);
    ds.readmit(ME.device);
    const started = deferred(); const release = deferred();
    const original = d.engine.deps.routes.getModerationRemovals;
    let feedReads = 0;
    d.engine.deps.routes.getModerationRemovals = async (...args) => {
      feedReads += 1;
      if (feedReads === 1) {
        started.resolve(); await release.promise;
        throw httpError(404, 'E_NOT_FOUND');
      }
      return original(...args);
    };
    const opening = d.engine.openChannel({ communityId: COMMUNITY, channelId: CHANNEL, textGroupId: g });
    await started.promise;
    // This work is already on the group lane when the feed's refusal is
    // delivered. Its proposal read repairs the leaf before open's repair job.
    const repaired = d.engine.runOnGroup(g, () => d.engine.commitNow(g, 0));
    let peerSeq = 0n;
    ds.inject('postResync', { after: () => { peerSeq = ds.peerSend(g, PEER, 'between repairs'); } });
    release.resolve();
    await Promise.all([opening, repaired]); await settle();
    expect.soft(count('postResync')).toBe(1);
    expect.soft(d.core.bodies(g)).toEqual(['between repairs']);
    expect(d.core.group(g)?.nextSeq).toBe(peerSeq + 1n);
    expect(feedReads).toBeGreaterThanOrEqual(2);
  });

  it('does not let another channel recovery hide a current membership loss', async () => {
    const d = device(ds, clock, ME);
    const first = await openRegistered(ds, d);
    const second = await openRegistered(ds, d, CHANNEL_2);
    ds.kick(first, ME.device); ds.kick(second, ME.device); ds.readmit(ME.device);
    const started = deferred(); const release = deferred();
    const original = d.engine.deps.routes.getModerationRemovals;
    let refused = false;
    d.engine.deps.routes.getModerationRemovals = async (...args) => {
      if (!refused && toHex(args[0]) === toHex(first)) {
        refused = true; started.resolve(); await release.promise;
        throw httpError(404, 'E_NOT_FOUND');
      }
      return original(...args);
    };
    const opening = d.engine.openChannel({ communityId: COMMUNITY, channelId: CHANNEL, textGroupId: first });
    await started.promise;
    await d.engine.runOnGroup(second, () => d.engine.commitNow(second, 0));
    release.resolve(); await opening; await settle();
    expect(ds.callsOf(ME.device, 'postResync').map((call) => call.group).sort()).toEqual([toHex(first), toHex(second)].sort());
  });

  it('does not start a new recovery after stop while a moderation read is in flight', async () => {
    const d = device(ds, clock, ME); const g = await openRegistered(ds, d);
    const started = deferred(); const release = deferred();
    d.engine.deps.routes.getModerationRemovals = async () => {
      started.resolve(); await release.promise; throw httpError(404, 'E_NOT_FOUND');
    };
    const opening = d.engine.openChannel({ communityId: COMMUNITY, channelId: CHANNEL, textGroupId: g });
    const refused = expect(opening).rejects.toMatchObject({ code: 'E_SYNC_STOPPED' });
    await started.promise; d.engine.stop(); release.resolve(); await refused;
    expect(count('postResync')).toBe(0);
    expect(d.core.group(g)?.state).toBe(2);
  });

  it('a 404 on getProposals of an active group is a membership loss: the queued message is sent after the rejoin', async () => {
    const { d, g } = await kicked();
    ds.readmit(ME.device);
    d.engine.send(g, 'still here');
    await settle();
    expect(count('getProposals')).toBe(1);
    expect(d.membership).toEqual([{ group: toHex(g), status: 'resyncing' }]);
    expect(count('postResync')).toBe(1);
    expect(ds.view(g).messages.map((m) => m.body)).toEqual(['still here']);
    expect(d.core.outbox(g)).toEqual([]);
  });

  it('a refused resync of an active group reports not-member, keeps the row active, and fails its queued messages', async () => {
    const { d, g } = await kicked();
    const m1 = d.engine.send(g, 'lost one');
    const m2 = d.engine.send(g, 'lost two');
    await settle();
    expect(d.membership).toEqual([
      { group: toHex(g), status: 'resyncing' },
      { group: toHex(g), status: 'not-member' },
    ]);
    expect(count('getGroupInfo')).toBe(1);
    expect(count('postResync')).toBe(0);
    expect(count('postMessage')).toBe(0);
    // Never discarded: group_discard of an active row deletes its history.
    expect(coreCalls(d, g, ['groupDiscard'])).toEqual([]);
    expect(d.core.group(g)?.state).toBe(2);
    expect(d.core.outbox(g)).toMatchObject([
      { msgId: m1, state: 2, error: 'E_NOT_FOUND' },
      { msgId: m2, state: 2, error: 'E_NOT_FOUND' },
    ]);
    await clock.advance(60_000);
    expect(count('getGroupInfo')).toBe(1);
    // A retry by the person tries the membership again; still refused, the row fails again.
    d.engine.retry(m1);
    await settle();
    expect(count('getGroupInfo')).toBe(2);
    expect(d.core.outbox(g)).toMatchObject([{ msgId: m1, state: 2 }, { msgId: m2, state: 2 }]);
    // Re-admitted, the next retry rejoins and sends.
    ds.readmit(ME.device);
    d.engine.retry(m1);
    await settle();
    expect(count('postResync')).toBe(1);
    expect(d.core.group(g)).toMatchObject({ state: 2, proposalsPending: 0 });
    expect(ds.view(g).messages.map((m) => m.body)).toEqual(['lost one']);
    expect(d.core.outbox(g)).toMatchObject([{ msgId: m2, state: 2 }]);
  });

  // The docs shoot's removal after a refused voice join (task-11 shots report, concern 3): the worker no longer drops a
  // not-member report on any state-2 apply (a gateway frame for the group proves nothing about its leaf), so the engine
  // says when the membership is confirmed again: a catch-up the DS served (it answers group reads to current leaves
  // only) reports member, and a later 404 after that reports not-member again, though the resync was tried already.
  it('a not-member report is withdrawn only by a catch-up the DS served, and a later 404 reports it again', async () => {
    const d = device(ds, clock, ME);
    const g = await openRegistered(ds, d);
    ds.inject('getMessages', { fail: httpError(404, 'E_NOT_FOUND') });
    ds.inject('getGroupInfo', { fail: httpError(503, 'E_UNAVAILABLE') });
    d.engine.requestCatchUp(g);
    await settle();
    expect(d.membership).toEqual([{ group: toHex(g), status: 'resyncing' }, { group: toHex(g), status: 'not-member' }]);
    d.engine.requestCatchUp(g);
    await settle();
    expect(d.membership.at(-1)).toEqual({ group: toHex(g), status: 'member' });
    expect(d.membership).toHaveLength(3);
    ds.inject('getMessages', { fail: httpError(404, 'E_NOT_FOUND') });
    d.engine.requestCatchUp(g);
    await settle();
    expect(d.membership.at(-1)).toEqual({ group: toHex(g), status: 'not-member' });
    expect(count('getGroupInfo')).toBe(1);   // no second resync before the next ready
    expect(d.membership).toHaveLength(4);
  });

  // The admin walk's rejoin in the same tab (web3.4): the resync refused while the device was out of the server stood as
  // not-member, and a resync is tried once per ready, so only a reload (a new ready) made the channel writable again.
  it('a rejoin of the community re-arms the resync: removed and rejoined twice in one ready, the channel opens writable', async () => {
    const { d, g } = await kicked();
    for (let round = 0; round < 2; round++) {
      if (round > 0) ds.kick(g, ME.device);
      d.engine.requestCatchUp(g);
      await settle();
      expect(d.membership.at(-1)).toEqual({ group: toHex(g), status: 'not-member' });
      ds.readmit(ME.device);
      d.engine.rejoined(COMMUNITY);
      await settle();
      expect(d.membership.at(-1)).toEqual({ group: toHex(g), status: 'resyncing' });
      expect(d.core.group(g)?.state).toBe(2);
    }
    expect(count('postResync')).toBe(2);
    expect(await d.engine.openChannel({ communityId: COMMUNITY, channelId: CHANNEL, textGroupId: g })).toEqual({ groupId: g, state: 2 });
    d.engine.send(g, 'back again');
    await settle();
    expect(ds.view(g).messages.map((m) => m.body)).toEqual(['back again']);
    expect(d.membership.at(-1)).toEqual({ group: toHex(g), status: 'resyncing' });
  });

  it('a loss met only after the rejoin resyncs, though a resync was tried earlier in the same ready', async () => {
    const { d, g } = await kicked();
    d.engine.requestCatchUp(g);
    await settle();
    ds.readmit(ME.device);
    d.engine.rejoined(COMMUNITY);
    await settle();
    expect(count('postResync')).toBe(1);
    ds.kick(g, ME.device);                           // removed again, and joined again before any read meets it
    ds.readmit(ME.device);
    d.engine.rejoined(COMMUNITY);
    await settle();
    expect(count('postResync')).toBe(1);
    d.engine.requestCatchUp(g);
    await settle();
    expect(count('postResync')).toBe(2);
    expect(d.membership.at(-1)).toEqual({ group: toHex(g), status: 'resyncing' });
    expect(d.core.group(g)?.state).toBe(2);
  });

  it('a rejoin resyncs only the lost groups of that community, and a refused one is not tried again before the next rejoin', async () => {
    const { d, g } = await kicked();
    d.engine.requestCatchUp(g);
    await settle();
    expect(count('getGroupInfo')).toBe(1);
    d.engine.rejoined(idOf(0xc0, 99));               // another server: nothing to do here
    await settle();
    expect(count('getGroupInfo')).toBe(1);
    d.engine.rejoined(COMMUNITY);                    // still refused by the ACL: tried once, reported again
    await settle();
    expect(count('getGroupInfo')).toBe(2);
    expect(d.membership.at(-1)).toEqual({ group: toHex(g), status: 'not-member' });
    d.engine.requestCatchUp(g);
    await settle();
    expect(count('getGroupInfo')).toBe(2);
  });

  it('a refused resync after a 404 on the catch-up reports not-member once per ready', async () => {
    const { d, g } = await kicked();
    d.gateway.ready(readyInfo(ME));
    await settle();
    expect(d.membership).toEqual([
      { group: toHex(g), status: 'resyncing' },
      { group: toHex(g), status: 'not-member' },
    ]);
    expect(count('getGroupInfo')).toBe(1);
    expect(d.core.group(g)?.state).toBe(2);
    await clock.advance(60_000);
    expect(count('getGroupInfo')).toBe(1);
    d.gateway.ready(readyInfo(ME));
    await settle();
    expect(count('getGroupInfo')).toBe(2);
  });
});
