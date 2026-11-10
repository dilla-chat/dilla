// Per-device token buckets at 80 % of the server rates (ruling 20).
export type Bucket = 'read' | 'write' | 'message' | 'commit' | 'proposal' | 'upload' | 'bot' | 'none';
export const PACING = {
  read: { perSecond: 8, burst: 48 }, write: { perSecond: 1.6, burst: 16 },
  message: { perSecond: 0.8, burst: 16 }, commit: { perSecond: 1.6, burst: 32 },
  proposal: { perSecond: 0.4, burst: 8 },
  // 80 % of blobs.uploads_per_minute (20, internal/api/blobmeter.go), per user.
  upload: { perSecond: 16 / 60, burst: 16 },
  // Task 21b fix round 1: POST …/bot/commands, the picker's commands and every Bot surface's reads and actions, in its own
  // bucket (dillad's bot_command meter at the read rate), so typing and other writes never hold a Bot answer back.
  bot: { perSecond: 8, burst: 48 },
} as const;

type MeteredBucket = Exclude<Bucket, 'none'>;
type State = { tokens: number; last: number };
const abort = (): DOMException => new DOMException('The operation was aborted.', 'AbortError');

export class TokenBuckets {
  private readonly states = new Map<MeteredBucket, State>();
  private readonly tails = new Map<MeteredBucket, Promise<void>>();

  constructor(private readonly deps: { now(): number; sleep(ms: number, signal?: AbortSignal): Promise<void> },
    private readonly config: typeof PACING = PACING) {}

  take(bucket: Bucket, signal?: AbortSignal): Promise<void> {
    if (signal?.aborted) return Promise.reject(abort());
    if (bucket === 'none') return Promise.resolve();
    const previous = this.tails.get(bucket) ?? Promise.resolve();
    const result = previous.then(() => this.acquire(bucket, signal));
    this.tails.set(bucket, result.catch(() => undefined));
    return result;
  }

  private async acquire(bucket: MeteredBucket, signal?: AbortSignal): Promise<void> {
    const config = this.config[bucket];
    let state = this.states.get(bucket);
    if (!state) {
      state = { tokens: config.burst, last: this.deps.now() };
      this.states.set(bucket, state);
    }
    for (;;) {
      if (signal?.aborted) throw abort();
      const now = this.deps.now();
      state.tokens = Math.min(config.burst, state.tokens + Math.max(0, now - state.last) * config.perSecond / 1000);
      state.last = now;
      if (state.tokens >= 1 - 1e-9) {
        state.tokens = Math.max(0, state.tokens - 1);
        return;
      }
      await this.deps.sleep(Math.ceil((1 - state.tokens) * 1000 / config.perSecond), signal);
    }
  }
}
