/** Typed, UI-independent command catalog and slash argument utilities. */

export type CommandArgumentKind =
  | 'text' | 'user' | 'channel' | 'role' | 'message' | 'queue-item' | 'poll'
  | 'announcement' | 'reminder' | 'choice' | 'number' | 'duration' | 'time';
export type CommandProvider = 'builtin' | 'bot';

export interface CommandOption {
  name: string;
  kind: CommandArgumentKind;
  description: string;
  required: boolean;
  /** A free-text tail consumes all remaining positional tokens. */
  freeText?: boolean;
  choices?: readonly string[];
  min?: number;
  max?: number;
}

export interface CommandDefinition {
  /** Canonical, space-separated command path. */
  id: string;
  description: string;
  scope: 'any' | 'community';
  provider: CommandProvider;
  handler: string;
  permissionBit?: number;
  options: readonly CommandOption[];
}

export interface CommandContext {
  communityId: string | null;
  channelId: string;
  threadId?: string;
  voiceChannelId?: string | null;
  bits?: number;
}

export interface CommandInvocation {
  commandId: string;
  args: Record<string, string>;
}

export interface CommandResult {
  status: 'pending' | 'success' | 'failure' | 'uncertain';
  message?: string;
  private: boolean;
}

export interface ArgumentIssue {
  code: 'missing' | 'unexpected' | 'empty' | 'invalid' | 'duplicate';
  name: string;
  message: string;
}

export interface CommandValidation {
  valid: boolean;
  issues: ArgumentIssue[];
  args: Record<string, string>;
}

type OptionSeed = Pick<CommandOption, 'name' | 'kind' | 'required'> & Partial<Pick<CommandOption, 'description' | 'freeText' | 'choices' | 'min' | 'max'>>;
const o = (name: string, kind: CommandArgumentKind, required = true, extra: Partial<CommandOption> = {}): OptionSeed => ({ name, kind, required, ...extra });
const text = (name = 'text', required = true, freeText = true): OptionSeed => o(name, 'text', required, { freeText });
const entity = (name: string, kind: CommandArgumentKind, required = true): OptionSeed => o(name, kind, required);
const choice = (name: string, choices: readonly string[], required = true): OptionSeed => o(name, 'choice', required, { choices });

