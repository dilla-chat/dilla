// Signal wave B Task 2: the command picker's copy. For the 16 commands the prototype lists (its CMDS), its icon,
// description, field labels and placeholders verbatim; every other registry command keeps its registry description and
// takes its icon from the plan's Q2 table (Jonas, 2026-10-30). The client-core registry is not edited.
import type { CommandDefinition, CommandOption } from '@dilla/client-core';
import type { IconName } from '@dilla/ui';

export type PickerSuggest = 'member' | 'duration' | 'when' | 'provider' | 'choice' | 'command' | 'none';
export interface PickerField { label: string; placeholder: string; suggest: PickerSuggest }
export interface PickerEntry {
  icon: IconName;
  description: string;
  /** The list row's argument line: `<required>`, `[optional]`, or "opens a form". */
  args: string;
  /** "built-in", or the bot's display name. */
  source: string;
  /** `poll create`: picking it opens the poll dialog instead of chips. */
  opensForm: boolean;
  fields: Record<string, PickerField>;
}

interface Override { icon: IconName; description: string; opensForm?: boolean; fields?: Record<string, { label?: string; placeholder?: string }> }

/** The prototype's CMDS, value for value. */
const PROTOTYPE: Readonly<Record<string, Override>> = {
  help: { icon: 'circle-help', description: 'List commands, or explain one', fields: { command: { placeholder: 'optional' } } },
  me: { icon: 'sparkles', description: 'Post an action in italics', fields: { text: { placeholder: 'packs the rope' } } },
  shrug: { icon: 'smile', description: 'Appends ¯\\_(ツ)_/¯ to your message', fields: { text: { placeholder: 'optional' } } },
  play: { icon: 'play', description: 'Play the first YouTube result, or a link, in your voice channel', fields: { 'query-or-url': { placeholder: 'song, video or playlist' } } },
  pause: { icon: 'pause', description: 'Pause playback' },
  resume: { icon: 'play', description: 'Resume playback' },
  skip: { icon: 'skip-forward', description: 'Skip to the next track' },
  stop: { icon: 'square', description: 'Stop and clear the queue' },
  queue: { icon: 'list-music', description: 'Show what plays next' },
  volume: { icon: 'volume-2', description: 'Set playback volume', fields: { level: { label: '0..100', placeholder: '60' } } },
  'poll create': { icon: 'vote', description: 'Ask the channel a question', opensForm: true },
  remind: { icon: 'alarm-clock', description: 'A private one-time reminder', fields: { when: { placeholder: 'in 1 hour' }, text: { placeholder: 'bring two ropes' } } },
  'reminders list': { icon: 'bell', description: 'Open your reminders inbox' },
  kick: { icon: 'user-x', description: 'Remove someone from the server', fields: { user: { placeholder: '' } } },
  ban: { icon: 'ban', description: 'Ban for a while, or for good', fields: { user: { placeholder: '' }, duration: { placeholder: 'permanent' }, reason: { placeholder: 'optional' } } },
  timeout: { icon: 'timer', description: 'Stop someone sending or speaking for a while', fields: { user: { placeholder: '' }, duration: { placeholder: '10 minutes' } } },
  warn: { icon: 'triangle-alert', description: 'A private warning, logged in modlog', fields: { user: { placeholder: '' }, reason: { placeholder: 'what happened' } } },
};

/** The plan's Q2 icons for the registry commands the prototype does not list. */
const ICONS: Readonly<Record<string, IconName>> = {
  tableflip: 'table', unflip: 'table', code: 'code', giphy: 'image', dm: 'message-square', invite: 'user-plus', search: 'search',
  topic: 'hash', thread: 'messages-square', reply: 'reply', edit: 'pencil', delete: 'trash-2', pin: 'pin', unpin: 'pin',
  join: 'volume-2', leave: 'phone-off', mute: 'mic-off', unmute: 'mic', deafen: 'headphone-off', undeafen: 'headphones',
  nowplaying: 'music', seek: 'fast-forward', shuffle: 'shuffle', repeat: 'repeat', untimeout: 'timer-off', unban: 'ban',
  modlog: 'scroll-text',
};
function iconFor(id: string): IconName {
  if (ICONS[id] !== undefined) return ICONS[id];
  if (id.startsWith('queue ') || id === 'music' || id.startsWith('music ')) return 'list-music';
  if (id.startsWith('role ') || id.startsWith('selfrole ') || id === 'rolepanel create') return 'shield';
  if (id.startsWith('announcement ')) return 'megaphone';
  if (id.startsWith('poll ')) return 'vote';
  if (id.startsWith('reminders ')) return 'bell';
  return 'hash';
}

