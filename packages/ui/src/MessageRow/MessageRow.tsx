import { useId, useRef, useState, type KeyboardEvent, type MouseEvent, type PointerEvent, type ReactNode, type Ref } from 'react';
import { Avatar } from '../Avatar/Avatar.tsx';
import { Tag } from '../Tag/Tag.tsx';
import { Button } from '../Button/Button.tsx';
import { Icon } from '../Icon/Icon.tsx';
import { usePhoneLayout } from '../internal/phoneLayout.ts';
import { RoleDot } from '../internal/RoleDot.tsx';
import { CodeBlock, type CodeBlockLabels } from '../CodeBlock/CodeBlock.tsx';
import './MessageRow.css';

/**
 * A piece of a message body: plain text, a mention shown as a pill whose text is its `@` name, or markdown (E5). The
 * formatting parts nest; code, code blocks and links carry only text. Nothing here is HTML: the row renders each as a React element.
 */
export type BodyPart =
  | { kind: 'text'; text: string }
  | { kind: 'mention'; label: string; me: boolean; broadcast: boolean; /** A role mention's colour, 0xRRGGBB. Null or 0: no dot. */ color?: number | null; /** A user mention's id: with `onOpenProfile` the pill is a button that opens that user's card. Role and broadcast pills have none. */ userId?: string }
  | { kind: 'strong' | 'em' | 'strike' | 'quote'; children: readonly BodyPart[] }
  | { kind: 'code'; text: string }
  /** `lang` is the fence's language word, shown on the block. */
  | { kind: 'codeblock'; text: string; lang?: string }
  /** A link. The row shows the href itself as its text (never `text`), isolated left to right, and renders an anchor only for an `http:`, `https:` or bare `mailto:` href; any other is plain `text`. */
  | { kind: 'link'; href: string; text: string };

export interface MessageRowProps {
  author: string;
  /** The author's id: with `onOpenProfile` the name and the avatar open the author's profile card. */
  authorId?: string;
  /** Opens a user's profile card, anchored to `anchor` (a mention pill, the author name or the avatar). */
  onOpenProfile?(userId: string, anchor: HTMLElement, from: 'mention' | 'author'): void;
  time: string;
  /** The time of day shown in the gutter of a grouped row (the day divider above names the day). Falls back to `time`; when given, `time` rides as the gutter's title. */
  gutterTime?: string;
  body: string | readonly BodyPart[];
  own?: boolean;
  tag?: 'web' | 'bot';
  state: 'ok' | 'pending' | 'failed' | 'cannot-read' | 'deleted';
  stateLabel?: string;
  detail?: string;
  actions?: readonly { label: string; onAction(): void }[];
  active?: boolean;
  editedLabel?: string;
  pinnedLabel?: string;
  reply?: {
    label: string; author: string; excerpt: string; state: 'ok' | 'missing' | 'deleted'; stateText?: string; jumpLabel: string; onJump?(): void;
    onJumpPointerDown?(e: PointerEvent<HTMLButtonElement>): void;
  };
  attachments?: ReactNode;
  /** A verified interactive payload such as a poll, rendered inside the message's main column. */
  card?: ReactNode;
  reactions?: ReactNode;
  /** A public thread's summary under the body: a chip button named by `label` ("2 replies") that opens it. */
  thread?: { label: string; onOpen(): void; /** "last 20:33" after the count (README §5.5), seen only: the name stays the count. */ last?: string };
  toolbar?: ReactNode;
  editor?: ReactNode;
  pendingAction?: { label: string; tone: 'pending' | 'failed'; actions?: readonly { label: string; onAction(): void }[] };
  flash?: boolean;
  mention?: boolean;
  onKeyDown?(e: KeyboardEvent): void;
  rowRef?: Ref<HTMLElement>;
  seq?: string | null;
  msgId?: string | null;
  /** The row continues the author's run above it (the caller decides): the author and time are hidden, the full time shows in the gutter on hover or focus. */
  grouped?: boolean;
  /** Phone only: the toolbar is rendered only while revealed. Off a phone the toolbar is always rendered. */
  revealed?: boolean;
  /** Phone only: a tap on the row body or Enter on the row asks to reveal (true); Escape or a second tap asks to hide (false). */
  onReveal?(on: boolean): void;
  /** The author's name colour (their top role's, 0xRRGGBB); none or 0: the text colour. A bot's name is the accent's second tone. */
  nameColor?: number | null;
  /** The labels of a fenced code block's header (README §6.4); without them a block shows its language only. */
  codeLabels?: CodeBlockLabels;
  /** The message the composer is replying to: tinted, with the accent bar (README §5.5). */
  replying?: boolean;
  /** Stories only: draw the hover state (the fill, the bar, the toolbar and the gutter time) without a pointer. */
  forceHover?: boolean;
}

