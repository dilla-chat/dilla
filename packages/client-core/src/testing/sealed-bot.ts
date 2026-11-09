// A test Bot behind sealed commands (Bot v2 Task 21b): it takes the page's `sendBotCommand` calls, hands each to the test as
// the v1 operation it runs (its revision split off, its ids as bytes), and answers through the `botAnswers` slice as the
// worker would publish an opened op 62.
import { encode } from '../cbor';
import { toHex } from '../hex';
import { sealedOperation, v1Operation } from '../bot/operations';
import type { BotAnswerView } from '../bot/commands';

const MUTATIONS = new Set([2, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 25, 31, 51, 52, 61, 70, 73, 74, 80, 82, 83]);
const CODES: Record<number, [number, string]> = { 1: [400, 'E_INVALID'], 2: [403, 'E_FORBIDDEN'], 3: [408, 'E_BOT_STALE'], 4: [409, 'E_CONFLICT'],
  5: [503, 'E_UNAVAILABLE'], 6: [429, 'E_BUSY'], 7: [404, 'E_NOT_FOUND'], 8: [426, 'E_UPGRADE_REQUIRED'], 9: [500, 'E_INTERNAL'] };

export interface SealedCall {
  requestId: string; communityId: string; channelId: string | null;
  /** The sealed operation sent and the v1 operation it runs. */
  sealed: number; operation: number;
  /** A mutation's expected revision (the first sealed argument), else null. */
  revision: string | null;
  /** The v1 arguments. */
  args: unknown[];
}
export type SealedReply = { status: number; revision?: string | null; data?: Uint8Array; extra?: unknown } | null;

/** The v1 operations the test catalog offers to the app (as sealed operations). */
export const APP_SURFACE_OPERATIONS = [1, 2, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 31, 50, 51, 52,
  61, 70, 71, 72, 73, 74, 75, 80, 81, 82, 83, 84];

export function sealedBotFake(respond: (call: SealedCall) => SealedReply | Promise<SealedReply>, options: { offered?: readonly number[] } = {}) {
  const calls: SealedCall[] = [];
  const answers: BotAnswerView[] = [];
  const listeners = new Set<() => void>();
  const replies = new Map<string, Exclude<SealedReply, null>>();
  let n = 0;
  const offered = options.offered ?? APP_SURFACE_OPERATIONS;
  const answer = (requestId: string, reply: Exclude<SealedReply, null>): void => {
    const [status, body] = reply.status === 0
      ? [200, encode([reply.revision === undefined || reply.revision === null ? null : BigInt(reply.revision), reply.data ?? encode([]),
        ...(reply.extra === undefined ? [] : [reply.extra as never])])]
      : [CODES[reply.status]?.[0] ?? 500, new TextEncoder().encode(CODES[reply.status]?.[1] ?? 'E_INTERNAL')];
    answers.push({ n: ++n, requestId, seq: 0, status, body: toHex(body), final: true });
    for (const listener of [...listeners]) listener();
  };
  return {
    calls,
    answer,
    /** The fake's part of `client.call`: undefined for a command it does not take. */
    call(command: Record<string, unknown>): Promise<unknown> | undefined {
      if (command.m === 'syncBotCatalog' || command.m === 'forgetBotCommand') return Promise.resolve(null);
      if (command.m !== 'sendBotCommand') return undefined;
      const sealed = command.operation as number;
      const operation = v1Operation(sealed) ?? sealed;
      const sent = [...(command.args as unknown[])];
      const revision = MUTATIONS.has(operation) && v1Operation(sealed) !== null ? String(sent.shift()) : null;
      const call: SealedCall = { requestId: command.requestId as string, communityId: command.communityId as string,
        channelId: command.channelId as string | null, sealed, operation, revision, args: sent };
      calls.push(call);
      // The Bot keeps each request's answers for five minutes: a resend of the same id is answered again, never run twice.
      const kept = replies.get(call.requestId);
      if (kept !== undefined) { void Promise.resolve().then(() => { answer(call.requestId, kept); }); return Promise.resolve(null); }
      void Promise.resolve().then(() => respond(call)).then(reply => {
        if (reply === null) return;
        replies.set(call.requestId, reply); answer(call.requestId, reply);
      });
      return Promise.resolve(null);
    },
    get(key: string): unknown {
      if (key === 'botAnswers') return answers;
      if (key.startsWith('botCatalog:')) {
        return { revision: '1', keyId: null, fingerprint: null, status: { state: 'ready', since: null }, botChannels: [], botUserId: null,
          commands: offered.map(op => ({ name: `op ${op}`, description: '', operation: sealedOperation(op), args: [], permissionBit: 0,
            needsBotChannel: false, handlerKind: 1 })) };
      }
      return undefined;
    },
    subscribe(key: string, listener: () => void): (() => void) | undefined {
      if (key !== 'botAnswers') return undefined;
      listeners.add(listener);
      return () => { listeners.delete(listener); };
    },
  };
}
