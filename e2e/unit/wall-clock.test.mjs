import assert from 'node:assert/strict';
import test from 'node:test';
import { WallClockFollower } from '../web/support/wall-clock.ts';

/** A test host's control port (GET /debug/clock reads the fake clock, POST moves it forward) and a wall clock the test
 *  sets. Every response settles a macrotask later, so overlapping calls interleave as they would over HTTP. */
function fakeHost(fakeStartS, wallStartMs) {
  const state = { fake: fakeStartS, wall: wallStartMs };
  const realFetch = globalThis.fetch;
  globalThis.fetch = async (_url, init) => {
    await new Promise(resolve => setTimeout(resolve, 0));
    if (init?.method === 'POST') {
      state.fake += JSON.parse(init.body).seconds;
      return new Response(null, { status: 204 });
    }
    return new Response(JSON.stringify({ now: state.fake }), { status: 200 });
  };
  return { state, wallMs: () => state.wall, restore: () => { globalThis.fetch = realFetch; } };
}

const T = 1_800_000_000;

// The bot-runtime.spec.ts "device 2" failure: a Bot restart's alignment applied the seconds the ticker had not applied
// yet, the ticker applied them again, and the host clock ran a second ahead of the wall clock.
test('a Bot start alignment followed by ticks never puts the fake clock ahead of the wall second', async () => {
  const { state, wallMs, restore } = fakeHost(T, T * 1_000 + 600);
  try {
    const follower = new WallClockFollower('http://control/debug/clock', wallMs);
    const steps = [
      ['tick', 2_900], ['align', 3_500], ['tick', 3_600], ['tick', 4_400], ['tick', 4_600],
      ['align', 7_950], ['tick', 8_100], ['tick', 9_050], ['tick', 12_700],
    ];
    for (const [kind, at] of steps) {
      state.wall = T * 1_000 + at;
      if (kind === 'tick') await follower.tick();
      else await follower.align('in the test');
      const wallSecond = Math.floor(state.wall / 1_000);
      assert.ok(state.fake <= wallSecond, `${kind} at +${at} ms: fake ${state.fake - T} s is ahead of wall ${wallSecond - T} s`);
      assert.ok(wallSecond - state.fake <= 1, `${kind} at +${at} ms: fake ${state.fake - T} s lags wall ${wallSecond - T} s`);
    }
  } finally { restore(); }
});

test('an alignment and a tick that overlap apply the elapsed seconds once', async () => {
  const { state, wallMs, restore } = fakeHost(T, T * 1_000 + 300);
  try {
    const follower = new WallClockFollower('http://control/debug/clock', wallMs);
    state.wall = T * 1_000 + 5_400;
    await Promise.all([follower.align('in the test'), follower.tick(), follower.tick()]);
    assert.equal(state.fake, T + 5);
  } finally { restore(); }
});

test('an alignment catches up a rewound clock and keeps a deliberate lead', async () => {
  const { state, wallMs, restore } = fakeHost(T, T * 1_000 + 100);
  try {
    const follower = new WallClockFollower('http://control/debug/clock', wallMs);
    state.fake = T - 40; // a restored snapshot rewound the host clock
    state.wall = T * 1_000 + 2_200;
    assert.equal(await follower.align('in the test'), 42);
    assert.equal(state.fake, T + 2);
    state.fake += 2; // a spec puts the instance 2 s ahead on purpose
    state.wall = T * 1_000 + 3_300;
    assert.equal(await follower.align('in the test'), 0);
    state.wall = T * 1_000 + 4_400;
    assert.equal(await follower.tick(), 1);
    assert.equal(state.fake, T + 5);
  } finally { restore(); }
});
