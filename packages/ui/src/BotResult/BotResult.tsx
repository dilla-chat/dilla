import { useEffect, useRef, type ReactNode } from 'react';
import { Avatar } from '../Avatar/Avatar.tsx';
import { Icon } from '../Icon/Icon.tsx';
import '../MessageRow/MessageRow.css';
import '../Tag/Tag.css';
import './BotResult.css';
export type BotStatus = 'pending' | 'success' | 'failed' | 'cancelled';
export interface BotResultProps {
  author: { name: string; avatarName: string; shape: 'bot' | 'circle' }; time: string;
  status: BotStatus; body: ReactNode; onDismiss(): void;
  /** Epoch ms at which the row dismisses itself. The timer pauses while the row is hovered or has focus within, and resumes with the time left. Omit for a row that stays. */
  autoDismissAt?: number | undefined;
  labels: { status: Record<BotStatus, string>; onlyYou: string; dismiss: string; dismissName: string; tag: string };
}
export function BotResult({ author, time, status, body, onDismiss, autoDismissAt, labels }: BotResultProps) {
  const icon = status === 'pending' ? 'loader' : status === 'success' ? 'check' : status === 'failed' ? 'x' : 'triangle-alert';
  const rowRef = useRef<HTMLElement>(null);
  const dismiss = (moveFocus = true) => {
    const row = rowRef.current;
    const rows = Array.from(row?.parentElement?.querySelectorAll<HTMLElement>('.d-bot-result') ?? []);
    const next = rows[rows.indexOf(row!) + 1]?.querySelector<HTMLButtonElement>('.d-bot-result__dismiss');
    let scope = row?.parentElement ?? null;
    const composerSelector = '.d-composer textarea, .d-composer input:not([type="file"])';
    while (scope && !scope.querySelector(composerSelector)) scope = scope.parentElement;
    const composer = scope?.querySelector<HTMLElement>(composerSelector);
    onDismiss();
    if (moveFocus) (next ?? composer)?.focus();
  };
  const dismissRef = useRef(dismiss);
  dismissRef.current = dismiss;
  const pause = useRef({ hover: false, focus: false });
  const timer = useRef<{ id: ReturnType<typeof setTimeout> | undefined; left: number; since: number }>({ id: undefined, left: 0, since: 0 });
  const arm = () => {
    const t = timer.current;
    if (t.id !== undefined || pause.current.hover || pause.current.focus) return;
    t.since = Date.now();
    t.id = setTimeout(() => {
      t.id = undefined;
      // No announcement. Focus moves only if it is inside the row (a timer that fires just as focus arrives).
      dismissRef.current(rowRef.current?.contains(document.activeElement) === true);
    }, t.left);
  };
  const hold = () => {
    const t = timer.current;
    if (t.id === undefined) return;
    clearTimeout(t.id); t.id = undefined; t.left = Math.max(0, t.left - (Date.now() - t.since));
  };
  useEffect(() => {
    const t = timer.current;
    hold();
    if (autoDismissAt === undefined) return;
    t.left = Math.max(0, autoDismissAt - Date.now());
    arm();
    return hold;
  // eslint-disable-next-line react-hooks/exhaustive-deps -- arm/hold only touch refs
  }, [autoDismissAt]);
  const set = (key: 'hover' | 'focus', on: boolean) => { pause.current[key] = on; if (on) hold(); else arm(); };
  // Suppress the surrounding log's live announcements; only the chip announces state changes.
  return <article ref={rowRef} className="d-message-row d-bot-result" data-status={status} aria-live="off"
    onPointerEnter={() => set('hover', true)} onPointerLeave={() => set('hover', false)}
    onFocus={() => set('focus', true)} onBlur={e => { if (!e.currentTarget.contains(e.relatedTarget as Node | null)) set('focus', false); }}>
    <span className="d-message-row__gutter" aria-hidden="true">{time}</span>
    <span className="d-message-row__avatar" data-builtin={author.shape === 'circle' ? 'true' : undefined}>
      {author.shape === 'bot' ? <Avatar name={author.avatarName} size="msg" shape="bot" /> : <span className="d-bot-result__tile" role="img" aria-label={author.avatarName}>D</span>}
    </span>
    <div className="d-message-row__main">
      <p className="d-message-row__head"><span className="d-message-row__author" data-bot="true">{author.name}</span><span className="d-tag" data-kind="bot">{labels.tag}</span><span className="d-bot-result__time">{time}</span></p>
      <div className="d-message-row__body">{body}</div>
      <div className="d-bot-result__meta"><span className="d-bot-result__status" role="status" aria-live="polite" aria-atomic="true">{icon ? <span className="d-bot-result__icon"><Icon name={icon} size={11} stroke={2.5} /></span> : null}{labels.status[status]}</span>
        <span className="d-bot-result__privacy"><Icon name="eye" size={11} />{labels.onlyYou}</span>
        <button type="button" className="d-bot-result__dismiss" aria-label={labels.dismissName} onClick={() => dismiss()}>{labels.dismiss}</button>
      </div>
    </div>
  </article>;
}
