import './StateDot.css';

export type StateDotTone = 'ok' | 'off' | 'pending' | 'idle' | 'dnd' | 'warn' | 'danger';

export interface StateDotProps {
  tone: StateDotTone;
  size?: 'sm' | 'md';
}

/**
 * A small state marker drawn in CSS (no font glyph). Filled for ok, warn and danger; a ring for off; a dashed ring
 * for pending. Idle and dnd are solid discs in signal (the prototype, Jonas 2026-10-27); light, high-contrast and
 * forced colours keep a half-filled ring for idle and a bar on dnd. It never carries meaning alone: every
 * caller keeps its text or a `d-sr-only` word beside it, so it is `aria-hidden`.
 */
export function StateDot({ tone, size }: StateDotProps) {
  return <span className="d-state-dot" data-tone={tone} data-size={size ?? 'sm'} aria-hidden="true" />;
}