const seeds: Array<{ id: string; description: string; scope?: 'any' | 'community'; permissionBit?: number; options?: OptionSeed[] }> = [
  { id: 'help', description: 'Show command help', scope: 'any', options: [o('command', 'text', false)] },
  { id: 'me', description: 'Send an action message', scope: 'any', options: [text()] },
  { id: 'shrug', description: 'Send a shrug', scope: 'any', options: [text('text', false)] },
  { id: 'tableflip', description: 'Flip a table', scope: 'any', options: [text('text', false)] },
  { id: 'unflip', description: 'Put a table back', scope: 'any', options: [text('text', false)] },
  { id: 'code', description: 'Send a code block', scope: 'any', options: [o('language', 'text'), text()] },
  { id: 'giphy', description: 'Find a GIF', options: [text('query', false)] },
  { id: 'dm', description: 'Open a direct message', scope: 'any', options: [entity('user', 'user')] },
  { id: 'invite', description: 'Create a community invite', permissionBit: 1 << 11 },
  { id: 'search', description: 'Search messages', scope: 'any', options: [text('query'), o('scope', 'text', false)] },
  { id: 'topic', description: 'Set the channel topic', permissionBit: 1 << 14, options: [text()] },
  { id: 'thread', description: 'Start a thread from a message', options: [entity('message', 'message')] },
  { id: 'reply', description: 'Reply to a message', scope: 'any', options: [entity('message', 'message'), text()] },
  { id: 'edit', description: 'Edit a message', scope: 'any', options: [entity('message', 'message'), text()] },
  { id: 'delete', description: 'Delete a message', scope: 'any', options: [entity('message', 'message'), text('reason', false)] },
  { id: 'pin', description: 'Pin a message', scope: 'any', permissionBit: 1 << 3, options: [entity('message', 'message')] },
  { id: 'unpin', description: 'Unpin a message', scope: 'any', permissionBit: 1 << 3, options: [entity('message', 'message')] },
  { id: 'join', description: 'Join a voice channel', options: [entity('voice-channel', 'channel')] },
  { id: 'leave', description: 'Leave voice', scope: 'any' },
  { id: 'mute', description: 'Mute your microphone', scope: 'any' },
  { id: 'unmute', description: 'Unmute your microphone', scope: 'any' },
  { id: 'deafen', description: 'Deafen yourself', scope: 'any' },
  { id: 'undeafen', description: 'Stop deafening yourself', scope: 'any' },
  { id: 'play', description: 'Play a search result or URL', options: [text('query-or-url')] },
  { id: 'pause', description: 'Pause playback' },
  { id: 'resume', description: 'Resume playback' },
  { id: 'skip', description: 'Skip the current track' },
  { id: 'stop', description: 'Stop playback' },
  { id: 'queue', description: 'Show the music queue' },
  { id: 'nowplaying', description: 'Show the current track' },
  { id: 'seek', description: 'Seek within the current track', options: [o('position', 'time')] },
  { id: 'volume', description: 'Set playback volume', options: [o('level', 'number', true, { min: 0, max: 100 })] },
  { id: 'shuffle', description: 'Toggle queue shuffle', options: [choice('mode', ['on', 'off'])] },
  { id: 'repeat', description: 'Set repeat mode', options: [choice('mode', ['off', 'one', 'all'])] },
  { id: 'queue remove', description: 'Remove a queued track', options: [entity('item', 'queue-item')] },
  { id: 'queue move', description: 'Move a queued track', options: [entity('item', 'queue-item'), o('position', 'number', true, { min: 1 })] },
  { id: 'queue clear', description: 'Clear the music queue' },
  { id: 'music', description: 'Show music command help' },
  { id: 'music help', description: 'Show music command help' },
  { id: 'kick', description: 'Remove a member from the community', permissionBit: 1 << 12, options: [entity('user', 'user')] },
  { id: 'ban', description: 'Ban a member', permissionBit: 1 << 13, options: [entity('user', 'user'), o('duration', 'duration', false), text('reason', false)] },
  { id: 'unban', description: 'Unban a member', permissionBit: 1 << 13, options: [entity('user', 'user')] },
  { id: 'timeout', description: 'Temporarily restrict a member', permissionBit: 1 << 24, options: [entity('user', 'user'), o('duration', 'duration')] },
  { id: 'untimeout', description: 'Remove a member timeout', permissionBit: 1 << 24, options: [entity('user', 'user')] },
  { id: 'warn', description: 'Warn a member', permissionBit: 1 << 24, options: [entity('user', 'user'), text('reason')] },
  { id: 'modlog', description: 'Show moderation history', permissionBit: 1 << 17 },
  { id: 'role add', description: 'Add a role to a member', permissionBit: 1 << 15, options: [entity('user', 'user'), entity('role', 'role')] },
  { id: 'role remove', description: 'Remove a role from a member', permissionBit: 1 << 15, options: [entity('user', 'user'), entity('role', 'role')] },
  { id: 'selfrole list', description: 'List self-assignable roles' },
  { id: 'selfrole add', description: 'Add one of your self-assignable roles', options: [entity('role', 'role')] },
  { id: 'selfrole remove', description: 'Remove one of your self-assignable roles', options: [entity('role', 'role')] },
  { id: 'rolepanel create', description: 'Create a role panel', permissionBit: 1 << 15 },
  { id: 'announcement list', description: 'List announcements' },
  { id: 'announcement create', description: 'Create an announcement', permissionBit: 1 << 16, options: [text('text', false)] },
  { id: 'announcement edit', description: 'Edit an announcement', permissionBit: 1 << 16, options: [entity('announcement', 'announcement'), text()] },
  { id: 'announcement delete', description: 'Delete an announcement', permissionBit: 1 << 16, options: [entity('announcement', 'announcement')] },
  { id: 'poll create', description: 'Create a poll', options: [text('question', false), text('answers', false), choice('multiple', ['on', 'off'], false), o('duration', 'duration', false)] },
  { id: 'poll vote', description: 'Vote in a poll', options: [entity('poll', 'poll'), entity('choice', 'choice', false)] },
  { id: 'poll results', description: 'Show poll results', options: [entity('poll', 'poll')] },
  { id: 'poll close', description: 'Close a poll', options: [entity('poll', 'poll')] },
  { id: 'remind', description: 'Create a personal reminder', options: [o('when', 'time'), text()] },
  { id: 'reminders list', description: 'List your reminders' },
  { id: 'reminders cancel', description: 'Cancel a reminder', options: [entity('reminder', 'reminder')] },
  { id: 'reminders dismiss', description: 'Dismiss a due reminder', options: [entity('reminder', 'reminder')] },
];

