import { test, expect, mediaWorkers } from '../voice/support/peer-browser';
import { VoicePeer, audible, framesPass, heard, heardCount, joinVoice, noiseOff, onboard, self, sentCount } from '../voice/support/voice';
import { desktopApplication, desktopE2E } from '../web/support/electron';
import { chromium, type Browser, type BrowserContext, type CDPSession, type Page } from '@playwright/test';
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { cpus, release } from 'node:os';
import { mkdir, readFile, writeFile } from 'node:fs/promises';
import { createHash } from 'node:crypto';
import { resolve } from 'node:path';
import { tmpdir } from 'node:os';
import { writeFixtures } from '../media/support/fixtures';
// @ts-expect-error measurement/release tooling is JavaScript
import { cpuPercent, parsePs } from '../../packages/desktop/scripts/performance-report.mjs';

const execute = promisify(execFile);
type ProcessSample = { pid: number; cpuSeconds: number; rssBytes: number };
async function snapshot(context: BrowserContext, cdp: CDPSession | null): Promise<ProcessSample[]> {
 const counters: { pid: number; cpuSeconds: number }[] = desktopE2E()
  ? await desktopApplication(context).evaluate(({ app }) => app.getAppMetrics().map(row => ({ pid: row.pid, cpuSeconds: row.cpu.cumulativeCPUUsage! })))
  : (await cdp!.send('SystemInfo.getProcessInfo')).processInfo.map((row: { id: number; cpuTime: number }) => ({ pid: row.id, cpuSeconds: row.cpuTime }));
 const pids = [...new Set(counters.map(row => row.pid))];
 if (pids.length < 2 || pids.some(pid => !Number.isSafeInteger(pid) || pid <= 0)) throw new Error('Missing browser process inventory');
 let rows: ProcessSample[];
 if (process.platform === 'win32') {
  const script = `@(Get-Process -Id ${pids.join(',')} -ErrorAction Stop | ForEach-Object { @{pid=$_.Id;cpuSeconds=$_.CPU;rssBytes=$_.WorkingSet64} }) | ConvertTo-Json -Compress`;
  const { stdout } = await execute('powershell.exe', ['-NoProfile', '-NonInteractive', '-Command', script], { timeout: 10000 });
  rows = JSON.parse(stdout);
 } else {
  const { stdout } = await execute('ps', ['-o', 'pid=,rss=,time=', '-p', pids.join(',')], { timeout: 10000 });
  rows = parsePs(stdout);
 }
 rows = rows.map(row => ({ ...row, cpuSeconds: counters.find(counter => counter.pid === row.pid)!.cpuSeconds }));
 if (rows.length !== pids.length || rows.some(row => !pids.includes(row.pid) || !Number.isFinite(row.cpuSeconds) || row.cpuSeconds < 0 || !Number.isFinite(row.rssBytes) || row.rssBytes <= 0)) throw new Error('Incomplete OS process measurement');
 return rows;
}
async function interval(context: BrowserContext, cdp: CDPSession | null, counter?: () => Promise<number>) {
 const discarded: { before: ProcessSample[]; after: ProcessSample[]; milliseconds: number }[] = [];
 for (let attempt = 0; attempt < 4; attempt++) {
  const before = await snapshot(context, cdp), frames = counter ? await counter() : null, start = performance.now();
  await new Promise(done => setTimeout(done, 5000));
  const after = await snapshot(context, cdp), received = counter ? await counter() : null, milliseconds = performance.now() - start;
  if (before.some(row => !after.some(next => next.pid === row.pid))) { discarded.push({ before, after, milliseconds }); continue; }
  return { cpu: cpuPercent(before, after, milliseconds), rss: after.reduce((sum, row) => sum + row.rssBytes, 0) / 1048576,
   rate: received === null || frames === null ? null : (received - frames) / (milliseconds / 1000), milliseconds, discarded };
 }
 throw new Error('No complete stable-process CPU window: ' + JSON.stringify(discarded));
}
async function searchPaint(page: Page) {
 const search = page.locator('.d-topbar__search');
 await expect(search).toBeVisible();
 await search.evaluate(element => {
  const root = window as unknown as { __dillaPaint: Promise<number> };
  root.__dillaPaint = new Promise<number>((done, reject) => {
   const timeout = setTimeout(() => { observer.disconnect(); reject(new Error('Search did not paint')); }, 10000);
   let start: number | null = null;
   element.addEventListener('pointerdown', () => { start = performance.now(); }, { once: true, capture: true });
   const observer = new MutationObserver(() => {
    if (start === null || !document.querySelector('dialog.d-search-palette-dialog[open]')) return;
    observer.disconnect();
    requestAnimationFrame(() => requestAnimationFrame(() => { clearTimeout(timeout); done(performance.now() - start!); }));
   });
   observer.observe(document.body, { childList: true, subtree: true });
  });
 });
 await search.click();
 const elapsed = await page.evaluate(() => (window as unknown as { __dillaPaint: Promise<number> }).__dillaPaint);
 await page.keyboard.press('Escape');
 return elapsed;
}

