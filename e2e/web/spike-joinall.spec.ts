import type { Page } from '@playwright/test';
import type { HarnessApi, HarnessResult, SpikeRecord } from '../../packages/client-core/harness/main';
import type { AccountState, Command } from '../../packages/client-core/src/index';
import { WebDriver, instanceInvite, testHostUrl, testkitEnv } from './support/driver';
import { expect, test } from './support/persistent';
import { completeFactor } from './support/totp';
import { e2ePort } from './support/ports';

// SP-W201 (plan web-2a task 13): a measurement, opt-in, never a CI gate (architect ruling 21).
const SPIKE = process.env.DILLA_SPIKE === '1';
const CI = process.env.CI === 'true';
const GROUPS = 200;
const FIRST = 50;
const OPEN_AFTER = 10;
const WAIT = 30_000;
const PASS_WAIT = 1_200_000;           // a ceiling for the poll, not a budget: the ruling threshold is RULING_MS
const RULING_MS = 240_000;

type HarnessWindow = Window & { dilla: HarnessApi };

async function call(page: Page, command: Command): Promise<HarnessResult> {
  return page.evaluate((c) => (window as unknown as HarnessWindow).dilla.call(c), command);
}
async function ok(page: Page, command: Command): Promise<unknown> {
  const result = await call(page, command);
  expect(result, `${command.m}: ${JSON.stringify(result)}`).toMatchObject({ ok: true });
  return result.ok ? result.value : undefined;
}
async function phaseOf(page: Page): Promise<string | null> {
  const account = await page.evaluate(() => (window as unknown as HarnessWindow).dilla.get('account')) as AccountState | null;
  return account?.phase ?? null;
}
async function openHarness(page: Page): Promise<void> {
  await page.goto('/');
  await expect(page.locator('#status')).toHaveText('ready', { timeout: WAIT });
  await ok(page, { m: 'start' });
}
async function active(page: Page): Promise<number> {
  const log = await page.evaluate(() => (window as unknown as HarnessWindow).dilla.spike.activeLog());
  return log.at(-1)?.active ?? 0;
}
function events(page: Page): Promise<SpikeRecord[]> {
  return page.evaluate(() => (window as unknown as HarnessWindow).dilla.spike.events());
}

test.skip(!SPIKE || CI, 'SP-W201 is a local measurement: set DILLA_SPIKE=1');

