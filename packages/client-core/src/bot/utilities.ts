import { decode, encode, type CborValue } from '../cbor/index.ts';
import type { CoreClient } from '../bridge';
import { BotInteractionBindingMismatchError, BotInteractionUncertainError, createBotInteractionClient, type BotInteractionClient, type BotInteractionResult, type BotInteractionScope } from './interactions.ts';
import { normalizeReminderTimezone } from './utility-time.ts';

const MAX_UINT = (1n << 64n) - 1n;
const MAX_UNIX_SECONDS = 253_402_300_799n;
const encoder = new TextEncoder();
const whiteSpaceOnly = /^\p{White_Space}*$/u;
const ID_HEX = /^[0-9a-f]{32}$/u;
const POLL_DURATIONS = new Set([3600n, 14_400n, 28_800n, 86_400n, 259_200n, 604_800n]);
const KNOWN_OPERATIONS = new Set([0, 1, 2, 3, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25,
  30, 31, 40, 50, 51, 52, 60, 61, 62, 63, 64, 70, 71, 72, 73, 74, 75, 80, 81, 82, 83, 84]);
const POLL_OPERATIONS = new Set([70, 71, 72, 73, 74, 75]);
const REMINDER_OPERATIONS = new Set([80, 81, 82, 83, 84]);
const MUTATION_OPERATIONS = new Set([70, 73, 74, 80, 82, 83]);

export class BotUtilitiesError extends Error {
  constructor(readonly code: string, message = code) {
    super(message);
    this.name = 'BotUtilitiesError';
  }
}


export class BotUtilitiesProviderUnavailableError extends BotUtilitiesError {
  constructor(readonly operation?: number, readonly requestId?: string, message = 'The Bot utility provider is unavailable for this operation.') {
    super('provider-unavailable', message);
    this.name = 'BotUtilitiesProviderUnavailableError';
  }
}
export class BotUtilitiesUncertainMutationError extends BotUtilitiesError {
  readonly uncertain = true;
  constructor(readonly operation: number, readonly status: number | null, readonly requestId: string, readonly cause?: unknown) {
    super('uncertain-mutation', 'The mutation may have been accepted. Reconcile using the same request ID before trying again.');
    this.name = 'BotUtilitiesUncertainMutationError';
  }
}

function uncertainMutation<T>(operation: number, requestId: string, cause: unknown, status: number | null = 0): T {
  throw new BotUtilitiesUncertainMutationError(operation, status, requestId, cause);
}

function decodeAcceptedMutation<T>(operation: number, requestId: string, decodeResult: () => T): T {
  try { return decodeResult(); }
  catch (cause) { return uncertainMutation(operation, requestId, cause); }
}
export class BotUtilitiesStatusError extends BotUtilitiesError {
  constructor(readonly operation: number, readonly status: number, readonly requestId: string, readonly revision: string | null) {
    super('interaction-status', `The Bot utility request failed with status ${status}.`);
    this.name = 'BotUtilitiesStatusError';
  }
}

function fail(code: string, message?: string): never { throw new BotUtilitiesError(code, message ?? code); }
function guarded<T>(fn: () => T): T {
  try { return fn(); } catch (error) {
    if (error instanceof BotUtilitiesError) throw error;
    return fail('invalid-response', 'The Bot returned an invalid utility response.');
  }
}
function cborBytes(value: Uint8Array, maximum = 2400): CborValue[] {
  if (!(value instanceof Uint8Array) || value.length < 1 || value.length > maximum) fail('invalid-response');
  const decoded = decode(value);
  if (!Array.isArray(decoded)) fail('invalid-response');
  return decoded;
}
function fields(value: unknown, exact: number): CborValue[] {
  if (!Array.isArray(value) || value.length !== exact) fail('invalid-response');
  return value as CborValue[];
}
function list(value: unknown, minimum: number, maximum: number): CborValue[] {
  if (!Array.isArray(value) || value.length < minimum || value.length > maximum) fail('invalid-response');
  return value as CborValue[];
}
function uint(value: unknown, min = 0n, max = MAX_UINT): bigint {
  if (typeof value !== 'bigint' || value < min || value > max) fail('invalid-response');
  return value;
}
function readId(value: unknown): string {
  if (!(value instanceof Uint8Array) || value.length !== 16 || value.every(byte => byte === 0)) fail('invalid-response');
  return toHex(value);
}
function bytesFromHex(value: string): Uint8Array {
  if (typeof value !== 'string' || !ID_HEX.test(value) || /^0{32}$/u.test(value)) fail('invalid-id', 'An ID must be 32 lowercase hexadecimal characters and nonzero.');
  const bytes = new Uint8Array(16);
  for (let index = 0; index < 16; index++) bytes[index] = Number.parseInt(value.slice(index * 2, index * 2 + 2), 16);
  return bytes;
}
function toHex(value: Uint8Array): string { return [...value].map(byte => byte.toString(16).padStart(2, '0')).join(''); }
function encodePayload(value: Parameters<typeof encode>[0]): Uint8Array {
  try { return encode(value); } catch { return fail('invalid-payload'); }
}
function sameId(left: string, right: string): boolean { return left === right; }
function readNullableId(value: unknown): string | null { return value === null ? null : readId(value); }
function readText(value: unknown, minimumBytes: number, maximumBytes: number, maximumChars?: number, nonBlank = false): string {
  if (typeof value !== 'string') fail('invalid-response');
  const byteLength = encoder.encode(value).length;
  const charLength = Array.from(value).length;
  if (byteLength < minimumBytes || byteLength > maximumBytes || (maximumChars !== undefined && charLength > maximumChars)
    || (nonBlank && whiteSpaceOnly.test(value))) fail('invalid-response');
  // encode rejects ill-formed UTF-16, unlike TextEncoder which replaces lone surrogates.
  encode(value);
  return value;
}
function versionOne(value: unknown, expectedLength: number): CborValue[] {
  const tuple = fields(value, expectedLength);
  if (uint(tuple[0]) !== 1n) fail('invalid-response');
  return tuple;
}

