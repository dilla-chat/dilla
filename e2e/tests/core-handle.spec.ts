import { expect, test, type Page } from '@playwright/test';
import { readFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
// L-CI-02 (task 2): the same check the WASM gate runs on packages/core-wasm/pkg.
import { checkWasm } from '../../scripts/check-wasm-size.mjs';

// Task 7, L-WASM-03…05. The facade over the real encrypted OPFS store: signup, KeyPackages, a group,
// sending, pause/resume and a reload into a new worker. Only the two projects with a persistent OPFS
// store run it: private Firefox and ephemeral WebKit boot in memory by design.
const PROJECTS = ['chromium', 'firefox-persistent'];
const CI = process.env.CI === 'true';
/**
 * Every wait: 30 s locally, 90 s on the 4-vCPU runner (lesson c). States, never durations. The spike
 * config sets no `expect.timeout`, so a web-first assertion in this file (`expect(locator)…`,
 * `expect.poll`) always passes `{ timeout: WAIT }`; the waits below pass it to `waitForFunction` and
 * to the per-op race.
 */
const WAIT = CI ? 90_000 : 30_000;
const here = dirname(fileURLToPath(import.meta.url));
const SPIKE_WASM = resolve(here, '..', '..', 'core', 'dilla-core-wasm', 'spike', 'pkg', 'dilla_core_wasm_bg.wasm');

type CoreResult = { ok: true; value: Record<string, unknown> } | { ok: false; error: string };

const rep = (byteHex: string, n: number): string => byteHex.repeat(n);
/** UTF-8 hex of the two bodies and the username the worker uses. */
const HELLO_HEX = '68656c6c6f2066726f6d2074686520666163616465'; // "hello from the facade", 21 bytes
const QUEUED_HEX = '7374696c6c20717565756564'; // "still queued", 12 bytes
const FACADE_HEX = '666163616465'; // "facade", 6 bytes

function instance(name: string): string {
  return `${name}-${Date.now().toString(36)}`;
}

async function waitForCore(page: Page): Promise<void> {
  await page.waitForFunction(() => typeof (globalThis as any).__dilla?.core === 'function', null, {
    timeout: WAIT,
  });
}

async function openCorePage(page: Page, id: string): Promise<void> {
  await page.goto(`/?core=1&instance=${id}`);
  await waitForCore(page);
}

/** One worker op; rejects when the worker has not answered within WAIT. */
async function core(page: Page, op: string): Promise<CoreResult> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  const timeout = new Promise<never>((_, reject) => {
    timer = setTimeout(() => reject(new Error(`core op ${op} did not answer within ${WAIT} ms`)), WAIT);
  });
  try {
    return await Promise.race([
      page.evaluate((o) => (globalThis as any).__dilla.core(o) as Promise<CoreResult>, op),
      timeout,
    ]);
  } finally {
    clearTimeout(timer);
  }
}

async function ok(page: Page, op: string): Promise<Record<string, unknown>> {
  const result = await core(page, op);
  // On failure the diff shows the worker's error message, e.g. E_STORE_BUSY under the task-7 mutation.
  expect(result, `core op ${op}`).toEqual({ ok: true, value: expect.anything() });
  return (result as { ok: true; value: Record<string, unknown> }).value;
}

test.beforeEach(() => {
  test.skip(!PROJECTS.includes(test.info().project.name), 'needs a persistent OPFS store');
  test.setTimeout(CI ? 600_000 : 240_000);
});

