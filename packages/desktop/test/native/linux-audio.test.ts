import { test } from 'node:test';
import assert from 'node:assert/strict';
import { spawn, execFile, type ChildProcess } from 'node:child_process';
import { promisify } from 'node:util';
import { mkdtemp, writeFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
const exec = promisify(execFile);
const pause = (ms: number) => new Promise(done => setTimeout(done, ms));
const binary = resolve(import.meta.dirname, '../../native-bin/dilla-audio');
function wave(frequency: number): Buffer {
  const samples = 48000 * 15, bytes = samples * 4, out = Buffer.alloc(44 + bytes);
  out.write('RIFF'); out.writeUInt32LE(bytes + 36, 4); out.write('WAVEfmt ', 8); out.writeUInt32LE(16, 16);
  out.writeUInt16LE(1, 20); out.writeUInt16LE(2, 22); out.writeUInt32LE(48000, 24); out.writeUInt32LE(192000, 28); out.writeUInt16LE(4, 32); out.writeUInt16LE(16, 34); out.write('data', 36); out.writeUInt32LE(bytes, 40);
  for (let frame = 0; frame < samples; frame++) { const sample = Math.round(Math.sin(2 * Math.PI * frequency * frame / 48000) * 10000); out.writeInt16LE(sample, 44 + frame * 4); out.writeInt16LE(sample, 46 + frame * 4); }
  return out;
}
async function terminate(child: ChildProcess): Promise<void> {
  if (child.exitCode !== null || child.signalCode !== null) return;
  await new Promise<void>(done => { child.once('exit', () => done()); child.kill('SIGTERM'); });
}
test('PipeWire captures a selected playback app and excludes a separate call stream', { timeout: 30000 }, async () => {
  const root = await mkdtemp(join(tmpdir(), 'dilla-pipewire-'));
  const env = { ...process.env, XDG_RUNTIME_DIR: root, PIPEWIRE_RUNTIME_DIR: root, XDG_STATE_HOME: root, XDG_CACHE_HOME: root };
  const children: ChildProcess[] = [];
  let diagnostic = '';
  const start = (exe: string, args: string[]) => { const child = spawn(exe, args, { env, stdio: ['ignore', 'pipe', 'pipe'] }); children.push(child); child.stderr!.on('data', chunk => { diagnostic += chunk.toString(); }); return child; };
  try {
    start('pipewire', []);
    for (let i = 0; i < 50; i++) { try { await exec('pw-cli', ['info', '0'], { env, timeout: 500 }); break; } catch { if (i === 49) throw new Error(diagnostic); await pause(100); } }
    start('wireplumber', []); await pause(500);
    await writeFile(join(root, 'selected.wav'), wave(1000)); await writeFile(join(root, 'call.wav'), wave(440));
    const selected = start('pw-play', ['--target=0', '--properties={ application.name="Dilla Selected Test" }', join(root, 'selected.wav')]);
    const call = start('pw-play', ['--target=0', '--properties={ application.name="Dilla Call Test" }', join(root, 'call.wav')]);
    let source: {id: string; label: string} | undefined;
    for (let i = 0; i < 50; i++) {
      const { stdout } = await exec(binary, ['list', String(call.pid)], { env, timeout: 1000 });
      const rows = JSON.parse(stdout) as {id: string; label: string}[];
      assert.ok(!rows.some(row => row.label === 'Dilla Call Test'));
      source = rows.find(row => row.label === 'Dilla Selected Test');
      if (source) break; await pause(100);
    }
    if (!source) diagnostic += (await exec('pw-dump', [], { env, timeout: 2000 })).stdout;
    assert.ok(source, diagnostic);
    const capture = start(binary, ['capture', source.id, String(call.pid)]);
    const chunks: Buffer[] = []; let length = 0;
    capture.stdout!.on('data', (data: Buffer) => { chunks.push(data); length += data.length; });
    for (let i = 0; i < 100 && length < 48000 * 8 * 2; i++) { await pause(100); if (capture.exitCode !== null) break; }
    if (!length) diagnostic += (await exec('pw-dump', [], { env, timeout: 2000 })).stdout;
    assert.ok(length >= 48000 * 8, `capture produced ${length} bytes\n${diagnostic}`);
    const pcm = Buffer.concat(chunks); const count = Math.min(48000, Math.floor(pcm.length / 8));
    const power = (hz: number) => { let re = 0, im = 0; for (let i = 0; i < count; i++) { const value = pcm.readFloatLE(i * 8); re += value * Math.cos(2 * Math.PI * hz * i / 48000); im += value * Math.sin(2 * Math.PI * hz * i / 48000); } return Math.hypot(re, im) / count; };
    assert.ok(power(1000) > 0.05, `selected app was inaudible: ${power(1000)}`);
    assert.ok(power(440) < 0.005, `call audio leaked: ${power(440)}`);
    await terminate(selected); await pause(200);
    assert.notEqual(capture.exitCode, null, 'capture must end when its selected source disappears');
  } finally { for (const child of children.reverse()) await terminate(child); await rm(root, { recursive: true, force: true }); }
});