export type PollSummary = {
  id: string;
  channelId: string;
  threadId: string | null;
  creatorId: string;
  question: string;
  answers: string[];
  multi: boolean;
  createdAt: bigint;
  dueAt: bigint;
  closed: boolean;
  counts: bigint[];
  myChoices: number[] | null;
  revision: bigint;
};

export type ReminderState = 'scheduled' | 'due' | 'cancelled' | 'dismissed';
export type ReminderSummary = {
  id: string;
  text: string;
  dueUnixSeconds: bigint;
  timezone: string;
  sourceChannelId: string | null;
  sourceThreadId: string | null;
  state: ReminderState;
  revision: bigint;
};

export type PollCreatePayloadInput = {
  channelId: string;
  threadId?: string | null;
  question: string;
  answers: readonly string[];
  multi: boolean;
  durationSeconds: number | bigint;
};

export type ReminderCreatePayloadInput = {
  whenKind: 0 | 1;
  when: number | bigint;
  text: string;
  timezone: string;
  sourceChannelId?: string | null;
  sourceThreadId?: string | null;
};

export type UtilityBinding = { installationId: string; botUserId: string };
export type UtilityCapabilities = UtilityBinding & { version: 1; supportedOperations: number[] };
export type PollPage = { items: PollSummary[]; nextCursor: Uint8Array | null };
export type ReminderPage = { items: ReminderSummary[]; nextCursor: Uint8Array | null };
export type PollVotersPage = { voters: string[]; nextCursor: Uint8Array | null };

function validPollText(value: unknown, maxChars: number): value is string {
  return typeof value === 'string' && !whiteSpaceOnly.test(value) && Array.from(value).length <= maxChars;
}
function idText(value: string): string { bytesFromHex(value); return value; }
function integer(value: number | bigint): bigint {
  if (typeof value === 'number') {
    if (!Number.isSafeInteger(value) || value < 0) fail('invalid-payload');
    return BigInt(value);
  }
  if (typeof value !== 'bigint' || value < 0n || value > MAX_UINT) fail('invalid-payload');
  return value;
}

/** Encode the canonical poll-create payload, enforcing field and aggregate byte budgets. */
export function encodePollCreatePayload(input: PollCreatePayloadInput): Uint8Array {
  if (!input || !Array.isArray(input.answers) || typeof input.multi !== 'boolean') fail('invalid-payload');
  const channelId = bytesFromHex(input.channelId);
  const threadId = input.threadId == null ? null : bytesFromHex(input.threadId);
  if (!validPollText(input.question, 300) || Array.from(input.question).length < 1) fail('invalid-poll-question');
  if (input.answers.length < 2 || input.answers.length > 10) fail('invalid-poll-answers');
  const answers = input.answers.map(answer => {
    if (!validPollText(answer, 100) || Array.from(answer).length < 1) fail('invalid-poll-answers');
    return answer;
  });
  if (new Set(answers).size !== answers.length) fail('invalid-poll-answers');
  const duration = integer(input.durationSeconds);
  if (!POLL_DURATIONS.has(duration)) fail('invalid-poll-duration');
  const payload = encodePayload([1n, channelId, threadId, input.question, answers, input.multi ? 1n : 0n, duration]);
  if (payload.length > 1800) fail('poll-payload-too-large', `Poll creation payload is ${payload.length} bytes; the aggregate budget is 1800 bytes.`);
  return payload;
}

/** Encode a canonical poll vote replacement; an empty set withdraws the current vote. */
export function encodePollSelection(selection: readonly number[]): Uint8Array {
  if (!Array.isArray(selection) || selection.length > 10) fail('invalid-poll-selection');
  const normalized: bigint[] = [];
  for (const value of selection) {
    if (!Number.isInteger(value) || value < 0 || value > 9) fail('invalid-poll-selection');
    normalized.push(BigInt(Number(value)));
  }
  if (new Set(normalized).size !== normalized.length) fail('invalid-poll-selection');
  const payload = encodePayload(normalized);
  if (payload.length > 2200) fail('invalid-poll-selection');
  return payload;
}

