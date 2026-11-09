import { useState } from 'react';
import { ChannelSettings, Select, Toggle } from '@dilla/ui';
import type { ChannelSummary } from '@dilla/client-core';
import { useCore } from '../../core/context.tsx';
import { t } from '../../strings/index.ts';
import { CHANNEL_NAME_MAX_BYTES, channelName, nameProblem } from '../Create.tsx';
import { ChannelPermissions } from './ChannelPermissions.tsx';
import { forbidden, refused, statusIs, useRun, type Announce } from './shared.ts';
import { errorOf } from '../../core/errors.ts';
import { useSlice } from '../../core/use-slice.ts';
import { SLOW_STEPS } from '../conversation/slow.ts';

const TOPIC_MAX_BYTES = 1024;
const UTF8 = new TextEncoder();
// Control characters other than line break and tab (the instance refuses them in a topic).
const TOPIC_CONTROL = /[\p{Cc}]/u;
const topicHasControl = (s: string): boolean => [...s].some(c => c !== '\n' && c !== '\t' && TOPIC_CONTROL.test(c));

const NO_ANNOUNCE: Announce = () => undefined;

interface Draft { id: string; name: string; topic: string; parent: string; slowmode: string }

const SLOW_OPTIONS = SLOW_STEPS.map(([seconds, key]) => ({ value: String(seconds), label: t(key) }));

/**
 * Edit a channel or category (flow 08) over `ChannelSettings`. The category select offers `top` (no category) and every
 * category; `parent` is sent as `keep` when it is unchanged, `top` for no category, else the category's id. Only the fields
 * that changed are sent; nothing changed closes the dialog without a request. `channel` null keeps the dialog closed.
 */
