// Task 2 proof: a Rust LiveKit participant (../src/bin/voice-proof.rs, the dilla hook in the vendored webrtc-sys)
// and the packages/media harness page (the real DillaE2EEManager, media worker and dilla-core-wasm MediaSender /
// MediaReceiver) exchange dilla-sframe/1 frames through dilla-testhost's in-process LiveKit.
//
// The keys are the brief's: epoch 1 with base key [7u8; 16], roster [(0, [1u8; 16]), (1, [2u8; 16])]; the Rust
// participant is leaf 0 (identity 0101…01), the page is leaf 1 (identity 0202…02); epoch 2 has base key [9u8; 16].
import { expect, test, type Page } from '@playwright/test';
import { execFileSync, spawn, type ChildProcess } from 'node:child_process';
import { createWriteStream, mkdirSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { createInterface } from 'node:readline';
import { fileURLToPath } from 'node:url';
import { debugToken } from '../../../e2e/media/support/lk.ts';
import type { DillaMediaStats } from '../../../packages/media/src/protocol';

const here = dirname(fileURLToPath(import.meta.url));
const CRATE = resolve(here, '..');
const BIN = process.env.VOICE_PROOF_BIN ?? join(CRATE, 'target', 'debug', 'voice-proof');
const LOGS = join(CRATE, 'target', 'proof-logs');
const CONTROL = 'http://127.0.0.1:8444';
const HARNESS = 'http://127.0.0.1:5179/';

const RUST_DEV = '01'.repeat(16);
const PAGE_DEV = '02'.repeat(16);
const KEY1 = '07'.repeat(16);
const KEY2 = '09'.repeat(16);
const GROUP = 'c3'.repeat(16);
const ROSTER = [{ leaf: 0, deviceId: RUST_DEV }, { leaf: 1, deviceId: PAGE_DEV }];
const ROSTER_ARG = ROSTER.map((r) => `${r.leaf}:${r.deviceId}`).join(',');
// The browser's KID keys (kidHex): leaf 0 is the Rust sender, leaf 1 the page.
const RUST_KID = (epoch: number) => (epoch & 0xff).toString(16);
const PAGE_KID = (epoch: number) => ((1 << 8) | (epoch & 0xff)).toString(16);

const epochWire = (epoch: number, baseKey: string) =>
  ({ groupId: GROUP, epoch: String(epoch), baseKey, selfLeaf: 1, minEpoch: '1', roster: ROSTER });

type Stats = {
  media: { encrypted: Record<string, number>; opened: Record<string, number>; failed: Record<string, number>;
    send_latency: Latency; recv_latency: Latency };
  audio: { frames: number; peak_hz: number; rms: number; peaks: number[] };
  video: { frames: number; width: number; height: number };
};
type Latency = { frames: number; p50_us: number; p99_us: number; max_us: number };
const sum = (r: Record<string, number>) => Object.values(r).reduce((a, b) => a + b, 0);

/** The Rust participant, over its stdin/stdout line protocol. */
class Agent {
  private readonly lines: Array<Record<string, unknown>> = [];
  private readonly waiters: Array<() => void> = [];
  private constructor(private readonly child: ChildProcess) {
    createInterface({ input: child.stdout! }).on('line', (l) => {
      try { this.lines.push(JSON.parse(l) as Record<string, unknown>); } catch { /* not ours */ }
      for (const w of this.waiters.splice(0)) w();
    });
  }

  static async start(name: string, room: string, extra: string[]): Promise<Agent> {
    const { url, token } = await debugToken(CONTROL, room, RUST_DEV, true);
    mkdirSync(LOGS, { recursive: true });
    const child = spawn(BIN, ['--url', url, '--token', token, '--leaf', '0', ...extra], {
      stdio: ['pipe', 'pipe', 'pipe'], env: { ...process.env, RUST_LOG: process.env.RUST_LOG ?? 'warn' },
    });
    child.stderr!.pipe(createWriteStream(join(LOGS, `${name}.stderr.log`)));
    const agent = new Agent(child);
    await agent.next('connected', 30_000);
    return agent;
  }

  /** The first line with this event at or after `from` (default: any so far). */
  async next(event: string, timeoutMs = 15_000, from = 0): Promise<Record<string, unknown>> {
    const deadline = Date.now() + timeoutMs;
    for (;;) {
      const hit = this.lines.slice(from).find((l) => l.event === event);
      if (hit !== undefined) return hit;
      if (this.child.exitCode !== null) throw new Error(`voice-proof exited (${this.child.exitCode}) before "${event}"`);
      const left = deadline - Date.now();
      if (left <= 0) throw new Error(`voice-proof printed no "${event}" within ${timeoutMs} ms`);
      await new Promise<void>((r) => { this.waiters.push(r); setTimeout(r, Math.min(left, 500)); });
    }
  }

  send(cmd: string): void { this.child.stdin!.write(`${cmd}\n`); }

  async stats(): Promise<Stats> {
    const from = this.lines.length;
    this.send('stats');
    return (await this.next('stats', 10_000, from)) as unknown as Stats;
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

async function joinPage(page: Page, room: string): Promise<void> {
  const { url, token } = await debugToken(CONTROL, room, PAGE_DEV, true);
  await page.goto(HARNESS);
  await page.waitForFunction(() => 'harness' in window);
  await page.evaluate(
    ([u, t, e]) => (window as any).harness.connect(u, t, { e2ee: 'dilla', iceServers: [], dilla: { epoch: e, lock: { instanceId: 'gpui-voice-proof', callGroupId: e.groupId } } }),
    [url, token, epochWire(1, KEY1)] as const,
  );
}

const pageStats = (page: Page): Promise<DillaMediaStats> => page.evaluate(() => (window as any).harness.mediaStats());

/** The page's decoded audio from `identity` through an AnalyserNode: the loudest bin's frequency over `ms`. */
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
  console.log(`PROOF ${name} ${JSON.stringify(data)}`);
}

const room = (name: string) => `gpui-proof-${name}-${Date.now().toString(36)}`;
const POLL = { timeout: 30_000, intervals: [250, 500, 1000] };

test.beforeAll(() => {
  if (process.env.VOICE_PROOF_BIN === undefined) {
    execFileSync('cargo', ['build', '--locked', '--bin', 'voice-proof'], { cwd: CRATE, stdio: 'inherit' });
  }
});

test('rust_to_browser_audio', async ({ page }) => {
  const r = room('r2b');
  await joinPage(page, r);
  const t0 = Date.now();
  const agent = await Agent.start('rust_to_browser_audio', r, ['--publish', 'audio', '--tone', '440', '--epoch', '1', '--base-key', KEY1, '--roster', ROSTER_ARG]);
  try {
    await expect.poll(async () => (await pageStats(page)).decrypted[RUST_KID(1)] ?? 0, POLL).toBeGreaterThan(100);
    const firstAudioMs = Date.now() - t0;
    const peak = await pagePeak(page, RUST_DEV, 1500);
    const s = await pageStats(page);
    const rust = await agent.stats();
    record('rust_to_browser_audio', { peak, firstAudioMs, page: { decrypted: s.decrypted, verified: s.verified, dropped: s.dropped, emptyFrames: s.emptyFrames }, rust: rust.media });
    expect(Math.abs(peak.peakHz - 440)).toBeLessThan(10);
    expect(peak.peakDb - peak.floorDb).toBeGreaterThan(30);
    expect(s.verified[RUST_DEV] ?? 0).toBeGreaterThan(0);
    // Zero decrypt failures on the receiver: every drop reason is zero.
    expect(sum(s.dropped as unknown as Record<string, number>)).toBe(0);
    expect(rust.media.failed).toEqual({});
    expect(rust.media.encrypted[RUST_KID(1)] ?? 0).toBeGreaterThan(100);
  } finally {
    await agent.close();
  }
});

test('browser_to_rust_audio', async ({ page }) => {
  const r = room('b2r');
  const agent = await Agent.start('browser_to_rust_audio', r, ['--epoch', '1', '--base-key', KEY1, '--roster', ROSTER_ARG]);
  try {
    await joinPage(page, r);
    await page.evaluate(() => (window as any).harness.publish({ mic: true }));
    await expect.poll(async () => (await agent.stats()).media.opened[PAGE_KID(1)] ?? 0, POLL).toBeGreaterThanOrEqual(100);
    await new Promise((res) => setTimeout(res, 1500));
    const rust = await agent.stats();
    const s = await pageStats(page);
    record('browser_to_rust_audio', { rust, page: { encrypted: s.encrypted, dropped: s.dropped } });
    expect(rust.media.opened[PAGE_KID(1)]).toBeGreaterThanOrEqual(100);
    expect(rust.media.failed).toEqual({});
    // The page's fake microphone is the media suite's 1 kHz tone (e2e/media/support/fixtures.ts).
    expect(Math.abs(rust.audio.peak_hz - 1000)).toBeLessThanOrEqual(20);
  } finally {
    await agent.close();
  }
});

test('epoch_change_mid_stream', async ({ page }) => {
  const r = room('epoch');
  const agent = await Agent.start('epoch_change_mid_stream', r, ['--publish', 'audio', '--tone', '440', '--epoch', '1', '--base-key', KEY1, '--roster', ROSTER_ARG]);
  try {
    await joinPage(page, r);
    await page.evaluate(() => (window as any).harness.publish({ mic: true }));
    await expect.poll(async () => (await pageStats(page)).decrypted[RUST_KID(1)] ?? 0, POLL).toBeGreaterThan(0);
    await expect.poll(async () => (await agent.stats()).media.opened[PAGE_KID(1)] ?? 0, POLL).toBeGreaterThan(0);
    await new Promise((res) => setTimeout(res, 2000));
    const beforePage = await pageStats(page);
    const beforeRust = await agent.stats();

    // Every receiver holds epoch 2 before any sender uses it, as an MLS Commit makes it: the Rust ring first,
    // then the page (its ring and, as the newest epoch, its sender), then the Rust sender switches.
    agent.send(`install 2 ${KEY2} ${ROSTER_ARG}`);
    await agent.next('installed', 5_000, 0);
    await page.evaluate((k) => (window as any).harness.installEpoch(k), epochWire(2, KEY2));
    agent.send(`send 2 ${KEY2}`);
    await agent.next('sending', 5_000, 0);

    await expect.poll(async () => (await pageStats(page)).decrypted[RUST_KID(2)] ?? 0, POLL).toBeGreaterThan(100);
    await expect.poll(async () => (await agent.stats()).media.opened[PAGE_KID(2)] ?? 0, POLL).toBeGreaterThan(100);
    const peak = await pagePeak(page, RUST_DEV, 1500);
    const afterPage = await pageStats(page);
    const afterRust = await agent.stats();
    const pageDropped = sum(afterPage.dropped as unknown as Record<string, number>) - sum(beforePage.dropped as unknown as Record<string, number>);
    const rustDropped = sum(afterRust.media.failed) - sum(beforeRust.media.failed);
    record('epoch_change_mid_stream', {
      peak, pageDropped, rustDropped,
      page: { decrypted: afterPage.decrypted, dropped: afterPage.dropped, byTrackKid: afterPage.decryptedByTrackKid },
      rust: afterRust,
    });
    // Audio continues in both directions under the new keys.
    expect(Math.abs(peak.peakHz - 440)).toBeLessThan(10);
    expect(Math.abs(afterRust.audio.peak_hz - 1000)).toBeLessThanOrEqual(20);
    // At most 10 dropped frames, both receivers together.
    expect(pageDropped + rustDropped).toBeLessThanOrEqual(10);
    // No frame accepted under a wrong key: every authenticated KID is the sender's own epoch-1 or epoch-2 KID,
    // and no frame failed its tag.
    expect(Object.keys(afterPage.decrypted).sort()).toEqual([RUST_KID(1), RUST_KID(2)].sort());
    expect(Object.keys(afterRust.media.opened).sort()).toEqual([PAGE_KID(1), PAGE_KID(2)].sort());
    expect((afterPage.dropped as unknown as Record<string, number>).aeadFail ?? 0).toBe(0);
    expect(afterRust.media.failed.E_SFRAME_AUTH ?? 0).toBe(0);
  } finally {
    await agent.close();
  }
});

test('video_vp8_rust_to_browser', async ({ page }) => {
  const r = room('vp8');
  await joinPage(page, r);
  const agent = await Agent.start('video_vp8_rust_to_browser', r, ['--publish', 'video', '--width', '640', '--height', '360', '--fps', '30', '--epoch', '1', '--base-key', KEY1, '--roster', ROSTER_ARG]);
  try {
    await expect.poll(async () => {
      const remote = await page.evaluate(() => (window as any).harness.remoteStats());
      return remote.find((x: any) => x.participantIdentity === RUST_DEV && x.kind === 'video')?.framesDecoded ?? 0;
    }, POLL).toBeGreaterThan(30);
    const size = await page.evaluate(() => {
      const v = document.querySelector('#media video') as HTMLVideoElement | null;
      return v === null ? null : [v.videoWidth, v.videoHeight];
    });
    const remote = await page.evaluate(() => (window as any).harness.remoteStats());
    const s = await pageStats(page);
    const rust = await agent.stats();
    record('video_vp8_rust_to_browser', { size, remote, page: { decrypted: s.decrypted, dropped: s.dropped }, rust: rust.media });
    expect(size).toEqual([640, 360]);
    expect(s.decrypted[RUST_KID(1)] ?? 0).toBeGreaterThan(30);
    expect(sum(s.dropped as unknown as Record<string, number>)).toBe(0);
    expect(rust.media.failed).toEqual({});
  } finally {
    await agent.close();
  }
});

// Step 4b: 1920x1080 at 30 fps VP8 from the Rust publisher, opened through the dilla hook and drawn in a GPUI window
// (../src/bin/video-window.rs) for 60 s. Needs a display and release builds: VOICE_PROOF_WINDOW=1 runs it, after
// `cargo build --release --bin voice-proof` (VOICE_PROOF_BIN=target/release/voice-proof) and
// `cargo build --release --features window --bin video-window`.
test('video_1080p_gpui_window', async () => {
  test.skip(process.env.VOICE_PROOF_WINDOW !== '1', 'VOICE_PROOF_WINDOW=1 runs the GPUI window measurement');
  test.setTimeout(240_000);
  const r = room('window');
  const publisher = await Agent.start('video_1080p_publisher', r, ['--publish', 'video', '--width', '1920', '--height', '1080', '--fps', '30', '--epoch', '1', '--base-key', KEY1, '--roster', ROSTER_ARG]);
  try {
    const { url, token } = await debugToken(CONTROL, r, PAGE_DEV, true);
    const window = spawn(process.env.VIDEO_WINDOW_BIN ?? join(CRATE, 'target', 'release', 'video-window'), [
      '--url', url, '--token', token, '--epoch', '1', '--base-key', KEY1, '--roster', ROSTER_ARG, '--seconds', process.env.VOICE_PROOF_WINDOW_SECONDS ?? '60', '--warmup', '5', '--fps', '30',
    ], { stdio: ['ignore', 'pipe', 'pipe'] });
    window.stderr!.pipe(createWriteStream(join(LOGS, 'video_1080p_gpui_window.stderr.log')));
    const lines: string[] = [];
    createInterface({ input: window.stdout! }).on('line', (l) => lines.push(l));
    const code = await new Promise<number | null>((res) => window.once('exit', res));
    const line = lines.find((l) => l.startsWith('VIDEO '));
    const json = lines.find((l) => l.startsWith('{'));
    record('video_1080p_gpui_window', { line, result: json === undefined ? null : JSON.parse(json), publisher: (await publisher.stats()).media });
    expect(code).toBe(0);
    expect(line).toMatch(/^VIDEO \S+ \d+(\.\d+)? \d+(\.\d+)?$/);
    const result = JSON.parse(json!) as { media: { failed: Record<string, number> }; painted: number };
    expect(result.painted).toBeGreaterThan(0);
    expect(result.media.failed).toEqual({});
  } finally {
    await publisher.close();
  }
});
