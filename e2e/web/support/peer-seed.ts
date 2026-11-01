/** Native peer usernames retain only 24 seed bits. Workers, including replacements after a
 * failure, need disjoint ranges while the invocation's shared host keeps earlier accounts. */
export function peerSeed(workerIndex: number, sequence: number): number {
  if (!Number.isInteger(workerIndex) || workerIndex < 0 || workerIndex > 254) throw new RangeError('peer worker index exceeds its 24-bit namespace');
  if (!Number.isInteger(sequence) || sequence < 0 || sequence > 65_535) throw new RangeError('peer worker exhausted its username range');
  return (workerIndex + 1) * 65_536 + sequence;
}
