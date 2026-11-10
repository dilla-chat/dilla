// Bot v2: the round trip of each socket request (frame written to response read), kept per route pattern for Diagnostics.

/** The samples kept per pattern; older ones drop out, so a slow start does not stay on the list for the worker's life. */
export const RTT_SAMPLES = 128;
/** The patterns kept; a pattern past this many is not recorded. The manifest holds far fewer. */
export const RTT_PATTERNS = 512;

export interface RouteTiming { pattern: string; p50: number; p95: number; count: number }

/** The nearest-rank percentile of sorted samples. */
function percentile(sorted: readonly number[], p: number): number {
  return sorted[Math.min(sorted.length - 1, Math.max(0, Math.ceil(p * sorted.length) - 1))];
}

export class RouteRtt {
  private readonly samples = new Map<string, { recent: number[]; next: number; count: number }>();

  record(pattern: string, ms: number): void {
    if (!Number.isFinite(ms) || ms < 0) return;
    let entry = this.samples.get(pattern);
    if (entry === undefined) {
      if (this.samples.size >= RTT_PATTERNS) return;
      entry = { recent: [], next: 0, count: 0 };
      this.samples.set(pattern, entry);
    }
    const rounded = Math.round(ms);
    if (entry.recent.length < RTT_SAMPLES) entry.recent.push(rounded);
    else entry.recent[entry.next] = rounded;
    entry.next = (entry.next + 1) % RTT_SAMPLES;
    entry.count += 1;
  }

  /** The n patterns with the highest p95 over their recent samples, slowest first; ties by pattern. `count` is every
   *  request recorded for the pattern. */
  slowest(n: number): RouteTiming[] {
    const rows: RouteTiming[] = [];
    for (const [pattern, entry] of this.samples) {
      const sorted = [...entry.recent].sort((a, b) => a - b);
      rows.push({ pattern, p50: percentile(sorted, 0.5), p95: percentile(sorted, 0.95), count: entry.count });
    }
    rows.sort((a, b) => b.p95 - a.p95 || (a.pattern < b.pattern ? -1 : a.pattern > b.pattern ? 1 : 0));
    return rows.slice(0, Math.max(0, n));
  }
}