const botCommandIds = new Set([
  'play', 'pause', 'resume', 'skip', 'stop', 'queue', 'nowplaying', 'seek', 'volume', 'shuffle', 'repeat',
  'queue remove', 'queue move', 'queue clear', 'music', 'music help', 'rolepanel create',
  'announcement list', 'announcement create', 'announcement edit', 'announcement delete',
  'poll create', 'poll vote', 'poll results', 'poll close',
  'remind', 'reminders list', 'reminders cancel', 'reminders dismiss',
]);

function materialize(seed: (typeof seeds)[number]): CommandDefinition {
  return {
    id: seed.id,
    description: seed.description,
    scope: seed.scope ?? 'community',
    provider: botCommandIds.has(seed.id) ? 'bot' : 'builtin',
    handler: seed.id.replaceAll(' ', '.'),
    ...(seed.permissionBit === undefined ? {} : { permissionBit: seed.permissionBit }),
    options: (seed.options ?? []).map((option) => ({ ...option, description: option.description ?? `${option.name} argument` })),
  };
}

export const COMMANDS: readonly CommandDefinition[] = Object.freeze(seeds.map(materialize));
const byId = new Map(COMMANDS.map((command) => [command.id, command]));
export const commandById = (id: string): CommandDefinition | undefined => byId.get(id);

export interface CommandAvailability {
  available: boolean;
  reason?: 'community-required' | 'permission-unknown' | 'permission-denied';
}

/** Fail closed when a community permission is not yet available in the current context. */
export function commandAvailability(commandOrId: string | CommandDefinition, context: CommandContext): CommandAvailability {
  const command = typeof commandOrId === 'string' ? commandById(commandOrId) : commandOrId;
  if (!command) return { available: false, reason: 'permission-denied' };
  if (command.scope === 'community' && context.communityId === null) return { available: false, reason: 'community-required' };
  if (command.permissionBit !== undefined) {
    // Community permission bits do not apply in DMs; the handler still checks target ownership.
    if (command.scope === 'any' && context.communityId === null) return { available: true };
    if (context.bits === undefined) return { available: false, reason: 'permission-unknown' };
    if ((context.bits & command.permissionBit) !== command.permissionBit) return { available: false, reason: 'permission-denied' };
  }
  return { available: true };
}

export function availableCommands(context: CommandContext, query = ''): CommandDefinition[] {
  return rankCommands(COMMANDS.filter((command) => commandAvailability(command, context).available), query, 25);
}

/** Search names and descriptions; ties preserve catalog order. */
export function searchCommands(query: string, limit = 25): CommandDefinition[] {
  return rankCommands(COMMANDS, query, limit);
}