/** Encode the canonical reminder-create payload; calendar interpretation belongs to parseReminderTime. */
export function encodeReminderCreatePayload(input: ReminderCreatePayloadInput): Uint8Array {
  if (!input || (input.whenKind !== 0 && input.whenKind !== 1)) fail('invalid-payload');
  const when = integer(input.when);
  if ((input.whenKind === 0 && (when < 60n || when > 31_536_000n))
    || (input.whenKind === 1 && (when < 1n || when > MAX_UNIX_SECONDS))) fail('invalid-reminder-time');
  const text = input.text;
  if (typeof text !== 'string' || encoder.encode(text).length < 1 || encoder.encode(text).length > 1500) fail('invalid-reminder-text');
  if (encode(text).length === 0) fail('invalid-reminder-text');
  const timezone = normalizeReminderTimezone(input.timezone);
  if (typeof input.timezone !== 'string' || encoder.encode(input.timezone).length < 1 || encoder.encode(input.timezone).length > 100
    || timezone === null) fail('invalid-reminder-timezone');
  const sourceChannel = input.sourceChannelId == null ? null : bytesFromHex(input.sourceChannelId);
  const sourceThread = input.sourceThreadId == null ? null : bytesFromHex(input.sourceThreadId);
  if (sourceThread !== null && sourceChannel === null) fail('invalid-reminder-source');
  return encodePayload([1n, BigInt(input.whenKind), when, text, timezone, sourceChannel, sourceThread]);
}

export function decodeBotUtilityCapabilities(data: Uint8Array, expected?: Partial<UtilityBinding>): UtilityCapabilities {
  return guarded(() => {
    const [version, installation, bot, generation, rawOps] = versionOne(cborBytes(data), 5);
    const installationId = readId(installation);
    const botUserId = readId(bot);
    if (uint(generation) !== 1n) fail('invalid-response');
    // Structure and types stay strict (at most 32 strictly ascending uint8 codes); a code this client does not
    // know is a newer Bot's operation, so it is dropped rather than rejecting the whole report.
    const reported = list(rawOps, 0, 32).map(value => Number(uint(value, 0n, 255n)));
    for (let index = 1; index < reported.length; index++) if (reported[index] <= reported[index - 1]) fail('invalid-response');
    const operations = reported.filter(n => KNOWN_OPERATIONS.has(n));
    if (expected?.installationId !== undefined && installationId !== idText(expected.installationId)) fail('binding-mismatch');
    if (expected?.botUserId !== undefined && botUserId !== idText(expected.botUserId)) fail('binding-mismatch');
    return { version: Number(uint(version)) as 1, installationId, botUserId, supportedOperations: operations };
  });
}

function readPollSummary(value: unknown): PollSummary {
  const tuple = fields(value, 13);
  const id = readId(tuple[0]);
  const channelId = readId(tuple[1]);
  const threadId = readNullableId(tuple[2]);
  const creatorId = readId(tuple[3]);
  const question = readText(tuple[4], 1, Number.MAX_SAFE_INTEGER, 300, true);
  const rawAnswers = list(tuple[5], 2, 10);
  const answers = rawAnswers.map(answer => readText(answer, 1, Number.MAX_SAFE_INTEGER, 100, true));
  if (new Set(answers).size !== answers.length) fail('invalid-response');
  const multiValue = uint(tuple[6], 0n, 1n);
  const createdAt = uint(tuple[7], 1n, MAX_UNIX_SECONDS);
  const dueAt = uint(tuple[8], 1n, MAX_UNIX_SECONDS);
  const duration = dueAt - createdAt;
  if (!POLL_DURATIONS.has(duration)) fail('invalid-response');
  const closed = uint(tuple[9], 0n, 1n) === 1n;
  const counts = list(tuple[10], answers.length, answers.length).map(count => uint(count));
  let myChoices: number[] | null;
  if (tuple[11] === null) myChoices = null;
  else {
    const selected = list(tuple[11], 0, answers.length).map(choice => Number(uint(choice, 0n, BigInt(answers.length - 1))));
    if (new Set(selected).size !== selected.length || (!multiValue && selected.length > 1)) fail('invalid-response');
    myChoices = selected;
  }
  const revision = uint(tuple[12], 1n, MAX_UINT);
  return { id, channelId, threadId, creatorId, question, answers, multi: multiValue === 1n,
    createdAt, dueAt, closed, counts, myChoices, revision };
}

