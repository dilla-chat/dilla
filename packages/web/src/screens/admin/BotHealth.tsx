// Bot v2 Task 22: the Bot page's health and command key, and its Bot channels. Health is the instance's catalog status
// (Task 17: state and a server-stamped since); the fingerprint is the command key this device verified (Task 20). The
// Bot channels are the ones this member can view (Task 9); a manager also reads the Bot's own status (sealed read 94) to
// see the Bot channels it needs adding to again (Task 16's needs re-add), with a button that adds it.
import { useCallback, useEffect, useId, useState } from 'react';
import { Button, SettingsRow } from '@dilla/ui';
import { BOT_STATUS_READ, catalogOffers, createBotOperations, decodeBotStatus, type BotCatalogView } from '@dilla/client-core';
import { useCore } from '../../core/context.tsx';
import { errorOf } from '../../core/errors.ts';
import { useSlice } from '../../core/use-slice.ts';
import { formatDayTime, t } from '../../strings/index.ts';
import type { Announce } from './shared.ts';

/** The health line: ready, failing (degraded) or offline, each with the instance's own time; null for any other state. */
export function healthText(status: BotCatalogView['status']): string | null {
  if (status.state === 'ready') return t('bot.health.ok');
  // With no server time the state stands alone (fix round 1, L4).
  if (status.state === 'degraded') return status.since === null ? t('bot.health.failingNow') : t('bot.health.failing', { time: formatDayTime(status.since) });
  if (status.state === 'offline') return status.since === null ? t('bot.health.offlineNow') : t('bot.health.offline', { time: formatDayTime(status.since) });
  if (status.state === 'unverified') return t('commands.botUnverified');
  if (status.state === 'changed') return t('commands.botChanged');
  return null;
}

/** The Bot's own status read (94): the Bot channels it needs re-adding to. Read when the catalog offers it to a manager. */
export function useBotNeedsReAdd(communityId: string, generation: number): ReadonlySet<string> {
  const client = useCore();
  const access = useSlice(`permissions:${communityId}`);
  const catalog = useSlice(`botCatalog:${communityId}`);
  const manager = access?.loaded === true && access.manageCommunity;
  const offered = catalog != null && catalogOffers(client, communityId, BOT_STATUS_READ);
  const [needs, setNeeds] = useState<ReadonlySet<string>>(new Set());
  useEffect(() => {
    if (!manager || !offered) { setNeeds(new Set()); return; }
    let live = true;
    void createBotOperations(client, communityId).request({ operation: BOT_STATUS_READ, args: [] }).then(result => {
      if (live && result.status === 0) setNeeds(new Set(decodeBotStatus(result.data).needsReAdd));
    }).catch(() => undefined);
    return () => { live = false; };
  }, [client, communityId, manager, offered, generation]);
  return manager ? needs : new Set();
}

export function BotHealth({ communityId, announce }: { communityId: string; announce: Announce }): React.JSX.Element | null {
  const client = useCore();
  const base = useId();
  const catalog = useSlice(`botCatalog:${communityId}`);
  const channels = useSlice(`channels:${communityId}`);
  const access = useSlice(`permissions:${communityId}`);
  const manager = access?.loaded === true && access.manageCommunity;
  const [generation, setGeneration] = useState(0);
  const needs = useBotNeedsReAdd(communityId, generation);
  const [busy, setBusy] = useState<string | null>(null);
  const [note, setNote] = useState<{ channelId: string; text: string } | null>(null);
  const reAdd = useCallback((channelId: string) => {
    if (busy !== null) return;
    setBusy(channelId); setNote(null);
    void client.call({ m: 'reAddBot', communityId, channelId }).then(value => {
      if (value === 'pending') setNote({ channelId, text: t('bot.channels.reAddPending') });
      else announce(t('bot.channels.reAdding'));
      setGeneration(g => g + 1);
    }).catch((e: unknown) => setNote({ channelId, text: t('bot.channels.reAddError', { code: errorOf(e).code }) }))
      .finally(() => setBusy(null));
  }, [announce, busy, client, communityId]);
  if (catalog == null) return null;
  const health = healthText(catalog.status);
  const named = new Map((channels ?? []).map(c => [c.id, c.name]));
  const botChannels = (catalog.botChannels ?? []).filter(id => named.has(id));
  return <>
    {health !== null ? <SettingsRow id={`${base}-health`} icon="activity" label={health}
      {...(catalog.fingerprint !== null ? { hint: t('bot.key.fingerprint', { fingerprint: catalog.fingerprint }) } : {})}>
      {null}
    </SettingsRow> : null}
    <h3>{t('bot.channels.title')}</h3>
    {catalog.botChannels !== null && botChannels.length === 0 ? <p>{t('bot.channels.empty')}</p> : null}
    {botChannels.map(id => {
      const name = `#${named.get(id) ?? ''}`;
      const stale = needs.has(id);
      return <SettingsRow key={id} id={`${base}-channel-${id}`} icon="hash" label={name} tag={t('bot.channel.badge')}
        {...(stale ? { hint: note?.channelId === id ? note.text : t('bot.channels.needsReAdd') } : note?.channelId === id ? { hint: note.text } : {})}>
        {stale && manager ? <Button size="sm" aria-disabled={busy !== null || undefined} aria-busy={busy === id || undefined}
          onClick={() => reAdd(id)}>{busy === id ? t('bot.channels.reAdding') : t('bot.channels.reAdd')}</Button> : null}
      </SettingsRow>;
    })}
  </>;
}