function rankCommands(commands: readonly CommandDefinition[], query: string, limit: number): CommandDefinition[] {
  const needle = query.trim().toLowerCase();
  if (limit <= 0) return [];
  const ranked = commands.map((command, index) => {
    const id = command.id.toLowerCase();
    const description = command.description.toLowerCase();
    const rank = needle === '' ? 3 : id === needle ? 0 : id.startsWith(needle) ? 1 : id.includes(needle) || description.includes(needle) ? 2 : 4;
    return { command, index, rank };
  }).filter((entry) => entry.rank < 4);
  ranked.sort((a, b) => a.rank - b.rank || a.index - b.index);
  return ranked.slice(0, Math.min(25, limit)).map((entry) => entry.command);
}

function issue(code: ArgumentIssue['code'], name: string, message: string): ArgumentIssue { return { code, name, message }; }

export function validateCommandArgs(command: CommandDefinition, input: Record<string, string>): CommandValidation {
  const issues: ArgumentIssue[] = [];
  const known = new Map(command.options.map((option) => [option.name, option]));
  const args: Record<string, string> = {};
  for (const [name, value] of Object.entries(input)) {
    const option = known.get(name);
    if (!option) { issues.push(issue('unexpected', name, `Unexpected argument: ${name}`)); continue; }
    if (typeof value !== 'string') { issues.push(issue('invalid', name, `${name} must be text`)); continue; }
    if (value.trim() === '') { issues.push(issue('empty', name, `${name} cannot be empty`)); continue; }
    args[name] = value;
    if (option.kind === 'choice' && option.choices && !option.choices.includes(value)) {
      issues.push(issue('invalid', name, `${name} must be one of: ${option.choices.join(', ')}`));
    } else if (option.kind === 'number') {
      const number = Number(value);
      if (!Number.isFinite(number) || !/^[+-]?(?:\d+(?:\.\d*)?|\.\d+)$/.test(value.trim())
        || (option.min !== undefined && number < option.min) || (option.max !== undefined && number > option.max)) {
        issues.push(issue('invalid', name, `${name} must be a number${option.min !== undefined || option.max !== undefined ? ` from ${option.min ?? '−∞'} to ${option.max ?? '∞'}` : ''}`));
      }
    } else if (option.kind === 'duration' && !isDuration(value)) {
      issues.push(issue('invalid', name, `${name} must be a duration such as 10m or 2h`));
    } else if (command.id === 'seek' && name === 'position' && parseSeekPosition(value) === null) {
      issues.push(issue('invalid', name, 'position must be seconds, mm:ss or hh:mm:ss'));
    } else if (option.kind === 'time' && command.id !== 'seek' && (command.id === 'remind' && name === 'when' ? !isReminderWhen(value) : !isTimeValue(value))) {
      issues.push(issue('invalid', name, `${name} must be a duration or ISO local date and time${command.id === 'remind' ? ' (from 1 minute to 365 days for durations)' : ''}`));
    }
  }
  for (const option of command.options) {
    if (option.required && !(option.name in args)) issues.push(issue('missing', option.name, `Missing required argument: ${option.name}`));
  }
  return { valid: issues.length === 0, issues, args };
}

function isDuration(value: string): boolean {
  return /^(?:\d+(?:\.\d+)?(?:ms|s|m|h|d|w))+$/.test(value.trim());
}

function durationMillis(value: string): number | null {
  const parts = [...value.trim().matchAll(/(\d+(?:\.\d+)?)(ms|s|m|h|d|w)/g)];
  if (parts.length === 0 || parts.map((part) => part[0]).join('') !== value.trim()) return null;
  const scale: Record<string, number> = { ms: 1, s: 1000, m: 60_000, h: 3_600_000, d: 86_400_000, w: 604_800_000 };
  return parts.reduce((sum, part) => sum + Number(part[1]) * scale[part[2]], 0);
}

