// Bot v2 Task 21b: the app's Bot surfaces (the player and its dock, poll cards and the poll form, the reminders inbox, the
// music provider status, announcements and role panels) read and act through sealed commands, like the picker does.
//
// Each surface still speaks its v1 operation (its arguments, revision and response data are unchanged); this module sends
// it as the sealed operation the Bot's catalog offers for it: the three sealed reads (poll get 91, music state 92,
// reminders due 93), or an app operation, 100 + the v1 operation. A mutation's expected revision goes first in the
// sealed arguments; ids go as 16 bytes. The Bot answers [revision uint|null, data bstr] or a bare code, which comes back
// here as the v1 status the surfaces already handle. Nothing polls: the answer is a pushed frame (op 62).
import type { CoreClient } from '../bridge';
import { bin, decode, u53, type CborValue } from '../cbor';
import { fromHex } from '../hex';
import { botCommands, type BotAnswer, type BotArg, type BotCommandRun, type createBotCommands } from './commands';

const ID = /^[0-9a-f]{32}$/u;
/** What an app operation adds to its v1 operation (the Bot's protocol.OpApp*). */
export const APP_OPERATION_BASE = 100;
/** The three sealed reads Task 15 defined, by their v1 operation. */
const READS: ReadonlyMap<number, number> = new Map([[71, 91], [10, 92], [84, 93]]);
const READ_OF: ReadonlyMap<number, number> = new Map([...READS].map(([v1, sealed]) => [sealed, v1]));
/** The v1 operations a surface sends that change state: their sealed arguments start with the expected revision. */
const MUTATIONS: ReadonlySet<number> = new Set([2, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 25, 31, 51, 52, 61, 70, 73, 74, 80, 82, 83]);
/** The v1 argument positions that hold an id (sent as 16 bytes when given as hex). */
const ID_ARGS: ReadonlyMap<number, readonly number[]> = new Map([[21, [0]], [25, [0]], [22, [0, 1]], [31, [0]], [51, [0]], [52, [0]], [61, [0]],
  [71, [0]], [72, [0, 1]], [73, [0]], [74, [0]], [75, [0]], [82, [0]], [83, [0]]]);
/** A sealed answer's bare refusal code, as the v1 status it stands for (internal/bot/service sealedRefusal). */
const STATUS_OF_CODE: ReadonlyMap<string, number> = new Map([['E_INVALID', 1], ['E_FORBIDDEN', 2], ['E_BOT_STALE', 3], ['E_CONFLICT', 4],
  ['E_UNAVAILABLE', 5], ['E_BUSY', 6], ['E_NOT_FOUND', 7], ['E_UPGRADE_REQUIRED', 8], ['E_INTERNAL', 9]]);

/** Sealed reads with no v1 operation (90–99: the Bot page's status, 94, Task 22): sent as they are. */
const SEALED_ONLY = (op: number): boolean => op >= 90 && op < APP_OPERATION_BASE;

/** The sealed operation that runs v1 operation `v1` (a sealed-only read is itself). */
export function sealedOperation(v1: number): number { return SEALED_ONLY(v1) ? v1 : READS.get(v1) ?? v1 + APP_OPERATION_BASE; }
/** The v1 operation a sealed read or app operation runs, or null for a picker command. */
export function v1Operation(sealed: number): number | null {
  const read = READ_OF.get(sealed);
  if (read !== undefined) return read;
  return sealed > APP_OPERATION_BASE && sealed < 2 * APP_OPERATION_BASE ? sealed - APP_OPERATION_BASE : null;
}
/** The v1 operations a catalog's commands offer to the app's surfaces, ascending. 84 names the due-reminders read (sealed
 *  93), which has no v1 request any more (Task 23). */
export function supportedV1Operations(commands: readonly { operation: number }[]): number[] {
  const out = new Set<number>();
  for (const command of commands) { const op = v1Operation(command.operation); if (op !== null) out.add(op); }
  return [...out].sort((a, b) => a - b);
}
/** Whether this device's catalog for the community offers v1 operation `v1` to the app. */
export function catalogOffers(client: CoreClient, communityId: string, v1: number): boolean {
  const sealed = sealedOperation(v1);
  return client.get(`botCatalog:${communityId}`)?.commands.some(command => command.operation === sealed) === true;
}

/** `extra` is a sealed read's third element, when it answers one (poll.get's voters: Task 21b fix round 1). */
export interface BotOperationResult { requestId: string; status: number; revision: string | null; data: Uint8Array; extra?: CborValue }
export interface BotOperationRequest {
  operation: number;
  channelId?: string | null;
  args: readonly (string | number | Uint8Array | null)[];
  revision?: string | bigint | null;
  requestId?: string;
}

/** No answer came in time (5 s): the Bot may still have acted. A resend of the same request id (read()) settles it. */
export class BotOperationUncertainError extends Error {
  constructor(readonly requestId: string, message = 'The Bot did not answer in time. Retry to check whether it acted.') {
    super(message);
    this.name = 'BotOperationUncertainError';
  }
}
/** The command never ran: the instance or the Bot refused it (offline, an operation the Bot does not offer, …). */
export class BotOperationRefusedError extends Error {
  constructor(readonly requestId: string, readonly code: string, readonly httpStatus: number) {
    super(`The Bot did not take the request (${code}).`);
    this.name = 'BotOperationRefusedError';
  }
}

