import { describe, expect, it } from 'vitest';
import {
  botCommandDefinitions,
  COMMANDS,
  commandAvailability,
  commandById,
  parseCommand,
  searchCommands,
  validateCommandArgs,
  type BotCatalogCommand,
} from './registry';

/** A Bot catalog as the instance sends it (bot/catalog.ts reads it): the Bot's commands are never in COMMANDS. */
const CATALOG: BotCatalogCommand[] = [
  { name: 'play', description: 'Play a song', operation: 12, args: [{ name: 'query-or-url', type: 0, required: true }], permissionBit: 0, needsBotChannel: false, handlerKind: 0 },
  { name: 'queue', description: 'Show the queue', operation: 10, args: [], permissionBit: 0, needsBotChannel: false, handlerKind: 0 },
  { name: 'queue remove', description: 'Remove a track', operation: 21, args: [{ name: 'item', type: 1, required: true }], permissionBit: 0, needsBotChannel: false, handlerKind: 0 },
  { name: 'remind', description: 'Set a reminder', operation: 80, args: [{ name: 'when', type: 4, required: true }, { name: 'text', type: 0, required: true }], permissionBit: 0, needsBotChannel: false, handlerKind: 0 },
  { name: 'poll create', description: 'Ask the channel', operation: 70, args: [{ name: 'question', type: 0, required: true }, { name: 'multi', type: 5, required: false },
    { name: 'channel', type: 3, required: false }, { name: 'for', type: 4, required: false }, { name: 'who', type: 2, required: false }], permissionBit: 0, needsBotChannel: true, handlerKind: 0 },
];
const BOT = botCommandDefinitions(CATALOG);
const ALL = [...COMMANDS, ...BOT];
const bot = (id: string) => BOT.find(command => command.id === id)!;