/** A click that starts on one of these is that control's own, not a tap on the row body. */
const INTERACTIVE = 'button, a, [data-profile="true"], input, textarea, select, summary, label, [role="button"], [role="toolbar"], [contenteditable="true"]';

const flag = (on: boolean | undefined): 'true' | undefined => (on === true ? 'true' : undefined);

const SAFE_HREF = /^(?:https?:\/\/[^\s/]\S*|mailto:[^\s?]+)$/i;

/** The body's parts as text children and mention pills: never HTML, never the raw token. A user pill is a button when the caller can open a profile. */
function bodyChildren(body: string | readonly BodyPart[], tabIndex: number, onOpenProfile: MessageRowProps['onOpenProfile']): ReactNode {
  if (typeof body === 'string') return body;
  return body.map((part, i) => {
    switch (part.kind) {
      case 'text': return part.text;
      case 'strong': return <strong key={i}>{bodyChildren(part.children, tabIndex, onOpenProfile)}</strong>;
      case 'em': return <em key={i}>{bodyChildren(part.children, tabIndex, onOpenProfile)}</em>;
      case 'strike': return <s key={i}>{bodyChildren(part.children, tabIndex, onOpenProfile)}</s>;
      case 'quote': return <blockquote key={i} className="d-message-row__quote">{bodyChildren(part.children, tabIndex, onOpenProfile)}</blockquote>;
      case 'code': return <code key={i} className="d-message-row__code">{part.text}</code>;
      case 'codeblock': return <pre key={i} className="d-message-row__codeblock" data-lang={part.lang}><code>{part.text}</code></pre>;
      case 'link':
        // Defence in depth: the parser already refuses other schemes; an href that is not exactly http(s) or mailto is shown as the text it came with.
        if (!SAFE_HREF.test(part.href)) return part.text;
        return <a key={i} className="d-link" href={part.href} target="_blank" rel="noopener noreferrer nofollow" dir="ltr" tabIndex={tabIndex}>{part.href}</a>;
      case 'mention': {
        const inner = <>{part.color ? <RoleDot color={part.color} inline /> : null}{part.label}</>;
        const attrs = { className: 'd-mention', 'data-me': flag(part.me), 'data-broadcast': flag(part.broadcast) };
        if (part.userId === undefined || onOpenProfile === undefined) return <span key={i} {...attrs}>{inner}</span>;
        const userId = part.userId;
        return <button key={i} type="button" {...attrs} tabIndex={tabIndex} onClick={e => onOpenProfile(userId, e.currentTarget, 'mention')}>{inner}</button>;
      }
    }
  });
}

/**
 * The body: one pre-wrapped run of text, or, when it holds a fenced block, its text runs as paragraphs with each block as a
 * card between them (the prototype's 6px grid).
 */
function Body({ body, tabIndex, onOpenProfile, codeLabels }: { body: string | readonly BodyPart[]; tabIndex: number;
  onOpenProfile: MessageRowProps['onOpenProfile']; codeLabels: CodeBlockLabels | undefined }) {
  if (typeof body === 'string' || !body.some(p => p.kind === 'codeblock')) {
    return <div className="d-message-row__body">{bodyChildren(body, tabIndex, onOpenProfile)}</div>;
  }
  const blocks: ReactNode[] = [];
  let run: BodyPart[] = [];
  const flush = () => {
    if (run.length > 0) blocks.push(<p key={blocks.length} className="d-message-row__text">{bodyChildren(run, tabIndex, onOpenProfile)}</p>);
    run = [];
  };
  for (const part of body) {
    if (part.kind === 'codeblock') {
      flush();
      blocks.push(<CodeBlock key={blocks.length} lang={part.lang} code={part.text} labels={codeLabels} tabIndex={tabIndex} />);
    } else run.push(part);
  }
  flush();
  return <div className="d-message-row__body" data-code="true">{blocks}</div>;
}

