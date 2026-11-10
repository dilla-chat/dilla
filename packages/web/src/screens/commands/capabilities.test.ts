import { describe, expect, it } from 'vitest';
import { commandBotOf } from './capabilities.ts';
import { botSince } from './bot-run.ts';
import { FIXTURE_BOT_USER, fixtureCatalog } from './bot-catalog.fixture.ts';

const COMMUNITY = '55'.repeat(16);

describe('the picker’s Bot commands (Bot v2)', () => {
  it('come from the catalog on this device, each with its operation', () => {
    const bot = commandBotOf(fixtureCatalog(), COMMUNITY);
    expect(bot.definitions.find(d => d.id === 'remind')?.bot?.operation).toBe(80);
    expect(bot.definitions.find(d => d.id === 'play')?.provider).toBe('bot');
    expect(bot.botUserId).toBe(FIXTURE_BOT_USER);
    expect(bot.greyed.size).toBe(0);
    expect(bot.waiting).toBe(false);
  });

  it('are none in a DM, and none (waiting) before the first sync of a community', () => {
    expect(commandBotOf(fixtureCatalog(), null).definitions).toEqual([]);
    expect(commandBotOf(undefined, COMMUNITY)).toMatchObject({ definitions: [], waiting: true });
    expect(commandBotOf(null, COMMUNITY)).toMatchObject({ definitions: [], waiting: false });
  });

  it('are greyed, never hidden, with the Bot’s state and since', () => {
    const since = 1_758_659_040;
    const offline = commandBotOf(fixtureCatalog({ status: { state: 'offline', since } }), COMMUNITY);
    expect(offline.definitions.length).toBeGreaterThan(0);
    expect(offline.greyed.get('play')).toBe(`The Bot is offline since ${botSince(since)}`);
    expect(commandBotOf(fixtureCatalog({ status: { state: 'degraded', since } }), COMMUNITY).greyed.get('remind'))
      .toBe(`The Bot is having trouble since ${botSince(since)}`);
    expect(commandBotOf(fixtureCatalog({ status: { state: 'disabled', since: null } }), COMMUNITY).greyed.get('remind')).toBe('The Bot is turned off in this server');
    expect(commandBotOf(fixtureCatalog({ status: { state: 'absent', since: null } }), COMMUNITY).greyed.get('remind')).toBe('This server has no Bot');
  });

  it('that post in a channel are greyed when this member can view no Bot channel', () => {
    const bot = commandBotOf(fixtureCatalog({ botChannels: [] }), COMMUNITY);
    expect(bot.greyed.get('poll create')).toBe('Mark a channel as a Bot channel first');
    expect(bot.greyed.get('remind')).toBeUndefined();
  });
});
