// Task 11 `desktop_and_web_in_one_call`: a desktop `dilla_media::Call` (examples/call-peer.rs: the vendored webrtc-sys
// hook, dilla_core::sframe) and the web client's media stack (the packages/media harness page: the real
// DillaE2EEManager, media worker and dilla-core-wasm MediaSender / MediaReceiver) in one LiveKit room of dilla-testhost's
// in-process SFU. They hear each other's tones, then change epoch mid-call.
//
// The keys are a real call group's (examples/call-keys.rs): the desktop's from its ClientCore (`call_media_epoch`), the
// web side's from its own MLS state, for the join epoch and for the epoch after the web member's commit. The room's
// identities are the two devices, as dillad mints them.
import { expect, test, type Page } from '@playwright/test';
import { execFileSync, spawn, type ChildProcess } from 'node:child_process';
import { createWriteStream, mkdirSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { createInterface } from 'node:readline';
import { fileURLToPath } from 'node:url';
import { debugToken } from '../../../../e2e/media/support/lk.ts';
import type { DillaMediaStats } from '../../../../packages/media/src/protocol';

const here = dirname(fileURLToPath(import.meta.url));
const ROOT = resolve(here, '..', '..', '..', '..');
const EXAMPLES = join(ROOT, 'target', 'debug', 'examples');
const LOGS = join(ROOT, 'target', 'dilla-media-browser');
const CONTROL = 'http://127.0.0.1:8444';
const HARNESS = 'http://127.0.0.1:5179/';
const POLL = { timeout: 30_000, intervals: [250, 500, 1000] };

type Roster = { leaf: number; deviceId: string }[];
type Side = { epoch: number; baseKey: string; selfLeaf: number; roster: Roster };
type Keys = { group: string; desktopDevice: string; webDevice: string; epochs: { desktop: Side; web: Side }[] };
type RustStats = {
  media: { encrypted: Record<string, number>; opened: Record<string, number>; failed: Record<string, number>;
    verified: Record<string, number>; sif_trailer_len: number };
  levels: Record<string, number>; peaks: Record<string, number>;
};

const kid = (leaf: number, epoch: number) => ((leaf << 8) | (epoch & 0xff)).toString(16);
const sum = (r: Record<string, number>) => Object.values(r).reduce((a, b) => a + b, 0);
const leafOf = (s: Side, device: string) => s.roster.find((r) => r.deviceId === device)!.leaf;
const epochLine = (s: Side) => `epoch ${s.epoch} ${s.baseKey} ${s.selfLeaf} ${s.roster.map((r) => `${r.leaf}:${r.deviceId}`).join(',')}`;
const wire = (group: string, s: Side, minEpoch: number) =>
  ({ groupId: group, epoch: String(s.epoch), baseKey: s.baseKey, selfLeaf: s.selfLeaf, minEpoch: String(minEpoch), roster: s.roster });

/** The desktop participant, over its stdin/stdout line protocol. */
class Desktop {
  private readonly lines: Array<Record<string, unknown>> = [];
  private readonly waiters: Array<() => void> = [];
  private constructor(private readonly child: ChildProcess) {
    createInterface({ input: child.stdout! }).on('line', (l) => {
      try { this.lines.push(JSON.parse(l) as Record<string, unknown>); } catch { /* not ours */ }
      for (const w of this.waiters.splice(0)) w();
    });
  }

  static async start(room: string, identity: string, first: Side): Promise<Desktop> {
    const { url, token } = await debugToken(CONTROL, room, identity, true);
    mkdirSync(LOGS, { recursive: true });
    const child = spawn(join(EXAMPLES, 'call-peer'), ['--url', url, '--token', token, '--tone', '440'], {
      stdio: ['pipe', 'pipe', 'pipe'], env: { ...process.env, RUST_LOG: process.env.RUST_LOG ?? 'warn' },
    });
    child.stderr!.pipe(createWriteStream(join(LOGS, 'call-peer.stderr.log')));
    const d = new Desktop(child);
    d.send(epochLine(first));
    await d.next('connected', 30_000);
    return d;
  }

  async next(event: string, timeoutMs = 15_000, from = 0): Promise<Record<string, unknown>> {
    const deadline = Date.now() + timeoutMs;
    for (;;) {
      const hit = this.lines.slice(from).find((l) => l.event === event);
      if (hit !== undefined) return hit;
      const failed = this.lines.find((l) => l.event === 'failed');
      if (failed !== undefined) throw new Error(`call-peer failed: ${JSON.stringify(failed)}`);
      if (this.child.exitCode !== null) throw new Error(`call-peer exited (${this.child.exitCode}) before "${event}"`);
      const left = deadline - Date.now();
      if (left <= 0) throw new Error(`call-peer printed no "${event}" within ${timeoutMs} ms`);
      await new Promise<void>((r) => { this.waiters.push(r); setTimeout(r, Math.min(left, 500)); });
    }
  }

  send(cmd: string): void { this.child.stdin!.write(`${cmd}\n`); }

  async stats(): Promise<RustStats> {
    const from = this.lines.length;
    this.send('stats');
    return (await this.next('stats', 10_000, from)) as unknown as RustStats;
  }

  async close(): Promise<void> {
    if (this.child.exitCode !== null) return;
    this.send('quit');
    await new Promise<void>((r) => {
      const kill = setTimeout(() => { this.child.kill('SIGKILL'); r(); }, 10_000);
      this.child.once('exit', () => { clearTimeout(kill); r(); });
    });
  }
}

const pageStats = (page: Page): Promise<DillaMediaStats> => page.evaluate(() => (window as any).harness.mediaStats());

/** The page's decoded audio from `identity` through an AnalyserNode: the loudest bin over `ms`. */
async function pagePeak(page: Page, identity: string, ms: number): Promise<{ peakHz: number; peakDb: number; floorDb: number }> {
  return page.evaluate(async ([id, wait]) => {
    const room = (window as any).harness.session().room;
    const p = room.remoteParticipants.get(id);
    const pub = p === undefined ? undefined : [...p.trackPublications.values()].find((x: any) => x.kind === 'audio' && x.track !== undefined);
    if (pub === undefined) throw new Error(`no subscribed audio track from ${id}`);
    const ctx = new AudioContext();
    await ctx.resume();
    const analyser = ctx.createAnalyser();
    analyser.fftSize = 16384;
    analyser.smoothingTimeConstant = 0.8;
    ctx.createMediaStreamSource(new MediaStream([pub.track.mediaStreamTrack])).connect(analyser);
    await new Promise((r) => setTimeout(r, wait));
    const bins = new Float32Array(analyser.frequencyBinCount);
    analyser.getFloatFrequencyData(bins);
    await ctx.close();
    let best = 0;
    for (let i = 1; i < bins.length; i++) if (bins[i] > bins[best]) best = i;
    const sorted = [...bins].filter(Number.isFinite).sort((a, b) => a - b);
    return { peakHz: (best * ctx.sampleRate) / analyser.fftSize, peakDb: bins[best], floorDb: sorted[Math.floor(sorted.length / 2)] ?? -Infinity };
  }, [identity, ms] as const);
}

function record(name: string, data: unknown): void {
  mkdirSync(LOGS, { recursive: true });
  createWriteStream(join(LOGS, `${name}.json`)).end(JSON.stringify(data, null, 2));
  console.log(`CALL ${name} ${JSON.stringify(data)}`);
}

let keys: Keys;

test.beforeAll(() => {
  execFileSync('cargo', ['build', '--locked', '-p', 'dilla-media', '--example', 'call-peer', '--example', 'call-keys'], { cwd: ROOT, stdio: 'inherit' });
  keys = JSON.parse(execFileSync(join(EXAMPLES, 'call-keys'), { encoding: 'utf8' })) as Keys;
});

test('desktop_and_web_in_one_call', async ({ page }) => {
  const room = `${keys.group}-${Date.now().toString(36)}`;
  const [e1, e2] = keys.epochs;
  const { desktopDevice: dDev, webDevice: wDev } = keys;
  const dLeaf = leafOf(e1.web, dDev);
  const wLeaf = leafOf(e1.desktop, wDev);

  const desktop = await Desktop.start(room, dDev, e1.desktop);
  try {
    const { url, token } = await debugToken(CONTROL, room, wDev, true);
    await page.goto(HARNESS);
    await page.waitForFunction(() => 'harness' in window);
    await page.evaluate(
      ([u, t, e]) => (window as any).harness.connect(u, t, { e2ee: 'dilla', iceServers: [], dilla: { epoch: e, lock: { instanceId: 'dilla-media-call', callGroupId: e.groupId } } }),
      [url, token, wire(keys.group, e1.web, e1.web.epoch)] as const,
    );
    await page.evaluate(() => (window as any).harness.publish({ mic: true }));

    // Desktop to web: the page authenticates the desktop's frames as its device and hears 440 Hz.
    await expect.poll(async () => (await pageStats(page)).decrypted[kid(dLeaf, e1.web.epoch)] ?? 0, POLL).toBeGreaterThan(100);
    const peak1 = await pagePeak(page, dDev, 1500);
    // Web to desktop: the desktop authenticates the page's frames (its fake microphone is the media suite's 1 kHz tone).
    await expect.poll(async () => (await desktop.stats()).media.opened[kid(wLeaf, e1.desktop.epoch)] ?? 0, POLL).toBeGreaterThan(100);
    await expect.poll(async () => (await desktop.stats()).peaks[wDev] ?? 0, POLL).toBeGreaterThan(980);
    const before = { page: await pageStats(page), desktop: await desktop.stats() };
    record('desktop_and_web_epoch1', { peak1, page: { decrypted: before.page.decrypted, verified: before.page.verified, dropped: before.page.dropped }, desktop: before.desktop });
    expect(Math.abs(peak1.peakHz - 440)).toBeLessThan(10);
    expect(peak1.peakDb - peak1.floorDb).toBeGreaterThan(30);
    expect(before.page.verified[dDev] ?? 0).toBeGreaterThan(0);
    expect(sum(before.page.dropped as unknown as Record<string, number>)).toBe(0);
    expect(Math.abs((before.desktop.peaks[wDev] ?? 0) - 1000)).toBeLessThanOrEqual(20);
    expect(before.desktop.media.verified[wDev] ?? 0).toBeGreaterThan(0);
    expect(before.desktop.media.failed).toEqual({});
    expect(before.desktop.media.sif_trailer_len).toBeGreaterThanOrEqual(43);

    // The web member's commit: its client installs epoch 2 and its sender switches; the desktop's ClientCore applies
    // the commit and its new epoch reaches the call.
    await page.evaluate((k) => (window as any).harness.installEpoch(k), wire(keys.group, e2.web, e1.web.epoch));
    const from = 0;
    desktop.send(epochLine(e2.desktop));
    await desktop.next('epoch', 5_000, from);
    await expect.poll(async () => (await pageStats(page)).decrypted[kid(dLeaf, e2.web.epoch)] ?? 0, POLL).toBeGreaterThan(100);
    await expect.poll(async () => (await desktop.stats()).media.opened[kid(wLeaf, e2.desktop.epoch)] ?? 0, POLL).toBeGreaterThan(100);
    const peak2 = await pagePeak(page, dDev, 1500);
    const after = { page: await pageStats(page), desktop: await desktop.stats() };
    const pageDropped = sum(after.page.dropped as unknown as Record<string, number>) - sum(before.page.dropped as unknown as Record<string, number>);
    const desktopDropped = sum(after.desktop.media.failed) - sum(before.desktop.media.failed);
    record('desktop_and_web_epoch2', { peak2, pageDropped, desktopDropped, page: { decrypted: after.page.decrypted, dropped: after.page.dropped }, desktop: after.desktop });
    expect(Math.abs(peak2.peakHz - 440)).toBeLessThan(10);
    expect(Math.abs((after.desktop.peaks[wDev] ?? 0) - 1000)).toBeLessThanOrEqual(20);
    expect(pageDropped + desktopDropped).toBeLessThanOrEqual(10);
    // No frame accepted under a wrong key: only each sender's own epoch-1 and epoch-2 KIDs authenticated.
    expect(Object.keys(after.page.decrypted).sort()).toEqual([kid(dLeaf, 1), kid(dLeaf, 2)].sort());
    expect(Object.keys(after.desktop.media.opened).sort()).toEqual([kid(wLeaf, 1), kid(wLeaf, 2)].sort());
    expect((after.page.dropped as unknown as Record<string, number>).aeadFail ?? 0).toBe(0);
    expect(after.desktop.media.failed.E_SFRAME_AUTH ?? 0).toBe(0);
  } finally {
    await desktop.close();
  }
});
