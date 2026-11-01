import './TypingIndicator.css';
export type TypingIndicatorProps = { names: readonly string[]; labels: { one: string; many: string } };
/** "Björn is typing…" in the accent, for the composer's footer (README §5.5); a polite status. Nothing while nobody types. */
export function TypingIndicator({ names, labels }: TypingIndicatorProps) {
  const distinct = [...new Set(names)];
  if (distinct.length === 0) return null;
  return <span className="d-typing-indicator" role="status" aria-live="polite">{distinct.length === 1 ? labels.one.replace('{name}', distinct[0]) : labels.many}</span>;
}
