// Result authors read installation identity without capability requests or a permanent cadence.
import { useCallback, useEffect, useRef, useState } from 'react';
import type { CoreClient } from '@dilla/client-core';
import { installationBotName } from './bot-name.ts';

type Member = { userId: string; kind?: number; display?: string; username?: string };
export function useResultBotName(client: CoreClient, communityId: string | null, members: readonly Member[] | undefined, visible: boolean): string {
  const account = client.get('account');
  // A replacement installation adds/removes its Bot member; ordinary display-name updates need no identity read.
  const installations = (members ?? []).filter(member => member.kind === 1).map(member => member.userId).sort().join('/');
  const key = `${account?.instance?.id ?? ''}/${account?.user?.id ?? ''}/${communityId ?? ''}/${installations}`;
  const [identity, setIdentity] = useState<{ key: string; botUserId: string | null } | null>(null);
  const generation = useRef(0);
  const pending = useRef(false);
  const read = useCallback(async () => {
    if (communityId === null || pending.current) return;
    pending.current = true;
    const current = generation.current;
    try {
      const status = await client.call({ m: 'getBotCommandStatus', communityId }) as { botUserId?: string | null } | null;
      if (current === generation.current) setIdentity({ key, botUserId: status?.botUserId ?? null });
    } catch { if (current === generation.current) setIdentity({ key, botUserId: null }); }
    finally { if (current === generation.current) pending.current = false; }
  }, [client, communityId, key]);
  useEffect(() => {
    const requests = generation;
    requests.current++; pending.current = false;
    void read();
    return () => { requests.current++; };
  }, [read]);
  const botUserId = identity?.key === key ? identity.botUserId : null;
  const known = botUserId !== null && members?.some(member => member.userId === botUserId && Boolean(member.display || member.username)) === true;
  useEffect(() => {
    // No installation means no polling, even if an old private result is still visible.
    if (!visible || known || botUserId === null) return;
    const timer = window.setInterval(() => { void read(); }, 30_000);
    return () => window.clearInterval(timer);
  }, [visible, known, botUserId, read]);
  return installationBotName(members, botUserId);
}
