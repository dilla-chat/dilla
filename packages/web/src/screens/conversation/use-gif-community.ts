import { useEffect, useRef, useState } from 'react';
import { useCore } from '../../core/context.tsx';
import { useSlice, useSlices } from '../../core/use-slice.ts';
import { useRoute } from '../../router.ts';
import { communityActivityOwner, recentCommunities } from '../community-activity.ts';

/** A GIF proxy server supplies media; the selected conversation remains the attachment/send destination. */
export function useGifCommunity(channelId: string, threadId?: string): string | null {
  const client = useCore();
  const account = useSlice('account');
  const communities = useSlice('communities');
  const dms = useSlice('dms');
  const [route] = useRoute();
  const owner = communityActivityOwner(account);
  const self = account?.user?.id ?? '';
  const dm = route.name === 'dm' && route.channelId === channelId ? dms?.find(row => row.id === channelId) : undefined;
  const peer = dm?.kind === 3 && dm.group === 'active' && dm.members.length === 2 && dm.members.includes(self)
    ? dm.members.find(id => id !== self) ?? null : null;
  const direct = peer !== null && owner !== null && threadId === undefined;
  const server = owner !== null && threadId === undefined && route.name === 'channel' && route.channelId === channelId
    ? route.communityId : null;
  const scope = server !== null ? `server/${channelId}/${server}` : direct ? `dm/${channelId}/${peer}` : '';
  const ids = direct ? communities?.map(row => row.id) ?? [] : [];
  const recent = recentCommunities(client, owner);
  const ordered = [...recent.filter(id => ids.includes(id)), ...ids.filter(id => !recent.includes(id))];
  const candidateKey = ordered.join('/');
  const memberLists = useSlices(ordered.map(id => `members:${id}` as const));
  const membershipKey = memberLists.map(list => list === undefined ? '?' : list.some(row => row.userId === self)
    && list.some(row => row.userId === peer) ? 'shared' : 'other').join('/');
  const key = `${owner ?? ''}/${scope}/${candidateKey}/${membershipKey}`;
  const [selected, setSelected] = useState<{ client: typeof client; key: string; communityId: string } | null>(null);
  const refreshes = useRef(new Map<string, { client: typeof client; promise: Promise<unknown> }>());

  useEffect(() => {
    if (scope === '') return;
    let cancelled = false;
    const choose = async () => {
      if (server === null && membershipKey.includes('?')) {
        const refreshKey = `${owner}/${candidateKey}`;
        const previous = refreshes.current.get(refreshKey);
        let refreshing = previous?.client === client ? previous.promise : undefined;
        if (refreshing === undefined) {
          // refreshTrust already reloads all listed member slices; no server selection or new worker/API route is needed.
          refreshing = client.call({ m: 'refreshTrust' }).catch(() => undefined);
          refreshes.current.set(refreshKey, { client, promise: refreshing });
        }
        await refreshing;
      }
      for (const communityId of server === null ? candidateKey.split('/').filter(Boolean) : [server]) {
        if (cancelled) return;
        if (server === null) {
          const members = client.get(`members:${communityId}`);
          if (!members?.some(row => row.userId === self) || !members.some(row => row.userId === peer)) continue;
        }
        try {
          const result = await client.call({ m: 'gifStatus', communityId }) as { on?: boolean } | null;
          if (cancelled) return;
          if (result?.on === true) { setSelected({ client, key, communityId }); return; }
        } catch { /* Try the next shared server when this key is absent or unavailable. */ }
      }
    };
    void choose();
    return () => { cancelled = true; };
  }, [client, key, scope, server, owner, candidateKey, membershipKey, self, peer]);

  return selected?.client === client && selected.key === key ? selected.communityId : null;
}
