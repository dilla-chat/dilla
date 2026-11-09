/** Test support for src/sync: one engine wired to ModelCore, ModelDs and FakeGateway. */
import type { ApplyResult, Id, OutboxRow } from '../../core-port';
import type { ReadyInfo } from '../../gateway/gateway';
import { toHex } from '../../hex';
import type { Instance } from '../../http/routes';
import { SyncEngine, type SyncDeps } from '../engine';
import type { JoinAllProgress } from '../joinall';
import { CHANNEL, COMMUNITY, FakeGateway, ModelCore, PEER, idOf, settle, type ManualClock, type ModelDs, type Peer } from './model';

export const INSTANCE: Instance = {
  instanceId: idOf(0xa0, 1), generation: 1n, name: 'model', registrationMode: 0, authMethods: [0], policyVersion: 1n,
  externalSenderPub: new Uint8Array(32).fill(7),
};

export interface Device {
  readonly me: Peer;
  readonly core: ModelCore;
  readonly gateway: FakeGateway;
  readonly engine: SyncEngine;
  readonly changes: { group: string; result: ApplyResult }[];
  readonly outboxChanges: string[];
  readonly membership: { group: string; status: 'resyncing' | 'not-member' | 'member' }[];
  readonly joinAll: JoinAllProgress[];
  readonly unexpected: string[];
  /** The msg id hex of every row beforeSend was called for, in call order. */
  readonly beforeSends: string[];
  /** Every onDiscarded report: the group hex, the msg id hex and the blob id hexes of the row. */
  readonly discarded: { group: string; msgId: string; blobIds: string[] }[];
  /** The group hex of every refreshGroupTrust call, in call order. */
  readonly refreshes: string[];
  /** What beforeSend and refreshGroupTrust answer after recording; a test replaces them. Both resolve by default. */
  readonly hooks: { beforeSend: SyncDeps['beforeSend']; refreshGroupTrust: SyncDeps['refreshGroupTrust'] };
  /** Every onSlow report (CB2): the group hex, untilMs and queued. */
  readonly slow: { group: string; until: number; queued: boolean }[];
  /** Every onPosted report (CB2): the group hex and the confirmed row's type. */
  readonly posted: { group: string; type: number }[];
  /** The server-confirmed application sequence passed to onPosted, when available. */
  readonly postedSequences: bigint[];
}

/** An engine for me, started, with its gateway attached to ds's fan-out. random() always answers `random`. */
export function device(ds: ModelDs, clock: ManualClock, me: Peer, random = 0.5): Device {
  const core = new ModelCore(me);
  const gateway = new FakeGateway();
  const changes: Device['changes'] = [];
  const outboxChanges: string[] = [];
  const membership: Device['membership'] = [];
  const joinAll: JoinAllProgress[] = [];
  const unexpected: string[] = [];
  const beforeSends: string[] = [];
  const discarded: Device['discarded'] = [];
  const refreshes: string[] = [];
  const slow: Device['slow'] = [];
  const posted: Device['posted'] = [];
  const postedSequences: Device['postedSequences'] = [];
  const hooks: Device['hooks'] = { beforeSend: () => Promise.resolve(), refreshGroupTrust: () => Promise.resolve() };
  ds.attach(me.device, (f) => {
    gateway.frame(f);
  });
  const engine = new SyncEngine({
    core,
    routes: ds.routesFor(me.device),
    gateway,
    instance: INSTANCE,
    deviceId: me.device,
    now: clock.now,
    random: () => random,
    setTimeout: clock.setTimeout,
    clearTimeout: clock.clearTimeout,
    onGroupChanged: (groupId, result) => { changes.push({ group: toHex(groupId), result }); },
    onTrustChanged: () => {},
    onOutboxChanged: (groupId) => { outboxChanges.push(toHex(groupId)); },
    onMembership: (groupId, status) => { membership.push({ group: toHex(groupId), status }); },
    onJoinAll: (p) => { joinAll.push({ ...p }); },
    onUnexpectedWelcome: (g) => { unexpected.push(toHex(g)); },
    refreshGroupTrust: (groupId: Id) => { refreshes.push(toHex(groupId)); return hooks.refreshGroupTrust(groupId); },
    beforeSend: (groupId: Id, row: OutboxRow) => { beforeSends.push(toHex(row.msgId)); return hooks.beforeSend(groupId, row); },
    onDiscarded: (groupId: Id, row: OutboxRow) => {
      discarded.push({ group: toHex(groupId), msgId: toHex(row.msgId), blobIds: row.attachments.map((a) => toHex(a.blobId)) });
    },
    onSlow: (groupId: Id, until: number, queued: boolean) => { slow.push({ group: toHex(groupId), until, queued }); },
    onPosted: (groupId: Id, type: number, _row, seq) => { posted.push({ group: toHex(groupId), type }); if (seq !== undefined) postedSequences.push(seq); },
  });
  engine.start();
  return { me, core, gateway, engine, changes, outboxChanges, membership, joinAll, unexpected, beforeSends, discarded, refreshes, hooks, slow, posted, postedSequences };
}

export function readyInfo(me: Peer, groups: ReadyInfo['groups'] = []): ReadyInfo {
  return {
    deviceId: me.device, userId: me.user, generation: 1n, versions: { wire: 2n, e2ee: 1n, media: 1n },
    keypackagesRemaining: 32, groups, heartbeatMs: 30000, maxFrameBytes: 131584,
    backoffMs: 300, backoffJitterMs: 300,
  };
}

/** Registers the channel's group through the engine, makes PEER a member, and clears both call logs. */
export async function openRegistered(ds: ModelDs, d: Device, channelId: Id = CHANNEL): Promise<Id> {
  const r = await d.engine.openChannel({ communityId: COMMUNITY, channelId, textGroupId: null });
  if (r.state !== 2) throw new Error(`registration left state ${String(r.state)}`);
  await settle();
  ds.join(r.groupId, PEER.device);
  d.core.resetLog();
  ds.resetCalls();
  return r.groupId;
}

/** The names of d's core calls for groupId that are in names, in call order. */
export function coreCalls(d: Device, groupId: Id, names: readonly string[]): string[] {
  const g = toHex(groupId);
  return d.core.calls.filter((c) => c.g === g && names.includes(c.m)).map((c) => c.m);
}

export function catchSync(fn: () => unknown): unknown {
  try {
    fn();
  } catch (e) {
    return e;
  }
  return undefined;
}