function expectedPoll(summary: PollSummary, expected?: { pollId?: string; channelId?: string; threadId?: string | null }): void {
  if (expected?.pollId !== undefined && !sameId(summary.id, idText(expected.pollId))) fail('request-mismatch');
  if (expected?.channelId !== undefined && !sameId(summary.channelId, idText(expected.channelId))) fail('context-mismatch');
  if (expected && 'threadId' in expected && summary.threadId !== (expected.threadId === null ? null : idText(expected.threadId!))) fail('context-mismatch');
}
function checkRevision(revision: bigint, expected?: string | bigint | null): void {
  if (expected === undefined) return;
  if (expected === null) fail('revision-mismatch');
  let parsed: bigint;
  try { parsed = typeof expected === 'bigint' ? expected : /^(?:0|[1-9]\d*)$/u.test(expected) ? BigInt(expected) : -1n; }
  catch { parsed = -1n; }
  if (parsed !== revision) fail('revision-mismatch');
}
function summaryEnvelope(data: Uint8Array, expectedId?: string): PollSummary {
  const [, raw] = versionOne(cborBytes(data), 2);
  const summary = readPollSummary(raw);
  if (expectedId !== undefined) expectedPoll(summary, { pollId: expectedId });
  return summary;
}
export type PollResponseExpectation = {
  pollId?: string;
  channelId?: string;
  threadId?: string | null;
  responseRevision?: string | bigint | null;
  myChoices?: readonly number[] | null;
  closed?: boolean;
};
export function decodePollResponse(data: Uint8Array, expected?: PollResponseExpectation): PollSummary {
  return guarded(() => {
    const summary = summaryEnvelope(data, expected?.pollId);
    expectedPoll(summary, expected);
    checkRevision(summary.revision, expected?.responseRevision);
    if (expected?.closed !== undefined && summary.closed !== expected.closed) fail('response-mismatch');
    if (expected && 'myChoices' in expected) {
      const selected = expected.myChoices;
      if (selected === null) {
        if (summary.myChoices !== null) fail('response-mismatch');
      } else if (selected !== undefined && (summary.myChoices === null || summary.myChoices.length !== selected.length
        || summary.myChoices.some((choice, index) => choice !== selected[index]))) fail('response-mismatch');
    }
    return summary;
  });
}
export function decodePollGet(data: Uint8Array, expected?: PollResponseExpectation): PollSummary {
  const summary = decodePollResponse(data, expected);
  if (summary.myChoices === null) fail('invalid-response');
  return summary;
}
export function decodePollCreate(data: Uint8Array, requestId: string, expected?: { channelId?: string; threadId?: string | null; responseRevision?: string | bigint | null }): PollSummary {
  return guarded(() => {
    const summary = summaryEnvelope(data, requestId);
    expectedPoll(summary, expected);
    checkRevision(summary.revision, expected?.responseRevision);
    if (summary.myChoices === null || summary.myChoices.length !== 0) fail('invalid-response');
    return summary;
  });
}

export function decodePollVote(data: Uint8Array, expected: { pollId: string; choices: readonly number[]; responseRevision?: string | bigint | null; channelId?: string; threadId?: string | null }): PollSummary {
  const summary = decodePollResponse(data, { ...expected, myChoices: expected.choices });
  if (summary.myChoices === null) fail('invalid-response');
  return summary;
}
export function decodePollClose(data: Uint8Array, expected: { pollId: string; responseRevision?: string | bigint | null; channelId?: string; threadId?: string | null }): PollSummary {
  const summary = decodePollResponse(data, { ...expected, closed: true });
  if (summary.myChoices === null) fail('invalid-response');
  return summary;
}

function readCursor(value: unknown): Uint8Array | null {
  if (value === null) return null;
  if (!(value instanceof Uint8Array) || value.length < 1 || value.length > 256) fail('invalid-response');
  return value.slice();
}
function checkPollListCursor(cursor: Uint8Array, context: { communityId: string; channelId: string; threadId?: string | null }, last?: PollSummary): void {
  guarded(() => {
    const tuple = versionOne(decode(cursor), 5);
    if (readId(tuple[1]) !== idText(context.communityId) || readId(tuple[2]) !== idText(context.channelId)
      || readNullableId(tuple[3]) !== (context.threadId == null ? null : idText(context.threadId))) fail('cursor-context-mismatch');
    if (!(tuple[4] instanceof Uint8Array)) fail('invalid-response');
    const key = new TextDecoder('utf-8', { fatal: true }).decode(tuple[4]);
    const prefix = 'dilla.bot.poll.v1/';
    const pollId = key.slice(prefix.length);
    if (!key.startsWith(prefix) || !ID_HEX.test(pollId) || /^0{32}$/u.test(pollId)) fail('invalid-response');
    if (last && key !== `${prefix}${last.id}`) fail('cursor-context-mismatch');
  });
}
export function decodePollListPage(data: Uint8Array, context: { communityId: string; channelId: string; threadId?: string | null }): PollPage {
  return guarded(() => {
    const [, rawItems, rawCursor] = versionOne(cborBytes(data), 3);
    const rawRows = list(rawItems, 0, 20);
    const items = rawRows.map(raw => readPollSummary(raw));
    for (const item of items) {
      expectedPoll(item, context);
      if (item.myChoices === null) fail('invalid-response');
    }
    if (items.some((item, index) => index > 0 && item.id <= items[index - 1].id)) fail('invalid-response');
    const nextCursor = readCursor(rawCursor);
    if (nextCursor !== null) {
      if (items.length === 0) fail('invalid-response');
      checkPollListCursor(nextCursor, context, items[items.length - 1]);
    }
    return { items, nextCursor };
  });
}

function checkPollVoterCursor(cursor: Uint8Array, pollId: string, option: number, lastVoter?: string): void {
  guarded(() => {
    const tuple = versionOne(decode(cursor), 4);
    if (readId(tuple[1]) !== idText(pollId) || uint(tuple[2], 0n, 9n) !== BigInt(option)) fail('cursor-context-mismatch');
    if (!(tuple[3] instanceof Uint8Array)) fail('invalid-response');
    const key = new TextDecoder('utf-8', { fatal: true }).decode(tuple[3]);
    const prefix = `dilla.bot.poll-vote.v1/${pollId}/`;
    const userId = key.slice(prefix.length);
    if (!key.startsWith(prefix) || !ID_HEX.test(userId) || /^0{32}$/u.test(userId)
      || (lastVoter !== undefined && userId !== lastVoter)) fail('cursor-context-mismatch');
  });
}