test('SP-W201: join-all and the load-free core at 200 groups', async ({ page, relaunch, browserName }, testInfo) => {
  test.setTimeout(3_600_000);
  const pageErrors: string[] = [];
  page.on('pageerror', (e) => pageErrors.push(e.message));
  const peer = await WebDriver.start(testHostUrl(), testkitEnv());
  try {
    const setupStarted = Date.now();
    const s = await peer.setup({ community: 'spike community', channel: 'c0', channels: GROUPS });
    const setupMs = Date.now() - setupStarted;
    expect(s.channel_ids).toHaveLength(GROUPS);
    expect(s.channel_ids[0]).toBe(s.channel_id);
    const username = `spk${(peer.seed & 0xff_ffff).toString(16).padStart(6, '0')}`;

    await openHarness(page);
    await expect.poll(() => phaseOf(page), { timeout: WAIT }).toBe('needs-signup');
    await ok(page, { m: 'signupBegin' });
    const keys = (await page.evaluate(() => (window as unknown as HarnessWindow).dilla.get('account'))) as AccountState;
    await ok(page, {
      m: 'signupSubmit', invite: instanceInvite(), username, display: 'Spike',
      password: keys.instance!.passwordSignup ? 'spike password 1' : null, recoveryKeyAcknowledged: true,
    });
    await completeFactor(page, process.env.DILLA_TESTKIT_CONTROL ?? `http://127.0.0.1:${String(e2ePort(8454))}`);
    await expect.poll(() => phaseOf(page), { timeout: WAIT }).toBe('ready');

    await page.evaluate((c) => { (window as unknown as HarnessWindow).dilla.spike.watchActive(c); }, s.community_id);
    const t0 = Date.now();
    expect(await ok(page, { m: 'joinCommunity', invite: s.invite_code })).toEqual({ communityId: s.community_id });
    await ok(page, { m: 'selectCommunity', communityId: s.community_id });

    await expect.poll(() => active(page), { timeout: PASS_WAIT, intervals: [1_000] }).toBeGreaterThanOrEqual(OPEN_AFTER);
    const last = s.channel_ids[GROUPS - 1]!;
    const openStarted = Date.now();
    await ok(page, { m: 'openChannel', channelId: last });
    const openMs = Date.now() - openStarted;
    const activeAtOpen = await active(page);
    // The mechanism of L-TS-22 rule 4: the open of a group the pass has not reached ran on its own lane.
    expect(activeAtOpen, 'the open of the last channel resolved before the pass finished').toBeLessThan(GROUPS);

    await expect.poll(() => active(page), { timeout: PASS_WAIT, intervals: [1_000] }).toBe(GROUPS);
    const log = await page.evaluate(() => (window as unknown as HarnessWindow).dilla.spike.activeLog());
    const firstMs = log.find((r) => r.active >= FIRST)!.at - t0;
    const allMs = log.find((r) => r.active >= GROUPS)!.at - t0;
    const joinAll = (await events(page)).filter((e) => e.kind === 'join-all').at(-1);
    expect(joinAll, 'the engine reported join-all progress').toBeDefined();
    if (joinAll?.kind !== 'join-all') throw new Error('unreachable');
    expect(joinAll.total).toBe(GROUPS);

    const groupsCall = await page.evaluate(() => (window as unknown as HarnessWindow).dilla.spike.timeGroups());
    expect(groupsCall.ok).toBe(true);
    expect(groupsCall.rows).toBe(GROUPS);
    // Record equal route counts across 21 groups() calls. This says nothing about MLS state loads;
    // task 1's native groups() test owns the single-store-query, load-free mechanism.
    // medianMs and maxMs are recorded below and never bounded (lesson c).
    expect(groupsCall.routeCalls.after, 'route counts recorded across groups() calls').toBe(groupsCall.routeCalls.before);

    const { page: again } = await relaunch();
    again.on('pageerror', (e) => pageErrors.push(e.message));
    await openHarness(again);
    await expect.poll(() => phaseOf(again), { timeout: WAIT }).toBe('ready');
    await expect.poll(async () => (await events(again)).some((e) => e.kind === 'sweep' && e.groups > 0), { timeout: PASS_WAIT, intervals: [1_000] }).toBe(true);
    const sweep = (await events(again)).find((e) => e.kind === 'sweep' && e.groups > 0);
    if (sweep?.kind !== 'sweep') throw new Error('unreachable');
    expect(sweep.groups, 'the relaunch sweep read every state-2 group').toBe(GROUPS);

    const result = {
      browser: browserName, groups: GROUPS, setupMs, firstMs, allMs, openMs, activeAtOpen,
      joinAll: { done: joinAll.done, total: joinAll.total, failed: joinAll.failed },
      groupsCall: { rows: groupsCall.rows, medianMs: groupsCall.medianMs, maxMs: groupsCall.maxMs, routeCalls: groupsCall.routeCalls },
      sweep: { groups: sweep.groups, ms: sweep.ms },
    };
    await testInfo.attach('spike-joinall.json', { body: JSON.stringify(result, null, 2), contentType: 'application/json' });
    console.log(`SP-W201 ${JSON.stringify(result)}`);
    if (allMs > RULING_MS) {
      testInfo.annotations.push({ type: 'ruling-request', description:
        `join-all at 200 groups took ${allMs} ms (> 4 min) on ${browserName}: revisit SYNC.joinAllConcurrency or the pacing before task 22 budgets` });
    }
    expect(pageErrors).toEqual([]);
  } finally {
    await peer.close();
  }
});
