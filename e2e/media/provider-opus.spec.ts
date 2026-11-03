import { createHash } from 'node:crypto';
import { constants } from 'node:fs';
import { access, chmod, mkdtemp, open, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { dirname, isAbsolute, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { spawn } from 'node:child_process';
import { expect, test } from './support/test';
import { DS_URL, MediaDriver, epochWire, kidHex, testkitEnv, type CallToken, type MediaKey } from './support/driver';
import type { DillaHarness, DillaHarness21, DillaRemoteStats } from '../../packages/media/harness/main';

type W = { harness: DillaHarness & DillaHarness21 };
type BotReport = { published: Record<string, string>; decrypted: number; decrypted_by_kind: Record<string, number> };
type BotClose = { code: number | null; signal: NodeJS.Signals | null };
type BotProcess = { result: Promise<BotReport>; stop: () => Promise<BotClose> };
const HARNESS = 'http://127.0.0.1:5179/';
const MAX_INPUT_BYTES = 1 << 20;
const MAX_BOT_OUTPUT_BYTES = 16_384;
const sourcePath = process.env.DILLA_PROVIDER_OPUS_FILE;
const providerName = process.env.DILLA_PROVIDER_NAME;

test.use({ trace: 'off' });

async function stageInput(path: string, scratch: string): Promise<{ bytes: number; sha256: string }> {
  if (!isAbsolute(path)) throw new Error('provider input must be an absolute regular file');
  let handle;
  try {
    handle = await open(path, constants.O_RDONLY | constants.O_NOFOLLOW | constants.O_NONBLOCK);
    const stat = await handle.stat();
    if (!stat.isFile() || stat.size < 1 || stat.size > MAX_INPUT_BYTES) throw new Error('invalid provider input');
    const data = Buffer.alloc(MAX_INPUT_BYTES + 1);
    let length = 0;
    while (length < data.length) {
      const { bytesRead } = await handle.read(data, length, data.length - length, length);
      if (bytesRead === 0) break;
      length += bytesRead;
    }
    if (length < 1 || length > MAX_INPUT_BYTES) throw new Error('invalid provider input');
    const content = data.subarray(0, length);
    const output = await open(join(scratch, 'tone.ogg'), 'wx', 0o600);
    try { await output.writeFile(content); } finally { await output.close(); }
    return { bytes: length, sha256: createHash('sha256').update(content).digest('hex') };
  } catch {
    throw new Error('provider input must be a readable, bounded regular file (symlinks are refused)');
  } finally {
    await handle?.close();
  }
}

function runEncryptedBot(args: string[], baseKey: string, timeoutMs: number, timeoutGate: Promise<void> = Promise.resolve()): BotProcess {
  const bin = process.env.DILLA_MEDIABOT ?? join(dirname(fileURLToPath(import.meta.url)), '..', '..', 'target', 'dilla-mediabot');
  const child = spawn(bin, args, { stdio: ['pipe', 'pipe', 'pipe'] });
  let output = '';
  let outputBytes = 0;
  let outputTooLarge = false;
  let timedOut = false;
  let spawnFailed = false;
  let closed = false;
  let hardKillTimer: ReturnType<typeof setTimeout> | undefined;
  let deadlineTimer: ReturnType<typeof setTimeout> | undefined;
  let resolveClosed!: (close: BotClose) => void;
  const closedPromise = new Promise<BotClose>((resolve) => { resolveClosed = resolve; });
  const requestStop = () => {
    if (closed) return;
    child.kill('SIGTERM');
    if (!hardKillTimer) {
      hardKillTimer = setTimeout(() => {
        if (!closed) child.kill('SIGKILL');
      }, 1_000);
    }
  };

  const result = new Promise<BotReport>((resolve, reject) => {
    void timeoutGate.then(() => {
      if (closed) return;
      deadlineTimer = setTimeout(() => {
        timedOut = true;
        requestStop();
      }, timeoutMs);
    }, () => requestStop());
    child.stdout.on('data', (chunk: Buffer) => {
      const remaining = MAX_BOT_OUTPUT_BYTES - outputBytes;
      if (remaining > 0) {
        const kept = chunk.subarray(0, remaining);
        output += kept.toString();
        outputBytes += kept.length;
      }
      if (chunk.length > remaining) {
        outputTooLarge = true;
        requestStop();
      }
    });
    // Do not forward child diagnostics: the room token and provider input must stay out of logs.
    child.stderr.on('data', () => {});
    child.once('error', () => {
      spawnFailed = true;
    });
    child.once('close', (code, signal) => {
      closed = true;
      clearTimeout(deadlineTimer);
      clearTimeout(hardKillTimer);
      resolveClosed({ code, signal });
      if (spawnFailed) {
        reject(new Error('dilla-mediabot could not be started'));
        return;
      }
      if (timedOut) {
        reject(new Error('dilla-mediabot timed out'));
        return;
      }
      if (outputTooLarge || code !== 0) {
        reject(new Error('dilla-mediabot did not complete successfully'));
        return;
      }
      try { resolve(JSON.parse(output.trim()) as BotReport); }
      catch { reject(new Error('dilla-mediabot returned an invalid report')); }
    });
    child.stdin.on('error', () => requestStop());
    child.stdin.end(`${baseKey}\n`);
  });
  // The caller may be awaiting browser observations before it awaits the bot report.
  void result.catch(() => {});
  return {
    result,
    async stop() {
      requestStop();
      return await closedPromise;
    },
  };
}

async function waitForFile(path: string, timeoutMs: number): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    try { await access(path); return; } catch { await new Promise((resolve) => setTimeout(resolve, 10)); }
  }
  throw new Error('mediabot did not signal readiness');
}