function ReplyLine({ reply, tabIndex }: { reply: NonNullable<MessageRowProps['reply']>; tabIndex: number }) {
  const labelId = useId();
  const jumpId = useId();
  // The prototype's 2px connector curve from the avatar column and the 14px mini avatar.
  const glyph = <><span className="d-message-row__reply-curve" aria-hidden="true" />{reply.author !== '' ? <span className="d-message-row__reply-avatar" aria-hidden="true"><Avatar name={reply.author} size="mini" /></span> : null}</>;
  const author = <span className="d-message-row__reply-author">{reply.author}</span>;
  const excerpt = <span className="d-message-row__reply-excerpt">{reply.excerpt}</span>;
  const stateText = <span className="d-message-row__reply-state">{reply.stateText}</span>;
  const canJump = reply.onJump !== undefined && (reply.state === 'ok' || reply.state === 'deleted');
  let line: ReactNode;
  if (canJump) {
    // Named by its visible text (WCAG 2.5.3), described by the label and the jump hint: no aria-label.
    line = (
      <button type="button" className="d-message-row__reply-jump" tabIndex={tabIndex} aria-describedby={`${labelId} ${jumpId}`}
        onPointerDown={reply.onJumpPointerDown} onClick={() => reply.onJump?.()}>
        {glyph}{author}{' '}{reply.state === 'ok' ? excerpt : stateText}
      </button>
    );
  } else if (reply.state === 'ok') {
    line = <>{glyph}{author}{' '}{excerpt}</>;
  } else {
    line = <>{glyph}{stateText}</>;
  }
  return (
    <div className="d-message-row__reply" data-state={reply.state}>
      <span className="d-sr-only" id={labelId}>{reply.label}</span>
      {canJump ? <span className="d-sr-only" id={jumpId}>{reply.jumpLabel}</span> : null}
      {line}
    </div>
  );
}

/**
 * One message, as an <article> named by its head. The body is shown for ok, pending and failed rows only: an
 * unreadable or deleted row shows its state label in the body's place, and never its body. The body is text
 * children and mention pills, never HTML. The row is a tab stop only while `active` (the log's roving row), and
 * every button it renders itself follows it; the slots (toolbar, attachments, reactions, editor) are the
 * caller's, who passes them `tabbable`. Escape inside the row brings focus back to the row; every other key,
 * and Escape on the row itself, goes to `onKeyDown`.
 */