function isIsoLocalDateTime(value: string): boolean {
  const match = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2})(?::(\d{2})(?:\.\d{1,9})?)?(?:Z|([+-])(\d{2}):(\d{2}))?$/.exec(value.trim());
  if (!match) return false;
  const [, y, mo, d, h, mi, s, , oh, om] = match;
  const year = Number(y), month = Number(mo), day = Number(d);
  const daysInMonth = new Date(Date.UTC(year, month, 0)).getUTCDate();
  if (month < 1 || month > 12 || day < 1 || day > daysInMonth || Number(h) > 23 || Number(mi) > 59 || Number(s ?? 0) > 59) return false;
  if (oh !== undefined && (Number(oh) > 23 || Number(om) > 59)) return false;
  return true;
}

function isReminderWhen(value: string): boolean {
  const millis = durationMillis(value);
  return (millis !== null && millis >= 60_000 && millis <= 365 * 86_400_000) || isIsoLocalDateTime(value);
}

function isTimeValue(value: string): boolean {
  return isDuration(value) || /^\d+(?::\d{1,2}){1,2}$/.test(value.trim()) || isIsoLocalDateTime(value);
}

export type CommandParseResult =
  | { type: 'literal'; text: string }
  | { type: 'unknown'; text: string; commandText: string }
  | { type: 'incomplete'; text: string; commandId: string; validation: CommandValidation; subcommands?: string[] }
  | { type: 'invocation'; text: string; invocation: CommandInvocation }
  | { type: 'invalid'; text: string; issues: ArgumentIssue[] };

interface Token { value: string; start: number; end: number; hasUnquotedColon: boolean }
function tokenize(source: string): Token[] | null {
  const tokens: Token[] = [];
  let value = '';
  let quote = '';
  let escaped = false;
  let start = 0;
  let active = false;
  let hasUnquotedColon = false;
  for (let i = 0; i < source.length; i++) {
    const char = source[i];
    if (!active && /\s/u.test(char)) continue;
    if (!active) { active = true; start = i; }
    if (escaped) { value += char; escaped = false; continue; }
    if (char === '\\' && quote) { escaped = true; continue; }
    if (quote) {
      if (char === quote) quote = ''; else value += char;
      continue;
    }
    if (char === '"' || char === "'") { quote = char; continue; }
    if (/\s/u.test(char)) { tokens.push({ value, start, end: i, hasUnquotedColon }); value = ''; active = false; hasUnquotedColon = false; continue; }
    if (char === ':') hasUnquotedColon = true;
    value += char;
  }
  if (escaped || quote) return null;
  if (active) tokens.push({ value, start, end: source.length, hasUnquotedColon });
  return tokens;
}

