import { useEffect, useId, useLayoutEffect, useRef, useState, type CSSProperties, type MouseEvent, type ReactNode, type RefObject } from 'react';
import { Button } from '../Button/Button.tsx';
import { PhoneHeader } from '../PhoneHeader/PhoneHeader.tsx';
import { PhoneLayoutProvider } from '../internal/phoneLayout.ts';
import { Splitter, type Pane } from './Splitter.tsx';
import './AppShell.css';

export interface AppShellProps {
  skipLabel: string;
  rail: ReactNode;
  sidebar: ReactNode;
  header: ReactNode;
  children: ReactNode;
  composer: ReactNode;
  /**
   * Signal (README §5.1–5.2): the 44px top bar over the whole desktop shell (rail, sidebar, main and aside), first in the focus
   * order after the skip link. Absent (undefined, null or false) there is no wrapper and the row is empty. A phone never has one
   * (§5.10): it is ignored there.
   */
  topBar?: ReactNode;
  /**
   * web3.8 (R23): desktop only, ignored on a phone. The bottom-left area, under the sidebar beside the full-height rail (Signal §5.1; the voice dock
   * over the user panel). Absent (undefined, null or false) the shell renders no wrapper and the row is empty. In DOM
   * order it follows the sidebar and its splitter and precedes main: rail, sidebar, splitter, bottom, main.
   */
  bottom?: ReactNode;
  banner?: ReactNode;
  /**
   * The incoming-ring banner (web-3, flow 06): the first thing in main, so its answer and decline are main's first
   * controls. On a phone it is fixed to the top of the shell and the grid moves down by its measured height, so it never
   * covers the stage controls.
   */
  ring?: ReactNode;
  /**
   * web3.4: a pane after main (the member list). Rendered only while `asideOpen`. From 64rem of shell width it is a
   * fourth grid column. Below that it is a modal overlay drawer from the right: the rest of the shell is inert behind a
   * backdrop (a click on it closes), focus moves in on open, Escape anywhere calls `onAsideClose` (unless something in
   * the pane handled it), and focus goes back to the opener.
   */
  aside?: ReactNode;
  asideLabel?: string;
  asideOpen?: boolean;
  onAsideClose?(): void;
  /** The drawer's close button (final fix I2): shown at its top only as the overlay, where it calls `onAsideClose`. */
  asideCloseLabel?: string;
  /** The control that opens the aside: focus goes back to it when the drawer closes and the opener was `body`. */
  asideReturnRef?: RefObject<HTMLElement | null>;
  /** What the aside holds, which sets its default column width (README §5.1): 'members' 232px (default) or 'thread' 340px. */
  asideKind?: 'members' | 'thread';
  /**
   * web3.7: 'phone' is the one-pane-at-a-time shell of a phone (flow 12). Absent or 'desktop', the shell renders exactly
   * the desktop DOM and ignores every phone prop below.
   */
  layout?: 'desktop' | 'phone';
  /** The phone pane: 'list' (rail and sidebar, main hidden) or 'view' (main alone). Default 'list'. */
  phonePane?: 'list' | 'view';
  /** The phone's bottom navigation; rendered on both panes of a phone, and hidden while an editable element has focus (data-typing). */
  tabBar?: ReactNode;
  /** The phone's call strip; rendered on both panes of a phone. */
  callBar?: ReactNode;
  /** Phone: told when typing starts or stops (a field has focus and the soft keyboard is up), so the caller can swap chrome for a cue. */
  onTypingChange?(typing: boolean): void;
  /** web3.7.1: a page above the shell on a phone (Settings): main, rail, sidebar, aside and call bar are inert; the tab bar stays live. */
  covered?: boolean;
  /**
   * web3.8: a page above a phone shell (Settings). Rendered after the call bar and before the tab bar, in a wrapper that
   * takes no box (`display: contents`), so the page positions itself. Ignored on desktop. While `covered`, the banner
   * above it stays live and `--d-banner-h` on <html> is its height, which the page starts below.
   */
  page?: ReactNode;
  /**
   * web3.8: desktop only, ignored on a phone. The widths of the channel list and of the aside column in CSS px; a missing
   * one is the default (--sidebar-w). The shell shows them clamped to what keeps main at 24rem and never rewrites them. The package never stores them: the app does.
   */
  paneWidths?: { sidebar?: number; aside?: number };
  /** A committed drag, key or reset. `null` is a reset to the default width. */
  onPaneResize?(pane: Pane, px: number | null): void;
  /** The accessible names of the two splitters. Without them the shell renders no splitter and its DOM is today's. */
  resizeLabels?: { sidebar: string; aside: string };
}

