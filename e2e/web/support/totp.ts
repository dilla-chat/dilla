// Second-factor support for the web e2e (web3.6): RFC 6238 codes computed for the test host's clock, and the counter
// rule the server applies. A code is burnt once used, so each draw takes a counter the server has not seen.
import { createHmac } from 'node:crypto';
import { expect, type Page } from '@playwright/test';

export const TOTP_STEP = 30;
const B32 = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567';

export function base32Decode(text: string): Buffer {
  const clean = text.replace(/[\s=-]/g, '').toUpperCase();
  let bits = 0;
  let value = 0;
  const out: number[] = [];
  for (const ch of clean) {
    const i = B32.indexOf(ch);
    if (i < 0) throw new Error(`not base32: ${ch}`);
    value = (value << 5) | i;
    bits += 5;
    if (bits >= 8) { out.push((value >>> (bits - 8)) & 0xff); bits -= 8; }
  }
  return Buffer.from(out);
}

/** RFC 6238 over HMAC-SHA1: the code for the 30 s window that holds `unix`. */
export function totp(secretB32: string, unix: number, digits = 6): string {
  const counter = Math.floor(unix / TOTP_STEP);
  const msg = Buffer.alloc(8);
  msg.writeBigUInt64BE(BigInt(counter));
  const mac = createHmac('sha1', base32Decode(secretB32)).update(msg).digest();
  const off = mac[mac.length - 1]! & 0x0f;
  const bin = ((mac[off]! & 0x7f) << 24) | (mac[off + 1]! << 16) | (mac[off + 2]! << 8) | mac[off + 3]!;
  return (bin % 10 ** digits).toString().padStart(digits, '0');
}

/** The test host's fake clock, in Unix seconds (`GET /debug/clock`). */
export async function hostNow(control: string): Promise<number> {
  const res = await fetch(`${control}/debug/clock`);
  const text = await res.text();
  expect(res.status, text).toBe(200);
  return (JSON.parse(text) as { now: number }).now;
}

/** Moves the host's fake clock forward (`POST /debug/clock`). */
export async function advanceClock(control: string, seconds: number): Promise<void> {
  const res = await fetch(`${control}/debug/clock`, {
    method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ seconds }),
  });
  expect(res.status, await res.text()).toBe(204);
}

/**
 * The next code the server will accept. With the host clock at counter c the server takes c-1..c+1 and refuses a
 * counter at or below the last one used, so the pick is max(c, last+1). While that is past c+1 the host clock moves
 * 30 s at a time. Records the counter in `account.lastCounter`.
 *
 * Never c-1: a host started with advanceClockWithWall ticks its clock with the wall clock, and c-1 falls out of the
 * window the moment the clock reaches the next step, which can be between this read and the server's check (the
 * 2026-10-30 CI failures: "That code did not match"). A pick of c or later stays good for at least one whole step.
 */
export async function nextCode(control: string, account: { totpSecret: string | null; lastCounter: number }): Promise<string> {
  if (account.totpSecret === null) throw new Error('this account has no authenticator secret');
  for (;;) {
    const c = Math.floor((await hostNow(control)) / TOTP_STEP);
    const n = Math.max(c, account.lastCounter + 1);
    if (n <= c + 1) {
      account.lastCounter = n;
      return totp(account.totpSecret, n * TOTP_STEP);
    }
    await advanceClock(control, TOTP_STEP);
  }
}

type Call = (c: unknown) => Promise<{ ok: boolean; value?: unknown; error?: unknown }>;

/**
 * Worker-harness sign-ups: after a `signupSubmit` that answered `factorOwed`, enrol an authenticator app through the
 * worker (factorTotpBegin, factorTotpConfirm) and finish with factorSetupDone. Returns what factorSetupDone answered
 * and the account's secret and codes.
 */
export async function completeFactor(page: Page, control: string, invite: string | null = null): Promise<{
  result: unknown; totpSecret: string; backupCodes: string[]; lastCounter: number;
}> {
  const call = async (command: unknown): Promise<unknown> => {
    const r = await page.evaluate((c) => (window as unknown as { dilla: { call: Call } }).dilla.call(c), command);
    expect(r, `${(command as { m: string }).m}: ${JSON.stringify(r)}`).toMatchObject({ ok: true });
    return r.value;
  };
  const begun = await call({ m: 'factorTotpBegin', stepup: null }) as { secret: string };
  const acct = { totpSecret: begun.secret, lastCounter: -1 };
  const code = await nextCode(control, acct);
  const confirmed = await call({ m: 'factorTotpConfirm', code }) as { backupCodes: string[] };
  const result = await call({ m: 'factorSetupDone', invite });
  return { result, totpSecret: begun.secret, backupCodes: confirmed.backupCodes, lastCounter: acct.lastCounter };
}
