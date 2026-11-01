import type { ApplyResult, ExpectedGroup, GroupInfo, Id } from '../core-port';
import type { SyncDeps } from './engine';
import type { SerialQueues } from './group-queue';

export interface SyncInternals {
  /** The engine's dependencies, including the two web-2b hooks send.ts calls: beforeSend (drainOne, before every upload
   *  of a row with attachments) and onDiscarded (discardSend, with the row as the outbox held it). */
  readonly deps: SyncDeps;
  readonly queues: SerialQueues;
  stopped(): boolean;
  /** Reports a membership change through deps.onMembership, keeping the engine's record of it. */
  reportMembership(groupId: Id, status: 'resyncing' | 'not-member' | 'member'): void;
  row(groupId: Id): GroupInfo | undefined;
  snapshot(groupId: Id): ApplyResult | null;
  expected(): ExpectedGroup[];
  addExpected(g: ExpectedGroup): void;
  catchUpNow(groupId: Id, source: 'catch-up' | 'commit'): Promise<boolean>;
  applyHook(groupId: Id, result: ApplyResult, source: 'frame' | 'catch-up' | 'commit'): Promise<void>;
  requestCatchUp(groupId: Id): void;
  requestDrain(groupId: Id): void;
  canSendInGroup(groupId: Id): boolean;
  requestCommit(groupId: Id, attempt: number): void;
  commitNow(groupId: Id, attempt: number): Promise<boolean>;
  activate(groupId: Id): Promise<void>;
  pollWelcomes(): Promise<void>;
  armTimer(ms: number, fn: () => void): number;
  wait(ms: number): Promise<void>;
  cancelTimer(id: number): void;
  echoWait: Map<string, { msgHex: string; timer: number }>;
  epochWait: Map<string, number>;
  /** CB2: group hex → its slow-mode wait (a 429 requeued a row): the timer and its deadline (ms since the epoch); while
   *  it is armed the group posts nothing. A later 429 only ever moves the deadline later. */
  slowWait: Map<string, { timer: number; until: number }>;
  /** CB2: msg hex of rows a 429 requeued; their confirmation clears the wait (onSlow(g, 0, false)). */
  slowed: Set<string>;
  /** FX-core: msg hex of the rows this engine queued, in the order it queued them. The core lists the outbox by
   *  (created second, random msg_id), so rows queued within one second can swap; drainOne takes this order instead. */
  queued: Set<string>;
  count425: Map<string, number>;
  reframedOnce: Set<string>;
  /** `${group hex}:${msg hex}` of rows a leaf verdict refused at send_encrypt: their one trust refresh is out, or done. */
  trustRetried: Map<string, 'pending' | 'done'>;
  resyncTried: Set<string>;
  /** Advances only after a successful replacement of local membership. */
  readonly recoveryGeneration: Map<string, bigint>;
  quiet: Set<string>;
  /** Unexpected groups already reported to onUnexpectedWelcome; cleared when the expected set changes. */
  unexpectedReported: Set<string>;
}