const SIDEBAR_REM = { min: 12, max: 24 };
const ASIDE_REM = { min: 14, max: 28 };
const MAIN_MIN_REM = 24;
/** The widths --sidebar-w gives both panes when nothing was dragged (packages/design-tokens). */
const DEFAULT_PANE_PX = 244;
/** The aside's default column widths: --members-w and --thread-w. */
const DEFAULT_ASIDE_PX = { members: 232, thread: 340 } as const;
const DEFAULT_RAIL_PX = 56;

export interface ClampInput {
  /** The shell's width, the rail's, and the two panes' requested widths, in CSS px. */
  shell: number;
  rail: number;
  sidebar: number;
  aside: number;
  /** True while the aside is a column of the grid (it takes room from main). */
  asideColumn: boolean;
  /** The root font size in px. */
  rem: number;
}

const clampTo = (v: number, min: number, max: number) => Math.min(Math.max(v, min), max);

/** The limits of one pane given the other pane's width: its own rem range, and main staying at 24rem. The minimum wins over the main rule. */
function paneRange(pane: Pane, i: ClampInput, otherPx: number) {
  const own = pane === 'sidebar' ? SIDEBAR_REM : ASIDE_REM;
  const min = own.min * i.rem;
  const taken = i.rail + (pane === 'sidebar' ? (i.asideColumn ? otherPx : 0) : otherPx);
  // An unmeasured shell (width 0: jsdom, or before layout) gives no room to reason about: only the pane's own range holds.
  const room = i.shell > 0 ? i.shell - taken - MAIN_MIN_REM * i.rem : Infinity;
  return { min, max: Math.max(min, Math.min(own.max * i.rem, room)) };
}

/**
 * The widths the two panes may have (web3.8): sidebar in [12rem, 24rem], aside in [14rem, 28rem], and main (the shell less
 * the rail, the sidebar and a column aside) at least 24rem. If even the minimums break that, the minimums win. The sidebar
 * is placed first; the aside takes what is left.
 */
export function clampPanes(i: ClampInput): { sidebar: number; aside: number } {
  const asideFirst = clampTo(i.aside, ASIDE_REM.min * i.rem, ASIDE_REM.max * i.rem);
  const sr = paneRange('sidebar', i, asideFirst);
  const sidebar = clampTo(i.sidebar, sr.min, sr.max);
  const ar = paneRange('aside', i, sidebar);
  return { sidebar, aside: clampTo(i.aside, ar.min, ar.max) };
}

/** The shell width from which the aside is a column (base.css holds the same 64rem in its container query). */
const COLUMN_REM = 64;
const NOT_TEXT = new Set(['checkbox', 'radio', 'range', 'button', 'submit', 'reset', 'file', 'image', 'color']);
/** An element the soft keyboard opens for: a text-like input, a textarea, a select or a contenteditable; not a read-only or disabled one. */
const editable = (el: EventTarget | null) => el instanceof HTMLElement
  && (el instanceof HTMLInputElement ? !NOT_TEXT.has(el.type) && !el.readOnly && !el.disabled
    : el instanceof HTMLTextAreaElement ? !el.readOnly && !el.disabled
      : el instanceof HTMLSelectElement ? !el.disabled : el.isContentEditable || el.getAttribute('contenteditable') === 'true');
/** The least the window must outgrow the visual viewport by for a soft keyboard to be up (browser bars move it by less). */
const KEYBOARD_PX = 100;
const FOCUSABLE = 'button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])';

/**
 * The Signal shell: the top bar over rail | sidebar | main (header, content, composer) | aside, with a banner
 * under the top bar. DOM order is the focus order of the web-1 keyboard map: skip link → top bar → banner → rail →
 * sidebar → main → aside.
 */
