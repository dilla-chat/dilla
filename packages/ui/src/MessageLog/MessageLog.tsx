import { Children, useLayoutEffect, useRef, type KeyboardEvent, type ReactNode } from 'react';
import { Button } from '../Button/Button.tsx';
import { Icon, type IconName } from '../Icon/Icon.tsx';
import './MessageLog.css';

export interface MessageLogProps {
  label: string;
  children: ReactNode;
  earlier?: { label: string; onLoad(): void };
  emptyLabel: string;
  busy?: boolean;
  onKeyDown?(e: KeyboardEvent): void;
  /** Set when the log can be away from the newest messages: `atPresent === false` shows the bar, and Ctrl+End jumps. */
  present?: { atPresent: boolean; olderLabel: string; jumpLabel: string; onJumpToPresent(): void };
  /**
   * The start of the conversation (README §5.5): a 40px tile with the channel's glyph, "Welcome to #general" and "This is
   * the start of #general.", above the first row. Pass it once the history is complete (no `earlier`); `emptyLabel` follows it
   * while there is no row.
   */
  welcome?: { icon: IconName; title: string; body: string };
}

/** The entrance stagger counts the last this many rows of the first paint (top to bottom); rows above them are off screen. */
export const STAGGER_ROWS = 20;

/** A day divider (README §5.5): "TODAY" in 10px/600 uppercase, then a hairline to the edge. Pass it as a child of the log. */
export function DateDivider({ label }: { label: string }) {
  return (
    <div className="d-date-divider" role="separator" aria-label={label}>
      <span className="d-date-divider__label" aria-hidden="true">{label}</span>
    </div>
  );
}

/** How far from the end, in CSS px, the reader may be and still count as reading the end. */
export const PIN_SLACK_PX = 24;

/**
 * The message feed: a polite, named log that is a keyboard-reachable scroll
 * region. It stays pinned to the end while the reader is there, keeps the
 * reader's place when rows arrive or earlier rows are put in front, and never
 * moves focus. `busy` is held by the caller while history is loading.
 *
 * The keyboard map (web-2b): while a row inside holds `tabindex="0"` (the roving active row) that row is the
 * log's one tab stop and the log itself leaves the tab order (tabIndex -1, still focusable by script); with no
 * such row the log stays a reachable scroll region. Keys reach the caller through `onKeyDown`.
 */
