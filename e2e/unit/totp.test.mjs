import assert from 'node:assert/strict';
import test from 'node:test';
import { TOTP_STEP, nextCode, totp } from '../web/support/totp.ts';

const SECRET = 'GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ';

/** A test host's control port: GET /debug/clock reads the fake clock, POST moves it forward. */
function fakeControl(start) {
  const clock = { now: start, advanced: 0 };
  const realFetch = globalThis.fetch;
  globalThis.fetch = async (_url, init) => {
    if (init?.method === 'POST') {
      const { seconds } = JSON.parse(init.body);
      clock.now += seconds;
      clock.advanced += seconds;
      return new Response(null, { status: 204 });
    }
    return new Response(JSON.stringify({ now: clock.now }), { status: 200 });
  };
  return { clock, restore: () => { globalThis.fetch = realFetch; } };
}

/** internal/auth.ValidateTOTP with skew 1: the lowest counter in now-1..now+1 above `after` whose code matches. */
function serverAccepts(code, unix, after) {
  const c = Math.floor(unix / TOTP_STEP);
  for (let n = c - 1; n <= c + 1; n++) if (n > after && totp(SECRET, n * TOTP_STEP) === code) return n;
  return null;
}

// The 2026-10-30 CI failures (bot-runtime, slash-commands, moderation-evidence): hosts started with advanceClockWithWall
// tick their fake clock with the wall clock, so the window can turn between drawing a code and the server checking it.
test('a drawn code stays good for a whole step after it is drawn, even when the window turns', async () => {
  for (const into of [0, 1, 15, 28, 29]) {
    const start = 1_800_000_000 - (1_800_000_000 % TOTP_STEP) + into;
    const { clock, restore } = fakeControl(start);
    try {
      const account = { totpSecret: SECRET, lastCounter: -1 };
      const code = await nextCode('http://control', account);
      for (const later of [0, 1, TOTP_STEP - into, TOTP_STEP - 1]) {
        assert.equal(serverAccepts(code, clock.now + later, -1), account.lastCounter,
          `drawn ${into} s into a step, checked ${later} s later`);
      }
    } finally { restore(); }
  }
});

test('codes drawn back to back never reuse a counter, and the clock moves only when the window is spent', async () => {
  const { clock, restore } = fakeControl(1_800_000_010);
  try {
    const account = { totpSecret: SECRET, lastCounter: -1 };
    const seen = [];
    for (let i = 0; i < 5; i++) {
      const code = await nextCode('http://control', account);
      const n = serverAccepts(code, clock.now, seen.at(-1) ?? -1);
      assert.equal(n, account.lastCounter);
      seen.push(n);
    }
    assert.deepEqual(seen.map((n, i) => (i === 0 ? 0 : n - seen[i - 1])), [0, 1, 1, 1, 1]);
    assert.equal(clock.advanced % TOTP_STEP, 0);
  } finally { restore(); }
});