export function parseCommand(input: string): CommandParseResult {
  const literal = input.startsWith('//') ? input.slice(1) : input;
  if (!input.startsWith('/') || input.startsWith('//')) return { type: 'literal', text: literal };
  // A code fence at the beginning is ordinary content; slash text inside is never inspected.
  if (input.startsWith('```')) return { type: 'literal', text: input };
  const raw = input.slice(1);
  const tokens = tokenize(raw);
  if (!tokens) return { type: 'invalid', text: input, issues: [issue('invalid', '', 'Unclosed quote or escape')] };
  if (tokens.length === 0) return { type: 'unknown', text: input, commandText: '' };

  const words: string[] = [];
  for (const token of tokens) {
    if (token.value.includes(':')) break;
    words.push(token.value.toLowerCase());
  }
  let command: CommandDefinition | undefined;
  let commandWordCount = 0;
  for (let count = 1; count <= words.length; count++) {
    const candidate = commandById(words.slice(0, count).join(' '));
    if (candidate) { command = candidate; commandWordCount = count; }
  }
  if (!command) {
    const path = words.join(' ');
    const subcommands = path === '' ? [] : [...new Set(COMMANDS.flatMap((entry) => {
      const prefix = `${path} `;
      return entry.id.startsWith(prefix) ? [entry.id.slice(prefix.length).split(' ')[0]] : [];
    }))];
    if (subcommands.length > 0 && tokens.length === words.length) {
      const missing = issue('missing', 'subcommand', `Choose a subcommand: ${subcommands.join(', ')}`);
      return { type: 'incomplete', text: input, commandId: path, validation: { valid: false, issues: [missing], args: {} }, subcommands };
    }
    return { type: 'unknown', text: input, commandText: path };
  }

  const args: Record<string, string> = {};
  const issues: ArgumentIssue[] = [];
  const positional: Token[] = [];
  const namedTokens: Token[] = [];
  const freeTextOnly = command.options.length === 1 && command.options[0]?.freeText === true;
  for (const token of tokens.slice(commandWordCount)) {
    const colon = token.value.indexOf(':');
    const named = token.hasUnquotedColon && colon >= 0 && command.options.some((option) => option.name === token.value.slice(0, colon).toLowerCase());
    const optionShaped = token.hasUnquotedColon && /^[a-z][a-z0-9_-]*:/i.test(token.value) && !/^[a-z][a-z0-9+.-]*:\/\//i.test(token.value);
    if (named) {
      namedTokens.push(token);
      const name = token.value.slice(0, colon).toLowerCase();
      const value = token.value.slice(colon + 1);
      if (!command.options.some((option) => option.name === name)) { issues.push(issue('unexpected', name, `Unexpected argument: ${name}`)); continue; }
      if (name in args) { issues.push(issue('duplicate', name, `Argument supplied more than once: ${name}`)); continue; }
      if (!value) { issues.push(issue('empty', name, `${name} cannot be empty`)); continue; }
      args[name] = value;
    } else if (optionShaped && !freeTextOnly) {
      const name = token.value.slice(0, colon).toLowerCase();
      issues.push(issue('unexpected', name, `Unknown named argument: ${name}`));
    } else positional.push(token);
  }
  let pos = 0;
  for (const option of command.options) {
    if (option.name in args) continue;
    if (pos >= positional.length) continue;
    args[option.name] = option.freeText
      ? positionalFreeText(raw, positional.slice(pos), namedTokens)
      : positional[pos].value;
    pos = option.freeText ? positional.length : pos + 1;
  }
  if (pos < positional.length) issues.push(issue('unexpected', '', `Unexpected argument: ${positional.slice(pos).map(token => token.value).join(' ')}`));
  if (issues.length) return { type: 'invalid', text: input, issues };
  const validation = validateCommandArgs(command, args);
  if (!validation.valid) return { type: 'incomplete', text: input, commandId: command.id, validation };
  return { type: 'invocation', text: input, invocation: { commandId: command.id, args: validation.args } };
}

/** Join positional free-text tokens with their original whitespace, omitting named arguments between them. */
function positionalFreeText(source: string, tokens: readonly Token[], namedTokens: readonly Token[]): string {
  if (tokens.length === 0) return '';
  let result = tokens[0].value;
  for (let index = 1; index < tokens.length; index++) {
    const previous = tokens[index - 1];
    const current = tokens[index];
    let cursor = previous.end;
    let gap = '';
    for (const named of namedTokens) {
      if (named.start < cursor || named.end > current.start) continue;
      gap += source.slice(cursor, named.start);
      cursor = named.end;
    }
    gap += source.slice(cursor, current.start);
    result += gap + current.value;
  }
  return result;
}

/** Seek positions are whole seconds or mm:ss/hh:mm:ss with bounded inner fields. */
export function parseSeekPosition(raw: string): number | null {
  const parts = raw.split(':');
  if (parts.length > 3 || parts.some(part => !/^\d+$/u.test(part))) return null;
  const numbers = parts.map(Number);
  if (numbers.slice(1).some(part => part >= 60)) return null;
  const milliseconds = numbers.reduce((seconds, part) => seconds * 60 + part, 0) * 1000;
  return Number.isSafeInteger(milliseconds) ? milliseconds : null;
}