describe('command registry', () => {
  it('contains unique canonical ids for the built-in commands only (the Bot commands come from its catalog)', () => {
    const ids = COMMANDS.map((command) => command.id);
    expect(new Set(ids).size).toBe(ids.length);
    for (const id of [
      'help', 'me', 'code', 'dm', 'search', 'reply', 'join', 'kick', 'timeout', 'role add', 'selfrole list',
    ]) expect(commandById(id)).toBeDefined();
    for (const id of ['play', 'queue', 'remind', 'poll create', 'reminders list']) expect(commandById(id)).toBeUndefined();
    expect(ids).toHaveLength(35);
    expect(COMMANDS.every(command => command.provider === 'builtin')).toBe(true);
  });

  it('turns a catalog into Bot command definitions, one option per argument shape', () => {
    expect(BOT.map(command => [command.id, command.provider, command.bot?.operation])).toEqual([
      ['play', 'bot', 12], ['queue', 'bot', 10], ['queue remove', 'bot', 21], ['remind', 'bot', 80], ['poll create', 'bot', 70]]);
    expect(bot('play').options).toEqual([{ name: 'query-or-url', kind: 'text', description: 'query-or-url argument', required: true, freeText: true }]);
    expect(bot('queue remove').options[0]).toMatchObject({ kind: 'number', min: 0 });
    // remind's `when` keeps the picker's time field (it resolves to seconds from now when sent).
    expect(bot('remind').options.map(o => o.kind)).toEqual(['time', 'text']);
    expect(bot('poll create').options.map(o => [o.name, o.kind, o.required])).toEqual([
      ['question', 'text', true], ['multi', 'choice', false], ['channel', 'channel', false], ['for', 'duration', false], ['who', 'user', false]]);
    expect(bot('poll create').options[1].choices).toEqual(['on', 'off']);
    expect(bot('poll create').bot).toEqual({ operation: 70, argTypes: [0, 5, 3, 4, 2], needsBotChannel: true, handlerKind: 0 });
    // A text argument named for something this device shows keeps its picker (its id travels as text).
    const [vote] = botCommandDefinitions([{ name: 'poll vote', description: '', operation: 73, args: [{ name: 'poll', type: 0, required: true },
      { name: 'choice', type: 0, required: false }], permissionBit: 0, needsBotChannel: false, handlerKind: 0 }]);
    expect(vote.options.map(o => [o.kind, o.freeText])).toEqual([['poll', undefined], ['text', true]]);
    // A catalog name that shadows a built-in is not offered twice: the built-in stays.
    expect(botCommandDefinitions([{ ...CATALOG[0], name: 'help' }])).toEqual([]);
    // M9: nor can it extend a built-in's first word (`/shrug hello`, `/role list`) and take over its invocation.
    expect(botCommandDefinitions(['shrug hello', 'giphy cat', 'role list', 'selfrole x', 'queue clear'].map(name => ({ ...CATALOG[0], name }))).map(c => c.id))
      .toEqual(['queue clear']);
  });

  it('validates required and optional arguments across schema kinds', () => {
    expect(validateCommandArgs(commandById('dm')!, {})).toMatchObject({ valid: false });
    expect(validateCommandArgs(commandById('dm')!, { user: 'user-123' })).toMatchObject({ valid: true });
    expect(validateCommandArgs(commandById('ban')!, { user: 'user-123', duration: '1h', reason: 'spam' })).toMatchObject({ valid: true });
    expect(validateCommandArgs(bot('queue remove'), { item: '2' })).toMatchObject({ valid: true });
    expect(validateCommandArgs(bot('remind'), { when: '1h', text: 'stretch' })).toMatchObject({ valid: true });
    expect(validateCommandArgs(bot('poll create'), { question: 'Lunch?', multi: 'on' })).toMatchObject({ valid: true });
    expect(validateCommandArgs(bot('poll create'), { question: 'Lunch?', multi: 'maybe' })).toMatchObject({ valid: false });
    expect(commandById('modlog')?.options).toHaveLength(0);
  });

  it('accepts selected entity ids without trying to resolve display names', () => {
    const cases = [
      ['join', { 'voice-channel': 'channel-7' }],
      ['thread', { message: 'message-8' }],
      ['role add', { user: 'user-9', role: 'role-10' }],
    ] as const;
    for (const [id, args] of cases) expect(validateCommandArgs(commandById(id)!, args).valid).toBe(true);
  });

  it('parses free text, named values and quoted Unicode without losing spaces', () => {
    expect(parseCommand('/play jazz 東京', ALL)).toMatchObject({
      type: 'invocation', invocation: { commandId: 'play', args: { 'query-or-url': 'jazz 東京' } },
    });
    expect(parseCommand('/play https://example.test/a:b', ALL)).toMatchObject({ type: 'invocation' });
    expect(parseCommand('/play jazz')).toMatchObject({ type: 'unknown' });
    expect(parseCommand('/remind when:"2026-10-26T09:00" text:"call Åsa"', ALL)).toMatchObject({
      type: 'invocation', invocation: { commandId: 'remind', args: { when: '2026-10-26T09:00', text: 'call Åsa' } },
    });
    expect(parseCommand('/warn user:user-1 reaso:spam')).toMatchObject({ type: 'invalid' });
    expect(parseCommand('/warn user:user-1 reason:spam')).toMatchObject({ type: 'invocation' });
    expect(parseCommand('/warn user:user-1 reason:"see https://example.test/a:b at 09:30"')).toMatchObject({ type: 'invocation' });
    expect(parseCommand('/warn user-1 "note: spam"')).toMatchObject({
      type: 'invocation', invocation: { commandId: 'warn', args: { user: 'user-1', reason: 'note: spam' } },
    });
    expect(parseCommand('/remind 1h "call: Åsa"', ALL)).toMatchObject({
      type: 'invocation', invocation: { commandId: 'remind', args: { when: '1h', text: 'call: Åsa' } },
    });
    expect(parseCommand('/me "say \\"hi\\" and C:\\\\music"')).toMatchObject({
      type: 'invocation', invocation: { args: { text: 'say "hi" and C:\\music' } },
    });
  });

  it('preserves multiline positional and named free text without absorbing named options', () => {
    expect(parseCommand('/search hello scope:all')).toMatchObject({
      type: 'invocation', invocation: { commandId: 'search', args: { query: 'hello', scope: 'all' } },
    });
    expect(parseCommand('/code js first line\nsecond line')).toMatchObject({
      type: 'invocation', invocation: { commandId: 'code', args: { language: 'js', text: 'first line\nsecond line' } },
    });
    expect(parseCommand('/search query:"hello\nworld" scope:all')).toMatchObject({
      type: 'invocation', invocation: { commandId: 'search', args: { query: 'hello\nworld', scope: 'all' } },
    });
  });

  it('labels built-in and bot-backed commands for the picker', () => {
    for (const command of BOT) expect(command.provider).toBe('bot');
    for (const id of ['warn', 'kick', 'ban', 'delete', 'reply', 'join', 'selfrole list', 'selfrole add', 'selfrole remove', 'modlog']) {
      expect(commandById(id)?.provider).toBe('builtin');
    }
  });

  it('validates reminder durations and ISO local date-times without resolving timezone rules', () => {
    const remind = bot('remind');
    expect(validateCommandArgs(remind, { when: 'banana', text: 'call' }).valid).toBe(false);
    expect(validateCommandArgs(remind, { when: '30s', text: 'call' }).valid).toBe(false);
    expect(validateCommandArgs(remind, { when: '366d', text: 'call' }).valid).toBe(false);
    expect(validateCommandArgs(remind, { when: '1m', text: 'call' }).valid).toBe(true);
    expect(validateCommandArgs(remind, { when: '2026-10-26T09:30', text: 'call' }).valid).toBe(true);
    expect(validateCommandArgs(remind, { when: '2026-10-26T09:30+02:00', text: 'call' }).valid).toBe(true);
    expect(validateCommandArgs(remind, { when: '2026-02-30T09:30', text: 'call' }).valid).toBe(false);
    expect(parseCommand('/remind when:banana text:call', ALL)).toMatchObject({ type: 'incomplete' });
  });

  it('filters commands by scope and fails closed when a required bit is unknown', () => {
    const dm = { communityId: null, channelId: 'dm-1' } as const;
    const member = { communityId: 'community-1', channelId: 'channel-1', bits: 0 } as const;
    const moderator = { ...member, bits: 1 << 12 };
    for (const id of ['me', 'code', 'search', 'reply', 'edit', 'pin', 'unpin']) {
      expect(commandAvailability(id, dm).available).toBe(true);
    }
    expect(commandAvailability('giphy', dm).reason).toBe('community-required');
    expect(commandAvailability(bot('queue'), dm).reason).toBe('community-required');
    expect(commandAvailability('pin', { communityId: member.communityId, channelId: member.channelId }).reason).toBe('permission-unknown');
    expect(commandAvailability('pin', member).reason).toBe('permission-denied');
    expect(commandAvailability('pin', { ...member, bits: 1 << 3 }).available).toBe(true);
    expect(commandAvailability('kick', { communityId: member.communityId, channelId: member.channelId }).reason).toBe('permission-unknown');
    expect(commandAvailability('kick', { ...member, bits: undefined }).reason).toBe('permission-unknown');
    expect(commandAvailability('kick', { ...member, bits: 0 }).reason).toBe('permission-denied');
    expect(commandAvailability('kick', moderator).available).toBe(true);
    expect(commandById('delete')?.permissionBit).toBeUndefined();
    // The instance filters the catalog per member: a Bot command carries no bit for the client to check.
    expect(bot('poll create').permissionBit).toBeUndefined();
  });

  it('distinguishes groups, incomplete and unknown commands', () => {
    expect(parseCommand('/queue', ALL)).toMatchObject({ type: 'invocation', invocation: { commandId: 'queue' } });
    expect(parseCommand('/queue remove', ALL)).toMatchObject({ type: 'incomplete', commandId: 'queue remove' });
    expect(parseCommand('/role')).toMatchObject({ type: 'incomplete', commandId: 'role', subcommands: ['add', 'remove'] });
    expect(parseCommand('/not-a-command')).toMatchObject({ type: 'unknown' });
  });

  it('treats only leading slashes as commands and supports // literal escape', () => {
    expect(parseCommand('hello /play jazz')).toMatchObject({ type: 'literal' });
    expect(parseCommand('```\n/play jazz\n```')).toMatchObject({ type: 'literal' });
    expect(parseCommand('//play jazz')).toMatchObject({ type: 'literal', text: '/play jazz' });
  });

  it('rejects malformed quotes, duplicate and unexpected arguments', () => {
    expect(parseCommand('/remind "unfinished', ALL)).toMatchObject({ type: 'invalid' });
    expect(validateCommandArgs(commandById('dm')!, { user: 'one', extra: 'two' })).toMatchObject({ valid: false });
    expect(parseCommand('/dm user:one user:two')).toMatchObject({ type: 'invalid' });
  });

  it('ranks exact, prefix and substring matches deterministically', () => {
    expect(searchCommands('role remove')[0]?.id).toBe('role remove');
    expect(searchCommands('role rem')[0]?.id).toBe('role remove');
    expect(searchCommands('selfrole')).toContainEqual(expect.objectContaining({ id: 'selfrole list' }));
    expect(searchCommands('', 100)).toHaveLength(Math.min(COMMANDS.length, 25));
  });
});