const remoteAudio = async (page: import('@playwright/test').Page, identity: string) =>
  (await page.evaluate(() => (globalThis as unknown as W).harness.dillaRemoteStats()))
    .filter((row) => row.participantIdentity === identity && row.kind === 'audio' && row.source === 'microphone');

test('mediabot timeout sends TERM, escalates to KILL, and reaps the process', async () => {
  const scratch = await mkdtemp(join(tmpdir(), 'dilla-mediabot-timeout-'));
  const fake = join(scratch, 'mediabot');
  const readyFile = join(scratch, 'ready');
  await writeFile(fake, '#!/usr/bin/env node\nprocess.on("SIGTERM", () => {}); require("node:fs").writeFileSync(process.env.DILLA_MEDIABOT_READY, "ready"); setInterval(() => {}, 1000);\n', { mode: 0o700 });
  await chmod(fake, 0o700);
  const previous = process.env.DILLA_MEDIABOT;
  const previousReady = process.env.DILLA_MEDIABOT_READY;
  process.env.DILLA_MEDIABOT = fake;
  process.env.DILLA_MEDIABOT_READY = readyFile;
  let readyResolve!: () => void;
  let readyReject!: (error: Error) => void;
  const readiness = new Promise<void>((resolve, reject) => { readyResolve = resolve; readyReject = reject; });
  const run = runEncryptedBot([], '0a'.repeat(16), 50, readiness);
  const started = Date.now();
  try {
    await waitForFile(readyFile, 5_000).then(readyResolve, readyReject);
    await expect(run.result).rejects.toThrow('dilla-mediabot timed out');
    expect(Date.now() - started).toBeGreaterThanOrEqual(900);
    expect(Date.now() - started).toBeLessThan(4_000);
    expect((await run.stop()).signal).toBe('SIGKILL');
  } finally {
    await run.stop();
    if (previous === undefined) delete process.env.DILLA_MEDIABOT;
    else process.env.DILLA_MEDIABOT = previous;
    if (previousReady === undefined) delete process.env.DILLA_MEDIABOT_READY;
    else process.env.DILLA_MEDIABOT_READY = previousReady;
    await rm(scratch, { recursive: true, force: true });
  }
});

test('mediabot output overflow is capped, terminated, and reaped', async () => {
  const scratch = await mkdtemp(join(tmpdir(), 'dilla-mediabot-output-'));
  const fake = join(scratch, 'mediabot');
  await writeFile(fake, '#!/usr/bin/env node\nprocess.on("SIGTERM", () => {}); process.stdout.write("x".repeat(131072)); setInterval(() => {}, 1000);\n', { mode: 0o700 });
  await chmod(fake, 0o700);
  const previous = process.env.DILLA_MEDIABOT;
  process.env.DILLA_MEDIABOT = fake;
  const run = runEncryptedBot([], '0a'.repeat(16), 10_000);
  const started = Date.now();
  try {
    await expect(run.result).rejects.toThrow('dilla-mediabot did not complete successfully');
    expect(Date.now() - started).toBeLessThan(4_000);
    expect((await run.stop()).signal).toBe('SIGKILL');
  } finally {
    await run.stop();
    if (previous === undefined) delete process.env.DILLA_MEDIABOT;
    else process.env.DILLA_MEDIABOT = previous;
    await rm(scratch, { recursive: true, force: true });
  }
});