test('the facade keeps one identity, group and outbox across pause, resume and a reload', async ({ page }) => {
  await openCorePage(page, instance('core-life'));
  expect(await ok(page, 'open')).toMatchObject({ abi: 15, phase: 0 });

  const signup = await ok(page, 'signup');
  expect(signup.recoveryKey).toMatch(/^[0-9A-HJKMNP-TV-Z]{52}$/); // 52 Crockford base32, ungrouped
  expect(signup.requestHead).toBe('88'); // the 8-element POST /v1/accounts body
  expect(signup.completeHead).toBe('8401'); // [1, blob, ssk_signature, prev_hash]
  expect(signup.keyPackagesHead).toBe('8282'); // [[kp, kp], last_resort]
  expect(String(signup.beginAgain)).toMatch(/^E_CORE_STATE/);

  const group = await ok(page, 'group');
  expect(group.createHead).toBe(`8450${rep('33', 16)}`); // [group_id, binding, group_info, ratchet_tree]
  expect(group.confirm).toBe(`8250${rep('33', 16)}01`); // [group_id, seq 1]
  expect(group.encryptHead).toBe(`8250${rep('33', 16)}`); // [group_id, message_body]

  const before = await ok(page, 'snapshot');
  const identity = String(before.identity);
  // [phase 2, instance 0x77…, user 0x66…, device, "facade", list_published 1]
  expect(identity).toMatch(new RegExp(`^860250${rep('77', 16)}50${rep('66', 16)}50[0-9a-f]{32}66${FACADE_HEX}01$`));
  const device = identity.slice(74, 106);
  // [[group 0x33…, kind 0, community 0x44…, target = channel 0x55…, state 2 (active), epoch 0, …]]
  expect(String(before.groups)).toMatch(new RegExp(`^818950${rep('33', 16)}0050${rep('44', 16)}50${rep('55', 16)}0200`));
  // [[seq 1, epoch 0, recv_ts 1760000000, status 0, "", user 0x66…, this device, kind 0 (user),
  //   tier 1 (browser), msg_id, type 0, "hello from the facade", edited_seq 0, reply null,
  //   [] reactions, not pinned, [] attachments, no mention, thread summary null (web3.7: 19 elements)]]
  expect(String(before.timeline)).toMatch(
    new RegExp(`^819301001a68e778000060${'50' + rep('66', 16)}50${device}000150[0-9a-f]{32}0075${HELLO_HEX}00f680008000f6$`),
  );
  // [[msg_id, state 1 (in flight), "", created 1760000000, "still queued", type 0, reply_to null, attachments []]]
  expect(String(before.outbox)).toMatch(new RegExp(`^818850[0-9a-f]{32}01601a68e778006c${QUEUED_HEX}00f680$`));
  expect(String(before.deviceList)).toMatch(/^8401/);
  expect(String(before.sealed)).toMatch(/^835867/); // three elements; root object: exactly 103 bytes
  // [nonce 0x22…, purpose 0, sig (64 bytes), null, null]: 104 bytes
  expect(String(before.session)).toMatch(new RegExp(`^855820${rep('22', 32)}005840[0-9a-f]{128}f6f6$`));

  expect(await ok(page, 'pause')).toEqual({});
  expect(await ok(page, 'pause')).toEqual({}); // a second pause is a no-op
  expect(await core(page, 'identity')).toEqual({ ok: false, error: 'E_STORE_PAUSED: the store is paused' });
  expect(await ok(page, 'resume')).toEqual({});
  expect(await ok(page, 'resume')).toEqual({}); // resume while open is a no-op
  expect(await ok(page, 'snapshot')).toEqual(before);

  await page.reload();
  await waitForCore(page);
  expect(await ok(page, 'open')).toMatchObject({ abi: 15, phase: 2 });
  // Byte-identical after a new worker reopened the store: the app tables, the MLS state and, through
  // the deterministic Ed25519 session signature, the DSK in OpenMLS's signature-key table (C6).
  expect(await ok(page, 'snapshot')).toEqual(before);
  // And the reopened core still writes: the in-flight row goes back to queued and is encrypted again.
  expect((await ok(page, 'resend-queued')).encryptHead).toBe(`8250${rep('33', 16)}`);
});

test('the facade reports the core error codes and names a wrong-length argument', async ({ page }) => {
  await openCorePage(page, instance('core-errors'));
  expect(await ok(page, 'open')).toMatchObject({ abi: 15, phase: 0 });
  expect(await ok(page, 'errors')).toEqual({
    keyPackagesBeforeIdentity: 'E_CORE_NO_IDENTITY',
    sessionBeforeIdentity: 'E_CORE_NO_IDENTITY',
    shortInstance: 'E_CORE_INPUT: instance_id: expected 16 bytes, got 15',
    phaseAfterBegin: 1,
    beginTwice: expect.stringMatching(/^E_CORE_STATE/),
    shortNonce: 'E_CORE_INPUT: nonce: expected 32 bytes, got 31',
    // L-WASM-04: 256 crosses the generated glue as a u32 and is refused before narrowing.
    purposeOutOfRange: 'E_CORE_INPUT: purpose: 256 is not 0 or 1',
    shortGroup: 'E_CORE_INPUT: group_id: expected 16 bytes, got 15',
    phaseAfterReset: 0,
  });
});

test('the spike wasm links the facade and stays inside the size budget', () => {
  test.skip(test.info().project.name !== 'chromium', 'a property of the build: checked once');
  const bytes = new Uint8Array(readFileSync(SPIKE_WASM));
  expect(checkWasm(bytes)).toEqual([]);
  const names = WebAssembly.Module.exports(new WebAssembly.Module(bytes)).map((e) => e.name);
  for (const name of [
    'core_open',
    'corehandle_pause',
    'corehandle_resume',
    'corehandle_session_sign',
    'corehandle_group_apply',
    'corehandle_message_deleted',
    'corehandle_timeline',
    'corehandle_send_prepare',
    'corehandle_pins',
    'corehandle_attachment_get',
    'corehandle_purges',
    'corehandle_purge_done',
    'corehandle_own_roles_set',
  ]) {
    expect(names).toContain(name);
  }
});
