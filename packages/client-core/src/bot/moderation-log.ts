// Bot v2 Task 23: the moderation log reads its two pages as sealed app operations (160: automatic cases, 164: manual
// evidence), answered by the Bot in a pushed frame like every other Bot page. They were requests in a per-member
// interaction group before; those groups are gone. The Bot authorises each read from the reader's view-audit-log bit.
import type { CoreClient } from '../bridge';
import { arr, decode } from '../cbor';
import { toHex } from '../hex';
import { createBotOperations, type BotOperations } from './operations';

/** The v1 operations the two pages run (sealed: 100 + each). */
export const MODERATION_CASES = 60;
export const MODERATION_EVIDENCE = 64;
const ID = /^[0-9a-f]{32}$/u;
const MAX_ROWS = 8;

export interface ModerationCase {
  key: string; channelId: string; actorId: string; messageId: string; revision: string;
  reason: string; action: string; excerpt: string; receivedAt: string;
}
/** One page: its rows, and the cursor of the next (hex) when there is one. */
export interface ModerationPage { status: number; cases: ModerationCase[]; nextCursor: string | null }

export class ModerationPageError extends Error {
  constructor(message: string) { super(message); this.name = 'ModerationPageError'; }
}
const invalid = (message: string): never => { throw new ModerationPageError(message); };

/** Decodes a page's data, [1, rows, cursor|null], strictly. Automatic case rows name this community. */
export function decodeModerationPage(operation: typeof MODERATION_CASES | typeof MODERATION_EVIDENCE, data: Uint8Array,
  communityId: string): Omit<ModerationPage, 'status'> {
  let fields: unknown[];
  try { fields = arr(decode(data)); } catch { return invalid('moderation response shape is invalid'); }
  if (fields.length !== 3 || fields[0] !== 1n || !Array.isArray(fields[1]) || (fields[2] !== null && !(fields[2] instanceof Uint8Array))
    || fields[1].length > MAX_ROWS) invalid('moderation response shape is invalid');
  const rows = fields[1] as unknown[];
  const cursor = fields[2] as Uint8Array | null;
  const cases: ModerationCase[] = [];
  for (const value of rows) {
    const row = Array.isArray(value) ? value as unknown[] : invalid('moderation case shape is invalid');
    if (operation === MODERATION_EVIDENCE) {
      if (row.length !== 5 || !(row[0] instanceof Uint8Array) || row[0].length !== 16 || typeof row[1] !== 'string'
        || !['remove', 'warn'].includes(row[1]) || typeof row[2] !== 'string' || row[2].length < 1 || row[2].length > 1024
        || typeof row[3] !== 'string' || row[3].length > 512 || typeof row[4] !== 'bigint') {
        invalid('manual evidence response shape is invalid');
      }
      const evidenceId = toHex(row[0] as Uint8Array);
      cases.push({ key: `manual:${evidenceId}`, channelId: '0'.repeat(32), actorId: '0'.repeat(32), messageId: evidenceId,
        revision: '1', reason: row[2] as string, action: row[1] as string, excerpt: row[3] as string, receivedAt: String(row[4]) });
      continue;
    }
    if (row.length !== 10 || row.slice(0, 5).some(v => typeof v !== 'string' || !ID.test(v))
      || typeof row[5] !== 'bigint' || typeof row[6] !== 'string'
      || !['log', 'remove', 'warn', 'timeout'].includes(String(row[7])) || typeof row[8] !== 'string'
      || typeof row[9] !== 'bigint' || row[8].length > 700) {
      invalid('moderation case shape is invalid');
    }
    if (row[0] !== communityId) invalid('moderation case community changed');
    const messageId = row[4] as string;
    cases.push({ key: `${messageId}:${String(row[5])}`, channelId: row[2] as string, actorId: row[3] as string, messageId,
      revision: String(row[5]), reason: row[6] as string, action: row[7] as string, excerpt: row[8] as string, receivedAt: String(row[9]) });
  }
  return { cases, nextCursor: cursor === null ? null : toHex(cursor) };
}

/** Reads one page of the moderation log. A refusal comes back as its v1 status with no rows. */
export async function readModerationPage(client: CoreClient, communityId: string,
  operation: typeof MODERATION_CASES | typeof MODERATION_EVIDENCE, cursor: string | null,
  operations: BotOperations = createBotOperations(client, communityId)): Promise<ModerationPage> {
  if (cursor !== null && !/^(?:[0-9a-f]{2}){1,256}$/u.test(cursor)) throw new ModerationPageError('moderation cursor is invalid');
  const after = cursor === null ? null : Uint8Array.from(cursor.match(/../gu)!.map(pair => Number.parseInt(pair, 16)));
  const result = await operations.request({ operation, args: [after] });
  if (result.status !== 0) return { status: result.status, cases: [], nextCursor: null };
  return { status: 0, ...decodeModerationPage(operation, result.data, communityId) };
}
