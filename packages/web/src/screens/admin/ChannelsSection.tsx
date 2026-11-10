import { useEffect, useId, useRef, useState } from 'react';
import { Button, ConfirmDialog, ChannelSettings, IconButton, TrailingSlot } from '@dilla/ui';
import { isBuiltinAFK, isPrivate, type ChannelSummary } from '@dilla/client-core';
import { useCore } from '../../core/context.tsx';
import { errorOf } from '../../core/errors.ts';
import { useSlice } from '../../core/use-slice.ts';
import { t } from '../../strings/index.ts';
import { CHANNEL_NAME_MAX_BYTES, channelName, nameProblem } from '../Create.tsx';
import { ChannelEditFlow } from './ChannelEditFlow.tsx';
import './ChannelsSection.css';
import { useRun, type SectionProps } from './shared.ts';

const byOrder = (a: ChannelSummary, b: ChannelSummary): number => a.position - b.position || (a.id < b.id ? -1 : a.id > b.id ? 1 : 0);
const CATEGORY = 2;
const NO_CHANNELS: readonly ChannelSummary[] = [];

interface Group { category: ChannelSummary | null; channels: ChannelSummary[] }

/** Channels with no category first, then each category with its channels, all in position order. */
export function groupChannels(all: readonly ChannelSummary[]): Group[] {
  const categories = all.filter(c => c.kind === CATEGORY).sort(byOrder);
  const known = new Set(categories.map(c => c.id));
  const rest = all.filter(c => c.kind !== CATEGORY);
  const top = rest.filter(c => c.parentId === null || !known.has(c.parentId)).sort(byOrder);
  return [{ category: null, channels: top },
    ...categories.map(category => ({ category, channels: rest.filter(c => c.parentId === category.id).sort(byOrder) }))];
}

type Target = { kind: 'edit' | 'delete'; id: string } | { kind: 'category' } | null;

/** The label a row shows: a text channel with its hash. */
const shown = (c: ChannelSummary): string => (c.kind === 0 ? `#${c.name}` : c.name);

/**
 * Channels tab: the list grouped by category with edit, move up, move down and delete on every row, and a new category
 * button. A move is one `moveChannel` step among the row's siblings; the first of a group cannot move up, the last cannot
 * move down. The built-in AFK channel has no moves and is not counted, so it stays last (R-AFK8). Focus follows a moved
 * row to its button.
 */