const SEEK = botCommandDefinitions([{ name: 'seek', description: 'Seek', operation: 17, args: [{ name: 'position', type: 0, required: true }],
  permissionBit: 0, needsBotChannel: false, handlerKind: 0 }])[0];
describe('seek position grammar', () => {
  it.each(['0', '90', '1:02', '1:02:03'])('accepts %s', position => {
    expect(validateCommandArgs(SEEK, { position }).valid).toBe(true);
  });
  it.each(['1m', '2026-10-26T12:00', '-1', '1:60', '1:02:60', '1.5', '1::2', '9007199254740991'])('rejects %s', position => {
    expect(validateCommandArgs(SEEK, { position }).valid).toBe(false);
  });
});

it('allows permanent only for ban duration and preserves the argument', () => {
  expect(validateCommandArgs(commandById('ban')!, { user: 'user-123', duration: 'permanent' })).toMatchObject({ valid: true, args: { duration: 'permanent' } });
  expect(validateCommandArgs(commandById('timeout')!, { user: 'user-123', duration: 'permanent' }).valid).toBe(false);
});

// Jonas, 2026-11-05: kick records no reason (kickMember carries none), so its entry takes the user alone; ban keeps its reason.
it('kick takes no reason and ban does', () => {
  const kick = commandById('kick')!;
  expect(kick.options.map(option => option.name)).toEqual(['user']);
  expect(validateCommandArgs(kick, { user: 'someone', reason: 'spam' })).toMatchObject({ valid: false, issues: [{ code: 'unexpected', name: 'reason', message: 'Unexpected argument: reason' }] });
  expect(parseCommand('/kick someone spam')).toMatchObject({ type: 'invalid', issues: [{ code: 'unexpected', message: 'Unexpected argument: spam' }] });
  expect(commandById('ban')!.options.map(option => option.name)).toContain('reason');
});
