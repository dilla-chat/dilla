// Bot v2 Task 22: what a webhook post needs: the community's pinned Bot user (only its posts carry a label the client
// reads) and the webhook's avatar, loaded only from the instance's member route (GET …/bot/webhook-avatars/{id}), never
// from a sender's URL. The instance keeps avatars in memory: a 404 (gone after its restart) leaves the placeholder.
import { useEffect, useState, useSyncExternalStore } from 'react';
import type { CoreClient } from '@dilla/client-core';

const NONE = (): (() => void) => () => undefined;

/** The Bot user this device pinned for the community (catalog slice), or null. */
export function usePinnedBotUser(client: CoreClient | undefined, communityId: string | null): string | null {
  const key = `botCatalog:${communityId ?? ''}` as const;
  return useSyncExternalStore(
    client === undefined || communityId === null ? NONE : listener => client.subscribe(key, listener),
    () => (client === undefined || communityId === null ? null : client.get(key)?.botUserId ?? null));
}

/** Bytes this page already read, by community and avatar id: one read per avatar per page. A miss is not kept. */
const read = new Map<string, Uint8Array>();
const READ_KEPT = 64;

/** An object URL for the webhook avatar, or undefined (none, loading, or the instance no longer holds it). */
export function useWebhookAvatar(client: CoreClient | undefined, communityId: string | null, avatarId: string | null): string | undefined {
  const [url, setUrl] = useState<string | undefined>(undefined);
  useEffect(() => {
    setUrl(undefined);
    if (client === undefined || communityId === null || avatarId === null) return;
    const key = `${communityId}:${avatarId}`;
    let live = true;
    let made: string | undefined;
    const show = (bytes: Uint8Array) => {
      if (!live || typeof URL.createObjectURL !== 'function') return;
      made = URL.createObjectURL(new Blob([new Uint8Array(bytes)], { type: 'image/png' }));
      setUrl(made);
    };
    const kept = read.get(key);
    if (kept !== undefined) show(kept);
    else {
      void client.call({ m: 'getWebhookAvatar', communityId, avatarId }).then(value => {
        if (!(value instanceof Uint8Array) || value.length === 0) return;
        read.set(key, value);
        while (read.size > READ_KEPT) read.delete(read.keys().next().value!);
        show(value);
      }).catch(() => undefined);
    }
    return () => { live = false; if (made !== undefined) URL.revokeObjectURL(made); };
  }, [client, communityId, avatarId]);
  return url;
}