export function MessageRow({
  author, authorId, onOpenProfile, time, gutterTime, body, own, tag, state, stateLabel, detail, actions, active, editedLabel, pinnedLabel, reply, attachments, card,
  reactions, thread, toolbar, editor, pendingAction, flash, mention, onKeyDown, rowRef, seq, msgId, grouped, revealed, onReveal,
  nameColor, codeLabels, replying, forceHover,
}: MessageRowProps) {
  const headId = useId();
  const tabIndex = active === true ? 0 : -1;
  // The author name and the pills come after the row in the tab order but are stops only while focus is in the row, so a Shift+Tab from after the log lands on the row itself.
  const [inside, setInside] = useState(false);
  const profileTab = active === true && inside ? 0 : -1;
  const showBody = state === 'ok' || state === 'pending' || state === 'failed';
  const headState = (state === 'pending' || state === 'failed') && !!stateLabel;
  const note = (state === 'cannot-read' || state === 'deleted') && !!stateLabel;
  const showDetail = (state === 'cannot-read' || state === 'failed') && !!detail;

  const authorEl = useRef<HTMLButtonElement>(null);
  const opens = authorId !== undefined && onOpenProfile !== undefined;
  const phone = usePhoneLayout();
  const reveals = phone && onReveal !== undefined;

  const onRowKeyDown = (e: KeyboardEvent<HTMLElement>) => {
    if (reveals && e.key === 'Escape' && revealed === true) {
      e.preventDefault();
      e.currentTarget.focus();
      onReveal(false);
      return;
    }
    if (reveals && e.key === 'Enter' && e.target === e.currentTarget && !e.repeat) {
      e.preventDefault();
      onReveal(true);
      return;
    }
    if (e.key === 'Escape' && e.target !== e.currentTarget) {
      e.preventDefault();
      e.currentTarget.focus();
      return;
    }
    onKeyDown?.(e);
  };

  const onRowClick = (e: MouseEvent<HTMLElement>) => {
    if (!reveals) return;
    if ((e.target as Element).closest(INTERACTIVE)) return;
    onReveal(revealed !== true);
  };
  const showToolbar = toolbar !== undefined && (!reveals || revealed === true);

  // The colour rides on a custom property set on the head, so a context that draws names in --fg (the thread, as the prototype's) can drop it.
  const nameStyle = nameColor ? { '--d-name-colour': '#' + nameColor.toString(16).padStart(6, '0') } as React.CSSProperties : undefined;
  const gutter = gutterTime ?? time;

  return (
    <article ref={rowRef} className="d-message-row" tabIndex={tabIndex} aria-labelledby={headId} onKeyDown={onRowKeyDown} onClick={reveals ? onRowClick : undefined}
      onFocus={() => setInside(true)} onBlur={e => { if (!e.currentTarget.contains(e.relatedTarget)) setInside(false); }}
      data-state={state} data-own={flag(own)} data-active={flag(active)}
      data-seq={seq ?? undefined} data-msg-id={msgId ?? undefined}
      data-edited={editedLabel !== undefined ? 'true' : undefined} data-pinned={pinnedLabel !== undefined ? 'true' : undefined}
      data-grouped={flag(grouped)} data-revealed={phone ? flag(revealed) : undefined} data-mention={flag(mention)} data-flash={flag(flash)}
      data-reply={flag(reply !== undefined)} data-replying={flag(replying)} data-hover={flag(forceHover)}>
      {/* The time gutter (README §5.5): always on a group's first row, on hover or focus for a follow-up. The head says it for a screen reader. */}
      <span className="d-message-row__gutter" aria-hidden="true" title={gutterTime === undefined ? undefined : time} data-long={gutter.length > 5 ? 'true' : undefined}>{gutter}</span>
      {grouped === true
        ? <span className="d-message-row__avatar" aria-hidden="true" />
        : <span className="d-message-row__avatar" aria-hidden="true" data-profile={opens ? 'true' : undefined}
          onClick={opens ? e => onOpenProfile(authorId, authorEl.current ?? e.currentTarget, 'author') : undefined}><Avatar name={author} size="msg" shape={tag === 'bot' ? 'bot' : 'circle'} /></span>}
      <div className="d-message-row__main">
        <p className="d-message-row__head" id={headId} style={nameStyle}>
          <span className={grouped === true ? 'd-message-row__author d-sr-only' : 'd-message-row__author'} data-bot={flag(tag === 'bot')}>
            {opens && grouped !== true
              ? <button ref={authorEl} type="button" className="d-message-row__author-open" tabIndex={profileTab} onClick={e => onOpenProfile(authorId, e.currentTarget, 'author')}>{author}</button>
              : author}
          </span>
          {tag && grouped !== true ? <Tag kind={tag} /> : null}
          {/* Desktop: the time stands in the gutter, so the head's copy is for a screen reader; a phone shows it beside the name. */}
          <span className={phone && grouped !== true ? 'd-message-row__time' : 'd-message-row__time d-sr-only'}>{time}</span>
          {editedLabel !== undefined ? <span className="d-message-row__edited">{editedLabel}</span> : null}
          {pinnedLabel !== undefined ? <span className="d-message-row__pinned">{pinnedLabel}</span> : null}
          {headState ? <span className="d-message-row__state">{stateLabel}</span> : null}
        </p>
        {/* The keyboard map: the head (author), the toolbar (it floats over the row's top edge), the reply line, which CSS
            draws above the head as the prototype does (§6.5). */}
        {showToolbar ? <div className="d-message-row__toolbar">{toolbar}</div> : null}
        {reply !== undefined ? <ReplyLine reply={reply} tabIndex={tabIndex} /> : null}
        {editor !== undefined ? editor : showBody ? <Body body={body} tabIndex={profileTab} onOpenProfile={onOpenProfile} codeLabels={codeLabels} /> : null}
        {card !== undefined ? <div className="d-message-row__attachments d-message-row__card">{card}</div> : null}
        {attachments !== undefined ? <div className="d-message-row__attachments">{attachments}</div> : null}
        {reactions !== undefined ? <div className="d-message-row__reactions">{reactions}</div> : null}
        {thread !== undefined ? (
          <button type="button" className="d-message-row__thread" tabIndex={tabIndex}
            onPointerDown={e => e.preventDefault()} onClick={() => thread.onOpen()}>
            <Icon name="messages-square" size={12} />{thread.label}
            {thread.last !== undefined ? <span className="d-message-row__thread-last" aria-hidden="true">· {thread.last}</span> : null}
          </button>
        ) : null}
        {pendingAction !== undefined ? (
          <p className="d-message-row__pending" data-tone={pendingAction.tone}>
            <span className="d-message-row__pending-label">{pendingAction.label}</span>
            {pendingAction.actions?.map(a => (
              <Button key={a.label} variant="ghost" tabIndex={tabIndex} onClick={a.onAction}>{a.label}</Button>
            ))}
          </p>
        ) : null}
        {note ? <p className="d-message-row__note">{stateLabel}</p> : null}
        {showDetail ? <code className="d-message-row__detail">{detail}</code> : null}
        {actions && actions.length > 0 ? (
          <div className="d-message-row__actions">
            {actions.map(a => <Button key={a.label} variant="ghost" tabIndex={tabIndex} onClick={a.onAction}>{a.label}</Button>)}
          </div>
        ) : null}
      </div>
    </article>
  );
}
