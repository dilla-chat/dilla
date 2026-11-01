import { spawn, execFile, type ChildProcessWithoutNullStreams } from 'node:child_process';
import { promisify } from 'node:util';
import type { AudioSource } from './contract.ts';

const run = promisify(execFile);
const FRAME_BYTES = 480 * 2 * 4;
export function parseAudioSources(text: string): AudioSource[] {
  if (text.length > 131072) throw new Error('Invalid audio source list');
  const rows: unknown = JSON.parse(text);
  if (!Array.isArray(rows) || rows.length > 256) throw new Error('Invalid audio source list');
  const seen = new Set<string>();
  return rows.map((row: unknown) => {
    if (!row || typeof row !== 'object' || Array.isArray(row)) throw new Error('Invalid audio source');
    const value = row as Record<string, unknown>;
    if (Object.keys(value).sort().join(',') !== 'id,label' || typeof value.id !== 'string' || !/^[0-9]{1,20}:[0-9]{1,10}$/.test(value.id)
      || typeof value.label !== 'string' || value.label.length < 1 || value.label.length > 512 || seen.has(value.id)) throw new Error('Invalid audio source');
    seen.add(value.id); return { id: value.id, label: value.label };
  });
}

/** The native pipe carries only little-endian F32 stereo at 48kHz. Never buffer more than one pipe chunk. */
export class PCMFrames {
  private tail = Buffer.alloc(0);
  private emit: (frame: Float32Array) => void;
  constructor(emit: (frame: Float32Array) => void) { this.emit = emit; }
  push(chunk: Buffer): void {
    if (chunk.length > 1024 * 1024) throw new Error('Invalid PCM chunk');
    const bytes = this.tail.length ? Buffer.concat([this.tail, chunk]) : chunk;
    let offset = 0;
    for (; offset + FRAME_BYTES <= bytes.length; offset += FRAME_BYTES) {
      const frame = new Float32Array(960);
      for (let i = 0; i < frame.length; i++) {
        const value = bytes.readFloatLE(offset + i * 4);
        if (!Number.isFinite(value)) throw new Error('Invalid PCM sample');
        frame[i] = Math.max(-1, Math.min(1, value));
      }
      this.emit(frame);
    }
    this.tail = Buffer.from(bytes.subarray(offset));
  }
}

export class NativeAudio {
  private executable: string;
  private excludedPID: number;
  private child: ChildProcessWithoutNullStreams | null = null;
  private generation = 0;
  constructor(executable: string, excludedPID = process.pid) { this.executable = executable; this.excludedPID = excludedPID; }
  async sources(): Promise<AudioSource[]> {
    const { stdout } = await run(this.executable, ['list', String(this.excludedPID)], { timeout: 10000, maxBuffer: 131072, windowsHide: true });
    return parseAudioSources(stdout);
  }
  start(source: AudioSource, pcm: (frame: Float32Array) => void, ended: () => void): void {
    if (!/^[0-9]{1,20}:[0-9]{1,10}$/.test(source.id)) throw new Error('Invalid audio selection');
    this.stop();
    const generation = ++this.generation;
    const child = spawn(this.executable, ['capture', source.id, String(this.excludedPID)], { stdio: 'pipe', windowsHide: true });
    this.child = child;
    const frames = new PCMFrames(frame => { if (generation === this.generation) pcm(frame); });
    child.stdout.on('data', (chunk: Buffer) => { try { frames.push(chunk); } catch { this.stop(); ended(); } });
    // Drain diagnostics, never send local process details or paths to the renderer.
    child.stderr.resume();
    const finish = () => { if (generation !== this.generation) return; this.child = null; this.generation++; ended(); };
    child.once('error', finish); child.once('exit', finish);
  }
  stop(): void { this.generation++; this.child?.kill(); this.child = null; }
}
