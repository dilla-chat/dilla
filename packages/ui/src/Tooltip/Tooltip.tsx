import { cloneElement, useCallback, useEffect, useId, useLayoutEffect, useRef, useState, type FocusEvent, type PointerEvent, type ReactElement, type ReactNode } from 'react';
import { createPortal } from 'react-dom';
import { KeyHint } from '../KeyHint/KeyHint.tsx';
import './Tooltip.css';

export type TooltipProps = {
  /** The control's accessible name, shown as text. The tooltip never adds a second name (it is not `aria-labelledby`). */
  label: string;
  /** The chord, drawn as a KeyHint chip at the right and read once as the control's description. */
  keys?: string[];
  /** Rich tooltip content, using the same hover, focus and placement behavior. */
  content?: ReactNode;
  placement?: 'top' | 'bottom' | 'right';
  /** Exactly one focusable element. */
  children: ReactElement<{ 'aria-describedby'?: string }>;
};

const SHOW_MS = 400;
const HIDE_MS = 100;
const GAP = 6;
const MARGIN = 8;

/** A touch screen or a device that cannot hover gets no tooltip (R15): the label is the name, and a long press is the OS's. */
function cannotHover(): boolean {
  return typeof window !== 'undefined' && typeof window.matchMedia === 'function' && window.matchMedia('(hover: none)').matches;
}

function canFocusVisible(el: Element): boolean {
  try { return el.matches(':focus-visible'); } catch { return true; }
}

/**
 * A hover and focus tooltip (R15, WCAG 1.4.13). It shows 400 ms after the pointer enters and at once on keyboard focus;
 * it hides 100 ms after the pointer leaves unless the pointer is on the tooltip (hoverable), on blur, and on Escape. Escape
 * is consumed only while a tooltip is shown, so a dialog behind it still closes on the next press; focus stays where it was.
 * The tooltip is drawn in a portal with position: fixed, so no list or rail that clips its overflow can cut it off.
 */
export function Tooltip({ label, keys, content, placement = 'top', children }: TooltipProps) {
  const id = useId();
  const chordId = `${id}-chord`;
  const anchor = useRef<HTMLSpanElement>(null);
  const tip = useRef<HTMLSpanElement>(null);
  const showTimer = useRef<number | undefined>(undefined);
  const hideTimer = useRef<number | undefined>(undefined);
  const touched = useRef(false);
  const [shown, setShown] = useState(false);
  const [pos, setPos] = useState<{ left: number; top: number }>({ left: 0, top: 0 });
  // A top tooltip with no room above its control (a bar at the top of the window) opens below it instead, so it never covers the control.
  const [flipped, setFlipped] = useState(false);

  const clear = useCallback(() => {
    window.clearTimeout(showTimer.current);
    window.clearTimeout(hideTimer.current);
  }, []);
  useEffect(() => clear, [clear]);

  // Place the tooltip against the anchor, then keep it inside the viewport.
  const place = useCallback(() => {
    if (!anchor.current || !tip.current) return;
    const a = anchor.current.getBoundingClientRect();
    const t = tip.current.getBoundingClientRect();
    const vw = document.documentElement.clientWidth;
    const vh = document.documentElement.clientHeight;
    let left: number;
    let top: number;
    let below = false;
    if (placement === 'right') {
      left = a.right + GAP;
      top = a.top + a.height / 2 - t.height / 2;
    } else {
      left = a.left + a.width / 2 - t.width / 2;
      top = placement === 'top' ? a.top - t.height - GAP : a.bottom + GAP;
      if (placement === 'top' && top < MARGIN) { top = a.bottom + GAP; below = true; }
    }
    left = Math.max(MARGIN, Math.min(left, vw - t.width - MARGIN));
    top = Math.max(MARGIN, Math.min(top, vh - t.height - MARGIN));
    setFlipped(below);
    setPos(p => (p.left === left && p.top === top ? p : { left, top }));
  }, [placement]);
  useLayoutEffect(() => { if (shown) place(); }, [shown, place, label, keys]);

  // A scroll in any ancestor (capture, so the sidebar's own scroll counts) or a resize moves the control: follow it, once per frame.
  useEffect(() => {
    if (!shown) return undefined;
    let frame = 0;
    const onMove = () => {
      if (frame) return;
      frame = window.requestAnimationFrame(() => { frame = 0; place(); });
    };
    document.addEventListener('scroll', onMove, true);
    window.addEventListener('resize', onMove);
    return () => {
      document.removeEventListener('scroll', onMove, true);
      window.removeEventListener('resize', onMove);
      if (frame) window.cancelAnimationFrame(frame);
    };
  }, [shown, place]);

  // Dismissible without moving the pointer or the focus (1.4.13): Escape, taken only while shown, before anything behind sees it.
  useEffect(() => {
    if (!shown) return undefined;
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Escape') return;
      e.stopPropagation();
      clear();
      setShown(false);
    };
    document.addEventListener('keydown', onKey, true);
    return () => document.removeEventListener('keydown', onKey, true);
  }, [shown, clear]);

  const onPointerEnter = (e: PointerEvent) => {
    window.clearTimeout(hideTimer.current);
    touched.current = e.pointerType === 'touch';
    if (touched.current || cannotHover() || shown) return;
    window.clearTimeout(showTimer.current);
    showTimer.current = window.setTimeout(() => setShown(true), SHOW_MS);
  };
  const onPointerLeave = () => {
    window.clearTimeout(showTimer.current);
    window.clearTimeout(hideTimer.current);
    hideTimer.current = window.setTimeout(() => setShown(false), HIDE_MS);
  };
  const onPointerDown = (e: PointerEvent) => {
    touched.current = e.pointerType === 'touch';
    if (touched.current) { clear(); setShown(false); }
  };
  const onFocus = (e: FocusEvent) => {
    if (!canFocusVisible(e.target as Element)) return;
    clear();
    setShown(true);
  };
  const onBlur = () => { clear(); setShown(false); };

  const describedBy = keys?.length ? [children.props['aria-describedby'], chordId].filter(Boolean).join(' ') : children.props['aria-describedby'];

  return (
    <span ref={anchor} className="d-tooltip-anchor" onPointerEnter={onPointerEnter} onPointerLeave={onPointerLeave} onPointerDown={onPointerDown}
      onFocus={onFocus} onBlur={onBlur}>
      {cloneElement(children, describedBy ? { 'aria-describedby': describedBy } : {})}
      {keys?.length ? <span id={chordId} className="d-sr-only">{keys.join(' ')}</span> : null}
      {shown ? createPortal(
        <span ref={tip} role="tooltip" className="d-tooltip" data-placement={placement === 'top' && flipped ? 'bottom' : placement}
          style={{ left: pos.left, top: pos.top }}>
          <span className="d-tooltip__label">{content ?? label}</span>
          {keys?.length ? <span aria-hidden="true"><KeyHint keys={keys} /></span> : null}
        </span>,
        document.body,
      ) : null}
    </span>
  );
}