const ENTITY = new Set(['channel', 'role', 'message', 'queue-item', 'poll', 'announcement', 'reminder']);
function suggestFor(command: CommandDefinition, option: CommandOption): PickerSuggest {
  if (command.id === 'help' && option.name === 'command') return 'command';
  if (option.kind === 'user') return 'member';
  if (option.kind === 'duration') return 'duration';
  if (option.kind === 'time') return command.id === 'remind' ? 'when' : 'none';
  if (option.kind === 'choice') return option.choices !== undefined && option.choices.length > 0 ? 'choice' : 'provider';
  if (ENTITY.has(option.kind)) return 'provider';
  if (command.id === 'play' && option.name === 'query-or-url') return 'provider';
  return 'none';
}

/** The chip label: the prototype's (`0..100`), else a bounded number's `min..max`, else the option's name. */
function labelFor(option: CommandOption, override: string | undefined): string {
  if (override !== undefined) return override;
  if (option.kind === 'number' && option.min !== undefined && option.max !== undefined) return `${String(option.min)}..${String(option.max)}`;
  return option.name;
}

export const BUILT_IN_SOURCE = 'built-in';
export const OPENS_A_FORM = 'opens a form';
export const OPTIONAL_PLACEHOLDER = 'optional';

export function pickerEntry(command: CommandDefinition, botName: string): PickerEntry {
  const override = PROTOTYPE[command.id];
  const fields: Record<string, PickerField> = {};
  for (const option of command.options) {
    const field = override?.fields?.[option.name];
    fields[option.name] = {
      label: labelFor(option, field?.label),
      placeholder: field?.placeholder ?? (option.required ? '' : OPTIONAL_PLACEHOLDER),
      suggest: suggestFor(command, option),
    };
  }
  const opensForm = override?.opensForm === true;
  const args = opensForm ? OPENS_A_FORM : command.options.map(option => {
    const label = fields[option.name].label;
    return option.required ? `<${label}>` : `[${label}]`;
  }).join(' ');
  return {
    icon: override?.icon ?? iconFor(command.id),
    description: override?.description ?? command.description,
    args,
    source: command.provider === 'builtin' ? BUILT_IN_SOURCE : botName,
    opensForm,
    fields,
  };
}

/** Bar suffixes (prototype `cap`): music commands say where Glenn plays, moderation says it is checked. */
export const MUSIC_COMMANDS: ReadonlySet<string> = new Set(['play', 'pause', 'resume', 'skip', 'stop', 'queue', 'nowplaying', 'seek', 'volume', 'shuffle',
  'repeat', 'queue remove', 'queue move', 'queue clear', 'music', 'music help']);
export const MODERATION_COMMANDS: ReadonlySet<string> = new Set(['kick', 'ban', 'unban', 'timeout', 'untimeout', 'warn']);

/** The prototype's duration suggestions (`/ban` adds "permanent"). */
export const DURATION_SUGGESTIONS = ['10 minutes', '1 hour', '1 day', '7 days'] as const;
export const PERMANENT = 'permanent';
/** The prototype's `when` suggestions. */
export const WHEN_SUGGESTIONS = ['in 1 minute', 'in 10 minutes', 'in 1 hour', 'tonight 20:00', 'tomorrow 09:00'] as const;

/**
 * A spoken duration ("10 minutes", "1 hour", "7 days", or the registry's "10m") as the registry's form ("10m"); null when
 * it is neither. "permanent" is the caller's (it drops the argument).
 */
export function registryDuration(value: string): string | null {
  const text = value.trim().toLowerCase();
  if (/^(?:\d+(?:\.\d+)?(?:ms|s|m|h|d|w))+$/u.test(text)) return text;
  const match = /^(\d+)\s*(seconds?|secs?|minutes?|mins?|hours?|hrs?|days?|weeks?|wks?)$/u.exec(text);
  if (match === null) return null;
  const unit = match[2].startsWith('s') ? 's' : match[2].startsWith('m') ? 'm' : match[2].startsWith('h') ? 'h' : match[2].startsWith('d') ? 'd' : 'w';
  return `${match[1]}${unit}`;
}
