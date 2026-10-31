/** UI read budget; the existing operation returns at most 20 voters per page. */
const PAGES_PER_ANSWER = 10;
const PAGES_PER_POLL = 10;
type Page = { voters: readonly string[]; nextCursor: Uint8Array | null };
export interface PollVoterRead { previews: string[][]; distinct: number; capped: boolean }
function advances(next: Uint8Array, previous: Uint8Array): boolean {
  // The validated CBOR cursor has a fixed layout and an ascending, fixed-length server key.
  for (let i = 0; i < Math.min(next.length, previous.length); i++) {
    if (next[i] !== previous[i]) return next[i] > previous[i];
  }
  return next.length > previous.length;
}
export async function walkPollVoters(counts: readonly bigint[], multi: boolean,
  read: (option: number, cursor: Uint8Array | null) => Promise<Page>, current: () => boolean = () => true): Promise<PollVoterRead> {
  const previews = counts.map(() => [] as string[]);
  const complete = counts.map(count => count === 0n);
  const distinct = new Set<string>(); const upper = counts.reduce((sum, count) => sum + count, 0n);
  let pages = 0, nextAnswer = 0, hitCap = false, stopped = false;
  const worker = async () => {
    while (!stopped && nextAnswer < counts.length && current() && BigInt(distinct.size) < upper) {
      const option = nextAnswer++;
      if (complete[option]) continue;
      const ids = new Set<string>(); let cursor: Uint8Array | null = null; let answerPages = 0;
      do {
        if (stopped || !current() || BigInt(distinct.size) >= upper) return;
        if (pages >= PAGES_PER_POLL || answerPages >= PAGES_PER_ANSWER) { hitCap = true; break; }
        // Reserve the shared budget synchronously before either worker awaits I/O.
        pages++; answerPages++;
        const page = await read(option, cursor);
        if (stopped || !current()) return;
        if (page.nextCursor !== null && (page.voters.length === 0 || cursor !== null && !advances(page.nextCursor, cursor))) throw new Error('Voter pagination made no progress');
        if (cursor === null) previews[option] = page.voters.slice(0, 5);
        for (const id of page.voters) { ids.add(id); distinct.add(id); }
        cursor = multi ? page.nextCursor : null;
        complete[option] = cursor === null || BigInt(ids.size) >= counts[option];
      } while (!complete[option]);
    }
  };
  const failures: unknown[] = [];
  const run = async () => { try { await worker(); } catch (cause) { stopped = true; failures.push(cause); } };
  // Drain the other in-flight request before releasing this walk's concurrency slot.
  await Promise.all([run(), run()]);
  if (failures.length) throw failures[0];
  return { previews, distinct: distinct.size, capped: hitCap && BigInt(distinct.size) < upper && !complete.every(Boolean) };
}
