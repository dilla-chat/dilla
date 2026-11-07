// Bot v2 (task 20): which Bot commands the picker lists. They are the community Bot's catalog, held on this device by the
// worker (the `botCatalog:<community>` slice): read at connect only when the instance's revision is newer, replaced live by
// op 61. Opening the picker reads that copy and sends nothing, so it never says "Loading Bot commands…" once synced. A Bot
// that is not ready, or a command that needs a Bot channel this member cannot view, greys its commands (bot-run.ts).
import { useCallback, useMemo, useSyncExternalStore } from 'react';
import { botCommandDefinitions, type BotCatalogView, type CommandDefinition, type CoreClient } from '@dilla/client-core';
import { botCommandReason } from './bot-run.ts';

export interface CommandBot {
  /** The catalog this device holds; null in a DM, or before the first sync of this community. */
  catalog: BotCatalogView | null;
  /** The catalog's commands as picker definitions (none while there is no catalog). */
  definitions: readonly CommandDefinition[];
  /** Command id → why it is greyed (the Bot's state and since, or a missing Bot channel). */
  greyed: ReadonlyMap<string, string>;
  /** The Bot's user id (the source tag's name, and the signer of its posts). */
  botUserId: string | null;
  /** No catalog has been read for this community yet (the first connect sync is still running). */
  waiting: boolean;
}

const NONE: CommandBot = Object.freeze({ catalog: null, definitions: [], greyed: new Map(), botUserId: null, waiting: false });

/** The picker's Bot commands for a community, from the catalog on this device; it makes no request. */
export function commandBotOf(catalog: BotCatalogView | null | undefined, communityId: string | null): CommandBot {
  if (communityId === null) return NONE;
  if (catalog === undefined || catalog === null) return { ...NONE, waiting: catalog === undefined };
  const definitions = botCommandDefinitions(catalog.commands);
  const greyed = new Map<string, string>();
  for (const definition of definitions) {
    const reason = botCommandReason(catalog, definition);
    if (reason !== undefined) greyed.set(definition.id, reason);
  }
  return { catalog, definitions, greyed, botUserId: catalog.botUserId, waiting: false };
}

export function useCommandBot(client: CoreClient, communityId: string | null): CommandBot {
  const name = `botCatalog:${communityId ?? ''}` as const;
  const subscribe = useCallback((listener: () => void) => (communityId === null ? () => undefined : client.subscribe(name, listener)), [client, communityId, name]);
  const snapshot = useCallback(() => (communityId === null ? null : client.get(name)), [client, communityId, name]);
  const catalog = useSyncExternalStore(subscribe, snapshot);
  return useMemo(() => commandBotOf(catalog, communityId), [catalog, communityId]);
}
