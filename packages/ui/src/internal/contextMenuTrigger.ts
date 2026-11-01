import { useEffect, useRef, type KeyboardEvent, type MouseEvent, type PointerEvent, type SyntheticEvent } from 'react';

/**
 * Ruling X's context-menu routes for an element (a voice row's person, a stage tile): a right-click, the ContextMenu key or
 * Shift+F10 (where both a keydown and a contextmenu arrive for one press, the menu opens once), and a touch held 500ms (the
 * click that ends it is swallowed, so it does not also act). `open` gets the element and where the menu should sit.
 * Without `open` the element gets no handlers.
 *
 * The handlers never stop their events: the page's own listeners (the AFK activity report on any input) must still see a
 * press or a move over a tile or a person. An enclosing element with a menu of its own (the voice row around its people)
 * ignores events from inside the person's row instead (ChannelList). They ignore events that only bubble through React from
 * the portalled menu itself (a key or a press inside the open menu is not a new request).
 */
export function useContextMenuTrigger<T extends HTMLElement>(open: ((el: T, at: { x: number; y: number }) => void) | undefined) {
  const keyOpenedAt = useRef(-Infinity);
  const press = useRef<{ timer: ReturnType<typeof setTimeout>; x: number; y: number } | null>(null);
  const swallowClick = useRef(false);
  useEffect(() => () => { if (press.current !== null) clearTimeout(press.current.timer); }, []);
  if (open === undefined) return {};
  const cancel = () => { if (press.current !== null) { clearTimeout(press.current.timer); press.current = null; } };
  /** True for an event of this element's own DOM (not one bubbling through React from the portalled menu). */
  const own = (e: SyntheticEvent<T>) => e.target instanceof Node && e.currentTarget.contains(e.target);
  return {
    onContextMenu: (e: MouseEvent<T>) => {
      if (!own(e)) return;
      e.preventDefault();
      if (e.timeStamp - keyOpenedAt.current < 500) return;
      open(e.currentTarget, { x: e.clientX, y: e.clientY });
    },
    onKeyDown: (e: KeyboardEvent<T>) => {
      if (!own(e) || (e.key !== 'ContextMenu' && !(e.key === 'F10' && e.shiftKey))) return;
      e.preventDefault();
      keyOpenedAt.current = e.timeStamp;
      const box = e.currentTarget.getBoundingClientRect();
      open(e.currentTarget, { x: box.left + 8, y: box.bottom });
    },
    onPointerDown: (e: PointerEvent<T>) => {
      if (!own(e)) return;
      swallowClick.current = false;
      if (e.pointerType !== 'touch') return;
      const el = e.currentTarget;
      const at = { x: e.clientX, y: e.clientY };
      cancel();
      press.current = { x: at.x, y: at.y, timer: setTimeout(() => {
        press.current = null;
        swallowClick.current = true;
        setTimeout(() => { swallowClick.current = false; }, 1000);
        open(el, at);
      }, 500) };
    },
    onPointerMove: (e: PointerEvent<T>) => {
      if (!own(e)) return;
      const p = press.current;
      if (p !== null && Math.hypot(e.clientX - p.x, e.clientY - p.y) > 10) cancel();
    },
    onPointerUp: (e: PointerEvent<T>) => { if (own(e)) cancel(); },
    onPointerCancel: (e: PointerEvent<T>) => { if (own(e)) cancel(); },
    onClickCapture: (e: MouseEvent<T>) => {
      if (!swallowClick.current) return;
      swallowClick.current = false;
      e.preventDefault();
      e.stopPropagation();
    },
  };
}