test('measure startup, resident memory, idle CPU, search paint and authenticated call audio', async ({ peerBrowser, launchOptions, baseURL }) => {
 const samples: Record<string, unknown>[] = [];
 let runtime = ''; 
 for (let iteration = 0; iteration < 3; iteration++) {
  const peer = peerBrowser.track(await VoicePeer.start());
  let own: Browser | null = null, listener: Browser | null = null, cdp: CDPSession | null = null;
  const start = performance.now();
  if (!desktopE2E()) own = peerBrowser.track(await chromium.launch({ ...launchOptions, headless: false, chromiumSandbox: true }));
  const alice = await peerBrowser.open('alice', own ? { browser: own } : undefined);
  if (!desktopE2E()) await alice.page.goto(baseURL!);
  await alice.page.waitForFunction(() => (document.getElementById('root')?.childElementCount ?? 0) > 0 && !document.querySelector('.d-splash__line'));
  const startup = performance.now() - start;
  runtime = await alice.page.evaluate(() => navigator.userAgent);
  try {
   await peer.setup(); await onboard(alice, peer, 'alice');
   if (own) cdp = await own.newBrowserCDPSession();
   const idle = await interval(alice.context, cdp);
   const interaction = await searchPaint(alice.page);
   // Distinct talkers: identical looping tones can be mistaken for acoustic
   // echo. Keep the shipping echo cancellation enabled, and give Bob 600 Hz.
   const fixture = writeFixtures(resolve(tmpdir(), 'dilla-performance-fixtures'), 600);
   const listenerOptions = { ...launchOptions, args: [...(launchOptions.args ?? []).filter(arg => !arg.startsWith('--use-file-for-fake-audio-capture=')), `--use-file-for-fake-audio-capture=${fixture.wav}`] };
   if (!desktopE2E()) listener = peerBrowser.track(await chromium.launch({ ...listenerOptions, headless: false, chromiumSandbox: true }));
   const bob = await peerBrowser.open('bob', listener ? { browser: listener } : { launchOptions: listenerOptions });
   try {
    await onboard(bob, peer, 'bob'); await peer.seat(2);
    await noiseOff(alice.page); await noiseOff(bob.page);
    await joinVoice(alice.page, peer.communityName, peer.voiceName);
    await joinVoice(bob.page, peer.communityName, peer.voiceName);
    const device = (await self(alice.page)).device;
    await heard(bob.page, device); await audible(bob.page);
    // Measure steady encrypted transport after real sender frames, so Chromium
    // can retire its temporary setup processes before the CPU window starts.
    await framesPass(alice.page, await sentCount(alice.page), 100);
    const call = await interval(alice.context, cdp, () => heardCount(bob.page, device));
    const callAudio = call.rate;
    const callRMS = await audible(bob.page);
    let passedThrough = 0;
    for (const person of [alice, bob]) for (const worker of mediaWorkers(person.page)) passedThrough += await worker.evaluate(() => {
     const stats = (globalThis as unknown as { __dillaMediaStats?: { passedThrough: number } }).__dillaMediaStats;
     if (!stats) throw new Error('No encrypted media counters'); return stats.passedThrough;
    });
    expect(passedThrough).toBe(0); expect(callAudio).toBeGreaterThan(0);
    samples.push({ startup, memory: idle.rss, idleCPU: idle.cpu, interaction, callAudio, callCPU: call.cpu, callRMS, passedThrough,
     idleWindowMs: idle.milliseconds, callWindowMs: call.milliseconds, discardedCPUWindows: { idle: idle.discarded, call: call.discarded } });
   } finally { await peerBrowser.close(bob); }
  } finally { await peerBrowser.close(alice); await peer.close(); await listener?.close(); await own?.close(); }
 }
 const mode = desktopE2E() ? 'desktop' : 'browser';
 const { stdout } = await execute('git', ['rev-parse', 'HEAD']);
 const target = `${process.platform === 'darwin' ? 'mac' : process.platform === 'win32' ? 'win' : 'linux'}-${process.arch}`;
 const { stdout: dirty } = await execute('git', ['status', '--porcelain', '--untracked-files=normal']);
 const manifestSHA = createHash('sha256').update(await readFile(resolve(desktopE2E() ? '../packages/desktop/dist/client/dilla-manifest.json' : '../packages/web/dist/dilla-manifest.json'))).digest('hex');
 const record = { format: 1, sourceDirty: dirty.trim().length > 0, sourceCommit: stdout.trim(), target, mode, samples, environment: { manifestSHA, runtime, headed: true, chromiumSandbox: true, os: release(), arch: process.arch, cpu: cpus()[0]?.model, cores: cpus().length, createdAt: new Date().toISOString() } };
 const directory = resolve(process.env.DILLA_PERFORMANCE_OUTPUT ?? '../packages/desktop/test-report');
 await mkdir(directory, { recursive: true });
 await writeFile(resolve(directory, `performance-${mode}-${target}.json`), JSON.stringify(record, null, 2) + '\n');
});
