import { describe, expect, it } from 'vitest';
import { COMMANDS, commandById } from '@dilla/client-core';
import { pickerEntry, registryDuration } from './picker-catalog.ts';

const entry = (id: string) => pickerEntry(commandById(id)!, 'Glenn');

describe('picker catalog', () => {
  it('carries the prototype copy for the commands it lists', () => {
    expect(entry('help')).toMatchObject({ icon: 'circle-help', description: 'List commands, or explain one', args: '[command]', source: 'built-in' });
    expect(entry('help').fields.command).toEqual({ label: 'command', placeholder: 'optional', suggest: 'command' });
    expect(entry('me')).toMatchObject({ icon: 'sparkles', description: 'Post an action in italics', args: '<text>' });
    expect(entry('me').fields.text.placeholder).toBe('packs the rope');
    expect(entry('shrug')).toMatchObject({ icon: 'smile', description: 'Appends ¯\\_(ツ)_/¯ to your message', args: '[text]' });
    expect(entry('play')).toMatchObject({ icon: 'play', description: 'Play the first YouTube result, or a link, in your voice channel', source: 'Glenn' });
    expect(entry('play').fields['query-or-url']).toEqual({ label: 'query-or-url', placeholder: 'song, video or playlist', suggest: 'provider' });
    expect(entry('pause')).toMatchObject({ icon: 'pause', description: 'Pause playback', args: '' });
    expect(entry('resume')).toMatchObject({ icon: 'play', description: 'Resume playback' });
    expect(entry('skip')).toMatchObject({ icon: 'skip-forward', description: 'Skip to the next track' });
    expect(entry('stop')).toMatchObject({ icon: 'square', description: 'Stop and clear the queue' });
    expect(entry('queue')).toMatchObject({ icon: 'list-music', description: 'Show what plays next' });
    expect(entry('volume')).toMatchObject({ icon: 'volume-2', description: 'Set playback volume', args: '<0..100>' });
    expect(entry('volume').fields.level).toEqual({ label: '0..100', placeholder: '60', suggest: 'none' });
    expect(entry('poll create')).toMatchObject({ icon: 'vote', description: 'Ask the channel a question', args: 'opens a form', opensForm: true });
    expect(entry('remind')).toMatchObject({ icon: 'alarm-clock', description: 'A private one-time reminder', args: '<when> <text>' });
    expect(entry('remind').fields).toEqual({ when: { label: 'when', placeholder: 'in 1 hour', suggest: 'when' }, text: { label: 'text', placeholder: 'bring two ropes', suggest: 'none' } });
    expect(entry('reminders list')).toMatchObject({ icon: 'bell', description: 'Open your reminders inbox' });
    expect(entry('kick')).toMatchObject({ icon: 'user-x', description: 'Remove someone from the server', args: '<user>' });
    expect(entry('ban')).toMatchObject({ icon: 'ban', description: 'Ban for a while, or for good', args: '<user> [duration] [reason]' });
    expect(entry('ban').fields.duration).toEqual({ label: 'duration', placeholder: 'permanent', suggest: 'duration' });
    expect(entry('timeout')).toMatchObject({ icon: 'timer', description: 'Stop someone sending or speaking for a while', args: '<user> <duration>' });
    expect(entry('timeout').fields.duration.placeholder).toBe('10 minutes');
    expect(entry('warn')).toMatchObject({ icon: 'triangle-alert', description: 'A private warning, logged in modlog' });
    expect(entry('warn').fields.reason.placeholder).toBe('what happened');
  });

  it('keeps the registry description elsewhere, with the Q2 icon', () => {
    expect(entry('tableflip')).toMatchObject({ icon: 'table', description: 'Flip a table' });
    expect(entry('dm')).toMatchObject({ icon: 'message-square', description: 'Open a direct message' });
    expect(entry('seek')).toMatchObject({ icon: 'fast-forward' });
    expect(entry('queue move')).toMatchObject({ icon: 'list-music', args: '<item> <position>' });
    expect(entry('role add')).toMatchObject({ icon: 'shield' });
    expect(entry('announcement edit')).toMatchObject({ icon: 'megaphone' });
    expect(entry('reminders cancel')).toMatchObject({ icon: 'bell' });
    expect(entry('untimeout')).toMatchObject({ icon: 'timer-off' });
  });

  // `icon` is an IconName (typed against icons.ts), so every icon here is a vendored one.
  it('gives every registry command an icon and a description', () => {
    for (const command of COMMANDS) {
      const picked = pickerEntry(command, 'Glenn');
      expect(picked.icon).not.toBe('');
      expect(picked.description).not.toBe('');
      expect(picked.source).toBe(command.provider === 'builtin' ? 'built-in' : 'Glenn');
    }
  });

  it('reads spoken durations as the registry form', () => {
    expect(registryDuration('10 minutes')).toBe('10m');
    expect(registryDuration('1 hour')).toBe('1h');
    expect(registryDuration('7 days')).toBe('7d');
    expect(registryDuration('2h')).toBe('2h');
    expect(registryDuration('soon')).toBeNull();
  });
});
