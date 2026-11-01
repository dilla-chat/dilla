// The open conversation's draft, mentions, replies and file tray.
import { useEffect, useId, useLayoutEffect, useReducer, useRef, useState, type ReactNode } from 'react';
import { encodeMentions, mentionQuery, PERM, type MemberSummary, type MentionMember, type RoleSummary, type SlowState, type TimelineItem } from '@dilla/client-core';
import { Composer, EmojiGrid, MentionList, ReplyChip } from '@dilla/ui';
import { REACTION_EMOJI } from '../../emoji.ts';
import { useCore } from '../../core/context.tsx';
import { useSlice } from '../../core/use-slice.ts';
import { useRoute } from '../../router.ts';
import { t } from '../../strings/index.ts';
import { editableText, mentionOptions, mentionRoles, offeredRoles, type NameBook } from '../shell-model.ts';
import { attachFiles, TrayList } from './Attachments.tsx';
import { GifPickerPanel } from './GifPicker.tsx';
import type { ConversationAction, ConversationUi } from './Conversation.tsx';
import { formatWait, spokenStep } from './slow.ts';
import { CommandHost } from '../commands/CommandHost.tsx';

export interface OutgoingMessage { raw: string; text: string; replyTo: string | null; attachments: readonly string[];
  /** The thread a send belongs to (the thread composer); absent for the channel's own composer. */
  threadId?: string | null }
export interface ComposerAreaProps { channelId: string; label: string; placeholder: string; blocked: string | null;
  /** Today's broadcast rule; `bits` (what I may do here, null while unknown) overrides it once known. */
  broadcast: boolean; bits?: number | null; roles?: readonly RoleSummary[]; channelName?: string; encodeWith: readonly MentionMember[]; candidates: readonly MemberSummary[]; items: readonly TimelineItem[];
  book: NameBook; draft: string; onDraft(text: string): void; ui: ConversationUi; dispatch(a: ConversationAction): void;
  onSend(m: OutgoingMessage): void; onError(e: unknown): void;
  /** The channel's slow-mode state; when absent the `slow:<channel>` slice stands in. */
  slow?: SlowState;
  /** True: a person who bypasses slow mode (no countdown). When absent, the channel bits say (bypass_slowmode). */
  bypass?: boolean;
  /** The thread this composer replies in: its sends carry it. */
  threadId?: string;
  /** True: an announcement channel. When absent the channel's `announcement` flag in the channels slice says. */
  announcement?: boolean;
  /** Speaks a queued send once, politely (the shell's announcer). */
  announce?: (text: string) => void;
  /** Someone else typing here: the footer's TypingIndicator (README §5.5). */
  typing?: ReactNode }

