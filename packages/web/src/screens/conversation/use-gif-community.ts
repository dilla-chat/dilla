import { useCallback, useEffect, useRef, useState } from 'react';
import { useCore } from '../../core/context.tsx';
import { useSlice, useSlices } from '../../core/use-slice.ts';
import { useRoute } from '../../router.ts';
import { communityActivityOwner, recentCommunities } from '../community-activity.ts';

/** A GIF proxy supplies media; the selected conversation remains the attachment/send destination. */
export function useGifCommunity(channelId: string, threadId?: string) {
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
  const shared = (communityId: string) => {
    const members = client.get(`members:${communityId}`);
    return members?.some(row => row.userId === self) === true && members.some(row => row.userId === peer);
  };
  const missingMembers = memberLists.some(list => list === undefined);
  const key = `${owner ?? ''}/${account?.deviceId ?? ''}/${scope}/${candidateKey}`;
  const current = useRef({ client, key }); current.current = { client, key };
  const mounted = useRef(true);
  useEffect(() => { mounted.current = true; return () => { mounted.current = false; }; }, []);
  const [selected, setSelected] = useState<{ client: typeof client; key: string; communityId: string } | null>(null);

  // In DMs this is called only by the GIF button (or the explicit /gif action), never by opening the conversation.
  const findCommunity = useCallback(async (wanted: () => boolean = () => true): Promise<string | null> => {
    const live = () => mounted.current && current.current.client === client && current.current.key === key && wanted();
    if (scope === '' || !live()) return null;
    if (server === null && missingMembers) {
      // The existing trust refresh loads membership; no DM identity or content is sent to a GIF route.
      await client.call({ m: 'refreshTrust' }).catch(() => undefined);
    }
    for (const communityId of server === null ? candidateKey.split('/').filter(Boolean) : [server]) {
      if (!live()) return null;
      const isShared = () => {
        const members = client.get(`members:${communityId}`);
        return members?.some(row => row.userId === self) === true && members.some(row => row.userId === peer);
      };
      if (server === null && !isShared()) continue;
      try {
        const result = await client.call({ m: 'gifStatus', communityId }) as { on?: boolean } | null;
        if (!live()) return null;
        if (result?.on === true && (server !== null || isShared())) {
          setSelected({ client, key, communityId });
          return communityId;
        }
      } catch { /* Try the next shared server when this key is absent or unavailable. */ }
    }
    if (live()) setSelected(null);
    return null;
  }, [client, key, scope, server, candidateKey, missingMembers, self, peer]);

  useEffect(() => {
    if (server === null) return; // No gifStatus on DM open, even with remembered consent.
    let cancelled = false;
    void findCommunity(() => !cancelled);
    return () => { cancelled = true; };
  }, [server, findCommunity]);

  const communityId = selected?.client === client && selected.key === key
    && (server !== null || shared(selected.communityId)) ? selected.communityId : null;
  return { communityId, direct, findCommunity,
    available: direct ? missingMembers || ordered.some(shared) : communityId !== null };
}