export function ChannelsSection({ communityId, serverName, announce }: SectionProps): React.JSX.Element {
  const client = useCore();
  const base = useId();
  const channels = useSlice(`channels:${communityId}`) ?? NO_CHANNELS;
  const groups = groupChannels(channels);
  const categories = groups.flatMap(g => (g.category === null ? [] : [g.category]));
  const access = useSlice(`permissions:${communityId}`);
  const overwrites = useSlice(`overwrites:${communityId}`);
  // Private means @everyone is denied viewing; with the overwrites unknown nothing is tagged.
  const tagged = (c: ChannelSummary): boolean => access?.overwritesLoaded === true && access.everyoneId !== '' && isPrivate(overwrites?.[c.id] ?? [], access.everyoneId);
  // Bot v2 Task 22: the Bot channels this member can view carry the Bot badge.
  const botChannels = new Set(useSlice(`botCatalog:${communityId}`)?.botChannels ?? []);
  const [target, setTarget] = useState<Target>(null);
  const [moveError, setMoveError] = useState<string | null>(null);
  const [moveAttempt, setMoveAttempt] = useState(0);
  const moving = useRef(false);
  const headingRef = useRef<HTMLHeadingElement>(null);
  const refocus = useRef<{ id: string; direction: 'up' | 'down' } | null>(null);
  const focusHeading = useRef(false);

  const live = target !== null && target.kind !== 'category' ? channels.find(c => c.id === target.id) : undefined;
  // A channel that vanishes while its dialog is open (deleted elsewhere) closes the dialog.
  const stale = target !== null && target.kind !== 'category' && live === undefined;
  useEffect(() => { if (stale) { focusHeading.current = true; setTarget(null); } }, [stale]);

  // After a move the row sits elsewhere: its button gets focus back; if it can no longer move that way, the other one.
  useEffect(() => {
    const want = refocus.current;
    if (want === null) return;
    refocus.current = null;
    const pick = (d: string) => document.querySelector<HTMLButtonElement>(`[data-move="${want.id}:${d}"]:not(:disabled)`);
    (pick(want.direction) ?? pick(want.direction === 'up' ? 'down' : 'up'))?.focus();
  }, [channels]);
  // After a delete the opener row is gone: focus goes to the heading.
  useEffect(() => {
    if (target !== null || !focusHeading.current) return;
    focusHeading.current = false;
    headingRef.current?.focus();
  }, [target]);

  const move = (c: ChannelSummary, direction: 'up' | 'down') => {
    if (moving.current) return;
    moving.current = true;
    setMoveError(null);
    refocus.current = { id: c.id, direction };
    client.call({ m: 'moveChannel', channelId: c.id, direction }).then(() => {
      announce(t('admin.channels.moved', { name: c.name }));
    }).catch((e: unknown) => {
      refocus.current = null;
      setMoveError(t('admin.channels.error.move', { name: c.name, code: errorOf(e).code }));
      setMoveAttempt(a => a + 1);
    }).finally(() => { moving.current = false; });
  };

  // I6: move up, move down, edit and delete are icon actions in four fixed trailing slots (the ManageList grammar); the AFK row is `fixed`,
  // so its delete slot stays empty and the icons keep one right edge. A long name is cut to one line (the full name is its title).
  // R-AFK8: AFK stands last in its run at 2^30 and never moves, so its move slots stay empty too, and the ordinary rows
  // count among themselves: the last one above AFK cannot move down.
  const afkId = useSlice(`afk:${communityId}`)?.channelId ?? null;
  const fixed = (c: ChannelSummary): boolean => c.kind !== CATEGORY && isBuiltinAFK(c.id, c.position, afkId);
  const controls = (c: ChannelSummary, index: number, siblings: number) => (
    <span className="d-manage-list__trail">
      <TrailingSlot>{fixed(c) ? null : <IconButton icon="arrow-up" label={t('admin.channels.upLabel', { name: c.name })} data-move={`${c.id}:up`}
        disabled={index === 0} onClick={() => move(c, 'up')} />}</TrailingSlot>
      <TrailingSlot>{fixed(c) ? null : <IconButton icon="arrow-down" label={t('admin.channels.downLabel', { name: c.name })} data-move={`${c.id}:down`}
        disabled={index === siblings - 1} onClick={() => move(c, 'down')} />}</TrailingSlot>
      <TrailingSlot><IconButton icon="square-pen" label={t('admin.channels.editLabel', { name: c.name })} onClick={() => setTarget({ kind: 'edit', id: c.id })} /></TrailingSlot>
      <TrailingSlot>{c.id === afkId ? null
        : <IconButton icon="x" label={t('admin.channels.deleteLabel', { name: c.name })} onClick={() => setTarget({ kind: 'delete', id: c.id })} />}</TrailingSlot>
    </span>
  );

  return (
    <section className="dw-settings-section" aria-labelledby={`${base}-title`}>
      <h2 id={`${base}-title`} ref={headingRef} tabIndex={-1}>{t('admin.channels.title')}</h2>
      <div className="dw-settings-actions">
        <Button onClick={() => setTarget({ kind: 'category' })}>{t('admin.channels.newCategory')}</Button>
      </div>
      {moveError !== null ? <p key={moveAttempt} role="alert" className="dw-admin-error">{moveError}</p> : null}
      {groups.map((g, gi) => {
        const movable = g.channels.filter(c => !fixed(c));
        return (
          <div key={g.category?.id ?? 'top'} className="dw-admin-group">
            <div className="dw-admin-grouphead d-manage-list__row" data-row>
              <h3 className="dw-admin-name" title={g.category?.name}>{g.category?.name ?? t('admin.channels.noCategory')}</h3>
              {g.category !== null ? controls(g.category, gi - 1, categories.length) : null}
            </div>
            <ul aria-label={g.category?.name ?? t('admin.channels.noCategory')} className="dw-admin-rows d-manage-list">
              {g.channels.map(c => (
                <li key={c.id} className="dw-admin-row d-manage-list__row" data-row>
                  <span className="dw-admin-name" title={shown(c)}>{shown(c)}</span>
                  {tagged(c) ? <span className="dw-admin-tag">{t('chperm.privateTag')}</span> : null}
                  {botChannels.has(c.id) ? <span className="dw-admin-tag">{t('bot.channel.badge')}</span> : null}
                  {controls(c, movable.indexOf(c), movable.length)}
                </li>
              ))}
            </ul>
          </div>
        );
      })}
      <ChannelEditFlow announce={announce} channel={target?.kind === 'edit' ? live ?? null : null} categories={categories} onClose={() => setTarget(null)} />
      <DeleteChannel channel={target?.kind === 'delete' ? live ?? null : null}
        onDone={() => { focusHeading.current = true; setTarget(null); headingRef.current?.focus(); }} onClose={() => setTarget(null)} />
      <NewCategory open={target?.kind === 'category'} communityId={communityId} serverName={serverName} onClose={() => setTarget(null)} />
    </section>
  );
}