/** One answer of an app operation or sealed read: 200 [revision, data], or a refusal's v1 status. Throws on any other. */
export function appOperationAnswer(answer: BotAnswer): { status: number; revision: string | null; data: Uint8Array; extra?: CborValue } {
  if (answer.status === 200) {
    const fields = decode(answer.body);
    if (!Array.isArray(fields) || fields.length < 2 || fields.length > 3) throw new BotOperationRefusedError('', 'E_BOT_ANSWER', 200);
    const [revision, data, extra] = fields;
    const out = { status: 0, revision: revision === null ? null : String(u53(revision)), data: bin(data) };
    return fields.length === 3 ? { ...out, extra } : out;
  }
  const code = new TextDecoder().decode(answer.body);
  const status = STATUS_OF_CODE.get(code);
  if (status === undefined) throw new BotOperationRefusedError('', code, answer.status);
  return { status, revision: null, data: new Uint8Array() };
}

export interface BotOperations {
  request(input: BotOperationRequest): Promise<BotOperationResult>;
  /** Sends a request this page sent before again, with its request id: the Bot answers what it answered then, or runs
   *  it if it never arrived. Null when this page never sent it. */
  read(requestId: string, operation: number): Promise<BotOperationResult | null>;
}

const SENT_LIMIT = 128;

/** Bot operations over a sendBotCommand implementation (the page's, or a test's). */
export function createBotOperationsOn(commands: ReturnType<typeof createBotCommands>, communityId: string): BotOperations {
  if (!ID.test(communityId)) throw new Error('Invalid community id.');
  const sent = new Map<string, { operation: number; channelId: string | null; args: BotArg[] }>();
  // A command can be lost on its way (the Bot's connection was re-established while it was relayed): the first answer that
  // does not come in time is followed by one resend of the same request, which the Bot runs or answers again, never twice
  // (Task 21b fix round 1). Only then is it uncertain.
  const run = async (requestId: string, operation: number, channelId: string | null, args: BotArg[], resent = false): Promise<BotOperationResult> => {
    const handle: BotCommandRun = commands.sendBotCommand({ communityId, channelId, operation, args, requestId });
    for await (const item of handle.answers) {
      if ('kind' in item) {
        if (item.kind === 'timeout') {
          handle.stop();
          if (!resent) return run(requestId, operation, channelId, args, true);
          throw new BotOperationUncertainError(requestId);
        }
        throw new BotOperationRefusedError(requestId, item.code, item.status);
      }
      if (!item.final) continue;
      try { return { requestId, ...appOperationAnswer(item) }; }
      catch (error) {
        if (error instanceof BotOperationRefusedError) throw new BotOperationRefusedError(requestId, error.code, error.httpStatus);
        throw error;
      }
    }
    throw new BotOperationUncertainError(requestId);
  };
  return {
    request: (input) => {
      const requestId = input.requestId ?? newRequestId();
      if (!ID.test(requestId)) return Promise.reject(new Error('Invalid Bot request id.'));
      const ids = ID_ARGS.get(input.operation) ?? [];
      const args: BotArg[] = input.args.map((value, index) => (ids.includes(index) && typeof value === 'string' && ID.test(value) ? fromHex(value) : value));
      if (MUTATIONS.has(input.operation)) {
        const revision = Number(input.revision ?? 0);
        if (!Number.isSafeInteger(revision) || revision < 0) return Promise.reject(new Error('Invalid Bot revision.'));
        args.unshift(revision);
      }
      const operation = sealedOperation(input.operation);
      const channelId = input.channelId ?? null;
      sent.set(requestId, { operation, channelId, args });
      while (sent.size > SENT_LIMIT) sent.delete(sent.keys().next().value!);
      return run(requestId, operation, channelId, args);
    },
    read: (requestId, operation) => {
      const prior = sent.get(requestId);
      if (prior === undefined || prior.operation !== sealedOperation(operation)) return Promise.resolve(null);
      return run(requestId, prior.operation, prior.channelId, prior.args);
    },
  };
}

const shared = new WeakMap<CoreClient, Map<string, BotOperations>>();
/** The page's Bot operations for a community: one per client and community, so read() finds what this page sent. */
export function createBotOperations(client: CoreClient, communityId: string): BotOperations {
  let byCommunity = shared.get(client);
  if (byCommunity === undefined) { byCommunity = new Map(); shared.set(client, byCommunity); }
  let ops = byCommunity.get(communityId);
  if (ops === undefined) { ops = createBotOperationsOn(botCommands(client), communityId); byCommunity.set(communityId, ops); }
  return ops;
}

function newRequestId(): string {
  const bytes = crypto.getRandomValues(new Uint8Array(16));
  if (bytes.every(b => b === 0)) bytes[15] = 1;
  return Array.from(bytes, b => b.toString(16).padStart(2, '0')).join('');
}