export function MessageLog({ label, children, earlier, emptyLabel, busy, onKeyDown, present, welcome }: MessageLogProps) {
  // Scrolling must not re-render: all of the pinning state lives in refs.
  const logRef = useRef<HTMLDivElement>(null);
  const rowsRef = useRef<HTMLDivElement>(null);
  const pinned = useRef(true);
  const prevHeight = useRef(0);
  const prevFirst = useRef<Element | null>(null);
  // The position the log last pinned itself to. A scroll event that still reports it is the log's own move, not the
  // reader's: when a row grew between the pin and that (asynchronous) event, the event sees the log short of the end,
  // and taking that as the reader scrolling up left the log stuck there (2026-10-30 CI, an image card that grew when
  // its thumbnail decoded).
  const pinnedTop = useRef<number | null>(null);
  const pin = (el: HTMLElement) => {
    el.scrollTop = el.scrollHeight;
    pinnedTop.current = el.scrollTop;
  };

  const onScroll = () => {
    const el = logRef.current;
    if (!el) return;
    const atEnd = el.scrollHeight - el.scrollTop - el.clientHeight <= PIN_SLACK_PX;
    if (el.scrollTop === pinnedTop.current) {
      pinned.current = pinned.current || atEnd;
    } else {
      pinnedTop.current = null;
      pinned.current = atEnd;
    }
  };

  // The first paint's rows rise in one after another, 40ms apart (README §5.5); rows that arrive later have no delay.
  useLayoutEffect(() => {
    const rows = Array.from(rowsRef.current?.querySelectorAll<HTMLElement>(':scope > .d-message-row') ?? []).slice(-STAGGER_ROWS);
    rows.forEach((row, i) => row.style.setProperty('--d-enter-i', String(i)));
  }, []);

  // A resize of the log (the shell shrinking under a soft keyboard), or of its rows without a render of the log (an
  // image that grows when it decodes, a card's own state), keeps the end in view for a reader who is at the end.
  useLayoutEffect(() => {
    const el = logRef.current;
    if (!el || typeof ResizeObserver === 'undefined') return;
    const observer = new ResizeObserver(() => {
      if (pinned.current) pin(el);
    });
    observer.observe(el);
    if (rowsRef.current) observer.observe(rowsRef.current);
    return () => observer.disconnect();
  }, []);

  // No dependency list: runs after every render.
  useLayoutEffect(() => {
    const el = logRef.current;
    if (!el) return;
    const first = rowsRef.current?.firstElementChild ?? null;
    if (pinned.current) {
      // Only when the content changed height: a render that adds nothing (a row taking focus on a press) must not move
      // the log under the pointer, or the release lands beside the button it pressed and no click is sent (2026-10-30 CI,
      // Firefox: "save data.bin" pressed 20 px short of the end, the log jumped 20 px, no download).
      if (el.scrollHeight !== prevHeight.current) pin(el);
    } else if (prevFirst.current !== null && first !== prevFirst.current) {
      // Earlier rows were put in front: move by the added height so the rows in view stay put.
      el.scrollTop = el.scrollTop + (el.scrollHeight - prevHeight.current);
    }
    prevHeight.current = el.scrollHeight;
    prevFirst.current = first;
    // One tab stop: a row that holds the stop takes it from the log.
    el.tabIndex = rowsRef.current?.querySelector('[tabindex="0"]') ? -1 : 0;
  });

  const onLogKeyDown = (e: KeyboardEvent) => {
    if (present && !present.atPresent && e.key === 'End' && e.ctrlKey && !e.altKey && !e.metaKey && !e.shiftKey) {
      e.preventDefault();
      present.onJumpToPresent();
      return;
    }
    onKeyDown?.(e);
  };

  const showBar = present !== undefined && !present.atPresent;
  const log = (
    <div ref={logRef} className="d-message-log" role="log" aria-live="polite" aria-relevant="additions" aria-label={label}
      aria-busy={busy ? true : undefined} tabIndex={0} onScroll={onScroll} onKeyDown={onLogKeyDown}>
      {earlier ? <Button className="d-message-log__earlier" variant="ghost" onClick={earlier.onLoad}>{earlier.label}</Button> : null}
      {welcome !== undefined ? (
        <div className="d-message-log__welcome">
          <span className="d-message-log__welcome-tile" aria-hidden="true"><Icon name={welcome.icon} size={20} /></span>
          <p className="d-message-log__welcome-title">{welcome.title}</p>
          <p className="d-message-log__welcome-body">{welcome.body}</p>
        </div>
      ) : null}
      {Children.count(children) === 0 ? <p className="d-message-log__empty">{emptyLabel}</p> : null}
      <div ref={rowsRef} className="d-message-log__rows">{children}</div>
    </div>
  );
  if (present === undefined) return log;
  // The bar sits beside the live region, not in it, so it is not read out as a log entry; its appearance is announced once
  // by the polite status, which holds the older-messages text only while the bar shows.
  return (
    <div className="d-message-log-frame">
      {log}
      {showBar ? (
        <div className="d-message-log__present">
          <span>{present.olderLabel}</span>
          <span aria-hidden="true">·</span>
          <Button variant="ghost" onClick={present.onJumpToPresent}>{present.jumpLabel}</Button>
        </div>
      ) : null}
      <p className="d-sr-only" role="status">{showBar ? present.olderLabel : ''}</p>
    </div>
  );
}
