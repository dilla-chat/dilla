import { useCallback, useRef } from 'react';

/**
 * Task 10b fix round 2: a native range's moves and commits. `input` previews (commit false). The native `change` event
 * commits; it fires when a pointer lets go, per keyboard step, and when VoiceOver or TalkBack set the value (they send no
 * key or pointer events). A blur or pointercancel after an uncommitted move commits it too. Listeners are native because
 * React's onChange for a range fires on `input` as well.
 */
export function useRangeCommit(onChange: (value: number, commit: boolean) => void, parse: (value: number) => number = v => v) {
  const handlers = useRef({ onChange, parse });
  handlers.current = { onChange, parse };
  const detach = useRef<(() => void) | null>(null);
  // A callback ref: the listeners follow the element, which may mount after the hook (the release delay shows with push to talk).
  const ref = useCallback((el: HTMLInputElement | null) => {
    detach.current?.();
    detach.current = null;
    if (el === null) return;
    // The value the last input moved to: a controlled range may show its old prop again before the commit arrives.
    let pending: number | null = null;
    const read = () => handlers.current.parse(Number(el.value));
    const input = () => { pending = read(); handlers.current.onChange(pending, false); };
    const change = () => { const v = pending ?? read(); pending = null; handlers.current.onChange(v, true); };
    const settle = () => { if (pending !== null) change(); };
    el.addEventListener('input', input);
    el.addEventListener('change', change);
    el.addEventListener('blur', settle);
    el.addEventListener('pointercancel', settle);
    detach.current = () => {
      el.removeEventListener('input', input);
      el.removeEventListener('change', change);
      el.removeEventListener('blur', settle);
      el.removeEventListener('pointercancel', settle);
    };
  }, []);
  /** Spread onto the input: the ref, and a no-op onChange so React keeps it a controlled input. */
  return { ref, onChange: () => undefined };
}
