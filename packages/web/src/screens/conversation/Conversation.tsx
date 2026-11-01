// The open conversation: folded rows, navigation, reactions, pins and overlays.
import { phoneNow } from '../../layout.ts';
import { Fragment, useEffect, useLayoutEffect, useReducer, useRef, useState, type KeyboardEvent, type PointerEvent } from 'react';
import { encodeMentions, PERM, type MentionMember, type RoleSummary, type TimelineItem, type TimelineState } from '@dilla/client-core';
import { Banner, Button, DateDivider, Dialog, DropOverlay, EmojiGrid, MessageLog, type IconName } from '@dilla/ui';
import { useCore } from '../../core/context.tsx';
import { useRoute } from '../../router.ts';
import { REACTION_EMOJI } from '../../emoji.ts';
import { t } from '../../strings/index.ts';
import { madeHere } from '../Create.tsx';
import { authorName, editableText, excerpt, editRoles, mentionRoles, plainBody, withRoles, withSenders, type NameBook } from '../shell-model.ts';
import { attachFiles, AttachmentLightbox, type LightboxState } from './Attachments.tsx';
import { dayLabel, groupedWithAbove, startsDay } from './log-model.ts';
import { MessageItem, type ProfileOpener, type RowEvent } from './MessageItem.tsx';
import { PinsPanel } from './Pins.tsx';
import { resolveManualRemoval, sendManualRemovalEvidence, type ManualRemovalDialogState } from './manual-removal.ts';

export const FLASH_MS = 1400;
/** How far from the end, in CSS px, the log may be and still count as at the newest message (the log's own pin slack). */
const AT_PRESENT_PX = 24;
/** A scroll this soon after a wheel, touch or scroll key is the reader's (a fling keeps scrolling after the finger lifts). */
const USER_SCROLL_MS = 800;
/** A press that never clicks (a drag, a long press) stops counting as the focus's cause after this long. */
const POINTER_FOCUS_MS = 600;
/** Keys that scroll the log without moving focus between rows (the arrows, Home and End do, and reveal the row they land on). */
const SCROLL_KEYS = ['PageUp', 'PageDown'];
export interface ConversationUi {
  reply: { msgId: string; author: string; excerpt: string } | null;
  editing: { msgId: string; value: string } | null;
  pinsOpen: boolean;
  jump: { msgId: string; n: number } | null;
  notice: 'shell.message.replyNotLoaded' | null;
  trayError: string | null;
}
export type ConversationAction =
  | { type: 'restore'; ui: ConversationUi }
  | { type: 'reply'; reply: ConversationUi['reply'] } | { type: 'replySent'; msgId: string }
  | { type: 'edit'; msgId: string; value: string } | { type: 'editChange'; value: string } | { type: 'editDone' }
  | { type: 'pins'; open: boolean } | { type: 'jump'; msgId: string }
  | { type: 'notice'; notice: ConversationUi['notice'] } | { type: 'trayError'; text: string | null } | { type: 'reset' };
export const INITIAL_UI: ConversationUi = { reply: null, editing: null, pinsOpen: false, jump: null, notice: null, trayError: null };
export function conversationReducer(s: ConversationUi, a: ConversationAction): ConversationUi {
  switch (a.type) {
    case 'restore': return a.ui;
    case 'reply': return { ...s, reply: a.reply };
    case 'replySent': return s.reply?.msgId === a.msgId ? { ...s, reply: null } : s;
    case 'edit': return { ...s, editing: { msgId: a.msgId, value: a.value } };
    case 'editChange': return s.editing === null ? s : { ...s, editing: { ...s.editing, value: a.value } };
    case 'editDone': return { ...s, editing: null };
    case 'pins': return { ...s, pinsOpen: a.open };
    case 'jump': return { ...s, pinsOpen: false, jump: { msgId: a.msgId, n: (s.jump?.n ?? 0) + 1 } };
    case 'notice': return { ...s, notice: a.notice };
    case 'trayError': return { ...s, trayError: a.text };
    case 'reset': return INITIAL_UI;
  }
}
export function useConversationUi(targetId: string | null): [ConversationUi, (a: ConversationAction) => void] {
  const [ui, dispatch] = useReducer(conversationReducer, INITIAL_UI);
  const [forId, setForId] = useState(targetId);
  if (forId !== targetId) {
    setForId(targetId);
    dispatch({ type: 'reset' });
  }
  return [ui, dispatch];
}
// Not on a phone (web3.7.1): script focus does not open the keyboard there, so the composer is only ever focused by a tap.
export function focusComposer(): void { if (phoneNow()) return; document.querySelector<HTMLTextAreaElement>('.d-composer textarea')?.focus(); }
export function focusLog(): void { document.querySelector<HTMLElement>('[role="log"]')?.focus(); }

