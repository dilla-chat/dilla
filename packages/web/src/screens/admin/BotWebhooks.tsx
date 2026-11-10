// Bot v2 Task 22: webhooks on the Bot page (Task 13's routes). A webhook posts into one Bot channel. Its address carries
// the secret and is answered only by create and rotate: it is shown once, in a dialog, and is gone when that closes; it is
// never kept in state beyond the dialog, nor read back (the list holds no secret).
import { useCallback, useEffect, useId, useState } from 'react';
import { Button, ConfirmDialog, Dialog, Select, SettingsRow, TextField, Toggle } from '@dilla/ui';
import type { BotWebhook } from '@dilla/client-core';
import { useCore } from '../../core/context.tsx';
import { errorOf } from '../../core/errors.ts';
import { t } from '../../strings/index.ts';
import type { Announce } from './shared.ts';

export interface BotWebhooksProps {
  communityId: string;
  /** The Bot channels this admin can view (id and name). */
  channels: readonly { id: string; name: string }[];
  canManage: boolean;
  announce: Announce;
}

type Shown = { name: string; url: string } | null;

export function BotWebhooks({ communityId, channels, canManage, announce }: BotWebhooksProps): React.JSX.Element {
  const client = useCore();
  const base = useId();
  const [rows, setRows] = useState<BotWebhook[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [creating, setCreating] = useState(false);
  const [draft, setDraft] = useState<{ name: string; channelId: string; allowEveryone: boolean }>({ name: '', channelId: '', allowEveryone: false });
  const [busy, setBusy] = useState(false);
  // The one-time address: held only while its dialog is open.
  const [shown, setShown] = useState<Shown>(null);
  const [deleting, setDeleting] = useState<BotWebhook | null>(null);
  const [copied, setCopied] = useState(false);
  const fail = (e: unknown) => setError(t('bot.webhooks.error', { code: errorOf(e).code }));

  const load = useCallback(() => {
    if (!canManage) return;
    void client.call({ m: 'listBotWebhooks', communityId }).then(value => setRows(value as BotWebhook[])).catch(fail);
  }, [canManage, client, communityId]);
  useEffect(() => { setRows(null); load(); }, [load]);

  const channelName = (id: string): string => channels.find(c => c.id === id)?.name ?? '';
  const noChannel = channels.length === 0;
  const create = () => {
    const channelId = draft.channelId !== '' ? draft.channelId : channels[0]?.id ?? '';
    if (busy || draft.name.trim() === '' || channelId === '') return;
    setBusy(true); setError(null);
    void client.call({ m: 'createBotWebhook', communityId, channelId, name: draft.name.trim(), allowEveryone: draft.allowEveryone }).then(value => {
      const made = value as { webhookId: string; url: string };
      setCreating(false);
      setDraft({ name: '', channelId: '', allowEveryone: false });
      setCopied(false);
      setShown({ name: draft.name.trim(), url: made.url });
      load();
    }).catch(fail).finally(() => setBusy(false));
  };
  const rotate = (row: BotWebhook) => {
    if (busy) return;
    setBusy(true); setError(null);
    void client.call({ m: 'rotateBotWebhook', communityId, webhookId: row.webhookId }).then(value => {
      setCopied(false);
      setShown({ name: row.name, url: (value as { url: string }).url });
      load();
    }).catch(fail).finally(() => setBusy(false));
  };
  const setEveryone = (row: BotWebhook, on: boolean) => {
    setRows(current => current?.map(r => r.webhookId === row.webhookId ? { ...r, allowEveryone: on } : r) ?? current);
    void client.call({ m: 'updateBotWebhook', communityId, webhookId: row.webhookId, name: row.name, allowEveryone: on })
      .catch((e: unknown) => { fail(e); load(); });
  };
  const remove = () => {
    if (deleting === null || busy) return;
    setBusy(true); setError(null);
    void client.call({ m: 'deleteBotWebhook', communityId, webhookId: deleting.webhookId }).then(() => {
      setDeleting(null); load();
    }).catch(fail).finally(() => setBusy(false));
  };
  const copy = (url: string) => {
    void navigator.clipboard?.writeText(url).then(() => { setCopied(true); announce(t('bot.webhooks.copied')); }).catch(() => undefined);
  };

  return <>
    <h3>{t('bot.webhooks.title')}</h3>
    {rows !== null && rows.length === 0 ? <p>{t('bot.webhooks.empty')}</p> : null}
    {(rows ?? []).map(row => <SettingsRow key={row.webhookId} id={`${base}-hook-${row.webhookId}`} icon="link" label={row.name}
      hint={t('bot.webhooks.posts', { channel: channelName(row.channelId) })}>
      <Toggle id={`${base}-hook-${row.webhookId}-everyone`} label={t('bot.webhooks.allowEveryone')} checked={row.allowEveryone}
        disabled={!canManage} onChange={on => setEveryone(row, on)} />
      <Button size="sm" aria-disabled={busy || !canManage || undefined} onClick={() => rotate(row)}>{t('bot.webhooks.rotate')}</Button>
      <Button size="sm" variant="danger" aria-disabled={busy || !canManage || undefined} onClick={() => setDeleting(row)}>{t('bot.webhooks.delete')}</Button>
    </SettingsRow>)}
    {noChannel ? <p role="note">{t('bot.webhooks.needsChannel')}</p> : null}
    <div className="dw-settings-actions">
      <Button aria-disabled={!canManage || noChannel || undefined} onClick={() => { if (canManage && !noChannel) setCreating(true); }}>
        {t('bot.webhooks.create')}
      </Button>
    </div>
    {error !== null ? <p role="alert">{error}</p> : null}
    <Dialog open={creating} title={t('bot.webhooks.newTitle')} closeLabel={t('admin.cancel')} onClose={() => { if (!busy) setCreating(false); }}
      footer={<Button variant="accent" aria-disabled={busy || draft.name.trim() === '' || undefined} onClick={create}>{t('bot.webhooks.create')}</Button>}>
      <TextField id={`${base}-name`} label={t('bot.webhooks.name')} maxLength={80} value={draft.name} initialFocus
        onChange={name => setDraft(d => ({ ...d, name }))} />
      <Select label={t('bot.webhooks.channel')} value={draft.channelId !== '' ? draft.channelId : channels[0]?.id ?? ''}
        options={channels.map(c => ({ value: c.id, label: `#${c.name}` }))} onChange={channelId => setDraft(d => ({ ...d, channelId }))} />
      <Toggle id={`${base}-everyone`} label={t('bot.webhooks.allowEveryone')} showLabel checked={draft.allowEveryone}
        onChange={allowEveryone => setDraft(d => ({ ...d, allowEveryone }))} />
    </Dialog>
    <Dialog open={shown !== null} title={t('bot.webhooks.addressTitle')} showDefaultClose={false} onClose={() => setShown(null)}
      footer={<>
        <Button onClick={() => { if (shown !== null) copy(shown.url); }}>{copied ? t('bot.webhooks.copied') : t('bot.webhooks.copy')}</Button>
        <Button variant="accent" onClick={() => setShown(null)}>{t('bot.webhooks.done')}</Button>
      </>}>
      <p>{t('bot.webhooks.urlOnce')}</p>
      {shown !== null ? <TextField id={`${base}-url`} label={shown.name} value={shown.url} readOnly onChange={() => undefined} initialFocus /> : null}
    </Dialog>
    <ConfirmDialog open={deleting !== null} title={t('bot.webhooks.delete')} body={t('bot.webhooks.deleteBody', { name: deleting?.name ?? '' })}
      confirmLabel={t('bot.webhooks.delete')} busyLabel={t('bot.webhooks.delete')} cancelLabel={t('admin.cancel')} busy={busy} error={null}
      onConfirm={remove} onClose={() => { if (!busy) setDeleting(null); }} />
  </>;
}
