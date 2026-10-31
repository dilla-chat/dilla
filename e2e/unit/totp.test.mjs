import test from 'node:test';
import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { nextCode, totp, TOTP_STEP } from '../web/support/totp.ts';

const secret = 'JBSWY3DPEHPK3PXP';

async function withClock(now, run) {
  const advances = [];
  const server = createServer(async (req, res) => {
    assert.equal(req.url, '/debug/clock');
    if (req.method === 'POST') {
      let body = '';
      for await (const chunk of req) body += chunk;
      const { seconds } = JSON.parse(body);
      advances.push(seconds);
      now = (typeof now === 'function' ? now() : now) + seconds;
      res.writeHead(204).end();
    } else {
      res.writeHead(200, { 'content-type': 'application/json' }).end(JSON.stringify({ now: typeof now === 'function' ? now() : now }));
    }
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  try {
    await run(`http://127.0.0.1:${server.address().port}`, advances);
  } finally {
    await new Promise((resolve, reject) => server.close(error => error ? reject(error) : resolve()));
  }
}

test('a fresh TOTP survives crossing the next 30-second boundary before auto-submit', async () => {
  await withClock(10 * TOTP_STEP - 0.01, async (control, advances) => {
    const account = { totpSecret: secret, lastCounter: -1 };
    const code = await nextCode(control, account);
    const serverCounterAtSubmit = 10;
    assert.ok(account.lastCounter >= serverCounterAtSubmit - 1, 'code must remain inside the server skew window after the tick');
    assert.equal(code, totp(secret, 9 * TOTP_STEP));
    assert.equal(account.lastCounter, 9);
    assert.deepEqual(advances, [], 'a fresh draw must not skew the host away from the Bot clock');
  });
});

test('the next unspent counter is used without advancing the host while it remains in the acceptance window', async () => {
  await withClock(10 * TOTP_STEP, async (control, advances) => {
    const account = { totpSecret: secret, lastCounter: 10 };
    assert.equal(await nextCode(control, account), totp(secret, 11 * TOTP_STEP));
    assert.equal(account.lastCounter, 11);
    assert.deepEqual(advances, []);
  });
});

test('a spent future counter advances only the minimum one host step needed', async () => {
  await withClock(10 * TOTP_STEP, async (control, advances) => {
    const account = { totpSecret: secret, lastCounter: 11 };
    assert.equal(await nextCode(control, account), totp(secret, 12 * TOTP_STEP));
    assert.equal(account.lastCounter, 12);
    assert.deepEqual(advances, [TOTP_STEP]);
  });
});


test('wall-clock TOTP waits for the natural next counter without advancing host time or forcing retention', async () => {
  let reads = 0;
  await withClock(() => reads++ === 0 ? 300 : 330, async (control, advances) => {
    const account = { totpSecret: secret, lastCounter: 11 };
    const sleeps = [];
    assert.equal(await nextCode(control, account, { wallClock: true, sleep: async ms => { sleeps.push(ms); } }), totp(secret, 12 * TOTP_STEP));
    assert.equal(account.lastCounter, 12);
    assert.deepEqual(advances, [], 'wall-clock mode must never POST a synthetic clock advance');
    assert.deepEqual(sleeps, [1000], 'poll the naturally ticking host instead');
  });
});

test('wall-clock TOTP stops after one 30-second window if the host ticker is stalled', async t => {
  let elapsed = 0;
  t.mock.method(Date, 'now', () => elapsed);
  await withClock(300, async (control, advances) => {
    const account = { totpSecret: secret, lastCounter: 11 };
    await assert.rejects(nextCode(control, account, {
      wallClock: true,
      sleep: async ms => { elapsed += ms; },
    }), /within 30 seconds/);
    assert.equal(elapsed, 30_000);
    assert.equal(account.lastCounter, 11, 'an unavailable code must not burn another counter');
    assert.deepEqual(advances, []);
  });
});
