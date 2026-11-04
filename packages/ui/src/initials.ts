/** Match the poll's two-character tile without splitting an emoji or combining sequence. */
export function graphemeInitials(name: string): string {
  const segments = new Intl.Segmenter(undefined, { granularity: 'grapheme' }).segment(name);
  return Array.from(segments, part => part.segment).slice(0, 2).join('').toUpperCase();
}
