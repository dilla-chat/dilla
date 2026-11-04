import { describe, expect, it } from 'vitest';
import { COMMANDS, commandById } from '@dilla/client-core';
import { COMMAND_OPERATIONS, commandListed } from './capabilities.ts';

const command = (id: string) => commandById(id)!;

describe('command capabilities', () => {
  it('hides poll create without operation 70 and keeps poll vote with 73', () => {
    const ops = new Set([71, 73, 74, 80, 81]);
    expect(commandListed(command('poll create'), ops)).toBe(false);
    expect(commandListed(command('poll vote'), ops)).toBe(true);
    expect(commandListed(command('remind'), ops)).toBe(true);
  });

  it('hides every Bot command while the operations are unknown, never a built-in', () => {
    for (const entry of COMMANDS) expect(commandListed(entry, null)).toBe(entry.provider !== 'bot');
  });

  it('names an operation for every Bot command', () => {
    for (const entry of COMMANDS.filter(row => row.provider === 'bot')) expect(COMMAND_OPERATIONS[entry.id]).toBeTypeOf('number');
    expect(COMMAND_OPERATIONS).toMatchObject({ play: 12, pause: 13, resume: 14, skip: 15, stop: 16, queue: 10, nowplaying: 10, seek: 17, volume: 18,
      shuffle: 19, repeat: 20, 'queue remove': 21, 'queue move': 22, 'queue clear': 23, music: 10, 'music help': 10, 'rolepanel create': 31,
      'announcement list': 50, 'announcement create': 51, 'announcement edit': 51, 'announcement delete': 52,
      'poll create': 70, 'poll results': 71, 'poll vote': 73, 'poll close': 74, remind: 80, 'reminders list': 81, 'reminders cancel': 82, 'reminders dismiss': 83 });
  });
});