function DeleteChannel({ channel, onDone, onClose }: { channel: ChannelSummary | null; onDone: () => void; onClose: () => void }): React.JSX.Element {
  const client = useCore();
  const action = useRun();
  // Keep the last title and body while closing, so the dialog does not flash empty.
  const last = useRef<ChannelSummary | null>(null);
  if (channel !== null) last.current = channel;
  const c = channel ?? last.current;
  const kind = c?.kind === CATEGORY ? 'Category' : c?.kind === 1 ? 'Voice' : 'Text';
  const name = c?.name ?? '';
  return (
    <ConfirmDialog open={channel !== null}
      title={t(`admin.channelDelete.title${kind}`, { name })}
      body={t(kind === 'Text' ? 'admin.channelDelete.body' : `admin.channelDelete.body${kind}`)}
      confirmLabel={t('admin.channelDelete.confirm')} busyLabel={t('admin.channelDelete.busy')} cancelLabel={t('admin.cancel')}
      busy={action.busy} error={action.error} attempt={action.attempt}
      onConfirm={() => {
        if (channel === null) return;
        void action.run(() => client.call({ m: 'deleteChannel', channelId: channel.id }), e => t('admin.channelDelete.error.other', { code: e.code }))
          .then(ok => { if (ok) onDone(); });
      }}
      onClose={() => { if (!action.busy) { action.reset(); onClose(); } }} />
  );
}

function NewCategory({ open, communityId, serverName, onClose }: { open: boolean; communityId: string; serverName: string; onClose: () => void }): React.JSX.Element {
  const client = useCore();
  const [name, setName] = useState('');
  const [local, setLocal] = useState<{ message: string; n: number } | null>(null);
  const action = useRun();
  const refuse = (message: string) => setLocal(l => ({ message, n: (l?.n ?? 0) + 1 }));
  const close = () => { if (!action.busy) { setName(''); setLocal(null); action.reset(); onClose(); } };
  const submit = () => {
    const value = channelName('voice', name);
    const problem = nameProblem(value, CHANNEL_NAME_MAX_BYTES);
    if (problem !== null) {
      refuse(t(problem === 'empty' ? 'admin.category.error.empty' : problem === 'long' ? 'admin.channel.error.long' : 'create.name.control'));
      return;
    }
    setLocal(null);
    void action.run(() => client.call({ m: 'createCategory', communityId, name: value }), e => t('admin.category.error.other', { code: e.code }))
      .then(ok => { if (ok) close(); });
  };
  return (
    <ChannelSettings open={open} kind="category" name={name} onName={v => { setName(v); setLocal(null); action.reset(); }}
      topic="" onTopic={() => undefined} parent="top" parents={[]} onParent={() => undefined}
      error={local?.message ?? action.error} busy={action.busy} attempt={(local?.n ?? 0) + action.attempt}
      labels={{ title: t('admin.category.title', { server: serverName }), name: t('admin.category.name'), nameHint: t('admin.category.hint'),
        topic: '', topicHint: '', category: '', submit: t('admin.category.submit'), busy: t('admin.category.busy'), close: t('admin.cancel') }}
      onSubmit={submit} onClose={close} />
  );
}
