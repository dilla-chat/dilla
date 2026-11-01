import { pushHistoryState, replaceHistoryState, subscribeCommittedTraversal } from './router.ts';
import { useCallback, useEffect, useRef, useSyncExternalStore } from 'react';

/**
 * The phone breakpoint (flow 12, R9): a window under 47.99rem wide, or a coarse-pointer window under 31.99rem tall (a
 * landscape phone). The page's media queries in ui and web read the same string; the shell's container queries cannot see
 * the viewport and have `[data-layout="phone"]` twins instead.
 */
export const PHONE_QUERY = '(max-width: 47.99rem), (pointer: coarse) and (max-height: 31.99rem)';

function subscribe(onChange: () => void): () => void {
  if (typeof window.matchMedia !== 'function') return () => undefined;
  const query = window.matchMedia(PHONE_QUERY);
  query.addEventListener('change', onChange);
  return () => query.removeEventListener('change', onChange);
}
const snapshot = (): boolean => typeof window.matchMedia === 'function' && window.matchMedia(PHONE_QUERY).matches;

/** The layout right now, read from the window (not from a render): for handlers that must act on the live viewport. */
export const phoneNow = snapshot;

/** True on a phone-width window; false where there is no matchMedia (the server, the test DOM). */
export function usePhoneLayout(): boolean {
  return useSyncExternalStore(subscribe, snapshot, () => false);
}

/** An element the soft keyboard types into. */
const editable = (el: Element | null): el is HTMLElement => el instanceof HTMLElement
  && (el.isContentEditable || el instanceof HTMLTextAreaElement
    || (el instanceof HTMLInputElement && !['button', 'checkbox', 'radio', 'submit', 'reset', 'range', 'file', 'image', 'color'].includes(el.type)));

/**
 * What a mounted phone shell does to the window, while enabled (R10). Sets `data-phone-shell` on <html> (the page lock in
 * app.css reads it), and keeps `--d-vvh` and `--d-vvtop` on <html> at the visual viewport's height and offset, so the shell
 * shrinks above a soft keyboard, follows iOS panning the viewport, and the composer stays in view. After a resize that shrinks
 * the height, the focused field is scrolled into view on the next frame (m7). All of it is removed when the hook unmounts or
 * turns off.
 */
export function useVisualViewportHeight(enabled = true): void {
  useEffect(() => {
    if (!enabled) return;
    const root = document.documentElement;
    root.setAttribute('data-phone-shell', '');
    const vv = window.visualViewport;
    if (vv === null || vv === undefined) return () => root.removeAttribute('data-phone-shell');
    let height = vv.height;
    let frame = 0;
    const set = () => {
      root.style.setProperty('--d-vvh', `${vv.height}px`);
      root.style.setProperty('--d-vvtop', `${vv.offsetTop}px`);
    };
    const onResize = () => {
      set();
      const shrank = vv.height < height;
      height = vv.height;
      if (!shrank) return;
      cancelAnimationFrame(frame);
      frame = requestAnimationFrame(() => {
        const active = document.activeElement;
        if (editable(active)) active.scrollIntoView({ block: 'nearest' });
      });
    };
    set();
    vv.addEventListener('resize', onResize);
    vv.addEventListener('scroll', set);
    return () => {
      cancelAnimationFrame(frame);
      vv.removeEventListener('resize', onResize);
      vv.removeEventListener('scroll', set);
      root.style.removeProperty('--d-vvh');
      root.style.removeProperty('--d-vvtop');
      root.removeAttribute('data-phone-shell');
    };
  }, [enabled]);
}

export type PhoneTab = 'home' | 'activity' | 'you';

const TAB_KEY = 'dilla.phoneTab.';

/** Remembers where a tab was last left, for this browser tab only. A blocked sessionStorage remembers nothing. */
export function rememberTab(tab: PhoneTab, path: string): void {
  // eslint-disable-next-line no-restricted-globals -- a remembered path is not a key, a token or state worth protecting (flow 12)
  try { sessionStorage.setItem(TAB_KEY + tab, path); } catch { /* blocked storage: no memory, the tab opens at its root */ }
}

