import { useEffect, useId, useLayoutEffect, useRef, type ReactNode } from 'react';
import { Button } from '../Button/Button.tsx';
import './Dialog.css';

export type DialogProps = { open: boolean; title: string; onClose: () => void; children: ReactNode; footer?: ReactNode; closeLabel?: string;
  /** The id of the element inside that describes the dialog (a confirmation's body). */
  describedBy?: string;
  /**
   * Where focus starts when nothing inside is marked `data-autofocus` (an enabled marked element always wins): the
   * dialog itself (the default) or its close button.
   */
  initialFocus?: 'dialog' | 'close';
  /**
   * Where focus goes on close when the opener is gone and no element carries its `data-focus-key` any more (a deleted
   * row). Resolved at close time.
   */
  returnFocus?: () => HTMLElement | null | undefined;
  /**
   * Task 9, the prototype's two dialog layouts: `form` (the default; a head bar over a hairline, the body, a footer bar) or `confirm`
   * (no bars: `lead` (a 40px avatar) beside the 15px title and `description`, the fields, then the buttons).
   */
  kind?: 'form' | 'confirm';
  /** Drawn left of the title (a confirmation's avatar). */
  lead?: ReactNode;
  /** Drawn under the title, in the head (a confirmation's body). */
  description?: ReactNode };

/**
 * Where focus goes back to when a dialog closes, resolved at close time: the opener while it is still in the document;
 * else the element that now carries the opener's `data-focus-key` (a row re-mounted elsewhere while the dialog was
 * open); else `fallback`. Null when none of them exists: the caller then leaves focus alone.
 */
export function focusReturnTarget(opener: HTMLElement | null, fallback?: () => HTMLElement | null | undefined, scope?: ParentNode | null): HTMLElement | null {
  if (opener !== null && opener.isConnected) return opener;
  const key = opener?.dataset.focusKey;
  if (key !== undefined) {
    const same = Array.from((scope ?? document).querySelectorAll<HTMLElement>('[data-focus-key]')).find(e => e.dataset.focusKey === key);
    if (same !== undefined) return same;
  }
  return fallback?.() ?? null;
}

const AUTOFOCUS = '[data-autofocus]:not(:disabled)';

/** Whether `e` is hidden inside `root`: by the `hidden` attribute, or by display:none or visibility:hidden on it or an ancestor. */
function hiddenIn(e: HTMLElement, root: HTMLElement): boolean {
  for (let n: HTMLElement | null = e; n !== null && n !== root; n = n.parentElement) {
    if (n.hidden) return true;
    const style = getComputedStyle(n);
    if (style.display === 'none' || style.visibility === 'hidden') return true;
  }
  return false;
}

/** The first enabled, shown element inside the dialog marked `data-autofocus`, or null. */
function marked(root: HTMLElement): HTMLElement | null {
  return Array.from(root.querySelectorAll<HTMLElement>(AUTOFOCUS)).find(e => !hiddenIn(e, root)) ?? null;
}

export function Dialog({ open, title, onClose, children, footer, closeLabel, describedBy, initialFocus = 'dialog', returnFocus, kind = 'form', lead, description }: DialogProps) {
  const ref = useRef<HTMLDialogElement>(null);
  const titleId = useId();
  // What had focus when the dialog opened, and the latest fallback (read at close time, so never stale).
  const opener = useRef<HTMLElement | null>(null);
  // Where the dialog put focus on opening when nothing inside was marked yet (contents still loading): a marked element
  // that arrives later takes focus from it, unless the user has moved focus since.
  const provisional = useRef<HTMLElement | null>(null);
  const fallback = useRef(returnFocus);
  fallback.current = returnFocus;
  // Focus goes back only when it was in the dialog (or was dropped on the body by the dialog leaving the document);
  // a flow that already moved focus elsewhere keeps it there.
  const giveBack = (el: HTMLDialogElement) => {
    const active = document.activeElement;
    const inside = active === null || active === document.body || el.contains(active);
    if (el.open) el.close();
    if (!inside) return;
    const target = focusReturnTarget(opener.current, fallback.current);
    opener.current = null;
    target?.focus();
  };
  // The <dialog> element is opened in the effect below and closed in this layout effect. The browser's own focus return
  // on close() covers only an opener still in the document and a dialog that stays mounted, so the return target is
  // resolved here instead. The opener is read here too, before any child's effect can move focus into the still-closed dialog.
  // The close runs in the commit that empties the dialog: a passive effect left an open <dialog> with nothing in it
  // until React's next task, and an Escape landing there was that empty dialog's cancel, so a settings frame under it
  // stayed open.
  useLayoutEffect(() => {
    const el = ref.current; if (!el) return;
    if (!open) { if (el.open) { provisional.current = null; giveBack(el); } return; }
    if (el.open) return;
    const active = document.activeElement;
    opener.current = active instanceof HTMLElement && active !== document.body && !el.contains(active) ? active : null;
  }, [open]); // eslint-disable-line react-hooks/exhaustive-deps
  // This effect runs after the children's, so a marked element inside wins over the dialog itself.
  useEffect(() => {
    const el = ref.current; if (!el || !open || el.open) return;
    el.showModal();
    const target = marked(el);
    const start = target ?? (initialFocus === 'close' ? el.querySelector<HTMLElement>('.d-dialog__footer button') : null) ?? el;
    start.focus();
    provisional.current = target === null ? start : null;
  }, [open]); // eslint-disable-line react-hooks/exhaustive-deps
  // After every render while open: a marked element that has just arrived takes over a provisional start, once.
  useEffect(() => {
    const el = ref.current; const start = provisional.current;
    if (!el || !el.open || start === null) return;
    if (document.activeElement !== start) { provisional.current = null; return; }
    const target = marked(el);
    if (target === null) return;
    provisional.current = null;
    target.focus();
  });
  // A dialog torn down while open (a flow mounted only while it shows) hands focus back too: by then the element has
  // left the document and the browser has put focus on the body.
  useEffect(() => {
    const el = ref.current;
    return () => { if (el?.open) giveBack(el); };
  }, []); // eslint-disable-line react-hooks/exhaustive-deps
  return (
    <dialog ref={ref} className="d-dialog" data-kind={kind} aria-labelledby={titleId} aria-describedby={open ? describedBy : undefined} aria-modal="true" tabIndex={-1}
      onCancel={e => { e.preventDefault(); onClose(); }}
      onKeyDown={e => { if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); onClose(); } }}
      onClick={e => { if (e.target === ref.current) onClose(); }}>
      {/* Contents exist only while open: a closed dialog leaks no hidden text. */}
      {open ? (
        <div className="d-dialog__panel" onClick={e => e.stopPropagation()}>
          <div className="d-dialog__head">
            {lead}
            <div className="d-dialog__heading">
              <h2 id={titleId} className="d-dialog__title">{title}</h2>
              {description}
            </div>
          </div>
          {children === undefined || children === null || children === false ? null : <div className="d-dialog__body">{children}</div>}
          <div className="d-dialog__footer">
            <Button variant="ghost" onClick={onClose}>{closeLabel ?? 'Close'}</Button>
            {footer}
          </div>
        </div>
      ) : null}
    </dialog>
  );
}
