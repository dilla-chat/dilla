import { useEffect, useId, useRef, type KeyboardEvent, type ReactNode } from 'react';
import { CallTile, type CallTileProps } from '../CallTile/CallTile.tsx';
import { Avatar } from '../Avatar/Avatar.tsx';
import '../Button/Button.css';
import './CallStage.css';
import { Icon } from '../Icon/Icon.tsx';

export interface CallStageNotice {
  /**
   * `paused` is the RV-3 pause banner and `blocked` a block notice (`voice.blocked`, `voice.blockedJoin`,
   * `voice.blockedChanged`): while either shows, no tile renders a media element. `info` is a transient notice
   * (`voice.full`, `voice.micDenied` and the like) that leaves the tiles as they are.
   */
  kind: 'paused' | 'blocked' | 'info';
  text: string;
  action: { label: string; onAction(): void } | null;
  /** Move focus to the notice (its action, or the notice itself) once, when this text first shows. */
  focusOnce: boolean;
}

/** The not-joined preview (Signal §5.6): who is inside (with a ring on whoever speaks), the room and Join voice. */
export interface CallStagePreview {
  people: readonly { id: string; name: string; speaking: boolean; bot?: boolean }[];
  /** The room's name, 16px 700. */
  name: string;
  /** "Ada and Björn are here", or that nobody is. */
  text: string;
  /** The green Join voice; null while a block holds joining back. */
  join: { label: string; onJoin(): void } | null;
}

export interface CallStageProps {
  /** The stage's heading, e.g. 'voice harbour' or the DM's name. */
  title: string;
  /** The tiles, in one stable order, keyed by `id` (R27). `focused`, `onFocusTile` and `onGrid` are injected here, never passed. */
  tiles: readonly Omit<CallTileProps, 'focused' | 'onFocusTile' | 'onGrid' | 'gridLabel'>[];
  /** The focused tile's id, or null for the grid. An id that names no current tile renders the grid. Additive: without
   *  `onFocus` the tiles have no focus button (the DM call box's compact cards). */
  focusedId?: string | null;
  /** Focus a tile (`id`) or clear the focus (`null`): a click on a tile, the Grid button, Escape, or a pause notice. */
  onFocus?(id: string | null): void;
  /**
   * The stage's 44 bar (C4): where the voice `ChannelHeader` goes. When given, it comes first, full width, and the stage's own
   * title is only the section's visually hidden name.
   */
  header?: ReactNode;
  /** Task 8b's slot: the now-playing bar at the top of the stage, above the tiles. */
  nowPlaying?: ReactNode;
  /** The call code, 30 digits (spaces are ignored), or null when there is none yet. A DM call box shows it in its head; a
   *  channel's stage (with `bar`) does not: fix round 1 (Jonas) moved it to the voice row's context menu. */
  code: string | null;
  /** A notice on the stage: the RV-3 pause banner (with its `voice.pausedAction`), a block notice or a transient one. */
  notice: CallStageNotice | null;
  /** The call controls (a `CallControls`). With `bar` they sit at the right of the 64px call bar. */
  controls: ReactNode;
  /**
   * The call bar's left side (Signal §5.6): the room with its --ok dot, and the stats line under it ("24 ms · ↑ 38 ↓ 71 kb/s ·
   * hearing 2 of 2"). Given, the controls region is the 64px call bar; absent (a DM call box, the AFK page), it is plain.
   */
  bar?: { name: string; stats: ReactNode };
  /** Not in the call: the preview stands where the tiles and the call bar would. */
  preview?: CallStagePreview;
  /** The DM call strip while this device's call rings others ('ringing {names}', '{name} declined.', 'No answer.'). */
  outgoing: { text: string } | null;
  /** `grid` is the focused tile's Grid button (the prototype's "Grid"). */
  labels: { code: string; codeHelp: string; empty: string; tiles: string; grid?: string };
}

const NOTICE_GLYPH = { paused: <Icon name="pause" size={14} />, blocked: '!', info: '›' } as const;

/** Six groups of five digits; anything that is not 30 digits is shown as given. */
function groupCode(code: string): string {
  const digits = code.replace(/\s+/g, '');
  return /^\d{30}$/.test(digits) ? (digits.match(/\d{5}/g) ?? []).join(' ') : code;
}