export function decodePollVotersPage(data: Uint8Array, expected: { pollId: string; option: number }): PollVotersPage {
  return guarded(() => {
    const pollId = idText(expected.pollId);
    if (!Number.isInteger(expected.option) || expected.option < 0 || expected.option > 9) fail('invalid-payload');
    const [, rawVoters, rawCursor] = versionOne(cborBytes(data), 3);
    const voters = list(rawVoters, 0, 20).map(readId);
    if (new Set(voters).size !== voters.length || voters.some((voter, index) => index > 0 && voter <= voters[index - 1])) fail('invalid-response');
    const nextCursor = readCursor(rawCursor);
    if (nextCursor !== null) {
      if (voters.length === 0) fail('invalid-response');
      checkPollVoterCursor(nextCursor, pollId, expected.option, voters[voters.length - 1]);
    }
    return { voters, nextCursor };
  });
}

function readReminderSummary(value: unknown): ReminderSummary {
  const tuple = fields(value, 8);
  const id = readId(tuple[0]);
  const text = readText(tuple[1], 1, 1500);
  const dueUnixSeconds = uint(tuple[2], 1n, MAX_UNIX_SECONDS);
  const rawTimezone = readText(tuple[3], 1, 100);
  const timezone = normalizeReminderTimezone(rawTimezone);
  if (timezone === null) fail('invalid-response');
  const sourceChannelId = readNullableId(tuple[4]);
  const sourceThreadId = readNullableId(tuple[5]);
  if (sourceChannelId === null && sourceThreadId !== null) fail('invalid-response');
  const stateValue = uint(tuple[6], 0n, 3n);
  const revision = uint(tuple[7], 1n, MAX_UINT);
  const states: ReminderState[] = ['scheduled', 'due', 'cancelled', 'dismissed'];
  return { id, text, dueUnixSeconds, timezone, sourceChannelId, sourceThreadId, state: states[Number(stateValue)], revision };
}
function reminderEnvelope(data: Uint8Array, expected?: { reminderId?: string; responseRevision?: string | bigint | null }): ReminderSummary {
  const [, raw] = versionOne(cborBytes(data), 2);
  const summary = readReminderSummary(raw);
  if (expected?.reminderId !== undefined && summary.id !== idText(expected.reminderId)) fail('request-mismatch');
  checkRevision(summary.revision, expected?.responseRevision);
  return summary;
}
export function decodeReminderCreate(data: Uint8Array, expected?: { reminderId?: string; responseRevision?: string | bigint | null }): ReminderSummary {
  return guarded(() => {
    const summary = reminderEnvelope(data, expected);
    if (summary.state !== 'scheduled') fail('response-mismatch');
    return summary;
  });
}
export function decodeReminderMutation(data: Uint8Array, reminderId: string, expectedState: 'cancelled' | 'dismissed', responseRevision?: string | bigint | null): ReminderSummary {
  return guarded(() => {
    const summary = reminderEnvelope(data, { reminderId, responseRevision });
    if (summary.state !== expectedState) fail('response-mismatch');
    return summary;
  });
}
export function decodeReminderCancel(data: Uint8Array, reminderId: string, responseRevision?: string | bigint | null): ReminderSummary {
  return decodeReminderMutation(data, reminderId, 'cancelled', responseRevision);
}
export function decodeReminderDismiss(data: Uint8Array, reminderId: string, responseRevision?: string | bigint | null): ReminderSummary {
  return decodeReminderMutation(data, reminderId, 'dismissed', responseRevision);
}

function checkReminderCursor(cursor: Uint8Array, ownerId: string, last?: ReminderSummary): void {
  guarded(() => {
    const value = new TextDecoder('utf-8', { fatal: true }).decode(cursor);
    const match = /^([0-9a-f]{32})\/(\d{20})\/([0-9a-f]{32})$/u.exec(value);
    if (!match || match[1] !== idText(ownerId) || /^0{32}$/u.test(match[3])) fail('cursor-context-mismatch');
    const dueAt = BigInt(match[2]);
    if (dueAt < 1n || dueAt > MAX_UNIX_SECONDS) fail('invalid-response');
    if (last && (match[2] !== last.dueUnixSeconds.toString().padStart(20, '0') || match[3] !== last.id)) fail('cursor-context-mismatch');
  });
}
export function decodeReminderListPage(data: Uint8Array, context: { ownerId: string }): ReminderPage {
  return guarded(() => {
    const ownerId = idText(context.ownerId);
    const [, rawItems, rawCursor] = versionOne(cborBytes(data), 3);
    const items = list(rawItems, 0, 20).map(readReminderSummary);
    if (items.some(item => item.state !== 'scheduled' && item.state !== 'due')) fail('invalid-response');
    const nextCursor = readCursor(rawCursor);
    if (nextCursor !== null) {
      if (items.length === 0) fail('invalid-response');
      checkReminderCursor(nextCursor, ownerId, items[items.length - 1]);
    }
    for (let i = 1; i < items.length; i++) {
      const prev = items[i - 1], current = items[i];
      if (current.dueUnixSeconds < prev.dueUnixSeconds || (current.dueUnixSeconds === prev.dueUnixSeconds && current.id <= prev.id)) fail('invalid-response');
    }
    return { items, nextCursor };
  });
}

