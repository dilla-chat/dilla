import { describe, expect, it } from 'vitest';
import {
  COMMANDS,
  commandAvailability,
  commandById,
  parseCommand,
  searchCommands,
  validateCommandArgs,
} from './registry';

describe('command registry', () => {
  it('contains unique canonical ids for the complete approved catalog', () => {
    const ids = COMMANDS.map((command) => command.id);
    expect(new Set(ids).size).toBe(ids.length);
    for (const id of [
      'help', 'me', 'code', 'dm', 'search', 'reply', 'join', 'play', 'queue',
      'queue remove', 'queue move', 'queue clear', 'kick', 'timeout', 'role add',
      'selfrole list', 'announcement create', 'poll vote', 'remind', 'reminders dismiss',
    ]) expect(commandById(id)).toBeDefined();
    expect(ids).toHaveLength(64);
  });

  it('validates required and optional arguments across schema kinds', () => {
    expect(validateCommandArgs(commandById('dm')!, {})).toMatchObject({ valid: false });
    expect(validateCommandArgs(commandById('dm')!, { user: 'user-123' })).toMatchObject({ valid: true });
    expect(validateCommandArgs(commandById('ban')!, { user: 'user-123', duration: '1h', reason: 'spam' })).toMatchObject({ valid: true });
    expect(validateCommandArgs(commandById('shuffle')!, { mode: 'on' })).toMatchObject({ valid: true });
    expect(validateCommandArgs(commandById('volume')!, { level: '101' })).toMatchObject({ valid: false });
    expect(validateCommandArgs(commandById('queue move')!, { item: 'track-id', position: '2' })).toMatchObject({ valid: true });
    expect(validateCommandArgs(commandById('remind')!, { when: '1h', text: 'stretch' })).toMatchObject({ valid: true });
    expect(validateCommandArgs(commandById('poll vote')!, { poll: 'poll-1' })).toMatchObject({ valid: true });
    expect(validateCommandArgs(commandById('poll create')!, { question: 'Lunch?', answers: 'Soup, salad' })).toMatchObject({ valid: true });
    expect(validateCommandArgs(commandById('announcement create')!, {}).valid).toBe(true);
    expect(commandById('modlog')?.options).toHaveLength(0);
  });

  it('accepts selected entity ids without trying to resolve display names', () => {
    const cases = [
      ['join', { 'voice-channel': 'channel-7' }],
      ['thread', { message: 'message-8' }],
      ['role add', { user: 'user-9', role: 'role-10' }],
      ['poll results', { poll: 'poll-11' }],
      ['announcement delete', { announcement: 'announcement-12' }],
      ['reminders cancel', { reminder: 'reminder-13' }],
    ] as const;
    for (const [id, args] of cases) expect(validateCommandArgs(commandById(id)!, args).valid).toBe(true);
  });

  it('parses free text, named values and quoted Unicode without losing spaces', () => {
    expect(parseCommand('/play jazz 東京')).toMatchObject({
      type: 'invocation', invocation: { commandId: 'play', args: { 'query-or-url': 'jazz 東京' } },
    });
    expect(parseCommand('/play https://example.test/a:b')).toMatchObject({ type: 'invocation' });
    expect(parseCommand('/remind when:"2026-10-26T09:00" text:"call Åsa"')).toMatchObject({
      type: 'invocation', invocation: { commandId: 'remind', args: { when: '2026-10-26T09:00', text: 'call Åsa' } },
    });
    expect(parseCommand('/warn user:user-1 reaso:spam')).toMatchObject({ type: 'invalid' });
    expect(parseCommand('/warn user:user-1 reason:spam')).toMatchObject({ type: 'invocation' });
    expect(parseCommand('/warn user:user-1 reason:"see https://example.test/a:b at 09:30"')).toMatchObject({ type: 'invocation' });
    expect(parseCommand('/warn user-1 "note: spam"')).toMatchObject({
      type: 'invocation', invocation: { commandId: 'warn', args: { user: 'user-1', reason: 'note: spam' } },
    });
    expect(parseCommand('/remind 1h "call: Åsa"')).toMatchObject({
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
    for (const id of ['play', 'queue remove', 'music', 'announcement create', 'rolepanel create', 'poll vote', 'remind']) {
      expect(commandById(id)?.provider).toBe('bot');
    }
    for (const id of ['warn', 'kick', 'ban', 'delete', 'reply', 'join', 'selfrole list', 'selfrole add', 'selfrole remove', 'modlog']) {
      expect(commandById(id)?.provider).toBe('builtin');
    }
  });

  it('validates reminder durations and ISO local date-times without resolving timezone rules', () => {
    const remind = commandById('remind')!;
    expect(validateCommandArgs(remind, { when: 'banana', text: 'call' }).valid).toBe(false);
    expect(validateCommandArgs(remind, { when: '30s', text: 'call' }).valid).toBe(false);
    expect(validateCommandArgs(remind, { when: '366d', text: 'call' }).valid).toBe(false);
    expect(validateCommandArgs(remind, { when: '1m', text: 'call' }).valid).toBe(true);
    expect(validateCommandArgs(remind, { when: '2026-10-26T09:30', text: 'call' }).valid).toBe(true);
    expect(validateCommandArgs(remind, { when: '2026-10-26T09:30+02:00', text: 'call' }).valid).toBe(true);
    expect(validateCommandArgs(remind, { when: '2026-02-30T09:30', text: 'call' }).valid).toBe(false);
    expect(parseCommand('/remind when:banana text:call')).toMatchObject({ type: 'incomplete' });
  });

  it('filters commands by scope and fails closed when a required bit is unknown', () => {
    const dm = { communityId: null, channelId: 'dm-1' } as const;
    const member = { communityId: 'community-1', channelId: 'channel-1', bits: 0 } as const;
    const moderator = { ...member, bits: 1 << 12 };
    for (const id of ['me', 'code', 'search', 'reply', 'edit', 'pin', 'unpin']) {
      expect(commandAvailability(id, dm).available).toBe(true);
    }
    expect(commandAvailability('giphy', dm).reason).toBe('community-required');
    expect(commandAvailability('reminders list', dm).reason).toBe('community-required');
    expect(commandAvailability('pin', { communityId: member.communityId, channelId: member.channelId }).reason).toBe('permission-unknown');
    expect(commandAvailability('pin', member).reason).toBe('permission-denied');
    expect(commandAvailability('pin', { ...member, bits: 1 << 3 }).available).toBe(true);
    expect(commandAvailability('kick', { communityId: member.communityId, channelId: member.channelId }).reason).toBe('permission-unknown');
    expect(commandAvailability('kick', { ...member, bits: undefined }).reason).toBe('permission-unknown');
    expect(commandAvailability('kick', { ...member, bits: 0 }).reason).toBe('permission-denied');
    expect(commandAvailability('kick', moderator).available).toBe(true);
    expect(commandById('delete')?.permissionBit).toBeUndefined();
    expect(commandById('poll close')?.permissionBit).toBeUndefined();
  });

  it('supports canonical /music help and the music help alias', () => {
    expect(parseCommand('/music')).toMatchObject({ type: 'invocation', invocation: { commandId: 'music' } });
    expect(parseCommand('/music help')).toMatchObject({ type: 'invocation', invocation: { commandId: 'music help' } });
  });

  it('distinguishes groups, incomplete and unknown commands', () => {
    expect(parseCommand('/queue')).toMatchObject({ type: 'invocation', invocation: { commandId: 'queue' } });
    expect(parseCommand('/queue remove')).toMatchObject({ type: 'incomplete', commandId: 'queue remove' });
    expect(parseCommand('/queue clear')).toMatchObject({ type: 'invocation', invocation: { commandId: 'queue clear' } });
    expect(parseCommand('/role')).toMatchObject({ type: 'incomplete', commandId: 'role', subcommands: ['add', 'remove'] });
    expect(parseCommand('/not-a-command')).toMatchObject({ type: 'unknown' });
  });

  it('treats only leading slashes as commands and supports // literal escape', () => {
    expect(parseCommand('hello /play jazz')).toMatchObject({ type: 'literal' });
    expect(parseCommand('```\n/play jazz\n```')).toMatchObject({ type: 'literal' });
    expect(parseCommand('//play jazz')).toMatchObject({ type: 'literal', text: '/play jazz' });
  });

  it('rejects malformed quotes, duplicate and unexpected arguments', () => {
    expect(parseCommand('/remind "unfinished')).toMatchObject({ type: 'invalid' });
    expect(validateCommandArgs(commandById('dm')!, { user: 'one', extra: 'two' })).toMatchObject({ valid: false });
    expect(parseCommand('/dm user:one user:two')).toMatchObject({ type: 'invalid' });
  });

  it('ranks exact, prefix and substring matches deterministically', () => {
    expect(searchCommands('queue remove')[0]?.id).toBe('queue remove');
    expect(searchCommands('queue rem')[0]?.id).toBe('queue remove');
    expect(searchCommands('remind')).toContainEqual(expect.objectContaining({ id: 'remind' }));
    expect(searchCommands('', 100)).toHaveLength(Math.min(COMMANDS.length, 25));
  });
});


describe('seek position grammar', () => {
  it.each(['0', '90', '1:02', '1:02:03'])('accepts %s', position => {
    expect(validateCommandArgs(commandById('seek')!, { position }).valid).toBe(true);
  });
  it.each(['1m', '2026-10-26T12:00', '-1', '1:60', '1:02:60', '1.5', '1::2', '9007199254740991'])('rejects %s', position => {
    expect(validateCommandArgs(commandById('seek')!, { position }).valid).toBe(false);
  });
});
