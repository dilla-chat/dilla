// Fake capture sources for the media tests. Chromium reads a Y4M and a WAV through
// --use-file-for-fake-video-capture / --use-file-for-fake-audio-capture (media_switches.cc at
// 153.0.8010.12); Firefox has its own fake devices (640x480@30 and a 1 kHz tone) and takes prefs,
// never `permissions` ("Unknown permission: camera").
import { execFileSync } from 'node:child_process';
import { existsSync, mkdirSync, realpathSync, statSync, writeFileSync } from 'node:fs';
import { homedir } from 'node:os';
import { join, posix } from 'node:path';

const W = 320, H = 240, FPS = 20, SECONDS = 2;
const RATE = 48_000;

/**
 * Chromium's macOS audio-service sandbox can read the user's audio plug-in subtree, but not arbitrary temporary files.
 * A denied fake WAV read leaves a live microphone producing zeros (simple_sources.cc, FileSource::LoadWavFile).
 * Keep test audio data inside that existing read grant; no sandbox switches or application signatures change.
 * Pinned policy: https://chromium.googlesource.com/chromium/src/+/156.0.8078.75/sandbox/policy/mac/audio.sb#17
 */
export function captureAudioDirectory(requested: string, platform: NodeJS.Platform = process.platform, home = homedir()): string {
  return platform === 'darwin' ? posix.join(home, 'Library', 'Audio', 'Plug-Ins', 'dilla-e2e-media-fixtures') : requested;
}

/** Y4M frames with a moving gradient (so the encoder emits delta frames), and a 1 kHz mono tone. */
export function writeFixtures(dir: string, toneHz = 1000): { y4m: string; wav: string } {
  if (!Number.isFinite(toneHz) || toneHz < 100 || toneHz > 10000) throw new Error('Invalid fake microphone tone');
  mkdirSync(dir, { recursive: true });
  const y4m = join(dir, 'fake-320x240.y4m');
  const audioDirectory = captureAudioDirectory(dir);
  mkdirSync(audioDirectory, { recursive: true });
  // Seatbelt checks resolved paths; avoid passing a symlink spelling such as /var instead of /private/var.
  const wav = join(process.platform === 'darwin' ? realpathSync(audioDirectory) : audioDirectory, toneHz === 1000 ? 'fake-1khz-48k.wav' : `fake-${toneHz}hz-48k.wav`);
  const frameBytes = W * H * 3 / 2;
  const header = Buffer.from(`YUV4MPEG2 W${W} H${H} F${FPS}:1 Ip A1:1 C420jpeg\n`);
  const y4mSize = header.length + FPS * SECONDS * (6 + frameBytes);
  if (!existsSync(y4m) || statSync(y4m).size !== y4mSize) {
    const parts: Buffer[] = [header];
    for (let f = 0; f < FPS * SECONDS; f++) {
      const frame = Buffer.alloc(frameBytes, 128);
      for (let y = 0; y < H; y++) for (let x = 0; x < W; x++) frame[y * W + x] = (x + y + f * 8) & 0xff;
      parts.push(Buffer.from('FRAME\n'), frame);
    }
    writeFileSync(y4m, Buffer.concat(parts));
  }
  const samples = RATE * SECONDS;
  const wavSize = 44 + samples * 2;
  if (!existsSync(wav) || statSync(wav).size !== wavSize) {
    const b = Buffer.alloc(wavSize);
    b.write('RIFF', 0); b.writeUInt32LE(wavSize - 8, 4); b.write('WAVE', 8);
    b.write('fmt ', 12); b.writeUInt32LE(16, 16); b.writeUInt16LE(1, 20); b.writeUInt16LE(1, 22);
    b.writeUInt32LE(RATE, 24); b.writeUInt32LE(RATE * 2, 28); b.writeUInt16LE(2, 32); b.writeUInt16LE(16, 34);
    b.write('data', 36); b.writeUInt32LE(samples * 2, 40);
    for (let i = 0; i < samples; i++) b.writeInt16LE(Math.round(Math.sin((2 * Math.PI * toneHz * i) / RATE) * 0.5 * 32767), 44 + i * 2);
    writeFileSync(wav, b);
  }
  return { y4m, wav };
}

/** The five Chromium switches of interfaces.md (f). */
export function chromiumMediaArgs(f: { y4m: string; wav: string }): string[] {
  return [
    '--use-fake-device-for-media-stream',
    '--use-fake-ui-for-media-stream',
    `--use-file-for-fake-video-capture=${f.y4m}`,
    `--use-file-for-fake-audio-capture=${f.wav}`,
    '--autoplay-policy=no-user-gesture-required',
  ];
}

/** The three Firefox prefs of interfaces.md (f), plus autoplay for the WebAudio observers. */
export const FIREFOX_MEDIA_PREFS: Record<string, boolean | number> = {
  'media.navigator.streams.fake': true,
  'media.navigator.permission.disabled': true,
  'media.devices.unfocused.enabled': true,
  'media.autoplay.default': 0,
  'media.autoplay.block-webaudio': false,
};

/**
 * Music fixtures for the Bot (Bot v2 task 18): a 30 s and a 5 s Ogg Opus file. Build the Bot with `-tags dilla_testhost`
 * and start it with DILLA_BOT_MUSIC_FIXTURES=<dir>; `/play fixture-30s` (or `.../watch?v=fixture-30s`) then plays the file
 * through the same single ffmpeg as a real track. The names are 11 characters, a valid video id, so a queue accepts them.
 */
export const MUSIC_FIXTURES = { long: 'fixture-30s', short: 'fixture-05s' } as const;

export function writeMusicFixtures(dir: string): { dir: string; long: string; short: string } {
  mkdirSync(dir, { recursive: true });
  const render = (stem: string, seconds: number, hz: number): string => {
    const path = join(dir, `${stem}.opus`);
    if (!existsSync(path) || statSync(path).size === 0) {
      // Opus at 48 kHz stereo, 20 ms frames: the format the Bot forwards.
      execFileSync('ffmpeg', ['-hide_banner', '-loglevel', 'error', '-nostdin', '-y',
        '-f', 'lavfi', '-i', `sine=frequency=${String(hz)}:sample_rate=48000:duration=${String(seconds)}`,
        '-ac', '2', '-c:a', 'libopus', '-b:a', '96k', '-frame_duration', '20', '-f', 'ogg', path], { stdio: 'ignore' });
    }
    return path;
  };
  return { dir, long: render(MUSIC_FIXTURES.long, 30, 440), short: render(MUSIC_FIXTURES.short, 5, 660) };
}
