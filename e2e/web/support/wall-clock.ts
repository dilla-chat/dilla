// Moves an e2e test host's fake clock (advanceClockWithWall) along with the wall clock.
//
// The 1 s ticker applies the whole wall seconds elapsed since the last second it applied, so a deliberate lead (a spec's
// POST /debug/clock) is kept. A Bot (re)start first aligns the fake clock to the wall second, since restoring a server
// snapshot rewinds it. That alignment also applies the seconds the ticker has not applied yet, so it restarts the
// ticker's baseline: before it did, the ticker applied those seconds a second time and the host ran about a second ahead
// after startBotRuntimeNow (the bot-runtime.spec.ts "device 2" failure: the interaction Welcome proof looked issued in
// the client's future). Ticks and alignments for one clock run one after another, so neither reads a stale baseline.

/** GET /debug/clock: the host's fake clock in whole seconds. */
export async function readFakeClock(clockURL: string, stage: string): Promise<number> {
  const response = await fetch(clockURL);
  if (!response.ok) throw new Error(`Bot host clock probe failed ${stage}: HTTP ${response.status}`);
  const now = (await response.json() as { now?: unknown }).now;
  if (typeof now !== 'number' || !Number.isSafeInteger(now)) throw new Error(`Bot host clock probe returned invalid time ${stage}`);
  return now;
}

async function advance(clockURL: string, seconds: number, stage: string): Promise<void> {
  const response = await fetch(clockURL, {
    method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ seconds }),
  });
  if (!response.ok) throw new Error(`Bot host clock catch-up failed ${stage}: HTTP ${response.status}`);
}

export class WallClockFollower {
  readonly clockURL: string;
  private readonly wallMs: () => number;
  /** Wall ms through which the fake clock has been advanced. */
  private advancedThrough: number;
  private chain: Promise<unknown> = Promise.resolve();

  constructor(clockURL: string, wallMs: () => number = Date.now) {
    this.clockURL = clockURL;
    this.wallMs = wallMs;
    this.advancedThrough = wallMs();
  }

  /** Applies the whole wall seconds elapsed since the last applied second; returns the seconds applied. */
  tick(): Promise<number> {
    return this.serial(async () => {
      const seconds = Math.floor((this.wallMs() - this.advancedThrough) / 1_000);
      if (seconds < 1) return 0;
      await advance(this.clockURL, seconds, 'on the wall-clock tick');
      this.advancedThrough += seconds * 1_000;
      return seconds;
    });
  }

  /** Advances the fake clock to the wall second when it is behind and restarts the tick baseline here, so the seconds
   *  this alignment covered are not applied again by the next tick. Returns the seconds applied. */
  align(stage: string): Promise<number> {
    return this.serial(async () => {
      const now = await readFakeClock(this.clockURL, stage);
      const wall = this.wallMs();
      const seconds = Math.floor(wall / 1_000) - now;
      if (seconds > 0) await advance(this.clockURL, seconds, stage);
      this.advancedThrough = wall;
      return Math.max(0, seconds);
    });
  }

  private serial<T>(run: () => Promise<T>): Promise<T> {
    const next = this.chain.then(run, run);
    this.chain = next.catch(() => undefined);
    return next;
  }
}

const followers = new Map<string, WallClockFollower>();

/** Starts following the wall clock for a host's clock URL, replacing the follower of an earlier host on the same port. */
export function followWallClock(clockURL: string): WallClockFollower {
  const follower = new WallClockFollower(clockURL);
  followers.set(clockURL, follower);
  return follower;
}

/** The follower that ticks this clock URL, or a fresh one when none does. */
export function wallClockFollower(clockURL: string): WallClockFollower {
  return followers.get(clockURL) ?? followWallClock(clockURL);
}