export function decodeReminderDuePage(data: Uint8Array, context: { ownerId: string }): ReminderPage {
  return guarded(() => {
    const page = decodeReminderListPage(data, context);
    if (page.items.some(item => item.state !== 'due')) fail('invalid-response');
    return page;
  });
}


export type PollReadOptions = { requestId?: string; channelId: string; threadId?: string | null };
export type PollListInput = { channelId: string; threadId?: string | null; cursor?: Uint8Array | null; requestId?: string };
export type PollVoteInput = { requestId: string; pollId: string; channelId: string; threadId?: string | null; choices: readonly number[]; revision: string | bigint };
export type PollCloseInput = { requestId: string; pollId: string; channelId: string; threadId?: string | null; revision: string | bigint };
export type PollVotersInput = { pollId: string; channelId: string; threadId?: string | null; option: number; cursor?: Uint8Array | null; requestId?: string };
export type PollCreateInput = PollCreatePayloadInput & { requestId: string };
export type ReminderCreateInput = ReminderCreatePayloadInput & { requestId: string };
export type ReminderMutationInput = { reminderId: string; revision: string | bigint; requestId: string };

export type BotUtilities = {
  capabilities(): Promise<UtilityCapabilities>;
  readonly binding: UtilityBinding | null;
  pollCreate(input: PollCreateInput): Promise<PollSummary>;
  pollGet(pollId: string, options: PollReadOptions): Promise<PollSummary>;
  pollList(input: PollListInput): Promise<PollPage>;
  pollVote(input: PollVoteInput): Promise<PollSummary>;
  pollClose(input: PollCloseInput): Promise<PollSummary>;
  pollVoters(input: PollVotersInput): Promise<PollVotersPage>;
  reminderCreate(input: ReminderCreateInput): Promise<ReminderSummary>;
  reminderList(cursor?: Uint8Array | null, requestId?: string): Promise<ReminderPage>;
  reminderCancel(input: ReminderMutationInput): Promise<ReminderSummary>;
  reminderDismiss(input: ReminderMutationInput): Promise<ReminderSummary>;
  reminderDue(cursor?: Uint8Array | null, requestId?: string): Promise<ReminderPage>;
  read(requestId: string, operation: number): Promise<BotInteractionResult | null>;
};

function validateRequestId(value: string): string { bytesFromHex(value); return value; }
function revisionString(value: string | bigint, positive = true): string {
  let revision: bigint;
  if (typeof value === 'bigint') revision = value;
  else if (typeof value === 'string' && /^(?:0|[1-9]\d*)$/u.test(value)) revision = BigInt(value);
  else return fail('invalid-revision');
  if (revision < (positive ? 1n : 0n) || revision > MAX_UINT) fail('invalid-revision');
  return revision.toString();
}
function generatedRequestId(): string {
  const value = new Uint8Array(16);
  crypto.getRandomValues(value);
  if (value.every(byte => byte === 0)) value[15] = 1;
  return toHex(value);
}
function requestIdOrGenerate(value?: string): string { return value === undefined ? generatedRequestId() : validateRequestId(value); }
function normalizeCursor(value?: Uint8Array | null): Uint8Array | null {
  if (value == null) return null;
  if (!(value instanceof Uint8Array) || value.length < 1 || value.length > 256) fail('invalid-cursor');
  return value.slice();
}
function checkNoRevision(result: BotInteractionResult): void { if (result.revision !== null) fail('revision-mismatch'); }
function assertSuccess(result: BotInteractionResult, operation: number): void {
  if (result.status === 0) return;
  if (result.status === 5 && MUTATION_OPERATIONS.has(operation)) {
    throw new BotUtilitiesUncertainMutationError(operation, result.status, result.requestId);
  }
  if (result.status === 5) throw new BotUtilitiesProviderUnavailableError(operation, result.requestId);
  throw new BotUtilitiesStatusError(operation, result.status, result.requestId, result.revision);
}
function responseRequest(result: BotInteractionResult, requestId: string, operation: number): void {
  if (result.requestId !== requestId) fail('request-mismatch');
  assertSuccess(result, operation);
}
function currentUserId(client: CoreClient): string {
  const account = client.get('account');
  const value = account?.user?.id;
  if (typeof value !== 'string') fail('account-unavailable');
  return idText(value);
}