/** The prototype's tile width by count: ≤2 min(46%, 560px), ≤4 min(46%, 420px), more min(31.5%, 340px). */
function band(n: number): 'two' | 'four' | 'many' {
  return n <= 2 ? 'two' : n <= 4 ? 'four' : 'many';
}

/**
 * The call stage: the bar (the voice header), the heading, the outgoing ring, a notice region, then the stage body (the
 * now-playing slot, the tiles as a list named `voice.tiles`, or `voice.empty`, or the not-joined preview), then the controls:
 * with `bar` the 64px call bar (the room, the stats and the controls). The notice region is a
 * `role="status"` that is always present, so its text is announced; a notice with `focusOnce` takes focus once per text.
 * While a pause or block notice shows, no tile renders media, no screen tile renders and the focus is cleared; while any
 * notice shows, the stage does not also say that nobody is in voice.
 *
 * The tiles are one keyed list that never changes order or parent: focus only sets `data-focused-stage` on the stage and
 * `data-focused` on the tile, and CSS does the rest (R27): a wrapping row of 16:9 tiles in the grid, the focused tile over
 * a filmstrip when focused. Escape unfocuses a tile, unless full screen is on.
 */
export function CallStage({ title, tiles: allTiles, focusedId = null, onFocus, header, nowPlaying, code, notice, controls, bar, preview,
  outgoing, labels }: CallStageProps) {
  const titleId = useId();
  const detailsRef = useRef<HTMLDetailsElement>(null);
  const noticeRef = useRef<HTMLDivElement>(null);
  const actionRef = useRef<HTMLButtonElement>(null);
  const focusedFor = useRef<string | null>(null);
  const noticeText = notice?.text ?? null;
  const focusOnce = notice?.focusOnce ?? false;

  useEffect(() => {
    if (noticeText === null) { focusedFor.current = null; return; }
    if (!focusOnce || focusedFor.current === noticeText) return;
    focusedFor.current = noticeText;
    (actionRef.current ?? noticeRef.current)?.focus();
  }, [noticeText, focusOnce]);

  const onCodeKey = (e: KeyboardEvent<HTMLDetailsElement>) => {
    const details = detailsRef.current;
    if (e.key !== 'Escape' || !details?.open) return;
    e.preventDefault();
    e.stopPropagation();
    details.open = false;
    details.querySelector('summary')?.focus();
  };

  const hideMedia = notice !== null && notice.kind !== 'info';
  const tiles = hideMedia ? allTiles.filter(t => t.source !== 'screen') : allTiles;
  const idOf = (t: (typeof allTiles)[number]) => t.id ?? t.view.deviceId;
  const stageMode = onFocus !== undefined;
  const focusedHere = !hideMedia && focusedId !== null && tiles.some(t => idOf(t) === focusedId);
  // A lone focused tile keeps the grid: a filmstrip row of nothing would only shrink it.
  const focusedLayout = focusedHere && tiles.length > 1;

  useEffect(() => {
    if (hideMedia && focusedId !== null) onFocus?.(null);
  }, [hideMedia, focusedId, onFocus]);

  const onStageKey = (e: KeyboardEvent<HTMLElement>) => {
    if (e.key !== 'Escape' || !focusedHere || document.fullscreenElement) return;
    e.preventDefault();
    e.stopPropagation();
    onFocus?.(null);
  };

  const hasHeader = header !== undefined && header !== null;
  const codeBox = code !== null ? (
    <details ref={detailsRef} className="d-call-stage__code" onKeyDown={onCodeKey}>
      <summary className="d-call-stage__summary">{labels.code}</summary>
      <div className="d-call-stage__code-body">
        <p className="d-call-stage__digits">{groupCode(code)}</p>
        <p className="d-call-stage__help">{labels.codeHelp}</p>
      </div>
    </details>
  ) : null;
  let body: ReactNode;
  if (preview !== undefined) {
    body = (
      <div className="d-call-stage__preview">
        {preview.people.length > 0 ? (
          <span className="d-call-stage__faces" aria-hidden="true">
            {preview.people.map(p => (
              <span key={p.id} className="d-call-stage__face" data-speaking={p.speaking ? 'true' : 'false'}>
                <Avatar name={p.name} size="lg" {...(p.bot === true ? { shape: 'bot' as const } : {})} />
              </span>
            ))}
          </span>
        ) : null}
        <span className="d-call-stage__preview-text">
          <span className="d-call-stage__preview-name">{preview.name}</span>
          <span className="d-call-stage__preview-who">{preview.text}</span>
        </span>
        {preview.join !== null ? (
          <button type="button" className="d-call-stage__join" onClick={preview.join.onJoin}>
            <Icon name="phone" size={15} />{preview.join.label}
          </button>
        ) : null}
      </div>
    );
  } else if (tiles.length === 0) {
    body = notice === null ? <p className="d-call-stage__empty">{labels.empty}</p> : null;
  } else {
    body = (
      <ul role="list" aria-label={labels.tiles} className="d-call-stage__tiles" data-tiles={stageMode ? 'stage' : undefined}
        data-band={band(tiles.length)}
        style={focusedLayout ? { '--d-strip-n': Math.max(1, tiles.length - 1) } as React.CSSProperties : undefined}>
        {tiles.map(t => {
          const id = idOf(t);
          const focused = focusedHere && id === focusedId;
          return (
            <CallTile key={id} {...t} media={hideMedia ? null : t.media} focused={focused}
              onFocusTile={onFocus ? () => { onFocus(focused ? null : id); } : undefined}
              {...(focused && onFocus && labels.grid !== undefined ? { onGrid: () => { onFocus(null); }, gridLabel: labels.grid } : {})} />
          );
        })}
      </ul>
    );
  }
  return (
    <section className="d-call-stage" aria-labelledby={titleId} data-media-hidden={hideMedia ? 'true' : undefined}
      data-focused-stage={focusedLayout ? 'true' : undefined} data-preview={preview !== undefined ? 'true' : undefined}
      data-bar={bar !== undefined && preview === undefined ? 'true' : undefined} onKeyDown={onStageKey}>
      {hasHeader ? <div className="d-call-stage__bar">{header}</div> : null}
      <header className="d-call-stage__head" data-bare={hasHeader || bar !== undefined || preview !== undefined ? 'true' : undefined}
        data-empty={outgoing === null && (code === null || bar !== undefined) ? 'true' : undefined}>
        {hasHeader || bar !== undefined || preview !== undefined
          ? <span id={titleId} className="d-sr-only">{title}</span>
          : <h2 id={titleId} className="d-call-stage__title">{title}</h2>}
        {outgoing !== null ? <p className="d-call-stage__outgoing">{outgoing.text}</p> : null}
        {bar === undefined ? codeBox : null}
      </header>
      <div ref={noticeRef} role="status" className="d-call-stage__notice" data-empty={notice === null ? 'true' : undefined} data-kind={notice?.kind}
        tabIndex={notice !== null && notice.action === null ? -1 : undefined}>
        {notice !== null ? (
          <>
            <span className="d-call-stage__notice-glyph" aria-hidden="true">{NOTICE_GLYPH[notice.kind]}</span>
            <p className="d-call-stage__notice-text">{notice.text}</p>
            {notice.action !== null ? (
              <button ref={actionRef} type="button" className="d-btn d-call-stage__notice-action" data-variant="default" data-size="md"
                onClick={notice.action.onAction}>{notice.action.label}</button>
            ) : null}
          </>
        ) : null}
      </div>
      <div className="d-call-stage__body">
        {nowPlaying ?? null}
        {body}
      </div>
      {preview !== undefined ? null : bar !== undefined ? (
        <div className="d-call-stage__controls" data-bar="true">
          <div className="d-call-stage__where">
            <span className="d-call-stage__room"><span className="d-call-stage__dot" aria-hidden="true" />{bar.name}</span>
            <span className="d-call-stage__stats">{bar.stats}</span>
          </div>
          {controls}
        </div>
      ) : <div className="d-call-stage__controls">{controls}</div>}
    </section>
  );
}