export function AppShell({
  skipLabel, rail, sidebar, header, children, composer, topBar, bottom, banner, ring, aside, asideLabel, asideOpen, onAsideClose, asideCloseLabel, asideReturnRef, asideKind = 'members',
  layout, phonePane = 'list', tabBar, callBar, onTypingChange, covered, page, paneWidths, onPaneResize, resizeLabels,
}: AppShellProps) {
  const mainId = useId();
  const mainRef = useRef<HTMLElement>(null);
  const rootRef = useRef<HTMLDivElement>(null);
  const ringRef = useRef<HTMLDivElement>(null);
  const asideRef = useRef<HTMLElement>(null);
  const hasRing = ring !== undefined && ring !== null && ring !== false;
  const hasTopBar = topBar !== undefined && topBar !== null && topBar !== false;
  const hasBottom = bottom !== undefined && bottom !== null && bottom !== false;
  const showAside = asideOpen === true && aside !== undefined && aside !== null && aside !== false;

  // Narrower than 64rem (root font size) of shell width: the aside is the overlay drawer. jsdom has no layout: width 0 reads as narrow.
  const [narrow, setNarrow] = useState(true);
  const narrowRef = useRef(true);
  const railRef = useRef<HTMLDivElement>(null);
  const [remPx, setRemPx] = useState(16);
  const [shellPx, setShellPx] = useState(0);
  const [railPx, setRailPx] = useState(DEFAULT_RAIL_PX);
  const measure = () => {
    const rem = parseFloat(getComputedStyle(document.documentElement).fontSize) || 16;
    const width = rootRef.current?.offsetWidth ?? 0;
    const next = width < COLUMN_REM * rem;
    narrowRef.current = next;
    setNarrow(next);
    setRemPx(rem);
    setShellPx(width);
    setRailPx(railRef.current?.offsetWidth || DEFAULT_RAIL_PX);
    return next;
  };
  // Live: the shell's own size decides, so a resize or a rotation while open switches column and drawer at once.
  // The phone and the desktop layouts render different root elements, so a flip mounts a new root that the previous
  // effect's observer never sees: the effect runs again for the layout, measuring and observing the root it now has.
  const phoneLayout = layout === 'phone';
  useLayoutEffect(() => {
    const root = rootRef.current;
    measure();
    if (root === null || typeof ResizeObserver === 'undefined') return;
    const observer = new ResizeObserver(() => { measure(); });
    observer.observe(root);
    return () => observer.disconnect();
  }, [phoneLayout]);
  const overlay = showAside && narrow;

  // Signal (README §5.1): on a desktop the column aside animates its width 0 <-> 232/340. A closed aside whose content is still
  // given stays mounted at width 0; when the content goes with the close, the last content is kept until the width transition
  // ends. Closed, it is inert and (after the transition) visibility: hidden, so it is neither read nor focusable. The overlay
  // drawer (narrow) and the phone are unchanged: they render only while open.
  const lastAside = useRef<{ node: ReactNode; kind: 'members' | 'thread'; label?: string } | null>(null);
  const hasAsideContent = aside !== undefined && aside !== null && aside !== false;
  if (hasAsideContent) lastAside.current = { node: aside, kind: asideKind, label: asideLabel };
  // `closing` is derived during render (React's "adjust state while rendering"), not in an effect: on the very render where
  // the aside closes, the same <aside> stays in the tree with the last content, so its width transitions to 0.
  const [closing, setClosing] = useState(false);
  const [wasShown, setWasShown] = useState(showAside);
  if (wasShown !== showAside) {
    setWasShown(showAside);
    setClosing(!showAside && !narrow && layout !== 'phone');
  }
  const closedAside = layout !== 'phone' && !narrow && !showAside && (hasAsideContent || (closing && lastAside.current !== null));
  const asideMounted = showAside || closedAside;
  const asideNode = hasAsideContent ? aside : lastAside.current?.node;
  const shownKind = hasAsideContent ? asideKind : lastAside.current?.kind ?? asideKind;
  useEffect(() => {
    if (!closing || showAside) return;
    const el = asideRef.current;
    const done = () => setClosing(false);
    const onEnd = (e: TransitionEvent) => { if (e.target === el && e.propertyName === 'width') done(); };
    el?.addEventListener('transitionend', onEnd);
    // A zero duration (reduced motion) fires no transitionend.
    const fallback = setTimeout(done, 600);
    return () => { el?.removeEventListener('transitionend', onEnd); clearTimeout(fallback); };
  }, [closing, showAside]);

  // web3.8 splitters: desktop only, and only when the app named them.
  const resizable = layout !== 'phone' && resizeLabels !== undefined;
  const columnAside = showAside && !narrow;
  // The limits hold on every render: the width shown is the given one clamped to the shell as it is now (the observer re-renders on a
  // resize), and the given width is never rewritten, so a wider window restores the person's choice.
  const [drag, setDrag] = useState<{ pane: Pane; px: number } | null>(null);
  const dragRef = useRef(drag);
  const widthsIn = {
    shell: shellPx, rail: railPx, rem: remPx, asideColumn: columnAside,
    sidebar: (drag?.pane === 'sidebar' ? drag.px : paneWidths?.sidebar) ?? DEFAULT_PANE_PX,
    aside: (drag?.pane === 'aside' ? drag.px : paneWidths?.aside) ?? DEFAULT_ASIDE_PX[asideKind],
  };
  const widths = clampPanes(widthsIn);
  const ranges = { sidebar: paneRange('sidebar', widthsIn, widths.aside), aside: paneRange('aside', widthsIn, widths.sidebar) };
  // Main under 28rem (the splitters keep it at 24rem or more): the channel header wraps its actions then (ChannelHeader.css).
  const mainPx = shellPx - railPx - widths.sidebar - (columnAside ? widths.aside : 0);
  const mainNarrow = layout !== 'phone' && shellPx > 0 && mainPx < 28 * remPx;
  const given = { sidebar: drag?.pane === 'sidebar' ? drag.px : paneWidths?.sidebar, aside: drag?.pane === 'aside' ? drag.px : paneWidths?.aside };
  const paneVars = resizable && (given.sidebar !== undefined || given.aside !== undefined) ? {
    ...(given.sidebar !== undefined ? { '--d-sidebar-w': `${widths.sidebar}px` } : {}),
    ...(given.aside !== undefined ? { '--d-aside-w': `${widths.aside}px` } : {}),
  } as CSSProperties : undefined;
  const onDrag = (pane: Pane, px: number) => {
    const r = ranges[pane];
    dragRef.current = { pane, px: clampTo(px, r.min, r.max) };
    setDrag(dragRef.current);
  };
  const onDragEnd = () => {
    const d = dragRef.current;
    dragRef.current = null;
    setDrag(null);
    if (d !== null) onPaneResize?.(d.pane, d.px);
  };
  const onSet = (pane: Pane, px: number | null) => {
    if (px === null) { onPaneResize?.(pane, null); return; }
    const r = ranges[pane];
    const next = clampTo(px, r.min, r.max);
    if (next !== widths[pane]) onPaneResize?.(pane, next);
  };
  const splitSidebarId = `${mainId}-sidebar`;
  const asideId = `${mainId}-aside`;
  const splitter = (pane: Pane, controls: string) => resizeLabels === undefined ? null : (
    <Splitter pane={pane} label={resizeLabels[pane]} controls={controls} now={widths[pane]} min={ranges[pane].min} max={ranges[pane].max}
      rem={remPx} onDrag={px => onDrag(pane, px)} onDragEnd={onDragEnd} onSet={px => onSet(pane, px)} inert={pane === 'sidebar' && overlay} />
  );

  // The drawer takes focus on open and hands it back to its opener when it closes; a column does neither.
  // It also runs when an open aside turns into the overlay (a resize), so focus left in now-inert content comes along.
  const showRef = useRef(showAside);
  showRef.current = showAside;
  useEffect(() => {
    if (!overlay || !measure()) return;
    const opener = document.activeElement instanceof HTMLElement && document.activeElement !== document.body ? document.activeElement : null;
    const el = asideRef.current;
    (el?.querySelector<HTMLElement>(FOCUSABLE) ?? el)?.focus();
    return () => {
      // Only a close hands focus back; turning into a column (still shown) leaves it where it is.
      if (showRef.current) return;
      if (document.activeElement !== document.body && !el?.contains(document.activeElement)) return;
      const back = opener?.isConnected ? opener : asideReturnRef?.current;
      back?.focus();
    };
  }, [overlay]);

  // Escape anywhere closes the drawer, unless a control inside (an open menu) already took it.
  useEffect(() => {
    if (!overlay) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Escape' || e.defaultPrevented) return;
      e.preventDefault();
      onAsideClose?.();
    };
    document.addEventListener('keydown', onKey);
    return () => document.removeEventListener('keydown', onKey);
  }, [overlay, onAsideClose]);

  // --d-ring-h is the ring's height, which the narrow layout moves the grid down by; 0px without a ring.
  useLayoutEffect(() => {
    const root = rootRef.current;
    const el = ringRef.current;
    if (root === null) return;
    // Also on <html> on a phone, where a settings page outside the shell starts below the ring.
    const html = layout === 'phone' ? document.documentElement : null;
    const measureRing = () => { const h = `${el?.offsetHeight ?? 0}px`; root.style.setProperty('--d-ring-h', h); html?.style.setProperty('--d-ring-h', h); };
    measureRing();
    const clear = () => html?.style.removeProperty('--d-ring-h');
    if (el === null || typeof ResizeObserver === 'undefined') return clear;
    const observer = new ResizeObserver(measureRing);
    observer.observe(el);
    return () => { observer.disconnect(); clear(); };
  }, [hasRing, layout]);
  const phone = layout === 'phone';
  const hasTabBar = phone && tabBar !== undefined && tabBar !== null && tabBar !== false;
  // Typing: an editable element anywhere in the document has focus (a dialog and a settings page included) AND the soft
  // keyboard is up, which is when a phone has lost the room under the composer. The tab bar steps aside.
  // On Android the keyboard can close (the back button) while the field keeps focus; the visual viewport says so (M4).
  const [fieldFocus, setFieldFocus] = useState(false);
  const [keyboardUp, setKeyboardUp] = useState(true);
  const typing = fieldFocus && keyboardUp;
  const typingCb = useRef(onTypingChange);
  typingCb.current = onTypingChange;
  useEffect(() => { typingCb.current?.(typing); }, [typing]);
  useEffect(() => {
    const root = rootRef.current;
    if (!phone || root === null) { setFieldFocus(false); return; }
    setFieldFocus(editable(document.activeElement));
    const onIn = (e: FocusEvent) => setFieldFocus(editable(e.target));
    const onOut = (e: FocusEvent) => setFieldFocus(editable(e.relatedTarget));
    document.addEventListener('focusin', onIn);
    document.addEventListener('focusout', onOut);
    return () => { document.removeEventListener('focusin', onIn); document.removeEventListener('focusout', onOut); setFieldFocus(false); };
  }, [phone]);
  // The keyboard is up when the visual viewport is clearly shorter than the window. Without a visualViewport it is assumed up.
  useEffect(() => {
    const vv: VisualViewport | null | undefined = window.visualViewport; // undefined in jsdom and old browsers
    if (!phone || vv === null || vv === undefined) { setKeyboardUp(true); return; }
    const read = () => setKeyboardUp(window.innerHeight - vv.height > KEYBOARD_PX);
    read();
    vv.addEventListener('resize', read);
    return () => { vv.removeEventListener('resize', read); setKeyboardUp(true); };
  }, [phone]);
  // WebKit fires no focusout when the focused field leaves the page (a dialog unmounts, a row re-renders): while typing,
  // a change to the page re-reads the active element.
  useEffect(() => {
    if (!fieldFocus || typeof MutationObserver === 'undefined') return;
    const observer = new MutationObserver(() => {
      const el = document.activeElement;
      if (!editable(el) || !(el as HTMLElement).isConnected) setFieldFocus(false);
    });
    observer.observe(document.body, { childList: true, subtree: true, attributes: true, attributeFilter: ['open', 'hidden', 'disabled', 'readonly'] });
    return () => observer.disconnect();
  }, [fieldFocus]);
  // --d-tabbar-h is the tab bar's height (0px while hidden or absent). It is also set on <html>, where a settings page outside the shell reads it.
  const tabBarRef = useRef<HTMLDivElement>(null);
  useLayoutEffect(() => {
    const root = rootRef.current;
    const el = tabBarRef.current;
    if (root === null || !phone) return;
    const html = document.documentElement;
    const measureTabs = () => { const h = `${el?.offsetHeight ?? 0}px`; root.style.setProperty('--d-tabbar-h', h); html.style.setProperty('--d-tabbar-h', h); };
    measureTabs();
    if (el === null || typeof ResizeObserver === 'undefined') return () => html.style.removeProperty('--d-tabbar-h');
    const observer = new ResizeObserver(measureTabs);
    observer.observe(el);
    return () => { observer.disconnect(); html.style.removeProperty('--d-tabbar-h'); };
  }, [phone, hasTabBar, typing]);
  const hasBanner = banner !== undefined && banner !== null && banner !== false;
  // --d-banner-h is the banner wrapper's height while a page covers a phone shell; it is set on <html>, where the page (and a
  // settings page outside the shell) starts below it. Cleared otherwise.
  const bannerRef = useRef<HTMLDivElement>(null);
  const bannerCover = phone && covered === true;
  useLayoutEffect(() => {
    if (!bannerCover) return;
    const html = document.documentElement;
    const el = bannerRef.current;
    const measureBanner = () => { html.style.setProperty('--d-banner-h', `${el?.offsetHeight ?? 0}px`); };
    measureBanner();
    const clear = () => html.style.removeProperty('--d-banner-h');
    if (el === null || typeof ResizeObserver === 'undefined') return clear;
    const observer = new ResizeObserver(measureBanner);
    observer.observe(el);
    return () => { observer.disconnect(); clear(); };
  }, [bannerCover, hasBanner]);
  const listPane = phone && phonePane === 'list';
  const sidebarRef = useRef<HTMLDivElement>(null);
  // No hash change and no history entry: focus moves to <main> (on a phone's list pane, the sidebar) and the URL stays.
  const onSkip = (e: MouseEvent<HTMLAnchorElement>) => {
    e.preventDefault();
    (listPane ? sidebarRef.current : mainRef.current)?.focus();
  };
  if (phone) {
    // One pane at a time. DOM order is the focus order: skip link, banner, (list: rail, sidebar, hidden main | view: main),
    // the aside and its backdrop, the call bar, the tab bar (on both panes; the sheet ends above it and it stays live). There is no status bar: the banner says when the link is down.
    const sidebarId = `${mainId}-list`;
    const inert = overlay || covered === true || undefined;
    const mainEl = (
      <main id={mainId} ref={mainRef} tabIndex={-1} className="d-app-shell__main" hidden={listPane || undefined} inert={inert}>
        {header}
        <div className="d-app-shell__content">{children}</div>
        {composer}
      </main>
    );
    return (
      <PhoneLayoutProvider value>
      <div ref={rootRef} className="d-app-shell" data-layout="phone" data-pane={phonePane} data-typing={typing ? 'true' : undefined} data-rail={listPane && rail === null ? 'none' : undefined}>
        <a className="d-app-shell__skip" href={`#${listPane ? sidebarId : mainId}`} onClick={onSkip} inert={covered === true || undefined}>{skipLabel}</a>
        <div className={showAside ? 'd-app-shell__grid d-app-shell__grid--aside' : 'd-app-shell__grid'}>
          {/* The ring is outside main (the list pane hides main), above whichever pane shows, and clears the notch. It is
              never inert and sits above the sheet (final fix I3), so a call can be answered while a sheet is open. */}
          {hasRing ? <div ref={ringRef} className="d-app-shell__ring">{ring}</div> : null}
          {hasBanner ? <div ref={bannerRef} className="d-app-shell__banner" inert={overlay || undefined}>{banner}</div> : null}
          {listPane ? (
            <>
              {rail === null ? null : <div className="d-app-shell__rail" inert={inert}>{rail}</div>}
              <div ref={sidebarRef} id={sidebarId} tabIndex={-1} className="d-app-shell__sidebar" inert={inert}>{sidebar}</div>
            </>
          ) : null}
          {mainEl}
          {overlay ? <div className="d-app-shell__backdrop" aria-hidden="true" onClick={() => onAsideClose?.()} /> : null}
          {showAside ? <aside ref={asideRef} aria-label={asideLabel} tabIndex={-1} className="d-app-shell__aside" inert={covered === true || undefined}>
            {overlay && asideCloseLabel !== undefined ? (
              <PhoneHeader title={asideLabel} backLabel={asideCloseLabel} onBack={() => onAsideClose?.()} />
            ) : null}
            {aside}
          </aside> : null}
          {callBar !== undefined && callBar !== null && callBar !== false ? <div className="d-app-shell__callbar" inert={inert}>{callBar}</div> : null}
          {page !== undefined && page !== null && page !== false ? <div className="d-app-shell__page">{page}</div> : null}
          {/* Fix round 2: a sheet covers the tab bar (Task 11, Q4), so the bar is inert under it, out of the tab order and the tree. */}
          {hasTabBar ? <div ref={tabBarRef} className="d-app-shell__tabbar" hidden={typing || undefined} inert={(overlay && covered !== true) || undefined}>{tabBar}</div> : null}
        </div>
      </div>
      </PhoneLayoutProvider>
    );
  }
  return (
    <div ref={rootRef} className="d-app-shell" style={paneVars} data-resizing={drag !== null ? 'true' : undefined} data-main-narrow={mainNarrow ? 'true' : undefined}>
      <a className="d-app-shell__skip" href={`#${mainId}`} onClick={onSkip}>{skipLabel}</a>
      <div className={asideMounted ? 'd-app-shell__grid d-app-shell__grid--aside' : 'd-app-shell__grid'} data-aside={asideMounted && shownKind === 'thread' ? 'thread' : undefined}>
        {hasTopBar ? <div className="d-app-shell__topbar" inert={overlay || undefined}>{topBar}</div> : null}
        {hasBanner ? <div className="d-app-shell__banner" inert={overlay || undefined}>{banner}</div> : null}
        <div ref={railRef} className="d-app-shell__rail" inert={overlay || undefined}>{rail}</div>
        <div id={resizable ? splitSidebarId : undefined} className="d-app-shell__sidebar" inert={overlay || undefined}>{sidebar}</div>
        {resizable ? splitter('sidebar', splitSidebarId) : null}
        {hasBottom ? <div className="d-app-shell__panel" inert={overlay || undefined}>{bottom}</div> : null}
        <main id={mainId} ref={mainRef} tabIndex={-1} className="d-app-shell__main" inert={overlay || undefined}>
          {hasRing ? <div ref={ringRef} className="d-app-shell__ring">{ring}</div> : null}
          {header}
          <div className="d-app-shell__content">{children}</div>
          {composer}
        </main>
        {overlay ? <div className="d-app-shell__backdrop" aria-hidden="true" onClick={() => onAsideClose?.()} /> : null}
        {asideMounted ? <aside ref={asideRef} id={resizable ? asideId : undefined} aria-label={showAside ? asideLabel : lastAside.current?.label ?? asideLabel} tabIndex={-1} className="d-app-shell__aside"
          data-open={showAside ? 'true' : 'false'} data-resizable={resizable && columnAside ? 'true' : undefined} inert={covered === true || !showAside || undefined}>
          {resizable && columnAside ? splitter('aside', asideId) : null}
          {overlay && asideCloseLabel !== undefined ? (
            <div className="d-app-shell__aside-head">
              <Button variant="ghost" size="sm" onClick={() => onAsideClose?.()}>{asideCloseLabel}</Button>
            </div>
          ) : null}
          {/* The body keeps the open width while the column animates, so its text never reflows. */}
          {/* The body clips by overflow, never by clip-path: a clip-path would cut every fixed-position popover inside the aside too
              (the member profile card hangs over main). Its scroller keeps the open width. */}
          <div className="d-app-shell__aside-body"><div className="d-app-shell__aside-scroll">{asideNode}</div></div>
        </aside> : null}
      </div>
    </div>
  );
}
