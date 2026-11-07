// Tests only: a community Bot catalog as the worker publishes it (`botCatalog:<community>`), holding the Bot's commands
// with the argument shapes the Bot declares. The picker's Bot commands come only from such a catalog (Bot v2 task 20).
import { botCommandDefinitions, COMMANDS, type BotCatalogView, type BotCommandSpec, type CommandDefinition } from '@dilla/client-core';

type Arg = [name: string, type: 0 | 1 | 2 | 3 | 4 | 5, required?: boolean];
const command = (name: string, operation: number, args: Arg[] = [], needsBotChannel = false, description = `${name} command`): BotCommandSpec => ({
  name, description, operation, args: args.map(([n, type, required = true]) => ({ name: n, type, required })), permissionBit: 0, needsBotChannel, handlerKind: 0 });

export const FIXTURE_COMMANDS: readonly BotCommandSpec[] = [
  command('play', 12, [['query-or-url', 0]]), command('pause', 13), command('resume', 14), command('skip', 15), command('stop', 16),
  command('queue', 10), command('nowplaying', 10), command('seek', 17, [['position', 0]]), command('volume', 18, [['level', 1]]),
  command('shuffle', 19, [['mode', 5]]), command('repeat', 20, [['mode', 0]]), command('queue remove', 21, [['item', 0]]),
  command('queue move', 22, [['item', 0], ['position', 1]]), command('queue clear', 23), command('music', 10), command('music help', 10),
  command('rolepanel create', 31, [], true), command('announcement list', 50), command('announcement create', 51, [['text', 0, false]], true),
  command('announcement edit', 51, [['announcement', 0], ['text', 0]]), command('announcement delete', 52, [['announcement', 0]]),
  command('poll create', 70, [['question', 0, false], ['answers', 0, false], ['multiple', 5, false], ['duration', 4, false], ['channel', 3, false]], true),
  command('poll vote', 73, [['poll', 0], ['choice', 0, false]]), command('poll results', 71, [['poll', 0]]), command('poll close', 74, [['poll', 0]]),
  command('remind', 80, [['when', 4], ['text', 0]]), command('reminders list', 81), command('reminders cancel', 82, [['reminder', 0]]),
  command('reminders dismiss', 83, [['reminder', 0]]),
];

export const FIXTURE_BOT_USER = '0b'.repeat(16);
export const FIXTURE_BOT_CHANNEL = '21'.repeat(16);

export function fixtureCatalog(over: Partial<BotCatalogView> = {}): BotCatalogView {
  return { revision: '3', commands: [...FIXTURE_COMMANDS], keyId: '0a'.repeat(16), fingerprint: '0123456789AB', status: { state: 'ready', since: null },
    botChannels: [FIXTURE_BOT_CHANNEL], botUserId: FIXTURE_BOT_USER, ...over };
}

/** The fixture's Bot commands as picker definitions. */
export const FIXTURE_BOT_DEFINITIONS: readonly CommandDefinition[] = botCommandDefinitions(FIXTURE_COMMANDS);
/** The built-ins and the fixture's Bot commands, as a composer with that catalog offers them. */
export const FIXTURE_DEFINITIONS: readonly CommandDefinition[] = [...COMMANDS, ...FIXTURE_BOT_DEFINITIONS];
export const fixtureCommand = (id: string): CommandDefinition => FIXTURE_DEFINITIONS.find(row => row.id === id)!;