export function ComposerArea(p: ComposerAreaProps): React.JSX.Element {
  const client = useCore();
  const tray = useSlice(`tray:${p.channelId}`) ?? [];
  const selfId = useSlice('account')?.user?.id ?? null;
  const [commandMode, setCommandMode] = useState(false);
  const [slashSubmission, setSlashSubmission] = useState<{ text: string; id: number } | null>(null);
  const slashSubmissionId = useRef(0);
  const [caret, setCaret] = useState(0);
  const [active, setActive] = useState(0);
  const [dismissed, setDismissed] = useState<number | null>(null);
  const textareaRef = useRef<HTMLTextAreaElement>(null);
  const inputRef = useRef<HTMLInputElement>(null);
  const pendingCaret = useRef<number | null>(null);
  const id = useId().replace(/:/g, '-');
  const bits = p.bits ?? null;
  const sliceSlow = useSlice(`slow:${p.channelId}`);
  const [route] = useRoute();
  // E8: the GIF button exists only in a channel of a server that has a Giphy key; a direct message has none, and
  // asks nothing, and a thread's composer has none. The key is a per-server setting, so the status is read per server (a failed read means no button).
  const gifCommunity = route.name === 'channel' && p.threadId === undefined ? route.communityId : null;
  const [gifStatus, setGifStatus] = useState<{ communityId: string; on: boolean } | null>(null);
  const [gifOpen, setGifOpen] = useState(false);
  const [gifQuery, setGifQuery] = useState('');
  const gifButton = useRef<HTMLButtonElement>(null);
  // Task 7 (README §5.5): the footer's emoji picker inserts at the caret; Escape gives focus back to its button.
  const [emojiOpen, setEmojiOpen] = useState(false);
  const emojiButton = useRef<HTMLButtonElement>(null);
  const insertEmoji = (emoji: string) => {
    const el = textareaRef.current;
    const from = el?.selectionStart ?? p.draft.length;
    const to = el?.selectionEnd ?? p.draft.length;
    pendingCaret.current = from + emoji.length;
    p.onDraft(p.draft.slice(0, from) + emoji + p.draft.slice(to));
  };
  useEffect(() => {
    if (gifCommunity === null) return;
    let cancelled = false;
    void client.call({ m: 'gifStatus', communityId: gifCommunity }).then(value => {
      const on = typeof value === 'object' && value !== null && (value as Record<string, unknown>).on === true;
      if (!cancelled) setGifStatus({ communityId: gifCommunity, on });
    }, () => { if (!cancelled) setGifStatus({ communityId: gifCommunity, on: false }); });
    return () => { cancelled = true; };
  }, [client, gifCommunity]);
  const gifsOn = gifCommunity !== null && gifStatus?.communityId === gifCommunity && gifStatus.on;
  const channels = useSlice(`channels:${route.name === 'channel' ? route.communityId : ''}`);
  const announcement = p.announcement ?? channels?.find(c => c.id === p.channelId)?.announcement === true;
  const bypass = p.bypass ?? (bits !== null && (bits & PERM.bypassSlowmode) !== 0);
  const slow = p.slow ?? sliceSlow;
  // The countdown: a timer re-renders once a second until the wait is over. A slice whose time has passed is over, queued or not.
  const [, tick] = useReducer((n: number) => n + 1, 0);
  const until = bypass ? 0 : slow?.until ?? 0;
  const remaining = until - Date.now();
  const waiting = remaining > 0;
  useEffect(() => {
    if (until <= Date.now()) return;
    // The visible text ticks each second; the end is its own timer, so "over" is exact.
    const id = window.setInterval(tick, 1000);
    // A timer may fire a fraction of a millisecond early: re-arm for what is left rather than end with "0:01" on screen.
    let end = 0;
    const arm = () => {
      end = window.setTimeout(() => {
        if (Date.now() < until) { arm(); return; }
        window.clearInterval(id);
        tick();
      }, Math.max(0, until - Date.now()));
    };
    arm();
    return () => { window.clearInterval(id); window.clearTimeout(end); };
  }, [until]);
  // The visible text changes each second and is hidden from the assistive tree; a polite status speaks on a change of
  // minute or of 10 s step only (flow 11).
  const waitText = waiting ? t('slow.wait', { time: formatWait(remaining) }) : '';
  const step = waiting ? spokenStep(remaining) : '';
  const [spoken, setSpoken] = useState({ step: '', text: '' });
  if (spoken.step !== step) setSpoken({ step, text: waitText });
  const queued = waiting && slow?.queued === true;
  const queuedBefore = useRef(false);
  const announceQueued = p.announce;
  useEffect(() => {
    if (queued && !queuedBefore.current) announceQueued?.(t('slow.queued'));
    queuedBefore.current = queued;
  }, [queued, announceQueued]);
  const broadcast = bits !== null ? (bits & PERM.mentionEveryone) !== 0 : p.broadcast;
  const offered = offeredRoles(p.roles ?? [], broadcast);
  const roleMentions = mentionRoles(offered, p.encodeWith);
  const query = mentionQuery(p.draft, caret);
  const queryEnded = query === null;
  const options = query === null ? [] : mentionOptions(query.query, p.candidates, broadcast, offered);
  const open = query !== null && options.length > 0 && query.start !== dismissed;
  const index = Math.min(active, options.length - 1);
  const listId = `${id}-mentions`;
  const optionId = (o: { id: string }) => `${id}-${o.id}`;
  useLayoutEffect(() => {
    if (queryEnded && dismissed !== null) setDismissed(null);
  }, [queryEnded, dismissed]);
  useLayoutEffect(() => {
    if (pendingCaret.current === null) return;
    const next = pendingCaret.current;
    pendingCaret.current = null;
    textareaRef.current?.setSelectionRange(next, next);
    setCaret(next);
    textareaRef.current?.focus();
  }, [p.draft]);
  useLayoutEffect(() => {
    if (p.ui.reply !== null) textareaRef.current?.focus();
  }, [p.ui.reply]);
  const insert = (option: typeof options[number]) => {
    if (query === null) return;
    const text = p.draft.slice(0, query.start) + option.insert + p.draft.slice(caret);
    pendingCaret.current = query.start + option.insert.length;
    setDismissed(query.start);
    p.onDraft(text);
  };
  const onKey = (key: 'ArrowUp' | 'ArrowDown' | 'Enter' | 'Tab' | 'Escape'): boolean => {
    if (open) {
      if (key === 'Escape') { setDismissed(query.start); return true; }
      if (key === 'ArrowDown') { setActive(i => (i + 1) % options.length); return true; }
      if (key === 'ArrowUp') { setActive(i => (i - 1 + options.length) % options.length); return true; }
      if (key === 'Enter' || key === 'Tab') { insert(options[index]); return true; }
    } else if (key === 'Escape' && p.ui.reply !== null) {
      p.dispatch({ type: 'reply', reply: null });
      textareaRef.current?.focus();
      return true;
    }
    return false;
  };
  const send = (value: string, explicitLiteral = false) => {
    if (value.startsWith('/') && !explicitLiteral) {
      setSlashSubmission({ text: value, id: ++slashSubmissionId.current });
      return;
    }
    if (tray.some(a => a.phase !== 'ready')) {
      p.dispatch({ type: 'trayError', text: t('shell.tray.notReady') });
      return;
    }
    p.dispatch({ type: 'trayError', text: null });
    p.onSend({ raw: value, text: encodeMentions(value, p.encodeWith, broadcast, roleMentions), replyTo: p.ui.reply?.msgId ?? null,
      attachments: tray.map(a => a.id), ...(p.threadId !== undefined ? { threadId: p.threadId } : {}) });
  };
  // Advisory: the instance decides, and W2b enforces it. The composer stays mounted, read-only with its reason (the draft,
  // the file tray, the reply chip and the focus all stay); the shell speaks the state once through its announcer.
  const cannotSend = bits !== null && (bits & PERM.sendMessages) === 0;
  const readOnly = cannotSend && announcement;
  const readOnlyText = t('announce.readOnly', { name: p.channelName ?? '' });
  const blocked = p.blocked ?? (readOnly ? readOnlyText : cannotSend ? t('chperm.cannotSend', { name: p.channelName ?? '' }) : null);
  // The read-only text is the disabled box's reason (below it): a notice above would say it twice.
  const noticeBody = readOnly ? null : waiting ? <>
    <span aria-hidden="true">{waitText}</span>{queued ? <span> {t('slow.queued')}</span> : null}
  </> : null;
  return <>
    <CommandHost communityId={route.name === 'channel' ? route.communityId : null} channelId={p.channelId}
      {...(p.threadId !== undefined ? { threadId: p.threadId } : {})} {...(bits !== null ? { bits } : {})}
      items={p.items} selfId={selfId} draft={p.draft} onDraft={text => p.onDraft(text)}
      encodeMessage={text => encodeMentions(text, p.encodeWith, broadcast, roleMentions)} openGif={query => { setGifQuery(query); setGifOpen(true); }} gifsEnabled={gifsOn}
      onSendLiteral={text => { p.onDraft(''); send(text, true); }} onModeChange={setCommandMode} slashSubmission={slashSubmission} />
    <div className="d-composer-pop">
    {emojiOpen && blocked === null ? <EmojiGrid id={`${id}-emoji`} label={t('shell.composer.emoji')} items={REACTION_EMOJI.map(a => ({ emoji: a.emoji, name: t(a.key) }))}
      search={{ label: t('shell.composer.emojiSearch'), placeholder: t('shell.composer.emojiSearch') }}
      onPick={emoji => { setEmojiOpen(false); insertEmoji(emoji); }}
      onClose={() => { setEmojiOpen(false); emojiButton.current?.focus(); }} /> : null}
    {gifsOn && gifOpen && blocked === null && gifCommunity !== null ? <GifPickerPanel id={`${id}-gifs`} communityId={gifCommunity} channelId={p.channelId} initialQuery={gifQuery} dispatch={a => { p.dispatch(a); }}
      {...(p.announce !== undefined ? { announce: p.announce } : {})}
      onClose={reason => {
        setGifOpen(false);
        if (reason === 'escape') gifButton.current?.focus();
        else if (reason === 'picked') textareaRef.current?.focus();
      }} /> : null}
    </div>
    <input ref={inputRef} className="dw-attach-input" type="file" multiple hidden
      onChange={e => { void attachFiles(client, p.channelId, Array.from(e.currentTarget.files ?? []), a => p.dispatch(a)); e.currentTarget.value = ''; }} />
    {!commandMode ? <Composer label={p.label} placeholder={p.placeholder} maxLength={4000} value={p.draft}
      onChange={text => { p.onDraft(text); setActive(0);
        setCaret(textareaRef.current?.selectionStart ?? text.length); }}
      disabled={blocked !== null} disabledReason={blocked ?? undefined} onSend={send}
      sendLabel={t('shell.composer.send')}
      counterLabel={n => n < 0 ? t('shell.composer.over', { n: -n }) : t('shell.composer.remaining', { n })}
      attachLabel={t('shell.composer.attach')} onAttach={() => inputRef.current?.click()}
      {...(gifsOn ? { gif: { label: t('gif.button'), open: gifOpen && blocked === null, controls: `${id}-gifs`, onToggle: () => { setEmojiOpen(false); setGifOpen(o => !o); }, buttonRef: gifButton } } : {})}
      emoji={{ label: t('shell.composer.emoji'), open: emojiOpen && blocked === null, controls: `${id}-emoji`, onToggle: () => { setGifOpen(false); setEmojiOpen(o => !o); }, buttonRef: emojiButton }}
      codeLabel={t('shell.composer.code')} hint={t('shell.composer.newLine')} typing={p.typing} replying={p.ui.reply !== null}
      {...(noticeBody !== null ? { notice: noticeBody } : {})}
      top={<>
        {p.ui.reply !== null ? <ReplyChip label={t('shell.composer.replying', { name: p.ui.reply.author })} name={p.ui.reply.author}
          excerpt={p.ui.reply.excerpt} cancelLabel={t('shell.composer.replyCancel')}
          onCancel={() => { p.dispatch({ type: 'reply', reply: null }); textareaRef.current?.focus(); }} /> : null}
        <TrayList channelId={p.channelId} items={tray} onError={e => p.onError(e)} />
        {p.ui.trayError !== null ? <p className="dw-tray-error d-composer__alert" role="alert">{p.ui.trayError}</p> : null}
        {open ? <MentionList id={listId} label={t('shell.composer.mentions')} title={t('shell.composer.mentionsTitle')}
          options={options.map(o => ({ id: optionId(o), primary: o.primary, secondary: o.secondary, person: o.id.startsWith('m-') }))}
          activeId={optionId(options[index])} onPick={picked => { const found = options.find(o => optionId(o) === picked); if (found) insert(found); }} /> : null}
      </>}
      measure={value => new TextEncoder().encode(encodeMentions(value, p.encodeWith, broadcast, roleMentions)).length}
      combobox={{ expanded: open, controls: listId, activeDescendant: open ? optionId(options[index]) : null, onKey: key => blocked === null && onKey(key) }}
      onCaret={setCaret} onArrowUpEmpty={() => {
        if (blocked !== null) return;
        const item = [...p.items].reverse().find(i => i.senderUser === p.book.self?.id && i.state === 'ok' && i.msgId !== null && i.seq !== null);
        if (item?.msgId) p.dispatch({ type: 'edit', msgId: item.msgId, value: editableText(item.body, p.book) });
      }}
      onPasteFiles={files => { void attachFiles(client, p.channelId, files, a => p.dispatch(a)); }} textareaRef={textareaRef}
      canSendEmpty={tray.length > 0} /> : null}
    <p className="d-sr-only" role="status" data-slow="">{spoken.text}</p>
  </>;
}