/** Build strict poll/reminder clients on the shared scope-checked interaction transport. */
export function createBotUtilities(client: CoreClient, communityId: string): BotUtilities {
  const community = idText(communityId);
  const transport: BotInteractionClient = createBotInteractionClient(client, community);
  let lastBinding: UtilityBinding | null = null;
  let lastCapabilityScope: BotInteractionScope | null = null;
  const sameScope = (left: BotInteractionScope, right: BotInteractionScope): boolean => left.installationId === right.installationId
    && left.botUserId === right.botUserId && left.revision === right.revision;

  const statusBinding = async (): Promise<UtilityBinding & { revision: string }> => {
    const status = await client.call({ m: 'getBotCommandStatus', communityId: community }) as {
      installationId?: unknown; botUserId?: unknown; state?: unknown; revision?: unknown;
    } | null;
    if (status === null || status.state !== 3 || typeof status.installationId !== 'string' || typeof status.botUserId !== 'string'
      || !ID_HEX.test(status.installationId) || !ID_HEX.test(status.botUserId) || /^0{32}$/u.test(status.installationId)
      || /^0{32}$/u.test(status.botUserId) || typeof status.revision !== 'string' || !/^(?:0|[1-9]\d*)$/u.test(status.revision)) {
      lastBinding = null;
      throw new BotUtilitiesProviderUnavailableError(undefined, undefined, 'The Bot installation is not ready for utilities.');
    }
    return { installationId: status.installationId, botUserId: status.botUserId, revision: status.revision };
  };
  const capabilities = async (): Promise<UtilityCapabilities> => {
    lastBinding = null;
    lastCapabilityScope = null;
    const before = await statusBinding();
    const expectedScope: BotInteractionScope = before;
    const result = await transport.request({ operation: 3, args: [], expectedScope });
    responseRequest(result, result.requestId, 3);
    checkNoRevision(result);
    const caps = decodeBotUtilityCapabilities(result.data, before);
    if (!caps.supportedOperations.includes(3)) fail('invalid-response');
    const after = await statusBinding();
    if (!sameScope(result.scope, before) || !sameScope(before, after)) {
      lastBinding = null;
      throw new BotUtilitiesProviderUnavailableError(3, result.requestId, 'The Bot installation changed while capabilities were loading.');
    }
    lastBinding = { installationId: caps.installationId, botUserId: caps.botUserId };
    lastCapabilityScope = result.scope;
    return caps;
  };
  const authorizeOperation = async (operation: number): Promise<BotInteractionScope> => {
    const caps = await capabilities();
    const supported = operation >= 70 && operation <= 75 ? isPollOperationSupported(caps, operation)
      : operation >= 80 && operation <= 84 ? isReminderOperationSupported(caps, operation) : false;
    if (!supported) throw new BotUtilitiesProviderUnavailableError(operation);
    if (lastCapabilityScope === null) fail('binding-mismatch');
    return { ...lastCapabilityScope };
  };
  const send = async (operation: number, args: readonly (string | number | Uint8Array | null)[], options: {
    requestId?: string; revision?: string | bigint | null;
  } = {}): Promise<BotInteractionResult> => {
    const requestId = requestIdOrGenerate(options.requestId);
    const expectedScope = await authorizeOperation(operation);
    const revision = options.revision === undefined || options.revision === null ? null : revisionString(options.revision, options.revision !== 0n && options.revision !== '0');
    let result: BotInteractionResult;
    try {
      result = await transport.request({ operation, requestId, channelId: null, args, revision, expectedScope });
    } catch (cause) {
      if (cause instanceof BotInteractionBindingMismatchError) {
        lastBinding = null;
        lastCapabilityScope = null;
        fail('binding-mismatch');
      }
      if (cause instanceof BotInteractionUncertainError) {
        lastBinding = null;
        lastCapabilityScope = null;
      }
      if (MUTATION_OPERATIONS.has(operation) && cause instanceof BotInteractionUncertainError) {
        return uncertainMutation(operation, requestId, cause, null);
      }
      throw cause;
    }
    if (!sameScope(result.scope, expectedScope)) {
      lastBinding = null;
      lastCapabilityScope = null;
      if (MUTATION_OPERATIONS.has(operation) && result.status === 0) return uncertainMutation(operation, requestId, 'installation changed after mutation acceptance');
      fail('binding-mismatch');
    }
    try { responseRequest(result, requestId, operation); }
    catch (cause) {
      if (MUTATION_OPERATIONS.has(operation) && result.status === 0) return uncertainMutation(operation, requestId, cause);
      throw cause;
    }
    return result;
  };
  const ownerId = (): string => currentUserId(client);
  const checkReminderSource = (summary: ReminderSummary, input: ReminderCreatePayloadInput): void => {
    const expectedChannel = input.sourceChannelId ?? null;
    const expectedThread = input.sourceThreadId ?? null;
    const exact = summary.sourceChannelId === expectedChannel && summary.sourceThreadId === expectedThread;
    const redacted = summary.sourceChannelId === null && summary.sourceThreadId === null;
    if (!exact && !redacted) fail('context-mismatch');
  };

  return {
    capabilities,
    get binding() { return lastBinding === null ? null : { ...lastBinding }; },
    pollCreate: async (input) => {
      const requestId = validateRequestId(input.requestId);
      const payload = encodePollCreatePayload(input);
      const result = await send(70, [payload], { requestId, revision: 0n });
      return decodeAcceptedMutation(70, requestId, () => decodePollCreate(result.data, requestId, { channelId: input.channelId, threadId: input.threadId ?? null, responseRevision: result.revision }));
    },
    pollGet: async (pollId, options) => {
      const targetId = idText(pollId);
      const channelId = idText(options.channelId);
      const threadId = options.threadId ?? null;
      if (threadId !== null) idText(threadId);
      const result = await send(71, [bytesFromHex(targetId)], { requestId: options.requestId });
      return decodePollGet(result.data, { pollId: targetId, channelId, threadId, responseRevision: result.revision });
    },
    pollList: async (input) => {
      const channelId = idText(input.channelId);
      const threadId = input.threadId ?? null;
      if (threadId !== null) idText(threadId);
      const cursor = normalizeCursor(input.cursor);
      if (cursor !== null) checkPollListCursor(cursor, { communityId: community, channelId, threadId });
      const result = await send(72, [bytesFromHex(channelId), threadId === null ? null : bytesFromHex(threadId), cursor], { requestId: input.requestId });
      checkNoRevision(result);
      return decodePollListPage(result.data, { communityId: community, channelId, threadId });
    },
    pollVote: async (input) => {
      const pollId = idText(input.pollId);
      const context = { channelId: idText(input.channelId), threadId: input.threadId ?? null };
      if (context.threadId !== null) idText(context.threadId);
      const result = await send(73, [bytesFromHex(pollId), encodePollSelection(input.choices)], { requestId: input.requestId, revision: revisionString(input.revision) });
      return decodeAcceptedMutation(73, input.requestId, () => decodePollVote(result.data, { pollId, choices: input.choices, responseRevision: result.revision, ...context }));
    },
    pollClose: async (input) => {
      const pollId = idText(input.pollId);
      const context = { channelId: idText(input.channelId), threadId: input.threadId ?? null };
      if (context.threadId !== null) idText(context.threadId);
      const result = await send(74, [bytesFromHex(pollId)], { requestId: input.requestId, revision: revisionString(input.revision) });
      return decodeAcceptedMutation(74, input.requestId, () => decodePollClose(result.data, { pollId, responseRevision: result.revision, ...context }));
    },
    pollVoters: async (input) => {
      const pollId = idText(input.pollId);
      const channelId = idText(input.channelId);
      const threadId = input.threadId ?? null;
      if (threadId !== null) idText(threadId);
      if (!Number.isInteger(input.option) || input.option < 0 || input.option > 9) fail('invalid-payload');
      const backing = await send(71, [bytesFromHex(pollId)], { requestId: generatedRequestId() });
      const poll = decodePollGet(backing.data, { pollId, channelId, threadId, responseRevision: backing.revision });
      if (!Number.isInteger(input.option) || input.option < 0 || input.option >= poll.answers.length) fail('invalid-payload');
      const cursor = normalizeCursor(input.cursor);
      if (cursor !== null) checkPollVoterCursor(cursor, pollId, input.option);
      const result = await send(75, [bytesFromHex(pollId), input.option, cursor], { requestId: input.requestId });
      checkNoRevision(result);
      return decodePollVotersPage(result.data, { pollId, option: input.option });
    },
    reminderCreate: async (input) => {
      const requestId = validateRequestId(input.requestId);
      const payload = encodeReminderCreatePayload(input);
      const result = await send(80, [payload], { requestId, revision: 0n });
      return decodeAcceptedMutation(80, requestId, () => {
        const summary = decodeReminderCreate(result.data, { reminderId: requestId, responseRevision: result.revision });
        checkReminderSource(summary, input);
        return summary;
      });
    },
    reminderList: async (cursorInput = null, requestIdInput) => {
      const owner = ownerId();
      const cursor = normalizeCursor(cursorInput);
      if (cursor !== null) checkReminderCursor(cursor, owner);
      const result = await send(81, [cursor], { requestId: requestIdInput });
      checkNoRevision(result);
      return decodeReminderListPage(result.data, { ownerId: owner });
    },
    reminderCancel: async (input) => {
      const reminderId = idText(input.reminderId);
      const result = await send(82, [bytesFromHex(reminderId)], { requestId: input.requestId, revision: revisionString(input.revision) });
      return decodeAcceptedMutation(82, input.requestId, () => decodeReminderCancel(result.data, reminderId, result.revision));
    },
    reminderDismiss: async (input) => {
      const reminderId = idText(input.reminderId);
      const result = await send(83, [bytesFromHex(reminderId)], { requestId: input.requestId, revision: revisionString(input.revision) });
      return decodeAcceptedMutation(83, input.requestId, () => decodeReminderDismiss(result.data, reminderId, result.revision));
    },
    reminderDue: async (cursorInput = null, requestIdInput) => {
      const owner = ownerId();
      const cursor = normalizeCursor(cursorInput);
      if (cursor !== null) checkReminderCursor(cursor, owner);
      const result = await send(84, [cursor], { requestId: requestIdInput });
      checkNoRevision(result);
      return decodeReminderDuePage(result.data, { ownerId: owner });
    },
    read: (requestId, operation) => {
      if (![3, ...POLL_OPERATIONS, ...REMINDER_OPERATIONS].includes(operation)) fail('invalid-operation');
      return transport.read(validateRequestId(requestId), operation);
    },
  };
}

export function isPollOperationSupported(capabilities: UtilityCapabilities, operation: number): boolean {
  return POLL_OPERATIONS.has(operation) && capabilities.supportedOperations.includes(operation);
}
export function isReminderOperationSupported(capabilities: UtilityCapabilities, operation: number): boolean {
  return REMINDER_OPERATIONS.has(operation) && capabilities.supportedOperations.includes(operation);
}