test('named Ogg Opus input is encrypted and authenticated in Chromium and Firefox', async ({ browser, browserName }, info) => {
  test.setTimeout(120_000);
  test.skip(!sourcePath, 'unqualified: set DILLA_PROVIDER_OPUS_FILE to a captured Ogg Opus file');
  test.skip(!providerName, 'unqualified: set DILLA_PROVIDER_NAME to youtube or synthetic');
  if (providerName !== 'youtube' && providerName !== 'synthetic') {
    throw new Error('DILLA_PROVIDER_NAME must be youtube or synthetic');
  }

  const scratch = await mkdtemp(join(tmpdir(), 'dilla-provider-opus-'));
  await chmod(scratch, 0o700);
  const tokenPath = join(scratch, 'room-token');
  let driver: MediaDriver | undefined;
  let bot: BotProcess | undefined;
  try {
    const input = await stageInput(sourcePath!, scratch);
    driver = await MediaDriver.start(DS_URL, testkitEnv());
    await driver.request('setup', { actors: ['alice', 'erin'] });
    await driver.request('open_call', { actor: 'alice' });
    await driver.request('join', { actor: 'erin' });
    await driver.request('sync', { actor: 'alice' });

    const page = await (await browser.newContext()).newPage();
    await page.goto(HARNESS);
    const aliceKey = await driver.request<MediaKey>('media_key', { actor: 'alice' });
    const aliceToken = await driver.request<CallToken>('call_token', { actor: 'alice', vdec: 'vp8' });
    await page.evaluate((o) => (globalThis as unknown as W).harness.dillaJoin(o), {
      livekitUrl: aliceToken.livekitUrl, token: aliceToken.token, iceServers: aliceToken.iceServers,
      epoch: epochWire(aliceKey, aliceKey.epoch), caps: aliceToken.caps,
    });

    const publisher = await driver.request<{ deviceId: string }>('device', { actor: 'erin' });
    const publisherKey = await driver.request<MediaKey>('media_key', { actor: 'erin' });
    const publisherToken = await driver.request<CallToken>('call_token', { actor: 'erin', vdec: 'vp8' });
    const tokenFile = await open(tokenPath, 'wx', 0o600);
    try { await tokenFile.writeFile(publisherToken.token); } finally { await tokenFile.close(); }

    bot = runEncryptedBot([
      '-url', publisherToken.livekitUrl, '-token-file', tokenPath, '-base-key-file', '-',
      '-leaf', String(publisherKey.selfLeaf), '-epoch', publisherKey.epoch,
      '-roster', publisherKey.roster.map((r) => `${r.leaf}:${r.deviceId}`).join(','),
      '-publish', 'opus', '-duration', '15s', '-media', scratch,
    ], publisherKey.baseKey, 40_000);
    let botFailed = false;
    void bot.result.catch(() => { botFailed = true; });
    const kid = kidHex(publisherKey.selfLeaf, publisherKey.epoch);

    let matched: DillaRemoteStats | undefined;
    await expect.poll(async () => {
      if (botFailed) throw new Error('dilla-mediabot exited before its track reached the receiver');
      const tracks = await remoteAudio(page, publisher.deviceId);
      if (tracks.length === 1) matched = tracks[0];
      return tracks.length === 1 && tracks[0].packetsReceived > 0;
    }, { timeout: 25_000, message: 'one microphone track from the expected provider peer reaches the SFU receiver' }).toBe(true);

    const trackId = matched!.trackId;
    const authenticated = async () => {
      const stats = await page.evaluate(() => (globalThis as unknown as W).harness.dillaStats());
      return stats.decryptedByTrackKid[trackId]?.[kid] ?? 0;
    };
    await expect.poll(authenticated, { timeout: 20_000 }).toBeGreaterThan(0);
    const first = await authenticated();
    await expect.poll(authenticated, { timeout: 5_000 }).toBeGreaterThan(first);
    await expect.poll(async () => {
      const tracks = await remoteAudio(page, publisher.deviceId);
      return tracks.length === 1 ? tracks[0].totalSamplesReceived : 0;
    }, { timeout: 10_000 }).toBeGreaterThan(0);

    let renderedRms = 0;
    await expect.poll(async () => {
      const probes = (await page.evaluate(() => (globalThis as unknown as W).harness.dillaRenderProbe(2_000)))
        .filter((row) => row.participantIdentity === publisher.deviceId && row.kind === 'audio');
      expect(probes).toHaveLength(1);
      renderedRms = Math.max(renderedRms, probes[0].rms);
      return renderedRms;
    }, { timeout: 12_000, message: 'the decrypted provider track renders audible samples' }).toBeGreaterThan(0.001);
    const finalTracks = await remoteAudio(page, publisher.deviceId);
    expect(finalTracks).toHaveLength(1);
    const finalDecrypted = await authenticated();
    expect(finalDecrypted).toBeGreaterThan(first);

    const botReport = await bot.result;
    expect(botReport.published.opus).toMatch(/^TR_/);
    expect(botReport.decrypted).toBe(0);

    const evidence = {
      provider: providerName,
      qualification: providerName === 'synthetic' ? 'harness-smoke-only' : 'provider-evidence',
      browser: browserName,
      sha256: input.sha256,
      input_bytes: input.bytes,
      audio_tracks: 1,
      authenticated_frames: finalDecrypted,
      samples_received: finalTracks[0].totalSamplesReceived,
      rms: renderedRms,
    };
    await info.attach('provider-opus-evidence.json', { body: JSON.stringify(evidence, null, 2), contentType: 'application/json' });
    console.log(`PROVIDER_OPUS ${JSON.stringify(evidence)}`);
  } finally {
    await bot?.stop();
    await bot?.result.catch(() => undefined);
    await driver?.close();
    await rm(scratch, { recursive: true, force: true });
  }
});