export interface ConversationProps { channelId: string; logKey: string; label: string; timeline: TimelineState | undefined;
  book: NameBook; encodeWith: readonly MentionMember[]; broadcast: boolean; bits?: number | null; roles?: readonly RoleSummary[]; pinsTitle: string; ui: ConversationUi;
  dispatch(a: ConversationAction): void; loadingEarlier: boolean; onLoadEarlier(): void; onError(e: unknown): void;
  /** Opens a message's thread (web3.7); without it a row offers no thread item and no chip. */
  onOpenThread?: (msgId: string) => void;
  /** Opens a profile card for a mentioned user or a message's author (MP7); without it neither is a button. */
  onOpenProfile?: ProfileOpener;
  /** The start of the conversation (README §5.5), shown over the first message once the whole history is here. */
  welcome?: { icon: IconName; title: string; body: string } }

export function Conversation(props: ConversationProps): React.JSX.Element {
  const client = useCore();
  const [route] = useRoute();
  const communityId = route.name === 'channel' ? route.communityId : null;
  // Role names and colours, and the broadcast right, once known; until then the shell's own book and rule stand.
  const bits = props.bits ?? null;
  const broadcast = bits !== null ? (bits & PERM.mentionEveryone) !== 0 : props.broadcast;
  const roleMentionsFor = (body: string) => mentionRoles(editRoles(props.roles ?? [], broadcast, body), props.encodeWith);
  const p = { ...props, book: withSenders(withRoles(props.book, props.roles ?? []), props.timeline?.items ?? []), broadcast };
  const items = p.timeline?.items ?? [];
  const [activeKey, setActiveKey] = useState<string | null>(null);
  const active = items.some(i => i.key === activeKey) ? activeKey : items.at(-1)?.key ?? null;
  const [emojiFor, setEmojiFor] = useState<{ key: string; opener: HTMLElement } | null>(null);
  const [deleteFor, setDeleteFor] = useState<{ key: string; msgId: string; seq: string } | null>(null);
  const [moderateFor, setModerateFor] = useState<ManualRemovalDialogState | null>(null);
  const [moderateDialogOpen, setModerateDialogOpen] = useState(false);
  const [lightbox, setLightbox] = useState<LightboxState | null>(null);
  const [flashKey, setFlashKey] = useState<string | null>(null);
  const [dragDepth, setDragDepth] = useState(0);
  const [gridPos, setGridPos] = useState<{ top: number; left: number } | null>(null);
  // R30: one revealed row at a time (phone), hidden by a second tap, Escape or a scroll of the log. `at` is when it was shown.
  const [revealFor, setRevealFor] = useState<{ log: string; key: string } | null>(null);
  const revealedKey = revealFor !== null && revealFor.log === p.logKey && items.some(i => i.key === revealFor.key) ? revealFor.key : null;
  // The reader's own scrolling (wheel, touch, scroll keys); a scroll with none of these before it is the log's own.
  const userScrollAt = useRef(0);
  const pointerTimer = useRef<number | null>(null);
  useEffect(() => () => { if (pointerTimer.current !== null) window.clearTimeout(pointerTimer.current); }, []);
  const byPointer = useRef(false); // a pointer press is under way was a pointer: a row it focuses is revealed by its tap, not by the focus
  const dragged = useRef(false); // a touch that moved is a scroll; one that did not is a tap, and a tap's reveal grows the row
  const markUserScroll = () => { userScrollAt.current = Date.now(); };
  const reveal = (key: string, on: boolean) => {
    if (on) setRevealFor({ log: p.logKey, key });
    else setRevealFor(cur => (cur?.key === key ? null : cur));
  };
  // R29: the log is away from the newest message (the "viewing older messages" bar), per log.
  const [awayFrom, setAwayFrom] = useState<string | null>(null);
  useEffect(() => { setAwayFrom(null); }, [p.logKey]);
  const rowEls = useRef(new Map<string, HTMLElement>());
  const paneRef = useRef<HTMLDivElement>(null);
  const gridRef = useRef<HTMLDivElement>(null);
  const flashTimer = useRef<number | null>(null);
  const deleted = useRef<{ key: string } | null>(null);
  const closedLightbox = useRef<HTMLElement | null>(null);
  const noticeDismissed = useRef(false);
  const now = Date.now();

  const command = (m: Parameters<typeof client.call>[0]) => { client.call(m).catch(e => p.onError(e)); };
  const closeModerateDialog = () => {
    setModerateDialogOpen(false);
    setModerateFor(current => current === null || current.phase === 'confirm' || current.evidenceUpload === 'accepted' ? null : current);
  };
  const removeForEveryone = async () => {
    const current = moderateFor;
    if (current === null || communityId === null || current.reason.trim().length === 0
      || new TextEncoder().encode(current.reason).length > 1024) return;
    setModerateFor({ ...current, phase: 'sending' });
    try {
      const result = await client.call({ m: 'moderateRemoveMessage', channelId: p.channelId, msgId: current.msgId, seq: current.seq,
        reason: current.reason, excerpt: current.excerpt }) as {
        removal?: unknown; proofRequestId?: unknown; evidenceRequestId?: unknown; evidenceUpload?: unknown;
      };
      if (result?.removal === 'unknown' && typeof result.proofRequestId === 'string') {
        setModerateFor({ ...current, phase: 'result', removal: 'unknown', proofRequestId: result.proofRequestId,
          evidenceRequestId: typeof result.evidenceRequestId === 'string' ? result.evidenceRequestId : null, evidenceUpload: 'unknown' });
        return;
      }
      if (result?.removal === 'rejected') {
        setModerateFor({ ...current, phase: 'confirm', removal: null, proofRequestId: null, evidenceRequestId: null, evidenceUpload: null });
        return;
      }
      if (result?.removal !== 'accepted' || typeof result.proofRequestId !== 'string' || typeof result.evidenceRequestId !== 'string') {
        throw new Error('E_REMOVAL_RESULT');
      }
      const evidenceUpload = result.evidenceUpload === 'accepted' || result.evidenceUpload === 'pending' || result.evidenceUpload === 'rejected'
        ? result.evidenceUpload : 'unknown';
      setModerateFor({ ...current, phase: 'result', proofRequestId: result.proofRequestId,
        evidenceRequestId: result.evidenceRequestId, evidenceUpload, removal: 'accepted' });
    } catch (error) {
      p.onError(error);
      setModerateFor({ ...current, phase: 'confirm' });
    }
  };
  const retryModerationEvidence = async () => {
    const current = moderateFor;
    if (current === null || communityId === null || current.proofRequestId === null) return;
    setModerateDialogOpen(true);
    setModerateFor({ ...current, phase: 'sending' });
    if (current.removal === 'unknown') {
      const result = await resolveManualRemoval(client, current.proofRequestId);
      if (result.removal === 'rejected') {
        setModerateFor({ ...current, phase: 'confirm', removal: null, proofRequestId: null, evidenceRequestId: null, evidenceUpload: null });
      } else setModerateFor({ ...current, phase: 'result', removal: result.removal, evidenceUpload: result.evidenceUpload });
      return;
    }
    const result = await sendManualRemovalEvidence(client, communityId, current);
    setModerateFor({ ...current, phase: 'result', removal: 'accepted', evidenceRequestId: result.requestId, evidenceUpload: result.upload });
  };
  const closeEmoji = (returnFocus: boolean) => {
    const opener = emojiFor?.opener;
    setEmojiFor(null);
    setGridPos(null);
    if (returnFocus) opener?.focus();
  };
  const jump = (msgId: string) => p.dispatch({ type: 'jump', msgId });
  // Focusing a reply button on pointer down activates its row. In a narrow log the toolbar then enters the layout
  // and moves the button before pointer up, so no click reaches it. The jump itself supplies the focus target.
  const keepJumpPointerStable = (e: PointerEvent<HTMLButtonElement>) => e.preventDefault();

  useEffect(() => {
    if (deleteFor !== null || deleted.current === null) return;
    rowEls.current.get(deleted.current.key)?.focus();
    deleted.current = null;
  }, [deleteFor]);
  useLayoutEffect(() => {
    if (lightbox !== null || closedLightbox.current === null) return;
    closedLightbox.current.focus();
    closedLightbox.current = null;
  }, [lightbox]);
  const jumpInputs = useRef({ items, dispatch: (a: ConversationAction) => p.dispatch(a) });
  jumpInputs.current = { items, dispatch: (a: ConversationAction) => p.dispatch(a) };
  const jumpTarget = p.ui.jump;
  useEffect(() => {
    const target = jumpTarget;
    if (target === null) return;
    const item = jumpInputs.current.items.find(i => i.msgId === target.msgId);
    if (item === undefined) {
      jumpInputs.current.dispatch({ type: 'notice', notice: 'shell.message.replyNotLoaded' });
      return;
    }
    setActiveKey(item.key);
    const row = rowEls.current.get(item.key);
    row?.focus();
    const behavior = window.matchMedia?.('(prefers-reduced-motion: reduce)').matches ? 'auto' : 'smooth';
    row?.scrollIntoView?.({ block: 'center', behavior });
    setFlashKey(item.key);
    if (flashTimer.current !== null) window.clearTimeout(flashTimer.current);
    flashTimer.current = window.setTimeout(() => setFlashKey(null), FLASH_MS);
  }, [jumpTarget]);
  useLayoutEffect(() => {
    if (!noticeDismissed.current || p.ui.notice !== null) return;
    noticeDismissed.current = false;
    if (active !== null) rowEls.current.get(active)?.focus();
  }, [p.ui.notice, active]);
  useEffect(() => () => { if (flashTimer.current !== null) window.clearTimeout(flashTimer.current); }, []);

  useLayoutEffect(() => {
    if (emojiFor === null || gridPos === null || gridRef.current === null || paneRef.current === null) return;
    const opener = emojiFor.opener.getBoundingClientRect();
    const pane = paneRef.current.getBoundingClientRect();
    const grid = gridRef.current.getBoundingClientRect();
    const left = Math.min(Math.max(0, opener.left - pane.left), Math.max(0, pane.width - grid.width));
    const top = opener.bottom - pane.top + grid.height <= pane.height ? opener.bottom - pane.top
      : Math.max(0, opener.top - pane.top - grid.height);
    if (left !== gridPos.left || top !== gridPos.top) setGridPos({ left, top });
  }, [emojiFor, gridPos]);

  const onRowEvent = (item: TimelineItem, ev: RowEvent) => {
    const msgId = item.msgId;
    switch (ev.kind) {
      case 'react': {
        const opener = ev.anchor.getBoundingClientRect();
        const pane = paneRef.current?.getBoundingClientRect();
        setEmojiFor({ key: item.key, opener: ev.anchor });
        setGridPos({ top: pane ? opener.bottom - pane.top : 0, left: pane ? Math.max(0, opener.left - pane.left) : 0 });
        break;
      }
      case 'reply': if (msgId !== null) p.dispatch({ type: 'reply', reply: {
        msgId, author: item.senderUser === null ? '' : authorName(item, p.book.members, p.book.self),
        excerpt: excerpt(plainBody(item.body, p.book)),
      } }); break;
      case 'edit': if (msgId !== null) p.dispatch({ type: 'edit', msgId, value: editableText(item.body, p.book) }); break;
      case 'pin': if (msgId !== null) command({ m: 'pin', channelId: p.channelId, msgId, on: ev.on }); break;
      case 'delete': if (msgId !== null) setDeleteFor({ key: item.key, msgId, seq: ev.seq }); break;
      case 'moderateRemove': if (msgId !== null) {
        setModerateFor({ key: item.key, msgId, seq: ev.seq, reason: '', excerpt: excerpt(plainBody(item.body, p.book),
          120), phase: 'confirm', proofRequestId: null, evidenceRequestId: null, evidenceUpload: null, removal: null });
        setModerateDialogOpen(true);
      } break;
      case 'toggle': if (msgId !== null) command({ m: 'react', channelId: p.channelId, msgId, emoji: ev.emoji, on: ev.on }); break;
      case 'jump': jump(ev.msgId); break;
      case 'retry': command({ m: 'retrySend', msgId: ev.msgId }); break;
      case 'discard': command({ m: 'discardSend', msgId: ev.msgId }); break;
    }
  };
  const jumpToPresent = () => {
    const log = paneRef.current?.querySelector<HTMLElement>('[role="log"]');
    if (!log) return;
    log.scrollTop = log.scrollHeight;
    const newest = items.at(-1);
    if (newest !== undefined) rowEls.current.get(newest.key)?.focus({ preventScroll: true });
  };
  // Fix round 2 (Jonas, the prototype's "Jump to present"): Ctrl+End jumps the open conversation to its newest message from
  // anywhere in the tab, not only from inside the log. A text field keeps the chord (it moves the caret to the end), a modal
  // layer keeps it, and at the present it does nothing (the key keeps its default).
  const jumpKey = useRef(jumpToPresent);
  jumpKey.current = jumpToPresent;
  const awayNow = useRef(false);
  awayNow.current = awayFrom === p.logKey;
  useEffect(() => {
    const onKey = (e: globalThis.KeyboardEvent) => {
      if (e.key !== 'End' || !e.ctrlKey || e.altKey || e.metaKey || e.shiftKey || e.isComposing || e.defaultPrevented) return;
      const target = e.target;
      if (target instanceof HTMLElement && (target.isContentEditable || target.closest('input, textarea, select') !== null)) return;
      // Fix round 3: not under any modal layer: a <dialog>, an aria-modal overlay (search, pickers) or a sheet's backdrop.
      if (document.querySelector('dialog[open], [aria-modal="true"]:not(dialog), .d-app-shell__backdrop') !== null || !awayNow.current) return;
      e.preventDefault();
      jumpKey.current();
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, []);
  const onLogScroll = (e: React.UIEvent<HTMLElement>) => {
    const log = e.target as HTMLElement;
    if (!(log instanceof HTMLElement) || log.getAttribute('role') !== 'log') return;
    const away = log.scrollHeight - log.scrollTop - log.clientHeight > AT_PRESENT_PX;
    setAwayFrom(away ? p.logKey : null);
    // Only the reader's own scroll hides it: a message arriving re-pins the log, and the log keeps the end in view under a row that grew.
    if (Date.now() - userScrollAt.current < USER_SCROLL_MS) setRevealFor(null);
  };
  const rowKey = (e: KeyboardEvent<HTMLElement>) => {
    if (e.key === 'Escape') setRevealFor(null);
    if (e.altKey || e.ctrlKey || e.metaKey || e.shiftKey) return;
    const target = e.target as HTMLElement;
    if (e.key === 'Escape') {
      if (target.closest('.d-message-editor, .d-emoji-grid')) return;
      if (target === e.currentTarget || target.classList.contains('d-message-row')) focusComposer();
      else target.closest<HTMLElement>('.d-message-row')?.focus();
      e.preventDefault();
      return;
    }
    if (!['ArrowUp', 'ArrowDown', 'Home', 'End'].includes(e.key)) return;
    if (target !== e.currentTarget && !target.classList.contains('d-message-row')) return;
    const from = target.classList.contains('d-message-row') ? items.findIndex(i => rowEls.current.get(i.key) === target)
      : items.findIndex(i => i.key === active);
    const index = e.key === 'Home' ? 0 : e.key === 'End' ? items.length - 1 : e.key === 'ArrowUp' ? Math.max(0, from - 1) : Math.min(items.length - 1, from + 1);
    const next = target === e.currentTarget && (e.key === 'ArrowUp' || e.key === 'ArrowDown') ? items.find(i => i.key === active) : items[index];
    if (next) { e.preventDefault(); rowEls.current.get(next.key)?.focus(); }
  };
  const fileDrag = (e: React.DragEvent) => Array.from(e.dataTransfer.types).includes('Files');
  const openImage = (item: TimelineItem, index: number, opener: HTMLElement) => {
    if (item.seq === null) return;
    const images = item.attachments.filter(a => a.kind === 'image' && !a.tooLarge);
    const position = images.findIndex(a => a.index === index);
    if (position >= 0) setLightbox({ seq: item.seq, images, index: position, opener });
  };
  const closeLightbox = () => { closedLightbox.current = lightbox?.opener ?? null; setLightbox(null); };

  return <div ref={paneRef} className="dw-conversation" data-channel-id={p.channelId} onScrollCapture={onLogScroll}
    onWheelCapture={markUserScroll} onTouchStartCapture={() => { dragged.current = false; }}
    onTouchMoveCapture={() => { dragged.current = true; markUserScroll(); }}
    onTouchEndCapture={() => { if (dragged.current) markUserScroll(); }}
    onPointerDownCapture={() => {
      // A pointer's press focuses a row between its pointerdown and its click (a touch's compatibility mousedown comes after
      // pointerup); a focus() by script after the click is not the pointer's.
      byPointer.current = true;
      if (pointerTimer.current !== null) window.clearTimeout(pointerTimer.current);
      pointerTimer.current = window.setTimeout(() => { byPointer.current = false; }, POINTER_FOCUS_MS);
    }}
    onClickCapture={() => { byPointer.current = false; }}
    onKeyDownCapture={e => { byPointer.current = false; if (SCROLL_KEYS.includes(e.key)) markUserScroll(); }}
    onFocus={e => { const row = (e.target as Element).closest<HTMLElement>('.d-message-row'); if (row) {
      const key = items.find(i => rowEls.current.get(i.key) === row)?.key;
      if (key) setActiveKey(key);
      // I1: a row that takes keyboard focus shows its actions on a phone layout, as focus-within did when they were always there.
      // A tap focuses the row too, but by pointer: the tap itself reveals it (and a second tap hides it).
      if (key && e.target === row && !byPointer.current) reveal(key, true);
    } }}
    onDragEnter={e => { if (fileDrag(e)) { e.preventDefault(); setDragDepth(n => n + 1); } }}
    onDragOver={e => { if (fileDrag(e)) e.preventDefault(); }}
    onDragLeave={e => { if (fileDrag(e)) setDragDepth(n => Math.max(0, n - 1)); }}
    onDrop={e => { if (!fileDrag(e)) return; e.preventDefault(); setDragDepth(0);
      if (bits !== null && (bits & PERM.sendMessages) === 0) return;
      void attachFiles(client, p.channelId, Array.from(e.dataTransfer.files), a => p.dispatch(a)); }}>
    {p.ui.notice !== null ? <Banner tone="info">{t(p.ui.notice)} <Button variant="ghost" onClick={() => {
      noticeDismissed.current = true; p.dispatch({ type: 'notice', notice: null });
    }}>{t('shell.banner.dismiss')}</Button></Banner> : null}
    <MessageLog key={p.logKey} label={p.label} emptyLabel={p.timeline === undefined ? t('shell.log.loading')
      : t(madeHere(client, p.channelId) ? 'shell.log.emptyMade' : 'shell.log.empty')}
      earlier={p.timeline?.hasEarlier ? { label: t('shell.log.earlier'), onLoad: () => p.onLoadEarlier() } : undefined}
      busy={p.timeline === undefined || p.loadingEarlier} onKeyDown={rowKey}
      present={{ atPresent: awayFrom !== p.logKey, olderLabel: t('layout.log.older'), jumpLabel: t('layout.log.jump'), onJumpToPresent: jumpToPresent }}
      {...(p.welcome !== undefined && p.timeline !== undefined && !p.timeline.hasEarlier ? { welcome: p.welcome } : {})}>
      {items.map((item, index) => <Fragment key={item.key}>
        {startsDay(items[index - 1], item) ? <DateDivider label={dayLabel(item.ts, now)} /> : null}
        <MessageItem item={item} channelId={p.channelId} book={p.book} now={now} bits={bits} client={client} communityId={communityId} threadId={null}
        grouped={groupedWithAbove(items[index - 1], item)} revealed={item.key === revealedKey} onReveal={on => reveal(item.key, on)}
        replying={item.msgId !== null && p.ui.reply?.msgId === item.msgId}
        active={item.key === active} flash={item.key === flashKey} editing={p.ui.editing?.msgId === item.msgId ? p.ui.editing.value : null}
        editMeasure={v => new TextEncoder().encode(encodeMentions(v, p.encodeWith, p.broadcast, roleMentionsFor(item.body))).length}
        rowRef={el => { if (el) rowEls.current.set(item.key, el); else rowEls.current.delete(item.key); }}
        onEvent={onRowEvent} onJumpPointerDown={keepJumpPointerStable} onEditChange={value => p.dispatch({ type: 'editChange', value })}
        onEditSave={value => {
          if (value.trim() === '') return;
          const start = editableText(item.body, p.book);
          p.dispatch({ type: 'editDone' });
          rowEls.current.get(item.key)?.focus();
          if (value !== start && item.msgId) command({ m: 'editMessage', channelId: p.channelId, msgId: item.msgId, text: encodeMentions(value, p.encodeWith, p.broadcast, roleMentionsFor(item.body)) });
        }}
        onEditCancel={() => { p.dispatch({ type: 'editDone' }); rowEls.current.get(item.key)?.focus(); }}
        onOpenImage={(index, opener) => openImage(item, index, opener)} onError={e => p.onError(e)}
        {...(p.onOpenThread !== undefined ? { onOpenThread: p.onOpenThread } : {})}
        {...(p.onOpenProfile !== undefined ? { onOpenProfile: p.onOpenProfile } : {})} />
      </Fragment>)}
    </MessageLog>
    <div className="dw-overlay">{emojiFor !== null && gridPos !== null ? <div ref={gridRef} style={gridPos}
      onBlur={e => { if (!e.currentTarget.contains(e.relatedTarget)) closeEmoji(false); }}>
      <EmojiGrid label={t('shell.emoji.label')} items={REACTION_EMOJI.map(a => ({ emoji: a.emoji, name: t(a.key) }))}
        onPick={emoji => {
          const item = items.find(i => i.key === emojiFor.key);
          if (item?.msgId) command({ m: 'react', channelId: p.channelId, msgId: item.msgId, emoji,
            on: !item.reactions.some(r => r.emoji === emoji && r.mine) });
          closeEmoji(true);
        }} onClose={() => closeEmoji(true)} />
    </div> : null}</div>
    <DropOverlay active={dragDepth > 0} title={t('shell.drop.title')} body={t('shell.drop.body')} />
    <p className="dw-drop-status d-sr-only" role="status">{dragDepth > 0 ? t('shell.drop.title') : ''}</p>
    <Dialog open={deleteFor !== null} title={t('shell.delete.title')} closeLabel={t('shell.delete.cancel')}
      onClose={() => setDeleteFor(null)} footer={<Button variant="danger" onClick={() => {
        if (deleteFor === null) return;
        command({ m: 'deleteMessage', channelId: p.channelId, msgId: deleteFor.msgId, seq: deleteFor.seq });
        deleted.current = { key: deleteFor.key };
        setDeleteFor(null);
      }}>{t('shell.delete.confirm')}</Button>}>{t('shell.delete.body')}</Dialog>
    <Dialog open={moderateDialogOpen && moderateFor !== null} title={t('shell.moderation.removeTitle')}
      closeLabel={moderateFor?.phase === 'sending' ? t('shell.moderation.submitting') : t('shell.delete.cancel')}
      onClose={() => { if (moderateFor?.phase !== 'sending') closeModerateDialog(); }}
      footer={moderateFor?.phase === 'confirm' ? <Button variant="danger" aria-disabled={moderateFor.reason.trim().length === 0
        || new TextEncoder().encode(moderateFor.reason).length > 1024 || undefined} onClick={() => void removeForEveryone()}>
        {t('shell.moderation.removeConfirm')}</Button>
        : moderateFor?.phase === 'sending' ? <Button aria-disabled="true">{t('shell.moderation.submitting')}</Button>
          : moderateFor?.evidenceUpload === 'accepted' ? <Button onClick={closeModerateDialog}>{t('shell.moderation.done')}</Button>
            : <Button variant="danger" onClick={() => void retryModerationEvidence()}>{moderateFor?.removal === 'unknown' ? t('shell.moderation.retryRemoval') : t('shell.moderation.retryEvidence')}</Button>}>
      {moderateFor?.phase === 'confirm' ? <>
        <p>{t('shell.moderation.removeBody')}</p>
        <label>{t('shell.moderation.reason')}<textarea maxLength={1024} value={moderateFor.reason}
          onChange={e => setModerateFor(current => current === null ? null : { ...current, reason: e.target.value })} /></label>
        <p><strong>{t('shell.moderation.excerptLabel')}</strong> {moderateFor.excerpt || t('shell.moderation.emptyExcerpt')}</p>
      </> : moderateFor?.phase === 'sending' ? <p>{t('shell.moderation.submitting')}</p>
        : moderateFor?.evidenceUpload === 'accepted' ? <p role="status">{t('shell.moderation.evidenceSent')}</p>
          : moderateFor?.evidenceUpload === 'pending' ? <p role="status">{t('shell.moderation.evidencePending')}</p>
            : moderateFor?.evidenceUpload === 'rejected' ? <p role="alert">{t('shell.moderation.evidenceRejected')}</p>
              : <p role="alert">{moderateFor?.removal === 'unknown' ? t('shell.moderation.removalUnknown') : t('shell.moderation.evidenceUnknown')}</p>}
    </Dialog>
    {moderateFor !== null && !moderateDialogOpen && moderateFor.phase === 'result' ? <div role="status" className="d-moderation-evidence-status">
      <p>{moderateFor.evidenceUpload === 'accepted' ? t('shell.moderation.evidenceSent')
        : moderateFor.evidenceUpload === 'pending' ? t('shell.moderation.evidencePending')
          : moderateFor.evidenceUpload === 'rejected' ? t('shell.moderation.evidenceRejected')
            : moderateFor.removal === 'unknown' ? t('shell.moderation.removalUnknown') : t('shell.moderation.evidenceUnknown')}</p>
      {moderateFor.evidenceUpload === 'accepted' ? <Button onClick={closeModerateDialog}>{t('shell.moderation.done')}</Button>
        : <Button onClick={() => void retryModerationEvidence()}>{moderateFor.removal === 'unknown' ? t('shell.moderation.retryRemoval') : t('shell.moderation.retryEvidence')}</Button>}
    </div> : null}
    <PinsPanel channelId={p.channelId} open={p.ui.pinsOpen} title={p.pinsTitle} note={bits !== null ? t('perm.pinsNote') : undefined} canUnpin={bits === null || (bits & PERM.pinMessages) !== 0} book={p.book} items={items} now={now}
      onClose={() => p.dispatch({ type: 'pins', open: false })} onJump={jump} onError={e => p.onError(e)} />
    {lightbox !== null ? <AttachmentLightbox channelId={p.channelId} state={lightbox}
      onIndex={index => setLightbox(s => s ? { ...s, index } : null)} onClose={closeLightbox} onError={e => p.onError(e)} /> : null}
  </div>;
}
