// Signal wave B Task 2: which Bot commands the picker lists. A Bot command shows only while the server's Bot reports its
// operation (`createBotUtilities(...).capabilities().supportedOperations`); while that is unknown, none show.
import { useEffect, useState } from 'react';
import { createBotUtilities, type CommandDefinition, type CoreClient } from '@dilla/client-core';

/** The Bot operation each Bot command needs. */
export const COMMAND_OPERATIONS: Readonly<Record<string, number>> = Object.freeze({
  play: 12, pause: 13, resume: 14, skip: 15, stop: 16, queue: 10, nowplaying: 10, seek: 17, volume: 18, shuffle: 19, repeat: 20,
  'queue remove': 21, 'queue move': 22, 'queue clear': 23, music: 10, 'music help': 10,
  'rolepanel create': 31,
  'announcement list': 50, 'announcement create': 51, 'announcement edit': 51, 'announcement delete': 52,
  'poll create': 70, 'poll results': 71, 'poll vote': 73, 'poll close': 74,
  remind: 80, 'reminders list': 81, 'reminders cancel': 82, 'reminders dismiss': 83,
});

/** Re-read every 30 s while the picker is open (an owner may turn a feature off in another tab). */
export const CAPABILITY_REFRESH_MS = 30_000;

/** Whether the picker lists `command`: built-ins always; a Bot command only with its operation reported. */
export function commandListed(command: CommandDefinition, operations: ReadonlySet<number> | null): boolean {
  if (command.provider !== 'bot') return true;
  const operation = COMMAND_OPERATIONS[command.id];
  return operations !== null && operation !== undefined && operations.has(operation);
}

export interface CommandBot { operations: ReadonlySet<number> | null; botUserId: string | null }

/**
 * The server Bot's operations and user id: read when the composer opens command mode (`open` turns true) and every 30 s
 * while it stays open; `null` while unknown, in a DM, or when the server has no Bot.
 */
export function useCommandBot(client: CoreClient, communityId: string | null, open = true): CommandBot {
  const [state, setState] = useState<{ key: string; bot: CommandBot } | null>(null);
  const key = communityId ?? '';
  useEffect(() => {
    if (communityId === null || !open) return;
    let live = true;
    const read = () => {
      void createBotUtilities(client, communityId).capabilities().then(caps => {
        if (live) setState({ key: communityId, bot: { operations: new Set(caps.supportedOperations), botUserId: caps.botUserId } });
      }, () => { if (live) setState({ key: communityId, bot: { operations: null, botUserId: null } }); });
    };
    read();
    const timer = window.setInterval(read, CAPABILITY_REFRESH_MS);
    return () => { live = false; window.clearInterval(timer); };
  }, [client, communityId, open]);
  return state !== null && state.key === key && communityId !== null ? state.bot : NO_BOT;
}
const NO_BOT: CommandBot = Object.freeze({ operations: null, botUserId: null });

/** The operations alone (the plan's interface): `null` while unknown. */
export function useCommandCapabilities(client: CoreClient, communityId: string | null, open = true): ReadonlySet<number> | null {
  return useCommandBot(client, communityId, open).operations;
}