/** The path a tab was last left at, or null. Home also reads what web3.8's `servers` and `dms` tabs left in this browser tab. */
export function rememberedTab(tab: PhoneTab): string | null {
  const keys = tab === 'home' ? ['home', 'servers', 'dms'] : [tab];
  try {
    for (const key of keys) {
      // eslint-disable-next-line no-restricted-globals -- a remembered path is not a key, a token or state worth protecting (flow 12)
      const kept = sessionStorage.getItem(TAB_KEY + key);
      if (kept !== null) return kept;
    }
    return null;
  } catch { return null; }
}

const PANE_KEY = 'dilla.paneWidths';

export type PaneWidths = { sidebar?: number; aside?: number };

const width = (v: unknown): v is number => typeof v === 'number' && Number.isFinite(v) && v > 0;

/** The pane widths this browser kept (R18): finite positive numbers only; anything else, or blocked storage, is `{}`. */
export function readPaneWidths(): PaneWidths {
  try {
    // eslint-disable-next-line no-restricted-globals -- a pane width is a per-viewer convenience, not a key, token or state worth protecting
    const raw: unknown = JSON.parse(localStorage.getItem(PANE_KEY) ?? 'null');
    if (typeof raw !== 'object' || raw === null || Array.isArray(raw)) return {};
    const o = raw as Record<string, unknown>;
    return { ...(width(o.sidebar) ? { sidebar: o.sidebar } : {}), ...(width(o.aside) ? { aside: o.aside } : {}) };
  } catch { return {}; }
}

/** Keeps the pane widths for this browser. A blocked localStorage keeps nothing. */
export function writePaneWidths(w: PaneWidths): void {
  // eslint-disable-next-line no-restricted-globals -- a pane width is a per-viewer convenience, not a key, token or state worth protecting
  try { localStorage.setItem(PANE_KEY, JSON.stringify(w)); } catch { /* blocked storage: the widths are not kept */ }
}

/** The history-state key of an open sheet (R13). */
const SHEET = 'phoneSheet';
const sheetOf = (state: unknown): unknown =>
  typeof state === 'object' && state !== null ? (state as Record<string, unknown>)[SHEET] : undefined;

/**
 * A sheet on a phone is a history entry, so the system back closes the sheet and not the view under it. While `open`,
 * one entry carrying `phoneSheet: <name>` is pushed on top of the current one (its state kept). A popstate that leaves
 * that entry calls `onClose`. `close()` is what the sheet's own controls call: it pops the entry (which ends in
 * `onClose` through the popstate, and returns true), or calls `onClose` at once when the sheet has no entry (returns false).
 */
export function usePhoneSheet(name: 'members' | 'thread', open: boolean, onClose: () => void): { close: () => boolean } {
  const onCloseRef = useRef(onClose);
  onCloseRef.current = onClose;
  const pushed = useRef(false);
  useEffect(() => {
    // An entry that names this sheet while it is closed is stale (a reload, a forward, a restored tab): it is stripped,
    // so a later close never pops the view itself.
    const strip = () => {
      if (sheetOf(window.history.state) !== name) return;
      const rest = { ...(window.history.state as Record<string, unknown>) };
      delete rest[SHEET];
      replaceHistoryState(Object.keys(rest).length === 0 ? null : rest, '', location.href);
    };
    if (!open) {
      pushed.current = false;
      strip();
      return subscribeCommittedTraversal(strip);
    }
    strip();
    const current = typeof window.history.state === 'object' && window.history.state !== null ? window.history.state as Record<string, unknown> : {};
    pushHistoryState({ ...current, [SHEET]: name }, '', location.href);
    pushed.current = true;
    const onPop = () => {
      if (sheetOf(window.history.state) === name) return;
      pushed.current = false;
      onCloseRef.current();
    };
    const unsubscribe = subscribeCommittedTraversal(onPop);
    return () => unsubscribe();
  }, [open, name]);
  const close = useCallback(() => {
    if (pushed.current && sheetOf(window.history.state) === name) { pushed.current = false; window.history.back(); return true; }
    onCloseRef.current();
    return false;
  }, [name]);
  return { close };
}