export function ChannelEditFlow({ channel, categories, announce, onClose }: {
  channel: ChannelSummary | null; categories: readonly ChannelSummary[]; announce?: Announce | undefined; onClose: () => void;
}): React.JSX.Element {
  const client = useCore();
  const [draft, setDraft] = useState<Draft | null>(null);
  const [local, setLocal] = useState<{ message: string; n: number } | null>(null);
  const action = useRun();
  const access = useSlice(`permissions:${channel?.communityId ?? ''}`);
  // R39: the server's AFK channel keeps its kind, settings and permissions; only its name and category change.
  const afkId = useSlice(`afk:${channel?.communityId ?? ''}`)?.channelId ?? null;
  const isAFK = channel !== null && channel.id === afkId;
  const kind = channel === null ? 'text' : channel.kind === 2 ? 'category' : channel.kind === 1 ? 'voice' : 'text';
  const initial: Draft = { id: channel?.id ?? '', name: channel?.name ?? '', topic: channel?.topic ?? '', parent: channel?.parentId != null && categories.some(c => c.id === channel.parentId) ? channel.parentId : 'top',
    slowmode: String(channel?.slowmode ?? 0) };
  const current = draft?.id === initial.id ? draft : initial;
  // Bot v2 Task 22: a text channel that is not readable (and not the AFK channel) can be a Bot channel; only who manages the community
  // marks one. The switch acts at once, beside the form: the mark is the instance's, not a field of the channel.
  const catalog = useSlice(`botCatalog:${channel?.communityId ?? ''}`);
  const botChannel = channel !== null && (catalog?.botChannels ?? []).includes(channel.id);
  const [marking, setMarking] = useState(false);
  const [markError, setMarkError] = useState<string | null>(null);
  const canMark = channel !== null && kind === 'text' && channel.mode === 0 && !isAFK && access?.loaded === true && access.manageCommunity;
  const mark = (on: boolean) => {
    if (channel === null || marking) return;
    setMarking(true); setMarkError(null);
    void client.call({ m: 'markBotChannel', communityId: channel.communityId, channelId: channel.id, on })
      .catch((e: unknown) => setMarkError(t('bot.channel.error', { code: errorOf(e).code })))
      .finally(() => setMarking(false));
  };
  const edit = (over: Partial<Draft>) => { setDraft({ ...current, ...over }); setLocal(null); action.reset(); };

  const close = () => { if (!action.busy) { setDraft(null); setLocal(null); action.reset(); onClose(); } };
  const refuse = (message: string) => setLocal(l => ({ message, n: (l?.n ?? 0) + 1 }));

  const submit = () => {
    if (channel === null) return;
    const shownName = channelName(kind === 'text' ? 'text' : 'voice', current.name);
    const problem = nameProblem(shownName, CHANNEL_NAME_MAX_BYTES);
    if (problem !== null) {
      refuse(t(problem === 'empty' ? 'admin.channel.error.empty' : problem === 'long' ? 'admin.channel.error.long' : 'create.name.control'));
      return;
    }
    if (kind === 'text') {
      if (UTF8.encode(current.topic).length > TOPIC_MAX_BYTES) { refuse(t('admin.channel.error.topicLong')); return; }
      if (topicHasControl(current.topic)) { refuse(t('admin.channel.error.topicControl')); return; }
    }
    const name = shownName === channel.name ? null : shownName;
    const topic = kind === 'text' && current.topic !== channel.topic ? current.topic : null;
    const parent = kind === 'category' || current.parent === initial.parent ? 'keep' : current.parent;
    const slowmode = kind === 'text' && current.slowmode !== initial.slowmode ? Number(current.slowmode) : null;
    if (name === null && topic === null && parent === 'keep' && slowmode === null) { close(); return; }
    void action.run(() => client.call({ m: 'updateChannel', channelId: channel.id, name, topic, parent, slowmode }), e =>
      forbidden(e) ? t('admin.channel.error.forbidden')
        : statusIs(e, 404) ? t('admin.channel.error.gone')
          : refused(e) ? t('admin.channel.error.refused')
            : t('admin.channel.error.other', { code: e.code }))
      .then(ok => { if (ok) close(); });
  };

  const title = channel === null ? '' : kind === 'category' ? t('admin.channel.titleCategory', { name: channel.name })
    : t(kind === 'voice' ? 'admin.channel.titleVoice' : 'admin.channel.titleText', { name: channel.name });
  return (
    <ChannelSettings open={channel !== null} kind={kind} name={current.name} onName={v => edit({ name: v })}
      topic={current.topic} onTopic={v => edit({ topic: v })} parent={current.parent} onParent={v => edit({ parent: v })}
      parents={[{ value: 'top', label: t('admin.channels.noCategory') }, ...categories.map(c => ({ value: c.id, label: c.name }))]}
      error={local?.message ?? action.error} busy={action.busy} attempt={(local?.n ?? 0) + action.attempt}
      labels={{ title, name: t('admin.channel.name'), nameHint: t('admin.channel.nameHint'), topic: t('admin.channel.topic'),
        topicHint: t('admin.channel.topicHint'), category: t('admin.channel.category'), submit: t('admin.channel.save'),
        busy: t('admin.channel.saving'), close: t('admin.cancel') }}
      onSubmit={submit} onClose={close}>
      {kind === 'text' ? <Select label={t('slow.label')} hint={t('slow.hint')} value={current.slowmode} options={SLOW_OPTIONS}
        onChange={v => edit({ slowmode: v })} /> : null}
      {canMark ? <Toggle id={`bot-channel-${channel.id}`} label={t('bot.channel.toggle')} showLabel hint={markError ?? t('bot.channel.toggleHint')}
        checked={botChannel} disabled={marking} onChange={mark} /> : null}
      {isAFK ? <p className="dw-afk-note">{t('afk.channel.note')}</p> : null}
      {channel !== null && kind !== 'category' && !isAFK && access?.manageRoles === true
        ? <ChannelPermissions channelId={channel.id} communityId={channel.communityId} announce={announce ?? NO_ANNOUNCE} /> : null}
    </ChannelSettings>
  );
}
